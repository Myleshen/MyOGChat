package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedUser inserts a minimal active user and returns it.
func seedUser(t *testing.T, s *Store, id, username string, admin bool) *User {
	t.Helper()
	u := &User{
		ID: id, Username: username, Email: username + "@example.com",
		PasswordHash: "x", IsAdmin: admin, Status: "active", CreatedAt: 1, UpdatedAt: 1,
	}
	if err := s.InsertUser(context.Background(), u); err != nil {
		t.Fatalf("InsertUser(%s): %v", username, err)
	}
	return u
}

// TestMigratesLegacySchema builds a pre-users, pre-projects database (the
// original release schema), then confirms Open adds the new columns, preserves
// data, and lets the first admin claim the legacy rows.
func TestMigratesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	const legacy = `
CREATE TABLE conversations (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL,
  effort     TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE messages (
  id                TEXT PRIMARY KEY,
  conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  role              TEXT NOT NULL,
  content           TEXT NOT NULL,
  reasoning         TEXT,
  model             TEXT,
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  cached_tokens     INTEGER,
  cost_micros       INTEGER,
  created_at        INTEGER NOT NULL
);
INSERT INTO conversations (id, title, model, created_at, updated_at)
VALUES ('legacy', 'Old Chat', 'glm-5.3-flash', 111, 222);`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	convCols, err := s.columns("conversations")
	if err != nil {
		t.Fatalf("columns(conversations): %v", err)
	}
	for _, col := range []string{"project_id", "user_id"} {
		if !convCols[col] {
			t.Errorf("migration did not add conversations.%s", col)
		}
	}
	msgCols, err := s.columns("messages")
	if err != nil {
		t.Fatalf("columns(messages): %v", err)
	}
	for _, col := range []string{"reasoning_tokens", "duration_ms", "attachments"} {
		if !msgCols[col] {
			t.Errorf("migration did not add messages.%s", col)
		}
	}
	userCols, err := s.columns("users")
	if err != nil {
		t.Fatalf("columns(users): %v", err)
	}
	if !userCols["status"] {
		t.Error("migration did not add users.status")
	}

	// The first admin claims the ownerless legacy rows.
	u := seedUser(t, s, "u1", "admin", true)
	if err := s.ClaimLegacyData(context.Background(), u.ID); err != nil {
		t.Fatalf("ClaimLegacyData: %v", err)
	}
	c, err := s.GetConversation(context.Background(), "legacy", u.ID)
	if err != nil {
		t.Fatalf("legacy conversation not claimed: %v", err)
	}
	if c.Title != "Old Chat" || c.Model != "glm-5.3-flash" {
		t.Errorf("legacy row corrupted: %+v", c)
	}
}

func TestUserScoping(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)

	aliceID := alice.ID
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c-alice", UserID: &aliceID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}

	// The owner sees it; another user does not.
	if _, err := s.GetConversation(ctx, "c-alice", alice.ID); err != nil {
		t.Fatalf("owner cannot read own conversation: %v", err)
	}
	if _, err := s.GetConversation(ctx, "c-alice", bob.ID); err != ErrNotFound {
		t.Errorf("cross-user read = %v, want ErrNotFound", err)
	}
	if list, _ := s.ListConversations(ctx, bob.ID); len(list) != 0 {
		t.Errorf("bob sees %d conversations, want 0", len(list))
	}

	// Counts and lockout guard.
	if n, _ := s.CountAdmins(ctx); n != 0 {
		t.Errorf("CountAdmins = %d, want 0 (no admin yet)", n)
	}
	seedUser(t, s, "u-admin", "root", true)
	if n, _ := s.CountAdmins(ctx); n != 1 {
		t.Errorf("CountAdmins = %d, want 1", n)
	}
}

func TestProjectsAndUnfile(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "owner", false)

	pid := "p1"
	p := &Project{ID: pid, UserID: &u.ID, Name: "Rust", Instructions: "be terse", CreatedAt: 1, UpdatedAt: 1}
	if err := s.InsertProject(ctx, p); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", UserID: &u.ID, Model: "glm-5.3-flash", ProjectID: &pid, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}

	got, err := s.GetProject(ctx, pid, u.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.ConversationCount != 1 || got.Instructions != "be terse" {
		t.Errorf("GetProject = %+v, want count 1 and instructions", got)
	}

	projects, err := s.ListProjects(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ConversationCount != 1 {
		t.Errorf("ListProjects = %+v, want one project with count 1", projects)
	}

	// Deleting a project must unfile its chats, never delete them.
	if err := s.DeleteProject(ctx, pid, u.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	c, err := s.GetConversation(ctx, "c1", u.ID)
	if err != nil {
		t.Fatalf("conversation deleted with project: %v", err)
	}
	if c.ProjectID != nil {
		t.Errorf("after project delete project_id = %v, want nil", *c.ProjectID)
	}
	if _, err := s.GetProject(ctx, pid, u.ID); err != ErrNotFound {
		t.Errorf("project still present after delete: err=%v", err)
	}
}

func TestRegenerateHelpers(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "owner", false)

	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", UserID: &u.ID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	has, err := s.HasUserMessage(ctx, "c1")
	if err != nil || has {
		t.Fatalf("HasUserMessage on empty = %v, %v; want false, nil", has, err)
	}
	n, err := s.DeleteAssistantAfterLastUser(ctx, "c1")
	if err != nil || n != 0 {
		t.Fatalf("DeleteAssistantAfterLastUser (empty) = %d, %v; want 0, nil", n, err)
	}

	insert := func(id, role string) {
		t.Helper()
		if err := s.InsertMessage(ctx, &Message{
			ID: id, ConversationID: "c1", Role: role, Content: role + " text", CreatedAt: 1,
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", id, err)
		}
	}
	insert("u1", "user")
	insert("a1", "assistant")
	insert("a2", "assistant")

	n, err = s.DeleteAssistantAfterLastUser(ctx, "c1")
	if err != nil {
		t.Fatalf("DeleteAssistantAfterLastUser: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d trailing assistants, want 2", n)
	}
	msgs, err := s.ListMessages(ctx, "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Errorf("after regenerate prep messages = %+v, want the single user message", msgs)
	}
}

func TestAttachmentsScopingAndMessageMetadata(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)

	att := &Attachment{
		ID: "att-1", UserID: alice.ID, Name: "notes.txt", Mime: "text/plain",
		Size: 5, Text: "hello", CreatedAt: 1,
	}
	if err := s.InsertAttachment(ctx, att); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}
	got, err := s.GetAttachment(ctx, "att-1", alice.ID)
	if err != nil {
		t.Fatalf("owner cannot read own attachment: %v", err)
	}
	if got.Text != "hello" || got.Chars() != 5 {
		t.Errorf("GetAttachment = %+v, want text hello and 5 chars", got)
	}
	if _, err := s.GetAttachment(ctx, "att-1", bob.ID); err != ErrNotFound {
		t.Errorf("cross-user read = %v, want ErrNotFound", err)
	}
	if n, err := s.DeleteAttachment(ctx, "att-1", bob.ID); err != nil || n != 0 {
		t.Errorf("cross-user delete = %d, %v; want 0, nil", n, err)
	}

	// A user message persists attachment display metadata as JSON.
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", UserID: &alice.ID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	if err := s.InsertMessage(ctx, &Message{
		ID: "m1", ConversationID: "c1", Role: "user", Content: "see file",
		Attachments: []AttachedFile{{ID: "att-1", Name: "notes.txt", Mime: "text/plain", Size: 5}},
		CreatedAt:   1,
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	msgs, err := s.ListMessages(ctx, "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Name != "notes.txt" {
		t.Errorf("message attachments not round-tripped: %+v", msgs)
	}

	if n, err := s.DeleteAttachment(ctx, "att-1", alice.ID); err != nil || n != 1 {
		t.Errorf("owner delete = %d, %v; want 1, nil", n, err)
	}
	if _, err := s.GetAttachment(ctx, "att-1", alice.ID); err != ErrNotFound {
		t.Errorf("attachment still present after delete: err=%v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	v, err := s.EnsureSetting(ctx, "allow_registration", "1")
	if err != nil || v != "1" {
		t.Fatalf("EnsureSetting = %q, %v; want 1, nil", v, err)
	}
	if err := s.SetSetting(ctx, "allow_registration", "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if v, _ := s.EnsureSetting(ctx, "allow_registration", "1"); v != "0" {
		t.Errorf("EnsureSetting after update = %q, want 0", v)
	}
}
