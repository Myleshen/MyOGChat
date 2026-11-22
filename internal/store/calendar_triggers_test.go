package store

import (
	"context"
	"testing"
)

// TestCalendarTriggerConfigRoundTrip covers the config row and its delete,
// which also clears the run ledger.
func TestCalendarTriggerConfigRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	u := seedUser(t, s, "u1", "alice", false)

	if _, err := s.GetCalendarTrigger(ctx, u.ID); err != ErrNotFound {
		t.Fatalf("GetCalendarTrigger(empty) = %v, want ErrNotFound", err)
	}
	start, end := "22:00", "06:00"
	tr := &CalendarTrigger{
		UserID: u.ID, Enabled: true, Model: "deepseek-v4.1-flash",
		PromptTemplate: "prep {{summary}}", QuietStart: &start, QuietEnd: &end, UpdatedAt: 10,
	}
	if err := s.UpsertCalendarTrigger(ctx, tr); err != nil {
		t.Fatalf("UpsertCalendarTrigger: %v", err)
	}
	got, err := s.GetCalendarTrigger(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetCalendarTrigger: %v", err)
	}
	if !got.Enabled || got.Model != tr.Model || got.PromptTemplate != tr.PromptTemplate ||
		got.QuietStart == nil || *got.QuietStart != start || got.QuietEnd == nil || *got.QuietEnd != end {
		t.Fatalf("config = %+v", got)
	}

	// Update keeps one row per user.
	tr.Enabled = false
	tr.QuietStart, tr.QuietEnd = nil, nil
	tr.UpdatedAt = 20
	if err := s.UpsertCalendarTrigger(ctx, tr); err != nil {
		t.Fatalf("second UpsertCalendarTrigger: %v", err)
	}
	got, _ = s.GetCalendarTrigger(ctx, u.ID)
	if got.Enabled || got.QuietStart != nil || got.UpdatedAt != 20 {
		t.Fatalf("updated config = %+v", got)
	}

	if _, err := s.InsertTriggerRun(ctx, &TriggerRun{
		ID: "r1", UserID: u.ID, EventUID: "e1", EventStart: 1, Summary: "s",
		Status: "fired", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTriggerRun: %v", err)
	}
	n, err := s.DeleteCalendarTrigger(ctx, u.ID)
	if err != nil || n != 1 {
		t.Fatalf("DeleteCalendarTrigger = %d, %v; want 1 row", n, err)
	}
	if _, err := s.GetCalendarTrigger(ctx, u.ID); err != ErrNotFound {
		t.Errorf("config survived delete: %v", err)
	}
	if runs, _ := s.ListTriggerRuns(ctx, u.ID, 10); len(runs) != 0 {
		t.Errorf("runs survived delete: %+v", runs)
	}
	// Idempotent.
	if n, err := s.DeleteCalendarTrigger(ctx, u.ID); err != nil || n != 0 {
		t.Errorf("second delete = %d, %v; want 0 rows", n, err)
	}
}

// TestTriggerRunDedupeAndOrdering covers the unique (user, uid, start) key and
// the newest-first list.
func TestTriggerRunDedupeAndOrdering(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	owner := seedUser(t, s, "u1", "alice", false)
	other := seedUser(t, s, "u2", "bob", false)

	first := &TriggerRun{
		ID: "r1", UserID: owner.ID, EventUID: "e1", EventStart: 100,
		Summary: "One", Status: "fired", CreatedAt: 10,
	}
	ok, err := s.InsertTriggerRun(ctx, first)
	if err != nil || !ok {
		t.Fatalf("first insert = %v, %v; want inserted", ok, err)
	}
	ok, err = s.InsertTriggerRun(ctx, &TriggerRun{
		ID: "r1-dup", UserID: owner.ID, EventUID: "e1", EventStart: 100,
		Summary: "One", Status: "fired", CreatedAt: 11,
	})
	if err != nil || ok {
		t.Fatalf("duplicate insert = %v, %v; want refused without error", ok, err)
	}
	// A different start is a different instance (a moved event).
	if ok, err := s.InsertTriggerRun(ctx, &TriggerRun{
		ID: "r2", UserID: owner.ID, EventUID: "e1", EventStart: 200,
		Summary: "One", Status: "fired", CreatedAt: 20,
	}); err != nil || !ok {
		t.Fatalf("moved instance insert = %v, %v; want inserted", ok, err)
	}
	// Another user's identical key is their own.
	if ok, err := s.InsertTriggerRun(ctx, &TriggerRun{
		ID: "r3", UserID: other.ID, EventUID: "e1", EventStart: 100,
		Summary: "One", Status: "fired", CreatedAt: 30,
	}); err != nil || !ok {
		t.Fatalf("other user insert = %v, %v; want inserted", ok, err)
	}
	has, err := s.HasTriggerRun(ctx, owner.ID, "e1", 100)
	if err != nil || !has {
		t.Fatalf("HasTriggerRun = %v, %v; want true", has, err)
	}
	if has, _ := s.HasTriggerRun(ctx, owner.ID, "e1", 999); has {
		t.Error("HasTriggerRun reported an unknown instance")
	}

	runs, err := s.ListTriggerRuns(ctx, owner.ID, 10)
	if err != nil {
		t.Fatalf("ListTriggerRuns: %v", err)
	}
	if len(runs) != 2 || runs[0].ID != "r2" || runs[1].ID != "r1" {
		t.Fatalf("runs = %+v, want newest first and owner-scoped", runs)
	}
	if runs[0].ConversationID != nil {
		t.Errorf("conversation = %v, want nil", runs[0].ConversationID)
	}

	if err := s.UpdateTriggerRun(ctx, "r1", "skipped_busy", "owner was busy", "c1"); err != nil {
		t.Fatalf("UpdateTriggerRun: %v", err)
	}
	runs, _ = s.ListTriggerRuns(ctx, owner.ID, 10)
	if runs[1].Status != "skipped_busy" || runs[1].Reason != "owner was busy" ||
		runs[1].ConversationID == nil || *runs[1].ConversationID != "c1" {
		t.Fatalf("updated run = %+v", runs[1])
	}

	if n, err := s.DeleteTriggerRunsOlderThan(ctx, 15); err != nil || n != 1 {
		t.Fatalf("DeleteTriggerRunsOlderThan = %d, %v; want 1", n, err)
	}
}

// TestListEnabledTriggersJoinsAccounts covers the scheduler's single query: only
// enabled configs, each with its owner's calendar account (nil when none).
func TestListEnabledTriggersJoinsAccounts(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	withCal := seedUser(t, s, "u1", "alice", false)
	noCal := seedUser(t, s, "u2", "bob", false)
	disabled := seedUser(t, s, "u3", "carol", false)

	for _, u := range []*User{withCal, noCal, disabled} {
		tr := &CalendarTrigger{UserID: u.ID, Enabled: u != disabled, Model: "m", UpdatedAt: 1}
		if err := s.UpsertCalendarTrigger(ctx, tr); err != nil {
			t.Fatalf("UpsertCalendarTrigger(%s): %v", u.Username, err)
		}
	}
	if err := s.UpsertCalendarAccount(ctx, &CalendarAccount{
		ID: "cal-1", UserID: withCal.ID, Kind: "caldav", CalendarID: "/c/",
		Username: "me", CredentialEnc: "enc", Endpoint: "https://cal.example", HomeSet: "/h/",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertCalendarAccount: %v", err)
	}

	rows, err := s.ListEnabledTriggers(ctx)
	if err != nil {
		t.Fatalf("ListEnabledTriggers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two enabled configs", rows)
	}
	byUser := map[string]CalendarTrigger{}
	for _, r := range rows {
		byUser[r.UserID] = r
	}
	if _, ok := byUser[disabled.ID]; ok {
		t.Error("disabled config was returned")
	}
	if r := byUser[withCal.ID]; r.Account == nil || r.Account.Kind != "caldav" ||
		r.Account.CalendarID != "/c/" || r.Account.CredentialEnc != "enc" {
		t.Errorf("joined account = %+v", r.Account)
	}
	if r := byUser[noCal.ID]; r.Account != nil {
		t.Errorf("account for a user without one = %+v", r.Account)
	}
}
