package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTavilyProvider(t *testing.T) {
	var gotMethod, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"results":[
			{"title":"Go","url":"https://go.dev","content":"The Go language."},
			{"title":"Rust","url":"https://rust-lang.org","content":"The Rust language."}
		]}`)
	}))
	defer srv.Close()

	p := newTavilyProvider("tvly-abc")
	p.baseURL = srv.URL
	p.hc = srv.Client()
	res, err := p.Search(context.Background(), "languages", Options{MaxResults: 7})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer tvly-abc" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody["query"] != "languages" {
		t.Errorf("query = %v", gotBody["query"])
	}
	if gotBody["max_results"].(float64) != 7 {
		t.Errorf("max_results = %v", gotBody["max_results"])
	}
	if gotBody["search_depth"] != "basic" {
		t.Errorf("search_depth = %v", gotBody["search_depth"])
	}
	if len(res) != 2 || res[0].Title != "Go" || res[0].URL != "https://go.dev" || res[0].Snippet != "The Go language." {
		t.Errorf("results = %+v", res)
	}
}

func TestTavilyProviderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"detail":"rate limited"}`)
	}))
	defer srv.Close()
	p := newTavilyProvider("k")
	p.baseURL = srv.URL
	p.hc = srv.Client()
	if _, err := p.Search(context.Background(), "x", Options{}); err == nil {
		t.Fatal("expected an error on HTTP 429")
	}
}

func TestBraveProvider(t *testing.T) {
	var gotToken, gotAccept string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/res/v1/web/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotToken = r.Header.Get("X-Subscription-Token")
		gotAccept = r.Header.Get("Accept")
		gotQuery = r.URL.Query()
		io.WriteString(w, `{"web":{"results":[
			{"title":"Best <strong>Greek</strong> Restaurants","url":"https://example.com/greek",
			 "description":"Find the best <strong>Greek</strong> food &amp; more."}
		]}}`)
	}))
	defer srv.Close()

	p := newBraveProvider("brave-key")
	p.baseURL = srv.URL
	p.hc = srv.Client()
	res, err := p.Search(context.Background(), "greek food", Options{MaxResults: 3, Region: "us-en", SafeSearch: "strict"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotToken != "brave-key" {
		t.Errorf("token = %q", gotToken)
	}
	if gotAccept != "application/json" {
		t.Errorf("accept = %q", gotAccept)
	}
	if got := gotQuery.Get("q"); got != "greek food" {
		t.Errorf("q = %q", got)
	}
	if got := gotQuery.Get("count"); got != "3" {
		t.Errorf("count = %q", got)
	}
	if got := gotQuery.Get("country"); got != "us" {
		t.Errorf("country = %q, want us", got)
	}
	if got := gotQuery.Get("safesearch"); got != "strict" {
		t.Errorf("safesearch = %q", got)
	}
	if len(res) != 1 {
		t.Fatalf("results = %+v", res)
	}
	// <strong> tags and HTML entities must be stripped from title + description.
	if strings.ContainsAny(res[0].Title+res[0].Snippet, "<>") {
		t.Errorf("tags not stripped: %+v", res[0])
	}
	if res[0].Title != "Best Greek Restaurants" {
		t.Errorf("title = %q", res[0].Title)
	}
	if res[0].Snippet != "Find the best Greek food & more." {
		t.Errorf("snippet = %q", res[0].Snippet)
	}
}

func TestBraveProviderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()
	p := newBraveProvider("k")
	p.baseURL = srv.URL
	p.hc = srv.Client()
	if _, err := p.Search(context.Background(), "x", Options{}); err == nil {
		t.Fatal("expected an error on HTTP 401")
	}
}

func TestDDGProvider(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "ddg_results.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var gotQuery url.Values
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/html/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html")
		w.Write(fixture)
	}))
	defer srv.Close()

	p := newDDGProvider()
	p.baseURL = srv.URL
	p.hc = srv.Client()
	res, err := p.Search(context.Background(), "go programming language", Options{MaxResults: 5, Region: "us-en", SafeSearch: "moderate"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery.Get("q") != "go programming language" {
		t.Errorf("q = %q", gotQuery.Get("q"))
	}
	if gotQuery.Get("kl") != "us-en" {
		t.Errorf("kl = %q", gotQuery.Get("kl"))
	}
	if gotUA == "" || !strings.Contains(gotUA, "Mozilla") {
		t.Errorf("user-agent = %q", gotUA)
	}
	if len(res) != 3 {
		t.Fatalf("results = %+v", res)
	}
	// uddg redirects must be decoded to the real destination.
	if res[0].URL != "https://go.dev/" {
		t.Errorf("url[0] = %q, want https://go.dev/", res[0].URL)
	}
	if res[0].Title != "The Go Programming Language" {
		t.Errorf("title[0] = %q", res[0].Title)
	}
	if !strings.Contains(res[0].Snippet, "open source programming language") {
		t.Errorf("snippet[0] = %q", res[0].Snippet)
	}
	if res[1].URL != "https://en.wikipedia.org/wiki/Go_(programming_language)" {
		t.Errorf("url[1] = %q", res[1].URL)
	}
	if res[2].URL != "https://pkg.go.dev/" {
		t.Errorf("url[2] = %q", res[2].URL)
	}
	if res[2].Snippet != "Discover & share Go packages." {
		t.Errorf("snippet[2] = %q", res[2].Snippet)
	}
}

func TestDDGProviderTruncatesToMaxResults(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "ddg_results.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture)
	}))
	defer srv.Close()
	p := newDDGProvider()
	p.baseURL = srv.URL
	p.hc = srv.Client()
	res, err := p.Search(context.Background(), "go", Options{MaxResults: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Errorf("len = %d, want 2", len(res))
	}
}

func TestDDGProviderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := newDDGProvider()
	p.baseURL = srv.URL
	p.hc = srv.Client()
	if _, err := p.Search(context.Background(), "x", Options{}); err == nil {
		t.Fatal("expected an error on HTTP 429")
	}
}

func TestDDGProviderNoResultsIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><body><div class="no-results">nothing</div></body></html>`)
	}))
	defer srv.Close()
	p := newDDGProvider()
	p.baseURL = srv.URL
	p.hc = srv.Client()
	if _, err := p.Search(context.Background(), "x", Options{}); err == nil {
		t.Fatal("expected an error when no results parse (markup drift)")
	}
}

func TestDecodeDDGURL(t *testing.T) {
	cases := map[string]string{
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&rut=x":               "https://go.dev/",
		"https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa%3Fb%3D1": "https://example.com/a?b=1",
		"https://example.com/direct":                                           "https://example.com/direct",
		"":                                                                     "",
	}
	for in, want := range cases {
		if got := decodeDDGURL(in); got != want {
			t.Errorf("decodeDDGURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildSearchProvidersAuto(t *testing.T) {
	// No keys: DuckDuckGo alone, so the chain is never empty.
	got := BuildSearchProviders(SearchConfig{Provider: "auto"})
	if len(got) != 1 || got[0].Name() != SearchProviderDDG {
		t.Fatalf("auto/no-keys = %v", names(got))
	}
	// Tavily + Brave keys: Tavily first.
	got = BuildSearchProviders(SearchConfig{Provider: "auto", TavilyKey: "t", BraveKey: "b"})
	if want := []string{"tavily", "brave", "ddg"}; !equal(names(got), want) {
		t.Fatalf("auto order = %v, want %v", names(got), want)
	}
	// Only Brave: Brave then DDG.
	got = BuildSearchProviders(SearchConfig{Provider: "auto", BraveKey: "b"})
	if want := []string{"brave", "ddg"}; !equal(names(got), want) {
		t.Fatalf("brave order = %v, want %v", names(got), want)
	}
}

func TestBuildSearchProvidersExplicit(t *testing.T) {
	// Explicit brave puts Brave first but keeps fallbacks.
	got := BuildSearchProviders(SearchConfig{Provider: "brave", TavilyKey: "t", BraveKey: "b"})
	if want := []string{"brave", "tavily", "ddg"}; !equal(names(got), want) {
		t.Fatalf("explicit brave = %v, want %v", names(got), want)
	}
	// Explicit tavily with no Tavily key degrades to the next usable.
	got = BuildSearchProviders(SearchConfig{Provider: "tavily", BraveKey: "b"})
	if want := []string{"brave", "ddg"}; !equal(names(got), want) {
		t.Fatalf("tavily-no-key = %v, want %v", names(got), want)
	}
	// Explicit ddg leads.
	got = BuildSearchProviders(SearchConfig{Provider: "ddg", TavilyKey: "t"})
	if want := []string{"ddg", "tavily"}; !equal(names(got), want) {
		t.Fatalf("explicit ddg = %v, want %v", names(got), want)
	}
	// Garbage normalizes to auto.
	got = BuildSearchProviders(SearchConfig{Provider: "bogus", TavilyKey: "t"})
	if got[0].Name() != SearchProviderTavily {
		t.Fatalf("garbage provider = %v", names(got))
	}
}

func TestSearchActive(t *testing.T) {
	if got := SearchActive(SearchConfig{}); got != SearchProviderDDG {
		t.Errorf("active = %q, want ddg", got)
	}
	if got := SearchActive(SearchConfig{TavilyKey: "t"}); got != SearchProviderTavily {
		t.Errorf("active = %q, want tavily", got)
	}
	if got := SearchActive(SearchConfig{BraveKey: "b"}); got != SearchProviderBrave {
		t.Errorf("active = %q, want brave", got)
	}
}

// stubProvider is a programmable SearchProvider for fallback-chain tests.
type stubProvider struct {
	name    string
	results []Result
	err     error
	calls   int
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Search(context.Context, string, Options) ([]Result, error) {
	s.calls++
	return s.results, s.err
}

func TestFallbackChainOnError(t *testing.T) {
	primary := &stubProvider{name: "primary", err: errors.New("boom")}
	secondary := &stubProvider{name: "secondary", results: []Result{{Title: "ok", URL: "https://ok"}}}
	tool := NewWebSearchTool([]SearchProvider{primary, secondary}, Options{})
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("calls = %d/%d, want 1/1", primary.calls, secondary.calls)
	}
	if !strings.Contains(out, "https://ok") {
		t.Errorf("out = %q", out)
	}
}

func TestFallbackChainOnEmpty(t *testing.T) {
	primary := &stubProvider{name: "primary"} // succeeds with zero results
	secondary := &stubProvider{name: "secondary", results: []Result{{Title: "ok", URL: "https://ok"}}}
	tool := NewWebSearchTool([]SearchProvider{primary, secondary}, Options{})
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("calls = %d/%d, want 1/1", primary.calls, secondary.calls)
	}
	if !strings.Contains(out, "https://ok") {
		t.Errorf("out = %q", out)
	}
}

func TestFallbackChainAllFail(t *testing.T) {
	a := &stubProvider{name: "a", err: errors.New("a down")}
	b := &stubProvider{name: "b", err: errors.New("b down")}
	tool := NewWebSearchTool([]SearchProvider{a, b}, Options{})
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":"q"}`)); err == nil {
		t.Fatal("expected an error when every provider fails")
	}
}

func TestFallbackChainAllEmpty(t *testing.T) {
	a := &stubProvider{name: "a"}
	b := &stubProvider{name: "b"}
	tool := NewWebSearchTool([]SearchProvider{a, b}, Options{})
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "No results found." {
		t.Errorf("out = %q", out)
	}
}

func names(ps []SearchProvider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
