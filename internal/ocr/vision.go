// This file adds the vision-model OCR engine (feature 2.13): instead of running
// Tesseract in Docker, the image is sent to a multimodal OpenCode Go model as a
// data URI and the model is asked to transcribe it verbatim. The wire shapes
// live in internal/upstream (upstream.VisionOnce); this package only owns the
// Engine seam, image sniffing/downscaling, and the per-request key binding.
package ocr

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"myogchat/internal/upstream"
)

// Defaults for the vision engine.
const (
	// DefaultVisionModel is the recommended chat-dialect vision model: cheap,
	// already registered, and OCR-grade.
	DefaultVisionModel = "deepseek-v4.1-flash"
	// DefaultVisionPrompt asks for a verbatim transcription and nothing else.
	DefaultVisionPrompt = "You are a precise OCR engine. Transcribe ALL text visible in this image exactly as written. " +
		"Preserve the original language(s); never translate. Preserve line breaks and reading order; approximate tables " +
		"and columns with spaces or tabs. Do NOT describe the image, add commentary, or answer questions. Output ONLY " +
		"the transcribed text, with no preamble and no code fences. If there is no readable text, output nothing."
	// DefaultVisionDetail is "high" (small text needs the detail).
	DefaultVisionDetail = "high"
	// DefaultVisionMaxTokens caps the transcription length.
	DefaultVisionMaxTokens = 2048
	// DefaultVisionTimeout bounds one vision OCR call.
	DefaultVisionTimeout = 90 * time.Second
)

// VisionClient is the slice of upstream.Client the engine needs. It is an
// interface so the engine (and its tests) never depend on a live network.
type VisionClient interface {
	VisionOnce(ctx context.Context, req upstream.VisionRequest) (string, error)
}

// KeyedEngine is an Engine that needs a per-request API key. Server.ocrFunc
// binds the caller's resolved key (BYOK -> group -> shared server key) before
// the engine runs.
type KeyedEngine interface {
	Engine
	// WithKey returns a bound copy that uses apiKey and sessionID.
	WithKey(apiKey, sessionID string) Engine
}

// VisionConfig configures a VisionModel. Empty fields take their default.
type VisionConfig struct {
	Client    VisionClient // required
	Model     string       // vision-capable registry id
	Dialect   string       // "chat" | "responses"
	APIKey    string       // set per request via WithKey
	SessionID string       // conversation UUID for x-opencode-session
	Prompt    string       // overrides DefaultVisionPrompt
	MaxTokens int          // 0 -> DefaultVisionMaxTokens
	Detail    string       // "" -> DefaultVisionDetail
	Timeout   time.Duration
	// MaxImageDim downscales images whose longest side exceeds it before they
	// are sent. 0 uses DefaultMaxImageDim; a negative value disables
	// downscaling entirely.
	MaxImageDim int
}

// VisionModel transcribes an image through a vision-capable upstream model.
type VisionModel struct {
	cfg VisionConfig
}

// NewVisionModel builds a vision engine, applying defaults for empty fields.
func NewVisionModel(cfg VisionConfig) *VisionModel {
	if cfg.Model == "" {
		cfg.Model = DefaultVisionModel
	}
	if cfg.Dialect == "" {
		cfg.Dialect = "chat"
	}
	if cfg.Prompt == "" {
		cfg.Prompt = DefaultVisionPrompt
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultVisionMaxTokens
	}
	if cfg.Detail == "" {
		cfg.Detail = DefaultVisionDetail
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultVisionTimeout
	}
	if cfg.MaxImageDim == 0 {
		cfg.MaxImageDim = DefaultMaxImageDim
	}
	return &VisionModel{cfg: cfg}
}

// WithKey returns a bound copy carrying the per-request API key and session id.
func (v *VisionModel) WithKey(apiKey, sessionID string) Engine {
	c := v.cfg
	c.APIKey = apiKey
	c.SessionID = sessionID
	return &VisionModel{cfg: c}
}

// OCR transcribes image through the configured vision model. The image is
// sniffed (JPEG/PNG/GIF/WebP), optionally downscaled, and sent as a data URI.
// Errors wrap into extract.ErrOCR at the HTTP layer.
func (v *VisionModel) OCR(ctx context.Context, image []byte) (string, error) {
	if len(image) == 0 {
		return "", errors.New("vision OCR: empty image")
	}
	if v.cfg.Client == nil {
		return "", errors.New("vision OCR: no client configured")
	}
	if strings.TrimSpace(v.cfg.Model) == "" {
		return "", errors.New("vision OCR: no model configured")
	}
	if strings.TrimSpace(v.cfg.APIKey) == "" {
		return "", errors.New("vision OCR: no API key")
	}

	mime := sniffImageMIME(image)
	if mime == "" {
		// BMP/TIFF/PNM and anything else the stdlib cannot decode. Marked
		// unavailable so a FallbackEngine can retry with Tesseract.
		return "", fmt.Errorf("%w: vision OCR: unsupported image format (need JPEG/PNG/GIF/WebP)", ErrUnavailable)
	}
	img := image
	if v.cfg.MaxImageDim > 0 {
		if scaled, smime, ok := downscaleImage(image, v.cfg.MaxImageDim); ok {
			img, mime = scaled, smime
		}
	}
	dataURI := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)

	runCtx, cancel := context.WithTimeout(ctx, v.cfg.Timeout)
	defer cancel()

	text, err := v.cfg.Client.VisionOnce(runCtx, upstream.VisionRequest{
		Dialect:   v.cfg.Dialect,
		Model:     v.cfg.Model,
		SessionID: v.cfg.SessionID,
		APIKey:    v.cfg.APIKey,
		Prompt:    v.cfg.Prompt,
		ImageURL:  dataURI,
		MaxTokens: v.cfg.MaxTokens,
		Detail:    v.cfg.Detail,
	})
	if err != nil {
		return "", fmt.Errorf("vision OCR: %w", err)
	}
	return strings.TrimSpace(text), nil
}

// sniffImageMIME returns the MIME type from the image's magic bytes, or "" when
// the format is not one the vision providers accept (JPEG/PNG/GIF/WebP).
func sniffImageMIME(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
