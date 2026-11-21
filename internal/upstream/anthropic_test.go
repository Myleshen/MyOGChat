package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// anthropicFrame renders one SSE event block the way the Messages API does.
func anthropicFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// anthropicTestClient points a Client at a test server.
func anthropicTestClient(srv *httptest.Server) *Client {
	return &Client{hc: srv.Client(), messagesBase: srv.URL}
}

// TestAnthropicRequestBody pins the exact wire shape of one full turn: folded
// system prompt, a user turn with an inline image, max_tokens, stream, and the
// flat tool spec. It also pins the headers the gateway routes on.
func TestAnthropicRequestBody(t *testing.T) {
	var raw []byte
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		headers = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			anthropicFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
			anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			anthropicFrame("message_stop", `{"type":"message_stop"}`),
		)
	}))
	defer srv.Close()

	c := anthropicTestClient(srv)
	collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:      "anthropic",
		Model:        "claude-haiku-5-5",
		SessionID:    "sess-1",
		APIKey:       "sk-test",
		MaxTokens:    256,
		Instructions: "Be terse.",
		Context:      "Attachment: notes.txt",
		Messages: []Message{{
			Role:    "user",
			Content: "what is this?",
			Images:  []string{"data:image/png;base64,AAAB"},
		}},
		Tools: []ToolSpec{{Type: "function", Function: ToolFunctionSpec{
			Name:        "fetch_url",
			Description: "Fetch a URL",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}}}`),
		}}},
	}, nil, AgentOptions{}))

	const want = `{"max_tokens":256,"messages":[{"content":[{"text":"what is this?","type":"text"},` +
		`{"source":{"data":"AAAB","media_type":"image/png","type":"base64"},"type":"image"}],"role":"user"}],` +
		`"model":"claude-haiku-5-5","stream":true,"system":"Attachment: notes.txt\n\nBe terse.",` +
		`"tools":[{"description":"Fetch a URL",` +
		`"input_schema":{"type":"object","properties":{"url":{"type":"string"}}},"name":"fetch_url"}]}`
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, raw)
	}
	if got := compact.String(); got != want {
		t.Errorf("request body =\n%s\nwant\n%s", got, want)
	}

	wantHeaders := map[string]string{
		"Content-Type":       "application/json",
		"x-api-key":          "sk-test",
		"Authorization":      "Bearer sk-test",
		"anthropic-version":  "2023-06-01",
		"x-opencode-session": "sess-1",
		"User-Agent":         "myogchat/1.0",
		"Accept":             "text/event-stream",
	}
	for name, want := range wantHeaders {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestAnthropicStreamTextAndThinking covers text split across chunks and
// thinking deltas in a single streaming call.
func TestAnthropicStreamTextAndThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			anthropicFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
			anthropicFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
			anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me"}}`),
			anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" think"}}`),
			anthropicFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			anthropicFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
			anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`),
			anthropicFrame("ping", `{"type":"ping"}`),
			anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`),
			anthropicFrame("content_block_stop", `{"type":"content_block_stop","index":1}`),
			anthropicFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`),
			anthropicFrame("message_stop", `{"type":"message_stop"}`),
		)
	}))
	defer srv.Close()

	events := collectEvents(t, anthropicTestClient(srv).Stream(context.Background(), Params{
		Dialect:  "anthropic",
		Model:    "minimax-m3",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))

	var text, thinking strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Delta)
		case EventReasoning:
			thinking.WriteString(ev.Delta)
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if got := text.String(); got != "Hello" {
		t.Errorf("text = %q, want %q", got, "Hello")
	}
	if got := thinking.String(); got != "Let me think" {
		t.Errorf("thinking = %q, want %q", got, "Let me think")
	}
}

// TestAnthropicUsageMapping pins the input/output/cache-read counts onto the
// Usage fields we already record.
func TestAnthropicUsageMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			anthropicFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}}`),
			anthropicFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
			anthropicFrame("message_stop", `{"type":"message_stop"}`),
		)
	}))
	defer srv.Close()

	events := collectEvents(t, anthropicTestClient(srv).Stream(context.Background(), Params{
		Dialect:  "anthropic",
		Model:    "qwen3.8-max",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))

	var usages []*Usage
	for _, ev := range events {
		if ev.Kind == EventUsage {
			usages = append(usages, ev.Usage)
		}
	}
	if len(usages) != 1 {
		t.Fatalf("usage events = %d, want 1", len(usages))
	}
	u := usages[0]
	if u.PromptTokens != 11 || u.CompletionTokens != 7 || u.CachedTokens != 3 || u.ReasoningTokens != 0 {
		t.Errorf("usage = %+v, want prompt 11, completion 7, cached 3", u)
	}
}

// TestAnthropicToolUseLoop covers a tool_use block whose arguments arrive as
// several input_json_delta fragments, and pins the round-trip the next request
// sends: assistant tool_use blocks then a user turn of tool_result blocks.
func TestAnthropicToolUseLoop(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var second []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			writeSSE(w,
				anthropicFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`),
				anthropicFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll fetch it"}}`),
				anthropicFrame("content_block_stop", `{"type":"content_block_stop","index":0}`),
				anthropicFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"fetch_url","input":{}}}`),
				anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"url\":"}}`),
				anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"https://example.com\"}"}}`),
				anthropicFrame("content_block_stop", `{"type":"content_block_stop","index":1}`),
				anthropicFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`),
				anthropicFrame("message_stop", `{"type":"message_stop"}`),
			)
			return
		}
		mu.Lock()
		second = body
		mu.Unlock()
		writeSSE(w,
			anthropicFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":20}}}`),
			anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Answer"}}`),
			anthropicFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`),
			anthropicFrame("message_stop", `{"type":"message_stop"}`),
		)
	}))
	defer srv.Close()

	var gotCall ToolCall
	exec := func(_ context.Context, call ToolCall) (string, string, error) {
		gotCall = call
		return "RESULT", "1 result", nil
	}
	events := collectEvents(t, anthropicTestClient(srv).StreamAgent(context.Background(), Params{
		Dialect:  "anthropic",
		Model:    "claude-haiku-5-5",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "fetch example.com"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "fetch_url"}}},
	}, exec, AgentOptions{}))

	if gotCall.ID != "toolu_1" || gotCall.Name != "fetch_url" ||
		gotCall.Arguments != `{"url":"https://example.com"}` {
		t.Errorf("exec got %+v", gotCall)
	}

	var text strings.Builder
	var usage *Usage
	for _, ev := range events {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Delta)
		case EventUsage:
			usage = ev.Usage
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if got := text.String(); got != "I'll fetch it\n\nAnswer" {
		t.Errorf("text = %q, want %q", got, "I'll fetch it\n\nAnswer")
	}
	if usage == nil || usage.PromptTokens != 30 || usage.CompletionTokens != 8 {
		t.Errorf("aggregated usage = %+v, want prompt 30, completion 8", usage)
	}

	mu.Lock()
	body := second
	mu.Unlock()
	var payload struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("second request: %v (%s)", err, body)
	}
	const wantMessages = `[{"content":[{"text":"fetch example.com","type":"text"}],"role":"user"},` +
		`{"content":[{"text":"I'll fetch it","type":"text"},` +
		`{"id":"toolu_1","input":{"url":"https://example.com"},"name":"fetch_url","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"RESULT","tool_use_id":"toolu_1","type":"tool_result"}],"role":"user"}]`
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload.Messages); err != nil {
		t.Fatalf("messages are not JSON: %v", err)
	}
	if got := compact.String(); got != wantMessages {
		t.Errorf("second request messages =\n%s\nwant\n%s", got, wantMessages)
	}
}

// TestAnthropicHistoryReplay pins how a stored tool round is rendered as
// history: the assistant's tool_use blocks followed by one user turn of
// tool_result blocks.
func TestAnthropicHistoryReplay(t *testing.T) {
	got := anthropicMessages(Message{
		ID:   "m1",
		Role: "assistant",
		Tools: []ToolResult{{
			Name:      "fetch_url",
			Arguments: `{"url":"https://x"}`,
			Output:    "OUT",
			Status:    ToolDone,
		}},
	})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `[{"content":[{"id":"call_m1_0","input":{"url":"https://x"},"name":"fetch_url","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"OUT","tool_use_id":"call_m1_0","type":"tool_result"}],"role":"user"}]`
	if string(b) != want {
		t.Errorf("history =\n%s\nwant\n%s", b, want)
	}
}

// TestAnthropicHTTPError pins that a non-2xx body becomes a clean error event,
// not a busy stream.
func TestAnthropicHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`)
	}))
	defer srv.Close()

	events := collectEvents(t, anthropicTestClient(srv).Stream(context.Background(), Params{
		Dialect:  "anthropic",
		Model:    "claude-haiku-5-5",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))
	if len(events) != 1 || events[0].Kind != EventError {
		t.Fatalf("events = %+v, want one error event", events)
	}
	msg := events[0].Err.Error()
	if !strings.Contains(msg, "upstream HTTP 400") || !strings.Contains(msg, "max_tokens: too large") {
		t.Errorf("error = %q", msg)
	}
}

// TestAnthropicStreamErrorEvent covers an `error` event mid-stream.
func TestAnthropicStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			anthropicFrame("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
		)
	}))
	defer srv.Close()

	events := collectEvents(t, anthropicTestClient(srv).Stream(context.Background(), Params{
		Dialect:  "anthropic",
		Model:    "qwen4-max",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))
	if len(events) != 1 || events[0].Kind != EventError {
		t.Fatalf("events = %+v, want one error event", events)
	}
	if msg := events[0].Err.Error(); !strings.Contains(msg, "overloaded_error") || !strings.Contains(msg, "Overloaded") {
		t.Errorf("error = %q", msg)
	}
}

// TestAnthropicTruncatedStream pins that a stream cut mid-frame (dropped
// connection, killed gateway) ends the channel quietly instead of panicking or
// hanging.
func TestAnthropicTruncatedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			anthropicFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":2}}}`),
			anthropicFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`),
			// Truncated: no blank line, no closing brace, no message_stop.
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tial`,
		)
	}))
	defer srv.Close()

	events := collectEvents(t, anthropicTestClient(srv).Stream(context.Background(), Params{
		Dialect:  "anthropic",
		Model:    "qwen3.7-plus",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))
	var text strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Delta)
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if got := text.String(); got != "par" {
		t.Errorf("text = %q, want %q", got, "par")
	}
}
