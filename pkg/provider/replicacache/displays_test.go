package replicacache

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// NetBox writes a MAC address in capitals with colons (its MACAddressField's
// EUI dialect, mac_unix_expanded_uppercase: 02:AB:CD:00:00:01 on the demo); the
// replica stores PostgreSQL's macaddr text, lowercase, and another connector
// may store another spelling. Every MAC address a query shows is written as
// NetBox writes it — the column, a reference's expanded address, the value
// list — whatever the replica stored, and a filter still matches it.
func TestQuery_MACsReadAsNetBoxWritesThem(t *testing.T) {
	f := interfacesWithMACs()
	f.entities["dcim/mac-addresses"] = []map[string]interface{}{
		{"id": 1, "mac_address": "58:56:9f:d1:e3:00"}, {"id": 2, "mac_address": "02-ab-cd-00-00-01"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/mac-addresses", Fields: []string{"id", "mac_address"}, Ordering: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if got := []interface{}{res.Rows[0]["mac_address"], res.Rows[1]["mac_address"]}; !slices.Equal(got, []interface{}{"58:56:9F:D1:E3:00", "02:AB:CD:00:00:01"}) {
		t.Errorf("mac_address = %v, want NetBox's capitals with colons", got)
	}

	res, err = p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/mac-addresses", Fields: []string{"id", "mac_address"},
		Filters: []provider.Filter{{Field: "mac_address", Value: "58-56-9F-D1-E3-00"}}})
	if err != nil || len(res.Rows) != 1 || res.Rows[0]["mac_address"] != "58:56:9F:D1:E3:00" {
		t.Errorf("mac_address = 58-56-9F-D1-E3-00: %v, %v; want the one address, in capitals", res, err)
	}

	res, err = p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/interfaces", Fields: []string{"id", "primary_mac_address"}, Ordering: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0]["primary_mac_address"] != "58:56:9F:D1:E3:00" || res.Rows[1]["primary_mac_address"] != nil {
		t.Errorf("primary_mac_address = %v, %v; want 58:56:9F:D1:E3:00 and null", res.Rows[0]["primary_mac_address"], res.Rows[1]["primary_mac_address"])
	}

	values, err := p.FieldValues(ctx, "dcim/mac-addresses", "mac_address", "", 0)
	if err != nil || !slices.Equal(values, []string{"02:AB:CD:00:00:01", "58:56:9F:D1:E3:00"}) {
		t.Errorf("FieldValues(mac_address) = %v, %v; want the addresses in capitals", values, err)
	}
	values, err = p.FieldValues(ctx, "dcim/interfaces", "primary_mac_address", "", 0)
	if err != nil || !slices.Equal(values, []string{"58:56:9F:D1:E3:00"}) {
		t.Errorf("FieldValues(primary_mac_address) = %v, %v; want the address in capitals", values, err)
	}
}

// A text that is no EUI-48 address is shown as stored rather than guessed at.
func TestNetBoxMAC(t *testing.T) {
	for in, want := range map[string]string{
		"58:56:9f:d1:e3:00": "58:56:9F:D1:E3:00", "5856.9fd1.e300": "58:56:9F:D1:E3:00", "58569FD1E300": "58:56:9F:D1:E3:00",
		"not a mac": "not a mac", "": "", "58:56:9f:d1:e3:00:11:22": "58:56:9f:d1:e3:00:11:22",
	} {
		if got := netboxMAC(in); got != want {
			t.Errorf("netboxMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

// displayFake serves the targets whose NetBox display (str() of the object,
// which NetBox mode shows for a related column) is not the one column the
// replica's expansion returns, with the columns and references replica-cache
// v1.40.1 serves them with (GET /v1/_meta/schema on a replica of NetBox
// 4.4.10): a VRF expands to its name and stores its rd, a VLAN its vid, a rack
// its facility_id, a provider account its account; a cable expands to its
// label, a wireless link to its ssid, an interface to its name and label; a
// device expands to its name and stores what an unnamed one is shown by.
func displayFake() *fakeService {
	f := newFakeService()
	f.addPrefixes(
		map[string]interface{}{"id": 1, "prefix": "10.1.0.0/16", "status": "active", "vrf_id": 1, "vlan_id": 1},
		map[string]interface{}{"id": 2, "prefix": "10.2.0.0/16", "status": "active", "vrf_id": 2, "vlan_id": nil},
		map[string]interface{}{"id": 3, "prefix": "10.3.0.0/16", "status": "active", "vrf_id": nil, "vlan_id": nil},
	)
	f.addColumns("ipam/prefixes", "vlan_id:BIGINT")
	f.addEntity("ipam/vrfs", "id:BIGINT:pk", "name:VARCHAR", "rd:VARCHAR")
	f.entities["ipam/vrfs"] = []map[string]interface{}{{"id": 1, "name": "Blue", "rd": "65000:1"}, {"id": 2, "name": "Red", "rd": ""}}
	f.addEntity("ipam/vlans", "id:BIGINT:pk", "vid:INTEGER", "name:VARCHAR")
	f.entities["ipam/vlans"] = []map[string]interface{}{{"id": 1, "vid": 110, "name": "ams1-lan"}}
	f.addReference("ipam/prefixes", "vrf_id", "ipam/vrfs", "vrf", "name")
	f.addReference("ipam/prefixes", "vlan_id", "ipam/vlans", "vlan", "name")

	f.addColumns("dcim/racks", "facility_id:VARCHAR")
	f.entities["dcim/racks"] = []map[string]interface{}{
		{"id": 1, "name": "R1", "facility_id": "PR-1"}, {"id": 2, "name": "R2", "facility_id": nil}, {"id": 3, "name": "R3", "facility_id": ""},
	}
	f.addColumns("dcim/devices", "asset_tag:VARCHAR", "virtual_chassis_id:BIGINT", "vc_position:INTEGER", "device_type_id:BIGINT")
	f.addEntity("dcim/virtual-chassis", "id:BIGINT:pk", "name:VARCHAR")
	f.entities["dcim/virtual-chassis"] = []map[string]interface{}{{"id": 1, "name": "vc-1"}}
	f.addEntity("dcim/manufacturers", "id:BIGINT:pk", "name:VARCHAR", "slug:VARCHAR")
	f.entities["dcim/manufacturers"] = []map[string]interface{}{{"id": 1, "name": "Juniper", "slug": "juniper"}}
	f.addEntity("dcim/device-types", "id:BIGINT:pk", "model:VARCHAR", "slug:VARCHAR", "manufacturer_id:BIGINT")
	f.entities["dcim/device-types"] = []map[string]interface{}{{"id": 1, "model": "QFX5100", "slug": "qfx5100", "manufacturer_id": 1}}
	device := func(id int, name, asset interface{}, vc, position, rack interface{}) map[string]interface{} {
		return map[string]interface{}{"id": id, "name": name, "asset_tag": asset, "virtual_chassis_id": vc, "vc_position": position,
			"device_type_id": 1, "rack_id": rack, "status": "active", "site_id": nil, "role_id": nil, "tenant_id": nil, "custom_field_data": "{}"}
	}
	f.entities["dcim/devices"] = []map[string]interface{}{
		device(1, "core-1", nil, nil, nil, 1),
		device(2, "core-2", "A-2", nil, nil, 2),
		device(3, nil, nil, 1, 2, 3),
		device(4, nil, nil, nil, nil, nil),
		device(5, nil, "A-5", nil, nil, nil),
		device(6, nil, "A-6", 1, nil, nil),
	}

	f.addEntity("dcim/cables", "id:BIGINT:pk", "label:VARCHAR")
	f.entities["dcim/cables"] = []map[string]interface{}{{"id": 7, "label": "C7"}, {"id": 9, "label": ""}}
	f.addEntity("wireless/wireless-links", "id:BIGINT:pk", "ssid:VARCHAR")
	f.entities["wireless/wireless-links"] = []map[string]interface{}{{"id": 1, "ssid": "wlan-a"}, {"id": 2, "ssid": ""}}
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "label:VARCHAR", "device_id:BIGINT", "cable_id:BIGINT",
		"lag_id:BIGINT", "wireless_link_id:BIGINT")
	f.addReference("dcim/interfaces", "device_id", "dcim/devices", "device", "name")
	f.addReference("dcim/interfaces", "cable_id", "dcim/cables", "cable", "label")
	f.addReference("dcim/interfaces", "lag_id", "dcim/interfaces", "lag", "name", "label")
	f.addReference("dcim/interfaces", "wireless_link_id", "wireless/wireless-links", "wireless_link", "ssid")
	iface := func(id, device int, label string, cable, lag, link interface{}) map[string]interface{} {
		return map[string]interface{}{"id": id, "name": fmt.Sprintf("et-%d", id), "label": label, "device_id": device,
			"cable_id": cable, "lag_id": lag, "wireless_link_id": link}
	}
	f.entities["dcim/interfaces"] = []map[string]interface{}{
		iface(1, 1, "uplink", 7, nil, 1),
		iface(2, 2, "", 9, 1, 2),
		iface(3, 3, "", nil, 2, nil),
		iface(4, 4, "", nil, nil, nil),
		iface(5, 5, "", nil, nil, nil),
		iface(6, 6, "", nil, nil, nil),
	}

	f.addEntity("circuits/provider-accounts", "id:BIGINT:pk", "account:VARCHAR", "name:VARCHAR")
	f.entities["circuits/provider-accounts"] = []map[string]interface{}{{"id": 1, "account": "ACC-1", "name": "Main"}, {"id": 2, "account": "ACC-2", "name": ""}}
	f.addEntity("circuits/circuits", "id:BIGINT:pk", "cid:VARCHAR", "provider_account_id:BIGINT")
	f.addReference("circuits/circuits", "provider_account_id", "circuits/provider-accounts", "provider_account", "name")
	f.entities["circuits/circuits"] = []map[string]interface{}{{"id": 1, "cid": "C-1", "provider_account_id": 1}, {"id": 2, "cid": "C-2", "provider_account_id": 2}}
	return f
}

// columnOf is one column of a result, row by row.
func columnOf(t *testing.T, p *Provider, spec provider.QuerySpec, name string) []interface{} {
	t.Helper()
	if spec.Ordering == "" {
		spec.Ordering = "id"
	}
	res, err := p.Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("Query(%+v): %v", spec, err)
	}
	out := make([]interface{}, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = r[name]
	}
	return out
}

// NetBox mode shows a related object by its display, str() of the object
// (BaseModelSerializer.get_display), and the replica's expansion returns one
// column of it. Where NetBox's __str__ is more than that column (NetBox 4.4.10
// and 4.6.10 read alike for each), the column shows what NetBox mode shows,
// composed from the parts the replica holds: a VRF's rd, a VLAN's vid, a
// rack's facility id, a provider account's account beside its name, a cable's
// or a wireless link's id when it has no label or SSID, an interface's label,
// and a device's asset tag — or, unnamed, its virtual chassis and position, or
// its manufacturer and model.
func TestQuery_RelatedColumnsShowNetBoxsDisplay(t *testing.T) {
	p := newTestProvider(t, displayFake())
	for _, c := range []struct {
		objectType, column string
		want               []interface{}
	}{
		{"ipam/prefixes", "vrf", []interface{}{"Blue (65000:1)", "Red", nil}},
		{"ipam/prefixes", "vlan", []interface{}{"ams1-lan (110)", nil, nil}},
		{"dcim/devices", "rack", []interface{}{"R1 (PR-1)", "R2", "R3", nil, nil, nil}},
		{"dcim/interfaces", "device", []interface{}{"core-1", "core-2 (A-2)", "vc-1:2", "Juniper QFX5100 (4)", "Juniper QFX5100 (A-5)", "vc-1:None (A-6)"}},
		{"dcim/interfaces", "cable", []interface{}{"C7", "#9", nil, nil, nil, nil}},
		{"dcim/interfaces", "lag", []interface{}{nil, "et-1 (uplink)", "et-2", nil, nil, nil}},
		{"dcim/interfaces", "wireless_link", []interface{}{"wlan-a", "#2", nil, nil, nil, nil}},
		{"circuits/circuits", "provider_account", []interface{}{"ACC-1 (Main)", "ACC-2"}},
	} {
		got := columnOf(t, p, provider.QuerySpec{ObjectType: c.objectType, Fields: []string{"id", c.column}}, c.column)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s %s = %v,\nwant %v", c.objectType, c.column, got, c.want)
		}
		// All columns shows them the same way.
		if got := columnOf(t, p, provider.QuerySpec{ObjectType: c.objectType}, c.column); !slices.Equal(got, c.want) {
			t.Errorf("%s %s, all columns = %v,\nwant %v", c.objectType, c.column, got, c.want)
		}
	}
}

// The parts are read once per reference, for the ids on the rows, projected to
// what the display needs; a reference the rows do not show — not selected, a
// filter's or a sort's only, or a count — reads nothing more, and one composed
// from the expansion alone (a cable, an interface) reads nothing either.
func TestQuery_RelatedDisplayReadsOnlyWhatItShows(t *testing.T) {
	f := displayFake()
	p := newTestProvider(t, f)
	ctx := context.Background()
	if _, err := p.Query(ctx, provider.QuerySpec{ObjectType: "ipam/prefixes", Fields: []string{"id", "vrf"}}); err != nil {
		t.Fatal(err)
	}
	reads := f.sentWith("ipam/vrfs", "filter[id]__in")
	if len(reads) != 1 || reads[0].query.Get("filter[id]__in") != "1,2" || reads[0].query.Get("fields") != "id,name,rd" {
		t.Errorf("VRF reads = %v, want one for ids 1,2 projected to id,name,rd", reads)
	}

	for _, spec := range []provider.QuerySpec{
		{ObjectType: "ipam/prefixes", Fields: []string{"id", "prefix"}},
		{ObjectType: "ipam/prefixes", Fields: []string{"id"}, Ordering: "vrf"},
		{ObjectType: "ipam/prefixes", CountOnly: true},
	} {
		f.forget()
		if _, err := p.Query(ctx, spec); err != nil {
			t.Fatal(err)
		}
		noRequestsFor(t, f, "ipam/vrfs", "ipam/vlans")
	}
	f.forget()
	if _, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/interfaces", Fields: []string{"id", "cable", "lag", "wireless_link"}}); err != nil {
		t.Fatal(err)
	}
	noRequestsFor(t, f, "dcim/cables", "wireless/wireless-links")
	if n := len(f.sentWith("dcim/interfaces", "filter[id]__in")); n != 0 {
		t.Errorf("%d interface reads for lag, which the expansion composes", n)
	}
}

// A join reads the column a panel is shown, so a join key on a related column
// is the display too, as in NetBox mode; a sort on one is still the replica's,
// on the name it joins in (NetBox orders a VRF by name, rd, then id).
func TestQuery_RelatedDisplayInJoinsAndSorts(t *testing.T) {
	f := displayFake()
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", Fields: []string{"id"}, KeyFields: []string{"vrf"}, Ordering: "-vrf"})
	if err != nil {
		t.Fatal(err)
	}
	if req, ok := f.requestWith("ipam/prefixes", "sort"); !ok || req.query.Get("sort") != "-vrf" {
		t.Errorf("sort sent = %v, want -vrf", req.query)
	}
	got := []interface{}{}
	for _, r := range res.Rows {
		got = append(got, r["vrf"])
	}
	if !slices.Contains(got, "Blue (65000:1)") || !slices.Contains(got, "Red") {
		t.Errorf("join key vrf = %v, want the displays", got)
	}
}

// An object the replica does not hold is a gap, as it is for any related
// name: blank, with the warning that says so — not the name of another, nor
// "#<id>" for a cable that is not there.
func TestQuery_RelatedDisplayOfAMissingObjectIsAGap(t *testing.T) {
	f := displayFake()
	f.entities["ipam/prefixes"][2]["vrf_id"] = 99
	f.entities["dcim/interfaces"][3]["cable_id"] = 98
	p := newTestProvider(t, f)
	for _, c := range []struct {
		objectType, column string
		at                 int
	}{{"ipam/prefixes", "vrf", 2}, {"dcim/interfaces", "cable", 3}} {
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: c.objectType, Fields: []string{"id", c.column}, Ordering: "id"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Rows[c.at][c.column] != nil {
			t.Errorf("%s %s of a missing object = %v, want blank", c.objectType, c.column, res.Rows[c.at][c.column])
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "reference objects this replica does not hold") {
			t.Errorf("%s warnings = %v, want the missing-object warning", c.objectType, res.Warnings)
		}
	}
}

// A replica whose target lacks a part NetBox's display reads cannot compose
// it: the column keeps the name the expansion returns, and a warning says it is
// not NetBox's, rather than passing one off as the other.
func TestQuery_RelatedDisplayWithoutItsPartsWarns(t *testing.T) {
	f := displayFake()
	f.addEntity("ipam/vrfs", "id:BIGINT:pk", "name:VARCHAR")
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", Fields: []string{"id", "vrf"}, Ordering: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0]["vrf"] != "Blue" {
		t.Errorf("vrf = %v, want the name the expansion returns", res.Rows[0]["vrf"])
	}
	want := `"vrf" is not shown as NetBox shows it: ipam/vrfs on this replica has no rd, which NetBox's display of it reads, so the column holds the name alone.`
	if !slices.Contains(res.Warnings, want) {
		t.Errorf("warnings = %v, want %q", res.Warnings, want)
	}
	noRequestsFor(t, f, "ipam/vrfs")
}

// The value list offers a related column's values as the rows show them.
func TestFieldValues_RelatedDisplay(t *testing.T) {
	p := newTestProvider(t, displayFake())
	values, err := p.FieldValues(context.Background(), "ipam/prefixes", "vrf", "", 0)
	if err != nil || !slices.Equal(values, []string{"Blue (65000:1)", "Red"}) {
		t.Errorf("FieldValues(vrf) = %v, %v; want the displays", values, err)
	}
	values, err = p.FieldValues(context.Background(), "ipam/prefixes", "vrf", "65000", 0)
	if err != nil || !slices.Equal(values, []string{"Blue (65000:1)"}) {
		t.Errorf("FieldValues(vrf, 65000) = %v, %v; want the display that contains it", values, err)
	}
}
