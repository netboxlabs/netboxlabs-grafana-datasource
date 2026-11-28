package replicacache

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A long id filter is read in batches, as NetBox mode reads one (#32), and
// has_primary_ip is answered as two disjoint parts. Both merge into what one
// read would return: totals summed, rows in the replica's order.

// idDevices is a devices table of ids 1..n, every one active but those offline
// names, with primary addresses on the ids primary names (4 and 6 of one id).
func idDevices(n int, offline ...int) *fakeService {
	f := newFakeService()
	f.addColumns("dcim/devices", "primary_ip4_id:BIGINT", "primary_ip6_id:BIGINT")
	rows := make([]map[string]interface{}, n)
	for i := range rows {
		id := i + 1
		status := "active"
		if slices.Contains(offline, id) {
			status = "offline"
		}
		rows[i] = map[string]interface{}{"id": float64(id), "name": fmt.Sprintf("d%05d", id), "serial": fmt.Sprintf("s%05d", n-id),
			"status": status, "site_id": nil, "role_id": nil, "tenant_id": nil, "rack_id": nil, "platform_id": nil,
			"primary_ip4_id": nil, "primary_ip6_id": nil, "custom_field_data": "{}"}
	}
	f.entities["dcim/devices"] = rows
	return f
}

// idValues is an id filter value of ids from..to.
func idValues(from, to int) string {
	vals := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		vals = append(vals, strconv.Itoa(i))
	}
	return strings.Join(vals, ",")
}

func resultIDs(t *testing.T, res *provider.Result) []int {
	t.Helper()
	out := make([]int, 0, len(res.Rows))
	for _, r := range res.Rows {
		id, ok := toInt(r["id"])
		if !ok {
			t.Fatalf("row id %#v", r["id"])
		}
		out = append(out, id)
	}
	return out
}

func span(from, to int) []int {
	out := make([]int, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// idReads is the devices requests that filtered by id.
func idReads(f *fakeService) []recordedRequest {
	return f.sentWith("dcim/devices", "filter[id]__in")
}

// An id list that fits one request is sent as before: one request, no note.
func TestQueryShortIDFilterIsOneRequest(t *testing.T) {
	f := idDevices(50)
	res, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"name"}, Filters: []provider.Filter{{Field: "id", Value: idValues(1, 10)}}, Limit: 100})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := resultIDs(t, res); !slices.Equal(got, span(1, 10)) || res.Total != 10 {
		t.Errorf("ids %v, total %d", got, res.Total)
	}
	if n := len(idReads(f)); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// Past the budget the ids go in batches, each carrying every other parameter,
// and the rows come back once each in the replica's own order (by id), as one
// read would return them, so there is no note to make: the ids are sorted
// before they are split, whatever order they were written in.
func TestQueryLongIDFilterIsBatched(t *testing.T) {
	f := idDevices(5000)
	ids := strings.Split(idValues(1, 5000), ",")
	slices.Reverse(ids)
	res, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"name"}, Limit: MaxLimit,
		Filters: []provider.Filter{{Field: "id", Value: strings.Join(ids, ",")}, {Field: "status", Value: "active"}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := resultIDs(t, res); !slices.Equal(got, span(1, 5000)) || res.Total != 5000 {
		t.Errorf("rows %d (first %v), total %d; want 1..5000 once each, in order", len(got), got[:min(3, len(got))], res.Total)
	}
	reads := idReads(f)
	if len(reads) < 2 {
		t.Fatalf("%d requests, want several batches", len(reads))
	}
	for _, r := range reads {
		if r.query.Get("filter[status]__eq") != "active" || r.query.Get("fields") == "" {
			t.Errorf("a batch lost the other parameters: %v", r.query)
		}
	}
	for _, n := range res.Notes {
		if strings.Contains(n, "batch") {
			t.Errorf("an unsorted batched read is in the replica's order, yet notes %q", n)
		}
	}
}

// Past the Limit the remaining batches are read for their count only; the rows
// are the first Limit by id, and the Total stays the full sum.
func TestQueryBatchedLimitKeepsTheTotal(t *testing.T) {
	f := idDevices(MaxLimit)
	res, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"name"}, Filters: []provider.Filter{{Field: "id", Value: idValues(1, MaxLimit)}}, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := resultIDs(t, res); !slices.Equal(got, span(1, 1000)) || res.Total != MaxLimit {
		t.Errorf("rows %d, total %d; want the first 1000 and %d", len(got), res.Total, MaxLimit)
	}
	counted := 0
	for _, r := range idReads(f) {
		if r.query.Get("limit") == "1" {
			counted++
		}
	}
	if counted == 0 {
		t.Error("no batch was read for its count only once the Limit was reached")
	}
}

// A count (an alert rule's) sums the batches' counts.
func TestQueryBatchedCount(t *testing.T) {
	res, err := newTestProvider(t, idDevices(5000)).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{{Field: "id", Value: idValues(1, 5000)}}, CountOnly: true})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 5000 {
		t.Errorf("total %d, want 5000", res.Total)
	}
}

// More ids than the ceiling are refused before any request, as in NetBox mode,
// with its error kind so the plugin says the same sentence. The count is of
// distinct ids, numbers compared as numbers.
func TestQueryRefusesMoreIDsThanTheCeiling(t *testing.T) {
	f := idDevices(12000)
	p := newTestProvider(t, f)
	for _, spec := range []provider.QuerySpec{
		{ObjectType: "dcim/devices", Filters: []provider.Filter{{Field: "id", Value: idValues(1, MaxLimit+1)}}, Limit: MaxLimit},
		{ObjectType: "dcim/devices", Filters: []provider.Filter{{Field: "id", Value: idValues(1, MaxLimit+1)}}, CountOnly: true},
	} {
		_, err := p.Query(context.Background(), spec)
		u := provider.Classify(err)
		if u == nil || u.Kind != provider.ErrorKindTooManyIDs || u.IDs != MaxLimit+1 || u.MaxIDs != MaxLimit {
			t.Fatalf("err = %v (%+v), want too many ids: %d of at most %d", err, u, MaxLimit+1, MaxLimit)
		}
	}
	if n := len(idReads(f)); n != 0 {
		t.Errorf("%d requests sent for a refused id list", n)
	}
	// The ceiling itself is read, repeats and all.
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{{Field: "id", Value: idValues(1, MaxLimit) + ",1,02,3"}}, CountOnly: true})
	if err != nil || res.Total != MaxLimit {
		t.Fatalf("err %v, total %v; want the %d ids counted", err, res, MaxLimit)
	}
}

// A negation beside a long id list is answered per batch, its total exact, as
// NetBox mode answers it.
func TestQueryBatchedWithANegation(t *testing.T) {
	f := idDevices(3000, 7, 2500, 2999)
	res, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"name"}, Limit: MaxLimit,
		Filters: []provider.Filter{{Field: "id", Value: idValues(1, 3000)}, {Field: "status", Operator: "n", Value: "offline"}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 2997 || len(res.Rows) != 2997 || slices.Contains(resultIDs(t, res), 2500) {
		t.Errorf("total %d, rows %d; want 2997 without the offline ones", res.Total, len(res.Rows))
	}
	if len(idReads(f)) < 2 {
		t.Error("the negated read was not batched")
	}
}

// A sorted batched read is sorted by the replica within each batch: the note
// says so, and which rows a cut kept.
func TestQueryBatchedSortedSaysSo(t *testing.T) {
	f := idDevices(5000)
	res, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"serial"}, Ordering: "serial", Limit: 100,
		Filters: []provider.Filter{{Field: "id", Value: idValues(1, 5000)}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 5000 || len(res.Rows) != 100 {
		t.Fatalf("total %d, rows %d", res.Total, len(res.Rows))
	}
	notes := strings.Join(res.Notes, " | ")
	if !strings.Contains(notes, "sorted by the replica within each") || !strings.Contains(notes, "Raise the Limit") {
		t.Errorf("notes %q, want the per-batch sort note", notes)
	}
}

// Other parameters that leave too little room for ids are not worked around
// with hundreds of tiny batches: the query is refused as too long, as before.
func TestQueryIDsWithoutRoomAreRefused(t *testing.T) {
	f := idDevices(3000)
	f.addColumns("dcim/devices", "vc_position:INTEGER")
	// A vc_position list that fits on its own and leaves well under 1 KB.
	_, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"name"}, Filters: []provider.Filter{{Field: "id", Value: idValues(1, 3000)}, {Field: "vc_position", Value: idValues(10000, 11950)}}})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("err = %v, want too long", err)
	}
	if n := len(idReads(f)); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// A batch answering more rows than it has ids means the id filter wasn't
// applied: the read fails rather than repeat rows.
func TestBatchTotalCheck(t *testing.T) {
	if err := checkBatchTotal("dcim/devices", 3, 10); err == nil {
		t.Error("a batch of 3 ids answering 10 rows passed")
	}
	if err := checkBatchTotal("dcim/devices", 3, 3); err != nil {
		t.Errorf("a batch answering its own ids failed: %v", err)
	}
}

// has_primary_ip is NetBox's _has_primary_ip: true is either primary address
// set, false is neither.
func TestQueryHasPrimaryIP(t *testing.T) {
	f := idDevices(6)
	for id, ips := range map[int][2]interface{}{1: {float64(10), nil}, 2: {nil, float64(20)}, 4: {float64(11), float64(21)}, 6: {nil, float64(22)}} {
		f.entities["dcim/devices"][id-1]["primary_ip4_id"], f.entities["dcim/devices"][id-1]["primary_ip6_id"] = ips[0], ips[1]
	}
	p := newTestProvider(t, f)
	for _, tc := range []struct {
		value string
		limit int
		want  []int
		total int
	}{
		{"true", 0, []int{1, 2, 4, 6}, 4},
		{"True", 2, []int{1, 2}, 4},
		{"2", 0, []int{1, 2, 4, 6}, 4},
		{"false", 0, []int{3, 5}, 2},
		{"3", 0, []int{3, 5}, 2},
	} {
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}, Limit: tc.limit,
			Filters: []provider.Filter{{Field: "has_primary_ip", Value: tc.value}}})
		if err != nil {
			t.Fatalf("%s: %v", tc.value, err)
		}
		if got := resultIDs(t, res); !slices.Equal(got, tc.want) || res.Total != tc.total {
			t.Errorf("has_primary_ip=%s: ids %v, total %d; want %v, %d", tc.value, got, res.Total, tc.want, tc.total)
		}
	}
	// Counted, and beside other filters.
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", CountOnly: true,
		Filters: []provider.Filter{{Field: "has_primary_ip", Value: "true"}}})
	if err != nil || res.Total != 4 {
		t.Errorf("count: err %v, total %v", err, res)
	}
	res, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"},
		Filters: []provider.Filter{{Field: "has_primary_ip", Value: "true"}, {Field: "id", Value: "2,3,4"}}})
	if err != nil || !slices.Equal(resultIDs(t, res), []int{2, 4}) || res.Total != 2 {
		t.Errorf("with ids: err %v, rows %v", err, res)
	}
	// A value NetBox ignores (its NullBooleanSelect reads 1 and 0 as no value)
	// is refused, as for other booleans.
	for _, v := range []string{"maybe", "1"} {
		if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
			Filters: []provider.Filter{{Field: "has_primary_ip", Value: v}}}); err == nil {
			t.Errorf("has_primary_ip=%s was answered", v)
		}
	}
	// The operator is the datasource's own: a query can't name it.
	if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{{Field: "primary_ip4_id", Operator: opEitherSet, Value: "primary_ip6_id"}}}); err == nil {
		t.Error("a query named the either-set operator")
	}
}

// The SNMP dashboard's query: devices with a primary address, read only for the
// ids its metrics carry.
func TestQueryHasPrimaryIPWithALongIDList(t *testing.T) {
	f := idDevices(4000)
	for i := range f.entities["dcim/devices"] {
		if (i+1)%2 == 0 {
			f.entities["dcim/devices"][i]["primary_ip4_id"] = float64(100000 + i)
		}
	}
	res, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"name"}, Limit: MaxLimit,
		Filters: []provider.Filter{{Field: "has_primary_ip", Value: "true"}, {Field: "id", Value: idValues(1, 4000)}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	got := resultIDs(t, res)
	if res.Total != 2000 || len(got) != 2000 || got[0] != 2 || got[1999] != 4000 || !slices.IsSorted(got) {
		t.Errorf("total %d, rows %d (%v…); want the 2000 even ids in order", res.Total, len(got), got[:min(3, len(got))])
	}
}
