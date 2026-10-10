package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// osmProvider is the keyless OpenStreetMap backend:
//   - Nominatim  → Geocode / Reverse
//   - OSRM       → Directions (geojson output, so no polyline decoding)
//   - Photon     → Places (text search), with an Overpass fallback around a point
//
// Every request passes through a shared 1 req/s limiter and an optional result
// cache (both are OSM policy requirements).
type osmProvider struct {
	nominatimBaseURL string
	osrmBaseURL      string
	photonBaseURL    string
	overpassBaseURL  string
	hc               *http.Client
	limiter          *rateLimiter
	cache            GeoCache
}

// newOSMProvider builds the provider. cache may be nil (caching disabled);
// limiter must be non-nil (use osmStackLimiter in production).
func newOSMProvider(cache GeoCache, limiter *rateLimiter) *osmProvider {
	return &osmProvider{
		nominatimBaseURL: nominatimBaseURL,
		osrmBaseURL:      osrmBaseURL,
		photonBaseURL:    photonBaseURL,
		overpassBaseURL:  overpassBaseURL,
		hc:               &http.Client{Timeout: geoTimeout},
		limiter:          limiter,
		cache:            cache,
	}
}

func (p *osmProvider) Name() string { return GeoProviderOSM }

// setHeaders applies the identity headers the OSM stack requires (a valid
// app-identifying User-Agent and/or Referer).
func (p *osmProvider) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", osmUserAgent)
	req.Header.Set("Referer", osmReferer)
	req.Header.Set("Accept", "application/json")
}

// get performs one rate-limited GET and returns the (capped) body.
func (p *osmProvider) get(ctx context.Context, base, path string, q url.Values) ([]byte, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	endpoint := base + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	p.setHeaders(req)
	return p.do(req)
}

// post performs one rate-limited form-encoded POST (Overpass).
func (p *osmProvider) post(ctx context.Context, base, path string, form url.Values) ([]byte, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	p.setHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return p.do(req)
}

func (p *osmProvider) do(req *http.Request) ([]byte, error) {
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("osm request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("osm: HTTP %d: %s", resp.StatusCode, firstLine(raw))
	}
	return raw, nil
}

// cacheGet returns a previously cached normalized result, if any.
func (p *osmProvider) cacheGet(ctx context.Context, key string) (json.RawMessage, bool) {
	if p.cache == nil {
		return nil, false
	}
	v, ok, err := p.cache.GetGeoCache(ctx, key)
	if err != nil || !ok || v == "" {
		return nil, false
	}
	return json.RawMessage(v), true
}

// cachePut stores a normalized result until now+ttl. Best-effort: a cache write
// failure never fails the call.
func (p *osmProvider) cachePut(ctx context.Context, key string, v any, ttl time.Duration) {
	if p.cache == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = p.cache.PutGeoCache(ctx, key, string(b), time.Now().Add(ttl).UnixMilli())
}

// ---------- Geocode (Nominatim) ----------

func (p *osmProvider) Geocode(ctx context.Context, query string, opts GeoOptions) ([]Place, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query is required")
	}
	country, _ := splitRegion(opts.Region)
	limit := clampGeoLimit(opts.Limit)
	key := "geocode:" + country + ":" + strconv.Itoa(limit) + ":" + strings.ToLower(query)
	if raw, ok := p.cacheGet(ctx, key); ok {
		var places []Place
		if json.Unmarshal(raw, &places) == nil {
			return places, nil
		}
	}
	places, err := p.nominatimGeocode(ctx, query, country, limit)
	if err != nil {
		return nil, err
	}
	p.cachePut(ctx, key, places, geoCacheTTL)
	return places, nil
}

func (p *osmProvider) nominatimGeocode(ctx context.Context, query, country string, limit int) ([]Place, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "jsonv2")
	q.Set("limit", strconv.Itoa(limit))
	q.Set("addressdetails", "1")
	if country != "" {
		q.Set("countrycodes", strings.ToLower(country))
	}
	raw, err := p.get(ctx, p.nominatimBaseURL, "/search", q)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
		Name        string `json:"name"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("nominatim: decode response: %w", err)
	}
	places := make([]Place, 0, len(rows))
	for _, r := range rows {
		lat, err1 := strconv.ParseFloat(r.Lat, 64)
		lon, err2 := strconv.ParseFloat(r.Lon, 64)
		if err1 != nil || err2 != nil || !validLatLng(lat, lon) {
			continue
		}
		name := strings.TrimSpace(r.Name)
		if name == "" {
			name = firstSegment(r.DisplayName)
		}
		places = append(places, Place{
			Name:    name,
			Address: r.DisplayName,
			Lat:     lat,
			Lon:     lon,
			Type:    r.Type,
			Source:  "nominatim",
		})
	}
	return places, nil
}

// ---------- Reverse (Nominatim) ----------

func (p *osmProvider) Reverse(ctx context.Context, at LatLng, opts GeoOptions) (Place, error) {
	if !validLatLng(at.Lat, at.Lon) {
		return Place{}, errors.New("invalid coordinates")
	}
	key := fmt.Sprintf("reverse:%.5f,%.5f", at.Lat, at.Lon)
	if raw, ok := p.cacheGet(ctx, key); ok {
		var pl Place
		if json.Unmarshal(raw, &pl) == nil {
			return pl, nil
		}
	}
	pl, err := p.nominatimReverse(ctx, at)
	if err != nil {
		return Place{}, err
	}
	p.cachePut(ctx, key, pl, geoCacheTTL)
	return pl, nil
}

func (p *osmProvider) nominatimReverse(ctx context.Context, at LatLng) (Place, error) {
	q := url.Values{}
	q.Set("lat", strconv.FormatFloat(at.Lat, 'f', 7, 64))
	q.Set("lon", strconv.FormatFloat(at.Lon, 'f', 7, 64))
	q.Set("format", "jsonv2")
	q.Set("addressdetails", "1")
	raw, err := p.get(ctx, p.nominatimBaseURL, "/reverse", q)
	if err != nil {
		return Place{}, err
	}
	var row struct {
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
		Name        string `json:"name"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return Place{}, fmt.Errorf("nominatim: decode response: %w", err)
	}
	if row.DisplayName == "" && row.Error != "" {
		return Place{}, fmt.Errorf("nominatim: %s", row.Error)
	}
	lat, _ := strconv.ParseFloat(row.Lat, 64)
	lon, _ := strconv.ParseFloat(row.Lon, 64)
	if !validLatLng(lat, lon) {
		lat, lon = at.Lat, at.Lon
	}
	name := strings.TrimSpace(row.Name)
	if name == "" {
		name = firstSegment(row.DisplayName)
	}
	return Place{
		Name:    name,
		Address: row.DisplayName,
		Lat:     lat,
		Lon:     lon,
		Type:    row.Type,
		Source:  "nominatim",
	}, nil
}

// ---------- Directions (OSRM) ----------

func (p *osmProvider) Directions(ctx context.Context, from, to LatLng, opts GeoOptions) (Route, error) {
	if !validLatLng(from.Lat, from.Lon) || !validLatLng(to.Lat, to.Lon) {
		return Route{}, errors.New("invalid coordinates")
	}
	mode := normalizeMode(opts.Mode)
	profile, err := osrmProfile(mode)
	if err != nil {
		return Route{}, err
	}
	key := fmt.Sprintf("route:%s:%.5f,%.5f:%.5f,%.5f", profile, from.Lat, from.Lon, to.Lat, to.Lon)
	if raw, ok := p.cacheGet(ctx, key); ok {
		var r Route
		if json.Unmarshal(raw, &r) == nil {
			return r, nil
		}
	}
	r, err := p.osrmRoute(ctx, profile, mode, from, to)
	if err != nil {
		return Route{}, err
	}
	p.cachePut(ctx, key, r, routeCacheTTL)
	return r, nil
}

func (p *osmProvider) osrmRoute(ctx context.Context, profile, mode string, from, to LatLng) (Route, error) {
	// OSRM coordinates are "lon,lat" and semicolon-joined.
	coords := osrmCoord(from) + ";" + osrmCoord(to)
	q := url.Values{}
	q.Set("overview", "false")
	q.Set("steps", "true")
	q.Set("geometries", "geojson")
	raw, err := p.get(ctx, p.osrmBaseURL, "/route/v1/"+profile+"/"+coords, q)
	if err != nil {
		return Route{}, err
	}
	var out struct {
		Code   string `json:"code"`
		Routes []struct {
			Distance float64 `json:"distance"`
			Duration float64 `json:"duration"`
			Legs     []struct {
				Steps []struct {
					Name     string  `json:"name"`
					Distance float64 `json:"distance"`
					Maneuver struct {
						Type     string `json:"type"`
						Modifier string `json:"modifier"`
					} `json:"maneuver"`
				} `json:"steps"`
			} `json:"legs"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Route{}, fmt.Errorf("osrm: decode response: %w", err)
	}
	if out.Code != "" && out.Code != "Ok" {
		return Route{}, fmt.Errorf("osrm: %s", out.Code)
	}
	if len(out.Routes) == 0 {
		return Route{}, errors.New("osrm: no route found")
	}
	r := out.Routes[0]
	steps := make([]string, 0)
	for _, leg := range r.Legs {
		for _, s := range leg.Steps {
			if text := formatStep(s.Maneuver.Type, s.Maneuver.Modifier, s.Name, s.Distance); text != "" {
				steps = append(steps, text)
			}
		}
	}
	return Route{
		DistanceMeters:  r.Distance,
		DurationSeconds: r.Duration,
		Mode:            mode,
		Steps:           steps,
		Source:          "osrm",
	}, nil
}

// osrmCoord renders a coordinate in OSRM's lon,lat order.
func osrmCoord(ll LatLng) string {
	return strconv.FormatFloat(ll.Lon, 'f', 6, 64) + "," + strconv.FormatFloat(ll.Lat, 'f', 6, 64)
}

// osrmProfile maps the normalized mode to an OSRM profile, rejecting transit
// (the keyless OSM stack has no transit routing).
func osrmProfile(mode string) (string, error) {
	switch mode {
	case "driving":
		return "driving", nil
	case "walking":
		return "foot", nil
	case "cycling":
		return "bike", nil
	case "transit":
		return "", errors.New("transit directions are not available from the keyless OSM stack")
	default:
		return "driving", nil
	}
}

// normalizeMode maps arbitrary input to driving|walking|cycling|transit.
func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "walking", "walk", "foot":
		return "walking"
	case "cycling", "bike", "bicycle":
		return "cycling"
	case "transit":
		return "transit"
	default:
		return "driving"
	}
}

// formatStep renders one OSRM maneuver as a short instruction.
func formatStep(typ, modifier, name string, meters float64) string {
	var b strings.Builder
	if action := stepAction(typ, modifier); action != "" {
		b.WriteString(action)
	}
	if name != "" {
		if b.Len() > 0 {
			b.WriteString(" on ")
		}
		b.WriteString(name)
	}
	if meters > 0 {
		fmt.Fprintf(&b, " (%s)", formatDistance(meters, "metric"))
	}
	return strings.TrimSpace(b.String())
}

// stepAction turns an OSRM maneuver type/modifier into a verb phrase.
func stepAction(typ, modifier string) string {
	switch typ {
	case "depart":
		return "Head"
	case "arrive":
		return "Arrive"
	case "turn", "end of road":
		if modifier != "" {
			return "Turn " + modifier
		}
		return "Turn"
	case "new name", "continue":
		return "Continue"
	case "merge":
		return "Merge"
	case "on ramp":
		return "Take the ramp"
	case "off ramp":
		return "Take the exit"
	case "fork":
		if modifier != "" {
			return "Keep " + modifier
		}
		return "Keep"
	case "roundabout", "rotary":
		return "Take the roundabout"
	case "":
		return ""
	default:
		return capitalize(strings.ReplaceAll(typ, "_", " "))
	}
}

// ---------- Places (Photon, Overpass fallback) ----------

func (p *osmProvider) Places(ctx context.Context, query string, opts GeoOptions) ([]Place, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query is required")
	}
	limit := clampGeoLimit(opts.Limit)
	key := "places:" + placesKey(query, opts, limit)
	if raw, ok := p.cacheGet(ctx, key); ok {
		var places []Place
		if json.Unmarshal(raw, &places) == nil {
			return places, nil
		}
	}
	places, err := p.photonSearch(ctx, query, opts, limit)
	if err != nil || len(places) == 0 {
		// Overpass can only search around a point, so it is a fallback for
		// Photon only when a location anchor is available.
		if opts.Near != nil {
			if op, oerr := p.overpassSearch(ctx, query, opts, limit); oerr == nil && len(op) > 0 {
				places, err = op, nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	p.cachePut(ctx, key, places, geoCacheTTL)
	return places, nil
}

// placesKey builds a stable cache key for a places query.
func placesKey(query string, opts GeoOptions, limit int) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(query))
	if opts.Near != nil {
		fmt.Fprintf(&b, ":%.5f,%.5f", opts.Near.Lat, opts.Near.Lon)
	}
	if opts.RadiusM > 0 {
		fmt.Fprintf(&b, ":r%d", opts.RadiusM)
	}
	if t := strings.TrimSpace(opts.Type); t != "" {
		b.WriteString(":" + strings.ToLower(t))
	}
	fmt.Fprintf(&b, ":l%d", limit)
	return b.String()
}

func (p *osmProvider) photonSearch(ctx context.Context, query string, opts GeoOptions, limit int) ([]Place, error) {
	_, lang := splitRegion(opts.Region)
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", strconv.Itoa(limit))
	if lang != "" {
		q.Set("lang", lang)
	}
	if opts.Near != nil {
		q.Set("lat", strconv.FormatFloat(opts.Near.Lat, 'f', 6, 64))
		q.Set("lon", strconv.FormatFloat(opts.Near.Lon, 'f', 6, 64))
	}
	raw, err := p.get(ctx, p.photonBaseURL, "/api/", q)
	if err != nil {
		return nil, err
	}
	var fc struct {
		Features []struct {
			Geometry struct {
				Coordinates []float64 `json:"coordinates"` // [lon, lat]
			} `json:"geometry"`
			Properties struct {
				Name     string `json:"name"`
				Street   string `json:"street"`
				City     string `json:"city"`
				State    string `json:"state"`
				Country  string `json:"country"`
				Postcode string `json:"postcode"`
				OSMValue string `json:"osm_value"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("photon: decode response: %w", err)
	}
	places := make([]Place, 0, len(fc.Features))
	for _, f := range fc.Features {
		if len(f.Geometry.Coordinates) < 2 {
			continue
		}
		// GeoJSON is [lon, lat] — never assume [lat, lon].
		lon, lat := f.Geometry.Coordinates[0], f.Geometry.Coordinates[1]
		if !validLatLng(lat, lon) {
			continue
		}
		places = append(places, Place{
			Name:    strings.TrimSpace(f.Properties.Name),
			Address: photonAddress(f.Properties.Street, f.Properties.City, f.Properties.State, f.Properties.Country, f.Properties.Postcode),
			Lat:     lat,
			Lon:     lon,
			Type:    f.Properties.OSMValue,
			Source:  "photon",
		})
	}
	return places, nil
}

// photonAddress joins the non-empty address parts.
func photonAddress(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

func (p *osmProvider) overpassSearch(ctx context.Context, query string, opts GeoOptions, limit int) ([]Place, error) {
	if opts.Near == nil {
		return nil, errors.New("overpass: a location is required")
	}
	radius := opts.RadiusM
	if radius <= 0 {
		radius = 5000
	}
	// Escape quotes/backslashes for the Overpass QL regex string. The query is
	// never interpolated into a shell or URL path — only this bounded form body.
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(query)
	ql := fmt.Sprintf(`[out:json][timeout:15];nwr["name"~"%s",i](around:%d,%.6f,%.6f);out center %d;`,
		esc, radius, opts.Near.Lat, opts.Near.Lon, limit)
	form := url.Values{}
	form.Set("data", ql)
	raw, err := p.post(ctx, p.overpassBaseURL, "/api/interpreter", form)
	if err != nil {
		return nil, err
	}
	var out struct {
		Elements []struct {
			Type   string  `json:"type"`
			ID     int64   `json:"id"`
			Lat    float64 `json:"lat"`
			Lon    float64 `json:"lon"`
			Center *struct {
				Lat float64 `json:"lat"`
				Lon float64 `json:"lon"`
			} `json:"center"`
			Tags map[string]string `json:"tags"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("overpass: decode response: %w", err)
	}
	places := make([]Place, 0, len(out.Elements))
	for _, e := range out.Elements {
		lat, lon := e.Lat, e.Lon
		if e.Center != nil {
			lat, lon = e.Center.Lat, e.Center.Lon
		}
		if !validLatLng(lat, lon) {
			continue
		}
		places = append(places, Place{
			Name:    strings.TrimSpace(e.Tags["name"]),
			Address: overpassAddress(e.Tags),
			Lat:     lat,
			Lon:     lon,
			Type:    overpassType(e.Tags),
			Source:  "overpass",
		})
	}
	return places, nil
}

// overpassAddress assembles a short address from common OSM address tags.
func overpassAddress(tags map[string]string) string {
	parts := make([]string, 0, 4)
	for _, k := range []string{"addr:housenumber", "addr:street", "addr:city", "addr:postcode"} {
		if v := strings.TrimSpace(tags[k]); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

// overpassType derives a coarse type from the OSM tags.
func overpassType(tags map[string]string) string {
	for _, k := range []string{"amenity", "shop", "tourism", "leisure", "historic", "office", "craft"} {
		if v := strings.TrimSpace(tags[k]); v != "" {
			return k + "=" + v
		}
	}
	return ""
}

// capitalize upper-cases the first byte of s (ASCII).
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
