package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

func TestChunkByBudget(t *testing.T) {
	t.Run("keeps every value exactly once, in order", func(t *testing.T) {
		var vals []string
		for i := 0; i < 500; i++ {
			vals = append(vals, fmt.Sprintf("10.%d.%d.5", i/256, i%256))
		}
		var flat []string
		for _, c := range chunkByBudget("address", vals, chunkBudgetBytes) {
			flat = append(flat, c...)
		}
		if len(flat) != len(vals) {
			t.Fatalf("lost values: got %d want %d", len(flat), len(vals))
		}
		for i := range vals {
			if flat[i] != vals[i] {
				t.Fatalf("order changed at %d: %q != %q", i, flat[i], vals[i])
			}
		}
	})

	t.Run("no chunk exceeds the budget once encoded", func(t *testing.T) {
		var vals []string
		for i := 0; i < 400; i++ {
			vals = append(vals, fmt.Sprintf("2001:db8:85a3::8a2e:370:%04x", i))
		}
		for _, c := range chunkByBudget("address", vals, chunkBudgetBytes) {
			q := url.Values{}
			for _, v := range c {
				q.Add("address", v)
			}
			if n := len(q.Encode()); n > chunkBudgetBytes {
				t.Fatalf("chunk encodes to %d bytes, over budget %d", n, chunkBudgetBytes)
			}
		}
	})

	t.Run("leaves room for the paging parameter added downstream", func(t *testing.T) {
		// The budget is for the whole query, and fetchList appends ?limit= after
		// the chunker has handed the batch over — no caller can pass it in, so
		// this function is the only place it can be reserved. enrich.go's
		// topology batching depends on that too.
		//
		// The widths are swept rather than fixed, and that is the whole design of
		// this test. A chunk fills to the widest batch that fits, so it lands
		// within ONE VALUE of the ceiling — with 49-byte IPv6 values it stops ~49
		// bytes short, and deleting the reservation moves it to exactly the budget
		// rather than past it. The test then passes either way and pins nothing.
		// Short values close that gap: at 1-3 bytes a chunk ends within a few bytes
		// of the ceiling, so the missing 10 push it over and the assertion bites.
		for _, width := range []int{1, 2, 3, 5, 8, 13, 21, 49} {
			pad := strings.Repeat("x", width-1)
			var vals []string
			for i := 0; i < 4000; i++ {
				vals = append(vals, fmt.Sprintf("%s%d", pad, i%10))
			}
			for _, c := range chunkByBudget("address", vals, chunkBudgetBytes) {
				q := url.Values{}
				for _, v := range c {
					q.Add("address", v)
				}
				q.Set("limit", fmt.Sprintf("%d", min(MaxLimit, pageSize)))
				if n := len(q.Encode()); n > chunkBudgetBytes {
					t.Fatalf("value width %d: chunk plus the appended limit encodes to %d bytes, over budget %d",
						width, n, chunkBudgetBytes)
				}
			}
		}
	})

	t.Run("ipv6 chunks more aggressively than ipv4 at equal count", func(t *testing.T) {
		var v4, v6 []string
		for i := 0; i < 300; i++ {
			v4 = append(v4, fmt.Sprintf("10.%d.%d.5", i/256, i%256))
			v6 = append(v6, fmt.Sprintf("2001:db8:85a3::8a2e:370:%04x", i))
		}
		n4 := len(chunkByBudget("address", v4, chunkBudgetBytes))
		n6 := len(chunkByBudget("address", v6, chunkBudgetBytes))
		if n6 <= n4 {
			t.Fatalf("ipv6 must chunk more: v4=%d v6=%d", n4, n6)
		}
	})

	t.Run("oversized single value gets its own chunk, not dropped", func(t *testing.T) {
		oversized := strings.Repeat("x", chunkBudgetBytes+1000)
		vals := []string{"10.0.0.1", oversized, "10.0.0.2"}
		chunks := chunkByBudget("address", vals, chunkBudgetBytes)

		// Verify we got 3 chunks (normal, oversized, normal)
		if len(chunks) != 3 {
			t.Fatalf("expected 3 chunks, got %d", len(chunks))
		}

		// Flatten and verify all values are present in order
		var flat []string
		for _, c := range chunks {
			flat = append(flat, c...)
		}
		if len(flat) != 3 {
			t.Fatalf("lost values: got %d want 3", len(flat))
		}
		if flat[0] != "10.0.0.1" || flat[1] != oversized || flat[2] != "10.0.0.2" {
			t.Fatalf("order not preserved")
		}

		// Verify oversized value is in its own chunk
		if len(chunks[1]) != 1 {
			t.Fatalf("oversized value should be alone in chunk, got %d values", len(chunks[1]))
		}
	})
}

// addressChunks is how the address hop batches a set of IPs: its own batcher,
// not a second copy of the arithmetic. Every fixture that predicts which IP
// lands in which request has to ask the object the request is built from — a
// local recomputation drifts the moment anything changes what else the query
// carries, and a fixture that names "the failing chunk" would then be naming a
// batch the hop never sent.
func addressChunks(ips []string) [][]string {
	return newQueryBatcher("address", nil).chunk(ips)
}

// TestBatchedQueriesStayWithinTheByteBudget measures the thing that actually
// goes on the wire — the full encoded query, fixed parameters and all — rather
// than the repeated parameter the chunker splits on. That gap is the bug: the
// budget was computed from the ids alone and ?fields=, ?exclude= and the ?limit=
// fetchList appends were added afterwards, so every batch shipped over the
// ceiling the constant exists to stay under.
//
// A batch that overflows does not return a shorter answer; it returns HTTP 431
// from a proxy, which degrades the whole batch to blank is_primary_ip AND
// raises a warning — and a warning is a hard failure on the alert path
// (pkg/plugin.degradationError). So this is asserted per REQUEST, at the
// boundary, for all three hops rather than the one the review happened to name.
//
// The assertions are two-sided on purpose. The upper bound is the requirement;
// the lower bound proves the fixture still packs a batch up against the
// boundary, without which a chunker that emitted one id per request would pass
// this test while making 893 requests.
func TestBatchedQueriesStayWithinTheByteBudget(t *testing.T) {
	// The reported case: ids 1..893, which is what one chunk held when only the
	// ids were counted.
	ids := make([]int, 893)
	for i := range ids {
		ids[i] = i + 1
	}

	// assertWithinBudget takes the encoded query of every request an endpoint
	// actually received and measures each one against the budget.
	assertWithinBudget := func(t *testing.T, queries []string) {
		t.Helper()
		if len(queries) == 0 {
			t.Fatal("no requests recorded; the assertion below would pass vacuously")
		}
		longest, over := 0, false
		for i, q := range queries {
			n := len(q)
			if n > longest {
				longest = n
			}
			if n > chunkBudgetBytes {
				over = true
				t.Errorf("request %d/%d encodes to %d bytes, over the %d-byte budget: %s…",
					i+1, len(queries), n, chunkBudgetBytes, q[:64])
			}
		}
		// Within 64 bytes of the ceiling: the budget is only meaningful if a
		// batch is actually filled to it. Skipped when something already went
		// over — the failure above is the whole story, and this one would only
		// misdescribe it.
		if !over && longest < chunkBudgetBytes-64 {
			t.Errorf("longest request is %d bytes against a %d-byte budget — the fixture no longer fills a batch, so the upper bound above proves nothing",
				longest, chunkBudgetBytes)
		}
	}

	t.Run("the reported overflow, reconstructed", func(t *testing.T) {
		// What the old accounting measured: the ids alone.
		q := url.Values{}
		for _, id := range ids {
			q.Add("id", fmt.Sprintf("%d", id))
		}
		if n := len(q.Encode()); n != 6142 || n > chunkBudgetBytes {
			t.Fatalf("893 ids encode to %d bytes of ?id=; the reported figure is 6142, inside the %d-byte budget", n, chunkBudgetBytes)
		}
		// What was sent: the same ids plus the parameters appended after the
		// budget had been computed — the VM hop's projection and exclusion, and
		// the limit fetchList adds to every batched query.
		for k, vs := range primaryIPProjection() {
			q[k] = vs
		}
		q.Set("limit", fmt.Sprintf("%d", min(MaxLimit, pageSize)))
		if n := len(q.Encode()); n != 6213 || n <= chunkBudgetBytes {
			t.Fatalf("the complete query for those ids is %d bytes; the reported figure is 6213, past the %d-byte budget. If primaryIPFields changed, these numbers move — the property under test is that the complete query, not the id list, is what the chunker must fit", n, chunkBudgetBytes)
		}
	})

	t.Run("virtual machine hop", func(t *testing.T) {
		var queries []string
		srv := idEndpointServer(t, "/api/virtualization/virtual-machines/", &queries)
		p := New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})

		got, deg := p.fetchVMs(context.Background(), ids)
		if deg.any() {
			t.Fatalf("a fully successful hop must report no degradation, got %+v", deg)
		}
		if len(got) != len(ids) {
			t.Fatalf("got %d virtual machines, want %d — splitting must not lose ids", len(got), len(ids))
		}
		if len(queries) < 2 {
			t.Fatalf("893 ids no longer fit one projected batch; got %d request(s)", len(queries))
		}
		assertWithinBudget(t, queries)
	})

	// The topology edge hop is the fourth batching surface, and it was found by
	// asking which OTHER callers budget a repeated parameter and then append a
	// fixed one — not by a report. It carries `base` (e.g. connected=true) into
	// every request, so budgeting device_id alone measured short exactly as the
	// three ip-enrichment hops did.
	t.Run("topology edge hop carries its base parameters into the budget", func(t *testing.T) {
		var queries []string
		mux := http.NewServeMux()
		mux.HandleFunc("/api/dcim/interfaces/", func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			n := len(r.URL.Query()[deviceScopeParam])
			rows := make([]string, 0, n)
			for i := 0; i < n; i++ {
				rows = append(rows, `{"id":1}`)
			}
			w.Header().Set("Content-Type", "application/json")
			// count == len(rows): fetchEdgeRows splits a batch whose total exceeds
			// the rows it read, and that split would mask an over-budget request.
			_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, n, strings.Join(rows, ","))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		devIDs := make([]string, 900)
		for i := range devIDs {
			devIDs[i] = fmt.Sprintf("%d", i+1)
		}
		p := New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})
		if _, _, err := p.fetchEdgeRows(context.Background(), "dcim/interfaces",
			url.Values{"connected": []string{"true"}}, devIDs); err != nil {
			t.Fatalf("fetchEdgeRows: %v", err)
		}
		assertWithinBudget(t, queries)
	})

	// Page 2 is NetBox's URL, not ours. fetchList follows `next` verbatim, and DRF
	// builds it by adding &offset= to the query it received — so a batch that fits
	// on page 1 can still exceed the ceiling on the page after it, and nothing in
	// ipenrich.go constructs that URL to notice. Reachable by construction: these
	// hops filter by primary key, so a batch of more than pageSize ids spans more
	// than one page.
	t.Run("the continuation page NetBox builds stays within budget", func(t *testing.T) {
		var queries []string
		mux := http.NewServeMux()
		var base string
		mux.HandleFunc("/api/virtualization/virtual-machines/", func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			q := r.URL.Query()
			offset, _ := strconv.Atoi(q.Get("offset"))
			ids := q["id"]
			// One row per id, paged like DRF: at most pageSize per response, and a
			// `next` that echoes this query with the offset advanced.
			end := min(offset+pageSize, len(ids))
			rows := make([]string, 0, max(0, end-offset))
			for _, id := range ids[offset:end] {
				rows = append(rows, fmt.Sprintf(
					`{"id":%s,"primary_ip4":{"id":%s,"address":"10.0.0.1/32"},"primary_ip6":null}`, id, id))
			}
			next := "null"
			if end < len(ids) {
				nq := r.URL.Query()
				nq.Set("offset", strconv.Itoa(end))
				next = fmt.Sprintf("%q", base+"?"+nq.Encode())
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"count":%d,"next":%s,"results":[%s]}`, len(ids), next, strings.Join(rows, ","))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		base = srv.URL + "/api/virtualization/virtual-machines/"

		p := New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})
		got, deg := p.fetchVMs(context.Background(), ids)
		if deg.any() {
			t.Fatalf("a fully successful hop must report no degradation, got %+v", deg)
		}
		if len(got) != len(ids) {
			t.Fatalf("got %d virtual machines, want %d — paging must not lose ids", len(got), len(ids))
		}
		var paged bool
		for _, q := range queries {
			if strings.Contains(q, "offset=") {
				paged = true
			}
		}
		if !paged {
			t.Fatal("no continuation page was fetched, so this test proves nothing about offset")
		}
		assertWithinBudget(t, queries)
	})

	t.Run("device hop, projected for is_primary_ip alone", func(t *testing.T) {
		var queries []string
		srv := idEndpointServer(t, "/api/dcim/devices/", &queries)
		p := New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})

		// wantDeviceColumns false is the projected path, and it is the newer of
		// the two: it appeared when the hop started running for is_primary_ip
		// with no device_* column selected.
		got, deg := p.fetchDevices(context.Background(), ids, false)
		if deg.any() {
			t.Fatalf("a fully successful hop must report no degradation, got %+v", deg)
		}
		if len(got) != len(ids) {
			t.Fatalf("got %d devices, want %d", len(got), len(ids))
		}
		assertWithinBudget(t, queries)
	})

	t.Run("device hop, unprojected", func(t *testing.T) {
		var queries []string
		srv := idEndpointServer(t, "/api/dcim/devices/", &queries)
		p := New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})

		// No fixed parameters of its own, and still not free: fetchList appends
		// ?limit= to this batch exactly as it does to the projected one.
		if _, deg := p.fetchDevices(context.Background(), ids, true); deg.any() {
			t.Fatalf("a fully successful hop must report no degradation, got %+v", deg)
		}
		assertWithinBudget(t, queries)
	})

	t.Run("address hop", func(t *testing.T) {
		// Max-width IPv4 (15 bytes, no escaping), which is what fills an address
		// batch fastest without reaching for IPv6.
		var addrs []string
		for a := 100; a < 256 && len(addrs) < 300; a++ {
			for b := 100; b < 256 && len(addrs) < 300; b++ {
				addrs = append(addrs, fmt.Sprintf("%d.%d.255.255", a, b))
			}
		}

		var queries []string
		mux := http.NewServeMux()
		mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			var results []string
			for i, a := range r.URL.Query()["address"] {
				results = append(results, fmt.Sprintf(`{"id":%d,"address":"%s/24"}`, len(queries)*10000+i, a))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})
		res, err := p.fetchAddressRecords(context.Background(), addrs)
		if err != nil {
			t.Fatalf("fetchAddressRecords: %v", err)
		}
		if len(res.byHost) != len(addrs) {
			t.Fatalf("indexed %d hosts, want %d", len(res.byHost), len(addrs))
		}
		assertWithinBudget(t, queries)
	})
}

// idEndpointServer answers an ?id=-batched endpoint the way NetBox does — one
// object per requested id, carrying only the projected keys — and records the
// encoded query of every request it received.
func idEndpointServer(t *testing.T, path string, queries *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		*queries = append(*queries, r.URL.RawQuery)
		var results []string
		for _, id := range r.URL.Query()["id"] {
			results = append(results, fmt.Sprintf(
				`{"id":%s,"primary_ip4":{"id":%s,"address":"10.0.0.1/32"},"primary_ip6":null}`, id, id))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"10.0.0.5/24":     "10.0.0.5",
		"10.0.0.5":        "10.0.0.5",
		"2001:db8::1/64":  "2001:db8::1",
		"2001:db8::1":     "2001:db8::1",
		"  10.0.0.5/32  ": "10.0.0.5",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFetchAddressRecords is not in the task brief; it was added so
// fetchAddressRecords has a real caller ahead of the task that wires it into
// the enrichment path, and to lock in the behavior the brief's docstring
// promises: results are indexed by host portion, and an anycast address that
// matches multiple records keeps every one of them (no truncation to
// len(chunk)).
func TestFetchAddressRecords(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":3,"next":null,"results":[
			{"address":"10.20.0.1/24"},
			{"address":"10.99.99.99/32"},
			{"address":"10.99.99.99/32"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.fetchAddressRecords(context.Background(), []string{"10.20.0.1", "10.99.99.99"})
	if err != nil {
		t.Fatalf("fetchAddressRecords: %v", err)
	}
	if res.deg.any() {
		t.Errorf("a fully successful hop must report no degradation, got %+v", res.deg)
	}
	if len(res.failed) != 0 {
		t.Errorf("a fully successful hop must report no failed IPs, got %v", res.failed)
	}
	if len(res.byHost["10.20.0.1"]) != 1 {
		t.Errorf("host 10.20.0.1: got %d record(s), want 1", len(res.byHost["10.20.0.1"]))
	}
	if len(res.byHost["10.99.99.99"]) != 2 {
		t.Errorf("host 10.99.99.99 (anycast duplicate): got %d record(s), want 2", len(res.byHost["10.99.99.99"]))
	}
}

// TestFetchAddressRecords_ChunkFailureDegrades locks in the spec's error
// handling: "A failed chunk degrades only its own IPs ... other chunks still
// return." Before this, the first chunk error aborted the whole call and the
// panel got zero rows, so one transient 500 on a large flow panel cost the
// entire result.
func TestFetchAddressRecords_ChunkFailureDegrades(t *testing.T) {
	// Enough IPs to span several byte-budget chunks.
	ips := make([]string, 0, 1200)
	for i := 0; i < 1200; i++ {
		ips = append(ips, fmt.Sprintf("10.%d.%d.7", i/256, i%256))
	}
	chunks := addressChunks(ips)
	if len(chunks) < 3 {
		t.Fatalf("test needs >=3 chunks to distinguish partial from total failure, got %d", len(chunks))
	}

	t.Run("one failing chunk still returns the others' records", func(t *testing.T) {
		call := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
			call++
			if call == 2 { // a transient 500 on exactly one batch
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"boom"}`))
				return
			}
			var results []string
			for _, a := range r.URL.Query()["address"] {
				results = append(results, fmt.Sprintf(`{"id":%d,"address":"%s/24"}`, call*100000+len(results), a))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
		res, err := p.fetchAddressRecords(context.Background(), ips)
		if err != nil {
			t.Fatalf("a partial failure must not fail the call: %v", err)
		}
		// The gap has to be REPORTED, not just survived: the failed chunk's IPs
		// come back with no address record, which is exactly what an unregistered
		// IP looks like. Without the degradation the caller cannot tell the two
		// apart, and the panel shows blank columns as if NetBox had answered.
		if !res.deg.any() {
			t.Fatal("a partial failure must be reported as a degradation, not swallowed")
		}
		if res.deg.failed != len(chunks[1]) {
			t.Errorf("degradation covers %d IPs, want %d (the failed chunk's)", res.deg.failed, len(chunks[1]))
		}
		if res.deg.total != len(ips) {
			t.Errorf("degradation total = %d, want %d (every IP asked about)", res.deg.total, len(ips))
		}
		if res.deg.cause == nil {
			t.Error("degradation must carry the cause; the warning text names it")
		}
		// The surviving chunks are everything except the second one.
		wantHosts := 0
		for i, c := range chunks {
			if i != 1 {
				wantHosts += len(c)
			}
		}
		if len(res.byHost) != wantHosts {
			t.Fatalf("got %d hosts, want %d (every chunk but the failed one)", len(res.byHost), wantHosts)
		}
		// Spot-check both sides of the gap: the first chunk's first IP resolved,
		// the failed chunk's first IP did not.
		if len(res.byHost[canonicalIP(chunks[0][0])]) == 0 {
			t.Errorf("chunk 1's records are missing; a later chunk's failure must not discard earlier results")
		}
		if len(res.byHost[canonicalIP(chunks[1][0])]) != 0 {
			t.Errorf("the failed chunk's IPs must come back with no records, got %v", res.byHost[canonicalIP(chunks[1][0])])
		}
		// The named set is what lets ResolveIPs tell "no record" from "no
		// answer". Without it the two are the same empty bucket.
		if len(res.failed) != len(chunks[1]) {
			t.Errorf("failed set covers %d IPs, want %d (the failed chunk's)", len(res.failed), len(chunks[1]))
		}
		if !res.failed[chunks[1][0]] {
			t.Errorf("the failed chunk's IPs must be named in the failed set")
		}
		if res.failed[chunks[0][0]] {
			t.Errorf("a surviving chunk's IPs must not appear in the failed set")
		}
	})

	t.Run("every chunk failing is still an error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
		if _, err := p.fetchAddressRecords(context.Background(), ips); err == nil {
			t.Fatal("want an error when there is no partial answer to give")
		}
	})
}

// TestFetchAddressRecords_OverflowingBatchIsExhausted covers the row cap.
// fetchRows stops at MaxLimit and reports what NetBox says actually exists; that
// second value used to be discarded (`raws, _, err := ...`), so a batch matching
// more records than one request can carry lost the remainder with no signal.
//
// The mocks below make a response "overflow" by reporting a count higher than
// the results they return, which is precisely the state fetchRows leaves behind
// when the cap truncates a read — and is the condition the code tests. Faking it
// this way costs one small response instead of 10,000 records over 20 pages, and
// exercises the same branch.
func TestFetchAddressRecords_OverflowingBatchIsExhausted(t *testing.T) {
	// oneRecordEach renders a NetBox list envelope: one record per address,
	// with count set independently so a response can claim to be truncated.
	oneRecordEach := func(w http.ResponseWriter, addrs []string, count int) {
		var results []string
		for i, a := range addrs {
			results = append(results, fmt.Sprintf(
				`{"id":%d,"address":"%s/24","status":{"value":"active"},"assigned_object_type":null,"assigned_object":null}`,
				1000+i, a))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, count, strings.Join(results, ","))
	}

	t.Run("a batch over the cap is halved until each half fits", func(t *testing.T) {
		// Over four addresses, the server behaves as the cap does: it answers
		// with a fraction of the matches and reports the true, larger total.
		var sizes []int
		mux := http.NewServeMux()
		mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
			addrs := r.URL.Query()["address"]
			sizes = append(sizes, len(addrs))
			if len(addrs) > 4 {
				oneRecordEach(w, addrs[:2], 99999) // truncated, and says so
				return
			}
			oneRecordEach(w, addrs, len(addrs))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		var ips []string
		for i := 0; i < 12; i++ {
			ips = append(ips, fmt.Sprintf("10.0.0.%d", i+1))
		}
		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
		res, err := p.fetchAddressRecords(context.Background(), ips)
		if err != nil {
			t.Fatalf("fetchAddressRecords: %v", err)
		}

		// Every address ends up with its record. This is the whole point: an
		// address whose records fell past the cap used to come back looking
		// exactly like an address NetBox has never heard of.
		for _, ip := range ips {
			if len(res.byHost[canonicalIP(ip)]) != 1 {
				t.Errorf("host %s: %d record(s), want 1", ip, len(res.byHost[canonicalIP(ip)]))
			}
		}
		if len(res.truncated) != 0 {
			t.Errorf("truncated = %v, want none: splitting resolved the overflow", res.truncated)
		}
		if res.deg.any() {
			t.Errorf("deg.failed = %d, want 0: an overflow is not a failure", res.deg.failed)
		}
		// 12 -> 6,6 -> 3,3,3,3. The counts are asserted so a future change that
		// "fixes" this by fetching everything twice, or by giving up after one
		// split, is visible rather than merely still-passing.
		want := []int{12, 6, 3, 3, 6, 3, 3}
		if fmt.Sprint(sizes) != fmt.Sprint(want) {
			t.Errorf("batch sizes = %v, want %v", sizes, want)
		}
	})

	t.Run("a single address over the cap is stated, and its records still kept", func(t *testing.T) {
		// The floor. One address matching more records than a request can carry
		// cannot be split any further, so the only honest move left is to say so.
		mux := http.NewServeMux()
		mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
			oneRecordEach(w, r.URL.Query()["address"], 99999) // always "truncated"
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
		res, err := p.fetchAddressRecords(context.Background(), []string{"10.0.0.1", "10.0.0.2"})
		if err != nil {
			t.Fatalf("an overflow is not a failure: %v", err)
		}
		if got := sorted(res.truncated); fmt.Sprint(got) != "[10.0.0.1 10.0.0.2]" {
			t.Errorf("truncated = %v, want both hosts named", got)
		}
		// Partial data still beats none, so what WAS read is indexed. The
		// warning is what stops it being read as complete.
		for _, ip := range []string{"10.0.0.1", "10.0.0.2"} {
			if len(res.byHost[ip]) == 0 {
				t.Errorf("host %s lost its records; a truncated read still returns what it read", ip)
			}
		}
		if res.deg.any() {
			t.Errorf("deg.failed = %d, want 0: nothing failed, it was merely incomplete", res.deg.failed)
		}
	})
}

// TestResolveIPs_CappedAddressBatchDoesNotFakeAnAbsentRecord is the user-visible
// half of the cap fix, and names the harm exactly: a registered IP whose records
// fell past the cap resolved to match_count 0, which sends the row down the
// prefix fallback and reports it to the user as an address NetBox does not hold.
// docs/RECIPES.md teaches that shape as "unknown/external traffic" — the precise
// opposite of the truth for an address NetBox has a record for.
func TestResolveIPs_CappedAddressBatchDoesNotFakeAnAbsentRecord(t *testing.T) {
	// Atomic because the prefix fallback runs its requests concurrently
	// (runPrefixFallback): a plain counter here would be a data race on the day
	// this assertion starts failing, which is the day it has to be readable.
	var prefixCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		addrs := r.URL.Query()["address"]
		// A multi-address batch answers for the FIRST address only and reports
		// the true total, exactly as the cap truncating a read would. A
		// single-address batch fits and answers in full.
		shown := addrs
		if len(addrs) > 1 {
			shown = addrs[:1]
		}
		var results []string
		for i, a := range shown {
			results = append(results, fmt.Sprintf(
				`{"id":%d,"address":"%s/24","dns_name":"%s.example.net","status":{"value":"active"},"assigned_object_type":null,"assigned_object":null}`,
				500+i, a, strings.ReplaceAll(a, ".", "-")))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(addrs), strings.Join(results, ","))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		prefixCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":7,"prefix":"10.0.0.0/24","description":"should never reach a registered IP"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.2"},
		[]string{"ip", "match_count", "address_dns_name", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}

	byIP := map[string]map[string]interface{}{}
	for _, row := range res.Rows {
		byIP[fmt.Sprint(row["ip"])] = row
	}
	// 10.0.0.2 is the one the truncated first response left out.
	row := byIP["10.0.0.2"]
	if row["match_count"] != float64(1) {
		t.Errorf("match_count = %v, want 1 — NetBox holds a record for this IP", row["match_count"])
	}
	if row["address_dns_name"] != "10-0-0-2.example.net" {
		t.Errorf("address_dns_name = %v, want the record's own dns_name", row["address_dns_name"])
	}
	if row["prefix_cidr"] != nil {
		t.Errorf("prefix_cidr = %v, want nil — a matched address must never fall through to the prefix fallback", row["prefix_cidr"])
	}
	if n := prefixCalls.Load(); n != 0 {
		t.Errorf("the prefix fallback ran %d time(s); both IPs have address records", n)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none — the overflow was resolved by splitting, not merely reported", res.Warnings)
	}
}

// TestAddressTruncationWarning locks the sentence the floor case emits. It has
// to name the addresses: unlike a failed batch, which covers an arbitrary slice
// of the input, this one is actionable only if the reader knows which address in
// NetBox to go and look at.
func TestAddressTruncationWarning(t *testing.T) {
	one := addressTruncationWarning([]string{"10.0.0.1"})
	for _, want := range []string{"10,000", "1 IP", "10.0.0.1", "match_count is a floor"} {
		if !strings.Contains(one, want) {
			t.Errorf("warning %q does not mention %q", one, want)
		}
	}
	many := addressTruncationWarning([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"})
	if !strings.Contains(many, "5 IPs") {
		t.Errorf("warning %q does not count the addresses", many)
	}
	// Capped: a panel notice is not a report.
	if !strings.Contains(many, "and 2 more") {
		t.Errorf("warning %q does not cap the named list", many)
	}
	if strings.Contains(many, "10.0.0.4") {
		t.Errorf("warning %q names more addresses than the cap allows", many)
	}
}

// twinnedSpellings builds an input where every host appears TWICE — once bare,
// once masked — with the two spellings far enough apart that the byte-budget
// chunker cannot put a pair in the same chunk. orphans are prepended and get no
// twin, so they are the only inputs a first-chunk failure can genuinely lose.
//
// This is the shape that separates "this input was in a failed request" from
// "this input has no answer": canonicalIP collapses both spellings onto one
// bucket, so whichever chunk succeeds answers for both.
func twinnedSpellings(hosts int, orphans ...string) []string {
	ips := append([]string{}, orphans...)
	for i := 0; i < hosts; i++ {
		ips = append(ips, fmt.Sprintf("10.%d.%d.1", i/256, i%256))
	}
	for i := 0; i < hosts; i++ {
		ips = append(ips, fmt.Sprintf("10.%d.%d.1/32", i/256, i%256))
	}
	return ips
}

// failFirstBatchServer answers ipam/ip-addresses like addressEchoServer but
// fails the FIRST request outright, and reports how many requests it saw.
func failFirstBatchServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	ids := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
			return
		}
		var results []string
		for _, a := range r.URL.Query()["address"] {
			h := hostOf(a)
			id, ok := ids[h]
			if !ok {
				id = len(ids) + 1
				ids[h] = id
			}
			results = append(results, fmt.Sprintf(
				`{"id":%d,"address":"%s/24","dns_name":"h%d.example.net","status":{"value":"active"},"assigned_object_type":null,"assigned_object":null}`,
				id, h, id))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestFetchAddressRecords_HostAnsweredByAnotherChunkIsNotDegraded closes a false
// degradation. The hop used to tally `deg.record(len(chunk), err)` the moment a
// chunk failed and never revisit it, but chunking is by the CALLER's spelling
// while indexing is by canonical host: "10.20.0.1" and "10.20.0.1/32" are one
// address in two chunks, and if one fails while the other succeeds the bucket is
// populated and the row is complete.
//
// The row-level path already recovered — ResolveIPs tests
// `addrFailed[ip] && len(cands) == 0` — so the visible columns were right while
// the batch-level tally said the result was degraded. That number is not
// cosmetic: pkg/plugin.degradationError turns ANY provider warning into an
// alerting error, so a complete result was rejected outright. Failing an alert on
// data that is not missing is the inverse of the bug the degradation reporting
// was built to fix.
func TestFetchAddressRecords_HostAnsweredByAnotherChunkIsNotDegraded(t *testing.T) {
	// assertTwinsSplit fails the test unless the fixture really does put every
	// member of the failing chunk's twins somewhere else. Without this the test
	// could pass because nothing was ever recovered.
	assertTwinsSplit := func(t *testing.T, chunks [][]string, exempt map[string]bool) {
		t.Helper()
		if len(chunks) < 3 {
			t.Fatalf("fixture needs >=3 chunks to tell partial from total failure, got %d", len(chunks))
		}
		elsewhere := map[string]bool{}
		for _, c := range chunks[1:] {
			for _, ip := range c {
				elsewhere[canonicalIP(ip)] = true
			}
		}
		for _, ip := range chunks[0] {
			if exempt[ip] || elsewhere[canonicalIP(ip)] {
				continue
			}
			t.Fatalf("fixture drifted: %q is in the failing chunk with no twin in a surviving one", ip)
		}
	}

	t.Run("a host every one of whose spellings recovered is not counted as degraded", func(t *testing.T) {
		ips := twinnedSpellings(800)
		assertTwinsSplit(t, addressChunks(ips), nil)

		srv, calls := failFirstBatchServer(t)
		p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
		res, err := p.fetchAddressRecords(context.Background(), ips)
		if err != nil {
			t.Fatalf("a partial failure must not fail the call: %v", err)
		}
		if *calls < 2 {
			t.Fatalf("the failure never happened: %d request(s)", *calls)
		}
		if len(res.failed) == 0 {
			t.Fatal("the failed set must still name every spelling that was in the failed request")
		}
		// The property. Every input in the failed chunk has a twin that
		// answered, so nothing was actually lost.
		if res.deg.any() {
			t.Errorf("deg.failed = %d, want 0: every IP the failed chunk covered was answered by another chunk", res.deg.failed)
		}
		// ...and the answers really are there, so "nothing degraded" is not
		// "nothing was fetched".
		for _, ip := range ips[:50] {
			if len(res.byHost[canonicalIP(ip)]) == 0 {
				t.Fatalf("no record for %q; the surviving chunk should have answered for it", ip)
			}
		}
	})

	t.Run("an input with no surviving twin is still counted, and it alone", func(t *testing.T) {
		// The other direction, and the reason the recount cannot simply be
		// "stop counting". orphan has one spelling only, so the failed chunk
		// really did lose it.
		const orphan = "10.255.255.254"
		ips := twinnedSpellings(800, orphan)
		assertTwinsSplit(t, addressChunks(ips), map[string]bool{orphan: true})

		srv, _ := failFirstBatchServer(t)
		p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
		res, err := p.fetchAddressRecords(context.Background(), ips)
		if err != nil {
			t.Fatalf("a partial failure must not fail the call: %v", err)
		}
		if !res.failed[orphan] {
			t.Fatal("the orphan must be named in the failed set")
		}
		if len(res.byHost[canonicalIP(orphan)]) != 0 {
			t.Fatal("the orphan must have no record; nothing else asked for it")
		}
		if res.deg.failed != 1 {
			t.Errorf("deg.failed = %d, want exactly 1 (the orphan) — the recount must not swallow a real loss", res.deg.failed)
		}
		if res.deg.total != len(ips) {
			t.Errorf("deg.total = %d, want %d (every IP asked about)", res.deg.total, len(ips))
		}
		if res.deg.cause == nil {
			t.Error("degradation must carry the cause; the warning text names it")
		}
	})

	t.Run("a fully recovered result carries no warning, so alerting does not reject it", func(t *testing.T) {
		// End-to-end, because the warning — not the tally — is what
		// pkg/plugin.degradationError converts into an alert-query error.
		ips := twinnedSpellings(800)
		srv, _ := failFirstBatchServer(t)
		p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})

		// An explicit limit above len(ips): the default would clamp to 1,000 and
		// cut the masked twins off, leaving nothing to recover with.
		res, err := p.ResolveIPs(context.Background(), ips,
			[]string{"ip", "match_count", "address_dns_name"}, len(ips)+10)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none — every IP the failed chunk covered was answered elsewhere", res.Warnings)
		}
		// The rows have to be complete too, or "no warning" would just be a
		// second bug agreeing with the first.
		for _, row := range res.Rows {
			if row["match_count"] != float64(1) {
				t.Fatalf("match_count for %v = %v, want 1", row["ip"], row["match_count"])
			}
			if row["address_dns_name"] == nil {
				t.Fatalf("address_dns_name for %v is nil; the row is not actually complete", row["ip"])
			}
		}
	})
}

// TestFetchDevices_ChunkFailureDegrades is the device-hop twin of the above:
// a failed batch costs only its own devices' device_* columns.
func TestFetchDevices_ChunkFailureDegrades(t *testing.T) {
	ids := make([]int, 2000)
	for i := range ids {
		ids[i] = i + 1
	}

	call := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
			return
		}
		var results []string
		for _, id := range r.URL.Query()["id"] {
			results = append(results, fmt.Sprintf(`{"id":%s,"name":"dev-%s"}`, id, id))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	got, deg := p.fetchDevices(context.Background(), ids, true)
	if len(got) == 0 {
		t.Fatal("the surviving batches' devices must still come back")
	}
	if len(got) == len(ids) {
		t.Fatal("test is not exercising the failure: every device resolved")
	}
	if call < 2 {
		t.Fatalf("expected the call to continue past the failed batch, got %d requests", call)
	}
	// Degrading is only half the contract; the other half is saying so. The rows
	// whose device is missing look identical to rows for IPs that genuinely have
	// no device, so the degradation is the only thing that can tell them apart.
	if !deg.any() {
		t.Fatal("a partial failure must be reported as a degradation, not swallowed")
	}
	if deg.failed != len(ids)-len(got) {
		t.Errorf("degradation covers %d devices, want %d (those the failed batch held)", deg.failed, len(ids)-len(got))
	}
	if deg.total != len(ids) {
		t.Errorf("degradation total = %d, want %d", deg.total, len(ids))
	}
	if deg.cause == nil {
		t.Error("degradation must carry the cause; the warning text names it")
	}
}

func rawIP(id int, status string, assigned bool) json.RawMessage {
	if !assigned {
		return rawIPAssigned(id, status, "", "null")
	}
	return rawIPAssigned(id, status, assignedTypeInterface,
		`{"id":9,"name":"Ethernet1","device":{"id":3,"name":"leaf-01"}}`)
}

// rawIPAssigned builds an address record with an explicit assignment TYPE.
// assigned_object is a generic relation, so the type is what decides whether the
// record can name an interface or a device — a fixture that omits it can only
// exercise the middle of pickAddress' three tiers.
func rawIPAssigned(id int, status, objectType, assignedObject string) json.RawMessage {
	typ := "null"
	if objectType != "" {
		typ = fmt.Sprintf("%q", objectType)
	}
	return json.RawMessage(fmt.Sprintf(
		`{"id":%d,"address":"10.0.0.1/24","status":{"value":%q},"assigned_object_type":%s,"assigned_object":%s}`,
		id, status, typ, assignedObject))
}

// rawFHRP is an address assigned to an FHRP group: assigned_object is populated
// and non-null, so it is indistinguishable from an interface assignment to
// anything that only tests for nullness — which is exactly the bug.
func rawFHRP(id int, status string) json.RawMessage {
	return rawIPAssigned(id, status, "ipam.fhrpgroup",
		`{"id":5,"display":"zz-probe-fhrp VRRPv3: 991 (10.0.0.1/24)","protocol":"vrrp3","group_id":991}`)
}

func idOf(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var o struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return o.ID
}

func TestPickAddress(t *testing.T) {
	t.Run("prefers assigned over unassigned", func(t *testing.T) {
		got := pickAddress([]json.RawMessage{rawIP(1, "active", false), rawIP(2, "active", true)})
		if idOf(t, got) != 2 {
			t.Fatalf("got id %d, want 2", idOf(t, got))
		}
	})
	t.Run("prefers non-deprecated when both assigned", func(t *testing.T) {
		got := pickAddress([]json.RawMessage{rawIP(1, "deprecated", true), rawIP(2, "active", true)})
		if idOf(t, got) != 2 {
			t.Fatalf("got id %d, want 2", idOf(t, got))
		}
	})
	t.Run("falls back to lowest id for stability", func(t *testing.T) {
		got := pickAddress([]json.RawMessage{rawIP(7, "active", true), rawIP(3, "active", true)})
		if idOf(t, got) != 3 {
			t.Fatalf("got id %d, want 3", idOf(t, got))
		}
	})
	t.Run("returns nil for no candidates", func(t *testing.T) {
		if pickAddress(nil) != nil {
			t.Fatal("expected nil")
		}
	})

	// The emergent bug from type-gating interface_*/device_*: an FHRP-assigned
	// record and a dcim.interface-assigned one for the same host tied on
	// "assigned_object is not null", and the lowest-id fallback then picked
	// whichever NetBox created first. When that was the FHRP record — which the
	// type gates in applyAddressColumns and deviceIDFromAddress correctly refuse —
	// the row lost its identity columns entirely, with the device-backed candidate
	// sitting unused in the same bucket. The lower id here is the whole point.
	t.Run("an interface assignment beats an FHRP one with a lower id", func(t *testing.T) {
		got := pickAddress([]json.RawMessage{
			rawFHRP(1, "active"),
			rawIPAssigned(2, "active", assignedTypeInterface,
				`{"id":9,"name":"Ethernet1","device":{"id":3,"name":"leaf-01"}}`),
		})
		if idOf(t, got) != 2 {
			t.Fatalf("got id %d, want 2 — the interface-assigned record is the only one that can fill interface_*/device_*", idOf(t, got))
		}
	})

	// A VM interface fills interface_* (never device_*), so it belongs in the same
	// top tier as a device interface — the pick must not hand the row to an FHRP
	// record that can fill neither.
	t.Run("a VM interface assignment also beats an FHRP one with a lower id", func(t *testing.T) {
		got := pickAddress([]json.RawMessage{
			rawFHRP(1, "active"),
			rawIPAssigned(2, "active", assignedTypeVMInterface, `{"id":9,"name":"eth0"}`),
		})
		if idOf(t, got) != 2 {
			t.Fatalf("got id %d, want 2", idOf(t, got))
		}
	})

	// Assignment outranks status, as it always has. A deprecated interface record
	// still names the device and the interface, and address_status says
	// "deprecated" in plain sight; an active FHRP record names neither.
	t.Run("a deprecated interface assignment still beats an active FHRP one", func(t *testing.T) {
		got := pickAddress([]json.RawMessage{
			rawFHRP(1, "active"),
			rawIPAssigned(2, "deprecated", assignedTypeInterface,
				`{"id":9,"name":"Ethernet1","device":{"id":3,"name":"leaf-01"}}`),
		})
		if idOf(t, got) != 2 {
			t.Fatalf("got id %d, want 2", idOf(t, got))
		}
	})

	// The documented middle tier: a non-interface assignment carries no identity
	// either, but it does document an address something is using, so it still
	// edges out a bare unassigned record.
	t.Run("an FHRP assignment still beats no assignment at all", func(t *testing.T) {
		got := pickAddress([]json.RawMessage{
			rawIP(1, "active", false),
			rawFHRP(2, "active"),
		})
		if idOf(t, got) != 2 {
			t.Fatalf("got id %d, want 2", idOf(t, got))
		}
	})
}

// TestResolveIPs_FHRPRecordDoesNotStealTheRow is the end-to-end half of the same
// finding, and the one that shows the symptom a user actually sees. A host with
// both an FHRP-assigned record (lower id) and an interface-assigned one came back
// with match_count 2 and every identity column blank — the row said "NetBox knows
// this address but can tell you nothing about it" while the answer was in the
// same response.
func TestResolveIPs_FHRPRecordDoesNotStealTheRow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":2,"next":null,"results":[%s,%s]}`,
			`{"id":11,"address":"10.77.77.77/24","status":{"value":"active"},`+
				`"assigned_object_type":"ipam.fhrpgroup",`+
				`"assigned_object":{"id":5,"display":"zz-probe-fhrp VRRPv3: 991 (10.77.77.77/24)"}}`,
			`{"id":12,"address":"10.77.77.77/24","status":{"value":"active"},`+
				`"assigned_object_type":"dcim.interface",`+
				`"assigned_object":{"id":9,"display":"Ethernet9","device":{"id":3,"name":"leaf-01"}}}`)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"id":3,"name":"leaf-01","site":{"name":"AMS1"}}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.77.77.77"},
		[]string{"ip", "match_count", "interface_name", "device_name", "device_site"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	// Both records are still counted: the pick is a pick, and match_count is what
	// makes it visible.
	if got := row["match_count"]; got != float64(2) {
		t.Errorf("match_count = %v, want 2", got)
	}
	if got := row["interface_name"]; got != "Ethernet9" {
		t.Errorf("interface_name = %v, want Ethernet9 — the FHRP record won the tie and the row went blank", got)
	}
	if got := row["device_name"]; got != "leaf-01" {
		t.Errorf("device_name = %v, want leaf-01", got)
	}
	if got := row["device_site"]; got != "AMS1" {
		t.Errorf("device_site = %v, want AMS1", got)
	}
}

func TestIsPrimaryIP(t *testing.T) {
	t.Run("true when primary_ip4 id matches", func(t *testing.T) {
		if !isPrimaryIP(map[string]interface{}{"primary_ip4_id": float64(42)}, 42) {
			t.Fatal("expected true")
		}
	})
	t.Run("true when primary_ip6 id matches", func(t *testing.T) {
		if !isPrimaryIP(map[string]interface{}{"primary_ip6_id": float64(7)}, 7) {
			t.Fatal("expected true")
		}
	})
	t.Run("false for a different primary", func(t *testing.T) {
		if isPrimaryIP(map[string]interface{}{"primary_ip4_id": float64(99)}, 42) {
			t.Fatal("expected false")
		}
	})
	t.Run("false when no primary is set", func(t *testing.T) {
		if isPrimaryIP(map[string]interface{}{}, 42) {
			t.Fatal("expected false")
		}
	})
}

// TestFetchDevices is not in the task brief; it was added because
// golangci-lint flags fetchDevices as unused until the next task wires it
// into the enrichment path. It exercises the function for real rather than
// suppressing the lint: enough ids are requested to force chunkByBudget into
// multiple batches, each served by a separate request whose "id" params
// reflect only that batch, and the results from every batch must land in one
// map keyed by device id.
func TestFetchDevices(t *testing.T) {
	t.Run("batches large id sets into a single id-keyed map", func(t *testing.T) {
		requests := 0

		mux := http.NewServeMux()
		mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
			requests++
			ids := r.URL.Query()["id"]
			var b strings.Builder
			b.WriteString(`{"count":0,"next":null,"results":[`)
			for i, id := range ids {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"id":%s,"name":"dev-%s","primary_ip4":{"id":%s,"address":"10.0.0.1/32"}}`, id, id, id)
			}
			b.WriteString("]}")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(b.String()))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

		const n = 2000 // comfortably over chunkBudgetBytes at "id=<n>" widths, forcing >1 chunk
		ids := make([]int, n)
		for i := range ids {
			ids[i] = i + 1
		}

		got, deg := p.fetchDevices(context.Background(), ids, true)
		if deg.any() {
			t.Fatalf("a fully successful hop must report no degradation, got %+v", deg)
		}
		if len(got) != n {
			t.Fatalf("got %d devices, want %d", len(got), n)
		}
		if requests < 2 {
			t.Fatalf("expected batching to span multiple requests, got %d", requests)
		}

		dev, ok := got[1]
		if !ok {
			t.Fatal("missing device id 1 in result map")
		}
		// primary_ip4 is a nested reference; flattenObject must have derived
		// primary_ip4_id from it as a float64 for isPrimaryIP to match on.
		if !isPrimaryIP(dev, 1) {
			t.Fatalf("expected device 1's primary_ip4_id to equal its own id, got %v", dev["primary_ip4_id"])
		}
	})

	// The projection is conditional, and both halves have a failure mode worth a
	// test. Sending it when device_* is selected would blank nine columns with a
	// 200 and no warning (?fields= is silent about names it does not know, see
	// fieldsParam); NOT sending it when the hop runs for is_primary_ip alone pays
	// the whole device serializer for one boolean — 6,378 bytes against 731 for
	// three ids on NetBox 4.4.10, measured, which is the whole reason the parameter
	// exists.
	//
	// The server answers with only the projected keys when a projection arrives,
	// exactly as NetBox does, so a hop that asked for the wrong names would fail
	// here rather than quietly resolve nothing.
	t.Run("projects to the flag's sources only when no device_* column is selected", func(t *testing.T) {
		var fields, excludes []string
		mux := http.NewServeMux()
		mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
			f := r.URL.Query().Get("fields")
			fields = append(fields, f)
			excludes = append(excludes, r.URL.Query().Get("exclude"))
			body := `{"id":100,"name":"leaf-01","site":{"id":5,"name":"AMS1","slug":"ams1"},"primary_ip4":{"id":1,"address":"10.0.0.1/32"},"primary_ip6":null}`
			if f != "" {
				body = `{"id":100,"primary_ip4":{"id":1,"address":"10.0.0.1/32"},"primary_ip6":null}`
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"count":1,"next":null,"results":[%s]}`, body)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

		got, _ := p.fetchDevices(context.Background(), []int{100}, false)
		if len(fields) != 1 || fields[0] != "id,primary_ip4,primary_ip6" {
			t.Fatalf("projection = %q, want id,primary_ip4,primary_ip6: the hop is running for is_primary_ip alone, so the rest of the device serializer is fetched and thrown away", fields)
		}
		// Free in bytes and not free upstream: DeviceViewSet annotates config
		// context onto the queryset, which the projection alone does not remove.
		if excludes[0] != configContextField {
			t.Errorf("exclude = %q, want %q alongside a projection that already drops it", excludes[0], configContextField)
		}
		// The projected response must still answer the question it was cut down
		// for, which is the failure ?fields= makes invisible.
		if !isPrimaryIP(got[100], 1) {
			t.Errorf("device 100 primary_ip4_id = %v, want the projected response to still resolve the flag", got[100]["primary_ip4_id"])
		}

		fields, excludes = nil, nil
		got, _ = p.fetchDevices(context.Background(), []int{100}, true)
		if len(fields) != 1 || fields[0] != "" {
			t.Fatalf("projection = %q, want none: device_* is nine columns spread across the serializer, and a name ?fields= does not recognize blanks one silently", fields)
		}
		if excludes[0] != "" {
			t.Errorf("exclude = %q, want none: config_context is a real column an unprojected device query surfaces", excludes[0])
		}
		if got[100]["name"] != "leaf-01" || got[100]["site"] != "AMS1" {
			t.Errorf("device 100 = %v, want the full serializer's device_* sources", got[100])
		}
	})

	t.Run("empty ids returns an empty map without a request", func(t *testing.T) {
		requests := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
			requests++
			_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
		got, deg := p.fetchDevices(context.Background(), nil, true)
		if deg.any() {
			t.Fatalf("no ids means nothing to degrade, got %+v", deg)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty map, got %d entries", len(got))
		}
		if requests != 0 {
			t.Fatalf("expected no requests for empty ids, got %d", requests)
		}
	})
}

// TestFetchVMs is TestFetchDevices' mirror, and covers the three properties the
// VM hop inherits from it rather than reinvents: batching a large id set into
// one id-keyed map, an empty id set costing no request at all (which is how
// ResolveIPs skips the hop), and a failed batch reporting a degradation instead
// of an error, because a VM-hop failure must never fail the query.
//
// It also pins the projection. The hop exists for one boolean, so asking NetBox
// for the whole VM serializer would be a payload this package has no use for —
// and ?fields= is SILENT about names it does not know (see fieldsParam), so a
// projection that named the wrong things would fetch objects with no
// primary_ip4 in them and answer false for every VM. That failure is invisible
// in the response and looks exactly like "no VM has a primary IP".
func TestFetchVMs(t *testing.T) {
	t.Run("batches large id sets into a single id-keyed map, projected to the flag's sources", func(t *testing.T) {
		requests := 0
		var projections []string

		mux := http.NewServeMux()
		mux.HandleFunc("/api/virtualization/virtual-machines/", func(w http.ResponseWriter, r *http.Request) {
			requests++
			projections = append(projections, r.URL.Query().Get("fields"))
			ids := r.URL.Query()["id"]
			var b strings.Builder
			b.WriteString(`{"count":0,"next":null,"results":[`)
			for i, id := range ids {
				if i > 0 {
					b.WriteString(",")
				}
				// Only the projected keys, exactly as NetBox answers a ?fields=
				// query: no name, no cluster, no status.
				fmt.Fprintf(&b, `{"id":%s,"primary_ip4":{"id":%s,"address":"10.0.0.1/32"},"primary_ip6":null}`, id, id)
			}
			b.WriteString("]}")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(b.String()))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

		const n = 2000 // comfortably over chunkBudgetBytes at "id=<n>" widths, forcing >1 chunk
		ids := make([]int, n)
		for i := range ids {
			ids[i] = i + 1
		}

		got, deg := p.fetchVMs(context.Background(), ids)
		if deg.any() {
			t.Fatalf("a fully successful hop must report no degradation, got %+v", deg)
		}
		if len(got) != n {
			t.Fatalf("got %d virtual machines, want %d", len(got), n)
		}
		if requests < 2 {
			t.Fatalf("expected batching to span multiple requests, got %d", requests)
		}
		for i, p := range projections {
			if p != "id,primary_ip4,primary_ip6" {
				t.Errorf("batch %d projection = %q, want %q — every batch, not just the first", i+1, p, "id,primary_ip4,primary_ip6")
			}
		}

		vm, ok := got[1]
		if !ok {
			t.Fatal("missing virtual machine id 1 in result map")
		}
		// primary_ip4 is a nested reference; flattenObject must have derived
		// primary_ip4_id from it as a float64 for isPrimaryIP to match on.
		if !isPrimaryIP(vm, 1) {
			t.Fatalf("expected VM 1's primary_ip4_id to equal its own id, got %v", vm["primary_ip4_id"])
		}
	})

	t.Run("empty ids returns an empty map without a request", func(t *testing.T) {
		requests := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/api/virtualization/virtual-machines/", func(w http.ResponseWriter, r *http.Request) {
			requests++
			_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
		got, deg := p.fetchVMs(context.Background(), nil)
		if deg.any() {
			t.Fatalf("no ids means nothing to degrade, got %+v", deg)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty map, got %d entries", len(got))
		}
		if requests != 0 {
			t.Fatalf("expected no requests for empty ids, got %d", requests)
		}
	})

	t.Run("a failed batch degrades instead of erroring", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/virtualization/virtual-machines/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"You do not have permission to perform this action."}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
		// There is no error return to check: the signature itself is the
		// guarantee, for the reason fetchDevices' comment gives — an error here
		// would have no consumer, and discarding one is how a total failure came
		// out looking like fact.
		got, deg := p.fetchVMs(context.Background(), []int{5, 6})
		if len(got) != 0 {
			t.Fatalf("a forbidden endpoint cannot produce virtual machines, got %v", got)
		}
		if deg.failed != 2 || deg.total != 2 {
			t.Fatalf("degradation = %+v, want 2 of 2 failed", deg)
		}
		if deg.cause == nil {
			t.Fatal("degradation must carry the cause; the warning renders it")
		}
	})
}

func TestIPEnrichColumnsAreNamespaced(t *testing.T) {
	cols := IPEnrichColumns()
	// Three un-namespaced columns lead, not two. is_primary_ip joined them when
	// it gained a virtual-machine half: it is answered by the device for one row
	// and by the virtual machine for the next, so device_ would be a lie on half
	// the rows it fills and vm_ a lie on the other half. Its position is
	// asserted rather than
	// merely tolerated — the frontend mirror groups by exactly these leading
	// names (TestIPContextFieldsMatchFrontend).
	if cols[0] != "ip" || cols[1] != "match_count" || cols[2] != isPrimaryIPColumn {
		t.Fatalf("identity columns must lead un-namespaced, got %v", cols[:3])
	}
	for _, c := range cols[3:] {
		switch {
		case strings.HasPrefix(c, "prefix_"), strings.HasPrefix(c, "address_"),
			strings.HasPrefix(c, "interface_"), strings.HasPrefix(c, "device_"),
			strings.HasPrefix(c, "vm_"):
		default:
			t.Errorf("column %q is not namespaced", c)
		}
	}
	for _, banned := range []string{"site", "tenant", "role", "vrf", "vlan", "prefix", "prefix_site"} {
		for _, c := range cols {
			if c == banned {
				t.Errorf("column %q must not exist", banned)
			}
		}
	}
	for _, unfillable := range []string{"interface_enabled", "interface_type", "interface_mtu", "interface_mac_address", "interface_lag"} {
		for _, c := range cols {
			if c == unfillable {
				t.Errorf("column %q has no source and must not be offered", unfillable)
			}
		}
	}
	// NetBox 4.4's NestedIPAddress serializer (nat_inside and every
	// nat_outside element) has no dns_name property at all — filling these
	// would cost a second API call per referenced NAT partner, ruled not
	// worth it. This guard is what stops them creeping back in.
	for _, unfillable := range []string{"address_nat_inside_dns", "address_nat_outside_dns"} {
		for _, c := range cols {
			if c == unfillable {
				t.Errorf("column %q has no source and must not be offered", unfillable)
			}
		}
	}
}

// tsGroupOptions parses the namespaced groups out of IP_CONTEXT_FIELD_GROUPS,
// each of which is a literal array of bare names mapped onto a prefix:
//
//	options: ['cidr', 'scope'].map((f) => ({ label: f, value: `prefix_${f}` })),
//
// (prettier sometimes wraps the .map( onto its own line, hence the \s*).
var tsGroupOptions = regexp.MustCompile("(?s)options:\\s*\\[([^\\]]*)\\]\\.map\\(\\s*\\(f\\)\\s*=>\\s*\\(\\{[^`]*`([a-z]+)_\\$\\{f\\}`")

// tsIdentityOption matches the un-namespaced Identity group, whose two options
// are written out in full rather than mapped.
var tsIdentityOption = regexp.MustCompile(`value: '([a-z_]+)'`)

var tsQuoted = regexp.MustCompile(`'([a-z_0-9]+)'`)

// TestIPContextFieldsMatchFrontend is the mechanical half of the "keep in sync"
// comments that sit on both IPEnrichColumns() and src/types.ts'
// IP_CONTEXT_FIELD_GROUPS. The picker is a closed vocabulary: a name offered in
// the editor that the backend cannot produce renders a permanently blank
// column, and a backend column missing from the editor is unreachable. Nothing
// enforced the pairing, so drift shipped silently.
//
// The check reads the TypeScript source rather than executing it because it
// belongs on whichever side can run it for free: reading a file from Jest needs
// Node's fs typings, which this project's tsconfig does not include.
func TestIPContextFieldsMatchFrontend(t *testing.T) {
	// Narrowed to the declaration (see tsDeclaration) so unrelated string
	// literals elsewhere in the file — filter operators, join transforms —
	// cannot leak into the comparison.
	decl := tsDeclaration(t, "IP_CONTEXT_FIELD_GROUPS")

	var tsCols []string
	for _, m := range tsIdentityOption.FindAllStringSubmatch(decl, -1) {
		tsCols = append(tsCols, m[1])
	}
	for _, m := range tsGroupOptions.FindAllStringSubmatch(decl, -1) {
		for _, q := range tsQuoted.FindAllStringSubmatch(m[1], -1) {
			tsCols = append(tsCols, m[2]+"_"+q[1])
		}
	}

	goCols := IPEnrichColumns()
	if len(tsCols) != len(goCols) {
		t.Fatalf("editor offers %d columns, IPEnrichColumns() emits %d\n editor: %v\n backend: %v",
			len(tsCols), len(goCols), sorted(tsCols), sorted(goCols))
	}
	a, b := sorted(tsCols), sorted(goCols)
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("column mismatch at %d: editor %q, backend %q (full lists: %v vs %v)", i, a[i], b[i], a, b)
		}
	}
}

// TestIPContextDefaultsMatchFrontend pairs the two default selections. Drift
// here means a brand-new query opens with a column nothing can fill.
func TestIPContextDefaultsMatchFrontend(t *testing.T) {
	decl := tsDeclaration(t, "DEFAULT_IP_CONTEXT_FIELDS")

	var tsDefaults []string
	for _, q := range tsQuoted.FindAllStringSubmatch(decl, -1) {
		tsDefaults = append(tsDefaults, q[1])
	}
	a, b := sorted(tsDefaults), sorted(defaultIPEnrichFields)
	if len(a) != len(b) {
		t.Fatalf("editor default is %v, backend default is %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("default mismatch at %d: editor %q, backend %q", i, a[i], b[i])
		}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestResolveIPs_MatchCountIsNumber locks in match_count's Go type as float64.
// pkg/plugin/frame.go's classifyColumn only treats float64 as numeric (the
// type every other column already carries via encoding/json + flattenObject);
// an int would silently render match_count as a string field in the Grafana
// frame, which breaks thresholding, filtering, and color-by-value on the very
// column that exists to flag an ambiguous pick.
func TestResolveIPs_MatchCountIsNumber(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":1,"address":"10.0.0.5/24","status":{"value":"active"},"assigned_object":null}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.5"}, []string{"ip", "match_count"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Rows))
	}

	v, ok := res.Rows[0]["match_count"].(float64)
	if !ok {
		t.Fatalf("match_count is %T, want float64 (an int here would render as a string column)", res.Rows[0]["match_count"])
	}
	if v != 1 {
		t.Fatalf("match_count = %v, want 1", v)
	}
}

// TestApplyAddressColumns_InterfaceDescriptionAndNATPeers locks in that
// interface_description is read straight off the raw JSON — flattenObject's
// generic nested-object rule only ever surfaces a nested object's
// display/id/slug, so assigned_object's description had no source at all
// before — alongside address_nat_inside/address_nat_outside, which DO come
// through flattenObject as the peer address's display string. There is no
// address_nat_inside_dns/address_nat_outside_dns: NetBox 4.4's
// NestedIPAddress serializer (used for both nat_inside and every nat_outside
// element) has no dns_name property, so that pair was dropped rather than
// costing a second API call per referenced NAT partner.
func TestApplyAddressColumns_InterfaceDescriptionAndNATPeers(t *testing.T) {
	raw := json.RawMessage(`{
		"id": 45,
		"address": "203.0.113.10/32",
		"status": {"value": "active"},
		"assigned_object_type": "dcim.interface",
		"assigned_object": {
			"id": 9, "name": "Ethernet1", "description": "core uplink",
			"device": {"id": 100, "name": "leaf-01"}
		},
		"nat_inside": {"id": 44, "display": "192.168.50.10/24"},
		"nat_outside": [
			{"id": 46, "display": "203.0.113.11/32"}
		]
	}`)

	row := map[string]interface{}{}
	applyAddressColumns(row, raw)

	if got := row["interface_description"]; got != "core uplink" {
		t.Errorf("interface_description = %v, want %q", got, "core uplink")
	}
	if got := row["address_nat_inside"]; got != "192.168.50.10/24" {
		t.Errorf("address_nat_inside = %v, want %q", got, "192.168.50.10/24")
	}
	if got := row["address_nat_outside"]; got != "203.0.113.11/32" {
		t.Errorf("address_nat_outside = %v, want %q", got, "203.0.113.11/32")
	}
	for _, c := range []string{"address_nat_inside_dns", "address_nat_outside_dns"} {
		if _, ok := row[c]; ok {
			t.Errorf("column %q = %v, want unset (dropped: NetBox has no source for it)", c, row[c])
		}
	}
}

// TestApplyAddressColumns_HandlesEmptyNATAndNoAssignment locks in that a bare
// address record — no NAT relationship in either direction, no interface
// assignment — leaves interface_description unset rather than panicking.
func TestApplyAddressColumns_HandlesEmptyNATAndNoAssignment(t *testing.T) {
	raw := json.RawMessage(`{
		"id": 1, "address": "10.0.0.1/32", "status": {"value": "active"},
		"assigned_object_type": null, "assigned_object": null,
		"nat_inside": null, "nat_outside": []
	}`)

	row := map[string]interface{}{}
	applyAddressColumns(row, raw) // must not panic

	if _, ok := row["interface_description"]; ok {
		t.Errorf("interface_description = %v, want unset", row["interface_description"])
	}
}

// TestApplyAddressColumns_InterfaceColumnsRequireAnInterface is the reproduction
// for a defect proved against live NetBox 4.4.10: assigned_object is a GENERIC
// relation, and reading it without testing assigned_object_type put an FHRP
// group's display string into interface_name.
//
// The fhrpGroup case below is the live payload verbatim (NetBox 4.4.10, group
// "zz-codex2-fhrp VRRPv3: 993", address 10.77.77.78/24), reduced only by the
// url/created/last_updated fields nothing here reads. Against the unfixed code
// it yields:
//
//	interface_name        = "zz-codex2-fhrp VRRPv3: 993 (10.77.77.78/24)"
//	interface_description = "temp fixture for codex round2 finding 3"
//
// A VRRP group rendered as a switch port, which is worse than a blank column:
// blank invites a question, a plausible wrong value does not. Both interface_*
// columns are checked because assigned_object.description is read by a second,
// independent code path that had the same ungated defect.
//
// The two interface cases are here rather than left to the tests above so that
// one table states the whole rule. A gate that excluded FHRP by excluding
// everything would pass an FHRP-only test and silently blank the VM case, which
// docs/RECIPES.md documents as populating interface_name.
// wantAssignedType is also checked in every case below: address_assigned_object_type
// is the one column that tells an FHRP-assigned address apart from a wholly
// unassigned one, since both leave interface_* and device_* blank. It carries the
// raw assigned_object_type string (never a friendly label — the field has none) and
// must be unset, not the empty string, for an address with no assignment at all.
func TestApplyAddressColumns_InterfaceColumnsRequireAnInterface(t *testing.T) {
	cases := []struct {
		name             string
		raw              string
		wantName         interface{} // nil = the column must be unset
		wantDesc         interface{}
		wantAssignedType interface{}
		wantVMName       interface{}
	}{
		{
			name: "a dcim.interface assignment fills interface_*",
			raw: `{"id":1,"address":"10.20.0.1/24","status":{"value":"active"},
				"assigned_object_type":"dcim.interface",
				"assigned_object":{"id":9,"display":"Ethernet1","name":"Ethernet1",
					"description":"uplink to AMS1-spine-01","device":{"id":100,"name":"AMS1-leaf-01"}}}`,
			wantName:         "Ethernet1",
			wantDesc:         "uplink to AMS1-spine-01",
			wantAssignedType: "dcim.interface",
		},
		{
			name: "a virtualization.vminterface assignment fills interface_* too",
			// device_* is what a VM withholds, NOT interface_*. If the gate is
			// ever narrowed to dcim.interface alone this case fails, which is
			// the point: the VM row's only context column would vanish.
			raw: `{"id":31,"address":"10.40.0.5/24","status":{"value":"active"},
				"assigned_object_type":"virtualization.vminterface",
				"assigned_object":{"id":55,"display":"eth0","name":"eth0",
					"description":"vm nic","virtual_machine":{"id":5,"name":"vm-01"}}}`,
			wantName:         "eth0",
			wantDesc:         "vm nic",
			wantAssignedType: "virtualization.vminterface",
			// The VM row IS named — just not in device_name. vm_name is the only
			// column that can carry it, so if this stops being set the row loses
			// its owner entirely.
			wantVMName: "vm-01",
		},
		{
			name: "an ipam.fhrpgroup assignment fills neither interface_* column, but is distinguishable via address_assigned_object_type",
			raw: `{"id":53,"display":"10.77.77.78/24","address":"10.77.77.78/24",
				"vrf":null,"tenant":null,"status":{"value":"active","label":"Active"},"role":null,
				"assigned_object_type":"ipam.fhrpgroup","assigned_object_id":2,
				"assigned_object":{"id":2,"display":"zz-codex2-fhrp VRRPv3: 993 (10.77.77.78/24)",
					"protocol":"vrrp3","group_id":993,
					"description":"temp fixture for codex round2 finding 3"},
				"nat_inside":null,"nat_outside":[],"dns_name":"zz-codex2-vip.example.net",
				"description":"temp fixture codex round2"}`,
			wantAssignedType: "ipam.fhrpgroup",
		},
		{
			name: "an unassigned address fills neither, and address_assigned_object_type is blank too",
			raw: `{"id":21,"address":"10.0.0.2/32","status":{"value":"active"},
				"assigned_object_type":null,"assigned_object":null}`,
		},
		{
			name: "an assigned_object with no type at all fills neither",
			// Not a shape NetBox sends; the point is that the gate fails CLOSED.
			// An unrecognised or absent type must withhold interface_*, so a
			// future NetBox assignment target cannot inherit the old defect
			// merely by being unknown to this code.
			raw: `{"id":77,"address":"10.0.0.7/32","status":{"value":"active"},
				"assigned_object":{"id":3,"display":"something new","description":"who knows"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]interface{}{}
			applyAddressColumns(row, json.RawMessage(tc.raw))

			if got := row["interface_name"]; got != tc.wantName {
				t.Errorf("interface_name = %#v, want %#v", got, tc.wantName)
			}
			if got := row["interface_description"]; got != tc.wantDesc {
				t.Errorf("interface_description = %#v, want %#v", got, tc.wantDesc)
			}
			if got := row["address_assigned_object_type"]; got != tc.wantAssignedType {
				t.Errorf("address_assigned_object_type = %#v, want %#v", got, tc.wantAssignedType)
			}
			// Asserted in EVERY case, not just the VM one: vm_name is the
			// counterpart of device_name and the two are mutually exclusive per
			// row, so a dcim.interface, an FHRP group and an unassigned address
			// must each leave it unset.
			if got := row["vm_name"]; got != tc.wantVMName {
				t.Errorf("vm_name = %#v, want %#v", got, tc.wantVMName)
			}
			// The address record itself is real whatever it is assigned to, so
			// withholding interface_* must not withhold address_*. Only the
			// assignment is something interface_* cannot describe.
			if _, ok := row["address_status"]; !ok {
				t.Errorf("address_status is unset; the address record is real regardless of what it is assigned to")
			}
		})
	}
}

// TestDeviceIDFromAddress is the unit-level companion to the VM subtest in
// TestResolveIPs below. It exists because deviceIDFromAddress has TWO
// independent reasons to reject a VM interface — the assigned_object_type gate
// and the "no assigned_object.device.id" fallback — and real NetBox
// vminterface JSON (which carries virtual_machine, never device) trips the
// second one, so an end-to-end test can never tell whether the type gate is
// still there. The third case below settles that directly: it asserts the
// documented contract ("VM interfaces return false") even for a payload the
// fallback would let through.
func TestDeviceIDFromAddress(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantID int
		wantOK bool
	}{
		{
			name:   "a dcim.interface assignment yields its device id",
			raw:    `{"id":1,"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}`,
			wantID: 100,
			wantOK: true,
		},
		{
			name:   "a virtualization.vminterface assignment is excluded",
			raw:    `{"id":31,"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":55,"name":"eth0","virtual_machine":{"id":5,"name":"vm-01"}}}`,
			wantOK: false,
		},
		{
			name: "the type gate excludes a vminterface even when a device id is present",
			// Not a shape NetBox sends today — that is the point. The type gate
			// is what makes the exclusion hold regardless of what the
			// assigned_object serializer grows later, so it gets asserted on
			// its own rather than through the device-id fallback.
			raw:    `{"id":31,"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":55,"name":"eth0","device":{"id":100,"name":"leaf-01"}}}`,
			wantOK: false,
		},
		{
			name:   "an unassigned address is excluded",
			raw:    `{"id":21,"assigned_object_type":null,"assigned_object":null}`,
			wantOK: false,
		},
		{
			name:   "a dcim.interface with no nested device is excluded",
			raw:    `{"id":2,"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1"}}`,
			wantOK: false,
		},
		{
			name:   "malformed JSON is excluded rather than panicking",
			raw:    `{"id":`,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := deviceIDFromAddress(json.RawMessage(tc.raw))
			if ok != tc.wantOK || id != tc.wantID {
				t.Errorf("deviceIDFromAddress = (%d, %t), want (%d, %t)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestVMIDFromAddress is the third member of the family, and the one the VM hop
// batches on. Same generic-relation payload, same standalone type gate, and the
// same reason for asserting the gate on its own: a dcim.interface payload that
// grew a virtual_machine key must yield nothing here, or a device row would be
// looked up in the VM keyspace — different NetBox model, unrelated object, and
// an is_primary_ip answered from it that is wrong rather than absent.
func TestVMIDFromAddress(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantID int
		wantOK bool
	}{
		{
			name:   "a virtualization.vminterface assignment yields its VM id",
			raw:    `{"id":31,"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":55,"name":"eth0","virtual_machine":{"id":5,"name":"vm-01"}}}`,
			wantID: 5,
			wantOK: true,
		},
		{
			name:   "a dcim.interface assignment is excluded",
			raw:    `{"id":1,"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}`,
			wantOK: false,
		},
		{
			name: "the type gate excludes a dcim.interface even when a virtual_machine is present",
			// Not a shape NetBox sends today, which is the point — the exclusion
			// must hold on the type constant rather than on what the serializer
			// happens to carry.
			raw:    `{"id":1,"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1","virtual_machine":{"id":5,"name":"vm-01"}}}`,
			wantOK: false,
		},
		{
			name: "an ipam.fhrpgroup assignment is excluded",
			// The FHRP case at its source: an FHRP group produces no owner to look
			// up, which is what leaves is_primary_ip absent rather than false.
			raw:    `{"id":53,"assigned_object_type":"ipam.fhrpgroup","assigned_object":{"id":2,"display":"web-vip VRRPv3: 993 (10.0.0.6/24)"}}`,
			wantOK: false,
		},
		{
			name:   "an unassigned address is excluded",
			raw:    `{"id":21,"assigned_object_type":null,"assigned_object":null}`,
			wantOK: false,
		},
		{
			name: "a vminterface with no nested virtual_machine is excluded",
			// id 0 is not a VM. Batching it would ask NetBox ?id=0 and index the
			// answer under a key no row can match.
			raw:    `{"id":32,"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":56,"name":"eth1"}}`,
			wantOK: false,
		},
		{
			name:   "malformed JSON is excluded rather than panicking",
			raw:    `{"id":`,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := vmIDFromAddress(json.RawMessage(tc.raw))
			if ok != tc.wantOK || id != tc.wantID {
				t.Errorf("vmIDFromAddress = (%d, %t), want (%d, %t)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestVMNameFromAddress mirrors TestDeviceIDFromAddress, and exists for the same
// reason: vmNameFromAddress has two independent ways to say no — the
// assigned_object_type gate and the "no assigned_object.virtual_machine.name"
// fallback — and real NetBox dcim.interface JSON (which carries device, never
// virtual_machine) trips the second one, so an end-to-end test could never show
// whether the type gate is still there.
//
// The third case is the mirror image of that test's third case, and is the whole
// point of keeping this a standalone function: a dcim.interface payload that
// hypothetically also carried a virtual_machine key must yield NO vm_name. That
// proves the gate is on the TYPE CONSTANT rather than on the payload's shape,
// which is what keeps vm_name and device_name mutually exclusive per row no
// matter what NetBox's serializer grows later.
func TestVMNameFromAddress(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantName string
		wantOK   bool
	}{
		{
			name: "a virtualization.vminterface assignment yields its VM name",
			// display deliberately DIFFERS from name. Real NetBox happens to send
			// them equal for a VM today, but a fixture that mirrors that cannot
			// tell the two keys apart — and telling them apart is the entire point
			// of this case, since sourcing the column from display is the exact
			// regression that produced the FHRP mislabelling fixed in #96.
			raw:      `{"id":31,"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":55,"name":"eth0","virtual_machine":{"id":2,"display":"demo-vm-01 (AMS1 cluster)","name":"demo-vm-01"}}}`,
			wantName: "demo-vm-01",
			wantOK:   true,
		},
		{
			name:   "a dcim.interface assignment is excluded",
			raw:    `{"id":1,"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}`,
			wantOK: false,
		},
		{
			name: "the type gate excludes a dcim.interface even when a virtual_machine is present",
			// Not a shape NetBox sends today — that is the point, and it is the
			// exact reverse of TestDeviceIDFromAddress' third case. Rejecting on
			// the type constant, not on "did the payload happen to carry a
			// virtual_machine", is what stops a device row acquiring a vm_name.
			raw:    `{"id":1,"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"},"virtual_machine":{"id":2,"name":"demo-vm-01"}}}`,
			wantOK: false,
		},
		{
			name:   "an ipam.fhrpgroup assignment is excluded",
			raw:    `{"id":53,"assigned_object_type":"ipam.fhrpgroup","assigned_object":{"id":2,"display":"zz-probe-fhrp VRRPv3: 991 (10.77.77.77/24)"}}`,
			wantOK: false,
		},
		{
			name:   "an unassigned address is excluded",
			raw:    `{"id":21,"assigned_object_type":null,"assigned_object":null}`,
			wantOK: false,
		},
		{
			name: "a vminterface with no nested virtual_machine name is excluded",
			// Blank is not a name. Setting vm_name to "" would put an empty
			// string where "this row has no VM" belongs, and the two read
			// differently in a table.
			raw:    `{"id":32,"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":56,"name":"eth1"}}`,
			wantOK: false,
		},
		{
			name:   "malformed JSON is excluded rather than panicking",
			raw:    `{"id":`,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, ok := vmNameFromAddress(json.RawMessage(tc.raw))
			if ok != tc.wantOK || name != tc.wantName {
				t.Errorf("vmNameFromAddress = (%q, %t), want (%q, %t)", name, ok, tc.wantName, tc.wantOK)
			}
		})
	}
}

// addressEchoServer answers ipam/ip-addresses with exactly one record per
// requested ?address= value, keyed on that value's host portion so the same
// host always gets the same NetBox id however it was spelled. It is the
// fixture for the two properties below — the chunk-boundary match_count bug and
// the default row limit — both of which need a large IP set and neither of
// which is about what the records contain.
func addressEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	ids := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			h := hostOf(a)
			id, ok := ids[h]
			if !ok {
				id = len(ids) + 1
				ids[h] = id
			}
			results = append(results, fmt.Sprintf(
				`{"id":%d,"address":"%s/24","status":{"value":"active"},"assigned_object":null}`, id, h))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveIPs_MatchCountCountsDistinctRecords reproduces a real
// double-count: input IPs are indexed by host portion, so "10.0.0.5" and
// "10.0.0.5/24" share a bucket, and when the two spellings land in DIFFERENT
// byte-budget chunks each chunk's response contributes the same NetBox record.
// len(bucket) then reported match_count 2 for a host with exactly one record —
// flagging an unambiguous resolution as ambiguous, which is the opposite of
// what the column exists for.
func TestResolveIPs_MatchCountCountsDistinctRecords(t *testing.T) {
	// Bracket a large filler set so the two spellings cannot share a chunk.
	ips := []string{"10.0.0.5"}
	for i := 0; i < 600; i++ {
		ips = append(ips, fmt.Sprintf("10.%d.%d.9", i/256, i%256))
	}
	ips = append(ips, "10.0.0.5/24")

	chunks := addressChunks(ips)
	if len(chunks) < 2 {
		t.Fatalf("test needs the two spellings in different chunks, got %d chunk(s)", len(chunks))
	}
	first, last := chunks[0], chunks[len(chunks)-1]
	if first[0] != "10.0.0.5" || last[len(last)-1] != "10.0.0.5/24" {
		t.Fatalf("fixture drifted: %q ... %q", first[0], last[len(last)-1])
	}

	srv := addressEchoServer(t)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "match_count"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}

	for _, row := range res.Rows {
		ip := fmt.Sprint(row["ip"])
		if ip != "10.0.0.5" && ip != "10.0.0.5/24" {
			continue
		}
		if got := row["match_count"]; got != float64(1) {
			t.Errorf("match_count for %q = %v, want 1 (both spellings resolve to the same single record)", ip, got)
		}
	}
}

func TestDistinctAddressCount(t *testing.T) {
	raw := func(id int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"id":%d,"address":"10.0.0.5/24"}`, id))
	}
	cases := []struct {
		name string
		in   []json.RawMessage
		want int
	}{
		{"none", nil, 0},
		{"one", []json.RawMessage{raw(1)}, 1},
		{"the same record twice collapses", []json.RawMessage{raw(1), raw(1)}, 1},
		{"genuine anycast duplicates all count", []json.RawMessage{raw(1), raw(2), raw(3)}, 3},
		// A payload with no id cannot be deduped; counting it keeps the number an
		// upper bound rather than silently hiding a candidate.
		{"id-less records each count", []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{}`)}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctAddressCount(tc.in); got != tc.want {
				t.Errorf("distinctAddressCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestResolveIPs_LimitDefaults locks the unset-limit fallback to defaultLimit
// rather than MaxLimit. This is not cosmetic: the prefix fallback issues one
// SERIAL ?contains= request per unmatched IP (contains takes a single value, so
// it cannot be batched), which made an unset limit a multi-minute ceiling.
// Query() has always defaulted the same way; ip-enrichment was the outlier.
func TestResolveIPs_LimitDefaults(t *testing.T) {
	makeIPs := func(n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("10.%d.%d.1", i/256, i%256))
		}
		return out
	}

	srv := addressEchoServer(t)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})

	t.Run("an unset limit uses defaultLimit, not MaxLimit", func(t *testing.T) {
		ips := makeIPs(defaultLimit + 5)
		res, err := p.ResolveIPs(context.Background(), ips, []string{"ip"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Rows) != defaultLimit {
			t.Errorf("rows = %d, want %d (defaultLimit)", len(res.Rows), defaultLimit)
		}
		if res.Total != len(ips) {
			t.Errorf("Total = %d, want %d — the clamp must still report as truncated", res.Total, len(ips))
		}
	})

	t.Run("an explicit limit above the default is still honoured", func(t *testing.T) {
		res, err := p.ResolveIPs(context.Background(), makeIPs(defaultLimit+300), []string{"ip"}, defaultLimit+200)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Rows) != defaultLimit+200 {
			t.Errorf("rows = %d, want %d — the default must be a default, not a cap", len(res.Rows), defaultLimit+200)
		}
	})
}

// TestResolveIPs covers the properties this task exists to guarantee that had
// no unit test: Total is the pre-clamp distinct-input count (not the row
// count), exactly one row per input IP (duplicates and whitespace collapse,
// nothing is dropped), an anycast IP's match_count reflects every candidate,
// a VM-assigned IP populates interface_name while every device_* stays nil,
// and "ip" is force-included even when the caller's field list omits it.
//
// The mock serves BOTH ipam/ip-addresses and dcim/devices. The second one is
// not incidental: ResolveIPs deliberately swallows a device-hop error, so a
// mock without a devices endpoint makes every device_* column nil for reasons
// that have nothing to do with the logic under test, and the VM subtest below
// becomes unfalsifiable. See the deviceFixtures comment.
func TestResolveIPs(t *testing.T) {
	// Address fixtures, keyed by the bare host NetBox's ?address= filter
	// matches on.
	addrFixtures := map[string][]string{
		"10.0.0.1": {
			`{"id":1,"address":"10.0.0.1/32","status":{"value":"active"},"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}`,
		},
		"10.0.0.2": { // anycast: three distinct records at the same host
			`{"id":21,"address":"10.0.0.2/32","status":{"value":"active"},"assigned_object_type":null,"assigned_object":null}`,
			`{"id":22,"address":"10.0.0.2/32","status":{"value":"active"},"assigned_object_type":null,"assigned_object":null}`,
			`{"id":23,"address":"10.0.0.2/32","status":{"value":"active"},"assigned_object_type":null,"assigned_object":null}`,
		},
		"10.0.0.3": { // VM-assigned: assigned_object_type is virtualization.vminterface
			`{"id":31,"address":"10.0.0.3/24","status":{"value":"active"},"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":55,"display":"eth0","name":"eth0","description":"","virtual_machine":{"id":5,"name":"vm-01"}}}`,
		},
		// FHRP-assigned: assigned_object is present and has a display string,
		// but it is a VRRP group, not an interface. Shape taken from live
		// NetBox 4.4.10.
		"10.0.0.6": {
			`{"id":53,"address":"10.0.0.6/24","dns_name":"vip.example.net","status":{"value":"active"},"assigned_object_type":"ipam.fhrpgroup","assigned_object_id":2,"assigned_object":{"id":2,"display":"web-vip VRRPv3: 993 (10.0.0.6/24)","protocol":"vrrp3","group_id":993,"description":"web tier VIP"}}`,
		},
	}

	// Device fixtures for the dcim/devices hop, keyed by the id string that
	// endpoint is queried with. Registering this endpoint at all is what makes
	// the VM subtest below able to fail: with it, device_* columns demonstrably
	// DO populate for a dcim.interface-assigned address in the very same call,
	// so a nil device_* on the VM row can only mean the VM exclusion gate
	// fired. Without it, fetchDevices 404s, ResolveIPs degrades rather than
	// failing (see the comment on its fetchDevices call), and every device_*
	// column comes out nil no matter what the VM logic does — a test that
	// cannot distinguish "correctly excluded" from "lookup broke".
	deviceFixtures := map[string]string{
		"100": `{"id":100,"name":"leaf-01","role":{"id":2,"name":"Leaf Switch","slug":"leaf-switch"},` +
			`"platform":{"id":3,"name":"Arista EOS","slug":"arista-eos"},` +
			`"device_type":{"id":4,"model":"DCS-7050TX"},"site":{"id":5,"name":"AMS1","slug":"ams1"},` +
			`"location":{"id":6,"name":"Hall 1","slug":"hall-1"},"rack":{"id":7,"name":"R101"},` +
			`"tenant":{"id":8,"name":"NetOps","slug":"netops"},"status":{"value":"active","label":"Active"},` +
			`"primary_ip4":{"id":1,"address":"10.0.0.1/32"}}`,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			results = append(results, addrFixtures[a]...)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, id := range r.URL.Query()["id"] {
			if fx, ok := deviceFixtures[id]; ok {
				results = append(results, fx)
				continue
			}
			// An id outside the fixtures still gets a real, successful device
			// back on purpose. If deviceIDFromAddress ever stops excluding VM
			// interfaces it will hand some device id over for the VM address,
			// and this makes that id resolve — so the VM subtest fails loudly
			// on a populated device_* column instead of passing because the
			// lookup happened to find nothing.
			results = append(results, fmt.Sprintf(
				`{"id":%s,"name":"unexpected-device-%s","status":{"value":"active","label":"Active"}}`, id, id))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	// The device_* group, and only it. is_primary_ip used to belong here and
	// deliberately no longer does: a VM-assigned row is entitled to a value in
	// it, so asserting it nil alongside device_name would pin the opposite of
	// the intended behaviour — and would have kept passing here,
	// since this mock serves no virtualization endpoint. The subtest below states
	// what is true of it instead. Its VM half is covered end-to-end, against a
	// mock that DOES answer, by TestResolveIPs_IsPrimaryIP.
	deviceColumns := []string{
		"device_name", "device_role", "device_platform", "device_device_type",
		"device_site", "device_location", "device_rack", "device_tenant",
		"device_status",
	}

	t.Run("Total is the pre-clamp distinct count, not the post-clamp row count", func(t *testing.T) {
		ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
		res, err := p.ResolveIPs(context.Background(), ips, []string{"ip"}, 2)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if res.Total != 5 {
			t.Errorf("Total = %d, want 5 (every distinct input IP, before the limit clamp)", res.Total)
		}
		if len(res.Rows) != 2 {
			t.Errorf("got %d rows, want 2 (clamped by limit)", len(res.Rows))
		}
	})

	t.Run("Total equals the row count when the input is under the limit", func(t *testing.T) {
		ips := []string{"10.0.0.1", "10.0.0.2"}
		res, err := p.ResolveIPs(context.Background(), ips, []string{"ip"}, 10)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if res.Total != 2 || len(res.Rows) != 2 {
			t.Errorf("Total=%d len(Rows)=%d, want both 2", res.Total, len(res.Rows))
		}
	})

	t.Run("duplicates and whitespace-only entries collapse to one row each, nothing dropped", func(t *testing.T) {
		ips := []string{"10.0.0.1", "", "   ", "10.0.0.1", "10.0.0.2", " 10.0.0.2 "}
		res, err := p.ResolveIPs(context.Background(), ips, []string{"ip"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if res.Total != 2 {
			t.Fatalf("Total = %d, want 2 distinct IPs", res.Total)
		}
		if len(res.Rows) != 2 {
			t.Fatalf("got %d rows, want 2", len(res.Rows))
		}
		got := []string{fmt.Sprint(res.Rows[0]["ip"]), fmt.Sprint(res.Rows[1]["ip"])}
		want := []string{"10.0.0.1", "10.0.0.2"}
		if got[0] != want[0] || got[1] != want[1] {
			t.Errorf("rows = %v, want %v in input order", got, want)
		}
	})

	t.Run("an anycast IP returns exactly one row with match_count 3", func(t *testing.T) {
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.2"}, []string{"ip", "match_count"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(res.Rows))
		}
		if got := res.Rows[0]["match_count"]; got != float64(3) {
			t.Errorf("match_count = %v (%T), want float64(3)", got, got)
		}
	})

	t.Run("a device-assigned IP fills every device_* column while a VM-assigned IP in the same call leaves them all nil", func(t *testing.T) {
		fields := append([]string{"ip", "interface_name", "is_primary_ip"}, deviceColumns...)
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.3"}, fields, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Rows) != 2 {
			t.Fatalf("got %d rows, want 2", len(res.Rows))
		}
		byIP := make(map[string]map[string]interface{}, len(res.Rows))
		for _, r := range res.Rows {
			byIP[fmt.Sprint(r["ip"])] = r
		}
		devRow, ok := byIP["10.0.0.1"]
		if !ok {
			t.Fatalf("no row for the device-assigned IP, got %v", res.Rows)
		}
		vmRow, ok := byIP["10.0.0.3"]
		if !ok {
			t.Fatalf("no row for the VM-assigned IP, got %v", res.Rows)
		}

		// Control half. 10.0.0.1 is assigned to a dcim.interface whose device
		// id resolves against the registered dcim/devices endpoint, so device
		// context provably attaches in this exact call. Without this half, the
		// VM half below would pass just as happily if the device hop had failed
		// outright — ResolveIPs discards that error by design — which is how an
		// earlier version of this subtest survived the VM gate being deleted.
		for _, c := range deviceColumns {
			if devRow[c] == nil {
				t.Errorf("device column %q is nil for a dcim.interface-assigned address; device context must attach here or the VM assertion below proves nothing", c)
			}
		}
		if devRow["device_name"] != "leaf-01" {
			t.Errorf("device_name = %v, want %q", devRow["device_name"], "leaf-01")
		}
		if devRow["device_site"] != "AMS1" {
			t.Errorf("device_site = %v, want %q", devRow["device_site"], "AMS1")
		}
		// This mock serves no virtualization endpoint, so selecting is_primary_ip
		// puts the VM hop through a 404 in this very call — and the device row's
		// flag must be true regardless. The two hops answer disjoint sets of rows,
		// so neither can blank the other's.
		if devRow["is_primary_ip"] != true {
			t.Errorf("is_primary_ip = %v, want true (device 100's primary_ip4 is address id 1)", devRow["is_primary_ip"])
		}

		// The property under test: a virtualization.vminterface assignment
		// yields interface_name and nothing else — never a device.
		if vmRow["interface_name"] != "eth0" {
			t.Errorf("interface_name = %v, want %q", vmRow["interface_name"], "eth0")
		}
		for _, c := range deviceColumns {
			if vmRow[c] != nil {
				t.Errorf("device column %q = %v, want nil for a VM-assigned address", c, vmRow[c])
			}
		}
	})

	// The end-to-end half of TestApplyAddressColumns_InterfaceColumnsRequireAnInterface:
	// the same call resolves an interface-assigned IP and an FHRP-assigned one,
	// so an empty interface_name on the FHRP row cannot be passed off as the
	// frame simply not carrying interface columns.
	t.Run("an FHRP-assigned IP keeps its address context and leaves interface_* empty", func(t *testing.T) {
		fields := []string{"ip", "match_count", "address_dns_name", "interface_name", "interface_description", "device_name"}
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.6"}, fields, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		byIP := make(map[string]map[string]interface{}, len(res.Rows))
		for _, r := range res.Rows {
			byIP[fmt.Sprint(r["ip"])] = r
		}

		// Control: interface context provably attaches in this very call.
		if byIP["10.0.0.1"]["interface_name"] != "Ethernet1" {
			t.Fatalf("interface_name = %v for the interface-assigned IP; the FHRP assertion below proves nothing without this",
				byIP["10.0.0.1"]["interface_name"])
		}

		fhrp := byIP["10.0.0.6"]
		if fhrp == nil {
			t.Fatalf("no row for the FHRP-assigned IP, got %v", res.Rows)
		}
		if fhrp["interface_name"] != nil {
			t.Errorf("interface_name = %v, want nil — an FHRP group is not an interface", fhrp["interface_name"])
		}
		if fhrp["interface_description"] != nil {
			t.Errorf("interface_description = %v, want nil — that is the FHRP group's description", fhrp["interface_description"])
		}
		// The row is not blanked wholesale: the address record exists and
		// everything it says about the ADDRESS is still true and still shown.
		if fhrp["match_count"] != float64(1) {
			t.Errorf("match_count = %v, want 1 — the address record is real", fhrp["match_count"])
		}
		if fhrp["address_dns_name"] != "vip.example.net" {
			t.Errorf("address_dns_name = %v, want %q", fhrp["address_dns_name"], "vip.example.net")
		}
	})

	t.Run("ip is force-included in columns and rows when the caller's field list omits it", func(t *testing.T) {
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, []string{"device_name"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Columns) == 0 || res.Columns[0] != "ip" {
			t.Errorf("Columns = %v, want \"ip\" present (and leading)", res.Columns)
		}
		if len(res.Rows) != 1 || res.Rows[0]["ip"] != "10.0.0.1" {
			t.Errorf("row missing ip: %v", res.Rows[0])
		}
	})

	// The negative half of the degradation contract, and the one that keeps the
	// warning worth reading: a query where every hop answered must carry no
	// warning at all. A mechanism that cries wolf on healthy results is no better
	// than the silence it replaced. Both IPs here have address fixtures (so no
	// prefix fallback runs) and the device hop resolves.
	t.Run("a fully successful resolution reports no warnings", func(t *testing.T) {
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.2"},
			[]string{"ip", "device_name"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none when nothing degraded", res.Warnings)
		}
	})
}

// TestResolveIPs_DeviceHopFailureIsStated is the finding this test file exists
// to close. The spec's error handling reads: "Emit rows with address and
// interface columns populated and device columns blank, plus a warning notice
// naming the degradation. Do not fail the query — partial context beats none,
// provided the gap is stated." Both halves are asserted here; only the first
// half used to hold, and a blank device_name is precisely what an IP with no
// device looks like, so the silent version told the dashboard author the
// opposite of the truth.
func TestResolveIPs_DeviceHopFailureIsStated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":1,"address":"10.0.0.1/32","dns_name":"leaf-01.example.net","status":{"value":"active"},
			 "assigned_object_type":"dcim.interface",
			 "assigned_object":{"id":9,"name":"Ethernet1","description":"uplink","device":{"id":100,"name":"leaf-01"}}}
		]}`))
	})
	// The whole device hop fails. 503, not 500, so the assertion below proves the
	// status is read from the response rather than hardcoded.
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"upstream unavailable"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"},
		[]string{"ip", "address_dns_name", "interface_name", "device_name", "device_site"}, 0)

	// Half one: the query does not fail, and address+interface context survives.
	if err != nil {
		t.Fatalf("a device-hop failure must not fail the query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if row["address_dns_name"] != "leaf-01.example.net" {
		t.Errorf("address_dns_name = %v, want the address hop's value to survive", row["address_dns_name"])
	}
	if row["interface_name"] != "Ethernet1" {
		t.Errorf("interface_name = %v, want the interface context to survive", row["interface_name"])
	}
	if row["device_name"] != nil || row["device_site"] != nil {
		t.Errorf("device columns = %v/%v, want blank", row["device_name"], row["device_site"])
	}

	// Half two: the gap is stated, and stated well enough to act on.
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %#v, want exactly one naming the device-hop failure", res.Warnings)
	}
	w := res.Warnings[0]
	for _, want := range []string{"device", "device_*", "503", "not because"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q must contain %q — a dashboard author needs to know blank device_* is a fetch failure, not absent data", w, want)
		}
	}
	// The reason must not carry NetBox's raw body or the ~6 KB batched request
	// URL into a panel notice; only the status travels.
	for _, leak := range []string{"upstream unavailable", srv.URL} {
		if strings.Contains(w, leak) {
			t.Errorf("warning %q leaks upstream detail (%q) into a user-facing string", w, leak)
		}
	}
}

// TestResolveIPs_AddressChunkFailureIsStated is the same contract for the
// chunk-degradation that landed in a535845: a batch that fails leaves its IPs
// with no address record, which is byte-for-byte what an unregistered IP looks
// like. Partial success was already tolerated; this locks in that it is also
// reported.
func TestResolveIPs_AddressChunkFailureIsStated(t *testing.T) {
	ips := make([]string, 0, 1200)
	for i := 0; i < 1200; i++ {
		ips = append(ips, fmt.Sprintf("10.%d.%d.7", i/256, i%256))
	}
	chunks := addressChunks(ips)
	if len(chunks) < 3 {
		t.Fatalf("test needs >=3 chunks to distinguish partial from total failure, got %d", len(chunks))
	}

	call := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
			return
		}
		var results []string
		for _, a := range r.URL.Query()["address"] {
			results = append(results, fmt.Sprintf(
				`{"id":%d,"address":"%s/24","status":{"value":"active"},"assigned_object":null}`,
				call*100000+len(results), a))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	// Registered so a stray fallback request would succeed rather than 404 into
	// a second warning, which would make the "exactly one warning" assertion
	// below pass for the wrong reason. The failed chunk's IPs must NOT reach it
	// at all — that is TestResolveIPs_FailedChunkIsNotFallbackFodder's subject.
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
	res, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "address_dns_name"}, len(ips))
	if err != nil {
		t.Fatalf("a partial failure must not fail the query: %v", err)
	}
	if len(res.Rows) != len(ips) {
		t.Fatalf("got %d rows, want one per IP (%d)", len(res.Rows), len(ips))
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %#v, want exactly one naming the address-hop failure", res.Warnings)
	}
	w := res.Warnings[0]
	for _, want := range []string{"address", "500", "not because"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q must contain %q", w, want)
		}
	}
	// The count must be the affected IPs, not the number of failed batches:
	// "1 of 3" would read as "one IP", understating the gap by two orders of
	// magnitude.
	if !strings.Contains(w, humanInt(len(chunks[1]))) {
		t.Errorf("warning %q must name how many IPs degraded (%d), not how many batches failed",
			w, len(chunks[1]))
	}
}

// TestResolveIPs_FailedChunkIsNotFallbackFodder is the regression test for the
// worst output this feature produced. An IP whose address chunk failed used to
// get match_count 0 AND a longest-matching prefix — the exact rendering of an IP
// that NetBox has genuinely never heard of — for a registered, interface-assigned
// device address. Reproduced live against the demo stack: 10.20.0.1, the primary
// IP of AMS1-leaf-01, came back as `match_count 0, prefix_cidr 10.0.0.0/8` and
// nothing else, byte-identical to 10.10.10.50, which really does lack a record.
//
// Both halves are asserted because either alone still lies: a 0 match_count is a
// positive numeric claim ("no record exists") emitted as float64 so users can
// threshold on it, and a populated prefix_* set looks exactly like a successful
// fallback.
func TestResolveIPs_FailedChunkIsNotFallbackFodder(t *testing.T) {
	// Two chunks: the first fails, the second answers. Everything about the
	// second chunk's rows must be unaffected.
	ips := make([]string, 0, 700)
	for i := 0; i < 700; i++ {
		ips = append(ips, fmt.Sprintf("10.%d.%d.7", i/256, i%256))
	}
	chunks := addressChunks(ips)
	if len(chunks) < 2 {
		t.Fatalf("test needs >=2 chunks, got %d", len(chunks))
	}
	failedIP, okIP := chunks[0][0], chunks[1][0]

	call := 0
	// Guarded: the prefix fallback issues its requests concurrently
	// (runPrefixFallback), so the handler that records them is called from
	// several goroutines at once.
	var prefixMu sync.Mutex
	var prefixAsked []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 1 {
			// 500, not 503. The subject here is what a chunk that could not be
			// answered does to the ROWS, so the failure has to be terminal:
			// fetchRows retries 502/503/504 (see getListPageRetry), and a
			// fail-the-first-call mock returning one of those would now be
			// answered on the retry and quietly stop testing anything.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
			return
		}
		var results []string
		for _, a := range r.URL.Query()["address"] {
			results = append(results, fmt.Sprintf(
				`{"id":%d,"address":"%s/24","status":{"value":"active"},"assigned_object":null}`,
				call*100000+len(results), a))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	// Answers with a real prefix, so a fallback that DID run would be visible as
	// a populated prefix_cidr rather than silently indistinguishable from one
	// that correctly did not.
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		prefixMu.Lock()
		prefixAsked = append(prefixAsked, r.URL.Query().Get("contains"))
		prefixMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[{"id":1,"prefix":"10.0.0.0/8"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
	res, err := p.ResolveIPs(context.Background(), ips,
		[]string{"ip", "match_count", "prefix_cidr"}, len(ips))
	if err != nil {
		t.Fatalf("a partial failure must not fail the query: %v", err)
	}

	byIP := map[string]map[string]interface{}{}
	for _, r := range res.Rows {
		byIP[r["ip"].(string)] = r
	}

	got := byIP[failedIP]
	if got == nil {
		t.Fatalf("no row for %s", failedIP)
	}
	if got["match_count"] != nil {
		t.Errorf("match_count for an IP whose lookup failed = %v, want nil — 0 asserts that NetBox holds no record, which is exactly what is unknown", got["match_count"])
	}
	if got["prefix_cidr"] != nil {
		t.Errorf("prefix_cidr for an IP whose lookup failed = %v, want nil — the fallback's precondition (no address record) was never established", got["prefix_cidr"])
	}
	prefixMu.Lock()
	askedIPs := append([]string(nil), prefixAsked...)
	prefixMu.Unlock()
	for _, asked := range askedIPs {
		if asked == failedIP {
			t.Errorf("the prefix fallback was queried for %s, whose address lookup failed", failedIP)
		}
	}

	// The surviving chunk is untouched: this is a per-IP gap, not a per-query one.
	if ok := byIP[okIP]; ok == nil || ok["match_count"] != float64(1) {
		t.Errorf("match_count for an IP in a SUCCESSFUL chunk = %v, want 1", byIP[okIP]["match_count"])
	}

	// And the gap is stated, naming the two columns that mislead.
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %#v, want exactly one naming the address-hop failure", res.Warnings)
	}
	for _, want := range []string{"match_count", "prefix_*", "not because"} {
		if !strings.Contains(res.Warnings[0], want) {
			t.Errorf("warning %q must name %q", res.Warnings[0], want)
		}
	}
}

// TestCanonicalIP covers the spelling collapse both the request and the index
// depend on. IPv4 and already-canonical input must be untouched — they are the
// overwhelming majority of real traffic and were never broken.
func TestCanonicalIP(t *testing.T) {
	cases := map[string]string{
		// unchanged: IPv4, canonical IPv6, and the masked forms of both
		"10.0.0.5":        "10.0.0.5",
		"10.0.0.5/24":     "10.0.0.5",
		"  10.0.0.5/32  ": "10.0.0.5",
		"2001:db8::1":     "2001:db8::1",
		"2001:db8::1/64":  "2001:db8::1",
		// uppercase
		"2001:DB8:85A3::8A2E:370:7334":    "2001:db8:85a3::8a2e:370:7334",
		"2001:DB8:85A3::8A2E:370:7334/64": "2001:db8:85a3::8a2e:370:7334",
		// zero-expanded
		"2001:0db8:85a3:0000:0000:8a2e:0370:7334": "2001:db8:85a3::8a2e:370:7334",
		// IPv4-mapped IPv6 denotes the same record NetBox stores as IPv4
		"::ffff:10.0.0.5": "10.0.0.5",
		// unparseable input keeps the old bare-slice behaviour
		"not-an-ip":    "not-an-ip",
		"leaf01.dc/24": "leaf01.dc",
		"":             "",
	}
	for in, want := range cases {
		if got := canonicalIP(in); got != want {
			t.Errorf("canonicalIP(%q) = %q, want %q", in, got, want)
		}
	}
}

// A zone is never part of an address NetBox stores (an inet carries none), so a
// zoned value can never match a record and is not an address here. Sending it
// cost nothing against NetBox (?address= answers 200 with count 0), but a
// list-valued host lookup refuses the whole list for one such value.
func TestCanonicalIPOK_RejectsZones(t *testing.T) {
	for _, in := range []string{"fe80::1%eth0", "fe80::1%eth0/64", "fe80::1%25eth0", " fe80::1%eth0 "} {
		if _, ok := canonicalIPOK(in); ok {
			t.Errorf("canonicalIPOK(%q) ok = true; a zoned value is not an address NetBox can hold", in)
		}
	}
	if c, ok := canonicalIPOK("fe80::1"); !ok || c != "fe80::1" {
		t.Errorf(`canonicalIPOK("fe80::1") = %q, %v; the same address without a zone is fine`, c, ok)
	}
	// An IPv4-mapped value denotes its IPv4 address, which carries no zone:
	// Unmap drops it, and the value resolves as it always did.
	if c, ok := canonicalIPOK("::ffff:10.1.2.9%eth0"); !ok || c != "10.1.2.9" {
		t.Errorf(`canonicalIPOK("::ffff:10.1.2.9%%eth0") = %q, %v; want 10.1.2.9, true`, c, ok)
	}
}

// The prefix fallback asks NetBox about the host the value names, keeping the
// answer every spelling that worked already got. NetBox's ?contains= has two
// rules: a bare value is strict (prefix >> address), a value with a mask is
// inclusive on the value's NETWORK (prefix >>= network). The caller's spelling
// went straight through, so "10.1.2.5/24" asked about the /24 network and missed
// the /30 holding the host, and "::ffff:10.1.2.5" matched no IPv4 prefix. Now a
// masked value asks about the host's own single-host prefix (10.1.2.5/32), which
// is what a /32 value always asked, and a bare one stays strict; values that are
// not addresses are not asked about at all.
func TestResolveIPs_PrefixFallbackAsksAboutTheHost(t *testing.T) {
	var mu sync.Mutex
	var addressed, contained []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		addressed = append(addressed, r.URL.Query()["address"]...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("contains")
		mu.Lock()
		contained = append(contained, c)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch c {
		case "10.1.2.5": // strict >>: the equal /32 is not among them
			_, _ = w.Write([]byte(`{"count":3,"next":null,"results":[{"id":1,"prefix":"10.0.0.0/8"},{"id":2,"prefix":"10.1.2.0/24"},{"id":3,"prefix":"10.1.2.4/30"}]}`))
		case "10.1.2.5/32": // >>= on the single-host network: every prefix holding the host
			_, _ = w.Write([]byte(`{"count":4,"next":null,"results":[{"id":1,"prefix":"10.0.0.0/8"},{"id":2,"prefix":"10.1.2.0/24"},{"id":3,"prefix":"10.1.2.4/30"},{"id":4,"prefix":"10.1.2.5/32"}]}`))
		case "10.1.2.5/24": // >>= on the network 10.1.2.0/24: the /30 is not among them
			_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[{"id":1,"prefix":"10.0.0.0/8"},{"id":2,"prefix":"10.1.2.0/24"}]}`))
		default:
			_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ips := []string{"10.1.2.5/24", "::ffff:10.1.2.5", "10.1.2.5/32", "10.1.2.5", "bogus", "fe80::1%eth0"}
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Rows) != len(ips) {
		t.Fatalf("got %d rows, want one per input", len(res.Rows))
	}
	for i, want := range []interface{}{"10.1.2.5/32", "10.1.2.4/30", "10.1.2.5/32", "10.1.2.4/30", nil, nil} {
		if got := res.Rows[i]["prefix_cidr"]; got != want {
			t.Errorf("row %q prefix_cidr = %v, want %v", ips[i], got, want)
		}
	}
	slices.Sort(contained)
	if want := []string{"10.1.2.5", "10.1.2.5", "10.1.2.5/32", "10.1.2.5/32"}; !slices.Equal(contained, want) {
		t.Errorf("?contains= asked %v, want %v: the host each value names, and nothing for a non-address", contained, want)
	}
	for _, a := range addressed {
		if strings.Contains(a, "%") {
			t.Errorf("?address=%q was sent; a zoned value is not an address", a)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v; values that are not addresses are absent data, not failures", res.Warnings)
	}
}

// A page that fails after the first is a failed lookup, stated as one: the
// row keeps blank prefix_* columns rather than the first page's answer, which
// could be a shorter prefix than the one the failed page held.
func TestResolveIPs_PrefixFallbackFailedPageIsStated(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":2,"next":%q,"results":[{"id":1,"prefix":"10.0.0.0/8"}]}`,
			srv.URL+"/api/ipam/prefixes/?contains=10.1.2.5&limit=100&offset=1")
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.1.2.5"}, []string{"ip", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if got := res.Rows[0]["prefix_cidr"]; got != nil {
		t.Errorf("prefix_cidr = %v, want blank: a half-read answer can be the wrong prefix", got)
	}
	if len(res.Warnings) == 0 {
		t.Error("a failed page must be stated as a failed prefix lookup")
	}
}

// A next link that never ends (a proxy looping, a server bug) cannot hold the
// request until the context expires: the walk stops after a bounded number of
// pages.
func TestResolveIPs_PrefixFallbackWalkIsBounded(t *testing.T) {
	var srv *httptest.Server
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":1,"next":%q,"results":[]}`, srv.URL+"/api/ipam/prefixes/?contains=10.1.2.5&limit=100&offset=1")
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := p.ResolveIPs(ctx, []string{"10.1.2.5"}, []string{"ip", "prefix_cidr"}, 0); err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the walk ran until the deadline; it must stop on its own")
	}
	if n := calls.Load(); n > int64(prefixPageCap) {
		t.Errorf("%d page requests, want at most %d", n, prefixPageCap)
	}
}

// NetBox orders containing prefixes VRF-first and shortest first, so with a
// hierarchy held in several VRFs the longest can sit on a later page. The
// fallback follows next instead of reading the first page alone.
func TestResolveIPs_PrefixFallbackReadsEveryPage(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("offset") == "" {
			_, _ = fmt.Fprintf(w, `{"count":2,"next":%q,"results":[{"id":1,"prefix":"10.0.0.0/8"}]}`,
				srv.URL+"/api/ipam/prefixes/?contains=10.1.2.5&limit=100&offset=1")
			return
		}
		_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[{"id":2,"prefix":"10.1.2.4/30"}]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.1.2.5"}, []string{"ip", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if got := res.Rows[0]["prefix_cidr"]; got != "10.1.2.4/30" {
		t.Errorf("prefix_cidr = %v, want 10.1.2.4/30 from the second page", got)
	}
}

// TestResolveIPs_NonCanonicalFormsResolve is the end-to-end proof for B3. The
// mock deliberately matches ?address= LITERALLY, exactly as NetBox's filter does
// — so a request that still sent the caller's spelling gets nothing back, and
// the test fails on the request side rather than passing by accident on a lenient
// stub. The records it returns are spelled NetBox's way, so a lookup key that
// still kept the caller's spelling fails on the index side too.
//
// Before this, all five non-canonical rows below came back completely blank,
// which docs/RECIPES.md teaches readers to interpret as external traffic.
func TestResolveIPs_NonCanonicalFormsResolve(t *testing.T) {
	// What NetBox actually holds, in NetBox's own spelling.
	stored := map[string]string{
		"2001:db8:85a3::8a2e:370:7334": `{"id":1,"address":"2001:db8:85a3::8a2e:370:7334/64","dns_name":"v6.example.net","status":{"value":"active"},"assigned_object":null}`,
		"10.20.0.1":                    `{"id":2,"address":"10.20.0.1/24","dns_name":"leaf01.example.net","status":{"value":"active"},"assigned_object":null}`,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			if fx, ok := stored[a]; ok { // literal match, like NetBox
				results = append(results, fx)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	// A containing prefix exists for every one of these, so a row that failed to
	// resolve would fall through and be visible as a prefix row rather than as
	// an ambiguous blank.
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[{"id":9,"prefix":"10.0.0.0/8"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct{ name, in, wantDNS string }{
		{"v6 canonical (unchanged)", "2001:db8:85a3::8a2e:370:7334", "v6.example.net"},
		{"v6 uppercase", "2001:DB8:85A3::8A2E:370:7334", "v6.example.net"},
		{"v6 uppercase + mask", "2001:DB8:85A3::8A2E:370:7334/64", "v6.example.net"},
		{"v6 zero-expanded", "2001:0db8:85a3:0000:0000:8a2e:0370:7334", "v6.example.net"},
		{"v6 expanded + mismatched mask", "2001:0DB8:85A3:0000:0000:8A2E:0370:7334/128", "v6.example.net"},
		{"v4 bare (unchanged)", "10.20.0.1", "leaf01.example.net"},
		{"v4 mask /32 vs stored /24", "10.20.0.1/32", "leaf01.example.net"},
		{"v4 mask /8 vs stored /24", "10.20.0.1/8", "leaf01.example.net"},
		{"v4 mask /25 vs stored /24", "10.20.0.1/25", "leaf01.example.net"},
	}

	// Each form is resolved ALONE. Sending them together would let one spelling
	// pull another's record into the shared by-host bucket, which is precisely
	// the batch-order dependence that made this bug intermittent: "10.20.0.1/32"
	// resolved only when "10.20.0.1" happened to be in the same query.
	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := p.ResolveIPs(context.Background(), []string{tc.in},
				[]string{"ip", "match_count", "address_dns_name", "prefix_cidr"}, 10)
			if err != nil {
				t.Fatalf("ResolveIPs: %v", err)
			}
			row := res.Rows[0]
			if row["address_dns_name"] != tc.wantDNS {
				t.Errorf("address_dns_name = %v, want %q — the address record was not found", row["address_dns_name"], tc.wantDNS)
			}
			if row["match_count"] != float64(1) {
				t.Errorf("match_count = %v, want 1", row["match_count"])
			}
			// A resolved IP must not also carry the prefix fallback.
			if row["prefix_cidr"] != nil {
				t.Errorf("prefix_cidr = %v, want nil for an IP with an address record", row["prefix_cidr"])
			}
			// The caller's own spelling is what the row is keyed by: it is the
			// documented Grafana join key, so rewriting it would break the join
			// against the user's flow data.
			if row["ip"] != tc.in {
				t.Errorf("ip = %v, want the caller's spelling %q", row["ip"], tc.in)
			}
		})
	}
}

// TestResolveIPs_AmbiguityIsNotedWithoutMatchCount: match_count is opt-in, so a
// user who deselects it sees one arbitrary-looking device for an anycast address
// with nothing to suggest a pick happened at all. The note carries that fact
// independently of the column.
func TestResolveIPs_AmbiguityIsNotedWithoutMatchCount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			if a == "10.0.0.2" { // anycast: three distinct records
				for id := 21; id <= 23; id++ {
					results = append(results, fmt.Sprintf(
						`{"id":%d,"address":"10.0.0.2/32","status":{"value":"active"},"assigned_object":null}`, id))
				}
				continue
			}
			results = append(results, fmt.Sprintf(
				`{"id":1,"address":"%s/32","status":{"value":"active"},"assigned_object":null}`, a))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})

	// match_count deliberately NOT requested.
	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.2"},
		[]string{"ip", "address_dns_name"}, 10)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Notes) != 1 {
		t.Fatalf("Notes = %#v, want one stating the ambiguous pick", res.Notes)
	}
	for _, want := range []string{"1 of 2", "match_count", "picked"} {
		if !strings.Contains(res.Notes[0], want) {
			t.Errorf("note %q must contain %q", res.Notes[0], want)
		}
	}
	// It is a NOTE, not a warning: the rows are complete and correct, and a
	// warning that fires on a routine correct result teaches users to ignore
	// warnings.
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %#v, want none — an ambiguous pick is not a degradation", res.Warnings)
	}

	t.Run("unambiguous results carry no note", func(t *testing.T) {
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, []string{"ip"}, 10)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if len(res.Notes) != 0 {
			t.Errorf("Notes = %#v, want none", res.Notes)
		}
	})
}

// TestResolveIPs_PrefixFallbackFailureIsStated covers the third hop. The
// fallback's error was discarded at the same altitude as the device hop's, with
// the same consequence: blank prefix_* columns that read as "no prefix contains
// this IP" when the truth is "we could not ask".
func TestResolveIPs_PrefixFallbackFailureIsStated(t *testing.T) {
	mux := http.NewServeMux()
	// No address record for either IP, so both go to the prefix fallback.
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	// Keyed on the IP, not on arrival order: runPrefixFallback issues these
	// requests concurrently, so "the first call" names whichever one won a race
	// and would make this test assert a different row on different runs. The
	// subject is per-ROW attribution — the IP whose lookup failed loses its
	// prefix columns and no other one does — which needs a fixed IP → outcome
	// mapping to be stated at all.
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("contains") == "10.0.0.1" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":7,"prefix":"10.0.0.0/24","scope":{"id":5,"name":"AMS1","slug":"ams1"}}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.2"},
		[]string{"ip", "prefix_cidr", "prefix_scope"}, 0)
	if err != nil {
		t.Fatalf("a prefix-fallback failure must not fail the query: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(res.Rows))
	}
	// The IP whose fallback succeeded still gets its prefix context.
	if res.Rows[1]["prefix_cidr"] != "10.0.0.0/24" {
		t.Errorf("second row prefix_cidr = %v, want the surviving lookup's value", res.Rows[1]["prefix_cidr"])
	}
	if res.Rows[0]["prefix_cidr"] != nil {
		t.Errorf("first row prefix_cidr = %v, want blank", res.Rows[0]["prefix_cidr"])
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %#v, want exactly one naming the prefix-hop failure", res.Warnings)
	}
	for _, want := range []string{"prefix_*", "1 of 2", "500", "not because"} {
		if !strings.Contains(res.Warnings[0], want) {
			t.Errorf("warning %q must contain %q", res.Warnings[0], want)
		}
	}
}

// TestResolveIPs_EmptyPrefixResultIsNotADegradation guards the distinction the
// whole mechanism rests on. An IP that genuinely sits in no prefix is ABSENCE,
// which blank columns already state correctly; warning about it would make the
// warning meaningless on the day it matters.
func TestResolveIPs_EmptyPrefixResultIsNotADegradation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"192.0.2.1"}, []string{"ip", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v; an IP with no containing prefix is absent data, not a failure", res.Warnings)
	}
}

// TestResolveIPs_MultipleHopFailuresAreListedSeparately checks that two
// degradations in one query produce two distinct warnings rather than the first
// masking the second — a dashboard author fixing only the hop they were told
// about would still be reading blank columns from the other.
func TestResolveIPs_MultipleHopFailuresAreListedSeparately(t *testing.T) {
	mux := http.NewServeMux()
	// 10.0.0.1 resolves to a device; 10.0.0.9 has no record and falls through.
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			if a == "10.0.0.1" {
				results = append(results, `{"id":1,"address":"10.0.0.1/32","status":{"value":"active"},`+
					`"assigned_object_type":"dcim.interface",`+
					`"assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}`)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.9"},
		[]string{"ip", "device_name", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 2 {
		t.Fatalf("Warnings = %#v, want two (device hop and prefix fallback)", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "device") {
		t.Errorf("first warning should be the device hop (hops report in pipeline order), got %q", res.Warnings[0])
	}
	if !strings.Contains(res.Warnings[1], "prefix") {
		t.Errorf("second warning should be the prefix fallback, got %q", res.Warnings[1])
	}
}

func TestCauseText(t *testing.T) {
	t.Run("an API error reports only the status", func(t *testing.T) {
		err := &APIError{Status: 503, URL: "http://nb/api/dcim/devices/?id=1&id=2", Body: `{"detail":"secret"}`}
		got := causeText(err)
		if got != "NetBox returned HTTP 503" {
			t.Errorf("causeText = %q", got)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "id=1") {
			t.Errorf("causeText %q must not carry the body or the batched URL into a panel notice", got)
		}
	})
	t.Run("a transport error gets a generic reason", func(t *testing.T) {
		if got := causeText(errors.New("dial tcp: connection refused")); got != "NetBox could not be reached" {
			t.Errorf("causeText = %q", got)
		}
	})
}

// TestDegradationScope pins the extent phrasing. "1 of 1 devices" reads as a
// formatting bug, which is a real cost on a notice whose whole job is to be
// believed.
func TestDegradationScope(t *testing.T) {
	cases := []struct {
		deg  degradation
		want string
	}{
		{degradation{failed: 1, total: 1}, "the only device"},
		{degradation{failed: 12, total: 12}, "all 12 devices"},
		{degradation{failed: 330, total: 1200}, "330 of 1,200 devices"},
	}
	for _, tc := range cases {
		if got := tc.deg.scope("device", "devices"); got != tc.want {
			t.Errorf("scope(%+v) = %q, want %q", tc.deg, got, tc.want)
		}
	}
}

// TestHopWarningTexts pins the four warnings' sentences verbatim. They are the whole
// deliverable of the degradation mechanism — the only thing standing between a
// blank device_* column and a dashboard author concluding the IP has no device —
// so they get asserted as text rather than by keyword, where a rewrite that
// dropped the "not because" clause would still pass.
//
// Each one names the columns that go blank, and is_primary_ip was added to two of
// those lists when it stopped being a device_* column: it is filled by the device
// hop for one row and by the VM hop for the next, so a device failure blanks it
// just as surely as it blanks device_name, and the sentence has to say so.
//
// The device hop gets three cases rather than one because it is the only hop whose
// list depends on the selection — widening its gate made it runnable for either of
// two unrelated columns, so a fixed list names an absent column in one direction or
// the other. See deviceHopWarning; the end-to-end half is
// TestResolveIPs_DeviceHopWarningNamesOnlySelectedColumns.
func TestHopWarningTexts(t *testing.T) {
	apiErr := &APIError{Status: 503, URL: "http://nb/api/dcim/devices/?id=1", Body: `{"detail":"nope"}`}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "address hop, partial",
			got:  addressHopWarning(degradation{failed: 330, total: 1200, cause: apiErr}),
			// match_count and prefix_* are named alongside the three namespaces
			// the hop fills. They are the two that actively mislead: match_count
			// 0 asserts "NetBox holds no record" and a prefix_cidr looks like a
			// successful longest-match fallback, on an IP NetBox knows.
			want: "Address lookup failed for 330 of 1,200 IPs — NetBox returned HTTP 503. " +
				"The match_count, address_*, interface_*, device_*, vm_name, is_primary_ip and prefix_* columns on the affected rows are empty " +
				"because the lookup failed, not because NetBox has no record for those IPs.",
		},
		{
			// Both selected, which is the default selection's shape.
			name: "device hop, total",
			got:  deviceHopWarning(degradation{failed: 12, total: 12, cause: apiErr}, true, true),
			want: "Device lookup failed for all 12 devices — NetBox returned HTTP 503. " +
				"The device_* and is_primary_ip columns on the affected rows are blank because the lookup failed, " +
				"not because those IPs have no device.",
		},
		{
			// device_* without is_primary_ip. Naming is_primary_ip here would send
			// the reader looking for a column the frame does not contain.
			name: "device hop, device_* selected without is_primary_ip",
			got:  deviceHopWarning(degradation{failed: 12, total: 12, cause: apiErr}, true, false),
			want: "Device lookup failed for all 12 devices — NetBox returned HTTP 503. " +
				"The device_* columns on the affected rows are blank because the lookup failed, " +
				"not because those IPs have no device.",
		},
		{
			// The mirror case, and the one the widened gate created: is_primary_ip
			// keeps the device hop alive on its own, so the hop can fail for a
			// selection with no device_* column in it at all. The "not because" clause moves too —
			// with only this column in the frame, the misreading to rule out is "not
			// primary", not "no device".
			name: "device hop, is_primary_ip selected without device_*",
			got:  deviceHopWarning(degradation{failed: 1, total: 1, cause: apiErr}, false, true),
			want: "Device lookup failed for the only device — NetBox returned HTTP 503. " +
				"The is_primary_ip column on the affected rows is blank because the lookup failed, " +
				"not because those IPs are not their device's primary address.",
		},
		{
			name: "virtual machine hop, partial",
			got:  vmHopWarning(degradation{failed: 2, total: 9, cause: apiErr}),
			// "not because those IPs are not their virtual machine's primary
			// address" is the load-bearing half: a blank is_primary_ip is exactly
			// what a correct FHRP-assigned or unassigned row looks like, so the
			// column alone cannot distinguish a failed hop from an honest absence.
			want: "Virtual machine lookup failed for 2 of 9 virtual machines — NetBox returned HTTP 503. " +
				"The is_primary_ip column on the affected rows is blank because the lookup failed, " +
				"not because those IPs are not their virtual machine's primary address.",
		},
		{
			name: "prefix fallback, single IP",
			got:  prefixHopWarning(degradation{failed: 1, total: 1, cause: errors.New("dial tcp")}),
			want: "Prefix lookup failed for the only IP with no address record — NetBox could not be reached. " +
				"The prefix_* columns on the affected rows are blank because the lookup failed, " +
				"not because no prefix contains those IPs.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("warning text drifted:\n got: %s\nwant: %s", tc.got, tc.want)
			}
		})
	}
}

func TestHumanInt(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 12345: "12,345", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := humanInt(in); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestWantsGroup pins the hop-selection predicate, including the case that used
// to look like an exception and is now a genuine gap: is_primary_ip was
// device_is_primary_ip, so the prefix test alone kept the device hop alive for
// it. It no longer does — this test asserts that it does NOT, because a reader
// who assumes otherwise would delete the explicit name test in ResolveIPs and
// silently blank the column for the default selection. The end-to-end guard for
// that is TestResolveIPs_DeviceHopOnlyRunsWhenSelected.
func TestWantsGroup(t *testing.T) {
	cases := []struct {
		fields []string
		group  string
		want   bool
	}{
		{defaultIPEnrichFields, "prefix_", false}, // the finding: no prefix_* in the default selection
		{defaultIPEnrichFields, "device_", true},  // device_site and device_tenant, not is_primary_ip
		{[]string{"ip", "is_primary_ip"}, "device_", false},
		{[]string{"ip", "match_count"}, "device_", false},
		{[]string{"ip", "match_count"}, "prefix_", false},
		{[]string{"ip", "address_dns_name", "interface_name"}, "device_", false},
		{[]string{"ip", "prefix_cidr"}, "prefix_", true},
		{nil, "prefix_", false},
	}
	for _, tc := range cases {
		if got := wantsGroup(tc.fields, tc.group); got != tc.want {
			t.Errorf("wantsGroup(%v, %q) = %v, want %v", tc.fields, tc.group, got, tc.want)
		}
	}
}

// TestResolveIPs_PrefixFallbackOnlyRunsWhenSelected is the regression test for
// the most expensive waste in this path. The fallback is SERIAL — NetBox's
// ?contains= takes one value, so it cannot be batched — and no prefix_* column
// is in defaultIPEnrichFields at all, yet it ran for every IP with no address
// record and project() then discarded every column it filled. A default flow
// panel full of external addresses paid one round trip per IP for output the
// user never saw.
//
// The assertion counts REQUESTS rather than checking the output shape on
// purpose: the projected frame looks identical either way, which is exactly why
// this survived so long.
func TestResolveIPs_PrefixFallbackOnlyRunsWhenSelected(t *testing.T) {
	// Atomic: the fallback's requests run concurrently (runPrefixFallback), so
	// the counter this test is built on is written from several goroutines.
	var prefixCalls atomic.Int64
	mux := http.NewServeMux()
	// No IP has an address record, so every one of them reaches the fallback arm.
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		prefixCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":7,"prefix":"10.0.0.0/24","scope":{"id":5,"name":"AMS1","slug":"ams1"}}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	unknown := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}

	t.Run("the default field selection makes no prefix request at all", func(t *testing.T) {
		prefixCalls.Store(0)
		res, err := p.ResolveIPs(context.Background(), unknown, nil, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if n := prefixCalls.Load(); n != 0 {
			t.Errorf("prefix requests = %d, want 0: no prefix_* column is in the default selection, so every one of them is a round trip whose output project() throws away", n)
		}
		if len(res.Rows) != len(unknown) {
			t.Fatalf("got %d rows, want %d — skipping the hop must not drop rows", len(res.Rows), len(unknown))
		}
		// match_count comes from the address index, not the prefix hop, so
		// skipping the hop must leave it exactly as it was: 0 means "NetBox
		// genuinely holds no record for this IP", which is still true here.
		for _, row := range res.Rows {
			if row["match_count"] != float64(0) {
				t.Errorf("match_count = %v (%T) for %v, want float64(0) — it is read off the address index and the prefix hop plays no part in it", row["match_count"], row["match_count"], row["ip"])
			}
		}
	})

	t.Run("a non-prefix explicit selection makes no prefix request either", func(t *testing.T) {
		prefixCalls.Store(0)
		if _, err := p.ResolveIPs(context.Background(), unknown,
			[]string{"ip", "match_count", "address_dns_name", "device_name"}, 0); err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if n := prefixCalls.Load(); n != 0 {
			t.Errorf("prefix requests = %d, want 0", n)
		}
	})

	t.Run("selecting a prefix_* column restores the fallback exactly", func(t *testing.T) {
		prefixCalls.Store(0)
		res, err := p.ResolveIPs(context.Background(), unknown, []string{"ip", "prefix_cidr", "prefix_scope"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if n := prefixCalls.Load(); n != int64(len(unknown)) {
			t.Errorf("prefix requests = %d, want %d — one per IP with no address record", n, len(unknown))
		}
		for _, row := range res.Rows {
			if row["prefix_cidr"] != "10.0.0.0/24" || row["prefix_scope"] != "AMS1" {
				t.Errorf("row %v = %v, want the containing prefix's columns", row["ip"], row)
			}
		}
	})
}

// TestResolveIPs_SkippedPrefixHopCannotWarn is the other half of finding 1. A
// hop that never ran has nothing to report, and a warning naming prefix_*
// columns the frame does not even contain is the same noise this branch spent
// several commits removing — worse here, because on an alerting path a Warning
// is a hard failure (see provider.Result.Warnings), so a broken prefix endpoint
// would break rules that never asked for a prefix column.
func TestResolveIPs_SkippedPrefixHopCannotWarn(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	// Every prefix lookup fails. If the hop runs, this is a guaranteed warning.
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, nil, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %#v, want none: no prefix_* column was requested, so no prefix_* column is blank", res.Warnings)
	}

	// Control: the same broken endpoint DOES warn once a prefix column is asked
	// for, so the assertion above cannot pass because warnings stopped working.
	res, err = p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, []string{"ip", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "prefix_*") {
		t.Errorf("Warnings = %#v, want one naming the prefix hop when prefix_cidr is selected", res.Warnings)
	}
}

// TestResolveIPs_DeviceHopOnlyRunsWhenSelected is finding 2: the batched device
// request was made whatever the caller selected. Wasted work for an address-only
// or prefix-only query, and worse than wasted when the token lacks DCIM
// permission or the endpoint is down — an otherwise complete result then carried
// a warning about blank device_* columns that were not in the output.
func TestResolveIPs_DeviceHopOnlyRunsWhenSelected(t *testing.T) {
	deviceCalls := 0
	deviceFields := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":1,"address":"10.0.0.1/32","dns_name":"leaf01.example.net","status":{"value":"active"},
			 "assigned_object_type":"dcim.interface",
			 "assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}
		]}`))
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		deviceCalls++
		deviceFields = r.URL.Query().Get("fields")
		w.Header().Set("Content-Type", "application/json")
		// Answered the way NetBox answers a ?fields= query — only the projected
		// keys — so a hop that projects when device_* was selected shows up here as
		// blank device columns rather than passing on a mock's generosity.
		if deviceFields != "" {
			_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
				{"id":100,"primary_ip4":{"id":1,"address":"10.0.0.1/32"},"primary_ip6":null}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":100,"name":"leaf-01","site":{"id":5,"name":"AMS1","slug":"ams1"},
			 "status":{"value":"active","label":"Active"},"primary_ip4":{"id":1,"address":"10.0.0.1/32"}}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	t.Run("an address/interface-only selection makes no device request", func(t *testing.T) {
		deviceCalls = 0
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"},
			[]string{"ip", "match_count", "address_dns_name", "interface_name"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if deviceCalls != 0 {
			t.Errorf("device requests = %d, want 0: no device_* column was selected, so the hop's whole output would be discarded", deviceCalls)
		}
		// The columns that were selected are unaffected by the skip.
		if res.Rows[0]["interface_name"] != "Ethernet1" || res.Rows[0]["address_dns_name"] != "leaf01.example.net" {
			t.Errorf("row = %v, want address_* and interface_* still populated", res.Rows[0])
		}
	})

	t.Run("a prefix-only selection makes no device request", func(t *testing.T) {
		deviceCalls = 0
		if _, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, []string{"ip", "prefix_cidr"}, 0); err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if deviceCalls != 0 {
			t.Errorf("device requests = %d, want 0", deviceCalls)
		}
	})

	t.Run("selecting device_name restores the hop, unprojected", func(t *testing.T) {
		deviceCalls, deviceFields = 0, ""
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, []string{"ip", "device_name", "device_site"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if deviceCalls != 1 {
			t.Errorf("device requests = %d, want 1", deviceCalls)
		}
		// device_* is nine columns spread across the serializer, so this query
		// wants the whole of it — and ?fields= is silent about a name it does not
		// recognize, so a projection here would blank a column with a 200.
		if deviceFields != "" {
			t.Errorf("device projection = %q, want none when device_* columns are selected", deviceFields)
		}
		if res.Rows[0]["device_name"] != "leaf-01" || res.Rows[0]["device_site"] != "AMS1" {
			t.Errorf("row = %v, want device context", res.Rows[0])
		}
	})

	// is_primary_ip is the trap, and losing its device_ prefix sharpened it: the
	// column keeps the device hop alive on its own while no longer carrying the
	// prefix the gate used to find it by. It is also in the default selection, so
	// a gate that tests prefixes alone blanks it on every default query — with no error, no
	// warning, and a column that simply reads "not primary" for the entire fleet.
	t.Run("is_primary_ip alone still keeps the device hop alive, projected", func(t *testing.T) {
		deviceCalls, deviceFields = 0, ""
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, []string{"ip", "is_primary_ip"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if deviceCalls != 1 {
			t.Fatalf("device requests = %d, want 1: is_primary_ip needs the device hop", deviceCalls)
		}
		// The hop is running for one boolean, so it asks for the two properties
		// that produce it (plus the id that keys the map) rather than the whole
		// device serializer: 6,378 bytes against 731 for three ids, measured
		// against NetBox 4.4.10. Asserted through ResolveIPs and not only through
		// fetchDevices because the value passed is the GATE's own boolean —
		// handing it wantDevice instead of wantDeviceGroup would fetch everything
		// again with every unit test still green.
		if deviceFields != "id,primary_ip4,primary_ip6" {
			t.Errorf("device projection = %q, want id,primary_ip4,primary_ip6: no device_* column was selected", deviceFields)
		}
		if res.Rows[0]["is_primary_ip"] != true {
			t.Errorf("is_primary_ip = %v, want true", res.Rows[0]["is_primary_ip"])
		}
	})

	t.Run("the default selection still makes the device request, unprojected", func(t *testing.T) {
		deviceCalls, deviceFields = 0, ""
		res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, nil, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if deviceCalls != 1 {
			t.Errorf("device requests = %d, want 1: the default selection contains device_* columns", deviceCalls)
		}
		// The default selects both device_* columns and is_primary_ip, so the full
		// serializer is the right ask and both kinds of column come back.
		if deviceFields != "" {
			t.Errorf("device projection = %q, want none: the default selection contains device_* columns", deviceFields)
		}
		if res.Rows[0]["device_site"] != "AMS1" || res.Rows[0]["is_primary_ip"] != true {
			t.Errorf("row = %v, want both device_* context and the flag", res.Rows[0])
		}
	})
}

// TestResolveIPs_SkippedDeviceHopCannotWarn is the device twin of the prefix
// case: a broken (or forbidden) dcim/devices endpoint must not put a warning on
// a result that has no device_* column to be blank — which on an alerting path
// is the difference between a rule evaluating and a rule erroring out.
func TestResolveIPs_SkippedDeviceHopCannotWarn(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[
			{"id":1,"address":"10.0.0.1/32","dns_name":"leaf01.example.net","status":{"value":"active"},
			 "assigned_object_type":"dcim.interface",
			 "assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}
		]}`))
	})
	// The token cannot read DCIM. If the hop runs, this is a guaranteed warning.
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"You do not have permission to perform this action."}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"},
		[]string{"ip", "address_dns_name", "interface_name"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %#v, want none: no device_* column was requested, so no device_* column is blank", res.Warnings)
	}

	// Control: the same forbidden endpoint DOES warn once device_name is asked
	// for, so the assertion above cannot pass for the wrong reason.
	res, err = p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, []string{"ip", "device_name"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "device_*") {
		t.Errorf("Warnings = %#v, want one naming the device hop when device_name is selected", res.Warnings)
	}
}

// TestResolveIPs_DeviceHopWarningNamesOnlySelectedColumns is the end-to-end half
// of the deviceHopWarning fix, and reproduces the mismatch as a user sees it: the
// frame's columns and the warning's column list, side by side, from one call.
//
// It is asserted against the FRAME rather than against a string, because that is
// the actual contract — every column the warning names must be one the reader can
// find and see blank. The hop now runs for `device_* selected OR is_primary_ip
// selected`, so it can fail for a selection holding only one of the two, and a
// fixed list then names a column that is not in the frame at all. That
// text is what an on-call reader meets first: a warning is a hard failure on the
// alert path (pkg/plugin.degradationError), so the notice IS the incident.
func TestResolveIPs_DeviceHopWarningNamesOnlySelectedColumns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			if fx, ok := primaryIPFixtures[a]; ok {
				results = append(results, fx)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	// The token cannot read DCIM, so every selection that runs the hop degrades.
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"You do not have permission to perform this action."}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	cases := []struct {
		name    string
		fields  []string
		want    []string // substrings the warning must contain
		absent  []string // column names it must NOT claim are blank
		notWhat string   // the reading the "not because" clause has to rule out
	}{
		{
			name:    "is_primary_ip alone",
			fields:  []string{"ip", "is_primary_ip"},
			want:    []string{"The is_primary_ip column", "is blank"},
			absent:  []string{"device_*"},
			notWhat: "not because those IPs are not their device's primary address",
		},
		{
			name:    "device_* alone",
			fields:  []string{"ip", "device_name", "device_site"},
			want:    []string{"The device_* columns", "are blank"},
			absent:  []string{"is_primary_ip"},
			notWhat: "not because those IPs have no device",
		},
		{
			name:    "both",
			fields:  []string{"ip", "device_name", "is_primary_ip"},
			want:    []string{"The device_* and is_primary_ip columns", "are blank"},
			notWhat: "not because those IPs have no device",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 10.0.0.1 is device-assigned, so the device hop has an id to ask about
			// and the VM hop has none — one warning, from the hop under test.
			res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, tc.fields, 0)
			if err != nil {
				t.Fatalf("ResolveIPs: %v", err)
			}
			if len(res.Warnings) != 1 {
				t.Fatalf("Warnings = %#v, want exactly one from the forbidden device hop", res.Warnings)
			}
			w := res.Warnings[0]
			for _, want := range append(tc.want, tc.notWhat) {
				if !strings.Contains(w, want) {
					t.Errorf("warning %q must contain %q", w, want)
				}
			}
			// The load-bearing assertion: a named column the frame does not have
			// sends the reader hunting for a gap that is not there.
			for _, col := range tc.absent {
				if strings.Contains(w, col) {
					t.Errorf("warning %q names %q, which this query did not select — columns are %v", w, col, res.Columns)
				}
			}
			// And the columns it DOES name are in the frame, blank, which is the
			// property the whole sentence exists to explain.
			for _, f := range tc.fields {
				if f == "ip" {
					continue
				}
				if _, ok := res.Rows[0][f]; !ok {
					t.Errorf("column %q is missing from the row entirely: %v", f, res.Rows[0])
				}
				if res.Rows[0][f] != nil {
					t.Errorf("column %q = %v, want blank — the warning explains a blank", f, res.Rows[0][f])
				}
			}
		})
	}
}

// primaryIPFixtures is one address per assignment kind is_primary_ip has to
// answer for, keyed by the bare host NetBox's ?address= filter matches on.
// Shapes taken from live NetBox 4.4.10, including the FHRP one: assigned_object
// is present and has a display string, and carries no primary_* field of any
// kind because FHRPGroup has none.
var primaryIPFixtures = map[string]string{
	// Device-assigned, and device 100's primary_ip4 → true.
	"10.0.0.1": `{"id":1,"address":"10.0.0.1/32","status":{"value":"active"},"assigned_object_type":"dcim.interface","assigned_object":{"id":9,"name":"Ethernet1","device":{"id":100,"name":"leaf-01"}}}`,
	// Device-assigned on the SAME device, but not its primary → false.
	"10.0.0.2": `{"id":2,"address":"10.0.0.2/24","status":{"value":"active"},"assigned_object_type":"dcim.interface","assigned_object":{"id":10,"name":"Ethernet2","device":{"id":100,"name":"leaf-01"}}}`,
	// VM-assigned, and VM 5's primary_ip4 → true.
	"10.0.0.3": `{"id":3,"address":"10.0.0.3/24","status":{"value":"active"},"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":55,"name":"eth0","virtual_machine":{"id":5,"name":"vm-01"}}}`,
	// VM-assigned on the SAME VM, but not its primary → false.
	"10.0.0.4": `{"id":4,"address":"10.0.0.4/24","status":{"value":"active"},"assigned_object_type":"virtualization.vminterface","assigned_object":{"id":56,"name":"eth1","virtual_machine":{"id":5,"name":"vm-01"}}}`,
	// FHRP-assigned → no value at all, which is the deliberate departure.
	"10.0.0.5": `{"id":5,"address":"10.0.0.5/24","status":{"value":"active"},"assigned_object_type":"ipam.fhrpgroup","assigned_object_id":2,"assigned_object":{"id":2,"display":"web-vip VRRPv3: 993 (10.0.0.5/24)","protocol":"vrrp3","group_id":993}}`,
	// Assigned to nothing → no value.
	"10.0.0.6": `{"id":6,"address":"10.0.0.6/24","status":{"value":"active"},"assigned_object_type":null,"assigned_object":null}`,
	// 10.0.0.7 is deliberately absent: an IP NetBox holds no record for.
}

// primaryIPHopCounts records how often each hop was asked, and what projection
// the VM hop sent, so a test can assert on the SHAPE of the requests rather than
// only on the row it got back.
type primaryIPHopCounts struct {
	device, vm int
	vmFields   string
	vmIDs      []string
}

// primaryIPServer serves all three hops is_primary_ip can reach: the address
// hop, dcim/devices and virtualization/virtual-machines.
//
// Registering the VM endpoint is what makes the VM assertions falsifiable, for
// the same reason the device endpoint is registered in TestResolveIPs: a VM-hop
// failure degrades silently by design, so a mock that does not serve it returns
// a nil is_primary_ip for every VM row no matter what the logic does — passing
// the FHRP assertion and the "not primary" assertion for entirely the wrong
// reason.
func primaryIPServer(t *testing.T, calls *primaryIPHopCounts) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			if fx, ok := primaryIPFixtures[a]; ok {
				results = append(results, fx)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		calls.device++
		var results []string
		for _, id := range r.URL.Query()["id"] {
			if id == "100" {
				results = append(results, `{"id":100,"name":"leaf-01","site":{"id":5,"name":"AMS1","slug":"ams1"},"status":{"value":"active","label":"Active"},"primary_ip4":{"id":1,"address":"10.0.0.1/32"}}`)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	mux.HandleFunc("/api/virtualization/virtual-machines/", func(w http.ResponseWriter, r *http.Request) {
		calls.vm++
		calls.vmFields = r.URL.Query().Get("fields")
		calls.vmIDs = append(calls.vmIDs, r.URL.Query()["id"]...)
		var results []string
		for _, id := range r.URL.Query()["id"] {
			if id == "5" {
				// Exactly the projection the hop asks for: NetBox answers a
				// ?fields= query with those keys and nothing else, so a hop that
				// reads anything wider would break against the real API.
				results = append(results, `{"id":5,"primary_ip4":{"id":3,"address":"10.0.0.3/24"},"primary_ip6":null}`)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveIPs_IsPrimaryIP is the is_primary_ip matrix: one row per assignment
// kind the column can meet, asserted in a single call so the four outcomes are
// visibly produced by the same code path.
//
// The FHRP and unassigned rows are the reason this test exists. NetBox's
// FHRPGroup has no primary_* field of any kind — its whole property set is
// auth_key, auth_type, comments, created, custom_fields, description, display,
// display_url, group_id, id, ip_addresses, last_updated, name, protocol, tags,
// url (4.4.10 /api/schema/) — so there is no such thing as an FHRP group's
// primary address, and false would be a confident answer to a question NetBox
// does not ask. The flag was asked for on FHRP groups too; it is deliberately NOT
// extended to them, and `!= false` is asserted separately from `== nil` so that
// anyone who "completes" it gets a failure that says why.
func TestResolveIPs_IsPrimaryIP(t *testing.T) {
	var calls primaryIPHopCounts
	srv := primaryIPServer(t, &calls)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6", "10.0.0.7"}
	fields := []string{"ip", "is_primary_ip", "device_name", "vm_name", "address_assigned_object_type"}
	res, err := p.ResolveIPs(context.Background(), ips, fields, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("Warnings = %#v, want none: every hop answered", res.Warnings)
	}
	byIP := make(map[string]map[string]interface{}, len(res.Rows))
	for _, r := range res.Rows {
		byIP[fmt.Sprint(r["ip"])] = r
	}

	cases := []struct {
		ip   string
		want interface{}
		why  string
	}{
		{"10.0.0.1", true, "address 1 is device 100's primary_ip4"},
		{"10.0.0.2", false, "address 2 is on device 100, whose primary_ip4 is address 1"},
		{"10.0.0.3", true, "address 3 is VM 5's primary_ip4"},
		{"10.0.0.4", false, "address 4 is on VM 5, whose primary_ip4 is address 3"},
		{"10.0.0.5", nil, "an ipam.fhrpgroup has no primary address in NetBox at all — see isPrimaryIP"},
		{"10.0.0.6", nil, "an unassigned address has nothing that could elect it"},
		{"10.0.0.7", nil, "NetBox holds no record for this IP"},
	}
	for _, tc := range cases {
		row, ok := byIP[tc.ip]
		if !ok {
			t.Fatalf("no row for %s, got %v", tc.ip, res.Rows)
		}
		if got := row["is_primary_ip"]; got != tc.want {
			t.Errorf("is_primary_ip for %s = %v (%T), want %v — %s", tc.ip, got, got, tc.want, tc.why)
		}
	}

	// Stated separately from the nil assertions above so the failure message can
	// name the decision rather than a type mismatch. false says "this object has
	// a primary address and this is not it"; for these three rows there is no
	// such object, which is a different fact — and the one this implementation
	// keeps true by departing from what was asked.
	for _, ip := range []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"} {
		if byIP[ip]["is_primary_ip"] == false {
			t.Errorf("is_primary_ip for %s = false; it must be ABSENT. false asserts that this address's owner has a primary IP and that this is not it, which is untrue for an FHRP group (NetBox has no such field), for an unassigned address and for an IP NetBox does not hold. Do not 'complete' the flag by filling this in", ip)
		}
	}

	// Controls. Without these the matrix above would pass just as happily if the
	// two hops had quietly stopped resolving anything: nil is the answer this
	// test is most interested in, and a broken hop produces it everywhere.
	if byIP["10.0.0.1"]["device_name"] != "leaf-01" {
		t.Errorf("device_name = %v, want leaf-01 — device context must attach in this same call", byIP["10.0.0.1"]["device_name"])
	}
	if byIP["10.0.0.3"]["vm_name"] != "vm-01" {
		t.Errorf("vm_name = %v, want vm-01 — VM identity must attach in this same call", byIP["10.0.0.3"]["vm_name"])
	}
	// The column that DOES answer for an FHRP-assigned row, and the reason a
	// blank is_primary_ip is not a dead end for the reader.
	if got := byIP["10.0.0.5"]["address_assigned_object_type"]; got != "ipam.fhrpgroup" {
		t.Errorf("address_assigned_object_type = %v, want ipam.fhrpgroup", got)
	}

	// One batched request per hop, for the two owners that actually appear —
	// not one per row, and not one per address.
	if calls.device != 1 || calls.vm != 1 {
		t.Errorf("hop requests: device=%d vm=%d, want 1 each (batched by id)", calls.device, calls.vm)
	}
	if len(calls.vmIDs) != 1 || calls.vmIDs[0] != "5" {
		t.Errorf("VM hop asked for ids %v, want exactly [5]: two addresses share VM 5, and an id is looked up once", calls.vmIDs)
	}
	// The VM hop exists for one boolean, so it asks for the three properties
	// that produce it and nothing else. flattenObject derives primary_ip4_id
	// from the nested primary_ip4 object, which is what isPrimaryIP reads.
	if calls.vmFields != "id,primary_ip4,primary_ip6" {
		t.Errorf("VM hop projection = %q, want %q", calls.vmFields, "id,primary_ip4,primary_ip6")
	}
}

// TestResolveIPs_VMHopOnlyRunsWhenSelected is the VM twin of
// TestResolveIPs_DeviceHopOnlyRunsWhenSelected, and the reason the VM hop is
// gated on one column name rather than on the "vm_" prefix: vm_name rides along
// on the address payload for free, so making it imply a request would charge
// every default query for something it already has.
func TestResolveIPs_VMHopOnlyRunsWhenSelected(t *testing.T) {
	var calls primaryIPHopCounts
	srv := primaryIPServer(t, &calls)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	// A VM-assigned IP, so the hop has something to ask about whenever it runs.
	ips := []string{"10.0.0.3"}

	t.Run("vm_name alone makes no VM request", func(t *testing.T) {
		calls = primaryIPHopCounts{}
		res, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "vm_name", "interface_name"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if calls.vm != 0 {
			t.Errorf("VM requests = %d, want 0: vm_name is read off the address payload the query already holds", calls.vm)
		}
		if res.Rows[0]["vm_name"] != "vm-01" {
			t.Errorf("vm_name = %v, want vm-01 — skipping the hop must not cost the free column", res.Rows[0]["vm_name"])
		}
	})

	t.Run("a device-only selection makes no VM request", func(t *testing.T) {
		calls = primaryIPHopCounts{}
		if _, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "device_name", "device_site"}, 0); err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if calls.vm != 0 {
			t.Errorf("VM requests = %d, want 0: no selected column can be filled by a virtual machine", calls.vm)
		}
	})

	t.Run("is_primary_ip runs both identity hops and nothing else does", func(t *testing.T) {
		calls = primaryIPHopCounts{}
		res, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "is_primary_ip"}, 0)
		if err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if calls.vm != 1 {
			t.Fatalf("VM requests = %d, want 1: is_primary_ip is the only column the VM hop fills", calls.vm)
		}
		if res.Rows[0]["is_primary_ip"] != true {
			t.Errorf("is_primary_ip = %v, want true", res.Rows[0]["is_primary_ip"])
		}
	})

	t.Run("the default selection makes the VM request", func(t *testing.T) {
		calls = primaryIPHopCounts{}
		if _, err := p.ResolveIPs(context.Background(), ips, nil, 0); err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if calls.vm != 1 {
			t.Errorf("VM requests = %d, want 1: is_primary_ip is in the default selection", calls.vm)
		}
	})

	t.Run("a device-assigned IP costs no VM request even when is_primary_ip is selected", func(t *testing.T) {
		calls = primaryIPHopCounts{}
		if _, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1"}, []string{"ip", "is_primary_ip"}, 0); err != nil {
			t.Fatalf("ResolveIPs: %v", err)
		}
		if calls.vm != 0 {
			t.Errorf("VM requests = %d, want 0: no address in this query is assigned to a VM, and an empty id set must not become a request", calls.vm)
		}
	})
}

// TestResolveIPs_VMHopFailureDegrades holds the VM hop to the rule the device
// hop already follows: a failure — partial or total — never fails the query. The
// affected rows lose the one column it fills and keep everything else, and the
// gap is STATED, because a blank is_primary_ip is what a correct FHRP row looks
// like and would otherwise be read as one.
func TestResolveIPs_VMHopFailureDegrades(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		var results []string
		for _, a := range r.URL.Query()["address"] {
			if fx, ok := primaryIPFixtures[a]; ok {
				results = append(results, fx)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	// The token cannot read virtualization. If the hop runs, this is a guaranteed
	// degradation.
	mux.HandleFunc("/api/virtualization/virtual-machines/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"You do not have permission to perform this action."}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.3"},
		[]string{"ip", "is_primary_ip", "vm_name", "interface_name"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v — a VM-hop failure must degrade, never fail the query", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Rows))
	}
	if res.Rows[0]["vm_name"] != "vm-01" || res.Rows[0]["interface_name"] != "eth0" {
		t.Errorf("row = %v, want the address payload's columns intact", res.Rows[0])
	}
	if res.Rows[0]["is_primary_ip"] != nil {
		t.Errorf("is_primary_ip = %v, want nil: the hop that answers it failed", res.Rows[0]["is_primary_ip"])
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "is_primary_ip") {
		t.Errorf("Warnings = %#v, want one naming is_primary_ip: a blank there is indistinguishable from a correct FHRP row", res.Warnings)
	}

	// The other half of the rule, and the one an alerting path depends on: a hop
	// that never ran cannot warn, because a Warning is a hard failure for alert
	// evaluation (see provider.Result.Warnings) and this endpoint is broken for
	// every query, not just the ones that need it.
	res, err = p.ResolveIPs(context.Background(), []string{"10.0.0.3"},
		[]string{"ip", "vm_name", "interface_name"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %#v, want none: is_primary_ip was not selected, so no column of it is blank", res.Warnings)
	}
}

// TestResolveIPs_DeclaresColumnTypes is the provider half of finding 3.
// is_primary_ip is a boolean by schema, not by whatever this refresh's IP
// set happened to resolve, and the frame layer cannot know that by scanning
// values that are all null. The declaration is what carries it across the seam.
func TestResolveIPs_DeclaresColumnTypes(t *testing.T) {
	mux := http.NewServeMux()
	// Nothing resolves: every device_* value in the result will be nil, which is
	// the exact situation the declaration exists for.
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.ResolveIPs(context.Background(), []string{"8.8.8.8"}, nil, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if res.Rows[0]["is_primary_ip"] != nil {
		t.Fatalf("fixture broken: is_primary_ip = %v, want nil for an unresolved IP", res.Rows[0]["is_primary_ip"])
	}
	if got := res.ColumnTypes["is_primary_ip"]; got != provider.FieldTypeBoolean {
		t.Errorf("ColumnTypes[is_primary_ip] = %q, want %q", got, provider.FieldTypeBoolean)
	}
	if got := res.ColumnTypes["match_count"]; got != provider.FieldTypeNumber {
		t.Errorf("ColumnTypes[match_count] = %q, want %q", got, provider.FieldTypeNumber)
	}

	// Declarations describe the columns the frame actually has: a field list
	// without either of them declares nothing, so no consumer can be told about a
	// column that is not there.
	res, err = p.ResolveIPs(context.Background(), []string{"8.8.8.8"}, []string{"ip", "address_dns_name"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.ColumnTypes) != 0 {
		t.Errorf("ColumnTypes = %v, want none when no declared column is selected", res.ColumnTypes)
	}
}

// recordingAddressServer echoes an address record per requested ?address= and
// keeps every raw query string it was sent, so a test can assert on the SHAPE of
// the requests and not just their answers.
func recordingAddressServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	ids := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		var results []string
		for _, a := range r.URL.Query()["address"] {
			h := hostOf(a)
			id, ok := ids[h]
			if !ok {
				id = len(ids) + 1
				ids[h] = id
			}
			results = append(results, fmt.Sprintf(
				`{"id":%d,"address":"%s/24","status":{"value":"active"},`+
					`"assigned_object_type":"dcim.interface",`+
					`"assigned_object":{"id":%d,"display":"Ethernet%d"}}`, id, h, id, id))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(results), strings.Join(results, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &queries
}

// TestResolveIPs_MalformedValuesDoNotConsumeTheBatchBudget pins the reason
// malformed values are filtered out of the batch, which is NOT error isolation:
// NetBox answers an invalid ?address= with HTTP 200 and count 0, so nothing is
// poisoned by sending one. What sending one costs is the 6144-byte chunk budget
// — junk in an interpolated $flow_ips variable pushes legitimate addresses into
// an extra request, and nearer the ~8 KB server ceiling, for a match that cannot
// happen.
//
// The valid set here is sized to fill exactly one chunk, so the junk is the only
// thing that can force a second. The rows must come out identical either way:
// the malformed inputs still get a row, still report match_count 0, and still
// carry blank context columns.
func TestResolveIPs_MalformedValuesDoNotConsumeTheBatchBudget(t *testing.T) {
	fields := []string{"ip", "match_count", "interface_name"}

	// Fill one chunk to the brim with valid, distinct addresses. The budget the
	// ADDRESSES get is the whole-query budget less the ?limit= fetchList appends
	// downstream; the address hop sends no fixed parameters of its own, so that
	// is the only reservation between them and chunkBudgetBytes.
	var valid []string
	used := 0
	for a := 0; a < 256 && used >= 0; a++ {
		for b := 0; b < 256; b++ {
			ip := fmt.Sprintf("10.20.%d.%d", a, b)
			cost := len("address") + len(url.QueryEscape(ip)) + 2
			if used+cost > chunkBudgetBytes-pagingParamBytes {
				used = -1
				break
			}
			used += cost
			valid = append(valid, ip)
		}
	}
	if n := len(addressChunks(valid)); n != 1 {
		t.Fatalf("fixture: the valid set needs exactly 1 chunk, got %d", n)
	}

	// Junk from the classes verified live against NetBox 4.4.10 — every one of
	// them HTTP 200 with count 0. Spliced into the middle so the split, if it
	// happened, would fall between valid addresses.
	//
	// A nonsense MASK ("10.0.0.1/99") is deliberately not in this list. The mask
	// is not part of the identity NetBox matches on, so canonicalIP drops it and
	// sends the host — which is a real address that can really match. Filtering on
	// anything but the host would change an answer rather than save a byte.
	junk := []string{"not-an-ip", "999.999.999.999", "2001:zzzz::1", "10.0.0.999", "no.such.host", "%%%"}
	mid := len(valid) / 2
	mixed := append(append(append([]string{}, valid[:mid]...), junk...), valid[mid:]...)
	if n := len(addressChunks(mixed)); n != 2 {
		t.Fatalf("fixture: junk must overflow the chunk to prove anything, got %d chunk(s)", n)
	}

	srv, queries := recordingAddressServer(t)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})
	mixedRes, err := p.ResolveIPs(context.Background(), mixed, fields, MaxLimit)
	if err != nil {
		t.Fatalf("ResolveIPs(mixed): %v", err)
	}

	if len(*queries) != 1 {
		t.Errorf("requests = %d, want 1 — malformed values must not consume chunk budget", len(*queries))
	}
	for i, q := range *queries {
		for _, j := range junk {
			if strings.Contains(q, url.QueryEscape(j)) {
				t.Errorf("request %d carries the unmatchable value %q", i, j)
			}
		}
	}

	cleanSrv, cleanQueries := recordingAddressServer(t)
	cleanRes, err := New(cleanSrv.URL, "test-token", &http.Client{Timeout: 10 * time.Second}).
		ResolveIPs(context.Background(), valid, fields, MaxLimit)
	if err != nil {
		t.Fatalf("ResolveIPs(valid): %v", err)
	}
	if len(*queries) != len(*cleanQueries) {
		t.Errorf("mixed sent %d request(s), junk-free sent %d — they must cost the same",
			len(*queries), len(*cleanQueries))
	}

	// Byte-identical rows for every valid input, and a row for every input.
	if len(mixedRes.Rows) != len(mixed) {
		t.Fatalf("rows = %d, want %d — every input keeps its row", len(mixedRes.Rows), len(mixed))
	}
	byIP := map[string]map[string]interface{}{}
	for _, row := range mixedRes.Rows {
		byIP[fmt.Sprint(row["ip"])] = row
	}
	for _, row := range cleanRes.Rows {
		ip := fmt.Sprint(row["ip"])
		got, ok := byIP[ip]
		if !ok {
			t.Fatalf("%s lost its row when junk was present", ip)
		}
		for _, f := range fields {
			if got[f] != row[f] {
				t.Errorf("%s.%s = %v with junk, %v without", ip, f, got[f], row[f])
			}
		}
	}
	// The junk rows themselves are unchanged from what a sent-and-unmatched
	// value produced: present, counted at zero, and blank. Not a warning, not a
	// note — a malformed value is not an error condition.
	for _, j := range junk {
		row, ok := byIP[j]
		if !ok {
			t.Fatalf("%q lost its row", j)
		}
		if row["match_count"] != float64(0) {
			t.Errorf("%q match_count = %v, want 0", j, row["match_count"])
		}
		if row["interface_name"] != nil {
			t.Errorf("%q interface_name = %v, want nil", j, row["interface_name"])
		}
	}
	if len(mixedRes.Warnings) != 0 || len(mixedRes.Notes) != 0 {
		t.Errorf("malformed input must not raise a warning or note; got %v / %v",
			mixedRes.Warnings, mixedRes.Notes)
	}
}

// TestResolveIPs_PrefixFallbackIsConcurrentAndBounded is the reason the hop was
// changed at all. NetBox's ?contains= takes ONE value, so the fallback is one
// request per unmatched IP however it is scheduled; running them one after
// another made the wall clock the sum of every round trip — 8.87 s for 25 IPs
// against a large remote NetBox, ~6 minutes at the 1,000-IP default limit, which
// no dashboard waits for.
//
// The server side proves it directly rather than timing it: every handler blocks
// until prefixFallbackWorkers requests are in flight AT ONCE, which a serial
// caller can never satisfy (it would sit out the timeout on each one in turn).
// The same barrier proves the other half — the pool is BOUNDED. There are three
// times as many IPs as workers and the gate opens at the worker count, so an
// unbounded fan-out would show a peak of len(ips) here; the semaphore is what
// keeps it at 8, and 8 is deliberate: an unbounded 1,000-request burst is what
// makes a busy instance answer 502/503, and this hop does not retry.
func TestResolveIPs_PrefixFallbackIsConcurrentAndBounded(t *testing.T) {
	var ips []string
	for i := 0; i < prefixFallbackWorkers*3; i++ {
		ips = append(ips, fmt.Sprintf("10.0.0.%d", i+1))
	}

	var mu sync.Mutex
	inflight, peak := 0, 0
	gate := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		n := inflight
		mu.Unlock()
		if n >= prefixFallbackWorkers {
			once.Do(func() { close(gate) })
		}
		// The timeout is the serial escape hatch: without it a serial caller
		// would deadlock here instead of failing with a readable peak.
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
		}
		mu.Lock()
		inflight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[{"id":7,"prefix":"10.0.0.0/24"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
	res, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Rows) != len(ips) {
		t.Fatalf("got %d rows, want %d", len(res.Rows), len(ips))
	}
	mu.Lock()
	got := peak
	mu.Unlock()
	// The literal 1 is deliberate and is NOT prefixFallbackWorkers: the barrier
	// above opens at the worker count, so an assertion written only against the
	// constant still passes when the constant is 1 — i.e. it would not notice the
	// hop going back to serial, which is the whole subject of this test.
	if got <= 1 {
		t.Errorf("peak concurrent prefix requests = %d: the hop is serial again, so its wall clock is the sum of one round trip per unmatched IP", got)
	}
	if got != prefixFallbackWorkers {
		t.Errorf("peak concurrent prefix requests = %d, want exactly %d: above it the pool is unbounded and a 1,000-IP query becomes a 1,000-request burst a busy instance answers with 502/503",
			got, prefixFallbackWorkers)
	}
}

// TestResolveIPs_PrefixFallbackOrderIsInputOrder is the regression guard for the
// property the concurrency had to preserve: WHICH row gets WHICH prefix, and in
// what order the rows come back. Completion order is now arbitrary — the mock
// inverts it deliberately, answering the first IP slowest — and the frame must
// not notice. A row that took its neighbour's prefix would be silently, plausibly
// wrong: prefix_cidr is a real-looking value either way.
func TestResolveIPs_PrefixFallbackOrderIsInputOrder(t *testing.T) {
	ips := []string{"10.0.1.1", "10.0.2.1", "10.0.3.1", "10.0.4.1", "10.0.5.1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		ip := r.URL.Query().Get("contains")
		// Delay inversely proportional to input position, so the completion
		// order is the exact reverse of the order the rows must come back in.
		for i, in := range ips {
			if in == ip {
				time.Sleep(time.Duration(len(ips)-i) * 20 * time.Millisecond)
			}
		}
		octet := strings.Split(ip, ".")[2]
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":1,"next":null,"results":[{"id":1,"prefix":"10.0.%s.0/24","description":"pfx-%s"}]}`, octet, octet)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
	res, err := p.ResolveIPs(context.Background(), ips, []string{"ip", "prefix_cidr", "prefix_description"}, 0)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(res.Rows) != len(ips) {
		t.Fatalf("got %d rows, want %d", len(res.Rows), len(ips))
	}
	for i, ip := range ips {
		octet := strings.Split(ip, ".")[2]
		if res.Rows[i]["ip"] != ip {
			t.Fatalf("row %d ip = %v, want %s — rows must stay in input order", i, res.Rows[i]["ip"], ip)
		}
		if want := "10.0." + octet + ".0/24"; res.Rows[i]["prefix_cidr"] != want {
			t.Errorf("row %d prefix_cidr = %v, want %s — each row must keep its OWN lookup's answer", i, res.Rows[i]["prefix_cidr"], want)
		}
		if want := "pfx-" + octet; res.Rows[i]["prefix_description"] != want {
			t.Errorf("row %d prefix_description = %v, want %s", i, res.Rows[i]["prefix_description"], want)
		}
	}
}

// TestResolveIPs_PrefixFallbackCauseFollowsInputOrder pins the one part of the
// degradation report that concurrency could have made non-deterministic. The
// warning states ONE reason (degradation.noteCause keeps the first), and when
// the requests ran in sequence "first" meant the earliest failing IP in the
// caller's order. Folding the outcomes in completion order instead would make
// two identical queries over identical data render two different reasons — the
// mock here answers the earliest IP LAST, so a completion-order fold reports
// 502 where the serial code reported 500.
func TestResolveIPs_PrefixFallbackCauseFollowsInputOrder(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("contains") == "10.0.0.1" {
			time.Sleep(150 * time.Millisecond) // the first IP finishes last
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "test-token", &http.Client{Timeout: 30 * time.Second})
	res, err := p.ResolveIPs(context.Background(), []string{"10.0.0.1", "10.0.0.2"},
		[]string{"ip", "prefix_cidr"}, 0)
	if err != nil {
		t.Fatalf("a prefix-fallback failure must not fail the query: %v", err)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %#v, want exactly one", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "HTTP 500") {
		t.Errorf("warning %q must name HTTP 500 — the cause of the FIRST failing IP in input order, not of whichever request finished first", res.Warnings[0])
	}
	if !strings.Contains(res.Warnings[0], "all 2 IPs") {
		t.Errorf("warning %q must count both failures", res.Warnings[0])
	}
}

// scopeServer serves ipam/ip-addresses under a parent filter, plus the device
// hop, the way a NetBox does. It records the query it was asked so a test can
// assert the filters reached NetBox as-is.
func scopeServer(t *testing.T, records string, count int) (*httptest.Server, *url.Values) {
	t.Helper()
	var seen url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, count, records)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[{"id":7,"name":"leaf1","primary_ip4":{"id":1,"address":"10.0.0.1/24"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

const scopeRecords = `
	{"id":1,"address":"10.0.0.1/24","status":{"value":"active"},"assigned_object_type":"dcim.interface","assigned_object_id":11,"assigned_object":{"id":11,"name":"eth0","device":{"id":7,"name":"leaf1"}}},
	{"id":2,"address":"10.0.0.2/24","status":{"value":"active"},"assigned_object_type":null,"assigned_object_id":null,"assigned_object":null},
	{"id":3,"address":"10.0.0.2/24","status":{"value":"active"},"assigned_object_type":null,"assigned_object_id":null,"assigned_object":null,"vrf":{"id":5,"name":"blue"}}`

func TestResolveScope_OneRowPerHostFromTheFilteredListing(t *testing.T) {
	srv, seen := scopeServer(t, scopeRecords, 3)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.ResolveScope(context.Background(),
		[]provider.Filter{{Field: "parent", Value: "10.0.0.0/24"}},
		[]string{"ip", "match_count", "device_name", "is_primary_ip"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The filter rows reach NetBox as the same parameters an objects query sends.
	if got := (*seen).Get("parent"); got != "10.0.0.0/24" {
		t.Errorf("NetBox saw parent=%q, want 10.0.0.0/24", got)
	}
	// Three records, two distinct hosts: one row each, in listing order.
	if got := ipsOf(res); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Errorf("rows = %v, want one per distinct host in listing order", got)
	}
	if res.Rows[1]["match_count"] != float64(2) {
		t.Errorf("10.0.0.2 is held twice in the scope (two VRFs); match_count = %v, want 2", res.Rows[1]["match_count"])
	}
	// The device hop ran exactly as it does for a list query.
	if res.Rows[0]["device_name"] != "leaf1" || res.Rows[0]["is_primary_ip"] != true {
		t.Errorf("row 0 = %v, want device_name leaf1 and is_primary_ip true", res.Rows[0])
	}
	// Complete listing: Total is the number of rows, so nothing reads as truncated.
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2 (distinct hosts) for a complete listing", res.Total)
	}
	if !slices.Equal(res.Columns, []string{"ip", "match_count", "device_name", "is_primary_ip"}) {
		t.Errorf("Columns = %v", res.Columns)
	}
}

func TestResolveScope_CutOffListingReadsAsTruncated(t *testing.T) {
	// NetBox reports 5,000 matches but the page (limit 2) carries two records.
	srv, _ := scopeServer(t, scopeRecords[:strings.LastIndex(scopeRecords, ",\n")], 5000)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.ResolveScope(context.Background(), []provider.Filter{{Field: "parent", Value: "10.0.0.0/8"}}, []string{"ip"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(res.Rows))
	}
	// The distinct-host count of the whole scope is unknowable from a cut-off
	// page, so Total is NetBox's record count: strictly more than the rows, which
	// is what makes a strict consumer refuse it.
	if res.Total != 5000 {
		t.Errorf("Total = %d, want 5000 (the reported count) for a cut-off listing", res.Total)
	}
	if res.MaxRows != MaxLimit {
		t.Errorf("MaxRows = %d, want %d", res.MaxRows, MaxLimit)
	}
}

// pagingScopeServer serves n synthetic addresses, honouring limit= and offset=
// the way NetBox pages, so the limit can be tested by what comes back rather
// than by the request parameter (fetchList sends min(limit, pageSize) per page,
// so the parameter alone says nothing about the clamp).
func pagingScopeServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		var recs []string
		for i := offset; i < n && len(recs) < limit; i++ {
			recs = append(recs, fmt.Sprintf(`{"id":%d,"address":"10.%d.%d.%d/32","assigned_object":null}`, i+1, (i>>16)&255, (i>>8)&255, i&255))
		}
		// fetchList follows the server's next link rather than computing offsets
		// itself, so the page after this one has to be named here.
		next := "null"
		if offset+limit < n {
			next = fmt.Sprintf("%q", fmt.Sprintf("http://%s/api/ipam/ip-addresses/?limit=%d&offset=%d", r.Host, limit, offset+limit))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":%s,"results":[%s]}`, n, next, strings.Join(recs, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveScope_DefaultsAndCaps(t *testing.T) {
	// More addresses than the default limit: an unset limit reads defaultLimit
	// records and reports the whole count, so the result reads as truncated.
	p := New(pagingScopeServer(t, defaultLimit+5).URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.ResolveScope(context.Background(), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != defaultLimit || res.Total != defaultLimit+5 {
		t.Errorf("limit 0: rows=%d total=%d, want %d and %d", len(res.Rows), res.Total, defaultLimit, defaultLimit+5)
	}
	// nil fields → the default column set, "ip" first, same as ResolveIPs.
	if res.Columns[0] != "ip" || len(res.Columns) != len(DefaultIPEnrichFields()) {
		t.Errorf("Columns = %v, want the default ip-enrichment set", res.Columns)
	}
	// An explicit limit below the page size is sent as-is.
	srv, seen := scopeServer(t, scopeRecords, 3)
	p = New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	if _, err := p.ResolveScope(context.Background(), nil, nil, 50); err != nil {
		t.Fatal(err)
	}
	if got := (*seen).Get("limit"); got != "50" {
		t.Errorf("limit 50: NetBox saw limit=%s", got)
	}
}

func TestResolveScope_ListingFailureIsAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})
	// Unlike a list query, where one failed batch degrades its own IPs, the scope
	// listing IS the row set: nothing can be answered without it.
	if _, err := p.ResolveScope(context.Background(), nil, []string{"ip"}, 10); err == nil {
		t.Fatal("a failed scope listing must be an error, not an empty table")
	}
}

func ipsOf(res *provider.Result) []string {
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, r["ip"].(string))
	}
	return out
}
