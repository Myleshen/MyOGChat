package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func collectEvents(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("timed out waiting for stream events")
			return out
		}
	}
}

func writeSSE(w http.ResponseWriter, frames ...string) {
	for _, f := range frames {
		io.WriteString(w, f)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestChatToolAccumulator(t *testing.T) {
	var acc chatToolAccumulator
	acc.addDelta(0, "call_1", "web_search", `{"query":`)
	acc.addDelta(0, "", "", `"golang"}`)
	acc.addDelta(1, "call_2", "fetch_url", `{"url":"https://example.com"}`)

	calls := acc.result()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "web_search" ||
		calls[0].Arguments != `{"query":"golang"}` {
		t.Errorf("call 0 = %+v", calls[0])
	}
	if calls[1].Name != "fetch_url" || calls[1].Arguments != `{"url":"https://example.com"}` {
		t.Errorf("call 1 = %+v", calls[1])
	}
}

func TestResponsesToolAccumulator(t *testing.T) {
	var acc responsesToolAccumulator
	acc.added("fc_1", "call_1", "fetch_url", "")
	acc.argsDelta("fc_1", `{"url":"https://a"`)
	acc.argsDelta("fc_1", `.com"}`)
	// output_item.done is authoritative and replaces the streamed args.
	acc.done("fc_1", "call_1", "fetch_url", `{"url":"https://a.com"}`)

	calls := acc.result()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	c := calls[0]
	if c.ID != "call_1" || c.ItemID != "fc_1" || c.Name != "fetch_url" ||
		c.Arguments != `{"url":"https://a.com"}` {
		t.Errorf("call = %+v", c)
	}
}

func TestResponsesToolAccumulatorArgsDone(t *testing.T) {
	var acc responsesToolAccumulator
	acc.added("fc_9", "call_9", "linkwarden_save", "")
	acc.argsDelta("fc_9", `{"url":`)
	acc.argsDone("fc_9", `{"url":"https://x","title":"X"}`)
	calls := acc.result()
	if len(calls) != 1 || calls[0].Arguments != `{"url":"https://x","title":"X"}` {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestUsageAdd(t *testing.T) {
	var u Usage
	u.add(&Usage{PromptTokens: 10, CompletionTokens: 2, CachedTokens: 1, ReasoningTokens: 1})
	u.add(&Usage{PromptTokens: 5, CompletionTokens: 3})
	if u.PromptTokens != 15 || u.CompletionTokens != 5 || u.CachedTokens != 1 || u.ReasoningTokens != 1 {
		t.Errorf("accumulated usage = %+v", u)
	}
}

func TestChatAssistantToolMessage(t *testing.T) {
	m := chatAssistantToolMessage("", []ToolCall{{ID: "call_1", Name: "web_search", Arguments: `{"q":"x"}`}})
	if _, ok := m["content"]; ok {
		t.Error("empty text should omit the content field")
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"tool_calls"`) || !strings.Contains(string(b), `"web_search"`) {
		t.Errorf("message = %s", b)
	}

	m2 := chatAssistantToolMessage("thinking", []ToolCall{{ID: "c", Name: "t", Arguments: "{}"}})
	if m2["content"] != "thinking" {
		t.Errorf("content = %v", m2["content"])
	}
}

func TestStreamAgentChatLoop(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var secondBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			writeSSE(w,
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\"}}]}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"golang\\\"}\"}}]}}]}\n\n",
				"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":1}}}\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		mu.Lock()
		secondBody = body
		mu.Unlock()
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"Answer\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":5}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	var gotCall ToolCall
	exec := func(_ context.Context, call ToolCall) (string, string, error) {
		gotCall = call
		return "RESULT", "1 result", nil
	}

	events := collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "web_search"}}},
	}, exec))

	if gotCall.Name != "web_search" || gotCall.Arguments != `{"query":"golang"}` {
		t.Errorf("exec got %+v", gotCall)
	}

	var sawRunning, sawDone, sawText bool
	var usage *Usage
	for _, ev := range events {
		switch {
		case ev.Kind == EventTool && ev.Tool != nil && ev.Tool.Status == ToolRunning:
			sawRunning = true
		case ev.Kind == EventTool && ev.Tool != nil && ev.Tool.Status == ToolDone:
			sawDone = true
		case ev.Kind == EventText && ev.Delta == "Answer":
			sawText = true
		case ev.Kind == EventUsage:
			usage = ev.Usage
		case ev.Kind == EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if !sawRunning || !sawDone {
		t.Errorf("missing tool events (running=%v done=%v)", sawRunning, sawDone)
	}
	if !sawText {
		t.Error("missing final text")
	}
	if usage == nil || usage.PromptTokens != 30 || usage.CompletionTokens != 7 || usage.CachedTokens != 1 {
		t.Errorf("aggregated usage = %+v", usage)
	}
	mu.Lock()
	body := secondBody
	mu.Unlock()
	if !strings.Contains(string(body), `"role":"tool"`) ||
		!strings.Contains(string(body), `"tool_call_id":"call_1"`) ||
		!strings.Contains(string(body), "RESULT") ||
		!strings.Contains(string(body), `"tool_calls"`) {
		t.Errorf("second request missing tool round-trip: %s", body)
	}
}

func TestStreamAgentResponsesLoop(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var secondBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			writeSSE(w,
				"event: response.output_item.added\ndata: {\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"fetch_url\",\"arguments\":\"\"}}\n\n",
				"event: response.function_call_arguments.delta\ndata: {\"item_id\":\"fc_1\",\"delta\":\"{\\\"url\\\":\\\"https://x\\\"}\"}\n\n",
				"event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":3},\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"fetch_url\",\"arguments\":\"{\\\"url\\\":\\\"https://x\\\"}\"}]}}\n\n",
			)
			return
		}
		mu.Lock()
		secondBody = body
		mu.Unlock()
		writeSSE(w,
			"event: response.output_text.delta\ndata: {\"delta\":\"Done\"}\n\n",
			"event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":4},\"output\":[]}}\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	exec := func(_ context.Context, call ToolCall) (string, string, error) {
		if call.Name != "fetch_url" || call.ID != "call_1" {
			t.Errorf("exec got %+v", call)
		}
		return "PAGE TEXT", "", nil
	}

	events := collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "responses",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "summarize"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "fetch_url"}}},
	}, exec))

	var sawText bool
	var usage *Usage
	for _, ev := range events {
		switch {
		case ev.Kind == EventText && ev.Delta == "Done":
			sawText = true
		case ev.Kind == EventUsage:
			usage = ev.Usage
		case ev.Kind == EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if !sawText {
		t.Error("missing final text")
	}
	if usage == nil || usage.PromptTokens != 12 || usage.CompletionTokens != 7 {
		t.Errorf("aggregated usage = %+v", usage)
	}
	mu.Lock()
	body := string(secondBody)
	mu.Unlock()
	if !strings.Contains(body, `"function_call"`) ||
		!strings.Contains(body, `"function_call_output"`) ||
		!strings.Contains(body, `"call_id":"call_1"`) ||
		!strings.Contains(body, "PAGE TEXT") {
		t.Errorf("second request missing function round-trip: %s", body)
	}
}

func TestStreamAgentBoundsIterations(t *testing.T) {
	var calls, toolLess int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(body), `"tools"`) {
			// Keep requesting a tool forever; the loop must still terminate.
			writeSSE(w,
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"type\":\"function\",\"function\":{\"name\":\"fetch_url\",\"arguments\":\"{}\"}}]}}]}\n\n",
				"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		atomic.AddInt32(&toolLess, 1)
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"final\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	exec := func(_ context.Context, _ ToolCall) (string, string, error) { return "r", "", nil }
	events := collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "fetch_url"}}},
	}, exec))

	if got := atomic.LoadInt32(&calls); got != maxToolIterations {
		t.Errorf("upstream calls = %d, want %d", got, maxToolIterations)
	}
	if atomic.LoadInt32(&toolLess) != 1 {
		t.Errorf("forced tool-less final round = %d, want 1", toolLess)
	}
	var sawFinal bool
	for _, ev := range events {
		if ev.Kind == EventText && ev.Delta == "final" {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Error("loop bound did not produce a final answer")
	}
}

func TestStreamAgentRetriesWithoutTools(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// Simulate a model that rejects tools.
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"tools not supported"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"plain\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	events := collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "x"}}},
	}, nil))

	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected a retry, got %d calls", calls)
	}
	for _, ev := range events {
		if ev.Kind == EventError {
			t.Fatalf("unexpected error after retry: %v", ev.Err)
		}
	}
}

func TestResponsesToolSpecs(t *testing.T) {
	specs := responsesToolSpecs([]ToolSpec{{
		Type: "function",
		Function: ToolFunctionSpec{
			Name:        "web_search",
			Description: "search",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}})
	b, _ := json.Marshal(specs)
	if !strings.Contains(string(b), `"name":"web_search"`) {
		t.Errorf("responses tool specs not flattened: %s", b)
	}
	if strings.Contains(string(b), `"function":`) {
		t.Errorf("responses tool specs must not nest under function: %s", b)
	}
}
