package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestUserProvidersMigrationOnLegacyDB builds a database from before custom
// providers existed and confirms Open adds the table without touching any row.
func TestUserProvidersMigrationOnLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	const legacy = `
CREATE TABLE conversations (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL,
  effort     TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
INSERT INTO conversations (id, title, model, created_at, updated_at)
VALUES ('legacy', 'Old Chat', 'glm-5.3-flash', 111, 222);`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM user_providers`).Scan(&n); err != nil {
		t.Fatalf("user_providers table missing after migration: %v", err)
	}
	var title string
	if err := s.db.QueryRow(`SELECT title FROM conversations WHERE id = 'legacy'`).Scan(&title); err != nil {
		t.Fatalf("legacy conversation lost: %v", err)
	}
	if title != "Old Chat" {
		t.Errorf("legacy title = %q, want %q", title, "Old Chat")
	}
}

// TestUserProviderCRUDAndScoping exercises the five store methods and confirms
// another user's rows behave as not found.
func TestUserProviderCRUDAndScoping(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	alice := seedUser(t, s, "u-alice", "alice", false)
	bob := seedUser(t, s, "u-bob", "bob", false)

	p := &Provider{
		ID: "p1", UserID: alice.ID, Name: "NVIDIA NIM", Slug: "nvidia-nim",
		BaseURL: "https://integrate.api.nvidia.com/v1", KeyEnc: "ciphertext",
		Models:    []ProviderModel{{ID: "meta/llama-3.1-8b", Name: "Llama 3.1 8B"}},
		CreatedAt: 10,
	}
	if err := s.CreateUserProvider(ctx, p); err != nil {
		t.Fatalf("CreateUserProvider: %v", err)
	}

	list, err := s.ListUserProviders(ctx, alice.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListUserProviders(alice) = %+v, %v; want one", list, err)
	}
	if got := list[0]; got.Slug != "nvidia-nim" || got.BaseURL != p.BaseURL ||
		got.KeyEnc != "ciphertext" || len(got.Models) != 1 || got.Models[0].ID != "meta/llama-3.1-8b" {
		t.Errorf("round-trip = %+v", got)
	}

	got, err := s.GetUserProvider(ctx, p.ID, alice.ID)
	if err != nil || got.ID != p.ID {
		t.Fatalf("GetUserProvider(alice) = %+v, %v", got, err)
	}

	if err := s.UpdateUserProviderModels(ctx, p.ID, alice.ID,
		[]ProviderModel{{ID: "nvidia/nemotron", Name: "Nemotron"}}); err != nil {
		t.Fatalf("UpdateUserProviderModels: %v", err)
	}
	got, _ = s.GetUserProvider(ctx, p.ID, alice.ID)
	if len(got.Models) != 1 || got.Models[0].ID != "nvidia/nemotron" {
		t.Errorf("updated models = %+v", got.Models)
	}

	// A second user sees nothing and cannot touch the row.
	if list, err := s.ListUserProviders(ctx, bob.ID); err != nil || len(list) != 0 {
		t.Errorf("ListUserProviders(bob) = %+v, %v; want empty", list, err)
	}
	if _, err := s.GetUserProvider(ctx, p.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserProvider(bob) = %v, want ErrNotFound", err)
	}
	if err := s.UpdateUserProviderModels(ctx, p.ID, bob.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateUserProviderModels(bob) = %v, want ErrNotFound", err)
	}
	if err := s.DeleteUserProvider(ctx, p.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteUserProvider(bob) = %v, want ErrNotFound", err)
	}

	if err := s.DeleteUserProvider(ctx, p.ID, alice.ID); err != nil {
		t.Fatalf("DeleteUserProvider(alice): %v", err)
	}
	if _, err := s.GetUserProvider(ctx, p.ID, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete GetUserProvider = %v, want ErrNotFound", err)
	}
}

// TestUserProviderEmptyModelsRoundTrip confirms an empty catalog reads back as
// an empty JSON array (not null), so the API always sends [].
func TestUserProviderEmptyModelsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "alice", false)
	if err := s.CreateUserProvider(ctx, &Provider{
		ID: "p1", UserID: u.ID, Name: "Local", Slug: "local",
		BaseURL: "http://localhost:8080/v1", KeyEnc: "x", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("CreateUserProvider: %v", err)
	}
	got, err := s.GetUserProvider(ctx, "p1", u.ID)
	if err != nil {
		t.Fatalf("GetUserProvider: %v", err)
	}
	if got.Models == nil || len(got.Models) != 0 {
		t.Errorf("Models = %#v, want empty non-nil slice", got.Models)
	}
}
