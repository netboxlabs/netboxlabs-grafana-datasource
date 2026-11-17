package netbox

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// allScope is an annotation whose Scope is a variable set to All with the
// custom value $__all, over scopeType ("" is the default, devices).
func allScope(scopeType string) provider.ChangeSpec {
	return provider.ChangeSpec{ScopeType: scopeType, ScopeTypeSet: scopeType != "", ScopeSet: true, ScopeAll: true, Limit: 50}
}

// related is an update of ct objID, recorded against the device devID, whose
// snapshots name the device, as an interface's do. Its changed object is a
// nested row under that device, displayed as devName.
func related(id, minutes int, ct string, objID, devID int64, devName string) loggedChange {
	return loggedChange{id: id, minutes: minutes, action: "update", ct: ct, objID: objID,
		relType: "dcim.device", relID: devID,
		changedObject: fmt.Sprintf(`{"id":%d,"display":"eth0","device":{"id":%d,"display":%q,"url":"http://nb/api/dcim/devices/%d/"}}`, objID, devID, devName, devID),
		pre:           fmt.Sprintf(`{"device":%d,"description":"a"}`, devID),
		post:          fmt.Sprintf(`{"device":%d,"description":"b"}`, devID)}
}

// snap is an update of ct objID whose snapshots are both data.
func snap(id, minutes int, ct string, objID int64, data string) loggedChange {
	return loggedChange{id: id, minutes: minutes, action: "update", ct: ct, objID: objID, pre: data, post: data}
}

// noParamIn fails the test when any of qs carries param.
func noParamIn(t *testing.T, qs []url.Values, param string) {
	t.Helper()
	for _, q := range qs {
		if q.Has(param) {
			t.Errorf("read %v carries %s, want every object of the type", q, param)
		}
	}
}

// $__all scopes the track to every object of the Scope type: their own
// changes and the changes recorded against them, with no object ids, and none
// of the per-object reads. Another type's change doesn't show. A change
// recorded against a device names it from its nested reference.
func TestChanges_AllScopeReadsTheScopeType(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.device", 99),
		ch(3, 3, "dcim.site", 5),
		related(4, 4, "dcim.interface", 500, 99, "leaf9"),
	)
	changes := changesOK(t, s.start(t), allScope(""))
	wantIDs(t, changes, "#1", "#2", "#4")
	if got := changes[2].Parent; got != "leaf9" {
		t.Errorf("interface change parent = %q, want leaf9", got)
	}
	if got := changes[0].Parent; got != "" {
		t.Errorf("device change parent = %q, want none: it is the scope object", got)
	}

	reads := s.changeLog()
	if len(reads) != 2 {
		t.Fatalf("change-log reads = %v, want the type read and the related read", reads)
	}
	noParamIn(t, reads, changedObjectIDParam)
	noParamIn(t, reads, relatedObjectIDParam)
	// The two reads run at once, so they arrive in either order.
	typeRead, relatedRead := reads[0], reads[1]
	if typeRead.Has(relatedObjectTypeParam) {
		typeRead, relatedRead = relatedRead, typeRead
	}
	if got := typeRead[changedObjectTypeIDParam]; !slices.Equal(got, []string{"12"}) {
		t.Errorf("type read types = %v, want [12]", got)
	}
	if got := relatedRead.Get(relatedObjectTypeParam); got != "dcim.device" {
		t.Errorf("related read type = %q, want dcim.device", got)
	}
	for _, path := range []string{"dcim/devices", "dcim/interfaces"} {
		if got := s.requestsTo(path); len(got) != 0 {
			t.Errorf("%s reads = %v, want none: All lists no objects", path, got)
		}
	}
}

// A NetBox that ignored related_object_type would return every change to the
// related read. Each row is kept only when its snapshot ties it to a device, so
// the track can't widen.
func TestChanges_AllScopeIgnoredRelatedFilterCannotWiden(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(3, 3, "dcim.site", 5),
		related(4, 4, "dcim.interface", 500, 99, "leaf9"),
	)
	s.ignoreRelatedQ = true
	wantIDs(t, changesOK(t, s.start(t), allScope("")), "#1", "#4")
}

// The related read retries related_object_type as an id on a NetBox that
// takes one (up to 4.4.3), still with no object ids.
func TestChanges_AllScopeRelatedTypeRetriesAsAnID(t *testing.T) {
	s := newScopedChangeServer(related(4, 4, "dcim.interface", 500, 99, "leaf9"))
	s.relatedByID = true
	wantIDs(t, changesOK(t, s.start(t), allScope("")), "#4")
	var types []string
	for _, q := range s.changeLog() {
		if q.Has(relatedObjectTypeParam) {
			types = append(types, q.Get(relatedObjectTypeParam))
		}
	}
	if !slices.Equal(types, []string{"dcim.device", "12"}) {
		t.Errorf("related reads' types = %v, want the name, then the id", types)
	}
	noParamIn(t, s.changeLog(), relatedObjectIDParam)
}

// allDescendantRows is a device track's descendant rows under All: what each
// type's snapshot says decides whether the row is a device's.
func allDescendantRows() []loggedChange {
	ipOnIface := snap(11, 2, "ipam.ipaddress", 39, `{"assigned_object_type":14,"assigned_object_id":100}`)
	// NetBox serves the changed object brief: an IP's names no interface or device.
	ipOnIface.changedObject = `{"id":39,"display":"10.0.0.1/24","family":4,"address":"10.0.0.1/24"}`
	module := snap(14, 5, "dcim.module", 3, `{"device":17}`)
	module.changedObject = `{"id":3,"display":"Slot 1","device":{"id":17,"display":"leaf1"}}`
	unassigned := loggedChange{id: 18, minutes: 9, action: "update", ct: "ipam.ipaddress", objID: 40,
		pre: `{"assigned_object_type":14,"assigned_object_id":100}`, post: `{"assigned_object_type":null,"assigned_object_id":null}`}
	return []loggedChange{
		ch(10, 1, "dcim.cable", 24),
		ipOnIface,
		snap(12, 3, "ipam.ipaddress", 41, `{"assigned_object_type":31,"assigned_object_id":7}`), // on a VM interface
		snap(13, 4, "dcim.macaddress", 4, `{"assigned_object_type":14,"assigned_object_id":100}`),
		module,
		snap(15, 6, "ipam.service", 3, `{"parent_object_type":12,"parent_object_id":17}`),
		snap(16, 7, "ipam.service", 4, `{"parent_object_type":30,"parent_object_id":3}`), // on a VM
		snap(17, 8, "ipam.service", 5, `{"device":17,"virtual_machine":null}`),           // NetBox 4.2's shape
		unassigned,
	}
}

// With Include descendants on, All also reads the types it follows, in the
// same read as the scope type's own changes: cables and modules by type, IP
// and MAC addresses on device interfaces, services whose parent is a device.
// None of the per-object descendant reads run.
func TestChanges_AllScopeDescendantTypes(t *testing.T) {
	s := newScopedChangeServer(allDescendantRows()...)
	changes := changesOK(t, s.start(t), withDescendants(allScope("")))
	wantIDs(t, changes, "#10", "#11", "#13", "#14", "#15", "#17", "#18")
	parents := map[string]string{}
	for _, c := range changes {
		parents[c.ObjectRepr] = c.Parent
	}
	if parents["#14"] != "leaf1" || parents["#11"] != "" {
		t.Errorf("parents = %v, want leaf1 for the module (its brief row's device) and none for the IP (its brief row names none)", parents)
	}

	var typeReads [][]string
	for _, q := range s.changeLog() {
		if !q.Has(relatedObjectTypeParam) {
			typeReads = append(typeReads, q[changedObjectTypeIDParam])
		}
	}
	if want := [][]string{{"12", "17", "20", "21", "22", "23"}}; !slices.EqualFunc(typeReads, want, slices.Equal[[]string]) {
		t.Errorf("type reads = %v, want one read of the device type and its descendant types %v", typeReads, want)
	}
	noParamIn(t, s.changeLog(), changedObjectIDParam)
	for _, path := range []string{"dcim/cables", "dcim/mac-addresses", "dcim/modules", "ipam/services"} {
		if got := s.requestsTo(path); len(got) != 0 {
			t.Errorf("%s reads = %v, want none under All", path, got)
		}
	}
	// The diffs' name lookup reads an interface by id; no list of a device's
	// interfaces is read.
	noParamIn(t, s.requestsTo("dcim/interfaces"), "device_id")
}

// The type read keeps only some of its rows, so it scans past Limit: a device
// IP behind more VM IPs than Limit still shows.
func TestChanges_AllScopeReadsScanPastLimit(t *testing.T) {
	s := newScopedChangeServer(
		snap(1, 1, "ipam.ipaddress", 41, `{"assigned_object_type":31,"assigned_object_id":7}`),
		snap(2, 2, "ipam.ipaddress", 42, `{"assigned_object_type":31,"assigned_object_id":7}`),
		snap(3, 3, "ipam.ipaddress", 43, `{"assigned_object_type":31,"assigned_object_id":7}`),
		snap(4, 4, "ipam.ipaddress", 39, `{"assigned_object_type":14,"assigned_object_id":100}`),
	)
	spec := withDescendants(allScope(""))
	spec.Limit = 1
	wantIDs(t, changesOK(t, s.start(t), spec), "#4")
}

// A scan that stops at its cap with only other rows read says so with the
// cut-off marker, rather than show a short track as if it were complete.
func TestChanges_AllScopeCappedScanDrawsTheCutoff(t *testing.T) {
	s := newScopedChangeServer(
		snap(1, 1, "ipam.ipaddress", 41, `{"assigned_object_type":31,"assigned_object_id":7}`),
		snap(2, 2, "ipam.ipaddress", 42, `{"assigned_object_type":31,"assigned_object_id":7}`),
		snap(3, 3, "ipam.ipaddress", 43, `{"assigned_object_type":31,"assigned_object_id":7}`),
		snap(4, 4, "ipam.ipaddress", 39, `{"assigned_object_type":14,"assigned_object_id":100}`),
	)
	p := s.start(t)
	p.changeScanCapOverride = 2
	spec := withDescendants(allScope(""))
	spec.Limit = 1
	wantIDs(t, changesOK(t, p, spec), "cutoff")
}

// Without the type map, the descendant types can't be checked, so they aren't
// read: the scope type's own changes are read by name, and the track ends with
// the descendants warning, as a scoped track's does.
func TestChanges_AllScopeTypeMapUnreadable(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17), ch(10, 2, "dcim.cable", 24))
	s.coreStatus = http.StatusForbidden
	changes := changesOK(t, s.start(t), withDescendants(allScope("")))
	wantIDs(t, changes, "#1", "descendants-error")
	annotationErr(t, descendantsError(t, changes).Err, provider.AnnotationReasonDescendantTypesUnreadable)
	for _, q := range s.changeLog() {
		if q.Get(changedObjectTypeParam) == "dcim.cable" || slices.Contains(q[changedObjectTypeIDParam], "17") {
			t.Errorf("read %v asks for cables, which can't be checked without the type map", q)
		}
	}
}

// An interface's cable could be a console or power port's from its snapshot,
// so interface All reads no cables by type; IP and MAC addresses it does.
func TestChanges_AllScopeInterfacesReadNoCables(t *testing.T) {
	s := newScopedChangeServer()
	changesOK(t, s.start(t), withDescendants(allScope("dcim.interface")))
	var typeReads [][]string
	for _, q := range s.changeLog() {
		if !q.Has(relatedObjectTypeParam) {
			typeReads = append(typeReads, q[changedObjectTypeIDParam])
		}
	}
	if want := [][]string{{"14", "20", "21"}}; !slices.EqualFunc(typeReads, want, slices.Equal[[]string]) {
		t.Errorf("type reads = %v, want %v", typeReads, want)
	}
}

// Object types narrows All as it does a scope: without the scope type or a
// descendant type in it, there's no type read, and the related read asks for
// the listed types.
func TestChanges_AllScopeObjectTypesNarrow(t *testing.T) {
	s := newScopedChangeServer(related(4, 4, "dcim.interface", 500, 99, "leaf9"))
	spec := withDescendants(allScope(""))
	spec.ObjectTypes = []string{"dcim.interface"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#4")
	reads := s.changeLog()
	if len(reads) != 1 || reads[0].Get(relatedObjectTypeParam) != "dcim.device" ||
		!slices.Equal(reads[0][changedObjectTypeIDParam], []string{"14"}) {
		t.Errorf("reads = %v, want only the related read, for interfaces", reads)
	}
}

// With two Actions, each All read is made once per Action, as a scope's are.
func TestChanges_AllScopeReadsPerAction(t *testing.T) {
	s := newScopedChangeServer()
	spec := allScope("")
	spec.Actions = []string{"create", "delete"}
	changesOK(t, s.start(t), spec)
	if got := len(s.changeLog()); got != 4 {
		t.Errorf("change-log reads = %d, want 4: two reads, once per Action", got)
	}
}

// The journal under All is every entry on an object of the Scope type: asked
// for by type, with no object ids, and checked by type.
func TestChanges_AllScopeJournal(t *testing.T) {
	s := newScopedChangeServer()
	s.journal = []journalRow{
		{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 1, text: "a"},
		{typ: "dcim.site", objID: 5, display: "ams", minutes: 2, text: "b"},
		{typ: "dcim.device", objID: 99, display: "leaf9", minutes: 3, text: "c"},
	}
	spec := allScope("")
	spec.Journal = true
	wantIDs(t, changesOK(t, s.start(t), spec), "leaf1", "leaf9")
	reads := s.requestsTo(journalEndpoint)
	if len(reads) != 1 || reads[0].Get(assignedObjectTypeIDParam) != "12" || reads[0].Has(assignedObjectIDParam) {
		t.Errorf("journal reads = %v, want one, for the device type, with no object ids", reads)
	}
}

// All reads descendant types for exactly the scope types Include descendants
// follows, so the editor's switch means the same under All.
func TestAllDescendantTypesMatchDescendantKinds(t *testing.T) {
	got := slices.Sorted(maps.Keys(allDescendantTypes))
	if want := slices.Sorted(maps.Keys(descendantKinds)); !slices.Equal(got, want) {
		t.Errorf("allDescendantTypes covers %v, descendantKinds %v", got, want)
	}
}

// A type with no field of its own can only be tied to its rows by the type map.
// Without it the related read could keep nothing, so it isn't made, rather than
// scan to its cap and draw a cut-off.
func TestChanges_AllScopeSkipsAnUntiableRelatedRead(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.site", 5))
	s.coreStatus = http.StatusForbidden
	wantIDs(t, changesOK(t, s.start(t), allScope("dcim.site")), "#1")
	reads := s.changeLog()
	if len(reads) != 1 || reads[0].Get(changedObjectTypeParam) != "dcim.site" || reads[0].Has(relatedObjectTypeParam) {
		t.Errorf("reads = %v, want only the site type read, by name", reads)
	}
}
