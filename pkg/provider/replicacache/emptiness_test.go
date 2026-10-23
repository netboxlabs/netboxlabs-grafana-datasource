package replicacache

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// NetBox's empty lookup on text, asked of the replica as the comparison it
// takes. The fake these tests stand on answers as v1.40.1 was measured to
// (ipam/ip-addresses dns_name, 25 rows, 23 of them blank: isnull=true finds 0,
// eq "" 23, gt "" 2, and its four text operators refuse an empty value), and
// the expectations are what NetBox 4.4.10 answers for the same filter:
// ?dns_name__empty=true is the blank and the NULL, =false the rest.

// textRows is dcim/devices rows with, in one column, what NetBox keeps in a text
// field: "", which is how a blank one is stored, NULL, and text.
func textRows(column string) []map[string]interface{} {
	row := func(id int, v interface{}) map[string]interface{} {
		d := deviceFixture(id, "n", 1)
		d[column] = v
		return d
	}
	return []map[string]interface{}{row(1, ""), row(2, nil), row(3, "x"), row(4, ""), row(5, "core-1")}
}

// emptyProvider serves textRows(column) as the devices a replica holds. The
// catalogue lists serial NOT NULL and name nullable, and nothing here may
// depend on which.
func emptyProvider(t *testing.T, column string) (*fakeService, *Provider) {
	t.Helper()
	f := newFakeService()
	f.entities["dcim/devices"] = textRows(column)
	return f, newTestProvider(t, f)
}

// pluginWidgets is a plugin's table, which NetBox's vocabulary does not cover:
// its filters are the catalogue's.
const pluginWidgets = "plugins/acme/widgets"

// widgetsProvider serves four widgets with two text columns to tell filters
// apart by:
//
//	1 name ""  code ""     2 name "a" code ""     3 name "" code "z"     4 name "b" code "y"
func widgetsProvider(t *testing.T) (*fakeService, *Provider) {
	t.Helper()
	f := newFakeService()
	f.addEntity(pluginWidgets, "id:BIGINT:pk", "name:VARCHAR", "code:VARCHAR")
	f.entities[pluginWidgets] = []map[string]interface{}{
		{"id": 1, "name": "", "code": ""}, {"id": 2, "name": "a", "code": ""},
		{"id": 3, "name": "", "code": "z"}, {"id": 4, "name": "b", "code": "y"},
	}
	return f, newTestProvider(t, f)
}

// namesBeforeTheRead is more rows than one query may read (MaxLimit) and three
// of them, the first three, have no name: reading finds them before the read
// stops.
func namesBeforeTheRead() []map[string]interface{} {
	rows := []map[string]interface{}{device(1, "", "active"), device(2, nil, "active"), device(3, "", "active")}
	for i := 4; i <= MaxLimit+4; i++ {
		rows = append(rows, device(i, fmt.Sprintf("dev-%05d", i), "active"))
	}
	return rows
}

// namesAfterTheRead is more rows than one query may read, none of them without
// a name, and then four that have none: past the point where reading stops, so
// only the counts know of them.
func namesAfterTheRead() []map[string]interface{} {
	return append(bigDevices(),
		device(MaxLimit+2, "", "active"), device(MaxLimit+3, nil, "offline"),
		device(MaxLimit+4, "", "offline"), device(MaxLimit+5, nil, "active"))
}

// gtValues is the value each request to an entity sent for filter[column]__gt.
func gtValues(f *fakeService, entity, column string) []string {
	key := "filter[" + column + "]__gt"
	var out []string
	for _, r := range f.sentWith(entity, key) {
		out = append(out, r.query.Get(key))
	}
	return out
}

// noIsNull fails the test when any request asked the replica an isnull: on text
// it answers the opposite question of NetBox's empty lookup (it finds 0 of the
// 23 blank rows), so none may be sent for it.
func noIsNull(t *testing.T, f *fakeService) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		for k := range r.query {
			if strings.HasSuffix(k, "__isnull") {
				t.Errorf("a request to %s asked %s=%s", r.entity, k, r.query.Get(k))
			}
		}
	}
}

// sentCounts is the count requests (limit=1) sent to an entity: for each, the
// filters it carried, in order — the terms of an inclusion–exclusion sum.
func sentCounts(f *fakeService, entity string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if r.entity != entity || r.query.Get("limit") != "1" {
			continue
		}
		var keys []string
		for k, vs := range r.query {
			if strings.HasPrefix(k, "filter[") {
				keys = append(keys, k+"="+vs[0])
			}
		}
		slices.Sort(keys)
		out = append(out, strings.Join(keys, "&"))
	}
	slices.Sort(out)
	return out
}

// wantRows fails the test unless a result is exactly the rows, and as many in
// total.
func wantRows(t *testing.T, res *provider.Result, want []int) {
	t.Helper()
	got := ids(res)
	if len(got) != len(want) || (len(want) > 0 && !slices.Equal(got, want)) || res.Total != len(want) {
		t.Errorf("rows %v (Total %d), want %v", got, res.Total, want)
	}
}

// "Has any value" on text is the replica's own comparison, greater than the
// empty string: every value, and neither the blank (which NetBox stores for a
// blank text field) nor the NULL. It is exact, so it is sent, not answered
// here, on a column the catalogue lists NOT NULL as on one it lists nullable.
func TestNotEmpty_TextSendsGTBlank(t *testing.T) {
	for _, column := range []string{"name", "serial"} {
		for _, filter := range []provider.Filter{
			{Field: column, Operator: "nempty"},
			{Field: column + "__empty", Value: "false"}, // NetBox's own spelling
		} {
			t.Run(column+" "+filter.Field+filter.Operator+filter.Value, func(t *testing.T) {
				f, p := emptyProvider(t, column)
				if got := relatedRows(t, p, "dcim/devices", filter); !slices.Equal(got, []int{3, 5}) {
					t.Errorf("rows %v, want [3 5]: the two with a value, not the blank or the NULL", got)
				}
				if got := gtValues(f, "dcim/devices", column); len(got) == 0 || slices.ContainsFunc(got, func(v string) bool { return v != "" }) {
					t.Errorf("filter[%s]__gt was sent as %q, want an empty value", column, got)
				}
				noIsNull(t, f)
			})
		}
	}
}

// "Is empty" on text is NetBox's empty=true: NULL or "". The replica has no
// way to say that in one filter, so it is the negation of the same comparison
// (Django's exclude() keeps NULL, which is what makes it NetBox's), and the
// total is count(base) - count(base AND gt ""): every row of the base, less
// those with a value.
func TestEmpty_TextKeepsNullAndBlank(t *testing.T) {
	for _, column := range []string{"name", "serial"} {
		for _, filter := range []provider.Filter{
			{Field: column, Operator: "empty"},
			{Field: column + "__empty", Value: "true"},
		} {
			t.Run(column+" "+filter.Field+filter.Operator+filter.Value, func(t *testing.T) {
				f, p := emptyProvider(t, column)
				res := negationQuery(t, p, provider.QuerySpec{Fields: []string{"id"}, Filters: []provider.Filter{filter}})
				wantRows(t, res, []int{1, 2, 4})
				noIsNull(t, f)
			})
		}
	}

	t.Run("the total is the base less the rows with a value", func(t *testing.T) {
		f := newFakeService()
		f.entities["dcim/devices"] = namesAfterTheRead()
		p := newTestProvider(t, f)
		res := negationQuery(t, p, provider.QuerySpec{Fields: []string{"id"}, Filters: []provider.Filter{{Field: "name", Operator: "empty"}}})
		// 10,005 rows, 10,001 of them with a value: the four that have none are
		// past the 10,000 the read stops at, so it is the counts that know of them.
		if res.Total != 4 {
			t.Errorf("Total = %d, want 4", res.Total)
		}
		if want := []string{"", "filter[name]__gt="}; !slices.Equal(sentCounts(f, "dcim/devices"), want) {
			t.Errorf("counts %q, want the base and the base with gt \"\"", sentCounts(f, "dcim/devices"))
		}
		// The partial read says so: none of the four was among the rows read.
		if len(res.Rows) != 0 || !strings.Contains(strings.Join(res.Warnings, " "), "Only the first 10,000 rows") {
			t.Errorf("%d rows, Warnings %v; want none read and a warning that the read stopped at 10,000", len(res.Rows), res.Warnings)
		}
		noIsNull(t, f)
	})

	t.Run("the rows it found are all of them", func(t *testing.T) {
		f := newFakeService()
		f.entities["dcim/devices"] = namesBeforeTheRead()
		p := newTestProvider(t, f)
		res := negationQuery(t, p, provider.QuerySpec{Fields: []string{"id"}, Filters: []provider.Filter{{Field: "name", Operator: "empty"}}})
		wantRows(t, res, []int{1, 2, 3})
		if len(res.Warnings) != 0 {
			t.Errorf("Warnings %v, want none: the counts show every match was read", res.Warnings)
		}
	})
}

// "Is empty" beside another negation is one more term of the same sum: the
// rows of the base, less those with a value, less those the other negation
// drops, plus those both drop.
func TestEmpty_WithAnotherNegation(t *testing.T) {
	t.Run("on a table read to the end", func(t *testing.T) {
		rows := []map[string]interface{}{
			device(1, "", "active"), device(2, nil, "offline"), device(3, "x", "active"),
			device(4, "", "offline"), device(5, "core-1", "planned"), device(6, nil, "active"),
		}
		for _, c := range []struct {
			name    string
			filters []provider.Filter
			want    []int
		}{
			{"not offline", []provider.Filter{{Field: "name", Operator: "empty"}, {Field: "status", Operator: "n", Value: "offline"}}, []int{1, 6}},
			{"in the other order", []provider.Filter{{Field: "status", Operator: "n", Value: "offline"}, {Field: "name", Operator: "empty"}}, []int{1, 6}},
			{"a text negation on the same column, which the empty rows never match",
				[]provider.Filter{{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nic", Value: "x"}, {Field: "status", Operator: "n", Value: "offline"}}, []int{1, 6}},
			{"a text negation that drops the rows with a value anyway",
				[]provider.Filter{{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nic", Value: "core"}}, []int{1, 2, 4, 6}},
		} {
			t.Run(c.name, func(t *testing.T) {
				f := newFakeService()
				f.entities["dcim/devices"] = rows
				p := newTestProvider(t, f)
				wantRows(t, negationQuery(t, p, provider.QuerySpec{Fields: []string{"id"}, Filters: c.filters}), c.want)
				if count := negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: c.filters}); count.Total != len(c.want) {
					t.Errorf("CountOnly Total = %d, want %d", count.Total, len(c.want))
				}
			})
		}
	})

	t.Run("counted as one term each", func(t *testing.T) {
		f := newFakeService()
		f.entities["dcim/devices"] = namesAfterTheRead()
		p := newTestProvider(t, f)
		res := negationQuery(t, p, provider.QuerySpec{Fields: []string{"id"}, Filters: []provider.Filter{
			{Field: "name", Operator: "empty"}, {Field: "status", Operator: "n", Value: "offline"}}})
		// The four rows without a name, less the two that are offline.
		if res.Total != 2 {
			t.Errorf("Total = %d, want 2", res.Total)
		}
		want := []string{
			"",
			"filter[name]__gt=",
			"filter[name]__gt=&filter[status]__eq=offline",
			"filter[status]__eq=offline",
		}
		if got := sentCounts(f, "dcim/devices"); !slices.Equal(got, want) {
			t.Errorf("counts %q,\nwant %q", got, want)
		}
		noIsNull(t, f)
	})
}

// The same "is empty" twice is one predicate, as the same text match twice is:
// counting it twice would spend the inclusion–exclusion budget (three
// negations), and carries no information. Its negation carries the one value
// the other negations are read through, the empty string.
func TestSplitNegations_EmptyIsOnePredicatePerColumn(t *testing.T) {
	positives, negs := splitNegations([]provider.Filter{
		{Field: "name", Operator: opNotGTBlank, Label: "name", LabelOperator: opEmpty},
		{Field: "status", Operator: "n", Value: "offline"},
		{Field: "name", Operator: opNotGTBlank, Label: "name", LabelOperator: opEmpty},
		{Field: "serial", Operator: opNotGTBlank},
		{Field: "name", Operator: opWireGTBlank},
	})
	if want := []provider.Filter{{Field: "name", Operator: opWireGTBlank}}; !slices.Equal(positives, want) {
		t.Errorf("positives %+v, want %+v", positives, want)
	}
	if len(negs) != 3 {
		t.Fatalf("%d negations, want name, status and serial: %+v", len(negs), negs)
	}
	for _, n := range negs {
		switch n.field {
		case "name", "serial":
			if n.op != opWireGTBlank || !slices.Equal(n.values, []string{""}) {
				t.Errorf("%s: negation of %q over %q, want the comparison with the empty string, whose value is \"\"", n.field, n.op, n.values)
			}
		}
	}
	if got := negs[0].source; got.Label != "name" || operatorLabel(got) != "empty" || negs[0].operator() != "empty" || negs[0].label() != "name" {
		t.Errorf("the negation names %q %q, want name empty as the user wrote it", negs[0].label(), negs[0].operator())
	}
}

// Where the replica's counts do not take the total — past three negations, or
// a text negation outside ASCII, whose case the replica and this code need not
// fold alike — it has to come from reading every row, which works up to 10,000:
// a caller that needs the total is refused beyond that, naming the filter as the
// user wrote it; one that does not gets the rows read and a warning that the
// read stopped.
func TestEmpty_PastTheWalkLimitIsRefused(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = bigDevices()
	p := newTestProvider(t, f)

	for _, c := range []struct {
		name    string
		filters []provider.Filter
		field   string // what the refusal names
		op      string
	}{
		{"four negations", []provider.Filter{
			{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nic", Value: "zz"},
			{Field: "serial", Operator: "nic", Value: "qq"}, {Field: "status", Operator: "n", Value: "offline"}}, "name", "empty"},
		{"in NetBox's spelling", []provider.Filter{
			{Field: "name__empty", Value: "true"}, {Field: "name", Operator: "nic", Value: "zz"},
			{Field: "serial", Operator: "nic", Value: "qq"}, {Field: "status", Operator: "n", Value: "offline"}}, "name__empty", "empty"},
		{"a text negation outside ASCII", []provider.Filter{
			{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nic", Value: "são"}}, "name", "empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: c.filters})
			var u *UnsupportedFilterError
			if !errors.As(err, &u) || u.Field != c.field || u.Operator != c.op {
				t.Fatalf("err = %v, want a refusal of %s %s, as the user wrote it", err, c.field, c.op)
			}
			for _, say := range []string{"10,000 rows", "Narrow them, or negate fewer values"} {
				if !strings.Contains(u.Reason, say) {
					t.Errorf("reason %q does not say %q", u.Reason, say)
				}
			}

			res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: c.filters, AllowUncounted: true})
			if err != nil {
				t.Fatalf("Query with AllowUncounted: %v", err)
			}
			if len(res.Rows) != 0 || !strings.Contains(strings.Join(res.Warnings, " "), "Only the first 10,000 rows") {
				t.Errorf("%d rows, Warnings %v; want none kept and a warning that the read stopped at 10,000", len(res.Rows), res.Warnings)
			}
		})
	}

	// The same negations on a table that fits are answered by reading it. The
	// negated serial is projected, so every row carries one, NULL or not.
	small := newFakeService()
	small.entities["dcim/devices"] = []map[string]interface{}{
		device(1, "", "active"), device(2, nil, "offline"), device(3, "x", "active"), device(4, "", "active"),
	}
	for _, row := range small.entities["dcim/devices"] {
		row["serial"] = nil
	}
	wantRows(t, negationQuery(t, newTestProvider(t, small), provider.QuerySpec{Fields: []string{"id"}, Filters: []provider.Filter{
		{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nic", Value: "zz"},
		{Field: "serial", Operator: "nic", Value: "qq"}, {Field: "status", Operator: "n", Value: "offline"}}}), []int{1, 4})
	wantRows(t, negationQuery(t, newTestProvider(t, small), provider.QuerySpec{Fields: []string{"id"}, Filters: []provider.Filter{
		{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nic", Value: "são"}}}), []int{1, 2, 4})
}

// The catalogue's operators translate to the editor's tokens, and a text
// column that lists gt is offered is-empty and has-any-value: the replica is
// asked for the comparison, whatever its nullability (a NOT NULL text column
// holds "" too) and its other operators. A number or a timestamp is offered
// them where it can be NULL, as before.
func TestSeamOperators_TextEmptiness(t *testing.T) {
	all := []string{"eq", "gt", "lt", "in", "isnull"}
	text := append(slices.Clone(all), "ilike")
	for _, tc := range []struct {
		name string
		col  column
		want []string
	}{
		{"nullable text", column{Type: "VARCHAR", Nullable: true, Operators: text}, []string{"", "n", "ic", "nic", "gt", "lt", "empty", "nempty"}},
		{"NOT NULL text", column{Type: "VARCHAR", Operators: text}, []string{"", "n", "ic", "nic", "gt", "lt", "empty", "nempty"}},
		{"text with the anchored matches", column{Type: "VARCHAR", Operators: append(slices.Clone(text), "istartswith", "iendswith", "iexact")},
			[]string{"", "n", "ic", "nic", "isw", "nisw", "iew", "niew", "ie", "nie", "gt", "lt", "empty", "nempty"}},
		{"text that lists no isnull", column{Type: "VARCHAR", Operators: []string{"eq", "gt", "lt", "in", "ilike"}},
			[]string{"", "n", "ic", "nic", "gt", "lt", "empty", "nempty"}},
		{"another spelling of a text type", column{Type: "TEXT", Operators: text}, []string{"", "n", "ic", "nic", "gt", "lt", "empty", "nempty"}},
		{"text that lists no gt", column{Type: "VARCHAR", Nullable: true, Operators: []string{"eq", "in", "isnull", "ilike"}}, []string{"", "n", "ic", "nic"}},
		{"a type that is no text", column{Type: "UUID", Nullable: true, Operators: all}, []string{"", "gt", "lt"}},
		{"nullable number", column{Type: "BIGINT", Nullable: true, Operators: all}, []string{"", "n", "gt", "gte", "lt", "lte", "empty", "nempty"}},
		{"NOT NULL number", column{Type: "BIGINT", Operators: all}, []string{"", "n", "gt", "gte", "lt", "lte"}},
		{"nullable timestamp", column{Type: "TIMESTAMP WITH TIME ZONE", Nullable: true, Operators: all}, []string{"", "gt", "lt", "empty", "nempty"}},
		{"NOT NULL timestamp", column{Type: "TIMESTAMP WITH TIME ZONE", Operators: all}, []string{"", "gt", "lt"}},
	} {
		if got := seamOperators(tc.col); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An alert reads the total alone, and it is exact for both: has any value is
// one count of the comparison, is empty the base less that count. No row is
// read for either.
func TestEmptiness_Count(t *testing.T) {
	for _, c := range []struct {
		name   string
		filter provider.Filter
		want   int
	}{
		{"has any value", provider.Filter{Field: "name", Operator: "nempty"}, 2},
		{"is empty", provider.Filter{Field: "name", Operator: "empty"}, 3},
		{"NetBox's name__empty=false", provider.Filter{Field: "name__empty", Value: "false"}, 2},
		{"NetBox's name__empty=true", provider.Filter{Field: "name__empty", Value: "true"}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, p := emptyProvider(t, "name")
			res := negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: []provider.Filter{c.filter}})
			if res.Total != c.want {
				t.Errorf("Total = %d, want %d", res.Total, c.want)
			}
			f.mu.Lock()
			for _, r := range f.requests {
				if r.entity == "dcim/devices" && r.query.Get("limit") != "1" {
					t.Errorf("a count-only query read rows: %v", r.query)
				}
			}
			f.mu.Unlock()
			noIsNull(t, f)
		})
	}

	// On a plugin's column too, where the filter is the operator the editor offers.
	_, p := widgetsProvider(t)
	for op, want := range map[string]int{"empty": 2, "nempty": 2} {
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: pluginWidgets, CountOnly: true, Filters: []provider.Filter{{Field: "name", Operator: op}}})
		if err != nil {
			t.Errorf("widgets name %s: %v", op, err)
			continue
		}
		if res.Total != want {
			t.Errorf("widgets name %s: Total %d, want %d", op, res.Total, want)
		}
	}
}

// A column that is not text keeps what it had: is empty is IS NULL where the
// column can be NULL, and the refusal where it cannot. Nothing is sent as a
// comparison with the empty string.
func TestEmptiness_NonTextUnchanged(t *testing.T) {
	f := newFakeService()
	f.addEntity(pluginWidgets, "id:BIGINT:pk", "name:VARCHAR", "qty:BIGINT", "seen:TIMESTAMP WITH TIME ZONE")
	f.entities[pluginWidgets] = []map[string]interface{}{
		{"id": 1, "name": "a", "qty": 5, "seen": "2026-05-01T10:00:00+00:00"},
		{"id": 2, "name": "b", "qty": nil, "seen": nil},
		{"id": 3, "name": "c", "qty": 0, "seen": nil},
	}
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "mtu:INTEGER")
	f.entities["dcim/interfaces"] = []map[string]interface{}{{"id": 1, "name": "eth0", "mtu": nil}, {"id": 2, "name": "eth1", "mtu": 1500}}
	p := newTestProvider(t, f)

	for _, c := range []struct {
		objectType string
		filter     provider.Filter
		isnull     string
		want       []int
	}{
		{pluginWidgets, provider.Filter{Field: "qty", Operator: "empty"}, "filter[qty]__isnull=true", []int{2}},
		{pluginWidgets, provider.Filter{Field: "qty", Operator: "nempty"}, "filter[qty]__isnull=false", []int{1, 3}},
		{pluginWidgets, provider.Filter{Field: "seen", Operator: "empty"}, "filter[seen]__isnull=true", []int{2, 3}},
		// NetBox's own filter on a number is IS NULL (mtu__empty).
		{"dcim/interfaces", provider.Filter{Field: "mtu", Operator: "empty"}, "filter[mtu]__isnull=true", []int{1}},
		{"dcim/interfaces", provider.Filter{Field: "mtu__empty", Value: "false"}, "filter[mtu]__isnull=false", []int{2}},
	} {
		f.forget()
		got := relatedRows(t, p, c.objectType, c.filter)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %+v: rows %v, want %v", c.objectType, c.filter, got, c.want)
		}
		key, value, _ := strings.Cut(c.isnull, "=")
		if reqs := f.sentWith(c.objectType, key); len(reqs) == 0 || reqs[0].query.Get(key) != value {
			t.Errorf("%s %+v: no request carried %s", c.objectType, c.filter, c.isnull)
		}
		for _, col := range []string{"qty", "seen", "mtu"} {
			if got := gtValues(f, c.objectType, col); len(got) != 0 {
				t.Errorf("%s %+v: %s was compared with gt %q", c.objectType, c.filter, col, got)
			}
		}
	}

	// A column that cannot be NULL is never empty, and the refusal says so.
	f.forget()
	u := relatedRefusal(t, p, pluginWidgets, provider.Filter{Field: "id", Operator: "empty"})
	if u.Field != "id" || u.Operator != "empty" || !strings.Contains(u.Reason, "NOT NULL") {
		t.Errorf("refusal %q %q %q, want id empty, saying the column is NOT NULL", u.Field, u.Operator, u.Reason)
	}
	noRequestsFor(t, f, pluginWidgets)
}

// NetBox's empty lookup means what it says on an `empty`-kind filter: "is
// empty" is NULL or "", "has any value" a value, however the filter is
// written, and of several the last is the answer, as NetBox's single-valued
// filters answer. Translation leaves the operators for the replica's terms to
// answer (vocabfilters.go), so this is pinned from the filter to the rows, and
// at the translation itself: a mapping swapped there would hand every
// dashboard's "is empty" the rows with a value, and read as healthy.
func TestEmptiness_NetBoxLookupMeansWhatItSays(t *testing.T) {
	for _, c := range []struct {
		name    string
		filters []provider.Filter
		want    []int
	}{
		{"is empty", []provider.Filter{{Field: "name", Operator: "empty"}}, []int{1, 2, 4}},
		{"has any value", []provider.Filter{{Field: "name", Operator: "nempty"}}, []int{3, 5}},
		{"name__empty=true", []provider.Filter{{Field: "name__empty", Value: "true"}}, []int{1, 2, 4}},
		{"name__empty=True", []provider.Filter{{Field: "name__empty", Value: "True"}}, []int{1, 2, 4}},
		{"name__empty=2", []provider.Filter{{Field: "name__empty", Value: "2"}}, []int{1, 2, 4}},
		{"name__empty=false", []provider.Filter{{Field: "name__empty", Value: "false"}}, []int{3, 5}},
		{"name__empty=False", []provider.Filter{{Field: "name__empty", Value: "False"}}, []int{3, 5}},
		{"name__empty=3", []provider.Filter{{Field: "name__empty", Value: "3"}}, []int{3, 5}},
		{"the last of is empty and has any value", []provider.Filter{{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nempty"}}, []int{3, 5}},
		{"the last of has any value and is empty", []provider.Filter{{Field: "name", Operator: "nempty"}, {Field: "name", Operator: "empty"}}, []int{1, 2, 4}},
		{"either spelling is the one filter", []provider.Filter{{Field: "name", Operator: "nempty"}, {Field: "name__empty", Value: "true"}}, []int{1, 2, 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, p := emptyProvider(t, "name")
			if got := relatedRows(t, p, "dcim/devices", c.filters...); !slices.Equal(got, c.want) {
				t.Errorf("%+v: rows %v, want %v", c.filters, got, c.want)
			}
		})
	}

	devices := vocabDevices()
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name", Operator: "empty"}),
		provider.Filter{Field: "name", Operator: opEmpty, Label: "name"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name", Operator: "nempty"}),
		provider.Filter{Field: "name", Operator: opNEmpty, Label: "name"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name__empty", Value: "true"}),
		provider.Filter{Field: "name", Operator: opEmpty, Label: "name__empty"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name__empty", Value: "false"}),
		provider.Filter{Field: "name", Operator: opNEmpty, Label: "name__empty"})
}

// rewriteEmptiness puts NetBox's empty lookup on a text column on the two
// internal operators, and leaves everything else as it is: the user's field and
// operator stay on the row for a refusal to name, the slice it was given is not
// changed, and a column that is not text keeps its rows for validateFilters.
func TestRewriteEmptiness(t *testing.T) {
	covered := serve("dcim/devices", "dcim_device", textColumn("name"), numberColumn("tenant_id", "BIGINT"))
	plugin := serve(pluginWidgets, fakeTable(pluginWidgets), textColumn("name"), numberColumn("qty", "BIGINT"),
		column{Name: "site_id", Type: "BIGINT", Nullable: true, Operators: v140NumberOps,
			Ref: &reference{Path: "/v1/dcim/sites", ExpandKey: "site", Columns: []string{"name", "slug"}, Available: true}})
	unfed := entity{Table: "dcim_device"}

	for _, c := range []struct {
		name string
		e    entity
		in   []provider.Filter
		want []provider.Filter // nil: the rows as they were given
	}{
		{"a text column of NetBox's model", covered.e,
			[]provider.Filter{{Field: "name", Operator: opEmpty, Label: "name"}},
			[]provider.Filter{{Field: "name", Operator: opNotGTBlank, Label: "name", LabelOperator: opEmpty}}},
		{"has any value", covered.e,
			[]provider.Filter{{Field: "name", Operator: opNEmpty, Label: "name"}},
			[]provider.Filter{{Field: "name", Operator: opWireGTBlank, Label: "name", LabelOperator: opNEmpty}}},
		{"the field as the user wrote it is kept", covered.e,
			[]provider.Filter{{Field: "name", Operator: opEmpty, Label: "name__empty"}},
			[]provider.Filter{{Field: "name", Operator: opNotGTBlank, Label: "name__empty", LabelOperator: opEmpty}}},
		{"a text column of a plugin's model", plugin.e,
			[]provider.Filter{{Field: "name", Operator: opEmpty}},
			[]provider.Filter{{Field: "name", Operator: opNotGTBlank, Label: "name", LabelOperator: opEmpty}}},
		// What the translation produced for a filter on a column the replica
		// does not store: the rewrite carries it on, so the refusal says the
		// replica does not store the column NetBox reads.
		{"a column of NetBox's model the replica lacks", covered.e,
			[]provider.Filter{{Field: "description", Operator: opEmpty, Label: "description"}},
			[]provider.Filter{{Field: "description", Operator: opNotGTBlank, Label: "description", LabelOperator: opEmpty}}},
		// An entity with no columns yet is not told apart by them: NetBox's
		// lookup is on text whatever the replica stores.
		{"an entity with no data yet", unfed,
			[]provider.Filter{{Field: "name", Operator: opNEmpty, Label: "name"}},
			[]provider.Filter{{Field: "name", Operator: opWireGTBlank, Label: "name", LabelOperator: opNEmpty}}},
		// Of two rows on one column the last is the answer; rows beside them stay.
		{"the last row on a column wins", plugin.e,
			[]provider.Filter{{Field: "name", Operator: opEmpty}, {Field: "name", Operator: "ic", Value: "x"}, {Field: "name", Operator: opNEmpty}, {Field: "qty", Operator: opEmpty}},
			[]provider.Filter{{Field: "name", Operator: "ic", Value: "x"}, {Field: "name", Operator: opWireGTBlank, Label: "name", LabelOperator: opNEmpty}, {Field: "qty", Operator: opEmpty}}},

		{"a number column", covered.e, []provider.Filter{{Field: "tenant_id", Operator: opEmpty}}, nil},
		{"a number column of a plugin's model", plugin.e, []provider.Filter{{Field: "qty", Operator: opNEmpty}}, nil},
		// The rows translation passes through unchanged are not NetBox's lookup.
		{"a custom field", covered.e, []provider.Filter{{Field: "cf_tier", Operator: opEmpty}}, nil},
		{"tags", covered.e, []provider.Filter{{Field: "tag", Operator: opNEmpty}}, nil},
		{"the search", covered.e, []provider.Filter{{Field: "q", Operator: opEmpty}}, nil},
		{"a name joined in from another table", plugin.e, []provider.Filter{{Field: "site", Operator: opEmpty}}, nil},
		{"a column a plugin's model lacks", plugin.e, []provider.Filter{{Field: "colour", Operator: opEmpty}}, nil},
		// The limit of the guard for an entity with no data yet: nothing says a
		// plugin's column is text, so the row is left as it is (asksTextEmptiness).
		{"an entity of a plugin with no data yet", entity{Table: fakeTable(pluginWidgets)}, []provider.Filter{{Field: "name", Operator: opEmpty}}, nil},
		{"a row with no field", covered.e, []provider.Filter{{Operator: opEmpty}}, nil},
		{"another operator", covered.e, []provider.Filter{{Field: "name", Operator: "ic", Value: "x"}}, nil},
		{"no rows", covered.e, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			given := slices.Clone(c.in)
			got := rewriteEmptiness(c.in, c.e, nil)
			want := c.want
			if want == nil {
				want = c.in
			}
			if !slices.Equal(got, want) {
				t.Errorf("rewritten to %+v,\nwant %+v", got, want)
			}
			if !slices.Equal(c.in, given) {
				t.Errorf("the slice it was given was changed: %+v, was %+v", c.in, given)
			}
		})
	}
}

// A plugin's text column, which the catalogue's rules govern: the filters the
// editor offers are answered as NetBox's lookup, and of two rows on one column
// the last is, as the editor says ("only the last one is applied") and as an
// is-empty on a number is (the parameter is set, not added). NetBox's own
// models read the rows of one filter together and keep the last, in translation.
func TestEmptiness_OnAPluginsTextColumn(t *testing.T) {
	_, p := widgetsProvider(t)
	for _, c := range []struct {
		name    string
		filters []provider.Filter
		want    []int
	}{
		{"is empty", []provider.Filter{{Field: "name", Operator: "empty"}}, []int{1, 3}},
		{"has any value", []provider.Filter{{Field: "name", Operator: "nempty"}}, []int{2, 4}},
		{"empty, then has any value", []provider.Filter{{Field: "name", Operator: "empty"}, {Field: "name", Operator: "nempty"}}, []int{2, 4}},
		{"has any value, then empty", []provider.Filter{{Field: "name", Operator: "nempty"}, {Field: "name", Operator: "empty"}}, []int{1, 3}},
		{"empty twice", []provider.Filter{{Field: "name", Operator: "empty"}, {Field: "name", Operator: "empty"}}, []int{1, 3}},
		{"two columns are two filters", []provider.Filter{{Field: "name", Operator: "empty"}, {Field: "code", Operator: "nempty"}}, []int{3}},
		{"beside a text match", []provider.Filter{{Field: "code", Operator: "empty"}, {Field: "name", Operator: "ic", Value: "a"}}, []int{2}},
	} {
		if got := relatedRows(t, p, pluginWidgets, c.filters...); !slices.Equal(got, c.want) {
			t.Errorf("%s: rows %v, want %v", c.name, got, c.want)
		}
	}
}

// An unfed entity has no columns to judge a filter against, and its row route
// answers 404 for it. On a table NetBox's vocabulary covers, its empty lookup is
// on text whatever the replica stores, so "has any value" is the comparison it
// would be, sent; and "is empty", which needs the rows to be answered, is
// refused, as a negation is: the request is never the IS NULL, which on a replica
// whose catalogue is ten minutes stale would answer the opposite question. (A
// plugin's table has only its columns to say a filter is on text: see the row of
// TestRewriteEmptiness for one with no data yet.)
func TestEmptiness_OnAnUnfedEntity(t *testing.T) {
	f := newFakeService()
	p := newTestProvider(t, f)

	// dcim/platforms has received no data in the fake's catalogue.
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/platforms",
		Filters: []provider.Filter{{Field: "name", Operator: "nempty"}}})
	if u := provider.Classify(err); u == nil || u.Kind != provider.ErrorKindNotReplicated {
		t.Errorf("has any value: err = %v / %+v, want not-replicated", err, u)
	}
	if got := gtValues(f, "dcim/platforms", "name"); !slices.Equal(got, []string{""}) {
		t.Errorf("filter[name]__gt was sent as %q, want once with an empty value", got)
	}
	noIsNull(t, f)

	f.forget()
	for _, c := range []struct {
		filter   provider.Filter
		field    string
		operator string
	}{
		{provider.Filter{Field: "name", Operator: "empty"}, "name", "empty"},
		{provider.Filter{Field: "name__empty", Value: "true"}, "name__empty", "empty"},
	} {
		u := relatedRefusal(t, p, "dcim/platforms", c.filter)
		if u.Field != c.field || u.Operator != c.operator || !strings.Contains(u.Reason, "works once this replica has received data for this object type") {
			t.Errorf("%+v: refusal %q %q %q, want it to name %s %s and say when it works", c.filter, u.Field, u.Operator, u.Reason, c.field, c.operator)
		}
	}
	noRequestsFor(t, f, "dcim/platforms")
}

// A greater-than the panel wrote on the column has the same key on the wire as
// "has any value": the blank must neither overwrite it nor be written beside it,
// whichever comes first. Whatever is greater than a non-empty value is not the
// empty string, so the panel's comparison is the whole of both. (A plugin's text
// column: NetBox has no greater-than on a name.)
func TestEmptiness_BesideAComparisonOnTheSameColumn(t *testing.T) {
	for _, c := range []struct {
		name    string
		filters []provider.Filter
		gt      string // what every request sent for the column's gt
		want    []int
	}{
		{"has any value after it", []provider.Filter{{Field: "name", Operator: "gt", Value: "a"}, {Field: "name", Operator: "nempty"}}, "a", []int{4}},
		{"has any value before it", []provider.Filter{{Field: "name", Operator: "nempty"}, {Field: "name", Operator: "gt", Value: "a"}}, "a", []int{4}},
		{"a range beside it", []provider.Filter{{Field: "name", Operator: "nempty"}, {Field: "name", Operator: "lt", Value: "b"}}, "", []int{2}},
		// No row greater than "a" is empty, however the counts are taken.
		{"is empty beside it", []provider.Filter{{Field: "name", Operator: "gt", Value: "a"}, {Field: "name", Operator: "empty"}}, "a", []int{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, p := widgetsProvider(t)
			res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: pluginWidgets, Fields: []string{"id"}, Ordering: "id", Filters: c.filters})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			wantRows(t, res, c.want)
			sent := gtValues(f, pluginWidgets, "name")
			if len(sent) == 0 {
				t.Fatal("no request carried a gt on name")
			}
			for _, v := range sent {
				if v != c.gt {
					t.Errorf("filter[name]__gt was sent as %q beside the comparison, want %q", v, c.gt)
				}
			}
		})
	}
}

// The operators the rewrite writes are this datasource's own, as the null
// operators are: a query that names one is refused before anything is read, on
// NetBox's model and on a plugin's, rather than taken for what the rewrite
// means by it.
func TestEmptiness_InternalOperatorsAreNeverAcceptedFromAQuery(t *testing.T) {
	f, p := widgetsProvider(t)
	f.entities["dcim/devices"] = textRows("name")
	for _, objectType := range []string{pluginWidgets, "dcim/devices"} {
		for _, op := range []string{opWireGTBlank, opNotGTBlank} {
			u := relatedRefusal(t, p, objectType, provider.Filter{Field: "name", Operator: op})
			if u.Field != "name" || u.Operator != op || !strings.Contains(u.Reason, "one this datasource writes for itself") {
				t.Errorf("%s %q: refusal %q %q %q, want the internal-operator refusal", objectType, op, u.Field, u.Operator, u.Reason)
			}
		}
		if n := f.countRequestsFor(objectType); n != 0 {
			t.Errorf("%s: %d requests sent for a refused operator", objectType, n)
		}
	}
	for _, op := range []string{opWireGTBlank, opNotGTBlank} {
		if !internalOperators[op] || !strings.Contains(op, " ") {
			t.Errorf("%q must be an internal operator no saved query can carry (a name with a space)", op)
		}
	}
}

// Every refusal after the rewrite names the filter as the user wrote it — the
// field, and "is empty" or "has any value" — never the comparison it was sent
// as, and says what to do.
func TestEmptiness_RefusalsNameWhatTheUserWrote(t *testing.T) {
	f := newFakeService()
	f.addEntity(pluginWidgets, "id:BIGINT:pk", "code:VARCHAR", "name:VARCHAR", "site_id:BIGINT")
	f.addReference(pluginWidgets, "site_id", "dcim/sites", "site", "name", "slug")
	// A build whose text column lists no gt cannot be asked what NetBox's lookup
	// means.
	f.mu.Lock()
	e := f.schema.Entities["/v1/"+pluginWidgets]
	for i, c := range e.Columns {
		if c.Name == "code" {
			e.Columns[i].Operators = []string{"eq", "in", "isnull", "ilike"}
		}
	}
	f.schema.Entities["/v1/"+pluginWidgets] = e
	f.mu.Unlock()
	f.entities[pluginWidgets] = []map[string]interface{}{{"id": 1, "code": "", "name": "", "site_id": nil}}
	f.entities["dcim/devices"] = textRows("name")
	p := newTestProvider(t, f)

	for _, c := range []struct {
		name       string
		objectType string
		filter     provider.Filter
		field, op  string // what the refusal names
		say        string // and says
	}{
		{"has any value on a column without gt", pluginWidgets, provider.Filter{Field: "code", Operator: "nempty"},
			"code", "nempty", "its schema lists no gt there"},
		{"is empty on a column without gt", pluginWidgets, provider.Filter{Field: "code", Operator: "empty"},
			"code", "empty", "its schema lists no gt there"},
		{"is empty on a name joined in from another table", pluginWidgets, provider.Filter{Field: "site", Operator: "empty"},
			"site", "empty", "filter on site_id"},
		{"has any value on one", pluginWidgets, provider.Filter{Field: "site_slug", Operator: "nempty"},
			"site_slug", "nempty", "filter on site_id"},
		{"a column of NetBox's model the replica does not store", "dcim/devices", provider.Filter{Field: "description", Operator: "empty"},
			"description", "empty", "this replica does not store description, so it cannot answer the filter; remove it, or choose a filter the editor offers"},
		{"the same, in NetBox's spelling", "dcim/devices", provider.Filter{Field: "description__empty", Value: "false"},
			"description__empty", "nempty", "this replica does not store description, the column NetBox reads for description__empty, so it cannot answer the filter"},
		{"a custom field", "dcim/devices", provider.Filter{Field: "cf_tier", Operator: "empty"},
			"cf_tier", "empty", "this replica has no such column to filter on"},
		{"tags", "dcim/devices", provider.Filter{Field: "tag", Operator: "nempty"},
			"tag", "nempty", "this replica has no such column to filter on"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.forget()
			u := relatedRefusal(t, p, c.objectType, c.filter)
			if u.Field != c.field || u.Operator != c.op {
				t.Errorf("refusal names %q %q, want %q %q", u.Field, u.Operator, c.field, c.op)
			}
			if strings.Contains(u.Reason, "gt blank") {
				t.Errorf("reason %q names an internal operator", u.Reason)
			}
			if !strings.Contains(u.Reason, c.say) {
				t.Errorf("reason %q, want it to say %q", u.Reason, c.say)
			}
			noRequestsFor(t, f, c.objectType)
		})
	}
}

// validateFilters is the last check before a filter is sent, and it does not
// trust the rewrite before it: a row that reached it as is-empty on a text
// column would be sent as IS NULL, which finds none of the 23 blank rows NetBox's
// lookup does (DATA-206), so it is refused — with a sentence that points at
// what works — and never sent. Has any value, as the rewrite writes it, is
// accepted where the column lists gt.
func TestValidateFilters_RawEmptinessOnTextIsNeverSent(t *testing.T) {
	c := catalogFromFake(t, devicesSchema())
	e := c.Entities["dcim/devices"]
	for _, op := range []string{opEmpty, opNEmpty} {
		err := validateFilters([]provider.Filter{{Field: "name", Operator: op}}, e, c)
		var u *UnsupportedFilterError
		if !errors.As(err, &u) || u.Operator != op {
			t.Errorf("%q on text: err = %v, want a refusal naming it", op, err)
			continue
		}
		if strings.Contains(u.Reason, "equality with an empty value") || !strings.Contains(u.Reason, netboxModeAdvice) {
			t.Errorf("%q on text: reason %q, want it to point at NetBox mode and not at an equality that sends nothing", op, u.Reason)
		}
		// Either row is sent as an isnull, so the sentence is about both.
		for _, say := range []string{"is empty", "has any value", "IS NULL", "IS NOT NULL"} {
			if !strings.Contains(u.Reason, say) {
				t.Errorf("%q on text: reason %q does not say %q", op, u.Reason, say)
			}
		}
	}
	if err := validateFilters([]provider.Filter{{Field: "name", Operator: opWireGTBlank}}, e, c); err != nil {
		t.Errorf("has any value on text, as rewritten: %v", err)
	}
	// It has no value to send, and is not skipped as a row that sends nothing: on
	// a column the replica does not store it is refused.
	if err := validateFilters([]provider.Filter{{Field: "colour", Operator: opWireGTBlank}}, e, c); err == nil {
		t.Error("has any value on a column the replica does not store was accepted")
	}
}

// Equality with an empty value sends no parameter, as in NetBox mode, which
// drops an empty value too: the replica takes `eq ""` but nothing asks it. Is
// empty is not an equality with "", and is answered as NetBox's lookup.
func TestEqualityWithAnEmptyValueSendsNothing(t *testing.T) {
	q, err := buildFilterValues([]provider.Filter{{Field: "serial", Operator: "exact", Value: ""}}, nil)
	if err != nil {
		t.Fatalf("buildFilterValues: %v", err)
	}
	if len(q) != 0 {
		t.Errorf("an empty equality value must emit no filter, got %v", q)
	}
}

// buildFilterValues writes has any value as the comparison with the empty
// string, which has no value to read, and a comparison on the column beside it
// wins whichever comes first. Each key is written once: the replica answers a
// filter key it is given twice with a 400 (TestFake_RepeatedFilterKeyIs400), so
// a second gt beside the first, which Add would write, is a request it refuses.
func TestBuildFilterValues_HasAnyValueOnText(t *testing.T) {
	for _, c := range []struct {
		name    string
		filters []provider.Filter
		want    map[string]string
	}{
		{"alone", []provider.Filter{{Field: "name", Operator: opWireGTBlank}}, map[string]string{"filter[name]__gt": ""}},
		{"twice", []provider.Filter{{Field: "name", Operator: opWireGTBlank}, {Field: "name", Operator: opWireGTBlank}}, map[string]string{"filter[name]__gt": ""}},
		{"beside a comparison", []provider.Filter{{Field: "name", Operator: opWireGTBlank}, {Field: "name", Operator: "gt", Value: "m"}}, map[string]string{"filter[name]__gt": "m"}},
		{"after a comparison", []provider.Filter{{Field: "name", Operator: "gt", Value: "m"}, {Field: "name", Operator: opWireGTBlank}}, map[string]string{"filter[name]__gt": "m"}},
		{"beside another operator", []provider.Filter{{Field: "name", Operator: opWireGTBlank}, {Field: "name", Operator: "lt", Value: "m"}},
			map[string]string{"filter[name]__gt": "", "filter[name]__lt": "m"}},
		{"on two columns", []provider.Filter{{Field: "name", Operator: opWireGTBlank}, {Field: "code", Operator: opWireGTBlank}},
			map[string]string{"filter[name]__gt": "", "filter[code]__gt": ""}},
	} {
		q, err := buildFilterValues(c.filters, nil)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(q) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, q, c.want)
		}
		for k, v := range c.want {
			if !q.Has(k) || q.Get(k) != v {
				t.Errorf("%s: %s = %q (sent %v), want %q", c.name, k, q.Get(k), q.Has(k), v)
			}
		}
		for k, vs := range q {
			if len(vs) != 1 {
				t.Errorf("%s: %s is written %d times (%q), want once", c.name, k, len(vs), vs)
			}
		}
	}
}

// The text matches refuse an empty value on the replica, and eq, in, gt and lt
// take one: the fake has to say so, or a test that sent an empty text match
// would pass where the replica answers 400, and one that compared with the empty
// string would find nothing.
func TestFake_EmptyValueOnText(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(devicesSchema())
	f.entities["dcim/devices"] = textRows("name")
	srv := f.start(t)
	for _, c := range []struct {
		param  string
		status int
		answer string
	}{
		{"filter[name]__ilike", 400, "invalid filter value for name: a text filter takes a non-empty value"},
		{"filter[name]__istartswith", 400, "invalid filter value for name: a text filter takes a non-empty value"},
		{"filter[name]__iendswith", 400, "invalid filter value for name: a text filter takes a non-empty value"},
		{"filter[name]__iexact", 400, "invalid filter value for name: a text filter takes a non-empty value"},
		{"filter[name]__eq", 200, "2"},     // the two blank rows
		{"filter[name]__in", 200, "2"},     // the same
		{"filter[name]__gt", 200, "2"},     // the two with a value, "x" and "core-1"; NULL compares as nothing
		{"filter[name]__lt", 200, "0"},     // nothing is less than the empty string
		{"filter[name]__isnull", 200, "1"}, // the NULL alone, which is why isnull answers another question
		{"filter[id]__gt", 400, "invalid filter value for id"},
	} {
		value := ""
		if strings.HasSuffix(c.param, "isnull") {
			value = "true"
		}
		if status, answer := fakeGet(t, srv.URL, "dcim/devices", c.param, value); status != c.status || answer != c.answer {
			t.Errorf("%s=%q: HTTP %d %q, want %d %q", c.param, value, status, answer, c.status, c.answer)
		}
	}
}

// The replica answers a filter key it is given twice with a 400 that names it,
// whatever the values (measured on v1.40.1, ipam/ip-addresses: filter[dns_name]__gt=
// beside filter[dns_name]__gt=x in either order, and __eq and __in twice, are each
// 400 "duplicate filter: filter[dns_name]__gt"; on a table that never received data
// it comes before the table's own 404), and takes one column under two operators
// (gt with lt), or one operator on two columns, which are different keys. The fake
// has to say so: it reads the first value of a key, so a request that repeats one
// — a has any value written beside the gt the panel wrote on the column — would
// pass every test while the replica refuses it.
func TestFake_RepeatedFilterKeyIs400(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(devicesSchema())
	f.entities["dcim/devices"] = textRows("name")
	srv := f.start(t)
	query := func(pairs ...string) string {
		q := url.Values{}
		for i := 0; i < len(pairs); i += 2 {
			q.Add(pairs[i], pairs[i+1])
		}
		return q.Encode()
	}
	for _, c := range []struct {
		name   string
		entity string
		query  string
		status int
		answer string
	}{
		{"gt twice, an empty value first", "dcim/devices", query("filter[name]__gt", "", "filter[name]__gt", "x"), 400, "duplicate filter: filter[name]__gt"},
		{"gt twice, an empty value last", "dcim/devices", query("filter[name]__gt", "x", "filter[name]__gt", ""), 400, "duplicate filter: filter[name]__gt"},
		{"gt twice, the same value", "dcim/devices", query("filter[name]__gt", "x", "filter[name]__gt", "x"), 400, "duplicate filter: filter[name]__gt"},
		{"eq twice", "dcim/devices", query("filter[name]__eq", "a", "filter[name]__eq", "b"), 400, "duplicate filter: filter[name]__eq"},
		{"in twice", "dcim/devices", query("filter[id]__in", "1", "filter[id]__in", "2"), 400, "duplicate filter: filter[id]__in"},
		{"a repeat among other filters", "dcim/devices", query("filter[id]__gt", "0", "filter[name]__gt", "", "filter[name]__gt", "x", "filter[name]__lt", "z"), 400, "duplicate filter: filter[name]__gt"},
		{"before the table's own 404", "dcim/platforms", query("filter[name]__gt", "", "filter[name]__gt", "x"), 400, "duplicate filter: filter[name]__gt"},

		// What is not a repeat.
		{"gt once", "dcim/devices", query("filter[name]__gt", ""), 200, "2"},
		{"gt and lt on one column", "dcim/devices", query("filter[name]__gt", "", "filter[name]__lt", "z"), 200, "2"},
		{"gt on two columns", "dcim/devices", query("filter[name]__gt", "", "filter[serial]__gt", ""), 200, "0"},
		{"an unfed table answers its own 404", "dcim/platforms", query("filter[name]__gt", ""), 404, "no data received for this entity"},
	} {
		if status, answer := fakeGetQuery(t, srv.URL, c.entity, c.query); status != c.status || answer != c.answer {
			t.Errorf("%s: HTTP %d %q, want %d %q", c.name, status, answer, c.status, c.answer)
		}
	}
}

// The editor is offered is-empty and has-any-value where a query is answered
// them: on NetBox's own filters whose __empty is the empty lookup on text, and
// on a plugin's text columns — whatever the nullability — and not on what is
// joined in from another table, nor on a column that lists no gt.
func TestFilterFields_OffersTextEmptiness(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(withPluginDevices(devicesSchema()))
	f.entities["dcim/devices"] = textRows("name")
	f.entities[pluginDevices] = []map[string]interface{}{}
	for _, target := range []string{"dcim/sites", "dcim/device-roles", "tenancy/tenants", "dcim/racks"} {
		f.entities[target] = []map[string]interface{}{{"id": 1, "name": "One", "slug": "one"}}
	}
	// The plugin's other text column lists no gt.
	f.mu.Lock()
	e := f.schema.Entities["/v1/"+pluginDevices]
	e.Columns = append(slices.Clone(e.Columns), fakeColumn{Name: "code", Type: "VARCHAR", Nullable: true, Operators: []string{"eq", "in", "isnull", "ilike"}})
	f.schema.Entities["/v1/"+pluginDevices] = e
	f.mu.Unlock()
	p := newTestProvider(t, f)

	offered := func(ops []string) bool { return slices.Contains(ops, "empty") && slices.Contains(ops, "nempty") }
	covered := filterOps(t, p, "dcim/devices")
	for _, name := range []string{"name", "serial", "status"} {
		if !offered(covered[name]) {
			t.Errorf("devices %s: %v, want is empty and has any value", name, covered[name])
		}
	}
	for _, name := range []string{"site", "role", "tenant", "rack_id"} {
		if ops, ok := covered[name]; ok && offered(ops) {
			t.Errorf("devices %s: %v, want no is empty on a related object", name, ops)
		}
	}
	plugin := filterOps(t, p, pluginDevices)
	for _, name := range []string{"name", "serial", "status", "created", "position"} {
		if !offered(plugin[name]) {
			t.Errorf("plugin %s: %v, want is empty and has any value", name, plugin[name])
		}
	}
	for _, name := range []string{"code", "id", "is_full_depth", "site", "site_slug", "role", "rack"} {
		if ops, ok := plugin[name]; ok && (slices.Contains(ops, "empty") || slices.Contains(ops, "nempty")) {
			t.Errorf("plugin %s: %v, want no is empty where a query would be refused it", name, ops)
		}
	}

	// And every operator offered is one a query takes.
	assertAdvertisedOperatorsAccepted(t, p, "dcim/devices", covered)
	assertAdvertisedOperatorsAccepted(t, p, pluginDevices, plugin)
}
