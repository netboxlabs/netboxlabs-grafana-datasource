package replicacache

import (
	"context"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

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

// A parent_id is the table's own primary key. Every entity on a live instance
// that carries the column is self-referential, and the naming candidates
// cannot find that — they look for a "parents" model, which no deployment has
// — so these columns stayed bare ids while NetBox mode resolved them to names.
func TestParentResolvesToTheQueriedEntity(t *testing.T) {
	for _, entity := range []string{
		"dcim/locations", "dcim/site-groups", "dcim/regions",
		"dcim/device-roles", "dcim/interfaces", "tenancy/tenant-groups",
	} {
		got, ok := fkTarget(entity, "parent", knownEntities)
		if !ok || got != entity {
			t.Errorf("fkTarget(%q,\"parent\") = %q,%v; want %q", entity, got, ok, entity)
		}
	}

	// Still bounded by what the deployment reports: an entity absent from the
	// list resolves to nothing rather than to itself.
	if got, ok := fkTarget("dcim/module-bays", "parent", knownEntities); ok {
		t.Errorf("resolved to %q, but dcim/module-bays is not in this deployment", got)
	}

	// And the polymorphic parent stays unresolved: it is a different column
	// (parent_object, backed by a content-type id) and pointing it at the
	// queried entity would be a confidently wrong name.
	if got, ok := fkTarget("dcim/locations", "parent_object", knownEntities); ok {
		t.Errorf("resolved parent_object to %q; its target is decided by a content-type id", got)
	}
}

// Measured, not guessed. fkTarget was run over every foreign-key column found
// on a live instance and its answer compared against the target NetBox's own
// 4.4.10 schema gives. These are the pairs the derivation could not reach, and
// the first one is the reason overrides are consulted before it: nothing named
// "virtual-machine-roles" exists, so the global-basename fallback settled on
// the only "roles" model in the list and rendered VM roles with IPAM names.
func TestFKTargetHandlesRelationshipsConventionCannotDerive(t *testing.T) {
	known := map[string]bool{}
	for k, v := range knownEntities {
		known[k] = v
	}
	known["dcim/mac-addresses"] = true
	known["virtualization/interfaces"] = true

	cases := []struct {
		entity, column, want string
	}{
		{"virtualization/virtual-machines", "role", "dcim/device-roles"},
		{"dcim/interfaces", "lag", "dcim/interfaces"},
		{"dcim/interfaces", "bridge", "dcim/interfaces"},
		{"dcim/interfaces", "untagged_vlan", "ipam/vlans"},
		{"dcim/interfaces", "qinq_svlan", "ipam/vlans"},
		{"dcim/interfaces", "primary_mac_address", "dcim/mac-addresses"},
		{"ipam/vlans", "qinq_svlan", "ipam/vlans"},
		{"virtualization/interfaces", "bridge", "virtualization/interfaces"},
		{"virtualization/interfaces", "untagged_vlan", "ipam/vlans"},
		{"dcim/device-types", "default_platform", "dcim/platforms"},
		{"dcim/locations", "parent", "dcim/locations"},
		// The derivation still owns everything it can reach.
		{"dcim/devices", "role", "dcim/device-roles"},
		{"ipam/prefixes", "role", "ipam/roles"},
		{"dcim/devices", "primary_ip4", "ipam/ip-addresses"},
	}
	for _, tc := range cases {
		got, ok := fkTarget(tc.entity, tc.column, known)
		if !ok || got != tc.want {
			t.Errorf("fkTarget(%q,%q) = %q,%v; want %q", tc.entity, tc.column, got, ok, tc.want)
		}
	}

	// Overrides stay gated on the entity list: a deployment not serving the
	// target degrades to the raw id rather than to a name it cannot read.
	thin := map[string]bool{"virtualization/virtual-machines": true, "dcim/interfaces": true}
	for _, tc := range []struct{ entity, column string }{
		{"virtualization/virtual-machines", "role"},
		{"dcim/interfaces", "untagged_vlan"},
	} {
		if got, ok := fkTarget(tc.entity, tc.column, thin); ok {
			t.Errorf("resolved %s.%s to %q, but the target is not served here", tc.entity, tc.column, got)
		}
	}
}

// Discovery is only worth waiting for if something in the rows could actually
// be resolved. These bases are rejected by fkTarget whatever discovery returns,
// so counting them bought a wait of up to the whole budget to learn something
// already known.
func TestUnresolvableIDsDoNotJustifyDiscovery(t *testing.T) {
	unresolvable := []map[string]interface{}{{
		"id":                 float64(1),
		"owner_id":           float64(7),
		"created_by_id":      float64(8),
		"last_updated_by_id": float64(9),
		"assigned_object_id": float64(10),
		"scope_id":           float64(11),
		"name":               "CORE-1",
	}}
	if hasResolvableFK(unresolvable) {
		t.Error("none of these can be resolved; discovery must not be waited on")
	}

	// One resolvable id alongside them is enough to make the wait worthwhile.
	withSite := []map[string]interface{}{{
		"id": float64(1), "owner_id": float64(7), "site_id": float64(4001),
	}}
	if !hasResolvableFK(withSite) {
		t.Error("site_id is resolvable, so discovery is worth waiting for")
	}
}

// A null row in a DIMENSION response is the trap the top-level fix does not
// cover: it decodes without error, leaves the map nil, and skipping it made
// fetchRelated report SUCCESS with a name missing — so resolveFKs raised no
// degradation warning and the panel showed a blank "site" with nothing to
// explain it.
func TestNullDimensionRowIsReportedNotSwallowed(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	f.nullRowsFor = "dcim/sites"
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		// The device rows are fine, so the query still answers: a failed
		// dimension degrades the result, it does not destroy it.
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("a dimension that returned nothing usable must be reported, not passed off as resolved")
	}
	var named bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "dcim/sites") {
			named = true
		}
	}
	if !named {
		t.Errorf("the warning should name the dimension that failed, got %v", res.Warnings)
	}
	// And the id survives, which is what the warning tells the reader to use.
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Errorf("site_id = %v, want 4001", res.Rows[0]["site_id"])
	}
}

// NetBox mode answers "tenant": null, which flattens to a tenant column of
// nulls. Here the same fact arrives as tenant_id: null, so nothing resolved and
// the column did not exist — a panel selecting tenant lost it on switching
// modes, and came back with NO columns when tenant was the only field selected.
func TestAllNullRelationshipStillHasItsColumn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "site_id": float64(4001), "tenant_id": nil},
		{"id": float64(2), "name": "CORE-2", "site_id": float64(4001), "tenant_id": nil},
	}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	// The partial case: site resolves, tenant is null everywhere, so tenant
	// never reached the resolution path at all.
	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var hasTenant bool
	for _, c := range res.Columns {
		if c == "tenant" {
			hasTenant = true
		}
	}
	if !hasTenant {
		t.Errorf("tenant missing from columns %v", res.Columns)
	}
	if v, ok := res.Rows[0]["tenant"]; !ok || v != nil {
		t.Errorf("tenant = %#v (present %v), want a nil value", v, ok)
	}
	if res.Rows[0]["site"] != "DC-Northeast" {
		t.Errorf("site = %v, want DC-Northeast — resolution must still work", res.Rows[0]["site"])
	}

	// And selecting only that column must not come back with nothing.
	res, err = p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"tenant"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Columns) != 1 || res.Columns[0] != "tenant" {
		t.Errorf("columns = %v, want just tenant", res.Columns)
	}
}

// Being object-shaped is not enough for a dimension row: without an id there is
// nothing to match against the rows that referenced it. Skipping it let
// fetchRelated report success with names missing, so no degradation warning was
// raised and the panel showed a blank "site" with nothing to explain it.
func TestDimensionRowWithoutAnIDIsReported(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	// The id is stripped on the way out, so the row still matches the filter
	// that asked for it and reaches the decoder — which is the path under test.
	f.idlessRowsFor = "dcim/sites"
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		// The device rows are fine, so the query still answers — a dead
		// dimension degrades the result rather than destroying it.
		t.Fatalf("Query: %v", err)
	}
	var named bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "dcim/sites") {
			named = true
		}
	}
	if !named {
		t.Errorf("the failed dimension must be named, got %v", res.Warnings)
	}
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Errorf("site_id = %v, want 4001 — the id the warning points at must survive", res.Rows[0]["site_id"])
	}
}

// A dimension that answers for only SOME of the ids asked about is not a
// protocol failure — these are separate tables replicated independently, so a
// reference can outrun the row it points at, or the row can be deleted between
// the two requests. But it leaves the column blank on those rows, which reads
// as "this device has no site", and an alert rule would consume that as fact.
func TestPartiallyResolvedDimensionIsReported(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		deviceFixture(1, "CORE-1", 4001),
		deviceFixture(2, "CORE-2", 4002), // site 4002 has not replicated yet
	}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("a lagging replica must not fail the query: %v", err)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "dcim/sites") && strings.Contains(w, "missing") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the unresolved reference must be stated, got %v", res.Warnings)
	}
	// The half that DID resolve still resolves, and both ids survive.
	var one, two map[string]interface{}
	for _, r := range res.Rows {
		switch r["id"] {
		case float64(1):
			one = r
		case float64(2):
			two = r
		}
	}
	if one["site"] != "DC-Northeast" {
		t.Errorf("site = %v, want DC-Northeast", one["site"])
	}
	if two["site_id"] != float64(4002) {
		t.Errorf("site_id = %v, want 4002 — the id the warning points at must survive", two["site_id"])
	}
}

// And a dimension that answers for everything asked about must stay silent, or
// the warning becomes noise that hides the real ones.
func TestFullyResolvedDimensionDoesNotWarn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("nothing was missing, so nothing should be reported: %v", res.Warnings)
	}
}

// The coverage check has to compare WHICH ids came back, not how many. A lookup
// for {4001,4002} answered with {4001,9999} has the same cardinality while
// leaving 4002 unresolved, so subtracting map sizes reported nothing missing.
func TestCoverageIsCheckedByIDNotByCount(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		deviceFixture(1, "CORE-1", 4001),
		deviceFixture(2, "CORE-2", 4002),
	}
	// The dimension answers with the right NUMBER of rows and the wrong ones.
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
		{"id": float64(9999), "name": "Somewhere-Else", "slug": "elsewhere"},
	}
	f.ignoreIDFilterFor = "dcim/sites"
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "dcim/sites") && strings.Contains(w, "missing") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("4002 was never answered for; that must be reported: %v", res.Warnings)
	}
}
