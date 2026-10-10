// Package imagegen is a small client for a self-hosted ComfyUI instance
// (feature 3.1). It builds a fixed SD-Turbo text-to-image workflow, submits it
// to ComfyUI's native API (POST /prompt), polls GET /history/{id} until the
// render finishes, and returns the PNG bytes fetched from GET /view.
//
// It uses only the standard library so the binary stays CGO-free and
// cross-compilable to linux/arm64. A bearer token is sent when configured even
// though stock ComfyUI ignores it — that keeps the client a drop-in for a
// token-guarded shim in front of ComfyUI.
package imagegen

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultModel is the checkpoint filename loaded by the workflow.
	DefaultModel = "sd_turbo.safetensors"
	// DefaultWidth / DefaultHeight are SD-Turbo's native 512² output.
	DefaultWidth  = 512
	DefaultHeight = 512

	// DefaultTimeout bounds one end-to-end generation (submit + poll + fetch).
	DefaultTimeout = 90 * time.Second
	// DefaultPollInterval is how often /history is checked.
	DefaultPollInterval = 500 * time.Millisecond
	// DefaultProbeTimeout bounds the health probe used by Settings.
	DefaultProbeTimeout = 3 * time.Second

	// maxImageBytes caps a single /view download (defensive; PNGs are ~300 KB).
	maxImageBytes = 16 << 20 // 16 MB
	// maxErrorBody caps how much of an error response is echoed.
	maxErrorBody = 512
)

// ErrUnreachable is wrapped around transport-level failures so callers can
// distinguish "the backend is down" from a protocol/render error.
var ErrUnreachable = errors.New("image backend unreachable")

// Config configures a Client.
type Config struct {
	// BaseURL is the ComfyUI origin, e.g. http://comfyui.local:8188. A missing
	// scheme is assumed to be http://.
	BaseURL string
	// Token is an optional bearer token (sent as Authorization when non-empty).
	Token string
	// Timeout bounds one Generate call; defaults to DefaultTimeout.
	Timeout time.Duration
	// PollInterval is the /history poll cadence; defaults to DefaultPollInterval.
	PollInterval time.Duration
}

// Client talks to one ComfyUI instance.
type Client struct {
	baseURL      string
	token        string
	hc           *http.Client
	timeout      time.Duration
	pollInterval time.Duration
}

// Result is a finished render.
type Result struct {
	Data   []byte // image bytes (PNG)
	Mime   string // content type reported by /view (defaults to image/png)
	Width  int
	Height int
	Model  string
	Prompt string
	Seed   int64
}

// New validates cfg and returns a client.
func New(cfg Config) (*Client, error) {
	base, err := NormalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	return &Client{
		baseURL:      base,
		token:        strings.TrimSpace(cfg.Token),
		hc:           &http.Client{Timeout: timeout},
		timeout:      timeout,
		pollInterval: poll,
	}, nil
}

// NormalizeBaseURL accepts a user-entered URL (with or without a scheme) and
// returns a clean origin with no trailing slash or path.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("image backend url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid image backend url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported image backend url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("image backend url has no host")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// BaseURL returns the normalized origin (for logging/diagnostics).
func (c *Client) BaseURL() string { return c.baseURL }

// Available probes GET /system_stats. It reports whether the backend answered;
// a probe timeout is bounded so Settings stays responsive.
func (c *Client) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, DefaultProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/system_stats", nil)
	if err != nil {
		return false
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// Generate renders one image for prompt at width×height and returns its bytes.
// A zero width/height falls back to 512².
func (c *Client) Generate(ctx context.Context, prompt string, width, height int) (*Result, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	// Enforce the overall wall-clock budget here: the per-request http.Client
	// timeout alone would let the /history poll loop run forever.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if width <= 0 {
		width = DefaultWidth
	}
	if height <= 0 {
		height = DefaultHeight
	}
	seed := randomSeed()

	wf := buildWorkflow(prompt, width, height, seed, DefaultModel)
	body, err := json.Marshal(map[string]any{"prompt": wf, "client_id": newUUID()})
	if err != nil {
		return nil, err
	}

	pid, err := c.submit(ctx, body)
	if err != nil {
		return nil, ctxError(ctx, err)
	}
	meta, err := c.wait(ctx, pid)
	if err != nil {
		return nil, ctxError(ctx, err)
	}
	data, mime, err := c.fetch(ctx, meta)
	if err != nil {
		return nil, ctxError(ctx, err)
	}
	return &Result{
		Data:   data,
		Mime:   mime,
		Width:  width,
		Height: height,
		Model:  DefaultModel,
		Prompt: prompt,
		Seed:   seed,
	}, nil
}

// submit POSTs the workflow and returns the prompt id.
func (c *Client) submit(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/prompt", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", c.statusError("POST /prompt", resp)
	}
	var out struct {
		PromptID string `json:"prompt_id"`
		Error    any    `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode /prompt response: %w", err)
	}
	if out.PromptID == "" {
		if out.Error != nil {
			return "", fmt.Errorf("image backend rejected the workflow: %v", out.Error)
		}
		return "", errors.New("image backend returned no prompt_id")
	}
	return out.PromptID, nil
}

// imageRef is one entry of a SaveImage node's outputs[].images[].
type imageRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// wait polls /history until the prompt completes, then returns the first
// output image reference.
func (c *Client) wait(ctx context.Context, pid string) (imageRef, error) {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		ref, done, err := c.pollHistory(ctx, pid)
		if err != nil {
			return imageRef{}, err
		}
		if done {
			return ref, nil
		}
		select {
		case <-ctx.Done():
			return imageRef{}, fmt.Errorf("timed out waiting for the image backend: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// pollHistory checks one /history/{id} response. done is true once the prompt
// has a completed entry; an errored entry returns an error.
func (c *Client) pollHistory(ctx context.Context, pid string) (imageRef, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/history/"+url.PathEscape(pid), nil)
	if err != nil {
		return imageRef{}, false, err
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return imageRef{}, false, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return imageRef{}, false, c.statusError("GET /history", resp)
	}
	var hist map[string]struct {
		Outputs map[string]struct {
			Images []imageRef `json:"images"`
		} `json:"outputs"`
		Status struct {
			Completed bool   `json:"completed"`
			StatusStr string `json:"status_str"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&hist); err != nil {
		return imageRef{}, false, fmt.Errorf("decode /history response: %w", err)
	}
	entry, ok := hist[pid]
	if !ok {
		return imageRef{}, false, nil // not finished yet
	}
	if entry.Status.StatusStr == "error" {
		return imageRef{}, false, errors.New("image backend reported a render error")
	}
	// Collect the first output image across nodes.
	for _, node := range entry.Outputs {
		for _, img := range node.Images {
			if img.Filename == "" {
				continue
			}
			return img, true, nil
		}
	}
	if entry.Status.Completed {
		return imageRef{}, false, errors.New("image backend completed with no image output")
	}
	return imageRef{}, false, nil
}

// fetch downloads the rendered image bytes from /view.
func (c *Client) fetch(ctx context.Context, ref imageRef) ([]byte, string, error) {
	q := url.Values{}
	q.Set("filename", ref.Filename)
	if ref.Subfolder != "" {
		q.Set("subfolder", ref.Subfolder)
	}
	typ := ref.Type
	if typ == "" {
		typ = "output"
	}
	q.Set("type", typ)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/view?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", c.statusError("GET /view", resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read image: %w", err)
	}
	if len(data) > maxImageBytes {
		return nil, "", errors.New("image exceeds the size limit")
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/png"
	}
	return data, mime, nil
}

// ctxError converts a context-deadline failure into a clear timeout error so
// callers get one consistent message regardless of which request tripped it.
func ctxError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out waiting for the image backend: %w", ctx.Err())
	}
	return err
}

// auth sets the optional bearer token and a non-generic User-Agent.
func (c *Client) auth(req *http.Request) {
	req.Header.Set("User-Agent", "myogchat/1.0")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// statusError builds a clear error from a non-2xx response.
func (c *Client) statusError(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("image backend %s returned %s", op, resp.Status)
	}
	return fmt.Errorf("image backend %s returned %s: %s", op, resp.Status, msg)
}

// buildWorkflow returns the fixed SD-Turbo txt2img workflow in ComfyUI's API
// format. It mirrors the graph in docs/research/3.1-imagegen-recheck.md with
// the owner's chosen sampler (dpmpp_2m/karras, 4 steps, cfg 1.0).
func buildWorkflow(prompt string, width, height int, seed int64, model string) map[string]any {
	return map[string]any{
		"4": map[string]any{
			"class_type": "CheckpointLoaderSimple",
			"inputs":     map[string]any{"ckpt_name": model},
		},
		"5": map[string]any{
			"class_type": "EmptyLatentImage",
			"inputs":     map[string]any{"width": width, "height": height, "batch_size": 1},
		},
		"6": map[string]any{
			"class_type": "CLIPTextEncode",
			"inputs":     map[string]any{"text": prompt, "clip": []any{"4", 1}},
		},
		"7": map[string]any{
			"class_type": "CLIPTextEncode",
			"inputs":     map[string]any{"text": "", "clip": []any{"4", 1}},
		},
		"3": map[string]any{
			"class_type": "KSampler",
			"inputs": map[string]any{
				"seed":         seed,
				"steps":        4,
				"cfg":          1.0,
				"sampler_name": "dpmpp_2m",
				"scheduler":    "karras",
				"denoise":      1.0,
				"model":        []any{"4", 0},
				"positive":     []any{"6", 0},
				"negative":     []any{"7", 0},
				"latent_image": []any{"5", 0},
			},
		},
		"8": map[string]any{
			"class_type": "VAEDecode",
			"inputs":     map[string]any{"samples": []any{"3", 0}, "vae": []any{"4", 2}},
		},
		"9": map[string]any{
			"class_type": "SaveImage",
			"inputs":     map[string]any{"filename_prefix": "myogchat", "images": []any{"8", 0}},
		},
	}
}

// randomSeed returns a non-negative seed that fits ComfyUI's int range.
func randomSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano() & 0x7fffffff
	}
	var n int64
	for _, x := range b {
		n = n<<8 | int64(x)
	}
	return n & 0x7fffffffffffffff
}

// newUUID returns a random RFC 4122 v4 UUID (ComfyUI client_id).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ParseSize parses a "WxH" size string (e.g. "512x512"). It returns the default
// 512² when size is empty, and clamps each dimension to a multiple of 64 within
// [64, 1024] to keep the latent graph valid.
func ParseSize(size string) (int, int) {
	size = strings.TrimSpace(strings.ToLower(size))
	if size == "" {
		return DefaultWidth, DefaultHeight
	}
	parts := strings.FieldsFunc(size, func(r rune) bool { return r == 'x' || r == '×' || r == '*' })
	if len(parts) != 2 {
		return DefaultWidth, DefaultHeight
	}
	w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil {
		return DefaultWidth, DefaultHeight
	}
	return clampDimension(w), clampDimension(h)
}

func clampDimension(v int) int {
	if v < 64 {
		v = 64
	}
	if v > 1024 {
		v = 1024
	}
	return (v / 64) * 64
}
