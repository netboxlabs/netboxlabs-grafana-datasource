package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Query executes an object query and returns flattened, joinable rows.
//
// Almost everything the NetBox provider does in Go happens upstream here:
// filtering, sorting, projection and counting are all query parameters, which
// is the entire reason this backend exists. What remains in process is turning
// the raw table row into the shape the rest of the plugin expects — custom
// fields out of their JSON blob, and foreign keys resolved to names.
func (p *Provider) Query(ctx context.Context, spec provider.QuerySpec) (*provider.Result, error) {
	// Same contradiction the NetBox provider rejects: a caller that reads only
	// the total cannot also be able to do without one.
	if spec.CountOnly && spec.AllowUncounted {
		return nil, fmt.Errorf("invalid query: CountOnly needs a total, AllowUncounted says one is not needed")
	}
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
	if err := p.validateObjectType(ctx, spec.ObjectType); err != nil {
		return nil, err
	}

	if needsTypeCheck(spec.Filters) {
		// A sampling FAILURE is reported as itself. Only a successful sample
		// that simply has no type for the column reaches validateFilterTypes,
		// which fails closed on it — the two look alike as a nil map and need
		// opposite answers.
		types, err := p.columnTypes(ctx, spec.ObjectType)
		if err != nil {
			return nil, err
		}
		if err := validateFilterTypes(spec.Filters, types); err != nil {
			return nil, err
		}
	}
	q, err := buildFilterValues(spec.Filters)
	if err != nil {
		return nil, err
	}

	limit := spec.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	// A count-only caller reads Total and nothing else, so fetch the smallest
	// page the service will return. The count in the envelope is for the whole
	// filter and is unaffected by the limit.
	if spec.CountOnly {
		limit = 1
	}

	// Sorting is only accepted on columns that physically exist: the service
	// answers `sort=site` with 400 "unknown sort column: site" rather than
	// ignoring it. Passing a derived column straight through would therefore
	// turn a saved panel into an error toast the moment someone sorts by a
	// related object's name.
	//
	// The seam permits a provider to ignore Ordering as long as it says so, so
	// the unsortable request is dropped and stated instead. Substituting the id
	// column was the tempting alternative and is worse: site_id order is not
	// site-name order, and the panel would look correctly sorted while being
	// ordered by something the reader cannot see.
	var notes []string
	if ordering := strings.TrimSpace(spec.Ordering); ordering != "" {
		// Trimmed for the same reason the NetBox path trims (see
		// netbox/ordering.go's orderingValue): the value can come from a
		// provisioned dashboard's YAML rather than the editor's picker, and a
		// stray space is not a different field to a human. Untrimmed, " -name "
		// did not even match the "-" prefix, so a stored descending sort on a
		// perfectly ordinary column was reported as derived and dropped.
		direction, field := "", ordering
		if rest, ok := strings.CutPrefix(field, "-"); ok {
			direction, field = "-", rest
		}
		field = strings.TrimSpace(field)
		ordering = direction + field
		raw, rerr := p.rawColumns(ctx, spec.ObjectType)
		switch {
		case rerr == nil && len(raw) > 0 && raw[field]:
			q.Set("sort", ordering)
		case rerr == nil && len(raw) > 0:
			notes = append(notes, fmt.Sprintf(
				"Rows are not sorted by %q: this backend sorts only on stored columns, and that one is derived from %s_id. Sort by %s_id instead, or use a datasource in NetBox mode.",
				field, field, field))
		default:
			// The schema could not be read, so we cannot tell a stored column
			// from a derived one. Pushing the sort anyway fails CLOSED in the
			// worst way: a derived column is answered with 400 "unknown sort
			// column", turning an optional ordering into a dead panel. Dropping
			// it costs the ordering and says so, which the seam permits.
			notes = append(notes, fmt.Sprintf(
				"Rows are not sorted by %q: this backend sorts only on stored columns, and its schema could not be read to confirm that one is stored. Retry, or use a datasource in NetBox mode.",
				field))
		}
	}

	// Projection. Ask only for the columns needed to build what was requested.
	if !spec.CountOnly {
		if cols, ok := p.projectColumns(ctx, spec); ok {
			q = withFields(q, cols)
		}
	}

	raws, total, err := p.client.list(ctx, spec.ObjectType, q, limit)
	if err != nil {
		return nil, err
	}

	cols, rows, err := flattenRows(raws)
	if err != nil {
		return nil, err
	}
	cols = append(cols, addChoiceValueAliases(spec.Fields, rows)...)
	var warnings []string
	if !spec.CountOnly {
		if addDeepLinks(p.netboxURL, spec.ObjectType, rows) {
			cols = append(cols, deepLinkColumn)
		}
		added, warns := p.resolveFKs(ctx, spec.ObjectType, rows)
		cols = append(cols, added...)
		warnings = warns
	}

	// Present the caller's chosen columns, in the order they asked for them.
	// KeyFields are fetched and left in the rows but never announced: the caller
	// reads them to build a join key and did not ask to see them.
	if len(spec.Fields) > 0 {
		present := map[string]bool{}
		for _, c := range cols {
			present[c] = true
		}
		var out []string
		for _, f := range spec.Fields {
			if present[f] {
				out = append(out, f)
			}
		}
		cols = out
	}

	return &provider.Result{
		Columns:  cols,
		Rows:     rows,
		Total:    total,
		MaxRows:  MaxLimit,
		Warnings: warnings,
		Notes:    notes,
	}, nil
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

func hasColumn(rows []map[string]interface{}, name string) bool {
	for _, row := range rows {
		if _, ok := row[name]; ok {
			return true
		}
	}
	return false
}

// projectColumns maps the caller's requested columns onto the columns that
// exist upstream.
//
// The mapping is needed because several columns the caller can ask for are ours
// rather than the service's: "site" is built from site_id, "cf_tier" out of the
// custom_field_data blob. Naming those upstream is an error ("unknown column"),
// and omitting their SOURCE would return the column empty.
//
// It reports ok=false when any requested column cannot be accounted for, and
// the caller then asks for every column. That fallback is deliberate: an
// unrecognized field usually means a saved dashboard naming something this
// deployment no longer has, and fetching a wider row is a cost, while
// projecting it away is a blank column with no explanation.
func (p *Provider) projectColumns(ctx context.Context, spec provider.QuerySpec) ([]string, bool) {
	// An empty Fields means "all columns" — the contract's wording and the query
	// editor's default. KeyFields alone must NOT trigger a projection: they name
	// join-key sources the caller reads but did not ask to see, so projecting
	// onto them would return a panel containing nothing but its join keys.
	//
	// Object queries populate KeyFields whenever a join mapping is configured,
	// so this fired on an ordinary panel left at "All columns" and silently
	// removed every unrelated column from it.
	if len(spec.Fields) == 0 {
		return nil, false
	}
	raw, err := p.rawColumns(ctx, spec.ObjectType)
	if err != nil || len(raw) == 0 {
		return nil, false
	}

	want := map[string]bool{}
	for _, f := range append(append([]string{}, spec.Fields...), spec.KeyFields...) {
		switch {
		case raw[f]:
			want[f] = true
		case raw[f+"_id"]:
			// A resolved name is built from its id column.
			want[f+"_id"] = true
		case strings.HasSuffix(f, "_slug") && raw[strings.TrimSuffix(f, "_slug")+"_id"]:
			want[strings.TrimSuffix(f, "_slug")+"_id"] = true
		case strings.HasSuffix(f, "_value") && raw[strings.TrimSuffix(f, "_value")]:
			// NetBox mode splits a choice into <field> (the label, "Active") and
			// <field>_value (the raw value, "active"). This backend stores the
			// raw value in the physical column and has no labels at all, so the
			// alias is built from it — see addChoiceValueAliases.
			want[strings.TrimSuffix(f, "_value")] = true
		case strings.HasPrefix(f, "cf_") && raw["custom_field_data"]:
			want["custom_field_data"] = true
		case f == deepLinkColumn:
			// Built from the primary key, which the service returns on every
			// projection regardless, so nothing extra needs requesting.
			want["id"] = true
		default:
			return nil, false
		}
	}
	out := make([]string, 0, len(want))
	for c := range want {
		out = append(out, c)
	}
	return out, true
}

// withFields sets the projection parameter. The service always returns the
// primary key regardless, which the flattener relies on for FK resolution.
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

func flattenRows(raws []json.RawMessage) ([]string, []map[string]interface{}, error) {
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
			// A relationship id that is present but not an identifier is a
			// malformed row, and a silent one: nullFKColumns would otherwise
			// read it as "no relationship" and synthesize a legitimate-looking
			// null column, which an alert-table query counts as a real row.
			for col, v := range obj {
				base, isFK := strings.CutSuffix(col, "_id")
				if !isFK || base == "" {
					continue
				}
				if _, kind := classifyFKValue(v); kind == fkBadID {
					return nil, nil, &TransportError{
						Op:      "reading " + col,
						Err:     errMalformedFK,
						Message: "Replica cache returned a relationship id that is not a usable identifier. The service is reachable but answered with something unexpected.",
					}
				}
			}
			if _, ok := toInt(obj["id"]); !ok {
				// Measured: the service returns id on every projection, even one
				// that did not ask for it — `fields=serial` comes back as
				// {id, serial} — which is the same invariant the deep-link column
				// already relies on. A row without one identifies no object, and
				// {"count":1,"results":[{}]} would otherwise be one empty row with
				// a matching total, which an alert-table query turns into a value
				// of 1.
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
		row := make(map[string]interface{}, len(obj)+4)
		for k, v := range obj {
			if k == "custom_field_data" {
				// Through the SHARED contract, not a copy of it. A custom field
				// holding a list or an object was previously written straight
				// into cf_<name>, so a panel selecting the cf_<name>_count that
				// NetBox mode produces got no such column, and the list itself
				// rendered in a different format.
				//
				// What cannot be matched is the OBJECT custom field. NetBox's API
				// expands it to a nested object; the column this mirrors holds
				// the bare id, and the target model is named by a content-type id
				// the service does not expose — the same wall as the polymorphic
				// FKs. So cf_<name> is that id, and no _id/_slug follow it.
				cf := customFields(v)
				for _, name := range sortedNames(cf) {
					provider.FlattenField("cf_"+name, cf[name], func(n string, val interface{}) {
						row[n] = val
					})
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

// customFields decodes the custom_field_data blob. It is a JSON object encoded
// as a string, so it needs a second decode; anything else is ignored rather
// than guessed at.
// sortedNames keeps the derived custom-field columns in a deterministic order,
// since Go randomizes map iteration and a column list that reordered between
// refreshes would reorder the panel's table on every refresh.
func sortedNames(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func customFields(v interface{}) map[string]interface{} {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	return m
}
