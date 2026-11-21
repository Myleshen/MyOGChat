package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortFirstByteTimeout shrinks the watchdog for one test and restores it.
func shortFirstByteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := firstByteTimeout
	firstByteTimeout = d
	t.Cleanup(func() { firstByteTimeout = prev })
}

// silentServer accepts the request and never writes, like a provider that lists
// a model it does not actually serve. reqs counts the requests it received; the
// handler is released at test cleanup so Close never waits on a live client.
func silentServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reqs atomic.Int32
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		<-release
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })
	return ts, &reqs
}

// TestCustomProviderFirstByteTimeout verifies a silent custom provider fails
// fast with an error that names the model instead of hanging for streamTimeout.
func TestCustomProviderFirstByteTimeout(t *testing.T) {
	shortFirstByteTimeout(t, 200*time.Millisecond)
	ts, _ := silentServer(t)
	c := NewClient()

	start := time.Now()
	events := drain(t, c.Stream(context.Background(), Params{
		Dialect:  "chat",
		Model:    "z-ai/glm-5.3-flash",
		APIKey:   "nvapi-key",
		BaseURL:  ts.URL + "/v1",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("Stream took %s, want the first-byte timeout", elapsed)
	}

	var got error
	for _, ev := range events {
		if ev.Kind == EventError {
			got = ev.Err
		}
	}
	if got == nil {
		t.Fatalf("no error event in %+v", events)
	}
	if !strings.Contains(got.Error(), `model "z-ai/glm-5.3-flash"`) {
		t.Errorf("error = %q, want the model named", got)
	}
	if !strings.Contains(got.Error(), "did not respond") {
		t.Errorf("error = %q, want the first-byte message", got)
	}
}

// TestAgentDoesNotRetrySilentProvider verifies the agent path (the production
// one) gets the same watchdog and does not re-send the request without tools,
// which would double the wait.
func TestAgentDoesNotRetrySilentProvider(t *testing.T) {
	shortFirstByteTimeout(t, 150*time.Millisecond)
	ts, reqs := silentServer(t)
	c := NewClient()

	events := drain(t, c.StreamAgent(context.Background(), Params{
		Dialect:  "chat",
		Model:    "z-ai/glm-5.3-flash",
		APIKey:   "nvapi-key",
		BaseURL:  ts.URL + "/v1",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolSpec{{
			Type:     "function",
			Function: ToolFunctionSpec{Name: "lookup", Description: "lookup", Parameters: []byte(`{"type":"object"}`)},
		}},
	}, func(context.Context, ToolCall) (string, string, error) { return "", "", nil }, AgentOptions{MaxIterations: 3}))

	var got error
	for _, ev := range events {
		if ev.Kind == EventError {
			got = ev.Err
		}
	}
	if got == nil || !strings.Contains(got.Error(), `model "z-ai/glm-5.3-flash"`) {
		t.Fatalf("error = %v, want the first-byte message", got)
	}
	if n := int(reqs.Load()); n != 1 {
		t.Errorf("requests = %d, want 1 (no no-tools retry after a timeout)", n)
	}
}

// TestSlowFirstByteKeepsWholeStream verifies the watchdog stops on the first
// byte: a stream that takes longer in total than the timeout still arrives to
// the end, usage and [DONE] included.
func TestSlowFirstByteKeepsWholeStream(t *testing.T) {
	shortFirstByteTimeout(t, 400*time.Millisecond)
	const deltas = 10
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Longer than a naive short first-byte timeout would allow, shorter
		// than this test's timeout.
		time.Sleep(150 * time.Millisecond)
		for i := 0; i < deltas; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d \"}}]}\n\n", i)
			fl.Flush()
			// The stream as a whole outlives firstByteTimeout.
			time.Sleep(60 * time.Millisecond)
		}
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":10}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(ts.Close)

	start := time.Now()
	events := drain(t, NewClient().Stream(context.Background(), Params{
		Dialect:  "chat",
		Model:    "meta/llama-3.1-8b",
		APIKey:   "nvapi-key",
		BaseURL:  ts.URL + "/v1",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}))
	if elapsed := time.Since(start); elapsed <= firstByteTimeout {
		t.Fatalf("stream finished in %s, want it to outlive the %s watchdog", elapsed, firstByteTimeout)
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
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if want := fmt.Sprintf("t0 t1 t2 t3 t4 t5 t6 t7 t8 t9 "); text.String() != want {
		t.Errorf("text = %q, want %q", text.String(), want)
	}
	if usage == nil || usage.PromptTokens != 4 || usage.CompletionTokens != 10 {
		t.Errorf("usage = %+v, want the final chunk", usage)
	}
}

// TestOpenCodeIgnoresFirstByteTimeout keeps the OpenCode path unchanged: no
// watchdog, so a slow first byte is fine even when the custom-provider timeout
// is tiny.
func TestOpenCodeIgnoresFirstByteTimeout(t *testing.T) {
	shortFirstByteTimeout(t, 20*time.Millisecond)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(ts.Close)
	c := NewClient()
	c.baseURL = ts.URL

	var text string
	for _, ev := range drain(t, c.Stream(context.Background(), Params{
		Dialect: "chat", Model: "glm-5.3-flash", SessionID: "conv-4", APIKey: "oc-key",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})) {
		if ev.Kind == EventText {
			text += ev.Delta
		}
		if ev.Kind == EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if text != "ok" {
		t.Errorf("text = %q, want ok", text)
	}
}

// TestSilentProviderHonoursCallerCancel verifies a user stop while waiting for
// the first byte ends the stream quietly: the watchdog never turns a cancel
// into the timeout error.
func TestSilentProviderHonoursCallerCancel(t *testing.T) {
	shortFirstByteTimeout(t, 5*time.Second)
	ts, _ := silentServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	for _, ev := range drain(t, NewClient().Stream(ctx, Params{
		Dialect:  "chat",
		Model:    "z-ai/glm-5.3-flash",
		APIKey:   "nvapi-key",
		BaseURL:  ts.URL + "/v1",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})) {
		if ev.Kind == EventError {
			t.Fatalf("cancel produced an error event: %v", ev.Err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("stream ended after %s, want the caller cancel", elapsed)
	}
}
