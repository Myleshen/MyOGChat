package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"myogchat/internal/auth"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// memoryRequest drives one /api/me/memory handler. An empty user.ID leaves the
// request unauthenticated.
func memoryRequest(u store.User, body string, h func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodGet, "/api/me/memory", nil)
	} else {
		r = httptest.NewRequest(http.MethodPut, "/api/me/memory", strings.NewReader(body))
	}
	if u.ID != "" {
		r = r.WithContext(auth.WithUser(r.Context(), u))
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// TestUserMemoryAPIRoundTrip covers the 6.8 contract: empty by default, PUT/GET
// round-trip, clear, the 8000-rune cap (runes, not bytes), 401 unauthenticated
// and per-user isolation.
func TestUserMemoryAPIRoundTrip(t *testing.T) {
	srv, st := newJobTestServer(t)
	alice := seedKeyUser(t, st, "u-a", "alice", "")
	bob := seedKeyUser(t, st, "u-b", "bob", "")

	type payload struct {
		Memory    string `json:"memory"`
		Limit     int    `json:"limit"`
		UpdatedAt *int64 `json:"updated_at"`
	}
	decode := func(rec *httptest.ResponseRecorder) payload {
		t.Helper()
		var p payload
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return p
	}

	rec := memoryRequest(alice, "", srv.handleGetMyMemory)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial get status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if p := decode(rec); p.Memory != "" || p.Limit != maxUserMemoryRunes || p.UpdatedAt != nil {
		t.Fatalf("initial get = %+v, want empty memory, limit %d, null updated_at", p, maxUserMemoryRunes)
	}

	rec = memoryRequest(alice, `{"memory":"likes Go"}`, srv.handlePutMyMemory)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d (body %s)", rec.Code, rec.Body.String())
	}
	put := decode(rec)
	if put.Memory != "likes Go" || put.UpdatedAt == nil {
		t.Fatalf("put = %+v, want the memory and an updated_at", put)
	}

	// The session user is loaded from the DB per request, so re-read it to
	// prove the write actually landed.
	alice, err := st.GetUserByID(context.Background(), alice.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	rec = memoryRequest(alice, "", srv.handleGetMyMemory)
	if got := decode(rec); got.Memory != "likes Go" || got.UpdatedAt == nil {
		t.Fatalf("get after put = %+v, want the stored memory", got)
	}

	// The cap counts runes, not bytes: 8000 two-byte runes are allowed.
	rec = memoryRequest(alice, `{"memory":"`+strings.Repeat("é", maxUserMemoryRunes)+`"}`, srv.handlePutMyMemory)
	if rec.Code != http.StatusOK {
		t.Fatalf("at-cap status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = memoryRequest(alice, `{"memory":"`+strings.Repeat("é", maxUserMemoryRunes+1)+`"}`, srv.handlePutMyMemory)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-cap status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "memory exceeds 8000 characters") {
		t.Errorf("over-cap message = %s, want the documented wording", rec.Body.String())
	}

	// Clearing empties the text and its timestamp.
	rec = memoryRequest(alice, `{"memory":""}`, srv.handlePutMyMemory)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if p := decode(rec); p.Memory != "" || p.UpdatedAt != nil {
		t.Fatalf("clear = %+v, want empty memory and null updated_at", p)
	}
	if reloaded, err := st.GetUserByID(context.Background(), alice.ID); err != nil {
		t.Fatalf("GetUserByID after clear: %v", err)
	} else if reloaded.Memory != "" || reloaded.MemoryUpdatedAt != nil {
		t.Fatalf("stored memory after clear = %q/%v, want empty", reloaded.Memory, reloaded.MemoryUpdatedAt)
	}

	// Isolation: bob never sees alice's memory.
	memoryRequest(alice, `{"memory":"alice only"}`, srv.handlePutMyMemory)
	bob, err = st.GetUserByID(context.Background(), bob.ID)
	if err != nil {
		t.Fatalf("GetUserByID(bob): %v", err)
	}
	if p := decode(memoryRequest(bob, "", srv.handleGetMyMemory)); p.Memory != "" {
		t.Fatalf("bob sees %q, want his own empty memory", p.Memory)
	}

	// Unauthenticated calls are refused.
	if rec := memoryRequest(store.User{}, "", srv.handleGetMyMemory); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth get = %d, want 401", rec.Code)
	}
	if rec := memoryRequest(store.User{}, `{"memory":"x"}`, srv.handlePutMyMemory); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth put = %d, want 401", rec.Code)
	}
}

// captureInstructions returns a streamer that records the composed system
// prompt and completes with a short reply.
func captureInstructions(got chan<- string) func(context.Context, upstream.Params, upstream.ExecuteTool, upstream.AgentOptions) <-chan upstream.Event {
	return func(_ context.Context, p upstream.Params, _ upstream.ExecuteTool, _ upstream.AgentOptions) <-chan upstream.Event {
		got <- p.Instructions
		out := make(chan upstream.Event, 2)
		out <- upstream.Event{Kind: upstream.EventText, Delta: "ok"}
		out <- upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}}
		close(out)
		return out
	}
}

// TestUserMemoryInjectionOrder pins 6.8's order: global memory, then project
// instructions, then project memory; absent when empty; still present on a
// calendar-triggered reply.
func TestUserMemoryInjectionOrder(t *testing.T) {
	t.Run("order", func(t *testing.T) {
		srv, st := newJobTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		u.Memory = "GLOBAL-MARK"
		mkProject(t, st, u, "p1", "PROJECT-INSTRUCTIONS", "PROJECT-MEMORY", true)
		mkProjectConv(t, st, u, "c1", "p1")
		got := make(chan string, 1)
		srv.streamAgentFn = captureInstructions(got)

		if rec := postMessage(srv, u, "c1", "hi"); rec.Code != http.StatusOK {
			t.Fatalf("send status = %d (body %s)", rec.Code, rec.Body.String())
		}
		instructions := <-got
		want := "## About the user\nGLOBAL-MARK\n\nPROJECT-INSTRUCTIONS\n\n## Project memory\nPROJECT-MEMORY"
		if instructions != want {
			t.Fatalf("instructions = %q, want %q", instructions, want)
		}
	})

	t.Run("absent when empty", func(t *testing.T) {
		srv, st := newJobTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		mkConv(t, st, u, "c1")
		got := make(chan string, 1)
		srv.streamAgentFn = captureInstructions(got)

		if rec := postMessage(srv, u, "c1", "hi"); rec.Code != http.StatusOK {
			t.Fatalf("send status = %d (body %s)", rec.Code, rec.Body.String())
		}
		if instructions := <-got; instructions != "" {
			t.Fatalf("instructions = %q, want none", instructions)
		}
	})

	t.Run("triggered reply keeps it", func(t *testing.T) {
		srv, st := newJobTestServer(t)
		u := seedKeyUser(t, st, "u1", "alice", "key")
		u.Memory = "GLOBAL-MARK"
		mkConv(t, st, u, "c-trig")
		conv, err := st.GetConversation(context.Background(), "c-trig", u.ID)
		if err != nil {
			t.Fatalf("GetConversation: %v", err)
		}
		got := make(chan string, 1)
		srv.streamAgentFn = captureInstructions(got)
		j, err := srv.hub().startAgentExclusive(conv.ID, u.ID, conv.Model)
		if err != nil {
			t.Fatalf("startAgentExclusive: %v", err)
		}
		srv.runReply(j, replySpec{conv: conv, user: u, apiKey: "key", userSaved: true, triggered: true, userContent: "prepare"})
		if j.snapshot().Status != jobDone {
			t.Fatalf("triggered reply status = %q, want done", j.snapshot().Status)
		}
		if instructions := <-got; !strings.Contains(instructions, "## About the user\nGLOBAL-MARK") {
			t.Fatalf("triggered instructions = %q, want the global memory block", instructions)
		}
	})
}

// TestAuthMeMemorySet covers the hint in /api/auth/me: a boolean, never the
// text.
func TestAuthMeMemorySet(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")

	call := func(u store.User) string {
		r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		r = r.WithContext(auth.WithUser(r.Context(), u))
		rec := httptest.NewRecorder()
		srv.handleMe(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("me status = %d (body %s)", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	body := call(u)
	if !strings.Contains(body, `"memory_set":false`) {
		t.Errorf("me with no memory = %s, want memory_set false", body)
	}
	if strings.Contains(body, `"memory":`) {
		t.Errorf("me leaks a memory field: %s", body)
	}

	u.Memory = "private note"
	body = call(u)
	if !strings.Contains(body, `"memory_set":true`) {
		t.Errorf("me with memory = %s, want memory_set true", body)
	}
	if strings.Contains(body, "private note") {
		t.Errorf("me leaks the memory text: %s", body)
	}
}
