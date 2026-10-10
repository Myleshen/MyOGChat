// Package server wires the HTTP API (contract §3, §8, §9), auth middleware,
// request logging and static SPA serving from the embedded filesystem.
package server

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/models"
	"myogchat/internal/ocr"
	"myogchat/internal/sandbox"
	"myogchat/internal/speech"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// Server holds the handler dependencies.
type Server struct {
	st     *store.Store
	up     *upstream.Client
	auth   *auth.Auth
	static fs.FS
	log    *slog.Logger

	encKey    []byte          // AES-256 key for per-user API keys (§2.1)
	serverKey string          // optional shared OpenCode Go key (may be empty)
	search    SearchDefaults  // env-derived web-search defaults (Phase 2.4)
	geo       GeoDefaults     // env-derived maps defaults (Phase 3.4)
	sbx       *sandbox.Runner // code sandbox runner; nil when disabled (2.6/2.7)
	ocrEngine ocr.Engine      // image OCR backend (feature 2.5/2.13); nil when disabled
	// ocrEngineName is the configured MYOGCHAT_OCR_ENGINE mode (tesseract |
	// vision | auto | none) for status reporting; ocrVisionAvailable records
	// whether a vision model was built.
	ocrEngineName      string
	ocrVisionAvailable bool

	// Speech (features 3.5 / 3.6). speechCfg holds env defaults; DB settings
	// override the URL/voice. newSTT/newTTS build an engine per request (so an
	// admin can repoint the services without a restart) and are injectable for
	// tests.
	speechCfg SpeechDefaults
	newSTT    func(speech.STTConfig) speech.Transcriber
	newTTS    func(speech.TTSConfig) speech.Synthesizer

	// Self-hosted image generation (feature 3.1) env defaults; DB settings
	// override the URL/token.
	imagegenURL   string
	imagegenToken string

	// Per-user Linkwarden (2.8) env fallback (LINKWARDEN_URL + LINKWARDEN_TOKEN,
	// both required). Each user can override with their own stored instance.
	linkwardenURL   string
	linkwardenToken string
}

// New builds a Server. encKey encrypts per-user API keys; serverKey is the
// optional shared key (empty when the deployment is pure BYOK); search holds the
// env-derived web-search defaults (keys, provider, result count, region); geo
// holds the env-derived maps defaults (3.4); sbx runs the code sandbox tools
// (nil when the sandbox is disabled by env); ocrEngine enables image OCR and may
// be nil; ocrEngineName is the configured OCR engine mode and ocrVisionAvailable
// reports whether vision OCR is usable; speechDefaults configures the speech
// engines (features 3.5 / 3.6); imagegenURL/imagegenToken are the env defaults
// for self-hosted image generation (feature 3.1, both may be empty);
// linkwardenURL/linkwardenToken are the optional env fallback for the per-user
// Linkwarden integration (feature 2.8, both must be set to apply).
func New(st *store.Store, up *upstream.Client, a *auth.Auth, staticFS fs.FS, log *slog.Logger, encKey []byte, serverKey string, search SearchDefaults, geo GeoDefaults, sbx *sandbox.Runner, ocrEngine ocr.Engine, ocrEngineName string, ocrVisionAvailable bool, speechDefaults SpeechDefaults, imagegenURL, imagegenToken, linkwardenURL, linkwardenToken string) *Server {
	s := &Server{
		st: st, up: up, auth: a, static: staticFS, log: log,
		encKey: encKey, serverKey: serverKey, search: search, geo: geo, sbx: sbx,
		ocrEngine: ocrEngine, ocrEngineName: ocrEngineName, ocrVisionAvailable: ocrVisionAvailable,
		speechCfg:       speechDefaults,
		imagegenURL:     imagegenURL,
		imagegenToken:   imagegenToken,
		linkwardenURL:   linkwardenURL,
		linkwardenToken: linkwardenToken,
	}
	// Build engines per request so DB URL/voice overrides take effect live.
	s.newSTT = func(cfg speech.STTConfig) speech.Transcriber { return speech.NewOpenAISTT(cfg) }
	if speechDefaults.TTSFormat == "piper" {
		s.newTTS = func(cfg speech.TTSConfig) speech.Synthesizer { return speech.NewPiperTTS(cfg) }
	} else {
		s.newTTS = func(cfg speech.TTSConfig) speech.Synthesizer { return speech.NewOpenAITTS(cfg) }
	}
	return s
}

// Handler returns the root handler: routes + auth + request logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public auth endpoints (§9.3).
	mux.HandleFunc("GET /api/auth/status", s.auth.Status)
	mux.HandleFunc("POST /api/auth/setup", s.auth.Setup)
	mux.HandleFunc("POST /api/auth/register", s.auth.Register)
	mux.HandleFunc("POST /api/auth/login", s.auth.Login)
	mux.HandleFunc("POST /api/auth/logout", s.auth.Logout)
	mux.HandleFunc("GET /api/auth/me", s.auth.Me)

	// Chat data (per-user).
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("GET /api/me/key", s.handleGetMyKey)
	mux.HandleFunc("PUT /api/me/key", s.handlePutMyKey)
	// Usage & quotas (2.11).
	mux.HandleFunc("GET /api/me/usage", s.handleMyUsage)
	mux.HandleFunc("GET /api/users/usage", s.handleUsersUsage)
	// Per-user Serper search key (2.12).
	mux.HandleFunc("GET /api/me/serper", s.handleGetMySerper)
	mux.HandleFunc("PUT /api/me/serper", s.handlePutMySerper)
	// Per-user Linkwarden (2.8).
	mux.HandleFunc("GET /api/me/linkwarden", s.handleGetMyLinkwarden)
	mux.HandleFunc("PUT /api/me/linkwarden", s.handlePutMyLinkwarden)
	mux.HandleFunc("DELETE /api/me/linkwarden", s.handleDeleteMyLinkwarden)
	// Calendar (per-user, 3.3).
	mux.HandleFunc("GET /api/me/calendar", s.handleGetCalendar)
	mux.HandleFunc("PUT /api/me/calendar", s.handlePutCalendar)
	mux.HandleFunc("DELETE /api/me/calendar", s.handleDeleteCalendar)
	// Groups & shared keys (§2.2).
	mux.HandleFunc("GET /api/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/groups", s.handleCreateGroup)
	mux.HandleFunc("PATCH /api/groups/{id}", s.handlePatchGroup)
	mux.HandleFunc("DELETE /api/groups/{id}", s.handleDeleteGroup)
	mux.HandleFunc("POST /api/groups/{id}/members", s.handleAddGroupMember)
	mux.HandleFunc("DELETE /api/groups/{id}/members/{userId}", s.handleRemoveGroupMember)

	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("PATCH /api/projects/{id}", s.handlePatchProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.handleDeleteProject)
	mux.HandleFunc("GET /api/conversations", s.handleListConversations)
	mux.HandleFunc("POST /api/conversations", s.handleCreateConversation)
	mux.HandleFunc("GET /api/conversations/{id}", s.handleGetConversation)
	mux.HandleFunc("PATCH /api/conversations/{id}", s.handlePatchConversation)
	mux.HandleFunc("DELETE /api/conversations/{id}", s.handleDeleteConversation)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.handlePostMessage)
	mux.HandleFunc("POST /api/conversations/{id}/regenerate", s.handleRegenerate)

	// Attachments (per-user, 2.3).
	mux.HandleFunc("POST /api/attachments", s.handleUploadAttachment)
	mux.HandleFunc("GET /api/attachments/{id}", s.handleGetAttachment)
	mux.HandleFunc("DELETE /api/attachments/{id}", s.handleDeleteAttachment)

	// Speech: STT/TTS (features 3.5 / 3.6, per-user).
	mux.HandleFunc("POST /api/stt", s.handleSTT)
	mux.HandleFunc("POST /api/tts", s.handleTTS)
	mux.HandleFunc("GET /api/speech/status", s.handleSpeechStatus)
	// Read-only email mailbox (per-user, feature 3.2).
	mux.HandleFunc("GET /api/me/mail", s.handleGetMyMail)
	mux.HandleFunc("PUT /api/me/mail", s.handlePutMyMail)
	mux.HandleFunc("PATCH /api/me/mail", s.handlePatchMyMail)
	mux.HandleFunc("DELETE /api/me/mail", s.handleDeleteMyMail)
	mux.HandleFunc("POST /api/me/mail/test", s.handleTestMyMail)
	// Image generation (per-user, 3.1).
	mux.HandleFunc("POST /api/images", s.handleCreateImage)
	mux.HandleFunc("GET /api/images/{id}", s.handleGetImage)
	mux.HandleFunc("DELETE /api/images/{id}", s.handleDeleteImage)

	// Admin: users and settings (§9.4).
	mux.HandleFunc("GET /api/users", s.handleListUsers)
	mux.HandleFunc("POST /api/users", s.handleCreateUser)
	mux.HandleFunc("PATCH /api/users/{id}", s.handlePatchUser)
	mux.HandleFunc("DELETE /api/users/{id}", s.handleDeleteUser)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PATCH /api/settings", s.handlePatchSettings)

	mux.Handle("/", s.staticHandler())
	return s.loggingMiddleware(s.auth.Middleware(mux))
}

// currentUser returns the authenticated user attached by the middleware.
func currentUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return store.User{}, false
	}
	return u, true
}

// handleModels returns the static registry sorted by output_per_m ascending.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, models.Sorted())
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	convs, err := s.st.ListConversations(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list conversations", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list conversations")
		return
	}
	// Optional ?project_id=<id|none> filter (§8.3).
	if filter := r.URL.Query().Get("project_id"); filter != "" {
		out := convs[:0]
		for _, c := range convs {
			if filter == "none" {
				if c.ProjectID == nil {
					out = append(out, c)
				}
				continue
			}
			if c.ProjectID != nil && *c.ProjectID == filter {
				out = append(out, c)
			}
		}
		convs = out
	}
	writeJSON(w, http.StatusOK, convs)
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Model     string  `json:"model"`
		Effort    string  `json:"effort"`
		ProjectID *string `json:"project_id"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// A project id must exist and belong to the caller; its default_model can
	// supply a missing model.
	var project *store.Project
	if req.ProjectID != nil && *req.ProjectID != "" {
		p, err := s.st.GetProject(r.Context(), *req.ProjectID, u.ID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "unknown project")
			return
		}
		if err != nil {
			s.log.Error("get project", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load project")
			return
		}
		project = &p
	}
	modelID := req.Model
	if modelID == "" && project != nil && project.DefaultModel != nil {
		modelID = *project.DefaultModel
	}
	m, ok := models.ByID(modelID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown model")
		return
	}
	var effort *string
	if req.Effort != "" {
		if m.Dialect != models.DialectResponses {
			writeError(w, http.StatusBadRequest, "effort is only allowed for responses-dialect models")
			return
		}
		if !models.ValidEffort(req.Effort) {
			writeError(w, http.StatusBadRequest, "invalid effort")
			return
		}
		effort = &req.Effort
	}
	now := time.Now().UnixMilli()
	userID := u.ID
	conv := store.Conversation{
		ID:        newUUID(),
		UserID:    &userID,
		Title:     "",
		Model:     m.ID,
		Effort:    effort,
		ProjectID: req.ProjectID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if conv.ProjectID != nil && *conv.ProjectID == "" {
		conv.ProjectID = nil
	}
	if err := s.st.InsertConversation(r.Context(), &conv); err != nil {
		s.log.Error("create conversation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create conversation")
		return
	}
	writeJSON(w, http.StatusCreated, conv)
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	msgs, err := s.st.ListMessages(r.Context(), conv.ID)
	if err != nil {
		s.log.Error("list messages", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		store.Conversation
		Messages []store.Message `json:"messages"`
	}{conv, msgs})
}

func (s *Server) handlePatchConversation(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	var req struct {
		Title     *string `json:"title"`
		Model     *string `json:"model"`
		Effort    *string `json:"effort"`
		ProjectID *string `json:"project_id"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID != nil {
		if *req.ProjectID == "" {
			conv.ProjectID = nil
		} else {
			if _, err := s.st.GetProject(r.Context(), *req.ProjectID, u.ID); errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "unknown project")
				return
			} else if err != nil {
				s.log.Error("get project", "error", err)
				writeError(w, http.StatusInternalServerError, "failed to load project")
				return
			}
			conv.ProjectID = req.ProjectID
		}
	}
	if req.Title != nil {
		if len([]rune(*req.Title)) > 200 {
			writeError(w, http.StatusBadRequest, "title exceeds 200 characters")
			return
		}
		conv.Title = *req.Title
	}
	if req.Model != nil {
		m, ok := models.ByID(*req.Model)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown model")
			return
		}
		conv.Model = m.ID
	}
	if req.Effort != nil {
		if *req.Effort == "" {
			conv.Effort = nil
		} else {
			if !models.ValidEffort(*req.Effort) {
				writeError(w, http.StatusBadRequest, "invalid effort")
				return
			}
			m, _ := models.ByID(conv.Model)
			if m.Dialect != models.DialectResponses {
				writeError(w, http.StatusBadRequest, "effort is only valid for responses-dialect models")
				return
			}
			conv.Effort = req.Effort
		}
	}
	// Effort is meaningless on chat-dialect models; keep the row consistent
	// when the model was switched across dialects.
	if m, ok := models.ByID(conv.Model); ok && m.Dialect != models.DialectResponses {
		conv.Effort = nil
	}
	conv.UpdatedAt = time.Now().UnixMilli()
	if err := s.st.UpdateConversation(r.Context(), &conv); err != nil {
		s.log.Error("update conversation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update conversation")
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	n, err := s.st.DeleteConversation(r.Context(), r.PathValue("id"), u.ID)
	if err != nil {
		s.log.Error("delete conversation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete conversation")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getConversation loads the {id} path conversation owned by userID, writing
// 404/500 on failure.
func (s *Server) getConversation(w http.ResponseWriter, r *http.Request, userID string) (store.Conversation, bool) {
	conv, err := s.st.GetConversation(r.Context(), r.PathValue("id"), userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "conversation not found")
		return store.Conversation{}, false
	}
	if err != nil {
		s.log.Error("get conversation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load conversation")
		return store.Conversation{}, false
	}
	return conv, true
}

// staticHandler serves the embedded SPA; unknown paths fall back to
// index.html. Static files are public (the SPA shows its own login screen).
func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if info, err := fs.Stat(s.static, name); err != nil || info.IsDir() {
			name = "index.html"
		}
		if strings.HasSuffix(name, ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		http.ServeFileFS(w, r, s.static, name)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return json.NewDecoder(r.Body).Decode(v)
}

// newUUID returns a random RFC 4122 version-4 UUID (crypto/rand, no deps).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// logWriter captures the response status for the request log while
// preserving http.Flusher for SSE streaming.
type logWriter struct {
	http.ResponseWriter
	status int
}

func (w *logWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE keeps working through the
// logging wrapper.
func (w *logWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lw.status,
			"ms", time.Since(start).Milliseconds(),
		)
	})
}
