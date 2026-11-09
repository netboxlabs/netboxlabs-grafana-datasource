package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// annoScopeServer is a NetBox 4.4-shaped instance for scope resolution: the
// object-type list, the OpenAPI schema, the branch list, and brief list rows
// filtered by id, name or cid, ignoring case as NetBox's device name filter
// does. Every request but the branch list is recorded as path?query, beside the
// X-NetBox-Branch it carried.
type annoScopeServer struct {
	coreStatus  int                 // non-zero: core/object-types answers this status
	rootStatus  int                 // non-zero: /api/ answers this status
	objectTypes []string            // object-type rows, see objectTypeRow
	schema      string              // /api/schema/; "" answers 403
	objects     map[string][]string // list endpoint -> its brief rows
	unfiltered  map[string]bool     // list endpoints that ignore the lookup parameters
	branches    string              // branch list body; "" answers 404 (no netbox-branching)

	mu       sync.Mutex
	requests []string
	headers  []string // X-NetBox-Branch of each recorded request, "" when none
}

// scopeSchema gives devices and interfaces a bare name filter, circuits a cid
// filter, and cables neither: a cable has a label, not a name.
const scopeSchema = `{"paths": {
  "/api/dcim/devices/":      {"get": {"parameters": [{"name": "id", "in": "query"}, {"name": "name", "in": "query"}, {"name": "name__ic", "in": "query"}]}},
  "/api/dcim/interfaces/":   {"get": {"parameters": [{"name": "name", "in": "query"}]}},
  "/api/circuits/circuits/": {"get": {"parameters": [{"name": "cid", "in": "query"}]}},
  "/api/dcim/cables/":       {"get": {"parameters": [{"name": "label", "in": "query"}]}}
}}`

func newAnnoScopeServer() *annoScopeServer {
	return &annoScopeServer{
		objectTypes: []string{
			objectTypeRow(12, "dcim.device", "/api/dcim/devices/"),
			objectTypeRow(14, "dcim.interface", "/api/dcim/interfaces/"),
			objectTypeRow(30, "circuits.circuit", "/api/circuits/circuits/"),
			objectTypeRow(17, "dcim.cable", "/api/dcim/cables/"),
			objectTypeRow(40, "dcim.cablepath", "null"),
		},
		schema: scopeSchema,
		objects: map[string][]string{
			"dcim/devices": {deviceRow(17, "leaf1"), deviceRow(18, "leaf2"), deviceRow(19, "spine1")},
		},
	}
}

// briefRow is one brief list row: id, url, display and the lookup field.
func briefRow(endpoint string, id int64, field, value, display string) string {
	return fmt.Sprintf(`{"id":%d,"url":"http://nb/api/%s/%d/","display":%q,%q:%q}`, id, endpoint, id, display, field, value)
}

func deviceRow(id int64, name string) string {
	return briefRow("dcim/devices", id, "name", name, name)
}

func interfaceRows(firstID int64, n int, name string) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = briefRow("dcim/interfaces", firstID+int64(i), "name", name, name)
	}
	return rows
}

func (s *annoScopeServer) start(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		refuse := func(status int) {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `{"detail":"nope"}`)
		}
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
		if path == "plugins/branching/branches" {
			if s.branches == "" {
				refuse(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprint(w, s.branches)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.Path+"?"+r.URL.RawQuery)
		s.headers = append(s.headers, r.Header.Get("X-NetBox-Branch"))
		s.mu.Unlock()
		switch {
		case path == "core/object-types":
			if s.coreStatus != 0 {
				refuse(s.coreStatus)
				return
			}
			writePage(w, len(s.objectTypes), s.objectTypes)
		case path == "schema":
			if s.schema == "" {
				refuse(http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, s.schema)
		case path == "" && s.rootStatus != 0:
			refuse(s.rootStatus)
		case s.objects[path] != nil:
			rows := s.objects[path]
			if !s.unfiltered[path] {
				rows = lookupMatches(rows, r.URL.Query())
			}
			total := len(rows)
			if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && limit < len(rows) {
				rows = rows[:limit]
			}
			writePage(w, total, rows)
		default:
			refuse(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", srv.Client())
}

// lookupMatches keeps the rows whose id, name and cid each equal, ignoring case,
// one of the request's values for that parameter, when the request has it.
func lookupMatches(rows []string, q url.Values) []string {
	var out []string
	for _, row := range rows {
		var obj map[string]any
		if json.Unmarshal([]byte(row), &obj) != nil {
			continue
		}
		keep := true
		for _, param := range []string{"id", "name", "cid"} {
			want, ok := q[param]
			if !ok {
				continue
			}
			have := fmt.Sprint(obj[param])
			if !slices.ContainsFunc(want, func(w string) bool { return strings.EqualFold(w, have) }) {
				keep = false
			}
		}
		if keep {
			out = append(out, row)
		}
	}
	return out
}

func (s *annoScopeServer) allRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// queries returns the query of every request to one exact path, in order.
func (s *annoScopeServer) queries(path string) []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []url.Values
	for _, req := range s.requests {
		p, raw, _ := strings.Cut(req, "?")
		if p == path {
			q, _ := url.ParseQuery(raw)
			out = append(out, q)
		}
	}
	return out
}

func nameScope(values ...string) provider.ChangeSpec {
	return provider.ChangeSpec{ScopeSet: true, Scope: values}
}

func idScope(values ...string) provider.ChangeSpec {
	spec := nameScope(values...)
	spec.ScopeMatch = "id"
	return spec
}

// annotationErr asserts that err is an AnnotationError for reason, and returns
// a copy of it.
func annotationErr(t *testing.T, err error, reason string) AnnotationError {
	t.Helper()
	var ae *AnnotationError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an AnnotationError (%s)", err, reason)
	}
	if ae.Reason != reason {
		t.Fatalf("reason = %q, want %q (%v)", ae.Reason, reason, err)
	}
	return *ae
}

func resolveOK(t *testing.T, p *Provider, spec provider.ChangeSpec) *resolvedScope {
	t.Helper()
	scope, err := p.resolveAnnotationScope(context.Background(), spec)
	if err != nil {
		t.Fatalf("resolveAnnotationScope: %v", err)
	}
	if scope == nil {
		t.Fatal("resolveAnnotationScope returned no scope for a scoped query")
	}
	return scope
}

func wantObjects(t *testing.T, scope *resolvedScope, want ...scopeObject) {
	t.Helper()
	if !reflect.DeepEqual(scope.Objects, want) {
		t.Errorf("objects = %+v, want %+v", scope.Objects, want)
	}
}

func TestAnnotationScope_Unscoped(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	for _, spec := range []provider.ChangeSpec{{}, {ScopeType: "dcim.interface", ScopeMatch: "id"}} {
		scope, err := p.resolveAnnotationScope(context.Background(), spec)
		if scope != nil || err != nil {
			t.Errorf("resolveAnnotationScope(%+v) = %+v, %v; want nil, nil", spec, scope, err)
		}
	}
	if got := s.allRequests(); len(got) != 0 {
		t.Errorf("requests = %v, want none for an unscoped query", got)
	}
}

func TestAnnotationScope_AllIsUnscoped(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	scope, err := p.resolveAnnotationScope(context.Background(), provider.ChangeSpec{ScopeSet: true, ScopeAll: true})
	if scope != nil || err != nil {
		t.Errorf("resolveAnnotationScope($__all) = %+v, %v; want nil, nil", scope, err)
	}
	if got := s.allRequests(); len(got) != 0 {
		t.Errorf("requests = %v, want none for $__all", got)
	}
}

// A scope that was configured but interpolated to nothing must not widen the
// query to all of NetBox.
func TestAnnotationScope_EmptyConfiguredScopeIsAnError(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	for _, spec := range []provider.ChangeSpec{{ScopeSet: true}, nameScope(" ", "")} {
		_, err := p.resolveAnnotationScope(context.Background(), spec)
		annotationErr(t, err, provider.AnnotationReasonEmptyScope)
	}
	if got := s.allRequests(); len(got) != 0 {
		t.Errorf("requests = %v, want none for an empty scope", got)
	}
}

func TestAnnotationScope_UnknownTypeIsUnknownObjectTypeError(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	spec := nameScope("leaf1")
	spec.ScopeType = " DCIM.NoSuchThing "
	_, err := p.resolveAnnotationScope(context.Background(), spec)
	var unknown *UnknownObjectTypeError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want an UnknownObjectTypeError", err)
	}
	if unknown.Type != "dcim.nosuchthing" || unknown.Known != len(s.objectTypes) {
		t.Errorf("error = %+v, want type dcim.nosuchthing and %d known types", unknown, len(s.objectTypes))
	}
	for _, req := range s.allRequests() {
		if !strings.HasPrefix(req, "/api/core/object-types/") {
			t.Errorf("request %q made for an unknown type", req)
		}
	}
}

func TestAnnotationScope_ByNameOneBatchedRequest(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	scope := resolveOK(t, p, nameScope("leaf1", "leaf2", "spine1"))

	if scope.Type != "dcim.device" || scope.TypeID != 12 || scope.Endpoint != "dcim/devices" {
		t.Errorf("scope = %q (%d) at %q, want dcim.device (12) at dcim/devices", scope.Type, scope.TypeID, scope.Endpoint)
	}
	wantObjects(t, scope, scopeObject{17, "leaf1"}, scopeObject{18, "leaf2"}, scopeObject{19, "spine1"})
	qs := s.queries("/api/dcim/devices/")
	if len(qs) != 1 {
		t.Fatalf("device lookups = %d, want 1", len(qs))
	}
	q := qs[0]
	if got := q["name"]; !reflect.DeepEqual(got, []string{"leaf1", "leaf2", "spine1"}) {
		t.Errorf("name = %v, want the three names in one request", got)
	}
	if q.Get("brief") != "true" || q.Get("limit") != "61" {
		t.Errorf("brief = %q, limit = %q; want true and 61 (3 names x 20 + 1)", q.Get("brief"), q.Get("limit"))
	}
}

// Device names match case-insensitively in NetBox, so the bucket must too. The
// object's name is NetBox's display, not the value as typed.
func TestAnnotationScope_NameCaseInsensitiveBucket(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	wantObjects(t, resolveOK(t, p, nameScope("LEAF1")), scopeObject{17, "leaf1"})
}

// Devices at different sites can share a name; every one of them is in scope.
func TestAnnotationScope_SharedNameKeepsAll(t *testing.T) {
	s := newAnnoScopeServer()
	s.objects["dcim/devices"] = append(s.objects["dcim/devices"], deviceRow(31, "leaf1"))
	p := s.start(t)
	wantObjects(t, resolveOK(t, p, nameScope("leaf1", "spine1")),
		scopeObject{17, "leaf1"}, scopeObject{31, "leaf1"}, scopeObject{19, "spine1"})
}

func TestAnnotationScope_NoMatch(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	_, err := p.resolveAnnotationScope(context.Background(), nameScope("leaf1", "nope"))
	if ae := annotationErr(t, err, provider.AnnotationReasonNoMatch); ae.Value != "nope" {
		t.Errorf("value = %q, want nope", ae.Value)
	}
}

func TestAnnotationScope_TooManyMatches(t *testing.T) {
	interfaces := func() provider.ChangeSpec {
		spec := nameScope("Gi0/1")
		spec.ScopeType = "dcim.interface"
		return spec
	}

	t.Run("a name over twenty", func(t *testing.T) {
		s := newAnnoScopeServer()
		s.objects["dcim/interfaces"] = interfaceRows(100, 21, "Gi0/1")
		p := s.start(t)
		_, err := p.resolveAnnotationScope(context.Background(), interfaces())
		if ae := annotationErr(t, err, provider.AnnotationReasonTooManyMatches); ae.Value != "Gi0/1" {
			t.Errorf("value = %q, want Gi0/1", ae.Value)
		}
	})

	t.Run("twenty is fine", func(t *testing.T) {
		s := newAnnoScopeServer()
		s.objects["dcim/interfaces"] = interfaceRows(100, 20, "Gi0/1")
		p := s.start(t)
		if n := len(resolveOK(t, p, interfaces()).Objects); n != 20 {
			t.Errorf("objects = %d, want 20", n)
		}
	})

	// A chunk that fills its limit has a name over twenty somewhere, even when
	// the rows that filled it match none of the names: NetBox ignored the
	// filter.
	t.Run("a chunk at its limit", func(t *testing.T) {
		s := newAnnoScopeServer()
		s.objects["dcim/interfaces"] = append(interfaceRows(100, 10, "eth0"), interfaceRows(200, 40, "xe-0/0/0")...)
		s.unfiltered = map[string]bool{"dcim/interfaces": true}
		p := s.start(t)
		spec := interfaces()
		spec.Scope = []string{"eth0", "eth1"}
		_, err := p.resolveAnnotationScope(context.Background(), spec)
		if ae := annotationErr(t, err, provider.AnnotationReasonTooManyMatches); ae.Value != "eth0" {
			t.Errorf("value = %q, want eth0 (the name with the most matches)", ae.Value)
		}
	})
}

// Under Match by Name, digits are a name too.
func TestAnnotationScope_NumericNameIsAName(t *testing.T) {
	s := newAnnoScopeServer()
	s.objects["dcim/devices"] = append(s.objects["dcim/devices"], deviceRow(9, "1234"), deviceRow(1234, "other"))
	p := s.start(t)
	wantObjects(t, resolveOK(t, p, nameScope("1234")), scopeObject{9, "1234"})
	for _, q := range s.queries("/api/dcim/devices/") {
		if q.Has("id") || q.Get("name") != "1234" {
			t.Errorf("lookup = %v, want name=1234 and no id", q)
		}
	}
}

// Circuits have no name; they are looked up by cid.
func TestAnnotationScope_CircuitUsesCid(t *testing.T) {
	s := newAnnoScopeServer()
	s.objects["circuits/circuits"] = []string{
		briefRow("circuits/circuits", 5, "cid", "CID-100", "CID-100"),
		briefRow("circuits/circuits", 6, "cid", "CID-200", "CID-200"),
	}
	p := s.start(t)
	spec := nameScope("CID-100")
	spec.ScopeType = "circuits.circuit"
	wantObjects(t, resolveOK(t, p, spec), scopeObject{5, "CID-100"})
	qs := s.queries("/api/circuits/circuits/")
	if len(qs) != 1 || qs[0].Get("cid") != "CID-100" || qs[0].Has("name") {
		t.Errorf("circuit lookups = %v, want one with cid=CID-100 and no name", qs)
	}
}

func TestAnnotationScope_TypeWithoutNameFilter(t *testing.T) {
	s := newAnnoScopeServer()
	s.objects["dcim/cables"] = []string{briefRow("dcim/cables", 1, "label", "c1", "#1")}
	p := s.start(t)
	spec := nameScope("c1")
	spec.ScopeType = "dcim.cable"
	_, err := p.resolveAnnotationScope(context.Background(), spec)
	annotationErr(t, err, provider.AnnotationReasonNoNameFilter)
	if qs := s.queries("/api/dcim/cables/"); len(qs) != 0 {
		t.Errorf("cable lookups = %v, want none", qs)
	}
}

// The name-filter check is a better error, not the lookup itself: a schema that
// can't be read skips it.
func TestAnnotationScope_SchemaUnreadableStillLooksUp(t *testing.T) {
	s := newAnnoScopeServer()
	s.schema = ""
	p := s.start(t)
	wantObjects(t, resolveOK(t, p, nameScope("leaf1")), scopeObject{17, "leaf1"})
}

func TestAnnotationScope_NoEndpointUnderName(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	spec := nameScope("p1")
	spec.ScopeType = "dcim.cablepath"
	_, err := p.resolveAnnotationScope(context.Background(), spec)
	annotationErr(t, err, provider.AnnotationReasonNoEndpoint)

	t.Run("under ID the endpoint is optional", func(t *testing.T) {
		spec := idScope("5")
		spec.ScopeType = "dcim.cablepath"
		scope := resolveOK(t, p, spec)
		if scope.Endpoint != "" || scope.TypeID != 40 {
			t.Errorf("scope endpoint = %q, type id = %d; want none and 40", scope.Endpoint, scope.TypeID)
		}
		wantObjects(t, scope, scopeObject{5, ""})
	})
}

// A failed endpoint discovery is NetBox failing, not the query being wrong, so
// it comes back as the upstream error it is.
func TestAnnotationScope_DiscoveryFailureIsUpstream(t *testing.T) {
	s := newAnnoScopeServer()
	s.coreStatus = http.StatusInternalServerError
	s.rootStatus = http.StatusInternalServerError
	p := s.start(t)
	_, err := p.resolveAnnotationScope(context.Background(), nameScope("leaf1"))
	var ae *AnnotationError
	if errors.As(err, &ae) {
		t.Fatalf("err = %v, want the discovery failure, not an AnnotationError", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("err = %v, want NetBox's HTTP 500", err)
	}

	t.Run("under ID the scope carries on", func(t *testing.T) {
		scope := resolveOK(t, p, idScope("17"))
		if scope.Type != "dcim.device" || scope.TypeID != 0 || scope.Endpoint != "" {
			t.Errorf("scope = %q (%d) at %q, want dcim.device (0) with no endpoint", scope.Type, scope.TypeID, scope.Endpoint)
		}
		wantObjects(t, scope, scopeObject{17, ""})
	})
}

func TestAnnotationScope_ByIDRejectsNonIntegerAndOverflow(t *testing.T) {
	for _, v := range []string{"sw1", "99999999999999999999", "0", "-3", "1.5"} {
		t.Run(v, func(t *testing.T) {
			s := newAnnoScopeServer()
			p := s.start(t)
			_, err := p.resolveAnnotationScope(context.Background(), idScope("17", v))
			if ae := annotationErr(t, err, provider.AnnotationReasonNotAnID); ae.Value != v {
				t.Errorf("value = %q, want %q", ae.Value, v)
			}
			if got := s.allRequests(); len(got) != 0 {
				t.Errorf("requests = %v, want none for a bad id", got)
			}
		})
	}
}

// An id is not checked against NetBox: a deleted object's id still has history.
// Names are looked up once, best effort.
func TestAnnotationScope_ByIDDeletedObjectIsFine(t *testing.T) {
	s := newAnnoScopeServer()
	p := s.start(t)
	scope := resolveOK(t, p, idScope("17", "999"))
	if scope.TypeID != 12 || scope.Endpoint != "dcim/devices" {
		t.Errorf("type id = %d, endpoint = %q; want 12 and dcim/devices", scope.TypeID, scope.Endpoint)
	}
	wantObjects(t, scope, scopeObject{17, "leaf1"}, scopeObject{999, ""})
	qs := s.queries("/api/dcim/devices/")
	if len(qs) != 1 || !reflect.DeepEqual(qs[0]["id"], []string{"17", "999"}) || qs[0].Get("brief") != "true" {
		t.Errorf("device lookups = %v, want one brief lookup for ids 17 and 999", qs)
	}
}

func TestAnnotationScope_TooManyObjects(t *testing.T) {
	t.Run("values", func(t *testing.T) {
		s := newAnnoScopeServer()
		p := s.start(t)
		values := make([]string, 101)
		for i := range values {
			values[i] = strconv.Itoa(i + 1)
		}
		for _, spec := range []provider.ChangeSpec{nameScope(values...), idScope(values...)} {
			_, err := p.resolveAnnotationScope(context.Background(), spec)
			if ae := annotationErr(t, err, provider.AnnotationReasonTooManyObjects); ae.Count != 101 {
				t.Errorf("count = %d, want 101", ae.Count)
			}
		}
		if got := s.allRequests(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})

	t.Run("resolved", func(t *testing.T) {
		s := newAnnoScopeServer()
		var names []string
		for i := range 6 {
			name := fmt.Sprintf("eth%d", i)
			names = append(names, name)
			s.objects["dcim/interfaces"] = append(s.objects["dcim/interfaces"], interfaceRows(int64(100*(i+1)), 20, name)...)
		}
		p := s.start(t)
		spec := nameScope(names...)
		spec.ScopeType = "dcim.interface"
		_, err := p.resolveAnnotationScope(context.Background(), spec)
		if ae := annotationErr(t, err, provider.AnnotationReasonTooManyObjects); ae.Count != 120 {
			t.Errorf("count = %d, want 120", ae.Count)
		}
	})
}

func TestAnnotationScope_CarriesBranch(t *testing.T) {
	for name, spec := range map[string]provider.ChangeSpec{"by name": nameScope("leaf1"), "by id": idScope("17")} {
		t.Run(name, func(t *testing.T) {
			s := newAnnoScopeServer()
			s.branches = `{"count":1,"next":null,"results":[{"name":"br1","schema_id":"td5smq0f"}]}`
			p := s.start(t)
			ctx := provider.WithBranch(context.Background(), "br1")
			if _, err := p.resolveAnnotationScope(ctx, spec); err != nil {
				t.Fatalf("resolveAnnotationScope: %v", err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.requests) == 0 {
				t.Fatal("no requests recorded")
			}
			for i, h := range s.headers {
				if h != "td5smq0f" {
					t.Errorf("%s sent X-NetBox-Branch %q, want td5smq0f", s.requests[i], h)
				}
			}
		})
	}
}
