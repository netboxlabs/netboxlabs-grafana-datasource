package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// childListCapRows bounds a current-children list Include descendants reads:
// a scope's interfaces or cables. The list is up to 20 pages, and every
// interface's id then goes into the cable-end reads a few hundred to a request,
// so a scope with more is refused rather than followed.
const childListCapRows = 10000

// childListCap is childListCapRows unless a test has lowered it.
func (p *Provider) childListCap() int {
	if p.childListCapOverride > 0 {
		return p.childListCapOverride
	}
	return childListCapRows
}

// cableEndWorkers is how many cable-end batches run at once (R16): bounded,
// without a long serial chain on a device with thousands of interfaces.
const cableEndWorkers = 4

// maxDescendantsBudget caps the time the descendant reads get. They run in a
// chain (lists, then cable ends, then scans), and a short dashboard refresh
// must not lose the scope's own changes to them.
const maxDescendantsBudget = 8 * time.Second

// descendantsBudget is min(half the datasource Timeout, 8s), the Timeout being
// requestTimeout, else defaultRequestTimeout; or descendantsWaitOverride when a
// test set one.
func (p *Provider) descendantsBudget() time.Duration {
	if p.descendantsWaitOverride > 0 {
		return p.descendantsWaitOverride
	}
	timeout := p.requestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return min(timeout/2, maxDescendantsBudget)
}

// descendantsDeadline is now + descendantsBudget(), narrowed to leave a fifth of
// the caller's remaining time when ctx has a deadline (R15), as
// utilizationDeadline does: the rest of the track is merged and returned in
// that fifth, rather than lost with the caller's deadline.
func (p *Provider) descendantsDeadline(ctx context.Context) time.Time {
	now := time.Now()
	deadline := now.Add(p.descendantsBudget())
	if dl, ok := ctx.Deadline(); ok {
		if reserved := now.Add(dl.Sub(now) * 4 / 5); reserved.Before(deadline) {
			return reserved
		}
	}
	return deadline
}

// listChildren reads a current-children list. It reads page 1 itself
// (getListPageRetry) and fails with too-many-children when that page's count
// passes childListCap(), before reading another page; then it follows next
// links, failing the same way if the rows read pass the cap. A 403 on any page
// is descendants-forbidden.
//
// The rows read are checked too because the count can be missing or wrong,
// and because they bound the walk: a NetBox that ignores offset and serves the
// same page forever stops at the cap.
func (p *Provider) listChildren(ctx context.Context, endpoint string, q url.Values) ([]json.RawMessage, error) {
	limit := p.childListCap()
	pq := url.Values{}
	maps.Copy(pq, q)
	pq.Set("limit", strconv.Itoa(pageSize))
	next := p.client.apiURL(endpoint, pq)
	var rows []json.RawMessage
	for first := true; next != ""; first = false {
		page, err := p.getListPageRetry(ctx, next)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
				return nil, &AnnotationError{Reason: provider.AnnotationReasonDescendantsForbidden}
			}
			return nil, err
		}
		if n, ok := page.total(); first && ok && n > limit {
			return nil, &AnnotationError{Reason: provider.AnnotationReasonTooManyChildren}
		}
		if rows = append(rows, page.Results...); len(rows) > limit {
			return nil, &AnnotationError{Reason: provider.AnnotationReasonTooManyChildren}
		}
		// An empty page ends the list too, so a malformed next link can't loop.
		if page.Next == nil || len(page.Results) == 0 {
			break
		}
		if next, err = p.client.nextPageURL(next, *page.Next); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// descendantKind is what Include descendants follows from one scope type.
type descendantKind struct {
	ifaceType  string // the interface type whose addresses (and, on devices, cable ends) are followed
	scopeList  string // the scope type's list endpoint, for R11; "" for interface scopes
	ifaceList  string // the scope's current interfaces; "" when the scope objects are the interfaces
	ownerParam string // ifaceList's filter by scope object
	ownerField string // names the scope object in an interface row, an interface snapshot and a 4.2 service snapshot; "" for interface scopes
	cables     bool
	modules    bool
	services   bool
}

// descendantKinds are the scope types Include descendants applies to. Any other
// scope type reads what it read before.
var descendantKinds = map[string]descendantKind{
	"dcim.device": {
		ifaceType: "dcim.interface", scopeList: "dcim/devices", ifaceList: "dcim/interfaces",
		ownerParam: "device_id", ownerField: "device", cables: true, modules: true, services: true,
	},
	"virtualization.virtualmachine": {
		ifaceType: "virtualization.vminterface", scopeList: "virtualization/virtual-machines", ifaceList: "virtualization/interfaces",
		ownerParam: "virtual_machine_id", ownerField: "virtual_machine", services: true,
	},
	"dcim.interface":             {ifaceType: "dcim.interface", cables: true},
	"virtualization.vminterface": {ifaceType: "virtualization.vminterface"},
}

// The types and lists the cable membership reads name: an interface's cable
// ends, a device's cables, and an interface scope's own rows, which carry its
// current cable. Only devices and interfaces have cables.
const (
	cableTerminationType = "dcim.cabletermination"
	interfaceType        = "dcim.interface"
	interfaceList        = "dcim/interfaces"
	cableList            = "dcim/cables"
)

// The types the descendant scans read: addresses in one scan, the parts in
// another, so address churn can't use up the cap the parts share.
const (
	ipAddressType  = "ipam.ipaddress"
	macAddressType = "dcim.macaddress"
	cableType      = "dcim.cable"
	moduleType     = "dcim.module"
	serviceType    = "ipam.service"
)

// descendantChanges reads the scope's descendant changes over [From, To]: the
// membership reads' results, then the scans' results, every one with scan and
// descendant set. nil, nil when the scope type has no descendantKinds row or no
// descendant type survives Object types. The first error among its reads is
// returned.
//
// The scan types are decided before anything is read, so a query whose Object
// types leave none reads nothing, not even the type map. Children are read
// only for the scans that need them (R5), and a scan nothing can match is
// skipped: no addresses without interfaces, no cables without cables. Each scan
// pushes its types as changed_object_type_id, and keeps a row only when the
// row's own snapshot, or its cable id, ties it to the scope, whatever NetBox
// did with the filters.
func (p *Provider) descendantChanges(ctx context.Context, spec provider.ChangeSpec, scope *resolvedScope, wanted []string, f changeFilter, limit int) ([]changeReadResult, error) {
	kind, ok := descendantKinds[scope.Type]
	if !ok {
		return nil, nil
	}
	notWanted := func(t string) bool { return len(wanted) > 0 && !slices.Contains(wanted, t) }
	addrTypes := slices.DeleteFunc([]string{ipAddressType, macAddressType}, notWanted)
	applies := map[string]bool{cableType: kind.cables, moduleType: kind.modules, serviceType: kind.services}
	partTypes := slices.DeleteFunc([]string{cableType, moduleType, serviceType}, func(t string) bool { return !applies[t] || notWanted(t) })
	if len(addrTypes) == 0 && len(partTypes) == 0 {
		return nil, nil
	}

	ids, err := p.objectTypeIDMap(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &AnnotationError{Reason: provider.AnnotationReasonDescendantTypesUnreadable}
	}
	// A type the map lacks is a model this NetBox doesn't have (R6). Without the
	// interface type's id, no address can be matched to an interface.
	unknown := func(t string) bool { return ids[t] == 0 }
	addrTypes = slices.DeleteFunc(addrTypes, unknown)
	if ids[kind.ifaceType] == 0 {
		addrTypes = nil
	}
	partTypes = slices.DeleteFunc(partTypes, unknown)
	if len(addrTypes) == 0 && len(partTypes) == 0 {
		return nil, nil
	}

	// A device's cable ends are read through its interfaces (R5).
	wantCables := slices.Contains(partTypes, cableType)
	wantIfaces := len(addrTypes) > 0 || (wantCables && scope.Type == "dcim.device")
	m, err := p.descendantMembers(ctx, spec, scope, kind, ids, wantIfaces, wantCables)
	if err != nil {
		return nil, err
	}
	if len(m.cables) == 0 {
		partTypes = slices.DeleteFunc(partTypes, func(t string) bool { return t == cableType })
	}
	if len(m.ifaces) == 0 {
		addrTypes = nil
	}

	inScope := make(map[int64]bool, len(scope.Objects))
	for _, o := range scope.Objects {
		inScope[o.ID] = true
	}
	ifaceTypeID, scopeTypeID := ids[kind.ifaceType], ids[scope.Type]
	match := func(oc objectChange) (int64, bool) {
		switch normalizeObjectType(oc.changedType()) {
		case ipAddressType, macAddressType:
			return matchAssigned(oc, ifaceTypeID, m.ifaces)
		case cableType:
			owner, ok := m.cables[oc.ChangedObjectID]
			return owner, ok
		case moduleType:
			return matchOwner(oc, "device", inScope)
		case serviceType:
			return matchServiceParent(oc, scopeTypeID, kind.ownerField, inScope)
		}
		return 0, false
	}

	// The address scan, then the parts scan; each owns its result slot.
	scans := [][]string{addrTypes, partTypes}
	found := make([]changeReadResult, len(scans))
	var wg sync.WaitGroup
	for i, types := range scans {
		if len(types) == 0 {
			continue
		}
		q := changeLogQuery(spec, f)
		for _, t := range types {
			q.Add(changedObjectTypeIDParam, strconv.Itoa(ids[t]))
		}
		r := changeRead{q: q, descendant: func(oc objectChange) (int64, bool) {
			// A type the scan didn't ask for isn't kept, so a NetBox that ignores
			// the type filter can't widen it.
			if !slices.Contains(types, normalizeObjectType(oc.changedType())) {
				return 0, false
			}
			return match(oc)
		}}
		wg.Go(func() {
			found[i] = p.runChangeRead(ctx, r, f, limit, p.changeScanCap())
			found[i].scan, found[i].descendant = true, true
		})
	}
	wg.Wait()
	results := m.reads
	for i, types := range scans {
		if len(types) == 0 {
			continue
		}
		if found[i].err != nil {
			return nil, found[i].err
		}
		results = append(results, found[i])
	}
	return results, nil
}

// members is who the scope's children are over [From, now]. Each child is
// mapped to the scope object that owns it, which names it on the track.
type members struct {
	ifaces map[int64]int64    // interface id -> owning scope object id
	cables map[int64]int64    // cable id -> owning scope object id
	reads  []changeReadResult // membership reads (R9)
}

// descendantMembers finds the scope's interfaces and cables over [From, now]:
// the current ones from their lists, plus the ones the change log names since
// From: interfaces deleted, and cables whose ends were on the scope's
// interfaces. The range runs to now, not To, because a child that left after
// To still had its changes in the range.
//
// wantIfaces reads a device or VM scope's interfaces; an interface scope's are
// its own objects, read or not. wantCables reads a device or interface scope's
// cables. A device's cable ends are found through its interfaces, so a caller
// that wants a device's cables wants its interfaces too (R5).
//
// Stage 1 runs the interface list, the interface-delete read and the cable list
// at once; stage 2, the cable-end reads, needs the interfaces stage 1 found.
// Each read fills its own variable and the sets are built after each stage's
// Wait, so no map is written concurrently. Every list row and change-log row is
// checked against the scope here, whatever NetBox did with the filters. The
// first error, in that order, is returned as it came.
func (p *Provider) descendantMembers(ctx context.Context, spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, wantIfaces, wantCables bool) (members, error) {
	m := members{ifaces: map[int64]int64{}, cables: map[int64]int64{}}
	inScope := make(map[int64]bool, len(scope.Objects))
	scopeIDs := make([]string, 0, len(scope.Objects))
	for _, o := range scope.Objects {
		inScope[o.ID] = true
		scopeIDs = append(scopeIDs, strconv.FormatInt(o.ID, 10))
	}
	// changeReads refuses a scope without objects before this runs. Without ids
	// every list and read here would ask for all of NetBox.
	if len(scopeIDs) == 0 {
		return m, nil
	}
	ifaceScope := kind.ifaceList == ""
	readIfaces := wantIfaces && !ifaceScope
	wantCables = wantCables && kind.cables

	var (
		ifaceRows, cableRows []json.RawMessage
		ifaceErr, cableErr   error
		deleted              changeReadResult
		deleteRan            bool
	)
	var wg sync.WaitGroup
	if readIfaces {
		wg.Go(func() { ifaceRows, ifaceErr = p.currentInterfaces(ctx, spec, kind, inScope, scopeIDs) })
		// A type the map lacks is a model this NetBox doesn't have (R6).
		if typeID := ids[kind.ifaceType]; typeID != 0 {
			deleteRan = true
			r := interfaceDeleteRead(spec, scope, kind, ids, typeID, inScope, scopeIDs)
			wg.Go(func() { deleted = p.runMembershipRead(ctx, r) })
		}
	}
	if wantCables {
		wg.Go(func() {
			if ifaceScope {
				cableRows, cableErr = p.listChildren(ctx, interfaceList, briefQuery("id", scopeIDs))
				return
			}
			q := url.Values{"device_id": slices.Clone(scopeIDs), "fields": {"id,a_terminations,b_terminations"}}
			cableRows, cableErr = p.listChildren(ctx, cableList, q)
		})
	}
	wg.Wait()
	for _, err := range []error{ifaceErr, deleted.err, cableErr} {
		if err != nil {
			return members{}, err
		}
	}

	if ifaceScope {
		for _, o := range scope.Objects {
			m.ifaces[o.ID] = o.ID
		}
	}
	for _, raw := range ifaceRows {
		row := decodeObject(raw)
		id, ok := snapshotID(row, "id")
		owner, owned := refID(row, kind.ownerField)
		if ok && owned && inScope[owner] {
			addMember(m.ifaces, id, owner)
		}
	}
	if deleteRan {
		for _, d := range deleted.rows {
			// keep checked the owner; a row without the interface's id names none.
			owner, _ := snapshotID(d.oc.PrechangeData, kind.ownerField)
			if d.oc.ChangedObjectID > 0 {
				addMember(m.ifaces, d.oc.ChangedObjectID, owner)
			}
		}
		m.reads = append(m.reads, membershipResult(spec, deleted))
	}
	for _, raw := range cableRows {
		row := decodeObject(raw)
		if ifaceScope {
			id, ok := snapshotID(row, "id")
			cable, cabled := refID(row, "cable")
			if ok && cabled && inScope[id] {
				addMember(m.cables, cable, id)
			}
			continue
		}
		if id, ok := snapshotID(row, "id"); ok {
			if owner, owned := cableOwner(row, inScope); owned {
				addMember(m.cables, id, owner)
			}
		}
	}

	ifaceTypeID, endTypeID := ids[interfaceType], ids[cableTerminationType]
	if !wantCables || len(m.ifaces) == 0 || ifaceTypeID == 0 || endTypeID == 0 {
		return m, nil
	}
	ends := p.cableEndReads(ctx, spec, ifaceTypeID, endTypeID, m.ifaces)
	for _, r := range ends {
		if r.err != nil {
			return members{}, r.err
		}
	}
	for _, r := range ends {
		for _, d := range r.rows {
			// The after side first (R7). A cable the list found keeps the list's
			// owner, which addMember leaves in place.
			for _, data := range []map[string]json.RawMessage{d.oc.PostchangeData, d.oc.PrechangeData} {
				if cable, owner, ok := cableEndMember(data, ifaceTypeID, m.ifaces); ok {
					addMember(m.cables, cable, owner)
				}
			}
		}
		m.reads = append(m.reads, membershipResult(spec, r))
	}
	return m, nil
}

// currentInterfaces reads a device or VM scope's current interfaces. On a
// Match-by-ID scope it first reads which scope objects exist, and lists only
// those: the interface lists' device_id and virtual_machine_id are choice
// filters, and one deleted id makes the list a 400 (R11). A deleted object's
// interface deletes still come from the delete read.
func (p *Provider) currentInterfaces(ctx context.Context, spec provider.ChangeSpec, kind descendantKind, inScope map[int64]bool, scopeIDs []string) ([]json.RawMessage, error) {
	owners := scopeIDs
	if spec.ScopeMatch == scopeMatchID {
		rows, err := p.listChildren(ctx, kind.scopeList, briefQuery("id", scopeIDs))
		if err != nil {
			return nil, err
		}
		exists := map[string]bool{}
		for _, raw := range rows {
			if id, ok := snapshotID(decodeObject(raw), "id"); ok && inScope[id] {
				exists[strconv.FormatInt(id, 10)] = true
			}
		}
		owners = slices.DeleteFunc(slices.Clone(scopeIDs), func(id string) bool { return !exists[id] })
		if len(owners) == 0 {
			return nil, nil
		}
	}
	return p.listChildren(ctx, kind.ifaceList, briefQuery(kind.ownerParam, owners))
}

// interfaceDeleteRead is the read of the scope's interfaces deleted since From:
// the deletes of the interface type recorded against the scope objects. A row
// counts only when its own before data names a scope object as the
// interface's device or VM, so a NetBox that ignores related_object_id can't
// widen it.
func interfaceDeleteRead(spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, ifaceTypeID int, inScope map[int64]bool, scopeIDs []string) changeRead {
	q := membershipQuery(spec)
	q.Set(relatedObjectTypeParam, scope.Type)
	q[relatedObjectIDParam] = slices.Clone(scopeIDs)
	q.Set(changedObjectTypeIDParam, strconv.Itoa(ifaceTypeID))
	q.Set(actionParam, "delete")
	return changeRead{
		q: q,
		keep: func(oc objectChange) bool {
			owner, ok := snapshotID(oc.PrechangeData, kind.ownerField)
			return normalizeObjectType(oc.changedType()) == kind.ifaceType && oc.Action.Value == "delete" && ok && inScope[owner]
		},
		fromRelated:   true,
		relatedTypeID: ids[scope.Type],
	}
}

// cableEndReads reads the cable ends recorded against ifaces since From: a
// related read over the interfaces, not a scan of every cable end, since cable
// ends are recorded against their port (R4). The ids are sorted and batched to
// the URL budget, and at most cableEndWorkers batches run at once (R16). The
// results are in batch order.
func (p *Provider) cableEndReads(ctx context.Context, spec provider.ChangeSpec, ifaceTypeID, endTypeID int, ifaces map[int64]int64) []changeReadResult {
	fixed := membershipQuery(spec)
	fixed.Set(relatedObjectTypeParam, interfaceType)
	fixed.Set(changedObjectTypeIDParam, strconv.Itoa(endTypeID))
	batcher := newQueryBatcher(relatedObjectIDParam, fixed)
	var values []string
	for _, id := range slices.Sorted(maps.Keys(ifaces)) {
		values = append(values, strconv.FormatInt(id, 10))
	}
	keep := func(oc objectChange) bool { return normalizeObjectType(oc.changedType()) == cableTerminationType }

	batches := batcher.chunk(values)
	results := make([]changeReadResult, len(batches))
	sem := make(chan struct{}, cableEndWorkers)
	var wg sync.WaitGroup
	for i, batch := range batches {
		r := changeRead{q: batcher.query(batch), keep: keep, fromRelated: true, relatedTypeID: ifaceTypeID}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = p.runMembershipRead(ctx, r)
		})
	}
	wg.Wait()
	return results
}

// membershipQuery is the part every membership read shares: newest first, from
// From, and no end, since membership covers [From, now]. No Action is pushed:
// the query's filters apply to what the track shows, not to who the children
// are.
func membershipQuery(spec provider.ChangeSpec) url.Values {
	return changeLogQuery(provider.ChangeSpec{From: spec.From}, changeFilter{})
}

// runMembershipRead walks one membership read to changeScanCap rows (R9). Its
// limit is one more, so it never stops for having kept enough: any row it
// hasn't read may name a child.
func (p *Provider) runMembershipRead(ctx context.Context, r changeRead) changeReadResult {
	maxRows := p.changeScanCap()
	return p.runChangeRead(ctx, r, changeFilter{}, maxRows+1, maxRows)
}

// membershipResult makes a membership read's result what Changes merges (R9).
// Its rows only named children, so none may reach the track. It scans, so its
// cap can have missed children, and it's a descendant read. A capped read that
// stopped after To may have missed a child whose changes lie anywhere in the
// range, so its cut-off is To.
func membershipResult(spec provider.ChangeSpec, r changeReadResult) changeReadResult {
	r.rows = nil
	r.scan, r.descendant = true, true
	if r.capped && !spec.To.IsZero() && r.oldest.After(spec.To) {
		r.oldest = spec.To
	}
	return r
}

// cableOwner is the scope device a cable row has an end on: the first such
// termination, A side then B. A row whose ends are on no scope device has
// none.
func cableOwner(row map[string]json.RawMessage, inScope map[int64]bool) (int64, bool) {
	for _, side := range []string{"a_terminations", "b_terminations"} {
		var ends []map[string]json.RawMessage
		if json.Unmarshal(row[side], &ends) != nil {
			continue
		}
		for _, end := range ends {
			if device, ok := refID(decodeObject(end["object"]), "device"); ok && inScope[device] {
				return device, true
			}
		}
	}
	return 0, false
}

// cableEndMember reads one side of a cable-end row: its cable, owned by the
// owner of its interface, when the end is on one of ifaces. The match is on the
// termination's type and id together, so a console port that shares an
// interface's id doesn't match (R4).
func cableEndMember(data map[string]json.RawMessage, ifaceTypeID int, ifaces map[int64]int64) (cable, owner int64, ok bool) {
	termType, typed := snapshotID(data, "termination_type")
	termID, identified := snapshotID(data, "termination_id")
	cable, cabled := snapshotID(data, "cable")
	if !typed || !identified || !cabled || termType != int64(ifaceTypeID) {
		return 0, 0, false
	}
	owner, ok = ifaces[termID]
	return cable, owner, ok
}

// addMember records a child's owner unless the child already has one: the
// first read to name a child owns it (R7).
func addMember(set map[int64]int64, child, owner int64) {
	if _, ok := set[child]; !ok {
		set[child] = owner
	}
}

// matchAssigned matches an address row to one of ifaces: its assigned_object_type
// is the interface type's id and its assigned_object_id is in ifaces. The after
// side is read first, so an address moved between two scope objects is on the
// one it moved to (R7). The owner is the interface's.
func matchAssigned(oc objectChange, ifaceTypeID int, ifaces map[int64]int64) (owner int64, ok bool) {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		typ, typed := snapshotID(data, "assigned_object_type")
		id, identified := snapshotID(data, "assigned_object_id")
		if !typed || !identified || typ != int64(ifaceTypeID) {
			continue
		}
		if owner, ok = ifaces[id]; ok {
			return owner, true
		}
	}
	return 0, false
}

// matchOwner matches a row whose field names a scope object, as a module's
// device does: the after side first, then the before (R7). The owner is that
// object.
func matchOwner(oc objectChange, field string, scopeIDs map[int64]bool) (owner int64, ok bool) {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		if id, named := snapshotID(data, field); named && scopeIDs[id] {
			return id, true
		}
	}
	return 0, false
}

// matchServiceParent matches a service row to its parent scope object: from
// NetBox 4.3, parent_object_type is the scope type's id and parent_object_id a
// scope object; on 4.2, ownerField (device or virtual_machine) is a scope
// object. The after side first, then the before (R7).
func matchServiceParent(oc objectChange, scopeTypeID int, ownerField string, scopeIDs map[int64]bool) (owner int64, ok bool) {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		typ, typed := snapshotID(data, "parent_object_type")
		id, identified := snapshotID(data, "parent_object_id")
		if typed && identified && typ == int64(scopeTypeID) && scopeIDs[id] {
			return id, true
		}
		if id, named := snapshotID(data, ownerField); ownerField != "" && named && scopeIDs[id] {
			return id, true
		}
	}
	return 0, false
}

// descendantParent names the scope object a descendant row matched, for the
// marker title's "on <name>": with one scope object it is that object, else
// the one whose id is owner, else "".
func descendantParent(scope *resolvedScope, owner int64) string {
	if len(scope.Objects) == 1 {
		return scopeObjectName(scope, scope.Objects[0])
	}
	for _, o := range scope.Objects {
		if o.ID == owner {
			return scopeObjectName(scope, o)
		}
	}
	return ""
}

// briefQuery is brief=true plus param once per value.
func briefQuery(param string, values []string) url.Values {
	return url.Values{"brief": {"true"}, param: slices.Clone(values)}
}

// decodeObject decodes a JSON object's keys, or returns nil for anything else.
func decodeObject(raw json.RawMessage) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

// snapshotID reads an id from a change-log snapshot or a list row. It counts
// only as a positive plain JSON integer: a string, an object, null, a fraction,
// zero or a negative number names nothing, so it can't match a scope id by
// accident.
func snapshotID(data map[string]json.RawMessage, key string) (int64, bool) {
	var id int64
	if json.Unmarshal(data[key], &id) != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// refID is the id of the object a list row nests under key, as in
// {"device":{"id":17}}, counted as snapshotID counts one.
func refID(data map[string]json.RawMessage, key string) (int64, bool) {
	return snapshotID(decodeObject(data[key]), "id")
}
