package replicacache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

func TestObjectTypesFromTheAPIDescription(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = nil
	f.entities["ipam/ip-addresses"] = nil
	p := newTestProvider(t, f)

	types, err := p.ObjectTypes(context.Background())
	if err != nil {
		t.Fatalf("ObjectTypes: %v", err)
	}
	if len(types) != 2 {
		t.Fatalf("want 2 object types, got %d: %+v", len(types), types)
	}
	// Sorted, so the editor's dropdown does not reshuffle between refreshes.
	if types[0].Value != "dcim/devices" || types[1].Value != "ipam/ip-addresses" {
		t.Errorf("want sorted values, got %q then %q", types[0].Value, types[1].Value)
	}
	if types[0].App != "dcim" || types[0].Model != "devices" {
		t.Errorf("app/model not split: %+v", types[0])
	}
	// Acronyms read as acronyms; "Ip Addresses" would look like a typo.
	if types[1].Label != "IP Addresses" {
		t.Errorf("label = %q, want %q", types[1].Label, "IP Addresses")
	}
}

// The per-id path must not become an object type: it is not listable.
func TestObjectTypesIgnoresItemPaths(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = nil
	p := newTestProvider(t, f)

	types, err := p.ObjectTypes(context.Background())
	if err != nil {
		t.Fatalf("ObjectTypes: %v", err)
	}
	for _, ty := range types {
		if strings.Contains(ty.Value, "{") {
			t.Errorf("item path leaked into object types: %q", ty.Value)
		}
	}
}

func TestHealthCheck(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = nil
	p := newTestProvider(t, f)

	msg, err := p.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !strings.Contains(msg, "1 object type") {
		t.Errorf("health message should report what was found, got %q", msg)
	}
}

func TestHealthCheckSurfacesAuthFailure(t *testing.T) {
	f := newFakeService()
	f.noSwagger = true
	p := newTestProvider(t, f)

	if _, err := p.HealthCheck(context.Background()); err == nil {
		t.Fatal("want an error when the service cannot be read")
	}
}

// Each unsupported capability must fail LOUDLY. An empty result is the failure
// mode that matters here: an annotation query that silently returns nothing
// looks identical to "nothing changed", and an alert rule on an empty
// enrichment evaluates as healthy.
func TestUnsupportedCapabilitiesRefuseRatherThanReturnNothing(t *testing.T) {
	p := newTestProvider(t, newFakeService())
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"Changes", func() error {
			_, err := p.Changes(ctx, provider.ChangeSpec{})
			return err
		}},
		{"ResolveIPs", func() error {
			_, err := p.ResolveIPs(ctx, []string{"10.0.0.1"}, nil, 10)
			return err
		}},
		{"Topology", func() error {
			_, err := p.Topology(ctx, provider.TopologySpec{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("want an explicit error, got nil: an empty result reads as a real answer")
			}
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatalf("want *UnsupportedError, got %T", err)
			}
			c := unsupported.Classification()
			if c.Kind != provider.ErrorKindUnsupported {
				t.Errorf("kind = %q, want unsupported", c.Kind)
			}
			// The detail is what the reader actually sees, so it has to name the
			// way out rather than only the problem.
			if !strings.Contains(c.Detail, "NetBox mode") {
				t.Errorf("detail should say what to use instead, got %q", c.Detail)
			}
		})
	}
}

func TestFieldsIncludesResolvedAndCustomColumns(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	have := map[string]provider.FieldType{}
	for _, f := range fields {
		have[f.Name] = f.Type
	}
	// The editor must offer exactly what a query returns, including the columns
	// that do not exist upstream.
	for _, want := range []string{"name", "site_id", "site", "site_slug", "cf_lifecycle_phase"} {
		if _, ok := have[want]; !ok {
			t.Errorf("Fields is missing %q; the editor would not offer a column queries return", want)
		}
	}
	if have["site_id"] != provider.FieldTypeNumber {
		t.Errorf("site_id typed %q, want number", have["site_id"])
	}
	if have["name"] != provider.FieldTypeString {
		t.Errorf("name typed %q, want string", have["name"])
	}
}

// An empty table is not an error: the type exists and a query against it
// legitimately returns no rows.
func TestFieldsOnAnEmptyTableIsNotAnError(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{}
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields on an empty table returned an error: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("want no fields from an empty table, got %v", fields)
	}
}

func TestFieldValuesSamplesDistinctValues(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		deviceFixture(1, "CORE-1", 1), deviceFixture(2, "CORE-2", 1), deviceFixture(3, "CORE-1", 1),
	}
	p := newTestProvider(t, f)

	vals, err := p.FieldValues(context.Background(), "dcim/devices", "name", "", 100)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if len(vals) != 2 || vals[0] != "CORE-1" || vals[1] != "CORE-2" {
		t.Errorf("want the two distinct names sorted, got %v", vals)
	}
}

// Autocomplete on a column we synthesize has nothing upstream to read, and
// asking for it by name would be rejected as an unknown column.
func TestFieldValuesOnADerivedColumnReturnsNothing(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": float64(4001), "name": "DC-1", "slug": "dc-1"}}
	p := newTestProvider(t, f)

	vals, err := p.FieldValues(context.Background(), "dcim/devices", "site", "", 100)
	if err != nil {
		t.Fatalf("FieldValues on a derived column errored: %v", err)
	}
	if len(vals) != 0 {
		t.Errorf("want no values for a derived column, got %v", vals)
	}
}

func TestAPIErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   provider.ErrorKind
	}{
		{400, provider.ErrorKindBadRequest},
		{401, provider.ErrorKindAuth},
		// 403 is documented as a tenant/token mismatch, which is the same thing
		// the reader has to go fix as a bad token.
		{403, provider.ErrorKindAuth},
		{404, provider.ErrorKindNotFound},
		{500, provider.ErrorKindUpstream},
		{503, provider.ErrorKindUpstream},
	}
	for _, tc := range cases {
		e := &APIError{Status: tc.status, Body: `{"error":"nope"}`, URL: "http://x/v1/dcim/devices"}
		got := e.Classification()
		if got.Kind != tc.want {
			t.Errorf("status %d classified %q, want %q", tc.status, got.Kind, tc.want)
		}
		if got.Status != tc.status {
			t.Errorf("status %d not carried through, got %d", tc.status, got.Status)
		}
		// The classification is what crosses the seam into user-facing text, so
		// it must never carry the response body or the URL.
		if strings.Contains(got.Detail, "nope") || strings.Contains(got.Detail, "http") {
			t.Errorf("classification leaked upstream text: %+v", got)
		}
	}
}

func TestQueryPropagatesUpstreamFailure(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "d", 1)}
	p := newTestProvider(t, f)
	// Prime discovery while the service is healthy, then break it.
	if _, err := p.ObjectTypes(context.Background()); err != nil {
		t.Fatalf("ObjectTypes: %v", err)
	}
	f.status, f.errBody = 401, "invalid api token"

	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("want an error when the service rejects the request")
	}
	u := provider.Classify(err)
	if u == nil || u.Kind != provider.ErrorKindAuth {
		t.Fatalf("want an auth classification through the seam, got %+v", u)
	}
}

// Discovery is served by one large document and was measured failing (TLS
// timeouts, truncated bodies) against an instance whose row endpoints were
// still answering fine. Blocking every query on it would turn a slow endpoint
// into a total outage, so a query proceeds and lets the row request be
// authoritative.
func TestQueryProceedsWhenDiscoveryIsUnavailable(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.noSwagger = true
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("a query must still run when discovery is down: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["name"] != "CORE-1" {
		t.Fatalf("rows not returned: %+v", res.Rows)
	}
	// FK names cannot be built without the entity list, so the gap is stated
	// rather than left as a silently missing column.
	if len(res.Warnings) == 0 {
		t.Error("want a warning that related names are missing")
	}
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Error("the raw id must still be present")
	}
}

// With discovery down, a genuinely unknown type is caught by the row request
// itself and classified from its 404 — not reported as "one of the 0 types
// this deployment reports", which is what a count from a failed discovery
// would have produced.
func TestUnknownTypeWithoutDiscoveryClassifiesAsNotFound(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "d", 1)}
	f.noSwagger = true
	p := newTestProvider(t, f)

	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/widgets"})
	if err == nil {
		t.Fatal("want an error for an unknown object type")
	}
	u := provider.Classify(err)
	if u == nil || u.Kind != provider.ErrorKindNotFound {
		t.Fatalf("want a not-found classification, got %+v", u)
	}
}

// A dimension that times out yields a SHORTER column list, because the resolved
// names are missing. Caching that would pin the loss for the whole TTL: the
// editor would stop offering columns that queries keep returning. Measured live
// as 47 and 53 columns where a healthy run returns 56.
func TestFieldsDoesNotCacheADegradedColumnList(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	// The sites dimension is unreachable, so "site" and "site_slug" cannot be built.
	f.failEntities["dcim/sites"] = true
	degraded, err := p.Fields(ctx, "dcim/devices")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	if names(degraded)["site"] {
		t.Fatal("site should be absent while its dimension is unreachable")
	}

	// It recovers, and the next read must reflect that rather than serving the
	// short list from cache.
	f.failEntities["dcim/sites"] = false
	healthy, err := p.Fields(ctx, "dcim/devices")
	if err != nil {
		t.Fatalf("Fields after recovery: %v", err)
	}
	if !names(healthy)["site"] {
		t.Error("site is still missing after the dimension recovered: a degraded column list was cached")
	}
	if len(healthy) <= len(degraded) {
		t.Errorf("recovered list (%d) should be longer than the degraded one (%d)", len(healthy), len(degraded))
	}
}

// A complete answer IS cached, or every editor interaction would re-sample.
func TestFieldsCachesACompleteColumnList(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := p.Fields(ctx, "dcim/devices"); err != nil {
			t.Fatalf("Fields %d: %v", i, err)
		}
	}
	if n := f.countRequestsFor("dcim/devices"); n != 1 {
		t.Errorf("sampled %d times across 3 calls, want 1", n)
	}
}

func names(fields []provider.Field) map[string]bool {
	out := map[string]bool{}
	for _, f := range fields {
		out[f.Name] = true
	}
	return out
}

// Regression: while a FK dimension is unreachable, the enriched field list is
// incomplete — but the raw schema from the main-table sample is not, and it is
// what decides which columns may be filtered. Withholding the whole cache entry
// made rawColumns return nil, which filterFieldsFor reads as "no restriction",
// advertising site and cf_* as filterable. Selecting one sends a synthesized
// name upstream as a physical column and answers 400.
func TestFilterFieldsNeverAdvertisesDerivedColumnsWhileDegraded(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	f.failEntities["dcim/sites"] = true
	ffs, err := p.FilterFields(ctx, "dcim/devices")
	if err != nil {
		t.Fatalf("FilterFields: %v", err)
	}
	if len(ffs) == 0 {
		t.Fatal("want the upstream columns to remain filterable while a dimension is down")
	}
	for _, ff := range ffs {
		if ff.Name == "site" || ff.Name == "site_slug" || strings.HasPrefix(ff.Name, "cf_") {
			t.Errorf("%q was advertised as filterable; it is not an upstream column and would answer 400", ff.Name)
		}
	}
	// The real columns are still offered, so the editor stays usable.
	found := false
	for _, ff := range ffs {
		if ff.Name == "site_id" {
			found = true
		}
	}
	if !found {
		t.Error("site_id should still be filterable: it is a stored column and the sample succeeded")
	}
}

// The raw schema stays available through a degradation, since it comes from the
// main-table sample rather than from FK resolution.
func TestRawColumnsSurviveDegradation(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": float64(4001), "name": "DC-1", "slug": "dc-1"}}
	p := newTestProvider(t, f)
	ctx := context.Background()

	f.failEntities["dcim/sites"] = true
	raw, err := p.rawColumns(ctx, "dcim/devices")
	if err != nil {
		t.Fatalf("rawColumns: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("raw columns must survive a failed dimension: the main-table sample succeeded")
	}
	if !raw["site_id"] || !raw["name"] {
		t.Errorf("raw schema incomplete: %v", raw)
	}
	if raw["site"] {
		t.Error("a resolved name must never appear in the raw schema")
	}
}

// BaseURL is what the plugin layer uses as the prefix when rewriting links from
// an internal host to a browser-facing one. The deep links point at NetBox, so
// returning the cache root left that prefix matching nothing and handed the
// user an internal, unreachable NetBox URL.
func TestBaseURLIsTheLinkBase(t *testing.T) {
	f := newFakeService()
	srv := f.start(t)

	withNetBox := New(srv.URL, "t", "nb", srv.Client(), WithNetBoxURL("https://netbox.internal/"))
	if got := withNetBox.BaseURL(); got != "https://netbox.internal" {
		t.Errorf("BaseURL = %q, want the NetBox base the links were built from", got)
	}

	// Nothing is linkable without a NetBox URL, so there is nothing to rewrite
	// and the cache root is a harmless fallback.
	without := New(srv.URL, "t", "nb", srv.Client())
	if got := without.BaseURL(); got != srv.URL {
		t.Errorf("BaseURL = %q, want the cache root as fallback", got)
	}
}

// Autocomplete must not push ILIKE at a non-text column: the service answers
// either 500 or, worse, HTTP 200 with the whole unfiltered population, so the
// dropdown would offer values that do not match what was typed.
func TestFieldValuesDoesNotPushTextSearchAtNonTextColumns(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		deviceFixture(1, "CORE-1", 4001), deviceFixture(22, "CORE-2", 4001), deviceFixture(3, "EDGE-1", 4001),
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	vals, err := p.FieldValues(ctx, "dcim/devices", "id", "2", 100)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	// No ilike parameter may reach the service for a numeric column.
	for _, r := range f.requests {
		for k := range r.query {
			if strings.Contains(k, "filter[id]__ilike") {
				t.Errorf("pushed an unsafe text predicate at a numeric column: %v", r.query)
			}
		}
	}
	// The search is still honoured, locally: 22 contains "2", 1 and 3 do not.
	if len(vals) != 1 || vals[0] != "22" {
		t.Errorf("want the locally matched value [22], got %v", vals)
	}
}

// A text column keeps the pushdown, which is the whole point of having it.
func TestFieldValuesPushesTextSearchForTextColumns(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		deviceFixture(1, "CORE-1", 4001), deviceFixture(2, "EDGE-1", 4001),
	}
	p := newTestProvider(t, f)

	vals, err := p.FieldValues(context.Background(), "dcim/devices", "name", "core", 100)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if _, ok := f.requestWith("dcim/devices", "filter[name]__ilike"); !ok {
		t.Error("a text column should still have its search pushed down")
	}
	if len(vals) != 1 || vals[0] != "CORE-1" {
		t.Errorf("got %v, want [CORE-1]", vals)
	}
}

// An RFC3339 timestamp arrives from JSON as a string like any other. Calling it
// text is what decides the column is offered a "contains" filter, which this
// backend then applies to a TIMESTAMP column — the 500-or-unfiltered hazard the
// provider already guards for numerics.
func TestTimestampColumnsAreNotText(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{
		"id": float64(1), "name": "CORE-1",
		"created":      "2026-05-06T17:34:30.696190Z",
		"last_updated": "2026-05-07T16:30:21.910011Z",
		// A text column that merely looks date-ish must keep its text operators.
		"description": "2026 refresh",
	}}
	p := newTestProvider(t, f)
	ctx := context.Background()

	byName := map[string]provider.FieldType{}
	fields, err := p.Fields(ctx, "dcim/devices")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	for _, fl := range fields {
		byName[fl.Name] = fl.Type
	}
	for _, ts := range []string{"created", "last_updated"} {
		if byName[ts] != provider.FieldTypeTime {
			t.Errorf("%s typed %q, want time", ts, byName[ts])
		}
	}
	if byName["description"] != provider.FieldTypeString {
		t.Errorf("description typed %q, want string", byName["description"])
	}

	ffs, err := p.FilterFields(ctx, "dcim/devices")
	if err != nil {
		t.Fatalf("FilterFields: %v", err)
	}
	for _, ff := range ffs {
		if ff.Name != "created" && ff.Name != "last_updated" {
			continue
		}
		for _, op := range ff.Operators {
			if textOperators[op] {
				t.Errorf("%s was offered text operator %q; ILIKE on a timestamp is the hazard this gate exists for", ff.Name, op)
			}
		}
	}
}

// The shared error wording names the NetBox URL and API token, which this mode
// does not use — so a credential failure would send the reader to fix a field
// with no bearing on it.
func TestCacheFailuresNameCacheSettings(t *testing.T) {
	cases := map[int][]string{
		401: {"replica-cache token", "NetBox instance ID"},
		403: {"replica-cache token"},
		404: {"replica-cache URL"},
		503: {"not a NetBox failure"},
	}
	for status, wants := range cases {
		e := &APIError{Status: status}
		d := e.Classification().Detail
		if d == "" {
			t.Errorf("status %d carries no guidance, so the generic NetBox wording would be shown", status)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(d, w) {
				t.Errorf("status %d guidance should mention %q, got: %s", status, w, d)
			}
		}
		if strings.Contains(d, "NetBox API token") || strings.Contains(d, "NetBox URL") {
			t.Errorf("status %d points at a NetBox setting this mode does not use: %s", status, d)
		}
	}
}

// A DateField serializes as YYYY-MM-DD rather than RFC3339, and a fixed list of
// five names cannot enumerate the columns that use it. Both gaps left date
// columns classified as text and offered ILIKE.
func TestDateColumnsAreNotText(t *testing.T) {
	f := newFakeService()
	f.entities["circuits/circuits"] = []map[string]interface{}{{
		"id": float64(1), "cid": "ntt-001",
		"termination_date": "2026-05-06",
		"install_date":     "2026-01-02",
		"created":          "2026-05-06T17:34:30.696190Z",
		// Not a date column by name, and must keep its text operators even
		// though the value would parse.
		"description": "2026-05-06 cutover",
	}}
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "circuits/circuits")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	byName := map[string]provider.FieldType{}
	for _, fl := range fields {
		byName[fl.Name] = fl.Type
	}
	for _, c := range []string{"termination_date", "install_date", "created"} {
		if byName[c] != provider.FieldTypeTime {
			t.Errorf("%s typed %q, want time", c, byName[c])
		}
	}
	if byName["description"] != provider.FieldTypeString {
		t.Errorf("description typed %q, want string — the name gate should keep it text", byName["description"])
	}
}

// A request that never reached the service must say so about the CACHE. Left
// unclassified it renders through the plugin's fallback as "Cannot reach
// NetBox", sending an operator to a connection that is not what failed.
func TestTransportFailuresNameTheCache(t *testing.T) {
	// A port nothing is listening on: the request cannot complete.
	p := New("http://127.0.0.1:1", "t", "nb", &http.Client{Timeout: 2 * time.Second})

	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("want an error when the cache is unreachable")
	}
	u := provider.Classify(err)
	if u == nil {
		t.Fatal("a transport failure must still be classified, or it renders as a NetBox failure")
	}
	if !strings.Contains(u.Detail, "replica-cache") {
		t.Errorf("guidance should name the cache, got: %q", u.Detail)
	}
	if strings.Contains(u.Detail, "NetBox URL") || strings.Contains(u.Detail, "NetBox API") {
		t.Errorf("guidance points at a NetBox setting: %q", u.Detail)
	}
}

// A 200 carrying no usable paths is a discovery FAILURE, not an answer of
// "nothing exists". Caching it would pin an empty entity set for the whole TTL,
// and every object query would then be rejected locally while the row endpoints
// are perfectly healthy — the opposite of the degradation this path is for.
func TestEmptyDiscoveryIsNotCachedAsAnAnswer(t *testing.T) {
	f := newFakeService() // no entities at all, so the document has no list paths
	p := newTestProvider(t, f)
	ctx := context.Background()

	if _, err := p.ObjectTypes(ctx); err == nil {
		t.Fatal("an empty API description must be an error, not an empty answer")
	}

	// And nothing was cached: a query must still reach the row endpoint rather
	// than being refused against an entity set that claims nothing exists.
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("the row request is authoritative when discovery failed: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("want the row, got %d", len(res.Rows))
	}
}

// A 200 whose body will not parse is the service's problem, not the network's —
// but unclassified it renders as "Couldn't reach NetBox", which is wrong twice:
// we reached it, and it was not NetBox.
func TestMalformedResponseNamesTheCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 3, "results": [{"id":`)) // truncated
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("want an error for an unreadable body")
	}
	u := provider.Classify(err)
	if u == nil {
		t.Fatal("a malformed response must be classified, or it renders as a NetBox failure")
	}
	if !strings.Contains(u.Detail, "Replica cache") {
		t.Errorf("guidance should name the cache: %q", u.Detail)
	}
	if strings.Contains(u.Detail, "Cannot reach") {
		t.Errorf("we did reach it; the wording should not say otherwise: %q", u.Detail)
	}
}

// Column names cannot be enumerated. Every name here is a real NetBox 4.4
// date or date-time field that no naming rule would have guessed — checked
// against the 4.4.10 OpenAPI schema — and classifying them as text advertises
// ILIKE, which this backend then pushes at a timestamp column.
func TestTimestampValuesDecideRegardlessOfColumnName(t *testing.T) {
	f := newFakeService()
	f.entities["core/jobs"] = []map[string]interface{}{{
		"id":        float64(1),
		"scheduled": "2026-05-06T17:34:30.696190Z",
		"started":   "2026-05-06T17:34:31.000000Z",
		"completed": "2026-05-06T17:35:02.100000Z",
		// DataSource.last_synced, Branch.last_sync, Notification.read,
		// User.last_login — all DateTimeFields, none name-guessable.
		"last_synced": "2026-05-06T17:35:02.100000Z",
		"last_sync":   "2026-05-06T17:35:02.100000Z",
		"read":        "2026-05-06T17:35:02.100000Z",
		"last_login":  "2026-05-06T17:35:02.100000Z",
		"merged_time": "2026-05-06T17:35:02.100000Z",
		// Aggregate.date_added is a plain DateField: ambiguous on its own, so
		// the name still has to agree — and the date_ prefix is how it does.
		"date_added": "2026-01-02",
		// Text that looks date-ish must keep its text operators: no zone, no T.
		"description": "2026-05-06 cutover",
		"name":        "2026-01-02",
	}}
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "core/jobs")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	byName := map[string]provider.FieldType{}
	for _, fl := range fields {
		byName[fl.Name] = fl.Type
	}
	for _, c := range []string{
		"scheduled", "started", "completed", "last_synced",
		"last_sync", "read", "last_login", "merged_time", "date_added",
	} {
		if byName[c] != provider.FieldTypeTime {
			t.Errorf("%s typed %q, want time", c, byName[c])
		}
	}
	if byName["description"] != provider.FieldTypeString {
		t.Errorf("description typed %q, want string", byName["description"])
	}

	// And the consequence that matters: no ILIKE offered on a timestamp.
	ffs, err := p.FilterFields(context.Background(), "core/jobs")
	if err != nil {
		t.Fatalf("FilterFields: %v", err)
	}
	timestamps := map[string]bool{
		"scheduled": true, "started": true, "completed": true,
		"last_synced": true, "last_sync": true, "read": true,
		"last_login": true, "merged_time": true, "date_added": true,
	}
	for _, ff := range ffs {
		if !timestamps[ff.Name] {
			continue
		}
		for _, op := range ff.Operators {
			switch op {
			case opIContns, opIStarts, opIEnds, opIExact:
				t.Errorf("%s is a timestamp but advertises the text operator %q", ff.Name, op)
			}
		}
	}
}

// The service that failed is replica-cache, and NetBox is optional in this
// mode — possibly not configured at all. An unclassified error renders through
// the plugin's fallback as "Cannot reach NetBox", sending Save & Test at the
// wrong service.
func TestEmptyDiscoveryPointsAtTheCacheNotNetBox(t *testing.T) {
	f := newFakeService() // no entities, so the description has no list paths
	p := newTestProvider(t, f)

	_, err := p.ObjectTypes(context.Background())
	if err == nil {
		t.Fatal("want an error for an empty API description")
	}
	if !errors.Is(err, errEmptyDiscovery) {
		t.Errorf("want errEmptyDiscovery, got %v", err)
	}
	u := provider.Classify(err)
	if u == nil {
		t.Fatal("an unclassified error renders as a NetBox failure")
	}
	if !strings.Contains(u.Detail, "Replica cache") {
		t.Errorf("guidance should name the cache: %q", u.Detail)
	}
	if strings.Contains(u.Detail, "NetBox URL") {
		t.Errorf("NetBox is optional in this mode; guidance should not send them there: %q", u.Detail)
	}
}

// Save & Test must make a live request. Answering from a discovery result up
// to ten minutes old lets it report "Connected" after the token has been
// revoked, while every query fails — a wrong answer from the one button whose
// whole job is to check the connection.
func TestHealthCheckDoesNotAnswerFromCache(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)
	ctx := context.Background()

	if _, err := p.HealthCheck(ctx); err != nil {
		t.Fatalf("first health check: %v", err)
	}
	// Everything now fails, as it would after a revoked token.
	f.mu.Lock()
	f.status, f.errBody = http.StatusUnauthorized, "invalid token"
	f.mu.Unlock()

	if msg, err := p.HealthCheck(ctx); err == nil {
		t.Fatalf("health check still reported %q from a stale cache", msg)
	}
	// And the cached entity list is still usable for the editor: a failed health
	// check must not empty the dropdowns.
	if types, err := p.ObjectTypes(ctx); err != nil || len(types) == 0 {
		t.Errorf("ObjectTypes should still serve its cache: %d types, err %v", len(types), err)
	}
}

// A plugin's models sit one level deeper, and the object type has to read
// plugins/bgp/bgp-sessions — the same value the NetBox provider produces for
// the same model. A saved query names one thing; both modes have to answer to
// it. Rejecting the path omitted every plugin model from the editor, and once
// discovery was cached validateObjectType rejected the saved query too.
func TestDiscoveryAcceptsPluginPaths(t *testing.T) {
	cases := []struct {
		path              string
		wantApp, wantMode string
		ok                bool
	}{
		{"/v1/dcim/devices", "dcim", "devices", true},
		{"/v1/plugins/bgp/bgp-sessions", "plugins/bgp", "bgp-sessions", true},
		{"/v1/plugins/branching/branches", "plugins/branching", "branches", true},
		// Detail routes are not collections, at either depth.
		{"/v1/dcim/devices/{id}", "", "", false},
		{"/v1/plugins/bgp/bgp-sessions/{id}", "", "", false},
		// Four segments that are not a plugin namespace stay rejected: nothing
		// says what they would mean.
		{"/v1/dcim/devices/interfaces", "", "", false},
		{"/v1/dcim", "", "", false},
	}
	for _, tc := range cases {
		app, model, ok := parseEntityPath(tc.path)
		if ok != tc.ok || app != tc.wantApp || model != tc.wantMode {
			t.Errorf("parseEntityPath(%q) = %q,%q,%v; want %q,%q,%v",
				tc.path, app, model, ok, tc.wantApp, tc.wantMode, tc.ok)
		}
	}
}

// The whole path: a plugin model must reach the editor's list, with the label
// the NetBox provider gives it, and a query against it must be accepted rather
// than refused by validateObjectType.
func TestPluginModelIsQueryable(t *testing.T) {
	f := newFakeService()
	f.entities["plugins/bgp/bgp-sessions"] = []map[string]interface{}{
		{"id": float64(1), "name": "peer-1"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	types, err := p.ObjectTypes(ctx)
	if err != nil {
		t.Fatalf("ObjectTypes: %v", err)
	}
	var found *provider.ObjectType
	for i := range types {
		if types[i].Value == "plugins/bgp/bgp-sessions" {
			found = &types[i]
		}
	}
	if found == nil {
		t.Fatalf("plugin model missing from %d discovered types", len(types))
	}
	if found.Label != "Bgp: Bgp Sessions" {
		t.Errorf("label = %q, want the NetBox provider's wording", found.Label)
	}

	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "plugins/bgp/bgp-sessions"})
	if err != nil {
		t.Fatalf("a discovered type must be queryable: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("rows = %d, want 1", len(res.Rows))
	}
}

// The schema sample has its own decoder, ahead of flattenRows. Unclassified, a
// malformed response from the cache rendered as "Couldn't reach NetBox" — a
// connection this mode may not even have configured.
func TestMalformedSampleRowNamesTheCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A valid envelope whose first row is a scalar.
		_, _ = w.Write([]byte(`{"count": 1, "results": ["CORE-1"]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	// A type-sensitive filter forces the cold schema sample before the query.
	_, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Filters:    []provider.Filter{{Field: "name", Operator: "ic", Value: "CORE"}},
	})
	if err == nil {
		t.Fatal("want an error for a malformed sample row")
	}
	u := provider.Classify(err)
	if u == nil {
		t.Fatal("an unclassified error renders as a NetBox failure")
	}
	if !strings.Contains(u.Detail, "Replica cache") {
		t.Errorf("guidance should name the cache: %q", u.Detail)
	}
}

// Skipping a malformed value row made an unreadable response look like a short
// list, which the editor presents as authoritative — the same silence the query
// path refuses, in the one place a user is choosing what to filter on.
func TestMalformedValueRowIsRejected(t *testing.T) {
	for _, bad := range []string{
		`{"count": 1, "results": ["CORE-1"]}`,
		`{"count": 1, "results": [null]}`,
		// Object-shaped but identifying no object. The projection asks for one
		// field and the service returns id beside it regardless, so a row
		// without one is malformed and its value belongs to nothing.
		`{"count": 1, "results": [{"name": "ghost"}]}`,
		`{"count": 1, "results": [{"id": 0, "name": "ghost"}]}`,
	} {
		// The schema sample is answered normally FIRST, so rawColumns succeeds
		// and caches. That is what makes the value loop reachable at all: with a
		// cold cache the malformed page is caught by the schema sample one level
		// up, and this decoder never runs.
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			w.Header().Set("Content-Type", "application/json")
			// Two good answers first: the schema sample, then the warming
			// value fetch. Only after rawColumns is cached does the value loop
			// become the sole guard, which is the path under test.
			if n <= 2 {
				_, _ = w.Write([]byte(`{"count": 1, "results": [{"id": 1, "name": "CORE-1"}]}`))
				return
			}
			_, _ = w.Write([]byte(bad))
		}))
		p := New(srv.URL, "t", "nb", srv.Client())
		if _, err := p.FieldValues(context.Background(), "dcim/devices", "name", "", 100); err != nil {
			t.Fatalf("the warming call must succeed: %v", err)
		}
		_, err := p.FieldValues(context.Background(), "dcim/devices", "name", "", 100)
		srv.Close()
		if err == nil {
			t.Errorf("%s produced a value list from an unreadable response", bad)
			continue
		}
		if u := provider.Classify(err); u == nil || !strings.Contains(u.Detail, "Replica cache") {
			t.Errorf("%s: guidance should name the cache, got %+v", bad, u)
		}
	}
}

// The URL setting explicitly tolerates a trailing "/api", so a datasource
// configured in NetBox mode and switched here carries it. Stored raw it made
// "View in NetBox" open the REST response for the object rather than its page.
func TestNetBoxURLIsNormalizedForDeepLinks(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	srv := f.start(t)

	for _, base := range []string{
		"https://netbox.example.com/api",
		"https://netbox.example.com/api/",
		"https://netbox.example.com/",
		"  https://netbox.example.com  ",
	} {
		p := New(srv.URL, "t", "nb", srv.Client(), WithNetBoxURL(base))
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
		if err != nil {
			t.Fatalf("%q: %v", base, err)
		}
		link, _ := res.Rows[0][deepLinkColumn].(string)
		if want := "https://netbox.example.com/dcim/devices/1/"; link != want {
			t.Errorf("%q produced %q, want %q", base, link, want)
		}
	}
}

// A search box's text is one literal name. buildFilterValues refuses % and _
// (no escape syntax upstream) and splits on commas as a multi-value dashboard
// filter — neither of which is what someone typing CORE_SW means — so
// propagating that refusal replaced the suggestions with an error.
func TestAutocompleteFallsBackForUnpushableText(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE_SW-01"},
		{"id": float64(2), "name": "CORE_SW-02"},
		{"id": float64(3), "name": "EDGE-RTR-01"},
	}
	p := newTestProvider(t, f)

	for _, q := range []string{"CORE_SW", "CORE_SW-0", "a,b"} {
		if _, err := p.FieldValues(context.Background(), "dcim/devices", "name", q, 100); err != nil {
			t.Errorf("%q: autocomplete must answer, not fail: %v", q, err)
		}
	}

	// And it matches locally rather than returning everything.
	got, err := p.FieldValues(context.Background(), "dcim/devices", "name", "CORE_SW", 100)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want the two CORE_SW names", got)
	}
	for _, v := range got {
		if !strings.Contains(v, "CORE_SW") {
			t.Errorf("%q does not match the search", v)
		}
	}

	// Ordinary text is still pushed down, so the wildcard fallback has not
	// quietly become the only path.
	if _, err := p.FieldValues(context.Background(), "dcim/devices", "name", "CORE", 100); err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if r, ok := f.requestWith("dcim/devices", "filter[name]__ilike"); !ok {
		t.Error("an ordinary search should still be pushed down")
	} else if got := r.query.Get("filter[name]__ilike"); got != "%CORE%" {
		t.Errorf("pushed %q, want %%CORE%%", got)
	}
}
