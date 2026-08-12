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

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// chunkBudgetBytes caps the encoded length of a batched query string. The
// measured server ceiling is ~8 KB (450 IPv4 addresses returned HTTP 431, as
// did 160 IPv6); 6 KB leaves headroom for other params and lower proxy limits.
// The budget is in BYTES on purpose: an IPv6 address encodes to roughly three
// times an IPv4 one, so a count-based limit tuned on IPv4 would pass every
// IPv4-only test and then fail on the first IPv6-heavy panel.
const chunkBudgetBytes = 6144

// chunkByBudget splits values into batches whose encoded "param=value&..."
// form stays within budget bytes. Order is preserved and no value is dropped;
// a single value larger than the budget gets a chunk of its own.
func chunkByBudget(param string, values []string, budget int) [][]string {
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
		"error", strings.NewReplacer("\n", " ", "\r", " ").Replace(err.Error()),
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

// The three degradation messages below each name the missing COLUMNS and end by
// stating what the blank does NOT mean. vm_name is spelled out individually
// because it is the one column with no group prefix to hide behind: a reader
// scanning for "vm_*" would not find it, and it is in the default selection, so
// a degraded default panel blanks it.
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
		"Address lookup failed for %s — %s. The match_count, address_*, interface_*, device_*, vm_name and prefix_* columns on the affected rows are empty because the lookup failed, not because NetBox has no record for those IPs.",
		d.scope("IP", "IPs"), causeText(d.cause))
}

func deviceHopWarning(d degradation) string {
	return fmt.Sprintf(
		"Device lookup failed for %s — %s. The device_* columns on the affected rows are blank because the lookup failed, not because those IPs have no device.",
		d.scope("device", "devices"), causeText(d.cause))
}

// addressTruncationWarning states the one gap batch splitting cannot close: a
// SINGLE address with more records than one request can carry (see
// fetchAddressRecords). It is unlike the three hop warnings around it, and says
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
		"NetBox holds more than %s address records for %s (%s) — only the first %s were read. match_count is a floor rather than a count for the affected rows, and their address_*, interface_*, device_* and vm_name columns describe a record picked from the part that was read.",
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
	// dropped, an expanded IPv6 compresses, uppercase stays the same length), so
	// the byte budget computed from the input remains a valid upper bound.
	chunks := chunkByBudget("address", sendable, chunkBudgetBytes)

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

		q := url.Values{}
		for _, ip := range b.ips {
			q.Add("address", canonicalIP(ip))
		}
		// MaxLimit, not len(b.ips): anycast means a batch can match more
		// records than addresses requested. total is what NetBox says exists.
		raws, total, err := p.fetchRows(ctx, "ipam/ip-addresses", q, MaxLimit)
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
				"address", host, "read", len(raws), "reported", total, "cap", MaxLimit,
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

// isPrimaryIP reports whether ipID is the device's primary address. This is
// the join key that lines a flow IP up with the device SNMP polls, so it is
// an explicit column rather than something consumers infer.
func isPrimaryIP(device map[string]interface{}, ipID int) bool {
	for _, key := range []string{"primary_ip4_id", "primary_ip6_id"} {
		if f, ok := device[key].(float64); ok && int(f) == ipID {
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
// There is no error return, and that is the point. By spec a device-hop failure
// — partial OR total — never fails the query, so an error here would have no
// consumer and the only honest thing the caller could do with it is discard it.
// It used to, as `devices, _ := p.fetchDevices(...)`, and that silent discard is
// exactly how a total failure came out looking like "these IPs have no device".
// The degradation return carries everything a caller needs, including the cause,
// so nothing is left to swallow: deg.failed == deg.total means the hop failed
// outright.
func (p *Provider) fetchDevices(ctx context.Context, ids []int) (map[int]map[string]interface{}, degradation) {
	out := make(map[int]map[string]interface{}, len(ids))
	deg := degradation{total: len(ids)}
	if len(ids) == 0 {
		return out, deg
	}

	strs := make([]string, 0, len(ids))
	for _, id := range ids {
		strs = append(strs, fmt.Sprintf("%d", id))
	}

	chunks := chunkByBudget("id", strs, chunkBudgetBytes)
	for i, chunk := range chunks {
		q := url.Values{}
		for _, id := range chunk {
			q.Add("id", id)
		}
		// The reported total is discarded here, and unlike the address hop that
		// is safe rather than an oversight. ?id= is an exact-match filter on the
		// primary key, so a batch matches at most one device per id it names —
		// verified live against NetBox 4.4.10: ?id=1&id=1&id=2&id=3 returns
		// count 3, so repeats collapse and an absent id contributes nothing. The
		// byte budget caps a batch at ~893 ids (614 for six-digit ones), an
		// order of magnitude below MaxLimit, so this response cannot overflow
		// the cap the way an anycast-heavy address batch can.
		raws, _, err := p.fetchRows(ctx, "dcim/devices", q, MaxLimit)
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

// IPEnrichColumns lists every column ip-enrichment can emit, in group order.
// "ip" and "match_count" are deliberately not namespaced: "ip" is the
// documented Grafana join key, and match_count describes the row itself.
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
// status — each of which needs a virtualization/virtual-machines hop this
// package does not make) and the dropped interface_* columns. The VM's NAME is
// not in that ruling: it is embedded in the address payload already, so it
// costs nothing and is shipped as vm_name.
func IPEnrichColumns() []string {
	return []string{
		"ip", "match_count",
		"prefix_cidr", "prefix_scope", "prefix_tenant", "prefix_role",
		"prefix_vrf", "prefix_vlan", "prefix_description",
		"address_dns_name", "address_status", "address_role", "address_vrf",
		"address_tenant", "address_description", "address_assigned_object_type",
		"address_nat_inside", "address_nat_outside",
		"interface_name", "interface_description",
		"device_name", "device_role", "device_platform", "device_device_type",
		"device_site", "device_location", "device_rack", "device_tenant",
		"device_status", "device_is_primary_ip",
		"vm_name",
	}
}

// defaultIPEnrichFields is what a new query selects. match_count is included
// on purpose: an ambiguous pick must be visible out of the box.
var defaultIPEnrichFields = []string{
	"ip", "match_count", "address_dns_name", "device_name", "vm_name", "interface_name",
	"device_is_primary_ip", "device_site", "device_tenant",
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
// string for an all-null column, so device_is_primary_ip alternated between a
// boolean field and a string one depending on whether any IP in the refresh
// happened to resolve a device. See provider.Result.ColumnTypes.
//
// Every other column flattens to a string, which is also the inference fallback,
// so listing them would change nothing and only invite drift.
var ipEnrichColumnTypes = map[string]provider.FieldType{
	"match_count":          provider.FieldTypeNumber,
	"device_is_primary_ip": provider.FieldTypeBoolean,
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
// Prefix matching is exact-by-construction: every column in IPEnrichColumns
// beginning with "device_" or "prefix_" comes from that group's hop, and the two
// non-namespaced columns ("ip", "match_count") belong to neither.
//
// "vm_" is NOT a group here, alongside "address_" and "interface_": vm_name is
// read off the address payload this query already holds, so there is no hop to
// skip. If VM device-grade columns are ever added, gate their hop on those
// specific non-free vm_* names — never on the whole "vm_" prefix, which would
// make the free column start paying for a request.
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

// applyDeviceColumns fills device_* from a flattened device, including the
// primary-IP flag.
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
	row["device_is_primary_ip"] = isPrimaryIP(dev, ipID)
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
	if len(fields) == 0 {
		fields = defaultIPEnrichFields
	}
	// "ip" is the documented Grafana join key (docs/RECIPES.md joins panels on
	// it). A caller-supplied field list that omits it would otherwise ship a
	// frame with no join key at all, so it is force-included rather than left
	// to the caller to remember.
	hasIP := false
	for _, f := range fields {
		if f == "ip" {
			hasIP = true
			break
		}
	}
	if !hasIP {
		fields = append([]string{"ip"}, fields...)
	}

	// Two of the three hops exist only to fill one namespaced column group, and
	// project() below drops whatever the caller did not select — so a hop for an
	// unselected group is work whose entire output is thrown away. Deciding here,
	// once, keeps the row loop and the degradation tallies consistent with each
	// other: a hop that never ran must not report a gap in columns the frame does
	// not contain, which would be a warning about nothing the reader can see.
	//
	// The prefix one is the expensive skip. That fallback is SERIAL — NetBox's
	// ?contains= takes a single value, so it cannot be batched — and measures
	// ~20 ms per IP against the demo NetBox (100 unknown IPs: 100 requests,
	// 2.0 s; after this gate, 0 requests and 0.09 s). No prefix_* column is in
	// defaultIPEnrichFields at all, so the default panel over external/unknown
	// addresses paid the whole of it for nothing: ~20 s at the 1,000-IP default
	// limit, enough to time the panel out.
	wantDevice := wantsGroup(fields, "device_")
	wantPrefix := wantsGroup(fields, "prefix_")

	// An unset limit falls back to defaultLimit, exactly as Query() does — not
	// to MaxLimit. When a prefix_* column is selected the fallback below issues
	// one SERIAL ?contains= request per unmatched IP and cannot be batched
	// (contains takes a single value), so the limit is a wall-clock ceiling here
	// in a way it is not for a batched object query: measured against the demo
	// NetBox, 400 unmatched IPs take ~9s, which puts MaxLimit at minutes and far
	// worse over a WAN. A caller that really wants more still gets it, up to
	// MaxLimit.
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

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

	picked := make(map[string]json.RawMessage, len(wanted))
	var deviceIDs []int
	seenDev := map[int]bool{}
	for _, ip := range wanted {
		best := pickAddress(addrs.byHost[canonicalIP(ip)])
		if best == nil {
			continue
		}
		picked[ip] = best
		// Collected only when a device_* column was asked for. Leaving deviceIDs
		// empty is what skips the hop: fetchDevices returns immediately on an
		// empty id set, with a zero degradation, so no request is made and no
		// warning can fire about columns this frame does not contain.
		if !wantDevice {
			continue
		}
		if id, ok := deviceIDFromAddress(best); ok && !seenDev[id] {
			seenDev[id] = true
			deviceIDs = append(deviceIDs, id)
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
	devices, devDeg := p.fetchDevices(ctx, deviceIDs)

	// The prefix fallback is per-IP and serial, so its degradation is tallied
	// here rather than inside a batching helper.
	prefixDeg := degradation{}

	// ambiguous counts the rows whose pick came from more than one candidate
	// record, so the ambiguity is still reported when the user has deselected the
	// match_count column — the only place it would otherwise be visible.
	ambiguous := 0

	rows := make([]map[string]interface{}, 0, len(wanted))
	for _, ip := range wanted {
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
			if id, ok := deviceIDFromAddress(best); ok {
				if dev, ok := devices[id]; ok {
					applyDeviceColumns(row, dev, addressID(best))
				}
			}
		case unknown:
			// No prefix fallback. Its precondition is "this IP has no address
			// record", and that is precisely what the failed lookup left
			// unestablished. Filling prefix_* here produced the worst output of
			// the three: a plausible-looking prefix beside match_count 0, on an
			// IP NetBox knows perfectly well.
		case wantPrefix:
			prefixDeg.total++
			if err := p.applyPrefixColumns(ctx, row, ip); err != nil {
				prefixDeg.record(1, err)
			}
		default:
			// This IP has no address record and no prefix_* column was selected,
			// so there is nothing left to fill. The row is complete: match_count
			// is already set from the address index, which the prefix hop plays no
			// part in.
		}
		rows = append(rows, project(row, fields))
	}

	// One summary line, not one per IP: this loop can run a thousand times and a
	// per-failure log would bury the rest of the query's diagnostics.
	if prefixDeg.any() {
		log.DefaultLogger.Warn(
			"ip-enrichment: prefix fallback failed; the IPs it covers degrade to blank prefix_* columns",
			"failed", prefixDeg.failed,
			"of", prefixDeg.total,
			"error", strings.NewReplacer("\n", " ", "\r", " ").Replace(prefixDeg.cause.Error()),
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
		warnings = append(warnings, deviceHopWarning(devDeg))
	}
	if prefixDeg.any() {
		warnings = append(warnings, prefixHopWarning(prefixDeg))
	}

	var notes []string
	if ambiguous > 0 {
		notes = append(notes, ambiguityNote(ambiguous, len(rows)))
	}

	return &provider.Result{
		Columns: fields, Rows: rows, Total: requested,
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
