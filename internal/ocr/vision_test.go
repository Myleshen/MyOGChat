package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"myogchat/internal/upstream"
)

// fakeVisionClient captures the VisionRequest and returns canned text.
type fakeVisionClient struct {
	got   upstream.VisionRequest
	calls int
	text  string
	err   error
}

func (f *fakeVisionClient) VisionOnce(_ context.Context, req upstream.VisionRequest) (string, error) {
	f.got = req
	f.calls++
	return f.text, f.err
}

// pngBytes encodes a solid PNG of the given size.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestVisionModelOCRSuccess(t *testing.T) {
	fc := &fakeVisionClient{text: "  transcribed text\n"}
	v := NewVisionModel(VisionConfig{
		Client:  fc,
		Model:   "deepseek-v4-flash-vision-exp",
		Dialect: "chat",
		APIKey:  "k",
	})
	got, err := v.OCR(context.Background(), pngBytes(t, 8, 8))
	if err != nil {
		t.Fatalf("OCR: %v", err)
	}
	if got != "transcribed text" {
		t.Errorf("OCR = %q, want trimmed", got)
	}
	if fc.calls != 1 {
		t.Fatalf("calls = %d, want 1", fc.calls)
	}
	if fc.got.Model != "deepseek-v4-flash-vision-exp" || fc.got.Dialect != "chat" {
		t.Errorf("request = %+v", fc.got)
	}
	if fc.got.Detail != DefaultVisionDetail {
		t.Errorf("detail = %q, want %q", fc.got.Detail, DefaultVisionDetail)
	}
	if fc.got.MaxTokens != DefaultVisionMaxTokens {
		t.Errorf("max tokens = %d, want %d", fc.got.MaxTokens, DefaultVisionMaxTokens)
	}
	if fc.got.Prompt != DefaultVisionPrompt {
		t.Errorf("prompt = %q, want the default prompt", fc.got.Prompt)
	}
	if !strings.HasPrefix(fc.got.ImageURL, "data:image/png;base64,") {
		t.Errorf("image url = %q, want a PNG data URI", fc.got.ImageURL)
	}
}

func TestVisionModelOCRSniffsFormats(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\nxxxx"), "image/png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg"},
		{"gif", []byte("GIF89a...."), "image/gif"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBPxxxx")...), "image/webp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeVisionClient{text: "x"}
			v := NewVisionModel(VisionConfig{Client: fc, Model: "m", Dialect: "chat", APIKey: "k", MaxImageDim: 0})
			if _, err := v.OCR(context.Background(), tc.data); err != nil {
				t.Fatalf("OCR: %v", err)
			}
			if !strings.HasPrefix(fc.got.ImageURL, "data:"+tc.want+";base64,") {
				t.Errorf("image url = %q, want prefix %q", fc.got.ImageURL, tc.want)
			}
		})
	}
}

func TestVisionModelOCRUnsupportedFormat(t *testing.T) {
	fc := &fakeVisionClient{}
	v := NewVisionModel(VisionConfig{Client: fc, Model: "m", Dialect: "chat", APIKey: "k"})
	// BMP magic ("BM") — the stdlib cannot decode it and providers reject it.
	_, err := v.OCR(context.Background(), []byte("BM\x00\x00\x00\x00"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable (so a fallback can retry)", err)
	}
	if fc.calls != 0 {
		t.Errorf("client called %d times, want 0", fc.calls)
	}
}

func TestVisionModelOCRErrors(t *testing.T) {
	t.Run("empty image", func(t *testing.T) {
		v := NewVisionModel(VisionConfig{Client: &fakeVisionClient{}, Model: "m", APIKey: "k"})
		if _, err := v.OCR(context.Background(), nil); err == nil {
			t.Fatal("expected an error for an empty image")
		}
	})
	t.Run("missing key", func(t *testing.T) {
		fc := &fakeVisionClient{}
		v := NewVisionModel(VisionConfig{Client: fc, Model: "m"})
		if _, err := v.OCR(context.Background(), pngBytes(t, 4, 4)); err == nil {
			t.Fatal("expected an error without an API key")
		}
		if fc.calls != 0 {
			t.Errorf("client called %d times, want 0", fc.calls)
		}
	})
	t.Run("client error wraps", func(t *testing.T) {
		fc := &fakeVisionClient{err: errors.New("boom")}
		v := NewVisionModel(VisionConfig{Client: fc, Model: "m", APIKey: "k"})
		_, err := v.OCR(context.Background(), pngBytes(t, 4, 4))
		if err == nil || !strings.Contains(err.Error(), "vision OCR:") {
			t.Fatalf("err = %v, want a vision OCR error", err)
		}
	})
}

func TestVisionModelWithKey(t *testing.T) {
	fc := &fakeVisionClient{text: "x"}
	base := NewVisionModel(VisionConfig{Client: fc, Model: "m", Dialect: "chat"})
	bound, ok := base.WithKey("user-key", "sess-9").(*VisionModel)
	if !ok {
		t.Fatalf("WithKey returned %T", base.WithKey("a", "b"))
	}
	if bound.cfg.APIKey != "user-key" || bound.cfg.SessionID != "sess-9" {
		t.Errorf("bound cfg = %+v", bound.cfg)
	}
	// The original must be untouched (WithKey returns a copy).
	if base.cfg.APIKey != "" || base.cfg.SessionID != "" {
		t.Errorf("base cfg mutated: %+v", base.cfg)
	}
	if _, err := bound.OCR(context.Background(), pngBytes(t, 4, 4)); err != nil {
		t.Fatalf("OCR: %v", err)
	}
	if fc.got.APIKey != "user-key" || fc.got.SessionID != "sess-9" {
		t.Errorf("request = %+v", fc.got)
	}
}

func TestVisionModelDownscalesLargeImages(t *testing.T) {
	fc := &fakeVisionClient{text: "x"}
	v := NewVisionModel(VisionConfig{
		Client: fc, Model: "m", Dialect: "chat", APIKey: "k", MaxImageDim: 64,
	})
	if _, err := v.OCR(context.Background(), pngBytes(t, 400, 200)); err != nil {
		t.Fatalf("OCR: %v", err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(fc.got.ImageURL, prefix) {
		t.Fatalf("image url = %q", fc.got.ImageURL)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fc.got.ImageURL, prefix))
	if err != nil {
		t.Fatalf("decode data uri: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 64 || b.Dy() != 32 {
		t.Errorf("downscaled to %dx%d, want 64x32", b.Dx(), b.Dy())
	}
}

func TestSniffImageMIMEUnknown(t *testing.T) {
	if m := sniffImageMIME([]byte("not an image")); m != "" {
		t.Errorf("sniff = %q, want empty", m)
	}
}
