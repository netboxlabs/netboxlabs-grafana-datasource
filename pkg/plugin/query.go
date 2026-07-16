package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netbox/pkg/provider"
	"github.com/netboxlabs/netbox/pkg/provider/netbox"
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
	// ContextFields (for ip-enrichment) selects which prefix columns to return.
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
func (d *Datasource) query(ctx context.Context, q backend.DataQuery) backend.DataResponse {
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
			return backend.ErrDataResponse(backend.StatusInternal, err.Error())
		}
		frame := buildAnnotationsFrame(changes)
		frame.RefID = q.RefID
		return backend.DataResponse{Frames: data.Frames{frame}}

	case queryTypeIPEnrichment:
		ips := splitList(qm.IPs)
		if len(ips) == 0 {
			return backend.DataResponse{}
		}
		res, err := d.provider.ResolveIPs(ctx, ips, qm.ContextFields, qm.Limit)
		if err != nil {
			return backend.ErrDataResponse(backend.StatusInternal, err.Error())
		}
		applyJoinKeys(res, qm.JoinKeys)
		rewriteLinks(res, d.provider.BaseURL(), d.cfg.PublicURL)
		frame := buildFrame("ip-enrichment", res, d.provider.BaseURL())
		frame.RefID = q.RefID
		return backend.DataResponse{Frames: data.Frames{frame}}

	case queryTypeTopology:
		graph, err := d.provider.Topology(ctx, provider.TopologySpec{Filters: qm.Filters, Limit: qm.Limit, ConnectedOnly: qm.ConnectedOnly, Connections: qm.Connections})
		if err != nil {
			return backend.ErrDataResponse(backend.StatusInternal, err.Error())
		}
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
			return backend.ErrDataResponse(backend.StatusInternal, err.Error())
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
			return backend.ErrDataResponse(backend.StatusInternal, err.Error())
		}
		if qm.ValueField != "" && !slices.Contains(res.Columns, qm.ValueField) {
			return backend.ErrDataResponse(backend.StatusBadRequest,
				fmt.Sprintf("value field %q not found in results — add it to Return fields", qm.ValueField))
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
		return backend.ErrDataResponse(backend.StatusInternal, err.Error())
	}

	applyJoinKeys(res, qm.JoinKeys)
	rewriteLinks(res, d.provider.BaseURL(), d.cfg.PublicURL)
	frame := buildFrame(qm.ObjectType, res, d.provider.BaseURL())
	frame.RefID = q.RefID
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
	return "Cannot reach NetBox: " + err.Error()
}
