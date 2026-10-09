package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/models"
	"myogchat/internal/store"
)

const (
	maxProjectNameRunes  = 120
	maxInstructionsRunes = 8000
)

// handleListProjects returns the caller's projects with conversation counts (§8.2).
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	projects, err := s.st.ListProjects(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list projects", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list projects")
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

// handleCreateProject creates a project owned by the caller (§8.2).
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Name         string  `json:"name"`
		Instructions string  `json:"instructions"`
		DefaultModel *string `json:"default_model"`
	}
	if err := decodeJSON(w, r, &req, 64*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len([]rune(name)) > maxProjectNameRunes {
		writeError(w, http.StatusBadRequest, "name exceeds 120 characters")
		return
	}
	if len([]rune(req.Instructions)) > maxInstructionsRunes {
		writeError(w, http.StatusBadRequest, "instructions exceed 8000 characters")
		return
	}
	dm, ok := validDefaultModel(w, req.DefaultModel)
	if !ok {
		return
	}
	now := time.Now().UnixMilli()
	userID := u.ID
	p := store.Project{
		ID:           newUUID(),
		UserID:       &userID,
		Name:         name,
		Instructions: req.Instructions,
		DefaultModel: dm,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.st.InsertProject(r.Context(), &p); err != nil {
		s.log.Error("create project", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create project")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handlePatchProject updates the caller's project (§8.2).
func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	p, ok := s.getProject(w, r, u.ID)
	if !ok {
		return
	}
	var req struct {
		Name         *string `json:"name"`
		Instructions *string `json:"instructions"`
		DefaultModel *string `json:"default_model"`
		SortOrder    *int64  `json:"sort_order"`
	}
	if err := decodeJSON(w, r, &req, 64*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		if len([]rune(name)) > maxProjectNameRunes {
			writeError(w, http.StatusBadRequest, "name exceeds 120 characters")
			return
		}
		p.Name = name
	}
	if req.Instructions != nil {
		if len([]rune(*req.Instructions)) > maxInstructionsRunes {
			writeError(w, http.StatusBadRequest, "instructions exceed 8000 characters")
			return
		}
		p.Instructions = *req.Instructions
	}
	if req.DefaultModel != nil {
		dm, ok := validDefaultModel(w, req.DefaultModel)
		if !ok {
			return
		}
		p.DefaultModel = dm
	}
	if req.SortOrder != nil {
		p.SortOrder = *req.SortOrder
	}
	p.UpdatedAt = time.Now().UnixMilli()
	if err := s.st.UpdateProject(r.Context(), &p); err != nil {
		s.log.Error("update project", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update project")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleDeleteProject deletes the caller's project; its chats move to Unfiled.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if _, ok := s.getProject(w, r, u.ID); !ok {
		return
	}
	if err := s.st.DeleteProject(r.Context(), r.PathValue("id"), u.ID); err != nil {
		s.log.Error("delete project", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getProject loads the {id} path project owned by userID, writing 404/500.
func (s *Server) getProject(w http.ResponseWriter, r *http.Request, userID string) (store.Project, bool) {
	p, err := s.st.GetProject(r.Context(), r.PathValue("id"), userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project not found")
		return store.Project{}, false
	}
	if err != nil {
		s.log.Error("get project", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load project")
		return store.Project{}, false
	}
	return p, true
}

// validDefaultModel validates an optional default-model reference. nil and ""
// both mean "no default".
func validDefaultModel(w http.ResponseWriter, m *string) (*string, bool) {
	if m == nil || *m == "" {
		return nil, true
	}
	if _, ok := models.ByID(*m); !ok {
		writeError(w, http.StatusBadRequest, "unknown model")
		return nil, false
	}
	return m, true
}
