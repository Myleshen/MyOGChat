package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/calendar"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// calendarAccountConfig builds the caller's calendar config from their stored
// account, decrypting the credential. ok is false when nothing is connected or
// the credential cannot be read.
func (s *Server) calendarAccountConfig(ctx context.Context, userID string) (calendar.Config, store.CalendarAccount, bool) {
	acc, err := s.st.GetCalendarAccount(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return calendar.Config{}, store.CalendarAccount{}, false
	}
	if err != nil || acc.CredentialEnc == "" {
		return calendar.Config{}, store.CalendarAccount{}, false
	}
	raw, err := secret.Decrypt(s.encKey, acc.CredentialEnc)
	if err != nil {
		s.log.Warn("calendar credential could not be decrypted", "error", err)
		return calendar.Config{}, store.CalendarAccount{}, false
	}
	cfg, ok := s.calendarConfigFor(ctx, acc, raw)
	if !ok {
		return calendar.Config{}, store.CalendarAccount{}, false
	}
	return cfg, acc, true
}

// calendarConfigFor builds a backend config from a stored account and its
// decrypted credential. It is split out so a caller holding an
// already-joined account row (the calendar-trigger scheduler) needs no second
// query. ok is false for an unknown kind.
func (s *Server) calendarConfigFor(ctx context.Context, acc store.CalendarAccount, raw []byte) (calendar.Config, bool) {
	cfg := calendar.Config{
		Kind:       calendar.Kind(acc.Kind),
		CalendarID: acc.CalendarID,
		Endpoint:   acc.Endpoint,
		Username:   acc.Username,
	}
	switch cfg.Kind {
	case calendar.KindCalDAV:
		// The stored calendar_id is the default collection, and the cached home
		// set means routine tool calls need no discovery at all.
		cfg.CalendarPath = acc.CalendarID
		cfg.HomeSet = acc.HomeSet
		cfg.Password = string(raw)
	case calendar.KindICS:
		cfg.URL = string(raw)
	case calendar.KindGoogle:
		var tok calendar.Token
		if err := json.Unmarshal(raw, &tok); err != nil {
			s.log.Warn("google calendar token could not be decoded", "error", err)
			return calendar.Config{}, false
		}
		g := s.google.withDefaults()
		cfg.ClientID = g.ClientID
		cfg.ClientSecret = g.ClientSecret
		cfg.Token = tok
		cfg.TokenURL = g.tokenURL
		cfg.APIBase = g.apiBase
		cfg.HTTPClient = g.hc
		// Keep the row current when the backend refreshes the access token.
		accID := acc.ID
		cfg.SaveToken = func(t calendar.Token) error {
			return s.saveGoogleToken(ctx, acc.UserID, accID, t)
		}
	default:
		return calendar.Config{}, false
	}
	return cfg, true
}

// calendarBackend builds the caller's calendar backend from their stored
// account. ok is false when nothing is connected or it cannot be read.
func (s *Server) calendarBackend(ctx context.Context, userID string) (calendar.Backend, bool) {
	cfg, _, ok := s.calendarAccountConfig(ctx, userID)
	if !ok {
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
		writeJSON(w, http.StatusOK, map[string]any{
			"connected":        false,
			"google_available": s.google.Available(),
		})
		return
	}
	if err != nil {
		s.log.Error("get calendar account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load calendar")
		return
	}
	out := calendarStatusJSON(acc)
	out["google_available"] = s.google.Available()
	writeJSON(w, http.StatusOK, out)
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
	case calendar.KindGoogle:
		// Google accounts are created only by the OAuth callback: the client
		// never holds a token to PUT.
		writeError(w, http.StatusBadRequest, "Google Calendar is connected with its own sign-in, not with a credential")
		return
	default:
		writeError(w, http.StatusBadRequest, "kind must be 'caldav' or 'ics'")
		return
	}

	// Validate by connecting: this proves the credential works and (for CalDAV)
	// resolves the endpoint, home set and default calendar so later calls skip
	// discovery.
	disc, err := calendar.Discover(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not connect to the calendar: "+err.Error())
		return
	}
	// A requested collection must be one the server actually exposes.
	if kind == calendar.KindCalDAV && cfg.CalendarID != "" && !calendar.CalendarExists(disc.Calendars, cfg.CalendarID) {
		writeError(w, http.StatusBadRequest, "unknown calendar")
		return
	}
	// disc.CalendarID is the canonical default collection (or ICS label).
	cfg.CalendarID = disc.CalendarID
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
		HomeSet:       disc.HomeSet,
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

// handleListCalendars lists the account's calendars so the client can pick a
// default one (3.3). Cheap on CalDAV once the home set is cached.
func (s *Server) handleListCalendars(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	cfg, acc, ok := s.calendarAccountConfig(r.Context(), u.ID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"connected": false})
		return
	}
	b, err := calendar.New(cfg)
	if err != nil {
		s.log.Error("calendar backend unavailable", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load calendar")
		return
	}
	cals, err := b.Calendars(r.Context())
	if err != nil {
		s.log.Warn("list calendars", "error", err)
		writeError(w, http.StatusBadGateway, "could not list calendars: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"default":   acc.CalendarID,
		"calendars": cals,
	})
}

// handlePatchCalendar changes the default collection without re-entering the
// credential (3.3). ICS feeds expose exactly one calendar, so they have nothing
// to switch to.
func (s *Server) handlePatchCalendar(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	cfg, acc, ok := s.calendarAccountConfig(r.Context(), u.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "no calendar connected")
		return
	}
	var req struct {
		CalendarID string `json:"calendar_id"`
	}
	if err := decodeJSON(w, r, &req, 4*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if cfg.Kind == calendar.KindICS {
		writeError(w, http.StatusBadRequest, "ICS feeds expose a single calendar")
		return
	}
	id := strings.TrimSpace(req.CalendarID)
	if id == "" {
		writeError(w, http.StatusBadRequest, "calendar_id is required")
		return
	}
	// Discovery doubles as validation: it lists the account's calendars and
	// refreshes the cached endpoint/home set if the account predates the cache.
	cfg.CalendarPath = id
	disc, err := calendar.Discover(r.Context(), cfg)
	if err != nil {
		s.log.Warn("calendar discovery", "error", err)
		writeError(w, http.StatusBadGateway, "could not reach the calendar: "+err.Error())
		return
	}
	if !calendar.CalendarExists(disc.Calendars, id) {
		writeError(w, http.StatusBadRequest, "unknown calendar")
		return
	}
	acc.CalendarID = disc.CalendarID
	acc.Endpoint = disc.Endpoint
	acc.HomeSet = disc.HomeSet
	acc.UpdatedAt = time.Now().UnixMilli()
	if err := s.st.UpsertCalendarAccount(r.Context(), &acc); err != nil {
		s.log.Error("store calendar account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store calendar")
		return
	}
	writeJSON(w, http.StatusOK, calendarStatusJSON(acc))
}

// handleDeleteCalendar disconnects the caller's calendar (3.3). A Google token
// is revoked first, best-effort.
func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if acc, err := s.st.GetCalendarAccount(r.Context(), u.ID); err == nil && acc.Kind == string(calendar.KindGoogle) {
		s.revokeGoogleToken(r.Context(), acc)
	}
	if _, err := s.st.DeleteCalendarAccount(r.Context(), u.ID); err != nil {
		s.log.Error("delete calendar account", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to disconnect calendar")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
