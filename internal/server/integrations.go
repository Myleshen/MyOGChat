package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"myogchat/internal/secret"
	"myogchat/internal/tools"
	"myogchat/internal/upstream"
)

// Settings keys for the tools/integrations feature (Phase 2.4 / 2.8).
const (
	settingToolsEnabled    = "tools_enabled"
	settingLinkwardenURL   = "linkwarden_url"
	settingLinkwardenToken = "linkwarden_token"
)

// toolsEnabled reports whether tool calling is on (default true).
func (s *Server) toolsEnabled(ctx context.Context) bool {
	v, ok, err := s.st.GetSetting(ctx, settingToolsEnabled)
	if err != nil || !ok {
		return true
	}
	return v == "1"
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
// disabled. web_search is only present when a Tavily key is configured;
// fetch_url is always available; Linkwarden tools require the integration.
func (s *Server) toolRegistry(ctx context.Context) *tools.Registry {
	reg := tools.NewRegistry()
	if !s.toolsEnabled(ctx) {
		return reg
	}
	if s.tavilyKey != "" {
		reg.Add(tools.NewWebSearchTool(s.tavilyKey))
	}
	reg.Add(tools.NewFetchURLTool())
	if c, ok := s.linkwardenClient(ctx); ok {
		reg.Add(tools.NewLinkwardenSearchTool(c))
		reg.Add(tools.NewLinkwardenSaveTool(c))
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
	return map[string]any{
		"tools_enabled":         s.toolsEnabled(ctx),
		"tavily_configured":     s.tavilyKey != "",
		"linkwarden_url":        rawURL,
		"linkwarden_configured": configured,
		"linkwarden_token_hint": hint,
	}
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
