// Package push sends Web Push notifications with VAPID authentication
// (RFC 8292) and RFC 8291 payload encryption, using only the standard library.
// No third-party push service: the message goes to the browser's own endpoint.
package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ErrGone reports that the push service no longer knows the subscription
// (HTTP 404/410): the row should be deleted.
var ErrGone = errors.New("push subscription is gone")

// jwtLifetime is the VAPID token lifetime. RFC 8292 caps it at 24 h.
const jwtLifetime = 12 * time.Hour

// GenerateKey returns a PKCS#8 DER VAPID private key and its base64url-encoded
// uncompressed P-256 public key. The private key must be stored encrypted and
// never logged.
func GenerateKey() (privDER []byte, publicKey string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate VAPID key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, "", fmt.Errorf("marshal VAPID key: %w", err)
	}
	return der, base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y)), nil
}

// Sender signs push requests with one VAPID key pair. Client may be replaced
// (tests do, to trust a local TLS server).
type Sender struct {
	key    *ecdsa.PrivateKey
	public string
	sub    string
	Client *http.Client
}

// NewSender parses a PKCS#8 DER VAPID private key. subject is the VAPID
// contact: a mailto: or https: URL. It is only sent to the browser's push
// service, never to users.
func NewSender(privDER []byte, subject string) (*Sender, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(privDER)
	if err != nil {
		return nil, fmt.Errorf("parse VAPID key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("VAPID key is not an ECDSA P-256 private key")
	}
	if subject == "" {
		return nil, errors.New("VAPID subject is required")
	}
	return &Sender{
		key:    key,
		public: base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y)),
		sub:    subject,
	}, nil
}

// PublicKey is the base64url VAPID public key a browser passes to
// pushManager.subscribe as applicationServerKey.
func (s *Sender) PublicKey() string { return s.public }

// authorization builds the RFC 8292 Authorization header for endpoint.
func (s *Sender) authorization(endpoint string, now time.Time) (string, error) {
	aud, err := endpointOrigin(endpoint)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{Aud: aud, Exp: now.Add(jwtLifetime).Unix(), Sub: s.sub})
	if err != nil {
		return "", fmt.Errorf("marshal VAPID claims: %w", err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign VAPID token: %w", err)
	}
	// JWS ES256 wants the raw 32-byte r || s, not ASN.1 DER.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return "vapid t=" + signingInput + "." + base64.RawURLEncoding.EncodeToString(sig) + ",k=" + s.public, nil
}

// endpointOrigin returns the RFC 6454 origin of a push endpoint (the JWT aud).
func endpointOrigin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid push endpoint")
	}
	return u.Scheme + "://" + u.Host, nil
}
