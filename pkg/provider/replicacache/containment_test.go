package replicacache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// containmentRows are the prefixes these tests search, by id, as v1.40.1 holds
// them: PostgreSQL's cidr text, the mask always written, a single host's /32
// and /128 included.
//
//	 1 0.0.0.0/0        2 10.0.0.0/8      3 10.0.0.0/24      4 10.0.0.1/32     5 10.0.1.0/24
//	 6 192.168.0.0/16   7 10.9.0.0/16 (VRF 1)                8 10.9.0.0/16 (no VRF)
//	11 ::/0            12 2001:db8::/32   13 2001:db8:1::/48 14 2001:db8::1/128
//	15 ::ffff:0.0.0.0/96 (the IPv4-mapped block)             16 ::ffff:10.0.0.0/104
func containmentRows() []map[string]interface{} {
	row := func(id int, prefix, status string, vrf interface{}) map[string]interface{} {
		return map[string]interface{}{"id": float64(id), "prefix": prefix, "status": status, "vrf_id": vrf}
	}
	return []map[string]interface{}{
		row(1, "0.0.0.0/0", "container", nil), row(2, "10.0.0.0/8", "container", nil), row(3, "10.0.0.0/24", "active", nil),
		row(4, "10.0.0.1/32", "reserved", nil), row(5, "10.0.1.0/24", "active", nil), row(6, "192.168.0.0/16", "active", nil),
		row(7, "10.9.0.0/16", "active", float64(1)), row(8, "10.9.0.0/16", "active", nil),
		row(11, "::/0", "container", nil), row(12, "2001:db8::/32", "container", nil), row(13, "2001:db8:1::/48", "active", nil),
		row(14, "2001:db8::1/128", "reserved", nil), row(15, "::ffff:0.0.0.0/96", "container", nil), row(16, "::ffff:10.0.0.0/104", "active", nil),
	}
}

func containmentProvider(t *testing.T) (*fakeService, *Provider) {
	t.Helper()
	f := newFakeService()
	f.addPrefixes(containmentRows()...)
	return f, newTestProvider(t, f)
}

// containing is the ids of the prefixes NetBox's contains filter finds for a
// value, as a query on the filter names them.
func containing(t *testing.T, p *Provider, value string) []int {
	t.Helper()
	return relatedRows(t, p, "ipam/prefixes", provider.Filter{Field: "contains", Value: value})
}

// sentContains is the value the one request that carried it sent the replica.
func sentContains(t *testing.T, f *fakeService) string {
	t.Helper()
	reqs := f.sentWith("ipam/prefixes", "filter[prefix]__contains")
	if len(reqs) != 1 {
		t.Fatalf("%d requests carried filter[prefix]__contains, want one", len(reqs))
	}
	return reqs[0].query.Get("filter[prefix]__contains")
}

func wantContaining(t *testing.T, p *Provider, want map[string][]int) {
	t.Helper()
	for value, ids := range want {
		if got := containing(t, p, value); !slices.Equal(got, ids) {
			t.Errorf("contains=%s: rows %v, want %v", value, got, ids)
		}
	}
}

// A bare address is contained strictly: the /32 that IS 10.0.0.1 is not a prefix
// that contains it (NetBox's search_contains filters a value without a mask by
// prefix__net_contains, PostgreSQL's >>). The replica's own operator reads a
// value written without a mask so, and the address is sent bare.
func TestContains_BareAddressIsStrict(t *testing.T) {
	f, p := containmentProvider(t)
	wantContaining(t, p, map[string][]int{
		"10.0.0.1": {1, 2, 3}, // not id 4, the host itself
		// A network address is inside its own prefix as a host.
		"10.0.1.0": {1, 2, 5},
		// Prefixes of other networks hold neither.
		"192.168.7.7": {1, 6},
		"172.16.0.1":  {1},
	})
	f.forget()
	containing(t, p, "10.0.0.1")
	if got := sentContains(t, f); got != "10.0.0.1" {
		t.Errorf("sent filter[prefix]__contains=%s, want the bare address", got)
	}
	// And only that filter: the value was never compared as equality.
	if reqs := f.sentWith("ipam/prefixes", "filter[prefix]__eq"); len(reqs) != 0 {
		t.Errorf("a contains was also sent as equality: %v", reqs[0].query)
	}
}

// A value with a mask is a prefix, and containment includes equality: the /24
// that IS 10.0.0.0/24 is found beside the networks around it, and a mask on a
// single host (/32) finds the /32 that is the host, which the bare address
// excludes. Its host bits are ignored, and so is the VRF, of which NetBox's
// filter has no notion.
func TestContains_MaskedIncludesEqual(t *testing.T) {
	f, p := containmentProvider(t)
	wantContaining(t, p, map[string][]int{
		"10.0.0.0/24": {1, 2, 3},    // id 3 is the prefix itself
		"10.0.0.1/32": {1, 2, 3, 4}, // id 4 is the host itself
		"10.0.0.5/24": {1, 2, 3},    // host bits ignored: this is 10.0.0.0/24
		"10.0.0.0/23": {1, 2},       // a /23 is not inside the /24
		"10.0.0.0/8":  {1, 2},
		"0.0.0.0/0":   {1},
		"10.9.1.1":    {1, 2, 7, 8}, // both /16s, whatever their VRF
		"10.9.0.0/16": {1, 2, 7, 8},
	})
	f.forget()
	containing(t, p, "10.0.0.5/24")
	if got := sentContains(t, f); got != "10.0.0.0/24" {
		t.Errorf("sent filter[prefix]__contains=%s, want the network with its host bits zeroed", got)
	}
	f.forget()
	containing(t, p, "10.0.0.1/32")
	if got := sentContains(t, f); got != "10.0.0.1/32" {
		t.Errorf("sent filter[prefix]__contains=%s, want the host with its mask: a mask is what asks for equality", got)
	}
}

// IPv6 reads as IPv4 does, in any spelling: the address is sent as Go writes
// it, and the families never contain each other.
func TestContains_IPv6(t *testing.T) {
	f, p := containmentProvider(t)
	wantContaining(t, p, map[string][]int{
		"2001:db8::1":             {11, 12},     // not id 14, the host itself
		"2001:DB8:0:0::1":         {11, 12},     // the same address
		"2001:db8::1/128":         {11, 12, 14}, // a mask: the host itself too
		"2001:db8:1::/48":         {11, 12, 13},
		"2001:db8:1::5/48":        {11, 12, 13}, // host bits ignored
		"2001:db8::/32":           {11, 12},
		"::1":                     {11},
		"2001:db8::1/ffff:ffff::": {11, 12}, // a netmask, as NetBox reads one
	})
	f.forget()
	containing(t, p, "2001:DB8:0:0::1")
	if got := sentContains(t, f); got != "2001:db8::1" {
		t.Errorf("sent filter[prefix]__contains=%s, want the address as Go writes it", got)
	}
	f.forget()
	containing(t, p, "2001:db8:1::5/48")
	if got := sentContains(t, f); got != "2001:db8:1::/48" {
		t.Errorf("sent filter[prefix]__contains=%s, want the network", got)
	}
}

// NetBox reads a value with netaddr, which takes what the replica's Go parser
// does not: a netmask or hostmask in place of a length, and a length written
// 024, +24 or " 24" (NetBox 4.4.10: ?contains=10.10.10.5/255.255.255.0 finds the
// /8 and the /24, where the replica answers 400 for it). The value is read as
// NetBox reads it and sent as the network it is.
func TestContains_NetmaskNotation(t *testing.T) {
	f, p := containmentProvider(t)
	for _, in := range []string{
		"10.0.0.5/255.255.255.0", "10.0.0.5/0.0.0.255", "10.0.0.5/024", "10.0.0.5/+24", "10.0.0.5/ 24", "10.0.0.5/2_4",
	} {
		if got := containing(t, p, in); !slices.Equal(got, []int{1, 2, 3}) {
			t.Errorf("contains=%s: rows %v, want what 10.0.0.0/24 finds: [1 2 3]", in, got)
		}
		f.forget()
		containing(t, p, in)
		if got := sentContains(t, f); got != "10.0.0.0/24" {
			t.Errorf("contains=%s: sent %q, want 10.0.0.0/24", in, got)
		}
	}
	if got := containing(t, p, "10.0.0.1/255.255.255.255"); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Errorf("a netmask of /32 is a mask: rows %v, want [1 2 3 4]", got)
	}
}

// An IPv4-mapped IPv6 value is IPv6 — NetBox keeps its family, and so does the
// replica's operator — so the IPv4 prefixes around 10.0.0.1 do not contain
// ::ffff:10.0.0.1, and the IPv6 ones that do are found. The value is sent as
// IPv6, never unmapped.
func TestContains_IPv4MappedStaysIPv6(t *testing.T) {
	f, p := containmentProvider(t)
	wantContaining(t, p, map[string][]int{
		"::ffff:10.0.0.1":     {11, 15, 16},
		"::ffff:a00:1":        {11, 15, 16}, // the same address written in hex
		"::ffff:10.0.0.1/120": {11, 15, 16},
		"::ffff:10.0.0.0/104": {11, 15, 16}, // id 16 is the prefix itself
	})
	f.forget()
	containing(t, p, "::ffff:a00:1")
	if got := sentContains(t, f); got != "::ffff:10.0.0.1" {
		t.Errorf("sent filter[prefix]__contains=%s, want the IPv4-mapped address, as IPv6", got)
	}
	f.forget()
	containing(t, p, "::ffff:10.0.0.5/120")
	if got := sentContains(t, f); got != "::ffff:10.0.0.0/120" {
		t.Errorf("sent filter[prefix]__contains=%s, want the IPv4-mapped network, as IPv6", got)
	}
}

// The filter takes one value. NetBox's CharFilter reads the last of several
// (?contains=10.10.10.5&contains=10.20.20.5 finds what the second finds), which
// would narrow a multi-value variable to one selection without a word, so
// several are refused, whether one row holds them or several rows do.
func TestContains_SeveralValuesRefused(t *testing.T) {
	f, p := containmentProvider(t)
	for name, filters := range map[string][]provider.Filter{
		"in one row":     {{Field: "contains", Value: "10.0.0.1,10.0.1.1"}},
		"in several":     {{Field: "contains", Value: "10.0.0.1"}, {Field: "contains", Value: "10.0.1.1"}},
		"the same twice": {{Field: "contains", Value: "10.0.0.1"}, {Field: "contains", Value: "10.0.0.1"}},
		"three":          {{Field: "contains", Value: "10.0.0.1, 10.0.1.1"}, {Field: "contains", Operator: "exact", Value: "::1"}},
	} {
		u := relatedRefusal(t, p, "ipam/prefixes", filters...)
		want := "NetBox's contains filter takes one address or prefix"
		if !strings.HasPrefix(u.Reason, want) || !strings.HasSuffix(u.Reason, "select a single value") {
			t.Errorf("%s: reason %q, want it to begin %q and say to select a single value", name, u.Reason, want)
		}
		if u.Field != "contains" || u.Operator != "" {
			t.Errorf("%s: refusal names %s %q, want the filter as written", name, u.Field, u.Operator)
		}
	}
	if !strings.Contains(relatedRefusal(t, p, "ipam/prefixes", provider.Filter{Field: "contains", Value: "10.0.0.1,10.0.1.1"}).Reason, "the 2 values given") {
		t.Error("the refusal does not say how many values were given")
	}
	noRequestsFor(t, f, "ipam/prefixes")

	// A trailing comma is not a second value: it is one, as everywhere.
	if got := containing(t, p, "10.0.0.1,"); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("contains=10.0.0.1, : rows %v, want [1 2 3]", got)
	}
}

// A value that is no address or prefix matches no row, as NetBox's does
// (queryset.none(), not a 400) — and needs no request, which the replica would
// refuse whole for it. Every one of these answered an empty 200 on NetBox
// 4.4.10, and a 400 from the replica.
func TestContains_InvalidMatchesNothingWithoutARequest(t *testing.T) {
	f, p := containmentProvider(t)
	for _, in := range []string{
		"abc", "10.0.0.256", "10.0.0.1/33", "10.0.0.1/abc", "10.0.0.1/", "/24", "fe80::1%eth0", "10", "10.1",
		"010.0.0.1", "10.0.0.1/255.0.255.0", "2001:db8::/129", "2001:db8::1/255.255.255.0", "１０.0.0.1", "10.0.0.1/24/8",
	} {
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes",
			Filters: []provider.Filter{{Field: "contains", Value: in}}})
		if err != nil {
			t.Errorf("contains=%s: %v, want no rows and no error", in, err)
		} else if len(res.Rows) != 0 || res.Total != 0 {
			t.Errorf("contains=%s: %d rows, total %d; want none", in, len(res.Rows), res.Total)
		}
		count, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", CountOnly: true,
			Filters: []provider.Filter{{Field: "contains", Value: in}}})
		if err != nil {
			t.Errorf("contains=%s: a count: %v, want 0", in, err)
		} else if count.Total != 0 {
			t.Errorf("contains=%s: a count is %d, want 0", in, count.Total)
		}
	}
	noRequestsFor(t, f, "ipam/prefixes")
}

// NetBox strips its value with str.strip(), which removes the ASCII separators
// \x1c-\x1f beside the whitespace every Go string trim knows (NetBox 4.4.10:
// ?contains=%1c10.10.10.5 finds what 10.10.10.5 finds), and filters nothing when
// nothing is left of it. A blank value filters nothing everywhere, this one
// included.
func TestContains_BlankValueFiltersNothing(t *testing.T) {
	f, p := containmentProvider(t)
	if got := containing(t, p, "\x1c10.0.0.1\x1f"); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("a value in separators: rows %v, want what 10.0.0.1 finds", got)
	}
	f.forget()
	containing(t, p, " \x1c10.0.0.5/24 ")
	if got := sentContains(t, f); got != "10.0.0.0/24" {
		t.Errorf("a masked value in separators: sent %q, want 10.0.0.0/24", got)
	}
	all := len(containmentRows())
	for _, blank := range []string{"", " ", "\x1c", " \x1f "} {
		f.forget()
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes",
			Filters: []provider.Filter{{Field: "contains", Value: blank}}})
		if err != nil {
			t.Errorf("contains=%q: %v, want every row", blank, err)
		} else if len(res.Rows) != all {
			t.Errorf("contains=%q: %d rows, want every row (%d), as NetBox's blank value", blank, len(res.Rows), all)
		}
		if reqs := f.sentWith("ipam/prefixes", "filter[prefix]__contains"); len(reqs) != 0 {
			t.Errorf("contains=%q was sent: %v", blank, reqs[0].query)
		}
	}
}

// pythonStrip removes exactly what Python 3.12's str.strip() does: of every
// code point, these 29 (measured in NetBox 4.4.10's interpreter), which are
// Go's unicode.IsSpace set and the four ASCII separators beside it.
func TestPythonStrip(t *testing.T) {
	stripped := []rune{0x9, 0xa, 0xb, 0xc, 0xd, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x85, 0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005,
		0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000}
	for r := rune(0); r <= 0x3100; r++ {
		got := pythonStrip(string(r)+"x"+string(r)) == "x"
		if want := slices.Contains(stripped, r); got != want {
			t.Errorf("U+%04X: stripped %v, want %v", r, got, want)
		}
	}
	if got := pythonStrip("\U00003000 \x1c10.0.0.1\x1f \U00002028"); got != "10.0.0.1" {
		t.Errorf("pythonStrip = %q", got)
	}
	if got := pythonStrip(" 10.0.0.1 /24"); got != "10.0.0.1 /24" {
		t.Errorf("the inside of a value is left alone: %q", got)
	}
}

// NetBox's form refuses a null character in a text value, and answers the
// request with its 400 rather than with no rows (NetBox 4.4.10:
// ?contains=10.10.10.5%00 is HTTP 400 {"contains": ["Null characters are not
// allowed."]}; the check follows the strip, so a null between spaces is refused
// too). It is no address, which would match nothing, but NetBox does not say
// that, and so a dashboard or an alert rule gets the error NetBox mode gets,
// with NetBox's words.
func TestContains_NullCharacterIsRefusedAsNetBoxRefusesIt(t *testing.T) {
	f, p := containmentProvider(t)
	want := "Null characters are not allowed. NetBox mode answers the same; remove the null character from the value"
	for _, in := range []string{"10.0.0.1\x00", "\x00", "10.0.\x000.1", "10.0.0.5/24\x00", " \x00 ", "\x00/24", "abc\x00", "\x1c\x00\x1f"} {
		for _, op := range []string{"", "exact"} {
			for _, countOnly := range []bool{false, true} {
				_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", CountOnly: countOnly,
					Filters: []provider.Filter{{Field: "contains", Operator: op, Value: in}}})
				var u *UnsupportedFilterError
				if !errors.As(err, &u) {
					t.Errorf("contains=%q %q CountOnly %v: %v, want an UnsupportedFilterError", in, op, countOnly, err)
					continue
				}
				if u.Reason != want || u.Field != "contains" || u.Operator != op {
					t.Errorf("contains=%q: refusal %s %q: %q,\nwant %q", in, u.Field, u.Operator, u.Reason, want)
				}
			}
		}
	}
	noRequestsFor(t, f, "ipam/prefixes")
}

// NetBox has no contains filter on an aggregate, so a dashboard filtering on it
// would return every aggregate in NetBox mode: refused as every filter NetBox
// lacks is (Task 1b's translation), before anything is sent.
func TestContains_RefusedOnAggregates(t *testing.T) {
	f := newFakeService()
	f.addEntity("ipam/aggregates", "id:BIGINT:pk", "prefix:VARCHAR")
	f.addOperators("ipam/aggregates", "prefix", "contains")
	f.schema.FilterLimits = map[string]fakeFilterLimit{"contains": {MaxValues: fakeContainsMaxValues}}
	p := newTestProvider(t, f)
	u := relatedRefusal(t, p, "ipam/aggregates", provider.Filter{Field: "contains", Value: "10.0.0.1"})
	want := "NetBox has no contains filter on ipam/aggregates, so a dashboard filtering on it would return every row in NetBox mode; remove it, or choose a filter the editor offers"
	if u.Reason != want || u.Field != "contains" {
		t.Errorf("refusal %s: %q,\nwant %q", u.Field, u.Reason, want)
	}
	noRequestsFor(t, f, "ipam/aggregates")
}

// On an IP range NetBox's contains is a method of its own (search_contains: the
// ranges whose start and end hold the address), which the replica has no
// operator for.
func TestContains_RefusedOnIPRanges(t *testing.T) {
	f := newFakeService()
	f.addEntity("ipam/ip-ranges", "id:BIGINT:pk", "start_address:VARCHAR", "end_address:VARCHAR")
	p := newTestProvider(t, f)
	u := relatedRefusal(t, p, "ipam/ip-ranges", provider.Filter{Field: "contains", Value: "10.0.0.1"})
	want := "NetBox computes contains with a query of its own (search_contains), which this replica cannot run; use a datasource in NetBox mode for this filter"
	if u.Reason != want || u.Field != "contains" {
		t.Errorf("refusal %s: %q,\nwant %q", u.Field, u.Reason, want)
	}
	noRequestsFor(t, f, "ipam/ip-ranges")
}

// The filter has no lookups: NetBox has no contains__n, nor contains__ic, nor
// contains__empty on a prefix (translateFilters refuses a name it lacks).
func TestContains_NoOtherLookups(t *testing.T) {
	f, p := containmentProvider(t)
	for _, op := range []string{"n", "ic", "nic", "gt", "empty", "nempty"} {
		u := relatedRefusal(t, p, "ipam/prefixes", provider.Filter{Field: "contains", Operator: op, Value: "10.0.0.1"})
		if !strings.HasPrefix(u.Reason, fmt.Sprintf("NetBox has no %s filter on ipam/prefixes", netboxName(provider.Filter{Field: "contains", Operator: op}))) {
			t.Errorf("contains %s: reason %q", op, u.Reason)
		}
	}
	noRequestsFor(t, f, "ipam/prefixes")
}

// The alert Count path sends the rule's filters with CountOnly: the smallest
// page, the filter, and the total the replica counts for it.
func TestContains_Count(t *testing.T) {
	f, p := containmentProvider(t)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", CountOnly: true,
		Filters: []provider.Filter{{Field: "contains", Value: "10.0.0.5/24"}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 3 || len(res.Rows) > 1 {
		t.Errorf("count = %d with %d rows, want 3 and at most the one row the envelope comes with", res.Total, len(res.Rows))
	}
	reqs := f.sentWith("ipam/prefixes", "filter[prefix]__contains")
	if len(reqs) != 1 || reqs[0].query.Get("limit") != "1" || reqs[0].query.Get("filter[prefix]__contains") != "10.0.0.0/24" {
		t.Errorf("count request %v, want limit=1 and filter[prefix]__contains=10.0.0.0/24", reqs)
	}
}

// Beside other filters it is one more condition on the rows, as in NetBox,
// where every filter must hold: one on the same column (prefix, by equality)
// and another column's (status), and a negation of either kind.
func TestContains_BesideOtherFilters(t *testing.T) {
	_, p := containmentProvider(t)
	contains := provider.Filter{Field: "contains", Value: "10.0.0.1"}
	for name, c := range map[string]struct {
		filters []provider.Filter
		want    []int
	}{
		"status":        {[]provider.Filter{contains, {Field: "status", Value: "active"}}, []int{3}},
		"prefix":        {[]provider.Filter{contains, {Field: "prefix", Value: "10.0.0.0/8"}}, []int{2}},
		"nothing alike": {[]provider.Filter{contains, {Field: "prefix", Value: "192.168.0.0/16"}}, nil},
		"not":           {[]provider.Filter{contains, {Field: "status", Operator: "n", Value: "container"}}, []int{3}},
		"not an id":     {[]provider.Filter{contains, {Field: "id", Operator: "n", Value: "2"}}, []int{1, 3}},
	} {
		if got := relatedRows(t, p, "ipam/prefixes", c.filters...); !slices.Equal(got, c.want) {
			t.Errorf("%s: rows %v, want %v", name, got, c.want)
		}
	}
}

// A negation beside it takes its total from counts, one for the rows the other
// filters select and one for each term taken away (negations.go), and every
// one of them carries the containment: a count without it would be of every
// prefix. The page is smaller than the answer, so the counts are needed.
func TestContains_NegatedCountsCarryTheContainment(t *testing.T) {
	f, p := containmentProvider(t)
	f.pageCap = 2
	// 10.0.0.1 is in the ids 1, 2 and 3; id 3 alone is active.
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", Fields: []string{"id"}, Ordering: "id", Limit: 1,
		Filters: []provider.Filter{{Field: "contains", Value: "10.0.0.1"}, {Field: "status", Operator: "n", Value: "active"}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 2 || len(res.Rows) != 1 || len(res.Warnings) != 0 {
		t.Errorf("total %d with %d rows and warnings %v, want 2 with 1 row", res.Total, len(res.Rows), res.Warnings)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	counts := 0
	for _, req := range f.requests {
		if req.entity != "ipam/prefixes" {
			continue
		}
		counts++
		if req.query.Get("filter[prefix]__contains") != "10.0.0.1" {
			t.Errorf("a request without the containment: %v", req.query)
		}
	}
	if counts < 3 {
		t.Errorf("%d requests to ipam/prefixes, want the rows and both counts", counts)
	}
}

// The column is asked for the operator the filter is sent as, and for no other:
// a prefix column whose schema lists no isnull still answers contains.
func TestContains_DoesNotNeedIsNull(t *testing.T) {
	f, p := containmentProvider(t)
	f.mu.Lock()
	e := f.schema.Entities["/v1/ipam/prefixes"]
	for i, c := range e.Columns {
		if c.Name == "prefix" {
			e.Columns[i].Operators = []string{"eq", "contains"}
		}
	}
	f.schema.Entities["/v1/ipam/prefixes"] = e
	f.mu.Unlock()
	if got := containing(t, p, "10.0.0.1"); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("rows %v, want [1 2 3]", got)
	}
}

// A replica that has received no prefixes has no columns to judge the filter
// against, and its row route answers for it, as for every filter: the request
// goes, and is not refused as a column the replica lacks.
func TestContains_OnAnUnfedTable(t *testing.T) {
	f, p := containmentProvider(t)
	f.setIngested("ipam/prefixes", false)
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes",
		Filters: []provider.Filter{{Field: "contains", Value: "10.0.0.1"}}})
	if u := provider.Classify(err); u == nil || u.Kind != provider.ErrorKindNotReplicated {
		t.Errorf("got %v / %+v, want not-replicated", err, u)
	}
	if n := f.countRequestsFor("ipam/prefixes"); n != 1 {
		t.Errorf("%d requests, want the one the row route answers", n)
	}
}

// A plugin's model is no table NetBox's filtersets describe, so contains is no
// filter of its own there: a column, or none.
func TestContains_PluginTypeHasNoSuchColumn(t *testing.T) {
	f := newFakeService()
	f.schema = withPluginDevices(f.schema)
	f.entities[pluginDevices] = []map[string]interface{}{{"id": float64(1), "name": "x"}}
	p := newTestProvider(t, f)
	u := relatedRefusal(t, p, pluginDevices, provider.Filter{Field: "contains", Value: "10.0.0.1"})
	if u.Reason != "this replica has no such column to filter on" || u.Field != "contains" {
		t.Errorf("refusal %s: %q, want the column refusal", u.Field, u.Reason)
	}
	noRequestsFor(t, f, pluginDevices)
}

// unsupportedBuildReason is the refusal of NetBox's contains on a build without
// the operator, as its users read it.
const unsupportedBuildReason = "this replica-cache build has no containment search (v1.39 added it), so it cannot answer NetBox's contains filter; ask NetBox Labs to upgrade the replica, or use a datasource in NetBox mode for this filter"

// A replica-cache build before v1.39 has no containment search, and its schema
// route states no limit for it. NetBox's contains is then refused with what to
// do, not sent as an operator the build answers with a 400, and not offered.
func TestContains_UnsupportedBuildIsRefused(t *testing.T) {
	for name, lacks := range map[string]func(f *fakeService){
		"no limit":       func(f *fakeService) { f.schema.FilterLimits = nil },
		"a zero limit":   func(f *fakeService) { f.schema.FilterLimits = map[string]fakeFilterLimit{"contains": {}} },
		"another limit":  func(f *fakeService) { f.schema.FilterLimits = map[string]fakeFilterLimit{"host": {MaxValues: 5}} },
		"no operator":    func(f *fakeService) { f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR") },
		"no such column": func(f *fakeService) { f.addEntity("ipam/prefixes", "id:BIGINT:pk", "description:VARCHAR") },
	} {
		f := newFakeService()
		f.addPrefixes(containmentRows()...)
		lacks(f)
		p := newTestProvider(t, f)
		u := relatedRefusal(t, p, "ipam/prefixes", provider.Filter{Field: "contains", Value: "10.0.0.1"})
		var want string
		if name == "no such column" {
			want = "this replica does not store prefix, the column NetBox reads for contains, so it cannot answer the filter; remove it, or choose a filter the editor offers"
		} else {
			want = unsupportedBuildReason
		}
		if u.Reason != want || u.Field != "contains" || u.Operator != "" {
			t.Errorf("%s: refusal %s %q: %q,\nwant %q", name, u.Field, u.Operator, u.Reason, want)
		}
		if ops, ok := filterOps(t, p, "ipam/prefixes")["contains"]; ok {
			t.Errorf("%s: contains is offered with %v, on a replica that cannot answer it", name, ops)
		}
		if reqs := f.sentWith("ipam/prefixes", "filter[prefix]__contains"); len(reqs) != 0 {
			t.Errorf("%s: contains was sent: %v", name, reqs[0].query)
		}
	}
}

// An entity the replica has received nothing for has no columns to judge a
// filter against, but whether the build has the operator needs none: a build
// before v1.39 is refused there as it is for a table with data. Sent, the
// operator would get a bare 400, since the service reads a request's operators
// before it looks at the table (TestFake_AnOperatorTheBuildLacksIs400BeforeTheTable),
// where every other filter gets the table's own not-replicated answer. A count,
// which the alert path sends with the rule's filters, is refused alike.
func TestContains_UnsupportedBuildIsRefusedOnAnUnfedTable(t *testing.T) {
	f := newFakeService()
	f.addPrefixes(containmentRows()...)
	f.setIngested("ipam/prefixes", false)
	f.schema.FilterLimits = nil
	p := newTestProvider(t, f)
	for _, countOnly := range []bool{false, true} {
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", CountOnly: countOnly,
			Filters: []provider.Filter{{Field: "contains", Value: "10.0.0.1"}}})
		var u *UnsupportedFilterError
		if !errors.As(err, &u) {
			t.Errorf("CountOnly %v: %v, want an UnsupportedFilterError", countOnly, err)
			continue
		}
		if u.Reason != unsupportedBuildReason || u.Field != "contains" || u.Operator != "" {
			t.Errorf("CountOnly %v: refusal %s %q: %q,\nwant %q", countOnly, u.Field, u.Operator, u.Reason, unsupportedBuildReason)
		}
	}
	noRequestsFor(t, f, "ipam/prefixes")

	// What needs no operator still goes: a value that is none is answered without
	// a request, and any other filter is the table's to answer.
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes",
		Filters: []provider.Filter{{Field: "contains", Value: "garbage"}}})
	if err != nil || len(res.Rows) != 0 {
		t.Errorf("a value that is no address: %v, %v; want no rows", res, err)
	}
	if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes",
		Filters: []provider.Filter{{Field: "prefix", Value: "10.0.0.0/8"}}}); provider.Classify(err) == nil || provider.Classify(err).Kind != provider.ErrorKindNotReplicated {
		t.Errorf("another filter on the unfed table: %v, want the table's not-replicated answer", err)
	}
}

// The editor offers contains on a prefix, with = alone (it has no lookups), and
// nowhere NetBox has none: not on aggregates, which lack the filter, nor on IP
// ranges, where it is a method, nor on any other type. A replica that cannot
// answer it does not offer it (TestContains_UnsupportedBuildIsRefused), and
// what is offered, a query takes.
func TestFilterFields_OffersContainsOnPrefixesOnly(t *testing.T) {
	f, p := containmentProvider(t)
	f.addEntity("ipam/aggregates", "id:BIGINT:pk", "prefix:VARCHAR")
	f.addOperators("ipam/aggregates", "prefix", "contains")
	f.addEntity("ipam/ip-ranges", "id:BIGINT:pk", "start_address:VARCHAR", "end_address:VARCHAR")
	f.addEntity("ipam/ip-addresses", "id:BIGINT:pk", "address:VARCHAR")
	for _, objectType := range []string{"dcim/devices", "ipam/aggregates", "ipam/ip-ranges", "ipam/ip-addresses"} {
		if ops, ok := filterOps(t, p, objectType)["contains"]; ok {
			t.Errorf("%s offers contains %v", objectType, ops)
		}
	}
	prefixes := filterOps(t, p, "ipam/prefixes")
	if want := []string{""}; !slices.Equal(prefixes["contains"], want) {
		t.Errorf("prefixes contains: %v, want %v", prefixes["contains"], want)
	}
	// The prefix filter itself is unchanged.
	if want := []string{""}; !slices.Equal(prefixes["prefix"], want) {
		t.Errorf("prefixes prefix: %v, want %v", prefixes["prefix"], want)
	}
	assertAdvertisedOperatorsAccepted(t, p, "ipam/prefixes", map[string][]string{"contains": prefixes["contains"], "prefix": prefixes["prefix"]})
}

// The operator this datasource writes for the filter is its own: a query that
// names it is refused on any type, as the null operators are.
func TestContains_InternalOperatorIsNeverAcceptedFromAQuery(t *testing.T) {
	f, p := containmentProvider(t)
	for _, objectType := range []string{"ipam/prefixes", "dcim/devices"} {
		for _, field := range []string{"prefix", "name", "contains"} {
			u := relatedRefusal(t, p, objectType, provider.Filter{Field: field, Operator: opContains, Value: "10.0.0.0/24"})
			if !strings.Contains(u.Reason, "one this datasource writes for itself") || u.Operator != opContains {
				t.Errorf("%s %s: refusal %q %q, want the internal-operator refusal", objectType, field, u.Operator, u.Reason)
			}
		}
	}
	if !internalOperators[opContains] || !strings.Contains(opContains, " ") {
		t.Errorf("%q must be an internal operator no saved query can carry (a name with a space)", opContains)
	}
	noRequestsFor(t, f, "ipam/prefixes")
}

// rewriteContains puts the one value of a prefix's contains on the prefix
// column, in the spelling the replica reads (a bare address stays bare; a mask
// is written with the host bits zeroed), as the operator that is the replica's
// containment. The user's field and operator stay on the row for a refusal.
func TestRewriteContains_WritesWhatTheReplicaReads(t *testing.T) {
	prefixes := vocabPrefixes()
	for in, want := range map[string]string{
		"10.0.0.1":                 "10.0.0.1",
		" 10.0.0.1 ":               "10.0.0.1",
		"10.0.0.5/24":              "10.0.0.0/24",
		"10.0.0.5/255.255.255.0":   "10.0.0.0/24",
		"10.0.0.5/0.0.0.255":       "10.0.0.0/24",
		"10.0.0.5/024":             "10.0.0.0/24",
		"10.0.0.1/32":              "10.0.0.1/32",
		"10.0.0.1/255.255.255.255": "10.0.0.1/32",
		"0.0.0.0/0":                "0.0.0.0/0",
		"10.0.0.1/0":               "0.0.0.0/0",
		"2001:DB8::1":              "2001:db8::1",
		"2001:db8::1/32":           "2001:db8::/32",
		"2001:db8::1/ffff:ffff::":  "2001:db8::/32",
		"::ffff:10.0.0.1":          "::ffff:10.0.0.1",
		"::ffff:a00:1":             "::ffff:10.0.0.1",
		"::ffff:10.0.0.1/120":      "::ffff:10.0.0.0/120",
		"::10.0.0.1":               "::a00:1",
		"\x1c10.0.0.1\x1f":         "10.0.0.1",
	} {
		for _, op := range []string{"", "exact"} {
			out, none, err := rewriteContains([]provider.Filter{{Field: "contains", Operator: op, Value: in}}, prefixes.objectType, prefixes.e)
			if err != nil || none {
				t.Errorf("rewriteContains(%q) = %+v, none %v, %v; want the filter", in, out, none, err)
				continue
			}
			wantFilters(t, out, provider.Filter{Field: "prefix", Operator: opContains, Value: want, Label: "contains", LabelOperator: op})
		}
	}

	// The rows around it are untouched, in place; the others of its name that
	// send nothing are not a value.
	rest := []provider.Filter{{Field: "status", Value: "active"}, {Field: "contains", Value: "10.0.0.1"}, {Field: "prefix", Operator: "n", Value: "10.0.0.0/8"}, {Field: "contains", Value: " "}}
	out, none, err := rewriteContains(rest, prefixes.objectType, prefixes.e)
	if err != nil || none {
		t.Fatalf("rewriteContains(%+v) = %+v, none %v, %v", rest, out, none, err)
	}
	wantFilters(t, out, rest[0], provider.Filter{Field: "prefix", Operator: opContains, Value: "10.0.0.1", Label: "contains"}, rest[2], rest[3])
	if rest[1].Field != "contains" {
		t.Error("rewriteContains changed the filters it was given")
	}

	// Nothing to rewrite: no row of the name, none with a value, none that is an
	// equality (NetBox has no other lookup on it), or another table, which keeps
	// its own columns.
	for _, filters := range [][]provider.Filter{nil, {{Field: "status", Value: "active"}}, {{Field: "contains", Value: ""}},
		{{Field: "contains", Operator: "ic", Value: "10.0.0.1"}}, {{Field: "contains", Operator: "n", Value: "10.0.0.1"}}} {
		out, none, err := rewriteContains(filters, prefixes.objectType, prefixes.e)
		if err != nil || none || !slices.Equal(out, filters) {
			t.Errorf("rewriteContains(%+v) = %+v, none %v, %v; want it as it was", filters, out, none, err)
		}
	}
	devices := vocabDevices()
	got, none, err := rewriteContains([]provider.Filter{{Field: "contains", Value: "10.0.0.1"}}, devices.objectType, devices.e)
	if err != nil || none || len(got) != 1 || got[0].Field != "contains" {
		t.Errorf("a table with no contains of NetBox's: %+v, none %v, %v; want the row as it was", got, none, err)
	}
}

// What cannot be read is no row, and the row that said so is gone: left in, it
// would be judged as a column named contains.
func TestRewriteContains_NoRowsDropsTheRow(t *testing.T) {
	prefixes := vocabPrefixes()
	out, none, err := rewriteContains([]provider.Filter{{Field: "status", Value: "active"}, {Field: "contains", Value: "garbage"}}, prefixes.objectType, prefixes.e)
	if err != nil || !none {
		t.Fatalf("rewriteContains = %+v, none %v, %v; want no row to match", out, none, err)
	}
	wantFilters(t, out, provider.Filter{Field: "status", Value: "active"})
}

// A null character is NetBox's error, not a value that matches nothing: the
// refusal names the filter as written, and the rows it was given are left as
// they were.
func TestRewriteContains_NullCharacter(t *testing.T) {
	prefixes := vocabPrefixes()
	given := []provider.Filter{{Field: "status", Value: "active"}, {Field: "contains", Operator: "exact", Value: "10.0.0.1\x00"}}
	before := slices.Clone(given)
	out, none, err := rewriteContains(given, prefixes.objectType, prefixes.e)
	var u *UnsupportedFilterError
	if !errors.As(err, &u) || none || out != nil || u.Field != "contains" || u.Operator != "exact" ||
		u.Reason != "Null characters are not allowed. NetBox mode answers the same; remove the null character from the value" {
		t.Errorf("rewriteContains = %+v, none %v, %v; want NetBox's null-character refusal on contains \"exact\"", out, none, err)
	}
	if !slices.Equal(given, before) {
		t.Errorf("the filters it was given are now %+v, want %+v", given, before)
	}
}

// Several values are one refusal, naming the filter as the first row wrote it.
func TestRewriteContains_SeveralValues(t *testing.T) {
	prefixes := vocabPrefixes()
	_, _, err := rewriteContains([]provider.Filter{{Field: "contains", Operator: "exact", Value: "10.0.0.1"}, {Field: "contains", Value: "10.0.0.2, 10.0.0.3"}}, prefixes.objectType, prefixes.e)
	var u *UnsupportedFilterError
	if !errors.As(err, &u) || u.Field != "contains" || u.Operator != "exact" ||
		u.Reason != "NetBox's contains filter takes one address or prefix, and it would use only the last of the 3 values given, dropping the rest without saying so; select a single value" {
		t.Errorf("rewriteContains = %v, want the several-values refusal on contains \"exact\"", err)
	}
}

// filter_limits.contains.max_values is what a build that has the operator
// reports; a build before it omits the block, and a block that cannot be read
// is no limit rather than a catalogue that cannot be read.
func TestCatalog_FilterLimits(t *testing.T) {
	entities := `"entities":{"/v1/dcim/sites":{"table":"dcim_site","primary_key":"id","ingested":true,"columns":[{"name":"id","type":"BIGINT","nullable":false,"operators":["eq"]}]}}`
	for name, c := range map[string]struct {
		limits string
		want   int
	}{
		"v1.40.1":       {`,"filter_limits":{"contains":{"max_values":1000}}`, 1000},
		"another limit": {`,"filter_limits":{"contains":{"max_values":50}}`, 50},
		"absent":        {``, 0},
		"null":          {`,"filter_limits":null`, 0},
		"empty":         {`,"filter_limits":{}`, 0},
		"zero":          {`,"filter_limits":{"contains":{"max_values":0}}`, 0},
		"negative":      {`,"filter_limits":{"contains":{"max_values":-1}}`, 0},
		"a string":      {`,"filter_limits":{"contains":{"max_values":"1000"}}`, 0},
		"a fraction":    {`,"filter_limits":{"contains":{"max_values":1000.5}}`, 0},
		"a list":        {`,"filter_limits":[1000]`, 0},
		"a number":      {`,"filter_limits":7`, 0},
		"other keys":    {`,"filter_limits":{"host":{"max_values":9},"contains":{"max_values":1000,"max_bytes":5}}`, 1000},
	} {
		var doc schemaDoc
		if err := json.Unmarshal([]byte(`{"snapshot_complete":true,`+entities+c.limits+`}`), &doc); err != nil {
			t.Errorf("%s: the catalogue does not decode: %v", name, err)
			continue
		}
		cat, err := doc.toCatalog()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if cat.ContainsMaxValues != c.want {
			t.Errorf("%s: ContainsMaxValues = %d, want %d", name, cat.ContainsMaxValues, c.want)
		}
	}
}

// The fake is the replica these tests stand on, so it has to refuse what the
// replica refuses: an operator it has no implementation of is a 400 — never a
// filter that matches every row, which is how a test of one passes without the
// operator working — and so is a contains the replica reads as a 400.
func TestFake_UnknownOperatorIs400(t *testing.T) {
	f := newFakeService()
	f.addPrefixes(containmentRows()...)
	// A column that lists no operators, as a schema-less build's does.
	f.mu.Lock()
	e := f.schema.Entities["/v1/ipam/prefixes"]
	e.Columns = append(slices.Clone(e.Columns), fakeColumn{Name: "bare", Type: "VARCHAR"})
	f.schema.Entities["/v1/ipam/prefixes"] = e
	f.mu.Unlock()
	srv := f.start(t)

	get := func(param, value string) (int, string) {
		t.Helper()
		return fakeGet(t, srv.URL, "ipam/prefixes", param, value)
	}
	for _, c := range []struct {
		param, value string
		status       int
	}{
		{"filter[status]__neq", "active", 400},
		{"filter[status]__regex", "act", 400},
		{"filter[status]__contains", "active", 400}, // contains binds a prefix column alone
		{"filter[bare]__nonsense", "x", 400},        // listing no operators does not make every one valid
		{"filter[bare]__between", "1", 400},
		{"filter[prefix]__contains", "abc", 400}, // the whole list is refused for one bad value
		{"filter[prefix]__contains", "10.0.0.1,abc", 400},
		{"filter[prefix]__contains", "10.0.0.1,", 400},    // an empty element
		{"filter[prefix]__contains", "fe80::1%eth0", 400}, // a zone
		{"filter[prefix]__contains", "10.0.0.1/255.255.255.0", 400},
		{"filter[prefix]__contains", "10.0.0.1/024", 400},
		{"filter[prefix]__contains", "", 400},
		{"filter[prefix]__contains", strings.Repeat("10.0.0.1,", fakeContainsMaxValues) + "10.0.0.1", 400},
		{"filter[status]__eq", "active", 200},
		{"filter[prefix]__contains", "10.0.0.1", 200},
		{"filter[prefix]__contains", strings.TrimSuffix(strings.Repeat("10.0.0.1,", fakeContainsMaxValues), ","), 200},
	} {
		if status, msg := get(c.param, c.value); status != c.status {
			t.Errorf("%s=%.40s: HTTP %d (%s), want %d", c.param, c.value, status, msg, c.status)
		}
	}
	if status, msg := get("filter[status]__neq", "active"); msg != "invalid filter operator: neq" {
		t.Errorf("HTTP %d %q, want the service's wording for an operator it does not know", status, msg)
	}
	// The union of a list, and the answers of the semantics the replica states.
	for value, want := range map[string]string{
		"10.0.0.1": "3", "10.0.0.1/32": "4", "10.0.0.5/24": "3", "10.0.0.1,10.0.1.1": "4", "::ffff:10.0.0.1": "3", "10.0.0.1,10.0.0.1": "3",
	} {
		if status, got := get("filter[prefix]__contains", value); status != 200 || got != want {
			t.Errorf("contains=%s: HTTP %d, count %s, want %s", value, status, got, want)
		}
	}
}

// fakeGet is one filter against a list route of the fake: the status, and the
// service's error, or the count of a 200.
func fakeGet(t *testing.T, baseURL, entity, param, value string) (int, string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/" + entity + "?" + url.Values{param: {value}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Error string `json:"error"`
		Count *int   `json:"count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode == http.StatusOK && body.Count != nil {
		return resp.StatusCode, fmt.Sprint(*body.Count)
	}
	return resp.StatusCode, body.Error
}

// The service reads a request's operators before it looks at the table:
// measured on v1.40.1 against a table that never received data, filter[id]__eq
// is its 404 "no data received for this entity" and filter[id]__bogus its 400
// "invalid filter operator: bogus". An operator the build lacks is that 400
// too, whatever the table: a build before v1.39 states no contains limit and has
// no such operator, so a request that sent it to an unfed table would get a bare
// 400 where the table's own 404 is what the other operators get.
func TestFake_AnOperatorTheBuildLacksIs400BeforeTheTable(t *testing.T) {
	f := newFakeService()
	f.addPrefixes(containmentRows()...)
	f.setIngested("ipam/prefixes", false)
	srv := f.start(t)
	for _, c := range []struct {
		param, value, msg string
		status            int
	}{
		{"filter[id]__eq", "1", "no data received for this entity", 404},
		{"filter[id]__bogus", "1", "invalid filter operator: bogus", 400},
		{"filter[prefix]__contains", "10.0.0.1", "no data received for this entity", 404}, // this build has it: the table decides
	} {
		if status, msg := fakeGet(t, srv.URL, "ipam/prefixes", c.param, c.value); status != c.status || msg != c.msg {
			t.Errorf("an unfed table, %s: HTTP %d %q, want %d %q", c.param, status, msg, c.status, c.msg)
		}
	}

	// A build before v1.39 states no limit for contains.
	f.mu.Lock()
	f.schema.FilterLimits = nil
	f.mu.Unlock()
	for _, c := range []struct {
		param, value, msg string
		status            int
	}{
		{"filter[prefix]__contains", "10.0.0.1", "invalid filter operator: contains", 400},
		{"filter[id]__eq", "1", "no data received for this entity", 404},
	} {
		if status, msg := fakeGet(t, srv.URL, "ipam/prefixes", c.param, c.value); status != c.status || msg != c.msg {
			t.Errorf("an unfed table, a build without contains, %s: HTTP %d %q, want %d %q", c.param, status, msg, c.status, c.msg)
		}
	}

	// The same on a table that has data, whose column lists the operator.
	fed := newFakeService()
	fed.addPrefixes(containmentRows()...)
	fed.schema.FilterLimits = nil
	fedSrv := fed.start(t)
	if status, msg := fakeGet(t, fedSrv.URL, "ipam/prefixes", "filter[prefix]__contains", "10.0.0.1"); status != 400 || msg != "invalid filter operator: contains" {
		t.Errorf("a fed table, a build without contains: HTTP %d %q, want 400 invalid filter operator: contains", status, msg)
	}
}
