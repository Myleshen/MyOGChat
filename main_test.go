package main

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"

	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// loadCLIUser opens the CLI's database, loads a user, and returns it. The store
// handle is closed before returning so runUserCmd can reopen it.
func loadCLIUser(t *testing.T, dbPath, ident string) store.User {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	u, err := st.GetUserByIdentifier(context.Background(), ident)
	if err != nil {
		t.Fatalf("GetUserByIdentifier(%s): %v", ident, err)
	}
	return u
}

// cliEncKey returns the decoded DB encryption_key (created on first use).
func cliEncKey(t *testing.T, dbPath string) []byte {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	v, ok, err := st.GetSetting(context.Background(), "encryption_key")
	if err != nil || !ok {
		t.Fatalf("encryption_key: %v (ok=%v)", err, ok)
	}
	key, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("decode encryption_key: %v", err)
	}
	return key
}

// TestUserSetKeyCLI exercises `myogchat user setkey` end to end: encrypt+store,
// then clear. It never returns a plaintext secret from storage.
func TestUserSetKeyCLI(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli.db")
	t.Setenv("MYOGCHAT_DB", dbPath)

	if err := runUserCmd([]string{"add", "--username", "alice",
		"--email", "alice@example.com", "--password", "password123"}); err != nil {
		t.Fatalf("user add: %v", err)
	}

	// Missing --username and no keys are both rejected.
	if err := runUserCmd([]string{"setkey", "--serper", "x"}); err == nil {
		t.Error("setkey without --username should error")
	}
	if err := runUserCmd([]string{"setkey", "--username", "alice"}); err == nil {
		t.Error("setkey with no keys should error")
	}

	// Set all three secrets.
	if err := runUserCmd([]string{"setkey", "--username", "alice",
		"--serper", "serper-secret",
		"--linkwarden-url", "https://links.example.com",
		"--linkwarden-token", "lw-secret"}); err != nil {
		t.Fatalf("setkey: %v", err)
	}

	u := loadCLIUser(t, dbPath, "alice")
	key := cliEncKey(t, dbPath)

	if u.SerperKeyEnc == nil || *u.SerperKeyEnc == "" || *u.SerperKeyEnc == "serper-secret" {
		t.Fatalf("serper not encrypted at rest: %v", u.SerperKeyEnc)
	}
	if raw, err := secret.Decrypt(key, *u.SerperKeyEnc); err != nil || string(raw) != "serper-secret" {
		t.Fatalf("serper decrypt = %q, %v", raw, err)
	}
	if u.LinkwardenURL == nil || *u.LinkwardenURL != "https://links.example.com" {
		t.Fatalf("linkwarden url = %v", u.LinkwardenURL)
	}
	if u.LinkwardenTokenEnc == nil || *u.LinkwardenTokenEnc == "" || *u.LinkwardenTokenEnc == "lw-secret" {
		t.Fatalf("linkwarden token not encrypted at rest: %v", u.LinkwardenTokenEnc)
	}
	if raw, err := secret.Decrypt(key, *u.LinkwardenTokenEnc); err != nil || string(raw) != "lw-secret" {
		t.Fatalf("linkwarden token decrypt = %q, %v", raw, err)
	}

	// An empty value clears that field; an omitted flag leaves it untouched.
	if err := runUserCmd([]string{"setkey", "--username", "alice",
		"--serper", "", "--linkwarden-token", ""}); err != nil {
		t.Fatalf("setkey(clear): %v", err)
	}
	u = loadCLIUser(t, dbPath, "alice")
	if u.SerperKeyEnc != nil {
		t.Fatalf("serper not cleared: %v", u.SerperKeyEnc)
	}
	if u.LinkwardenTokenEnc != nil {
		t.Fatalf("linkwarden token not cleared: %v", u.LinkwardenTokenEnc)
	}
	if u.LinkwardenURL == nil || *u.LinkwardenURL != "https://links.example.com" {
		t.Fatalf("omitted linkwarden-url should be preserved, got %v", u.LinkwardenURL)
	}

	// Clearing the URL too resets both fields.
	if err := runUserCmd([]string{"setkey", "--username", "alice", "--linkwarden-url", ""}); err != nil {
		t.Fatalf("setkey(clear url): %v", err)
	}
	u = loadCLIUser(t, dbPath, "alice")
	if u.LinkwardenURL != nil {
		t.Fatalf("linkwarden url not cleared: %v", u.LinkwardenURL)
	}
}
