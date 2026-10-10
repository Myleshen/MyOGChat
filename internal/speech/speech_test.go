package speech

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAISTTSuccess(t *testing.T) {
	var gotModel, gotLang, gotFormat, gotName, gotMime string
	var gotAudio []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %s, want /v1/audio/transcriptions", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("content-type = %q, want multipart", r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			return
		}
		gotModel = r.FormValue("model")
		gotLang = r.FormValue("language")
		gotFormat = r.FormValue("response_format")
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
			return
		}
		defer f.Close()
		gotName = h.Filename
		gotMime = h.Header.Get("Content-Type")
		gotAudio, _ = io.ReadAll(f)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "  hello world  "})
	}))
	defer ts.Close()

	c := NewOpenAISTT(STTConfig{BaseURL: ts.URL, Model: "base.en", Language: "en", Client: ts.Client()})
	got, err := c.Transcribe(context.Background(), []byte("AUDIO"), "clip.webm", "audio/webm")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got != "hello world" {
		t.Errorf("text = %q, want %q (trimmed)", got, "hello world")
	}
	if gotModel != "base.en" || gotLang != "en" || gotFormat != "json" {
		t.Errorf("form fields = model:%q lang:%q format:%q", gotModel, gotLang, gotFormat)
	}
	if gotName != "clip.webm" || gotMime != "audio/webm" {
		t.Errorf("file part = %q / %q", gotName, gotMime)
	}
	if string(gotAudio) != "AUDIO" {
		t.Errorf("audio = %q, want AUDIO", gotAudio)
	}
}

func TestOpenAISTTEmptyAudio(t *testing.T) {
	c := NewOpenAISTT(STTConfig{BaseURL: "http://unused.invalid"})
	if _, err := c.Transcribe(context.Background(), nil, "a.webm", "audio/webm"); !errors.Is(err, ErrEmptyAudio) {
		t.Fatalf("err = %v, want ErrEmptyAudio", err)
	}
}

func TestOpenAISTTHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()
	c := NewOpenAISTT(STTConfig{BaseURL: ts.URL, Client: ts.Client()})
	_, err := c.Transcribe(context.Background(), []byte("x"), "a.webm", "audio/webm")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v, want HTTP 500", err)
	}
}

func TestOpenAISTTUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close() // now nothing is listening
	c := NewOpenAISTT(STTConfig{BaseURL: url})
	_, err := c.Transcribe(context.Background(), []byte("x"), "a.webm", "audio/webm")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestOpenAISTTTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow handler: the client gives up first, but the server still returns
		// so httptest.Server.Close() does not block.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	c := NewOpenAISTT(STTConfig{BaseURL: ts.URL, Timeout: 20 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Transcribe(ctx, []byte("x"), "a.webm", "audio/webm"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestOpenAITTSSuccess(t *testing.T) {
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("path = %s, want /v1/audio/speech", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF....WAVE"))
	}))
	defer ts.Close()

	c := NewOpenAITTS(TTSConfig{BaseURL: ts.URL, Voice: "en_US-lessac-medium", Client: ts.Client()})
	audio, mime, err := c.Synthesize(context.Background(), "hello", "")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(audio) != "RIFF....WAVE" || mime != "audio/wav" {
		t.Errorf("audio = %q mime = %q", audio, mime)
	}
	if body["input"] != "hello" || body["voice"] != "en_US-lessac-medium" {
		t.Errorf("body = %v", body)
	}
	if body["response_format"] != "wav" || body["model"] != DefaultTTSModel {
		t.Errorf("body = %v", body)
	}
}

func TestOpenAITTSVoiceOverride(t *testing.T) {
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte("x"))
	}))
	defer ts.Close()
	c := NewOpenAITTS(TTSConfig{BaseURL: ts.URL, Client: ts.Client()})
	if _, _, err := c.Synthesize(context.Background(), "hi", "custom-voice"); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if body["voice"] != "custom-voice" {
		t.Errorf("voice = %v, want custom-voice", body["voice"])
	}
}

func TestOpenAITTSEmptyText(t *testing.T) {
	c := NewOpenAITTS(TTSConfig{BaseURL: "http://unused.invalid"})
	if _, _, err := c.Synthesize(context.Background(), "   ", ""); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("err = %v, want ErrEmptyText", err)
	}
}

func TestOpenAITTSUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()
	c := NewOpenAITTS(TTSConfig{BaseURL: url})
	if _, _, err := c.Synthesize(context.Background(), "hi", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestPiperTTSSuccess(t *testing.T) {
	var body map[string]any
	var path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("WAV"))
	}))
	defer ts.Close()

	c := NewPiperTTS(TTSConfig{BaseURL: ts.URL, Voice: "en_US-ryan-low", Client: ts.Client()})
	audio, mime, err := c.Synthesize(context.Background(), "hello", "")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(audio) != "WAV" || mime != "audio/wav" {
		t.Errorf("audio = %q mime = %q", audio, mime)
	}
	if body["text"] != "hello" || body["voice"] != "en_US-ryan-low" {
		t.Errorf("body = %v", body)
	}
	if path != "/synthesize" {
		t.Errorf("path = %q, want /synthesize", path)
	}
}

func TestPiperTTSUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()
	c := NewPiperTTS(TTSConfig{BaseURL: url})
	if _, _, err := c.Synthesize(context.Background(), "hi", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestDefaults(t *testing.T) {
	stt := NewOpenAISTT(STTConfig{BaseURL: "http://x/"})
	if stt.model != DefaultSTTModel || stt.baseURL != "http://x" {
		t.Errorf("stt defaults = model:%q base:%q", stt.model, stt.baseURL)
	}
	tts := NewOpenAITTS(TTSConfig{BaseURL: "http://x"})
	if tts.voice != DefaultTTSVoice || tts.model != DefaultTTSModel {
		t.Errorf("tts defaults = voice:%q model:%q", tts.voice, tts.model)
	}
	piper := NewPiperTTS(TTSConfig{})
	if piper.baseURL != DefaultTTSURL || piper.voice != DefaultTTSVoice {
		t.Errorf("piper defaults = base:%q voice:%q", piper.baseURL, piper.voice)
	}
}

func TestFuncAdapters(t *testing.T) {
	sttFn := STT(context.Background(), fakeTranscriber{text: "ok"})
	if got, err := sttFn(nil, "a", "b"); err != nil || got != "ok" {
		t.Fatalf("STT adapter = %q, %v", got, err)
	}
	ttsFn := TTS(context.Background(), fakeSynth{audio: []byte("wav"), mime: "audio/wav"})
	if audio, mime, err := ttsFn("hi", ""); err != nil || string(audio) != "wav" || mime != "audio/wav" {
		t.Fatalf("TTS adapter = %q, %q, %v", audio, mime, err)
	}
}

type fakeTranscriber struct{ text string }

func (f fakeTranscriber) Transcribe(context.Context, []byte, string, string) (string, error) {
	return f.text, nil
}

type fakeSynth struct {
	audio []byte
	mime  string
}

func (f fakeSynth) Synthesize(context.Context, string, string) ([]byte, string, error) {
	return f.audio, f.mime, nil
}
