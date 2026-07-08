package netbox

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// defaultPrefixFields are the columns returned by ResolveIPs when the caller
// does not specify any.
var defaultPrefixFields = []string{"prefix", "site", "tenant", "role", "vrf", "vlan", "description"}

// buildFilterValues turns provider filters into NetBox query params, expanding
// CSV values (from multi-value variables) into repeated params (OR).
func buildFilterValues(filters []provider.Filter) url.Values {
	q := url.Values{}
	for _, f := range filters {
		if f.Field == "" {
			continue
		}
		if f.Operator == "empty" {
			q.Set(f.Field+"__empty", "true")
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

// ResolveIPs maps each input IP to the context of its longest-matching NetBox
// prefix, returning a table keyed by "ip". Uses NetBox's `?contains=<ip>` filter,
// which a value-equality join can't replicate.
func (p *Provider) ResolveIPs(ctx context.Context, ips []string, fields []string, limit int) (*provider.Result, error) {
	if limit <= 0 || limit > pageSize {
		limit = 200
	}
	if len(fields) == 0 {
		fields = defaultPrefixFields
	}

	columns := []string{"ip"}
	colSeen := map[string]bool{"ip": true}
	addCol := func(c string) {
		if !colSeen[c] {
			colSeen[c] = true
			columns = append(columns, c)
		}
	}
	for _, f := range fields {
		addCol(f)
	}

	seen := map[string]bool{}
	var rows []map[string]interface{}
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		if len(rows) >= limit {
			break
		}

		row := map[string]interface{}{"ip": ip}
		q := url.Values{}
		q.Set("contains", ip)
		q.Set("limit", "100")
		var page listPage
		if err := p.client.getJSON(ctx, p.client.apiURL("ipam/prefixes", q), &page); err == nil {
			if best := pickLongestPrefix(page.Results); best != nil {
				if _, vals, err := flattenObject(best); err == nil {
					for _, f := range fields {
						if v, ok := vals[f]; ok {
							row[f] = v
						}
					}
				}
			}
		}
		rows = append(rows, row)
	}

	return &provider.Result{Columns: columns, Rows: rows}, nil
}

// pickLongestPrefix returns the raw prefix object with the longest mask.
func pickLongestPrefix(results []json.RawMessage) json.RawMessage {
	var best json.RawMessage
	bestLen := -1
	for _, r := range results {
		var o struct {
			Prefix string `json:"prefix"`
		}
		if json.Unmarshal(r, &o) != nil {
			continue
		}
		idx := strings.LastIndex(o.Prefix, "/")
		if idx < 0 {
			continue
		}
		l, err := strconv.Atoi(o.Prefix[idx+1:])
		if err != nil {
			continue
		}
		if l > bestLen {
			bestLen = l
			best = r
		}
	}
	return best
}

// Topology returns devices as nodes and inter-device cables as edges.
func (p *Provider) Topology(ctx context.Context, spec provider.TopologySpec) (*provider.Graph, error) {
	limit := spec.Limit
	if limit <= 0 || limit > maxLimit {
		limit = 1000
	}

	devRows, err := p.fetchRows(ctx, "dcim/devices", buildFilterValues(spec.Filters), limit)
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
		})
	}

	cabRows, err := p.fetchRows(ctx, "dcim/cables", url.Values{}, maxLimit)
	if err != nil {
		return nil, err
	}
	var edges []provider.GraphEdge
	connected := map[string]bool{}
	for _, raw := range cabRows {
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
		edges = append(edges, provider.GraphEdge{ID: strconv.Itoa(c.ID), Source: a, Target: b})
		connected[a] = true
		connected[b] = true
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

// termination is a cable termination; only interface terminations resolve to a
// device.
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
		if t.ObjectType == "dcim.interface" && t.Object.Device.ID != 0 {
			return strconv.Itoa(t.Object.Device.ID)
		}
	}
	return ""
}
