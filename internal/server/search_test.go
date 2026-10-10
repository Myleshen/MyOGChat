package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"myogchat/internal/store"
	"myogchat/internal/tools"
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
	if settings["brave_mode"] != "web" {
		t.Errorf("brave_mode = %v, want web", settings["brave_mode"])
	}
}

func TestIntegrationSettingsSerperField(t *testing.T) {
	// The server-wide Serper key (SERPER_API_KEY) surfaces in the admin view.
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto", SerperKey: "serper-env"})
	settings := srv.integrationSettings(context.Background())
	if settings["serper_configured"] != true {
		t.Errorf("serper_configured = %v, want true", settings["serper_configured"])
	}
	// With a Serper key and no Tavily/Brave keys, Serper answers first.
	if settings["search_active"] != "serper" {
		t.Errorf("search_active = %v, want serper", settings["search_active"])
	}
}

func TestSearchConfigBraveMode(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto", BraveKey: "b"})
	ctx := context.Background()
	if srv.searchConfig(ctx).BraveContext {
		t.Error("default Brave mode should be web")
	}
	if err := srv.setBraveMode(ctx, "context"); err != nil {
		t.Fatalf("setBraveMode(context): %v", err)
	}
	if !srv.searchConfig(ctx).BraveContext {
		t.Error("brave_mode=context should enable llm/context mode")
	}
	// Unknown values normalize back to web.
	if err := srv.setBraveMode(ctx, "bogus"); err != nil {
		t.Fatalf("setBraveMode(bogus): %v", err)
	}
	if srv.searchConfig(ctx).BraveContext {
		t.Error("unknown brave_mode should normalize to web")
	}
}

func TestSetUserSerperKeyEncryptedAndPrecedence(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto", SerperKey: "serper-env"})
	ctx := context.Background()

	u := store.User{ID: "u-serper", Username: "alice", Email: "a@example.com",
		PasswordHash: "x", Status: "active", CreatedAt: 1, UpdatedAt: 1}
	if err := srv.st.InsertUser(ctx, &u); err != nil {
		t.Fatalf("InsertUser: %v", err)
	}

	// Env fallback before any per-user key.
	if cfg := srv.searchConfigFor(ctx, u); cfg.SerperKey != "serper-env" {
		t.Fatalf("env serper = %q, want serper-env", cfg.SerperKey)
	}

	// A stored per-user key is encrypted at rest and takes precedence over env.
	if err := srv.setUserSerperKey(ctx, u, "serper-db"); err != nil {
		t.Fatalf("setUserSerperKey: %v", err)
	}
	stored, err := srv.st.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if stored.SerperKeyEnc == nil || *stored.SerperKeyEnc == "" || *stored.SerperKeyEnc == "serper-db" {
		t.Fatalf("stored serper key = %v, want ciphertext", stored.SerperKeyEnc)
	}
	if cfg := srv.searchConfigFor(ctx, stored); cfg.SerperKey != "serper-db" {
		t.Fatalf("per-user serper = %q, want serper-db", cfg.SerperKey)
	}
	// The admin/global view does not leak the per-user key.
	if cfg := srv.searchConfig(ctx); cfg.SerperKey != "serper-env" {
		t.Fatalf("global serper = %q, want serper-env", cfg.SerperKey)
	}

	// Clearing falls back to env again.
	if err := srv.setUserSerperKey(ctx, u, ""); err != nil {
		t.Fatalf("setUserSerperKey(clear): %v", err)
	}
	stored, _ = srv.st.GetUserByID(ctx, u.ID)
	if stored.SerperKeyEnc != nil && *stored.SerperKeyEnc != "" {
		t.Fatalf("serper key not cleared: %v", stored.SerperKeyEnc)
	}
	if cfg := srv.searchConfigFor(ctx, stored); cfg.SerperKey != "serper-env" {
		t.Fatalf("serper after clear = %q, want serper-env", cfg.SerperKey)
	}
}

func TestUserSerperKeyBuildsProviderChain(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto"})
	ctx := context.Background()
	u := store.User{ID: "u2", Username: "bob", Email: "b@example.com",
		PasswordHash: "x", Status: "active", CreatedAt: 1, UpdatedAt: 1}
	if err := srv.st.InsertUser(ctx, &u); err != nil {
		t.Fatalf("InsertUser: %v", err)
	}
	// With no keys anywhere, DDG answers.
	if got := tools.SearchActive(srv.searchConfigFor(ctx, u)); got != "ddg" {
		t.Fatalf("active = %q, want ddg", got)
	}
	// A per-user Serper key makes Serper the active provider for that user.
	if err := srv.setUserSerperKey(ctx, u, "serper-db"); err != nil {
		t.Fatalf("setUserSerperKey: %v", err)
	}
	stored, _ := srv.st.GetUserByID(ctx, u.ID)
	if got := tools.SearchActive(srv.searchConfigFor(ctx, stored)); got != "serper" {
		t.Fatalf("active = %q, want serper", got)
	}
}

func TestToolRegistryRegistersWebSearchWithoutKeys(t *testing.T) {
	// DuckDuckGo is keyless, so web_search must be registered even with no keys.
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto"})
	reg := srv.toolRegistry(context.Background(), store.User{})
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
