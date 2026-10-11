// Package upstream implements the two OpenCode Go API dialect adapters
// (chat completions and responses) per contract §4. Both emit a common
// channel of normalized events.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BaseURL is the OpenCode Go API base (contract §4).
const BaseURL = "https://opencode.ai/zen/go/v1"

// userAgent is required by the upstream ToS on every request.
const userAgent = "myogchat/1.0"

// streamTimeout bounds one streaming upstream call (contract §4).
const streamTimeout = 5 * time.Minute

// Event kinds emitted on the stream channel.
const (
	EventReasoning = "reasoning"
	EventText      = "text"
	EventUsage     = "usage"
	EventError     = "error"
	// EventTool reports tool activity during the agentic loop (Phase 2.4).
	EventTool = "tool"
)

// Tool statuses surfaced by EventTool.
const (
	ToolRunning = "running"
	ToolDone    = "done"
	ToolError   = "error"
)

// maxToolIterations bounds the agentic tool-calling loop so a misbehaving model
// cannot loop forever.
const maxToolIterations = 4

// ToolSpec is one function exposed to the model. Function is nested to match
// the chat-completions wire format; the responses dialect flattens it.
type ToolSpec struct {
	Type     string           `json:"type"`
	Function ToolFunctionSpec `json:"function"`
}

// ToolFunctionSpec describes a callable function.
type ToolFunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolActivity is emitted on EventTool to surface tool progress to the client.
// Phase 4.1 also carries the tool's arguments, output, error text, and wall
// time so the full call can be shown (collapsibly) and persisted.
type ToolActivity struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // running | done | error
	Summary    string `json:"summary,omitempty"`
	Args       string `json:"args,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// ToolCall is one function call requested by the model. ID is the tool-call id
// for the chat dialect and the call_id for the responses dialect; ItemID is the
// responses output-item id (empty for chat).
type ToolCall struct {
	ID        string
	ItemID    string
	Name      string
	Arguments string
}

// ExecuteTool runs one requested tool call. It returns the textual result fed
// back to the model plus a short summary for the UI. A non-nil error is
// reported to the client as a failed tool event and the error text is returned
// to the model so it can recover.
type ExecuteTool func(ctx context.Context, call ToolCall) (result, summary string, err error)

// Message is a single history entry sent upstream (role + content only).
//
// Images carries optional inline image data URIs ("data:image/png;base64,…")
// attached to this message (vision input, Phase 5.1). A message with images is
// rendered dialect-specifically into multimodal content parts when a request
// payload is built (see chatMessageObj / responsesInputObj); the struct's own
// JSON tags are not used for it.
type Message struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"-"`
}

// visionDetail is the image-resolution hint sent with vision parts (Phase 5.1),
// matching VisionOnce's default.
const visionDetail = "high"

// chatMessageObj renders one history Message as the chat-completions wire
// object. A message carrying Images becomes a multimodal content array
// ({type:"text"} + {type:"image_url"}), the same shape VisionOnce uses.
func chatMessageObj(m Message) map[string]any {
	if len(m.Images) == 0 {
		return map[string]any{"role": m.Role, "content": m.Content}
	}
	parts := make([]any, 0, len(m.Images)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, url := range m.Images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url, "detail": visionDetail},
		})
	}
	return map[string]any{"role": m.Role, "content": parts}
}

// responsesInputObj is the responses-dialect counterpart: a message with Images
// becomes [{type:"input_text"}…, {type:"input_image", image_url, detail}].
func responsesInputObj(m Message) map[string]any {
	if len(m.Images) == 0 {
		return map[string]any{"role": m.Role, "content": m.Content}
	}
	parts := make([]any, 0, len(m.Images)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": m.Content})
	}
	for _, url := range m.Images {
		parts = append(parts, map[string]any{
			"type": "input_image", "image_url": url, "detail": visionDetail,
		})
	}
	return map[string]any{"role": m.Role, "content": parts}
}

// responsesInput builds the responses-dialect "input" array: the leading
// attachment context (2.3) as a system item, then each history message.
func responsesInput(p Params) []any {
	input := make([]any, 0, len(p.Messages)+1)
	if p.Context != "" {
		input = append(input, map[string]any{"role": "system", "content": p.Context})
	}
	for _, m := range p.Messages {
		input = append(input, responsesInputObj(m))
	}
	return input
}

// Usage carries token counts from the final upstream event.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
}

// Event is the normalized unit emitted by both dialect adapters.
type Event struct {
	Kind  string // "reasoning" | "text" | "usage" | "error" | "tool"
	Delta string
	Usage *Usage
	Tool  *ToolActivity
	Err   error
}

// Params describes one streaming upstream call.
type Params struct {
	Dialect   string // "chat" | "responses"
	Model     string
	Effort    string // may be empty; responses dialect defaults to "medium"
	SessionID string // conversation UUID, sent as x-opencode-session
	APIKey    string // bearer key for this request (per-user, §2.1)
	Messages  []Message
	// Instructions is the project system prompt (§8.6). Chat dialect gets it
	// as a leading system message; responses dialect as a top-level field.
	Instructions string
	// Context is leading attachment text (2.3). Chat dialect prepends it as a
	// system message; responses dialect as a leading input item.
	Context string
	// Tools are exposed to the model for the agentic loop (Phase 2.4). Empty
	// disables tool calling.
	Tools []ToolSpec
}

// Client calls the OpenCode Go API. The bearer key is supplied per request
// (per-user keys, §2.1), so a single Client serves every user.
type Client struct {
	hc      *http.Client
	baseURL string // defaults to BaseURL; overridable in tests
}

// NewClient returns a Client. The HTTP client has no overall timeout; every
// call is bounded by its context.
func NewClient() *Client {
	return &Client{hc: &http.Client{}, baseURL: BaseURL}
}

// ValidateKey checks an API key with a minimal (free-model) completion. The
// public /models endpoint does not authenticate, but completions do.
func (c *Client) ValidateKey(ctx context.Context, apiKey string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload := map[string]any{
		"model":      "longcat-2.5-preview-free",
		"max_tokens": 1,
		"stream":     false,
		"messages":   []Message{{Role: "user", Content: "hi"}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-opencode-session", "key-check")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errors.New("invalid API key")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	// Guard against gateways that return 200 with an error body.
	if bytes.Contains(snippet, []byte("\"error\"")) && !bytes.Contains(snippet, []byte("\"choices\"")) {
		return errors.New("invalid API key")
	}
	return nil
}

// Stream starts a streaming call and returns a channel of normalized events.
// The channel is closed when the stream ends; a terminal failure is delivered
// as an EventError before close. Cancel ctx to abort (e.g. client disconnect).
func (c *Client) Stream(ctx context.Context, p Params) <-chan Event {
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		ctx, cancel := context.WithTimeout(ctx, streamTimeout)
		defer cancel()
		send := func(ev Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		var err error
		if p.Dialect == "responses" {
			err = c.streamResponses(ctx, p, send)
		} else {
			err = c.streamChat(ctx, p, send)
		}
		if err != nil && ctx.Err() == nil {
			send(Event{Kind: EventError, Err: err})
		}
	}()
	return out
}

// ChatOnce performs one non-streaming chat-completions call and returns the
// assistant message content. Used by the auto-title feature (contract §3).
func (c *Client) ChatOnce(ctx context.Context, apiKey, sessionID, model string, msgs []Message, maxTokens int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"stream":     false,
	}
	resp, err := c.do(ctx, "/chat/completions", sessionID, apiKey, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode chat completion: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("empty completion")
	}
	return out.Choices[0].Message.Content, nil
}

// do issues one POST with the required headers and returns the response if
// its status is 2xx. Non-2xx yields an error with status + body snippet.
func (c *Client) do(ctx context.Context, path, sessionID, apiKey string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		resp.Body.Close()
		return nil, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return resp, nil
}

// chatChunk is a chat.completion.chunk (contract §4).
type chatChunk struct {
	Choices []struct {
		Delta struct {
			ReasoningContent string `json:"reasoning_content"`
			Content          string `json:"content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// streamChat implements the chat-completions dialect.
func (c *Client) streamChat(ctx context.Context, p Params, send func(Event) bool) error {
	// A project's instructions and the attachment context lead the request as
	// system messages (§8.6 / 2.3).
	var msgs []any
	if p.Context != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": p.Context})
	}
	if p.Instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": p.Instructions})
	}
	for _, m := range p.Messages {
		msgs = append(msgs, chatMessageObj(m))
	}
	payload := map[string]any{
		"model":          p.Model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	resp, err := c.do(ctx, "/chat/completions", p.SessionID, p.APIKey, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return scanSSE(resp.Body, func(_, data string) (bool, error) {
		if data == "[DONE]" {
			return true, nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Ignore malformed/keepalive data lines.
			return false, nil
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.ReasoningContent != "" {
				if !send(Event{Kind: EventReasoning, Delta: delta.ReasoningContent}) {
					return true, nil
				}
			}
			if delta.Content != "" {
				if !send(Event{Kind: EventText, Delta: delta.Content}) {
					return true, nil
				}
			}
		}
		if chunk.Usage != nil {
			u := &Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
			}
			if chunk.Usage.PromptTokensDetails != nil {
				u.CachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			if chunk.Usage.CompletionTokensDetails != nil {
				u.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			send(Event{Kind: EventUsage, Usage: u})
			return true, nil
		}
		return false, nil
	})
}

// streamResponses implements the responses dialect.
func (c *Client) streamResponses(ctx context.Context, p Params, send func(Event) bool) error {
	effort := p.Effort
	if effort == "" {
		effort = "medium"
	}
	payload := map[string]any{
		"model":             p.Model,
		"input":             responsesInput(p),
		"store":             false,
		"stream":            true,
		"max_output_tokens": 4096,
		"reasoning":         map[string]any{"effort": effort, "summary": "auto"},
	}
	// Project instructions go in the responses API's dedicated field (§8.6).
	if p.Instructions != "" {
		payload["instructions"] = p.Instructions
	}
	resp, err := c.do(ctx, "/responses", p.SessionID, p.APIKey, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return scanSSE(resp.Body, func(event, data string) (bool, error) {
		if data == "[DONE]" {
			return true, nil
		}
		if event == "" {
			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(data), &probe); err == nil {
				event = probe.Type
			}
		}
		switch event {
		case "response.output_text.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &d); err == nil && d.Delta != "" {
				if !send(Event{Kind: EventText, Delta: d.Delta}) {
					return true, nil
				}
			}
		case "response.reasoning_summary_text.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &d); err == nil && d.Delta != "" {
				if !send(Event{Kind: EventReasoning, Delta: d.Delta}) {
					return true, nil
				}
			}
		case "response.completed":
			var d struct {
				Response struct {
					Usage *struct {
						InputTokens        int64 `json:"input_tokens"`
						OutputTokens       int64 `json:"output_tokens"`
						InputTokensDetails *struct {
							CachedTokens int64 `json:"cached_tokens"`
						} `json:"input_tokens_details"`
						OutputTokensDetails *struct {
							ReasoningTokens int64 `json:"reasoning_tokens"`
						} `json:"output_tokens_details"`
					} `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &d); err == nil && d.Response.Usage != nil {
				u := &Usage{
					PromptTokens:     d.Response.Usage.InputTokens,
					CompletionTokens: d.Response.Usage.OutputTokens,
				}
				if d.Response.Usage.InputTokensDetails != nil {
					u.CachedTokens = d.Response.Usage.InputTokensDetails.CachedTokens
				}
				if d.Response.Usage.OutputTokensDetails != nil {
					u.ReasoningTokens = d.Response.Usage.OutputTokensDetails.ReasoningTokens
				}
				send(Event{Kind: EventUsage, Usage: u})
			}
			return true, nil
		case "response.failed", "error":
			return false, fmt.Errorf("upstream %s: %s", event, firstN(data, 200))
		}
		return false, nil
	})
}

// scanSSE reads an SSE stream and dispatches each event block to handle,
// which returns done=true to stop early. Comment lines (": ...") are skipped.
func scanSSE(body io.Reader, handle func(event, data string) (done bool, err error)) error {
	br := bufio.NewReader(body)
	var event string
	var data strings.Builder
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if data.Len() > 0 {
					done, herr := handle(event, data.String())
					if herr != nil {
						return herr
					}
					if done {
						return nil
					}
				}
				event = ""
				data.Reset()
			case strings.HasPrefix(line, ":"):
				// comment / keepalive — ignore
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				d := strings.TrimPrefix(line, "data:")
				d = strings.TrimPrefix(d, " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(d)
			}
		}
		if err != nil {
			if err == io.EOF {
				if data.Len() > 0 {
					_, herr := handle(event, data.String())
					return herr
				}
				return nil
			}
			return err
		}
	}
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
