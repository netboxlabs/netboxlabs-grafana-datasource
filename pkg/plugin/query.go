package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

// queryType discriminates the kinds of query the editor can issue.
const (
	queryTypeObjects       = "objects"
	queryTypeAnnotations   = "annotations"
	queryTypeIPEnrichment  = "ip-enrichment"
	queryTypeTopology      = "topology"
	queryTypeTopologyEdges = "topology-edges"
)

// ipSourceScope is the IPSource value that lists a NetBox scope instead of
// resolving a caller-supplied IP list.
const ipSourceScope = "scope"

// queryModel is the JSON shape sent by the frontend query/variable/annotation
// editors.
type queryModel struct {
	QueryType  string            `json:"queryType"`
	ObjectType string            `json:"objectType"`
	Filters    []provider.Filter `json:"filters"`
	Fields     []string          `json:"fields"`
	// Ordering names the field NetBox should sort by, "-" prefixed for descending
	// (e.g. "name", "-last_updated"). Empty is NetBox's natural order. The
	// provider decides whether it can be honored (see netbox/ordering.go) and says
	// so in a note when it cannot, so nothing here validates it.
	Ordering string `json:"ordering"`
	Limit    int    `json:"limit"`
	// JoinKeys derive extra key columns so the result lines up with metric labels.
	JoinKeys []joinKey `json:"joinKeys"`
	// Count, when true on an objects query, returns a single-value numeric
	// "count" frame (row count) instead of the table — used by alert rules.
	Count bool `json:"count"`
	// AlertTable, when true on an objects query, reshapes the result into the
	// tabular form Grafana alerting evaluates: string label columns plus one
	// numeric "value" column, one alert instance per row. Count wins if both
	// are set.
	AlertTable bool `json:"alertTable"`
	// ValueField names the column that supplies the numeric value for
	// AlertTable (e.g. "utilization"). Empty means a constant 1 per row.
	ValueField string `json:"valueField"`
	// ObjectTypes optionally restricts annotation queries to specific NetBox
	// content types (e.g. "dcim.device").
	ObjectTypes []string `json:"objectTypes"`
	// IPs (for ip-enrichment) is a free-form list of IPs separated by commas,
	// whitespace or newlines; supports interpolated $variables.
	IPs string `json:"ips"`
	// IPSource (for ip-enrichment) says where the addresses come from: "list"
	// (or empty) resolves the IPs in IPs; "scope" lists every address NetBox
	// holds under Filters (ipam/ip-addresses filters) and enriches those. Scope
	// is the alert-rule path — a rule cannot supply an IP list, but it can join a
	// metric's IP against this table in a SQL expression.
	IPSource string `json:"ipSource"`
	// ContextFields (for ip-enrichment) selects which context columns to
	// return (see IPEnrichColumns).
	ContextFields []string `json:"contextFields"`
	// ConnectedOnly (for topology) drops devices with no inter-device cable.
	ConnectedOnly bool `json:"connectedOnly"`
	// Connections (for topology): "logical" (default — NetBox cable paths) or
	// "physical" (raw cables; panels appear as nodes).
	Connections string `json:"connections"`
	// Branch, when set, is a netbox-branching schema id; the query (and every
	// upstream request it makes) targets that branch via the X-NetBox-Branch
	// header. Empty targets the default (main) branch.
	Branch string `json:"branch"`
}

// query executes a single query and returns its data response.
//
// c says what will read the frames (see consumer and requestConsumer). It is an
// explicit parameter rather than something read back out of the context so that
// every future call site is forced by the compiler to state which it is: getting
// it wrong in the silent direction means alerting, or computing an expression, on
// data the query itself knows is incomplete.
func (d *Datasource) query(ctx context.Context, q backend.DataQuery, c consumer) backend.DataResponse {
	var qm queryModel
	if err := json.Unmarshal(q.JSON, &qm); err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err))
	}

	// Once, here, so no branch below can forget it (see dropAllFilters).
	qm.Filters = dropAllFilters(qm.Filters)

	// Scope every request this query makes to the selected branch (no-op when empty).
	ctx = provider.WithBranch(ctx, qm.Branch)

	switch qm.QueryType {
	case queryTypeAnnotations:
		changes, err := d.provider.Changes(ctx, provider.ChangeSpec{
			From:        q.TimeRange.From,
			To:          q.TimeRange.To,
			ObjectTypes: qm.ObjectTypes,
			Limit:       qm.Limit,
		})
		if err != nil {
			return queryErrorResponse(err)
		}
		frame := buildAnnotationsFrame(changes)
		frame.RefID = q.RefID
		return backend.DataResponse{Frames: data.Frames{frame}}

	case queryTypeIPEnrichment:
		// A join key's source has to be in the REQUEST, not just in the editor:
		// ResolveIPs projects each row down to the fields asked for, so a source
		// outside the context selection was gone before applyJoinKeys could read
		// it and the output column came out empty for every row. joinOnly names
		// the fields fetched only for that, dropped again below so the join does
		// not silently add a column to the table. See ipEnrichFields.
		fields, joinOnly := ipEnrichFields(qm.ContextFields, qm.JoinKeys)
		var res *provider.Result
		var err error
		noun := nounIPs
		if qm.IPSource == ipSourceScope {
			// The filter rows are the scope. $__all rows are already gone
			// (dropAllFilters ran before the switch), so "All" narrows nothing here
			// either. Total counts address records, so the notices say so.
			//
			// A scope with no effective filter is the whole ipam/ip-addresses
			// table, up to the row cap, on every evaluation — never what a rule
			// meant, and the editor holds such a query back. A provisioned rule or
			// an API caller has no editor, so it is refused here too, and the list
			// path's "nothing to resolve" empty response would be the wrong answer:
			// silence is how a rule over an unscoped table would fail.
			if !netbox.FiltersNarrow(qm.Filters) {
				return backend.ErrDataResponse(backend.StatusBadRequest,
					"A NetBox scope needs at least one filter with a value — a prefix (parent), VRF or tenant — or it is the whole address table. A filter set to All ($__all) narrows nothing.")
			}
			noun = nounScope
			res, err = d.provider.ResolveScope(ctx, qm.Filters, fields, qm.Limit)
		} else {
			// The list IS the input here, so All has nothing to drop and resolves
			// nothing — the same as an empty list — rather than asking NetBox for an
			// address called $__all. Same token, same meaning as on a filter row.
			ips := slices.DeleteFunc(splitList(qm.IPs), func(ip string) bool { return ip == allFilterValue })
			if len(ips) == 0 {
				return backend.DataResponse{}
			}
			res, err = d.provider.ResolveIPs(ctx, ips, fields, qm.Limit)
		}
		if err != nil {
			return queryErrorResponse(err)
		}
		// Alert evaluation converts this frame to numeric-multi and drops
		// meta.notices, so the truncation and degradation notices appended below
		// are invisible to a rule — reproduced: 5 IPs at limit 2, reduce +
		// threshold, HTTP 200 with no error and no notices key. A rule must
		// therefore fail rather than evaluate on a partial or degraded answer,
		// which is what the objects branch already does via its alertTable flag.
		// Dashboards keep the opposite policy — partial beats none, with the gap
		// stated in a notice — which is exactly why this is gated on the consumer
		// and not applied unconditionally.
		//
		// An expression is the same reader as a rule here: it drops meta.notices
		// too, so c.strict() covers both (see consumer).
		if c.strict() {
			// ResolveIPs caps nothing today, so the cap check inside cannot fire from
			// here. It runs anyway because the alternative is the bug this exists
			// for in reverse: a strict path that silently ignores a new Result field
			// is exactly how a deliberate cap ended up evaluating as a column of
			// zeroes.
			if msg := resultRefusal(c, res, noun); msg != "" {
				return backend.ErrDataResponse(backend.StatusBadRequest, msg)
			}
		}
		applyJoinKeys(res, qm.JoinKeys)
		dropColumns(res, joinOnly)
		rewriteLinks(res, d.provider.BaseURL(), d.cfg.PublicURL)
		frame := buildFrame("ip-enrichment", res, d.provider.BaseURL())
		frame.RefID = q.RefID
		// Tell the user when this is only part of the answer. buildFrame always sets
		// Meta, so appending here is safe. Not nounObjects: a list query's Total
		// counts the IPs that were asked about, not objects that matched — plenty
		// of them match nothing — and a scope query's counts address records.
		frame.Meta.Notices = append(frame.Meta.Notices, resultNotices(res, qm.Limit, noun)...)
		return backend.DataResponse{Frames: data.Frames{frame}}

	case queryTypeTopology, queryTypeTopologyEdges:
		graph, err := d.provider.Topology(ctx, provider.TopologySpec{
			Filters: qm.Filters, Limit: qm.Limit,
			ConnectedOnly: qm.ConnectedOnly, Connections: qm.Connections,
			// The edges table answers "who are this device's neighbours", so a
			// link leaving the filtered set still names a real one. The node
			// graph wants it dropped instead, so nothing dangles off the picture.
			IncludeBoundaryPeers: qm.QueryType == queryTypeTopologyEdges,
		})
		if err != nil {
			return queryErrorResponse(err)
		}
		if qm.QueryType == queryTypeTopologyEdges {
			// A truncated traversal is not a smaller answer to this question, it is
			// a wrong one: the missing devices take their links with them, so a
			// device can appear to have lost its only healthy upstream. The
			// suppression recipe reads exactly that and would silence an alert that
			// should have paged. Refused for the same reason, and in the same
			// words, as the object and IP-enrichment alert paths. A SQL join reads
			// this table the same way, so an expression is refused as well.
			if c.strict() {
				if msg := graphTruncationError(c, graph); msg != "" {
					return backend.ErrDataResponse(backend.StatusBadRequest, msg)
				}
				// Edge-set gaps are refused too, and separately: a device whose
				// links could not all be read looks less connected than it is,
				// which reads as "no working path" and silences a page. Same
				// reason degradationError exists on the object path.
				if msg := graphDegradationError(c, graph); msg != "" {
					return backend.ErrDataResponse(backend.StatusBadRequest, msg)
				}
			}
			// No link rewriting: the edges table carries no URLs. It is data for a
			// join, not something a user clicks.
			frame := buildTopologyEdgesFrame(graph)
			frame.RefID = q.RefID
			return backend.DataResponse{Frames: data.Frames{frame}}
		}
		rewriteGraphLinks(graph, d.provider.BaseURL(), d.cfg.PublicURL)
		frames := buildNodeGraphFrames(graph)
		for _, f := range frames {
			f.RefID = q.RefID
		}
		return backend.DataResponse{Frames: frames}
	}

	if qm.ObjectType == "" {
		// Nothing selected yet; return an empty response.
		return backend.DataResponse{}
	}

	if qm.Count {
		// Count returns the total number of matching objects (from the source's
		// list envelope), independent of the row limit. Fetch minimally — we need
		// the total, not a page of rows: CountOnly says so out loud, which lets
		// the provider skip the per-row work NetBox would otherwise do to build a
		// row nothing here reads. It also forbids any access path that cannot
		// report a total, which is the whole content of this frame.
		res, err := d.provider.Query(ctx, provider.QuerySpec{
			ObjectType: qm.ObjectType,
			Filters:    qm.Filters,
			Limit:      1,
			CountOnly:  true,
		})
		if err != nil {
			return queryErrorResponse(err)
		}
		frame := buildCountFrame(qm.ObjectType, res.Total)
		frame.RefID = q.RefID
		return backend.DataResponse{Frames: data.Frames{frame}}
	}

	if qm.AlertTable {
		fields := qm.Fields
		if qm.ValueField != "" && len(fields) > 0 && !slices.Contains(fields, qm.ValueField) {
			fields = append(slices.Clone(fields), qm.ValueField)
		}
		res, err := d.provider.Query(ctx, provider.QuerySpec{
			ObjectType: qm.ObjectType,
			Filters:    qm.Filters,
			Fields:     fields,
			KeyFields:  joinKeySources(qm.JoinKeys),
			Ordering:   queryOrdering(qm.Ordering, c),
			Limit:      qm.Limit,
		})
		if err != nil {
			return queryErrorResponse(err)
		}
		if qm.ValueField != "" && !slices.Contains(res.Columns, qm.ValueField) {
			return backend.ErrDataResponse(backend.StatusBadRequest,
				fmt.Sprintf("value field %q not found in results — add it to Return fields", qm.ValueField))
		}
		// Grafana alert evaluation cannot see frame notices, so an alert-shaped
		// result that is truncated, capped or degraded must fail loudly instead of
		// alerting on an arbitrary subset or on values that were never measured.
		// Unconditional, unlike the other branches: asking for this shape is
		// asking for a rule's input, whoever is asking. See resultRefusal for the
		// three checks and why they run in that order.
		if msg := resultRefusal(c, res, nounObjects); msg != "" {
			return backend.ErrDataResponse(backend.StatusBadRequest, msg)
		}
		applyJoinKeys(res, qm.JoinKeys)
		rewriteLinks(res, d.provider.BaseURL(), d.cfg.PublicURL)
		frame := buildAlertFrame(qm.ObjectType, res, qm.ValueField)
		frame.RefID = q.RefID
		return backend.DataResponse{Frames: data.Frames{frame}}
	}

	res, err := d.provider.Query(ctx, provider.QuerySpec{
		ObjectType: qm.ObjectType,
		Filters:    qm.Filters,
		Fields:     qm.Fields,
		// A join key reads its source out of the row, which a provider that
		// fetches only the selected fields would otherwise never have fetched.
		// See joinKeySources.
		KeyFields: joinKeySources(qm.JoinKeys),
		Ordering:  queryOrdering(qm.Ordering, c),
		Limit:     qm.Limit,
		// A DASHBOARD table is the one thing that can present a result with no
		// match count: a missing Total costs it the "showing 100 of N" notice and
		// nothing else, and the provider replaces that with a note saying the
		// count is unavailable. It is what lets a datasource opt into cursor
		// paging for its panels without touching alerting, where Total decides
		// whether the rule fires at all. Not set on the count or alert-table
		// branches above, deliberately.
		//
		// And not set for an alert evaluation here either, which is why the
		// consumer is read. The alertTable flag is the rule author's choice of frame
		// SHAPE, not a statement about who is asking: an ordinary objects query
		// is a perfectly valid alert-rule query (the editor defaults alertTable
		// to false), and with fast paging on it used to reach this line and
		// evaluate the 100 lowest-ID rows with Total 0 — an arbitrary subset
		// reported as "nothing matched", the truncation guard blind because it
		// compares against that same zero, and the gap stated only in a frame
		// notice, which alert evaluation drops. Gating on the consumer is what makes
		// "alert rules are unaffected by fast paging" — README, the config
		// switch, models.PluginSettings — true of EVERY rule rather than only the
		// two shapes above. It costs an alert rule on a multi-million-row model
		// the count it was skipping; that is the same trade the count and
		// alert-table paths already make, and correctness is the side it is made
		// on.
		//
		// An expression-consumed query is in the same position: its truncation
		// guard below reads Total, so it needs the real count as well.
		AllowUncounted: !c.strict(),
	})
	if err != nil {
		return queryErrorResponse(err)
	}

	// The same argument that gates the paging above applies to the RESULT: an
	// ordinary objects query is a perfectly valid alert-rule query, so this branch
	// serves alert evaluation too, and alert evaluation drops meta.notices — the
	// notices appended below are invisible to a rule. A capped or degraded result
	// therefore has to fail here exactly as it does on the alertTable branch, or a
	// row whose utilization was never measured evaluates as 0% (buildAlertFrame
	// coerces a missing value) and a "utilization > 90" rule silently stops firing.
	//
	// resultRefusal runs the same three checks, in the same order, as the
	// alertTable branch.
	//
	// Only for a strict consumer. A dashboard keeps partial-beats-none: it shows
	// the same gap as a frame notice below and a partial answer there is useful.
	//
	// A query feeding an expression is strict for the alert's reason exactly: the
	// expression drops meta.notices. Measured live, a SQL join over dcim/devices
	// at limit 5 of 15 put "AMS1: 5" on the panel with no notice anywhere.
	if c.strict() {
		// This branch sets AllowUncounted false for a strict consumer precisely so
		// Total is a real count here — reading it is what makes that worth paying
		// for. Without the truncation check, an alert on the editor's DEFAULT query
		// shape evaluates whatever subset the row limit happened to return:
		// measured live, limit 2 against 4 matching prefixes evaluated two
		// instances and dropped an 86%-utilized prefix entirely, so a
		// "utilization > 90" rule never fired and nothing anywhere said why.
		if msg := resultRefusal(c, res, nounObjects); msg != "" {
			return backend.ErrDataResponse(backend.StatusBadRequest, msg)
		}
	}

	applyJoinKeys(res, qm.JoinKeys)
	rewriteLinks(res, d.provider.BaseURL(), d.cfg.PublicURL)
	frame := buildFrame(qm.ObjectType, res, d.provider.BaseURL())
	frame.RefID = q.RefID
	// Tell the user when this is only part of the answer. buildFrame always sets
	// Meta, so appending here is safe.
	frame.Meta.Notices = append(frame.Meta.Notices, resultNotices(res, qm.Limit, nounObjects)...)
	return backend.DataResponse{Frames: data.Frames{frame}}
}

// queryOrdering returns the sort to push down for this call: the query's own
// choice for a dashboard, and none at all for an alert evaluation.
//
// An alert rule cannot see row order — evaluation reduces the frame to one
// instance per row — so the sort could only change a rule's answer by changing
// WHICH rows come back, and that can only happen when the result is truncated,
// which every alert path here already refuses outright (truncationError, with the
// count it compares against guaranteed by AllowUncounted being false for an
// alert). What the sort would change instead is the evaluation's cost: ordering
// 6.8M devices by role measured 27.6s against 0.9s natural, once per evaluation,
// forever.
//
// It mirrors AllowUncounted's polarity for the same reason. A future row-returning
// branch that forgets this helper sends the sort, which costs speed; one that
// forgot to drop it in the other direction would have cost an alert its answer.
//
// An expression-consumed query keeps its sort. It is strict about completeness
// for the alert's reason (see consumer), but the same frame may still be drawn
// in the panel beside the expression's output, where row order shows.
func queryOrdering(ordering string, c consumer) string {
	if c == consumerAlert {
		return ""
	}
	return ordering
}

// allFilterValue is Grafana's own token for a variable's "All" option. A filter
// row whose value is exactly this means "do not filter on this field".
const allFilterValue = "$__all"

// dropAllFilters removes the filter rows whose value is "All".
//
// It is opt-in, by whoever writes the query. Grafana expands All into every
// option of the variable, which the provider turns into repeated NetBox
// parameters: on a few hundred sites, a URL long enough to be refused and a slow
// query, to say nothing. A dashboard author who knows the variable lists
// everything sets its Custom all value to this token and the row goes away
// instead. A caller with no variables to expand — a backend posting to
// /api/ds/query — sends it for the same reason.
//
// It is NOT inferred from a variable being set to All, because All does not
// always mean everything: for a chained variable ("sites in $region") or a
// hand-written list it means every option on offer, and dropping the row would
// silently widen the panel from that subset to the whole inventory.
//
// The operator is not consulted: All is the absence of a filter, whichever way
// the row would have compared. The token has to be the whole value. Grafana never
// produces it as one element of a list, so a list containing it is malformed,
// and that must not be what turns a filtered query into an unfiltered one.
func dropAllFilters(filters []provider.Filter) []provider.Filter {
	if filters == nil {
		return nil
	}
	out := make([]provider.Filter, 0, len(filters))
	for _, f := range filters {
		if strings.TrimSpace(f.Value) != allFilterValue {
			out = append(out, f)
		}
	}
	return out
}

// splitList parses a free-form list separated by commas, whitespace or newlines.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// healthErrorMessage maps an upstream error to a concise, user-facing message.
func healthErrorMessage(err error) string {
	// Status 0 means the provider classified something that was not an HTTP
	// refusal; there is no code to report, so fall through to the transport
	// message rather than printing "HTTP 0".
	// Detail first, and before the status check: a transport failure carries no
	// HTTP code but is exactly where the provider's own wording matters most —
	// the generic fallback below names NetBox, which is the wrong service when
	// a different backend is what could not be reached.
	if u := provider.Classify(err); u != nil {
		if d := boundedDetail(u); d != "" {
			return d
		}
	}
	if u := provider.Classify(err); u != nil && u.Status != 0 {
		switch u.Kind {
		case provider.ErrorKindAuth:
			return "Authentication failed (check API token)"
		case provider.ErrorKindNotFound:
			return "NetBox API not found at this URL (check the base URL)"
		}
		return fmt.Sprintf("NetBox returned HTTP %d", u.Status)
	}
	return "Cannot reach NetBox: " + upstreamDetail(err)
}

// boundedDetail returns the provider's own sentence about a failure, bounded.
//
// Detail is provider-authored and never carries upstream response text (see
// provider.UpstreamError), but it is bounded here anyway on the same principle
// as every other echoed string: the caller cannot see who wrote it.
func boundedDetail(u *provider.UpstreamError) string {
	d := strings.TrimSpace(u.Detail)
	if len(d) > maxUpstreamDetail {
		d = d[:maxUpstreamDetail] + "…"
	}
	return d
}

// maxUpstreamDetail bounds the free-form tail of a user-facing error. It matches
// netbox.snippet()'s and truncateURL's cap for the same reason: these strings
// land side by side in the same Grafana toast.
const maxUpstreamDetail = 300

// upstreamDetail renders a non-APIError upstream failure — a transport error, a
// decode error — as a BOUNDED string that still names the actual cause.
//
// The APIError path was already sanitized; this one was not, and that is the
// whole bug. A transport error is not an APIError, so it fell straight through
// to err.Error() verbatim and netbox.truncateURL (added on this branch for
// exactly this) never ran. Measured on the commonest failure there is — NetBox
// unreachable, 400 IPs — the toast was 12,480 characters of repeated ?address=
// parameters with "connection refused" at the very end.
//
// Head-truncating that string would have cut off the one part worth reading, so
// a *url.Error is UNWRAPPED to its cause instead: url.Error.Error() is
// `Get "<url>": <cause>`, and the cause ("dial tcp 172.20.0.6:9999: connect:
// connection refused") is short, specific, and free of the request line. Errors
// that are not url.Errors are truncated instead — they have no comparable
// structure, and their URL, when they carry one, is already truncateURL'd at the
// client layer.
//
// The raw error still reaches the operator log in full via queryErrorResponse.
func upstreamDetail(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	s := err.Error()
	if len(s) > maxUpstreamDetail {
		return s[:maxUpstreamDetail] + "…"
	}
	return s
}

// queryErrorMessage maps an upstream error to a concise, user-facing message so
// raw NetBox API errors (500/405/etc.) and exception bodies aren't surfaced to
// the user. The raw error is logged separately (sanitized) for operators.
func queryErrorMessage(err error) string {
	if u := provider.Classify(err); u != nil {
		// A provider-authored sentence wins wherever one exists. The wording
		// below says "NetBox" and points at the NetBox URL and API token, which
		// is wrong for a datasource reading from a different backend: it sends
		// the reader to settings that mode does not even use. Only the provider
		// knows which credential its failure is about.
		if u.Kind != provider.ErrorKindUnknownObjectType {
			if d := boundedDetail(u); d != "" {
				return d
			}
		}
		switch u.Kind {
		// An object type the upstream does not know is the USER's input, not an
		// upstream failure, and it is the one error here the reader can act on.
		// Unclassified it fell through into the transport message below, which
		// reported a typo in the annotation editor as "Couldn't reach NetBox:
		// unknown NetBox object type …" — an accusation against the network for a
		// misspelled field.
		case provider.ErrorKindUnknownObjectType:
			// Bounded like every other echoed string here: the type is free text
			// from the annotation editor and lands in the same toast.
			name := u.ObjectType
			if len(name) > maxUpstreamDetail {
				name = name[:maxUpstreamDetail] + "…"
			}
			return fmt.Sprintf("NetBox has no object type %q — it isn't one of the %d types this instance reports. Annotations filter by app_label.model, singular (e.g. dcim.device, ipam.ipaddress).",
				name, u.KnownTypes)
		// A capability the backend does not have. The provider's own sentence is
		// preferred because only it can name what is missing and what to use
		// instead; it is provider-authored (never an upstream body) but bounded
		// here anyway, on the same principle as every other echoed string.
		case provider.ErrorKindUnsupported:
			if d := boundedDetail(u); d != "" {
				return d
			}
			return "This query isn't supported by the backend this datasource is configured to use."
		case provider.ErrorKindNotListable:
			return "This object type can't be queried — the NetBox endpoint doesn't support listing (HTTP 405). It may be an action endpoint, not a queryable collection."
		case provider.ErrorKindInvalidBranch:
			return "NetBox didn't recognize that branch. Check the branch name or schema id against the branch list."
		case provider.ErrorKindBadRequest:
			return "NetBox rejected this query (HTTP 400). Check the filters and try again."
		case provider.ErrorKindAuth:
			return "Authentication failed (check the API token)."
		case provider.ErrorKindNotFound:
			return "This object type was not found in NetBox (HTTP 404)."
		case provider.ErrorKindNotOrderable:
			return "NetBox couldn't list this object type — the endpoint doesn't support pagination (HTTP 500). This model may not be queryable."
		}
		if u.Status != 0 {
			return fmt.Sprintf("NetBox returned HTTP %d for this object type.", u.Status)
		}
	}
	return "Couldn't reach NetBox: " + upstreamDetail(err)
}

// queryErrorResponse logs the raw upstream error (sanitized) and returns a data
// response carrying only the user-facing mapped message.
func queryErrorResponse(err error) backend.DataResponse {
	log.DefaultLogger.Warn("netbox query error", "detail", sanitizeLog(err.Error()))
	return backend.ErrDataResponse(queryErrorStatus(err), queryErrorMessage(err))
}

// queryErrorStatus says whose fault the failure is. Everything upstream —
// a NetBox 500, an unreachable host — stays StatusInternal, which is the honest
// "our side broke, try again". A query that could never have worked as written
// is a bad request, and saying otherwise tells the reader to retry something
// only they can fix.
func queryErrorStatus(err error) backend.Status {
	// These three are settled facts about what was asked for, and StatusInternal
	// says "our side broke, try again" — the opposite of what the reader has to
	// do. A bad request belongs with them: whether it came from the upstream
	// answering 400 or from a filter this backend cannot express, the query is
	// what has to change, and no retry will help.
	//
	// Genuine upstream failures are untouched. A 500 or an unreachable host
	// classifies as ErrorKindUpstream or not at all, and stays internal.
	if u := provider.Classify(err); u != nil {
		switch u.Kind {
		case provider.ErrorKindUnknownObjectType, provider.ErrorKindUnsupported, provider.ErrorKindBadRequest:
			return backend.StatusBadRequest
		}
	}
	return backend.StatusInternal
}
