package replicacache

import (
	"errors"
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

func TestFilterFieldsAdvertisesOnlySupportedOperators(t *testing.T) {
	fields := []provider.Field{{Name: "name", Type: provider.FieldTypeString}, {Name: "id", Type: provider.FieldTypeNumber}}
	raw := map[string]bool{"name": true, "id": true}
	types := map[string]provider.FieldType{"name": provider.FieldTypeString, "id": provider.FieldTypeNumber}

	got := filterFieldsFor(fields, raw, types)
	if len(got) != 2 {
		t.Fatalf("want 2 filter fields, got %d", len(got))
	}
	// Sorted, so the editor's list is stable between refreshes.
	if got[0].Name != "id" || got[1].Name != "name" {
		t.Errorf("want fields sorted by name, got %q then %q", got[0].Name, got[1].Name)
	}
	for _, op := range got[0].Operators {
		if _, err := buildFilterValues([]provider.Filter{{Field: "id", Operator: op, Value: "1"}}); err != nil {
			t.Errorf("operator %q is advertised but rejected by the translator: %v", op, err)
		}
	}
}

// ILIKE against a non-text column was measured returning HTTP 200 with the
// UNFILTERED total, so a text operator must never be offered for one.
func TestFilterFieldsWithholdsTextOperatorsFromNonTextColumns(t *testing.T) {
	fields := []provider.Field{
		{Name: "name", Type: provider.FieldTypeString},
		{Name: "id", Type: provider.FieldTypeNumber},
		{Name: "sometimes_null", Type: provider.FieldTypeString},
	}
	raw := map[string]bool{"name": true, "id": true, "sometimes_null": true}
	types := map[string]provider.FieldType{
		"name": provider.FieldTypeString,
		"id":   provider.FieldTypeNumber,
		// never seen holding a value, so it cannot be typed
		"sometimes_null": provider.FieldType(""),
	}

	byName := map[string][]string{}
	for _, f := range filterFieldsFor(fields, raw, types) {
		byName[f.Name] = f.Operators
	}
	has := func(ops []string, op string) bool {
		for _, o := range ops {
			if o == op {
				return true
			}
		}
		return false
	}
	if !has(byName["name"], opIContns) {
		t.Error("a text column must keep the text operators")
	}
	if has(byName["id"], opIContns) {
		t.Error("a numeric column must not be offered a text match: ILIKE on it returns every row unfiltered")
	}
	if has(byName["sometimes_null"], opIContns) {
		t.Error("an untypeable column must not be offered a text match")
	}
	// Equality stays available everywhere.
	for _, n := range []string{"name", "id", "sometimes_null"} {
		if !has(byName[n], opExact) {
			t.Errorf("%q lost equality", n)
		}
	}
	// Is-empty is the mirror case: kept where blank means NULL, withheld for
	// text where it would answer the opposite question (see the empty-family
	// tests below).
	if !has(byName["id"], opEmpty) {
		t.Error("a numeric column should keep is-empty: blank means NULL there")
	}
	if has(byName["name"], opEmpty) {
		t.Error("a text column must not be offered is-empty: IS NULL inverts it")
	}
}

// Derived columns are built here, not stored upstream, so filtering on one is
// rejected as an unknown column. They must not be advertised as filterable.
func TestFilterFieldsExcludesDerivedColumns(t *testing.T) {
	fields := []provider.Field{
		{Name: "site_id", Type: provider.FieldTypeNumber},
		{Name: "site", Type: provider.FieldTypeString},
		{Name: "cf_tier", Type: provider.FieldTypeString},
	}
	raw := map[string]bool{"site_id": true}
	types := map[string]provider.FieldType{"site_id": provider.FieldTypeNumber, "site": provider.FieldTypeString}

	got := filterFieldsFor(fields, raw, types)
	if len(got) != 1 || got[0].Name != "site_id" {
		t.Fatalf("want only the upstream column offered, got %+v", got)
	}
}

// A saved dashboard predates the editor's current answer, so the combination
// has to be refused at query time too.
func TestValidateFilterTypesRefusesTextMatchOnNonTextColumn(t *testing.T) {
	types := map[string]provider.FieldType{"id": provider.FieldTypeNumber, "name": provider.FieldTypeString}

	err := validateFilterTypes([]provider.Filter{{Field: "id", Operator: opIContns, Value: "1"}}, types)
	if err == nil {
		t.Fatal("want a refusal for a text match on a numeric column")
	}
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Fatalf("want *UnsupportedFilterError, got %T", err)
	}

	if err := validateFilterTypes([]provider.Filter{{Field: "name", Operator: opIContns, Value: "x"}}, types); err != nil {
		t.Errorf("a text match on a text column must be allowed: %v", err)
	}
	// Equality is safe on any type and must not be blocked.
	if err := validateFilterTypes([]provider.Filter{{Field: "id", Operator: opExact, Value: "1"}}, types); err != nil {
		t.Errorf("equality on a numeric column must be allowed: %v", err)
	}
	// Unknown types fail CLOSED. This previously asserted the opposite, on the
	// reasoning that the request itself would be authoritative — which does not
	// hold here: a text match on a numeric column does not fail, it answers
	// HTTP 200 over the unfiltered population. There is no error to defer to.
	if err := validateFilterTypes([]provider.Filter{{Field: "id", Operator: opIContns, Value: "1"}}, nil); err == nil {
		t.Error("a type-sensitive filter must be refused when no type information is available")
	}
	// A column absent from an otherwise-populated map is the same case.
	if err := validateFilterTypes([]provider.Filter{{Field: "mystery", Operator: opIContns, Value: "x"}},
		map[string]provider.FieldType{"name": provider.FieldTypeString}); err == nil {
		t.Error("an unseen column must be refused a text match")
	}
	// Operators whose validity does not depend on type are unaffected.
	if err := validateFilterTypes([]provider.Filter{{Field: "anything", Operator: opExact, Value: "x"}}, nil); err != nil {
		t.Errorf("equality does not depend on type and must still work: %v", err)
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

// NetBox stores a blank character field as "" rather than NULL, so translating
// is-empty to IS NULL asks the opposite question. Measured on staging, where
// every device has a blank serial: isnull=true matched 0 of 6,824,570 rows and
// isnull=false matched all of them — so "is empty" found nothing where every
// row qualified, and "has any value" found everything where none did.
func TestFilterFieldsWithholdsTheEmptyFamilyFromTextColumns(t *testing.T) {
	fields := []provider.Field{
		{Name: "serial", Type: provider.FieldTypeString},
		{Name: "tenant_id", Type: provider.FieldTypeNumber},
	}
	raw := map[string]bool{"serial": true, "tenant_id": true}
	types := map[string]provider.FieldType{
		"serial":    provider.FieldTypeString,
		"tenant_id": provider.FieldTypeNumber,
	}

	byName := map[string][]string{}
	for _, f := range filterFieldsFor(fields, raw, types) {
		byName[f.Name] = f.Operators
	}
	has := func(ops []string, op string) bool {
		for _, o := range ops {
			if o == op {
				return true
			}
		}
		return false
	}

	for _, op := range []string{opEmpty, opNEmpty} {
		if has(byName["serial"], op) {
			t.Errorf("text column was offered %q; IS NULL answers the opposite question for a blank string", op)
		}
		if !has(byName["tenant_id"], op) {
			t.Errorf("numeric column lost %q; blank really is NULL there", op)
		}
	}
}

// A saved dashboard predates the editor's current answer, so the combination
// has to be refused at query time as well.
func TestValidateFilterTypesRefusesTheEmptyFamilyOnTextColumns(t *testing.T) {
	types := map[string]provider.FieldType{
		"serial":    provider.FieldTypeString,
		"tenant_id": provider.FieldTypeNumber,
	}

	for _, op := range []string{opEmpty, opNEmpty} {
		err := validateFilterTypes([]provider.Filter{{Field: "serial", Operator: op}}, types)
		if err == nil {
			t.Fatalf("want a refusal for %q on a text column", op)
		}
		var unsupported *UnsupportedFilterError
		if !errors.As(err, &unsupported) {
			t.Fatalf("want *UnsupportedFilterError, got %T", err)
		}
		// The message has to say what to do instead, since the operator is in
		// the editor for every other column type.
		if !strings.Contains(unsupported.Reason, "empty string") {
			t.Errorf("reason should explain the blank-string mismatch: %q", unsupported.Reason)
		}

		if err := validateFilterTypes([]provider.Filter{{Field: "tenant_id", Operator: op}}, types); err != nil {
			t.Errorf("%q on a numeric column must be allowed: %v", op, err)
		}
	}
}

// A column whose type was never established must NOT get the empty family.
//
// This test previously asserted the opposite, and was wrong: "unknown" is not
// "not text". A nullable text column that was NULL in every sampled row is
// precisely what cannot be typed, and precisely where IS NULL would later miss
// the "" rows — so the untyped case is the one most likely to invert, not the
// one safe to wave through.
func TestEmptyFamilyRefusedWhenTypeIsUnknown(t *testing.T) {
	types := map[string]provider.FieldType{"mystery": provider.FieldType("")}
	for _, op := range []string{opEmpty, opNEmpty} {
		if err := validateFilterTypes([]provider.Filter{{Field: "mystery", Operator: op}}, types); err == nil {
			t.Errorf("%q on an untyped column must be refused: it may be nullable text", op)
		}
	}

	// A confirmed non-text type still gets them. The time column is named for
	// what it is, which for THIS operator is part of the evidence — see below.
	ok := map[string]provider.FieldType{
		"vcpus":        provider.FieldTypeNumber,
		"is_active":    provider.FieldTypeBoolean,
		"last_updated": provider.FieldTypeTime,
	}
	for field := range ok {
		if err := validateFilterTypes([]provider.Filter{{Field: field, Operator: opEmpty}}, ok); err != nil {
			t.Errorf("is-empty on a confirmed %s column must be allowed: %v", ok[field], err)
		}
	}

	// A column TYPED as time whose name does not agree does not get it. The
	// type is inferred from values, and a text column whose sampled values all
	// look like RFC3339 — contrived for NetBox's own models, but a plugin can
	// define anything — would otherwise be handed an operator this backend
	// answers with IS NULL, while blank text is stored as "": the exact
	// opposite population, returned without an error.
	valueOnly := map[string]provider.FieldType{"note": provider.FieldTypeTime}
	for _, op := range []string{opEmpty, opNEmpty} {
		if err := validateFilterTypes([]provider.Filter{{Field: "note", Operator: op}}, valueOnly); err == nil {
			t.Errorf("%q on a time type with no corroborating name must be refused", op)
		}
	}
}

// The is-empty refusal used to recommend "filter on equality with an empty
// value". Following that advice lands in the branch below, which drops the
// filter and returns the unfiltered population — the very outcome the refusal
// exists to prevent. This pins both halves: the advice, and the behaviour that
// makes the old advice wrong.
func TestIsEmptyRefusalDoesNotRecommendADiscardedFilter(t *testing.T) {
	types := map[string]provider.FieldType{"serial": provider.FieldTypeString}
	err := validateFilterTypes([]provider.Filter{{Field: "serial", Operator: "empty"}}, types)
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

// The service has no escape syntax. Measured, not assumed: a backslash is
// matched literally rather than consumed, so `COR\E-N95%` returns nothing where
// Postgres escape semantics would have matched CORE-N9504-01. A % or _ in the
// user's own value therefore silently widens the population, and a count or
// alert query evaluates that with no rows on screen to reveal it.
func TestTextFilterRefusesWildcardsItCannotEscape(t *testing.T) {
	for _, op := range []string{"ic", "ie", "isw", "iew"} {
		for _, v := range []string{"A_B", "50%", "rack_1"} {
			_, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: op, Value: v}})
			if err == nil {
				t.Errorf("%s %q was accepted; it would match more rows than asked for", op, v)
				continue
			}
			if !strings.Contains(err.Error(), "wildcard") {
				t.Errorf("%s %q: the reason should name the problem, got %v", op, v, err)
			}
		}
	}

	// Ordinary values are untouched, and equality is unaffected: it is not a
	// pattern match, so a % or _ there means itself.
	q, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: "ic", Value: "CORE"}})
	if err != nil {
		t.Fatalf("an ordinary text match must still work: %v", err)
	}
	if got := q.Get(param("name", "ilike")); got != "%CORE%" {
		t.Errorf("pattern = %q, want %%CORE%%", got)
	}
	if _, err := buildFilterValues([]provider.Filter{{Field: "name", Operator: "", Value: "rack_1"}}); err != nil {
		t.Errorf("equality is not a pattern match and must accept it: %v", err)
	}
}
