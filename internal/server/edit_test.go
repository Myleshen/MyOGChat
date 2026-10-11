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

// editRequest drives handleEditMessage with an injected user and body.
func editRequest(t *testing.T, srv *Server, u store.User, convID, msgID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+convID+"/messages/"+msgID+"/edit", strings.NewReader(body))
	req.SetPathValue("id", convID)
	req.SetPathValue("messageId", msgID)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleEditMessage(rec, req)
	return rec
}

func seedEditConversation(t *testing.T, st *store.Store, owner store.User) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertConversation(ctx, &store.Conversation{
		ID: "c1", UserID: &owner.ID, Title: "T", Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	for _, m := range []store.Message{
		{ID: "m1", ConversationID: "c1", Role: "user", Content: "original", CreatedAt: 1},
		{ID: "a1", ConversationID: "c1", Role: "assistant", Content: "reply", CreatedAt: 2},
	} {
		mm := m
		if err := st.InsertMessage(ctx, &mm); err != nil {
			t.Fatalf("InsertMessage(%s): %v", mm.ID, err)
		}
	}
}

func TestEditMessageSuccess(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedEditConversation(t, st, u)

	rec := editRequest(t, srv, u, "c1", "m1", `{"content":"edited text"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out["ok"] {
		t.Errorf("response = %s, want ok:true", rec.Body.String())
	}
	msgs, err := st.ListMessages(context.Background(), "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// The edit rewrote m1 and truncated the trailing assistant reply.
	if len(msgs) != 1 || msgs[0].ID != "m1" || msgs[0].Content != "edited text" {
		t.Fatalf("after edit = %+v, want only edited m1", msgs)
	}
}

// TestEditMessageNotFound covers a non-user role target and a cross-user
// conversation, both of which must be indistinguishable from a missing row.
func TestEditMessageNotFound(t *testing.T) {
	srv, st := newKeyTestServer(t)
	owner := seedKeyUser(t, st, "u1", "alice", "")
	other := seedKeyUser(t, st, "u2", "bob", "")
	seedEditConversation(t, st, owner)

	// Editing an assistant message → 404.
	if rec := editRequest(t, srv, owner, "c1", "a1", `{"content":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("edit assistant status = %d, want 404", rec.Code)
	}
	// The other user cannot see the conversation at all → 404.
	if rec := editRequest(t, srv, other, "c1", "m1", `{"content":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("cross-user edit status = %d, want 404", rec.Code)
	}
	// A missing message id → 404.
	if rec := editRequest(t, srv, owner, "c1", "ghost", `{"content":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing message status = %d, want 404", rec.Code)
	}
}

func TestEditMessageValidation(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedEditConversation(t, st, u)

	if rec := editRequest(t, srv, u, "c1", "m1", `{"content":"   "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty content status = %d, want 400", rec.Code)
	}
	big := `{"content":"` + strings.Repeat("a", maxContentBytes+1) + `"}`
	if rec := editRequest(t, srv, u, "c1", "m1", big); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized content status = %d, want 400", rec.Code)
	}
	// The failed attempts must not have modified the stored message.
	msgs, _ := st.ListMessages(context.Background(), "c1")
	if len(msgs) != 2 || msgs[0].Content != "original" {
		t.Errorf("validation failures mutated state: %+v", msgs)
	}
}
