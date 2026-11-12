package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// typeServer is a NetBox shaped for what endpoint and name resolution read: the
// object-type list (core/ from 4.4, extras/ on 4.2 and 4.3), the API index tree
// ObjectTypes walks, the OpenAPI schema, brief id lookups on list endpoints, and
// the change log. Every request is recorded as path?query.
type typeServer struct {
	coreStatus   int                         // non-zero: core/object-types answers this status
	extrasServes bool                        // extras/object-types answers the rows (4.2/4.3); 404 otherwise
	objectTypes  []string                    // object-type rows, see objectTypeRow
	rootStatus   int                         // non-zero: /api/ answers this status
	index        map[string][]string         // API index pages: "dcim" -> ["devices"], "plugins/bgp" -> ["session"]
	schema       string                      // /api/schema/; "" answers 403
	schemaDelay  time.Duration               // held before /api/schema/ answers
	objects      map[string]map[int64]string // list endpoint -> id -> display, served to ?id= lookups
	failing      map[string]int              // list endpoint -> status it answers with
	changes      []string                    // core/object-changes rows

	mu       sync.Mutex
	requests []string
}

func (s *typeServer) start(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.Path+"?"+r.URL.RawQuery)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		refuse := func(status int) {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `{"detail":"nope"}`)
		}
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
		switch {
		case path == "core/object-types":
			if s.coreStatus != 0 {
				refuse(s.coreStatus)
				return
			}
			writePage(w, len(s.objectTypes), s.objectTypes)
		case path == "extras/object-types":
			if !s.extrasServes {
				refuse(http.StatusNotFound)
				return
			}
			writePage(w, len(s.objectTypes), s.objectTypes)
		case path == "schema":
			time.Sleep(s.schemaDelay)
			if s.schema == "" {
				refuse(http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, s.schema)
		case path == "core/object-changes":
			writePage(w, len(s.changes), s.changes)
		case path == "":
			if s.rootStatus != 0 {
				refuse(s.rootStatus)
				return
			}
			var apps []string
			for k := range s.index {
				if !strings.Contains(k, "/") {
					apps = append(apps, k)
				}
			}
			writeIndex(w, r, "", apps)
		case s.index[path] != nil:
			writeIndex(w, r, path, s.index[path])
		case s.failing[path] != 0:
			refuse(s.failing[path])
		case s.objects[path] != nil:
			var rows []string
			for _, v := range r.URL.Query()["id"] {
				id, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					refuse(http.StatusBadRequest)
					return
				}
				if name, ok := s.objects[path][id]; ok {
					rows = append(rows, fmt.Sprintf(`{"id":%d,"url":"http://nb/api/%s/%d/","display":%q}`, id, path, id, name))
				}
			}
			writePage(w, len(rows), rows)
		default:
			refuse(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", srv.Client())
}

// writeIndex answers an API index page: {key: absolute URL}, as NetBox does.
func writeIndex(w http.ResponseWriter, r *http.Request, path string, keys []string) {
	prefix := "http://" + r.Host + "/api/"
	if path != "" {
		prefix += path + "/"
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%q:%q`, k, prefix+k+"/"))
	}
	_, _ = fmt.Fprintf(w, `{%s}`, strings.Join(parts, ","))
}

// hits counts requests to one exact path ("/api/dcim/devices/").
func (s *typeServer) hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, req := range s.requests {
		if p, _, _ := strings.Cut(req, "?"); p == path {
			n++
		}
	}
	return n
}

// queries returns the query of every request to one exact path, in order.
func (s *typeServer) queries(path string) []url.Values {
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

func (s *typeServer) allRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *typeServer) objectTypeRequests() int { return s.hits("/api/core/object-types/") }

// objectTypeRow is one object-type list row. rest is the rest_api_endpoint: ""
// leaves the key out (NetBox 4.2/4.3), "null" sends null (a model with no API).
func objectTypeRow(id int, ct, rest string) string {
	app, model, _ := strings.Cut(ct, ".")
	switch rest {
	case "":
		return fmt.Sprintf(`{"id":%d,"app_label":%q,"model":%q}`, id, app, model)
	case "null":
		return fmt.Sprintf(`{"id":%d,"app_label":%q,"model":%q,"rest_api_endpoint":null}`, id, app, model)
	}
	return fmt.Sprintf(`{"id":%d,"app_label":%q,"model":%q,"rest_api_endpoint":%q}`, id, app, model, rest)
}

// NetBox 4.2 and 4.3 serve the object-type list only at extras/object-types, and
// answer core/object-types with a 404. The ids must still resolve there.
func TestObjectTypes_FallsBackToExtrasOn404(t *testing.T) {
	s := &typeServer{
		coreStatus:   http.StatusNotFound,
		extrasServes: true,
		objectTypes:  []string{objectTypeRow(12, "dcim.device", ""), objectTypeRow(95, "ipam.ipaddress", "")},
	}
	p := s.start(t)

	ids, err := p.objectTypeIDMap(context.Background())
	if err != nil {
		t.Fatalf("objectTypeIDMap: %v", err)
	}
	if ids["dcim.device"] != 12 || ids["ipam.ipaddress"] != 95 {
		t.Errorf("ids = %v, want dcim.device 12 and ipam.ipaddress 95", ids)
	}
	if n := s.hits("/api/extras/object-types/"); n != 1 {
		t.Errorf("extras/object-types read %d times, want 1", n)
	}

	t.Run("only a 404 falls back", func(t *testing.T) {
		forbidden := &typeServer{coreStatus: http.StatusForbidden, extrasServes: true, objectTypes: s.objectTypes}
		p := forbidden.start(t)
		if _, err := p.objectTypeIDMap(context.Background()); err == nil {
			t.Fatal("objectTypeIDMap succeeded behind a 403 from core/object-types")
		}
		if n := forbidden.hits("/api/extras/object-types/"); n != 0 {
			t.Errorf("extras/object-types read %d times after a 403, want 0: core/ exists there, the token just can't read it", n)
		}
	})
}

// rest_api_endpoint rides on the read the id map already makes.
func TestObjectTypes_EndpointsCostNoExtraRequest(t *testing.T) {
	s := &typeServer{objectTypes: []string{
		objectTypeRow(12, "dcim.device", "/api/dcim/devices/"),
		objectTypeRow(95, "ipam.ipaddress", "/api/ipam/ip-addresses/"),
	}}
	p := s.start(t)
	ctx := context.Background()

	if _, err := p.objectTypeIDMap(ctx); err != nil {
		t.Fatalf("objectTypeIDMap: %v", err)
	}
	endpoint, found, err := p.contentTypeEndpoint(ctx, "ipam.ipaddress")
	if err != nil || !found || endpoint != "ipam/ip-addresses" {
		t.Errorf("contentTypeEndpoint(ipam.ipaddress) = %q, %v, %v; want ipam/ip-addresses, true, nil", endpoint, found, err)
	}
	if n := s.objectTypeRequests(); n != 1 {
		t.Errorf("object types read %d times for the ids and the endpoints, want 1", n)
	}
	if got := s.allRequests(); len(got) != 1 {
		t.Errorf("requests = %v, want only the one object-type read", got)
	}
}

// rest_api_endpoint is read beside the id, never instead of it.
func TestObjectTypes_OddRestAPIEndpointKeepsTheID(t *testing.T) {
	s := &typeServer{objectTypes: []string{
		`{"id":12,"app_label":"dcim","model":"device","rest_api_endpoint":{"list":"/api/dcim/devices/"}}`,
		objectTypeRow(95, "ipam.ipaddress", "/api/ipam/ip-addresses/"),
	}}
	p := s.start(t)

	ids, err := p.objectTypeIDMap(context.Background())
	if err != nil {
		t.Fatalf("objectTypeIDMap: %v", err)
	}
	if ids["dcim.device"] != 12 {
		t.Errorf("ids = %v, want dcim.device 12 despite its unreadable rest_api_endpoint", ids)
	}
	if _, found, err := p.contentTypeEndpoint(context.Background(), "dcim.device"); found || err != nil {
		t.Errorf("contentTypeEndpoint(dcim.device) found = %v, err = %v; want not found, no error", found, err)
	}
}

func TestContentTypeEndpoint_FromRestAPIEndpoint(t *testing.T) {
	s := &typeServer{objectTypes: []string{
		// NetBox's BASE_PATH is part of rest_api_endpoint.
		objectTypeRow(12, "dcim.device", "/netbox/api/dcim/devices/"),
		objectTypeRow(201, "netbox_bgp.bgpsession", "/api/plugins/bgp/session/"),
		objectTypeRow(40, "dcim.cablepath", "null"),
	}}
	p := s.start(t)

	cases := []struct {
		ct, want  string
		wantFound bool
	}{
		{"dcim.device", "dcim/devices", true},
		{"netbox_bgp.bgpsession", "plugins/bgp/session", true},
		{" DCIM.Device ", "dcim/devices", true},
		// The instance reports endpoints, so a type without one has none.
		{"dcim.cablepath", "", false},
	}
	for _, c := range cases {
		endpoint, found, err := p.contentTypeEndpoint(context.Background(), c.ct)
		if err != nil || found != c.wantFound || endpoint != c.want {
			t.Errorf("contentTypeEndpoint(%q) = %q, %v, %v; want %q, %v, nil", c.ct, endpoint, found, err, c.want, c.wantFound)
		}
	}
	if n := s.hits("/api/"); n != 0 {
		t.Errorf("the API index was walked %d times; rest_api_endpoint answers without it", n)
	}
}

// A BASE_PATH can hold "/api/" itself. The instance's own API root is
// stripped from rest_api_endpoint, not everything up to the first "/api/",
// which would leave "netbox/api/dcim/devices". A value outside that root, as
// behind a proxy that rewrites the path, still falls back to the first /api/.
func TestContentTypeEndpoint_BasePathHoldingAPI(t *testing.T) {
	rows := []string{
		objectTypeRow(12, "dcim.device", "/api/netbox/api/dcim/devices/"),
		objectTypeRow(14, "dcim.interface", "/other/api/dcim/interfaces/"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/netbox/api/core/object-types/" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"detail":"Not found."}`)
			return
		}
		writePage(w, len(rows), rows)
	}))
	t.Cleanup(srv.Close)
	p := New(srv.URL+"/api/netbox/", "token", srv.Client())

	for ct, want := range map[string]string{"dcim.device": "dcim/devices", "dcim.interface": "dcim/interfaces"} {
		if endpoint, found, err := p.contentTypeEndpoint(context.Background(), ct); err != nil || !found || endpoint != want {
			t.Errorf("contentTypeEndpoint(%s) = %q, %v, %v; want %q", ct, endpoint, found, err, want)
		}
	}
}

// NetBox 4.2 and 4.3 have no rest_api_endpoint, so the endpoint is matched
// against the discovered list endpoints by NetBox's URL naming.
func TestContentTypeEndpoint_FallbackHeuristic(t *testing.T) {
	s := &typeServer{
		coreStatus:   http.StatusNotFound,
		extrasServes: true,
		objectTypes:  []string{objectTypeRow(12, "dcim.device", "")},
		index: map[string][]string{
			"dcim":           {"devices", "interfaces", "virtual-chassis"},
			"ipam":           {"ip-addresses", "prefixes"},
			"extras":         {"journal-entries"},
			"virtualization": {"interfaces", "virtual-machines"},
			"plugins":        {"bgp"},
			"plugins/bgp":    {"session"},
		},
	}
	p := s.start(t)

	cases := []struct{ ct, want string }{
		{"dcim.device", "dcim/devices"},
		{"dcim.interface", "dcim/interfaces"},
		{"ipam.ipaddress", "ipam/ip-addresses"},
		{"ipam.prefix", "ipam/prefixes"},
		{"dcim.virtualchassis", "dcim/virtual-chassis"},
		{"extras.journalentry", "extras/journal-entries"},
		{"virtualization.virtualmachine", "virtualization/virtual-machines"},
		{"virtualization.vminterface", "virtualization/interfaces"},
	}
	for _, c := range cases {
		endpoint, found, err := p.contentTypeEndpoint(context.Background(), c.ct)
		if err != nil || !found || endpoint != c.want {
			t.Errorf("contentTypeEndpoint(%q) = %q, %v, %v; want %q, true, nil", c.ct, endpoint, found, err, c.want)
		}
	}

	// Plugin apps are discovered as plugins/<name>, which no app_label matches.
	endpoint, found, err := p.contentTypeEndpoint(context.Background(), "netbox_bgp.bgpsession")
	if err != nil || found || endpoint != "" {
		t.Errorf("contentTypeEndpoint(netbox_bgp.bgpsession) = %q, %v, %v; want not found, no error", endpoint, found, err)
	}
	if n := s.hits("/api/"); n != 1 {
		t.Errorf("the API index was walked %d times, want 1 (ObjectTypes caches it)", n)
	}
}

func TestContentTypeEndpoint_DiscoveryFailureIsAnError(t *testing.T) {
	cases := []struct {
		name string
		s    *typeServer
	}{
		{"no rest_api_endpoint and the API index refuses", &typeServer{
			coreStatus: http.StatusNotFound, extrasServes: true,
			objectTypes: []string{objectTypeRow(12, "dcim.device", "")},
			rootStatus:  http.StatusForbidden,
		}},
		{"neither the object types nor the API index can be read", &typeServer{
			coreStatus: http.StatusForbidden,
			rootStatus: http.StatusForbidden,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.s.start(t)
			endpoint, found, err := p.contentTypeEndpoint(context.Background(), "dcim.device")
			if err == nil || found || endpoint != "" {
				t.Fatalf("contentTypeEndpoint = %q, %v, %v; want an error", endpoint, found, err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
				t.Errorf("err = %v, want the upstream 403 wrapped", err)
			}
		})
	}

	t.Run("a heuristic match does not need the object types", func(t *testing.T) {
		s := &typeServer{coreStatus: http.StatusForbidden, index: map[string][]string{"dcim": {"devices"}}}
		p := s.start(t)
		endpoint, found, err := p.contentTypeEndpoint(context.Background(), "dcim.device")
		if err != nil || !found || endpoint != "dcim/devices" {
			t.Errorf("contentTypeEndpoint = %q, %v, %v; want dcim/devices, true, nil", endpoint, found, err)
		}
	})
}

func TestLookupField(t *testing.T) {
	for ct, want := range map[string]string{
		"circuits.circuit":   "cid",
		" Circuits.Circuit ": "cid",
		"dcim.device":        "name",
		"circuits.provider":  "name",
	} {
		if got := lookupField(ct); got != want {
			t.Errorf("lookupField(%q) = %q, want %q", ct, got, want)
		}
	}
}
