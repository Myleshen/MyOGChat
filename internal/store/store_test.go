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

// TestMigratesLegacySchema builds a pre-projects database (the original
// release schema), then confirms Open adds the new columns and preserves data.
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
	if !convCols["project_id"] {
		t.Error("migration did not add conversations.project_id")
	}
	msgCols, err := s.columns("messages")
	if err != nil {
		t.Fatalf("columns(messages): %v", err)
	}
	for _, col := range []string{"reasoning_tokens", "duration_ms"} {
		if !msgCols[col] {
			t.Errorf("migration did not add messages.%s", col)
		}
	}

	c, err := s.GetConversation(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("legacy conversation lost: %v", err)
	}
	if c.Title != "Old Chat" || c.Model != "glm-5.3-flash" {
		t.Errorf("legacy row corrupted: %+v", c)
	}
	if c.ProjectID != nil {
		t.Errorf("migrated project_id = %v, want nil", *c.ProjectID)
	}
}

func TestProjectsAndUnfile(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	pid := "p1"
	p := &Project{ID: pid, Name: "Rust", Instructions: "be terse", CreatedAt: 1, UpdatedAt: 1}
	if err := s.InsertProject(ctx, p); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", Model: "glm-5.3-flash", ProjectID: &pid, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}

	got, err := s.GetProject(ctx, pid)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.ConversationCount != 1 || got.Instructions != "be terse" {
		t.Errorf("GetProject = %+v, want count 1 and instructions", got)
	}

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ConversationCount != 1 {
		t.Errorf("ListProjects = %+v, want one project with count 1", projects)
	}

	// Deleting a project must unfile its chats, never delete them.
	if err := s.DeleteProject(ctx, pid); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	c, err := s.GetConversation(ctx, "c1")
	if err != nil {
		t.Fatalf("conversation deleted with project: %v", err)
	}
	if c.ProjectID != nil {
		t.Errorf("after project delete project_id = %v, want nil", *c.ProjectID)
	}
	if _, err := s.GetProject(ctx, pid); err != ErrNotFound {
		t.Errorf("project still present after delete: err=%v", err)
	}
}

func TestRegenerateHelpers(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	has, err := s.HasUserMessage(ctx, "c1")
	if err != nil || has {
		t.Fatalf("HasUserMessage on empty = %v, %v; want false, nil", has, err)
	}
	// No user message: deleting trailing assistants is a no-op.
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

	has, err = s.HasUserMessage(ctx, "c1")
	if err != nil || !has {
		t.Fatalf("HasUserMessage = %v, %v; want true, nil", has, err)
	}
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
