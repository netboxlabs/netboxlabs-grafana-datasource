// Package provider defines the enrichment backend abstraction used by the
// NetBox Grafana datasource.
//
// The datasource never talks to NetBox (or any other source) directly. Instead
// it talks to a Provider. Today there is one implementation that queries the
// NetBox REST API (package netbox). A second, high-volume enrichment backend is
// planned; keeping the datasource behind this interface is what makes that
// addition a drop-in rather than a rewrite.
package provider

import (
	"context"
	"time"
)

// ObjectType is a queryable collection of objects, discovered dynamically from
// the upstream so that core models AND plugin-provided models (e.g. BGP) are
// available without code changes.
type ObjectType struct {
	// Value is the stable identifier used in queries, e.g. "dcim/devices" or
	// "plugins/bgp/bgp-sessions".
	Value string `json:"value"`
	// Label is a human-friendly name for the query editor, e.g. "Devices".
	Label string `json:"label"`
	// App is the owning application/plugin, e.g. "dcim" or "bgp".
	App string `json:"app"`
	// Model is the model name, e.g. "devices".
	Model string `json:"model"`
}

// FieldType is the logical type of a column, used to build the right Grafana
// data frame field.
type FieldType string

const (
	FieldTypeString  FieldType = "string"
	FieldTypeNumber  FieldType = "number"
	FieldTypeBoolean FieldType = "boolean"
	FieldTypeTime    FieldType = "time"
)

// Field is a column in a query result.
type Field struct {
	Name string    `json:"name"`
	Type FieldType `json:"type"`
}

// FilterField is a queryable filter parameter for an object type and the set of
// operators NetBox supports on it (operator tokens match Filter.Operator).
type FilterField struct {
	Name      string   `json:"name"`
	Operators []string `json:"operators"`
}

// Filter is a single field/operator/value constraint applied to a query.
type Filter struct {
	Field    string `json:"field"`
	Operator string `json:"operator"` // NetBox lookup, e.g. "" (exact), "ic", "n", "gte"
	Value    string `json:"value"`
}

// QuerySpec describes an object query.
type QuerySpec struct {
	ObjectType string   `json:"objectType"`
	Filters    []Filter `json:"filters"`
	// Fields optionally restricts the returned columns (and their order). Empty
	// means "all discovered columns".
	Fields []string `json:"fields"`
	// Limit caps the number of rows returned (0 = provider default).
	Limit int `json:"limit"`
}

// Result is a flattened, table-shaped query result. Columns is the ordered set
// of column names; Rows holds one map per object keyed by column name. Values
// are JSON-native (string, float64, bool, nil).
type Result struct {
	Columns []string                 `json:"columns"`
	Rows    []map[string]interface{} `json:"rows"`
	// Total is the number of objects matching the query as reported by the
	// source (e.g. NetBox's list-envelope "count"), independent of Rows/limit.
	// Used for count-only queries (alerting). 0 when the source cannot report it.
	Total int `json:"total"`
	// Warnings reports partial-result degradation: the rows are worth returning,
	// but some column the caller asked for is blank because a lookup FAILED
	// rather than because the source holds nothing there. Without this, a blank
	// column is indistinguishable from genuine absence — the two carry opposite
	// conclusions, so the gap has to be stated.
	//
	// Each entry is a complete, user-facing sentence naming what is missing and
	// why. The plugin layer turns them into frame notices (pkg/plugin/notices.go)
	// at WARNING severity; producers must therefore keep them free of raw
	// upstream response bodies and URLs.
	//
	// Deliberately []string and not []data.Notice: this package is the seam a
	// second, non-NetBox backend plugs into, so it stays free of Grafana SDK
	// frame types — the same reason FieldType exists here instead of reusing
	// data.FieldType. Optional: nil for a complete result, which is the normal
	// case, so every existing consumer is unaffected.
	//
	// Grafana alert evaluation cannot see frame notices (the same limitation
	// truncationError exists for), so a query path that feeds alerting must treat
	// a non-empty Warnings as a hard failure rather than warn. The ip-enrichment
	// branch does: it fails on both truncation and non-empty Warnings. The
	// objects/alertTable branch currently fails on truncation only, keying off
	// its explicit alertTable flag. A future producer that sets Warnings on
	// alertTable must also wire degradationError there, as ip-enrichment does.
	// A rule querying ip-enrichment and reducing over match_count was observed
	// to keep evaluating silently on a degraded result — health ok, lastError
	// nil, no notice surfaced — before that check existed. Any new alert-facing
	// path must repeat the pattern; this field is invisible to alerting.
	Warnings []string `json:"warnings,omitempty"`
	// Notes reports something true about the result that is worth stating but is
	// not a degradation: the rows are complete and correct, and the reader would
	// still draw a wrong conclusion without the sentence. The plugin layer renders
	// them as INFO frame notices, the same way Warnings become WARNING ones.
	//
	// The distinction from Warnings is severity, and it is load-bearing: a warning
	// that fires on a routine, correct result trains users to ignore warnings, so
	// the two must not be merged. ResolveIPs' only note today says some rows were
	// picked out of several matching address records — deterministic, documented,
	// and invisible once the opt-in match_count column is deselected.
	//
	// Alert evaluation treats Notes as harmless: unlike Warnings they do not mean
	// a column is empty for a reason the data cannot show, so a rule may evaluate
	// on a result that carries them.
	Notes []string `json:"notes,omitempty"`
	// ColumnTypes declares the logical type of a column whose name the producer
	// knows the type of up front, independent of what this particular result
	// happens to contain. It exists because the plugin layer otherwise infers a
	// column's frame type by SCANNING the values, and a column that is entirely
	// absent in one refresh is unclassifiable: it falls back to string.
	//
	// That inference is right for a discovered object query, whose columns come
	// from NetBox's schema at runtime and can hold anything. It is wrong for a
	// FIXED schema like ip-enrichment's, where device_is_primary_ip is a boolean
	// by definition and merely happens to be null for every row of an IP set that
	// resolves no device (all external, all VM-assigned, all unassigned). Without
	// this hint the same saved query alternates between a boolean field and a
	// string one as the data moves, and boolean value mappings, `filterByValue
	// isTrue`, overrides and transformations silently stop applying on the refresh
	// where nothing matched.
	//
	// It is a HINT for the unclassifiable case only: the plugin consults it when a
	// column has no values at all, and observed values always win otherwise, so a
	// wrong or stale entry can never delete data. Optional — nil means "infer
	// everything", which is what the objects, alert-table and topology paths do.
	//
	// FieldType, not data.FieldType, for the same reason Warnings is []string and
	// not []data.Notice: this package is the seam a second, non-NetBox backend
	// plugs into, so it stays free of Grafana SDK frame types.
	ColumnTypes map[string]FieldType `json:"columnTypes,omitempty"`
}

// Change is a single change-log/audit event, used to render annotations.
type Change struct {
	Time       time.Time `json:"time"`
	Action     string    `json:"action"`
	ObjectType string    `json:"objectType"`
	ObjectRepr string    `json:"objectRepr"`
	User       string    `json:"user"`
	URL        string    `json:"url"` // deep link to the object/change in the UI
}

// ChangeSpec bounds an annotation query.
type ChangeSpec struct {
	From        time.Time
	To          time.Time
	ObjectTypes []string // content-type filters, e.g. ["dcim.device"]; empty = all
	Limit       int
}

// TopologySpec bounds a topology (node-graph) query.
type TopologySpec struct {
	Filters []Filter // applied to the device set, e.g. site/role
	Limit   int
	// ConnectedOnly drops devices that have no cable to another device, so the
	// graph shows the connected fabric rather than isolated nodes.
	ConnectedOnly bool
	// Connections selects edge derivation: "logical" (default — NetBox-computed
	// cable paths, so patch panels and circuits resolve to the far device) or
	// "physical" (raw cables; panels appear as nodes).
	Connections string
}

// GraphNode is a device in the topology graph.
type GraphNode struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	SubTitle string `json:"subTitle"`
	MainStat string `json:"mainStat"`
	// Status drives node color (e.g. "active", "offline", "failed").
	Status string `json:"status"`
	// URL is the device's NetBox page (display_url); empty if unavailable.
	URL string `json:"url,omitempty"`
}

// GraphEdge is a link between two devices.
type GraphEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Label  string `json:"label"`
	// Kind tags the edge source: "path" | "cable" | "wireless".
	Kind string `json:"kind"`
}

// Graph is a device/link topology.
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// Provider is the enrichment backend the datasource depends on.
type Provider interface {
	// Name identifies the provider implementation (for health messages/logs).
	Name() string

	// HealthCheck verifies connectivity and auth, returning a human-friendly
	// status message on success.
	HealthCheck(ctx context.Context) (string, error)

	// ObjectTypes returns the dynamically discovered set of queryable object
	// types, including plugin-provided models.
	ObjectTypes(ctx context.Context) ([]ObjectType, error)

	// Fields returns the columns available for an object type, derived from a
	// sample object so plugin models work without hard-coding.
	Fields(ctx context.Context, objectType string) ([]Field, error)

	// FilterFields returns the valid filter parameters for an object type and,
	// for each, the operators NetBox supports — so the editor can offer only
	// combinations the API honors. Empty result ⇒ caller should fall back.
	FilterFields(ctx context.Context, objectType string) ([]FilterField, error)

	// Query executes an object query and returns flattened, joinable rows.
	Query(ctx context.Context, spec QuerySpec) (*Result, error)

	// ResolveIPs maps each input IP to the context of its longest-matching NetBox
	// prefix (and any exact host match), returning a table keyed by "ip". This is
	// the enrichment path for flow/log data carrying arbitrary IPs that a
	// value-equality join cannot handle.
	ResolveIPs(ctx context.Context, ips []string, fields []string, limit int) (*Result, error)

	// Topology returns a device/link graph for the node-graph visualization.
	Topology(ctx context.Context, spec TopologySpec) (*Graph, error)

	// FieldValues returns distinct values for a field, for query-editor
	// autocomplete. q is an optional case-insensitive substring filter.
	FieldValues(ctx context.Context, objectType, field, q string, limit int) ([]string, error)

	// Changes returns change-log events within a time window for annotations.
	Changes(ctx context.Context, spec ChangeSpec) ([]Change, error)

	// BaseURL returns the upstream base URL (used to build deep links).
	BaseURL() string
}

// BranchingCapable is an OPTIONAL capability a Provider may implement when its
// backend supports netbox-branching. It is deliberately kept OFF the core
// Provider interface: the seam is backend-agnostic (a second, non-NetBox
// backend is planned), and branching is a NetBox-specific plugin concept that
// backend cannot meaningfully satisfy. Callers type-assert; a provider that
// does not implement it is treated as "branching unavailable".
type BranchingCapable interface {
	// BranchingInstalled reports whether the netbox-branching plugin is
	// installed on the connected upstream. conclusive is false when detection
	// is inconclusive (a transient upstream failure); callers must treat that
	// as "unknown" and fail open (do not disable branch UI), never as "absent".
	BranchingInstalled(ctx context.Context) (installed bool, conclusive bool)
}
