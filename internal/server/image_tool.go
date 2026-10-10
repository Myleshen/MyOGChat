package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"myogchat/internal/imagegen"
	"myogchat/internal/store"
)

// imageTool is the model-facing generate_image tool (Phase 4.3b). The assistant
// calls it mid-conversation (so it has full context of what the user asked for);
// the tool generates via the configured ComfyUI backend, stores the bytes
// owner-scoped, and records a reference via collect so streamReply can attach it
// to the assistant message for the UI to render.
type imageTool struct {
	s       *Server
	u       store.User
	collect func(store.GeneratedImage)
}

func (t *imageTool) Name() string { return "generate_image" }

func (t *imageTool) Description() string {
	return "Generate a picture from a detailed text prompt and show it to the user. " +
		"Use this when the user asks you to create, draw, imagine, or generate an image. " +
		"Write a rich, concrete prompt describing the subject and its appearance, the pose/action, " +
		"the setting, the art style, and lighting. Translate named characters or brands into " +
		"visual descriptions. Never include real people's names, trademarks, nudity, or graphic content."
}

func (t *imageTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"A detailed text-to-image prompt."}},"required":["prompt"]}`)
}

func (t *imageTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}
	prompt = truncateRunes(prompt, maxImagePromptChars)

	client, ok := t.s.imagegenClient(ctx)
	if !ok {
		return "", fmt.Errorf("image generation is not configured")
	}
	width, height := imagegen.ParseSize("")
	res, err := client.Generate(ctx, prompt, width, height)
	if err != nil {
		return "", err
	}
	img := store.Image{
		ID:        newUUID(),
		UserID:    t.u.ID,
		Prompt:    prompt,
		Model:     res.Model,
		Mime:      res.Mime,
		Width:     res.Width,
		Height:    res.Height,
		Size:      int64(len(res.Data)),
		Data:      res.Data,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := t.s.st.InsertImage(ctx, &img); err != nil {
		return "", fmt.Errorf("failed to store image: %w", err)
	}
	if t.collect != nil {
		t.collect(store.GeneratedImage{
			ID: img.ID, Prompt: img.Prompt, Mime: img.Mime,
			Width: img.Width, Height: img.Height, Size: img.Size,
		})
	}
	return fmt.Sprintf("Generated an image (%dx%d) and attached it to your reply. Prompt used: %q",
		img.Width, img.Height, prompt), nil
}
