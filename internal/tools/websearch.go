package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// WebSearchTool queries an ordered chain of search providers and returns the top
// results as "title — url\nsnippet" blocks. The model-facing name and contract
// ("web_search") are unchanged regardless of which provider answers.
type WebSearchTool struct {
	providers []SearchProvider
	opts      Options
}

// NewWebSearchTool returns a web_search tool that tries providers in order,
// falling through on error or empty results. Callers should only register it
// when at least one provider is usable.
func NewWebSearchTool(providers []SearchProvider, opts Options) *WebSearchTool {
	return &WebSearchTool{providers: providers, opts: opts}
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "Search the web for current information. Returns the top results with " +
		"titles, URLs, and short snippets. Use this when the answer depends on " +
		"recent or external facts you do not already know."
}

func (t *WebSearchTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query.",
			},
		},
		"required": []string{"query"},
	})
}

func (t *WebSearchTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	if len(t.providers) == 0 {
		return "", errors.New("web search is not configured")
	}
	var in struct {
		Query string `json:"query"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", errors.New("query is required")
	}
	results, err := t.search(ctx, query)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No results found.", nil
	}
	return formatSearchResults(results), nil
}

// search walks the provider chain, returning the first non-empty result set. On
// a provider error or empty result it logs a warning and continues to the next.
// The whole chain shares one overall time budget (searchBudget).
func (t *WebSearchTool) search(ctx context.Context, query string) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, searchBudget)
	defer cancel()

	var lastErr error
	gotResponse := false
	tried := 0
	for _, p := range t.providers {
		if tried >= maxSearchChain {
			break
		}
		tried++
		results, err := p.Search(ctx, query, t.opts)
		if err != nil {
			slog.Default().Warn("web search provider failed", "provider", p.Name(), "error", err)
			lastErr = err
			continue
		}
		gotResponse = true
		if len(results) == 0 {
			slog.Default().Warn("web search provider returned no results", "provider", p.Name())
			continue
		}
		return results, nil
	}
	if gotResponse {
		// At least one provider answered; it just had nothing to show.
		return nil, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("web search failed: %w", lastErr)
	}
	return nil, errors.New("web search is not configured")
}

// formatSearchResults renders results as compact blocks for the model.
func formatSearchResults(results []Result) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = r.URL
		}
		b.WriteString(title)
		if r.URL != "" {
			b.WriteString(" — ")
			b.WriteString(r.URL)
		}
		if snippet := strings.TrimSpace(r.Snippet); snippet != "" {
			b.WriteString("\n")
			b.WriteString(snippet)
		}
	}
	return b.String()
}

// firstLine returns the first line of b, trimmed and capped.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(s, 200)
}
