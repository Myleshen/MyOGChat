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

// seedExportConversation inserts an owned conversation with one user + one
// assistant message (with model/cost) and returns it.
func seedExportConversation(t *testing.T, st *store.Store, owner store.User) store.Conversation {
	t.Helper()
	ctx := context.Background()
	conv := store.Conversation{
		ID: "c1", UserID: &owner.ID, Title: "My Chat!", Model: "glm-5.3-flash",
		CreatedAt: 1000, UpdatedAt: 2000,
	}
	if err := st.InsertConversation(ctx, &conv); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	model := "glm-5.3-flash"
	cost := int64(1234)
	msgs := []store.Message{
		{ID: "m1", ConversationID: "c1", Role: "user", Content: "Hello world", CreatedAt: 1000},
		{ID: "a1", ConversationID: "c1", Role: "assistant", Content: "Hi there", Model: &model, CostMicros: &cost, CreatedAt: 2000},
	}
	for i := range msgs {
		if err := st.InsertMessage(ctx, &msgs[i]); err != nil {
			t.Fatalf("InsertMessage(%s): %v", msgs[i].ID, err)
		}
	}
	return conv
}

func exportRequest(t *testing.T, srv *Server, u store.User, convID, format string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/conversations/"+convID+"/export?format="+format, nil)
	req.SetPathValue("id", convID)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleExportConversation(rec, req)
	return rec
}

func TestExportConversationJSON(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedExportConversation(t, st, u)

	rec := exportRequest(t, srv, u, "c1", "json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="my-chat-c1.json"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	var out struct {
		store.Conversation
		Messages []store.Message `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != "c1" || out.Title != "My Chat!" {
		t.Errorf("conversation = %+v", out.Conversation)
	}
	if len(out.Messages) != 2 || out.Messages[0].Role != "user" || out.Messages[1].Role != "assistant" {
		t.Fatalf("messages = %+v", out.Messages)
	}
}

func TestExportConversationMarkdown(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedExportConversation(t, st, u)

	rec := exportRequest(t, srv, u, "c1", "md")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="my-chat-c1.md"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	body := rec.Body.String()
	for _, want := range []string{"# My Chat!", "## You", "## Assistant", "Hello world", "Hi there", "glm-5.3-flash", "$0.0012"} {
		if !strings.Contains(body, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, body)
		}
	}
}

// TestExportConversationScoping verifies a non-owner gets 404 (no cross-user
// transcript leak), matching every other conversation route.
func TestExportConversationScoping(t *testing.T) {
	srv, st := newKeyTestServer(t)
	owner := seedKeyUser(t, st, "u1", "alice", "")
	other := seedKeyUser(t, st, "u2", "bob", "")
	seedExportConversation(t, st, owner)

	rec := exportRequest(t, srv, other, "c1", "json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user export status = %d, want 404", rec.Code)
	}
}

func TestExportConversationUnknownFormat(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedExportConversation(t, st, u)
	rec := exportRequest(t, srv, u, "c1", "pdf")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown format status = %d, want 400", rec.Code)
	}
}

func TestAdminBackupAdminOnly(t *testing.T) {
	srv, st := newKeyTestServer(t)
	user := seedKeyUser(t, st, "u1", "alice", "")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/backup", nil)
	req = req.WithContext(auth.WithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	srv.handleAdminBackup(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin backup status = %d, want 403", rec.Code)
	}
}

func TestAdminBackupServesSQLite(t *testing.T) {
	srv, st := newKeyTestServer(t)
	admin := store.User{
		ID: "u1", Username: "admin", Email: "admin@example.com",
		PasswordHash: "x", IsAdmin: true, Status: "active", CreatedAt: 1, UpdatedAt: 1,
	}
	if err := st.InsertUser(context.Background(), &admin); err != nil {
		t.Fatalf("InsertUser: %v", err)
	}
	seedExportConversation(t, st, admin)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/backup", nil)
	req = req.WithContext(auth.WithUser(req.Context(), admin))
	rec := httptest.NewRecorder()
	srv.handleAdminBackup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "myogchat-backup-") || !strings.HasSuffix(cd, `.db"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	body := rec.Body.Bytes()
	if len(body) < 16 || string(body[:16]) != "SQLite format 3\x00" {
		t.Fatalf("backup is not a SQLite file (first bytes %q)", body[:min(16, len(body))])
	}
}
