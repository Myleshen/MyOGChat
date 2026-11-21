package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestChatOnceRequestShape pins the wire shape of a one-shot helper call: one
// non-streaming chat completion, a small token budget, and no tools.
func TestChatOnceRequestShape(t *testing.T) {
	var got map[string]any
	var path, session, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, session, auth = r.URL.Path, r.Header.Get("x-opencode-session"), r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"Title here"}}]}`)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	out, err := c.ChatOnce(context.Background(), "k", "sess-1", "mimo-v2.6-flash",
		[]Message{{Role: "user", Content: "hi"}}, 24)
	if err != nil || out != "Title here" {
		t.Fatalf("ChatOnce = %q, %v", out, err)
	}
	if path != "/chat/completions" {
		t.Fatalf("path = %q", path)
	}
	if session != "sess-1" || auth != "Bearer k" {
		t.Fatalf("session/auth = %q/%q", session, auth)
	}
	if got["model"] != "mimo-v2.6-flash" || got["max_tokens"] != float64(24) || got["stream"] != false {
		t.Fatalf("payload = %+v", got)
	}
	if _, ok := got["tools"]; ok {
		t.Fatalf("helper call must not send tools: %+v", got)
	}
	if msgs, _ := got["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("messages = %+v, want one", got["messages"])
	}
}
