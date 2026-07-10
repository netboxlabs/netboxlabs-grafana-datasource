package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// mockNetBox returns an httptest server emulating the relevant slice of the
// NetBox API, including a plugin app and a direct plugin collection.
func mockNetBox(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"dcim":"%s/api/dcim/","ipam":"%s/api/ipam/","plugins":"%s/api/plugins/","status":"%s/api/status/"}`, base, base, base, base)
	})
	mux.HandleFunc("/api/dcim/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"devices":"%s/api/dcim/devices/","interfaces":"%s/api/dcim/interfaces/"}`, base, base)
	})
	mux.HandleFunc("/api/ipam/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ip-addresses":"%s/api/ipam/ip-addresses/","prefixes":"%s/api/ipam/prefixes/","ip-ranges":"%s/api/ipam/ip-ranges/"}`, base, base, base)
	})
	mux.HandleFunc("/api/plugins/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"bgp":"%s/api/plugins/bgp/","installed-plugins":"%s/api/plugins/installed-plugins/"}`, base, base)
	})
	mux.HandleFunc("/api/plugins/bgp/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"bgp-sessions":"%s/api/plugins/bgp/bgp-sessions/"}`, base)
	})
	// installed-plugins is a paginated collection, NOT a URL index.
	mux.HandleFunc("/api/plugins/installed-plugins/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"name":"bgp"}]}`)
	})
	mux.HandleFunc("/api/status/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, `{"netbox-version":"4.5.8"}`)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		// Echo a filter back so tests can assert filter translation.
		role := r.URL.Query().Get("role")
		_ = role
		_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[
			{"id":1,"name":"leaf1","display_url":"`+base+`/dcim/devices/1/","site":{"id":2,"name":"dc1","slug":"dc1"},"status":{"value":"active","label":"Active"},"interface_count":48},
			{"id":2,"name":"leaf2","display_url":"`+base+`/dcim/devices/2/","site":{"id":2,"name":"dc1","slug":"dc1"},"status":{"value":"active","label":"Active"},"interface_count":48}
		]}`)
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("within") == "10.0.0.0/23" {
			_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[{"prefix":"10.0.0.0/24"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":1,"prefix":"10.0.0.0/24","status":{"value":"active","label":"Active"},
			 "is_pool":false,"mark_utilized":false,"family":{"value":4,"label":"IPv4"},"vrf":null}
		]}`))
	})
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 10.5.0.0/24 models a prefix with no individual IPs (only a utilized range).
		if r.URL.Query().Get("parent") == "10.5.0.0/24" {
			_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
			return
		}
		// Default: 3 distinct host addresses. `count` (=3) feeds IP-range utilization
		// (raw count per CIDR block); `results` feed leaf-prefix IPSet computation.
		_, _ = w.Write([]byte(`{"count":3,"next":null,"results":[
			{"address":"10.0.0.11/24"},{"address":"10.0.0.12/24"},{"address":"10.0.0.21/24"}
		]}`))
	})
	mux.HandleFunc("/api/ipam/ip-ranges/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 10.5.0.0/24 contains one marked-utilized range of 10 addresses.
		if r.URL.Query().Get("parent") == "10.5.0.0/24" {
			_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
				{"start_address":"10.5.0.10/24","end_address":"10.5.0.19/24"}
			]}`))
			return
		}
		// Default: no utilized child ranges (leaf-prefix util depends on this being empty).
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/core/object-changes/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[
			{"time":"2026-06-27T00:42:48Z","user_name":"admin","action":{"value":"update","label":"Updated"},"changed_object_type":"dcim.device","object_repr":"leaf1","display_url":"`+base+`/core/changelog/1/","changed_object":{"display_url":"`+base+`/dcim/devices/1/"}}
		]}`)
	})

	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func newTestProvider(t *testing.T) *Provider {
	srv := mockNetBox(t)
	return New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
}

func TestHealthCheck(t *testing.T) {
	p := newTestProvider(t)
	msg, err := p.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !strings.Contains(msg, "4.5.8") {
		t.Errorf("health message = %q, want version", msg)
	}
}

func TestObjectTypes_Discovery(t *testing.T) {
	p := newTestProvider(t)
	types, err := p.ObjectTypes(context.Background())
	if err != nil {
		t.Fatalf("ObjectTypes: %v", err)
	}
	got := map[string]bool{}
	for _, ot := range types {
		got[ot.Value] = true
	}
	for _, want := range []string{
		"dcim/devices",
		"dcim/interfaces",
		"ipam/ip-addresses",
		"plugins/bgp/bgp-sessions",  // plugin sub-app model
		"plugins/installed-plugins", // direct plugin collection
	} {
		if !got[want] {
			t.Errorf("missing discovered type %q; got %v", want, keys(got))
		}
	}
	if got["status/"] || got["status"] {
		t.Errorf("status should not be discovered as an object type")
	}
}

func TestQuery_FlattensAndJoins(t *testing.T) {
	p := newTestProvider(t)
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Filters:    []provider.Filter{{Field: "role", Operator: "", Value: "leaf"}},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(res.Rows))
	}
	if !containsStr(res.Columns, "name") || !containsStr(res.Columns, "site") || !containsStr(res.Columns, "display_url") {
		t.Errorf("columns missing join/link keys: %v", res.Columns)
	}
	if res.Rows[0]["name"] != "leaf1" {
		t.Errorf("row0 name = %v", res.Rows[0]["name"])
	}
}

func TestQuery_FieldProjection(t *testing.T) {
	p := newTestProvider(t)
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "name" || res.Columns[1] != "site" {
		t.Errorf("projection failed: %v", res.Columns)
	}
}

func TestFields(t *testing.T) {
	p := newTestProvider(t)
	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	types := map[string]provider.FieldType{}
	for _, f := range fields {
		types[f.Name] = f.Type
	}
	if types["name"] != provider.FieldTypeString {
		t.Errorf("name type = %v", types["name"])
	}
	if types["interface_count"] != provider.FieldTypeNumber {
		t.Errorf("interface_count type = %v", types["interface_count"])
	}
}

func TestChanges(t *testing.T) {
	p := newTestProvider(t)
	changes, err := p.Changes(context.Background(), provider.ChangeSpec{Limit: 10})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.ObjectRepr != "leaf1" || c.Action != "Updated" || c.ObjectType != "dcim.device" || c.User != "admin" {
		t.Errorf("unexpected change: %+v", c)
	}
	if !strings.Contains(c.URL, "/dcim/devices/1/") {
		t.Errorf("deep link = %q", c.URL)
	}
}

func TestChanges_TypeFilter(t *testing.T) {
	p := newTestProvider(t)
	changes, err := p.Changes(context.Background(), provider.ChangeSpec{ObjectTypes: []string{"dcim.site"}})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 changes after filtering to dcim.site, got %d", len(changes))
	}
}

func TestQuery_ReportsEnvelopeTotal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Envelope reports 500 matches but this page returns only 2 rows.
		_, _ = fmt.Fprint(w, `{"count":500,"next":null,"results":[{"id":1,"name":"a"},{"id":2,"name":"b"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "token", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 500 {
		t.Errorf("Total = %d, want 500 (envelope count)", res.Total)
	}
	if len(res.Rows) != 2 {
		t.Errorf("len(Rows) = %d, want 2", len(res.Rows))
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
