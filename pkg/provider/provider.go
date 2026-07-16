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
