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
)

// GoogleBaseURL is the Google Generative Language base on the Go gateway:
// POST {base}/models/{model}:streamGenerateContent?alt=sse (contract §4).
const GoogleBaseURL = "https://opencode.ai/inference/go/google/v1beta"

// googlePart is one content part of a Google GenerateContent request/response.
// ThoughtSignature is Gemini's opaque signature for the part; a functionCall
// part must carry it back on every later request or tool calling fails.
type googlePart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	FunctionCall     *googleFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *googleFunctionResponse `json:"functionResponse,omitempty"`
	InlineData       *googleInlineData       `json:"inlineData,omitempty"`
}

type googleFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type googleFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type googleInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

// googleContent is one turn of the Google contents array.
type googleContent struct {
	Role  string       `json:"role"`
	Parts []googlePart `json:"parts"`
}

// googleResponse is one streamed GenerateContentResponse.
type googleResponse struct {
	Candidates    []googleCandidate `json:"candidates"`
	UsageMetadata *googleUsage      `json:"usageMetadata"`
	Error         *struct {
		Status  string `json:"status"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type googleCandidate struct {
	Content struct {
		Role  string       `json:"role"`
		Parts []googlePart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

// googleUsage maps usageMetadata onto our token counts. promptTokenCount
// includes cached tokens, exactly like our cached_tokens subset. Google counts
// thought tokens outside candidatesTokenCount, while our completion_tokens (like
// OpenAI's) covers all billed output, so the thoughts are added in.
type googleUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
}

func (u *googleUsage) usage() Usage {
	return Usage{
		PromptTokens:     u.PromptTokenCount,
		CompletionTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CachedTokens:     u.CachedContentTokenCount,
		ReasoningTokens:  u.ThoughtsTokenCount,
	}
}

// googleContents renders the history as Google contents: text and inline image
// parts, an assistant turn as role "model" with functionCall parts, and each
// stored tool result as a following user turn of functionResponse parts.
func googleContents(msgs []Message) []googleContent {
	out := make([]googleContent, 0, len(msgs)+2)
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
			// Old turns may still hold leaked DeepSeek markup in the DB.
			m.Content = SanitizeDSML(m.Content)
		}
		parts := googleMessageParts(m)
		calls := replayCalls(m)
		for _, c := range calls {
			parts = append(parts, googlePart{
				FunctionCall:     &googleFunctionCall{Name: c.name, Args: googleCallArgs(c.arguments)},
				ThoughtSignature: c.thoughtSignature,
			})
		}
		if len(parts) > 0 {
			out = append(out, googleContent{Role: role, Parts: parts})
		}
		if len(calls) > 0 {
			replies := make([]googlePart, 0, len(calls))
			for _, c := range calls {
				replies = append(replies, googlePart{FunctionResponse: &googleFunctionResponse{
					Name: c.name, Response: googleToolResponse(c.output),
				}})
			}
			out = append(out, googleContent{Role: "user", Parts: replies})
		}
	}
	return out
}

// googleMessageParts renders one message's text and images.
func googleMessageParts(m Message) []googlePart {
	var parts []googlePart
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, googlePart{Text: m.Content})
	}
	for _, img := range m.Images {
		// Google takes raw base64 bytes, not a data URI; other URLs are skipped.
		if mime, data, ok := googleInlineImage(img); ok {
			parts = append(parts, googlePart{InlineData: &googleInlineData{MIMEType: mime, Data: data}})
		}
	}
	return parts
}

// googleInlineImage splits a "data:<mime>;base64,<data>" URI.
func googleInlineImage(url string) (mime, data string, ok bool) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return "", "", false
	}
	mime, data, ok = strings.Cut(rest, ",")
	if !ok || data == "" {
		return "", "", false
	}
	return strings.TrimSuffix(mime, ";base64"), data, true
}

// googleCallArgs embeds stored arguments as the JSON object functionCall.args
// requires, falling back to wrapping text that is not valid JSON.
func googleCallArgs(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	b, _ := json.Marshal(map[string]string{"input": args})
	return b
}

// googleToolResponse is the functionResponse payload body.
func googleToolResponse(output string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"result": output})
	return b
}

// googleSystemInstruction carries the attachment context and the project
// instructions as system parts.
func googleSystemInstruction(p Params) map[string]any {
	var parts []googlePart
	if p.Context != "" {
		parts = append(parts, googlePart{Text: p.Context})
	}
	if p.Instructions != "" {
		parts = append(parts, googlePart{Text: p.Instructions})
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]any{"parts": parts}
}

// googleGenerationConfig maps our effort onto a thinking budget. An empty or
// unknown effort leaves the budget dynamic (Google's default).
func googleGenerationConfig(p Params) map[string]any {
	thinking := map[string]any{"includeThoughts": true}
	if budget, ok := googleThinkingBudget(p.Effort); ok {
		thinking["thinkingBudget"] = budget
	}
	return map[string]any{
		"maxOutputTokens": p.budget(),
		"temperature":     1.0,
		"thinkingConfig":  thinking,
	}
}

func googleThinkingBudget(effort string) (int, bool) {
	switch effort {
	case "none":
		return 0, true
	case "low":
		return 1024, true
	case "medium":
		return 8192, true
	case "high":
		return 24576, true
	case "xhigh", "max":
		return -1, true // dynamic thinking
	}
	return 0, false
}

// googleTools renders our tool specs as one functionDeclarations array.
func googleTools(tools []ToolSpec) []any {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]any, 0, len(tools))
	for _, t := range tools {
		d := map[string]any{"name": t.Function.Name, "description": t.Function.Description}
		if len(t.Function.Parameters) > 0 {
			d["parameters"] = t.Function.Parameters
		}
		decls = append(decls, d)
	}
	return []any{map[string]any{"functionDeclarations": decls}}
}

// googleBase is the effective Google base (overridable in tests).
func (c *Client) googleBase() string {
	if c.googleURL != "" {
		return c.googleURL
	}
	return GoogleBaseURL
}

// doGoogle issues one streaming GenerateContent POST. The gateway validates
// either Authorization: Bearer or x-goog-api-key; we keep the bearer form used
// by every other dialect. Non-2xx yields a clean error from the error body.
func (c *Client) doGoogle(ctx context.Context, p Params, contents []googleContent, tools []ToolSpec) (*http.Response, error) {
	payload := map[string]any{
		"contents":         contents,
		"generationConfig": googleGenerationConfig(p),
	}
	if si := googleSystemInstruction(p); si != nil {
		payload["systemInstruction"] = si
	}
	if ts := googleTools(tools); ts != nil {
		payload["tools"] = ts
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	endpoint := c.googleBase() + "/models/" + p.Model + ":streamGenerateContent?alt=sse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("x-opencode-session", p.SessionID)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		resp.Body.Close()
		return nil, googleHTTPError(resp.StatusCode, snippet)
	}
	return resp, nil
}

// googleHTTPError turns Google's {"error":{code,status,message}} envelope (and
// the gateway's {"error":{type,message}} one) into a readable error.
func googleHTTPError(status int, body []byte) error {
	var env struct {
		Error struct {
			Status  string `json:"status"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		label := env.Error.Status
		if label == "" {
			label = env.Error.Type
		}
		if label != "" {
			return fmt.Errorf("upstream HTTP %d: %s: %s", status, label, env.Error.Message)
		}
		return fmt.Errorf("upstream HTTP %d: %s", status, env.Error.Message)
	}
	return fmt.Errorf("upstream HTTP %d: %s", status, strings.TrimSpace(firstN(string(body), 200)))
}

// googleStreamError reports an error object delivered inside the stream.
func googleStreamError(r *googleResponse) error {
	label := r.Error.Status
	if label == "" {
		label = r.Error.Type
	}
	if label != "" {
		return fmt.Errorf("upstream %s: %s", label, firstN(r.Error.Message, 200))
	}
	return fmt.Errorf("upstream error: %s", firstN(r.Error.Message, 200))
}

// parseGoogleStream decodes either an SSE stream of GenerateContentResponse
// objects (alt=sse) or a chunked JSON array of them, so the request works
// whether or not the endpoint honours alt=sse. Malformed and truncated tails
// end the stream without failing the reply.
func parseGoogleStream(body io.Reader, handle func(*googleResponse) (bool, error)) error {
	br := bufio.NewReader(body)
	first, err := firstNonSpace(br)
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	if first != '[' {
		return scanSSE(br, func(_, data string) (bool, error) {
			if data == "[DONE]" {
				return true, nil
			}
			var r googleResponse
			if err := json.Unmarshal([]byte(data), &r); err != nil {
				// Ignore malformed/keepalive data lines.
				return false, nil
			}
			return handle(&r)
		})
	}
	dec := json.NewDecoder(br)
	if _, err := dec.Token(); err != nil {
		return nil
	}
	for dec.More() {
		var r googleResponse
		if err := dec.Decode(&r); err != nil {
			// Truncated array: keep what already streamed.
			return nil
		}
		done, herr := handle(&r)
		if herr != nil {
			return herr
		}
		if done {
			return nil
		}
	}
	return nil
}

// firstNonSpace peeks at the first non-whitespace byte, leaving it unread.
func firstNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b == ' ' || b == '\n' || b == '\r' || b == '\t' {
			continue
		}
		if err := br.UnreadByte(); err != nil {
			return 0, err
		}
		return b, nil
	}
}

// streamGoogle implements the Google dialect for a plain (non-agent) stream.
func (c *Client) streamGoogle(ctx context.Context, p Params, send func(Event) bool) error {
	resp, err := c.doGoogle(ctx, p, googleContents(p.Messages), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var usage Usage
	var hasUsage bool
	err = parseGoogleStream(resp.Body, func(r *googleResponse) (bool, error) {
		if r.Error != nil {
			return false, googleStreamError(r)
		}
		if r.UsageMetadata != nil {
			usage = r.UsageMetadata.usage()
			hasUsage = true
		}
		for _, cand := range r.Candidates {
			for _, part := range cand.Content.Parts {
				if part.FunctionCall != nil {
					// A plain stream exposes no tools, so a call is ignored.
					continue
				}
				if part.Text == "" {
					continue
				}
				kind := EventText
				if part.Thought {
					kind = EventReasoning
				}
				if !send(Event{Kind: kind, Delta: part.Text}) {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if hasUsage {
		send(Event{Kind: EventUsage, Usage: &usage})
	}
	return nil
}

// agentGoogle runs the agentic loop for the Google dialect. Google matches a
// functionResponse by name (no call ids), so ids are synthesized locally.
func (c *Client) agentGoogle(ctx context.Context, p Params, exec ExecuteTool, opts AgentOptions, send func(Event) bool) error {
	contents := googleContents(p.Messages)

	maxIter := opts.iterations()
	var total Usage
	var reasoning strings.Builder // reasoning emitted this turn, across rounds
	for iter := 0; iter < maxIter; iter++ {
		if opts.OnStep != nil {
			opts.OnStep(iter+1, maxIter)
		}
		// The last iteration runs without tools so the model must answer.
		useTools := len(p.Tools) > 0 && iter < maxIter-1
		var tools []ToolSpec
		if useTools {
			tools = p.Tools
		}

		roundCtx, cancelRound := context.WithTimeout(ctx, opts.callTimeout())
		resp, err := c.doGoogle(roundCtx, p, contents, tools)
		if err != nil && useTools && iter == 0 {
			// A model that rejects tool calling must not break the chat: retry
			// once without tools and answer normally.
			resp, err = c.doGoogle(roundCtx, p, contents, nil)
		}
		if err != nil {
			cancelRound()
			return err
		}

		var modelParts []googlePart
		var calls []ToolCall
		roundReasoning := false
		var round Usage
		var hasRound bool
		err = parseGoogleStream(resp.Body, func(r *googleResponse) (bool, error) {
			if r.Error != nil {
				return false, googleStreamError(r)
			}
			if r.UsageMetadata != nil {
				round = r.UsageMetadata.usage()
				hasRound = true
			}
			for _, cand := range r.Candidates {
				for _, part := range cand.Content.Parts {
					switch {
					case part.FunctionCall != nil:
						calls = append(calls, ToolCall{
							ID:               fmt.Sprintf("google_%d_%d", iter, len(calls)),
							Name:             part.FunctionCall.Name,
							Arguments:        googleCallArgsString(part.FunctionCall.Args),
							ThoughtSignature: part.ThoughtSignature,
						})
						modelParts = append(modelParts, googlePart{
							FunctionCall:     part.FunctionCall,
							ThoughtSignature: part.ThoughtSignature,
						})
					case part.Text == "":
						// Some streams deliver the signature in a part of its own.
						if part.ThoughtSignature != "" {
							googleAttachSignature(modelParts, calls, part.ThoughtSignature)
						}
					case part.Thought:
						safe := part.Text
						if !roundReasoning {
							safe = roundSeparator(reasoning.String()) + safe
							roundReasoning = true
						}
						reasoning.WriteString(safe)
						if !send(Event{Kind: EventReasoning, Delta: safe}) {
							return true, nil
						}
					default:
						modelParts = append(modelParts, googlePart{
							Text: part.Text, ThoughtSignature: part.ThoughtSignature,
						})
						if !send(Event{Kind: EventText, Delta: part.Text}) {
							return true, nil
						}
					}
				}
			}
			return false, nil
		})
		resp.Body.Close()
		cancelRound()
		if err != nil {
			return err
		}
		if hasRound {
			total.add(&round)
		}

		if len(calls) == 0 || exec == nil {
			send(Event{Kind: EventUsage, Usage: &total})
			return nil
		}

		contents = append(contents, googleContent{Role: "model", Parts: modelParts})
		replies := make([]googlePart, 0, len(calls))
		for _, call := range calls {
			result := runTool(ctx, exec, send, call)
			replies = append(replies, googlePart{FunctionResponse: &googleFunctionResponse{
				Name: call.Name, Response: googleToolResponse(result),
			}})
		}
		contents = append(contents, googleContent{Role: "user", Parts: replies})
	}
	send(Event{Kind: EventUsage, Usage: &total})
	return nil
}

// googleAttachSignature puts a signature delivered in a part of its own onto
// the most recent functionCall part without one, which is where Google expects
// it back.
func googleAttachSignature(parts []googlePart, calls []ToolCall, sig string) {
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i].FunctionCall == nil || parts[i].ThoughtSignature != "" {
			continue
		}
		parts[i].ThoughtSignature = sig
		call := 0
		for j := 0; j <= i; j++ {
			if parts[j].FunctionCall != nil {
				call++
			}
		}
		if call > 0 && call <= len(calls) {
			calls[call-1].ThoughtSignature = sig
		}
		return
	}
}

// googleCallArgsString renders decoded call args back as our Arguments string.
func googleCallArgsString(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}"
	}
	return string(raw)
}
