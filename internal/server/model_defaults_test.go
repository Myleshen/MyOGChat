package server

import (
	"testing"

	"myogchat/internal/models"
	"myogchat/internal/ocr"
)

// Feature defaults name model ids by string, so a catalog retirement must break
// a test here rather than a live chat or OCR call.
func TestFeatureModelDefaultsAreRegistered(t *testing.T) {
	for name, id := range map[string]string{
		"titleModel":             titleModel,
		"defaultEnhanceModel":    defaultEnhanceModel,
		"ocr.DefaultVisionModel": ocr.DefaultVisionModel,
	} {
		if _, ok := models.ByID(id); !ok {
			t.Errorf("%s = %q is not a registered model", name, id)
		}
	}
	// A title model may reason (reasoning is billed inside max_tokens, so it can
	// return an empty answer) — that is why autoTitle retries once with a larger
	// budget. What must hold is that the retry is actually larger.
	if titleRetryTokens <= titleMaxTokens {
		t.Errorf("titleRetryTokens = %d, want > titleMaxTokens = %d", titleRetryTokens, titleMaxTokens)
	}
}
