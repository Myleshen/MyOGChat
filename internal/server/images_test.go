package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"myogchat/internal/auth"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// fakeComfyPNG is returned by the fake ComfyUI /view endpoint.
var fakeComfyPNG = []byte("\x89PNG\r\n\x1a\nmyogchat-fake-png")

// fakeComfyHandler is a minimal ComfyUI native API used by the server tests.
func fakeComfyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": "pid-1"})
	})
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"pid-1": map[string]any{
				"outputs": map[string]any{
					"9": map[string]any{
						"images": []map[string]string{
							{"filename": "myogchat_00001_.png", "subfolder": "", "type": "output"},
						},
					},
				},
				"status": map[string]any{"completed": true, "status_str": "success"},
			},
		})
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(fakeComfyPNG)
	})
	mux.HandleFunc("GET /system_stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"system":{}}`))
	})
	return mux
}

// newImageTestServer builds a full server (auth + handlers) pointed at comfyURL.
func newImageTestServer(t *testing.T, comfyURL string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	a := auth.New(st, []byte("test-session-secret"))
	srv := New(st, upstream.NewClient(), a, nil, slog.Default(), testEncKey(), "", SearchDefaults{}, GeoDefaults{}, nil, nil, "", false, SpeechDefaults{}, comfyURL, "", "", "")
	return srv, st
}

// seedActiveUser inserts an active user with a known password.
func seedActiveUser(t *testing.T, st *store.Store, id, username string, admin bool) {
	t.Helper()
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u := &store.User{
		ID: id, Username: username, Email: username + "@example.com",
		PasswordHash: hash, IsAdmin: admin, Status: "active", CreatedAt: 1, UpdatedAt: 1,
	}
	if err := st.InsertUser(context.Background(), u); err != nil {
		t.Fatalf("InsertUser(%s): %v", username, err)
	}
}

// login returns the session cookie for a seeded user.
func login(t *testing.T, h http.Handler, username string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"identifier": username, "password": "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, body %s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "myogchat" {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

func doJSON(t *testing.T, h http.Handler, cookie *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestImageCreateFetchDelete(t *testing.T) {
	comfy := httptest.NewServer(fakeComfyHandler())
	defer comfy.Close()

	srv, st := newImageTestServer(t, comfy.URL)
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	// A conversation owned by alice (inserted directly to avoid model setup).
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: "c1", UserID: strptr("u-alice"), Model: "glm-5.3-flash", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}

	rr := doJSON(t, h, cookie, http.MethodPost, "/api/images", map[string]any{
		"prompt": "a red fox in a snowy forest", "size": "512x512", "conversation_id": "c1",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /api/images status = %d, body %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create response missing id: %s", rr.Body.String())
	}
	if created["width"] != float64(512) || created["height"] != float64(512) {
		t.Errorf("dims = %vx%v, want 512x512", created["width"], created["height"])
	}
	if created["mime"] != "image/png" {
		t.Errorf("mime = %v, want image/png", created["mime"])
	}
	if created["url"] != "/api/images/"+id {
		t.Errorf("url = %v", created["url"])
	}

	// The image bytes round-trip.
	rr = doJSON(t, h, cookie, http.MethodGet, "/api/images/"+id, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET image status = %d", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), fakeComfyPNG) {
		t.Errorf("image bytes = %q, want %q", rr.Body.Bytes(), fakeComfyPNG)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}

	// The image was folded into the conversation as a user + assistant message.
	convRR := doJSON(t, h, cookie, http.MethodGet, "/api/conversations/c1", nil)
	if convRR.Code != http.StatusOK {
		t.Fatalf("GET conversation status = %d", convRR.Code)
	}
	var conv struct {
		Messages []store.Message `json:"messages"`
	}
	if err := json.Unmarshal(convRR.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if len(conv.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (user + assistant)", len(conv.Messages))
	}
	if len(conv.Messages[1].Images) != 1 || conv.Messages[1].Images[0].ID != id {
		t.Errorf("assistant message images = %+v, want one ref to %s", conv.Messages[1].Images, id)
	}

	// Delete, then it is gone.
	rr = doJSON(t, h, cookie, http.MethodDelete, "/api/images/"+id, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", rr.Code)
	}
	if rr := doJSON(t, h, cookie, http.MethodGet, "/api/images/"+id, nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET after delete status = %d, want 404", rr.Code)
	}
}

func TestImageOwnerScoping(t *testing.T) {
	comfy := httptest.NewServer(fakeComfyHandler())
	defer comfy.Close()

	srv, st := newImageTestServer(t, comfy.URL)
	seedActiveUser(t, st, "u-alice", "alice", false)
	seedActiveUser(t, st, "u-bob", "bob", false)
	h := srv.Handler()
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")

	rr := doJSON(t, h, alice, http.MethodPost, "/api/images", map[string]any{"prompt": "a cat"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("alice create status = %d, body %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	id := created["id"].(string)

	// Bob cannot read or delete alice's image.
	if rr := doJSON(t, h, bob, http.MethodGet, "/api/images/"+id, nil); rr.Code != http.StatusNotFound {
		t.Errorf("bob GET status = %d, want 404", rr.Code)
	}
	if rr := doJSON(t, h, bob, http.MethodDelete, "/api/images/"+id, nil); rr.Code != http.StatusNotFound {
		t.Errorf("bob DELETE status = %d, want 404", rr.Code)
	}
	// Alice still sees it.
	if rr := doJSON(t, h, alice, http.MethodGet, "/api/images/"+id, nil); rr.Code != http.StatusOK {
		t.Errorf("alice GET status = %d, want 200", rr.Code)
	}

	// An unknown conversation id is rejected before any GPU work.
	if rr := doJSON(t, h, alice, http.MethodPost, "/api/images", map[string]any{
		"prompt": "x", "conversation_id": "nope",
	}); rr.Code != http.StatusNotFound {
		t.Errorf("unknown conversation status = %d, want 404", rr.Code)
	}
}

func TestImageCreateNotConfigured(t *testing.T) {
	srv, st := newImageTestServer(t, "") // no imagegen URL
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	rr := doJSON(t, h, cookie, http.MethodPost, "/api/images", map[string]any{"prompt": "x"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (not configured); body %s", rr.Code, rr.Body.String())
	}
}

func TestImageCreateRejectsEmptyPrompt(t *testing.T) {
	comfy := httptest.NewServer(fakeComfyHandler())
	defer comfy.Close()
	srv, st := newImageTestServer(t, comfy.URL)
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	if rr := doJSON(t, h, cookie, http.MethodPost, "/api/images", map[string]any{"prompt": "   "}); rr.Code != http.StatusBadRequest {
		t.Errorf("empty prompt status = %d, want 400", rr.Code)
	}
}

func TestImagegenSettingsEncryptionAndPrecedence(t *testing.T) {
	comfy := httptest.NewServer(fakeComfyHandler())
	defer comfy.Close()

	srv, _ := newImageTestServer(t, "http://env.example:8188")
	ctx := context.Background()

	// Env defaults apply when nothing is stored.
	if url, token, ok := srv.imagegenConfig(ctx); !ok || url != "http://env.example:8188" || token != "" {
		t.Fatalf("imagegenConfig = %q, %q, %v; want env url, no token", url, token, ok)
	}

	// A stored URL/token overrides env, and the token is encrypted at rest.
	if err := srv.setImagegen(ctx, comfy.URL, strptr("db-token")); err != nil {
		t.Fatalf("setImagegen: %v", err)
	}
	stored, _, _ := srv.st.GetSetting(ctx, settingImagegenToken)
	if stored == "" || stored == "db-token" {
		t.Fatalf("stored token = %q, want ciphertext", stored)
	}
	url, token, ok := srv.imagegenConfig(ctx)
	if !ok || url != comfy.URL || token != "db-token" {
		t.Fatalf("imagegenConfig = %q, %q, %v; want stored values", url, token, ok)
	}

	// Settings expose configured + available + a token hint (never the token).
	s := srv.imagegenSettings(ctx)
	if s["imagegen_configured"] != true {
		t.Errorf("imagegen_configured = %v", s["imagegen_configured"])
	}
	if s["imagegen_available"] != true {
		t.Errorf("imagegen_available = %v", s["imagegen_available"])
	}
	if s["imagegen_token_hint"] != "oken" {
		t.Errorf("imagegen_token_hint = %v, want oken", s["imagegen_token_hint"])
	}
	if s["imagegen_url"] != comfy.URL {
		t.Errorf("imagegen_url = %v, want %s", s["imagegen_url"], comfy.URL)
	}

	// Clearing the token falls back to no token; clearing the URL unconfigures.
	if err := srv.setImagegen(ctx, comfy.URL, strptr("")); err != nil {
		t.Fatalf("setImagegen(clear token): %v", err)
	}
	if _, token, _ := srv.imagegenConfig(ctx); token != "" {
		t.Errorf("token after clear = %q, want empty", token)
	}
	if err := srv.setImagegen(ctx, "", nil); err != nil {
		t.Fatalf("setImagegen(clear url): %v", err)
	}
	// Clearing the stored URL falls back to the env default (env is still set).
	if url, _, ok := srv.imagegenConfig(ctx); !ok || url != "http://env.example:8188" {
		t.Errorf("imagegenConfig after clearing url = %q, %v; want env fallback", url, ok)
	}
}

// TestSettingsPatchImagegen verifies the admin PATCH path stores and reports.
func TestSettingsPatchImagegen(t *testing.T) {
	comfy := httptest.NewServer(fakeComfyHandler())
	defer comfy.Close()
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-admin", "admin", true)
	h := srv.Handler()
	cookie := login(t, h, "admin")

	rr := doJSON(t, h, cookie, http.MethodPatch, "/api/settings", map[string]any{
		"imagegen_url": comfy.URL, "imagegen_token": "tok-1234",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /api/settings status = %d, body %s", rr.Code, rr.Body.String())
	}
	var s map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if s["imagegen_configured"] != true || s["imagegen_available"] != true {
		t.Errorf("settings = configured:%v available:%v", s["imagegen_configured"], s["imagegen_available"])
	}
	if s["imagegen_url"] != comfy.URL {
		t.Errorf("imagegen_url = %v, want %s", s["imagegen_url"], comfy.URL)
	}

	// The stored token is encrypted, not echoed back.
	stored, _, _ := st.GetSetting(context.Background(), settingImagegenToken)
	if strings.Contains(stored, "tok-1234") {
		t.Errorf("stored token leaks plaintext: %q", stored)
	}
}

func strptr(s string) *string { return &s }

// TestImageEnhanceFallsBackWithoutKey checks the enhancer degrades gracefully:
// with no usable key it returns a template expansion (enhanced:false) so the UI
// can still generate.
func TestImageEnhanceFallsBackWithoutKey(t *testing.T) {
	srv, st := newImageTestServer(t, "")
	seedActiveUser(t, st, "u-alice", "alice", false)
	h := srv.Handler()
	cookie := login(t, h, "alice")

	rr := doJSON(t, h, cookie, http.MethodPost, "/api/images/enhance", map[string]any{
		"brief": "a cat on a roof at sunset",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("enhance status = %d, body %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["enhanced"] != false {
		t.Errorf("enhanced = %v, want false (no key)", out["enhanced"])
	}
	p, _ := out["prompt"].(string)
	if !strings.Contains(p, "a cat on a roof at sunset") || !strings.Contains(p, "highly detailed") {
		t.Errorf("fallback prompt = %q, want brief + template", p)
	}

	// An empty brief is rejected.
	if rr := doJSON(t, h, cookie, http.MethodPost, "/api/images/enhance", map[string]any{"brief": "  "}); rr.Code != http.StatusBadRequest {
		t.Errorf("empty brief status = %d, want 400", rr.Code)
	}
}

func TestSanitizeImagePrompt(t *testing.T) {
	cases := map[string]string{
		"  \"Prompt: a red  fox\nin snow\"  ": "a red fox in snow",
		"Here is the prompt: a wooden boat":   "a wooden boat",
		"a lone lighthouse at dusk":           "a lone lighthouse at dusk",
	}
	for in, want := range cases {
		if got := sanitizeImagePrompt(in); got != want {
			t.Errorf("sanitizeImagePrompt(%q) = %q, want %q", in, got, want)
		}
	}
}
