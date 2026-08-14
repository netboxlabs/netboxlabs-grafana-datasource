package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// dimSchemaFixture is a miniature NetBox OpenAPI document shaped exactly like
// the real one: list paths whose 200 response is a Paginated<X>List whose
// results items point at the object schema, foreign keys expressed as BriefX
// refs (bare and nullable-allOf), a list-valued ref, and an inline choice.
const dimSchemaFixture = `{
 "paths": {
  "/api/dcim/devices/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedDeviceWithConfigContextList"}}}}}}},
  "/api/dcim/sites/":   {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedSiteList"}}}}}}},
  "/api/tenancy/tenants/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedTenantList"}}}}}}},
  "/api/extras/tags/":   {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedTagList"}}}}}}},
  "/api/dcim/interfaces/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedInterfaceList"}}}}}}},
  "/api/dcim/devices/{id}/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/DeviceWithConfigContext"}}}}}}}
 },
 "components": {"schemas": {
  "PaginatedDeviceWithConfigContextList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/DeviceWithConfigContext"}}}},
  "PaginatedSiteList":   {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/Site"}}}},
  "PaginatedTenantList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/Tenant"}}}},
  "PaginatedTagList":    {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/Tag"}}}},
  "PaginatedInterfaceList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/Interface"}}}},
  "DeviceWithConfigContext": {"properties": {
    "id":     {"type": "integer"},
    "name":   {"type": "string"},
    "site":   {"$ref": "#/components/schemas/BriefSite"},
    "tenant": {"allOf": [{"$ref": "#/components/schemas/BriefTenant"}], "nullable": true},
    "tags":   {"type": "array", "items": {"$ref": "#/components/schemas/NestedTag"}},
    "custom_fields": {"type": "object"},
    "status": {"type": "object", "properties": {
       "value": {"type": "string", "enum": ["offline", "active", "decommissioning"]},
       "label": {"type": "string", "enum": ["Offline", "Active", "Decommissioning"]}
    }}
  }},
  "Interface": {"properties": {"device": {"$ref": "#/components/schemas/BriefDevice"}}},
  "Site":   {"properties": {"name": {"type": "string"}}},
  "Tenant": {"properties": {"name": {"type": "string"}}},
  "Tag":    {"properties": {"name": {"type": "string"}}}
 }}
}`

// dimSchemaWithSlugFilter is the same document with the sites list ALSO
// advertising its query parameters, the way a real NetBox schema does: `slug`
// plus its lookup variants, of which `slug__ic` (case-insensitive contains) is
// the one that can carry a typed substring. The base fixture deliberately
// advertises none, so the two fixtures cover both halves of "check what the
// schema actually offers".
var dimSchemaWithSlugFilter = strings.Replace(dimSchemaFixture,
	`"/api/dcim/sites/":   {"get": {`,
	`"/api/dcim/sites/":   {"get": {"parameters": [`+
		`{"name":"q","in":"query"},{"name":"slug","in":"query"},{"name":"slug__ic","in":"query"}],`,
	1)

func TestParseDimensions(t *testing.T) {
	idx, err := parseDimensions([]byte(dimSchemaFixture))
	if err != nil {
		t.Fatalf("parseDimensions: %v", err)
	}

	cases := []struct {
		field    string
		wantKind dimensionKind
		wantEnd  string
	}{
		{"site", dimRelated, "dcim/sites"},        // bare $ref, Brief prefix stripped
		{"tenant", dimRelated, "tenancy/tenants"}, // nullable allOf spelling
		{"status", dimChoice, ""},                 // inline enumeration
		{"name", dimNone, ""},                     // plain scalar
		{"tags", dimNone, ""},                     // list-valued: column is a joined string
		{"custom_fields", dimNone, ""},            // hoisted to cf_*, not a dimension
	}
	for _, c := range cases {
		dim, base, ok := idx.resolve("dcim/devices", c.field)
		if !ok {
			t.Errorf("resolve(dcim/devices, %s): not resolved", c.field)
			continue
		}
		if base != c.field {
			t.Errorf("resolve(%s) base = %q, want %q", c.field, base, c.field)
		}
		if dim.kind != c.wantKind || dim.endpoint != c.wantEnd {
			t.Errorf("resolve(%s) = kind %v endpoint %q, want kind %v endpoint %q",
				c.field, dim.kind, dim.endpoint, c.wantKind, c.wantEnd)
		}
	}

	// A BriefDevice must resolve back to the devices list even though that list
	// returns DeviceWithConfigContext.
	if dim, _, ok := idx.resolve("dcim/interfaces", "device"); !ok || dim.endpoint != "dcim/devices" {
		t.Errorf("interfaces.device -> %q (ok=%v), want dcim/devices", dim.endpoint, ok)
	}

	// A detail path ({id}) must not register an object type.
	if _, ok := idx["dcim/devices/{id}"]; ok {
		t.Error("detail path leaked into the index")
	}
}

func TestDimIndexResolve_FlattenedColumnVariants(t *testing.T) {
	idx, err := parseDimensions([]byte(dimSchemaFixture))
	if err != nil {
		t.Fatalf("parseDimensions: %v", err)
	}
	cases := []struct{ field, wantBase string }{
		{"site", "site"},
		{"site_id", "site"},
		{"site_slug", "site"},
		{"status_value", "status"},
		{"tags_count", "tags"},
		{"cf_owner", "custom_fields"},
	}
	for _, c := range cases {
		_, base, ok := idx.resolve("dcim/devices", c.field)
		if !ok || base != c.wantBase {
			t.Errorf("resolve(%s) base = %q ok=%v, want %q", c.field, base, ok, c.wantBase)
		}
	}
	if _, _, ok := idx.resolve("dcim/devices", "utilization_percent"); ok {
		t.Error("a synthetic column must not resolve to a property")
	}
	if _, _, ok := idx.resolve("dcim/nonexistent", "site"); ok {
		t.Error("an unknown object type must not resolve")
	}
}

// dimServer emulates the slice of NetBox that FieldValues touches: a schema
// endpoint, a fact table whose reported count is independent of the page it
// serves, and a related list endpoint. It records every request path+query so
// tests can assert which endpoints were (and were not) consulted.
type dimServer struct {
	*httptest.Server
	mu         sync.Mutex
	requests   []string
	deviceRows []string // JSON objects served by the devices list
	deviceLen  int      // reported count; > len(deviceRows) means "sample is a fraction"
	siteRows   []string
	schema     string
}

func newDimServer(t *testing.T) *dimServer {
	t.Helper()
	d := &dimServer{schema: dimSchemaFixture}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/schema/", func(w http.ResponseWriter, r *http.Request) {
		d.record(r)
		if d.schema == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, d.schema)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		d.record(r)
		rows := d.deviceRows
		if f := r.URL.Query().Get("fields"); f != "" {
			rows = projectFields(rows, f)
		}
		writePage(w, d.deviceLen, rows)
	})
	// The sites list emulates the filtering NetBox actually implements, because
	// WHICH parameter carries the user's substring is the thing under test:
	//
	//   - ?q= is SiteFilterSet.search, a TEXT search over name/facility/
	//     description/addresses/comments. It reads neither the numeric id nor the
	//     slug (verified against NetBox 4.4: /api/dcim/sites/?q=3 answers 0 on an
	//     instance that has site id 3).
	//   - ?slug__ic= is the slug's own case-insensitive "contains" lookup.
	//
	// A cruder stub that matched the substring against the whole raw JSON row
	// would let a pushed-down ?q=123 "find" `{"id":123,...}` and hide the bug.
	mux.HandleFunc("/api/dcim/sites/", func(w http.ResponseWriter, r *http.Request) {
		d.record(r)
		rows := d.siteRows
		if q := r.URL.Query().Get("q"); q != "" {
			rows = keepRows(rows, func(o map[string]interface{}) bool { return fieldContains(o, "name", q) })
		}
		if s := r.URL.Query().Get("slug__ic"); s != "" {
			rows = keepRows(rows, func(o map[string]interface{}) bool { return fieldContains(o, "slug", s) })
		}
		writePage(w, len(rows), rows)
	})
	d.Server = httptest.NewServer(mux)
	t.Cleanup(d.Close)
	return d
}

func (d *dimServer) record(r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, r.URL.Path+"?"+r.URL.Query().Encode())
}

func (d *dimServer) hits(pathPrefix string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, req := range d.requests {
		if strings.HasPrefix(req, pathPrefix) {
			n++
		}
	}
	return n
}

func (d *dimServer) lastQuery(pathPrefix string) url.Values {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.requests) - 1; i >= 0; i-- {
		req := d.requests[i]
		if strings.HasPrefix(req, pathPrefix) {
			_, raw, _ := strings.Cut(req, "?")
			v, _ := url.ParseQuery(raw)
			return v
		}
	}
	return nil
}

// projectFields mimics NetBox's ?fields=: it keeps only the named keys, and answers an
// unknown name with an empty object rather than an error.
func projectFields(rows []string, fields string) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(row), &obj); err != nil {
			out = append(out, row)
			continue
		}
		kept := map[string]json.RawMessage{}
		for _, f := range strings.Split(fields, ",") {
			if v, ok := obj[f]; ok {
				kept[f] = v
			}
		}
		b, _ := json.Marshal(kept)
		out = append(out, string(b))
	}
	return out
}

// keepRows filters raw JSON rows by a predicate over the decoded object.
func keepRows(rows []string, keep func(map[string]interface{}) bool) []string {
	var out []string
	for _, row := range rows {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(row), &obj); err != nil {
			continue
		}
		if keep(obj) {
			out = append(out, row)
		}
	}
	return out
}

// fieldContains is one icontains test against ONE named property — never the
// whole object.
func fieldContains(obj map[string]interface{}, key, sub string) bool {
	s, _ := obj[key].(string)
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func writePage(w http.ResponseWriter, count int, rows []string) {
	_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"previous":null,"results":[%s]}`, count, strings.Join(rows, ","))
}

func newDimProvider(d *dimServer) *Provider {
	return New(d.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
}

// TestFieldValues_CompleteSampleWins is the small-instance guarantee: when the
// object type holds no more rows than one sample page, the sample IS the
// population, so its values are returned verbatim and the dimension endpoint is
// never touched — a site with no devices must NOT appear.
func TestFieldValues_CompleteSampleWins(t *testing.T) {
	d := newDimServer(t)
	d.deviceRows = []string{
		`{"id":1,"name":"a","site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`,
		`{"id":2,"name":"b","site":{"id":2,"display":"NYC1","name":"NYC1","slug":"nyc1"}}`,
	}
	d.deviceLen = 2
	d.siteRows = []string{
		`{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}`,
		`{"id":2,"display":"NYC1","name":"NYC1","slug":"nyc1"}`,
		`{"id":3,"display":"ZZZ9","name":"ZZZ9","slug":"zzz9"}`, // exists, but no devices
	}

	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "site", "", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"AMS1", "NYC1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want %v (the complete sample, not the whole dimension)", got, want)
	}
	if n := d.hits("/api/dcim/sites/"); n != 0 {
		t.Errorf("dimension endpoint hit %d times; a complete sample must not need it", n)
	}
}

// TestFieldValues_IncompleteSampleUsesDimension is the huge-instance fix: once
// the reported count exceeds what one page can hold, the sample is an arbitrary
// slice, so the values come from the related endpoint instead.
func TestFieldValues_IncompleteSampleUsesDimension(t *testing.T) {
	d := newDimServer(t)
	d.deviceRows = []string{`{"id":1,"name":"a","site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`}
	d.deviceLen = 6824570
	d.siteRows = []string{
		`{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}`,
		`{"id":2,"display":"NYC1","name":"NYC1","slug":"nyc1"}`,
		`{"id":3,"display":"ZZZ9","name":"ZZZ9","slug":"zzz9"}`,
	}

	p := newDimProvider(d)
	got, err := p.FieldValues(context.Background(), "dcim/devices", "site", "", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"AMS1", "NYC1", "ZZZ9"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want the whole dimension %v", got, want)
	}
	if q := d.lastQuery("/api/dcim/sites/"); q.Get("brief") != "true" {
		t.Errorf("dimension query = %v, want brief=true", q)
	}

	// The flattened variants must render exactly as the query path renders them.
	for _, c := range []struct {
		field string
		want  []string
	}{
		{"site_id", []string{"1", "2", "3"}},
		{"site_slug", []string{"ams1", "nyc1", "zzz9"}},
	} {
		got, err := p.FieldValues(context.Background(), "dcim/devices", c.field, "", 0)
		if err != nil {
			t.Fatalf("FieldValues(%s): %v", c.field, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %v, want %v", c.field, got, c.want)
		}
	}
}

// TestFieldValues_SubstringGoesUpstream proves the tail is reachable: the typed
// substring is sent to the dimension endpoint, not merely applied to a page that
// was already fetched.
func TestFieldValues_SubstringGoesUpstream(t *testing.T) {
	d := newDimServer(t)
	d.deviceRows = []string{`{"id":1,"name":"a","site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`}
	d.deviceLen = 6824570
	d.siteRows = []string{
		`{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}`,
		`{"id":2,"display":"Yonkers","name":"Yonkers","slug":"yonkers"}`,
	}

	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "site", "Yonkers", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"Yonkers"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want %v", got, want)
	}
	if q := d.lastQuery("/api/dcim/sites/"); q.Get("q") != "Yonkers" {
		t.Errorf("dimension query = %v, want q=Yonkers sent upstream", q)
	}
}

// TestFieldValues_IdSubstringIsNotPushedUpstream: flattening turns one property
// into several columns, and `site_id` renders a NUMBER. NetBox's ?q= is a text
// search that never reads the id, so pushing the typed digits down as ?q=
// EXCLUDES the very row the user is typing towards and the dropdown comes back
// empty for a value that exists. The substring belongs locally here.
func TestFieldValues_IdSubstringIsNotPushedUpstream(t *testing.T) {
	d := newDimServer(t)
	d.deviceRows = []string{`{"id":1,"site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`}
	d.deviceLen = 6824570
	d.siteRows = []string{
		`{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}`,
		`{"id":123,"display":"Yonkers","name":"Yonkers","slug":"yonkers"}`,
	}

	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "site_id", "123", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("site_id values = %v, want %v", got, want)
	}
	if q := d.lastQuery("/api/dcim/sites/"); q.Get("q") != "" {
		t.Errorf("dimension query = %v, want no q: a text search cannot match an id", q)
	}
}

// TestFieldValues_SlugSubstringUsesTheSlugFilter: `site_slug` renders the slug,
// which the generic ?q= does not search either — but the slug has its own
// filter, and where the schema advertises the case-insensitive contains lookup
// it is EXACTLY the local test, evaluated over the whole dimension rather than
// one page.
func TestFieldValues_SlugSubstringUsesTheSlugFilter(t *testing.T) {
	d := newDimServer(t)
	d.schema = dimSchemaWithSlugFilter
	d.deviceRows = []string{`{"id":1,"site":{"id":1,"display":"Amsterdam 1","name":"Amsterdam 1","slug":"ams1"}}`}
	d.deviceLen = 6824570
	d.siteRows = []string{
		`{"id":1,"display":"Amsterdam 1","name":"Amsterdam 1","slug":"ams1"}`,
		`{"id":2,"display":"New York 1","name":"New York 1","slug":"nyc1"}`,
	}

	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "site_slug", "ams1", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"ams1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("site_slug values = %v, want %v", got, want)
	}
	q := d.lastQuery("/api/dcim/sites/")
	if q.Get("slug__ic") != "ams1" {
		t.Errorf("dimension query = %v, want slug__ic=ams1 upstream", q)
	}
	if q.Get("q") != "" {
		t.Errorf("dimension query = %v, want no q alongside the slug filter", q)
	}
}

// TestFieldValues_SlugSubstringFallsBackToLocalFilter: the same column on a
// model whose schema advertises no slug lookup. Nothing goes upstream and the
// substring is applied to the page that came back — narrower than the slug
// filter, but never wrong.
func TestFieldValues_SlugSubstringFallsBackToLocalFilter(t *testing.T) {
	d := newDimServer(t) // the base fixture advertises no query parameters
	d.deviceRows = []string{`{"id":1,"site":{"id":1,"display":"Amsterdam 1","name":"Amsterdam 1","slug":"ams1"}}`}
	d.deviceLen = 6824570
	d.siteRows = []string{
		`{"id":1,"display":"Amsterdam 1","name":"Amsterdam 1","slug":"ams1"}`,
		`{"id":2,"display":"New York 1","name":"New York 1","slug":"nyc1"}`,
	}

	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "site_slug", "ams1", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"ams1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("site_slug values = %v, want %v", got, want)
	}
	if q := d.lastQuery("/api/dcim/sites/"); q.Get("q") != "" || q.Get("slug__ic") != "" {
		t.Errorf("dimension query = %v, want no substring parameter this model does not implement", q)
	}
}

// TestFieldValues_ChoiceFromSchema: a choice column has no endpoint, but the
// schema enumerates it. Sampling a fraction of a multi-million-row table would miss a
// rare status; the schema cannot.
func TestFieldValues_ChoiceFromSchema(t *testing.T) {
	d := newDimServer(t)
	d.deviceRows = []string{`{"id":1,"status":{"value":"active","label":"Active"}}`}
	d.deviceLen = 6824570

	p := newDimProvider(d)
	got, err := p.FieldValues(context.Background(), "dcim/devices", "status", "", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"Active", "Decommissioning", "Offline"}; !reflect.DeepEqual(got, want) {
		t.Errorf("status labels = %v, want %v", got, want)
	}
	got, err = p.FieldValues(context.Background(), "dcim/devices", "status_value", "", 0)
	if err != nil {
		t.Fatalf("FieldValues(status_value): %v", err)
	}
	if want := []string{"active", "decommissioning", "offline"}; !reflect.DeepEqual(got, want) {
		t.Errorf("status values = %v, want %v", got, want)
	}
}

// TestFieldValues_NoDimensionFallsBackToSampling: a free-text column has no
// dimension anywhere, so it must keep its historical behaviour rather than error.
func TestFieldValues_NoDimensionFallsBackToSampling(t *testing.T) {
	d := newDimServer(t)
	d.deviceRows = []string{`{"id":1,"name":"a"}`, `{"id":2,"name":"b"}`}
	d.deviceLen = 6824570

	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "name", "", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want %v", got, want)
	}
	if q := d.lastQuery("/api/dcim/devices/"); q.Get("fields") != "name" {
		t.Errorf("sample query = %v, want the page projected to fields=name", q)
	}
}

// TestFieldValues_RejectedProjectionRefetches: NetBox answers an unknown
// ?fields= name with empty objects and HTTP 200. That must degrade to a full
// fetch, never to an empty dropdown.
func TestFieldValues_RejectedProjectionRefetches(t *testing.T) {
	rows := []string{`{"id":1,"name":"a"}`, `{"id":2,"name":"b"}`}
	var projected, plain int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/schema/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, dimSchemaFixture)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fields") != "" {
			projected++
			writePage(w, len(rows), []string{`{}`, `{}`}) // NetBox's answer to a name it will not project
			return
		}
		plain++
		writePage(w, len(rows), rows)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	got, err := p.FieldValues(context.Background(), "dcim/devices", "name", "", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want %v (a rejected projection must re-fetch)", got, want)
	}
	if projected != 1 || plain != 1 {
		t.Errorf("projected=%d plain=%d, want exactly one of each", projected, plain)
	}
}

// TestFieldValues_SchemaUnavailableKeepsSampling: an instance whose schema
// endpoint is unreachable must still serve values, unprojected, as before.
func TestFieldValues_SchemaUnavailableKeepsSampling(t *testing.T) {
	d := newDimServer(t)
	d.schema = ""
	d.deviceRows = []string{`{"id":1,"site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`}
	d.deviceLen = 6824570

	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "site", "", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"AMS1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want %v", got, want)
	}
	if q := d.lastQuery("/api/dcim/devices/"); q.Get("fields") != "" {
		t.Errorf("sample query = %v, want no projection without a schema", q)
	}
	if n := d.hits("/api/dcim/sites/"); n != 0 {
		t.Errorf("dimension endpoint hit %d times without a schema to resolve it", n)
	}
}

// TestFieldValues_DimensionFailureFallsBackToSample: a dimension that will not
// answer must cost the user a worse dropdown, not an error toast.
func TestFieldValues_DimensionFailureFallsBackToSample(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/schema/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, dimSchemaFixture)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, _ *http.Request) {
		writePage(w, 6824570, []string{`{"site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`})
	})
	mux.HandleFunc("/api/dcim/sites/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	got, err := p.FieldValues(context.Background(), "dcim/devices", "site", "", 0)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if want := []string{"AMS1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want the sampled fallback %v", got, want)
	}
}

// TestFieldValues_LimitCaps keeps the caller's cap honoured on the dimension
// path, the same way it always was on the sampling path.
func TestFieldValues_LimitCaps(t *testing.T) {
	d := newDimServer(t)
	d.deviceRows = []string{`{"site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`}
	d.deviceLen = 6824570
	d.siteRows = []string{
		`{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}`,
		`{"id":2,"display":"NYC1","name":"NYC1","slug":"nyc1"}`,
		`{"id":3,"display":"SIN1","name":"SIN1","slug":"sin1"}`,
	}
	got, err := newDimProvider(d).FieldValues(context.Background(), "dcim/devices", "site", "", 2)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("values = %v, want 2 values", got)
	}
}
