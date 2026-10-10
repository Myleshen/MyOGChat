package server

import (
	"errors"
	"net/http"
	"strconv"
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
		// Per-user quota overrides (2.11): omitted = unchanged, null =
		// clear/inherit, number = override.
		DailyTokenLimit  nullableInt64 `json:"daily_token_limit"`
		MonthlyCostLimit nullableInt64 `json:"monthly_cost_limit"`
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
	// Per-user quota overrides (2.11): apply only the fields that were present.
	if req.DailyTokenLimit.Set || req.MonthlyCostLimit.Set {
		daily, monthly := target.DailyTokenLimit, target.MonthlyCostLimit
		if req.DailyTokenLimit.Set {
			daily = req.DailyTokenLimit.Value
		}
		if req.MonthlyCostLimit.Set {
			monthly = req.MonthlyCostLimit.Value
		}
		if (daily != nil && *daily < 0) || (monthly != nil && *monthly < 0) {
			writeError(w, http.StatusBadRequest, "quota limits must be >= 0")
			return
		}
		if err := s.st.SetUserLimits(r.Context(), target.ID, daily, monthly, now); err != nil {
			s.log.Error("set user limits", "error", err)
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
		AllowRegistration *bool   `json:"allow_registration"`
		ShareServerKey    *bool   `json:"share_server_key"`
		ToolsEnabled      *bool   `json:"tools_enabled"`
		LinkwardenURL     *string `json:"linkwarden_url"`
		LinkwardenToken   *string `json:"linkwarden_token"`
		SearchProvider    *string `json:"search_provider"`
		BraveKey          *string `json:"brave_key"`
		SandboxEnabled    *bool   `json:"sandbox_enabled"`
		OCREnabled        *bool   `json:"ocr_enabled"` // feature 2.5
		// Quota limits (2.11); 0 = off.
		DailyTokenLimit  *int64 `json:"daily_token_limit"`
		MonthlyCostLimit *int64 `json:"monthly_cost_limit"`
		// Maps (feature 3.4).
		MapsProvider *string `json:"maps_provider"`
		MapsRegion   *string `json:"maps_region"`
		MapsUnits    *string `json:"maps_units"`
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
	if req.ShareServerKey != nil {
		v := "0"
		if *req.ShareServerKey {
			v = "1"
		}
		if err := s.st.SetSetting(r.Context(), "share_server_key", v); err != nil {
			s.log.Error("set setting", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.ToolsEnabled != nil {
		if err := s.setToolsEnabled(r.Context(), *req.ToolsEnabled); err != nil {
			s.log.Error("set tools_enabled", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.SandboxEnabled != nil {
		if err := s.setSandboxEnabled(r.Context(), *req.SandboxEnabled); err != nil {
			s.log.Error("set sandbox_enabled", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.OCREnabled != nil {
		if err := s.setOCREnabled(r.Context(), *req.OCREnabled); err != nil {
			s.log.Error("set ocr_enabled", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.LinkwardenURL != nil || req.LinkwardenToken != nil {
		rawURL := ""
		if req.LinkwardenURL != nil {
			rawURL = *req.LinkwardenURL
		} else if v, _, err := s.st.GetSetting(r.Context(), settingLinkwardenURL); err == nil {
			rawURL = v
		}
		if err := s.setLinkwarden(r.Context(), rawURL, req.LinkwardenToken); err != nil {
			s.log.Error("set linkwarden settings", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.SearchProvider != nil {
		if err := s.setSearchProvider(r.Context(), *req.SearchProvider); err != nil {
			s.log.Error("set search_provider", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.BraveKey != nil {
		if err := s.setBraveKey(r.Context(), *req.BraveKey); err != nil {
			s.log.Error("set brave_key", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.DailyTokenLimit != nil {
		if *req.DailyTokenLimit < 0 {
			writeError(w, http.StatusBadRequest, "daily_token_limit must be >= 0")
			return
		}
		if err := s.st.SetSetting(r.Context(), settingDailyTokenLimit, strconv.FormatInt(*req.DailyTokenLimit, 10)); err != nil {
			s.log.Error("set daily_token_limit", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.MapsProvider != nil {
		if err := s.setMapsProvider(r.Context(), *req.MapsProvider); err != nil {
			s.log.Error("set maps_provider", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.MonthlyCostLimit != nil {
		if *req.MonthlyCostLimit < 0 {
			writeError(w, http.StatusBadRequest, "monthly_cost_limit must be >= 0")
			return
		}
		if err := s.st.SetSetting(r.Context(), settingMonthlyCostLimit, strconv.FormatInt(*req.MonthlyCostLimit, 10)); err != nil {
			s.log.Error("set monthly_cost_limit", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.MapsRegion != nil {
		if err := s.setMapsRegion(r.Context(), *req.MapsRegion); err != nil {
			s.log.Error("set maps_region", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update settings")
			return
		}
	}
	if req.MapsUnits != nil {
		if err := s.setMapsUnits(r.Context(), *req.MapsUnits); err != nil {
			s.log.Error("set maps_units", "error", err)
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
	share, err := s.shareServerKey(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read settings")
		return
	}
	out := map[string]any{
		"allow_registration": open,
		"pending_users":      pending,
		"share_server_key":   share,
		"server_key_present": s.serverKey != "",
	}
	for k, v := range s.integrationSettings(r.Context()) {
		out[k] = v
	}
	// Feature 2.5: image OCR (self-contained block).
	for k, v := range s.ocrSettings(r.Context()) {
		out[k] = v
	}
	// 2.11: quota limits (self-contained block).
	for k, v := range s.usageSettings(r.Context()) {
		out[k] = v
	}
	// Feature 3.4: maps (self-contained block).
	for k, v := range s.mapsSettings(r.Context()) {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}
