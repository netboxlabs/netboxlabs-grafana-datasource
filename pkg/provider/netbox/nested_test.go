package netbox

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// typedDeviceFixtures are devices whose device_type carries its manufacturer,
// as NetBox's brief device type does (measured on 4.4.10). The third has no
// device type; the fourth carries a real top-level key with a double
// underscore, which no NetBox model has but a plugin's could.
func typedDeviceFixtures() []map[string]interface{} {
	devType := func(id float64, model, mfr, mfrSlug string) map[string]interface{} {
		return map[string]interface{}{
			"id": id, "url": "http://nb/api/dcim/device-types/1/", "display": model,
			"manufacturer": map[string]interface{}{"id": 7.0, "url": "http://nb/api/dcim/manufacturers/7/", "display": mfr, "name": mfr, "slug": mfrSlug},
			"model":        model, "slug": strings.ToLower(model),
		}
	}
	return []map[string]interface{}{
		{"id": 1.0, "name": "leaf1", "tenant": nil, "device_type": devType(1, "DCS-7050SX3", "Arista", "arista"),
			"site": map[string]interface{}{"id": 2.0, "name": "dc1", "slug": "dc1"}},
		{"id": 2.0, "name": "leaf2", "tenant": nil, "device_type": devType(2, "QFX5120", "Juniper", "juniper"),
			"site": map[string]interface{}{"id": 3.0, "name": "dc2", "slug": "dc2"}},
		{"id": 3.0, "name": "spare", "tenant": nil, "device_type": nil,
			"site": map[string]interface{}{"id": 3.0, "name": "dc2", "slug": "dc2"}},
		{"id": 4.0, "name": "odd", "tenant": nil, "device_type": "not an object", "plugin__flag": "kept",
			"site": map[string]interface{}{"id": 3.0, "name": "dc2", "slug": "dc2"}},
	}
}

func typedDevices() map[string]func(url.Values) []map[string]interface{} {
	return map[string]func(url.Values) []map[string]interface{}{"dcim/devices": fixed(typedDeviceFixtures()...)}
}

// columnValues is one column's value on every row, in row order.
func columnValues(res *provider.Result, col string) []interface{} {
	out := make([]interface{}, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = r[col]
	}
	return out
}

// A <ref>__<col> column is <col> of the object under <ref>, flattened as a row
// is: a reference in it gives its display and _slug, a scalar its value. A ref
// that is null or not an object gives null. The projection asks for <ref>.
func TestQueryNestedColumnFromAReference(t *testing.T) {
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100,
		Fields: []string{"name", "device_type__manufacturer", "device_type__manufacturer_slug", "device_type__model"}}
	_, projected, spy := runBoth(t, spec, typedDevices())

	if want := spec.Fields; !reflect.DeepEqual(projected.Columns, want) {
		t.Fatalf("columns = %v, want %v", projected.Columns, want)
	}
	for col, want := range map[string][]interface{}{
		"device_type__manufacturer":      {"Arista", "Juniper", nil, nil},
		"device_type__manufacturer_slug": {"arista", "juniper", nil, nil},
		"device_type__model":             {"DCS-7050SX3", "QFX5120", nil, nil},
	} {
		if got := columnValues(projected, col); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", col, got, want)
		}
	}
	sent := spy.fieldSet(t, "dcim/devices")
	if !sent["device_type"] || !sent["name"] {
		t.Errorf("projection %v, want name and device_type", sortedKeysOf(sent))
	}
	for name := range sent {
		if strings.Contains(name, "__") {
			t.Errorf("projection sends %q: a nested column is computed, never a NetBox property", name)
		}
	}
}

// The column is the same whether NetBox honoured the projection or sent whole
// objects.
func TestQueryNestedColumnSameWithoutProjection(t *testing.T) {
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100, Fields: []string{"name", "device_type__manufacturer"}}
	control, projected, _ := runBoth(t, spec, typedDevices())
	assertSameAnswer(t, control, projected)
}

// A nested column named only as a join key is computed into the rows but isn't
// a column, as any key source isn't: an "All columns" table keeps its shape.
func TestQueryNestedJoinKeyIsNotAColumn(t *testing.T) {
	for _, fields := range [][]string{{"name"}, nil} {
		spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100, Fields: fields, KeyFields: []string{"device_type__manufacturer"}}
		_, projected, _ := runBoth(t, spec, typedDevices())
		if got := columnValues(projected, "device_type__manufacturer"); !reflect.DeepEqual(got, []interface{}{"Arista", "Juniper", nil, nil}) {
			t.Errorf("fields %v: key source = %#v", fields, got)
		}
		for _, c := range projected.Columns {
			if c == "device_type__manufacturer" {
				t.Errorf("fields %v: columns %v hold the key source", fields, projected.Columns)
			}
		}
	}
}

// A real key holding a double underscore keeps its own value: it isn't
// recomputed from a reference.
func TestQueryNestedColumnKeepsARealKey(t *testing.T) {
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100, Fields: []string{"name", "plugin__flag"}}
	control, _, _ := runBoth(t, spec, typedDevices())
	if got := columnValues(control, "plugin__flag"); !reflect.DeepEqual(got, []interface{}{nil, nil, nil, "kept"}) {
		t.Errorf("plugin__flag = %#v", got)
	}
}

// A custom field isn't a top-level property, so cf_<x>__<y> is null, and it
// sends nothing upstream for the reference.
func TestQueryNestedColumnOnACustomFieldIsNull(t *testing.T) {
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100, Fields: []string{"name", "cf_owner__name"}}
	_, projected, spy := runBoth(t, spec, typedDevices())
	if got := columnValues(projected, "cf_owner__name"); !reflect.DeepEqual(got, []interface{}{nil, nil, nil, nil}) {
		t.Errorf("cf_owner__name = %#v", got)
	}
	if sent := spy.fieldSet(t, "dcim/devices"); sent["cf_owner"] {
		t.Errorf("projection %v sends cf_owner", sortedKeysOf(sent))
	}
}

// The column list offers <ref>__<nested> for each reference nested in a
// reference of the sample.
func TestFieldsOfferNestedReferences(t *testing.T) {
	p := (&fakeNetBox{objects: typedDevices()}).start(t)
	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	found := false
	for _, n := range names {
		if n == "device_type__manufacturer" {
			found = true
		}
		if n == "device_type__url" || n == "site__name" {
			t.Errorf("fields offer %q: only references nested in a reference are offered", n)
		}
	}
	if !found {
		t.Errorf("fields %v don't offer device_type__manufacturer", names)
	}
}

func TestNestedColumn(t *testing.T) {
	for _, tc := range []struct {
		name, ref, col string
		ok             bool
	}{
		{"device_type__manufacturer", "device_type", "manufacturer", true},
		{"device_type__manufacturer_slug", "device_type", "manufacturer_slug", true},
		{"a__b__c", "a", "b__c", true},
		{"device_type", "", "", false},
		{"__x", "", "", false},
		{"x__", "", "", false},
		{"site_id", "", "", false},
		{"cf__owner", "", "", false}, // the custom field _owner
		{"cf_owner__name", "", "", false},
	} {
		ref, col, ok := nestedColumn(tc.name)
		if ref != tc.ref || col != tc.col || ok != tc.ok {
			t.Errorf("nestedColumn(%q) = %q, %q, %v; want %q, %q, %v", tc.name, ref, col, ok, tc.ref, tc.col, tc.ok)
		}
	}
}

// A nested column that can't exist (no row has its reference: usually a typo;
// or NetBox's short form of the reference has no such field) is a warning, so
// an alert rule or SQL join reading it refuses rather than read nulls as
// values. One whose reference is present but unset on every row is a note.
func TestQueryNestedColumnThatDoesntExistSaysSo(t *testing.T) {
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100,
		Fields: []string{"name", "device_typ__model", "site__region", "tenant__group", "device_type__model"}}
	_, projected, _ := runBoth(t, spec, typedDevices())
	warnings, notes := strings.Join(projected.Warnings, " | "), strings.Join(projected.Notes, " | ")
	if !strings.Contains(warnings, "device_typ__model") || !strings.Contains(warnings, "Check the name") {
		t.Errorf("warnings %q, want device_typ__model with Check the name", warnings)
	}
	if !strings.Contains(warnings, "site__region") {
		t.Errorf("warnings %q, want site__region", warnings)
	}
	if !strings.Contains(notes, "tenant__group") || strings.Contains(notes, "Check the name") || strings.Contains(warnings, "tenant__group") {
		t.Errorf("notes %q, warnings %q: want tenant__group as a note without Check the name", notes, warnings)
	}
	if strings.Contains(warnings+notes, "device_type__model") {
		t.Errorf("device_type__model has values but is named in %q / %q", warnings, notes)
	}
}

// A property whose own name holds a double underscore (a plugin serializer can
// declare one) is a plain column when NetBox's schema lists it: it is projected
// by its own name and keeps its value, rather than being read as a nested
// column of a reference that doesn't exist.
func TestQueryRealDoubleUnderscorePropertyIsNotNested(t *testing.T) {
	d := newDimServer(t)
	d.schema = strings.Replace(dimSchemaFixture, `"name":   {"type": "string"},
    "site":`, `"name":   {"type": "string"},
    "plugin__flag": {"type": "string"},
    "site":`, 1)
	d.deviceRows = []string{`{"id":1,"name":"a","plugin__flag":"kept","site":{"id":1,"display":"AMS1","name":"AMS1","slug":"ams1"}}`}
	d.deviceLen = 1
	p := newDimProvider(d)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "plugin__flag"}, Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["plugin__flag"]; got != "kept" {
		t.Errorf("plugin__flag = %#v, want kept", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %v, want none", res.Warnings)
	}
}
