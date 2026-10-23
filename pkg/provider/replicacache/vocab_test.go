package replicacache

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// vocabAlts returns the alternatives for one NetBox filter, failing the test
// when the table or the filter is not in the vocabulary.
func vocabAlts(t *testing.T, table, name string) []vocabEntry {
	t.Helper()
	vocab, ok := vocabFor(table)
	if !ok {
		t.Fatalf("%s is not covered by the vocabulary", table)
	}
	alts, ok := vocab[name]
	if !ok || len(alts) == 0 {
		t.Fatalf("NetBox has a %s filter on %s, the vocabulary has none", name, table)
	}
	return alts
}

// vocabOne is the only meaning of a filter, for the ones whose meaning does not
// depend on the NetBox version.
func vocabOne(t *testing.T, table, name string) vocabEntry {
	t.Helper()
	alts := vocabAlts(t, table, name)
	if len(alts) != 1 {
		t.Fatalf("%s %s has %d meanings, want 1: %+v", table, name, len(alts), alts)
	}
	return alts[0]
}

// NetBox ignores a filter it does not have and returns every row, so a name that
// only the cache accepted would make a dashboard right in cache mode and wrong
// in NetBox mode. A device has site and site_id, and rack_id but no rack.
func TestVocab_DevicesHaveSiteNotRack(t *testing.T) {
	for _, name := range []string{"site", "site_id", "rack_id", "cluster_id", "oob_ip_id"} {
		vocabAlts(t, "dcim_device", name)
	}
	vocab, _ := vocabFor("dcim_device")
	for _, name := range []string{"rack", "cluster", "oob_ip"} {
		if _, ok := vocab[name]; ok {
			t.Errorf("dcim_device has a %s filter; NetBox has none and returns every row for it", name)
		}
	}
	if vocab, _ := vocabFor("ipam_prefix"); vocab["vlan"] != nil {
		t.Error("ipam_prefix has a vlan filter; NetBox has vlan_id")
	}
	if vocab, _ := vocabFor("dcim_interface"); vocab["lag"] != nil || vocab["cable"] != nil {
		t.Error("dcim_interface has a lag or cable filter; NetBox has lag_id and cable_id")
	}
}

// A component's site is the site cached on the interface itself (_site), not
// the one its device has: the filter names the field and the replica stores the
// column.
func TestVocab_InterfacesSiteIsTheCachedColumn(t *testing.T) {
	want := map[string]vocabEntry{
		"site":    {Kind: "related", Column: "_site_id", ToField: "slug", Target: "dcim.site", Null: true},
		"site_id": {Kind: "related", Column: "_site_id", ToField: "id", Target: "dcim.site", Null: true},
	}
	for name, w := range want {
		if got := vocabOne(t, "dcim_interface", name); got != w {
			t.Errorf("dcim_interface %s = %+v, want %+v", name, got, w)
		}
	}
}

// NetBox's parent filter on a role is a plain relation: it matches direct
// children only. Only the filters NetBox declares as tree filters include a
// subtree.
func TestVocab_ParentOnRolesIsNotTree(t *testing.T) {
	for name, to := range map[string]string{"parent": "slug", "parent_id": "id"} {
		got := vocabOne(t, "dcim_devicerole", name)
		if got.Kind != "related" || got.Column != "parent_id" || got.ToField != to || got.Target != "dcim.devicerole" {
			t.Errorf("dcim_devicerole %s = %+v, want a related filter on parent_id by %s", name, got, to)
		}
	}
}

// Both of a device's role filters include the descendants of the roles they
// name, by slug and by id alike.
func TestVocab_RoleOnDevicesIsTree(t *testing.T) {
	for name, to := range map[string]string{"role": "slug", "role_id": "id"} {
		got := vocabOne(t, "dcim_device", name)
		if got.Kind != "tree" || got.Column != "role_id" || got.ToField != to || got.Target != "dcim.devicerole" {
			t.Errorf("dcim_device %s = %+v, want a tree filter on role_id by %s", name, got, to)
		}
	}
}

// A component has no location tree filter: NetBox compares the slug or the id
// and no subtree.
func TestVocab_InterfacesLocationIsNotTree(t *testing.T) {
	for name, to := range map[string]string{"location": "slug", "location_id": "id"} {
		got := vocabOne(t, "dcim_interface", name)
		if got.Kind != "related" || got.Column != "_location_id" || got.ToField != to {
			t.Errorf("dcim_interface %s = %+v, want a related filter on _location_id by %s", name, got, to)
		}
	}
}

// Prefixes cache where they sit as _location, _region and _site_group, and
// NetBox's filters on them include the subtree.
func TestVocab_PrefixLocationRegionSiteGroupAreTreeOnCachedColumns(t *testing.T) {
	for _, c := range []struct{ name, column string }{
		{"location", "_location_id"}, {"location_id", "_location_id"},
		{"region", "_region_id"}, {"region_id", "_region_id"},
		{"site_group", "_site_group_id"}, {"site_group_id", "_site_group_id"},
	} {
		got := vocabOne(t, "ipam_prefix", c.name)
		if got.Kind != "tree" || got.Column != c.column {
			t.Errorf("ipam_prefix %s = %+v, want a tree filter on %s", c.name, got, c.column)
		}
	}
}

// NetBox matches a device's name without regard to case, and its negation with
// it. That is the difference between them: name__n is exact.
func TestVocab_DeviceNameIsIExact(t *testing.T) {
	name := vocabOne(t, "dcim_device", "name")
	if name.Kind != "value" || name.Column != "name" || name.Lookup != "iexact" || name.Exclude {
		t.Errorf("dcim_device name = %+v, want iexact on name", name)
	}
	negated := vocabOne(t, "dcim_device", "name__n")
	if negated.Kind != "value" || negated.Column != "name" || negated.Lookup != "exact" || !negated.Exclude {
		t.Errorf("dcim_device name__n = %+v, want a negated exact on name", negated)
	}
	if got := vocabOne(t, "dcim_device", "name__ie"); got.Lookup != "iexact" || got.Exclude {
		t.Errorf("dcim_device name__ie = %+v, want iexact", got)
	}
}

// An interface's device filter compares the device's NAME, through the device_id
// column; device_id compares the id.
func TestVocab_InterfaceDeviceIsRelatedOnName(t *testing.T) {
	want := map[string]vocabEntry{
		"device":    {Kind: "related", Column: "device_id", ToField: "name", Target: "dcim.device", Null: true},
		"device_id": {Kind: "related", Column: "device_id", ToField: "id", Target: "dcim.device", Null: true},
	}
	for name, w := range want {
		if got := vocabOne(t, "dcim_interface", name); got != w {
			t.Errorf("dcim_interface %s = %+v, want %+v", name, got, w)
		}
	}
}

// On an IP address, device is not a relation but a query NetBox runs itself
// (through the interfaces of the device and its virtual chassis).
func TestVocab_IPAddressDeviceIsAMethod(t *testing.T) {
	for _, name := range []string{"device", "device_id", "virtual_machine", "virtual_machine_id"} {
		got := vocabOne(t, "ipam_ipaddress", name)
		if got.Kind != "method" || got.Method == "" {
			t.Errorf("ipam_ipaddress %s = %+v, want a method filter", name, got)
		}
	}
	if got := vocabOne(t, "ipam_ipaddress", "device"); got.Method != "filter_device" {
		t.Errorf("ipam_ipaddress device method = %q, want filter_device", got.Method)
	}
}

// Whether NetBox accepts the literal null on a relation follows its form field:
// every declared filter does, cable-terminations' cable does not (a cable
// termination always has one).
func TestVocab_NullOnRelatedFilters(t *testing.T) {
	cable := vocabOne(t, "dcim_cabletermination", "cable")
	if cable.Kind != "modelchoice" || !cable.Single || cable.Null || cable.Column != "cable_id" || cable.ToField != "id" {
		t.Errorf("dcim_cabletermination cable = %+v, want a single-valued modelchoice on cable_id that takes no null", cable)
	}
	for _, c := range []struct{ table, name string }{{"dcim_device", "tenant"}, {"dcim_device", "tenant_id"}, {"dcim_interface", "device_id"}} {
		if got := vocabOne(t, c.table, c.name); !got.Null {
			t.Errorf("%s %s = %+v, want null accepted", c.table, c.name, got)
		}
	}
}

// A few filters mean different things in the two NetBoxes the file is generated
// from: front ports' rear_port_id is a column on 4.4 and a join through the
// mapping table on 4.6. The replica mirrors one of them, so the alternative that
// fits its own columns wins, and the first one, the newest NetBox's, otherwise.
func TestPickVocab_ServedColumnWins(t *testing.T) {
	alts := vocabAlts(t, "dcim_frontport", "rear_port_id")
	if len(alts) != 2 || alts[0].Kind != "path" || alts[1].Kind != "related" || alts[1].Column != "rear_port_id" {
		t.Fatalf("dcim_frontport rear_port_id = %+v, want the 4.6 path first and the 4.4 relation second", alts)
	}

	frontPorts44 := entity{Table: "dcim_frontport", Ingested: true, Columns: []column{{Name: "id"}, {Name: "name"}, {Name: "rear_port_id"}, {Name: "rear_port_position"}}}
	if got := pickVocab(alts, frontPorts44); got.Kind != "related" || got.Column != "rear_port_id" {
		t.Errorf("on a 4.4-shaped catalogue pickVocab = %+v, want the relation on the column it stores", got)
	}

	frontPorts46 := entity{Table: "dcim_frontport", Ingested: true, Columns: []column{{Name: "id"}, {Name: "name"}}}
	if got := pickVocab(alts, frontPorts46); got.Kind != "path" {
		t.Errorf("on a 4.6-shaped catalogue pickVocab = %+v, want the first alternative", got)
	}

	// A filter with one meaning is that meaning, served or not; no alternatives
	// is no entry rather than a panic.
	one := vocabAlts(t, "dcim_device", "site_id")
	if got := pickVocab(one, entity{}); got != one[0] {
		t.Errorf("pickVocab of one alternative = %+v, want it", got)
	}
	if got := pickVocab(nil, frontPorts44); got != (vocabEntry{}) {
		t.Errorf("pickVocab of nothing = %+v, want the zero entry", got)
	}
}

// vocabFor says false for what the vocabulary does not describe, and the caller
// then keeps today's catalogue-only behaviour: a plugin's model, a table the
// replica reads that NetBox has no API viewset for, a name that is no table.
func TestVocabFor_UncoveredTables(t *testing.T) {
	for _, table := range []string{"netbox_acme_widget", "plugins_acme_widgets", "dcim_cablepath", "", "dcim", "dcim_nosuchmodel", "dcim.device"} {
		if vocab, ok := vocabFor(table); ok || vocab != nil {
			t.Errorf("vocabFor(%q) = %d filters, %v; want not covered", table, len(vocab), ok)
		}
	}
	if _, ok := vocabFor("dcim_device"); !ok {
		t.Error("dcim_device is not covered")
	}
}

// replicaTables are the 75 tables with an API viewset among the 76 entities
// replica-cache v1.40.1 lists in GET /v1/_meta/schema (the 76th, dcim_cablepath,
// has none). A table missing here is a model the generator dropped.
var replicaTables = []string{
	"circuits_circuit", "circuits_circuittermination", "circuits_circuittype", "circuits_provider", "circuits_provideraccount", "circuits_providernetwork",
	"core_objecttype",
	"dcim_cable", "dcim_cabletermination", "dcim_consoleport", "dcim_consoleserverport", "dcim_device", "dcim_devicebay", "dcim_devicerole", "dcim_devicetype",
	"dcim_frontport", "dcim_interface", "dcim_inventoryitem", "dcim_inventoryitemrole", "dcim_location", "dcim_macaddress", "dcim_manufacturer", "dcim_module",
	"dcim_modulebay", "dcim_moduletype", "dcim_platform", "dcim_powerfeed", "dcim_poweroutlet", "dcim_powerpanel", "dcim_powerport", "dcim_rack", "dcim_rackrole",
	"dcim_racktype", "dcim_rearport", "dcim_region", "dcim_site", "dcim_sitegroup", "dcim_virtualchassis", "dcim_virtualdevicecontext",
	"ipam_aggregate", "ipam_asn", "ipam_asnrange", "ipam_fhrpgroup", "ipam_fhrpgroupassignment", "ipam_ipaddress", "ipam_iprange", "ipam_prefix", "ipam_rir",
	"ipam_role", "ipam_service", "ipam_servicetemplate", "ipam_vlan", "ipam_vlangroup", "ipam_vrf",
	"tenancy_tenant", "tenancy_tenantgroup",
	"virtualization_cluster", "virtualization_clustergroup", "virtualization_clustertype", "virtualization_virtualdisk", "virtualization_virtualmachine", "virtualization_vminterface",
	"vpn_ikepolicy", "vpn_ikeproposal", "vpn_ipsecpolicy", "vpn_ipsecprofile", "vpn_ipsecproposal", "vpn_l2vpn", "vpn_l2vpntermination", "vpn_tunnel", "vpn_tunnelgroup", "vpn_tunneltermination",
	"wireless_wirelesslan", "wireless_wirelesslangroup", "wireless_wirelesslink",
}

func TestVocab_CoversTheTablesTheReplicaServes(t *testing.T) {
	if len(replicaTables) != 75 {
		t.Fatalf("the list holds %d tables, want the 75 the v1.40.1 catalogue serves with an API viewset", len(replicaTables))
	}
	for _, table := range replicaTables {
		if vocab, ok := vocabFor(table); !ok || len(vocab) == 0 {
			t.Errorf("%s is served by the replica and not covered", table)
		}
	}
}

// The embedded file is generated; it must decode into exactly the fields the
// generator writes, so a renamed or added field breaks here instead of being
// dropped on the way in.
func TestVocab_EmbeddedFileDecodesStrictly(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(filterVocabJSON))
	dec.DisallowUnknownFields()
	var raw map[string]map[string][]vocabEntry
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("filtervocab.json: %v", err)
	}
	if len(raw) < 129 {
		t.Errorf("filtervocab.json holds %d models, want every NetBox model with an API viewset (129 on 4.6.10)", len(raw))
	}
	for label := range raw {
		if !strings.Contains(label, ".") || strings.Count(label, ".") != 1 {
			t.Errorf("model label %q is not app.model", label)
		}
	}
}

// Every entry says what its kind needs, so a rule that stopped setting a field
// is a failure here and not a filter that quietly does nothing.
func TestVocab_EveryEntryIsWellFormed(t *testing.T) {
	kinds := []string{kindValue, kindMAC, kindBool, kindIsNull, kindEmpty, kindRelated, kindTree, kindModelChoice,
		kindAddress, kindPrefix, kindContains, kindLocalContext, kindMethod, kindPath, kindRefuse}
	var raw map[string]map[string][]vocabEntry
	if err := json.Unmarshal(filterVocabJSON, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("filtervocab.json is empty")
	}
	for label, names := range raw {
		for name, alts := range names {
			if len(alts) == 0 {
				t.Errorf("%s %s has no meaning", label, name)
			}
			for i, e := range alts {
				where := label + " " + name
				if !slices.Contains(kinds, e.Kind) {
					t.Errorf("%s: unknown kind %q", where, e.Kind)
				}
				if strings.HasSuffix(e.Column, "_id_id") {
					t.Errorf("%s: column %q doubles the _id", where, e.Column)
				}
				need := func(field, value string) {
					if value == "" {
						t.Errorf("%s: a %s entry has no %s: %+v", where, e.Kind, field, e)
					}
				}
				switch e.Kind {
				case kindValue, kindMAC:
					need("column", e.Column)
					need("lookup", e.Lookup)
				case kindBool, kindIsNull, kindEmpty, kindAddress, kindPrefix, kindContains, kindLocalContext:
					need("column", e.Column)
				case kindRelated, kindTree, kindModelChoice:
					need("column", e.Column)
					need("to-field", e.ToField)
					need("target", e.Target)
				case kindMethod:
					need("method", e.Method)
				case kindPath:
					need("path", e.Path)
					if e.Column != "" {
						t.Errorf("%s: a path names column %q", where, e.Column)
					}
				case kindRefuse:
					need("reason", e.Reason)
				}
				if e.Kind != kindRefuse && e.Reason != "" {
					t.Errorf("%s: a %s entry has the reason %q", where, e.Kind, e.Reason)
				}
				if e.Kind == kindRefuse && e.Exclude {
					t.Errorf("%s: a refusal carries the exclude flag", where)
				}
				if e.NullValue && e.Kind != kindValue && e.Kind != kindMAC {
					t.Errorf("%s: nv on a %s entry", where, e.Kind)
				}
				if e.Null && e.Kind != kindRelated && e.Kind != kindTree && e.Kind != kindModelChoice {
					t.Errorf("%s: null on a %s entry", where, e.Kind)
				}
				for _, other := range alts[i+1:] {
					a, b := e, other
					a.Reason, b.Reason = "", ""
					if a == b {
						t.Errorf("%s: two alternatives are one meaning: %+v", where, e)
					}
				}
			}
		}
	}
}

// Where NetBox reads the literal null as IS NULL on a text, choice or MAC
// filter — measured by compiling name=null — the entry says so; where it refuses
// the text or compares it as the empty string it does not.
func TestVocab_NullOnValueFilters(t *testing.T) {
	for _, c := range []struct {
		table, name string
		want        bool
	}{
		{"dcim_device", "asset_tag", true},
		{"dcim_device", "asset_tag__n", true},
		{"dcim_device", "name", true},
		{"dcim_device", "name__nie", true},
		{"dcim_device", "airflow", true},
		{"dcim_device", "status", false},         // null is not one of its choices
		{"dcim_device", "asset_tag__ic", false},  // a text search on None raises
		{"dcim_device", "vc_position", false},    // a number: its form refuses the text
		{"dcim_interface", "mtu", false},         // likewise
		{"dcim_site", "time_zone", false},        // NetBox compares null as ''
		{"dcim_site", "time_zone__n", false},     // and so does its negation
		{"ipam_ipaddress", "role", true},         // a choice that accepts null
		{"dcim_macaddress", "mac_address", true}, // a MAC address filter
		{"dcim_macaddress", "mac_address__n", true},
	} {
		got := vocabOne(t, c.table, c.name)
		if got.NullValue != c.want {
			t.Errorf("%s %s nv = %v, want %v (%+v)", c.table, c.name, got.NullValue, c.want, got)
		}
	}
}

// The booleans, the empties and the refusals, each as NetBox 4.4.10 answered
// it: cabled=true is IS NOT NULL on the cable column, mtu__empty works and
// speed__empty is a 400 ("Enter a whole number."), a prefix's family is not an
// equality.
func TestVocab_BooleansEmptiesAndRefusals(t *testing.T) {
	want := []struct {
		table, name string
		want        vocabEntry
	}{
		{"dcim_interface", "enabled", vocabEntry{Kind: "bool", Column: "enabled", Single: true}},
		{"dcim_interface", "cabled", vocabEntry{Kind: "isnull", Column: "cable_id", Exclude: true, Single: true}},
		{"dcim_interface", "mtu__empty", vocabEntry{Kind: "isnull", Column: "mtu", Single: true}},
		{"dcim_interface", "speed__empty", vocabEntry{Kind: "refuse", Reason: "badempty"}},
		{"dcim_device", "created__empty", vocabEntry{Kind: "refuse", Reason: "badempty"}},
		{"dcim_device", "created", vocabEntry{Kind: "refuse", Reason: "date"}},
		{"dcim_device", "name__empty", vocabEntry{Kind: "empty", Column: "name", Single: true}},
		{"ipam_prefix", "family", vocabEntry{Kind: "refuse", Reason: "lookup:family", Single: true}},
		{"ipam_prefix", "mask_length", vocabEntry{Kind: "refuse", Reason: "lookup:net_mask_length"}},
		{"ipam_prefix", "children", vocabEntry{Kind: "value", Column: "_children", Lookup: "exact"}},
		{"ipam_ipaddress", "address", vocabEntry{Kind: "address", Column: "address"}},
		{"ipam_prefix", "prefix", vocabEntry{Kind: "prefix", Column: "prefix"}},
		{"ipam_aggregate", "prefix", vocabEntry{Kind: "prefix", Column: "prefix", Single: true}},
		{"ipam_prefix", "contains", vocabEntry{Kind: "contains", Column: "prefix", Single: true}},
		{"ipam_iprange", "contains", vocabEntry{Kind: "method", Method: "search_contains", Single: true}},
		{"dcim_device", "local_context_data", vocabEntry{Kind: "localcontext", Column: "local_context_data", Single: true}},
	}
	for _, c := range want {
		if got := vocabOne(t, c.table, c.name); got != c.want {
			t.Errorf("%s %s = %+v, want %+v", c.table, c.name, got, c.want)
		}
	}
}

// The paths NetBox filters through, refused by name, as they were measured on
// NetBox 4.4.10 and 4.6.10: through a second relation, a many-to-many, a generic
// relation, and the generic relation behind an interface's MAC addresses.
func TestVocab_PathsAreNamed(t *testing.T) {
	for _, c := range []struct{ table, name, path string }{
		{"dcim_device", "tenant_group", "tenant → group"},
		{"dcim_device", "manufacturer", "device_type → manufacturer → slug"},
		{"dcim_device", "region", "site → region"},
		{"dcim_device", "tag", "tags → slug"},
		{"dcim_site", "asn", "asns → asn"},
		{"ipam_ipaddress", "interface", "interface → name"},
		{"dcim_interface", "mac_address", "mac_addresses → mac_address"},
	} {
		got := vocabOne(t, c.table, c.name)
		if got.Kind != "path" || got.Path != c.path || got.Column != "" {
			t.Errorf("%s %s = %+v, want the path %q", c.table, c.name, got, c.path)
		}
	}
}

func mustVocab(t *testing.T, table string) map[string][]vocabEntry {
	t.Helper()
	vocab, ok := vocabFor(table)
	if !ok {
		t.Fatalf("%s is not covered", table)
	}
	return vocab
}

// The shape the walk produced when it was checked against NetBox: on the models
// the replica serves, merged across NetBox 4.4.10 and 4.6.10, without the names
// a filter translation passes through whatever they mean (q, tag, tag_id and
// their lookup variants; 505 meanings, 432 of them paths and 73 methods). A
// large drift means the walk, not NetBox, changed. When the file is regenerated
// from other NetBox versions these move on purpose.
func TestVocab_KindCountsOnServedModels(t *testing.T) {
	want := map[string]int{
		"value": 4710, "path": 1249, "related": 932, "refuse:date": 906, "method": 366, "empty": 292, "isnull": 207,
		"refuse:badempty": 157, "tree": 124, "refuse:decimal": 96, "refuse:contenttype": 30, "bool": 27, "mac": 12,
		"refuse:wwn": 12, "refuse:array": 12, "refuse:lookup": 11, "address": 3, "modelchoice": 2, "prefix": 2,
		"localcontext": 2, "contains": 1,
	}
	got := map[string]int{}
	for _, table := range replicaTables {
		for name, alts := range mustVocab(t, table) {
			if base, _, _ := strings.Cut(name, "__"); base == "q" || base == "tag" || base == "tag_id" {
				continue
			}
			for _, e := range alts {
				kind := e.Kind
				if kind == "refuse" {
					kind += ":" + strings.SplitN(e.Reason, ":", 2)[0]
				}
				got[kind]++
			}
		}
	}
	for kind, n := range want {
		if got[kind] != n {
			t.Errorf("%s = %d, want %d", kind, got[kind], n)
		}
	}
	for kind, n := range got {
		if _, ok := want[kind]; !ok {
			t.Errorf("%s = %d, a kind the expected shape does not have", kind, n)
		}
	}
}
