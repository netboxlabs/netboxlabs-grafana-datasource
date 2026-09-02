package replicacache

import "testing"

// knownEntities is a realistic slice of what a deployment reports, including
// the pairs that make FK targeting ambiguous.
var knownEntities = map[string]bool{
	"dcim/devices": true, "dcim/device-roles": true, "dcim/device-types": true,
	"dcim/sites": true, "dcim/site-groups": true, "dcim/locations": true,
	"dcim/regions": true, "dcim/racks": true, "dcim/rack-roles": true,
	"dcim/manufacturers": true, "dcim/platforms": true, "dcim/interfaces": true,
	"dcim/virtual-chassis": true,
	"ipam/prefixes":        true, "ipam/roles": true, "ipam/vlans": true,
	"ipam/vlan-groups": true, "ipam/ip-addresses": true, "ipam/vrfs": true,
	"tenancy/tenants": true, "tenancy/tenant-groups": true,
	"virtualization/clusters": true, "virtualization/cluster-groups": true,
	"virtualization/virtual-machines": true,
}

func TestFKTarget(t *testing.T) {
	cases := []struct {
		name   string
		entity string
		column string
		want   string
	}{
		// The same column name means different things per app. This is the case
		// a hand-written table gets wrong, and the reason targeting is derived
		// and checked against the real entity list.
		{"device role is a device-role", "dcim/devices", "role", "dcim/device-roles"},
		{"rack role is a rack-role", "dcim/racks", "role", "dcim/rack-roles"},
		{"prefix role is the shared ipam role", "ipam/prefixes", "role", "ipam/roles"},

		// Owner-qualified groups.
		{"site group", "dcim/sites", "group", "dcim/site-groups"},
		{"tenant group", "tenancy/tenants", "group", "tenancy/tenant-groups"},
		{"cluster group", "virtualization/clusters", "group", "virtualization/cluster-groups"},
		{"vlan group", "ipam/vlans", "group", "ipam/vlan-groups"},

		// Same app, plain name.
		{"device site", "dcim/devices", "site", "dcim/sites"},
		{"device rack", "dcim/devices", "rack", "dcim/racks"},
		{"device location", "dcim/devices", "location", "dcim/locations"},
		{"site region", "dcim/sites", "region", "dcim/regions"},
		{"underscores become hyphens", "dcim/devices", "device_type", "dcim/device-types"},

		// Cross-app, unique match.
		{"device tenant crosses into tenancy", "dcim/devices", "tenant", "tenancy/tenants"},
		{"vm cluster crosses into virtualization", "dcim/devices", "cluster", "virtualization/clusters"},

		// IP columns that convention cannot name.
		{"primary_ip4", "dcim/devices", "primary_ip4", "ipam/ip-addresses"},
		{"primary_ip6", "dcim/devices", "primary_ip6", "ipam/ip-addresses"},
		{"oob_ip", "dcim/devices", "oob_ip", "ipam/ip-addresses"},
		{"nat_inside", "ipam/ip-addresses", "nat_inside", "ipam/ip-addresses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := fkTarget(tc.entity, tc.column, knownEntities)
			if !ok || got != tc.want {
				t.Errorf("fkTarget(%q,%q) = %q,%v; want %q", tc.entity, tc.column, got, ok, tc.want)
			}
		})
	}
}

// These must NOT resolve. Each would otherwise produce a confidently wrong
// name, which is worse than leaving the id visible.
func TestFKTargetRefusesWhatItCannotKnow(t *testing.T) {
	cases := []struct {
		name   string
		entity string
		column string
		why    string
	}{
		{"polymorphic assignment", "ipam/ip-addresses", "assigned_object",
			"the target is decided by a content-type id the service does not expose"},
		{"polymorphic scope", "dcim/locations", "scope",
			"same content-type problem"},
		{"users are not replicated", "dcim/devices", "owner",
			"no table to resolve against"},
		{"config templates are not replicated", "dcim/devices", "config_template",
			"no table to resolve against"},
		{"an unknown model", "dcim/devices", "widget",
			"nothing in the entity list matches"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := fkTarget(tc.entity, tc.column, knownEntities); ok {
				t.Errorf("resolved %q to %q, but %s", tc.column, got, tc.why)
			}
		})
	}
}

// An entity list that does not contain the target must not resolve, so a
// deployment serving fewer models degrades to raw ids rather than to names of
// things it cannot read.
func TestFKTargetHonoursTheDeploymentsEntityList(t *testing.T) {
	minimal := map[string]bool{"dcim/devices": true}
	if got, ok := fkTarget("dcim/devices", "site", minimal); ok {
		t.Errorf("resolved site to %q against a deployment that does not serve sites", got)
	}
}

func TestSingularize(t *testing.T) {
	cases := map[string]string{
		"devices": "device", "sites": "site", "ip-addresses": "ip-address",
		"device-roles": "device-role", "virtual-machines": "virtual-machine",
		"virtual-chassis": "virtual-chassis",
	}
	for in, want := range cases {
		if got := singularize(in); got != want {
			t.Errorf("singularize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplayOf(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]interface{}
		want string
	}{
		{"prefers name", map[string]interface{}{"name": "DC-1", "slug": "dc-1"}, "DC-1"},
		{"device types show their model", map[string]interface{}{"model": "N9K-C9504"}, "N9K-C9504"},
		{"ip addresses show the address", map[string]interface{}{"address": "10.0.0.1/24"}, "10.0.0.1/24"},
		{"circuits show the cid", map[string]interface{}{"cid": "ntt-001"}, "ntt-001"},
		// Never blank for a row that was genuinely found: a blank would read as
		// "this device has no site".
		{"falls back to the id", map[string]interface{}{"id": float64(7)}, "7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayOf(tc.obj); got != tc.want {
				t.Errorf("displayOf = %q, want %q", got, tc.want)
			}
		})
	}
}
