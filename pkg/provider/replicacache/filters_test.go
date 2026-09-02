package replicacache

import (
	"errors"
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
			if c := unsupported.Classification(); c.Kind != provider.ErrorKindBadRequest {
				t.Errorf("want a bad-request classification so the editor is blamed, not the network; got %q", c.Kind)
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
	got := filterFieldsFor([]provider.Field{{Name: "name"}, {Name: "id"}})
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
