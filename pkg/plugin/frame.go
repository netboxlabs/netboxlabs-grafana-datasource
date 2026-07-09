package plugin

import (
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// timeColumns are column names whose RFC3339 string values are rendered as time
// fields.
var timeColumns = map[string]bool{
	"created": true, "last_updated": true, "last_used": true, "expires": true,
}

// buildFrame converts a flattened provider.Result into a table-shaped data frame
// whose columns are typed and joinable. The frame is keyed for Grafana's
// "Outer join" transformation: string key columns (name, device, address, …)
// keep their natural names so they line up with metric labels.
func buildFrame(objectType string, res *provider.Result, baseURL string) *data.Frame {
	frame := data.NewFrame(frameName(objectType))
	frame.Meta = &data.FrameMeta{PreferredVisualization: data.VisTypeTable}

	for _, col := range res.Columns {
		frame.Fields = append(frame.Fields, buildField(col, res.Rows))
	}

	addObjectDataLinks(frame, res.Columns)
	return frame
}

// frameName derives a stable frame/refId-friendly name, e.g. "dcim/devices" ->
// "netbox_devices".
func frameName(objectType string) string {
	parts := strings.Split(objectType, "/")
	last := parts[len(parts)-1]
	return "netbox_" + strings.ReplaceAll(last, "-", "_")
}

// buildField creates a typed data.Field for a column by scanning its values.
func buildField(name string, rows []map[string]interface{}) *data.Field {
	switch classifyColumn(name, rows) {
	case colBool:
		vals := make([]*bool, len(rows))
		for i, r := range rows {
			if b, ok := r[name].(bool); ok {
				vals[i] = &b
			}
		}
		return data.NewField(name, nil, vals)
	case colNumber:
		vals := make([]*float64, len(rows))
		for i, r := range rows {
			if f, ok := r[name].(float64); ok {
				vals[i] = &f
			}
		}
		return data.NewField(name, nil, vals)
	case colTime:
		vals := make([]*time.Time, len(rows))
		for i, r := range rows {
			if s, ok := r[name].(string); ok && s != "" {
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					vals[i] = &t
				}
			}
		}
		return data.NewField(name, nil, vals)
	default:
		// Strings are non-nullable for clean joins; nil becomes "".
		vals := make([]string, len(rows))
		for i, r := range rows {
			vals[i] = toString(r[name])
		}
		return data.NewField(name, nil, vals)
	}
}

type colKind int

const (
	colString colKind = iota
	colNumber
	colBool
	colTime
)

func classifyColumn(name string, rows []map[string]interface{}) colKind {
	hasValue, allBool, allNumber := false, true, true
	for _, r := range rows {
		v, ok := r[name]
		if !ok || v == nil {
			continue
		}
		hasValue = true
		switch v.(type) {
		case bool:
			allNumber = false
		case float64:
			allBool = false
		default:
			allBool, allNumber = false, false
		}
	}
	if !hasValue {
		return colString
	}
	if allBool {
		return colBool
	}
	if allNumber {
		return colNumber
	}
	if timeColumns[name] {
		for _, r := range rows {
			if s, ok := r[name].(string); ok && s != "" {
				if _, err := time.Parse(time.RFC3339, s); err == nil {
					return colTime
				}
				break
			}
		}
	}
	return colString
}

// addObjectDataLinks attaches a "View in NetBox" deep link to the primary label
// column, using each row's display_url. The link survives joins, so a metrics
// table enriched with NetBox columns stays clickable through to NetBox.
func addObjectDataLinks(frame *data.Frame, columns []string) {
	if !contains(columns, "display_url") {
		return
	}
	labelCol := firstPresent(columns, "name", "display", "address", "prefix")
	if labelCol == "" {
		return
	}
	for _, f := range frame.Fields {
		if f.Name != labelCol {
			continue
		}
		if f.Config == nil {
			f.Config = &data.FieldConfig{}
		}
		f.Config.Links = append(f.Config.Links, data.DataLink{
			Title:       "View in NetBox",
			URL:         `${__data.fields["display_url"]}`,
			TargetBlank: true,
		})
	}
}

// buildAnnotationsFrame builds a frame whose field names follow Grafana's
// annotation convention (time/title/text/tags), so changelog events render as
// annotations on any time-series panel.
func buildAnnotationsFrame(changes []provider.Change) *data.Frame {
	times := make([]time.Time, 0, len(changes))
	titles := make([]string, 0, len(changes))
	texts := make([]string, 0, len(changes))
	tags := make([]string, 0, len(changes))

	for _, c := range changes {
		times = append(times, c.Time)
		titles = append(titles, c.ObjectRepr)
		text := c.Action
		if c.User != "" {
			text = fmt.Sprintf("%s by %s", c.Action, c.User)
		}
		texts = append(texts, text)
		tagParts := []string{"netbox"}
		if c.ObjectType != "" {
			tagParts = append(tagParts, c.ObjectType)
		}
		if c.Action != "" {
			tagParts = append(tagParts, strings.ToLower(c.Action))
		}
		tags = append(tags, strings.Join(tagParts, ","))
	}

	frame := data.NewFrame("netbox_changes",
		data.NewField("time", nil, times),
		data.NewField("title", nil, titles),
		data.NewField("text", nil, texts),
		data.NewField("tags", nil, tags),
	)
	return frame
}

// buildNodeGraphFrames builds the two frames (nodes + edges) Grafana's node
// graph panel expects, with nodes colored by NetBox operational status.
func buildNodeGraphFrames(g *provider.Graph) data.Frames {
	n := len(g.Nodes)
	ids := make([]string, n)
	titles := make([]string, n)
	subs := make([]string, n)
	stats := make([]string, n)
	colors := make([]string, n)
	for i, node := range g.Nodes {
		ids[i] = node.ID
		titles[i] = node.Title
		subs[i] = node.SubTitle
		stats[i] = node.MainStat
		colors[i] = statusColor(node.Status)
	}
	nodes := data.NewFrame("nodes",
		data.NewField("id", nil, ids),
		data.NewField("title", nil, titles),
		data.NewField("subTitle", nil, subs),
		data.NewField("mainStat", nil, stats),
		data.NewField("color", nil, colors),
	)
	nodes.Meta = &data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph}

	m := len(g.Edges)
	eid := make([]string, m)
	esrc := make([]string, m)
	etgt := make([]string, m)
	for i, e := range g.Edges {
		eid[i] = e.ID
		esrc[i] = e.Source
		etgt[i] = e.Target
	}
	edges := data.NewFrame("edges",
		data.NewField("id", nil, eid),
		data.NewField("source", nil, esrc),
		data.NewField("target", nil, etgt),
	)
	edges.Meta = &data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph}

	return data.Frames{nodes, edges}
}

// statusColor maps a NetBox device status to a node color.
func statusColor(status string) string {
	switch status {
	case "active":
		return "#73BF69" // green
	case "offline", "failed", "decommissioning":
		return "#F2495C" // red
	case "staged", "planned", "inventory":
		return "#5794F2" // blue
	default:
		return "#8E8E8E" // grey
	}
}

func toString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		// Render whole numbers without a trailing ".0".
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		return fmt.Sprintf("%t", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func firstPresent(cols []string, candidates ...string) string {
	for _, c := range candidates {
		if contains(cols, c) {
			return c
		}
	}
	return ""
}

// buildCountFrame returns a single-value numeric frame (no time column) holding
// the row count. Grafana-managed alert rules can threshold this directly, without
// a Reduce step — the fallback for when a plain table frame is not reducible.
func buildCountFrame(objectType string, n int) *data.Frame {
	return data.NewFrame(frameName(objectType),
		data.NewField("count", nil, []float64{float64(n)}),
	)
}
