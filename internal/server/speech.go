package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/speech"
)

// DB-backed settings for speech (features 3.5 / 3.6). The enable flags default
// to true; the URLs and voice are optional overrides of the env defaults, so an
// admin can repoint the services from the Settings page without a restart.
const (
	settingSTTEnabled = "stt_enabled"
	settingTTSEnabled = "tts_enabled"
	settingSTTURL     = "stt_url"
	settingTTSURL     = "tts_url"
	settingTTSVoice   = "tts_voice"
)

// Limits for the speech endpoints.
const (
	// maxSTTBytes caps one uploaded recording (a 60 s Opus clip is ~0.5 MB).
	maxSTTBytes = 25 << 20 // 25 MB
	// maxTTSTextChars caps the synthesized text (matches Piper/OpenAI limits).
	maxTTSTextChars = 4000
)

// SpeechDefaults holds the env-derived speech configuration. DB settings
// (stt_url, tts_url, tts_voice) override these per request.
type SpeechDefaults struct {
	STTURL      string
	STTModel    string
	STTLanguage string
	STTAPIKey   string
	STTTimeout  time.Duration

	TTSURL     string
	TTSModel   string
	TTSVoice   string
	TTSFormat  string // "openai" | "piper"
	TTSAPIKey  string
	TTSTimeout time.Duration
}

// sttSettingEnabled reports whether the admin setting allows STT (default true).
func (s *Server) sttSettingEnabled(ctx context.Context) bool {
	return s.boolSettingDefault(ctx, settingSTTEnabled, true)
}

// ttsSettingEnabled reports whether the admin setting allows TTS (default true).
func (s *Server) ttsSettingEnabled(ctx context.Context) bool {
	return s.boolSettingDefault(ctx, settingTTSEnabled, true)
}

// boolSettingDefault reads a "1"/"0" setting, returning def when unset/error.
func (s *Server) boolSettingDefault(ctx context.Context, key string, def bool) bool {
	v, ok, err := s.st.GetSetting(ctx, key)
	if err != nil || !ok {
		return def
	}
	return v == "1"
}

// setBoolSetting persists a boolean as "1"/"0".
func (s *Server) setBoolSetting(ctx context.Context, key string, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return s.st.SetSetting(ctx, key, v)
}

// settingOrEnv returns the DB override for key when non-empty, else fallback.
func (s *Server) settingOrEnv(ctx context.Context, key, fallback string) string {
	if v, ok, err := s.st.GetSetting(ctx, key); err == nil && ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return fallback
}

// sttURL is the effective STT service URL (DB override or env default).
func (s *Server) sttURL(ctx context.Context) string {
	return s.settingOrEnv(ctx, settingSTTURL, s.speechCfg.STTURL)
}

// ttsURL is the effective TTS service URL (DB override or env default).
func (s *Server) ttsURL(ctx context.Context) string {
	return s.settingOrEnv(ctx, settingTTSURL, s.speechCfg.TTSURL)
}

// ttsVoice is the effective default voice (DB override or env default).
func (s *Server) ttsVoice(ctx context.Context) string {
	return s.settingOrEnv(ctx, settingTTSVoice, s.speechCfg.TTSVoice)
}

// sttConfigured reports whether an STT service is configured at all.
func (s *Server) sttConfigured(ctx context.Context) bool {
	return strings.TrimSpace(s.sttURL(ctx)) != ""
}

// ttsConfigured reports whether a TTS service is configured at all.
func (s *Server) ttsConfigured(ctx context.Context) bool {
	return strings.TrimSpace(s.ttsURL(ctx)) != ""
}

// sttTranscriber builds an STT engine from the resolved config, or nil when STT
// is disabled or unconfigured. language optionally overrides the env default.
func (s *Server) sttTranscriber(ctx context.Context, language string) speech.Transcriber {
	if s.newSTT == nil || !s.sttSettingEnabled(ctx) {
		return nil
	}
	url := s.sttURL(ctx)
	if url == "" {
		return nil
	}
	lang := strings.TrimSpace(language)
	if lang == "" {
		lang = s.speechCfg.STTLanguage
	}
	return s.newSTT(speech.STTConfig{
		BaseURL:  url,
		Model:    s.speechCfg.STTModel,
		Language: lang,
		APIKey:   s.speechCfg.STTAPIKey,
		Timeout:  s.speechCfg.STTTimeout,
	})
}

// ttsSynthesizer builds a TTS engine from the resolved config, or nil when TTS
// is disabled or unconfigured.
func (s *Server) ttsSynthesizer(ctx context.Context) speech.Synthesizer {
	if s.newTTS == nil || !s.ttsSettingEnabled(ctx) {
		return nil
	}
	url := s.ttsURL(ctx)
	if url == "" {
		return nil
	}
	return s.newTTS(speech.TTSConfig{
		BaseURL: url,
		Model:   s.speechCfg.TTSModel,
		Voice:   s.ttsVoice(ctx),
		APIKey:  s.speechCfg.TTSAPIKey,
		Timeout: s.speechCfg.TTSTimeout,
	})
}

// handleSTT transcribes an uploaded recording (multipart field "audio").
func (s *Server) handleSTT(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(w, r); !ok {
		return
	}
	// The language form field can override the default; read it before the
	// multipart form is consumed (ParseMultipartForm populates Form too).
	r.Body = http.MaxBytesReader(w, r.Body, maxSTTBytes+1<<20)
	if err := r.ParseMultipartForm(maxSTTBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "recording exceeds the 25 MB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "malformed multipart upload")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	tr := s.sttTranscriber(r.Context(), r.FormValue("language"))
	if tr == nil {
		writeError(w, http.StatusServiceUnavailable, "speech-to-text is not available")
		return
	}

	file, header, err := r.FormFile("audio")
	if err != nil {
		// Tolerate the generic "file" field name too.
		file, header, err = r.FormFile("file")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing audio field")
		return
	}
	defer file.Close()
	if header.Size > maxSTTBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "recording exceeds the 25 MB limit")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSTTBytes+1))
	if err != nil {
		s.log.Error("read recording", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read recording")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "the recording is empty")
		return
	}
	if len(data) > maxSTTBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "recording exceeds the 25 MB limit")
		return
	}

	name := strings.TrimSpace(header.Filename)
	if name == "" {
		name = "audio.webm"
	}
	mimeType := header.Header.Get("Content-Type")
	if strings.TrimSpace(mimeType) == "" {
		mimeType = http.DetectContentType(data)
	}

	start := time.Now()
	text, err := tr.Transcribe(r.Context(), data, name, mimeType)
	if err != nil {
		s.writeSpeechError(w, "transcribe", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text":        text,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

// handleTTS synthesizes text into a WAV (or the service's native audio type).
func (s *Server) handleTTS(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(w, r); !ok {
		return
	}
	var req struct {
		Text  string `json:"text"`
		Voice string `json:"voice"`
	}
	if err := decodeJSON(w, r, &req, 64*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if len([]rune(text)) > maxTTSTextChars {
		writeError(w, http.StatusBadRequest, "text exceeds the 4000 character limit")
		return
	}
	sy := s.ttsSynthesizer(r.Context())
	if sy == nil {
		writeError(w, http.StatusServiceUnavailable, "text-to-speech is not available")
		return
	}
	voice := strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = s.ttsVoice(r.Context())
	}
	audio, mimeType, err := sy.Synthesize(r.Context(), text, voice)
	if err != nil {
		s.writeSpeechError(w, "synthesize", err)
		return
	}
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "audio/wav"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

// handleSpeechStatus reports speech availability to any signed-in user so the
// composer can show/hide the mic and speaker buttons.
func (s *Server) handleSpeechStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := currentUser(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.speechStatus(r.Context()))
}

// speechStatus is the per-user speech availability payload.
func (s *Server) speechStatus(ctx context.Context) map[string]any {
	return map[string]any{
		"stt_available": s.sttConfigured(ctx),
		"stt_enabled":   s.sttSettingEnabled(ctx),
		"tts_available": s.ttsConfigured(ctx),
		"tts_enabled":   s.ttsSettingEnabled(ctx),
		"tts_voice":     s.ttsVoice(ctx),
	}
}

// speechSettings is the self-contained admin settings block for speech. It is
// merged into GET/PATCH /api/settings.
func (s *Server) speechSettings(ctx context.Context) map[string]any {
	return map[string]any{
		"stt_enabled":   s.sttSettingEnabled(ctx),
		"stt_available": s.sttConfigured(ctx),
		"stt_url":       s.sttURL(ctx),
		"tts_enabled":   s.ttsSettingEnabled(ctx),
		"tts_available": s.ttsConfigured(ctx),
		"tts_url":       s.ttsURL(ctx),
		"tts_voice":     s.ttsVoice(ctx),
	}
}

// writeSpeechError maps a speech engine error to an HTTP status.
func (s *Server) writeSpeechError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, speech.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "the speech service is unavailable")
	case errors.Is(err, speech.ErrEmptyAudio):
		writeError(w, http.StatusBadRequest, "the recording is empty")
	case errors.Is(err, speech.ErrEmptyText):
		writeError(w, http.StatusBadRequest, "text is required")
	default:
		s.log.Error("speech "+op, "error", err)
		writeError(w, http.StatusBadGateway, op+" failed")
	}
}
