package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// tavilyBaseURL is the Tavily search API endpoint.
const tavilyBaseURL = "https://api.tavily.com"

// webSearchTimeout bounds one Tavily request.
const webSearchTimeout = 20 * time.Second

// WebSearchTool queries Tavily and returns the top results as
// "title — url\nsnippet" blocks.
type WebSearchTool struct {
	apiKey  string
	baseURL string
	hc      *http.Client
}

// NewWebSearchTool returns a web_search tool using apiKey. Callers should only
// register it when apiKey is non-empty.
func NewWebSearchTool(apiKey string) *WebSearchTool {
	return &WebSearchTool{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: tavilyBaseURL,
		hc:      &http.Client{Timeout: webSearchTimeout},
	}
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
	if t.apiKey == "" {
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

type tavilyResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// search performs one Tavily search and returns up to 5 results.
func (t *WebSearchTool) search(ctx context.Context, query string) ([]tavilyResult, error) {
	payload := map[string]any{
		"query":        query,
		"max_results":  5,
		"search_depth": "basic",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web search failed: %w", err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("web search failed: HTTP %d: %s", resp.StatusCode, firstLine(snippet))
	}
	var out struct {
		Results []tavilyResult `json:"results"`
	}
	if err := json.Unmarshal(snippet, &out); err != nil {
		return nil, fmt.Errorf("decode web search response: %w", err)
	}
	return out.Results, nil
}

// formatSearchResults renders results as compact blocks for the model.
func formatSearchResults(results []tavilyResult) string {
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
		if content := strings.TrimSpace(r.Content); content != "" {
			b.WriteString("\n")
			b.WriteString(content)
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
