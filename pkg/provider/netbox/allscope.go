package netbox

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

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
	"dcim.device":                   {"dcim.cable", "ipam.ipaddress", "dcim.macaddress", "dcim.module", "ipam.service"},
	"virtualization.virtualmachine": {"ipam.ipaddress", "dcim.macaddress", "ipam.service"},
	"dcim.interface":                {"ipam.ipaddress", "dcim.macaddress"},
	"virtualization.vminterface":    {"ipam.ipaddress", "dcim.macaddress"},
}

// wantedAllDescendants is which descendant types an All query reads: none
// without Include descendants, else the scope type's, less those Object types
// leaves out.
func wantedAllDescendants(scope *resolvedScope, spec provider.ChangeSpec, wanted []string) []string {
	if !spec.Descendants {
		return nil
	}
	types := slices.Clone(allDescendantTypes[scope.Type])
	if len(wanted) > 0 {
		types = slices.DeleteFunc(types, func(t string) bool { return !slices.Contains(wanted, t) })
	}
	return types
}

// allScopeReads is an All query's change-log reads: the type read (the scope
// type's own changes and the descendant types, in one read) and the related
// read (the changes recorded against objects of the scope type). Both keep only
// some of what NetBox returns, so both are scans. Without the type map
// (TypeID 0) the descendant types can't be checked and aren't read; Changes
// says so with the descendants warning.
func (p *Provider) allScopeReads(ctx context.Context, spec provider.ChangeSpec, scope *resolvedScope, wanted []string, f changeFilter) ([]changeRead, error) {
	var reads []changeRead

	own := len(wanted) == 0 || slices.Contains(wanted, scope.Type)
	var desc []string
	ids := map[string]int{}
	if scope.TypeID != 0 {
		if desc = wantedAllDescendants(scope, spec, wanted); len(desc) > 0 {
			var err error
			if ids, err = p.objectTypeIDMap(ctx); err != nil {
				return nil, err
			}
			// A type this NetBox doesn't have can't change.
			desc = slices.DeleteFunc(desc, func(t string) bool { return ids[t] == 0 })
		}
	}
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
			q.Add(changedObjectTypeIDParam, strconv.Itoa(ids[t]))
		}
		kind := descendantKinds[scope.Type]
		ifaceTypeID := ids[kind.ifaceType]
		reads = append(reads, changeRead{q: q, scan: true, keep: func(oc objectChange) bool {
			t := normalizeObjectType(oc.changedType())
			if t == scope.Type {
				return own
			}
			return slices.Contains(desc, t) && allDescendantMatch(oc, t, scope, kind, ifaceTypeID)
		}})
	}

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
	return perAction(reads, f.pushes), nil
}

// allDescendantMatch reports whether a row of a descendant type t is the scope
// type's, as far as its snapshots can tell: an IP or MAC address on the scope
// kind's interface type, a service whose parent is of the scope type (or, on
// NetBox 4.2, names one in the kind's owner field). Cables and modules are kept
// by type: a device's cables are all attached to it, and modules only exist on
// devices.
func allDescendantMatch(oc objectChange, t string, scope *resolvedScope, kind descendantKind, ifaceTypeID int) bool {
	switch t {
	case "ipam.ipaddress", "dcim.macaddress":
		return snapshotTypeIs(oc, "assigned_object_type", ifaceTypeID)
	case "ipam.service":
		return snapshotTypeIs(oc, "parent_object_type", scope.TypeID) ||
			(kind.ownerField != "" && snapshotNames(oc, kind.ownerField))
	}
	return true
}

// snapshotTypeIs reports whether either snapshot's key holds the object-type
// id typeID. An unknown id (0) matches nothing.
func snapshotTypeIs(oc objectChange, key string, typeID int) bool {
	if typeID == 0 {
		return false
	}
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		if id, ok := snapshotID(data, key); ok && id == int64(typeID) {
			return true
		}
	}
	return false
}

// snapshotNames reports whether either snapshot's field holds an id.
func snapshotNames(oc objectChange, field string) bool {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		if _, ok := snapshotID(data, field); ok {
			return true
		}
	}
	return false
}

// tiedToScopeType reports whether a related read's row names an object of the
// scope type in a snapshot: the type's own field (scopeParentFields) holding an
// id, or a generic <x>_type / <x>_id pair whose type is the scope's. The change
// log doesn't return the related object, so this is what keeps a NetBox that
// ignored related_object_type from putting every change on the track. A row
// that can't be tied (no named field, and the type map unreadable) is dropped.
func tiedToScopeType(scope *resolvedScope, oc objectChange) bool {
	field, named := scopeParentFields[scope.Type]
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		if _, ok := snapshotID(data, field); named && ok {
			return true
		}
		if scope.TypeID == 0 {
			continue
		}
		for key := range data {
			x, ok := strings.CutSuffix(key, "_type")
			if !ok {
				continue
			}
			if t, ok := snapshotID(data, key); ok && t == int64(scope.TypeID) {
				if _, ok := snapshotID(data, x+"_id"); ok {
					return true
				}
			}
		}
	}
	return false
}

// allParent names the device or VM a row under an All scope belongs to, for
// the marker's "on <name>", from the changed object's nested references: its
// owner field (an interface's device, a module's), the owner of the object it's
// assigned to (an IP's interface's device), or a service's parent. There is no
// object list to check them against, so the reference's display is the name.
// The scope type's own rows, interface scopes, and deleted objects (no nested
// references) get none.
func allParent(scope *resolvedScope, oc objectChange) string {
	kind := descendantKinds[scope.Type]
	if kind.ownerField == "" || normalizeObjectType(oc.changedType()) == scope.Type {
		return ""
	}
	obj := oc.changedObjectMap()
	refs := []json.RawMessage{obj[kind.ownerField]}
	var assigned map[string]json.RawMessage
	if json.Unmarshal(obj["assigned_object"], &assigned) == nil {
		refs = append(refs, assigned[kind.ownerField])
	}
	if normalizeObjectType(oc.changedType()) == "ipam.service" {
		refs = append(refs, obj["parent"])
	}
	for _, raw := range refs {
		var ref struct {
			Display string `json:"display"`
		}
		if json.Unmarshal(raw, &ref) == nil && ref.Display != "" {
			return ref.Display
		}
	}
	return ""
}
