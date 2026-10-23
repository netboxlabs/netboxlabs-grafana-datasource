package replicacache

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// IP enrichment is NetBox mode's (netbox/ipenrich.go) answered from the
// replica's tables: the same rows and columns, the same pick, match_count and
// prefix fallback, the same display strings, wherever the replica holds the
// same data. NetBox mode reads what NetBox's serializers compute — an
// address's assigned object, a VRF's display, a nested NAT list. The replica
// serves the rows those are computed from, so each is computed here from the
// rows, by NetBox's own rule, and an address's owner is named only through a
// content-type id learned exactly from the rows (contenttypes.go): an id that
// cannot be learned leaves the owner blank and says so.

// addressesEntity is the table every row starts from.
const addressesEntity = "ipam/ip-addresses"

// addressRecordFields are the address columns a row is built from; tenant and
// nat_inside come by name under expand=, which needs their ids named here to
// return them when a target has no data.
const addressRecordFields = "id,address,status,role,dns_name,description,tenant_id,vrf_id,nat_inside_id,assigned_object_type_id,assigned_object_id"

// addressRecord is an ipam/ip-addresses row as ip-enrichment reads it. An id
// of 0 is a null reference: the replica's ids are positive.
type addressRecord struct {
	ID          int     `json:"id"`
	Address     string  `json:"address"`
	Status      *string `json:"status"`
	Role        *string `json:"role"`
	DNSName     *string `json:"dns_name"`
	Description *string `json:"description"`
	TenantID    int     `json:"tenant_id"`
	Tenant      *string `json:"tenant"`
	VRFID       int     `json:"vrf_id"`
	NATInsideID int     `json:"nat_inside_id"`
	NATInside   *string `json:"nat_inside"`
	TypeID      int     `json:"assigned_object_type_id"`
	ObjectID    int     `json:"assigned_object_id"`
}

// ResolveIPs maps each input IP to its NetBox context, as NetBox mode does:
// the address record and its interface and device where one exists, the
// longest containing prefix where none does. Exactly one row per distinct
// input — the documented Grafana join on "ip" needs unique keys — with
// match_count saying how many records matched.
func (p *Provider) ResolveIPs(ctx context.Context, ips []string, fields []string, limit int) (*provider.Result, error) {
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
	fields, limit = provider.NormalizeIPEnrichArgs(fields, limit, defaultLimit, MaxLimit)
	// Deduplicated in input order, as NetBox mode does: requested is every
	// distinct input and becomes Total, so a list cut by the limit reads as
	// truncated.
	seen := map[string]bool{}
	var wanted []string
	requested := 0
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		requested++
		if len(wanted) < limit {
			wanted = append(wanted, ip)
		}
	}
	addresses, c, err := p.entityFor(ctx, addressesEntity)
	if err != nil {
		return nil, err
	}
	st := &readStats{}
	byHost, cappedHosts, err := p.recordsByHost(ctx, wanted, addresses, st)
	if err != nil {
		return nil, err
	}
	en := newEnrichment(p, c, addresses, fields, st)
	if len(cappedHosts) > 0 {
		en.warnings = append(en.warnings, recordsCappedWarning(cappedHosts))
	}
	return en.run(ctx, wanted, byHost, requested)
}

// recordsByHost reads the address records of every input that is an address,
// by NetBox's own match — the host, whatever the mask — and indexes them by
// canonical host (provider.CanonicalIPOK), which is also how an input finds
// its bucket. An input that is no address is never sent: the replica refuses a
// whole host list for one bad element, and NetBox could hold no record for it.
//
// A host with more records than one read returns is named in capped: its
// match_count is a floor and its pick was made over the part that was read.
func (p *Provider) recordsByHost(ctx context.Context, ips []string, addresses entity, st *readStats) (map[string][]addressRecord, []string, error) {
	var hosts []string
	for _, ip := range ips {
		if host, ok := provider.CanonicalIPOK(ip); ok {
			hosts = append(hosts, pgAddrText(netip.MustParseAddr(host)))
		}
	}
	if len(hosts) == 0 {
		return nil, nil, nil
	}
	if col, ok := addresses.column("address"); addresses.Ingested && (!ok || !slices.Contains(col.Operators, "host")) {
		return nil, nil, &UnsupportedError{
			Feature: "IP enrichment",
			Detail:  "This replica-cache build cannot match an address by its host whatever its mask (a newer build adds it), which is how NetBox finds an IP's records. Ask NetBox Labs to upgrade the replica, or use a datasource in NetBox mode for IP enrichment.",
		}
	}
	rows, capped, err := p.listBatched(ctx, addressesEntity, param("address", "host"), hosts, 0, MaxLimit, addressRecordParams(addresses), st)
	if err != nil {
		return nil, nil, err
	}
	records, err := decodeRows[addressRecord](addressesEntity, rows)
	if err != nil {
		return nil, nil, err
	}
	byHost := indexByHost(records)
	var cappedHosts []string
	if capped {
		// A read that stopped at its ceiling did so on one host alone (a batch
		// over it is split down to one value), and returned that many rows of
		// it, so the hosts holding that many are the ones cut short.
		for _, h := range slices.Sorted(maps.Keys(byHost)) {
			if len(byHost[h]) >= MaxLimit {
				cappedHosts = append(cappedHosts, h)
			}
		}
	}
	return byHost, cappedHosts, nil
}

// addressRecordParams is the projection and the references joined in. Only an
// available reference is expanded: one whose target has no data on the tenant
// joins nothing, and its id, named in fields=, is what comes back.
func addressRecordParams(addresses entity) url.Values {
	q := url.Values{"fields": {addressRecordFields}}
	if expand := availableExpansions(addresses, "tenant", "nat_inside"); len(expand) > 0 {
		q.Set("expand", strings.Join(expand, ","))
	}
	return q
}

// indexByHost is the records under their canonical host.
func indexByHost(records []addressRecord) map[string][]addressRecord {
	byHost := map[string][]addressRecord{}
	for _, r := range records {
		h, _ := provider.CanonicalIPOK(r.Address)
		byHost[h] = append(byHost[h], r)
	}
	return byHost
}

// recordsCappedWarning is NetBox mode's addressTruncationWarning for a host
// with more records than one read can carry: the row is real, but match_count
// is a floor and the pick was made over a part. The hosts are named, since
// the reader can act on it only by looking at them; there cannot be many.
func recordsCappedWarning(hosts []string) string {
	const maxNamed = 3
	named := hosts[:min(len(hosts), maxNamed)]
	list := strings.Join(named, ", ")
	if rest := len(hosts) - len(named); rest > 0 {
		list += fmt.Sprintf(", and %s more", provider.HumanInt(rest))
	}
	subject := provider.HumanInt(len(hosts)) + " IPs"
	if len(hosts) == 1 {
		subject = "1 IP"
	}
	return fmt.Sprintf(
		"The replica holds more than %s address records for %s (%s) — only the first %s were read. match_count is a floor rather than a count for the affected rows, and their address_*, interface_*, device_*, vm_name and is_primary_ip columns describe a record picked from the part that was read.",
		provider.HumanInt(MaxLimit), subject, list, provider.HumanInt(MaxLimit))
}

// ResolveScope is ResolveIPs for every address the filters select, for alert
// rules: a rule cannot hand a query an IP list, but it can join a metric's IP
// against this table in a SQL expression. The filters are NetBox's filters on
// ipam/ip-addresses, answered as an objects query answers them (Query). One
// row per distinct host, in NetBox's address order (family, then host), and a
// host's records are the ones the scope listed, as in NetBox mode — an
// address held in two VRFs, one of them in the scope, matched one record here.
//
// Total is the hosts when the listing is complete and the records the filters
// match when it is not: the host count of a cut-off scope is unknowable, and
// the record count is at least the rows, so a cut-off scope reads as truncated
// and a strict consumer refuses it. Which records a cut-off scope keeps is the
// replica's order, not NetBox's.
func (p *Provider) ResolveScope(ctx context.Context, filters []provider.Filter, fields []string, limit int) (*provider.Result, error) {
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
	if err := refuseAddressNetworkFilters(filters); err != nil {
		return nil, err
	}
	fields, limit = provider.NormalizeIPEnrichArgs(fields, limit, defaultLimit, MaxLimit)
	listed, err := p.Query(ctx, provider.QuerySpec{ObjectType: addressesEntity, Filters: filters, Fields: []string{"id"}, Limit: limit})
	if err != nil {
		return nil, err
	}
	addresses, c, err := p.entityFor(ctx, addressesEntity)
	if err != nil {
		return nil, err
	}
	st := &readStats{}
	st.note(listed.DataAsOf)
	var ids []int
	for _, row := range listed.Rows {
		if id, ok := toInt(row["id"]); ok {
			ids = append(ids, id)
		}
	}
	// The records by id, in the shape the list path reads them: the listing
	// is the objects path's, which names columns its own way.
	rows, _, err := p.listByIDs(ctx, addressesEntity, "id", intStrings(ids), addressRecordParams(addresses), st)
	if err != nil {
		return nil, err
	}
	records, err := decodeRows[addressRecord](addressesEntity, rows)
	if err != nil {
		return nil, err
	}
	byHost := indexByHost(records)
	hosts := slices.SortedFunc(maps.Keys(byHost), compareHosts)
	total := len(hosts)
	if listed.Total > len(listed.Rows) {
		total = listed.Total
	}
	en := newEnrichment(p, c, addresses, fields, st)
	en.warnings = append(en.warnings, withoutFreshness(listed.Warnings)...)
	en.notes = append(en.notes, withoutFreshness(listed.Notes)...)
	return en.run(ctx, hosts, byHost, total)
}

// compareHosts is NetBox's address order (IPAddressManager orders by the host
// cast back to an inet): IPv4 before IPv6, then by address. A key that is no
// address — a record the replica holds in a form netip cannot read — goes
// last.
func compareHosts(a, b string) int {
	x, errX := netip.ParseAddr(a)
	y, errY := netip.ParseAddr(b)
	switch {
	case errX != nil && errY != nil:
		return strings.Compare(a, b)
	case errX != nil:
		return 1
	case errY != nil:
		return -1
	}
	return x.Compare(y)
}

// addressNetworkFilters are NetBox's ipam/ip-addresses filters that read the
// address's own network: the prefix it lies in (parent), its mask length and
// its family. NetBox computes each with a lookup on the inet; the replica keeps
// the address as text and has no operator for any of them.
var addressNetworkFilters = map[string]bool{"parent": true, "mask_length": true, "family": true}

// refuseAddressNetworkFilters refuses those filters by name, before the
// vocabulary does in its own words: the reason here is the one that tells a
// scope's author what to use instead. A row that sends nothing is left alone,
// as everywhere.
func refuseAddressNetworkFilters(filters []provider.Filter) error {
	for _, f := range filters {
		if sendsNothing(f) {
			continue
		}
		if base, _, _ := strings.Cut(f.Field, "__"); addressNetworkFilters[base] {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
				Reason: "NetBox answers it from the address's network (the prefix it lies in, its mask length or its family), and the replica has no address-within-prefix filter, so it cannot list those addresses; scope by VRF or tenant instead, or " + netboxModeAdvice}
		}
	}
	return nil
}

// ipSelection is what the columns asked for need read. A read whose every
// column is unselected is skipped, as NetBox mode skips such a hop: its output
// would be projected away, and a warning about it would name columns the
// frame does not have.
type ipSelection struct {
	cols map[string]bool
}

func newIPSelection(fields []string) ipSelection {
	s := ipSelection{cols: map[string]bool{}}
	for _, f := range fields {
		s.cols[f] = true
	}
	return s
}

func (s ipSelection) has(col string) bool { return s.cols[col] }

func (s ipSelection) group(prefix string) bool {
	for c := range s.cols {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// owner is whether any column is named from what an address is assigned to.
func (s ipSelection) owner() bool {
	return s.group("interface_") || s.group("device_") || s.has(provider.IsPrimaryIPColumn) || s.has("vm_name")
}

// typed is whether any column needs the assignment's type named.
func (s ipSelection) typed() bool { return s.owner() || s.has("address_assigned_object_type") }

// picked is whether any column shows which of a host's records was picked.
func (s ipSelection) picked() bool { return s.owner() || s.group("address_") }

// enrichment is one ip-enrichment answer in the making: what was asked, what
// the reads found, and what the result has to say about it.
type enrichment struct {
	p         *Provider
	c         *catalog
	addresses entity
	fields    []string
	sel       ipSelection
	st        *readStats

	types      contentTypes
	ifaces     map[int]ifaceRecord
	vmIfaces   map[int]vmIfaceRecord
	devices    map[int]deviceRecord
	vms        map[int]vmRecord
	racks      map[int]rackRecord
	vrfs       map[int]vrfRecord
	vlans      map[int]vlanRecord
	natOutside map[int][]string
	prefixOf   map[string]*prefixRecord
	scopeNames map[scopeLevel]map[int]string

	warnings, notes []string
}

func newEnrichment(p *Provider, c *catalog, addresses entity, fields []string, st *readStats) *enrichment {
	return &enrichment{p: p, c: c, addresses: addresses, fields: fields, sel: newIPSelection(fields), st: st}
}

// ipRow is one row being built, and whether a reference on it named an object
// the replica does not hold.
type ipRow struct {
	cols     map[string]interface{}
	sel      ipSelection
	dangling bool
}

func (r *ipRow) set(col string, v interface{}) { r.cols[col] = v }

// ref sets a column named from a reference NetBox keeps intact (a foreign key):
// nil when the id is null, the name when the replica holds its object, and nil
// beside a counted gap when it does not — which a consistent NetBox never
// shows, so it is the replica still applying changes.
func (r *ipRow) ref(col string, id int, name *string) {
	switch {
	case id == 0:
		r.cols[col] = nil
	case name != nil:
		r.cols[col] = *name
	default:
		r.cols[col] = nil
		r.dangling = r.dangling || r.sel.has(col)
	}
}

// run builds the rows for hosts, in order, from their records: the pick, the
// owner, the prefix fallback for a host with none, the display strings — each
// read made once for every row that needs it — and states what the result
// cannot show.
func (en *enrichment) run(ctx context.Context, hosts []string, byHost map[string][]addressRecord, total int) (*provider.Result, error) {
	// Each row's records are under its canonical host, the key they were
	// indexed by; an input that is no address has a key no record has.
	cands := make(map[string][]addressRecord, len(hosts))
	for _, h := range hosts {
		key, _ := provider.CanonicalIPOK(h)
		cands[h] = byHost[key]
	}
	if err := en.learnTypes(ctx, cands); err != nil {
		return nil, err
	}

	picked := map[string]addressRecord{}
	undecided := 0
	for _, h := range hosts {
		if len(cands[h]) == 0 {
			continue
		}
		r, decided := pickRecord(cands[h], en.types)
		picked[h] = r
		if !decided && en.sel.picked() {
			undecided++
		}
	}
	if err := en.readOwners(ctx, picked); err != nil {
		return nil, err
	}
	if err := en.readNATOutside(ctx, picked); err != nil {
		return nil, err
	}
	if err := en.readPrefixes(ctx, hosts, cands); err != nil {
		return nil, err
	}
	if err := en.readDisplays(ctx, picked); err != nil {
		return nil, err
	}

	rows := make([]map[string]interface{}, 0, len(hosts))
	ambiguous, unresolved, dangling := 0, 0, 0
	for _, h := range hosts {
		row := &ipRow{cols: map[string]interface{}{"ip": h}, sel: en.sel}
		// float64, as every numeric column is: frame building types a column
		// numeric from float64 values alone (see the NetBox provider's
		// match_count).
		row.set("match_count", float64(len(cands[h])))
		if r, ok := picked[h]; ok {
			if len(cands[h]) > 1 {
				ambiguous++
			}
			en.addressColumns(row, r)
			if !en.ownerColumns(row, r) && en.sel.typed() {
				unresolved++
			}
		} else {
			en.prefixColumns(row, h)
		}
		if row.dangling {
			dangling++
		}
		rows = append(rows, project(row.cols, en.fields))
	}

	if undecided > 0 {
		en.warnings = append(en.warnings, pickUndecidedWarning(undecided, len(rows)))
	}
	if unresolved > 0 {
		en.warnings = append(en.warnings, unresolvedAssignmentWarning(unresolved))
	}
	if dangling > 0 {
		en.warnings = append(en.warnings, fmt.Sprintf(
			"Related objects are missing for %s of %s rows: they reference objects this replica does not hold, so those columns are blank there.",
			provider.HumanInt(dangling), provider.HumanInt(len(rows))))
	}
	if ambiguous > 0 {
		en.notes = append(en.notes, provider.AmbiguityNote(ambiguous, len(rows)))
	}
	notes, warnings := enrichmentFreshness(en.c, en.addresses, en.st)
	complete := en.c.SnapshotComplete
	return &provider.Result{
		Columns:          en.fields,
		Rows:             rows,
		Total:            total,
		MaxRows:          MaxLimit,
		Warnings:         append(en.warnings, warnings...),
		Notes:            append(en.notes, notes...),
		ColumnTypes:      provider.DeclaredIPEnrichColumnTypes(en.fields),
		DataAsOf:         en.st.dataAsOf(),
		SnapshotComplete: &complete,
	}, nil
}

// project narrows a row to the requested fields, in the requested order; a
// column the row never set is null, as in NetBox mode.
func project(row map[string]interface{}, fields []string) map[string]interface{} {
	out := make(map[string]interface{}, len(fields))
	for _, f := range fields {
		out[f] = row[f]
	}
	return out
}

// pickUndecidedWarning is the pick a type this replica cannot name makes
// impossible to repeat: NetBox mode ranks a record on an interface over one on
// anything else, and a record on an object of an unknown type could be either.
func pickUndecidedWarning(rows, total int) string {
	return fmt.Sprintf(
		"%s of %s rows matched several address records, some of them assigned to objects this replica cannot identify, so the record they show is the one with the lowest id among those NetBox mode could pick, which may not be the one it picks.",
		provider.HumanInt(rows), provider.HumanInt(total))
}

// unresolvedAssignmentWarning counts the rows whose address is assigned to an
// object of a type the rows did not teach (contenttypes.go): their owner
// columns are blank because the type is unknown, not because there is none.
func unresolvedAssignmentWarning(n int) string {
	if n == 1 {
		return "1 address is assigned to an object this replica cannot identify (it does not serve NetBox's content-type names), so its interface and device columns are blank."
	}
	return fmt.Sprintf("%d addresses are assigned to objects this replica cannot identify (it does not serve NetBox's content-type names), so their interface and device columns are blank.", n)
}

// enrichmentFreshness states the age of the answer as an objects query states
// its own (freshnessNotes): the oldest instant of every read, or why there is
// none. With nothing read — no input was an address — the addresses' own
// instant stands in, as the catalogue's does for a query that sends no
// request; with some read of unknown age, the loading warning while the
// snapshot loads (the rows may be a fraction of the data) and a note once it
// has.
func enrichmentFreshness(c *catalog, addresses entity, st *readStats) (notes, warnings []string) {
	switch asOf := st.dataAsOf(); {
	case asOf != nil:
		return freshnessNotes(c, addresses, asOf)
	case !st.unknown:
		return freshnessNotes(c, addresses, addresses.DataAsOf)
	case !c.SnapshotComplete:
		return nil, []string{loadingWarning}
	}
	return []string{agePrefix + "is unknown: the replica reports no commit time for a table this context was read from."}, nil
}

// learnTypes names the content types of the assignments the answer depends on:
// every record's, when a column is named from the owner or its type, and those
// of hosts with several records when only the pick depends on them. A record
// on an unknown type is still ranked (see pickRecord).
func (en *enrichment) learnTypes(ctx context.Context, cands map[string][]addressRecord) error {
	en.types = contentTypes{}
	var ids []int
	for _, rs := range cands {
		if !en.sel.typed() && (!en.sel.picked() || len(rs) < 2) {
			continue
		}
		for _, r := range rs {
			if r.TypeID > 0 {
				ids = append(ids, r.TypeID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	types, err := en.p.assignmentTypes(ctx, ids, en.st)
	if err != nil {
		return err
	}
	en.types = types
	return nil
}

// pickRecord is NetBox mode's pick (provider.PickAddressRank, then the lowest
// id) over a host's records, and whether it is the one NetBox mode makes.
//
// A record on an object of a type the rows did not teach could rank as an
// interface's or as another assignment's — and every record carrying that type
// id ranks the same way, since one id is one model. So the pick is made under
// each reading of each distinct unknown id (a host's records carry at most the
// three types NetBox assigns addresses to, so at most eight readings): when
// every reading picks the same record it is NetBox mode's pick whatever the
// types are; when they differ, the pick is undecided, which the caller states,
// and the record kept is the lowest id of those some reading picks — one NetBox
// mode could show, never one that loses under every reading.
//
// "Another assignment" is a record with an object id, where NetBox mode tests
// that the object exists: an address the API assigned to an object that does
// not exist ranks above an unassigned one here and level with it there, the
// only place the two can differ.
func pickRecord(cands []addressRecord, types contentTypes) (addressRecord, bool) {
	var unknown []int
	for _, r := range cands {
		if _, known := types[r.TypeID]; r.TypeID > 0 && !known && !slices.Contains(unknown, r.TypeID) {
			unknown = append(unknown, r.TypeID)
		}
	}
	lowest := func(rs []addressRecord) addressRecord {
		return slices.MinFunc(rs, func(a, b addressRecord) int { return cmp.Compare(a.ID, b.ID) })
	}
	// More unknown ids than NetBox has assignment models is data NetBox's API
	// does not write; there is no reading to trust, so no record is preferred.
	if len(unknown) > maxUnknownAssignmentTypes {
		return lowest(cands), false
	}
	var picks []addressRecord
	for reading := range 1 << len(unknown) {
		onInterface := func(r addressRecord) bool {
			if name, known := types[r.TypeID]; known {
				return name == typeInterface || name == typeVMInterface
			}
			i := slices.Index(unknown, r.TypeID)
			return i >= 0 && reading&(1<<i) != 0
		}
		var best addressRecord
		bestRank := -1
		for _, r := range cands {
			deprecated := r.Status != nil && *r.Status == "deprecated"
			assigned := r.TypeID > 0 && r.ObjectID > 0
			rank := provider.PickAddressRank(onInterface(r), assigned, deprecated)
			if rank > bestRank || (rank == bestRank && r.ID < best.ID) {
				best, bestRank = r, rank
			}
		}
		if !slices.ContainsFunc(picks, func(p addressRecord) bool { return p.ID == best.ID }) {
			picks = append(picks, best)
		}
	}
	return lowest(picks), len(picks) == 1
}

// maxUnknownAssignmentTypes bounds the readings pickRecord tries: NetBox's
// serializer assigns an address to an interface, a VM interface or an FHRP
// group, so no host's records carry more type ids than that.
const maxUnknownAssignmentTypes = 3

// The owners' rows: what an interface, a VM interface, a device and a virtual
// machine contribute to a row. A name is null only where NetBox allows it.

type ifaceRecord struct {
	ID          int     `json:"id"`
	Name        *string `json:"name"`
	Label       *string `json:"label"`
	Description *string `json:"description"`
	Device      int     `json:"device_id"`
}

type vmIfaceRecord struct {
	ID          int     `json:"id"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	VM          int     `json:"virtual_machine_id"`
}

type deviceRecord struct {
	ID           int     `json:"id"`
	Name         *string `json:"name"`
	Status       *string `json:"status"`
	RackID       int     `json:"rack_id"`
	IP4          int     `json:"primary_ip4_id"`
	IP6          int     `json:"primary_ip6_id"`
	RoleID       int     `json:"role_id"`
	Role         *string `json:"role"`
	PlatformID   int     `json:"platform_id"`
	Platform     *string `json:"platform"`
	DeviceTypeID int     `json:"device_type_id"`
	DeviceType   *string `json:"device_type"`
	SiteID       int     `json:"site_id"`
	Site         *string `json:"site"`
	LocationID   int     `json:"location_id"`
	Location     *string `json:"location"`
	TenantID     int     `json:"tenant_id"`
	Tenant       *string `json:"tenant"`
}

type vmRecord struct {
	ID   int     `json:"id"`
	Name *string `json:"name"`
	IP4  int     `json:"primary_ip4_id"`
	IP6  int     `json:"primary_ip6_id"`
}

type rackRecord struct {
	ID       int     `json:"id"`
	Name     *string `json:"name"`
	Facility *string `json:"facility_id"`
}

type vrfRecord struct {
	ID   int     `json:"id"`
	Name *string `json:"name"`
	RD   *string `json:"rd"`
}

type vlanRecord struct {
	ID   int     `json:"id"`
	Name *string `json:"name"`
	VID  *int    `json:"vid"`
}

type namedRecord struct {
	ID   int     `json:"id"`
	Name *string `json:"name"`
}

// deviceFields is the device columns a row's device_* columns come from;
// primaryFields is all is_primary_ip needs, which a selection without a
// device_* column reads alone, as NetBox mode projects its device hop then.
const (
	deviceFields  = "id,name,status,rack_id,primary_ip4_id,primary_ip6_id,role_id,platform_id,device_type_id,site_id,location_id,tenant_id"
	primaryFields = "id,primary_ip4_id,primary_ip6_id"
)

// readOwners reads what the picked records are assigned to, and the devices,
// virtual machines and racks behind them, for the columns that need them.
func (en *enrichment) readOwners(ctx context.Context, picked map[string]addressRecord) error {
	var ifaceIDs, vmIfaceIDs []int
	for _, r := range picked {
		switch en.types[r.TypeID] {
		case typeInterface:
			ifaceIDs = append(ifaceIDs, r.ObjectID)
		case typeVMInterface:
			vmIfaceIDs = append(vmIfaceIDs, r.ObjectID)
		}
	}
	sel := en.sel
	var err error
	if sel.group("interface_") || sel.group("device_") || sel.has(provider.IsPrimaryIPColumn) {
		if en.ifaces, err = readByID[ifaceRecord](ctx, en.p, "dcim/interfaces", ifaceIDs, url.Values{"fields": {"id,name,label,description,device_id"}}, en.st,
			func(r ifaceRecord) int { return r.ID }); err != nil {
			return err
		}
	}
	if sel.group("interface_") || sel.has("vm_name") || sel.has(provider.IsPrimaryIPColumn) {
		if en.vmIfaces, err = readByID[vmIfaceRecord](ctx, en.p, "virtualization/interfaces", vmIfaceIDs, url.Values{"fields": {"id,name,description,virtual_machine_id"}}, en.st,
			func(r vmIfaceRecord) int { return r.ID }); err != nil {
			return err
		}
	}
	if sel.group("device_") || sel.has(provider.IsPrimaryIPColumn) {
		var deviceIDs []int
		for _, i := range en.ifaces {
			deviceIDs = append(deviceIDs, i.Device)
		}
		params := url.Values{"fields": {primaryFields}}
		if sel.group("device_") {
			params.Set("fields", deviceFields)
			e, _, err := en.p.entityFor(ctx, "dcim/devices")
			if err != nil && !unknownEntity(err) {
				return err
			}
			if expand := availableExpansions(e, "role", "platform", "device_type", "site", "location", "tenant"); len(expand) > 0 {
				params.Set("expand", strings.Join(expand, ","))
			}
		}
		if en.devices, err = readByID[deviceRecord](ctx, en.p, "dcim/devices", deviceIDs, params, en.st, func(r deviceRecord) int { return r.ID }); err != nil {
			return err
		}
	}
	if sel.has("vm_name") || sel.has(provider.IsPrimaryIPColumn) {
		var vmIDs []int
		for _, i := range en.vmIfaces {
			vmIDs = append(vmIDs, i.VM)
		}
		if en.vms, err = readByID[vmRecord](ctx, en.p, "virtualization/virtual-machines", vmIDs, url.Values{"fields": {"id,name,primary_ip4_id,primary_ip6_id"}}, en.st,
			func(r vmRecord) int { return r.ID }); err != nil {
			return err
		}
	}
	if sel.has("device_rack") {
		var rackIDs []int
		for _, d := range en.devices {
			rackIDs = append(rackIDs, d.RackID)
		}
		if en.racks, err = readByID[rackRecord](ctx, en.p, "dcim/racks", rackIDs, url.Values{"fields": {"id,name,facility_id"}}, en.st,
			func(r rackRecord) int { return r.ID }); err != nil {
			return err
		}
	}
	return nil
}

// readByID reads the rows of objectType with the given primary keys, by key.
// A table this replica does not serve, or has no data for, has none of them —
// no error and no warning: a table an empty NetBox never fed is referenced by
// no row, and a row that references one anyway is counted by the row
// (ipRow.ref).
func readByID[T any](ctx context.Context, p *Provider, objectType string, ids []int, params url.Values, st *readStats, key func(T) int) (map[int]T, error) {
	out := map[int]T{}
	var want []int
	for _, id := range ids {
		if id > 0 {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return out, nil
	}
	rows, err := p.rowsByID(ctx, objectType, "id", intStrings(distinctSorted(want)), params, st)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeRows[T](objectType, rows)
	if err != nil {
		return nil, err
	}
	for _, r := range decoded {
		out[key(r)] = r
	}
	return out, nil
}

// readNATOutside reads, for the picked records, the addresses whose NAT inside
// each is: NetBox's nat_outside, ordered as NetBox orders addresses (family,
// then host; the id between two of one host, which NetBox leaves to the
// database), each shown with its mask.
func (en *enrichment) readNATOutside(ctx context.Context, picked map[string]addressRecord) error {
	if !en.sel.has("address_nat_outside") {
		return nil
	}
	var ids []int
	for _, r := range picked {
		ids = append(ids, r.ID)
	}
	type outside struct {
		ID     int    `json:"id"`
		Inside int    `json:"nat_inside_id"`
		Addr   string `json:"address"`
	}
	rows, capped, err := en.p.listBatched(ctx, addressesEntity, param("nat_inside_id", "in"), intStrings(distinctSorted(ids)), 0, MaxLimit,
		url.Values{"fields": {"id,address,nat_inside_id"}}, en.st)
	if err != nil {
		return err
	}
	if capped {
		en.warnings = append(en.warnings, fmt.Sprintf(
			"An address has more than %s NAT outside addresses, so address_nat_outside lists only the first %s read for it.",
			provider.HumanInt(MaxLimit), provider.HumanInt(MaxLimit)))
	}
	outs, err := decodeRows[outside](addressesEntity, rows)
	if err != nil {
		return err
	}
	slices.SortFunc(outs, func(a, b outside) int {
		ha, _ := provider.CanonicalIPOK(a.Addr)
		hb, _ := provider.CanonicalIPOK(b.Addr)
		return cmp.Or(compareHosts(ha, hb), cmp.Compare(a.ID, b.ID))
	})
	en.natOutside = map[int][]string{}
	for _, o := range outs {
		en.natOutside[o.Inside] = append(en.natOutside[o.Inside], withSingleHostMask(o.Addr))
	}
	return nil
}

// scopeLevel is one of the scope levels NetBox caches on a prefix, most
// specific first: a prefix scoped to a location caches the location, its site
// and the site's region and group; to a site, the site, region and group; to
// a region or a site group, that alone. So the most specific level set is the
// scope itself.
type scopeLevel struct{ table string }

var scopeLevels = []scopeLevel{{"dcim/locations"}, {"dcim/sites"}, {"dcim/site-groups"}, {"dcim/regions"}}

// prefixRecord is an ipam/prefixes row as the fallback reads it.
type prefixRecord struct {
	ID          int     `json:"id"`
	Prefix      string  `json:"prefix"`
	VRF         int     `json:"vrf_id"`
	TenantID    int     `json:"tenant_id"`
	Tenant      *string `json:"tenant"`
	RoleID      int     `json:"role_id"`
	Role        *string `json:"role"`
	VLAN        int     `json:"vlan_id"`
	Description *string `json:"description"`
	Scope       int     `json:"scope_id"`
	Location    int     `json:"_location_id"`
	Site        int     `json:"_site_id"`
	SiteGroup   int     `json:"_site_group_id"`
	Region      int     `json:"_region_id"`
	net         netip.Prefix
}

// level is the prefix's id at a scope level, 0 for none.
func (r *prefixRecord) level(l scopeLevel) int {
	switch l.table {
	case "dcim/locations":
		return r.Location
	case "dcim/sites":
		return r.Site
	case "dcim/site-groups":
		return r.SiteGroup
	}
	return r.Region
}

// prefixRecordFields is what the fallback reads of a prefix; tenant and role come
// by name under expand=.
const prefixRecordFields = "id,prefix,vrf_id,tenant_id,role_id,vlan_id,description,scope_id,_site_id,_location_id,_region_id,_site_group_id"

// readPrefixes is NetBox mode's prefix fallback, for the hosts with no record
// when a prefix_* column is asked for: the prefixes that contain the value
// provider.PrefixQuery builds — a bare input strictly, a masked one
// inclusively, as NetBox's contains reads them — read in as many requests as
// the replica's containment limit and the budget take, then, per host, the
// longest, ties broken in NetBox's prefix order: the global table first, then
// by VRF id, then by id (Prefix.Meta.ordering is vrf nulls first, prefix, pk,
// and prefixes of one length holding one host are one network).
func (en *enrichment) readPrefixes(ctx context.Context, hosts []string, cands map[string][]addressRecord) error {
	if !en.sel.group("prefix_") {
		return nil
	}
	terms := map[string]netip.Prefix{}
	var values []string
	for _, h := range hosts {
		if len(cands[h]) > 0 {
			continue
		}
		v, ok := provider.PrefixQuery(h)
		if !ok {
			continue
		}
		term, err := queryTerm(v)
		if err != nil {
			continue
		}
		terms[h] = term
		values = append(values, v)
	}
	if len(values) == 0 {
		return nil
	}
	e, _, err := en.p.entityFor(ctx, "ipam/prefixes")
	if err != nil {
		if unknownEntity(err) {
			return nil
		}
		return err
	}
	if !e.Ingested {
		return nil
	}
	if !en.c.offersContains() {
		return &UnsupportedError{
			Feature: "the prefix columns of IP enrichment",
			Detail:  "This replica-cache build has no containment search (v1.39 added it), so it cannot find the prefix that holds an address with no record. Leave the prefix_* columns out, ask NetBox Labs to upgrade the replica, or use a datasource in NetBox mode.",
		}
	}
	params := url.Values{"fields": {prefixRecordFields}}
	if expand := availableExpansions(e, "tenant", "role"); len(expand) > 0 {
		params.Set("expand", strings.Join(expand, ","))
	}
	rows, capped, err := en.p.listBatched(ctx, "ipam/prefixes", param("prefix", "contains"), values, en.c.ContainsMaxValues, MaxLimit, params, en.st)
	if err != nil {
		return err
	}
	if capped {
		en.warnings = append(en.warnings, fmt.Sprintf(
			"More than %s prefixes contain one of these IPs, so its prefix_* columns come from the first %s read and may name a shorter prefix than the longest that holds it.",
			provider.HumanInt(MaxLimit), provider.HumanInt(MaxLimit)))
	}
	prefixes, err := decodeRows[prefixRecord]("ipam/prefixes", rows)
	if err != nil {
		return err
	}
	// Indexed by network, so each host looks up its own networks, longest
	// first, rather than testing every prefix read.
	byNet := map[netip.Prefix][]*prefixRecord{}
	for i := range prefixes {
		pr := &prefixes[i]
		net, err := netip.ParsePrefix(pr.Prefix)
		if err != nil {
			return &TransportError{Op: "reading ipam/prefixes", Err: err, Message: rowShapeGuidance}
		}
		pr.net = net.Masked()
		byNet[pr.net] = append(byNet[pr.net], pr)
	}
	en.prefixOf = map[string]*prefixRecord{}
	for h, term := range terms {
		if best := longestPrefix(term, byNet); best != nil {
			en.prefixOf[h] = best
		}
	}
	return nil
}

// queryTerm reads a PrefixQuery value back: a bare host is asked about
// strictly (its own single-host prefix is no answer), a host/len inclusively.
func queryTerm(v string) (netip.Prefix, error) {
	if strings.Contains(v, "/") {
		return netip.ParsePrefix(v)
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Prefix{}, err
	}
	// One bit short of the host: strict containment.
	return netip.PrefixFrom(a, a.BitLen()-1), nil
}

// longestPrefix is the longest prefix holding term's host, by NetBox's order
// between prefixes of one network.
func longestPrefix(term netip.Prefix, byNet map[netip.Prefix][]*prefixRecord) *prefixRecord {
	for bits := term.Bits(); bits >= 0; bits-- {
		rows := byNet[netip.PrefixFrom(term.Addr(), bits).Masked()]
		if len(rows) == 0 {
			continue
		}
		return slices.MinFunc(rows, func(a, b *prefixRecord) int {
			return cmp.Or(cmp.Compare(min(a.VRF, 1), min(b.VRF, 1)), cmp.Compare(a.VRF, b.VRF), cmp.Compare(a.ID, b.ID))
		})
	}
	return nil
}

// readDisplays reads the objects whose NetBox display strings the rows show:
// VRFs (the address's and the prefix's), VLANs and the scope's names.
func (en *enrichment) readDisplays(ctx context.Context, picked map[string]addressRecord) error {
	var vrfIDs, vlanIDs []int
	if en.sel.has("address_vrf") {
		for _, r := range picked {
			vrfIDs = append(vrfIDs, r.VRFID)
		}
	}
	scopeIDs := map[scopeLevel][]int{}
	for _, pr := range en.prefixOf {
		if en.sel.has("prefix_vrf") {
			vrfIDs = append(vrfIDs, pr.VRF)
		}
		if en.sel.has("prefix_vlan") {
			vlanIDs = append(vlanIDs, pr.VLAN)
		}
		if en.sel.has("prefix_scope") {
			for _, l := range scopeLevels {
				if id := pr.level(l); id > 0 {
					scopeIDs[l] = append(scopeIDs[l], id)
					break
				}
			}
		}
	}
	var err error
	if en.vrfs, err = readByID[vrfRecord](ctx, en.p, "ipam/vrfs", vrfIDs, url.Values{"fields": {"id,name,rd"}}, en.st,
		func(r vrfRecord) int { return r.ID }); err != nil {
		return err
	}
	if en.vlans, err = readByID[vlanRecord](ctx, en.p, "ipam/vlans", vlanIDs, url.Values{"fields": {"id,name,vid"}}, en.st,
		func(r vlanRecord) int { return r.ID }); err != nil {
		return err
	}
	en.scopeNames = map[scopeLevel]map[int]string{}
	for _, l := range scopeLevels {
		named, err := readByID[namedRecord](ctx, en.p, l.table, scopeIDs[l], url.Values{"fields": {"id,name"}}, en.st,
			func(r namedRecord) int { return r.ID })
		if err != nil {
			return err
		}
		en.scopeNames[l] = map[int]string{}
		for id, r := range named {
			if r.Name != nil {
				en.scopeNames[l][id] = *r.Name
			}
		}
	}
	return nil
}

// addressColumns fills address_* from the picked record, as NetBox mode's
// flattened address does: a choice by its label, a related object by its
// display, a blank text as itself.
func (en *enrichment) addressColumns(row *ipRow, r addressRecord) {
	row.set("address_dns_name", textValue(r.DNSName))
	row.set("address_status", choiceValue(addressesEntity, "status", r.Status))
	row.set("address_role", choiceValue(addressesEntity, "role", r.Role))
	row.set("address_description", textValue(r.Description))
	row.ref("address_tenant", r.TenantID, r.Tenant)
	en.vrfColumn(row, "address_vrf", r.VRFID)
	var inside *string
	if r.NATInside != nil {
		masked := withSingleHostMask(*r.NATInside)
		inside = &masked
	}
	row.ref("address_nat_inside", r.NATInsideID, inside)
	// A record names its outsides even when it has none: NetBox mode flattens
	// an empty list to "".
	row.set("address_nat_outside", strings.Join(en.natOutside[r.ID], "; "))
	if name, ok := en.types[r.TypeID]; ok {
		row.set("address_assigned_object_type", name)
	}
}

// ownerColumns fills the columns named from what the record is assigned to,
// and reports whether its type was known (or there is no assignment): an
// unknown type leaves them blank, which the caller counts. A target the
// replica does not hold leaves them blank uncounted, as NetBox mode shows an
// address assigned to an object that does not exist.
func (en *enrichment) ownerColumns(row *ipRow, r addressRecord) bool {
	if r.TypeID == 0 {
		return true
	}
	name, known := en.types[r.TypeID]
	if !known {
		return false
	}
	switch name {
	case typeInterface:
		iface, ok := en.ifaces[r.ObjectID]
		if !ok {
			return true
		}
		row.set("interface_name", interfaceDisplay(iface.Name, iface.Label))
		if iface.Description != nil && *iface.Description != "" {
			row.set("interface_description", *iface.Description)
		}
		if iface.Device == 0 {
			return true
		}
		dev, ok := en.devices[iface.Device]
		if !ok {
			row.dangling = row.dangling || en.sel.group("device_") || en.sel.has(provider.IsPrimaryIPColumn)
			return true
		}
		en.deviceColumns(row, dev, r.ID)
	case typeVMInterface:
		vi, ok := en.vmIfaces[r.ObjectID]
		if !ok {
			return true
		}
		row.set("interface_name", textValue(vi.Name))
		if vi.Description != nil && *vi.Description != "" {
			row.set("interface_description", *vi.Description)
		}
		if vi.VM == 0 {
			return true
		}
		vm, ok := en.vms[vi.VM]
		if !ok {
			row.dangling = row.dangling || en.sel.has("vm_name") || en.sel.has(provider.IsPrimaryIPColumn)
			return true
		}
		if vm.Name != nil && *vm.Name != "" {
			row.set("vm_name", *vm.Name)
		}
		row.set(provider.IsPrimaryIPColumn, vm.IP4 == r.ID || vm.IP6 == r.ID)
	}
	return true
}

// deviceColumns fills device_* and the device's answer for is_primary_ip:
// whether the address is its primary IPv4 or IPv6 (an out-of-band address is
// neither).
func (en *enrichment) deviceColumns(row *ipRow, d deviceRecord, addressID int) {
	row.set(provider.IsPrimaryIPColumn, d.IP4 == addressID || d.IP6 == addressID)
	if !en.sel.group("device_") {
		return
	}
	row.set("device_name", textValue(d.Name))
	row.set("device_status", choiceValue("dcim/devices", "status", d.Status))
	row.ref("device_role", d.RoleID, d.Role)
	row.ref("device_platform", d.PlatformID, d.Platform)
	row.ref("device_device_type", d.DeviceTypeID, d.DeviceType)
	row.ref("device_site", d.SiteID, d.Site)
	row.ref("device_location", d.LocationID, d.Location)
	row.ref("device_tenant", d.TenantID, d.Tenant)
	var rack *string
	if rr, ok := en.racks[d.RackID]; ok && rr.Name != nil {
		shown := *rr.Name
		// Rack.__str__: the facility id beside the name when it has one.
		if rr.Facility != nil && *rr.Facility != "" {
			shown += " (" + *rr.Facility + ")"
		}
		rack = &shown
	}
	if en.sel.has("device_rack") {
		row.ref("device_rack", d.RackID, rack)
	}
}

// prefixColumns fills prefix_* from the host's longest containing prefix, or
// leaves them null when none holds it.
func (en *enrichment) prefixColumns(row *ipRow, host string) {
	pr, ok := en.prefixOf[host]
	if !ok {
		return
	}
	row.set("prefix_cidr", pr.Prefix)
	row.set("prefix_description", textValue(pr.Description))
	row.ref("prefix_tenant", pr.TenantID, pr.Tenant)
	row.ref("prefix_role", pr.RoleID, pr.Role)
	en.vrfColumn(row, "prefix_vrf", pr.VRF)
	var vlan *string
	if v, ok := en.vlans[pr.VLAN]; ok && v.Name != nil && v.VID != nil {
		// VLAN.__str__.
		shown := *v.Name + " (" + strconv.Itoa(*v.VID) + ")"
		vlan = &shown
	}
	row.ref("prefix_vlan", pr.VLAN, vlan)
	// The scope is named from the most specific level cached on the prefix. A
	// scope with no level cached cannot be named, and is a gap like any other.
	scope, scopeID := (*string)(nil), pr.Scope
	for _, l := range scopeLevels {
		if id := pr.level(l); id > 0 {
			scopeID = id
			if name, ok := en.scopeNames[l][id]; ok {
				scope = &name
			}
			break
		}
	}
	row.ref("prefix_scope", scopeID, scope)
}

// vrfColumn sets a VRF as NetBox displays one (VRF.__str__): the name, and the
// route distinguisher beside it when there is one.
func (en *enrichment) vrfColumn(row *ipRow, col string, id int) {
	var shown *string
	if v, ok := en.vrfs[id]; ok && v.Name != nil {
		s := *v.Name
		if v.RD != nil && *v.RD != "" {
			s += " (" + *v.RD + ")"
		}
		shown = &s
	}
	row.ref(col, id, shown)
}

// interfaceDisplay is a device interface as NetBox displays it
// (ComponentModel.__str__): the label beside the name when it has one. A VM
// interface has no label, and displays as its name.
func interfaceDisplay(name, label *string) interface{} {
	if name == nil {
		return nil
	}
	if label != nil && *label != "" {
		return *name + " (" + *label + ")"
	}
	return *name
}

// textValue is a text column as NetBox serializes it: null stays null, a blank
// stays blank.
func textValue(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

// choiceValue is a choice as NetBox mode shows it: the label, null for none —
// NetBox's choice field serializes a blank choice as null too.
func choiceValue(objectType, column string, v *string) interface{} {
	if v == nil || *v == "" {
		return nil
	}
	if label, ok := choiceLabel(objectType, column, *v); ok {
		return label
	}
	return *v
}

// availableExpansions is the expand= keys of an entity among keys whose
// targets have data, in catalogue order: an unavailable one joins nothing.
func availableExpansions(e entity, keys ...string) []string {
	var out []string
	for _, col := range e.Columns {
		if col.Ref != nil && col.Ref.Available && slices.Contains(keys, col.Ref.ExpandKey) && !slices.Contains(out, col.Ref.ExpandKey) {
			out = append(out, col.Ref.ExpandKey)
		}
	}
	return out
}
