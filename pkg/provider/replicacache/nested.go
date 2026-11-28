package replicacache

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A nested column, <ref>__<col>, is NetBox mode's (netbox/nested.go): column
// <col> of the object the reference <ref> names, as an Objects query on that
// object's table shows it (a related object by its display, a choice by its
// label, an id as stored). NetBox nests a short form of the object in the row;
// the replica's row holds the reference's id alone, so the referenced rows are
// read by id, once for every reference named, and shaped as Query shapes rows.
//
// Only names a query asks for are computed (Fields, or a join key's source); a
// Fields name is a column whatever its value, a KeyFields one only in the rows,
// so "All columns" keeps its shape. Every row gets the key, null included, so
// the missing-column backstop never reports one a second time, and an empty
// column says why in a sentence that quotes its name, which is how the alert
// table finds a value field's reason.
//
// Where the two modes differ: the replica holds the whole referenced row, so a
// stored column NetBox's short form leaves out (a site's region) is answered
// here and empty there; a column NetBox computes (a device type's
// device_count, an address's family) is answered there and empty here.

// nestedName is one nested column a query asks for.
type nestedName struct {
	name, ref, col string
	// via is the reference column, when ref is one the catalogue declares.
	via      column
	declared bool
}

// nestedNames is the nested columns spec asks for on e, each once, in the order
// asked: names spelt <ref>__<col> that aren't e's own columns.
func nestedNames(e entity, spec provider.QuerySpec) []nestedName {
	var out []nestedName
	seen := map[string]bool{}
	for _, name := range selectedFields(spec) {
		ref, col, ok := provider.NestedColumn(name)
		if !ok || seen[name] || e.has(name) {
			continue
		}
		seen[name] = true
		n := nestedName{name: name, ref: ref, col: col}
		n.via, n.declared = referenceByKey(e, ref)
		out = append(out, n)
	}
	return out
}

// referenceByKey is e's reference column whose expand key is key: NetBox's
// property name for it (device_type, through device_type_id).
func referenceByKey(e entity, key string) (column, bool) {
	for _, c := range e.Columns {
		if c.Ref != nil && c.Ref.ExpandKey == key {
			return c, true
		}
	}
	return column{}, false
}

// isNestedOn reports whether name is a nested column on e rather than one of
// its own.
func isNestedOn(e entity, name string) bool {
	_, _, ok := provider.NestedColumn(name)
	return ok && !e.has(name)
}

// nestedAdditions is what the nested columns add to a result.
type nestedAdditions struct {
	cols, notes, warnings []string
}

// addNestedColumns computes spec's nested columns into rows. The referenced
// rows' reads note their age in st. shown is the warnings the result already
// carries, so a reason the reference's own column gave isn't given twice.
func (p *Provider) addNestedColumns(ctx context.Context, c *catalog, e entity, spec provider.QuerySpec, plan request, rows []map[string]interface{}, st *readStats) (nestedAdditions, error) {
	var out nestedAdditions
	names := nestedNames(e, spec)
	if len(names) == 0 {
		return out, nil
	}
	for _, n := range names {
		for _, row := range rows {
			if _, ok := row[n.name]; !ok {
				row[n.name] = nil
			}
		}
		if slices.Contains(spec.Fields, n.name) {
			out.cols = append(out.cols, n.name)
		}
	}
	// No rows, nothing to say about them: an empty result is not a gap.
	if len(rows) == 0 {
		return out, nil
	}
	expanded := map[string]bool{}
	for _, x := range plan.expanded {
		expanded[x.key] = true
	}
	warn := func(w string) {
		if !slices.Contains(plan.warnings, w) && !slices.Contains(out.warnings, w) {
			out.warnings = append(out.warnings, w)
		}
	}
	var refs []string
	byRef := map[string][]nestedName{}
	for _, n := range names {
		switch {
		case n.col == "id" && (n.declared || e.has(n.ref+"_id")):
			// The reference's id, which the row holds whatever the replica holds
			// of its target (one of several types, or one with no data).
			src := n.ref + "_id"
			if n.declared {
				src = n.via.Name
			}
			for _, row := range rows {
				row[n.name] = row[src]
			}
		case !n.declared:
			warn(undeclaredNestedWarning(e, spec.ObjectType, n))
		case !n.via.Ref.Available:
			warn(unavailableReferenceWarning(n.via))
		case n.col == "display":
			// The display the row shows for the reference (showRelatedDisplays
			// was asked for it); a referenced object the replica doesn't hold is
			// the reference's own dangling warning.
			for _, row := range rows {
				if _, set := toInt(row[n.via.Name]); set {
					row[n.name] = row[n.ref]
				}
			}
		default:
			if _, ok := byRef[n.ref]; !ok {
				refs = append(refs, n.ref)
			}
			byRef[n.ref] = append(byRef[n.ref], n)
		}
	}
	for _, ref := range refs {
		if err := p.fillNested(ctx, c, rows, byRef[ref], expanded[ref], st, warn, &out); err != nil {
			return nestedAdditions{}, err
		}
	}
	return out, nil
}

// fillNested reads the rows one reference names and fills its nested columns.
// ownWarning is whether the result counts the reference's missing objects
// already (the reference is expanded for a column of its own).
func (p *Provider) fillNested(ctx context.Context, c *catalog, rows []map[string]interface{}, names []nestedName, ownWarning bool, st *readStats, warn func(string), out *nestedAdditions) error {
	via := names[0].via
	objectType := strings.TrimPrefix(via.Ref.Path, "/v1/")
	te, _, err := p.entityFor(ctx, objectType)
	if err != nil {
		if unknownEntity(err) {
			warn(unavailableReferenceWarning(via))
			return nil
		}
		return err
	}
	var read []nestedName
	for _, n := range names {
		via2, _, expansion := te.expandedColumn(n.col)
		switch {
		case te.has(n.col):
			read = append(read, n)
		case expansion && !via2.Ref.Available:
			warn(fmt.Sprintf("%q is empty: %s has received no data in this replica; %s__%s still holds the value.",
				n.name, strings.TrimPrefix(via2.Ref.Path, "/v1/"), n.ref, via2.Name))
		case isNestedOn(te, n.col):
			warn(fmt.Sprintf("%q is empty: %s has no column %s; a nested column reads one level.", n.name, objectType, n.col))
		case netboxComputed(n.col) || strings.HasPrefix(n.col, "_"):
			// NetBox's underscore fields a short form carries (_depth, _occupied)
			// are computed too; a stored one (_name) is a column, above.
			warn(fmt.Sprintf("%q is computed by NetBox and is not available in replica-cache mode, so it is empty here. Use a datasource in NetBox mode for it.", n.name))
		default:
			// An expansion, a choice's _value or an alias: the read decides.
			read = append(read, n)
		}
	}
	if len(read) == 0 {
		return nil
	}
	var ids []int
	seen := map[int]bool{}
	for _, row := range rows {
		if id, ok := toInt(row[via.Name]); ok && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		for _, n := range read {
			out.notes = append(out.notes, fmt.Sprintf("%q is empty: no row has a %s set.", n.name, n.ref))
		}
		return nil
	}
	cols := make([]string, 0, len(read))
	for _, n := range read {
		cols = append(cols, n.col)
	}
	targets, displayWarnings, err := p.readNestedTargets(ctx, c, te, objectType, cols, ids, st)
	if err != nil {
		return err
	}
	missing := 0
	// The second hop: a referenced row whose own reference names an object the
	// replica doesn't hold yet, by nested column.
	secondMissing := map[string]int{}
	for _, row := range rows {
		id, set := toInt(row[via.Name])
		if !set {
			continue
		}
		target, ok := targets[id]
		if !ok {
			missing++
			continue
		}
		for _, n := range read {
			row[n.name] = target[n.col]
			if via2, _, expansion := te.expandedColumn(n.col); expansion && target[n.col] == nil {
				if _, set := toInt(target[via2.Name]); set {
					secondMissing[n.name]++
				}
			}
		}
	}
	blank := func(n nestedName, count int, of string) {
		warn(fmt.Sprintf("%q is blank on %s of %s rows: they reference %s objects this replica does not hold yet.",
			n.name, provider.HumanInt(count), provider.HumanInt(len(rows)), of))
	}
	for _, n := range read {
		if len(targets) > 0 && !hasTargetColumn(targets, n.col) {
			warn(fmt.Sprintf("%q is empty: %s has no column %s.", n.name, objectType, n.col))
			continue
		}
		if missing > 0 && !ownWarning {
			blank(n, missing, objectType)
		}
		if via2, _, expansion := te.expandedColumn(n.col); expansion && secondMissing[n.name] > 0 {
			blank(n, secondMissing[n.name], strings.TrimPrefix(via2.Ref.Path, "/v1/"))
		}
		// The target's display sentences, about its column, are about this one.
		prefix := fmt.Sprintf("%q ", n.col)
		for _, w := range displayWarnings {
			if strings.HasPrefix(w, prefix) {
				warn(fmt.Sprintf("%q ", n.name) + strings.TrimPrefix(w, prefix))
			}
		}
	}
	return nil
}

// hasTargetColumn reports whether any referenced row has column col.
func hasTargetColumn(targets map[int]map[string]interface{}, col string) bool {
	for _, t := range targets {
		if _, ok := t[col]; ok {
			return true
		}
	}
	return false
}

// readNestedTargets reads objectType's rows with ids, the columns cols, shaped
// as Query shapes its rows: addresses with their masks, MACs in capitals,
// related objects by NetBox's display, choices by label and with their
// _value, custom-field id aliases. Keyed by id. Its age goes into st; the
// display warnings come back for the caller to word for its columns.
func (p *Provider) readNestedTargets(ctx context.Context, c *catalog, te entity, objectType string, cols []string, ids []int, st *readStats) (map[int]map[string]interface{}, []string, error) {
	// An expanded column's reference rides along, so a name the replica can't
	// join in yet is told from one that isn't set.
	var keys []string
	for _, col := range cols {
		if via, _, ok := te.expandedColumn(col); ok {
			keys = append(keys, via.Name)
		}
	}
	spec := provider.QuerySpec{ObjectType: objectType, Fields: cols, KeyFields: keys}
	plan := planRequest(te, spec)
	params := url.Values{}
	if len(plan.expand) > 0 {
		params.Set("expand", strings.Join(plan.expand, ","))
	}
	params = withFields(params, plan.fields)
	raws, _, err := p.listByIDs(ctx, objectType, te.pk(), intStrings(ids), params, st)
	if err != nil {
		return nil, nil, err
	}
	tcols, rows, err := flattenRows(raws, plan.fields, te.pk())
	if err != nil {
		return nil, nil, err
	}
	showSingleHostMasks(rows, tcols, addressField(te, c))
	showMACs(rows, tcols, macField(te, c))
	warnings, err := p.showRelatedDisplays(ctx, c, te, plan.expanded, rows, func(key string) bool { return slices.Contains(cols, key) })
	if err != nil {
		return nil, nil, err
	}
	addChoiceValueAliases(cols, rows)
	applyChoiceLabels(objectType, rows, false)
	addCustomFieldIDAliases(cols, rows)
	out := make(map[int]map[string]interface{}, len(rows))
	for _, row := range rows {
		if id, ok := toInt(row[te.pk()]); ok {
			out[id] = row
		}
	}
	return out, warnings, nil
}

// undeclaredNestedWarning is the reason for a nested column whose reference
// the catalogue doesn't declare: one the replica holds another way (a
// reference to one of several types, a foreign key without a declared target,
// a choice or JSON NetBox flattens) can't be read here, and anything else is a
// name to check.
func undeclaredNestedWarning(e entity, objectType string, n nestedName) string {
	if e.has(n.ref) || e.has(n.ref+"_id") || e.has(n.ref+"_type_id") {
		return fmt.Sprintf("%q is empty: replica-cache mode can't read %s's own fields. Use a datasource in NetBox mode for it.", n.name, n.ref)
	}
	return fmt.Sprintf("%q is empty: %s has no reference %s. Check the name.", n.name, objectType, n.ref)
}
