package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// sanitizeLog strips CR/LF from a user-derived value so it can't forge or inject
// extra log lines (CodeQL: log entries created from user input).
func sanitizeLog(s string) string {
	return strings.NewReplacer("\n", "", "\r", "").Replace(s)
}

// newRouter wires the resource endpoints consumed by the frontend query editor
// and variable support.
func (d *Datasource) newRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/object-types", d.handleObjectTypes)
	mux.HandleFunc("/fields", d.handleFields)
	mux.HandleFunc("/field-values", d.handleFieldValues)
	mux.HandleFunc("/filter-fields", d.handleFilterFields)
	mux.HandleFunc("/query", d.handleQuery)
	mux.HandleFunc("/branching", d.handleBranching)
	return mux
}

// GET /branching -> {"installed": bool}
// Branching detection is instance-wide (not branch-scoped). A provider that does
// not implement provider.BranchingCapable (e.g. a future non-NetBox backend) has
// no branching concept -> {"installed": false}. An inconclusive probe (transient
// upstream failure) fails OPEN -> {"installed": true} so the branch UI stays
// usable rather than latching a false "absent". Always HTTP 200 so this probe
// never raises a frontend error toast.
func (d *Datasource) handleBranching(w http.ResponseWriter, r *http.Request) {
	bc, ok := d.provider.(provider.BranchingCapable)
	if !ok {
		writeJSON(w, map[string]bool{"installed": false})
		return
	}
	installed, conclusive := bc.BranchingInstalled(r.Context())
	if !conclusive {
		installed = true // fail open on an inconclusive probe
	}
	writeJSON(w, map[string]bool{"installed": installed})
}

// GET /object-types -> []provider.ObjectType
// object-types are code-level (branch-invariant); not branch-scoped.
func (d *Datasource) handleObjectTypes(w http.ResponseWriter, r *http.Request) {
	types, err := d.provider.ObjectTypes(r.Context())
	if err != nil {
		writeProviderError(w, err)
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
		writeProviderError(w, err)
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
		writeProviderError(w, err)
		return
	}
	writeJSON(w, values)
}

// GET /filter-fields?type=<objectType>&branch=<branch> -> []provider.FilterField
// On provider error we log and return an empty list (HTTP 200) so the editor
// falls back to the column list + all operators rather than losing filtering.
func (d *Datasource) handleFilterFields(w http.ResponseWriter, r *http.Request) {
	objectType := r.URL.Query().Get("type")
	if objectType == "" {
		writeError(w, http.StatusBadRequest, errMsg("type is required"))
		return
	}
	ctx := provider.WithBranch(r.Context(), r.URL.Query().Get("branch"))
	fields, err := d.provider.FilterFields(ctx, objectType)
	if err != nil {
		log.DefaultLogger.Warn("filter-fields unavailable; editor will fall back", "type", sanitizeLog(objectType), "error", sanitizeLog(err.Error()))
		writeJSON(w, []provider.FilterField{})
		return
	}
	if fields == nil {
		fields = []provider.FilterField{}
	}
	writeJSON(w, fields)
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
		writeProviderError(w, err)
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

// writeProviderError logs the raw provider error (sanitized) and writes the
// user-facing mapped message, so raw NetBox API errors aren't surfaced.
func writeProviderError(w http.ResponseWriter, err error) {
	log.DefaultLogger.Warn("netbox resource error", "detail", sanitizeLog(err.Error()))
	writeError(w, http.StatusBadGateway, errMsg(queryErrorMessage(err)))
}

type errMsg string

func (e errMsg) Error() string { return string(e) }
