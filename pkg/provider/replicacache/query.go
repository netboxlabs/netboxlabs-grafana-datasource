package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Query executes an object query and returns flattened, joinable rows.
//
// Almost everything the NetBox provider does in Go happens upstream here:
// filtering, sorting, projection, counting and the resolution of related
// names (expand=) are all query parameters, which is the entire reason this
// backend exists. What remains in process is turning the raw table row into
// the shape the rest of the plugin expects — custom fields out of their JSON
// blob, the aliases a NetBox-mode panel selects, the link — and saying what
// the numbers cannot: how fresh the rows are, and which references the replica
// could not resolve.
func (p *Provider) Query(ctx context.Context, spec provider.QuerySpec) (*provider.Result, error) {
	// Same contradiction the NetBox provider rejects: a caller that reads only
	// the total cannot also be able to do without one.
	if spec.CountOnly && spec.AllowUncounted {
		return nil, fmt.Errorf("invalid query: CountOnly needs a total, AllowUncounted says one is not needed")
	}
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
	e, c, err := p.entityFor(ctx, spec.ObjectType)
	if err != nil {
		return nil, err
	}
	q, err := buildFilterValues(spec.Filters, addressField(e, c))
	if err != nil {
		return nil, err
	}
	// An entity the catalogue has as unfed is NOT refused here: the row route
	// answers 404 for it, classified as not-replicated by the client, and it
	// is current where the catalogue can be ten minutes stale. Nor is the
	// request judged against that entity's catalogue entry — it has no
	// columns until ingested, so every filter would be refused as "no such
	// column", the wrong subject. The bare request goes, and the service
	// answers for itself: 404 while unfed, its own validation once fed. The
	// one thing judged without columns is the operator vocabulary, which is
	// the build's (validateTextOperators).
	var plan request
	if e.Ingested {
		if err := validateFilters(spec.Filters, e, c); err != nil {
			return nil, err
		}
		plan = planRequest(e, spec)
	} else if err := validateTextOperators(spec.Filters, c); err != nil {
		return nil, err
	}

	// expand= goes with every query, count-only included: a filter on an
	// expanded name is only valid under it, and the alert Count path sends the
	// rule's filters with CountOnly. Only the sort and the projection are
	// pointless when nothing but the total is read — and for that caller the
	// smallest page the service returns is enough, since the count in the
	// envelope is for the whole filter and unaffected by the limit.
	if len(plan.expand) > 0 {
		q.Set("expand", strings.Join(plan.expand, ","))
	}
	limit, projected := clampLimit(spec.Limit), plan.fields
	if spec.CountOnly {
		limit, projected = 1, nil
	} else {
		if plan.sort != "" {
			q.Set("sort", plan.sort)
		}
		q = withFields(q, plan.fields)
	}

	raws, total, asOf, err := p.client.list(ctx, spec.ObjectType, q, limit)
	if err != nil {
		return nil, err
	}
	cols, rows, err := flattenRows(raws, projected, e.pk())
	if err != nil {
		return nil, err
	}
	showSingleHostMasks(rows, cols, addressField(e, c))
	if asOf == nil {
		asOf = e.DataAsOf
	}
	notes, warnings := freshnessNotes(c, e, asOf)
	notes, warnings = append(plan.notes, notes...), append(plan.warnings, warnings...)
	if !spec.CountOnly {
		// All of these build columns a count-only caller never reads: it takes
		// Result.Total and nothing else. The third one can also COST something —
		// it reads the custom-field names, one row request when the cache is
		// cold, so an alert counting a multi-million-row table paid for a
		// second list request to decorate rows it discards.
		//
		// Each takes Fields AND KeyFields. A join source is fetched but not
		// displayed, so an alias asked for only as a join key had its physical
		// source projected in and then never built — applyJoinKeys produced an
		// empty output column, and since the backstop covers key fields too,
		// alert evaluation failed on it.
		cols = append(cols, addChoiceValueAliases(selectedFields(spec), rows)...)
		cols = append(cols, addCustomFieldIDAliases(selectedFields(spec), rows)...)
		cols = append(cols, addUnsetCustomFieldColumns(spec, rows)...)
		if addDeepLinks(p.linkBase(c), spec.ObjectType, rows, e.pk()) {
			cols = append(cols, deepLinkColumn)
		}
		warnings = append(warnings, danglingReferenceWarnings(plan.expanded, rows)...)
		warnings = append(warnings, unresolvedRelationWarnings(spec, rows, plan.unavailable)...)
	}

	snapshotComplete := c.SnapshotComplete
	return &provider.Result{
		Columns:          restrictColumns(cols, spec.Fields),
		Rows:             rows,
		Total:            total,
		MaxRows:          MaxLimit,
		Warnings:         warnings,
		Notes:            notes,
		DataAsOf:         asOf,
		SnapshotComplete: &snapshotComplete,
	}, nil
}

// freshnessNotes states how current the rows are. asOf is the instant for
// THIS response (the list envelope's, else the catalogue's). Loading — no
// instant and the tenant-wide snapshot not complete — is a WARNING, because
// the rows may be a fraction of the fleet served as a confident 200, and a
// warning is what keeps an alert rule from evaluating them as the whole. An
// unknown age after the snapshot is a note, and so is an instant the replica
// reported that this datasource could not read: the reader should see the
// value rather than "unknown".
func freshnessNotes(c *catalog, e entity, asOf *time.Time) (notes, warnings []string) {
	switch {
	case asOf != nil:
		notes = append(notes, fmt.Sprintf("Data as of %s (replica-cache).", asOf.UTC().Format("2006-01-02 15:04:05 UTC")))
	case e.DataAsOfRaw != "":
		notes = append(notes, fmt.Sprintf("The age of this data could not be read: the replica reports %q as the commit time.", truncate(e.DataAsOfRaw)))
	case !c.SnapshotComplete:
		warnings = append(warnings, "This replica is still loading its initial snapshot; results may be incomplete and their age is unknown.")
	default:
		notes = append(notes, "The age of this data is unknown: the replica reports no commit time for this entity.")
	}
	return notes, warnings
}

// danglingReferenceWarnings reports, per expanded reference, the rows whose
// id points at an object the replica does not hold: the server's join yields
// a null name beside a real id, which is indistinguishable from "no site" to
// a rule grouping or joining on the name. The normal state while a snapshot
// loads (devices arrive before their sites) and after any create/delete
// window — and the one the client-side cascade used to report.
func danglingReferenceWarnings(expanded []expansion, rows []map[string]interface{}) []string {
	var out []string
	for _, x := range expanded {
		missing := 0
		for _, row := range rows {
			if _, ok := toInt(row[x.via]); ok && row[x.key] == nil {
				missing++
			}
		}
		if missing > 0 {
			out = append(out, fmt.Sprintf(
				"Related names from %s are missing for %d of %d objects: those rows reference objects this replica does not hold, so %q is blank there; the %s column still holds the values.",
				x.target, missing, len(rows), x.key, x.via))
		}
	}
	return out
}

// request is what a QuerySpec becomes on the wire, decided from the catalogue
// before anything is sent. It is a plain value so the derivation can be tested
// without a server.
type request struct {
	fields   []string // stored columns for fields=; nil means every column
	expand   []string // expand= keys, in catalogue order
	sort     string   // sort= value; "" when the ordering was dropped
	notes    []string
	warnings []string
	// unavailable is every requested name an unavailable reference would have
	// produced, so the missing-column backstop does not report it a second time.
	unavailable map[string]bool
	// expanded is what expand carries, with the column each key resolves
	// through and the entity it resolves to, for the dangling-reference check.
	expanded []expansion
}

type expansion struct{ key, via, target string }

// planRequest maps the caller's columns onto the service's vocabulary.
//
// Several columns the caller can ask for are not stored: "site" and "site_slug"
// come from expanding site_id, "cf_tier" from the custom_field_data blob,
// display_url from the primary key. Naming those in fields= is a 400, and
// omitting their source returns the column empty — so each is translated to
// what produces it. A reference whose target has no data on this replica
// cannot be expanded (the service returns the id alone); asking for it is a
// warning naming the cause, never a blank column, and sorting on it a note for
// the same reason.
//
// A reference is expanded when something asks for it by name — a field, a
// join key, a filter, the ordering — and an unprojected query ("All columns")
// that reads rows expands every reference whose target has data, because
// NetBox mode returns related names by default. The service pages the rows
// first and joins only the keys on that page (DATA-404), so resolving all of
// them costs about what resolving none does: on a large replica's 12.9M-row
// dcim/interfaces, all eight references on a 100-row page took 1.35 s, the
// same as no expansion. A count-only query reads no rows and expands only
// what its filters need.
func planRequest(e entity, spec provider.QuerySpec) request {
	r := request{unavailable: map[string]bool{}}
	wantAll := len(spec.Fields) == 0

	// Which references to expand: the ones a filter names — the only ones a
	// count-only caller needs, since it reads nothing but the total — plus,
	// for a caller that reads rows, the ones a requested name resolves to.
	// KeyFields count, by the same rule: a join on "site" needs the name, a
	// join on "site_id" does not.
	expandSet := map[string]bool{}
	for _, f := range spec.Filters {
		// validateFilters has already refused a filter on an unavailable one.
		if via, _, ok := e.expandedColumn(f.Field); ok && via.Ref.Available {
			expandSet[via.Ref.ExpandKey] = true
		}
	}
	warned := map[string]bool{}
	ask := func(name string) {
		via, _, ok := e.expandedColumn(name)
		if !ok {
			return
		}
		if !via.Ref.Available {
			r.unavailable[name] = true
			if !warned[via.Ref.ExpandKey] {
				warned[via.Ref.ExpandKey] = true
				r.warnings = append(r.warnings, unavailableReferenceWarning(via))
			}
			return
		}
		expandSet[via.Ref.ExpandKey] = true
	}
	if !spec.CountOnly {
		for _, name := range selectedFields(spec) {
			ask(name)
		}
	}
	if wantAll && !spec.CountOnly {
		// Every reference whose target has data. One whose target has none is
		// left out without a warning: nobody named it, and Fields does not
		// offer it either.
		for _, col := range e.Columns {
			if col.Ref != nil && col.Ref.Available {
				expandSet[col.Ref.ExpandKey] = true
			}
		}
	}

	// Ordering, which a count-only caller has no use for. Trimmed for the same reason the NetBox path trims (see
	// netbox/ordering.go's orderingValue): the value can come from a
	// provisioned dashboard's YAML rather than the editor's picker, and a
	// stray space is not a different field to a human. Untrimmed, " -name "
	// did not even match the "-" prefix, so a stored descending sort on a
	// perfectly ordinary column was reported as derived and dropped.
	//
	// The seam permits a provider to ignore Ordering as long as it says so.
	// Substituting the id column was the tempting alternative and is worse:
	// site_id order is not site-name order, and the panel would look correctly
	// sorted while being ordered by something the reader cannot see.
	if ordering := strings.TrimSpace(spec.Ordering); ordering != "" && !spec.CountOnly {
		direction, field := "", ordering
		if rest, ok := strings.CutPrefix(field, "-"); ok {
			direction, field = "-", rest
		}
		field = strings.TrimSpace(field)
		via, _, isExpansion := e.expandedColumn(field)
		switch {
		case e.has(field):
			r.sort = direction + field
		case isExpansion && via.Ref.Available:
			expandSet[via.Ref.ExpandKey] = true
			r.sort = direction + field
		case isExpansion:
			r.notes = append(r.notes, fmt.Sprintf(
				"Rows are not sorted by %q: %s has received no data on this replica, so the name cannot be resolved to sort on. Sort by %s instead.",
				field, strings.TrimPrefix(via.Ref.Path, "/v1/"), via.Name))
		default:
			r.notes = append(r.notes, fmt.Sprintf("Rows are not sorted by %q: this replica has no such column.", field))
		}
	}
	for _, col := range e.Columns { // catalogue order, so the parameter is deterministic
		if col.Ref != nil && expandSet[col.Ref.ExpandKey] {
			delete(expandSet, col.Ref.ExpandKey) // once, should two references share a key
			r.expand = append(r.expand, col.Ref.ExpandKey)
			r.expanded = append(r.expanded, expansion{key: col.Ref.ExpandKey, via: col.Name, target: strings.TrimPrefix(col.Ref.Path, "/v1/")})
		}
	}
	if wantAll || spec.CountOnly {
		// An empty Fields means "all columns" — the contract's wording and the
		// editor's default — and KeyFields alone must NOT trigger a projection:
		// they name join-key sources the caller reads but did not ask to see,
		// so projecting onto them would return a panel containing nothing but
		// its join keys. Object queries populate KeyFields whenever a join
		// mapping is configured, so this fired on an ordinary panel left at
		// "All columns" and silently removed every unrelated column from it.
		return r
	}

	// fields=: the stored columns behind what was asked for. The primary key
	// rides along — the service returns it regardless, and the deep link and
	// the row checks read it.
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			r.fields = append(r.fields, name)
		}
	}
	for _, f := range selectedFields(spec) {
		switch {
		case e.has(f):
			add(f)
		case strings.HasSuffix(f, "_value") && e.has(strings.TrimSuffix(f, "_value")):
			// NetBox mode splits a choice into <field> (the label, "Active") and
			// <field>_value (the raw value, "active"). This backend stores the
			// raw value in the physical column and has no labels at all, so the
			// alias is built from it — see addChoiceValueAliases.
			add(strings.TrimSuffix(f, "_value"))
		case strings.HasPrefix(f, "cf_") && e.has(customFieldDataColumn):
			add(customFieldDataColumn)
		}
	}
	add(e.pk())
	return r
}

func unavailableReferenceWarning(col column) string {
	return fmt.Sprintf("%s cannot be resolved: %s has received no data in this replica; %s still holds the value.",
		col.Ref.ExpandKey, strings.TrimPrefix(col.Ref.Path, "/v1/"), col.Name)
}

// clampLimit applies the provider's defaults: unset means defaultLimit, above
// the ceiling means MaxLimit. The per-page size is the client's business.
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// restrictColumns presents the caller's chosen columns, in the order they
// asked for them; an empty request keeps everything. KeyFields are fetched and
// left in the rows but never announced: the caller reads them to build a join
// key and did not ask to see them.
func restrictColumns(cols, requested []string) []string {
	if len(requested) == 0 {
		return cols
	}
	present := map[string]bool{}
	for _, c := range cols {
		present[c] = true
	}
	var out []string
	for _, f := range requested {
		if present[f] {
			out = append(out, f)
		}
	}
	return out
}

// addChoiceValueAliases fills in the <field>_value columns a panel written
// against NetBox mode selects.
//
// There, flattenObject splits a choice object into <field> carrying the LABEL
// ("Active") and <field>_value carrying the raw value ("active"). This backend
// stores choices as the raw value in a plain column and publishes no labels at
// all, so <field> already holds what <field>_value would, and a saved panel
// selecting status_value went blank on switching modes.
//
// Only requested aliases are added. Emitting <field>_value beside every string
// column would double the width of every result for the sake of a name almost
// nobody asks for, and nothing outside a NetBox-mode panel asks for one: the
// editor here offers the physical columns.
func addChoiceValueAliases(fields []string, rows []map[string]interface{}) []string {
	var added []string
	for _, f := range fields {
		base, ok := strings.CutSuffix(f, "_value")
		if !ok || base == "" {
			continue
		}
		// A real column by that name wins, and only for THIS field: a custom
		// choice already flattened to cf_x_value must not stop a status_value
		// later in the same selection from being built, which is what returning
		// here did — the projection then dropped status_value entirely.
		if hasColumn(rows, f) {
			continue
		}
		used := false
		for _, row := range rows {
			v, ok := row[base]
			if !ok {
				continue
			}
			row[f] = v
			used = true
		}
		if used {
			added = append(added, f)
		}
	}
	return added
}

// addCustomFieldIDAliases fills in the cf_<name>_id columns a NetBox-mode panel
// selects for an OBJECT custom field.
//
// There the API expands the field to a nested object, which flattens to
// cf_<name> (the display name) plus cf_<name>_id. The column this mirrors holds
// the bare id, so cf_<name> is that number and the _id column was simply
// absent — a saved panel selecting it got no column and no explanation.
//
// The name still cannot be resolved: the target model is behind a content-type
// id the service does not expose, which is the same wall as the polymorphic
// FKs. But the ID is right there, and it is what a join or a link is built
// from, so withholding it because the label is unavailable helps nobody.
func addCustomFieldIDAliases(fields []string, rows []map[string]interface{}) []string {
	var added []string
	for _, f := range fields {
		base, ok := strings.CutSuffix(f, "_id")
		if !ok || !strings.HasPrefix(base, "cf_") {
			continue
		}
		if hasColumn(rows, f) {
			continue
		}
		used := false
		for _, row := range rows {
			// Only when the value really is an identifier. A text or list custom
			// field named cf_x has no id to offer, and inventing one from a
			// number that is not a key would be worse than the missing column.
			if id, isID := toInt(row[base]); isID {
				row[f] = float64(id)
				used = true
			}
		}
		if used {
			added = append(added, f)
		}
	}
	return added
}

// checkProjection verifies that EVERY row carries the columns the projection
// asked the service for.
//
// Per row, not existentially: a page answering fields=name with
// [{"id":1,"name":"a"},{"id":2}] has the column somewhere, so an "is it
// anywhere" test passes it, and the second object becomes a blank cell — a
// variable option silently dropped, or an alert label that lost its identity.
// FieldValues already refuses exactly this, and it is the same protocol
// violation: the column was requested, so its absence is the service failing to
// answer rather than the object having no value.
//
// A column PRESENT and null is untouched; that is an object with no value
// there.
//
// It runs on the objects as they ARRIVED, not on the flattened rows, because
// custom_field_data does not survive flattening — it becomes cf_* columns — so
// afterwards a row that omitted it is indistinguishable from one that carried
// an empty blob, and the existential check downstream passes as soon as any
// row supplies the cf_* column.
func checkProjection(projected []string, rows []map[string]interface{}) error {
	if len(projected) == 0 {
		return nil
	}
	for _, row := range rows {
		for _, col := range projected {
			if _, ok := row[col]; !ok {
				return &TransportError{
					Op:      "reading " + col,
					Err:     errRowWithoutField,
					Message: rowShapeGuidance,
				}
			}
		}
	}
	return nil
}

// addUnsetCustomFieldColumns gives a requested cf_* column its null values when
// no row has that custom field set.
//
// null and {} are how the blob says "this object has no custom field values",
// so a page where every matched object leaves one unset produces no cf_ key at
// all — and the missing-column backstop then reports the field as one this
// backend does not produce, which alert evaluation turns into an error. A rule
// would fail merely because everything it matched left the field unset, which
// is the healthy state of most such rules.
//
// Gated on the rows carrying any custom field at all. Where they do not, a
// requested cf_* really is a column this deployment cannot produce, and the
// backstop is right to say so.
func addUnsetCustomFieldColumns(spec provider.QuerySpec, rows []map[string]interface{}) []string {
	// The blob is flattened away before this runs, so "the entity has the
	// blob" is read off the rows' cf_* keys.
	if len(rows) == 0 || !anyCustomField(rows) {
		return nil
	}
	var added []string
	for _, f := range selectedFields(spec) {
		if !strings.HasPrefix(f, "cf_") {
			continue
		}
		// A count is zero, not null. The shared contract gives an empty list a
		// _count of float64(0), and an all-null column is typed as strings by
		// buildFrame — so a threshold or numeric transformation that worked in
		// NetBox mode would stop working here. Unset and empty are different
		// things in NetBox, but for a COUNT they mean the same one.
		// A _count suffix is not proof that this is a list's derived count: a
		// custom field can be NAMED service_count, and NetBox shows an unset one
		// as null. So the base has to be a column this entity actually has, and
		// the rows in hand are the whole of the evidence: a defined custom field
		// is present in every row's blob (measured), so a wider read could not
		// find what these rows lack. Without it, the suffix is treated as part
		// of the field's own name.
		isCount := false
		if base, ok := strings.CutSuffix(f, "_count"); ok {
			// Direct evidence first. The flattener emits cf_X_count only
			// ALONGSIDE cf_X for the same object, so a row carrying the count
			// without the base proves the name came out of the blob literally —
			// a custom field really called service_count, next to a separate
			// one called service. NetBox shows an unset literal field as null,
			// and the base-name heuristic alone would fill it with zero.
			literal := false
			for _, row := range rows {
				_, hasCount := row[f]
				_, hasBase := row[base]
				if hasCount && !hasBase {
					literal = true
					break
				}
			}
			// Otherwise the base's existence in these rows is the evidence.
			if !literal {
				isCount = hasColumn(rows, base)
			}
		}
		var unset interface{}
		if isCount {
			unset = float64(0)
		}
		// ONLY the derived count is synthesized. A defined custom field is
		// present in the blob even when unset — measured on a live instance,
		// where every dcim/devices row carries the same six keys with three of
		// them null — so flattening already gives it a column, and a cf_* that
		// is absent from every row is one this deployment does not have:
		// deleted, or mistyped in a saved query. Filling it with nulls hid that
		// from the missing-column backstop and let an alert evaluate a column of
		// nothing as authoritative.
		//
		// A count is different because it is DERIVED rather than a blob key: an
		// object whose list is null produces no _count at all, so the mixed page
		// genuinely needs filling and the base's presence is the evidence that
		// it exists.
		if !isCount {
			continue
		}
		// PER ROW, not once for the page. A mixed result — some objects with
		// the field set, some without — already carries the column, so deciding
		// existentially skipped the whole synthesis and left the unset rows
		// without it, which is the difference the contract exists to remove.
		existed := hasColumn(rows, f)
		filled := false
		for _, row := range rows {
			if _, ok := row[f]; !ok {
				row[f] = unset
				filled = true
			}
		}
		if filled && !existed {
			added = append(added, f)
		}
	}
	return added
}

// anyCustomField reports whether any row carries a flattened custom field,
// which is how "this entity has the blob" reads once flattening has run.
func anyCustomField(rows []map[string]interface{}) bool {
	for _, row := range rows {
		for k := range row {
			if strings.HasPrefix(k, "cf_") {
				return true
			}
		}
	}
	return false
}

func hasColumn(rows []map[string]interface{}, name string) bool {
	for _, row := range rows {
		if _, ok := row[name]; ok {
			return true
		}
	}
	return false
}

// unresolvedRelationWarnings reports every column the caller asked for that the
// result does not contain.
//
// Not only relationships. It first covered those alone, on the reasoning that a
// field this deployment simply does not have is the projection's business
// rather than a degradation — which was wrong for the same reason everything
// else in this file is: absence is indistinguishable from emptiness. A saved
// panel selecting a NetBox-computed column such as ipam/prefixes.utilization
// gets nothing here, and nothing said so, while alert evaluation treats an
// empty Warnings list as permission to run.
//
// A blank column at least reads as a blank; a missing one a panel selected is
// invisible.
func unresolvedRelationWarnings(spec provider.QuerySpec, rows []map[string]interface{}, skip map[string]bool) []string {
	// Both empty, not just Fields. "All columns" with a join mapping leaves
	// Fields empty while KeyFields still names a source that has to exist, and
	// skipping on Fields alone let applyJoinKeys announce an output column that
	// was blank on every row with nothing to say why. With Fields empty the
	// loop below sees only the key fields, which is right: every column the
	// backend can produce is already present.
	if (len(spec.Fields) == 0 && len(spec.KeyFields) == 0) || len(rows) == 0 {
		// No rows is not a degradation. hasColumn is false for every field when
		// there is nothing to observe a column in, so an ordinary empty result —
		// an offline-device rule while nothing is offline — would report every
		// requested column as missing, and degradationError turns that into a
		// rule in Error rather than a healthy empty evaluation. The most common
		// state of a working alert is the one this would have broken.
		return nil
	}
	var out []string
	// KeyFields as well as Fields. A join source is deliberately absent from
	// cols — the caller reads it without displaying it — so this asks the ROWS
	// whether the value was built. Checking cols would have reported every join
	// key as missing, and omitting them let a join announce an output column
	// with an empty value on every row, silently.
	for _, f := range selectedFields(spec) {
		// skip holds the names an unavailable reference would have produced;
		// planRequest already said why those are missing.
		if skip[f] || hasColumn(rows, f) {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%q is not a column this backend produces for this object type, so it is missing from this result.", f))
	}
	return out
}

// selectedFields is every column the caller needs the value of: the ones it
// asked to SEE plus the ones it reads to build a join key. Three separate
// places have now had to learn that KeyFields count, so it is one function.
func selectedFields(spec provider.QuerySpec) []string {
	return append(append(make([]string, 0, len(spec.Fields)+len(spec.KeyFields)),
		spec.Fields...), spec.KeyFields...)
}

// toInt reads a primary key or foreign key. Every caller wants an identifier,
// so the bar is a positive whole number that fits: JSON has one number type, and
// an id of 1.9 silently truncated to 1 would be a DIFFERENT object in every
// lookup and deep link built from it, while 0, a negative and a non-finite
// value are not identifiers at all.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return fromFloat(n)
	case int:
		return n, n > 0
	case json.Number:
		i, err := n.Int64()
		if err != nil || i <= 0 {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// maxExactID bounds ids at the range where float64 is INJECTIVE, which is a
// stronger property than being exactly representable and is the one that
// matters here.
//
// Ids arrive through encoding/json as float64, and the rounding happens during
// decode, before anything here can inspect the value: the wire integer
// 9007199254740993 is already 9007199254740992 by the time it is checked, so a
// bound that merely excluded inexact values could not detect the collision.
// Below 2^53 no two integers share a float64, so an accepted value names
// exactly one object. 2^53 itself is excluded because 2^53+1 rounds onto it.
//
// The alternative — decoding rows with json.Number to keep the token — would
// change the type of every value in every row, and with it frame building, to
// defend a boundary no NetBox instance reaches: its keys are 64-bit, but 2^53
// is nine quadrillion rows.
const maxExactID = 1 << 53

func fromFloat(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f <= 0 || f >= maxExactID {
		return 0, false
	}
	return int(f), true
}

// withFields sets the projection parameter. The service always returns the
// primary key regardless, which the row checks and the deep link rely on.
func withFields(q url.Values, cols []string) url.Values {
	if len(cols) == 0 {
		return q
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("fields", strings.Join(cols, ","))
	return q
}

// flattenRows converts raw table rows into flat column/value maps.
//
// Rows are already flat — the service serves database columns — so the only
// real work is custom_field_data, which arrives as a JSON document encoded in a
// string. It is expanded to cf_<name> columns to match what the NetBox provider
// produces from the API's custom_fields object, so the same panel reads the
// same column from either backend.
// rowShapeGuidance is shared by both row-shape failures: the cause is the same
// and so is the thing to check.
const rowShapeGuidance = "Replica cache returned a result row that is not a usable object. The service is reachable but answered with something unexpected."

func flattenRows(raws []json.RawMessage, projected []string, pk string) ([]string, []map[string]interface{}, error) {
	var (
		cols []string
		seen = map[string]bool{}
		out  = make([]map[string]interface{}, 0, len(raws))
	)
	addCol := func(name string) {
		if !seen[name] {
			seen[name] = true
			cols = append(cols, name)
		}
	}

	objs := make([]map[string]interface{}, 0, len(raws))
	seenIDs := make(map[int]bool, len(raws))
	for _, raw := range raws {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, nil, &TransportError{
				Op:      "reading a result row",
				Err:     err,
				Message: rowShapeGuidance,
			}
		}
		if obj != nil {
			if id, ok := toInt(obj[pk]); ok {
				// A repeated id means one object came back twice and another
				// never did, while len(rows) still reaches the reported total —
				// so the result looks complete and an alert evaluates it.
				//
				// Safe to call a protocol violation rather than concurrency:
				// the service's cursor is a KEYSET, measured by decoding it.
				// With sparse ids it reads [185205,185205] — the last row's id
				// in both slots, not an offset — so each page asks for ids after
				// the last one seen, and inserts or deletes elsewhere cannot
				// make a row repeat.
				if seenIDs[id] {
					return nil, nil, &TransportError{
						Op:      "reading a result row",
						Err:     errDuplicateRow,
						Message: "Replica cache returned the same object twice, so some rows are missing from a result that otherwise looks complete. The service is reachable but answered with something unexpected.",
					}
				}
				seenIDs[id] = true
			} else {
				// Measured: the service returns the primary key on every
				// projection, even one that did not ask for it — `fields=serial`
				// comes back as {id, serial} — which is the same invariant the
				// deep-link column already relies on. A row without one
				// identifies no object, and {"count":1,"results":[{}]} would
				// otherwise be one empty row with a matching total, which an
				// alert-table query turns into a value of 1.
				return nil, nil, &TransportError{
					Op:      "reading a result row",
					Err:     errRowWithoutID,
					Message: rowShapeGuidance,
				}
			}
		}
		if obj == nil {
			// A JSON null decodes into the map without error and leaves it nil.
			// Skipping it silently drops a row the count still includes; keeping
			// it appends an EMPTY row, which an alert-table query turns into a
			// value of 1 — a spurious alert built out of a malformed response.
			return nil, nil, &TransportError{
				Op:      "reading a result row",
				Err:     errMalformedRow,
				Message: rowShapeGuidance,
			}
		}
		objs = append(objs, obj)
	}

	// Before anything is consumed. custom_field_data is gone from the flattened
	// rows — it becomes cf_* columns — so a row that omitted it could not be
	// told from one that carried an empty blob once flattening had run, and the
	// existential check downstream passed as soon as ANY row supplied the cf_*
	// column. Checked here, against the objects as they arrived.
	if err := checkProjection(projected, objs); err != nil {
		return nil, nil, err
	}

	for _, obj := range objs {
		row := make(map[string]interface{}, len(obj)+4)
		for k, v := range obj {
			if k == customFieldDataColumn {
				if err := expandCustomFields(v, row); err != nil {
					return nil, nil, err
				}
				continue
			}
			row[k] = v
		}
		out = append(out, row)
	}

	// Announce every column any row carries, in a stable order. Sorting rather
	// than preserving document order is deliberate: Go randomizes map iteration,
	// and a column list that reordered between refreshes would reorder the
	// panel's table on every refresh.
	for _, row := range out {
		names := make([]string, 0, len(row))
		for k := range row {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			addCol(k)
		}
	}
	return cols, out, nil
}

// expandCustomFields writes the blob's fields onto row as cf_<name> columns,
// through the SHARED contract rather than a copy of it. A custom field holding
// a list or an object was previously written straight into cf_<name>, so a
// panel selecting the cf_<name>_count that NetBox mode produces got no such
// column, and the list itself rendered in a different format.
//
// What cannot be matched is the OBJECT custom field. NetBox's API expands it to
// a nested object; the column this mirrors holds the bare id, and the target
// model is named by a content-type id the service does not expose — the same
// wall as the polymorphic FKs. So cf_<name> is that id, and no _id/_slug
// follow it.
func expandCustomFields(v interface{}, row map[string]interface{}) error {
	cf, err := customFields(v)
	if err != nil {
		return err
	}
	// The blob is the same object NetBox's API serves as custom_fields, so
	// the contract's own branch for it produces the columns — one place for
	// the cf_ prefix, the list _count, the object id.
	provider.FlattenField("custom_fields", cf, func(n string, val interface{}) {
		row[n] = val
	})
	return nil
}

// customFields decodes the custom_field_data blob. It is a JSON object encoded
// as a string, so it needs a second decode; anything else is ignored rather
// than guessed at.
func customFields(v interface{}) (map[string]interface{}, error) {
	// Absent or blank is a real answer: this object has no custom fields.
	if v == nil {
		return nil, nil
	}
	bad := func() error {
		// Returning nil here read as "no custom fields", so the physical column
		// was dropped, every requested cf_* column vanished, and nothing said
		// why — a dashboard quietly short of data, and an alert-table query
		// proceeding without the labels it asked for.
		return &TransportError{
			Op:      "reading custom_field_data",
			Err:     errMalformedCustomFields,
			Message: "Replica cache returned custom field data that could not be read. The service is reachable but answered with something unexpected.",
		}
	}
	switch t := v.(type) {
	case map[string]interface{}:
		// Tolerated: the column is a JSON string today, but a service that sent
		// the object itself would be giving us the same thing in a better shape.
		return t, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(t), &m); err != nil || m == nil {
			return nil, bad()
		}
		return m, nil
	}
	return nil, bad()
}
