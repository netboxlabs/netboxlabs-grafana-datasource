package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A spine/leaf fragment: two spines, one leaf dual-homed to both, and a border
// above spine1. Enough to exercise both directions and a device with two peers.
func fabric() *provider.Graph {
	return &provider.Graph{
		Nodes: []provider.GraphNode{
			{ID: "1", Title: "leaf1", Site: "DC1", Role: "Access", Status: "active"},
			{ID: "2", Title: "spine1", Site: "DC1", Role: "Spine", Status: "active"},
			{ID: "3", Title: "spine2", Site: "DC1", Role: "Spine", Status: "active"},
			{ID: "4", Title: "border1", Site: "DC1", Role: "Core", Status: "active"},
		},
		Edges: []provider.GraphEdge{
			{ID: "e1", Source: "1", Target: "2", Kind: "path"},
			{ID: "e2", Source: "1", Target: "3", Kind: "path"},
			{ID: "e3", Source: "2", Target: "4", Kind: "path"},
		},
	}
}

func colOf(t *testing.T, f *data.Frame, name string) []string {
	t.Helper()
	for _, fld := range f.Fields {
		if fld.Name == name {
			out := make([]string, fld.Len())
			for i := 0; i < fld.Len(); i++ {
				s, _ := fld.At(i).(string)
				out[i] = s
			}
			return out
		}
	}
	t.Fatalf("column %q not present; got %v", name, f.Fields)
	return nil
}

// The rule asks "does THIS device have a failing peer", so every device must be
// able to look up its own peers. One row per edge would answer that for only
// one end of each link.
func TestTopologyEdgesEmitsBothDirections(t *testing.T) {
	f := buildTopologyEdgesFrame(fabric())

	devices := colOf(t, f, "device")
	peers := colOf(t, f, "peer")
	if len(devices) != 6 {
		t.Fatalf("want 6 rows (3 edges, both directions), got %d", len(devices))
	}

	pairs := map[string]bool{}
	for i := range devices {
		pairs[devices[i]+"->"+peers[i]] = true
	}
	for _, want := range []string{
		"leaf1->spine1", "spine1->leaf1",
		"leaf1->spine2", "spine2->leaf1",
		"spine1->border1", "border1->spine1",
	} {
		if !pairs[want] {
			t.Errorf("missing pair %q; a device cannot find that peer", want)
		}
	}
}

// Role is what the recipe's SQL keys on to express direction, so it has to be
// present for BOTH ends of every row.
func TestTopologyEdgesCarriesRoleAndSiteForBothEnds(t *testing.T) {
	f := buildTopologyEdgesFrame(fabric())

	devices := colOf(t, f, "device")
	peers := colOf(t, f, "peer")
	deviceRoles := colOf(t, f, "device_role")
	peerRoles := colOf(t, f, "peer_role")
	deviceSites := colOf(t, f, "device_site")
	peerSites := colOf(t, f, "peer_site")

	for i := range devices {
		if deviceRoles[i] == "" || peerRoles[i] == "" {
			t.Errorf("row %d (%s->%s) missing a role: %q/%q", i, devices[i], peers[i], deviceRoles[i], peerRoles[i])
		}
		if deviceSites[i] == "" || peerSites[i] == "" {
			t.Errorf("row %d missing a site", i)
		}
	}

	// Spot-check that the roles follow the right end of the pair.
	for i := range devices {
		if devices[i] == "leaf1" && peers[i] == "spine1" {
			if deviceRoles[i] != "Access" || peerRoles[i] != "Spine" {
				t.Errorf("leaf1->spine1 roles = %q/%q, want Access/Spine", deviceRoles[i], peerRoles[i])
			}
		}
	}
}

// The duplicate-device-name hazard is already documented in docs/JOIN-KEYS.md
// and was the root cause of three review findings on PR #131. Ids are the
// escape hatch that lets an author detect it.
func TestTopologyEdgesCarriesIDs(t *testing.T) {
	f := buildTopologyEdgesFrame(fabric())
	ids := colOf(t, f, "device_id")
	peerIDs := colOf(t, f, "peer_id")
	for i := range ids {
		if ids[i] == "" || peerIDs[i] == "" {
			t.Errorf("row %d missing an id", i)
		}
	}
}

// An edge naming a device the node set does not contain cannot be rendered as a
// row: there is no name, role or site for that end. Dropping it is correct;
// emitting it with blanks would put an unjoinable row in front of a rule.
func TestTopologyEdgesDropsEdgesToUnknownDevices(t *testing.T) {
	g := fabric()
	g.Edges = append(g.Edges, provider.GraphEdge{ID: "e9", Source: "1", Target: "999", Kind: "path"})

	f := buildTopologyEdgesFrame(g)
	if got := len(colOf(t, f, "device")); got != 6 {
		t.Errorf("want the dangling edge dropped (6 rows), got %d", got)
	}
}

func TestTopologyEdgesEmptyGraph(t *testing.T) {
	f := buildTopologyEdgesFrame(&provider.Graph{})
	if f == nil {
		t.Fatal("want a frame with columns even when empty, so the editor can show the schema")
	}
	if got := len(colOf(t, f, "device")); got != 0 {
		t.Errorf("want 0 rows, got %d", got)
	}
}

// A truncated traversal is not a smaller answer, it is a wrong one: edges are
// discovered only among the devices that were kept, so a dual-homed device
// inside the slice can retain a failing upstream and lose a healthy one that
// fell outside it. The suppression recipe then reads "every upstream is down"
// and silences an alert that should have paged someone.
func TestGraphTruncationIsDetected(t *testing.T) {
	complete := fabric()
	complete.Total, complete.Fetched = len(complete.Nodes), len(complete.Nodes)
	if graphTruncated(complete) {
		t.Error("a complete graph must not report truncation")
	}

	partial := fabric()
	partial.Total, partial.Fetched = 40, 4 // NetBox matched 40; the traversal fetched 4
	if !graphTruncated(partial) {
		t.Fatal("want truncation detected when fewer devices were kept than matched")
	}

	// A source that cannot count is not evidence of truncation, matching
	// isTruncated's contract for tables.
	unknown := fabric()
	unknown.Total, unknown.Fetched = 0, 4
	if graphTruncated(unknown) {
		t.Error("Total 0 means unknown, not truncated")
	}
}

// The refusal has to name the same dial, in the same words, as the object and
// IP-enrichment alert paths — a rule author hitting it should not have to learn
// a second vocabulary.
func TestGraphTruncationErrorMatchesTheOtherAlertPaths(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched, g.MaxRows = 40, 4, 10000

	msg := graphTruncationError(consumerAlert, g)
	if msg == "" {
		t.Fatal("want a refusal for a truncated traversal")
	}
	for _, want := range []string{"4 of 40", "matching devices", "Raise the row limit", "10,000"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}

	complete := fabric()
	complete.Total, complete.Fetched = 4, 4
	if graphTruncationError(consumerAlert, complete) != "" {
		t.Error("an untruncated graph must produce no refusal")
	}
}

// Dashboards keep partial-beats-none, but the gap is stated — and states the
// consequence, since a missing device silently takes its links with it.
func TestTruncatedGraphCarriesANotice(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched = 40, 4
	f := buildTopologyEdgesFrame(g)

	if f.Meta == nil || len(f.Meta.Notices) == 0 {
		t.Fatal("want a notice on a truncated traversal")
	}
	txt := f.Meta.Notices[0].Text
	if !strings.Contains(txt, "4 of 40") || !strings.Contains(txt, "links") {
		t.Errorf("notice should give the counts and say links are missing: %q", txt)
	}

	whole := fabric()
	whole.Total, whole.Fetched = 4, 4
	if f2 := buildTopologyEdgesFrame(whole); f2.Meta != nil && len(f2.Meta.Notices) > 0 {
		t.Errorf("a complete traversal needs no notice: %v", f2.Meta.Notices)
	}
}

// End to end through the alert path: the refusal must actually reach the rule,
// not merely be computable. This is the missed-page case — a rule evaluating a
// truncated traversal reads "every upstream is down" for a device whose healthy
// upstream simply fell outside the slice.
func TestQuery_TopologyEdgesRefusesTruncationForAlerts(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched, g.MaxRows = 40, 4, 10000
	d := newTestDatasource(&fakeProvider{graph: g})

	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"topology-edges","limit":4}`)}

	resp := d.query(context.Background(), q, consumerAlert)
	if resp.Error == nil {
		t.Fatal("an alert evaluation must refuse an incomplete traversal")
	}
	if resp.Status != backend.StatusBadRequest {
		t.Errorf("status = %v, want %v: the rule author must fix the query, not retry it",
			resp.Status, backend.StatusBadRequest)
	}
	if !strings.Contains(resp.Error.Error(), "matching devices") {
		t.Errorf("error should name what was truncated: %v", resp.Error)
	}

	// A dashboard keeps partial-beats-none, with the gap stated on the frame.
	resp = d.query(context.Background(), q, consumerDashboard)
	if resp.Error != nil {
		t.Fatalf("a dashboard query must still render: %v", resp.Error)
	}
	if len(resp.Frames) != 1 || resp.Frames[0].Meta == nil || len(resp.Frames[0].Meta.Notices) == 0 {
		t.Error("want the truncation stated as a notice on the dashboard path")
	}
}

// A complete traversal is not refused — the guard must not fire on the ordinary
// case, which is what would make rule authors raise limits they do not need to.
func TestQuery_TopologyEdgesAllowsCompleteTraversalForAlerts(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched = len(g.Nodes), len(g.Nodes)
	d := newTestDatasource(&fakeProvider{graph: g})

	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A", JSON: []byte(`{"queryType":"topology-edges","limit":100}`),
	}, consumerAlert)
	if resp.Error != nil {
		t.Fatalf("a complete traversal must be usable by an alert rule: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(resp.Frames))
	}
}

// ConnectedOnly is the editor default and removes isolated devices AFTER they
// were fetched — a deliberate, complete answer. Measuring truncation against
// len(Nodes) therefore failed every alert whose filters happened to match one
// unconnected device, on data that was not truncated at all.
func TestConnectedOnlyPruningIsNotTruncation(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched = 6, 6 // six matched, six fetched
	g.Nodes = g.Nodes[:4]     // two were isolated and pruned for display

	if graphTruncated(g) {
		t.Error("pruning isolated devices is not truncation: every match was fetched")
	}
	if msg := graphTruncationError(consumerAlert, g); msg != "" {
		t.Errorf("an alert must not be refused over display pruning: %s", msg)
	}
}

// Boundary peers ADD nodes from outside the filter, which must not read as
// having fetched more devices than matched.
func TestBoundaryPeersAreNotTruncation(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched = 3, 3
	// fabric() has 4 nodes; treat the fourth as a peer pulled in from outside.
	if graphTruncated(g) {
		t.Error("extra boundary-peer nodes must not affect truncation")
	}
}

// An incomplete edge set is refused separately from a truncated device set: all
// the devices are present, but one of them is missing links it really has, and
// the recipe would read that as "no working path".
func TestQuery_TopologyEdgesRefusesDegradedEdgesForAlerts(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched = len(g.Nodes), len(g.Nodes) // not truncated
	g.Warnings = []string{"Wireless links could not be read in full, so some devices may show fewer connections than they have."}
	d := newTestDatasource(&fakeProvider{graph: g})

	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"topology-edges","limit":100}`)}

	resp := d.query(context.Background(), q, consumerAlert)
	if resp.Error == nil {
		t.Fatal("an alert evaluation must refuse an incomplete link set")
	}
	if !strings.Contains(resp.Error.Error(), "working path") {
		t.Errorf("the error should say what the consequence is: %v", resp.Error)
	}

	// Dashboards still render, with the gap as a warning notice.
	resp = d.query(context.Background(), q, consumerDashboard)
	if resp.Error != nil {
		t.Fatalf("a dashboard query must still render: %v", resp.Error)
	}
	notices := resp.Frames[0].Meta.Notices
	if len(notices) == 0 || notices[0].Severity != data.NoticeSeverityWarning {
		t.Errorf("want a warning notice for a degraded link set, got %+v", notices)
	}
}

// The edges query must ask for boundary peers; the node graph must not.
func TestQuery_TopologyEdgesAsksForBoundaryPeers(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched = len(g.Nodes), len(g.Nodes)

	fp := &fakeProvider{graph: g}
	d := newTestDatasource(fp)
	d.query(context.Background(), backend.DataQuery{
		RefID: "A", JSON: []byte(`{"queryType":"topology-edges","limit":100}`),
	}, consumerDashboard)
	if !fp.topoSpec.IncludeBoundaryPeers {
		t.Error("the edges query must keep links whose far end is outside the filter")
	}

	fp2 := &fakeProvider{graph: g}
	d2 := newTestDatasource(fp2)
	d2.query(context.Background(), backend.DataQuery{
		RefID: "A", JSON: []byte(`{"queryType":"topology","limit":100}`),
	}, consumerDashboard)
	if fp2.topoSpec.IncludeBoundaryPeers {
		t.Error("the node graph must not draw links dangling outside the filter")
	}
}

// A boundary peer's own links were never traversed — only the ones it shares
// with in-scope devices. Putting it on the device side would present a single
// link as its entire neighbour set, and a rule reading that concludes it has no
// other path and suppresses its alert. It is exactly the bug the boundary-peer
// fix was meant to solve, reintroduced from the other direction.
func TestBoundaryPeersNeverAppearOnTheDeviceSide(t *testing.T) {
	g := &provider.Graph{
		Nodes: []provider.GraphNode{
			{ID: "1", Title: "spine1", Site: "AMS1", Role: "Spine"},
			// core1 matched no filter; it was pulled in because a link reached it.
			{ID: "9", Title: "core1", Site: "NYC1", Role: "Core", Boundary: true},
		},
		Edges:   []provider.GraphEdge{{ID: "e1", Source: "1", Target: "9", Kind: "path"}},
		Total:   1,
		Fetched: 1,
	}

	f := buildTopologyEdgesFrame(g)
	devices := colOf(t, f, "device")
	peers := colOf(t, f, "peer")

	if len(devices) != 1 {
		t.Fatalf("want exactly one row (spine1 -> core1), got %d: %v -> %v", len(devices), devices, peers)
	}
	if devices[0] != "spine1" || peers[0] != "core1" {
		t.Errorf("row = %s -> %s, want spine1 -> core1", devices[0], peers[0])
	}
	for _, d := range devices {
		if d == "core1" {
			t.Error("core1 is a boundary peer: its own links were never traversed, so it must not be a subject")
		}
	}
}

// Fully in-scope edges still emit both directions — the boundary rule must not
// cost the ordinary case, which is what makes every device able to look itself up.
func TestInScopeEdgesStillEmitBothDirections(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched = len(g.Nodes), len(g.Nodes)

	f := buildTopologyEdgesFrame(g)
	if got := len(colOf(t, f, "device")); got != 6 {
		t.Errorf("want 6 rows for 3 fully in-scope edges, got %d", got)
	}
}

// The refusal quotes what was fetched, not the node count — pruning and
// boundary peers both move len(Nodes) and would otherwise report "0 of 40", or
// even more devices than matched.
func TestTruncationMessageQuotesTheFetchedCount(t *testing.T) {
	g := fabric()
	g.Total, g.Fetched, g.MaxRows = 40, 7, 10000
	g.Nodes = g.Nodes[:1] // pruned for display, plus boundary peers elsewhere

	msg := graphTruncationError(consumerAlert, g)
	if !strings.Contains(msg, "7 of 40") {
		t.Errorf("want the fetched count in the message, got: %s", msg)
	}
}
