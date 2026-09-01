package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Client is a thin authenticated HTTP client for the NetBox REST API. The
// underlying *http.Client is supplied by the caller — in production it is built
// from the Grafana SDK (backend/httpclient) using the datasource instance
// settings, so Grafana's proxy/TLS/timeout config and Private Data Source
// Connect (PDC) are honored automatically.
type Client struct {
	base  string // base URL without trailing slash, e.g. https://netbox.example.com
	token string
	http  *http.Client

	// branchNames/branchIDs cache the netbox-branching branch list so the Branch
	// field can accept a name or a schema id. Both empty after a failed/absent
	// fetch (e.g. branching not installed), in which case values pass through.
	branchMu     sync.Mutex
	branchNames  map[string]string // branch name -> schema id
	branchIDs    map[string]bool   // known schema ids
	branchExpiry time.Time
}

// NewClient builds a NetBox API client over the given HTTP client. base may
// include or omit a trailing "/api"; it is normalized to the instance root.
func NewClient(base, token string, httpClient *http.Client) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/api")
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{base: base, token: token, http: httpClient}
}

// BaseURL returns the instance root URL.
func (c *Client) BaseURL() string { return c.base }

// v2TokenPrefix is the NetBox v2 (hashed) API token prefix (TOKEN_PREFIX). v2
// tokens authenticate with the "Bearer" scheme; classic v1 tokens use "Token".
const v2TokenPrefix = "nbt_"

// authHeader returns the Authorization header value for a NetBox API token,
// auto-detecting the token version. A user may also paste a value that already
// includes the scheme ("Bearer …"/"Token …"); that is passed through verbatim.
func authHeader(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "Bearer ") || strings.HasPrefix(token, "Token ") {
		return token
	}
	if strings.HasPrefix(token, v2TokenPrefix) {
		return "Bearer " + token
	}
	return "Token " + token
}

// apiURL builds an absolute API URL from a path relative to /api, with optional
// query parameters.
func (c *Client) apiURL(path string, query url.Values) string {
	path = strings.Trim(path, "/")
	u := c.base + "/api/"
	if path != "" {
		u += path + "/"
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// getJSON performs an authenticated GET and decodes the JSON body into out.
func (c *Client) getJSON(ctx context.Context, rawURL string, out interface{}) error {
	body, err := c.getBytes(ctx, rawURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", truncateURL(rawURL), err)
	}
	return nil
}

// getListPage fetches one page of a list endpoint, tolerating both the standard
// DRF paginated envelope ({count,next,results}) and endpoints that return a bare
// JSON array of objects (e.g. /api/plugins/installed-plugins/). A bare array is
// normalized to a single page: Results = the array, Next = nil, Count = len.
func (c *Client) getListPage(ctx context.Context, rawURL string) (listPage, error) {
	body, err := c.getBytes(ctx, rawURL)
	if err != nil {
		return listPage{}, err
	}
	var page listPage
	envErr := json.Unmarshal(body, &page)
	if envErr == nil {
		return page, nil
	}
	// Some endpoints return a bare JSON array instead of the envelope. There is
	// no pagination cursor, so Count = len(arr) assumes the array is the full
	// result set (true for known array endpoints, which ignore ?limit).
	var arr []json.RawMessage
	if json.Unmarshal(body, &arr) == nil {
		n := len(arr)
		return listPage{Count: &n, Results: arr}, nil
	}
	return listPage{}, fmt.Errorf("decode %s: %w", truncateURL(rawURL), envErr)
}

// getBytes performs an authenticated GET and returns the raw response body.
func (c *Client) getBytes(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", authHeader(c.token))
	}
	req.Header.Set("Accept", "application/json")
	if branch := provider.BranchFromContext(ctx); branch != "" && ctx.Value(noBranchResolveKey{}) == nil {
		// A resolved value of "" means the default (main) branch — send no header.
		if resolved := c.resolveBranch(ctx, branch); resolved != "" {
			req.Header.Set("X-NetBox-Branch", resolved)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// No rawURL in the format string. http.Client.Do returns a *url.Error,
		// whose own Error() already renders `Get "<rawURL>": <cause>` — adding it
		// here embedded the URL TWICE. On a batched ip-enrichment hop that is two
		// copies of a ~6 KB request line, measured at 12,480 characters with
		// "address=" appearing 632 times and the actual cause ("connection
		// refused") at the very tail, and it reached the user as a Grafana toast.
		// Bounding the user-facing string is pkg/plugin.upstreamDetail's job; not
		// doubling it first is this one's.
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		// io.ReadAll's error carries no URL of its own, so unlike the Do path
		// above this one has to name the request — but through truncateURL, for
		// the same reason APIError does.
		return nil, fmt.Errorf("read body %s: %w", truncateURL(rawURL), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, URL: rawURL, Body: snippet(body)}
	}
	return body, nil
}

// branchTTL bounds how long the branch name -> schema id map is cached.
const branchTTL = 5 * time.Minute

// branchRetryTTL is a short negative cache after a transient branch-list failure
// (timeout/5xx), so a blip doesn't block resolution for the full branchTTL.
const branchRetryTTL = 15 * time.Second

// maxBranchPages caps branch-list pagination (50 pages of 1000 is far beyond any
// real deployment) so a malformed self-referential "next" can't loop forever.
const maxBranchPages = 50

// noBranchResolveKey marks a context whose requests must skip branch resolution
// and the X-NetBox-Branch header. It is set on the internal branch-list fetch so
// that fetch targets main and does not recurse back into resolveBranch (which
// would deadlock on branchMu). WithBranch can't clear an existing branch (a
// blank id is a no-op), so this flag is how the fetch opts out.
type noBranchResolveKey struct{}

// resolveBranch maps a Branch field value to a netbox-branching schema id.
// NetBox's X-NetBox-Branch header only accepts the schema id, but users expect
// to use the branch name: a name resolves to its schema id; a value that is
// already a schema id (or is unknown, or branching isn't installed) passes
// through unchanged so NetBox makes the final call.
func (c *Client) resolveBranch(ctx context.Context, value string) string {
	// "" and "main"/"Main" mean the default branch (NetBox-branching's base, which
	// is addressed by sending NO header). NetBox does not list the default as a
	// branch, and branch names are not unique, so treat the reserved default
	// keyword as the base rather than trying to resolve it to a schema id.
	if value == "" || strings.EqualFold(value, "main") {
		return ""
	}
	c.branchMu.Lock()
	defer c.branchMu.Unlock()
	if c.branchNames == nil || time.Now().After(c.branchExpiry) {
		if names, ids, ok := c.fetchBranches(ctx); ok {
			c.branchNames, c.branchIDs = names, ids
			c.branchExpiry = time.Now().Add(branchTTL)
		} else {
			// Transient failure (timeout/5xx): keep any prior cache and retry
			// soon rather than blocking resolution for the full branchTTL.
			if c.branchNames == nil {
				c.branchNames, c.branchIDs = map[string]string{}, map[string]bool{}
			}
			c.branchExpiry = time.Now().Add(branchRetryTTL)
		}
	}
	// A known schema id wins over a name: a branch name can collide with another
	// branch's schema id (both are short alphanumerics), and the schema id is the
	// canonical, unambiguous identifier, so honor it as-is first.
	if c.branchIDs[value] {
		return value
	}
	if id, ok := c.branchNames[value]; ok {
		return id
	}
	return value
}

// fetchBranches reads the branch list into a name -> schema id map and a set of
// known schema ids. The fetch carries no branch header (the list lives on main)
// and is flagged to skip resolution, so it does not recurse through
// resolveBranch. Returns empty maps if branching isn't installed or the list
// can't be read; callers then pass values through.
func (c *Client) fetchBranches(ctx context.Context) (map[string]string, map[string]bool, bool) {
	names := map[string]string{}
	ids := map[string]bool{}
	ctx = context.WithValue(ctx, noBranchResolveKey{}, struct{}{})
	next := c.apiURL("plugins/branching/branches", url.Values{"limit": {"1000"}})
	// Follow pagination so a name on a later page still resolves; the page cap
	// bounds a pathological self-referential "next".
	for i := 0; next != "" && i < maxBranchPages; i++ {
		page, err := c.getListPage(ctx, next)
		if err != nil {
			// 404 means branching isn't installed: an authoritative empty result
			// (cache it). Anything else (5xx/timeout/network) is transient, so
			// report failure and let the caller retry soon.
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				return names, ids, true
			}
			return names, ids, false
		}
		for _, raw := range page.Results {
			var b struct {
				Name     string `json:"name"`
				SchemaID string `json:"schema_id"`
			}
			if json.Unmarshal(raw, &b) == nil && b.SchemaID != "" {
				ids[b.SchemaID] = true
				if b.Name != "" {
					names[b.Name] = b.SchemaID
				}
			}
		}
		if page.Next == nil {
			break
		}
		next = *page.Next
	}
	return names, ids, true
}

// BranchingInstalled probes the netbox-branching branches endpoint to detect
// whether the plugin is installed. It reuses the exact signal fetchBranches /
// resolveBranch rely on (an authoritative 404 on that endpoint means branching
// is absent), so branch-field gating and branch resolution always agree.
// Returns (installed, conclusive):
//   - (true,  true):  the endpoint responded 2xx — branching is present.
//   - (false, true):  an authoritative 404 — branching is absent.
//   - (false, false): any other failure (5xx / auth / timeout / network) —
//     inconclusive; callers must not treat this as absent.
//
// The probe carries no branch header (targets main) and skips branch
// resolution via noBranchResolveKey, mirroring fetchBranches, so it cannot
// recurse into resolveBranch or deadlock on branchMu.
func (c *Client) BranchingInstalled(ctx context.Context) (installed bool, conclusive bool) {
	ctx = context.WithValue(ctx, noBranchResolveKey{}, struct{}{})
	_, err := c.getBytes(ctx, c.apiURL("plugins/branching/branches", url.Values{"limit": {"1"}}))
	if err == nil {
		return true, true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return false, true
	}
	return false, false
}

// APIError represents a non-2xx response from NetBox.
type APIError struct {
	Status int
	URL    string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("netbox API %d for %s: %s", e.Status, truncateURL(e.URL), e.Body)
}

// truncateURL keeps the error message readable when the failed request was a
// batched one. IP enrichment builds ~6 KB request lines (hundreds of repeated
// ?address= / ?id= parameters), and this message reaches the user as a Grafana
// error toast; the endpoint and the first parameters identify the request, the
// remaining kilobytes only bury it. The kept length matches snippet()'s cap on
// the body for the same reason.
func truncateURL(u string) string {
	const max = 300
	if len(u) <= max {
		return u
	}
	return u[:max] + "…"
}

func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// listPage is one page of a paginated NetBox list response. Results are kept as
// raw JSON so object key order is preserved for flattening.
type listPage struct {
	// Count is the number of objects matching the query, from the envelope.
	//
	// It is a POINTER because NetBox has two pagination modes and only one of
	// them answers the question. In the default offset mode the envelope always
	// carries a number. In cursor mode (?start=<pk>, NetBox 4.6+) NetBoxPagination
	// never calls .count() and serializes `"count": null` — "not answered", which
	// is a different statement from "nothing matched". Decoding both into a plain
	// int would collapse them into 0 and turn "how many devices are offline" into
	// zero with no error anywhere, so the two shapes are kept distinguishable at
	// the only place that can still tell them apart. Read it through total().
	Count   *int              `json:"count"`
	Next    *string           `json:"next"`
	Results []json.RawMessage `json:"results"`
}

// total returns the envelope's match count and whether the envelope reported one
// at all. known == false means the source declined to count (cursor mode); it is
// never the same as a reported zero.
func (p listPage) total() (int, bool) {
	if p.Count == nil {
		return 0, false
	}
	return *p.Count, true
}

// Classification reports this failure in the seam's backend-agnostic terms.
//
// The body inspection lives HERE, not in the plugin layer, because only this
// package knows what NetBox's bodies mean — and because the body must not cross
// the seam: it carries the request URL and up to 300 characters of upstream
// response, neither of which belongs anywhere near a user-facing string.
// Everything this returns is safe to render.
func (e *APIError) Classification() *provider.UpstreamError {
	c := &provider.UpstreamError{Status: e.Status, Kind: provider.ErrorKindUpstream}
	switch e.Status {
	case 400:
		// netbox-branching rejects an unknown branch with this exact 400. The
		// Branch field accepts a branch name or schema id (names resolve to the
		// schema id); a 400 here means neither matched a real branch.
		if strings.Contains(e.Body, "Invalid branch identifier") {
			c.Kind = provider.ErrorKindInvalidBranch
			return c
		}
		c.Kind = provider.ErrorKindBadRequest
	case 401, 403:
		c.Kind = provider.ErrorKindAuth
	case 404:
		c.Kind = provider.ErrorKindNotFound
	case 405:
		c.Kind = provider.ErrorKindNotListable
	case 500:
		// QuerySetNotOrdered appears near the start of NetBox's error body, well
		// within snippet()'s 300-char cap. A longer body (e.g. a debug traceback)
		// could push the token past the cap; the match then falls through to the
		// generic upstream kind — still safe, just less specific.
		if strings.Contains(e.Body, "QuerySetNotOrdered") {
			c.Kind = provider.ErrorKindNotOrderable
		}
	}
	return c
}
