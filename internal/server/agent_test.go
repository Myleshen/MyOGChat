package server

// Agent mode: per-conversation flag, one-per-user FIFO queue, snapshot progress,
// partial persistence on stop, the disk pre-flight and workspace management.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/sandbox"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// mkAgentConv inserts an agent-mode conversation owned by u.
func mkAgentConv(t *testing.T, st *store.Store, u store.User, id string) {
	t.Helper()
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: id, UserID: &u.ID, Title: "agent " + id, Model: "glm-5.3-flash",
		AgentMode: true, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", id, err)
	}
}

// blockingStream emits text (when non-empty), then waits for release or cancel
// before reporting usage — long enough to observe/stop a running job.
func blockingStream(text string, release <-chan struct{}) func(context.Context, upstream.Params, upstream.ExecuteTool, upstream.AgentOptions) <-chan upstream.Event {
	return func(c context.Context, _ upstream.Params, _ upstream.ExecuteTool, _ upstream.AgentOptions) <-chan upstream.Event {
		out := make(chan upstream.Event, 2)
		go func() {
			defer close(out)
			if text != "" {
				select {
				case out <- upstream.Event{Kind: upstream.EventText, Delta: text}:
				case <-c.Done():
					return
				}
			}
			select {
			case <-release:
			case <-c.Done():
				return
			}
			out <- upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}}
		}()
		return out
	}
}

// postMessageAsync drives handlePostMessage in the background (the SSE relay
// blocks until the job terminates).
func postMessageAsync(srv *Server, u store.User, convID, content string) chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postMessage(srv, u, convID, content) }()
	return done
}

// stopGeneration drives handleStopGeneration for convID.
func stopGeneration(srv *Server, u store.User, convID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+convID+"/generation/stop", nil)
	req.SetPathValue("id", convID)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleStopGeneration(rec, req)
	return rec
}

// TestAgentSendQueuesBehindRunningJob covers the FIFO queue: a second agent send
// while one is running answers 202 with its position, persists the message, and
// starts automatically when the first job finishes.
func TestAgentSendQueuesBehindRunningJob(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkAgentConv(t, st, u, "c1")
	mkAgentConv(t, st, u, "c2")

	release := make(chan struct{})
	srv.streamAgentFn = blockingStream("hello", release)

	first := postMessageAsync(srv, u, "c1", "first")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning() && j.snapshot().Content == "hello"
	})

	rec := postMessage(srv, u, "c2", "second")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queued send status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Queued   bool `json:"queued"`
		Position int  `json:"position"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode queued response: %v", err)
	}
	if !body.Queued || body.Position != 1 {
		t.Fatalf("queued response = %+v, want queued position 1", body)
	}

	// The queued message is visible immediately, with no assistant row yet.
	msgs, err := st.ListMessages(context.Background(), "c2")
	if err != nil {
		t.Fatalf("ListMessages(c2): %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "second" {
		t.Fatalf("c2 messages = %+v, want the queued user message", msgs)
	}

	// The snapshot reports the queue position and agent block.
	snap := getGeneration(t, srv, u, "c2")
	if snap.Status != jobQueued || !snap.Active {
		t.Fatalf("queued snapshot = %+v", snap)
	}
	if snap.Agent == nil || !snap.Agent.Enabled || !snap.Agent.Queued ||
		snap.Agent.QueuePosition != 1 || snap.Agent.MaxSteps != agentMaxSteps {
		t.Fatalf("queued agent block = %+v", snap.Agent)
	}

	// Finishing the first job starts the queued one.
	close(release)
	<-first
	waitFor(t, func() bool {
		j := srv.hub().get("c2")
		return j != nil && j.snapshot().Status == jobDone
	})
	if j := srv.hub().get("c2"); j == nil || !j.agent {
		t.Fatalf("dequeued job = %+v, want an agent job", j)
	}
	msgs, _ = st.ListMessages(context.Background(), "c2")
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Content != "hello" {
		t.Fatalf("c2 messages after dequeue = %+v, want user + assistant", msgs)
	}
}

// TestAgentStopQueuedDequeues proves stopping a waiting send removes it from the
// queue instead of starting it later.
func TestAgentStopQueuedDequeues(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkAgentConv(t, st, u, "c1")
	mkAgentConv(t, st, u, "c2")

	release := make(chan struct{})
	srv.streamAgentFn = blockingStream("hello", release)
	first := postMessageAsync(srv, u, "c1", "first")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning() && j.snapshot().Content == "hello"
	})
	if rec := postMessage(srv, u, "c2", "second"); rec.Code != http.StatusAccepted {
		t.Fatalf("queued send status = %d, want 202", rec.Code)
	}

	if rec := stopGeneration(srv, u, "c2"); rec.Code != http.StatusNoContent {
		t.Fatalf("stop queued status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if j := srv.hub().get("c2"); j != nil {
		t.Fatalf("c2 job after stop = %+v, want none", j)
	}
	if snap := getGeneration(t, srv, u, "c2"); snap.Status != "none" || snap.Active {
		t.Fatalf("c2 snapshot after stop = %+v", snap)
	}

	close(release)
	<-first
	// Give the dequeue path a moment: c2 must never start.
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Status == jobDone
	})
	if j := srv.hub().get("c2"); j != nil {
		t.Fatalf("stopped queued job started anyway: %+v", j)
	}
	msgs, _ := st.ListMessages(context.Background(), "c2")
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("c2 messages = %+v, want only the user message", msgs)
	}
}

// TestAgentStopPersistsPartial proves an interrupted agent reply keeps what it
// produced, while an empty one is never stored.
func TestAgentStopPersistsPartial(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkAgentConv(t, st, u, "c1")

	release := make(chan struct{}) // never closed: only the stop ends the job
	srv.streamAgentFn = blockingStream("partial answer", release)
	postMessageAsync(srv, u, "c1", "hi")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning() && j.snapshot().Content == "partial answer"
	})
	if rec := stopGeneration(srv, u, "c1"); rec.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d, want 204", rec.Code)
	}
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.snapshot().Status == jobStopped
	})
	waitFor(t, func() bool {
		msgs, _ := st.ListMessages(context.Background(), "c1")
		return len(msgs) == 2
	})
	msgs, _ := st.ListMessages(context.Background(), "c1")
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Content != "partial answer" {
		t.Fatalf("messages after stop = %+v, want user + partial assistant", msgs)
	}
	if msgs[1].PromptTokens != nil || msgs[1].CostMicros != nil {
		t.Errorf("partial reply must not claim usage: %+v", msgs[1])
	}

	// An agent reply stopped before any text persists nothing.
	srv.streamAgentFn = blockingStream("", release)
	mkAgentConv(t, st, u, "c2")
	postMessageAsync(srv, u, "c2", "hi")
	waitFor(t, func() bool {
		j := srv.hub().get("c2")
		return j != nil && j.isRunning()
	})
	if rec := stopGeneration(srv, u, "c2"); rec.Code != http.StatusNoContent {
		t.Fatalf("stop status = %d, want 204", rec.Code)
	}
	waitFor(t, func() bool {
		j := srv.hub().get("c2")
		return j != nil && j.snapshot().Status == jobStopped
	})
	msgs, _ = st.ListMessages(context.Background(), "c2")
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("empty stop messages = %+v, want only the user message", msgs)
	}
}

// TestAgentSnapshotProgress covers the agent job's limits and the step callback
// feeding the generation snapshot.
func TestAgentSnapshotProgress(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkAgentConv(t, st, u, "c1")

	optsCh := make(chan upstream.AgentOptions, 1)
	release := make(chan struct{})
	srv.streamAgentFn = func(c context.Context, p upstream.Params, exec upstream.ExecuteTool, opts upstream.AgentOptions) <-chan upstream.Event {
		optsCh <- opts
		// The real loop reports each round before calling upstream.
		if opts.OnStep != nil {
			opts.OnStep(1, opts.MaxIterations)
		}
		return blockingStream("hello", release)(c, p, exec, opts)
	}
	done := postMessageAsync(srv, u, "c1", "hi")

	var opts upstream.AgentOptions
	select {
	case opts = <-optsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("agent stream was not called")
	}
	if opts.MaxIterations != agentMaxSteps || opts.CallTimeout != agentCallTimeout || opts.OnStep == nil {
		t.Fatalf("agent options = %+v, want %d steps, %s per call, OnStep set",
			opts, agentMaxSteps, agentCallTimeout)
	}

	snap := getGeneration(t, srv, u, "c1")
	if snap.Agent == nil || !snap.Agent.Enabled || snap.Agent.Queued || snap.Agent.QueuePosition != 0 {
		t.Fatalf("running agent block = %+v", snap.Agent)
	}
	if snap.Agent.Step != 1 || snap.Agent.MaxSteps != agentMaxSteps {
		t.Fatalf("initial step = %d/%d, want 1/%d", snap.Agent.Step, snap.Agent.MaxSteps, agentMaxSteps)
	}
	opts.OnStep(3, opts.MaxIterations)
	waitFor(t, func() bool {
		s := getGeneration(t, srv, u, "c1")
		return s.Agent != nil && s.Agent.Step == 3
	})
	close(release)
	<-done

	// A normal chat reports no agent block and keeps the historical limits.
	optsCh2 := make(chan upstream.AgentOptions, 1)
	mkConv(t, st, u, "c2")
	srv.streamAgentFn = func(c context.Context, p upstream.Params, exec upstream.ExecuteTool, opts upstream.AgentOptions) <-chan upstream.Event {
		optsCh2 <- opts
		return blockingStream("plain", release)(c, p, exec, opts)
	}
	done2 := postMessageAsync(srv, u, "c2", "hi")
	normal := <-optsCh2
	if normal.MaxIterations != 0 || normal.CallTimeout != 0 || normal.OnStep != nil {
		t.Errorf("normal chat options = %+v, want the defaults", normal)
	}
	waitFor(t, func() bool {
		j := srv.hub().get("c2")
		return j != nil && j.snapshot().Content == "plain"
	})
	if snap := getGeneration(t, srv, u, "c2"); snap.Agent != nil {
		t.Errorf("normal chat snapshot has an agent block: %+v", snap.Agent)
	}
	<-done2
}

// TestAgentDiskGuard covers the 507 pre-flight and its fail-open behaviour.
func TestAgentDiskGuard(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkAgentConv(t, st, u, "c1")
	srv.sbx = sandbox.NewRunner(sandbox.Config{WorkDir: t.TempDir()})
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "ok"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}},
	)

	srv.freeBytes = func(string) (uint64, error) { return 1 << 20, nil }
	rec := postMessage(srv, u, "c1", "hi")
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("low-disk send status = %d, want 507 (body %s)", rec.Code, rec.Body.String())
	}
	if msgs, _ := st.ListMessages(context.Background(), "c1"); len(msgs) != 0 {
		t.Fatalf("low-disk send persisted %d message(s), want none", len(msgs))
	}

	// Fail open: an unreadable path never blocks the send.
	srv.freeBytes = func(string) (uint64, error) { return 0, errors.New("statfs failed") }
	if rec := postMessage(srv, u, "c1", "hi"); rec.Code != http.StatusOK {
		t.Fatalf("fail-open send status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAgentKillSwitch proves the admin switch degrades an agent conversation to
// a normal chat (no queue, no agent block) and is exposed in the settings API.
func TestAgentKillSwitch(t *testing.T) {
	ctx := context.Background()
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-admin", "admin", true)
	h := srv.Handler()
	cookie := login(t, h, "admin")

	rr := doJSON(t, h, cookie, http.MethodPatch, "/api/settings", map[string]any{"agent_enabled": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH settings status = %d (body %s)", rr.Code, rr.Body.String())
	}
	var settings map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &settings)
	if v, ok := settings["agent_enabled"].(bool); !ok || v {
		t.Fatalf("settings agent_enabled = %v, want false", settings["agent_enabled"])
	}
	rr = doJSON(t, h, cookie, http.MethodGet, "/api/settings", nil)
	settings = nil
	_ = json.Unmarshal(rr.Body.Bytes(), &settings)
	if v, ok := settings["agent_enabled"].(bool); !ok || v {
		t.Fatalf("GET settings agent_enabled = %v, want false", settings["agent_enabled"])
	}

	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkAgentConv(t, st, u, "c1")
	mkAgentConv(t, st, u, "c2")
	release := make(chan struct{})
	srv.streamAgentFn = blockingStream("hello", release)

	first := postMessageAsync(srv, u, "c1", "first")
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning() && j.snapshot().Content == "hello"
	})
	if j := srv.hub().get("c1"); j.agent {
		t.Error("agent mode is disabled but the job was started as an agent job")
	}
	// A second send runs normally instead of queueing.
	second := postMessageAsync(srv, u, "c2", "second")
	waitFor(t, func() bool {
		j := srv.hub().get("c2")
		return j != nil && j.isRunning() && j.snapshot().Content == "hello"
	})
	if pos := srv.hub().queuedPosition("c2"); pos != 0 {
		t.Errorf("queued position = %d, want 0", pos)
	}
	if v, ok, _ := st.GetSetting(ctx, settingAgentEnabled); !ok || v != "0" {
		t.Errorf("agent_enabled setting = %q, ok %v", v, ok)
	}
	close(release)
	<-first
	if rec := <-second; rec.Code != http.StatusOK {
		t.Fatalf("second send with agents disabled = %d, want 200", rec.Code)
	}
}

// TestAgentToolsSwitchDisablesAgentMode proves tools off degrades an agent
// conversation to a normal chat: normal budget, no queue, no 507 disk guard.
func TestAgentToolsSwitchDisablesAgentMode(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	mkAgentConv(t, st, u, "c1")
	mkAgentConv(t, st, u, "c2")
	if err := srv.setToolsEnabled(context.Background(), false); err != nil {
		t.Fatalf("setToolsEnabled: %v", err)
	}

	// The disk pre-flight only applies to agent jobs.
	srv.sbx = sandbox.NewRunner(sandbox.Config{WorkDir: t.TempDir()})
	srv.freeBytes = func(string) (uint64, error) { return 1 << 20, nil }

	optsCh := make(chan upstream.AgentOptions, 1)
	release := make(chan struct{})
	srv.streamAgentFn = func(c context.Context, p upstream.Params, exec upstream.ExecuteTool, opts upstream.AgentOptions) <-chan upstream.Event {
		optsCh <- opts
		return blockingStream("hello", release)(c, p, exec, opts)
	}

	first := postMessageAsync(srv, u, "c1", "first")
	select {
	case opts := <-optsCh:
		if opts.MaxIterations != 0 || opts.CallTimeout != 0 || opts.OnStep != nil {
			t.Errorf("tools-off options = %+v, want the normal-chat defaults", opts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send with tools off did not start")
	}
	waitFor(t, func() bool {
		j := srv.hub().get("c1")
		return j != nil && j.isRunning() && !j.agent
	})

	// A second send runs normally instead of queueing.
	second := postMessageAsync(srv, u, "c2", "second")
	waitFor(t, func() bool {
		j := srv.hub().get("c2")
		return j != nil && j.isRunning()
	})
	if pos := srv.hub().queuedPosition("c2"); pos != 0 {
		t.Errorf("queued position = %d, want 0", pos)
	}
	close(release)
	<-first
	if rec := <-second; rec.Code != http.StatusOK {
		t.Fatalf("second send with tools disabled = %d, want 200", rec.Code)
	}
}

// TestMeExposesAgentEnabled proves every user (not just admins) can read the
// server-wide switches from their session endpoint.
func TestMeExposesAgentEnabled(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-admin", "admin", true)
	seedActiveUser(t, st, "u-bob", "bob", false)
	h := srv.Handler()
	adminCookie := login(t, h, "admin")
	bobCookie := login(t, h, "bob")

	meFlags := func(cookie *http.Cookie) (agent, tools bool) {
		t.Helper()
		rr := doJSON(t, h, cookie, http.MethodGet, "/api/auth/me", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /api/auth/me status = %d (body %s)", rr.Code, rr.Body.String())
		}
		var me map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &me); err != nil {
			t.Fatalf("decode /api/auth/me: %v", err)
		}
		agent, ok := me["agent_enabled"].(bool)
		if !ok {
			t.Fatalf("me agent_enabled = %v, want a boolean", me["agent_enabled"])
		}
		tools, ok = me["tools_enabled"].(bool)
		if !ok {
			t.Fatalf("me tools_enabled = %v, want a boolean", me["tools_enabled"])
		}
		return agent, tools
	}

	if agent, tools := meFlags(bobCookie); !agent || !tools {
		t.Fatalf("me switches = agent %v tools %v by default, want both true", agent, tools)
	}
	rr := doJSON(t, h, adminCookie, http.MethodPatch, "/api/settings", map[string]any{"agent_enabled": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /api/settings status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if agent, _ := meFlags(bobCookie); agent {
		t.Fatal("agent_enabled = true after an admin turned it off, want false")
	}
	rr = doJSON(t, h, adminCookie, http.MethodPatch, "/api/settings", map[string]any{"tools_enabled": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH tools_enabled status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if _, tools := meFlags(bobCookie); tools {
		t.Fatal("tools_enabled = true after an admin turned it off, want false")
	}
}

// TestAgentModePatchAndScoping covers the PATCH/GET round-trip and owner scoping.
func TestAgentModePatchAndScoping(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	seedActiveUser(t, st, "u-bob", "bob", false)
	h := srv.Handler()
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")

	rr := doJSON(t, h, alice, http.MethodPost, "/api/conversations", map[string]any{"model": "glm-5.3-flash", "agent_mode": true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d (body %s)", rr.Code, rr.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(rr.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if !conv.AgentMode {
		t.Error("create with agent_mode did not carry the flag")
	}
	// And it can be switched off again through PATCH.
	if rr := doJSON(t, h, alice, http.MethodPatch, "/api/conversations/"+conv.ID, map[string]any{"agent_mode": false}); rr.Code != http.StatusOK {
		t.Fatalf("PATCH agent_mode off status = %d", rr.Code)
	}
	rr = doJSON(t, h, alice, http.MethodPatch, "/api/conversations/"+conv.ID, map[string]any{"agent_mode": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH agent_mode status = %d (body %s)", rr.Code, rr.Body.String())
	}
	conv = store.Conversation{}
	_ = json.Unmarshal(rr.Body.Bytes(), &conv)
	if !conv.AgentMode {
		t.Error("PATCH response did not carry agent_mode")
	}
	rr = doJSON(t, h, alice, http.MethodGet, "/api/conversations/"+conv.ID, nil)
	var got struct {
		store.Conversation
		Messages []store.Message `json:"messages"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.AgentMode {
		t.Error("GET conversation did not carry agent_mode")
	}

	// Other fields keep working, and the flag survives them.
	rr = doJSON(t, h, alice, http.MethodPatch, "/api/conversations/"+conv.ID, map[string]any{"title": "renamed"})
	_ = json.Unmarshal(rr.Body.Bytes(), &conv)
	if conv.Title != "renamed" || !conv.AgentMode {
		t.Fatalf("after title PATCH = %+v, want renamed + agent_mode", conv)
	}

	// Cross-user access is a 404, as for any conversation.
	if rr := doJSON(t, h, bob, http.MethodPatch, "/api/conversations/"+conv.ID, map[string]any{"agent_mode": false}); rr.Code != http.StatusNotFound {
		t.Errorf("cross-user PATCH = %d, want 404", rr.Code)
	}
	if rr := doJSON(t, h, bob, http.MethodGet, "/api/conversations/"+conv.ID, nil); rr.Code != http.StatusNotFound {
		t.Errorf("cross-user GET = %d, want 404", rr.Code)
	}
}

// TestArtifactDedupe covers agent-run artifact de-duplication: identical bytes
// in one conversation reuse the stored row, the same bytes in another
// conversation are stored separately.
func TestArtifactDedupe(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	ctx := context.Background()
	for _, id := range []string{"c1", "c2"} {
		if err := st.InsertConversation(ctx, &store.Conversation{
			ID: id, UserID: ptr("u-alice"), Title: id, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("InsertConversation(%s): %v", id, err)
		}
	}

	art := sandbox.Artifact{Name: "report.csv", MIME: "text/csv", Data: []byte("a,b\n1\n")}
	first, err := srv.storeArtifact(ctx, "u-alice", "c1", art)
	if err != nil {
		t.Fatalf("storeArtifact(first): %v", err)
	}
	again, err := srv.storeArtifact(ctx, "u-alice", "c1", art)
	if err != nil {
		t.Fatalf("storeArtifact(again): %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("identical upload stored twice: %s vs %s", first.ID, again.ID)
	}
	other, err := srv.storeArtifact(ctx, "u-alice", "c2", art)
	if err != nil {
		t.Fatalf("storeArtifact(other conv): %v", err)
	}
	if other.ID == first.ID {
		t.Error("the same bytes in a different conversation must be a separate artifact")
	}
	if _, err := st.GetArtifact(ctx, first.ID, "u-alice"); err != nil {
		t.Errorf("deduped artifact is not retrievable: %v", err)
	}
}

// TestAgentWorkspaceEndpoints covers listing sizes/scoping and deletion.
func TestAgentWorkspaceEndpoints(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	seedActiveUser(t, st, "u-bob", "bob", false)
	root := t.TempDir()
	srv.sbx = sandbox.NewRunner(sandbox.Config{WorkDir: root})

	alice := store.User{ID: "u-alice"}
	mkAgentConv(t, st, alice, "c1")
	mkAgentConv(t, st, alice, "c2")
	mkConv(t, st, alice, "c3") // not an agent chat

	dir := filepath.Join(root, sandbox.AgentWorkspaceDirName, "u-alice", "c1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("12345"), 0o600); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	// c2 is an agent chat with no directory; c3 has one but is not agent mode.
	if err := os.MkdirAll(filepath.Join(root, sandbox.AgentWorkspaceDirName, "u-alice", "c3"), 0o700); err != nil {
		t.Fatalf("mkdir c3: %v", err)
	}

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doJSON(t, h, cookie, http.MethodGet, "/api/me/agent/workspaces", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d (body %s)", rr.Code, rr.Body.String())
	}
	var list []agentWorkspaceInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode workspaces: %v", err)
	}
	if len(list) != 1 || list[0].ConversationID != "c1" {
		t.Fatalf("workspaces = %+v, want only c1", list)
	}
	if list[0].Bytes != 5 || list[0].UpdatedAt <= 0 || !strings.HasPrefix(list[0].Title, "agent") {
		t.Errorf("workspace entry = %+v", list[0])
	}

	// Another user sees nothing and cannot delete it.
	bob := login(t, h, "bob")
	var empty []agentWorkspaceInfo
	rr = doJSON(t, h, bob, http.MethodGet, "/api/me/agent/workspaces", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &empty)
	if len(empty) != 0 {
		t.Errorf("bob's workspaces = %+v, want none", empty)
	}
	if rr := doJSON(t, h, bob, http.MethodDelete, "/api/conversations/c1/workspace", nil); rr.Code != http.StatusNotFound {
		t.Errorf("cross-user delete = %d, want 404", rr.Code)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cross-user delete removed the directory: %v", err)
	}

	// The owner can remove it, twice (idempotent).
	for i := 0; i < 2; i++ {
		if rr := doJSON(t, h, cookie, http.MethodDelete, "/api/conversations/c1/workspace", nil); rr.Code != http.StatusNoContent {
			t.Fatalf("owner delete #%d = %d, want 204 (body %s)", i+1, rr.Code, rr.Body.String())
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("workspace still exists after delete: %v", err)
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }
