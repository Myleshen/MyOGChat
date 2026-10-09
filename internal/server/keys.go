package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// errNoKey is returned when a request has no usable API key (§2.1).
var errNoKey = errors.New("no OpenCode Go API key configured — add one in Settings")

// shareServerKey reports whether the shared server key may be used by users who
// have not set their own (admin-controlled, default on).
func (s *Server) shareServerKey(ctx context.Context) (bool, error) {
	v, ok, err := s.st.GetSetting(ctx, "share_server_key")
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	return v == "1", nil
}

// resolveKey returns the bearer key for u using the §2.2 order: the user's own
// key, else a group key (the oldest applicable group whose owner shares a key),
// else the shared server key when sharing is enabled, else errNoKey.
func (s *Server) resolveKey(ctx context.Context, u store.User) (string, error) {
	if u.APIKeyEnc != nil && *u.APIKeyEnc != "" {
		raw, err := secret.Decrypt(s.encKey, *u.APIKeyEnc)
		if err != nil {
			return "", errors.New("your stored key could not be read; please set it again")
		}
		return string(raw), nil
	}
	if key, _, ok, err := s.groupKey(ctx, u.ID); err != nil {
		return "", err
	} else if ok {
		return key, nil
	}
	if s.serverKey != "" {
		share, err := s.shareServerKey(ctx)
		if err != nil {
			return "", err
		}
		if share {
			return s.serverKey, nil
		}
	}
	return "", errNoKey
}

// groupKey returns the first usable shared key among the groups userID belongs
// to, in created_at order, together with that group's name. A group contributes
// a key only when it is shared and its owner has a readable stored key (§2.2).
func (s *Server) groupKey(ctx context.Context, userID string) (key, groupName string, ok bool, err error) {
	groups, err := s.st.ListKeyGroupsForUser(ctx, userID)
	if err != nil {
		return "", "", false, err
	}
	for _, g := range groups {
		owner, err := s.st.GetUserByID(ctx, g.OwnerID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", "", false, err
		}
		if owner.APIKeyEnc == nil || *owner.APIKeyEnc == "" {
			continue
		}
		raw, err := secret.Decrypt(s.encKey, *owner.APIKeyEnc)
		if err != nil {
			// Owner's key is unreadable; fall through to the next group.
			continue
		}
		return string(raw), g.Name, true, nil
	}
	return "", "", false, nil
}

// keyHint returns the last 4 characters of a key, for display only.
func keyHint(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 4 {
		return ""
	}
	return key[len(key)-4:]
}

// handleGetMyKey reports the caller's key status (§2.1).
func (s *Server) handleGetMyKey(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	share, err := s.shareServerKey(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read settings")
		return
	}
	hasOwn := u.APIKeyEnc != nil && *u.APIKeyEnc != ""
	hint := ""
	if hasOwn {
		if raw, err := secret.Decrypt(s.encKey, *u.APIKeyEnc); err == nil {
			hint = keyHint(string(raw))
		}
	}
	_, groupName, groupOK, err := s.groupKey(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve group key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_key":              hasOwn,
		"key_hint":             hint,
		"server_key_present":   s.serverKey != "",
		"shared_key_available": s.serverKey != "" && share,
		"using_shared_key":     !hasOwn && !groupOK && s.serverKey != "" && share,
		"group_key_available":  groupOK,
		"using_group_key":      !hasOwn && groupOK,
		"group_name":           groupName,
	})
}

// handlePutMyKey sets or clears the caller's OpenCode Go key (§2.1). A non-empty
// key is validated against the upstream before it is stored.
func (s *Server) handlePutMyKey(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	key := strings.TrimSpace(req.APIKey)
	now := time.Now().UnixMilli()
	if key == "" {
		if err := s.st.SetUserAPIKey(r.Context(), u.ID, nil, now); err != nil {
			s.log.Error("clear api key", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update key")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"has_key": false})
		return
	}
	if err := s.up.ValidateKey(r.Context(), key); err != nil {
		writeError(w, http.StatusBadRequest, "key check failed: "+err.Error())
		return
	}
	enc, err := secret.Encrypt(s.encKey, []byte(key))
	if err != nil {
		s.log.Error("encrypt api key", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store key")
		return
	}
	if err := s.st.SetUserAPIKey(r.Context(), u.ID, &enc, now); err != nil {
		s.log.Error("store api key", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"has_key": true, "key_hint": keyHint(key)})
}
