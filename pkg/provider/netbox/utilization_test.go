package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"errors"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"go4.org/netipx"
)

func TestIPSetSize(t *testing.T) {
	addr := netip.MustParseAddr
	// Duplicate host objects (e.g. HA/VIP) count once (NetBox uses an IPSet).
	hosts := []netip.Addr{addr("10.0.0.1"), addr("10.0.0.1"), addr("10.0.0.2")}
	if got := ipSetSize(hosts, nil); got != 2 {
		t.Errorf("dedup hosts: got %v want 2", got)
	}
	// A host inside a utilized range is not double-counted; ranges union with hosts.
	ranges := []netipx.IPRange{netipx.IPRangeFrom(addr("10.0.0.1"), addr("10.0.0.5"))} // 5 addrs
	if got := ipSetSize(hosts, ranges); got != 5 {
		t.Errorf("host+range union: got %v want 5", got)
	}
}

func TestComputeUtilization_LeafUtilizedRange(t *testing.T) {
	p := newTestProvider(t)
	// 10.5.0.0/24 has no individual IPs but one marked-utilized range of 10 addrs.
	raw := []byte(`{"prefix":"10.5.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"family":{"value":4},"vrf":null}`)
	used, avail, util, err := p.computeUtilization(context.Background(), &utilCost{}, "ipam/prefixes", raw)
	// size 254; used = 10 (range) -> floor(10/254*100) = 3.
	if err != nil || used != 10 || avail != 244 || util != 3 {
		t.Fatalf("leaf utilized range: used=%v avail=%v util=%v err=%v", used, avail, util, err)
	}
}

func TestPrefixUsableSize(t *testing.T) {
	cases := []struct {
		cidr   string
		isPool bool
		want   float64
	}{
		{"10.0.0.0/24", false, 254},                    // IPv4 /24 non-pool: 256-2
		{"10.0.0.0/24", true, 256},                     // pool: no exclusion
		{"10.0.0.0/31", false, 2},                      // /31: no exclusion
		{"10.0.0.1/32", false, 1},                      // /32: no exclusion
		{"2001:db8::/64", false, 18446744073709551616}, // 2^64, no exclusion
	}
	for _, c := range cases {
		pfx := netip.MustParsePrefix(c.cidr)
		if got := prefixUsableSize(pfx, c.isPool); got != c.want {
			t.Errorf("prefixUsableSize(%s,pool=%v)=%v want %v", c.cidr, c.isPool, got, c.want)
		}
	}
}

func TestRangeCIDRsAndUnion(t *testing.T) {
	// 10.0.0.1 .. 10.0.0.6 is not block-aligned -> multiple CIDRs covering 6 hosts.
	cidrs := rangeCIDRs(netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.6"))
	if got := unionPrefixSize(cidrs); got != 6 {
		t.Fatalf("union size = %v, want 6 (cidrs=%v)", got, cidrs)
	}
	// Overlapping prefixes must not double-count: /24 plus one of its /25s = 256.
	overlap := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24"), netip.MustParsePrefix("10.0.0.0/25")}
	if got := unionPrefixSize(overlap); got != 256 {
		t.Fatalf("union of /24 and /25 = %v, want 256", got)
	}
}

func TestComputeUtilization_LeafPrefix(t *testing.T) {
	p := newTestProvider(t) // mock returns count=3 for ip-addresses (see Step 3 mock)
	raw := []byte(`{"prefix":"10.0.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"family":{"value":4},"vrf":null}`)
	used, avail, util, err := p.computeUtilization(context.Background(), &utilCost{}, "ipam/prefixes", raw)
	if err != nil {
		t.Fatalf("computeUtilization: %v", err)
	}
	if used != 3 || avail != 251 || util != 1 { // 3/254*100 = 1.18 -> floor 1
		t.Fatalf("used=%v avail=%v util=%v; want 3/251/1", used, avail, util)
	}
}

func TestComputeUtilization_MarkUtilized(t *testing.T) {
	p := newTestProvider(t)
	raw := []byte(`{"prefix":"10.0.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":true,"family":{"value":4},"vrf":null}`)
	used, avail, util, err := p.computeUtilization(context.Background(), &utilCost{}, "ipam/prefixes", raw)
	if err != nil || util != 100 || used != 254 || avail != 0 {
		t.Fatalf("mark_utilized: used=%v avail=%v util=%v err=%v", used, avail, util, err)
	}
}

func TestQuery_UtilizationOptIn(t *testing.T) {
	p := newTestProvider(t) // prefixes mock returns 1 prefix; ip-addresses count=3
	// Requested -> columns present, value computed.
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Columns, "utilization") || !contains(res.Columns, "used") {
		t.Fatalf("columns missing util: %v", res.Columns)
	}
	if res.Rows[0]["utilization"] != float64(1) || res.Rows[0]["used"] != float64(3) {
		t.Fatalf("row util=%v used=%v", res.Rows[0]["utilization"], res.Rows[0]["used"])
	}
}

func TestQuery_UtilizationNotRequested(t *testing.T) {
	p := newTestProvider(t)
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contains(res.Columns, "utilization") {
		t.Fatalf("utilization present but not requested: %v", res.Columns)
	}
	if _, ok := res.Rows[0]["used"]; ok {
		t.Fatal("used present but not requested")
	}
}

func TestComputeUtilization_Container(t *testing.T) {
	p := newTestProvider(t)
	raw := []byte(`{"prefix":"10.0.0.0/23","status":{"value":"container"},"family":{"value":4},"vrf":null}`)
	used, _, util, err := p.computeUtilization(context.Background(), &utilCost{}, "ipam/prefixes", raw)
	// /23 size = 512; one child /24 = 256 -> 256/512 = 50%.
	if err != nil || used != 256 || util != 50 {
		t.Fatalf("container: used=%v util=%v err=%v", used, util, err)
	}
}

func TestComputeUtilization_ContainerMarkUtilized(t *testing.T) {
	p := newTestProvider(t)
	raw := []byte(`{"prefix":"10.0.0.0/23","status":{"value":"container"},"mark_utilized":true,"family":{"value":4},"vrf":null}`)
	// mark_utilized short-circuits the container branch: no "within" child-prefix
	// lookup happens, used=size (no network/broadcast deduction for containers).
	used, avail, util, err := p.computeUtilization(context.Background(), &utilCost{}, "ipam/prefixes", raw)
	if err != nil || used != 512 || avail != 0 || util != 100 {
		t.Fatalf("container mark_utilized: used=%v avail=%v util=%v err=%v", used, avail, util, err)
	}
}

func TestComputeUtilization_IPRange(t *testing.T) {
	p := newTestProvider(t)
	raw := []byte(`{"start_address":"10.9.0.1/24","end_address":"10.9.0.6/24","size":6,"mark_utilized":false,"vrf":null}`)
	// ip-addresses mock returns count=3 for every ?parent=; range covers 4 CIDR blocks
	// (10.9.0.1/32, 10.9.0.2/31, 10.9.0.4/31, 10.9.0.6/32) -> summed used is capped at size 6.
	used, avail, util, err := p.computeUtilization(context.Background(), &utilCost{}, "ipam/ip-ranges", raw)
	if err != nil || used != 6 || avail != 0 || util != 100 {
		t.Fatalf("range: used=%v avail=%v util=%v err=%v", used, avail, util, err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestFields_AdvertisesUtilization(t *testing.T) {
	p := newTestProvider(t)
	fields, err := p.Fields(context.Background(), "ipam/prefixes")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"utilization": false, "used": false, "available": false}
	for _, f := range fields {
		if _, ok := want[f.Name]; ok {
			if f.Type != provider.FieldTypeNumber {
				t.Errorf("%s type = %s, want number", f.Name, f.Type)
			}
			want[f.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("Fields(ipam/prefixes) missing %q", name)
		}
	}
}

// countingIPAM is a NetBox stand-in for the utilization child lookups that
// records every request it serves, so a test can assert on the REQUEST COUNT and
// not only on the numbers. The amplification these tests guard is invisible to a
// value assertion: a version that spends six requests and one that spends none
// return the same row.
type countingIPAM struct {
	mu       sync.Mutex
	requests []recordedRequest
}

// recordedRequest is one child lookup as the fake NetBox saw it.
type recordedRequest struct {
	path  string
	query url.Values
}

func (c *countingIPAM) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, recordedRequest{path: r.URL.Path, query: r.URL.Query()})
}

func (c *countingIPAM) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// newCountingProvider serves ip-addresses/ip-ranges/prefixes list endpoints,
// reporting perParent child IPs for each named `parent` block (modelling
// NetBox's multi-value, OR-ing `parent` filter) and no child ranges or prefixes.
func newCountingProvider(t *testing.T, perParent int) (*Provider, *countingIPAM) {
	t.Helper()
	c := &countingIPAM{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[]}`,
			perParent*len(r.URL.Query()["parent"]))
	})
	for _, path := range []string{"/api/ipam/ip-ranges/", "/api/ipam/prefixes/"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			c.record(r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second}), c
}

// TestCountContainedIPs_OneRequestPerRange pins the batching: the blocks tiling
// one IP range travel in a SINGLE request as repeated `parent=` values, and the
// total is what the per-block loop summed. 10.9.0.1-10.9.0.6 tiles into four
// blocks (/32, /31, /31, /32).
func TestCountContainedIPs_OneRequestPerRange(t *testing.T) {
	p, c := newCountingProvider(t, 3)
	blocks := rangeCIDRs(netip.MustParseAddr("10.9.0.1"), netip.MustParseAddr("10.9.0.6"))
	if len(blocks) != 4 {
		t.Fatalf("blocks = %v, want 4", blocks)
	}
	got, err := p.countContainedIPs(context.Background(), &utilCost{}, blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 12 { // 4 blocks x 3 = what one request per block summed to
		t.Errorf("count = %v, want 12", got)
	}
	if c.count() != 1 {
		t.Errorf("requests = %d, want 1 (was one per block)", c.count())
	}
	if parents := c.requests[0].query["parent"]; len(parents) != 4 {
		t.Errorf("parent values = %v, want all 4 blocks in one request", parents)
	}
}

// TestCountContainedIPs_ChunksLongTilings covers a range whose tiling exceeds
// maxParentsPerRequest: it splits across requests and the counts still sum,
// because the blocks are pairwise disjoint.
func TestCountContainedIPs_ChunksLongTilings(t *testing.T) {
	p, c := newCountingProvider(t, 1)
	// A range one address short of a full /64 tiles into 64 IPv6 blocks.
	blocks := rangeCIDRs(netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::ffff:ffff:ffff:ffff"))
	if len(blocks) <= maxParentsPerRequest {
		t.Fatalf("blocks = %d, want more than the per-request cap %d", len(blocks), maxParentsPerRequest)
	}
	got, err := p.countContainedIPs(context.Background(), &utilCost{}, blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != float64(len(blocks)) {
		t.Errorf("count = %v, want %d (one per block, summed across chunks)", got, len(blocks))
	}
	wantReqs := (len(blocks) + maxParentsPerRequest - 1) / maxParentsPerRequest
	if c.count() != wantReqs {
		t.Errorf("requests = %d, want %d", c.count(), wantReqs)
	}
}

// TestComputeUtilization_ZeroSizeRangeCostsNothing covers the `size <= 0`
// short-circuit on an IP range. NetBox stores `size` as a denormalized column,
// and bulk-loaded data leaves it 0; the clamp then forces (0, 0, 0) whatever the
// children are, so the lookups must not happen at all.
func TestComputeUtilization_ZeroSizeRangeCostsNothing(t *testing.T) {
	p, c := newCountingProvider(t, 3)
	raw := []byte(`{"start_address":"10.224.0.100/24","end_address":"10.224.0.200/24","size":0,"mark_utilized":false,"vrf":{"id":2}}`)
	used, avail, util, err := p.computeUtilization(context.Background(), &utilCost{}, "ipam/ip-ranges", raw)
	if err != nil || used != 0 || avail != 0 || util != 0 {
		t.Fatalf("zero-size range: used=%v avail=%v util=%v err=%v; want 0/0/0/nil", used, avail, util, err)
	}
	if c.count() != 0 {
		t.Errorf("requests = %d, want 0", c.count())
	}
}

// TestUtilizationChildLookups_ProjectFields pins that each child lookup asks
// NetBox to serialize only the property it reads. The saving is invisible in the
// values — that is the point — so nothing but an assertion on the request keeps
// it from being dropped by a later edit.
func TestUtilizationChildLookups_ProjectFields(t *testing.T) {
	cases := []struct {
		name       string
		objectType string
		raw        string
		want       map[string]string // endpoint path -> expected fields value
	}{
		{
			name:       "leaf prefix",
			objectType: "ipam/prefixes",
			raw:        `{"prefix":"10.0.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}`,
			want: map[string]string{
				"/api/ipam/ip-addresses/": "address",
				"/api/ipam/ip-ranges/":    "start_address,end_address",
			},
		},
		{
			name:       "container prefix",
			objectType: "ipam/prefixes",
			raw:        `{"prefix":"10.0.0.0/16","status":{"value":"container"},"mark_utilized":false,"vrf":null}`,
			want:       map[string]string{"/api/ipam/prefixes/": "prefix"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, c := newCountingProvider(t, 0)
			if _, _, _, err := p.computeUtilization(context.Background(), &utilCost{}, tc.objectType, []byte(tc.raw)); err != nil {
				t.Fatalf("computeUtilization: %v", err)
			}
			seen := map[string]string{}
			for _, q := range c.requests {
				seen[q.path] = q.query.Get(fieldsParam)
			}
			for path, want := range tc.want {
				if got := seen[path]; got != want {
					t.Errorf("%s fields = %q, want %q (all requests: %v)", path, got, want, c.requests)
				}
			}
		})
	}
}

// newFlakyUtilProvider serves two leaf prefixes and lets the caller decide, per
// `parent` value, how the child-IP lookup behaves.
func newFlakyUtilProvider(t *testing.T, ipAddresses http.HandlerFunc) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("within") != "" { // container child-prefix lookup
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[
			{"id":1,"prefix":"10.1.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null},
			{"id":2,"prefix":"10.2.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}
		]}`)
	})
	mux.HandleFunc("/api/ipam/ip-ranges/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
	})
	mux.HandleFunc("/api/ipam/ip-addresses/", ipAddresses)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})
}

// TestQuery_UtilizationFailureIsReported covers the degradation contract: the
// rows that worked keep their values, the row that failed is BLANK rather than
// wrong, and the result says so. A blank utilization cell renders like a zero,
// so without the warning the reader is told a busy prefix is empty.
func TestQuery_UtilizationFailureIsReported(t *testing.T) {
	p := newFlakyUtilProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("parent") == "10.2.0.0/24" {
			w.WriteHeader(http.StatusInternalServerError) // not retryable
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"address":"10.1.0.5/24"}]}`)
	})
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0]["used"] != float64(1) {
		t.Errorf("healthy row used = %v, want 1", res.Rows[0]["used"])
	}
	for _, col := range utilizationFieldNames() {
		if v, ok := res.Rows[1][col]; ok {
			t.Errorf("failed row %s = %v, want absent (a wrong number is worse than none)", col, v)
		}
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "1 of the 2 rows measured") {
		t.Fatalf("warnings = %v, want one naming 1 of the 2 rows measured", res.Warnings)
	}
}

// TestQuery_UtilizationUnsizeableIsReportedSeparately pins the other sentence: a
// row NetBox gives no usable size for is not a failure of ours and retrying will
// not change it, so it must not be reported as one.
func TestQuery_UtilizationUnsizeableIsReportedSeparately(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/ip-ranges/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[
			{"id":1,"start_address":"10.9.0.1/24","end_address":"10.9.0.6/24","mark_utilized":false,"vrf":null}
		]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := New(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/ip-ranges",
		Fields:     []string{"utilization", "used", "available"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no usable prefix or range size") {
		t.Fatalf("warnings = %v, want the unsizeable sentence", res.Warnings)
	}
}

// TestUtilizationChildLookup_RetriesTransientFailure pins that the child lookups
// go through the retrying page walk. They did not: they called getJSON directly,
// so a single 503 from an upstream that sheds load under exactly the query sizes
// this feature is for blanked the row outright.
func TestUtilizationChildLookup_RetriesTransientFailure(t *testing.T) {
	var attempts int32
	p := newFlakyUtilProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= 2 { // one 503 per prefix, then success
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"address":"10.1.0.5/24"}]}`)
	})
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none (the 503 should have been retried)", res.Warnings)
	}
	for i := range res.Rows {
		if res.Rows[i]["used"] != float64(1) {
			t.Errorf("row %d used = %v, want 1", i, res.Rows[i]["used"])
		}
	}
}

// TestQuery_UtilizationReportsItsCost pins the note. Selecting these three
// columns is the difference between one upstream request and one per row plus,
// and the query editor gives no hint of that — the user picks three columns from
// the same list as every other column and the query gets an order of magnitude
// slower. The note is the only place that is ever said.
func TestQuery_UtilizationReportsItsCost(t *testing.T) {
	p := newFlakyUtilProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"address":"10.1.0.5/24"}]}`)
	})
	// Two leaf prefixes, each costing a child-IP page and a child-range page.
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "4 extra NetBox requests for these 2 rows") {
		t.Fatalf("notes = %v, want one naming 4 extra requests for 2 rows", res.Notes)
	}

	// Not selected, not charged, and nothing to say.
	res, err = p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) != 0 {
		t.Fatalf("notes = %v, want none when utilization is not selected", res.Notes)
	}
}

// newUtilizationServer serves `rows` leaf prefixes and answers every child
// lookup after `delay`, counting the lookups. delay 0 is a healthy NetBox (the
// bundled demo answers a child lookup in ~20ms); a non-zero delay stands in for
// the slow remote instance the old fixed row cap was calibrated against.
func newUtilizationServer(t *testing.T, rows int, delay time.Duration, lookups *atomic.Int64) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("within") != "" {
			lookups.Add(1)
			time.Sleep(delay)
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
			return
		}
		var b strings.Builder
		for i := range rows {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"prefix":"10.%d.%d.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}`,
				i+1, i/256, i%256)
		}
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, rows, b.String())
	})
	for _, path := range []string{"/api/ipam/ip-addresses/", "/api/ipam/ip-ranges/"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			lookups.Add(1)
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestQuery_UtilizationIsBoundedByTime pins the "fail properly" bound. The rows
// past it keep every other column and their place in the order; only the three
// computed columns are blank, the unmeasured rows are a contiguous suffix rather
// than a scatter, and the result says so. The alternative is not a slower
// answer, it is a Grafana timeout: an error toast, an empty panel, and thousands
// of requests spent anyway.
func TestQuery_UtilizationIsBoundedByTime(t *testing.T) {
	const rows = 300
	var lookups atomic.Int64
	// ~50ms of upstream per row (two sequential child lookups) against a 200ms
	// budget and 8 workers: the bound bites well inside the page.
	url := newUtilizationServer(t, rows, 25*time.Millisecond, &lookups)
	p := New(url, "test-token", &http.Client{Timeout: 10 * time.Second},
		WithRequestTimeout(300*time.Millisecond))

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
		Limit:      rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != rows {
		t.Fatalf("rows = %d, want %d — the bound must not drop rows", len(res.Rows), rows)
	}
	if res.Capped == nil {
		t.Fatal("res.Capped = nil: a budget this small against an upstream this slow must bind")
	}
	measured := res.Capped.Measured
	if measured < 1 || measured >= rows {
		t.Fatalf("measured %d of %d rows, want at least one and not all", measured, rows)
	}
	if res.Capped.Rows != rows {
		t.Errorf("cap rows = %d, want %d", res.Capped.Rows, rows)
	}
	if !slices.Equal(res.Capped.Columns, utilizationFieldNames()) {
		t.Errorf("cap columns = %v, want %v", res.Capped.Columns, utilizationFieldNames())
	}
	// The measured rows are the FIRST ones, so a user reading down the table sees
	// values then blanks, not blanks salted through the middle of the answer.
	for i, row := range res.Rows {
		_, ok := row["utilization"]
		if want := i < measured; ok != want {
			t.Fatalf("row %d has utilization = %v, want %v (measured = %d)", i, ok, want, measured)
		}
		if row["prefix"] == nil {
			t.Fatalf("row %d lost its other columns", i)
		}
	}
	// The bound is reported as a CAP, not as a warning. A warning means a lookup
	// failed and an alert rule must refuse to evaluate; this measured everything
	// it said it would, and an alert over it needs to be told to lower its limit.
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none — nothing failed here", res.Warnings)
	}
	// The point of the bound is the requests it does not spend.
	if n := lookups.Load(); n > 2*int64(measured)+16 {
		t.Errorf("lookups = %d, want about %d — the bound stopped dispatching, it did not just hide results", n, 2*measured)
	}
}

// TestQuery_UtilizationMeasuresEveryRowOnAHealthyUpstream is the other half of
// the same change. 200 rows is over the fixed 150-row cap this bound replaced,
// and on an instance that answers instantly capping there was pure loss: the
// blank columns broke a live alert rule and protected nobody from a timeout that
// was never going to happen.
func TestQuery_UtilizationMeasuresEveryRowOnAHealthyUpstream(t *testing.T) {
	const rows = 200
	var lookups atomic.Int64
	p := New(newUtilizationServer(t, rows, 0, &lookups), "test-token",
		&http.Client{Timeout: 10 * time.Second}, WithRequestTimeout(30*time.Second))

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
		Limit:      rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Capped != nil {
		t.Errorf("cap = %+v, want none: every row was measured well inside the budget", res.Capped)
	}
	for i, row := range res.Rows {
		if _, ok := row["utilization"]; !ok {
			t.Fatalf("row %d is blank, want measured", i)
		}
	}
}

// newTieredUtilizationServer serves `rows` leaf prefixes whose child lookups
// answer immediately for the first `fast` rows and only after `delay` for every
// row past them. The row index is recovered from the `parent` CIDR, which is the
// prefix the row was listed as, so the two tiers line up exactly with the rows.
//
// The shape matters: a uniform delay cannot separate "stopped dispatching" from
// "stopped working", because the overrun it produces is one row's worth of time
// either way. Here the budget expires with a whole wave of slow lookups in
// flight, which is the case the bound has to cover.
func newTieredUtilizationServer(t *testing.T, rows, fast int, delay time.Duration) string {
	t.Helper()
	rowIndex := func(r *http.Request) int {
		var a, b int
		if _, err := fmt.Sscanf(r.URL.Query().Get("parent"), "10.%d.%d.0/24", &a, &b); err != nil {
			return 0
		}
		return a*256 + b
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var b strings.Builder
		for i := range rows {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"prefix":"10.%d.%d.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}`,
				i+1, i/256, i%256)
		}
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, rows, b.String())
	})
	for _, path := range []string{"/api/ipam/ip-addresses/", "/api/ipam/ip-ranges/"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if rowIndex(r) >= fast {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestQuery_UtilizationBudgetBoundsRowsInFlight pins the half of the bound that
// dispatch alone cannot deliver: the lookups already RUNNING when the budget
// expires stop too.
//
// Checking the clock before starting a row bounds nothing on its own. A wave
// dispatched a millisecond before the budget runs out still gets the whole
// per-request timeout, and the wait for it is the wait for the query — so a 200ms
// budget against this upstream ran for seconds and came back with a page of
// failed lookups, which is the timeout and the empty panel the bound exists to
// prevent.
//
// The three assertions are one statement each:
//   - the wall clock stops near the budget, not near the per-request timeout;
//   - NOTHING is reported as a failed lookup, because nothing failed — a lookup
//     the budget cut short is the bound working, and a warning here fails an
//     alert rule outright (see pkg/plugin/query.go);
//   - the cap's Measured names the rows that actually carry values, because the
//     alert message tells the user to lower the row limit TO that number.
func TestQuery_UtilizationBudgetBoundsRowsInFlight(t *testing.T) {
	const (
		rows = 40
		fast = 8
	)
	// The first wave answers instantly and gets measured; every row after it
	// sleeps far past the budget. The per-request timeout is 1s — the quantity the
	// budget must NOT be allowed to run to.
	url := newTieredUtilizationServer(t, rows, fast, 2*time.Second)
	p := New(url, "test-token", &http.Client{Timeout: time.Second},
		WithRequestTimeout(300*time.Millisecond)) // a 200ms measurement budget

	start := time.Now()
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
		Limit:      rows,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 900*time.Millisecond {
		t.Errorf("query took %v for a 200ms budget: the rows in flight when the budget expired ran on to the per-request timeout", elapsed)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none: a lookup the budget cut short did not fail, and a warning here fails an alert rule", res.Warnings)
	}
	if len(res.Rows) != rows {
		t.Fatalf("rows = %d, want %d — the bound must not drop rows", len(res.Rows), rows)
	}
	if res.Capped == nil {
		t.Fatal("res.Capped = nil: this budget cannot measure every row and must say so")
	}
	// Measured must be the rows that actually hold values, and they must be the
	// leading ones — "lower the row limit to N" is false advice otherwise.
	withValues := 0
	for i, row := range res.Rows {
		_, ok := row["utilization"]
		if ok {
			withValues++
		}
		if want := i < res.Capped.Measured; ok != want {
			t.Fatalf("row %d has utilization = %v, want %v (measured = %d)", i, ok, want, res.Capped.Measured)
		}
	}
	if res.Capped.Measured != withValues {
		t.Errorf("cap measured = %d, but %d rows carry values", res.Capped.Measured, withValues)
	}
	if withValues < 1 || withValues > fast {
		t.Errorf("%d rows measured, want between 1 and %d — the fast rows and no slow one", withValues, fast)
	}
}

// TestQuery_UtilizationCallerCancellationIsNotACap is the other side of the
// distinction the budget deadline introduces. Both cancellations reach the
// transport as the same context error, and only one of them is a bound: the
// budget stopping this enrichment is a cap, while the CALLER going away — a
// closed dashboard, the datasource timeout expiring — is not. Nothing about the
// row limit fixes it, so telling the user to lower it would send them off to fix
// the wrong thing.
func TestQuery_UtilizationCallerCancellationIsNotACap(t *testing.T) {
	const rows = 8
	var lookups atomic.Int64
	// A budget far larger than the caller's own patience: whatever stops these
	// lookups, it is not the budget.
	url := newUtilizationServer(t, rows, 200*time.Millisecond, &lookups)
	p := New(url, "test-token", &http.Client{Timeout: 10 * time.Second},
		WithRequestTimeout(30*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for lookups.Load() == 0 { // cancel with the first wave in flight
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	res, err := p.Query(ctx, provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization", "used", "available"},
		Limit:      rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Capped != nil {
		t.Errorf("cap = %+v, want none: the caller went away, which no row limit fixes", res.Capped)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "lookups those columns need failed") {
		t.Fatalf("warnings = %v, want the one naming failed lookups", res.Warnings)
	}
}

func TestUtilizationBudget(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		// The default timeout reproduces the bound the old constant encoded: ~20s
		// measures ~150 rows on the slow instance it was calibrated against.
		{"unset falls back to the settings default", 0, 20 * time.Second},
		{"the default timeout", 30 * time.Second, 20 * time.Second},
		{"a short timeout is obeyed, not floored", 3 * time.Second, 2 * time.Second},
		{"a long timeout stops at the ceiling", 10 * time.Minute, maxUtilizationBudget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New("http://nb", "t", &http.Client{}, WithRequestTimeout(tc.timeout))
			if got := p.utilizationBudget(); got != tc.want {
				t.Errorf("budget = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUtilizationDeadlineReservesTimeForRowsInFlight covers the interaction that
// would otherwise turn a slow query into a false alarm: dispatching a row just
// before ctx expires buys a CANCELLED lookup, which counts as a failure and is
// reported as degraded data — the very confusion this whole distinction exists
// to prevent.
func TestUtilizationDeadlineReservesTimeForRowsInFlight(t *testing.T) {
	p := New("http://nb", "t", &http.Client{}, WithRequestTimeout(30*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	left := time.Until(p.utilizationDeadline(ctx))
	if left <= 0 || left > 850*time.Millisecond {
		t.Errorf("deadline leaves %v of a 1s context, want a little under 800ms", left)
	}

	// No deadline on the context: the budget stands on its own.
	if left := time.Until(p.utilizationDeadline(context.Background())); left < 19*time.Second {
		t.Errorf("deadline leaves %v, want the whole 20s budget", left)
	}
}

// cutByBudget decides whether an unmeasured row is a deliberate BOUND (counts
// toward the cap, stays out of Warnings) or a genuine FAILURE (a warning, which
// the alert path turns into a hard error). Getting it wrong in either direction
// is a bug this branch already shipped once.
//
// The subtle part: context.WithDeadlineCause makes net/http surface the CAUSE,
// so a budget-cut lookup wraps errUtilizationBudget and does NOT wrap
// context.DeadlineExceeded. An earlier revision required the latter and
// therefore never fired at all.
func TestCutByBudget(t *testing.T) {
	budgetCtx, cancel := context.WithDeadlineCause(
		context.Background(), time.Now().Add(-time.Second), errUtilizationBudget)
	defer cancel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"a lookup the budget cut", fmt.Errorf("get: %w", errUtilizationBudget), true},
		{"the bare budget cause", errUtilizationBudget, true},
		{"a bare deadline, which the budget path does NOT produce", context.DeadlineExceeded, false},
		{"the caller cancelling", context.Canceled, false},
		{"a genuine upstream failure that merely landed late", errors.New("netbox API 502"), false},
		{"an unsizeable row", errUnsizeable, false},
		{"no error at all", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutByBudget(budgetCtx, tc.err); got != tc.want {
				t.Errorf("cutByBudget = %v, want %v — misclassifying this turns a bound into a "+
					"warning (hard alert error) or a real failure into a silent blank", got, tc.want)
			}
		})
	}
}

// Every sentence about the computed columns must name the ones the query asked
// for. Projection removes the others from the result entirely, so telling
// someone their `used`-only alert failed because "utilization, used and
// available" are blank names two columns that are not in their frame.
func TestSelectedUtilizationFields(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   []string
	}{
		{"only used", []string{"prefix", "used"}, []string{"used"}},
		{"only available", []string{"available"}, []string{"available"}},
		{"used and available, in declaration order", []string{"available", "used"}, []string{"used", "available"}},
		{"all three", []string{"utilization", "used", "available"}, []string{"utilization", "used", "available"}},
		{"none selected falls back rather than saying nothing", []string{"prefix"}, []string{"utilization", "used", "available"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectedUtilizationFields(tc.fields)
			if !slices.Equal(got, tc.want) {
				t.Errorf("selectedUtilizationFields(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

func TestAndListNames(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"used"}, "used"},
		{[]string{"used", "available"}, "used and available"},
		{[]string{"utilization", "used", "available"}, "utilization, used and available"},
		{nil, ""},
	} {
		if got := andListNames(tc.in); got != tc.want {
			t.Errorf("andListNames(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// End to end: a used-only query whose lookups fail must not mention utilization
// or available anywhere in what the user is told.
func TestUtilizationMessagesNameOnlyTheSelectedColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/ip-addresses") || strings.Contains(r.URL.Path, "/ip-ranges") {
			w.WriteHeader(http.StatusInternalServerError) // force a lookup failure
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"results":[{"id":1,"prefix":"10.0.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "used"},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("a failed lookup must be reported")
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "utilization") || strings.Contains(w, "available") {
			t.Errorf("warning names a column the query did not select: %q", w)
		}
		if !strings.Contains(w, "used") {
			t.Errorf("warning does not name the column that IS blank: %q", w)
		}
	}
}

func TestIsAre(t *testing.T) {
	if got := isAre(1); got != "is" {
		t.Errorf("isAre(1) = %q, want is", got)
	}
	for _, n := range []int{0, 2, 3} {
		if got := isAre(n); got != "are" {
			t.Errorf("isAre(%d) = %q, want are", n, got)
		}
	}
}

// The cost note exists to tell a user what these columns cost NetBox. A page
// that succeeded on its third attempt cost three requests; reporting one
// understates the load during exactly the degraded conditions where the number
// is worth reading.
func TestUtilizationCostCountsEveryAttempt(t *testing.T) {
	var childCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/ip-addresses") {
			childCalls++
			if childCalls <= 2 { // two transient failures, then success
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
			return
		}
		if strings.Contains(r.URL.Path, "/ip-ranges") {
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"results":[{"id":1,"prefix":"10.0.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "utilization"},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Notes) == 0 {
		t.Fatal("the cost note must be present")
	}
	// The child-IP lookup took 3 HTTP requests (2 x 503 + 1 success) plus the
	// ip-ranges lookup: 4. Counting per call rather than per attempt reports 2.
	if !strings.Contains(res.Notes[0], "4 extra NetBox requests") {
		t.Errorf("cost note undercounts retried attempts: %q", res.Notes[0])
	}
}

// The cost note is the third surface that names the computed columns, after the
// cap and the warnings. It was missed when the other two were fixed.
func TestUtilizationCostNoteNamesOnlySelectedColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/ip-addresses") || strings.Contains(r.URL.Path, "/ip-ranges") {
			_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"results":[{"id":1,"prefix":"10.0.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes",
		Fields:     []string{"prefix", "used"},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Notes) == 0 {
		t.Fatal("the cost note must be present")
	}
	note := res.Notes[0]
	for _, unwanted := range []string{"utilization", "available"} {
		if strings.Contains(note, unwanted) && !strings.Contains(note, "publishes utilization on no list endpoint") {
			t.Errorf("cost note names a column the query did not select (%s): %q", unwanted, note)
		}
	}
	if !strings.Contains(note, "The used column") {
		t.Errorf("cost note does not name the selected column in the singular: %q", note)
	}
	if !strings.Contains(note, "Deselect it") {
		t.Errorf("cost note does not agree its pronoun with one column: %q", note)
	}
}
