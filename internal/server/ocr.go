package server

import (
	"context"

	"myogchat/internal/ocr"
)

// settingOCREnabled is the DB-backed global toggle for image OCR (feature 2.5).
// Default true; admins flip it via GET/PATCH /api/settings.
const settingOCREnabled = "ocr_enabled"

// ocrSettingEnabled reports whether the admin setting allows OCR (default true,
// so a fresh database behaves as "on" until an admin turns it off).
func (s *Server) ocrSettingEnabled(ctx context.Context) bool {
	v, ok, err := s.st.GetSetting(ctx, settingOCREnabled)
	if err != nil || !ok {
		return true
	}
	return v == "1"
}

// ocrFunc returns the OCR function injected into extraction, or nil when OCR is
// unavailable (no engine configured at startup) or disabled by the admin
// setting. A nil function makes internal/extract reject images with
// ErrUnsupported.
func (s *Server) ocrFunc(ctx context.Context) ocr.OCRFunc {
	if s.ocrEngine == nil || !s.ocrSettingEnabled(ctx) {
		return nil
	}
	return ocr.Func(ctx, s.ocrEngine)
}

// ocrSettings is the self-contained admin settings block for OCR (feature 2.5).
// It is merged into the /api/settings response and is deliberately small so it
// can be relocated when the settings UI is restructured.
func (s *Server) ocrSettings(ctx context.Context) map[string]any {
	return map[string]any{
		"ocr_enabled":   s.ocrSettingEnabled(ctx),
		"ocr_available": s.ocrEngine != nil,
	}
}

// setOCREnabled persists the global OCR toggle.
func (s *Server) setOCREnabled(ctx context.Context, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return s.st.SetSetting(ctx, settingOCREnabled, v)
}
