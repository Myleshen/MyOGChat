package secret

import (
	"crypto/rand"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	const plain = "sk-super-secret-key-value"
	enc, err := Encrypt(key, []byte(plain))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc == plain {
		t.Fatal("ciphertext equals plaintext")
	}
	got, err := Decrypt(key, enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("round trip = %q, want %q", got, plain)
	}
	// A different key must fail (AEAD authentication).
	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(other, enc); err == nil {
		t.Error("decrypt with wrong key succeeded")
	}
	if _, err := Decrypt(key, "not-base64!!"); err == nil {
		t.Error("decrypt of garbage succeeded")
	}
}
