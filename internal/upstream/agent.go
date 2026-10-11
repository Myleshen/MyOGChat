package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// StreamAgent performs an agentic streaming call (Phase 2.4). It streams the
// assistant reply, executes any tool calls the model requests via exec, appends
// the results to the conversation, and repeats until the model produces a final
// answer or maxToolIterations is reached.
//
// It emits the same normalized events as Stream, plus EventTool activity as
// tools run, and a single aggregated EventUsage (summed across rounds) at the
// end so cost accounting reflects the whole turn.
func (c *Client) StreamAgent(ctx context.Context, p Params, exec ExecuteTool) <-chan Event {
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
			err = c.agentResponses(ctx, p, exec, send)
		} else {
			err = c.agentChat(ctx, p, exec, send)
		}
		if err != nil && ctx.Err() == nil {
			send(Event{Kind: EventError, Err: err})
		}
	}()
	return out
}

// agentChat runs the agentic loop for the chat-completions dialect.
func (c *Client) agentChat(ctx context.Context, p Params, exec ExecuteTool, send func(Event) bool) error {
	msgs := make([]any, 0, len(p.Messages)+2)
	if p.Context != "" {
		// Attachment text (2.3) is leading context, exactly as in streamChat.
		msgs = append(msgs, map[string]any{"role": "system", "content": p.Context})
	}
	if p.Instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": p.Instructions})
	}
	for _, m := range p.Messages {
		msgs = append(msgs, chatMessageObj(m))
	}

	var total Usage
	for iter := 0; iter < maxToolIterations; iter++ {
		// The last iteration runs without tools so the model must answer.
		useTools := len(p.Tools) > 0 && iter < maxToolIterations-1
		payload := map[string]any{
			"model":          p.Model,
			"messages":       msgs,
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
		}
		if useTools {
			payload["tools"] = p.Tools
			payload["tool_choice"] = "auto"
		}

		resp, err := c.do(ctx, "/chat/completions", p.SessionID, p.APIKey, payload)
		if err != nil && useTools && iter == 0 {
			// A model that rejects tool calling must not break the chat: retry
			// once without tools and answer normally.
			delete(payload, "tools")
			delete(payload, "tool_choice")
			resp, err = c.do(ctx, "/chat/completions", p.SessionID, p.APIKey, payload)
		}
		if err != nil {
			return err
		}

		var text strings.Builder
		var acc chatToolAccumulator
		var round Usage
		var hasRound bool
		err = scanSSE(resp.Body, func(_, data string) (bool, error) {
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
					text.WriteString(delta.Content)
					if !send(Event{Kind: EventText, Delta: delta.Content}) {
						return true, nil
					}
				}
				for _, tc := range delta.ToolCalls {
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					acc.addDelta(idx, tc.ID, tc.Function.Name, tc.Function.Arguments)
				}
			}
			if chunk.Usage != nil {
				round = Usage{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
				}
				if chunk.Usage.PromptTokensDetails != nil {
					round.CachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				if chunk.Usage.CompletionTokensDetails != nil {
					round.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
				}
				hasRound = true
			}
			return false, nil
		})
		resp.Body.Close()
		if err != nil {
			return err
		}
		if hasRound {
			total.add(&round)
		}

		calls := acc.result()
		if len(calls) == 0 || exec == nil {
			send(Event{Kind: EventUsage, Usage: &total})
			return nil
		}

		msgs = append(msgs, chatAssistantToolMessage(text.String(), calls))
		for _, call := range calls {
			result := runTool(ctx, exec, send, call)
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": call.ID,
				"content":      result,
			})
		}
	}
	// Iteration bound reached; report what we have.
	send(Event{Kind: EventUsage, Usage: &total})
	return nil
}

// agentResponses runs the agentic loop for the responses dialect.
func (c *Client) agentResponses(ctx context.Context, p Params, exec ExecuteTool, send func(Event) bool) error {
	input := make([]any, 0, len(p.Messages)+2)
	if p.Context != "" {
		// Attachment text (2.3) is leading context, exactly as in streamResponses.
		input = append(input, map[string]any{"role": "system", "content": p.Context})
	}
	for _, m := range p.Messages {
		input = append(input, responsesInputObj(m))
	}

	effort := p.Effort
	if effort == "" {
		effort = "medium"
	}

	var total Usage
	for iter := 0; iter < maxToolIterations; iter++ {
		// The last iteration runs without tools so the model must answer.
		useTools := len(p.Tools) > 0 && iter < maxToolIterations-1
		payload := map[string]any{
			"model":             p.Model,
			"input":             input,
			"store":             false,
			"stream":            true,
			"max_output_tokens": 4096,
			"reasoning":         map[string]any{"effort": effort, "summary": "auto"},
		}
		if p.Instructions != "" {
			payload["instructions"] = p.Instructions
		}
		if useTools {
			payload["tools"] = responsesToolSpecs(p.Tools)
		}

		resp, err := c.do(ctx, "/responses", p.SessionID, p.APIKey, payload)
		if err != nil && useTools && iter == 0 {
			delete(payload, "tools")
			resp, err = c.do(ctx, "/responses", p.SessionID, p.APIKey, payload)
		}
		if err != nil {
			return err
		}

		var acc responsesToolAccumulator
		var round Usage
		var hasRound bool
		err = scanSSE(resp.Body, func(event, data string) (bool, error) {
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
			case "response.output_item.added":
				var d struct {
					Item functionCallItem `json:"item"`
				}
				if err := json.Unmarshal([]byte(data), &d); err == nil && d.Item.Type == "function_call" {
					acc.added(d.Item.ID, d.Item.CallID, d.Item.Name, d.Item.Arguments)
				}
			case "response.function_call_arguments.delta":
				var d struct {
					ItemID string `json:"item_id"`
					Delta  string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(data), &d); err == nil {
					acc.argsDelta(d.ItemID, d.Delta)
				}
			case "response.function_call_arguments.done":
				var d struct {
					ItemID    string `json:"item_id"`
					Arguments string `json:"arguments"`
				}
				if err := json.Unmarshal([]byte(data), &d); err == nil {
					acc.argsDone(d.ItemID, d.Arguments)
				}
			case "response.output_item.done":
				var d struct {
					Item functionCallItem `json:"item"`
				}
				if err := json.Unmarshal([]byte(data), &d); err == nil && d.Item.Type == "function_call" {
					acc.done(d.Item.ID, d.Item.CallID, d.Item.Name, d.Item.Arguments)
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
						Output []functionCallItem `json:"output"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(data), &d); err == nil {
					if u := d.Response.Usage; u != nil {
						round = Usage{
							PromptTokens:     u.InputTokens,
							CompletionTokens: u.OutputTokens,
						}
						if u.InputTokensDetails != nil {
							round.CachedTokens = u.InputTokensDetails.CachedTokens
						}
						if u.OutputTokensDetails != nil {
							round.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
						}
						hasRound = true
					}
					// The completed response is authoritative for function calls.
					for _, item := range d.Response.Output {
						if item.Type == "function_call" {
							acc.done(item.ID, item.CallID, item.Name, item.Arguments)
						}
					}
				}
				return true, nil
			case "response.failed", "error":
				return false, fmt.Errorf("upstream %s: %s", event, firstN(data, 200))
			}
			return false, nil
		})
		resp.Body.Close()
		if err != nil {
			return err
		}
		if hasRound {
			total.add(&round)
		}

		calls := acc.result()
		if len(calls) == 0 || exec == nil {
			send(Event{Kind: EventUsage, Usage: &total})
			return nil
		}
		for _, call := range calls {
			input = append(input, responsesFunctionCallItem(call))
			result := runTool(ctx, exec, send, call)
			input = append(input, responsesFunctionCallOutput(call, result))
		}
	}
	send(Event{Kind: EventUsage, Usage: &total})
	return nil
}

// functionCallItem is the responses-dialect function_call output item.
type functionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// runTool executes one tool call, emitting running/done/error activity events.
// Tool failures are returned to the model as text so it can recover.
func runTool(ctx context.Context, exec ExecuteTool, send func(Event) bool, call ToolCall) string {
	send(Event{Kind: EventTool, Tool: &ToolActivity{
		Name: call.Name, Status: ToolRunning, Args: call.Arguments,
	}})
	start := time.Now()
	result, summary, err := exec(ctx, call)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		send(Event{Kind: EventTool, Tool: &ToolActivity{
			Name: call.Name, Status: ToolError, Summary: firstN(err.Error(), 200),
			Error: err.Error(), DurationMs: elapsed,
		}})
		return "Tool error: " + err.Error()
	}
	send(Event{Kind: EventTool, Tool: &ToolActivity{
		Name: call.Name, Status: ToolDone, Summary: summary,
		Output: result, DurationMs: elapsed,
	}})
	return result
}

// add accumulates token counts across tool rounds.
func (u *Usage) add(o *Usage) {
	if o == nil {
		return
	}
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.CachedTokens += o.CachedTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// chatAssistantToolMessage renders an assistant turn that requested tools.
func chatAssistantToolMessage(text string, calls []ToolCall) map[string]any {
	toolCalls := make([]any, 0, len(calls))
	for _, c := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.Arguments,
			},
		})
	}
	m := map[string]any{"role": "assistant", "tool_calls": toolCalls}
	if strings.TrimSpace(text) != "" {
		m["content"] = text
	}
	return m
}

// responsesToolSpecs flattens tool specs into the responses wire format.
func responsesToolSpecs(tools []ToolSpec) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Function.Name,
			"description": t.Function.Description,
			"parameters":  t.Function.Parameters,
		})
	}
	return out
}

func responsesFunctionCallItem(call ToolCall) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"call_id":   call.ID,
		"name":      call.Name,
		"arguments": call.Arguments,
	}
}

func responsesFunctionCallOutput(call ToolCall, output string) map[string]any {
	return map[string]any{
		"type":    "function_call_output",
		"call_id": call.ID,
		"output":  output,
	}
}

// ---------- streamed tool-call accumulation ----------

// toolCallFragment is a partially received function call.
type toolCallFragment struct {
	id   string
	name string
	args strings.Builder
}

// chatToolAccumulator reassembles streamed chat-completions delta.tool_calls,
// which arrive as fragments keyed by index.
type chatToolAccumulator struct {
	byIndex map[int]*toolCallFragment
	order   []int
}

func (a *chatToolAccumulator) addDelta(index int, id, name, args string) {
	if a.byIndex == nil {
		a.byIndex = map[int]*toolCallFragment{}
	}
	f, ok := a.byIndex[index]
	if !ok {
		f = &toolCallFragment{}
		a.byIndex[index] = f
		a.order = append(a.order, index)
	}
	if id != "" {
		f.id = id
	}
	if name != "" {
		f.name = name
	}
	if args != "" {
		f.args.WriteString(args)
	}
}

func (a *chatToolAccumulator) result() []ToolCall {
	calls := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		f := a.byIndex[idx]
		if f.name == "" {
			continue
		}
		calls = append(calls, ToolCall{ID: f.id, Name: f.name, Arguments: f.args.String()})
	}
	return calls
}

// responsesToolAccumulator reassembles streamed responses function-call items,
// keyed by output-item id.
type responsesToolAccumulator struct {
	byItem map[string]*toolCallFragment
	order  []string
}

func (a *responsesToolAccumulator) ensure(itemID string) *toolCallFragment {
	if a.byItem == nil {
		a.byItem = map[string]*toolCallFragment{}
	}
	f, ok := a.byItem[itemID]
	if !ok {
		f = &toolCallFragment{}
		a.byItem[itemID] = f
		a.order = append(a.order, itemID)
	}
	return f
}

func (a *responsesToolAccumulator) added(itemID, callID, name, args string) {
	f := a.ensure(itemID)
	if callID != "" {
		f.id = callID
	}
	if name != "" {
		f.name = name
	}
	if args != "" {
		f.args.WriteString(args)
	}
}

func (a *responsesToolAccumulator) argsDelta(itemID, delta string) {
	if delta == "" {
		return
	}
	a.ensure(itemID).args.WriteString(delta)
}

func (a *responsesToolAccumulator) argsDone(itemID, args string) {
	if args == "" {
		return
	}
	f := a.ensure(itemID)
	f.args.Reset()
	f.args.WriteString(args)
}

func (a *responsesToolAccumulator) done(itemID, callID, name, args string) {
	f := a.ensure(itemID)
	if callID != "" {
		f.id = callID
	}
	if name != "" {
		f.name = name
	}
	if args != "" {
		f.args.Reset()
		f.args.WriteString(args)
	}
}

func (a *responsesToolAccumulator) result() []ToolCall {
	calls := make([]ToolCall, 0, len(a.order))
	for _, itemID := range a.order {
		f := a.byItem[itemID]
		if f.name == "" {
			continue
		}
		calls = append(calls, ToolCall{ID: f.id, ItemID: itemID, Name: f.name, Arguments: f.args.String()})
	}
	return calls
}
