package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/store"
)

// handleListUsers returns all users (admin only, §9.4).
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, u) {
		return
	}
	users, err := s.st.ListUsers(r.Context())
	if err != nil {
		s.log.Error("list users", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// handleCreateUser creates an already-active user (admin only, §9.4).
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, u) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	username := strings.TrimSpace(req.Username)
	email := strings.TrimSpace(req.Email)
	if msg, ok := auth.ValidateCredentials(username, email, req.Password); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	now := time.Now().UnixMilli()
	nu := store.User{
		ID: newUUID(), Username: username, Email: email, PasswordHash: hash,
		IsAdmin: req.IsAdmin, Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.InsertUser(r.Context(), &nu); err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			writeError(w, http.StatusConflict, "username or email already exists")
			return
		}
		s.log.Error("insert user", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	writeJSON(w, http.StatusCreated, nu)
}

// handlePatchUser updates a user's password, admin flag, or status (admin
// only). It refuses to remove or disable the last active admin.
func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	caller, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, caller) {
		return
	}
	target, err := s.st.GetUserByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		s.log.Error("get user", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	var req struct {
		Password *string `json:"password"`
		IsAdmin  *bool   `json:"is_admin"`
		Status   *string `json:"status"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Guard against locking everyone out of administration.
	demotes := req.IsAdmin != nil && !*req.IsAdmin && target.IsAdmin
	disables := req.Status != nil && *req.Status != "active" && target.IsAdmin
	if (demotes || disables) && target.IsAdmin {
		n, err := s.st.CountAdmins(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check admins")
			return
		}
		if n <= 1 {
			writeError(w, http.StatusBadRequest, "cannot remove or disable the last admin")
			return
		}
	}

	now := time.Now().UnixMilli()
	if req.Password != nil {
		if len(*req.Password) < 8 {
			writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to hash password")
			return
		}
		if err := s.st.UpdateUserPassword(r.Context(), target.ID, hash, now); err != nil {
			s.log.Error("update password", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
			return
		}
	}
	if req.IsAdmin != nil {
		if err := s.st.SetUserAdmin(r.Context(), target.ID, *req.IsAdmin, now); err != nil {
			s.log.Error("set admin", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
			return
		}
	}
	if req.Status != nil {
		switch *req.Status {
		case "active", "pending", "disabled":
		default:
			writeError(w, http.StatusBadRequest, "invalid status")
			return
		}
		if err := s.st.SetUserStatus(r.Context(), target.ID, *req.Status, now); err != nil {
			s.log.Error("set status", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
			return
		}
	}
	updated, err := s.st.GetUserByID(r.Context(), target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload user")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteUser deletes a user (admin only); you cannot delete yourself and
// the last admin cannot be deleted (§9.4).
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	caller, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, caller) {
		return
	}
	id := r.PathValue("id")
	if id == caller.ID {
		writeError(w, http.StatusBadRequest, "cannot delete your own account")
		return
	}
	target, err := s.st.GetUserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if target.IsAdmin {
		n, err := s.st.CountAdmins(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check admins")
			return
		}
		if n <= 1 {
			writeError(w, http.StatusBadRequest, "cannot delete the last admin")
			return
		}
	}
	if err := s.st.DeleteUser(r.Context(), id); err != nil {
		s.log.Error("delete user", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetSettings returns admin-visible settings (§9.4).
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, u) {
		return
	}
	s.writeSettings(w, r)
}

// handlePatchSettings updates admin settings (§9.4).
func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, u) {
		return
	}
	var req struct {
		AllowRegistration *bool `json:"allow_registration"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AllowRegistration != nil {
		v := "0"
		if *req.AllowRegistration {
			v = "1"
		}
		if err := s.st.SetSetting(r.Context(), auth.SettingAllowRegistration, v); err != nil {
			s.log.Error("set setting", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	s.writeSettings(w, r)
}

func (s *Server) writeSettings(w http.ResponseWriter, r *http.Request) {
	open := true
	if v, ok, err := s.st.GetSetting(r.Context(), auth.SettingAllowRegistration); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read settings")
		return
	} else if ok {
		open = v == "1"
	}
	pending, err := s.st.CountPendingUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"allow_registration": open,
		"pending_users":      pending,
	})
}
