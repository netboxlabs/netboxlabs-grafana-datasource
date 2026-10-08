package replicacache

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// racksFixture is a replica table with a whole-number column, a DECIMAL one, a
// floating-point one and text, so each comparison rule has a column to act on.
func racksFixture(t *testing.T) (*fakeService, *Provider) {
	t.Helper()
	f := newFakeService()
	f.addEntity("dcim/racks", "id:BIGINT:pk", "name:VARCHAR", "u_height:SMALLINT", "weight:DECIMAL(8,2)", "ratio:DOUBLE")
	f.entities["dcim/racks"] = []map[string]interface{}{
		{"id": 1, "name": "R1", "u_height": 40, "weight": 10.00, "ratio": 0.5},
		{"id": 2, "name": "R2", "u_height": 42, "weight": 10.01, "ratio": 0.5},
		{"id": 3, "name": "R3", "u_height": 44, "weight": 10.5, "ratio": 0.5},
		{"id": 4, "name": "R4", "u_height": 48, "weight": 11, "ratio": 0.5},
		{"id": 5, "name": "R5", "u_height": nil, "weight": nil, "ratio": nil},
	}
	return f, newTestProvider(t, f)
}

func rackNames(res *provider.Result) []string {
	var out []string
	for _, r := range res.Rows {
		out = append(out, r["name"].(string))
	}
	return out
}

// The replica has gt and lt but no gte or lte. On a column whose values are
// whole numbers, or DECIMALs of a known scale, the inclusive comparison has an
// exact strict equivalent: >= 42 is > 41, <= 10.5 is < 10.51. Floating-point
// columns have no such neighbour that survives the service's own conversion,
// and NetBox has none, so they are not offered.
func TestQuery_InclusiveComparisonsBecomeExactStrictOnes(t *testing.T) {
	cases := []struct {
		name    string
		filters []provider.Filter
		sent    map[string]string
		want    []string
	}{
		{"gte on a whole number", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "42"}},
			map[string]string{"filter[u_height]__gt": "41"}, []string{"R2", "R3", "R4"}},
		{"lte on a whole number", []provider.Filter{{Field: "u_height", Operator: "lte", Value: "44"}},
			map[string]string{"filter[u_height]__lt": "45"}, []string{"R1", "R2", "R3"}},
		{"gte with a fraction on a whole number", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "42.5"}},
			map[string]string{"filter[u_height]__gt": "42"}, []string{"R3", "R4"}},
		{"gte below zero", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "-1.5"}},
			map[string]string{"filter[u_height]__gt": "-2"}, []string{"R1", "R2", "R3", "R4"}},
		{"a range", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "42"}, {Field: "u_height", Operator: "lte", Value: "44"}},
			map[string]string{"filter[u_height]__gt": "41", "filter[u_height]__lt": "45"}, []string{"R2", "R3"}},
		{"gte on a decimal", []provider.Filter{{Field: "weight", Operator: "gte", Value: "10.01"}},
			map[string]string{"filter[weight]__gt": "10.00"}, []string{"R2", "R3", "R4"}},
		{"lte on a decimal", []provider.Filter{{Field: "weight", Operator: "lte", Value: "10.5"}},
			map[string]string{"filter[weight]__lt": "10.51"}, []string{"R1", "R2", "R3"}},
		{"gte finer than the scale", []provider.Filter{{Field: "weight", Operator: "gte", Value: "10.005"}},
			map[string]string{"filter[weight]__gt": "10.00"}, []string{"R2", "R3", "R4"}},
		// gt and lt are put on the column's own scale too, so the answer never
		// depends on how the service rounds a finer literal.
		{"gt finer than the scale", []provider.Filter{{Field: "weight", Operator: "gt", Value: "10.005"}},
			map[string]string{"filter[weight]__gt": "10.00"}, []string{"R2", "R3", "R4"}},
		{"lt finer than the scale", []provider.Filter{{Field: "weight", Operator: "lt", Value: "10.505"}},
			map[string]string{"filter[weight]__lt": "10.51"}, []string{"R1", "R2", "R3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, p := racksFixture(t)
			res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/racks", Filters: c.filters})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got := rackNames(res); !slices.Equal(got, c.want) {
				t.Errorf("rows = %v, want %v", got, c.want)
			}
			if res.Total != len(c.want) {
				t.Errorf("Total = %d, want %d", res.Total, len(c.want))
			}
			for key, value := range c.sent {
				req, ok := f.requestWith("dcim/racks", key)
				if !ok || req.query.Get(key) != value {
					t.Errorf("sent %v, want %s=%s", req.query, key, value)
				}
			}
		})
	}
}

func TestQuery_InclusiveComparisonsRefuseWhatHasNoExactRewrite(t *testing.T) {
	cases := []struct {
		name    string
		filters []provider.Filter
		reason  string
	}{
		{"a floating-point column", []provider.Filter{{Field: "ratio", Operator: "gte", Value: "0.5"}}, "does not take that operator"},
		{"a text column", []provider.Filter{{Field: "name", Operator: "lte", Value: "R2"}}, "does not take that operator"},
		{"not a number", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "tall"}}, "takes a number"},
		{"a fraction written as one", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "1/2"}}, "takes a number"},
		{"hexadecimal", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "0x10"}}, "takes a number"},
		{"several values", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "40,42"}}, "single value"},
		{"with gt on the same field", []provider.Filter{{Field: "u_height", Operator: "gt", Value: "40"}, {Field: "u_height", Operator: "gte", Value: "42"}}, "applied twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, p := racksFixture(t)
			_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/racks", Filters: c.filters})
			var unsupported *UnsupportedFilterError
			if !errors.As(err, &unsupported) {
				t.Fatalf("err = %v, want an UnsupportedFilterError", err)
			}
			if !strings.Contains(unsupported.Reason, c.reason) {
				t.Errorf("reason = %q, want it to say %q", unsupported.Reason, c.reason)
			}
			if unsupported.Operator != c.filters[len(c.filters)-1].Operator {
				t.Errorf("the error names operator %q, want the one the panel used", unsupported.Operator)
			}
			if n := f.countRequestsFor("dcim/racks"); n != 0 {
				t.Errorf("%d requests sent for a refused filter", n)
			}
		})
	}
}

// The editor offers >= and <= exactly where the rewrite applies.
func TestFilterFields_OfferInclusiveComparisonsOnWholeAndDecimalColumns(t *testing.T) {
	_, p := racksFixture(t)
	fields, err := p.FilterFields(context.Background(), "dcim/racks")
	if err != nil {
		t.Fatalf("FilterFields: %v", err)
	}
	ops := map[string][]string{}
	for _, f := range fields {
		ops[f.Name] = f.Operators
	}
	for _, col := range []string{"u_height", "weight"} {
		if !slices.Contains(ops[col], "gte") || !slices.Contains(ops[col], "lte") {
			t.Errorf("%s operators = %v, want gte and lte", col, ops[col])
		}
	}
	for _, col := range []string{"ratio", "name"} {
		if slices.Contains(ops[col], "gte") || slices.Contains(ops[col], "lte") {
			t.Errorf("%s operators = %v, want no gte or lte", col, ops[col])
		}
	}
}

// An entity the replica has not received data for has no columns, so there is
// no type to rewrite against. The refusal says so rather than calling >= an
// operator this backend lacks.
func TestQuery_InclusiveComparisonOnAnUnfedEntitySaysWhy(t *testing.T) {
	p := newTestProvider(t, newFakeService())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/platforms",
		Filters: []provider.Filter{{Field: "id", Operator: "gte", Value: "1"}}})
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want an UnsupportedFilterError", err)
	}
	if !strings.Contains(unsupported.Reason, "received data") {
		t.Errorf("reason = %q, want it to name the missing data", unsupported.Reason)
	}
}

// At the edge of what a type holds, the strict equivalent would be a literal
// the type cannot hold. The filter is still exact: "<= the largest value" is
// every row with a value, and ">= past the largest" is no row at all.
func TestQuery_InclusiveComparisonsAtTheTypesLimits(t *testing.T) {
	cases := []struct {
		name    string
		filters []provider.Filter
		want    []string
		sent    map[string]string
	}{
		{"<= the largest SMALLINT is every value", []provider.Filter{{Field: "u_height", Operator: "lte", Value: "32767"}},
			[]string{"R1", "R2", "R3", "R4"}, map[string]string{"filter[u_height]__isnull": "false"}},
		{">= the smallest SMALLINT is every value", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "-40000"}},
			[]string{"R1", "R2", "R3", "R4"}, map[string]string{"filter[u_height]__isnull": "false"}},
		{"<= past the largest DECIMAL(8,2)", []provider.Filter{{Field: "weight", Operator: "lte", Value: "1000000"}},
			[]string{"R1", "R2", "R3", "R4"}, map[string]string{"filter[weight]__isnull": "false"}},
		{">= past the largest SMALLINT is no value", []provider.Filter{{Field: "u_height", Operator: "gte", Value: "40000"}},
			nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, p := racksFixture(t)
			res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/racks", Filters: c.filters})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got := rackNames(res); !slices.Equal(got, c.want) || res.Total != len(c.want) {
				t.Errorf("rows = %v (Total %d), want %v", got, res.Total, c.want)
			}
			for key, value := range c.sent {
				if req, ok := f.requestWith("dcim/racks", key); !ok || req.query.Get(key) != value {
					t.Errorf("sent %v, want %s=%s", req.query, key, value)
				}
			}
			if c.want == nil && f.countRequestsFor("dcim/racks") != 0 {
				t.Error("a filter no row can match still sent a request")
			}
		})
	}
}

// A comparison no row can satisfy answers "no rows", but only once the rest of
// the query is known to be valid: a typo beside it is still an error, not a
// healthy-looking zero an alert would act on.
func TestQuery_UnsatisfiableComparisonStillValidatesTheRest(t *testing.T) {
	_, p := racksFixture(t)
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/racks", Filters: []provider.Filter{
		{Field: "u_height", Operator: "gte", Value: "99999"}, {Field: "nosuchcolumn", Value: "x"}}})
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want the unknown column refused", err)
	}
}
