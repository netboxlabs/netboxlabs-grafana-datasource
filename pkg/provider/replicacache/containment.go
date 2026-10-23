package replicacache

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

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
// as NetBox does, with no request. And NetBox's filter takes one value, the last
// of several, which would narrow a multi-value variable to one selection
// without a word, so several are refused.

// containsName is the NetBox filter this file answers.
const containsName = "contains"

// opContains is the operator the rewrite sends a prefix's contains as: the
// replica's own containment, on the prefix column. It is internal like the null
// operators (see internalOperators), and the name has a space for the same
// reason: no NetBox lookup does, so no saved query carries it.
const opContains = "contains net"

// offersContains reports whether this replica-cache build has the containment
// operator, by its schema route: v1.39 and later state its limit, and an older
// build, whose answer to the operator is a 400, states none.
func (c *catalog) offersContains() bool {
	return c != nil && c.ContainsMaxValues > 0
}

// containsUnsupportedReason is the refusal of NetBox's contains filter on a
// build that cannot answer it.
const containsUnsupportedReason = "this replica-cache build has no containment search (v1.39 added it), so it cannot answer NetBox's contains filter; ask NetBox Labs to upgrade the replica, or use a datasource in NetBox mode for this filter"

// rewriteContains puts a prefix's contains filter on the replica's containment
// operator: one value, read as NetBox reads it, written as the replica reads it,
// on the column NetBox's filter compares (the prefix). The row keeps the field
// and operator the user wrote for a refusal to name.
//
// It runs on a table the vocabulary gives a containment filter — a prefix's —
// and leaves every other as it is: NetBox's filtersets refused contains on the
// models that have none (translateFilters), and on one they do not describe, a
// plugin's, the name is a column's or no column, which the checks that follow
// say. The entity's table is what identifies NetBox's model, so objectType, the
// type the filters are for, is not read.
//
// none is a value that is no address or prefix, which no row can match; the row
// is dropped, since left in it would be judged as a column named contains. A
// value blank after NetBox's strip filters nothing, as it does there.
func rewriteContains(filters []provider.Filter, objectType string, e entity) (out []provider.Filter, none bool, err error) {
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

// pythonStrip is Python's str.strip(), which NetBox applies to the value: it
// removes what str.isspace() holds — Unicode's White_Space and, beside it, the
// ASCII separators \x1c-\x1f, which unicode.IsSpace leaves (NetBox 4.4.10:
// ?contains=%1c10.10.10.5 finds what 10.10.10.5 finds).
func pythonStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}
