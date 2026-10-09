// Package server wires the HTTP API (contract §3), auth middleware,
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
}

// New builds a Server.
func New(st *store.Store, up *upstream.Client, a *auth.Auth, staticFS fs.FS, log *slog.Logger) *Server {
	return &Server{st: st, up: up, auth: a, static: staticFS, log: log}
}

// Handler returns the root handler: routes + auth + request logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.auth.Login)
	mux.HandleFunc("POST /api/logout", s.auth.Logout)
	mux.HandleFunc("GET /api/me", s.auth.Me)
	mux.HandleFunc("GET /api/models", s.handleModels)
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
	mux.Handle("/", s.staticHandler())
	return s.loggingMiddleware(s.auth.Middleware(mux))
}

// handleModels returns the static registry sorted by output_per_m ascending.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, models.Sorted())
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	convs, err := s.st.ListConversations(r.Context())
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
	var req struct {
		Model     string  `json:"model"`
		Effort    string  `json:"effort"`
		ProjectID *string `json:"project_id"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// A project id must exist; its default_model can supply a missing model.
	var project *store.Project
	if req.ProjectID != nil && *req.ProjectID != "" {
		p, err := s.st.GetProject(r.Context(), *req.ProjectID)
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
	conv := store.Conversation{
		ID:        newUUID(),
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
	conv, ok := s.getConversation(w, r)
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
	conv, ok := s.getConversation(w, r)
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
			if _, err := s.st.GetProject(r.Context(), *req.ProjectID); errors.Is(err, store.ErrNotFound) {
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
	if err := s.st.DeleteConversation(r.Context(), r.PathValue("id")); err != nil {
		s.log.Error("delete conversation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete conversation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getConversation loads the {id} path conversation, writing 404/500 on failure.
func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) (store.Conversation, bool) {
	conv, err := s.st.GetConversation(r.Context(), r.PathValue("id"))
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
// index.html. Static files are public (contract §1).
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
		// crypto/rand only fails if the OS CSPRNG is broken; nothing
		// sensible to fall back to.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
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
