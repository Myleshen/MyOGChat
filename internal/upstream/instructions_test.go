package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProjectMemoryInstructionsReachBothDialects pins §8.6 + 6.2: the composed
// system prompt (instructions plus the "## Project memory" block) is what each
// dialect actually sends — a leading system message for chat, the dedicated
// `instructions` field for responses.
func TestProjectMemoryInstructionsReachBothDialects(t *testing.T) {
	const withMemory = "Be terse.\n\n## Project memory\n- uses mise"
	const withoutMemory = "Be terse."

	cases := []struct {
		dialect string
		frames  []string
	}{
		{"chat", []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		}},
		{"responses", []string{
			"event: response.output_text.delta\ndata: {\"delta\":\"ok\"}\n\n",
			"event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		}},
	}
	for _, tc := range cases {
		for _, instructions := range []string{withMemory, withoutMemory} {
			var raw []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, tc.frames...)
			}))
			c := &Client{hc: srv.Client(), baseURL: srv.URL}
			collectEvents(t, c.StreamAgent(context.Background(), Params{
				Dialect:      tc.dialect,
				Model:        "test",
				APIKey:       "k",
				Instructions: instructions,
				Messages:     []Message{{Role: "user", Content: "hi"}},
			}, nil, AgentOptions{}))
			srv.Close()

			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("%s: decode payload: %v", tc.dialect, err)
			}
			switch tc.dialect {
			case "chat":
				msgs, _ := payload["messages"].([]any)
				if len(msgs) == 0 {
					t.Fatalf("chat: no messages in %s", raw)
				}
				first, _ := msgs[0].(map[string]any)
				if first["role"] != "system" || first["content"] != instructions {
					t.Fatalf("chat system message = %+v, want %q", first, instructions)
				}
			case "responses":
				if payload["instructions"] != instructions {
					t.Fatalf("responses instructions = %v, want %q", payload["instructions"], instructions)
				}
			}
		}
	}
}
