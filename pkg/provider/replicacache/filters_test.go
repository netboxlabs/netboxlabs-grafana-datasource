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
			name:    "icontains wraps the value in wildcards",
			filters: []provider.Filter{{Field: "name", Operator: "ic", Value: "core"}},
			want:    map[string]string{"filter[name]__ilike": "%core%"},
		},
		{
			name:    "istartswith anchors left",
			filters: []provider.Filter{{Field: "name", Operator: "isw", Value: "core"}},
			want:    map[string]string{"filter[name]__ilike": "core%"},
		},
		{
			name:    "iendswith anchors right",
			filters: []provider.Filter{{Field: "name", Operator: "iew", Value: "01"}},
			want:    map[string]string{"filter[name]__ilike": "%01"},
		},
		{
			// ilike with no wildcards is exactly case-insensitive equality.
			name:    "iexact uses ilike without wildcards",
			filters: []provider.Filter{{Field: "name", Operator: "ie", Value: "CORE-1"}},
			want:    map[string]string{"filter[name]__ilike": "CORE-1"},
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
			got, err := buildFilterValues(tc.filters)
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

// Every operator refused here has a plausible near-miss the code could have
// silently substituted. Refusing is the behaviour under test: a gte quietly
// answered as gt returns a different set of rows and looks completely healthy.
func TestBuildFilterValuesRefusesUnsupportedOperators(t *testing.T) {
	for _, op := range []string{"gte", "lte", "n", "nic", "nie", "nisw", "niew", "regex", "iregex"} {
		t.Run(op, func(t *testing.T) {
			_, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: op, Value: "x"}})
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
	_, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: "ic", Value: "core,edge"}})
	if err == nil {
		t.Fatal("want an error for a multi-value substring match, got none")
	}
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want *UnsupportedFilterError, got %T", err)
	}
}

func TestBuildFilterValuesRefusesMultiValueComparison(t *testing.T) {
	if _, err := buildFilterValues([]provider.Filter{{Field: "id", Operator: "gt", Value: "1,2"}}); err == nil {
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
	})
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
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := got.Get("filter[status]__in"); v != "active,planned,offline" {
		t.Errorf("filter[status]__in = %q", v)
	}
}

// A single exact filter still uses eq rather than a one-element in.
func TestBuildFilterValuesKeepsSingleExactAsEq(t *testing.T) {
	got, err := buildFilterValues([]provider.Filter{{Field: "status", Value: "active"}})
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
	})
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
			if _, err := buildFilterValues(tc.filters); err == nil {
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
	})
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
	q, err := buildFilterValues([]provider.Filter{{Field: "serial", Operator: "exact", Value: ""}})
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
		{"nullable text", column{Type: "VARCHAR", Nullable: true, Operators: text}, []string{"", "ie", "ic", "isw", "iew", "gt", "lt"}},
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
		{"expanded slug", provider.Filter{Field: "site_slug", Operator: "isw", Value: "ams"}, true},
		{"unavailable expansion", provider.Filter{Field: "platform", Value: "x"}, false},
		{"cf_ is not filterable", provider.Filter{Field: "cf_lifecycle_phase", Value: "x"}, false},
		{"blank row is ignored", provider.Filter{Field: "", Value: ""}, true},
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

// An expansion whose target has no data looks perfectly real in a saved
// query, so the refusal names the entity that is missing.
func TestValidateFilters_NamesTheUnfedTarget(t *testing.T) {
	c := catalogFromFake(t, devicesSchema())
	err := validateFilters([]provider.Filter{{Field: "platform", Value: "x"}}, c.Entities["dcim/devices"], c)
	if err == nil || !strings.Contains(err.Error(), "dcim/platforms") {
		t.Errorf("the refusal should name the entity that has no data: %v", err)
	}
}
