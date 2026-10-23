package replicacache

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A filter on a related object means here what NetBox's means. NetBox's site
// filter compares the site's slug — its to_field_name — and site_id the id; a
// VRF filter compares the RD, a primary IP the address. Its form checks every
// value before anything is compared, and answers 400 for one that names
// nothing. A tree filter (a device's role, a prefix's location) takes each
// node with every node below it. The replica's own expansion compares the name
// it joins in, and a value that names nothing is no row there — which an alert
// rule reads as "nothing is wrong" where NetBox mode fails it. So each value is
// checked as NetBox's form checks it, then looked up in the target's table, and
// the filter is sent on the foreign key as the ids NetBox would compare.
//
// Measured on NetBox 4.4.10 (GET only), on dcim/devices unless named:
//
//	site=AMS1                    400 Select a valid choice. AMS1 is not one of the available choices.
//	site_id=999&site_id=abc      400 “abc” is not a valid value.
//	site_id=null&site_id=abc     400 “abc” is not a valid value.
//	site_id=01, +1, 1_0          400 Select a valid choice. 01 is not one of the available choices.
//	primary_ip4=10.10.10.14      400 Select a valid choice. … (NetBox writes it 10.10.10.14/24)
//	primary_ip4=garbage, …/33    400 Invalid IP address format: garbage
//	role=parity-role-parent      the 3 devices on its child role
//	site=ams1&site_id=2          0 rows: two names are ANDed
//	site=ams1&site=nyc1          both sites' devices: one name's values are ORed
//	tenant=null                  the 2 devices with no tenant; tenant__n=null the other 16
//	dcim/interfaces primary_mac_address=gar bage
//	                             400 Invalid MAC address format: garbage
//	dcim/cable-terminations cable=null, abc, 1.0, 99999
//	                             400 Select a valid choice. That choice is not one of the available choices.

// maxTreeNodes bounds the nodes one tree filter may match. Each can cost a read
// of the nodes below it, one request apiece, so a filter matching more is
// refused rather than sent as a hundred requests and more.
const maxTreeNodes = 100

// relatedName is one of NetBox's related filters as a query names it: every
// row of one NetBox name, read together, as NetBox mode sends them as one
// parameter.
type relatedName struct {
	name  string // NetBox's name for it: site, site__n
	entry vocabEntry
	first provider.Filter // the first row, which a refusal names
	// values are every value given, across the rows and in their order; the
	// literal null is set apart in null where NetBox's form reads it as no
	// value.
	values []string
	null   bool
	// keys are the values as NetBox compares them with the target's field,
	// and reads as this replica stores them, one each.
	keys, reads []string
	// ids are the target's objects the values name, and the nodes below them
	// for a tree filter.
	ids map[int64]bool
}

// resolveRelatedFilters answers every filter the vocabulary calls related,
// tree or modelchoice (translateFilters passes them on as written, but for a
// single-valued one's last value): each row is replaced, per foreign key,
// by the ids its values name — NetBox's IS NULL for the literal null — and
// NetBox's own refusal where its form refuses a value. none is a combination
// no row can match, so no request is needed. A table the vocabulary does not
// cover keeps its filters as written.
func (p *Provider) resolveRelatedFilters(ctx context.Context, table string, filters []provider.Filter, e entity, c *catalog) (out []provider.Filter, none bool, err error) {
	vocab, covered := vocabFor(table)
	if !covered {
		return filters, false, nil
	}

	// The rows by the foreign key they filter on, then by NetBox name, each
	// in the order first written.
	byColumn := map[string][]*relatedName{}
	var columns []string
	column := make([]string, len(filters))
	for i, f := range filters {
		if sendsNothing(f) {
			continue
		}
		name := netboxName(f)
		alts, ok := vocab[name]
		if !ok {
			continue
		}
		entry := pickVocab(alts, e)
		if entry.Kind != kindRelated && entry.Kind != kindTree && entry.Kind != kindModelChoice {
			continue
		}
		column[i] = entry.Column
		names, seen := byColumn[entry.Column]
		if !seen {
			columns = append(columns, entry.Column)
		}
		at := slices.IndexFunc(names, func(n *relatedName) bool { return n.name == name })
		if at < 0 {
			names = append(names, &relatedName{name: name, entry: entry, first: f})
			at = len(names) - 1
		}
		n := names[at]
		for _, v := range splitValues(f.Value) {
			// Where the form has no null choice (cable-terminations' cable),
			// null is a value like any other, and its checks refuse it as
			// NetBox's do.
			if v == nullValue && entry.Null {
				n.null = true
				continue
			}
			n.values = append(n.values, v)
		}
		byColumn[entry.Column] = names
	}
	if len(columns) == 0 {
		return filters, false, nil
	}
	var all []*relatedName
	for _, col := range columns {
		all = append(all, byColumn[col]...)
	}

	// NetBox's form checks every value before it compares any, and a value
	// it refuses is refused whatever the other filters say; this makes the
	// same checks before anything is read. Then the targets, which must be
	// tables this replica has data in, before any is read.
	for _, n := range all {
		if err := n.check(c); err != nil {
			return nil, false, err
		}
	}
	targets := make([]relatedTarget, len(all))
	for i, n := range all {
		if len(n.values) == 0 {
			continue // null alone needs no target: works where it has no data
		}
		if targets[i], err = n.target(c); err != nil {
			return nil, false, err
		}
	}
	for i, n := range all {
		if len(n.values) == 0 {
			continue
		}
		if err := p.resolve(ctx, n, targets[i], c); err != nil {
			return nil, false, err
		}
	}
	// NetBox's form sets null apart and answers for the other values first —
	// one it cannot read, one it does not find (site_id=null&site_id=abc is
	// "“abc” is not a valid value.", site=null&site=ams9 "Select a valid
	// choice. ams9 …") — so null beside values on = is refused only once they
	// are known to be good. NetBox then returns rows matching either, which
	// the replica, ANDing its filters, cannot.
	for _, n := range all {
		if n.null && len(n.values) > 0 && !n.entry.Exclude {
			return nil, false, n.refuse("NetBox returns rows matching either null or the values, and this replica cannot combine the two; filter on null and on the values in separate queries, or " + netboxModeAdvice)
		}
	}

	resolved := map[string][]provider.Filter{}
	for _, col := range columns {
		fs, unmatched := combineRelated(col, byColumn[col])
		resolved[col] = fs
		none = none || unmatched
	}
	out = make([]provider.Filter, 0, len(filters))
	written := map[string]bool{}
	for i, f := range filters {
		switch col := column[i]; {
		case col == "":
			out = append(out, f)
		case !written[col]:
			written[col] = true
			out = append(out, resolved[col]...)
		}
	}
	return out, none || nullAndNotNull(out), nil
}

// check makes NetBox's checks of the values that need no read, in NetBox's
// order: every value the field cannot parse first, whichever comes first in
// the query, then every value it parses but would not find, being written
// otherwise than the target's field is (NetBox compares str() of that field
// with the value as given). Each sets the values as compared and as read.
func (n *relatedName) check(c *catalog) error {
	if n.entry.Kind == kindModelChoice && (n.entry.ToField == "" || n.entry.ToField == "id") && len(n.values) > 0 {
		// A ModelChoiceFilter reads the last value (translateFilters kept
		// it) as int() reads it, and finds the object by that number: 01 is
		// cable 1 there, and no text is compared.
		v := n.values[len(n.values)-1]
		id, ok := pythonInt(v)
		if !ok {
			return n.refuse(n.invalidChoice(v, c))
		}
		n.values = []string{v}
		n.keys = []string{strconv.FormatInt(id, 10)}
		n.reads = n.keys
		return nil
	}
	switch n.entry.ToField {
	case "", "id":
		// int() first: a value it rejects is NetBox's invalid_pk_value,
		// raised while the values are looked up and so before any is
		// found missing.
		for _, v := range n.values {
			if _, ok := pythonInt(v); !ok {
				return n.refuse(fmt.Sprintf("“%s” is not a valid value. NetBox mode answers the same; %s", v, n.takes(c)))
			}
		}
		for _, v := range n.values {
			if id, _ := pythonInt(v); strconv.FormatInt(id, 10) != v {
				return n.refuse(n.invalidChoice(v, c))
			}
		}
		n.keys, n.reads = n.values, n.values
	case "address":
		// NetBox's IPAddressField reads each value as netaddr's IPNetwork,
		// and compares the address with its mask as NetBox writes it
		// (10.0.0.1/32); the replica stores a single host bare.
		for _, v := range n.values {
			if _, ok := netaddrNetwork(v); !ok {
				return n.refuse(fmt.Sprintf("Invalid IP address format: %s. NetBox mode answers the same; %s", v, n.takes(c)))
			}
		}
		for _, v := range n.values {
			p, _ := netaddrNetwork(v)
			if cidrText(p) != v {
				return n.refuse(n.invalidChoice(v, c))
			}
			n.keys, n.reads = append(n.keys, v), append(n.reads, inetText(p))
		}
	case "mac_address":
		// NetBox's MACAddressField removes spaces and reads netaddr's EUI-48,
		// naming the value so read when it cannot (4.4's wording; 4.6 says
		// “garbage” is not a valid value.), and writes the address in
		// capitals with colons; the replica stores PostgreSQL's macaddr text,
		// lowercase.
		for _, v := range n.values {
			if _, ok := netaddrEUI48(v); !ok {
				return n.refuse(fmt.Sprintf("Invalid MAC address format: %s. NetBox mode answers the same; %s", strings.ReplaceAll(v, " ", ""), n.takes(c)))
			}
		}
		for _, v := range n.values {
			mac, _ := netaddrEUI48(v)
			if strings.ToUpper(mac) != v {
				return n.refuse(n.invalidChoice(v, c))
			}
			n.keys, n.reads = append(n.keys, v), append(n.reads, mac)
		}
	default:
		// A slug, a name, an RD: the text as written, case and all.
		n.keys, n.reads = n.values, n.values
	}
	return nil
}

// relatedTarget is the table a related filter's values are looked up in: the
// object type the catalogue serves it as, and its entry.
type relatedTarget struct {
	objectType string
	e          entity
}

// target is the table of a filter's target model, which must be one this
// replica has data in: a value on any other cannot be checked as NetBox
// checks it, and is refused rather than compared with nothing.
func (n *relatedName) target(c *catalog) (relatedTarget, error) {
	objectType, t, ok := c.servedTable(tableOf(n.entry.Target))
	switch {
	case !ok:
		return relatedTarget{}, n.refuse(fmt.Sprintf("%s cannot be checked on this replica: it does not serve %s; %s", n.first.Field, n.entry.Target, netboxModeAdvice))
	case !t.Ingested:
		return relatedTarget{}, n.refuse(n.unfed(objectType))
	}
	return relatedTarget{objectType: objectType, e: t}, nil
}

func (n *relatedName) unfed(objectType string) string {
	return fmt.Sprintf("%s cannot be checked on this replica: %s has received no data, so its values cannot be matched as NetBox matches them; %s", n.first.Field, objectType, netboxModeAdvice)
}

// resolve looks the values up in the target's table, refuses the first one
// that names nothing, and keeps the ids of what they name, with the nodes
// below each for a tree filter.
func (p *Provider) resolve(ctx context.Context, n *relatedName, t relatedTarget, c *catalog) error {
	pk := t.e.pk()
	field := n.entry.ToField
	if field == "" || field == "id" {
		field = pk
	}
	fields := []string{pk}
	if field != pk {
		fields = append(fields, field)
	}
	// A tree filter on a model the replica's NetBox stores flat (device roles
	// before 4.3, platforms before 4.4) has no tree to read: the nodes named
	// are the answer. A flat model has no parent_id either; every model a tree
	// filter targets that has one is nested by django-mptt, so a target with
	// parent_id and without the tree columns is a replica that leaves them out,
	// and the nodes named alone would drop their descendants without a word.
	tree := n.entry.Kind == kindTree && t.e.has("tree_id") && t.e.has("lft") && t.e.has("rght")
	if n.entry.Kind == kindTree && !tree && t.e.has("parent_id") {
		var missing []string
		for _, col := range []string{"tree_id", "lft", "rght"} {
			if !t.e.has(col) {
				missing = append(missing, col)
			}
		}
		return n.refuse(fmt.Sprintf("%s includes in NetBox the objects below each one named, and %s on this replica has parent_id without %s, the columns those are found by; %s",
			n.first.Field, t.objectType, joinNames(missing), netboxModeAdvice))
	}
	if tree {
		fields = append(fields, "tree_id", "lft", "rght")
	}
	reads := slices.Compact(slices.Sorted(slices.Values(n.reads)))
	q := withFields(url.Values{param(field, "in"): {strings.Join(reads, ",")}}, fields)
	// The client would refuse a lookup past the budget too, but could name
	// only the target's column (slug); here the filter is known as written.
	if !fitsBudget(q) {
		return n.refuse(fmt.Sprintf("%s carries %s, which takes the lookup in %s past the %d KB this datasource sends at once; select fewer values, or %s",
			n.first.Field, valueCount(len(reads)), t.objectType, queryBudget>>10, netboxModeAdvice))
	}
	rows, err := p.readTarget(ctx, n, t, q, field, tree)
	if err != nil {
		return err
	}
	// Rows compare as NetBox compares them, and only those it would find
	// count, whatever the replica's equality took in.
	wanted := map[string]bool{}
	for _, key := range n.keys {
		wanted[key] = true
	}
	found := map[string]bool{}
	var nodes []targetRow
	n.ids = map[int64]bool{}
	for _, r := range rows {
		if wanted[r.key] {
			found[r.key] = true
			n.ids[r.id] = true
			nodes = append(nodes, r)
		}
	}
	for i, key := range n.keys {
		if !found[key] {
			return n.refuse(n.invalidChoice(n.values[i], c))
		}
	}
	if !tree {
		return nil
	}
	if len(nodes) > maxTreeNodes {
		return n.refuse(fmt.Sprintf("%s matches %d objects in %s, and this datasource reads the descendants of at most %d for one filter; select fewer, or %s",
			n.first.Field, len(nodes), t.objectType, maxTreeNodes, netboxModeAdvice))
	}
	// django-mptt keeps a node's descendants in its tree between its lft and
	// rght, so one read finds them. A leaf (rght = lft + 1) has none, and a
	// node inside another one read already is covered by that read.
	slices.SortFunc(nodes, func(a, b targetRow) int { return cmp.Or(cmp.Compare(a.tree, b.tree), cmp.Compare(a.lft, b.lft)) })
	var covering *targetRow
	for i, node := range nodes {
		if node.rght == node.lft+1 || covering != nil && node.tree == covering.tree && node.lft < covering.rght {
			continue
		}
		covering = &nodes[i]
		q := withFields(url.Values{
			param("tree_id", "eq"): {strconv.FormatInt(node.tree, 10)},
			param("lft", "gt"):     {strconv.FormatInt(node.lft-1, 10)},
			param("lft", "lt"):     {strconv.FormatInt(node.rght+1, 10)},
		}, []string{pk})
		below, err := p.readTarget(ctx, n, t, q, pk, false)
		if err != nil {
			return err
		}
		for _, r := range below {
			n.ids[r.id] = true
		}
	}
	return nil
}

// targetRow is a row of a related filter's target: its id, its to-field as
// NetBox writes it, and its place in the tree when one was read.
type targetRow struct {
	id              int64
	key             string
	tree, lft, rght int64
}

// readTarget reads every row of the target a query matches, up to MaxLimit; a
// target that turns out to have no data (the catalogue is up to ten minutes
// old) is the same refusal as one the catalogue has as unfed, and more rows
// than MaxLimit are refused rather than read in part.
func (p *Provider) readTarget(ctx context.Context, n *relatedName, t relatedTarget, q url.Values, field string, tree bool) ([]targetRow, error) {
	raws, total, _, err := p.client.list(ctx, t.objectType, q, MaxLimit)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Classification().Kind == provider.ErrorKindNotReplicated {
			return nil, n.refuse(n.unfed(t.objectType))
		}
		return nil, err
	}
	if total > len(raws) {
		return nil, n.refuse(fmt.Sprintf("%s matches more than %s objects in %s, more than this datasource reads to resolve one filter; narrow it, or %s",
			n.first.Field, thousands(MaxLimit), t.objectType, netboxModeAdvice))
	}
	pk := t.e.pk()
	rows := make([]targetRow, 0, len(raws))
	for _, raw := range raws {
		r, err := decodeTargetRow(raw, pk, field, n.entry.ToField, tree)
		if err != nil {
			return nil, &TransportError{Op: "reading " + t.objectType + " to resolve " + n.first.Field, Err: err, Message: rowShapeGuidance}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// decodeTargetRow reads a target row with its numbers intact (an id past 2^53
// is still that id) and its to-field as NetBox writes it: an address with its
// mask, which the replica leaves off a single host; a MAC address in capitals.
// A row without the columns it was projected onto is the service failing to
// answer.
func decodeTargetRow(raw json.RawMessage, pk, field, toField string, tree bool) (targetRow, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var obj map[string]interface{}
	if err := d.Decode(&obj); err != nil {
		return targetRow{}, err
	}
	if obj == nil {
		return targetRow{}, errMalformedRow
	}
	var r targetRow
	var ok bool
	if r.id, ok = jsonInt(obj[pk]); !ok || r.id <= 0 {
		return targetRow{}, errRowWithoutID
	}
	v, present := obj[field]
	if !present {
		return targetRow{}, errRowWithoutField
	}
	switch v := v.(type) {
	case string:
		r.key = v
	case json.Number:
		r.key = v.String()
	}
	switch {
	case field == pk:
		r.key = strconv.FormatInt(r.id, 10)
	case toField == "address":
		r.key = withSingleHostMask(r.key)
	case toField == "mac_address":
		r.key = strings.ToUpper(r.key)
	}
	if tree {
		for _, col := range []struct {
			name string
			to   *int64
		}{{"tree_id", &r.tree}, {"lft", &r.lft}, {"rght", &r.rght}} {
			if *col.to, ok = jsonInt(obj[col.name]); !ok {
				return targetRow{}, errRowWithoutField
			}
		}
	}
	return r, nil
}

// jsonInt reads an integer decoded with UseNumber.
func jsonInt(v interface{}) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// combineRelated is one foreign key's filter from the names on it, as NetBox
// combines them: one name's values are ORed (its rows are one parameter),
// different names are ANDed, so the ids the names select (P) are intersected,
// and what the negated names exclude (N) is taken off them. A name of null
// alone selects nothing — it is its IS NULL or IS NOT NULL, beside the rest —
// so without a name that selects, the exclusion goes as a negation. Each
// filter keeps the name, as written, of the first row it came from. none is a
// selection with nothing left in it.
func combineRelated(column string, names []*relatedName) ([]provider.Filter, bool) {
	var (
		selected, excluded map[int64]bool
		selLabel, exLabel  string
		nulls              []provider.Filter
	)
	for _, n := range names {
		if n.null {
			nulls = append(nulls, provider.Filter{Field: column, Operator: nullOperator(!n.entry.Exclude), Label: n.first.Field, LabelOperator: n.first.Operator})
		}
		switch {
		case len(n.values) == 0:
		case n.entry.Exclude:
			if excluded == nil {
				excluded, exLabel = map[int64]bool{}, n.first.Field
			}
			maps.Copy(excluded, n.ids)
		case selected == nil:
			selected, selLabel = maps.Clone(n.ids), n.first.Field
		default:
			maps.DeleteFunc(selected, func(id int64, _ bool) bool { return !n.ids[id] })
		}
	}
	var out []provider.Filter
	switch {
	case selected != nil:
		maps.DeleteFunc(selected, func(id int64, _ bool) bool { return excluded[id] })
		if len(selected) == 0 {
			return nulls, true
		}
		out = append(out, provider.Filter{Field: column, Value: idList(selected), Label: selLabel})
	case len(excluded) > 0:
		out = append(out, provider.Filter{Field: column, Operator: "n", Value: idList(excluded), Label: exLabel})
	}
	return append(out, nulls...), false
}

// idList is a set of ids as one filter value, in order.
func idList(ids map[int64]bool) string {
	sorted := slices.Sorted(maps.Keys(ids))
	text := make([]string, len(sorted))
	for i, id := range sorted {
		text[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(text, ",")
}

// refuse is a refusal of the filter, naming it as the user wrote it.
func (n *relatedName) refuse(reason string) error {
	return &UnsupportedFilterError{Field: n.first.Field, Operator: n.first.Operator, Reason: reason}
}

// invalidChoice is NetBox's 400 for a value its filter does not find, in its
// words: a ModelMultipleChoiceField's names the value, a ModelChoiceField's
// does not.
func (n *relatedName) invalidChoice(v string, c *catalog) string {
	if n.entry.Kind == kindModelChoice {
		return "Select a valid choice. That choice is not one of the available choices. NetBox mode answers the same; " + n.takes(c)
	}
	return fmt.Sprintf("Select a valid choice. %s is not one of the available choices. NetBox mode answers the same; %s", v, n.takes(c))
}

// takes ends a refusal of a value with what the filter takes instead: which
// field of which table, written as NetBox writes it.
func (n *relatedName) takes(c *catalog) string {
	target := n.entry.Target
	if objectType, _, ok := c.servedTable(tableOf(n.entry.Target)); ok {
		target = objectType
	}
	switch n.entry.ToField {
	case "", "id":
		return fmt.Sprintf("%s takes the id of an object in %s, as a plain whole number", n.first.Field, target)
	case "address":
		return fmt.Sprintf("%s takes the address of an object in %s as NetBox writes it, with its mask (10.0.0.1/32)", n.first.Field, target)
	case "mac_address":
		return fmt.Sprintf("%s takes the MAC address of an object in %s as NetBox writes it, in capitals (58:56:9F:D1:E3:00)", n.first.Field, target)
	}
	return fmt.Sprintf("%s takes the %s of an object in %s, exactly as NetBox writes it", n.first.Field, n.entry.ToField, target)
}

// tableOf is the replica's table for a NetBox model label: dcim.site is
// dcim_site (see loadVocab).
func tableOf(label string) string {
	return strings.Replace(label, ".", "_", 1)
}
