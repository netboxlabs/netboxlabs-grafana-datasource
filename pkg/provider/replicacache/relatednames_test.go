package replicacache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// relatedDevice is a dcim/devices row with its site, role and tenant; nil is
// NULL.
func relatedDevice(id int, name string, site, role, tenant interface{}) map[string]interface{} {
	return map[string]interface{}{"id": id, "name": name, "status": "active", "site_id": site, "role_id": role,
		"tenant_id": tenant, "platform_id": nil, "custom_field_data": `{}`}
}

// relatedFake is the devices fixture with the rows its related filters
// resolve through: three sites; two tenants, one of them slugged "null",
// which tenant=null must never look up; device roles nested as NetBox 4.3 and
// later store them (core > core-leaf > core-leaf-edge, and access); and five
// devices across them.
func relatedFake() *fakeService {
	f := newFakeService()
	f.addTreeRoles()
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": 1, "name": "AMS1", "slug": "ams1"},
		{"id": 2, "name": "NYC1", "slug": "nyc1"},
		{"id": 3, "name": "LON1", "slug": "lon1"},
	}
	f.entities["tenancy/tenants"] = []map[string]interface{}{
		{"id": 1, "name": "Acme", "slug": "acme"},
		{"id": 2, "name": "Null Corp", "slug": "null"},
	}
	f.entities["dcim/devices"] = []map[string]interface{}{
		relatedDevice(1, "a", 1, 5, 1),
		relatedDevice(2, "b", 1, 8, nil),
		relatedDevice(3, "c", 2, 9, 2),
		relatedDevice(4, "d", 2, 6, nil),
		relatedDevice(5, "e", nil, 6, 1),
	}
	return f
}

// relatedRows runs a query on objectType, ordered by id, and returns the ids
// of its rows.
func relatedRows(t *testing.T, p *Provider, objectType string, filters ...provider.Filter) []int {
	t.Helper()
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: objectType, Fields: []string{"id"}, Ordering: "id", Filters: filters})
	if err != nil {
		t.Fatalf("Query(%s, %+v): %v", objectType, filters, err)
	}
	return ids(res)
}

// relatedRefusal runs a query that must be refused, and returns the refusal.
func relatedRefusal(t *testing.T, p *Provider, objectType string, filters ...provider.Filter) *UnsupportedFilterError {
	t.Helper()
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: objectType, Filters: filters})
	var u *UnsupportedFilterError
	if !errors.As(err, &u) {
		rows := 0
		if res != nil {
			rows = len(res.Rows)
		}
		t.Fatalf("Query(%s, %+v) = %d rows, %v; want an UnsupportedFilterError", objectType, filters, rows, err)
	}
	return u
}

// noRequestsFor fails when anything was asked of an entity since the last
// forget.
func noRequestsFor(t *testing.T, f *fakeService, entities ...string) {
	t.Helper()
	for _, e := range entities {
		if n := f.countRequestsFor(e); n != 0 {
			t.Errorf("%d requests sent to %s; want none", n, e)
		}
	}
}

// NetBox's site filter compares the site's slug, its to_field_name, so
// site=ams1 finds the devices at AMS1. It is sent as the id the slug names, on
// site_id: no expansion, and nothing compared by name.
func TestRelated_SiteMatchesTheSlug(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "site", Value: "ams1"}); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("site=ams1: rows %v, want [1 2]", got)
	}
	read := f.sentWith("dcim/sites", "filter[slug]__in")
	if len(read) != 1 || read[0].query.Get("filter[slug]__in") != "ams1" || read[0].query.Get("fields") != "id,slug" {
		t.Errorf("sites read %v, want one with filter[slug]__in=ams1 and fields=id,slug", read)
	}
	req := f.sentWith("dcim/devices", "filter[site_id]__eq")
	if len(req) != 1 || req[0].query.Get("filter[site_id]__eq") != "1" || req[0].query.Has("expand") {
		t.Errorf("devices request %v, want filter[site_id]__eq=1 and no expansion", req)
	}
}

// A value that names no site is NetBox's 400, in NetBox's words — an alert
// rule sees an error, not an empty answer — and so is a site's name, which is
// not its slug (NetBox 4.4.10: site=AMS1 → "Select a valid choice. AMS1 is not
// one of the available choices."). The first unknown in the order written is
// named; NetBox names one in the order of a Python set. Nothing is asked of
// the devices.
func TestRelated_UnknownSlugRefusedWithNetBoxsMessage(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	for _, c := range []struct {
		f     provider.Filter
		named string
	}{
		{provider.Filter{Field: "site", Value: "ams1,ams9"}, "ams9"},
		{provider.Filter{Field: "site", Value: "AMS1"}, "AMS1"},
		{provider.Filter{Field: "site", Value: "nyc9, ams9"}, "nyc9"},
		{provider.Filter{Field: "site", Operator: "n", Value: "ams9"}, "ams9"},
	} {
		u := relatedRefusal(t, p, "dcim/devices", c.f)
		wantReason(t, u, fmt.Sprintf("Select a valid choice. %s is not one of the available choices. NetBox mode answers the same; site takes the slug of an object in dcim/sites", c.named))
		if u.Field != "site" || u.Operator != c.f.Operator {
			t.Errorf("%+v: the refusal names %s %q, want the filter as written", c.f, u.Field, u.Operator)
		}
	}
	noRequestsFor(t, f, "dcim/devices")
}

// An id that names no site is the same 400.
func TestRelated_UnknownIDRefused(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	for _, filter := range []provider.Filter{{Field: "site_id", Value: "1,99"}, {Field: "site_id", Operator: "n", Value: "99"}} {
		wantReason(t, relatedRefusal(t, p, "dcim/devices", filter), "Select a valid choice. 99 is not one of the available choices.")
	}
	noRequestsFor(t, f, "dcim/devices")
}

// An id Python's int() cannot read is NetBox's other message, and it wins over
// an unknown id whatever the order (NetBox 4.4.10: site_id=999&site_id=abc →
// "“abc” is not a valid value."): NetBox looks every value up before it asks
// which were found. Nothing is read to say so.
func TestRelated_MalformedIDWinsOverUnknown(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	for in, named := range map[string]string{"999,abc": "abc", "abc,999": "abc", "01,abc": "abc", "1.0": "1.0", "1e0": "1e0"} {
		u := relatedRefusal(t, p, "dcim/devices", provider.Filter{Field: "site_id", Value: in})
		wantReason(t, u, fmt.Sprintf("“%s” is not a valid value. NetBox mode answers the same; site_id takes the id of an object in dcim/sites", named))
	}
	noRequestsFor(t, f, "dcim/sites", "dcim/devices")
}

// int() reads 01, +1 and 1_0 (as 1, 1 and 10), so NetBox looks them up, then
// compares each id's own text with the value as written, which they are not:
// an invalid choice (NetBox 4.4.10: site_id=01 and site_id=1_0). The text
// decides, so nothing is read to say so.
func TestRelated_NonCanonicalIDIsAnInvalidChoice(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	for _, v := range []string{"01", "+1", "1_0"} {
		u := relatedRefusal(t, p, "dcim/devices", provider.Filter{Field: "site_id", Value: v})
		wantReason(t, u, "Select a valid choice. "+v+" is not one of the available choices.")
	}
	noRequestsFor(t, f, "dcim/sites", "dcim/devices")
}

// NetBox's role filter on devices is a tree filter: a role stands for itself
// and every role below it (NetBox 4.4.10: role=parity-role-parent finds the 3
// devices on its child role). The role is read with its place in the tree, and
// the roles below it as the range of that tree its lft and rght enclose.
func TestRelated_RoleIncludesDescendants(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role", Value: "core"}); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("role=core: rows %v, want [1 2 3], the devices on core and on the two roles below it", got)
	}
	read := f.sentWith("dcim/device-roles", "filter[slug]__in")
	if len(read) != 1 || read[0].query.Get("fields") != "id,slug,tree_id,lft,rght" {
		t.Errorf("roles read %v, want one projected to id,slug,tree_id,lft,rght", read)
	}
	below := f.sentWith("dcim/device-roles", "filter[tree_id]__eq")
	if len(below) != 1 {
		t.Fatalf("descendant reads %v, want one", below)
	}
	if q := below[0].query; q.Get("filter[tree_id]__eq") != "1" || q.Get("filter[lft]__gt") != "0" || q.Get("filter[lft]__lt") != "7" || q.Get("fields") != "id" {
		t.Errorf("descendant read %v, want tree 1 with lft in (0, 7), projected to id", q)
	}
	req := f.sentWith("dcim/devices", "filter[role_id]__in")
	if len(req) != 1 || req[0].query.Get("filter[role_id]__in") != "5,8,9" {
		t.Errorf("devices request %v, want filter[role_id]__in=5,8,9", req)
	}
}

// role_id is a tree filter too, the id form of the same one. A role with
// nothing below it (rght = lft + 1, as django-mptt keeps a leaf) has no
// descendants to read.
func TestRelated_RoleIDIncludesDescendants(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role_id", Value: "8"}); !slices.Equal(got, []int{2, 3}) {
		t.Errorf("role_id=8: rows %v, want [2 3]", got)
	}
	f.forget()
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role_id", Value: "6"}); !slices.Equal(got, []int{4, 5}) {
		t.Errorf("role_id=6: rows %v, want [4 5]", got)
	}
	if below := f.sentWith("dcim/device-roles", "filter[tree_id]__eq"); len(below) != 0 {
		t.Errorf("a leaf's descendants were read: %v", below)
	}
	// A node inside another's range is covered by that one's read.
	f.forget()
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role", Value: "core,core-leaf"}); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("role=core,core-leaf: rows %v, want [1 2 3]", got)
	}
	if below := f.sentWith("dcim/device-roles", "filter[tree_id]__eq"); len(below) != 1 {
		t.Errorf("descendant reads %v, want core's alone", below)
	}
}

// A role's parent filter is a plain relation, so it is the role's direct
// children alone; ancestor is NetBox's tree filter on the same column, and
// takes every role below.
func TestRelated_ParentOnRolesIsDirectChildrenOnly(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/device-roles", provider.Filter{Field: "parent", Value: "core"}); !slices.Equal(got, []int{8}) {
		t.Errorf("parent=core: roles %v, want [8]", got)
	}
	if below := f.sentWith("dcim/device-roles", "filter[tree_id]__eq"); len(below) != 0 {
		t.Errorf("parent read descendants: %v", below)
	}
	if got := relatedRows(t, p, "dcim/device-roles", provider.Filter{Field: "ancestor", Value: "core"}); !slices.Equal(got, []int{8, 9}) {
		t.Errorf("ancestor=core: roles %v, want [8 9]", got)
	}
}

// A target stored flat — device roles before NetBox 4.3, platforms before 4.6
// — has no tree to read: a tree filter on it is the nodes named.
func TestRelated_FlatTargetResolvesWithoutDescendants(t *testing.T) {
	f := relatedFake()
	f.schema.Entities["/v1/dcim/device-roles"] = devicesSchema().Entities["/v1/dcim/device-roles"]
	f.entities["dcim/device-roles"] = []map[string]interface{}{
		{"id": 5, "name": "Core", "slug": "core"}, {"id": 8, "name": "Core Leaf", "slug": "core-leaf"},
	}
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role", Value: "core"}); !slices.Equal(got, []int{1}) {
		t.Errorf("role=core: rows %v, want [1]", got)
	}
	read := f.sentWith("dcim/device-roles", "filter[slug]__in")
	if len(read) != 1 || read[0].query.Get("fields") != "id,slug" {
		t.Errorf("roles read %v, want one projected to id,slug", read)
	}
	if below := f.sentWith("dcim/device-roles", "filter[tree_id]__eq"); len(below) != 0 {
		t.Errorf("descendants read on a flat table: %v", below)
	}
}

// Each node a tree filter matches can be one more read, so a filter matching
// more than 100 is refused, naming how many, before any is read.
func TestRelated_TreeNodeCap(t *testing.T) {
	f := relatedFake()
	var slugs []string
	for i := 1; i <= 101; i++ {
		slugs = append(slugs, fmt.Sprintf("r%d", i))
		f.entities["dcim/device-roles"] = append(f.entities["dcim/device-roles"], treeNode(100+i, fmt.Sprintf("R%d", i), slugs[i-1], nil, 100+i, 1, 4, 0))
	}
	p := newTestProvider(t, f)
	u := relatedRefusal(t, p, "dcim/devices", provider.Filter{Field: "role", Value: strings.Join(slugs, ",")})
	wantReason(t, u, "role matches 101 objects in dcim/device-roles, and this datasource reads the descendants of at most 100 for one filter")
	if below := f.sentWith("dcim/device-roles", "filter[tree_id]__eq"); len(below) != 0 {
		t.Errorf("%d descendant reads before the refusal", len(below))
	}
	noRequestsFor(t, f, "dcim/devices")

	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role", Value: strings.Join(slugs[:100], ",")}); len(got) != 0 {
		t.Errorf("100 roles no device has: rows %v, want none", got)
	}
}

// A component's site filter reads the site its device's site cached on the
// component's row, _site_id (NetBox's interface site filter is on _site).
func TestRelated_InterfacesSiteUsesTheCachedColumn(t *testing.T) {
	f := relatedFake()
	f.addInterfaces()
	f.entities["dcim/interfaces"] = []map[string]interface{}{
		{"id": 1, "name": "eth0", "device_id": 1, "_site_id": 1, "primary_mac_address_id": nil},
		{"id": 2, "name": "eth0", "device_id": 3, "_site_id": 2, "primary_mac_address_id": nil},
	}
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/interfaces", provider.Filter{Field: "site", Value: "ams1"}); !slices.Equal(got, []int{1}) {
		t.Errorf("site=ams1: interfaces %v, want [1]", got)
	}
	if req := f.sentWith("dcim/interfaces", "filter[_site_id]__eq"); len(req) != 1 || req[0].query.Get("filter[_site_id]__eq") != "1" {
		t.Errorf("interfaces request %v, want filter[_site_id]__eq=1", req)
	}
}

// An interface's device filter compares the device's name, as written —
// case and all — and every device of that name is meant: a name repeats
// across sites.
func TestRelated_InterfaceDeviceMatchesTheName(t *testing.T) {
	f := relatedFake()
	f.addInterfaces()
	f.entities["dcim/devices"] = append(f.entities["dcim/devices"], relatedDevice(6, "a", 3, 6, nil))
	f.entities["dcim/interfaces"] = []map[string]interface{}{
		{"id": 1, "name": "eth0", "device_id": 1, "_site_id": 1, "primary_mac_address_id": nil},
		{"id": 2, "name": "eth0", "device_id": 3, "_site_id": 2, "primary_mac_address_id": nil},
		{"id": 3, "name": "eth1", "device_id": 6, "_site_id": 3, "primary_mac_address_id": nil},
	}
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/interfaces", provider.Filter{Field: "device", Value: "a"}); !slices.Equal(got, []int{1, 3}) {
		t.Errorf("device=a: interfaces %v, want [1 3], on both devices named a", got)
	}
	if read := f.sentWith("dcim/devices", "filter[name]__in"); len(read) != 1 || read[0].query.Get("fields") != "id,name" {
		t.Errorf("devices read %v, want one with filter[name]__in projected to id,name", read)
	}
	wantReason(t, relatedRefusal(t, p, "dcim/interfaces", provider.Filter{Field: "device", Value: "A"}),
		"Select a valid choice. A is not one of the available choices.")
}

// prefixesWithVRFs is ipam/prefixes with a VRF each, or none.
func prefixesWithVRFs(vrfsFed bool) *fakeService {
	f := newFakeService()
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR")
	f.addVRFs("ipam/prefixes", vrfsFed)
	f.entities["ipam/prefixes"] = []map[string]interface{}{
		{"id": 1, "prefix": "10.0.0.0/8", "vrf_id": 1},
		{"id": 2, "prefix": "10.1.0.0/16", "vrf_id": nil},
		{"id": 3, "prefix": "10.2.0.0/16", "vrf_id": 2},
	}
	return f
}

// NetBox's vrf filter compares the RD, which the replica stores and never
// joins in: the catalogue's vrf reference is the name. A VRF's name is not its
// RD, and is an invalid choice.
func TestRelated_VRFMatchesTheRD(t *testing.T) {
	f := prefixesWithVRFs(true)
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "ipam/prefixes", provider.Filter{Field: "vrf", Value: "65000:1"}); !slices.Equal(got, []int{1}) {
		t.Errorf("vrf=65000:1: prefixes %v, want [1]", got)
	}
	if read := f.sentWith("ipam/vrfs", "filter[rd]__in"); len(read) != 1 || read[0].query.Get("filter[rd]__in") != "65000:1" || read[0].query.Get("fields") != "id,rd" {
		t.Errorf("vrfs read %v, want filter[rd]__in=65000:1 projected to id,rd", read)
	}
	if got := relatedRows(t, p, "ipam/prefixes", provider.Filter{Field: "vrf_id", Value: "2"}); !slices.Equal(got, []int{3}) {
		t.Errorf("vrf_id=2: prefixes %v, want [3]", got)
	}
	wantReason(t, relatedRefusal(t, p, "ipam/prefixes", provider.Filter{Field: "vrf", Value: "Blue"}),
		"Select a valid choice. Blue is not one of the available choices. NetBox mode answers the same; vrf takes the rd of an object in ipam/vrfs")
}

// withPrimaryIPs gives the devices a primary_ip4 as the catalogue references
// it, and two addresses: a single host, which the replica stores without its
// mask, and an address in a /21.
func withPrimaryIPs(f *fakeService) {
	f.addAddressEntity(true)
	f.entities["ipam/ip-addresses"] = []map[string]interface{}{{"id": 11, "address": "10.0.0.11"}, {"id": 12, "address": "10.0.0.12/21"}}
	f.addColumns("dcim/devices", "primary_ip4_id:BIGINT")
	f.addReference("dcim/devices", "primary_ip4_id", "ipam/ip-addresses", "primary_ip4", "address")
	f.entities["dcim/devices"][0]["primary_ip4_id"] = 11
	f.entities["dcim/devices"][1]["primary_ip4_id"] = 12
}

// A device's primary_ip4 compares the address as NetBox writes it, its mask
// always (NetBox 4.4.10: primary_ip4=10.10.10.14/24 finds the device;
// 10.10.10.14, 10.10.10.14/255.255.255.0 and /024 are invalid choices). The
// replica stores a single host bare, so the read sends it bare and the mask is
// put back before comparing.
func TestRelated_PrimaryIPNeedsItsCanonicalText(t *testing.T) {
	f := relatedFake()
	withPrimaryIPs(f)
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "primary_ip4", Value: "10.0.0.11/32"}); !slices.Equal(got, []int{1}) {
		t.Errorf("primary_ip4=10.0.0.11/32: rows %v, want [1]", got)
	}
	if read := f.sentWith("ipam/ip-addresses", "filter[address]__in"); len(read) != 1 || read[0].query.Get("filter[address]__in") != "10.0.0.11" {
		t.Errorf("addresses read %v, want filter[address]__in=10.0.0.11, as the replica stores it", read)
	}
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "primary_ip4", Value: "10.0.0.12/21"}); !slices.Equal(got, []int{2}) {
		t.Errorf("primary_ip4=10.0.0.12/21: rows %v, want [2]", got)
	}
	f.forget()
	for _, v := range []string{"10.0.0.11", "10.0.0.12/255.255.248.0", "10.0.0.12/021"} {
		wantReason(t, relatedRefusal(t, p, "dcim/devices", provider.Filter{Field: "primary_ip4", Value: v}),
			"Select a valid choice. "+v+" is not one of the available choices. NetBox mode answers the same; primary_ip4 takes the address of an object in ipam/ip-addresses as NetBox writes it, with its mask (10.0.0.1/32)")
	}
	noRequestsFor(t, f, "ipam/ip-addresses", "dcim/devices")
}

// A value netaddr cannot read as a network is NetBox's own message, whether
// the address or the mask is wrong (NetBox 4.4.10: garbage, 010.10.10.14/24,
// 10.10.10.14/33, /abc), and it wins over an invalid choice.
func TestRelated_PrimaryIPMalformedHasNetBoxsMessage(t *testing.T) {
	f := relatedFake()
	withPrimaryIPs(f)
	p := newTestProvider(t, f)
	for in, named := range map[string]string{
		"garbage": "garbage", "010.10.10.14/24": "010.10.10.14/24", "10.10.10.14/33": "10.10.10.14/33", "/abc": "/abc",
		"10.0.0.11, garbage": "garbage",
	} {
		wantReason(t, relatedRefusal(t, p, "dcim/devices", provider.Filter{Field: "primary_ip4", Value: in}), "Invalid IP address format: "+named+". NetBox mode answers the same")
	}
	noRequestsFor(t, f, "ipam/ip-addresses", "dcim/devices")
}

// interfacesWithMACs is the interfaces fixture with one MAC address, stored as
// the replica stores PostgreSQL's macaddr: lowercase.
func interfacesWithMACs() *fakeService {
	f := relatedFake()
	f.addInterfaces()
	f.entities["dcim/mac-addresses"] = []map[string]interface{}{{"id": 1, "mac_address": "58:56:9f:d1:e3:00"}}
	f.entities["dcim/interfaces"] = []map[string]interface{}{
		{"id": 1, "name": "eth0", "device_id": 1, "_site_id": 1, "primary_mac_address_id": 1},
		{"id": 2, "name": "eth1", "device_id": 1, "_site_id": 1, "primary_mac_address_id": nil},
	}
	return f
}

// An interface's primary_mac_address compares the address as NetBox writes
// it, in capitals with colons; any other spelling is an invalid choice. The
// replica stores it lowercase, so the read sends it so.
func TestRelated_PrimaryMACIsUppercase(t *testing.T) {
	f := interfacesWithMACs()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/interfaces", provider.Filter{Field: "primary_mac_address", Value: "58:56:9F:D1:E3:00"}); !slices.Equal(got, []int{1}) {
		t.Errorf("primary_mac_address=58:56:9F:D1:E3:00: interfaces %v, want [1]", got)
	}
	if read := f.sentWith("dcim/mac-addresses", "filter[mac_address]__in"); len(read) != 1 || read[0].query.Get("filter[mac_address]__in") != "58:56:9f:d1:e3:00" {
		t.Errorf("MAC read %v, want filter[mac_address]__in=58:56:9f:d1:e3:00", read)
	}
	f.forget()
	for _, v := range []string{"58:56:9f:d1:e3:00", "58-56-9F-D1-E3-00", "5856.9fd1.e300"} {
		wantReason(t, relatedRefusal(t, p, "dcim/interfaces", provider.Filter{Field: "primary_mac_address", Value: v}),
			"Select a valid choice. "+v+" is not one of the available choices. NetBox mode answers the same; primary_mac_address takes the MAC address of an object in dcim/mac-addresses as NetBox writes it, in capitals (58:56:9F:D1:E3:00)")
	}
	noRequestsFor(t, f, "dcim/mac-addresses", "dcim/interfaces")
}

// A value that is no MAC address is NetBox 4.4's message, which writes the
// value with its spaces removed, as NetBox read it (4.6 says “garbage” is not
// a valid value. — ours is 4.4's).
func TestRelated_PrimaryMACMalformed(t *testing.T) {
	f := interfacesWithMACs()
	p := newTestProvider(t, f)
	for _, v := range []string{"garbage", "gar bage", "58:56:9F:D1:E3:00:00"} {
		wantReason(t, relatedRefusal(t, p, "dcim/interfaces", provider.Filter{Field: "primary_mac_address", Value: v}),
			"Invalid MAC address format: "+strings.ReplaceAll(v, " ", "")+". NetBox mode answers the same")
	}
	noRequestsFor(t, f, "dcim/mac-addresses", "dcim/interfaces")
}

// The literal null is IS NULL on the foreign key, IS NOT NULL negated — the
// tenant slugged "null" is never looked up — with no read of the target, so it
// works where the target has no data (platforms here; NetBox 4.4.10:
// platform_id=null → every device with none).
func TestRelated_NullMeansIsNull(t *testing.T) {
	f := relatedFake()
	f.entities["dcim/devices"][4]["platform_id"] = 7
	p := newTestProvider(t, f)
	for _, c := range []struct {
		f    provider.Filter
		want []int
	}{
		{provider.Filter{Field: "tenant", Value: "null"}, []int{2, 4}},
		{provider.Filter{Field: "tenant", Operator: "n", Value: "null"}, []int{1, 3, 5}},
		{provider.Filter{Field: "tenant_id", Value: "null"}, []int{2, 4}},
		{provider.Filter{Field: "platform", Value: "null"}, []int{1, 2, 3, 4}},
		{provider.Filter{Field: "platform_id", Value: "null"}, []int{1, 2, 3, 4}},
		{provider.Filter{Field: "platform_id", Operator: "n", Value: "null"}, []int{5}},
	} {
		if got := relatedRows(t, p, "dcim/devices", c.f); !slices.Equal(got, c.want) {
			t.Errorf("%+v: rows %v, want %v", c.f, got, c.want)
		}
	}
	if req := f.sentWith("dcim/devices", "filter[tenant_id]__isnull"); len(req) != 3 || req[0].query.Get("filter[tenant_id]__isnull") != "true" || req[1].query.Get("filter[tenant_id]__isnull") != "false" {
		t.Errorf("tenant requests %v, want filter[tenant_id]__isnull=true, then false", req)
	}
	noRequestsFor(t, f, "tenancy/tenants", "dcim/platforms")
}

// Cable-terminations' cable is a ModelChoiceFilter whose form has no null
// choice, so null is the value it cannot read: NetBox's "That choice" 400
// (NetBox 4.4.10: cable=null), with no read.
func TestRelated_NullRefusedWhereNetBoxRefusesIt(t *testing.T) {
	f := newFakeService()
	f.addCableTerminations()
	p := newTestProvider(t, f)
	for _, op := range []string{"", "n"} {
		wantReason(t, relatedRefusal(t, p, "dcim/cable-terminations", provider.Filter{Field: "cable", Operator: op, Value: "null"}),
			"Select a valid choice. That choice is not one of the available choices.")
	}
	noRequestsFor(t, f, "dcim/cables", "dcim/cable-terminations")
}

// null beside values on a negation is IS NOT NULL and not the values (NetBox
// 4.4.10: site__n=null&site__n=ams1 → the 8 devices at the other sites).
func TestRelated_NullWithValuesNegated(t *testing.T) {
	p := newTestProvider(t, relatedFake())
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "site", Operator: "n", Value: "null,ams1"}); !slices.Equal(got, []int{3, 4}) {
		t.Errorf("site__n=null,ams1: rows %v, want [3 4]", got)
	}
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "site", Operator: "n", Value: "null"},
		provider.Filter{Field: "site", Operator: "n", Value: "ams1"}); !slices.Equal(got, []int{3, 4}) {
		t.Errorf("site__n=null&site__n=ams1: rows %v, want [3 4]", got)
	}
}

// On = NetBox returns rows matching either (site=null&site=ams1 → AMS1's
// devices and those with no site), which the replica's filters, ANDed, cannot
// say. Two rows of one name are one parameter, so they are refused too.
func TestRelated_NullWithValuesExactRefused(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	for _, filters := range [][]provider.Filter{
		{{Field: "site", Value: "null,ams1"}},
		{{Field: "site", Value: "null"}, {Field: "site", Value: "ams1"}},
	} {
		wantReason(t, relatedRefusal(t, p, "dcim/devices", filters...),
			"NetBox returns rows matching either null or the values, and this replica cannot combine the two")
	}
	noRequestsFor(t, f, "dcim/sites", "dcim/devices")
}

// A row of null alone adds its IS NULL and nothing else: beside another
// name's exclusion it makes no positive set (NetBox 4.4.10:
// tenant=null&tenant__n=grafana-demo → the 2 devices with no tenant).
func TestRelated_NullBesideAnotherNamesExclusion(t *testing.T) {
	p := newTestProvider(t, relatedFake())
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "tenant", Value: "null"},
		provider.Filter{Field: "tenant", Operator: "n", Value: "acme"}); !slices.Equal(got, []int{2, 4}) {
		t.Errorf("tenant=null&tenant__n=acme: rows %v, want [2 4]", got)
	}
}

// Rows of different names are different parameters, which NetBox ANDs
// (NetBox 4.4.10: site=ams1&site_id=2 → 0). Two that cannot both hold send
// nothing.
func TestRelated_NameAndIDAreANDed(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "site", Value: "ams1"}, provider.Filter{Field: "site_id", Value: "2"}); len(got) != 0 {
		t.Errorf("site=ams1&site_id=2: rows %v, want none", got)
	}
	noRequestsFor(t, f, "dcim/devices")
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "site", Value: "ams1,nyc1"}, provider.Filter{Field: "site_id", Value: "1"}); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("site=ams1,nyc1&site_id=1: rows %v, want [1 2]", got)
	}
}

// Rows of one name are one parameter, its values ORed (NetBox 4.4.10:
// site=ams1&site=nyc1 → 11, the devices at both).
func TestRelated_SameNameRowsAreORed(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "site", Value: "ams1"}, provider.Filter{Field: "site", Value: "nyc1"}); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Errorf("site=ams1&site=nyc1: rows %v, want [1 2 3 4]", got)
	}
	if req := f.sentWith("dcim/devices", "filter[site_id]__in"); len(req) != 1 || req[0].query.Get("filter[site_id]__in") != "1,2" {
		t.Errorf("devices request %v, want filter[site_id]__in=1,2", req)
	}
}

// A negated tree filter drops the node and every node below it, and keeps the
// rows with none, as Django's exclude() keeps them: a prefix's location__n=dc
// drops the prefixes in dc and in its room.
func TestRelated_NegatedSlugExcludesTheSubtree(t *testing.T) {
	f := newFakeService()
	f.addLocations()
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR", "_location_id:BIGINT")
	f.entities["ipam/prefixes"] = []map[string]interface{}{
		{"id": 1, "prefix": "10.0.0.0/8", "_location_id": 1},
		{"id": 2, "prefix": "10.1.0.0/16", "_location_id": 2},
		{"id": 3, "prefix": "10.2.0.0/16", "_location_id": 3},
		{"id": 4, "prefix": "10.3.0.0/16", "_location_id": nil},
	}
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "ipam/prefixes", provider.Filter{Field: "location", Operator: "n", Value: "dc"}); !slices.Equal(got, []int{3, 4}) {
		t.Errorf("location__n=dc: prefixes %v, want [3 4]", got)
	}
	if got := relatedRows(t, p, "ipam/prefixes", provider.Filter{Field: "location", Value: "dc"}); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("location=dc: prefixes %v, want [1 2]", got)
	}
}

// What a negated name excludes comes off what the others select: role=core
// with role__n=core-leaf is core alone (NetBox 4.4.10:
// role=parity-role-parent&role__n=parity-role-child → its devices, all on the
// child, excluded). Nothing left is no row, and no request.
func TestRelated_PositiveMinusNegative(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role", Value: "core"}, provider.Filter{Field: "role", Operator: "n", Value: "core-leaf"}); !slices.Equal(got, []int{1}) {
		t.Errorf("role=core&role__n=core-leaf: rows %v, want [1]", got)
	}
	f.forget()
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "role", Value: "core-leaf"}, provider.Filter{Field: "role_id", Operator: "n", Value: "5"}); len(got) != 0 {
		t.Errorf("role=core-leaf&role_id__n=5: rows %v, want none", got)
	}
	noRequestsFor(t, f, "dcim/devices")
}

// Cable-terminations' cable is single-valued (a ModelChoiceFilter): the last
// value given, across rows, as NetBox reads it (cable=999&cable=1 → cable 1's
// terminations), and its id read as int() reads it, with no check of the
// text (NetBox 4.4.10: cable=01 is cable 1).
func TestRelated_ModelChoiceTakesTheLastValue(t *testing.T) {
	f := newFakeService()
	f.addCableTerminations()
	p := newTestProvider(t, f)
	for _, c := range []struct {
		filters []provider.Filter
		want    []int
	}{
		{[]provider.Filter{{Field: "cable", Value: "7"}, {Field: "cable", Value: "9"}}, []int{2, 3}},
		{[]provider.Filter{{Field: "cable", Value: "9, 7"}}, []int{1}},
		{[]provider.Filter{{Field: "cable", Value: "07"}}, []int{1}},
		{[]provider.Filter{{Field: "cable", Operator: "n", Value: "7"}}, []int{2, 3}},
	} {
		if got := relatedRows(t, p, "dcim/cable-terminations", c.filters...); !slices.Equal(got, c.want) {
			t.Errorf("%+v: terminations %v, want %v", c.filters, got, c.want)
		}
	}
	if read := f.sentWith("dcim/cables", "filter[id]__in"); len(read) == 0 || read[2].query.Get("filter[id]__in") != "7" {
		t.Errorf("cables read %v, want cable=07 read as 7", read)
	}
}

// A cable that does not exist is NetBox's 400 for a ModelChoiceFilter, which
// names no value (NetBox 4.4.10: cable=99999).
func TestRelated_ModelChoiceUnknownHasNetBoxsMessage(t *testing.T) {
	f := newFakeService()
	f.addCableTerminations()
	p := newTestProvider(t, f)
	wantReason(t, relatedRefusal(t, p, "dcim/cable-terminations", provider.Filter{Field: "cable", Value: "99"}),
		"Select a valid choice. That choice is not one of the available choices. NetBox mode answers the same; cable takes the id of an object in dcim/cables")
	noRequestsFor(t, f, "dcim/cable-terminations")
}

// A value int() cannot read is the same message, with nothing read (NetBox
// 4.4.10: cable=1.0, cable=abc).
func TestRelated_ModelChoiceMalformedHasThatChoiceMessage(t *testing.T) {
	f := newFakeService()
	f.addCableTerminations()
	p := newTestProvider(t, f)
	for _, v := range []string{"1.0", "abc"} {
		wantReason(t, relatedRefusal(t, p, "dcim/cable-terminations", provider.Filter{Field: "cable", Value: v}),
			"Select a valid choice. That choice is not one of the available choices.")
	}
	noRequestsFor(t, f, "dcim/cables", "dcim/cable-terminations")
}

// A target the replica has received no data for cannot be checked as NetBox
// checks it, so a value on it is refused, naming the target; null needs no
// check and still works.
func TestRelated_VRFTableUnfed(t *testing.T) {
	f := prefixesWithVRFs(false)
	p := newTestProvider(t, f)
	for _, filter := range []provider.Filter{{Field: "vrf", Value: "65000:1"}, {Field: "vrf_id", Operator: "n", Value: "1"}} {
		u := relatedRefusal(t, p, "ipam/prefixes", filter)
		wantReason(t, u, filter.Field+" cannot be checked on this replica: ipam/vrfs has received no data, so its values cannot be matched as NetBox matches them")
	}
	noRequestsFor(t, f, "ipam/vrfs", "ipam/prefixes")
	if got := relatedRows(t, p, "ipam/prefixes", provider.Filter{Field: "vrf", Value: "null"}); !slices.Equal(got, []int{2}) {
		t.Errorf("vrf=null: prefixes %v, want [2]", got)
	}
}

// A target the replica does not serve at all — a device's config template —
// is refused, naming it.
func TestRelated_TargetNotInCatalogue(t *testing.T) {
	f := relatedFake()
	f.addColumns("dcim/devices", "config_template_id:BIGINT")
	p := newTestProvider(t, f)
	wantReason(t, relatedRefusal(t, p, "dcim/devices", provider.Filter{Field: "config_template_id", Value: "1"}),
		"config_template_id cannot be checked on this replica: it does not serve extras.configtemplate")
	noRequestsFor(t, f, "dcim/devices")
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "config_template_id", Value: "null"}); !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
		t.Errorf("config_template_id=null: rows %v, want all five", got)
	}
}

// The catalogue can be minutes old: a target it has as fed may answer that it
// has received no data, which is the same refusal.
func TestRelated_TargetAnswersNotReplicated(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	if got := relatedRows(t, p, "dcim/devices", provider.Filter{Field: "site", Value: "ams1"}); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("site=ams1 before: rows %v, want [1 2]", got)
	}
	f.setIngested("dcim/sites", false)
	wantReason(t, relatedRefusal(t, p, "dcim/devices", provider.Filter{Field: "site", Value: "nyc1"}),
		"site cannot be checked on this replica: dcim/sites has received no data, so its values cannot be matched as NetBox matches them")
}

// Every refusal names the filter as the user wrote it, never the column it is
// sent on: an interface's site is refused as site, not _site_id, whether
// NetBox's form refuses it or a step after the resolution does.
func TestRelated_ErrorsNameTheUsersField(t *testing.T) {
	f := relatedFake()
	f.addInterfaces()
	f.entities["dcim/interfaces"] = []map[string]interface{}{}
	p := newTestProvider(t, f)
	for _, c := range []struct {
		objectType string
		filter     provider.Filter
		reason     string
	}{
		{"dcim/interfaces", provider.Filter{Field: "site", Value: "ams9"}, "Select a valid choice. ams9 is not one of the available choices."},
		{"dcim/devices", provider.Filter{Field: "site_id", Operator: "n", Value: "abc"}, "“abc” is not a valid value."},
	} {
		u := relatedRefusal(t, p, c.objectType, c.filter)
		wantReason(t, u, c.reason)
		if u.Field != c.filter.Field || u.Operator != c.filter.Operator || strings.Contains(u.Error(), "_site_id") {
			t.Errorf("%+v: %v; want it to name %s %q as written", c.filter, u, c.filter.Field, c.filter.Operator)
		}
	}
	// On an entity with no data yet a negation is refused after the
	// resolution, on the column it is sent on; it is named as written.
	f.setIngested("dcim/interfaces", false)
	f.forget()
	p = newTestProvider(t, f)
	u := relatedRefusal(t, p, "dcim/interfaces", provider.Filter{Field: "site", Operator: "n", Value: "ams1"})
	if len(f.sentWith("dcim/sites", "filter[slug]__in")) != 1 {
		t.Errorf("site__n on unfed interfaces was refused before its slug was resolved: %v", u)
	}
	if u.Field != "site" || strings.Contains(u.Error(), "_site_id") || !strings.Contains(u.Reason, "negated filter") {
		t.Errorf("site__n on unfed interfaces: %v; want the negation's refusal naming site", u)
	}
}

// A count goes through the same resolution: counted on the ids, with no
// expansion, negated or not.
func TestRelated_Count(t *testing.T) {
	f := relatedFake()
	p := newTestProvider(t, f)
	for _, c := range []struct {
		f    provider.Filter
		want int
	}{
		{provider.Filter{Field: "site", Value: "ams1"}, 2},
		{provider.Filter{Field: "site", Operator: "n", Value: "ams1"}, 3},
		{provider.Filter{Field: "role", Value: "core"}, 3},
	} {
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", CountOnly: true, Filters: []provider.Filter{c.f}})
		if err != nil {
			t.Fatalf("%+v: %v", c.f, err)
		}
		if res.Total != c.want {
			t.Errorf("%+v: Total %d, want %d", c.f, res.Total, c.want)
		}
	}
	if req := f.sentWith("dcim/devices", "filter[site_id]__eq"); len(req) == 0 || req[0].query.Has("expand") {
		t.Errorf("count requests %v, want filter[site_id]__eq with no expansion", req)
	}
}

// The editor offers a related filter only where its target has data: its
// values cannot be checked otherwise, and a query with one is refused — the
// editor never offers what a query refuses. null is the one value that needs
// no check, and a query may still name it.
func TestFilterFields_RelatedOnlyWhereTheTargetIsFed(t *testing.T) {
	f := relatedFake()
	f.addColumns("dcim/devices", "config_template_id:BIGINT")
	devices := filterOps(t, newTestProvider(t, f), "dcim/devices")
	for _, name := range []string{"site", "site_id", "tenant", "tenant_id", "role", "role_id", "rack_id"} {
		if want := []string{"", "n"}; !slices.Equal(devices[name], want) {
			t.Errorf("devices %s: %v, want %v", name, devices[name], want)
		}
	}
	// platforms have no data on the fixture, and the config template is
	// served by no table.
	for _, name := range []string{"platform", "platform_id", "config_template_id"} {
		if ops, ok := devices[name]; ok {
			t.Errorf("devices offer %s %v, whose target this replica cannot check values against", name, ops)
		}
	}
}
