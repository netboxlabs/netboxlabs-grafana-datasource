package replicacache

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeService mimics the replica-cache read API closely enough to test against,
// including the behaviours that were MEASURED on a live instance rather than
// read off its spec: the 400 above the page size ceiling, the {"error": …}
// failure body, the LEFT-JOIN semantics of expand=, and the cursor that is
// absent on the last page.
type fakeService struct {
	mu sync.Mutex

	// schema is the catalogue the fake serves at /v1/_meta/schema. Nil means
	// the route does not exist (an older build), and the handler answers 404
	// "endpoint not found" — which the provider turns into a health-check
	// failure naming the upgrade.
	schema *fakeSchema

	// entities maps "app/model" to its rows.
	entities map[string][]map[string]interface{}
	// pageCap is the most rows a single response returns, whatever was asked.
	pageCap int
	// status, when non-zero, is returned for every list request.
	status int
	// errBody is the message sent with status.
	errBody string
	// requests records every list request's query string, for asserting pushdown.
	requests []recordedRequest
	// gate, when non-nil, parks every request after recording it until the
	// channel is closed — how a test holds callers in flight to prove they
	// share one fetch, or cancels one while others wait.
	gate chan struct{}
	// rejectCursors is how many cursor-carrying requests still answer 400
	// "cursor does not belong to this walk" — what the service says when the
	// catalogue changed underneath a walk. One models the case the client
	// recovers from; two models a service that keeps refusing.
	rejectCursors int
	// asOfs, when non-empty, is the data_as_of of the next row responses, one
	// each in order — a replica applying changes between requests.
	asOfs []string
}

type recordedRequest struct {
	entity string
	query  url.Values
}

func newFakeService() *fakeService {
	return &fakeService{
		schema:   devicesSchema(),
		entities: map[string][]map[string]interface{}{},
		pageCap:  1000,
	}
}

func (f *fakeService) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

// withSchema puts the devices fixture's catalogue in front of a bare handler,
// so a test that scripts row responses by hand does not have to script the
// schema route as well. Only /v1/_meta/schema is intercepted; everything else,
// and every request count a test keeps, sees the bare handler alone.
func withSchema(h http.HandlerFunc) http.Handler {
	schema := devicesSchema()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/_meta/schema" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema)
			return
		}
		h(w, r)
	})
}

func (f *fakeService) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/_meta/schema" {
		// Same discipline as the row branch below: under the lock, record the
		// request, honour the status/errBody knobs (Save & Test must fail on a
		// revoked token even with a warm catalogue), then serve the document.
		// The pointer is encoded after unlocking; tests that mutate the schema
		// do so under the lock and sequentially, which is enough.
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{entity: "_meta/schema", query: r.URL.Query()})
		schema, status, errBody, gate := f.schema, f.status, f.errBody, f.gate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if status != 0 && status != 200 {
			writeErr(w, status, errBody)
			return
		}
		if schema == nil {
			writeErr(w, 404, "endpoint not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema)
		return
	}

	entity := strings.TrimPrefix(r.URL.Path, "/v1/")
	q := r.URL.Query()
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{entity: entity, query: q})
	rows, ok := f.entities[entity]
	status, errBody, pageCap := f.status, f.errBody, f.pageCap
	var se *fakeEntity
	if f.schema != nil {
		if e, known := f.schema.Entities["/v1/"+entity]; known {
			se = &e
		}
	}
	var asOf *string
	if len(f.asOfs) > 0 {
		asOf, f.asOfs = &f.asOfs[0], f.asOfs[1:]
	}
	rejectCursor := f.rejectCursors > 0 && q.Get("cursor") != ""
	if rejectCursor {
		f.rejectCursors--
	}
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}

	if status != 0 {
		writeErr(w, status, errBody)
		return
	}
	// The catalogue decides what exists and what has data, as the service
	// does: an entity it does not list is no endpoint, one it lists unfed
	// answers 404 with the sentence the provider classifies.
	if f.schema != nil && se == nil {
		writeErr(w, 404, "endpoint not found")
		return
	}
	if se != nil && !se.Ingested {
		writeErr(w, 404, "no data received for this entity")
		return
	}
	if !ok {
		writeErr(w, 404, "endpoint not found")
		return
	}
	if rejectCursor {
		writeErr(w, 400, "cursor does not belong to this walk")
		return
	}
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 1000 {
		writeErr(w, 400, fmt.Sprintf("limit %d exceeds the maximum page size of 1000", l))
		return
	}

	// The columns a request may name are the schema's plus the keys it
	// expands — never the rows', which is how the service behaves and what
	// makes a projection bug surface here as the 400 it would be in
	// production. A schema-less fake (an older build) falls back to the rows.
	pk := "id"
	stored := map[string]fakeColumn{}
	if se != nil {
		pk = se.PrimaryKey
		for _, c := range se.Columns {
			stored[c.Name] = c
		}
	} else if len(rows) > 0 {
		for k := range rows[0] {
			stored[k] = fakeColumn{Name: k, Type: "VARCHAR"}
		}
	}

	// Expansions, resolved from the UNPROJECTED rows the way the service's
	// LEFT JOIN does: the derived columns are present whenever the key is
	// expanded and null when the id is null or the target row is absent. An
	// unavailable target adds nothing, and sorting on it is refused by name.
	type expansion struct {
		col, target, key string
		cols             []string
	}
	var expansions []expansion
	unavailable := map[string]string{}
	if ex := q.Get("expand"); ex != "" {
		for _, key := range strings.Split(ex, ",") {
			var found *fakeColumn
			for _, c := range stored {
				if c.References != nil && c.References.ExpandKey == key {
					cc := c
					found = &cc
				}
			}
			if found == nil {
				writeErr(w, 400, "unknown expand key: "+key)
				return
			}
			target := strings.TrimPrefix(found.References.Path, "/v1/")
			if !found.References.Available {
				unavailable[key] = strings.ReplaceAll(target, "/", "_")
				continue
			}
			expansions = append(expansions, expansion{col: found.Name, target: target, key: key, cols: found.References.Columns})
		}
	}
	f.mu.Lock()
	resolved := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		out := make(map[string]interface{}, len(row)+2*len(expansions))
		for k, v := range row {
			out[k] = v
		}
		for _, x := range expansions {
			var target map[string]interface{}
			if id, ok := toInt(row[x.col]); ok {
				for _, t := range f.entities[x.target] {
					if tid, ok := toInt(t["id"]); ok && tid == id {
						target = t
					}
				}
			}
			out[x.key] = nil
			if target != nil {
				out[x.key] = target[x.cols[0]]
			}
			for _, c := range x.cols[1:] {
				out[x.key+"_"+c] = nil
				if target != nil {
					out[x.key+"_"+c] = target[c]
				}
			}
		}
		resolved = append(resolved, out)
	}
	f.mu.Unlock()
	nameOK := func(name string) bool {
		if _, ok := stored[name]; ok {
			return true
		}
		for _, x := range expansions {
			if name == x.key {
				return true
			}
			for _, c := range x.cols[1:] {
				if name == x.key+"_"+c {
					return true
				}
			}
		}
		return false
	}

	// Filtering: an unknown column and ilike on a non-text column are refused
	// the way the service refuses them.
	filtered := resolved
	for k, vs := range q {
		if !strings.HasPrefix(k, "filter[") {
			continue
		}
		col := k[len("filter["):strings.Index(k, "]")]
		op := k[strings.Index(k, "]")+3:]
		if !nameOK(col) {
			writeErr(w, 400, "unknown column: "+col)
			return
		}
		// An expanded name filters on the target's column, so it is gated by
		// that column's catalogue entry, as a stored one is by its own.
		c, isStored := stored[col]
		if !isStored {
			for _, x := range expansions {
				tc := ""
				if col == x.key {
					tc = x.cols[0]
				} else if strings.HasPrefix(col, x.key+"_") && slices.Contains(x.cols[1:], strings.TrimPrefix(col, x.key+"_")) {
					tc = strings.TrimPrefix(col, x.key+"_")
				}
				if tc == "" {
					continue
				}
				for _, sc := range f.schema.Entities["/v1/"+x.target].Columns {
					if sc.Name == tc {
						c, isStored = sc, true
					}
				}
			}
		}
		if isStored {
			if isTextOperator(op) && c.Type != "VARCHAR" {
				writeErr(w, 400, fmt.Sprintf("operator %s requires a text column: %s is %s", op, col, c.Type))
				return
			}
			// The catalogue is derived from the handler's own operator specs,
			// so an operator it does not list is one the handler refuses — a
			// build before DATA-408 answers istartswith this way.
			if c.Operators != nil && !slices.Contains(c.Operators, op) {
				writeErr(w, 400, "invalid filter operator: "+op)
				return
			}
		}
		// host (DATA-417) takes IP addresses with no mask or zone, and one bad
		// element refuses the whole list, as the service does.
		if op == "host" {
			for _, h := range strings.Split(vs[0], ",") {
				if a, err := netip.ParseAddr(h); err != nil || a.Zone() != "" {
					writeErr(w, 400, "invalid filter value for "+col+": host takes IP addresses with no mask or zone")
					return
				}
			}
		}
		keep := []map[string]interface{}{} // the service answers [], never null, when nothing matches
		for _, row := range filtered {
			if matches(row[col], op, vs[0]) {
				keep = append(keep, row)
			}
		}
		filtered = keep
	}

	// Sorting: a stored column or an expanded key, nulls last; anything else
	// is the service's 400, including the named refusal for an unfed target.
	if srt := q.Get("sort"); srt != "" {
		key := strings.TrimPrefix(srt, "-")
		if table, bad := unavailable[key]; bad {
			writeErr(w, 400, fmt.Sprintf("cannot sort on %s: %s is not present in this replica", key, table))
			return
		}
		if !nameOK(key) {
			writeErr(w, 400, "unknown sort column: "+key)
			return
		}
		desc := strings.HasPrefix(srt, "-")
		sort.SliceStable(filtered, func(i, j int) bool {
			a, b := filtered[i][key], filtered[j][key]
			if a == nil || b == nil {
				return a != nil && b == nil
			}
			less, equal := fmt.Sprint(a) < fmt.Sprint(b), fmt.Sprint(a) == fmt.Sprint(b)
			if fa, ok := a.(float64); ok {
				if fb, ok := b.(float64); ok {
					less, equal = fa < fb, fa == fb
				}
			}
			if desc {
				return !less && !equal
			}
			return less
		})
	}

	total := len(filtered)

	start := 0
	if c := q.Get("cursor"); c != "" {
		start, _ = strconv.Atoi(c)
	}
	limit := pageCap
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l < pageCap {
		limit = l
	}
	end := start + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]

	// Projection: the primary key always comes back, as documented, and so do
	// the expanded columns and the column each expansion resolves through —
	// none of which fields= names.
	if fs := q.Get("fields"); fs != "" {
		want := map[string]bool{pk: true}
		for _, c := range strings.Split(fs, ",") {
			if _, exists := stored[c]; !exists {
				writeErr(w, 400, "unknown field: "+c)
				return
			}
			want[c] = true
		}
		for _, x := range expansions {
			// The service returns the referencing column beside the expanded
			// ones whether or not it was projected, which is what lets a
			// dangling reference be told from a null one.
			want[x.col] = true
			want[x.key] = true
			for _, c := range x.cols[1:] {
				want[x.key+"_"+c] = true
			}
		}
		projected := make([]map[string]interface{}, 0, len(page))
		for _, row := range page {
			cut := map[string]interface{}{}
			for k, v := range row {
				if want[k] {
					cut[k] = v
				}
			}
			projected = append(projected, cut)
		}
		page = projected
	}

	// Every list envelope carries the entity's instant, as the service's does.
	resp := map[string]interface{}{"count": total, "results": page, "data_as_of": nil}
	if se != nil && se.DataAsOf != nil {
		resp["data_as_of"] = *se.DataAsOf
	}
	if asOf != nil {
		resp["data_as_of"] = *asOf
	}
	if end < len(filtered) {
		resp["next_cursor"] = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func toFloat(v interface{}) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

func matches(v interface{}, op, want string) bool {
	s := fmt.Sprintf("%v", v)
	if f, ok := v.(float64); ok {
		s = strconv.FormatFloat(f, 'f', -1, 64)
	}
	// A number column compares by value, as the service's typed columns do:
	// 4 equals "4.0".
	same := func(w string) bool {
		if f, ok := v.(float64); ok {
			n, err := strconv.ParseFloat(w, 64)
			return err == nil && n == f
		}
		if i, ok := v.(int); ok {
			n, err := strconv.ParseFloat(w, 64)
			return err == nil && n == float64(i)
		}
		return s == w
	}
	switch op {
	case "eq":
		return v != nil && same(want)
	case "in":
		if v == nil {
			return false
		}
		for _, w := range strings.Split(want, ",") {
			if same(w) {
				return true
			}
		}
		return false
	case "isnull":
		return (v == nil) == (want == "true")
	case "gt", "lt":
		// A number compares as a number and text as text, as the service's
		// typed columns do; NULL compares as nothing.
		var order int
		switch n := v.(type) {
		case nil:
			return false
		case int, int64, float64:
			w, err := strconv.ParseFloat(want, 64)
			if err != nil {
				return false
			}
			order = cmp.Compare(toFloat(n), w)
		default:
			order = strings.Compare(s, want)
		}
		return (op == "gt" && order > 0) || (op == "lt" && order < 0)
	case "ilike":
		// The service's ilike is a case-insensitive contains on the literal
		// value; a % in the value is that character.
		return strings.Contains(strings.ToLower(s), strings.ToLower(want))
	// DATA-408: literal and case-insensitive like ilike, anchored.
	case "istartswith":
		return strings.HasPrefix(strings.ToLower(s), strings.ToLower(want))
	case "iendswith":
		return strings.HasSuffix(strings.ToLower(s), strings.ToLower(want))
	case "iexact":
		return strings.EqualFold(s, want)
	case "host":
		// The stored inet text's address, compared as an address with any
		// listed one; the mask plays no part.
		if v == nil {
			return false
		}
		a, err := netip.ParseAddr(strings.SplitN(s, "/", 2)[0])
		if err != nil {
			return false
		}
		for _, w := range strings.Split(want, ",") {
			if b, err := netip.ParseAddr(w); err == nil && a == b {
				return true
			}
		}
		return false
	}
	return true
}

// addAddressEntity adds ipam/ip-addresses as a replica with the host operator
// (DATA-417) serves it: address is VARCHAR and lists host beside the text
// operators. withHost false is a build before host.
func (f *fakeService) addAddressEntity(withHost bool) {
	f.addEntity("ipam/ip-addresses", "id:BIGINT:pk", "address:VARCHAR")
	if !withHost {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.schema.Entities["/v1/ipam/ip-addresses"]
	for i := range e.Columns {
		if e.Columns[i].Name == "address" {
			e.Columns[i].Operators = append(e.Columns[i].Operators, "host")
		}
	}
	f.schema.Entities["/v1/ipam/ip-addresses"] = e
}

// isTextOperator is the service's type gate: these four bind VARCHAR alone.
func isTextOperator(op string) bool {
	return op == "ilike" || slices.Contains(anchoredTextOperators, op)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// requestWith returns the recorded request for an entity that carries a given
// query parameter. Several requests can hit one entity in a single Query — the
// row fetch plus the custom-field names read — so tests that assert pushdown
// must name the one they mean rather than taking the last.
func (f *fakeService) requestWith(entity, key string) (recordedRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.entity == entity && r.query.Get(key) != "" {
			return r, true
		}
	}
	return recordedRequest{}, false
}

func (f *fakeService) countRequestsFor(entity string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.entity == entity {
			n++
		}
	}
	return n
}

// fakeSchema is the wire shape of GET /v1/_meta/schema, as the fake serves it.
type fakeSchema struct {
	SnapshotComplete bool                  `json:"snapshot_complete"`
	NetBoxURL        string                `json:"netbox_url,omitempty"`
	Entities         map[string]fakeEntity `json:"entities"`
}

type fakeEntity struct {
	Table      string       `json:"table"`
	PrimaryKey string       `json:"primary_key"`
	Ingested   bool         `json:"ingested"`
	DataAsOf   *string      `json:"data_as_of"`
	Columns    []fakeColumn `json:"columns"`
}

type fakeColumn struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Nullable   bool           `json:"nullable"`
	Operators  []string       `json:"operators"`
	References *fakeReference `json:"references,omitempty"`
}

type fakeReference struct {
	Path      string   `json:"path"`
	ExpandKey string   `json:"expand_key"`
	Columns   []string `json:"columns"`
	Available bool     `json:"available"`
}

// devicesSchema is a live replica's dcim/devices catalogue cut down to what the
// tests exercise: text, number, boolean and VARCHAR-timestamp columns
// (DATA-250), custom_field_data, the columns deviceFixture carries, and
// references — available ones with and without a slug, and one whose target
// has received no data.
func devicesSchema() *fakeSchema {
	ops := func(text bool) []string {
		o := []string{"eq", "gt", "lt", "in", "isnull"}
		if text {
			o = append(o, "ilike")
		}
		return o
	}
	asOf := strp("2026-09-22T14:03:11Z")
	nameSlug := func(table string) fakeEntity {
		return fakeEntity{Table: table, PrimaryKey: "id", Ingested: true, DataAsOf: asOf, Columns: []fakeColumn{
			{Name: "id", Type: "BIGINT", Operators: ops(false)},
			{Name: "name", Type: "VARCHAR", Operators: ops(true)},
			{Name: "slug", Type: "VARCHAR", Operators: ops(true)},
		}}
	}
	return &fakeSchema{
		SnapshotComplete: true,
		Entities: map[string]fakeEntity{
			"/v1/dcim/devices": {Table: "dcim_device", PrimaryKey: "id", Ingested: true, DataAsOf: asOf, Columns: []fakeColumn{
				{Name: "id", Type: "BIGINT", Operators: ops(false)},
				{Name: "name", Type: "VARCHAR", Nullable: true, Operators: ops(true)},
				{Name: "serial", Type: "VARCHAR", Nullable: false, Operators: ops(true)},
				{Name: "position", Type: "DOUBLE", Nullable: true, Operators: ops(false)},
				{Name: "is_full_depth", Type: "BOOLEAN", Operators: ops(false)},
				{Name: "created", Type: "VARCHAR", Nullable: true, Operators: ops(true)},
				{Name: "custom_field_data", Type: "VARCHAR", Nullable: true, Operators: ops(true)},
				{Name: "status", Type: "VARCHAR", Nullable: true, Operators: ops(true)},
				{Name: "role_id", Type: "BIGINT", Nullable: true, Operators: ops(false),
					References: &fakeReference{Path: "/v1/dcim/device-roles", ExpandKey: "role", Columns: []string{"name", "slug"}, Available: true}},
				{Name: "tenant_id", Type: "BIGINT", Nullable: true, Operators: ops(false),
					References: &fakeReference{Path: "/v1/tenancy/tenants", ExpandKey: "tenant", Columns: []string{"name", "slug"}, Available: true}},
				{Name: "site_id", Type: "BIGINT", Nullable: true, Operators: ops(false),
					References: &fakeReference{Path: "/v1/dcim/sites", ExpandKey: "site", Columns: []string{"name", "slug"}, Available: true}},
				{Name: "rack_id", Type: "BIGINT", Nullable: true, Operators: ops(false),
					References: &fakeReference{Path: "/v1/dcim/racks", ExpandKey: "rack", Columns: []string{"name"}, Available: true}},
				{Name: "platform_id", Type: "BIGINT", Nullable: true, Operators: ops(false),
					References: &fakeReference{Path: "/v1/dcim/platforms", ExpandKey: "platform", Columns: []string{"name", "slug"}, Available: false}},
			}},
			"/v1/dcim/sites":        nameSlug("dcim_site"),
			"/v1/dcim/device-roles": nameSlug("dcim_devicerole"),
			"/v1/tenancy/tenants":   nameSlug("tenancy_tenant"),
			"/v1/dcim/racks": {Table: "dcim_rack", PrimaryKey: "id", Ingested: true, DataAsOf: asOf, Columns: []fakeColumn{
				{Name: "id", Type: "BIGINT", Operators: ops(false)},
				{Name: "name", Type: "VARCHAR", Operators: ops(true)},
			}},
			"/v1/dcim/platforms": {Table: "dcim_platform", PrimaryKey: "id", Ingested: false},
			"/v1/core/object-types": {Table: "core_objecttype", PrimaryKey: "contenttype_ptr_id", Ingested: true, DataAsOf: asOf, Columns: []fakeColumn{
				{Name: "contenttype_ptr_id", Type: "INTEGER", Operators: ops(false)},
				{Name: "public", Type: "BOOLEAN", Operators: ops(false)},
			}},
		},
	}
}

func strp(s string) *string { return &s }

// anchoredTextOperators are DATA-408's three operators: case-insensitive
// starts-with, ends-with and exact match, literal like ilike, listed by the
// catalogue on text columns only.
var anchoredTextOperators = []string{"istartswith", "iendswith", "iexact"}

// withAnchoredText is a replica-cache build that ships DATA-408: every VARCHAR
// column of every entity lists the three anchored matches beside ilike.
func withAnchoredText(s *fakeSchema) *fakeSchema {
	for path, e := range s.Entities {
		for i, c := range e.Columns {
			if c.Type == "VARCHAR" {
				e.Columns[i].Operators = append(slices.Clone(c.Operators), anchoredTextOperators...)
			}
		}
		s.Entities[path] = e
	}
	return s
}

// addEntity registers an ingested entity with plain columns, for tests that
// use an object type the devices fixture does not carry. cols is "name:TYPE";
// the primary key is "id" unless a col is "<name>:TYPE:pk".
func (f *fakeService) addEntity(objectType string, cols ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := fakeEntity{Table: strings.ReplaceAll(objectType, "/", "_"), PrimaryKey: "id", Ingested: true, DataAsOf: strp("2026-09-22T14:03:11Z")}
	for _, c := range cols {
		parts := strings.Split(c, ":")
		col := fakeColumn{Name: parts[0], Type: parts[1], Nullable: true, Operators: []string{"eq", "gt", "lt", "in", "isnull"}}
		if parts[1] == "VARCHAR" {
			col.Operators = append(col.Operators, "ilike")
		}
		if len(parts) == 3 && parts[2] == "pk" {
			e.PrimaryKey, col.Nullable = parts[0], false
		}
		e.Columns = append(e.Columns, col)
	}
	f.schema.Entities["/v1/"+objectType] = e
}

// addReference declares col of objectType as a foreign key to target, expanded
// under key with the target's cols. Available follows the target's ingestion
// in the fake schema, as the real catalogue's does per tenant.
func (f *fakeService) addReference(objectType, col, target, key string, cols ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.schema.Entities["/v1/"+objectType]
	t, known := f.schema.Entities["/v1/"+target]
	for i := range e.Columns {
		if e.Columns[i].Name == col {
			e.Columns[i].References = &fakeReference{Path: "/v1/" + target, ExpandKey: key, Columns: cols, Available: known && t.Ingested}
		}
	}
	f.schema.Entities["/v1/"+objectType] = e
}
