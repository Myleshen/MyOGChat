package models

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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
	if len(Registry) != 28 {
		t.Errorf("registry has %d models, want 28", len(Registry))
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

// retiredIDs are the registry entries the live catalog no longer lists. They
// must stay resolvable (existing conversations reference them) but must never
// be offered for new chats.
var retiredIDs = []string{
	"glm-5.2",
	"longcat-2.5-preview-free", // replaced by the differently-named longcat-2.5-free
	"space-bunny",
	"mimo-v2.5",
	"mimo-v2.5-pro",
	"deepseek-v4-flash",
	"deepseek-v4-flash-vision-exp",
	"kimi-k2.6",
	"grok-4.6",
}

func TestLegacyModels(t *testing.T) {
	catalog := loadCatalog(t)
	offered := make(map[string]bool, len(Sorted()))
	for _, m := range Sorted() {
		offered[m.ID] = true
	}
	for _, id := range retiredIDs {
		if _, ok := ByID(id); !ok {
			t.Errorf("legacy model %q should still resolve", id)
		}
		if offered[id] {
			t.Errorf("legacy model %q must not appear in Sorted()", id)
		}
		if _, live := catalog.model(id); live {
			t.Errorf("legacy model %q is back in the live catalog; restore it to Registry", id)
		}
	}
}

// notRegistered are live catalog models the app deliberately does not offer.
// Every catalog id must be either in Registry or listed here, so that a
// refreshed snapshot cannot silently drop a new model.
var notRegistered = []string{
	// go.training=true: the provider trains on prompts (repo policy).
	"iceman-contributor-free",
	"muse-spark-1.2-contributor",
	"muse-spark-1.3-contributor",
}

// catalog is a trimmed snapshot of GET .../inference/go/v1/models.
type catalog struct {
	Source  string         `json:"source"`
	Fetched string         `json:"fetched"`
	Count   int            `json:"count"`
	Data    []catalogModel `json:"data"`
}

type catalogModel struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	RecommendedAPI   string   `json:"recommended_api"`
	InputModalities  []string `json:"input_modalities"`
	ReasoningSupport bool     `json:"reasoning_supported"`
	Context          int64    `json:"context"`
	Training         bool     `json:"training"`
	API              map[string]struct {
		ReasoningField *string `json:"reasoning_field"`
	} `json:"api"`
	Cost struct {
		Text struct {
			Input  catalogPrice `json:"input"`
			Output catalogPrice `json:"output"`
		} `json:"text"`
		Multipliers []struct {
			When struct {
				// A date-gated multiplier is a scheduled price change, not peak hours.
				Date struct {
					GTE string `json:"gte"`
				} `json:"date"`
			} `json:"when"`
			Factor float64 `json:"factor"`
		} `json:"multipliers"`
	} `json:"cost"`
}

type catalogPrice struct {
	Price float64 `json:"price"`
	Cache struct {
		Read float64 `json:"read"`
	} `json:"cache"`
	Overrides []struct {
		When struct {
			Context struct {
				GT int64 `json:"gt"`
			} `json:"context"`
		} `json:"when"`
		Price float64 `json:"price"`
		Cache struct {
			Read float64 `json:"read"`
		} `json:"cache"`
	} `json:"overrides"`
}

func loadCatalog(t *testing.T) catalog {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "catalog.json"))
	if err != nil {
		t.Fatalf("read catalog fixture: %v", err)
	}
	var c catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse catalog fixture: %v", err)
	}
	return c
}

func (c catalog) model(id string) (catalogModel, bool) {
	for _, m := range c.Data {
		if m.ID == id {
			return m, true
		}
	}
	return catalogModel{}, false
}

func (m catalogModel) hasImageInput() bool {
	for _, mod := range m.InputModalities {
		if len(mod) >= 6 && mod[:6] == "image/" {
			return true
		}
	}
	return false
}

// visibleReasoning mirrors the registry doc: chat models surface reasoning only
// when the catalog declares a reasoning_field; responses and Google models
// expose a reasoning control whenever reasoning is supported.
func (m catalogModel) visibleReasoning(dialect string) bool {
	if dialect == DialectResponses || dialect == DialectGoogle {
		return m.ReasoningSupport
	}
	for _, api := range m.API {
		if api.ReasoningField != nil && *api.ReasoningField != "" {
			return true
		}
	}
	return false
}

// dialectFor maps a catalog recommended_api to our dialect constant, and false
// for an API this app has no adapter for.
func dialectFor(api string) (string, bool) {
	switch api {
	case "chat-completions":
		return DialectChat, true
	case "responses":
		return DialectResponses, true
	case "messages":
		return DialectAnthropic, true
	case "generate-content", "interactions":
		return DialectGoogle, true
	}
	return "", false
}

// TestCatalogFixtureFresh checks the snapshot records where it came from and
// is self-consistent (so a stale or hand-edited fixture is obvious).
func TestCatalogFixtureFresh(t *testing.T) {
	c := loadCatalog(t)
	if c.Source != "https://opencode.ai/inference/go/openai/v1/models" {
		t.Errorf("fixture source = %q", c.Source)
	}
	if _, err := time.Parse("2006-01-02", c.Fetched); err != nil {
		t.Errorf("fixture fetched date %q: %v", c.Fetched, err)
	}
	if c.Count != len(c.Data) || c.Count == 0 {
		t.Errorf("fixture count = %d, models = %d", c.Count, len(c.Data))
	}
}

// TestRegistryMatchesCatalog is the drift guard: every price, cache price,
// tier, peak factor, vision flag, thinking flag and dialect must come straight
// from the live catalog.
func TestRegistryMatchesCatalog(t *testing.T) {
	c := loadCatalog(t)
	eq := func(id, field string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: %s = %v, catalog has %v", id, field, got, want)
		}
	}
	for _, m := range Registry {
		cm, ok := c.model(m.ID)
		if !ok {
			t.Errorf("registered model %q is not in the live catalog", m.ID)
			continue
		}
		if cm.Training {
			t.Errorf("registered model %q has training:true in the catalog", m.ID)
		}

		dialect, known := dialectFor(cm.RecommendedAPI)
		if !known {
			t.Errorf("%s: catalog recommends %q, which has no adapter", m.ID, cm.RecommendedAPI)
			continue
		}
		if m.Dialect != dialect {
			t.Errorf("%s: dialect = %q, catalog says %q", m.ID, m.Dialect, dialect)
		}
		if m.Vision != cm.hasImageInput() {
			t.Errorf("%s: vision = %v, catalog says %v", m.ID, m.Vision, cm.hasImageInput())
		}
		if want := cm.visibleReasoning(dialect); m.Thinking != want {
			t.Errorf("%s: thinking = %v, catalog says %v", m.ID, m.Thinking, want)
		}

		eq(m.ID, "input_per_m", m.InputPerM, cm.Cost.Text.Input.Price)
		eq(m.ID, "output_per_m", m.OutputPerM, cm.Cost.Text.Output.Price)
		eq(m.ID, "cache_read_per_m", m.CacheReadPerM, cm.Cost.Text.Input.Cache.Read)

		if ov := cm.Cost.Text.Input.Overrides; len(ov) > 0 {
			eq(m.ID, "tier_threshold", float64(m.TierThreshold), float64(ov[0].When.Context.GT))
			eq(m.ID, "tier_input_per_m", m.TierInputPerM, ov[0].Price)
			eq(m.ID, "tier_cache_per_m", m.TierCachePerM, ov[0].Cache.Read)
			out := cm.Cost.Text.Output.Overrides
			if len(out) == 0 {
				t.Errorf("%s: catalog has an input tier but no output tier", m.ID)
			} else {
				eq(m.ID, "tier_output_per_m", m.TierOutputPerM, out[0].Price)
			}
		} else if m.TierThreshold != 0 || m.TierInputPerM != 0 || m.TierOutputPerM != 0 || m.TierCachePerM != 0 {
			t.Errorf("%s: registry has tier pricing, catalog has no override", m.ID)
		}

		peak := false
		for _, mult := range cm.Cost.Multipliers {
			if mult.Factor == 2 && mult.When.Date.GTE == "" {
				peak = true
			}
		}
		if m.HasPeak != peak {
			t.Errorf("%s: HasPeak = %v, catalog multiplier says %v", m.ID, m.HasPeak, peak)
		}
		if peak {
			eq(m.ID, "peak_input_per_m", m.PeakInputPerM, 2*m.InputPerM)
			eq(m.ID, "peak_output_per_m", m.PeakOutputPerM, 2*m.OutputPerM)
			eq(m.ID, "peak_cache_per_m", m.PeakCachePerM, 2*m.CacheReadPerM)
		}
	}
}

// TestCatalogCovered fails when a refreshed snapshot adds a model that nobody
// decided about, or when an ignored id is actually selectable.
func TestCatalogCovered(t *testing.T) {
	c := loadCatalog(t)
	registered := make(map[string]bool, len(Registry))
	for _, m := range Registry {
		registered[m.ID] = true
	}
	ignored := make(map[string]bool, len(notRegistered))
	for _, id := range notRegistered {
		if ignored[id] {
			t.Errorf("%q listed twice in notRegistered", id)
		}
		ignored[id] = true
		if registered[id] {
			t.Errorf("%q is both registered and in notRegistered", id)
		}
		if _, ok := c.model(id); !ok {
			t.Errorf("%q is in notRegistered but not in the catalog fixture", id)
		}
		if _, ok := ByID(id); ok {
			t.Errorf("%q must not resolve to a model", id)
		}
	}
	for _, cm := range c.Data {
		if !registered[cm.ID] && !ignored[cm.ID] {
			t.Errorf("catalog model %q is neither registered nor documented in notRegistered", cm.ID)
		}
	}
}

// TestNoTrainingModelSelectable enforces the policy that prompt-training
// models are never offered.
func TestNoTrainingModelSelectable(t *testing.T) {
	c := loadCatalog(t)
	offered := make(map[string]bool, len(Sorted()))
	for _, m := range Sorted() {
		offered[m.ID] = true
	}
	training := 0
	for _, cm := range c.Data {
		if !cm.Training {
			continue
		}
		training++
		if offered[cm.ID] {
			t.Errorf("training model %q is selectable", cm.ID)
		}
		if _, ok := ByID(cm.ID); ok {
			t.Errorf("training model %q resolves to a registry entry", cm.ID)
		}
	}
	if training == 0 {
		t.Error("fixture has no training:true models; the policy check is vacuous")
	}
}
