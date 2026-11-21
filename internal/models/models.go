// Package models holds the static model registry and the cost calculation
// described in contract §5.
package models

import (
	"math"
	"sort"
	"time"
)

// Dialect identifies the upstream API dialect a model speaks.
const (
	DialectChat      = "chat"
	DialectResponses = "responses"
	DialectAnthropic = "anthropic"
	DialectGoogle    = "google"
)

// Model describes one upstream LLM. Tier thresholds and peak-pricing
// internals are intentionally not exposed via JSON (contract §3).
type Model struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Dialect       string  `json:"dialect"`
	Effort        bool    `json:"effort"`
	Thinking      bool    `json:"thinking"`
	Vision        bool    `json:"vision"`
	InputPerM     float64 `json:"input_per_m"`
	OutputPerM    float64 `json:"output_per_m"`
	CacheReadPerM float64 `json:"cache_read_per_m"`
	// Provider is the slug of a user-added custom provider (§22); empty for
	// registry models.
	Provider string `json:"provider,omitempty"`
	// CostTracked is false for custom-provider models, whose prices are unknown.
	// It is set by the server when the model list is built; registry models are
	// tracked.
	CostTracked bool `json:"cost_tracked"`

	// Tier pricing applies when prompt_tokens exceed TierThreshold.
	TierThreshold  int64   `json:"-"`
	TierInputPerM  float64 `json:"-"`
	TierOutputPerM float64 `json:"-"`
	TierCachePerM  float64 `json:"-"`

	// Peak pricing (DeepSeek only) applies during UTC peak hours Mon–Fri.
	HasPeak        bool    `json:"-"`
	PeakInputPerM  float64 `json:"-"`
	PeakOutputPerM float64 `json:"-"`
	PeakCachePerM  float64 `json:"-"`
}

// Registry is the static model list from contract §5. Exactly these models —
// the Go catalog over all four dialects (chat completions, responses, Anthropic
// messages and Google generate-content) as of 2026-11-21.
//
// iceman-contributor-free, muse-spark-1.2-contributor and
// muse-spark-1.3-contributor are excluded outright: their catalog entry has
// go.training=true, i.e. the provider trains on prompts.
//
// Thinking marks models that surface visible reasoning: a chat model whose
// catalog entry declares a reasoning_field (otherwise reasoning stays hidden
// upstream), or a responses/Google model with reasoning.supported (both expose
// a reasoning control) — see contract §8.7. Anthropic models stay unmarked
// because the adapter sends no thinking parameter.
//
// Gemini's catalog multiplier doubles prices from 2026-12-01 (a scheduled
// change, not peak hours); it is not modelled in CostMicros, so its rows carry
// no peak rates.
var Registry = []Model{
	{ID: "beam-free", Name: "Beam Free", Dialect: DialectChat, Thinking: true},
	{ID: "longcat-2.5-free", Name: "LongCat 2.5 Free", Dialect: DialectChat, Thinking: true},
	{ID: "hy4-flash-free", Name: "Hy4 Flash Free", Dialect: DialectChat, Vision: true},
	{ID: "mimo-v2.6-flash", Name: "MiMo V2.6 Flash", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.14, OutputPerM: 0.28, CacheReadPerM: 0.0028},
	{ID: "glm-5.3-flash", Name: "GLM-5.3 Flash", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.15, OutputPerM: 0.50, CacheReadPerM: 0.03},
	{ID: "hy3", Name: "Hy3", Dialect: DialectChat, InputPerM: 0.14, OutputPerM: 0.58, CacheReadPerM: 0.035},
	{ID: "minimax-m3.1-flash", Name: "MiniMax M3.1 Flash", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.15, OutputPerM: 0.60, CacheReadPerM: 0.015},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.15, OutputPerM: 0.60, CacheReadPerM: 0.003, HasPeak: true, PeakInputPerM: 0.30, PeakOutputPerM: 1.20, PeakCachePerM: 0.006},
	{ID: "mimo-v2.6-pro", Name: "MiMo V2.6 Pro", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.435, OutputPerM: 0.87, CacheReadPerM: 0.003625},
	{ID: "longcat-2.0", Name: "LongCat 2.0", Dialect: DialectChat, Thinking: true, InputPerM: 0.30, OutputPerM: 1.20, CacheReadPerM: 0.006},
	{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Dialect: DialectChat, Thinking: true, InputPerM: 0.66, OutputPerM: 1.98, CacheReadPerM: 0.022, HasPeak: true, PeakInputPerM: 1.32, PeakOutputPerM: 3.96, PeakCachePerM: 0.044},
	{ID: "hy4-preview", Name: "Hy4 Preview", Dialect: DialectChat, InputPerM: 0.834, OutputPerM: 2.501, CacheReadPerM: 0.042},
	{ID: "hy4", Name: "Hy4", Dialect: DialectChat, Vision: true, InputPerM: 0.834, OutputPerM: 2.501, CacheReadPerM: 0.042},
	{ID: "step-5-preview", Name: "Step 5 Preview", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 1.00, OutputPerM: 2.70, CacheReadPerM: 0.10},
	{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.95, OutputPerM: 4.00, CacheReadPerM: 0.19},
	{ID: "glm-5.3", Name: "GLM-5.3", Dialect: DialectChat, Thinking: true, InputPerM: 1.40, OutputPerM: 4.40, CacheReadPerM: 0.26},
	{ID: "kimi-k3.1-pro", Name: "Kimi K3.1 Pro", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 2.00, OutputPerM: 10.00, CacheReadPerM: 0.20},
	{ID: "kimi-k3", Name: "Kimi K3", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 3.00, OutputPerM: 15.00, CacheReadPerM: 0.30},
	{ID: "gpt-6-luna", Name: "GPT 6 Luna", Dialect: DialectResponses, Effort: true, Thinking: true, Vision: true, InputPerM: 0.10, OutputPerM: 0.50, CacheReadPerM: 0.01, TierThreshold: 272000, TierInputPerM: 0.20, TierOutputPerM: 0.75, TierCachePerM: 0.02},
	{ID: "gpt-5.6-luna", Name: "GPT 5.6 Luna", Dialect: DialectResponses, Effort: true, Thinking: true, Vision: true, InputPerM: 0.20, OutputPerM: 1.20, CacheReadPerM: 0.02, TierThreshold: 272000, TierInputPerM: 0.40, TierOutputPerM: 1.80, TierCachePerM: 0.04},
	{ID: "grok-4.7", Name: "Grok 4.7", Dialect: DialectResponses, Effort: true, Thinking: true, Vision: true, InputPerM: 2.00, OutputPerM: 6.00, CacheReadPerM: 0.50, TierThreshold: 200000, TierInputPerM: 4.00, TierOutputPerM: 12.00, TierCachePerM: 1.00},

	// Anthropic Messages dialect (§4). No thinking parameter is sent, so
	// Thinking stays false until the adapter controls a reasoning budget.
	{ID: "qwen3.8-flash", Name: "Qwen3.8 Flash", Dialect: DialectAnthropic, Vision: true, InputPerM: 0.15, OutputPerM: 0.47, CacheReadPerM: 0.016},
	{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5", Dialect: DialectAnthropic, Vision: true, InputPerM: 0.10, OutputPerM: 0.50, CacheReadPerM: 0.01, TierThreshold: 100000, TierInputPerM: 0.50, TierOutputPerM: 2.50, TierCachePerM: 0.05},
	{ID: "minimax-m3", Name: "MiniMax M3", Dialect: DialectAnthropic, Vision: true, InputPerM: 0.30, OutputPerM: 1.20, CacheReadPerM: 0.06, TierThreshold: 512000, TierInputPerM: 0.60, TierOutputPerM: 2.40, TierCachePerM: 0.12},
	{ID: "qwen3.7-plus", Name: "Qwen3.7 Plus", Dialect: DialectAnthropic, Vision: true, InputPerM: 0.40, OutputPerM: 1.60, CacheReadPerM: 0.04, TierThreshold: 256000, TierInputPerM: 1.20, TierOutputPerM: 4.80, TierCachePerM: 0.12},
	{ID: "qwen4-max", Name: "Qwen4 Max", Dialect: DialectAnthropic, Vision: true, InputPerM: 1.50, OutputPerM: 4.50, CacheReadPerM: 0.075},
	{ID: "qwen3.8-max", Name: "Qwen3.8 Max", Dialect: DialectAnthropic, Vision: true, InputPerM: 2.00, OutputPerM: 6.00, CacheReadPerM: 0.25},

	// Google generate-content dialect (§4).
	{ID: "gemini-4-argon", Name: "Gemini 4 Argon", Dialect: DialectGoogle, Thinking: true, Vision: true, InputPerM: 2.00, OutputPerM: 10.00, CacheReadPerM: 0.10},
}

// legacy keeps retired model IDs resolvable so existing conversations still
// load (and can be switched to a current model) instead of failing with
// "unknown model". Retired IDs never appear in Sorted(), so they are not
// offered to new chats.
var legacy = []Model{
	{ID: "longcat-2.5-preview-free", Name: "LongCat 2.5 Preview Free", Dialect: DialectChat, Thinking: true, Vision: true},
	{ID: "space-bunny", Name: "Space Bunny", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.15, OutputPerM: 0.60, CacheReadPerM: 0.03},
	{ID: "mimo-v2.5", Name: "MiMo V2.5", Dialect: DialectChat, Vision: true, InputPerM: 0.14, OutputPerM: 0.28, CacheReadPerM: 0.0028},
	{ID: "mimo-v2.5-pro", Name: "MiMo V2.5 Pro", Dialect: DialectChat, InputPerM: 0.435, OutputPerM: 0.87, CacheReadPerM: 0.003625},
	{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", Dialect: DialectChat, Thinking: true, InputPerM: 0.15, OutputPerM: 0.60, CacheReadPerM: 0.003, HasPeak: true, PeakInputPerM: 0.30, PeakOutputPerM: 1.20, PeakCachePerM: 0.006},
	{ID: "deepseek-v4-flash-vision-exp", Name: "DeepSeek V4 Flash Vision", Dialect: DialectChat, Thinking: true, Vision: true, InputPerM: 0.15, OutputPerM: 0.60, CacheReadPerM: 0.003, HasPeak: true, PeakInputPerM: 0.30, PeakOutputPerM: 1.20, PeakCachePerM: 0.006},
	{ID: "kimi-k2.6", Name: "Kimi K2.6", Dialect: DialectChat, Vision: true, InputPerM: 0.95, OutputPerM: 4.00, CacheReadPerM: 0.16},
	{ID: "glm-5.2", Name: "GLM-5.2", Dialect: DialectChat, Thinking: true, InputPerM: 1.40, OutputPerM: 4.40, CacheReadPerM: 0.26},
	{ID: "grok-4.6", Name: "Grok 4.6", Dialect: DialectResponses, Effort: true, Thinking: true, Vision: true, InputPerM: 2.00, OutputPerM: 6.00, CacheReadPerM: 0.50, TierThreshold: 200000, TierInputPerM: 4.00, TierOutputPerM: 12.00, TierCachePerM: 1.00},
}

var byID = func() map[string]Model {
	m := make(map[string]Model, len(Registry)+len(legacy))
	for _, model := range Registry {
		m[model.ID] = model
	}
	for _, model := range legacy {
		m[model.ID] = model
	}
	return m
}()

// ByID returns the registry entry for id, or false if unknown.
func ByID(id string) (Model, bool) {
	m, ok := byID[id]
	return m, ok
}

// Sorted returns the registry sorted by output_per_m ascending (contract §3).
// Every entry is cost-tracked: its prices come from the live catalog.
func Sorted() []Model {
	out := make([]Model, len(Registry))
	copy(out, Registry)
	for i := range out {
		out[i].CostTracked = true
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].OutputPerM < out[j].OutputPerM
	})
	return out
}

// ValidEffort reports whether e is an allowed effort value (contract §2).
func ValidEffort(e string) bool {
	switch e {
	case "none", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// IsPeak reports whether now falls inside the DeepSeek peak window:
// 01:00–04:00 and 06:00–10:00 UTC, Monday–Friday.
func IsPeak(now time.Time) bool {
	now = now.UTC()
	if w := now.Weekday(); w == time.Saturday || w == time.Sunday {
		return false
	}
	h := now.Hour()
	return (h >= 1 && h < 4) || (h >= 6 && h < 10)
}

// CostMicros computes the integer micro-dollar cost of a completion:
//
//	cost_micros = (prompt−cached)·in_rate + cached·cache_rate + completion·out_rate
//
// where the per-token rates are price_per_m (USD per 1M tokens) — numerically
// equal to micro-dollars per token. Tier/peak rate selection happens first.
//
// A custom-provider model (§22) carries no rates because its prices are unknown.
// The arithmetic below then yields 0, which the payload marks as untracked
// (cost_tracked false) rather than presenting a fabricated price.
func CostMicros(m Model, prompt, completion, cached int64, now time.Time) int64 {
	in, out, cache := m.InputPerM, m.OutputPerM, m.CacheReadPerM
	switch {
	case m.HasPeak && IsPeak(now):
		in, out, cache = m.PeakInputPerM, m.PeakOutputPerM, m.PeakCachePerM
	case m.TierThreshold > 0 && prompt > m.TierThreshold:
		in, out, cache = m.TierInputPerM, m.TierOutputPerM, m.TierCachePerM
	}
	if cached < 0 {
		cached = 0
	}
	// cached_tokens is a subset of prompt_tokens; guard against upstream
	// anomalies that report more cached than total prompt tokens.
	if cached > prompt {
		cached = prompt
	}
	billable := prompt - cached
	cost := float64(billable)*in + float64(cached)*cache + float64(completion)*out
	return int64(math.Round(cost))
}
