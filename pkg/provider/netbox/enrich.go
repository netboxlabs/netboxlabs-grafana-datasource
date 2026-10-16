package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// FiltersNarrow reports whether at least one of filters would reach NetBox as a
// query parameter — the rule buildFilterValues applies, exposed so a caller can
// ask "does this filter set narrow anything?" without duplicating it: a row with
// no field or a blank value is dropped, an empty-family operator needs no value.
func FiltersNarrow(filters []provider.Filter) bool {
	return len(buildFilterValues(filters)) > 0
}

// buildFilterValues turns provider filters into NetBox query params, expanding
// CSV values (from multi-value variables) into repeated params (OR).
func buildFilterValues(filters []provider.Filter) url.Values {
	q := url.Values{}
	for _, f := range filters {
		if f.Field == "" {
			continue
		}
		// __empty is a BooleanFilter: "empty" asks for true, "nempty" ("has any
		// value") asks for false. Set (not Add) because repeated __empty params
		// resolve to the last one — two empty-family rows on one field are a
		// conflict the editor flags rather than something to encode twice.
		if f.Operator == "empty" || f.Operator == "nempty" {
			q.Set(f.Field+"__empty", strconv.FormatBool(f.Operator == "empty"))
			continue
		}
		key := f.Field
		if f.Operator != "" && f.Operator != "exact" {
			key = f.Field + "__" + f.Operator
		}
		for _, v := range strings.Split(f.Value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				q.Add(key, v)
			}
		}
	}
	return q
}

// Topology returns devices as nodes and inter-device links as edges: logical
// cable paths (default — patch panels and circuits resolve to the far device)
// or raw physical cables, plus wireless links in both views.
func (p *Provider) Topology(ctx context.Context, spec provider.TopologySpec) (*provider.Graph, error) {
	// One branch for every request and cache key this call makes (pinBranch).
	ctx, _ = p.client.pinBranch(ctx)
	// Clamped to the ceiling rather than reset to the default, matching Query.
	// Treating an over-limit request like an unset one sent a caller asking for
	// 5,000 devices back to 1,000 — and the truncation refusal then advises
	// raising the limit "(max 10,000)", so following the advice changed nothing
	// and the rule failed again for a reason the message had denied.
	limit := spec.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	devRows, total, err := p.fetchRows(ctx, "dcim/devices", buildFilterValues(spec.Filters), limit)
	if err != nil {
		return nil, err
	}

	nodes := make([]provider.GraphNode, 0, len(devRows))
	inSet := map[string]bool{}
	// nodeIDs is inSet in device order. The edge fetches are scoped to it (see
	// fetchEdgeRows), and keeping NetBox's own device ordering means the scoped
	// responses arrive in the same relative order the unscoped fetch delivered
	// them, so the emitted edge order is unchanged.
	nodeIDs := make([]string, 0, len(devRows))
	for _, raw := range devRows {
		n, ok := deviceNode(raw)
		if !ok {
			continue
		}
		if !inSet[n.ID] {
			inSet[n.ID] = true
			nodeIDs = append(nodeIDs, n.ID)
		}
		nodes = append(nodes, n)
	}

	// Links dedup by identity — the sorted interface-ID pair — NOT by device
	// pair, so parallel links between the same two devices (dual uplinks, LAG
	// members) all render. Each logical path is reported from both of its end
	// interfaces and collapses onto the same key; a wireless link's own
	// computed path carries the same interface pair as the wireless edge, so
	// emitting wireless first dedups that duplicate while a distinct wired
	// path between the same devices survives. The physical view is strictly
	// raw: one edge per cable, plus the wireless-link edges.
	seen := map[string]bool{}
	// Completeness of the EDGE set is tracked separately from the device limit.
	// A missing link is invisible in the output — the device is still listed,
	// merely looking less connected than it is — so a caller reasoning about
	// neighbours has to be told, or it will read the gap as fact.
	var warnings []string
	edges, wirelessIncomplete := p.wirelessEdges(ctx, inSet, seen, spec.IncludeBoundaryPeers)
	if wirelessIncomplete {
		warnings = append(warnings, "Wireless links could not be read in full, so some devices may show fewer connections than they have.")
	}
	if spec.Connections == "physical" {
		physical, capped, err := p.physicalEdges(ctx, inSet, nodeIDs, spec.IncludeBoundaryPeers)
		if err != nil {
			return nil, err
		}
		if capped {
			warnings = append(warnings, "Some cables could not be read: a device has more links than one request can return, so its remaining connections are missing.")
		}
		edges = append(edges, physical...)
	} else {
		logical, capped, err := p.logicalEdges(ctx, inSet, nodeIDs, seen, spec.IncludeBoundaryPeers)
		if err != nil {
			return nil, err
		}
		if capped {
			warnings = append(warnings, "Some links could not be read: a device has more connections than one request can return, so its remaining connections are missing.")
		}
		edges = append(edges, logical...)
	}

	// Peers outside the filtered set have no node yet, so their name, role and
	// site are unknown. Fetch them, or the edge that names them is unusable.
	if spec.IncludeBoundaryPeers {
		peerNodes, missingPeers, err := p.boundaryPeerNodes(ctx, inSet, edges)
		if err != nil {
			return nil, err
		}
		if missingPeers > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%d connected device(s) could not be read, so links to them are missing; a device may have a working path that is not shown.",
				missingPeers))
		}
		nodes = append(nodes, peerNodes...)
	}

	connected := map[string]bool{}
	for _, e := range edges {
		connected[e.Source] = true
		connected[e.Target] = true
	}

	// Measured BEFORE pruning: dropping an isolated device is a deliberate,
	// complete answer, so counting it as truncation would fail every alert whose
	// filters happen to match one unconnected device.
	fetched := len(devRows)

	if spec.ConnectedOnly {
		kept := nodes[:0]
		for _, n := range nodes {
			if connected[n.ID] {
				kept = append(kept, n)
			}
		}
		nodes = kept
	}

	return &provider.Graph{
		Nodes:    nodes,
		Edges:    edges,
		Total:    total,
		Fetched:  fetched,
		MaxRows:  MaxLimit,
		Warnings: warnings,
	}, nil
}

// deviceScopeParam is the query parameter that restricts an edge endpoint to a
// device set. dcim/interfaces and dcim/cables both accept it, repeated values
// OR together, and both were verified honored by comparing the list envelope's
// count against the unfiltered one — NetBox answers an unknown filter with HTTP
// 200 and the FULL result set, so a no-op filter is otherwise indistinguishable
// from a working one.
const deviceScopeParam = "device_id"

// fetchEdgeRows reads an edge endpoint restricted to the topology's own device
// set, in device order.
//
// The scoping is a correctness fix, not an optimisation. Both edge endpoints
// used to be fetched UNSCOPED and capped at MaxLimit rows, which made the edge
// source a fixed prefix of the whole database — on a multi-million-device instance,
// dcim/interfaces?connected=true ordered by device name yielded interfaces for
// ~390 devices (0.006%), and dcim/cables 10,000 of 1,197,300 (0.84%). An edge is
// only emitted when its NEAR end came from that fetch, so any node set outside
// that window produced ZERO edges no matter how densely cabled it was: a
// site-filtered topology rendered 200 nodes and 0 edges in 476s where the true
// answer was 6 edges in one request. Silent, and indistinguishable from "these
// devices are not connected".
//
// Batching reuses ipenrich.go's byte budget rather than a second mechanism,
// because the ceiling is the same one (a server URL-length limit, measured at
// ~8 KB) and a count-based cap tuned on short ids would break on long ones.
//
// A batch that overflows the row cap is SPLIT IN HALF and both halves
// re-queried, exactly as fetchAddressRecords does, because the cap is what this
// function exists to stop hitting: ~340 device ids fit one batch, and at the
// measured ~51 cables per device that is 17,000 cables against a 10,000-row cap.
// Nothing is kept from an overflowing batch — the halves re-read all of it — so
// no row can be counted twice, and every split strictly shrinks the batch, so
// this terminates at a single device. A single device with more rows than one
// request can carry is the one case left; it is logged, since there is nothing
// to split.
//
// An overflowing batch is only discovered after its pages have been walked, so
// a split costs the pages already read. That is deliberate: the alternative is a
// count probe before every batch, which would add a request to every topology
// query — including the small ones that never overflow — to save requests on the
// rare large one. Overflow needs a physical view of several hundred densely
// cabled devices to happen at all, and the answer is right either way.
//
// Order is preserved: batches are seeded on a stack in reverse and a split
// pushes its halves so the first half pops first, so rows come back in the same
// relative order the unscoped fetch delivered them. That is what keeps the
// emitted edge order — which is observable in the node graph — unchanged on an
// instance small enough for one batch, i.e. every instance the old code was
// already right about.
// fetchEdgeRows returns the edge rows for a device set, and whether it had to
// drop any links to stay inside the row cap.
func (p *Provider) fetchEdgeRows(ctx context.Context, objectType string, base url.Values, deviceIDs []string) (_ []json.RawMessage, capped bool, _ error) {
	if len(deviceIDs) == 0 {
		return nil, false, nil
	}

	// Through queryBatcher, not chunkByBudget directly: base is a fixed parameter
	// set appended to every request, and budgeting the device ids alone measured
	// something shorter than what went on the wire. With base {"connected":"true"}
	// and 900 devices that put the query 12 bytes past the ceiling — small, and
	// still the accounting gap the batcher exists to close. Using it here also
	// means a future caller adding to base cannot reintroduce the overflow, since
	// the batcher builds the request from the same set it measured.
	batcher := newQueryBatcher(deviceScopeParam, base)
	chunks := batcher.chunk(deviceIDs)
	work := make([][]string, 0, len(chunks))
	for i := len(chunks) - 1; i >= 0; i-- {
		work = append(work, chunks[i])
	}

	var out []json.RawMessage
	for len(work) > 0 {
		ids := work[len(work)-1]
		work = work[:len(work)-1]

		rows, total, err := p.fetchRows(ctx, objectType, batcher.query(ids), MaxLimit)
		if err != nil {
			return nil, false, err
		}
		if total > len(rows) {
			if len(ids) > 1 {
				mid := len(ids) / 2
				work = append(work, ids[mid:], ids[:mid])
				continue
			}
			log.DefaultLogger.Warn(
				"topology: edge lookup hit the row cap for a single device; some links are missing",
				"endpoint", logSafe(objectType), "device_id", ids[0], "read", len(rows), "reported", total, "cap", MaxLimit,
			)
			// Also reported to the caller. A log line is enough for an operator
			// looking at a diagram; it is not enough for an alert rule deciding
			// whether a device still has a working path, which cannot see logs.
			capped = true
		}
		out = append(out, rows...)
	}
	return out, capped, nil
}

// termination is a cable termination; interface and panel-port (front/rear)
// terminations resolve to their device.
type termination struct {
	ObjectType string `json:"object_type"`
	Object     struct {
		Device struct {
			ID int `json:"id"`
		} `json:"device"`
	} `json:"object"`
}

// devicesFromTerminations returns every DISTINCT device a cable end lands on,
// in termination order.
//
// A cable end is a list because one cable can terminate on several ports — a
// breakout is the common case. Taking only the first device silently discards
// the rest, and a discarded peer may be the healthy upstream: the suppression
// recipe then reads the remaining down peer as the whole neighbour set and
// stops a page. Returning them all costs one edge per real connection, which is
// also what the node graph should have been drawing.
func devicesFromTerminations(ts []termination) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range ts {
		switch t.ObjectType {
		case "dcim.interface", "dcim.frontport", "dcim.rearport":
			if t.Object.Device.ID == 0 {
				continue
			}
			id := strconv.Itoa(t.Object.Device.ID)
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// linkKey is a link's identity: the sorted pair of its end-interface IDs.
// Both directions of the same path (and a wireless link vs. its own computed
// path) collapse onto one key, while parallel links between the same devices
// keep distinct keys.
func linkKey(a, b int) string {
	if a > b {
		a, b = b, a
	}
	return strconv.Itoa(a) + "-" + strconv.Itoa(b)
}

// logicalEdges derives device-to-device edges from NetBox's computed cable
// paths (interface connected_endpoints): patch panels and circuits resolve to
// the far device. Each path is seen from both end interfaces — dedup by link
// identity, so parallel paths between the same device pair all render.
// The fetch is scoped to deviceIDs (the node set) because only an interface on
// an IN-SET device can start an edge — the `!inSet[a]` test below already threw
// every other row away, so scoping loses nothing and is what stops the row cap
// truncating the fetch to an unrelated slice of the database.
func (p *Provider) logicalEdges(ctx context.Context, inSet map[string]bool, deviceIDs []string, seen map[string]bool, keepBoundary bool) (_ []provider.GraphEdge, capped bool, _ error) {
	rows, capped, err := p.fetchEdgeRows(ctx, "dcim/interfaces", url.Values{"connected": {"true"}}, deviceIDs)
	if err != nil {
		return nil, false, err
	}
	var edges []provider.GraphEdge
	for _, raw := range rows {
		var i struct {
			ID     int `json:"id"`
			Device struct {
				ID int `json:"id"`
			} `json:"device"`
			EndpointsType string `json:"connected_endpoints_type"`
			Reachable     bool   `json:"connected_endpoints_reachable"`
			Endpoints     []struct {
				ID     int `json:"id"`
				Device struct {
					ID int `json:"id"`
				} `json:"device"`
			} `json:"connected_endpoints"`
		}
		if json.Unmarshal(raw, &i) != nil || i.ID == 0 || i.Device.ID == 0 {
			continue
		}
		if i.EndpointsType != "dcim.interface" || !i.Reachable {
			continue
		}
		a := strconv.Itoa(i.Device.ID)
		for _, ep := range i.Endpoints {
			if ep.ID == 0 || ep.Device.ID == 0 {
				continue
			}
			b := strconv.Itoa(ep.Device.ID)
			if a == b || !edgeInScope(inSet, a, b, keepBoundary) {
				continue
			}
			k := linkKey(i.ID, ep.ID)
			if seen[k] {
				continue
			}
			seen[k] = true
			edges = append(edges, provider.GraphEdge{ID: "p" + k, Source: a, Target: b, Kind: "path"})
		}
	}
	return edges, capped, nil
}

// physicalEdges derives one edge per raw cable — parallel cables between the
// same device pair all render. Front/rear-port terminations resolve to their
// device (the patch panel), so panel-cabled fabrics render device—panel—device
// instead of dropping the link.
//
// The fetch is scoped to deviceIDs (the node set): a cable both of whose ends
// are in the set is matched by either end, so nothing is lost. Because a cable
// spanning two BATCHES is returned by both, cables are deduped by their own
// NetBox id — which is the identity of a cable, so this can never merge two
// distinct parallel cables, and on a node set small enough for one batch (where
// NetBox returns each cable once) it removes nothing at all.
func (p *Provider) physicalEdges(ctx context.Context, inSet map[string]bool, deviceIDs []string, keepBoundary bool) (_ []provider.GraphEdge, capped bool, _ error) {
	rows, capped, err := p.fetchEdgeRows(ctx, "dcim/cables", url.Values{}, deviceIDs)
	if err != nil {
		return nil, false, err
	}
	var edges []provider.GraphEdge
	seenCable := map[int]bool{}
	for _, raw := range rows {
		var c struct {
			ID int           `json:"id"`
			A  []termination `json:"a_terminations"`
			B  []termination `json:"b_terminations"`
		}
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		if seenCable[c.ID] {
			continue
		}
		seenCable[c.ID] = true

		// Every cross pair, not just the first: a multi-termination cable
		// connects each device on one end to each device on the other, and
		// dropping any of them hides a real neighbour.
		for _, a := range devicesFromTerminations(c.A) {
			for _, b := range devicesFromTerminations(c.B) {
				if a == b || !edgeInScope(inSet, a, b, keepBoundary) {
					continue
				}
				id := strconv.Itoa(c.ID)
				if len(c.A) > 1 || len(c.B) > 1 {
					// Edge ids must stay unique once one cable yields several.
					id += "-" + a + "-" + b
				}
				edges = append(edges, provider.GraphEdge{ID: id, Source: a, Target: b, Kind: "cable"})
			}
		}
	}
	return edges, capped, nil
}

// wirelessEdges links the two ends of each NetBox wireless link, marking the
// link identity (interface pair) as seen so the logical view doesn't repeat
// the same link as a computed path. Fetch failures are non-fatal: wired
// topology still renders.
//
// This is the ONE edge fetch that stays unscoped, and not by choice:
// wireless/wireless-links has no device filter at all — its OpenAPI parameter
// list (NetBox 4.4) offers only interface_a_id/interface_b_id, and this view
// holds device ids, not interface ids. Scoping by interface would need an extra
// interface fetch that the physical view does not otherwise make, to filter a
// table that is orders of magnitude smaller than the two that were actually
// truncating (0 rows on the multi-million-device instance measured, against 1,197,300
// cables), so it is left as it was rather than paid for speculatively. The
// residual is the same in kind: an instance with more than MaxLimit wireless
// links can lose wireless edges outside the first 10,000.
// wirelessEdges returns wireless links, and whether the set is incomplete —
// either because the lookup failed or because it hit the row cap.
//
// It previously returned nil on error, which is right for a diagram (draw what
// you have) and wrong for an alert rule: a device whose healthy path is
// wireless would appear to have only its failing wired upstream.
func (p *Provider) wirelessEdges(ctx context.Context, inSet map[string]bool, seen map[string]bool, keepBoundary bool) (_ []provider.GraphEdge, incomplete bool) {
	rows, total, err := p.fetchRows(ctx, "wireless/wireless-links", url.Values{}, MaxLimit)
	if err != nil {
		return nil, true
	}
	if total > len(rows) {
		incomplete = true
	}
	var edges []provider.GraphEdge
	for _, raw := range rows {
		var wl struct {
			ID int `json:"id"`
			A  struct {
				ID     int `json:"id"`
				Device struct {
					ID int `json:"id"`
				} `json:"device"`
			} `json:"interface_a"`
			B struct {
				ID     int `json:"id"`
				Device struct {
					ID int `json:"id"`
				} `json:"device"`
			} `json:"interface_b"`
		}
		if json.Unmarshal(raw, &wl) != nil || wl.A.Device.ID == 0 || wl.B.Device.ID == 0 {
			continue
		}
		a, b := strconv.Itoa(wl.A.Device.ID), strconv.Itoa(wl.B.Device.ID)
		if a == b || !edgeInScope(inSet, a, b, keepBoundary) {
			continue
		}
		k := linkKey(wl.A.ID, wl.B.ID)
		if seen[k] {
			continue
		}
		seen[k] = true
		edges = append(edges, provider.GraphEdge{ID: "w" + strconv.Itoa(wl.ID), Source: a, Target: b, Kind: "wireless"})
	}
	return edges, incomplete
}

// edgeInScope decides whether an edge belongs in the result.
//
// The node-graph view keeps only edges with BOTH ends inside the filtered set,
// so nothing dangles off the picture. A caller asking about a device's
// neighbours needs the opposite: an edge with one end in scope names a real
// neighbour, and dropping it hides exactly the peer that might be healthy —
// which for the suppression recipe reads as "no working path" and silences a
// page. Per-site filtering, which that recipe recommends, is precisely when a
// link leaves the set.
func edgeInScope(inSet map[string]bool, a, b string, keepBoundary bool) bool {
	if keepBoundary {
		return inSet[a] || inSet[b]
	}
	return inSet[a] && inSet[b]
}

// boundaryPeerNodes fetches the devices that edges reference but the filter did
// not select, so a row naming them carries their name, role and site.
func (p *Provider) boundaryPeerNodes(ctx context.Context, inSet map[string]bool, edges []provider.GraphEdge) (_ []provider.GraphNode, missingPeers int, _ error) {
	missing := map[string]bool{}
	for _, e := range edges {
		for _, id := range []string{e.Source, e.Target} {
			if id != "" && !inSet[id] {
				missing[id] = true
			}
		}
	}
	if len(missing) == 0 {
		return nil, 0, nil
	}
	ids := make([]string, 0, len(missing))
	for id := range missing {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	batcher := newQueryBatcher("id", url.Values{})
	var out []provider.GraphNode
	for _, chunk := range batcher.chunk(ids) {
		rows, _, err := p.fetchRows(ctx, "dcim/devices", batcher.query(chunk), MaxLimit)
		if err != nil {
			return nil, 0, err
		}
		for _, raw := range rows {
			if n, ok := deviceNode(raw); ok {
				n.Boundary = true
				out = append(out, n)
			}
		}
	}

	// A peer that does not come back — hidden by object permissions, or deleted
	// between the edge lookup and this one — takes its edge with it, because the
	// frame cannot name an endpoint it has no device for. That silently removes
	// a neighbour, and a removed neighbour may be the healthy one, so the caller
	// has to be told rather than handed a quietly shorter answer.
	resolved := make(map[string]bool, len(out))
	for _, n := range out {
		resolved[n.ID] = true
	}
	for id := range missing {
		if !resolved[id] {
			missingPeers++
		}
	}
	return out, missingPeers, nil
}

// deviceNode decodes one dcim/devices row into a graph node. Shared by the
// filtered device set and the boundary-peer fetch so the two cannot decode the
// same object differently.
func deviceNode(raw json.RawMessage) (provider.GraphNode, bool) {
	var d struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Site struct {
			Name string `json:"name"`
		} `json:"site"`
		Role struct {
			Name string `json:"name"`
		} `json:"role"`
		Status struct {
			Value string `json:"value"`
		} `json:"status"`
		DisplayURL string `json:"display_url"`
	}
	if json.Unmarshal(raw, &d) != nil || d.ID == 0 {
		return provider.GraphNode{}, false
	}
	return provider.GraphNode{
		ID:     strconv.Itoa(d.ID),
		Title:  d.Name,
		Site:   d.Site.Name,
		Role:   d.Role.Name,
		Status: d.Status.Value,
		URL:    d.DisplayURL,
	}, true
}
