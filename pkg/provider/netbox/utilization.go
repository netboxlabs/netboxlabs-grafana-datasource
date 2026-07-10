package netbox

import (
	"context"
	"encoding/json"
	"math"
	"net/netip"
	"net/url"
	"strconv"
	"sync"

	"go4.org/netipx"
)

// utilizationFieldNames are the computed, opt-in columns added for IPAM types.
func utilizationFieldNames() []string { return []string{"utilization", "used", "available"} }

// isUtilizationType reports whether an object type gets utilization columns.
func isUtilizationType(objectType string) bool {
	return objectType == "ipam/prefixes" || objectType == "ipam/ip-ranges"
}

// prefixUsableSize is the utilization denominator for a prefix, matching NetBox:
// 2^hostbits, minus network+broadcast for IPv4 prefixes shorter than /31 that
// are not pools.
func prefixUsableSize(pfx netip.Prefix, isPool bool) float64 {
	hostBits := pfx.Addr().BitLen() - pfx.Bits()
	size := math.Pow(2, float64(hostBits))
	if pfx.Addr().Is4() && pfx.Bits() < 31 && !isPool {
		size -= 2
	}
	return size
}

// rangeCIDRs returns the minimal set of CIDR blocks that exactly tile the
// inclusive host range [start, end]. Blocks are pairwise-disjoint.
func rangeCIDRs(start, end netip.Addr) []netip.Prefix {
	r := netipx.IPRangeFrom(start, end)
	if !r.IsValid() {
		return nil
	}
	return r.Prefixes()
}

// unionPrefixSize returns the number of distinct addresses covered by the given
// prefixes, de-duplicating overlaps (NetBox uses an IPSet for this).
func unionPrefixSize(prefixes []netip.Prefix) float64 {
	var b netipx.IPSetBuilder
	for _, p := range prefixes {
		b.AddPrefix(p)
	}
	set, err := b.IPSet()
	if err != nil {
		return 0
	}
	var total float64
	for _, p := range set.Prefixes() {
		total += math.Pow(2, float64(p.Addr().BitLen()-p.Bits()))
	}
	return total
}

type countEnvelope struct {
	Count int `json:"count"`
}

// vrfRef is the minimal VRF reference NetBox embeds in IPAM objects.
type vrfRef struct {
	ID int `json:"id"`
}

// utilObject is the subset of a NetBox prefix/ip-range we read for utilization.
type utilObject struct {
	Prefix       string   `json:"prefix"`        // prefixes
	StartAddress string   `json:"start_address"` // ip-ranges
	EndAddress   string   `json:"end_address"`
	Size         *float64 `json:"size"` // ip-ranges (API-provided)
	IsPool       bool     `json:"is_pool"`
	MarkUtilized bool     `json:"mark_utilized"`
	Status       struct {
		Value string `json:"value"`
	} `json:"status"`
	VRF *vrfRef `json:"vrf"`
}

func setVRF(q url.Values, vrf *vrfRef) {
	if vrf == nil {
		q.Set("vrf_id", "null")
	} else {
		q.Set("vrf_id", strconv.Itoa(vrf.ID))
	}
}

// countContainedIPs returns how many IP addresses NetBox reports within cidr.
// Used for IP-range utilization, which NetBox computes as a raw child-IP count
// (not an IPSet) — so this does not de-duplicate hosts, matching NetBox.
func (p *Provider) countContainedIPs(ctx context.Context, cidr string, vrf *vrfRef) (float64, error) {
	q := url.Values{}
	q.Set("parent", cidr)
	q.Set("limit", "1")
	setVRF(q, vrf)
	var env countEnvelope
	if err := p.client.getJSON(ctx, p.client.apiURL("ipam/ip-addresses", q), &env); err != nil {
		return 0, err
	}
	return float64(env.Count), nil
}

// leafUsedSize computes NetBox's "used" for a non-container prefix: the size of
// the IPSet formed by every child IP host address plus every marked-utilized
// child IP range. The IPSet de-duplicates hosts (NetBox counts a host once even
// when multiple IPAddress objects exist for it, e.g. HA/VIP) and folds in
// utilized ranges that carry no individual IPAddress rows — mirroring
// Prefix.get_utilization() in NetBox.
func (p *Provider) leafUsedSize(ctx context.Context, cidr string, vrf *vrfRef) (float64, error) {
	hosts, err := p.childIPHosts(ctx, cidr, vrf)
	if err != nil {
		return 0, err
	}
	ranges, err := p.utilizedChildRanges(ctx, cidr, vrf)
	if err != nil {
		return 0, err
	}
	return ipSetSize(hosts, ranges), nil
}

// ipSetSize returns the number of distinct addresses covered by the given host
// addresses and ranges combined (overlaps counted once).
func ipSetSize(hosts []netip.Addr, ranges []netipx.IPRange) float64 {
	var b netipx.IPSetBuilder
	for _, h := range hosts {
		b.Add(h)
	}
	for _, r := range ranges {
		b.AddRange(r)
	}
	set, err := b.IPSet()
	if err != nil {
		return 0
	}
	var total float64
	for _, p := range set.Prefixes() {
		total += math.Pow(2, float64(p.Addr().BitLen()-p.Bits()))
	}
	return total
}

// childIPHosts returns the host address of every IP address contained in cidr.
func (p *Provider) childIPHosts(ctx context.Context, cidr string, vrf *vrfRef) ([]netip.Addr, error) {
	q := url.Values{}
	q.Set("parent", cidr)
	q.Set("limit", strconv.Itoa(pageSize))
	setVRF(q, vrf)
	var out []netip.Addr
	next := p.client.apiURL("ipam/ip-addresses", q)
	for next != "" {
		var page struct {
			Next    *string `json:"next"`
			Results []struct {
				Address string `json:"address"`
			} `json:"results"`
		}
		if err := p.client.getJSON(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Results {
			if a, err := netip.ParseAddr(stripMask(r.Address)); err == nil {
				out = append(out, a)
			}
		}
		if page.Next == nil {
			break
		}
		next = *page.Next
	}
	return out, nil
}

// utilizedChildRanges returns the marked-utilized IP ranges contained in cidr.
func (p *Provider) utilizedChildRanges(ctx context.Context, cidr string, vrf *vrfRef) ([]netipx.IPRange, error) {
	q := url.Values{}
	q.Set("parent", cidr)
	q.Set("mark_utilized", "true")
	q.Set("limit", strconv.Itoa(pageSize))
	setVRF(q, vrf)
	var out []netipx.IPRange
	next := p.client.apiURL("ipam/ip-ranges", q)
	for next != "" {
		var page struct {
			Next    *string `json:"next"`
			Results []struct {
				Start string `json:"start_address"`
				End   string `json:"end_address"`
			} `json:"results"`
		}
		if err := p.client.getJSON(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Results {
			s, err1 := netip.ParseAddr(stripMask(r.Start))
			e, err2 := netip.ParseAddr(stripMask(r.End))
			if err1 == nil && err2 == nil {
				if rg := netipx.IPRangeFrom(s, e); rg.IsValid() {
					out = append(out, rg)
				}
			}
		}
		if page.Next == nil {
			break
		}
		next = *page.Next
	}
	return out, nil
}

func (p *Provider) computeUtilization(ctx context.Context, objectType string, raw json.RawMessage) (used, available, util float64, ok bool) {
	var o utilObject
	if err := json.Unmarshal(raw, &o); err != nil {
		return 0, 0, 0, false
	}

	var size float64
	switch objectType {
	case "ipam/prefixes":
		pfx, err := netip.ParsePrefix(o.Prefix)
		if err != nil {
			return 0, 0, 0, false
		}
		isContainer := o.Status.Value == "container"
		if isContainer {
			size = math.Pow(2, float64(pfx.Addr().BitLen()-pfx.Bits()))
		} else {
			size = prefixUsableSize(pfx, o.IsPool)
		}
		if o.MarkUtilized {
			used = size
		} else if isContainer {
			children, err := p.childPrefixes(ctx, o.Prefix, o.VRF)
			if err != nil {
				return 0, 0, 0, false
			}
			used = unionPrefixSize(children)
		} else {
			u, err := p.leafUsedSize(ctx, o.Prefix, o.VRF)
			if err != nil {
				return 0, 0, 0, false
			}
			used = u
		}
	case "ipam/ip-ranges":
		if o.Size == nil {
			return 0, 0, 0, false
		}
		size = *o.Size
		if o.MarkUtilized {
			used = size
		} else {
			start, err1 := netip.ParseAddr(stripMask(o.StartAddress))
			end, err2 := netip.ParseAddr(stripMask(o.EndAddress))
			if err1 != nil || err2 != nil {
				return 0, 0, 0, false
			}
			for _, cidr := range rangeCIDRs(start, end) {
				c, err := p.countContainedIPs(ctx, cidr.String(), o.VRF)
				if err != nil {
					return 0, 0, 0, false
				}
				used += c
			}
		}
	default:
		return 0, 0, 0, false
	}

	if used > size {
		used = size
	}
	available = size - used
	if available < 0 {
		available = 0
	}
	if size > 0 {
		util = math.Floor(used / size * 100)
		if util > 100 {
			util = 100
		}
	}
	return used, available, util, true
}

// wantsUtilization reports whether the requested fields include any util column.
func wantsUtilization(fields []string) bool {
	for _, f := range fields {
		for _, u := range utilizationFieldNames() {
			if f == u {
				return true
			}
		}
	}
	return false
}

// enrichUtilization computes utilization for each object and merges the values
// into the corresponding flat row. raws and flatRows are index-aligned by
// construction (both appended together in Query). Bounded to 8 concurrent
// upstream calls; a row that fails computation is left without util values
// rather than failing the whole query.
func (p *Provider) enrichUtilization(ctx context.Context, objectType string, raws []json.RawMessage, flatRows []map[string]interface{}) {
	const workers = 8
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range raws {
		wg.Add(1)
		sem <- struct{}{}
		go func(raw json.RawMessage, row map[string]interface{}) {
			defer wg.Done()
			defer func() { <-sem }()
			if used, available, util, ok := p.computeUtilization(ctx, objectType, raw); ok {
				row["used"] = used
				row["available"] = available
				row["utilization"] = util
			}
		}(raws[i], flatRows[i])
	}
	wg.Wait()
}

// childPrefixes lists prefixes contained within cidr (for container utilization).
func (p *Provider) childPrefixes(ctx context.Context, cidr string, vrf *vrfRef) ([]netip.Prefix, error) {
	q := url.Values{}
	q.Set("within", cidr)
	q.Set("limit", "500")
	setVRF(q, vrf)
	var out []netip.Prefix
	next := p.client.apiURL("ipam/prefixes", q)
	for next != "" {
		var page struct {
			Next    *string `json:"next"`
			Results []struct {
				Prefix string `json:"prefix"`
			} `json:"results"`
		}
		if err := p.client.getJSON(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Results {
			if pfx, err := netip.ParsePrefix(r.Prefix); err == nil {
				out = append(out, pfx)
			}
		}
		if page.Next == nil {
			break
		}
		next = *page.Next
	}
	return out, nil
}

// stripMask returns the address portion of "10.0.0.1/24" -> "10.0.0.1".
func stripMask(s string) string {
	if i := indexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
