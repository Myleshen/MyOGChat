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

// TestStreamAgentGoogleReplaysThoughtSignature checks that the signature Gemini
// emits with a functionCall is echoed on that same part in the follow-up
// request, and that it rides the tool activity for persistence.
func TestStreamAgentGoogleReplaysThoughtSignature(t *testing.T) {
	const sig = "sig-AbC123+/="
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
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"web_search\",\"args\":{\"query\":\"go\"}},\"thoughtSignature\":\""+sig+"\"}]}}]}\n\n",
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
	var gotActivity string
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
	for _, ev := range events {
		switch ev.Kind {
		case EventTool:
			if ev.Tool != nil && ev.Tool.Status == ToolDone {
				gotActivity = ev.Tool.ThoughtSignature
			}
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if gotCall.ThoughtSignature != sig {
		t.Errorf("exec call signature = %q, want %q", gotCall.ThoughtSignature, sig)
	}
	if gotActivity != sig {
		t.Errorf("tool activity signature = %q, want %q", gotActivity, sig)
	}

	mu.Lock()
	body := secondBody
	mu.Unlock()
	if !strings.Contains(string(body), `"thoughtSignature":"`+sig+`"`) {
		t.Fatalf("second request lost the signature: %s", body)
	}
	got := mustJSON(t, string(body))
	wantParts := []any{
		map[string]any{"text": "Searching."},
		map[string]any{
			"functionCall":     map[string]any{"name": "web_search", "args": map[string]any{"query": "go"}},
			"thoughtSignature": sig,
		},
	}
	var foundModel bool
	for _, raw := range got.(map[string]any)["contents"].([]any) {
		c := raw.(map[string]any)
		if c["role"] == "model" {
			if !reflect.DeepEqual(c["parts"], wantParts) {
				t.Errorf("model turn parts = %v, want %v", c["parts"], wantParts)
			}
			foundModel = true
		}
	}
	if !foundModel {
		t.Errorf("second request lost the model functionCall turn: %s", body)
	}
}

// TestStreamAgentGoogleSignatureOnlyPart covers a stream that delivers the
// signature in a part of its own, after the functionCall part.
func TestStreamAgentGoogleSignatureOnlyPart(t *testing.T) {
	const sig = "sig-in-own-part"
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
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"web_search\",\"args\":{\"query\":\"go\"}}}]}}]}\n\n",
				"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"thoughtSignature\":\""+sig+"\"}]}}]}\n\n",
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
		return "RESULT", "", nil
	}
	collectEvents(t, newGoogleClient(srv).StreamAgent(context.Background(), Params{
		Dialect:  "google",
		Model:    "gemini-4-argon",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "web_search"}}},
	}, exec, AgentOptions{}))

	if gotCall.ThoughtSignature != sig {
		t.Errorf("exec call signature = %q, want %q", gotCall.ThoughtSignature, sig)
	}
	mu.Lock()
	body := secondBody
	mu.Unlock()
	want := `"thoughtSignature":"` + sig + `"`
	if !strings.Contains(string(body), want) {
		t.Fatalf("second request lost the signature: %s", body)
	}
	if strings.Contains(string(body), `"thoughtSignature":"`+sig+`","thoughtSignature"`) {
		t.Errorf("signature duplicated: %s", body)
	}
}

// TestGoogleContentsReplaysStoredSignature checks a reloaded conversation (a
// stored ToolResult) replays its signature, and one without a signature builds
// exactly as before.
func TestGoogleContentsReplaysStoredSignature(t *testing.T) {
	const sig = "sig-stored"
	newMsg := func(signature string) Message {
		return Message{
			ID: "m1", Role: "assistant",
			Tools: []ToolResult{{
				Name: "web_search", Arguments: `{"query":"go"}`, Output: "RESULT",
				Status: ToolDone, ThoughtSignature: signature,
			}},
		}
	}

	body, err := json.Marshal(googleContents([]Message{newMsg(sig)}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := mustJSON(t, `[
		{"role":"model","parts":[{"functionCall":{"name":"web_search","args":{"query":"go"}},"thoughtSignature":"sig-stored"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"web_search","response":{"result":"RESULT"}}}]}
	]`)
	if got := mustJSON(t, string(body)); !reflect.DeepEqual(got, want) {
		t.Errorf("contents = %s, want %s", body, mustMarshalIndent(t, want))
	}

	plain, err := json.Marshal(googleContents([]Message{newMsg("")}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "thoughtSignature") {
		t.Errorf("empty signature emitted a key: %s", plain)
	}
}

// TestGoogleTextOnlyPartsUnchanged pins a text-only Google turn: no stray
// thoughtSignature key appears (omitempty).
func TestGoogleTextOnlyPartsUnchanged(t *testing.T) {
	body, err := json.Marshal(googleContents([]Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"hello"}]}]`
	if string(body) != want {
		t.Errorf("contents = %s, want %s", body, want)
	}
}

// TestOtherDialectsIgnoreThoughtSignature checks the chat, responses and
// anthropic builders produce byte-identical bodies whether or not a stored
// call carries Google metadata.
func TestOtherDialectsIgnoreThoughtSignature(t *testing.T) {
	base := Message{ID: "m1", Role: "assistant", Content: "ran", Tools: []ToolResult{{
		Name: "web_search", Arguments: `{"q":"x"}`, Output: "out", Status: ToolDone,
	}}}
	signed := base
	signed.Tools = append([]ToolResult(nil), base.Tools...)
	signed.Tools[0].ThoughtSignature = "sig-ignored"

	builders := map[string]func(Message) any{
		"chat":      func(m Message) any { return chatMessages(m) },
		"responses": func(m Message) any { return responsesMessages(m) },
		"anthropic": func(m Message) any { return anthropicMessages(m) },
	}
	for name, build := range builders {
		plain, err := json.Marshal(build(base))
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		withSig, err := json.Marshal(build(signed))
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		if string(plain) != string(withSig) {
			t.Errorf("%s body changed with metadata:\nplain: %s\nsigned: %s", name, plain, withSig)
		}
		if strings.Contains(string(withSig), "sig-ignored") {
			t.Errorf("%s leaked Google metadata: %s", name, withSig)
		}
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
