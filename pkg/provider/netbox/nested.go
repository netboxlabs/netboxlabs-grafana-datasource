package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A nested column, <ref>__<col>, is column <col> of the object under the
// top-level property <ref>, flattened by the same rules as a row
// (flattenObject): device_type__manufacturer is a device's manufacturer, which
// NetBox already sends inside its device type, so no further request is made.
// It is computed only when a query names it (Fields, or a join key's source):
// "All columns" doesn't add it.
//
// The separator is a double underscore, not a dot: a dot is read as a table or
// database separator by Grafana's SQL expressions and breaks an alert label in
// a template. NetBox's serializer fields never hold "__", and NetBox refuses it
// in a custom field's name, so the split is never ambiguous.
const nestedSep = "__"

// nestedColumn splits a nested column's name at its first "__" into the
// reference and the column of it (provider.NestedColumn, which both modes
// share).
func nestedColumn(name string) (ref, col string, ok bool) { return provider.NestedColumn(name) }

// isNestedColumn reports whether name is a nested column. No such name is ever
// sent in ?fields= (NetBox has no property of that name); its reference is.
func isNestedColumn(name string) bool {
	_, _, ok := nestedColumn(name)
	return ok
}

// nestedIn is isNestedColumn for a query whose plain names are known: a name
// in plain is one of the object type's own properties (schemaProperties), not
// a nested column, however it is spelt.
func nestedIn(name string, plain map[string]bool) bool {
	return !plain[name] && isNestedColumn(name)
}

// nestedSchemaWait bounds how long a query waits for NetBox's schema to tell a
// property spelt <a>__<b> from a nested column. Past it the name is read as a
// nested column; the schema fetch carries on, and a later refresh has it.
const nestedSchemaWait = 2 * time.Second

// schemaProperties is which of names, spelt like nested columns, NetBox's
// schema lists as objectType's own properties. Django refuses "__" in a model
// field's name, so NetBox's own properties never hold one, but a plugin's
// serializer can declare one, and that property must be projected and read by
// its own name. Only asked when a name looks nested; nil when none does or the
// schema can't be read within nestedSchemaWait.
func (p *Provider) schemaProperties(ctx context.Context, objectType string, names []string) map[string]bool {
	var candidates []string
	for _, n := range names {
		if isNestedColumn(n) {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, nestedSchemaWait)
	defer cancel()
	entry, err := p.schema(sctx)
	if err != nil || entry.dims == nil {
		return nil
	}
	props := entry.dims[objectType]
	var plain map[string]bool
	for _, n := range candidates {
		if _, ok := props[n]; ok {
			if plain == nil {
				plain = map[string]bool{}
			}
			plain[n] = true
		}
	}
	return plain
}

// nestedRefs is the references the nested columns among names read, for the
// projection, each once. plain names aren't nested (nestedIn).
func nestedRefs(names []string, plain map[string]bool) []string {
	var refs []string
	for _, n := range names {
		ref, _, ok := nestedColumn(n)
		if ok && !plain[n] && !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// addNestedColumns computes the nested columns a query names, in fields or as
// key sources, into each row from its raw object (raws is index-aligned with
// rows), and returns columns with those named in fields appended, in fields'
// order. A key source is computed but isn't a column, as any key source isn't.
// A row that already holds a key of that name (a plugin model's own property)
// keeps it. A reference that is absent, null or not an object, or whose
// flattened object has no such column, gives null.
//
// A column that can't exist is a warning naming it, so an alert rule or SQL
// join reading it refuses rather than read nulls as values: no row has its
// reference at all (usually a typo), or NetBox's short form of the reference
// has no such column. One whose reference is present but unset on every row is
// a note: that can be the data.
func addNestedColumns(fields, keys []string, plain map[string]bool, raws []json.RawMessage, rows []map[string]interface{}, columns []string, seen map[string]bool) (cols, notes, warnings []string) {
	var want []string
	for _, n := range append(append([]string(nil), fields...), keys...) {
		if nestedIn(n, plain) && !slices.Contains(want, n) {
			want = append(want, n)
		}
	}
	if len(want) == 0 {
		return columns, nil, nil
	}
	type found struct{ real, refKey, ref, col bool }
	seenIn := make(map[string]*found, len(want))
	for _, n := range want {
		seenIn[n] = &found{}
	}
	for i, raw := range raws {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			obj = nil
		}
		flat := map[string]map[string]interface{}{} // each reference flattened once a row
		for _, name := range want {
			if _, real := rows[i][name]; real {
				seenIn[name].real = true
				continue
			}
			ref, col, _ := nestedColumn(name)
			if _, ok := obj[ref]; ok {
				seenIn[name].refKey = true
			}
			vals, done := flat[ref]
			if !done {
				vals = flattenReference(obj[ref])
				flat[ref] = vals
			}
			if vals != nil {
				seenIn[name].ref = true
				if _, ok := vals[col]; ok {
					seenIn[name].col = true
				}
			}
			rows[i][name] = vals[col]
		}
	}
	for _, n := range fields {
		if nestedIn(n, plain) && !seen[n] {
			seen[n] = true
			columns = append(columns, n)
		}
	}
	if len(rows) > 0 {
		for _, n := range want {
			f := seenIn[n]
			ref, col, _ := nestedColumn(n)
			switch {
			case f.real:
			case !f.refKey:
				warnings = append(warnings, fmt.Sprintf("%s is empty: no row has a %s. Check the name.", n, ref))
			case !f.ref:
				notes = append(notes, fmt.Sprintf("%s is empty: no row has a %s set.", n, ref))
			case !f.col:
				warnings = append(warnings, fmt.Sprintf("%s is empty: NetBox's short form of %s has no %s.", n, ref, col))
			}
		}
	}
	return columns, notes, warnings
}

// IsNestedColumn reports whether a column name is a nested column,
// <ref>__<col>, for the plugin layer.
func IsNestedColumn(name string) bool { return isNestedColumn(name) }

// flattenReference flattens a reference's object as a row is flattened; nil
// when it isn't an object.
func flattenReference(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	_, vals, err := flattenObject(raw)
	if err != nil {
		return nil
	}
	return vals
}

// nestedReferenceOffers is the nested columns the column list offers for an
// object type, from a sample object: <ref>__<nested> for each reference (an
// object with an id) inside a top-level reference, as a device's
// device_type__manufacturer. Other nested columns can be typed.
func nestedReferenceOffers(raw json.RawMessage) []string {
	keys, err := orderedKeys(raw)
	if err != nil {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []string
	for _, k := range keys {
		if k == customFieldsKey || !isReference(obj[k]) {
			continue
		}
		inner, err := orderedKeys(obj[k])
		if err != nil {
			continue
		}
		var ref map[string]json.RawMessage
		if json.Unmarshal(obj[k], &ref) != nil {
			continue
		}
		for _, nk := range inner {
			if isReference(ref[nk]) {
				out = append(out, k+nestedSep+nk)
			}
		}
	}
	return out
}

// isReference reports whether raw is an object with an id, as NetBox's nested
// references are (a choice object has none).
func isReference(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	return json.Unmarshal(raw, &probe) == nil && len(probe.ID) > 0 && string(probe.ID) != "null"
}
