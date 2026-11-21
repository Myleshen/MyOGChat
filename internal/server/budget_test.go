package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// TestTurnSendsOutputBudget proves every send hands the upstream a budget, so
// the dialects that require one (anthropic, google) and the one that takes one
// (responses) always get turnMaxTokens instead of a dialect default. The chat
// dialect ignores MaxTokens; internal/upstream asserts it sends no cap.
func TestTurnSendsOutputBudget(t *testing.T) {
	for _, id := range []string{"glm-5.3-flash", "gpt-6-luna", "claude-haiku-5-5", "gemini-4-argon"} {
		t.Run(id, func(t *testing.T) {
			srv, st := newJobTestServer(t)
			u := seedKeyUser(t, st, "u1", "alice", "key")
			if err := st.InsertConversation(context.Background(), &store.Conversation{
				ID: "c1", UserID: &u.ID, Title: "t", Model: id, CreatedAt: 1, UpdatedAt: 1,
			}); err != nil {
				t.Fatalf("InsertConversation: %v", err)
			}

			params := make(chan upstream.Params, 1)
			srv.streamAgentFn = func(_ context.Context, p upstream.Params, _ upstream.ExecuteTool, _ upstream.AgentOptions) <-chan upstream.Event {
				params <- p
				out := make(chan upstream.Event, 2)
				out <- upstream.Event{Kind: upstream.EventText, Delta: "ok"}
				out <- upstream.Event{Kind: upstream.EventUsage, Usage: &upstream.Usage{PromptTokens: 1, CompletionTokens: 1}}
				close(out)
				return out
			}

			if rec := postMessage(srv, u, "c1", "hi"); rec.Code != http.StatusOK {
				t.Fatalf("send status = %d, body %s", rec.Code, rec.Body.String())
			}
			select {
			case p := <-params:
				if p.MaxTokens != turnMaxTokens {
					t.Errorf("%s: MaxTokens = %d, want %d", id, p.MaxTokens, turnMaxTokens)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upstream call never happened")
			}
		})
	}
}
