package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fmt"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// applyNetBoxFields reproduces NetBox's ?fields= semantics on one object: keep
// the named top-level properties, and SILENTLY drop every name the serializer
// does not know. Verified live against NetBox 4.4.10 —
// dcim/devices/?fields=name,site_id,nosuch answers HTTP 200 with {"name": ...}
// and no hint that two thirds of the request went nowhere.
//
// That silence is the whole reason these tests exist. A projecting server is the
// only way a wrong projection can be caught: a server that ignores ?fields=
// (which is also what an older NetBox does) returns the right answer no matter
// what we ask for, so every assertion about the RESULT would pass for the wrong
// reason.
func applyNetBoxFields(obj map[string]interface{}, fields string) map[string]interface{} {
	if fields == "" {
		return obj
	}
	out := map[string]interface{}{}
	for _, f := range strings.Split(fields, ",") {
		if v, ok := obj[f]; ok {
			out[f] = v
		}
	}
	return out
}

// listSpy records the query parameters of every list request.
type listSpy struct {
	mu      sync.Mutex
	queries []url.Values
}

func (s *listSpy) record(q url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
}

// fieldsFor returns the ?fields= value of the first request to a path.
func (s *listSpy) fieldsFor(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queries {
		if q.Get("__path") == path {
			v, ok := q["fields"]
			if !ok {
				return "", false
			}
			return v[0], true
		}
	}
	return "", false
}

// topLevelLists counts the requests that fetched the QUERY'S OWN objects from a
// path. The child lookups utilization makes hit the same paths and always carry
// `within` (child prefixes) or `parent` (child addresses), so excluding those
// two leaves exactly the list requests Query itself issued — one per query
// unless something made it fetch the page twice.
func (s *listSpy) topLevelLists(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.queries {
		if q.Get("__path") == path && q.Get("within") == "" && q.Get("parent") == "" {
			n++
		}
	}
	return n
}

// fieldSet returns the ?fields= names of the first request to a path, as a set.
func (s *listSpy) fieldSet(t *testing.T, path string) map[string]bool {
	t.Helper()
	v, ok := s.fieldsFor(path)
	if !ok {
		t.Fatalf("no ?fields= projection was sent to %s — NetBox serialized every property", path)
	}
	set := map[string]bool{}
	for _, n := range strings.Split(v, ",") {
		set[n] = true
	}
	return set
}

// fakeNetBox serves list endpoints from fixtures. honorFields switches between
// the two NetBox behaviours that matter here: a server that applies ?fields=
// (4.x) and one that ignores it (the control, and any older release).
type fakeNetBox struct {
	honorFields bool
	spy         *listSpy
	// objects maps an object-type path ("ipam/prefixes") to a function of the
	// request's query params, so a fixture can depend on filters (vrf_id, parent).
	objects map[string]func(url.Values) []map[string]interface{}
}

func (f *fakeNetBox) start(t *testing.T) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	for path, fn := range f.objects {
		mux.HandleFunc("/api/"+path+"/", f.handler(path, fn))
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", &http.Client{Timeout: 5 * time.Second})
}

func (f *fakeNetBox) handler(path string, fn func(url.Values) []map[string]interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if f.spy != nil {
			rec := url.Values{}
			for k, v := range q {
				rec[k] = v
			}
			rec.Set("__path", path)
			f.spy.record(rec)
		}
		objs := fn(q)
		results := make([]map[string]interface{}, 0, len(objs))
		for _, o := range objs {
			if f.honorFields {
				o = applyNetBoxFields(o, q.Get("fields"))
			}
			results = append(results, o)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": len(objs), "next": nil, "results": results,
		})
	}
}

func fixed(objs ...map[string]interface{}) func(url.Values) []map[string]interface{} {
	return func(url.Values) []map[string]interface{} { return objs }
}

// deviceFixtures are one page of devices carrying every shape flattenObject
// derives a column from: a nested object (site), a choice object (status), a
// list (tags), a real *_count property, and custom fields.
func deviceFixtures() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"id": 1.0, "name": "leaf1", "display_url": "http://nb/dcim/devices/1/",
			"site":            map[string]interface{}{"id": 2.0, "name": "dc1", "slug": "dc1"},
			"status":          map[string]interface{}{"value": "active", "label": "Active"},
			"tags":            []interface{}{map[string]interface{}{"id": 9.0, "name": "edge", "slug": "edge"}},
			"interface_count": 48.0,
			"serial":          "SN1",
			"custom_fields":   map[string]interface{}{"owner": "neteng", "ticket": "T-1"},
		},
		{
			"id": 2.0, "name": "leaf2", "display_url": "http://nb/dcim/devices/2/",
			"site":            map[string]interface{}{"id": 3.0, "name": "dc2", "slug": "dc2"},
			"status":          map[string]interface{}{"value": "planned", "label": "Planned"},
			"tags":            []interface{}{},
			"interface_count": 24.0,
			"serial":          "SN2",
			"custom_fields":   map[string]interface{}{"owner": "sre", "ticket": "T-2"},
		},
	}
}

// runBoth executes the same spec against a server that IGNORES ?fields= (what
// the provider used to rely on, and what the answer must keep looking like) and
// one that APPLIES it, returning both results plus the projecting run's spy.
func runBoth(t *testing.T, spec provider.QuerySpec, objects map[string]func(url.Values) []map[string]interface{}) (control, projected *provider.Result, spy *listSpy) {
	t.Helper()
	unprojected := (&fakeNetBox{honorFields: false, objects: objects}).start(t)
	control, err := unprojected.Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("control query: %v", err)
	}
	spy = &listSpy{}
	honoring := (&fakeNetBox{honorFields: true, spy: spy, objects: objects}).start(t)
	projected, err = honoring.Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("projected query: %v", err)
	}
	return control, projected, spy
}

// assertSameAnswer compares what a caller can observe: the columns, the total,
// and every value under a returned column (plus any fetch-only column the caller
// named). Rows may legitimately carry fewer OTHER keys once the request is
// projected — nothing reads them — but a single returned value differing is the
// regression this change must not have.
func assertSameAnswer(t *testing.T, control, projected *provider.Result, alsoReadable ...string) {
	t.Helper()
	if !reflect.DeepEqual(control.Columns, projected.Columns) {
		t.Fatalf("columns changed: %v -> %v", control.Columns, projected.Columns)
	}
	if control.Total != projected.Total {
		t.Errorf("total changed: %d -> %d", control.Total, projected.Total)
	}
	if len(control.Rows) != len(projected.Rows) {
		t.Fatalf("row count changed: %d -> %d", len(control.Rows), len(projected.Rows))
	}
	read := append(append([]string{}, control.Columns...), alsoReadable...)
	for i := range control.Rows {
		for _, c := range read {
			want, got := control.Rows[i][c], projected.Rows[i][c]
			if !reflect.DeepEqual(want, got) {
				t.Errorf("row %d column %q: %#v -> %#v", i, c, want, got)
			}
		}
	}
}

func TestQuerySendsFieldsProjection(t *testing.T) {
	objects := map[string]func(url.Values) []map[string]interface{}{
		"dcim/devices": fixed(deviceFixtures()...),
	}
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "site", "serial"}, Limit: 100}
	control, projected, spy := runBoth(t, spec, objects)

	got, ok := spy.fieldsFor("dcim/devices")
	if !ok {
		t.Fatal("no ?fields= projection was sent: NetBox serialized all 66 properties to return 3")
	}
	if got != "name,site,serial" {
		t.Errorf("fields = %q, want %q", got, "name,site,serial")
	}
	assertSameAnswer(t, control, projected)
}

func TestQueryProjectionRequestsDerivedColumnBases(t *testing.T) {
	// Every one of these columns is DERIVED by flattenObject from a property with
	// a different name — except interface_count, which is a real device property
	// whose name happens to look derived. Both kinds have to survive.
	objects := map[string]func(url.Values) []map[string]interface{}{
		"dcim/devices": fixed(deviceFixtures()...),
	}
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100, Fields: []string{
		"name", "site_id", "site_slug", "status_value", "tags_count", "interface_count", "cf_owner",
	}}
	control, projected, spy := runBoth(t, spec, objects)

	sent := spy.fieldSet(t, "dcim/devices")
	for _, want := range []string{"name", "site", "status", "tags", "interface_count", "custom_fields"} {
		if !sent[want] {
			t.Errorf("projection %v omits %q — the column derived from it comes back empty", sortedKeysOf(sent), want)
		}
	}
	assertSameAnswer(t, control, projected)

	// And the values are actually there, not merely the column headings.
	if got := projected.Rows[0]["cf_owner"]; got != "neteng" {
		t.Errorf("cf_owner = %#v, want %q (custom_fields must be requested for cf_* columns)", got, "neteng")
	}
	if got := projected.Rows[0]["site_slug"]; got != "dc1" {
		t.Errorf("site_slug = %#v, want %q", got, "dc1")
	}
	if got := projected.Rows[0]["status_value"]; got != "active" {
		t.Errorf("status_value = %#v, want %q", got, "active")
	}
	if got := projected.Rows[0]["tags_count"]; got != 1.0 {
		t.Errorf("tags_count = %#v, want 1", got)
	}
}

func sortedKeysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// utilizationObjects models a VRF-scoped leaf prefix. The child-IP endpoint
// answers differently per vrf_id, so a projection that drops `vrf` does not
// error — it silently counts the GLOBAL table instead of the prefix's own VRF.
func utilizationObjects() map[string]func(url.Values) []map[string]interface{} {
	ips := make([]map[string]interface{}, 0, 100)
	for i := 0; i < 100; i++ {
		ips = append(ips, map[string]interface{}{"address": "10.0.0." + string(rune('0'+i%10)) + "/24"})
	}
	return map[string]func(url.Values) []map[string]interface{}{
		"ipam/prefixes": func(q url.Values) []map[string]interface{} {
			if q.Get("within") != "" { // container child-prefix lookup
				return []map[string]interface{}{{"prefix": "10.1.0.0/24"}, {"prefix": "10.1.1.0/24"}}
			}
			return []map[string]interface{}{
				{
					"id": 1.0, "prefix": "10.0.0.0/24", "description": "leaf",
					"status":        map[string]interface{}{"value": "active", "label": "Active"},
					"is_pool":       false,
					"mark_utilized": false,
					"vrf":           map[string]interface{}{"id": 7.0, "name": "blue"},
				},
				{
					"id": 2.0, "prefix": "10.1.0.0/16", "description": "container",
					"status":        map[string]interface{}{"value": "container", "label": "Container"},
					"is_pool":       false,
					"mark_utilized": false,
					"vrf":           nil,
				},
			}
		},
		"ipam/ip-addresses": func(q url.Values) []map[string]interface{} {
			if q.Get("vrf_id") == "7" {
				// The prefix's own VRF holds exactly two addresses.
				return []map[string]interface{}{{"address": "10.0.0.1/24"}, {"address": "10.0.0.2/24"}}
			}
			// The global table holds a hundred. Reachable only by dropping `vrf`.
			return ips
		},
		"ipam/ip-ranges": fixed(),
	}
}

func TestQueryProjectionKeepsUtilizationSources(t *testing.T) {
	spec := provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
		Limit:      100,
	}
	control, projected, spy := runBoth(t, spec, utilizationObjects())

	sent := spy.fieldSet(t, "ipam/prefixes")
	for _, want := range []string{"prefix", "status", "is_pool", "mark_utilized", "vrf", "start_address", "end_address", "size"} {
		if !sent[want] {
			t.Errorf("projection %v omits %q — utilization is then computed from a property that was never fetched", sortedKeysOf(sent), want)
		}
	}
	for _, never := range utilizationFieldNames() {
		if sent[never] {
			t.Errorf("projection asks NetBox for %q, which is computed here and is not a NetBox field", never)
		}
	}

	assertSameAnswer(t, control, projected)

	// State the numbers outright: the VRF-scoped leaf has 2 used of 254 usable.
	// Losing `vrf` would count the global table's 100 and still return HTTP 200.
	if got := projected.Rows[0]["used"]; got != 2.0 {
		t.Errorf("used = %#v, want 2 (the prefix's own VRF); 100 means vrf was dropped", got)
	}
	if got := projected.Rows[0]["available"]; got != 252.0 {
		t.Errorf("available = %#v, want 252", got)
	}
	// The container's utilization comes from child PREFIXES (512 of 65536), which
	// is only reached because `status` said "container".
	if got := projected.Rows[1]["used"]; got != 512.0 {
		t.Errorf("container used = %#v, want 512; a dropped status turns it into a leaf", got)
	}
}

// TestQueryComputedOnlySelectionFetchesOnce covers the selection an alert table
// or a variable makes when it wants the number and nothing else: Fields is only
// computed columns, so the projection asks for the properties utilization is
// computed FROM and none of the requested names can come back in the response.
// The safety net must not read that as an unhonoured projection — refetching
// here throws away a correct answer and re-reads up to MaxLimit whole objects,
// which is the cost the projection exists to avoid.
func TestQueryComputedOnlySelectionFetchesOnce(t *testing.T) {
	for _, fields := range [][]string{
		{"utilization"},
		{"utilization", "used", "available"},
	} {
		t.Run(strings.Join(fields, ","), func(t *testing.T) {
			spec := provider.QuerySpec{ObjectType: "ipam/prefixes", Fields: fields, Limit: 100}
			control, projected, spy := runBoth(t, spec, utilizationObjects())

			if n := spy.topLevelLists("ipam/prefixes"); n != 1 {
				t.Errorf("ipam/prefixes was listed %d times, want 1: the projection guard refetched every object unprojected", n)
			}
			assertSameAnswer(t, control, projected)
			if !reflect.DeepEqual(projected.Columns, fields) {
				t.Fatalf("columns = %v, want %v", projected.Columns, fields)
			}
			// The numbers still have to be the computed ones, not blanks.
			if got := projected.Rows[0]["utilization"]; got != 0.0 {
				t.Errorf("utilization = %#v, want 0 (2 of 254 used)", got)
			}
		})
	}
}

func TestQueryProjectionIncludesKeyFields(t *testing.T) {
	objects := map[string]func(url.Values) []map[string]interface{}{
		"dcim/devices": fixed(deviceFixtures()...),
	}
	// "Return fields" is just name; the join key reads site_slug out of the row.
	spec := provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
		KeyFields:  []string{"site_slug", "status"},
		Limit:      100,
	}
	control, projected, spy := runBoth(t, spec, objects)

	sent := spy.fieldSet(t, "dcim/devices")
	for _, want := range []string{"name", "site", "status"} {
		if !sent[want] {
			t.Errorf("projection %v omits %q — a join key reading it would derive an empty column on every row", sortedKeysOf(sent), want)
		}
	}
	assertSameAnswer(t, control, projected, "site_slug", "status")

	if got := projected.Rows[0]["site_slug"]; got != "dc1" {
		t.Errorf("row is missing the join-key source: site_slug = %#v, want %q", got, "dc1")
	}
	// A key source must not become a column: joining on a field is not asking to
	// display it, and adding it would relayout every existing panel.
	if !reflect.DeepEqual(projected.Columns, []string{"name"}) {
		t.Errorf("columns = %v, want [name] — KeyFields must not add columns", projected.Columns)
	}
}

func TestQueryDoesNotClobberAFilterNamedFields(t *testing.T) {
	objects := map[string]func(url.Values) []map[string]interface{}{
		"dcim/devices": fixed(deviceFixtures()...),
	}
	spy := &listSpy{}
	p := (&fakeNetBox{honorFields: true, spy: spy, objects: objects}).start(t)
	_, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Filters:    []provider.Filter{{Field: "fields", Value: "serial"}},
		Fields:     []string{"name", "site"},
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got, ok := spy.fieldsFor("dcim/devices")
	if !ok {
		t.Fatal("the user's fields filter disappeared from the request")
	}
	if got != "serial" {
		t.Errorf("fields = %q, want %q: the projection overwrote the user's own filter", got, "serial")
	}
}

func TestQuerySendsNoProjectionForAllColumns(t *testing.T) {
	objects := map[string]func(url.Values) []map[string]interface{}{
		"dcim/devices": fixed(deviceFixtures()...),
	}
	spy := &listSpy{}
	p := (&fakeNetBox{honorFields: true, spy: spy, objects: objects}).start(t)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if v, ok := spy.fieldsFor("dcim/devices"); ok {
		t.Errorf("an unrestricted query sent ?fields=%q; no projection can express every column", v)
	}
	if len(res.Columns) < 10 {
		t.Errorf("columns = %v, want the full flattened set", res.Columns)
	}
}

// TestQueryRefetchesWhenProjectionYieldsNothing pins the fallback: a NetBox that
// answers a projected request with objects carrying none of the requested
// properties must cost the user a second request, not their whole table.
func TestQueryRefetchesWhenProjectionYieldsNothing(t *testing.T) {
	var mu sync.Mutex
	var projectedReqs, plainReqs int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Query().Get("fields") != "" {
			projectedReqs++
		} else {
			plainReqs++
		}
		projected := r.URL.Query().Get("fields") != ""
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if projected {
			// A serializer that discards the projection entirely.
			_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[{},{}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[{"name":"leaf1"},{"name":"leaf2"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := New(srv.URL, "token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", Fields: []string{"name"}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !reflect.DeepEqual(res.Columns, []string{"name"}) {
		t.Fatalf("columns = %v, want [name]: an unhonoured projection must fall back, not empty the table", res.Columns)
	}
	if res.Rows[0]["name"] != "leaf1" {
		t.Errorf("row 0 = %#v, want the unprojected value", res.Rows[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if projectedReqs == 0 || plainReqs == 0 {
		t.Errorf("projected=%d plain=%d, want one of each", projectedReqs, plainReqs)
	}
}

// TestVariableQueryValuesSurviveProjection is the variable-list path: the
// frontend sends fields=[valueField,textField] as FLATTENED names and then reads
// row[valueField] straight off the resource response. A projection that fetched
// the wrong property leaves both reads empty and the dashboard's variable
// dropdown simply has nothing in it — no error to explain why.
func TestVariableQueryValuesSurviveProjection(t *testing.T) {
	objects := map[string]func(url.Values) []map[string]interface{}{
		"dcim/devices": fixed(deviceFixtures()...),
	}
	for _, tc := range []struct {
		valueField, textField string
		want                  []string
	}{
		{"name", "name", []string{"leaf1", "leaf2"}},
		{"site_slug", "site", []string{"dc1", "dc2"}},
		{"status_value", "status", []string{"active", "planned"}},
		// The pairs below put a resolvable textField beside a derived valueField
		// on purpose. When BOTH names miss, the response is empty enough for the
		// refetch guard to catch it; when only the value name misses, the query
		// looks perfectly healthy and the dropdown is simply blank. That is the
		// shape this path actually fails in.
		{"site_slug", "name", []string{"dc1", "dc2"}},
		{"cf_owner", "name", []string{"neteng", "sre"}},
		{"status_value", "name", []string{"active", "planned"}},
	} {
		t.Run(tc.valueField+"-"+tc.textField, func(t *testing.T) {
			p := (&fakeNetBox{honorFields: true, objects: objects}).start(t)
			res, err := p.Query(context.Background(), provider.QuerySpec{
				ObjectType: "dcim/devices",
				Fields:     []string{tc.valueField, tc.textField},
				Limit:      1000,
			})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			var got []string
			for _, row := range res.Rows {
				s, _ := row[tc.valueField].(string)
				got = append(got, s)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("variable values for %q = %v, want %v", tc.valueField, got, tc.want)
			}
		})
	}
}

func TestFetchableFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []string
		want   []string
	}{
		{"no selection stays empty", nil, []string{}},
		{"plain columns pass through", []string{"prefix", "status"}, []string{"prefix", "status"}},
		{"computed columns are removed", []string{"prefix", "utilization", "used", "available"}, []string{"prefix"}},
		{"a selection of only computed columns is empty", utilizationFieldNames(), []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fetchableFields(tc.fields); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fetchableFields(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

func TestProjectionValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []string
		extra  []string
		want   string
	}{
		{"nothing to ask for is no projection", nil, nil, ""},
		// Only the computed columns were selected: nothing of the selection is a
		// NetBox property, but the properties utilization is computed from still
		// have to be fetched, so the request stays projected.
		{"computed-only selection still projects its sources",
			fetchableFields([]string{"utilization", "used"}), []string{"prefix", "vrf"}, "prefix,vrf"},
		{"plain columns pass through", []string{"name", "serial"}, nil, "name,serial"},
		{"nested id sends the object too", []string{"site_id"}, nil, "site_id,site"},
		{"slug sends the object too", []string{"site_slug"}, nil, "site_slug,site"},
		{"choice value sends the choice too", []string{"status_value"}, nil, "status_value,status"},
		{"list count sends the list too", []string{"tags_count"}, nil, "tags_count,tags"},
		{"a real _count property is kept as well", []string{"interface_count"}, nil, "interface_count,interface"},
		{"custom fields map to their container", []string{"cf_owner"}, nil, "cf_owner,custom_fields"},
		{"derived custom field maps too", []string{"cf_site_id"}, nil, "cf_site_id,custom_fields,cf_site"},
		{"only one suffix is stripped", []string{"vlan_group_id"}, nil, "vlan_group_id,vlan_group"},
		{"computed columns are never asked for",
			fetchableFields([]string{"prefix", "utilization", "used", "available"}), nil, "prefix"},
		{"extras are appended and de-duplicated", []string{"name", "site"}, []string{"site", "status_value"}, "name,site,status_value,status"},
		{"duplicates collapse", []string{"name", "name", "site_id", "site"}, nil, "name,site_id,site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectionValue(tc.fields, tc.extra); got != tc.want {
				t.Errorf("projectionValue(%v, %v) = %q, want %q", tc.fields, tc.extra, got, tc.want)
			}
		})
	}
}

// rejectingNetBox answers a list request with HTTP 500 whenever the ?fields=
// projection names a property it cannot serialize, and serves the objects
// normally otherwise.
//
// This is NetBox's real behaviour, not a hypothetical: `?fields=` feeds
// prefetch_related(), so a name that looks like a relation and is not one raises
// AttributeError. Verified on NetBox 4.4.10 —
// dcim/sites/?fields=prefix answers HTTP 500 with
// "Cannot find 'prefix' on Site object, 'prefix' is an invalid parameter to
// prefetch_related()". A `prefix_count` column on dcim/sites sends exactly that
// name, because appendUpstreamNames must strip the derived suffix (`tags_count`
// really does come from `tags`) and nothing in the column name says which is
// which.
type rejectingNetBox struct {
	poison  string // the projection name that provokes the 500
	spy     *listSpy
	objects []map[string]interface{}
}

func (f *rejectingNetBox) start(t *testing.T) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/sites/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if f.spy != nil {
			rec := url.Values{}
			for k, v := range q {
				rec[k] = v
			}
			rec.Set("__path", "dcim/sites")
			f.spy.record(rec)
		}
		for _, n := range strings.Split(q.Get("fields"), ",") {
			if n == f.poison {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error": "Cannot find '` + f.poison + `' on Site object"}`))
				return
			}
		}
		results := make([]map[string]interface{}, 0, len(f.objects))
		for _, o := range f.objects {
			results = append(results, applyNetBoxFields(o, q.Get("fields")))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": len(f.objects), "next": nil, "results": results,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", &http.Client{Timeout: 5 * time.Second})
}

func siteFixtures() []map[string]interface{} {
	return []map[string]interface{}{
		{"id": 1.0, "name": "dc1", "prefix_count": 4.0, "device_count": 7.0,
			"tags": []interface{}{}, "config_context": map[string]interface{}{"k": "v"}},
		{"id": 2.0, "name": "dc2", "prefix_count": 1.0, "device_count": 4.0,
			"tags": []interface{}{}, "config_context": map[string]interface{}{"k": "v"}},
	}
}

// A projection NetBox refuses must cost the query its speed, never its answer.
func TestQueryRefetchesWhenProjectionIsRejected(t *testing.T) {
	spy := &listSpy{}
	p := (&rejectingNetBox{poison: "prefix", spy: spy, objects: siteFixtures()}).start(t)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/sites",
		Fields:     []string{"name", "prefix_count"},
	})
	if err != nil {
		t.Fatalf("query: %v — a rejected projection must not fail the query", err)
	}
	if want := []string{"name", "prefix_count"}; !reflect.DeepEqual(res.Columns, want) {
		t.Fatalf("columns = %v, want %v", res.Columns, want)
	}
	if len(res.Rows) != 2 || res.Rows[0]["prefix_count"] != 4.0 || res.Rows[1]["name"] != "dc2" {
		t.Fatalf("rows = %v, want the two sites with their prefix_count", res.Rows)
	}
	if res.Total != 2 {
		t.Fatalf("total = %d, want 2", res.Total)
	}

	// Exactly two requests: the rejected projected one, then the whole-object
	// refetch — which must carry neither ?fields= nor the config-context
	// exclusion that rides with it.
	if n := spy.topLevelLists("dcim/sites"); n != 2 {
		t.Fatalf("requests = %d, want 2 (projected, then unprojected)", n)
	}
	spy.mu.Lock()
	last := spy.queries[len(spy.queries)-1]
	spy.mu.Unlock()
	if last.Has("fields") || last.Has("exclude") {
		t.Fatalf("refetch still carried %v, want no fields/exclude", last)
	}
}

// The retry is for a request NetBox REFUSED, not for one it never answered: a
// transport failure has nothing to do with the projection, and repeating it
// costs another full timeout on a query that is already slow.
func TestProjectionRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"a 500 is the observed rejection", &APIError{Status: 500}, true},
		{"a 400 is the stricter spelling of the same", &APIError{Status: 400}, true},
		// Was true, on the reasoning that a second 404 is harmless. It is not
		// about the projection — the test name always said so — and once this
		// predicate became an allowlist of what a bad projection actually
		// causes, "harmless" stopped being a reason to include it. A 404 means
		// the object type is not there; dropping ?fields= cannot change that.
		{"404 is the endpoint, not the projection", &APIError{Status: 404}, false},
		{"502 is load shedding, already retried", &APIError{Status: 502}, false},
		{"503 likewise", &APIError{Status: 503}, false},
		{"504 likewise", &APIError{Status: 504}, false},
		{"a transport failure never reached NetBox", &url.Error{Op: "Get", Err: context.DeadlineExceeded}, false},
		{"a decode failure is not an answer either", errors.New("decode"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectionRejected(tc.err); got != tc.want {
				t.Errorf("projectionRejected(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The projection fallback costs a second unprojected fetch of up to MaxLimit
// whole objects, so it must fire only where dropping ?fields= could be the cure.
// It used to be a denylist — everything except 502/503/504 — which swept in 429
// and re-issued the expensive request while NetBox was throttling us, through
// the very door getListPageRetry closes on purpose.
func TestProjectionRejected_OnlyForStatusesAProjectionCanCause(t *testing.T) {
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{http.StatusBadRequest, true, "NetBox rejecting the fields parameter outright"},
		{http.StatusInternalServerError, true, "a projected name that is a relation on another model hits prefetch_related"},
		{http.StatusTooManyRequests, false, "re-requesting unprojected ignores Retry-After and doubles load while throttled"},
		{http.StatusUnauthorized, false, "the token will not become valid on the second try"},
		{http.StatusForbidden, false, "permission will not change on the second try"},
		{http.StatusNotFound, false, "the object type is not there at all"},
		{http.StatusBadGateway, false, "transient; getListPageRetry already owns it"},
		{http.StatusServiceUnavailable, false, "transient; getListPageRetry already owns it"},
		{http.StatusGatewayTimeout, false, "transient; getListPageRetry already owns it"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &APIError{Status: tc.status, URL: "http://x/api/dcim/devices/"})
			if got := projectionRejected(err); got != tc.want {
				t.Errorf("projectionRejected(%d) = %v, want %v — %s", tc.status, got, tc.want, tc.why)
			}
		})
	}
	if projectionRejected(errors.New("not an API error")) {
		t.Error("a transport failure is not a projection rejection")
	}
}
