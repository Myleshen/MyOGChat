package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"myogchat/internal/models"
	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// Custom upstream providers (§22): a user's own OpenAI-compatible endpoint
// (NVIDIA NIM, OpenRouter, a local vLLM, …). Only chat-completions is supported,
// prices are unknown, and the key is encrypted per user.

const (
	// maxProviderNameRunes bounds the display name.
	maxProviderNameRunes = 64
	// providerFetchTimeout bounds one GET <base>/models.
	providerFetchTimeout = 10 * time.Second
)

// errUnknownModel is the single "no such model" error from resolveModel.
var errUnknownModel = errors.New("unknown model")

// providerView is the API shape of a provider: the encrypted key is never
// included, only whether one is set.
type providerView struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Slug    string                `json:"slug"`
	BaseURL string                `json:"base_url"`
	HasKey  bool                  `json:"has_key"`
	Models  []store.ProviderModel `json:"models"`
}

func providerViewOf(p store.Provider) providerView {
	models := p.Models
	if models == nil {
		models = []store.ProviderModel{}
	}
	return providerView{
		ID: p.ID, Name: p.Name, Slug: p.Slug, BaseURL: p.BaseURL,
		HasKey: p.KeyEnc != "", Models: models,
	}
}

// customProvider is the runtime handle of one resolved custom model: the model
// id to put on the wire plus its host and decrypted key.
type customProvider struct {
	slug    string
	model   string // the provider's own id (the part after "<slug>/")
	baseURL string
	apiKey  string
}

// resolveModel maps a stored/requested model id to either a registry model
// (Bare ids) or one of the caller's custom providers ("<slug>/<vendor-id>").
// A custom id is accepted when its prefix is one of the caller's provider
// slugs; a vendor id missing from the cached catalog is still sent, because the
// list may be stale and the provider's own error is more accurate than ours.
func (s *Server) resolveModel(ctx context.Context, u store.User, id string) (models.Model, *customProvider, error) {
	if !strings.Contains(id, "/") {
		m, ok := models.ByID(id)
		if !ok {
			return models.Model{}, nil, errUnknownModel
		}
		m.CostTracked = true
		return m, nil, nil
	}
	slug, vendorID, _ := strings.Cut(id, "/")
	if slug == "" || vendorID == "" {
		return models.Model{}, nil, errUnknownModel
	}
	providers, err := s.st.ListUserProviders(ctx, u.ID)
	if err != nil {
		return models.Model{}, nil, err
	}
	for _, p := range providers {
		if p.Slug != slug {
			continue
		}
		raw, err := secret.Decrypt(s.encKey, p.KeyEnc)
		if err != nil {
			return models.Model{}, nil, errors.New("the provider key could not be read; delete and re-add the provider")
		}
		// Custom models are chat-dialect only and have no known prices.
		return models.Model{
			ID: id, Name: vendorID, Dialect: models.DialectChat, Provider: p.Slug,
		}, &customProvider{slug: p.Slug, model: vendorID, baseURL: p.BaseURL, apiKey: string(raw)}, nil
	}
	return models.Model{}, nil, errUnknownModel
}

// handleListProviders returns the caller's providers.
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	providers, err := s.st.ListUserProviders(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list providers", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}
	views := make([]providerView, 0, len(providers))
	for _, p := range providers {
		views = append(views, providerViewOf(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": views})
}

// handleCreateProvider validates the endpoint by fetching its model list, then
// stores it. Nothing is persisted unless that fetch succeeds.
func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := decodeJSON(w, r, &req, 16*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if n := len([]rune(name)); n == 0 || n > maxProviderNameRunes {
		writeError(w, http.StatusBadRequest, "name must be 1-64 characters")
		return
	}
	base, err := normalizeProviderBaseURL(req.BaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		writeError(w, http.StatusBadRequest, "api_key is required")
		return
	}
	// The fetch proves the key works and fills the model list.
	list, err := fetchProviderModels(r.Context(), base, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not list models: "+err.Error())
		return
	}
	slug := slugify(name)
	existing, err := s.st.ListUserProviders(r.Context(), u.ID)
	if err != nil {
		s.log.Error("list providers", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create provider")
		return
	}
	enc, err := secret.Encrypt(s.encKey, []byte(key))
	if err != nil {
		s.log.Error("encrypt provider key", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store provider")
		return
	}
	p := store.Provider{
		ID:        newUUID(),
		UserID:    u.ID,
		Name:      name,
		Slug:      uniqueProviderSlug(slug, existing),
		BaseURL:   base,
		KeyEnc:    enc,
		Models:    list,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.st.CreateUserProvider(r.Context(), &p); err != nil {
		s.log.Error("create provider", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create provider")
		return
	}
	writeJSON(w, http.StatusCreated, providerViewOf(p))
}

// handleRefreshProviderModels re-fetches the catalog; on failure the cached list
// is kept and the upstream message is returned with 502.
func (s *Server) handleRefreshProviderModels(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	p, err := s.st.GetUserProvider(r.Context(), r.PathValue("id"), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		s.log.Error("get provider", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load provider")
		return
	}
	raw, err := secret.Decrypt(s.encKey, p.KeyEnc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the provider key could not be read; delete and re-add the provider")
		return
	}
	list, err := fetchProviderModels(r.Context(), p.BaseURL, string(raw))
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not list models: "+err.Error())
		return
	}
	if err := s.st.UpdateUserProviderModels(r.Context(), p.ID, u.ID, list); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "provider not found")
			return
		}
		s.log.Error("update provider models", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": list})
}

// handleDeleteProvider removes one owned provider.
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	err := s.st.DeleteUserProvider(r.Context(), r.PathValue("id"), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		s.log.Error("delete provider", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete provider")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// normalizeProviderBaseURL accepts only an absolute http(s) URL with no query,
// fragment or embedded credentials, and returns it without a trailing slash.
func normalizeProviderBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("base_url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("base_url must be an absolute http or https URL")
	}
	if u.Host == "" || u.Opaque != "" {
		return "", errors.New("base_url must be an absolute http or https URL")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("base_url must not have a query or fragment")
	}
	if u.User != nil {
		return "", errors.New("base_url must not contain credentials")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// uniqueProviderSlug appends -2, -3, … until it does not collide with the
// caller's existing slugs.
func uniqueProviderSlug(base string, existing []store.Provider) string {
	taken := make(map[string]bool, len(existing))
	for _, p := range existing {
		taken[p.Slug] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// providerModelJSON is one entry of a provider's /models response; both the
// OpenAI object shape and a bare array are accepted.
type providerModelJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// fetchProviderModels reads <base>/models with the provider key. It accepts
// {"data":[{"id","name"?}]} and a bare array; a missing name falls back to the id.
func fetchProviderModels(ctx context.Context, baseURL, apiKey string) ([]store.ProviderModel, error) {
	ctx, cancel := context.WithTimeout(ctx, providerFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "myogchat/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(firstNBytes(string(body), 300)))
	}
	var list []providerModelJSON
	if err := json.Unmarshal(body, &list); err != nil {
		var wrapper struct {
			Data []providerModelJSON `json:"data"`
		}
		if werr := json.Unmarshal(body, &wrapper); werr != nil {
			return nil, fmt.Errorf("decode model list: %w", werr)
		}
		list = wrapper.Data
	}
	out := make([]store.ProviderModel, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, m := range list {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, store.ProviderModel{ID: m.ID, Name: name})
	}
	return out, nil
}
