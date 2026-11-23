package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"myogchat/internal/sandbox"
	"myogchat/internal/store"
)

// forkChat seeds an owned conversation with four alternating messages and
// returns them in transcript order. Message m2 carries every persisted field a
// copy must carry.
func forkChat(t *testing.T, st *store.Store, owner store.User, convID, title string, agent bool) []store.Message {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertConversation(ctx, &store.Conversation{
		ID: convID, UserID: &owner.ID, Title: title, Model: "glm-5.3-flash",
		Effort: strptr("high"), AgentMode: agent, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation(%s): %v", convID, err)
	}
	msgs := []store.Message{
		{ID: convID + "-m1", Role: "user", Content: "one", CreatedAt: 10},
		{ID: convID + "-m2", Role: "assistant", Content: "two",
			Reasoning: strptr("because"), Model: strptr("glm-5.3-flash"),
			PromptTokens: int64ptr(11), CompletionTokens: int64ptr(22),
			CachedTokens: int64ptr(3), CostMicros: int64ptr(44),
			ReasoningTokens: int64ptr(5), DurationMs: int64ptr(66),
			Tools:     []store.ToolActivity{{Name: "web_search", Status: "done", Summary: "1 result"}},
			Images:    []store.GeneratedImage{{ID: "img-1", Prompt: "a cat", Mime: "image/png", Width: 2, Height: 2, Size: 9}},
			Artifacts: []store.ArtifactRef{{ID: "art-1", Name: "out.csv", Mime: "text/csv", Size: 6}},
			CreatedAt: 20},
		{ID: convID + "-m3", Role: "user", Content: "three",
			Attachments: []store.AttachedFile{{ID: "att-1", Name: "notes.txt", Mime: "text/plain", Size: 5}},
			CreatedAt:   30},
		{ID: convID + "-m4", Role: "assistant", Content: "four", CreatedAt: 40},
	}
	for i := range msgs {
		msgs[i].ConversationID = convID
		if err := st.InsertMessage(ctx, &msgs[i]); err != nil {
			t.Fatalf("InsertMessage(%s): %v", msgs[i].ID, err)
		}
	}
	return msgs
}

// forkResponse is the documented 201 body.
type forkResponse struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	MessageCount int    `json:"message_count"`
}

// doFork POSTs /fork for messageID and returns the recorder.
func doFork(t *testing.T, h http.Handler, cookie *http.Cookie, convID, messageID string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, cookie, http.MethodPost, "/api/conversations/"+convID+"/fork",
		map[string]string{"message_id": messageID})
}

// forkMessages loads a conversation's transcript through the API.
func forkMessages(t *testing.T, h http.Handler, cookie *http.Cookie, convID string) []store.Message {
	t.Helper()
	rr := doJSON(t, h, cookie, http.MethodGet, "/api/conversations/"+convID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d (body %s)", convID, rr.Code, rr.Body.String())
	}
	var out struct {
		Messages []store.Message `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	return out.Messages
}

// TestForkConversationMiddle forks in the middle of a four-message chat: the
// head of the transcript (inclusive) is copied, the tail is not, and the source
// is untouched.
func TestForkConversationMiddle(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	alice := store.User{ID: "u-alice"}
	src := forkChat(t, st, alice, "c1", "Deep work", false)

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "c1", "c1-m3")
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork status = %d, want 201 (body %s)", rr.Code, rr.Body.String())
	}
	var got forkResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode fork: %v", err)
	}
	if got.ID == "" || got.ID == "c1" {
		t.Fatalf("fork id = %q, want a fresh id", got.ID)
	}
	if got.Title != "Deep work — fork" {
		t.Errorf("title = %q, want %q", got.Title, "Deep work — fork")
	}
	if got.MessageCount != 3 {
		t.Fatalf("message_count = %d, want 3", got.MessageCount)
	}

	forked := forkMessages(t, h, cookie, got.ID)
	if len(forked) != 3 {
		t.Fatalf("fork has %d messages, want 3", len(forked))
	}
	for i := range forked {
		if forked[i].Role != src[i].Role || forked[i].Content != src[i].Content {
			t.Errorf("message %d = %s/%q, want %s/%q", i, forked[i].Role, forked[i].Content, src[i].Role, src[i].Content)
		}
		if forked[i].ID == src[i].ID {
			t.Errorf("message %d kept its source id %q", i, forked[i].ID)
		}
		if forked[i].CreatedAt != src[i].CreatedAt {
			t.Errorf("message %d created_at = %d, want %d", i, forked[i].CreatedAt, src[i].CreatedAt)
		}
		if forked[i].ConversationID != got.ID {
			t.Errorf("message %d conversation_id = %q, want %q", i, forked[i].ConversationID, got.ID)
		}
	}

	// The fork keeps model, effort and project scope of the source.
	conv, err := st.GetConversation(context.Background(), got.ID, alice.ID)
	if err != nil {
		t.Fatalf("GetConversation(fork): %v", err)
	}
	if conv.Model != "glm-5.3-flash" || conv.Effort == nil || *conv.Effort != "high" {
		t.Errorf("fork model/effort = %q/%v, want glm-5.3-flash/high", conv.Model, conv.Effort)
	}

	// The source is untouched.
	after := forkMessages(t, h, cookie, "c1")
	if len(after) != 4 {
		t.Fatalf("source has %d messages after fork, want 4", len(after))
	}
	for i := range after {
		if after[i].ID != src[i].ID || after[i].Content != src[i].Content {
			t.Errorf("source message %d changed: %+v", i, after[i])
		}
	}
}

// TestForkConversationFirst copies exactly one message.
func TestForkConversationFirst(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	forkChat(t, st, store.User{ID: "u-alice"}, "c1", "", false)

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "c1", "c1-m1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork status = %d (body %s)", rr.Code, rr.Body.String())
	}
	var got forkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.MessageCount != 1 {
		t.Fatalf("message_count = %d, want 1", got.MessageCount)
	}
	forked := forkMessages(t, h, cookie, got.ID)
	if len(forked) != 1 || forked[0].Content != "one" {
		t.Fatalf("fork messages = %+v, want just %q", forked, "one")
	}
	if got.Title != "Untitled — fork" {
		t.Errorf("untitled fork title = %q, want %q", got.Title, "Untitled — fork")
	}
}

// TestForkConversationLast copies the whole transcript.
func TestForkConversationLast(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	forkChat(t, st, store.User{ID: "u-alice"}, "c1", "Whole", false)

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "c1", "c1-m4")
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork status = %d (body %s)", rr.Code, rr.Body.String())
	}
	var got forkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.MessageCount != 4 {
		t.Fatalf("message_count = %d, want 4", got.MessageCount)
	}
}

// TestForkCopiesMessageFields checks every persisted field travels with the
// copy, including the attachment/artifact/image references.
func TestForkCopiesMessageFields(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	forkChat(t, st, store.User{ID: "u-alice"}, "c1", "Fields", false)

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "c1", "c1-m4")
	var got forkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)

	forked := forkMessages(t, h, cookie, got.ID)
	if len(forked) != 4 {
		t.Fatalf("fork has %d messages, want 4", len(forked))
	}
	m := forked[1]
	if m.Reasoning == nil || *m.Reasoning != "because" ||
		m.Model == nil || *m.Model != "glm-5.3-flash" ||
		*m.PromptTokens != 11 || *m.CompletionTokens != 22 || *m.CachedTokens != 3 ||
		*m.CostMicros != 44 || *m.ReasoningTokens != 5 || *m.DurationMs != 66 {
		t.Errorf("usage fields not copied: %+v", m)
	}
	if len(m.Tools) != 1 || m.Tools[0].Name != "web_search" {
		t.Errorf("tools not copied: %+v", m.Tools)
	}
	if len(m.Images) != 1 || m.Images[0].ID != "img-1" {
		t.Errorf("images not copied: %+v", m.Images)
	}
	if len(m.Artifacts) != 1 || m.Artifacts[0].ID != "art-1" {
		t.Errorf("artifacts not copied: %+v", m.Artifacts)
	}
	if len(forked[2].Attachments) != 1 || forked[2].Attachments[0].ID != "att-1" {
		t.Errorf("attachments not copied: %+v", forked[2].Attachments)
	}
}

// TestForkConversationUnknownMessageIs404 covers a message id that is missing,
// or that lives in a different conversation of the same user.
func TestForkConversationUnknownMessageIs404(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	alice := store.User{ID: "u-alice"}
	forkChat(t, st, alice, "c1", "One", false)
	forkChat(t, st, alice, "c2", "Two", false)

	h := srv.Handler()
	cookie := login(t, h, "alice")
	for _, id := range []string{"nope", "", "c2-m1"} {
		rr := doFork(t, h, cookie, "c1", id)
		if id == "" {
			if rr.Code != http.StatusBadRequest {
				t.Errorf("empty message_id = %d, want 400", rr.Code)
			}
			continue
		}
		if rr.Code != http.StatusNotFound {
			t.Errorf("fork at %q = %d, want 404 (body %s)", id, rr.Code, rr.Body.String())
		}
	}
}

// TestForkConversationOtherUserIs404 checks another user's conversation is
// indistinguishable from a missing one.
func TestForkConversationOtherUserIs404(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	seedActiveUser(t, st, "u-bob", "bob", false)
	forkChat(t, st, store.User{ID: "u-alice"}, "c1", "Alice", false)

	h := srv.Handler()
	bob := login(t, h, "bob")
	if rr := doFork(t, h, bob, "c1", "c1-m1"); rr.Code != http.StatusNotFound {
		t.Errorf("cross-user fork = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
	// Nothing was created for bob.
	rr := doJSON(t, h, bob, http.MethodGet, "/api/conversations", nil)
	var list []store.Conversation
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Errorf("bob has %d conversations, want 0", len(list))
	}
}

// TestForkConversationTooLargeIs413 refuses a copy above the message cap.
func TestForkConversationTooLargeIs413(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	alice := store.User{ID: "u-alice"}
	ctx := context.Background()
	if err := st.InsertConversation(ctx, &store.Conversation{
		ID: "big", UserID: &alice.ID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	for i := 0; i <= forkMessageLimit; i++ {
		if err := st.InsertMessage(ctx, &store.Message{
			ID: "big-" + strconv.Itoa(i), ConversationID: "big", Role: "user",
			Content: "x", CreatedAt: int64(i),
		}); err != nil {
			t.Fatalf("InsertMessage(%d): %v", i, err)
		}
	}

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "big", "big-"+strconv.Itoa(forkMessageLimit))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized fork = %d, want 413 (body %s)", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); body != `{"error":"conversation too large to fork"}`+"\n" {
		t.Errorf("413 body = %q", body)
	}
	// Nothing was written.
	list := forkMessages(t, h, cookie, "big")
	if len(list) != forkMessageLimit+1 {
		t.Errorf("source has %d messages, want %d", len(list), forkMessageLimit+1)
	}
}

// TestForkAgentConversationKeepsModeWithoutWorkspace forks an agent chat: the
// copy stays agent-mode and gets no workspace directory.
func TestForkAgentConversationKeepsModeWithoutWorkspace(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	root := t.TempDir()
	srv.sbx = sandbox.NewRunner(sandbox.Config{WorkDir: root})
	forkChat(t, st, store.User{ID: "u-alice"}, "c1", "Agent run", true)

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "c1", "c1-m2")
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork status = %d (body %s)", rr.Code, rr.Body.String())
	}
	var got forkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)

	conv, err := st.GetConversation(context.Background(), got.ID, "u-alice")
	if err != nil {
		t.Fatalf("GetConversation(fork): %v", err)
	}
	if !conv.AgentMode {
		t.Error("fork lost agent_mode")
	}
	if dir := srv.agentWorkspaceDir("u-alice", got.ID); dir != "" {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("fork created a workspace at %s: %v", dir, err)
		}
	}
	// The source workspace is untouched (it had none either way).
	if _, err := os.Stat(filepath.Join(root, sandbox.AgentWorkspaceDirName)); !os.IsNotExist(err) {
		t.Errorf("fork touched the workspace root: %v", err)
	}
}

// TestForkIsIndependentOfSource proves edits and deletes on either side leave
// the other alone.
func TestForkIsIndependentOfSource(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	forkChat(t, st, store.User{ID: "u-alice"}, "c1", "Split", false)

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "c1", "c1-m4")
	var got forkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	forked := forkMessages(t, h, cookie, got.ID)

	// Edit in the fork (which truncates its own tail): the source is unchanged.
	rr = doJSON(t, h, cookie, http.MethodPost,
		"/api/conversations/"+got.ID+"/messages/"+forked[0].ID+"/edit",
		map[string]string{"content": "edited in fork"})
	if rr.Code != http.StatusOK {
		t.Fatalf("edit in fork = %d (body %s)", rr.Code, rr.Body.String())
	}
	src := forkMessages(t, h, cookie, "c1")
	if len(src) != 4 || src[0].Content != "one" {
		t.Fatalf("source changed by a fork edit: %+v", src)
	}

	// Edit in the source: the already-truncated fork keeps what it has.
	rr = doJSON(t, h, cookie, http.MethodPost, "/api/conversations/c1/messages/c1-m1/edit",
		map[string]string{"content": "edited in source"})
	if rr.Code != http.StatusOK {
		t.Fatalf("edit in source = %d (body %s)", rr.Code, rr.Body.String())
	}
	after := forkMessages(t, h, cookie, got.ID)
	if len(after) != 1 || after[0].Content != "edited in fork" {
		t.Fatalf("fork changed by a source edit: %+v", after)
	}

	// Deleting the source conversation leaves the fork intact.
	if rr := doJSON(t, h, cookie, http.MethodDelete, "/api/conversations/c1", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete source = %d (body %s)", rr.Code, rr.Body.String())
	}
	if kept := forkMessages(t, h, cookie, got.ID); len(kept) != 1 {
		t.Errorf("fork lost messages with the source: %+v", kept)
	}
}

// TestForkSharesAttachmentAndArtifactBytes checks a fork copies the references
// (no duplicated blobs) and can still download the same artifact.
func TestForkSharesAttachmentAndArtifactBytes(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	forkChat(t, st, store.User{ID: "u-alice"}, "c1", "Refs", false)
	if err := st.InsertAttachment(context.Background(), &store.Attachment{
		ID: "att-1", UserID: "u-alice", Name: "notes.txt", Mime: "text/plain",
		Size: 5, Text: "hello", Status: "ready", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}
	insertArtifact(t, st, store.Artifact{
		ID: "art-1", UserID: "u-alice", ConversationID: strptr("c1"), Name: "out.csv",
		Mime: "text/csv", Size: 6, Data: []byte("a,b\n1\n"), CreatedAt: 1,
	})

	h := srv.Handler()
	cookie := login(t, h, "alice")
	rr := doFork(t, h, cookie, "c1", "c1-m4")
	var got forkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)

	// No new attachment or artifact rows: the fork points at the same bytes.
	if rr := doJSON(t, h, cookie, http.MethodGet, "/api/artifacts/art-1", nil); rr.Code != http.StatusOK ||
		rr.Body.String() != "a,b\n1\n" {
		t.Fatalf("artifact after fork = %d %q", rr.Code, rr.Body.String())
	}
	// The reference still resolves for the owner after the fork.
	forked := forkMessages(t, h, cookie, got.ID)
	if len(forked[1].Artifacts) != 1 || forked[1].Artifacts[0].ID != "art-1" {
		t.Fatalf("fork artifact refs = %+v", forked[1].Artifacts)
	}
}

// TestForkRouteRegistered checks the route exists and sits behind auth.
func TestForkRouteRegistered(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	rr := doJSON(t, h, nil, http.MethodPost, "/api/conversations/c1/fork",
		map[string]string{"message_id": "m1"})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated fork = %d, want 401", rr.Code)
	}
}
