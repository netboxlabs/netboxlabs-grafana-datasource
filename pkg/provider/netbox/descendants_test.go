package netbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// membersSetup resolves spec's scope on s and reads the type map, then forgets
// those requests, so a test sees only the ones descendantMembers makes.
func membersSetup(t *testing.T, s *scopedChangeServer, spec provider.ChangeSpec) (*Provider, *resolvedScope, descendantKind, map[string]int) {
	t.Helper()
	p := s.start(t)
	scope := resolveOK(t, p, spec)
	ids, err := p.objectTypeIDMap(context.Background())
	if err != nil {
		t.Fatalf("objectTypeIDMap: %v", err)
	}
	kind, ok := descendantKinds[scope.Type]
	if !ok {
		t.Fatalf("no descendantKinds row for %s", scope.Type)
	}
	s.mu.Lock()
	s.requests, s.headers = nil, nil
	s.mu.Unlock()
	return p, scope, kind, ids
}

// failLog records each error descendantMembers passes to fail.
type failLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *failLog) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *failLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.errs)
}

// readMembers runs descendantMembers on a scope set up by membersSetup, wanting
// its interfaces and cables as asked.
func readMembers(p *Provider, spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, wantIfaces, wantCables bool) (members, error) {
	return readMembersWith(p, spec, scope, kind, ids, descendantWants{ifaces: wantIfaces, cables: wantCables}, &failLog{})
}

// readMembersWith runs descendantMembers on a scope set up by membersSetup,
// wanting want, under a fresh semaphore of cableEndWorkers slots. fails records
// what it passes to fail.
func readMembersWith(p *Provider, spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, want descendantWants, fails *failLog) (members, error) {
	return p.descendantMembers(context.Background(), fails.fail, make(chan struct{}, cableEndWorkers), spec, scope, kind, ids, want)
}

// membersWith fails the test on an error, or on any fail call.
func membersWith(t *testing.T, p *Provider, spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, want descendantWants) members {
	t.Helper()
	var fails failLog
	m, err := readMembersWith(p, spec, scope, kind, ids, want, &fails)
	if err != nil {
		t.Fatalf("descendantMembers: %v", err)
	}
	if n := fails.count(); n != 0 {
		t.Fatalf("descendantMembers called fail %d times (%v), want none", n, fails.errs)
	}
	return m
}

func membersOK(t *testing.T, p *Provider, spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, wantIfaces, wantCables bool) members {
	t.Helper()
	m, err := readMembers(p, spec, scope, kind, ids, wantIfaces, wantCables)
	if err != nil {
		t.Fatalf("descendantMembers: %v", err)
	}
	return m
}

func wantSet(t *testing.T, name string, got, want map[int64]int64) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// ifaceDelete is change id: interface iface deleted from device, minutes before
// scopedNow, recorded against the device.
func ifaceDelete(id, minutes int, iface, device int64) loggedChange {
	return ch(id, minutes, "dcim.interface", iface).relatedTo("dcim.device", device).as("delete").
		snapshots(fmt.Sprintf(`{"device":%d}`, device), "")
}

// cableEndChange is change id: an end of cable on termination termType termID,
// minutes before scopedNow, recorded against interface iface. A create carries
// the end in its after data, a delete in its before.
func cableEndChange(id, minutes int, action string, iface, cable int64, termType int, termID int64) loggedChange {
	end := fmt.Sprintf(`{"cable":%d,"cable_end":"A","termination_type":%d,"termination_id":%d}`, cable, termType, termID)
	c := ch(id, minutes, "dcim.cabletermination", int64(900+id)).relatedTo("dcim.interface", iface).as(action)
	if action == "delete" {
		return c.snapshots(end, "")
	}
	return c.snapshots("", end)
}

// ipOnType is change id: IP ip created or deleted on interface iface of type
// ifaceType ("dcim.interface", "virtualization.vminterface"), minutes before
// scopedNow, related to the interface as NetBox records it: a create carries
// the assignment in its after data, a delete in its before.
func ipOnType(id, minutes int, action string, ip int64, ifaceType string, iface int64) loggedChange {
	on := assignedTo(scopedTypeIDs[ifaceType], iface)
	c := ch(id, minutes, "ipam.ipaddress", ip).relatedTo(ifaceType, iface).as(action)
	if action == "delete" {
		return c.snapshots(on, "")
	}
	return c.snapshots("", on)
}

// ipOn is ipOnType on a dcim.interface.
func ipOn(id, minutes int, action string, ip, iface int64) loggedChange {
	return ipOnType(id, minutes, action, ip, "dcim.interface", iface)
}

// ipMoved is change id: IP ip updated from dcim interface from to interface to
// (0 is unassigned), minutes before scopedNow. NetBox records an update against
// the interface the IP is on after it, so an IP unassigned has no related
// object.
func ipMoved(id, minutes int, ip, from, to int64) loggedChange {
	on := func(iface int64) string {
		if iface == 0 {
			return unassigned
		}
		return assignedTo(scopedTypeIDs["dcim.interface"], iface)
	}
	c := ch(id, minutes, "ipam.ipaddress", ip).snapshots(on(from), on(to))
	if to != 0 {
		c = c.relatedTo("dcim.interface", to)
	}
	return c
}

// rangeSpec is leaf1 over [scopedNow-60m, scopedNow-10m].
func rangeSpec() provider.ChangeSpec {
	spec := nameScope("leaf1")
	spec.From, spec.To = scopedNow.Add(-60*time.Minute), scopedNow.Add(-10*time.Minute)
	return spec
}

// leaf1Children is leaf1 (17) with interfaces 100 and 101, leaf2 (18) with 102,
// and leaf1's interface deletes: 105 in the range, 106 after To (in: membership
// reads run to now) and 107 before From (out). The change log holds those
// deletes and changes, newest first.
func leaf1Children(changes ...loggedChange) *scopedChangeServer {
	rows := append(changes,
		ifaceDelete(1, 30, 105, 17),
		ifaceDelete(2, 5, 106, 17),
		ifaceDelete(3, 90, 107, 17),
	)
	slices.SortStableFunc(rows, func(a, b loggedChange) int { return cmp.Compare(a.minutes, b.minutes) })
	s := newScopedChangeServer(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 101, owner: 17}, {id: 102, owner: 18}}
	return s
}

func TestDescendantMembers_DeviceInterfaces(t *testing.T) {
	s := leaf1Children()
	spec := rangeSpec()
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersOK(t, p, spec, scope, kind, ids, true, false)
	wantSet(t, "ifaces", m.ifaces, map[int64]int64{100: 17, 101: 17, 105: 17, 106: 17})
	// The delete read ran out of rows: its oldest is the last row it read.
	if len(m.reads) != 1 || m.reads[0].capped || !m.reads[0].oldest.Equal(scopedNow.Add(-30*time.Minute)) {
		t.Errorf("reads = %+v, want the delete read, not capped, oldest at 30m", m.reads)
	}

	lists := s.requestsTo("dcim/interfaces")
	if len(lists) != 1 || !reflect.DeepEqual(lists[0]["device_id"], []string{"17"}) || lists[0].Get("brief") != "true" {
		t.Errorf("interface lists = %v, want one with device_id=17&brief=true", lists)
	}
	reads := s.changeLog()
	if len(reads) != 1 {
		t.Fatalf("change-log reads = %v, want one", reads)
	}
	q := reads[0]
	for param, want := range map[string][]string{
		"action":                 {"delete"},
		"changed_object_type_id": {"14"},
		"related_object_type":    {"dcim.device"},
		"related_object_id":      {"17"},
		"time_after":             {spec.From.Format(time.RFC3339)},
	} {
		if !reflect.DeepEqual(q[param], want) {
			t.Errorf("delete read %s = %v, want %v", param, q[param], want)
		}
	}
	if q.Has("time_before") {
		t.Errorf("delete read sent time_before=%s; membership reads run to now", q.Get("time_before"))
	}
}

// A NetBox that ignores related_object_id can't put another device's deleted
// interface on this one: the delete's own before data names the device, as a
// plain integer.
func TestDescendantMembers_DeleteReadChecksTheSnapshotOwner(t *testing.T) {
	s := newScopedChangeServer(
		ifaceDelete(1, 30, 105, 17),
		ifaceDelete(2, 31, 108, 18),
		ch(3, 32, "dcim.interface", 109).relatedTo("dcim.device", 17).as("delete").snapshots(`{"device":"17"}`, ""),
	)
	s.ignoreRelatedQ = true
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	wantSet(t, "ifaces", membersOK(t, p, spec, scope, kind, ids, true, false).ifaces, map[int64]int64{105: 17})
}

// A NetBox that ignores the lists' filters can't put another device's
// interfaces or cables on this one, or another interface's cable on an
// interface scope.
func TestDescendantMembers_ListRowsCheckedLocally(t *testing.T) {
	t.Run("device scope", func(t *testing.T) {
		s := newScopedChangeServer()
		s.interfaces = []childRow{{id: 100, owner: 17}, {id: 102, owner: 18}}
		s.cables = []cableRow{
			{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}, {"dcim.interface", 200, 18}}},
			{id: 302, ends: []cableEnd{{"dcim.interface", 102, 18}, {"dcim.interface", 201, 18}}},
		}
		s.ignoreListFilter = true
		spec := nameScope("leaf1")
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersOK(t, p, spec, scope, kind, ids, true, true)
		wantSet(t, "ifaces", m.ifaces, map[int64]int64{100: 17})
		wantSet(t, "cables", m.cables, map[int64]int64{300: 17})
	})
	t.Run("interface scope", func(t *testing.T) {
		s := newScopedChangeServer()
		s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}, {id: 101, owner: 17, cable: 302}}
		s.ignoreListFilter = true
		spec := idScope("100")
		spec.ScopeType = "dcim.interface"
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersOK(t, p, spec, scope, kind, ids, true, true)
		wantSet(t, "cables", m.cables, map[int64]int64{300: 100})
	})
}

// device_id is a choice filter on the interface list: one deleted device's id
// would make it a 400. A Match-by-ID scope sends only the ids that exist, and a
// deleted device's interface deletes still come from the delete read.
func TestDescendantMembers_DeletedScopeID(t *testing.T) {
	t.Run("one of two deleted", func(t *testing.T) {
		s := newScopedChangeServer()
		s.interfaces = []childRow{{id: 100, owner: 17}}
		spec := idScope("17", "99")
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersOK(t, p, spec, scope, kind, ids, true, false)
		wantSet(t, "ifaces", m.ifaces, map[int64]int64{100: 17})
		lists := s.requestsTo("dcim/interfaces")
		if len(lists) != 1 || !reflect.DeepEqual(lists[0]["device_id"], []string{"17"}) {
			t.Errorf("interface lists = %v, want one with device_id=17 only", lists)
		}
	})
	t.Run("only id deleted", func(t *testing.T) {
		s := newScopedChangeServer(ifaceDelete(1, 30, 105, 99))
		spec := idScope("99")
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersOK(t, p, spec, scope, kind, ids, true, false)
		wantSet(t, "ifaces", m.ifaces, map[int64]int64{105: 99})
		if lists := s.requestsTo("dcim/interfaces"); len(lists) != 0 {
			t.Errorf("interface lists = %v, want none", lists)
		}
		reads := s.changeLog()
		if len(reads) != 1 || !reflect.DeepEqual(reads[0]["related_object_id"], []string{"99"}) {
			t.Errorf("change-log reads = %v, want the delete read with related_object_id=99", reads)
		}
	})
}

func TestDescendantMembers_DeviceCables(t *testing.T) {
	s := leaf1Children(
		cableEndChange(10, 20, "delete", 100, 301, 14, 100),
		cableEndChange(11, 25, "delete", 105, 304, 14, 105),
		cableEndChange(12, 5, "create", 100, 303, 14, 100),
		cableEndChange(13, 20, "create", 100, 305, 99, 100),
	)
	// 306 is on leaf1 by its B side only.
	s.cables = []cableRow{
		{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}, {"dcim.interface", 200, 18}}},
		{id: 306, ends: []cableEnd{{"dcim.interface", 202, 18}, {"dcim.interface", 101, 17}}},
	}
	spec := rangeSpec()
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersOK(t, p, spec, scope, kind, ids, true, true)
	wantSet(t, "cables", m.cables, map[int64]int64{300: 17, 301: 17, 303: 17, 304: 17, 306: 17})

	lists := s.requestsTo("dcim/cables")
	if len(lists) != 1 || lists[0].Get("fields") != "id,a_terminations,b_terminations" ||
		!reflect.DeepEqual(lists[0]["device_id"], []string{"17"}) {
		t.Errorf("cable lists = %v, want one with device_id=17&fields=id,a_terminations,b_terminations", lists)
	}
	var ends []url.Values
	for _, q := range s.changeLog() {
		if q.Get("changed_object_type_id") == "24" {
			ends = append(ends, q)
		}
	}
	if len(ends) != 1 {
		t.Fatalf("cable-end reads = %v, want one", ends)
	}
	q := ends[0]
	if got := q["related_object_type"]; !reflect.DeepEqual(got, []string{"dcim.interface"}) {
		t.Errorf("cable-end read related_object_type = %v, want [dcim.interface]", got)
	}
	for _, id := range []string{"100", "101", "105", "106"} {
		if !slices.Contains(q["related_object_id"], id) {
			t.Errorf("cable-end read related_object_id = %v, want %s among them", q["related_object_id"], id)
		}
	}
	if q.Has("time_before") {
		t.Errorf("cable-end read sent time_before=%s; membership reads run to now", q.Get("time_before"))
	}
}

// A cable's owner is the one the cable list gave it, else the first match
// in the cable-end rows, newest first, the after side before the before side.
func TestDescendantMembers_CableOwners(t *testing.T) {
	moved := ch(10, 20, "dcim.cabletermination", 910).relatedTo("dcim.interface", 100).snapshots(
		`{"cable":301,"termination_type":14,"termination_id":200}`,
		`{"cable":301,"termination_type":14,"termination_id":100}`)
	s := newScopedChangeServer(
		moved,
		cableEndChange(11, 21, "delete", 200, 300, 14, 200),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}, {"dcim.interface", 200, 18}}}}
	spec := nameScope("leaf1", "leaf2")
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersOK(t, p, spec, scope, kind, ids, true, true)
	wantSet(t, "cables", m.cables, map[int64]int64{300: 17, 301: 17})
}

// A device with many interfaces splits its cable-end read into batches, each
// within the URL budget, which between them ask for every interface.
func TestDescendantMembers_CableEndsBatch(t *testing.T) {
	s := newScopedChangeServer()
	for id := int64(1000); id < 1600; id++ {
		s.interfaces = append(s.interfaces, childRow{id: id, owner: 17})
	}
	spec := rangeSpec()
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersOK(t, p, spec, scope, kind, ids, true, true)
	if len(m.ifaces) != 600 {
		t.Fatalf("ifaces = %d, want 600", len(m.ifaces))
	}
	sent := map[string]bool{}
	batches := 0
	for _, raw := range s.rawQueries(changeLogEndpoint) {
		q, _ := url.ParseQuery(raw)
		if q.Get("changed_object_type_id") != "24" {
			continue
		}
		batches++
		if len(raw) > chunkBudgetBytes {
			t.Errorf("cable-end query is %d bytes, over the %d budget", len(raw), chunkBudgetBytes)
		}
		for _, id := range q["related_object_id"] {
			sent[id] = true
		}
	}
	if batches < 2 {
		t.Errorf("cable-end reads = %d, want more than one", batches)
	}
	for id := 1000; id < 1600; id++ {
		if !sent[strconv.Itoa(id)] {
			t.Errorf("interface %d was in no cable-end read", id)
		}
	}
}

// An interface scope's interfaces are its own objects, whether or not the
// caller wants them read: its cable ends are read through them either way.
func TestDescendantMembers_InterfaceScope(t *testing.T) {
	for name, wantIfaces := range map[string]bool{"interfaces wanted": true, "cables only": false} {
		t.Run(name, func(t *testing.T) {
			s := newScopedChangeServer(cableEndChange(10, 20, "delete", 100, 301, 14, 100))
			s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}, {id: 101, owner: 17, cable: 302}}
			spec := idScope("100")
			spec.ScopeType = "dcim.interface"
			p, scope, kind, ids := membersSetup(t, s, spec)
			m := membersOK(t, p, spec, scope, kind, ids, wantIfaces, true)
			wantSet(t, "ifaces", m.ifaces, map[int64]int64{100: 100})
			wantSet(t, "cables", m.cables, map[int64]int64{300: 100, 301: 100})

			lists := s.requestsTo("dcim/interfaces")
			if len(lists) != 1 || !reflect.DeepEqual(lists[0]["id"], []string{"100"}) || lists[0].Get("brief") != "true" {
				t.Errorf("interface lists = %v, want one with id=100&brief=true", lists)
			}
			if lists := s.requestsTo("dcim/cables"); len(lists) != 0 {
				t.Errorf("cable lists = %v, want none", lists)
			}
			reads := s.changeLog()
			if len(reads) != 1 || reads[0].Get("changed_object_type_id") != "24" ||
				!reflect.DeepEqual(reads[0]["related_object_id"], []string{"100"}) {
				t.Errorf("change-log reads = %v, want one cable-end read related to 100", reads)
			}
		})
	}
}

func TestDescendantMembers_VMScope(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 30, "virtualization.vminterface", 501).relatedTo("virtualization.virtualmachine", 40).as("delete").
			snapshots(`{"virtual_machine":40}`, ""),
	)
	s.vms = []string{briefRow("virtualization/virtual-machines", 40, "name", "vm40", "vm40")}
	s.vmInterfaces = []childRow{{id: 500, owner: 40}}
	spec := idScope("40")
	spec.ScopeType = "virtualization.virtualmachine"
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersOK(t, p, spec, scope, kind, ids, true, true)
	wantSet(t, "ifaces", m.ifaces, map[int64]int64{500: 40, 501: 40})
	wantSet(t, "cables", m.cables, map[int64]int64{})

	lists := s.requestsTo("virtualization/interfaces")
	if len(lists) != 1 || !reflect.DeepEqual(lists[0]["virtual_machine_id"], []string{"40"}) || lists[0].Get("brief") != "true" {
		t.Errorf("VM interface lists = %v, want one with virtual_machine_id=40&brief=true", lists)
	}
	reads := s.changeLog()
	if len(reads) != 1 {
		t.Fatalf("change-log reads = %v, want the delete read only", reads)
	}
	if got := reads[0].Get("changed_object_type_id"); got != "31" {
		t.Errorf("delete read changed_object_type_id = %s, want 31", got)
	}
	if got := reads[0].Get("related_object_type"); got != "virtualization.virtualmachine" {
		t.Errorf("delete read related_object_type = %s, want virtualization.virtualmachine", got)
	}
	if lists := s.requestsTo("dcim/cables"); len(lists) != 0 {
		t.Errorf("cable lists = %v, want none", lists)
	}
}

// Up to NetBox 4.4.3 related_object_type takes an id: both related membership
// reads retry in that form, and their rows count.
func TestDescendantMembers_RelatedTypeRetriedAsID(t *testing.T) {
	s := leaf1Children(cableEndChange(10, 20, "delete", 100, 301, 14, 100))
	s.relatedByID = true
	spec := rangeSpec()
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersOK(t, p, spec, scope, kind, ids, true, true)
	wantSet(t, "ifaces", m.ifaces, map[int64]int64{100: 17, 101: 17, 105: 17, 106: 17})
	wantSet(t, "cables", m.cables, map[int64]int64{301: 17})

	forms := map[string][]string{}
	for _, q := range s.changeLog() {
		typ := q.Get("changed_object_type_id")
		forms[typ] = append(forms[typ], q.Get("related_object_type"))
	}
	if want := []string{"dcim.device", "12"}; !reflect.DeepEqual(forms["14"], want) {
		t.Errorf("delete read related_object_type = %v, want %v", forms["14"], want)
	}
	if want := []string{"dcim.interface", "14"}; !reflect.DeepEqual(forms["24"], want) {
		t.Errorf("cable-end read related_object_type = %v, want %v", forms["24"], want)
	}
}

// The first page's count decides a list over the cap, before another page is
// read.
func TestDescendantMembers_ListOverCapFailsOnFirstPage(t *testing.T) {
	s := newScopedChangeServer()
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 101, owner: 17}, {id: 102, owner: 17}}
	s.listPageSize = 1
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	p.childListCapOverride = 2
	_, err := readMembers(p, spec, scope, kind, ids, true, false)
	annotationErr(t, err, provider.AnnotationReasonTooManyChildren)
	if lists := s.requestsTo("dcim/interfaces"); len(lists) != 1 {
		t.Errorf("interface list requests = %d, want 1", len(lists))
	}
}

func TestDescendantMembers_ListPagesUnderTheCap(t *testing.T) {
	s := newScopedChangeServer()
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 101, owner: 17}}
	s.listPageSize = 1
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	wantSet(t, "ifaces", membersOK(t, p, spec, scope, kind, ids, true, false).ifaces, map[int64]int64{100: 17, 101: 17})
	if lists := s.requestsTo("dcim/interfaces"); len(lists) != 2 {
		t.Errorf("interface list requests = %d, want 2", len(lists))
	}
}

// listChildren's rows decide too, for a NetBox that doesn't count.
func TestListChildren_RowsOverCapWithoutCount(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pages.Add(1)
		_, _ = fmt.Fprintf(w, `{"count":null,"next":"http://%s%s?offset=%d","previous":null,"results":[{"id":%d}]}`,
			r.Host, r.URL.Path, n, n)
	}))
	t.Cleanup(srv.Close)
	p := New(srv.URL, "token", srv.Client())
	p.childListCapOverride = 2
	_, err := p.listChildren(context.Background(), "dcim/interfaces", url.Values{"device_id": {"17"}})
	annotationErr(t, err, provider.AnnotationReasonTooManyChildren)
	if n := pages.Load(); n != 3 {
		t.Errorf("pages read = %d, want 3", n)
	}
}

// Membership rows only build the sets; none may reach the track, capped or not.
func TestDescendantMembers_MembershipRowsCleared(t *testing.T) {
	s := leaf1Children(cableEndChange(10, 20, "delete", 100, 301, 14, 100))
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	p.changeScanCapOverride = 2
	m := membersOK(t, p, spec, scope, kind, ids, true, true)
	if len(m.reads) != 2 {
		t.Fatalf("reads = %d, want 2 (the delete read and one cable-end read)", len(m.reads))
	}
	var capped []bool
	for i, r := range m.reads {
		if len(r.rows) != 0 || !r.scan || !r.descendant {
			t.Errorf("reads[%d]: %d rows, scan %v, descendant %v; want no rows, scan and descendant", i, len(r.rows), r.scan, r.descendant)
		}
		capped = append(capped, r.capped)
	}
	if !reflect.DeepEqual(capped, []bool{true, false}) {
		t.Errorf("capped = %v, want [true false]: the delete read stops at 2 of its 3 rows", capped)
	}
}

func TestDescendantMembers_ListForbidden(t *testing.T) {
	s := newScopedChangeServer()
	s.listStatus = http.StatusForbidden
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	_, err := readMembers(p, spec, scope, kind, ids, true, true)
	annotationErr(t, err, provider.AnnotationReasonDescendantsForbidden)
}

// cableEndRead is leaf1 over [scopedNow-60m, to], with cable ends on interface
// 100 at the given minutes before scopedNow, read maxRows rows at most (0: the
// default cap). It returns the cable-end read's result, after the delete read's.
func cableEndRead(t *testing.T, to time.Time, maxRows int, minutes ...int) changeReadResult {
	t.Helper()
	var rows []loggedChange
	for i, m := range minutes {
		rows = append(rows, cableEndChange(10+i, m, "create", 100, int64(301+i), 14, 100))
	}
	s := newScopedChangeServer(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	spec := nameScope("leaf1")
	spec.From, spec.To = scopedNow.Add(-60*time.Minute), to
	p, scope, kind, ids := membersSetup(t, s, spec)
	p.changeScanCapOverride = maxRows
	m := membersOK(t, p, spec, scope, kind, ids, true, true)
	if len(m.reads) != 2 {
		t.Fatalf("reads = %d, want 2 (the delete read and one cable-end read)", len(m.reads))
	}
	r := m.reads[1]
	if !r.scan || !r.descendant || len(r.rows) != 0 {
		t.Errorf("cable-end read: %d rows, scan %v, descendant %v; want no rows, scan and descendant", len(r.rows), r.scan, r.descendant)
	}
	return r
}

// A membership read capped after To may have missed members that changed
// anywhere in the range, so its cut-off is To.
func TestDescendantMembers_CappedReadClampsToTo(t *testing.T) {
	to := scopedNow.Add(-10 * time.Minute)
	r := cableEndRead(t, to, 2, 1, 2, 3)
	if !r.capped || !r.oldest.Equal(to) {
		t.Errorf("capped %v, oldest %v; want capped at To %v", r.capped, r.oldest, to)
	}
}

func TestDescendantMembers_NoClampWithoutTo(t *testing.T) {
	r, want := cableEndRead(t, time.Time{}, 2, 1, 2, 3), scopedNow.Add(-2*time.Minute)
	if !r.capped || !r.oldest.Equal(want) {
		t.Errorf("capped %v, oldest %v; want capped at the oldest row read, %v", r.capped, r.oldest, want)
	}
}

// Only a capped read is clamped, and only when it stopped after To.
func TestDescendantMembers_ClampOnlyCappedReadsAfterTo(t *testing.T) {
	to := scopedNow.Add(-10 * time.Minute)
	t.Run("not capped", func(t *testing.T) {
		r, want := cableEndRead(t, to, 0, 1, 2, 3), scopedNow.Add(-3*time.Minute)
		if r.capped || !r.oldest.Equal(want) {
			t.Errorf("capped %v, oldest %v; want not capped, at the oldest row read, %v", r.capped, r.oldest, want)
		}
	})
	t.Run("capped before To", func(t *testing.T) {
		r, want := cableEndRead(t, to, 3, 1, 2, 15, 20), scopedNow.Add(-15*time.Minute)
		if !r.capped || !r.oldest.Equal(want) {
			t.Errorf("capped %v, oldest %v; want capped at the oldest row read, %v", r.capped, r.oldest, want)
		}
	})
}

func TestDescendantMembers_NothingWantedNoRequests(t *testing.T) {
	s := leaf1Children(cableEndChange(10, 20, "delete", 100, 301, 14, 100))
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersOK(t, p, spec, scope, kind, ids, false, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 0 {
		t.Errorf("requests = %v, want none", s.requests)
	}
	if len(m.ifaces) != 0 || len(m.cables) != 0 || len(m.reads) != 0 {
		t.Errorf("members = %+v, want empty", m)
	}
}

// A device's modules and services come from their lists, by the device. Each
// row is checked here, whether NetBox honoured the lists' filters or not: a
// module on leaf2, a service on VM 17 and one on leaf2 don't join. Interfaces
// aren't listed for them, but their deletes are read, for the modules they
// name.
func TestDescendantMembers_ModulesAndServices(t *testing.T) {
	for name, ignore := range map[string]bool{"filters honoured": false, "filters ignored": true} {
		t.Run(name, func(t *testing.T) {
			s := newScopedChangeServer()
			s.modules = []childRow{{id: 500, owner: 17}, {id: 501, owner: 18}}
			s.services = []serviceRow{
				{id: 600, parentType: "dcim.device", parent: 17},
				{id: 601, parentType: "virtualization.virtualmachine", parent: 17},
				{id: 602, parentType: "dcim.device", parent: 18},
			}
			s.ignoreListFilter = ignore
			spec := nameScope("leaf1")
			p, scope, kind, ids := membersSetup(t, s, spec)
			m := membersWith(t, p, spec, scope, kind, ids, descendantWants{modules: true, services: true})
			wantSet(t, "modules", m.modules, map[int64]int64{500: 17})
			wantSet(t, "services", m.services, map[int64]int64{600: 17})
			if m.degraded {
				t.Error("degraded = true, want false: no list was refused")
			}

			modules := s.requestsTo("dcim/modules")
			if len(modules) != 1 || !reflect.DeepEqual(modules[0]["device_id"], []string{"17"}) ||
				!reflect.DeepEqual(modules[0]["fields"], []string{"id,device"}) {
				t.Errorf("module lists = %v, want one with device_id=17&fields=id,device", modules)
			}
			const serviceFields = "id,parent_object_type,parent_object_id,device,virtual_machine"
			services := s.requestsTo("ipam/services")
			if len(services) != 1 || !reflect.DeepEqual(services[0]["device_id"], []string{"17"}) ||
				!reflect.DeepEqual(services[0]["fields"], []string{serviceFields}) {
				t.Errorf("service lists = %v, want one with device_id=17&fields=%s", services, serviceFields)
			}
			if lists := s.requestsTo("dcim/interfaces"); len(lists) != 0 {
				t.Errorf("interface lists = %v, want none: interfaces weren't wanted", lists)
			}
			reads := s.changeLog()
			if len(reads) != 1 || reads[0].Get("action") != "delete" || reads[0].Get("changed_object_type_id") != "14" {
				t.Errorf("change-log reads = %v, want the interface-delete read only", reads)
			}
		})
	}
}

// NetBox 4.2's service rows name their device or VM instead of a parent
// object. A VM's services are listed by virtual_machine_id, and a VM has no
// modules to list, though the caller wants them.
func TestDescendantMembers_Services42Shape(t *testing.T) {
	t.Run("device", func(t *testing.T) {
		s := newScopedChangeServer()
		s.servicesV42 = true
		s.ignoreListFilter = true
		s.services = []serviceRow{
			{id: 600, parentType: "dcim.device", parent: 17},
			{id: 601, parentType: "virtualization.virtualmachine", parent: 17},
		}
		spec := nameScope("leaf1")
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersWith(t, p, spec, scope, kind, ids, descendantWants{services: true})
		wantSet(t, "services", m.services, map[int64]int64{600: 17})
	})
	t.Run("VM", func(t *testing.T) {
		s := newScopedChangeServer()
		s.servicesV42 = true
		s.vms = []string{briefRow("virtualization/virtual-machines", 40, "name", "vm40", "vm40")}
		s.services = []serviceRow{{id: 601, parentType: "virtualization.virtualmachine", parent: 40}}
		spec := idScope("40")
		spec.ScopeType = "virtualization.virtualmachine"
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersWith(t, p, spec, scope, kind, ids, descendantWants{modules: true, services: true})
		wantSet(t, "services", m.services, map[int64]int64{601: 40})
		wantSet(t, "modules", m.modules, map[int64]int64{})

		lists := s.requestsTo("ipam/services")
		if len(lists) != 1 || !reflect.DeepEqual(lists[0]["virtual_machine_id"], []string{"40"}) || lists[0].Has("device_id") {
			t.Errorf("service lists = %v, want one with virtual_machine_id=40 only", lists)
		}
		if lists := s.requestsTo("dcim/modules"); len(lists) != 0 {
			t.Errorf("module lists = %v, want none: a VM has no modules", lists)
		}
	})
}

// A module deleted along with its interfaces is gone from the module list, but
// their deletes' before data names it, on the device they were on. The delete
// read runs for modules alone, and counts toward the cut-off; the interface
// list doesn't run.
func TestDescendantMembers_ModuleIDsFromInterfaceDeletes(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 30, "dcim.interface", 105).relatedTo("dcim.device", 17).as("delete").snapshots(`{"device":17,"module":502}`, ""),
	)
	spec := rangeSpec()
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersWith(t, p, spec, scope, kind, ids, descendantWants{modules: true})
	wantSet(t, "modules", m.modules, map[int64]int64{502: 17})
	if len(m.reads) != 1 {
		t.Errorf("reads = %d, want the delete read's", len(m.reads))
	}
	if lists := s.requestsTo("dcim/interfaces"); len(lists) != 0 {
		t.Errorf("interface lists = %v, want none: interfaces weren't wanted", lists)
	}
}

// A Match-by-ID scope may name a deleted device, which the interface and module
// lists refuse with a 400 (the service list too, on NetBox 4.2). Which scope
// devices exist is read once, for every list, and only those are listed. A
// deleted device's modules still come from its interface deletes.
func TestDescendantMembers_ExistingScopeObjectsReadOnce(t *testing.T) {
	server := func() *scopedChangeServer {
		s := newScopedChangeServer(
			ch(1, 30, "dcim.interface", 105).relatedTo("dcim.device", 99).as("delete").snapshots(`{"device":99,"module":503}`, ""),
		)
		s.interfaces = []childRow{{id: 100, owner: 17}}
		s.modules = []childRow{{id: 500, owner: 17}}
		s.services = []serviceRow{{id: 600, parentType: "dcim.device", parent: 17}}
		return s
	}
	want := descendantWants{ifaces: true, modules: true, services: true}
	lists := []string{"dcim/interfaces", "dcim/modules", "ipam/services"}
	t.Run("one of two deleted", func(t *testing.T) {
		s := server()
		spec := idScope("17", "99")
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersWith(t, p, spec, scope, kind, ids, want)
		wantSet(t, "modules", m.modules, map[int64]int64{500: 17, 503: 99})
		wantSet(t, "services", m.services, map[int64]int64{600: 17})
		if reads := s.requestsTo("dcim/devices"); len(reads) != 1 {
			t.Errorf("device lists = %v, want one: the existence read, shared", reads)
		}
		for _, path := range lists {
			if got := s.requestsTo(path); len(got) != 1 || !reflect.DeepEqual(got[0]["device_id"], []string{"17"}) {
				t.Errorf("%s lists = %v, want one with device_id=17 only", path, got)
			}
		}
	})
	t.Run("only id deleted", func(t *testing.T) {
		s := server()
		spec := idScope("99")
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersWith(t, p, spec, scope, kind, ids, want)
		wantSet(t, "modules", m.modules, map[int64]int64{503: 99})
		for _, path := range lists {
			if got := s.requestsTo(path); len(got) != 0 {
				t.Errorf("%s lists = %v, want none", path, got)
			}
		}
	})
}

var errBoom = errors.New("boom")

// endedWithBoom is a context cancelled with cause errBoom.
func endedWithBoom() context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errBoom)
	return ctx
}

// endsAfter is a context ended with errBoom whose Err reports it only after
// its first live calls, and whose Done never closes: takeSlot's select can't
// see it end, only its Err checks can, so each check is pinned on its own.
type endsAfter struct {
	context.Context // ended with errBoom, so context.Cause finds it
	live            int32
	calls           atomic.Int32
}

func (c *endsAfter) Err() error {
	if c.calls.Add(1) <= c.live {
		return nil
	}
	return context.Canceled
}

func (c *endsAfter) Done() <-chan struct{} { return nil }

// takeSlot either holds a slot and returns nil, or holds none and returns the
// context's cause. A free slot isn't kept once the context has ended, and a
// full semaphore's slots stay with whoever holds them.
func TestTakeSlot(t *testing.T) {
	t.Run("free slot", func(t *testing.T) {
		sem := make(chan struct{}, 1)
		if err := takeSlot(context.Background(), sem); err != nil || len(sem) != 1 {
			t.Errorf("takeSlot = %v, %d slots held; want nil, 1", err, len(sem))
		}
	})
	t.Run("free slot, context ended", func(t *testing.T) {
		sem := make(chan struct{}, 1)
		if err := takeSlot(endedWithBoom(), sem); !errors.Is(err, errBoom) || len(sem) != 0 {
			t.Errorf("takeSlot = %v, %d slots held; want %v, none", err, len(sem), errBoom)
		}
	})
	t.Run("full, context ended", func(t *testing.T) {
		sem := make(chan struct{}, 1)
		sem <- struct{}{}
		if err := takeSlot(endedWithBoom(), sem); !errors.Is(err, errBoom) || len(sem) != 1 {
			t.Errorf("takeSlot = %v, %d slots held; want %v, 1", err, len(sem), errBoom)
		}
	})
	// The context ends between the first check and the select, which wins the
	// free slot: the slot goes back.
	t.Run("slot won as the context ended", func(t *testing.T) {
		sem := make(chan struct{}, 1)
		if err := takeSlot(&endsAfter{Context: endedWithBoom(), live: 1}, sem); !errors.Is(err, errBoom) || len(sem) != 0 {
			t.Errorf("takeSlot = %v, %d slots held; want %v, none", err, len(sem), errBoom)
		}
	})
	// Only the first check can return here: the select would wait on the full
	// semaphore for good.
	t.Run("full, ended before the select", func(t *testing.T) {
		sem := make(chan struct{}, 1)
		sem <- struct{}{}
		done := make(chan error, 1)
		go func() { done <- takeSlot(&endsAfter{Context: endedWithBoom()}, sem) }()
		select {
		case err := <-done:
			if !errors.Is(err, errBoom) || len(sem) != 1 {
				t.Errorf("takeSlot = %v, %d slots held; want %v, 1", err, len(sem), errBoom)
			}
		case <-time.After(time.Second):
			t.Fatal("takeSlot still waiting 1s after its context ended")
		}
	})
}

// A cable-end batch cut while it waits for a slot fails with the context's
// cause: never no rows and no error, which would read as a batch that found
// nothing. It sends nothing, and leaves the slot another read holds alone.
func TestCableEndReads_CancelledWhileWaitingForASlot(t *testing.T) {
	s := newScopedChangeServer()
	p := s.start(t)
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	time.AfterFunc(20*time.Millisecond, func() { cancel(errBoom) })

	done := make(chan []changeReadResult, 1)
	go func() {
		done <- p.cableEndReads(ctx, func(error) {}, sem, rangeSpec(), 14, 24, map[int64]int64{100: 17, 101: 17})
	}()
	var results []changeReadResult
	select {
	case results = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cableEndReads still blocked 2s after its context was cancelled")
	}
	if len(results) == 0 {
		t.Fatal("no results, want one per batch")
	}
	for i, r := range results {
		if !errors.Is(r.err, errBoom) {
			t.Errorf("results[%d].err = %v, want %v", i, r.err, errBoom)
		}
	}
	if len(sem) != 1 {
		t.Errorf("%d slots held, want 1: the slot held before the call", len(sem))
	}
	if reads := s.changeLog(); len(reads) != 0 {
		t.Errorf("change-log reads = %v, want none", reads)
	}
}

// A list the token may not read (401 or 403), or one over the cap, leaves only
// its own type to the change log: the other members stand, and nothing is
// cancelled. A refused module list still keeps the modules the interface
// deletes name; a refused MAC list leaves no MACs, since no change names their
// interface. The interface list refused fails, as it always has.
func TestDescendantMembers_ListRefusedFailsOnlyItsType(t *testing.T) {
	server := func() *scopedChangeServer {
		s := newScopedChangeServer(
			ch(1, 30, "dcim.interface", 105).relatedTo("dcim.device", 17).as("delete").snapshots(`{"device":17,"module":502}`, ""),
		)
		s.interfaces = []childRow{{id: 100, owner: 17}}
		s.macs = []macRow{{id: 403, ct: "dcim.interface", iface: 100}}
		s.modules = []childRow{{id: 500, owner: 17}, {id: 501, owner: 17}}
		s.services = []serviceRow{{id: 600, parentType: "dcim.device", parent: 17}}
		return s
	}
	want := descendantWants{ifaces: true, modules: true, services: true}
	for _, tc := range []struct {
		name     string
		setup    func(*scopedChangeServer)
		listCap  int
		macs     map[int64]int64 // non-nil: MACs are wanted too, and these are found
		modules  map[int64]int64
		services map[int64]int64
	}{
		{
			name:    "services forbidden",
			setup:   func(s *scopedChangeServer) { s.listStatusFor = map[string]int{"ipam/services": http.StatusForbidden} },
			modules: map[int64]int64{500: 17, 501: 17, 502: 17}, services: map[int64]int64{},
		},
		{
			name:    "modules unauthorized",
			setup:   func(s *scopedChangeServer) { s.listStatusFor = map[string]int{"dcim/modules": http.StatusUnauthorized} },
			modules: map[int64]int64{502: 17}, services: map[int64]int64{600: 17},
		},
		{
			name:    "modules over the cap",
			setup:   func(s *scopedChangeServer) { s.modules = append(s.modules, childRow{id: 504, owner: 17}) },
			listCap: 2,
			modules: map[int64]int64{502: 17}, services: map[int64]int64{600: 17},
		},
		{
			name: "MACs forbidden",
			setup: func(s *scopedChangeServer) {
				s.listStatusFor = map[string]int{"dcim/mac-addresses": http.StatusForbidden}
			},
			macs: map[int64]int64{}, modules: map[int64]int64{500: 17, 501: 17, 502: 17}, services: map[int64]int64{600: 17},
		},
		{
			name: "MACs, modules and services forbidden",
			setup: func(s *scopedChangeServer) {
				s.listStatusFor = map[string]int{
					"dcim/mac-addresses": http.StatusForbidden, "dcim/modules": http.StatusForbidden, "ipam/services": http.StatusForbidden,
				}
			},
			macs: map[int64]int64{}, modules: map[int64]int64{502: 17}, services: map[int64]int64{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := server()
			tc.setup(s)
			spec := rangeSpec()
			p, scope, kind, ids := membersSetup(t, s, spec)
			p.childListCapOverride = tc.listCap
			want := want
			want.macs = tc.macs != nil
			var fails failLog
			m, err := readMembersWith(p, spec, scope, kind, ids, want, &fails)
			if err != nil {
				t.Fatalf("descendantMembers: %v", err)
			}
			if !m.degraded {
				t.Error("degraded = false, want true")
			}
			if n := fails.count(); n != 0 {
				t.Errorf("fail called %d times (%v), want none", n, fails.errs)
			}
			if tc.macs != nil {
				wantSet(t, "macs", m.macs, tc.macs)
			}
			wantSet(t, "modules", m.modules, tc.modules)
			wantSet(t, "services", m.services, tc.services)
			wantSet(t, "ifaces", m.ifaces, map[int64]int64{100: 17, 105: 17})
		})
	}
	t.Run("interfaces forbidden", func(t *testing.T) {
		s := server()
		s.listStatusFor = map[string]int{"dcim/interfaces": http.StatusForbidden}
		spec := rangeSpec()
		p, scope, kind, ids := membersSetup(t, s, spec)
		var fails failLog
		_, err := readMembersWith(p, spec, scope, kind, ids, want, &fails)
		annotationErr(t, err, provider.AnnotationReasonDescendantsForbidden)
		if n := fails.count(); n != 1 {
			t.Errorf("fail called %d times (%v), want once", n, fails.errs)
		}
	})
	// A Match-by-ID scope's existence read refused is refused for the lists it
	// serves: with interfaces not wanted, the module and service lists.
	t.Run("existence read forbidden", func(t *testing.T) {
		s := server()
		s.listStatusFor = map[string]int{"dcim/devices": http.StatusForbidden}
		spec := idScope("17")
		p, scope, kind, ids := membersSetup(t, s, spec)
		var fails failLog
		m, err := readMembersWith(p, spec, scope, kind, ids, descendantWants{modules: true, services: true}, &fails)
		if err != nil {
			t.Fatalf("descendantMembers: %v", err)
		}
		if !m.degraded {
			t.Error("degraded = false, want true")
		}
		if n := fails.count(); n != 0 {
			t.Errorf("fail called %d times (%v), want none", n, fails.errs)
		}
		wantSet(t, "modules", m.modules, map[int64]int64{502: 17})
		wantSet(t, "services", m.services, map[int64]int64{})
		for _, path := range []string{"dcim/modules", "ipam/services"} {
			if lists := s.requestsTo(path); len(lists) != 0 {
				t.Errorf("%s lists = %v, want none", path, lists)
			}
		}
	})
}

// A list cut by ctx gets ctx's cause, which may look like a refusal: the
// interface list's descendants-forbidden, handed to fail, or a 401 the scope's
// own read hands to the caller's cancel. That isn't the list's own refusal: it
// never ran, so its type isn't degraded, and the read fails.
func TestDescendantMembers_CutListIsNotRefused(t *testing.T) {
	for name, cause := range map[string]error{
		"descendants-forbidden": &AnnotationError{Reason: provider.AnnotationReasonDescendantsForbidden},
		"401":                   &APIError{Status: http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			s := newScopedChangeServer()
			s.services = []serviceRow{{id: 600, parentType: "dcim.device", parent: 17}}
			spec := nameScope("leaf1")
			p, scope, kind, ids := membersSetup(t, s, spec)
			// The one slot is held, so the service list waits for it.
			sem := make(chan struct{}, 1)
			sem <- struct{}{}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			time.AfterFunc(20*time.Millisecond, func() { cancel(cause) })
			m, err := p.descendantMembers(ctx, func(error) {}, sem, spec, scope, kind, ids, descendantWants{services: true})
			if !errors.Is(err, cause) {
				t.Errorf("err = %v, want the cut's cause %v", err, cause)
			}
			if m.degraded {
				t.Error("degraded = true, want false: the list was cut, not refused")
			}
			if lists := s.requestsTo("ipam/services"); len(lists) != 0 {
				t.Errorf("service lists = %v, want none", lists)
			}
		})
	}
	// Guard: as the query runs it, fail cancels ctx. The interface list's 403
	// is the error, whatever the cancel cut.
	t.Run("interface list forbidden, cancelling", func(t *testing.T) {
		s := newScopedChangeServer()
		s.interfaces = []childRow{{id: 100, owner: 17}}
		s.modules = []childRow{{id: 500, owner: 17}}
		s.services = []serviceRow{{id: 600, parentType: "dcim.device", parent: 17}}
		s.listStatusFor = map[string]int{"dcim/interfaces": http.StatusForbidden}
		spec := nameScope("leaf1")
		p, scope, kind, ids := membersSetup(t, s, spec)
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		m, err := p.descendantMembers(ctx, cancel, make(chan struct{}, 1), spec, scope, kind, ids,
			descendantWants{ifaces: true, modules: true, services: true})
		annotationErr(t, err, provider.AnnotationReasonDescendantsForbidden)
		if m.degraded {
			t.Error("degraded = true, want false")
		}
	})
}

// readsCarrying is the change-log requests that carry any of the type ids.
func readsCarrying(s *scopedChangeServer, typeIDs ...int) []url.Values {
	return slices.DeleteFunc(s.changeLog(), func(q url.Values) bool { return !carries(q, typeIDs...) })
}

// An IP's changes recorded against the scope's interfaces make it a member,
// whenever they were made: the read has no start, so an IP created before From
// is in. An interface deleted since From still counts. The read asks only for
// IP changes, newest first, with no range.
func TestDescendantMembers_IPHistory(t *testing.T) {
	s := leaf1Children(
		ipOn(10, 120, "create", 401, 100), // before From: the read has no start
		ipMoved(11, 40, 402, 101, 101),    // an edit while on 101
		ipOn(12, 20, "create", 403, 102),  // on leaf2's interface
		ipOn(13, 35, "delete", 404, 105),  // on an interface deleted since
	)
	spec := rangeSpec()
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, ips: true})
	wantSet(t, "ips", m.ips, map[int64]int64{401: 17, 402: 17, 404: 17})

	history := readsCarrying(s, 20)
	if len(history) != 1 {
		t.Fatalf("IP-history reads = %v, want one", history)
	}
	q := history[0]
	if got := q["related_object_type"]; !reflect.DeepEqual(got, []string{"dcim.interface"}) {
		t.Errorf("related_object_type = %v, want [dcim.interface]", got)
	}
	if got := q["changed_object_type_id"]; !reflect.DeepEqual(got, []string{"20"}) {
		t.Errorf("changed_object_type_id = %v, want [20]", got)
	}
	if got := sortedInts(t, q["related_object_id"]); !reflect.DeepEqual(got, []int64{100, 101, 105, 106}) {
		t.Errorf("related_object_id = %v, want [100 101 105 106]", got)
	}
	if got := q["ordering"]; !reflect.DeepEqual(got, []string{"-time"}) {
		t.Errorf("ordering = %v, want [-time]", got)
	}
	if q.Has("time_after") || q.Has("time_before") {
		t.Errorf("IP-history read %v: want no time_after and no time_before", q)
	}
	if len(m.reads) != 2 {
		t.Fatalf("reads = %d, want 2 (the delete read and the IP-history read)", len(m.reads))
	}
	if r := m.reads[1]; len(r.rows) != 0 || !r.scan || !r.descendant {
		t.Errorf("IP-history read: %d rows, scan %v, descendant %v; want no rows, scan and descendant", len(r.rows), r.scan, r.descendant)
	}

	// A VM's IPs are read against its VM interfaces, not dcim.interface.
	t.Run("VM", func(t *testing.T) {
		s := newScopedChangeServer(ipOnType(20, 20, "create", 410, "virtualization.vminterface", 500))
		s.vms = []string{briefRow("virtualization/virtual-machines", 40, "name", "vm40", "vm40")}
		s.vmInterfaces = []childRow{{id: 500, owner: 40}}
		spec := idScope("40")
		spec.ScopeType = "virtualization.virtualmachine"
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, ips: true})
		wantSet(t, "ips", m.ips, map[int64]int64{410: 40})

		history := readsCarrying(s, 20)
		if len(history) != 1 {
			t.Fatalf("IP-history reads = %v, want one", history)
		}
		if got := history[0]["related_object_type"]; !reflect.DeepEqual(got, []string{"virtualization.vminterface"}) {
			t.Errorf("related_object_type = %v, want [virtualization.vminterface]", got)
		}
		if got := history[0]["changed_object_type_id"]; !reflect.DeepEqual(got, []string{"20"}) {
			t.Errorf("changed_object_type_id = %v, want [20]", got)
		}
	})
}

// A NetBox that ignores the related and type filters returns every change:
// each row is checked by its own type and snapshot, the interface's type and
// id together. An IP on leaf2's interface, or on a VM interface sharing
// leaf1's interface id, isn't a member, nor is a MAC on leaf1's interface.
func TestDescendantMembers_IPHistoryChecksTheSnapshot(t *testing.T) {
	s := newScopedChangeServer(
		ipOn(9, 19, "create", 401, 100),  // on leaf1's interface: kept
		ipOn(10, 20, "create", 403, 102), // leaf2's
		created(11, 21, "ipam.ipaddress", 405, assignedTo(31, 100)).relatedTo("virtualization.vminterface", 100),
		created(12, 22, "dcim.macaddress", 406, assignedTo(14, 100)).relatedTo("dcim.interface", 100),
	)
	s.ignoreRelatedQ, s.ignoreChangedQ = true, true
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 102, owner: 18}}
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, ips: true})
	wantSet(t, "ips", m.ips, map[int64]int64{401: 17})
}

// Up to NetBox 4.4.3 related_object_type takes an id: the IP-history read
// retries with the interface type's id, and its rows count.
func TestDescendantMembers_IPHistoryRelatedTypeRetried(t *testing.T) {
	s := newScopedChangeServer(ipOn(10, 20, "create", 401, 100))
	s.relatedByID = true
	s.interfaces = []childRow{{id: 100, owner: 17}}
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, ips: true})
	wantSet(t, "ips", m.ips, map[int64]int64{401: 17})

	var forms []string
	for _, q := range readsCarrying(s, 20) {
		forms = append(forms, q.Get("related_object_type"))
	}
	if want := []string{"dcim.interface", "14"}; !reflect.DeepEqual(forms, want) {
		t.Errorf("IP-history related_object_type = %v, want %v", forms, want)
	}
}

// IP churn can't use up the cable ends' cap: the IP history and the cable ends
// are read apart, each to its own cap.
func TestDescendantMembers_IPHistoryApartFromCableEnds(t *testing.T) {
	s := newScopedChangeServer(
		ipOn(1, 1, "create", 401, 100),
		ipOn(2, 2, "create", 402, 100),
		ipOn(3, 3, "create", 403, 100),
		cableEndChange(20, 4, "create", 100, 301, 14, 100),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	p.changeScanCapOverride = 2
	m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, ips: true, cables: true})
	if _, ok := m.cables[301]; !ok {
		t.Errorf("cables = %v, want 301: its end is past the IPs' cap", m.cables)
	}
	if history := readsCarrying(s, 20); len(history) == 0 {
		t.Error("no IP-history read")
	}
	if len(m.ips) != 2 {
		t.Errorf("ips = %v, want the 2 the capped history read", m.ips)
	}
	for _, q := range s.changeLog() {
		if carries(q, 20) && carries(q, 24) {
			t.Errorf("change-log read %v asks for IPs and cable ends together", q)
		}
	}
}

// The IP-history read has no start, so its cap can stop it past From. It read
// every IP change in the range then, so it isn't a cut-off. Stopped before
// From, it is, as any membership read is; and with no From there's none to
// pass.
func TestDescendantMembers_IPHistoryCapPastFrom(t *testing.T) {
	from := scopedNow.Add(-60 * time.Minute)
	for _, tc := range []struct {
		name    string
		from    time.Time
		minutes []int
		capped  bool
		oldest  time.Time
	}{
		{"passed From", from, []int{30, 90, 120}, false, scopedNow.Add(-90 * time.Minute)},
		{"stopped before From", from, []int{5, 20, 30}, true, scopedNow.Add(-20 * time.Minute)},
		{"no From", time.Time{}, []int{30, 90, 120}, true, scopedNow.Add(-90 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []loggedChange
			for i, minutes := range tc.minutes {
				rows = append(rows, ipOn(10+i, minutes, "create", int64(401+i), 100))
			}
			s := newScopedChangeServer(rows...)
			s.interfaces = []childRow{{id: 100, owner: 17}}
			spec := nameScope("leaf1")
			spec.From = tc.from
			p, scope, kind, ids := membersSetup(t, s, spec)
			p.changeScanCapOverride = 2
			m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, ips: true})
			if len(m.reads) != 2 {
				t.Fatalf("reads = %d, want 2 (the delete read and the IP-history read)", len(m.reads))
			}
			if r := m.reads[1]; r.capped != tc.capped || !r.oldest.Equal(tc.oldest) {
				t.Errorf("IP-history read capped %v, oldest %v; want %v, %v", r.capped, r.oldest, tc.capped, tc.oldest)
			}
			wantSet(t, "ips", m.ips, map[int64]int64{401: 17, 402: 17})
		})
	}
}

// The MAC list is filtered by assigned_object_id alone, a number on every
// NetBox version. assigned_object_type takes an id up to 4.4.3 and a name from
// 4.4.4, so it isn't sent, and each row's type string is checked here. The same
// MACs are found on both, whether NetBox applied the filter or not: not a MAC
// on a VM interface sharing an interface's id, nor one on leaf2's interface.
func TestDescendantMembers_MACListAcrossVersions(t *testing.T) {
	for _, byID := range []bool{false, true} {
		for _, ignore := range []bool{false, true} {
			t.Run(fmt.Sprintf("relatedByID %v, ignoreListFilter %v", byID, ignore), func(t *testing.T) {
				s := leaf1Children()
				s.macs = []macRow{
					{id: 403, ct: "dcim.interface", iface: 101},
					{id: 404, ct: "virtualization.vminterface", iface: 101},
					{id: 405, ct: "dcim.interface", iface: 102},
				}
				s.relatedByID, s.ignoreListFilter = byID, ignore
				// With no range, interface 107, deleted before From, would join.
				spec := rangeSpec()
				p, scope, kind, ids := membersSetup(t, s, spec)
				m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, macs: true})
				wantSet(t, "macs", m.macs, map[int64]int64{403: 17})

				lists := s.requestsTo("dcim/mac-addresses")
				if len(lists) != 1 {
					t.Fatalf("MAC lists = %v, want one", lists)
				}
				q := lists[0]
				if q.Has("assigned_object_type") {
					t.Errorf("MAC list %v: want no assigned_object_type", q)
				}
				if got := q["fields"]; !reflect.DeepEqual(got, []string{"id,assigned_object_type,assigned_object_id"}) {
					t.Errorf("fields = %v, want [id,assigned_object_type,assigned_object_id]", got)
				}
				if got := sortedInts(t, q["assigned_object_id"]); !reflect.DeepEqual(got, []int64{100, 101, 105, 106}) {
					t.Errorf("assigned_object_id = %v, want [100 101 105 106]", got)
				}
			})
		}
	}
	t.Run("VM", func(t *testing.T) {
		s := newScopedChangeServer()
		s.vms = []string{briefRow("virtualization/virtual-machines", 40, "name", "vm40", "vm40")}
		s.vmInterfaces = []childRow{{id: 500, owner: 40}}
		s.macs = []macRow{{id: 406, ct: "virtualization.vminterface", iface: 500}, {id: 407, ct: "dcim.interface", iface: 500}}
		spec := idScope("40")
		spec.ScopeType = "virtualization.virtualmachine"
		p, scope, kind, ids := membersSetup(t, s, spec)
		m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, macs: true})
		wantSet(t, "macs", m.macs, map[int64]int64{406: 40})
	})
}

// manyInterfaces is leaf1 (17) with 900 interfaces, 1000 to 1899.
func manyInterfaces() *scopedChangeServer {
	s := newScopedChangeServer()
	for id := int64(1000); id < 1900; id++ {
		s.interfaces = append(s.interfaces, childRow{id: id, owner: 17})
	}
	return s
}

// everyHundredth is interfaces 1000, 1100, ..., 1800 of manyInterfaces(): one
// in every batch of its ids.
func everyHundredth() []int64 {
	var out []int64
	for id := int64(1000); id < 1900; id += 100 {
		out = append(out, id)
	}
	return out
}

// A scope with many interfaces lists its MACs in batches within the URL budget,
// which together name every interface once.
func TestDescendantMembers_MACListBatches(t *testing.T) {
	s := manyInterfaces()
	want := map[int64]int64{}
	for i, iface := range everyHundredth() {
		mac := int64(3000 + i)
		s.macs = append(s.macs, macRow{id: mac, ct: "dcim.interface", iface: iface})
		want[mac] = 17
	}
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	m := membersWith(t, p, spec, scope, kind, ids, descendantWants{ifaces: true, macs: true})
	wantSet(t, "macs", m.macs, want)

	raws := s.rawQueries("dcim/mac-addresses")
	if len(raws) < 2 {
		t.Errorf("MAC lists = %d, want more than one", len(raws))
	}
	var sent []string
	for _, raw := range raws {
		if len(raw) > chunkBudgetBytes {
			t.Errorf("MAC list query is %d bytes, over the %d budget", len(raw), chunkBudgetBytes)
		}
		q, _ := url.ParseQuery(raw)
		sent = append(sent, q["assigned_object_id"]...)
	}
	var all []int64
	for id := int64(1000); id < 1900; id++ {
		all = append(all, id)
	}
	if got := sortedInts(t, sent); !reflect.DeepEqual(got, all) {
		t.Errorf("assigned_object_id across the lists = %d values, want each of the 900 interfaces once", len(got))
	}
}

// A MAC batch that fails stops the other reads as it fails, its held sibling
// batch included, rather than once every MAC batch has finished: the track
// ends at once, with the 500 on its marker, not descendants-slow. The 500 is
// answered only once the held batch's request has arrived, so the sibling is
// in flight when it fails, whichever batch the scheduler sent first.
func TestChanges_MACBatchFailureStopsTheOthers(t *testing.T) {
	s := manyInterfaces()
	s.macStatusFor = map[int64]int{1000: http.StatusInternalServerError}
	s.macHoldFor = map[int64]time.Duration{1899: 5 * time.Second}
	s.macHeld = make(chan struct{})
	p := s.start(t)
	p.descendantsWaitOverride = 3 * time.Second
	spec := withDescendants(nameScope("leaf1"))
	spec.ObjectTypes = []string{"dcim.macaddress"}
	changes, elapsed := changesWithin(t, p, spec, 5*time.Second)
	if elapsed > time.Second {
		t.Errorf("Changes took %v, want the held MAC batch stopped when its sibling failed", elapsed)
	}
	c := descendantsError(t, changes)
	if u := provider.Classify(c.Err); u != nil && u.AnnotationReason == provider.AnnotationReasonDescendantsSlow {
		t.Errorf("Err = %v, want the MAC batch's 500", c.Err)
	}
	var apiErr *APIError
	if !errors.As(c.Err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Err = %v, want the MAC batch's 500", c.Err)
	}
	held := slices.ContainsFunc(s.requestsTo("dcim/mac-addresses"), func(q url.Values) bool {
		return slices.Contains(q["assigned_object_id"], "1899")
	})
	if !held {
		t.Error("the held MAC batch was never sent")
	}
}

// Among MAC batches, an error that isn't a refusal outranks a refusal, though
// the refused batch comes first: the list fails with the 500, and only the
// 500 calls fail.
func TestDescendantMembers_MACBatchErrorOutranksRefusal(t *testing.T) {
	s := manyInterfaces()
	s.macStatusFor = map[int64]int{1000: http.StatusForbidden, 1899: http.StatusInternalServerError}
	spec := nameScope("leaf1")
	p, scope, kind, ids := membersSetup(t, s, spec)
	var fails failLog
	_, err := readMembersWith(p, spec, scope, kind, ids, descendantWants{ifaces: true, macs: true}, &fails)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("err = %v, want the 500", err)
	}
	if n := fails.count(); n != 1 || !errors.As(fails.errs[0], &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("fail called with %v, want once, with the 500", fails.errs)
	}
}

// Every membership and by-id request, a list's or a change-log read's, takes a
// slot of one semaphore, so no more than cableEndWorkers are answered at once.
// Two MACs on every interface make more by-id batches than slots. The IP
// creates come back only through the by-id read, since newer creates on an
// interface that isn't leaf1's fill the address scan's page, so every
// IP-history batch's IPs, one or more in each, must reach it.
func TestDescendantChanges_MembershipTakesFourSlots(t *testing.T) {
	s := manyInterfaces()
	for id := int64(1000); id < 1900; id++ {
		s.macs = append(s.macs,
			macRow{id: 10 * id, ct: "dcim.interface", iface: id},
			macRow{id: 10*id + 1, ct: "dcim.interface", iface: id})
	}
	for i := range 11 {
		s.changes = append(s.changes, ipOn(100+i, 0, "create", int64(5000+i), 9999))
	}
	var creates []int
	for i, iface := range everyHundredth() {
		s.changes = append(s.changes, ipOn(10+i, 1+i, "create", int64(2000+i), iface))
		creates = append(creates, 10+i)
	}
	s.changeDelay, s.listDelay = 20*time.Millisecond, 20*time.Millisecond
	p := s.start(t)
	p.changeScanCapOverride, p.residualRowsOverride = 10, 10
	results := descendantsOf(t, p, withDescendants(nameScope("leaf1")))
	s.mu.Lock()
	most := s.maxSlotInFlight
	s.mu.Unlock()
	if most < 2 || most > cableEndWorkers {
		t.Errorf("at most %d slot-taking requests answered at once, want 2 to %d", most, cableEndWorkers)
	}
	history := slices.DeleteFunc(readsCarrying(s, 20), func(q url.Values) bool { return !q.Has(relatedObjectIDParam) })
	if len(history) < 2 {
		t.Errorf("IP-history reads = %d, want more than one batch", len(history))
	}
	byID := byIDRequests(s)
	if n := len(byID); n <= cableEndWorkers {
		t.Errorf("by-id reads = %d, want more batches than the %d slots", n, cableEndWorkers)
	}
	// Every batch asks for one member type, and both are asked for: the IPs'
	// and the MACs'.
	types := map[string]bool{}
	for _, q := range byID {
		got := q[changedObjectTypeIDParam]
		if len(got) != 1 {
			t.Errorf("by-id changed_object_type_id = %v, want one type in every batch", got)
			continue
		}
		types[got[0]] = true
	}
	if got := slices.Sorted(maps.Keys(types)); !reflect.DeepEqual(got, []string{"20", "21"}) {
		t.Errorf("by-id reads ask for types %v, want [20 21]", got)
	}
	seen := map[int]bool{}
	for _, r := range results {
		for _, d := range r.rows {
			seen[d.oc.ID] = true
		}
	}
	if got := slices.Sorted(maps.Keys(seen)); !reflect.DeepEqual(got, creates) {
		t.Errorf("change ids = %v, want the IP creates %v", got, creates)
	}
}

// byIDReadsOf runs byIDReads on p over sets' batches (byIDBatches), as scans,
// with no filter, a limit of 50 and a fresh semaphore of cableEndWorkers
// slots. A read that calls fail fails the test.
func byIDReadsOf(t *testing.T, p *Provider, spec provider.ChangeSpec, ids map[string]int, sets map[string]map[int64]int64, match func(objectChange) (int64, bool)) []changeReadResult {
	t.Helper()
	var fails failLog
	batches := byIDBatches(spec, changeFilter{}, ids, sets, match)
	results := p.byIDReads(context.Background(), fails.fail, true, make(chan struct{}, cableEndWorkers), changeFilter{}, 50, batches)
	if n := fails.count(); n != 0 {
		t.Fatalf("byIDReads called fail %d times (%v), want none", n, fails.errs)
	}
	return results
}

// onLeaf1 matches every row, to leaf1.
func onLeaf1(objectChange) (int64, bool) { return 17, true }

// onLeaf1Interface matches an address row on leaf1's interface 100.
func onLeaf1Interface(oc objectChange) (int64, bool) {
	return matchAssigned(oc, 14, map[int64]int64{100: 17})
}

// A by-id read asks for one member type's changes over the query's range,
// newest first: that type's id alone, and its members' ids. The types are read
// in ascending id order. It reads by what a change changed, so it sends no
// related filter.
func TestByIDReads_Query(t *testing.T) {
	s := newScopedChangeServer()
	spec := rangeSpec()
	p, _, _, ids := membersSetup(t, s, spec)
	results := byIDReadsOf(t, p, spec, ids, map[string]map[int64]int64{
		ipAddressType: {401: 17},
		cableType:     {300: 17},
	}, onLeaf1)
	if len(results) != 2 {
		t.Errorf("results = %d, want one per type", len(results))
	}

	reads := s.changeLog()
	if len(reads) != 2 {
		t.Fatalf("by-id reads = %v, want one per type", reads)
	}
	slices.SortFunc(reads, func(a, b url.Values) int {
		return cmp.Compare(a.Get(changedObjectTypeIDParam), b.Get(changedObjectTypeIDParam))
	})
	for i, want := range []struct{ typeID, id string }{{"17", "300"}, {"20", "401"}} {
		q := reads[i]
		if got := q[changedObjectTypeIDParam]; !reflect.DeepEqual(got, []string{want.typeID}) {
			t.Errorf("changed_object_type_id = %v, want [%s]", got, want.typeID)
		}
		if got := q[changedObjectIDParam]; !reflect.DeepEqual(got, []string{want.id}) {
			t.Errorf("type %s: changed_object_id = %v, want [%s]", want.typeID, got, want.id)
		}
		if got := q["ordering"]; !reflect.DeepEqual(got, []string{"-time"}) {
			t.Errorf("ordering = %v, want [-time]", got)
		}
		if got, want := q.Get("time_after"), spec.From.UTC().Format(time.RFC3339); got != want {
			t.Errorf("time_after = %q, want From %q", got, want)
		}
		if got, want := q.Get("time_before"), spec.To.UTC().Format(time.RFC3339); got != want {
			t.Errorf("time_before = %q, want To %q", got, want)
		}
		if q.Has(relatedObjectTypeParam) || q.Has(relatedObjectIDParam) {
			t.Errorf("by-id read %v: want no related_object_type and no related_object_id", q)
		}
	}
}

// A row is kept only when its type and id together are a member's, and then
// only when it matches the scope: a module that shares an IP member's id isn't
// kept, nor is the IP member's edit on leaf2's interface. The same rows are
// kept whether NetBox applies the type and id filters or not.
func TestByIDReads_KeepOnlyMembersThatMatch(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignoreChangedQ %v", ignore), func(t *testing.T) {
			s := newestFirst(
				ipOn(1, 1, "create", 401, 100),                     // member, on a scope interface: kept
				created(2, 2, "dcim.module", 401, `{"device":17}`), // shares an IP member's id: dropped
				created(3, 3, "dcim.module", 500, `{"device":17}`), // module member: kept
				ipOn(4, 4, "create", 402, 100),                     // not a member: dropped
				ipMoved(5, 5, 401, 200, 200),                       // member, edited on leaf2's interface: dropped
			)
			s.ignoreChangedQ = ignore
			spec := nameScope("leaf1")
			p, _, _, ids := membersSetup(t, s, spec)
			match := func(oc objectChange) (int64, bool) {
				if normalizeObjectType(oc.changedType()) == moduleType {
					return matchOwner(oc, "device", map[int64]bool{17: true})
				}
				return onLeaf1Interface(oc)
			}
			results := byIDReadsOf(t, p, spec, ids, map[string]map[int64]int64{
				ipAddressType: {401: 17},
				moduleType:    {500: 17},
			}, match)
			var kept []int
			for _, r := range results {
				for _, d := range r.rows {
					kept = append(kept, d.oc.ID)
					if !d.descendant || d.owner != 17 {
						t.Errorf("#%d: descendant %v, owner %d; want true, 17", d.oc.ID, d.descendant, d.owner)
					}
				}
			}
			slices.Sort(kept)
			if !reflect.DeepEqual(kept, []int{1, 3}) {
				t.Errorf("kept change ids = %v, want [1 3]", kept)
			}
		})
	}
}

// Many members are read in batches within the URL budget. Each batch asks for
// their one type, and the batches together name every member once.
func TestByIDReads_Batches(t *testing.T) {
	s := newScopedChangeServer()
	spec := rangeSpec()
	p, _, _, ids := membersSetup(t, s, spec)
	ips := map[int64]int64{}
	var all []int64
	for id := int64(1000); id < 1900; id++ {
		ips[id] = 17
		all = append(all, id)
	}
	byIDReadsOf(t, p, spec, ids, map[string]map[int64]int64{ipAddressType: ips}, onLeaf1)

	raws := s.rawQueries(changeLogEndpoint)
	if len(raws) < 2 {
		t.Errorf("by-id reads = %d, want more than one", len(raws))
	}
	var sent []string
	for _, raw := range raws {
		if len(raw) > chunkBudgetBytes {
			t.Errorf("by-id read query is %d bytes, over the %d budget", len(raw), chunkBudgetBytes)
		}
		q, _ := url.ParseQuery(raw)
		if got := q[changedObjectTypeIDParam]; !reflect.DeepEqual(got, []string{"20"}) {
			t.Errorf("changed_object_type_id = %v, want [20] in every batch", got)
		}
		sent = append(sent, q[changedObjectIDParam]...)
	}
	if got := sortedInts(t, sent); !reflect.DeepEqual(got, all) {
		t.Errorf("changed_object_id across the reads = %d values, want each of the 900 members once", len(got))
	}
}

// A by-id read that stops at the scan cap may have missed rows past it: it is
// capped, and counts for the cut-off as a scan does.
func TestByIDReads_CapCountsForTheCutoff(t *testing.T) {
	s := newestFirst(
		ipOn(1, 1, "create", 401, 100),
		ipOn(2, 2, "create", 401, 100),
		ipOn(3, 3, "create", 401, 100),
	)
	spec := nameScope("leaf1")
	p, _, _, ids := membersSetup(t, s, spec)
	p.changeScanCapOverride = 2
	results := byIDReadsOf(t, p, spec, ids, map[string]map[int64]int64{ipAddressType: {401: 17}}, onLeaf1Interface)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one", len(results))
	}
	if r := results[0]; !r.capped || !r.scan || !r.descendant || len(r.rows) != 2 {
		t.Errorf("by-id read: capped %v, scan %v, descendant %v, %d rows; want capped, scan, descendant, 2 rows",
			r.capped, r.scan, r.descendant, len(r.rows))
	}
}

// Each by-id request asks for one type's members. Module 401, on leaf2, shares
// member IP 401's id and has more changes in range than the scan cap: read
// with the IP, its rows would fill the IP's read. Read apart, the IP's read
// and the module member's aren't capped, and the track has no cut-off.
func TestChanges_ByIDReadsAreOneTypeEach(t *testing.T) {
	rows := []loggedChange{
		ipOn(1, 10, "create", 401, 100),
		created(2, 11, "dcim.module", 500, `{"device":17}`),
	}
	for i := range 6 {
		rows = append(rows, ch(10+i, 1+i, "dcim.module", 401).snapshots(`{"device":18,"serial":"a"}`, `{"device":18,"serial":"b"}`))
	}
	s := newestFirst(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	s.modules = []childRow{{id: 500, owner: 17}}
	p := s.start(t)
	p.changeScanCapOverride = 3
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	wantIDs(t, changesOK(t, p, spec), "#1", "#2")
	if got, want := byIDAsks(t, s), map[string][]string{"20": {"401"}, "22": {"500"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("by-id reads ask for %v, want IP 401 under type 20 and module 500 under type 22", got)
	}
}

func TestSnapshotID(t *testing.T) {
	data := map[string]json.RawMessage{}
	for key, raw := range map[string]string{
		"int": `17`, "string": `"17"`, "object": `{"id":17}`, "null": `null`,
		"zero": `0`, "negative": `-1`, "fraction": `1.5`,
	} {
		data[key] = json.RawMessage(raw)
	}
	if id, ok := snapshotID(data, "int"); !ok || id != 17 {
		t.Errorf("snapshotID(17) = %d, %v; want 17, true", id, ok)
	}
	for _, key := range []string{"string", "object", "null", "zero", "negative", "fraction", "missing"} {
		if id, ok := snapshotID(data, key); ok {
			t.Errorf("snapshotID(%s) = %d, true; want false", key, id)
		}
	}
}

// withDescendants is spec with Include descendants on, read to a limit of 50
// unless spec sets one.
func withDescendants(spec provider.ChangeSpec) provider.ChangeSpec {
	spec.Descendants = true
	if spec.Limit == 0 {
		spec.Limit = 50
	}
	return spec
}

// descendantsOf runs descendantChanges for spec on p, as Changes does, with
// context.Background() and a limit of 50. An error, or a read that calls
// fail, fails the test.
func descendantsOf(t *testing.T, p *Provider, spec provider.ChangeSpec) []changeReadResult {
	t.Helper()
	scope := resolveOK(t, p, spec)
	f, err := newChangeFilter(spec)
	if err != nil {
		t.Fatalf("newChangeFilter: %v", err)
	}
	var fails failLog
	results, err := p.descendantChanges(context.Background(), fails.fail, spec, scope, normalizeObjectTypes(spec.ObjectTypes), f, 50)
	if err != nil {
		t.Fatalf("descendantChanges: %v", err)
	}
	if n := fails.count(); n != 0 {
		t.Fatalf("descendantChanges called fail %d times (%v), want none", n, fails.errs)
	}
	return results
}

// assignedTo is an address snapshot assigned to interface type typeID's id.
func assignedTo(typeID int, id int64) string {
	return fmt.Sprintf(`{"assigned_object_type":%d,"assigned_object_id":%d}`, typeID, id)
}

// unassigned is an address snapshot on no interface.
const unassigned = `{"assigned_object_type":null,"assigned_object_id":null}`

// created is change id: ct objID created minutes before scopedNow, with post as
// its after data.
func created(id, minutes int, ct string, objID int64, post string) loggedChange {
	return ch(id, minutes, ct, objID).as("create").snapshots("", post)
}

// newestFirst is a server holding rows sorted newest first, as the change log
// returns them.
func newestFirst(rows ...loggedChange) *scopedChangeServer {
	slices.SortStableFunc(rows, func(a, b loggedChange) int { return cmp.Compare(a.minutes, b.minutes) })
	return newScopedChangeServer(rows...)
}

// wantParents asserts each named change is on parent.
func wantParents(t *testing.T, changes []provider.Change, parent string, reprs ...string) {
	t.Helper()
	for _, repr := range reprs {
		if c := byRepr(t, changes, repr); c.Parent != parent {
			t.Errorf("%s Parent = %q, want %q", repr, c.Parent, parent)
		}
	}
}

// typeIDsOf is a change-log request's changed_object_type_id values, sorted.
func typeIDsOf(t *testing.T, q url.Values) []int64 {
	t.Helper()
	return sortedInts(t, q["changed_object_type_id"])
}

// carries reports whether a change-log request asks for any of the type ids.
func carries(q url.Values, typeIDs ...int) bool {
	return slices.ContainsFunc(typeIDs, func(id int) bool { return slices.Contains(q["changed_object_type_id"], strconv.Itoa(id)) })
}

// scanReads is the change-log requests that carry any of the type ids and are
// neither an own read nor a related read.
func scanReads(s *scopedChangeServer, typeIDs ...int) []url.Values {
	return slices.DeleteFunc(s.changeLog(), func(q url.Values) bool {
		return !carries(q, typeIDs...) || q.Has("changed_object_id") || q.Has("related_object_id")
	})
}

// byIDRequests is the change-log requests that carry changed_object_id but
// whose changed_object_type_id doesn't hold the scope type's id (12). The
// scope's own read always carries changed_object_id (changeReads), and
// perAction copies it once per Action.
func byIDRequests(s *scopedChangeServer) []url.Values {
	return slices.DeleteFunc(s.changeLog(), func(q url.Values) bool {
		return !q.Has(changedObjectIDParam) || carries(q, scopedTypeIDs["dcim.device"])
	})
}

// byIDAsks is what the by-id reads asked for: the changed_object_id values by
// changed_object_type_id. A read with other than one type id fails the test.
func byIDAsks(t *testing.T, s *scopedChangeServer) map[string][]string {
	t.Helper()
	got := map[string][]string{}
	for _, q := range byIDRequests(s) {
		types := q[changedObjectTypeIDParam]
		if len(types) != 1 {
			t.Errorf("by-id read %v: want one changed_object_type_id", q)
			continue
		}
		got[types[0]] = append(got[types[0]], q[changedObjectIDParam]...)
	}
	return got
}

// descendantsError is the track's last change, which must be its one
// descendants-error marker.
func descendantsError(t *testing.T, changes []provider.Change) provider.Change {
	t.Helper()
	n := 0
	for _, c := range changes {
		if c.Kind == provider.ChangeKindDescendantsError {
			n++
		}
	}
	if n != 1 || len(changes) == 0 || changes[len(changes)-1].Kind != provider.ChangeKindDescendantsError {
		t.Fatalf("changes = %v, want one descendants-error marker, last", changeIDs(changes))
	}
	c := changes[len(changes)-1]
	if c.Err == nil {
		t.Fatal("descendants-error marker has no Err")
	}
	return c
}

// sameRows asserts two tracks read from different fixture servers hold the
// same rows. Each row's link names its server, so links are left out.
func sameRows(t *testing.T, got, want []provider.Change) {
	t.Helper()
	unlinked := func(changes []provider.Change) []provider.Change {
		out := slices.Clone(changes)
		for i := range out {
			out[i].URL = ""
		}
		return out
	}
	if !reflect.DeepEqual(unlinked(got), unlinked(want)) {
		t.Errorf("changes = %+v, want %+v as with Include descendants off", got, want)
	}
}

// eachKindServer is leaf1 (17) with interfaces 100 (cable 300) and 101, leaf2
// (18) with interface 200, and a change log holding one change of each kind
// Include descendants shows on leaf1 (#1-#8), then the ones it must not (#9-#15).
func eachKindServer() *scopedChangeServer {
	s := newestFirst(
		ch(1, 1, "dcim.cable", 300),
		ch(2, 2, "dcim.cable", 301).as("delete"),
		created(3, 3, "ipam.ipaddress", 401, assignedTo(14, 100)),
		ch(4, 4, "ipam.ipaddress", 402).snapshots(assignedTo(14, 100), `{"assigned_object_type":null,"assigned_object_id":null}`),
		created(5, 5, "dcim.macaddress", 403, assignedTo(14, 101)),
		created(6, 6, "dcim.module", 500, `{"device":17}`),
		created(7, 7, "ipam.service", 600, `{"parent_object_type":12,"parent_object_id":17}`),
		created(8, 8, "ipam.service", 601, `{"device":17}`),

		// A VM interface sharing interface 100's id.
		created(9, 9, "ipam.ipaddress", 404, assignedTo(31, 100)),
		// On leaf2's interface, and in leaf2.
		created(10, 10, "ipam.ipaddress", 405, assignedTo(14, 200)),
		created(11, 11, "dcim.module", 501, `{"device":18}`),
		// A cable on no leaf1 interface.
		ch(12, 12, "dcim.cable", 999),
		// A service on VM 17, not device 17.
		created(13, 13, "ipam.service", 602, `{"parent_object_type":30,"parent_object_id":17}`),
		// An interface id that isn't a plain integer.
		created(14, 14, "ipam.ipaddress", 406, `{"assigned_object_type":14,"assigned_object_id":"100"}`),
		// Cable 301's end on interface 100, deleted with it: only this names it.
		cableEndChange(15, 15, "delete", 100, 301, 14, 100),
	)
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}, {id: 101, owner: 17}, {id: 200, owner: 18}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}, {"dcim.interface", 200, 18}}}}
	return s
}

func TestChanges_DescendantsDeviceShowsEachKind(t *testing.T) {
	changes := changesOK(t, eachKindServer().start(t), withDescendants(nameScope("leaf1")))
	shown := []string{"#1", "#2", "#3", "#4", "#5", "#6", "#7", "#8"}
	wantIDs(t, changes, shown...)
	wantParents(t, changes, "leaf1", shown...)
}

// An IP moved from leaf1's interface to leaf2's is on leaf1 by its before data
// and on leaf2 by its after data.
func TestChanges_DescendantIPMovedShowsOnBothTracks(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "ipam.ipaddress", 401).snapshots(assignedTo(14, 100), assignedTo(14, 200)))
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	p := s.start(t)
	for _, device := range []string{"leaf1", "leaf2"} {
		changes := changesOK(t, p, withDescendants(nameScope(device)))
		wantIDs(t, changes, "#1")
		wantParents(t, changes, device, "#1")
	}
}

// An IP moved between two scope objects is returned by both the by-id read and
// the address scan, and each change shows once, on the object it matched.
func TestChanges_DescendantIPMovedBetweenScopeObjectsShowsOnce(t *testing.T) {
	s := newestFirst(ipOn(1, 40, "create", 401, 100), ipMoved(2, 30, 401, 100, 200))
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	changes := changesOK(t, s.start(t), withDescendants(nameScope("leaf1", "leaf2")))
	wantIDs(t, changes, "#2", "#1")
	wantParents(t, changes, "leaf2", "#2")
	wantParents(t, changes, "leaf1", "#1")
	if byID := byIDRequests(s); len(byID) != 1 {
		t.Errorf("by-id reads = %v, want one beside the address scan", byID)
	}
}

// The scans' pages end short of everything attached; the by-id reads find it:
// an IP's create and its move off, a MAC, a module, a service and a cable.
func TestChanges_DescendantsFoundByIDPastTheScanCap(t *testing.T) {
	svc := `{"parent_object_type":12,"parent_object_id":17}`
	rows := []loggedChange{
		ipOn(1, 40, "create", 401, 100),
		ipMoved(2, 30, 401, 100, 200), // moved off
		ch(3, 35, "dcim.macaddress", 403).snapshots(assignedTo(14, 100), assignedTo(14, 100)),
		ch(4, 25, "dcim.module", 500).snapshots(`{"device":17,"serial":"a"}`, `{"device":17,"serial":"b"}`),
		ch(5, 20, "ipam.service", 600).snapshots(svc, svc),
		ch(6, 15, "dcim.cable", 300),
	}
	// Leaf2's, newer than every target, and more than either scan's page.
	for i := range 11 {
		rows = append(rows,
			ipOn(100+i, 1, "create", int64(700+i), 200),
			created(200+i, 1, "dcim.module", int64(800+i), `{"device":18}`),
		)
	}
	s := newestFirst(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}, {id: 200, owner: 18}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}}}}
	s.macs = []macRow{{id: 403, ct: "dcim.interface", iface: 100}}
	s.modules = []childRow{{id: 500, owner: 17}}
	s.services = []serviceRow{{id: 600, parentType: "dcim.device", parent: 17}}
	p := s.start(t)
	p.changeScanCapOverride, p.residualRowsOverride = 10, 10
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	changes := changesOK(t, p, spec)
	wantIDs(t, changes, "#6", "#5", "#4", "#2", "#3", "#1", "cutoff")
	wantParents(t, changes, "leaf1", "#1", "#2", "#3", "#4", "#5", "#6")

	// One read per member type, each with its own members.
	want := map[string][]string{"17": {"300"}, "20": {"401"}, "21": {"403"}, "22": {"500"}, "23": {"600"}}
	if got := byIDAsks(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("by-id reads ask for %v, want %v", got, want)
	}
}

// cableByIDServer is leaf1 (17) with its own change (#1), and cable 300 on its
// interface 100 (#2). Without Object types, only the by-id read asks for
// cables (type 17).
func cableByIDServer() *scopedChangeServer {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17), ch(2, 2, "dcim.cable", 300))
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}}}}
	return s
}

// A by-id read that fails is descendantChanges' error, though the scans beside
// it succeeded, and calls fail with it.
func TestDescendantChanges_ByIDReadFailure(t *testing.T) {
	s := cableByIDServer()
	s.failTypeID = scopedTypeIDs["dcim.cable"]
	p := s.start(t)
	spec := withDescendants(nameScope("leaf1"))
	var fails failLog
	_, err := p.descendantChanges(context.Background(), fails.fail, spec, resolveOK(t, p, spec), nil, changeFilter{}, 50)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("err = %v, want the by-id read's 500", err)
	}
	if n := fails.count(); n != 1 || !errors.As(fails.errs[0], &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("fail called with %v, want once, with the 500", fails.errs)
	}
	if scans := scanReads(s, 20, 21, 22, 23); len(scans) != 2 {
		t.Errorf("scans = %v, want the address and parts scans", scans)
	}
}

// A by-id read that fails stops the other descendant reads as it fails, the
// held parts scan included: the track ends at once, with the 500 on its
// marker, not descendants-slow.
func TestChanges_ByIDReadFailureStopsTheOthers(t *testing.T) {
	s := cableByIDServer()
	s.failTypeID = scopedTypeIDs["dcim.cable"]
	s.delayTypeID, s.delayFor = scopedTypeIDs["dcim.module"], 5*time.Second
	p := s.start(t)
	p.descendantsWaitOverride = 3 * time.Second
	changes, elapsed := changesWithin(t, p, withDescendants(nameScope("leaf1")), 5*time.Second)
	if elapsed > time.Second {
		t.Errorf("Changes took %v, want the parts scan stopped when the by-id read failed", elapsed)
	}
	wantIDs(t, changes, "#1", "descendants-error")
	var apiErr *APIError
	if c := descendantsError(t, changes); !errors.As(c.Err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Err = %v, want the by-id read's 500", c.Err)
	}
}

// With several scope objects, a descendant is on the one it matched.
func TestChanges_DescendantsMultiScopeParent(t *testing.T) {
	s := newestFirst(
		created(1, 1, "dcim.module", 500, `{"device":18}`),
		ch(2, 2, "dcim.cable", 300),
		created(3, 3, "ipam.ipaddress", 401, assignedTo(14, 101)),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 101, owner: 17}, {id: 200, owner: 18}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}, {"dcim.interface", 200, 18}}}}
	changes := changesOK(t, s.start(t), withDescendants(nameScope("leaf1", "leaf2")))
	wantIDs(t, changes, "#1", "#2", "#3")
	wantParents(t, changes, "leaf2", "#1")
	wantParents(t, changes, "leaf1", "#2", "#3")
}

// Changed fields applies to descendant updates as to any other; creates and
// deletes pass.
func TestChanges_DescendantsChangedFieldsApply(t *testing.T) {
	s := newestFirst(
		ch(1, 1, "dcim.cable", 300).changing("status", "connected", "planned"),
		ch(2, 2, "dcim.cable", 300),
		ch(3, 3, "dcim.cable", 300).as("delete"),
	)
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}}}}
	spec := withDescendants(nameScope("leaf1"))
	spec.ChangedFields = []string{"status"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#3")
}

func TestChanges_DescendantsVM(t *testing.T) {
	s := newestFirst(
		created(1, 1, "ipam.ipaddress", 401, assignedTo(31, 500)),
		created(2, 2, "dcim.macaddress", 402, assignedTo(31, 500)),
		created(3, 3, "ipam.service", 600, `{"parent_object_type":30,"parent_object_id":40}`),
		created(4, 4, "ipam.service", 601, `{"virtual_machine":40}`),
		// A device interface sharing VM interface 500's id.
		created(5, 5, "ipam.ipaddress", 403, assignedTo(14, 500)),
	)
	s.vms = []string{briefRow("virtualization/virtual-machines", 40, "name", "vm40", "vm40")}
	s.vmInterfaces = []childRow{{id: 500, owner: 40}}
	spec := withDescendants(idScope("40"))
	spec.ScopeType = "virtualization.virtualmachine"
	changes := changesOK(t, s.start(t), spec)
	shown := []string{"#1", "#2", "#3", "#4"}
	wantIDs(t, changes, shown...)
	wantParents(t, changes, "vm40", shown...)
}

// On an interface scope, an IP on it is also recorded against it: it shows
// once.
func TestChanges_DescendantsInterfaceScope(t *testing.T) {
	s := newestFirst(
		ch(1, 1, "dcim.cable", 300),
		created(2, 2, "dcim.macaddress", 402, assignedTo(14, 100)),
		created(3, 3, "ipam.ipaddress", 401, assignedTo(14, 100)).relatedTo("dcim.interface", 100),
	)
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}}
	spec := withDescendants(idScope("100"))
	spec.ScopeType = "dcim.interface"
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#2", "#3")
}

// A device with nothing attached reads nothing by id. Its residual scans start
// before that's known, the address scan too, but a scope without interfaces
// drops the address scan's result: no IP or MAC it could match exists, so its
// capped page makes no cut-off, and the track is today's. The parts scan reads
// modules and services: cables are only read by id.
func TestChanges_DescendantsNothingAttached(t *testing.T) {
	rows := []loggedChange{
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
		created(3, 3, "ipam.ipaddress", 401, assignedTo(14, 200)),
		created(4, 4, "ipam.ipaddress", 402, assignedTo(14, 200)),
	}
	s := newScopedChangeServer(rows...)
	p := s.start(t)
	p.residualRowsOverride = 1
	got := changesOK(t, p, withDescendants(nameScope("leaf1")))
	sameRows(t, got, changesOK(t, newScopedChangeServer(rows...).start(t), scoped(nameScope("leaf1"), 50)))

	if addr := scanReads(s, 20, 21); len(addr) != 1 {
		t.Errorf("address scans = %v, want one", addr)
	}
	parts := scanReads(s, 17, 22, 23)
	if len(parts) != 1 {
		t.Fatalf("parts scans = %v, want one", parts)
	}
	if got := typeIDsOf(t, parts[0]); !reflect.DeepEqual(got, []int64{22, 23}) {
		t.Errorf("parts scan changed_object_type_id = %v, want [22 23]", got)
	}
	if byID := byIDRequests(s); len(byID) != 0 {
		t.Errorf("by-id reads = %v, want none: there are no members", byID)
	}
}

func TestChanges_DescendantsOffMakesTodaysRequests(t *testing.T) {
	s := eachKindServer()
	changesOK(t, s.start(t), scoped(nameScope("leaf1"), 50))
	for _, path := range []string{"dcim/interfaces", "dcim/cables", "dcim/mac-addresses", "dcim/modules", "ipam/services", "virtualization/virtual-machines", "virtualization/interfaces"} {
		if lists := s.requestsTo(path); len(lists) != 0 {
			t.Errorf("%s requests = %v, want none", path, lists)
		}
	}
	if own, related, all := len(s.ownReads()), len(s.relatedReads()), len(s.changeLog()); own != 1 || related != 1 || all != 2 {
		t.Errorf("own reads %d, related reads %d, all %d; want 1, 1, 2", own, related, all)
	}
}

// Where Include descendants can't apply, it makes no request and reads no type
// map: with the map failing, there is no descendants-error marker. Each run
// gets a fresh server and provider, since the map's failure is cached.
func TestChanges_DescendantsNoopWhereTheyCantApply(t *testing.T) {
	site := idScope("6")
	site.ScopeType = "dcim.site"
	deviceOnly := idScope("17")
	deviceOnly.ObjectTypes = []string{"dcim.device"}
	for name, spec := range map[string]provider.ChangeSpec{
		"unscoped":                    {},
		"site scope":                  site,
		"device scope, own type only": deviceOnly,
	} {
		t.Run(name, func(t *testing.T) {
			run := func(descendants bool) ([]provider.Change, []string) {
				s := newScopedChangeServer(ch(1, 1, "dcim.device", 17), ch(2, 2, "dcim.site", 6))
				s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}}
				s.coreStatus = http.StatusForbidden
				spec := spec
				spec.Limit, spec.Descendants = 10, descendants
				changes := changesOK(t, s.start(t), spec)
				s.mu.Lock()
				defer s.mu.Unlock()
				return changes, slices.Sorted(slices.Values(s.requests))
			}
			on, onRequests := run(true)
			off, offRequests := run(false)
			if slices.Contains(changeIDs(on), string(provider.ChangeKindDescendantsError)) {
				t.Errorf("changes = %v, want no descendants-error marker", changeIDs(on))
			}
			sameRows(t, on, off)
			if !reflect.DeepEqual(onRequests, offRequests) {
				t.Errorf("requests = %v, want %v as with Include descendants off", onRequests, offRequests)
			}
		})
	}
}

// Object types decides which reads run, and which children are read for them:
// a device's cables need its interfaces, which its cable ends are read
// through, and are read by id, with no scan. The interface deletes that build
// them don't reach the track.
func TestChanges_DescendantsObjectTypesNarrowTheReads(t *testing.T) {
	s := newestFirst(
		ch(1, 1, "dcim.cable", 300),
		ifaceDelete(2, 2, 105, 17),
	)
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}}}}
	spec := withDescendants(nameScope("leaf1"))
	spec.ObjectTypes = []string{"dcim.cable"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1")

	if lists := s.requestsTo("dcim/interfaces"); len(lists) != 1 {
		t.Errorf("interface lists = %v, want one", lists)
	}
	deletes := slices.DeleteFunc(s.changeLog(), func(q url.Values) bool { return !carries(q, 14) || q.Get("action") != "delete" })
	if len(deletes) != 1 {
		t.Errorf("interface-delete reads = %v, want one", deletes)
	}
	for _, q := range s.changeLog() {
		if carries(q, 20, 21) {
			t.Errorf("change-log read %v: want no address scan", q)
		}
	}
	if parts := scanReads(s, 17, 22, 23); len(parts) != 0 {
		t.Errorf("parts scans = %v, want none: cables are read by id", parts)
	}
	byID := byIDRequests(s)
	if len(byID) != 1 || !reflect.DeepEqual(byID[0][changedObjectTypeIDParam], []string{"17"}) ||
		!reflect.DeepEqual(byID[0][changedObjectIDParam], []string{"300"}) {
		t.Errorf("by-id reads = %v, want one with changed_object_type_id=17 and changed_object_id=300", byID)
	}
}

// A NetBox that ignores the type and id filters can't widen the address scan or
// the by-id read: each keeps only the types it asked for and, by id, its
// members, though the module would match leaf1.
func TestChanges_DescendantScansCheckTheirTypes(t *testing.T) {
	s := newestFirst(
		ipOn(1, 1, "create", 401, 100),
		created(2, 2, "dcim.module", 500, `{"device":17}`),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	s.ignoreChangedQ = true
	spec := withDescendants(nameScope("leaf1"))
	spec.ObjectTypes = []string{"ipam.ipaddress"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1")
	// The IP history found member 401, so a by-id read ran, and NetBox gave it
	// both rows.
	if byID := byIDRequests(s); len(byID) != 1 {
		t.Errorf("by-id reads = %v, want one", byID)
	}
}

// One Action is pushed to the scans and the by-id read. The membership reads
// don't take the query's filters: only the interface-delete read asks for an
// action, its own.
func TestChanges_DescendantsOneActionPushed(t *testing.T) {
	s := newestFirst(
		ch(1, 1, "dcim.cable", 300).as("delete"),
		created(2, 2, "ipam.ipaddress", 401, assignedTo(14, 100)),
	)
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}}}}
	spec := withDescendants(nameScope("leaf1"))
	spec.Actions = []string{"delete"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1")

	all := scanReads(s, 20, 21, 17, 22, 23)
	if len(all) != 2 {
		t.Fatalf("scans = %v, want the address and parts scans", all)
	}
	for _, q := range all {
		if !reflect.DeepEqual(q["action"], []string{"delete"}) {
			t.Errorf("scan %v: want action=delete", q)
		}
	}
	if byID := byIDRequests(s); len(byID) != 1 || !reflect.DeepEqual(byID[0]["action"], []string{"delete"}) {
		t.Errorf("by-id reads = %v, want one with action=delete", byID)
	}
	membership := 0
	for _, q := range s.changeLog() {
		switch {
		case carries(q, 14):
			membership++
			if !reflect.DeepEqual(q["action"], []string{"delete"}) {
				t.Errorf("interface-delete read %v: want action=delete", q)
			}
		case carries(q, 24):
			membership++
			if q.Has("action") {
				t.Errorf("cable-end read %v: want no action", q)
			}
		}
	}
	if membership != 2 {
		t.Errorf("membership reads = %d, want the interface-delete and cable-end reads", membership)
	}
}

// The scans and the by-id read aren't read once per Action. With two Actions
// they ask for none, and keep only the rows of those two.
func TestChanges_TwoActionsDescendantScansFilterLocally(t *testing.T) {
	onIface := func(desc string) string {
		return fmt.Sprintf(`{"assigned_object_type":14,"assigned_object_id":100,"description":%q}`, desc)
	}
	s := newestFirst(
		created(1, 1, "ipam.ipaddress", 401, assignedTo(14, 100)),
		ch(2, 2, "ipam.ipaddress", 401).snapshots(onIface("a"), onIface("b")),
		ch(3, 3, "ipam.ipaddress", 402).as("delete").snapshots(assignedTo(14, 100), ""),
		created(4, 4, "dcim.module", 500, `{"device":17}`),
		ch(5, 5, "dcim.module", 500).snapshots(`{"device":17,"serial":"a"}`, `{"device":17,"serial":"b"}`),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	s.modules = []childRow{{id: 500, owner: 17}}
	spec := withDescendants(nameScope("leaf1"))
	spec.Actions = []string{"create", "delete"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#3", "#4")

	for name, ids := range map[string][]int{"address": {20, 21}, "parts": {17, 22, 23}} {
		scans := scanReads(s, ids...)
		if len(scans) != 1 || scans[0].Has("action") {
			t.Errorf("%s scans = %v, want one with no action", name, scans)
		}
	}
	byID := byIDRequests(s)
	if len(byID) == 0 {
		t.Error("no by-id read, want module 500's")
	}
	for _, q := range byID {
		if q.Has("action") {
			t.Errorf("by-id read %v: want no action", q)
		}
	}
}

// A scan whose page ends with rows left ends the track with a cut-off that says
// only what's attached was cut.
func TestChanges_DescendantScanCapShowsCutoff(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 5; i++ {
		rows = append(rows, created(i, i, "ipam.ipaddress", int64(400+i), assignedTo(14, 200)))
	}
	rows = append(rows, ch(10, 10, "dcim.device", 17))
	s := newScopedChangeServer(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	p := s.start(t)
	p.residualRowsOverride = 3
	changes := changesOK(t, p, withDescendants(nameScope("leaf1")))
	wantIDs(t, changes, "#10", "cutoff")
	if cut := changes[len(changes)-1]; !cut.Residual || !slices.Equal(cut.Types, []string{"ipam.ipaddress", "dcim.macaddress"}) {
		t.Errorf("cut-off Residual %v Types %v, want a residual cut-off of the address scan's types", cut.Residual, cut.Types)
	} else if want := scopedNow.Add(-3 * time.Minute); !nearNow(cut.Time) || !cut.Until.Equal(want) {
		// With no To the marker sits at now, the end of the range; the reach stays in Until.
		t.Errorf("cut-off at %v until %v, want at about now, until the reach %v", cut.Time, cut.Until, want)
	}
}

func TestChanges_CutoffNotDescendantsOnlyWhenOwnReadCut(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17), ch(2, 2, "dcim.device", 17), ch(3, 3, "dcim.device", 17))
	p := s.start(t)
	p.changeScanCapOverride = 2
	spec := withDescendants(nameScope("leaf1"))
	spec.ChangedFields = []string{"status"}
	changes := changesOK(t, p, spec)
	wantIDs(t, changes, "cutoff")
	if cut := changes[len(changes)-1]; cut.DescendantsOnly {
		t.Errorf("cut-off DescendantsOnly = true, want false: the own read was cut")
	}
}

// A membership read capped after To may have missed a child with changes
// anywhere in the range: the cut-off is To.
func TestChanges_DescendantMembershipCapShowsCutoffAtTo(t *testing.T) {
	s := newScopedChangeServer(
		ifaceDelete(1, 1, 105, 17),
		ifaceDelete(2, 2, 106, 17),
		ifaceDelete(3, 3, 107, 17),
		ch(4, 20, "dcim.device", 17),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	p := s.start(t)
	p.changeScanCapOverride = 2
	spec := withDescendants(rangeSpec())
	changes := changesOK(t, p, spec)
	wantIDs(t, changes, "#4", "cutoff")
	if cut := changes[len(changes)-1]; !cut.Time.Equal(spec.To) || !cut.DescendantsOnly {
		t.Errorf("cut-off at %v, DescendantsOnly %v; want at To %v, true", cut.Time, cut.DescendantsOnly, spec.To)
	}
}

// leaf1 with interface 100, its own change (#1), a change recorded against it
// (#2), and an IP (#3) and a module (#4) attached to it.
func trackServer() *scopedChangeServer {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 100).relatedTo("dcim.device", 17),
		created(3, 3, "ipam.ipaddress", 401, assignedTo(14, 100)),
		created(4, 4, "dcim.module", 500, `{"device":17}`),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	return s
}

// A failed descendant read drops every descendant row, the module list's that
// succeeded included, and keeps the rest of the track, with a marker. The
// IP-history read fails, and the residual scans, which started beside
// membership, are dropped with the rest.
func TestChanges_DescendantReadFailureKeepsTheTrack(t *testing.T) {
	s := trackServer()
	s.failTypeID = 20
	spec := withDescendants(nameScope("leaf1"))
	spec.To = scopedNow
	changes := changesOK(t, s.start(t), spec)
	wantIDs(t, changes, "#1", "#2", "descendants-error")
	if c := descendantsError(t, changes); !c.Time.Equal(spec.To) {
		t.Errorf("descendants-error at %v, want at To %v", c.Time, spec.To)
	}
	if lists := s.requestsTo("dcim/modules"); len(lists) != 1 {
		t.Errorf("module lists = %v, want one: its members are dropped, not unread", lists)
	}
}

func TestChanges_DescendantTypeMapUnreadableKeepsTheTrack(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	s.coreStatus = http.StatusForbidden
	before := time.Now()
	changes := changesOK(t, s.start(t), withDescendants(idScope("17")))
	wantIDs(t, changes, "#1", "descendants-error")
	c := descendantsError(t, changes)
	if c.Time.Before(before) || c.Time.After(time.Now()) {
		t.Errorf("descendants-error at %v, want now: the query has no To", c.Time)
	}
	if u := provider.Classify(c.Err); u == nil || u.AnnotationReason != provider.AnnotationReasonDescendantTypesUnreadable {
		t.Errorf("Err = %v, want %s", c.Err, provider.AnnotationReasonDescendantTypesUnreadable)
	}
}

// A descendant read held past the descendants deadline doesn't hold the track:
// it shows, with a descendants-slow marker.
func TestChanges_DescendantsSlowKeepsTheTrack(t *testing.T) {
	s := trackServer()
	s.delayTypeID, s.delayFor = 20, 5*time.Second
	p := s.start(t)
	p.descendantsWaitOverride = 100 * time.Millisecond
	changes, _ := changesWithin(t, p, withDescendants(nameScope("leaf1")), 2*time.Second)
	wantIDs(t, changes, "#1", "#2", "descendants-error")
	c := descendantsError(t, changes)
	if u := provider.Classify(c.Err); u == nil || u.AnnotationReason != provider.AnnotationReasonDescendantsSlow || u.AnnotationCount < 1 {
		t.Errorf("Err = %v, want %s with a count of seconds", c.Err, provider.AnnotationReasonDescendantsSlow)
	}
}

// A descendant read waiting out a retry's backoff when the descendants deadline
// passes was cut by that deadline as much as a held one: descendants-slow, not
// the 503 it was about to retry.
func TestChanges_DescendantsSlowDuringRetryBackoff(t *testing.T) {
	saved := retryBackoff
	retryBackoff = []time.Duration{2 * time.Second}
	t.Cleanup(func() { retryBackoff = saved })

	s := trackServer()
	s.unavailableTypeID = 20
	p := s.start(t)
	p.descendantsWaitOverride = 100 * time.Millisecond
	changes, _ := changesWithin(t, p, withDescendants(nameScope("leaf1")), time.Second)
	wantIDs(t, changes, "#1", "#2", "descendants-error")
	c := descendantsError(t, changes)
	if u := provider.Classify(c.Err); u == nil || u.AnnotationReason != provider.AnnotationReasonDescendantsSlow {
		t.Errorf("Err = %v, want %s: the deadline cut the IP-history read's retry", c.Err, provider.AnnotationReasonDescendantsSlow)
	}
}

// The caller's context ending is the query ending: an error, not a marker. It
// is cancelled once a read of IP changes is held: most often the address scan,
// which starts beside the own and related reads, else the IP-history read.
// Every read of IP changes is held, the IP-history read included, so the
// membership reads can't finish before the cancel, and the descendant reads
// end with it, whether or not the own and related reads had finished; if they
// hadn't, the query fails through them too, as it should.
func TestChanges_CallerCancelledFails(t *testing.T) {
	s := trackServer()
	s.delayTypeID, s.delayFor = 20, 5*time.Second
	p := s.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for end := time.Now().Add(2 * time.Second); len(readsCarrying(s, 20)) == 0 && time.Now().Before(end); {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	type result struct {
		changes []provider.Change
		err     error
	}
	done := make(chan result, 1)
	go func() {
		changes, err := p.Changes(ctx, withDescendants(nameScope("leaf1")))
		done <- result{changes, err}
	}()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("Changes = %v, %v; want the cancellation", changeIDs(got.changes), got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Changes still blocked 3s after its context was cancelled")
	}
	if held := readsCarrying(s, 20); len(held) == 0 {
		t.Error("no read of IP changes, want the one the cancel cut")
	}
}

// The caller's context ending while only the residual scans are left still
// ends the query: an error, not a track whose scans read as unchecked. The
// scans are held; the caller cancels once the by-id reads, the IP's and the
// module's, have been answered and the two held scans are the only change-log
// requests left, well before the residual deadline.
func TestChanges_CallerCancelledDuringResidualScansFails(t *testing.T) {
	s := newestFirst(
		ch(1, 1, "dcim.device", 17),
		ipOn(2, 2, "create", 401, 100),
		ch(3, 3, "dcim.module", 500).snapshots(`{"device":17,"serial":"a"}`, `{"device":17,"serial":"b"}`),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	s.modules = []childRow{{id: 500, owner: 17}}
	s.scanHold = 5 * time.Second
	p := s.start(t)
	p.descendantsWaitOverride = 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onlyScansLeft := func() bool {
		s.mu.Lock()
		inFlight := s.inFlight
		s.mu.Unlock()
		return inFlight == 2 && len(byIDRequests(s)) == 2
	}
	var reached atomic.Bool
	go func() {
		for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if onlyScansLeft() {
				reached.Store(true)
				break
			}
		}
		// Time for the by-id reads' answers to reach descendantChanges.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	type result struct {
		changes []provider.Change
		err     error
	}
	done := make(chan result, 1)
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	go func() {
		changes, err := p.Changes(ctx, spec)
		done <- result{changes, err}
	}()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("Changes = %v, %v; want the cancellation", changeIDs(got.changes), got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Changes still blocked 3s after its context was cancelled")
	}
	if !reached.Load() {
		t.Error("the cancel came before only the residual scans were left")
	}
}

// A track whose descendant rows were merged says when the limit left a change
// out. Without them, the track is today's.
func TestChanges_DescendantsLimitMarker(t *testing.T) {
	rows := []loggedChange{
		ch(1, 1, "dcim.device", 17),
		created(2, 2, "ipam.ipaddress", 401, assignedTo(14, 100)),
		ch(3, 3, "dcim.interface", 100).relatedTo("dcim.device", 17),
		ch(4, 4, "dcim.device", 17),
		created(5, 5, "dcim.module", 500, `{"device":17}`),
	}
	from := scopedNow.Add(-60 * time.Minute)
	start := func() *Provider {
		s := newScopedChangeServer(rows...)
		s.interfaces = []childRow{{id: 100, owner: 17}}
		return s.start(t)
	}
	spec := withDescendants(nameScope("leaf1"))
	spec.From, spec.Limit = from, 3

	t.Run("descendants merged", func(t *testing.T) {
		changes := changesOK(t, start(), spec)
		wantIDs(t, changes, "#1", "#2", "#3", "limit")
		c := changes[len(changes)-1]
		if until := scopedNow.Add(-3 * time.Minute); !c.Time.Equal(from) || !c.Until.Equal(until) || c.Limit != 3 {
			t.Errorf("limit marker at %v, until %v, limit %d; want at From %v, until %v, limit 3", c.Time, c.Until, c.Limit, from, until)
		}
	})
	t.Run("From zero", func(t *testing.T) {
		spec := spec
		spec.From = time.Time{}
		changes := changesOK(t, start(), spec)
		wantIDs(t, changes, "#1", "#2", "#3", "limit")
		if c, until := changes[len(changes)-1], scopedNow.Add(-3*time.Minute); !c.Time.Equal(until) || !c.Until.Equal(until) {
			t.Errorf("limit marker at %v, until %v; want both at the oldest shown change, %v", c.Time, c.Until, until)
		}
	})
	t.Run("descendants off", func(t *testing.T) {
		spec := spec
		spec.Descendants = false
		wantIDs(t, changesOK(t, start(), spec), "#1", "#3", "#4")
	})
	// The change-log reads stop short of the limit (#1 and #2); the journal
	// merge drops the older of two entries. The oldest shown is the newer one.
	t.Run("journal merge drops a change", func(t *testing.T) {
		s := newScopedChangeServer(rows[0], rows[1])
		s.interfaces = []childRow{{id: 100, owner: 17}}
		s.journal = []journalRow{
			{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 3},
			{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 4},
		}
		spec := spec
		spec.Journal = true
		changes := changesOK(t, s.start(t), spec)
		var kinds []provider.ChangeKind
		for _, c := range changes {
			kinds = append(kinds, c.Kind)
		}
		want := []provider.ChangeKind{provider.ChangeKindChange, provider.ChangeKindChange, provider.ChangeKindJournal, provider.ChangeKindLimit}
		if !reflect.DeepEqual(kinds, want) {
			t.Fatalf("kinds = %v (%v), want two changes, a journal entry, then limit", kinds, changeIDs(changes))
		}
		if c := changes[3]; !c.Until.Equal(scopedNow.Add(-3 * time.Minute)) {
			t.Errorf("limit marker until %v, want the journal entry's time", c.Until)
		}
	})
	t.Run("unscoped", func(t *testing.T) {
		wantIDs(t, changesOK(t, start(), provider.ChangeSpec{From: from, Limit: 3, Descendants: true}), "#1", "#2", "#3")
	})
}

// On an interface scope the related read and the address scan return the same
// IP changes, and the track keeps each once. The address scan stopping at the
// limit still fills the track: a read that stops there holds limit distinct
// changes, so the dedupe can't leave the track short of it. The limit marker
// shows, until the oldest shown change; nothing older is shown.
func TestChanges_DescendantsLimitMarkerAfterDedupe(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 5; i++ {
		rows = append(rows, created(i, i, "ipam.ipaddress", int64(400+i), assignedTo(14, 100)).relatedTo("dcim.interface", 100))
	}
	s := newestFirst(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	spec := withDescendants(idScope("100"))
	spec.ScopeType, spec.Limit = "dcim.interface", 3
	changes := changesOK(t, s.start(t), spec)
	wantIDs(t, changes, "#1", "#2", "#3", "limit")
	if c, until := changes[len(changes)-1], scopedNow.Add(-3*time.Minute); !c.Until.Equal(until) {
		t.Errorf("limit marker until %v, want the oldest shown change, %v", c.Until, until)
	}
	if scans := scanReads(s, 20); len(scans) != 1 {
		t.Errorf("address scans = %v, want one", scans)
	}
}

func TestDescendantsDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		caller  time.Duration // 0: no caller deadline
		want    time.Duration
	}{
		{"capped at 8s", 30 * time.Second, 0, 8 * time.Second},
		{"half the Timeout", 10 * time.Second, 0, 5 * time.Second},
		{"a fifth of the caller's time left", 30 * time.Second, 5 * time.Second, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New("http://nb", "token", http.DefaultClient, WithRequestTimeout(tc.timeout))
			ctx := context.Background()
			if tc.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.caller)
				defer cancel()
			}
			want := time.Now().Add(tc.want)
			if got := p.descendantsDeadline(ctx); got.Sub(want).Abs() > 250*time.Millisecond {
				t.Errorf("deadline = now+%v, want about now+%v", time.Until(got).Round(time.Millisecond), tc.want)
			}
		})
	}
}

func TestResidualContext(t *testing.T) {
	t.Run("half the time left", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		want := time.Now().Add(time.Second)
		rctx, stop := residualContext(ctx)
		defer stop()
		if got, ok := rctx.Deadline(); !ok || got.Sub(want).Abs() > 250*time.Millisecond {
			t.Errorf("deadline = now+%v (%v), want about now+1s", time.Until(got).Round(time.Millisecond), ok)
		}
	})
	t.Run("no deadline", func(t *testing.T) {
		rctx, stop := residualContext(context.Background())
		defer stop()
		if got, ok := rctx.Deadline(); ok {
			t.Errorf("deadline = %v, want none", got)
		}
	})
	t.Run("stop ends only it", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		rctx, stop := residualContext(ctx)
		stop()
		if rctx.Err() == nil || ctx.Err() != nil {
			t.Errorf("after stop: residual Err %v, descendants Err %v; want ended, live", rctx.Err(), ctx.Err())
		}
	})
}

// holdChanges holds the change-log requests carrying typeID until the returned
// open is called, which is safe to call more than once and is called when the
// test ends. Call it after s.start, so the gate opens before the server closes.
func holdChanges(t *testing.T, s *scopedChangeServer, typeID int) (open func()) {
	t.Helper()
	gate := make(chan struct{})
	s.changeHoldTypeID, s.changeHold = typeID, gate
	var once sync.Once
	open = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	return open
}

// The residual scans read one page each, of residualRows rows: 500 unless a
// test lowers it.
func TestChanges_ResidualScansReadOnePage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override int
		limit    string
	}{
		{"lowered", 2, "2"},
		{"default", 0, "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []loggedChange
			for i := 1; i <= 5; i++ {
				rows = append(rows,
					created(i, i, "ipam.ipaddress", int64(400+i), assignedTo(14, 200)),
					created(10+i, i, "dcim.module", int64(500+i), `{"device":18}`))
			}
			s := newestFirst(rows...)
			s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
			p := s.start(t)
			p.residualRowsOverride = tc.override
			changesOK(t, p, withDescendants(nameScope("leaf1")))
			for name, ids := range map[string][]int{"address": {20, 21}, "parts": {22, 23}} {
				if scans := scanReads(s, ids...); len(scans) != 1 || scans[0].Get("limit") != tc.limit {
					t.Errorf("%s scans = %v, want one with limit=%s", name, scans, tc.limit)
				}
			}
		})
	}
}

// One page, whatever its size: a page shorter than asked, with a next link,
// isn't followed. The IP was unassigned in range from leaf1's interface, and
// its history is gone: only the address scan can find it.
func TestChanges_ResidualScanReadsOnePageWhateverItsSize(t *testing.T) {
	rows := []loggedChange{ch(1, 1, "ipam.ipaddress", 401).snapshots(assignedTo(14, 100), unassigned)}
	for i := 2; i <= 5; i++ {
		rows = append(rows, created(i, i, "ipam.ipaddress", int64(400+i), assignedTo(14, 200)))
	}
	s := newestFirst(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	s.scanPageSize = 2
	changes := changesOK(t, s.start(t), withDescendants(nameScope("leaf1")))
	for name, ids := range map[string][]int{"address": {20, 21}, "parts": {22, 23}} {
		if scans := scanReads(s, ids...); len(scans) != 1 {
			t.Errorf("%s scans = %v, want one", name, scans)
		}
	}
	// The page was matched, and the address scan is capped at its two rows.
	wantIDs(t, changes, "#1", "cutoff")
	if cut, want := changes[len(changes)-1], scopedNow.Add(-2*time.Minute); !cut.Until.Equal(want) || !nearNow(cut.Time) {
		t.Errorf("cut-off at %v until %v, want at about now, until the page's oldest row %v", cut.Time, cut.Until, want)
	}
}

// The residual scans start with membership, and the address scan matches once
// membership is known. The IP was unassigned in range, and its history is gone:
// only the address scan can find it.
func TestChanges_ResidualScansStartWithMembership(t *testing.T) {
	s := newScopedChangeServer(ch(1, 20, "ipam.ipaddress", 401).snapshots(assignedTo(14, 100), unassigned))
	s.interfaces = []childRow{{id: 100, owner: 17}}
	p := s.start(t)
	open := holdChanges(t, s, scopedTypeIDs["dcim.interface"]) // the interface-delete read

	type result struct {
		changes []provider.Change
		err     error
	}
	done := make(chan result, 1)
	go func() {
		changes, err := p.Changes(context.Background(), withDescendants(nameScope("leaf1")))
		done <- result{changes, err}
	}()
	started := func() bool { return len(scanReads(s, 20, 21)) == 1 && len(scanReads(s, 22, 23)) == 1 }
	for end := time.Now().Add(2 * time.Second); !started(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			open()
			t.Fatal("residual scans waited for membership")
		}
	}
	open()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Changes: %v", got.err)
		}
		wantIDs(t, got.changes, "#1")
	case <-time.After(5 * time.Second):
		t.Fatal("Changes still blocked 5s after membership was let through")
	}
}

// A residual scan held past its own deadline, or failing, leaves the indexed
// results standing: the track doesn't wait for the descendants deadline, and
// has no descendants-error.
func TestChanges_ResidualTimeoutKeepsIndexedResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hold   time.Duration
		status int
	}{
		{"held past its deadline", 5 * time.Second, 0},
		{"failed", 0, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newestFirst(
				ch(1, 1, "dcim.device", 17),
				ipOn(2, 2, "create", 401, 100),
				ch(3, 3, "dcim.module", 500).snapshots(`{"device":17,"serial":"a"}`, `{"device":17,"serial":"b"}`),
			)
			s.interfaces = []childRow{{id: 100, owner: 17}}
			s.modules = []childRow{{id: 500, owner: 17}}
			s.scanHold, s.scanStatus = tc.hold, tc.status
			p := s.start(t)
			p.descendantsWaitOverride = 2 * time.Second
			spec := withDescendants(nameScope("leaf1"))
			spec.From = scopedNow.Add(-60 * time.Minute)
			changes, elapsed := changesWithin(t, p, spec, 4*time.Second)
			wantIDs(t, changes, "#1", "#2", "#3", "cutoff")
			cut := changes[len(changes)-1]
			if all := []string{"ipam.ipaddress", "dcim.macaddress", "dcim.module", "ipam.service"}; !cut.Residual || !cut.Until.IsZero() || !nearNow(cut.Time) || !slices.Equal(cut.Types, all) {
				t.Errorf("cut-off = Residual %v Until %v at %v Types %v; want residual, no reach, at the end of the range, all four types", cut.Residual, cut.Until, cut.Time, cut.Types)
			}
			if elapsed >= 1800*time.Millisecond {
				t.Errorf("Changes took %v, want the residual scans given up at their own deadline, about 1s", elapsed)
			}
			if tc.hold == 0 {
				return
			}
			ended := func() int {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.scanHoldEnded
			}
			for end := time.Now().Add(time.Second); ended() != 2 && time.Now().Before(end); {
				time.Sleep(5 * time.Millisecond)
			}
			if n := ended(); n != 2 {
				t.Errorf("held residual scans ended by their request's context = %d, want 2: cancelled, not abandoned", n)
			}
		})
	}
}

// A page that arrived in time is matched once membership is known, even after
// the residual deadline. The IP was unassigned in range with no history left:
// only the address scan can find it.
func TestChanges_ResidualPageMatchedAfterSlowMembership(t *testing.T) {
	s := newestFirst(
		ch(1, 1, "ipam.ipaddress", 401).snapshots(assignedTo(14, 100), unassigned),
		created(2, 5, "ipam.ipaddress", 402, assignedTo(14, 200)),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	p := s.start(t)
	p.residualRowsOverride = 1
	// The residual deadline is about 1.5s. The interface-delete read is let
	// through at 2.2s: about 0.7s past it, and 0.8s before the descendants one.
	p.descendantsWaitOverride = 3 * time.Second
	time.AfterFunc(2200*time.Millisecond, holdChanges(t, s, scopedTypeIDs["dcim.interface"]))
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	changes, _ := changesWithin(t, p, spec, 5*time.Second)
	wantIDs(t, changes, "#1", "cutoff")
	// The page's reach: an unchecked scan would have none.
	if cut, want := changes[len(changes)-1], scopedNow.Add(-time.Minute); !cut.Residual || !cut.Until.Equal(want) || !nearNow(cut.Time) {
		t.Errorf("cut-off Residual %v until %v at %v, want residual, until the page's oldest row %v, at the end of the range", cut.Residual, cut.Until, cut.Time, want)
	}
}

// Membership failing ends an address scan waiting for it, at once: the track
// waits for neither deadline, and the failure is its marker's.
func TestChanges_MembershipFailureEndsTheWaitingAddressScan(t *testing.T) {
	s := trackServer()
	s.failTypeID = scopedTypeIDs["dcim.interface"] // the interface-delete read
	// So the address scan's page has arrived, and waits for membership.
	s.failDelay = 200 * time.Millisecond
	p := s.start(t)
	p.descendantsWaitOverride = 3 * time.Second
	changes, elapsed := changesWithin(t, p, withDescendants(nameScope("leaf1")), 5*time.Second)
	if elapsed >= time.Second {
		t.Errorf("Changes took %v, want the address scan ended when membership failed", elapsed)
	}
	var apiErr *APIError
	if c := descendantsError(t, changes); !errors.As(c.Err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Err = %v, want the interface-delete read's 500", c.Err)
	}
	if scans := scanReads(s, 20, 21); len(scans) != 1 {
		t.Errorf("address scans = %v, want one", scans)
	}
}

// An error that came back before the descendants deadline keeps its own cause,
// though another read was still held when it did: the IP-history read fails
// beside the cable-end read, which is held past the deadline. Unless the
// failure stops it as it comes, the deadline passes first.
func TestChanges_EarlyFailureIsNotSlow(t *testing.T) {
	s := trackServer()
	s.failTypeID = 20
	// So the cable-end read is sent, and held, before the 500 comes.
	s.failDelay = 100 * time.Millisecond
	s.delayTypeID, s.delayFor = scopedTypeIDs["dcim.cabletermination"], 5*time.Second
	p := s.start(t)
	// Long enough that the IP-history read, held 100ms, fails well inside it,
	// even on a loaded -race run on one CPU.
	p.descendantsWaitOverride = time.Second
	changes, _ := changesWithin(t, p, withDescendants(nameScope("leaf1")), 3*time.Second)
	wantIDs(t, changes, "#1", "#2", "descendants-error")
	c := descendantsError(t, changes)
	if u := provider.Classify(c.Err); u != nil && u.AnnotationReason == provider.AnnotationReasonDescendantsSlow {
		t.Errorf("Err = %v, want the 500's own cause", c.Err)
	}
	var apiErr *APIError
	if !errors.As(c.Err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Err = %v, want the IP-history read's 500", c.Err)
	}
	if ends := readsCarrying(s, scopedTypeIDs["dcim.cabletermination"]); len(ends) != 1 {
		t.Errorf("cable-end reads = %v, want the one held beside the IP-history read", ends)
	}
}

// A descendant read that fails stops the other descendant reads rather than
// leave them to the descendants deadline, or the residual deadline: the parts
// scan, held beside membership, is cancelled, and the track ends at once, with
// the IP-history read's 500 on its marker.
func TestChanges_DescendantFailureStopsTheOthers(t *testing.T) {
	s := trackServer()
	s.failTypeID = 20
	// So the parts scan is sent, and held, before the 500 comes.
	s.failDelay = 100 * time.Millisecond
	s.delayTypeID, s.delayFor = 22, 5*time.Second
	p := s.start(t)
	p.descendantsWaitOverride = 3 * time.Second
	changes, elapsed := changesWithin(t, p, withDescendants(nameScope("leaf1")), 5*time.Second)
	if elapsed > time.Second {
		t.Errorf("Changes took %v, want the parts scan stopped when the IP-history read failed", elapsed)
	}
	wantIDs(t, changes, "#1", "#2", "descendants-error")
	var apiErr *APIError
	if c := descendantsError(t, changes); !errors.As(c.Err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Err = %v, want the IP-history read's 500", c.Err)
	}
	if parts := scanReads(s, 22, 23); len(parts) != 1 {
		t.Errorf("parts scans = %v, want the one held beside membership", parts)
	}
}

// A read cut by the HTTP client's own timeout matches context.DeadlineExceeded
// too, but the descendants deadline didn't cut it: it keeps its own cause.
func TestChanges_RequestTimeoutIsNotSlow(t *testing.T) {
	s := trackServer()
	s.delayTypeID, s.delayFor = 20, 5*time.Second
	p := s.start(t)
	p.client.http.Timeout = 200 * time.Millisecond
	p.descendantsWaitOverride = 3 * time.Second
	changes, _ := changesWithin(t, p, withDescendants(nameScope("leaf1")), 2*time.Second)
	wantIDs(t, changes, "#1", "#2", "descendants-error")
	c := descendantsError(t, changes)
	if u := provider.Classify(c.Err); u != nil && u.AnnotationReason == provider.AnnotationReasonDescendantsSlow {
		t.Errorf("Err = %v, want the client timeout's own cause", c.Err)
	}
	if !errors.Is(c.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want a timeout", c.Err)
	}
}

// A query whose own read failed doesn't wait out the descendant reads: they
// are cancelled, and the own read's error is the query's. The reads of IP
// changes, the IP-history read and the address scan, are held for longer than
// the test waits, with the descendants deadline left at its 8s default.
func TestChanges_FailedReadCancelsDescendants(t *testing.T) {
	s := trackServer()
	s.failTypeID = scopedTypeIDs["dcim.device"]
	s.delayTypeID, s.delayFor = 20, 5*time.Second
	p := s.start(t)
	done := make(chan error, 1)
	go func() {
		_, err := p.Changes(context.Background(), withDescendants(nameScope("leaf1")))
		done <- err
	}()
	select {
	case err := <-done:
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
			t.Errorf("Changes error = %v, want the own read's 500", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Changes still blocked 1s after its own read failed")
	}
}

// Match by is read whatever its case and padding: a provisioned "ID" matches by
// ID, in the scope lookup and in the descendant reads, which list the
// interfaces of only the ids that exist. By name, "17" and "99" match no
// device, and the interface list would refuse the deleted device 99.
func TestChanges_ScopeMatchIDIgnoresCaseAndPadding(t *testing.T) {
	for _, match := range []string{"ID", " id "} {
		t.Run(match, func(t *testing.T) {
			spec := withDescendants(idScope("17", "99"))
			spec.ScopeMatch = match
			wantIDs(t, changesOK(t, trackServer().start(t), spec), "#1", "#2", "#3", "#4")
		})
	}
}

func TestChanges_DescendantsCarryBranch(t *testing.T) {
	s := eachKindServer()
	s.branches = `{"count":1,"next":null,"results":[{"name":"br1","schema_id":"td5smq0f"}]}`
	changes, err := s.start(t).Changes(provider.WithBranch(context.Background(), "br1"), withDescendants(nameScope("leaf1")))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	wantIDs(t, changes, "#1", "#2", "#3", "#4", "#5", "#6", "#7", "#8")

	for _, path := range []string{"dcim/interfaces", "dcim/cables", "dcim/modules", "ipam/services", "dcim/mac-addresses"} {
		if len(s.requestsTo(path)) == 0 {
			t.Errorf("no %s request", path)
		}
	}
	if len(scanReads(s, 20)) == 0 {
		t.Error("no address scan")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byIDOnBranch := false
	for i, h := range s.headers {
		if h != "td5smq0f" {
			t.Errorf("%s sent X-NetBox-Branch %q, want td5smq0f", s.requests[i], h)
		}
		// A by-id read: a change-log request carrying changed_object_id, not the
		// scope's own read (dcim.device).
		u, err := url.Parse(s.requests[i])
		if err != nil {
			t.Fatalf("request %q: %v", s.requests[i], err)
		}
		q := u.Query()
		if u.Path == "/api/"+changeLogEndpoint+"/" && q.Has(changedObjectIDParam) && !carries(q, scopedTypeIDs["dcim.device"]) && h == "td5smq0f" {
			byIDOnBranch = true
		}
	}
	if !byIDOnBranch {
		t.Error("no by-id read sent with X-NetBox-Branch td5smq0f")
	}
}

func TestMatchers(t *testing.T) {
	change := func(pre, post string) objectChange {
		return objectChange{PrechangeData: decodeObject(json.RawMessage(pre)), PostchangeData: decodeObject(json.RawMessage(post))}
	}
	ifaces := map[int64]int64{100: 17, 200: 18}
	scopeIDs := map[int64]bool{17: true}
	for _, tc := range []struct {
		name      string
		match     func(objectChange) (int64, bool)
		pre, post string
		owner     int64
		ok        bool
	}{
		{"assigned: after side", nil, "", assignedTo(14, 100), 17, true},
		{"assigned: before side", nil, assignedTo(14, 100), unassigned, 17, true},
		{"assigned: after side wins", nil, assignedTo(14, 100), assignedTo(14, 200), 18, true},
		{"assigned: type mismatch", nil, "", assignedTo(31, 100), 0, false},
		{"assigned: string id", nil, "", `{"assigned_object_type":14,"assigned_object_id":"100"}`, 0, false},
		{"service: 4.3 shape", func(oc objectChange) (int64, bool) { return matchServiceParent(oc, 12, "device", scopeIDs) },
			"", `{"parent_object_type":12,"parent_object_id":17}`, 17, true},
		{"service: 4.2 shape", func(oc objectChange) (int64, bool) { return matchServiceParent(oc, 12, "device", scopeIDs) },
			"", `{"device":17}`, 17, true},
		{"service: wrong parent type", func(oc objectChange) (int64, bool) { return matchServiceParent(oc, 12, "device", scopeIDs) },
			"", `{"parent_object_type":30,"parent_object_id":17}`, 0, false},
		{"owner: before side", func(oc objectChange) (int64, bool) { return matchOwner(oc, "device", scopeIDs) },
			`{"device":17}`, "", 17, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match := tc.match
			if match == nil {
				match = func(oc objectChange) (int64, bool) { return matchAssigned(oc, 14, ifaces) }
			}
			if owner, ok := match(change(tc.pre, tc.post)); owner != tc.owner || ok != tc.ok {
				t.Errorf("= %d, %v; want %d, %v", owner, ok, tc.owner, tc.ok)
			}
		})
	}
}

// nearNow reports whether t is within a second of the present.
func nearNow(t time.Time) bool {
	d := time.Since(t)
	return d > -time.Second && d < time.Second
}

// A residual scan cut at its page makes a cut-off at the end of the range that
// names only the types it scanned: the indexed reads covered the rest of it.
func TestChanges_ResidualCutoffAtTheEnd(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 3; i++ {
		rows = append(rows, ipOn(i, i, "create", int64(400+i), 200))
	}
	s := newestFirst(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	p := s.start(t)
	p.residualRowsOverride = 2
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	spec.To = scopedNow.Add(time.Minute)
	changes := changesOK(t, p, spec)
	wantIDs(t, changes, "cutoff")
	cut := changes[len(changes)-1]
	if !cut.Residual || !cut.Time.Equal(spec.To) || !cut.Until.Equal(scopedNow.Add(-2*time.Minute)) {
		t.Errorf("cut-off Residual %v at %v until %v; want residual, at To %v, until %v", cut.Residual, cut.Time, cut.Until, spec.To, scopedNow.Add(-2*time.Minute))
	}
	// With no To the end of the range is now.
	spec.To = time.Time{}
	changes = changesOK(t, p, spec)
	if cut := changes[len(changes)-1]; !cut.Residual || !nearNow(cut.Time) {
		t.Errorf("cut-off Residual %v at %v; want residual, at about now when To is zero", cut.Residual, cut.Time)
	}
	if want := []string{"ipam.ipaddress", "dcim.macaddress"}; !slices.Equal(cut.Types, want) {
		t.Errorf("Types = %v, want %v", cut.Types, want)
	}
}

// Object types narrow what the residual marker names, and the reads behind it.
func TestChanges_ResidualCutoffModuleOnly(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 3; i++ {
		rows = append(rows, created(i, i, "dcim.module", int64(500+i), `{"device":18}`))
	}
	s := newestFirst(rows...)
	p := s.start(t)
	p.residualRowsOverride = 2
	spec := withDescendants(nameScope("leaf1"))
	spec.ObjectTypes = []string{"dcim.module"}
	spec.From = scopedNow.Add(-60 * time.Minute)
	changes := changesOK(t, p, spec)
	if len(changes) == 0 {
		t.Fatal("no changes, want a residual cut-off")
	}
	cut := changes[len(changes)-1]
	if cut.Kind != provider.ChangeKindCutoff || !cut.Residual || !slices.Equal(cut.Types, []string{"dcim.module"}) {
		t.Fatalf("last change = %+v, want a residual cut-off of dcim.module", cut)
	}
	for _, path := range []string{"dcim/interfaces", "dcim/mac-addresses", "dcim/cables", "ipam/services"} {
		if lists := s.requestsTo(path); len(lists) != 0 {
			t.Errorf("%s requests = %v, want none", path, lists)
		}
	}
	if lists := s.requestsTo("dcim/modules"); len(lists) != 1 {
		t.Errorf("module lists = %v, want one", lists)
	}
	deletes := slices.DeleteFunc(s.changeLog(), func(q url.Values) bool { return !carries(q, 14) || q.Get("action") != "delete" })
	if len(deletes) != 1 {
		t.Errorf("interface-delete reads = %v, want one", deletes)
	}
	for _, q := range s.changeLog() {
		if carries(q, 20, 21) {
			t.Errorf("change-log read %v: want none for addresses", q)
		}
	}
	if parts := scanReads(s, 22, 23); len(parts) != 1 || !reflect.DeepEqual(parts[0][changedObjectTypeIDParam], []string{"22"}) {
		t.Errorf("parts scans = %v, want one with changed_object_type_id=[22]", parts)
	}
}

// A refused list leaves the scans' misses undescribable by the residual
// marker: it keeps the general text, at its reach.
func TestChanges_ResidualCutoffGeneralWhenAListRefused(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 3; i++ {
		rows = append(rows, ipOn(i, i, "create", int64(400+i), 200))
	}
	s := newestFirst(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	s.macs = []macRow{{id: 403, ct: "dcim.interface", iface: 100}}
	s.listStatusFor = map[string]int{"dcim/mac-addresses": http.StatusForbidden}
	p := s.start(t)
	p.residualRowsOverride = 2
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	changes := changesOK(t, p, spec)
	wantIDs(t, changes, "cutoff")
	cut := changes[len(changes)-1]
	if cut.Residual || !cut.DescendantsOnly || !cut.Time.Equal(scopedNow.Add(-2*time.Minute)) {
		t.Errorf("cut-off Residual %v DescendantsOnly %v at %v; want general, descendants-only, at its reach %v", cut.Residual, cut.DescendantsOnly, cut.Time, scopedNow.Add(-2*time.Minute))
	}
}

// The residual cut-off sits at the end of the range and the limit marker at
// From, so Grafana can show both; each says its own reach in Until.
func TestChanges_ResidualAndLimitMarkers(t *testing.T) {
	s := newestFirst(
		ipOn(1, 1, "create", 400, 200),
		ipOn(2, 2, "create", 401, 100),
		ipOn(3, 3, "create", 402, 100),
		ipOn(4, 4, "create", 403, 100),
	)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	p := s.start(t)
	p.residualRowsOverride = 1
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	spec.To = scopedNow.Add(time.Minute)
	spec.Limit = 2
	changes := changesOK(t, p, spec)
	wantIDs(t, changes, "#2", "#3", "cutoff", "limit")
	cut, lim := changes[2], changes[3]
	if !cut.Residual || !cut.Time.Equal(spec.To) || !cut.Until.Equal(scopedNow.Add(-time.Minute)) {
		t.Errorf("cut-off = Residual %v at %v until %v, want residual at To %v until %v", cut.Residual, cut.Time, cut.Until, spec.To, scopedNow.Add(-time.Minute))
	}
	if cut.Time.Equal(lim.Time) {
		t.Errorf("cut-off and limit marker share time %v", cut.Time)
	}
	if !lim.Time.Equal(spec.From) || !lim.Until.Equal(scopedNow.Add(-3*time.Minute)) {
		t.Errorf("limit marker at %v until %v, want at From until %v", lim.Time, lim.Until, scopedNow.Add(-3*time.Minute))
	}
}

// A residual page that is capped but yielded no row with a time checked
// nothing: its marker is the not-checked form at the end of the range, and
// stands even when the own reads fill Limit.
func TestChanges_ResidualPageWithoutTimedRowsIsUnchecked(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"no rows", `{"count":9,"next":"http://x/api/core/object-changes/?offset=1","previous":null,"results":[]}`},
		{"unparseable time", `{"count":9,"next":"http://x/api/core/object-changes/?offset=1","previous":null,"results":[{"id":99,"time":"garbage","action":{"value":"delete"},"changed_object_type":"ipam.ipaddress","changed_object_id":9}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newestFirst(ch(1, 1, "dcim.device", 17), ch(2, 2, "dcim.device", 17), ch(3, 3, "dcim.device", 17))
			s.scanBody = tc.body
			p := s.start(t)
			spec := withDescendants(nameScope("leaf1"))
			spec.From = scopedNow.Add(-60 * time.Minute)
			spec.To = scopedNow.Add(time.Minute)
			spec.Limit = 2
			changes := changesOK(t, p, spec)
			cut := changes[len(changes)-1]
			if cut.Kind == provider.ChangeKindLimit && len(changes) > 1 {
				cut = changes[len(changes)-2]
			}
			if cut.Kind != provider.ChangeKindCutoff || !cut.Residual || !cut.Until.IsZero() || !cut.Time.Equal(spec.To) {
				t.Errorf("cut-off = Kind %v Residual %v Until %v at %v; want residual, no reach, at To %v", cut.Kind, cut.Residual, cut.Until, cut.Time, spec.To)
			}
		})
	}
}

// removedServer is leaf1 (17) with interface 100, and leaf2 (18) with
// interface 200. Leaf1's MAC 403, module 500 and service 600 were deleted
// (#3, #2, #1) after their creates (#4-#6). No list names them, and no history
// is recorded against what they were on: only the residual pages find them.
// Leaf2's newer IPs and modules fill those pages, three rows each, so the
// creates lie past their reach: the parts page reaches back 6 minutes, the
// address page 7. Leaf2's IPs aren't recorded against its interface, so no
// IP history is read.
func removedServer() *scopedChangeServer {
	svc := `{"parent_object_type":12,"parent_object_id":17}`
	rows := []loggedChange{
		ch(1, 3, "ipam.service", 600).as("delete").snapshots(svc, ""),
		ch(2, 4, "dcim.module", 500).as("delete").snapshots(`{"device":17}`, ""),
		ch(3, 5, "dcim.macaddress", 403).as("delete").snapshots(assignedTo(14, 100), ""),
		created(4, 30, "dcim.macaddress", 403, assignedTo(14, 100)),
		created(5, 31, "dcim.module", 500, `{"device":17}`),
		created(6, 32, "ipam.service", 600, svc),
	}
	for i := range 3 {
		rows = append(rows,
			created(10+i, 6+i, "ipam.ipaddress", int64(700+i), assignedTo(14, 200)),
			created(20+i, 6+i, "dcim.module", int64(800+i), `{"device":18}`))
	}
	s := newestFirst(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
	return s
}

// removedSpec is leaf1 over the last hour, with Include descendants on.
func removedSpec() provider.ChangeSpec {
	spec := withDescendants(nameScope("leaf1"))
	spec.From = scopedNow.Add(-60 * time.Minute)
	return spec
}

// A MAC, a module and a service the residual pages found deleted show their
// earlier changes in the range too, older than the pages' reach: a by-id read
// follows the pages, one per type, for what they found that isn't a member.
func TestChanges_RemovedObjectsShowTheirHistory(t *testing.T) {
	s := removedServer()
	p := s.start(t)
	p.residualRowsOverride = 3
	changes := changesOK(t, p, removedSpec())
	wantIDs(t, changes, "#1", "#2", "#3", "#4", "#5", "#6", "cutoff")
	wantParents(t, changes, "leaf1", "#1", "#2", "#3", "#4", "#5", "#6")
	if cut := changes[len(changes)-1]; !cut.Residual {
		t.Errorf("cut-off Residual = false, want the residual pages' cut-off")
	}
	want := map[string][]string{"21": {"403"}, "22": {"500"}, "23": {"600"}}
	if got := byIDAsks(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("by-id reads ask for %v, want %v", got, want)
	}
}

// The read that follows the residual pages is best effort: one that fails
// drops its own rows, and the track, the deletions in it, stands with no
// descendants-error and no fail.
func TestChanges_RemovedHistoryReadFailureKeepsTheTrack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed []int
		want   []string
	}{
		{"every type", []int{21, 22, 23}, []string{"#1", "#2", "#3", "cutoff"}},
		{"the module's", []int{22}, []string{"#1", "#2", "#3", "#4", "#6", "cutoff"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := removedServer()
			s.byIDStatusFor = map[int]int{}
			for _, id := range tc.failed {
				s.byIDStatusFor[id] = http.StatusInternalServerError
			}
			p := s.start(t)
			p.residualRowsOverride = 3
			wantIDs(t, changesOK(t, p, removedSpec()), tc.want...)

			// descendantsOf fails the test on an error or a call to fail.
			var kept []int
			for _, r := range descendantsOf(t, p, removedSpec()) {
				for _, d := range r.rows {
					kept = append(kept, d.oc.ID)
				}
			}
			slices.Sort(kept)
			kept = slices.Compact(kept)
			var wantKept []int
			for _, id := range tc.want[:len(tc.want)-1] {
				n, _ := strconv.Atoi(strings.TrimPrefix(id, "#"))
				wantKept = append(wantKept, n)
			}
			if !reflect.DeepEqual(kept, wantKept) {
				t.Errorf("descendant rows = %v, want %v", kept, wantKept)
			}
		})
	}
}

// The read that follows the residual pages doesn't scan: stopped at the scan
// cap, it makes no cut-off of its own, and the track's is the residual pages'.
func TestChanges_RemovedHistoryCapMakesNoCutoff(t *testing.T) {
	s := removedServer()
	for i := range 4 {
		s.changes = append(s.changes, ch(40+i, 40+i, "dcim.macaddress", 403).snapshots(assignedTo(14, 100), assignedTo(14, 100)))
	}
	p := s.start(t)
	p.residualRowsOverride, p.changeScanCapOverride = 3, 2
	changes := changesOK(t, p, removedSpec())
	cut := changes[len(changes)-1]
	if want := scopedNow.Add(-6 * time.Minute); cut.Kind != provider.ChangeKindCutoff || !cut.Residual || !cut.Until.Equal(want) {
		t.Errorf("last change = Kind %v Residual %v Until %v, want the residual cut-off until the pages' reach %v",
			cut.Kind, cut.Residual, cut.Until, want)
	}
}

// Residual pages that found only members, or removals not on the scope, make
// no read after them; nor do pages that read the whole range, which already
// hold every change of what they found removed.
func TestChanges_NoRemovedHistoryReadWithoutNewIDs(t *testing.T) {
	t.Run("members and removals off the scope", func(t *testing.T) {
		// Two-row pages, each capped: the address page holds member IP 401's
		// create and leaf2's IP 402's delete, the parts page member module 500's
		// create and leaf2's module 501's delete.
		s := newestFirst(
			ipOn(1, 1, "create", 401, 100),
			ipOn(2, 2, "delete", 402, 200),
			created(3, 3, "dcim.module", 500, `{"device":17}`),
			ch(4, 4, "dcim.module", 501).as("delete").snapshots(`{"device":18}`, ""),
			ipOn(5, 5, "create", 403, 200),
			created(6, 6, "dcim.module", 502, `{"device":18}`),
		)
		s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
		s.modules = []childRow{{id: 500, owner: 17}}
		p := s.start(t)
		p.residualRowsOverride = 2
		wantIDs(t, changesOK(t, p, removedSpec()), "#1", "#3", "cutoff")
		want := map[string][]string{"20": {"401"}, "22": {"500"}}
		if got := byIDAsks(t, s); !reflect.DeepEqual(got, want) {
			t.Errorf("by-id reads ask for %v, want only the members' %v", got, want)
		}
	})
	t.Run("pages that read the whole range", func(t *testing.T) {
		s := removedServer()
		wantIDs(t, changesOK(t, s.start(t), removedSpec()), "#1", "#2", "#3", "#4", "#5", "#6")
		if byID := byIDRequests(s); len(byID) != 0 {
			t.Errorf("by-id reads = %v, want none", byID)
		}
	})
}

// An object whose create is on the page, newer than the page's oldest row, has
// all its changes since on the page: no read follows for it. A create at the
// page's oldest row may share its time with a change past the page, so one
// does.
func TestChanges_NoRemovedHistoryReadWhenTheCreateIsOnThePage(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows int
		want map[string][]string
	}{
		{"newer than the oldest row", 3, map[string][]string{}},
		{"at the oldest row", 2, map[string][]string{"21": {"403"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newestFirst(
				ch(1, 2, "dcim.macaddress", 403).as("delete").snapshots(assignedTo(14, 100), ""),
				created(2, 3, "dcim.macaddress", 403, assignedTo(14, 100)),
				created(3, 6, "ipam.ipaddress", 700, assignedTo(14, 200)),
				created(4, 7, "ipam.ipaddress", 701, assignedTo(14, 200)),
			)
			s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
			p := s.start(t)
			p.residualRowsOverride = tc.rows
			wantIDs(t, changesOK(t, p, removedSpec()), "#1", "#2", "cutoff")
			if got := byIDAsks(t, s); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("by-id reads ask for %v, want %v", got, tc.want)
			}
		})
	}
}

// With one Action read, create or delete, the row the page found is the only
// one its object has: no read follows it. The delete page holds MAC 403's
// delete, the create page MAC 403's create as its oldest row.
func TestChanges_NoRemovedHistoryReadForOneCreateOrDeleteAction(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   []string
	}{
		{"delete", []string{"#1", "cutoff"}},
		{"create", []string{"#4", "cutoff"}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			s := newestFirst(
				ch(1, 2, "dcim.macaddress", 403).as("delete").snapshots(assignedTo(14, 100), ""),
				ch(2, 3, "ipam.ipaddress", 700).as("delete").snapshots(assignedTo(14, 200), ""),
				ch(3, 4, "ipam.ipaddress", 701).as("delete").snapshots(assignedTo(14, 200), ""),
				created(4, 30, "dcim.macaddress", 403, assignedTo(14, 100)),
				created(5, 1, "ipam.ipaddress", 702, assignedTo(14, 200)),
				created(6, 40, "ipam.ipaddress", 703, assignedTo(14, 200)),
			)
			s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
			p := s.start(t)
			p.residualRowsOverride = 2
			spec := removedSpec()
			spec.Actions = []string{tc.action}
			wantIDs(t, changesOK(t, p, spec), tc.want...)
			if byID := byIDRequests(s); len(byID) != 0 {
				t.Errorf("by-id reads = %v, want none", byID)
			}
		})
	}
}

// A capped scanning read with no row of a parsed time has shown no reach: it
// counts as reaching the end of the range, not time zero. The IP history, read
// to the scan cap, stops at two rows whose times don't parse: its cut-off sits
// at To, and a track the own read fills to Limit still shows it.
func TestChanges_CappedReadWithoutTimedRowsCutsAtTheEnd(t *testing.T) {
	for _, limit := range []int{50, 2} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			rows := []loggedChange{ch(1, 1, "dcim.device", 17), ch(2, 2, "dcim.device", 17), ch(3, 3, "dcim.device", 17)}
			for i := range 3 {
				rows = append(rows, ipOn(10+i, 120+i, "create", int64(400+i), 100).withoutTime())
			}
			s := newestFirst(rows...)
			s.interfaces = []childRow{{id: 100, owner: 17}}
			p := s.start(t)
			p.changeScanCapOverride = 2
			spec := withDescendants(nameScope("leaf1"))
			spec.From = scopedNow.Add(-60 * time.Minute)
			spec.To = scopedNow.Add(time.Minute)
			spec.Limit = limit
			changes := changesOK(t, p, spec)
			i := slices.IndexFunc(changes, func(c provider.Change) bool { return c.Kind == provider.ChangeKindCutoff })
			if i < 0 {
				t.Fatalf("changes = %v, want a cut-off", changeIDs(changes))
			}
			if cut := changes[i]; cut.Residual || !cut.DescendantsOnly || !cut.Time.Equal(spec.To) {
				t.Errorf("cut-off Residual %v DescendantsOnly %v at %v; want general, descendants-only, at To %v", cut.Residual, cut.DescendantsOnly, cut.Time, spec.To)
			}
		})
	}
}
