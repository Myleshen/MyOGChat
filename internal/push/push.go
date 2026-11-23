package push

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ttlSeconds is how long the push service keeps an undelivered notification: a
// phone that was offline for a day has missed the point of it.
const ttlSeconds = 12 * 60 * 60

// requestTimeout bounds one delivery attempt.
const requestTimeout = 10 * time.Second

// Subscription is the minimum needed to deliver to one browser subscription.
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Send delivers payload to one subscription. payload must be JSON for the
// service worker; it is encrypted end-to-end, so the push service only sees
// ciphertext.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("push payload is %d bytes, limit is %d", len(payload), MaxPayload)
	}
	body, err := Encrypt(payload, sub.P256dh, sub.Auth)
	if err != nil {
		return fmt.Errorf("encrypt push payload: %w", err)
	}
	auth, err := s.authorization(sub.Endpoint, time.Now())
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(ttlSeconds))
	req.Header.Set("Urgency", "normal")

	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("push request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	default:
		return fmt.Errorf("push service returned %s", resp.Status)
	}
}

// IsGone reports whether err means the subscription must be deleted.
func IsGone(err error) bool { return errors.Is(err, ErrGone) }
