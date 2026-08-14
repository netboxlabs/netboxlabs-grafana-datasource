package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go4.org/netipx"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"strings"
)

// utilizationFieldNames are the computed, opt-in columns added for IPAM types.
func utilizationFieldNames() []string { return []string{"utilization", "used", "available"} }

// utilizationSourceFields are the object properties utilization is computed
// FROM — the fields utilObject unmarshals out of the same raw row the flattened
// columns came from (see computeUtilization).
//
// They must be requested explicitly whenever a utilization column is asked for,
// because none of them is named by a utilization column and a projected request
// would drop them. Dropping one does not fail: it returns a WRONG NUMBER with a
// 200. Without `vrf`, every child-object lookup queries the global table instead
// of the object's own VRF. Without `status`, a container prefix reads as a leaf
// and gets enumerated child IP by child IP instead of by child prefix. Without
// `is_pool` / `mark_utilized` the denominator or the numerator silently shifts;
// without `size`, `start_address` and `end_address` an IP range cannot be sized
// at all.
//
// The computed columns themselves (utilization, used, available) are NOT here
// and must never be sent: they are not NetBox fields.
func utilizationSourceFields() []string {
	return []string{
		"prefix", "status", "is_pool", "mark_utilized", "vrf",
		"start_address", "end_address", "size",
	}
}

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

// The utilization measurement bound.
//
// Some bound is required, because there is no cheap version of this work to fall
// back on (see enrichUtilization: NetBox publishes utilization nowhere).
// Measured against NetBox Cloud staging, a 50-prefix page costs 119 extra
// requests and 7.5s — about 2.4 requests and 0.15s per row at 8 concurrent
// workers (medians of three paced samples; the request count is exact and
// reproduces, the time moves with that instance's load — see enrichUtilization).
// The provider's DEFAULT row limit is 1,000 and its maximum is 10,000, so an
// unbounded fan-out there is ~2,400 requests and ~2.5 minutes, or ~24,000
// requests and ~25. Neither returns: Grafana's own query timeout fires
// first and the user gets an error toast and an empty panel, which is the worst
// of every outcome — no data, no numbers, no explanation, and a few thousand
// requests spent on the upstream anyway.
//
// The bound is WALL-CLOCK TIME, not a row count, and that is a change. It used
// to be a fixed 150 rows, derived from a heavier state of that same instance
// than the one measured above — 200 rows in 26.5s — rounded down to leave
// headroom inside a 30s timeout. That the two disagree is the point. The flaw
// is not the arithmetic, it is that the arithmetic describes ONE deliberately
// throttled instance and was then applied to every instance regardless. A
// healthy NetBox answers a child lookup in ~20ms; the bundled demo measures its
// whole prefix table in well under a second, and refusing to measure row 151
// there protects nobody from anything. It did break a live alert rule.
//
// Time is the quantity actually worth bounding here — the thing the constant was
// defending against was a deadline, and a deadline cannot be mis-calibrated for
// an instance nobody measured. Fast upstream: every row gets measured. Slow
// upstream: as many as fit, and the result says how many.
//
// Past the bound the rows still come back, in the same order, with every other
// column intact; only the three computed columns are blank, and the result says
// so through Result.Capped — NOT through Warnings, which means a lookup failed.
// A partial answer that names its own edge beats a timeout, and the two outcomes
// are not symmetric: a bound set too low costs the user a filter, a bound set too
// high costs them the whole panel.
const (
	// utilizationBudgetNum/Den is the share of the datasource's request timeout
	// spent measuring. The timeout is the only statement the user makes about how
	// long this datasource may take; two thirds of it go to measurement, leaving a
	// third for the list fetch itself and for the rows still in flight when the
	// budget runs out. At the default 30s timeout that is 20s, which on the
	// throttled instance the old constant was calibrated against measures ~150
	// rows: the same bound, on the same instance, derived instead of guessed.
	utilizationBudgetNum = 2
	utilizationBudgetDen = 3

	// defaultRequestTimeout mirrors models.PluginSettings' own default, for a
	// provider constructed without WithRequestTimeout (tests, and any future
	// caller that forgets). Being wrong here costs speed, never correctness.
	defaultRequestTimeout = 30 * time.Second

	// maxUtilizationBudget is a ceiling with no matching floor, and the asymmetry
	// is deliberate. A SMALL timeout is a statement of impatience and is obeyed
	// exactly: the derivation stays proportional all the way down, and the
	// "measure at least one row" rule in enrichUtilization is the only floor
	// there is. A LARGE one is not the matching statement of patience — it bounds
	// ONE request, and a user who raised it because a single list page was slow
	// did not thereby ask for a five-minute panel refresh. Past a minute the
	// refresh has stopped being a refresh, so the budget stops there and says it
	// capped. A ctx deadline narrows things further either way (see
	// utilizationDeadline) — Grafana's own timeout always wins.
	maxUtilizationBudget = 60 * time.Second
)

// utilizationBudget is how long this query may spend measuring utilization.
func (p *Provider) utilizationBudget() time.Duration {
	timeout := p.requestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return min(timeout*utilizationBudgetNum/utilizationBudgetDen, maxUtilizationBudget)
}

// utilizationDeadline is the instant after which no row is measured any
// further: the budget, narrowed by the caller's own deadline when there is one.
//
// The reservation matters. Dispatching a row a millisecond before ctx expires
// buys a cancelled lookup, which is counted as a FAILURE and reported as
// degraded data — turning a slow query into the exact false alarm this whole
// distinction exists to prevent. A fifth of the remaining time is left over for
// the rows already running and for building the response.
func (p *Provider) utilizationDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(p.utilizationBudget())
	if dl, ok := ctx.Deadline(); ok {
		if reserved := time.Now().Add(time.Until(dl) * 4 / 5); reserved.Before(deadline) {
			return reserved
		}
	}
	return deadline
}

// errUtilizationBudget is the cause the measurement context carries when the
// budget — and not the caller — is what stopped a lookup.
//
// It exists to keep three cancellations apart that all surface as the same
// context.DeadlineExceeded down at the transport:
//
//   - the BUDGET expiring, which is this code deliberately stopping. Nothing
//     failed and nothing is missing that could have been fetched in the time the
//     user allowed, so it is reported through provider.Cap.
//   - the CALLER's context ending — a closed dashboard, the datasource timeout —
//     which is not a cap. The rows were not skipped for costing too much; the
//     query itself ran out, and the existing warning is the honest answer.
//   - a genuine lookup failure, which is neither.
//
// Only the first of the three is the bound working as designed. Collapsing it
// into the second turns a partial-but-correct answer into "degraded data", and
// pkg/plugin/query.go turns any warning on an alert path into a hard error — so
// a rule over a large prefix table would go to Error state naming a remedy that
// cannot help. That is the exact bug the Cap channel was added to fix; this is
// the same bug one layer down.
var errUtilizationBudget = errors.New("utilization measurement budget expired")

// cutByBudget reports whether a failed lookup was stopped by the budget rather
// than by a failure or by the caller going away.
//
// Both halves are needed. context.Cause names WHICH deadline fired — a caller
// cancellation propagates its own cause instead, so this is false there — while
// the error check keeps a genuine failure that merely landed after the budget
// expired (a 502 answered late, a body that stopped short) counted as the
// failure it is.
func cutByBudget(measureCtx context.Context, err error) bool {
	// Deliberately keyed on the error's own chain, NOT on
	// errors.Is(err, context.DeadlineExceeded). context.WithDeadlineCause makes
	// net/http surface the CAUSE, so a lookup the budget cut wraps
	// errUtilizationBudget and does NOT wrap context.DeadlineExceeded — an
	// earlier revision required both and therefore never fired once, which a
	// mutation test caught only because deleting it changed nothing.
	//
	// The cause alone is the right discriminator anyway: the caller's own
	// cancellation propagates the PARENT's cause, so a closed dashboard or an
	// expired datasource timeout does not match and keeps its warning; and a
	// genuine failure that merely landed after the deadline carries its own
	// error, not this one.
	_ = measureCtx
	return errors.Is(err, errUtilizationBudget)
}

// utilCost counts the upstream requests one enrichment spent on top of the list
// fetch the query would have made anyway.
//
// It is threaded explicitly through every lookup rather than sniffed at the HTTP
// client, because the number has to mean "what selecting these three columns
// cost you" and nothing else — the list page, the schema fetch and the branch
// resolution are not part of that answer. Atomic because the row workers run
// concurrently.
type utilCost struct{ requests atomic.Int64 }

// utilPage fetches one list page for a utilization lookup and counts it. Every
// utilization lookup goes through here, which is what keeps the reported cost
// honest: a lookup added later without this call would understate it.
func (p *Provider) utilPage(ctx context.Context, cost *utilCost, rawURL string) (listPage, error) {
	// Charged per ATTEMPT, not per call. A page that succeeded on its third try
	// cost the upstream three requests, and the note exists to tell the user what
	// these columns cost NetBox — understating it during exactly the degraded
	// conditions where the number is worth reading would invert its purpose.
	page, requests, err := p.getListPageRetryN(ctx, rawURL)
	cost.requests.Add(int64(requests))
	return page, err
}

// maxParentsPerRequest bounds how many `parent=` blocks ride in one request URL.
//
// NetBox's `parent` filter on ipam/ip-addresses is MULTI-VALUE and ORs its
// values — declared `{"type":"array"}` in /api/schema/ and verified live on both
// 4.4.10 and 4.6.4 by checking that two disjoint /24s holding 4 and 4 (resp. 1
// and 1) child IPs return 8 (resp. 2) together, i.e. neither the extra value
// being dropped nor the two being ANDed. That is what lets the blocks tiling one
// IP range share a single count request instead of taking one each.
//
// The cap only keeps the URL a sane length: an IPv6 range can tile into hundreds
// of blocks. Splitting across requests is exact, not approximate, because
// rangeCIDRs returns pairwise-disjoint blocks — every child IP falls in exactly
// one of them, so the counts of any partition sum to the same total the
// one-request-per-block loop produced.
const maxParentsPerRequest = 48

// countContainedIPs returns how many IP addresses NetBox reports within the
// given blocks combined. Used for IP-range utilization, which NetBox computes as
// a raw child-IP count (not an IPSet) — so this does not de-duplicate hosts,
// matching NetBox. The blocks must be pairwise disjoint (rangeCIDRs guarantees
// it); a duplicate address object is then still counted once per object, exactly
// as summing the per-block counts did.
func (p *Provider) countContainedIPs(ctx context.Context, cost *utilCost, cidrs []netip.Prefix, vrf *vrfRef) (float64, error) {
	var total float64
	for start := 0; start < len(cidrs); start += maxParentsPerRequest {
		q := url.Values{}
		for _, c := range cidrs[start:min(start+maxParentsPerRequest, len(cidrs))] {
			q.Add("parent", c.String())
		}
		q.Set("limit", "1")
		setVRF(q, vrf)
		page, err := p.utilPage(ctx, cost, p.client.apiURL("ipam/ip-addresses", q))
		if err != nil {
			return 0, err
		}
		n, ok := page.total()
		if !ok {
			// This walk asks NetBox to count and reads nothing else, so an
			// envelope with no count is not an answer at all. It cannot happen
			// here — the request carries no cursor parameter — but adding the
			// missing count to the total as zero would understate utilization,
			// i.e. report a full prefix as empty, so say so instead.
			return 0, errors.New("netbox returned no count for ipam/ip-addresses")
		}
		total += float64(n)
	}
	return total, nil
}

// leafUsedSize computes NetBox's "used" for a non-container prefix: the size of
// the IPSet formed by every child IP host address plus every marked-utilized
// child IP range. The IPSet de-duplicates hosts (NetBox counts a host once even
// when multiple IPAddress objects exist for it, e.g. HA/VIP) and folds in
// utilized ranges that carry no individual IPAddress rows — mirroring
// Prefix.get_utilization() in NetBox.
func (p *Provider) leafUsedSize(ctx context.Context, cost *utilCost, cidr string, vrf *vrfRef) (float64, error) {
	hosts, err := p.childIPHosts(ctx, cost, cidr, vrf)
	if err != nil {
		return 0, err
	}
	ranges, err := p.utilizedChildRanges(ctx, cost, cidr, vrf)
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
//
// This is the hop that enumerates, rather than counts: a leaf prefix's `used` is
// an IPSet over its child hosts, so every child object has to come back. Asking
// for `address` alone is therefore worth more here than anywhere else — a full
// IPAddress serialization carries tenant, VRF, assigned object, tags and custom
// fields, none of which is read, for each of up to pageSize objects per page.
//
// The projection is safe to lose: NetBox answers 200 and ignores an unrecognised
// `fields` name (see projection.go), which here costs bandwidth and nothing
// else, because the address is read out of the decoded object by name either
// way. NetBox carries the parameter into its own `next` link, so it survives
// paging.
func (p *Provider) childIPHosts(ctx context.Context, cost *utilCost, cidr string, vrf *vrfRef) ([]netip.Addr, error) {
	q := url.Values{}
	q.Set("parent", cidr)
	q.Set("limit", strconv.Itoa(pageSize))
	q.Set(fieldsParam, "address")
	setVRF(q, vrf)
	var out []netip.Addr
	next := p.client.apiURL("ipam/ip-addresses", q)
	for next != "" {
		page, err := p.utilPage(ctx, cost, next)
		if err != nil {
			return nil, err
		}
		for _, raw := range page.Results {
			var r struct {
				Address string `json:"address"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, fmt.Errorf("decode child IP of %s: %w", cidr, err)
			}
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
// Only the two endpoints are read; see childIPHosts on why the projection is
// requested and why losing it is harmless.
func (p *Provider) utilizedChildRanges(ctx context.Context, cost *utilCost, cidr string, vrf *vrfRef) ([]netipx.IPRange, error) {
	q := url.Values{}
	q.Set("parent", cidr)
	q.Set("mark_utilized", "true")
	q.Set("limit", strconv.Itoa(pageSize))
	q.Set(fieldsParam, "start_address,end_address")
	setVRF(q, vrf)
	var out []netipx.IPRange
	next := p.client.apiURL("ipam/ip-ranges", q)
	for next != "" {
		page, err := p.utilPage(ctx, cost, next)
		if err != nil {
			return nil, err
		}
		for _, raw := range page.Results {
			var r struct {
				Start string `json:"start_address"`
				End   string `json:"end_address"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, fmt.Errorf("decode utilized child range of %s: %w", cidr, err)
			}
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

// computeUtilization derives (used, available, utilization) for one prefix or IP
// range, issuing the child lookups NetBox gives no other way to get (see
// enrichUtilization for why there is no bulk alternative).
//
// # The IP-range `size <= 0` short-circuit
//
// The ip-ranges branch below skips its lookups entirely when size is not
// positive. This is an optimisation with NO observable effect, and the equality
// is worth spelling out because the whole point of it is that nothing moves.
// When size is not positive, the tail of this function pins the answer
// regardless of what the child lookup returns: `used` is clamped down to size,
// `available` is size-used = 0 (and the `available < 0` guard cannot fire), and
// `util` stays 0 because it is only ever computed under `size > 0`. So
// (size, 0, 0) is the result for every possible child count — the lookup can
// only spend requests, never change a number.
//
// This is not hypothetical. NetBox stores an IP range's `size` as a
// denormalized column populated on save; the bulk-loaded staging instance
// leaves it 0 on all 4,000 of its ranges, so a 10-row page spent 60 upstream
// requests to compute ten guaranteed zeroes.
//
// There is deliberately no equivalent on the prefix branch: prefixUsableSize
// only subtracts network+broadcast below /31, so a prefix's size is at least 2
// (and a container's is a full power of two). The case cannot arise there, and a
// branch that cannot be reached is a branch nobody can check.
// errUnsizeable marks an object utilization simply cannot be derived from: a
// prefix NetBox serialized in a form net/netip will not parse, an IP range whose
// `size` the serializer omitted, or an object type with no utilization at all.
//
// It is kept distinct from a lookup failure because the two blanks mean opposite
// things to the reader. A lookup failure is OUR gap — the number exists, we
// could not fetch it, and a retry may well produce it. errUnsizeable is the
// source's own answer, unchanged by retrying. enrichUtilization reports them as
// two different sentences for that reason.
var errUnsizeable = errors.New("no usable prefix or range size")

func (p *Provider) computeUtilization(ctx context.Context, cost *utilCost, objectType string, raw json.RawMessage) (used, available, util float64, err error) {
	var o utilObject
	if err := json.Unmarshal(raw, &o); err != nil {
		return 0, 0, 0, errUnsizeable
	}

	var size float64
	switch objectType {
	case "ipam/prefixes":
		pfx, err := netip.ParsePrefix(o.Prefix)
		if err != nil {
			return 0, 0, 0, errUnsizeable
		}
		isContainer := o.Status.Value == "container"
		if isContainer {
			size = math.Pow(2, float64(pfx.Addr().BitLen()-pfx.Bits()))
		} else {
			size = prefixUsableSize(pfx, o.IsPool)
		}
		switch {
		case o.MarkUtilized:
			used = size
		case isContainer:
			children, err := p.childPrefixes(ctx, cost, o.Prefix, o.VRF)
			if err != nil {
				return 0, 0, 0, err
			}
			used = unionPrefixSize(children)
		default:
			u, err := p.leafUsedSize(ctx, cost, o.Prefix, o.VRF)
			if err != nil {
				return 0, 0, 0, err
			}
			used = u
		}
	case "ipam/ip-ranges":
		if o.Size == nil {
			return 0, 0, 0, errUnsizeable
		}
		size = *o.Size
		switch {
		case o.MarkUtilized:
			used = size
		case size <= 0:
			used = size // see the "size <= 0" note on computeUtilization
		default:
			start, err1 := netip.ParseAddr(stripMask(o.StartAddress))
			end, err2 := netip.ParseAddr(stripMask(o.EndAddress))
			if err1 != nil || err2 != nil {
				return 0, 0, 0, errUnsizeable
			}
			c, err := p.countContainedIPs(ctx, cost, rangeCIDRs(start, end), o.VRF)
			if err != nil {
				return 0, 0, 0, err
			}
			used = c
		}
	default:
		return 0, 0, 0, errUnsizeable
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
	return used, available, util, nil
}

// wantsUtilization reports whether the requested fields include any util column.
// andListNames renders column names as "a", "a and b", "a, b and c" for a
// sentence a user reads. Deliberately a local copy of pkg/plugin's andList
// rather than an import: this package is the seam a second, non-NetBox backend
// plugs into and does not depend on the plugin layer.
func andListNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// isAre agrees the verb with the number of column names, because these strings
// are read by a person: "used is blank" and "used and available are blank" are
// both right, and "utilization, used and available is blank" is the kind of
// detail that makes a diagnostic look machine-generated and therefore ignorable.
func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// columnWord and itThem agree the rest of the cost sentence with how many
// columns it names, for the same reason isAre exists: this note is read by a
// person deciding whether to keep an expensive column.
func columnWord(n int) string {
	if n == 1 {
		return "column"
	}
	return "columns"
}

func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// selectedUtilizationFields is the computed columns this query actually asked
// for, in the order they are declared. Every sentence the user reads about them
// — the cap, the failure warnings, the cost note — must name these and not the
// full set: projection removes the unselected ones from the result entirely, so
// telling someone their `used`-only alert failed because "utilization, used and
// available" are blank names two columns that are not in their frame and sends
// them looking for a problem they do not have.
//
// Never empty at any call site: enrichUtilization only runs when
// wantsUtilization already said yes. The fallback is defensive, not expected.
func selectedUtilizationFields(fields []string) []string {
	var out []string
	for _, u := range utilizationFieldNames() {
		for _, f := range fields {
			if f == u {
				out = append(out, u)
				break
			}
		}
	}
	if len(out) == 0 {
		return utilizationFieldNames()
	}
	return out
}

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
//
// It returns the sentences describing every row it could not fill BECAUSE
// SOMETHING FAILED, and, separately, a *provider.Cap when it deliberately
// stopped short of the last row. A partial
// answer is the right one here — one prefix whose child walk lost a page should
// not blank the other ninety-nine — but a blank utilization cell renders exactly
// like a genuine zero, and the two carry opposite conclusions: "this prefix is
// empty" versus "we do not know". The upstream this exists for returns 502/503
// under load, so the failing case is routine rather than exotic; five of these
// lookups failed in a single measured 10-row page on NetBox Cloud staging, and
// before this the user was told nothing at all.
//
// The two counts are reported separately because they are not the same problem:
// see errUnsizeable.
//
// It also returns a NOTE stating what the columns cost. Selecting them turns a
// one-request query into one that fans out per row: against NetBox Cloud staging
// a 10-prefix page goes from 1 request and 0.30s to 34 requests and 3.6s
// (medians of six paced samples), and nothing in the UI hinted at that.
//
// Trust the request count, not the clock. 33 extra requests for those ten rows
// is a property of the algorithm and reproduces on every run; the seconds are
// one instance in one state and are only worth quoting as the ratio, ~12x. In
// particular, a timing taken while that upstream is shedding 502s measures the
// retry backoff (0.5s + 1.5s per failed lookup) and not this code at all — that
// is how an earlier sample of this same page came out at 26.7s, seven times the
// figure above, in a run that also lost five lookups. A reader who believed it
// would conclude that ten rows cannot fit inside a 30s query timeout.
//
// The note is INFO, not a warning: the result
// is complete and correct, and the reader is only being told the price of the
// thing they chose. Section 1 of the investigation behind this is why it cannot
// simply be made cheap: NetBox 4.6.4 exposes utilization on NO endpoint —
// neither the prefix list nor its detail serializer nor GraphQL's PrefixType —
// and `?fields=utilization` is accepted with a 200 and silently dropped. There
// is no bulk source to switch to.
func (p *Provider) enrichUtilization(ctx context.Context, objectType string, selected []string, raws []json.RawMessage, flatRows []map[string]interface{}) (notes, warnings []string, capped *provider.Cap) {
	cols := selectedUtilizationFields(selected)
	const workers = 8
	sem := make(chan struct{}, workers)
	var cost utilCost
	var wg sync.WaitGroup
	// Index-aligned with raws, so each goroutine owns its own slot and no two
	// ever write the same one: no lock, and the tallies below are deterministic
	// whatever order the rows finish in.
	failures := make([]error, len(raws))
	deadline := p.utilizationDeadline(ctx)
	// The deadline bounds the WORK, not just the decision to start it. Checking
	// the clock before dispatching a row leaves the row itself unbounded: a wave
	// started a millisecond before the budget runs out still gets the full
	// per-request timeout each, and wg.Wait below waits for all of it, so a 20s
	// budget ran to ~50s — the timeout and the empty panel the bound exists to
	// prevent, arrived at through the bound.
	//
	// Cancelling here rather than per row is what makes the cost bounded: every
	// lookup of every row shares this one deadline, so the whole enrichment ends
	// at it however many rows are in flight.
	measureCtx, cancelMeasure := context.WithDeadlineCause(ctx, deadline, errUtilizationBudget)
	defer cancelMeasure()
	// Rows are dispatched in index order and the loop blocks on the semaphore, so
	// the clock advances here at the rate the upstream actually answers. Once the
	// budget is gone every LATER row is skipped too — dispatch is sequential and
	// the deadline only moves one way — so the unmeasured rows are always a
	// suffix, never a scatter the user would have to hunt for.
	dispatched := 0
	for i := range raws {
		// i > 0: give at least one row the whole budget to answer in, even on an
		// upstream slow enough to have spent it already. One measured row plus an
		// honest cap beats a column of blanks with no evidence of why. (What the
		// row cannot have is more than the budget: an upstream too slow to measure
		// a single row inside the time the user allowed reports zero measured, and
		// that is the true answer.)
		if i > 0 && !time.Now().Before(deadline) {
			break
		}
		dispatched++
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			used, available, util, err := p.computeUtilization(measureCtx, &cost, objectType, raws[i])
			if err != nil {
				if cutByBudget(measureCtx, err) {
					err = errUtilizationBudget
				}
				failures[i] = err
				return
			}
			flatRows[i]["used"] = used
			flatRows[i]["available"] = available
			flatRows[i]["utilization"] = util
		}(i)
	}
	wg.Wait()

	// A row the budget cut short was not measured, so the count that goes to the
	// user has to stop at the first one — Cap.Measured is what the alert message
	// tells them to lower the row limit TO, and a number that still would not fit
	// is advice that fails twice.
	//
	// Everything from there on is reported blank even where the value arrived:
	// the last wave finishes out of order, so one row of it may well beat the
	// deadline while the row before it did not, and keeping that value would
	// scatter measured rows through the blanks. The suffix is worth more than the
	// stray value — it is what lets a reader see where the answer stops, and what
	// makes the row limit an honest dial.
	measured := dispatched
	for i := range dispatched {
		if errors.Is(failures[i], errUtilizationBudget) {
			measured = i
			break
		}
	}
	for i := measured; i < len(raws); i++ {
		for _, col := range utilizationFieldNames() {
			delete(flatRows[i], col)
		}
	}

	var lookupFailed, unsizeable int
	for _, err := range failures[:measured] {
		switch {
		case err == nil:
		case errors.Is(err, errUnsizeable):
			unsizeable++
		default:
			lookupFailed++
		}
	}
	// measured, not dispatched: every count in every sentence the user reads here
	// denominates in the same rows, so the note, the cap and the warnings can be
	// read together. Requests spent on a row the budget then cut are still charged
	// to that total, which overstates the per-row price slightly and only ever in
	// the direction of "these columns are expensive" — the thing the note is for.
	// Nothing measured means no price worth quoting; the cap explains that case on
	// its own.
	if n := cost.requests.Load(); n > 0 && measured > 0 {
		notes = append(notes, fmt.Sprintf(
			"The %s %s cost %d extra NetBox requests for these %d rows: NetBox publishes utilization on no list endpoint, so every row is measured with its own child lookups. Deselect %s for a much faster query.",
			andListNames(cols), columnWord(len(cols)), n, measured, itThem(len(cols))))
	}
	// Reported as a CAP, never as a warning: nothing failed here, and the two
	// blanks call for opposite responses from a reader and from an alert rule.
	// See provider.Result.Capped.
	if measured < len(raws) {
		capped = &provider.Cap{
			Columns:  cols,
			Measured: measured,
			Rows:     len(raws),
		}
	}
	if lookupFailed > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s %s blank for %d of the %d rows measured because the extra NetBox lookups those columns need failed — the values there are missing, not zero. Re-run the query, or narrow it so fewer rows need the lookups.",
			andListNames(cols), isAre(len(cols)), lookupFailed, measured))
	}
	if unsizeable > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s %s blank for %d of the %d rows measured because NetBox reported no usable prefix or range size for them.",
			andListNames(cols), isAre(len(cols)), unsizeable, measured))
	}
	return notes, warnings, capped
}

// childPrefixes lists prefixes contained within cidr (for container
// utilization). Only the CIDR itself is read; see childIPHosts on why the
// projection is requested and why losing it is harmless.
func (p *Provider) childPrefixes(ctx context.Context, cost *utilCost, cidr string, vrf *vrfRef) ([]netip.Prefix, error) {
	q := url.Values{}
	q.Set("within", cidr)
	q.Set("limit", "500")
	q.Set(fieldsParam, "prefix")
	setVRF(q, vrf)
	var out []netip.Prefix
	next := p.client.apiURL("ipam/prefixes", q)
	for next != "" {
		page, err := p.utilPage(ctx, cost, next)
		if err != nil {
			return nil, err
		}
		for _, raw := range page.Results {
			var r struct {
				Prefix string `json:"prefix"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, fmt.Errorf("decode child prefix of %s: %w", cidr, err)
			}
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
