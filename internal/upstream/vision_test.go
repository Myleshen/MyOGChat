package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// visionCapture records the request the server received.
type visionCapture struct {
	path   string
	header http.Header
	body   map[string]any
}

func newVisionServer(t *testing.T, resp string) (*Client, *visionCapture) {
	t.Helper()
	cap := &visionCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &cap.body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return &Client{hc: srv.Client(), baseURL: srv.URL}, cap
}

func TestVisionOnceChatShape(t *testing.T) {
	c, cap := newVisionServer(t, `{"choices":[{"message":{"content":"hello\nworld"}}]}`)

	got, err := c.VisionOnce(context.Background(), VisionRequest{
		Dialect:   "chat",
		Model:     "deepseek-v4-flash-vision-exp",
		SessionID: "sess-1",
		APIKey:    "secret-key",
		Prompt:    "transcribe",
		ImageURL:  "data:image/png;base64,AAAA",
	})
	if err != nil {
		t.Fatalf("VisionOnce: %v", err)
	}
	if got != "hello\nworld" {
		t.Errorf("text = %q", got)
	}
	if cap.path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", cap.path)
	}
	if h := cap.header.Get("Authorization"); h != "Bearer secret-key" {
		t.Errorf("Authorization = %q", h)
	}
	if h := cap.header.Get("x-opencode-session"); h != "sess-1" {
		t.Errorf("x-opencode-session = %q", h)
	}
	if h := cap.header.Get("User-Agent"); h != userAgent {
		t.Errorf("User-Agent = %q", h)
	}
	if v, _ := cap.body["stream"].(bool); v {
		t.Errorf("stream = %v, want false", v)
	}
	if v, _ := cap.body["model"].(string); v != "deepseek-v4-flash-vision-exp" {
		t.Errorf("model = %v", cap.body["model"])
	}
	// Default max_tokens is 2048.
	if v, _ := cap.body["max_tokens"].(float64); v != 2048 {
		t.Errorf("max_tokens = %v, want 2048", cap.body["max_tokens"])
	}

	msgs, _ := cap.body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", cap.body["messages"])
	}
	msg := msgs[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content parts = %v", msg["content"])
	}
	textPart := content[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "transcribe" {
		t.Errorf("text part = %v", textPart)
	}
	imgPart := content[1].(map[string]any)
	if imgPart["type"] != "image_url" {
		t.Fatalf("image part = %v", imgPart)
	}
	iu, ok := imgPart["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("chat image_url must be an object, got %T", imgPart["image_url"])
	}
	if iu["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image url = %v", iu["url"])
	}
	if iu["detail"] != "high" {
		t.Errorf("detail = %v, want high", iu["detail"])
	}
}

func TestVisionOnceResponsesShape(t *testing.T) {
	c, cap := newVisionServer(t, `{"output":[{"type":"message","content":[{"type":"output_text","text":"line1"},{"type":"output_text","text":"line2"}]}]}`)

	got, err := c.VisionOnce(context.Background(), VisionRequest{
		Dialect:   "responses",
		Model:     "gpt-6-luna",
		SessionID: "sess-2",
		APIKey:    "k",
		Prompt:    "transcribe",
		ImageURL:  "data:image/jpeg;base64,BBBB",
		MaxTokens: 512,
		Detail:    "low",
	})
	if err != nil {
		t.Fatalf("VisionOnce: %v", err)
	}
	if got != "line1line2" {
		t.Errorf("text = %q", got)
	}
	if cap.path != "/responses" {
		t.Errorf("path = %q, want /responses", cap.path)
	}
	if v, _ := cap.body["store"].(bool); v {
		t.Errorf("store = %v, want false", v)
	}
	if v, _ := cap.body["max_output_tokens"].(float64); v != 512 {
		t.Errorf("max_output_tokens = %v, want 512", v)
	}

	input, _ := cap.body["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", cap.body["input"])
	}
	content, _ := input[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content parts = %v", content)
	}
	if content[0].(map[string]any)["type"] != "input_text" {
		t.Errorf("text part = %v", content[0])
	}
	imgPart := content[1].(map[string]any)
	if imgPart["type"] != "input_image" {
		t.Fatalf("image part = %v", imgPart)
	}
	if imgPart["image_url"] != "data:image/jpeg;base64,BBBB" {
		t.Errorf("image_url must be a flat string, got %T", imgPart["image_url"])
	}
	if imgPart["detail"] != "low" {
		t.Errorf("detail = %v, want low", imgPart["detail"])
	}
}

func TestVisionOnceHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "bad model")
	}))
	defer srv.Close()
	c := &Client{hc: srv.Client(), baseURL: srv.URL}

	_, err := c.VisionOnce(context.Background(), VisionRequest{Dialect: "chat", Model: "m", ImageURL: "data:x"})
	if err == nil || !strings.Contains(err.Error(), "upstream HTTP 400") {
		t.Fatalf("err = %v, want an upstream HTTP 400 error", err)
	}
}

func TestVisionOnceEmptyResponse(t *testing.T) {
	c, _ := newVisionServer(t, `{"choices":[{"message":{"content":"   "}}]}`)
	if _, err := c.VisionOnce(context.Background(), VisionRequest{Dialect: "chat", Model: "m"}); err == nil {
		t.Fatal("expected an error for an empty completion")
	}
}

func TestParseResponsesVisionSkipsNonText(t *testing.T) {
	body := []byte(`{"output":[{"content":[{"type":"reasoning_text","text":"ignore"},{"type":"output_text","text":"keep"}]}]}`)
	got, err := parseResponsesVision(body)
	if err != nil {
		t.Fatalf("parseResponsesVision: %v", err)
	}
	if got != "keep" {
		t.Errorf("text = %q, want keep", got)
	}
}

// TestChatMessageObjRendersVisionParts verifies a message with inline images
// becomes a multimodal content array (5.1), while a text-only message keeps a
// plain string content.
func TestChatMessageObjRendersVisionParts(t *testing.T) {
	plain := chatMessageObj(Message{Role: "user", Content: "hi"})
	if plain["content"] != "hi" {
		t.Fatalf("plain content = %v, want the raw string", plain["content"])
	}
	withImg := chatMessageObj(Message{
		Role: "user", Content: "what is this?",
		Images: []string{"data:image/png;base64,AAAA"},
	})
	parts, ok := withImg["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts = %v", withImg["content"])
	}
	if parts[0].(map[string]any)["type"] != "text" || parts[0].(map[string]any)["text"] != "what is this?" {
		t.Errorf("text part = %v", parts[0])
	}
	img := parts[1].(map[string]any)
	iu, _ := img["image_url"].(map[string]any)
	if img["type"] != "image_url" || iu["url"] != "data:image/png;base64,AAAA" || iu["detail"] != visionDetail {
		t.Errorf("image part = %v", img)
	}
}

// TestResponsesInputObjRendersVisionParts is the responses-dialect counterpart.
func TestResponsesInputObjRendersVisionParts(t *testing.T) {
	plain := responsesInputObj(Message{Role: "user", Content: "hi"})
	if plain["content"] != "hi" {
		t.Fatalf("plain content = %v", plain["content"])
	}
	m := responsesInputObj(Message{
		Role: "user", Content: "see this",
		Images: []string{"data:image/jpeg;base64,BBBB"},
	})
	parts, ok := m["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts = %v", m["content"])
	}
	if parts[0].(map[string]any)["type"] != "input_text" {
		t.Errorf("text part = %v", parts[0])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "input_image" || img["image_url"] != "data:image/jpeg;base64,BBBB" || img["detail"] != visionDetail {
		t.Errorf("image part = %v", img)
	}
}

// TestStreamAgentChatSendsVisionParts drives the agentic chat loop (the path
// the server uses) with an image-bearing user message and asserts the inline
// image is sent as a content part.
func TestStreamAgentChatSendsVisionParts(t *testing.T) {
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
		Dialect:  "chat",
		Model:    "test",
		APIKey:   "k",
		Context:  "ATTACHMENT-TEXT",
		Messages: []Message{{Role: "user", Content: "what is this?", Images: []string{"data:image/png;base64,AAAA"}}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "x"}}},
	}, func(context.Context, ToolCall) (string, string, error) { return "", "", nil }))

	mu.Lock()
	got := string(body)
	mu.Unlock()
	if !strings.Contains(got, `"type":"image_url"`) ||
		!strings.Contains(got, `"url":"data:image/png;base64,AAAA"`) {
		t.Errorf("chat agent request missing image part: %s", got)
	}
	if !strings.Contains(got, `"detail":"high"`) {
		t.Errorf("chat agent request missing detail: %s", got)
	}
	if !strings.Contains(got, "ATTACHMENT-TEXT") {
		t.Errorf("chat agent request dropped the folded context: %s", got)
	}
}

// TestStreamAgentResponsesSendsVisionParts is the responses-dialect counterpart.
func TestStreamAgentResponsesSendsVisionParts(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
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
		Messages: []Message{{Role: "user", Content: "see", Images: []string{"data:image/jpeg;base64,BBBB"}}},
		Tools:    []ToolSpec{{Type: "function", Function: ToolFunctionSpec{Name: "x"}}},
	}, func(context.Context, ToolCall) (string, string, error) { return "", "", nil }))

	mu.Lock()
	got := string(body)
	mu.Unlock()
	if !strings.Contains(got, `"type":"input_image"`) ||
		!strings.Contains(got, `"image_url":"data:image/jpeg;base64,BBBB"`) {
		t.Errorf("responses agent request missing image part: %s", got)
	}
}
