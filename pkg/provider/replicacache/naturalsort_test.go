package replicacache

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The list is NetBox's, read from its models (db_collation, Meta.ordering) in
// NetBox 4.4.10 and 4.6.10, which agree on all 47 tables; these pin what a sort
// depends on, and that every entry is a table NetBox has.
func TestNaturalSortNames(t *testing.T) {
	if len(naturalSortNames) != 47 {
		t.Errorf("%d tables, want NetBox's 47", len(naturalSortNames))
	}
	for table, leads := range map[string]bool{
		"dcim_device": true, "dcim_site": true, "ipam_vrf": true, "tenancy_tenant": true, "virtualization_cluster": true,
		"virtualization_virtualmachine": true, "dcim_virtualchassis": true, "circuits_provider": true,
		"dcim_interface": false, "dcim_rack": false, "dcim_consoleport": false, "dcim_powerpanel": false,
	} {
		if got, ok := naturalSortNames[table]; !ok || got != leads {
			t.Errorf("%s: %v, %v; want natural, ordered by it first %v", table, got, ok, leads)
		}
	}
	for _, table := range []string{"dcim_devicerole", "dcim_region", "dcim_location", "ipam_vlan", "dcim_devicetype", "dcim_platform"} {
		if _, ok := naturalSortNames[table]; ok {
			t.Errorf("%s: NetBox does not declare its name natural_sort", table)
		}
	}
	for table := range naturalSortNames {
		if _, ok := vocabFor(table); !ok {
			t.Errorf("%s is no NetBox table the vocabulary knows", table)
		}
	}
}

// naturalFake serves devices whose names the replica's byte order and NetBox's
// natural_sort collation put apart (NetBox 4.4.10's demo: parity-dev-* between
// NYC1-* and SIN1-*, leaf2 before leaf10), two of one name, one without, and an
// interface on each.
func naturalFake() *fakeService {
	f := newFakeService()
	names := []interface{}{"SIN1-leaf-10", "parity-dev-a", "SIN1-leaf-2", nil, "NYC1-leaf-01", "Leaf3", "leaf2", "leaf2"}
	f.entities["dcim/devices"] = nil
	for i, name := range names {
		f.entities["dcim/devices"] = append(f.entities["dcim/devices"], map[string]interface{}{
			"id": i + 1, "name": name, "serial": strings.ToLower(strings.TrimSpace(strings.Repeat("x", 8-i))), "status": "active",
			"site_id": nil, "role_id": nil, "tenant_id": nil, "custom_field_data": "{}"})
	}
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "device_id:BIGINT")
	f.addReference("dcim/interfaces", "device_id", "dcim/devices", "device", "name")
	for i := range names {
		f.entities["dcim/interfaces"] = append(f.entities["dcim/interfaces"], map[string]interface{}{"id": 101 + i, "name": "eth0", "device_id": i + 1})
	}
	return f
}

func idsOf(res *provider.Result, col string) []int {
	var out []int
	for _, r := range res.Rows {
		id, _ := toInt(r[col])
		out = append(out, id)
	}
	return out
}

// A sort on a name NetBox declares with the natural_sort collation lists the
// rows as NetBox mode does (ordering=name,id): digit runs by value, letters
// without regard to case first, the same name by id, no name last — and
// descending, no name first and the same name still by id, as PostgreSQL
// orders name DESC, id. A sort on a reference to a model NetBox orders by such
// a name (an interface's device) is that order too.
func TestQuery_NaturalSortFollowsNetBox(t *testing.T) {
	p := newTestProvider(t, naturalFake())
	ctx := context.Background()
	for _, c := range []struct {
		ordering string
		want     []int
	}{
		// leaf2 (7, 8), Leaf3, NYC1-leaf-01, parity-dev-a, SIN1-leaf-2, SIN1-leaf-10, none.
		{"name", []int{7, 8, 6, 5, 2, 3, 1, 4}},
		{"-name", []int{4, 1, 3, 2, 5, 6, 7, 8}},
	} {
		res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"id", "name"}, Ordering: c.ordering})
		if err != nil {
			t.Fatal(err)
		}
		if got := idsOf(res, "id"); !slices.Equal(got, c.want) {
			t.Errorf("devices by %s: %v, want %v", c.ordering, got, c.want)
		}
		if len(res.Notes) != 1 || len(res.Warnings) != 0 {
			t.Errorf("devices by %s: notes %v, warnings %v; want the freshness note alone", c.ordering, res.Notes, res.Warnings)
		}
	}
	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/interfaces", Fields: []string{"id", "device"}, Ordering: "device"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(res, "id"); !slices.Equal(got, []int{107, 108, 106, 105, 102, 103, 101, 104}) {
		t.Errorf("interfaces by device: %v, want their devices' NetBox order", got)
	}
}

// A sort on any other column is the replica's: serial is no natural_sort
// column, so its rows stay as the replica sorted them.
func TestQuery_OtherSortsAreTheReplicas(t *testing.T) {
	p := newTestProvider(t, naturalFake())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"id", "serial"}, Ordering: "serial"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(res, "id"); !slices.Equal(got, []int{8, 7, 6, 5, 4, 3, 2, 1}) {
		t.Errorf("devices by serial: %v, want the replica's order", got)
	}
}

// A limit below the number of matching rows keeps the replica's first by
// their bytes, which can be other rows than NetBox mode keeps. Those kept are
// put in NetBox's order, and a note — not a warning: the rows are real — says
// which rows the limit kept and how to read them all; nothing is refused.
func TestQuery_NaturalSortCutShortSaysSo(t *testing.T) {
	p := newTestProvider(t, naturalFake())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"id", "name"}, Ordering: "name", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	// The replica's first three by bytes: Leaf3, NYC1-leaf-01, SIN1-leaf-10.
	if got := idsOf(res, "id"); !slices.Equal(got, []int{6, 5, 1}) {
		t.Errorf("rows %v, want the replica's first three in NetBox's order", got)
	}
	want := `Rows are sorted by "name" as NetBox sorts it, but the limit kept 3 of the 8 rows that match, and the replica, which compares names by their bytes, chose which: NetBox mode can keep others. Raise the limit to 8 to read them all.`
	if !slices.Contains(res.Notes, want) || len(res.Warnings) != 0 {
		t.Errorf("notes %q, warnings %v;\nwant the note %q and no warning", res.Notes, res.Warnings, want)
	}
}
