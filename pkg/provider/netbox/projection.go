package netbox

import (
	"net/url"
	"strings"
)

// fieldsParam is NetBox's serializer-projection parameter: `?fields=a,b` makes
// the list endpoint emit only those top-level properties per object. It cuts the
// payload NetBox builds, sends and we decode, without changing which objects
// match or how many there are — the envelope `count` is identical either way
// (verified against a large instance: a 49-key object narrows to the 3 keys the
// query asked for, and the envelope count is unchanged either way).
//
// It is also SILENT about names it does not recognize: an unknown name is
// dropped from the projection with a 200 and no warning, so a projection that
// asks for the wrong thing does not fail — it returns objects missing the very
// property the caller needed. Everything below exists to make sure the names we
// send are the ones the flattener actually reads.
const fieldsParam = "fields"

// customFieldsKey is the object property flattenObject hoists into cf_* columns,
// and cfPrefix is the prefix it hoists them under.
const (
	customFieldsKey = "custom_fields"
	cfPrefix        = "cf_"
)

// excludeParam / configContextField are NetBox's documented opt-out from config
// contexts: `?exclude=config_context`.
//
// It is worth having because most of what a device COUNT costs is not COUNT(*)
// at all — DeviceViewSet drags annotate_config_context_data() into the counted
// queryset, and dropping it cuts a count-only probe on a large instance to a
// fraction of its cost. The projection alone does not help — the annotation is
// on the queryset, not the serializer, so trimming fields leaves it in place.
//
// `exclude` is NOT a general field-exclusion parameter, which is what makes it
// safe to send at every endpoint: verified against NetBox 4.4.10 that
// `?exclude=name` leaves `name` in the response untouched, and that
// `?exclude=config_context` on a model that has no such field (ipam/prefixes)
// answers 200 with all 24 keys intact. Only config_context responds to it.
//
// It is nonetheless sent ONLY alongside a ?fields= projection that does not name
// config_context — see setExcludeConfigContext. `config_context` is a real
// column this provider surfaces (a top-level object property, flattened like any
// other), so sending this unconditionally would silently delete a column from
// every unprojected device query. That is a visible change on an instance of any
// size, which no amount of speed pays for.
const (
	excludeParam       = "exclude"
	configContextField = "config_context"
)

// setExcludeConfigContext adds ?exclude=config_context when the projection has
// already excluded it from the response, making the parameter free: the bytes
// are identical either way and only the upstream queryset gets cheaper.
//
// It leaves an existing `exclude` parameter alone. NetBox has no model filter
// named "exclude" today, but the same reasoning that guards the projection
// applies — a user's own parameter is their answer, ours is an optimisation.
func setExcludeConfigContext(q url.Values, projection string) {
	if q.Has(excludeParam) || projectionNames(projection)[configContextField] {
		return
	}
	q.Set(excludeParam, configContextField)
}

// projectionNames splits a ?fields= value back into the set of property names it
// asks for.
func projectionNames(projection string) map[string]bool {
	names := map[string]bool{}
	for _, n := range strings.Split(projection, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names[n] = true
		}
	}
	return names
}

// countOnlyFields is the projection for a QuerySpec.CountOnly query: the caller
// reads only the envelope's count, so the cheapest row NetBox will still build
// is one carrying the object's identity. "id" is not chosen for its value but
// because every NetBox model has it, and asking for a name NetBox does not know
// yields `{}` per row rather than an error (see fieldsParam).
var countOnlyFields = []string{"id"}

// derivedSuffixes are the suffixes flattenObject appends when it DERIVES a
// column from a property rather than passing the property through: a nested
// object contributes <key>_id and <key>_slug, a choice object <key>_value, and a
// list <key>_count (see flatten.go). A derived column therefore names a
// serializer property that does not exist, and asking NetBox for it yields
// nothing at all — so a column ending in one of these suffixes derives from the
// property named by the remaining prefix, which is the name that must be sent
// and the name whose dimension applies.
//
// ONE declaration on purpose. This list had a second copy in dimension.go
// (columnSuffixes) with the same four entries, and the two are not independent:
// appendUpstreamNames uses it to decide which property to FETCH for a column and
// dimIndex.resolve uses it to decide what that column MEANS. Let them disagree
// and a column is either fetched under a name nothing resolves or resolved to a
// property nothing fetched — both silent. Splitting it again reintroduces that.
//
// TestDerivedSuffixesMatchFlattenField pins the entries to what the FLATTENER
// appends, on both paths that can name a column: through flattenObject, which
// every result column comes out of and whose own key loop can derive a name
// flattenField never sees, and through flattenField directly, the way
// dimension.go's flattenValues calls it. Each probe carries its own object key,
// so a branch selected by the key rather than the value — the custom_fields
// hoist — is covered too, its columns matched against the cf_<name> they are
// hoisted under. A suffix added to flatten.go and not here is invisible to every
// other test in this package.
var derivedSuffixes = []string{"_id", "_slug", "_value", "_count"}

// computedColumns are result columns this provider CALCULATES from other
// properties (see utilization.go). NetBox has no such serializer fields, so
// projecting on them would be a no-op at best; the properties they are computed
// from are requested separately via utilizationSourceFields.
var computedColumns = map[string]bool{}

func init() {
	for _, name := range utilizationFieldNames() {
		computedColumns[name] = true
	}
}

// appendUpstreamNames adds every NetBox property a flattened column could have
// come from — the column's own name AND the property it may have been derived
// from — via add, which de-duplicates and preserves first-seen order.
//
// BOTH are always sent, and that is deliberate rather than defensive. The
// suffixes are not reserved: `interface_count` is a REAL device property while
// `tags_count` is one flattenObject derived from the `tags` list, and nothing in
// a column name distinguishes the two. Sending the pair is what makes the
// question unnecessary — a name NetBox does not know costs nothing (it is
// silently ignored) whereas a name we failed to send costs the user a column.
//
// Exactly one suffix is stripped. Flattening appends one, so `vlan_group_id`
// derives from `vlan_group`, never from `vlan`; stripping further would request
// an unrelated property and, worse, could make a genuinely missing column look
// requested.
func appendUpstreamNames(column string, add func(string)) {
	if column == "" {
		return
	}
	add(column)
	if strings.HasPrefix(column, cfPrefix) {
		// Custom fields are not top-level properties: they live inside the
		// custom_fields object, which the flattener hoists to cf_*.
		add(customFieldsKey)
	}
	for _, suffix := range derivedSuffixes {
		base, ok := strings.CutSuffix(column, suffix)
		if !ok || base == "" {
			continue
		}
		add(base)
		if strings.HasPrefix(base, cfPrefix) {
			add(customFieldsKey)
		}
		return
	}
}

// fetchableFields is the part of a caller's selection that NetBox can actually
// serialize: the selection minus this provider's computed columns.
//
// It is what the projection asks for AND what the projection's safety net
// compares the response against, and it has to be the same list on both sides.
// Asking that response for a computed column is asking for something that is
// added later, after the fetch, by enrichUtilization — so a selection of only
// computed columns yields none of its names no matter how healthy the request
// was (see Query).
func fetchableFields(fields []string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if computedColumns[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// projectionValue builds the ?fields= value for a query, or "" when there is
// nothing to project.
//
// fields is the caller's selected columns after fetchableFields; extra is
// everything else the query READS out of a row without returning it as a column
// (join-key sources, the properties utilization is computed from). Both are
// mapped through appendUpstreamNames, so a derived column name still fetches its
// source property.
//
// An EMPTY selection is "every column", which no projection can express — the
// caller does not know the column set, that is what the request is for. That
// case is decided by the caller, which holds the untouched selection: an empty
// fields here means only that nothing in the selection is a NetBox property, and
// the extras still have to be fetched.
func projectionValue(fields, extra []string) string {
	seen := make(map[string]bool, len(fields)+len(extra)+2)
	names := make([]string, 0, len(fields)+len(extra)+2)
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}
	for _, f := range fields {
		appendUpstreamNames(f, add)
	}
	for _, e := range extra {
		appendUpstreamNames(e, add)
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ",")
}
