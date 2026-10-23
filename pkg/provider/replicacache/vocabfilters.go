package replicacache

import (
	"fmt"
	"maps"
	"math"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A filter in replica-cache mode means what it means in NetBox mode, or is
// refused saying why. NetBox ignores a filter it does not have and returns
// every row, so a filter only this backend took (a text match on a related
// name, a column NetBox has no filter for) made a dashboard right here and
// silently wrong on switching modes. This file reads each filter as NetBox
// would — by NetBox's own vocabulary for the model (vocab.go) — and puts it
// in the replica's terms: the column NetBox reads, the operator that means
// what NetBox's lookup means. What follows in the pipeline (comparisons.go,
// negations.go, filters.go) sends what this produces.

// Internal operators, produced here and nowhere else: IS NULL and IS NOT NULL on
// a column, sent as filter[c]__isnull. NetBox spells them several ways (the
// literal null, mtu__empty, cabled, local_context_data), none of them an
// operator a query names, and a NetBox lookup is a Python identifier, so a
// name with a space is never one a saved query carries.
const (
	opIsNull  = "is null"
	opNotNull = "is not null"
)

// internalOperators are the operators this datasource writes for itself, which
// a query never names. Query refuses a filter that names one before anything
// reads it, on any object type: a plugin's, which nothing translates, would
// otherwise have a hand-written "is null" taken for what the translation means
// by it.
var internalOperators = map[string]bool{opIsNull: true, opNotNull: true}

// refuseInternalOperators is that refusal. A row with no field sends nothing,
// whatever its operator, and is left alone as everywhere else.
func refuseInternalOperators(filters []provider.Filter) error {
	for _, f := range filters {
		if f.Field != "" && internalOperators[f.Operator] {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "that operator is one this datasource writes for itself, not one a query can name; use an operator the editor offers"}
		}
	}
	return nil
}

// nullValue is the literal NetBox's forms read as no value (django-filter's
// null_value, "null"): IS NULL where the filter takes it.
const nullValue = "null"

// netboxModeAdvice ends a refusal of a filter NetBox mode answers.
const netboxModeAdvice = "use a datasource in NetBox mode for this filter"

// translateFilters reads the caller's filters by NetBox's vocabulary for the
// replica table, before anything else reads them. Each row is looked up under
// the name NetBox mode sends it as (field, field__<operator>, field__empty for
// both emptiness tests), and is rewritten onto the column NetBox reads with
// NetBox's meaning, passed on unchanged for the steps that answer it (a filter
// on a related object, a prefix's contains), or refused. Every rewritten row
// keeps the field as written in Label, for refusals.
//
// The rows of one NetBox name are one filter there: NetBox mode adds every
// row's values to one key (netbox/enrich.go), so they are read together — a
// single-valued filter's last value, the case-insensitive name's count, the
// literal null — and written at the first of them.
//
// none is a filter no row can match (a MAC address NetBox cannot read, a
// prefix with host bits set), so no request is needed. A table the vocabulary
// does not cover — a plugin's model — keeps the catalogue's rules: its filters
// come back as written.
func translateFilters(table string, filters []provider.Filter, e entity, c *catalog) (out []provider.Filter, none bool, err error) {
	vocab, covered := vocabFor(table)
	if !covered {
		return filters, false, nil
	}
	tr := translation{vocab: vocab, table: table, e: e, c: c}
	names := make([]string, len(filters))
	rows := map[string][]provider.Filter{}
	for i, f := range filters {
		if sendsNothing(f) || leftAsWritten(f) {
			continue
		}
		names[i] = netboxName(f)
		rows[names[i]] = append(rows[names[i]], f)
	}
	out = make([]provider.Filter, 0, len(filters))
	done := map[string]bool{}
	for i, f := range filters {
		name := names[i]
		switch {
		case name == "":
			out = append(out, f)
		case !done[name]:
			done[name] = true
			translated, unmatchable, err := tr.filter(name, rows[name])
			if err != nil {
				return nil, false, err
			}
			none = none || unmatchable
			out = append(out, translated...)
		}
	}
	return out, none || nullAndNotNull(out), nil
}

// sendsNothing is a row buildFilterValues skips, as NetBox mode sends no
// parameter for it either: no field, or no value on an operator that needs
// one. It is not judged, so a saved query carrying one keeps running.
func sendsNothing(f provider.Filter) bool {
	return f.Field == "" || f.Operator != opEmpty && f.Operator != opNEmpty && len(splitValues(f.Value)) == 0
}

// leftAsWritten is a filter whose refusal further on already says what is
// true: q (NetBox's search, which the replica does not run), tag and tag_id
// with their lookups (the replica stores no tags), cf_* (a custom field is not
// a column). The vocabulary would call the first a method and the tags paths,
// which says less.
func leftAsWritten(f provider.Filter) bool {
	base, _, _ := strings.Cut(f.Field, "__")
	return base == "q" || base == "tag" || base == "tag_id" || strings.HasPrefix(f.Field, "cf_")
}

// netboxName is the query parameter NetBox mode sends a row as
// (netbox/enrich.go buildFilterValues): the field for an exact match,
// field__empty for "is empty" and "has any value" alike, field__<operator>
// otherwise.
func netboxName(f provider.Filter) string {
	switch f.Operator {
	case opExact, "exact":
		return f.Field
	case opEmpty, opNEmpty:
		return f.Field + "__empty"
	}
	return f.Field + "__" + f.Operator
}

// rowValues is what NetBox mode sends for a row: its comma-separated values,
// trimmed, and true for "is empty", false for "has any value".
func rowValues(f provider.Filter) []string {
	switch f.Operator {
	case opEmpty:
		return []string{"true"}
	case opNEmpty:
		return []string{"false"}
	}
	return splitValues(f.Value)
}

// translation is one table's vocabulary and the replica it is read against.
type translation struct {
	vocab map[string][]vocabEntry
	table string
	e     entity
	c     *catalog
}

// filter translates the rows of one NetBox name.
func (tr translation) filter(name string, rows []provider.Filter) ([]provider.Filter, bool, error) {
	first := rows[0]
	refuse := func(reason string) ([]provider.Filter, bool, error) {
		return nil, false, &UnsupportedFilterError{Field: first.Field, Operator: first.Operator, Reason: reason}
	}
	alts, ok := tr.vocab[name]
	if !ok {
		return refuse(tr.lacks(name, first.Field))
	}
	entry := pickVocab(alts, tr.e)
	var values []string
	for _, r := range rows {
		values = append(values, rowValues(r)...)
	}
	// NetBox's single-valued filters read the last value of the parameter
	// (QueryDict.get), whatever came before it: face=front&face=rear is rear,
	// airflow=front-to-rear&airflow=null is null. A prefix's contains is left
	// whole: several values are to be refused there, not narrowed to the last.
	if entry.Single && entry.Kind != kindContains {
		values = values[len(values)-1:]
	}
	switch entry.Kind {
	case kindRelated, kindTree, kindModelChoice, kindContains:
		if entry.Single && entry.Kind != kindContains {
			last := rows[len(rows)-1]
			return []provider.Filter{{Field: last.Field, Operator: last.Operator, Value: values[0], Label: last.Field}}, false, nil
		}
		return rows, false, nil
	case kindValue, kindMAC:
		return tr.values(name, entry, rows, values)
	case kindBool, kindIsNull, kindLocalContext, kindEmpty:
		last := values[len(values)-1]
		b, ok := netboxBool(last)
		if !ok {
			return refuse(fmt.Sprintf("NetBox ignores %q for %s and returns every row; use true or false", last, name))
		}
		return []provider.Filter{booleanFilter(entry, b, first.Field)}, false, nil
	case kindAddress:
		// NetBox's filter_address is the net_in match setAddressMatch makes.
		return []provider.Filter{{Field: entry.Column, Value: strings.Join(values, ","), Label: first.Field}}, false, nil
	case kindPrefix:
		return tr.prefix(entry, first, values)
	case kindPath:
		return refuse(fmt.Sprintf("NetBox filters %s through %s, which this replica cannot join; %s", first.Field, entry.Path, netboxModeAdvice))
	case kindMethod:
		return refuse(fmt.Sprintf("NetBox computes %s with a query of its own (%s), which this replica cannot run; %s", first.Field, entry.Method, netboxModeAdvice))
	}
	return refuse(refusedReason(entry.Reason, name, first.Field))
}

// lacks is the refusal of a name NetBox does not have on the model, saying
// what to do instead. A field NetBox has with a lookup it lacks (site__ic)
// points at NetBox's other lookups on it; otherwise the filter that does what
// was meant is named where NetBox has one the replica takes: <x>_id for a
// relation NetBox filters by id alone (a device's rack), <x> for an <x>_slug
// (NetBox's site is the slug) and for a cached _<x> (a prefix's children). One
// NetBox applies through another relation (a device's region) is no
// alternative: it is refused too.
func (tr translation) lacks(name, field string) string {
	reason := fmt.Sprintf("NetBox has no %s filter on %s, so a dashboard filtering on it would return every row in NetBox mode", name, tr.objectType())
	base := field
	if i := strings.LastIndex(field, "__"); i >= 0 && lookupSuffixes[field[i+2:]] {
		base = field[:i]
	}
	if tr.takesFilter(base) {
		return reason + "; use another of NetBox's lookups on " + base
	}
	for _, alt := range []string{base + "_id", strings.TrimSuffix(base, "_slug"), strings.TrimPrefix(base, "_")} {
		if alt != base && tr.takesFilter(alt) {
			return reason + "; filter on " + alt + " instead"
		}
	}
	return reason + "; remove it, or choose a filter the editor offers"
}

// takesFilter reports whether the translation takes a NetBox filter on this
// model rather than refusing it for what it is (a path, a method, a refusal).
func (tr translation) takesFilter(name string) bool {
	alts, ok := tr.vocab[name]
	if !ok {
		return false
	}
	switch pickVocab(alts, tr.e).Kind {
	case kindRefuse, kindPath, kindMethod:
		return false
	}
	return true
}

// lookupSuffixes are the lookups a filter name can end in, as the editor's
// operators spell them.
var lookupSuffixes = map[string]bool{
	"n": true, "ie": true, "nie": true, "ic": true, "nic": true, "isw": true, "nisw": true, "iew": true, "niew": true,
	"regex": true, "iregex": true, "gt": true, "gte": true, "lt": true, "lte": true, "empty": true,
}

// objectType is the object type the catalogue serves the table as — what the
// user picked — for a refusal to name; the table when the catalogue has none.
func (tr translation) objectType() string {
	if tr.c != nil {
		for _, objectType := range slices.Sorted(maps.Keys(tr.c.Entities)) {
			if tr.c.Entities[objectType].Table == tr.table {
				return objectType
			}
		}
	}
	return tr.table
}

// lookupOperators is the editor's operator for each of NetBox's lookups on a
// value filter, and for its negation where NetBox has one.
var lookupOperators = map[string]struct{ match, negated string }{
	"exact":       {opExact, "n"},
	"iexact":      {opIExact, "nie"},
	"icontains":   {opIContns, "nic"},
	"istartswith": {opIStarts, "nisw"},
	"iendswith":   {opIEnds, "niew"},
	"gt":          {opGT, ""},
	"gte":         {opGTE, ""},
	"lt":          {opLT, ""},
	"lte":         {opLTE, ""},
}

// textSearch is a lookup NetBox cannot run on None: asset_tag__ic=null is a
// 500 on NetBox 4.4.10, "Cannot use None as a query value".
func textSearch(lookup string) bool {
	switch lookup {
	case "icontains", "istartswith", "iendswith":
		return true
	}
	return false
}

// timeZoneNull is a site's time zone filter, or its negation, on the literal
// null: NetBox reads it as the empty string, not as IS NULL, its form being a
// time zone field's (NetBox 4.4.10: time_zone=null → the 3 sites with a blank
// time zone, time_zone__n=null → none), which the replica cannot send yet. The
// vocabulary gives the two no nv (vocab.go); time_zone__ie and __nie read
// null as IS NULL and carry it.
func (tr translation) timeZoneNull(entry vocabEntry) bool {
	return tr.table == "dcim_site" && entry.Column == "time_zone" && !entry.NullValue
}

// values translates a value or MAC filter: one column compared with NetBox's
// lookup.
func (tr translation) values(name string, entry vocabEntry, rows []provider.Filter, values []string) ([]provider.Filter, bool, error) {
	first := rows[0]
	refuse := func(reason string) ([]provider.Filter, bool, error) {
		return nil, false, &UnsupportedFilterError{Field: first.Field, Operator: first.Operator, Reason: reason}
	}
	// NetBox's regex and iregex are PostgreSQL's ~ and ~*; the replica has no
	// regular-expression match to send them as.
	if entry.Lookup == "regex" || entry.Lookup == "iregex" {
		return refuse(fmt.Sprintf("the replica has no regular-expression match, so it cannot answer %s; %s", name, netboxModeAdvice))
	}
	ops, known := lookupOperators[entry.Lookup]
	op := ops.match
	if entry.Exclude {
		op = ops.negated
	}
	if !known || (entry.Exclude && ops.negated == "") {
		return refuse(fmt.Sprintf("NetBox compares %s by %s, which the replica cannot reproduce; %s", first.Field, entry.Lookup, netboxModeAdvice))
	}
	on := func(op string, values []string, label string) provider.Filter {
		return provider.Filter{Field: entry.Column, Operator: op, Value: strings.Join(values, ","), Label: label}
	}

	// A MAC address filter (the base and its negation) reads each value as an
	// EUI-48 address, and one NetBox cannot read makes the whole filter match
	// nothing: MultiValueMACAddressFilter answers the ValidationError with
	// qs.none() (NetBox 4.6.10: a valid address beside garbage → 0 rows). The
	// replica stores PostgreSQL's macaddr text, so the value is written so. The
	// text searches compare the text as written, and so go as written.
	normalised := entry.Kind == kindMAC && entry.Lookup == "exact"
	if normalised {
		values = slices.Clone(values)
		for i, v := range values {
			if v == nullValue && entry.NullValue {
				continue
			}
			mac, ok := netaddrEUI48(v)
			if !ok {
				return nil, true, nil
			}
			values[i] = mac
		}
	}

	if slices.Contains(values, nullValue) {
		switch {
		case textSearch(entry.Lookup):
			return refuse(fmt.Sprintf("NetBox answers null on %s with an error; remove the filter", name))
		case tr.timeZoneNull(entry):
			return refuse(fmt.Sprintf("NetBox reads null on %s as an empty time zone, which this replica cannot send yet; %s", first.Field, netboxModeAdvice))
		case entry.NullValue:
			// NetBox's form reads null as None, and Django's exact and iexact
			// read None as IS NULL (NetBox 4.4.10: asset_tag=null → the 18
			// devices with none), excluded on a negation.
			rest := slices.DeleteFunc(slices.Clone(values), func(v string) bool { return v == nullValue })
			if len(rest) == 0 {
				return []provider.Filter{{Field: entry.Column, Operator: nullOperator(!entry.Exclude), Label: first.Field}}, false, nil
			}
			if !entry.Exclude {
				return refuse("NetBox returns rows matching either null or the values, and this replica cannot combine the two; filter on null and on the values in separate queries, or " + netboxModeAdvice)
			}
			return []provider.Filter{{Field: entry.Column, Operator: opNotNull, Label: first.Field}, on(op, rest, first.Field)}, false, nil
		case tr.isChoice(entry.Column):
			// A choice whose form has no null choice (status): NetBox's 400,
			// in its words.
			return refuse(fmt.Sprintf("Select a valid choice. null is not one of the available choices. NetBox mode answers the same; filter on one of the stored values instead, which %s_value shows", entry.Column))
		}
		// Anywhere else null is a value like any other; a number filter's form
		// refuses it, as validateFilters does here.
	}

	// NetBox compares a device's or VM's name, and the serial of a device, rack,
	// module or inventory item, without regard to case: its base filter's lookup
	// is iexact. One value is the replica's iexact. Several are an exact in, the
	// documented divergence: the replica has no case-insensitive in, and the
	// values a multi-select variable offers come from the data, in its case.
	// Values equal but for case are one value to iexact, and go as one.
	if entry.Lookup == "iexact" && !entry.Exclude && !strings.Contains(name, "__") {
		folded := map[string]bool{}
		for _, v := range values {
			folded[strings.ToLower(v)] = true
		}
		if len(folded) > 1 {
			return []provider.Filter{on(opExact, values, first.Field)}, false, nil
		}
		if !tr.takes(entry.Column, wireTextOperator[opIExact]) {
			return refuse(fmt.Sprintf("this replica cannot compare %s case-insensitively as NetBox does (a newer replica-cache build can); filter on id, or use a datasource in NetBox mode", first.Field))
		}
		return []provider.Filter{on(opIExact, values[:1], first.Field)}, false, nil
	}

	// Otherwise row by row, as written, so the steps after this refuse two rows
	// they cannot combine as they always have.
	if entry.Single || normalised {
		return []provider.Filter{on(op, values, first.Field)}, false, nil
	}
	out := make([]provider.Filter, 0, len(rows))
	for _, r := range rows {
		out = append(out, on(op, rowValues(r), r.Field))
	}
	return out, false, nil
}

// takes reports whether the replica takes a wire operator on a column: the
// column's own list where the entity has it, else the build's, there being no
// column to ask on an entity that has not received data.
func (tr translation) takes(column, wire string) bool {
	if col, ok := tr.e.column(column); ok {
		return slices.Contains(col.Operators, wire)
	}
	return tr.c.offersOperator(wire)
}

// isChoice reports whether NetBox's filter on a column is a choice filter, by
// the choice-label map: every choice filter whose form takes no null (status,
// type) has its column labelled there (checked over NetBox 4.4.10's and
// 4.6.10's filtersets). The other filters on a labelled column are text ones,
// whose nv decides null before this is asked, and on 4.6 a console port's
// speed__ie, a number filter whose form refuses null as well.
func (tr translation) isChoice(column string) bool {
	_, ok := loadChoiceLabels()[tr.objectType()][column]
	return ok
}

// netboxBool reads a boolean as NetBox's form does (Django's NullBooleanSelect,
// which reads the last value): true, True and 2 are true, false, False and 3
// are false. Anything else — 1, 0, TRUE, yes — is no answer, and NetBox drops
// the filter and returns every row (NetBox 4.4.10: mgmt_only=1 → every
// interface).
func netboxBool(v string) (bool, bool) {
	switch v {
	case "true", "True", "2":
		return true, true
	case "false", "False", "3":
		return false, true
	}
	return false, false
}

// booleanFilter is a boolean NetBox filter's answer in the replica's terms.
func booleanFilter(entry vocabEntry, b bool, label string) provider.Filter {
	f := provider.Filter{Field: entry.Column, Label: label}
	switch entry.Kind {
	case kindBool:
		// Equality on the column; an excluding one keeps the rows NetBox's
		// exclude() keeps, NULL included.
		f.Value = strconv.FormatBool(b)
		if entry.Exclude {
			f.Operator = "n"
		}
	case kindIsNull:
		// cabled=true is the cable column IS NOT NULL (an isnull that
		// excludes); mtu__empty=true is mtu IS NULL.
		f.Operator = nullOperator(b != entry.Exclude)
	case kindLocalContext:
		// NetBox's _local_context_data: true is "has one".
		f.Operator = nullOperator(!b)
	case kindEmpty:
		// NetBox's empty lookup on text, NULL or "", left as the emptiness
		// operators on the column: one filter cannot test both there, and
		// validateFilters refuses it on text, saying so (emptyOnTextReason).
		f.Operator = opNEmpty
		if b {
			f.Operator = opEmpty
		}
	}
	return f
}

func nullOperator(isNull bool) string {
	if isNull {
		return opIsNull
	}
	return opNotNull
}

// nullAndNotNull reports a column both IS NULL and IS NOT NULL, from two names
// NetBox ANDs (asset_tag=null, asset_tag__n=null): no row is both, and the
// replica takes one isnull per column, so the answer is given here.
func nullAndNotNull(filters []provider.Filter) bool {
	isNull, notNull := map[string]bool{}, map[string]bool{}
	for _, f := range filters {
		switch f.Operator {
		case opIsNull:
			isNull[f.Field] = true
		case opNotNull:
			notNull[f.Field] = true
		}
	}
	for field := range isNull {
		if notNull[field] {
			return true
		}
	}
	return false
}

// prefix translates NetBox's filter_prefix: equality on the stored cidr text,
// mask always written (cidrText), with each value read as netaddr's
// IPNetwork reads it. It never goes through the address match, whatever the
// catalogue lists on the column (addressField leaves network columns out).
//
// An aggregate's is single-valued and takes IPNetwork(value).cidr, host bits
// zeroed (NetBox 4.4.10: prefix=10.250.3.4/15 finds 10.250.0.0/15); a value
// that does not parse is queryset.none(). A prefix's drops what does not parse
// and compares the networks as given, each network once, in whichever spelling:
// one with host bits set matches nothing alone (prefix=10.10.10.5/24 → 0 rows,
// so does 10.10.10.5/24 beside itself or 10.10.10.5/255.255.255.0) and is
// PostgreSQL's invalid-cidr error beside a different network (a 500), so it is
// refused there.
func (tr translation) prefix(entry vocabEntry, first provider.Filter, values []string) ([]provider.Filter, bool, error) {
	if entry.Single {
		p, ok := netaddrNetwork(values[0])
		if !ok {
			return nil, true, nil
		}
		return []provider.Filter{{Field: entry.Column, Value: cidrText(p.Masked()), Label: first.Field}}, false, nil
	}
	var networks []netip.Prefix
	for _, v := range values {
		if p, ok := netaddrNetwork(v); ok && !slices.Contains(networks, p) {
			networks = append(networks, p)
		}
	}
	texts := make([]string, 0, len(networks))
	for _, p := range networks {
		if p != p.Masked() {
			if len(networks) == 1 {
				return nil, true, nil
			}
			return nil, false, &UnsupportedFilterError{Field: first.Field, Operator: first.Operator,
				Reason: fmt.Sprintf("NetBox answers this with an error, because %s has bits set past its mask; write it as %s", p, p.Masked())}
		}
		texts = append(texts, cidrText(p))
	}
	if len(texts) == 0 {
		return nil, true, nil
	}
	return []provider.Filter{{Field: entry.Column, Value: strings.Join(texts, ","), Label: first.Field}}, false, nil
}

// cidrText is a network as PostgreSQL prints a cidr: the address as it prints
// one (pgAddrText), the mask always, /32 included.
func cidrText(p netip.Prefix) string {
	return pgAddrText(p.Addr()) + "/" + strconv.Itoa(p.Bits())
}

// refusedReason is the sentence for a filter the vocabulary refuses, by its
// reason: what the replica holds that NetBox compares otherwise.
func refusedReason(reason, name, field string) string {
	why, lookup, _ := strings.Cut(reason, ":")
	switch why {
	case "decimal":
		return fmt.Sprintf("the replica stores %s as text with a fixed scale, so it cannot compare it as NetBox compares a number; %s", field, netboxModeAdvice)
	case "date":
		// Timestamps arrive as text and dates as a day count (DATA-250).
		return fmt.Sprintf("the replica does not store %s as a date, so it cannot compare it as NetBox compares a date; %s", field, netboxModeAdvice)
	case "contenttype":
		return fmt.Sprintf("NetBox matches %s by content-type name, and the replica has no content-type names; %s", field, netboxModeAdvice)
	case "array":
		return fmt.Sprintf("the replica stores %s as JSON text, so it cannot compare its elements as NetBox does; %s", field, netboxModeAdvice)
	case "uuid":
		return fmt.Sprintf("the replica cannot compare %s as NetBox compares a UUID; %s", field, netboxModeAdvice)
	case "wwn":
		return fmt.Sprintf("NetBox reads %s as a WWN in any of its spellings, which the replica cannot reproduce on the stored text; %s", field, netboxModeAdvice)
	case "badempty":
		// The filter keeps its number or date class with the isnull lookup, so
		// true fails its form: speed__empty is "Enter a whole number.".
		return fmt.Sprintf("NetBox answers %s with an error; remove the filter", name)
	case "lookup":
		return fmt.Sprintf("NetBox compares %s by %s, which the replica cannot reproduce; %s", field, lookup, netboxModeAdvice)
	}
	return fmt.Sprintf("NetBox's %s filter is not one the replica can reproduce; %s", name, netboxModeAdvice)
}

// euiFormats are netaddr 1.3's EUI-48 spellings (netaddr.strategy.eui48's
// RE_MAC_FORMATS, case-insensitive), which NetBox's MACAddressField reads after
// removing spaces, each with the width its groups are zero-padded to: six
// groups of one or two hex digits, all colons or all hyphens; three of one to
// four, joined by colons, hyphens or dots; two of five or six, joined by colons
// or hyphens; twelve or eleven bare digits. Nothing else parses, an EUI-64
// address included, which is why net.ParseMAC is no stand-in.
var euiFormats = func() []struct {
	re    *regexp.Regexp
	width int
} {
	groups := func(n int, sep, digits string) *regexp.Regexp {
		g := "([0-9a-fA-F]{" + digits + "})"
		return regexp.MustCompile("^" + g + strings.Repeat(regexp.QuoteMeta(sep)+g, n-1) + "$")
	}
	type format = struct {
		re    *regexp.Regexp
		width int
	}
	return []format{
		{groups(6, ":", "1,2"), 2}, {groups(6, "-", "1,2"), 2},
		{groups(3, ":", "1,4"), 4}, {groups(3, "-", "1,4"), 4}, {groups(3, ".", "1,4"), 4},
		{groups(2, "-", "5,6"), 6}, {groups(2, ":", "5,6"), 6},
		{groups(1, "", "12"), 12}, {groups(1, "", "11"), 12},
	}
}()

// netaddrEUI48 reads a MAC address as NetBox does — spaces removed, then one of
// netaddr's EUI-48 spellings (euiFormats) — and writes it as the replica
// stores PostgreSQL's macaddr: lowercase, six colon-separated pairs. Python's $
// also matches before one trailing newline, so that is let through too.
func netaddrEUI48(s string) (string, bool) {
	s = strings.TrimSuffix(strings.ReplaceAll(s, " ", ""), "\n")
	for _, f := range euiFormats {
		m := f.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		var hex strings.Builder
		for _, g := range m[1:] {
			hex.WriteString(strings.Repeat("0", f.width-len(g)))
			hex.WriteString(strings.ToLower(g))
		}
		h := hex.String()
		return h[0:2] + ":" + h[2:4] + ":" + h[4:6] + ":" + h[6:8] + ":" + h[8:10] + ":" + h[10:12], true
	}
	return "", false
}

// netaddrNetwork reads a network as netaddr 1.3's IPNetwork does, host bits
// kept: the address strictly (inet_pton: no leading zeros, no short forms, no
// zone), then optionally / and either a length as Python's int() reads it
// (/024, /+24, / 24 are /24) within the family's width, or a netmask or
// hostmask of the same family (/255.255.255.0 and /0.0.0.255 are /24). A bare
// address is the single host, /32 or /128. NetBox reads a prefix filter's
// values with it (filter_prefix), and those of its contains filter and of a
// related filter on an address too.
func netaddrNetwork(s string) (netip.Prefix, bool) {
	addrText, maskText, masked := strings.Cut(s, "/")
	addr, ok := strictAddr(addrText)
	if !ok {
		return netip.Prefix{}, false
	}
	bits := addr.BitLen()
	if masked {
		if n, isInt := pythonInt(maskText); isInt {
			if n < 0 || n > int64(addr.BitLen()) {
				return netip.Prefix{}, false
			}
			bits = int(n)
		} else if mask, ok := strictAddr(maskText); ok && mask.BitLen() == addr.BitLen() {
			if bits, ok = maskBits(mask); !ok {
				return netip.Prefix{}, false
			}
		} else {
			return netip.Prefix{}, false
		}
	}
	return netip.PrefixFrom(addr, bits), true
}

// strictAddr reads an address as inet_pton does, which netaddr asks of an
// IPNetwork's address and mask (flags=INET_PTON): IPv4 as four decimal parts
// with no leading zeros, IPv6 without a zone. netip's parser is that.
func strictAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	return a, err == nil && a.Zone() == ""
}

// maskBits is the prefix length a netmask (ones, then zeros) or a hostmask
// (zeros, then ones) stands for, the netmask tried first as netaddr tries it:
// so 0.0.0.0 is /0 and 255.255.255.255 is /32.
func maskBits(mask netip.Addr) (int, bool) {
	b := mask.AsSlice()
	run := func(lead byte) (int, bool) {
		n := 0
		for n < len(b)*8 && (b[n/8]>>(7-n%8))&1 == lead {
			n++
		}
		for i := n; i < len(b)*8; i++ {
			if (b[i/8]>>(7-i%8))&1 == lead {
				return 0, false
			}
		}
		return n, true
	}
	if n, ok := run(1); ok {
		return n, true
	}
	return run(0)
}

// pythonInt reads an integer as Python's int() reads text, which netaddr uses
// for a prefix length and NetBox's forms for an id: whitespace around it (int()
// strips ASCII \t\n\v\f\r and space and Unicode's White_Space, not \x1c-\x1f,
// which is unicode.IsSpace's set), one sign, digits of any script with single
// underscores between them, leading zeros. A value past what an int64 holds
// saturates; no id or prefix length is near it.
func pythonInt(s string) (int64, bool) {
	s = strings.TrimFunc(s, unicode.IsSpace)
	negative := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		negative, s = s[0] == '-', s[1:]
	}
	var n int64
	digit := false
	for _, r := range s {
		if r == '_' && digit {
			digit = false
			continue
		}
		d, ok := decimalDigit(r)
		if !ok {
			return 0, false
		}
		if n > (math.MaxInt64-int64(d))/10 {
			n = math.MaxInt64
		} else {
			n = n*10 + int64(d)
		}
		digit = true
	}
	if !digit {
		return 0, false
	}
	if negative {
		n = -n
	}
	return n, true
}

// decimalDigit is the value of a decimal digit of any script, as Python's int()
// reads one. Unicode encodes each script's digits as runs of ten from zero, so
// the value is the distance from the start of the digits around it, modulo ten
// (where runs abut, as the mathematical digits do). All 680 agree with Python
// 3.12's unicodedata.decimal, Unicode 15.0.0 on both sides.
func decimalDigit(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if r < 0x80 || !unicode.IsDigit(r) {
		return 0, false
	}
	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}
	return int(r-start) % 10, true
}

// networkColumns are the columns NetBox's prefix filters read on the entity's
// table (a prefix's or an aggregate's prefix): networks, stored as cidr, each
// with its mask. None is ever matched by host (see addressField).
func networkColumns(e entity) map[string]bool {
	vocab, covered := vocabFor(e.Table)
	if !covered {
		return nil
	}
	out := map[string]bool{}
	for _, alts := range vocab {
		for _, entry := range alts {
			if entry.Kind == kindPrefix {
				out[entry.Column] = true
			}
		}
	}
	return out
}

// valueColumn is the stored column a filter's values come from, for
// autocomplete: the column NetBox's filter reads where it compares that
// column's own values (a value, MAC, boolean, address or prefix filter), which
// is not always its name — a prefix's children are its cached _children. Any
// other name is returned as it is: a relation keeps the expanded name it is
// read under.
func valueColumn(e entity, field string) string {
	vocab, covered := vocabFor(e.Table)
	if !covered {
		return field
	}
	alts, ok := vocab[field]
	if !ok {
		return field
	}
	switch entry := pickVocab(alts, e); entry.Kind {
	case kindValue, kindMAC, kindBool, kindAddress, kindPrefix:
		if e.has(entry.Column) {
			return entry.Column
		}
	}
	return field
}

// vocabFilterFields is what the editor offers on an entity the vocabulary
// covers: NetBox's own filters, under NetBox's names, where the replica stores
// the column NetBox reads (a 4.6-only filter is not offered on a replica of
// 4.4, whose table lacks its column), each with the lookups NetBox has on it
// that the translation can send and the column takes. A filter on a related
// object offers = and not. is-empty and has-any-value go where NetBox's
// __empty is a boolean meaning IS NULL — and only there: a number's or a
// date's __empty keeps that class and is a NetBox 400 (speed__empty). A
// decimal or date NetBox compares as such, which the replica cannot, is offered
// with those two alone. A prefix's contains is not offered: nothing answers it
// yet, and the filter it is passed on as is refused.
func vocabFilterFields(vocab map[string][]vocabEntry, e entity) []provider.FilterField {
	var out []provider.FilterField
	for _, name := range slices.Sorted(maps.Keys(vocab)) {
		if strings.Contains(name, "__") {
			continue // a lookup, offered as its filter's operator
		}
		if ops := vocabOperators(vocab, name, e); len(ops) > 0 {
			out = append(out, provider.FilterField{Name: name, Operators: ops})
		}
	}
	return out
}

func vocabOperators(vocab map[string][]vocabEntry, name string, e entity) []string {
	base := pickVocab(vocab[name], e)
	col, stored := e.column(base.Column)
	var ops []string
	switch base.Kind {
	case kindRelated, kindTree, kindModelChoice:
		if stored {
			ops = append(ops, opExact)
			if n, ok := vocab[name+"__n"]; ok && pickVocab(n, e).Kind == base.Kind {
				ops = append(ops, "n")
			}
		}
	case kindIsNull, kindLocalContext:
		if stored && slices.Contains(col.Operators, "isnull") {
			ops = append(ops, opExact)
		}
	case kindValue, kindMAC, kindBool, kindAddress, kindPrefix:
		if !stored {
			break
		}
		for _, op := range seamOperators(col) {
			switch op {
			case opEmpty, opNEmpty:
				// NetBox's __empty decides, below.
			case opExact:
				// One value of a case-insensitive name needs the replica's iexact.
				if base.Lookup != "iexact" || slices.Contains(col.Operators, wireTextOperator[opIExact]) {
					ops = append(ops, op)
				}
			default:
				if v, ok := vocab[name+"__"+op]; ok {
					switch variant := pickVocab(v, e); variant.Kind {
					case kindRefuse, kindPath, kindMethod:
					default:
						if variant.Column == base.Column {
							ops = append(ops, op)
						}
					}
				}
			}
		}
	}
	if (len(ops) > 0 && base.Kind == kindValue) || base.Kind == kindRefuse {
		if v, ok := vocab[name+"__empty"]; ok {
			if empty := pickVocab(v, e); empty.Kind == kindIsNull {
				if c, ok := e.column(empty.Column); ok && slices.Contains(c.Operators, "isnull") {
					ops = append(ops, opEmpty, opNEmpty)
				}
			}
		}
	}
	return ops
}
