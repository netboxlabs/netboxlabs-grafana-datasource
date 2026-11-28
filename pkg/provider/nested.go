package provider

import "strings"

// NestedColumn splits a nested column's name, <ref>__<col>, at its first "__"
// into the reference and the column of the object it names
// (device_type__manufacturer is a device's manufacturer). ok is false for any
// other name, and for a custom field's column (cf_…): custom fields aren't
// properties of the object, and one whose name starts with an underscore
// flattens to cf__<name>. Both providers and the plugin read names by it, so a
// panel means the same thing in either mode.
//
// The separator is a double underscore, not a dot: a dot is read as a table or
// database separator by Grafana's SQL expressions and breaks an alert label in
// a template. NetBox's serializer fields never hold "__", and NetBox refuses
// it in a custom field's name.
func NestedColumn(name string) (ref, col string, ok bool) {
	if strings.HasPrefix(name, "cf_") {
		return "", "", false
	}
	ref, col, ok = strings.Cut(name, "__")
	if !ok || ref == "" || col == "" {
		return "", "", false
	}
	return ref, col, true
}
