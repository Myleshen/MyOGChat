package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// newGoogleClient points a Client at a test server.
func newGoogleClient(srv *httptest.Server) *Client {
	return &Client{hc: srv.Client(), googleURL: srv.URL}
}

// mustJSON decodes a JSON literal for whole-body comparisons.
func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad expected JSON: %v", err)
	}
	return v
}

// TestStreamGoogleTextAndUsage covers streamed text across chunks, a thought
// part, and usageMetadata mapping, plus the endpoint path and headers.
func TestStreamGoogleTextAndUsage(t *testing.T) {
	var path, query, session, auth, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		session, auth, ua = r.Header.Get("x-opencode-session"), r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Let me \",\"thought\":true}]}}]}\n\n",
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n",
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]}}]}\n\n",
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":4,\"cachedContentTokenCount\":6,\"thoughtsTokenCount\":2}}\n\n",
		)
	}))
	defer srv.Close()

	events := collectEvents(t, newGoogleClient(srv).Stream(context.Background(), Params{
		Dialect:   "google",
		Model:     "gemini-4-argon",
		SessionID: "sess-1",
		APIKey:    "k",
		Messages:  []Message{{Role: "user", Content: "hi"}},
	}))

	if want := "/models/gemini-4-argon:streamGenerateContent"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if query != "alt=sse" {
		t.Errorf("query = %q, want alt=sse", query)
	}
	if session != "sess-1" || auth != "Bearer k" || ua != userAgent {
		t.Errorf("session/auth/ua = %q/%q/%q", session, auth, ua)
	}

	var text, reasoning strings.Builder
	var usage *Usage
	for _, ev := range events {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Delta)
		case EventReasoning:
			reasoning.WriteString(ev.Delta)
		case EventUsage:
			usage = ev.Usage
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if got := text.String(); got != "Hello" {
		t.Errorf("text = %q, want %q", got, "Hello")
	}
	if got := reasoning.String(); got != "Let me " {
		t.Errorf("reasoning = %q", got)
	}
	want := Usage{PromptTokens: 10, CompletionTokens: 6, CachedTokens: 6, ReasoningTokens: 2}
	if usage == nil || *usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

// TestStreamGoogleRequestShape pins one complete request: system instruction,
// contents with an image and a replayed tool round-trip, generationConfig and
// functionDeclarations.
func TestStreamGoogleRequestShape(t *testing.T) {
	var got any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]}}]}\n\n")
	}))
	defer srv.Close()

	params := []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`)
	// The agent path is the one that sends tools.
	collectEvents(t, newGoogleClient(srv).StreamAgent(context.Background(), Params{
		Dialect:      "google",
		Model:        "gemini-4-argon",
		APIKey:       "k",
		Effort:       "medium",
		Context:      "attachment text",
		Instructions: "be nice",
		Messages: []Message{
			{Role: "user", Content: "hi", Images: []string{"data:image/png;base64,AAAA"}},
			{
				ID:    "m1",
				Role:  "assistant",
				Tools: []ToolResult{{Name: "web_search", Arguments: `{"query":"go"}`, Output: "RESULT", Status: ToolDone}},
			},
			{Role: "user", Content: "again"},
		},
		Tools: []ToolSpec{{Type: "function", Function: ToolFunctionSpec{
			Name: "web_search", Description: "search", Parameters: params,
		}}},
	}, func(context.Context, ToolCall) (string, string, error) { return "", "", nil }, AgentOptions{}))

	want := mustJSON(t, `{
		"systemInstruction": {"parts": [{"text": "attachment text"}, {"text": "be nice"}]},
		"contents": [
			{"role": "user", "parts": [
				{"text": "hi"},
				{"inlineData": {"mimeType": "image/png", "data": "AAAA"}}
			]},
			{"role": "model", "parts": [
				{"functionCall": {"name": "web_search", "args": {"query": "go"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "web_search", "response": {"result": "RESULT"}}}
			]},
			{"role": "user", "parts": [{"text": "again"}]}
		],
		"generationConfig": {
			"maxOutputTokens": 4096,
			"temperature": 1.0,
			"thinkingConfig": {"includeThoughts": true, "thinkingBudget": 8192}
		},
		"tools": [{"functionDeclarations": [{
			"name": "web_search",
			"description": "search",
			"parameters": {"type": "object", "properties": {"query": {"type": "string"}}}
		}]}]
	}`)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("request body =\n%s\nwant\n%s", g, mustMarshalIndent(t, want))
	}
}

func mustMarshalIndent(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestStreamAgentGoogleToolLoop drives a functionCall mid-stream, then checks
// the tool ran and the follow-up request carries the functionResponse turn.
func TestStreamAgentGoogleToolLoop(t *testing.T) {
	var mu sync.Mutex
	var secondBody []byte
	var round int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		round++
		n := round
		if n == 2 {
			secondBody = body
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			writeSSE(w,
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Searching.\"}]}}]}\n\n",
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"web_search\",\"args\":{\"query\":\"go\"}}}]}}]}\n\n",
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2}}\n\n",
			)
			return
		}
		writeSSE(w,
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Answer\"}]}}],\"usageMetadata\":{\"promptTokenCount\":20,\"candidatesTokenCount\":5}}\n\n",
		)
	}))
	defer srv.Close()

	var gotCall ToolCall
	exec := func(_ context.Context, call ToolCall) (string, string, error) {
		gotCall = call
		return "RESULT", "1 result", nil
	}

	events := collectEvents(t, newGoogleClient(srv).StreamAgent(context.Background(), Params{
		Dialect:  "google",
		Model:    "gemini-4-argon",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "web_search"}}},
	}, exec, AgentOptions{}))

	if gotCall.Name != "web_search" || gotCall.Arguments != `{"query":"go"}` {
		t.Errorf("exec got %+v", gotCall)
	}
	var text strings.Builder
	var sawDone bool
	var usage *Usage
	for _, ev := range events {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Delta)
		case EventTool:
			if ev.Tool != nil && ev.Tool.Status == ToolDone && ev.Tool.Output == "RESULT" {
				sawDone = true
			}
		case EventUsage:
			usage = ev.Usage
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if !sawDone {
		t.Error("missing tool done event")
	}
	if got := text.String(); got != "Searching.Answer" {
		t.Errorf("text = %q", got)
	}
	if usage == nil || usage.PromptTokens != 30 || usage.CompletionTokens != 7 {
		t.Errorf("aggregated usage = %+v", usage)
	}

	mu.Lock()
	body := secondBody
	mu.Unlock()
	got := mustJSON(t, string(body))
	wantParts := []any{
		map[string]any{"text": "Searching."},
		map[string]any{"functionCall": map[string]any{"name": "web_search", "args": map[string]any{"query": "go"}}},
	}
	var foundModel, foundResponse bool
	for _, raw := range got.(map[string]any)["contents"].([]any) {
		c := raw.(map[string]any)
		switch c["role"] {
		case "model":
			if reflect.DeepEqual(c["parts"], wantParts) {
				foundModel = true
			}
		case "user":
			if reflect.DeepEqual(c["parts"], []any{map[string]any{
				"functionResponse": map[string]any{
					"name": "web_search", "response": map[string]any{"result": "RESULT"},
				},
			}}) {
				foundResponse = true
			}
		}
	}
	if !foundModel {
		t.Errorf("second request lost the model functionCall turn: %s", body)
	}
	if !foundResponse {
		t.Errorf("second request lost the functionResponse turn: %s", body)
	}
}

// TestStreamGoogleJSONArray covers an endpoint that ignores alt=sse and streams
// the chunked JSON-array form instead.
func TestStreamGoogleJSONArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]},
			{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}],
			 "usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}
		]`)
	}))
	defer srv.Close()

	events := collectEvents(t, newGoogleClient(srv).Stream(context.Background(), Params{
		Dialect:  "google",
		Model:    "gemini-4-argon",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))

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
	if got := text.String(); got != "Hello" {
		t.Errorf("text = %q", got)
	}
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", usage)
	}
}

// TestStreamGoogleProviderError checks a non-2xx Google error body becomes a
// clean error event rather than a raw dump.
func TestStreamGoogleProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"tool schema is invalid"}}`)
	}))
	defer srv.Close()

	events := collectEvents(t, newGoogleClient(srv).Stream(context.Background(), Params{
		Dialect:  "google",
		Model:    "gemini-4-argon",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))

	if len(events) != 1 || events[0].Kind != EventError {
		t.Fatalf("events = %+v", events)
	}
	want := "upstream HTTP 400: INVALID_ARGUMENT: tool schema is invalid"
	if got := events[0].Err.Error(); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// TestStreamGoogleInStreamError covers an error object delivered as a frame.
func TestStreamGoogleInStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"part\"}]}}]}\n\n",
			"data: {\"error\":{\"status\":\"RESOURCE_EXHAUSTED\",\"message\":\"quota exceeded\"}}\n\n",
		)
	}))
	defer srv.Close()

	events := collectEvents(t, newGoogleClient(srv).Stream(context.Background(), Params{
		Dialect:  "google",
		Model:    "gemini-4-argon",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))

	var text, errText string
	for _, ev := range events {
		switch ev.Kind {
		case EventText:
			text += ev.Delta
		case EventError:
			errText = ev.Err.Error()
		}
	}
	if text != "part" {
		t.Errorf("text = %q", text)
	}
	want := "upstream RESOURCE_EXHAUSTED: quota exceeded"
	if errText != want {
		t.Errorf("error = %q, want %q", errText, want)
	}
}

// TestStreamGoogleTruncated checks a stream cut mid-frame ends cleanly with the
// text that did arrive.
func TestStreamGoogleTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"kept\"}]}}]}\n\n",
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"par",
		)
	}))
	defer srv.Close()

	events := collectEvents(t, newGoogleClient(srv).Stream(context.Background(), Params{
		Dialect:  "google",
		Model:    "gemini-4-argon",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))

	var text strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Delta)
		case EventError:
			t.Fatalf("truncated stream must not error: %v", ev.Err)
		}
	}
	if got := text.String(); got != "kept" {
		t.Errorf("text = %q, want %q", got, "kept")
	}
}

// TestParseGoogleJSONArrayTruncated covers a cut JSON-array body: complete
// elements are delivered, the broken tail is dropped.
func TestParseGoogleJSONArrayTruncated(t *testing.T) {
	body := `[{"candidates":[{"content":{"parts":[{"text":"a"}]}}]},{"candidates":[{"content":{"par`
	var got []string
	err := parseGoogleStream(strings.NewReader(body), func(r *googleResponse) (bool, error) {
		for _, c := range r.Candidates {
			for _, p := range c.Content.Parts {
				got = append(got, p.Text)
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("parseGoogleStream: %v", err)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("parts = %v", got)
	}
}
