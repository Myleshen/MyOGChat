package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// TestAutoTitleRequestShape pins the one-shot title call: the caller's key and
// conversation id, the cheap Go model, a small token budget, and at most 400
// chars of the first user message.
func TestAutoTitleRequestShape(t *testing.T) {
	srv, _ := newJobTestServer(t)
	var gotKey, gotSession, gotModel string
	var gotMsgs []upstream.Message
	var gotMax int
	srv.chatOnceFn = func(_ context.Context, apiKey, sessionID, model string, msgs []upstream.Message, maxTokens int) (string, error) {
		gotKey, gotSession, gotModel, gotMsgs, gotMax = apiKey, sessionID, model, msgs, maxTokens
		return `  "Quarterly budget review"  `, nil
	}

	title := srv.autoTitle(context.Background(), "conv-1", "user-key", strings.Repeat("a", 1000))

	if title != "Quarterly budget review" {
		t.Fatalf("title = %q, want sanitized title", title)
	}
	if gotModel != "mimo-v2.6-flash" {
		t.Fatalf("model = %q, want mimo-v2.6-flash", gotModel)
	}
	if gotKey != "user-key" || gotSession != "conv-1" {
		t.Fatalf("key/session = %q/%q, want the caller's", gotKey, gotSession)
	}
	if gotMax != 24 {
		t.Fatalf("max_tokens = %d, want 24", gotMax)
	}
	if len(gotMsgs) != 1 || gotMsgs[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", gotMsgs)
	}
	if _, body, ok := strings.Cut(gotMsgs[0].Content, "User: "); !ok || body != strings.Repeat("a", 400) {
		t.Fatalf("user text sent = %q..., want exactly 400 chars", truncateRunes(body, 12))
	}
}

// TestAutoTitleFallsBack covers every failure mode: each leaves the truncated
// prompt as the title and returns no error.
func TestAutoTitleFallsBack(t *testing.T) {
	srv, _ := newJobTestServer(t)
	firstUser := "how do I rotate the tyres on my van safely"
	want := "how do I rotate the tyres on my van safe"

	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		return "", errors.New("upstream HTTP 429: FreeUsageLimitError")
	}
	if got := srv.autoTitle(context.Background(), "c", "k", firstUser); got != want {
		t.Fatalf("on error title = %q, want fallback %q", got, want)
	}

	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		return "   ", nil
	}
	if got := srv.autoTitle(context.Background(), "c", "k", firstUser); got != want {
		t.Fatalf("on empty reply title = %q, want fallback %q", got, want)
	}

	srv.chatOnceFn = nil
	if got := srv.autoTitle(context.Background(), "c", "", firstUser); got != want {
		t.Fatalf("without a client title = %q, want fallback %q", got, want)
	}
}

// TestTitleFailureKeepsReplyGreen runs a whole reply with a failing title call:
// the truncated title is persisted and the job still finishes as done.
func TestTitleFailureKeepsReplyGreen(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: "c1", UserID: &u.ID, Title: "", Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 3, CompletionTokens: 2}},
	)
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		return "", errors.New("429 FreeUsageLimitError")
	}

	rec := postMessage(srv, u, "c1", "how do I rotate the tyres on my van safely")
	if rec.Code != 200 {
		t.Fatalf("send status = %d", rec.Code)
	}
	j := srv.hub().get("c1")
	if j == nil || j.snapshot().Status != jobDone {
		t.Fatalf("job = %+v, want done", j.snapshot())
	}
	conv, err := st.GetConversation(context.Background(), "c1", u.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if want := "how do I rotate the tyres on my van safe"; conv.Title != want {
		t.Fatalf("title = %q, want fallback %q", conv.Title, want)
	}
}
