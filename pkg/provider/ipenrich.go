package provider

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// The ip-enrichment contract both backends answer: which columns exist, what a
// new query selects, how an input is read as an address, how one record is
// picked out of several, and the sentences that describe the result. One copy,
// because a panel saved in one mode and pointed at the other has to keep its
// columns and its rows, and the editor offers exactly this vocabulary
// (src/types.ts, IP_CONTEXT_FIELD_GROUPS).

// IsPrimaryIPColumn is the column both identity lookups fill: a device's
// answer for an address on one of its interfaces, a virtual machine's for one
// on a VM interface. It carries no namespace (see IPEnrichColumns), so a gate
// that switches lookups on by prefix has to name it, and every place that
// names it reads this constant.
const IsPrimaryIPColumn = "is_primary_ip"

// IPEnrichColumns lists every column ip-enrichment can emit, in group order.
//
// "ip", "match_count" and "is_primary_ip" are deliberately not namespaced: "ip"
// is the documented Grafana join key, match_count describes the row itself, and
// is_primary_ip describes the address's relationship to whatever it is
// assigned to — a device for one row and a virtual machine for the next, so no
// single namespace could name it honestly (it was device_is_primary_ip until it
// gained a virtual-machine half).
//
// There is no prefix_site: NetBox 4.2 replaced a prefix's site with the
// generic scope (a site, region, site group or location), which is why 4.2 is
// the supported floor (README Requirements).
//
// interface_* stops at the name and description, and vm_* at the name: in
// NetBox mode the address payload embeds the brief interface (and, for a VM
// interface, the brief virtual machine), which carries nothing else worth a
// column, and any other attribute (a VM's cluster, site, role, platform or
// status) costs a request per address that was ruled not worth it.
// vm_name is not folded into device_name: a VM and a device live in different
// NetBox models, NetBox permits one of each with the same name, and the
// documented device_name join onto a Prometheus `device` label would then
// match the wrong thing. At most one of the two is set per row, and both are
// blank for an address assigned to neither (an FHRP group, or nothing).
//
// address_assigned_object_type is NetBox's relation name (dcim.interface,
// virtualization.vminterface, ipam.fhrpgroup), never a friendly label: it is
// what tells an FHRP-assigned address from an unassigned one, both of which
// leave interface_* and device_* blank.
//
// There is no address_nat_inside_dns or address_nat_outside_dns: NetBox's
// nested IP serializer, which carries both NAT sides, has no dns_name, so
// filling them would cost a request per NAT partner — ruled not worth it.
//
// Keep src/types.ts in step: pkg/provider/netbox's
// TestIPContextFieldsMatchFrontend compares the two.
func IPEnrichColumns() []string {
	return []string{
		"ip", "match_count", IsPrimaryIPColumn,
		"prefix_cidr", "prefix_scope", "prefix_tenant", "prefix_role",
		"prefix_vrf", "prefix_vlan", "prefix_description",
		"address_dns_name", "address_status", "address_role", "address_vrf",
		"address_tenant", "address_description", "address_assigned_object_type",
		"address_nat_inside", "address_nat_outside",
		"interface_name", "interface_description",
		"device_name", "device_role", "device_platform", "device_device_type",
		"device_site", "device_location", "device_rack", "device_tenant",
		"device_status",
		"vm_name",
	}
}

// defaultIPEnrichFields is what a new query selects. match_count is included
// on purpose: an ambiguous pick must be visible out of the box.
var defaultIPEnrichFields = []string{
	"ip", "match_count", "address_dns_name", "device_name", "vm_name", "interface_name",
	IsPrimaryIPColumn, "device_site", "device_tenant",
}

// DefaultIPEnrichFields returns the selection an empty field list stands for —
// which is what the query editor sends until the user first changes the
// multi-select, so it is the common case rather than an edge one.
//
// The plugin layer reads it too, to know the EFFECTIVE selection before the
// call and so whether a join key's source is already coming back
// (pkg/plugin.ipEnrichFields); a second copy there could disagree with this one
// and silently un-request a column. A clone, because a package-level slice
// handed out by value is one the caller can rewrite.
func DefaultIPEnrichFields() []string { return slices.Clone(defaultIPEnrichFields) }

// ipEnrichColumnTypes declares the two ip-enrichment columns that are not
// strings. This schema is fixed in code, so a column's type is known even when
// the current IP set puts no value in it — and an FHRP-only, unassigned-only or
// external-address refresh puts none in is_primary_ip, which would otherwise be
// typed from its values and alternate between a boolean field and a string one
// from refresh to refresh. See Result.ColumnTypes.
//
// Every other column is a string, which is also the inference fallback, so
// listing them would change nothing and only invite drift.
var ipEnrichColumnTypes = map[string]FieldType{
	"match_count":     FieldTypeNumber,
	IsPrimaryIPColumn: FieldTypeBoolean,
}

// DeclaredIPEnrichColumnTypes returns the type declarations for the fields a
// query actually returns, so Result.ColumnTypes never describes a column the
// frame does not have. nil when none of them is selected.
func DeclaredIPEnrichColumnTypes(fields []string) map[string]FieldType {
	var out map[string]FieldType
	for _, f := range fields {
		t, ok := ipEnrichColumnTypes[f]
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]FieldType, len(ipEnrichColumnTypes))
		}
		out[f] = t
	}
	return out
}

// NormalizeIPEnrichArgs applies the argument rules both sources share: an
// empty field list means the default set, "ip" is always a column (the
// documented Grafana join key — a frame without it has no key to join on), and
// the limit falls back to defaultLimit when unset and is clamped to maxLimit,
// as an objects query's is. The limits are the caller's own, since each
// backend states its ceiling (Result.MaxRows).
func NormalizeIPEnrichArgs(fields []string, limit, defaultLimit, maxLimit int) ([]string, int) {
	if len(fields) == 0 {
		fields = DefaultIPEnrichFields()
	}
	if !slices.Contains(fields, "ip") {
		fields = append([]string{"ip"}, fields...)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	return fields, limit
}

// HumanInt formats a count with thousands separators, matching the notices the
// plugin layer writes (pkg/plugin/notices.go's thousands) — these strings land
// side by side in the same panel header, and "1000" next to "1,000" reads as a
// typo.
func HumanInt(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// AmbiguityNote states that some rows were picked out of several address
// records. match_count says so per row, but it is an opt-in column: a user who
// deselects it gets one arbitrary-looking device for an anycast address with
// nothing to suggest the pick was a pick. A note, not a warning: the pick is
// deterministic and documented, so the row is correct and stable.
func AmbiguityNote(ambiguous, rows int) string {
	return fmt.Sprintf(
		"%s of %s rows matched more than one NetBox address record (an anycast address, or a VIP shared across a redundant pair). One record was picked deterministically — interface-assigned first, then any other assignment, then non-deprecated, then lowest id — so the row is stable; add the match_count column to see which rows.",
		HumanInt(ambiguous), HumanInt(rows))
}

// The tiers PickAddressRank ranks a record by. Spread apart so the status
// tie-break (+1) only ever orders records WITHIN a tier: an assignment must
// never be outvoted by a status, because only an assignment can carry
// identity.
const (
	rankInterfaceAssigned = 4
	rankOtherAssigned     = 2
)

// PickAddressRank ranks one of several address records for the same host
// (anycast, VRF overlap, a VIP shared across a pair): interface-assigned first,
// then any other assignment, then non-deprecated. The highest rank is the pick
// and the lowest id breaks a tie, which the caller applies; match_count reports
// how many records there were, so the pick is never silent.
//
// interfaceAssigned is an assignment to an interface of either kind (a device's
// or a virtual machine's), the only kind that can fill interface_* and
// device_*; otherAssigned is an assignment to anything, so it decides only
// between records that are not on an interface. Ranking every assignment alike
// handed a host to whichever record was created first, and when that was an
// FHRP group's the row came back with blank identity while a device-backed
// record sat unused beside it. A non-interface assignment still edges out none
// at all: neither names a device, but an assigned record documents an address
// something uses, where an unassigned one may be a bare reservation.
//
// Assignment outranks status: an interface-assigned but deprecated record
// still names its device and interface, with address_status saying
// "Deprecated" in plain sight, which is more than an active record that can
// name neither.
func PickAddressRank(interfaceAssigned, otherAssigned, deprecated bool) int {
	rank := 0
	switch {
	case interfaceAssigned:
		rank += rankInterfaceAssigned
	case otherAssigned:
		rank += rankOtherAssigned
	}
	if !deprecated {
		rank++
	}
	return rank
}

// CanonicalIPOK reduces an input to the ONE spelling the address lookup and
// the by-host index agree on, and says whether it was an address at all.
//
// The mask is dropped — it is not part of the identity NetBox matches an
// address on, so "10.20.0.1/32" finds a record stored as "10.20.0.1/24" — and
// the host is re-rendered by netip, so every spelling of one IPv6 address
// (uppercase, zero-expanded, masked) collapses to one key. An IPv4-mapped IPv6
// literal ("::ffff:10.20.0.1") is unmapped onto the IPv4 address it denotes,
// which is the same record in NetBox.
//
// Not an address: anything netip cannot parse, and a zoned IPv6 value
// ("fe80::1%eth0") — an inet carries no zone, so no record could ever match it.
// Such a value is never sent (a list-valued host match refuses the whole list
// for one bad element) and resolves to no record. Its key is the input less
// any mask, a string no record's canonical form can equal.
func CanonicalIPOK(addr string) (string, bool) {
	host := hostOf(addr)
	if a, err := netip.ParseAddr(host); err == nil {
		if a = a.Unmap(); a.Zone() == "" {
			return a.String(), true
		}
	}
	return host, false
}

// hostOf strips a CIDR mask from a trimmed input. It is a string operation,
// not a parse; CanonicalIPOK is what decides what the result means.
func hostOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, "/"); i >= 0 {
		return addr[:i]
	}
	return addr
}

// PrefixQuery is the containment value the prefix fallback asks about for an
// input, or false for a value that is not an address (nothing is asked; no
// prefix contains such a value, so the row is the same). It follows NetBox's
// own two rules for its contains filter: a bare value is strict (a prefix
// equal to the address is left out), and a masked one is inclusive on the
// value's network. The host is what is asked about, in the rule its spelling
// chose: a bare value stays bare, and a masked one becomes the host's own
// single-host prefix (10.1.2.5/32, /128 for IPv6), whose inclusive answer is
// every prefix holding the host, an equal one included. Asking about the
// caller's network instead missed a /30 holding a host written "10.1.2.5/24",
// and "::ffff:10.1.2.5" matched no IPv4 prefix.
func PrefixQuery(ip string) (string, bool) {
	host, ok := CanonicalIPOK(ip)
	if !ok {
		return "", false
	}
	if !strings.Contains(ip, "/") {
		return host, true
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return "", false
	}
	return netip.PrefixFrom(a, a.BitLen()).String(), true
}
