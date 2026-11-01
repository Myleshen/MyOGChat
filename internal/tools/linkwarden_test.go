package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestLinkwardenSearchOptions(t *testing.T) {
	var got url.Values
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		got = r.URL.Query()
		links := make([]string, 0, 6)
		for i := 1; i <= 6; i++ {
			links = append(links, fmt.Sprintf(`{"id":%d,"name":"L%d","url":"https://e%d.example","tags":[{"id":7,"name":"dev"}],"collection":{"id":9,"name":"Read"}}`, i, i, i))
		}
		io.WriteString(w, `{"data":{"links":[`+strings.Join(links, ",")+`],"nextCursor":null}}`)
	})
	links, err := c.SearchOptions(context.Background(), LinkwardenListOptions{
		Query:        "go",
		Sort:         "name_desc",
		Limit:        5,
		TagID:        7,
		CollectionID: 9,
		PinnedOnly:   true,
	})
	if err != nil {
		t.Fatalf("SearchOptions: %v", err)
	}
	if len(links) != 5 {
		t.Fatalf("len(links) = %d, want 5", len(links))
	}
	for key, want := range map[string]string{
		"sort": "3", "searchQueryString": "go", "tagId": "7",
		"collectionId": "9", "pinnedOnly": "true",
	} {
		if v := got.Get(key); v != want {
			t.Errorf("%s = %q, want %q", key, v, want)
		}
	}
}

func TestLinkwardenTags(t *testing.T) {
	var got url.Values
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tags" {
			t.Errorf("path = %q", r.URL.Path)
		}
		got = r.URL.Query()
		io.WriteString(w, `{"data":{"tags":[
			{"id":1,"name":"dev","_count":{"links":12}},
			{"id":2,"name":"go"}
		]}}`)
	})
	tags, err := c.Tags(context.Background(), LinkwardenTagOptions{Search: "de", Sort: "count_desc", Limit: 10})
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 || tags[0].Name != "dev" || tags[0].Links != 12 {
		t.Fatalf("tags = %+v", tags)
	}
	if got.Get("search") != "de" || got.Get("sort") != "4" {
		t.Errorf("query = %v", got)
	}
	out, err := NewLinkwardenTagsTool(c).Run(context.Background(), json.RawMessage(`{"sort":"count_desc"}`))
	if err != nil || !strings.Contains(out, "dev (12 links)") || !strings.Contains(out, "- go") {
		t.Fatalf("tags out = %q, err = %v", out, err)
	}
}

func TestLinkwardenCollections(t *testing.T) {
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/collections" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"response":[
			{"id":2,"name":"Projects","parentId":1,"_count":{"links":4}},
			{"id":3,"name":"Archive","isPublic":true},
			{"id":1,"name":"Work"}
		]}`)
	})
	cols, err := c.Collections(context.Background(), LinkwardenCollectionOptions{})
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if len(cols) != 3 || cols[0].Name != "Archive" || cols[1].Name != "Projects" || cols[1].Links != 4 {
		t.Fatalf("collections = %+v", cols)
	}
	out, err := NewLinkwardenCollectionsTool(c).Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("collections tool: %v", err)
	}
	if !strings.Contains(out, "- Work / Projects (4 links)") || !strings.Contains(out, "- Archive (public)") {
		t.Fatalf("collections out = %q", out)
	}
}

func TestLinkwardenSearchResolvesFiltersAndFormats(t *testing.T) {
	var searchQuery url.Values
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tags":
			io.WriteString(w, `{"data":{"tags":[{"id":5,"name":"golang","_count":{"links":2}}]}}`)
		case "/api/v1/collections":
			io.WriteString(w, `{"response":[{"id":8,"name":"Reading"}]}`)
		case "/api/v1/search":
			searchQuery = r.URL.Query()
			io.WriteString(w, `{"data":{"links":[{
				"id":1,"name":"Go","url":"https://go.dev","description":"the language",
				"createdAt":"2026-10-31T22:38:11Z","pinnedBy":[1],
				"tags":[{"id":5,"name":"golang"}],
				"collection":{"id":8,"name":"Reading"}
			}]}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})
	out, err := NewLinkwardenSearchTool(c).Run(context.Background(),
		json.RawMessage(`{"query":"go","tag":"golang","collection":"Reading","sort":"newest","limit":5}`))
	if err != nil {
		t.Fatalf("search tool: %v", err)
	}
	if searchQuery.Get("tagId") != "5" || searchQuery.Get("collectionId") != "8" || searchQuery.Get("sort") != "0" {
		t.Errorf("search query = %v", searchQuery)
	}
	for _, want := range []string{"folder: Reading", "tags: golang", "saved: 2026-10-31", "pinned", "the language"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestLinkwardenTagFilterPaginates(t *testing.T) {
	var cursors []string
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		if cursor == "" {
			io.WriteString(w, `{"data":{"links":[
				{"id":3,"name":"Untagged"},
				{"id":2,"name":"Tagged","tags":[{"id":5,"name":"dev"}]},
				{"id":1,"name":"Untagged too"}
			],"nextCursor":100}}`)
			return
		}
		io.WriteString(w, `{"data":{"links":[
			{"id":9,"name":"Older tagged","tags":[{"id":5,"name":"dev"}]}
		],"nextCursor":null}}`)
	})
	links, err := c.SearchOptions(context.Background(), LinkwardenListOptions{TagID: 5, Limit: 2})
	if err != nil {
		t.Fatalf("SearchOptions: %v", err)
	}
	if len(links) != 2 || links[0].ID != 2 || links[1].ID != 9 {
		t.Fatalf("links = %+v", links)
	}
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "100" {
		t.Fatalf("cursors = %v", cursors)
	}
}

func TestLinkwardenUnknownSort(t *testing.T) {
	c, _ := newTestLinkwarden(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected upstream request")
	})
	if _, err := c.SearchOptions(context.Background(), LinkwardenListOptions{Sort: "sideways"}); err == nil {
		t.Fatal("SearchOptions should reject an unknown sort")
	}
	if _, err := c.Tags(context.Background(), LinkwardenTagOptions{Sort: "sideways"}); err == nil {
		t.Fatal("Tags should reject an unknown sort")
	}
}
