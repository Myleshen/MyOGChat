package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/store"
)

// maxGroupName bounds a group's display name.
const maxGroupName = 120

// handleListGroups returns the groups the caller owns or belongs to (§2.2).
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	groups, err := s.st.ListGroupsForUser(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list groups", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list groups")
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

// handleCreateGroup creates a group owned by the caller (§2.2).
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Name     string `json:"name"`
		ShareKey *bool  `json:"share_key"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len([]rune(name)) > maxGroupName {
		writeError(w, http.StatusBadRequest, "name exceeds 120 characters")
		return
	}
	// Sharing is on by default (schema default), matching the "use my key for
	// this group" intent.
	share := true
	if req.ShareKey != nil {
		share = *req.ShareKey
	}
	now := time.Now().UnixMilli()
	g := store.Group{
		ID: newUUID(), Name: name, OwnerID: u.ID, ShareKey: share,
		CreatedAt: now, UpdatedAt: now, IsOwner: true,
		Members: []store.GroupMember{},
	}
	if err := s.st.InsertGroup(r.Context(), &g); err != nil {
		s.log.Error("create group", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create group")
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// handlePatchGroup updates a group's name and/or sharing flag (owner only).
func (s *Server) handlePatchGroup(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	g, ok := s.getOwnedGroup(w, r, u.ID)
	if !ok {
		return
	}
	var req struct {
		Name     *string `json:"name"`
		ShareKey *bool   `json:"share_key"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		if len([]rune(name)) > maxGroupName {
			writeError(w, http.StatusBadRequest, "name exceeds 120 characters")
			return
		}
		g.Name = name
	}
	if req.ShareKey != nil {
		g.ShareKey = *req.ShareKey
	}
	g.UpdatedAt = time.Now().UnixMilli()
	if err := s.st.UpdateGroup(r.Context(), &g); err != nil {
		s.log.Error("update group", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update group")
		return
	}
	g.IsOwner = true
	writeJSON(w, http.StatusOK, g)
}

// handleDeleteGroup deletes a group (owner only); memberships cascade.
func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	g, ok := s.getOwnedGroup(w, r, u.ID)
	if !ok {
		return
	}
	if _, err := s.st.DeleteGroup(r.Context(), g.ID, u.ID); err != nil {
		s.log.Error("delete group", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAddGroupMember adds a user to a group by username or email (owner only).
func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	g, ok := s.getOwnedGroup(w, r, u.ID)
	if !ok {
		return
	}
	var req struct {
		Identifier string `json:"identifier"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ident := strings.TrimSpace(req.Identifier)
	if ident == "" {
		writeError(w, http.StatusBadRequest, "identifier is required")
		return
	}
	target, err := s.st.GetUserByIdentifier(r.Context(), ident)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		s.log.Error("find member", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to find user")
		return
	}
	if target.ID == g.OwnerID {
		writeError(w, http.StatusBadRequest, "the owner is already part of the group")
		return
	}
	if err := s.st.AddGroupMember(r.Context(), g.ID, target.ID, "member", time.Now().UnixMilli()); err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") ||
			strings.Contains(strings.ToUpper(err.Error()), "PRIMARY KEY") {
			writeError(w, http.StatusConflict, "user is already a member")
			return
		}
		s.log.Error("add group member", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to add member")
		return
	}
	updated, err := s.st.GetGroup(r.Context(), g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload group")
		return
	}
	updated.IsOwner = true
	writeJSON(w, http.StatusCreated, updated)
}

// handleRemoveGroupMember removes a member from a group (owner or admin).
func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	g, err := s.st.GetGroup(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if err != nil {
		s.log.Error("get group", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load group")
		return
	}
	if g.OwnerID != u.ID && !u.IsAdmin {
		writeError(w, http.StatusForbidden, "owner or admin required")
		return
	}
	n, err := s.st.RemoveGroupMember(r.Context(), g.ID, r.PathValue("userId"))
	if err != nil {
		s.log.Error("remove group member", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to remove member")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "member not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getOwnedGroup loads the {id} group and enforces owner-only access, writing
// 404/403/500 on failure.
func (s *Server) getOwnedGroup(w http.ResponseWriter, r *http.Request, userID string) (store.Group, bool) {
	g, err := s.st.GetGroup(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "group not found")
		return store.Group{}, false
	}
	if err != nil {
		s.log.Error("get group", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load group")
		return store.Group{}, false
	}
	if g.OwnerID != userID {
		writeError(w, http.StatusForbidden, "owner required")
		return store.Group{}, false
	}
	return g, true
}
