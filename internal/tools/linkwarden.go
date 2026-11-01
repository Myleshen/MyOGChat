package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// linkwardenTimeout bounds one Linkwarden API request.
	linkwardenTimeout = 20 * time.Second
	// linkwardenDefaultLimit is the page size used when a tool does not ask for
	// one, and linkwardenMaxLimit caps what the tools return.
	linkwardenDefaultLimit = 10
	linkwardenMaxLimit     = 50
	// linkwardenMaxScanPages bounds how many result pages a filtered listing
	// reads when it has to re-check tag membership client-side.
	linkwardenMaxScanPages = 5
	// linkwardenListBodyLimit bounds a link/tag/collection listing. List
	// payloads are larger than the 1 MB used for error snippets.
	linkwardenListBodyLimit = 8 << 20
)

// LinkwardenClient talks to a self-hosted Linkwarden instance's v1 API
// (https://docs.linkwarden.app). Authentication is a per-user API token sent as
// "Authorization: Bearer <token>".
type LinkwardenClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// LinkwardenLink is the subset of a Linkwarden link used by the tools.
type LinkwardenLink struct {
	ID          int64                 `json:"id"`
	Name        string                `json:"name"`
	URL         string                `json:"url"`
	Description string                `json:"description"`
	CreatedAt   string                `json:"createdAt"`
	Tags        []LinkwardenTag       `json:"tags"`
	Collection  *LinkwardenCollection `json:"collection"`
	PinnedBy    []int64               `json:"pinnedBy"`
}

// LinkwardenTag is a tag attached to a link, or one entry of the tag list. The
// list endpoint includes _count.links; links embed tags without a count.
type LinkwardenTag struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Links int64  `json:"-"`
}

// UnmarshalJSON flattens the nested "_count": {"links": N} shape.
func (t *LinkwardenTag) UnmarshalJSON(data []byte) error {
	var v struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Count struct {
			Links int64 `json:"links"`
		} `json:"_count"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	t.ID, t.Name, t.Links = v.ID, v.Name, v.Count.Links
	return nil
}

// LinkwardenCollection is a collection (folder), possibly nested. The
// collection list includes _count.links; links embed their collection without
// a count.
type LinkwardenCollection struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID *int64 `json:"parentId"`
	IsPublic bool   `json:"isPublic"`
	Links    int64  `json:"-"`
}

// UnmarshalJSON flattens the nested "_count": {"links": N} shape.
func (c *LinkwardenCollection) UnmarshalJSON(data []byte) error {
	var v struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		ParentID *int64 `json:"parentId"`
		IsPublic bool   `json:"isPublic"`
		Count    struct {
			Links int64 `json:"links"`
		} `json:"_count"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	c.ID, c.Name, c.ParentID, c.IsPublic, c.Links = v.ID, v.Name, v.ParentID, v.IsPublic, v.Count.Links
	return nil
}

// LinkwardenListOptions selects and orders links for SearchOptions.
type LinkwardenListOptions struct {
	Query        string
	Sort         string // newest (default), oldest, name_asc, name_desc
	Limit        int    // default 10, max 50
	TagID        int64  // optional filter
	CollectionID int64  // optional filter
	PinnedOnly   bool
}

// LinkwardenTagOptions selects and orders the tag list.
type LinkwardenTagOptions struct {
	Search string
	Sort   string // name_asc (default), name_desc, newest, oldest, count_desc, count_asc
	Limit  int
}

// LinkwardenCollectionOptions selects collections (folders).
type LinkwardenCollectionOptions struct {
	Search string
	Limit  int
}

// NewLinkwardenClient validates and normalizes rawURL and returns a client.
func NewLinkwardenClient(rawURL, token string) (*LinkwardenClient, error) {
	base, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("linkwarden token is required")
	}
	return &LinkwardenClient{baseURL: base, token: token, hc: &http.Client{Timeout: linkwardenTimeout}}, nil
}

// NormalizeBaseURL accepts a user-entered instance URL (with or without a
// scheme and trailing "/api/v1") and returns a clean origin to append API paths
// to.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("linkwarden url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid linkwarden url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported linkwarden url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("linkwarden url has no host")
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/api/v1")
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// Check verifies the instance URL and token with a lightweight authenticated
// request (a no-op search). It returns nil when the instance accepts the
// credentials, or an error describing the failure.
func (c *LinkwardenClient) Check(ctx context.Context) error {
	q := url.Values{}
	q.Set("searchQueryString", "linkwarden")
	endpoint := c.baseURL + "/api/v1/search?" + q.Encode()
	body, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("linkwarden rejected the credentials: HTTP %d: %s", status, firstLine(body))
	}
	return nil
}

// Search queries the instance's search endpoint and returns matching links.
func (c *LinkwardenClient) Search(ctx context.Context, query string) ([]LinkwardenLink, error) {
	return c.SearchOptions(ctx, LinkwardenListOptions{Query: query, Limit: linkwardenMaxLimit})
}

// SearchOptions searches or lists links with sorting, paging and tag/collection
// filters. An empty Query lists all visible links in the requested order, so
// sort=newest&limit=5 returns the five most recently saved links.
func (c *LinkwardenClient) SearchOptions(ctx context.Context, opts LinkwardenListOptions) ([]LinkwardenLink, error) {
	sortValue, err := linkwardenSortValue(opts.Sort)
	if err != nil {
		return nil, err
	}
	limit := clampLimit(opts.Limit)
	q := url.Values{}
	q.Set("sort", strconv.Itoa(sortValue))
	if s := strings.TrimSpace(opts.Query); s != "" {
		q.Set("searchQueryString", s)
	}
	if opts.TagID > 0 {
		q.Set("tagId", strconv.FormatInt(opts.TagID, 10))
	}
	if opts.CollectionID > 0 {
		q.Set("collectionId", strconv.FormatInt(opts.CollectionID, 10))
	}
	if opts.PinnedOnly {
		q.Set("pinnedOnly", "true")
	}

	// Linkwarden combines tagId with its other filters as an OR, so a tag-only
	// query can return untagged links. Re-check tag/collection membership here
	// and page forward (bounded) until enough matches are found.
	matches := func(l LinkwardenLink) bool {
		if opts.TagID > 0 && !linkHasTag(l, opts.TagID) {
			return false
		}
		if opts.CollectionID > 0 && (l.Collection == nil || l.Collection.ID != opts.CollectionID) {
			return false
		}
		return true
	}

	out := make([]LinkwardenLink, 0, limit)
	cursor := int64(0)
	for page := 0; page < linkwardenMaxScanPages; page++ {
		links, next, err := c.searchPage(ctx, q, cursor)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if matches(l) {
				out = append(out, l)
				if len(out) >= limit {
					return out, nil
				}
			}
		}
		if next == nil || *next == 0 || *next == cursor || len(links) == 0 {
			break
		}
		cursor = *next
	}
	return out, nil
}

// searchPage fetches one page of results. cursor is Linkwarden's opaque
// nextCursor (an offset with Meilisearch, a link id with the SQL fallback).
func (c *LinkwardenClient) searchPage(ctx context.Context, q url.Values, cursor int64) ([]LinkwardenLink, *int64, error) {
	params := url.Values{}
	for k, v := range q {
		params[k] = append([]string(nil), v...)
	}
	if cursor > 0 {
		params.Set("cursor", strconv.FormatInt(cursor, 10))
	}
	endpoint := c.baseURL + "/api/v1/search?" + params.Encode()
	body, status, err := c.doLimit(ctx, http.MethodGet, endpoint, nil, linkwardenListBodyLimit)
	if err != nil {
		return nil, nil, err
	}
	if status < 200 || status > 299 {
		return nil, nil, fmt.Errorf("linkwarden search failed: HTTP %d: %s", status, firstLine(body))
	}
	return parseSearchPage(body)
}

// linkHasTag reports whether a link carries the tag with the given id.
func linkHasTag(l LinkwardenLink, tagID int64) bool {
	for _, tag := range l.Tags {
		if tag.ID == tagID {
			return true
		}
	}
	return false
}

// Tags returns the user's tags, newest/name/count sorted as requested.
func (c *LinkwardenClient) Tags(ctx context.Context, opts LinkwardenTagOptions) ([]LinkwardenTag, error) {
	sortValue, err := linkwardenTagSortValue(opts.Sort)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("sort", strconv.Itoa(sortValue))
	if s := strings.TrimSpace(opts.Search); s != "" {
		q.Set("search", s)
	}
	endpoint := c.baseURL + "/api/v1/tags?" + q.Encode()
	body, status, err := c.doLimit(ctx, http.MethodGet, endpoint, nil, linkwardenListBodyLimit)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("linkwarden tags failed: HTTP %d: %s", status, firstLine(body))
	}
	tags, err := parseTagsResponse(body)
	if err != nil {
		return nil, err
	}
	return limitTags(tags, opts.Limit), nil
}

// Collections returns the user's collections (folders), sorted by full path.
// The instance returns all collections at once (no pagination).
func (c *LinkwardenClient) Collections(ctx context.Context, opts LinkwardenCollectionOptions) ([]LinkwardenCollection, error) {
	endpoint := c.baseURL + "/api/v1/collections"
	body, status, err := c.doLimit(ctx, http.MethodGet, endpoint, nil, linkwardenListBodyLimit)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("linkwarden collections failed: HTTP %d: %s", status, firstLine(body))
	}
	collections, err := parseCollectionsResponse(body)
	if err != nil {
		return nil, err
	}
	if s := strings.ToLower(strings.TrimSpace(opts.Search)); s != "" {
		kept := collections[:0]
		for _, col := range collections {
			if strings.Contains(strings.ToLower(col.Name), s) {
				kept = append(kept, col)
			}
		}
		collections = kept
	}
	sort.SliceStable(collections, func(i, j int) bool {
		return strings.ToLower(collections[i].Name) < strings.ToLower(collections[j].Name)
	})
	return limitCollections(collections, opts.Limit), nil
}

// Save adds a link to the instance. name is optional; when empty Linkwarden
// derives a title from the page.
func (c *LinkwardenClient) Save(ctx context.Context, rawURL, name string) (LinkwardenLink, error) {
	payload := map[string]any{"url": strings.TrimSpace(rawURL)}
	if strings.TrimSpace(name) != "" {
		payload["name"] = strings.TrimSpace(name)
	}
	enc, err := json.Marshal(payload)
	if err != nil {
		return LinkwardenLink{}, err
	}
	endpoint := c.baseURL + "/api/v1/links"
	body, status, err := c.do(ctx, http.MethodPost, endpoint, enc)
	if err != nil {
		return LinkwardenLink{}, err
	}
	if status < 200 || status > 299 {
		return LinkwardenLink{}, fmt.Errorf("linkwarden save failed: HTTP %d: %s", status, firstLine(body))
	}
	// Success is {"response": <link>}; failures embed a message string instead.
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return LinkwardenLink{}, fmt.Errorf("decode linkwarden response: %w", err)
	}
	if len(env.Response) == 0 {
		return LinkwardenLink{}, errors.New("linkwarden returned an empty response")
	}
	var link LinkwardenLink
	if err := json.Unmarshal(env.Response, &link); err != nil || link.URL == "" {
		var msg string
		if json.Unmarshal(env.Response, &msg) == nil && msg != "" {
			return LinkwardenLink{}, fmt.Errorf("linkwarden: %s", msg)
		}
		return LinkwardenLink{}, errors.New("linkwarden returned an unexpected response")
	}
	return link, nil
}

// do issues one request with the bearer token and returns the body (capped) and
// status code. Transport errors are wrapped; HTTP error statuses are returned
// to the caller for message shaping.
func (c *LinkwardenClient) do(ctx context.Context, method, endpoint string, body []byte) ([]byte, int, error) {
	return c.doLimit(ctx, method, endpoint, body, 1<<20)
}

// doLimit is do with an explicit response-body cap.
func (c *LinkwardenClient) doLimit(ctx context.Context, method, endpoint string, body []byte, limit int64) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, linkwardenTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("linkwarden request failed: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return out, resp.StatusCode, nil
}

// parseSearchPage accepts both documented search shapes:
// {"data":{"links":[...], "nextCursor":N}} and {"data":[...]}, and returns the
// next cursor when the response carries one.
func parseSearchPage(body []byte) ([]LinkwardenLink, *int64, error) {
	var env struct {
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, nil, fmt.Errorf("decode linkwarden search: %w", err)
	}
	if len(env.Data) == 0 {
		return nil, nil, nil
	}
	var wrapped struct {
		Links      []LinkwardenLink `json:"links"`
		NextCursor *int64           `json:"nextCursor"`
	}
	if err := json.Unmarshal(env.Data, &wrapped); err == nil && wrapped.Links != nil {
		return wrapped.Links, wrapped.NextCursor, nil
	}
	var list []LinkwardenLink
	if err := json.Unmarshal(env.Data, &list); err == nil {
		return list, nil, nil
	}
	return nil, nil, errors.New("linkwarden returned an unexpected search response")
}

// parseTagsResponse accepts the list envelopes Linkwarden has used for tags:
// {"data":{"tags":[...]}}, {"data":[...]} and {"response":[...]}.
func parseTagsResponse(body []byte) ([]LinkwardenTag, error) {
	var env struct {
		Data     json.RawMessage `json:"data"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode linkwarden tags: %w", err)
	}
	raw := env.Data
	if len(raw) == 0 {
		raw = env.Response
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var wrapped struct {
		Tags []LinkwardenTag `json:"tags"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Tags != nil {
		return wrapped.Tags, nil
	}
	var list []LinkwardenTag
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	return nil, errors.New("linkwarden returned an unexpected tags response")
}

// parseCollectionsResponse accepts {"response":[...]} (the collections route)
// and newer {"data":{"collections":[...]}} / {"data":[...]} shapes.
func parseCollectionsResponse(body []byte) ([]LinkwardenCollection, error) {
	var env struct {
		Data     json.RawMessage `json:"data"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode linkwarden collections: %w", err)
	}
	raw := env.Response
	if len(raw) == 0 {
		raw = env.Data
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var wrapped struct {
		Collections []LinkwardenCollection `json:"collections"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Collections != nil {
		return wrapped.Collections, nil
	}
	var list []LinkwardenCollection
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	return nil, errors.New("linkwarden returned an unexpected collections response")
}

// linkwardenSortValue maps the model-facing sort name to Linkwarden's Sort
// enum (0 newest, 1 oldest, 2 name A-Z, 3 name Z-A).
func linkwardenSortValue(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "newest", "recent", "date_newest", "date_newest_first":
		return 0, nil
	case "oldest", "date_oldest", "date_oldest_first":
		return 1, nil
	case "name", "name_asc", "az", "a-z":
		return 2, nil
	case "name_desc", "za", "z-a":
		return 3, nil
	default:
		return 0, fmt.Errorf("unknown sort %q (use newest, oldest, name_asc or name_desc)", s)
	}
}

// linkwardenTagSortValue maps the model-facing sort name to Linkwarden's
// TagSort enum (0 newest, 1 oldest, 2 name A-Z, 3 name Z-A, 4 most links,
// 5 fewest links).
func linkwardenTagSortValue(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "name", "name_asc", "az", "a-z":
		return 2, nil
	case "newest", "recent":
		return 0, nil
	case "oldest":
		return 1, nil
	case "name_desc", "za", "z-a":
		return 3, nil
	case "count_desc", "most", "most_links":
		return 4, nil
	case "count_asc", "fewest", "fewest_links":
		return 5, nil
	default:
		return 0, fmt.Errorf("unknown tag sort %q (use name_asc, name_desc, newest, oldest, count_desc or count_asc)", s)
	}
}

func clampLimit(n int) int {
	if n <= 0 {
		return linkwardenDefaultLimit
	}
	if n > linkwardenMaxLimit {
		return linkwardenMaxLimit
	}
	return n
}

func limitTags(tags []LinkwardenTag, n int) []LinkwardenTag {
	n = clampLimit(n)
	if len(tags) <= n {
		return tags
	}
	return tags[:n]
}

func limitCollections(cols []LinkwardenCollection, n int) []LinkwardenCollection {
	n = clampLimit(n)
	if len(cols) <= n {
		return cols
	}
	return cols[:n]
}

// resolveTagID finds a tag by name (exact case-insensitive match preferred, a
// unique substring otherwise) so a tool call can filter by human-readable tag.
func (c *LinkwardenClient) resolveTagID(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	tags, err := c.Tags(ctx, LinkwardenTagOptions{Search: name, Limit: linkwardenMaxLimit})
	if err != nil {
		return 0, err
	}
	var partial []string
	for _, tag := range tags {
		if strings.EqualFold(tag.Name, name) {
			return tag.ID, nil
		}
		partial = append(partial, tag.Name)
	}
	if len(tags) == 1 {
		return tags[0].ID, nil
	}
	if len(partial) > 0 {
		return 0, fmt.Errorf("tag %q not found; matching tags: %s", name, strings.Join(partial, ", "))
	}
	return 0, fmt.Errorf("tag %q not found", name)
}

// resolveCollectionID finds a collection by name the same way resolveTagID
// resolves tags.
func (c *LinkwardenClient) resolveCollectionID(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	cols, err := c.Collections(ctx, LinkwardenCollectionOptions{Search: name, Limit: linkwardenMaxLimit})
	if err != nil {
		return 0, err
	}
	var partial []string
	for _, col := range cols {
		if strings.EqualFold(col.Name, name) {
			return col.ID, nil
		}
		partial = append(partial, col.Name)
	}
	if len(cols) == 1 {
		return cols[0].ID, nil
	}
	if len(partial) > 0 {
		return 0, fmt.Errorf("collection %q not found; matching collections: %s", name, strings.Join(partial, ", "))
	}
	return 0, fmt.Errorf("collection %q not found", name)
}

// formatLink renders one link for the model: a title/URL line, an optional
// metadata line (folder, tags, save date, pinned) and an optional description.
func formatLink(l LinkwardenLink) string {
	name := strings.TrimSpace(l.Name)
	if name == "" {
		name = l.URL
	}
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(name)
	if l.URL != "" {
		b.WriteString(" — ")
		b.WriteString(l.URL)
	}
	var meta []string
	if l.Collection != nil && strings.TrimSpace(l.Collection.Name) != "" {
		meta = append(meta, "folder: "+strings.TrimSpace(l.Collection.Name))
	}
	if len(l.Tags) > 0 {
		names := make([]string, 0, len(l.Tags))
		for _, t := range l.Tags {
			if n := strings.TrimSpace(t.Name); n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			meta = append(meta, "tags: "+strings.Join(names, ", "))
		}
	}
	if date := linkwardenDate(l.CreatedAt); date != "" {
		meta = append(meta, "saved: "+date)
	}
	if len(l.PinnedBy) > 0 {
		meta = append(meta, "pinned")
	}
	if len(meta) > 0 {
		b.WriteString("\n  ")
		b.WriteString(strings.Join(meta, " · "))
	}
	if desc := strings.TrimSpace(l.Description); desc != "" {
		b.WriteString("\n  ")
		b.WriteString(desc)
	}
	return b.String()
}

// linkwardenDate keeps the date part of an RFC3339 timestamp; other values are
// returned unchanged.
func linkwardenDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10]
	}
	return s
}

// collectionPath renders "Parent / Child" for a collection, guarding against
// cycles in malformed data.
func collectionPath(all map[int64]LinkwardenCollection, col LinkwardenCollection) string {
	parts := []string{strings.TrimSpace(col.Name)}
	parent := col.ParentID
	seen := map[int64]bool{col.ID: true}
	for parent != nil && !seen[*parent] {
		p, ok := all[*parent]
		if !ok {
			break
		}
		seen[p.ID] = true
		parts = append([]string{strings.TrimSpace(p.Name)}, parts...)
		parent = p.ParentID
	}
	return strings.Join(parts, " / ")
}

// ---------- tools ----------

// LinkwardenSearchTool searches the configured Linkwarden instance.
type LinkwardenSearchTool struct{ c *LinkwardenClient }

// NewLinkwardenSearchTool wraps c as a linkwarden_search tool.
func NewLinkwardenSearchTool(c *LinkwardenClient) *LinkwardenSearchTool {
	return &LinkwardenSearchTool{c: c}
}

func (t *LinkwardenSearchTool) Name() string { return "linkwarden_search" }

func (t *LinkwardenSearchTool) Description() string {
	return "Search or browse the user's saved Linkwarden bookmarks. Use this " +
		"when the user asks about links they have already saved, wants to find a " +
		"previously bookmarked page, wants the most recent saves (sort \"newest\"," +
		" e.g. limit 5), or wants results ordered by name. Optionally filter by " +
		"tag name or collection (folder) name. Results include tags, folder and " +
		"save date."
}

func (t *LinkwardenSearchTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Optional text to search saved links for. Omit to list links (e.g. the latest 5).",
			},
			"sort": map[string]any{
				"type":        "string",
				"enum":        []string{"newest", "oldest", "name_asc", "name_desc"},
				"description": "Result order: newest (default), oldest, name_asc or name_desc.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of links to return (default 10, max 50).",
			},
			"tag": map[string]any{
				"type":        "string",
				"description": "Optional tag name to restrict results to (use linkwarden_tags to discover tags).",
			},
			"collection": map[string]any{
				"type":        "string",
				"description": "Optional collection (folder) name to restrict results to (use linkwarden_collections to discover them).",
			},
			"pinned": map[string]any{
				"type":        "boolean",
				"description": "Only return pinned links.",
			},
		},
	})
}

func (t *LinkwardenSearchTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query      string `json:"query"`
		Sort       string `json:"sort"`
		Limit      int    `json:"limit"`
		Tag        string `json:"tag"`
		Collection string `json:"collection"`
		Pinned     bool   `json:"pinned"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	opts := LinkwardenListOptions{
		Query:      strings.TrimSpace(in.Query),
		Sort:       in.Sort,
		Limit:      in.Limit,
		PinnedOnly: in.Pinned,
	}
	if tag := strings.TrimSpace(in.Tag); tag != "" {
		id, err := t.c.resolveTagID(ctx, tag)
		if err != nil {
			return "", err
		}
		opts.TagID = id
	}
	if col := strings.TrimSpace(in.Collection); col != "" {
		id, err := t.c.resolveCollectionID(ctx, col)
		if err != nil {
			return "", err
		}
		opts.CollectionID = id
	}
	links, err := t.c.SearchOptions(ctx, opts)
	if err != nil {
		return "", err
	}
	if len(links) == 0 {
		if opts.Query == "" {
			return "No saved links found.", nil
		}
		return "No saved links matched.", nil
	}
	var b strings.Builder
	for i, l := range links {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(formatLink(l))
	}
	return b.String(), nil
}

// LinkwardenTagsTool lists the user's tags.
type LinkwardenTagsTool struct{ c *LinkwardenClient }

// NewLinkwardenTagsTool wraps c as a linkwarden_tags tool.
func NewLinkwardenTagsTool(c *LinkwardenClient) *LinkwardenTagsTool {
	return &LinkwardenTagsTool{c: c}
}

func (t *LinkwardenTagsTool) Name() string { return "linkwarden_tags" }

func (t *LinkwardenTagsTool) Description() string {
	return "List the user's Linkwarden tags (labels) with how many saved links " +
		"use each. Use this to discover tags before filtering linkwarden_search " +
		"by tag."
}

func (t *LinkwardenTagsTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"search": map[string]any{
				"type":        "string",
				"description": "Optional text to filter tag names.",
			},
			"sort": map[string]any{
				"type":        "string",
				"enum":        []string{"name_asc", "name_desc", "newest", "oldest", "count_desc", "count_asc"},
				"description": "Tag order: name_asc (default), name_desc, newest, oldest, count_desc (most links) or count_asc.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of tags to return (default 10, max 50).",
			},
		},
	})
}

func (t *LinkwardenTagsTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Search string `json:"search"`
		Sort   string `json:"sort"`
		Limit  int    `json:"limit"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	tags, err := t.c.Tags(ctx, LinkwardenTagOptions{
		Search: strings.TrimSpace(in.Search),
		Sort:   in.Sort,
		Limit:  in.Limit,
	})
	if err != nil {
		return "", err
	}
	if len(tags) == 0 {
		return "No Linkwarden tags found.", nil
	}
	var b strings.Builder
	for i, tag := range tags {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %s", strings.TrimSpace(tag.Name))
		if tag.Links > 0 {
			fmt.Fprintf(&b, " (%d links)", tag.Links)
		}
	}
	return b.String(), nil
}

// LinkwardenCollectionsTool lists the user's collections (folders).
type LinkwardenCollectionsTool struct{ c *LinkwardenClient }

// NewLinkwardenCollectionsTool wraps c as a linkwarden_collections tool.
func NewLinkwardenCollectionsTool(c *LinkwardenClient) *LinkwardenCollectionsTool {
	return &LinkwardenCollectionsTool{c: c}
}

func (t *LinkwardenCollectionsTool) Name() string { return "linkwarden_collections" }

func (t *LinkwardenCollectionsTool) Description() string {
	return "List the user's Linkwarden collections (folders), including nested " +
		"folders and link counts. Use this to discover folders before filtering " +
		"linkwarden_search by collection."
}

func (t *LinkwardenCollectionsTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"search": map[string]any{
				"type":        "string",
				"description": "Optional text to filter collection names.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of collections to return (default 10, max 50).",
			},
		},
	})
}

func (t *LinkwardenCollectionsTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Search string `json:"search"`
		Limit  int    `json:"limit"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	// Resolve parents from the unfiltered list so paths stay complete.
	all, err := t.c.Collections(ctx, LinkwardenCollectionOptions{Limit: linkwardenMaxLimit})
	if err != nil {
		return "", err
	}
	byID := make(map[int64]LinkwardenCollection, len(all))
	for _, col := range all {
		byID[col.ID] = col
	}
	cols := all
	if s := strings.ToLower(strings.TrimSpace(in.Search)); s != "" {
		filtered := make([]LinkwardenCollection, 0, len(cols))
		for _, col := range cols {
			if strings.Contains(strings.ToLower(col.Name), s) {
				filtered = append(filtered, col)
			}
		}
		cols = filtered
	}
	cols = limitCollections(cols, in.Limit)
	if len(cols) == 0 {
		return "No Linkwarden collections found.", nil
	}
	var b strings.Builder
	for i, col := range cols {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %s", collectionPath(byID, col))
		var notes []string
		if col.Links > 0 {
			notes = append(notes, fmt.Sprintf("%d links", col.Links))
		}
		if col.IsPublic {
			notes = append(notes, "public")
		}
		if len(notes) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(notes, ", "))
		}
	}
	return b.String(), nil
}

// LinkwardenSaveTool saves a URL to the configured Linkwarden instance.
type LinkwardenSaveTool struct{ c *LinkwardenClient }

// NewLinkwardenSaveTool wraps c as a linkwarden_save tool.
func NewLinkwardenSaveTool(c *LinkwardenClient) *LinkwardenSaveTool {
	return &LinkwardenSaveTool{c: c}
}

func (t *LinkwardenSaveTool) Name() string { return "linkwarden_save" }

func (t *LinkwardenSaveTool) Description() string {
	return "Save a URL to the user's Linkwarden bookmarks. Use this when the user " +
		"asks to save, bookmark, or add a link to Linkwarden."
}

func (t *LinkwardenSaveTool) Parameters() json.RawMessage {
	return schemaObj(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The absolute http(s) URL to save.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Optional title for the saved link.",
			},
		},
		"required": []string{"url"},
	})
}

func (t *LinkwardenSaveTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := DecodeArgs(args, &in); err != nil {
		return "", err
	}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("a valid absolute http(s) url is required")
	}
	link, err := t.c.Save(ctx, rawURL, in.Title)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(link.Name)
	if name == "" {
		name = rawURL
	}
	return fmt.Sprintf("Saved to Linkwarden: %s (%s)", name, link.URL), nil
}
