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
	SearchProviderSerper = "serper"
	SearchProviderDDG    = "ddg"
)

// Default endpoints. Providers keep a baseURL field so tests can point them at
// an httptest.Server.
const (
	tavilyBaseURL = "https://api.tavily.com"
	braveBaseURL  = "https://api.search.brave.com"
	serperBaseURL = "https://google.serper.dev"
	ddgBaseURL    = "https://html.duckduckgo.com"
)

// serperMaxNum caps the Serper "num" field at 10. Serper bills 10 results as
// 1 credit but 11–20 results as 2 credits; the tool caps results at
// maxSearchResults (20), so staying at 10 keeps every query at 1 credit.
const serperMaxNum = 10

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
const maxSearchChain = 4

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
	Provider     string // tavily | brave | serper | ddg | auto
	TavilyKey    string
	BraveKey     string
	SerperKey    string
	BraveContext bool // Brave llm/context (RAG) mode instead of web/search
	MaxResults   int
	Region       string
	SafeSearch   string
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
	case SearchProviderSerper:
		return SearchProviderSerper
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
		usable[SearchProviderBrave] = newBraveProvider(cfg.BraveKey, cfg.BraveContext)
	}
	if strings.TrimSpace(cfg.SerperKey) != "" {
		usable[SearchProviderSerper] = newSerperProvider(cfg.SerperKey)
	}

	order := []string{SearchProviderTavily, SearchProviderBrave, SearchProviderSerper, SearchProviderDDG}
	switch NormalizeProvider(cfg.Provider) {
	case SearchProviderTavily:
		order = []string{SearchProviderTavily, SearchProviderBrave, SearchProviderSerper, SearchProviderDDG}
	case SearchProviderBrave:
		order = []string{SearchProviderBrave, SearchProviderTavily, SearchProviderSerper, SearchProviderDDG}
	case SearchProviderSerper:
		order = []string{SearchProviderSerper, SearchProviderTavily, SearchProviderBrave, SearchProviderDDG}
	case SearchProviderDDG:
		order = []string{SearchProviderDDG, SearchProviderTavily, SearchProviderBrave, SearchProviderSerper}
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
	apiKey     string
	baseURL    string
	useContext bool // llm/context (RAG) mode instead of web/search
	hc         *http.Client
}

// newBraveProvider builds the Brave provider. useContext selects the
// llm/context retrieval mode (extracted page chunks) instead of the classic
// web/search SERP; both share the same key, host, and identity (2.12).
func newBraveProvider(apiKey string, useContext bool) *braveProvider {
	return &braveProvider{
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    braveBaseURL,
		useContext: useContext,
		hc:         &http.Client{Timeout: webSearchTimeout},
	}
}

func (p *braveProvider) Name() string { return SearchProviderBrave }

func (p *braveProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	if p.useContext {
		return p.searchContext(ctx, query, opts)
	}
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

// braveContextMaxTokens bounds the llm/context token budget. Brave's default is
// 8192 tokens, far too large for a single tool result; 4096 keeps the payload
// useful without bloating the model context.
const braveContextMaxTokens = 4096

// searchContext queries the Brave llm/context (RAG) endpoint. Each
// grounding.generic entry carries extracted page chunks (snippets[]) which are
// joined into the provider-neutral Result.Snippet; the model-facing web_search
// tool is unchanged. An empty grounding set is treated as a provider error so
// the fallback chain continues (matching DuckDuckGo's zero-result contract).
func (p *braveProvider) searchContext(ctx context.Context, query string, opts Options) ([]Result, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(clampResults(opts.MaxResults)))
	q.Set("maximum_number_of_tokens", strconv.Itoa(braveContextMaxTokens))
	if country := countryFromRegion(opts.Region); country != "" {
		q.Set("country", country)
	}
	if lang := langFromRegion(opts.Region); lang != "" {
		q.Set("search_lang", lang)
	}
	if ss := strings.TrimSpace(opts.SafeSearch); ss != "" {
		q.Set("safesearch", ss)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/res/v1/llm/context?"+q.Encode(), nil)
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("brave: HTTP %d: %s", resp.StatusCode, firstLine(raw))
	}
	var out struct {
		Grounding struct {
			Generic []struct {
				URL      string   `json:"url"`
				Title    string   `json:"title"`
				Snippets []string `json:"snippets"`
			} `json:"generic"`
		} `json:"grounding"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("brave: decode response: %w", err)
	}
	results := make([]Result, 0, len(out.Grounding.Generic))
	for _, g := range out.Grounding.Generic {
		// Clean each chunk individually so joining with "\n" survives (cleanText
		// collapses whitespace, which would otherwise flatten the separator).
		chunks := make([]string, 0, len(g.Snippets))
		for _, sn := range g.Snippets {
			if t := cleanText(sn); t != "" {
				chunks = append(chunks, t)
			}
		}
		results = append(results, Result{
			Title:   cleanText(g.Title),
			URL:     g.URL,
			Snippet: strings.Join(chunks, "\n"),
		})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("brave: no results in grounding")
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

// langFromRegion extracts the language code from a region like "us-en" (the
// suffix after the first "-"/"_"); Serper wants hl and Brave context wants
// search_lang. Defaults to "en" when no suffix is present.
func langFromRegion(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		return "en"
	}
	if i := strings.IndexAny(region, "-_"); i >= 0 {
		if i+1 < len(region) {
			return region[i+1:]
		}
		return "en"
	}
	return region
}

// ---------- Serper ----------

type serperProvider struct {
	apiKey  string
	baseURL string
	hc      *http.Client
}

func newSerperProvider(apiKey string) *serperProvider {
	return &serperProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: serperBaseURL,
		hc:      &http.Client{Timeout: webSearchTimeout},
	}
}

func (p *serperProvider) Name() string { return SearchProviderSerper }

func (p *serperProvider) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	num := clampResults(opts.MaxResults)
	if num > serperMaxNum {
		// >10 results doubles the Serper credit cost; cap at 10.
		num = serperMaxNum
	}
	payload := map[string]any{
		"q":   query,
		"num": num,
	}
	if gl := countryFromRegion(opts.Region); gl != "" {
		payload["gl"] = gl
	}
	if hl := langFromRegion(opts.Region); hl != "" {
		payload["hl"] = hl
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serper: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("serper: HTTP %d: %s", resp.StatusCode, firstLine(raw))
	}
	var out struct {
		Organic []struct {
			Title    string `json:"title"`
			Link     string `json:"link"` // Serper uses "link", not "url"
			Snippet  string `json:"snippet"`
			Position int    `json:"position"`
		} `json:"organic"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("serper: decode response: %w", err)
	}
	results := make([]Result, 0, len(out.Organic))
	for _, r := range out.Organic {
		results = append(results, Result{Title: cleanText(r.Title), URL: r.Link, Snippet: cleanText(r.Snippet)})
	}
	return results, nil
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
