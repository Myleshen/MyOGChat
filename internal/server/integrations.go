package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"myogchat/internal/secret"
	"myogchat/internal/store"
	"myogchat/internal/tools"
	"myogchat/internal/upstream"
)

// Settings keys for the tools/integrations feature (Phase 2.4 / 2.8 / 2.6-2.7).
const (
	settingToolsEnabled    = "tools_enabled"
	settingLinkwardenURL   = "linkwarden_url"
	settingLinkwardenToken = "linkwarden_token"
	// Web-search provider selection + Brave key (Phase 2.4).
	settingSearchProvider = "search_provider"
	settingBraveKey       = "brave_key"
	settingBraveMode      = "brave_mode" // web | context (2.12)
	settingSandboxEnabled = "sandbox_enabled"
)

// SearchDefaults holds the env-derived web-search configuration. DB settings
// (search_provider, brave_key, brave_mode) override these per request.
type SearchDefaults struct {
	Provider     string // tavily | brave | serper | ddg | auto
	TavilyKey    string
	BraveKey     string
	SerperKey    string // optional server-wide Serper key (SERPER_API_KEY)
	BraveContext bool   // Brave llm/context mode (MYOGCHAT_BRAVE_MODE=context)
	MaxResults   int
	Region       string
	SafeSearch   string
}

// searchConfig resolves the effective web-search configuration: env defaults
// overlaid with the DB provider selection and (decrypted) Brave key. It does not
// include any per-user key; use searchConfigFor for a specific caller.
func (s *Server) searchConfig(ctx context.Context) tools.SearchConfig {
	cfg := tools.SearchConfig{
		Provider:     s.search.Provider,
		TavilyKey:    s.search.TavilyKey,
		SerperKey:    s.search.SerperKey,
		BraveContext: s.search.BraveContext,
		MaxResults:   s.search.MaxResults,
		Region:       s.search.Region,
		SafeSearch:   s.search.SafeSearch,
	}
	if v, ok, err := s.st.GetSetting(ctx, settingSearchProvider); err == nil && ok && strings.TrimSpace(v) != "" {
		cfg.Provider = v
	}
	if v, ok, err := s.st.GetSetting(ctx, settingBraveMode); err == nil && ok {
		cfg.BraveContext = strings.EqualFold(strings.TrimSpace(v), "context")
	}
	if key, ok := s.braveKey(ctx); ok {
		cfg.BraveKey = key
	}
	return cfg
}

// searchConfigFor is searchConfig with the caller's own Serper key overlaid:
// own key → optional server-wide SERPER_API_KEY (2.12).
func (s *Server) searchConfigFor(ctx context.Context, u store.User) tools.SearchConfig {
	cfg := s.searchConfig(ctx)
	if key, ok := s.userSerperKey(ctx, u); ok {
		cfg.SerperKey = key
	}
	return cfg
}

// userSerperKey returns the caller's own (decrypted) Serper key, if set. ok is
// false when the user has not stored one.
func (s *Server) userSerperKey(ctx context.Context, u store.User) (string, bool) {
	if u.SerperKeyEnc != nil && *u.SerperKeyEnc != "" {
		if raw, err := secret.Decrypt(s.encKey, *u.SerperKeyEnc); err == nil {
			if k := strings.TrimSpace(string(raw)); k != "" {
				return k, true
			}
		}
	}
	return "", false
}

// braveKey returns the Brave API key from the DB setting (decrypted) or, when
// unset, the env default. ok is false when neither is present.
func (s *Server) braveKey(ctx context.Context) (string, bool) {
	if enc, ok, _ := s.st.GetSetting(ctx, settingBraveKey); ok && enc != "" {
		if raw, err := secret.Decrypt(s.encKey, enc); err == nil {
			if k := strings.TrimSpace(string(raw)); k != "" {
				return k, true
			}
		}
	}
	if k := strings.TrimSpace(s.search.BraveKey); k != "" {
		return k, true
	}
	return "", false
}

// setSearchProvider persists the provider selector (normalized; unknown → auto).
func (s *Server) setSearchProvider(ctx context.Context, provider string) error {
	return s.st.SetSetting(ctx, settingSearchProvider, tools.NormalizeProvider(provider))
}

// setBraveKey persists the encrypted Brave API key. An empty value clears it.
func (s *Server) setBraveKey(ctx context.Context, key string) error {
	plain := strings.TrimSpace(key)
	if plain == "" {
		return s.st.SetSetting(ctx, settingBraveKey, "")
	}
	enc, err := secret.Encrypt(s.encKey, []byte(plain))
	if err != nil {
		return err
	}
	return s.st.SetSetting(ctx, settingBraveKey, enc)
}

// setBraveMode persists the Brave retrieval mode ("web" | "context"). Unknown
// values normalize to "web".
func (s *Server) setBraveMode(ctx context.Context, mode string) error {
	if strings.EqualFold(strings.TrimSpace(mode), "context") {
		return s.st.SetSetting(ctx, settingBraveMode, "context")
	}
	return s.st.SetSetting(ctx, settingBraveMode, "web")
}

// setUserSerperKey persists the caller's encrypted Serper key (2.12). An empty
// value clears it. It mirrors setBraveKey but is stored per user.
func (s *Server) setUserSerperKey(ctx context.Context, u store.User, key string) error {
	plain := strings.TrimSpace(key)
	now := time.Now().UnixMilli()
	if plain == "" {
		return s.st.SetUserSerperKey(ctx, u.ID, nil, now)
	}
	enc, err := secret.Encrypt(s.encKey, []byte(plain))
	if err != nil {
		return err
	}
	return s.st.SetUserSerperKey(ctx, u.ID, &enc, now)
}

// toolsEnabled reports whether tool calling is on (default true).
func (s *Server) toolsEnabled(ctx context.Context) bool {
	v, ok, err := s.st.GetSetting(ctx, settingToolsEnabled)
	if err != nil || !ok {
		return true
	}
	return v == "1"
}

// sandboxEnabled reports whether the code sandbox tools are on (default true).
// This is the DB-backed admin toggle; the MYOGCHAT_SANDBOX env var is a hard
// kill switch applied at startup (it leaves s.sbx nil).
func (s *Server) sandboxEnabled(ctx context.Context) bool {
	v, ok, err := s.st.GetSetting(ctx, settingSandboxEnabled)
	if err != nil || !ok {
		return true
	}
	return v == "1"
}

// setSandboxEnabled persists the global sandbox toggle.
func (s *Server) setSandboxEnabled(ctx context.Context, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return s.st.SetSetting(ctx, settingSandboxEnabled, v)
}

// linkwardenConfig returns the configured instance URL and decrypted token.
// ok is false when either is missing or the token cannot be decrypted.
func (s *Server) linkwardenConfig(ctx context.Context) (rawURL, token string, ok bool) {
	rawURL, _, _ = s.st.GetSetting(ctx, settingLinkwardenURL)
	enc, _, _ := s.st.GetSetting(ctx, settingLinkwardenToken)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || enc == "" {
		return "", "", false
	}
	raw, err := secret.Decrypt(s.encKey, enc)
	if err != nil {
		return "", "", false
	}
	token = strings.TrimSpace(string(raw))
	if token == "" {
		return "", "", false
	}
	return rawURL, token, true
}

// linkwardenClient builds a client when the integration is configured.
func (s *Server) linkwardenClient(ctx context.Context) (*tools.LinkwardenClient, bool) {
	rawURL, token, ok := s.linkwardenConfig(ctx)
	if !ok {
		return nil, false
	}
	c, err := tools.NewLinkwardenClient(rawURL, token)
	if err != nil {
		s.log.Warn("linkwarden client unavailable", "error", err)
		return nil, false
	}
	return c, true
}

// toolRegistry builds the per-request set of tools. It is empty when tools are
// disabled. web_search is present whenever any search provider is usable
// (DuckDuckGo needs no key, so it is always available); fetch_url is always
// available; Linkwarden tools require the integration. u supplies the caller's
// per-user search credentials (Serper key, 2.12).
func (s *Server) toolRegistry(ctx context.Context, u store.User) *tools.Registry {
	reg := tools.NewRegistry()
	if !s.toolsEnabled(ctx) {
		return reg
	}
	cfg := s.searchConfigFor(ctx, u)
	if providers := tools.BuildSearchProviders(cfg); len(providers) > 0 {
		reg.Add(tools.NewWebSearchTool(providers, cfg.Options()))
	}
	reg.Add(tools.NewFetchURLTool())
	if c, ok := s.linkwardenClient(ctx); ok {
		reg.Add(tools.NewLinkwardenSearchTool(c))
		reg.Add(tools.NewLinkwardenSaveTool(c))
	}
	// Code sandbox (Phase 2.6 / 2.7): registered only when the DB toggle is on
	// AND Docker is reachable. Missing images surface a clear error at call time.
	if s.sbx != nil && s.sandboxEnabled(ctx) && s.sbx.Available(ctx) {
		reg.Add(tools.NewRunBashTool(s.sbx))
		reg.Add(tools.NewRunPythonTool(s.sbx))
	}
	return reg
}

// upstreamToolSpecs converts registry specs to the adapter's wire format.
func upstreamToolSpecs(specs []tools.Spec) []upstream.ToolSpec {
	out := make([]upstream.ToolSpec, 0, len(specs))
	for _, sp := range specs {
		out = append(out, upstream.ToolSpec{
			Type: "function",
			Function: upstream.ToolFunctionSpec{
				Name:        sp.Name,
				Description: sp.Description,
				Parameters:  sp.Parameters,
			},
		})
	}
	return out
}

// toolExecutor adapts the registry to upstream.ExecuteTool, producing a short
// UI summary from the tool result.
func (s *Server) toolExecutor(reg *tools.Registry) upstream.ExecuteTool {
	return func(ctx context.Context, call upstream.ToolCall) (string, string, error) {
		t, ok := reg.Get(call.Name)
		if !ok {
			return "", "", fmt.Errorf("unknown tool %q", call.Name)
		}
		result, err := t.Run(ctx, json.RawMessage(call.Arguments))
		if err != nil {
			s.log.Warn("tool failed", "tool", call.Name, "error", err)
			return "", "", err
		}
		return result, toolSummary(result), nil
	}
}

// toolSummary is a compact one-line description of a tool result for the UI.
func toolSummary(result string) string {
	s := strings.TrimSpace(result)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(strings.TrimSpace(s), 80)
}

// integrationSettings returns the admin-facing tools/integrations state.
func (s *Server) integrationSettings(ctx context.Context) map[string]any {
	rawURL, token, configured := s.linkwardenConfig(ctx)
	hint := ""
	if configured {
		hint = keyHint(token)
	}
	cfg := s.searchConfig(ctx)
	_, braveConfigured := s.braveKey(ctx)
	sandboxAvailable := s.sbx != nil && s.sbx.Available(ctx)
	return map[string]any{
		"tools_enabled":         s.toolsEnabled(ctx),
		"tavily_configured":     strings.TrimSpace(cfg.TavilyKey) != "",
		"brave_configured":      braveConfigured,
		"serper_configured":     strings.TrimSpace(cfg.SerperKey) != "",
		"search_provider":       tools.NormalizeProvider(cfg.Provider),
		"search_active":         tools.SearchActive(cfg),
		"brave_mode":            braveMode(cfg.BraveContext),
		"linkwarden_url":        rawURL,
		"linkwarden_configured": configured,
		"linkwarden_token_hint": hint,
		"sandbox_enabled":       s.sandboxEnabled(ctx),
		"sandbox_available":     sandboxAvailable,
	}
}

// braveMode renders the Brave retrieval mode for the API/UI.
func braveMode(useContext bool) string {
	if useContext {
		return "context"
	}
	return "web"
}

// setToolsEnabled persists the global tool toggle.
func (s *Server) setToolsEnabled(ctx context.Context, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return s.st.SetSetting(ctx, settingToolsEnabled, v)
}

// setLinkwarden persists the instance URL and (when token is non-nil) the
// encrypted API token. A nil token leaves the stored token untouched; an empty
// string clears it.
func (s *Server) setLinkwarden(ctx context.Context, rawURL string, token *string) error {
	if err := s.st.SetSetting(ctx, settingLinkwardenURL, strings.TrimSpace(rawURL)); err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	plain := strings.TrimSpace(*token)
	if plain == "" {
		return s.st.SetSetting(ctx, settingLinkwardenToken, "")
	}
	enc, err := secret.Encrypt(s.encKey, []byte(plain))
	if err != nil {
		return err
	}
	return s.st.SetSetting(ctx, settingLinkwardenToken, enc)
}

// handleGetMySerper reports the caller's per-user Serper key status plus a
// provider list (2.12). Visible to all users, not just admins.
func (s *Server) handleGetMySerper(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	cfg := s.searchConfigFor(r.Context(), u)
	hasOwn := u.SerperKeyEnc != nil && *u.SerperKeyEnc != ""
	hint := ""
	if key, ok := s.userSerperKey(r.Context(), u); ok {
		hint = keyHint(key)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_key":            hasOwn,
		"key_hint":           hint,
		"server_key_present": strings.TrimSpace(s.search.SerperKey) != "",
		"serper_configured":  strings.TrimSpace(cfg.SerperKey) != "",
		"tavily_configured":  strings.TrimSpace(cfg.TavilyKey) != "",
		"brave_configured":   strings.TrimSpace(cfg.BraveKey) != "",
		"ddg_configured":     true,
		"search_active":      tools.SearchActive(cfg),
	})
}

// handlePutMySerper sets or clears the caller's per-user Serper key (2.12). An
// empty value clears it; no upstream validation (Serper has no cheap probe).
func (s *Server) handlePutMySerper(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := decodeJSON(w, r, &req, 8*1024); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.setUserSerperKey(r.Context(), u, req.APIKey); err != nil {
		s.log.Error("set serper key", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update key")
		return
	}
	key := strings.TrimSpace(req.APIKey)
	writeJSON(w, http.StatusOK, map[string]any{"has_key": key != "", "key_hint": keyHint(key)})
}
