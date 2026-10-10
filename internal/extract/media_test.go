package extract

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestExtractMediaTranscribes checks audio/video are routed to the injected
// speech-to-text function (Phase 4.5).
func TestExtractMediaTranscribes(t *testing.T) {
	var gotName, gotMime string
	tr := func(data []byte, name, mime string) (string, error) {
		gotName, gotMime = name, mime
		return "  hello from the clip  ", nil
	}
	got, err := ExtractOpts("clip.mp3", "audio/mpeg", []byte("ID3-not-really"), Options{Transcribe: tr})
	if err != nil {
		t.Fatalf("ExtractOpts(mp3): %v", err)
	}
	if !strings.Contains(got, "hello from the clip") {
		t.Errorf("transcript = %q", got)
	}
	if gotName != "clip.mp3" || gotMime != "audio/mpeg" {
		t.Errorf("transcribe got name=%q mime=%q", gotName, gotMime)
	}

	// Video by extension with a generic MIME.
	called := false
	tr2 := func([]byte, string, string) (string, error) { called = true; return "video audio", nil }
	if _, err := ExtractOpts("movie.mp4", "", []byte("...."), Options{Transcribe: tr2}); err != nil || !called {
		t.Errorf("mp4 not transcribed (err=%v called=%v)", err, called)
	}
}

func TestExtractMediaDisabled(t *testing.T) {
	_, err := ExtractOpts("clip.mp4", "video/mp4", []byte("x"), Options{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestExtractMediaTranscribeError(t *testing.T) {
	tr := func([]byte, string, string) (string, error) { return "", fmt.Errorf("engine down") }
	_, err := ExtractOpts("clip.m4a", "", []byte("x"), Options{Transcribe: tr})
	if !errors.Is(err, ErrTranscribe) {
		t.Errorf("err = %v, want ErrTranscribe", err)
	}

	// Empty transcript → ErrEmpty.
	empty := func([]byte, string, string) (string, error) { return "   ", nil }
	if _, err := ExtractOpts("clip.wav", "", []byte("x"), Options{Transcribe: empty}); !errors.Is(err, ErrEmpty) {
		t.Errorf("err = %v, want ErrEmpty", err)
	}
}
