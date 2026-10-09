package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// linkwardenTimeout bounds one Linkwarden API request.
const linkwardenTimeout = 20 * time.Second

// LinkwardenClient talks to a self-hosted Linkwarden instance's v1 API
// (https://docs.linkwarden.app). Authentication is a per-user API token sent as
// "Authorization: Bearer <token>".
type LinkwardenClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// LinkwardenLink is the subset of a Linkwarden link used by the tools.
type LinkwardenLink struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// NewLinkwardenClient validates and normalizes rawURL and returns a client.
func NewLinkwardenClient(rawURL, token string) (*LinkwardenClient, error) {
	base, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("linkwarden token is required")
	}
	return &LinkwardenClient{baseURL: base, token: token, hc: &http.Client{Timeout: linkwardenTimeout}}, nil
}

// NormalizeBaseURL accepts a user-entered instance URL (with or without a
// scheme and trailing "/api/v1") and returns a clean origin to append API paths
// to.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("linkwarden url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid linkwarden url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported linkwarden url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("linkwarden url has no host")
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/api/v1")
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// Search queries the instance's search endpoint and returns matching links.
func (c *LinkwardenClient) Search(ctx context.Context, query string) ([]LinkwardenLink, error) {
	q := url.Values{}
	q.Set("searchQueryString", query)
	endpoint := c.baseURL + "/api/v1/search?" + q.Encode()
	body, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("linkwarden search failed: HTTP %d: %s", status, firstLine(body))
	}
	return parseSearchResponse(body)
}

// Save adds a link to the instance. name is optional; when empty Linkwarden
// derives a title from the page.
func (c *LinkwardenClient) Save(ctx context.Context, rawURL, name string) (LinkwardenLink, error) {
	payload := map[string]any{"url": strings.TrimSpace(rawURL)}
	if strings.TrimSpace(name) != "" {
		payload["name"] = strings.TrimSpace(name)
	}
	enc, err := json.Marshal(payload)
	if err != nil {
		return LinkwardenLink{}, err
	}
	endpoint := c.baseURL + "/api/v1/links"
	body, status, err := c.do(ctx, http.MethodPost, endpoint, enc)
	if err != nil {
		return LinkwardenLink{}, err
	}
	if status < 200 || status > 299 {
		return LinkwardenLink{}, fmt.Errorf("linkwarden save failed: HTTP %d: %s", status, firstLine(body))
	}
	// Success is {"response": <link>}; failures embed a message string instead.
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return LinkwardenLink{}, fmt.Errorf("decode linkwarden response: %w", err)
	}
	if len(env.Response) == 0 {
		return LinkwardenLink{}, errors.New("linkwarden returned an empty response")
	}
	var link LinkwardenLink
	if err := json.Unmarshal(env.Response, &link); err != nil || link.URL == "" {
		var msg string
		if json.Unmarshal(env.Response, &msg) == nil && msg != "" {
			return LinkwardenLink{}, fmt.Errorf("linkwarden: %s", msg)
		}
		return LinkwardenLink{}, errors.New("linkwarden returned an unexpected response")
	}
	return link, nil
}

// do issues one request with the bearer token and returns the body (capped) and
// status code. Transport errors are wrapped; HTTP error statuses are returned
// to the caller for message shaping.
func (c *LinkwardenClient) do(ctx context.Context, method, endpoint string, body []byte) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, linkwardenTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("linkwarden request failed: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, nil
}

// parseSearchResponse accepts both documented search shapes:
// {"data":{"links":[...]}} and {"data":[...]}.
func parseSearchResponse(body []byte) ([]LinkwardenLink, error) {
	var env struct {
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode linkwarden search: %w", err)
	}
	if len(env.Data) == 0 {
		return nil, nil
	}
	var wrapped struct {
		Links []LinkwardenLink `json:"links"`
	}
	if err := json.Unmarshal(env.Data, &wrapped); err == nil && wrapped.Links != nil {
		return wrapped.Links, nil
	}
	var list []LinkwardenLink
	if err := json.Unmarshal(env.Data, &list); err == nil {
		return list, nil
	}
	return nil, errors.New("linkwarden returned an unexpected search response")
}

// ---------- tools ----------

// LinkwardenSearchTool searches the configured Linkwarden instance.
type LinkwardenSearchTool struct{ c *LinkwardenClient }

// NewLinkwardenSearchTool wraps c as a linkwarden_search tool.
func NewLinkwardenSearchTool(c *LinkwardenClient) *LinkwardenSearchTool {
	return &LinkwardenSearchTool{c: c}
}

func (t *LinkwardenSearchTool) Name() string { return "linkwarden_search" }

func (t *LinkwardenSearchTool) Description() string {
	return "Search the user's saved Linkwarden bookmarks. Use this when the user " +
		"asks about links they have already saved or wants to find a previously " +
		"bookmarked page."
}

func (t *LinkwardenSearchTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The text to search saved links for.",
			},
		},
		"required": []string{"query"},
	})
}

func (t *LinkwardenSearchTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
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
	links, err := t.c.Search(ctx, query)
	if err != nil {
		return "", err
	}
	if len(links) == 0 {
		return "No saved links matched.", nil
	}
	var b strings.Builder
	for i, l := range links {
		if i > 0 {
			b.WriteString("\n")
		}
		name := strings.TrimSpace(l.Name)
		if name == "" {
			name = l.URL
		}
		b.WriteString("- ")
		b.WriteString(name)
		if l.URL != "" {
			b.WriteString(" — ")
			b.WriteString(l.URL)
		}
		if desc := strings.TrimSpace(l.Description); desc != "" {
			b.WriteString("\n  ")
			b.WriteString(desc)
		}
	}
	return b.String(), nil
}

// LinkwardenSaveTool saves a URL to the configured Linkwarden instance.
type LinkwardenSaveTool struct{ c *LinkwardenClient }

// NewLinkwardenSaveTool wraps c as a linkwarden_save tool.
func NewLinkwardenSaveTool(c *LinkwardenClient) *LinkwardenSaveTool {
	return &LinkwardenSaveTool{c: c}
}

func (t *LinkwardenSaveTool) Name() string { return "linkwarden_save" }

func (t *LinkwardenSaveTool) Description() string {
	return "Save a URL to the user's Linkwarden bookmarks. Use this when the user " +
		"asks to save, bookmark, or add a link to Linkwarden."
}

func (t *LinkwardenSaveTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The absolute http(s) URL to save.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Optional title for the saved link.",
			},
		},
		"required": []string{"url"},
	})
}

func (t *LinkwardenSaveTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("a valid absolute http(s) url is required")
	}
	link, err := t.c.Save(ctx, rawURL, in.Title)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(link.Name)
	if name == "" {
		name = rawURL
	}
	return fmt.Sprintf("Saved to Linkwarden: %s (%s)", name, link.URL), nil
}
