package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestOSM returns a provider pointed at a test server with rate limiting
// disabled (so tests do not sleep 1s between requests).
func newTestOSM(t *testing.T, srv *httptest.Server, cache GeoCache) *osmProvider {
	t.Helper()
	p := newOSMProvider(cache, newRateLimiter(0))
	p.nominatimBaseURL = srv.URL
	p.osrmBaseURL = srv.URL
	p.photonBaseURL = srv.URL
	p.overpassBaseURL = srv.URL
	p.hc = srv.Client()
	return p
}

func TestNominatimGeocode(t *testing.T) {
	var gotQuery url.Values
	var gotUA, gotReferer, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("Referer")
		io.WriteString(w, `[{"place_id":1,"lat":"48.8566","lon":"2.3522",
			"display_name":"Paris, Île-de-France, France","type":"city","name":"Paris"}]`)
	}))
	defer srv.Close()

	p := newTestOSM(t, srv, nil)
	places, err := p.Geocode(context.Background(), "Paris", GeoOptions{Region: "us-en", Limit: 3})
	if err != nil {
		t.Fatalf("Geocode: %v", err)
	}
	if gotPath != "/search" {
		t.Errorf("path = %q, want /search", gotPath)
	}
	if got := gotQuery.Get("q"); got != "Paris" {
		t.Errorf("q = %q", got)
	}
	if got := gotQuery.Get("format"); got != "jsonv2" {
		t.Errorf("format = %q", got)
	}
	if got := gotQuery.Get("limit"); got != "3" {
		t.Errorf("limit = %q", got)
	}
	// Region "us-en" → countrycodes=us (lang is dropped for Nominatim).
	if got := gotQuery.Get("countrycodes"); got != "us" {
		t.Errorf("countrycodes = %q, want us", got)
	}
	if !strings.Contains(gotUA, "myogchat") {
		t.Errorf("user-agent = %q", gotUA)
	}
	if gotReferer == "" {
		t.Error("referer not set")
	}
	if len(places) != 1 {
		t.Fatalf("places = %+v", places)
	}
	if places[0].Name != "Paris" || places[0].Lat != 48.8566 || places[0].Lon != 2.3522 {
		t.Errorf("place = %+v", places[0])
	}
	if places[0].Source != "nominatim" {
		t.Errorf("source = %q", places[0].Source)
	}
}

func TestNominatimReverse(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reverse" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		io.WriteString(w, `{"lat":"48.8584","lon":"2.2945","display_name":"5 Avenue Anatole France, Paris, France","type":"house","name":"Eiffel Tower"}`)
	}))
	defer srv.Close()

	p := newTestOSM(t, srv, nil)
	place, err := p.Reverse(context.Background(), LatLng{Lat: 48.8584, Lon: 2.2945}, GeoOptions{})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if got := gotQuery.Get("lat"); !strings.HasPrefix(got, "48.8584") {
		t.Errorf("lat = %q", got)
	}
	if got := gotQuery.Get("lon"); !strings.HasPrefix(got, "2.2945") {
		t.Errorf("lon = %q", got)
	}
	if place.Name != "Eiffel Tower" || place.Lat != 48.8584 || place.Lon != 2.2945 {
		t.Errorf("place = %+v", place)
	}
}

func TestNominatimReverseRejectsInvalidCoords(t *testing.T) {
	p := newOSMProvider(nil, newRateLimiter(0))
	if _, err := p.Reverse(context.Background(), LatLng{Lat: 200, Lon: 0}, GeoOptions{}); err == nil {
		t.Fatal("expected an error for out-of-range latitude")
	}
}

func TestOSRMDirections(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		io.WriteString(w, `{"code":"Ok","routes":[{"distance":1234.5,"duration":300.0,"legs":[{"steps":[
			{"name":"Main St","distance":500,"maneuver":{"type":"turn","modifier":"left"}},
			{"name":"","distance":0,"maneuver":{"type":"arrive"}}
		]}]}]}`)
	}))
	defer srv.Close()

	p := newTestOSM(t, srv, nil)
	from := LatLng{Lat: 48.8566, Lon: 2.3522}
	to := LatLng{Lat: 48.8584, Lon: 2.2945}
	route, err := p.Directions(context.Background(), from, to, GeoOptions{Mode: "driving"})
	if err != nil {
		t.Fatalf("Directions: %v", err)
	}
	// OSRM coordinates are lon,lat and semicolon-joined.
	wantPath := "/route/v1/driving/2.352200,48.856600;2.294500,48.858400"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if got := gotQuery.Get("geometries"); got != "geojson" {
		t.Errorf("geometries = %q, want geojson", got)
	}
	if got := gotQuery.Get("overview"); got != "false" {
		t.Errorf("overview = %q", got)
	}
	if route.DistanceMeters != 1234.5 || route.DurationSeconds != 300.0 {
		t.Errorf("route = %+v", route)
	}
	if route.Source != "osrm" || route.Mode != "driving" {
		t.Errorf("route meta = %+v", route)
	}
	if len(route.Steps) != 2 || !strings.Contains(route.Steps[0], "Turn left") {
		t.Errorf("steps = %+v", route.Steps)
	}
}

func TestOSRMDirectionsTransitUnsupported(t *testing.T) {
	p := newOSMProvider(nil, newRateLimiter(0))
	_, err := p.Directions(context.Background(), LatLng{1, 1}, LatLng{2, 2}, GeoOptions{Mode: "transit"})
	if err == nil || !strings.Contains(err.Error(), "transit") {
		t.Fatalf("err = %v, want a transit-unsupported error", err)
	}
}

func TestPhotonPlaces(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		io.WriteString(w, `{"features":[{"geometry":{"coordinates":[2.3522,48.8566]},
			"properties":{"name":"Paris","city":"Paris","country":"France","osm_value":"city"}}]}`)
	}))
	defer srv.Close()

	p := newTestOSM(t, srv, nil)
	places, err := p.Places(context.Background(), "Paris", GeoOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Places: %v", err)
	}
	if got := gotQuery.Get("q"); got != "Paris" {
		t.Errorf("q = %q", got)
	}
	if len(places) != 1 {
		t.Fatalf("places = %+v", places)
	}
	// GeoJSON coordinates are [lon, lat]; a swapped parse would set Lat=2.35.
	if places[0].Lat != 48.8566 || places[0].Lon != 2.3522 {
		t.Errorf("lat/lon order wrong: %+v", places[0])
	}
	if places[0].Source != "photon" {
		t.Errorf("source = %q", places[0].Source)
	}
}

func TestOverpassPlacesFallback(t *testing.T) {
	var photonCalls, overpassCalls int
	var overpassBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/":
			photonCalls++
			io.WriteString(w, `{"features":[]}`)
		case r.URL.Path == "/api/interpreter":
			overpassCalls++
			b, _ := io.ReadAll(r.Body)
			overpassBody = string(b)
			io.WriteString(w, `{"elements":[{"type":"node","id":1,"lat":48.86,"lon":2.35,
				"tags":{"name":"Café X","amenity":"cafe","addr:street":"Rue Y"}}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newTestOSM(t, srv, nil)
	near := LatLng{Lat: 48.86, Lon: 2.35}
	places, err := p.Places(context.Background(), "Café", GeoOptions{Near: &near, RadiusM: 1000, Limit: 3})
	if err != nil {
		t.Fatalf("Places: %v", err)
	}
	if photonCalls != 1 || overpassCalls != 1 {
		t.Fatalf("calls photon=%d overpass=%d, want 1/1", photonCalls, overpassCalls)
	}
	if !strings.Contains(overpassBody, "around%3A1000") && !strings.Contains(overpassBody, "around:1000") {
		t.Errorf("overpass body = %q", overpassBody)
	}
	if len(places) != 1 || places[0].Source != "overpass" {
		t.Fatalf("places = %+v", places)
	}
	if places[0].Name != "Café X" || places[0].Type != "amenity=cafe" {
		t.Errorf("place = %+v", places[0])
	}
}

func TestPlacesNoFallbackWithoutLocation(t *testing.T) {
	var overpassCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/interpreter" {
			overpassCalls++
		}
		io.WriteString(w, `{"features":[]}`)
	}))
	defer srv.Close()

	p := newTestOSM(t, srv, nil)
	places, err := p.Places(context.Background(), "nowhere", GeoOptions{})
	if err != nil {
		t.Fatalf("Places: %v", err)
	}
	if len(places) != 0 {
		t.Errorf("places = %+v, want empty", places)
	}
	if overpassCalls != 0 {
		t.Errorf("overpass called %d times without a location anchor", overpassCalls)
	}
}

// ---------- rate limiter ----------

func TestRateLimiterEnforcesSpacing(t *testing.T) {
	l := newRateLimiter(40 * time.Millisecond)
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait 2: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 40ms", elapsed)
	}
}

func TestRateLimiterContextCancel(t *testing.T) {
	l := newRateLimiter(time.Hour)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait canceled = %v, want context.Canceled", err)
	}
}

// ---------- cache ----------

type cachedGeo struct {
	value   string
	expires int64
}

type fakeGeoCache struct {
	mu    sync.Mutex
	items map[string]cachedGeo
}

func newFakeGeoCache() *fakeGeoCache { return &fakeGeoCache{items: map[string]cachedGeo{}} }

func (c *fakeGeoCache) GetGeoCache(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || it.expires <= time.Now().UnixMilli() {
		return "", false, nil
	}
	return it.value, true, nil
}

func (c *fakeGeoCache) PutGeoCache(_ context.Context, key, value string, expiresAt int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = cachedGeo{value: value, expires: expiresAt}
	return nil
}

func (c *fakeGeoCache) expireAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.items {
		v.expires = time.Now().Add(-time.Hour).UnixMilli()
		c.items[k] = v
	}
}

func TestGeocodeUsesCache(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `[{"lat":"1.0","lon":"2.0","display_name":"X","name":"X"}]`)
	}))
	defer srv.Close()

	cache := newFakeGeoCache()
	p := newTestOSM(t, srv, cache)
	ctx := context.Background()

	if _, err := p.Geocode(ctx, "X", GeoOptions{}); err != nil {
		t.Fatalf("Geocode 1: %v", err)
	}
	if _, err := p.Geocode(ctx, "X", GeoOptions{}); err != nil {
		t.Fatalf("Geocode 2: %v", err)
	}
	if calls != 1 {
		t.Errorf("HTTP calls = %d, want 1 (second should be cached)", calls)
	}

	// Once the entry expires, the provider hits the network again.
	cache.expireAll()
	if _, err := p.Geocode(ctx, "X", GeoOptions{}); err != nil {
		t.Fatalf("Geocode 3: %v", err)
	}
	if calls != 2 {
		t.Errorf("HTTP calls after expiry = %d, want 2", calls)
	}
}

func TestGeocodeWithoutCacheAlwaysCalls(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `[{"lat":"1.0","lon":"2.0","display_name":"X","name":"X"}]`)
	}))
	defer srv.Close()

	p := newTestOSM(t, srv, nil)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := p.Geocode(ctx, "X", GeoOptions{}); err != nil {
			t.Fatalf("Geocode: %v", err)
		}
	}
	if calls != 2 {
		t.Errorf("HTTP calls = %d, want 2 (no cache)", calls)
	}
}

// ---------- helpers, selection ----------

func TestParseLatLng(t *testing.T) {
	cases := []struct {
		in       string
		ok       bool
		lat, lon float64
	}{
		{"48.8566,2.3522", true, 48.8566, 2.3522},
		{"48.8566 2.3522", true, 48.8566, 2.3522},
		{"-33.86,151.21", true, -33.86, 151.21},
		{"Paris", false, 0, 0},
		{"200,0", false, 0, 0},
		{"", false, 0, 0},
		{"1,2,3", false, 0, 0},
	}
	for _, c := range cases {
		ll, ok := parseLatLng(c.in)
		if ok != c.ok {
			t.Errorf("parseLatLng(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && (ll.Lat != c.lat || ll.Lon != c.lon) {
			t.Errorf("parseLatLng(%q) = %+v, want %v,%v", c.in, ll, c.lat, c.lon)
		}
	}
}

func TestBuildGeoProvidersAndActive(t *testing.T) {
	if got := GeoActive(GeoConfig{}); got != GeoProviderOSM {
		t.Errorf("GeoActive = %q, want osm", got)
	}
	providers := BuildGeoProviders(GeoConfig{Provider: "auto"})
	if len(providers) != 1 || providers[0].Name() != GeoProviderOSM {
		t.Fatalf("providers = %v", providers)
	}
	if got := NormalizeGeoProvider("google"); got != GeoProviderAuto {
		t.Errorf("NormalizeGeoProvider(google) = %q, want auto", got)
	}
	if got := NormalizeGeoProvider("osm"); got != GeoProviderOSM {
		t.Errorf("NormalizeGeoProvider(osm) = %q, want osm", got)
	}
	if got := NormalizeGeoUnits("imperial"); got != "imperial" {
		t.Errorf("NormalizeGeoUnits = %q", got)
	}
	if got := NormalizeGeoUnits(""); got != "metric" {
		t.Errorf("NormalizeGeoUnits default = %q, want metric", got)
	}
}

func TestGeoConfigOptionsClamp(t *testing.T) {
	if got := (GeoConfig{Limit: 0}).Options().Limit; got != defaultGeoLimit {
		t.Errorf("default limit = %d", got)
	}
	if got := (GeoConfig{Limit: 100}).Options().Limit; got != maxGeoLimit {
		t.Errorf("clamped limit = %d", got)
	}
}

func TestFormatDistance(t *testing.T) {
	if got := formatDistance(1500, "metric"); got != "1.5 km" {
		t.Errorf("metric = %q", got)
	}
	if got := formatDistance(1500, "imperial"); got != "0.9 mi" {
		t.Errorf("imperial = %q", got)
	}
}

// ---------- tools (chain + formatting) ----------

// stubGeoProvider is a programmable GeoProvider for tool/chain tests.
type stubGeoProvider struct {
	name   string
	places []Place
	route  Route
	err    error
}

func (s *stubGeoProvider) Name() string { return s.name }
func (s *stubGeoProvider) Geocode(context.Context, string, GeoOptions) ([]Place, error) {
	return s.places, s.err
}
func (s *stubGeoProvider) Reverse(context.Context, LatLng, GeoOptions) (Place, error) {
	if len(s.places) > 0 {
		return s.places[0], s.err
	}
	return Place{}, s.err
}
func (s *stubGeoProvider) Directions(context.Context, LatLng, LatLng, GeoOptions) (Route, error) {
	return s.route, s.err
}
func (s *stubGeoProvider) Places(context.Context, string, GeoOptions) ([]Place, error) {
	return s.places, s.err
}

func TestGeocodeToolFormatsWithAttribution(t *testing.T) {
	stub := &stubGeoProvider{name: "osm", places: []Place{{Name: "Paris", Address: "Paris, France", Lat: 48.8566, Lon: 2.3522}}}
	tool := NewGeocodeTool([]GeoProvider{stub}, GeoOptions{})
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"Paris"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "Paris") || !strings.Contains(out, OSMAttribution) {
		t.Errorf("out = %q", out)
	}
}

func TestGeocodeToolFallbackOnError(t *testing.T) {
	bad := &stubGeoProvider{name: "bad", err: errors.New("boom")}
	good := &stubGeoProvider{name: "good", places: []Place{{Name: "Ok", Lat: 1, Lon: 2}}}
	tool := NewGeocodeTool([]GeoProvider{bad, good}, GeoOptions{})
	out, err := tool.Run(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "Ok") {
		t.Errorf("out = %q", out)
	}
}

func TestDirectionsToolResolvesTextPoints(t *testing.T) {
	// The stub geocodes any text to (48.85, 2.35) and routes successfully.
	stub := &stubGeoProvider{
		name:   "osm",
		places: []Place{{Name: "X", Lat: 48.85, Lon: 2.35}},
		route:  Route{DistanceMeters: 1000, DurationSeconds: 120, Mode: "driving", Source: "osrm"},
	}
	tool := NewDirectionsTool([]GeoProvider{stub}, GeoOptions{})
	out, err := tool.Run(context.Background(), json.RawMessage(`{"origin":"A","destination":"B"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "1.0 km") || !strings.Contains(out, "2 min") || !strings.Contains(out, OSMAttribution) {
		t.Errorf("out = %q", out)
	}
}

func TestDirectionsToolRejectsBadCoords(t *testing.T) {
	stub := &stubGeoProvider{name: "osm"}
	tool := NewDirectionsTool([]GeoProvider{stub}, GeoOptions{})
	// "200,0" is not a valid lat,lon and the stub geocodes nothing → error.
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"origin":"200,0","destination":"B"}`)); err == nil {
		t.Fatal("expected an error for an unresolvable origin")
	}
}

func TestReverseToolValidatesRange(t *testing.T) {
	stub := &stubGeoProvider{name: "osm"}
	tool := NewReverseTool([]GeoProvider{stub}, GeoOptions{})
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"lat":999,"lon":0}`)); err == nil {
		t.Fatal("expected a validation error")
	}
}
