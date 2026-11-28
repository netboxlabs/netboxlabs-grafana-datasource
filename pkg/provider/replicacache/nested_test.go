package replicacache

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A nested column, <ref>__<col>, is NetBox mode's (#32): column <col> of the
// object <ref> names, as an Objects query on that object's table shows it. The
// replica's row holds only the reference's id, so the referenced rows are read,
// once for every reference named, by id.

// nestedWorld is devices with a device type (and its manufacturer) and a
// primary IPv4 reference, which no device sets.
func nestedWorld() *fakeService {
	f := newFakeService()
	f.addIPEnrichmentTables()
	f.addColumns("dcim/devices", "device_type_id:BIGINT", "primary_ip4_id:BIGINT")
	f.addReference("dcim/devices", "device_type_id", "dcim/device-types", "device_type", "model", "slug")
	f.addReference("dcim/devices", "primary_ip4_id", "ipam/ip-addresses", "primary_ip4", "address")
	f.addReference("dcim/device-types", "manufacturer_id", "dcim/manufacturers", "manufacturer", "name", "slug")
	f.addRows("dcim/manufacturers",
		map[string]interface{}{"id": float64(1), "name": "Arista", "slug": "arista"},
		map[string]interface{}{"id": float64(2), "name": "Juniper", "slug": "juniper"})
	f.addRows("dcim/device-types",
		map[string]interface{}{"id": float64(1), "model": "DCS-7050", "slug": "dcs-7050", "manufacturer_id": float64(1)},
		map[string]interface{}{"id": float64(2), "model": "QFX5100", "slug": "qfx5100", "manufacturer_id": float64(2)})
	device := func(id int, name string, deviceType interface{}) map[string]interface{} {
		return map[string]interface{}{"id": float64(id), "name": name, "status": "active", "device_type_id": deviceType,
			"primary_ip4_id": nil, "site_id": nil, "role_id": nil, "tenant_id": nil, "rack_id": nil, "platform_id": nil, "custom_field_data": "{}"}
	}
	f.entities["dcim/devices"] = []map[string]interface{}{
		device(1, "leaf-1", float64(1)), device(2, "leaf-2", float64(2)), device(3, "loose", nil), device(4, "leaf-4", float64(1)),
	}
	return f
}

func nestedQuery(t *testing.T, f *fakeService, spec provider.QuerySpec) *provider.Result {
	t.Helper()
	spec.ObjectType = "dcim/devices"
	res, err := newTestProvider(t, f).Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return res
}

// nestedValues is column name across the rows, in order.
func nestedValues(res *provider.Result, name string) []interface{} {
	out := make([]interface{}, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = r[name]
	}
	return out
}

// A reference's own fields, as its table shows them: a related object by its
// display, an id as stored. One read of the device types, for the ids the rows
// name, whatever number of columns of it are asked for.
func TestQueryNestedColumnReadsTheReferencedRow(t *testing.T) {
	f := nestedWorld()
	res := nestedQuery(t, f, provider.QuerySpec{Fields: []string{"name", "device_type__manufacturer", "device_type__model", "device_type__manufacturer_id"}})
	if !slices.Equal(res.Columns, []string{"name", "device_type__manufacturer", "device_type__model", "device_type__manufacturer_id"}) {
		t.Errorf("columns %v", res.Columns)
	}
	for name, want := range map[string][]interface{}{
		"device_type__manufacturer":    {"Arista", "Juniper", nil, "Arista"},
		"device_type__model":           {"DCS-7050", "QFX5100", nil, "DCS-7050"},
		"device_type__manufacturer_id": {float64(1), float64(2), nil, float64(1)},
	} {
		if got := nestedValues(res, name); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %q", res.Warnings)
	}
	reads := f.sentWith("dcim/device-types", "filter[id]__in")
	if len(reads) != 1 || reads[0].query.Get("filter[id]__in") != "1,2" {
		t.Errorf("device types read %v, want once for ids 1 and 2", reads)
	}
}

// A join key's nested source is in the rows, not the columns, and "All
// columns" keeps its shape.
func TestQueryNestedKeyFieldIsNotAColumn(t *testing.T) {
	res := nestedQuery(t, nestedWorld(), provider.QuerySpec{Fields: []string{"name"}, KeyFields: []string{"device_type__manufacturer"}})
	if slices.Contains(res.Columns, "device_type__manufacturer") || res.Rows[0]["device_type__manufacturer"] != "Arista" {
		t.Errorf("columns %v, row %v", res.Columns, res.Rows[0])
	}
	all := nestedQuery(t, nestedWorld(), provider.QuerySpec{})
	withKey := nestedQuery(t, nestedWorld(), provider.QuerySpec{KeyFields: []string{"device_type__manufacturer"}})
	if !slices.Equal(all.Columns, withKey.Columns) || withKey.Rows[1]["device_type__manufacturer"] != "Juniper" {
		t.Errorf("All columns changed shape: %v vs %v", all.Columns, withKey.Columns)
	}
}

// <ref>__id is the reference's id, and <ref>__display the display the row
// already shows for it: neither reads the referenced table.
func TestQueryNestedIDAndDisplayNeedNoRead(t *testing.T) {
	f := nestedWorld()
	res := nestedQuery(t, f, provider.QuerySpec{Fields: []string{"name", "device_type__id", "device_type__display"}})
	if got := nestedValues(res, "device_type__id"); !slices.Equal(got, []interface{}{float64(1), float64(2), nil, float64(1)}) {
		t.Errorf("device_type__id = %v", got)
	}
	if got := nestedValues(res, "device_type__display"); !slices.Equal(got, []interface{}{"DCS-7050", "QFX5100", nil, "DCS-7050"}) {
		t.Errorf("device_type__display = %v", got)
	}
	if n := len(f.sentWith("dcim/device-types", "filter[id]__in")); n != 0 {
		t.Errorf("%d device type reads", n)
	}
	if slices.Contains(res.Columns, "device_type") {
		t.Errorf("the display's reference became a column: %v", res.Columns)
	}
}

// An empty column says why, naming it as written (quoted, as the alert table
// looks a value field up), and is still a column of the result: on every row,
// null, and never also reported as a column the backend doesn't produce.
func TestQueryNestedColumnSaysWhyItIsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		warning    bool
	}{
		{"devic_type__model", `"devic_type__model" is empty: dcim/devices has no reference devic_type. Check the name.`, true},
		{"status__label", `"status__label" is empty: replica-cache mode can't read status's own fields. Use a datasource in NetBox mode for it.`, true},
		{"device_type__nope", `"device_type__nope" is empty: dcim/device-types has no column nope.`, true},
		{"device_type__manufacturer__name", `"device_type__manufacturer__name" is empty: dcim/device-types has no column manufacturer__name; a nested column reads one level.`, true},
		{"device_type__device_count", `"device_type__device_count" is computed by NetBox and is not available in replica-cache mode, so it is empty here. Use a datasource in NetBox mode for it.`, true},
		{"primary_ip4__address", `"primary_ip4__address" is empty: no row has a primary_ip4 set.`, false},
		{"platform__name", "platform cannot be resolved: dcim/platforms has received no data in this replica; platform_id still holds the value.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := nestedWorld()
			res := nestedQuery(t, f, provider.QuerySpec{Fields: []string{"name", tc.name}})
			lines := res.Notes
			if tc.warning {
				lines = res.Warnings
			}
			if !slices.Contains(lines, tc.want) {
				t.Errorf("warnings %q, notes %q; want %q", res.Warnings, res.Notes, tc.want)
			}
			if len(res.Warnings) > 1 || (!tc.warning && len(res.Warnings) > 0) {
				t.Errorf("more than the one reason: %q", res.Warnings)
			}
			if !slices.Contains(res.Columns, tc.name) {
				t.Errorf("columns %v lack %s", res.Columns, tc.name)
			}
			for _, r := range res.Rows {
				if v, ok := r[tc.name]; !ok || v != nil {
					t.Errorf("row %v: %s = %v (present %v), want null", r["name"], tc.name, v, ok)
				}
			}
		})
	}
	// None of them read more than the reference itself: a name nested twice
	// doesn't read the manufacturers, nor an unset reference the addresses.
	f := nestedWorld()
	nestedQuery(t, f, provider.QuerySpec{Fields: []string{"device_type__manufacturer__name", "primary_ip4__address"}})
	for _, table := range []string{"dcim/manufacturers", "ipam/ip-addresses", "dcim/device-types"} {
		if n := len(f.sentWith(table, "filter[id]__in")); n != 0 {
			t.Errorf("%d reads of %s", n, table)
		}
	}
}

// A referenced row the replica doesn't hold yet blanks the column, counted; the
// reference's own warning, when it is a column too, says it once.
func TestQueryNestedColumnCountsMissingRows(t *testing.T) {
	f := nestedWorld()
	f.entities["dcim/devices"][3]["device_type_id"] = float64(9)
	res := nestedQuery(t, f, provider.QuerySpec{Fields: []string{"name", "device_type__model"}})
	want := `"device_type__model" is blank on 1 of 4 rows: they reference dcim/device-types objects this replica does not hold yet.`
	if !slices.Equal(res.Warnings, []string{want}) {
		t.Errorf("warnings %q, want %q", res.Warnings, want)
	}
	res = nestedQuery(t, f, provider.QuerySpec{Fields: []string{"name", "device_type", "device_type__model"}})
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "Related names from dcim/device-types are missing") {
		t.Errorf("warnings %q, want the reference's own, once", res.Warnings)
	}
}

// The referenced rows' age is the result's too: of unknown age, the result is.
func TestQueryNestedColumnCarriesTheTargetsAge(t *testing.T) {
	f := nestedWorld()
	e := f.schema.Entities["/v1/dcim/device-types"]
	e.DataAsOf = nil
	f.schema.Entities["/v1/dcim/device-types"] = e
	res := nestedQuery(t, f, provider.QuerySpec{Fields: []string{"name", "device_type__model"}})
	if res.DataAsOf != nil {
		t.Errorf("DataAsOf %v, want unknown", res.DataAsOf)
	}
	res = nestedQuery(t, nestedWorld(), provider.QuerySpec{Fields: []string{"name", "device_type__model"}})
	if res.DataAsOf == nil {
		t.Error("DataAsOf unknown with every read's age known")
	}
}

// A nested column can't be sorted on, and the note says so.
func TestQueryNestedColumnIsNotSorted(t *testing.T) {
	res := nestedQuery(t, nestedWorld(), provider.QuerySpec{Fields: []string{"name", "device_type__model"}, Ordering: "device_type__model"})
	want := `Rows are not sorted by "device_type__model": a nested column can't be sorted on.`
	if !slices.Contains(res.Notes, want) {
		t.Errorf("notes %q, want %q", res.Notes, want)
	}
}

// A column the catalogue lists under a name spelt like a nested one (a
// plugin's) is read as itself.
func TestQueryRealDoubleUnderscoreColumnIsItself(t *testing.T) {
	const widgets = "plugins/acme/widgets"
	f := newFakeService()
	f.addEntity(widgets, "id:BIGINT:pk", "plugin__flag:VARCHAR")
	f.entities[widgets] = []map[string]interface{}{{"id": float64(1), "plugin__flag": "on"}}
	res, err := newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: widgets, Fields: []string{"plugin__flag"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0]["plugin__flag"] != "on" || len(res.Warnings) != 0 {
		t.Errorf("row %v, warnings %q", res.Rows[0], res.Warnings)
	}
}
