package replicacache

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

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

const (
	// pageSize is the rows requested per upstream call: the service's maximum
	// page, above which it answers 400 "limit N exceeds the maximum page size
	// of 1000". Larger limits are reached with the cursor.
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
		c.Detail = "Replica cache rejected this query (HTTP 400). Check the filters and try again."
	case 401, 403:
		c.Kind = provider.ErrorKindAuth
		// Names THIS backend's settings. The shared wording says to check the
		// NetBox API token and URL, which this mode does not use at all — so it
		// would send a reader to fix a field that has no bearing on the failure.
		// 403 is a tenant/token mismatch, so the instance ID is as likely to be
		// wrong as the token.
		c.Detail = "Replica cache rejected the credentials. Check the replica-cache token and the NetBox instance ID."
	case 404:
		// Two different 404s. An entity the replica is configured for but has
		// received nothing for says so in the body; the provider normally
		// pre-empts it from the catalogue, but the catalogue can be minutes
		// stale, so the route's own answer classifies the same way.
		if strings.Contains(e.Message, "no data received") {
			c.Kind = provider.ErrorKindNotReplicated
			c.Detail = notReplicatedDetail(e.entity())
			break
		}
		c.Kind = provider.ErrorKindNotFound
		c.Detail = "Replica cache has no such endpoint. Check the replica-cache URL, and that this object type is one the cache serves."
	case 500, 502, 503, 504:
		c.Detail = fmt.Sprintf("Replica cache returned HTTP %d. The cache is reachable but could not answer; this is not a NetBox failure.", e.Status)
	default:
		// Every remaining status still needs to name the right service. Without
		// this, a 429, 405 or 413 kept an empty Detail and both renderers fell
		// through to "NetBox returned HTTP 429" — pointing at a connection this
		// mode may not even have configured.
		c.Detail = fmt.Sprintf("Replica cache returned HTTP %d for this request. The cache is reachable but did not answer it; this is not a NetBox failure.", e.Status)
	}
	return c
}

// entity is the object type the failed URL addressed, for a message that
// names it. Empty when the URL was not a list route.
func (e *APIError) entity() string {
	_, after, ok := strings.Cut(e.URL, "/v1/")
	if !ok {
		return ""
	}
	after, _, _ = strings.Cut(after, "?")
	return after
}

// TransportError is a request that never reached the service — the host is
// unreachable, TLS failed, or it timed out.
//
// It exists to be CLASSIFIED. An ordinary wrapped error is unclassified, and
// the plugin layer's fallback for that is "Cannot reach NetBox", which points
// an operator at the NetBox connection when the thing that failed was the
// cache. Carrying our own sentence is the difference between a useful message
// and one that sends them to the wrong setting.
type TransportError struct {
	Op  string
	Err error
	// Message overrides the default guidance, for a failure that is not about
	// reachability — a 200 whose body will not parse, say.
	Message string
}

func (e *TransportError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Classification reports an unreachable cache, naming the setting to check.
// Status stays 0: nothing answered, so there is no code to report.
func (e *TransportError) Classification() *provider.UpstreamError {
	detail := e.Message
	if detail == "" {
		detail = "Cannot reach replica-cache. Check the replica-cache URL and that the service is reachable from Grafana."
	}
	return &provider.UpstreamError{Kind: provider.ErrorKindUpstream, Detail: detail}
}

// maxBodyBytes caps a single response. The same 64 MiB the NetBox client uses.
// A var so that a test can lower it rather than allocate the real limit.
var maxBodyBytes int64 = 64 << 20

// errOversizedBody and errMissingCount are the protocol violations the client
// refuses outright, kept as sentinels so tests can name what they assert.
var (
	errOversizedBody         = errors.New("response exceeds 64 MiB")
	errMalformedEnvelope     = errors.New(`response envelope is missing "count" or "results"`)
	errMalformedRow          = errors.New("result row is not an object")
	errRowWithoutID          = errors.New("result row has no usable id")
	errDuplicateRow          = errors.New("the same object id appeared twice")
	errRowWithoutField       = errors.New("row is missing the column it was projected onto")
	errMalformedCustomFields = errors.New("custom field data could not be read")
	errInconsistentCount     = errors.New("page holds more rows than its reported total")
	errCursorNotAdvancing    = errors.New("pagination cursor repeated")
	errEmptyCatalogue        = errors.New("catalogue lists no object types")
)

// errorBody is the service's failure shape: {"error": "unknown column: foo"}.
type errorBody struct {
	Error string `json:"error"`
}

// listPage is one page of a list response. Rows stay as raw JSON so that key
// order survives into the flattened output.
// Count and Results are pointers so that an envelope MISSING either is
// distinguishable from one reporting zero rows. The difference matters: Total
// feeds the count query's single number and the "showing N of M" truncation
// notice, which is suppressed when Total is zero. Decoded as plain values, a
// proxy or error page answering {"results":[...]} would present a truncated
// table as the whole population, and one answering {"count":0} would report no
// matches — indistinguishable from a real empty answer, including to an alert
// rule evaluating it.
type listPage struct {
	Count      *int               `json:"count"`
	NextCursor string             `json:"next_cursor"`
	Results    *[]json.RawMessage `json:"results"`
	// DataAsOf is the instant this response reflects: everything committed at
	// or before it is here. Per response, unlike the catalogue's copy, which
	// is up to ten minutes old.
	DataAsOf *string `json:"data_as_of"`
}

// rows is Results with the presence check already done by listOnce.
func (p listPage) rows() []json.RawMessage {
	if p.Results == nil {
		return nil
	}
	return *p.Results
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
		// A URL that will not parse fails here, before anything is sent, and
		// unclassified it renders as "Cannot reach NetBox" — wrong twice over:
		// nothing was attempted, and the setting at fault is the cache's own.
		return &TransportError{
			Op:      "building a request for " + truncate(raw),
			Err:     err,
			Message: "The replica-cache URL could not be used to build a request. Check it for typos — a stray percent sign or an unclosed bracket is enough.",
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("NBC-Netbox-ID", c.netboxID)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return &TransportError{Op: "requesting " + truncate(raw), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap so that an oversized body is rejected rather
	// than silently truncated into a parse error. Matches the NetBox client's
	// limit; without it a single upstream response could exhaust the backend.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		// A body that stops mid-read is the same class of failure: the service
		// never finished answering.
		return &TransportError{Op: "reading response from " + truncate(raw), Err: err}
	}
	if int64(len(body)) > maxBodyBytes {
		return &TransportError{
			Op:      "reading response from " + truncate(raw),
			Err:     errOversizedBody,
			Message: "Replica cache returned a response larger than 64 MiB. Narrow the query with filters or a smaller limit.",
		}
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
		// A 200 carrying something we cannot parse is the service's problem, not
		// the network's — but left unclassified it renders through the plugin's
		// fallback as "Couldn't reach NetBox", which is wrong twice over: we
		// reached it, and it was not NetBox.
		return &TransportError{
			Op:      "decoding response from " + truncate(raw),
			Err:     err,
			Message: "Replica cache returned a response that could not be read. The service is reachable but answered with something unexpected.",
		}
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
	if page.Count != nil && page.Results != nil {
		// Presence is not enough: the two members have to agree. A page holding
		// more rows than the total it reports, or a negative total, is
		// internally contradictory — and the shape that matters is
		// {"count":0,"results":[{…}]}, which a count query would report as no
		// matches while the truncation guard reads Total==0 as "unavailable"
		// rather than as a reason to refuse. An alert would then evaluate a
		// response that contradicts itself.
		if n := *page.Count; n < 0 || len(*page.Results) > n {
			return listPage{}, &TransportError{
				Op:      "reading response from " + truncate(c.base+"/v1/"+entity),
				Err:     errInconsistentCount,
				Message: "Replica cache returned a response whose row count contradicts its total. The service is reachable but answered with something unexpected.",
			}
		}
	}
	if page.Count == nil || page.Results == nil {
		// Valid JSON in the wrong shape — an intermediary's error page, or a
		// different service behind the URL. Accepting it would report zero for
		// count queries and quietly drop the truncation notice.
		return listPage{}, &TransportError{
			Op:      "reading response from " + truncate(c.base+"/v1/"+entity),
			Err:     errMalformedEnvelope,
			Message: "Replica cache returned a response in an unexpected shape. Check that the replica-cache URL points at the service and not at a proxy or error page.",
		}
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
// whole population. The instant is the first page's data_as_of, nil when the
// page carries none or one this client cannot read.
func (c *Client) list(ctx context.Context, entity string, q url.Values, limit int) ([]json.RawMessage, int, *time.Time, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	var (
		rows []json.RawMessage
		// total is the first page's count, which is what Result.Total carries.
		// maxTotal is the largest count any page reported, which is what the
		// rows are validated against — see the check below the append.
		total    int
		maxTotal int
		cursor   string
		first    = true
		// restarted records the one restart a refused cursor is allowed.
		restarted bool
		asOf      *time.Time
		// seenCursors is every cursor already followed, so a service that
		// repeats one is caught rather than walked in circles.
		seenCursors = map[string]bool{}
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
			// A cursor is minted against the catalogue, and when the catalogue
			// changes underneath a walk the service refuses the next page with
			// a 400 that begins "cursor" ("cursor does not belong to this
			// walk"). The walk starts over from the first page once; a second
			// refusal is returned as it is, or a service that keeps refusing
			// would be walked forever.
			var apiErr *APIError
			if cursor != "" && !restarted && errors.As(err, &apiErr) && apiErr.Status == 400 && strings.HasPrefix(apiErr.Message, "cursor") {
				restarted = true
				rows, total, maxTotal, cursor, first, asOf = nil, 0, 0, "", true, nil
				seenCursors = map[string]bool{}
				continue
			}
			return nil, 0, nil, err
		}
		if first {
			total = *page.Count
			first = false
			if page.DataAsOf != nil {
				if t, ok := parseDataAsOf(*page.DataAsOf); ok {
					asOf = &t
				}
			}
		}
		if n := *page.Count; n > maxTotal {
			maxTotal = n
		}
		rows = append(rows, page.rows()...)

		// Pages that individually agree with their own count can still
		// contradict each other: two one-row pages each reporting count 1 hand
		// back two rows for a total of one, and the truncation guard only looks
		// for the opposite inequality, so an alert would evaluate the extra row
		// as authoritative.
		//
		// The comparison is against the LARGEST count seen, not the first. The
		// first page's total goes stale by design: this mirrors a database being
		// written to, cursor paging does not freeze a snapshot, and rows
		// inserted mid-walk legitimately push the running count past a total
		// that was correct when it was read. In that case the later pages' own
		// counts have grown to cover them, so the check passes. A service
		// contradicting itself has no such growth, and is refused.
		if len(rows) > maxTotal {
			return nil, 0, nil, &TransportError{
				Op:      "reading response from " + truncate(c.base+"/v1/"+entity),
				Err:     errInconsistentCount,
				Message: "Replica cache returned more rows than any page's total said existed. The service is reachable but answered with something unexpected.",
			}
		}

		// A page that comes back empty or without a cursor is the end of the
		// result set. Both conditions are needed: the service omits the cursor on
		// the last page, and a zero-row page with a cursor would otherwise spin.
		if page.NextCursor == "" || len(page.rows()) == 0 {
			break
		}
		// A cursor that does not advance would re-fetch the same page until the
		// limit was reached, handing back one row duplicated and another never
		// seen — and passing every count check on the way, since the duplicates
		// are real rows and the totals agree. It is also the only shape here
		// that could spin: the loop's other exits are an empty page and an
		// absent cursor.
		if seenCursors[page.NextCursor] {
			return nil, 0, nil, &TransportError{
				Op:      "reading response from " + truncate(c.base+"/v1/"+entity),
				Err:     errCursorNotAdvancing,
				Message: "Replica cache returned the same pagination cursor twice, so the results would repeat rather than continue. The service is reachable but answered with something unexpected.",
			}
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	// The LARGEST count seen, not the first. Both are the service's own answer,
	// but a stale one understates: with a first page of 9,999, later pages
	// reporting 10,001 and a limit of 10,000, returning 9,999 beside 10,000 rows
	// makes isTruncated read len(Rows) >= Total and report a complete answer, so
	// an alert evaluates a subset as the whole population. maxTotal can only be
	// larger, so this can only make truncation MORE visible, never less.
	if maxTotal > total {
		total = maxTotal
	}
	return rows, total, asOf, nil
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
