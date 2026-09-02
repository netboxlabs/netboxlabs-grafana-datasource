package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

const (
	// pageSize is the rows requested per upstream call.
	//
	// The published spec says limit is "capped at 10000". Measured against a
	// staging instance it is not: limit=2000 returns 1000 rows, as does
	// limit=5000. Asking for more than the server will give does not fail, it
	// silently returns fewer — so a caller that trusted the documented cap would
	// read 1000 rows and believe it had seen 10000 of them.
	//
	// We therefore page at the size the server actually honours and reach larger
	// limits with the cursor, rather than in one oversized request.
	pageSize = 1000

	// defaultLimit is the row count for a query that does not ask for one. It
	// matches the NetBox provider's default so switching Mode does not silently
	// change how much a saved panel returns.
	defaultLimit = 1000

	// MaxLimit is the most rows a single Query will return, reached by walking
	// the cursor over several pages. It is this provider's own ceiling and is
	// reported on every Result (provider.Result.MaxRows) rather than read off a
	// package constant by the plugin layer.
	MaxLimit = 10000

	// maxSnippet bounds any upstream text kept on an error. Response bodies and
	// URLs never reach user-facing strings (see APIError.Classification), but
	// they do reach logs, and an unbounded body in a log line is its own problem.
	maxSnippet = 300
)

// Client is a thin authenticated HTTP client for the replica-cache read API.
//
// The underlying *http.Client is supplied by the caller: in production it comes
// from the Grafana SDK built against the datasource instance settings, so
// proxy, TLS and timeout configuration are honoured without this package
// knowing about any of it.
type Client struct {
	base     string // base URL without trailing slash
	token    string
	netboxID string
	http     *http.Client
}

// NewClient builds a replica-cache client. base is the service root; a trailing
// slash or "/v1" suffix is tolerated and normalized away, because both are
// plausible things to paste out of a browser.
func NewClient(base, token, netboxID string, httpClient *http.Client) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/v1")
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		base:     base,
		token:    token,
		netboxID: strings.TrimSpace(netboxID),
		http:     httpClient,
	}
}

// BaseURL returns the service root URL.
func (c *Client) BaseURL() string { return c.base }

// APIError is a non-2xx response from replica-cache.
type APIError struct {
	Status int
	URL    string
	// Body is the response snippet. It stays inside this package: see
	// Classification, which is what crosses the provider seam.
	Body string
	// Message is the "error" field the service returns on a failure
	// ({"error":"unknown column: foo"}), when the body was JSON in that shape.
	// It is upstream text and is treated as untrusted, bounded like Body.
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("replica-cache API %d for %s: %s", e.Status, truncate(e.URL), e.Message)
	}
	return fmt.Sprintf("replica-cache API %d for %s: %s", e.Status, truncate(e.URL), e.Body)
}

// Classification reports this failure in the seam's backend-agnostic terms.
//
// The status meanings come from the service's own published spec:
//
//	400  invalid request — in practice an unknown column, an unsupported filter
//	     operator, or an unparseable primary key
//	401  missing or invalid API token
//	403  the NBC-Netbox-ID header does not match the tenant bound to the token
//	404  no such endpoint
//	500  server error
//
// 403 classifies as auth rather than as a generic refusal because its only
// documented cause is a credential/tenant mismatch, which is the same thing the
// reader has to go fix.
func (e *APIError) Classification() *provider.UpstreamError {
	c := &provider.UpstreamError{Status: e.Status, Kind: provider.ErrorKindUpstream}
	switch e.Status {
	case 400:
		c.Kind = provider.ErrorKindBadRequest
	case 401, 403:
		c.Kind = provider.ErrorKindAuth
	case 404:
		c.Kind = provider.ErrorKindNotFound
	}
	return c
}

// errorBody is the service's failure shape: {"error": "unknown column: foo"}.
type errorBody struct {
	Error string `json:"error"`
}

// listPage is one page of a list response. Rows stay as raw JSON so that key
// order survives into the flattened output.
type listPage struct {
	Count      int               `json:"count"`
	NextCursor string            `json:"next_cursor"`
	Results    []json.RawMessage `json:"results"`
}

// get performs an authenticated GET and returns the decoded body.
//
// Both headers are always sent. NBC-Netbox-ID is not optional: without it the
// service answers 400, so omitting it when unset would turn a misconfigured
// datasource into a puzzling bad-request rather than something HealthCheck can
// explain.
func (c *Client) get(ctx context.Context, path string, q url.Values, out interface{}) error {
	raw := c.base + path
	if len(q) > 0 {
		raw += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("NBC-Netbox-ID", c.netboxID)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", truncate(raw), err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response from %s: %w", truncate(raw), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, URL: raw, Body: snippet(body)}
		var eb errorBody
		if json.Unmarshal(body, &eb) == nil {
			apiErr.Message = truncate(strings.TrimSpace(eb.Error))
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", truncate(raw), err)
	}
	return nil
}

// listOnce fetches a single page for an entity.
func (c *Client) listOnce(ctx context.Context, entity string, q url.Values, cursor string) (listPage, error) {
	params := url.Values{}
	for k, vs := range q {
		for _, v := range vs {
			params.Add(k, v)
		}
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	var page listPage
	// url.Values.Encode percent-encodes the brackets in filter[col], which the
	// service requires: a literal "[" is rejected by the edge before it reaches
	// the API.
	if err := c.get(ctx, "/v1/"+entity, params, &page); err != nil {
		return listPage{}, err
	}
	return page, nil
}

// list walks the cursor until it has limit rows, the pages run out, or the
// context is done.
//
// The returned count is the service's own total for the filter, which is
// independent of limit and of how many pages were walked. It is what
// provider.Result.Total carries, and alerting reads it, so it is taken from the
// FIRST page and never recomputed from len(rows) — those two numbers answer
// different questions and conflating them would report a truncated page as the
// whole population.
func (c *Client) list(ctx context.Context, entity string, q url.Values, limit int) ([]json.RawMessage, int, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	var (
		rows   []json.RawMessage
		total  int
		cursor string
		first  = true
	)
	for len(rows) < limit {
		want := limit - len(rows)
		if want > pageSize {
			want = pageSize
		}
		pq := url.Values{}
		for k, vs := range q {
			for _, v := range vs {
				pq.Add(k, v)
			}
		}
		pq.Set("limit", strconv.Itoa(want))

		page, err := c.listOnce(ctx, entity, pq, cursor)
		if err != nil {
			return nil, 0, err
		}
		if first {
			total = page.Count
			first = false
		}
		rows = append(rows, page.Results...)

		// A page that comes back empty or without a cursor is the end of the
		// result set. Both conditions are needed: the service omits the cursor on
		// the last page, and a zero-row page with a cursor would otherwise spin.
		if page.NextCursor == "" || len(page.Results) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total, nil
}

func truncate(s string) string {
	if len(s) <= maxSnippet {
		return s
	}
	return s[:maxSnippet] + "…"
}

func snippet(b []byte) string {
	return truncate(strings.TrimSpace(string(b)))
}
