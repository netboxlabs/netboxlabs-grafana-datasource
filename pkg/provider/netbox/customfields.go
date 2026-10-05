package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// customFieldsEndpoint lists every custom field NetBox defines, with its type.
// Custom-field NAMES are unique across a NetBox instance (extras.CustomField
// declares name unique), so the type of the cf_<name> column an object query
// emits is knowable from the name alone — no object-type mapping is needed, and
// one fetch covers every object type.
const customFieldsEndpoint = "extras/custom-fields"

// customFieldsLimit bounds the definition walk at the provider's own ceiling.
// An instance defines custom fields by the dozen, not the thousand, so the
// walk is one page in practice; the ceiling exists so a pathological instance
// cannot turn a type lookup into an unbounded walk. An instance that exceeds
// it is not silently cached as complete: the envelope total says the index is
// truncated, and an unknown column is then resolved by name (see
// resolveCustomFieldByName) rather than assumed undefined.
const customFieldsLimit = MaxLimit

// customFieldsRetryTTL is how long a TRANSIENT definition failure — a 5xx, a
// 429, a dropped connection, an undecodable body — is remembered before the
// next query asks again. It is short because the cost of asking is one small
// request and the cost of not asking is the string-column fallback on every
// numeric custom field for the whole window: an alert comparing against such
// a column reads ” as 0 and fires, which is the failure this index exists to
// prevent. One minute keeps a busy instance from being polled per evaluation
// without turning a single timeout into half an hour of wrong types.
const customFieldsRetryTTL = time.Minute

// customFieldsTTL is how long a SUCCESSFUL definition index is trusted. It is
// cacheTTL, the same five minutes the field picker's sample gets, and not
// schemaTTL's thirty: the OpenAPI schema describes NetBox's code and changes
// on upgrade, but custom-field definitions are data an administrator edits
// at will. The refresh-on-unknown path catches a field that appears; it
// cannot catch a field deleted and recreated under the same name with
// another type while every value is still unset, because nothing in a result
// distinguishes that from the field it replaced. Five minutes bounds how long
// such a replacement is typed as its predecessor, at the cost of one small
// request per branch per five minutes.
const customFieldsTTL = cacheTTL

// customFieldsWaitBudget is how long a query waits for a definition fetch that
// is in flight before answering without it. The index is a HINT: a query that
// goes out untyped gets value inference for this one refresh, while a query
// that waits the caller's whole budget on a stalled extras endpoint gets
// nothing at all — a dashboard that times out, an alert rule in Error. The
// fetch itself is not abandoned at this point; it carries on to its own budget
// and fills the cache for the next caller (see customFieldsFlight).
//
// Two seconds is generous for a single small list request against a healthy
// instance and short against Grafana's request budget. The override on the
// Provider exists only so tests can shrink it.
const customFieldsWaitBudget = 2 * time.Second

// customFieldTypesEntry is one branch's cached custom-field type index. A failed
// fetch is cached too, as an empty index, for a TTL chosen by
// customFieldsFailureTTL: a token that cannot read extras gets the same answer
// on every query and is remembered for schemaTTL; a NetBox that is merely busy
// is asked again soon.
type customFieldTypesEntry struct {
	types map[string]provider.FieldType
	// known is every custom-field NAME the fetch returned, mapped to its NetBox
	// type, declared or not: a Text field is known and undeclared, a field
	// created after the fetch is unknown. The type is kept because it decides
	// which DERIVED columns the field can produce (see suffixDerivable). The
	// distinction is what lets an unfamiliar cf_* column trigger
	// a refresh without a Text column doing the same on every query.
	known     map[string]string
	ok        bool // the fetch succeeded; false is a cached failure
	fetchedAt time.Time
	expiry    time.Time
	// truncated: the instance defines more custom fields than the walk
	// returned, so known is incomplete and an unknown name may still be a
	// real definition. Resolved by name on demand.
	truncated bool
	// refreshFailed: this entry is a previous successful index published
	// back because the refresh that was replacing it failed (see
	// customFieldsFlight.previous). Its declarations are good; the column
	// that triggered the refresh is still unresolved, and a caller must not
	// cache anything typed from it as if the refresh had succeeded.
	refreshFailed bool
}

// customFieldIndex is what a caller gets from customFieldTypes.
type customFieldIndex struct {
	types map[string]provider.FieldType
	known map[string]string
	// settled reports whether this is an answer (a populated index, or a
	// failure NetBox actually gave, either of which the cache now holds) or
	// the stand-in a caller gets when the fetch is still in flight past its
	// wait budget or the caller's own context ended.
	settled bool
	// ok is settled AND the fetch succeeded, so known is complete — unless
	// truncated, in which case known is only the first customFieldsLimit.
	ok            bool
	truncated     bool
	refreshFailed bool
	fetchedAt     time.Time
	// validUntil is when the cache will ask NetBox again; zero for the stand-in.
	validUntil time.Time
}

// standIn is the index a caller gets instead of waiting any longer.
func standIn() customFieldIndex {
	return customFieldIndex{types: map[string]provider.FieldType{}}
}

func (e customFieldTypesEntry) index() customFieldIndex {
	// A previous index published back after a failed refresh (refreshFailed)
	// is a good answer for a query and not a settled one for a cache: the
	// column that triggered the refresh is still unresolved, and a field list
	// cached from this entry would keep its string fallback past the retry
	// that resolves it. Every reader sees that, not just the caller that
	// started the refresh.
	return customFieldIndex{types: e.types, known: e.known, settled: !e.refreshFailed, ok: e.ok, truncated: e.truncated, refreshFailed: e.refreshFailed, fetchedAt: e.fetchedAt, validUntil: e.expiry}
}

// customFieldsFailureTTL decides how long a failed definition fetch stays
// cached. The statuses that are NetBox's settled answer about this request —
// a 401/403 on a token without extras permission, a 404 or 410 on an instance
// without the endpoint, a 405 on one that has it but will not list it, a 400
// on the request itself — are remembered for schemaTTL, like a success.
// Everything else is asked again after customFieldsRetryTTL: retryable()'s
// "not now" statuses, a 429, a 500, a 408 (a proxy or NetBox timing the
// request out is the same event as the fetch budget running out, seen from
// the other side), any 4xx this list does not name, a transport failure, a
// body that did not decode, the fetch budget itself.
//
// The settled list is enumerated rather than "any 4xx" because a 4xx is not
// one thing: 408 and 429 are explicitly transient, and an unlisted status is
// safer retried in a minute than believed for thirty.
//
// Caller cancellation never reaches here: the fetch runs detached from the
// starter's context (see customFieldsFlight), so the only deadline it can hit
// is its own budget, which is a statement about NetBox, not about the caller.
func customFieldsFailureTTL(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone:
			return schemaTTL
		}
	}
	return customFieldsRetryTTL
}

// customFieldType maps a NetBox custom-field type to the frame type its values
// carry. Only the types whose values the frame builder could not otherwise
// classify from an all-null column are declared:
//
//   - integer and decimal arrive as JSON numbers;
//   - boolean arrives as JSON true/false.
//
// Everything else — text, longtext, url, json, select, multiselect, date,
// datetime, object, multiobject — is left undeclared. Their values flatten to
// strings (or to a string plus an _id column) whenever present, so a string
// fallback for the all-null case is the same type the values would produce and
// there is no flip to prevent. Declaring date/datetime as time would CREATE
// one: the values are not RFC 3339 in the dcim time-column sense the frame
// builder parses, so a populated column is a string and an empty one would
// have been time.
func customFieldType(t string) (provider.FieldType, bool) {
	switch t {
	case "integer", "decimal":
		return provider.FieldTypeNumber, true
	case "boolean":
		return provider.FieldTypeBoolean, true
	}
	return "", false
}

// customFieldsFlight is one in-progress definition fetch for one branch. It
// exists for the same reason objectTypeFlight does: a dashboard opening runs
// every panel's query at once, so a cold or expired index is missed by all of
// them together, and without a flight each would fetch on its own — and, worse,
// each would then WRITE the cache, so a slow request that failed could land
// after a fast one that succeeded and replace a populated index with an empty
// one for customFieldsRetryTTL. With a flight there is exactly one fetch and
// one writer per branch; everyone else waits on done.
type customFieldsFlight struct {
	done  chan struct{}
	entry customFieldTypesEntry
	// previous is the successful index a REFRESH is replacing, nil for a
	// cold or expired fetch. A refresh that fails must not trade a usable
	// index for a failure entry: the fetch publishes previous back instead,
	// with its fetch time bumped so the unknown column that triggered the
	// refresh backs off for customFieldsRetryTTL.
	previous *customFieldTypesEntry
}

// customFieldTypes returns the cf_<name> -> type index for the branch on ctx,
// fetching it from NetBox at most once per cache period and at most once at a
// time. It never fails: a fetch that cannot be made (no permission on extras,
// an older instance, an outage) logs once and yields an empty index for a TTL
// chosen by customFieldsFailureTTL, so the column types fall back to value
// inference exactly as they did before the index existed. The query itself
// must not fail because a type HINT could not be obtained.
//
// A caller whose own context ends, or whose customFieldsWaitBudget runs out,
// while the fetch is in flight gets the empty index for THIS query and nothing
// is cached on its behalf; the fetch it started carries on for the callers
// that are still waiting and for the cache. settled reports which of the two
// the caller got: a real answer (a populated index, or a failure NetBox
// actually gave, either of which the cache now holds) or the stand-in. A
// caller that caches something derived from the index must not do so on the
// stand-in, or it would remember the fallback long after the flight has
// filled the cache behind it — and must not keep it past validUntil, which is
// when the index itself will be asked again: a failure NetBox gave is settled
// for customFieldsRetryTTL, not forever, and a cache built on it that lasts
// longer would outlive the retry that repairs it. validUntil is zero for the
// stand-in.
func (p *Provider) customFieldTypes(ctx context.Context) customFieldIndex {
	ctx, branch := p.client.pinBranch(ctx)

	p.mu.Lock()
	e, ok := p.customFieldsByBranch[branch]
	if ok && time.Now().Before(e.expiry) {
		p.mu.Unlock()
		return e.index()
	}
	// An EXPIRED successful index is still the best fallback there is: a
	// refetch that fails must publish it back rather than a failure entry,
	// exactly as an unknown-column refresh does, or every declared column
	// reverts to a string for the retry TTL because NetBox had a bad minute.
	var previous *customFieldTypesEntry
	if ok && e.ok {
		prev := e
		previous = &prev
	}
	flight := p.startOrJoinCustomFieldsFlight(ctx, branch, previous)
	p.mu.Unlock()

	return p.awaitCustomFieldsFlight(ctx, flight)
}

// startOrJoinCustomFieldsFlight returns the branch's in-progress fetch,
// starting one with previous (see customFieldsFlight) if none is in flight.
// Callers hold p.mu.
func (p *Provider) startOrJoinCustomFieldsFlight(ctx context.Context, branch string, previous *customFieldTypesEntry) *customFieldsFlight {
	flight := p.customFieldsFlightByBranch[branch]
	if flight == nil {
		flight = &customFieldsFlight{done: make(chan struct{}), previous: previous}
		if p.customFieldsFlightByBranch == nil {
			p.customFieldsFlightByBranch = map[string]*customFieldsFlight{}
		}
		p.customFieldsFlightByBranch[branch] = flight
		go p.runCustomFieldsFetch(ctx, branch, flight)
	}
	return flight
}

// awaitCustomFieldsFlight waits for flight within the wait budget and the
// caller's context, returning its index or the stand-in.
func (p *Provider) awaitCustomFieldsFlight(ctx context.Context, flight *customFieldsFlight) customFieldIndex {
	wait := time.NewTimer(p.customFieldsWait())
	defer wait.Stop()
	select {
	case <-flight.done:
		return flight.entry.index()
	case <-ctx.Done():
		return flight.fallback()
	case <-wait.C:
		log.DefaultLogger.Debug("netbox: custom-field definitions still in flight; answering this query with value inference",
			"waited", p.customFieldsWait().String())
		return flight.fallback()
	}
}

// fallback is what a caller gets instead of waiting any longer: the index the
// flight is replacing, if it is replacing one, reported unsettled — so a
// concurrent waiter that joined a refresh keeps every declaration the caller
// that started it keeps — and the empty stand-in for a cold fetch.
func (f *customFieldsFlight) fallback() customFieldIndex {
	if f.previous != nil {
		idx := f.previous.index()
		idx.settled = false
		return idx
	}
	return standIn()
}

// refreshCustomFieldTypes replaces the cached index for the branch on ctx —
// provided it is still the one the caller saw, so two callers noticing the
// same unknown column at once refresh it once — with a fresh fetch, and
// returns the result, or the stand-in if that takes longer than the wait
// budget. The index being replaced rides on the flight as previous: a fetch
// that fails publishes it back rather than a failure entry.
func (p *Provider) refreshCustomFieldTypes(ctx context.Context, seen customFieldIndex) customFieldIndex {
	ctx, branch := p.client.pinBranch(ctx)
	p.mu.Lock()
	e, ok := p.customFieldsByBranch[branch]
	if !ok || !e.fetchedAt.Equal(seen.fetchedAt) {
		// Someone else already replaced it; use what is there now.
		p.mu.Unlock()
		return p.customFieldTypes(ctx)
	}
	delete(p.customFieldsByBranch, branch)
	previous := e
	flight := p.startOrJoinCustomFieldsFlight(ctx, branch, &previous)
	p.mu.Unlock()
	return p.awaitCustomFieldsFlight(ctx, flight)
}

// customFieldsWait is customFieldsWaitBudget unless a test has shortened it.
func (p *Provider) customFieldsWait() time.Duration {
	if p.customFieldsWaitOverride > 0 {
		return p.customFieldsWaitOverride
	}
	return customFieldsWaitBudget
}

// runCustomFieldsFetch performs one read of the definitions for branch,
// publishes the outcome to the cache and releases everyone waiting on flight.
//
// ctx is the STARTER'S context and is used only for its values — the branch
// header rides on it — never for its cancellation, for the reason
// runObjectTypeFetch gives: the starter is one of several waiters, and its
// leaving must not abort the read the others need. objectTypeFetchBudget bounds
// the read instead, so a hanging endpoint cannot pin the flight.
func (p *Provider) runCustomFieldsFetch(ctx context.Context, branch string, flight *customFieldsFlight) {
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.objectTypeFetchBudget())
	defer cancel()

	types := map[string]provider.FieldType{}
	known := map[string]string{}
	ttl := customFieldsTTL
	rows, total, err := p.fetchRows(fetchCtx, customFieldsEndpoint, url.Values{}, customFieldsLimit)
	// total is the envelope count, 0 when the source could not report one; a
	// count above what came back means the walk hit customFieldsLimit.
	truncated := err == nil && total > len(rows)
	if truncated {
		log.DefaultLogger.Warn("netbox: custom-field definitions exceed the walk ceiling; the type index is partial and unknown columns are resolved by name",
			"returned", len(rows), "total", total)
	}
	if err != nil {
		ttl = customFieldsFailureTTL(err)
		log.DefaultLogger.Warn("netbox: custom-field definitions unavailable; custom-field column types fall back to value inference",
			"retry_in", ttl.String(), "error", logSafe(err.Error()))
	}
	// A row that does not decode is skipped, not fatal: the index is a hint,
	// and one odd definition should cost its own column value inference, not
	// the whole fetch. But the skip is counted and said out loud, because
	// "one odd row" has a shape where it is every row — a serializer that
	// shapes `type` differently from {value,label} — and a silently empty
	// index would then make every cf_* column look unknown and re-fetch once
	// a minute forever with nothing in the logs to say why. When nothing
	// decoded at all, the fetch is treated as failed on the retry TTL rather
	// than cached as a successful empty index for customFieldsTTL.
	var undecodable int
	var firstDecodeErr error
	for _, raw := range rows {
		var cf struct {
			Name string `json:"name"`
			Type struct {
				Value string `json:"value"`
			} `json:"type"`
		}
		if derr := json.Unmarshal(raw, &cf); derr != nil || cf.Name == "" {
			undecodable++
			if firstDecodeErr == nil {
				firstDecodeErr = derr
				if firstDecodeErr == nil {
					firstDecodeErr = errors.New("definition has no name")
				}
			}
			continue
		}
		known[cfPrefix+cf.Name] = cf.Type.Value
		if t, ok := customFieldType(cf.Type.Value); ok {
			types[cfPrefix+cf.Name] = t
		}
	}
	if undecodable > 0 {
		if undecodable == len(rows) && err == nil {
			err = fmt.Errorf("none of %d custom-field definitions decoded: %w", undecodable, firstDecodeErr)
			ttl = customFieldsRetryTTL
		}
		log.DefaultLogger.Warn("netbox: custom-field definitions could not be decoded; their columns fall back to value inference",
			"skipped", undecodable, "of", len(rows), "retry_in", ttl.String(), "first_error", logSafe(firstDecodeErr.Error()))
	}

	p.mu.Lock()
	if p.customFieldsByBranch == nil {
		p.customFieldsByBranch = map[string]customFieldTypesEntry{}
	}
	now := time.Now()
	entry := customFieldTypesEntry{types: types, known: known, ok: err == nil, truncated: truncated, fetchedAt: now, expiry: now.Add(ttl)}
	if err != nil && flight.previous != nil && flight.previous.ok {
		// A refresh that failed. The index it was replacing is still the
		// best answer there is; publish it back with its fetch time bumped,
		// so the unknown column that triggered this backs off for the retry
		// TTL instead of re-triggering on the next query, and with its
		// expiry extended at least that far so a cold miss does not follow
		// straight after.
		entry = *flight.previous
		entry.fetchedAt = now
		entry.refreshFailed = true
		if entry.expiry.Before(now.Add(customFieldsRetryTTL)) {
			entry.expiry = now.Add(customFieldsRetryTTL)
		}
	}
	// No dropExpired here, unlike the schema and fields caches: an expired
	// successful index is the fallback a failed refetch publishes back (see
	// customFieldTypes), so it has to outlive its expiry. Entries are a few
	// names and types per branch actually queried.
	p.customFieldsByBranch[branch] = entry
	// Retire the flight under the SAME lock that published the outcome, so a
	// caller arriving now sees one or the other and never a gap between them.
	delete(p.customFieldsFlightByBranch, branch)
	p.mu.Unlock()

	flight.entry = entry
	close(flight.done)
}

// customFieldNotDefined marks a name that was asked about by a by-name lookup
// and is not a definition, so it is not asked about again until the index is
// refetched. It is never a NetBox type value.
const customFieldNotDefined = "\x00not-defined"

// knows reports whether the index has a definition under column's exact name.
//
// Exact, deliberately. The flattener derives cf_<name>_id and cf_<name>_slug
// from a populated Object field, cf_<name>_count from a populated list, and
// nothing at all from an unset one — a derived column never exists with a
// null value. So of the columns this index is ever asked about, which are the
// ALL-NULL ones (see declaredCustomFieldTypes), none can be a derivative: an
// all-null cf_owner_id is a field named owner_id, whatever cf_owner is, and
// asking whether some base "could derive" it would only be a way to get that
// wrong.
func (i customFieldIndex) knows(column string) bool {
	_, ok := i.known[column]
	return ok
}

// resolveCustomFieldByName looks one custom field up by its exact name and
// merges the answer into the cached index for the branch on ctx — the type if
// the field is Integer/Decimal/Boolean, and the name as known either way, so
// the same column is not asked about again until the index is refetched. A
// lookup that fails leaves the index untouched and reports false, so the
// column is asked about again on the next query — one small request per
// query for as long as NetBox is failing it, the same cost as any other
// failing query — and the caller knows not to cache anything typed from it.
//
// Only reached on a truncated index (see customFieldTypesEntry.truncated),
// where an unknown name may be a definition the walk did not get to.
//
// ctx should already carry the caller's share of customFieldsWaitBudget (see
// declaredCustomFieldTypes): the lookup is a hint like the walk, and a stalled
// endpoint must degrade to inference, not hold the query.
func (p *Provider) resolveCustomFieldByName(ctx context.Context, index customFieldIndex, column string) (customFieldIndex, bool) {
	name := strings.TrimPrefix(column, cfPrefix)
	rows, _, err := p.fetchRows(ctx, customFieldsEndpoint, url.Values{"name": {name}}, 1)
	if err != nil {
		log.DefaultLogger.Warn("netbox: custom-field definition lookup failed; the column falls back to value inference",
			"column", logSafe(column), "error", logSafe(err.Error()))
		return index, false
	}
	// Not a definition until proven otherwise: remembered under the column's
	// name either way, so it is not asked about again until the index is
	// refetched.
	found, foundType := column, customFieldNotDefined
	var typ provider.FieldType
	var declared bool
	for _, raw := range rows {
		var cf struct {
			Name string `json:"name"`
			Type struct {
				Value string `json:"value"`
			} `json:"type"`
		}
		if json.Unmarshal(raw, &cf) != nil || cf.Name != name {
			continue
		}
		foundType = cf.Type.Value
		typ, declared = customFieldType(cf.Type.Value)
	}

	_, branch := p.client.pinBranch(ctx)
	p.mu.Lock()
	e, ok := p.customFieldsByBranch[branch]
	if ok && e.fetchedAt.Equal(index.fetchedAt) {
		// Copy-on-write: the maps are shared with every caller holding this
		// index, and a reader may be ranging over them outside the lock.
		known := make(map[string]string, len(e.known)+1)
		for k, v := range e.known {
			known[k] = v
		}
		known[found] = foundType
		types := e.types
		if declared {
			types = make(map[string]provider.FieldType, len(e.types)+1)
			for k, v := range e.types {
				types[k] = v
			}
			types[found] = typ
		}
		e.known, e.types = known, types
		p.customFieldsByBranch[branch] = e
		index = e.index()
	}
	p.mu.Unlock()
	return index, true
}

// declaredCustomFieldTypes returns the type declarations for nullColumns —
// the cf_* columns of a result that hold NO value in any row, which are the
// only ones a declaration can matter for, since the frame builder lets values
// decide whenever there are any — or nil when there are none to declare;
// whether those declarations are settled (see customFieldTypes), trivially so
// when nullColumns is empty since nothing was waited for; and until when the
// index they came from is valid, zero when no index was consulted. The fetch
// happens only when a result actually needs a declaration, so a query whose
// custom-field columns all carry values never touches extras.
//
// Restricting the question to all-null columns is also what makes the
// unknown-column rule exact rather than heuristic: see knows.
//
// A cf_* column the index has never heard of is the signature of a custom
// field created after the index was fetched — the state the setup step of a
// recipe leaves things in when some other custom-field query warmed the cache
// first — and a successful index is otherwise trusted for customFieldsTTL. So an
// unknown column refreshes the index, at most once per customFieldsRetryTTL
// per branch: a name that is still unknown after a refresh (a field NetBox
// does not define, from a branch or a plugin) costs one fetch a minute, not
// one per query. A KNOWN undeclared column — a Text field — never refreshes.
func (p *Provider) declaredCustomFieldTypes(ctx context.Context, nullColumns []string) (declared map[string]provider.FieldType, settled bool, validUntil time.Time) {
	var cfCols []string
	for _, c := range nullColumns {
		if strings.HasPrefix(c, cfPrefix) {
			cfCols = append(cfCols, c)
		}
	}
	if len(cfCols) == 0 {
		return nil, true, time.Time{}
	}
	// One wait budget for everything this call may wait on — the walk and
	// any by-name lookups after it. They are hints, and however many of them
	// a result needs, a stalled endpoint costs the query at most
	// customFieldsWaitBudget before it goes out with value inference.
	waitCtx, cancel := context.WithTimeout(ctx, p.customFieldsWait())
	defer cancel()
	index := p.customFieldTypes(waitCtx)
	if index.ok && index.truncated {
		// The walk did not reach every definition, so an unknown name is not
		// evidence of anything; ask for it directly, on what is left of the
		// budget. Each name is asked once per index lifetime, whatever the
		// answer.
		for _, c := range cfCols {
			if !index.knows(c) {
				resolved, ok := p.resolveCustomFieldByName(waitCtx, index, c)
				if !ok {
					// The column's type is not known and not settled: a
					// field list typed from this answer must not be cached.
					index.settled = false
					continue
				}
				index = resolved
			}
		}
	} else if index.ok && time.Since(index.fetchedAt) < customFieldsRetryTTL {
		// Too young to refresh. An unknown column is still an unknown
		// column, though: cap how long anything typed from this answer may
		// be cached at the moment the refresh window opens, so a field list
		// stored now is re-sampled then rather than at the index's expiry.
		for _, c := range cfCols {
			if !index.knows(c) {
				if window := index.fetchedAt.Add(customFieldsRetryTTL); window.Before(index.validUntil) {
					index.validUntil = window
				}
				break
			}
		}
	} else if index.ok {
		for _, c := range cfCols {
			if !index.knows(c) {
				log.DefaultLogger.Info("netbox: custom-field column absent from the definition index; refreshing",
					"column", logSafe(c))
				// Unlike a cold cache, the index in hand already holds usable
				// declarations. If the refresh does not settle inside the
				// budget, keep them: the known columns stay typed from the
				// index they were typed from a moment ago, and only the
				// unknown one falls back to inference. The refresh carries on
				// and fills the cache for the next caller — which is why the
				// answer is then reported UNSETTLED even though its
				// declarations are real: the unknown column's type is still
				// in flight, and a field list cached now would keep its
				// string fallback long after the refresh has landed.
				// Adopt the refresh only when it succeeded: a stalled one is
				// still in flight and a failed one has published the old
				// index back (see customFieldsFlight.previous). Either way
				// the known columns keep their declarations and the answer
				// is reported unsettled, so no field list is cached on it.
				if refreshed := p.refreshCustomFieldTypes(waitCtx, index); refreshed.ok && !refreshed.refreshFailed {
					index = refreshed
				} else {
					index.settled = false
				}
				break
			}
		}
	}
	for _, c := range cfCols {
		if t, ok := index.types[c]; ok {
			if declared == nil {
				declared = map[string]provider.FieldType{}
			}
			declared[c] = t
		}
	}
	return declared, index.settled, index.validUntil
}
