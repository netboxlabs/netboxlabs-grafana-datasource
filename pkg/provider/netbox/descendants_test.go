package netbox

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
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

// readMembers runs descendantMembers on a scope set up by membersSetup.
func readMembers(p *Provider, spec provider.ChangeSpec, scope *resolvedScope, kind descendantKind, ids map[string]int, wantIfaces, wantCables bool) (members, error) {
	return p.descendantMembers(context.Background(), spec, scope, kind, ids, wantIfaces, wantCables)
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

// R7: a cable's owner is the one the cable list gave it, else the first match
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
