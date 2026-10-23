package replicacache

import (
	"fmt"
	"slices"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// NetBox's empty lookup (extras/lookups.py, Empty) asks of a text field
// whether it holds nothing: <field>__empty=true is NULL or "", =false is a
// value (CAST(LENGTH(field) AS BOOLEAN) IS [NOT] TRUE). NetBox stores a blank
// text field as "", and one that allows NULL, a device's name, holds both. The
// replica's isnull is another question: measured on v1.40.1, ipam/ip-addresses
// has 23 blank dns_name of 25, and dns_name__isnull=true finds none of them
// (DATA-206 measured 0 of 6,824,570 blank device serials).
//
// What the replica takes is a comparison, and on text it takes an empty value
// (measured on the same rows: dns_name__gt= finds the 2 that have a value, and
// __eq= the 23 that do not; its four text operators refuse an empty value, "a
// text filter takes a non-empty value"). So:
//
//   - has any value is gt "", sent as it is: every non-empty string is greater
//     than "", and NULL is not greater than "" (it compares as unknown), so it
//     is NetBox's empty=false exactly (opWireGTBlank);
//   - is empty is everything else, NULL included: the negation of that same
//     comparison (opNotGTBlank), which negations.go answers as it answers every
//     negation, with Django's exclude() meaning — a row is dropped when its
//     value matches, so NULL is kept — which is NetBox's empty=true. The total
//     comes from counts, count(base) - count(base AND gt ""); the rows from
//     reading those the other filters match and testing each here, with that
//     machinery's limits (10,000 rows read, a warning when the read stopped
//     short, a refusal where the total could come only from reading more).
//
// Nothing depends on the catalogue's nullability: v1.40.1's schema lists every
// text column nullable, a device's serial among them, and a NOT NULL one holds
// "" as well.

// The operators the rewrite writes. Both are this datasource's own (see
// internalOperators), and the names have a space for the reason the null
// operators do: no NetBox lookup does, so no saved query carries one.
const (
	opWireGTBlank = "gt blank"
	opNotGTBlank  = "not gt blank"
)

// emptyNeedsDataReason is the refusal of "is empty" on an entity the replica
// has received nothing for: it is answered from the rows, and an entity with no
// data has neither them nor the columns that say how to test them.
const emptyNeedsDataReason = "is empty on a text column is answered from the rows, which works once this replica has received data for this object type"

// rawEmptinessReason refuses an is-empty or has-any-value row that reached
// validateFilters on a text column. rewriteEmptiness answers every one before,
// so none arrives; were one to, it would be sent as the replica's isnull (IS
// NULL for is empty, IS NOT NULL for has any value), which answers the opposite
// question (see above), so it is refused rather than sent. One sentence says it
// of either operator.
const rawEmptinessReason = "this backend would send is empty and has any value on a text column as IS NULL and IS NOT NULL, which is not what NetBox's empty lookup means, since NetBox keeps a blank as \"\" and not as NULL, so it cannot answer the filter; " + netboxModeAdvice

// rewriteEmptiness puts NetBox's empty lookup on a text column on the
// operators above: has any value becomes opWireGTBlank, is empty opNotGTBlank.
// The row keeps the field and the operator it was given, for a refusal to name:
// the user's, but for NetBox's own spelling (name__empty=true), which
// translateFilters has read as is empty already. The slice it was given is not
// changed, and the rows it does not rewrite stay where they are.
//
// Of several rows on one column the last is the answer. The editor says "only
// the last one is applied" (src/types.ts), NetBox mode sets the one parameter,
// and is-empty on a number does the same here; NetBox's own models are read as
// one filter per name, which keeps the last, in translateFilters. Left to the
// steps after this, an is-empty beside a has-any-value would be answered as
// both, no row, on a plugin's table.
//
// It runs on the filters as translated, before negations are split off, and
// leaves a column that is not text its rows for validateFilters: IS NULL is
// exact there. A table the vocabulary covers is rewritten even with no data yet;
// a plugin's is not (asksTextEmptiness).
func rewriteEmptiness(filters []provider.Filter, e entity, _ *catalog) []provider.Filter {
	var (
		rewritten map[int]provider.Filter
		last      map[string]int
	)
	for i, f := range filters {
		if !asksTextEmptiness(f, e) {
			continue
		}
		if rewritten == nil {
			rewritten, last = map[int]provider.Filter{}, map[string]int{}
		}
		op := opWireGTBlank
		if f.Operator == opEmpty {
			op = opNotGTBlank
		}
		rewritten[i] = provider.Filter{Field: f.Field, Operator: op, Label: fieldLabel(f), LabelOperator: f.Operator}
		last[f.Field] = i
	}
	if rewritten == nil {
		return filters
	}
	out := make([]provider.Filter, 0, len(filters))
	for i, f := range filters {
		r, ok := rewritten[i]
		switch {
		case !ok:
			out = append(out, f)
		case last[f.Field] == i:
			out = append(out, r)
		}
	}
	return out
}

// asksTextEmptiness reports whether an is-empty or has-any-value row is
// NetBox's empty lookup on a text column.
//
// On a table the vocabulary covers, translateFilters has already made NetBox's
// IS NULL on a number or a date into the null operators, so what it leaves as
// one of the two is NetBox's own __empty on text — but for the rows it passes
// through as written (q, tag and tag_id, a custom field), which are no lookup
// at all. The vocabulary is what says the filter is on text, so the column need
// not be stored for that, nor the entity fed: a column the replica lacks is the
// refusal that says so, further on, and a catalogue ten minutes stale, which
// still lists an entity as having no data, cannot send the replica's isnull to
// one that has rows by now. A column the replica stores as something other than
// text keeps its rows.
//
// On a table it does not cover — a plugin's — the catalogue's rules hold: it is
// a text column the replica stores. A name joined in from another table is not.
// Nothing but the columns says a plugin's filter is on text, so until the
// catalogue lists the table as having data the row is left as it is and goes out
// as isnull: the opposite question, if the table has rows by now and the
// catalogue, up to ten minutes old, still lists it as having none. That is the
// limit of the guard above.
func asksTextEmptiness(f provider.Filter, e entity) bool {
	if f.Field == "" || (f.Operator != opEmpty && f.Operator != opNEmpty) {
		return false
	}
	col, stored := e.column(f.Field)
	if _, covered := vocabFor(e.Table); covered {
		return !leftAsWritten(f) && (!stored || isTextType(col.Type))
	}
	return stored && isTextType(col.Type)
}

// answersTextEmptiness reports whether the replica can be asked what NetBox's
// empty lookup means of a column: it is text, and its schema lists the
// comparison it is asked with.
func answersTextEmptiness(col column) bool {
	return isTextType(col.Type) && slices.Contains(col.Operators, "gt")
}

// joinedNameOperators is what the editor offers on a name joined in from
// another table: the target column's operators but for is empty and has any
// value, which are asked of the column that holds the id.
func joinedNameOperators(col column) []string {
	return slices.DeleteFunc(seamOperators(col), func(op string) bool { return op == opEmpty || op == opNEmpty })
}

// joinedEmptinessReason refuses is empty or has any value on a name joined in
// from another table (a plugin's site, which the catalogue lists as the target's
// name): it is not a column the replica stores, so it has no text of its own to
// ask about; the column that holds the id is the one whose emptiness says the
// object is missing.
func joinedEmptinessReason(label string, via column) string {
	return fmt.Sprintf("%s is a name this replica looks up in %s, so it cannot test it for emptiness; filter on %s, the column that holds its id, instead",
		label, strings.TrimPrefix(via.Ref.Path, "/v1/"), via.Name)
}

// noGTReason refuses has any value on a text column whose schema lists no gt,
// the comparison it is asked with. named is the column, as namedColumn says it.
func noGTReason(named string) string {
	return fmt.Sprintf("this replica cannot tell whether there is a value in %s (its schema lists no gt there, which is how the replica is asked what NetBox's empty lookup means), so it cannot answer the filter; %s",
		named, netboxModeAdvice)
}

// namedColumn is the column a refusal is about: the replica's, and for a filter
// the user wrote under another name, with the column NetBox reads for it —
// "_children, the column NetBox reads for children". Every refusal of a filter on
// a column the replica lacks or cannot test names it so, in whatever sentence,
// which puts a comma or a parenthesis after it.
func namedColumn(column, label string) string {
	if column == label {
		return column
	}
	return column + ", the column NetBox reads for " + label
}
