package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The schema, field and custom-field caches are partitioned by branch. The
// branch arrives as the query's raw value, from a saved query, a dashboard
// variable or a request anyone who can query the datasource writes, so keying
// on it let one caller fill the caches without bound — and each new value
// cost NetBox a full OpenAPI schema download. They are keyed on the branch the
// value resolves to instead: its schema id, or main when NetBox has no
// branching (which ignores the header and answers from main anyway).

// branchCacheServer counts schema and device-sample requests and records every
// X-NetBox-Branch header it is sent. branches is the body of the branch list;
// "" makes the list 404, as on a NetBox without netbox-branching.
type branchCacheServer struct {
	*httptest.Server
	schemaHits, sampleHits atomic.Int64
	mu                     sync.Mutex
	headers                []string
}

func newBranchCacheServer(t *testing.T, branches string, schemaDelay time.Duration) *branchCacheServer {
	t.Helper()
	s := &branchCacheServer{}
	record := func(r *http.Request) {
		if v, ok := r.Header["X-Netbox-Branch"]; ok {
			s.mu.Lock()
			s.headers = append(s.headers, v[0])
			s.mu.Unlock()
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/plugins/branching/branches/", func(w http.ResponseWriter, r *http.Request) {
		if branches == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(branches))
	})
	mux.HandleFunc("/api/schema/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		s.schemaHits.Add(1)
		time.Sleep(schemaDelay)
		_, _ = w.Write([]byte(`{"paths":{"/api/ipam/prefixes/":{"get":{"parameters":[{"name":"prefix","in":"query"}]}}}}`))
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		s.sampleHits.Add(1)
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[{"id":1,"name":"leaf1"}]}`))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *branchCacheServer) sentHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.headers...)
}

func TestBranchCaches_WithoutBranchingEveryValueIsMain(t *testing.T) {
	srv := newBranchCacheServer(t, "", 0)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	for i := range 25 {
		ctx := context.Background()
		if i > 0 {
			ctx = provider.WithBranch(ctx, "made-up-"+strconv.Itoa(i))
		}
		if _, err := p.FilterFields(ctx, "ipam/prefixes"); err != nil {
			t.Fatalf("FilterFields: %v", err)
		}
		if _, err := p.Fields(ctx, "dcim/devices"); err != nil {
			t.Fatalf("Fields: %v", err)
		}
	}
	if n := srv.schemaHits.Load(); n != 1 {
		t.Errorf("OpenAPI schema fetched %d times for main and 24 made-up branches, want 1", n)
	}
	if n := srv.sampleHits.Load(); n != 1 {
		t.Errorf("device sample fetched %d times, want 1", n)
	}
	p.mu.Lock()
	schemas, fields := len(p.schemaByBranch), len(p.fields)
	p.mu.Unlock()
	if schemas != 1 || fields != 1 {
		t.Errorf("cache entries: schema %d, fields %d; want 1 each", schemas, fields)
	}
	if h := srv.sentHeaders(); len(h) != 0 {
		t.Errorf("X-NetBox-Branch sent to a NetBox without branching: %v", h)
	}
}

// A branch can be named or given by schema id; both are the same branch, one
// cache entry, one fetch, and the header always carries the schema id.
func TestBranchCaches_NameAndSchemaIDAreOneEntry(t *testing.T) {
	srv := newBranchCacheServer(t, `{"count":1,"next":null,"results":[{"name":"feature","schema_id":"td5smq0f"}]}`, 0)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	for _, v := range []string{"feature", "td5smq0f", "feature"} {
		ctx := provider.WithBranch(context.Background(), v)
		if _, err := p.FilterFields(ctx, "ipam/prefixes"); err != nil {
			t.Fatalf("FilterFields(%s): %v", v, err)
		}
		if _, err := p.Fields(ctx, "dcim/devices"); err != nil {
			t.Fatalf("Fields(%s): %v", v, err)
		}
	}
	if n := srv.schemaHits.Load(); n != 1 {
		t.Errorf("schema fetched %d times for one branch named two ways, want 1", n)
	}
	if n := srv.sampleHits.Load(); n != 1 {
		t.Errorf("device sample fetched %d times, want 1", n)
	}
	for _, h := range srv.sentHeaders() {
		if h != "td5smq0f" {
			t.Errorf("X-NetBox-Branch = %q, want the schema id td5smq0f", h)
		}
	}
}

// Entries were only ever overwritten by the same key, so a branch that was
// merged and deleted stayed cached for the life of the process. Writing a
// schema or fields entry now drops the ones that have expired. The custom-field
// index is the exception: an expired SUCCESSFUL index is the fallback a failed
// refetch publishes back (customFieldTypes), so it must outlive its expiry.
func TestBranchCaches_ExpiredEntriesAreDroppedOnWrite(t *testing.T) {
	srv := newBranchCacheServer(t, "", 0)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	past := time.Now().Add(-time.Minute)
	p.mu.Lock()
	p.schemaByBranch = map[string]schemaCacheEntry{"gone": {expiry: past}}
	p.fields["dcim/devices\x00gone"] = fieldsCacheEntry{expiry: past}
	if p.customFieldsByBranch == nil {
		p.customFieldsByBranch = map[string]customFieldTypesEntry{}
	}
	p.customFieldsByBranch["gone"] = customFieldTypesEntry{ok: true, expiry: past}
	p.mu.Unlock()

	if _, err := p.FilterFields(context.Background(), "ipam/prefixes"); err != nil {
		t.Fatalf("FilterFields: %v", err)
	}
	if _, err := p.Fields(context.Background(), "dcim/devices"); err != nil {
		t.Fatalf("Fields: %v", err)
	}
	p.customFieldTypes(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		_, settled := p.customFieldsByBranch[""]
		p.mu.Unlock()
		if settled || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.schemaByBranch["gone"]; ok {
		t.Error("expired schema entry survived a write")
	}
	if _, ok := p.fields["dcim/devices\x00gone"]; ok {
		t.Error("expired fields entry survived a write")
	}
	if _, ok := p.customFieldsByBranch["gone"]; !ok {
		t.Error("an expired successful custom-field index was dropped; it is a failed refetch's fallback")
	}
}

// Panels open together. On a cold cache each of them fetched the whole
// OpenAPI schema, the most expensive request this plugin makes; they now wait
// for one.
func TestSchema_ConcurrentColdCallersShareOneFetch(t *testing.T) {
	srv := newBranchCacheServer(t, "", 150*time.Millisecond)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			ff, err := p.FilterFields(context.Background(), "ipam/prefixes")
			if err == nil && len(ff) == 0 {
				err = fmt.Errorf("no filter fields")
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("FilterFields: %v", err)
		}
	}
	if n := srv.schemaHits.Load(); n != 1 {
		t.Errorf("schema fetched %d times by 8 concurrent cold callers, want 1", n)
	}
}
