package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// fetchMaxBytes caps how much of a response body is read (2 MB). Readable text
// is truncated further before it is handed to the model.
const fetchMaxBytes = 2 << 20

// fetchTextLimit is the maximum number of characters of readable text returned
// to the model, keeping the tool result within a sane token budget.
const fetchTextLimit = 12000

// fetchTimeout bounds one URL fetch.
const fetchTimeout = 20 * time.Second

// FetchURLTool fetches a URL and returns its readable text. It is the primitive
// behind URL summarization (Phase 2.8): the model calls it and then summarizes
// the returned text.
type FetchURLTool struct {
	hc *http.Client
}

// NewFetchURLTool returns a fetch_url tool whose transport refuses to connect
// to loopback, private, link-local, or otherwise non-public addresses (SSRF
// guard) and which does not follow redirects to such addresses.
func NewFetchURLTool() *FetchURLTool {
	return &FetchURLTool{hc: newSafeHTTPClient(fetchTimeout)}
}

func (t *FetchURLTool) Name() string { return "fetch_url" }

func (t *FetchURLTool) Description() string {
	return "Fetch a web page and return its readable text (HTML stripped). " +
		"Use this to read or summarize the contents of a specific URL. " +
		"Only public http/https URLs are allowed."
}

func (t *FetchURLTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The absolute http(s) URL to fetch.",
			},
		},
		"required": []string{"url"},
	})
}

func (t *FetchURLTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	raw := strings.TrimSpace(in.URL)
	if raw == "" {
		return "", errors.New("url is required")
	}
	title, text, err := FetchReadable(ctx, t.hc, raw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "The page contained no readable text.", nil
	}
	if title != "" {
		return title + "\n\n" + text, nil
	}
	return text, nil
}

// FetchReadable GETs rawURL and returns its <title> (if any) and readable text.
// The client must be an SSRF-safe client (see newSafeHTTPClient).
func FetchReadable(ctx context.Context, hc *http.Client, rawURL string) (title, text string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported url scheme %q (only http/https)", u.Scheme)
	}
	if u.Host == "" {
		return "", "", errors.New("url has no host")
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; MyOGChat/1.0; +https://github.com/Myleshen/MyOGChat)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")

	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", fmt.Errorf("fetch failed: HTTP %d", resp.StatusCode)
	}

	ctype := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil {
		return "", "", fmt.Errorf("read response: %w", err)
	}
	page := string(body)
	if isHTML(ctype, page) {
		title = ExtractTitle(page)
		text = HTMLToText(page)
	} else {
		text = collapseWhitespace(page)
	}
	text = truncateRunes(text, fetchTextLimit)
	return title, text, nil
}

// isHTML reports whether the response should be treated as HTML. It trusts the
// Content-Type first, then sniffs the body.
func isHTML(contentType, body string) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "html") {
		return true
	}
	if ct != "" && !strings.Contains(ct, "xml") && !strings.Contains(ct, "text/plain") {
		return false
	}
	head := strings.ToLower(body)
	if i := strings.IndexByte(head, '>'); i >= 0 {
		head = head[:i]
	}
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
}

var (
	// commentRe strips HTML comments (including conditional comments).
	commentRe = regexp.MustCompile(`(?is)<!--.*?-->`)
	// titleRe extracts the <title> element.
	titleRe = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
	// blockRe replaces block-level boundaries with newlines so text does not run
	// together.
	blockRe = regexp.MustCompile(`(?i)<(/?(p|div|br|li|ul|ol|tr|td|th|h[1-6]|section|article|header|footer|blockquote|pre|table|form|nav|aside|main|figure|figcaption|dl|dt|dd)\b[^>]*)>`)
	// tagRe removes any remaining tags.
	tagRe = regexp.MustCompile(`(?s)<[^>]*>`)
	// wsRe collapses runs of horizontal whitespace.
	wsRe = regexp.MustCompile(`[ \t\f\v\xa0]+`)
	// nlRe collapses three or more newlines to two.
	nlRe = regexp.MustCompile(`\n{3,}`)
)

// dropTags are elements whose contents never contribute readable text. RE2 has
// no backreferences, so each tag gets its own pattern.
var dropTags = []string{"script", "style", "noscript", "template", "svg", "head", "iframe", "object", "embed"}

var dropRes = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, 0, len(dropTags))
	for _, tag := range dropTags {
		res = append(res, regexp.MustCompile(`(?is)<`+tag+`\b[^>]*>.*?</`+tag+`\s*>`))
	}
	return res
}()

// HTMLToText converts an HTML document to readable plain text. It is
// intentionally dependency-free (stdlib only) and good enough for feeding a
// language model: scripts/styles are dropped, block elements become line
// breaks, tags are stripped, and entities are decoded.
func HTMLToText(page string) string {
	s := commentRe.ReplaceAllString(page, " ")
	for _, re := range dropRes {
		s = re.ReplaceAllString(s, " ")
	}
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = wsRe.ReplaceAllString(s, " ")
	// Trim trailing spaces on each line and drop empty lines.
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return nlRe.ReplaceAllString(strings.Join(out, "\n"), "\n\n")
}

// ExtractTitle returns the decoded, one-line <title> of an HTML document.
func ExtractTitle(page string) string {
	m := titleRe.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	title := html.UnescapeString(m[1])
	return strings.TrimSpace(wsRe.ReplaceAllString(title, " "))
}

// collapseWhitespace trims and squeezes whitespace in plain-text responses.
func collapseWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = wsRe.ReplaceAllString(s, " ")
	s = nlRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// newSafeHTTPClient builds an HTTP client that refuses to dial non-public
// addresses. The check runs at connection time on the resolved IP, which also
// defends against DNS rebinding.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("refusing to dial non-IP address %q", host)
			}
			if blockedIP(ip) {
				return fmt.Errorf("refusing to dial non-public address %s", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
	}
}

// blockedIP reports whether ip is not a public, routable address.
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsPrivate() {
		return true
	}
	// Carrier-grade NAT (100.64.0.0/10) is not covered by IsPrivate.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
	}
	return false
}
