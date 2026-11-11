package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// changeScanCapRows bounds a change-log read whose rows are filtered here rather
// than by NetBox (two Actions, or Changed fields): it scans at most this many
// rows, and the track then ends with a cut-off marker. 10,000 rows across two
// reads timed out on busy instances, and a timed-out annotation query shows
// nothing at all.
const changeScanCapRows = 2500

// changeScanCap is changeScanCapRows unless a test has lowered it.
func (p *Provider) changeScanCap() int {
	if p.changeScanCapOverride > 0 {
		return p.changeScanCapOverride
	}
	return changeScanCapRows
}

// walkRows reads a list endpoint in offset pages and hands each row to visit, in
// the order NetBox returns them. It stops when visit returns false, after
// maxRows rows, or at the end of the list. Each page asks for no more rows than
// are left to read, so the walk fetches no row it won't visit.
//
// Offset pages shift when rows are added mid-walk, so a page can start with rows
// the page before ended with. A row whose id the walk has already visited is
// skipped: visit doesn't see it and it doesn't count toward maxRows, so repeats
// can't cut a walk short. A row without an id is never a repeat. After maxRows
// repeats the walk ends with more set, so a NetBox that ignores offset and
// serves the same page forever can't loop it.
//
// scanned counts the rows visited. more is true when the walk stopped with rows
// or pages it hadn't visited.
//
// fetchList pages the same way, but collects its rows and keeps the envelope's
// count; this visits rows as they arrive, so the caller can stop on what it has
// found.
func (p *Provider) walkRows(ctx context.Context, endpoint string, q url.Values, maxRows int, visit func(json.RawMessage) bool) (scanned int, more bool, err error) {
	if maxRows <= 0 {
		return 0, false, nil
	}
	pq := url.Values{}
	for k, vs := range q {
		pq[k] = vs
	}
	pq.Set("limit", strconv.Itoa(min(pageSize, maxRows)))
	next := p.client.apiURL(endpoint, pq)
	seen := map[int64]bool{}
	repeats := 0
	for {
		page, err := p.getListPageRetry(ctx, next)
		if err != nil {
			return scanned, false, err
		}
		hasNext := page.Next != nil && *page.Next != ""
		for i, raw := range page.Results {
			var row struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(raw, &row) == nil && row.ID != 0 {
				if seen[row.ID] {
					if repeats++; repeats == maxRows {
						return scanned, true, nil
					}
					continue
				}
				seen[row.ID] = true
			}
			scanned++
			if !visit(raw) || scanned == maxRows {
				return scanned, i < len(page.Results)-1 || hasNext, nil
			}
		}
		// An empty page ends the walk too, so a malformed next link can't loop.
		if !hasNext || len(page.Results) == 0 {
			return scanned, false, nil
		}
		if next, err = p.client.nextPageURL(next, *page.Next); err != nil || next == "" {
			return scanned, false, err
		}
		if next, err = withLimit(next, min(pageSize, maxRows-scanned)); err != nil {
			return scanned, false, err
		}
	}
}

// withLimit sets a page URL's limit parameter. NetBox's next link repeats the
// limit of the page before it; offset paging counts rows, so a smaller limit on
// the last page reads the same rows, only fewer of them.
func withLimit(rawURL string, limit int) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse page URL: %w", err)
	}
	q := u.Query()
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()
	return u.String(), nil
}
