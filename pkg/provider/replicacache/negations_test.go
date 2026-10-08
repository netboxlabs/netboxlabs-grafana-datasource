package replicacache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// device is a dcim/devices row with the name and status given; nil is NULL.
func device(id int, name, status interface{}) map[string]interface{} {
	d := deviceFixture(id, "", 4001)
	d["name"], d["status"] = name, status
	return d
}

// mixedDevices covers what a negation has to get right: names in either case,
// a status outside NetBox's built-in list, and a row whose name and status are
// NULL — which NetBox's exclude() keeps.
func mixedDevices() []map[string]interface{} {
	return []map[string]interface{}{
		device(1, "CORE-1", "active"),
		device(2, "core-2", "offline"),
		device(3, "EDGE-1", "planned"),
		device(4, "edge-2", "quarantined"),
		device(5, nil, nil),
		device(6, "EDGE-3", "offline"),
	}
}

func ids(res *provider.Result) []int {
	var out []int
	for _, r := range res.Rows {
		out = append(out, int(r["id"].(float64)))
	}
	return out
}

func negationQuery(t *testing.T, p *Provider, spec provider.QuerySpec) *provider.Result {
	t.Helper()
	if spec.ObjectType == "" {
		spec.ObjectType = "dcim/devices"
	}
	if spec.Ordering == "" {
		spec.Ordering = "id"
	}
	res, err := p.Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return res
}

func TestQuery_NegationsMatchNetBoxExclude(t *testing.T) {
	cases := []struct {
		name    string
		filters []provider.Filter
		want    []int
	}{
		{"not equal keeps NULL and values outside the choice list",
			[]provider.Filter{{Field: "status", Operator: "n", Value: "offline"}}, []int{1, 3, 4, 5}},
		{"not equal to several values drops each",
			[]provider.Filter{{Field: "status", Operator: "n", Value: "offline,planned"}}, []int{1, 4, 5}},
		{"two not-equal rows on one field drop both",
			[]provider.Filter{{Field: "status", Operator: "n", Value: "offline"}, {Field: "status", Operator: "n", Value: "active"}}, []int{3, 4, 5}},
		{"not contains is case-insensitive and keeps NULL",
			[]provider.Filter{{Field: "name", Operator: "nic", Value: "core"}}, []int{3, 4, 5, 6}},
		{"not contains either of two values",
			[]provider.Filter{{Field: "name", Operator: "nic", Value: "core,EDGE-3"}}, []int{3, 4, 5}},
		{"not contains values that share no text",
			[]provider.Filter{{Field: "name", Operator: "nic", Value: "core,edge"}}, []int{5}},
		{"negations on two fields",
			[]provider.Filter{{Field: "name", Operator: "nic", Value: "core"}, {Field: "status", Operator: "n", Value: "offline"}}, []int{3, 4, 5}},
		{"a negation beside a positive filter",
			[]provider.Filter{{Field: "name", Operator: "ic", Value: "edge"}, {Field: "status", Operator: "n", Value: "offline"}}, []int{3, 4}},
		{"not equal on a number",
			[]provider.Filter{{Field: "id", Operator: "n", Value: "2,4.0"}}, []int{1, 3, 5, 6}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeService()
			f.entities["dcim/devices"] = mixedDevices()
			p := newTestProvider(t, f)

			res := negationQuery(t, p, provider.QuerySpec{Filters: c.filters})
			if got := ids(res); !slices.Equal(got, c.want) {
				t.Errorf("rows = %v, want %v", got, c.want)
			}
			if res.Total != len(c.want) {
				t.Errorf("Total = %d, want %d", res.Total, len(c.want))
			}
			// The alert path reads the total alone and must agree.
			count := negationQuery(t, p, provider.QuerySpec{Filters: c.filters, CountOnly: true})
			if count.Total != len(c.want) {
				t.Errorf("CountOnly Total = %d, want %d", count.Total, len(c.want))
			}
		})
	}
}

// The anchored text negations, on a replica whose catalogue has the anchored
// matches (DATA-408).
func TestQuery_AnchoredTextNegations(t *testing.T) {
	cases := []struct {
		op, value string
		want      []int
	}{
		{"nisw", "edge", []int{1, 2, 5}},
		{"niew", "-1", []int{2, 4, 5, 6}},
		{"nie", "CORE-2", []int{1, 3, 4, 5, 6}},
		{"nisw", "ed,EDGE-", []int{1, 2, 5}},
	}
	for _, c := range cases {
		t.Run(c.op+" "+c.value, func(t *testing.T) {
			f := newFakeService()
			f.schema = withAnchoredText(devicesSchema())
			f.entities["dcim/devices"] = mixedDevices()
			p := newTestProvider(t, f)
			res := negationQuery(t, p, provider.QuerySpec{Filters: []provider.Filter{{Field: "name", Operator: c.op, Value: c.value}}})
			if got := ids(res); !slices.Equal(got, c.want) || res.Total != len(c.want) {
				t.Errorf("rows = %v (Total %d), want %v", got, res.Total, c.want)
			}
		})
	}
}

// A negation on a field that also has an equality filter is a smaller
// equality: status in (active, planned) and not active is status = planned,
// sent as such, with no walk.
func TestQuery_NegationSubtractsFromAnEqualityOnTheSameField(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Filters: []provider.Filter{
		{Field: "status", Value: "active,planned"}, {Field: "status", Operator: "n", Value: "active"}}})
	if got := ids(res); !slices.Equal(got, []int{3}) || res.Total != 1 {
		t.Errorf("rows = %v (Total %d), want [3]", got, res.Total)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request")
	}
	for _, r := range f.requests {
		if r.entity == "dcim/devices" && r.query.Get("filter[status]__eq") != "planned" {
			t.Errorf("sent %v, want only filter[status]__eq=planned", r.query)
		}
	}
}

// Subtracting every value leaves nothing to ask for. The answer is empty and
// sends nothing, but still says how fresh it is: a max-data-age rule fails a
// result with no instant.
func TestQuery_NegationThatExcludesTheWholeEqualityIsEmpty(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)
	f.mu.Lock()
	before := len(f.requests)
	f.mu.Unlock()

	res := negationQuery(t, p, provider.QuerySpec{Filters: []provider.Filter{
		{Field: "status", Value: "active"}, {Field: "status", Operator: "n", Value: "active"}}})
	if len(res.Rows) != 0 || res.Total != 0 {
		t.Errorf("rows = %v (Total %d), want none", ids(res), res.Total)
	}
	if res.DataAsOf == nil {
		t.Error("DataAsOf is nil; the catalogue's instant should stand in")
	}
	if n := f.countRequestsFor("dcim/devices"); n != 0 {
		t.Errorf("%d row requests sent for an empty answer (%d requests before)", n, before)
	}
}

// When every row's value is in NetBox's list, "not offline" is the rest of the
// list, which the replica can filter, sort and page itself. The row counts
// prove it before the answer is used: values outside the list or NULLs would
// make them disagree (TestQuery_NegationsMatchNetBoxExclude covers that).
//
// A table whose first page answers the query is read instead (see
// TestQuery_NegationOnASmallTableReadsOnce); this one's first page is all
// offline, so reading on would cost a page per thousand rows dropped.
func TestQuery_NegatedChoiceIsSentAsTheRestOfTheList(t *testing.T) {
	f := newFakeService()
	var rows []map[string]interface{}
	for i := 1; i <= pageSize; i++ {
		rows = append(rows, device(i, fmt.Sprintf("off-%d", i), "offline"))
	}
	rows = append(rows, device(pageSize+1, "CORE-1", "active"), device(pageSize+2, "EDGE-1", "planned"), device(pageSize+3, "edge-2", "active"))
	f.entities["dcim/devices"] = rows
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Limit: 2, Filters: []provider.Filter{{Field: "status", Operator: "n", Value: "offline"}}})
	if got := ids(res); !slices.Equal(got, []int{pageSize + 1, pageSize + 2}) || res.Total != 3 {
		t.Errorf("rows = %v (Total %d), want the first two of 3", got, res.Total)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var main *recordedRequest
	for i, r := range f.requests {
		if r.entity == "dcim/devices" && r.query.Get("limit") == "2" {
			main = &f.requests[i]
		}
	}
	if main == nil {
		t.Fatalf("no request asked the replica for the page itself: %v", f.requests)
	}
	in := strings.Split(main.query.Get("filter[status]__in"), ",")
	if !slices.Contains(in, "active") || !slices.Contains(in, "planned") || slices.Contains(in, "offline") {
		t.Errorf("filter[status]__in = %v, want the list without offline", in)
	}
}

// A negation the replica cannot express is applied to the rows as they are
// read. Each request asks for a full page: sizing it by the rows still wanted
// would fetch one row at a time once a page's worth were nearly kept.
func TestQuery_NegationWalkReadsFullPages(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)

	negationQuery(t, p, provider.QuerySpec{Limit: 2, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "core"}}})
	f.mu.Lock()
	defer f.mu.Unlock()
	walked := false
	for _, r := range f.requests {
		if r.entity != "dcim/devices" || r.query.Get("filter[name]__ilike") != "" {
			continue // a count
		}
		if l := r.query.Get("limit"); l != "1" && l != "1000" {
			t.Errorf("a walk page asked for %s rows, want a full page", l)
		}
		walked = walked || r.query.Get("limit") == "1000"
	}
	if !walked {
		t.Errorf("no full page was read: %v", f.requests)
	}
}

// The alert Count reads the total alone. It comes from row counts, never from
// reading the rows.
func TestQuery_NegationCountOnlyReadsNoRows(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "core"}}})
	if res.Total != 4 {
		t.Errorf("Total = %d, want 4", res.Total)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.entity == "dcim/devices" && r.query.Get("limit") != "1" {
			t.Errorf("a count-only query read rows: %v", r.query)
		}
	}
}

// A field fetched only to test a negation is not shown.
func TestQuery_NegatedFieldIsFetchedButNotShown(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Fields: []string{"name"}, Filters: []provider.Filter{{Field: "status", Operator: "n", Value: "offline"}}})
	if got := ids(res); !slices.Equal(got, []int{1, 3, 4, 5}) {
		t.Errorf("rows = %v, want [1 3 4 5]", got)
	}
	if !slices.Equal(res.Columns, []string{"name"}) {
		t.Errorf("Columns = %v, want [name]", res.Columns)
	}
}

// bigDevices is more rows than one query may read (MaxLimit), all named so
// that every one contains "de" and "ev" but no text holds both values as one.
func bigDevices() []map[string]interface{} {
	rows := make([]map[string]interface{}, 0, MaxLimit+1)
	for i := 1; i <= MaxLimit+1; i++ {
		rows = append(rows, device(i, fmt.Sprintf("dev-%05d", i), "active"))
	}
	return rows
}

// Two text negations on one field whose values neither contain the other have
// no count the replica can take (it cannot AND two ilike on one column), so
// the total comes from reading every row — possible only up to MaxLimit rows.
// Past it, a caller that needs the total is refused with what to do; a panel,
// which can do without, gets the rows read so far and a warning when the read
// stopped short.
func TestQuery_UncountableNegationOnALargeTable(t *testing.T) {
	filters := []provider.Filter{{Field: "name", Operator: "nic", Value: "de,ev"}}

	f := newFakeService()
	f.entities["dcim/devices"] = bigDevices()
	p := newTestProvider(t, f)

	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: filters})
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Reason, "10,000") {
		t.Fatalf("err = %v, want a refusal naming the 10,000-row limit", err)
	}

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: filters, AllowUncounted: true})
	if err != nil {
		t.Fatalf("Query with AllowUncounted: %v", err)
	}
	if len(res.Rows) != 0 || res.Total != 0 {
		t.Errorf("rows = %d (Total %d), want none, uncounted", len(res.Rows), res.Total)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "10,000") {
		t.Errorf("Warnings = %v, want one saying the read stopped at 10,000 rows", res.Warnings)
	}
}

// Row counts taken while the replica applies changes do not add up to one
// answer. The counts are retried once; if the data still moved, the total
// carries a warning, which fails an alert evaluation rather than letting it
// act on a number from no single moment.
func TestQuery_NegationCountsFromDifferentMomentsWarn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)
	for i := 0; i < 20; i++ {
		f.asOfs = append(f.asOfs, fmt.Sprintf("2026-09-22T14:%02d:00Z", i))
	}

	res := negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "core"}}})
	if !strings.Contains(strings.Join(res.Warnings, " "), "changes") {
		t.Errorf("Warnings = %v, want one saying the counts moved", res.Warnings)
	}

	f.mu.Lock()
	f.asOfs = nil
	f.mu.Unlock()
	res = negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "core"}}})
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v for counts from one moment", res.Warnings)
	}
}

func TestQuery_NegationRefusals(t *testing.T) {
	cases := []struct {
		name       string
		objectType string
		filter     provider.Filter
		reason     string
	}{
		// NetBox matches an address by host, whatever the mask; the rows here
		// hold PostgreSQL's text, so "not this host" cannot be tested on them
		// the way NetBox tests it.
		{"not equal on an IP address", "ipam/ip-addresses", provider.Filter{Field: "address", Operator: "n", Value: "10.0.0.1"}, "IP address"},
		{"a label as the value", "dcim/devices", provider.Filter{Field: "status", Operator: "n", Value: "Active"}, `"active"`},
		{"a text negation on a number", "dcim/devices", provider.Filter{Field: "id", Operator: "nic", Value: "1"}, "does not take that operator"},
		{"an entity with no data yet", "dcim/platforms", provider.Filter{Field: "name", Operator: "n", Value: "x"}, "received data"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeService()
			f.addAddressEntity(true)
			f.entities["dcim/devices"] = mixedDevices()
			p := newTestProvider(t, f)
			_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: c.objectType, Filters: []provider.Filter{c.filter}})
			var unsupported *UnsupportedFilterError
			if !errors.As(err, &unsupported) {
				t.Fatalf("err = %v, want an UnsupportedFilterError", err)
			}
			if !strings.Contains(unsupported.Reason, c.reason) {
				t.Errorf("reason = %q, want it to say %q", unsupported.Reason, c.reason)
			}
		})
	}
}

// The editor offers each negation where its positive is offered, except "not
// equal" on an IP address column.
func TestFilterFields_OfferNegationsBesideTheirPositives(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(devicesSchema())
	f.addAddressEntity(true)
	p := newTestProvider(t, f)

	ops := func(objectType string) map[string][]string {
		ff, err := p.FilterFields(context.Background(), objectType)
		if err != nil {
			t.Fatalf("FilterFields: %v", err)
		}
		out := map[string][]string{}
		for _, x := range ff {
			out[x.Name] = x.Operators
		}
		return out
	}
	devices := ops("dcim/devices")
	for _, op := range []string{"n", "nic", "nisw", "niew", "nie"} {
		if !slices.Contains(devices["name"], op) {
			t.Errorf("name operators = %v, want %q", devices["name"], op)
		}
	}
	if !slices.Contains(devices["id"], "n") || slices.Contains(devices["id"], "nic") {
		t.Errorf("id operators = %v, want n and no text negation", devices["id"])
	}
	address := ops("ipam/ip-addresses")["address"]
	if slices.Contains(address, "n") {
		t.Errorf("address operators = %v, want no n", address)
	}
}

// A negated related name (site) is tested on the name the replica joins in,
// which comes with expand= whatever the projection lists.
func TestQuery_NegatedRelatedName(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-1", "slug": "dc-1"},
		{"id": float64(4002), "name": "DC-2", "slug": "dc-2"},
	}
	other := deviceFixture(2, "CORE-2", 4002)
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001), other, device(3, "EDGE-1", "active")}
	f.entities["dcim/devices"][2]["site_id"] = nil
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Fields: []string{"name"}, Filters: []provider.Filter{{Field: "site", Operator: "n", Value: "DC-1"}}})
	if got := ids(res); !slices.Equal(got, []int{2, 3}) || res.Total != 2 {
		t.Errorf("rows = %v (Total %d), want [2 3]: DC-2 and the device with no site", got, res.Total)
	}
	if !slices.Equal(res.Columns, []string{"name"}) {
		t.Errorf("Columns = %v, want [name]", res.Columns)
	}
}

// The rows are tested here and the counts by the replica, so a negation is
// offered only where the two compare alike: text, numbers and booleans. A
// timestamp the replica casts before comparing is not one.
func TestQuery_NegationOnlyWhereTheRowTestComparesLikeTheReplica(t *testing.T) {
	f := newFakeService()
	f.addEntity("dcim/cables", "id:BIGINT:pk", "label:VARCHAR", "seen:TIMESTAMP WITH TIME ZONE")
	f.entities["dcim/cables"] = []map[string]interface{}{{"id": 1, "label": "a", "seen": "2026-05-01T10:00:00+00:00"}}
	p := newTestProvider(t, f)

	ff, err := p.FilterFields(context.Background(), "dcim/cables")
	if err != nil {
		t.Fatalf("FilterFields: %v", err)
	}
	for _, x := range ff {
		if x.Name == "seen" && slices.Contains(x.Operators, "n") {
			t.Errorf("seen operators = %v, want no n on a timestamp", x.Operators)
		}
		if x.Name == "label" && !slices.Contains(x.Operators, "n") {
			t.Errorf("label operators = %v, want n", x.Operators)
		}
	}
	_, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/cables",
		Filters: []provider.Filter{{Field: "seen", Operator: "n", Value: "2026-05-01T10:00:00Z"}}})
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// A filter row with no field sends nothing in either mode; a negation must not
// become a predicate on "", which would count every row as excluded.
func TestQuery_NegationWithNoFieldIsIgnored(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: []provider.Filter{{Operator: "n", Value: "x"}}})
	if res.Total != 6 {
		t.Errorf("Total = %d, want 6", res.Total)
	}
}

// A small table is answered by reading it: once the read reaches the end, the
// rows kept are the total, and no row counts are needed.
func TestQuery_NegationOnASmallTableReadsOnce(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = mixedDevices()
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "core"}}})
	if res.Total != 4 {
		t.Errorf("Total = %d, want 4", res.Total)
	}
	if n := f.countRequestsFor("dcim/devices"); n != 1 {
		t.Errorf("%d requests, want the one read", n)
	}
}

// Read to the end is read to the end even when the replica hands out a cursor
// with its last page: a strict query whose other filters match exactly 10,000
// rows is answered, not refused as having grown.
func TestQuery_UncountableNegationAtExactlyTheReadLimit(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = bigDevices()[:MaxLimit]
	f.cursorOnLastPage = true
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "de,ev"}}})
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0", res.Total)
	}
}

// The replica folds case for the counts and this code for the rows. Outside
// ASCII the two need not agree, so such a negation is counted by reading the
// rows, never by mixing the two.
func TestQuery_NonASCIITextNegationIsCountedFromTheRows(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{device(1, "São Paulo", "active"), device(2, "Lisboa", "active")}
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{CountOnly: true, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "são"}}})
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1", res.Total)
	}
	if _, ok := f.requestWith("dcim/devices", "filter[name]__ilike"); ok {
		t.Error("the replica was asked to count a non-ASCII text match")
	}
}

// A read cut short at 10,000 rows warns that more may match — unless the
// counts show every match was already found.
func TestQuery_NoReadLimitWarningWhenTheCountsShowNothingWasMissed(t *testing.T) {
	f := newFakeService()
	rows := bigDevices()
	for i := 0; i < 5; i++ {
		rows[i]["name"] = fmt.Sprintf("other-%d", i)
	}
	f.entities["dcim/devices"] = append(rows, device(MaxLimit+2, "dev-x", "active"))
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Limit: 1000, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "dev"}}})
	if len(res.Rows) != 5 || res.Total != 5 {
		t.Errorf("rows = %d (Total %d), want 5 of 5", len(res.Rows), res.Total)
	}
	if strings.Contains(strings.Join(res.Warnings, " "), "Only the first") {
		t.Errorf("Warnings = %v, want no read-limit warning: the counts show all 5 were found", res.Warnings)
	}
}

// A blank value in another text filter sends nothing (buildFilterValues drops
// it), so it must not count as a constraint in the totals either: here it made
// "is foo" and "is ”" look contradictory, and the foo rows were never
// subtracted.
func TestQuery_NegationTotalIgnoresABlankTextFilter(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(devicesSchema())
	f.entities["dcim/devices"] = append(bigDevices(), device(MaxLimit+2, "foo", "active"), device(MaxLimit+3, "FOO", "active"))
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Limit: 10, Filters: []provider.Filter{
		{Field: "name", Operator: "ie", Value: " "}, {Field: "name", Operator: "nie", Value: "foo"}}})
	if res.Total != MaxLimit+1 {
		t.Errorf("Total = %d, want %d", res.Total, MaxLimit+1)
	}
}

// Count requests read nothing but the count: no joins, one narrow row.
func TestQuery_NegationCountsReadNoColumns(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = bigDevices()
	p := newTestProvider(t, f)

	negationQuery(t, p, provider.QuerySpec{Limit: 5, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "zz"}}})
	f.mu.Lock()
	defer f.mu.Unlock()
	counted := false
	for _, r := range f.requests {
		if r.entity != "dcim/devices" || r.query.Get("limit") != "1" {
			continue
		}
		counted = true
		if r.query.Get("expand") != "" || r.query.Get("fields") != "id" {
			t.Errorf("a count asked for %v, want fields=id and no expand", r.query)
		}
	}
	if !counted {
		t.Error("no count was taken")
	}
}

// The rows and the total read at different moments warn as counts from
// different moments do.
func TestQuery_NegationRowsAndTotalFromDifferentMomentsWarn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = bigDevices()
	p := newTestProvider(t, f)
	f.asOfs = []string{"2026-09-22T14:00:00Z", "2026-09-22T14:01:00Z", "2026-09-22T14:01:00Z"}

	res := negationQuery(t, p, provider.QuerySpec{Limit: 5, Filters: []provider.Filter{{Field: "name", Operator: "nic", Value: "zz"}}})
	if !strings.Contains(strings.Join(res.Warnings, " "), "changes") {
		t.Errorf("Warnings = %v, want one saying the data moved", res.Warnings)
	}
}

// The rows are decoded with their numbers intact: a 64-bit value float64
// rounds would be kept by the row test while the replica's count drops it.
func TestQuery_NegationComparesLargeIntegersExactly(t *testing.T) {
	f := newFakeService()
	big := device(1, "A", "active")
	big["tenant_id"] = int64(9007199254740993)
	f.entities["dcim/devices"] = []map[string]interface{}{big, device(2, "B", "active")}
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Filters: []provider.Filter{{Field: "tenant_id", Operator: "n", Value: "9007199254740993"}}})
	if got := ids(res); !slices.Equal(got, []int{2}) {
		t.Errorf("rows = %v, want [2]", got)
	}
}

// The same text negation twice is one predicate, not two of the three the
// counts can take.
func TestQuery_RepeatedTextNegationsCountOnce(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(devicesSchema())
	f.entities["dcim/devices"] = bigDevices()
	p := newTestProvider(t, f)

	res := negationQuery(t, p, provider.QuerySpec{Limit: 5, Filters: []provider.Filter{
		{Field: "name", Operator: "nic", Value: "zz"}, {Field: "name", Operator: "nic", Value: "ZZ"},
		{Field: "name", Operator: "nisw", Value: "yy,yy"}}})
	if res.Total != MaxLimit+1 {
		t.Errorf("Total = %d, want %d", res.Total, MaxLimit+1)
	}
}
