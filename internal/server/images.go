package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"myogchat/internal/imagegen"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// Settings keys for self-hosted image generation (feature 3.1).
const (
	settingImagegenURL   = "imagegen_url"
	settingImagegenToken = "imagegen_token"
)

const (
	// maxImagePromptChars caps a generation prompt.
	maxImagePromptChars = 2000
	// imageCacheControl is a long cache header for immutable image bytes (the id
	// is a UUID and the bytes never change).
	imageCacheControl = "private, max-age=31536000, immutable"
)

// imagegenConfig resolves the effective image backend config: the DB URL/token
// (token decrypted) overlaid on the env defaults. ok is false when no URL is
// configured anywhere.
func (s *Server) imagegenConfig(ctx context.Context) (rawURL, token string, ok bool) {
	rawURL = strings.TrimSpace(s.imagegenURL)
	if v, found, _ := s.st.GetSetting(ctx, settingImagegenURL); found && strings.TrimSpace(v) != "" {
		rawURL = strings.TrimSpace(v)
	}
	token = strings.TrimSpace(s.imagegenToken)
	if enc, found, _ := s.st.GetSetting(ctx, settingImagegenToken); found && enc != "" {
		if raw, err := secret.Decrypt(s.encKey, enc); err == nil {
			if t := strings.TrimSpace(string(raw)); t != "" {
				token = t
			}
		}
	}
	if rawURL == "" {
		return "", "", false
	}
	return rawURL, token, true
}

// imagegenClient builds a client when the integration is configured.
func (s *Server) imagegenClient(ctx context.Context) (*imagegen.Client, bool) {
	rawURL, token, ok := s.imagegenConfig(ctx)
	if !ok {
		return nil, false
	}
	c, err := imagegen.New(imagegen.Config{BaseURL: rawURL, Token: token})
	if err != nil {
		s.log.Warn("imagegen client unavailable", "error", err)
		return nil, false
	}
	return c, true
}

// imagegenSettings is the self-contained admin settings block for image
// generation (feature 3.1). It is merged into /api/settings.
func (s *Server) imagegenSettings(ctx context.Context) map[string]any {
	rawURL, token, configured := s.imagegenConfig(ctx)
	hint := ""
	if token != "" {
		hint = keyHint(token)
	}
	available := false
	if c, ok := s.imagegenClient(ctx); ok {
		available = c.Available(ctx)
	}
	return map[string]any{
		"imagegen_url":        rawURL,
		"imagegen_configured": configured,
		"imagegen_available":  available,
		"imagegen_token_hint": hint,
	}
}

// setImagegen persists the backend URL and (when token is non-nil) the encrypted
// bearer token. A nil token leaves the stored token untouched; an empty string
// clears it.
func (s *Server) setImagegen(ctx context.Context, rawURL string, token *string) error {
	if err := s.st.SetSetting(ctx, settingImagegenURL, strings.TrimSpace(rawURL)); err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	plain := strings.TrimSpace(*token)
	if plain == "" {
		return s.st.SetSetting(ctx, settingImagegenToken, "")
	}
	enc, err := secret.Encrypt(s.encKey, []byte(plain))
	if err != nil {
		return err
	}
	return s.st.SetSetting(ctx, settingImagegenToken, enc)
}

// handleCreateImage generates an image (3.1), stores the bytes owner-scoped, and
// returns its metadata. When conversation_id is supplied (and owned), the prompt
// and the resulting image are also persisted as a user + assistant message so
// the image survives a reload.
func (s *Server) handleCreateImage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Prompt         string `json:"prompt"`
		Size           string `json:"size"`
		ConversationID string `json:"conversation_id"`
	}
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if len([]rune(prompt)) > maxImagePromptChars {
		writeError(w, http.StatusBadRequest, "prompt exceeds 2000 characters")
		return
	}

	client, ok := s.imagegenClient(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "image generation is not configured")
		return
	}

	// Verify conversation ownership before spending GPU time.
	var conv store.Conversation
	if id := strings.TrimSpace(req.ConversationID); id != "" {
		c, err := s.st.GetConversation(r.Context(), id, u.ID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "conversation not found")
			return
		}
		if err != nil {
			s.log.Error("get conversation", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load conversation")
			return
		}
		conv = c
	}

	width, height := imagegen.ParseSize(req.Size)
	res, err := client.Generate(r.Context(), prompt, width, height)
	if err != nil {
		s.log.Error("image generation failed", "error", err)
		writeError(w, http.StatusBadGateway, imagegenMessage(err))
		return
	}

	now := time.Now().UnixMilli()
	img := store.Image{
		ID:        newUUID(),
		UserID:    u.ID,
		Prompt:    prompt,
		Model:     res.Model,
		Mime:      res.Mime,
		Width:     res.Width,
		Height:    res.Height,
		Size:      int64(len(res.Data)),
		Data:      res.Data,
		CreatedAt: now,
	}
	if err := s.st.InsertImage(r.Context(), &img); err != nil {
		s.log.Error("insert image", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store image")
		return
	}

	resp := map[string]any{
		"id":         img.ID,
		"prompt":     img.Prompt,
		"mime":       img.Mime,
		"width":      img.Width,
		"height":     img.Height,
		"size":       img.Size,
		"created_at": img.CreatedAt,
		"url":        "/api/images/" + img.ID,
	}

	// Optionally fold the image into a conversation as a persisted pair of
	// messages so it renders after a reload.
	if conv.ID != "" {
		ref := store.GeneratedImage{
			ID: img.ID, Prompt: img.Prompt, Mime: img.Mime,
			Width: img.Width, Height: img.Height, Size: img.Size,
		}
		userMsg := &store.Message{
			ID: newUUID(), ConversationID: conv.ID, Role: "user",
			Content: prompt, CreatedAt: now,
		}
		assistantMsg := &store.Message{
			ID: newUUID(), ConversationID: conv.ID, Role: "assistant",
			Images: []store.GeneratedImage{ref}, CreatedAt: now + 1,
		}
		if err := s.st.InsertMessage(r.Context(), userMsg); err != nil {
			s.log.Error("insert image user message", "error", err)
		} else if err := s.st.InsertMessage(r.Context(), assistantMsg); err != nil {
			s.log.Error("insert image assistant message", "error", err)
		} else {
			if err := s.st.TouchConversation(r.Context(), conv.ID, now); err != nil {
				s.log.Error("touch conversation", "error", err)
			}
			if strings.TrimSpace(conv.Title) == "" {
				if err := s.st.SetTitle(r.Context(), conv.ID, truncateRunes(prompt, 40)); err != nil {
					s.log.Error("set image conversation title", "error", err)
				}
			}
			resp["conversation_id"] = conv.ID
			resp["message_id"] = assistantMsg.ID
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}

// handleGetImage returns an image's raw bytes (owner only, 3.1).
func (s *Server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	img, err := s.st.GetImage(r.Context(), r.PathValue("id"), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	if err != nil {
		s.log.Error("get image", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load image")
		return
	}
	mime := img.Mime
	if mime == "" {
		mime = "image/png"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", imageCacheControl)
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(img.Data)), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(img.Data); err != nil {
		s.log.Error("write image", "error", err)
	}
}

// handleDeleteImage removes an image (owner only, 3.1).
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	n, err := s.st.DeleteImage(r.Context(), r.PathValue("id"), u.ID)
	if err != nil {
		s.log.Error("delete image", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete image")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// imagegenMessage maps a client error to a user-facing message.
func imagegenMessage(err error) string {
	switch {
	case errors.Is(err, imagegen.ErrUnreachable):
		return "image backend unreachable — check the ComfyUI URL and that the instance is running"
	case strings.Contains(err.Error(), "timed out"):
		return "image generation timed out"
	default:
		return "image generation failed: " + err.Error()
	}
}
