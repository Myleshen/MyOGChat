package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// upstreamRequest is one request a fake server received.
type upstreamRequest struct {
	Path   string
	Header http.Header
	Body   []byte
}

type upstreamLog struct {
	mu   sync.Mutex
	reqs []upstreamRequest
}

func (l *upstreamLog) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, upstreamRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
}

func (l *upstreamLog) all(t *testing.T) []upstreamRequest {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reqs) == 0 {
		t.Fatal("fake upstream received no requests")
	}
	return append([]upstreamRequest(nil), l.reqs...)
}

// newSSEServer answers every POST with one SSE body per request, in order.
func newSSEServer(t *testing.T, bodies ...string) (*httptest.Server, *upstreamLog) {
	t.Helper()
	log := &upstreamLog{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		log.mu.Lock()
		n := len(log.reqs) - 1
		log.mu.Unlock()
		if n >= len(bodies) {
			n = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bodies[n])
	}))
	t.Cleanup(ts.Close)
	return ts, log
}

func drain(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// TestCustomBaseURLHeaders posts to a custom provider: Bearer + UA, and no
// x-opencode-session.
func TestCustomBaseURLHeaders(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	ts, log := newSSEServer(t, sse)
	c := NewClient()

	events := drain(t, c.Stream(context.Background(), Params{
		Dialect:   "chat",
		Model:     "meta/llama-3.1-8b",
		SessionID: "conv-1",
		APIKey:    "nvapi-key",
		BaseURL:   ts.URL + "/v1",
		Messages:  []Message{{Role: "user", Content: "hi"}},
	}))
	var text string
	for _, ev := range events {
		if ev.Kind == EventText {
			text += ev.Delta
		}
		if ev.Kind == EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if text != "hi" {
		t.Errorf("text = %q", text)
	}
	got := log.all(t)[0]
	if got.Path != "/v1/chat/completions" {
		t.Errorf("path = %q", got.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer nvapi-key" {
		t.Errorf("Authorization = %q", h)
	}
	if h := got.Header.Get("x-opencode-session"); h != "" {
		t.Errorf("x-opencode-session = %q, want absent", h)
	}
	if h := got.Header.Get("User-Agent"); h != userAgent {
		t.Errorf("User-Agent = %q", h)
	}
	if !strings.Contains(string(got.Body), `"model":"meta/llama-3.1-8b"`) {
		t.Errorf("body = %s", got.Body)
	}
}

// TestOpenCodeKeepsSessionHeader keeps the OpenCode path unchanged.
func TestOpenCodeKeepsSessionHeader(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	ts, log := newSSEServer(t, sse)
	c := NewClient()
	c.baseURL = ts.URL

	drain(t, c.Stream(context.Background(), Params{
		Dialect: "chat", Model: "glm-5.3-flash", SessionID: "conv-2", APIKey: "oc-key",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))
	got := log.all(t)[0]
	if h := got.Header.Get("x-opencode-session"); h != "conv-2" {
		t.Errorf("x-opencode-session = %q, want conv-2", h)
	}
	if !strings.HasPrefix(got.Path, "/chat/completions") {
		t.Errorf("path = %q", got.Path)
	}
}

// TestAgentToolLoopThroughCustomProvider runs one tool round-trip through a
// custom base URL, asserting both requests skip the OpenCode header.
func TestAgentToolLoopThroughCustomProvider(t *testing.T) {
	callSSE := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"go\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	answerSSE := "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	ts, log := newSSEServer(t, callSSE, answerSSE)
	c := NewClient()

	var toolCalls int
	events := drain(t, c.StreamAgent(context.Background(), Params{
		Dialect:   "chat",
		Model:     "meta/llama-3.1-8b",
		SessionID: "conv-3",
		APIKey:    "nvapi-key",
		BaseURL:   ts.URL + "/v1",
		Messages:  []Message{{Role: "user", Content: "find go"}},
		Tools: []ToolSpec{{
			Type:     "function",
			Function: ToolFunctionSpec{Name: "lookup", Description: "lookup", Parameters: []byte(`{"type":"object"}`)},
		}},
	}, func(_ context.Context, call ToolCall) (string, string, error) {
		toolCalls++
		if call.Name != "lookup" {
			t.Errorf("tool name = %q", call.Name)
		}
		return "result", "1 result", nil
	}, AgentOptions{MaxIterations: 3}))

	if toolCalls != 1 {
		t.Errorf("tool calls = %d, want 1", toolCalls)
	}
	var text string
	for _, ev := range events {
		if ev.Kind == EventText {
			text += ev.Delta
		}
		if ev.Kind == EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if text != "done" {
		t.Errorf("text = %q, want done", text)
	}
	reqs := log.all(t)
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for i, r := range reqs {
		if r.Path != "/v1/chat/completions" {
			t.Errorf("request %d path = %q", i, r.Path)
		}
		if h := r.Header.Get("x-opencode-session"); h != "" {
			t.Errorf("request %d x-opencode-session = %q, want absent", i, h)
		}
	}
	if !strings.Contains(string(reqs[1].Body), "call_1") || !strings.Contains(string(reqs[1].Body), "result") {
		t.Errorf("second request did not replay the tool round: %s", reqs[1].Body)
	}
}
