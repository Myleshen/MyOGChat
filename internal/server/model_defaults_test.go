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
	// The title call must not reason: reasoning tokens are billed inside
	// max_tokens, and a model that spends the budget thinking returns no title.
	if m, _ := models.ByID(titleModel); m.Thinking {
		t.Errorf("titleModel = %q reasons; use a non-reasoning model", titleModel)
	}
}
