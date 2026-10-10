package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file implements the pluggable web-search provider layer (Phase 2.4).
// The model-facing tool ("web_search") lives in websearch.go and delegates to an
// ordered chain of SearchProviders so a dead key or a rate-limited backend
// degrades to the next instead of failing the turn.

// Search backend identifiers (used by env/DB config and Name()).
const (
	SearchProviderAuto   = "auto"
	SearchProviderTavily = "tavily"
	SearchProviderBrave  = "brave"
	SearchProviderDDG    = "ddg"
)

// Default endpoints. Providers keep a baseURL field so tests can point them at
// an httptest.Server.
const (
	tavilyBaseURL = "https://api.tavily.com"
	braveBaseURL  = "https://api.search.brave.com"
	ddgBaseURL    = "https://html.duckduckgo.com"
)

// defaultSearchResults is the per-query result cap when none is configured.
const defaultSearchResults = 5

// maxSearchResults is the hard upper bound (Tavily and Brave both cap at 20).
const maxSearchResults = 20

// webSearchTimeout bounds one provider request. Kept at 20s (unchanged from the
// original Tavily-only implementation).
const webSearchTimeout = 20 * time.Second

// searchBudget bounds the whole fallback chain so a multi-provider turn cannot
// stall for (chain length × 20s).
const searchBudget = 25 * time.Second

// maxSearchChain caps how many providers a single query will try.
const maxSearchChain = 3

// ddgUserAgent is a realistic desktop browser UA; DuckDuckGo's HTML endpoint
// serves an anomaly/block page to obviously non-browser clients.
const ddgUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// Result is a provider-neutral search hit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Options carries per-call knobs providers may honor.
type Options struct {
	MaxResults int    // default 5
	Region     string // DDG kl / Brave country source, e.g. "us-en"
	SafeSearch string // off | moderate | strict
}

// SearchProvider is one search backend.
type SearchProvider interface {
	Name() string
	Search(ctx context.Context, query string, opts Options) ([]Result, error)
}

// SearchConfig is the resolved web-search configuration (env + DB settings).
type SearchConfig struct {
	Provider   string // tavily | brave | ddg | auto
	TavilyKey  string
	BraveKey   string
	MaxResults int
	Region     string
	SafeSearch string
}

// Options derives the per-call Options from the resolved config.
func (c SearchConfig) Options() Options {
	return Options{
		MaxResults: clampResults(c.MaxResults),
		Region:     strings.TrimSpace(c.Region),
		SafeSearch: strings.TrimSpace(c.SafeSearch),
	}
}

// NormalizeProvider maps arbitrary input to one of the known selectors, falling
// back to "auto" for anything unrecognized.
func NormalizeProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case SearchProviderTavily:
		return SearchProviderTavily
	case SearchProviderBrave:
		return SearchProviderBrave
	case SearchProviderDDG:
		return SearchProviderDDG
	default:
		return SearchProviderAuto
	}
}

// BuildSearchProviders returns the ordered candidate chain for the configured
// selection, including only usable providers. A keyed provider is never
// constructed with an empty key; DuckDuckGo needs no key, so it is always
// present (the chain is therefore never empty).
//
// Explicit selection still keeps the other providers behind it as fallbacks; an
// explicitly selected provider whose key is missing is simply skipped.
func BuildSearchProviders(cfg SearchConfig) []SearchProvider {
	usable := map[string]SearchProvider{
		SearchProviderDDG: newDDGProvider(),
	}
	if strings.TrimSpace(cfg.TavilyKey) != "" {
		usable[SearchProviderTavily] = newTavilyProvider(cfg.TavilyKey)
	}
	if strings.TrimSpace(cfg.BraveKey) != "" {
		usable[SearchProviderBrave] = newBraveProvider(cfg.BraveKey)
	}

	order := []string{SearchProviderTavily, SearchProviderBrave, SearchProviderDDG}
	switch NormalizeProvider(cfg.Provider) {
	case SearchProviderTavily:
		order = []string{SearchProviderTavily, SearchProviderBrave, SearchProviderDDG}
	case SearchProviderBrave:
		order = []string{SearchProviderBrave, SearchProviderTavily, SearchProviderDDG}
	case SearchProviderDDG:
		order = []string{SearchProviderDDG, SearchProviderTavily, SearchProviderBrave}
	}

	out := make([]SearchProvider, 0, len(order))
	for _, name := range order {
		if p, ok := usable[name]; ok {
			out = append(out, p)
		}
	}
	return out
}

// SearchActive reports which provider would actually be tried first for cfg, or
// "" when none is usable (unreachable while DuckDuckGo exists).
func SearchActive(cfg SearchConfig) string {
	providers := BuildSearchProviders(cfg)
	if len(providers) == 0 {
		return ""
	}
	return providers[0].Name()
}

// clampResults normalizes a configured result count to [1, maxSearchResults].
func clampResults(n int) int {
	if n <= 0 {
		return defaultSearchResults
	}
	if n > maxSearchResults {
		return maxSearchResults
	}
	return n
}

// ---------- Tavily ----------

type tavilyProvider struct {
	apiKey  string
	baseURL string
	hc      *http.Client
}

func newTavilyProvider(apiKey string) *tavilyProvider {
	return &tavilyProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: tavilyBaseURL,
		hc:      &http.Client{Timeout: webSearchTimeout},
	}
}

func (p *tavilyProvider) Name() string { return SearchProviderTavily }

func (p *tavilyProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	payload := map[string]any{
		"query":        query,
		"max_results":  clampResults(opts.MaxResults),
		"search_depth": "basic",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("tavily: HTTP %d: %s", resp.StatusCode, firstLine(raw))
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tavily: decode response: %w", err)
	}
	results := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return results, nil
}

// ---------- Brave ----------

type braveProvider struct {
	apiKey  string
	baseURL string
	hc      *http.Client
}

func newBraveProvider(apiKey string) *braveProvider {
	return &braveProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: braveBaseURL,
		hc:      &http.Client{Timeout: webSearchTimeout},
	}
}

func (p *braveProvider) Name() string { return SearchProviderBrave }

func (p *braveProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(clampResults(opts.MaxResults)))
	if country := countryFromRegion(opts.Region); country != "" {
		q.Set("country", country)
	}
	if ss := strings.TrimSpace(opts.SafeSearch); ss != "" {
		q.Set("safesearch", ss)
	}
	// Accept-Encoding is left unset so Go's transport negotiates and transparently
	// decompresses gzip; setting it manually would require manual decompression.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/res/v1/web/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", p.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("brave: HTTP %d: %s", resp.StatusCode, firstLine(raw))
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("brave: decode response: %w", err)
	}
	results := make([]Result, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		// description carries inline <strong> highlight markup; strip it.
		results = append(results, Result{Title: cleanText(r.Title), URL: r.URL, Snippet: cleanText(r.Description)})
	}
	return results, nil
}

// countryFromRegion extracts a 2-letter country code from a region like "us-en"
// (Brave wants country=us; DDG wants kl=us-en).
func countryFromRegion(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		return ""
	}
	if i := strings.IndexAny(region, "-_"); i > 0 {
		return region[:i]
	}
	return region
}

// ---------- DuckDuckGo ----------

type ddgProvider struct {
	baseURL string
	hc      *http.Client
}

func newDDGProvider() *ddgProvider {
	return &ddgProvider{
		baseURL: ddgBaseURL,
		hc:      &http.Client{Timeout: webSearchTimeout},
	}
}

func (p *ddgProvider) Name() string { return SearchProviderDDG }

func (p *ddgProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	q := url.Values{}
	q.Set("q", query)
	if region := strings.TrimSpace(opts.Region); region != "" {
		q.Set("kl", region)
	}
	if kp := ddgSafeSearch(opts.SafeSearch); kp != "" {
		q.Set("kp", kp)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/html/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ddgUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("duckduckgo: HTTP %d", resp.StatusCode)
	}
	results := parseDDGResults(raw)
	if len(results) == 0 {
		// Markup drift or an anomaly/block page: treat as a provider failure so
		// the chain can continue. Never panic on unexpected markup.
		return nil, fmt.Errorf("duckduckgo: no results parsed")
	}
	if n := clampResults(opts.MaxResults); len(results) > n {
		results = results[:n]
	}
	return results, nil
}

// ddgSafeSearch maps the normalized safe-search value to DuckDuckGo's kp param.
// Best-effort: -2 = off, -1 = moderate, 1 = strict.
func ddgSafeSearch(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off":
		return "-2"
	case "strict":
		return "1"
	case "moderate":
		return "-1"
	default:
		return ""
	}
}

var (
	ddgTitleRe   = regexp.MustCompile(`(?is)<a\b([^>]*class="[^"]*result__a[^"]*"[^>]*)>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?is)<[a-z]+[^>]*class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</[a-z]+>`)
	hrefRe       = regexp.MustCompile(`(?i)href="([^"]*)"`)
	htmlTagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// parseDDGResults extracts organic results from a DuckDuckGo HTML page. It pairs
// each result__a anchor with the first result__snippet that follows it (before
// the next anchor) and decodes the uddg redirect to the real URL. It returns nil
// when nothing matches, and never panics on unexpected markup.
func parseDDGResults(body []byte) []Result {
	doc := string(body)
	titleMatches := ddgTitleRe.FindAllStringSubmatchIndex(doc, -1)
	if len(titleMatches) == 0 {
		return nil
	}
	snippetMatches := ddgSnippetRe.FindAllStringSubmatchIndex(doc, -1)

	results := make([]Result, 0, len(titleMatches))
	for i, m := range titleMatches {
		// m: [full0, full1, attrs0, attrs1, inner0, inner1]
		attrs := doc[m[2]:m[3]]
		title := cleanText(doc[m[4]:m[5]])
		rawURL := ""
		if hm := hrefRe.FindStringSubmatch(attrs); hm != nil {
			rawURL = hm[1]
		}
		target := decodeDDGURL(rawURL)
		if target == "" {
			continue
		}
		snippet := ""
		nextStart := len(doc)
		if i+1 < len(titleMatches) {
			nextStart = titleMatches[i+1][0]
		}
		for _, sm := range snippetMatches {
			if sm[0] > m[1] && sm[0] < nextStart {
				snippet = cleanText(doc[sm[2]:sm[3]])
				break
			}
		}
		results = append(results, Result{Title: title, URL: target, Snippet: snippet})
	}
	return results
}

// decodeDDGURL recovers the real destination from a DuckDuckGo result href. The
// href is a redirect of the form "//duckduckgo.com/l/?uddg=<percent-encoded>";
// other links are returned as-is.
func decodeDDGURL(raw string) string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if strings.Contains(u.Host, "duckduckgo.com") {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
	}
	if u.Scheme == "" || u.Host == "" {
		return ""
	}
	return raw
}

// cleanText strips HTML tags, unescapes entities, and collapses whitespace.
func cleanText(s string) string {
	s = htmlTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}
