package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"myogchat/internal/auth"
	"myogchat/internal/secret"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// fakeLinkwardenHandler is a minimal Linkwarden v1 API: it accepts only the
// given token and answers the lightweight validation search.
func fakeLinkwardenHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"links":[{"id":1,"name":"Go","url":"https://go.dev"}]}}`))
	})
	return mux
}

// newLinkwardenTestServer builds a full server with the Linkwarden env fallback.
func newLinkwardenTestServer(t *testing.T, envURL, envToken string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	a := auth.New(st, []byte("test-session-secret"))
	srv := New(st, upstream.NewClient(), a, nil, slog.Default(), testEncKey(), "",
		SearchDefaults{}, GeoDefaults{}, nil, nil, "", false, SpeechDefaults{},
		"", "", envURL, envToken)
	return srv, st
}

// TestLinkwardenConfigForResolution covers the per-user resolution order:
// the caller's own instance wins; the env fallback applies only when both env
// vars are set.
func TestLinkwardenConfigForResolution(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u1", "alice", "")

	// Neither own config nor env → not configured.
	if _, _, ok := srv.linkwardenConfigFor(ctx, u); ok {
		t.Fatal("expected no Linkwarden config")
	}

	// Env fallback requires both vars.
	srv.linkwardenURL = "https://env.example.com"
	if _, _, ok := srv.linkwardenConfigFor(ctx, u); ok {
		t.Fatal("env fallback must require both LINKWARDEN_URL and LINKWARDEN_TOKEN")
	}
	srv.linkwardenToken = "env-token"
	if url, tok, ok := srv.linkwardenConfigFor(ctx, u); !ok || url != "https://env.example.com" || tok != "env-token" {
		t.Fatalf("env fallback = %q, %q, %v", url, tok, ok)
	}

	// The caller's own instance takes precedence over the env fallback.
	enc, err := secret.Encrypt(srv.encKey, []byte("user-token"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := st.SetUserLinkwarden(ctx, u.ID, "https://mine.example.com", &enc, 1); err != nil {
		t.Fatalf("SetUserLinkwarden: %v", err)
	}
	stored, err := st.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if url, tok, ok := srv.linkwardenConfigFor(ctx, stored); !ok || url != "https://mine.example.com" || tok != "user-token" {
		t.Fatalf("own config = %q, %q, %v; want the user's instance", url, tok, ok)
	}

	// A client is built and all tools are registered from the caller's config.
	if _, ok := srv.linkwardenClientFor(ctx, stored); !ok {
		t.Fatal("expected a Linkwarden client for a configured user")
	}
	reg := srv.toolRegistry(ctx, stored)
	for _, name := range []string{"linkwarden_search", "linkwarden_tags", "linkwarden_collections", "linkwarden_save"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s not registered for a configured user", name)
		}
	}
}

// TestLinkwardenEndpoints exercises GET/PUT/DELETE /api/me/linkwarden.
func TestLinkwardenEndpoints(t *testing.T) {
	fake := httptest.NewServer(fakeLinkwardenHandler("token-1234"))
	defer fake.Close()

	srv, st := newLinkwardenTestServer(t, "", "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	// Initially unconfigured.
	rr := doJSON(t, h, cookie, http.MethodGet, "/api/me/linkwarden", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["configured"] != false {
		t.Fatalf("configured = %v, want false", got["configured"])
	}

	// A bad token is rejected by the lightweight check.
	rr = doJSON(t, h, cookie, http.MethodPut, "/api/me/linkwarden",
		map[string]any{"url": fake.URL, "token": "wrong"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT(bad token) status = %d, want 400; body %s", rr.Code, rr.Body.String())
	}

	// A good token is validated, encrypted, and stored.
	rr = doJSON(t, h, cookie, http.MethodPut, "/api/me/linkwarden",
		map[string]any{"url": fake.URL, "token": "token-1234"})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body %s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["configured"] != true || got["url"] != fake.URL {
		t.Fatalf("PUT response = %v", got)
	}
	if hint, _ := got["token_hint"].(string); hint != "1234" {
		t.Fatalf("token_hint = %q, want 1234", hint)
	}
	row, err := st.GetUserByID(context.Background(), "u-alice")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if row.LinkwardenURL == nil || *row.LinkwardenURL != fake.URL {
		t.Fatalf("stored url = %v", row.LinkwardenURL)
	}
	if row.LinkwardenTokenEnc == nil || *row.LinkwardenTokenEnc == "" || *row.LinkwardenTokenEnc == "token-1234" {
		t.Fatalf("stored token = %v, want ciphertext", row.LinkwardenTokenEnc)
	}

	// GET reflects the stored config.
	rr = doJSON(t, h, cookie, http.MethodGet, "/api/me/linkwarden", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["configured"] != true {
		t.Fatalf("GET after PUT configured = %v", got["configured"])
	}

	// Updating only the URL keeps the stored token.
	rr = doJSON(t, h, cookie, http.MethodPut, "/api/me/linkwarden",
		map[string]any{"url": fake.URL})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT(url only) status = %d, body %s", rr.Code, rr.Body.String())
	}

	// DELETE clears the caller's config.
	rr = doJSON(t, h, cookie, http.MethodDelete, "/api/me/linkwarden", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", rr.Code)
	}
	rr = doJSON(t, h, cookie, http.MethodGet, "/api/me/linkwarden", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["configured"] != false {
		t.Fatalf("GET after DELETE configured = %v", got["configured"])
	}
}

// TestLinkwardenEndpointEnvFallback reports the env fallback status.
func TestLinkwardenEndpointEnvFallback(t *testing.T) {
	fake := httptest.NewServer(fakeLinkwardenHandler("envtok"))
	defer fake.Close()

	srv, st := newLinkwardenTestServer(t, fake.URL, "envtok")
	seedActiveUser(t, st, "u-bob", "bob", false)
	h := srv.Handler()
	cookie := login(t, h, "bob")

	rr := doJSON(t, h, cookie, http.MethodGet, "/api/me/linkwarden", nil)
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["configured"] != true || got["url"] != fake.URL {
		t.Fatalf("env fallback status = %v", got)
	}
}
