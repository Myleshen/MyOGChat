package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/speech"
	"myogchat/internal/store"
)

// fakeSTT is an injectable Transcriber that records the resolved config.
type fakeSTT struct {
	cfg  speech.STTConfig
	text string
	err  error
	got  []byte
	mime string
	name string
}

func (f *fakeSTT) Transcribe(_ context.Context, audio []byte, filename, mime string) (string, error) {
	f.got, f.name, f.mime = audio, filename, mime
	return f.text, f.err
}

// fakeTTS is an injectable Synthesizer that records the resolved config.
type fakeTTS struct {
	cfg   speech.TTSConfig
	audio []byte
	mime  string
	err   error
	text  string
	voice string
}

func (f *fakeTTS) Synthesize(_ context.Context, text, voice string) ([]byte, string, error) {
	f.text, f.voice = text, voice
	return f.audio, f.mime, f.err
}

// newSpeechTestServer builds a Server with fake engines wired in.
func newSpeechTestServer(t *testing.T) (*Server, *store.Store, *fakeSTT, *fakeTTS) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	stt := &fakeSTT{text: "hello there"}
	tts := &fakeTTS{audio: []byte("RIFF-WAV"), mime: "audio/wav"}
	srv := &Server{
		st:  st,
		log: slog.Default(),
		speechCfg: SpeechDefaults{
			STTURL:     "http://stt.invalid",
			STTModel:   "base.en",
			STTTimeout: time.Second,
			TTSURL:     "http://tts.invalid",
			TTSVoice:   "en_US-lessac-medium",
			TTSFormat:  "openai",
			TTSTimeout: time.Second,
		},
	}
	srv.newSTT = func(cfg speech.STTConfig) speech.Transcriber { stt.cfg = cfg; return stt }
	srv.newTTS = func(cfg speech.TTSConfig) speech.Synthesizer { tts.cfg = cfg; return tts }
	return srv, st, stt, tts
}

func speechUser() store.User {
	return store.User{ID: "u1", Username: "tester", Status: "active"}
}

// multipartAudio builds a multipart body with an "audio" file field.
func multipartAudio(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("audio", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func TestHandleSTTSuccess(t *testing.T) {
	srv, _, stt, _ := newSpeechTestServer(t)
	body, ct := multipartAudio(t, "clip.webm", []byte("AUDIODATA"))
	req := httptest.NewRequest(http.MethodPost, "/api/stt", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()

	srv.handleSTT(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text       string `json:"text"`
		DurationMs int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Text != "hello there" {
		t.Errorf("text = %q", out.Text)
	}
	if string(stt.got) != "AUDIODATA" {
		t.Errorf("engine got %q", stt.got)
	}
	if stt.name != "clip.webm" {
		t.Errorf("filename = %q", stt.name)
	}
}

func TestHandleSTTLanguageOverride(t *testing.T) {
	srv, _, stt, _ := newSpeechTestServer(t)
	body, ct := multipartAudio(t, "clip.webm", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "/api/stt", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()

	srv.handleSTT(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stt.cfg.BaseURL != "http://stt.invalid" || stt.cfg.Model != "base.en" {
		t.Errorf("resolved config = %+v", stt.cfg)
	}
}

func TestHandleSTTUnavailable(t *testing.T) {
	srv, _, stt, _ := newSpeechTestServer(t)
	stt.err = speech.ErrUnavailable
	body, ct := multipartAudio(t, "clip.webm", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "/api/stt", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()

	srv.handleSTT(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleSTTMissingAudio(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("note", "no file")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/stt", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()

	srv.handleSTT(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSTTDisabledBySetting(t *testing.T) {
	srv, st, _, _ := newSpeechTestServer(t)
	if err := st.SetSetting(context.Background(), settingSTTEnabled, "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	body, ct := multipartAudio(t, "clip.webm", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "/api/stt", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()

	srv.handleSTT(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleSTTUnconfigured(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	srv.speechCfg.STTURL = ""
	body, ct := multipartAudio(t, "clip.webm", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, "/api/stt", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()

	srv.handleSTT(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleTTSSuccess(t *testing.T) {
	srv, _, _, tts := newSpeechTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tts", strings.NewReader(`{"text":"read this"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()

	srv.handleTTS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "audio/wav" {
		t.Errorf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != "RIFF-WAV" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if tts.text != "read this" || tts.voice != "en_US-lessac-medium" {
		t.Errorf("engine got text=%q voice=%q", tts.text, tts.voice)
	}
}

func TestHandleTTSVoiceOverride(t *testing.T) {
	srv, _, _, tts := newSpeechTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tts", strings.NewReader(`{"text":"hi","voice":"custom"}`))
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleTTS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if tts.voice != "custom" {
		t.Errorf("voice = %q, want custom", tts.voice)
	}
}

func TestHandleTTSEmpty(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tts", strings.NewReader(`{"text":"  "}`))
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleTTS(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleTTSTooLong(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	long := strings.Repeat("a", maxTTSTextChars+1)
	body, _ := json.Marshal(map[string]string{"text": long})
	req := httptest.NewRequest(http.MethodPost, "/api/tts", bytes.NewReader(body))
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleTTS(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleTTSUnavailable(t *testing.T) {
	srv, _, _, tts := newSpeechTestServer(t)
	tts.err = speech.ErrUnavailable
	req := httptest.NewRequest(http.MethodPost, "/api/tts", strings.NewReader(`{"text":"hi"}`))
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleTTS(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleTTSDisabledBySetting(t *testing.T) {
	srv, st, _, _ := newSpeechTestServer(t)
	if err := st.SetSetting(context.Background(), settingTTSEnabled, "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tts", strings.NewReader(`{"text":"hi"}`))
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleTTS(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestSpeechStatus(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/speech/status", nil)
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleSpeechStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["stt_available"] != true || out["tts_available"] != true {
		t.Errorf("availability = %v", out)
	}
	if out["tts_voice"] != "en_US-lessac-medium" {
		t.Errorf("tts_voice = %v", out["tts_voice"])
	}
}

func TestSpeechSettingsSurfacesFlags(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	got := srv.speechSettings(context.Background())
	for _, key := range []string{"stt_enabled", "stt_available", "stt_url", "tts_enabled", "tts_available", "tts_url", "tts_voice"} {
		if _, ok := got[key]; !ok {
			t.Errorf("speechSettings missing %q", key)
		}
	}
	if got["stt_url"] != "http://stt.invalid" || got["tts_url"] != "http://tts.invalid" {
		t.Errorf("urls = %v / %v", got["stt_url"], got["tts_url"])
	}
}

func TestSpeechSettingsDefaultsTrue(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	got := srv.speechSettings(context.Background())
	if got["stt_enabled"] != true || got["tts_enabled"] != true {
		t.Errorf("enable flags should default true: %v", got)
	}
}

func TestSpeechURLOverrideFromSettings(t *testing.T) {
	srv, st, _, _ := newSpeechTestServer(t)
	ctx := context.Background()
	if err := st.SetSetting(ctx, settingSTTURL, "http://override:9000"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.SetSetting(ctx, settingTTSVoice, "en_US-amy-low"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := srv.sttURL(ctx); got != "http://override:9000" {
		t.Errorf("sttURL = %q, want override", got)
	}
	if got := srv.ttsVoice(ctx); got != "en_US-amy-low" {
		t.Errorf("ttsVoice = %q, want override", got)
	}
}

func TestSpeechErrorMapping(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	cases := []struct {
		err  error
		want int
	}{
		{speech.ErrUnavailable, http.StatusServiceUnavailable},
		{speech.ErrEmptyAudio, http.StatusBadRequest},
		{speech.ErrEmptyText, http.StatusBadRequest},
		{errors.New("boom"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		srv.writeSpeechError(rec, "test", tc.err)
		if rec.Code != tc.want {
			t.Errorf("writeSpeechError(%v) = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}

// ensure the fake satisfies the interfaces at compile time.
var (
	_ speech.Transcriber = (*fakeSTT)(nil)
	_ speech.Synthesizer = (*fakeTTS)(nil)
)

// TestHandleSTTWithRealEngine wires the real OpenAI-compatible client to an
// httptest fake, proving the handler→engine→wire path end to end.
func TestHandleSTTWithRealEngine(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "from server"})
	}))
	defer ts.Close()

	srv, _, _, _ := newSpeechTestServer(t)
	srv.speechCfg.STTURL = ts.URL
	srv.newSTT = func(cfg speech.STTConfig) speech.Transcriber { return speech.NewOpenAISTT(cfg) }

	body, ct := multipartAudio(t, "clip.webm", []byte("AUDIO"))
	req := httptest.NewRequest(http.MethodPost, "/api/stt", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleSTT(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Text != "from server" {
		t.Errorf("text = %q, want from server", out.Text)
	}
}

// TestHandleTTSWithRealEngine drives the real OpenAI-compatible client against
// an httptest fake and checks the raw audio response.
func TestHandleTTSWithRealEngine(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("WAVDATA"))
	}))
	defer ts.Close()

	srv, _, _, _ := newSpeechTestServer(t)
	srv.speechCfg.TTSURL = ts.URL
	srv.newTTS = func(cfg speech.TTSConfig) speech.Synthesizer { return speech.NewOpenAITTS(cfg) }

	req := httptest.NewRequest(http.MethodPost, "/api/tts", strings.NewReader(`{"text":"hi"}`))
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handleTTS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "WAVDATA" || rec.Header().Get("Content-Type") != "audio/wav" {
		t.Errorf("body = %q ct = %q", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

// TestPatchSettingsSpeech persists the speech settings through the admin PATCH
// handler and confirms they round-trip in the settings response.
func TestPatchSettingsSpeech(t *testing.T) {
	srv, st, _, _ := newSpeechTestServer(t)
	admin := store.User{ID: "admin", Username: "admin", IsAdmin: true, Status: "active"}
	body := `{"stt_enabled":false,"tts_enabled":true,"stt_url":"http://stt:9","tts_url":"http://tts:9","tts_voice":"en_US-amy-low"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithUser(req.Context(), admin))
	rec := httptest.NewRecorder()
	srv.handlePatchSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["stt_enabled"] != false || out["tts_enabled"] != true {
		t.Errorf("enable flags = %v", out)
	}
	if out["stt_url"] != "http://stt:9" || out["tts_url"] != "http://tts:9" || out["tts_voice"] != "en_US-amy-low" {
		t.Errorf("overrides = %v", out)
	}
	if v, _, _ := st.GetSetting(context.Background(), settingSTTURL); v != "http://stt:9" {
		t.Errorf("persisted stt_url = %q", v)
	}
}

// TestPatchSettingsSpeechNonAdmin verifies the admin gate still applies.
func TestPatchSettingsSpeechNonAdmin(t *testing.T) {
	srv, _, _, _ := newSpeechTestServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"stt_enabled":false}`))
	req = req.WithContext(auth.WithUser(req.Context(), speechUser()))
	rec := httptest.NewRecorder()
	srv.handlePatchSettings(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
