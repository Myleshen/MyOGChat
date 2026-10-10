package server

import (
	"context"

	"myogchat/internal/ocr"
	"myogchat/internal/store"
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
//
// A KeyedEngine (the vision engine, feature 2.13) needs a per-request bearer
// key: the caller's resolved key (BYOK -> group -> shared server key, §2.1/2.2)
// is bound here. When no key resolves, the returned function errors so the
// upload is rejected with a clear message (mapped to extract.ErrOCR) rather
// than silently failing.
func (s *Server) ocrFunc(ctx context.Context, u store.User) ocr.OCRFunc {
	if s.ocrEngine == nil || !s.ocrSettingEnabled(ctx) {
		return nil
	}
	eng := s.ocrEngine
	if ke, ok := eng.(ocr.KeyedEngine); ok {
		key, err := s.resolveKey(ctx, u)
		if err != nil {
			return func([]byte) (string, error) { return "", err }
		}
		eng = ke.WithKey(key, newUUID())
	}
	return ocr.Func(ctx, eng)
}

// ocrSettings is the self-contained admin settings block for OCR (features 2.5
// and 2.13). It is merged into the /api/settings response and is deliberately
// small so it can be relocated when the settings UI is restructured.
func (s *Server) ocrSettings(ctx context.Context) map[string]any {
	return map[string]any{
		"ocr_enabled":          s.ocrSettingEnabled(ctx),
		"ocr_available":        s.ocrEngine != nil,
		"ocr_engine":           s.ocrEngineName, // tesseract | vision | auto | none
		"ocr_vision_available": s.ocrVisionAvailable,
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
