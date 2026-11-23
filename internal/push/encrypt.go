package push

// RFC 8291 (Message Encryption for Web Push) payload encryption: one
// aes128gcm record (RFC 8188) with the subscription's ECDH key and auth secret.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	// MaxPayload is the largest plaintext a push service is required to carry
	// (RFC 8291 §4: 4096-byte body less the 86-byte header, padding and tag).
	MaxPayload = 3993
	// recordSize satisfies RFC 8291 §4: larger than plaintext + pad + tag.
	recordSize = 4096
)

// ValidateKeys checks the browser-generated subscription keys: p256dh is an
// uncompressed P-256 public point, auth is a 16-byte secret.
func ValidateKeys(p256dh, auth string) error {
	pub, err := decodeB64(p256dh)
	if err != nil {
		return fmt.Errorf("p256dh: %w", err)
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return fmt.Errorf("p256dh: %w", err)
	}
	secret, err := decodeB64(auth)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if len(secret) != 16 {
		return fmt.Errorf("auth: got %d bytes, want 16", len(secret))
	}
	return nil
}

// decodeB64 accepts base64url with or without padding (browsers send both).
func decodeB64(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// encryptPayload produces the request body for one subscription. salt and the
// application-server key are parameters so the RFC 8291 test vector can be
// reproduced exactly.
func encryptPayload(payload, uaPublic, authSecret, salt []byte, asKey *ecdh.PrivateKey) ([]byte, error) {
	uaPub, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("parse p256dh: %w", err)
	}
	asPub := asKey.PublicKey().Bytes()
	shared, err := asKey.ECDH(uaPub)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	// RFC 8291 §3.3/§3.4: mix the ECDH secret with the auth secret, then bind
	// both public keys into the info string.
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	info := append([]byte("WebPush: info\x00"), uaPublic...)
	info = append(info, asPub...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// A single record always ends with the last-record delimiter 0x02.
	plaintext := append(append([]byte{}, payload...), 0x02)
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	body := make([]byte, 0, 16+4+1+len(asPub)+len(ciphertext))
	body = append(body, salt...)
	body = append(body, byte(recordSize>>24), byte(recordSize>>16), byte(recordSize>>8), byte(recordSize%256))
	body = append(body, byte(len(asPub)))
	body = append(body, asPub...)
	return append(body, ciphertext...), nil
}

// Encrypt encrypts payload for a subscription, generating a fresh salt and
// application-server key pair (RFC 8291).
func Encrypt(payload []byte, p256dh, auth string) ([]byte, error) {
	uaPublic, err := decodeB64(p256dh)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	authSecret, err := decodeB64(auth)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth: want 16 bytes")
	}
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptPayload(payload, uaPublic, authSecret, salt, asKey)
}
