package replicacache

import (
	"context"
	"errors"
	"strings"
	"testing"

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
