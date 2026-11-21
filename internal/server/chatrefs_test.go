package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"myogchat/internal/store"
	"myogchat/internal/tools"
)

// seedChatUserConversation inserts one conversation owned by userID.
func seedChatUserConversation(t *testing.T, st *store.Store, id, userID, title string, projectID *string, updatedAt int64) {
	t.Helper()
	uid := userID
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: id, UserID: &uid, Title: title, Model: "glm-5.3-flash",
		ProjectID: projectID, CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", id, err)
	}
}

// TestToolRegistryRegistersChatTools checks both chat tools ship whenever tools
// are enabled and disappear when tools are off.
func TestToolRegistryRegistersChatTools(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ctx := context.Background()

	reg := srv.toolRegistry(ctx, u)
	for _, name := range []string{"list_chats", "read_chat"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s not registered when tools are enabled", name)
		}
	}

	if err := srv.setToolsEnabled(ctx, false); err != nil {
		t.Fatalf("setToolsEnabled: %v", err)
	}
	reg = srv.toolRegistry(ctx, u)
	if reg.Len() != 0 {
		t.Errorf("registry len = %d with tools off, want 0", reg.Len())
	}
	if _, ok := reg.Get("read_chat"); ok {
		t.Error("read_chat registered with tools off")
	}
}

// TestChatSourceOwnerScoping proves another user's conversation is invisible:
// it never appears in listings or search and reads behave as not found.
func TestChatSourceOwnerScoping(t *testing.T) {
	_, st := newKeyTestServer(t)
	ctx := context.Background()
	alice := seedKeyUser(t, st, "u1", "alice", "")
	bob := seedKeyUser(t, st, "u2", "bob", "")

	seedChatUserConversation(t, st, "alice-chat", alice.ID, "Alice launcher", nil, 10)
	seedChatUserConversation(t, st, "bob-chat", bob.ID, "Bob secret launcher", nil, 20)
	if err := st.InsertMessage(ctx, &store.Message{
		ID: "bob-m1", ConversationID: "bob-chat", Role: "user", Content: "secret launcher code", CreatedAt: 20,
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	src := chatSource{st: st, userID: alice.ID}
	chats, err := src.Chats(ctx)
	if err != nil {
		t.Fatalf("Chats: %v", err)
	}
	for _, c := range chats {
		if c.ID == "bob-chat" {
			t.Fatal("another user's chat leaked into Chats")
		}
	}
	ids, err := src.SearchIDs(ctx, "launcher")
	if err != nil {
		t.Fatalf("SearchIDs: %v", err)
	}
	for _, id := range ids {
		if id == "bob-chat" {
			t.Fatal("another user's chat leaked into search")
		}
	}
	if _, err := src.Messages(ctx, "bob-chat"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Messages(foreign) = %v, want ErrNotFound", err)
	}

	read, err := tools.NewReadChatTool(src).Run(ctx, json.RawMessage(`{"id":"bob-chat"}`))
	if err == nil {
		t.Fatalf("read_chat on a foreign id returned %q, want error", read)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks foreign content: %v", err)
	}
	if _, err := tools.NewReadChatTool(src).Run(ctx, json.RawMessage(`{"id":"alice-chat"}`)); err != nil {
		t.Errorf("read_chat on own chat: %v", err)
	}
}

// TestChatSourceMapsStoredActivity checks the store-to-tool mapping keeps
// project names, thinking, tool activity and file metadata.
func TestChatSourceMapsStoredActivity(t *testing.T) {
	_, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u1", "alice", "")
	uid := u.ID
	if err := st.InsertProject(ctx, &store.Project{
		ID: "p1", UserID: &uid, Name: "Launcher", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	project := "p1"
	seedChatUserConversation(t, st, "c1", u.ID, "Launcher plan", &project, 10)
	reasoning := "checked the logs"
	if err := st.InsertMessage(ctx, &store.Message{
		ID: "m1", ConversationID: "c1", Role: "assistant", Content: "done",
		Reasoning: &reasoning, CreatedAt: 11,
		Tools: []store.ToolActivity{{
			Name: "run_bash", Status: "done", Args: `{"cmd":"ls"}`,
			Output: "a.txt", DurationMs: 42,
		}},
		Attachments: []store.AttachedFile{{ID: "f1", Name: "spec.pdf", Mime: "application/pdf", Size: 7}},
		Artifacts:   []store.ArtifactRef{{ID: "a1", Name: "out.csv", Mime: "text/csv", Size: 9}},
		Images:      []store.GeneratedImage{{ID: "i1", Prompt: "a sunset", Mime: "image/png", Size: 11}},
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	src := chatSource{st: st, userID: u.ID}
	out, err := tools.NewReadChatTool(src).Run(ctx, json.RawMessage(`{"title":"launcher plan"}`))
	if err != nil {
		t.Fatalf("read_chat: %v", err)
	}
	for _, want := range []string{
		"Project: Launcher",
		"  Thinking: checked the logs",
		"  Tool: run_bash · done · 42ms",
		`    args: {"cmd":"ls"}`,
		"    output: a.txt",
		"attached: spec.pdf (application/pdf, 7 bytes)",
		"artifact: out.csv (text/csv, 9 bytes)",
		"image: a sunset (image/png, 11 bytes)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("read_chat output missing %q:\n%s", want, out)
		}
	}

	list, err := tools.NewListChatsTool(src).Run(ctx, json.RawMessage(`{"project":"launcher"}`))
	if err != nil {
		t.Fatalf("list_chats: %v", err)
	}
	if !strings.Contains(list, `"project":"Launcher"`) || !strings.Contains(list, `"message_count":1`) {
		t.Errorf("list_chats = %s", list)
	}
}
