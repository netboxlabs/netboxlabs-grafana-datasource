package replicacache

import (
	"fmt"
	"net/url"
	"slices"
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

// Classification reports the filter as an unsupported capability, carrying the
// reason with it.
//
// Not a bare bad request: that kind renders as the generic "NetBox rejected
// this query (HTTP 400)", which loses the only useful part of this error and
// blames NetBox for a request that never left this process. The unsupported
// kind carries Detail, which the plugin layer prefers over its own wording —
// exactly so a provider can explain a limit only it understands. Both still
// answer as StatusBadRequest, so nothing about retry behaviour changes.
func (e *UnsupportedFilterError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{
		Kind:   provider.ErrorKindUnsupported,
		Detail: e.Error(),
	}
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

	// Exact filters are accumulated per field rather than written as they are
	// seen. Two rows on one field mean OR — the NetBox provider encodes that as
	// repeated params, which DRF unions — and writing each one in turn would let
	// the last overwrite the rest, silently narrowing "status is active or
	// planned" to "status is planned". replica-cache expresses the same thing
	// with `in`, so the values are collected and emitted once.
	exactValues := map[string][]string{}
	var exactOrder []string

	// Non-exact operators cannot be unioned, so a repeated one on the same field
	// is refused rather than resolved by last-write-wins. Different operators on
	// one field are fine and compose (gt with lt is a range).
	seen := map[string]bool{}

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
			if _, ok := exactValues[f.Field]; !ok {
				exactOrder = append(exactOrder, f.Field)
			}
			exactValues[f.Field] = append(exactValues[f.Field], values...)
		case opGT, opLT:
			if len(values) > 1 {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "a comparison accepts a single value, but several were given"}
			}
			key := f.Field + "|" + op
			if seen[key] {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "the same comparison is applied twice to this field; this backend cannot combine them, so remove one"}
			}
			seen[key] = true
			q.Set(param(f.Field, op), values[0])
		case opIExact, opIContns, opIStarts, opIEnds:
			if len(values) > 1 {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "this backend cannot combine several values for a text match; select one value or filter on equality"}
			}
			if i := strings.IndexAny(values[0], "%_"); i >= 0 {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: fmt.Sprintf("the value contains %q, which this backend treats as a wildcard rather than as itself, so the filter would match more rows than were asked for. It has no escape syntax — a backslash is matched literally rather than consumed — so the wider result cannot be avoided, and a count or alert query would evaluate it with no rows on screen to reveal the mismatch. Filter on equality, or use a datasource in NetBox mode, which escapes it", string(values[0][i]))}
			}
			key := f.Field + "|ilike"
			if seen[key] {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "two text matches are applied to this field; this backend cannot combine them, so remove one"}
			}
			seen[key] = true
			q.Set(param(f.Field, "ilike"), likePattern(op, values[0]))
		default:
			return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "this backend supports only equality, text match, greater/less than and is-empty"}
		}
	}

	for _, field := range exactOrder {
		values := exactValues[field]
		if len(values) == 1 {
			q.Set(param(field, "eq"), values[0])
			continue
		}
		// `in` takes the comma-separated union of every exact value asked for,
		// whether they arrived as one multi-value variable or as several rows.
		q.Set(param(field, "in"), strings.Join(values, ","))
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
// start with "50".
//
// A value carrying % or _ of its own is refused before it gets here (see
// wildcardError): the service has no escape syntax, which was measured rather
// than assumed. Backslash is matched LITERALLY, not consumed as an escape —
// `COR\E-N95%` returns nothing where Postgres escape semantics would have
// matched CORE-N9504-01 — so there is no way to ask for a literal wildcard.
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

// seamOperators translates a catalogue column's operators into the editor's
// tokens. eq and in collapse to "" (the editor sends a CSV for several
// values); ilike fans out to the four text matches; isnull becomes
// empty/nempty only where "empty" and NULL coincide — a nullable non-text
// column. On text the two diverge (see emptyOnTextReason), and on a NOT NULL
// column nothing is ever empty.
func seamOperators(col column) []string {
	var out []string
	has := func(op string) bool { return slices.Contains(col.Operators, op) }
	if has("eq") || has("in") {
		out = append(out, opExact)
	}
	if has("ilike") {
		out = append(out, opIExact, opIContns, opIStarts, opIEnds)
	}
	if has("gt") {
		out = append(out, opGT)
	}
	if has("lt") {
		out = append(out, opLT)
	}
	if has("isnull") && col.Nullable && fieldTypeOf(col.Type) != provider.FieldTypeString {
		out = append(out, opEmpty, opNEmpty)
	}
	return out
}

// validateFilters refuses, before anything is sent, a filter the catalogue
// says the backend cannot honour: an unknown column, an operator the column
// does not take, an expansion whose target has no data, a cf_* column (not a
// stored column at all). FilterFields already hides those combinations from
// the editor, but a saved dashboard predates the editor's current answer and
// a provisioned one never consulted it — and "the request will tell us" only
// holds when a wrong request produces an error. It does not here: ILIKE on a
// non-text column was measured answering both ways on the same column,
//
//	filter[id]__ilike=ZZZZ  -> HTTP 500 {"error":"count query failed"}
//	filter[id]__ilike=%1%   -> HTTP 200, count 6824570 — the UNFILTERED total
//
// and the second is a panel quietly showing the whole fleet while claiming to
// show a subset.
func validateFilters(filters []provider.Filter, e entity, c *catalog) error {
	for _, f := range filters {
		if f.Field == "" {
			continue
		}
		var col column
		if own, ok := e.column(f.Field); ok {
			col = own
		} else if via, target, ok := e.expandedColumn(f.Field); ok {
			if !via.Ref.Available {
				return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: fmt.Sprintf("%s cannot be resolved on this replica: %s has received no data, so nothing can be filtered by it; %s still holds the raw id",
						f.Field, strings.TrimPrefix(via.Ref.Path, "/v1/"), via.Name)}
			}
			col = targetColumn(c, via.Ref, target)
		} else {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator, Reason: "this replica has no such column to filter on"}
		}
		op := f.Operator
		if op == "exact" {
			op = opExact
		}
		if op == opEmpty || op == opNEmpty {
			if fieldTypeOf(col.Type) == provider.FieldTypeString {
				return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator, Reason: emptyOnTextReason(col)}
			}
			if !col.Nullable {
				return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator, Reason: "that column is NOT NULL on this replica, so nothing in it is ever empty"}
			}
		}
		if !slices.Contains(seamOperators(col), op) {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: fmt.Sprintf("the backend does not take that operator on a %s column", strings.ToLower(col.Type))}
		}
	}
	return nil
}

// targetColumn is the column an expanded name filters and sorts on: the
// target entity's own column when the catalogue has it (it lists every served
// entity), else a text stand-in — every declared target column (name, slug,
// label, address, prefix, cid, model, mac_address, ssid) is text in NetBox.
func targetColumn(c *catalog, ref *reference, name string) column {
	if c != nil {
		if t, ok := c.Entities[strings.TrimPrefix(ref.Path, "/v1/")]; ok {
			if col, ok := t.column(name); ok {
				return col
			}
		}
	}
	return column{Name: name, Type: "VARCHAR", Nullable: true, Operators: []string{"eq", "gt", "lt", "in", "isnull", "ilike"}}
}

// emptyOnTextReason is the DATA-206 refusal, stated from the catalogue: the
// column is nullable text, NetBox stores a blank as "" rather than NULL, and
// the backend's only test is IS NULL — so "is empty" answers the inverse of
// the population it names. Measured on 6.8M devices:
//
//	filter[serial]__isnull=true   -> 0          (what "is empty" would send)
//	filter[serial]__eq=           -> 6,824,570  (what NetBox means by empty)
//	filter[serial]__isnull=false  -> 6,824,570  (our "has any value")
//
// Neither can be expressed here: "is empty" needs `isnull OR eq ""` and the
// backend ANDs its filters with no OR; "has any value" needs a negation it
// does not have. Equality with an empty value is NOT the advice: buildFilterValues
// drops it, as NetBox mode does, so following it returns every row.
func emptyOnTextReason(col column) string {
	return fmt.Sprintf("this backend answers \"is empty\" on a text column with IS NULL, but NetBox stores a blank %s as \"\", not NULL, so the filter would keep the blanks and drop only the nulls — the opposite of what was asked. There is no equivalent here; use a datasource in NetBox mode for this filter", col.Name)
}
