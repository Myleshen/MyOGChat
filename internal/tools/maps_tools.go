package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// This file defines the four model-facing maps tools (Phase 3.4), reusing the
// 2.4 tool loop. Each delegates to an ordered chain of GeoProviders and formats
// a compact, attributed result. origin/destination/near accept free text or a
// "lat,lon" pair.

// tryGeoProviders walks the provider chain, returning the first successful
// result that isEmpty reports as non-empty. Providers that error or return an
// empty result are skipped (logged); the last error is returned when none
// succeed. Mirrors the web-search fallback chain.
func tryGeoProviders[T any](providers []GeoProvider, isEmpty func(T) bool, fn func(GeoProvider) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for _, p := range providers {
		v, err := fn(p)
		if err != nil {
			slog.Default().Warn("maps provider failed", "provider", p.Name(), "error", err)
			lastErr = err
			continue
		}
		if isEmpty(v) {
			continue
		}
		return v, nil
	}
	if lastErr != nil {
		return zero, lastErr
	}
	return zero, nil
}

// resolvePoint turns a "lat,lon" pair or a free-text place into coordinates,
// geocoding the text through the provider chain when needed.
func resolvePoint(ctx context.Context, providers []GeoProvider, s string, opts GeoOptions) (LatLng, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return LatLng{}, errors.New("a location is required")
	}
	if ll, ok := parseLatLng(s); ok {
		return ll, nil
	}
	places, err := tryGeoProviders(providers,
		func(p []Place) bool { return len(p) == 0 },
		func(p GeoProvider) ([]Place, error) { return p.Geocode(ctx, s, opts) })
	if err != nil {
		return LatLng{}, err
	}
	if len(places) == 0 {
		return LatLng{}, fmt.Errorf("could not find a location for %q", s)
	}
	return LatLng{Lat: places[0].Lat, Lon: places[0].Lon}, nil
}

// ---------- maps_geocode ----------

// GeocodeTool resolves a free-text place name to coordinates.
type GeocodeTool struct {
	providers []GeoProvider
	opts      GeoOptions
}

// NewGeocodeTool wraps providers as a maps_geocode tool.
func NewGeocodeTool(providers []GeoProvider, opts GeoOptions) *GeocodeTool {
	return &GeocodeTool{providers: providers, opts: opts}
}

func (t *GeocodeTool) Name() string { return "maps_geocode" }

func (t *GeocodeTool) Description() string {
	return "Look up the coordinates and address of a place by name (geocoding). " +
		"Use this to turn a city, address, or landmark into latitude/longitude. " +
		"Data © OpenStreetMap contributors (ODbL)."
}

func (t *GeocodeTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The place name or address to look up.",
			},
			"region": map[string]any{
				"type":        "string",
				"description": "Optional country code to bias results, e.g. \"us\".",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of matches (1-20, default 5).",
			},
		},
		"required": []string{"query"},
	})
}

func (t *GeocodeTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query  string `json:"query"`
		Region string `json:"region"`
		Limit  int    `json:"limit"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", errors.New("query is required")
	}
	opts := t.opts
	if strings.TrimSpace(in.Region) != "" {
		opts.Region = strings.TrimSpace(in.Region)
	}
	if in.Limit > 0 {
		opts.Limit = in.Limit
	}
	places, err := tryGeoProviders(t.providers,
		func(p []Place) bool { return len(p) == 0 },
		func(p GeoProvider) ([]Place, error) { return p.Geocode(ctx, query, opts) })
	if err != nil {
		return "", err
	}
	if len(places) == 0 {
		return "No matches found.", nil
	}
	return formatPlaces(places), nil
}

// ---------- maps_reverse ----------

// ReverseTool resolves coordinates to the nearest address.
type ReverseTool struct {
	providers []GeoProvider
	opts      GeoOptions
}

// NewReverseTool wraps providers as a maps_reverse tool.
func NewReverseTool(providers []GeoProvider, opts GeoOptions) *ReverseTool {
	return &ReverseTool{providers: providers, opts: opts}
}

func (t *ReverseTool) Name() string { return "maps_reverse" }

func (t *ReverseTool) Description() string {
	return "Reverse-geocode a latitude/longitude pair into the nearest address. " +
		"Use this when you have coordinates and need the place name. " +
		"Data © OpenStreetMap contributors (ODbL)."
}

func (t *ReverseTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"lat": map[string]any{"type": "number", "description": "Latitude (-90..90)."},
			"lon": map[string]any{"type": "number", "description": "Longitude (-180..180)."},
		},
		"required": []string{"lat", "lon"},
	})
}

func (t *ReverseTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	if !validLatLng(in.Lat, in.Lon) {
		return "", errors.New("valid lat (-90..90) and lon (-180..180) are required")
	}
	place, err := tryGeoProviders(t.providers,
		func(p Place) bool { return p.Name == "" && p.Address == "" },
		func(p GeoProvider) (Place, error) { return p.Reverse(ctx, LatLng{Lat: in.Lat, Lon: in.Lon}, t.opts) })
	if err != nil {
		return "", err
	}
	if place.Name == "" && place.Address == "" {
		return "No address found for those coordinates.", nil
	}
	return formatPlaces([]Place{place}), nil
}

// ---------- maps_directions ----------

// DirectionsTool computes a route between two points.
type DirectionsTool struct {
	providers []GeoProvider
	opts      GeoOptions
}

// NewDirectionsTool wraps providers as a maps_directions tool.
func NewDirectionsTool(providers []GeoProvider, opts GeoOptions) *DirectionsTool {
	return &DirectionsTool{providers: providers, opts: opts}
}

func (t *DirectionsTool) Name() string { return "maps_directions" }

func (t *DirectionsTool) Description() string {
	return "Get directions between two places, given as names or \"lat,lon\". " +
		"Returns distance, estimated duration, and turn-by-turn steps. Modes: " +
		"driving, walking, cycling (transit is not available keylessly). " +
		"Data © OpenStreetMap contributors (ODbL)."
}

func (t *DirectionsTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"origin": map[string]any{
				"type":        "string",
				"description": "Start place name or \"lat,lon\".",
			},
			"destination": map[string]any{
				"type":        "string",
				"description": "End place name or \"lat,lon\".",
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{"driving", "walking", "cycling", "transit"},
				"description": "Travel mode (default driving).",
			},
			"units": map[string]any{
				"type":        "string",
				"enum":        []string{"metric", "imperial"},
				"description": "Distance units (default metric).",
			},
		},
		"required": []string{"origin", "destination"},
	})
}

func (t *DirectionsTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Origin      string `json:"origin"`
		Destination string `json:"destination"`
		Mode        string `json:"mode"`
		Units       string `json:"units"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Origin) == "" || strings.TrimSpace(in.Destination) == "" {
		return "", errors.New("origin and destination are required")
	}
	opts := t.opts
	if strings.TrimSpace(in.Mode) != "" {
		opts.Mode = in.Mode
	}
	if strings.TrimSpace(in.Units) != "" {
		opts.Units = NormalizeGeoUnits(in.Units)
	}
	from, err := resolvePoint(ctx, t.providers, in.Origin, opts)
	if err != nil {
		return "", err
	}
	to, err := resolvePoint(ctx, t.providers, in.Destination, opts)
	if err != nil {
		return "", err
	}
	route, err := tryGeoProviders(t.providers,
		func(r Route) bool { return r.DistanceMeters == 0 && r.DurationSeconds == 0 },
		func(p GeoProvider) (Route, error) { return p.Directions(ctx, from, to, opts) })
	if err != nil {
		return "", err
	}
	if route.DistanceMeters == 0 && route.DurationSeconds == 0 {
		return "No route found.", nil
	}
	return formatRoute(in.Origin, in.Destination, route, opts.Units), nil
}

// ---------- maps_places ----------

// PlacesTool searches for points of interest.
type PlacesTool struct {
	providers []GeoProvider
	opts      GeoOptions
}

// NewPlacesTool wraps providers as a maps_places tool.
func NewPlacesTool(providers []GeoProvider, opts GeoOptions) *PlacesTool {
	return &PlacesTool{providers: providers, opts: opts}
}

func (t *PlacesTool) Name() string { return "maps_places" }

func (t *PlacesTool) Description() string {
	return "Search for points of interest (shops, restaurants, landmarks) by name, " +
		"optionally near a place or \"lat,lon\". Use this to find businesses or " +
		"amenities. Data © OpenStreetMap contributors (ODbL)."
}

func (t *PlacesTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "What to search for, e.g. \"coffee\" or a business name.",
			},
			"near": map[string]any{
				"type":        "string",
				"description": "Optional place name or \"lat,lon\" to search around.",
			},
			"radius_m": map[string]any{
				"type":        "integer",
				"description": "Optional search radius in meters (default 5000).",
			},
			"type": map[string]any{
				"type":        "string",
				"description": "Optional category hint, e.g. \"restaurant\".",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum results (1-20, default 5).",
			},
		},
		"required": []string{"query"},
	})
}

func (t *PlacesTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query   string `json:"query"`
		Near    string `json:"near"`
		RadiusM int    `json:"radius_m"`
		Type    string `json:"type"`
		Limit   int    `json:"limit"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", errors.New("query is required")
	}
	opts := t.opts
	if strings.TrimSpace(in.Near) != "" {
		near, err := resolvePoint(ctx, t.providers, in.Near, opts)
		if err != nil {
			return "", err
		}
		opts.Near = &near
	}
	if in.RadiusM > 0 {
		opts.RadiusM = in.RadiusM
	}
	if strings.TrimSpace(in.Type) != "" {
		opts.Type = strings.TrimSpace(in.Type)
	}
	if in.Limit > 0 {
		opts.Limit = in.Limit
	}
	places, err := tryGeoProviders(t.providers,
		func(p []Place) bool { return len(p) == 0 },
		func(p GeoProvider) ([]Place, error) { return p.Places(ctx, query, opts) })
	if err != nil {
		return "", err
	}
	if len(places) == 0 {
		return "No places found.", nil
	}
	return formatPlaces(places), nil
}

// ---------- formatting ----------

// formatPlaces renders places as a numbered, attributed list for the model.
func formatPlaces(places []Place) string {
	var b strings.Builder
	for i, p := range places {
		if i > 0 {
			b.WriteString("\n")
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSpace(p.Address)
		}
		fmt.Fprintf(&b, "%d. %s", i+1, name)
		if addr := strings.TrimSpace(p.Address); addr != "" && addr != name {
			b.WriteString(" — " + addr)
		}
		fmt.Fprintf(&b, "\n   %s", formatLatLng(p.Lat, p.Lon))
		if p.Type != "" {
			fmt.Fprintf(&b, " · %s", p.Type)
		}
	}
	b.WriteString("\n\n" + attributionLine)
	return b.String()
}

// formatRoute renders a route as distance/duration plus steps.
func formatRoute(origin, dest string, r Route, units string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Route: %s · %s (%s)\n",
		formatDistance(r.DistanceMeters, units), formatDuration(r.DurationSeconds), r.Mode)
	fmt.Fprintf(&b, "%s → %s\n", strings.TrimSpace(origin), strings.TrimSpace(dest))
	for i, step := range r.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	b.WriteString("\n" + attributionLine)
	return b.String()
}
