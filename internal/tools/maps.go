package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file implements the pluggable geocoding/maps provider layer (Phase 3.4),
// mirroring the web-search layer in search.go. MyOGChat ships a single keyless
// OpenStreetMap backend (Nominatim + OSRM + Photon/Overpass) — no Google Maps,
// no API key, no billing (owner decision). The model-facing tools live in
// maps_tools.go and delegate to an ordered chain of GeoProviders.
//
// Two things here are policy requirements, not optimizations:
//   - a process-wide 1 request/second limiter (Nominatim's absolute cap), and
//   - a result cache (see GeoCache) so repeated queries never hammer the public
//     donated servers.

// Geo backend identifiers (used by env/DB config and Name()).
const (
	GeoProviderAuto = "auto"
	GeoProviderOSM  = "osm"
)

// Default endpoints. Each provider holds a baseURL field so tests can point it
// at an httptest.Server.
const (
	nominatimBaseURL = "https://nominatim.openstreetmap.org"
	osrmBaseURL      = "https://router.project-osrm.org"
	photonBaseURL    = "https://photon.komoot.io"
	overpassBaseURL  = "https://overpass-api.de"
)

// OSMAttribution is the ODbL attribution that must accompany OSM-derived data.
const OSMAttribution = "© OpenStreetMap contributors"

// attributionLine is appended to every tool result so the model relays it.
const attributionLine = "Source: " + OSMAttribution + " (ODbL)"

// osmUserAgent identifies the app to the OSM stack. Nominatim's usage policy
// requires a valid, app-identifying User-Agent (stock library UAs are rejected).
const osmUserAgent = "myogchat/1.0 (+https://chat.example.com)"

// osmReferer is sent alongside the User-Agent (Nominatim accepts either; sending
// both is safest).
const osmReferer = "https://chat.example.com"

// geoTimeout bounds one provider request.
const geoTimeout = 20 * time.Second

// defaultGeoLimit is the per-query result cap when none is configured.
const defaultGeoLimit = 5

// maxGeoLimit is the hard upper bound.
const maxGeoLimit = 20

// Cache TTLs. OSM's policy requires caching; Google's ToS caps lat/lng caching
// at 30 days, so 30 days is the ceiling. Static geocoding/place data lasts the
// full window; routes are refreshed weekly.
const (
	geoCacheTTL   = 30 * 24 * time.Hour
	routeCacheTTL = 7 * 24 * time.Hour
)

// osmStackLimiter is the process-wide 1 req/s limiter for the whole OSM stack
// (Nominatim's cap applies to the sum of all users). It is shared by every
// provider instance so the cap holds across concurrent requests.
var osmStackLimiter = newRateLimiter(time.Second)

// LatLng is a WGS84 coordinate. lon/lat ordering is centralized here (and in the
// helpers below) so per-endpoint quirks — OSRM wants "lon,lat", Google wants
// "lat,lng", Nominatim reverse takes lat=/lon= — never leak into call sites.
type LatLng struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Place is a provider-neutral geocoding/POI hit.
type Place struct {
	Name    string  `json:"name"`
	Address string  `json:"address"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Type    string  `json:"type,omitempty"`
	Source  string  `json:"source,omitempty"` // nominatim | photon | overpass
}

// Route is a provider-neutral routing result.
type Route struct {
	DistanceMeters  float64  `json:"distance_meters"`
	DurationSeconds float64  `json:"duration_seconds"`
	Mode            string   `json:"mode,omitempty"`
	Steps           []string `json:"steps,omitempty"`
	Source          string   `json:"source,omitempty"` // osrm
}

// GeoOptions carries per-call knobs providers honor.
type GeoOptions struct {
	Limit   int     // default 5, clamped to 1..20
	Region  string  // country code (ISO 3166-1 alpha-2) and/or "us-en"
	Units   string  // metric | imperial
	Mode    string  // driving | walking | cycling | transit
	Near    *LatLng // optional bias for Places
	RadiusM int     // optional search radius (meters) for Places
	Type    string  // optional POI type hint for Places
}

// GeoProvider is one maps backend.
type GeoProvider interface {
	Name() string
	Geocode(ctx context.Context, query string, opts GeoOptions) ([]Place, error)
	Reverse(ctx context.Context, at LatLng, opts GeoOptions) (Place, error)
	Directions(ctx context.Context, from, to LatLng, opts GeoOptions) (Route, error)
	Places(ctx context.Context, query string, opts GeoOptions) ([]Place, error)
}

// GeoCache persists provider results so repeated queries stay within OSM's
// usage policy (caching is mandatory for apps, not an optimization). It is
// satisfied by *store.Store; the tools package keeps only the interface so it
// stays free of storage types. Values are normalized results encoded as JSON.
type GeoCache interface {
	// GetGeoCache returns the cached value for key when present and unexpired.
	GetGeoCache(ctx context.Context, key string) (value string, ok bool, err error)
	// PutGeoCache stores value for key until expiresAt (unix milliseconds).
	PutGeoCache(ctx context.Context, key, value string, expiresAt int64) error
}

// GeoConfig is the resolved maps configuration (env + DB settings).
type GeoConfig struct {
	Provider string // auto | osm
	Region   string
	Units    string
	Limit    int
	Cache    GeoCache // optional; nil disables caching
	limiter  *rateLimiter
}

// Options derives the per-call GeoOptions from the resolved config.
func (c GeoConfig) Options() GeoOptions {
	return GeoOptions{
		Limit:  clampGeoLimit(c.Limit),
		Region: strings.TrimSpace(c.Region),
		Units:  NormalizeGeoUnits(c.Units),
	}
}

// NormalizeGeoProvider maps arbitrary input to a known selector. Only "osm" is
// selectable today (Google Maps was dropped); anything unrecognized becomes
// "auto", which also resolves to OSM.
func NormalizeGeoProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case GeoProviderOSM:
		return GeoProviderOSM
	default:
		return GeoProviderAuto
	}
}

// NormalizeGeoUnits maps arbitrary input to metric|imperial (default metric).
func NormalizeGeoUnits(u string) string {
	switch strings.ToLower(strings.TrimSpace(u)) {
	case "imperial", "us", "mi", "miles":
		return "imperial"
	default:
		return "metric"
	}
}

// BuildGeoProviders returns the ordered candidate chain for the configured
// selection. OSM needs no key, so the chain is never empty.
func BuildGeoProviders(cfg GeoConfig) []GeoProvider {
	limiter := cfg.limiter
	if limiter == nil {
		limiter = osmStackLimiter
	}
	return []GeoProvider{newOSMProvider(cfg.Cache, limiter)}
}

// GeoActive reports which provider would answer first, or "" when none is
// usable (unreachable while OSM exists).
func GeoActive(cfg GeoConfig) string {
	providers := BuildGeoProviders(cfg)
	if len(providers) == 0 {
		return ""
	}
	return providers[0].Name()
}

// clampGeoLimit normalizes a configured result count to [1, maxGeoLimit].
func clampGeoLimit(n int) int {
	if n <= 0 {
		return defaultGeoLimit
	}
	if n > maxGeoLimit {
		return maxGeoLimit
	}
	return n
}

// splitRegion splits a region like "us-en" into a country code and language.
func splitRegion(region string) (country, lang string) {
	region = strings.TrimSpace(region)
	if region == "" {
		return "", ""
	}
	if i := strings.IndexAny(region, "-_"); i > 0 {
		return region[:i], region[i+1:]
	}
	return region, ""
}

// parseLatLng parses "lat,lon" (also tolerating a space separator) and validates
// the WGS84 ranges. It is the single place free-text coordinates are accepted.
func parseLatLng(s string) (LatLng, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return LatLng{}, false
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	if len(parts) != 2 {
		return LatLng{}, false
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || !validLatLng(lat, lon) {
		return LatLng{}, false
	}
	return LatLng{Lat: lat, Lon: lon}, true
}

// validLatLng reports whether the coordinate is within valid WGS84 bounds.
func validLatLng(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// formatLatLng renders a coordinate as "lat, lon" (lat first).
func formatLatLng(lat, lon float64) string {
	return fmt.Sprintf("%.5f, %.5f", lat, lon)
}

// formatDistance renders meters in the requested unit system.
func formatDistance(meters float64, units string) string {
	if NormalizeGeoUnits(units) == "imperial" {
		miles := meters / 1609.344
		if miles < 0.1 {
			return fmt.Sprintf("%.0f ft", meters*3.28084)
		}
		return fmt.Sprintf("%.1f mi", miles)
	}
	if meters < 1000 {
		return fmt.Sprintf("%.0f m", meters)
	}
	return fmt.Sprintf("%.1f km", meters/1000)
}

// formatDuration renders seconds as "1 h 5 min" / "18 min" / "45 s".
func formatDuration(seconds float64) string {
	s := int(seconds + 0.5)
	if s < 60 {
		return fmt.Sprintf("%d s", s)
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

// firstSegment returns the first comma-separated part of a Nominatim
// display_name, used as a short place name when no explicit name is present.
func firstSegment(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ','); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// rateLimiter serializes calls to at most one per interval. The mutex is held
// across the wait so concurrent callers queue behind each other — a true global
// cap, matching Nominatim's absolute 1 req/s policy.
type rateLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

// newRateLimiter returns a limiter with the given minimum spacing. An interval
// of 0 disables waiting (used by tests).
func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{interval: interval}
}

// Wait blocks until at least one interval has elapsed since the previous Wait,
// or ctx is canceled. The first call returns immediately.
func (l *rateLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		if wait := l.interval - time.Since(l.last); wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	l.last = time.Now()
	return nil
}
