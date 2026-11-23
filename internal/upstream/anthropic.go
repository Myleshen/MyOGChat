package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicBaseURL is the OpenCode Go base for the Anthropic Messages dialect.
// It is identical for streaming and non-streaming calls; both POST /messages.
const AnthropicBaseURL = "https://opencode.ai/inference/go/anthropic/v1"

// anthropicVersion pins the Messages API version; the endpoint rejects calls
// without it.
const anthropicVersion = "2023-06-01"

// doAnthropic POSTs one Messages request. The gateway accepts the key in
// either x-api-key or Authorization (probed anonymous, 2026-11-21), so both
// are sent: x-api-key is what native Anthropic clients use, and Authorization
// keeps parity with the other dialects.
func (c *Client) doAnthropic(ctx context.Context, p Params, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	base := c.messagesBase
	if base == "" {
		base = AnthropicBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("x-opencode-session", p.SessionID)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		resp.Body.Close()
		return nil, upstreamHTTPError(resp.StatusCode, snippet)
	}
	return resp, nil
}

// anthropicSystem folds the attachment context and the project instructions
// into the single top-level system string Messages takes.
func anthropicSystem(p Params) string {
	parts := make([]string, 0, 2)
	if p.Context != "" {
		parts = append(parts, p.Context)
	}
	if p.Instructions != "" {
		parts = append(parts, p.Instructions)
	}
	return strings.Join(parts, "\n\n")
}

// anthropicPayload builds one request body from the history accumulated so far.
func anthropicPayload(p Params, msgs []any, useTools bool) map[string]any {
	payload := map[string]any{
		"model":      p.Model,
		"messages":   msgs,
		"max_tokens": p.budget(),
		"stream":     true,
	}
	if sys := anthropicSystem(p); sys != "" {
		payload["system"] = sys
	}
	if p.Temperature != nil {
		payload["temperature"] = *p.Temperature
	}
	if useTools {
		payload["tools"] = anthropicToolSpecs(p.Tools)
	}
	return payload
}

// anthropicToolSpecs renders ToolSpec in the Messages wire format: a flat
// name/description/input_schema object.
func anthropicToolSpecs(tools []ToolSpec) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":         t.Function.Name,
			"description":  t.Function.Description,
			"input_schema": t.Function.Parameters,
		})
	}
	return out
}

// anthropicImageBlock turns a "data:<media>;base64,<data>" URI into an image
// content block. Anything else is dropped rather than sent malformed.
func anthropicImageBlock(uri string) map[string]any {
	media, data, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ";base64,")
	if !ok || media == "" || data == "" {
		return nil
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": media, "data": data},
	}
}

// anthropicInput is a tool_use input object. The API rejects a JSON string or
// an invalid document, so anything unexpected becomes {}.
func anthropicInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return json.RawMessage("{}")
}

// anthropicMessages renders one history message. As in the other dialects a
// stored tool round becomes the assistant's tool_use blocks followed by a user
// turn carrying the matching tool_result blocks (the Messages API requires the
// results in the very next user turn).
func anthropicMessages(m Message) []any {
	if m.Role == "assistant" {
		// Old turns may still hold leaked DeepSeek markup in the DB.
		m.Content = SanitizeDSML(m.Content)
	}
	parts := make([]any, 0, len(m.Images)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, uri := range m.Images {
		if block := anthropicImageBlock(uri); block != nil {
			parts = append(parts, block)
		}
	}

	calls := replayCalls(m)
	if len(calls) == 0 || m.Role != "assistant" {
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"type": "text", "text": ""})
		}
		return []any{map[string]any{"role": m.Role, "content": parts}}
	}

	out := make([]any, 0, len(calls)+1)
	for _, c := range calls {
		parts = append(parts, map[string]any{
			"type": "tool_use", "id": c.id, "name": c.name, "input": anthropicInput(c.arguments),
		})
	}
	out = append(out, map[string]any{"role": "assistant", "content": parts})

	results := make([]any, 0, len(calls))
	for _, c := range calls {
		results = append(results, map[string]any{
			"type": "tool_result", "tool_use_id": c.id, "content": c.output,
		})
	}
	return append(out, map[string]any{"role": "user", "content": results})
}

// anthropicAssistantToolMessage renders the assistant turn this loop just
// streamed: its text (if any) plus one tool_use block per requested call.
func anthropicAssistantToolMessage(text string, calls []ToolCall) []any {
	parts := make([]any, 0, len(calls)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, c := range calls {
		parts = append(parts, map[string]any{
			"type": "tool_use", "id": c.ID, "name": c.Name, "input": anthropicInput(c.Arguments),
		})
	}
	return []any{map[string]any{"role": "assistant", "content": parts}}
}

// anthropicTurn is what one streamed Messages turn produced.
type anthropicTurn struct {
	Usage    Usage
	hasUsage bool // the turn reported usage
	toolAcc  chatToolAccumulator
}

// calls returns the tool_use blocks the turn requested, in stream order.
func (t *anthropicTurn) calls() []ToolCall { return t.toolAcc.result() }

// scanAnthropic consumes one Messages SSE stream, forwarding text and thinking
// deltas to send and accumulating tool_use blocks plus usage. send returning
// false stops early (client gone).
func scanAnthropic(body io.Reader, send func(Event) bool) (*anthropicTurn, error) {
	type usageObj struct {
		InputTokens     int64 `json:"input_tokens"`
		OutputTokens    int64 `json:"output_tokens"`
		CacheReadTokens int64 `json:"cache_read_input_tokens"`
	}
	turn := &anthropicTurn{}
	err := scanSSE(body, func(event, data string) (bool, error) {
		if event == "" {
			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(data), &probe); err == nil {
				event = probe.Type
			}
		}
		switch event {
		case "message_start":
			var d struct {
				Message struct {
					Usage usageObj `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &d); err == nil {
				turn.Usage.PromptTokens = d.Message.Usage.InputTokens
				turn.Usage.CachedTokens = d.Message.Usage.CacheReadTokens
				turn.hasUsage = true
			}
		case "content_block_start":
			var d struct {
				Index int `json:"index"`
				// The first text of a text/thinking block may arrive here.
				ContentBlock struct {
					Type     string `json:"type"`
					ID       string `json:"id"`
					Name     string `json:"name"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return false, nil
			}
			switch d.ContentBlock.Type {
			case "tool_use":
				// Content-block index is what the chat accumulator keys on.
				turn.toolAcc.addDelta(d.Index, d.ContentBlock.ID, d.ContentBlock.Name, "")
			case "text":
				if d.ContentBlock.Text != "" && !send(Event{Kind: EventText, Delta: d.ContentBlock.Text}) {
					return true, nil
				}
			case "thinking":
				if d.ContentBlock.Thinking != "" &&
					!send(Event{Kind: EventReasoning, Delta: d.ContentBlock.Thinking}) {
					return true, nil
				}
			}
		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return false, nil
			}
			switch d.Delta.Type {
			case "text_delta":
				if d.Delta.Text != "" && !send(Event{Kind: EventText, Delta: d.Delta.Text}) {
					return true, nil
				}
			case "thinking_delta":
				if d.Delta.Thinking != "" && !send(Event{Kind: EventReasoning, Delta: d.Delta.Thinking}) {
					return true, nil
				}
			case "input_json_delta":
				turn.toolAcc.addDelta(d.Index, "", "", d.Delta.PartialJSON)
			}
		case "message_delta":
			// message_delta carries the cumulative output count; input counts
			// are repeated here by newer gateway versions.
			var d struct {
				Usage usageObj `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &d); err == nil {
				turn.Usage.CompletionTokens = d.Usage.OutputTokens
				if d.Usage.InputTokens > 0 {
					turn.Usage.PromptTokens = d.Usage.InputTokens
				}
				if d.Usage.CacheReadTokens > 0 {
					turn.Usage.CachedTokens = d.Usage.CacheReadTokens
				}
				turn.hasUsage = true
			}
		case "message_stop":
			return true, nil
		case "ping":
			// keepalive
		case "error":
			var d struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &d); err == nil && d.Error.Message != "" {
				return false, fmt.Errorf("upstream %s: %s", d.Error.Type, d.Error.Message)
			}
			return false, fmt.Errorf("upstream error: %s", firstN(data, 200))
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return turn, nil
}

// streamAnthropic implements the Messages dialect for a single streaming call.
func (c *Client) streamAnthropic(ctx context.Context, p Params, send func(Event) bool) error {
	msgs := make([]any, 0, len(p.Messages))
	for _, m := range p.Messages {
		msgs = append(msgs, anthropicMessages(m)...)
	}
	resp, err := c.doAnthropic(ctx, p, anthropicPayload(p, msgs, false))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	turn, err := scanAnthropic(resp.Body, send)
	if err != nil {
		return err
	}
	if turn.hasUsage {
		send(Event{Kind: EventUsage, Usage: &turn.Usage})
	}
	return nil
}

// agentAnthropic runs the agentic loop for the Messages dialect.
func (c *Client) agentAnthropic(ctx context.Context, p Params, exec ExecuteTool, opts AgentOptions, send func(Event) bool) error {
	msgs := make([]any, 0, len(p.Messages)+2)
	for _, m := range p.Messages {
		msgs = append(msgs, anthropicMessages(m)...)
	}

	maxIter := opts.iterations()
	var total Usage
	var streamed strings.Builder  // text emitted this turn, across rounds
	var reasoning strings.Builder // reasoning emitted this turn, across rounds
	for iter := 0; iter < maxIter; iter++ {
		if opts.OnStep != nil {
			opts.OnStep(iter+1, maxIter)
		}
		// The last iteration runs without tools so the model must answer.
		useTools := len(p.Tools) > 0 && iter < maxIter-1
		payload := anthropicPayload(p, msgs, useTools)

		roundCtx, cancelRound := context.WithTimeout(ctx, opts.callTimeout())
		resp, err := c.doAnthropic(roundCtx, p, payload)
		if err != nil && useTools && iter == 0 {
			// A model that rejects tool calling must not break the chat: retry
			// once without tools and answer normally.
			resp, err = c.doAnthropic(roundCtx, p, anthropicPayload(p, msgs, false))
		}
		if err != nil {
			cancelRound()
			return err
		}

		var text strings.Builder
		roundText := false
		roundReasoning := false
		roundSend := func(ev Event) bool {
			switch ev.Kind {
			case EventReasoning:
				if !roundReasoning {
					ev.Delta = roundSeparator(reasoning.String()) + ev.Delta
					roundReasoning = true
				}
				reasoning.WriteString(ev.Delta)
			case EventText:
				if !roundText {
					ev.Delta = roundSeparator(streamed.String()) + ev.Delta
					roundText = true
				}
				text.WriteString(ev.Delta)
				streamed.WriteString(ev.Delta)
			}
			return send(ev)
		}
		turn, err := scanAnthropic(resp.Body, roundSend)
		resp.Body.Close()
		cancelRound()
		if err != nil {
			return err
		}
		if turn.hasUsage {
			total.add(&turn.Usage)
		}

		calls := turn.calls()
		if len(calls) == 0 || exec == nil {
			send(Event{Kind: EventUsage, Usage: &total})
			return nil
		}

		msgs = append(msgs, anthropicAssistantToolMessage(text.String(), calls)...)
		results := make([]any, 0, len(calls))
		for _, call := range calls {
			results = append(results, map[string]any{
				"type":        "tool_result",
				"tool_use_id": call.ID,
				"content":     runTool(ctx, exec, send, call),
			})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": results})
	}
	// Iteration bound reached; report what we have.
	send(Event{Kind: EventUsage, Usage: &total})
	return nil
}
