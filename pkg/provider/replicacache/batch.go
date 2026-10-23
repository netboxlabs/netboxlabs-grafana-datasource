package replicacache

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// readStats is what a result read from several tables knows of its age: the
// oldest instant any read reflects, and whether some read reflects none it
// could state. A topology is as old as its oldest part — a link read from a
// table an hour behind is an hour old, however fresh the devices are — and of
// unknown age when any part is.
type readStats struct {
	asOf    *time.Time
	unknown bool
}

// note records one read's instant; nil is a read of unknown age.
func (s *readStats) note(t *time.Time) {
	if t == nil {
		s.unknown = true
		return
	}
	if s.asOf == nil || t.Before(*s.asOf) {
		oldest := *t
		s.asOf = &oldest
	}
}

// dataAsOf is the instant the whole result reflects: the oldest of its reads,
// nil when any read's age is unknown.
func (s *readStats) dataAsOf() *time.Time {
	if s.unknown {
		return nil
	}
	return s.asOf
}

// merge notes what another's reads reflect, for a result that waited on reads
// made on its behalf by a flight it shared.
func (s *readStats) merge(o readStats) {
	s.unknown = s.unknown || o.unknown
	if o.asOf != nil {
		s.note(o.asOf)
	}
}

// listBatched reads every row of objectType whose filterParam column holds one
// of values, in as many requests as the values need: comma-joined in the one
// key the replica takes (a repeated key is its 400), each request within the
// budget beside params (fitsBudget, checked once per request) and carrying at
// most maxValues values (0 is no bound), each walked by its cursor up to
// ceiling rows. Rows come back batch by batch, once each by primary key: the
// replica applies changes between requests, so a row can move from one batch's
// values to another's and be read twice.
//
// capped says some single value had more rows than ceiling, so rows are
// missing. A batch whose values only TOGETHER pass it is split in half and
// read again, as NetBox mode splits a device batch, so that the cap means what
// it means there — one device with more links than one read returns — and not
// how many values this datasource chose to put in one request. The split is
// decided on the batch's first page, whose count is the whole batch's, so it
// costs that page and no more.
//
// A table that has no data on this tenant is no rows, not a failure: an empty
// NetBox table is never fed, so a tenant without wireless links or patch
// panels has those tables unfed and its topology is complete. The catalogue
// says so without a request; a catalogue up to ten minutes stale is corrected
// by the route's own 404. Any other failure, on any batch, fails the read.
func (p *Provider) listBatched(ctx context.Context, objectType, filterParam string, values []string, maxValues, ceiling int, params url.Values, st *readStats) (rows []json.RawMessage, capped bool, err error) {
	values = distinct(values)
	if len(values) == 0 {
		return nil, false, nil
	}
	e, _, err := p.entityFor(ctx, objectType)
	if err != nil {
		return nil, false, err
	}
	if !e.Ingested {
		return nil, false, nil
	}
	batches, err := batchValues(filterParam, values, maxValues, params)
	if err != nil {
		return nil, false, err
	}
	// A stack, so that a split batch's halves are read next and in order, and
	// rows come back in the order of the values.
	work := slices.Clone(batches)
	slices.Reverse(work)
	pk := e.pk()
	seen := map[int]bool{}
	for len(work) > 0 {
		batch := work[len(work)-1]
		work = work[:len(work)-1]
		q := withParam(params, filterParam, strings.Join(batch, ","))

		w := newWalker()
		got, err := p.client.walkOn(ctx, objectType, q, scan{want: ceiling, scanCap: pageSize}, w)
		if err == nil && !got.exhausted {
			if got.total > ceiling && len(batch) > 1 {
				mid := len(batch) / 2
				work = append(work, batch[mid:], batch[:mid])
				continue
			}
			got, err = p.client.walkOn(ctx, objectType, q, scan{want: ceiling}, w)
		}
		if err != nil {
			if isNotReplicated(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		capped = capped || !got.exhausted
		// The response's instant, else the catalogue's, as Query reads it.
		asOf := got.asOf
		if asOf == nil {
			asOf = e.DataAsOf
		}
		st.note(asOf)

		inBatch := map[int]bool{}
		for _, raw := range got.rows {
			id, err := rowID(raw, pk)
			if err != nil {
				return nil, false, &TransportError{Op: "reading " + objectType, Err: err, Message: rowShapeGuidance}
			}
			// Twice in one walk is the protocol violation flattenRows refuses
			// (one object came back twice, so another never did); twice
			// across batches is one object two requests matched.
			if inBatch[id] {
				return nil, false, &TransportError{Op: "reading " + objectType, Err: errDuplicateRow, Message: rowShapeGuidance}
			}
			inBatch[id] = true
			if seen[id] {
				continue
			}
			seen[id] = true
			rows = append(rows, raw)
		}
	}
	return rows, capped, nil
}

// isNotReplicated reports the replica's 404 for a table that has never
// received data: no rows, to a read that treats an unfed table so.
func isNotReplicated(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Classification().Kind == provider.ErrorKindNotReplicated
}

// listByIDs reads every row whose column holds one of ids. Each id names few
// rows — one on a primary key, a cable's ends on cable_id — so the ceiling is
// the provider's own, and a batch, which the budget holds to under two thousand
// ids, does not reach it.
func (p *Provider) listByIDs(ctx context.Context, objectType, column string, ids []string, params url.Values, st *readStats) ([]json.RawMessage, bool, error) {
	return p.listBatched(ctx, objectType, param(column, "in"), ids, 0, MaxLimit, params, st)
}

// batchValues divides values, in order, into the batches one request each
// carries: as many as fit the budget beside params, and at most maxValues.
// A value's cost is exact: url.Values.Encode escapes byte by byte, so a
// comma-joined list costs its values' escapes plus one escaped comma between
// each. Every batch is then put to fitsBudget, the one authority on it, so a
// single value too long to send is refused here, naming its filter, rather
// than by the walk.
func batchValues(filterParam string, values []string, maxValues int, params url.Values) ([][]string, error) {
	room := queryBudget - cursorReserve - len(withParam(params, filterParam, "").Encode())
	comma := len(url.QueryEscape(","))
	var (
		out  [][]string
		cur  []string
		used int
	)
	for _, v := range values {
		cost := len(url.QueryEscape(v))
		if len(cur) > 0 && (used+comma+cost > room || (maxValues > 0 && len(cur) >= maxValues)) {
			out, cur, used = append(out, cur), nil, 0
		}
		if len(cur) > 0 {
			cost += comma
		}
		cur, used = append(cur, v), used+cost
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	for _, b := range out {
		if q := withParam(params, filterParam, strings.Join(b, ",")); !fitsBudget(q) {
			return nil, overBudget(q)
		}
	}
	return out, nil
}

// withParam is params with key set to value, params itself untouched.
func withParam(params url.Values, key, value string) url.Values {
	q := make(url.Values, len(params)+1)
	for k, vs := range params {
		q[k] = vs
	}
	q.Set(key, value)
	return q
}

// distinct is values without repeats, in order of first appearance.
func distinct(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// rowID reads a row's primary key by the rule every consumer of these rows
// applies (toInt): a positive whole number below 2^53.
func rowID(raw json.RawMessage, pk string) (int, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return 0, err
	}
	if obj == nil {
		return 0, errMalformedRow
	}
	var v interface{}
	if err := json.Unmarshal(obj[pk], &v); err != nil {
		return 0, errRowWithoutID
	}
	id, ok := toInt(v)
	if !ok {
		return 0, errRowWithoutID
	}
	return id, nil
}
