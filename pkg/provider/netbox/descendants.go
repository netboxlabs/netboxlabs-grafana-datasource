package netbox

import (
	"cmp"
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

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

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

// cableEndWorkers is how many membership and by-id requests (the lists, the
// cable-end, IP-history and MAC batches, and the by-id batches) run at once:
// bounded, without a long serial chain on a device with thousands of
// interfaces.
const cableEndWorkers = 4

// maxDescendantsBudget caps the time the descendant reads get. They run in a
// chain (lists, then the cable ends, IP history and MAC list, then the by-id
// reads, with the residual scans beside them all, then the by-id read of what
// those scans found removed), and a short dashboard refresh must not lose the
// scope's own changes to them.
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

// The descendant types. Every one is read by id, its members' changes. The
// residual scans read all but cables: addresses in one scan, modules and
// services in another, so address churn can't fill the page the parts share.
const (
	ipAddressType  = "ipam.ipaddress"
	macAddressType = "dcim.macaddress"
	cableType      = "dcim.cable"
	moduleType     = "dcim.module"
	serviceType    = "ipam.service"
)

// descendantChanges reads the scope's descendant changes over [From, To]: the
// membership reads' results, then the by-id reads', then the residual scans',
// then the history of what those scans found removed (removedHistory), every
// one a descendant read and all but the last scans. nil, nil when the scope
// type has no descendantKinds row or no descendant type survives Object types.
// The first error among the membership and by-id reads is returned, or ctx's
// cause when ctx ended once they had finished, and each read that fails calls
// fail with its error as it does, so the caller can stop the others (Changes
// cancels ctx with it).
//
// The descendant types are decided before anything is read, so a query whose
// Object types leave none reads nothing, not even the type map. Children are
// read only for the types the query wants. Once they are known, the by-id
// reads (byIDReads) ask for their changes. Cables are read by id alone: a
// scanned cable change showed only when its cable was already a member. No
// by-id read is made without members.
//
// The residual scans look for what the by-id reads can miss: a child taken off
// the scope or deleted before membership could name it. The address scan reads
// IPs and MACs, the parts scan modules and services, one page each
// (residualScan). They start at once, beside membership, under their own
// deadline (residualContext) and without a slot of sem, and never fail the
// others: one that fails or runs out of its own time is unchecked. ctx ending
// still ends them, and is then the call's error, though the other reads had
// finished: a caller's cancel or the descendants deadline is never read as an
// unchecked scan. The parts scan matches a row by the scope objects alone. The
// address scan matches by the scope's interfaces, so its page waits for
// membership, or for ctx ending; not for its own deadline, so a page that came
// in time is matched however long membership takes. The scans are waited for
// on every path, so none outlives the call. A scope without interfaces drops
// the address scan's result: no IP or MAC it could match exists, so it can't
// have missed one. What the scans found that isn't a member is then read by id
// (removedHistory), so an object removed inside a page's reach shows its
// earlier changes in the range too; that read is best effort, and never fails
// the others either.
//
// Each read pushes its types as changed_object_type_id, and keeps a row only
// when the row's own type and snapshot, or its cable id, tie it to the scope,
// whatever NetBox did with the filters.
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

	// Membership is read for the types the query reads. A device's cable ends
	// are read through its interfaces, so its cables need them read too.
	want := descendantWants{
		ips:      slices.Contains(addrTypes, ipAddressType),
		macs:     slices.Contains(addrTypes, macAddressType),
		cables:   slices.Contains(partTypes, cableType),
		modules:  slices.Contains(partTypes, moduleType),
		services: slices.Contains(partTypes, serviceType),
	}
	want.ifaces = want.ips || want.macs || (want.cables && scope.Type == "dcim.device")

	inScope := scope.idSet()
	ifaceTypeID, scopeTypeID := ids[kind.ifaceType], ids[scope.Type]
	// m is assigned once, before ready is closed, and read only after: by the
	// by-id reads, and by the address scan's matcher once ready is closed.
	var m members
	ready := make(chan struct{})
	// byScope matches a module or service row by the scope objects alone, so
	// the parts scan needs no membership.
	byScope := func(oc objectChange) (int64, bool) {
		switch normalizeObjectType(oc.changedType()) {
		case moduleType:
			return matchOwner(oc, "device", inScope)
		case serviceType:
			return matchServiceParent(oc, scopeTypeID, kind.ownerField, inScope)
		}
		return 0, false
	}
	match := func(oc objectChange) (int64, bool) {
		switch normalizeObjectType(oc.changedType()) {
		case ipAddressType, macAddressType:
			return matchAssigned(oc, ifaceTypeID, m.ifaces)
		case cableType:
			owner, ok := m.cables[oc.ChangedObjectID]
			return owner, ok
		}
		return byScope(oc)
	}
	// afterMembership is match once membership is known. ctx ending first
	// matches nothing: descendantChanges then returns an error, whether
	// membership failed or ctx ended later, so the rows it gave up on are
	// dropped with the rest.
	afterMembership := func(oc objectChange) (int64, bool) {
		select {
		case <-ready:
			return match(oc)
		case <-ctx.Done():
			return 0, false
		}
	}

	// The residual scans start now, beside membership. Their own deadline and
	// cancel end only them, and they are stopped and waited for on every path.
	rctx, stopResidual := residualContext(ctx)
	var residual sync.WaitGroup
	defer func() {
		stopResidual()
		residual.Wait()
	}()
	// Cables are read by id alone: the parts scan reads modules and services.
	partScan := slices.DeleteFunc(slices.Clone(partTypes), func(t string) bool { return t == cableType })
	scans := []struct {
		types []string
		match func(objectChange) (int64, bool)
	}{
		{addrTypes, afterMembership},
		{partScan, byScope},
	}
	found := make([]changeReadResult, len(scans))
	for i, scan := range scans {
		if len(scan.types) == 0 {
			continue
		}
		q := changeLogQuery(spec, f)
		for _, t := range scan.types {
			q.Add(changedObjectTypeIDParam, strconv.Itoa(ids[t]))
		}
		r := changeRead{q: q, descendant: func(oc objectChange) (int64, bool) {
			// A type the scan didn't ask for isn't kept, so a NetBox that ignores
			// the type filter can't widen it.
			if !slices.Contains(scan.types, normalizeObjectType(oc.changedType())) {
				return 0, false
			}
			return scan.match(oc)
		}}
		residual.Go(func() { found[i] = p.residualScan(rctx, spec, r, f, limit) })
	}

	// One semaphore bounds every membership and by-id request, so a scope with
	// many interfaces or members can't open more than cableEndWorkers at once.
	sem := make(chan struct{}, cableEndWorkers)
	m, err = p.descendantMembers(ctx, fail, sem, spec, scope, kind, ids, want)
	close(ready)
	if err != nil {
		return nil, err
	}
	var byID []changeReadResult
	if sets := memberSets(m, want); len(sets) > 0 {
		byID = p.byIDReads(ctx, fail, true, sem, f, limit, byIDBatches(spec, f, ids, sets, match))
	}
	for _, r := range byID {
		if r.err != nil {
			return nil, r.err
		}
	}
	residual.Wait()
	// The residual marker can describe a scan's misses only when every list was
	// read. This is deliberately conservative: any refused or over-cap list, a
	// MAC list too when it is the parts scan that was cut, makes the marker the
	// general one.
	for i := range found {
		found[i].residual = !m.degraded
		found[i].types = scans[i].types
	}
	var kept []changeReadResult
	if len(addrTypes) > 0 && len(m.ifaces) > 0 {
		kept = append(kept, found[0])
	}
	if len(partScan) > 0 {
		kept = append(kept, found[1])
	}
	removed := p.removedHistory(ctx, sem, spec, f, limit, ids, removedSets(m, kept, f), match)
	// ctx ended while only the residual scans, or the read after them, were
	// left: the caller's cancel, a failed own read's, or the descendants
	// deadline just as membership succeeded, which the address scan's matcher
	// may have seen first. A scan it cut reads as unchecked, as one its own
	// deadline cut does, so ctx's cause is the error, as it is for any other
	// read ctx cuts.
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	return slices.Concat(m.reads, byID, kept, removed), nil
}

// removedSets is what the residual scans' kept rows name that isn't a member:
// each object by type, as memberSets has them. An IP, MAC, module or service
// there was taken off the scope or deleted before membership could name it.
// Only what a read by id could add counts:
//
//   - only a capped scan's rows: a page that read every change of its types in
//     the range already holds each such object's changes, and one that stopped
//     at limit holds limit changes newer than any it left;
//   - nothing when the query reads one Action, create or delete: an object has
//     one row of either, and it is the one the page found;
//   - no object whose create the page kept newer than its oldest row: every
//     change to it since is newer still, so on the page. One at the oldest row
//     may share its time with a change past the page.
func removedSets(m members, scans []changeReadResult, f changeFilter) map[string]map[int64]int64 {
	if len(f.actions) == 1 && !f.actions["update"] {
		return nil
	}
	capped := slices.DeleteFunc(slices.Clone(scans), func(r changeReadResult) bool { return !r.capped })
	type object struct {
		typ string
		id  int64
	}
	onPage := map[object]bool{}
	for _, r := range capped {
		for _, d := range r.rows {
			if d.oc.Action.Value == "create" && d.at.After(r.oldest) {
				onPage[object{normalizeObjectType(d.oc.changedType()), d.oc.ChangedObjectID}] = true
			}
		}
	}
	sets := map[string]map[int64]int64{}
	for _, r := range capped {
		for _, d := range r.rows {
			t, id := normalizeObjectType(d.oc.changedType()), d.oc.ChangedObjectID
			if _, member := m.set(t)[id]; member || id <= 0 || onPage[object{t, id}] {
				continue
			}
			if sets[t] == nil {
				sets[t] = map[int64]int64{}
			}
			addMember(sets[t], id, d.owner)
		}
	}
	return sets
}

// removedHistory reads by id (byIDReads, with match) the changes over [From,
// To] to the objects in sets, what the residual scans found removed: no list
// names them, and a MAC, module or service has no history recorded against
// what it was on, so without this an object removed inside a page's reach
// would show its removal but not its earlier changes in the range. With sets
// empty it reads nothing.
//
// It is best effort, under its own deadline (residualContext) so that running
// out of time ends only it: a read that fails or that its context cuts is
// dropped, rows and all, and calls no fail. A warning logs it, unless ctx
// ended, which fails the call anyway. Whatever it drops is history older than
// the scan that found the removal, and the residual marker already says that
// what lies past a scan's reach wasn't checked. For the same reason its reads
// aren't scans: one stopped at the scan cap makes no cut-off. They aren't
// residual either.
func (p *Provider) removedHistory(ctx context.Context, sem chan struct{}, spec provider.ChangeSpec, f changeFilter, limit int, ids map[string]int, sets map[string]map[int64]int64, match func(objectChange) (int64, bool)) []changeReadResult {
	if len(sets) == 0 {
		return nil
	}
	hctx, stop := residualContext(ctx)
	defer stop()
	batches := byIDBatches(spec, f, ids, sets, match)
	results := p.byIDReads(hctx, func(error) {}, false, sem, f, limit, batches)
	kept := results[:0]
	for i, r := range results {
		if r.err == nil {
			kept = append(kept, r)
			continue
		}
		if ctx.Err() == nil {
			log.DefaultLogger.Warn("netbox: history read of removed descendants failed; their older changes in the range don't show",
				"objectType", batches[i].typ, "ids", batches[i].ids, "error", logSafe(r.err.Error()))
		}
	}
	return kept
}

// set is m's members of type t, a descendant type's name (ipam.ipaddress,
// dcim.macaddress, dcim.cable, dcim.module, ipam.service), or nil for another.
func (m members) set(t string) map[int64]int64 {
	switch t {
	case ipAddressType:
		return m.ips
	case macAddressType:
		return m.macs
	case cableType:
		return m.cables
	case moduleType:
		return m.modules
	case serviceType:
		return m.services
	}
	return nil
}

// memberSets is what the by-id reads ask for: each wanted type with at least
// one member, by type name.
func memberSets(m members, want descendantWants) map[string]map[int64]int64 {
	wanted := map[string]bool{ipAddressType: want.ips, macAddressType: want.macs, cableType: want.cables, moduleType: want.modules, serviceType: want.services}
	sets := map[string]map[int64]int64{}
	for t, ok := range wanted {
		if set := m.set(t); ok && len(set) > 0 {
			sets[t] = set
		}
	}
	return sets
}

// byIDBatch is one by-id request: its read, and the type and number of the
// members it asks for.
type byIDBatch struct {
	r   changeRead
	typ string
	ids int
}

// byIDBatches is the requests that read the changes over [From, To] to the
// members in sets by id, through the change log's index on what a change
// changed, so the changes to other objects of these types can't use up a cap
// before them. Each request is changeLogQuery's, which pushes the query's one
// Action but never its Changed fields, plus one member type's id from ids and
// a batch of that type's members' ids, sorted and batched to the URL budget
// with the rest of the query measured in it. The types come in ascending id
// order.
//
// One type a request: NetBox ANDs the type filter with the id filter, so a
// request with two types would also return the rows of an object of one type
// whose id is a member of the other, as a module sharing an IP member's id,
// and those rows would use up its cap. A NetBox may ignore either filter too,
// so a row is kept only when its type is the request's and its id a member's,
// and then only when match, the type's scope check, passes it and names its
// owner.
func byIDBatches(spec provider.ChangeSpec, f changeFilter, ids map[string]int, sets map[string]map[int64]int64, match func(objectChange) (int64, bool)) []byIDBatch {
	var batches []byIDBatch
	for _, t := range slices.SortedFunc(maps.Keys(sets), func(a, b string) int { return cmp.Compare(ids[a], ids[b]) }) {
		set := sets[t]
		fixed := changeLogQuery(spec, f)
		fixed.Set(changedObjectTypeIDParam, strconv.Itoa(ids[t]))
		member := func(oc objectChange) (int64, bool) {
			if _, ok := set[oc.ChangedObjectID]; !ok || normalizeObjectType(oc.changedType()) != t {
				return 0, false
			}
			return match(oc)
		}
		batcher := newQueryBatcher(changedObjectIDParam, fixed)
		for _, batch := range batcher.chunk(sortedIDs(set)) {
			batches = append(batches, byIDBatch{r: changeRead{q: batcher.query(batch), descendant: member}, typ: t, ids: len(batch)})
		}
	}
	return batches
}

// byIDReads runs batches (byIDBatches), each walked to limit rows kept or
// changeScanCap rows read, under a slot of sem, which the membership reads
// share, and writing only its own result. A result is a descendant read, and a
// scan when scan is set: a batch that stops at the cap may have missed rows
// past it, so it counts for the cut-off. A batch that fails, or that ctx ends
// while it waits for a slot, calls fail with its error as it does. The results
// are in batch order.
func (p *Provider) byIDReads(ctx context.Context, fail context.CancelCauseFunc, scan bool, sem chan struct{}, f changeFilter, limit int, batches []byIDBatch) []changeReadResult {
	results := make([]changeReadResult, len(batches))
	var wg sync.WaitGroup
	for i, b := range batches {
		wg.Go(func() {
			// A batch ctx ended before it ran fails with ctx's cause: with no
			// error, it would read as one that found nothing.
			if err := takeSlot(ctx, sem); err != nil {
				results[i].err = err
			} else {
				results[i] = p.runChangeRead(ctx, b.r, f, limit, p.changeScanCap())
				<-sem
			}
			results[i].scan, results[i].descendant = scan, true
			if results[i].err != nil {
				fail(results[i].err)
			}
		})
	}
	wg.Wait()
	return results
}

// residualRows is the page a residual scan reads: pageSize, unless a test has
// lowered it.
func (p *Provider) residualRows() int {
	if p.residualRowsOverride > 0 {
		return p.residualRowsOverride
	}
	return pageSize
}

// residualContext is ctx with a deadline halfway between now and ctx's own,
// half the time left to the descendants deadline, or with only a cancel when
// ctx has no deadline. Its deadline and its cancel end only the reads under
// it, the residual scans or the read after them (removedHistory), so the
// membership and by-id reads keep the other half, and running out of time
// fails nothing, while ctx ending (the descendants deadline, a failed read's
// cancel or the caller's) still ends them.
func residualContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	now := time.Now()
	return context.WithDeadline(ctx, now.Add(deadline.Sub(now)/2))
}

// residualScan reads exactly one page of r under ctx, the residual context,
// residualRows rows long, and never follows its next link, whatever the page's
// size: NetBox's MAX_PAGE_SIZE may serve fewer rows than asked, and a second
// fetch, made after r's matcher has waited for membership, could run under an
// ended ctx and throw away a page already matched. Each row goes through
// visit, as walkChangeRead's do, and the result is capped or full as theirs
// are (stop), more being a next link, or rows left on the page when limit
// stopped it. A scan that stops at limit is therefore full, not capped, which
// the limit marker counts. oldest is the oldest row read.
//
// A page that came in time is matched however long r's matcher waits, and
// stands even if ctx has ended by then. A fetch that failed, or that ctx ended
// before the page came, checked nothing: the result is uncheckedResult's.
// Either way it has no error and calls no fail: a residual scan doesn't fail
// the descendant reads. The result is a scan and a descendant read.
func (p *Provider) residualScan(ctx context.Context, spec provider.ChangeSpec, r changeRead, f changeFilter, limit int) changeReadResult {
	q := url.Values{}
	maps.Copy(q, r.q)
	q.Set("limit", strconv.Itoa(p.residualRows()))
	page, err := p.getListPageRetry(ctx, p.client.apiURL(changeLogEndpoint, q))
	if err != nil {
		return uncheckedResult()
	}
	res := changeReadResult{scan: true, descendant: true}
	scanned, more := 0, page.Next != nil && *page.Next != ""
	for i, raw := range page.Results {
		scanned++
		if !res.visit(r, f, limit, raw) {
			more = more || i < len(page.Results)-1
			break
		}
	}
	res.stop(scanned, more, limit)
	if res.capped && res.oldest.IsZero() {
		// A capped page with no row of a parsed time says nothing about where
		// its reach ends: it checked nothing.
		return uncheckedResult()
	}
	return res
}

// uncheckedResult is a residual scan's result when it checked nothing: a
// descendant scan, unchecked and capped, with no rows and no error. Its oldest
// is zero, which cutoffChange counts as reaching only the end of the range, the
// one Changes computes, since anything in the range may have been missed.
func uncheckedResult() changeReadResult {
	return changeReadResult{scan: true, descendant: true, capped: true, unchecked: true}
}

// rangeEnd is where a query's range ends: To, or now with no To.
func rangeEnd(spec provider.ChangeSpec) time.Time {
	if spec.To.IsZero() {
		return time.Now()
	}
	return spec.To
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
	// module's, from its interfaces' deletes) and the residual scan.
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
			macs, err := p.macMembers(ctx, fail, sem, kind.ifaceType, m.ifaces)
			listedMACs = macs
			macRefused, macErr = ownRefusal(err)
			// A batch already failed any error that isn't a refusal. A refusal
			// that isn't the list's own (ownRefusal) fails here.
			if listRefused(macErr) {
				fail(macErr)
			}
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
// older history is left to the address scan. A read whose rows' times didn't
// parse (oldest zero) hasn't shown it passed From, and stays capped.
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
// ctx's cause. A batch whose error isn't a refusal calls fail with it as it
// gets it, as a cable-end batch does, so its siblings and the other reads stop
// then, not once every batch has finished; a refusal never calls fail.
func (p *Provider) macMembers(ctx context.Context, fail context.CancelCauseFunc, sem chan struct{}, ifaceType string, ifaces map[int64]int64) (map[int64]int64, error) {
	batcher := newQueryBatcher("assigned_object_id", url.Values{"fields": {"id,assigned_object_type,assigned_object_id"}})
	batches := batcher.chunk(sortedIDs(ifaces))
	rows := make([][]json.RawMessage, len(batches))
	errs := make([]error, len(batches))
	var wg sync.WaitGroup
	for i, batch := range batches {
		q := batcher.query(batch)
		wg.Go(func() {
			if errs[i] = takeSlot(ctx, sem); errs[i] == nil {
				rows[i], errs[i] = p.listChildren(ctx, macAddressList, q)
				<-sem
			}
			if errs[i] != nil && !listRefused(errs[i]) {
				fail(errs[i])
			}
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
// is the interface type's id and its assigned_object_id is in ifaces. The owner
// is the interface's.
func matchAssigned(oc objectChange, ifaceTypeID int, ifaces map[int64]int64) (owner int64, ok bool) {
	return assignedOwner(oc, ifaceTypeID, func(iface int64) (int64, bool) {
		owner, ok := ifaces[iface]
		return owner, ok
	})
}

// assignedOwner matches an address row whose assigned_object_type is the
// interface type's id and whose assigned_object_id owner accepts, and returns
// the owner it gives. The after side is read first, so an address moved
// between two interfaces is on the one it moved to.
func assignedOwner(oc objectChange, ifaceTypeID int, owner func(iface int64) (int64, bool)) (int64, bool) {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		typ, typed := snapshotID(data, "assigned_object_type")
		id, identified := snapshotID(data, "assigned_object_id")
		if !typed || !identified || typ != int64(ifaceTypeID) {
			continue
		}
		if o, ok := owner(id); ok {
			return o, true
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
	return serviceParent(oc, scopeTypeID, ownerField, func(id int64) bool { return scopeIDs[id] })
}

// serviceParent is matchServiceParent with the parent's id checked by accept.
func serviceParent(oc objectChange, scopeTypeID int, ownerField string, accept func(id int64) bool) (owner int64, ok bool) {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		typ, typed := snapshotID(data, "parent_object_type")
		id, identified := snapshotID(data, "parent_object_id")
		if typed && identified && typ == int64(scopeTypeID) && accept(id) {
			return id, true
		}
		if ownerField == "" {
			continue
		}
		if id, named := snapshotID(data, ownerField); named && accept(id) {
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
