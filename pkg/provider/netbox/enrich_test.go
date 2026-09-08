package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Pins the existing contract: a filter key is the field, or field__operator, and
// CSV values become repeated params (which NetBox ORs).
func TestBuildFilterValues_KeysAndCSV(t *testing.T) {
	got := buildFilterValues([]provider.Filter{
		{Field: "site", Operator: "", Value: "dc1,dc2"},
		{Field: "name", Operator: "ic", Value: "spine"},
	})
	if vs := got["site"]; len(vs) != 2 || vs[0] != "dc1" || vs[1] != "dc2" {
		t.Errorf("site = %v, want [dc1 dc2] (repeated params = OR)", vs)
	}
	if v := got.Get("name__ic"); v != "spine" {
		t.Errorf("name__ic = %q, want spine", v)
	}
}

func TestBuildFilterValues_EmptyFamily(t *testing.T) {
	got := buildFilterValues([]provider.Filter{
		// A stray value is deliberate on both rows: the editor hides the value
		// control for empty-family operators but does not clear an already-saved
		// value, so the backend must ignore it. The value also makes the
		// "no leaked key" guard below load-bearing — without it, the pre-fix code
		// skips the row entirely (Split("", ",") yields only "") and the guard
		// would pass vacuously.
		{Field: "name", Operator: "empty", Value: "ignored"},
		{Field: "serial", Operator: "nempty", Value: "ignored"},
	})
	if v := got.Get("name__empty"); v != "true" {
		t.Errorf("name__empty = %q, want true", v)
	}
	if v := got.Get("serial__empty"); v != "false" {
		t.Errorf("serial__empty = %q, want false", v)
	}
	// The operator must not leak into the key as a suffix of its own.
	if _, bad := got["serial__nempty"]; bad {
		t.Error("serial__nempty must not be emitted; nempty maps to __empty=false")
	}
}

// An over-limit request used to be treated like an unset one and reset to the
// default, so a caller asking for 5,000 devices got 1,000 — while the
// truncation refusal told them the maximum was 10,000. Following that advice
// changed nothing and the rule failed again.
//
// Asserted on how many devices survive rather than on the outgoing limit
// parameter, because fetchRows pages internally and never sends the caller's
// number verbatim.
func TestTopologyClampsLimitToMaxRatherThanResetting(t *testing.T) {
	const available = 1500 // more than the default, fewer than the ceiling

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "dcim/devices") {
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if limit <= 0 {
			limit = 50
		}
		var rows []string
		for i := offset; i < offset+limit && i < available; i++ {
			rows = append(rows, fmt.Sprintf(`{"id":%d,"name":"d%d","status":{"value":"active"}}`, i+1, i+1))
		}
		next := "null"
		if offset+limit < available {
			next = fmt.Sprintf("%q", fmt.Sprintf("%s/api/dcim/devices/?limit=%d&offset=%d", srvURL, limit, offset+limit))
		}
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":%s,"results":[%s]}`, available, next, strings.Join(rows, ","))
	}))
	defer srv.Close()
	srvURL = srv.URL

	// Above the ceiling: this is the case the bug affected. A value inside the
	// ceiling was always honoured, so testing 5,000 would have proved nothing.
	p := New(srv.URL, "t", srv.Client())
	g, err := p.Topology(context.Background(), provider.TopologySpec{Limit: MaxLimit + 1})
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if g.Fetched != available {
		t.Errorf("fetched %d devices for a limit above the ceiling; want %d — an over-limit request was reset to the default (%d) instead of clamped to the maximum (%d)",
			g.Fetched, available, defaultLimit, MaxLimit)
	}
	if g.Total != available {
		t.Errorf("Total = %d, want %d", g.Total, available)
	}
}

// A boundary peer that cannot be read — hidden by object permissions, or
// deleted between the edge lookup and the device lookup — takes its edge with
// it, because the frame cannot name an endpoint it has no device for. That
// removes a neighbour silently, and the removed one may be the healthy one, so
// the result has to carry the gap or an alert rule will suppress on it.
func TestTopologyReportsUnresolvableBoundaryPeers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		// The in-scope device set.
		case strings.Contains(r.URL.Path, "dcim/devices") && q.Get("id") == "":
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":1,"name":"spine1","status":{"value":"active"},"role":{"name":"Spine"}}]}`))
		// The boundary-peer lookup: device 9 is deliberately withheld.
		case strings.Contains(r.URL.Path, "dcim/devices"):
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
		// spine1 has a link to device 9, which is outside the filtered set.
		case strings.Contains(r.URL.Path, "dcim/interfaces"):
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":10,"device":{"id":1},"connected_endpoints_type":"dcim.interface",
				 "connected_endpoints_reachable":true,
				 "connected_endpoints":[{"id":20,"device":{"id":9}}]}]}`))
		default:
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
		}
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	g, err := p.Topology(context.Background(), provider.TopologySpec{
		Limit: 100, IncludeBoundaryPeers: true,
	})
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if len(g.Warnings) == 0 {
		t.Fatal("an unreadable boundary peer must be reported: its link vanishes from the result")
	}
	if !strings.Contains(g.Warnings[0], "working path") {
		t.Errorf("the warning should state the consequence, got: %q", g.Warnings[0])
	}
}

// The ordinary case stays quiet: a boundary peer that resolves is not a gap.
func TestTopologyDoesNotWarnWhenBoundaryPeersResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case strings.Contains(r.URL.Path, "dcim/devices") && q.Get("id") == "":
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":1,"name":"spine1","status":{"value":"active"},"role":{"name":"Spine"}}]}`))
		case strings.Contains(r.URL.Path, "dcim/devices"):
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":9,"name":"core1","status":{"value":"active"},"role":{"name":"Core"}}]}`))
		case strings.Contains(r.URL.Path, "dcim/interfaces"):
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":10,"device":{"id":1},"connected_endpoints_type":"dcim.interface",
				 "connected_endpoints_reachable":true,
				 "connected_endpoints":[{"id":20,"device":{"id":9}}]}]}`))
		default:
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
		}
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	g, err := p.Topology(context.Background(), provider.TopologySpec{
		Limit: 100, IncludeBoundaryPeers: true,
	})
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if len(g.Warnings) != 0 {
		t.Errorf("a resolved boundary peer is not a gap: %v", g.Warnings)
	}
	var sawBoundary bool
	for _, n := range g.Nodes {
		if n.Title == "core1" && n.Boundary {
			sawBoundary = true
		}
	}
	if !sawBoundary {
		t.Error("the boundary peer should be present and marked")
	}
}

// A cable end is a list because one cable can terminate on several ports — a
// breakout is the common case. Taking only the first device discards the rest,
// and a discarded peer may be the healthy upstream: the suppression recipe then
// reads the remaining down peer as the whole neighbour set and stops a page.
func TestPhysicalEdgesKeepEveryTerminationPeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "dcim/devices"):
			_, _ = w.Write([]byte(`{"count":3,"results":[
				{"id":1,"name":"leaf1","status":{"value":"active"},"role":{"name":"Access"}},
				{"id":2,"name":"spine1","status":{"value":"active"},"role":{"name":"Spine"}},
				{"id":3,"name":"spine2","status":{"value":"active"},"role":{"name":"Spine"}}]}`))
		// One breakout cable: leaf1 on the A side, BOTH spines on the B side.
		case strings.Contains(r.URL.Path, "dcim/cables"):
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":77,
				 "a_terminations":[{"object_type":"dcim.interface","object":{"id":10,"device":{"id":1}}}],
				 "b_terminations":[
					{"object_type":"dcim.interface","object":{"id":20,"device":{"id":2}}},
					{"object_type":"dcim.interface","object":{"id":21,"device":{"id":3}}}]}]}`))
		default:
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
		}
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	g, err := p.Topology(context.Background(), provider.TopologySpec{Limit: 100, Connections: "physical"})
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}

	pairs := map[string]bool{}
	ids := map[string]bool{}
	for _, e := range g.Edges {
		pairs[e.Source+"-"+e.Target] = true
		if ids[e.ID] {
			t.Errorf("duplicate edge id %q: one cable yielding several edges must still produce unique ids", e.ID)
		}
		ids[e.ID] = true
	}
	if len(g.Edges) != 2 {
		t.Fatalf("want 2 edges from the breakout cable, got %d: %+v", len(g.Edges), g.Edges)
	}
	for _, want := range []string{"1-2", "1-3"} {
		if !pairs[want] {
			t.Errorf("missing pair %s — a real neighbour was dropped", want)
		}
	}
}

// An ordinary point-to-point cable keeps its plain cable id, so nothing about
// the common case changes.
func TestPhysicalEdgesKeepPlainIDsForSimpleCables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "dcim/devices"):
			_, _ = w.Write([]byte(`{"count":2,"results":[
				{"id":1,"name":"leaf1","status":{"value":"active"}},
				{"id":2,"name":"spine1","status":{"value":"active"}}]}`))
		case strings.Contains(r.URL.Path, "dcim/cables"):
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":77,
				 "a_terminations":[{"object_type":"dcim.interface","object":{"id":10,"device":{"id":1}}}],
				 "b_terminations":[{"object_type":"dcim.interface","object":{"id":20,"device":{"id":2}}}]}]}`))
		default:
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
		}
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	g, err := p.Topology(context.Background(), provider.TopologySpec{Limit: 100, Connections: "physical"})
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].ID != "77" {
		t.Errorf("want one edge with the plain cable id, got %+v", g.Edges)
	}
}
