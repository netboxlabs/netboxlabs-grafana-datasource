package netbox

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
// reference and the column of it. ok is false for any other name, and for a
// custom field's column (cf_…): custom fields aren't top-level properties, and
// one whose name starts with an underscore flattens to cf__<name>.
func nestedColumn(name string) (ref, col string, ok bool) {
	if strings.HasPrefix(name, cfPrefix) {
		return "", "", false
	}
	ref, col, ok = strings.Cut(name, nestedSep)
	if !ok || ref == "" || col == "" {
		return "", "", false
	}
	return ref, col, true
}

// isNestedColumn reports whether name is a nested column. No such name is ever
// sent in ?fields= (NetBox has no property of that name); its reference is.
func isNestedColumn(name string) bool {
	_, _, ok := nestedColumn(name)
	return ok
}

// nestedRefs is the references the nested columns among names read, for the
// projection, each once.
func nestedRefs(names []string) []string {
	var refs []string
	for _, n := range names {
		ref, _, ok := nestedColumn(n)
		if ok && !slices.Contains(refs, ref) {
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
// flattened object has no such column, gives null. A column that is null on
// every row because no row has its reference, or no reference has its column,
// gets a note naming it: a typo, or a field NetBox's short form of the
// reference doesn't carry, rather than a silent blank.
func addNestedColumns(fields, keys []string, raws []json.RawMessage, rows []map[string]interface{}, columns []string, seen map[string]bool) ([]string, []string) {
	var want []string
	for _, n := range append(append([]string(nil), fields...), keys...) {
		if isNestedColumn(n) && !slices.Contains(want, n) {
			want = append(want, n)
		}
	}
	if len(want) == 0 {
		return columns, nil
	}
	type found struct{ real, ref, col bool }
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
		if isNestedColumn(n) && !seen[n] {
			seen[n] = true
			columns = append(columns, n)
		}
	}
	var notes []string
	if len(rows) > 0 {
		for _, n := range want {
			f := seenIn[n]
			ref, col, _ := nestedColumn(n)
			switch {
			case f.real:
			case !f.ref:
				notes = append(notes, fmt.Sprintf("%s is empty: no row has a %s to read %s from. Check the name.", n, ref, col))
			case !f.col:
				notes = append(notes, fmt.Sprintf("%s is empty: NetBox's short form of %s has no %s.", n, ref, col))
			}
		}
	}
	return columns, notes
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
