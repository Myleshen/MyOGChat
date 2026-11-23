// Package upstream implements the four OpenCode Go API dialect adapters
// (chat completions, responses, Anthropic messages and Google
// generate-content) per contract §4. All
// emit a common channel of normalized events.
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
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// BaseURL is the OpenCode Go inference base for the OpenAI-style protocols
// (chat completions and responses). The legacy /zen/go/v1 path still routes to
// the same backend but the protocol-scoped base is canonical (contract §4).
const BaseURL = "https://opencode.ai/inference/go/openai/v1"

// ModelsURL lists the model catalog and doubles as the credential check:
// without a key it is public, with an unknown key it answers 401.
const ModelsURL = "https://opencode.ai/inference/go/v1/models"

// userAgent is required by the upstream ToS on every request.
const userAgent = "myogchat/1.0"

// streamTimeout bounds one streaming upstream call (contract §4).
const streamTimeout = 5 * time.Minute

// firstByteTimeout is how long a custom provider may take to send the first
// streamed byte. Some providers list models they do not serve and then accept
// the connection without ever answering; without this the call would sit until
// streamTimeout. A package var, not a Params field: tests shorten it and no
// caller can drift from the default.
var firstByteTimeout = 60 * time.Second

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

// maxToolIterations is the default bound on the agentic tool-calling loop so a
// misbehaving model cannot loop forever (AgentOptions.MaxIterations overrides).
const maxToolIterations = 4

// AgentOptions tunes one StreamAgent call. The zero value keeps the historical
// behaviour: maxToolIterations rounds and a streamTimeout-bounded call.
//
// The overall budget is the caller's context: StreamAgent no longer wraps the
// whole loop in one timeout, so a long agent job is bounded by its job context
// while each upstream call and each tool call gets its own limit.
type AgentOptions struct {
	// MaxIterations bounds the tool loop; 0 = maxToolIterations.
	MaxIterations int
	// CallTimeout bounds one upstream HTTP call; 0 = streamTimeout.
	CallTimeout time.Duration
	// OnStep is called before each round with the 1-based step number and the
	// iteration bound (agent-mode progress reporting). May be nil.
	OnStep func(step, max int)
}

// iterations is the effective loop bound.
func (o AgentOptions) iterations() int {
	if o.MaxIterations > 0 {
		return o.MaxIterations
	}
	return maxToolIterations
}

// callTimeout is the effective per-upstream-call limit.
func (o AgentOptions) callTimeout() time.Duration {
	if o.CallTimeout > 0 {
		return o.CallTimeout
	}
	return streamTimeout
}

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
	// ThoughtSignature is Google-only provider metadata, carried through the
	// activity so the server can persist it. It is never sent to the client.
	ThoughtSignature string `json:"-"`
}

// ToolCall is one function call requested by the model. ID is the tool-call id
// for the chat dialect and the call_id for the responses dialect; ItemID is the
// responses output-item id (empty for chat). ThoughtSignature is Google-only
// provider metadata (the opaque thoughtSignature emitted with the call), which
// every other dialect ignores.
type ToolCall struct {
	ID               string
	ItemID           string
	Name             string
	Arguments        string
	ThoughtSignature string
}

// ExecuteTool runs one requested tool call. It returns the textual result fed
// back to the model plus a short summary for the UI. A non-nil error is
// reported to the client as a failed tool event and the error text is returned
// to the model so it can recover.
type ExecuteTool func(ctx context.Context, call ToolCall) (result, summary string, err error)

// Message is a single history entry sent upstream.
//
// Images carries optional inline image data URIs ("data:image/png;base64,…")
// attached to this message (vision input, Phase 5.1). A message with images is
// rendered dialect-specifically into multimodal content parts when a request
// payload is built (see chatMessageObj / responsesInputObj); the struct's own
// JSON tags are not used for it.
//
// Tools carries the stored tool calls of an assistant message so a later turn
// can see that the tools ran and what they returned. ID is the message id used
// to build stable synthetic tool-call ids.
type Message struct {
	ID      string       `json:"-"`
	Role    string       `json:"role"`
	Content string       `json:"content"`
	Images  []string     `json:"-"`
	Tools   []ToolResult `json:"-"`
}

// ToolResult is one stored tool call replayed as conversation history, taken
// from messages.tools (2.10/4.1).
type ToolResult struct {
	Name      string
	Arguments string // JSON-encoded call arguments
	Output    string
	Error     string
	Status    string // done | error | running
	// ThoughtSignature is the Google-only signature stored with the call so a
	// reloaded conversation can echo it back; other dialects ignore it.
	ThoughtSignature string
}

// visionDetail is the image-resolution hint sent with vision parts (Phase 5.1),
// matching VisionOnce's default.
const visionDetail = "high"

// chatMessageObj renders one history Message as the chat-completions wire
// object. A message carrying Images becomes a multimodal content array
// ({type:"text"} + {type:"image_url"}), the same shape VisionOnce uses.
func chatMessageObj(m Message) map[string]any {
	if m.Role == "assistant" {
		// Old turns may still hold leaked DeepSeek markup in the DB.
		m.Content = SanitizeDSML(m.Content)
	}
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
	if m.Role == "assistant" {
		// Old turns may still hold leaked DeepSeek markup in the DB.
		m.Content = SanitizeDSML(m.Content)
	}
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
		input = append(input, responsesMessages(m)...)
	}
	return input
}

// Replay budgets for stored tool calls sent back as history. Outputs are capped
// per call and per request, newest calls first.
const (
	replayToolOutputBytes = 8 << 10
	replayToolTotalBytes  = 32 << 10
)

// replayedCall is one stored tool call rendered as a history round-trip.
type replayedCall struct {
	id, name, arguments, output string
	thoughtSignature            string
}

// chatMessages renders one history message for the chat dialect. An assistant
// turn that ran tools becomes its tool_calls request followed by one tool
// result message per call, so the model can see the round-trip on later turns.
func chatMessages(m Message) []any {
	calls := replayCalls(m)
	if len(calls) == 0 {
		return []any{chatMessageObj(m)}
	}
	obj := chatMessageObj(m)
	if strings.TrimSpace(SanitizeDSML(m.Content)) == "" && len(m.Images) == 0 {
		delete(obj, "content")
	}
	specs := make([]any, 0, len(calls))
	out := make([]any, 0, len(calls)+1)
	out = append(out, obj)
	for _, c := range calls {
		specs = append(specs, map[string]any{
			"id":   c.id,
			"type": "function",
			"function": map[string]any{
				"name":      c.name,
				"arguments": c.arguments,
			},
		})
		out = append(out, map[string]any{
			"role":         "tool",
			"tool_call_id": c.id,
			"content":      c.output,
		})
	}
	obj["tool_calls"] = specs
	return out
}

// responsesMessages is the responses-dialect counterpart: the assistant text
// item (when there is text) plus a function_call/function_call_output pair per
// stored call.
func responsesMessages(m Message) []any {
	calls := replayCalls(m)
	if len(calls) == 0 {
		return []any{responsesInputObj(m)}
	}
	out := make([]any, 0, len(calls)*2+1)
	if strings.TrimSpace(SanitizeDSML(m.Content)) != "" || len(m.Images) > 0 {
		out = append(out, responsesInputObj(m))
	}
	for _, c := range calls {
		out = append(out, responsesFunctionCallItem(ToolCall{ID: c.id, Name: c.name, Arguments: c.arguments}))
		out = append(out, responsesFunctionCallOutput(ToolCall{ID: c.id}, c.output))
	}
	return out
}

// replayCalls maps stored tools to calls the dialects can replay. Running calls
// and calls with no result are skipped rather than invented.
func replayCalls(m Message) []replayedCall {
	id := m.ID
	if id == "" {
		id = "hist"
	}
	out := make([]replayedCall, 0, len(m.Tools))
	for i, t := range m.Tools {
		if t.Status == ToolRunning || t.Name == "" || toolReplayText(t) == "" {
			continue
		}
		args := t.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, replayedCall{
			id:               fmt.Sprintf("call_%s_%d", id, i),
			name:             t.Name,
			arguments:        args,
			output:           toolReplayText(t),
			thoughtSignature: t.ThoughtSignature,
		})
	}
	return out
}

// toolReplayText is what the model sees for a stored tool: a failed tool
// replays its error text.
func toolReplayText(t ToolResult) string {
	if t.Error != "" {
		return "Tool error: " + t.Error
	}
	return t.Output
}

// budgetToolReplay caps each replayed output and the total replayed bytes per
// request. Messages are visited newest first so recent results survive.
func budgetToolReplay(msgs []Message) []Message {
	remaining := replayToolTotalBytes
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := len(out) - 1; i >= 0; i-- {
		if len(out[i].Tools) == 0 {
			continue
		}
		kept := make([]ToolResult, 0, len(out[i].Tools))
		for _, t := range out[i].Tools {
			text := toolReplayText(t)
			if t.Status == ToolRunning || text == "" {
				continue
			}
			if len(text) > replayToolOutputBytes {
				text = capBytes(text, replayToolOutputBytes)
			}
			if len(text) > remaining {
				continue
			}
			remaining -= len(text)
			t.Output = text
			t.Error = ""
			kept = append(kept, t)
		}
		out[i].Tools = kept
	}
	return out
}

// capBytes cuts s to at most max bytes on a UTF-8 boundary and marks the cut.
func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… [truncated]"
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
	Dialect   string // "chat" | "responses" | "anthropic" | "google"
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
	// MaxTokens bounds the reply for the dialects that take an explicit output
	// budget (responses, anthropic, google). 0 lets the dialect use its default.
	MaxTokens int
	// Temperature is sent only when non-nil.
	Temperature *float64
	// BaseURL overrides the OpenCode Go base for a user-added OpenAI-compatible
	// provider (§22). When set, the request goes to BaseURL+path with a Bearer
	// key and User-Agent, and x-opencode-session is NOT sent: the header belongs
	// to OpenCode. Custom models are always chat-dialect.
	BaseURL string
}

// defaultMaxTokens is the output budget used when a caller sets none. The
// server always sets Params.MaxTokens; this only serves direct callers/tests.
const defaultMaxTokens = 4096

// budget resolves the output budget for a dialect that takes one.
func (p Params) budget() int {
	if p.MaxTokens > 0 {
		return p.MaxTokens
	}
	return defaultMaxTokens
}

// Client calls the OpenCode Go API. The bearer key is supplied per request
// (per-user keys, §2.1), so a single Client serves every user.
type Client struct {
	hc           *http.Client
	baseURL      string // defaults to BaseURL; overridable in tests
	modelsURL    string // defaults to ModelsURL; overridable in tests
	messagesBase string // defaults to AnthropicBaseURL; overridable in tests
	googleURL    string // defaults to GoogleBaseURL; overridable in tests
}

// NewClient returns a Client. The HTTP client has no overall timeout; every
// call is bounded by its context.
func NewClient() *Client {
	return &Client{
		hc:           &http.Client{},
		baseURL:      BaseURL,
		modelsURL:    ModelsURL,
		messagesBase: AnthropicBaseURL,
		googleURL:    GoogleBaseURL,
	}
}

// ValidateKey checks an API key against the model catalog, which answers 401
// for a credential the gateway does not recognise. It is a catalog read, not a
// completion: it costs no usage and does not depend on any model staying in the
// catalog (the previous free-model probe broke when that model was retired).
func (c *Client) ValidateKey(ctx context.Context, apiKey string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	url := c.modelsURL
	if url == "" {
		url = ModelsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
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
		return upstreamHTTPError(resp.StatusCode, snippet)
	}
	// Guard against gateways that return 200 with an error body.
	if bytes.Contains(snippet, []byte("\"error\"")) && !bytes.Contains(snippet, []byte("\"data\"")) {
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
		p.Messages = budgetToolReplay(p.Messages)
		send := func(ev Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		var err error
		switch p.Dialect {
		case "responses":
			err = c.streamResponses(ctx, p, send)
		case "anthropic":
			err = c.streamAnthropic(ctx, p, send)
		case "google":
			err = c.streamGoogle(ctx, p, send)
		default:
			err = c.streamChat(ctx, p, send)
		}
		if err != nil && ctx.Err() == nil {
			send(Event{Kind: EventError, Err: err})
		}
	}()
	return out
}

// ChatOnce performs one non-streaming chat-completions call on the Go endpoint
// and returns the assistant message content (used for image-prompt enhancement
// and auto-titles).
func (c *Client) ChatOnce(ctx context.Context, apiKey, sessionID, model string, msgs []Message, maxTokens int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"stream":     false,
	}
	resp, err := c.do(ctx, "", "/chat/completions", sessionID, apiKey, payload)
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

// do issues one POST against the Go base (or baseURL when set) with the required
// headers and returns the response if its status is 2xx. Non-2xx yields an error
// with status + body snippet. x-opencode-session is sent only to OpenCode itself:
// a custom provider (§22) never receives it.
func (c *Client) do(ctx context.Context, baseURL, path, sessionID, apiKey string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	base := c.baseURL
	if baseURL != "" {
		base = strings.TrimRight(baseURL, "/")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if baseURL == "" {
		req.Header.Set("x-opencode-session", sessionID)
	}
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

// silentProviderError reports a custom provider that accepted the request and
// then sent nothing within firstByteTimeout. It is a distinct type so the agent
// tool loop does not retry it (the no-tools retry would double the wait).
type silentProviderError struct {
	model   string
	timeout time.Duration
}

func (e silentProviderError) Error() string {
	d := fmt.Sprintf("%ds", int64(e.timeout/time.Second))
	if e.timeout < time.Second {
		d = e.timeout.String()
	}
	return fmt.Sprintf("provider did not respond within %s (model %q); the model may not be available — try another model", d, e.model)
}

// isSilentProvider reports a first-byte timeout, which must not be retried.
func isSilentProvider(err error) bool {
	var silent silentProviderError
	return errors.As(err, &silent)
}

// openChatStream posts one chat-completions request. For a custom provider
// (BaseURL set) it also bounds the wait for the first response byte: the timer
// is stopped by the first byte, so the rest of the stream is unbounded and long
// replies are never truncated (a whole-request Timeout would cut them). OpenCode
// turns take exactly the old path.
//
// shortcut: only the first byte is bounded, so a provider that sends headers or
// keepalives and then stalls still waits for streamTimeout; add an idle-read
// deadline if that ever shows up.
func (c *Client) openChatStream(ctx context.Context, p Params, payload any) (*http.Response, error) {
	if p.BaseURL == "" {
		return c.do(ctx, "", "/chat/completions", p.SessionID, p.APIKey, payload)
	}
	parent := ctx
	ctx, cancel := context.WithCancel(parent)
	var fired atomic.Bool
	timer := time.AfterFunc(firstByteTimeout, func() {
		fired.Store(true)
		cancel()
	})
	resp, err := c.do(ctx, p.BaseURL, "/chat/completions", p.SessionID, p.APIKey, payload)
	if err != nil {
		watchdogFired := !timer.Stop() && fired.Load() && parent.Err() == nil
		cancel()
		if watchdogFired {
			return nil, silentProviderError{model: p.Model, timeout: firstByteTimeout}
		}
		return nil, err
	}
	resp.Body = &firstByteBody{
		ReadCloser: resp.Body,
		timer:      timer,
		cancel:     cancel,
		parent:     parent,
		fired:      &fired,
		err:        silentProviderError{model: p.Model, timeout: firstByteTimeout},
	}
	return resp, nil
}

// firstByteBody stops the first-byte watchdog as soon as a byte arrives and
// reports the timeout error if the watchdog cancelled a silent body. Close
// releases the derived context.
type firstByteBody struct {
	io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
	parent context.Context
	fired  *atomic.Bool
	err    error
	once   sync.Once
}

func (b *firstByteBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		// First byte arrived: the rest of the stream must be allowed to run.
		b.once.Do(func() { b.timer.Stop() })
	}
	if err != nil && b.fired.Load() && b.parent.Err() == nil {
		return n, b.err
	}
	return n, err
}

func (b *firstByteBody) Close() error {
	b.once.Do(func() { b.timer.Stop() })
	b.cancel()
	return b.ReadCloser.Close()
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
		msgs = append(msgs, chatMessages(m)...)
	}
	payload := map[string]any{
		"model":          p.Model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	resp, err := c.openChatStream(ctx, p, payload)
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
		"max_output_tokens": p.budget(),
		"reasoning":         map[string]any{"effort": effort, "summary": "auto"},
	}
	// Project instructions go in the responses API's dedicated field (§8.6).
	if p.Instructions != "" {
		payload["instructions"] = p.Instructions
	}
	resp, err := c.do(ctx, p.BaseURL, "/responses", p.SessionID, p.APIKey, payload)
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

// upstreamHTTPError renders a non-2xx upstream response as one line: the
// upstream's own message when the body is a JSON error envelope, else a
// truncated body snippet. The status code always survives.
func upstreamHTTPError(status int, body []byte) error {
	if msg := upstreamJSONMessage(body); msg != "" {
		return fmt.Errorf("upstream HTTP %d: %s", status, msg)
	}
	return fmt.Errorf("upstream HTTP %d: %s", status, firstN(strings.TrimSpace(string(body)), 200))
}

// upstreamJSONMessage digs the human message out of the error envelopes the
// dialects receive: {"error":{"message"}}, {"error":{"type","message"}},
// Anthropic's {"type":"error","error":{…}}, Google's
// {"error":{"code","message","status"}}, and a bare {"message"}. Empty for
// anything else, including an HTML error page.
func upstreamJSONMessage(body []byte) string {
	var env struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	if msg := oneLine(env.Message); msg != "" {
		return msg
	}
	var inner struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(env.Error, &inner) != nil {
		return ""
	}
	return oneLine(inner.Message)
}

// oneLine folds a message onto a single line and caps it at 200 characters, so
// one upstream failure stays one readable error.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}
