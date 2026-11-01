package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newKeyCheckServer serves the catalog endpoint and records what it received.
func newKeyCheckServer(t *testing.T, status int, body string) (*Client, *http.Header) {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{hc: srv.Client(), modelsURL: srv.URL}, &got
}

func TestValidateKey(t *testing.T) {
	t.Run("valid key", func(t *testing.T) {
		c, hdr := newKeyCheckServer(t, http.StatusOK, `{"object":"list","data":[]}`)
		if err := c.ValidateKey(context.Background(), "oc_sk_good"); err != nil {
			t.Fatalf("ValidateKey = %v, want nil", err)
		}
		if got := hdr.Get("Authorization"); got != "Bearer oc_sk_good" {
			t.Errorf("Authorization = %q", got)
		}
		if got := *hdr; got.Get("User-Agent") != userAgent {
			t.Errorf("User-Agent = %q", got.Get("User-Agent"))
		}
	})

	t.Run("rejected key", func(t *testing.T) {
		c, _ := newKeyCheckServer(t, http.StatusUnauthorized,
			`{"error":{"type":"server_error","message":"Invalid credential"}}`)
		err := c.ValidateKey(context.Background(), "oc_sk_bad")
		if err == nil || err.Error() != "invalid API key" {
			t.Fatalf("ValidateKey = %v, want invalid API key", err)
		}
	})

	t.Run("upstream failure keeps the body", func(t *testing.T) {
		c, _ := newKeyCheckServer(t, http.StatusBadRequest, `{"error":{"message":"bad request"}}`)
		err := c.ValidateKey(context.Background(), "oc_sk_x")
		if err == nil {
			t.Fatal("ValidateKey = nil, want error")
		}
		want := `upstream HTTP 400: {"error":{"message":"bad request"}}`
		if err.Error() != want {
			t.Errorf("ValidateKey = %q, want %q", err, want)
		}
	})

	t.Run("200 with error body", func(t *testing.T) {
		c, _ := newKeyCheckServer(t, http.StatusOK, `{"error":{"message":"nope"}}`)
		if err := c.ValidateKey(context.Background(), "oc_sk_x"); err == nil {
			t.Fatal("ValidateKey = nil, want error")
		}
	})
}

// A Client built without NewClient (as tests do) must still hit the real
// catalog URL rather than an empty string.
func TestValidateKeyDefaultsModelsURL(t *testing.T) {
	c := &Client{hc: http.DefaultClient}
	if c.modelsURL != "" {
		t.Fatalf("modelsURL = %q, want empty", c.modelsURL)
	}
	if ModelsURL == "" {
		t.Fatal("ModelsURL must not be empty")
	}
}
