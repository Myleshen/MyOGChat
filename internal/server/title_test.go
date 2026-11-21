package server

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// TestAutoTitleRequestShape pins the one-shot title call: the caller's key and
// conversation id, the non-reasoning Go model, a small token budget, and at
// most 400 chars of the first user message.
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
	if gotModel != "hy3" {
		t.Fatalf("model = %q, want hy3", gotModel)
	}
	if gotKey != "user-key" || gotSession != "conv-1" {
		t.Fatalf("key/session = %q/%q, want the caller's", gotKey, gotSession)
	}
	if gotMax != titleMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", gotMax, titleMaxTokens)
	}
	if len(gotMsgs) != 1 || gotMsgs[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", gotMsgs)
	}
	if _, body, ok := strings.Cut(gotMsgs[0].Content, "User: "); !ok || body != strings.Repeat("a", 400) {
		t.Fatalf("user text sent = %q..., want exactly 400 chars", truncateRunes(body, 12))
	}
}

// TestAutoTitleBudgetSurvivesReasoning injects the old behaviour through the
// fake upstream: 32 tokens burned thinking before the answer, and text only
// when the budget has room left (that is how mimo-v2.6-flash returned empty
// titles). The title model is non-reasoning and the budget has headroom, so the
// title lands even against a model that burns that much.
func TestAutoTitleBudgetSurvivesReasoning(t *testing.T) {
	const reasoningTokens = 32
	reasoningModel := func(_ context.Context, _, _, _ string, _ []upstream.Message, maxTokens int) (string, error) {
		if maxTokens <= reasoningTokens {
			return "", errors.New("empty completion")
		}
		return "Van tyre rotation guide", nil
	}

	srv, _ := newJobTestServer(t)
	srv.chatOnceFn = reasoningModel
	if got := srv.autoTitle(context.Background(), "c", "k", "how do I rotate the tyres on my van safely"); got != "Van tyre rotation guide" {
		t.Fatalf("title = %q, want the model title, not the fallback", got)
	}

	// The fake really does starve: the old 24-token budget still returns empty,
	// so the assertion above is about the headroom, not a fake that never fails.
	if _, err := reasoningModel(context.Background(), "k", "c", titleModel, nil, 24); err == nil {
		t.Fatal("24-token budget did not starve the injected reasoning burn")
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
	// The title lands on its own goroutine, so wait for it rather than assume it
	// beat `done`.
	want := "how do I rotate the tyres on my van safe"
	waitFor(t, func() bool {
		conv, err := st.GetConversation(context.Background(), "c1", u.ID)
		return err == nil && conv.Title == want
	})
}

// streamAfter emits text and usage only once ready reports true (or the job is
// cancelled), so a test can pin what the reply does after the title has landed.
func streamAfter(ready func() bool) func(context.Context, upstream.Params, upstream.ExecuteTool, upstream.AgentOptions) <-chan upstream.Event {
	return func(c context.Context, _ upstream.Params, _ upstream.ExecuteTool, _ upstream.AgentOptions) <-chan upstream.Event {
		out := make(chan upstream.Event, 2)
		go func() {
			defer close(out)
			for !ready() {
				select {
				case <-time.After(2 * time.Millisecond):
				case <-c.Done():
					return
				}
			}
			out <- upstream.Event{Kind: upstream.EventText, Delta: "hello"}
			out <- upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 3, CompletionTokens: 2}}
		}()
		return out
	}
}

// seedEmptyTitleConv inserts an untitled conversation owned by u.
func seedEmptyTitleConv(t *testing.T, st *store.Store, u store.User, id string) {
	t.Helper()
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: id, UserID: &u.ID, Title: "", Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
}

// TestAutoTitleRunsParallelWithReply runs a reply that only finishes after the
// title has been stored: `done` must carry it, and the model is called once.
func TestAutoTitleRunsParallelWithReply(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	seedEmptyTitleConv(t, st, u, "c1")

	var calls atomic.Int32
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		calls.Add(1)
		return "Van tyre rotation guide", nil
	}
	srv.streamAgentFn = streamAfter(func() bool {
		j := srv.hub().get("c1")
		return j != nil && len(j.titleCh) == 1
	})

	rec := postMessage(srv, u, "c1", "how do I rotate the tyres on my van safely")
	if rec.Code != 200 {
		t.Fatalf("send status = %d", rec.Code)
	}
	if got := srv.hub().get("c1").snapshot().Title; got != "Van tyre rotation guide" {
		t.Fatalf("done title = %q, want the generated title", got)
	}
	conv, err := st.GetConversation(context.Background(), "c1", u.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.Title != "Van tyre rotation guide" {
		t.Fatalf("stored title = %q, want the generated title", conv.Title)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("title calls = %d, want exactly one", n)
	}
}

// TestAutoTitleNeverDelaysDone blocks the title call for the whole reply: `done`
// must still fire, with no title, and the title must be persisted afterwards.
func TestAutoTitleNeverDelaysDone(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	seedEmptyTitleConv(t, st, u, "c1")

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		started <- struct{}{}
		<-release
		return "Warehouse shelving suppliers", nil
	}
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 3, CompletionTokens: 2}},
	)

	reply := postMessageAsync(srv, u, "c1", "compare two shelving suppliers")
	select {
	case rec := <-reply:
		if rec.Code != 200 {
			t.Fatalf("send status = %d", rec.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("done did not fire while the title call was still blocked")
	}
	select {
	case <-started:
	default:
		t.Fatal("title call never started")
	}
	if snap := srv.hub().get("c1").snapshot(); snap.Status != jobDone || snap.Title != "" {
		t.Fatalf("job = %+v, want done without a title", snap)
	}
	if conv, err := st.GetConversation(context.Background(), "c1", u.ID); err != nil || conv.Title != "" {
		t.Fatalf("stored title = %q (err %v), want none yet", conv.Title, err)
	}

	close(release)
	waitFor(t, func() bool {
		conv, err := st.GetConversation(context.Background(), "c1", u.ID)
		return err == nil && conv.Title == "Warehouse shelving suppliers"
	})
}

// TestAutoTitleLateErrorFallsBack keeps the safety net: a title call that fails
// after `done` still leaves the truncated prompt, and nothing re-writes it.
func TestAutoTitleLateErrorFallsBack(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	seedEmptyTitleConv(t, st, u, "c1")

	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		return "", errors.New("context deadline exceeded")
	}
	srv.streamAgentFn = streamAfter(func() bool {
		j := srv.hub().get("c1")
		return j != nil && len(j.titleCh) == 1
	})

	rec := postMessage(srv, u, "c1", "how do I rotate the tyres on my van safely")
	if rec.Code != 200 {
		t.Fatalf("send status = %d", rec.Code)
	}
	want := "how do I rotate the tyres on my van safe"
	if got := srv.hub().get("c1").snapshot().Title; got != want {
		t.Fatalf("done title = %q, want fallback %q", got, want)
	}
	if conv, err := st.GetConversation(context.Background(), "c1", u.ID); err != nil || conv.Title != want {
		t.Fatalf("stored title = %q (err %v), want fallback %q", conv.Title, err, want)
	}
}

// TestAutoTitleNeverOverwritesARename: a title that resolves after the user
// renamed the conversation is dropped, and `done` carries no title for it.
func TestAutoTitleNeverOverwritesARename(t *testing.T) {
	srv, st := newJobTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "key")
	seedEmptyTitleConv(t, st, u, "c1")

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		started <- struct{}{}
		<-release
		return "Warehouse shelving suppliers", nil
	}
	srv.streamAgentFn = fixedStream(
		upstream.Event{Kind: upstream.EventText, Delta: "hello"},
		upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 3, CompletionTokens: 2}},
	)

	reply := postMessageAsync(srv, u, "c1", "compare two shelving suppliers")
	select {
	case <-reply:
	case <-time.After(2 * time.Second):
		t.Fatal("done did not fire while the title call was still blocked")
	}
	<-started
	if err := st.SetTitle(context.Background(), "c1", "My title"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	close(release)
	waitFor(t, func() bool {
		conv, err := st.GetConversation(context.Background(), "c1", u.ID)
		return err == nil && conv.Title == "My title"
	})
}
