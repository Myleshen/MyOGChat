package push

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.ReplaceAll(s, "\n", ""))
	if err != nil {
		t.Fatalf("base64url %q: %v", s, err)
	}
	return b
}

// TestRFC8291KnownAnswer reproduces the example in RFC 8291 §5: fixed salt,
// fixed application-server key and the receiver's public key must produce the
// RFC's exact aes128gcm body (appendix A lists the intermediate values).
func TestRFC8291KnownAnswer(t *testing.T) {
	const (
		plaintext = "When I grow up, I want to be a watermelon"
		saltB64   = "DGv6ra1nlYgDCS1FRnbzlw"
		asPrivB64 = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
		uaPubB64  = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
		authB64   = "BTBZMqHH6r4Tts7J_aSIgg"
		wantB64   = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
			"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
			"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	)
	asKey, err := ecdh.P256().NewPrivateKey(mustB64(t, asPrivB64))
	if err != nil {
		t.Fatalf("application server key: %v", err)
	}
	got, err := encryptPayload([]byte(plaintext), mustB64(t, uaPubB64), mustB64(t, authB64),
		mustB64(t, saltB64), asKey)
	if err != nil {
		t.Fatalf("encryptPayload: %v", err)
	}
	if enc := base64.RawURLEncoding.EncodeToString(got); enc != wantB64 {
		t.Fatalf("body mismatch\n got %s\nwant %s", enc, wantB64)
	}
	// Appendix A splits the same 144-byte body into the 86-byte header and the
	// 58-byte ciphertext; checking both catches a transcription slip in wantB64.
	if wantHeader := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"); string(got[:86]) != string(wantHeader) {
		t.Fatal("header does not match the RFC's salt + record size + keyid")
	}
	if wantCT := mustB64(t, "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ"); string(got[86:]) != string(wantCT) {
		t.Fatal("ciphertext does not match the RFC's Appendix A vector")
	}
}

// TestVAPIDJWT asserts the Authorization header carries an ES256 JWT whose aud
// is the endpoint origin, whose exp is ~12 h out, and whose signature verifies.
func TestVAPIDJWT(t *testing.T) {
	var (
		authHeader string
		gotBody    []byte
		encoding   string
		ttl, urg   string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		encoding = r.Header.Get("Content-Encoding")
		ttl = r.Header.Get("TTL")
		urg = r.Header.Get("Urgency")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	der, pubB64, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	snd, err := NewSender(der, "mailto:test@example.com")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if snd.PublicKey() != pubB64 {
		t.Fatalf("public key %q != generated %q", snd.PublicKey(), pubB64)
	}
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("subscription key: %v", err)
	}
	auth := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	sub := Subscription{
		Endpoint: ts.URL + "/push/abc",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes()),
		Auth:     auth,
	}
	if err := snd.Send(nil, sub, []byte(`{"title":"t","body":"b"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(authHeader, "vapid t=") || !strings.Contains(authHeader, ",k="+pubB64) {
		t.Fatalf("Authorization = %q, want vapid t=...,k=%s", authHeader, pubB64)
	}
	if encoding != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q, want aes128gcm", encoding)
	}
	if ttl == "" || urg != "normal" {
		t.Fatalf("TTL = %q, Urgency = %q; both must be set", ttl, urg)
	}
	if len(gotBody) <= 86 {
		t.Fatalf("body is %d bytes, want header + ciphertext", len(gotBody))
	}

	token := strings.TrimPrefix(strings.Split(authHeader, ",")[0], "vapid t=")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(mustB64(t, parts[0]), &header); err != nil {
		t.Fatalf("JWT header: %v", err)
	}
	if header.Alg != "ES256" || header.Typ != "JWT" {
		t.Fatalf("JWT header = %+v, want ES256/JWT", header)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatalf("JWT claims: %v", err)
	}
	if claims.Aud != ts.URL {
		t.Fatalf("aud = %q, want endpoint origin %q", claims.Aud, ts.URL)
	}
	if claims.Sub != "mailto:test@example.com" {
		t.Fatalf("sub = %q", claims.Sub)
	}
	if d := time.Until(time.Unix(claims.Exp, 0)); d < jwtLifetime-time.Minute || d > jwtLifetime+time.Minute {
		t.Fatalf("exp is %v out, want ~%v", d, jwtLifetime)
	}

	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("ES256 signature is %d bytes, want 64", len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&snd.key.PublicKey, digest[:], r, s) {
		t.Fatal("JWT signature does not verify")
	}
}

func TestOversizedPayloadRejected(t *testing.T) {
	der, _, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	snd, err := NewSender(der, "mailto:test@example.com")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("subscription key: %v", err)
	}
	sub := Subscription{
		Endpoint: "https://push.example.net/x",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
	}
	if err := snd.Send(nil, sub, make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("oversized payload was accepted")
	}
}

func TestMalformedKeysRejected(t *testing.T) {
	if _, _, err := GenerateKey(); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if _, err := NewSender([]byte("not-a-key"), "mailto:a@b.c"); err == nil {
		t.Fatal("NewSender accepted garbage")
	}
	bad := []struct{ p256dh, auth string }{
		{"!!!", base64.RawURLEncoding.EncodeToString(make([]byte, 16))},
		{base64.RawURLEncoding.EncodeToString(make([]byte, 65)), base64.RawURLEncoding.EncodeToString(make([]byte, 16))},
		{"", ""},
		{validPub(t), base64.RawURLEncoding.EncodeToString(make([]byte, 8))},
	}
	for _, b := range bad {
		if err := ValidateKeys(b.p256dh, b.auth); err == nil {
			t.Fatalf("ValidateKeys(%q, %q) accepted malformed keys", b.p256dh, b.auth)
		}
	}
}

func validPub(t *testing.T) string {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
}
