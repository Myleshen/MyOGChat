package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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
	for _, col := range []string{"reasoning_tokens", "duration_ms", "attachments", "tools", "images"} {
		if !msgCols[col] {
			t.Errorf("migration did not add messages.%s", col)
		}
	}
	userCols, err := s.columns("users")
	if err != nil {
		t.Fatalf("columns(users): %v", err)
	}
	for _, col := range []string{"status", "daily_token_limit", "monthly_cost_limit"} {
		if !userCols[col] {
			t.Errorf("migration did not add users.%s", col)
		}
	}
	if !userCols["api_key_enc"] {
		t.Error("migration did not add users.api_key_enc")
	}
	if !userCols["serper_key_enc"] {
		t.Error("migration did not add users.serper_key_enc")
	}
	if !userCols["linkwarden_url"] {
		t.Error("migration did not add users.linkwarden_url")
	}
	if !userCols["linkwarden_token_enc"] {
		t.Error("migration did not add users.linkwarden_token_enc")
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

// TestUserSerperKeyRoundTrip stores and clears a per-user Serper key.
func TestUserSerperKeyRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u-serper", "serper", false)

	enc := "ciphertext"
	if err := s.SetUserSerperKey(ctx, u.ID, &enc, 42); err != nil {
		t.Fatalf("SetUserSerperKey: %v", err)
	}
	got, err := s.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.SerperKeyEnc == nil || *got.SerperKeyEnc != "ciphertext" {
		t.Fatalf("SerperKeyEnc = %v, want ciphertext", got.SerperKeyEnc)
	}
	if got.UpdatedAt != 42 {
		t.Errorf("UpdatedAt = %d, want 42", got.UpdatedAt)
	}

	// Clearing sets NULL.
	if err := s.SetUserSerperKey(ctx, u.ID, nil, 43); err != nil {
		t.Fatalf("SetUserSerperKey(clear): %v", err)
	}
	got, _ = s.GetUserByID(ctx, u.ID)
	if got.SerperKeyEnc != nil {
		t.Fatalf("SerperKeyEnc = %v, want nil", got.SerperKeyEnc)
	}
}

// TestUserLinkwardenRoundTrip stores and clears a per-user Linkwarden config
// and confirms it is scoped to the owning user (2.8).
func TestUserLinkwardenRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-lw-alice", "alice", false)
	bob := seedUser(t, s, "u-lw-bob", "bob", false)

	enc := "ciphertext"
	if err := s.SetUserLinkwarden(ctx, alice.ID, "https://links.example.com", &enc, 42); err != nil {
		t.Fatalf("SetUserLinkwarden: %v", err)
	}
	got, err := s.GetUserByID(ctx, alice.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.LinkwardenURL == nil || *got.LinkwardenURL != "https://links.example.com" {
		t.Fatalf("LinkwardenURL = %v, want the stored url", got.LinkwardenURL)
	}
	if got.LinkwardenTokenEnc == nil || *got.LinkwardenTokenEnc != "ciphertext" {
		t.Fatalf("LinkwardenTokenEnc = %v, want ciphertext", got.LinkwardenTokenEnc)
	}
	if got.UpdatedAt != 42 {
		t.Errorf("UpdatedAt = %d, want 42", got.UpdatedAt)
	}

	// The other user is unaffected (scoping).
	other, _ := s.GetUserByID(ctx, bob.ID)
	if other.LinkwardenURL != nil || other.LinkwardenTokenEnc != nil {
		t.Fatalf("bob's Linkwarden = %v / %v, want nil", other.LinkwardenURL, other.LinkwardenTokenEnc)
	}

	// Clearing both sets NULL.
	if err := s.SetUserLinkwarden(ctx, alice.ID, "", nil, 43); err != nil {
		t.Fatalf("SetUserLinkwarden(clear): %v", err)
	}
	got, _ = s.GetUserByID(ctx, alice.ID)
	if got.LinkwardenURL != nil || got.LinkwardenTokenEnc != nil {
		t.Fatalf("Linkwarden not cleared: %v / %v", got.LinkwardenURL, got.LinkwardenTokenEnc)
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

func TestMessageToolsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "owner", false)
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", UserID: &u.ID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	if err := s.InsertMessage(ctx, &Message{
		ID: "a1", ConversationID: "c1", Role: "assistant", Content: "hi",
		Tools: []ToolActivity{
			{Name: "web_search", Status: "done", Summary: "3 results",
				Args: `{"query":"go"}`, Output: "result body", DurationMs: 42},
			{Name: "run_bash", Status: "error", Summary: "boom",
				Error: "exit 1", DurationMs: 7, Truncated: true},
		},
		CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertMessage(a1): %v", err)
	}
	// A message with no tools must round-trip as nil, not an empty slice.
	if err := s.InsertMessage(ctx, &Message{
		ID: "a2", ConversationID: "c1", Role: "assistant", Content: "plain", CreatedAt: 2,
	}); err != nil {
		t.Fatalf("InsertMessage(a2): %v", err)
	}
	msgs, err := s.ListMessages(ctx, "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	got := msgs[0].Tools
	if len(got) != 2 ||
		got[0].Name != "web_search" || got[0].Status != "done" || got[0].Summary != "3 results" ||
		got[0].Args != `{"query":"go"}` || got[0].Output != "result body" || got[0].DurationMs != 42 ||
		got[1].Name != "run_bash" || got[1].Status != "error" || got[1].Summary != "boom" ||
		got[1].Error != "exit 1" || got[1].DurationMs != 7 || !got[1].Truncated {
		t.Errorf("tools not round-tripped: %+v", got)
	}
	if msgs[1].Tools != nil {
		t.Errorf("message with no tools = %+v, want nil", msgs[1].Tools)
	}
}

// TestMessageImagesRoundTrip checks that a message's image refs survive a
// round-trip (feature 3.1).
func TestMessageImagesRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u-img", "imger", false)
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "ci1", UserID: &u.ID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	if err := s.InsertMessage(ctx, &Message{
		ID: "mi1", ConversationID: "ci1", Role: "assistant", Content: "",
		Images:    []GeneratedImage{{ID: "img-1", Prompt: "a red fox", Mime: "image/png", Width: 512, Height: 512, Size: 4}},
		CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	msgs, err := s.ListMessages(ctx, "ci1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Images) != 1 || msgs[0].Images[0].ID != "img-1" {
		t.Fatalf("message images not round-tripped: %+v", msgs)
	}
	if msgs[0].Images[0].Prompt != "a red fox" {
		t.Errorf("image prompt = %q, want %q", msgs[0].Images[0].Prompt, "a red fox")
	}
}

// TestImagesRoundTripAndScoping covers the images store (feature 3.1):
// round-trip, owner scoping, and the retention janitor.
func TestImagesRoundTripAndScoping(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)

	img := &Image{
		ID: "img-1", UserID: alice.ID, Prompt: "a red fox", Model: "sd_turbo.safetensors",
		Mime: "image/png", Width: 512, Height: 512, Size: 4,
		Data: []byte("PNG!"), CreatedAt: 1000,
	}
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatalf("InsertImage: %v", err)
	}

	got, err := s.GetImage(ctx, "img-1", alice.ID)
	if err != nil {
		t.Fatalf("owner cannot read own image: %v", err)
	}
	if got.Prompt != "a red fox" || string(got.Data) != "PNG!" || got.Width != 512 || got.Height != 512 {
		t.Errorf("GetImage = %+v (data %q), want the stored row", got, got.Data)
	}

	// Cross-user read and delete are indistinguishable from a missing row.
	if _, err := s.GetImage(ctx, "img-1", bob.ID); err != ErrNotFound {
		t.Errorf("cross-user read = %v, want ErrNotFound", err)
	}
	if n, err := s.DeleteImage(ctx, "img-1", bob.ID); err != nil || n != 0 {
		t.Errorf("cross-user delete = %d, %v; want 0, nil", n, err)
	}

	// Retention janitor purge by age.
	if n, err := s.DeleteImagesOlderThan(ctx, 500); err != nil || n != 0 {
		t.Errorf("DeleteImagesOlderThan(500) = %d, %v; want 0, nil", n, err)
	}
	if n, err := s.DeleteImagesOlderThan(ctx, 2000); err != nil || n != 1 {
		t.Errorf("DeleteImagesOlderThan(2000) = %d, %v; want 1, nil", n, err)
	}

	// Owner delete.
	if err := s.InsertImage(ctx, img); err != nil {
		t.Fatalf("re-InsertImage: %v", err)
	}
	if n, err := s.DeleteImage(ctx, "img-1", alice.ID); err != nil || n != 1 {
		t.Errorf("owner delete = %d, %v; want 1, nil", n, err)
	}
}

// TestArtifactsRoundTripAndScoping covers the artifacts store (5.8):
// round-trip, owner scoping, the retention janitor, and the message ref list.
func TestArtifactsRoundTripAndScoping(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "ca1", UserID: &alice.ID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}

	conv := "ca1"
	art := &Artifact{
		ID: "art-1", UserID: alice.ID, ConversationID: &conv, Name: "report.csv",
		Mime: "text/csv", Size: 4, SHA256: "deadbeef", Data: []byte("a,b\n"), CreatedAt: 1000,
	}
	if err := s.InsertArtifact(ctx, art); err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}

	got, err := s.GetArtifact(ctx, "art-1", alice.ID)
	if err != nil {
		t.Fatalf("owner cannot read own artifact: %v", err)
	}
	if got.Name != "report.csv" || string(got.Data) != "a,b\n" || got.Mime != "text/csv" ||
		got.ConversationID == nil || *got.ConversationID != "ca1" || got.SHA256 != "deadbeef" {
		t.Errorf("GetArtifact = %+v", got)
	}

	// Cross-user read and delete are indistinguishable from a missing row.
	if _, err := s.GetArtifact(ctx, "art-1", bob.ID); err != ErrNotFound {
		t.Errorf("cross-user read = %v, want ErrNotFound", err)
	}
	if n, err := s.DeleteArtifact(ctx, "art-1", bob.ID); err != nil || n != 0 {
		t.Errorf("cross-user delete = %d, %v; want 0, nil", n, err)
	}

	// The reference survives a message round-trip.
	if err := s.InsertMessage(ctx, &Message{
		ID: "ma1", ConversationID: "ca1", Role: "assistant",
		Artifacts: []ArtifactRef{{ID: "art-1", Name: "report.csv", Mime: "text/csv", Size: 4}},
		CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	msgs, err := s.ListMessages(ctx, "ca1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Artifacts) != 1 || msgs[0].Artifacts[0].Name != "report.csv" {
		t.Fatalf("message artifacts not round-tripped: %+v", msgs)
	}

	// Retention purge by age, then owner delete.
	if n, err := s.DeleteArtifactsOlderThan(ctx, 500); err != nil || n != 0 {
		t.Errorf("DeleteArtifactsOlderThan(500) = %d, %v; want 0, nil", n, err)
	}
	if n, err := s.DeleteArtifactsOlderThan(ctx, 2000); err != nil || n != 1 {
		t.Errorf("DeleteArtifactsOlderThan(2000) = %d, %v; want 1, nil", n, err)
	}
	if err := s.InsertArtifact(ctx, art); err != nil {
		t.Fatalf("re-InsertArtifact: %v", err)
	}
	if n, err := s.DeleteArtifact(ctx, "art-1", alice.ID); err != nil || n != 1 {
		t.Errorf("owner delete = %d, %v; want 1, nil", n, err)
	}
}

func int64p(v int64) *int64 { return &v }

// seedUsageMessage inserts an assistant message with the given counters.
func seedUsageMessage(t *testing.T, s *Store, id, convID, model string, createdAt, prompt, completion, cached, reasoning, cost int64) {
	t.Helper()
	if err := s.InsertMessage(context.Background(), &Message{
		ID: id, ConversationID: convID, Role: "assistant", Content: "x", Model: &model,
		PromptTokens: int64p(prompt), CompletionTokens: int64p(completion),
		CachedTokens: int64p(cached), ReasoningTokens: int64p(reasoning),
		CostMicros: int64p(cost), CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("InsertMessage(%s): %v", id, err)
	}
}

func TestUsageAggregates(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)

	for _, c := range []struct {
		id  string
		uid string
	}{{"c-alice", alice.ID}, {"c-bob", bob.ID}} {
		uid := c.uid
		if err := s.InsertConversation(ctx, &Conversation{
			ID: c.id, UserID: &uid, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("InsertConversation(%s): %v", c.id, err)
		}
	}

	day1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	day2 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC).UnixMilli()

	// A user message (nil tokens) must be ignored by every aggregate.
	if err := s.InsertMessage(ctx, &Message{
		ID: "u1", ConversationID: "c-alice", Role: "user", Content: "q", CreatedAt: day1,
	}); err != nil {
		t.Fatalf("InsertMessage(user): %v", err)
	}
	seedUsageMessage(t, s, "a1", "c-alice", "model-a", day1, 100, 50, 10, 5, 1000)
	seedUsageMessage(t, s, "a2", "c-alice", "model-b", day2, 200, 100, 20, 10, 2000)
	seedUsageMessage(t, s, "b1", "c-bob", "model-a", day1, 7, 7, 0, 0, 70)

	totals, err := s.UserUsageTotals(ctx, alice.ID, 0)
	if err != nil {
		t.Fatalf("UserUsageTotals: %v", err)
	}
	want := UsageTotals{PromptTokens: 300, CompletionTokens: 150, CachedTokens: 30, ReasoningTokens: 15, CostMicros: 3000, Messages: 2}
	if totals != want {
		t.Errorf("UserUsageTotals = %+v, want %+v", totals, want)
	}

	// Cutoff filtering excludes day1 (alice's day1 cost 1000).
	totals, err = s.UserUsageTotals(ctx, alice.ID, day2)
	if err != nil {
		t.Fatalf("UserUsageTotals(cutoff): %v", err)
	}
	if totals.CostMicros != 2000 || totals.Messages != 1 {
		t.Errorf("cutoff totals = %+v, want cost 2000 / 1 message", totals)
	}

	byDay, err := s.UserUsageByDay(ctx, alice.ID, 0)
	if err != nil {
		t.Fatalf("UserUsageByDay: %v", err)
	}
	if len(byDay) != 2 || byDay[0].Day != "2026-05-01" || byDay[1].Day != "2026-05-02" {
		t.Fatalf("by day = %+v, want two ordered buckets", byDay)
	}
	if byDay[0].CostMicros != 1000 || byDay[1].CostMicros != 2000 {
		t.Errorf("by day cost = %d, %d; want 1000, 2000", byDay[0].CostMicros, byDay[1].CostMicros)
	}

	byModel, err := s.UserUsageByModel(ctx, alice.ID, 0)
	if err != nil {
		t.Fatalf("UserUsageByModel: %v", err)
	}
	if len(byModel) != 2 || byModel[0].Model != "model-b" || byModel[1].Model != "model-a" {
		t.Fatalf("by model = %+v, want model-b (costlier) first", byModel)
	}

	// All-users view includes both, and excludes the user message.
	all, err := s.AllUsersUsage(ctx, 0)
	if err != nil {
		t.Fatalf("AllUsersUsage: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all users = %+v, want 2", all)
	}
	byUser := map[string]UserUsage{}
	for _, uu := range all {
		byUser[uu.UserID] = uu
	}
	if byUser[alice.ID].CostMicros != 3000 || byUser[bob.ID].CostMicros != 70 {
		t.Errorf("all users cost = %+v", byUser)
	}
	if byUser[bob.ID].Username != "bob" {
		t.Errorf("all users username = %q, want bob", byUser[bob.ID].Username)
	}
}

func TestUserLimitsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "owner", false)
	if u.DailyTokenLimit != nil || u.MonthlyCostLimit != nil {
		t.Fatalf("fresh user limits = %v, %v; want nil, nil", u.DailyTokenLimit, u.MonthlyCostLimit)
	}
	if err := s.SetUserLimits(ctx, u.ID, int64p(1000), int64p(2000), 2); err != nil {
		t.Fatalf("SetUserLimits: %v", err)
	}
	got, err := s.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.DailyTokenLimit == nil || *got.DailyTokenLimit != 1000 || got.MonthlyCostLimit == nil || *got.MonthlyCostLimit != 2000 {
		t.Errorf("limits after set = %v, %v", got.DailyTokenLimit, got.MonthlyCostLimit)
	}
	// nil clears back to inherit.
	if err := s.SetUserLimits(ctx, u.ID, nil, nil, 3); err != nil {
		t.Fatalf("SetUserLimits(clear): %v", err)
	}
	got, _ = s.GetUserByID(ctx, u.ID)
	if got.DailyTokenLimit != nil || got.MonthlyCostLimit != nil {
		t.Errorf("limits after clear = %v, %v; want nil, nil", got.DailyTokenLimit, got.MonthlyCostLimit)
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

func TestCalendarAccounts(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u1", "alice", false)
	bob := seedUser(t, s, "u2", "bob", false)

	if _, err := s.GetCalendarAccount(ctx, alice.ID); err != ErrNotFound {
		t.Fatalf("missing account err = %v, want ErrNotFound", err)
	}

	acc := &CalendarAccount{
		ID: "cal-1", UserID: alice.ID, Kind: "caldav", Provider: "apple",
		CalendarID: "/123/calendars/home/", Username: "alice@example.com",
		CredentialEnc: "enc-secret", Endpoint: "https://p01-caldav.icloud.com",
		CreatedAt: 1, UpdatedAt: 1,
	}
	if err := s.UpsertCalendarAccount(ctx, acc); err != nil {
		t.Fatalf("UpsertCalendarAccount: %v", err)
	}
	got, err := s.GetCalendarAccount(ctx, alice.ID)
	if err != nil {
		t.Fatalf("GetCalendarAccount: %v", err)
	}
	if got.Kind != "caldav" || got.Username != "alice@example.com" || got.CredentialEnc != "enc-secret" || got.CalendarID != "/123/calendars/home/" {
		t.Errorf("account round-trip = %+v", got)
	}
	// Scoping: another user sees nothing.
	if _, err := s.GetCalendarAccount(ctx, bob.ID); err != ErrNotFound {
		t.Errorf("cross-user account err = %v, want ErrNotFound", err)
	}

	// Upsert replaces in place (one account per user) and keeps created_at.
	acc.Kind = "ics"
	acc.CredentialEnc = "enc-url"
	acc.UpdatedAt = 2
	if err := s.UpsertCalendarAccount(ctx, acc); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}
	got, _ = s.GetCalendarAccount(ctx, alice.ID)
	if got.Kind != "ics" || got.CredentialEnc != "enc-url" || got.CreatedAt != 1 {
		t.Errorf("upsert did not replace correctly: %+v", got)
	}

	if n, err := s.DeleteCalendarAccount(ctx, alice.ID); err != nil || n != 1 {
		t.Fatalf("DeleteCalendarAccount = %d, %v; want 1, nil", n, err)
	}
	if _, err := s.GetCalendarAccount(ctx, alice.ID); err != ErrNotFound {
		t.Errorf("account still present after delete")
	}
}

// TestMailAccountRoundTrip covers the per-user mailbox (feature 3.2): connect,
// read back, toggle, disconnect, and the ErrNotFound path.
func TestMailAccountRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u-mail", "mailer", false)

	if _, err := s.GetMailAccount(ctx, u.ID); err != ErrNotFound {
		t.Fatalf("GetMailAccount before connect = %v, want ErrNotFound", err)
	}

	acct := &MailAccount{
		UserID: u.ID, Provider: "gmail", Email: "mailer@gmail.com",
		Host: "imap.gmail.com", Port: 993, TLS: "implicit", Enabled: true,
		SecretEnc: "ciphertext", CreatedAt: 10, UpdatedAt: 10,
	}
	if err := s.SetMailAccount(ctx, acct); err != nil {
		t.Fatalf("SetMailAccount: %v", err)
	}
	got, err := s.GetMailAccount(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetMailAccount: %v", err)
	}
	if got.Provider != "gmail" || got.Email != "mailer@gmail.com" || got.Host != "imap.gmail.com" || !got.Enabled || got.SecretEnc != "ciphertext" {
		t.Fatalf("mail account = %+v", got)
	}

	if err := s.SetMailAccountEnabled(ctx, u.ID, false, 20); err != nil {
		t.Fatalf("SetMailAccountEnabled: %v", err)
	}
	if got, _ := s.GetMailAccount(ctx, u.ID); got.Enabled {
		t.Error("enabled = true after disabling")
	}

	// Upsert replaces the credential.
	acct.SecretEnc = "new-ciphertext"
	acct.UpdatedAt = 30
	if err := s.SetMailAccount(ctx, acct); err != nil {
		t.Fatalf("SetMailAccount (upsert): %v", err)
	}
	if got, _ := s.GetMailAccount(ctx, u.ID); got.SecretEnc != "new-ciphertext" {
		t.Errorf("SecretEnc = %q after upsert", got.SecretEnc)
	}

	if err := s.DeleteMailAccount(ctx, u.ID); err != nil {
		t.Fatalf("DeleteMailAccount: %v", err)
	}
	if _, err := s.GetMailAccount(ctx, u.ID); err != ErrNotFound {
		t.Fatalf("GetMailAccount after delete = %v, want ErrNotFound", err)
	}
}

// TestEditUserMessageAndTruncate covers the edit/truncate store methods used by
// the edit-and-resend flow (§18): only the owner's user messages can be edited,
// and TruncateAfter drops everything that follows by (created_at, rowid).
func TestEditUserMessageAndTruncate(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	owner := seedUser(t, s, "u1", "owner", false)
	other := seedUser(t, s, "u2", "other", false)

	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", UserID: &owner.ID, Title: "T", Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	insert := func(id, role, content string, at int64) {
		t.Helper()
		if err := s.InsertMessage(ctx, &Message{
			ID: id, ConversationID: "c1", Role: role, Content: content, CreatedAt: at,
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", id, err)
		}
	}
	insert("m1", "user", "hello", 1)
	insert("a1", "assistant", "hi there", 2)
	insert("m2", "user", "second question", 3)
	insert("a2", "assistant", "second answer", 4)

	if err := s.EditUserMessage(ctx, "c1", "m1", owner.ID, "hello edited"); err != nil {
		t.Fatalf("EditUserMessage: %v", err)
	}
	// The edit must not disturb the other rows.
	if msgs, _ := s.ListMessages(ctx, "c1"); len(msgs) != 4 || msgs[0].Content != "hello edited" {
		t.Fatalf("after edit messages = %+v, want 4 with m1 edited", msgs)
	}

	// Editing an assistant message or another user's message is ErrNotFound.
	if err := s.EditUserMessage(ctx, "c1", "a1", owner.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("edit assistant = %v, want ErrNotFound", err)
	}
	if err := s.EditUserMessage(ctx, "c1", "m2", other.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("edit cross-user = %v, want ErrNotFound", err)
	}
	if err := s.EditUserMessage(ctx, "c1", "missing", owner.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("edit missing = %v, want ErrNotFound", err)
	}

	if err := s.TruncateAfter(ctx, "c1", "m1"); err != nil {
		t.Fatalf("TruncateAfter: %v", err)
	}
	msgs, err := s.ListMessages(ctx, "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "m1" {
		t.Fatalf("after truncate = %+v, want only m1", msgs)
	}

	// A missing anchor truncates nothing.
	if err := s.TruncateAfter(ctx, "c1", "ghost"); err != nil {
		t.Fatalf("TruncateAfter(ghost): %v", err)
	}
	if msgs, _ := s.ListMessages(ctx, "c1"); len(msgs) != 1 {
		t.Errorf("truncate on missing anchor removed rows: %+v", msgs)
	}
}

// TestBackupCreatesValidDatabase checks that Backup produces a readable SQLite
// file with the same data (the admin backup download, §19).
func TestBackupCreatesValidDatabase(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "owner", false)
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "c1", UserID: &u.ID, Title: "Back me up", Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	if err := s.InsertMessage(ctx, &Message{
		ID: "m1", ConversationID: "c1", Role: "user", Content: "hello", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if n != 1 {
		t.Errorf("backup messages = %d, want 1", n)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM conversations WHERE id = 'c1'`).Scan(&title); err != nil {
		t.Fatalf("query backup conversation: %v", err)
	}
	if title != "Back me up" {
		t.Errorf("backup title = %q", title)
	}
}

// seedSearchConv inserts a conversation with an optional title and one message,
// returning the conversation id.
func seedSearchConv(t *testing.T, s *Store, id, userID, title, content string, updatedAt int64) {
	t.Helper()
	uid := userID
	if err := s.InsertConversation(context.Background(), &Conversation{
		ID: id, UserID: &uid, Title: title, Model: "glm-5.3-flash",
		CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", id, err)
	}
	if content != "" {
		if err := s.InsertMessage(context.Background(), &Message{
			ID: id + "-m1", ConversationID: id, Role: "user", Content: content, CreatedAt: updatedAt,
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", id, err)
		}
	}
}

// TestSearchConversations covers title + content matching, case-insensitivity,
// user scoping, ordering, the result cap, and snippet generation.
func TestSearchConversations(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)

	seedSearchConv(t, s, "c-rust", alice.ID, "Rust notes",
		"A short intro. The "+strings.Repeat("x", 200)+" borrow checker rules apply here.", 100)
	seedSearchConv(t, s, "c-bake", alice.ID, "Baking Bread", "sourdough starter and proofing", 300)
	seedSearchConv(t, s, "c-bob", bob.ID, "Bob's chat", "borrow checker appears for bob too", 999)

	// Content match, case-insensitive, scoped to the owner.
	hits, err := s.SearchConversations(ctx, alice.ID, "BORROW", 50)
	if err != nil {
		t.Fatalf("SearchConversations(content): %v", err)
	}
	if len(hits) != 1 || hits[0].ConversationID != "c-rust" {
		t.Fatalf("content hits = %+v, want only c-rust", hits)
	}
	if hits[0].Title != "Rust notes" || hits[0].UpdatedAt != 100 {
		t.Errorf("hit meta = %+v", hits[0])
	}
	if !strings.Contains(hits[0].Snippet, "borrow checker") {
		t.Errorf("snippet = %q, want it to contain the match", hits[0].Snippet)
	}
	if n := len([]rune(hits[0].Snippet)); n > 122 {
		t.Errorf("snippet is %d runes, want <= 122", n)
	}

	// Title-only match: returned, with an empty snippet (no matching message).
	hits, err = s.SearchConversations(ctx, alice.ID, "baking", 50)
	if err != nil {
		t.Fatalf("SearchConversations(title): %v", err)
	}
	if len(hits) != 1 || hits[0].ConversationID != "c-bake" || hits[0].Snippet != "" {
		t.Fatalf("title hits = %+v, want c-bake with empty snippet", hits)
	}

	// A term that only exists in another user's chat does not leak.
	if hits, _ := s.SearchConversations(ctx, alice.ID, "for bob", 50); len(hits) != 0 {
		t.Errorf("cross-user leak: %+v", hits)
	}
	if hits, _ := s.SearchConversations(ctx, bob.ID, "borrow", 50); len(hits) != 1 || hits[0].ConversationID != "c-bob" {
		t.Errorf("bob hits = %+v, want c-bob", hits)
	}

	// No match, empty query, and a zero/negative cap.
	if hits, _ := s.SearchConversations(ctx, alice.ID, "nonexistent", 50); len(hits) != 0 {
		t.Errorf("no-match hits = %+v, want none", hits)
	}
	if hits, _ := s.SearchConversations(ctx, alice.ID, "   ", 50); len(hits) != 0 {
		t.Errorf("blank query hits = %+v, want none", hits)
	}
}

// TestSearchConversationsOrderAndLimit covers ordering (updated_at DESC) and
// the result cap.
func TestSearchConversationsOrderAndLimit(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "owner", false)

	seedSearchConv(t, s, "c-old", u.ID, "alpha old", "contains alpha", 100)
	seedSearchConv(t, s, "c-mid", u.ID, "alpha mid", "contains alpha", 200)
	seedSearchConv(t, s, "c-new", u.ID, "alpha new", "contains alpha", 300)

	hits, err := s.SearchConversations(ctx, u.ID, "alpha", 50)
	if err != nil {
		t.Fatalf("SearchConversations: %v", err)
	}
	got := []string{hits[0].ConversationID, hits[1].ConversationID, hits[2].ConversationID}
	want := []string{"c-new", "c-mid", "c-old"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	capped, err := s.SearchConversations(ctx, u.ID, "alpha", 2)
	if err != nil {
		t.Fatalf("SearchConversations(cap): %v", err)
	}
	if len(capped) != 2 || capped[0].ConversationID != "c-new" || capped[1].ConversationID != "c-mid" {
		t.Errorf("capped hits = %+v, want the two newest", capped)
	}
}

// TestPinnedRoundTripAndScoping covers the pinned flag (feature B): default,
// owner-guarded toggle, ordering pinned-first, and cross-user isolation.
func TestPinnedRoundTripAndScoping(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)

	seedSearchConv(t, s, "a1", alice.ID, "Recent", "x", 300)
	seedSearchConv(t, s, "a2", alice.ID, "Older", "x", 100)
	seedSearchConv(t, s, "b1", bob.ID, "Bob", "x", 999)

	// A fresh row is unpinned.
	if c, _ := s.GetConversation(ctx, "a1", alice.ID); c.Pinned {
		t.Error("new conversation is pinned by default")
	}

	// Pin the older alice chat; it must sort first.
	if err := s.SetConversationPinned(ctx, "a2", alice.ID, true); err != nil {
		t.Fatalf("SetConversationPinned: %v", err)
	}
	if c, _ := s.GetConversation(ctx, "a2", alice.ID); !c.Pinned {
		t.Error("pinned flag did not persist")
	}
	list, err := s.ListConversations(ctx, alice.ID)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 2 || list[0].ID != "a2" || !list[0].Pinned {
		t.Fatalf("pinned-first order = %+v, want a2 first", list)
	}

	// Cross-user toggle is indistinguishable from missing.
	if err := s.SetConversationPinned(ctx, "a2", bob.ID, true); err != ErrNotFound {
		t.Errorf("cross-user pin = %v, want ErrNotFound", err)
	}
	if c, _ := s.GetConversation(ctx, "a2", alice.ID); !c.Pinned {
		t.Error("cross-user attempt changed the row")
	}

	// InsertConversation round-trips an explicitly pinned row.
	pinned := true
	if err := s.InsertConversation(ctx, &Conversation{
		ID: "a3", UserID: &alice.ID, Model: "glm-5.3-flash", Pinned: pinned, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation(pinned): %v", err)
	}
	if c, _ := s.GetConversation(ctx, "a3", alice.ID); !c.Pinned {
		t.Error("InsertConversation did not persist pinned")
	}

	// Unpin and confirm the ordering returns to updated_at DESC.
	if err := s.SetConversationPinned(ctx, "a2", alice.ID, false); err != nil {
		t.Fatalf("SetConversationPinned(unpin): %v", err)
	}
	if c, _ := s.GetConversation(ctx, "a2", alice.ID); c.Pinned {
		t.Error("unpin did not persist")
	}
}

// TestUpdateConversationPersists guards the owner guard on UpdateConversation:
// GetConversation must load user_id so the WHERE clause matches (regression for
// a latent bug where the guard compared against NULL and every update no-op'd).
func TestUpdateConversationPersists(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	seedSearchConv(t, s, "c1", alice.ID, "Old", "x", 1)

	conv, err := s.GetConversation(ctx, "c1", alice.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.UserID == nil || *conv.UserID != alice.ID {
		t.Fatalf("GetConversation UserID = %v, want %q", conv.UserID, alice.ID)
	}
	conv.Title = "New title"
	conv.Model = "glm-5.2"
	conv.UpdatedAt = 99
	if err := s.UpdateConversation(ctx, &conv); err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}
	got, _ := s.GetConversation(ctx, "c1", alice.ID)
	if got.Title != "New title" || got.Model != "glm-5.2" || got.UpdatedAt != 99 {
		t.Errorf("update did not persist: %+v", got)
	}
}
