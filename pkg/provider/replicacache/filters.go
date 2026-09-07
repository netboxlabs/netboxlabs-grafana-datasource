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

// nullOperators ask whether a column has a value. They compile to SQL IS NULL,
// which is the wrong question for a TEXT column.
//
// NetBox stores a blank character field as "" rather than NULL, so the two
// answers invert. Measured against a staging instance where every device has a
// blank serial:
//
//	filter[serial]__isnull=true   -> 0          (what we send for "is empty")
//	filter[serial]__eq=           -> 6,824,570  (what NetBox means by empty)
//	filter[serial]__isnull=false  -> 6,824,570  (our "has any value")
//
// So "is empty" matched nothing where every row qualified, and "has any value"
// matched everything where none did — a panel switched from NetBox mode to this
// one silently inverts, with no error.
//
// Neither can be expressed correctly here. "Is empty" would need `isnull OR
// eq ""`, and this backend ANDs its filters with no OR; `eq ""` alone is right
// only for a column that is NOT NULL, which we cannot know without a schema
// endpoint (DATA-206). "Has any value" needs a negation operator the backend
// does not have at all. So they are withheld for text and kept for everything
// else, where blank-string semantics do not arise.
var nullOperators = map[string]bool{
	opEmpty: true, opNEmpty: true,
}

// blankSafe reports whether a column's type is one where blank genuinely means
// NULL, so IS NULL answers the question asked.
//
// It requires a CONFIRMED type. An unknown type is not "not text": a nullable
// text column that happened to be NULL in every sampled row is exactly the case
// that cannot be typed, and it is also exactly the case where IS NULL would
// later miss the "" rows. Treating unknown as safe reproduced the inversion
// this gate exists to prevent, on the columns most likely to hit it.
func blankSafe(name string, t provider.FieldType) bool {
	switch t {
	case provider.FieldTypeNumber, provider.FieldTypeBoolean:
		return true
	case provider.FieldTypeTime:
		// A timestamp gets is-empty only when the NAME agrees as well. The type
		// is inferred from values, and values alone can be wrong in the one
		// direction that matters here: a text column whose sampled values all
		// look like RFC3339 — contrived for NetBox's own models, but a plugin
		// can define anything — would otherwise be offered an operator this
		// backend answers with IS NULL, while blank text is stored as "", so
		// the filter returns the exact opposite population and looks healthy.
		//
		// Corroboration is required only for this operator, not for the type.
		// Value-led typing still suppresses ILIKE on the eleven NetBox
		// date-time columns no naming rule finds — that is what it was for —
		// and this withholds the one operator whose failure is silent and
		// inverted rather than visible.
		return isTimeColumn(name)
	}
	return false
}

// textOperators need the column to hold text. They compile to SQL ILIKE, which
// the backend applies without checking the column's type.
var textOperators = map[string]bool{
	opIExact: true, opIContns: true, opIStarts: true, opIEnds: true,
}

// filterFieldsFor advertises the columns that can actually be filtered, each
// with the operators that are safe for its type.
//
// Two restrictions, both of which produce a broken query if skipped.
//
// Only columns that exist UPSTREAM are offered. The resolved names (site) and
// custom fields (cf_*) are built here, not stored there, so filtering on one is
// rejected as an unknown column.
//
// Text operators are offered only for a column confirmed to hold text. Operator
// validity does vary by column, which is not obvious from how the service
// reports errors: it validates the operator against a fixed list and the column
// against the schema, and reports those two clearly and separately. Type
// compatibility is a third check that does not happen, and ILIKE against a
// non-text column was measured failing two different ways on the same column:
//
//	filter[id]__ilike=ZZZZ  -> HTTP 500 {"error":"count query failed"}
//	filter[id]__ilike=%1%   -> HTTP 200, count 6824570 — the UNFILTERED total
//
// The second is why this gate exists. A 500 is at least visible; a filter that
// returns every row under a healthy status code is a panel quietly showing the
// whole fleet while claiming to show a subset.
//
// A column never seen holding a value cannot be typed and is treated as
// non-text. Withholding an operator costs a dropdown entry; offering one that
// silently matches everything costs a wrong answer.
func filterFieldsFor(fields []provider.Field, raw map[string]bool, types map[string]provider.FieldType) []provider.FilterField {
	out := make([]provider.FilterField, 0, len(fields))
	for _, f := range fields {
		if len(raw) > 0 && !raw[f.Name] {
			continue
		}
		isText := types[f.Name] == provider.FieldTypeString
		ops := make([]string, 0, len(supportedOperators))
		for _, op := range supportedOperators {
			if textOperators[op] && !isText {
				continue
			}
			// The mirror of the rule above: a text match needs text, and an
			// is-empty check needs a column where blank means NULL.
			if nullOperators[op] && !blankSafe(f.Name, types[f.Name]) {
				continue
			}
			ops = append(ops, op)
		}
		out = append(out, provider.FilterField{Name: f.Name, Operators: ops})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// needsTypeCheck reports whether any filter uses an operator whose validity
// depends on the column's type. It exists so a query with no text match never
// pays for a schema sample — notably the count-only path, whose whole point is
// to skip work it will not read.
func needsTypeCheck(filters []provider.Filter) bool {
	for _, f := range filters {
		op := f.Operator
		if op == "exact" {
			op = opExact
		}
		if textOperators[op] || nullOperators[op] {
			return true
		}
	}
	return false
}

// validateFilterTypes refuses a filter whose operator is unsafe for the
// column's type, before any request is built.
//
// FilterFields already hides those combinations from the editor, but a saved
// dashboard predates the editor's current answer and a provisioned one never
// consulted it. Sending it anyway is what produces the silent unfiltered
// result above, so it is refused here as well.
func validateFilterTypes(filters []provider.Filter, types map[string]provider.FieldType) error {
	for _, f := range filters {
		op := f.Operator
		if op == "exact" {
			op = opExact
		}
		// Only these operators care about the column's type. Equality, `in` and
		// the comparisons work on anything, so they are never held up by a
		// schema we could not read.
		if !textOperators[op] && !nullOperators[op] {
			continue
		}

		// For the ones that do care, fail CLOSED when the type cannot be
		// confirmed — whether the schema sample failed outright (types nil) or
		// this column was never seen holding a value.
		//
		// The earlier version let those through, reasoning that the request
		// itself was authoritative. That is wrong for exactly this family: a
		// text match against a numeric column does not fail, it answers HTTP
		// 200 over the unfiltered population. "The request will tell us" only
		// holds when a wrong request produces an error.
		t, seen := types[f.Field]
		if !seen || t == provider.FieldType("") {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "this column's type could not be determined, and this operator is only safe on some types — a text match on a numeric column returns every row while appearing to filter. Retry once the object type's fields have loaded, or filter on equality"}
		}
		if textOperators[op] && t != provider.FieldTypeString {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "a text match needs a text column, and this backend applies it to any column without checking — returning either a server error or, worse, every row unfiltered"}
		}
		if nullOperators[op] && !blankSafe(f.Field, t) {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "this backend answers is-empty with IS NULL, but NetBox stores a blank text field as an empty string, so the result would be the exact opposite of what was asked. There is no equivalent here — an equality filter with an empty value is dropped, as it is in NetBox mode, so it would return every row — use NetBox mode for this filter"}
		}
	}
	return nil
}
