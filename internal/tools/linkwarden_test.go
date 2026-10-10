package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://links.example.com":           "https://links.example.com",
		"https://links.example.com/":          "https://links.example.com",
		"https://links.example.com/api/v1":    "https://links.example.com",
		"https://links.example.com/api/v1/":   "https://links.example.com",
		"links.example.com":                   "https://links.example.com",
		"http://192.168.1.10:3000":            "http://192.168.1.10:3000",
		"https://links.example.com/sub/path/": "https://links.example.com/sub/path",
	}
	for in, want := range cases {
		got, err := NormalizeBaseURL(in)
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "ftp://example.com"} {
		if _, err := NormalizeBaseURL(bad); err == nil {
			t.Errorf("NormalizeBaseURL(%q) should error", bad)
		}
	}
}

func newTestLinkwarden(t *testing.T, handler http.HandlerFunc) (*LinkwardenClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewLinkwardenClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewLinkwardenClient: %v", err)
	}
	c.hc = srv.Client()
	return c, srv
}

func TestLinkwardenSearchWrapped(t *testing.T) {
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("searchQueryString"); got != "golang" {
			t.Errorf("query = %q", got)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"data":{"links":[
			{"id":1,"name":"Go","url":"https://go.dev","description":"lang"},
			{"id":2,"name":"Docs","url":"https://go.dev/doc"}
		]},"success":true}`)
	})
	links, err := c.Search(context.Background(), "golang")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(links) != 2 || links[0].Name != "Go" || links[1].URL != "https://go.dev/doc" {
		t.Fatalf("links = %+v", links)
	}
}

func TestLinkwardenSearchList(t *testing.T) {
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":7,"name":"Solo","url":"https://solo.example"}]}`)
	})
	links, err := c.Search(context.Background(), "solo")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(links) != 1 || links[0].Name != "Solo" {
		t.Fatalf("links = %+v", links)
	}
}

func TestLinkwardenSave(t *testing.T) {
	var body map[string]any
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/links" {
			t.Errorf("method/path = %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"response":{"id":9,"name":"Example","url":"https://example.com"}}`)
	})
	link, err := c.Save(context.Background(), "https://example.com", "Example")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if link.ID != 9 || link.URL != "https://example.com" {
		t.Fatalf("link = %+v", link)
	}
	if body["url"] != "https://example.com" || body["name"] != "Example" {
		t.Errorf("request body = %v", body)
	}
}

func TestLinkwardenSaveErrorResponse(t *testing.T) {
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"response":"Link already exists"}`)
	})
	if _, err := c.Save(context.Background(), "https://example.com", ""); err == nil ||
		!strings.Contains(err.Error(), "Link already exists") {
		t.Fatalf("err = %v, want the Linkwarden message", err)
	}
}

func TestLinkwardenCheck(t *testing.T) {
	// A 2xx authenticated search is accepted.
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"data":{"links":[]}}`)
	})
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check(ok): %v", err)
	}

	// A 401 is surfaced as an error.
	bad, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"unauthorized"}`)
	})
	if err := bad.Check(context.Background()); err == nil {
		t.Fatal("Check(401) should error")
	}
}

func TestLinkwardenToolsFormat(t *testing.T) {
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/search") {
			io.WriteString(w, `{"data":{"links":[{"id":1,"name":"Go","url":"https://go.dev","description":"lang"}]}}`)
			return
		}
		io.WriteString(w, `{"response":{"id":2,"name":"Saved","url":"https://saved.example"}}`)
	})
	search := NewLinkwardenSearchTool(c)
	out, err := search.Run(context.Background(), json.RawMessage(`{"query":"go"}`))
	if err != nil || !strings.Contains(out, "Go — https://go.dev") || !strings.Contains(out, "lang") {
		t.Fatalf("search out = %q, err = %v", out, err)
	}
	save := NewLinkwardenSaveTool(c)
	out, err = save.Run(context.Background(), json.RawMessage(`{"url":"https://saved.example","title":"Saved"}`))
	if err != nil || !strings.Contains(out, "Saved to Linkwarden: Saved") {
		t.Fatalf("save out = %q, err = %v", out, err)
	}
	if _, err := save.Run(context.Background(), json.RawMessage(`{"url":"notaurl"}`)); err == nil {
		t.Fatal("save should reject a relative url")
	}
}
