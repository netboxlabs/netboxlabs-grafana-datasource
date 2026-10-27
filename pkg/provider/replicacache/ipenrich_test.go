package replicacache

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// IP enrichment is NetBox mode's (netbox/ipenrich.go) answered from the
// replica's tables. The expectations below are what NetBox mode returns for the
// same data, as NetBox 4.4.10 renders it: a VRF as "name (rd)", a rack as
// "name (facility_id)", a VLAN as "name (vid)", a device interface as
// "name (label)", a choice by its label, an address with its mask.

// ipWorld mirrors the demo NetBox's parity objects (NetBox 4.4.10, as loaded
// into the local replica), with the demo's content-type ids: a device with
// primary IPv4 and IPv6 addresses and an out-of-band one, a device whose
// primary address is the outside of a NAT, a VM's primary and non-primary
// addresses, an FHRP VIP, a NAT pair, a VRF with an RD and one without,
// anycast and mixed-assignment hosts, scoped and overlapping prefixes, a rack
// with a facility id, and VLANs on prefixes.
func ipWorld() *fakeService {
	f := newFakeService()
	f.addIPEnrichmentTables()
	named := func(id int, name string) map[string]interface{} {
		return map[string]interface{}{"id": float64(id), "name": name, "slug": slugOf(name)}
	}
	f.addRows("tenancy/tenants", named(1, "Grafana Demo"), named(2, "parity-tenant"))
	f.addRows("dcim/sites", named(1, "AMS1"))
	f.addRows("dcim/device-roles", named(1, "Leaf"), named(2, "Spine"))
	f.addRows("dcim/device-types", map[string]interface{}{"id": float64(1), "model": "DCS-7050SX3", "slug": "dcs-7050sx3"})
	f.addRows("dcim/locations", named(1, "parity-loc"))
	f.addRows("dcim/regions", named(1, "parity-region"))
	f.addRows("dcim/site-groups", named(1, "parity-sitegroup"))
	f.addRows("ipam/roles", named(1, "LAN"))
	f.addRows("dcim/racks",
		map[string]interface{}{"id": float64(1), "name": "parity-rack", "facility_id": "PR-1"},
		map[string]interface{}{"id": float64(2), "name": "rack-2", "facility_id": nil})
	f.addRows("ipam/vrfs",
		map[string]interface{}{"id": float64(1), "name": "parity-vrf-a", "rd": "65000:101"},
		map[string]interface{}{"id": float64(2), "name": "vrf-b", "rd": nil},
		map[string]interface{}{"id": float64(3), "name": "vrf-c", "rd": "65000:103"})
	f.addRows("ipam/vlans",
		map[string]interface{}{"id": float64(1), "name": "ams1-lan", "vid": float64(110)},
		map[string]interface{}{"id": float64(2), "name": "parity-vlan", "vid": float64(3999)})

	f.addRows("dcim/devices",
		ipDevice(1, "leaf-01", "active", 1, 1, 1, 1, 1, 100, 101),
		ipDevice(2, "spine-01", "offline", 2, 0, 2, 0, 0, 401, 0),
		ipDevice(3, "leaf-02", "active", 1, 1, 0, 0, 0, 0, 0))
	f.addRows("dcim/interfaces",
		ipIface(10, 1, "eth0", "uplink", "to spine"),
		ipIface(11, 1, "mgmt0", "", ""),
		ipIface(12, 2, "Ethernet1", "", ""),
		ipIface(13, 3, "Ethernet1", "", ""))
	f.addRows("virtualization/virtual-machines", map[string]interface{}{"id": float64(1), "name": "vm-01", "primary_ip4_id": float64(200), "primary_ip6_id": float64(202)})
	f.addRows("virtualization/interfaces", map[string]interface{}{"id": float64(20), "name": "eth0", "description": "vm uplink", "virtual_machine_id": float64(1)})
	f.addRows("ipam/fhrp-groups", idRow(1))

	f.addRows("ipam/ip-addresses",
		ipAddr(100, "10.1.0.1/24", idInterface, 10).with("dns_name", "leaf01.example.net").with("tenant_id", float64(1)),
		ipAddr(101, "2001:db8:1::1/64", idInterface, 10),
		ipAddr(102, "10.1.9.1/24", idInterface, 11),
		ipAddr(200, "10.4.0.1/24", idVMInterface, 20),
		ipAddr(201, "10.4.0.2/24", idVMInterface, 20),
		ipAddr(202, "2001:db8:4::1/64", idVMInterface, 20),
		ipAddr(300, "10.5.0.1/24", idFHRPGroup, 1).with("role", "vrrp"),
		ipAddr(400, "10.6.0.10/24", idInterface, 12).with("description", "nat inside"),
		// The outsides of 400, stored as the replica stores an inet: without a
		// single-host mask. 401 is spine-01's primary address.
		ipAddr(401, "198.51.100.250", 0, 0).with("nat_inside_id", float64(400)),
		ipAddr(402, "10.9.9.9/24", 0, 0).with("nat_inside_id", float64(400)),
		ipAddr(403, "2001:db8:ff::1", 0, 0).with("nat_inside_id", float64(400)),
		// An inside that is a single host, stored bare, and its outsides: two
		// of one host (in two VRFs, say), served out of id order, which is the
		// order between them.
		ipAddr(404, "10.6.0.20", idInterface, 12),
		ipAddr(405, "203.0.113.20", 0, 0).with("nat_inside_id", float64(404)),
		ipAddr(409, "198.51.100.7/24", 0, 0).with("nat_inside_id", float64(404)),
		ipAddr(408, "198.51.100.7", 0, 0).with("nat_inside_id", float64(404)),
		ipAddr(500, "10.250.1.10/24", 0, 0).with("vrf_id", float64(1)),
		ipAddr(501, "10.250.2.10/24", 0, 0).with("vrf_id", float64(2)),
		// Anycast: three interface assignments; the deprecated one is out of
		// the running, the two active ones tie, and the lower id wins.
		ipAddr(600, "10.99.99.99", idInterface, 10).with("status", "deprecated").with("role", "anycast"),
		ipAddr(602, "10.99.99.99", idInterface, 13).with("role", "anycast"),
		ipAddr(601, "10.99.99.99", idInterface, 12).with("role", "anycast"),
		// One host three ways: unassigned (lowest id), on a group, and on an
		// interface while deprecated — the interface is the pick.
		ipAddr(700, "10.7.0.1/24", 0, 0),
		ipAddr(701, "10.7.0.1/24", idFHRPGroup, 1),
		ipAddr(702, "10.7.0.1/24", idInterface, 13).with("status", "deprecated"),
		// Unassigned against a group: the group is the pick.
		ipAddr(710, "10.7.0.2/24", 0, 0),
		ipAddr(711, "10.7.0.2/24", idFHRPGroup, 1))

	scoped := func(r map[string]interface{}, level string, id int) map[string]interface{} {
		r["scope_id"], r["scope_type_id"] = float64(id), float64(1)
		r[level] = float64(id)
		return r
	}
	f.addRows("ipam/prefixes",
		ipPrefix(1, "10.0.0.0/8", 0, ""),
		scoped(ipPrefix(2, "10.10.10.0/24", 0, "").with("tenant_id", float64(1)).with("role_id", float64(1)).with("vlan_id", float64(1)), "_site_id", 1),
		ipPrefix(5, "10.250.0.0/16", 0, "global-16"),
		ipPrefix(6, "10.250.0.0/16", 1, "vrf-16"),
		ipPrefix(7, "10.250.1.0/24", 1, "vrf-nested-24"),
		// Scoped to a location, which NetBox caches with the location's site.
		scoped(scoped(ipPrefix(8, "10.250.10.0/24", 0, "").with("vlan_id", float64(2)), "_site_id", 1), "_location_id", 1),
		scoped(ipPrefix(9, "10.250.11.0/24", 0, ""), "_region_id", 1),
		scoped(ipPrefix(10, "10.250.12.0/24", 0, ""), "_site_group_id", 1),
		ipPrefix(11, "10.250.20.7/32", 0, "host-32"),
		// The same /16 in two VRFs, one of them twice, served out of
		// NetBox's order: VRF id 1 comes first, then the lower id.
		ipPrefix(14, "10.251.0.0/16", 1, "dup-b"),
		ipPrefix(12, "10.251.0.0/16", 3, "vrf-c"),
		ipPrefix(13, "10.251.0.0/16", 1, "dup-a"))
	return f
}

// fakeRow is a fake table row that can be extended in place.
type fakeRow map[string]interface{}

func (r fakeRow) with(k string, v interface{}) fakeRow { r[k] = v; return r }

func ipAddr(id int, address string, typeID, objectID int) fakeRow {
	return fakeRow{"id": float64(id), "address": address, "status": "active", "role": nil, "dns_name": "", "description": "",
		"tenant_id": nil, "vrf_id": nil, "nat_inside_id": nil,
		"assigned_object_type_id": num(typeID), "assigned_object_id": num(objectID)}
}

func ipDevice(id int, name, status string, role, tenant, rack, location, deviceType, ip4, ip6 int) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "name": name, "status": status, "role_id": num(role), "tenant_id": num(tenant),
		"site_id": float64(1), "rack_id": num(rack), "location_id": num(location), "device_type_id": num(deviceType),
		"platform_id": nil, "primary_ip4_id": num(ip4), "primary_ip6_id": num(ip6)}
}

func ipIface(id, device int, name, label, description string) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "device_id": float64(device), "name": name, "_name": name, "label": label,
		"description": description, "_path_id": nil, "cable_id": nil, "cable_end": nil}
}

func ipPrefix(id int, prefix string, vrf int, description string) fakeRow {
	return fakeRow{"id": float64(id), "prefix": prefix, "status": "active", "vrf_id": num(vrf), "description": description,
		"tenant_id": nil, "role_id": nil, "vlan_id": nil, "scope_id": nil, "scope_type_id": nil,
		"_location_id": nil, "_region_id": nil, "_site_id": nil, "_site_group_id": nil}
}

// addFakeRows appends fakeRows to an entity of the fake.
func (f *fakeService) addFakeRows(objectType string, rows ...fakeRow) {
	for _, r := range rows {
		f.addRows(objectType, r)
	}
}

func resolveIPs(t *testing.T, p *Provider, ips []string, fields ...string) *provider.Result {
	t.Helper()
	res, err := p.ResolveIPs(context.Background(), ips, fields, 0)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// rowFor is the row whose ip is ip.
func rowFor(t *testing.T, res *provider.Result, ip string) map[string]interface{} {
	t.Helper()
	for _, row := range res.Rows {
		if row["ip"] == ip {
			return row
		}
	}
	t.Fatalf("no row for %s in %v", ip, res.Rows)
	return nil
}

// wantRow checks the columns named in want, and that the row has exactly the
// result's columns.
func wantRow(t *testing.T, res *provider.Result, ip string, want map[string]interface{}) {
	t.Helper()
	row := rowFor(t, res, ip)
	for k, v := range want {
		if got, ok := row[k]; !ok || !reflect.DeepEqual(got, v) {
			t.Errorf("%s: %s = %#v (present %v), want %#v", ip, k, got, ok, v)
		}
	}
	if len(row) != len(res.Columns) {
		t.Errorf("%s: row has %d columns, the result %d: %v", ip, len(row), len(res.Columns), row)
	}
}

// noWarnings fails on any warning: each one fails an alert rule.
func noWarnings(t *testing.T, res *provider.Result) {
	t.Helper()
	if len(res.Warnings) > 0 {
		t.Errorf("warnings on a complete answer: %q", res.Warnings)
	}
}

// allColumns is every column ip-enrichment emits.
func allColumns() []string { return provider.IPEnrichColumns() }

// A device interface's address: every column, as NetBox mode fills it.
func TestResolveIPs_DeviceInterfaceAddress(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.1.0.1"}, allColumns()...)
	noWarnings(t, res)
	if !reflect.DeepEqual(res.Columns, allColumns()) {
		t.Errorf("columns = %v", res.Columns)
	}
	wantRow(t, res, "10.1.0.1", map[string]interface{}{
		"ip": "10.1.0.1", "match_count": float64(1), "is_primary_ip": true,
		"prefix_cidr": nil, "prefix_scope": nil, "prefix_tenant": nil, "prefix_role": nil,
		"prefix_vrf": nil, "prefix_vlan": nil, "prefix_description": nil,
		"address_dns_name": "leaf01.example.net", "address_status": "Active", "address_role": nil, "address_vrf": nil,
		"address_tenant": "Grafana Demo", "address_description": "", "address_assigned_object_type": "dcim.interface",
		"address_nat_inside": nil, "address_nat_outside": "",
		"interface_name": "eth0 (uplink)", "interface_description": "to spine",
		"device_name": "leaf-01", "device_role": "Leaf", "device_platform": nil, "device_device_type": "DCS-7050SX3",
		"device_site": "AMS1", "device_location": "parity-loc", "device_rack": "parity-rack (PR-1)", "device_tenant": "Grafana Demo",
		"device_status": "Active", "vm_name": nil,
	})
}

// Both of a device's primary addresses are its primary, whatever the input's
// spelling: a mask and an uppercase IPv6 address find the same record.
func TestResolveIPs_DevicePrimaryIPv4AndIPv6(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.1.0.1", "2001:DB8:1::1/128"}, "is_primary_ip", "device_name")
	noWarnings(t, res)
	for _, ip := range []string{"10.1.0.1", "2001:DB8:1::1/128"} {
		wantRow(t, res, ip, map[string]interface{}{"is_primary_ip": true, "device_name": "leaf-01"})
	}
}

// An out-of-band address is the device's, and not its primary.
func TestResolveIPs_OOBIsNotPrimary(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.1.9.1"}, "interface_name", "interface_description", "device_name", "is_primary_ip")
	noWarnings(t, res)
	// A blank interface description is no description, as in NetBox mode.
	wantRow(t, res, "10.1.9.1", map[string]interface{}{"interface_name": "mgmt0", "interface_description": nil, "device_name": "leaf-01", "is_primary_ip": false})
}

// A device's primary address that is the outside of a NAT is assigned to
// nothing: its owner is what it is assigned to, as in NetBox mode, so the
// device columns stay blank and so does the flag — blank, not false.
func TestResolveIPs_NATOutsidePrimaryLeavesOwnerBlank(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"198.51.100.250"}, allColumns()...)
	noWarnings(t, res)
	wantRow(t, res, "198.51.100.250", map[string]interface{}{
		"match_count": float64(1), "address_nat_inside": "10.6.0.10/24", "address_nat_outside": "",
		"address_assigned_object_type": nil, "interface_name": nil, "device_name": nil, "vm_name": nil, "is_primary_ip": nil,
	})
}

func TestResolveIPs_VMPrimary(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.4.0.1", "2001:db8:4::1"}, allColumns()...)
	noWarnings(t, res)
	wantRow(t, res, "2001:db8:4::1", map[string]interface{}{"vm_name": "vm-01", "is_primary_ip": true})
	wantRow(t, res, "10.4.0.1", map[string]interface{}{
		"address_assigned_object_type": "virtualization.vminterface", "interface_name": "eth0", "interface_description": "vm uplink",
		"vm_name": "vm-01", "is_primary_ip": true, "device_name": nil, "device_site": nil,
	})
}

// A VM address that is not the VM's primary carries the type id the primary
// one taught: the type is learned from another row.
func TestResolveIPs_VMNonPrimaryWithLearnedType(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.4.0.2"}, "vm_name", "is_primary_ip", "address_assigned_object_type")
	noWarnings(t, res)
	wantRow(t, res, "10.4.0.2", map[string]interface{}{
		"vm_name": "vm-01", "is_primary_ip": false, "address_assigned_object_type": "virtualization.vminterface",
	})
}

// An FHRP VIP names its group's type and nothing else: a group is neither an
// interface nor a device, and has no primary address to be.
func TestResolveIPs_FHRP(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.5.0.1"}, allColumns()...)
	noWarnings(t, res)
	wantRow(t, res, "10.5.0.1", map[string]interface{}{
		"address_assigned_object_type": "ipam.fhrpgroup", "address_role": "VRRP",
		"interface_name": nil, "interface_description": nil, "device_name": nil, "vm_name": nil, "is_primary_ip": nil,
	})
}

// A host with no address record takes its prefix's columns; one with a record
// — an unassigned one too — does not, as in NetBox mode.
func TestResolveIPs_UnassignedFallsBackToPrefix(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.10.10.77", "10.250.1.10"},
		"match_count", "prefix_cidr", "prefix_scope", "prefix_tenant", "prefix_role", "prefix_vrf", "prefix_vlan", "prefix_description", "address_vrf")
	noWarnings(t, res)
	wantRow(t, res, "10.10.10.77", map[string]interface{}{
		"match_count": float64(0), "prefix_cidr": "10.10.10.0/24", "prefix_scope": "AMS1", "prefix_tenant": "Grafana Demo",
		"prefix_role": "LAN", "prefix_vrf": nil, "prefix_vlan": "ams1-lan (110)", "prefix_description": "", "address_vrf": nil,
	})
	wantRow(t, res, "10.250.1.10", map[string]interface{}{
		"match_count": float64(1), "prefix_cidr": nil, "address_vrf": "parity-vrf-a (65000:101)",
	})
}

// A bare input asks NetBox's strict question (a /32 equal to it is left out);
// a masked one asks about its host, inclusively, and not about its network.
func TestResolveIPs_MaskedInputFallsBackAsHost(t *testing.T) {
	f := ipWorld()
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.250.20.7", "10.250.20.7/24", "10.250.10.5/16"}, "prefix_cidr", "prefix_description")
	noWarnings(t, res)
	wantRow(t, res, "10.250.20.7", map[string]interface{}{"prefix_cidr": "10.250.0.0/16", "prefix_description": "global-16"})
	wantRow(t, res, "10.250.20.7/24", map[string]interface{}{"prefix_cidr": "10.250.20.7/32", "prefix_description": "host-32"})
	wantRow(t, res, "10.250.10.5/16", map[string]interface{}{"prefix_cidr": "10.250.10.0/24"})
	sent := f.sentWith("ipam/prefixes", "filter[prefix]__contains")
	if len(sent) != 1 || sent[0].query.Get("filter[prefix]__contains") != "10.250.20.7,10.250.20.7/32,10.250.10.5/32" {
		t.Errorf("contains sent as %v", sent)
	}
}

func TestResolveIPs_OutsideEveryPrefix(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"192.0.2.1"}, "match_count", "prefix_cidr", "prefix_scope")
	noWarnings(t, res)
	wantRow(t, res, "192.0.2.1", map[string]interface{}{"match_count": float64(0), "prefix_cidr": nil, "prefix_scope": nil})
}

// A value that is not an address is never sent: the replica refuses a whole
// host list for one such value. Its row is the row NetBox mode gives it.
func TestResolveIPs_InvalidInput(t *testing.T) {
	f := ipWorld()
	res := resolveIPs(t, newTestProvider(t, f), []string{"not-an-ip", "fe80::1%eth0", "10.1.0.1"}, "match_count", "device_name", "prefix_cidr")
	noWarnings(t, res)
	for _, ip := range []string{"not-an-ip", "fe80::1%eth0"} {
		wantRow(t, res, ip, map[string]interface{}{"match_count": float64(0), "device_name": nil, "prefix_cidr": nil})
	}
	wantRow(t, res, "10.1.0.1", map[string]interface{}{"match_count": float64(1), "device_name": "leaf-01"})
	sent := f.sentWith("ipam/ip-addresses", "filter[address]__host")
	if len(sent) != 1 || sent[0].query.Get("filter[address]__host") != "10.1.0.1" {
		t.Errorf("host match sent as %v", sent)
	}
	if n := len(f.sentWith("ipam/prefixes", "filter[prefix]__contains")); n != 0 {
		t.Errorf("%d containment requests for values that are not addresses", n)
	}

	f = ipWorld()
	res = resolveIPs(t, newTestProvider(t, f), []string{"not-an-ip"}, "match_count")
	if n := f.countRequestsFor("ipam/ip-addresses"); n != 0 || len(res.Rows) != 1 {
		t.Errorf("nothing to ask: %d requests, %d rows", n, len(res.Rows))
	}
}

// Several records for one host: NetBox mode's pick, its match_count, and the
// note that says a pick was made.
func TestResolveIPs_AmbiguousHostPicksLikeNetBox(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.99.99.99", "10.7.0.1", "10.7.0.2"},
		"match_count", "address_status", "address_assigned_object_type", "device_name")
	noWarnings(t, res)
	wantRow(t, res, "10.99.99.99", map[string]interface{}{"match_count": float64(3), "address_status": "Active", "device_name": "spine-01"})
	wantRow(t, res, "10.7.0.1", map[string]interface{}{"match_count": float64(3), "address_status": "Deprecated",
		"address_assigned_object_type": "dcim.interface", "device_name": "leaf-02"})
	wantRow(t, res, "10.7.0.2", map[string]interface{}{"match_count": float64(2), "address_assigned_object_type": "ipam.fhrpgroup", "device_name": nil})
	if want := provider.AmbiguityNote(3, 3); !slices.Contains(res.Notes, want) {
		t.Errorf("notes %q lack %q", res.Notes, want)
	}
}

// An address on an object of a type this replica cannot name leaves the
// owner blank and says so, counted; a host whose pick that type decides keeps
// its lowest id and says so too, while one whose pick it cannot change does
// not.
func TestResolveIPs_UnclassifiedAssignmentWarns(t *testing.T) {
	captureLogs(t) // the learn logs the contradiction it rules 999 out on
	f := ipWorld()
	f.addFakeRows("ipam/ip-addresses",
		ipAddr(800, "10.8.0.1/24", 999, 5),
		// An active interface record beats whatever 811 is (a tie at best,
		// which the lower id wins).
		ipAddr(810, "10.8.0.2/24", idInterface, 12),
		ipAddr(811, "10.8.0.2/24", 999, 5),
		// Here 820 would win were it an interface: undecidable.
		ipAddr(820, "10.8.0.3/24", 999, 5),
		ipAddr(821, "10.8.0.3/24", idInterface, 12))
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.8.0.1", "10.8.0.2", "10.8.0.3"}, allColumns()...)
	for _, ip := range []string{"10.8.0.1", "10.8.0.3"} {
		wantRow(t, res, ip, map[string]interface{}{"address_assigned_object_type": nil, "interface_name": nil,
			"device_name": nil, "vm_name": nil, "is_primary_ip": nil, "address_status": "Active"})
	}
	wantRow(t, res, "10.8.0.2", map[string]interface{}{"address_assigned_object_type": "dcim.interface", "device_name": "spine-01"})
	want := []string{
		"1 of 3 rows matched several address records, some of them assigned to objects this replica cannot identify, so the record they show is the one with the lowest id among those NetBox mode could pick, which may not be the one it picks.",
		"2 addresses are assigned to objects this replica cannot identify (it does not serve NetBox's content-type names), so their address_assigned_object_type, interface_*, device_*, vm_name and is_primary_ip columns are blank.",
	}
	if !reflect.DeepEqual(res.Warnings, want) {
		t.Errorf("warnings = %q, want %q", res.Warnings, want)
	}

	// One address takes the singular, and the warning names the columns the
	// frame has that the type blanks: a column alone by its name, a group of
	// them by the group's.
	for fields, want := range map[string]string{
		"device_name":                                 "1 address is assigned to an object this replica cannot identify (it does not serve NetBox's content-type names), so its device_name column is blank.",
		"address_assigned_object_type":                "1 address is assigned to an object this replica cannot identify (it does not serve NetBox's content-type names), so its address_assigned_object_type column is blank.",
		"device_name,device_site,vm_name":             "1 address is assigned to an object this replica cannot identify (it does not serve NetBox's content-type names), so its device_* and vm_name columns are blank.",
		"interface_name,is_primary_ip,ip,match_count": "1 address is assigned to an object this replica cannot identify (it does not serve NetBox's content-type names), so its interface_name and is_primary_ip columns are blank.",
	} {
		res = resolveIPs(t, newTestProvider(t, f), []string{"10.8.0.1"}, strings.Split(fields, ",")...)
		if !reflect.DeepEqual(res.Warnings, []string{want}) {
			t.Errorf("%s: warnings = %q, want %q", fields, res.Warnings, want)
		}
	}
	// A count is written as the result's other counts are.
	many := ipWorld()
	var ips []string
	for i := range 1000 {
		ip := fmt.Sprintf("10.9.%d.%d", i/250, 1+i%250)
		ips = append(ips, ip)
		many.addFakeRows(addressesEntity, ipAddr(20000+i, ip+"/24", 999, 5))
	}
	res = resolveIPs(t, newTestProvider(t, many), ips, "device_name")
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "1,000 addresses are assigned") {
		t.Errorf("warnings = %q", res.Warnings)
	}
	// A frame with none of the columns the type fills has nothing blank to
	// warn about.
	res = resolveIPs(t, newTestProvider(t, f), []string{"10.8.0.1"}, "match_count", "address_dns_name")
	noWarnings(t, res)
	// Nor one that shows nothing of which record was picked.
	res = resolveIPs(t, newTestProvider(t, f), []string{"10.8.0.3"}, "match_count")
	noWarnings(t, res)
}

// NetBox's own renderings of the objects a row names.
func TestResolveIPs_DisplayStrings(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.250.1.10", "10.250.2.10", "10.1.0.1", "10.1.9.1", "10.6.0.10", "10.250.10.5", "10.251.0.5"},
		"address_vrf", "address_status", "device_rack", "device_status", "device_device_type", "interface_name", "prefix_vlan", "prefix_vrf")
	noWarnings(t, res)
	wantRow(t, res, "10.250.1.10", map[string]interface{}{"address_vrf": "parity-vrf-a (65000:101)", "address_status": "Active"})
	wantRow(t, res, "10.250.2.10", map[string]interface{}{"address_vrf": "vrf-b"})
	wantRow(t, res, "10.1.0.1", map[string]interface{}{"device_rack": "parity-rack (PR-1)", "device_status": "Active",
		"device_device_type": "DCS-7050SX3", "interface_name": "eth0 (uplink)"})
	wantRow(t, res, "10.1.9.1", map[string]interface{}{"interface_name": "mgmt0"})
	wantRow(t, res, "10.6.0.10", map[string]interface{}{"device_rack": "rack-2", "device_status": "Offline", "interface_name": "Ethernet1"})
	wantRow(t, res, "10.250.10.5", map[string]interface{}{"prefix_vlan": "parity-vlan (3999)", "prefix_vrf": nil})
	wantRow(t, res, "10.251.0.5", map[string]interface{}{"prefix_vrf": "parity-vrf-a (65000:101)"})
}

// The inside of a NAT names its outsides, ordered as NetBox orders addresses
// (family, then host) and each with its mask; an outside names its inside.
func TestResolveIPs_NATColumns(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.6.0.10", "198.51.100.250", "10.1.0.1", "203.0.113.20", "10.6.0.20"}, "address_nat_inside", "address_nat_outside")
	noWarnings(t, res)
	wantRow(t, res, "10.6.0.10", map[string]interface{}{"address_nat_inside": nil,
		"address_nat_outside": "10.9.9.9/24; 198.51.100.250/32; 2001:db8:ff::1/128"})
	wantRow(t, res, "198.51.100.250", map[string]interface{}{"address_nat_inside": "10.6.0.10/24", "address_nat_outside": ""})
	wantRow(t, res, "203.0.113.20", map[string]interface{}{"address_nat_inside": "10.6.0.20/32", "address_nat_outside": ""})
	wantRow(t, res, "10.6.0.20", map[string]interface{}{"address_nat_inside": nil, "address_nat_outside": "198.51.100.7/32; 198.51.100.7/24; 203.0.113.20/32"})
	wantRow(t, res, "10.1.0.1", map[string]interface{}{"address_nat_inside": nil, "address_nat_outside": ""})
}

// A host with no record has match_count 0 — a number — and nothing else: no
// address columns at all, not even the empty NAT outside list a record has.
func TestResolveIPs_NoRecordRowShape(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"192.0.2.1"}, allColumns()...)
	noWarnings(t, res)
	want := map[string]interface{}{}
	for _, c := range allColumns() {
		want[c] = nil
	}
	want["ip"], want["match_count"] = "192.0.2.1", float64(0)
	if got := rowFor(t, res, "192.0.2.1"); !reflect.DeepEqual(got, want) {
		t.Errorf("row = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(res.ColumnTypes, map[string]provider.FieldType{"match_count": provider.FieldTypeNumber, "is_primary_ip": provider.FieldTypeBoolean}) {
		t.Errorf("column types = %v", res.ColumnTypes)
	}
}

// Longest mask first; between prefixes of one length, NetBox's order: the
// global table first, then VRF id, then id.
func TestResolveIPs_LongestPrefixTieFollowsNetBoxOrder(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.250.99.1", "10.251.0.5", "10.250.1.99"}, "prefix_cidr", "prefix_description")
	noWarnings(t, res)
	wantRow(t, res, "10.250.99.1", map[string]interface{}{"prefix_cidr": "10.250.0.0/16", "prefix_description": "global-16"})
	wantRow(t, res, "10.251.0.5", map[string]interface{}{"prefix_cidr": "10.251.0.0/16", "prefix_description": "dup-a"})
	wantRow(t, res, "10.250.1.99", map[string]interface{}{"prefix_cidr": "10.250.1.0/24", "prefix_description": "vrf-nested-24"})
}

// A prefix's scope is named from the most specific level NetBox caches on it.
func TestResolveIPs_PrefixScopeNames(t *testing.T) {
	res := resolveIPs(t, newTestProvider(t, ipWorld()), []string{"10.10.10.5", "10.250.10.5", "10.250.11.5", "10.250.12.5", "10.250.99.1"}, "prefix_scope")
	noWarnings(t, res)
	for ip, scope := range map[string]interface{}{"10.10.10.5": "AMS1", "10.250.10.5": "parity-loc", "10.250.11.5": "parity-region",
		"10.250.12.5": "parity-sitegroup", "10.250.99.1": nil} {
		wantRow(t, res, ip, map[string]interface{}{"prefix_scope": scope})
	}

	// A scope with no level cached (NetBox caches one whenever it saves a
	// scoped prefix) cannot be named: a gap, counted.
	f := ipWorld()
	r := ipPrefix(30, "10.252.0.0/16", 0, "")
	r["scope_id"], r["scope_type_id"] = float64(4), float64(1)
	f.addFakeRows("ipam/prefixes", r)
	res = resolveIPs(t, newTestProvider(t, f), []string{"10.252.0.1"}, "prefix_cidr", "prefix_scope")
	wantRow(t, res, "10.252.0.1", map[string]interface{}{"prefix_cidr": "10.252.0.0/16", "prefix_scope": nil})
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "Related objects are missing for 1 of 1 rows") {
		t.Errorf("warnings = %q, want the uncached scope counted", res.Warnings)
	}
}

// A tenant whose NetBox has no VRFs, racks, VLANs, VMs, FHRP groups, regions,
// site groups, locations or platforms has those tables unfed: an answer from
// the rest is complete, and says nothing.
func TestResolveIPs_AuxiliaryTablesUnfed(t *testing.T) {
	f := newFakeService()
	f.addIPEnrichmentTables()
	f.addRows("dcim/sites", map[string]interface{}{"id": float64(1), "name": "AMS1", "slug": "ams1"})
	f.addRows("dcim/devices", ipDevice(1, "leaf-01", "active", 0, 0, 0, 0, 0, 100, 0))
	f.addRows("dcim/interfaces", ipIface(10, 1, "eth0", "", ""))
	f.addFakeRows("ipam/ip-addresses", ipAddr(100, "10.1.0.1/24", idInterface, 10))
	f.addFakeRows("ipam/prefixes", ipPrefix(1, "10.0.0.0/8", 0, "all of ten"))
	f.unfeed("ipam/vrfs", "dcim/racks", "ipam/vlans", "virtualization/virtual-machines", "virtualization/interfaces",
		"ipam/fhrp-groups", "dcim/regions", "dcim/site-groups", "dcim/locations", "dcim/device-roles", "dcim/device-types",
		"tenancy/tenants", "ipam/roles")
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.1.0.1", "10.2.0.1", "192.0.2.1"}, allColumns()...)
	noWarnings(t, res)
	wantRow(t, res, "10.1.0.1", map[string]interface{}{"device_name": "leaf-01", "device_site": "AMS1", "device_role": nil,
		"device_rack": nil, "interface_name": "eth0", "is_primary_ip": true, "address_vrf": nil, "address_tenant": nil})
	wantRow(t, res, "10.2.0.1", map[string]interface{}{"match_count": float64(0), "prefix_cidr": "10.0.0.0/8", "prefix_vrf": nil, "prefix_scope": nil})
	wantRow(t, res, "192.0.2.1", map[string]interface{}{"match_count": float64(0), "prefix_cidr": nil})
}

// No fields is the default selection, with "ip" first; nothing the default
// does not show is read.
func TestResolveIPs_DefaultFields(t *testing.T) {
	f := ipWorld()
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.1.0.1", "192.0.2.1"})
	if !reflect.DeepEqual(res.Columns, provider.DefaultIPEnrichFields()) {
		t.Errorf("columns = %v", res.Columns)
	}
	wantRow(t, res, "10.1.0.1", map[string]interface{}{"match_count": float64(1), "address_dns_name": "leaf01.example.net",
		"device_name": "leaf-01", "vm_name": nil, "interface_name": "eth0 (uplink)", "is_primary_ip": true,
		"device_site": "AMS1", "device_tenant": "Grafana Demo"})
	if n := f.countRequestsFor("ipam/prefixes"); n != 0 {
		t.Errorf("%d prefix requests for a selection with no prefix column", n)
	}
}

// One row per distinct input, up to the limit; Total counts every distinct
// input, so a clamped list reads as truncated.
func TestResolveIPs_LimitAndTotal(t *testing.T) {
	p := newTestProvider(t, ipWorld())
	res, err := p.ResolveIPs(context.Background(), []string{"10.1.0.1", " 10.1.0.1 ", "", "10.4.0.1", "10.5.0.1", "192.0.2.1"}, []string{"match_count"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	var ips []interface{}
	for _, row := range res.Rows {
		ips = append(ips, row["ip"])
	}
	if !reflect.DeepEqual(ips, []interface{}{"10.1.0.1", "10.4.0.1"}) || res.Total != 4 || res.MaxRows != MaxLimit {
		t.Errorf("rows %v, Total %d, MaxRows %d; want two rows of 4", ips, res.Total, res.MaxRows)
	}
	if !reflect.DeepEqual(res.Columns, []string{"ip", "match_count"}) {
		t.Errorf("columns = %v", res.Columns)
	}
}

// Nothing matches: every row is the no-record row, and the default selection
// asks for no prefix.
func TestResolveIPs_NoMatches(t *testing.T) {
	f := ipWorld()
	res := resolveIPs(t, newTestProvider(t, f), []string{"192.0.2.1", "192.0.2.2"})
	noWarnings(t, res)
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %v", res.Rows)
	}
	for _, row := range res.Rows {
		if row["match_count"] != float64(0) || row["device_name"] != nil || row["is_primary_ip"] != nil {
			t.Errorf("row = %v", row)
		}
	}
	if len(res.Notes) != 1 || !strings.HasPrefix(res.Notes[0], dataAsOfPrefix) {
		t.Errorf("notes = %q", res.Notes)
	}
}

// Thousands of hosts go in as many requests as the budget takes, and the
// prefix fallback's in as many as the containment limit takes: the same answer
// as one request, from requests the replica accepts.
func TestResolveIPs_BatchesHostsAndContains(t *testing.T) {
	f := ipWorld()
	var ips []string
	for i := range 2500 {
		ips = append(ips, fmt.Sprintf("2001:db8:abcd:1234:5678:9abc:%x:%x", 0x1000+i/256, 0x1000+i%256))
	}
	for i := range 3000 {
		ips = append(ips, fmt.Sprintf("10.200.%d.%d", i/250, 1+i%250))
	}
	f.addFakeRows("ipam/prefixes", ipPrefix(20, "2001:db8:abcd::/48", 0, "v6"), ipPrefix(21, "10.200.0.0/16", 0, "v4"))
	f.addFakeRows("ipam/ip-addresses", ipAddr(900, ips[0]+"/64", 0, 0), ipAddr(901, ips[3000]+"/24", 0, 0))
	p := newTestProvider(t, f)
	res, err := p.ResolveIPs(context.Background(), ips, []string{"match_count", "prefix_cidr"}, MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	noWarnings(t, res)
	if len(res.Rows) != len(ips) || res.Total != len(ips) {
		t.Fatalf("%d rows, Total %d; want %d", len(res.Rows), res.Total, len(ips))
	}
	for i, row := range res.Rows {
		want := map[string]interface{}{"ip": ips[i], "match_count": float64(0), "prefix_cidr": "2001:db8:abcd::/48"}
		switch {
		case i == 0 || i == 3000:
			want["match_count"], want["prefix_cidr"] = float64(1), nil
		case i >= 2500:
			want["prefix_cidr"] = "10.200.0.0/16"
		}
		if !reflect.DeepEqual(row, want) {
			t.Fatalf("row %d = %v, want %v", i, row, want)
		}
	}
	hosts := f.sentWith("ipam/ip-addresses", "filter[address]__host")
	contains := f.sentWith("ipam/prefixes", "filter[prefix]__contains")
	if len(hosts) < 2 || len(contains) < 4 {
		t.Errorf("%d host requests and %d containment requests; want the hosts split by the budget and the prefixes by the 1,000-value limit", len(hosts), len(contains))
	}
	for _, r := range append(hosts, contains...) {
		if !fitsBudget(withoutCursor(r.query)) {
			t.Errorf("a request of %d bytes", len(r.query.Encode()))
		}
	}
	largest := 0
	for _, r := range contains {
		largest = max(largest, len(strings.Split(r.query.Get("filter[prefix]__contains"), ",")))
	}
	// The limit, not the budget, is what bounds the IPv4 batches.
	if largest != fakeContainsMaxValues {
		t.Errorf("the largest containment request carries %d values, want the limit of %d", largest, fakeContainsMaxValues)
	}
}

// The result is as old as its oldest read, and a replica still loading with
// no instant to report says so in a warning, as an objects query does.
func TestResolveIPs_CarriesFreshness(t *testing.T) {
	f := ipWorld()
	f.asOfs = []string{"2026-09-22T14:10:00Z", "2026-09-22T13:00:00Z"}
	for range 60 {
		f.asOfs = append(f.asOfs, "2026-09-22T14:10:00Z")
	}
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.1.0.1", "10.10.10.5"}, allColumns()...)
	noWarnings(t, res)
	want := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	if res.DataAsOf == nil || !res.DataAsOf.Equal(want) {
		t.Errorf("DataAsOf = %v, want %v", res.DataAsOf, want)
	}
	if res.SnapshotComplete == nil || !*res.SnapshotComplete {
		t.Errorf("SnapshotComplete = %v", res.SnapshotComplete)
	}
	if !slices.Contains(res.Notes, "Data as of 2026-09-22 13:00:00 UTC (replica-cache).") {
		t.Errorf("notes = %q", res.Notes)
	}

	loading := ipWorld()
	loading.mu.Lock()
	loading.schema.SnapshotComplete = false
	for path, e := range loading.schema.Entities {
		e.DataAsOf = nil
		loading.schema.Entities[path] = e
	}
	loading.mu.Unlock()
	res = resolveIPs(t, newTestProvider(t, loading), []string{"10.1.0.1"}, "device_name")
	if !reflect.DeepEqual(res.Warnings, []string{loadingWarning}) || res.DataAsOf != nil {
		t.Errorf("warnings = %q, DataAsOf %v; want the loading warning", res.Warnings, res.DataAsOf)
	}
	if res.SnapshotComplete == nil || *res.SnapshotComplete {
		t.Errorf("SnapshotComplete = %v", res.SnapshotComplete)
	}
}

func TestResolveIPs_RejectsBranch(t *testing.T) {
	f := ipWorld()
	p := newTestProvider(t, f)
	_, err := p.ResolveIPs(provider.WithBranch(context.Background(), "feature-x"), []string{"10.1.0.1"}, nil, 0)
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want the branch refusal", err)
	}
	if _, err := p.ResolveScope(provider.WithBranch(context.Background(), "feature-x"), []provider.Filter{{Field: "tenant", Value: "grafana-demo"}}, nil, 0); !errors.As(err, &unsupported) {
		t.Fatalf("scope err = %v, want the branch refusal", err)
	}
	if n := f.requestCount(); n != 0 {
		t.Errorf("%d requests for a refused branch", n)
	}
	if _, err := p.ResolveIPs(provider.WithBranch(context.Background(), "main"), []string{"10.1.0.1"}, nil, 0); err != nil {
		t.Errorf("main is the replica's own dataset: %v", err)
	}
}

// A scope is the addresses NetBox's filters select, through the objects
// translation; each host's records are the ones the scope listed, as in
// NetBox mode, and the rows come in NetBox's address order.
func TestResolveScope_TenantScope(t *testing.T) {
	f := ipWorld()
	f.mu.Lock()
	for _, row := range f.entities["ipam/ip-addresses"] {
		switch row["id"] {
		case float64(101), float64(600), float64(601), float64(401):
			row["tenant_id"] = float64(1)
		}
	}
	f.mu.Unlock()
	p := newTestProvider(t, f)
	scope := []provider.Filter{{Field: "tenant", Value: "grafana-demo"}}
	res, err := p.ResolveScope(context.Background(), scope, []string{"match_count", "address_status", "device_name", "address_tenant"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	noWarnings(t, res)
	var ips []interface{}
	for _, row := range res.Rows {
		ips = append(ips, row["ip"])
	}
	if want := []interface{}{"10.1.0.1", "10.99.99.99", "198.51.100.250", "2001:db8:1::1"}; !reflect.DeepEqual(ips, want) {
		t.Errorf("rows %v, want %v", ips, want)
	}
	if res.Total != 4 {
		t.Errorf("Total = %d, want the 4 hosts", res.Total)
	}
	// 10.99.99.99 has three records, two of them in the scope: those two are
	// what it matched, and the pick is made between them.
	wantRow(t, res, "10.99.99.99", map[string]interface{}{"match_count": float64(2), "address_status": "Active",
		"device_name": "spine-01", "address_tenant": "Grafana Demo"})
	if r, ok := f.requestWith("ipam/ip-addresses", "filter[tenant_id]__eq"); !ok || r.query.Get("filter[tenant_id]__eq") != "1" {
		t.Errorf("the tenant was not resolved to its id: %v", r)
	}
	if res.DataAsOf == nil {
		t.Error("the scope carries no instant")
	}

	// Cut off by the limit, the scope reads as truncated: Total is the
	// records the filter matches.
	res, err = p.ResolveScope(context.Background(), scope, []string{"match_count"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 || len(res.Rows) > 2 {
		t.Errorf("%d rows, Total %d; want at most 2 of 5", len(res.Rows), res.Total)
	}
}

// The replica has no address-within-prefix filter, so the filters NetBox
// answers from an address's network are refused by name, before anything is
// read. within is no address filter in NetBox at all.
func TestResolveScope_ParentRefusedByName(t *testing.T) {
	for _, flt := range []provider.Filter{
		{Field: "parent", Value: "10.0.0.0/8"},
		{Field: "mask_length", Value: "24"},
		{Field: "mask_length", Operator: "gte", Value: "24"},
		{Field: "family", Value: "4"},
	} {
		f := ipWorld()
		p := newTestProvider(t, f)
		_, err := p.ResolveScope(context.Background(), []provider.Filter{{Field: "tenant", Value: "grafana-demo"}, flt}, nil, 0)
		var refused *UnsupportedFilterError
		if !errors.As(err, &refused) || refused.Field != flt.Field || !strings.Contains(err.Error(), "the replica has no address-within-prefix filter") {
			t.Errorf("%v: err = %v, want a refusal naming %s and the missing filter", flt, err, flt.Field)
		}
		if n := f.requestCount(); n != 0 {
			t.Errorf("%v: %d requests before the refusal", flt, n)
		}
	}
	p := newTestProvider(t, ipWorld())
	if _, err := p.ResolveScope(context.Background(), []provider.Filter{{Field: "within", Value: "10.0.0.0/8"}}, nil, 0); err == nil {
		t.Error("within was answered")
	}
	// A row with no value sends nothing, here as everywhere.
	if _, err := p.ResolveScope(context.Background(), []provider.Filter{{Field: "tenant", Value: "grafana-demo"}, {Field: "parent"}}, nil, 0); err != nil {
		t.Errorf("an empty parent row: %v", err)
	}
}

// A read is made only for a column that shows what it reads, as NetBox mode
// makes a hop only for its columns: match_count needs the records alone, and
// is_primary_ip a device's two primary ids.
func TestResolveIPs_ReadsOnlyWhatTheColumnsNeed(t *testing.T) {
	read := func(f *fakeService) []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []string
		for _, r := range f.requests {
			if r.entity != "_meta/schema" && !slices.Contains(out, r.entity) {
				out = append(out, r.entity)
			}
		}
		return out
	}
	f := ipWorld()
	resolveIPs(t, newTestProvider(t, f), []string{"10.1.0.1", "10.99.99.99", "192.0.2.1"}, "match_count")
	if got := read(f); !reflect.DeepEqual(got, []string{addressesEntity}) {
		t.Errorf("match_count read %v", got)
	}

	f = ipWorld()
	resolveIPs(t, newTestProvider(t, f), []string{"10.1.0.1"}, provider.IsPrimaryIPColumn)
	devices := f.sentWith("dcim/devices", "filter[id]__in")
	if len(devices) != 1 || devices[0].query.Get("fields") != "id,primary_ip4_id,primary_ip6_id" || devices[0].query.Has("expand") {
		t.Errorf("is_primary_ip read devices as %v", devices)
	}
	for _, table := range []string{"dcim/racks", "ipam/vrfs", "ipam/prefixes", "virtualization/virtual-machines"} {
		if n := len(f.sentWith(table, "filter[id]__in")) + len(f.sentWith(table, "filter[prefix]__contains")); n != 0 {
			t.Errorf("is_primary_ip read %s", table)
		}
	}

	// The flag alone on a VM's addresses reads the VM interface and the VM,
	// which answer it, and no device.
	f = ipWorld()
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.4.0.1", "10.4.0.2", "2001:db8:4::1"}, provider.IsPrimaryIPColumn)
	noWarnings(t, res)
	for ip, primary := range map[string]bool{"10.4.0.1": true, "10.4.0.2": false, "2001:db8:4::1": true} {
		wantRow(t, res, ip, map[string]interface{}{provider.IsPrimaryIPColumn: primary})
	}
	if n := len(f.sentWith("dcim/devices", "filter[id]__in")); n != 0 {
		t.Errorf("is_primary_ip on VM addresses read %d device batches", n)
	}
}

// A reference NetBox keeps intact that names an object the replica does not
// hold is the replica still applying changes: the column is blank, and the
// result says so. An assignment to an object that does not exist is not one:
// NetBox leaves a generic assignment unchecked, and NetBox mode shows such an
// address with no owner and nothing to say.
func TestResolveIPs_MissingRelatedObjectsWarn(t *testing.T) {
	f := ipWorld()
	f.addFakeRows("ipam/ip-addresses",
		ipAddr(850, "10.85.0.1/24", 0, 0).with("tenant_id", float64(9)),
		ipAddr(851, "10.85.0.2/24", idInterface, 77))
	f.addRows("dcim/devices", ipDevice(5, "orphan", "active", 0, 0, 7, 0, 0, 0, 0))
	f.addRows("dcim/interfaces", ipIface(14, 5, "eth9", "", ""))
	f.addFakeRows("ipam/ip-addresses", ipAddr(852, "10.85.0.3/24", idInterface, 14))
	// An interface whose device, and a VM interface whose VM, the replica does
	// not hold.
	f.addRows("dcim/interfaces", ipIface(15, 6, "eth10", "", ""))
	f.addRows("virtualization/interfaces", map[string]interface{}{"id": float64(21), "name": "eth1", "description": "", "virtual_machine_id": float64(9)})
	f.addFakeRows("ipam/ip-addresses",
		ipAddr(853, "10.85.0.4/24", idInterface, 15),
		ipAddr(854, "10.85.0.5/24", idVMInterface, 21))
	ips := []string{"10.85.0.1", "10.85.0.2", "10.85.0.3", "10.85.0.4", "10.85.0.5", "10.1.0.1"}
	res := resolveIPs(t, newTestProvider(t, f), ips, "address_tenant", "interface_name", "device_name", "device_rack", "vm_name", provider.IsPrimaryIPColumn)
	wantRow(t, res, "10.85.0.1", map[string]interface{}{"address_tenant": nil})
	wantRow(t, res, "10.85.0.2", map[string]interface{}{"interface_name": nil, "device_name": nil})
	wantRow(t, res, "10.85.0.3", map[string]interface{}{"interface_name": "eth9", "device_name": "orphan", "device_rack": nil})
	wantRow(t, res, "10.85.0.4", map[string]interface{}{"interface_name": "eth10", "device_name": nil, provider.IsPrimaryIPColumn: nil})
	wantRow(t, res, "10.85.0.5", map[string]interface{}{"interface_name": "eth1", "vm_name": nil, provider.IsPrimaryIPColumn: nil})
	want := []string{"Related objects are missing for 4 of 6 rows: they reference objects this replica does not hold, so those columns are blank there."}
	if !reflect.DeepEqual(res.Warnings, want) {
		t.Errorf("warnings = %q, want %q", res.Warnings, want)
	}
	// Each counts for the columns it blanks, and only those: the missing VM
	// blanks is_primary_ip alone too, the missing device as well.
	for _, ip := range []string{"10.85.0.4", "10.85.0.5"} {
		res = resolveIPs(t, newTestProvider(t, f), []string{ip}, provider.IsPrimaryIPColumn)
		if len(res.Warnings) != 1 {
			t.Errorf("%s with is_primary_ip alone: warnings %q, want the missing owner counted", ip, res.Warnings)
		}
	}
	// Not shown, not counted.
	res = resolveIPs(t, newTestProvider(t, f), []string{"10.85.0.1", "10.85.0.3", "10.85.0.5"}, "device_name")
	noWarnings(t, res)
	res = resolveIPs(t, newTestProvider(t, f), []string{"10.85.0.4", "10.85.0.5"}, "interface_name")
	noWarnings(t, res)
}

// A read that fails fails the answer: a blank column from a failed read is
// indistinguishable from one with nothing to show.
func TestResolveIPs_FailedReadIsAnError(t *testing.T) {
	for _, table := range []string{addressesEntity, "dcim/devices", "ipam/prefixes", "ipam/vrfs"} {
		f := ipWorld()
		f.failing = map[string]int{table: 503}
		_, err := newTestProvider(t, f).ResolveIPs(context.Background(), []string{"10.1.0.1", "10.250.1.10", "10.10.10.5"}, allColumns(), 0)
		if err == nil {
			t.Errorf("%s failing: no error", table)
		}
	}
}

// A build without the host match cannot find an address's records as NetBox
// does, and one without containment cannot find a prefix: each is refused,
// saying so, and only when it is needed.
func TestResolveIPs_RefusesWhatTheBuildCannotAnswer(t *testing.T) {
	f := ipWorld()
	f.mu.Lock()
	e := f.schema.Entities["/v1/"+addressesEntity]
	for i, c := range e.Columns {
		if c.Name == "address" {
			e.Columns[i].Operators = slices.DeleteFunc(slices.Clone(c.Operators), func(op string) bool { return op == "host" })
		}
	}
	f.schema.Entities["/v1/"+addressesEntity] = e
	f.mu.Unlock()
	_, err := newTestProvider(t, f).ResolveIPs(context.Background(), []string{"10.1.0.1"}, nil, 0)
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "NetBox mode") || !strings.Contains(err.Error(), "v1.38") {
		t.Errorf("no host match: err = %v, want the refusal naming the build that added it", err)
	}

	f = ipWorld()
	f.mu.Lock()
	f.schema.FilterLimits = nil
	f.mu.Unlock()
	p := newTestProvider(t, f)
	if _, err := p.ResolveIPs(context.Background(), []string{"10.10.10.5"}, []string{"prefix_cidr"}, 0); !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "prefix_*") {
		t.Errorf("no containment: err = %v", err)
	}
	if _, err := p.ResolveIPs(context.Background(), []string{"10.1.0.1"}, []string{"prefix_cidr"}, 0); err != nil {
		t.Errorf("no host needs a prefix: %v", err)
	}
}

// One host with more records than a read returns: the row stands, its
// match_count is a floor, and the host is named.
func TestResolveIPs_RecordsPastTheCeilingWarn(t *testing.T) {
	f := ipWorld()
	rows := make([]map[string]interface{}, MaxLimit+1)
	for i := range rows {
		rows[i] = ipAddr(5000+i, "10.66.0.1/24", 0, 0)
	}
	f.addRows(addressesEntity, rows...)
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.66.0.1", "10.1.0.1"}, "match_count")
	wantRow(t, res, "10.66.0.1", map[string]interface{}{"match_count": float64(MaxLimit)})
	wantRow(t, res, "10.1.0.1", map[string]interface{}{"match_count": float64(1)})
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "The replica holds more than 10,000 address records for 1 IP (10.66.0.1)") {
		t.Errorf("warnings = %q", res.Warnings)
	}
}

// The scope is as old as its listing too, not only as the records read after
// it: an alert rule's Max data age applies to the whole answer.
func TestResolveScope_CarriesTheListingsAge(t *testing.T) {
	scope := []provider.Filter{{Field: "tenant", Value: "grafana-demo"}}
	isListing := func(r recordedRequest) bool {
		return r.entity == addressesEntity && r.query.Has("filter[tenant_id]__eq")
	}
	// Where the listing falls among the row requests, from a first run.
	f := ipWorld()
	if _, err := newTestProvider(t, f).ResolveScope(context.Background(), scope, allColumns(), 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	at := -1
	n := 0
	for _, r := range f.requests {
		if r.entity == "_meta/schema" {
			continue
		}
		if isListing(r) && at < 0 {
			at = n
		}
		n++
	}
	f.mu.Unlock()
	if at < 0 {
		t.Fatal("no listing request")
	}
	// Then only the listing is old.
	f = ipWorld()
	for i := range n + 10 {
		f.asOfs = append(f.asOfs, map[bool]string{true: "2026-09-22T13:00:00Z", false: "2026-09-22T14:10:00Z"}[i == at])
	}
	res, err := newTestProvider(t, f).ResolveScope(context.Background(), scope, allColumns(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC); res.DataAsOf == nil || !res.DataAsOf.Equal(want) {
		t.Errorf("DataAsOf = %v, want the listing's %v", res.DataAsOf, want)
	}
}

// The listing's own warnings are the scope's: a negated scope that could check
// only part of the addresses says so, and an alert rule must see it.
func TestResolveScope_CarriesTheListingsWarnings(t *testing.T) {
	f := newFakeService()
	f.addIPEnrichmentTables()
	// 10,001 addresses with a DNS name, then two without, past the 10,000 rows
	// the negated read checks.
	rows := make([]map[string]interface{}, 0, MaxLimit+3)
	for i := range MaxLimit + 1 {
		rows = append(rows, ipAddr(1+i, fmt.Sprintf("10.%d.%d.%d/24", 100+i/65536, i/256%256, i%256), 0, 0).with("dns_name", fmt.Sprintf("h%d.example.net", i)))
	}
	rows = append(rows, ipAddr(MaxLimit+10, "10.200.0.1/24", 0, 0), ipAddr(MaxLimit+11, "10.200.0.2/24", 0, 0))
	f.addRows(addressesEntity, rows...)
	res, err := newTestProvider(t, f).ResolveScope(context.Background(), []provider.Filter{{Field: "dns_name", Operator: "empty"}}, []string{"match_count"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "Only the first 10,000 rows") {
		t.Errorf("warnings = %q, want the listing's partial-check warning", res.Warnings)
	}
}

// Records that carry one unknown type are of one model, whichever it is, so
// they are ranked together: a pick that is the same under every reading of
// each unknown type is NetBox mode's, and when it is not, the record kept is
// one NetBox mode could pick — never one that loses under every reading.
func TestResolveIPs_UnknownTypeSettlesWhatItCan(t *testing.T) {
	captureLogs(t) // the learn logs the contradiction it rules 999 out on
	f := ipWorld()
	f.addFakeRows("ipam/ip-addresses",
		// Both on the unknown type: the active one wins as an interface and
		// as anything else.
		ipAddr(802, "10.8.0.4/24", 999, 5).with("status", "deprecated").with("description", "802"),
		ipAddr(803, "10.8.0.4/24", 999, 5).with("description", "803"),
		// The unassigned, deprecated one loses under every reading; the
		// other two each win under one.
		ipAddr(805, "10.8.0.5/24", 0, 0).with("status", "deprecated").with("description", "805"),
		ipAddr(830, "10.8.0.5/24", 999, 5).with("description", "830"),
		ipAddr(831, "10.8.0.5/24", idInterface, 12).with("description", "831"))
	res := resolveIPs(t, newTestProvider(t, f), []string{"10.8.0.4", "10.8.0.5"}, "match_count", "address_description")
	wantRow(t, res, "10.8.0.4", map[string]interface{}{"match_count": float64(2), "address_description": "803"})
	wantRow(t, res, "10.8.0.5", map[string]interface{}{"match_count": float64(3), "address_description": "830"})
	var undecided []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "matched several address records") {
			undecided = append(undecided, w)
		}
	}
	if len(undecided) != 1 || !strings.HasPrefix(undecided[0], "1 of 2 rows") {
		t.Errorf("undecided-pick warnings %q, want one, for 10.8.0.5 alone", undecided)
	}
}
