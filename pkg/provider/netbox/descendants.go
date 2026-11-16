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

// ChildListCapRows bounds a current-children list Include descendants reads:
// a scope's interfaces or cables. The list is up to 20 pages, and every
// interface's id then goes into the cable-end reads a few hundred to a request,
// so a scope with more is refused rather than followed. It is exported because
// pkg/plugin's annotationErrorMessage states it in the refusal.
const ChildListCapRows = 10000

// childListCap is ChildListCapRows unless a test has lowered it.
func (p *Provider) childListCap() int {
	if p.childListCapOverride > 0 {
		return p.childListCapOverride
	}
	return ChildListCapRows
}

// cableEndWorkers is how many membership requests (the lists, and the
// cable-end, IP-history and MAC batches) run at once: bounded, without a long
// serial chain on a device with thousands of interfaces.
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
// the caller's remaining time when ctx has a deadline, as utilizationDeadline
// does: the rest of the track is merged and returned in that fifth, rather
// than lost with the caller's deadline.
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
	scopeList  string // the scope type's list endpoint, to find which Match-by-ID objects exist; "" for interface scopes
	ifaceList  string // the scope's current interfaces; "" when the scope objects are the interfaces
	ownerParam string // ifaceList's filter by scope object
	ownerField string // names the scope object in an interface row, an interface snapshot and a 4.2 service snapshot; "" for interface scopes
	cables     bool
	modules    bool
	services   bool
}

// descendantKinds are the scope types Include descendants applies to. Any other
// scope type reads what it read before. Its keys MUST be changed together with
// DESCENDANT_SCOPE_TYPES (src/types.ts), which shows the switch:
// TestDescendantScopeTypesMatchFrontend fails when they differ.
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

// The lists a device's modules, a device's or VM's services and the scope's
// interfaces' MACs are read from.
const (
	moduleList     = "dcim/modules"
	serviceList    = "ipam/services"
	macAddressList = "dcim/mac-addresses"
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
// returned, and each read that fails calls fail with its error as it does, so
// the caller can stop the others (Changes cancels ctx with it).
//
// The scan types are decided before anything is read, so a query whose Object
// types leave none reads nothing, not even the type map. Children are read
// only for the scans that need them, and a scan nothing can match is skipped:
// no addresses without interfaces, no cables without cables. Each scan pushes
// its types as changed_object_type_id, and keeps a row only when the row's own
// snapshot, or its cable id, ties it to the scope, whatever NetBox did with
// the filters.
func (p *Provider) descendantChanges(ctx context.Context, fail context.CancelCauseFunc, spec provider.ChangeSpec, scope *resolvedScope, wanted []string, f changeFilter, limit int) ([]changeReadResult, error) {
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
	// A type the map lacks is a model this NetBox doesn't have. Without the
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

	// Membership is read for the types the scans read. A device's cable ends
	// are read through its interfaces, so its cables need them read too.
	want := descendantWants{
		ips:      slices.Contains(addrTypes, ipAddressType),
		macs:     slices.Contains(addrTypes, macAddressType),
		cables:   slices.Contains(partTypes, cableType),
		modules:  slices.Contains(partTypes, moduleType),
		services: slices.Contains(partTypes, serviceType),
	}
	want.ifaces = want.ips || want.macs || (want.cables && scope.Type == "dcim.device")
	// One semaphore bounds every membership request, so a scope with many
	// interfaces can't open more than cableEndWorkers at once.
	sem := make(chan struct{}, cableEndWorkers)
	m, err := p.descendantMembers(ctx, fail, sem, spec, scope, kind, ids, want)
	if err != nil {
		return nil, err
	}
	if len(m.cables) == 0 {
		partTypes = slices.DeleteFunc(partTypes, func(t string) bool { return t == cableType })
	}
	if len(m.ifaces) == 0 {
		addrTypes = nil
	}

	inScope := scope.idSet()
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
			if found[i].err != nil {
				fail(found[i].err)
			}
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

// descendantWants is which descendant types a query reads, after Object types,
// the scope kind and the type map.
type descendantWants struct {
	ifaces   bool // read the scope's interfaces (current and deleted since From): for addresses, and a device's cable ends
	ips      bool
	macs     bool
	cables   bool
	modules  bool
	services bool
}

// members is who the scope's children are over [From, now]. Each set maps a
// child's id to the scope object that owns it, which names it on the track.
type members struct {
	ifaces   map[int64]int64 // interface id -> owning scope object id
	cables   map[int64]int64
	ips      map[int64]int64
	macs     map[int64]int64
	modules  map[int64]int64
	services map[int64]int64
	// degraded: a MAC, module or service list was refused (401/403) or over
	// its cap, so that type is left to the members the change log names (a
	// module's, from its interfaces' deletes) and the scan.
	degraded bool
	reads    []changeReadResult // membership reads, rows cleared, for the cut-off (membershipResult)
}

// descendantMembers finds the scope's children over [From, now], for the types
// want names: the current ones from their lists, plus the ones the change log
// names since From: interfaces deleted, the modules those interfaces were on,
// and cables whose ends were on the scope's interfaces. IPs are the ones with
// a change recorded against the scope's interfaces at any time (ipHistoryReads),
// and MACs the ones on them now (macMembers). The range runs to now, not To,
// because a child that left after To still had its changes in the range.
//
// want.ifaces reads a device or VM scope's interfaces; an interface scope's are
// its own objects, read or not. Each part type is read only where the kind has
// it, whatever the caller wants: cables on devices and interfaces, modules on
// devices, services on devices and VMs. A device's cable ends, and any scope's
// IPs and MACs, are found through its interfaces, so a caller that wants them
// wants its interfaces too.
//
// Stage 1 runs the interface list, the interface-delete read and the cable,
// module and service lists at once. Stage 2 runs the reads keyed by the
// interfaces stage 1 found at once: the cable ends, the IP history and the MAC
// list. Every request runs under a slot of sem, and none holds a slot while it
// waits for another, so no slot sits idle. Each read fills its own variables
// and the sets are built after each stage's Wait, in a fixed order, so no map
// is written concurrently and the owners don't depend on timing. Every list
// row and change-log row is checked against the scope here, whatever NetBox
// did with the filters.
//
// A MAC, module or service list refused (401 or 403) or over the cap leaves
// only its own type short: members is degraded, with no error and no fail. Any
// other failure is returned, the first in a fixed order, as it came, and each
// read that fails calls fail as it does (descendantChanges).
func (p *Provider) descendantMembers(ctx context.Context, fail context.CancelCauseFunc, sem chan struct{}, spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, want descendantWants) (members, error) {
	m := members{
		ifaces: map[int64]int64{}, cables: map[int64]int64{}, ips: map[int64]int64{},
		macs: map[int64]int64{}, modules: map[int64]int64{}, services: map[int64]int64{},
	}
	inScope, scopeIDs := scope.idSet(), scope.idStrings()
	// changeReads refuses a scope without objects before this runs. Without ids
	// every list and read here would ask for all of NetBox.
	if len(scopeIDs) == 0 {
		return m, nil
	}
	ifaceScope := kind.ifaceList == ""
	readIfaces := want.ifaces && !ifaceScope
	wantCables := want.cables && kind.cables
	wantModules := want.modules && kind.modules
	wantServices := want.services && kind.services
	// The deletes name the interfaces, and the modules they were on. A type the
	// map lacks is a model this NetBox doesn't have: no deletes to read.
	readDeletes := (readIfaces || wantModules) && !ifaceScope && ids[kind.ifaceType] != 0

	// slotted runs read under a slot of sem. A slot it can't take, because ctx
	// ended, is read's error.
	slotted := func(read func() error) error {
		if err := takeSlot(ctx, sem); err != nil {
			return err
		}
		defer func() { <-sem }()
		return read()
	}
	// existing is the scope objects the device- and VM-keyed lists may name,
	// read at most once for all of them. Only a Match-by-ID scope reads, so only
	// it takes a slot; a list asks before it takes its own, so none holds one
	// while it waits.
	existing := sync.OnceValues(func() ([]string, error) {
		if spec.ScopeMatch != scopeMatchID {
			return scopeIDs, nil
		}
		var owners []string
		err := slotted(func() (err error) {
			owners, err = p.existingScopeIDs(ctx, spec, kind, inScope, scopeIDs)
			return err
		})
		return owners, err
	})
	// listExisting runs list over the scope objects that exist, under a slot.
	// With none, it lists nothing: an empty filter would list all of NetBox.
	listExisting := func(list func(owners []string) error) error {
		owners, err := existing()
		if err != nil || len(owners) == 0 {
			return err
		}
		return slotted(func() error { return list(owners) })
	}

	var (
		ifaceRows, cableRows                                 []json.RawMessage
		listedModules, listedServices                        map[int64]int64
		deleted                                              changeReadResult
		ifaceErr, deleteErr, cableErr, moduleErr, serviceErr error
		moduleRefused, serviceRefused                        bool
	)
	failed := func(err error) {
		if err != nil {
			fail(err)
		}
	}
	// ownRefusal splits a list's error into its own refusal, which fails only
	// its type and so doesn't stop the others, and any other error. The refusal
	// must come while ctx is live: once ctx has ended, a refusal-shaped error is
	// its cause, another read's failure (the interface list's 403, or the
	// scope's own read's 401) handed to a list it cut.
	ownRefusal := func(err error) (bool, error) {
		if listRefused(err) && ctx.Err() == nil {
			return true, nil
		}
		return false, err
	}
	var wg sync.WaitGroup
	if readIfaces {
		wg.Go(func() {
			ifaceErr = listExisting(func(owners []string) (err error) {
				ifaceRows, err = p.currentInterfaces(ctx, kind, owners)
				return err
			})
			failed(ifaceErr)
		})
	}
	if readDeletes {
		r := interfaceDeleteRead(spec, scope, kind, ids, ids[kind.ifaceType], inScope, scopeIDs)
		wg.Go(func() {
			deleteErr = slotted(func() error {
				deleted = p.runMembershipRead(ctx, r)
				return deleted.err
			})
			failed(deleteErr)
		})
	}
	if wantCables {
		wg.Go(func() {
			cableErr = slotted(func() (err error) {
				if ifaceScope {
					cableRows, err = p.listChildren(ctx, interfaceList, briefQuery("id", scopeIDs))
				} else {
					q := url.Values{"device_id": slices.Clone(scopeIDs), "fields": {"id,a_terminations,b_terminations"}}
					cableRows, err = p.listChildren(ctx, cableList, q)
				}
				return err
			})
			failed(cableErr)
		})
	}
	if wantModules {
		wg.Go(func() {
			moduleRefused, moduleErr = ownRefusal(listExisting(func(owners []string) (err error) {
				listedModules, err = p.moduleMembers(ctx, owners, inScope)
				return err
			}))
			failed(moduleErr)
		})
	}
	if wantServices {
		wg.Go(func() {
			serviceRefused, serviceErr = ownRefusal(listExisting(func(owners []string) (err error) {
				listedServices, err = p.serviceMembers(ctx, scope.Type, kind, owners, inScope)
				return err
			}))
			failed(serviceErr)
		})
	}
	wg.Wait()
	for _, err := range []error{ifaceErr, deleteErr, cableErr, moduleErr, serviceErr} {
		if err != nil {
			return members{}, err
		}
	}
	m.degraded = moduleRefused || serviceRefused

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
	// The listed modules first, so a module the list found keeps the list's
	// owner, which addMember leaves in place.
	maps.Copy(m.modules, listedModules)
	if readDeletes {
		for _, d := range deleted.rows {
			// keep checked the owner; a row without the interface's id names none.
			owner, _ := snapshotID(d.oc.PrechangeData, kind.ownerField)
			if readIfaces && d.oc.ChangedObjectID > 0 {
				addMember(m.ifaces, d.oc.ChangedObjectID, owner)
			}
			// A module deleted along with its interfaces is gone from the list:
			// only their deletes still name it.
			if module, ok := snapshotID(d.oc.PrechangeData, "module"); ok && wantModules {
				addMember(m.modules, module, owner)
			}
		}
		m.reads = append(m.reads, membershipResult(spec, deleted))
	}
	maps.Copy(m.services, listedServices)
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

	// Stage 2 reads by the interfaces. Without any it reads nothing: an empty
	// id filter would ask for all of NetBox. Cables are only on devices and
	// interfaces, whose interface type is dcim.interface. A type the map lacks
	// is a model this NetBox doesn't have: nothing to read for it.
	if len(m.ifaces) == 0 {
		return m, nil
	}
	endIfaceTypeID, endTypeID := ids[interfaceType], ids[cableTerminationType]
	ifaceTypeID, ipTypeID := ids[kind.ifaceType], ids[ipAddressType]
	var (
		ends, history []changeReadResult
		listedMACs    map[int64]int64
		macErr        error
		macRefused    bool
	)
	if wantCables && endIfaceTypeID != 0 && endTypeID != 0 {
		wg.Go(func() { ends = p.cableEndReads(ctx, fail, sem, spec, endIfaceTypeID, endTypeID, m.ifaces) })
	}
	if want.ips && ifaceTypeID != 0 && ipTypeID != 0 {
		wg.Go(func() { history = p.ipHistoryReads(ctx, fail, sem, kind.ifaceType, ifaceTypeID, ipTypeID, m.ifaces) })
	}
	if want.macs && ids[macAddressType] != 0 {
		wg.Go(func() {
			macs, err := p.macMembers(ctx, sem, kind.ifaceType, m.ifaces)
			listedMACs = macs
			macRefused, macErr = ownRefusal(err)
			failed(macErr)
		})
	}
	wg.Wait()
	for _, r := range slices.Concat(ends, history) {
		if r.err != nil {
			return members{}, r.err
		}
	}
	if macErr != nil {
		return members{}, macErr
	}

	for _, r := range ends {
		for _, d := range r.rows {
			// The after side first, so an end that moved names the interface
			// it's on now. A cable the list found keeps the list's owner, which
			// addMember leaves in place.
			for _, data := range []map[string]json.RawMessage{d.oc.PostchangeData, d.oc.PrechangeData} {
				if cable, owner, ok := cableEndMember(data, endIfaceTypeID, m.ifaces); ok {
					addMember(m.cables, cable, owner)
				}
			}
		}
		m.reads = append(m.reads, membershipResult(spec, r))
	}
	for _, r := range history {
		for _, d := range r.rows {
			// The row's own snapshot names the interface, type and id together,
			// whatever NetBox did with related_object_id.
			if owner, ok := matchAssigned(d.oc, ifaceTypeID, m.ifaces); ok && d.oc.ChangedObjectID > 0 {
				addMember(m.ips, d.oc.ChangedObjectID, owner)
			}
		}
		m.reads = append(m.reads, ipHistoryResult(spec, r))
	}
	// A refused list left listedMACs nil: no MACs, and the type degraded.
	maps.Copy(m.macs, listedMACs)
	m.degraded = m.degraded || macRefused
	return m, nil
}

// takeSlot takes a slot of sem, or returns context.Cause(ctx) once ctx has
// ended. A non-nil return holds no slot: ctx is checked first, and a slot the
// select won as ctx ended is given back. The caller stores a non-nil return
// as its read's error, never a result with no rows and no error, and releases
// the slot (<-sem) only after a nil return.
func takeSlot(ctx context.Context, sem chan struct{}) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	select {
	case sem <- struct{}{}:
		if ctx.Err() != nil {
			<-sem
			return context.Cause(ctx)
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// listRefused reports a list error that fails only the list's own type: the
// token may not read it (a 401, or listChildren's descendants-forbidden for a
// 403), or it is over listChildren's cap. nil is not refused.
func listRefused(err error) bool {
	var annErr *AnnotationError
	if errors.As(err, &annErr) {
		return annErr.Reason == provider.AnnotationReasonDescendantsForbidden ||
			annErr.Reason == provider.AnnotationReasonTooManyChildren
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// existingScopeIDs is the scope objects of scopeIDs that exist now. On a
// Match-by-ID scope it reads them from the scope type's list: the device_id
// and virtual_machine_id filters of the interface and module lists (and the
// service list's, on NetBox 4.2) are choice filters, and one deleted id makes
// the list a 400. A scope matched by name was just looked up, so its ids are
// returned as they are. A deleted object's interface deletes, and the modules
// they name, still come from the delete read.
func (p *Provider) existingScopeIDs(ctx context.Context, spec provider.ChangeSpec, kind descendantKind, inScope map[int64]bool, scopeIDs []string) ([]string, error) {
	if spec.ScopeMatch != scopeMatchID {
		return scopeIDs, nil
	}
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
	return slices.DeleteFunc(slices.Clone(scopeIDs), func(id string) bool { return !exists[id] }), nil
}

// currentInterfaces lists the current interfaces of owners, the device or VM
// scope objects that exist (existingScopeIDs).
func (p *Provider) currentInterfaces(ctx context.Context, kind descendantKind, owners []string) ([]json.RawMessage, error) {
	return p.listChildren(ctx, kind.ifaceList, briefQuery(kind.ownerParam, owners))
}

// moduleMembers lists the modules on owners, the scope devices that exist:
// each module whose device is in the scope, owned by that device.
func (p *Provider) moduleMembers(ctx context.Context, owners []string, inScope map[int64]bool) (map[int64]int64, error) {
	q := url.Values{"device_id": slices.Clone(owners), "fields": {"id,device"}}
	rows, err := p.listChildren(ctx, moduleList, q)
	if err != nil {
		return nil, err
	}
	set := map[int64]int64{}
	for _, raw := range rows {
		row := decodeObject(raw)
		id, ok := snapshotID(row, "id")
		owner, owned := refID(row, "device")
		if ok && owned && inScope[owner] {
			addMember(set, id, owner)
		}
	}
	return set, nil
}

// serviceMembers lists the services on owners, the device or VM scope objects
// that exist: each service whose parent is in the scope, owned by that parent.
// fields names the parent in both shapes, NetBox 4.3's parent object and 4.2's
// device and virtual_machine; NetBox leaves out a name its serializer doesn't
// have.
func (p *Provider) serviceMembers(ctx context.Context, scopeType string, kind descendantKind, owners []string, inScope map[int64]bool) (map[int64]int64, error) {
	q := url.Values{kind.ownerParam: slices.Clone(owners), "fields": {"id,parent_object_type,parent_object_id,device,virtual_machine"}}
	rows, err := p.listChildren(ctx, serviceList, q)
	if err != nil {
		return nil, err
	}
	set := map[int64]int64{}
	for _, raw := range rows {
		row := decodeObject(raw)
		id, ok := snapshotID(row, "id")
		owner, owned := serviceListOwner(row, scopeType, kind.ownerField, inScope)
		if ok && owned {
			addMember(set, id, owner)
		}
	}
	return set, nil
}

// serviceListOwner is the scope object a service list row is on. From NetBox
// 4.3 a row names its parent as parent_object_type, an app.model string that
// must be scopeType, so a VM doesn't match a device that shares its id, and
// parent_object_id. On 4.2 it nests its device or VM under ownerField.
func serviceListOwner(row map[string]json.RawMessage, scopeType, ownerField string, inScope map[int64]bool) (int64, bool) {
	var parentType string
	if json.Unmarshal(row["parent_object_type"], &parentType) == nil && normalizeObjectType(parentType) == scopeType {
		if id, ok := snapshotID(row, "parent_object_id"); ok && inScope[id] {
			return id, true
		}
	}
	if id, ok := refID(row, ownerField); ok && inScope[id] {
		return id, true
	}
	return 0, false
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
// ends are recorded against their port. The results are in batch order
// (ifaceRelatedReads).
func (p *Provider) cableEndReads(ctx context.Context, fail context.CancelCauseFunc, sem chan struct{}, spec provider.ChangeSpec, ifaceTypeID, endTypeID int, ifaces map[int64]int64) []changeReadResult {
	fixed := membershipQuery(spec)
	fixed.Set(relatedObjectTypeParam, interfaceType)
	fixed.Set(changedObjectTypeIDParam, strconv.Itoa(endTypeID))
	keep := func(oc objectChange) bool { return normalizeObjectType(oc.changedType()) == cableTerminationType }
	return p.ifaceRelatedReads(ctx, fail, sem, fixed, keep, ifaceTypeID, ifaces)
}

// ipHistoryReads reads the IP changes recorded against ifaces, newest first and
// with no start, batched over related_object_id like cableEndReads but as
// separate requests, each taking a slot of sem: IP churn on an interface can't
// use up the cap its cable ends are read to. NetBox records an IP's create and
// edits against the interface it's on after them, and its delete against the
// one it was on, so these rows name every IP that was on ifaces in the change
// history NetBox keeps. There is no start because an IP moved off or
// unassigned in the range is recorded against where it went, or nothing: only
// its older changes name it. ifaceType is the scope's interface type, the form
// related_object_type takes from NetBox 4.4.4, and ifaceTypeID the form it
// takes up to 4.4.3, for the retry.
func (p *Provider) ipHistoryReads(ctx context.Context, fail context.CancelCauseFunc, sem chan struct{}, ifaceType string, ifaceTypeID, ipTypeID int, ifaces map[int64]int64) []changeReadResult {
	fixed := url.Values{}
	fixed.Set(orderingParam, "-time")
	fixed.Set(relatedObjectTypeParam, ifaceType)
	fixed.Set(changedObjectTypeIDParam, strconv.Itoa(ipTypeID))
	keep := func(oc objectChange) bool { return normalizeObjectType(oc.changedType()) == ipAddressType }
	return p.ifaceRelatedReads(ctx, fail, sem, fixed, keep, ifaceTypeID, ifaces)
}

// ipHistoryResult is membershipResult, except that a read capped after passing
// From (oldest strictly before a non-zero From) isn't capped: it read every IP
// change recorded in the range, so in-range membership is complete, and only
// older history is left to the scan. A read whose rows' times didn't parse
// (oldest zero) hasn't shown it passed From, and stays capped.
func ipHistoryResult(spec provider.ChangeSpec, r changeReadResult) changeReadResult {
	if r.capped && !spec.From.IsZero() && !r.oldest.IsZero() && r.oldest.Before(spec.From) {
		r.capped = false
	}
	return membershipResult(spec, r)
}

// ifaceRelatedReads reads the changes recorded against ifaces that fixed asks
// for. The interface ids are sorted and batched to the URL budget with fixed
// measured in it, so each read's own parameters are, and each batch is a
// related read, walked to changeScanCap rows under a slot of sem, which the
// other membership reads share. keep checks every row, and relatedTypeID is
// the interface type's id, for the related-type retry. The results are in
// batch order, each batch writing only its own. A batch that fails, or that
// ctx ends while it waits for a slot, calls fail with its error as it does.
func (p *Provider) ifaceRelatedReads(ctx context.Context, fail context.CancelCauseFunc, sem chan struct{}, fixed url.Values, keep func(objectChange) bool, relatedTypeID int, ifaces map[int64]int64) []changeReadResult {
	batcher := newQueryBatcher(relatedObjectIDParam, fixed)
	batches := batcher.chunk(sortedIDs(ifaces))
	results := make([]changeReadResult, len(batches))
	var wg sync.WaitGroup
	for i, batch := range batches {
		r := changeRead{q: batcher.query(batch), keep: keep, fromRelated: true, relatedTypeID: relatedTypeID}
		wg.Go(func() {
			// A batch ctx ended before it ran fails with ctx's cause: with no
			// error, it would read as one that found nothing.
			if err := takeSlot(ctx, sem); err != nil {
				results[i].err = err
				fail(err)
				return
			}
			results[i] = p.runMembershipRead(ctx, r)
			<-sem
			if results[i].err != nil {
				fail(results[i].err)
			}
		})
	}
	wg.Wait()
	return results
}

// macMembers lists the MAC addresses on ifaces, batched over assigned_object_id,
// fields=id,assigned_object_type,assigned_object_id, each batch a list under a
// slot of sem: a MAC's changes are recorded against nothing, so its list is
// the only index of which are on an interface. It keeps a row only when its
// assigned_object_type string is ifaceType and its assigned_object_id is in
// ifaces, owned by that interface's owner. assigned_object_type is never sent:
// up to NetBox 4.4.3 it takes an id and a name is a 400, from 4.4.4 a name and
// an id lists nothing, while assigned_object_id is a number on every version.
//
// Each batch writes only its own slot. Any batch's error fails the list, one
// that isn't a refusal (listRefused) before one that is, so a refused batch
// refuses the whole list, and a batch ctx ended before it ran fails it with
// ctx's cause.
func (p *Provider) macMembers(ctx context.Context, sem chan struct{}, ifaceType string, ifaces map[int64]int64) (map[int64]int64, error) {
	batcher := newQueryBatcher("assigned_object_id", url.Values{"fields": {"id,assigned_object_type,assigned_object_id"}})
	batches := batcher.chunk(sortedIDs(ifaces))
	rows := make([][]json.RawMessage, len(batches))
	errs := make([]error, len(batches))
	var wg sync.WaitGroup
	for i, batch := range batches {
		q := batcher.query(batch)
		wg.Go(func() {
			if errs[i] = takeSlot(ctx, sem); errs[i] != nil {
				return
			}
			defer func() { <-sem }()
			rows[i], errs[i] = p.listChildren(ctx, macAddressList, q)
		})
	}
	wg.Wait()
	var refused error
	for _, err := range errs {
		switch {
		case err == nil:
		case !listRefused(err):
			return nil, err
		case refused == nil:
			refused = err
		}
	}
	if refused != nil {
		return nil, refused
	}
	set := map[int64]int64{}
	for _, batch := range rows {
		for _, raw := range batch {
			row := decodeObject(raw)
			var typ string
			id, ok := snapshotID(row, "id")
			iface, assigned := snapshotID(row, "assigned_object_id")
			if !ok || !assigned || json.Unmarshal(row["assigned_object_type"], &typ) != nil || normalizeObjectType(typ) != ifaceType {
				continue
			}
			if owner, on := ifaces[iface]; on {
				addMember(set, id, owner)
			}
		}
	}
	return set, nil
}

// sortedIDs is set's ids in order, as the values an id filter sends.
func sortedIDs(set map[int64]int64) []string {
	out := make([]string, 0, len(set))
	for _, id := range slices.Sorted(maps.Keys(set)) {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// membershipQuery is the part the membership reads share, the IP history
// aside (ipHistoryReads): newest first, from From, and no end, since
// membership covers [From, now]. No Action is pushed: the query's filters
// apply to what the track shows, not to who the children are.
func membershipQuery(spec provider.ChangeSpec) url.Values {
	return changeLogQuery(provider.ChangeSpec{From: spec.From}, changeFilter{})
}

// runMembershipRead walks one membership read to changeScanCap rows, through
// runChangeRead so it gets the related-type retry. Its limit is one more, so
// it never stops for having kept enough: any row it hasn't read may name a
// child.
func (p *Provider) runMembershipRead(ctx context.Context, r changeRead) changeReadResult {
	maxRows := p.changeScanCap()
	return p.runChangeRead(ctx, r, changeFilter{}, maxRows+1, maxRows)
}

// membershipResult makes a membership read's result what Changes merges.
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
// interface's id doesn't match.
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
// first read to name a child owns it.
func addMember(set map[int64]int64, child, owner int64) {
	if _, ok := set[child]; !ok {
		set[child] = owner
	}
}

// matchAssigned matches an address row to one of ifaces: its assigned_object_type
// is the interface type's id and its assigned_object_id is in ifaces. The after
// side is read first, so an address moved between two scope objects is on the
// one it moved to. The owner is the interface's.
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
// device does: the after side first, then the before, so a moved module is on
// the device it moved to. The owner is that object.
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
// object. The after side first, then the before, so a moved service is on its
// new parent.
func matchServiceParent(oc objectChange, scopeTypeID int, ownerField string, scopeIDs map[int64]bool) (owner int64, ok bool) {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		typ, typed := snapshotID(data, "parent_object_type")
		id, identified := snapshotID(data, "parent_object_id")
		if typed && identified && typ == int64(scopeTypeID) && scopeIDs[id] {
			return id, true
		}
		if ownerField == "" {
			continue
		}
		if id, named := snapshotID(data, ownerField); named && scopeIDs[id] {
			return id, true
		}
	}
	return 0, false
}

// descendantParent names the scope object a descendant row matched, for the
// marker title's "on <name>": with one scope object it is that object, else
// the one whose id is owner, else "". objects is the scope's objects by id
// (objectsByID).
func descendantParent(scope *resolvedScope, objects map[int64]scopeObject, owner int64) string {
	if len(scope.Objects) == 1 {
		return scopeObjectName(scope, scope.Objects[0])
	}
	if o, ok := objects[owner]; ok {
		return scopeObjectName(scope, o)
	}
	return ""
}

// briefQuery is brief=true plus param once per value.
func briefQuery(param string, values []string) url.Values {
	return url.Values{"brief": {"true"}, param: slices.Clone(values)}
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
