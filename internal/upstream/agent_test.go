package upstream

import (
	"context"
	"encoding/json"
	"fmt"
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
	}, exec, AgentOptions{}))

	if gotCall.Name != "web_search" || gotCall.Arguments != `{"query":"golang"}` {
		t.Errorf("exec got %+v", gotCall)
	}

	var sawRunning, sawDone, sawText bool
	var runningArgs, doneOutput string
	var usage *Usage
	for _, ev := range events {
		switch {
		case ev.Kind == EventTool && ev.Tool != nil && ev.Tool.Status == ToolRunning:
			sawRunning = true
			runningArgs = ev.Tool.Args
		case ev.Kind == EventTool && ev.Tool != nil && ev.Tool.Status == ToolDone:
			sawDone = true
			doneOutput = ev.Tool.Output
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
	// Phase 4.1: the running event carries the arguments; the done event the output.
	if runningArgs != `{"query":"golang"}` {
		t.Errorf("running args = %q, want the call arguments", runningArgs)
	}
	if doneOutput != "RESULT" {
		t.Errorf("done output = %q, want the tool result", doneOutput)
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

// TestStreamAgentReasoningSeparated covers reasoning across tool rounds: the
// second round's reasoning must not glue onto the first ("Let's run it.Report
// every line…").
func TestStreamAgentReasoningSeparated(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			writeSSE(w,
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"first\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_bash\",\"arguments\":\"{}\"}}]}}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"second\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	exec := func(context.Context, ToolCall) (string, string, error) { return "ok", "ok", nil }
	events := collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "run_bash"}}},
	}, exec, AgentOptions{}))

	var reasoning, text strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case EventReasoning:
			reasoning.WriteString(ev.Delta)
		case EventText:
			text.WriteString(ev.Delta)
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if got := reasoning.String(); got != "first\n\nsecond" {
		t.Errorf("reasoning = %q, want %q", got, "first\n\nsecond")
	}
	if got := text.String(); got != "done" {
		t.Errorf("text = %q, want done", got)
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
	}, exec, AgentOptions{}))

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
	}, exec, AgentOptions{}))

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

// TestStreamAgentStepCallback proves the loop reports each round and the
// iteration bound, which is what agent-mode progress is built on.
func TestStreamAgentStepCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(body), `"tools"`) {
			writeSSE(w,
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"type\":\"function\",\"function\":{\"name\":\"fetch_url\",\"arguments\":\"{}\"}}]}}]}\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"final\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	var steps []int
	var bound int
	collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "fetch_url"}}},
	}, func(context.Context, ToolCall) (string, string, error) { return "r", "", nil },
		AgentOptions{MaxIterations: 3, OnStep: func(step, max int) {
			steps = append(steps, step)
			bound = max
		}}))

	if bound != 3 {
		t.Errorf("reported bound = %d, want 3", bound)
	}
	if len(steps) != 3 || steps[0] != 1 || steps[1] != 2 || steps[2] != 3 {
		t.Errorf("steps = %v, want [1 2 3]", steps)
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
	}, nil, AgentOptions{}))

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

// TestStreamAgentChatIncludesContext guards the 2.3 regression where the agent
// loop (used whenever tools are enabled) dropped Params.Context, so attachment
// text never reached the model.
func TestStreamAgentChatIncludesContext(t *testing.T) {
	var mu sync.Mutex
	var firstBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		if firstBody == nil {
			firstBody = body
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Context:  "ATTACHMENT-CONTEXT-BLOCK",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "x"}}},
	}, func(context.Context, ToolCall) (string, string, error) { return "", "", nil }, AgentOptions{}))

	mu.Lock()
	body := string(firstBody)
	mu.Unlock()
	if !strings.Contains(body, "ATTACHMENT-CONTEXT-BLOCK") {
		t.Errorf("chat agent request missing attachment context: %s", body)
	}
	if !strings.Contains(body, `"role":"system"`) {
		t.Errorf("context was not sent as a system message: %s", body)
	}
}

// TestStreamAgentResponsesIncludesContext is the responses-dialect counterpart.
func TestStreamAgentResponsesIncludesContext(t *testing.T) {
	var mu sync.Mutex
	var firstBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		if firstBody == nil {
			firstBody = body
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			"event: response.output_text.delta\ndata: {\"delta\":\"ok\"}\n\n",
			"event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1},\"output\":[]}}\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "responses",
		Model:    "test",
		APIKey:   "k",
		Context:  "ATTACHMENT-CONTEXT-BLOCK",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "x"}}},
	}, func(context.Context, ToolCall) (string, string, error) { return "", "", nil }, AgentOptions{}))

	mu.Lock()
	body := string(firstBody)
	mu.Unlock()
	if !strings.Contains(body, "ATTACHMENT-CONTEXT-BLOCK") {
		t.Errorf("responses agent request missing attachment context: %s", body)
	}
	if !strings.Contains(body, `"role":"system"`) {
		t.Errorf("context was not sent as a system message: %s", body)
	}
}

// TestStreamAgentDSMLFallback proves a reply that leaks DeepSeek's native tool
// markup is executed as a real tool call and never reaches the client.
func TestStreamAgentDSMLFallback(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var secondBody []byte
	var gotCall ToolCall

	textFrame := func(chunk string) string {
		b, _ := json.Marshal(chunk)
		return "data: {\"choices\":[{\"delta\":{\"content\":" + string(b) + "}}]}\n\n"
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			// Three runes per delta so markers straddle deltas.
			runes := []rune("Let me check." + dsmlFixtureSingle)
			frames := make([]string, 0, len(runes)/3+3)
			for i := 0; i < len(runes); i += 3 {
				end := i + 3
				if end > len(runes) {
					end = len(runes)
				}
				frames = append(frames, textFrame(string(runes[i:end])))
			}
			frames = append(frames,
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\n",
				"data: [DONE]\n\n",
			)
			writeSSE(w, frames...)
			return
		}
		mu.Lock()
		secondBody = body
		mu.Unlock()
		writeSSE(w,
			textFrame("All set."),
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	exec := func(_ context.Context, call ToolCall) (string, string, error) {
		gotCall = call
		return "RESULT", "ok", nil
	}
	events := collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "run_python"}}},
	}, exec, AgentOptions{}))

	if gotCall.Name != "run_python" || gotCall.Arguments != `{"code":"import json\nprint(1)\n"}` {
		t.Errorf("exec got %+v", gotCall)
	}
	var answer strings.Builder
	for _, ev := range events {
		if ev.Kind == EventError {
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
		if ev.Kind == EventText {
			if strings.Contains(ev.Delta, "\uFF5CDSML") {
				t.Fatalf("markup leaked to the client: %q", ev.Delta)
			}
			answer.WriteString(ev.Delta)
		}
	}
	if answer.String() != "Let me check.\n\nAll set." {
		t.Errorf("client text = %q", answer.String())
	}

	mu.Lock()
	body := string(secondBody)
	mu.Unlock()
	for _, want := range []string{`"role":"tool"`, `"tool_call_id":"call_dsml_1"`, "RESULT", `"tool_calls"`} {
		if !strings.Contains(body, want) {
			t.Errorf("second request missing %s: %s", want, body)
		}
	}
}

// TestStreamAgentDSMLUnknownTool proves a hallucinated tool name is fed back as
// an ordinary tool error instead of ending the turn.
func TestStreamAgentDSMLUnknownTool(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var secondBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			writeSSE(w,
				"data: {\"choices\":[{\"delta\":{\"content\":\"<\\uFF5CDSML\\uFF5Cinvoke name=\\\"email_list\\\">\\n</\\uFF5CDSML\\uFF5Cinvoke>\"}}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
				"data: [DONE]\n\n",
			)
			return
		}
		mu.Lock()
		secondBody = body
		mu.Unlock()
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"Sorry.\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	exec := func(_ context.Context, call ToolCall) (string, string, error) {
		return "", "", fmt.Errorf("unknown tool %q; available tools: web_search, fetch_url", call.Name)
	}
	events := collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Messages: []Message{{Role: "user", Content: "email"}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "web_search"}}},
	}, exec, AgentOptions{}))

	var sawToolError, sawFinal bool
	for _, ev := range events {
		switch {
		case ev.Kind == EventError:
			t.Fatalf("unknown tool must not be fatal: %v", ev.Err)
		case ev.Kind == EventTool && ev.Tool != nil && ev.Tool.Status == ToolError:
			sawToolError = true
		case ev.Kind == EventText && ev.Delta == "Sorry.":
			sawFinal = true
		}
	}
	if !sawToolError || !sawFinal {
		t.Errorf("toolError=%v final=%v", sawToolError, sawFinal)
	}
	mu.Lock()
	body := string(secondBody)
	mu.Unlock()
	if !strings.Contains(body, "Tool error: unknown tool") || !strings.Contains(body, "available tools: web_search") {
		t.Errorf("second request missing the tool error: %s", body)
	}
}

// TestReplayToolHistoryChat proves stored tool calls are replayed as a
// tool_calls assistant turn plus tool result messages.
func TestReplayToolHistoryChat(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client(), baseURL: srv.URL}
	collectEvents(t, c.StreamAgent(context.Background(), Params{
		Dialect: "chat",
		Model:   "test",
		APIKey:  "k",
		Messages: []Message{
			{Role: "user", Content: "digest"},
			{
				ID:      "m1",
				Role:    "assistant",
				Content: "Here is the digest.\n" + dsmlFixtureSingle, // old polluted row
				Tools: []ToolResult{
					{Name: "email_digest", Arguments: `{"range":"today"}`, Output: "DIGEST", Status: "done"},
					{Name: "run_python", Arguments: "{}", Error: "kaboom", Status: "error"},
					{Name: "still_running", Arguments: "{}", Status: "running"},
				},
			},
		},
	}, nil, AgentOptions{}))

	mu.Lock()
	got := string(body)
	mu.Unlock()
	for _, want := range []string{
		`"role":"assistant"`, "Here is the digest.", `"tool_calls"`,
		`"tool_call_id":"call_m1_0"`, "DIGEST",
		`"tool_call_id":"call_m1_1"`, "Tool error: kaboom",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("request missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "\uFF5CDSML") {
		t.Errorf("replayed history still contains leaked markup: %s", got)
	}
	if strings.Contains(got, "still_running") {
		t.Errorf("running tool must not be replayed: %s", got)
	}
	if n := strings.Count(got, `"role":"tool"`); n != 2 {
		t.Errorf("tool messages = %d, want 2: %s", n, got)
	}
}

// TestReplayToolHistoryBudget proves each replayed output and the request total
// are capped, with the newest calls kept.
func TestReplayToolHistoryBudget(t *testing.T) {
	run := func(t *testing.T, msgs []Message) string {
		t.Helper()
		var mu sync.Mutex
		var body []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			body = b
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w,
				"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
				"data: [DONE]\n\n",
			)
		}))
		defer srv.Close()
		c := &Client{hc: srv.Client(), baseURL: srv.URL}
		collectEvents(t, c.StreamAgent(context.Background(), Params{
			Dialect: "chat", Model: "test", APIKey: "k", Messages: msgs,
		}, nil, AgentOptions{}))
		mu.Lock()
		defer mu.Unlock()
		return string(body)
	}

	t.Run("single output cap", func(t *testing.T) {
		body := run(t, []Message{{
			ID: "big", Role: "assistant", Content: "ran",
			Tools: []ToolResult{{Name: "t", Arguments: "{}", Output: strings.Repeat("a", 20000), Status: "done"}},
		}})
		if !strings.Contains(body, "[truncated]") {
			t.Error("capped output is not marked")
		}
		if strings.Contains(body, strings.Repeat("a", 9000)) {
			t.Error("output was not capped")
		}
	})

	t.Run("total cap keeps newest", func(t *testing.T) {
		var msgs []Message
		for i := 0; i < 5; i++ {
			msgs = append(msgs, Message{
				ID: fmt.Sprintf("m%d", i), Role: "assistant", Content: "ran",
				Tools: []ToolResult{{
					Name: "t", Arguments: "{}", Status: "done",
					Output: fmt.Sprintf("OUT%d", i) + strings.Repeat("x", 8<<10-4),
				}},
			})
		}
		body := run(t, msgs)
		if n := strings.Count(body, `"role":"tool"`); n != 4 {
			t.Errorf("tool messages = %d, want 4 (32 KB budget)", n)
		}
		for _, want := range []string{"OUT4", "OUT3", "OUT2", "OUT1"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing recent output %s", want)
			}
		}
		if strings.Contains(body, "OUT0") {
			t.Error("oldest output exceeded the per-request budget")
		}
	})
}

// TestResponsesHistoryReplay proves the responses dialect replays stored calls
// as function_call / function_call_output items.
func TestResponsesHistoryReplay(t *testing.T) {
	input := responsesInput(Params{Messages: []Message{{
		ID: "m1", Role: "assistant", Content: "ran", Tools: []ToolResult{
			{Name: "fetch_url", Arguments: `{"url":"https://x"}`, Output: "PAGE", Status: "done"},
		},
	}}})
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{`"type":"function_call"`, `"type":"function_call_output"`, `"call_id":"call_m1_0"`, `"output":"PAGE"`, `"content":"ran"`} {
		if !strings.Contains(body, want) {
			t.Errorf("responses input missing %s: %s", want, body)
		}
	}
}
