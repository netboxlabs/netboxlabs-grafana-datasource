package replicacache

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The operator vocabulary in provider.Filter is NetBox's set of DRF lookups.
// replica-cache implements a much smaller one — eq, in, isnull, ilike, gt, lt —
// and its own documentation is explicit that gte/lte are absent.
//
// Rather than widen the seam's vocabulary for the smaller backend, this file
// translates what it can and REFUSES what it cannot. Refusing matters more than
// translating: every unsupported lookup here has a plausible-looking near-miss
// (gte as gt, n as a negated eq the API cannot express), and silently
// substituting one would answer a different question than the panel asked while
// looking entirely healthy. FilterFields advertises only the operators below,
// so the query editor never offers a combination that lands here as an error.
const (
	opExact   = ""
	opIExact  = "ie"
	opIContns = "ic"
	opIStarts = "isw"
	opIEnds   = "iew"
	opGT      = "gt"
	opLT      = "lt"
	opEmpty   = "empty"
	opNEmpty  = "nempty"
)

// supportedOperators is the set FilterFields advertises, in the editor's
// canonical display order.
var supportedOperators = []string{
	opExact, opIExact, opIContns, opIStarts, opIEnds, opGT, opLT, opEmpty, opNEmpty,
}

// UnsupportedFilterError is a filter the backend cannot express. It is the
// user's input rather than an upstream failure, so it classifies as a bad
// request: the query editor is where it gets fixed.
type UnsupportedFilterError struct {
	Field    string
	Operator string
	Reason   string
}

func (e *UnsupportedFilterError) Error() string {
	return fmt.Sprintf("filter %s with operator %q is not supported by replica-cache: %s",
		e.Field, e.Operator, e.Reason)
}

// Classification reports the filter as a bad request rather than an outage.
func (e *UnsupportedFilterError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{Kind: provider.ErrorKindBadRequest}
}

// buildFilterValues translates provider filters into replica-cache query
// parameters, or fails.
//
// Multi-value handling is the subtle part. A Grafana multi-value variable
// arrives as one comma-separated Value, and the NetBox provider expands it into
// repeated params, which DRF ORs together. replica-cache has a direct
// equivalent for equality — the `in` operator — but none for the substring
// family: there is no way to ask it for "name contains A OR name contains B".
// So a multi-value substring filter is refused rather than approximated by
// picking one value, which would silently narrow a dashboard's variable to its
// first selection.
func buildFilterValues(filters []provider.Filter) (url.Values, error) {
	q := url.Values{}
	for _, f := range filters {
		if f.Field == "" {
			continue
		}
		op := f.Operator
		if op == "exact" {
			op = opExact
		}

		// The empty family is a predicate on the column, not a comparison, so it
		// ignores Value entirely. Set rather than Add: two empty-family rows on
		// one field contradict each other, and the last one winning matches how
		// the NetBox provider resolves the same conflict.
		if op == opEmpty || op == opNEmpty {
			q.Set(param(f.Field, "isnull"), boolString(op == opEmpty))
			continue
		}

		values := splitValues(f.Value)
		if len(values) == 0 {
			continue
		}

		switch op {
		case opExact:
			if len(values) == 1 {
				q.Set(param(f.Field, "eq"), values[0])
			} else {
				// `in` takes the comma-separated list the variable already gave us.
				q.Set(param(f.Field, "in"), strings.Join(values, ","))
			}
		case opGT, opLT:
			if len(values) > 1 {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "a comparison accepts a single value, but several were given"}
			}
			q.Set(param(f.Field, op), values[0])
		case opIExact, opIContns, opIStarts, opIEnds:
			if len(values) > 1 {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "this backend cannot combine several values for a text match; select one value or filter on equality"}
			}
			q.Set(param(f.Field, "ilike"), likePattern(op, values[0]))
		default:
			return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "this backend supports only equality, text match, greater/less than and is-empty"}
		}
	}
	return q, nil
}

// param builds the service's filter parameter name. The brackets are literal
// here; url.Values.Encode percent-encodes them on the way out, which the
// service requires.
func param(field, op string) string {
	return "filter[" + field + "]__" + op
}

// likePattern wraps a value in the SQL LIKE wildcards that turn `ilike` into
// the requested match.
//
// Note on wildcards in user input: a value containing % or _ is passed through
// unescaped, so "50%" as a contains-match also matches strings that merely
// start with "50". The service documents no escape syntax, and inventing one
// that it may not honour would risk the opposite error — a pattern that matches
// nothing, reported as an empty result. Over-matching is at least visible in
// the rows returned.
func likePattern(op, v string) string {
	switch op {
	case opIContns:
		return "%" + v + "%"
	case opIStarts:
		return v + "%"
	case opIEnds:
		return "%" + v
	default: // opIExact: no wildcards, so ilike is case-insensitive equality
		return v
	}
}

func splitValues(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// filterFieldsFor advertises every discovered column with the operator set the
// backend honours.
//
// The operator set does not vary by column. That is a property of the service,
// not an assumption: it validates the operator against a fixed list and the
// column against the table's schema, and reports the two failures separately
// ("invalid filter operator: regex" versus "unknown column: nope"). So there is
// nothing per-column to discover, and offering the same list everywhere is
// accurate rather than a simplification.
func filterFieldsFor(fields []provider.Field) []provider.FilterField {
	out := make([]provider.FilterField, 0, len(fields))
	for _, f := range fields {
		out = append(out, provider.FilterField{Name: f.Name, Operators: supportedOperators})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
