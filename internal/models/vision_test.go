package models

import "testing"

func TestVisionFlag(t *testing.T) {
	// The recommended OCR vision models must be flagged so main.go can validate
	// MYOGCHAT_OCR_VISION_MODEL.
	for _, id := range []string{
		"deepseek-v4-flash-vision-exp",
		"deepseek-v4.1-flash",
		"gpt-6-luna",
		"gpt-5.6-luna",
	} {
		m, ok := ByID(id)
		if !ok {
			t.Errorf("model %q not registered", id)
			continue
		}
		if !m.Vision {
			t.Errorf("model %q should be Vision=true", id)
		}
	}
	// A text-only model must not be flagged.
	if m, ok := ByID("deepseek-v4-flash"); !ok || m.Vision {
		t.Errorf("deepseek-v4-flash Vision = %v, want false", m.Vision)
	}
}
