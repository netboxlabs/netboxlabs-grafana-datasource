package replicacache

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// NetBox declares some names with its natural_sort collation (ICU und-u-kn,
// deterministic), which compares digit runs by value and letters without regard
// to case first: leaf2 before leaf10, parity-… before SIN1-…. A sort on one is
// that order in NetBox mode, and the replica's is the bytes', so a complete
// result is put in NetBox's order here (sortNaturally), and a result a limit
// cut short says that the rows it kept are the replica's first.

// naturalSortNames are NetBox's tables whose name column it declares with
// db_collation="natural_sort" — name on every one — each with whether NetBox
// orders the model by that name first (its Meta.ordering begins with name), so
// that a sort on a reference to it is a sort on that name: an interface sorted
// by device, a device by site. Read from NetBox's own models in NetBox 4.4.10
// and 4.6.10, which agree on every one (Django's _meta, read-only:
// field.db_collation and Meta.ordering of every concrete model); regenerate it
// beside the filter vocabulary when the supported range moves. A model ordered
// by something else first (a rack by its site, an interface by its device) is
// natural on its own name and is not one a reference sorts by.
var naturalSortNames = map[string]bool{
	"circuits_provider":              true,
	"circuits_providernetwork":       false,
	"dcim_consoleport":               false,
	"dcim_consoleporttemplate":       false,
	"dcim_consoleserverport":         false,
	"dcim_consoleserverporttemplate": false,
	"dcim_device":                    true,
	"dcim_devicebay":                 false,
	"dcim_devicebaytemplate":         false,
	"dcim_frontport":                 false,
	"dcim_frontporttemplate":         false,
	"dcim_interface":                 false,
	"dcim_interfacetemplate":         false,
	"dcim_inventoryitem":             false,
	"dcim_inventoryitemtemplate":     false,
	"dcim_modulebay":                 false,
	"dcim_modulebaytemplate":         false,
	"dcim_powerfeed":                 false,
	"dcim_poweroutlet":               false,
	"dcim_poweroutlettemplate":       false,
	"dcim_powerpanel":                false,
	"dcim_powerport":                 false,
	"dcim_powerporttemplate":         false,
	"dcim_rack":                      false,
	"dcim_rearport":                  false,
	"dcim_rearporttemplate":          false,
	"dcim_site":                      true,
	"dcim_virtualchassis":            true,
	"dcim_virtualdevicecontext":      true,
	"ipam_asnrange":                  true,
	"ipam_routetarget":               true,
	"ipam_vlangroup":                 true,
	"ipam_vrf":                       true,
	"tenancy_contact":                true,
	"tenancy_tenant":                 true,
	"tenancy_tenantgroup":            true,
	"virtualization_cluster":         true,
	"virtualization_virtualdisk":     false,
	"virtualization_virtualmachine":  true,
	"vpn_ikepolicy":                  true,
	"vpn_ikeproposal":                true,
	"vpn_ipsecpolicy":                true,
	"vpn_ipsecprofile":               true,
	"vpn_ipsecproposal":              true,
	"vpn_l2vpn":                      true,
	"vpn_tunnel":                     true,
	"wireless_wirelesslangroup":      true,
}

// naturalSortColumn is the column every table above declares natural_sort.
const naturalSortColumn = "name"

// naturalSort says how NetBox orders rows by field when it sorts them by a
// natural_sort name: the entity's own name, or a reference whose target NetBox
// orders by its name first and whose expansion is that name (via is then the
// reference's column). False for any other field.
func naturalSort(e entity, c *catalog, field string) (via string, ok bool) {
	if field == naturalSortColumn && e.has(field) {
		_, natural := naturalSortNames[e.Table]
		return "", natural
	}
	ref, target, expanded := e.expandedColumn(field)
	if !expanded || field != ref.Ref.ExpandKey || target != naturalSortColumn || c == nil {
		return "", false
	}
	t, known := c.Entities[strings.TrimPrefix(ref.Ref.Path, "/v1/")]
	return ref.Name, known && naturalSortNames[t.Table]
}

// sortNaturally puts rows in the order NetBox lists them by a natural_sort
// name, column: ascending, the name under the collation (ICU und-u-kn, as
// migration dcim/0197 creates it), names ICU holds equal by their bytes, as
// PostgreSQL breaks a deterministic collation's ties, a null last; descending
// all of that reversed, a null first, as PostgreSQL orders DESC. Then, for a
// reference, the referenced object's id in the same direction, as NetBox
// orders two devices or VMs of one name (name, pk; a VRF's rd, which NetBox
// puts between, is not read), and the row's own id ascending, the tie-break
// NetBox mode adds to every sort. Measured against
// PostgreSQL 16.15: 64 names of mixed case, digit runs with and without
// leading zeros, and punctuation, in the same order. Where Go's collation
// tables and the database's ICU are of different Unicode versions, a name
// outside ASCII can sort differently, and so can a digit run of value zero
// followed by text ("0a").
func sortNaturally(rows []map[string]interface{}, column, via string, descending bool) {
	c := collate.New(language.Und, collate.Numeric)
	var buf collate.Buffer
	type keyed struct {
		row     map[string]interface{}
		name    string
		null    bool
		key     []byte
		ref, id int
	}
	ks := make([]keyed, len(rows))
	for i, row := range rows {
		name, named := row[column].(string)
		ks[i] = keyed{row: row, name: name, null: !named}
		ks[i].id, _ = toInt(row["id"])
		if via != "" {
			ks[i].ref, _ = toInt(row[via])
		}
		if named {
			ks[i].key = c.KeyFromString(&buf, name)
		}
	}
	slices.SortStableFunc(ks, func(a, b keyed) int {
		order := 0
		switch {
		case a.null != b.null && a.null:
			order = 1
		case a.null != b.null:
			order = -1
		default:
			order = cmp.Or(bytes.Compare(a.key, b.key), strings.Compare(a.name, b.name), cmp.Compare(a.ref, b.ref))
		}
		if descending {
			order = -order
		}
		return cmp.Or(order, cmp.Compare(a.id, b.id))
	})
	for i, k := range ks {
		rows[i] = k.row
	}
}

// sortByNetBoxName puts device rows in the order NetBox lists devices:
// ordering ('name', 'pk'), the name under the natural_sort collation NetBox
// declares on it (Device.name, db_collation="natural_sort").
func sortByNetBoxName(rows []map[string]interface{}) {
	sortNaturally(rows, naturalSortColumn, "", false)
}

// naturalSortCutShortNote says that the rows a limit kept are the replica's
// first by their bytes, which NetBox mode, comparing by its collation, need not
// keep: the rows are real and in NetBox's order, so it is a note, not a
// warning, and how to read them all.
func naturalSortCutShortNote(field string, kept, total int) string {
	how := fmt.Sprintf("Raise the limit to %s to read them all.", provider.HumanInt(total))
	if total > MaxLimit {
		how = fmt.Sprintf("Narrow the filters to %s rows or fewer to read them all.", provider.HumanInt(MaxLimit))
	}
	return fmt.Sprintf("Rows are sorted by %q as NetBox sorts it, but the limit kept %s of the %s rows that match, and the replica, which compares names by their bytes, chose which: NetBox mode can keep others. %s",
		field, provider.HumanInt(kept), provider.HumanInt(total), how)
}
