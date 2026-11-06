package replicacache

import (
	"fmt"
	"slices"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// NetBox's contains filter on a prefix finds the prefixes that contain an
// address or a prefix (ipam/filtersets.py, search_contains). replica-cache
// v1.39 added an operator for it on the prefix column, filter[prefix]__contains,
// which answers the way NetBox's filter does:
//
//   - a value written without a mask is an address, contained strictly: a
//     stored /32 that is the address does not contain it (NetBox filters it by
//     prefix__net_contains, PostgreSQL's >>);
//   - a value with a mask is a prefix, contained or equal (>>=), its host bits
//     ignored;
//   - the VRF plays no part, the two families never contain each other, and an
//     IPv4-mapped address is an IPv6 one.
//
// What the replica does not do, this does before sending. It reads the value
// as NetBox's netaddr does, which takes what the replica's Go parser answers
// with a 400 — a netmask or hostmask for a length, a length written 024 or +24,
// the separators Python's strip removes — and sends the network it names. A
// value that is no address or prefix finds no row in NetBox (queryset.none(),
// not an error), where the replica refuses the whole request, so it is answered
// as NetBox does, with no request; a null character in it is NetBox's own 400,
// and is refused so. And NetBox's filter takes one value, the last of several,
// which would narrow a multi-value variable to one selection without a word, so
// several are refused.

// containsName is the NetBox filter this file answers.
const containsName = "contains"

// opContains is the operator the rewrite sends a prefix's contains as: the
// replica's own containment, on the prefix column. It is internal like the null
// operators (see internalOperators), and the name has a space for the same
// reason: no NetBox lookup does, so no saved query carries it.
const opContains = "contains net"

// nullCharacterReason is Django's refusal of a null character in a text value,
// which NetBox's contains answers with, and what to do.
const nullCharacterReason = "Null characters are not allowed. NetBox mode answers the same; remove the null character from the value"

// containsUnsupportedReason is the refusal of NetBox's contains filter on a
// build that cannot answer it.
const containsUnsupportedReason = "this replica-cache build has no containment search (v1.39 added it), so it cannot answer NetBox's contains filter; ask NetBox Labs to upgrade the replica, or " + netboxModeAdvice

// rewriteContains puts a prefix's contains filter on the replica's containment
// operator: one value, read as NetBox reads it, written as the replica reads it,
// on the column NetBox's filter compares (the prefix). The row keeps the field
// and operator the user wrote for a refusal to name.
//
// It runs on a table the vocabulary gives a containment filter — a prefix's —
// and leaves every other as it is: NetBox's filtersets refused contains on the
// models that have none (translateFilters), and on one they do not describe, a
// plugin's, the name is a column's or no column, which the checks that follow
// say. The entity's table is what identifies NetBox's model, so the object type
// the caller names is not read.
//
// none is a value that is no address or prefix, which no row can match; the row
// is dropped, since left in it would be judged as a column named contains. A
// value blank after NetBox's strip filters nothing, as it does there.
func rewriteContains(filters []provider.Filter, _ string, e entity) (out []provider.Filter, none bool, err error) {
	entry, ok := containsFilter(e)
	if !ok {
		return filters, false, nil
	}
	// The rows of the name that carry a value, which are one filter, as in
	// NetBox: the rows that carry none send nothing, and stay as they are.
	at := -1
	var values []string
	for i, f := range filters {
		if f.Field != containsName || !equalityOperator(f.Operator) {
			continue
		}
		if vs := splitValues(f.Value); len(vs) > 0 {
			if at < 0 {
				at = i
			}
			values = append(values, vs...)
		}
	}
	if at < 0 {
		return filters, false, nil
	}
	row := filters[at]
	if len(values) > 1 {
		return nil, false, &UnsupportedFilterError{Field: fieldLabel(row), Operator: operatorLabel(row),
			Reason: fmt.Sprintf("NetBox's contains filter takes one address or prefix, and it would use only the last of the %s given, dropping the rest without saying so; select a single value", valueCount(len(values)))}
	}

	value := pythonStrip(values[0])
	if value == "" {
		return slices.Delete(slices.Clone(filters), at, at+1), false, nil
	}
	// NetBox's form refuses a null character in a text value with its own 400,
	// after the strip, whatever else the value says: it is no address, which
	// would match nothing, but NetBox does not say that (NetBox 4.4.10:
	// ?contains=10.10.10.5%00 is HTTP 400 {"contains": ["Null characters are not
	// allowed."]}, and so is a null between spaces).
	if strings.ContainsRune(value, 0) {
		return nil, false, &UnsupportedFilterError{Field: fieldLabel(row), Operator: operatorLabel(row), Reason: nullCharacterReason}
	}
	network, ok := netaddrNetwork(value)
	if !ok {
		return slices.Delete(slices.Clone(filters), at, at+1), true, nil
	}
	// NetBox tells an address from a prefix by the slash, and so does the
	// replica: the address is sent bare, the prefix as the network it names.
	text := network.Addr().String()
	if strings.Contains(value, "/") {
		text = network.Masked().String()
	}
	return slices.Replace(slices.Clone(filters), at, at+1, provider.Filter{
		Field: entry.Column, Operator: opContains, Value: text, Label: fieldLabel(row), LabelOperator: row.Operator,
	}), false, nil
}

// validateContainsBuild is the part of validateFilters that needs no column,
// for an entity the replica has received nothing for, which has none to ask: a
// build without the containment operator is refused as it is for an entity with
// data (validateFilters), not sent the operator. A build answers an operator it
// does not know with a 400 before it looks at the table, where every other
// filter on an unfed table gets the table's own 404 (measured on v1.40.1 against
// a table that never received data: filter[id]__eq is the 404, filter[id]__bogus
// the 400 "invalid filter operator: bogus"), and that bare 400 says nothing of
// what to do.
func validateContainsBuild(filters []provider.Filter, c *catalog) error {
	if c.offersContains() {
		return nil
	}
	for _, f := range filters {
		if f.Field != "" && f.Operator == opContains {
			return &UnsupportedFilterError{Field: fieldLabel(f), Operator: operatorLabel(f), Reason: containsUnsupportedReason}
		}
	}
	return nil
}

// containsFilter is NetBox's containment filter on the entity's table: the
// vocabulary's entry of that kind, which names the column it compares.
func containsFilter(e entity) (vocabEntry, bool) {
	vocab, covered := vocabFor(e.Table)
	if !covered {
		return vocabEntry{}, false
	}
	alts, ok := vocab[containsName]
	if !ok {
		return vocabEntry{}, false
	}
	entry := pickVocab(alts, e)
	return entry, entry.Kind == kindContains
}
