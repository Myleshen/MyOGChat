package imagegen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeComfy is a minimal ComfyUI-native-API stand-in.
type fakeComfy struct {
	// historyPolls counts GET /history calls before it returns the finished entry.
	historyPolls int32
	// historyCalls is the running count.
	historyCalls int32
	image        []byte
	submitted    map[string]any // captured POST /prompt body
	token        string         // required bearer token ("" = no check)
}

func (f *fakeComfy) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.submitted)
		_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": "pid-1"})
	})
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.historyCalls, 1)
		if n <= f.historyPolls {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		resp := map[string]any{
			"pid-1": map[string]any{
				"outputs": map[string]any{
					"9": map[string]any{
						"images": []map[string]string{
							{"filename": "myogchat_00001_.png", "subfolder": "", "type": "output"},
						},
					},
				},
				"status": map[string]any{"completed": true, "status_str": "success"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filename") != "myogchat_00001_.png" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(f.image)
	})
	mux.HandleFunc("GET /system_stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"system":{"comfyui_version":"1.0"}}`))
	})
	return mux
}

func newTestClient(t *testing.T, f *fakeComfy) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestGenerateRoundTrip(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	f := &fakeComfy{historyPolls: 2, image: png}
	c, _ := newTestClient(t, f)

	res, err := c.Generate(context.Background(), "a red fox in a snowy forest", 512, 512)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(res.Data) != string(png) {
		t.Errorf("image bytes = %q, want %q", res.Data, png)
	}
	if res.Mime != "image/png" {
		t.Errorf("mime = %q, want image/png", res.Mime)
	}
	if res.Width != 512 || res.Height != 512 {
		t.Errorf("dims = %dx%d, want 512x512", res.Width, res.Height)
	}
	if res.Model != DefaultModel {
		t.Errorf("model = %q, want %q", res.Model, DefaultModel)
	}
	if atomic.LoadInt32(&f.historyCalls) < 3 {
		t.Errorf("history polled %d times, want ≥3 (2 empty + 1 done)", f.historyCalls)
	}

	// The submitted workflow must carry the owner's chosen graph parameters.
	raw, ok := f.submitted["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("submitted body missing prompt graph: %#v", f.submitted)
	}
	ckpt := raw["4"].(map[string]any)["inputs"].(map[string]any)["ckpt_name"]
	if ckpt != DefaultModel {
		t.Errorf("ckpt_name = %v, want %s", ckpt, DefaultModel)
	}
	ks := raw["3"].(map[string]any)["inputs"].(map[string]any)
	if ks["sampler_name"] != "dpmpp_2m" || ks["scheduler"] != "karras" {
		t.Errorf("sampler = %v/%v, want dpmpp_2m/karras", ks["sampler_name"], ks["scheduler"])
	}
	if ks["steps"] != float64(4) || ks["cfg"] != float64(1) {
		t.Errorf("steps/cfg = %v/%v, want 4/1", ks["steps"], ks["cfg"])
	}
	latent := raw["5"].(map[string]any)["inputs"].(map[string]any)
	if latent["width"] != float64(512) || latent["height"] != float64(512) {
		t.Errorf("latent = %vx%v, want 512x512", latent["width"], latent["height"])
	}
	pos := raw["6"].(map[string]any)["inputs"].(map[string]any)["text"]
	if pos != "a red fox in a snowy forest" {
		t.Errorf("positive prompt = %v", pos)
	}
}

func TestGenerateUnreachable(t *testing.T) {
	// Point at a closed port: New validates the URL, Generate must report
	// ErrUnreachable rather than hanging or panicking.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	c, err := New(Config{BaseURL: url, Timeout: 500 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Generate(context.Background(), "anything", 512, 512)
	if err == nil || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Generate unreachable = %v, want ErrUnreachable", err)
	}
}

func TestGenerateBackendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prompt" {
			http.Error(w, "prompt too long", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Timeout: 2 * time.Second})
	_, err := c.Generate(context.Background(), "x", 512, 512)
	if err == nil || !strings.Contains(err.Error(), "prompt too long") {
		t.Fatalf("Generate error = %v, want body echoed", err)
	}
}

func TestGenerateTimeout(t *testing.T) {
	f := &fakeComfy{historyPolls: 1 << 30} // never completes
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Timeout: 150 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	_, err := c.Generate(context.Background(), "x", 512, 512)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Generate timeout = %v, want timeout error", err)
	}
}

func TestAvailable(t *testing.T) {
	f := &fakeComfy{image: []byte("x")}
	c, _ := newTestClient(t, f)
	if !c.Available(context.Background()) {
		t.Error("Available = false, want true")
	}
	// A closed server is not available.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c2, _ := New(Config{BaseURL: url})
	if c2.Available(context.Background()) {
		t.Error("Available on a closed server = true, want false")
	}
}

func TestTokenSent(t *testing.T) {
	f := &fakeComfy{image: []byte("x"), token: "s3cret"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "s3cret", Timeout: 2 * time.Second, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Generate(context.Background(), "x", 512, 512); err != nil {
		t.Fatalf("Generate with token: %v", err)
	}

	// Wrong token is rejected with a clear error.
	c2, _ := New(Config{BaseURL: srv.URL, Token: "wrong", Timeout: 2 * time.Second})
	if _, err := c2.Generate(context.Background(), "x", 512, 512); err == nil {
		t.Fatal("Generate with wrong token succeeded, want error")
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"comfyui.local:8188", "http://comfyui.local:8188", false},
		{"http://comfyui.local:8188/", "http://comfyui.local:8188", false},
		{"https://comfy.example.com/base/", "https://comfy.example.com/base", false},
		{"", "", true},
		{"ftp://host", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeBaseURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeBaseURL(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
	}{
		{"", 512, 512},
		{"512x512", 512, 512},
		{"768x512", 768, 512},
		{"bogus", 512, 512},
		{"2000x2000", 1024, 1024}, // clamped
		{"10x10", 64, 64},         // floored
	}
	for _, tc := range cases {
		w, h := ParseSize(tc.in)
		if w != tc.w || h != tc.h {
			t.Errorf("ParseSize(%q) = %d,%d; want %d,%d", tc.in, w, h, tc.w, tc.h)
		}
	}
}

// TestLiveSmoke renders a real image against a running ComfyUI instance. It is
// skipped unless MYOGCHAT_IMAGEGEN_LIVE=1 and MYOGCHAT_IMAGEGEN_URL are set:
//
//	MYOGCHAT_IMAGEGEN_LIVE=1 MYOGCHAT_IMAGEGEN_URL=http://comfyui.local:8188 \
//	  mise exec -- go test ./internal/imagegen/ -run LiveSmoke -v
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("MYOGCHAT_IMAGEGEN_LIVE") != "1" {
		t.Skip("set MYOGCHAT_IMAGEGEN_LIVE=1 and MYOGCHAT_IMAGEGEN_URL to run")
	}
	url := os.Getenv("MYOGCHAT_IMAGEGEN_URL")
	if url == "" {
		t.Skip("MYOGCHAT_IMAGEGEN_URL not set")
	}
	c, err := New(Config{BaseURL: url, Timeout: 120 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !c.Available(context.Background()) {
		t.Fatalf("backend at %s is not available", url)
	}
	start := time.Now()
	res, err := c.Generate(context.Background(), "a red fox in a snowy forest", 512, 512)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("generated %d bytes in %s (mime %s)", len(res.Data), time.Since(start).Round(time.Millisecond), res.Mime)
	if len(res.Data) < 1000 {
		t.Errorf("image too small: %d bytes", len(res.Data))
	}
	if !bytes.HasPrefix(res.Data, []byte("\x89PNG")) {
		t.Errorf("output is not a PNG: % x", res.Data[:min(8, len(res.Data))])
	}
}

// Sanity: the workflow marshals to valid JSON.
func TestWorkflowJSON(t *testing.T) {
	b, err := json.Marshal(buildWorkflow("p", 512, 512, 1, DefaultModel))
	if err != nil {
		t.Fatalf("marshal workflow: %v", err)
	}
	if !strings.Contains(string(b), "CheckpointLoaderSimple") {
		t.Errorf("workflow missing nodes: %s", b)
	}
	_ = fmt.Sprint()
}
