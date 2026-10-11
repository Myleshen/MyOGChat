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
)

// seedConvWithMessage inserts an owner-scoped conversation plus one user
// message so search has content to match.
func seedConvWithMessage(t *testing.T, st *store.Store, id, userID, title, content string, updatedAt int64) {
	t.Helper()
	uid := userID
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: id, UserID: &uid, Title: title, Model: "glm-5.3-flash",
		CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", id, err)
	}
	if content != "" {
		if err := st.InsertMessage(context.Background(), &store.Message{
			ID: id + "-m", ConversationID: id, Role: "user", Content: content, CreatedAt: updatedAt,
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", id, err)
		}
	}
}

type searchResp struct {
	Results []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Snippet   string `json:"snippet"`
		UpdatedAt int64  `json:"updated_at"`
	} `json:"results"`
}

// TestHandleSearchRequiresAuth verifies the endpoint is session-guarded.
func TestHandleSearchRequiresAuth(t *testing.T) {
	srv, _ := newKeyTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/search?q=hello", nil)
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestHandleSearchEmptyQuery returns an empty (not null) result set.
func TestHandleSearchEmptyQuery(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=", nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"results":[]}` {
		t.Errorf("body = %s, want empty results array", body)
	}
}

// TestHandleSearchOversizeQuery floors an over-long query to empty results.
func TestHandleSearchOversizeQuery(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedConvWithMessage(t, st, "c1", u.ID, "Long", "hello world", 1)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+strings.Repeat("a", searchQueryMax+1), nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Results) != 0 {
		t.Errorf("results = %+v, want none", out.Results)
	}
}

// TestHandleSearchScopingAndShape checks the response shape and that a user
// only ever sees their own conversations.
func TestHandleSearchScopingAndShape(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u-alice", "alice", "")
	bob := seedKeyUser(t, st, "u-bob", "bob", "")

	seedConvWithMessage(t, st, "c-alice", alice.ID, "Rust notes", "the borrow checker", 100)
	seedConvWithMessage(t, st, "c-bob", bob.ID, "Bob notes", "the borrow checker", 500)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=borrow", nil)
	req = req.WithContext(auth.WithUser(req.Context(), alice))
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %+v, want only the caller's conversation", out.Results)
	}
	got := out.Results[0]
	if got.ID != "c-alice" || got.Title != "Rust notes" || got.UpdatedAt != 100 {
		t.Errorf("result = %+v", got)
	}
	if !strings.Contains(got.Snippet, "borrow checker") {
		t.Errorf("snippet = %q, want the match", got.Snippet)
	}
}

// TestHandleSearchTitleMatch checks a title-only match returns an empty
// snippet (there is no matching message).
func TestHandleSearchTitleMatch(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedConvWithMessage(t, st, "c1", u.ID, "Baking Bread", "unrelated body", 1)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=baking", nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)

	var out searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Results) != 1 || out.Results[0].ID != "c1" || out.Results[0].Snippet != "" {
		t.Errorf("results = %+v, want c1 with an empty snippet", out.Results)
	}
}

// TestHandlePatchConversationPinned covers the pinned field on PATCH:
// round-trip, the returned object, and cross-user isolation (404).
func TestHandlePatchConversationPinned(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u-alice", "alice", "")
	bob := seedKeyUser(t, st, "u-bob", "bob", "")

	seedConvWithMessage(t, st, "c-alice", alice.ID, "Alice chat", "", 10)
	seedConvWithMessage(t, st, "c-bob", bob.ID, "Bob chat", "", 10)

	patch := func(user store.User, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/conversations/"+id, strings.NewReader(body))
		req.SetPathValue("id", id)
		req = req.WithContext(auth.WithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		srv.handlePatchConversation(rec, req)
		return rec
	}

	rec := patch(alice, "c-alice", `{"pinned":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pin status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Pinned {
		t.Errorf("response pinned = false, want true")
	}
	if stored, _ := st.GetConversation(context.Background(), "c-alice", alice.ID); !stored.Pinned {
		t.Error("pinned flag did not persist")
	}

	// Unpin round-trips too.
	if rec := patch(alice, "c-alice", `{"pinned":false}`); rec.Code != http.StatusOK {
		t.Fatalf("unpin status = %d, want 200", rec.Code)
	}
	if stored, _ := st.GetConversation(context.Background(), "c-alice", alice.ID); stored.Pinned {
		t.Error("unpin did not persist")
	}

	// Another user cannot pin the caller's conversation (404, not 500).
	if rec := patch(bob, "c-alice", `{"pinned":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("cross-user pin status = %d, want 404", rec.Code)
	}
}

// TestNewConversationDefaultsUnpinned guards the create path against an
// accidental default-pinned regression.
func TestNewConversationDefaultsUnpinned(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")

	req := httptest.NewRequest(http.MethodPost, "/api/conversations",
		strings.NewReader(`{"model":"glm-5.3-flash"}`))
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleCreateConversation(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var out store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Pinned {
		t.Error("new conversation is pinned")
	}
}
