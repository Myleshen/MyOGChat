package server

// Web Push notifications (6.6). A finished reply buzzes the user's phone even
// with the PWA closed — but only when no live SSE client is watching the reply,
// so reading along never notifies. Nothing here can affect a reply: every send
// runs in its own goroutine with a timeout, and every failure is logged at
// Debug and dropped.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"myogchat/internal/push"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

const (
	// settingPushEnabled is the admin switch; on by default.
	settingPushEnabled = "push_enabled"
	// settingVAPIDKey holds the encrypted PKCS#8 VAPID private key (one row).
	settingVAPIDKey = "vapid_private_key"

	// maxConcurrentPushes bounds in-flight HTTP deliveries (the Pi is small).
	maxConcurrentPushes = 4
	// maxPushFailures drops a subscription after this many consecutive errors.
	maxPushFailures = 5
	// pushTimeout bounds one delivery attempt and the bookkeeping around it.
	pushTimeout = 10 * time.Second
)

// pushPayload is the JSON the service worker turns into a notification. It
// deliberately carries no message content: a notification is visible on a lock
// screen, so only the conversation title and a status word are sent.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

// pushSubscriptionSettings is the caller's notification state.
type pushSubscriptionSettings struct {
	Enabled       bool   `json:"enabled"`
	PublicKey     string `json:"public_key"`
	Subscriptions int    `json:"subscriptions"`
	Supported     bool   `json:"supported"`
}

// pushEnabled reports the admin toggle (default on).
func (s *Server) pushEnabled(ctx context.Context) bool {
	v, ok, err := s.st.GetSetting(ctx, settingPushEnabled)
	if err != nil {
		s.log.Error("read push_enabled", "error", err)
		return false
	}
	return !ok || v != "0"
}

// pushSubject is the VAPID contact URL sent to the browser's push service.
func (s *Server) pushSubject() string {
	if u, err := url.Parse(s.google.PublicURL); err == nil && u.Scheme == "https" && u.Host != "" {
		return s.google.PublicURL
	}
	return "mailto:myogchat@localhost"
}

// pushSender returns the process-wide VAPID sender, generating and storing the
// key pair on first use. Both keys stay server-side; the private key is
// encrypted with the same key as per-user API keys.
func (s *Server) pushSender(ctx context.Context) (*push.Sender, error) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if s.pushSnd != nil {
		return s.pushSnd, nil
	}
	der, err := s.vapidKey(ctx)
	if err != nil {
		return nil, err
	}
	snd, err := push.NewSender(der, s.pushSubject())
	if err != nil {
		return nil, err
	}
	snd.Client = s.pushHTTP
	s.pushSnd = snd
	return snd, nil
}

// vapidKey loads the VAPID private key, creating it on first use.
func (s *Server) vapidKey(ctx context.Context) ([]byte, error) {
	enc, ok, err := s.st.GetSetting(ctx, settingVAPIDKey)
	if err != nil {
		return nil, err
	}
	if ok && enc != "" {
		der, err := secret.Decrypt(s.encKey, enc)
		if err != nil {
			return nil, fmt.Errorf("decrypt VAPID key: %w", err)
		}
		return der, nil
	}
	der, _, err := push.GenerateKey()
	if err != nil {
		return nil, err
	}
	stored, err := secret.Encrypt(s.encKey, der)
	if err != nil {
		return nil, fmt.Errorf("encrypt VAPID key: %w", err)
	}
	if err := s.st.SetSetting(ctx, settingVAPIDKey, stored); err != nil {
		return nil, err
	}
	return der, nil
}

// pushLimiter returns the shared send semaphore (four concurrent deliveries).
func (s *Server) pushLimiter() chan struct{} {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if s.pushSem == nil {
		s.pushSem = make(chan struct{}, maxConcurrentPushes)
	}
	return s.pushSem
}

// notifyReply queues a notification for a finished reply job. It is a no-op
// when a live SSE client watched the reply, when the job left nothing to read,
// or when the admin switched notifications off.
func (s *Server) notifyReply(j *genJob, conv store.Conversation) {
	status, title, ok := j.pushNotice(conv.Title)
	if !ok {
		return
	}
	if title == "" {
		title = "New chat"
	}
	body := "Reply ready"
	switch status {
	case jobError:
		body = "Job failed"
	case jobStopped:
		body = "Job stopped"
	}
	s.queuePush(j.userID, pushPayload{Title: title, Body: body, URL: "/#/c/" + j.convID, Tag: j.convID})
}

// queuePush sends p to every subscription the user has, without blocking the
// caller.
func (s *Server) queuePush(userID string, p pushPayload) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		if !s.pushEnabled(ctx) {
			return
		}
		subs, err := s.st.ListPushSubscriptions(ctx, userID)
		if err != nil {
			s.log.Debug("list push subscriptions", "error", err)
			return
		}
		if len(subs) == 0 {
			return
		}
		snd, err := s.pushSender(ctx)
		if err != nil {
			s.log.Debug("push sender unavailable", "error", err)
			return
		}
		payload, err := json.Marshal(p)
		if err != nil {
			s.log.Debug("marshal push payload", "error", err)
			return
		}
		for _, sub := range subs {
			s.deliverPush(snd, sub, payload)
		}
	}()
}

// deliverPush sends one payload in the background. When all four slots are
// busy the notification is dropped rather than queued: a late notification is
// worse than none.
func (s *Server) deliverPush(snd *push.Sender, sub store.PushSubscription, payload []byte) {
	select {
	case s.pushLimiter() <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-s.pushLimiter() }()
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		err := snd.Send(ctx, push.Subscription{
			Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
		}, payload)
		s.recordPushResult(sub.ID, err)
	}()
}

// recordPushResult books one delivery: reset the failure streak, delete a
// subscription the push service no longer knows, or drop one that keeps
// failing.
func (s *Server) recordPushResult(id string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	if err == nil {
		if err := s.st.MarkPushSent(ctx, id); err != nil {
			s.log.Debug("mark push sent", "error", err)
		}
		return
	}
	// Never log the endpoint or keys: only the row id and the error.
	s.log.Debug("push delivery failed", "subscription", id, "error", err)
	if push.IsGone(err) {
		if err := s.st.DeletePushSubscriptionByID(ctx, id); err != nil {
			s.log.Debug("delete gone push subscription", "error", err)
		}
		return
	}
	n, ferr := s.st.MarkPushFailed(ctx, id)
	if ferr != nil {
		s.log.Debug("mark push failed", "error", ferr)
		return
	}
	if n >= maxPushFailures {
		if err := s.st.DeletePushSubscriptionByID(ctx, id); err != nil {
			s.log.Debug("delete failing push subscription", "error", err)
		}
	}
}

// handleNotifications reports the caller's notification state and the VAPID
// public key a browser needs to subscribe.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	count, err := s.st.CountPushSubscriptions(r.Context(), u.ID)
	if err != nil {
		s.log.Error("count push subscriptions", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read notifications")
		return
	}
	out := pushSubscriptionSettings{
		Enabled:       s.pushEnabled(r.Context()),
		Subscriptions: count,
	}
	if snd, err := s.pushSender(r.Context()); err == nil {
		out.Supported = true
		out.PublicKey = snd.PublicKey()
	} else {
		s.log.Debug("push unavailable", "error", err)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePushSubscribe registers (or updates) a browser push subscription.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Endpoint  string `json:"endpoint"`
		P256dh    string `json:"p256dh"`
		Auth      string `json:"auth"`
		UserAgent string `json:"user_agent"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	endpoint, err := url.Parse(req.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		writeError(w, http.StatusBadRequest, "endpoint must be an https URL")
		return
	}
	if err := push.ValidateKeys(req.P256dh, req.Auth); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub := store.PushSubscription{
		ID:        newUUID(),
		UserID:    u.ID,
		Endpoint:  req.Endpoint,
		P256dh:    req.P256dh,
		Auth:      req.Auth,
		UserAgent: truncateRunes(strings.TrimSpace(req.UserAgent), 200),
	}
	id, err := s.st.UpsertPushSubscription(r.Context(), &sub)
	if err != nil {
		s.log.Error("save push subscription", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save subscription")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// handlePushDelete removes one of the caller's subscriptions.
func (s *Server) handlePushDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	deleted, err := s.st.DeletePushSubscription(r.Context(), id, u.ID)
	if err != nil {
		s.log.Error("delete push subscription", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete subscription")
		return
	}
	if !deleted {
		// Idempotent for an id that never existed or is already gone; a row
		// owned by someone else stays hidden (404).
		owner, err := s.st.PushSubscriptionOwner(r.Context(), id)
		if err == nil && owner != u.ID {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("read push subscription owner", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to delete subscription")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePushTest sends a test notification to all of the caller's devices.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	subs, err := s.st.ListPushSubscriptions(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list push subscriptions", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read subscriptions")
		return
	}
	if !s.pushEnabled(r.Context()) {
		writeError(w, http.StatusServiceUnavailable, "notifications are disabled")
		return
	}
	if len(subs) == 0 {
		writeJSON(w, http.StatusOK, map[string]int{"sent": 0, "failed": 0})
		return
	}
	snd, err := s.pushSender(r.Context())
	if err != nil {
		s.log.Debug("push sender unavailable", "error", err)
		writeError(w, http.StatusServiceUnavailable, "notifications are not available")
		return
	}
	payload, err := json.Marshal(pushPayload{
		Title: "MyOGChat", Body: "Notifications are working", URL: "/#/", Tag: "myogchat-test",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to send test")
		return
	}
	sent, failed := 0, 0
	for _, sub := range subs {
		err := snd.Send(r.Context(), push.Subscription{
			Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
		}, payload)
		s.recordPushResult(sub.ID, err)
		if err != nil {
			failed++
		} else {
			sent++
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": sent, "failed": failed})
}
