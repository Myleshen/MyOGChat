// Package upstream implements the two OpenCode Go API dialect adapters
// (chat completions and responses) per contract §4. Both emit a common
// channel of normalized events.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
)

// Message is a single history entry sent upstream (role + content only).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	Kind  string // "reasoning" | "text" | "usage" | "error"
	Delta string
	Usage *Usage
	Err   error
}

// Params describes one streaming upstream call.
type Params struct {
	Dialect   string // "chat" | "responses"
	Model     string
	Effort    string // may be empty; responses dialect defaults to "medium"
	SessionID string // conversation UUID, sent as x-opencode-session
	Messages  []Message
	// Instructions is the project system prompt (§8.6). Chat dialect gets it
	// as a leading system message; responses dialect as a top-level field.
	Instructions string
}

// Client calls the OpenCode Go API.
type Client struct {
	apiKey string
	hc     *http.Client
}

// NewClient returns a Client using apiKey as the Bearer token. The HTTP
// client has no overall timeout; every call is bounded by its context.
func NewClient(apiKey string) *Client {
	return &Client{apiKey: apiKey, hc: &http.Client{}}
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
func (c *Client) ChatOnce(ctx context.Context, sessionID, model string, msgs []Message, maxTokens int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"stream":     false,
	}
	resp, err := c.do(ctx, "/chat/completions", sessionID, payload)
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
func (c *Client) do(ctx context.Context, path, sessionID string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		PromptTokensDetails     *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// streamChat implements the chat-completions dialect.
func (c *Client) streamChat(ctx context.Context, p Params, send func(Event) bool) error {
	// A project's instructions are prepended as a system message (§8.6).
	msgs := p.Messages
	if p.Instructions != "" {
		msgs = append([]Message{{Role: "system", Content: p.Instructions}}, msgs...)
	}
	payload := map[string]any{
		"model":          p.Model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	resp, err := c.do(ctx, "/chat/completions", p.SessionID, payload)
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
		"input":             p.Messages,
		"store":             false,
		"stream":            true,
		"max_output_tokens": 4096,
		"reasoning":         map[string]any{"effort": effort, "summary": "auto"},
	}
	// Project instructions go in the responses API's dedicated field (§8.6).
	if p.Instructions != "" {
		payload["instructions"] = p.Instructions
	}
	resp, err := c.do(ctx, "/responses", p.SessionID, payload)
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
						InputTokens         int64 `json:"input_tokens"`
						OutputTokens        int64 `json:"output_tokens"`
						InputTokensDetails  *struct {
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
