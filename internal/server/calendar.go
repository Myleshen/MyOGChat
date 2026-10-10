package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/calendar"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// calendarBackend builds the caller's calendar backend from their stored
// account, decrypting the credential. ok is false when nothing is connected or
// the credential cannot be read.
func (s *Server) calendarBackend(ctx context.Context, userID string) (calendar.Backend, bool) {
	acc, err := s.st.GetCalendarAccount(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false
	}
	if err != nil || acc.CredentialEnc == "" {
		return nil, false
	}
	raw, err := secret.Decrypt(s.encKey, acc.CredentialEnc)
	if err != nil {
		s.log.Warn("calendar credential could not be decrypted", "error", err)
		return nil, false
	}
	cfg := calendar.Config{
		Kind:       calendar.Kind(acc.Kind),
		CalendarID: acc.CalendarID,
		Endpoint:   acc.Endpoint,
		Username:   acc.Username,
	}
	switch cfg.Kind {
	case calendar.KindCalDAV:
		cfg.Password = string(raw)
	case calendar.KindICS:
		cfg.URL = string(raw)
	default:
		return nil, false
	}
	b, err := calendar.New(cfg)
	if err != nil {
		s.log.Warn("calendar backend unavailable", "error", err)
		return nil, false
	}
	return b, true
}

// calendarStatusJSON is the client-facing account shape (never the secret).
func calendarStatusJSON(acc store.CalendarAccount) map[string]any {
	return map[string]any{
		"connected":   true,
		"kind":        acc.Kind,
		"provider":    acc.Provider,
		"calendar_id": acc.CalendarID,
		"username":    acc.Username,
		"endpoint":    acc.Endpoint,
		"can_write":   acc.Kind == string(calendar.KindCalDAV),
	}
}

// handleGetCalendar reports the caller's connected calendar (3.3).
func (s *Server) handleGetCalendar(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	acc, err := s.st.GetCalendarAccount(r.Context(), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"connected": false})
		return
	}
	if err != nil {
		s.log.Error("get calendar account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load calendar")
		return
	}
	writeJSON(w, http.StatusOK, calendarStatusJSON(acc))
}

// handlePutCalendar connects or replaces the caller's calendar after validating
// it against the provider (3.3).
func (s *Server) handlePutCalendar(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind       string `json:"kind"`
		Provider   string `json:"provider"`
		Endpoint   string `json:"endpoint"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		URL        string `json:"url"`
		CalendarID string `json:"calendar_id"`
	}
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	kind := calendar.Kind(strings.ToLower(strings.TrimSpace(req.Kind)))
	cfg := calendar.Config{
		Kind:       kind,
		CalendarID: strings.TrimSpace(req.CalendarID),
	}
	var credential string
	switch kind {
	case calendar.KindCalDAV:
		cfg.Endpoint = strings.TrimSpace(req.Endpoint)
		cfg.Username = strings.TrimSpace(req.Username)
		cfg.Password = req.Password
		if cfg.Endpoint == "" {
			writeError(w, http.StatusBadRequest, "server URL is required")
			return
		}
		if cfg.Username == "" {
			writeError(w, http.StatusBadRequest, "username is required")
			return
		}
		if strings.TrimSpace(cfg.Password) == "" {
			writeError(w, http.StatusBadRequest, "app-specific password is required")
			return
		}
		credential = cfg.Password
	case calendar.KindICS:
		cfg.URL = strings.TrimSpace(req.URL)
		if cfg.URL == "" {
			writeError(w, http.StatusBadRequest, "calendar URL is required")
			return
		}
		credential = cfg.URL
	default:
		writeError(w, http.StatusBadRequest, "kind must be 'caldav' or 'ics'")
		return
	}

	// Validate by connecting: this proves the credential works and (for CalDAV)
	// resolves the endpoint + default calendar so later calls skip discovery.
	disc, err := calendar.Discover(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not connect to the calendar: "+err.Error())
		return
	}
	if cfg.CalendarID == "" {
		cfg.CalendarID = disc.CalendarID
	}
	enc, err := secret.Encrypt(s.encKey, []byte(credential))
	if err != nil {
		s.log.Error("encrypt calendar credential", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store calendar")
		return
	}
	now := time.Now().UnixMilli()
	acc := store.CalendarAccount{
		ID:            newUUID(),
		UserID:        u.ID,
		Kind:          string(kind),
		Provider:      strings.TrimSpace(req.Provider),
		CalendarID:    cfg.CalendarID,
		Username:      cfg.Username,
		CredentialEnc: enc,
		Endpoint:      disc.Endpoint,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if existing, err := s.st.GetCalendarAccount(r.Context(), u.ID); err == nil {
		acc.ID = existing.ID
		acc.CreatedAt = existing.CreatedAt
	}
	if err := s.st.UpsertCalendarAccount(r.Context(), &acc); err != nil {
		s.log.Error("store calendar account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store calendar")
		return
	}
	out := calendarStatusJSON(acc)
	out["calendars"] = disc.Calendars
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteCalendar disconnects the caller's calendar (3.3).
func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if _, err := s.st.DeleteCalendarAccount(r.Context(), u.ID); err != nil {
		s.log.Error("delete calendar account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to disconnect calendar")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
