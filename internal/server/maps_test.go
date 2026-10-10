package server

import (
	"context"
	"testing"

	"myogchat/internal/store"
)

func TestGeoConfigDefaultsAndOverrides(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto"})
	srv.geo = GeoDefaults{Provider: "osm", Region: "", Units: "metric", Limit: 5}
	ctx := context.Background()

	cfg := srv.geoConfig(ctx)
	if cfg.Options().Limit != 5 {
		t.Fatalf("Limit = %d, want 5", cfg.Options().Limit)
	}
	if cfg.Options().Units != "metric" {
		t.Fatalf("Units = %q, want metric", cfg.Options().Units)
	}
	if cfg.Cache == nil {
		t.Fatal("geo cache not wired to the store")
	}

	// DB settings override the env defaults.
	if err := srv.setMapsRegion(ctx, "us"); err != nil {
		t.Fatalf("setMapsRegion: %v", err)
	}
	if err := srv.setMapsUnits(ctx, "imperial"); err != nil {
		t.Fatalf("setMapsUnits: %v", err)
	}
	if err := srv.setMapsProvider(ctx, "google"); err != nil {
		t.Fatalf("setMapsProvider: %v", err)
	}
	cfg = srv.geoConfig(ctx)
	if cfg.Region != "us" {
		t.Errorf("Region = %q, want us", cfg.Region)
	}
	if cfg.Options().Units != "imperial" {
		t.Errorf("Units = %q, want imperial", cfg.Options().Units)
	}
	// An unknown provider normalizes to auto (which still resolves to OSM).
	if got := srv.mapsSettings(ctx)["maps_provider"]; got != "auto" {
		t.Errorf("maps_provider = %v, want auto", got)
	}
}

func TestMapsSettingsFields(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto"})
	srv.geo = GeoDefaults{Provider: "osm", Units: "metric", Limit: 5}
	m := srv.mapsSettings(context.Background())
	if m["maps_provider"] != "osm" {
		t.Errorf("maps_provider = %v", m["maps_provider"])
	}
	if m["maps_units"] != "metric" {
		t.Errorf("maps_units = %v", m["maps_units"])
	}
	if m["maps_active"] != "osm" {
		t.Errorf("maps_active = %v, want osm", m["maps_active"])
	}
	if _, ok := m["maps_region"]; !ok {
		t.Error("maps_region missing")
	}
}

func TestToolRegistryRegistersMaps(t *testing.T) {
	srv := newSearchTestServer(t, SearchDefaults{Provider: "auto"})
	reg := srv.toolRegistry(context.Background(), store.User{})
	for _, name := range []string{"maps_geocode", "maps_reverse", "maps_directions", "maps_places"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s not registered", name)
		}
	}
}
