package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// mockTopologyNetBox emulates the topology slice of the API: five devices
// (leaf1, leaf2, panel, ap1, ap2), a panel-spliced cable run, interface
// paths, and one wireless link.
func mockTopologyNetBox(t *testing.T) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":5,"next":null,"results":[
			{"id":1,"name":"leaf1","site":{"name":"dc1"},"role":{"name":"leaf"},"status":{"value":"active"},"display_url":"https://nb/dcim/devices/1/"},
			{"id":2,"name":"leaf2","site":{"name":"dc1"},"role":{"name":"leaf"},"status":{"value":"active"},"display_url":"https://nb/dcim/devices/2/"},
			{"id":3,"name":"pp1","site":{"name":"dc1"},"role":{"name":"panel"},"status":{"value":"active"},"display_url":"https://nb/dcim/devices/3/"},
			{"id":4,"name":"ap1","site":{"name":"dc1"},"role":{"name":"ap"},"status":{"value":"active"},"display_url":"https://nb/dcim/devices/4/"},
			{"id":5,"name":"ap2","site":{"name":"dc1"},"role":{"name":"ap"},"status":{"value":"active"},"display_url":"https://nb/dcim/devices/5/"}
		]}`)
	})
	// Physical: leaf1 -> pp1 front port; pp1 rear port -> leaf2; plus a
	// PARALLEL pair of direct leaf1<->leaf2 cables (both must render raw).
	mux.HandleFunc("/api/dcim/cables/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":4,"next":null,"results":[
			{"id":10,"a_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":1}}}],
			         "b_terminations":[{"object_type":"dcim.frontport","object":{"device":{"id":3}}}]},
			{"id":11,"a_terminations":[{"object_type":"dcim.rearport","object":{"device":{"id":3}}}],
			         "b_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":2}}}]},
			{"id":12,"a_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":1}}}],
			         "b_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":2}}}]},
			{"id":13,"a_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":1}}}],
			         "b_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":2}}}]}
		]}`)
	})
	// Logical: both directions of the leaf1<->leaf2 path (same link, dedup) +
	// a PARALLEL second leaf1<->leaf2 path (distinct interfaces — must render),
	// one unreachable path, one provider-network endpoint (skipped), the
	// wireless link's own computed path (wlan ifaces 130/131 — must dedup
	// against the wireless edge), and a distinct WIRED ap1<->ap2 path
	// (ifaces 120/121 — must survive alongside the wireless edge).
	mux.HandleFunc("/api/dcim/interfaces/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("connected") != "true" {
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"count":9,"next":null,"results":[
			{"id":100,"device":{"id":1},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":101,"device":{"id":2}}]},
			{"id":101,"device":{"id":2},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":100,"device":{"id":1}}]},
			{"id":110,"device":{"id":1},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":111,"device":{"id":2}}]},
			{"id":111,"device":{"id":2},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":110,"device":{"id":1}}]},
			{"id":102,"device":{"id":1},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":false,
			 "connected_endpoints":[{"id":999,"device":{"id":5}}]},
			{"id":103,"device":{"id":2},"connected_endpoints_type":"circuits.providernetwork","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":0,"device":{"id":0}}]},
			{"id":130,"device":{"id":4},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":131,"device":{"id":5}}]},
			{"id":120,"device":{"id":4},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":121,"device":{"id":5}}]},
			{"id":121,"device":{"id":5},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":120,"device":{"id":4}}]}
		]}`)
	})
	mux.HandleFunc("/api/wireless/wireless-links/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[
			{"id":30,"interface_a":{"id":130,"device":{"id":4}},"interface_b":{"id":131,"device":{"id":5}}}
		]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
}

func edgeSet(g *provider.Graph) map[string]string { // "src->dst" -> kind
	m := map[string]string{}
	for _, e := range g.Edges {
		m[e.Source+"->"+e.Target] = e.Kind
	}
	return m
}

// kindCount tallies edges between two devices (either direction) by kind.
func kindCount(g *provider.Graph, a, b string) map[string]int {
	m := map[string]int{}
	for _, e := range g.Edges {
		if (e.Source == a && e.Target == b) || (e.Source == b && e.Target == a) {
			m[e.Kind]++
		}
	}
	return m
}

func TestTopology_Logical(t *testing.T) {
	p := mockTopologyNetBox(t)
	g, err := p.Topology(context.Background(), provider.TopologySpec{Connections: "logical"})
	if err != nil {
		t.Fatal(err)
	}
	// leaf1<->leaf2: two PARALLEL paths, each reported from both ends —
	// dedup by link identity keeps both, once each.
	if kc := kindCount(g, "1", "2"); kc["path"] != 2 {
		t.Errorf("leaf1<->leaf2 = %v, want 2 parallel path edges", kc)
	}
	// ap1<->ap2: the wireless edge (its own computed path dedups against it)
	// plus the distinct WIRED path.
	if kc := kindCount(g, "4", "5"); kc["wireless"] != 1 || kc["path"] != 1 {
		t.Errorf("ap1<->ap2 = %v, want 1 wireless + 1 wired path", kc)
	}
	if len(g.Edges) != 4 {
		t.Fatalf("edges = %v, want 4 (2 parallel paths + wireless + wired path)", edgeSet(g))
	}
}

func TestTopology_LogicalIsDefault(t *testing.T) {
	p := mockTopologyNetBox(t)
	g, err := p.Topology(context.Background(), provider.TopologySpec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 4 {
		t.Fatalf("default should be logical (4 edges), got %v", edgeSet(g))
	}
}

func TestTopology_Physical(t *testing.T) {
	p := mockTopologyNetBox(t)
	g, err := p.Topology(context.Background(), provider.TopologySpec{Connections: "physical"})
	if err != nil {
		t.Fatal(err)
	}
	es := edgeSet(g)
	// leaf1->pp1, pp1->leaf2 (panel hops!), plus the wireless link.
	if es["1->3"] != "cable" || es["3->2"] != "cable" {
		t.Errorf("panel hops missing: %v", es)
	}
	if es["4->5"] != "wireless" && es["5->4"] != "wireless" {
		t.Errorf("wireless edge missing or mislabeled: %v", es)
	}
	// Raw view: BOTH parallel leaf1<->leaf2 cables render, with cable IDs.
	parallel := 0
	for _, e := range g.Edges {
		if e.Source == "1" && e.Target == "2" && e.Kind == "cable" {
			parallel++
			if e.ID != "12" && e.ID != "13" {
				t.Errorf("parallel cable edge has unexpected id %q", e.ID)
			}
		}
	}
	if parallel != 2 {
		t.Errorf("parallel cables = %d, want 2 (no pair dedup in physical view)", parallel)
	}
	if len(g.Edges) != 5 {
		t.Fatalf("edges = %v, want 2 panel hops + 2 parallel cables + 1 wireless", es)
	}
}

func TestTopology_UnknownConnectionsFallsBackToLogical(t *testing.T) {
	p := mockTopologyNetBox(t)
	g, err := p.Topology(context.Background(), provider.TopologySpec{Connections: "bogus"})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 4 {
		t.Fatalf("unknown connections value should behave as logical (4 edges), got %v", edgeSet(g))
	}
}

func TestTopology_ConnectedOnlyKeepsWirelessNodes(t *testing.T) {
	p := mockTopologyNetBox(t)
	g, err := p.Topology(context.Background(), provider.TopologySpec{Connections: "logical", ConnectedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		ids[n.ID] = true
	}
	for _, want := range []string{"1", "2", "4", "5"} {
		if !ids[want] {
			t.Errorf("connectedOnly dropped node %s; nodes=%v", want, ids)
		}
	}
	if ids["3"] {
		t.Errorf("panel should be dropped in logical+connectedOnly; nodes=%v", ids)
	}
}

func TestTopology_NodeURL(t *testing.T) {
	p := mockTopologyNetBox(t)
	g, err := p.Topology(context.Background(), provider.TopologySpec{})
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	var got string
	for _, n := range g.Nodes {
		if n.ID == "1" {
			got = n.URL
		}
	}
	if got != "https://nb/dcim/devices/1/" {
		t.Errorf("node 1 URL = %q, want the device display_url", got)
	}
}
