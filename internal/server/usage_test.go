package server

import (
	"context"
	"testing"
	"time"

	"myogchat/internal/store"
)

func int64ptr(v int64) *int64 { return &v }

// seedServerUsage inserts an assistant message with counters into convID.
func seedServerUsage(t *testing.T, st *store.Store, id, convID, model string, createdAt, prompt, completion, cost int64) {
	t.Helper()
	if err := st.InsertMessage(context.Background(), &store.Message{
		ID: id, ConversationID: convID, Role: "assistant", Content: "x", Model: &model,
		PromptTokens: int64ptr(prompt), CompletionTokens: int64ptr(completion),
		CachedTokens: int64ptr(0), ReasoningTokens: int64ptr(0),
		CostMicros: int64ptr(cost), CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("InsertMessage(%s): %v", id, err)
	}
}

func TestUsageCutoff(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	if c, l := usageCutoff("all", now); c != 0 || l != "all" {
		t.Errorf("all = %d, %q; want 0, all", c, l)
	}
	want7 := startOfUTCDay(time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC))
	if c, l := usageCutoff("7d", now); c != want7 || l != "7d" {
		t.Errorf("7d = %d, %q; want %d, 7d", c, l, want7)
	}
	want30 := startOfUTCDay(time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC))
	if c, l := usageCutoff("30d", now); c != want30 || l != "30d" {
		t.Errorf("30d = %d, %q; want %d, 30d", c, l, want30)
	}
	// Unknown/absent ranges default to 30d.
	if c, l := usageCutoff("bogus", now); c != want30 || l != "30d" {
		t.Errorf("bogus = %d, %q; want the 30d default", c, l)
	}
}

func TestStartOfUTCDayMonth(t *testing.T) {
	tm := time.Date(2026, 5, 15, 23, 30, 0, 0, time.UTC)
	if got, want := startOfUTCDay(tm), time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC).UnixMilli(); got != want {
		t.Errorf("startOfUTCDay = %d, want %d", got, want)
	}
	if got, want := startOfUTCMonth(tm), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).UnixMilli(); got != want {
		t.Errorf("startOfUTCMonth = %d, want %d", got, want)
	}
}

func TestEffectiveLimits(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u1", "alice", "")

	if err := st.SetSetting(ctx, settingDailyTokenLimit, "1000"); err != nil {
		t.Fatalf("SetSetting(daily): %v", err)
	}
	if err := st.SetSetting(ctx, settingMonthlyCostLimit, "2000"); err != nil {
		t.Fatalf("SetSetting(monthly): %v", err)
	}
	if d, m := srv.effectiveLimits(ctx, u); d != 1000 || m != 2000 {
		t.Errorf("global limits = %d, %d; want 1000, 2000", d, m)
	}

	// A per-user override wins over the global setting.
	u.DailyTokenLimit = int64ptr(5)
	u.MonthlyCostLimit = int64ptr(6)
	if d, m := srv.effectiveLimits(ctx, u); d != 5 || m != 6 {
		t.Errorf("override limits = %d, %d; want 5, 6", d, m)
	}
}

func TestCheckQuota(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u1", "alice", "")

	if err := st.InsertConversation(ctx, &store.Conversation{
		ID: "c1", UserID: &u.ID, Model: "m", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertConversation: %v", err)
	}
	now := time.Now()
	// Today: 50+30 = 80 tokens, 500 micros.
	seedServerUsage(t, st, "a1", "c1", "m", now.UnixMilli(), 50, 30, 500)

	// No limits → allowed (and no queries).
	if err := srv.checkQuota(ctx, u); err != nil {
		t.Fatalf("no limits = %v, want nil", err)
	}

	// Daily token limit: 100 > 80 → allowed.
	u.DailyTokenLimit = int64ptr(100)
	if err := srv.checkQuota(ctx, u); err != nil {
		t.Fatalf("daily 100 = %v, want nil", err)
	}
	// Daily token limit: 80 >= 80 → denied.
	u.DailyTokenLimit = int64ptr(80)
	if err := srv.checkQuota(ctx, u); err == nil {
		t.Fatal("daily 80 = nil, want denial")
	}

	// Monthly cost limit: 500 >= 500 → denied (daily off).
	u.DailyTokenLimit = nil
	u.MonthlyCostLimit = int64ptr(500)
	if err := srv.checkQuota(ctx, u); err == nil {
		t.Fatal("monthly 500 = nil, want denial")
	}
	// Monthly cost limit: 501 > 500 → allowed.
	u.MonthlyCostLimit = int64ptr(501)
	if err := srv.checkQuota(ctx, u); err != nil {
		t.Fatalf("monthly 501 = %v, want nil", err)
	}
}

// TestCheckQuotaFailsOpen verifies a DB error cannot lock a user out. The limit
// is supplied as a per-user override so the limit lookup itself does not need a
// working DB; closing the store then forces the usage query to fail.
func TestCheckQuotaFailsOpen(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u1", "alice", "")
	u.DailyTokenLimit = int64ptr(1000)
	st.Close() // subsequent queries error

	if err := srv.checkQuota(ctx, u); err != nil {
		t.Fatalf("checkQuota on DB error = %v, want nil (fail open)", err)
	}
}
