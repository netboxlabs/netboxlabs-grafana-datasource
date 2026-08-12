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
	queryTypeObjects      = "objects"
	queryTypeAnnotations  = "annotations"
	queryTypeIPEnrichment = "ip-enrichment"
	queryTypeTopology     = "topology"
)

// queryModel is the JSON shape sent by the frontend query/variable/annotation
// editors.
type queryModel struct {
	QueryType  string            `json:"queryType"`
	ObjectType string            `json:"objectType"`
	Filters    []provider.Filter `json:"filters"`
	Fields     []string          `json:"fields"`
	Limit      int               `json:"limit"`
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

	case queryTypeTopology:
		graph, err := d.provider.Topology(ctx, provider.TopologySpec{Filters: qm.Filters, Limit: qm.Limit, ConnectedOnly: qm.ConnectedOnly, Connections: qm.Connections})
		if err != nil {
			return queryErrorResponse(err)
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
		// the total, not a page of rows.
		res, err := d.provider.Query(ctx, provider.QuerySpec{
			ObjectType: qm.ObjectType,
			Filters:    qm.Filters,
			Limit:      1,
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
		Limit:      qm.Limit,
	})
	if err != nil {
		return queryErrorResponse(err)
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
	var apiErr *netbox.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case 401, 403:
			return "Authentication failed (check API token)"
		case 404:
			return "NetBox API not found at this URL (check the base URL)"
		}
		return fmt.Sprintf("NetBox returned HTTP %d", apiErr.Status)
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
	var apiErr *netbox.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case 405:
			return "This object type can't be queried — the NetBox endpoint doesn't support listing (HTTP 405). It may be an action endpoint, not a queryable collection."
		case 400:
			// netbox-branching rejects an unknown branch with this exact 400. The
			// Branch field accepts a branch name or schema id (names resolve to the
			// schema id); a 400 here means neither matched a real branch.
			if strings.Contains(apiErr.Body, "Invalid branch identifier") {
				return "NetBox didn't recognize that branch. Check the branch name or schema id against the branch list."
			}
			return "NetBox rejected this query (HTTP 400). Check the filters and try again."
		case 401, 403:
			return "Authentication failed (check the API token)."
		case 404:
			return "This object type was not found in NetBox (HTTP 404)."
		case 500:
			// QuerySetNotOrdered appears near the start of NetBox's error body,
			// well within snippet()'s 300-char cap. A longer body (e.g. a debug
			// traceback) could push the token past the cap; the match then falls
			// through to the generic HTTP 500 message below — still safe.
			if strings.Contains(apiErr.Body, "QuerySetNotOrdered") {
				return "NetBox couldn't list this object type — the endpoint doesn't support pagination (HTTP 500). This model may not be queryable."
			}
		}
		return fmt.Sprintf("NetBox returned HTTP %d for this object type.", apiErr.Status)
	}
	return "Couldn't reach NetBox: " + upstreamDetail(err)
}

// queryErrorResponse logs the raw upstream error (sanitized) and returns a data
// response carrying only the user-facing mapped message.
func queryErrorResponse(err error) backend.DataResponse {
	log.DefaultLogger.Warn("netbox query error", "detail", sanitizeLog(err.Error()))
	return backend.ErrDataResponse(backend.StatusInternal, queryErrorMessage(err))
}
