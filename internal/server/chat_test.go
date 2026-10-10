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
	"myogchat/internal/upstream"
)

// TestMergeToolEventCarriesArgsOutput verifies Phase 4.1 keeps the tool's
// arguments (captured when the call starts) and its output/duration.
func TestMergeToolEventCarriesArgsOutput(t *testing.T) {
	var list []store.ToolActivity
	list = mergeToolEvent(list, &upstream.ToolActivity{
		Name: "web_search", Status: upstream.ToolRunning, Args: `{"query":"go"}`,
	})
	list = mergeToolEvent(list, &upstream.ToolActivity{
		Name: "web_search", Status: upstream.ToolDone, Summary: "3 results",
		Output: "OUT", DurationMs: 5,
	})
	if len(list) != 1 {
		t.Fatalf("want one entry, got %+v", list)
	}
	if list[0].Args != `{"query":"go"}` || list[0].Output != "OUT" || list[0].DurationMs != 5 {
		t.Errorf("args/output/duration not carried: %+v", list[0])
	}
}

// TestCappedToolTruncates verifies oversized inputs/outputs are cut on the caps.
func TestCappedToolTruncates(t *testing.T) {
	entry := cappedTool(&upstream.ToolActivity{
		Name: "fetch_url", Status: upstream.ToolDone,
		Args:   strings.Repeat("a", maxToolArgsBytes+500),
		Output: strings.Repeat("b", maxToolOutputBytes+500),
		Error:  strings.Repeat("e", maxToolErrorBytes+500),
	})
	if !entry.Truncated {
		t.Error("expected Truncated=true")
	}
	if len(entry.Args) > maxToolArgsBytes+len("\n… [truncated]") {
		t.Errorf("args not truncated: len=%d", len(entry.Args))
	}
	if len(entry.Output) > maxToolOutputBytes+len("\n… [truncated]") {
		t.Errorf("output not truncated: len=%d", len(entry.Output))
	}
	if len(entry.Error) > maxToolErrorBytes {
		t.Errorf("error not truncated: len=%d", len(entry.Error))
	}
	// Small values pass through untouched.
	small := cappedTool(&upstream.ToolActivity{Name: "x", Status: upstream.ToolDone, Output: "ok"})
	if small.Truncated || small.Output != "ok" {
		t.Errorf("small output should not be truncated: %+v", small)
	}
}

func TestMergeToolEvent(t *testing.T) {
	var list []store.ToolActivity

	// running → done collapses into one entry with the summary.
	list = mergeToolEvent(list, &upstream.ToolActivity{Name: "web_search", Status: upstream.ToolRunning})
	list = mergeToolEvent(list, &upstream.ToolActivity{Name: "web_search", Status: upstream.ToolDone, Summary: "3 results"})
	if len(list) != 1 || list[0].Status != upstream.ToolDone || list[0].Summary != "3 results" {
		t.Fatalf("running→done = %+v, want one done entry", list)
	}

	// running → error merges the same way.
	list = mergeToolEvent(list, &upstream.ToolActivity{Name: "run_bash", Status: upstream.ToolRunning})
	list = mergeToolEvent(list, &upstream.ToolActivity{Name: "run_bash", Status: upstream.ToolError, Summary: "boom"})
	if len(list) != 2 || list[1].Name != "run_bash" || list[1].Status != upstream.ToolError || list[1].Summary != "boom" {
		t.Fatalf("running→error = %+v", list)
	}

	// An unmatched terminal event appends.
	list = mergeToolEvent(list, &upstream.ToolActivity{Name: "fetch_url", Status: upstream.ToolDone, Summary: "ok"})
	if len(list) != 3 || list[2].Name != "fetch_url" {
		t.Fatalf("unmatched terminal = %+v, want appended", list)
	}

	// Same tool called again: a new running entry closes on its own terminal.
	list = mergeToolEvent(list, &upstream.ToolActivity{Name: "web_search", Status: upstream.ToolRunning})
	list = mergeToolEvent(list, &upstream.ToolActivity{Name: "web_search", Status: upstream.ToolDone, Summary: "again"})
	if len(list) != 4 || list[3].Summary != "again" {
		t.Fatalf("sequential same-name = %+v, want a fresh fourth entry", list)
	}
}

// TestHandleGetConversationIncludesTools drives the handler directly with an
// injected user (auth.WithUser) and asserts persisted tool activity is part of
// the message JSON (2.10).
func TestHandleGetConversationIncludesTools(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u1", "alice", "")

	convID := "c1"
	if err := st.InsertConversation(ctx, &store.Conversation{
		ID: convID, UserID: &u.ID, Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	if err := st.InsertMessage(ctx, &store.Message{
		ID: "a1", ConversationID: convID, Role: "assistant", Content: "hi",
		Tools:     []store.ToolActivity{{Name: "web_search", Status: "done", Summary: "3 results"}},
		CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/conversations/"+convID, nil)
	req.SetPathValue("id", convID)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleGetConversation(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Messages []store.Message `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Tools) != 1 {
		t.Fatalf("response messages = %+v, want one message with one tool", out.Messages)
	}
	got := out.Messages[0].Tools[0]
	if got.Name != "web_search" || got.Status != "done" || got.Summary != "3 results" {
		t.Errorf("tool = %+v", got)
	}
}
