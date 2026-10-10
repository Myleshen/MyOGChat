package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"myogchat/internal/store"
)

// TestGenerateImageTool checks the model-facing generate_image tool (4.3b) is
// registered only when a backend + collector are present, and that running it
// stores the image and reports it through the collector.
func TestGenerateImageTool(t *testing.T) {
	comfy := httptest.NewServer(fakeComfyHandler())
	defer comfy.Close()

	srv, st := newImageTestServer(t, comfy.URL)
	ctx := context.Background()
	seedActiveUser(t, st, "u-img", "img", false)
	u := store.User{ID: "u-img", Username: "img"}

	var collected []store.GeneratedImage
	reg := srv.toolRegistryWithImage(ctx, u, func(img store.GeneratedImage) {
		collected = append(collected, img)
	})
	tool, ok := reg.Get("generate_image")
	if !ok {
		t.Fatal("generate_image not registered when configured")
	}
	res, err := tool.Run(ctx, json.RawMessage(`{"prompt":"a red fox in a snowy forest"}`))
	if err != nil {
		t.Fatalf("generate_image.Run: %v", err)
	}
	if !strings.Contains(res, "Generated") {
		t.Errorf("result = %q", res)
	}
	if len(collected) != 1 || collected[0].ID == "" {
		t.Fatalf("collector = %+v, want one image", collected)
	}
	// The image was persisted owner-scoped.
	if _, err := st.GetImage(ctx, collected[0].ID, u.ID); err != nil {
		t.Errorf("stored image not found: %v", err)
	}

	// Empty prompt is rejected.
	if _, err := tool.Run(ctx, json.RawMessage(`{"prompt":"  "}`)); err == nil {
		t.Error("empty prompt should error")
	}

	// Not registered without a collector (e.g. regeneration w/o image support).
	if _, ok := srv.toolRegistry(ctx, u).Get("generate_image"); ok {
		t.Error("generate_image registered without a collector")
	}

	// Not registered when no backend is configured.
	srv2, _ := newImageTestServer(t, "")
	if _, ok := srv2.toolRegistryWithImage(ctx, u, func(store.GeneratedImage) {}).Get("generate_image"); ok {
		t.Error("generate_image registered without a backend")
	}
}
