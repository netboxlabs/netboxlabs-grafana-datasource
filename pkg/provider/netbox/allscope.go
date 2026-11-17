package netbox

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A Scope of $__all is every object of the Scope type (resolvedScope.All). Its
// track is that type's own changes and the changes recorded against objects of
// it, read by type with no object ids, and, with Include descendants on, the
// types Include descendants follows, read by type too and checked as far as a
// row's snapshot can tell. None of the per-object reads run (descendantChanges):
// there are no objects to list.

// allDescendantTypes are the types Include descendants reads by type under All,
// per scope type; its keys are descendantKinds'. Interfaces and VM interfaces
// aren't among them: their changes are recorded against the device or VM, so
// the related read has them. Neither is an interface's cable: a cable's
// snapshot can't tell an interface's cable from a console or power port's, and
// the cable-termination rows the related read has tie cables to interfaces.
var allDescendantTypes = map[string][]string{
	"dcim.device":                   {cableType, ipAddressType, macAddressType, moduleType, serviceType},
	"virtualization.virtualmachine": {ipAddressType, macAddressType, serviceType},
	"dcim.interface":                {ipAddressType, macAddressType},
	"virtualization.vminterface":    {ipAddressType, macAddressType},
}

// allDescendants is which descendant types an All query reads: none without
// Include descendants, else the scope type's, less those Object types leaves
// out and those this NetBox doesn't have. Their checks need type ids, so
// without the type map none are read, and unreadable reports that some were
// wanted. Changes calls it once, for the reads and the warning both.
func allDescendants(scope *resolvedScope, spec provider.ChangeSpec, wanted []string) (types []string, unreadable bool) {
	if !spec.Descendants {
		return nil, false
	}
	types = slices.DeleteFunc(slices.Clone(allDescendantTypes[scope.Type]), func(t string) bool {
		return len(wanted) > 0 && !slices.Contains(wanted, t)
	})
	if len(types) == 0 {
		return nil, false
	}
	if scope.typeIDs == nil {
		return nil, true
	}
	return slices.DeleteFunc(types, func(t string) bool { return scope.typeIDs[t] == 0 }), false
}

// allScopeReads is an All query's change-log reads: the type read (the scope
// type's own changes and the descendant types, in one read) and the related
// read (the changes recorded against objects of the scope type). Both keep only
// some of what NetBox returns, so both are scans.
func (p *Provider) allScopeReads(ctx context.Context, spec provider.ChangeSpec, scope *resolvedScope, wanted []string, f changeFilter) ([]changeRead, error) {
	var reads []changeRead

	own := len(wanted) == 0 || slices.Contains(wanted, scope.Type)
	desc := scope.allDesc
	if own || len(desc) > 0 {
		q := changeLogQuery(spec, f)
		if own {
			if scope.TypeID != 0 {
				q.Add(changedObjectTypeIDParam, strconv.Itoa(scope.TypeID))
			} else {
				q.Set(changedObjectTypeParam, scope.Type)
			}
		}
		for _, t := range desc {
			q.Add(changedObjectTypeIDParam, strconv.Itoa(scope.typeIDs[t]))
		}
		kind := descendantKinds[scope.Type]
		ifaceTypeID := scope.typeIDs[kind.ifaceType]
		reads = append(reads, changeRead{q: q, scan: true, keep: func(oc objectChange) bool {
			t := normalizeObjectType(oc.changedType())
			if t == scope.Type {
				return own
			}
			return slices.Contains(desc, t) && allDescendantMatch(oc, t, scope, kind, ifaceTypeID)
		}})
	}

	// Rows are kept only when a snapshot ties them to the scope type
	// (tiedToScopeType). Without the type map only a device's or VM's rows can
	// be, by the field naming them (their components'); the others' tie by a
	// generic <x>_type pair, so the read isn't made, and the track says so
	// (resolvedScope.allPartial).
	if scope.TypeID != 0 || descendantKinds[scope.Type].ownerField != "" {
		trs, err := p.objectTypeReads(ctx, wanted, changedObjectTypeIDParam, changedObjectTypeParam)
		if err != nil {
			return nil, err
		}
		for _, tr := range trs {
			q := changeLogQuery(spec, f)
			q.Set(relatedObjectTypeParam, scope.Type)
			reads = append(reads, changeRead{q: tr.with(q), scan: true, fromRelated: true, relatedTypeID: scope.TypeID,
				keep: func(oc objectChange) bool {
					return tr.keeps(normalizeObjectType(oc.changedType())) && tiedToScopeType(scope, oc)
				}})
		}
	}
	return perAction(reads, f.pushes), nil
}

// anyObject accepts any object id: under All every object of the type counts.
func anyObject(int64) bool { return true }

// allDescendantMatch reports whether a row of a descendant type t is the scope
// type's, as far as its snapshots can tell: an IP or MAC address on the scope
// kind's interface type, a service whose parent is of the scope type. These are
// the scoped checks (assignedOwner, serviceParent) with any object accepted.
// Cables and modules are kept by type: modules only exist on devices, and a
// cable's snapshot names its ends by id alone, so a device track under All has
// every cable, the rare one between two circuits included (RECIPES says so).
func allDescendantMatch(oc objectChange, t string, scope *resolvedScope, kind descendantKind, ifaceTypeID int) bool {
	switch t {
	case ipAddressType, macAddressType:
		_, ok := assignedOwner(oc, ifaceTypeID, func(iface int64) (int64, bool) { return iface, true })
		return ok
	case serviceType:
		_, ok := serviceParent(oc, scope.TypeID, kind.ownerField, anyObject)
		return ok
	}
	return true
}

// tiedToScopeType reports whether a related read's row names an object of the
// scope type in a snapshot (snapshotRef, any object accepted). The change log
// doesn't return the related object, so this is what keeps a NetBox that
// ignored related_object_type from putting every change on the track.
func tiedToScopeType(scope *resolvedScope, oc objectChange) bool {
	_, ok := snapshotRef(scope, oc, anyObject)
	return ok
}

// allParent names the object of the scope type a row under All belongs to, for
// the marker's "on <name>": the display of the changed object's nested
// reference under the type's field (scopeParentFields), as an interface's or a
// module's brief row carries its device, or a circuit termination's its
// circuit. There is no object list to check it against. Interface scopes get
// none, since an interface's display doesn't name its device, as do the scope
// type's own rows, rows whose brief row names no parent (an IP or MAC address,
// a service) and deleted objects (no nested references).
func allParent(scope *resolvedScope, oc objectChange) string {
	field := scopeParentFields[scope.Type]
	if k, ok := descendantKinds[scope.Type]; field == "" || (ok && k.ownerField == "") ||
		normalizeObjectType(oc.changedType()) == scope.Type {
		return ""
	}
	var ref struct {
		Display string `json:"display"`
	}
	if json.Unmarshal(oc.changedObjectMap()[field], &ref) != nil {
		return ""
	}
	return ref.Display
}
