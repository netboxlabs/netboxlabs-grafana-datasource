package replicacache

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The ids are the demo NetBox's (django_content_type, NetBox 4.4.10, as loaded
// into the local replica): the replica serves the ids an address carries and
// no name for any of them, so what each stands for is learned from the rows.
const (
	idInterface   = 9
	idFrontPort   = 47
	idRearPort    = 48
	idVMInterface = 11
	idFHRPGroup   = 86
)

// The tables' rows as replica-cache serves them; 0 is null.

func ifaceRow(id, device, path int) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "device_id": num(device), "_path_id": num(path), "cable_id": nil, "cable_end": nil}
}

// cabledAt is an interface or a panel port with a cable on one end.
func cabledAt(id, cable int, end string) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "device_id": float64(1), "_path_id": nil, "cable_id": float64(cable), "cable_end": end}
}

func termRow(id, cable int, end string, typeID, objectID int) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "cable_id": float64(cable), "cable_end": end,
		"termination_type_id": float64(typeID), "termination_id": float64(objectID), "_device_id": float64(1)}
}

// pathRow is a cable path as the replica serves it: PostgreSQL's jsonb text,
// the origin first.
func pathRow(id int, nodes ...string) map[string]interface{} {
	steps := make([]string, len(nodes))
	for i, n := range nodes {
		steps[i] = fmt.Sprintf("[%q]", n)
	}
	return map[string]interface{}{"id": float64(id), "path": "[" + strings.Join(steps, ", ") + "]", "is_complete": true, "is_active": true}
}

func ownerRow(id, ip4, ip6 int) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "primary_ip4_id": num(ip4), "primary_ip6_id": num(ip6)}
}

func addrRow(id, typeID, objectID, natInside int) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "assigned_object_type_id": num(typeID), "assigned_object_id": num(objectID), "nat_inside_id": num(natInside)}
}

func vmIfaceRow(id, vm int) map[string]interface{} {
	return map[string]interface{}{"id": float64(id), "virtual_machine_id": num(vm)}
}

func idRow(id int) map[string]interface{} { return map[string]interface{}{"id": float64(id)} }

// assignFake is a replica whose tables are fed and empty.
func assignFake() *fakeService {
	f := newFakeService()
	f.addAssignmentTables()
	return f
}

// pathOrigin gives interface id a cable path that starts at it, as NetBox
// writes one, naming its type typeID.
func (f *fakeService) pathOrigin(typeID, id int) {
	f.addRows("dcim/interfaces", ifaceRow(id, 1, 500+id))
	f.addRows("dcim/cable-paths", pathRow(500+id, fmt.Sprintf("%d:%d", typeID, id), "53:1", fmt.Sprintf("%d:%d", typeID, id+1)))
}

// devicePrimary makes interface ifaceID of device 1 carry address addrID,
// which the device has as its primary IPv4.
func (f *fakeService) devicePrimary(typeID, ifaceID, addrID int) {
	f.addRows("dcim/devices", ownerRow(1, addrID, 0))
	f.addRows("dcim/interfaces", ifaceRow(ifaceID, 1, 0))
	f.addRows("ipam/ip-addresses", addrRow(addrID, typeID, ifaceID, 0))
}

// vmPrimary is devicePrimary for a virtual machine and its interface.
func (f *fakeService) vmPrimary(typeID, ifaceID, addrID int) {
	f.addRows("virtualization/virtual-machines", ownerRow(1, addrID, 0))
	f.addRows("virtualization/interfaces", vmIfaceRow(ifaceID, 1))
	f.addRows("ipam/ip-addresses", addrRow(addrID, typeID, ifaceID, 0))
}

// fhrpAddress puts an address on FHRP group groupID, which exists.
func (f *fakeService) fhrpAddress(typeID, addrID, groupID int) {
	f.addRows("ipam/fhrp-groups", idRow(groupID))
	f.addRows("ipam/ip-addresses", addrRow(addrID, typeID, groupID, 0))
}

func learn(t *testing.T, p *Provider, ids ...int) contentTypes {
	t.Helper()
	var st readStats
	got, err := p.assignmentTypes(context.Background(), ids, &st)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func wantTypes(t *testing.T, got, want contentTypes) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("learned %v, want %v", got, want)
	}
}

// A device's primary address is validated against its interfaces
// (Device.clean), so the type id it carries is the interface type's when the
// interface it names is the device's own: the one piece of evidence a NetBox
// with no cabled interface has.
func TestAssignmentTypes_InterfaceFromDevicePrimaryIP(t *testing.T) {
	f := assignFake()
	f.devicePrimary(idInterface, 5, 100)
	// A primary IPv6 is evidence as well, and so is a second device's.
	f.addRows("dcim/devices", ownerRow(2, 0, 101))
	f.addRows("dcim/interfaces", ifaceRow(6, 2, 0))
	f.addRows("ipam/ip-addresses", addrRow(101, idInterface, 6, 0))
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, idInterface), contentTypes{idInterface: "dcim.interface"})
}

// The same, from a NetBox whose interfaces are cabled and have no device with
// a primary address: NetBox writes a cable path only on the object it starts
// at, so the type of the first node of an interface's path is the interface
// type.
func TestAssignmentTypes_InterfaceFromCablePathWhenNoPrimaryIP(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, idInterface), contentTypes{idInterface: "dcim.interface"})
	// Read as an interface's path is: by id, the paths of the interfaces that
	// name one.
	if _, ok := f.requestWith("dcim/cable-paths", "filter[id]__in"); !ok {
		t.Error("the path was not read by id")
	}
}

func TestAssignmentTypes_VMInterfaceFromVMPrimaryIP(t *testing.T) {
	f := assignFake()
	f.vmPrimary(idVMInterface, 7, 200)
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, idVMInterface), contentTypes{idVMInterface: "virtualization.vminterface"})
}

// No address can be assigned to a VM interface when there is none: with no
// virtualization/interfaces rows (an empty NetBox table is never fed, so on
// most tenants there are none) the id left over once the device interfaces are
// known is the FHRP group's, if what it is assigned to is a group. When VM
// interfaces exist and none is learned, the id could be theirs, so it is left
// unknown.
func TestAssignmentTypes_NoVMInterfacesRulesThemOut(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *fakeService)
		want  contentTypes
	}{
		{"unfed", func(f *fakeService) { f.setIngested("virtualization/interfaces", false) },
			contentTypes{idInterface: "dcim.interface", idFHRPGroup: "ipam.fhrpgroup"}},
		{"fed and empty", func(f *fakeService) {},
			contentTypes{idInterface: "dcim.interface", idFHRPGroup: "ipam.fhrpgroup"}},
		{"VM interfaces exist, none learned", func(f *fakeService) { f.addRows("virtualization/interfaces", vmIfaceRow(7, 1)) },
			contentTypes{idInterface: "dcim.interface"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.fhrpAddress(idFHRPGroup, 300, 2)
			c.setup(f)
			p := newTestProvider(t, f)

			wantTypes(t, learn(t, p, idInterface, idFHRPGroup), c.want)
		})
	}
}

// The group's id is the one left once the other two are learned, and only
// when every address that carries it is on a group that exists.
func TestAssignmentTypes_FHRPByElimination(t *testing.T) {
	world := func() *fakeService {
		f := assignFake()
		f.pathOrigin(idInterface, 5)
		f.vmPrimary(idVMInterface, 7, 200)
		f.fhrpAddress(idFHRPGroup, 300, 2)
		f.fhrpAddress(idFHRPGroup, 301, 3)
		return f
	}
	all := contentTypes{idInterface: "dcim.interface", idVMInterface: "virtualization.vminterface", idFHRPGroup: "ipam.fhrpgroup"}

	t.Run("the one left", func(t *testing.T) {
		f := world()
		wantTypes(t, learn(t, newTestProvider(t, f), idInterface, idVMInterface, idFHRPGroup), all)
		// Every address carrying the id was read, by that id alone, and read again
		// once the tables were (see EliminationReadsInOrder).
		sent := f.sentWith("ipam/ip-addresses", "filter[assigned_object_type_id]__eq")
		if len(sent) != 2 || sent[0].query.Get("filter[assigned_object_type_id]__eq") != "86" || sent[1].query.Get("filter[assigned_object_type_id]__eq") != "86" {
			t.Errorf("addresses read by type: %v", sent)
		}
	})
	t.Run("two left", func(t *testing.T) {
		f := world()
		f.fhrpAddress(87, 302, 4)
		wantTypes(t, learn(t, newTestProvider(t, f), idInterface, idVMInterface, idFHRPGroup, 87),
			contentTypes{idInterface: "dcim.interface", idVMInterface: "virtualization.vminterface"})
	})
	// The rows contradict the elimination: the id stays unknown, and the
	// contradiction is logged.
	for _, c := range []struct {
		name  string
		setup func(f *fakeService)
	}{
		{"an address on something that is no group", func(f *fakeService) { f.addRows("ipam/ip-addresses", addrRow(303, idFHRPGroup, 99, 0)) }},
		{"an unassigned address carrying it", func(f *fakeService) { f.addRows("ipam/ip-addresses", addrRow(303, idFHRPGroup, 0, 0)) }},
		{"groups not replicated", func(f *fakeService) { f.setIngested("ipam/fhrp-groups", false) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLogs(t)
			f := world()
			c.setup(f)
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface, idVMInterface, idFHRPGroup),
				contentTypes{idInterface: "dcim.interface", idVMInterface: "virtualization.vminterface"})
			if got := logs.warnings(); len(got) != 1 || !strings.Contains(got[0], "86") {
				t.Errorf("logged %q, want one line naming id 86", got)
			}
		})
	}
	t.Run("no device interfaces at all", func(t *testing.T) {
		f := assignFake()
		f.vmPrimary(idVMInterface, 7, 200)
		f.fhrpAddress(idFHRPGroup, 300, 2)
		f.setIngested("dcim/interfaces", false)
		wantTypes(t, learn(t, newTestProvider(t, f), idVMInterface, idFHRPGroup),
			contentTypes{idVMInterface: "virtualization.vminterface", idFHRPGroup: "ipam.fhrpgroup"})
	})
}

// An id the rows name as some model, even as two, is not the one left over:
// it cannot be the group's.
func TestAssignmentTypes_DisputedIDIsNotTheGroups(t *testing.T) {
	logs := captureLogs(t)
	f := assignFake()
	f.pathOrigin(12, 5)
	f.addRows("dcim/front-ports", cabledAt(8, 9, "B"))
	f.addRows("dcim/rear-ports", cabledAt(8, 10, "A"))
	f.addRows("dcim/cable-terminations", termRow(1, 9, "B", idFHRPGroup, 8), termRow(2, 10, "A", idFHRPGroup, 8))
	f.fhrpAddress(idFHRPGroup, 300, 2)
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, 12, idFHRPGroup), contentTypes{12: "dcim.interface"})
	if got := logs.warnings(); len(got) != 1 || !strings.Contains(got[0], "86") {
		t.Errorf("logged %q, want one line naming id 86", got)
	}
}

// The check on the addresses that carry the id is of every one of them: the
// first ten thousand say nothing of the rest.
func TestAssignmentTypes_FHRPCheckIsOfEveryAddress(t *testing.T) {
	for _, c := range []struct {
		name      string
		addresses int
		want      contentTypes
	}{
		{"all of them", MaxLimit, contentTypes{idInterface: "dcim.interface", idFHRPGroup: "ipam.fhrpgroup"}},
		{"more than can be read", MaxLimit + 1, contentTypes{idInterface: "dcim.interface"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.addRows("ipam/fhrp-groups", idRow(2))
			rows := make([]map[string]interface{}, c.addresses)
			for i := range rows {
				rows[i] = addrRow(1000+i, idFHRPGroup, 2, 0)
			}
			f.addRows("ipam/ip-addresses", rows...)
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface, idFHRPGroup), c.want)
		})
	}
}

// A table with no rows rules a type out only when the replica has loaded all
// it will: while it loads the rows may not have arrived.
func TestAssignmentTypes_NothingIsRuledOutWhileTheSnapshotLoads(t *testing.T) {
	for _, c := range []struct {
		name     string
		complete bool
		want     contentTypes
	}{
		{"loaded", true, contentTypes{idInterface: "dcim.interface", idFHRPGroup: "ipam.fhrpgroup"}},
		{"loading", false, contentTypes{idInterface: "dcim.interface"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.fhrpAddress(idFHRPGroup, 300, 2)
			f.setIngested("virtualization/interfaces", false)
			f.mu.Lock()
			f.schema.SnapshotComplete = c.complete
			f.mu.Unlock()
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface, idFHRPGroup), c.want)
		})
	}
}

// staleUnfed makes the cached catalogue say what it said before the tables
// were fed: no data.
func staleUnfed(p *Provider, tables ...string) {
	p.catMu.Lock()
	defer p.catMu.Unlock()
	for _, k := range tables {
		e := p.cat.Entities[k]
		e.Ingested = false
		p.cat.Entities[k] = e
	}
}

// notNamed fails a test that learned a wrong model for id: no name is a fair
// answer, the right one too.
func notNamed(t *testing.T, got contentTypes, id int, right string) {
	t.Helper()
	if name, ok := got[id]; ok && name != right {
		t.Errorf("id %d learned as %s, want %s or nothing", id, name, right)
	}
}

var vmTables = []string{"virtualization/interfaces", "virtualization/virtual-machines"}

// The catalogue is cached for ten minutes, and what it says of a table is only
// as fresh: a tenant's first VM is fed, and its address assigned to the VM
// interface, inside that time. Ids are small sequences in every table (the
// demo's VM interfaces 1 and 2, its FHRP groups 1 and 2), so an address on a
// group that exists proves nothing: that a table has no rows is the row
// route's to say, not the catalogue's.
func TestAssignmentTypes_StaleCatalogueRulesNothingOut(t *testing.T) {
	world := func() *fakeService {
		f := assignFake()
		f.pathOrigin(idInterface, 5)
		f.fhrpAddress(idFHRPGroup, 13, 1)
		f.addRows("ipam/fhrp-groups", idRow(2))
		return f
	}
	t.Run("read before the first VM", func(t *testing.T) {
		f := world()
		p := newTestProvider(t, f)
		if _, err := p.catalogue(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		staleUnfed(p, vmTables...)
		f.vmPrimary(idVMInterface, 1, 11)
		got := learn(t, p, idInterface, idVMInterface)
		wantTypes(t, contentTypes{idInterface: got[idInterface]}, contentTypes{idInterface: "dcim.interface"})
		notNamed(t, got, idVMInterface, "virtualization.vminterface")
	})
	t.Run("VM interfaces that are not the addresses' objects", func(t *testing.T) {
		f := world()
		p := newTestProvider(t, f)
		if _, err := p.catalogue(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		staleUnfed(p, vmTables...)
		f.addRows("virtualization/interfaces", vmIfaceRow(7, 1))
		f.addRows("ipam/ip-addresses", addrRow(11, idVMInterface, 2, 0))
		notNamed(t, learn(t, p, idInterface, idVMInterface), idVMInterface, "virtualization.vminterface")
	})
	t.Run("asked alone, once the interface type is known", func(t *testing.T) {
		f := world()
		p := newTestProvider(t, f)
		if _, err := p.catalogue(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		staleUnfed(p, vmTables...)
		f.vmPrimary(idVMInterface, 1, 11)
		learn(t, p, idInterface)
		notNamed(t, learn(t, p, idVMInterface), idVMInterface, "virtualization.vminterface")
	})
}

// That a table has no rows is the row route's to say, not the catalogue's, even
// a catalogue read a moment ago: the VM interfaces are fed after it was read.
func TestAssignmentTypes_TheRowRouteSaysWhetherATableHasRows(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.fhrpAddress(idVMInterface, 300, 2)
	f.setIngested("virtualization/interfaces", false)
	var once sync.Once
	f.afterRequest = func(entity string, q url.Values) {
		if entity != "ipam/ip-addresses" || !q.Has("filter[assigned_object_type_id]__eq") {
			return
		}
		once.Do(func() {
			f.addEntity("virtualization/interfaces", "id:BIGINT:pk", "virtual_machine_id:BIGINT")
			f.addRows("virtualization/interfaces", vmIfaceRow(5, 1))
		})
	}
	p := newTestProvider(t, f)

	notNamed(t, learn(t, p, idInterface, idVMInterface), idVMInterface, "virtualization.vminterface")
}

// The replica starts reloading inside the catalogue's time to live: the cached
// catalogue still says complete, and the tables that are not back yet answer
// 404. A table with no rows is not out of the running then.
func TestAssignmentTypes_ReloadInsideTheCatalogueTTLRulesNothingOut(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.fhrpAddress(idFHRPGroup, 13, 1)
	f.vmPrimary(idVMInterface, 1, 11)
	p := newTestProvider(t, f)
	if _, err := p.catalogue(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for _, table := range vmTables {
		f.setIngested(table, false)
	}
	f.mu.Lock()
	f.schema.SnapshotComplete = false
	f.mu.Unlock()

	notNamed(t, learn(t, p, idInterface, idVMInterface), idVMInterface, "virtualization.vminterface")
}

// A reload that begins while the tables are being read leaves them as
// untrustworthy as one that began before: the catalogue is looked at again
// when the reads are done.
func TestAssignmentTypes_ReloadDuringTheReadsRulesNothingOut(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.fhrpAddress(idFHRPGroup, 300, 2)
	f.setIngested("virtualization/interfaces", false)
	f.afterRequest = func(entity string, q url.Values) {
		if entity == "ipam/fhrp-groups" {
			f.mu.Lock()
			f.schema.SnapshotComplete = false
			f.mu.Unlock()
		}
	}
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
}

// What was undecided only because the replica was loading is asked again
// when it has loaded, not left unknown for the catalogue's whole time to live
// — and not on every query meanwhile.
func TestAssignmentTypes_LoadingIsNotCachedAsUnknown(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.fhrpAddress(idFHRPGroup, 300, 2)
	f.setIngested("virtualization/interfaces", false)
	p := newTestProvider(t, f)
	if _, err := p.catalogue(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.schema.SnapshotComplete = false
	f.mu.Unlock()

	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
	f.forget()
	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
	if n := f.requestCount(); n != 0 {
		t.Errorf("a query during the load read %d times again", n)
	}

	// The load finishes, and the time to ask again comes.
	f.mu.Lock()
	f.schema.SnapshotComplete = true
	f.mu.Unlock()
	p.typeMu.Lock()
	for id := range p.types.deferred {
		p.types.deferred[id] = time.Now().Add(-time.Second)
	}
	p.typeMu.Unlock()
	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface", idFHRPGroup: "ipam.fhrpgroup"})
}

// The tables are read after the addresses that carry the id, never before: a
// VM interface and the address assigned to it appear together, and an absence
// read first and an address read second would see the address and not the
// interface. This pins what comes of it; EliminationReadsInOrder pins the order.
func TestAssignmentTypes_AbsenceIsReadAfterTheAddresses(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.addRows("ipam/fhrp-groups", idRow(1))
	var once sync.Once
	f.afterRequest = func(entity string, q url.Values) {
		if entity != "virtualization/interfaces" {
			return
		}
		once.Do(func() {
			f.addRows("virtualization/interfaces", vmIfaceRow(1, 1))
			f.addRows("ipam/ip-addresses", addrRow(11, idVMInterface, 1, 0))
		})
	}
	p := newTestProvider(t, f)

	notNamed(t, learn(t, p, idInterface, idVMInterface), idVMInterface, "virtualization.vminterface")
}

// eliminationTrace is the reads of an elimination, by what each is for, in the
// order they were sent.
func eliminationTrace(f *fakeService) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		switch {
		case r.entity == "_meta/schema":
			out = append(out, "catalogue")
		case r.entity == "ipam/ip-addresses" && r.query.Has("filter[assigned_object_type_id]__eq"):
			out = append(out, "addresses carrying the id")
		case r.entity == "virtualization/interfaces" && r.query.Has("filter[id]__in"):
			out = append(out, "VM interfaces by id")
		case r.entity == "virtualization/interfaces":
			out = append(out, "VM interfaces, any")
		case r.entity == "ipam/fhrp-groups":
			out = append(out, "groups")
		}
	}
	return out
}

// The elimination's reads, in the order that makes them mean something: the
// catalogue, the addresses carrying the id, then what a type's having rows
// and the objects' being its rows, then the groups, then the addresses again
// (what is the same in both was there throughout), then the catalogue again.
func TestAssignmentTypes_EliminationReadsInOrder(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.fhrpAddress(idFHRPGroup, 300, 2)
	p := newTestProvider(t, f)
	if _, err := p.catalogue(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.forget()

	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface", idFHRPGroup: "ipam.fhrpgroup"})
	want := []string{"catalogue", "addresses carrying the id", "VM interfaces, any", "VM interfaces by id", "groups", "addresses carrying the id", "catalogue"}
	if got := eliminationTrace(f); !slices.Equal(got, want) {
		t.Errorf("read in this order:\n  %q\nwant\n  %q", got, want)
	}
}

// The last VM is deleted after the addresses carrying its type were read and
// before the VM interface table is: NetBox deletes its interfaces and their
// addresses with it, so the table reads empty. The deleted interface's id is an
// FHRP group's (the demo's VM interfaces 1 and 2 are its groups 1 and 2), so
// without a second look the VM interface type would be named the group's, and
// kept. An address that is in both reads proves its interface was there
// throughout: deletion cascades and ids are not reused.
func TestAssignmentTypes_DeletionDuringTheReadsRulesNothingOut(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.addRows("virtualization/virtual-machines", ownerRow(1, 0, 0))
	f.addRows("virtualization/interfaces", vmIfaceRow(1, 1))
	f.addRows("ipam/ip-addresses", addrRow(13, idVMInterface, 1, 0))
	f.fhrpAddress(idFHRPGroup, 300, 1)
	var once sync.Once
	f.afterRequest = func(entity string, q url.Values) {
		if entity == "ipam/ip-addresses" && q.Get("filter[assigned_object_type_id]__eq") == "11" {
			once.Do(func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.entities["virtualization/virtual-machines"] = []map[string]interface{}{}
				f.entities["virtualization/interfaces"] = []map[string]interface{}{}
				f.entities["ipam/ip-addresses"] = slices.DeleteFunc(f.entities["ipam/ip-addresses"], func(r map[string]interface{}) bool { return r["id"] == float64(13) })
			})
		}
	}
	p := newTestProvider(t, f)

	notNamed(t, learn(t, p, idInterface, idVMInterface), idVMInterface, "virtualization.vminterface")
	// Undecided, not settled: asked about again soon, not unknown for the TTL.
	p.typeMu.Lock()
	defer p.typeMu.Unlock()
	if p.types.asked[idVMInterface] || p.types.deferred[idVMInterface].IsZero() {
		t.Errorf("the id was settled (asked %v, deferred until %v), want undecided", p.types.asked[idVMInterface], p.types.deferred[idVMInterface])
	}
}

// Any difference between the two reads of the addresses leaves the id
// undecided: one that appears is as much a sign the replica is changing as one
// that goes.
func TestAssignmentTypes_AddressesThatChangeDuringTheReadsRuleNothingOut(t *testing.T) {
	for name, change := range map[string]func(f *fakeService){
		"one appears": func(f *fakeService) { f.addRows("ipam/ip-addresses", addrRow(301, idFHRPGroup, 2, 0)) },
		"one is reassigned": func(f *fakeService) {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, r := range f.entities["ipam/ip-addresses"] {
				if r["id"] == float64(300) {
					r["assigned_object_id"] = float64(3)
				}
			}
			f.entities["ipam/fhrp-groups"] = append(f.entities["ipam/fhrp-groups"], idRow(3))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.fhrpAddress(idFHRPGroup, 300, 2)
			var once sync.Once
			f.afterRequest = func(entity string, q url.Values) {
				if entity == "ipam/ip-addresses" && q.Has("filter[assigned_object_type_id]__eq") {
					once.Do(func() { change(f) })
				}
			}
			p := newTestProvider(t, f)

			wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
			p.typeMu.Lock()
			defer p.typeMu.Unlock()
			if p.types.asked[idFHRPGroup] || p.types.deferred[idFHRPGroup].IsZero() {
				t.Errorf("the id was settled, want undecided")
			}
		})
	}
}

// More addresses than can be read the second time are a difference too, whatever
// the first of them say: the first ten thousand are the same.
func TestAssignmentTypes_AddressesPastTheCapOnTheSecondReadRuleNothingOut(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.addRows("ipam/fhrp-groups", idRow(2))
	rows := make([]map[string]interface{}, MaxLimit)
	for i := range rows {
		rows[i] = addrRow(1000+i, idFHRPGroup, 2, 0)
	}
	f.addRows("ipam/ip-addresses", rows...)
	// The first read is MaxLimit rows in pages of 1000; one more turns up after it.
	var pages atomic.Int32
	f.afterRequest = func(entity string, q url.Values) {
		if entity == "ipam/ip-addresses" && q.Has("filter[assigned_object_type_id]__eq") && pages.Add(1) == MaxLimit/pageSize {
			f.addRows("ipam/ip-addresses", addrRow(1000+MaxLimit, idFHRPGroup, 2, 0))
		}
	}
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
}

// The groups are matched to the objects by id: a route that hands one group
// back twice, and another not at all, has not shown both exist.
func TestAssignmentTypes_GroupsAreMatchedByID(t *testing.T) {
	logs := captureLogs(t)
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.addRows("ipam/fhrp-groups", idRow(1), idRow(1))
	f.addRows("ipam/ip-addresses", addrRow(300, idFHRPGroup, 1, 0), addrRow(301, idFHRPGroup, 2, 0))
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
	if got := logs.warnings(); len(got) != 1 || !strings.Contains(got[0], "86") {
		t.Errorf("logged %q, want one line naming id 86", got)
	}
}

// Objects the addresses are assigned to that are rows of a type not yet ruled
// out leave the id unknown: it may be that type's. A second look, by the
// objects' ids, after the table was found empty.
func TestAssignmentTypes_ObjectsOfTheAddressesAreNotThoseModels(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.fhrpAddress(idVMInterface, 300, 2)
	var once sync.Once
	f.afterRequest = func(entity string, q url.Values) {
		if entity != "virtualization/interfaces" || q.Has("filter[id]__in") {
			return
		}
		once.Do(func() { f.addRows("virtualization/interfaces", vmIfaceRow(2, 1)) })
	}
	p := newTestProvider(t, f)

	notNamed(t, learn(t, p, idInterface, idVMInterface), idVMInterface, "virtualization.vminterface")
	if _, ok := f.requestWith("virtualization/interfaces", "filter[id]__in"); !ok {
		t.Error("the objects the addresses are assigned to were not looked for among the VM interfaces")
	}
}

// A table the catalogue does not list has no evidence in it and cannot be
// ruled out: neither is a failure.
func TestAssignmentTypes_AbsentEntityIsNoEvidenceAndNoElimination(t *testing.T) {
	drop := func(f *fakeService, tables ...string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, table := range tables {
			delete(f.schema.Entities, "/v1/"+table)
		}
	}
	for _, table := range []string{"virtualization/virtual-machines", "virtualization/interfaces", "dcim/devices", "dcim/front-ports",
		"dcim/rear-ports", "dcim/cable-terminations", "dcim/cable-paths", "ipam/ip-addresses"} {
		t.Run(table+" evidence", func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.devicePrimary(idInterface, 6, 100)
			f.vmPrimary(idVMInterface, 7, 200)
			f.addRows("dcim/interfaces", cabledAt(8, 9, "A"))
			f.addRows("dcim/front-ports", cabledAt(8, 10, "B"))
			f.addRows("dcim/rear-ports", cabledAt(8, 11, "B"))
			f.addRows("dcim/cable-terminations", termRow(1, 9, "A", idInterface, 8), termRow(2, 10, "B", idFrontPort, 8), termRow(3, 11, "B", idRearPort, 8))
			drop(f, table)
			got := learn(t, newTestProvider(t, f), idInterface, idVMInterface)
			wantTypes(t, contentTypes{idInterface: got[idInterface]}, contentTypes{idInterface: "dcim.interface"})
		})
	}
	for _, table := range []string{"virtualization/interfaces", "ipam/fhrp-groups", "ipam/ip-addresses"} {
		t.Run(table+" elimination", func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.fhrpAddress(idFHRPGroup, 300, 2)
			drop(f, table)
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
		})
	}
}

// A contradiction is logged once however many learns follow it.
func TestAssignmentTypes_FHRPContradictionIsLoggedOnce(t *testing.T) {
	logs := captureLogs(t)
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	f.addRows("ipam/fhrp-groups", idRow(1))
	f.addRows("ipam/ip-addresses", addrRow(300, idFHRPGroup, 99, 0))
	p := newTestProvider(t, f)

	wantTypes(t, learn(t, p, idInterface, idFHRPGroup), contentTypes{idInterface: "dcim.interface"})
	// A VM turns up, and the learn that finds its type leaves the same id left.
	f.vmPrimary(idVMInterface, 7, 200)
	wantTypes(t, learn(t, p, idInterface, idVMInterface, idFHRPGroup),
		contentTypes{idInterface: "dcim.interface", idVMInterface: "virtualization.vminterface"})
	if got := logs.warnings(); len(got) != 1 {
		t.Errorf("logged %d lines: %q", len(got), got)
	}
}

// What the elimination reads fails it, as every read does.
func TestAssignmentTypes_FailedEliminationReadIsAnError(t *testing.T) {
	for _, table := range []string{"ipam/ip-addresses", "virtualization/interfaces", "ipam/fhrp-groups"} {
		t.Run(table, func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.fhrpAddress(idFHRPGroup, 300, 2)
			f.failing = map[string]int{table: 503}
			var st readStats
			_, err := newTestProvider(t, f).assignmentTypes(context.Background(), []int{idInterface, idFHRPGroup}, &st)
			if u := provider.Classify(err); u == nil || u.Status != 503 {
				t.Errorf("err = %v, want the replica's 503", err)
			}
		})
	}
}

// With nothing to go on an id stays unknown, however few remain: a guess
// names the wrong owner on a dashboard with nothing to show for it.
func TestAssignmentTypes_UnknownWithoutEvidence(t *testing.T) {
	// Interfaces and VM interfaces exist; no path, no cable, no primary.
	silent := func() *fakeService {
		f := assignFake()
		f.addRows("dcim/interfaces", ifaceRow(5, 1, 0))
		f.addRows("virtualization/interfaces", vmIfaceRow(7, 1))
		f.addRows("ipam/ip-addresses", addrRow(100, idInterface, 5, 0), addrRow(200, idVMInterface, 7, 0))
		return f
	}
	t.Run("rows that say nothing", func(t *testing.T) {
		wantTypes(t, learn(t, newTestProvider(t, silent()), idInterface, idVMInterface), contentTypes{})
	})
	t.Run("the same rows with one path", func(t *testing.T) {
		f := silent()
		f.addRows("dcim/interfaces", ifaceRow(6, 1, 506))
		f.addRows("dcim/cable-paths", pathRow(506, "9:6", "53:1", "9:7"))
		wantTypes(t, learn(t, newTestProvider(t, f), idInterface, idVMInterface), contentTypes{idInterface: "dcim.interface"})
	})
	t.Run("nothing replicated", func(t *testing.T) {
		f := assignFake()
		for _, table := range []string{"dcim/devices", "dcim/interfaces", "dcim/front-ports", "dcim/rear-ports", "dcim/cable-terminations",
			"dcim/cable-paths", "virtualization/virtual-machines", "virtualization/interfaces", "ipam/ip-addresses", "ipam/fhrp-groups"} {
			f.setIngested(table, false)
		}
		p := newTestProvider(t, f)
		var st readStats
		got, err := p.assignmentTypes(context.Background(), []int{idInterface, idVMInterface}, &st)
		if err != nil {
			t.Fatalf("tables with no data are no evidence, not a failure: %v", err)
		}
		wantTypes(t, got, contentTypes{})
		if st.unknown || st.asOf != nil {
			t.Errorf("noted an age (%v, unknown %v) with nothing read", st.asOf, st.unknown)
		}
	})
}

// Device.clean accepts a primary address that is the outside of a NAT whose
// inside is on one of the device's interfaces, so a primary address with a
// NAT inside is not known to be on the interface it names.
func TestAssignmentTypes_NATPrimaryIsNotEvidence(t *testing.T) {
	for _, c := range []struct {
		name      string
		natInside int
		want      contentTypes
	}{
		{"no NAT inside", 0, contentTypes{idInterface: "dcim.interface"}},
		{"a NAT inside", 101, contentTypes{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := assignFake()
			f.addRows("dcim/devices", ownerRow(1, 100, 0))
			f.addRows("dcim/interfaces", ifaceRow(5, 1, 0))
			f.addRows("ipam/ip-addresses", addrRow(100, idInterface, 5, c.natInside), addrRow(101, idInterface, 5, 0))
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface), c.want)
		})
	}
}

// A primary address with nothing it is assigned to says nothing about a type.
func TestAssignmentTypes_UnassignedPrimaryIsNotEvidence(t *testing.T) {
	for _, c := range []struct {
		name string
		row  map[string]interface{}
		want contentTypes
	}{
		{"assigned", addrRow(100, idInterface, 5, 0), contentTypes{idInterface: "dcim.interface"}},
		{"no type, no object", addrRow(100, 0, 0, 0), contentTypes{}},
		{"a type, no object", addrRow(100, idInterface, 0, 0), contentTypes{}},
		{"an object, no type id", addrRow(100, 0, 5, 0), contentTypes{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := assignFake()
			f.addRows("dcim/devices", ownerRow(1, 100, 0))
			f.addRows("dcim/interfaces", ifaceRow(5, 1, 0))
			f.addRows("ipam/ip-addresses", c.row)
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface), c.want)
		})
	}
}

// What the interface says is checked: a primary address on an interface that
// is another device's is not the validation Device.clean makes, and a path that
// starts at another object is not this interface's.
func TestAssignmentTypes_EvidenceMustNameTheOwner(t *testing.T) {
	for _, c := range []struct {
		name   string
		device int // the device the address's interface is on
		want   contentTypes
	}{
		{"primary on its own device's interface", 3, contentTypes{idInterface: "dcim.interface"}},
		{"primary on another device's interface", 8, contentTypes{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := assignFake()
			f.addRows("dcim/devices", ownerRow(3, 100, 0))
			f.addRows("dcim/interfaces", ifaceRow(5, c.device, 0))
			f.addRows("ipam/ip-addresses", addrRow(100, idInterface, 5, 0))
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface), c.want)
		})
	}
	for _, c := range []struct {
		name   string
		origin string
		want   contentTypes
	}{
		{"path of its own origin", "9:6", contentTypes{idInterface: "dcim.interface"}},
		{"path of another origin", "9:99", contentTypes{}},
		{"path that cannot be read", "9", contentTypes{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := assignFake()
			f.addRows("dcim/interfaces", ifaceRow(6, 1, 532))
			f.addRows("dcim/cable-paths", pathRow(532, c.origin, "53:1", "9:98"))
			wantTypes(t, learn(t, newTestProvider(t, f), idInterface), c.want)
		})
	}
}

// A cable termination names its object by type id, and the component tables
// say which kind a row is: the termination that is an interface's own (same
// id, cable and end) carries the interface type, a panel port's the port
// type. The end is what keeps two components of one id apart.
func TestAssignmentTypes_TerminationsClassifyPorts(t *testing.T) {
	f := assignFake()
	f.addRows("dcim/interfaces", cabledAt(60, 7, "A"), cabledAt(61, 8, "A"))
	f.addRows("dcim/front-ports", cabledAt(60, 7, "B"))
	f.addRows("dcim/rear-ports", cabledAt(9, 10, "A"))
	f.addRows("dcim/cable-terminations",
		termRow(1, 7, "A", idInterface, 60), termRow(2, 7, "B", idFrontPort, 60),
		termRow(3, 8, "A", idInterface, 61), termRow(4, 10, "A", idRearPort, 9),
		// A console port that happens to share an id with an interface on
		// another cable.
		termRow(5, 12, "A", 43, 60))
	p := newTestProvider(t, f)

	// What a caller is given are the models an address can be assigned to; the
	// ports are kept to check the one-id-per-model rule and what is left over.
	got := learn(t, p, idInterface, idFrontPort, idRearPort, 43)
	wantTypes(t, got, contentTypes{idInterface: "dcim.interface"})
	p.typeMu.Lock()
	kept := maps.Clone(p.types.learned)
	p.typeMu.Unlock()
	wantTypes(t, kept, contentTypes{idInterface: "dcim.interface", idFrontPort: "dcim.frontport", idRearPort: "dcim.rearport"})
}

// Every piece of evidence for an id must agree, and a model has one id: a
// disagreement leaves every id it touches unknown, and is logged once.
func TestAssignmentTypes_ConflictingEvidenceLeavesTheIDUnknownAndLogsOnce(t *testing.T) {
	t.Run("two models for one id", func(t *testing.T) {
		logs := captureLogs(t)
		f := assignFake()
		f.pathOrigin(idInterface, 5)
		// A front port whose termination says it is of the interface's type.
		f.addRows("dcim/front-ports", cabledAt(8, 9, "B"))
		f.addRows("dcim/cable-terminations", termRow(1, 9, "B", idInterface, 8))
		f.vmPrimary(idVMInterface, 7, 200)
		p := newTestProvider(t, f)

		wantTypes(t, learn(t, p, idInterface, idVMInterface), contentTypes{idVMInterface: "virtualization.vminterface"})
		learn(t, p, idInterface, idVMInterface)
		// Another id learned later reads the evidence for the interface again, and
		// finds the same disagreement.
		wantTypes(t, learn(t, p, idInterface, idVMInterface, 77), contentTypes{idVMInterface: "virtualization.vminterface"})
		if got := logs.warnings(); len(got) != 1 || !strings.Contains(got[0], "9") {
			t.Errorf("logged %q, want one line naming id 9", got)
		}
	})
	t.Run("two ids for one model", func(t *testing.T) {
		logs := captureLogs(t)
		f := assignFake()
		f.pathOrigin(idInterface, 5)
		f.devicePrimary(12, 6, 100)
		p := newTestProvider(t, f)

		wantTypes(t, learn(t, p, idInterface, 12), contentTypes{})
		if got := logs.warnings(); len(got) != 1 {
			t.Errorf("logged %q, want one line", got)
		}
	})
	t.Run("agreeing evidence is quiet", func(t *testing.T) {
		logs := captureLogs(t)
		f := assignFake()
		f.pathOrigin(idInterface, 5)
		f.devicePrimary(idInterface, 6, 100)
		f.addRows("dcim/interfaces", cabledAt(8, 9, "A"))
		f.addRows("dcim/cable-terminations", termRow(1, 9, "A", idInterface, 8))
		wantTypes(t, learn(t, newTestProvider(t, f), idInterface), contentTypes{idInterface: "dcim.interface"})
		if got := logs.warnings(); len(got) != 0 {
			t.Errorf("logged %q", got)
		}
	})
}

// A type id once learned is not read for again until the cache expires; an
// id not yet asked about is learned without reading the evidence for a type
// that is already known.
func TestAssignmentTypes_CachedAndLearnsOnlyNewIDs(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	p := newTestProvider(t, f)

	var st readStats
	got, err := p.assignmentTypes(context.Background(), []int{idInterface}, &st)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, got, contentTypes{idInterface: "dcim.interface"})
	if st.dataAsOf() == nil {
		t.Error("a learn that read tables noted no age")
	}

	// The same ids again: no request of any kind, and no age to note.
	f.forget()
	var again readStats
	got, err = p.assignmentTypes(context.Background(), []int{idInterface}, &again)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes(t, got, contentTypes{idInterface: "dcim.interface"})
	if n := f.requestCount(); n != 0 {
		t.Errorf("a cached answer sent %d requests", n)
	}
	if again.asOf != nil || again.unknown {
		t.Error("a cached answer noted an age")
	}

	// A VM appears, and an address carries a type not asked about before: the
	// new id is learned, and the interface type is not read for again.
	f.vmPrimary(idVMInterface, 7, 200)
	got = learn(t, p, idInterface, idVMInterface)
	wantTypes(t, got, contentTypes{idInterface: "dcim.interface", idVMInterface: "virtualization.vminterface"})
	for _, entity := range []string{"dcim/cable-paths", "dcim/devices"} {
		if n := f.countRequestsFor(entity); n != 0 {
			t.Errorf("%d requests to %s to learn an id that is not an interface's", n, entity)
		}
	}
	if f.countRequestsFor("virtualization/virtual-machines") == 0 {
		t.Error("the new id was learned without reading what it could be learned from")
	}

	// An id that no evidence names is asked about once, not on every query.
	f.forget()
	wantTypes(t, learn(t, p, idInterface, idVMInterface, 77), contentTypes{idInterface: "dcim.interface", idVMInterface: "virtualization.vminterface"})
	sent := f.requestCount()
	f.forget()
	learn(t, p, idInterface, idVMInterface, 77)
	if n := f.requestCount(); n != 0 {
		t.Errorf("an id that stayed unknown was read for again (%d requests; the first ask took %d)", n, sent)
	}
}

// What is learned is kept for the catalogue's time to live, no longer: a
// replica rebuilt from another NetBox assigns its ids anew.
func TestAssignmentTypes_ExpiresWithTheCatalogue(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	p := newTestProvider(t, f)
	learn(t, p, idInterface)

	f.forget()
	learn(t, p, idInterface)
	if f.requestCount() != 0 {
		t.Fatal("not cached")
	}
	p.typeMu.Lock()
	p.types.expires = time.Now().Add(-time.Second)
	p.typeMu.Unlock()
	wantTypes(t, learn(t, p, idInterface), contentTypes{idInterface: "dcim.interface"})
	if f.countRequestsFor("dcim/cable-paths") == 0 {
		t.Error("an expired answer was served without reading again")
	}
}

// A read that fails fails the learn, as 5xx fails every other read, and
// nothing of it is cached: the next query reads again.
func TestAssignmentTypes_FailedReadIsAnErrorAndNotCached(t *testing.T) {
	for _, table := range []string{"dcim/interfaces", "dcim/cable-paths", "dcim/cable-terminations", "dcim/front-ports",
		"dcim/devices", "ipam/ip-addresses", "virtualization/virtual-machines"} {
		t.Run(table, func(t *testing.T) {
			f := assignFake()
			f.pathOrigin(idInterface, 5)
			f.devicePrimary(idInterface, 6, 100)
			f.addRows("dcim/front-ports", cabledAt(8, 9, "B"))
			f.addRows("dcim/cable-terminations", termRow(1, 9, "B", idFrontPort, 8))
			f.failing = map[string]int{table: 503}
			p := newTestProvider(t, f)

			var st readStats
			_, err := p.assignmentTypes(context.Background(), []int{idInterface}, &st)
			if u := provider.Classify(err); u == nil || u.Status != 503 {
				t.Fatalf("err = %v, want the replica's 503", err)
			}
			f.mu.Lock()
			f.failing = nil
			f.mu.Unlock()
			wantTypes(t, learn(t, p, idInterface), contentTypes{idInterface: "dcim.interface"})
		})
	}
}

// Twenty panels refreshing together learn once.
func TestAssignmentTypes_ConcurrentCallersShareOneRead(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	p := newTestProvider(t, f)
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var st readStats
			got, err := p.assignmentTypes(context.Background(), []int{idInterface}, &st)
			if err != nil {
				t.Error(err)
				return
			}
			if !maps.Equal(got, contentTypes{idInterface: "dcim.interface"}) {
				t.Errorf("learned %v", got)
			}
		}()
	}
	waitForRequest(t, f, "_meta/schema")
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := f.countRequestsFor("dcim/cable-paths"); n != 1 {
		t.Errorf("the paths were read %d times by 20 callers, want once", n)
	}
}

// A caller that gives up does not take the learn down with it.
func TestAssignmentTypes_CancelledCallerDoesNotPoisonJoiners(t *testing.T) {
	f := assignFake()
	f.pathOrigin(idInterface, 5)
	gate := make(chan struct{})
	f.gate = gate
	p := newTestProvider(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { var st readStats; _, err := p.assignmentTypes(ctx, []int{idInterface}, &st); leader <- err }()
	waitForRequest(t, f, "_meta/schema")
	joiner := make(chan error, 1)
	go func() {
		var st readStats
		_, err := p.assignmentTypes(context.Background(), []int{idInterface}, &st)
		joiner <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-leader; err == nil {
		t.Error("the cancelled caller itself is told so")
	}
	close(gate)
	if err := <-joiner; err != nil {
		t.Errorf("a joiner with a live context got the leader's cancellation: %v", err)
	}
}

func TestAssignmentTypes_IgnoresNothingToAsk(t *testing.T) {
	f := assignFake()
	p := newTestProvider(t, f)
	for _, ids := range [][]int{nil, {}, {0, -3}} {
		wantTypes(t, learn(t, p, ids...), contentTypes{})
	}
	if n := f.requestCount(); n != 0 {
		t.Errorf("%d requests to learn nothing", n)
	}
}

// waitForRequest parks the test until the fake has seen a request for entity,
// and fails it if none comes.
func waitForRequest(t *testing.T, f *fakeService, entity string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); f.countRequestsFor(entity) < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no request for %s", entity)
		}
	}
}

// recordedLogger keeps what the package logs at Warn.
type recordedLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordedLogger) Debug(string, ...interface{}) {}
func (l *recordedLogger) Info(string, ...interface{})  {}
func (l *recordedLogger) Error(string, ...interface{}) {}
func (l *recordedLogger) Warn(msg string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg+" "+fmt.Sprint(args...))
}
func (l *recordedLogger) With(...interface{}) log.Logger         { return l }
func (l *recordedLogger) Level() log.Level                       { return log.Debug }
func (l *recordedLogger) FromContext(context.Context) log.Logger { return l }
func (l *recordedLogger) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// captureLogs sends the SDK's default logger to a recorder for the test.
func captureLogs(t *testing.T) *recordedLogger {
	t.Helper()
	l := &recordedLogger{}
	prev := log.DefaultLogger
	log.DefaultLogger = l
	t.Cleanup(func() { log.DefaultLogger = prev })
	return l
}
