package replicacache

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The value is app/model, split at the last slash, and the label reads as the
// NetBox provider's would: "Ip Addresses" looks like a typo.
func TestObjectTypes_SplitAppAndModelAndLabelAcronyms(t *testing.T) {
	f := newFakeService()
	f.addEntity("ipam/ip-addresses", "id:BIGINT:pk", "address:VARCHAR")
	p := newTestProvider(t, f)

	types, err := p.ObjectTypes(context.Background())
	if err != nil {
		t.Fatalf("ObjectTypes: %v", err)
	}
	byValue := map[string]provider.ObjectType{}
	for _, ty := range types {
		byValue[ty.Value] = ty
	}
	if d := byValue["dcim/devices"]; d.App != "dcim" || d.Model != "devices" {
		t.Errorf("app/model not split: %+v", d)
	}
	if ip := byValue["ipam/ip-addresses"]; ip.Label != "IP Addresses" {
		t.Errorf("label = %q, want %q", ip.Label, "IP Addresses")
	}
}

func TestHealthCheckSurfacesAuthFailure(t *testing.T) {
	f := newFakeService()
	f.status, f.errBody = 401, "unauthorized"
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
		{"ResolveScope", func() error {
			_, err := p.ResolveScope(ctx, []provider.Filter{{Field: "parent", Value: "10.0.0.0/24"}}, nil, 10)
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

// A related name FilterFields advertises can be searched: the service returns
// the expanded columns beside the primary key under a projection, so the
// picker reads them from there, with the search pushed down under expand=.
func TestFieldValuesOnAnExpandedNameReadsTheTarget(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001), deviceFixture(2, "CORE-2", 4002), deviceFixture(3, "EDGE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-1", "slug": "dc-1"},
		{"id": float64(4002), "name": "Branch-9", "slug": "branch-9"},
	}
	p := newTestProvider(t, f)

	vals, err := p.FieldValues(context.Background(), "dcim/devices", "site", "", 100)
	if err != nil {
		t.Fatalf("FieldValues on an expanded name: %v", err)
	}
	if !slices.Equal(vals, []string{"Branch-9", "DC-1"}) {
		t.Errorf("values = %v, want the distinct site names", vals)
	}
	req, ok := f.requestWith("dcim/devices", "expand")
	if !ok || req.query.Get("expand") != "site" || req.query.Get("fields") != "id" {
		t.Errorf("request = %v, want expand=site with only the key projected", req.query)
	}

	vals, err = p.FieldValues(context.Background(), "dcim/devices", "site", "dc", 100)
	if err != nil || !slices.Equal(vals, []string{"DC-1"}) {
		t.Errorf("searched values = %v err=%v", vals, err)
	}
	if r, ok := f.requestWith("dcim/devices", "filter[site]__ilike"); !ok || r.query.Get("expand") != "site" {
		t.Error("the search must be pushed down under expand=")
	}

	// A name that is neither stored nor an available expansion has nothing
	// to read.
	if vals, err := p.FieldValues(context.Background(), "dcim/devices", "platform", "", 100); err != nil || len(vals) != 0 {
		t.Errorf("unavailable expansion: %v %v", vals, err)
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
	// Prime the catalogue while the service is healthy, then break it.
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

// BaseURL is what the plugin layer uses as the prefix when rewriting links from
// an internal host to a browser-facing one. The deep links point at the NetBox
// the catalogue reports, so returning the cache root while links exist would
// leave that prefix matching nothing and hand the user an unreachable URL.
func TestBaseURLIsTheLinkBase(t *testing.T) {
	f := newFakeService()
	f.schema.NetBoxURL = "https://netbox.internal/"
	p := newTestProvider(t, f)
	if _, err := p.ObjectTypes(context.Background()); err != nil { // the catalogue is read once
		t.Fatal(err)
	}
	if got := p.BaseURL(); got != "https://netbox.internal" {
		t.Errorf("BaseURL = %q, want the NetBox base the links were built from", got)
	}

	// Nothing is linkable when the replica reports no NetBox and the datasource
	// has no Browser URL, so there is nothing to rewrite and the cache root is
	// a harmless fallback.
	f2 := newFakeService()
	srv := f2.start(t)
	without := New(srv.URL, "t", "nb", srv.Client())
	if _, err := without.ObjectTypes(context.Background()); err != nil {
		t.Fatal(err)
	}
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

// The shared error wording names the NetBox URL and API token, which this mode
// does not use — so a credential failure would send the reader to fix a field
// with no bearing on it.
func TestCacheFailuresNameCacheSettings(t *testing.T) {
	cases := map[int][]string{
		401: {"API token", "NetBox instance ID"},
		403: {"API token"},
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

// A 200 carrying no entities is a catalogue FAILURE, not an answer of "nothing
// exists". Caching it would pin an empty entity set for the whole TTL, and
// every query would then be refused locally while the service is healthy.
func TestEmptyCatalogueIsNotCachedAsAnAnswer(t *testing.T) {
	f := newFakeService()
	f.schema.Entities = map[string]fakeEntity{}
	p := newTestProvider(t, f)
	ctx := context.Background()

	if _, err := p.ObjectTypes(ctx); err == nil {
		t.Fatal("an empty catalogue must be an error, not an empty answer")
	}

	// And nothing was cached: once the catalogue fills, the next call reads it
	// rather than the remembered failure.
	f.mu.Lock()
	f.schema = devicesSchema()
	f.mu.Unlock()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("a failed catalogue fetch must not be remembered: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("want the row, got %d", len(res.Rows))
	}
}

// A 200 whose body will not parse is the service's problem, not the network's —
// but unclassified it renders as "Couldn't reach NetBox", which is wrong twice:
// we reached it, and it was not NetBox.
func TestMalformedResponseNamesTheCache(t *testing.T) {
	srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
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

// The service that failed is replica-cache, and NetBox is optional in this
// mode — possibly not configured at all. An unclassified error renders through
// the plugin's fallback as "Cannot reach NetBox", sending Save & Test at the
// wrong service.
func TestEmptyCataloguePointsAtTheCacheNotNetBox(t *testing.T) {
	f := newFakeService()
	f.schema.Entities = map[string]fakeEntity{} // a catalogue with nothing in it
	p := newTestProvider(t, f)

	_, err := p.ObjectTypes(context.Background())
	if err == nil {
		t.Fatal("want an error for an empty API description")
	}
	if !errors.Is(err, errEmptyCatalogue) {
		t.Errorf("want errEmptyCatalogue, got %v", err)
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

// Save & Test must make a live request. Answering from a catalogue up to ten
// minutes old lets it report "Connected" after the token has been
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

// The whole path: a plugin model must reach the editor's list, with the label
// the NetBox provider gives it, and a query against it must be accepted rather
// than refused as unknown.
func TestPluginModelIsQueryable(t *testing.T) {
	f := newFakeService()
	f.addEntity("plugins/bgp/bgp-sessions", "id:BIGINT:pk", "name:VARCHAR")
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
		// A healthy page first, so the failure below is the value decoder's and
		// not the transport's.
		var n int
		srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
			n++
			w.Header().Set("Content-Type", "application/json")
			if n <= 1 {
				_, _ = w.Write([]byte(`{"count": 1, "results": [{"id": 1, "name": "CORE-1"}]}`))
				return
			}
			_, _ = w.Write([]byte(bad))
		}))
		p := New(srv.URL, "t", "nb", srv.Client())
		if _, err := p.FieldValues(context.Background(), "dcim/devices", "name", "", 100); err != nil {
			t.Fatalf("a healthy page must succeed: %v", err)
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

// The route's netbox_url is normalised the way the NetBox provider normalises
// its own URL: a trailing "/api" or slash would make "View in NetBox" open the
// REST response for the object rather than its page.
func TestNetBoxURLIsNormalizedForDeepLinks(t *testing.T) {
	for _, base := range []string{
		"https://netbox.example.com/api",
		"https://netbox.example.com/api/",
		"https://netbox.example.com/",
		"  https://netbox.example.com  ",
	} {
		f := newFakeService()
		f.schema.NetBoxURL = base
		f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
		p := newTestProvider(t, f)
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

// A search box's text is one literal name. buildFilterValues splits on commas
// as a multi-value dashboard filter, which is not what someone typing "a,b"
// means, so that text is matched locally instead of being refused; everything
// else goes upstream as written, since the backend takes the value literally.
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

	// Ordinary text is pushed down as written — the backend's ilike is a
	// contains on the literal value — so the local fallback has not quietly
	// become the only path.
	if _, err := p.FieldValues(context.Background(), "dcim/devices", "name", "CORE_SW", 100); err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if r, ok := f.requestWith("dcim/devices", "filter[name]__ilike"); !ok {
		t.Error("an ordinary search should still be pushed down")
	} else if got := r.query.Get("filter[name]__ilike"); got != "CORE_SW" {
		t.Errorf("pushed %q, want CORE_SW", got)
	}
}

// Two rows sharing an id carry two values for one object, and only one can be
// its own — offering both puts a value in the picker that filtering by it would
// then match nothing. Deduplicating by the displayed value does not catch it:
// the values differ, which is the problem.
func TestDuplicateIDsInAutocompleteAreRejected(t *testing.T) {
	srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 2, "results": [{"id": 1, "name": "CORE-1"}, {"id": 1, "name": "STALE"}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.FieldValues(context.Background(), "dcim/devices", "name", "", 100)
	if err == nil {
		t.Fatal("two values for one object must not both be offered")
	}
	if !errors.Is(err, errDuplicateRow) {
		t.Errorf("want errDuplicateRow, got %v", err)
	}
}

// The projection asked for the column, so its absence is the service failing to
// answer rather than the object having no value — and treating the two alike
// turned an incomplete response into a short list the editor presents as
// authoritative.
func TestValueRowMissingTheProjectedFieldIsRejected(t *testing.T) {
	srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 2, "results": [{"id": 1, "name": "CORE-1"}, {"id": 2}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.FieldValues(context.Background(), "dcim/devices", "name", "", 100)
	if err == nil {
		t.Fatal("a row without the projected column must not be read as a value-less object")
	}
	if !errors.Is(err, errRowWithoutField) {
		t.Errorf("want errRowWithoutField, got %v", err)
	}
}

// A column that is PRESENT and null is an object with no value there, which is
// an ordinary answer and must stay one.
func TestValueRowWithANullFieldIsAnAnswer(t *testing.T) {
	var n int
	srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"count": 1, "results": [{"id": 1, "serial": "ABC"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count": 2, "results": [{"id": 1, "serial": "ABC"}, {"id": 2, "serial": null}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	got, err := p.FieldValues(context.Background(), "dcim/devices", "serial", "", 100)
	if err != nil {
		t.Fatalf("a null value is an ordinary answer: %v", err)
	}
	if len(got) != 1 || got[0] != "ABC" {
		t.Errorf("got %v, want just ABC", got)
	}
}

// "" and "main" both mean the unbranched dataset — the sentinel the NetBox
// client already honours, and the value the documented branch variable emits
// for its always-present first option. This mode mirrors exactly that dataset,
// so rejecting the word for it broke every panel driven by that variable while
// it pointed at the data we serve.
func TestMainIsTheUnbranchedDataset(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	for _, branch := range []string{"main", "Main", "MAIN", " main "} {
		ctx := provider.WithBranch(context.Background(), branch)
		if _, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"}); err != nil {
			t.Errorf("branch %q means the main dataset: %v", branch, err)
		}
	}

	// A real branch is still refused: the cache cannot answer it, and answering
	// from main would look healthy while reporting the wrong data.
	ctx := provider.WithBranch(context.Background(), "schema_abc")
	if _, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"}); err == nil {
		t.Error("a branch-scoped query must still be refused")
	}
}

func TestObjectTypes_ComeFromTheCatalogue(t *testing.T) {
	f := newFakeService()
	p := newTestProvider(t, f)
	types, err := p.ObjectTypes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, ot := range types {
		values = append(values, ot.Value)
	}
	// Every configured entity, fed or not, sorted; the unfed one stays listed so
	// the error on query can explain it rather than the picker hiding it.
	want := []string{"core/object-types", "dcim/device-roles", "dcim/devices", "dcim/platforms", "dcim/racks", "dcim/sites", "tenancy/tenants"}
	if !slices.Equal(values, want) {
		t.Errorf("values = %v, want %v", values, want)
	}
	if n := f.countRequestsFor("docs/openapi.json"); n != 0 {
		t.Errorf("discovery must not touch openapi.json any more, saw %d requests", n)
	}
}

// A plugin model keeps its whole app prefix and is labelled after the plugin,
// exactly as the OpenAPI-path discovery used to produce it.
func TestObjectTypes_LabelPluginModels(t *testing.T) {
	f := newFakeService()
	f.addEntity("plugins/bgp/bgp-sessions", "id:BIGINT", "name:VARCHAR")
	p := newTestProvider(t, f)
	types, err := p.ObjectTypes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ot := range types {
		if ot.Value == "plugins/bgp/bgp-sessions" {
			if ot.Label != "Bgp: Bgp Sessions" || ot.App != "plugins/bgp" || ot.Model != "bgp-sessions" {
				t.Errorf("plugin type = %+v", ot)
			}
			if _, _, err := p.entityFor(context.Background(), ot.Value); err != nil {
				t.Errorf("a listed plugin type must resolve: %v", err)
			}
			return
		}
	}
	t.Error("the plugin model was not listed")
}

func TestHealthCheck_ReportsFedEntitiesAndRequiresTheRoute(t *testing.T) {
	f := newFakeService()
	p := newTestProvider(t, f)
	msg, err := p.HealthCheck(context.Background())
	if err != nil || !strings.Contains(msg, "7 object types") || !strings.Contains(msg, "6 with data") {
		t.Errorf("msg=%q err=%v", msg, err)
	}
	f.mu.Lock()
	f.schema = nil
	f.mu.Unlock()
	if _, err := p.HealthCheck(context.Background()); err == nil || !strings.Contains(provider.Classify(err).Detail, "predates") {
		t.Errorf("an older build must fail Save & Test with the upgrade message, got %v", err)
	}
}

func TestEntityFor_AnswersFromTheCatalogue(t *testing.T) {
	p := newTestProvider(t, newFakeService())
	if e, _, err := p.entityFor(context.Background(), "dcim/devices"); err != nil || e.PrimaryKey != "id" {
		t.Errorf("known type: entity=%+v err=%v", e, err)
	}
	_, _, err := p.entityFor(context.Background(), "dcim/widgets")
	var unknown *UnknownObjectTypeError
	if !errors.As(err, &unknown) {
		t.Errorf("unknown type: got %v, want UnknownObjectTypeError", err)
	}
}

// Fields is read off the catalogue: the physical columns in table order,
// typed by the catalogue, each choice column followed by its <field>_value;
// then the columns every AVAILABLE reference adds under expand=; then the
// custom fields; then the link. The one row read is for custom-field names —
// the catalogue cannot carry them — projected to the blob and bounded.
func TestFields_AreTheCatalogueColumnsPlusExpansions(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "a", "custom_field_data": `{"lifecycle_phase":"production","tags_count":2}`},
	}
	f.schema.NetBoxURL = "https://netbox.example.com"
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]provider.FieldType{}
	var order []string
	for _, fl := range fields {
		byName[fl.Name] = fl.Type
		order = append(order, fl.Name)
	}
	wantOrder := []string{
		"id", "name", "serial", "position", "is_full_depth", "created", "status", "status_value", "role_id", "tenant_id", "site_id", "rack_id", "platform_id",
		"role", "role_slug", "tenant", "tenant_slug", "site", "site_slug", "rack",
		"cf_lifecycle_phase", "cf_tags_count",
		"display_url",
	}
	if !slices.Equal(order, wantOrder) {
		t.Errorf("order = %v\nwant    %v", order, wantOrder)
	}
	for name, want := range map[string]provider.FieldType{
		"id": provider.FieldTypeNumber, "name": provider.FieldTypeString, "position": provider.FieldTypeNumber,
		"is_full_depth": provider.FieldTypeBoolean,
		// VARCHAR on every tenant today (DATA-250); time the day the catalogue says so.
		"created": provider.FieldTypeString,
		"site":    provider.FieldTypeString, "site_slug": provider.FieldTypeString,
		"status_value":       provider.FieldTypeString,
		"cf_lifecycle_phase": provider.FieldTypeString, "cf_tags_count": provider.FieldTypeNumber,
	} {
		if byName[name] != want {
			t.Errorf("%s: type %q, want %q", name, byName[name], want)
		}
	}
	if _, ok := byName["platform"]; ok {
		t.Error("an expansion whose target has no data must not be offered as a column")
	}
	if _, ok := byName["custom_field_data"]; ok {
		t.Error("the raw JSON column is not a field; its cf_* expansion is")
	}
	req, ok := f.requestWith("dcim/devices", "fields")
	if !ok || req.query.Get("fields") != "custom_field_data" || req.query.Get("limit") != "20" {
		t.Errorf("custom-field discovery request = %v, want fields=custom_field_data&limit=20", req.query)
	}
	if n := f.countRequestsFor("dcim/devices"); n != 1 {
		t.Errorf("Fields made %d row requests, want exactly the custom-field read", n)
	}
}

// Rows show a choice by its label ("Active") and <field>_value is how a panel
// reads the stored value ("active"). A query accepts the alias, so Return fields
// has to offer it: with only the physical columns there is nothing to pick.
// NetBox mode lists the two side by side, so each alias follows its column here,
// typed as the stored value is: text for a status, a number for a rack's width.
//
// An alias is only worth offering when a query can fill it, so the aliases
// listed are selected as well, and must come back carrying the stored value.
func TestFields_ListsChoiceValueAliases(t *testing.T) {
	f := newFakeService()
	f.addEntity("dcim/racks", "id:BIGINT:pk", "name:VARCHAR", "status:VARCHAR", "width:SMALLINT")
	f.entities["dcim/racks"] = []map[string]interface{}{
		{"id": float64(1), "name": "R1", "status": "active", "width": float64(19)},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	got, err := p.Fields(ctx, "dcim/racks")
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Field{
		{Name: "id", Type: provider.FieldTypeNumber},
		{Name: "name", Type: provider.FieldTypeString},
		{Name: "status", Type: provider.FieldTypeString},
		{Name: "status_value", Type: provider.FieldTypeString},
		{Name: "width", Type: provider.FieldTypeNumber},
		{Name: "width_value", Type: provider.FieldTypeNumber},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fields = %v\nwant     %v", got, want)
	}

	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/racks", Fields: []string{"status_value", "width_value"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !slices.Equal(res.Columns, []string{"status_value", "width_value"}) {
		t.Errorf("columns = %v, want both aliases a query is asked for", res.Columns)
	}
	if v := res.Rows[0]["status_value"]; v != "active" {
		t.Errorf("status_value = %#v, want the stored value", v)
	}
	if v := res.Rows[0]["width_value"]; v != float64(19) {
		t.Errorf("width_value = %#v, want the stored number", v)
	}
}

// The label map is NetBox's, taken from its API schemas; the catalogue is the
// replica's tables; and the two differ. NetBox labels a prefix's family, which
// it works out from the prefix, so no table has a column for it. An alias
// offered for a column the replica lacks would be one no query can fill, so
// only a column the catalogue stores gets one.
func TestFields_NoAliasForUnstoredLabelledColumn(t *testing.T) {
	// The fixture is the case it names only while the map labels family.
	if _, ok := choiceLabel("ipam/prefixes", "family", "4"); !ok {
		t.Fatal("the label map no longer labels a prefix's family; this test needs another labelled column the replica does not store")
	}
	f := newFakeService()
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR", "status:VARCHAR")
	f.entities["ipam/prefixes"] = []map[string]interface{}{{"id": float64(1), "prefix": "10.0.0.0/8", "status": "active"}}
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "ipam/prefixes")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fl := range fields {
		names = append(names, fl.Name)
	}
	// status is stored and labelled, so this is not an empty alias list passing
	// for the right reason.
	if want := []string{"id", "prefix", "status", "status_value"}; !slices.Equal(names, want) {
		t.Errorf("fields = %v, want %v: family is labelled but not stored, so neither it nor family_value is offered", names, want)
	}
}

// An object custom field flattens in NetBox mode to cf_<name> plus
// cf_<name>_id. The replica reads the blob as raw JSON, where an object field
// is a bare number like an integer one, so cf_<name>_id beside every number
// would offer a column most of them cannot fill. A query that selects it still
// gets the id where the value is one (TestObjectCustomFieldExposesItsID); it is
// only not listed.
func TestFields_NoCustomFieldIDAliases(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{
		"id": float64(1), "name": "a", "status": "active",
		// owning_tenant holds the id an object field would; rack_units is a
		// plain integer field. The replica cannot tell them apart.
		"custom_field_data": `{"owning_tenant":22,"rack_units":4,"tier":"gold"}`,
	}}
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, fl := range fields {
		listed[fl.Name] = true
	}
	// A guard is worth something only while there are custom fields to attach
	// an id to.
	custom := []string{"cf_owning_tenant", "cf_rack_units", "cf_tier"}
	for _, name := range custom {
		if !listed[name] {
			t.Fatalf("%s is not offered, so the fixture has nothing to attach an id to: %v", name, fields)
		}
	}
	for _, name := range custom {
		if listed[name+"_id"] {
			t.Errorf("%s_id is listed: the replica cannot tell an object custom field from an integer one", name)
		}
	}
}

// An empty table still has columns: they are the catalogue's, not the rows'.
// Only the custom-field names are missing, since no row could name them.
func TestFields_EmptyTableStillListsTheCatalogueColumns(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{}
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields on an empty table: %v", err)
	}
	seen := map[string]bool{}
	for _, fl := range fields {
		seen[fl.Name] = true
	}
	if !seen["name"] || !seen["site"] {
		t.Errorf("catalogue columns missing from an empty table's fields: %v", fields)
	}
	for _, fl := range fields {
		if strings.HasPrefix(fl.Name, "cf_") || fl.Name == deepLinkColumn {
			t.Errorf("%s offered with no rows to name it and no link base", fl.Name)
		}
	}
}

// An entity the catalogue lists but has fed no data for carries no columns
// in the catalogue (the service lists none until it has ingested), so it has
// no fields; and nothing is read from it, since the row route answers 404.
func TestFields_UnfedEntityHasNoFieldsAndReadsNoRows(t *testing.T) {
	f := newFakeService()
	f.schema.NetBoxURL = "https://netbox.example.com"
	p := newTestProvider(t, f)

	fields, err := p.Fields(context.Background(), "dcim/platforms")
	if err != nil || len(fields) != 0 {
		t.Errorf("fields=%v err=%v, want none", fields, err)
	}
	if n := f.countRequestsFor("dcim/platforms"); n != 0 {
		t.Errorf("an unfed entity answers 404 on rows; %d requests were made anyway", n)
	}
}

// The names are cached with the catalogue's TTL, or every editor interaction
// would read the blob again.
func TestFields_CachesTheCustomFieldNames(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := p.Fields(ctx, "dcim/devices"); err != nil {
			t.Fatalf("Fields %d: %v", i, err)
		}
	}
	if n := f.countRequestsFor("dcim/devices"); n != 1 {
		t.Errorf("read custom-field names %d times across 3 calls, want 1", n)
	}
}

// pluginDevices is the devices fixture's table under a plugin's object type.
// The vocabulary covers no plugin, so its filters are the catalogue's, which
// is what the tests using it are about; NetBox's own devices offer NetBox's
// filters (TestFilterFields_OffersNetBoxVocabularyOnly).
const pluginDevices = "plugins/acme/devices"

func withPluginDevices(s *fakeSchema) *fakeSchema {
	e := s.Entities["/v1/dcim/devices"]
	e.Table = fakeTable(pluginDevices)
	e.Columns = slices.Clone(e.Columns)
	s.Entities["/v1/"+pluginDevices] = e
	return s
}

// The operators come from the catalogue, translated to the editor's tokens,
// on a table the vocabulary does not cover. The two withholdings that remain
// are deliberate: is-empty on TEXT (NetBox stores a blank as "", the backend
// tests IS NULL — DATA-206) and on a NOT NULL column (nothing is ever empty
// there).
func TestFilterFields_OperatorsComeFromTheCatalogue(t *testing.T) {
	f := newFakeService()
	f.schema = withPluginDevices(f.schema)
	f.entities[pluginDevices] = []map[string]interface{}{}
	p := newTestProvider(t, f)
	ff, err := p.FilterFields(context.Background(), pluginDevices)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string][]string{}
	for _, x := range ff {
		ops[x.Name] = x.Operators
	}
	for name, want := range map[string][]string{
		"name":          {"", "n", "ic", "nic", "gt", "lt"},       // nullable VARCHAR: contains, no is-empty
		"serial":        {"", "n", "ic", "nic", "gt", "lt"},       // NOT NULL VARCHAR: the same
		"position":      {"", "n", "gt", "lt", "empty", "nempty"}, // nullable DOUBLE: is-empty is exact
		"id":            {"", "n", "gt", "gte", "lt", "lte"},      // NOT NULL BIGINT: never empty; whole numbers take >= and <=
		"is_full_depth": {"", "n", "gt", "lt"},                    // NOT NULL BOOLEAN
		"site":          {"", "n", "ic", "nic", "gt", "lt"},       // expanded name: the target's name column
		"site_slug":     {"", "n", "ic", "nic", "gt", "lt"},
		"rack":          {"", "n", "ic", "nic", "gt", "lt"},
	} {
		if !slices.Equal(ops[name], want) {
			t.Errorf("%s: %v, want %v", name, ops[name], want)
		}
	}
	for _, absent := range []string{"platform", "platform_slug", "rack_slug", "cf_lifecycle_phase", "custom_field_data", "display_url"} {
		if _, ok := ops[absent]; ok {
			t.Errorf("%s must not be filterable", absent)
		}
	}
	// Every advertised operator is one a query accepts.
	assertAdvertisedOperatorsAccepted(t, p, pluginDevices, ops)
}

// assertAdvertisedOperatorsAccepted runs one query per advertised operator, so
// the check covers every step a filter passes through — the rewrites as well
// as the translation — not the translator alone. What the fake then answers
// is beside the point; a refusal is what an advertised operator must never get.
// The value is one the filter takes: NetBox's booleans read true, not 1. A
// NetBox relation whose target has received no data is left out: its values
// cannot be checked until that table is fed, which the editor cannot know
// when it lists the filter.
func assertAdvertisedOperatorsAccepted(t *testing.T, p *Provider, objectType string, ops map[string][]string) {
	t.Helper()
	e, c, err := p.entityFor(context.Background(), objectType)
	if err != nil {
		t.Fatal(err)
	}
	vocab, covered := vocabFor(e.Table)
	for name, list := range ops {
		value := "1"
		if covered {
			switch entry := pickVocab(vocab[name], e); entry.Kind {
			case kindBool, kindIsNull, kindLocalContext:
				value = "true"
			case kindPrefix:
				value = "10.0.0.0/8"
			case kindMAC:
				value = "58:56:9f:d1:e3:00"
			case kindRelated, kindTree, kindModelChoice:
				target := strings.Replace(entry.Target, ".", "_", 1)
				fed := slices.ContainsFunc(slices.Collect(maps.Values(c.Entities)), func(e entity) bool { return e.Table == target && e.Ingested })
				if !fed {
					continue
				}
			}
		}
		for _, op := range list {
			_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: objectType,
				Filters: []provider.Filter{{Field: name, Operator: op, Value: value}}})
			var refused *UnsupportedFilterError
			if errors.As(err, &refused) {
				t.Errorf("%s advertises %q but a query with it is refused: %v", name, op, err)
			}
		}
	}
}

// A replica that lists DATA-408's operators gets them offered, on text columns
// and expanded names only; the translator accepts every one advertised.
func TestFilterFields_OffersAnchoredMatchesWhenTheCatalogueListsThem(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(withPluginDevices(devicesSchema()))
	f.entities[pluginDevices] = []map[string]interface{}{}
	p := newTestProvider(t, f)
	ff, err := p.FilterFields(context.Background(), pluginDevices)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string][]string{}
	for _, x := range ff {
		ops[x.Name] = x.Operators
	}
	for name, want := range map[string][]string{
		"name":      {"", "n", "ic", "nic", "isw", "nisw", "iew", "niew", "ie", "nie", "gt", "lt"},
		"site":      {"", "n", "ic", "nic", "isw", "nisw", "iew", "niew", "ie", "nie", "gt", "lt"},
		"site_slug": {"", "n", "ic", "nic", "isw", "nisw", "iew", "niew", "ie", "nie", "gt", "lt"},
		"id":        {"", "n", "gt", "gte", "lt", "lte"},
		"position":  {"", "n", "gt", "lt", "empty", "nempty"},
	} {
		if !slices.Equal(ops[name], want) {
			t.Errorf("%s: %v, want %v", name, ops[name], want)
		}
	}
	assertAdvertisedOperatorsAccepted(t, p, pluginDevices, ops)
}

// filterOps is FilterFields as a map from name to operators.
func filterOps(t *testing.T, p *Provider, objectType string) map[string][]string {
	t.Helper()
	ff, err := p.FilterFields(context.Background(), objectType)
	if err != nil {
		t.Fatalf("FilterFields(%s): %v", objectType, err)
	}
	ops := map[string][]string{}
	for _, x := range ff {
		ops[x.Name] = x.Operators
	}
	return ops
}

// The editor offers NetBox's filters, under NetBox's names, with the lookups
// NetBox has on them: a device filters on site (NetBox compares its slug) with
// = and not, and has no rack (NetBox's is rack_id), no site_slug and no site
// contains; a prefix filters on children, which the replica stores as
// _children; an interface on enabled.
func TestFilterFields_OffersNetBoxVocabularyOnly(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(f.schema)
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR", "_children:BIGINT")
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "enabled:BOOLEAN")
	p := newTestProvider(t, f)

	text := []string{"", "n", "ic", "nic", "isw", "nisw", "iew", "niew", "ie", "nie"}
	devices := filterOps(t, p, "dcim/devices")
	for name, want := range map[string][]string{
		"site": {"", "n"}, "site_id": {"", "n"}, "rack_id": {"", "n"}, "tenant": {"", "n"}, "role": {"", "n"},
		"name": text, "serial": text, "status": text,
		"id": {"", "n", "gt", "gte", "lt", "lte", "empty", "nempty"},
		// A decimal NetBox compares as a number, which the replica cannot; its
		// __empty is a boolean NetBox answers, IS NULL.
		"position": {"empty", "nempty"},
	} {
		if !slices.Equal(devices[name], want) {
			t.Errorf("devices %s: %v, want %v", name, devices[name], want)
		}
	}
	// No filter NetBox lacks: rack and the _slug names were the cache's own,
	// created is a date the replica holds as text (and created__empty a 400),
	// is_full_depth is the device type's.
	for _, absent := range []string{"rack", "site_slug", "role_slug", "tenant_slug", "created", "is_full_depth", "custom_field_data", "cf_lifecycle_phase"} {
		if ops, ok := devices[absent]; ok {
			t.Errorf("devices offer %s %v; NetBox has no such filter the replica can answer", absent, ops)
		}
	}

	prefixes := filterOps(t, p, "ipam/prefixes")
	if want := []string{"", "n", "gt", "gte", "lt", "lte"}; !slices.Equal(prefixes["children"], want) {
		t.Errorf("prefixes children: %v, want %v", prefixes["children"], want)
	}
	if ops, ok := prefixes["_children"]; ok {
		t.Errorf("prefixes offer _children %v; NetBox's filter is children", ops)
	}
	if want := []string{""}; !slices.Equal(prefixes["prefix"], want) {
		t.Errorf("prefixes prefix: %v, want %v", prefixes["prefix"], want)
	}

	interfaces := filterOps(t, p, "dcim/interfaces")
	if want := []string{""}; !slices.Equal(interfaces["enabled"], want) {
		t.Errorf("interfaces enabled: %v, want %v", interfaces["enabled"], want)
	}

	// And a query takes every one offered.
	assertAdvertisedOperatorsAccepted(t, p, "dcim/devices", devices)
	assertAdvertisedOperatorsAccepted(t, p, "ipam/prefixes", prefixes)
	assertAdvertisedOperatorsAccepted(t, p, "dcim/interfaces", interfaces)
}

// NetBox answers speed__empty, children__empty and depth__empty with a 400 —
// their __empty keeps the number filter's class, so true fails its form — so
// is-empty is not offered on them, nullable numbers though they are; mtu__empty
// is a boolean filter, and is. A filter refused on its own (a decimal stored as
// text) whose __empty is a boolean is offered with is-empty alone.
func TestFilterFields_NoEmptyWhereNetBoxErrors(t *testing.T) {
	f := newFakeService()
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR", "_children:BIGINT", "_depth:INTEGER")
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "mtu:INTEGER", "speed:INTEGER")
	f.addEntity("dcim/device-types", "id:BIGINT:pk", "model:VARCHAR", "u_height:VARCHAR")
	p := newTestProvider(t, f)

	interfaces, prefixes := filterOps(t, p, "dcim/interfaces"), filterOps(t, p, "ipam/prefixes")
	for name, ops := range map[string][]string{"speed": interfaces["speed"], "children": prefixes["children"], "depth": prefixes["depth"]} {
		if len(ops) == 0 {
			t.Errorf("%s is not offered at all", name)
		}
		if slices.Contains(ops, "empty") || slices.Contains(ops, "nempty") {
			t.Errorf("%s: %v; NetBox answers %s__empty with an error", name, ops, name)
		}
	}
	if !slices.Contains(interfaces["mtu"], "empty") || !slices.Contains(interfaces["mtu"], "nempty") {
		t.Errorf("mtu: %v, want is-empty and has-any-value", interfaces["mtu"])
	}
	if got := filterOps(t, p, "dcim/device-types")["u_height"]; !slices.Equal(got, []string{"empty", "nempty"}) {
		t.Errorf("device types u_height: %v, want is-empty and has-any-value alone", got)
	}
}

// The vocabulary is the union of NetBox 4.4's filters and 4.6's, and a replica
// mirrors one NetBox: owner, a filter only 4.6 has, on its owner_id column, is
// not offered on a replica whose table lacks the column, and is on one that
// stores it.
func TestFilterFields_OnlyStoredColumns(t *testing.T) {
	f := newFakeService()
	devices := filterOps(t, newTestProvider(t, f), "dcim/devices")
	for _, name := range []string{"owner", "owner_id", "primary_ip4", "config_template_id"} {
		if ops, ok := devices[name]; ok {
			t.Errorf("devices offer %s %v, on a column this replica does not store", name, ops)
		}
	}

	f.mu.Lock()
	e := f.schema.Entities["/v1/dcim/devices"]
	e.Columns = append(slices.Clone(e.Columns), fakeColumn{Name: "owner_id", Type: "BIGINT", Nullable: true, Operators: []string{"eq", "gt", "lt", "in", "isnull"}})
	f.schema.Entities["/v1/dcim/devices"] = e
	f.mu.Unlock()
	devices = filterOps(t, newTestProvider(t, f), "dcim/devices")
	for _, name := range []string{"owner", "owner_id"} {
		if want := []string{"", "n"}; !slices.Equal(devices[name], want) {
			t.Errorf("a replica that stores owner_id: %s %v, want %v", name, devices[name], want)
		}
	}
}

// Deep links point at the NetBox the replica reports it mirrors. A replica that
// reports none, with no Browser URL to fall back on (TestDeepLinks_* below),
// yields rows without a link column, not links to nowhere; BaseURL agrees with
// whichever base the links were built from, or the plugin's public-URL rewrite
// matches nothing.
func TestLinks_ComeFromTheCatalogueNetBoxURL(t *testing.T) {
	f := newFakeService()
	f.schema.NetBoxURL = "https://nb.example.com"
	f.entities["dcim/devices"] = []map[string]interface{}{{"id": 7, "name": "a", "custom_field_data": `{}`}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "display_url"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0]["display_url"] != "https://nb.example.com/dcim/devices/7/" {
		t.Errorf("display_url = %v", res.Rows[0]["display_url"])
	}
	if p.BaseURL() != "https://nb.example.com" {
		t.Errorf("BaseURL = %q must match the base the links were built from", p.BaseURL())
	}

	f2 := newFakeService()
	f2.entities["dcim/devices"] = f.entities["dcim/devices"]
	p = newTestProvider(t, f2)
	res, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "display_url"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Rows[0]["display_url"]; ok || slices.Contains(res.Columns, "display_url") {
		t.Errorf("a replica that reports no NetBox yields no link column, got %v", res.Rows[0])
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "display_url") {
		t.Errorf("the requested link column is reported missing: %v", res.Warnings)
	}
}

// A replica that reports no netbox_url (production's, until DATA-383) can still
// link: the datasource's Browser URL is where its users' browsers reach NetBox,
// which makes it the one NetBox address this datasource has been given. Links
// are built on it as on a reported URL, the editor offers the column they live
// in, and BaseURL is that base, so the plugin's rewrite of links to the Browser
// URL finds nothing to change.
func TestDeepLinks_FallBackToBrowserURL(t *testing.T) {
	f := newFakeService() // reports no netbox_url
	f.entities["dcim/devices"] = []map[string]interface{}{{"id": 7, "name": "a", "custom_field_data": `{}`}}
	p := newTestProvider(t, f, WithFallbackNetBoxURL("https://nb.example.com/"))
	ctx := context.Background()

	// Selected by name, and left at "All columns": the default panel links too.
	for name, fields := range map[string][]string{"selected": {"name", "display_url"}, "all columns": nil} {
		res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices", Fields: fields})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := res.Rows[0]["display_url"]; got != "https://nb.example.com/dcim/devices/7/" {
			t.Errorf("%s: display_url = %v", name, got)
		}
		if !slices.Contains(res.Columns, "display_url") {
			t.Errorf("%s: display_url is not announced as a column, so the data link is never attached: %v", name, res.Columns)
		}
	}

	offered, err := p.Fields(ctx, "dcim/devices")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(offered, func(fl provider.Field) bool { return fl.Name == "display_url" }) {
		t.Errorf("Fields does not offer display_url: %v", offered)
	}
	if got := p.BaseURL(); got != "https://nb.example.com" {
		t.Errorf("BaseURL = %q must match the base the links were built from", got)
	}
}

// The reported URL stays first: it is the NetBox the replica says it mirrors,
// which the Browser URL — where browsers reach that NetBox — need not be
// spelled the same as. The plugin's rewrite is what carries a link from one to
// the other.
func TestDeepLinks_ReportedURLWins(t *testing.T) {
	f := newFakeService()
	f.schema.NetBoxURL = "https://nb.internal"
	f.entities["dcim/devices"] = []map[string]interface{}{{"id": 7, "name": "a", "custom_field_data": `{}`}}
	p := newTestProvider(t, f, WithFallbackNetBoxURL("https://nb.example.com"))

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "display_url"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0]["display_url"]; got != "https://nb.internal/dcim/devices/7/" {
		t.Errorf("display_url = %v, want the reported NetBox's", got)
	}
	if got := p.BaseURL(); got != "https://nb.internal" {
		t.Errorf("BaseURL = %q must match the base the links were built from", got)
	}
}

// The Browser URL becomes the target of every link, so it is held to what a
// reported one is (TestCatalog_NetBoxURLIsValidated): an http(s) URL with a
// host. Anything else is no fallback at all — rows without a link column, not
// links a browser would open on another scheme or resolve against a host that
// was never named.
func TestDeepLinks_FallbackMustBeHTTP(t *testing.T) {
	for _, raw := range []string{"", "   ", "nb.example.com", "//nb.example.com", "ftp://nb.example.com", "javascript:alert(1)", "https://"} {
		f := newFakeService()
		f.entities["dcim/devices"] = []map[string]interface{}{{"id": 7, "name": "a", "custom_field_data": `{}`}}
		srv := f.start(t)
		p := New(srv.URL, "t", "nb", srv.Client(), WithFallbackNetBoxURL(raw))
		ctx := context.Background()

		res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "display_url"}})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if _, ok := res.Rows[0]["display_url"]; ok || slices.Contains(res.Columns, "display_url") {
			t.Errorf("%q: built a link: %v", raw, res.Rows[0])
		}
		offered, err := p.Fields(ctx, "dcim/devices")
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if slices.ContainsFunc(offered, func(fl provider.Field) bool { return fl.Name == "display_url" }) {
			t.Errorf("%q: Fields offers a link column with nothing to link to", raw)
		}
		if got := p.BaseURL(); got != srv.URL {
			t.Errorf("%q: BaseURL = %q, want the cache root", raw, got)
		}
	}
}

// The fallback is normalised as a reported URL is
// (TestNetBoxURLIsNormalizedForDeepLinks), so the two sources build the same
// paths: a trailing slash or the REST suffix would otherwise sit in every link.
func TestDeepLinks_FallbackIsNormalizedLikeAReportedURL(t *testing.T) {
	for _, raw := range []string{
		"https://netbox.example.com/api",
		"https://netbox.example.com/api/",
		"https://netbox.example.com/",
		"  https://netbox.example.com  ",
	} {
		f := newFakeService()
		f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
		p := newTestProvider(t, f, WithFallbackNetBoxURL(raw))
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got := res.Rows[0]["display_url"]; got != "https://netbox.example.com/dcim/devices/1/" {
			t.Errorf("%q produced %v", raw, got)
		}
	}
}

// Every editor open that lists fields must not re-read the custom-field names
// once per panel; concurrent callers share one read.
func TestFields_ConcurrentCallersShareOneNamesRead(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)
	if _, err := p.ObjectTypes(context.Background()); err != nil { // warm the catalogue
		t.Fatal(err)
	}
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Fields(context.Background(), "dcim/devices"); err != nil {
				t.Error(err)
			}
		}()
	}
	for f.countRequestsFor("dcim/devices") < 1 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := f.countRequestsFor("dcim/devices"); n != 1 {
		t.Errorf("read custom-field names %d times for one burst, want 1", n)
	}
}
