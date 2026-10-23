package replicacache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

// The topology is checked against NetBox mode itself: one model of a NetBox
// feeds both the fake replica (its tables, as replica-cache v1.40.1 serves
// them) and a mock of the NetBox API (the cables, interfaces and wireless
// links NetBox mode reads, as NetBox 4.4 serializes them), and the two
// providers' graphs are compared.

// ctKind is a content type by NetBox's label.
type ctKind string

const (
	kIface         ctKind = "dcim.interface"
	kFront         ctKind = "dcim.frontport"
	kRear          ctKind = "dcim.rearport"
	kConsole       ctKind = "dcim.consoleport"
	kConsoleServer ctKind = "dcim.consoleserverport"
	kCircuitTerm   ctKind = "circuits.circuittermination"
	kProviderNet   ctKind = "circuits.providernetwork"
	kCable         ctKind = "dcim.cable"
	kWirelessLink  ctKind = "wireless.wirelesslink"
)

// demoTypeIDs are the demo NetBox's content type ids (django_content_type).
func demoTypeIDs() map[ctKind]int {
	return map[ctKind]int{kIface: 9, kFront: 47, kRear: 48, kConsole: 43, kConsoleServer: 44,
		kCircuitTerm: 37, kProviderNet: 7, kCable: 53, kWirelessLink: 149}
}

type topoDevice struct {
	id                       int
	name, site, role, status string
}

// topoComp is something a cable ends on or a path passes: a device component,
// or a circuit termination (device 0). Cable terminations are numbered in the
// order components are listed, which is the order NetBox lists a cable end's
// terminations in.
type topoComp struct {
	k      ctKind
	id     int
	device int
	name   string
	cable  int
	end    string
	path   int // an interface's _path_id
}

type topoNode struct {
	k  ctKind
	id int
}

type topoPath struct {
	id               int
	steps            [][]topoNode
	complete, active bool
}

type topoLink struct{ id, a, b int } // a wireless link between two interfaces

type topoModel struct {
	devices  []topoDevice
	comps    []topoComp
	paths    []topoPath
	wireless []topoLink
	typeIDs  map[ctKind]int
}

func iface(id, device int, name string, cable int, end string, path int) topoComp {
	return topoComp{k: kIface, id: id, device: device, name: name, cable: cable, end: end, path: path}
}

func nd(k ctKind, id int) topoNode { return topoNode{k, id} }

func steps(nodes ...[]topoNode) [][]topoNode { return nodes }

func at(nodes ...topoNode) []topoNode { return nodes }

// reversed is the path NetBox writes for the far end: the same steps, the
// other way.
func reversed(s [][]topoNode) [][]topoNode {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

// demoModel mirrors the demo NetBox (NetBox 4.4.10, as loaded into the local
// replica): three spine/leaf/access sites, a run spliced through a patch
// panel, two access points on a wireless link, and the parity objects — a
// 1×2 breakout, a console cable, a path through a circuit, an incomplete path
// ending at a circuit termination, a planned (inactive) cable — plus a path
// ending at a provider network and a complete one ending at a circuit
// termination. Two names differ (PAR1-, LON1-): NetBox orders names
// naturally and case-insensitively, the replica bytewise, and this model
// keeps the two orders equal so the graphs can be compared edge for edge.
func demoModel() topoModel {
	m := topoModel{typeIDs: demoTypeIDs()}
	m.devices = []topoDevice{
		{1, "AMS1-spine-01", "AMS1", "Spine", "active"},
		{2, "AMS1-leaf-01", "AMS1", "Leaf", "active"},
		{3, "AMS1-leaf-02", "AMS1", "Leaf", "active"},
		{4, "AMS1-access-01", "AMS1", "Access", "active"},
		{5, "NYC1-spine-01", "NYC1", "Spine", "active"},
		{6, "NYC1-leaf-01", "NYC1", "Leaf", "active"},
		{7, "NYC1-leaf-02", "NYC1", "Leaf", "active"},
		{8, "NYC1-access-01", "NYC1", "Access", "offline"},
		{9, "SIN1-spine-01", "SIN1", "Spine", "active"},
		{10, "SIN1-leaf-01", "SIN1", "Leaf", "active"},
		{11, "SIN1-leaf-02", "SIN1", "Leaf", "active"},
		{12, "SIN1-access-01", "SIN1", "Access", "active"},
		{13, "AMS1-pp-01", "AMS1", "Patch Panel", "active"},
		{14, "AMS1-ap-01", "AMS1", "Access Point", "active"},
		{15, "AMS1-ap-02", "AMS1", "Access Point", "active"},
		{17, "PAR1-dev-a", "PAR1", "Parity", "active"},
		{18, "PAR1-dev-b", "PAR1", "Parity", "active"},
		{19, "LON1-dev-c", "LON1", "Parity", "planned"},
	}
	// Nine direct device-to-device cables, each with the pair of paths NetBox
	// writes for it.
	for i, c := range []struct{ aDev, aPort, bDev, bPort int }{
		{1, 1, 2, 1}, {1, 2, 3, 1}, {2, 2, 4, 1},
		{5, 1, 6, 1}, {5, 2, 7, 1}, {6, 2, 8, 1},
		{9, 1, 10, 1}, {9, 2, 11, 1}, {10, 2, 12, 1},
	} {
		cable, a, b := i+1, 2*i+1, 2*i+2
		m.comps = append(m.comps,
			iface(a, c.aDev, fmt.Sprintf("Ethernet%d", c.aPort), cable, "A", a),
			iface(b, c.bDev, fmt.Sprintf("Ethernet%d", c.bPort), cable, "B", b))
		s := steps(at(nd(kIface, a)), at(nd(kCable, cable)), at(nd(kIface, b)))
		m.paths = append(m.paths, topoPath{a, s, true, true}, topoPath{b, reversed(s), true, true})
	}
	m.comps = append(m.comps,
		// leaf-02 to access-01 through the panel's front and rear port.
		iface(19, 3, "Ethernet3", 10, "A", 21),
		topoComp{k: kFront, id: 1, device: 13, name: "FP1", cable: 10, end: "B"},
		topoComp{k: kRear, id: 1, device: 13, name: "RP1", cable: 11, end: "A"},
		iface(20, 4, "Ethernet3", 11, "B", 22),
		// The access points' radios: no cable, a wireless link.
		iface(21, 14, "wlan0", 0, "", 23),
		iface(22, 15, "wlan0", 0, "", 24),
		// A 1×2 breakout.
		iface(36, 17, "eth1", 12, "A", 25),
		iface(41, 18, "eth1", 12, "B", 26),
		iface(42, 18, "eth2", 12, "B", 26),
		// A console cable.
		topoComp{k: kConsole, id: 1, device: 17, name: "con0", cable: 13, end: "A"},
		topoComp{k: kConsoleServer, id: 1, device: 18, name: "con0", cable: 13, end: "B"},
		// A path through a circuit.
		iface(37, 17, "eth2", 14, "A", 32),
		topoComp{k: kCircuitTerm, id: 1, cable: 14, end: "B"},
		topoComp{k: kCircuitTerm, id: 2, cable: 15, end: "A"},
		iface(40, 18, "eth0", 15, "B", 33),
		// An incomplete path: the circuit's far side is not cabled.
		iface(38, 17, "eth3", 16, "A", 35),
		topoComp{k: kCircuitTerm, id: 3, cable: 16, end: "B"},
		// A planned cable: complete paths, not active.
		iface(35, 17, "eth0", 17, "A", 36),
		iface(43, 19, "eth0", 17, "B", 37),
		// A path ending at a provider network, and one ending at a circuit
		// termination marked connected: complete, and not to an interface.
		iface(44, 19, "eth1", 18, "A", 38),
		topoComp{k: kCircuitTerm, id: 4, cable: 18, end: "B"},
		iface(46, 11, "Ethernet3", 19, "A", 39),
		topoComp{k: kCircuitTerm, id: 5, cable: 19, end: "B"},
		// A link between sites, so PAR1's peers outside it are two, whose
		// names (LON1-, SIN1-) are not in their ids' order (19, 12).
		iface(47, 18, "eth3", 21, "A", 42),
		iface(48, 12, "Ethernet2", 21, "B", 43),
	)
	panel := steps(at(nd(kIface, 19)), at(nd(kCable, 10)), at(nd(kFront, 1)), at(nd(kRear, 1)), at(nd(kCable, 11)), at(nd(kIface, 20)))
	radio := steps(at(nd(kIface, 21)), at(nd(kWirelessLink, 1)), at(nd(kIface, 22)))
	breakout := steps(at(nd(kIface, 36)), at(nd(kCable, 12)), at(nd(kIface, 41), nd(kIface, 42)))
	console := steps(at(nd(kConsole, 1)), at(nd(kCable, 13)), at(nd(kConsoleServer, 1)))
	circuit := steps(at(nd(kIface, 37)), at(nd(kCable, 14)), at(nd(kCircuitTerm, 1)), at(nd(kCircuitTerm, 2)), at(nd(kCable, 15)), at(nd(kIface, 40)))
	planned := steps(at(nd(kIface, 35)), at(nd(kCable, 17)), at(nd(kIface, 43)))
	between := steps(at(nd(kIface, 47)), at(nd(kCable, 21)), at(nd(kIface, 48)))
	m.paths = append(m.paths,
		topoPath{21, panel, true, true}, topoPath{22, reversed(panel), true, true},
		topoPath{23, radio, true, true}, topoPath{24, reversed(radio), true, true},
		topoPath{25, breakout, true, true}, topoPath{26, reversed(breakout), true, true},
		topoPath{27, console, true, true}, topoPath{28, reversed(console), true, true},
		topoPath{32, circuit, true, true}, topoPath{33, reversed(circuit), true, true},
		topoPath{35, steps(at(nd(kIface, 38)), at(nd(kCable, 16)), at(nd(kCircuitTerm, 3))), false, true},
		topoPath{36, planned, true, false}, topoPath{37, reversed(planned), true, false},
		topoPath{38, steps(at(nd(kIface, 44)), at(nd(kCable, 18)), at(nd(kCircuitTerm, 4)), at(nd(kProviderNet, 1))), true, true},
		topoPath{39, steps(at(nd(kIface, 46)), at(nd(kCable, 19)), at(nd(kCircuitTerm, 5))), true, true},
		topoPath{42, between, true, true}, topoPath{43, reversed(between), true, true},
	)
	m.wireless = []topoLink{{1, 21, 22}}
	return m
}

func (m topoModel) comp(k ctKind, id int) (topoComp, bool) {
	for _, c := range m.comps {
		if c.k == k && c.id == id {
			return c, true
		}
	}
	return topoComp{}, false
}

func (m topoModel) path(id int) (topoPath, bool) {
	for _, p := range m.paths {
		if p.id == id {
			return p, true
		}
	}
	return topoPath{}, false
}

func (m topoModel) device(id int) (topoDevice, bool) {
	for _, d := range m.devices {
		if d.id == id {
			return d, true
		}
	}
	return topoDevice{}, false
}

// sitesAndRoles numbers the sites and roles in order of first use.
func (m topoModel) sitesAndRoles() (sites, roles []string) {
	for _, d := range m.devices {
		if !slices.Contains(sites, d.site) {
			sites = append(sites, d.site)
		}
		if !slices.Contains(roles, d.role) {
			roles = append(roles, d.role)
		}
	}
	return sites, roles
}

func slugOf(name string) string { return strings.ReplaceAll(strings.ToLower(name), " ", "-") }

func num(i int) interface{} {
	if i == 0 {
		return nil
	}
	return float64(i)
}

func text(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// pathText is a path as the replica serves it: PostgreSQL's jsonb text.
func (m topoModel) pathText(s [][]topoNode) string {
	parts := make([]string, len(s))
	for i, st := range s {
		nodes := make([]string, len(st))
		for j, n := range st {
			nodes[j] = strconv.Quote(fmt.Sprintf("%d:%d", m.typeIDs[n.k], n.id))
		}
		parts[i] = "[" + strings.Join(nodes, ", ") + "]"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// fake serves the model as replica-cache v1.40.1 does, mirroring a NetBox at
// https://nb.
func (m topoModel) fake() *fakeService {
	f := newFakeService()
	f.schema.NetBoxURL = "https://nb"
	f.addTopologyTables()
	sites, roles := m.sitesAndRoles()
	for i, s := range sites {
		f.entities["dcim/sites"] = append(f.entities["dcim/sites"], map[string]interface{}{"id": float64(i + 1), "name": s, "slug": slugOf(s)})
	}
	for i, r := range roles {
		f.entities["dcim/device-roles"] = append(f.entities["dcim/device-roles"], map[string]interface{}{"id": float64(i + 1), "name": r, "slug": slugOf(r)})
	}
	devices := slices.Clone(m.devices)
	slices.SortFunc(devices, func(a, b topoDevice) int { return a.id - b.id })
	f.entities["dcim/devices"] = []map[string]interface{}{}
	for _, d := range devices {
		f.entities["dcim/devices"] = append(f.entities["dcim/devices"], map[string]interface{}{
			"id": float64(d.id), "name": d.name, "status": d.status,
			"site_id": float64(slices.Index(sites, d.site) + 1), "role_id": float64(slices.Index(roles, d.role) + 1)})
	}
	tables := map[ctKind]string{kIface: "dcim/interfaces", kFront: "dcim/front-ports", kRear: "dcim/rear-ports"}
	for _, t := range tables {
		f.entities[t] = []map[string]interface{}{}
	}
	f.entities["dcim/cable-terminations"] = []map[string]interface{}{}
	term := 0
	for _, c := range m.comps {
		if c.cable > 0 {
			term++
			f.entities["dcim/cable-terminations"] = append(f.entities["dcim/cable-terminations"], map[string]interface{}{
				"id": float64(term), "cable_id": float64(c.cable), "cable_end": c.end,
				"termination_type_id": float64(m.typeIDs[c.k]), "termination_id": float64(c.id), "_device_id": num(c.device)})
		}
		table, ok := tables[c.k]
		if !ok {
			continue
		}
		row := map[string]interface{}{"id": float64(c.id), "name": c.name, "device_id": num(c.device),
			"cable_id": num(c.cable), "cable_end": text(c.end)}
		if c.k == kIface {
			row["_name"], row["_path_id"] = c.name, num(c.path)
		}
		f.entities[table] = append(f.entities[table], row)
	}
	for _, t := range tables {
		slices.SortFunc(f.entities[t], func(a, b map[string]interface{}) int { return int(a["id"].(float64) - b["id"].(float64)) })
	}
	f.entities["dcim/cable-paths"] = []map[string]interface{}{}
	for _, p := range m.paths {
		f.entities["dcim/cable-paths"] = append(f.entities["dcim/cable-paths"], map[string]interface{}{
			"id": float64(p.id), "path": m.pathText(p.steps), "is_complete": p.complete, "is_active": p.active})
	}
	f.entities["wireless/wireless-links"] = []map[string]interface{}{}
	for _, l := range m.wireless {
		a, _ := m.comp(kIface, l.a)
		b, _ := m.comp(kIface, l.b)
		f.entities["wireless/wireless-links"] = append(f.entities["wireless/wireless-links"], map[string]interface{}{
			"id": float64(l.id), "interface_a_id": float64(l.a), "interface_b_id": float64(l.b),
			"_interface_a_device_id": num(a.device), "_interface_b_device_id": num(b.device)})
	}
	return f
}

// netboxAPI serves the model as NetBox 4.4's REST API does, for what NetBox
// mode's topology reads: devices (filtered by id and site, ordered by name),
// cables and connected interfaces scoped by device_id, wireless links.
func (m topoModel) netboxAPI(t *testing.T) *netbox.Provider {
	t.Helper()
	sites, roles := m.sitesAndRoles()
	write := func(w http.ResponseWriter, r *http.Request, rows []interface{}) {
		total, q := len(rows), r.URL.Query()
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit <= 0 {
			limit = 1000
		}
		rows = rows[min(offset, len(rows)):min(offset+limit, len(rows))]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": total, "next": nil, "previous": nil, "results": rows})
	}
	inParam := func(r *http.Request, key string, id int) bool {
		vs := r.URL.Query()[key]
		return len(vs) == 0 || slices.Contains(vs, strconv.Itoa(id))
	}
	ordered := slices.Clone(m.devices)
	slices.SortFunc(ordered, func(a, b topoDevice) int { return strings.Compare(a.name, b.name) })
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		var rows []interface{}
		for _, d := range ordered {
			if !inParam(r, "id", d.id) {
				continue
			}
			if s := r.URL.Query()["site"]; len(s) > 0 && !slices.Contains(s, slugOf(d.site)) {
				continue
			}
			if n := r.URL.Query()["name"]; len(n) > 0 && !slices.Contains(n, d.name) {
				continue
			}
			rows = append(rows, map[string]interface{}{"id": d.id, "name": d.name,
				"site":        map[string]interface{}{"id": slices.Index(sites, d.site) + 1, "name": d.site, "slug": slugOf(d.site)},
				"role":        map[string]interface{}{"id": slices.Index(roles, d.role) + 1, "name": d.role, "slug": slugOf(d.role)},
				"status":      map[string]interface{}{"value": d.status, "label": strings.ToUpper(d.status[:1]) + d.status[1:]},
				"display_url": fmt.Sprintf("https://nb/dcim/devices/%d/", d.id)})
		}
		write(w, r, rows)
	})
	object := func(c topoComp) map[string]interface{} {
		o := map[string]interface{}{"id": c.id}
		if c.device > 0 {
			o["device"] = map[string]interface{}{"id": c.device}
		}
		return o
	}
	mux.HandleFunc("/api/dcim/cables/", func(w http.ResponseWriter, r *http.Request) {
		var ids []int
		for _, c := range m.comps {
			if c.cable > 0 && !slices.Contains(ids, c.cable) {
				ids = append(ids, c.cable)
			}
		}
		slices.Sort(ids)
		var rows []interface{}
		for _, id := range ids {
			a, b, scoped := []interface{}{}, []interface{}{}, false
			for _, c := range m.comps {
				if c.cable != id {
					continue
				}
				if c.device > 0 && inParam(r, "device_id", c.device) {
					scoped = true
				}
				term := map[string]interface{}{"object_type": string(c.k), "object_id": c.id, "object": object(c)}
				if c.end == "A" {
					a = append(a, term)
				} else {
					b = append(b, term)
				}
			}
			if scoped {
				rows = append(rows, map[string]interface{}{"id": id, "a_terminations": a, "b_terminations": b})
			}
		}
		write(w, r, rows)
	})
	mux.HandleFunc("/api/dcim/interfaces/", func(w http.ResponseWriter, r *http.Request) {
		var ifaces []topoComp
		for _, c := range m.comps {
			if c.k != kIface || !inParam(r, "device_id", c.device) {
				continue
			}
			p, ok := m.path(c.path)
			// connected=true is NetBox's _path__is_active=True.
			if r.URL.Query().Get("connected") == "true" && (!ok || !p.active) {
				continue
			}
			ifaces = append(ifaces, c)
		}
		name := func(c topoComp) string { d, _ := m.device(c.device); return d.name }
		slices.SortFunc(ifaces, func(a, b topoComp) int {
			if n := strings.Compare(name(a), name(b)); n != 0 {
				return n
			}
			return strings.Compare(a.name, b.name)
		})
		var rows []interface{}
		for _, c := range ifaces {
			p, _ := m.path(c.path)
			row := map[string]interface{}{"id": c.id, "name": c.name, "device": map[string]interface{}{"id": c.device},
				"connected_endpoints_type": nil, "connected_endpoints": nil,
				"connected_endpoints_reachable": p.complete && p.active}
			if p.complete {
				var eps []interface{}
				for _, n := range p.steps[len(p.steps)-1] {
					if comp, ok := m.comp(n.k, n.id); ok {
						eps = append(eps, object(comp))
					} else {
						eps = append(eps, map[string]interface{}{"id": n.id})
					}
				}
				row["connected_endpoints"], row["connected_endpoints_type"] = eps, string(p.steps[len(p.steps)-1][0].k)
			}
			rows = append(rows, row)
		}
		write(w, r, rows)
	})
	mux.HandleFunc("/api/wireless/wireless-links/", func(w http.ResponseWriter, r *http.Request) {
		var rows []interface{}
		for _, l := range m.wireless {
			a, _ := m.comp(kIface, l.a)
			b, _ := m.comp(kIface, l.b)
			rows = append(rows, map[string]interface{}{"id": l.id, "interface_a": object(a), "interface_b": object(b)})
		}
		write(w, r, rows)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return netbox.New(srv.URL, "test-token", srv.Client())
}

// graphShape is what both modes must agree on.
type graphShape struct {
	Nodes                   []provider.GraphNode
	Edges                   []provider.GraphEdge
	Total, Fetched, MaxRows int
	Warnings                []string
}

func shapeOf(g *provider.Graph) graphShape {
	s := graphShape{Nodes: g.Nodes, Edges: g.Edges, Total: g.Total, Fetched: g.Fetched, MaxRows: g.MaxRows, Warnings: g.Warnings}
	if s.Nodes == nil {
		s.Nodes = []provider.GraphNode{}
	}
	if s.Edges == nil {
		s.Edges = []provider.GraphEdge{}
	}
	if s.Warnings == nil {
		s.Warnings = []string{}
	}
	return s
}

// bothModes builds the model's graph in NetBox mode and in cache mode.
func bothModes(t *testing.T, m topoModel, spec provider.TopologySpec, prepare ...func(*fakeService, *Provider)) (nb, rc *provider.Graph) {
	t.Helper()
	nb, err := m.netboxAPI(t).Topology(context.Background(), spec)
	if err != nil {
		t.Fatalf("NetBox mode: %v", err)
	}
	f := m.fake()
	p := newTestProvider(t, f)
	for _, fn := range prepare {
		fn(f, p)
	}
	rc, err = p.Topology(context.Background(), spec)
	if err != nil {
		t.Fatalf("cache mode: %v", err)
	}
	return nb, rc
}

func sameGraph(t *testing.T, label string, nb, rc *provider.Graph) {
	t.Helper()
	want, got := shapeOf(nb), shapeOf(rc)
	if reflect.DeepEqual(want, got) {
		return
	}
	if !reflect.DeepEqual(want.Nodes, got.Nodes) {
		t.Errorf("%s: nodes differ\nNetBox mode: %+v\ncache mode:  %+v", label, want.Nodes, got.Nodes)
	}
	if !reflect.DeepEqual(want.Edges, got.Edges) {
		t.Errorf("%s: edges differ\nNetBox mode: %+v\ncache mode:  %+v", label, want.Edges, got.Edges)
	}
	if want.Total != got.Total || want.Fetched != got.Fetched || want.MaxRows != got.MaxRows {
		t.Errorf("%s: total/fetched/max %d/%d/%d in NetBox mode, %d/%d/%d in cache mode", label,
			want.Total, want.Fetched, want.MaxRows, got.Total, got.Fetched, got.MaxRows)
	}
	if !reflect.DeepEqual(want.Warnings, got.Warnings) {
		t.Errorf("%s: warnings differ\nNetBox mode: %q\ncache mode:  %q", label, want.Warnings, got.Warnings)
	}
}

func siteFilter(slug string) []provider.Filter {
	return []provider.Filter{{Field: "site", Value: slug}}
}

func edgeIDs(g *provider.Graph) []string {
	var out []string
	for _, e := range g.Edges {
		out = append(out, e.ID)
	}
	return out
}

func findEdge(g *provider.Graph, id string) (provider.GraphEdge, bool) {
	for _, e := range g.Edges {
		if e.ID == id {
			return e, true
		}
	}
	return provider.GraphEdge{}, false
}

func cacheTopology(t *testing.T, f *fakeService, spec provider.TopologySpec, opts ...Option) *provider.Graph {
	t.Helper()
	g, err := newTestProvider(t, f, opts...).Topology(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var topologySpecs = []struct {
	name string
	spec provider.TopologySpec
}{
	{"everything", provider.TopologySpec{}},
	{"connected only", provider.TopologySpec{ConnectedOnly: true}},
	{"one site", provider.TopologySpec{Filters: siteFilter("par1")}},
	{"one site with boundary peers", provider.TopologySpec{Filters: siteFilter("par1"), IncludeBoundaryPeers: true}},
	{"two sites with boundary peers, connected only", provider.TopologySpec{Filters: []provider.Filter{{Field: "site", Value: "ams1,lon1"}}, IncludeBoundaryPeers: true, ConnectedOnly: true}},
	{"a limit", provider.TopologySpec{Limit: 5}},
}

// One edge per raw cable between devices, as NetBox mode draws it: the same
// nodes, edges (ids, ends, order), counts and warnings, on the demo's data.
func TestTopology_PhysicalMatchesNetBoxMode(t *testing.T) {
	for _, c := range topologySpecs {
		spec := c.spec
		spec.Connections = "physical"
		nb, rc := bothModes(t, demoModel(), spec)
		sameGraph(t, c.name, nb, rc)
		if c.name == "everything" && len(rc.Edges) != 15 {
			t.Errorf("%d edges, want 15 (fourteen cables between devices and the wireless link): %v", len(rc.Edges), edgeIDs(rc))
		}
	}
}

// The same for paths: what NetBox computes as each interface's connected
// endpoints, read from the cable paths it stored.
func TestTopology_LogicalMatchesNetBoxMode(t *testing.T) {
	for _, c := range topologySpecs {
		nb, rc := bothModes(t, demoModel(), c.spec)
		sameGraph(t, c.name, nb, rc)
		if c.name == "everything" && len(rc.Edges) != 15 {
			t.Errorf("%d edges, want 15: %v", len(rc.Edges), edgeIDs(rc))
		}
	}
}

// A console cable and a cable to a circuit termination are cables, but not
// between devices' network ports: NetBox mode draws neither, and nor does the
// replica's — the console port and circuit termination are in none of the
// three tables a termination is classified by.
func TestTopology_PhysicalDropsConsoleAndCircuit(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{Connections: "physical"})
	for _, id := range []string{"13", "14", "15", "16", "18", "19"} {
		if _, ok := findEdge(g, id); ok {
			t.Errorf("edge %s drawn; it is a console or circuit cable", id)
		}
	}
	// The planned cable is still a cable.
	if e, ok := findEdge(g, "17"); !ok || e.Source != "17" || e.Target != "19" || e.Kind != "cable" {
		t.Errorf("the planned cable 17: %+v %v", e, ok)
	}
	// Through the panel: a cable to it, and one from it.
	for id, ends := range map[string][2]string{"10": {"3", "13"}, "11": {"13", "4"}} {
		if e, ok := findEdge(g, id); !ok || e.Source != ends[0] || e.Target != ends[1] {
			t.Errorf("cable %s: %+v %v, want %s to %s", id, e, ok, ends[0], ends[1])
		}
	}
}

// A cable with several terminations on an end connects each device on one end
// to each on the other; its edge ids stay unique by naming the pair.
func TestTopology_PhysicalBreakoutCrossProduct(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{Connections: "physical"})
	if e, ok := findEdge(g, "12-17-18"); !ok || e.Source != "17" || e.Target != "18" {
		t.Errorf("the breakout to one device: %+v %v, want one edge 12-17-18", e, ok)
	}

	// Its far end split over two devices.
	m := demoModel()
	for i, c := range m.comps {
		if c.k == kIface && c.id == 42 {
			m.comps[i].device = 19
		}
	}
	nb, rc := bothModes(t, m, provider.TopologySpec{Connections: "physical"})
	sameGraph(t, "split breakout", nb, rc)
	var got []string
	for _, e := range rc.Edges {
		if strings.HasPrefix(e.ID, "12") {
			got = append(got, e.ID+":"+e.Source+">"+e.Target)
		}
	}
	if want := []string{"12-17-18:17>18", "12-17-19:17>19"}; !slices.Equal(got, want) {
		t.Errorf("breakout edges %v, want %v", got, want)
	}
}

// A path spliced through a patch panel's front and rear ports ends at the
// far interface, and the edge joins the two devices, not the panel. Its
// direction is the one NetBox mode sees first, reading interfaces by device
// name: AMS1-access-01 before AMS1-leaf-02.
func TestTopology_LogicalResolvesThroughPatchPanel(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{})
	want := provider.GraphEdge{ID: "p19-20", Source: "4", Target: "3", Kind: "path"}
	if e, ok := findEdge(g, "p19-20"); !ok || e != want {
		t.Errorf("edge %+v %v, want %+v", e, ok, want)
	}
	for _, e := range g.Edges {
		if e.Source == "13" || e.Target == "13" {
			t.Errorf("edge %+v ends at the panel; the logical view passes through it", e)
		}
	}
}

// Only a path NetBox reports as reachable and ending at interfaces is an
// edge: an incomplete one (the circuit's far side not cabled), an inactive
// one (a planned cable), one ending at a provider network and one ending at
// a circuit termination are not.
func TestTopology_LogicalSkipsIncompleteInactiveAndCircuitEnd(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{})
	var got []string
	for _, e := range g.Edges {
		for _, d := range []string{"11", "17", "18", "19"} {
			if e.Source == d || e.Target == d {
				got = append(got, e.ID)
				break
			}
		}
	}
	slices.Sort(got)
	if want := []string{"p15-16", "p36-41", "p36-42", "p37-40", "p47-48"}; !slices.Equal(got, want) {
		t.Errorf("edges at devices 11, 17, 18, 19: %v, want %v", got, want)
	}
}

// A path through a circuit to a far interface is complete and active: NetBox
// reports its far interface as the endpoint, and the edge joins the devices.
func TestTopology_LogicalKeepsAPathThroughACircuit(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{})
	want := provider.GraphEdge{ID: "p37-40", Source: "17", Target: "18", Kind: "path"}
	if e, ok := findEdge(g, "p37-40"); !ok || e != want {
		t.Errorf("edge %+v %v, want %+v", e, ok, want)
	}
}

// The interface's content type id is the instance's own, read from the
// path's first node — NetBox writes a path only on its origin objects — and
// never assumed: here interfaces are 12 and 9 is a front port.
func TestTopology_LogicalOriginTypeFromFirstNode(t *testing.T) {
	m := demoModel()
	m.typeIDs[kIface], m.typeIDs[kFront] = 12, 9
	nb, rc := bothModes(t, m, provider.TopologySpec{})
	sameGraph(t, "interfaces as type 12", nb, rc)
	if len(rc.Edges) != 15 {
		t.Errorf("%d edges, want 15: %v", len(rc.Edges), edgeIDs(rc))
	}
}

// A wireless link is an edge in both views, drawn first. Its own computed
// path carries the same interface pair, so the logical view does not draw it
// twice — while a distinct wired path between the same two devices is its
// own link and stays.
func TestTopology_WirelessInBothViewsDedupedAgainstPath(t *testing.T) {
	m := demoModel()
	m.comps = append(m.comps, iface(23, 14, "eth0", 20, "A", 40), iface(24, 15, "eth0", 20, "B", 41))
	wired := steps(at(nd(kIface, 23)), at(nd(kCable, 20)), at(nd(kIface, 24)))
	m.paths = append(m.paths, topoPath{40, wired, true, true}, topoPath{41, reversed(wired), true, true})
	// A second radio pair, its link written the other way round, so that the
	// links' order is something to get right.
	m.comps = append(m.comps, iface(25, 14, "wlan1", 0, "", 0), iface(26, 15, "wlan1", 0, "", 0))
	m.wireless = append(m.wireless, topoLink{2, 26, 25})
	for _, connections := range []string{"physical", "logical"} {
		nb, rc := bothModes(t, m, provider.TopologySpec{Connections: connections})
		sameGraph(t, connections, nb, rc)
		want := []provider.GraphEdge{{ID: "w1", Source: "14", Target: "15", Kind: "wireless"}, {ID: "w2", Source: "15", Target: "14", Kind: "wireless"}}
		if len(rc.Edges) < 2 || !slices.Equal(rc.Edges[:2], want) {
			t.Errorf("%s: edges %+v, want the wireless links first, in id order", connections, rc.Edges)
		}
	}
	_, rc := bothModes(t, m, provider.TopologySpec{})
	if _, ok := findEdge(rc, "p21-22"); ok {
		t.Error("the wireless link's own path was drawn beside it")
	}
	if _, ok := findEdge(rc, "p23-24"); !ok {
		t.Error("the wired path between the access points is missing")
	}
}

// A tenant with no wireless links and no patch panels has those tables unfed
// (an empty NetBox table is never fed): the topology is complete, with no
// warning — whether the catalogue says so or, stale, the route's 404 does.
func TestTopology_NoWirelessNoPanelsTablesUnfed(t *testing.T) {
	m := demoModel()
	m.wireless = nil
	m.comps = slices.DeleteFunc(m.comps, func(c topoComp) bool {
		return c.k == kFront || c.k == kRear || c.id == 19 && c.k == kIface || c.id == 20 && c.k == kIface ||
			c.device == 14 || c.device == 15
	})
	m.paths = slices.DeleteFunc(m.paths, func(p topoPath) bool { return p.id >= 21 && p.id <= 24 })
	unfed := []string{"wireless/wireless-links", "dcim/front-ports", "dcim/rear-ports"}
	for _, stale := range []bool{false, true} {
		for _, connections := range []string{"physical", "logical"} {
			label := fmt.Sprintf("%s, stale catalogue %v", connections, stale)
			nb, rc := bothModes(t, m, provider.TopologySpec{Connections: connections, IncludeBoundaryPeers: true}, func(f *fakeService, p *Provider) {
				if stale {
					// The catalogue was read while the tables had rows, and is
					// kept for ten minutes: only the route knows.
					if _, err := p.catalogue(context.Background(), false); err != nil {
						t.Fatal(err)
					}
				}
				for _, e := range unfed {
					f.setIngested(e, false)
				}
			})
			sameGraph(t, label, nb, rc)
			if len(rc.Warnings) != 0 {
				t.Errorf("%s: warnings %q", label, rc.Warnings)
			}
			if len(rc.Edges) == 0 {
				t.Errorf("%s: no edges", label)
			}
		}
	}
}

// Links leaving the filtered set name their far device as a boundary node,
// with its name, site, role and stored status, and a device that cannot be
// read is reported, not silently dropped.
func TestTopology_BoundaryPeers(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{Connections: "physical", Filters: siteFilter("par1"), IncludeBoundaryPeers: true})
	want := []provider.GraphNode{
		{ID: "19", Title: "LON1-dev-c", Site: "LON1", Role: "Parity", Status: "planned", URL: "https://nb/dcim/devices/19/", Boundary: true},
		{ID: "12", Title: "SIN1-access-01", Site: "SIN1", Role: "Access", Status: "active", URL: "https://nb/dcim/devices/12/", Boundary: true},
	}
	if len(g.Nodes) != 4 || !slices.Equal(g.Nodes[2:], want) {
		t.Errorf("nodes %+v, want PAR1's two, then %+v by name", g.Nodes, want)
	}
	if g.Fetched != 2 || g.Total != 2 {
		t.Errorf("fetched/total %d/%d: a boundary peer is not a fetched device", g.Fetched, g.Total)
	}
	if len(g.Warnings) != 0 {
		t.Errorf("warnings %q", g.Warnings)
	}

	// A cable to a device the replica does not hold (deleted since, say).
	m := demoModel()
	m.comps = append(m.comps, iface(50, 17, "eth9", 30, "A", 0), iface(51, 99, "eth0", 30, "B", 0))
	nb, rc := bothModes(t, m, provider.TopologySpec{Connections: "physical", Filters: siteFilter("par1"), IncludeBoundaryPeers: true})
	sameGraph(t, "missing peer", nb, rc)
	if !slices.Contains(rc.Warnings, "1 connected device(s) could not be read, so links to them are missing; a device may have a working path that is not shown.") {
		t.Errorf("warnings %q, want the missing peer reported", rc.Warnings)
	}
}

// ConnectedOnly drops the devices no edge reaches, after counting them.
func TestTopology_ConnectedOnly(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{ConnectedOnly: true})
	for _, n := range g.Nodes {
		if n.ID == "13" || n.ID == "19" {
			t.Errorf("node %s kept: no path reaches it", n.ID)
		}
	}
	if len(g.Nodes) != 16 || g.Fetched != 18 || g.Total != 18 {
		t.Errorf("nodes=%d fetched=%d total=%d, want 16, 18, 18", len(g.Nodes), g.Fetched, g.Total)
	}
}

// Total is every matching device, Fetched those read, MaxRows the ceiling.
func TestTopology_TotalAndFetched(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{Limit: 5})
	if g.Total != 18 || g.Fetched != 5 || g.MaxRows != MaxLimit || len(g.Nodes) != 5 {
		t.Errorf("total=%d fetched=%d max=%d nodes=%d, want 18, 5, %d, 5", g.Total, g.Fetched, g.MaxRows, len(g.Nodes), MaxLimit)
	}
	g = cacheTopology(t, demoModel().fake(), provider.TopologySpec{Limit: 50_000})
	if g.Fetched != 18 || g.MaxRows != MaxLimit {
		t.Errorf("fetched=%d max=%d: a limit past the ceiling is clamped to it, not reset", g.Fetched, g.MaxRows)
	}
}

// A node's status is the stored value NetBox mode reads (status.value), which
// drives its colour — not the label the replica's rows are shown with.
func TestTopology_NodeStatusIsTheStoredValue(t *testing.T) {
	g := cacheTopology(t, demoModel().fake(), provider.TopologySpec{})
	status := map[string]string{}
	for _, n := range g.Nodes {
		status[n.ID] = n.Status
	}
	if status["8"] != "offline" || status["1"] != "active" || status["19"] != "planned" {
		t.Errorf("statuses %v", status)
	}
}

// A replica that reports no NetBox, with no Browser URL to fall back on,
// gives nodes no link — and that is not a degraded topology.
func TestTopology_NoDisplayURLWarning(t *testing.T) {
	f := demoModel().fake()
	f.schema.NetBoxURL = ""
	g := cacheTopology(t, f, provider.TopologySpec{IncludeBoundaryPeers: true, Filters: siteFilter("par1"), Connections: "physical"})
	if len(g.Warnings) != 0 {
		t.Errorf("warnings %q", g.Warnings)
	}
	for _, n := range g.Nodes {
		if n.URL != "" {
			t.Errorf("node %s has a link %q with no NetBox to point at", n.ID, n.URL)
		}
	}
	f = demoModel().fake()
	f.schema.NetBoxURL = ""
	g = cacheTopology(t, f, provider.TopologySpec{IncludeBoundaryPeers: true, Filters: siteFilter("par1"), Connections: "physical"},
		WithFallbackNetBoxURL("https://browser.example"))
	for _, n := range g.Nodes {
		if n.URL != "https://browser.example/dcim/devices/"+n.ID+"/" {
			t.Errorf("node %s link %q, want one on the Browser URL", n.ID, n.URL)
		}
	}
}

// The graph is as old as the oldest table it was read from, and says so; a
// table of unknown age while the replica loads is the loading warning, which
// keeps an alert rule from evaluating a partial fabric.
func TestTopology_CarriesFreshness(t *testing.T) {
	devicesAsOf, linksAsOf := "2026-10-23T16:28:18Z", "2026-10-23T15:00:00Z"
	f := demoModel().fake()
	for path, e := range f.schema.Entities {
		e.DataAsOf = strp(devicesAsOf)
		if path == "/v1/dcim/cable-terminations" {
			e.DataAsOf = strp(linksAsOf)
		}
		f.schema.Entities[path] = e
	}
	g := cacheTopology(t, f, provider.TopologySpec{Connections: "physical"})
	want := time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)
	if g.DataAsOf == nil || !g.DataAsOf.Equal(want) {
		t.Errorf("DataAsOf %v, want the cable terminations' %v", g.DataAsOf, want)
	}
	if g.SnapshotComplete == nil || !*g.SnapshotComplete {
		t.Errorf("SnapshotComplete %v", g.SnapshotComplete)
	}
	if !slices.Equal(g.Notes, []string{"Data as of 2026-10-23 15:00:00 UTC (replica-cache)."}) {
		t.Errorf("notes %q, want one note with the oldest instant", g.Notes)
	}

	// Loading: the snapshot is not complete, and the paths have no instant.
	f = demoModel().fake()
	f.schema.SnapshotComplete = false
	e := f.schema.Entities["/v1/dcim/cable-paths"]
	e.DataAsOf = nil
	f.schema.Entities["/v1/dcim/cable-paths"] = e
	g = cacheTopology(t, f, provider.TopologySpec{})
	if g.DataAsOf != nil {
		t.Errorf("DataAsOf %v, want unknown", g.DataAsOf)
	}
	if g.SnapshotComplete == nil || *g.SnapshotComplete {
		t.Errorf("SnapshotComplete %v", g.SnapshotComplete)
	}
	loading := "This replica is still loading its initial snapshot; results may be incomplete and their age is unknown."
	if !slices.Equal(g.Warnings, []string{loading}) {
		t.Errorf("warnings %q, want the loading warning once", g.Warnings)
	}
	for _, n := range g.Notes {
		if strings.HasPrefix(n, "Data as of") {
			t.Errorf("note %q: the graph's age is unknown", n)
		}
	}

	// The devices' own: their table of unknown age while loading.
	f = demoModel().fake()
	f.schema.SnapshotComplete = false
	e = f.schema.Entities["/v1/dcim/devices"]
	e.DataAsOf = nil
	f.schema.Entities["/v1/dcim/devices"] = e
	g = cacheTopology(t, f, provider.TopologySpec{})
	if g.DataAsOf != nil || !slices.Equal(g.Warnings, []string{loading}) {
		t.Errorf("DataAsOf %v, warnings %q, want unknown and the loading warning once", g.DataAsOf, g.Warnings)
	}
}

// No device matches: an empty graph, and no link table is read.
func TestTopology_NoDevices(t *testing.T) {
	f := demoModel().fake()
	g := cacheTopology(t, f, provider.TopologySpec{Filters: []provider.Filter{{Field: "status", Value: "decommissioning"}}, IncludeBoundaryPeers: true})
	if len(g.Nodes) != 0 || len(g.Edges) != 0 || g.Total != 0 || g.Fetched != 0 || len(g.Warnings) != 0 {
		t.Errorf("graph %+v, want empty", g)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.entity != "_meta/schema" && r.entity != "dcim/devices" {
			t.Errorf("read %s for no devices", r.entity)
		}
	}
}

// Thousands of devices whose ids cannot share one request: every request
// stays within the budget (the fake refuses one that does not), and the
// answer is the one a single request would give.
func TestTopology_BatchesLongDeviceSets(t *testing.T) {
	const n = 2000
	m := topoModel{typeIDs: demoTypeIDs()}
	for i := range n {
		id := 1_000_000_000_000 + i
		m.devices = append(m.devices, topoDevice{id, fmt.Sprintf("D-%05d", i), "DC1", "Leaf", "active"})
		if i > 0 {
			cable := i
			s := steps(at(nd(kIface, 2*i-1)), at(nd(kCable, cable)), at(nd(kIface, 2*i)))
			m.comps = append(m.comps, iface(2*i-1, id-1, "up", cable, "A", 2*i-1), iface(2*i, id, "down", cable, "B", 2*i))
			m.paths = append(m.paths, topoPath{2*i - 1, s, true, true}, topoPath{2 * i, reversed(s), true, true})
		}
	}
	for _, connections := range []string{"physical", "logical"} {
		f := m.fake()
		g := cacheTopology(t, f, provider.TopologySpec{Connections: connections, Limit: n})
		if len(g.Edges) != n-1 || len(g.Warnings) != 0 || g.Fetched != n {
			t.Errorf("%s: %d edges, %d fetched, warnings %q; want %d edges and none", connections, len(g.Edges), g.Fetched, g.Warnings, n-1)
		}
		key := "filter[_device_id]__in"
		table := "dcim/cable-terminations"
		if connections == "logical" {
			key, table = "filter[device_id]__in", "dcim/interfaces"
		}
		if sent := f.sentWith(table, key); len(sent) < 2 {
			t.Errorf("%s: %d requests by device, want the ids spread over several", connections, len(sent))
		}
		for _, r := range f.sentWith(table, key) {
			if !fitsBudget(r.query) {
				t.Errorf("%s: a request of %d bytes", connections, len(withoutCursor(r.query).Encode()))
			}
		}
	}
}

// A branch-scoped topology would be answered from main and look healthy.
func TestTopology_RejectsBranch(t *testing.T) {
	f := demoModel().fake()
	p := newTestProvider(t, f)
	_, err := p.Topology(provider.WithBranch(context.Background(), "feature-x"), provider.TopologySpec{})
	var u *UnsupportedError
	if !errors.As(err, &u) || u.Feature != "branches" {
		t.Fatalf("err = %v, want the branch refusal", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.requests); n != 0 {
		t.Errorf("%d requests sent for a refused query", n)
	}
}

// A wireless read that fails leaves the wired topology standing, as in NetBox
// mode, and says the wireless links are incomplete: a warning, which refuses
// an alert rule, rather than a graph that looks whole without them.
func TestTopology_WirelessReadFailureIsAWarning(t *testing.T) {
	for _, connections := range []string{"physical", "logical"} {
		f := demoModel().fake()
		f.failing = map[string]int{"wireless/wireless-links": 503}
		g := cacheTopology(t, f, provider.TopologySpec{Connections: connections})
		if !slices.Equal(g.Warnings, []string{provider.WirelessIncompleteWarning}) {
			t.Errorf("%s: warnings %q, want the wireless warning alone", connections, g.Warnings)
		}
		if _, ok := findEdge(g, "w1"); ok {
			t.Errorf("%s: a wireless edge from a read that failed", connections)
		}
		if len(g.Edges) < 10 {
			t.Errorf("%s: %d edges; the wired links still draw", connections, len(g.Edges))
		}
	}
}

// Any other table a view is built from failing fails the topology, with the
// replica's own status: a graph missing its cables or paths would look like
// a fabric with fewer links.
func TestTopology_FailedLinkReadIsAnError(t *testing.T) {
	for _, c := range []struct{ connections, table string }{
		{"physical", "dcim/cable-terminations"},
		{"physical", "dcim/interfaces"},
		{"physical", "dcim/front-ports"},
		{"physical", "dcim/rear-ports"},
		{"logical", "dcim/interfaces"},
		{"logical", "dcim/cable-paths"},
	} {
		f := demoModel().fake()
		f.failing = map[string]int{c.table: 503}
		_, err := newTestProvider(t, f).Topology(context.Background(), provider.TopologySpec{Connections: c.connections})
		if u := provider.Classify(err); u == nil || u.Status != 503 {
			t.Errorf("%s, %s failing: err = %v, want the replica's 503", c.connections, c.table, err)
		}
	}
}

// One device with more rows than one read returns: each view says which of
// its link sets is incomplete, in NetBox mode's words — the warning an alert
// rule is refused on. Only the read by device is past the ceiling here, so
// the warning cannot come from another.
func TestTopology_CappedReadsWarn(t *testing.T) {
	over := MaxLimit + 1
	t.Run("cables", func(t *testing.T) {
		f := demoModel().fake()
		for i := range over {
			f.entities["dcim/cable-terminations"] = append(f.entities["dcim/cable-terminations"], map[string]interface{}{
				"id": float64(100_000 + i), "cable_id": float64(100_000 + i), "cable_end": "A",
				"termination_type_id": float64(43), "termination_id": float64(100_000 + i), "_device_id": float64(1)})
		}
		g := cacheTopology(t, f, provider.TopologySpec{Connections: "physical"})
		if !slices.Equal(g.Warnings, []string{provider.CablesCappedWarning}) {
			t.Errorf("warnings %q, want the cables warning alone", g.Warnings)
		}
	})
	t.Run("paths", func(t *testing.T) {
		f := demoModel().fake()
		for i := range over {
			f.entities["dcim/interfaces"] = append(f.entities["dcim/interfaces"], map[string]interface{}{
				"id": float64(100_000 + i), "name": fmt.Sprintf("x%05d", i), "_name": fmt.Sprintf("x%05d", i),
				"device_id": float64(1), "cable_id": nil, "cable_end": nil, "_path_id": float64(900_000)})
		}
		g := cacheTopology(t, f, provider.TopologySpec{})
		if !slices.Equal(g.Warnings, []string{provider.PathsCappedWarning}) {
			t.Errorf("warnings %q, want the links warning alone", g.Warnings)
		}
	})
	t.Run("wireless", func(t *testing.T) {
		f := demoModel().fake()
		for i := range over {
			f.entities["wireless/wireless-links"] = append(f.entities["wireless/wireless-links"], map[string]interface{}{
				"id": float64(100 + i), "interface_a_id": float64(100_000 + i), "interface_b_id": float64(200_000 + i),
				"_interface_a_device_id": float64(14), "_interface_b_device_id": float64(15)})
		}
		g := cacheTopology(t, f, provider.TopologySpec{Connections: "physical"})
		if !slices.Equal(g.Warnings, []string{provider.WirelessIncompleteWarning}) {
			t.Errorf("warnings %q, want the wireless warning alone", g.Warnings)
		}
	})
}

// Component ids are per table, so one cable can end on interface 60 at one
// end and front port 60 at the other. The end is what tells the two apart in
// the join: without it both terminations find the same row.
func TestTopology_PhysicalSameIDAtBothEnds(t *testing.T) {
	m := demoModel()
	m.comps = append(m.comps,
		iface(60, 2, "Ethernet9", 30, "A", 0),
		topoComp{k: kFront, id: 60, device: 13, name: "FP60", cable: 30, end: "B"})
	nb, rc := bothModes(t, m, provider.TopologySpec{Connections: "physical"})
	sameGraph(t, "same id at both ends", nb, rc)
	if e, ok := findEdge(rc, "30"); !ok || e.Source != "2" || e.Target != "13" {
		t.Errorf("cable 30: %+v %v, want AMS1-leaf-01 to the panel", e, ok)
	}
}
