package replicacache

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The operator vocabulary in provider.Filter is NetBox's set of DRF lookups.
// replica-cache implements a much smaller one — eq, in, isnull, ilike, gt, lt;
// on a build with DATA-408 the anchored text matches istartswith, iendswith
// and iexact; on a build with DATA-417 host, on IP address columns — and its
// own documentation is explicit that gte/lte are absent. Which of them a column takes is the catalogue's word, per
// column, and nothing is offered or sent that it does not list.
//
// Rather than widen the seam's vocabulary for the smaller backend, this file
// translates what it can and REFUSES what it cannot. Refusing matters more than
// translating: every unsupported lookup here has a plausible-looking near-miss
// (gte as gt, n as a negated eq the API cannot express), and silently
// substituting one would answer a different question than the panel asked while
// looking entirely healthy. A translation is made only where it is exact:
// gte/lte on number columns with a fixed step (comparisons.go), and the
// negations, which are answered here (negations.go) rather than sent. FilterFields advertises only the operators below,
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

// wireTextOperator is the service's name for each of the seam's text matches.
// ilike is a case-insensitive CONTAINS on the literal value (measured:
// ilike=core matched CORE-N9504-01, while %core% matched nothing); the other
// three are DATA-408's anchored and whole-value matches, literal and
// case-insensitive in the same way. Every value goes as written: there is
// nothing to wrap and nothing to escape.
var wireTextOperator = map[string]string{
	opIContns: "ilike",
	opIStarts: "istartswith",
	opIEnds:   "iendswith",
	opIExact:  "iexact",
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
func buildFilterValues(filters []provider.Filter, isAddress func(field string) bool) (url.Values, error) {
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
		case opIContns, opIStarts, opIEnds, opIExact:
			if len(values) > 1 {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "this backend cannot combine several values for a text match; select one value or filter on equality"}
			}
			wire := wireTextOperator[op]
			key := f.Field + "|" + wire
			if seen[key] {
				return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: "the same text match is applied twice to this field; this backend cannot combine them, so remove one"}
			}
			seen[key] = true
			q.Set(param(f.Field, wire), values[0])
		case "n", "nic", "nisw", "niew", "nie":
			// Query answers these itself (negations.go) once it knows the
			// columns; one that reaches here is on an entity with none yet.
			return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "a negated filter works once this replica has received data for this object type"}
		case opGTE, opLTE:
			// rewriteComparisons turns these into gt/lt wherever it can, which
			// needs the column's type; validateFilters refuses them on any other
			// column, so one that reaches here is on an entity the replica has no
			// columns for yet.
			return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "greater/less than or equal works on number columns once this replica has received data for this object type"}
		default:
			return nil, &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "this backend supports only equality, text match, greater/less than (or equal, on number columns) and is-empty"}
		}
	}

	for _, field := range exactOrder {
		values := exactValues[field]
		if isAddress != nil && isAddress(field) {
			if err := setAddressMatch(q, field, values); err != nil {
				return nil, err
			}
			continue
		}
		setEquality(q, field, values)
	}
	return q, nil
}

// setEquality writes an exact filter: eq for one value, and `in` for the
// comma-separated union of several, whether they arrived as one multi-value
// variable or as several rows.
func setEquality(q url.Values, field string, values []string) {
	if len(values) == 1 {
		q.Set(param(field, "eq"), values[0])
		return
	}
	q.Set(param(field, "in"), strings.Join(values, ","))
}

// setAddressMatch writes an exact filter on an IP address column the way
// NetBox matches one (ipam's net_in lookup). A value without a mask matches
// every record with that host, whatever its mask: the replica's host operator.
// A value with a mask matches that exact address: equality on the stored text,
// written as PostgreSQL prints an inet (see inetText). Equality on the bare
// text instead — what this sent before — finds nothing for "10.0.0.1" against a
// stored "10.0.0.1/24", and looks healthy doing it.
//
// NetBox compares HOST(address) with the text, so a value that is no address,
// or carries a zone, matches nothing there. host would refuse the whole list
// for one such value, so it is left out; with nothing left, the values go as
// equality, which still matches nothing rather than dropping the filter.
//
// NetBox ORs masked and bare values. When every masked value's address is
// among the bare ones, the OR is the host match alone — each record a masked
// value matches has that host — so they merge. Otherwise the replica, which has
// no OR, cannot express it, and the filter is refused rather than narrowed.
func setAddressMatch(q url.Values, field string, values []string) error {
	var hosts, exact []string
	bare := map[netip.Addr]bool{}
	var maskedAddrs []netip.Addr
	for _, v := range values {
		if a, err := netip.ParseAddr(v); err == nil {
			if a.Zone() == "" && !bare[a] {
				bare[a] = true
				hosts = append(hosts, pgAddrText(a))
			}
			continue
		}
		if pfx, err := netip.ParsePrefix(v); err == nil {
			exact = append(exact, inetText(pfx))
			maskedAddrs = append(maskedAddrs, pfx.Addr())
		}
	}
	covered := true
	for _, a := range maskedAddrs {
		covered = covered && bare[a]
	}
	switch {
	case len(hosts) > 0 && len(exact) > 0 && !covered:
		return &UnsupportedFilterError{Field: field, Operator: "exact",
			Reason: "it mixes addresses with and without a mask. NetBox matches the first exactly and the second by host, whatever its mask, and returns either; this backend cannot combine the two, so write every value with a mask or every value without one"}
	case len(hosts) > 0:
		q.Set(param(field, "host"), strings.Join(hosts, ","))
	case len(exact) > 0:
		setEquality(q, field, exact)
	default:
		setEquality(q, field, values)
	}
	return nil
}

// inetText is an address with its mask as PostgreSQL prints an inet: the mask
// is left out when it covers a single host, and the address is written as
// PostgreSQL writes it (pgAddrText). The host bits are kept — an inet is an
// address in its network, not the network.
func inetText(p netip.Prefix) string {
	if p.Bits() == p.Addr().BitLen() {
		return pgAddrText(p.Addr())
	}
	return pgAddrText(p.Addr()) + "/" + strconv.Itoa(p.Bits())
}

// pgAddrText is an address as PostgreSQL prints it. It differs from netip's
// rendering in one class, the rule PostgreSQL's inet output inherits from
// BIND: an IPv4-compatible IPv6 address — the first 96 bits zero and the
// seventh 16-bit word not — prints its low 32 bits dotted ("::10.0.0.1"),
// where netip prints hex ("::a00:1"). Compared over 25,000 random addresses
// and prefixes against PostgreSQL 16, that is the only difference.
func pgAddrText(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		b := a.As16()
		if b == [16]byte{12: b[12], 13: b[13], 14: b[14], 15: b[15]} && (b[12] != 0 || b[13] != 0) {
			return "::" + netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}).String()
		}
	}
	return a.String()
}

// withSingleHostMask shows a stored IP address the way NetBox shows it: the
// replica keeps PostgreSQL's inet text, which leaves out a single-host mask
// ("10.0.0.1" for 10.0.0.1/32), and NetBox's API always writes one. Shown bare,
// a value picked from a row or the value list read as a bare address and was
// matched by host — broader than the record picked. A value that is not an
// address is returned as it is.
func withSingleHostMask(s string) string {
	if strings.Contains(s, "/") {
		return s
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return s + "/" + strconv.Itoa(a.BitLen())
	}
	return s
}

// showSingleHostMasks applies withSingleHostMask to every IP address column
// of the rows: the columns isAddress names, stored or expanded.
func showSingleHostMasks(rows []map[string]interface{}, cols []string, isAddress func(field string) bool) {
	var addressCols []string
	for _, col := range cols {
		if isAddress(col) {
			addressCols = append(addressCols, col)
		}
	}
	for _, row := range rows {
		for _, col := range addressCols {
			if s, ok := row[col].(string); ok {
				row[col] = withSingleHostMask(s)
			}
		}
	}
}

// addressField reports, for an entity, whether an exact filter on a field
// compares IP addresses by NetBox's rule: whether the column it filters on —
// the stored column, or an expanded name's target column — lists the
// replica's host operator. The catalogue decides, not the column's name; a
// build without host keeps equality on the text.
func addressField(e entity, c *catalog) func(field string) bool {
	return func(field string) bool {
		if col, ok := e.column(field); ok {
			return slices.Contains(col.Operators, "host")
		}
		if via, target, ok := e.expandedColumn(field); ok && via.Ref.Available {
			return slices.Contains(targetColumn(c, via.Ref, target).Operators, "host")
		}
		return false
	}
}

// param builds the service's filter parameter name. The brackets are literal
// here; url.Values.Encode percent-encodes them on the way out, which the
// service requires.
func param(field, op string) string {
	return "filter[" + field + "]__" + op
}

// textMatchReason is why an anchored or whole-value text match is refused on
// a replica whose catalogue lists ilike alone: its one text match is a
// contains, and every near-miss (send the value and hope) matches MORE rows
// than asked for while looking healthy. A build that lists istartswith,
// iendswith and iexact (DATA-408) never gets here.
const textMatchReason = "this replica's only text match is a case-insensitive contains: it cannot anchor a match to the start or end of a value, nor compare whole values case-insensitively (a newer replica-cache build adds those). Use contains, equality (which is case-sensitive), or a datasource in NetBox mode"

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
// values); each text match is offered exactly when the catalogue lists its
// wire name (ilike is the contains match and nothing else; the anchored
// matches arrive with DATA-408); isnull becomes empty/nempty only where
// "empty" and NULL coincide — a nullable non-text column. On text the two
// diverge (see emptyOnTextReason), and on a NOT NULL column nothing is ever
// empty.
func seamOperators(col column) []string {
	var out []string
	has := func(op string) bool { return slices.Contains(col.Operators, op) }
	// Each match is followed by its negation (negations.go) where the rows can
	// be tested as the replica compares (rowComparable), except "not equal" on
	// an IP address column, which NetBox matches by host.
	negatable := rowComparable(col.Type)
	if has("eq") || has("in") {
		out = append(out, opExact)
		if negatable && !has("host") {
			out = append(out, "n")
		}
	}
	for _, op := range []string{opIContns, opIStarts, opIEnds, opIExact} {
		if has(wireTextOperator[op]) {
			out = append(out, op)
			if negatable {
				out = append(out, "n"+op)
			}
		}
	}
	_, _, _, stepped := stepOf(col.Type)
	if has("gt") {
		out = append(out, opGT)
		if stepped {
			out = append(out, opGTE) // see rewriteComparisons
		}
	}
	if has("lt") {
		out = append(out, opLT)
		if stepped {
			out = append(out, opLTE)
		}
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
		op := f.Operator
		if op == "exact" {
			op = opExact
		}
		// A row with an operator and no value emits no parameter (as in
		// NetBox mode; see buildFilterValues), so it is not judged either: a
		// saved query carrying one used to run.
		if op != opEmpty && op != opNEmpty && len(splitValues(f.Value)) == 0 {
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
		// An anchored match on a text column of a replica that lists only
		// ilike gets the reason that names what does work, not the generic
		// "does not take that operator".
		if wire, anchored := wireTextOperator[op]; anchored && op != opIContns &&
			slices.Contains(col.Operators, "ilike") && !slices.Contains(col.Operators, wire) {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator, Reason: textMatchReason}
		}
		if op == opEmpty || op == opNEmpty {
			if fieldTypeOf(col.Type) == provider.FieldTypeString {
				return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator, Reason: emptyOnTextReason(f.Field)}
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
// The stand-in takes the text matches the BUILD offers (see offersOperator),
// so it neither withholds an anchored match a DATA-408 build has nor sends
// one to a build that lacks it.
func targetColumn(c *catalog, ref *reference, name string) column {
	if c != nil {
		if t, ok := c.Entities[strings.TrimPrefix(ref.Path, "/v1/")]; ok {
			if col, ok := t.column(name); ok {
				return col
			}
		}
	}
	ops := []string{"eq", "gt", "lt", "in", "isnull", "ilike"}
	for _, op := range []string{opIStarts, opIEnds, opIExact} {
		if c.offersOperator(wireTextOperator[op]) {
			ops = append(ops, wireTextOperator[op])
		}
	}
	return column{Name: name, Type: "VARCHAR", Nullable: true, Operators: ops}
}

// offersOperator reports whether this replica-cache BUILD has a filter
// operator: the catalogue is derived from the handler's own operator specs,
// so an operator listed on any column anywhere is one the handler knows, and
// one listed nowhere is one it refuses. It stands in for a per-column answer
// where there is no column to ask — an entity not yet fed has none — and
// only the text matches are ever asked about.
func (c *catalog) offersOperator(wire string) bool {
	if c == nil {
		return false
	}
	for _, e := range c.Entities {
		for _, col := range e.Columns {
			if slices.Contains(col.Operators, wire) {
				return true
			}
		}
	}
	return false
}

// validateTextOperators is the part of validateFilters that needs no column:
// an anchored text match on a build whose catalogue lists it nowhere is
// refused before sending, with the reason that names what does work. It runs
// for an entity the catalogue has as unfed — validateFilters cannot, that
// entry has no columns — because the vocabulary is the build's, not the
// entity's, and the request would otherwise go out to be answered with a bare
// HTTP 400 the moment the entity is fed.
func validateTextOperators(filters []provider.Filter, c *catalog) error {
	for _, f := range filters {
		op := f.Operator
		wire, isText := wireTextOperator[op]
		if f.Field == "" || !isText || op == opIContns || len(splitValues(f.Value)) == 0 {
			continue
		}
		if !c.offersOperator(wire) {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator, Reason: textMatchReason}
		}
	}
	return nil
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
func emptyOnTextReason(field string) string {
	return fmt.Sprintf("this backend answers \"is empty\" on a text column with IS NULL, but NetBox stores a blank %s as \"\", not NULL, so the filter would keep the blanks and drop only the nulls — the opposite of what was asked. There is no equivalent here; use a datasource in NetBox mode for this filter", field)
}
