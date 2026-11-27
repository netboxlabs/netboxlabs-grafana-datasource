package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A long id filter is read in batches. An Objects query that filters by many
// ids (a dashboard passing the NetBox ids its metrics carry, so the NetBox side
// reads only the devices the table shows) sends each id as one repeated
// parameter, and past the URL budget the other batchers use (chunkBudgetBytes)
// that one request would be refused. So the ids are split into requests under
// the budget, each carrying every other parameter of the query.
//
// Only the plain id filter: each id names at most one object, so the batches
// never overlap and their counts sum exactly, which a count (an alert rule's)
// needs. Other filters can overlap across batches (tags, tree filters such as
// region_id) and are sent as before.

// idParam is NetBox's primary-key filter; its repeated values OR.
const idParam = "id"

// minIDBudget is the least room the other parameters must leave for ids. Below
// it the query is sent as before, as one request, rather than as one request
// per few ids.
const minIDBudget = 1024

// idBatchWorkers is how many batches are read at once.
const idBatchWorkers = 4

// idBatchingNeeded reports whether q's id filter must be read in batches: it
// holds more than one id, the query doesn't fit the budget with reserve more
// bytes (the sort applyOrdering adds after this), and the other parameters
// leave at least minIDBudget for ids. Query de-duplicates the ids and decides
// this before cursor paging and the sort, since a batched read pages by offset.
func idBatchingNeeded(q url.Values, reserve int) bool {
	if len(q[idParam]) < 2 || len(q.Encode())+pagingParamBytes+reserve <= chunkBudgetBytes {
		return false
	}
	return idRoom(q)-reserve >= minIDBudget
}

// idBatcherFor is the batcher for q's id filter: every other parameter fixed.
func idBatcherFor(q url.Values) queryBatcher {
	fixed := make(url.Values, len(q))
	for k, vs := range q {
		if k != idParam {
			fixed[k] = vs
		}
	}
	return newQueryBatcher(idParam, fixed)
}

// idRoom is the bytes q's other parameters leave for ids.
func idRoom(q url.Values) int {
	return chunkBudgetBytes - idBatcherFor(q).fixedBytes() - pagingParamBytes
}

// dedupeIDs is id filter values without repeats, in first-seen order, compared
// as NetBox reads them: as numbers, so 7 and 007 are one id (the first
// spelling kept). A value that isn't a number is compared as written; NetBox
// refuses it anyway.
func dedupeIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		key := "s:" + v
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			key = strconv.FormatInt(n, 10)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, v)
		}
	}
	return out
}

// fetchIDBatches reads q in batches of its ids, in offset mode. Batches are read
// a few at a time, in order; once the rows read reach limit, the remaining
// batches are asked for their count only (one row each, dropped). Rows merge in
// batch order and are cut at limit, and the total is the batches' sum. A batch
// whose count passes its own id count means NetBox didn't apply the id filter
// (an endpoint that ignores filters would repeat its whole table per batch), and
// the read fails. The first batch error is the read's.
func (p *Provider) fetchIDBatches(ctx context.Context, objectType string, q url.Values, limit int) (listResult, error) {
	ids := dedupeIDs(q[idParam])
	b := idBatcherFor(q)
	chunks := b.chunk(ids)
	if len(chunks) <= 1 {
		return p.fetchList(ctx, objectType, b.query(ids), limit, false)
	}
	out := listResult{totalKnown: true, batches: len(chunks), ids: len(ids)}
	results := make([]listResult, len(chunks))
	for start := 0; start < len(chunks); start += idBatchWorkers {
		end := min(start+idBatchWorkers, len(chunks))
		countOnly := len(out.rows) >= limit
		if err := p.readIDBatchWave(ctx, objectType, b, chunks, results, start, end, limit, countOnly); err != nil {
			return listResult{}, err
		}
		for i := start; i < end; i++ {
			out.rows = append(out.rows, results[i].rows...)
			out.total += results[i].total
			out.totalKnown = out.totalKnown && results[i].totalKnown
		}
	}
	if len(out.rows) > limit {
		out.rows = out.rows[:limit]
	}
	return out, nil
}

// readIDBatchWave reads chunks[start:end] at once into results. A failure
// stops the wave's other reads, and the first failure is returned, not a
// sibling's cancellation.
func (p *Provider) readIDBatchWave(ctx context.Context, objectType string, b queryBatcher, chunks [][]string, results []listResult, start, end, limit int, countOnly bool) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i := start; i < end; i++ {
		wg.Go(func() {
			n := limit
			if countOnly {
				n = 1
			}
			res, err := p.fetchList(wctx, objectType, b.query(chunks[i]), n, false)
			if err == nil && !batchHonoursIDs(res, chunks[i]) {
				err = &idFilterIgnoredError{objectType: objectType, ids: len(chunks[i]), matched: res.total}
			}
			if err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			if countOnly {
				res.rows = nil
			}
			results[i] = res
		})
	}
	wg.Wait()
	return firstErr
}

// batchHonoursIDs reports whether a batch's answer is what its id filter
// allows: no more objects than ids, and every row one of its ids. An endpoint
// that ignores the filter fails one or the other, however small its table. A
// row without an integer id (a caller's own fields filter left it out, or a
// model keyed by something else) can't be checked by id: the count is its
// check.
func batchHonoursIDs(res listResult, ids []string) bool {
	if res.totalKnown && res.total > len(ids) {
		return false
	}
	// Compared as numbers, as NetBox reads them: an id written 007 is row 7.
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64); err == nil {
			set[n] = true
		}
	}
	for _, raw := range res.rows {
		var row struct {
			ID json.RawMessage `json:"id"`
		}
		var id int64
		if json.Unmarshal(raw, &row) != nil || json.Unmarshal(row.ID, &id) != nil {
			continue
		}
		if !set[id] {
			return false
		}
	}
	return true
}

// idBatchNote says a batched read is ordered within each batch only, and, when
// Limit cut it, that the rows kept are the first batches' (with a sort, not the
// top ones by it): NetBox's order (natural sort, a reference by its own
// ordering) can't be reproduced here to merge the batches.
func idBatchNote(page listResult, sorted bool, shown int) string {
	if page.batches < 2 {
		return ""
	}
	cut := page.totalKnown && page.total > shown
	switch {
	case cut && sorted:
		return fmt.Sprintf("Read in %d batches (%s ids), each sorted by NetBox on its own, and cut at the Limit: the rows shown are the first batches', not the top %s by the sort. Raise the Limit to cover every id.",
			page.batches, thousands(page.ids), thousands(shown))
	case cut:
		return fmt.Sprintf("Read in %d batches (%s ids), each in NetBox's own order, and cut at the Limit: the rows shown are the first batches'. Raise the Limit to cover every id.",
			page.batches, thousands(page.ids))
	case sorted:
		return fmt.Sprintf("Read in %d batches (%s ids), each sorted by NetBox on its own; sort in the panel to order the whole table.",
			page.batches, thousands(page.ids))
	}
	return fmt.Sprintf("Read in %d batches (%s ids), each in NetBox's own order; sort in the panel to order the whole table.",
		page.batches, thousands(page.ids))
}

// idFilterIgnoredError is a batch of ids answered with more objects than ids:
// NetBox didn't apply the id filter to this object type.
type idFilterIgnoredError struct {
	objectType   string
	ids, matched int
}

func (e *idFilterIgnoredError) Error() string {
	return fmt.Sprintf("netbox: %s didn't apply the id filter: a batch of %d ids matched %d objects", e.objectType, e.ids, e.matched)
}

// Classification reports it as the query's: it can't be answered as written.
func (e *idFilterIgnoredError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{Kind: provider.ErrorKindIDFilterIgnored}
}
