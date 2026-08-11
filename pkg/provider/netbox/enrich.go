package netbox

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/netboxlabs/netbox/pkg/provider"
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
		inSet[id] = true
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
		physical, err := p.physicalEdges(ctx, inSet)
		if err != nil {
			return nil, err
		}
		edges = append(edges, physical...)
	} else {
		logical, err := p.logicalEdges(ctx, inSet, seen)
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
func (p *Provider) logicalEdges(ctx context.Context, inSet map[string]bool, seen map[string]bool) ([]provider.GraphEdge, error) {
	rows, _, err := p.fetchRows(ctx, "dcim/interfaces", url.Values{"connected": {"true"}}, MaxLimit)
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
func (p *Provider) physicalEdges(ctx context.Context, inSet map[string]bool) ([]provider.GraphEdge, error) {
	rows, _, err := p.fetchRows(ctx, "dcim/cables", url.Values{}, MaxLimit)
	if err != nil {
		return nil, err
	}
	var edges []provider.GraphEdge
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
		edges = append(edges, provider.GraphEdge{ID: strconv.Itoa(c.ID), Source: a, Target: b, Kind: "cable"})
	}
	return edges, nil
}

// wirelessEdges links the two ends of each NetBox wireless link, marking the
// link identity (interface pair) as seen so the logical view doesn't repeat
// the same link as a computed path. Fetch failures are non-fatal: wired
// topology still renders.
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
