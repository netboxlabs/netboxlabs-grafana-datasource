package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// newRouter wires the resource endpoints consumed by the frontend query editor
// and variable support.
func (d *Datasource) newRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/object-types", d.handleObjectTypes)
	mux.HandleFunc("/fields", d.handleFields)
	mux.HandleFunc("/field-values", d.handleFieldValues)
	mux.HandleFunc("/query", d.handleQuery)
	return mux
}

// GET /object-types -> []provider.ObjectType
// object-types are code-level (branch-invariant); not branch-scoped.
func (d *Datasource) handleObjectTypes(w http.ResponseWriter, r *http.Request) {
	types, err := d.provider.ObjectTypes(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, types)
}

// GET /fields?type=<objectType>&branch=<branch> -> []provider.Field
func (d *Datasource) handleFields(w http.ResponseWriter, r *http.Request) {
	objectType := r.URL.Query().Get("type")
	if objectType == "" {
		writeError(w, http.StatusBadRequest, errMsg("type is required"))
		return
	}
	ctx := provider.WithBranch(r.Context(), r.URL.Query().Get("branch"))
	fields, err := d.provider.Fields(ctx, objectType)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, fields)
}

// GET /field-values?type=<objectType>&field=<field>&q=<substr>&limit=<n>&branch=<branch> -> []string
func (d *Datasource) handleFieldValues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	objectType := q.Get("type")
	field := q.Get("field")
	if objectType == "" || field == "" {
		writeError(w, http.StatusBadRequest, errMsg("type and field are required"))
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	ctx := provider.WithBranch(r.Context(), q.Get("branch"))
	values, err := d.provider.FieldValues(ctx, objectType, field, q.Get("q"), limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, values)
}

// queryResourceRequest is the body for POST /query, used by variable queries and
// the query-editor preview.
type queryResourceRequest struct {
	ObjectType string            `json:"objectType"`
	Filters    []provider.Filter `json:"filters"`
	Fields     []string          `json:"fields"`
	Limit      int               `json:"limit"`
	Branch     string            `json:"branch"`
}

// POST /query -> provider.Result {columns, rows}
func (d *Datasource) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errMsg("POST required"))
		return
	}
	var req queryResourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ObjectType == "" {
		writeError(w, http.StatusBadRequest, errMsg("objectType is required"))
		return
	}
	ctx := provider.WithBranch(r.Context(), req.Branch)
	res, err := d.provider.Query(ctx, provider.QuerySpec{
		ObjectType: req.ObjectType,
		Filters:    req.Filters,
		Fields:     req.Fields,
		Limit:      req.Limit,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	rewriteLinks(res, d.provider.BaseURL(), d.cfg.PublicURL)
	writeJSON(w, res)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.DefaultLogger.Error("encode resource response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

type errMsg string

func (e errMsg) Error() string { return string(e) }
