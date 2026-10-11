package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// visionTimeout bounds one non-streaming vision/OCR call. It is deliberately
// shorter than streamTimeout's 5 minutes: the user is waiting on an upload, not
// a chat reply.
const visionTimeout = 90 * time.Second

// VisionRequest describes one non-streaming image + prompt call (feature 2.13).
// ImageURL is a data URI ("data:image/png;base64,..."); both dialects accept it.
type VisionRequest struct {
	Dialect   string // "chat" | "responses"
	Model     string
	SessionID string // sent as x-opencode-session
	APIKey    string // bearer key for this request (per-user, §2.1)
	Prompt    string // instruction text accompanying the image
	ImageURL  string // data URI
	MaxTokens int    // 0 -> 2048
	Detail    string // "", "low", "high", "original", "auto" (0-value -> "high")
}

// VisionOnce sends one image + text prompt and returns the assistant's text. It
// is a single, non-streaming, non-tool call: OCR must never enter the agentic
// loop (no tools, no reasoning iterations). It owns both wire shapes:
//
//	chat      -> messages[].content[] with {"type":"image_url","image_url":{"url","detail"}}
//	responses -> input[].content[]    with {"type":"input_image","image_url","detail"}
func (c *Client) VisionOnce(ctx context.Context, req VisionRequest) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, visionTimeout)
	defer cancel()

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	detail := req.Detail
	if detail == "" {
		detail = visionDetail
	}

	var path string
	var payload map[string]any
	switch req.Dialect {
	case "responses":
		path = "/responses"
		payload = map[string]any{
			"model":             req.Model,
			"store":             false,
			"stream":            false,
			"max_output_tokens": maxTokens,
			"input": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": req.Prompt},
						// The responses dialect wants a flat image_url string and a
						// part-level detail.
						map[string]any{"type": "input_image", "image_url": req.ImageURL, "detail": detail},
					},
				},
			},
		}
	default: // "chat"
		path = "/chat/completions"
		payload = map[string]any{
			"model":      req.Model,
			"stream":     false,
			"max_tokens": maxTokens,
			"messages": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "text", "text": req.Prompt},
						// The chat dialect wants image_url as an object and detail
						// nested inside it.
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": req.ImageURL, "detail": detail}},
					},
				},
			},
		}
	}

	resp, err := c.do(ctx, path, req.SessionID, req.APIKey, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if req.Dialect == "responses" {
		return parseResponsesVision(body)
	}
	return parseChatVision(body)
}

// parseChatVision extracts choices[0].message.content.
func parseChatVision(body []byte) (string, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode vision completion: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("empty vision completion")
	}
	return out.Choices[0].Message.Content, nil
}

// parseResponsesVision concatenates output[].content[].text where the part is an
// output_text (the top-level output_text field is an SDK convenience, not part
// of the raw JSON).
func parseResponsesVision(body []byte) (string, error) {
	var out struct {
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode vision response: %w", err)
	}
	var b strings.Builder
	for _, item := range out.Output {
		for _, part := range item.Content {
			if part.Text == "" {
				continue
			}
			if part.Type != "" && part.Type != "output_text" {
				continue
			}
			b.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", fmt.Errorf("empty vision response")
	}
	return b.String(), nil
}
