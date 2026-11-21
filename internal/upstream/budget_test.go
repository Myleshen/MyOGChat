package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDialectTurnBudget pins the output budget on the wire for every dialect
// that takes one, and its absence from chat completions (which has never sent
// a cap). It is the adapter half of the server's turnMaxTokens guarantee.
func TestDialectTurnBudget(t *testing.T) {
	const budget = 8192
	cases := []struct {
		name      string
		dialect   string
		reply     string
		field     string // dotted path to the budget in the request body
		wantField bool
	}{
		{name: "anthropic", dialect: "anthropic", reply: anthropicFrame("message_stop", `{"type":"message_stop"}`), field: "max_tokens", wantField: true},
		{name: "google", dialect: "google", reply: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n", field: "generationConfig.maxOutputTokens", wantField: true},
		{name: "responses", dialect: "responses", reply: "data: {\"type\":\"response.completed\",\"response\":{}}\n\n", field: "max_output_tokens", wantField: true},
		{name: "chat sends no budget", dialect: "chat", reply: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", field: "max_tokens", wantField: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, c.reply)
			}))
			defer srv.Close()

			cl := &Client{hc: srv.Client(), baseURL: srv.URL, messagesBase: srv.URL, googleURL: srv.URL}
			collectEvents(t, cl.Stream(context.Background(), Params{
				Dialect:   c.dialect,
				Model:     "m",
				APIKey:    "k",
				MaxTokens: budget,
				Messages:  []Message{{Role: "user", Content: "hi"}},
			}))

			got, ok := dotted(body, c.field)
			if !c.wantField {
				if ok {
					t.Errorf("%s carries %s = %v, want no budget field", c.dialect, c.field, got)
				}
				return
			}
			if !ok || got != float64(budget) {
				t.Errorf("%s: %s = %v (present %v), want %d", c.dialect, c.field, got, ok, budget)
			}
		})
	}
}

// dotted walks a nested JSON object by dot-separated keys.
func dotted(obj map[string]any, path string) (any, bool) {
	var cur any = obj
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
