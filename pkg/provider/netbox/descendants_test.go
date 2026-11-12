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

// assignedTo is an address snapshot assigned to interface type typeID's id.
func assignedTo(typeID int, id int64) string {
	return fmt.Sprintf(`{"assigned_object_type":%d,"assigned_object_id":%d}`, typeID, id)
}

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

// A device with nothing attached reads no addresses, and scans only for
// modules and services: no cable can match. Its track is today's.
func TestChanges_DescendantsNothingAttached(t *testing.T) {
	rows := []loggedChange{
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
	}
	s := newScopedChangeServer(rows...)
	got := changesOK(t, s.start(t), withDescendants(nameScope("leaf1")))
	sameRows(t, got, changesOK(t, newScopedChangeServer(rows...).start(t), scoped(nameScope("leaf1"), 50)))

	for _, q := range s.changeLog() {
		if carries(q, 20, 21) {
			t.Errorf("change-log read %v: want no address scan", q)
		}
	}
	parts := scanReads(s, 17, 22, 23)
	if len(parts) != 1 {
		t.Fatalf("parts scans = %v, want one", parts)
	}
	if got := typeIDsOf(t, parts[0]); !reflect.DeepEqual(got, []int64{22, 23}) {
		t.Errorf("parts scan changed_object_type_id = %v, want [22 23]", got)
	}
}

func TestChanges_DescendantsOffMakesTodaysRequests(t *testing.T) {
	s := eachKindServer()
	changesOK(t, s.start(t), scoped(nameScope("leaf1"), 50))
	for _, path := range []string{"dcim/interfaces", "dcim/cables", "virtualization/virtual-machines", "virtualization/interfaces"} {
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

// Object types decides which scans run, and which children are read for them:
// a device's cables need its interfaces, which its cable ends are read
// through. The interface deletes that build them don't reach the track.
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
	parts := scanReads(s, 17, 22, 23)
	if len(parts) != 1 || !reflect.DeepEqual(parts[0]["changed_object_type_id"], []string{"17"}) {
		t.Errorf("parts scans = %v, want one with changed_object_type_id=17 only", parts)
	}
}

// A NetBox that ignores the scans' type filter can't widen them: a scan keeps
// only the types it asked for, though the IP and the module would match leaf1.
func TestChanges_DescendantScansCheckTheirTypes(t *testing.T) {
	s := newestFirst(
		ch(1, 1, "dcim.cable", 300),
		created(2, 2, "ipam.ipaddress", 401, assignedTo(14, 100)),
		created(3, 3, "dcim.module", 500, `{"device":17}`),
	)
	s.interfaces = []childRow{{id: 100, owner: 17, cable: 300}}
	s.cables = []cableRow{{id: 300, ends: []cableEnd{{"dcim.interface", 100, 17}}}}
	s.ignoreChangedQ = true
	spec := withDescendants(nameScope("leaf1"))
	spec.ObjectTypes = []string{"dcim.cable"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1")
}

// One Action is pushed to the scans. The membership reads don't take the
// query's filters: only the interface-delete read asks for an action, its own.
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

// A scan that stops at the cap ends the track with a cut-off that says only
// what's attached was cut.
func TestChanges_DescendantScanCapShowsCutoff(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 5; i++ {
		rows = append(rows, created(i, i, "ipam.ipaddress", int64(400+i), assignedTo(14, 200)))
	}
	rows = append(rows, ch(10, 10, "dcim.device", 17))
	s := newScopedChangeServer(rows...)
	s.interfaces = []childRow{{id: 100, owner: 17}}
	p := s.start(t)
	p.changeScanCapOverride = 3
	changes := changesOK(t, p, withDescendants(nameScope("leaf1")))
	wantIDs(t, changes, "#10", "cutoff")
	if cut := changes[len(changes)-1]; !cut.DescendantsOnly {
		t.Errorf("cut-off DescendantsOnly = false, want true: only the address scan was cut")
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

// A failed descendant read drops every descendant row, the parts scan's that
// succeeded included, and keeps the rest of the track, with a marker.
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
	if parts := scanReads(s, 22); len(parts) != 1 {
		t.Errorf("parts scans = %v, want one: its rows are dropped, not unread", parts)
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

// The caller's context ending is the query ending: an error, not a marker. It
// is cancelled once the address scan is held. By then the own and related
// reads have very likely finished, so it is a descendant read the cancel cuts;
// if one of them hadn't, the query still fails, as it should.
func TestChanges_CallerCancelledFails(t *testing.T) {
	s := trackServer()
	s.delayTypeID, s.delayFor = 20, 5*time.Second
	p := s.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for end := time.Now().Add(2 * time.Second); len(scanReads(s, 20)) == 0 && time.Now().Before(end); {
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
	if held := scanReads(s, 20); len(held) != 1 {
		t.Errorf("address scans = %v, want the one the cancel cut", held)
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

// An error that came back before the descendants deadline keeps its own cause,
// though the deadline then cut another read.
func TestChanges_EarlyFailureIsNotSlow(t *testing.T) {
	s := trackServer()
	s.failTypeID = 20
	s.delayTypeID, s.delayFor = 22, 5*time.Second
	p := s.start(t)
	// Long enough that the membership reads finish well inside it, even on a
	// loaded -race run: only the held parts scan is cut.
	p.descendantsWaitOverride = 500 * time.Millisecond
	changes, _ := changesWithin(t, p, withDescendants(nameScope("leaf1")), 2*time.Second)
	wantIDs(t, changes, "#1", "#2", "descendants-error")
	c := descendantsError(t, changes)
	if u := provider.Classify(c.Err); u != nil && u.AnnotationReason == provider.AnnotationReasonDescendantsSlow {
		t.Errorf("Err = %v, want the 500's own cause", c.Err)
	}
	var apiErr *APIError
	if !errors.As(c.Err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Err = %v, want the address scan's 500", c.Err)
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
// are cancelled, and the own read's error is the query's. The address scan is
// held for longer than the test waits, with the descendants deadline left at
// its 8s default.
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

func TestChanges_DescendantsCarryBranch(t *testing.T) {
	s := eachKindServer()
	s.branches = `{"count":1,"next":null,"results":[{"name":"br1","schema_id":"td5smq0f"}]}`
	changes, err := s.start(t).Changes(provider.WithBranch(context.Background(), "br1"), withDescendants(nameScope("leaf1")))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	wantIDs(t, changes, "#1", "#2", "#3", "#4", "#5", "#6", "#7", "#8")

	for _, path := range []string{"dcim/interfaces", "dcim/cables"} {
		if len(s.requestsTo(path)) == 0 {
			t.Errorf("no %s request", path)
		}
	}
	if len(scanReads(s, 20)) == 0 {
		t.Error("no address scan")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.headers {
		if h != "td5smq0f" {
			t.Errorf("%s sent X-NetBox-Branch %q, want td5smq0f", s.requests[i], h)
		}
	}
}

func TestMatchers(t *testing.T) {
	change := func(pre, post string) objectChange {
		return objectChange{PrechangeData: decodeObject(json.RawMessage(pre)), PostchangeData: decodeObject(json.RawMessage(post))}
	}
	ifaces := map[int64]int64{100: 17, 200: 18}
	scopeIDs := map[int64]bool{17: true}
	unassigned := `{"assigned_object_type":null,"assigned_object_id":null}`
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
