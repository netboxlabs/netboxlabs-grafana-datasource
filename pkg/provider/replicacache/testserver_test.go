package replicacache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeService mimics the replica-cache read API closely enough to test against,
// including the behaviours that were MEASURED on a live instance rather than
// read off its spec: the page size ceiling that ignores a larger limit, the
// {"error": …} failure body, and the cursor that is absent on the last page.
type fakeService struct {
	mu sync.Mutex

	// entities maps "app/model" to its rows.
	entities map[string][]map[string]interface{}
	// pageCap is the most rows a single response returns, whatever was asked.
	pageCap int
	// status, when non-zero, is returned for every list request.
	status int
	// errBody is the message sent with status.
	errBody string
	// requests records every list request's query string, for asserting pushdown.
	requests []recordedRequest
	// noSwagger serves a 500 for the API description.
	noSwagger bool
	// hangSwagger blocks the API description until the test finishes, modelling
	// the measured failure where discovery times out while row endpoints answer.
	hangSwagger chan struct{}
	// failEntities names entities that answer 500, to simulate one dimension
	// timing out while the rest of the service is healthy.
	failEntities map[string]bool
	// nullRowsFor names an entity whose rows come back as JSON nulls, which
	// decode without error but leave the row map nil.
	nullRowsFor string
	// idlessRowsFor names an entity whose rows come back as objects with their
	// primary key stripped — object-shaped, but nothing to match a reference
	// against. Stripped on the way OUT so the row still matches the id filter
	// that asked for it, which is what makes this reachable at all.
	idlessRowsFor string
	// failOnce names entities whose FIRST request answers 500 and whose later
	// requests are served normally — a transient failure, which is how the
	// schema probe can fail while the row fetch behind it succeeds.
	failOnce map[string]bool
}

type recordedRequest struct {
	entity string
	query  url.Values
}

func newFakeService() *fakeService {
	return &fakeService{
		entities:     map[string][]map[string]interface{}{},
		failEntities: map[string]bool{},
		failOnce:     map[string]bool{},
		pageCap:      1000,
	}
}

func (f *fakeService) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeService) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/docs/openapi.json" {
		f.mu.Lock()
		hang := f.hangSwagger
		f.requests = append(f.requests, recordedRequest{entity: "docs/openapi.json", query: r.URL.Query()})
		fail := f.noSwagger
		// status applies here too: a revoked token or a wrong instance id is
		// rejected on every path, discovery included.
		status, errBody := f.status, f.errBody
		paths := map[string]interface{}{}
		for e := range f.entities {
			paths["/v1/"+e] = map[string]interface{}{"get": map[string]interface{}{}}
			paths["/v1/"+e+"/{id}"] = map[string]interface{}{"get": map[string]interface{}{}}
		}
		f.mu.Unlock()
		if hang != nil {
			<-hang
		}
		if status != 0 {
			writeErr(w, status, errBody)
			return
		}
		if fail {
			writeErr(w, 500, "server error")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"swagger": "2.0", "paths": paths})
		return
	}

	entity := strings.TrimPrefix(r.URL.Path, "/v1/")
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{entity: entity, query: r.URL.Query()})
	rows, ok := f.entities[entity]
	status, errBody, pageCap := f.status, f.errBody, f.pageCap
	f.mu.Unlock()

	if status != 0 {
		writeErr(w, status, errBody)
		return
	}
	f.mu.Lock()
	broken := f.failEntities[entity]
	if f.failOnce[entity] {
		delete(f.failOnce, entity)
		broken = true
	}
	f.mu.Unlock()
	if broken {
		writeErr(w, 500, "server error")
		return
	}
	if !ok {
		writeErr(w, 404, "endpoint not found")
		return
	}

	q := r.URL.Query()
	// Filtering: only what the tests need, but rejecting an unknown column the
	// way the real service does, so a projection bug surfaces as a failure.
	filtered := rows
	for k, vs := range q {
		if !strings.HasPrefix(k, "filter[") {
			continue
		}
		col := k[len("filter["):strings.Index(k, "]")]
		op := k[strings.Index(k, "]")+3:]
		if len(rows) > 0 {
			if _, exists := rows[0][col]; !exists {
				writeErr(w, 400, "unknown column: "+col)
				return
			}
		}
		var keep []map[string]interface{}
		for _, row := range filtered {
			if matches(row[col], op, vs[0]) {
				keep = append(keep, row)
			}
		}
		filtered = keep
	}

	// The real service rejects a sort on anything but a stored column:
	// sort=site answers 400 "unknown sort column: site" rather than ignoring it.
	// The fake must do the same, or a provider that passes a derived column
	// through looks healthy here and 400s in production.
	if srt := strings.TrimPrefix(q.Get("sort"), "-"); srt != "" && len(rows) > 0 {
		if _, exists := rows[0][srt]; !exists {
			writeErr(w, 400, "unknown sort column: "+srt)
			return
		}
	}

	total := len(filtered)

	start := 0
	if c := q.Get("cursor"); c != "" {
		start, _ = strconv.Atoi(c)
	}
	limit := pageCap
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l < pageCap {
		limit = l
	}
	end := start + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]

	// Projection. The primary key always comes back, as documented.
	if fs := q.Get("fields"); fs != "" {
		want := map[string]bool{"id": true}
		for _, c := range strings.Split(fs, ",") {
			want[c] = true
		}
		projected := make([]map[string]interface{}, 0, len(page))
		for _, row := range page {
			for c := range want {
				if _, exists := row[c]; !exists && c != "id" {
					writeErr(w, 400, "unknown field: "+c)
					return
				}
			}
			cut := map[string]interface{}{}
			for k, v := range row {
				if want[k] {
					cut[k] = v
				}
			}
			projected = append(projected, cut)
		}
		page = projected
	}

	f.mu.Lock()
	nullRows := f.nullRowsFor == entity
	idless := f.idlessRowsFor == entity
	f.mu.Unlock()
	if idless {
		stripped := make([]map[string]interface{}, 0, len(page))
		for _, row := range page {
			cut := map[string]interface{}{}
			for k, v := range row {
				if k != "id" {
					cut[k] = v
				}
			}
			stripped = append(stripped, cut)
		}
		page = stripped
	}
	if nullRows {
		// Valid JSON, valid envelope, unusable rows: each element decodes into
		// a map without error and leaves it nil.
		nulls := make([]interface{}, len(page))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": total, "results": nulls})
		return
	}

	resp := map[string]interface{}{"count": total, "results": page}
	if end < len(filtered) {
		resp["next_cursor"] = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func matches(v interface{}, op, want string) bool {
	s := fmt.Sprintf("%v", v)
	if f, ok := v.(float64); ok {
		s = strconv.FormatFloat(f, 'f', -1, 64)
	}
	switch op {
	case "eq":
		return s == want
	case "in":
		for _, w := range strings.Split(want, ",") {
			if s == w {
				return true
			}
		}
		return false
	case "isnull":
		return (v == nil) == (want == "true")
	case "ilike":
		p := strings.ToLower(strings.Trim(want, "%"))
		return strings.Contains(strings.ToLower(s), p)
	}
	return true
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// requestWith returns the recorded request for an entity that carries a given
// query parameter. Several requests can hit one entity in a single Query — the
// row fetch plus the cached schema sample — so tests that assert pushdown must
// name the one they mean rather than taking the last.
func (f *fakeService) requestWith(entity, key string) (recordedRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.entity == entity && r.query.Get(key) != "" {
			return r, true
		}
	}
	return recordedRequest{}, false
}

func (f *fakeService) countRequestsFor(entity string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.entity == entity {
			n++
		}
	}
	return n
}
