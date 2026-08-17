package netbox

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

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
	limit := spec.Limit
	if limit <= 0 || limit > MaxLimit {
		limit = 1000
	}

	devRows, _, err := p.fetchRows(ctx, "dcim/devices", buildFilterValues(spec.Filters), limit)
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
			continue
		}
		id := strconv.Itoa(d.ID)
		if !inSet[id] {
			inSet[id] = true
			nodeIDs = append(nodeIDs, id)
		}
		nodes = append(nodes, provider.GraphNode{
			ID:       id,
			Title:    d.Name,
			SubTitle: d.Site.Name,
			MainStat: d.Role.Name,
			Status:   d.Status.Value,
			URL:      d.DisplayURL,
		})
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
	edges := p.wirelessEdges(ctx, inSet, seen)
	if spec.Connections == "physical" {
		physical, err := p.physicalEdges(ctx, inSet, nodeIDs)
		if err != nil {
			return nil, err
		}
		edges = append(edges, physical...)
	} else {
		logical, err := p.logicalEdges(ctx, inSet, nodeIDs, seen)
		if err != nil {
			return nil, err
		}
		edges = append(edges, logical...)
	}

	connected := map[string]bool{}
	for _, e := range edges {
		connected[e.Source] = true
		connected[e.Target] = true
	}

	if spec.ConnectedOnly {
		kept := nodes[:0]
		for _, n := range nodes {
			if connected[n.ID] {
				kept = append(kept, n)
			}
		}
		nodes = kept
	}

	return &provider.Graph{Nodes: nodes, Edges: edges}, nil
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
func (p *Provider) fetchEdgeRows(ctx context.Context, objectType string, base url.Values, deviceIDs []string) ([]json.RawMessage, error) {
	if len(deviceIDs) == 0 {
		return nil, nil
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
			return nil, err
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
		}
		out = append(out, rows...)
	}
	return out, nil
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

func deviceFromTerminations(ts []termination) string {
	for _, t := range ts {
		switch t.ObjectType {
		case "dcim.interface", "dcim.frontport", "dcim.rearport":
			if t.Object.Device.ID != 0 {
				return strconv.Itoa(t.Object.Device.ID)
			}
		}
	}
	return ""
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
func (p *Provider) logicalEdges(ctx context.Context, inSet map[string]bool, deviceIDs []string, seen map[string]bool) ([]provider.GraphEdge, error) {
	rows, err := p.fetchEdgeRows(ctx, "dcim/interfaces", url.Values{"connected": {"true"}}, deviceIDs)
	if err != nil {
		return nil, err
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
			if a == b || !inSet[a] || !inSet[b] {
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
	return edges, nil
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
func (p *Provider) physicalEdges(ctx context.Context, inSet map[string]bool, deviceIDs []string) ([]provider.GraphEdge, error) {
	rows, err := p.fetchEdgeRows(ctx, "dcim/cables", url.Values{}, deviceIDs)
	if err != nil {
		return nil, err
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
		a := deviceFromTerminations(c.A)
		b := deviceFromTerminations(c.B)
		if a == "" || b == "" || a == b || !inSet[a] || !inSet[b] {
			continue
		}
		if seenCable[c.ID] {
			continue
		}
		seenCable[c.ID] = true
		edges = append(edges, provider.GraphEdge{ID: strconv.Itoa(c.ID), Source: a, Target: b, Kind: "cable"})
	}
	return edges, nil
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
func (p *Provider) wirelessEdges(ctx context.Context, inSet map[string]bool, seen map[string]bool) []provider.GraphEdge {
	rows, _, err := p.fetchRows(ctx, "wireless/wireless-links", url.Values{}, MaxLimit)
	if err != nil {
		return nil
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
		if a == b || !inSet[a] || !inSet[b] {
			continue
		}
		k := linkKey(wl.A.ID, wl.B.ID)
		if seen[k] {
			continue
		}
		seen[k] = true
		edges = append(edges, provider.GraphEdge{ID: "w" + strconv.Itoa(wl.ID), Source: a, Target: b, Kind: "wireless"})
	}
	return edges
}
