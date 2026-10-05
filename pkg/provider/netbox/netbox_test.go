package netbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"crypto/tls"
	"crypto/x509"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"strconv"
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
	// installed-plugins returns a BARE JSON ARRAY, not the DRF envelope (real NetBox behavior).
	mux.HandleFunc("/api/plugins/installed-plugins/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"name":"NetBox Labs Console","package":"netbox_labs_console","version":"2.0.1"},{"name":"NetBox Branching","package":"netbox_branching","version":"1.0.4"}]`)
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
		parents := r.URL.Query()["parent"]
		// 10.5.0.0/24 models a prefix with no individual IPs (only a utilized range).
		if len(parents) == 1 && parents[0] == "10.5.0.0/24" {
			_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
			return
		}
		// Default: 3 distinct host addresses PER named block. `count` feeds IP-range
		// utilization (a raw child count), `results` feed leaf-prefix IPSet
		// computation — which only ever names one parent.
		//
		// The scaling by len(parents) is the point of this handler, not decoration.
		// NetBox's `parent` filter is MULTI-VALUE and ORs its values, so one request
		// naming N pairwise-disjoint blocks reports the sum of their counts. A mock
		// that answered a flat 3 however many blocks were named would make batching
		// look like it changed the number when on a real NetBox it cannot.
		count := 3 * max(len(parents), 1)
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[
			{"address":"10.0.0.11/24"},{"address":"10.0.0.12/24"},{"address":"10.0.0.21/24"}
		]}`, count)
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

// TestFields_BranchCachePartition guards that the fields cache is keyed by
// branch, not objectType alone: a branch-scoped Fields call must issue its own
// NetBox request (carrying X-NetBox-Branch) rather than return the cached main
// fields, so branch-only columns (e.g. a custom field defined in the branch)
// are discoverable.
func TestFields_BranchCachePartition(t *testing.T) {
	var branches []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		b := r.Header.Get("X-NetBox-Branch")
		branches = append(branches, b)
		extra := ""
		if b != "" {
			extra = `,"cf_branch_only":"x"` // a custom field that exists only in the branch
		}
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"id":1,"name":"leaf1"`+extra+`}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	has := func(fields []provider.Field, name string) bool {
		for _, f := range fields {
			if f.Name == name {
				return true
			}
		}
		return false
	}

	mainFields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields(main): %v", err)
	}
	if has(mainFields, "cf_branch_only") {
		t.Fatal("main fields should not include the branch-only field")
	}

	branchFields, err := p.Fields(provider.WithBranch(context.Background(), "td5smq0f"), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields(branch): %v", err)
	}
	if !has(branchFields, "cf_branch_only") {
		t.Errorf("branch fields should include the branch-only field (cache not partitioned by branch)")
	}
	if len(branches) != 2 || branches[1] != "td5smq0f" {
		t.Errorf("expected a second branch-scoped request; saw header values %v", branches)
	}
}

func TestFilterFields_FromSchema(t *testing.T) {
	var schemaHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/schema/", func(w http.ResponseWriter, r *http.Request) {
		schemaHits++
		_, _ = fmt.Fprint(w, `{"paths":{"/api/ipam/prefixes/":{"get":{"parameters":[
			{"name":"prefix","in":"query"},
			{"name":"status","in":"query"},
			{"name":"status__ic","in":"query"},
			{"name":"limit","in":"query"}
		]}}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	ff, err := p.FilterFields(context.Background(), "ipam/prefixes")
	if err != nil {
		t.Fatalf("FilterFields: %v", err)
	}
	byName := map[string][]string{}
	for _, f := range ff {
		byName[f.Name] = f.Operators
	}
	if got := byName["prefix"]; len(got) != 1 || got[0] != "" {
		t.Errorf("prefix operators = %v, want [\"\"]", got)
	}
	if got := byName["status"]; len(got) != 2 || got[0] != "" || got[1] != "ic" {
		t.Errorf("status operators = %v, want [\"\" \"ic\"]", got)
	}
	if _, ok := byName["limit"]; ok {
		t.Error("limit must be excluded")
	}
	// second call is cached (no second schema fetch)
	if _, err := p.FilterFields(context.Background(), "ipam/prefixes"); err != nil {
		t.Fatal(err)
	}
	if schemaHits != 1 {
		t.Errorf("schema fetched %d times, want 1 (cached)", schemaHits)
	}
}

// TestFilterFields_BranchCachePartition guards that the OpenAPI filter-schema
// cache is keyed by branch, not global: a branch-scoped FilterFields call must
// issue its own schema fetch (carrying X-NetBox-Branch) rather than return the
// cached main schema, so branch-only custom-field filters (cf_*) are discovered
// and main-only filters don't leak into a branch.
func TestFilterFields_BranchCachePartition(t *testing.T) {
	var branches []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/schema/", func(w http.ResponseWriter, r *http.Request) {
		b := r.Header.Get("X-NetBox-Branch")
		branches = append(branches, b)
		extra := ""
		if b != "" {
			extra = `,{"name":"cf_branch_only","in":"query"}` // a custom-field filter that exists only in the branch
		}
		_, _ = fmt.Fprint(w, `{"paths":{"/api/ipam/prefixes/":{"get":{"parameters":[{"name":"prefix","in":"query"}`+extra+`]}}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	has := func(ff []provider.FilterField, name string) bool {
		for _, f := range ff {
			if f.Name == name {
				return true
			}
		}
		return false
	}

	mainFF, err := p.FilterFields(context.Background(), "ipam/prefixes")
	if err != nil {
		t.Fatalf("FilterFields(main): %v", err)
	}
	if has(mainFF, "cf_branch_only") {
		t.Fatal("main schema must not include the branch-only filter")
	}

	branchFF, err := p.FilterFields(provider.WithBranch(context.Background(), "td5smq0f"), "ipam/prefixes")
	if err != nil {
		t.Fatalf("FilterFields(branch): %v", err)
	}
	if !has(branchFF, "cf_branch_only") {
		t.Errorf("branch schema should include cf_branch_only (cache not partitioned by branch)")
	}
	if len(branches) != 2 || branches[1] != "td5smq0f" {
		t.Errorf("expected a second branch-scoped schema fetch; saw header values %v", branches)
	}
}

func TestQuery_InstalledPlugins_BareArray(t *testing.T) {
	p := newTestProvider(t)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "plugins/installed-plugins", Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(res.Rows))
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2 (len of bare array)", res.Total)
	}
	if !containsStr(res.Columns, "name") || !containsStr(res.Columns, "package") || !containsStr(res.Columns, "version") {
		t.Errorf("columns missing plugin fields: %v", res.Columns)
	}
	if res.Rows[0]["package"] != "netbox_labs_console" {
		t.Errorf("row0 package = %v, want netbox_labs_console", res.Rows[0]["package"])
	}
}

// The bare-array path clamps Rows to the caller's limit while Total still
// reports the full array length — the same split as the envelope path (Rows =
// page, Total = full match count).
func TestQuery_InstalledPlugins_BareArray_LimitClamp(t *testing.T) {
	p := newTestProvider(t)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "plugins/installed-plugins", Limit: 1})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("Rows = %d, want 1 (clamped to limit)", len(res.Rows))
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2 (full array length, not the clamped page)", res.Total)
	}
}

func TestFields_InstalledPlugins_BareArray(t *testing.T) {
	p := newTestProvider(t)
	fields, err := p.Fields(context.Background(), "plugins/installed-plugins")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if !containsStr(names, "name") || !containsStr(names, "package") || !containsStr(names, "version") {
		t.Errorf("Fields missing plugin columns: %v", names)
	}
}

// withFastRetries shrinks the retry backoff so a test can exhaust it without
// spending the production 2s. It keeps the SHAPE (two retries) so attempt counts
// stay meaningful.
func withFastRetries(t *testing.T) {
	t.Helper()
	saved := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryBackoff = saved })
}

// retryingServer answers with status for the first `fail` requests, then serves
// a one-row page. It counts every request so a test can assert how many times a
// page was actually asked for.
func retryingServer(t *testing.T, status, fail int) (*Provider, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= fail {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"upstream unavailable"}`))
			return
		}
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"id":1,"name":"a"}]}`)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second}), &calls
}

// TestFetchRows_RetriesTransientUpstream: a busy NetBox behind a gateway answers
// 502/503 under load, and a paged walk fails if ANY of its pages does — so the chance of
// losing a whole query grows with the result size, which is precisely the case
// this provider has to support. Measured before this retry existed: 3 of 4 real
// topology runs against a large remote instance aborted on a 5xx mid-walk, and the
// user got an error toast and an empty panel for a query that worked next try.
func TestFetchRows_RetriesTransientUpstream(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			withFastRetries(t)
			p, calls := retryingServer(t, status, 1)
			rows, total, err := p.fetchRows(context.Background(), "dcim/devices", nil, 10)
			if err != nil {
				t.Fatalf("a single %d must not fail the query: %v", status, err)
			}
			if len(rows) != 1 || total != 1 {
				t.Errorf("rows=%d total=%d, want the page the retry fetched", len(rows), total)
			}
			if *calls != 2 {
				t.Errorf("requests = %d, want 2 (the failure and one retry)", *calls)
			}
		})
	}
}

// TestFetchRows_GivesUpAfterBoundedRetries: "fail properly" means a fast, clear
// error, not an unbounded retry loop that hangs the panel. The user still gets
// the upstream status, so the message is actionable.
func TestFetchRows_GivesUpAfterBoundedRetries(t *testing.T) {
	withFastRetries(t)
	p, calls := retryingServer(t, http.StatusServiceUnavailable, 99)
	_, _, err := p.fetchRows(context.Background(), "dcim/devices", nil, 10)
	if err == nil {
		t.Fatal("a persistently failing upstream must surface an error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q must carry the upstream status", err)
	}
	if want := len(retryBackoff) + 1; *calls != want {
		t.Errorf("requests = %d, want %d (one attempt plus each bounded retry)", *calls, want)
	}
}

// TestFetchRows_DoesNotRetryClientErrors: a 4xx is the user's answer, not a
// blip. Repeating it only delays the real error — and on the ip-enrichment
// hops, which fan out into hundreds of batched requests, it would multiply that
// delay by the batch count.
func TestFetchRows_DoesNotRetryClientErrors(t *testing.T) {
	withFastRetries(t)
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			p, calls := retryingServer(t, status, 99)
			if _, _, err := p.fetchRows(context.Background(), "dcim/devices", nil, 10); err == nil {
				t.Fatalf("HTTP %d must surface as an error", status)
			}
			if *calls != 1 {
				t.Errorf("requests = %d for HTTP %d, want 1 (no retry)", *calls, status)
			}
		})
	}
}

// TestFetchRows_RetryStopsOnContextCancel: a closed dashboard or an expired
// query deadline must not be held open by a backoff sleep. The original error
// is returned rather than a context one, because the upstream failure is what
// the operator needs to see.
func TestFetchRows_RetryStopsOnContextCancel(t *testing.T) {
	saved := retryBackoff
	retryBackoff = []time.Duration{time.Hour}
	t.Cleanup(func() { retryBackoff = saved })

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"upstream unavailable"}`))
		cancel() // the caller goes away while the retry is waiting
	}))
	t.Cleanup(srv.Close)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	start := time.Now()
	_, _, err := p.fetchRows(ctx, "dcim/devices", nil, 10)
	if err == nil {
		t.Fatal("want the upstream error")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("cancelled query waited %s; the backoff must abort on ctx.Done", el)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1 (cancelled before the retry)", calls)
	}
}

// TestFetchRows_RetriesTransportFailure is the case a status-only rule misses.
// The run that motivated this retry died with "read: connection reset by peer"
// on page 18 of a 20-page walk — the connection dropped mid-response, so there
// is no HTTP status to classify, and the whole query was lost. Hijacking and
// closing the connection reproduces exactly that.
func TestFetchRows_RetriesTransportFailure(t *testing.T) {
	withFastRetries(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"id":1,"name":"a"}]}`)
	}))
	t.Cleanup(srv.Close)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	rows, _, err := p.fetchRows(context.Background(), "dcim/devices", nil, 10)
	if err != nil {
		t.Fatalf("a dropped connection must not fail the query: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %d, want the page the retry fetched", len(rows))
	}
	if calls != 2 {
		t.Errorf("requests = %d, want 2 (the dropped connection and one retry)", calls)
	}
}

// TestRetryable_ClassifiesEachShapeOfFailure pins the classification itself, so
// the deliberate exclusions cannot be widened by accident later: the transport
// rule is stated as "the connection failed", and every entry below is an
// argument about whether a repeat could possibly help.
func TestRetryable_ClassifiesEachShapeOfFailure(t *testing.T) {
	// The two wrappers client.getBytes actually applies, so the table tests the
	// errors as they really arrive rather than bare.
	readBody := func(err error) error { return fmt.Errorf("read body http://nb/api/x/: %w", err) }
	requestFailed := func(err error) error {
		return fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: "http://nb/api/x/", Err: err})
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		// Retryable: the connection failed, wherever it surfaced.
		{"reset mid-body", readBody(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), true},
		{"bare ECONNRESET mid-body", readBody(syscall.ECONNRESET), true},
		{"broken pipe", readBody(syscall.EPIPE), true},
		{"body short of its Content-Length", readBody(io.ErrUnexpectedEOF), true},
		{"connection refused before the headers", requestFailed(syscall.ECONNREFUSED), true},
		{"load shedding", &APIError{Status: http.StatusServiceUnavailable}, true},

		// Not retryable: the server answered, and this is the answer.
		{"bad request", &APIError{Status: http.StatusBadRequest}, false},
		{"forbidden", &APIError{Status: http.StatusForbidden}, false},
		{"rate limited (Retry-After is the protocol, not our backoff)", &APIError{Status: http.StatusTooManyRequests}, false},
		{"application error", &APIError{Status: http.StatusInternalServerError}, false},

		// Not retryable: nothing about the connection went wrong.
		{"empty document", readBody(io.EOF), false},
		{"body was not JSON", fmt.Errorf("decode http://nb/api/x/: %w", errors.New("invalid character 'x'")), false},

		// Not retryable: the caller went away. It arrives looking exactly like a
		// transport failure, which is why it is checked before anything else.
		{"cancelled", requestFailed(context.Canceled), false},
		{"deadline exceeded", requestFailed(context.DeadlineExceeded), false},
		// The datasource's own Timeout expiring mid-body. It reads as a net.Error
		// like every reset above, so only the deadline guard keeps the user's
		// stated budget from being spent three times over.
		{"datasource timeout while reading the body", readBody(fmt.Errorf(
			"context deadline exceeded (Client.Timeout or context cancellation while reading body): %w",
			context.DeadlineExceeded)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(context.Background(), tc.err); got != tc.want {
				t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestFetchRows_RetriesMidBodyFailureOnALaterPage is the failure this retry was
// BUILT for, and the one a url.Error-only rule lets through.
//
// getListPageRetry's own reason for existing is "a connection reset on page 18
// of 20". A reset that late in a page does not land on http.Client.Do — the
// headers were served long before — it lands in io.ReadAll on the response body,
// where client.getBytes wraps it as "read body <url>: <cause>". That cause is
// neither an *APIError (there is a 200 in the headers) nor a *url.Error (the
// url.Error wrapper only exists on the Do path), so the walk aborted on the very
// event the retry was added for.
//
// The server here serves page 1 whole, then kills page 2 mid-body: headers
// promising a Content-Length the body never reaches, so the client's read fails
// after Do has already returned. Two shapes of the same event, because which one
// the kernel delivers is not ours to choose — an orderly close truncates the
// body (io.ErrUnexpectedEOF), an RST surfaces as ECONNRESET inside a
// *net.OpError.
func TestFetchRows_RetriesMidBodyFailureOnALaterPage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
	}{
		{name: "truncated body", reset: false},
		{name: "connection reset", reset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastRetries(t)
			var base string
			calls := 0
			page2Calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if !r.URL.Query().Has("page2") {
					_, _ = fmt.Fprintf(w, `{"count":2,"next":"%s/api/dcim/devices/?page2=1","results":[{"id":1,"name":"a"}]}`, base)
					return
				}
				page2Calls++
				if page2Calls > 1 {
					_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[{"id":2,"name":"b"}]}`)
					return
				}
				conn, bufrw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				// A 200 and a Content-Length the body never reaches: the client
				// gets its headers, returns from Do, and dies inside io.ReadAll.
				_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n")
				_, _ = bufrw.WriteString(`{"count":2,"next":null,"results":[{"id":2,"na`)
				_ = bufrw.Flush()
				if tc.reset {
					// Close with an RST rather than a FIN, so the read fails with
					// ECONNRESET instead of a short body. The pause keeps the
					// reset strictly after the headers are consumed — before them
					// it would be an ordinary Do failure and would prove nothing.
					time.Sleep(100 * time.Millisecond)
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
				}
				_ = conn.Close()
			}))
			t.Cleanup(srv.Close)
			base = srv.URL
			p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

			rows, total, err := p.fetchRows(context.Background(), "dcim/devices", nil, 10)
			if err != nil {
				t.Fatalf("a page that died mid-body must not lose the walk: %v", err)
			}
			if len(rows) != 2 || total != 2 {
				t.Errorf("rows=%d total=%d, want both pages (the retry refetched page 2)", len(rows), total)
			}
			if page2Calls != 2 {
				t.Errorf("page 2 requests = %d, want 2 (the mid-body failure and one retry)", page2Calls)
			}
			if calls != 3 {
				t.Errorf("requests = %d, want 3 (page 1, page 2 failing, page 2 retried)", calls)
			}
		})
	}
}

// A certificate failure reaches retryable inside a *url.Error, which satisfies
// net.Error — so without an explicit exclusion the widened predicate calls an
// expired certificate "transient" and spends three attempts and two seconds of
// backoff discovering what the first attempt already knew.
func TestRetryable_DoesNotRetryACertificateFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			"untrusted authority",
			fmt.Errorf("request failed: %w", &url.Error{
				Op: "Get", URL: "https://netbox.example.com/api/dcim/devices/",
				Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			}),
		},
		{
			"wrong hostname",
			fmt.Errorf("request failed: %w", &url.Error{
				Op: "Get", URL: "https://netbox.example.com/api/dcim/devices/",
				Err: x509.HostnameError{Host: "netbox.example.com"},
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if retryable(context.Background(), tc.err) {
				t.Error("a certificate does not become valid 2s later; this must not be retried")
			}
		})
	}
}

// The local-filter path widens its fetch to a whole page so the pass has the
// most rows to work with. Without the widening it fetches only the caller's cap
// and the fallback loses rows it could have seen in the same single request.
func TestFieldValues_LocalFilterFetchesAWholePage(t *testing.T) {
	var fetched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/schema") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(dimSchemaFixture))
			return
		}
		// Record ONLY the dimension request. FieldValues probes the OBJECT TYPE
		// first, at pageSize, to decide whether one page is the whole story — so
		// keying on the first request of any kind would assert on that probe and
		// pass no matter what the dimension fetch does. (It did, until a mutation
		// test caught it.)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/devices") {
			// The object type must NOT fit in one page, or FieldValues answers
			// from the sample and never consults the dimension at all — which is
			// how the first version of this test passed against every mutant.
			_, _ = w.Write([]byte(`{"count":99999,"results":[]}`))
			return
		}
		fetched = append(fetched, r.URL.Query().Get("limit"))
		_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	// site_id has no upstream substring lookup, so this takes the local path.
	if _, err := p.FieldValues(context.Background(), "dcim/devices", "site_id", "12", 5); err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if len(fetched) == 0 {
		t.Fatal("no dimension request was made — the test never reached the path it means to cover")
	}
	t.Logf("requests: %v", fetched)
	if got := fetched[0]; got != strconv.Itoa(pageSize) {
		t.Errorf("local-filter path fetched limit=%s, want the full page size %d — a narrow fetch "+
			"throws away rows the same single request would have returned", got, pageSize)
	}
}
