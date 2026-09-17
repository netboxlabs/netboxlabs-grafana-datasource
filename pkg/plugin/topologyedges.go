package plugin

import (
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// graphTruncated reports whether the traversal saw fewer devices than matched.
//
// It matters more here than for a table of rows. A truncated object list is a
// short list; a truncated GRAPH is a different graph, because edges are only
// discovered among the devices that were kept. A dual-homed device inside the
// slice can retain a failing neighbour and lose a healthy one that fell
// outside it, and the suppression recipe would then read "every upstream is
// down" and suppress an alert that should have paged someone.
func graphTruncated(g *provider.Graph) bool {
	if g == nil || g.Total <= 0 {
		return false
	}
	// Fetched, not len(Nodes). Two things move the node count for reasons that
	// are not truncation: ConnectedOnly drops isolated devices (a deliberate,
	// complete answer — and it is the editor default, so comparing against
	// len(Nodes) failed every alert whose filters matched one unconnected
	// device), and boundary peers ADD nodes from outside the filter.
	return g.Fetched < g.Total
}

// graphTruncationError is the alert-path refusal for an incomplete traversal,
// worded exactly as the object and IP-enrichment paths word theirs.
func graphTruncationError(c consumer, g *provider.Graph) string {
	if !graphTruncated(g) {
		return ""
	}
	return truncationMessage(c, g.Fetched, g.Total, g.MaxRows, nounDevices)
}

// graphDegradationError is the alert-path refusal for an incomplete EDGE set,
// which is a different failure from too few devices: every device is present,
// but one of them is missing links it really has.
func graphDegradationError(c consumer, g *provider.Graph) string {
	if g == nil || len(g.Warnings) == 0 {
		return ""
	}
	v := c.voice()
	return v.subject + " returned an incomplete set of links, so " + v.links + ". " +
		strings.Join(g.Warnings, " ")
}

// graphNotices states the same gap on a dashboard, where a partial answer is
// still worth showing.
func graphNotices(g *provider.Graph) []data.Notice {
	var out []data.Notice
	if graphTruncated(g) {
		out = append(out, data.Notice{
			Severity: data.NoticeSeverityInfo,
			Text: fmt.Sprintf("Showing %s of %s %s; links to devices outside this set are missing.",
				thousands(g.Fetched), thousands(g.Total), nounDevices),
		})
	}
	// Warning, not info: unlike a row limit the reader did not choose this, and
	// a device with links missing looks exactly like a device with fewer links.
	for _, w := range g.Warnings {
		out = append(out, data.Notice{Severity: data.NoticeSeverityWarning, Text: w})
	}
	return out
}

// buildTopologyEdgesFrame renders a topology graph as a joinable table: one row
// per ORDERED (device, peer) pair, so every device can look up its own peers.
//
// This exists because the node-graph frames the topology query returns cannot be
// joined. A SQL expression in an alert rule needs rows, and the question a
// suppression rule asks — "is any device adjacent to THIS one also failing?" —
// is answered by joining the metric to this table on device name.
//
// Both directions are emitted deliberately. The graph deduplicates each link to
// a single edge, which is right for drawing it and wrong for querying it: with
// only leaf1→spine1 present, a rule evaluating spine1 would find no peers at
// all and conclude it has no upstream. Two rows per link is the cost of every
// device being able to answer the same question.
//
// Direction is NOT inferred here. The rows carry both ends' roles and the rule
// expresses which way is upstream (see the suppression recipe in
// docs/ALERTING.md). That is a deliberate limit: a role hierarchy is
// per-estate, we cannot verify one, and a wrong guess would over-suppress —
// which is a missed page, the failure that does not announce itself. Expressed
// in SQL the assumption is at least visible to whoever wrote the rule.
func buildTopologyEdgesFrame(g *provider.Graph) *data.Frame {
	type node struct {
		name, role, site string
		boundary         bool
	}
	byID := make(map[string]node, len(g.Nodes))
	for _, n := range g.Nodes {
		byID[n.ID] = node{name: n.Title, role: n.Role, site: n.Site, boundary: n.Boundary}
	}

	var (
		deviceIDs, devices, deviceRoles, deviceSites []string
		peerIDs, peers, peerRoles, peerSites         []string
		kinds                                        []string
	)
	add := func(aID, bID, kind string) {
		a, aok := byID[aID]
		b, bok := byID[bID]
		// An edge to a device outside the node set has no name, role or site for
		// that end. A row of blanks would not join to anything and would read as
		// a peer with no role, so drop it.
		if !aok || !bok {
			return
		}
		// A boundary device never appears on the DEVICE side. Only the links it
		// shares with in-scope devices were traversed, so a row claiming it as
		// the subject would present one link as its whole neighbour set — and a
		// rule reading that concludes it has no other path and suppresses its
		// alert. It still appears as a peer, which is all that was actually
		// established about it.
		if a.boundary {
			return
		}
		deviceIDs = append(deviceIDs, aID)
		devices = append(devices, a.name)
		deviceRoles = append(deviceRoles, a.role)
		deviceSites = append(deviceSites, a.site)
		peerIDs = append(peerIDs, bID)
		peers = append(peers, b.name)
		peerRoles = append(peerRoles, b.role)
		peerSites = append(peerSites, b.site)
		kinds = append(kinds, kind)
	}

	for _, e := range g.Edges {
		add(e.Source, e.Target, e.Kind)
		add(e.Target, e.Source, e.Kind)
	}

	frame := data.NewFrame("netbox_topology_edges",
		// device/peer are the join keys, named to line up with the `device`
		// label metric exporters conventionally carry (see docs/JOIN-KEYS.md).
		data.NewField("device", nil, devices),
		data.NewField("device_role", nil, deviceRoles),
		data.NewField("device_site", nil, deviceSites),
		data.NewField("peer", nil, peers),
		data.NewField("peer_role", nil, peerRoles),
		data.NewField("peer_site", nil, peerSites),
		data.NewField("link_kind", nil, kinds),
		// NetBox permits duplicate device names across sites, which silently
		// collapses two devices into one join. The ids are the escape hatch for
		// detecting that; they are not the join key because metrics carry names.
		data.NewField("device_id", nil, deviceIDs),
		data.NewField("peer_id", nil, peerIDs),
	)
	frame.Meta = &data.FrameMeta{
		PreferredVisualization: data.VisTypeTable,
		Notices:                graphNotices(g),
	}
	return frame
}
