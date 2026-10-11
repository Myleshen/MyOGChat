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

// postConversation drives handleCreateConversation with an injected user.
func postConversation(t *testing.T, srv *Server, u store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/conversations", strings.NewReader(body))
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleCreateConversation(rec, req)
	return rec
}

// seedDraft inserts a conversation row for the 5.10 reuse tests.
func seedDraft(t *testing.T, st *store.Store, id, userID, title string, projectID *string, pinned bool, updatedAt int64) {
	t.Helper()
	uid := userID
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: id, UserID: &uid, Title: title, Model: "glm-5.3-flash",
		ProjectID: projectID, Pinned: pinned, CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", id, err)
	}
}

func countConversations(t *testing.T, st *store.Store, userID string) int {
	t.Helper()
	list, err := st.ListConversations(context.Background(), userID)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	return len(list)
}

// TestCreateConversationReusesDraft covers the reuse hit (same id, no second
// row), the no-draft create, and back-compat when the flag is absent.
func TestCreateConversationReusesDraft(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u-alice", "alice", "")
	bob := seedKeyUser(t, st, "u-bob", "bob", "")

	// No draft yet → a normal 201 create.
	rec := postConversation(t, srv, bob, `{"model":"glm-5.3-flash","reuse_empty":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("no-draft status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// Seed an eligible draft; reuse must return it with 200 and create nothing.
	seedDraft(t, st, "d1", alice.ID, "", nil, false, 100)
	rec = postConversation(t, srv, alice, `{"model":"glm-5.3-flash","reuse_empty":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reuse status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != "d1" {
		t.Errorf("reused id = %q, want d1", out.ID)
	}
	if n := countConversations(t, st, alice.ID); n != 1 {
		t.Errorf("conversation count = %d, want 1 (reused draft only)", n)
	}

	// Without the flag a new row is created even though a draft exists.
	rec = postConversation(t, srv, alice, `{"model":"glm-5.3-flash"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("no-flag status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if n := countConversations(t, st, alice.ID); n != 2 {
		t.Errorf("conversation count = %d, want 2", n)
	}
}

// TestCreateConversationReuseExclusions checks that titled, messaged, pinned
// and other users' drafts are never reused.
func TestCreateConversationReuseExclusions(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u-alice", "alice", "")
	bob := seedKeyUser(t, st, "u-bob", "bob", "")

	seedDraft(t, st, "titled", alice.ID, "Named", nil, false, 100)
	seedDraft(t, st, "pinned", alice.ID, "", nil, true, 200)
	seedDraft(t, st, "messaged", alice.ID, "", nil, false, 300)
	if err := st.InsertMessage(context.Background(), &store.Message{
		ID: "m1", ConversationID: "messaged", Role: "user", Content: "hi", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	seedDraft(t, st, "bobs", bob.ID, "", nil, false, 400)

	rec := postConversation(t, srv, alice, `{"model":"glm-5.3-flash","reuse_empty":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var out store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID == "titled" || out.ID == "pinned" || out.ID == "messaged" || out.ID == "bobs" {
		t.Errorf("reused ineligible draft %q", out.ID)
	}
	// Bob's draft is untouched and still his only row.
	if n := countConversations(t, st, bob.ID); n != 1 {
		t.Errorf("bob rows = %d, want 1", n)
	}
}

// TestCreateConversationReuseProjectScope checks NULL-safe project scoping.
func TestCreateConversationReuseProjectScope(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u-alice", "alice", "")
	projA, projB := "p-a", "p-b"
	for _, id := range []string{projA, projB} {
		p := id
		if err := st.InsertProject(context.Background(), &store.Project{
			ID: p, UserID: &alice.ID, Name: p, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("InsertProject(%s): %v", p, err)
		}
	}
	seedDraft(t, st, "unfiled", alice.ID, "", nil, false, 100)
	seedDraft(t, st, "in-a", alice.ID, "", &projA, false, 200)
	seedDraft(t, st, "in-b", alice.ID, "", &projB, false, 300)

	// Project A scope returns only the project A draft.
	rec := postConversation(t, srv, alice, `{"model":"glm-5.3-flash","project_id":"p-a","reuse_empty":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("project A status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != "in-a" {
		t.Errorf("project A draft = %q, want in-a", out.ID)
	}

	// Unfiled scope returns the unfiled draft, not a project one.
	rec = postConversation(t, srv, alice, `{"model":"glm-5.3-flash","reuse_empty":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unfiled status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != "unfiled" {
		t.Errorf("unfiled draft = %q, want unfiled", out.ID)
	}
}

// TestCreateConversationReuseAlignsModel checks that an explicitly different
// model (and its effort) is written onto the reused draft.
func TestCreateConversationReuseAlignsModel(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u-alice", "alice", "")
	seedDraft(t, st, "d1", alice.ID, "", nil, false, 100)

	// glm-5.3-flash draft, chat-dialect request for glm-5.2.
	rec := postConversation(t, srv, alice, `{"model":"glm-5.2","reuse_empty":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err := st.GetConversation(context.Background(), "d1", alice.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Model != "glm-5.2" {
		t.Errorf("model = %q, want glm-5.2", got.Model)
	}

	// A responses-dialect request carries an effort that must be aligned too.
	rec = postConversation(t, srv, alice, `{"model":"gpt-5.6-luna","effort":"high","reuse_empty":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("responses status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got, err = st.GetConversation(context.Background(), "d1", alice.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Model != "gpt-5.6-luna" || got.Effort == nil || *got.Effort != "high" {
		t.Errorf("aligned draft = %+v, want gpt-5.6-luna/high", got)
	}
}
