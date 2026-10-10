// Package speech provides speech-to-text (STT, feature 3.5) and text-to-speech
// (TTS, feature 3.6) engines for MyOGChat.
//
// Whisper and Piper are native runtimes that cannot be cgo-linked into the
// CGO_ENABLED=0 single binary. Like internal/ocr, they run as separate
// *persistent* HTTP services (the model stays warm) and are called over the
// network. Keeping the wire format OpenAI-compatible ("/v1/audio/transcriptions"
// and "/v1/audio/speech") means a local whisper.cpp/Piper server, a Proxmox GPU
// shim, or the hosted OpenAI audio APIs are all drop-in.
//
// Ollama does NOT serve speech — it is LLMs + embeddings only.
package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// Defaults for the OpenAI-compatible engines.
const (
	// DefaultSTTURL is the whisper.cpp `whisper-server` (or a shim) base URL.
	DefaultSTTURL = "http://127.0.0.1:8080"
	// DefaultSTTModel is the informational model name sent to the server.
	DefaultSTTModel = "base.en"
	// DefaultSTTTimeout is generous: whisper `base.en` is ~2× real-time on a Pi.
	DefaultSTTTimeout = 120 * time.Second

	// DefaultTTSURL is the Piper server (or an OpenAI-compatible shim) base URL.
	DefaultTTSURL = "http://127.0.0.1:5000"
	// DefaultTTSVoice is a CC0-friendly Piper voice.
	DefaultTTSVoice = "en_US-lessac-medium"
	// DefaultTTSModel is the OpenAI speech model name (ignored by Piper).
	DefaultTTSModel = "tts-1"
	// DefaultTTSTimeout bounds one synthesis request.
	DefaultTTSTimeout = 30 * time.Second

	// DefaultUserAgent is sent on every request (matches the upstream client).
	DefaultUserAgent = "myogchat/1.0"

	// maxJSONResponse caps the transcription JSON read from the server.
	maxJSONResponse = 8 << 20 // 8 MB
	// maxAudioResponse caps the synthesized audio read from the server.
	maxAudioResponse = 64 << 20 // 64 MB
)

// Sentinel errors so the HTTP layer can map failures to clear statuses.
var (
	// ErrUnavailable marks a speech service that could not be reached (the
	// persistent server is down, the URL is wrong, or the request timed out).
	ErrUnavailable = errors.New("speech service unavailable")
	// ErrEmptyAudio marks a transcription request with no audio bytes.
	ErrEmptyAudio = errors.New("empty audio")
	// ErrEmptyText marks a synthesis request with no text.
	ErrEmptyText = errors.New("empty text")
)

// Transcriber turns audio bytes into text (feature 3.5).
type Transcriber interface {
	// Transcribe returns the text spoken in audio. filename and mime describe
	// the upload (used for the multipart part); ctx bounds the work.
	Transcribe(ctx context.Context, audio []byte, filename, mime string) (string, error)
}

// Synthesizer turns text into audio bytes (feature 3.6).
type Synthesizer interface {
	// Synthesize returns audio for text in voice. An empty voice uses the
	// engine default. The returned mime is the audio content type (audio/wav).
	Synthesize(ctx context.Context, text, voice string) ([]byte, string, error)
}

// STTFunc adapts a Transcriber to a plain function bound to ctx. It mirrors
// ocr.OCRFunc so internal/extract can fold audio transcripts the same way.
type STTFunc func(audio []byte, filename, mime string) (string, error)

// TTSFunc adapts a Synthesizer to a plain function bound to ctx.
type TTSFunc func(text, voice string) ([]byte, string, error)

// STT binds a Transcriber to ctx.
func STT(ctx context.Context, t Transcriber) STTFunc {
	return func(audio []byte, filename, mime string) (string, error) {
		return t.Transcribe(ctx, audio, filename, mime)
	}
}

// TTS binds a Synthesizer to ctx.
func TTS(ctx context.Context, s Synthesizer) TTSFunc {
	return func(text, voice string) ([]byte, string, error) {
		return s.Synthesize(ctx, text, voice)
	}
}

// STTConfig configures an OpenAI-compatible transcription client.
type STTConfig struct {
	// BaseURL is the service root, e.g. "http://127.0.0.1:8080". Required.
	BaseURL string
	// Model is the model name sent in the form (informational for whisper.cpp).
	Model string
	// Language is an optional ISO-639-1 hint ("en"); empty auto-detects.
	Language string
	// APIKey is an optional bearer token for a protected shim (Proxmox/OpenAI).
	APIKey string
	// Timeout bounds one request (and the default http.Client).
	Timeout time.Duration
	// Client overrides the HTTP client (tests); nil builds one with Timeout.
	Client *http.Client
}

// OpenAISTT transcribes audio via POST /v1/audio/transcriptions (multipart). It
// speaks the whisper.cpp `whisper-server` shape and is OpenAI-compatible.
type OpenAISTT struct {
	baseURL  string
	model    string
	language string
	apiKey   string
	client   *http.Client
}

// NewOpenAISTT builds a transcription client, applying defaults.
func NewOpenAISTT(cfg STTConfig) *OpenAISTT {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	model := cfg.Model
	if model == "" {
		model = DefaultSTTModel
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &OpenAISTT{
		baseURL:  base,
		model:    model,
		language: strings.TrimSpace(cfg.Language),
		apiKey:   strings.TrimSpace(cfg.APIKey),
		client:   client,
	}
}

// Transcribe uploads audio as multipart/form-data and returns the transcript.
func (c *OpenAISTT) Transcribe(ctx context.Context, audio []byte, filename, mime string) (string, error) {
	if len(audio) == 0 {
		return "", ErrEmptyAudio
	}
	if filename == "" {
		filename = "audio"
	}
	if mime == "" {
		mime = "application/octet-stream"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	header.Set("Content-Type", mime)
	part, err := mw.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("stt: build multipart: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("stt: write multipart: %w", err)
	}
	// whisper.cpp ignores response_format but OpenAI requires it.
	_ = mw.WriteField("response_format", "json")
	if c.model != "" {
		_ = mw.WriteField("model", c.model)
	}
	if c.language != "" {
		_ = mw.WriteField("language", c.language)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("stt: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", DefaultUserAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponse))
	if err != nil {
		return "", fmt.Errorf("stt: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("stt: HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("stt: decode response: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// TTSConfig configures an OpenAI-compatible or Piper synthesis client.
type TTSConfig struct {
	// BaseURL is the service root, e.g. "http://127.0.0.1:5000". Required.
	BaseURL string
	// Model is the OpenAI speech model name (ignored by Piper).
	Model string
	// Voice is the default voice when the request does not specify one.
	Voice string
	// APIKey is an optional bearer token for a protected shim.
	APIKey string
	// Timeout bounds one request (and the default http.Client).
	Timeout time.Duration
	// Client overrides the HTTP client (tests); nil builds one with Timeout.
	Client *http.Client
}

// OpenAITTS synthesizes speech via POST /v1/audio/speech (JSON). It is a drop-in
// for an OpenAI-compatible shim (Proxmox GPU / `speaches` / OpenAI hosted).
type OpenAITTS struct {
	baseURL string
	model   string
	voice   string
	apiKey  string
	client  *http.Client
}

// NewOpenAITTS builds an OpenAI-compatible synthesis client, applying defaults.
func NewOpenAITTS(cfg TTSConfig) *OpenAITTS {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	model := cfg.Model
	if model == "" {
		model = DefaultTTSModel
	}
	voice := strings.TrimSpace(cfg.Voice)
	if voice == "" {
		voice = DefaultTTSVoice
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &OpenAITTS{baseURL: base, model: model, voice: voice, apiKey: strings.TrimSpace(cfg.APIKey), client: client}
}

// Synthesize posts text to /v1/audio/speech and returns the audio bytes.
func (c *OpenAITTS) Synthesize(ctx context.Context, text, voice string) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", ErrEmptyText
	}
	if strings.TrimSpace(voice) == "" {
		voice = c.voice
	}
	payload := map[string]any{
		"model":           c.model,
		"input":           text,
		"voice":           voice,
		"response_format": "wav",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", DefaultUserAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return c.do(req)
}

// PiperTTS synthesizes speech against a native Piper HTTP server. Piper's
// `piper.http_server` accepts POST {"text": ...} on `/synthesize` and returns a
// WAV; the voice is normally fixed at server start, so the "voice" field is
// advisory.
type PiperTTS struct {
	baseURL string
	voice   string
	client  *http.Client
}

// NewPiperTTS builds a native Piper synthesis client, applying defaults.
func NewPiperTTS(cfg TTSConfig) *PiperTTS {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultTTSURL
	}
	voice := strings.TrimSpace(cfg.Voice)
	if voice == "" {
		voice = DefaultTTSVoice
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &PiperTTS{baseURL: base, voice: voice, client: client}
}

// Synthesize posts text to the Piper server and returns the WAV bytes.
func (c *PiperTTS) Synthesize(ctx context.Context, text, voice string) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", ErrEmptyText
	}
	if strings.TrimSpace(voice) == "" {
		voice = c.voice
	}
	payload := map[string]any{"text": text, "voice": voice}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/synthesize", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", DefaultUserAgent)
	return c.do(req)
}

// do runs a synthesis request and returns (audio, mime).
func (c *PiperTTS) do(req *http.Request) ([]byte, string, error) {
	return doAudio(c.client, req)
}

// do runs an OpenAI TTS request and returns (audio, mime).
func (c *OpenAITTS) do(req *http.Request) ([]byte, string, error) {
	return doAudio(c.client, req)
}

// doAudio executes req, reading the audio body and mapping HTTP/network errors.
func doAudio(client *http.Client, req *http.Request) ([]byte, string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxAudioResponse))
	if err != nil {
		return nil, "", fmt.Errorf("tts: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("tts: HTTP %d: %s", resp.StatusCode, snippet(audio))
	}
	mime := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mime == "" {
		mime = "audio/wav"
	}
	return audio, mime, nil
}

// snippet trims a response body to a short, log-safe preview.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
