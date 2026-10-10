package server

import (
	"context"
	"strings"

	"myogchat/internal/tools"
)

// Settings keys for the maps feature (Phase 3.4).
const (
	settingMapsProvider = "maps_provider"
	settingMapsRegion   = "maps_region"
	settingMapsUnits    = "maps_units"
)

// GeoDefaults holds the env-derived maps configuration. DB settings
// (maps_provider, maps_region, maps_units) override these per request.
type GeoDefaults struct {
	Provider string // auto | osm (only OSM is supported; keyless)
	Region   string // country code (ISO 3166-1 alpha-2), optional
	Units    string // metric | imperial
	Limit    int
}

// geoConfig resolves the effective maps configuration: env defaults overlaid
// with DB settings, plus the SQLite-backed result cache. The cache is a policy
// requirement (OSM mandates caching); a nil store (some tests) disables it.
func (s *Server) geoConfig(ctx context.Context) tools.GeoConfig {
	cfg := tools.GeoConfig{
		Provider: s.geo.Provider,
		Region:   s.geo.Region,
		Units:    s.geo.Units,
		Limit:    s.geo.Limit,
	}
	if v, ok, err := s.st.GetSetting(ctx, settingMapsProvider); err == nil && ok && strings.TrimSpace(v) != "" {
		cfg.Provider = v
	}
	if v, ok, err := s.st.GetSetting(ctx, settingMapsRegion); err == nil && ok && strings.TrimSpace(v) != "" {
		cfg.Region = v
	}
	if v, ok, err := s.st.GetSetting(ctx, settingMapsUnits); err == nil && ok && strings.TrimSpace(v) != "" {
		cfg.Units = v
	}
	if s.st != nil {
		// *store.Store implements tools.GeoCache (GetGeoCache/PutGeoCache).
		cfg.Cache = s.st
	}
	return cfg
}

// mapsSettings is the self-contained admin settings block for maps (3.4). It is
// merged into the /api/settings response so the SPA block can be relocated.
func (s *Server) mapsSettings(ctx context.Context) map[string]any {
	cfg := s.geoConfig(ctx)
	return map[string]any{
		"maps_provider": tools.NormalizeGeoProvider(cfg.Provider),
		"maps_region":   cfg.Region,
		"maps_units":    tools.NormalizeGeoUnits(cfg.Units),
		"maps_active":   tools.GeoActive(cfg),
	}
}

// setMapsProvider persists the provider selector (only "osm" today).
func (s *Server) setMapsProvider(ctx context.Context, provider string) error {
	return s.st.SetSetting(ctx, settingMapsProvider, tools.NormalizeGeoProvider(provider))
}

// setMapsRegion persists the optional country-code bias ("" clears it).
func (s *Server) setMapsRegion(ctx context.Context, region string) error {
	return s.st.SetSetting(ctx, settingMapsRegion, strings.TrimSpace(region))
}

// setMapsUnits persists the unit system (normalized to metric|imperial).
func (s *Server) setMapsUnits(ctx context.Context, units string) error {
	return s.st.SetSetting(ctx, settingMapsUnits, tools.NormalizeGeoUnits(units))
}
