package replicacache

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

func TestBuildFilterValues(t *testing.T) {
	cases := []struct {
		name    string
		filters []provider.Filter
		want    map[string]string
	}{
		{
			name:    "exact becomes eq",
			filters: []provider.Filter{{Field: "status", Value: "active"}},
			want:    map[string]string{"filter[status]__eq": "active"},
		},
		{
			name:    "explicit exact is the same as empty",
			filters: []provider.Filter{{Field: "status", Operator: "exact", Value: "active"}},
			want:    map[string]string{"filter[status]__eq": "active"},
		},
		{
			// A multi-value dashboard variable arrives as one CSV string. The
			// NetBox provider ORs repeated params; `in` is the equivalent here.
			name:    "multi-value equality becomes in",
			filters: []provider.Filter{{Field: "status", Value: "active, offline"}},
			want:    map[string]string{"filter[status]__in": "active,offline"},
		},
		{
			// The backend's ilike IS a case-insensitive contains on the literal
			// value (measured: ilike=core matched CORE-N9504-01; %core% matched
			// nothing). Nothing is wrapped, and nothing needs escaping.
			name:    "icontains sends the value bare",
			filters: []provider.Filter{{Field: "name", Operator: "ic", Value: "core"}},
			want:    map[string]string{"filter[name]__ilike": "core"},
		},
		{
			name:    "a wildcard character is sent as itself",
			filters: []provider.Filter{{Field: "name", Operator: "ic", Value: "50%"}},
			want:    map[string]string{"filter[name]__ilike": "50%"},
		},
		{
			// DATA-408: the anchored and whole-value matches are separate
			// operators with Django's lookup names, literal like ilike.
			name:    "starts-with becomes istartswith, value bare",
			filters: []provider.Filter{{Field: "name", Operator: "isw", Value: "core"}},
			want:    map[string]string{"filter[name]__istartswith": "core"},
		},
		{
			name:    "ends-with becomes iendswith",
			filters: []provider.Filter{{Field: "name", Operator: "iew", Value: "-01"}},
			want:    map[string]string{"filter[name]__iendswith": "-01"},
		},
		{
			name:    "case-insensitive equality becomes iexact",
			filters: []provider.Filter{{Field: "name", Operator: "ie", Value: "core-n9504-01"}},
			want:    map[string]string{"filter[name]__iexact": "core-n9504-01"},
		},
		{
			name:    "empty asks isnull true",
			filters: []provider.Filter{{Field: "tenant_id", Operator: "empty"}},
			want:    map[string]string{"filter[tenant_id]__isnull": "true"},
		},
		{
			name:    "nempty asks isnull false",
			filters: []provider.Filter{{Field: "tenant_id", Operator: "nempty"}},
			want:    map[string]string{"filter[tenant_id]__isnull": "false"},
		},
		{
			name:    "comparisons pass through",
			filters: []provider.Filter{{Field: "id", Operator: "gt", Value: "100"}},
			want:    map[string]string{"filter[id]__gt": "100"},
		},
		{
			name:    "a blank field is skipped",
			filters: []provider.Filter{{Field: "", Value: "x"}},
			want:    map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildFilterValues(tc.filters, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got.Get(k) != v {
					t.Errorf("param %q = %q, want %q", k, got.Get(k), v)
				}
			}
		})
	}
}

// An exact filter on an IP address column follows NetBox's own rule (ipam's
// net_in lookup): a value without a mask matches every record with that host,
// whatever its mask; a value with a mask matches that exact address. The first
// is the replica's host operator. The second is equality on the stored text,
// which PostgreSQL prints without a single-host mask (/32, /128).
func TestBuildFilterValues_AddressColumnsMatchLikeNetBox(t *testing.T) {
	isAddress := func(field string) bool { return field == "address" }
	for _, tc := range []struct {
		name    string
		filters []provider.Filter
		want    map[string]string
	}{
		{"a bare address matches by host", []provider.Filter{{Field: "address", Value: "10.0.0.1"}},
			map[string]string{"filter[address]__host": "10.0.0.1"}},
		{"several bare addresses are one host list", []provider.Filter{{Field: "address", Value: "10.0.0.1, 10.0.0.2"}},
			map[string]string{"filter[address]__host": "10.0.0.1,10.0.0.2"}},
		{"rows on one field union into the list", []provider.Filter{{Field: "address", Value: "10.0.0.1"}, {Field: "address", Operator: "exact", Value: "10.0.0.2"}},
			map[string]string{"filter[address]__host": "10.0.0.1,10.0.0.2"}},
		{"IPv6 is written canonically", []provider.Filter{{Field: "address", Value: "2001:0DB8::0001"}},
			map[string]string{"filter[address]__host": "2001:db8::1"}},
		{"a mapped address stays IPv6, as NetBox compares it", []provider.Filter{{Field: "address", Value: "::ffff:10.0.0.1"}},
			map[string]string{"filter[address]__host": "::ffff:10.0.0.1"}},
		{"a masked address matches exactly", []provider.Filter{{Field: "address", Value: "10.0.16.1/21"}},
			map[string]string{"filter[address]__eq": "10.0.16.1/21"}},
		{"a single-host mask is written as PostgreSQL prints it", []provider.Filter{{Field: "address", Value: "10.0.0.1/32"}},
			map[string]string{"filter[address]__eq": "10.0.0.1"}},
		{"an IPv6 single-host mask too", []provider.Filter{{Field: "address", Value: "2001:DB8::1/128"}},
			map[string]string{"filter[address]__eq": "2001:db8::1"}},
		{"several masked addresses are one in list", []provider.Filter{{Field: "address", Value: "10.0.16.1/21, 10.0.16.2/21"}},
			map[string]string{"filter[address]__in": "10.0.16.1/21,10.0.16.2/21"}},
		// PostgreSQL prints an IPv4-compatible IPv6 address (the first 96 bits
		// zero) with its low 32 bits dotted, where netip prints hex; the stored
		// text is PostgreSQL's, so equality has to be written its way.
		{"an IPv4-compatible address is written as PostgreSQL prints it", []provider.Filter{{Field: "address", Value: "::a00:1/64"}},
			map[string]string{"filter[address]__eq": "::10.0.0.1/64"}},
		{"already in PostgreSQL's form it is unchanged", []provider.Filter{{Field: "address", Value: "::0.1.0.2/64"}},
			map[string]string{"filter[address]__eq": "::0.1.0.2/64"}},
		{"a low address with a zero seventh word stays hex, in both", []provider.Filter{{Field: "address", Value: "::100/128"}},
			map[string]string{"filter[address]__eq": "::100"}},
		{"a bare IPv4-compatible host goes out in PostgreSQL's form too", []provider.Filter{{Field: "address", Value: "::a00:1"}},
			map[string]string{"filter[address]__host": "::10.0.0.1"}},
		// NetBox ORs the masked value with the bare one; every record the masked
		// value matches has that host, so the OR is the host match alone.
		{"a masked value its bare host already covers is merged", []provider.Filter{{Field: "address", Value: "10.0.0.1, 10.0.0.1/24"}},
			map[string]string{"filter[address]__host": "10.0.0.1"}},
		// NetBox compares HOST(address) with the text, so a value that is no
		// address, or carries a zone, matches nothing there. Here host would
		// refuse the whole list for it, so it is left out instead.
		{"a value that is no address is left out of the host list", []provider.Filter{{Field: "address", Value: "10.0.0.1, bogus"}},
			map[string]string{"filter[address]__host": "10.0.0.1"}},
		{"so is an address with a zone", []provider.Filter{{Field: "address", Value: "10.0.0.1, fe80::1%eth0"}},
			map[string]string{"filter[address]__host": "10.0.0.1"}},
		// With nothing left, the filter must still match nothing: dropping it
		// would return every row.
		{"nothing valid still matches nothing", []provider.Filter{{Field: "address", Value: "bogus"}},
			map[string]string{"filter[address]__eq": "bogus"}},
		{"any other column is untouched", []provider.Filter{{Field: "name", Value: "10.0.0.1"}},
			map[string]string{"filter[name]__eq": "10.0.0.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildFilterValues(tc.filters, isAddress)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got.Get(k) != v {
					t.Errorf("param %q = %q, want %q (all: %v)", k, got.Get(k), v, got)
				}
			}
		})
	}
}

// NetBox ORs the two kinds of value (masked exactly, bare by host); the
// replica ANDs its filters and has no OR, so a filter mixing them is refused
// rather than narrowed to one kind.
func TestBuildFilterValues_RefusesMixingMaskedAndBareAddresses(t *testing.T) {
	_, err := buildFilterValues([]provider.Filter{{Field: "address", Value: "10.0.0.1, 10.0.0.2/24"}},
		func(field string) bool { return field == "address" })
	var u *UnsupportedFilterError
	if !errors.As(err, &u) || !strings.Contains(u.Reason, "mask") {
		t.Fatalf("want a refusal explaining the mix of masked and bare addresses, got %v", err)
	}
}

// No text match can be unioned: replica-cache has no OR, so "name starts with
// A or B" has no expression. Each of the four is refused for several values,
// and for a repeat on one field, rather than narrowed to the first value.
func TestBuildFilterValuesRefusesSeveralValuesForAnyTextMatch(t *testing.T) {
	for _, op := range []string{opIContns, opIExact, opIStarts, opIEnds} {
		_, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: op, Value: "core, spine"}}, nil)
		var unsupported *UnsupportedFilterError
		if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Reason, "several values") {
			t.Fatalf("%q with two values: want the several-values refusal, got %v", op, err)
		}
		_, err = buildFilterValues([]provider.Filter{{Field: "name", Operator: op, Value: "core"}, {Field: "name", Operator: op, Value: "spine"}}, nil)
		if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Reason, "applied twice") {
			t.Fatalf("%q twice on one field: want the applied-twice refusal, got %v", op, err)
		}
	}
}

// Every operator refused here has a plausible near-miss the code could have
// silently substituted. Refusing is the behaviour under test: a gte quietly
// answered as gt returns a different set of rows and looks completely healthy.
func TestBuildFilterValuesRefusesUnsupportedOperators(t *testing.T) {
	for _, op := range []string{"gte", "lte", "n", "nic", "nie", "nisw", "niew", "regex", "iregex"} {
		t.Run(op, func(t *testing.T) {
			_, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: op, Value: "x"}}, nil)
			if err == nil {
				t.Fatalf("operator %q was accepted; it must be refused, not approximated", op)
			}
			var unsupported *UnsupportedFilterError
			if !errors.As(err, &unsupported) {
				t.Fatalf("want *UnsupportedFilterError, got %T", err)
			}
			c := unsupported.Classification()
			if c.Kind != provider.ErrorKindUnsupported {
				t.Errorf("want an unsupported classification so the reason survives; got %q", c.Kind)
			}
			// Detail is what the user actually reads. Without it the message
			// degrades to a generic "NetBox rejected this query", which blames a
			// service the request never reached.
			if !strings.Contains(c.Detail, op) {
				t.Errorf("Detail should name the offending operator: %q", c.Detail)
			}
		})
	}
}

// A multi-value substring filter cannot be expressed, and picking the first
// value would silently narrow a dashboard variable to one of its selections.
func TestBuildFilterValuesRefusesMultiValueTextMatch(t *testing.T) {
	_, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: "ic", Value: "core,edge"}}, nil)
	if err == nil {
		t.Fatal("want an error for a multi-value substring match, got none")
	}
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want *UnsupportedFilterError, got %T", err)
	}
}

func TestBuildFilterValuesRefusesMultiValueComparison(t *testing.T) {
	if _, err := buildFilterValues([]provider.Filter{{Field: "id", Operator: "gt", Value: "1,2"}}, nil); err == nil {
		t.Fatal("want an error for a multi-value comparison, got none")
	}
}

// Two rows on one field mean OR. Writing each in turn let the last overwrite
// the rest, so "status is active or planned" silently became "status is
// planned" — a narrower result set, with nothing to show it happened.
func TestBuildFilterValuesUnionsRepeatedExactFilters(t *testing.T) {
	got, err := buildFilterValues([]provider.Filter{
		{Field: "status", Value: "active"},
		{Field: "status", Value: "planned"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := got.Get("filter[status]__in"); v != "active,planned" {
		t.Errorf("filter[status]__in = %q, want %q", v, "active,planned")
	}
	if got.Get("filter[status]__eq") != "" {
		t.Error("a unioned filter must not also emit eq")
	}
}

// The same union, whether the values arrive as several rows, one multi-value
// variable, or a mix of the two.
func TestBuildFilterValuesUnionsMixedExactSources(t *testing.T) {
	got, err := buildFilterValues([]provider.Filter{
		{Field: "status", Value: "active,planned"},
		{Field: "status", Value: "offline"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := got.Get("filter[status]__in"); v != "active,planned,offline" {
		t.Errorf("filter[status]__in = %q", v)
	}
}

// A single exact filter still uses eq rather than a one-element in.
func TestBuildFilterValuesKeepsSingleExactAsEq(t *testing.T) {
	got, err := buildFilterValues([]provider.Filter{{Field: "status", Value: "active"}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := got.Get("filter[status]__eq"); v != "active" {
		t.Errorf("filter[status]__eq = %q", v)
	}
}

// Filters on different fields stay independent.
func TestBuildFilterValuesKeepsFieldsSeparate(t *testing.T) {
	got, err := buildFilterValues([]provider.Filter{
		{Field: "status", Value: "active"},
		{Field: "status", Value: "planned"},
		{Field: "name", Value: "CORE-1"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := got.Get("filter[status]__in"); v != "active,planned" {
		t.Errorf("status = %q", v)
	}
	if v := got.Get("filter[name]__eq"); v != "CORE-1" {
		t.Errorf("name = %q", v)
	}
}

// Non-exact operators cannot be unioned, so a repeat is refused rather than
// resolved by last-write-wins — the failure mode this whole change is about.
func TestBuildFilterValuesRefusesRepeatedNonExactOperators(t *testing.T) {
	cases := []struct {
		name    string
		filters []provider.Filter
	}{
		{"two contains", []provider.Filter{
			{Field: "name", Operator: "ic", Value: "core"},
			{Field: "name", Operator: "ic", Value: "edge"},
		}},
		{"two gt", []provider.Filter{
			{Field: "id", Operator: "gt", Value: "5"},
			{Field: "id", Operator: "gt", Value: "9"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildFilterValues(tc.filters, nil); err == nil {
				t.Fatal("want a refusal, got none: silently dropping one narrows the result")
			}
		})
	}
}

// Different operators on one field compose into a range and must be kept.
func TestBuildFilterValuesAllowsARangeOnOneField(t *testing.T) {
	got, err := buildFilterValues([]provider.Filter{
		{Field: "id", Operator: "gt", Value: "5"},
		{Field: "id", Operator: "lt", Value: "10"},
	}, nil)
	if err != nil {
		t.Fatalf("a range must be allowed: %v", err)
	}
	if got.Get("filter[id]__gt") != "5" || got.Get("filter[id]__lt") != "10" {
		t.Errorf("range lost: %v", got)
	}
}

// The is-empty refusal used to recommend "filter on equality with an empty
// value". Following that advice lands in the branch below, which drops the
// filter and returns the unfiltered population — the very outcome the refusal
// exists to prevent. This pins both halves: the advice, and the behaviour that
// makes the old advice wrong.
func TestIsEmptyRefusalDoesNotRecommendADiscardedFilter(t *testing.T) {
	c := catalogFromFake(t, devicesSchema())
	err := validateFilters([]provider.Filter{{Field: "name", Operator: "empty"}}, c.Entities["dcim/devices"], c)
	if err == nil {
		t.Fatal("is-empty on a text column must still be refused")
	}
	msg := err.Error()
	if strings.Contains(msg, "equality with an empty value") {
		t.Errorf("recommends a filter that is discarded: %q", msg)
	}
	if !strings.Contains(msg, "NetBox mode") {
		t.Errorf("should point at what actually works: %q", msg)
	}

	// Why that advice was wrong: an equality filter with an empty value emits
	// no parameter at all. Same as NetBox mode, which drops empties too — so
	// this is consistency, not a gap to close here.
	q, err := buildFilterValues([]provider.Filter{{Field: "serial", Operator: "exact", Value: ""}}, nil)
	if err != nil {
		t.Fatalf("buildFilterValues: %v", err)
	}
	if len(q) != 0 {
		t.Errorf("an empty equality value must emit no filter, got %v", q)
	}
}

// The catalogue's operators translate to the editor's tokens.
func TestSeamOperators_TranslateTheCatalogue(t *testing.T) {
	all := []string{"eq", "gt", "lt", "in", "isnull"}
	text := append(slices.Clone(all), "ilike")
	for _, tc := range []struct {
		name string
		col  column
		want []string
	}{
		{"nullable text", column{Type: "VARCHAR", Nullable: true, Operators: text}, []string{"", "ic", "gt", "lt"}},
		// DATA-408: a replica that lists the anchored matches gets them offered,
		// in the editor's tokens; one that lists only ilike does not (above).
		{"text with the anchored matches", column{Type: "VARCHAR", Nullable: true, Operators: append(slices.Clone(text), "istartswith", "iendswith", "iexact")},
			[]string{"", "ic", "isw", "iew", "ie", "gt", "lt"}},
		{"nullable number", column{Type: "BIGINT", Nullable: true, Operators: all}, []string{"", "gt", "lt", "empty", "nempty"}},
		{"not null number", column{Type: "BIGINT", Operators: all}, []string{"", "gt", "lt"}},
		{"nullable timestamp", column{Type: "TIMESTAMP WITH TIME ZONE", Nullable: true, Operators: all}, []string{"", "gt", "lt", "empty", "nempty"}},
		{"equality only", column{Type: "VARCHAR", Nullable: true, Operators: []string{"eq"}}, []string{""}},
		{"nothing", column{Type: "VARCHAR"}, nil},
	} {
		if got := seamOperators(tc.col); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A saved or provisioned query predates the editor's answer, so every filter
// is checked against the catalogue before anything is sent. Fail closed: a
// text match on a number used to be a 200 over the whole table.
func TestValidateFilters_AgainstTheCatalogue(t *testing.T) {
	c := catalogFromFake(t, devicesSchema())
	e := c.Entities["dcim/devices"]
	for _, tc := range []struct {
		name string
		f    provider.Filter
		ok   bool
	}{
		{"text op on VARCHAR", provider.Filter{Field: "name", Operator: "ic", Value: "core"}, true},
		{"text op on BIGINT", provider.Filter{Field: "id", Operator: "ic", Value: "1"}, false},
		{"exact is spelled two ways", provider.Filter{Field: "id", Operator: "exact", Value: "1"}, true},
		{"empty on nullable number", provider.Filter{Field: "position", Operator: "empty"}, true},
		{"empty on NOT NULL", provider.Filter{Field: "id", Operator: "empty"}, false},
		{"empty on text", provider.Filter{Field: "name", Operator: "empty"}, false},
		{"unknown column", provider.Filter{Field: "colour", Value: "x"}, false},
		{"expanded name, text op", provider.Filter{Field: "site", Operator: "ic", Value: "ams"}, true},
		{"expanded slug, contains", provider.Filter{Field: "site_slug", Operator: "ic", Value: "ams"}, true},
		{"starts-with is not a match this backend has", provider.Filter{Field: "name", Operator: "isw", Value: "core"}, false},
		{"case-insensitive equality neither", provider.Filter{Field: "name", Operator: "ie", Value: "core"}, false},
		{"nor on an expanded name", provider.Filter{Field: "site", Operator: "isw", Value: "dc-"}, false},
		{"unavailable expansion", provider.Filter{Field: "platform", Value: "x"}, false},
		{"cf_ is not filterable", provider.Filter{Field: "cf_lifecycle_phase", Value: "x"}, false},
		{"blank row is ignored", provider.Filter{Field: "", Value: ""}, true},
		// A row with an operator and no value emits no parameter (as in NetBox
		// mode), so it must not be refused either: a saved query carrying one
		// used to run.
		{"blank value is ignored", provider.Filter{Field: "name", Operator: "isw", Value: ""}, true},
		{"blank value on a number is ignored", provider.Filter{Field: "id", Operator: "ic", Value: " "}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFilters([]provider.Filter{tc.f}, e, c)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				var u *UnsupportedFilterError
				if !errors.As(err, &u) {
					t.Errorf("refusal must be an UnsupportedFilterError, got %T", err)
				}
			}
		})
	}
}

// A saved query with an anchored match is told what does work on this
// replica, not the generic "does not take that operator".
func TestValidateFilters_ExplainsTheTextMatchLimit(t *testing.T) {
	c := catalogFromFake(t, devicesSchema())
	err := validateFilters([]provider.Filter{{Field: "name", Operator: "isw", Value: "core"}}, c.Entities["dcim/devices"], c)
	if err == nil || !strings.Contains(err.Error(), "contains") {
		t.Errorf("the refusal should point at the match that does work: %v", err)
	}
}

// A replica that lists the anchored matches (DATA-408) takes them on the
// columns it lists them for — stored text, and an expanded name whose target
// column lists them — and on nothing else.
func TestValidateFilters_AcceptsAnchoredMatchesTheCatalogueLists(t *testing.T) {
	c := catalogFromFake(t, withAnchoredText(devicesSchema()))
	e := c.Entities["dcim/devices"]
	for _, tc := range []struct {
		name string
		f    provider.Filter
		ok   bool
	}{
		{"starts-with on text", provider.Filter{Field: "name", Operator: "isw", Value: "core"}, true},
		{"ends-with on text", provider.Filter{Field: "name", Operator: "iew", Value: "-01"}, true},
		{"case-insensitive equality on text", provider.Filter{Field: "name", Operator: "ie", Value: "core"}, true},
		{"starts-with on an expanded name", provider.Filter{Field: "site", Operator: "isw", Value: "dc-"}, true},
		{"ends-with on an expanded slug", provider.Filter{Field: "site_slug", Operator: "iew", Value: "-east"}, true},
		{"starts-with on a number", provider.Filter{Field: "id", Operator: "isw", Value: "1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateFilters([]provider.Filter{tc.f}, e, c); (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// The catalogue lists every served entity, so an expansion's target is
// normally there; when it is not, the stand-in text column takes what the
// BUILD offers — the anchored matches on a DATA-408 build — so a filter on it
// is neither refused for the wrong reason nor sent to a build that lacks it.
func TestValidateFilters_StandInTargetFollowsTheBuild(t *testing.T) {
	f := provider.Filter{Field: "site", Operator: "isw", Value: "dc-"}
	anchored := withAnchoredText(devicesSchema())
	delete(anchored.Entities, "/v1/dcim/sites")
	c := catalogFromFake(t, anchored)
	if err := validateFilters([]provider.Filter{f}, c.Entities["dcim/devices"], c); err != nil {
		t.Errorf("a DATA-408 build takes starts-with on a stand-in: %v", err)
	}
	plain := devicesSchema()
	delete(plain.Entities, "/v1/dcim/sites")
	c = catalogFromFake(t, plain)
	if err := validateFilters([]provider.Filter{f}, c.Entities["dcim/devices"], c); err == nil || !strings.Contains(err.Error(), "contains") {
		t.Errorf("a pre-408 build refuses it, pointing at contains: %v", err)
	}
}

// An expansion whose target has no data looks perfectly real in a saved
// query, so the refusal names the entity that is missing.
func TestValidateFilters_NamesTheUnfedTarget(t *testing.T) {
	c := catalogFromFake(t, devicesSchema())
	err := validateFilters([]provider.Filter{{Field: "platform", Value: "x"}}, c.Entities["dcim/devices"], c)
	if err == nil || !strings.Contains(err.Error(), "dcim/platforms") {
		t.Errorf("the refusal should name the entity that has no data: %v", err)
	}
}
