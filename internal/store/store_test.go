package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

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

func TestGroupsAndMembership(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	owner := seedUser(t, s, "u-owner", "owner", false)
	member := seedUser(t, s, "u-member", "member", false)
	other := seedUser(t, s, "u-other", "other", false)

	g := &Group{ID: "g1", Name: "Team", OwnerID: owner.ID, ShareKey: true, CreatedAt: 10, UpdatedAt: 10}
	if err := s.InsertGroup(ctx, g); err != nil {
		t.Fatalf("InsertGroup: %v", err)
	}
	got, err := s.GetGroup(ctx, "g1")
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if got.Name != "Team" || !got.ShareKey || got.OwnerID != owner.ID || len(got.Members) != 0 {
		t.Fatalf("GetGroup = %+v", got)
	}

	// Owner sees it (is_owner); the member does not until added.
	owned, err := s.ListGroupsForUser(ctx, owner.ID)
	if err != nil || len(owned) != 1 || !owned[0].IsOwner {
		t.Fatalf("ListGroupsForUser(owner) = %+v, %v", owned, err)
	}
	if list, _ := s.ListGroupsForUser(ctx, member.ID); len(list) != 0 {
		t.Fatalf("member sees groups before joining: %+v", list)
	}

	// Add member; the member now sees the group and its members list.
	if err := s.AddGroupMember(ctx, "g1", member.ID, "member", 11); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
	if ok, err := s.IsGroupMember(ctx, "g1", member.ID); err != nil || !ok {
		t.Fatalf("IsGroupMember = %v, %v; want true, nil", ok, err)
	}
	memberGroups, err := s.ListGroupsForUser(ctx, member.ID)
	if err != nil || len(memberGroups) != 1 {
		t.Fatalf("ListGroupsForUser(member) = %+v, %v", memberGroups, err)
	}
	if memberGroups[0].IsOwner {
		t.Error("member marked as owner")
	}
	if len(memberGroups[0].Members) != 1 || memberGroups[0].Members[0].Username != "member" {
		t.Fatalf("members = %+v", memberGroups[0].Members)
	}

	// Key-sharing groups: the member qualifies (share on); the owner does not
	// (ownership is not membership).
	keyGroups, err := s.ListKeyGroupsForUser(ctx, member.ID)
	if err != nil || len(keyGroups) != 1 || keyGroups[0].ID != "g1" {
		t.Fatalf("ListKeyGroupsForUser(member) = %+v, %v", keyGroups, err)
	}
	if list, _ := s.ListKeyGroupsForUser(ctx, owner.ID); len(list) != 0 {
		t.Fatalf("owner has key groups: %+v", list)
	}

	// Several applicable groups are returned oldest-first.
	g2 := &Group{ID: "g0", Name: "Older", OwnerID: owner.ID, ShareKey: true, CreatedAt: 5, UpdatedAt: 5}
	if err := s.InsertGroup(ctx, g2); err != nil {
		t.Fatalf("InsertGroup(g0): %v", err)
	}
	if err := s.AddGroupMember(ctx, "g0", member.ID, "member", 11); err != nil {
		t.Fatalf("AddGroupMember(g0): %v", err)
	}
	keyGroups, err = s.ListKeyGroupsForUser(ctx, member.ID)
	if err != nil || len(keyGroups) != 2 || keyGroups[0].ID != "g0" {
		t.Fatalf("key groups order = %+v, %v; want g0 first", keyGroups, err)
	}

	// Turning sharing off removes it from the key list.
	g2.ShareKey = false
	g2.UpdatedAt = 12
	if err := s.UpdateGroup(ctx, g2); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	keyGroups, err = s.ListKeyGroupsForUser(ctx, member.ID)
	if err != nil || len(keyGroups) != 1 || keyGroups[0].ID != "g1" {
		t.Fatalf("key groups after share off = %+v, %v", keyGroups, err)
	}

	// Only the owner may update or delete (guarded by owner_id).
	if err := s.UpdateGroup(ctx, &Group{ID: "g1", Name: "Hax", OwnerID: other.ID, UpdatedAt: 13}); err != ErrNotFound {
		t.Errorf("non-owner UpdateGroup = %v, want ErrNotFound", err)
	}
	if n, err := s.DeleteGroup(ctx, "g1", other.ID); err != nil || n != 0 {
		t.Errorf("non-owner DeleteGroup = %d, %v; want 0, nil", n, err)
	}

	// Remove the member, then delete the group; memberships cascade.
	if n, err := s.RemoveGroupMember(ctx, "g1", member.ID); err != nil || n != 1 {
		t.Fatalf("RemoveGroupMember = %d, %v; want 1, nil", n, err)
	}
	if n, err := s.DeleteGroup(ctx, "g1", owner.ID); err != nil || n != 1 {
		t.Fatalf("DeleteGroup = %d, %v; want 1, nil", n, err)
	}
	if _, err := s.GetGroup(ctx, "g1"); err != ErrNotFound {
		t.Errorf("GetGroup after delete = %v, want ErrNotFound", err)
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

// TestGeoCache covers the maps result cache (3.4): round-trip, expiry, and
// janitor purge.
func TestGeoCache(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if err := s.PutGeoCache(ctx, "geocode:us:5:paris", `[{"name":"Paris"}]`, now+60_000); err != nil {
		t.Fatalf("PutGeoCache: %v", err)
	}
	v, ok, err := s.GetGeoCache(ctx, "geocode:us:5:paris")
	if err != nil || !ok || v != `[{"name":"Paris"}]` {
		t.Fatalf("GetGeoCache = %q, %v, %v", v, ok, err)
	}

	// Upsert replaces the value.
	if err := s.PutGeoCache(ctx, "geocode:us:5:paris", `[{"name":"Lutetia"}]`, now+60_000); err != nil {
		t.Fatalf("PutGeoCache (upsert): %v", err)
	}
	if v, _, _ := s.GetGeoCache(ctx, "geocode:us:5:paris"); v != `[{"name":"Lutetia"}]` {
		t.Errorf("upsert value = %q", v)
	}

	// An expired row is never returned.
	if err := s.PutGeoCache(ctx, "reverse:1,2", `{}`, now-1); err != nil {
		t.Fatalf("PutGeoCache (expired): %v", err)
	}
	if _, ok, _ := s.GetGeoCache(ctx, "reverse:1,2"); ok {
		t.Error("expired row was returned")
	}

	// A missing key is simply absent.
	if _, ok, err := s.GetGeoCache(ctx, "nope"); err != nil || ok {
		t.Errorf("GetGeoCache(missing) = %v, %v; want false, nil", ok, err)
	}

	// The janitor purges only expired rows.
	n, err := s.DeleteExpiredGeoCache(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredGeoCache: %v", err)
	}
	if n != 1 {
		t.Errorf("purged = %d, want 1", n)
	}
	if _, ok, _ := s.GetGeoCache(ctx, "geocode:us:5:paris"); !ok {
		t.Error("unexpired row was purged")
	}
}
