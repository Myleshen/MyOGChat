package upstream

import (
	"strings"
	"testing"
)

// TestUpstreamHTTPError covers the error envelopes the dialects actually
// receive: the message is surfaced with the status code and never as raw JSON.
func TestUpstreamHTTPError(t *testing.T) {
	long := strings.Repeat("x", 250)
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			"openai error.message",
			429,
			`{"error":{"message":"rate limit reached, try again shortly"}}`,
			"upstream HTTP 429: rate limit reached, try again shortly",
		},
		{
			"openai error.type+message",
			429,
			`{"error":{"type":"rate_limit_error","message":"rate limit reached, try again shortly"}}`,
			"upstream HTTP 429: rate limit reached, try again shortly",
		},
		{
			"anthropic envelope",
			429,
			`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit reached, try again shortly"}}`,
			"upstream HTTP 429: rate limit reached, try again shortly",
		},
		{
			"google envelope",
			429,
			`{"error":{"code":429,"message":"rate limit reached, try again shortly","status":"RESOURCE_EXHAUSTED"}}`,
			"upstream HTTP 429: rate limit reached, try again shortly",
		},
		{
			"bare message",
			429,
			`{"message":"rate limit reached, try again shortly"}`,
			"upstream HTTP 429: rate limit reached, try again shortly",
		},
		{
			"status code survives",
			503,
			`{"error":{"message":"backend unavailable"}}`,
			"upstream HTTP 503: backend unavailable",
		},
		{
			"newlines collapse",
			429,
			"{\"error\":{\"message\":\"line one\\nline two\\r\\nline three\"}}",
			"upstream HTTP 429: line one line two line three",
		},
		{
			"long message caps at 200 chars",
			429,
			`{"error":{"message":"` + long + `"}}`,
			"upstream HTTP 429: " + long[:200],
		},
		{
			"plain text body",
			429,
			"rate limited\n",
			"upstream HTTP 429: rate limited",
		},
		{
			"empty body",
			429,
			"",
			"upstream HTTP 429: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := upstreamHTTPError(tc.status, []byte(tc.body))
			if err == nil {
				t.Fatal("err = nil")
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("err = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUpstreamHTTPErrorTruncatesHTML keeps the old fallback for a body that is
// not JSON: a plain snippet, cut at 200 bytes.
func TestUpstreamHTTPErrorTruncatesHTML(t *testing.T) {
	body := "<html><body><h1>400 Bad Request</h1>" + strings.Repeat("padding ", 60) + "</body></html>"
	got := upstreamHTTPError(400, []byte(body)).Error()
	want := "upstream HTTP 400: " + body[:200]
	if got != want {
		t.Errorf("err = %q, want %q", got, want)
	}
	if strings.Contains(got, "</html>") {
		t.Error("the HTML page was not truncated")
	}
}
