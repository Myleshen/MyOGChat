package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// newJobTestServer returns a Server backed by a temp store whose reply stream
// is injected by each test via srv.streamAgentFn.
func newJobTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return newKeyTestServer(t)
}

// mkConv inserts a conversation owned by u.
func mkConv(t *testing.T, st *store.Store, u store.User, id string) {
	t.Helper()
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: id, UserID: &u.ID, Title: "t", Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", id, err)
	}
}

// waitFor polls cond until it holds or a short deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// fixedStream returns a streamer that emits events then closes.
func fixedStream(events ...upstream.Event) func(context.Context, upstream.Params, upstream.ExecuteTool) <-chan upstream.Event {
	return func(context.Context, upstream.Params, upstream.ExecuteTool) <-chan upstream.Event {
		out := make(chan upstream.Event, len(events))
		for _, ev := range events {
			out <- ev
		}
		close(out)
		return out
	}
}

// gatedStream emits a text delta and then waits for release (or context cancel)
// before completing with usage — long enough to observe a running job.
func gatedStream(release <-chan struct{}) func(context.Context, upstream.Params, upstream.ExecuteTool) <-chan upstream.Event {
	return func(c context.Context, _ upstream.Params, _ upstream.ExecuteTool) <-chan upstream.Event {
		out := make(chan upstream.Event, 2)
		go func() {
			defer close(out)
			select {
			case out <- upstream.Event{Kind: upstream.EventText, Delta: "hello"}:
			case <-c.Done():
				return
			}
			select {
			case <-release:
			case <-c.Done():
				return
			}
			out <- upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{
				PromptTokens: 3, CompletionTokens: 2, CachedTokens: 1,
			}}
		}()
		return out
	}
}

// postMessage drives handlePostMessage synchronously and returns the recorder.
func postMessage(srv *Server, u store.User, convID, content string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+convID+"/messages",
		strings.NewReader(`{"content":"`+content+`"}`))
	req.SetPathValue("id", convID)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handlePostMessage(rec, req)
	return rec
}

// TestJobConcurrentAndRefused starts two conversations' replies at once and
// asserts they run concurrently while a second job for the same conversation is
// refused with 409.
func TestJobConcurrentAndRefused(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")
	mkConv(t, st, u, "c2")

	release := make(chan struct{})
	srv.streamAgentFn = gatedStream(release)

	go postMessage(srv, u, "c1", "hi1")
	go postMessage(srv, u, "c2", "hi2")

	waitFor(t, func() bool {
		j1, j2 := srv.hub().get("c1"), srv.hub().get("c2")
		return j1 != nil && j1.isRunning() && j2 != nil && j2.isRunning()
	})

	// A second job for a conversation that is already generating is refused.
	rec := postMessage(srv, u, "c1", "again")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second job status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	close(release)
	waitFor(t, func() bool {
		j1, j2 := srv.hub().get("c1"), srv.hub().get("c2")
		return j1.snapshot().Status == jobDone && j2.snapshot().Status == jobDone
	})
}

// TestDisconnectStillPersists verifies a client disconnect only unsubscribes;
// the detached job finishes and persists the assistant reply.
func TestDisconnectStillPersists(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")

	release := make(chan struct{})
	srv.streamAgentFn = gatedStream(release)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.SetPathValue("id", "c1")
	req = req.WithContext(auth.WithUser(ctx, u))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { srv.handlePostMessage(rec, req); close(done) }()

	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Content == "hello"
	})

	// Simulate a client disconnect / reload.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not return after disconnect")
	}

	// The job is still running and the user message is the only row so far.
	j := srv.hub().get("c1")
	if j == nil || !j.isRunning() {
		t.Fatalf("job should still be running after disconnect, got %+v", j)
	}
	msgs, err := st.ListMessages(context.Background(), "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("messages after disconnect = %+v, want one user message", msgs)
	}

	close(release)
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Status == jobDone
	})
	msgs, _ = st.ListMessages(context.Background(), "c1")
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Content != "hello" {
		t.Fatalf("persisted messages = %+v, want user + assistant 'hello'", msgs)
	}
}

// TestStopGeneration cancels a running reply and persists no assistant row.
func TestStopGeneration(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")

	release := make(chan struct{}) // never closed: the reply stays running
	srv.streamAgentFn = gatedStream(release)

	done := make(chan struct{})
	go func() { postMessage(srv, u, "c1", "hi"); close(done) }()

	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning() && j.snapshot().Content == "hello"
	})

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/generation/stop", nil)
	req.SetPathValue("id", "c1")
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleStopGeneration(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not return after stop")
	}
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Status == jobStopped
	})
	// A stopped reply persists nothing beyond the user message.
	msgs, _ := st.ListMessages(context.Background(), "c1")
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("messages after stop = %+v, want only the user message", msgs)
	}
}

// TestRegenerateJobReplacesTrailingReply verifies the trailing assistant delete
// moved into the detached job and a fresh reply is persisted.
func TestRegenerateJobReplacesTrailingReply(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")

	ctx := context.Background()
	if err := st.InsertMessage(ctx, &store.Message{
		ID: "u-old", ConversationID: "c1", Role: "user", Content: "old question", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertMessage(user): %v", err)
	}
	if err := st.InsertMessage(ctx, &store.Message{
		ID: "a-old", ConversationID: "c1", Role: "assistant", Content: "old answer", CreatedAt: 2,
	}); err != nil {
		t.Fatalf("InsertMessage(assistant): %v", err)
	}

	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "new answer"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
	)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/regenerate", nil)
	req.SetPathValue("id", "c1")
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleRegenerate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	msgs, err := st.ListMessages(ctx, "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Content != "new answer" {
		t.Fatalf("regenerated messages = %+v, want user + new assistant", msgs)
	}
}

// TestGenerationOwnerScoping verifies the snapshot and stop endpoints 404 for a
// conversation that is not the caller's.
func TestGenerationOwnerScoping(t *testing.T) {
	srv, st := newJobTestServer(t)
	owner := seedKeyUser(t, st, "u-owner", "owner", "key")
	other := seedKeyUser(t, st, "u-other", "other", "key")
	mkConv(t, st, owner, "c1")

	release := make(chan struct{})
	srv.streamAgentFn = gatedStream(release)
	go postMessage(srv, owner, "c1", "hi")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning()
	})

	for _, tc := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"snapshot", srv.handleGetGeneration},
		{"stop", srv.handleStopGeneration},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/conversations/c1/generation", nil)
		req.SetPathValue("id", "c1")
		req = req.WithContext(auth.WithUser(req.Context(), other))
		rec := httptest.NewRecorder()
		tc.fn(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s for another user's chat = %d, want 404", tc.name, rec.Code)
		}
	}
	close(release)
}

// TestGenerationSnapshot verifies the snapshot endpoint content while running
// and after completion.
func TestGenerationSnapshot(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkConv(t, st, u, "c1")

	release := make(chan struct{})
	srv.streamAgentFn = gatedStream(release)

	done := make(chan struct{})
	go func() { postMessage(srv, u, "c1", "hi"); close(done) }()
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning() && j.snapshot().Content == "hello"
	})

	snap := getGeneration(t, srv, u, "c1")
	if !snap.Active || snap.Status != jobRunning {
		t.Fatalf("running snapshot = %+v", snap)
	}
	if snap.Content != "hello" || snap.MessageID == "" || snap.Model != "glm-5.3-flash" {
		t.Fatalf("running snapshot fields = %+v", snap)
	}
	if snap.Usage != nil {
		t.Fatalf("usage should be absent while running, got %+v", snap.Usage)
	}

	close(release)
	<-done
	waitFor(t, func() bool {
		return getGeneration(t, srv, u, "c1").Status == jobDone
	})
	done2 := getGeneration(t, srv, u, "c1")
	if done2.Active || done2.Content != "hello" || done2.Usage == nil || done2.Usage.CompletionTokens != 2 {
		t.Fatalf("finished snapshot = %+v", done2)
	}

	// A conversation with no generation reports a non-active "none" snapshot.
	mkConv(t, st, u, "c2")
	none := getGeneration(t, srv, u, "c2")
	if none.Active || none.Status != "none" {
		t.Fatalf("no-job snapshot = %+v", none)
	}
}

// getGeneration drives handleGetGeneration and decodes the snapshot.
func getGeneration(t *testing.T, srv *Server, u store.User, convID string) generationSnapshot {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/conversations/"+convID+"/generation", nil)
	req.SetPathValue("id", convID)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleGetGeneration(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generation status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var snap generationSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return snap
}
