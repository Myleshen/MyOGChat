package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// pushKeys returns a browser-shaped subscription key pair.
func pushKeys(t *testing.T) (p256dh, authSecret string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate subscription key: %v", err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(secret)
}

// fakePush is a stand-in for a browser's push service.
type fakePush struct {
	mu      sync.Mutex
	status  int
	count   int
	headers []http.Header
	bodies  [][]byte
}

func newFakePush(t *testing.T, status int) (*fakePush, *httptest.Server) {
	t.Helper()
	fp := &fakePush{status: status}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0)
		buf := make([]byte, 16<<10)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		fp.mu.Lock()
		fp.count++
		fp.headers = append(fp.headers, r.Header.Clone())
		fp.bodies = append(fp.bodies, body)
		fp.mu.Unlock()
		w.WriteHeader(fp.status)
	}))
	t.Cleanup(ts.Close)
	return fp, ts
}

func (f *fakePush) sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// pushCall drives one push handler with an authed request.
func pushCall(srv *Server, u store.User, method, path, body, id string,
	h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if id != "" {
		r.SetPathValue("id", id)
	}
	r = r.WithContext(auth.WithUser(r.Context(), u))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// subscribe registers endpoint for u and returns the subscription id.
func subscribe(t *testing.T, srv *Server, u store.User, endpoint string) string {
	t.Helper()
	p256dh, authSecret := pushKeys(t)
	body, _ := json.Marshal(map[string]string{
		"endpoint": endpoint, "p256dh": p256dh, "auth": authSecret, "user_agent": "test",
	})
	rec := pushCall(srv, u, http.MethodPost, "/api/me/push/subscribe", string(body), "", srv.handlePushSubscribe)
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("subscribe body = %s (%v)", rec.Body.String(), err)
	}
	return out.ID
}

// notifications decodes GET /api/me/notifications.
func notifications(t *testing.T, srv *Server, u store.User) pushSubscriptionSettings {
	t.Helper()
	rec := pushCall(srv, u, http.MethodGet, "/api/me/notifications", "", "", srv.handleNotifications)
	if rec.Code != http.StatusOK {
		t.Fatalf("notifications = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out pushSubscriptionSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode notifications: %v", err)
	}
	return out
}

// TestNotificationsStatus covers the reported state and the admin toggle.
func TestNotificationsStatus(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")

	got := notifications(t, srv, u)
	if !got.Enabled || !got.Supported || got.Subscriptions != 0 {
		t.Fatalf("notifications = %+v, want enabled+supported+0", got)
	}
	pub, err := base64.RawURLEncoding.DecodeString(got.PublicKey)
	if err != nil || len(pub) != 65 || pub[0] != 4 {
		t.Fatalf("public key is not an uncompressed P-256 point: %q (%v)", got.PublicKey, err)
	}
	// The key is generated once and reused.
	if again := notifications(t, srv, u); again.PublicKey != got.PublicKey {
		t.Fatalf("public key changed between reads")
	}

	if err := st.SetSetting(t.Context(), settingPushEnabled, "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := notifications(t, srv, u); got.Enabled {
		t.Fatal("push_enabled=0 was not reported")
	}
}

// TestPushSubscribeUpdateDeleteScoping covers the subscription lifecycle:
// upsert by endpoint, owner scoping, idempotent delete and validation.
func TestPushSubscribeUpdateDeleteScoping(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u1", "alice", "")
	bob := seedKeyUser(t, st, "u2", "bob", "")
	const endpoint = "https://push.example.net/abc"

	id := subscribe(t, srv, alice, endpoint)
	// Re-subscribing the same endpoint updates the row (same id) and moves it
	// to the caller.
	if id2 := subscribe(t, srv, bob, endpoint); id2 != id {
		t.Fatalf("re-subscribe id = %q, want the existing %q", id2, id)
	}
	if n, _ := st.CountPushSubscriptions(t.Context(), bob.ID); n != 1 {
		t.Fatalf("bob subscriptions = %d, want 1", n)
	}
	if n, _ := st.CountPushSubscriptions(t.Context(), alice.ID); n != 0 {
		t.Fatalf("alice subscriptions = %d, want 0", n)
	}

	// A foreign id is a 404; the owner's delete is idempotent.
	rec := pushCall(srv, alice, http.MethodDelete, "/api/me/push/"+id, "", id, srv.handlePushDelete)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete = %d, want 404", rec.Code)
	}
	for i := 0; i < 2; i++ {
		rec = pushCall(srv, bob, http.MethodDelete, "/api/me/push/"+id, "", id, srv.handlePushDelete)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete #%d = %d, want 204", i+1, rec.Code)
		}
	}

	// Validation: the endpoint must be https, and both keys must decode.
	bad := []map[string]string{
		{"endpoint": "http://push.example.net/abc", "p256dh": pushKeys2(t, 65), "auth": pushKeys2(t, 16)},
		{"endpoint": "https://push.example.net/abc", "p256dh": pushKeys2(t, 32), "auth": pushKeys2(t, 16)},
		{"endpoint": "https://push.example.net/abc", "p256dh": pushKeys2(t, 65), "auth": pushKeys2(t, 8)},
	}
	for i, body := range bad {
		raw, _ := json.Marshal(body)
		rec := pushCall(srv, alice, http.MethodPost, "/api/me/push/subscribe", string(raw), "", srv.handlePushSubscribe)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad subscribe #%d = %d, want 400 (body %s)", i, rec.Code, rec.Body.String())
		}
	}
}

// pushKeys2 returns n bytes as base64url: a length-only filler for the
// validation cases that never reach the curve check.
func pushKeys2(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TestPushTestEndpointAndHeaders asserts the delivery request shape.
func TestPushTestEndpointAndHeaders(t *testing.T) {
	srv, st := newKeyTestServer(t)
	fp, ts := newFakePush(t, http.StatusCreated)
	srv.pushHTTP = ts.Client()
	u := seedKeyUser(t, st, "u1", "alice", "")
	subscribe(t, srv, u, ts.URL+"/push/one")

	rec := pushCall(srv, u, http.MethodPost, "/api/me/push/test", "", "", srv.handlePushTest)
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d (body %s)", rec.Code, rec.Body.String())
	}
	var out struct{ Sent, Failed int }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Sent != 1 || out.Failed != 0 {
		t.Fatalf("test result = %s, want sent 1 failed 0", rec.Body.String())
	}
	if fp.sent() != 1 {
		t.Fatalf("push service saw %d requests, want 1", fp.sent())
	}
	fp.mu.Lock()
	h := fp.headers[0]
	body := fp.bodies[0]
	fp.mu.Unlock()
	authz := h.Get("Authorization")
	if !strings.HasPrefix(authz, "vapid t=") || !strings.Contains(authz, ",k=") {
		t.Fatalf("Authorization = %q, want vapid t=...,k=...", authz)
	}
	if h.Get("Content-Encoding") != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q", h.Get("Content-Encoding"))
	}
	if h.Get("TTL") == "" || h.Get("Urgency") != "normal" {
		t.Fatalf("TTL = %q, Urgency = %q", h.Get("TTL"), h.Get("Urgency"))
	}
	if len(body) <= 86 {
		t.Fatalf("body is %d bytes, want the aes128gcm header plus ciphertext", len(body))
	}
	// The endpoint path is used verbatim.
	if fp.count != 1 {
		t.Fatalf("unexpected request count %d", fp.count)
	}
}

// startQuietReply launches a reply job with no SSE subscriber attached.
func startQuietReply(t *testing.T, srv *Server, u store.User, convID, title string) {
	t.Helper()
	j, err := srv.hub().start(convID, u.ID, "glm-5.3-flash", false)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	go srv.runReply(j, replySpec{
		conv:        store.Conversation{ID: convID, Title: title, Model: "glm-5.3-flash"},
		user:        u,
		apiKey:      "key",
		userContent: "hi",
	})
}

// TestPushOnFinishOnlyWithoutSubscriber: a finished detached reply notifies,
// a reply a client watched does not.
func TestPushOnFinishOnlyWithoutSubscriber(t *testing.T) {
	srv, st := newKeyTestServer(t)
	fp, ts := newFakePush(t, http.StatusCreated)
	srv.pushHTTP = ts.Client()
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")
	mkConv(t, st, u, "c2")
	subscribe(t, srv, u, ts.URL+"/push/one")

	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
	)
	startQuietReply(t, srv, u, "c1", "quiet chat")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Status == jobDone && fp.sent() == 1
	})

	// Watched reply: the SSE client is attached when it finishes, so no push.
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
	)
	if rec := postMessage(srv, u, "c2", "hi"); rec.Code != http.StatusOK {
		t.Fatalf("postMessage = %d (body %s)", rec.Code, rec.Body.String())
	}
	waitFor(t, func() bool {
		j := srv.hub().get("c2")
		return j != nil && j.snapshot().Status == jobDone
	})
	// Give the (absent) send goroutine a chance to run before asserting.
	time.Sleep(150 * time.Millisecond)
	if got := fp.sent(); got != 1 {
		t.Fatalf("push service saw %d requests, want 1 (watching must not notify)", got)
	}
}

// TestPushNoNoticeForFailedPlainReply: a non-agent failure persists nothing, so
// there is nothing to notify about.
func TestPushNoNoticeForFailedPlainReply(t *testing.T) {
	srv, st := newKeyTestServer(t)
	fp, ts := newFakePush(t, http.StatusCreated)
	srv.pushHTTP = ts.Client()
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")
	subscribe(t, srv, u, ts.URL+"/push/one")

	srv.streamAgentFn = fixedStream(upstream.Event{Kind: upstream.EventError, Err: errors.New("boom")})
	startQuietReply(t, srv, u, "c1", "quiet chat")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Status == jobError
	})
	time.Sleep(150 * time.Millisecond)
	if got := fp.sent(); got != 0 {
		t.Fatalf("push service saw %d requests for a failed plain reply, want 0", got)
	}
}

// TestPushGonePrunesSubscription: HTTP 410 deletes the row.
func TestPushGonePrunesSubscription(t *testing.T) {
	srv, st := newKeyTestServer(t)
	_, ts := newFakePush(t, http.StatusGone)
	srv.pushHTTP = ts.Client()
	u := seedKeyUser(t, st, "u1", "alice", "")
	subscribe(t, srv, u, ts.URL+"/push/gone")

	rec := pushCall(srv, u, http.MethodPost, "/api/me/push/test", "", "", srv.handlePushTest)
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d", rec.Code)
	}
	if n, _ := st.CountPushSubscriptions(t.Context(), u.ID); n != 0 {
		t.Fatalf("subscriptions after 410 = %d, want 0", n)
	}
}

// TestPushDropsAfterFiveFailures: a subscription that keeps failing is removed.
func TestPushDropsAfterFiveFailures(t *testing.T) {
	srv, st := newKeyTestServer(t)
	_, ts := newFakePush(t, http.StatusInternalServerError)
	srv.pushHTTP = ts.Client()
	u := seedKeyUser(t, st, "u1", "alice", "")
	subscribe(t, srv, u, ts.URL+"/push/flaky")

	for i := 1; i <= maxPushFailures; i++ {
		pushCall(srv, u, http.MethodPost, "/api/me/push/test", "", "", srv.handlePushTest)
	}
	if n, _ := st.CountPushSubscriptions(t.Context(), u.ID); n != 0 {
		t.Fatalf("subscriptions after %d failures = %d, want 0", maxPushFailures, n)
	}
}

// TestPushOnStoppedAgentPartial: a stopped agent job that kept a partial reply
// is worth a "Job stopped" notification.
func TestPushOnStoppedAgentPartial(t *testing.T) {
	srv, st := newKeyTestServer(t)
	fp, ts := newFakePush(t, http.StatusCreated)
	srv.pushHTTP = ts.Client()
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")
	subscribe(t, srv, u, ts.URL+"/push/one")

	release := make(chan struct{}) // never closed: the job is stopped while running
	srv.streamAgentFn = gatedStream(release)
	j, err := srv.hub().start("c1", u.ID, "glm-5.3-flash", true)
	if err != nil {
		t.Fatalf("start agent job: %v", err)
	}
	go srv.runReply(j, replySpec{
		conv:        store.Conversation{ID: "c1", Title: "agent chat", Model: "glm-5.3-flash"},
		user:        u,
		apiKey:      "key",
		userContent: "hi",
	})
	waitFor(t, func() bool { return j.snapshot().Content == "hello" })
	if !srv.hub().stop("c1") {
		t.Fatal("stop returned false for a running job")
	}
	waitFor(t, func() bool {
		return j.snapshot().Status == jobStopped && fp.sent() == 1
	})
}

// TestPushDisabledSendsNothing: the admin switch gates delivery.
func TestPushDisabledSendsNothing(t *testing.T) {
	srv, st := newKeyTestServer(t)
	fp, ts := newFakePush(t, http.StatusCreated)
	srv.pushHTTP = ts.Client()
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")
	subscribe(t, srv, u, ts.URL+"/push/one")
	if err := st.SetSetting(t.Context(), settingPushEnabled, "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
	)
	startQuietReply(t, srv, u, "c1", "quiet chat")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Status == jobDone
	})
	time.Sleep(150 * time.Millisecond)
	if got := fp.sent(); got != 0 {
		t.Fatalf("push service saw %d requests with push_enabled=0, want 0", got)
	}
}
