package netbox

import (
	"context"
	"net/netip"
	"testing"

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
	used, avail, util, ok := p.computeUtilization(context.Background(), "ipam/prefixes", raw)
	// size 254; used = 10 (range) -> floor(10/254*100) = 3.
	if !ok || used != 10 || avail != 244 || util != 3 {
		t.Fatalf("leaf utilized range: used=%v avail=%v util=%v ok=%v", used, avail, util, ok)
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
	used, avail, util, ok := p.computeUtilization(context.Background(), "ipam/prefixes", raw)
	if !ok {
		t.Fatal("ok=false")
	}
	if used != 3 || avail != 251 || util != 1 { // 3/254*100 = 1.18 -> floor 1
		t.Fatalf("used=%v avail=%v util=%v; want 3/251/1", used, avail, util)
	}
}

func TestComputeUtilization_MarkUtilized(t *testing.T) {
	p := newTestProvider(t)
	raw := []byte(`{"prefix":"10.0.0.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":true,"family":{"value":4},"vrf":null}`)
	used, avail, util, ok := p.computeUtilization(context.Background(), "ipam/prefixes", raw)
	if !ok || util != 100 || used != 254 || avail != 0 {
		t.Fatalf("mark_utilized: used=%v avail=%v util=%v ok=%v", used, avail, util, ok)
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
	used, _, util, ok := p.computeUtilization(context.Background(), "ipam/prefixes", raw)
	// /23 size = 512; one child /24 = 256 -> 256/512 = 50%.
	if !ok || used != 256 || util != 50 {
		t.Fatalf("container: used=%v util=%v ok=%v", used, util, ok)
	}
}

func TestComputeUtilization_ContainerMarkUtilized(t *testing.T) {
	p := newTestProvider(t)
	raw := []byte(`{"prefix":"10.0.0.0/23","status":{"value":"container"},"mark_utilized":true,"family":{"value":4},"vrf":null}`)
	// mark_utilized short-circuits the container branch: no "within" child-prefix
	// lookup happens, used=size (no network/broadcast deduction for containers).
	used, avail, util, ok := p.computeUtilization(context.Background(), "ipam/prefixes", raw)
	if !ok || used != 512 || avail != 0 || util != 100 {
		t.Fatalf("container mark_utilized: used=%v avail=%v util=%v ok=%v", used, avail, util, ok)
	}
}

func TestComputeUtilization_IPRange(t *testing.T) {
	p := newTestProvider(t)
	raw := []byte(`{"start_address":"10.9.0.1/24","end_address":"10.9.0.6/24","size":6,"mark_utilized":false,"vrf":null}`)
	// ip-addresses mock returns count=3 for every ?parent=; range covers 4 CIDR blocks
	// (10.9.0.1/32, 10.9.0.2/31, 10.9.0.4/31, 10.9.0.6/32) -> summed used is capped at size 6.
	used, avail, util, ok := p.computeUtilization(context.Background(), "ipam/ip-ranges", raw)
	if !ok || used != 6 || avail != 0 || util != 100 {
		t.Fatalf("range: used=%v avail=%v util=%v ok=%v", used, avail, util, ok)
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
