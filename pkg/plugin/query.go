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
)

// queryType discriminates the kinds of query the editor can issue.
const (
	queryTypeObjects       = "objects"
	queryTypeAnnotations   = "annotations"
	queryTypeIPEnrichment  = "ip-enrichment"
	queryTypeTopology      = "topology"
	queryTypeTopologyEdges = "topology-edges"
)

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
// fromAlert says this call is an alert-rule evaluation (see isAlertRequest). It
// is an explicit parameter rather than something read back out of the context so
// that every future call site is forced by the compiler to state which it is:
// getting it wrong in the silent direction means alerting on data the query
// itself knows is incomplete.
func (d *Datasource) query(ctx context.Context, q backend.DataQuery, fromAlert bool) backend.DataResponse {
	var qm queryModel
	if err := json.Unmarshal(q.JSON, &qm); err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err))
	}

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
		ips := splitList(qm.IPs)
		if len(ips) == 0 {
			return backend.DataResponse{}
		}
		// A join key's source has to be in the REQUEST, not just in the editor:
		// ResolveIPs projects each row down to the fields asked for, so a source
		// outside the context selection was gone before applyJoinKeys could read
		// it and the output column came out empty for every row. joinOnly names
		// the fields fetched only for that, dropped again below so the join does
		// not silently add a column to the table. See ipEnrichFields.
		fields, joinOnly := ipEnrichFields(qm.ContextFields, qm.JoinKeys)
		res, err := d.provider.ResolveIPs(ctx, ips, fields, qm.Limit)
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
		// stated in a notice — which is exactly why this is gated on fromAlert
		// and not applied unconditionally.
		if fromAlert {
			if msg := truncationError(res, qm.Limit, nounIPs); msg != "" {
				return backend.ErrDataResponse(backend.StatusBadRequest, msg)
			}
			// ResolveIPs caps nothing today, so this cannot fire from here. It is
			// wired anyway because the alternative is the bug this fix exists for
			// in reverse: an alert-facing path that silently ignores a new
			// Result field is exactly how a deliberate cap ended up evaluating as
			// a column of zeroes.
			if msg := capError(res); msg != "" {
				return backend.ErrDataResponse(backend.StatusBadRequest, msg)
			}
			if msg := degradationError(res); msg != "" {
				return backend.ErrDataResponse(backend.StatusBadRequest, msg)
			}
		}
		applyJoinKeys(res, qm.JoinKeys)
		dropColumns(res, joinOnly)
		rewriteLinks(res, d.provider.BaseURL(), d.cfg.PublicURL)
		frame := buildFrame("ip-enrichment", res, d.provider.BaseURL())
		frame.RefID = q.RefID
		// Tell the user when this is only part of the answer. buildFrame always sets
		// Meta, so appending here is safe. nounIPs, not nounObjects: an
		// ip-enrichment Total counts the IPs that were asked about, not objects
		// that matched — plenty of them match nothing.
		frame.Meta.Notices = append(frame.Meta.Notices, resultNotices(res, qm.Limit, nounIPs)...)
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
			// words, as the object and IP-enrichment alert paths.
			if fromAlert {
				if msg := graphTruncationError(graph); msg != "" {
					return backend.ErrDataResponse(backend.StatusBadRequest, msg)
				}
				// Edge-set gaps are refused too, and separately: a device whose
				// links could not all be read looks less connected than it is,
				// which reads as "no working path" and silences a page. Same
				// reason degradationError exists on the object path.
				if msg := graphDegradationError(graph); msg != "" {
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
			Ordering:   queryOrdering(qm.Ordering, fromAlert),
			Limit:      qm.Limit,
		})
		if err != nil {
			return queryErrorResponse(err)
		}
		if qm.ValueField != "" && !slices.Contains(res.Columns, qm.ValueField) {
			return backend.ErrDataResponse(backend.StatusBadRequest,
				fmt.Sprintf("value field %q not found in results — add it to Return fields", qm.ValueField))
		}
		// Grafana alert evaluation cannot see frame notices, so a truncated alert
		// result must fail loudly instead of alerting on an arbitrary subset.
		if msg := truncationError(res, qm.Limit, nounObjects); msg != "" {
			return backend.ErrDataResponse(backend.StatusBadRequest, msg)
		}
		// A capped result is not a degraded one and must not be reported as one:
		// nothing failed, the values present are correct, and the fix is the row
		// limit the rule author already controls. Checked before the degradation
		// branch so the message the user gets names the dial that works — this is
		// the whole content of the bug: a 200-row utilization rule on a healthy
		// NetBox was told its data was degraded and sent to Error state.
		if msg := capError(res); msg != "" {
			return backend.ErrDataResponse(backend.StatusBadRequest, msg)
		}
		// Same reason, different gap: the objects query now degrades as well as
		// truncates. A utilization column whose child lookups failed comes back
		// blank, alert evaluation drops meta.notices, and a blank numeric field
		// evaluates as absent — so a rule watching prefixes over 90% would simply
		// stop firing, looking exactly like prefixes that came back under 90%.
		if msg := degradationError(res); msg != "" {
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
		Ordering:  queryOrdering(qm.Ordering, fromAlert),
		Limit:     qm.Limit,
		// A DASHBOARD table is the one thing that can present a result with no
		// match count: a missing Total costs it the "showing 100 of N" notice and
		// nothing else, and the provider replaces that with a note saying the
		// count is unavailable. It is what lets a datasource opt into cursor
		// paging for its panels without touching alerting, where Total decides
		// whether the rule fires at all. Not set on the count or alert-table
		// branches above, deliberately.
		//
		// And not set for an alert evaluation here either, which is why fromAlert
		// is read. The alertTable flag is the rule author's choice of frame
		// SHAPE, not a statement about who is asking: an ordinary objects query
		// is a perfectly valid alert-rule query (the editor defaults alertTable
		// to false), and with fast paging on it used to reach this line and
		// evaluate the 100 lowest-ID rows with Total 0 — an arbitrary subset
		// reported as "nothing matched", the truncation guard blind because it
		// compares against that same zero, and the gap stated only in a frame
		// notice, which alert evaluation drops. Gating on fromAlert is what makes
		// "alert rules are unaffected by fast paging" — README, the config
		// switch, models.PluginSettings — true of EVERY rule rather than only the
		// two shapes above. It costs an alert rule on a multi-million-row model
		// the count it was skipping; that is the same trade the count and
		// alert-table paths already make, and correctness is the side it is made
		// on.
		AllowUncounted: !fromAlert,
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
	// capError BEFORE degradationError, mirroring the alertTable branch: a cap is
	// not a failure, and its message names the dial the rule author can actually
	// turn — lower the row limit — rather than telling them their NetBox is
	// degraded when nothing went wrong.
	//
	// Only for alerting. A dashboard keeps partial-beats-none: it shows the same
	// gap as a frame notice below and a partial answer there is useful.
	if fromAlert {
		// Truncation first, for the same reason cap precedes degradation: it names
		// the most actionable dial. This branch sets AllowUncounted false for an
		// alert evaluation precisely so Total is a real count here — reading it is
		// what makes that worth paying for. Without this, an alert on the editor's
		// DEFAULT query shape evaluates whatever subset the row limit happened to
		// return: measured live, limit 2 against 4 matching prefixes evaluated two
		// instances and dropped an 86%-utilized prefix entirely, so a
		// "utilization > 90" rule never fired and nothing anywhere said why.
		if msg := truncationError(res, qm.Limit, nounObjects); msg != "" {
			return backend.ErrDataResponse(backend.StatusBadRequest, msg)
		}
		if msg := capError(res); msg != "" {
			return backend.ErrDataResponse(backend.StatusBadRequest, msg)
		}
		if msg := degradationError(res); msg != "" {
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
func queryOrdering(ordering string, fromAlert bool) string {
	if fromAlert {
		return ""
	}
	return ordering
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
	if u := provider.Classify(err); u != nil && u.Kind == provider.ErrorKindUnknownObjectType {
		return backend.StatusBadRequest
	}
	return backend.StatusInternal
}
