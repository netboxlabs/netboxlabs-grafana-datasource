package plugin

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
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
		frame.Fields = append(frame.Fields, buildField(col, res.Rows, res.ColumnTypes))
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

// buildField creates a typed data.Field for a column by scanning its values:
// numbers → nullable float, booleans → nullable bool, known timestamp columns →
// time, everything else → non-nullable string. types carries the producer's
// declared types for columns whose type is known independently of the values
// (see provider.Result.ColumnTypes); it is nil for producers that declare none.
func buildField(name string, rows []map[string]interface{}, types map[string]provider.FieldType) *data.Field {
	switch classifyColumn(name, rows, types) {
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

// declaredKinds maps the provider seam's backend-agnostic column types onto the
// frame kinds buildField emits. A producer that declares a type this table does
// not cover falls through to inference, so the seam can grow a FieldType without
// breaking frame building.
var declaredKinds = map[provider.FieldType]colKind{
	provider.FieldTypeString:  colString,
	provider.FieldTypeNumber:  colNumber,
	provider.FieldTypeBoolean: colBool,
	provider.FieldTypeTime:    colTime,
}

// classifyColumn picks a column's frame type. Values decide it whenever there
// are any; a producer-declared type (types, from provider.Result.ColumnTypes)
// decides the one case values cannot — a column that is null in every row.
//
// The declared type is deliberately NOT allowed to override observed values. A
// hint that contradicts the data would mean building a field of the declared
// type and dropping every value that does not fit it, so a stale declaration
// would delete data rather than mistype it. Inference alone is right for
// discovered object columns, whose types are only knowable from the values;
// the declaration is for fixed schemas (ip-enrichment) where a column has a type
// even on the refresh where it has no values.
func classifyColumn(name string, rows []map[string]interface{}, types map[string]provider.FieldType) colKind {
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
		// Nothing to infer from. Reading types on a nil map yields the zero
		// FieldType, which is not in declaredKinds, so an undeclared column keeps
		// the historical string fallback.
		if k, ok := declaredKinds[types[name]]; ok {
			return k
		}
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

// Annotation rendering limits. A marker shows provider.MaxDiffLines diff lines.
const (
	maxDiffValueLen  = 60
	maxJournalLength = 200
)

// buildAnnotationsFrame builds a frame whose field names follow Grafana's
// annotation convention (time/title/text/tags), so changelog events render as
// annotations on any time-series panel. Title and text are HTML, so every
// NetBox-supplied value is escaped. The id field is always 0: a marker with an
// id makes Grafana's tooltip offer Edit, which would save a copy.
func buildAnnotationsFrame(changes []provider.Change) *data.Frame {
	times := make([]time.Time, 0, len(changes))
	titles := make([]string, 0, len(changes))
	texts := make([]string, 0, len(changes))
	tags := make([]string, 0, len(changes))
	ids := make([]int64, 0, len(changes))

	for _, c := range changes {
		times = append(times, c.Time)
		ids = append(ids, 0)
		var title, text, tag string
		switch c.Kind {
		case provider.ChangeKindJournal:
			title, text, tag = journalMarker(c)
		case provider.ChangeKindCutoff:
			title, text, tag = cutoffMarker(c)
		case provider.ChangeKindLimit:
			title, text, tag = limitMarker(c)
		case provider.ChangeKindDescendantsError:
			// The track around it is intact, so it isn't titled like a failed query.
			title, text, tag = "NetBox annotation warning", html.EscapeString(c.Message), "netbox,error"
		default:
			title, text, tag = changeMarker(c)
		}
		titles = append(titles, title)
		texts = append(texts, text)
		tags = append(tags, tag)
	}

	return data.NewFrame("netbox_changes",
		data.NewField("time", nil, times),
		data.NewField("title", nil, titles),
		data.NewField("text", nil, texts),
		data.NewField("tags", nil, tags),
		data.NewField("id", nil, ids),
	)
}

func changeMarker(c provider.Change) (title, text, tag string) {
	title = html.EscapeString(c.ObjectRepr)
	if c.Parent != "" {
		title += " on " + html.EscapeString(c.Parent)
	}

	var lines []string
	for i, d := range c.Diff {
		if i == provider.MaxDiffLines {
			lines = append(lines, fmt.Sprintf("+%d more", len(c.Diff)-provider.MaxDiffLines))
			break
		}
		lines = append(lines, fmt.Sprintf("%s: %s → %s",
			html.EscapeString(d.Field), diffValue(d.Before), diffValue(d.After)))
	}
	if c.Message != "" {
		lines = append(lines, `"`+html.EscapeString(c.Message)+`"`)
	}
	who := html.EscapeString(c.Action)
	if c.User != "" {
		who += " by " + html.EscapeString(c.User)
	}
	lines = append(lines, who+markerLink(c.URL))
	text = strings.Join(lines, "<br>")

	tagParts := []string{"netbox"}
	if c.ObjectType != "" {
		tagParts = append(tagParts, c.ObjectType)
	}
	if c.Action != "" {
		tagParts = append(tagParts, strings.ToLower(c.Action))
	}
	return title, text, strings.Join(tagParts, ",")
}

func journalMarker(c provider.Change) (title, text, tag string) {
	who := "Journal"
	if c.User != "" {
		who += " by " + html.EscapeString(c.User)
	}
	// CRLF (from NetBox's UI) and a lone CR are line ends too.
	comments := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(c.Comments)
	comments = strings.ReplaceAll(html.EscapeString(truncateRunes(comments, maxJournalLength)), "\n", "<br>")
	return html.EscapeString(c.ObjectRepr),
		comments + "<br>" + who + markerLink(c.URL),
		"netbox,journal," + c.JournalKind
}

func cutoffMarker(c provider.Change) (title, text, tag string) {
	text = fmt.Sprintf("Older changes not checked: read %s change-log entries without reaching the start of the range.", thousands(c.Scanned))
	if c.DescendantsOnly {
		text = fmt.Sprintf("Older changes to what's attached not checked: read %s change-log entries without reaching the start of the range. The scope's own changes were all read.", thousands(c.Scanned))
	}
	return "NetBox change log", text, "netbox,cutoff"
}

// limitMarker says the query's Limit hid older changes in the range, and from
// when. Past the largest Limit there is nothing to raise.
func limitMarker(c provider.Change) (title, text, tag string) {
	at := c.Until.UTC().Format("2006-01-02 15:04:05 UTC")
	text = fmt.Sprintf("Limit reached: changes before %s in this range aren't shown. Raise Limit (now %d), narrow the range, or narrow Object types.", at, c.Limit)
	if c.Limit >= netbox.MaxLimit {
		text = fmt.Sprintf("Limit reached: changes before %s in this range aren't shown. Narrow the range or Object types.", at)
	}
	return "NetBox change log", text, "netbox,limit"
}

// annotationErrorFrame is a failed annotation query's error as one marker at
// at, in buildAnnotationsFrame's shape. Grafana's scenes dashboards drop an
// annotation query's error, so without it a failure looks like a quiet window.
// The message can quote a variable's value, so it is escaped.
func annotationErrorFrame(msg string, at time.Time) *data.Frame {
	return data.NewFrame("netbox_changes",
		data.NewField("time", nil, []time.Time{at}),
		data.NewField("title", nil, []string{"NetBox annotation error"}),
		data.NewField("text", nil, []string{html.EscapeString(msg)}),
		data.NewField("tags", nil, []string{"netbox,error"}),
		data.NewField("id", nil, []int64{0}),
	)
}

// diffValue formats one side of a field change: empty becomes a dash, newlines
// a space, and long values are cut.
func diffValue(v string) string {
	if v == "" {
		return "—"
	}
	v = strings.ReplaceAll(v, "\n", " ")
	return html.EscapeString(truncateRunes(v, maxDiffValueLen))
}

func markerLink(url string) string {
	if url == "" {
		return ""
	}
	return ` · <a href="` + html.EscapeString(url) + `" target="_blank">Open in NetBox</a>`
}

// truncateRunes cuts s to n runes, marking the cut with an ellipsis.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
	urls := make([]string, n)
	allHaveURL := true
	for i, node := range g.Nodes {
		ids[i] = node.ID
		titles[i] = node.Title
		subs[i] = node.Site
		stats[i] = node.Role
		colors[i] = statusColor(node.Status)
		urls[i] = node.URL
		if node.URL == "" {
			allHaveURL = false
		}
	}
	urlField := data.NewField("url", nil, urls)
	// A field-level data link is advertised for every row, so only attach the
	// "View in NetBox" link when every node has a URL — otherwise a node without
	// one would show a dead menu item (empty href). Topology nodes are devices,
	// which always carry display_url, so in practice the link is always attached.
	if allHaveURL {
		urlField.Config = &data.FieldConfig{Links: []data.DataLink{{
			Title:       "View in NetBox",
			URL:         `${__data.fields["url"]}`,
			TargetBlank: true,
		}}}
	}
	nodes := data.NewFrame("nodes",
		data.NewField("id", nil, ids),
		data.NewField("title", nil, titles),
		data.NewField("subTitle", nil, subs),
		data.NewField("mainStat", nil, stats),
		data.NewField("color", nil, colors),
		urlField,
	)
	nodes.Meta = &data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph}

	m := len(g.Edges)
	eid := make([]string, m)
	esrc := make([]string, m)
	etgt := make([]string, m)
	kinds := make([]string, m)
	for i, e := range g.Edges {
		eid[i] = e.ID
		esrc[i] = e.Source
		etgt[i] = e.Target
		kinds[i] = e.Kind
	}
	edges := data.NewFrame("edges",
		data.NewField("id", nil, eid),
		data.NewField("source", nil, esrc),
		data.NewField("target", nil, etgt),
		data.NewField("detail__kind", nil, kinds),
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

// buildAlertFrame reshapes a Result into the tabular form Grafana alerting
// evaluates: every column except valueField becomes a plain string label
// column, plus exactly one numeric column named "value" (each row is one
// alert instance). valueField == "" emits a constant 1 per row — "this row
// matched the filters"; otherwise the named column supplies the number
// (unparseable values become 0).
//
// The frame is tagged numeric-long (the dataplane "SQL table" contract: string
// fields are labels, numeric fields are values). Without this type tag Grafana's
// server-side expression engine cannot classify the multi-row frame and a
// Reduce/Threshold step fails with "input data must be a wide series but got
// type ...". The tag lets Grafana convert it to numeric-multi — one series per
// row, keyed by the string columns — so a Threshold expression yields one alert
// instance per row with those columns as labels.
func buildAlertFrame(objectType string, res *provider.Result, valueField string) *data.Frame {
	frame := data.NewFrame(frameName(objectType))
	frame.Meta = &data.FrameMeta{
		Type:        data.FrameTypeNumericLong,
		TypeVersion: data.FrameTypeVersion{0, 1},
	}

	for _, col := range res.Columns {
		if col == valueField {
			continue
		}
		vals := make([]string, len(res.Rows))
		for i, r := range res.Rows {
			vals[i] = toString(r[col])
		}
		frame.Fields = append(frame.Fields, data.NewField(col, nil, vals))
	}

	nums := make([]float64, len(res.Rows))
	for i, r := range res.Rows {
		if valueField == "" {
			nums[i] = 1
			continue
		}
		nums[i] = toFloat(r[valueField])
	}
	frame.Fields = append(frame.Fields, data.NewField("value", nil, nums))
	return frame
}

// toFloat coerces a JSON-native value to a float64, returning 0 when the
// value is missing, unparseable, or of an unsupported type.
func toFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}
