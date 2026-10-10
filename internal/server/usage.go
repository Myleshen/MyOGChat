package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/store"
)

// Usage & quota accounting (2.11).
//
// Usage is an aggregate over assistant `messages` joined to `conversations` for
// owner scoping — no new table. Quotas are optional admin settings (0 = off)
// with optional per-user overrides on `users`; enforcement fails open.

// DB-backed global quota limits (0 = off).
const (
	settingDailyTokenLimit  = "daily_token_limit"
	settingMonthlyCostLimit = "monthly_cost_limit"
)

// handleMyUsage returns the caller's usage totals plus their quota status.
func (s *Server) handleMyUsage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cutoff, label := usageCutoff(r.URL.Query().Get("range"), time.Now())

	totals, err := s.st.UserUsageTotals(ctx, u.ID, cutoff)
	if err != nil {
		s.log.Error("usage totals", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load usage")
		return
	}
	byDay, err := s.st.UserUsageByDay(ctx, u.ID, cutoff)
	if err != nil {
		s.log.Error("usage by day", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load usage")
		return
	}
	byModel, err := s.st.UserUsageByModel(ctx, u.ID, cutoff)
	if err != nil {
		s.log.Error("usage by model", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load usage")
		return
	}

	daily, monthly := s.effectiveLimits(ctx, u)
	quota := map[string]any{
		"daily_token_limit":  daily,
		"monthly_cost_limit": monthly,
		"today_tokens":       int64(0),
		"month_cost_micros":  int64(0),
		"daily_exceeded":     false,
		"monthly_exceeded":   false,
	}
	now := time.Now()
	if daily > 0 {
		if t, err := s.st.UserUsageTotals(ctx, u.ID, startOfUTCDay(now)); err != nil {
			s.log.Warn("usage quota today", "error", err)
		} else {
			today := t.PromptTokens + t.CompletionTokens
			quota["today_tokens"] = today
			quota["daily_exceeded"] = today >= daily
		}
	}
	if monthly > 0 {
		if t, err := s.st.UserUsageTotals(ctx, u.ID, startOfUTCMonth(now)); err != nil {
			s.log.Warn("usage quota month", "error", err)
		} else {
			quota["month_cost_micros"] = t.CostMicros
			quota["monthly_exceeded"] = t.CostMicros >= monthly
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"range":    label,
		"totals":   totals,
		"by_day":   byDay,
		"by_model": byModel,
		"quota":    quota,
	})
}

// handleUsersUsage returns every user's usage totals (admin only).
func (s *Server) handleUsersUsage(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	if !auth.RequireAdmin(w, u) {
		return
	}
	cutoff, label := usageCutoff(r.URL.Query().Get("range"), time.Now())
	users, err := s.st.AllUsersUsage(r.Context(), cutoff)
	if err != nil {
		s.log.Error("all users usage", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load usage")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": label, "users": users})
}

// usageCutoff maps ?range=7d|30d|all to a unix-ms cutoff (0 = all time) and a
// normalized label. Unknown/absent ranges default to 30d.
func usageCutoff(rangeParam string, now time.Time) (cutoff int64, label string) {
	switch rangeParam {
	case "7d":
		return startOfUTCDay(now.AddDate(0, 0, -6)), "7d"
	case "all":
		return 0, "all"
	default:
		return startOfUTCDay(now.AddDate(0, 0, -29)), "30d"
	}
}

// startOfUTCDay returns the unix-ms instant at 00:00 UTC of t's day.
func startOfUTCDay(t time.Time) int64 {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).UnixMilli()
}

// startOfUTCMonth returns the unix-ms instant at 00:00 UTC on the 1st of t's
// UTC month.
func startOfUTCMonth(t time.Time) int64 {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
}

// effectiveLimits prefers a per-user override over the global admin setting.
// A limit of 0 means "off".
func (s *Server) effectiveLimits(ctx context.Context, u store.User) (dailyTokens, monthlyCostMicros int64) {
	dailyTokens, monthlyCostMicros = s.usageLimits(ctx)
	if u.DailyTokenLimit != nil {
		dailyTokens = *u.DailyTokenLimit
	}
	if u.MonthlyCostLimit != nil {
		monthlyCostMicros = *u.MonthlyCostLimit
	}
	return dailyTokens, monthlyCostMicros
}

// checkQuota returns a non-nil error when the user has hit a configured quota.
// It is cheap: queries run only when a limit is set, using the start of the UTC
// day/month. It fails open on a DB error so a transient fault cannot lock users
// out.
func (s *Server) checkQuota(ctx context.Context, u store.User) error {
	daily, monthly := s.effectiveLimits(ctx, u)
	if daily <= 0 && monthly <= 0 {
		return nil
	}
	now := time.Now()
	if daily > 0 {
		t, err := s.st.UserUsageTotals(ctx, u.ID, startOfUTCDay(now))
		if err != nil {
			s.log.Warn("quota check failed, allowing (daily)", "user", u.ID, "error", err)
		} else if t.PromptTokens+t.CompletionTokens >= daily {
			return fmt.Errorf("daily token limit reached (%d tokens); resets at 00:00 UTC", daily)
		}
	}
	if monthly > 0 {
		t, err := s.st.UserUsageTotals(ctx, u.ID, startOfUTCMonth(now))
		if err != nil {
			s.log.Warn("quota check failed, allowing (monthly)", "user", u.ID, "error", err)
		} else if t.CostMicros >= monthly {
			return fmt.Errorf("monthly cost limit reached ($%.2f); contact an admin", float64(monthly)/1e6)
		}
	}
	return nil
}

// usageLimits reads the global quota settings (0 = off, invalid = off).
func (s *Server) usageLimits(ctx context.Context) (daily, monthly int64) {
	return s.settingInt64(ctx, settingDailyTokenLimit), s.settingInt64(ctx, settingMonthlyCostLimit)
}

// usageSettings is the self-contained admin settings block for quotas (2.11).
// It is merged into /api/settings.
func (s *Server) usageSettings(ctx context.Context) map[string]any {
	daily, monthly := s.usageLimits(ctx)
	return map[string]any{
		"daily_token_limit":  daily,
		"monthly_cost_limit": monthly,
	}
}

// settingInt64 reads a non-negative integer setting; missing/invalid → 0.
func (s *Server) settingInt64(ctx context.Context, key string) int64 {
	v, ok, err := s.st.GetSetting(ctx, key)
	if err != nil || !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// nullableInt64 distinguishes an omitted JSON field from an explicit null so
// PATCH can treat omitted = unchanged, null = clear/inherit, number = override.
type nullableInt64 struct {
	Set   bool
	Value *int64
}

// UnmarshalJSON records presence and parses either null or an integer.
func (n *nullableInt64) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}
