package models

import (
	"testing"
	"time"
)

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// 2026-10-08 is a Thursday; 2026-10-09 Friday, 10 Saturday, 11 Sunday, 12 Monday.
func TestIsPeak(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"fri 00:59 before window", utc(2026, time.October, 9, 0, 59), false},
		{"fri 01:00 window start", utc(2026, time.October, 9, 1, 0), true},
		{"fri 03:59 in window", utc(2026, time.October, 9, 3, 59), true},
		{"fri 04:00 window end", utc(2026, time.October, 9, 4, 0), false},
		{"fri 05:30 between windows", utc(2026, time.October, 9, 5, 30), false},
		{"fri 06:00 window2 start", utc(2026, time.October, 9, 6, 0), true},
		{"fri 09:59 in window2", utc(2026, time.October, 9, 9, 59), true},
		{"fri 10:00 window2 end", utc(2026, time.October, 9, 10, 0), false},
		{"fri afternoon", utc(2026, time.October, 9, 14, 0), false},
		{"sat peak hours", utc(2026, time.October, 10, 2, 0), false},
		{"sun peak hours", utc(2026, time.October, 11, 7, 0), false},
		{"mon window2", utc(2026, time.October, 12, 6, 30), true},
		{"thu window1", utc(2026, time.October, 8, 2, 30), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsPeak(c.at); got != c.want {
				t.Errorf("IsPeak(%v) = %v, want %v", c.at, got, c.want)
			}
		})
	}
}

func TestCostMicros(t *testing.T) {
	mustModel := func(id string) Model {
		m, ok := ByID(id)
		if !ok {
			t.Fatalf("model %q not in registry", id)
		}
		return m
	}
	offPeak := utc(2026, time.October, 8, 12, 0) // Thursday noon
	peak := utc(2026, time.October, 8, 2, 30)    // Thursday 02:30

	cases := []struct {
		name       string
		model      Model
		prompt     int64
		completion int64
		cached     int64
		at         time.Time
		want       int64
	}{
		{"free model is always zero", mustModel("beam-free"), 100000, 50000, 0, offPeak, 0},
		// 1000·0.95 + 1000·4.00 = 950 + 4000
		{"simple input+output", mustModel("kimi-k2.7-code"), 1000, 1000, 0, offPeak, 4950},
		// 600·0.95 + 400·0.19 + 100·4.00 = 570 + 76 + 400
		{"cached tokens billed at cache rate", mustModel("kimi-k2.7-code"), 1000, 100, 400, offPeak, 1046},
		// 272000·0.10 + 100·0.50 = 27200 + 50 (threshold not exceeded)
		{"tier threshold not exceeded at exactly 272000", mustModel("gpt-6-luna"), 272000, 100, 0, offPeak, 27250},
		// 272001·0.20 + 100·0.75 = 54400.2 + 75 = 54475.2 → 54475
		{"tier rates apply above threshold", mustModel("gpt-6-luna"), 272001, 100, 0, offPeak, 54475},
		// 10000·0.15 + 1000·0.60 = 1500 + 600
		{"deepseek off-peak base rates", mustModel("deepseek-v4.1-flash"), 10000, 1000, 0, offPeak, 2100},
		// 10000·0.30 + 1000·1.20 = 3000 + 1200
		{"deepseek peak rates", mustModel("deepseek-v4.1-flash"), 10000, 1000, 0, peak, 4200},
		// 4000·1.32 + 1000·0.044 + 500·3.96 = 5280 + 44 + 1980
		{"deepseek pro peak with cache", mustModel("deepseek-v4-pro"), 5000, 500, 1000, peak, 7304},
		// 1·2.501 = 2.501 → 3
		{"rounds to nearest micro", mustModel("hy4-preview"), 0, 1, 0, offPeak, 3},
		// cached > prompt: billable clamped to 0 → 10·0.19 = 1.9 → 2
		{"billable prompt clamped at zero", mustModel("kimi-k2.7-code"), 10, 0, 20, offPeak, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CostMicros(c.model, c.prompt, c.completion, c.cached, c.at); got != c.want {
				t.Errorf("CostMicros(%s, %d, %d, %d) = %d, want %d",
					c.model.ID, c.prompt, c.completion, c.cached, got, c.want)
			}
		})
	}
}

func TestRegistryShape(t *testing.T) {
	if len(Registry) != 19 {
		t.Errorf("registry has %d models, want 19", len(Registry))
	}
	seen := make(map[string]bool, len(Registry))
	for _, m := range Registry {
		if seen[m.ID] {
			t.Errorf("duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
		if m.Dialect == DialectResponses && !m.Effort {
			t.Errorf("responses model %q must have Effort=true", m.ID)
		}
		if m.Dialect == DialectChat && m.Effort {
			t.Errorf("chat model %q must have Effort=false", m.ID)
		}
	}
	// Sorted must be ascending by output_per_m.
	s := Sorted()
	for i := 1; i < len(s); i++ {
		if s[i-1].OutputPerM > s[i].OutputPerM {
			t.Errorf("Sorted() not ascending at %q (%v) > %q (%v)",
				s[i-1].ID, s[i-1].OutputPerM, s[i].ID, s[i].OutputPerM)
		}
	}
}

// Retired IDs must stay resolvable (existing conversations reference them) but
// must never be offered for new chats.
func TestLegacyModels(t *testing.T) {
	offered := make(map[string]bool, len(Sorted()))
	for _, m := range Sorted() {
		offered[m.ID] = true
	}
	for _, id := range []string{"longcat-2.5-preview-free", "deepseek-v4-flash", "grok-4.6"} {
		if _, ok := ByID(id); !ok {
			t.Errorf("legacy model %q should still resolve", id)
		}
		if offered[id] {
			t.Errorf("legacy model %q must not appear in Sorted()", id)
		}
	}
}
