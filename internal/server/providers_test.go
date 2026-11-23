package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/models"
	"myogchat/internal/secret"
	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// recordedRequest is one request a fake upstream received.
type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type requestLog struct {
	mu   sync.Mutex
	reqs []recordedRequest
	// modelsStatus/modelsBody configure the fake /v1/models response. Tests set
	// them before issuing a request, so no lock is needed.
	modelsStatus int
	modelsBody   string
}

func (l *requestLog) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, recordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
}

func (l *requestLog) last(t *testing.T) recordedRequest {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reqs) == 0 {
		t.Fatal("fake upstream received no requests")
	}
	return l.reqs[len(l.reqs)-1]
}

// newFakeProviderServer answers GET /v1/models and POST /v1/chat/completions.
// modelsStatus/modelsBody are re-read on every call, so a test can change them
// between requests through the returned pointers.
func newFakeProviderServer(t *testing.T, chatSSE string) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(log.modelsStatus)
			_, _ = io.WriteString(w, log.modelsBody)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatSSE)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, log
}

// providerMux exposes only the provider handlers, with a user already attached.
func providerMux(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me/providers", srv.handleListProviders)
	mux.HandleFunc("POST /api/me/providers", srv.handleCreateProvider)
	mux.HandleFunc("PATCH /api/me/providers/{id}", srv.handleUpdateProvider)
	mux.HandleFunc("POST /api/me/providers/{id}/models", srv.handleRefreshProviderModels)
	mux.HandleFunc("DELETE /api/me/providers/{id}", srv.handleDeleteProvider)
	return mux
}

func providerRequest(t *testing.T, srv *Server, u store.User, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(auth.WithUser(r.Context(), u))
	rec := httptest.NewRecorder()
	providerMux(srv).ServeHTTP(rec, r)
	return rec
}

// seedProvider inserts a provider with an encrypted key.
func seedProvider(t *testing.T, st *store.Store, u store.User, slug, baseURL, apiKey string) store.Provider {
	t.Helper()
	enc, err := secret.Encrypt(testEncKey(), []byte(apiKey))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	p := store.Provider{
		ID: newUUID(), UserID: u.ID, Name: slug, Slug: slug, BaseURL: baseURL,
		KeyEnc: enc, Models: []store.ProviderModel{{ID: "meta/llama-3.1-8b", Name: "Llama 3.1 8B"}},
		CreatedAt: 1,
	}
	if err := st.CreateUserProvider(context.Background(), &p); err != nil {
		t.Fatalf("CreateUserProvider: %v", err)
	}
	return p
}

func decodeProvider(t *testing.T, rec *httptest.ResponseRecorder) providerView {
	t.Helper()
	var p providerView
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode provider: %v (body %s)", err, rec.Body.String())
	}
	return p
}

// TestProviderCRUDAndSlugUniquing covers create/list/delete, slug uniquing and
// cross-user 404/empty behaviour.
func TestProviderCRUDAndSlugUniquing(t *testing.T) {
	srv, st := newKeyTestServer(t)
	alice := seedKeyUser(t, st, "u-alice", "alice", "")
	bob := seedKeyUser(t, st, "u-bob", "bob", "")

	ts, log := newFakeProviderServer(t, "")
	log.modelsStatus = http.StatusOK
	log.modelsBody = `{"data":[{"id":"meta/llama-3.1-8b","name":"Llama 3.1 8B"},{"id":"nvidia/nemotron"}]}`

	body := `{"name":"NVIDIA NIM","base_url":"` + ts.URL + `/v1/","api_key":"nvapi-test"}`
	rec := providerRequest(t, srv, alice, http.MethodPost, "/api/me/providers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	first := decodeProvider(t, rec)
	if first.Slug != "nvidia-nim" || !first.HasKey {
		t.Errorf("provider = %+v", first)
	}
	if len(first.Models) != 2 || first.Models[1].Name != "nvidia/nemotron" {
		t.Errorf("models = %+v (name should fall back to id)", first.Models)
	}
	if got := log.last(t).Header.Get("Authorization"); got != "Bearer nvapi-test" {
		t.Errorf("models fetch Authorization = %q", got)
	}

	// A second provider with the same name gets a unique slug.
	rec = providerRequest(t, srv, alice, http.MethodPost, "/api/me/providers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create = %d (body %s)", rec.Code, rec.Body.String())
	}
	second := decodeProvider(t, rec)
	if second.Slug != "nvidia-nim-2" {
		t.Errorf("second slug = %q, want nvidia-nim-2", second.Slug)
	}

	// The list never leaks the key or its ciphertext.
	rec = providerRequest(t, srv, alice, http.MethodGet, "/api/me/providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "nvapi-test") || strings.Contains(rec.Body.String(), "key_enc") {
		t.Errorf("list leaked the key: %s", rec.Body.String())
	}
	var list struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Providers) != 2 {
		t.Errorf("providers = %+v, want 2", list.Providers)
	}

	// A foreign user sees an empty list and gets 404 on delete.
	rec = providerRequest(t, srv, bob, http.MethodGet, "/api/me/providers", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Providers) != 0 {
		t.Errorf("bob list = %s, %v; want empty", rec.Body.String(), err)
	}
	if rec := providerRequest(t, srv, bob, http.MethodDelete, "/api/me/providers/"+first.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign delete = %d, want 404", rec.Code)
	}

	if rec := providerRequest(t, srv, alice, http.MethodDelete, "/api/me/providers/"+first.ID, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", rec.Code)
	}
	if rec := providerRequest(t, srv, alice, http.MethodDelete, "/api/me/providers/"+first.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("repeat delete = %d, want 404", rec.Code)
	}
}

// TestProviderCreateValidation rejects bad names, URLs and keys, and stores
// nothing on any rejected request.
func TestProviderCreateValidation(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	longName := strings.Repeat("a", maxProviderNameRunes+1)
	cases := []struct{ label, body string }{
		{"empty name", `{"name":"","base_url":"https://x.example/v1","api_key":"k"}`},
		{"long name", `{"name":"` + longName + `","base_url":"https://x.example/v1","api_key":"k"}`},
		{"ftp scheme", `{"name":"x","base_url":"ftp://x.example/v1","api_key":"k"}`},
		{"relative url", `{"name":"x","base_url":"/v1","api_key":"k"}`},
		{"query", `{"name":"x","base_url":"https://x.example/v1?key=1","api_key":"k"}`},
		{"fragment", `{"name":"x","base_url":"https://x.example/v1#frag","api_key":"k"}`},
		{"empty key", `{"name":"x","base_url":"https://x.example/v1","api_key":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			rec := providerRequest(t, srv, u, http.MethodPost, "/api/me/providers", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
	providers, err := st.ListUserProviders(context.Background(), u.ID)
	if err != nil || len(providers) != 0 {
		t.Errorf("providers after rejects = %+v, %v; want none", providers, err)
	}
}

// TestProviderCreateFailurePersistsNothing returns the upstream message with
// 400 and stores nothing.
func TestProviderCreateFailurePersistsNothing(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ts, log := newFakeProviderServer(t, "")
	log.modelsStatus = http.StatusUnauthorized
	log.modelsBody = `{"error":{"message":"invalid credential"}}`

	body := `{"name":"NVIDIA","base_url":"` + ts.URL + `/v1","api_key":"bad"}`
	rec := providerRequest(t, srv, u, http.MethodPost, "/api/me/providers", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream HTTP 401") || !strings.Contains(rec.Body.String(), "invalid credential") {
		t.Errorf("error = %s, want upstream status and message", rec.Body.String())
	}
	providers, err := st.ListUserProviders(context.Background(), u.ID)
	if err != nil || len(providers) != 0 {
		t.Errorf("providers = %+v, %v; want none", providers, err)
	}
}

// TestProviderKeyErrorIsReadable keeps a failed key check to one human line: a
// JSON error envelope becomes its message, an HTML page a short snippet.
func TestProviderKeyErrorIsReadable(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ts, log := newFakeProviderServer(t, "")

	log.modelsStatus = http.StatusUnauthorized
	log.modelsBody = `{"error":{"message":"invalid_api_key"}}`
	body := `{"name":"NVIDIA","base_url":"` + ts.URL + `/v1","api_key":"bad"}`
	rec := providerRequest(t, srv, u, http.MethodPost, "/api/me/providers", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "invalid_api_key") || strings.Contains(msg, `{"`) {
		t.Errorf("error = %s, want the envelope's message and no JSON", msg)
	}

	log.modelsStatus = http.StatusBadGateway
	log.modelsBody = "<html><body><h1>502 Bad Gateway</h1>" + strings.Repeat("x", 400) + "</body></html>"
	rec = providerRequest(t, srv, u, http.MethodPost, "/api/me/providers", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "502 Bad Gateway") || !strings.Contains(msg, "upstream HTTP 502") || strings.Contains(msg, "</html>") {
		t.Errorf("error = %s, want a truncated HTML snippet and the status", msg)
	}
}

// errorMessage returns the message inside the {"error":…} envelope.
func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body.String())
	}
	return env.Error
}

// TestProviderRefreshKeepsOldListOn502 keeps the cached catalog when the
// upstream fails a refresh.
func TestProviderRefreshKeepsOldListOn502(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ts, log := newFakeProviderServer(t, "")
	log.modelsStatus = http.StatusOK
	log.modelsBody = `{"data":[{"id":"meta/llama-3.1-8b","name":"Llama 3.1 8B"}]}`
	p := seedProvider(t, st, u, "nvidia-nim", ts.URL+"/v1", "nvapi-key")

	log.modelsStatus = http.StatusInternalServerError
	log.modelsBody = `{"error":"boom"}`
	rec := providerRequest(t, srv, u, http.MethodPost, "/api/me/providers/"+p.ID+"/models", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("refresh = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not list models") || !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("error = %s", rec.Body.String())
	}
	got, err := st.GetUserProvider(context.Background(), p.ID, u.ID)
	if err != nil {
		t.Fatalf("GetUserProvider: %v", err)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "meta/llama-3.1-8b" {
		t.Errorf("models after failed refresh = %+v, want the old list", got.Models)
	}
}

// TestProviderPatchNameKeepsSlugAndModels: a name-only edit changes the display
// name, keeps the slug (model ids stay valid) and the cached models, and never
// calls the upstream.
func TestProviderPatchNameKeepsSlugAndModels(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ts, log := newFakeProviderServer(t, "")
	log.modelsStatus = http.StatusOK
	log.modelsBody = `{"data":[{"id":"meta/llama-3.1-8b","name":"Llama 3.1 8B"}]}`
	p := seedProvider(t, st, u, "nvidia-nim", ts.URL+"/v1", "nvapi-key")

	rec := providerRequest(t, srv, u, http.MethodPatch, "/api/me/providers/"+p.ID, `{"name":"NVIDIA renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := decodeProvider(t, rec)
	if got.Name != "NVIDIA renamed" || got.Slug != "nvidia-nim" {
		t.Errorf("name/slug = %q/%q, want the new name and the old slug", got.Name, got.Slug)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "meta/llama-3.1-8b" {
		t.Errorf("models = %+v, want the cached list untouched", got.Models)
	}
	log.mu.Lock()
	n := len(log.reqs)
	log.mu.Unlock()
	if n != 0 {
		t.Errorf("upstream requests = %d, want 0 for a name-only edit", n)
	}

	row, err := st.GetUserProvider(context.Background(), p.ID, u.ID)
	if err != nil || row.Name != "NVIDIA renamed" || row.Slug != "nvidia-nim" {
		t.Errorf("stored row = %+v, %v", row, err)
	}
	// The model selector keeps working: the slug prefix still resolves.
	if _, prov, err := srv.resolveModel(context.Background(), u, "nvidia-nim/meta/llama-3.1-8b"); err != nil || prov == nil {
		t.Errorf("resolveModel after rename = %v, %v; want the provider", prov, err)
	}
}

// TestProviderPatchKeyRefreshesModels: a new key re-validates against the
// upstream, swaps the model list and keeps the slug.
func TestProviderPatchKeyRefreshesModels(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ts, log := newFakeProviderServer(t, "")
	log.modelsStatus = http.StatusOK
	log.modelsBody = `{"data":[{"id":"meta/llama-3.1-8b","name":"Llama 3.1 8B"}]}`
	p := seedProvider(t, st, u, "nvidia-nim", ts.URL+"/v1", "old-key")

	log.modelsBody = `{"data":[{"id":"nvidia/nemotron"},{"id":"meta/llama-3.1-8b"}]}`
	rec := providerRequest(t, srv, u, http.MethodPatch, "/api/me/providers/"+p.ID, `{"api_key":"new-key"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := decodeProvider(t, rec)
	if got.Slug != "nvidia-nim" {
		t.Errorf("slug = %q, want it unchanged", got.Slug)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "nvidia/nemotron" {
		t.Errorf("models = %+v, want the fresh list", got.Models)
	}
	last := log.last(t)
	if last.Path != "/v1/models" || last.Header.Get("Authorization") != "Bearer new-key" {
		t.Errorf("fetch = %s with %q, want /v1/models with the new key", last.Path, last.Header.Get("Authorization"))
	}
	row, err := st.GetUserProvider(context.Background(), p.ID, u.ID)
	if err != nil {
		t.Fatalf("GetUserProvider: %v", err)
	}
	raw, err := secret.Decrypt(testEncKey(), row.KeyEnc)
	if err != nil || string(raw) != "new-key" {
		t.Errorf("stored key = %q, %v; want new-key", raw, err)
	}
}

// TestProviderPatchBadBaseURLKeepsRow: a base URL that does not answer /models
// returns the upstream message with 400 and changes nothing.
func TestProviderPatchBadBaseURLKeepsRow(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	ts, log := newFakeProviderServer(t, "")
	log.modelsStatus = http.StatusOK
	log.modelsBody = `{"data":[{"id":"meta/llama-3.1-8b","name":"Llama 3.1 8B"}]}`
	p := seedProvider(t, st, u, "nvidia-nim", ts.URL+"/v1", "nvapi-key")

	rec := providerRequest(t, srv, u, http.MethodPatch, "/api/me/providers/"+p.ID,
		`{"name":"Should not stick","base_url":"`+ts.URL+`/dead"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not list models") || !strings.Contains(rec.Body.String(), "404") {
		t.Errorf("error = %s, want the upstream message", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "nvapi-key") {
		t.Errorf("error leaked the key: %s", rec.Body.String())
	}
	row, err := st.GetUserProvider(context.Background(), p.ID, u.ID)
	if err != nil {
		t.Fatalf("GetUserProvider: %v", err)
	}
	if row.Name != p.Name || row.BaseURL != p.BaseURL {
		t.Errorf("row name/base_url = %q/%q, want the old values", row.Name, row.BaseURL)
	}
	if len(row.Models) != 1 || row.Models[0].ID != "meta/llama-3.1-8b" {
		t.Errorf("models = %+v, want the old list", row.Models)
	}
	raw, err := secret.Decrypt(testEncKey(), row.KeyEnc)
	if err != nil || string(raw) != "nvapi-key" {
		t.Errorf("stored key = %q, %v; want the old key", raw, err)
	}
}

// TestProviderPatchValidation covers empty bodies, empty strings, and foreign or
// unknown ids.
func TestProviderPatchValidation(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	other := seedKeyUser(t, st, "u2", "bob", "")
	p := seedProvider(t, st, u, "nvidia-nim", "https://integrate.api.nvidia.com/v1", "k")

	cases := []struct{ label, body string }{
		{"empty object", `{}`},
		{"empty name", `{"name":""}`},
		{"long name", `{"name":"` + strings.Repeat("a", maxProviderNameRunes+1) + `"}`},
		{"empty base_url", `{"base_url":""}`},
		{"empty api_key", `{"api_key":""}`},
		{"relative base_url", `{"base_url":"/v1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			rec := providerRequest(t, srv, u, http.MethodPatch, "/api/me/providers/"+p.ID, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
	if rec := providerRequest(t, srv, other, http.MethodPatch, "/api/me/providers/"+p.ID, `{"name":"mine now"}`); rec.Code != http.StatusNotFound {
		t.Errorf("foreign patch = %d, want 404", rec.Code)
	}
	if rec := providerRequest(t, srv, u, http.MethodPatch, "/api/me/providers/nope", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown patch = %d, want 404", rec.Code)
	}
	row, err := st.GetUserProvider(context.Background(), p.ID, u.ID)
	if err != nil || row.Name != p.Name || row.BaseURL != p.BaseURL {
		t.Errorf("row = %+v, %v; want it untouched by rejected patches", row, err)
	}
}

// TestModelsEndpointShape covers built-in and custom entries in GET /api/models.
func TestModelsEndpointShape(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedProvider(t, st, u, "nvidia-nim", "https://integrate.api.nvidia.com/v1", "k")

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handleModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out []models.Model
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(models.Registry)+1 {
		t.Fatalf("models = %d, want %d (registry + custom)", len(out), len(models.Registry)+1)
	}
	for _, m := range out[:len(models.Registry)] {
		if !m.CostTracked || m.Provider != "" {
			t.Errorf("built-in %s = cost_tracked %v, provider %q; want true, empty", m.ID, m.CostTracked, m.Provider)
		}
	}
	custom := out[len(out)-1]
	if custom.ID != "nvidia-nim/meta/llama-3.1-8b" || custom.Dialect != models.DialectChat ||
		custom.Provider != "nvidia-nim" || custom.CostTracked {
		t.Errorf("custom entry = %+v", custom)
	}
	if !strings.Contains(rec.Body.String(), `"cost_tracked":true`) {
		t.Errorf("payload is missing cost_tracked:true: %s", rec.Body.String())
	}
}

// TestResolveModelCustom accepts an owned slug, rejects unknown prefixes, and
// keeps the vendor id (which may itself contain a slash) for the wire.
func TestResolveModelCustom(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	other := seedKeyUser(t, st, "u2", "bob", "")
	seedProvider(t, st, u, "nvidia-nim", "https://integrate.api.nvidia.com/v1", "nvapi-key")
	ctx := context.Background()

	m, prov, err := srv.resolveModel(ctx, u, "nvidia-nim/meta/llama-3.1-8b")
	if err != nil {
		t.Fatalf("resolveModel(owned) = %v", err)
	}
	if m.Dialect != models.DialectChat || m.Vision || m.CostTracked || m.Provider != "nvidia-nim" {
		t.Errorf("model = %+v; want chat, no vision, untracked", m)
	}
	if prov == nil || prov.model != "meta/llama-3.1-8b" || prov.apiKey != "nvapi-key" || prov.baseURL != "https://integrate.api.nvidia.com/v1" {
		t.Errorf("customProvider = %+v", prov)
	}
	if cost := models.CostMicros(m, 1000, 500, 0, time.Now()); cost != 0 {
		t.Errorf("cost = %d, want 0 (unknown, not free)", cost)
	}

	// A vendor id missing from the cached catalog is still allowed.
	if _, _, err := srv.resolveModel(ctx, u, "nvidia-nim/not-listed"); err != nil {
		t.Errorf("resolveModel(stale catalog id) = %v, want accepted", err)
	}
	for _, id := range []string{"other/some-model", "nvidia-nim/", "/some-model", "builtin/unknown"} {
		if _, _, err := srv.resolveModel(ctx, u, id); !errors.Is(err, errUnknownModel) {
			t.Errorf("resolveModel(%q) = %v, want errUnknownModel", id, err)
		}
	}
	if _, _, err := srv.resolveModel(ctx, other, "nvidia-nim/meta/llama-3.1-8b"); !errors.Is(err, errUnknownModel) {
		t.Errorf("resolveModel(foreign) = %v, want errUnknownModel", err)
	}
	if m, prov, err := srv.resolveModel(ctx, u, "glm-5.3-flash"); err != nil || prov != nil || !m.CostTracked {
		t.Errorf("resolveModel(built-in) = %+v, %+v, %v; want tracked registry model", m, prov, err)
	}
}

// TestChatTurnToCustomProvider sends a real chat turn at a fake provider and
// asserts the wire request: vendor model id, Bearer key, no x-opencode-session.
func TestChatTurnToCustomProvider(t *testing.T) {
	srv, st := newKeyTestServer(t)
	srv.up = upstream.NewClient()
	u := seedKeyUser(t, st, "u1", "alice", "")

	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	ts, log := newFakeProviderServer(t, sse)
	seedProvider(t, st, u, "nvidia-nim", ts.URL+"/v1", "nvapi-key")

	convID := "c1"
	if err := st.InsertConversation(context.Background(), &store.Conversation{
		ID: convID, UserID: &u.ID, Model: "nvidia-nim/meta/llama-3.1-8b", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+convID+"/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.SetPathValue("id", convID)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	srv.handlePostMessage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}

	got := log.last(t)
	if got.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", got.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer nvapi-key" {
		t.Errorf("Authorization = %q", h)
	}
	if h := got.Header.Get("x-opencode-session"); h != "" {
		t.Errorf("x-opencode-session = %q, want absent for a custom provider", h)
	}
	if h := got.Header.Get("User-Agent"); h != "myogchat/1.0" {
		t.Errorf("User-Agent = %q", h)
	}
	if !strings.Contains(string(got.Body), `"model":"meta/llama-3.1-8b"`) {
		t.Errorf("request model = %s, want the vendor id alone", got.Body)
	}
	if strings.Contains(string(got.Body), "nvidia-nim/") {
		t.Errorf("request leaked the local slug: %s", got.Body)
	}

	msgs, err := st.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var reply *store.Message
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			reply = &msgs[i]
		}
	}
	if reply == nil || reply.Model == nil || *reply.Model != "nvidia-nim/meta/llama-3.1-8b" {
		t.Fatalf("assistant message = %+v, want the full local model id", reply)
	}
	if reply.CostMicros == nil || *reply.CostMicros != 0 {
		t.Errorf("cost_micros = %v, want 0 (untracked)", reply.CostMicros)
	}
}

// TestCustomProviderNotUsedForHelpers asserts internal helpers keep bare
// OpenCode ids and never need a custom provider.
func TestCustomProviderNotUsedForHelpers(t *testing.T) {
	for _, id := range []string{titleModel, memoryModel, defaultEnhanceModel} {
		if strings.Contains(id, "/") {
			t.Errorf("helper model %q must stay a bare OpenCode id", id)
		}
		if _, ok := models.ByID(id); !ok {
			t.Errorf("helper model %q is not in the registry", id)
		}
	}
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u1", "alice", "")
	seedProvider(t, st, u, "nvidia-nim", "https://integrate.api.nvidia.com/v1", "k")
	ctx := context.Background()

	m, _, err := srv.resolveModel(ctx, u, "nvidia-nim/meta/llama-3.1-8b")
	if err != nil || m.Vision {
		t.Fatalf("custom model = %+v, %v; want non-vision", m, err)
	}
	// With no OpenCode key the title falls back instead of calling a provider.
	srv.chatOnceFn = func(context.Context, string, string, string, []upstream.Message, int) (string, error) {
		t.Error("helper called upstream without an OpenCode key")
		return "", nil
	}
	if got := srv.autoTitle(ctx, "c1", "", "a first user message"); got != "a first user message" {
		t.Errorf("autoTitle fallback = %q", got)
	}
}
