package store

import (
	"context"
	"errors"
	"testing"
)

func pushSub(id, userID, endpoint string) *PushSubscription {
	return &PushSubscription{
		ID: id, UserID: userID, Endpoint: endpoint,
		P256dh: "p-" + id, Auth: "a-" + id, UserAgent: "ua",
	}
}

// TestPushSubscriptionUpsertByEndpoint: re-subscribing the same endpoint
// updates the existing row (and its owner) instead of adding one.
func TestPushSubscriptionUpsertByEndpoint(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u1 := seedUser(t, s, "u1", "alice", false)
	u2 := seedUser(t, s, "u2", "bob", false)

	id, err := s.UpsertPushSubscription(ctx, pushSub("p1", u1.ID, "https://push.example/1"))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if id != "p1" {
		t.Fatalf("id = %q, want p1", id)
	}
	// Same endpoint, new id + owner + keys: the original row wins the id.
	id2, err := s.UpsertPushSubscription(ctx, pushSub("p2", u2.ID, "https://push.example/1"))
	if err != nil {
		t.Fatalf("Upsert again: %v", err)
	}
	if id2 != "p1" {
		t.Fatalf("re-upsert id = %q, want the existing p1", id2)
	}
	if n, err := s.CountPushSubscriptions(ctx, u2.ID); err != nil || n != 1 {
		t.Fatalf("count(u2) = %d, %v; want 1", n, err)
	}
	if n, err := s.CountPushSubscriptions(ctx, u1.ID); err != nil || n != 0 {
		t.Fatalf("count(u1) = %d, %v; want 0 (row moved to u2)", n, err)
	}
	subs, err := s.ListPushSubscriptions(ctx, u2.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("list = %+v, %v", subs, err)
	}
	if subs[0].P256dh != "p-p2" || subs[0].Auth != "a-p2" {
		t.Fatalf("keys were not updated: %+v", subs[0])
	}
}

// TestPushSubscriptionFailuresAndPrune covers the failure counter, the reset on
// success, the owner-scoped delete and the unscoped prune.
func TestPushSubscriptionFailuresAndPrune(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "alice", false)
	other := seedUser(t, s, "u2", "bob", false)

	if _, err := s.UpsertPushSubscription(ctx, pushSub("p1", u.ID, "https://push.example/1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for want := 1; want <= 3; want++ {
		n, err := s.MarkPushFailed(ctx, "p1")
		if err != nil {
			t.Fatalf("MarkPushFailed: %v", err)
		}
		if n != want {
			t.Fatalf("failures = %d, want %d", n, want)
		}
	}
	if err := s.MarkPushSent(ctx, "p1"); err != nil {
		t.Fatalf("MarkPushSent: %v", err)
	}
	subs, _ := s.ListPushSubscriptions(ctx, u.ID)
	if len(subs) != 1 || subs[0].Failures != 0 || subs[0].LastUsed == 0 {
		t.Fatalf("after success: %+v, want failures 0 and last_used set", subs)
	}

	// Another user's delete changes nothing.
	if deleted, err := s.DeletePushSubscription(ctx, "p1", other.ID); err != nil || deleted {
		t.Fatalf("foreign delete = %v, %v; want false, nil", deleted, err)
	}
	if owner, err := s.PushSubscriptionOwner(ctx, "p1"); err != nil || owner != u.ID {
		t.Fatalf("owner = %q, %v; want %q", owner, err, u.ID)
	}
	if _, err := s.PushSubscriptionOwner(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner of missing row = %v, want ErrNotFound", err)
	}
	if deleted, err := s.DeletePushSubscription(ctx, "p1", u.ID); err != nil || !deleted {
		t.Fatalf("own delete = %v, %v; want true, nil", deleted, err)
	}

	// The delivery path prunes without an owner check.
	if _, err := s.UpsertPushSubscription(ctx, pushSub("p3", u.ID, "https://push.example/3")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.DeletePushSubscriptionByID(ctx, "p3"); err != nil {
		t.Fatalf("DeletePushSubscriptionByID: %v", err)
	}
	if n, _ := s.CountPushSubscriptions(ctx, u.ID); n != 0 {
		t.Fatalf("count after prune = %d, want 0", n)
	}
}

// TestPushSubscriptionCascade: deleting a user removes their subscriptions.
func TestPushSubscriptionCascade(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "alice", false)
	if _, err := s.UpsertPushSubscription(ctx, pushSub("p1", u.ID, "https://push.example/1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if n, err := s.CountPushSubscriptions(ctx, u.ID); err != nil || n != 0 {
		t.Fatalf("count after user delete = %d, %v; want 0", n, err)
	}
}
