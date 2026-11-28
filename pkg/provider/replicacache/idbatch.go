package replicacache

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Some queries are read in disjoint parts and merged: a long id filter, in
// batches of ids, as NetBox mode reads one (its idbatch.go), and
// has_primary_ip=true, as its two halves (a primary IPv4 address set; none, and
// a primary IPv6 set), since the replica ANDs its filters and NetBox's ORs two
// columns. The parts share no row, so their totals sum exactly, which a count
// (an alert rule's) needs, and the rows merge into what one read would return:
// by primary key, the replica's own order, when unsorted.

// minIDRoom is the least room the other parameters must leave for ids. Below
// it a long id list is refused as too long, as before, rather than read in
// hundreds of requests of a few ids each.
const minIDRoom = 1024

// idBatchWorkers is how many batches are read at once.
const idBatchWorkers = 4

// opEitherSet is has_primary_ip=true in the replica's terms: Field or Value
// (another column) is set. Query reads it as two parts (eitherSetParts); it is
// never sent.
const opEitherSet = "either is set"

// The columns NetBox's _has_primary_ip reads, on every model that has it.
const (
	primaryIP4Column = "primary_ip4_id"
	primaryIP6Column = "primary_ip6_id"
)

// hasPrimaryIPMethod is NetBox's _has_primary_ip filter method (devices,
// virtual machines, virtual device contexts): true is a primary IPv4 or IPv6
// address set, false neither.
const hasPrimaryIPMethod = "_has_primary_ip"

// hasPrimaryIP answers has_primary_ip: false as both primary columns empty,
// true as opEitherSet. The last value wins and only NetBox's boolean spellings
// count, as for NetBox's other boolean filters.
func (tr translation) hasPrimaryIP(name string, first provider.Filter, values []string) ([]provider.Filter, bool, error) {
	last := values[len(values)-1]
	b, ok := netboxBool(last)
	if !ok {
		return refused(first, fmt.Sprintf("NetBox ignores %q for %s and returns every row; use true or false", last, name))
	}
	if !b {
		return []provider.Filter{
			{Field: primaryIP4Column, Operator: opIsNull, Label: first.Field, LabelOperator: first.Operator},
			{Field: primaryIP6Column, Operator: opIsNull, Label: first.Field, LabelOperator: first.Operator},
		}, false, nil
	}
	return []provider.Filter{{Field: primaryIP4Column, Operator: opEitherSet, Value: primaryIP6Column, Label: first.Field, LabelOperator: first.Operator}}, false, nil
}

// eitherSetParts takes an opEitherSet filter out of filters and returns what
// each of its parts adds: the first column set; the first empty and the other
// set. nil when there is none.
func eitherSetParts(filters []provider.Filter) ([]provider.Filter, [][]provider.Filter) {
	i := slices.IndexFunc(filters, func(f provider.Filter) bool { return f.Operator == opEitherSet })
	if i < 0 {
		return filters, nil
	}
	f := filters[i]
	rest := slices.Delete(slices.Clone(filters), i, i+1)
	set := func(col string) provider.Filter {
		return provider.Filter{Field: col, Operator: opNotNull, Label: f.Label, LabelOperator: f.LabelOperator}
	}
	unset := provider.Filter{Field: f.Field, Operator: opIsNull, Label: f.Label, LabelOperator: f.LabelOperator}
	return rest, [][]provider.Filter{{set(f.Field)}, {unset, set(f.Value)}}
}

// primaryKeyIDs is the distinct values the primary-key equality filters name,
// numbers as numbers (7 and 007 are one id, written 7), in first-seen order.
func primaryKeyIDs(filters []provider.Filter, pk string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range filters {
		if f.Field != pk || !equalityOperator(f.Operator) {
			continue
		}
		for _, v := range splitValues(f.Value) {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				v = strconv.FormatInt(n, 10)
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// tooManyIDsError is an id filter naming more distinct ids than one query
// reads, refused before any request with NetBox mode's error kind, so the
// plugin says the same thing in either mode.
type tooManyIDsError struct{ ids int }

func (e *tooManyIDsError) Error() string {
	return fmt.Sprintf("replica-cache: the id filter names %d ids, more than the %d one query reads", e.ids, MaxLimit)
}

// Classification reports it as the query's: it can't be answered as written.
func (e *tooManyIDsError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{Kind: provider.ErrorKindTooManyIDs, IDs: e.ids, MaxIDs: MaxLimit}
}

// partRead is what one part of a query read, or the merge of several.
type partRead struct {
	raws     []json.RawMessage
	total    int
	asOf     *time.Time
	warnings []string
	// fields is the projection the rows were read with when it differs from
	// the query's (a negated read adds the negated columns); nil otherwise.
	fields []string
	// reads and ids are how many requests the rows came from, and over how
	// many ids when they were batches; for the note on a sorted read.
	reads, ids int
	// uncounted is a read the replica couldn't count (a negation on a caller
	// that can do without a total): its total is no count, and nor is a sum
	// with it.
	uncounted bool
}

// partReader reads a query's parts, once its filters are translated: what
// every part shares.
type partReader struct {
	p     *Provider
	spec  provider.QuerySpec
	e     entity
	c     *catalog
	plan  request
	limit int
	// keepAll is a read whose rows are put in order after the merge (a
	// natural sort): every part's rows are kept for Query to sort and cut.
	keepAll bool
}

// readParts reads each part (filters plus its extra) and merges them.
func (r *partReader) readParts(ctx context.Context, filters []provider.Filter, extras [][]provider.Filter, none bool) (partRead, error) {
	if extras == nil {
		return r.readPart(ctx, filters, none)
	}
	reads := make([]partRead, 0, len(extras))
	for _, extra := range extras {
		got, err := r.readPart(ctx, append(slices.Clone(filters), extra...), none)
		if err != nil {
			return partRead{}, err
		}
		reads = append(reads, got)
	}
	return r.merge(reads)
}

// readPart is one part's read: the per-request steps Query always ran
// (comparisons, negations, validation, the subtraction of negations, the
// request built), then one read, or batches of its ids when the request with
// them is too long.
func (r *partReader) readPart(ctx context.Context, filters []provider.Filter, none bool) (partRead, error) {
	e, c := r.e, r.c
	none = none || nullAndNotNull(filters)
	var negs []negation
	if e.Ingested {
		var (
			unsatisfiable bool
			err           error
		)
		if filters, unsatisfiable, err = rewriteComparisons(filters, e, c); err != nil {
			return partRead{}, err
		}
		none = none || unsatisfiable
		filters, negs = splitNegations(filters)
		if err := validateFilters(filters, e, c); err != nil {
			return partRead{}, err
		}
		if err := validateNegations(negs, e, c); err != nil {
			return partRead{}, err
		}
		if !none {
			filters, negs, none = subtractNegations(filters, negs, e, c)
		}
	} else {
		if err := validateTextOperators(filters, c); err != nil {
			return partRead{}, err
		}
		if err := validateContainsBuild(filters, c); err != nil {
			return partRead{}, err
		}
	}
	q, err := r.build(filters)
	if err != nil {
		return partRead{}, err
	}
	if none {
		return partRead{}, nil
	}
	if !fitsBudget(q) {
		if ids, rest, ok := r.batchable(filters); ok {
			return r.readBatches(ctx, rest, negs, ids)
		}
		// Too long and not batchable: the read refuses it, naming the filter
		// that made it long, as before.
	}
	return r.readOnce(ctx, filters, negs, q, r.limit)
}

// build is the request for filters: their values, expand=, and for a read of
// rows its sort and projection. expand= goes with every query, count-only
// included: a filter on an expanded name is only valid under it, and the alert
// Count path sends the rule's filters with CountOnly. The sort and the
// projection are pointless when nothing but the total is read.
func (r *partReader) build(filters []provider.Filter) (url.Values, error) {
	q, err := buildFilterValues(filters, addressField(r.e, r.c))
	if err != nil {
		return nil, err
	}
	if len(r.plan.expand) > 0 {
		q.Set("expand", strings.Join(r.plan.expand, ","))
	}
	if !r.spec.CountOnly {
		if r.plan.sort != "" {
			q.Set("sort", r.plan.sort)
		}
		q = withFields(q, r.plan.fields)
	}
	return q, nil
}

// readOnce is one read of filters: the negated route when there are
// negations, else one list walk.
func (r *partReader) readOnce(ctx context.Context, filters []provider.Filter, negs []negation, q url.Values, limit int) (partRead, error) {
	if len(negs) > 0 {
		got, err := r.p.queryNegated(ctx, negatedQuery{spec: r.spec, e: r.e, c: r.c, filters: filters, negs: negs,
			expand: r.plan.expand, countExpand: expandList(r.e, filterExpansions(r.e, r.spec.Filters)), sort: r.plan.sort,
			fields: r.plan.fields, limit: limit, isAddress: addressField(r.e, r.c)})
		if err != nil {
			return partRead{}, err
		}
		// A total below the rows read is the route's "not counted" (an
		// uncountable negation for a caller that can do without one).
		return partRead{raws: got.raws, total: got.total, asOf: got.asOf, warnings: got.warnings, fields: got.fields, reads: 1,
			uncounted: got.total < len(got.raws)}, nil
	}
	raws, total, asOf, err := r.p.client.list(ctx, r.spec.ObjectType, q, limit)
	if err != nil {
		return partRead{}, err
	}
	return partRead{raws: raws, total: total, asOf: asOf, reads: 1}, nil
}

// batchable reports whether filters' id list can be read in batches: more
// than one id on the primary key, and at least minIDRoom left for ids beside
// every other parameter. ids are sorted, numbers as numbers, so batches come
// back in the replica's own order; rest is filters without them.
func (r *partReader) batchable(filters []provider.Filter) (ids []string, rest []provider.Filter, ok bool) {
	pk := r.e.pk()
	ids = primaryKeyIDs(filters, pk)
	if len(ids) < 2 {
		return nil, nil, false
	}
	for _, f := range filters {
		if f.Field != pk || !equalityOperator(f.Operator) {
			rest = append(rest, f)
		}
	}
	q, err := r.build(rest)
	if err != nil || queryBudget-cursorReserve-len(withParam(q, param(pk, "in"), "").Encode()) < minIDRoom {
		return nil, nil, false
	}
	slices.SortFunc(ids, compareIDs)
	return ids, rest, true
}

// compareIDs orders ids as the replica orders its primary key: numbers by
// value, before anything else, which goes by its bytes.
func compareIDs(a, b string) int {
	x, errX := strconv.ParseInt(a, 10, 64)
	y, errY := strconv.ParseInt(b, 10, 64)
	switch {
	case errX == nil && errY == nil:
		return cmp.Compare(x, y)
	case errX == nil:
		return -1
	case errY == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// readBatches reads rest with ids in batches under the budget, a few at a
// time, in order. Unsorted, once the rows read reach the limit the remaining
// batches are read for their count only: the batches are in id order, so the
// rows kept are the ones one read would keep. Sorted, each batch reads up to the
// limit, and Query says each was sorted on its own (a natural sort is redone
// over them all).
func (r *partReader) readBatches(ctx context.Context, rest []provider.Filter, negs []negation, ids []string) (partRead, error) {
	pk := r.e.pk()
	base, err := r.build(rest)
	if err != nil {
		return partRead{}, err
	}
	batches, err := batchValues(param(pk, "in"), ids, 0, base)
	if err != nil {
		return partRead{}, err
	}
	results := make([]partRead, len(batches))
	rows := 0
	for start := 0; start < len(batches); start += idBatchWorkers {
		end := min(start+idBatchWorkers, len(batches))
		limit := r.limit
		countOnly := r.keyOrdered() && rows >= r.limit
		if countOnly {
			limit = 1
		}
		if err := r.readWave(ctx, rest, negs, batches, results, start, end, limit); err != nil {
			return partRead{}, err
		}
		for i := start; i < end; i++ {
			if countOnly {
				results[i].raws = nil
			}
			rows += len(results[i].raws)
		}
	}
	got, err := r.merge(results)
	if err != nil {
		return partRead{}, err
	}
	got.ids = len(ids)
	return got, nil
}

// readWave reads batches[start:end] at once into results. A failure stops the
// wave's other reads, and the first failure is returned, not a sibling's
// cancellation.
func (r *partReader) readWave(ctx context.Context, rest []provider.Filter, negs []negation, batches [][]string, results []partRead, start, end, limit int) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i := start; i < end; i++ {
		wg.Go(func() {
			got, err := r.readBatch(wctx, rest, negs, batches[i], limit)
			if err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			results[i] = got
		})
	}
	wg.Wait()
	return firstErr
}

// readBatch reads rest with one batch of ids. A batch is sized for the plain
// request; a negated read adds parameters of its own (a choice's other values,
// a count's), and one it makes too long is refused before it is sent, so the
// batch is split in half and each half read, in order.
func (r *partReader) readBatch(ctx context.Context, rest []provider.Filter, negs []negation, ids []string, limit int) (partRead, error) {
	filters := append(slices.Clone(rest), provider.Filter{Field: r.e.pk(), Value: strings.Join(ids, ","), Label: "id"})
	q, err := r.build(filters)
	if err != nil {
		return partRead{}, err
	}
	got, err := r.readOnce(ctx, filters, negs, q, limit)
	if isTooLong(err) && len(ids) > 1 {
		mid := len(ids) / 2
		first, err := r.readBatch(ctx, rest, negs, ids[:mid], limit)
		if err != nil {
			return partRead{}, err
		}
		second, err := r.readBatch(ctx, rest, negs, ids[mid:], limit)
		if err != nil {
			return partRead{}, err
		}
		return r.merge([]partRead{first, second})
	}
	if err != nil {
		return partRead{}, err
	}
	return got, checkBatchTotal(r.spec.ObjectType, len(ids), got.total)
}

// checkBatchTotal refuses a batch that matched more rows than it named ids:
// each id names at most one row, so the id filter wasn't applied, and the
// batches would repeat rows. The replica refuses a filter it can't apply, so
// this is a check of its answer, as the client checks the rest of it.
func checkBatchTotal(objectType string, ids, total int) error {
	if total > ids {
		return &TransportError{Op: "reading " + objectType, Err: fmt.Errorf("a batch of %d ids matched %d rows", ids, total), Message: rowShapeGuidance}
	}
	return nil
}

// merge joins disjoint reads. Totals sum; the age is the oldest, unknown when
// any read's is (the catalogue's instant standing in for a read that had none,
// as Query reads one); the warnings and the projection carry over. Unsorted, the
// rows merge by primary key, the replica's order, and are cut at the limit, so
// they are what one read returns. Sorted, they are put one read after another
// and cut, unless keepAll leaves the cut to Query.
func (r *partReader) merge(reads []partRead) (partRead, error) {
	reads, err := r.sameProjection(reads)
	if err != nil {
		return partRead{}, err
	}
	var out partRead
	known := true
	for i, got := range reads {
		out.raws = append(out.raws, got.raws...)
		out.total += got.total
		for _, w := range got.warnings {
			if !slices.Contains(out.warnings, w) {
				out.warnings = append(out.warnings, w)
			}
		}
		out.reads += got.reads
		// Parts of one query batch the same ids: counted once.
		out.ids = max(out.ids, got.ids)
		out.uncounted = out.uncounted || got.uncounted
		if out.fields == nil {
			out.fields = got.fields
		}
		asOf := cmp.Or(got.asOf, r.e.DataAsOf)
		switch {
		case asOf == nil:
			known = false
		case i == 0 || out.asOf == nil || asOf.Before(*out.asOf):
			out.asOf = asOf
		}
	}
	if !known {
		out.asOf = nil
	}
	if out.uncounted {
		out.total = 0
	}
	if len(reads) > 1 {
		pk := r.e.pk()
		keys := make([]int, len(out.raws))
		for i, raw := range out.raws {
			id, err := rowID(raw, pk)
			if err != nil {
				return partRead{}, &TransportError{Op: "reading " + r.spec.ObjectType, Err: err, Message: rowShapeGuidance}
			}
			keys[i] = id
		}
		order := make([]int, len(out.raws))
		for i := range order {
			order[i] = i
		}
		if r.keyOrdered() {
			slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(keys[a], keys[b]) })
		}
		// A row the replica moved from one part to another between their
		// requests (has_primary_ip's, as an address is changed) is one row,
		// kept where it was first read, and counted once.
		seen := make(map[int]bool, len(order))
		merged := make([]json.RawMessage, 0, len(order))
		for _, j := range order {
			if seen[keys[j]] {
				if !out.uncounted && out.total > 0 {
					out.total--
				}
				continue
			}
			seen[keys[j]] = true
			merged = append(merged, out.raws[j])
		}
		out.raws = merged
	}
	if !r.keepAll && len(out.raws) > r.limit {
		out.raws = out.raws[:r.limit]
	}
	return out, nil
}

// sameProjection gives every read the same columns. A negated read can project
// the negated columns in, to test its rows, and reads of one query can take
// different routes (one batch's negation answered by counts, another's by
// testing rows), so when they differ the rows that carry extra columns lose
// them and the query's own projection is what they were read with.
func (r *partReader) sameProjection(reads []partRead) ([]partRead, error) {
	if len(reads) < 2 || r.plan.fields == nil {
		return reads, nil
	}
	same := true
	for _, got := range reads[1:] {
		same = same && slices.Equal(got.fields, reads[0].fields)
	}
	if same {
		return reads, nil
	}
	out := slices.Clone(reads)
	for i, got := range out {
		var drop []string
		for _, f := range got.fields {
			if !slices.Contains(r.plan.fields, f) {
				drop = append(drop, f)
			}
		}
		if len(drop) > 0 {
			raws := make([]json.RawMessage, len(got.raws))
			for j, raw := range got.raws {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
					return nil, &TransportError{Op: "reading " + r.spec.ObjectType, Err: errMalformedRow, Message: rowShapeGuidance}
				}
				for _, k := range drop {
					delete(obj, k)
				}
				b, err := json.Marshal(obj)
				if err != nil {
					return nil, err
				}
				raws[j] = b
			}
			out[i].raws = raws
		}
		out[i].fields = nil
	}
	return out, nil
}

// keyOrdered is whether the rows' order is the primary key's: unsorted (the
// replica's own order) or sorted by it, ascending. Batches of sorted ids then
// come back in order, so a merge by key is what one read returns.
func (r *partReader) keyOrdered() bool {
	return r.plan.sort == "" || r.plan.sort == r.e.pk()
}

// partsSortedNote says a sorted read made in several requests was sorted by
// the replica within each, and, when the limit cut it, that the rows kept are
// the first requests', not the top ones by the sort.
func partsSortedNote(got partRead, total, shown int) string {
	what := fmt.Sprintf("Read in %d requests", got.reads)
	if got.ids > 0 {
		what += fmt.Sprintf(" (%s ids)", provider.HumanInt(got.ids))
	}
	if total > shown {
		how := "Raise the Limit to cover them all."
		if total > MaxLimit {
			how = fmt.Sprintf("Narrow the filters to %s rows or fewer to read them all.", provider.HumanInt(MaxLimit))
		}
		return fmt.Sprintf("%s, sorted by the replica within each, and cut at the Limit: the rows shown are the first requests', not the top %s by the sort. %s",
			what, provider.HumanInt(shown), how)
	}
	return what + ", sorted by the replica within each; sort in the panel to order the whole table."
}
