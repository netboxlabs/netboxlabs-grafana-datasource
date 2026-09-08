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
	// KeyFields names columns the caller READS out of each returned row but does
	// NOT want as output columns — join-key sources, today (pkg/plugin.joinKeys).
	//
	// It exists because a provider is allowed to fetch only what it was asked
	// for: the NetBox provider projects the upstream request onto Fields, and a
	// join whose source is outside the selection would then derive its output
	// from a value that was never fetched — an empty column on every row, with
	// no error to say so. Naming the source here fetches it without displaying
	// it, because asking to join on a column is not asking to see it.
	//
	// Result.Columns is unaffected; only Result.Rows is guaranteed to carry
	// these values.
	KeyFields []string `json:"keyFields"`
	// Ordering asks the SOURCE to sort, naming one field, optionally prefixed
	// with "-" for descending (e.g. "name", "-last_updated"). Empty means the
	// source's natural order, which is what every caller got before this field
	// existed and what a caller that never sets it keeps getting.
	//
	// It exists because sorting and Limit are the same question. A panel showing
	// 100 of 6.8 million devices sorted client-side sorts the arbitrary 100 it was
	// given; the same query sorted upstream returns a DIFFERENT hundred — the
	// first hundred by that field. Only the source can answer the second one, so
	// the request has to travel through this seam rather than being applied to the
	// rows on the way out.
	//
	// A provider is permitted to IGNORE it, and must say so on Result.Notes when
	// it does. That is not a weakness of the contract but the honest shape of it:
	// whether a given field can be sorted on is the remote's decision, it is not
	// discoverable at runtime on NetBox (see the netbox backend's ordering.go),
	// and the alternative — failing the query — would turn a saved dashboard into
	// an error toast over a preference. Callers that CANNOT accept a different
	// order must therefore not ask; nothing here promises the rows came back
	// sorted.
	Ordering string `json:"ordering"`
	// Limit caps the number of rows returned (0 = provider default).
	Limit int `json:"limit"`
	// AllowUncounted says this caller can present its answer WITHOUT a reliable
	// Result.Total, and therefore permits a provider to use a faster access path
	// that does not produce one (the NetBox backend's cursor pagination, which
	// is itself off unless the datasource opts in).
	//
	// The polarity is the safety property, not a style choice. Total is
	// load-bearing: a count-only alerting query IS Result.Total, and the
	// truncation guard that stops a rule evaluating on an arbitrary subset is a
	// comparison against it. An uncounted result decodes to Total 0, which reads
	// as "nothing matched" — the same shape as a healthy answer. Defaulting to
	// false means a query path written later, by someone who has never read this
	// comment, gets the counted behaviour by omission: forgetting this field
	// costs speed, never correctness.
	AllowUncounted bool `json:"allowUncounted"`
	// CountOnly says this caller reads Result.Total and nothing else, so the
	// provider may return a minimal Columns/Rows (it still returns at least the
	// object identity). It exists so the count query can skip the expensive
	// parts of building a row — NetBox's per-device config-context annotation
	// dominates the count on a multi-million-row instance — without the caller
	// having to pretend it wants a column it will never read.
	//
	// Mutually exclusive with AllowUncounted, which the provider rejects: a
	// caller that reads only the total cannot also be able to do without it.
	CountOnly bool `json:"countOnly"`
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
	// MaxRows is the row ceiling that applied to THIS query, as the provider
	// enforced it. 0 means the provider does not report one.
	//
	// It lives on the result rather than being a package constant the plugin
	// layer reads off one backend, for two reasons. A second Provider has its own
	// ceiling — nothing says it matches NetBox's. And even within one backend the
	// ceiling is not single-valued: the objects path clamps to the maximum while
	// the change-log path clamps to a smaller default, so a single exported
	// number was already standing in for two policies and could only be right
	// about one of them.
	//
	// Consumers must treat 0 as "unknown" and omit any advice that would quote a
	// ceiling, rather than printing one.
	MaxRows int `json:"maxRows,omitempty"`
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
	// a non-empty Warnings as a hard failure rather than warn. All three do: the
	// ip-enrichment, alert-table and plain objects branches reject a degraded
	// result when the call is an alert evaluation (pkg/plugin/query.go, keyed off
	// fromAlert — not off the alertTable flag, which is the rule author's choice
	// of frame shape and says nothing about who is asking). A future alert-facing
	// branch must wire degradationError the same way.
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
	// Capped, when non-nil, reports that an expensive per-row enrichment
	// deliberately measured only the first Cap.Measured rows and left its columns
	// blank on the rest.
	//
	// This is NOT a Warning, and keeping the two apart is the whole point of the
	// field. A warning means a lookup FAILED: the number exists upstream, we could
	// not fetch it, a retry may well produce it, and an alert rule must refuse to
	// evaluate because the gap is invisible in the numbers. A cap means the
	// opposite — nothing failed, every value present is correct, there is simply
	// less of it, and the user's own row limit is the dial that fixes it. Sharing
	// one channel made an alert rule over a result that was merely large report
	// "degraded data" and go to Error state, with the only suggested remedy being
	// the one thing that could not help.
	//
	// The plugin layer renders it as a frame notice for dashboards and, for
	// alerting, as the same shape of error truncation produces: state the two
	// counts and say to lower the limit (see pkg/plugin/notices.go).
	Capped *Cap `json:"capped,omitempty"`
	// ColumnTypes declares the logical type of a column whose name the producer
	// knows the type of up front, independent of what this particular result
	// happens to contain. It exists because the plugin layer otherwise infers a
	// column's frame type by SCANNING the values, and a column that is entirely
	// absent in one refresh is unclassifiable: it falls back to string.
	//
	// That inference is right for a discovered object query, whose columns come
	// from NetBox's schema at runtime and can hold anything. It is wrong for a
	// FIXED schema like ip-enrichment's, where is_primary_ip is a boolean
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

// Cap describes a deliberate bound on how many rows an expensive per-row
// enrichment measured (see Result.Capped).
//
// It carries counts rather than a finished sentence — unlike Warnings and Notes,
// which the producer writes in full — because the two consumers need to say
// different things about the same fact: a dashboard states it and moves on,
// while an alert has to name the number to lower the limit TO. That number is
// Measured, and only the producer knows it.
type Cap struct {
	// Columns names the columns left blank past the bound, in the order the
	// producer wants them read back to the user (e.g. utilization, used,
	// available). Never empty when Cap is set.
	Columns []string
	// Measured is how many leading rows carry those columns.
	Measured int
	// Rows is how many rows the result holds in total; Rows-Measured are blank.
	// Always greater than Measured — a producer that measured everything must
	// leave Result.Capped nil rather than report a cap that did not bite.
	Rows int
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
	// IncludeBoundaryPeers keeps links whose far end lies OUTSIDE the filtered
	// device set, adding that peer as a node.
	//
	// The node-graph view wants the opposite: an edge leaving the filtered set
	// is drawn dangling, so it is dropped. A caller asking "does this device
	// have a healthy neighbour?" cannot accept that, because the dropped
	// neighbour may be the healthy one — and per-site filtering, which the
	// suppression recipe recommends, is exactly when a link leaves the set.
	IncludeBoundaryPeers bool
}

// GraphNode is a device in the topology graph.
type GraphNode struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Site and Role are the device's NetBox context. They are named for what
	// they ARE, not for the node-graph slots they happen to fill: the
	// topology-edges query emits them as data columns, and a column whose
	// meaning is "whatever we currently show as the node subtitle" would change
	// under a presentation edit. buildNodeGraphFrames maps them to the panel's
	// subTitle/mainStat.
	Site string `json:"site"`
	Role string `json:"role"`
	// Status drives node color (e.g. "active", "offline", "failed").
	Status string `json:"status"`
	// URL is the device's NetBox page (display_url); empty if unavailable.
	URL string `json:"url,omitempty"`
	// Boundary marks a node pulled in because a link reached it, NOT because it
	// matched the filter. Its own links were never traversed, so only the links
	// it shares with in-scope devices are known.
	//
	// The distinction is load-bearing for anything reasoning about neighbours: a
	// boundary node looks like a device with exactly one connection, and
	// treating that as its full neighbour set concludes it has no other path.
	Boundary bool `json:"boundary,omitempty"`
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
	// Total is the number of devices matching the spec's filters as the source
	// reported it, independent of Limit. 0 when the source cannot report one.
	//
	// It exists because a truncated graph is not a smaller answer, it is a
	// DIFFERENT one: edges are discovered from the devices that were retained,
	// so a device inside the slice can keep one neighbour and lose another that
	// fell outside it. A caller reasoning about a device's neighbours — the
	// topology-edges suppression recipe does exactly that — would draw a
	// confident, wrong conclusion with nothing to show for it.
	Total int `json:"total"`
	// Fetched is how many matching devices the traversal actually retrieved,
	// BEFORE any presentation pruning (ConnectedOnly) and excluding boundary
	// peers pulled in from outside the filter.
	//
	// Truncation is Fetched < Total, and it is a separate number from
	// len(Nodes) precisely because those two adjustments move len(Nodes) for
	// reasons that are not truncation: dropping an isolated device is a
	// deliberate, complete answer, and adding a boundary peer is extra
	// information rather than missing information.
	Fetched int `json:"fetched"`
	// MaxRows is the device ceiling that applied to this traversal, as the
	// provider enforced it. 0 means the provider does not report one. Same
	// contract as Result.MaxRows.
	MaxRows int `json:"maxRows,omitempty"`
	// Warnings reports that the EDGE set is incomplete for reasons other than
	// the device limit: a link lookup that failed, or one that hit the row cap
	// for a single device and dropped the remainder.
	//
	// Same contract and the same reason as Result.Warnings. A missing link is
	// invisible in the output — the device is still there, simply looking less
	// connected than it is — and for the suppression recipe that reads as "this
	// device has no healthy upstream", which silences a page. Each entry is a
	// complete, user-facing sentence.
	Warnings []string `json:"warnings,omitempty"`
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
