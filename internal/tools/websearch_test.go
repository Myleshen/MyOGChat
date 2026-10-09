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

func TestWebSearchTool(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[
			{"title":"Go","url":"https://go.dev","content":"The Go language."},
			{"title":"Rust","url":"https://rust-lang.org","content":"The Rust language."}
		]}`)
	}))
	defer srv.Close()

	tool := NewWebSearchTool("tvly-test")
	tool.baseURL = srv.URL
	tool.hc = srv.Client()

	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"languages"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotAuth != "Bearer tvly-test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody["query"] != "languages" {
		t.Errorf("query body = %v", gotBody["query"])
	}
	if !strings.Contains(out, "Go — https://go.dev") ||
		!strings.Contains(out, "The Go language.") ||
		!strings.Contains(out, "Rust — https://rust-lang.org") {
		t.Errorf("formatted results = %q", out)
	}
}

func TestWebSearchNoResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	tool := NewWebSearchTool("k")
	tool.baseURL = srv.URL
	tool.hc = srv.Client()
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"zzz"}`))
	if err != nil || out != "No results found." {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestWebSearchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"detail":{"error":"bad key"}}`)
	}))
	defer srv.Close()
	tool := NewWebSearchTool("k")
	tool.baseURL = srv.URL
	tool.hc = srv.Client()
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("expected an error on HTTP 401")
	}
}

func TestWebSearchNotConfigured(t *testing.T) {
	tool := NewWebSearchTool("")
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("expected an error when no API key is configured")
	}
}
