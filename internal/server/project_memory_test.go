package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// mkProject inserts a project owned by u.
func mkProject(t *testing.T, st *store.Store, u store.User, id, instructions, memory string, auto bool) {
	t.Helper()
	if err := st.InsertProject(context.Background(), &store.Project{
		ID: id, UserID: &u.ID, Name: id, Instructions: instructions,
		Memory: memory, MemoryAuto: auto, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertProject(%s): %v", id, err)
	}
}

// mkProjectConv inserts a titled (so no auto-title call) chat in projectID.
func mkProjectConv(t *testing.T, st *store.Store, u store.User, id, projectID string) {
	t.Helper()
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: id, UserID: &u.ID, Title: "t", Model: "glm-5.3-flash",
		ProjectID: &projectID, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", id, err)
	}
}

// projectHandler drives one project handler with an authed request.
func projectHandler(srv *Server, u store.User, id, body string, h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(body))
	}
	if id != "" {
		r.SetPathValue("id", id)
	}
	r = r.WithContext(auth.WithUser(r.Context(), u))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// TestProjectMemoryAPIRoundTrip covers the 6.2 contract: create defaults
// memory_auto on, PATCH/GET round-trip memory and the flag, the 32000-rune cap
// returns 400, and another user's project is 404.
func TestProjectMemoryAPIRoundTrip(t *testing.T) {
	srv, st := newJobTestServer(t)
	owner := seedKeyUser(t, st, "u-owner", "owner", "")
	other := seedKeyUser(t, st, "u-other", "other", "")

	rec := projectHandler(srv, owner, "", `{"name":"Rust"}`, srv.handleCreateProject)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created store.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.Memory != "" || !created.MemoryAuto {
		t.Fatalf("created project = %+v, want empty memory and auto on", created)
	}

	rec = projectHandler(srv, owner, created.ID, `{"memory":"likes Go","memory_auto":false}`, srv.handlePatchProject)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var patched store.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	if patched.Memory != "likes Go" || patched.MemoryAuto {
		t.Fatalf("patched project = %+v", patched)
	}

	rec = projectHandler(srv, owner, created.ID, "", srv.handleGetProject)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var got store.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if got.Memory != "likes Go" || got.MemoryAuto {
		t.Fatalf("got project = %+v, want the patched memory", got)
	}

	rec = projectHandler(srv, owner, created.ID, `{"memory":"`+strings.Repeat("é", maxProjectMemoryRunes+1)+`"}`, srv.handlePatchProject)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-cap status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "32000") {
		t.Errorf("over-cap message = %s, want the rune cap named", rec.Body.String())
	}

	// The cap counts runes, not bytes: 32000 two-byte runes are allowed.
	rec = projectHandler(srv, owner, created.ID, `{"memory":"`+strings.Repeat("é", maxProjectMemoryRunes)+`"}`, srv.handlePatchProject)
	if rec.Code != http.StatusOK {
		t.Fatalf("at-cap status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	if rec := projectHandler(srv, other, created.ID, "", srv.handleGetProject); rec.Code != http.StatusNotFound {
		t.Errorf("other user get = %d, want 404", rec.Code)
	}
	if rec := projectHandler(srv, other, created.ID, `{"memory":"mine"}`, srv.handlePatchProject); rec.Code != http.StatusNotFound {
		t.Errorf("other user patch = %d, want 404", rec.Code)
	}
}

// TestProjectInstructionsAddsMemoryBlock covers the single composition point:
// memory adds its own headed block after a blank line; empty memory adds none.
func TestProjectInstructionsAddsMemoryBlock(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	mkProject(t, st, u, "p1", "Be terse.", "- uses mise", true)
	mkProject(t, st, u, "p2", "Be terse.", "", true)

	if got, want := srv.projectInstructions(context.Background(), ptr("p1"), u.ID),
		"Be terse.\n\n## Project memory\n- uses mise"; got != want {
		t.Errorf("with memory = %q, want %q", got, want)
	}
	if got, want := srv.projectInstructions(context.Background(), ptr("p2"), u.ID), "Be terse."; got != want {
		t.Errorf("without memory = %q, want %q", got, want)
	}
}

// TestProjectMemoryUpdatesAfterReply covers the rolling update: one call on the
// cheap model after a successful project reply, and the merged text persisted.
func TestProjectMemoryUpdatesAfterReply(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkProject(t, st, u, "p1", "Be terse.", "", true)
	mkProjectConv(t, st, u, "c1", "p1")
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 3, CompletionTokens: 2}},
	)
	var calls int32
	var gotModel, gotSession, gotKey string
	var gotMax int
	var gotPrompt string
	srv.chatOnceFn = func(_ context.Context, apiKey, sessionID, model string, msgs []upstream.Message, maxTokens int) (string, error) {
		atomic.AddInt32(&calls, 1)
		gotKey, gotSession, gotModel, gotMax, gotPrompt = apiKey, sessionID, model, maxTokens, msgs[0].Content
		return "uses mise\nlikes Go", nil
	}

	if rec := postMessage(srv, u, "c1", "remember I use mise"); rec.Code != http.StatusOK {
		t.Fatalf("send status = %d (body %s)", rec.Code, rec.Body.String())
	}
	waitFor(t, func() bool {
		p, err := st.GetProject(context.Background(), "p1", u.ID)
		return err == nil && p.Memory == "uses mise\nlikes Go"
	})
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("memory calls = %d, want exactly 1", n)
	}
	if gotModel != memoryModel || gotMax != memoryMaxTokens {
		t.Errorf("model/max_tokens = %q/%d, want %q/%d", gotModel, gotMax, memoryModel, memoryMaxTokens)
	}
	if gotKey != "key" || gotSession != "c1" {
		t.Errorf("key/session = %q/%q, want the caller's key and conversation", gotKey, gotSession)
	}
	for _, want := range []string{"remember I use mise", "hello", "(empty)"} {
		if !strings.Contains(gotPrompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, gotPrompt)
		}
	}
}

// TestProjectMemorySkipped covers the three no-update cases: unfiled chat, auto
// off, and a failed reply.
func TestProjectMemorySkipped(t *testing.T) {
	t.Run("unfiled", func(t *testing.T) {
		srv, st := newJobTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		mkConv(t, st, u, "c1")
		srv.streamAgentFn = fixedStream(
			upstream.Event{Kind: upstream.EventText, Delta: "hello"},
			upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
		)
		var calls int32
		srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "x", nil
		}
		postMessage(srv, u, "c1", "hi")
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Fatalf("unfiled chat made %d memory calls", n)
		}
	})

	t.Run("auto off", func(t *testing.T) {
		srv, st := newJobTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		mkProject(t, st, u, "p1", "", "", false)
		mkProjectConv(t, st, u, "c1", "p1")
		var calls int32
		srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "x", nil
		}
		srv.updateProjectMemory(context.Background(), u.ID, "c1", "p1", "key", "hi", "hello")
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Fatalf("auto-off project made %d memory calls", n)
		}
	})

	t.Run("failed reply", func(t *testing.T) {
		srv, st := newJobTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		mkProject(t, st, u, "p1", "", "", true)
		mkProjectConv(t, st, u, "c1", "p1")
		srv.streamAgentFn = fixedStream(upstream.Event{Kind: upstream.EventError})
		var calls int32
		srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
			atomic.AddInt32(&calls, 1)
			return "x", nil
		}
		postMessage(srv, u, "c1", "hi")
		if j := srv.hub().get("c1"); j == nil || j.snapshot().Status != jobError {
			t.Fatalf("job = %+v, want a failed reply", j)
		}
		time.Sleep(50 * time.Millisecond)
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Fatalf("failed reply made %d memory calls", n)
		}
	})
}

// TestProjectMemoryCASKeepsUserEdit proves a background merge never overwrites
// an edit that landed while the call was running.
func TestProjectMemoryCASKeepsUserEdit(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkProject(t, st, u, "p1", "", "old", true)
	mkProjectConv(t, st, u, "c1", "p1")
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
	)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		atomic.AddInt32(&calls, 1)
		close(started)
		<-release
		return "model merged text", nil
	}

	done := postMessageAsync(srv, u, "c1", "hi")
	<-started
	// The user edits memory while the merge call is in flight.
	p, err := st.GetProject(context.Background(), "p1", u.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	p.Memory = "user text"
	if err := st.UpdateProject(context.Background(), &p); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	close(release)
	<-done
	// Give the detached write a moment, then prove it took the CAS miss path.
	time.Sleep(100 * time.Millisecond)
	got, err := st.GetProject(context.Background(), "p1", u.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Memory != "user text" {
		t.Fatalf("memory = %q, want the user edit", got.Memory)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("memory calls = %d, want no retry", n)
	}
}

// TestProjectMemoryDoesNotDelayDone blocks the memory call and asserts the
// reply's `done` still arrives: the update is off the critical path.
func TestProjectMemoryDoesNotDelayDone(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkProject(t, st, u, "p1", "", "", true)
	mkProjectConv(t, st, u, "c1", "p1")
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
	)
	started := make(chan struct{})
	release := make(chan struct{})
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		close(started)
		<-release
		return "uses mise", nil
	}

	if rec := postMessage(srv, u, "c1", "hi"); rec.Code != http.StatusOK {
		t.Fatalf("send status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if j := srv.hub().get("c1"); j == nil || j.snapshot().Status != jobDone {
		t.Fatalf("job = %+v, want done while the memory call is still blocked", j)
	}
	close(release)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("memory call never started")
	}
	waitFor(t, func() bool {
		p, err := st.GetProject(context.Background(), "p1", u.ID)
		return err == nil && p.Memory == "uses mise"
	})
}
