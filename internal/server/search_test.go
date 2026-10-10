package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"myogchat/internal/store"
)

func newSearchTestServer(t *testing.T, defaults SearchDefaults) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{st: st, encKey: testEncKey(), log: slog.Default(), search: defaults}
}

func TestSearchConfigDefaultsAndOverrides(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{
		Provider: "auto", TavilyKey: "tav", BraveKey: "brave-env",
		MaxResults: 7, Region: "us-en", SafeSearch: "moderate",
	})
	ctx := context.Background()

	cfg := srv.searchConfig(ctx)
	if cfg.TavilyKey != "tav" || cfg.BraveKey != "brave-env" {
		t.Fatalf("cfg keys = %q / %q", cfg.TavilyKey, cfg.BraveKey)
	}
	if got := cfg.Options().MaxResults; got != 7 {
		t.Fatalf("MaxResults = %d, want 7", got)
	}

	// DB provider selection overrides the env default.
	if err := srv.setSearchProvider(ctx, "ddg"); err != nil {
		t.Fatalf("setSearchProvider: %v", err)
	}
	if cfg := srv.searchConfig(ctx); cfg.Provider != "ddg" {
		t.Fatalf("provider = %q, want ddg", cfg.Provider)
	}
	// Unknown selectors normalize to auto.
	if err := srv.setSearchProvider(ctx, "bogus"); err != nil {
		t.Fatalf("setSearchProvider(bogus): %v", err)
	}
	if cfg := srv.searchConfig(ctx); cfg.Provider != "auto" {
		t.Fatalf("provider = %q, want auto", cfg.Provider)
	}
}

func TestSetBraveKeyEncryptedAndPrecedence(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto", BraveKey: "brave-env"})
	ctx := context.Background()

	// Env fallback before any DB key is set.
	if key, ok := srv.braveKey(ctx); !ok || key != "brave-env" {
		t.Fatalf("braveKey = %q, %v; want brave-env", key, ok)
	}

	// A stored key is encrypted at rest and takes precedence over env.
	if err := srv.setBraveKey(ctx, "brave-db"); err != nil {
		t.Fatalf("setBraveKey: %v", err)
	}
	stored, _, _ := srv.st.GetSetting(ctx, settingBraveKey)
	if stored == "" || stored == "brave-db" {
		t.Fatalf("stored key = %q, want ciphertext", stored)
	}
	if key, ok := srv.braveKey(ctx); !ok || key != "brave-db" {
		t.Fatalf("braveKey = %q, %v; want brave-db", key, ok)
	}

	// Clearing falls back to env again.
	if err := srv.setBraveKey(ctx, ""); err != nil {
		t.Fatalf("setBraveKey(clear): %v", err)
	}
	if key, ok := srv.braveKey(ctx); !ok || key != "brave-env" {
		t.Fatalf("braveKey after clear = %q, %v; want brave-env", key, ok)
	}
}

func TestIntegrationSettingsSearchFields(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto", TavilyKey: "t", BraveKey: "b"})
	settings := srv.integrationSettings(context.Background())
	if settings["tavily_configured"] != true {
		t.Errorf("tavily_configured = %v", settings["tavily_configured"])
	}
	if settings["brave_configured"] != true {
		t.Errorf("brave_configured = %v", settings["brave_configured"])
	}
	if settings["search_provider"] != "auto" {
		t.Errorf("search_provider = %v", settings["search_provider"])
	}
	if settings["search_active"] != "tavily" {
		t.Errorf("search_active = %v, want tavily", settings["search_active"])
	}
}

func TestToolRegistryRegistersWebSearchWithoutKeys(t *testing.T) {
	// DuckDuckGo is keyless, so web_search must be registered even with no keys.
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto"})
	reg := srv.toolRegistry(context.Background(), "")
	if _, ok := reg.Get("web_search"); !ok {
		t.Fatal("web_search not registered with no keys configured")
	}
}

func TestToolRegistrySearchActiveDDG(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto"})
	settings := srv.integrationSettings(context.Background())
	if settings["search_active"] != "ddg" {
		t.Errorf("search_active = %v, want ddg", settings["search_active"])
	}
	if settings["brave_configured"] != false {
		t.Errorf("brave_configured = %v, want false", settings["brave_configured"])
	}
}
