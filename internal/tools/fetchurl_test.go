package tools

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTMLToText(t *testing.T) {
	page := `<!DOCTYPE html>
<html><head><title>  Hello &amp; World </title>
<style>body { color: red; }</style>
<script>var x = "<p>not text</p>";</script>
</head>
<body>
  <!-- a comment -->
  <h1>Heading</h1>
  <p>First   paragraph &amp; entity.</p>
  <p>Second<br>line</p>
  <ul><li>one</li><li>two</li></ul>
</body></html>`
	got := HTMLToText(page)
	for _, want := range []string{"Heading", "First paragraph & entity.", "Second", "line", "one", "two"} {
		if !strings.Contains(got, want) {
			t.Errorf("HTMLToText missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"color: red", "var x", "a comment", "<h1>"} {
		if strings.Contains(got, bad) {
			t.Errorf("HTMLToText should not contain %q in:\n%s", bad, got)
		}
	}
	if title := ExtractTitle(page); title != "Hello & World" {
		t.Errorf("ExtractTitle = %q", title)
	}
}

func TestBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.0.0.1", "192.168.1.5", "172.16.0.1",
		"169.254.1.1", "0.0.0.0", "100.64.1.1", "fe80::1", "fc00::1"}
	for _, s := range blocked {
		if ip := net.ParseIP(s); ip == nil || !blockedIP(ip) {
			t.Errorf("blockedIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if ip := net.ParseIP(s); ip == nil || blockedIP(ip) {
			t.Errorf("blockedIP(%s) = true, want false", s)
		}
	}
}

func TestFetchReadableHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><head><title>Page Title</title></head><body><p>Hello world.</p></body></html>`)
	}))
	defer srv.Close()

	title, text, err := FetchReadable(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchReadable: %v", err)
	}
	if title != "Page Title" {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(text, "Hello world.") {
		t.Errorf("text = %q", text)
	}
}

func TestFetchReadablePlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "line one\n\n\nline two")
	}))
	defer srv.Close()

	_, text, err := FetchReadable(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchReadable: %v", err)
	}
	if text != "line one\n\nline two" {
		t.Errorf("text = %q", text)
	}
}

func TestFetchReadableRejectsBadScheme(t *testing.T) {
	if _, _, err := FetchReadable(context.Background(), &http.Client{}, "file:///etc/passwd"); err == nil {
		t.Fatal("file:// should be rejected")
	}
}

// The SSRF-safe client must refuse to dial loopback (httptest serves 127.0.0.1).
func TestSafeClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "secret")
	}))
	defer srv.Close()

	hc := newSafeHTTPClient(2 * time.Second)
	if _, _, err := FetchReadable(context.Background(), hc, srv.URL); err == nil {
		t.Fatal("safe client should refuse loopback, but the fetch succeeded")
	}
}
