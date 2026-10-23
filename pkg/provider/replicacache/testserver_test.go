package replicacache

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math/big"
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
	// failing, when it names an entity, is the status that entity's row route
	// answers instead of rows — one table failing while the rest answer, which
	// status cannot model: it fails the catalogue and every table at once.
	failing map[string]int
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
	// cursorOnLastPage hands out a next_cursor with a full last page, as a
	// keyset-paged service may; the page after it is empty.
	cursorOnLastPage bool
	// afterRequest, when set, runs once each row request has been answered
	// (and before the next can be sent) — how a test has the replica change
	// between two reads of one learn.
	afterRequest func(entity string, q url.Values)
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
	defer func() {
		f.mu.Lock()
		hook := f.afterRequest
		f.mu.Unlock()
		if hook != nil {
			hook(entity, q)
		}
	}()
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{entity: entity, query: q})
	// The datasource's own budget, not the replica's (which answered a 36 KB
	// query): a request whose query, less the cursor a page adds, passes
	// 16 KB is one this datasource must never send, so the fake refuses it
	// rather than letting a test pass on a request production would not get.
	if base := withoutCursor(q); len(base.Encode()) > queryBudget {
		f.mu.Unlock()
		writeErr(w, 400, fmt.Sprintf("query of %d bytes exceeds this test's 16 KB budget", len(base.Encode())))
		return
	}
	rows, ok := f.entities[entity]
	status, errBody, pageCap, cursorOnLastPage := f.status, f.errBody, f.pageCap, f.cursorOnLastPage
	if s := f.failing[entity]; s != 0 {
		status, errBody = s, "service unavailable"
	}
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
	// The service reads a request's operators before it looks at the table:
	// measured on v1.40.1 against a table that never received data,
	// filter[id]__eq is its 404 and filter[id]__bogus its 400, "invalid filter
	// operator: bogus". An operator the handler has no implementation of is that
	// 400 whatever the catalogue lists on the column (a schema-less fake lists
	// none), never a filter that matches every row: matches would answer true for
	// it, and a test sending one the service refuses would pass for the wrong
	// reason. An operator the BUILD lacks is the same 400 (see buildKnows).
	//
	// So is a filter key given twice, whatever its values (measured on v1.40.1,
	// ipam/ip-addresses: filter[dns_name]__gt= beside filter[dns_name]__gt=x in
	// either order, and __eq and __in twice, are each 400 "duplicate filter:
	// filter[dns_name]__gt", on a table that never received data as on one that
	// has: it comes before the table's own 404). The fake would otherwise read the
	// first value and never notice the second, and a request the replica refuses
	// would pass. One column under two operators, or one operator on two columns,
	// is two keys.
	for k, vs := range q {
		if strings.HasPrefix(k, "filter[") {
			if len(vs) > 1 {
				writeErr(w, 400, "duplicate filter: "+k)
				return
			}
			if op := k[strings.Index(k, "]")+3:]; !f.buildKnows(op) {
				writeErr(w, 400, "invalid filter operator: "+op)
				return
			}
		}
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
			// The four text operators refuse an empty value, where eq, in, gt and
			// lt take one on text (measured on v1.40.1, ipam/ip-addresses dns_name:
			// __ilike=, __istartswith=, __iendswith= and __iexact= are each 400
			// "invalid filter value for dns_name: a text filter takes a non-empty
			// value"; __eq= is the 23 blank rows of 25, __in= the same, __gt= the 2
			// with a value, __lt= none). matches answers the comparisons: "" is the
			// least string, and NULL compares as nothing.
			if isTextOperator(op) && vs[0] == "" {
				writeErr(w, 400, "invalid filter value for "+col+": a text filter takes a non-empty value")
				return
			}
			// The catalogue is derived from the handler's own operator specs,
			// so an operator it does not list is one the handler refuses — a
			// build before DATA-408 answers istartswith this way.
			if c.Operators != nil && !slices.Contains(c.Operators, op) {
				writeErr(w, 400, "invalid filter operator: "+op)
				return
			}
			// A whole-number column takes whole numbers: the service refuses
			// "4.0" rather than comparing it as 4 (measured on v1.40).
			if _, whole := wholeNumberBits[strings.ToUpper(c.Type)]; whole && (op == "eq" || op == "in" || op == "gt" || op == "lt") {
				for _, v := range strings.Split(vs[0], ",") {
					if _, ok := new(big.Int).SetString(v, 10); !ok {
						writeErr(w, 400, "invalid filter value for "+col)
						return
					}
				}
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
		// contains (v1.39) takes at most filter_limits.contains.max_values
		// values, and one that is no address or prefix refuses the whole filter.
		if op == "contains" {
			parts := strings.Split(vs[0], ",")
			if len(parts) > fakeContainsMaxValues {
				writeErr(w, 400, fmt.Sprintf("invalid filter value for %s: contains takes at most %d values, got %d", col, fakeContainsMaxValues, len(parts)))
				return
			}
			for _, part := range parts {
				if _, ok := containsOperand(strings.TrimSpace(part)); !ok {
					writeErr(w, 400, "invalid filter value for "+col+": contains takes IP addresses or prefixes with no zone")
					return
				}
			}
		}
		keep := []map[string]interface{}{} // the service answers [], never null, when nothing matches
		match := func(v interface{}) bool { return matches(v, op, vs[0]) }
		if op == "in" {
			match = inMatcher(vs[0])
		}
		for _, row := range filtered {
			if match(row[col]) {
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
	if end < len(filtered) || (cursorOnLastPage && end > start) {
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
		// typed columns do; NULL compares as nothing. On text the empty string is
		// the least value, so gt "" is every non-empty string (and no NULL) and
		// lt "" none — which is how the replica is asked what NetBox's empty
		// lookup means (emptiness.go).
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
		// DuckDB's ILIKE lowercases both sides (simple Unicode mapping); it
		// does not case-fold, so "s" and "ſ" differ, where strings.EqualFold
		// would match them.
		lowered := strings.ToLower(s)
		return lowered == strings.ToLower(want)
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
	case "contains":
		// The prefixes that contain any listed value: the union of the list. s is
		// the stored cidr text.
		stored, err := netip.ParsePrefix(s)
		if v == nil || err != nil {
			return false
		}
		for _, w := range strings.Split(want, ",") {
			if operand, ok := containsOperand(strings.TrimSpace(w)); ok && operand.within(stored) {
				return true
			}
		}
		return false
	}
	return true
}

// inMatcher is matches(v, "in", want) with the list split once per request
// rather than once per row: a batched read sends a thousand values against
// thousands of rows, and splitting the list for every row made such a test
// quadratic. Same semantics: a number compares by value, text as written, and
// NULL is in no list.
func inMatcher(want string) func(interface{}) bool {
	texts, numbers := map[string]bool{}, map[float64]bool{}
	for _, w := range strings.Split(want, ",") {
		texts[w] = true
		if n, err := strconv.ParseFloat(w, 64); err == nil {
			numbers[n] = true
		}
	}
	return func(v interface{}) bool {
		switch n := v.(type) {
		case nil:
			return false
		case float64:
			return numbers[n]
		case int:
			return numbers[float64(n)]
		}
		return texts[fmt.Sprintf("%v", v)]
	}
}

// withoutCursor is a request's query less its cursor, which is what the
// budget is measured on in the fake.
func withoutCursor(q url.Values) url.Values {
	out := url.Values{}
	for k, vs := range q {
		if k != "cursor" {
			out[k] = vs
		}
	}
	return out
}

// fakeContainsMaxValues is filter_limits.contains.max_values on v1.40.1.
const fakeContainsMaxValues = 1000

// implementedOperators are the filter operators the fake answers, as the
// catalogue of v1.40.1 lists them across its columns.
var implementedOperators = []string{"eq", "gt", "lt", "in", "isnull", "ilike", "istartswith", "iendswith", "iexact", "host", "contains"}

// buildKnows reports whether the replica-cache build the fake stands for has an
// operator: one the fake answers, and contains only where the schema route
// states its limit, as v1.39 and later do and a build before it does not.
func (f *fakeService) buildKnows(op string) bool {
	if !slices.Contains(implementedOperators, op) {
		return false
	}
	if op != "contains" {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.schema == nil {
		return false
	}
	_, states := f.schema.FilterLimits["contains"]
	return states
}

// containsTerm is one element of a contains list, as the replica reads it
// (internal/api/filter.go containsCandidates): a bare address is contained
// strictly, a value with a mask is a prefix, contained or equal with its host
// bits ignored. The families never contain each other, and an IPv4-mapped
// value is IPv6.
type containsTerm struct {
	addr   netip.Addr
	bits   int
	strict bool
}

// containsOperand reads a value as the replica does: Go's address and prefix
// parsers, which take no netmask notation, no zone, and no length written other
// than in plain digits. False is the 400 the replica answers for the whole list.
func containsOperand(s string) (containsTerm, bool) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return containsTerm{addr: p.Masked().Addr(), bits: p.Bits()}, err == nil
	}
	a, err := netip.ParseAddr(s)
	return containsTerm{addr: a, bits: a.BitLen(), strict: true}, err == nil && a.Zone() == ""
}

// within reports whether the stored prefix contains the value.
func (c containsTerm) within(stored netip.Prefix) bool {
	if c.strict && stored.Bits() >= c.bits {
		return false
	}
	return stored.Bits() <= c.bits && stored.Contains(c.addr)
}

// addPrefixes serves ipam/prefixes as replica-cache v1.40.1 does: the prefix
// is cidr text with its mask always written, the column lists contains, and the
// schema route states the 1,000-value limit. A test that wants a build without
// the operator clears schema.FilterLimits, or the column's operator.
func (f *fakeService) addPrefixes(rows ...map[string]interface{}) {
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR", "status:VARCHAR", "vrf_id:BIGINT")
	f.addOperators("ipam/prefixes", "prefix", "contains")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schema.FilterLimits = map[string]fakeFilterLimit{"contains": {MaxValues: fakeContainsMaxValues}}
	f.entities["ipam/prefixes"] = rows
}

// addAddressEntity adds ipam/ip-addresses as a replica with the host operator
// (DATA-417) serves it: address is VARCHAR and lists host beside the text
// operators. withHost false is a build before host.
func (f *fakeService) addAddressEntity(withHost bool) {
	f.addEntity("ipam/ip-addresses", "id:BIGINT:pk", "address:VARCHAR")
	if withHost {
		f.addOperators("ipam/ip-addresses", "address", "host")
	}
}

// addOperators lists more operators on one column of an entity's catalogue
// entry, as a replica-cache build that has them does (host, contains).
func (f *fakeService) addOperators(objectType, col string, ops ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.schema.Entities["/v1/"+objectType]
	for i := range e.Columns {
		if e.Columns[i].Name == col {
			e.Columns[i].Operators = append(slices.Clone(e.Columns[i].Operators), ops...)
		}
	}
	f.schema.Entities["/v1/"+objectType] = e
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

// sentWith is every recorded request to an entity that carried a parameter,
// in the order sent.
func (f *fakeService) sentWith(entity, key string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.entity == entity && r.query.Has(key) {
			out = append(out, r)
		}
	}
	return out
}

// forget clears the recorded requests, so the next assertions see only what
// follows.
func (f *fakeService) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

// requestCount is how many requests the fake has recorded.
func (f *fakeService) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
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
	// FilterLimits is the per-request limit of each operator that has one
	// (v1.39 and later). Nil omits the block, as a build before containment does.
	FilterLimits map[string]fakeFilterLimit `json:"filter_limits,omitempty"`
}

type fakeFilterLimit struct {
	MaxValues int `json:"max_values"`
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

// netboxTables is the table replica-cache v1.40.1 reads for each NetBox object
// type it serves, as its catalogue reports them (GET /v1/_meta/schema on a
// replica of NetBox 4.4.10). The table is what ties an entity to NetBox's
// filter vocabulary (vocabFor), so the fake has to carry the real one: a name
// derived from the path (ipam_ip-addresses) is covered by nothing, and every
// filter on it would quietly keep the catalogue-only rules.
var netboxTables = map[string]string{
	"circuits/circuit-terminations":   "circuits_circuittermination",
	"circuits/circuit-types":          "circuits_circuittype",
	"circuits/circuits":               "circuits_circuit",
	"circuits/provider-accounts":      "circuits_provideraccount",
	"circuits/provider-networks":      "circuits_providernetwork",
	"circuits/providers":              "circuits_provider",
	"core/object-types":               "core_objecttype",
	"dcim/cable-paths":                "dcim_cablepath",
	"dcim/cable-terminations":         "dcim_cabletermination",
	"dcim/cables":                     "dcim_cable",
	"dcim/console-ports":              "dcim_consoleport",
	"dcim/console-server-ports":       "dcim_consoleserverport",
	"dcim/device-bays":                "dcim_devicebay",
	"dcim/device-roles":               "dcim_devicerole",
	"dcim/device-types":               "dcim_devicetype",
	"dcim/devices":                    "dcim_device",
	"dcim/front-ports":                "dcim_frontport",
	"dcim/interfaces":                 "dcim_interface",
	"dcim/inventory-item-roles":       "dcim_inventoryitemrole",
	"dcim/inventory-items":            "dcim_inventoryitem",
	"dcim/locations":                  "dcim_location",
	"dcim/mac-addresses":              "dcim_macaddress",
	"dcim/manufacturers":              "dcim_manufacturer",
	"dcim/module-bays":                "dcim_modulebay",
	"dcim/module-types":               "dcim_moduletype",
	"dcim/modules":                    "dcim_module",
	"dcim/platforms":                  "dcim_platform",
	"dcim/power-feeds":                "dcim_powerfeed",
	"dcim/power-outlets":              "dcim_poweroutlet",
	"dcim/power-panels":               "dcim_powerpanel",
	"dcim/power-ports":                "dcim_powerport",
	"dcim/rack-roles":                 "dcim_rackrole",
	"dcim/rack-types":                 "dcim_racktype",
	"dcim/racks":                      "dcim_rack",
	"dcim/rear-ports":                 "dcim_rearport",
	"dcim/regions":                    "dcim_region",
	"dcim/site-groups":                "dcim_sitegroup",
	"dcim/sites":                      "dcim_site",
	"dcim/virtual-chassis":            "dcim_virtualchassis",
	"dcim/virtual-device-contexts":    "dcim_virtualdevicecontext",
	"ipam/aggregates":                 "ipam_aggregate",
	"ipam/asn-ranges":                 "ipam_asnrange",
	"ipam/asns":                       "ipam_asn",
	"ipam/fhrp-group-assignments":     "ipam_fhrpgroupassignment",
	"ipam/fhrp-groups":                "ipam_fhrpgroup",
	"ipam/ip-addresses":               "ipam_ipaddress",
	"ipam/ip-ranges":                  "ipam_iprange",
	"ipam/prefixes":                   "ipam_prefix",
	"ipam/rirs":                       "ipam_rir",
	"ipam/roles":                      "ipam_role",
	"ipam/service-templates":          "ipam_servicetemplate",
	"ipam/services":                   "ipam_service",
	"ipam/vlan-groups":                "ipam_vlangroup",
	"ipam/vlans":                      "ipam_vlan",
	"ipam/vrfs":                       "ipam_vrf",
	"tenancy/tenant-groups":           "tenancy_tenantgroup",
	"tenancy/tenants":                 "tenancy_tenant",
	"virtualization/cluster-groups":   "virtualization_clustergroup",
	"virtualization/cluster-types":    "virtualization_clustertype",
	"virtualization/clusters":         "virtualization_cluster",
	"virtualization/interfaces":       "virtualization_vminterface",
	"virtualization/virtual-disks":    "virtualization_virtualdisk",
	"virtualization/virtual-machines": "virtualization_virtualmachine",
	"vpn/ike-policies":                "vpn_ikepolicy",
	"vpn/ike-proposals":               "vpn_ikeproposal",
	"vpn/ipsec-policies":              "vpn_ipsecpolicy",
	"vpn/ipsec-profiles":              "vpn_ipsecprofile",
	"vpn/ipsec-proposals":             "vpn_ipsecproposal",
	"vpn/l2vpn-terminations":          "vpn_l2vpntermination",
	"vpn/l2vpns":                      "vpn_l2vpn",
	"vpn/tunnel-groups":               "vpn_tunnelgroup",
	"vpn/tunnel-terminations":         "vpn_tunneltermination",
	"vpn/tunnels":                     "vpn_tunnel",
	"wireless/wireless-lan-groups":    "wireless_wirelesslangroup",
	"wireless/wireless-lans":          "wireless_wirelesslan",
	"wireless/wireless-links":         "wireless_wirelesslink",
}

// fakeTable is the table the fake's catalogue gives an object type: NetBox's
// own for a NetBox type, and one derived from the path for a plugin's, which
// the vocabulary does not cover. A NetBox type missing from netboxTables is a
// test that would bypass the vocabulary without saying so, so it stops here.
func fakeTable(objectType string) string {
	if table, ok := netboxTables[objectType]; ok {
		return table
	}
	if !strings.HasPrefix(objectType, "plugins/") {
		panic("fakeTable: add NetBox's table for " + objectType + " to netboxTables")
	}
	return strings.ReplaceAll(objectType, "/", "_")
}

// addEntity registers an ingested entity with plain columns, for tests that
// use an object type the devices fixture does not carry. cols is "name:TYPE";
// the primary key is "id" unless a col is "<name>:TYPE:pk".
func (f *fakeService) addEntity(objectType string, cols ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := fakeEntity{Table: fakeTable(objectType), PrimaryKey: "id", Ingested: true, DataAsOf: strp("2026-09-22T14:03:11Z")}
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

// The tables a related filter is resolved through, as replica-cache v1.40.1
// serves them from a NetBox 4.4.10 database (GET /v1/_meta/schema): a nested
// model carries django-mptt's columns (tree_id, lft, rght, level, INTEGER)
// beside its parent_id; a VRF stores its rd, though the catalogue's vrf
// reference joins in the name alone; a component stores the device's site as
// _site_id, with no reference declared.

// addColumns appends columns to an entity of the fake's catalogue, each
// nullable with the operators v1.40.1 lists for its type.
func (f *fakeService) addColumns(objectType string, cols ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.schema.Entities["/v1/"+objectType]
	e.Columns = slices.Clone(e.Columns)
	for _, c := range cols {
		name, typ, _ := strings.Cut(c, ":")
		col := fakeColumn{Name: name, Type: typ, Nullable: true, Operators: []string{"eq", "gt", "lt", "in", "isnull"}}
		if typ == "VARCHAR" {
			col.Operators = append(col.Operators, "ilike")
		}
		e.Columns = append(e.Columns, col)
	}
	f.schema.Entities["/v1/"+objectType] = e
}

// treeNode is a row of a nested model: its parent and its place in the tree.
func treeNode(id int, name, slug string, parent interface{}, tree, lft, rght, level int) map[string]interface{} {
	return map[string]interface{}{"id": id, "name": name, "slug": slug, "parent_id": parent,
		"tree_id": tree, "lft": lft, "rght": rght, "level": level}
}

// addTreeRoles serves device roles as NetBox 4.3 and later store them, nested:
// core > core-leaf > core-leaf-edge in one tree, access alone in another.
func (f *fakeService) addTreeRoles() {
	f.addColumns("dcim/device-roles", "parent_id:BIGINT", "tree_id:INTEGER", "lft:INTEGER", "rght:INTEGER", "level:INTEGER")
	f.addReference("dcim/device-roles", "parent_id", "dcim/device-roles", "parent", "name", "slug")
	f.entities["dcim/device-roles"] = []map[string]interface{}{
		treeNode(5, "Core", "core", nil, 1, 1, 6, 0),
		treeNode(8, "Core Leaf", "core-leaf", 5, 1, 2, 5, 1),
		treeNode(9, "Core Leaf Edge", "core-leaf-edge", 8, 1, 3, 4, 2),
		treeNode(6, "Access", "access", nil, 2, 1, 2, 0),
	}
}

// addLocations serves dcim/locations, nested: dc > room, and lab alone. A
// prefix scoped to one caches it as _location_id, on which NetBox's location
// filter is a tree filter.
func (f *fakeService) addLocations() {
	f.addEntity("dcim/locations", "id:BIGINT:pk", "name:VARCHAR", "slug:VARCHAR", "parent_id:BIGINT",
		"tree_id:INTEGER", "lft:INTEGER", "rght:INTEGER", "level:INTEGER")
	f.entities["dcim/locations"] = []map[string]interface{}{
		treeNode(1, "DC", "dc", nil, 1, 1, 4, 0),
		treeNode(2, "Room", "room", 1, 1, 2, 3, 1),
		treeNode(3, "Lab", "lab", nil, 2, 1, 2, 0),
	}
}

// addVRFs serves ipam/vrfs with its rd and gives objectType a vrf_id that
// references it as the catalogue does, by name alone: NetBox's vrf filter
// compares the RD, which is never an expanded column.
func (f *fakeService) addVRFs(objectType string, ingested bool) {
	f.addEntity("ipam/vrfs", "id:BIGINT:pk", "name:VARCHAR", "rd:VARCHAR")
	f.setIngested("ipam/vrfs", ingested)
	f.entities["ipam/vrfs"] = []map[string]interface{}{
		{"id": 1, "name": "Blue", "rd": "65000:1"},
		{"id": 2, "name": "Red", "rd": nil},
	}
	f.addColumns(objectType, "vrf_id:BIGINT")
	f.addReference(objectType, "vrf_id", "ipam/vrfs", "vrf", "name")
}

// addInterfaces serves dcim/interfaces with the device it belongs to (by
// name, the catalogue's reference), the device's site cached as _site_id with
// no reference, and a primary MAC address.
func (f *fakeService) addInterfaces() {
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "device_id:BIGINT", "_site_id:BIGINT", "primary_mac_address_id:BIGINT")
	f.addReference("dcim/interfaces", "device_id", "dcim/devices", "device", "name")
	f.addEntity("dcim/mac-addresses", "id:BIGINT:pk", "mac_address:VARCHAR")
	f.addReference("dcim/interfaces", "primary_mac_address_id", "dcim/mac-addresses", "primary_mac_address", "mac_address")
}

// addCableTerminations serves dcim/cable-terminations and dcim/cables, where
// NetBox's cable filter is a single-valued ModelChoiceFilter whose form takes
// no null.
func (f *fakeService) addCableTerminations() {
	f.addEntity("dcim/cables", "id:BIGINT:pk", "label:VARCHAR")
	f.addEntity("dcim/cable-terminations", "id:BIGINT:pk", "cable_id:BIGINT", "cable_end:VARCHAR")
	f.addReference("dcim/cable-terminations", "cable_id", "dcim/cables", "cable", "label")
	f.entities["dcim/cables"] = []map[string]interface{}{{"id": 7, "label": "C7"}, {"id": 9, "label": "C9"}}
	f.entities["dcim/cable-terminations"] = []map[string]interface{}{
		{"id": 1, "cable_id": 7, "cable_end": "A"},
		{"id": 2, "cable_id": 9, "cable_end": "A"},
		{"id": 3, "cable_id": 9, "cable_end": "B"},
	}
}

// setIngested marks an entity of the fake's catalogue fed or unfed; an unfed
// one lists no columns, as the service's does, and its row route answers 404
// "no data received for this entity".
func (f *fakeService) setIngested(objectType string, ingested bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.schema.Entities["/v1/"+objectType]
	e.Ingested = ingested
	if !ingested {
		e.Columns, e.DataAsOf = nil, nil
	}
	f.schema.Entities["/v1/"+objectType] = e
}

// addTopologyTables serves the tables a topology is read from, with the
// columns and types replica-cache v1.40.1 serves them with (GET
// /v1/_meta/schema on a replica of NetBox 4.4.10), cut down to what the
// topology reads: a cable termination caches its device (_device_id, null on
// a circuit termination) and names its object only by content type and id;
// interfaces and panel ports cache their cable and its end; an interface
// names its cable path; the path is jsonb text; a wireless link caches both
// ends' devices.
func (f *fakeService) addTopologyTables() {
	f.addEntity("dcim/cable-terminations", "id:BIGINT:pk", "cable_end:VARCHAR", "termination_id:BIGINT",
		"cable_id:BIGINT", "termination_type_id:INTEGER", "_device_id:BIGINT")
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "_name:VARCHAR", "_path_id:BIGINT",
		"cable_id:BIGINT", "device_id:BIGINT", "cable_end:VARCHAR")
	for _, ports := range []string{"dcim/front-ports", "dcim/rear-ports"} {
		f.addEntity(ports, "id:BIGINT:pk", "name:VARCHAR", "cable_id:BIGINT", "device_id:BIGINT", "cable_end:VARCHAR")
	}
	f.addEntity("dcim/cable-paths", "id:BIGINT:pk", "is_active:BOOLEAN", "path:VARCHAR", "is_complete:BOOLEAN")
	f.addEntity("wireless/wireless-links", "id:BIGINT:pk", "_interface_a_device_id:BIGINT", "_interface_b_device_id:BIGINT",
		"interface_a_id:BIGINT", "interface_b_id:BIGINT")
}

// addAssignmentTables serves the tables an address's owner type is learned
// from, fed and empty, with the columns replica-cache v1.40.1 serves them
// with (GET /v1/_meta/schema on a replica of NetBox 4.4.10), cut down to what
// the learning reads: an address names its assignment by content type id and
// object id, a NAT'd address its inside address; a device and a virtual
// machine each cache their primary addresses; an interface and a VM interface
// name their owner. The topology's tables come with it, being where a cable
// path and a termination's type are read.
func (f *fakeService) addAssignmentTables() {
	f.addTopologyTables()
	f.addColumns("dcim/devices", "primary_ip4_id:BIGINT", "primary_ip6_id:BIGINT")
	f.addEntity("virtualization/virtual-machines", "id:BIGINT:pk", "primary_ip4_id:BIGINT", "primary_ip6_id:BIGINT")
	f.addEntity("virtualization/interfaces", "id:BIGINT:pk", "virtual_machine_id:BIGINT")
	f.addEntity("ipam/ip-addresses", "id:BIGINT:pk", "address:VARCHAR", "assigned_object_type_id:INTEGER",
		"assigned_object_id:BIGINT", "nat_inside_id:BIGINT")
	f.addEntity("ipam/fhrp-groups", "id:BIGINT:pk")
	for _, table := range []string{"dcim/devices", "dcim/interfaces", "dcim/front-ports", "dcim/rear-ports", "dcim/cable-terminations",
		"dcim/cable-paths", "virtualization/virtual-machines", "virtualization/interfaces", "ipam/ip-addresses", "ipam/fhrp-groups"} {
		f.entities[table] = []map[string]interface{}{}
	}
}

// addRows appends rows to an entity of the fake.
func (f *fakeService) addRows(objectType string, rows ...map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entities[objectType] = append(f.entities[objectType], rows...)
}

// addIPEnrichmentTables serves the tables an IP's context is read from, with
// the columns replica-cache v1.40.1 serves them with (GET /v1/_meta/schema on a
// replica of NetBox 4.4.10), cut down to what is read: an address's own
// columns, its tenant by name and its NAT inside by address, as the catalogue
// declares them; an interface's label and description, and a VM interface's
// name and description (that table has no label); a device's type, location
// and primary addresses beside the references the devices fixture declares; a
// rack's facility id; a VRF's RD and a VLAN's VID; and a prefix with the
// scope NetBox caches on it (_location_id, _site_id, _site_group_id,
// _region_id) and the containment operator. The assignment tables come with
// it, being where an address's owner type is learned. Every table is fed and
// empty.
func (f *fakeService) addIPEnrichmentTables() {
	f.addAssignmentTables()
	f.addColumns("ipam/ip-addresses", "status:VARCHAR", "role:VARCHAR", "dns_name:VARCHAR", "description:VARCHAR",
		"tenant_id:BIGINT", "vrf_id:BIGINT")
	f.addOperators("ipam/ip-addresses", "address", "host")
	f.addColumns("dcim/interfaces", "label:VARCHAR", "description:VARCHAR")
	f.addColumns("virtualization/interfaces", "name:VARCHAR", "description:VARCHAR")
	f.addColumns("virtualization/virtual-machines", "name:VARCHAR")
	f.addColumns("dcim/devices", "device_type_id:BIGINT", "location_id:BIGINT")
	f.addColumns("dcim/racks", "facility_id:VARCHAR")
	f.addEntity("dcim/device-types", "id:BIGINT:pk", "model:VARCHAR", "slug:VARCHAR")
	for _, named := range []string{"dcim/locations", "dcim/regions", "dcim/site-groups", "ipam/roles"} {
		f.addEntity(named, "id:BIGINT:pk", "name:VARCHAR", "slug:VARCHAR")
	}
	f.addEntity("ipam/vrfs", "id:BIGINT:pk", "name:VARCHAR", "rd:VARCHAR")
	f.addEntity("ipam/vlans", "id:BIGINT:pk", "vid:INTEGER", "name:VARCHAR")
	f.addPrefixes()
	f.addColumns("ipam/prefixes", "description:VARCHAR", "tenant_id:BIGINT", "role_id:BIGINT", "vlan_id:BIGINT",
		"scope_id:BIGINT", "scope_type_id:INTEGER", "_location_id:BIGINT", "_region_id:BIGINT", "_site_id:BIGINT", "_site_group_id:BIGINT")
	for _, table := range []string{"tenancy/tenants", "dcim/sites", "dcim/device-roles", "dcim/racks", "dcim/device-types",
		"dcim/locations", "dcim/regions", "dcim/site-groups", "ipam/roles", "ipam/vrfs", "ipam/vlans", "ipam/prefixes"} {
		f.entities[table] = []map[string]interface{}{}
	}
	f.addReference("ipam/ip-addresses", "tenant_id", "tenancy/tenants", "tenant", "name", "slug")
	f.addReference("ipam/ip-addresses", "vrf_id", "ipam/vrfs", "vrf", "name")
	f.addReference("ipam/ip-addresses", "nat_inside_id", "ipam/ip-addresses", "nat_inside", "address")
	f.addReference("dcim/devices", "device_type_id", "dcim/device-types", "device_type", "model", "slug")
	f.addReference("dcim/devices", "location_id", "dcim/locations", "location", "name", "slug")
	f.addReference("dcim/interfaces", "device_id", "dcim/devices", "device", "name")
	f.addReference("virtualization/interfaces", "virtual_machine_id", "virtualization/virtual-machines", "virtual_machine", "name")
	f.addReference("ipam/prefixes", "tenant_id", "tenancy/tenants", "tenant", "name", "slug")
	f.addReference("ipam/prefixes", "role_id", "ipam/roles", "role", "name", "slug")
	f.addReference("ipam/prefixes", "vrf_id", "ipam/vrfs", "vrf", "name")
	f.addReference("ipam/prefixes", "vlan_id", "ipam/vlans", "vlan", "name")
}

// unfeed marks entities as having received no data, as a tenant whose NetBox
// tables are empty has them: each lists no columns and answers 404, and every
// reference to it is unavailable, as the catalogue says per tenant.
func (f *fakeService) unfeed(objectTypes ...string) {
	for _, objectType := range objectTypes {
		f.setIngested(objectType, false)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for path, e := range f.schema.Entities {
		for i, c := range e.Columns {
			if c.References == nil {
				continue
			}
			if t, ok := f.schema.Entities[c.References.Path]; ok && !t.Ingested {
				ref := *c.References
				ref.Available = false
				e.Columns[i].References = &ref
			}
		}
		f.schema.Entities[path] = e
	}
}
