package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
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

// scopedTopologyServer is a NetBox that RECORDS the device_id scoping of every
// edge request and refuses to answer an unscoped one. The refusal is the point:
// a mock that answered regardless would let an unscoped fetch pass the test,
// which is exactly the bug — an unscoped edge fetch is capped at MaxLimit rows
// and silently becomes a fixed prefix of the database, so a site-filtered
// topology renders its nodes with zero edges.
type scopedTopologyServer struct {
	mu    sync.Mutex
	scope map[string][][]string // endpoint -> device_id sets, one per request
}

func (s *scopedTopologyServer) record(endpoint string, r *http.Request) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scope == nil {
		s.scope = map[string][][]string{}
	}
	ids := r.URL.Query()[deviceScopeParam]
	s.scope[endpoint] = append(s.scope[endpoint], ids)
	return ids
}

func (s *scopedTopologyServer) requests(endpoint string) [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope[endpoint]
}

// TestTopology_EdgeFetchesAreScopedToTheNodeSet is the regression test for the
// silent-zero-edges defect: dcim/interfaces and dcim/cables must be asked only
// about the devices that are actually in the graph.
func TestTopology_EdgeFetchesAreScopedToTheNodeSet(t *testing.T) {
	for _, conns := range []string{"logical", "physical"} {
		t.Run(conns, func(t *testing.T) {
			rec := &scopedTopologyServer{}
			mux := http.NewServeMux()
			mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, `{"count":3,"next":null,"results":[
					{"id":7,"name":"a"},{"id":8,"name":"b"},{"id":9,"name":"c"}]}`)
			})
			mux.HandleFunc("/api/dcim/interfaces/", func(w http.ResponseWriter, r *http.Request) {
				if len(rec.record("interfaces", r)) == 0 {
					http.Error(w, `{"detail":"unscoped"}`, http.StatusBadRequest)
					return
				}
				_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[
					{"id":70,"device":{"id":7},"connected_endpoints_type":"dcim.interface","connected_endpoints_reachable":true,
					 "connected_endpoints":[{"id":80,"device":{"id":8}}]}]}`)
			})
			mux.HandleFunc("/api/dcim/cables/", func(w http.ResponseWriter, r *http.Request) {
				if len(rec.record("cables", r)) == 0 {
					http.Error(w, `{"detail":"unscoped"}`, http.StatusBadRequest)
					return
				}
				_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[
					{"id":50,"a_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":7}}}],
					        "b_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":8}}}]}]}`)
			})
			mux.HandleFunc("/api/wireless/wireless-links/", func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

			g, err := p.Topology(context.Background(), provider.TopologySpec{Connections: conns})
			if err != nil {
				t.Fatalf("Topology: %v", err)
			}
			if len(g.Edges) != 1 {
				t.Fatalf("edges = %v, want the one edge inside the node set", edgeSet(g))
			}

			endpoint := "interfaces"
			if conns == "physical" {
				endpoint = "cables"
			}
			got := rec.requests(endpoint)
			if len(got) != 1 {
				t.Fatalf("%s requests = %d, want exactly 1 batch for a 3-device graph", endpoint, len(got))
			}
			want := []string{"7", "8", "9"}
			if !slices.Equal(got[0], want) {
				t.Errorf("%s scoped to %v, want %v (the node set, in device order)", endpoint, got[0], want)
			}
			// The OTHER endpoint must not be touched at all: each view derives its
			// edges from one source, and a stray fetch is the unscoped walk coming
			// back through a different door.
			other := "cables"
			if conns == "physical" {
				other = "interfaces"
			}
			if n := len(rec.requests(other)); n != 0 {
				t.Errorf("%s was fetched %d times for the %s view", other, n, conns)
			}
		})
	}
}

// TestTopology_NoDevicesMakesNoEdgeRequests: an empty node set can produce no
// edge, so asking is pure latency — and on a big instance it was a 20-page walk
// of the whole cable table to arrive at nothing.
func TestTopology_NoDevicesMakesNoEdgeRequests(t *testing.T) {
	var edgeCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
	})
	for _, path := range []string{"/api/dcim/interfaces/", "/api/dcim/cables/"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			edgeCalls++
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
		})
	}
	mux.HandleFunc("/api/wireless/wireless-links/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	for _, conns := range []string{"logical", "physical"} {
		g, err := p.Topology(context.Background(), provider.TopologySpec{Connections: conns})
		if err != nil {
			t.Fatalf("Topology(%s): %v", conns, err)
		}
		if len(g.Nodes) != 0 || len(g.Edges) != 0 {
			t.Fatalf("%s: want an empty graph, got %d nodes / %d edges", conns, len(g.Nodes), len(g.Edges))
		}
	}
	if edgeCalls != 0 {
		t.Errorf("edge endpoints were fetched %d times for a graph with no devices", edgeCalls)
	}
}

// TestTopology_OverflowingBatchIsSplitAndCablesDedup covers the two things that
// only appear once a node set is too big for one request.
//
// A batch whose envelope count exceeds the rows one request can carry is SPLIT,
// because keeping it would be the same silent truncation at batch scale. And a
// cable is matched by device_id from EITHER of its ends, so once the two ends
// land in different batches NetBox returns it twice — which must still be one
// edge, not a phantom parallel link.
func TestTopology_OverflowingBatchIsSplitAndCablesDedup(t *testing.T) {
	var batches [][]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[
			{"id":1,"name":"a"},{"id":2,"name":"b"}]}`)
	})
	mux.HandleFunc("/api/wireless/wireless-links/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
	})
	mux.HandleFunc("/api/dcim/cables/", func(w http.ResponseWriter, r *http.Request) {
		ids := r.URL.Query()[deviceScopeParam]
		batches = append(batches, ids)
		if len(ids) > 1 {
			// Overflow: NetBox reports far more matches than it returned. The
			// provider must NOT index this page — it must re-ask in halves.
			_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[
				{"id":50,"a_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":1}}}],
				        "b_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":2}}}]}]}`, MaxLimit+1)
			return
		}
		// Single-device batch: the cable touches device 1 and device 2, so both
		// halves return it.
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[
			{"id":50,"a_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":1}}}],
			        "b_terminations":[{"object_type":"dcim.interface","object":{"device":{"id":2}}}]}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	g, err := p.Topology(context.Background(), provider.TopologySpec{Connections: "physical"})
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	want := [][]string{{"1", "2"}, {"1"}, {"2"}}
	if !slices.EqualFunc(batches, want, slices.Equal[[]string]) {
		t.Errorf("cable batches = %v, want %v (the overflowing batch split, first half first)", batches, want)
	}
	if len(g.Edges) != 1 {
		t.Fatalf("edges = %v, want exactly 1 — the same cable seen from both of its ends is one link", g.Edges)
	}
	if g.Edges[0].ID != "50" {
		t.Errorf("edge id = %q, want the cable id", g.Edges[0].ID)
	}
}
