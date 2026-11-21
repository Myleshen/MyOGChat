package server

import (
	"context"
	"strings"
	"testing"

	"myogchat/internal/tools"
	"myogchat/internal/upstream"
)

// TestToolExecutorUnknownToolListsAvailable guards the feedback shape for a
// hallucinated tool name: a normal tool error naming what can actually be called.
func TestToolExecutorUnknownToolListsAvailable(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Add(tools.NewFetchURLTool())

	s := &Server{}
	_, _, err := s.toolExecutor(reg, 0)(context.Background(), upstream.ToolCall{Name: "email_list"})
	if err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
	for _, want := range []string{`unknown tool "email_list"`, "fetch_url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}
