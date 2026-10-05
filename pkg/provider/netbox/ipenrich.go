package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// chunkBudgetBytes caps the encoded length of a batched query string — the
// WHOLE query, not just the repeated parameter it is split on. The measured
// server ceiling is ~8 KB (450 IPv4 addresses returned HTTP 431, as did 160
// IPv6); 6 KB leaves headroom for proxies that cap a URL lower than NetBox does.
// The budget is in BYTES on purpose: an IPv6 address encodes to roughly three
// times an IPv4 one, so a count-based limit tuned on IPv4 would pass every
// IPv4-only test and then fail on the first IPv6-heavy panel.
const chunkBudgetBytes = 6144

// pagingParamBytes reserves the paging parameters no caller can pass to the
// chunker, because they are appended after the batch leaves this file.
//
// TWO of them, and the second is the one that is easy to miss. fetchList adds
// "&limit=<min(limit, pageSize)>" to the first page, which is ours to measure.
// Every page after that is fetched from NetBox's own `next` URL, followed
// verbatim (netbox.go, `next = *page.Next`), and DRF builds that by adding
// "&offset=<n>" to the query it received. So a batch that fits on page 1 can
// still exceed the ceiling on page 2, and nothing in this file constructs that
// URL to notice.
//
// It is reachable, not theoretical: these hops filter by primary key, so a batch
// of more than pageSize ids matches more than one page by construction — a
// projected id batch admits ~883 ids at 6,143 bytes, and page 2 arrives at 6,154.
//
// Both halves are upper bounds rather than guesses. min(limit, pageSize) is
// never wider than pageSize however large a limit the hop asks for; offset never
// exceeds MaxLimit, since fetchList stops walking at that many rows. Reading both
// constants here means a change to either moves the reservation with it.
//
// ?start= is not covered and does not need to be: it is cursor mode, and every
// batched hop goes through fetchRows, which passes cursor=false by design.
var pagingParamBytes = len("&limit=") + len(strconv.Itoa(pageSize)) +
	len("&offset=") + len(strconv.Itoa(MaxLimit))

// chunkByBudget splits values into batches whose encoded "param=value&..." form
// stays within budget bytes, less the paging parameter added downstream. Order
// is preserved and no value is dropped; a single value larger than the budget
// gets a chunk of its own.
//
// budget is the ceiling for the WHOLE query. A caller that sends fixed
// parameters alongside each batch must therefore subtract them — which is what
// queryBatcher does, and why the three ip-enrichment hops go through it instead
// of calling this directly.
func chunkByBudget(param string, values []string, budget int) [][]string {
	budget -= pagingParamBytes

	var out [][]string
	var cur []string
	curLen := 0

	for _, v := range values {
		vLen := len(param) + len(url.QueryEscape(v)) + 2 // "&" + param + "=" + value
		if len(cur) > 0 && curLen+vLen > budget {
			out = append(out, cur)
			cur, curLen = nil, 0
		}
		cur = append(cur, v)
		curLen += vLen
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// queryBatcher splits one hop's values into batches AND builds the query each
// batch is sent as — both from the same fixed-parameter set.
//
// It is a type rather than two loose functions because the budget and the
// request have to agree about what the request contains, and they did not. The
// budget was computed from the repeated parameter alone while every fixed
// parameter was appended afterwards, so the chunker measured something shorter
// than what went on the wire. Measured on the virtual-machine hop, which always
// projects: 893 ids encode to 6,142 bytes of ?id=, one byte inside the budget,
// and to 6,213 once ?fields=, ?exclude= and ?limit= are on them — 69 bytes past
// a ceiling whose entire purpose is headroom below the ~8 KB one NetBox
// enforces. The device hop overflowed identically on the path where it projects.
// The address hop overflowed too, by less: it sends no fixed parameters of its
// own, but the appended ?limit= still put a full batch of 256 max-width IPv4
// addresses 9 bytes over.
//
// Reserving the fixed parameters from the budget would have fixed those three
// and left a fourth parameter free to reintroduce the overflow, because nothing
// would tie the reservation to the request. Here the tie is structural: fixed is
// what query() BUILDS the request from, so a parameter that is not in it is
// never sent, and one that is in it is always measured. The remaining way to
// exceed the budget is a batch of a single oversized value, as it has always
// been, because there is nothing left to split.
type queryBatcher struct {
	param string
	fixed url.Values
}

// newQueryBatcher takes the fixed parameters as an argument rather than leaving
// them to be filled in later: a hop that sends none passes nil and says so at
// the call site, which is where forgetting is visible.
func newQueryBatcher(param string, fixed url.Values) queryBatcher {
	return queryBatcher{param: param, fixed: fixed}
}

// chunk splits values into batches whose query() encodes within
// chunkBudgetBytes.
func (b queryBatcher) chunk(values []string) [][]string {
	return chunkByBudget(b.param, values, chunkBudgetBytes-b.fixedBytes())
}

// query returns the query one batch is sent as: the fixed parameters, plus one
// repeated param per value, in the order given. The fixed values are cloned so
// no caller can append into the set the budget was computed from.
func (b queryBatcher) query(values []string) url.Values {
	q := make(url.Values, len(b.fixed)+1)
	for k, vs := range b.fixed {
		q[k] = slices.Clone(vs)
	}
	for _, v := range values {
		q.Add(b.param, v)
	}
	return q
}

// fixedBytes is what the fixed parameters cost in the encoded query: their own
// encoded length plus the "&" that joins them to the batch's values.
func (b queryBatcher) fixedBytes() int {
	if len(b.fixed) == 0 {
		return 0
	}
	return len(b.fixed.Encode()) + 1
}

// hostOf strips a CIDR mask so a bare input IP ("10.0.0.5") indexes against a
// stored record ("10.0.0.5/24"). Verified live: NetBox's ?address= filter
// matches on the host portion, so bare input is the normal case.
//
// It is a pure string operation and therefore NOT sufficient on its own — see
// canonicalIP, which is what both the request and the index actually use.
func hostOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, "/"); i >= 0 {
		return addr[:i]
	}
	return addr
}

// canonicalIP reduces an address to the ONE spelling that both NetBox's
// ?address= filter and this package's by-host index agree on: the mask is
// dropped and the host is re-rendered by netip, so every legal spelling of the
// same address collapses to the same key.
//
// A pure string comparison is not enough, and the two halves of the bug it fixes
// are independent. IPv6 has many spellings of one address — the same host can
// arrive uppercase, zero-expanded, or with a mask — and:
//
//   - a MASKED non-canonical form ("2001:DB8:85A3::8A2E:370:7334/64") used to be
//     fetched from NetBox successfully and then dropped on the floor, because the
//     record came back indexed under NetBox's canonical spelling while the lookup
//     key kept the caller's. That half was entirely ours.
//   - a BARE non-canonical form ("2001:0db8:85a3:0000:0000:8a2e:0370:7334") never
//     even matched: NetBox's ?address= filter compares literally, so the request
//     itself had to be canonicalised too.
//
// Both output an all-blank row, which docs/RECIPES.md teaches readers to read as
// "external/unknown traffic" — the opposite of the truth for an address NetBox
// holds. Canonicalising in ONE function used by BOTH the request and the index is
// what keeps the two spellings from drifting apart again.
//
// It also fixes mask-mismatched input for free ("10.20.0.1/32" against a stored
// "10.20.0.1/24"): the mask is not part of the identity NetBox matches on, so
// sending the bare host is both correct and batch-order independent. Previously
// such an input resolved only when some other input in the SAME query happened to
// spell the host the way NetBox does.
//
// IPv4 and already-canonical input are unchanged: netip renders them back exactly
// as given. Unmap folds an IPv4-mapped IPv6 literal ("::ffff:10.20.0.1") onto the
// IPv4 address it denotes, which is the same record in NetBox; on anything else it
// is a no-op. Unparseable input (a hostname, a typo, an empty string) falls back
// to the bare hostOf slice, so it behaves exactly as it did before.
func canonicalIP(addr string) string {
	c, _ := canonicalIPOK(addr)
	return c
}

// canonicalIPOK is canonicalIP plus the one extra bit fetchAddressRecords needs:
// whether the value was an address at all. It exists so there is exactly ONE
// notion of validity in this package — "netip parsed it" — shared by the key, the
// request, and the decision not to send one. A separate validator would be free
// to disagree with the canonicalisation and reintroduce the class of bug
// canonicalIP was written to close.
func canonicalIPOK(addr string) (string, bool) {
	host := hostOf(addr)
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().String(), true
	}
	return host, false
}

// logChunkFailure records one degraded batch. Newlines are stripped for the
// same reason pkg/plugin's sanitizeLog does it: an upstream error body must not
// be able to forge extra log lines.
func logChunkFailure(what string, chunkIdx, chunks, ips int, err error) {
	log.DefaultLogger.Warn(
		"ip-enrichment: "+what+" batch failed; the IPs it covers degrade to blank columns",
		"batch", fmt.Sprintf("%d/%d", chunkIdx, chunks),
		"values", ips,
		"error", logSafe(err.Error()),
	)
}

// degradation records how much of one lookup hop failed, so the caller can
// state the gap instead of shipping blank columns that read as absent data.
// A blank device_name means "NetBox has no device for this IP" OR "we could not
// ask" — opposite conclusions — and the log alone cannot tell the dashboard
// author which they are looking at.
type degradation struct {
	// failed is how many of the hop's inputs (IPs, device ids, or fallback
	// lookups) were in a request that failed; total is how many it was asked
	// for. failed == total means the hop failed outright.
	failed int
	total  int
	// cause is the first failure's error, rendered by causeText for the user.
	cause error
}

// any reports whether anything degraded.
func (d degradation) any() bool { return d.failed > 0 }

// record folds one failed request covering n inputs into the tally.
func (d *degradation) record(n int, err error) {
	d.failed += n
	d.noteCause(err)
}

// noteCause keeps the first failure's cause WITHOUT adding to the tally, for a
// hop that can only decide how much actually degraded once every request has
// run. Splitting it out of record keeps the "first cause wins" rule in one
// place: fetchAddressRecords counts afterwards (a host lost by one batch can be
// answered by another), the other two hops count at failure time, and both must
// report the same cause.
func (d *degradation) noteCause(err error) {
	if d.cause == nil {
		d.cause = err
	}
}

// scope renders how much of the hop degraded. "all 12 devices" rather than
// "12 of 12 devices", and "the only IP" rather than "1 of 1 IPs", because those
// forms read as a formatting bug and invite the reader to distrust the number.
// total == 1 implies failed == 1 for any degradation that gets reported, so the
// first case cannot understate anything.
func (d degradation) scope(singular, plural string) string {
	switch {
	case d.total == 1:
		return "the only " + singular
	case d.failed >= d.total:
		return "all " + humanInt(d.total) + " " + plural
	default:
		return humanInt(d.failed) + " of " + humanInt(d.total) + " " + plural
	}
}

// causeText renders an upstream failure as a short reason fit for a panel
// notice. Only the status travels: an APIError also carries the request URL —
// which for these batched hops is ~6 KB of repeated ?address= / ?id=
// parameters — and up to 300 characters of NetBox's raw response body, neither
// of which belongs in a user-facing string. This mirrors what
// pkg/plugin.queryErrorMessage does for errors that reach the user as toasts.
func causeText(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("NetBox returned HTTP %d", apiErr.Status)
	}
	return "NetBox could not be reached"
}

// The four degradation messages below each name the missing COLUMNS and end by
// stating what the blank does NOT mean. vm_name and is_primary_ip are spelled
// out individually because they are the columns with no group prefix to hide
// behind: a reader scanning for "vm_*" would not find one, and since
// is_primary_ip left the device_* group, "device_*" no longer covers the other.
// Both are in the default selection, so a degraded default panel blanks them.
// ruling out the reading a blank column would otherwise invite. "Degraded" on
// its own would tell a dashboard author nothing actionable; which namespace went
// blank, and whether blank means "absent" or "unknown", is the whole point.
// They say "the affected rows" rather than "their" so the sentence stays correct
// whether one IP degraded or a thousand.
// addressHopWarning names match_count and prefix_* alongside the three
// namespaces the hop fills, because when this hop fails those two are affected
// too and they are the ones that mislead. match_count is empty rather than 0 (0
// would assert "NetBox holds no record", which is exactly what is unknown), and
// the prefix fallback is deliberately NOT run for these rows: the fallback's
// precondition is "this IP has no address record", which is not established when
// the lookup never answered.
func addressHopWarning(d degradation) string {
	return fmt.Sprintf(
		"Address lookup failed for %s — %s. The match_count, address_*, interface_*, device_*, vm_name, is_primary_ip and prefix_* columns on the affected rows are empty because the lookup failed, not because NetBox has no record for those IPs.",
		d.scope("IP", "IPs"), causeText(d.cause))
}

// deviceHopWarning names only the columns THIS query selected, which is a
// deliberate departure from the three warnings around it — they enumerate their
// groups unconditionally, and are right to: a hop that fills exactly one column
// group runs only when that group is selected, so its list can never name a
// column the frame lacks.
//
// The device hop stopped being one of those when is_primary_ip gained a
// virtual-machine half and moved out of the device_* group. Its gate is now
// `wantsGroup("device_") || is_primary_ip selected`, so it runs for a selection
// containing only ONE of the two, and a fixed list then names an absent column in
// both directions — proved with a 403 on dcim/devices: selecting [ip,
// is_primary_ip] yielded a frame of exactly those columns under a warning about
// blank device_* ones, and selecting [ip, device_name] the same warning about a
// blank is_primary_ip. The "not because" clause moves with the columns for the
// same reason: with only is_primary_ip in the frame, the misreading to rule out is
// "this address is not its device's primary", not "this IP has no device".
//
// Worth diverging for because a warning is a HARD failure on the Grafana alert
// path (pkg/plugin.degradationError turns any warning into an error, since alert
// evaluation drops frame notices), so this sentence is the first thing an on-call
// reader sees — and one that names a column the panel does not contain sends them
// hunting for the wrong gap.
//
// The two booleans are ResolveIPs' own gate variables rather than a second test
// of `fields` here, so the warning cannot describe a hop the gate did not run.
func deviceHopWarning(d degradation, wantDeviceGroup, wantPrimaryIP bool) string {
	blanked, verb, notBecause := "The device_* and "+isPrimaryIPColumn+" columns", "are", "those IPs have no device"
	switch {
	case !wantDeviceGroup:
		// wantPrimaryIP is necessarily true here — with both false the hop never
		// ran and there is no degradation to report.
		blanked, verb = "The "+isPrimaryIPColumn+" column", "is"
		notBecause = "those IPs are not their device's primary address"
	case !wantPrimaryIP:
		blanked = "The device_* columns"
	}
	return fmt.Sprintf(
		"Device lookup failed for %s — %s. %s on the affected rows %s blank because the lookup failed, not because %s.",
		d.scope("device", "devices"), causeText(d.cause), blanked, verb, notBecause)
}

// vmHopWarning is the VM half of the device warning, and names the one column
// that hop fills. Its "not because" clause is the sharpest of the four: a blank
// is_primary_ip is what a correct FHRP-assigned, unassigned or unknown row looks
// like, so without this sentence a failed lookup is indistinguishable from an
// address whose owner has no primary-IP concept at all.
func vmHopWarning(d degradation) string {
	return fmt.Sprintf(
		"Virtual machine lookup failed for %s — %s. The is_primary_ip column on the affected rows is blank because the lookup failed, not because those IPs are not their virtual machine's primary address.",
		d.scope("virtual machine", "virtual machines"), causeText(d.cause))
}

// addressTruncationWarning states the one gap batch splitting cannot close: a
// SINGLE address with more records than one request can carry (see
// fetchAddressRecords). It is unlike the hop warnings around it, and says
// so — the data is present and true, just not complete. match_count is a floor
// rather than a count, and the pick was made over the records that fit rather
// than all of them, so the row is stable but not necessarily the same row a
// complete read would produce.
//
// The affected addresses are NAMED rather than counted. There cannot be many —
// each one is a single address NetBox holds over MaxLimit records for — and
// unlike the hop warnings, which cover an arbitrary batch, the reader can act on
// this one only if they know which address to go and look at. The list is capped
// anyway, because a panel notice is not a report.
func addressTruncationWarning(hosts []string) string {
	const maxNamed = 3
	named := hosts
	if len(named) > maxNamed {
		named = named[:maxNamed]
	}
	list := strings.Join(named, ", ")
	if rest := len(hosts) - len(named); rest > 0 {
		list += fmt.Sprintf(", and %s more", humanInt(rest))
	}
	subject := humanInt(len(hosts)) + " IPs"
	if len(hosts) == 1 {
		subject = "1 IP"
	}
	return fmt.Sprintf(
		"NetBox holds more than %s address records for %s (%s) — only the first %s were read. match_count is a floor rather than a count for the affected rows, and their address_*, interface_*, device_*, vm_name and is_primary_ip columns describe a record picked from the part that was read.",
		humanInt(MaxLimit), subject, list, humanInt(MaxLimit))
}

func prefixHopWarning(d degradation) string {
	return fmt.Sprintf(
		"Prefix lookup failed for %s — %s. The prefix_* columns on the affected rows are blank because the lookup failed, not because no prefix contains those IPs.",
		d.scope("IP with no address record", "IPs with no address record"), causeText(d.cause))
}

// humanInt formats a count with thousands separators, matching the notices the
// plugin layer already writes (pkg/plugin/notices.go's thousands) — these
// strings land side by side in the same panel header, and "1000" next to
// "1,000" reads as a typo.
func humanInt(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// addressLookup is everything one address hop learned. It is a struct rather
// than a list of returns because the hop reports three independent things about
// the same input set — what it found, what it could not ASK about, and what it
// could not finish READING — and each carries a different conclusion for the
// row. A five-value signature made that unreadable at the call site.
type addressLookup struct {
	// byHost holds every record fetched, keyed by canonicalIP.
	byHost map[string][]json.RawMessage
	// failed names the caller's own spellings that were in a request that
	// failed. Per-spelling, NOT per-host, and deliberately so: it answers "did
	// this ROW get an answer", and ResolveIPs pairs it with an empty bucket.
	failed map[string]bool
	// truncated names the canonical hosts NetBox holds more records for than one
	// request can carry, after batch splitting has done all it can. Their rows
	// are present and their columns are real, but match_count undercounts and
	// the pick was made over a subset.
	truncated []string
	// deg counts the inputs left with no record at all.
	deg degradation
}

// fetchAddressRecords batches ips into ?address= queries and returns every
// matching record indexed by host portion. Pagination is exhausted per batch:
// a truncated page would undercount anycast duplicates and corrupt
// match_count, which is the signal that keeps an ambiguous pick visible.
//
// A failed batch degrades only the IPs it covers, and addressLookup.failed names
// them. That set is not a convenience: without it the per-IP loop cannot tell
// "NetBox has no address record for this IP" from "we never got an answer for
// this IP", and those carry opposite conclusions. Reproduced before the set
// existed: a registered, interface-assigned device IP whose chunk 503'd rendered
// match_count 0 with a containing prefix and nothing else — byte-identical to an
// unregistered IP, and worse than a blank row, because 0 is a positive numeric
// assertion emitted as float64 precisely so users can threshold on it.
//
// Other batches still return, because one transient 500 on a 400-IP flow panel
// must not cost the whole result. The failure is logged for operators AND
// reported in the returned degradation, which ResolveIPs turns into a
// user-facing warning. It escalates to an error only when EVERY batch failed: at
// that point there is no partial answer to give.
//
// The degradation is tallied AFTER every batch has run, not as each one fails,
// and the two are not the same number. Chunking is by the caller's spelling
// while indexing is by canonical host, so two spellings of one address
// ("10.20.0.1" and "10.20.0.1/32") can land in different batches and either one
// answers for both. Counting at failure time reported a host as degraded on a
// result that in fact holds its record — a warning about nothing on a dashboard,
// and on the alert path a REJECTED result (pkg/plugin.degradationError turns any
// warning into an error, by design, because alert evaluation drops frame
// notices). Failing an alert on complete data is the exact inverse of the gap
// this reporting exists to close.
//
// # Overflowing the row cap
//
// One request reads at most MaxLimit records, and a batch can legitimately match
// more: ~350 addresses per batch, each reused across many VRFs or answered by an
// anycast fleet, reaches 10,000 without anything being wrong. NetBox reports the
// true count in the list envelope, so the overflow is detectable — and it used
// to be discarded, which made the loss silent and its symptoms indistinguishable
// from fact: an understated match_count, and any address whose records all fell
// past the cap reported as having no address record at all, which sends it down
// the prefix fallback and out to the user as "not in IPAM".
//
// So an overflowing batch is SPLIT IN HALF and both halves re-queried, until
// each fits. Nothing is indexed from a batch that overflows — the halves re-read
// all of it — so no record can be counted twice. This terminates: every split
// strictly shrinks the batch, and a batch of one address is the floor. Splitting
// also always shrinks the encoded request, so the byte budget still holds.
//
// Only ONE case survives that, and it cannot be split: a SINGLE address with
// more than MaxLimit records of its own. Those hosts are named in
// addressLookup.truncated and stated as a warning, because there is nothing left
// to do but say so. Raising the per-request limit instead would trade a bounded,
// stated gap for an unbounded allocation driven by whatever count NetBox
// reports, which is a worse failure than the one being fixed.
func (p *Provider) fetchAddressRecords(ctx context.Context, ips []string) (addressLookup, error) {
	out := addressLookup{
		byHost: make(map[string][]json.RawMessage, len(ips)),
		failed: make(map[string]bool),
		deg:    degradation{total: len(ips)},
	}

	// A value netip cannot parse is never SENT. It is not an error condition and
	// is not reported as one: the row is built exactly as before, with
	// match_count 0 and blank columns, because that is what the answer would have
	// been anyway.
	//
	// It could never have matched. Every record is indexed under
	// canonicalIP(record.address), which is netip's own rendering and therefore
	// always parses; an unparseable input keeps its raw spelling as its key, and a
	// string netip rejects is not a string netip prints. The two can never be
	// equal, so a malformed value's bucket is empty whether or not NetBox was
	// asked. (Asked is also harmless — NetBox 4.4.10 answers an invalid ?address=
	// with HTTP 200 and count 0, verified across twelve malformed classes and in a
	// batch mixed with a valid address — so this is not error isolation.)
	//
	// What it does cost is the byte budget. Junk in an interpolated $flow_ips
	// variable consumes chunk space that legitimate addresses then have to spill
	// out of, turning one request into two and pushing the encoded query nearer
	// the ~8 KB server ceiling, for a match that cannot happen.
	//
	// A skipped value can therefore never appear in out.failed, and out.deg.total
	// deliberately still counts it: the hop is responsible for one row per input
	// IP, and a denominator that quietly shrank would not match the row count the
	// reader is looking at.
	sendable := make([]string, 0, len(ips))
	for _, ip := range ips {
		if _, ok := canonicalIPOK(ip); ok {
			sendable = append(sendable, ip)
		}
	}

	// Chunked on the caller's spellings but SENT as canonical ones. Every
	// canonical form is at most as long as the input it came from (a mask is
	// dropped, an expanded IPv6 compresses, uppercase stays the same length, an
	// IPv4-mapped literal folds onto the IPv4 address), and that survives escaping
	// too — the dropped "/" and the dropped colons were the three-byte escapes —
	// so the budget computed from the input is a valid upper bound FOR THE
	// ADDRESSES. It was never an upper bound for the REQUEST: this hop sends no
	// fixed parameters of its own, but fetchList still appends ?limit=, which put
	// a full batch of 256 max-width IPv4 addresses 9 bytes past the budget.
	// queryBatcher is what accounts for that, here and in the two id hops where
	// the same gap was 69 bytes wide.
	batcher := newQueryBatcher("address", nil)
	chunks := batcher.chunk(sendable)

	// A stack, not a range over chunks, so an overflowing batch can push its two
	// halves back on. Seeded in reverse so the first chunk is requested first:
	// request order is otherwise unobservable, and keeping it in input order
	// keeps failures reproducible.
	type batch struct {
		ips []string
		idx int // 1-based index of the top-level chunk, for the operator log
	}
	work := make([]batch, 0, len(chunks))
	for i := len(chunks) - 1; i >= 0; i-- {
		work = append(work, batch{ips: chunks[i], idx: i + 1})
	}

	// Counted per terminal batch rather than per chunk: a chunk that split has
	// no outcome of its own, only its halves do. "Every batch failed" is
	// therefore "none succeeded and at least one failed", which is the same
	// condition as before whenever no split happened.
	okBatches, failedBatches := 0, 0

	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]

		// Canonicalised here rather than at chunk time because the bookkeeping
		// above is per caller spelling; the query is built by the same batcher
		// that measured the budget, so what is sent is what was measured.
		canon := make([]string, 0, len(b.ips))
		for _, ip := range b.ips {
			canon = append(canon, canonicalIP(ip))
		}
		// MaxLimit, not len(b.ips): anycast means a batch can match more
		// records than addresses requested. total is what NetBox says exists.
		raws, total, err := p.fetchRows(ctx, "ipam/ip-addresses", batcher.query(canon), MaxLimit)
		if err != nil {
			failedBatches++
			out.deg.noteCause(err)
			for _, ip := range b.ips {
				out.failed[ip] = true
			}
			logChunkFailure("address lookup", b.idx, len(chunks), len(b.ips), err)
			continue
		}
		if total > len(raws) && len(b.ips) > 1 {
			mid := len(b.ips) / 2
			work = append(work,
				batch{ips: b.ips[mid:], idx: b.idx},
				batch{ips: b.ips[:mid], idx: b.idx})
			continue
		}

		okBatches++
		for _, raw := range raws {
			var o struct {
				Address string `json:"address"`
			}
			if json.Unmarshal(raw, &o) != nil {
				continue
			}
			// Same function as the request side: index and lookup key cannot
			// drift apart, which is how a successfully fetched record used to be
			// discarded.
			h := canonicalIP(o.Address)
			out.byHost[h] = append(out.byHost[h], raw)
		}
		if total > len(raws) {
			// len(b.ips) == 1 by the branch above: one address, more records
			// than a request can carry, nothing left to split.
			host := canonicalIP(b.ips[0])
			out.truncated = append(out.truncated, host)
			log.DefaultLogger.Warn(
				"ip-enrichment: address lookup hit the row cap for a single address; match_count undercounts it",
				"address", logSafe(host), "read", len(raws), "reported", total, "cap", MaxLimit,
			)
		}
	}

	// Only an input left with NOTHING in its bucket actually degraded. `failed`
	// itself keeps its per-spelling semantics untouched — the row-level test in
	// ResolveIPs is `addrFailed[ip] && len(cands) == 0`, and it needs to know
	// which spellings were in a failed request even when the host recovered.
	// This is the same recovery, applied to the number that drives the warning
	// and the alert-path rejection.
	for ip := range out.failed {
		if len(out.byHost[canonicalIP(ip)]) == 0 {
			out.deg.failed++
		}
	}

	if failedBatches > 0 && okBatches == 0 {
		return out, out.deg.cause
	}
	return out, nil
}

// The three tiers pickAddress ranks an address record by. Spread apart so the
// status tie-break below (+1) can only ever order records WITHIN a tier: an
// assignment must never be outvoted by a status, because only an assignment can
// carry identity.
const (
	rankInterfaceAssigned = 4
	rankOtherAssigned     = 2
)

// pickAddress applies the documented tie-break when one address matches
// several records (anycast, VRF overlap, or a VIP shared across a pair):
// interface-assigned first, then any other assignment, then non-deprecated,
// then lowest id. The caller reports the candidate count via match_count so the
// pick is never silent.
//
// The top tier tests assigned_object_TYPE, not merely that assigned_object is
// non-null, and that distinction is the entire point of the ranking.
// assigned_object is a GENERIC relation — NetBox also assigns addresses to FHRP
// groups (see isInterfaceAssignment) — and ONLY an interface assignment can fill
// interface_* or device_*. Ranking on non-nullness tied an FHRP-assigned record
// with a dcim.interface-assigned one for the same host, and the lowest-id
// fallback then handed the row to whichever was created first; when that was the
// FHRP record, applyAddressColumns and deviceIDFromAddress both (correctly)
// declined it and the row came back with BLANK identity columns while a
// device-backed candidate sat unused in the same bucket. Reusing THEIR constants
// via isInterfaceAssignment is what stops a third notion of "assigned" drifting
// away from the two that consume the pick.
//
// A non-interface assignment still edges out no assignment at all. Neither can
// name an interface or a device, and the address_* columns come from the record
// itself either way, so the choice costs nothing in identity terms — but an
// assigned record documents an address something is actually using, while an
// unassigned one may be a bare reservation, which makes preferring it the better
// default. It also leaves the pick unchanged for the case this rule is not about
// (an FHRP record against an unassigned one), so the only behaviour that moves is
// the one that was wrong.
//
// Assignment outranks status, as it always has: an interface-assigned but
// deprecated record still yields the device and the interface, with
// address_status carrying "Deprecated" in plain sight, which is strictly more
// than an active record that can name neither.
func pickAddress(cands []json.RawMessage) json.RawMessage {
	var best json.RawMessage
	bestRank, bestID := -1, 0

	for _, raw := range cands {
		var o struct {
			ID     int `json:"id"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
			Type           string          `json:"assigned_object_type"`
			AssignedObject json.RawMessage `json:"assigned_object"`
		}
		if json.Unmarshal(raw, &o) != nil {
			continue
		}
		rank := 0
		switch {
		case isInterfaceAssignment(o.Type):
			rank += rankInterfaceAssigned
		case hasAssignment(o.AssignedObject):
			rank += rankOtherAssigned
		}
		if o.Status.Value != "deprecated" {
			rank++
		}
		if rank > bestRank || (rank == bestRank && o.ID < bestID) {
			best, bestRank, bestID = raw, rank, o.ID
		}
	}
	return best
}

// hasAssignment reports whether an address record is assigned to ANY object,
// whatever kind. This is the middle tier only: it is deliberately not a test of
// whether the assignment means anything for interface_* or device_*, which is
// isInterfaceAssignment's job and is checked first.
func hasAssignment(assigned json.RawMessage) bool {
	return len(assigned) > 0 && string(assigned) != "null"
}

// isPrimaryIPColumn is the column both identity hops fill. It is a constant
// rather than a literal because the SPELLING is shared by places that must
// agree: the two setters, the type declaration, and the gate in ResolveIPs that
// decides whether either hop runs at all. Carrying no namespace prefix, it is
// outside the wantsGroup prefix test that switches the device_* and prefix_*
// groups on, so the gate names this column explicitly — and a typo there would
// turn the column off for every query while a test asserting on a setter's own
// output still passed.
const isPrimaryIPColumn = "is_primary_ip"

// isPrimaryIP reports whether ipID is the primary address of the object the
// address is assigned to — a dcim.device or a virtualization.virtualmachine.
// This is the join key that lines a flow IP up with the SNMP polls of the thing
// that owns it, so it is an explicit column rather than something consumers
// infer. Both models expose primary_ip4/primary_ip6 with identical semantics, so
// one function reads both (verified against NetBox 4.4.10's /api/schema/).
//
// # Why an FHRP-assigned address gets NO value, not false
//
// The third assignment kind this file knows, ipam.fhrpgroup, has no primary_*
// field of any kind: FHRPGroup's entire property set is auth_key, auth_type,
// comments, created, custom_fields, description, display, display_url, group_id,
// id, ip_addresses, last_updated, name, protocol, tags, url (NetBox 4.4.10
// /api/schema/). A group HOLDS addresses; NetBox never elects one as primary.
//
// So the column is left ABSENT for those rows, and for every other assignment
// kind a later NetBox may add. Blank means "this kind of assignment has no
// primary-IP concept"; false would mean "it has one, and this address is not
// it". Emitting false for an FHRP address is a confidently wrong answer to a
// question NetBox does not ask — the same failure class as the FHRP group that
// once came back as an interface_name (see isInterfaceAssignment), and worse
// than a blank column for the same reason.
//
// The flag was asked for on FHRP groups too. It is deliberately not extended to
// them: do not "complete" it by adding a false here or an else-branch at the
// call sites. address_assigned_object_type is the column that tells an
// FHRP-assigned row apart from an unassigned one.
func isPrimaryIP(owner map[string]interface{}, ipID int) bool {
	for _, key := range []string{"primary_ip4_id", "primary_ip6_id"} {
		if f, ok := owner[key].(float64); ok && int(f) == ipID {
			return true
		}
	}
	return false
}

// fetchDevices batches device ids and returns flattened device columns by id.
// Required because assigned_object embeds only the brief interface form,
// whose nested device carries just {id,name,url,display,description} — no
// site, role, tenant or platform.
//
// Chunk failures degrade the same way fetchAddressRecords' do: the devices in
// the failed batch are simply absent from the map, so their rows keep address
// and interface context and lose only device_*.
//
// # What it asks NetBox for
//
// wantDeviceColumns says whether any device_* column is selected, and it is the
// same boolean ResolveIPs gates the hop with, passed in rather than re-derived so
// the request cannot disagree with the reason it was made.
//
// True means the whole device serializer, unprojected. device_* is nine columns
// spread across most of it (name, role, platform, device_type, site, location,
// rack, tenant, status), so a projection would save little and risk much:
// ?fields= is SILENT about a name it does not recognize (see fieldsParam), so one
// wrong name returns a 200 with that column quietly blank for every row.
//
// False means the hop is running for is_primary_ip alone — the case that
// appeared when the flag stopped being a device_* column and the hop started
// running for selections holding no device_* column at all, where the full
// serializer was fetched, decoded and thrown away for one boolean. Then it asks
// for just what that boolean is computed from. Measured against NetBox 4.4.10,
// ?id=1&id=2&id=3 on dcim/devices: 6,378 bytes whole against 731 projected, an
// 8.7x cut, with the envelope count identical either way.
//
// Safe because nothing else reads this map. ResolveIPs passes it to
// applyDeviceColumns and to nobody else, and that function reads exactly the nine
// device_* sources — which the projection is only ever dropped for — plus
// isPrimaryIP's two derived keys, which it keeps. `id` is in it because the map is
// keyed on it, and a device that lost its id would vanish from the result with no
// error and no warning.
//
// There is no error return, and that is the point. By spec a device-hop failure
// — partial OR total — never fails the query, so an error here would have no
// consumer and the only honest thing the caller could do with it is discard it.
// It used to, as `devices, _ := p.fetchDevices(...)`, and that silent discard is
// exactly how a total failure came out looking like "these IPs have no device".
// The degradation return carries everything a caller needs, including the cause,
// so nothing is left to swallow: deg.failed == deg.total means the hop failed
// outright.
func (p *Provider) fetchDevices(ctx context.Context, ids []int, wantDeviceColumns bool) (map[int]map[string]interface{}, degradation) {
	out := make(map[int]map[string]interface{}, len(ids))
	deg := degradation{total: len(ids)}
	if len(ids) == 0 {
		return out, deg
	}

	strs := make([]string, 0, len(ids))
	for _, id := range ids {
		strs = append(strs, fmt.Sprintf("%d", id))
	}

	// nil when device_* is selected, which is what leaves the request
	// unprojected. Built ONCE and handed to the batcher, which measures it against
	// the budget and builds every request from it, so the query that was costed
	// and the query that is sent cannot be different queries.
	var fixed url.Values
	if !wantDeviceColumns {
		fixed = primaryIPProjection()
	}

	batcher := newQueryBatcher("id", fixed)
	chunks := batcher.chunk(strs)
	for i, chunk := range chunks {
		// The reported total is discarded here, and unlike the address hop that
		// is safe rather than an oversight. ?id= is an exact-match filter on the
		// primary key, so a batch matches at most one device per id it names —
		// verified live against NetBox 4.4.10: ?id=1&id=1&id=2&id=3 returns
		// count 3, so repeats collapse and an absent id contributes nothing. The
		// byte budget caps a batch at ~870 ids (~610 for six-digit ones), an
		// order of magnitude below MaxLimit, so this response cannot overflow
		// the cap the way an anycast-heavy address batch can.
		raws, _, err := p.fetchRows(ctx, "dcim/devices", batcher.query(chunk), MaxLimit)
		if err != nil {
			deg.record(len(chunk), err)
			logChunkFailure("device lookup", i+1, len(chunks), len(chunk), err)
			continue
		}
		for _, raw := range raws {
			_, vals, err := flattenObject(raw)
			if err != nil {
				continue
			}
			if v, ok := vals["id"].(float64); ok {
				out[int(v)] = vals
			}
		}
	}
	return out, deg
}

// primaryIPFields is the projection a hop sends when is_primary_ip is the only
// thing it is being asked for. It names the three properties that produce the
// flag and nothing else: id to key the map, primary_ip4 and primary_ip6 for
// isPrimaryIP, which reads the primary_ip4_id/primary_ip6_id that flattenObject
// derives from those nested objects.
//
// ONE list for both models, because isPrimaryIP is one function for both: a
// device and a virtual machine expose primary_ip4/primary_ip6 with identical
// semantics (NetBox 4.4.10 /api/schema/), so a per-hop copy could only ever drift
// from the reader that consumes it. The VM hop always sends it — the flag is the
// only reason that hop exists — while the device hop sends it only when no
// device_* column was selected; see fetchDevices.
//
// Verified against NetBox 4.4.10 on both endpoints: ?fields=id,primary_ip4,
// primary_ip6 answers with exactly those three keys and an unchanged envelope
// count. A projection is SILENT about names it does not know (see fieldsParam),
// so these three are named the way the flattener will read them rather than the
// way the column is spelled.
var primaryIPFields = []string{"id", "primary_ip4", "primary_ip6"}

// primaryIPProjection is the FIXED parameter set a hop sends when is_primary_ip
// is all it needs from the object: the ?fields= projection, and the
// ?exclude=config_context that travels with it.
//
// The exclusion is free in bytes — the projection already leaves config_context
// out of the response — and not free upstream: DeviceViewSet and
// VirtualMachineViewSet both annotate config context onto the queryset, which a
// projection alone does not remove. It is sent ONLY alongside the projection,
// for the reason setExcludeConfigContext gives: config_context is a real column
// an unprojected device query surfaces.
//
// One function for both hops rather than a copy each, for the same reason
// primaryIPFields is one list — and one more: this is the set queryBatcher
// charges against the byte budget, so a parameter added to one hop's copy and
// not the other's would make one of the two budgets describe a request nobody
// sends.
func primaryIPProjection() url.Values {
	q := url.Values{}
	projection := projectionValue(nil, primaryIPFields)
	if projection == "" {
		return q
	}
	q.Set(fieldsParam, projection)
	setExcludeConfigContext(q, projection)
	return q
}

// fetchVMs batches virtual machine ids and returns the flattened objects by id,
// for the one column a VM-assigned row cannot get for free: is_primary_ip.
//
// It is fetchDevices' mirror image and keeps every one of its properties, for
// the reasons stated there — batching by id within the same byte budget, one
// map for every batch, and NO error return, because a VM-hop failure (partial or
// total) must never fail the query. The degradation it returns instead is what
// lets ResolveIPs say the column is blank because the lookup failed rather than
// because the address is not a primary.
//
// It exists at all because the address payload cannot answer the question. The
// nested virtual_machine object inside a vminterface assignment is the BRIEF
// serializer form — {id,url,display,name,description}, verified live on 4.4.10 —
// so vm_name rides along free (see vmNameFromAddress) while primary_ip4 does
// not appear in it at any depth. That asymmetry is the whole reason this hop is
// gated separately from vm_name's.
func (p *Provider) fetchVMs(ctx context.Context, ids []int) (map[int]map[string]interface{}, degradation) {
	out := make(map[int]map[string]interface{}, len(ids))
	deg := degradation{total: len(ids)}
	if len(ids) == 0 {
		return out, deg
	}

	strs := make([]string, 0, len(ids))
	for _, id := range ids {
		strs = append(strs, fmt.Sprintf("%d", id))
	}

	// Always projected — the flag is the only reason this hop exists — so unlike
	// fetchDevices there is no unprojected path. The batcher is given the same
	// parameter set the request is built from; see queryBatcher for what used to
	// go wrong when the budget and the request were computed separately.
	batcher := newQueryBatcher("id", primaryIPProjection())
	chunks := batcher.chunk(strs)
	for i, chunk := range chunks {
		// The reported total is discarded for the same reason fetchDevices
		// discards it: ?id= is an exact-match filter on the primary key, so a
		// batch matches at most one VM per id it names and the byte budget caps a
		// batch far below MaxLimit.
		raws, _, err := p.fetchRows(ctx, "virtualization/virtual-machines", batcher.query(chunk), MaxLimit)
		if err != nil {
			deg.record(len(chunk), err)
			logChunkFailure("virtual machine lookup", i+1, len(chunks), len(chunk), err)
			continue
		}
		for _, raw := range raws {
			_, vals, err := flattenObject(raw)
			if err != nil {
				continue
			}
			if v, ok := vals["id"].(float64); ok {
				out[int(v)] = vals
			}
		}
	}
	return out, deg
}

// IPEnrichColumns lists every column ip-enrichment can emit, in group order.
// "ip", "match_count" and "is_primary_ip" are deliberately not namespaced: "ip"
// is the documented Grafana join key, match_count describes the row itself, and
// is_primary_ip describes the address's relationship to whatever it is assigned
// to — a device for one row and a virtual machine for the next, so no single
// namespace could name it honestly (it was device_is_primary_ip until it gained
// a virtual-machine half).
// There is no prefix_site — NetBox 4.2 replaced a prefix's `site` with the
// generic `scope` (site, region or location), which is why 4.2 and not 4.1 is
// the supported floor (README Requirements); 4.1 has no `scope` at all, so this
// column would be blank there for every result.
// Only interface_name and interface_description are present, and the reason
// differs by assignment kind. For a dcim.interface, assigned_object is the brief
// interface form and carries nothing else worth surfacing. A
// virtualization.vminterface carries one thing more — a nested virtual_machine
// object — and that is where vm_name comes from; it costs nothing, because the
// address hop already has the payload.
// vm_name is the ONLY vm_* column. It is deliberately not device_name: a VM id
// and a device id live in different NetBox models, so reusing the device
// keyspace would collide, and NetBox permits a device and a VM with the same
// name, which would make the documented device_name → Prometheus `device` join
// match the wrong thing. Per row at most one of vm_name and device_name is set,
// never both — and both are blank whenever the address is assigned to neither
// (an ipam.fhrpgroup, or nothing at all), as they are on a prefix-fallback row;
// see deviceIDFromAddress and vmNameFromAddress.
// address_assigned_object_type carries the raw NetBox relation string
// (dcim.interface, virtualization.vminterface, ipam.fhrpgroup), never a
// friendly label: unlike address_status, assigned_object_type is a plain
// string with no separate label field, so raw is the only representation
// there is. It is what lets a caller tell an FHRP-assigned address apart from
// a wholly unassigned one — both otherwise leave interface_* and device_*
// equally blank.
// There is no address_nat_inside_dns/address_nat_outside_dns: NetBox 4.4's
// nested NestedIPAddress serializer (used for both nat_inside and every
// nat_outside element) has no dns_name property at all — verified against the
// OpenAPI schema and live, including a patch-and-refetch that ruled out the
// field being merely omitted when empty. Filling it would need a second API
// call per referenced NAT partner; ruled: not worth the extra request, same
// as VM DEVICE-GRADE enrichment (a VM's cluster, site, role, platform or
// status) and the dropped interface_* columns. The VM's NAME is not in that
// ruling: it is embedded in the address payload already, so it costs nothing
// and is shipped as vm_name.
// is_primary_ip is the one VM attribute that IS worth a request, and the only
// column here that costs one for a VM-assigned row: it is the SNMP/flow join
// key ("is this the address the poller talks to"), it is in the default
// selection, and NetBox's nested virtual_machine object is the brief serializer
// form — {id,url,display,name,description}, verified live on 4.4.10 — so unlike
// vm_name it cannot be read off the address payload. Its hop is gated on the
// column itself (see ResolveIPs), so a query that does not select it pays
// nothing.
func IPEnrichColumns() []string {
	return []string{
		"ip", "match_count", "is_primary_ip",
		"prefix_cidr", "prefix_scope", "prefix_tenant", "prefix_role",
		"prefix_vrf", "prefix_vlan", "prefix_description",
		"address_dns_name", "address_status", "address_role", "address_vrf",
		"address_tenant", "address_description", "address_assigned_object_type",
		"address_nat_inside", "address_nat_outside",
		"interface_name", "interface_description",
		"device_name", "device_role", "device_platform", "device_device_type",
		"device_site", "device_location", "device_rack", "device_tenant",
		"device_status",
		"vm_name",
	}
}

// defaultIPEnrichFields is what a new query selects. match_count is included
// on purpose: an ambiguous pick must be visible out of the box.
var defaultIPEnrichFields = []string{
	"ip", "match_count", "address_dns_name", "device_name", "vm_name", "interface_name",
	"is_primary_ip", "device_site", "device_tenant",
}

// DefaultIPEnrichFields returns the selection ResolveIPs substitutes for an
// empty field list — which is what the query editor sends until the user first
// changes the multi-select, so it is the common case rather than an edge one.
//
// It is exported for the plugin layer, which has to know the EFFECTIVE selection
// before the call in order to work out whether a join key's source field is
// already coming back (pkg/plugin.ipEnrichFields). Reading the same variable
// ResolveIPs reads is the point: a second copy of this list at the plugin layer
// could disagree with this one and silently un-request a column. A clone,
// because a package-level slice handed out by value is a package-level slice the
// caller can rewrite.
func DefaultIPEnrichFields() []string { return slices.Clone(defaultIPEnrichFields) }

// ipEnrichColumnTypes declares the two ip-enrichment columns that are not
// strings. Unlike an object query's columns — discovered from NetBox at runtime,
// so only the values can say what they are — this schema is fixed here in code,
// which means the type of a column is known even when the current IP set puts no
// value in it. The plugin layer would otherwise scan the values and fall back to
// string for an all-null column, so is_primary_ip alternated between a boolean
// field and a string one depending on whether any IP in the refresh happened to
// resolve a device. See provider.Result.ColumnTypes.
//
// Answering the flag from either owner made that all-null column ordinary rather
// than exceptional. The flag is emitted only for a device- or VM-assigned
// address, so an FHRP-only, an unassigned-only or an external-address refresh
// puts no value in it at all — the very shape whose type nothing but this
// declaration can state.
//
// Every other column flattens to a string, which is also the inference fallback,
// so listing them would change nothing and only invite drift.
var ipEnrichColumnTypes = map[string]provider.FieldType{
	"match_count":     provider.FieldTypeNumber,
	isPrimaryIPColumn: provider.FieldTypeBoolean,
}

// declaredColumnTypes returns the type declarations for the fields this query
// actually returns, so Result.ColumnTypes never describes a column the frame
// does not have. nil when none of them are selected.
func declaredColumnTypes(fields []string) map[string]provider.FieldType {
	var out map[string]provider.FieldType
	for _, f := range fields {
		t, ok := ipEnrichColumnTypes[f]
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]provider.FieldType, len(ipEnrichColumnTypes))
		}
		out[f] = t
	}
	return out
}

// wantsGroup reports whether any requested field belongs to a namespaced column
// group ("device_", "prefix_"). Both groups are filled by a lookup hop that
// exists only to fill them, so this is the test for whether the hop is worth
// making at all: project() drops every column the caller did not ask for, and a
// hop whose entire output is then discarded is pure latency.
//
// It is NOT the whole gate, and stopped being it once is_primary_ip existed
// outside both groups. It has no namespace to test — it is answered by the
// device for one row and by the VM for the next — so ResolveIPs ORs this against
// an explicit test for that name.
// A hop gate that reads wantsGroup alone would leave the default selection's
// is_primary_ip permanently blank.
//
// Prefix matching is exact-by-construction: every column in IPEnrichColumns
// beginning with "device_" or "prefix_" comes from that group's hop, and the
// three non-namespaced columns ("ip", "match_count", "is_primary_ip") belong to
// neither group — the last of them needing the explicit test above.
//
// "vm_" is NOT a group here, alongside "address_" and "interface_": vm_name is
// read off the address payload this query already holds, so there is no hop to
// skip. The VM hop is gated on is_primary_ip by name for exactly that reason —
// gating it on the "vm_" prefix would make the free column start paying for a
// request, and gating it on the whole selection would make every ip-enrichment
// query pay for a column nobody asked for.
func wantsGroup(fields []string, group string) bool {
	for _, f := range fields {
		if strings.HasPrefix(f, group) {
			return true
		}
	}
	return false
}

// distinctAddressCount counts how many DISTINCT NetBox records a host matched,
// which is what match_count means. len() would double-count: input IPs are
// indexed by host portion, so "10.0.0.5" and "10.0.0.5/24" share a bucket, and
// when they land in different chunks each chunk's response contributes the same
// record — reporting match_count 2 for a host that has exactly one record and
// falsely flagging an unambiguous pick as ambiguous. Records with no id (a
// malformed or partial payload) cannot be deduped and are each counted, which
// keeps the count an upper bound rather than silently hiding candidates.
func distinctAddressCount(raws []json.RawMessage) int {
	seen := make(map[int]bool, len(raws))
	n := 0
	for _, raw := range raws {
		id := addressID(raw)
		if id == 0 {
			n++
			continue
		}
		if !seen[id] {
			seen[id] = true
			n++
		}
	}
	return n
}

// addressID returns the NetBox id of an address record, or 0.
func addressID(raw json.RawMessage) int {
	var o struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return 0
	}
	return o.ID
}

// The assigned_object_type values NetBox uses for the two interface kinds an IP
// can be assigned to. assigned_object is a GENERIC relation, not an interface
// one: NetBox also assigns addresses to FHRP groups (ipam.fhrpgroup), and a
// later release can add more targets without any change here. Every read of
// assigned_object must therefore test the type first.
//
// Proved live against NetBox 4.4.10: an address assigned to an FHRP group came
// back with interface_name set to the group's display string
// ("zz-probe-fhrp VRRPv3: 991 (10.77.77.77/24)") — a VRRP group presented as a
// switch port, which is worse than a blank column because it is confidently
// wrong. The constants are shared by the two gates below so a NetBox rename
// cannot fix one and leave the other reading a stale literal.
const (
	assignedTypeInterface   = "dcim.interface"
	assignedTypeVMInterface = "virtualization.vminterface"
)

// isInterfaceAssignment reports whether assigned_object is an interface of
// either kind, and therefore whether its display string is an interface name.
//
// Deliberately WIDER than deviceIDFromAddress's gate, which takes dcim.interface
// alone: a VM interface is a real interface (interface_name populates) but has
// no NetBox device (device_* stays blank), and that asymmetry is the documented
// contract in docs/RECIPES.md. Two different questions, one set of constants.
//
// A VM-assigned row is still named, just not in device_name: its identity lands
// in vm_name, filled by vmNameFromAddress off the same payload.
func isInterfaceAssignment(objectType string) bool {
	return objectType == assignedTypeInterface || objectType == assignedTypeVMInterface
}

// deviceIDFromAddress returns the owning device id for an address assigned to
// a dcim.interface. VM interfaces return false: device_* stays blank rather
// than mislabelling a virtual machine as a device — see vmNameFromAddress,
// which is where a VM-assigned row is named instead, so this exclusion is a
// rule rather than an omission.
func deviceIDFromAddress(raw json.RawMessage) (int, bool) {
	var o struct {
		Type           string `json:"assigned_object_type"`
		AssignedObject struct {
			Device struct {
				ID int `json:"id"`
			} `json:"device"`
		} `json:"assigned_object"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Type != assignedTypeInterface {
		return 0, false
	}
	if o.AssignedObject.Device.ID == 0 {
		return 0, false
	}
	return o.AssignedObject.Device.ID, true
}

// vmIDFromAddress returns the owning virtual machine id for an address assigned
// to a virtualization.vminterface, and is what the VM hop batches on. Device
// interfaces return false, exactly as deviceIDFromAddress excludes VM ones: the
// two ids come from different NetBox models and index different maps, so a
// device id reaching the VM hop would look up an unrelated object and answer
// is_primary_ip from it.
//
// Separate from vmNameFromAddress, which reads the same nested object, because
// the two have different failure conditions and neither should inherit the
// other's. A VM with no name is still a VM whose primary IP can be looked up,
// and an id of 0 is not a VM at all however well-named it is.
func vmIDFromAddress(raw json.RawMessage) (int, bool) {
	var o struct {
		Type           string `json:"assigned_object_type"`
		AssignedObject struct {
			VirtualMachine struct {
				ID int `json:"id"`
			} `json:"virtual_machine"`
		} `json:"assigned_object"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Type != assignedTypeVMInterface {
		return 0, false
	}
	if o.AssignedObject.VirtualMachine.ID == 0 {
		return 0, false
	}
	return o.AssignedObject.VirtualMachine.ID, true
}

// vmNameFromAddress returns the owning virtual machine's name for an address
// assigned to a virtualization.vminterface. It is deviceIDFromAddress' mirror
// image: the same generic-relation payload, read through the OTHER type
// constant, and it is what fills vm_name.
//
// It costs no request. NetBox's address serializer embeds the whole nested
// virtual_machine object ({id,url,display,name,description}) inside
// assigned_object, and the address hop already fetched it — flattenObject merely
// collapses a nested object to its display/id/slug and drops what is inside,
// which is the same reason interface_description is read off the raw JSON here.
//
// Standalone rather than inlined at the one call site, for the reason
// TestDeviceIDFromAddress states about its twin: the TYPE GATE has to be
// assertable on its own, independently of whether the payload happens to carry
// the field. A dcim.interface payload that grew a virtual_machine key must still
// yield nothing here.
//
// The NAME, never .display. They are equal today for a virtual machine, but
// display is a rendering — the FHRP mislabelling this file documents came from
// trusting one — and only name is the value the rest of NetBox joins on.
func vmNameFromAddress(raw json.RawMessage) (string, bool) {
	var o struct {
		Type           string `json:"assigned_object_type"`
		AssignedObject struct {
			VirtualMachine struct {
				Name string `json:"name"`
			} `json:"virtual_machine"`
		} `json:"assigned_object"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Type != assignedTypeVMInterface {
		return "", false
	}
	if o.AssignedObject.VirtualMachine.Name == "" {
		return "", false
	}
	return o.AssignedObject.VirtualMachine.Name, true
}

// applyAddressColumns fills address_* from one address record, and interface_*
// too when — and only when — the record is assigned to an interface.
// description on assigned_object is read straight off the raw JSON
// rather than through flattenObject: flattenObject's generic nested-object
// rule only ever surfaces a nested object's display/id/slug, so it silently
// drops every other field a specific relationship carries — here, the
// interface's own description. Reading the raw JSON directly needs no extra
// HTTP request.
//
// nat_inside/nat_outside's dns_name is deliberately NOT surfaced: NetBox
// 4.4's NestedIPAddress serializer (used for both) has no dns_name property,
// so filling it would need a second API call per referenced NAT partner —
// ruled not worth it. address_nat_inside/address_nat_outside (the peer
// address's own display string, via flattenObject below) remain: that part
// is real and useful.
func applyAddressColumns(row map[string]interface{}, raw json.RawMessage) {
	_, vals, err := flattenObject(raw)
	if err != nil {
		return
	}
	for src, dst := range map[string]string{
		"dns_name":    "address_dns_name",
		"status":      "address_status",
		"role":        "address_role",
		"vrf":         "address_vrf",
		"tenant":      "address_tenant",
		"description": "address_description",
		"nat_inside":  "address_nat_inside",
		"nat_outside": "address_nat_outside",
	} {
		if v, ok := vals[src]; ok {
			row[dst] = v
		}
	}

	// address_assigned_object_type is set only when the address is actually
	// assigned to something: assignedType returns "" for a null/absent field
	// (an unassigned address), and "" is not a value worth putting in the row —
	// omitted here reads the same as omitted everywhere else (project() below
	// leaves an absent key nil). This is the raw relation string, not a label:
	// see the reasoning on IPEnrichColumns.
	objectType := assignedType(vals)
	if objectType != "" {
		row["address_assigned_object_type"] = objectType
	}

	// interface_* is filled ONLY for an actual interface. assigned_object is a
	// generic relation (see isInterfaceAssignment): NetBox also assigns
	// addresses to FHRP groups, and their display string is a VRRP/HSRP group,
	// not a port. Blank interface_* is the truthful answer for those — the
	// column means "the interface this address sits on", and there is none.
	//
	// The address_* columns above still populate, on purpose: the address record
	// is real and everything in it is true. Only the ASSIGNMENT is something
	// interface_* cannot describe, so that is all that is withheld.
	if !isInterfaceAssignment(objectType) {
		return
	}

	// assigned_object flattens to the interface's display name.
	if v, ok := vals["assigned_object"]; ok {
		row["interface_name"] = v
	}

	// assigned_object.description is a real field NetBox's brief interface
	// serializer sends, but flattenObject's generic rule never surfaces it
	// (only display/id/slug). A null/absent assigned_object leaves this at
	// its zero value, so a bare, unassigned address can't panic here.
	var nested struct {
		AssignedObject struct {
			Description string `json:"description"`
		} `json:"assigned_object"`
	}
	if json.Unmarshal(raw, &nested) != nil {
		return
	}
	if nested.AssignedObject.Description != "" {
		row["interface_description"] = nested.AssignedObject.Description
	}

	// vm_name, for the VM half of this arm only — vmNameFromAddress gates on
	// virtualization.vminterface itself, so a dcim.interface row leaves it unset
	// and device_name is what names that row. Same move as
	// interface_description above: the value is already in the payload and only
	// flattenObject's collapse to display/id/slug hid it.
	if n, ok := vmNameFromAddress(raw); ok {
		row["vm_name"] = n
	}
}

// assignedType reads assigned_object_type out of a flattened address record.
// It is a plain JSON string, so flattenObject passes it through untouched; a
// null or absent one type-asserts to "", which is not an interface type and so
// fails the gate, exactly as an unassigned address should.
func assignedType(vals map[string]interface{}) string {
	s, _ := vals["assigned_object_type"].(string)
	return s
}

// applyDeviceColumns fills device_* from a flattened device, plus is_primary_ip
// — which is no longer a device_* column, but is still the DEVICE's answer for a
// dcim.interface-assigned row.
func applyDeviceColumns(row map[string]interface{}, dev map[string]interface{}, ipID int) {
	for src, dst := range map[string]string{
		"name":        "device_name",
		"role":        "device_role",
		"platform":    "device_platform",
		"device_type": "device_device_type",
		"site":        "device_site",
		"location":    "device_location",
		"rack":        "device_rack",
		"tenant":      "device_tenant",
		"status":      "device_status",
	} {
		if v, ok := dev[src]; ok {
			row[dst] = v
		}
	}
	row[isPrimaryIPColumn] = isPrimaryIP(dev, ipID)
}

// applyVMColumns fills what a fetched virtual machine can say about the address:
// is_primary_ip, and nothing else.
//
// One column, and that is not an oversight. vm_name is already on the row by the
// time this runs — the address payload carries it (see vmNameFromAddress) — and
// every other VM attribute a dashboard might want (cluster, site, role,
// platform, status) is ruled out on IPEnrichColumns as not worth the request.
// This hop is made for the flag alone, so it reads the flag alone. Adding a
// vm_* column here would also silently change what the hop's gate means, since
// that gate names is_primary_ip rather than a namespace.
func applyVMColumns(row map[string]interface{}, vm map[string]interface{}, ipID int) {
	row[isPrimaryIPColumn] = isPrimaryIP(vm, ipID)
}

// applyPrefixColumns is the fallback for IPs with no address record: today's
// longest-containing-prefix lookup, unchanged, on a much smaller set.
//
// It returns an error ONLY for a failed request, so the caller can report the
// gap. "No prefix contains this IP" and an unflattenable payload are not
// errors — the first is genuine absence, which blank prefix_* columns state
// correctly, and the second cannot be distinguished from it by anything the
// user could act on. Only the request failure means "we could not ask", which
// is the case a blank column silently misrepresents.
func (p *Provider) applyPrefixColumns(ctx context.Context, row map[string]interface{}, ip string) error {
	q := url.Values{}
	q.Set("contains", ip)
	q.Set("limit", "100")
	var page listPage
	if err := p.client.getJSON(ctx, p.client.apiURL("ipam/prefixes", q), &page); err != nil {
		return err
	}
	best := pickLongestPrefix(page.Results)
	if best == nil {
		return nil
	}
	_, vals, err := flattenObject(best)
	if err != nil {
		return nil
	}
	for src, dst := range map[string]string{
		"prefix":      "prefix_cidr",
		"scope":       "prefix_scope",
		"tenant":      "prefix_tenant",
		"role":        "prefix_role",
		"vrf":         "prefix_vrf",
		"vlan":        "prefix_vlan",
		"description": "prefix_description",
	} {
		if v, ok := vals[src]; ok {
			row[dst] = v
		}
	}
	return nil
}

// prefixFallbackWorkers bounds how many ?contains= requests the prefix fallback
// has in flight at once. It mirrors enrichUtilization's pool deliberately: the
// two hops make the same shape of call (many small, independent GETs against one
// NetBox), so a reader tuning upstream pressure has one number to reason about
// rather than two that happen to differ.
//
// 8 rather than "one per IP": the fallback runs at up to the 1,000-IP default
// limit (MaxLimit 10,000 if the caller asks), and an unbounded fan-out would
// point ten thousand simultaneous requests at an instance that answers a single
// one in ~0.35 s. A busy instance sheds that load with 502/503 — which this hop
// does not retry — so the "faster" version would degrade rows that the serial
// version returned correctly. That is the opposite of the trade being made here.
const prefixFallbackWorkers = 8

// prefixJob is one IP's prefix fallback: the row to fill, the IP to ask NetBox
// about, and where the outcome lands. The error is a FIELD rather than a channel
// send because the caller folds the failures back in INPUT order — see
// runPrefixFallback.
type prefixJob struct {
	row map[string]interface{}
	ip  string
	err error
}

// runPrefixFallback fills prefix_* on every job's row, up to
// prefixFallbackWorkers requests at a time.
//
// NetBox's ?contains= takes a single value, so this hop cannot be batched the
// way the address and device hops are — but the requests are INDEPENDENT of one
// another, and running them one at a time was costing a request's full latency
// per unmatched IP. Measured against a large remote NetBox (25 unmatched IPs, all
// of them inside a real prefix): 8.87 s serial, and the same instance answers a
// single ?contains= in ~0.35 s. At the 1,000-IP default limit the serial form is
// ~6 minutes, which no dashboard waits for.
//
// Nothing observable moves. Three properties do the work:
//
//   - Each job owns its own row map — ResolveIPs dedupes the input, so one IP
//     means one row and no two goroutines ever touch the same map — and writes
//     its outcome to its own struct field. The jobs slice is complete before the
//     pool starts and is never appended to while it runs.
//   - The rows are built and projected by the caller's loops, in input order,
//     with this hop only FILLING columns in between. Completion order therefore
//     cannot reorder anything.
//   - The failures are folded into the degradation tally afterwards, walking the
//     jobs in input order, so "first cause wins" (degradation.noteCause) picks
//     the same error the serial version picked: the earliest failing IP in the
//     caller's own order, not whichever request happened to lose the race.
//
// Cancellation behaves as it did. Each job carries the caller's ctx into
// getJSON; on a cancelled context the outstanding requests fail immediately and
// the queued ones fail without touching the network, exactly as the serial loop
// did when its ctx expired mid-walk. The pool always drains, so no goroutine
// outlives the call.
//
// # Why not fetch the prefix table once and match locally
//
// Considered and rejected on measurement. The obvious alternative to N requests
// is ONE walk of ipam/prefixes with longest-match done in memory, and it loses
// on every axis that matters here. Sized against the same instance: 36,041
// prefixes at pageSize 500 is 73 pages of ~412 KB, ~0.92 s each — and paging is
// cursor-based, so those 73 requests are unavoidably sequential. That is ~67 s
// and ~30 MB to answer what is usually a handful of unmatched IPs, against
// ~2 s for the pool. It only overtakes the pool somewhere past a thousand
// unmatched IPs on one refresh, which is the case the limit already bounds.
//
// It would also put the longest-match RULE in this package, where ?contains=
// currently puts it in NetBox: VRF-duplicated prefixes, and the tie-break
// between two containing prefixes of equal length, would then be ours to
// reproduce exactly — a correctness risk taken on for a slower common case.
func (p *Provider) runPrefixFallback(ctx context.Context, jobs []prefixJob) {
	if len(jobs) == 0 {
		return
	}
	sem := make(chan struct{}, prefixFallbackWorkers)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j *prefixJob) {
			defer wg.Done()
			defer func() { <-sem }()
			j.err = p.applyPrefixColumns(ctx, j.row, j.ip)
		}(&jobs[i])
	}
	wg.Wait()
}

// project narrows a row to the requested fields, in the requested order.
func project(row map[string]interface{}, fields []string) map[string]interface{} {
	out := make(map[string]interface{}, len(fields))
	for _, f := range fields {
		if v, ok := row[f]; ok {
			out[f] = v
		} else {
			out[f] = nil
		}
	}
	return out
}

// ResolveIPs maps each input IP to its NetBox context: the address record and
// its interface/device where one exists, falling back to the longest
// containing prefix where it does not. Exactly one row per input IP — the
// documented Grafana join on "ip" requires unique keys — with match_count
// reporting how many address records matched.
func (p *Provider) ResolveIPs(ctx context.Context, ips []string, fields []string, limit int) (*provider.Result, error) {
	// The limit matters more here than for a batched object query: when a
	// prefix_* column is selected the fallback below issues one ?contains=
	// request per unmatched IP and cannot be batched (contains takes a single
	// value), so it is a request-count ceiling. runPrefixFallback runs those
	// requests prefixFallbackWorkers at a time rather than one at a time, which
	// cuts the wall clock by that factor but leaves the count — and the load
	// NetBox sees — proportional to the limit. A caller that really wants more
	// still gets it, up to MaxLimit.
	fields, limit = normalizeIPEnrichArgs(fields, limit)

	// Dedupe preserving input order. requested counts every distinct IP the
	// caller asked about; wanted is what the limit allows us to answer. Total
	// is the former so a clamped query reports as truncated.
	seen := map[string]bool{}
	var wanted []string
	requested := 0
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		requested++
		if len(wanted) < limit {
			wanted = append(wanted, ip)
		}
	}

	addrs, err := p.fetchAddressRecords(ctx, wanted)
	if err != nil {
		return nil, err
	}

	return p.enrichHosts(ctx, wanted, addrs, fields, requested)
}

// ResolveScope answers "whose IPs are these?" for every address NetBox holds
// under the given filters, instead of for a list the caller supplies. It exists
// for alert rules: a rule has no variables and cannot feed one query's output to
// another, so it cannot hand ResolveIPs the IPs a metric carries — but it can
// join the metric's IP against a table of every address in a prefix, VRF or
// tenant inside a SQL expression.
//
// The filters are ipam/ip-addresses filters, sent exactly as an objects query
// would send them (buildFilterValues), so the editor's schema-aware rows drive
// this directly. The listing is one paged request bounded by limit (defaultLimit
// when unset, MaxLimit at most). One row per DISTINCT host: an address held in
// two VRFs is one row whose match_count says two, which is what a join wants —
// two rows would fan the metric out. Everything after the listing is
// enrichHosts, unchanged from the list path.
//
// Total is the number of distinct hosts when the listing was complete, and
// NetBox's reported record count when it was not: the host count of a scope that
// was cut off is unknowable, and the reported count is at least as large as the
// rows, so a cut-off scope always reads as truncated — which a strict consumer
// then refuses rather than joining on a subset.
//
// A failed listing is an error, not a degraded result. On the list path a failed
// batch blanks its own IPs and the rest of the table stands; here the listing is
// the row set, and there is nothing to stand.
func (p *Provider) ResolveScope(ctx context.Context, filters []provider.Filter, fields []string, limit int) (*provider.Result, error) {
	fields, limit = normalizeIPEnrichArgs(fields, limit)

	raws, total, err := p.fetchRows(ctx, "ipam/ip-addresses", buildFilterValues(filters), limit)
	if err != nil {
		return nil, err
	}

	addrs := addressLookup{
		byHost: make(map[string][]json.RawMessage, len(raws)),
		failed: map[string]bool{},
	}
	var hosts []string
	for _, raw := range raws {
		var o struct {
			Address string `json:"address"`
		}
		if json.Unmarshal(raw, &o) != nil {
			continue
		}
		h := canonicalIP(o.Address)
		if _, seen := addrs.byHost[h]; !seen {
			hosts = append(hosts, h)
		}
		addrs.byHost[h] = append(addrs.byHost[h], raw)
	}

	// A cut-off listing can also cut a host's own records in two (NetBox orders
	// addresses by network, mask and host, so one host's records need not even
	// be adjacent), so match_count and the pick may be made over a subset for
	// any host in it. That is not repaired here: the result reads as truncated,
	// which a strict consumer refuses outright and a dashboard is told about.
	reported := len(hosts)
	if total > len(raws) {
		reported = total
	}
	return p.enrichHosts(ctx, hosts, addrs, fields, reported)
}

// normalizeIPEnrichArgs applies the argument rules ResolveIPs and ResolveScope
// share: an empty field list means the default set, "ip" is always a column
// (it is the documented Grafana join key — docs/RECIPES.md joins panels on it —
// and a frame without it has no join key at all), and the limit falls back to
// defaultLimit when unset and is clamped to MaxLimit, exactly as Query() does.
func normalizeIPEnrichArgs(fields []string, limit int) ([]string, int) {
	if len(fields) == 0 {
		fields = defaultIPEnrichFields
	}
	if !slices.Contains(fields, "ip") {
		fields = append([]string{"ip"}, fields...)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	return fields, limit
}

// enrichHosts is the half of an ip-enrichment query that does not care where
// the address records came from: pick one record per host, run the device, VM
// and prefix hops the selected columns ask for, build and project the rows,
// and turn each hop's gaps into notices. ResolveIPs feeds it the records it
// fetched for the caller's IPs; ResolveScope feeds it the records it listed
// under the caller's filters. hosts is the row order and the row key
// (row["ip"]), addrs the records by canonical host, fields the final column
// list (normalised by the caller: non-empty, "ip" present), and total what the
// Result reports as Total — the two callers mean different things by it, and
// only they know which.
func (p *Provider) enrichHosts(ctx context.Context, hosts []string, addrs addressLookup, fields []string, total int) (*provider.Result, error) {
	// Two of the three hops exist only to fill one namespaced column group, and
	// project() below drops whatever the caller did not select — so a hop for an
	// unselected group is work whose entire output is thrown away. Deciding here,
	// once, keeps the row loop and the degradation tallies consistent with each
	// other: a hop that never ran must not report a gap in columns the frame does
	// not contain, which would be a warning about nothing the reader can see.
	//
	// The prefix one is the expensive skip, and remains so after
	// runPrefixFallback made it concurrent. NetBox's ?contains= takes a single
	// value, so the hop is one REQUEST per unmatched IP however it is scheduled —
	// ~20 ms each against the demo NetBox, ~0.35 s each against a large remote NetBox —
	// and concurrency divides the wall clock without removing a single request.
	// No prefix_* column is in defaultIPEnrichFields at all, so the default panel
	// over external/unknown addresses paid the whole of it for nothing: measured
	// on the demo, 100 unknown IPs cost 100 requests and 2.0 s; after this gate,
	// 0 requests and 0.09 s.
	// is_primary_ip is the one column no prefix test can find, and it switches on
	// BOTH identity hops rather than either group: it is the device's answer for a
	// dcim.interface-assigned row and the VM's for a virtualization.vminterface
	// one, and neither hop can supply the other's rows. It is also the only reason
	// the VM hop exists at all — vm_name rides along on the address payload for
	// free — so a query that leaves it unselected never asks NetBox about a
	// virtual machine.
	//
	// Selecting it alone therefore costs two batched requests, not one. That is
	// the honest price of a column whose value depends on which model owns the
	// address, and it is still paid only by queries that ask for it.
	//
	// wantDeviceGroup is kept apart from the gate it feeds because it answers a
	// second question the gate cannot: WHY the device hop is running. It is what
	// tells fetchDevices whether the whole device serializer is needed or only the
	// flag's two source properties, and what tells deviceHopWarning which columns a
	// failure actually blanked in THIS frame. Deriving either of those from
	// `fields` again, separately, is how a request or a warning drifts away from
	// the gate that caused it.
	wantPrimaryIP := slices.Contains(fields, isPrimaryIPColumn)
	wantDeviceGroup := wantsGroup(fields, "device_")
	wantDevice := wantDeviceGroup || wantPrimaryIP
	wantVM := wantPrimaryIP
	wantPrefix := wantsGroup(fields, "prefix_")

	picked := make(map[string]json.RawMessage, len(hosts))
	var deviceIDs, vmIDs []int
	seenDev, seenVM := map[int]bool{}, map[int]bool{}
	for _, ip := range hosts {
		best := pickAddress(addrs.byHost[canonicalIP(ip)])
		if best == nil {
			continue
		}
		picked[ip] = best
		// Collected only when a column that needs the hop was asked for. Leaving
		// the id set empty is what skips it: both fetchers return immediately on
		// an empty set, with a zero degradation, so no request is made and no
		// warning can fire about columns this frame does not contain.
		//
		// The two gates are independent tests rather than one branch: an address
		// is assigned to a device interface, to a VM interface, or to neither, so
		// a query for is_primary_ip over a device-only IP list collects no VM ids
		// at all and makes no VM request.
		if wantDevice {
			if id, ok := deviceIDFromAddress(best); ok && !seenDev[id] {
				seenDev[id] = true
				deviceIDs = append(deviceIDs, id)
			}
		}
		if wantVM {
			if id, ok := vmIDFromAddress(best); ok && !seenVM[id] {
				seenVM[id] = true
				vmIDs = append(vmIDs, id)
			}
		}
	}

	// A device-hop failure degrades to address+interface context rather than
	// failing the whole query — but the blank device columns are NOT the signal,
	// which is why fetchDevices reports a degradation instead of an error for the
	// caller to drop on the floor. Blank device_* is what an IP with no device
	// legitimately looks like, so on its own it says the opposite of the truth.
	// The degradation becomes a Result warning below.
	//
	// Note for tests: a mock NetBox that does not serve dcim/devices yields nil
	// device_* columns for every row, which makes any assertion of the form
	// "device_* is nil here" pass for the wrong reason. A test that means to
	// prove a row was excluded from device context must register dcim/devices and
	// show device context attaching to some other row in the same call — see
	// TestResolveIPs.
	devices, devDeg := p.fetchDevices(ctx, deviceIDs, wantDeviceGroup)

	// The VM hop degrades on the same terms, and its blank is the more
	// misleading of the two: a missing is_primary_ip is exactly what a correct
	// FHRP-assigned or unassigned row looks like, so nothing in the frame would
	// distinguish "we could not ask" from "there is nothing to ask". Hence a
	// degradation here too, and a warning below.
	vms, vmDeg := p.fetchVMs(ctx, vmIDs)

	// The prefix fallback is per-IP — ?contains= takes a single value, so it
	// cannot be batched — so its degradation is tallied here rather than inside a
	// batching helper.
	prefixDeg := degradation{}

	// The fallback requests are collected during the row pass and run together
	// afterwards, on a bounded pool (runPrefixFallback), rather than one at a
	// time inside the loop. Splitting the pass in two is what keeps the output
	// byte-identical: rows are BUILT here in input order, FILLED by the pool in
	// whatever order the requests complete, and PROJECTED below in input order.
	var prefixJobs []prefixJob

	// ambiguous counts the rows whose pick came from more than one candidate
	// record, so the ambiguity is still reported when the user has deselected the
	// match_count column — the only place it would otherwise be visible.
	ambiguous := 0

	built := make([]map[string]interface{}, 0, len(hosts))
	for _, ip := range hosts {
		row := map[string]interface{}{"ip": ip}
		cands := addrs.byHost[canonicalIP(ip)]
		best, hasBest := picked[ip]

		// unknown means the address hop never answered FOR THIS IP: its chunk
		// failed and nothing landed in its bucket. Both conditions are needed —
		// a duplicate spelling of the same host ("10.20.0.1" and "10.20.0.1/32")
		// can straddle a failed and a successful chunk, and the successful one
		// answers for both, since a host's records all come from whichever chunk
		// asked for it. Checking only addrFailed would then blank a row whose
		// answer we hold.
		unknown := addrs.failed[ip] && len(cands) == 0

		if unknown {
			// nil, not 0. 0 is a positive assertion that NetBox holds no record
			// for this IP, emitted as a number specifically so users can
			// threshold on it; here we simply do not know. project() would also
			// leave it nil by omission, but say it explicitly so the intent
			// cannot be mistaken for an oversight.
			row["match_count"] = nil
		} else {
			// float64, not int: buildField's classifyColumn (pkg/plugin/frame.go)
			// only recognizes float64 as numeric — the type every other column
			// already carries via encoding/json + flattenObject. An int here would
			// silently render match_count as a string field, defeating threshold/
			// filter/color-by-value use in Grafana.
			matches := distinctAddressCount(cands)
			row["match_count"] = float64(matches)
			if hasBest && matches > 1 {
				ambiguous++
			}
		}

		switch {
		case hasBest:
			applyAddressColumns(row, best)
			// Exactly one of these two can fire, and often neither: the type gates
			// inside deviceIDFromAddress and vmIDFromAddress are mutually
			// exclusive, and an address assigned to an FHRP group or to nothing at
			// all satisfies neither. Those rows leave is_primary_ip ABSENT — not
			// false — and that is a decision rather than a gap; isPrimaryIP states
			// why, including why the FHRP case is left without a value on purpose.
			if id, ok := deviceIDFromAddress(best); ok {
				if dev, ok := devices[id]; ok {
					applyDeviceColumns(row, dev, addressID(best))
				}
			}
			if id, ok := vmIDFromAddress(best); ok {
				if vm, ok := vms[id]; ok {
					applyVMColumns(row, vm, addressID(best))
				}
			}
		case unknown:
			// No prefix fallback. Its precondition is "this IP has no address
			// record", and that is precisely what the failed lookup left
			// unestablished. Filling prefix_* here produced the worst output of
			// the three: a plausible-looking prefix beside match_count 0, on an
			// IP NetBox knows perfectly well.
		case wantPrefix:
			// Queued, not requested: the request itself is made below. The tally
			// of what was ASKED is still taken here, in input order, because it
			// counts rows rather than outcomes.
			prefixDeg.total++
			prefixJobs = append(prefixJobs, prefixJob{row: row, ip: ip})
		default:
			// This IP has no address record and no prefix_* column was selected,
			// so there is nothing left to fill. The row is complete: match_count
			// is already set from the address index, which the prefix hop plays no
			// part in.
		}
		built = append(built, row)
	}

	// Every queued fallback runs here, concurrently and bounded. It fills columns
	// on rows that already exist and never adds, drops or reorders one.
	p.runPrefixFallback(ctx, prefixJobs)

	// Folded in INPUT order, not completion order: degradation.noteCause keeps
	// the FIRST cause, and "first" has to mean the same thing it meant when the
	// requests ran one after another — the earliest failing IP in the caller's
	// own order — or the warning's rendered reason would vary between two
	// identical queries over identical data.
	for i := range prefixJobs {
		if err := prefixJobs[i].err; err != nil {
			prefixDeg.record(1, err)
		}
	}

	// Projected after the fill, in input order, so the frame's rows are the ones
	// the loop above built and in the order it built them.
	rows := make([]map[string]interface{}, 0, len(built))
	for _, row := range built {
		rows = append(rows, project(row, fields))
	}

	// One summary line, not one per IP: this loop can run a thousand times and a
	// per-failure log would bury the rest of the query's diagnostics.
	if prefixDeg.any() {
		log.DefaultLogger.Warn(
			"ip-enrichment: prefix fallback failed; the IPs it covers degrade to blank prefix_* columns",
			"failed", prefixDeg.failed,
			"of", prefixDeg.total,
			"error", logSafe(prefixDeg.cause.Error()),
		)
	}

	// Order: the hops in the order they run, so the notices read as the pipeline
	// broke down. Address first — it is upstream of both others, and when it
	// degrades the IPs it lost also lose device context, which the device warning
	// alone would not explain. The address hop can report two different gaps and
	// both belong here, next to each other: one names what could not be asked,
	// the other what could not be finished.
	var warnings []string
	if addrs.deg.any() {
		warnings = append(warnings, addressHopWarning(addrs.deg))
	}
	if len(addrs.truncated) > 0 {
		warnings = append(warnings, addressTruncationWarning(addrs.truncated))
	}
	if devDeg.any() {
		warnings = append(warnings, deviceHopWarning(devDeg, wantDeviceGroup, wantPrimaryIP))
	}
	if vmDeg.any() {
		warnings = append(warnings, vmHopWarning(vmDeg))
	}
	if prefixDeg.any() {
		warnings = append(warnings, prefixHopWarning(prefixDeg))
	}

	var notes []string
	if ambiguous > 0 {
		notes = append(notes, ambiguityNote(ambiguous, len(rows)))
	}

	return &provider.Result{
		MaxRows: MaxLimit,
		Columns: fields, Rows: rows, Total: total,
		Warnings: warnings, Notes: notes,
		ColumnTypes: declaredColumnTypes(fields),
	}, nil
}

// ambiguityNote states that some rows were picked out of several candidates.
// match_count already says so per row, but it is an opt-in column: a user who
// deselects it gets one arbitrary-looking device for an anycast address with
// nothing at all to suggest the pick was a pick. Info, not warning — the pick is
// deterministic and documented, so the row is correct and stable; the reader just
// needs to know "the device" is not unique for that address.
func ambiguityNote(ambiguous, rows int) string {
	return fmt.Sprintf(
		"%s of %s rows matched more than one NetBox address record (an anycast address, or a VIP shared across a redundant pair). One record was picked deterministically — interface-assigned first, then any other assignment, then non-deprecated, then lowest id — so the row is stable; add the match_count column to see which rows.",
		humanInt(ambiguous), humanInt(rows))
}

// pickLongestPrefix returns the raw prefix object with the longest mask.
func pickLongestPrefix(results []json.RawMessage) json.RawMessage {
	var best json.RawMessage
	bestLen := -1
	for _, r := range results {
		var o struct {
			Prefix string `json:"prefix"`
		}
		if json.Unmarshal(r, &o) != nil {
			continue
		}
		idx := strings.LastIndex(o.Prefix, "/")
		if idx < 0 {
			continue
		}
		l, err := strconv.Atoi(o.Prefix[idx+1:])
		if err != nil {
			continue
		}
		if l > bestLen {
			bestLen = l
			best = r
		}
	}
	return best
}
