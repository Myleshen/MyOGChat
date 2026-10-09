package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"myogchat/internal/secret"
	"myogchat/internal/store"
)

// testEncKey is a deterministic 32-byte AES key shared by the fixtures.
func testEncKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func newKeyTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := &Server{st: st, encKey: testEncKey(), log: slog.Default()}
	return srv, st
}

// seedKeyUser inserts an active user, storing rawKey encrypted when non-empty.
func seedKeyUser(t *testing.T, st *store.Store, id, username, rawKey string) store.User {
	t.Helper()
	u := store.User{
		ID: id, Username: username, Email: username + "@example.com",
		PasswordHash: "x", Status: "active", CreatedAt: 1, UpdatedAt: 1,
	}
	if rawKey != "" {
		enc, err := secret.Encrypt(testEncKey(), []byte(rawKey))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		u.APIKeyEnc = &enc
	}
	if err := st.InsertUser(context.Background(), &u); err != nil {
		t.Fatalf("InsertUser(%s): %v", username, err)
	}
	return u
}

func mustGroup(t *testing.T, st *store.Store, g *store.Group) {
	t.Helper()
	if err := st.InsertGroup(context.Background(), g); err != nil {
		t.Fatalf("InsertGroup(%s): %v", g.ID, err)
	}
}

func TestResolveKeyOrder(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()

	owner := seedKeyUser(t, st, "u-owner", "owner", "owner-key")
	member := seedKeyUser(t, st, "u-member", "member", "")
	other := seedKeyUser(t, st, "u-other", "other", "")

	// No own, group, or server key → clear error.
	if _, err := srv.resolveKey(ctx, member); err != errNoKey {
		t.Fatalf("resolveKey(no keys) = %v, want errNoKey", err)
	}

	// A shared group key is used by its members.
	mustGroup(t, st, &store.Group{ID: "g1", Name: "Team", OwnerID: owner.ID, ShareKey: true, CreatedAt: 10, UpdatedAt: 10})
	if err := st.AddGroupMember(ctx, "g1", member.ID, "member", 11); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
	if got, err := srv.resolveKey(ctx, member); err != nil || got != "owner-key" {
		t.Fatalf("resolveKey(member) = %q, %v; want owner-key", got, err)
	}

	// The user's own key beats any group key.
	enc, err := secret.Encrypt(testEncKey(), []byte("member-key"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	withOwn := member
	withOwn.APIKeyEnc = &enc
	if got, err := srv.resolveKey(ctx, withOwn); err != nil || got != "member-key" {
		t.Fatalf("own-key precedence = %q, %v; want member-key", got, err)
	}

	// The oldest applicable group wins when several apply.
	owner2 := seedKeyUser(t, st, "u-owner2", "owner2", "owner2-key")
	mustGroup(t, st, &store.Group{ID: "g0", Name: "Older", OwnerID: owner2.ID, ShareKey: true, CreatedAt: 5, UpdatedAt: 5})
	if err := st.AddGroupMember(ctx, "g0", member.ID, "member", 11); err != nil {
		t.Fatalf("AddGroupMember(g0): %v", err)
	}
	if key, name, ok, err := srv.groupKey(ctx, member.ID); err != nil || !ok || key != "owner2-key" || name != "Older" {
		t.Fatalf("groupKey = %q, %q, %v, %v; want owner2-key, Older, true, nil", key, name, ok, err)
	}

	// A group whose owner has no key is skipped.
	owner3 := seedKeyUser(t, st, "u-owner3", "owner3", "")
	mustGroup(t, st, &store.Group{ID: "g2", Name: "NoKey", OwnerID: owner3.ID, ShareKey: true, CreatedAt: 1, UpdatedAt: 1})
	if err := st.AddGroupMember(ctx, "g2", member.ID, "member", 11); err != nil {
		t.Fatalf("AddGroupMember(g2): %v", err)
	}
	if key, _, _, err := srv.groupKey(ctx, member.ID); err != nil || key != "owner2-key" {
		t.Fatalf("groupKey with keyless group = %q, %v; want owner2-key", key, err)
	}

	// Server-key fallback for a user with no own or group key (sharing default on).
	srv.serverKey = "server-key"
	if got, err := srv.resolveKey(ctx, other); err != nil || got != "server-key" {
		t.Fatalf("resolveKey(server fallback) = %q, %v; want server-key", got, err)
	}
	// ...and off when sharing is disabled.
	if err := st.SetSetting(ctx, "share_server_key", "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := srv.resolveKey(ctx, other); err != errNoKey {
		t.Fatalf("resolveKey(server sharing off) = %v, want errNoKey", err)
	}
}
