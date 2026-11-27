package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// idServer is a NetBox list of devices 1..n that filters by repeated id
// values (unless ignoreID), pages by limit and offset with a count and next
// link, and records each request's raw query.
type idServer struct {
	n           int
	ignoreID    bool
	failFor     string // an id: a request carrying it answers 400
	honorFields bool   // rows carry only the ?fields= properties, as NetBox's do

	mu      sync.Mutex
	queries []string
}

func (s *idServer) start(t *testing.T, opts ...Option) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.RawQuery)
		s.mu.Unlock()
		q := r.URL.Query()
		if s.failFor != "" && slices.Contains(q["id"], s.failFor) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"id":["Select a valid choice."]}`)
			return
		}
		// Ids are read as numbers, as NetBox reads them.
		want := map[int]bool{}
		for _, v := range q["id"] {
			if n, err := strconv.Atoi(v); err == nil {
				want[n] = true
			}
		}
		var ids []int
		for i := 1; i <= s.n; i++ {
			if s.ignoreID || len(q["id"]) == 0 || want[i] {
				ids = append(ids, i)
			}
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		end := min(len(ids), offset+limit)
		var rows []string
		for _, id := range ids[min(offset, len(ids)):end] {
			obj := map[string]string{"id": strconv.Itoa(id), "name": fmt.Sprintf("%q", fmt.Sprintf("d%d", id))}
			var parts []string
			for _, k := range []string{"id", "name"} {
				if !s.honorFields || q.Get("fields") == "" || slices.Contains(strings.Split(q.Get("fields"), ","), k) {
					parts = append(parts, fmt.Sprintf("%q:%s", k, obj[k]))
				}
			}
			rows = append(rows, "{"+strings.Join(parts, ",")+"}")
		}
		next := "null"
		if end < len(ids) {
			q.Set("offset", strconv.Itoa(end))
			next = strconv.Quote("http://" + r.Host + r.URL.Path + "?" + q.Encode())
		}
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":%s,"previous":null,"results":[%s]}`, len(ids), next, strings.Join(rows, ","))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", &http.Client{Timeout: 5 * time.Second}, opts...)
}

func (s *idServer) requests() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]url.Values, len(s.queries))
	for i, raw := range s.queries {
		out[i], _ = url.ParseQuery(raw)
	}
	return out
}

func (s *idServer) rawQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries)
}

// idFilter is an id filter on ids from..to.
func idFilter(from, to int) provider.Filter {
	var vals []string
	for i := from; i <= to; i++ {
		vals = append(vals, strconv.Itoa(i))
	}
	return provider.Filter{Field: "id", Value: strings.Join(vals, ",")}
}

func idsOf(t *testing.T, res *provider.Result) []int {
	t.Helper()
	var out []int
	for _, r := range res.Rows {
		f, ok := r["id"].(float64)
		if !ok {
			t.Fatalf("row id %#v", r["id"])
		}
		out = append(out, int(f))
	}
	return out
}

func seq(from, to int) []int {
	var out []int
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// An id list that fits one request is sent as before: one request, no note.
func TestQueryShortIDListIsOneRequest(t *testing.T) {
	s := &idServer{n: 50}
	res, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 10)}, Limit: 100})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := idsOf(t, res); !slices.Equal(got, seq(1, 10)) || res.Total != 10 {
		t.Errorf("ids %v, total %d", got, res.Total)
	}
	if n := len(s.rawQueries()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// An id list past the URL budget is read in batches: each request under the
// budget and carrying every other parameter, each row once, the Total summed.
func TestQueryLongIDListIsBatched(t *testing.T) {
	s := &idServer{n: 3000}
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Limit: MaxLimit, Fields: []string{"id", "name"},
		Filters: []provider.Filter{idFilter(1, 3000), {Field: "status", Value: "active"}}}
	res, err := s.start(t).Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := idsOf(t, res); !slices.Equal(got, seq(1, 3000)) || res.Total != 3000 {
		t.Errorf("rows %d (first %v), total %d; want 1..3000 once each", len(got), got[:min(3, len(got))], res.Total)
	}
	raws := s.rawQueries()
	if len(raws) < 2 {
		t.Fatalf("requests = %d, want several batches", len(raws))
	}
	for _, raw := range raws {
		if len(raw) > chunkBudgetBytes {
			t.Errorf("a batch's query is %d bytes, over the %d budget", len(raw), chunkBudgetBytes)
		}
	}
	for _, q := range s.requests() {
		if q.Get("status") != "active" || q.Get("fields") == "" {
			t.Errorf("a batch lost the other parameters: status=%q fields=%q", q.Get("status"), q.Get("fields"))
		}
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes %v, want none without a sort", res.Notes)
	}
}

// Past the Limit the remaining batches are asked for their count only; the rows
// are the first Limit in batch order, and the Total stays the full sum.
func TestQueryBatchedLimitKeepsTheTotal(t *testing.T) {
	s := &idServer{n: 6000}
	res, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 6000)}, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := idsOf(t, res); !slices.Equal(got, seq(1, 1000)) || res.Total != 6000 {
		t.Errorf("rows %d, total %d; want the first 1000 and 6000", len(got), res.Total)
	}
	counted := 0
	for _, q := range s.requests() {
		if q.Get("limit") == "1" {
			counted++
		}
	}
	if counted == 0 {
		t.Errorf("no batch was asked for its count only once the Limit was reached")
	}
}

// A count-only query (an alert rule's Count) sums the batches' counts.
func TestQueryBatchedCount(t *testing.T) {
	s := &idServer{n: 3000}
	res, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 3000)}, CountOnly: true})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 3000 {
		t.Errorf("total = %d, want 3000", res.Total)
	}
}

// An id listed twice is read once.
func TestQueryBatchedDedupesIDs(t *testing.T) {
	s := &idServer{n: 3000}
	f := idFilter(1, 3000)
	f.Value += "," + f.Value
	res, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{f}, Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := idsOf(t, res); !slices.Equal(got, seq(1, 3000)) || res.Total != 3000 {
		t.Errorf("rows %d, total %d; want each id once", len(got), res.Total)
	}
}

// An endpoint that ignores the id filter would repeat its whole table in every
// batch; a batch answering more rows than it has ids is refused.
func TestQueryBatchedRefusesAnIgnoredIDFilter(t *testing.T) {
	s := &idServer{n: 3000, ignoreID: true}
	_, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 2000)}, Limit: MaxLimit})
	if err == nil || !strings.Contains(err.Error(), "id filter") {
		t.Errorf("err = %v, want a refusal naming the id filter", err)
	}
}

// A batch's error is the query's, not a sibling's cancellation, so Query's own
// retries still see what NetBox answered.
func TestQueryBatchedErrorIsTheBatchs(t *testing.T) {
	s := &idServer{n: 3000, failFor: "2500"}
	_, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 3000)}, Limit: MaxLimit})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Errorf("err = %v, want the batch's 400", err)
	}
}

// Fast paging can't sum counts, so a batched read pages by offset: the sort is
// sent, no start parameter is, and the note says each batch is sorted on its
// own. Without a sort there is no note.
func TestQueryBatchedSortsAndSaysSo(t *testing.T) {
	s := &idServer{n: 3000}
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 3000)}, Limit: MaxLimit,
		Ordering: "name", AllowUncounted: true}
	res, err := s.start(t, WithCursorPaging(true)).Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, q := range s.requests() {
		if q.Has("start") || !strings.HasPrefix(q.Get("ordering"), "name") {
			t.Errorf("a batch sent start=%q ordering=%q, want the name sort and no start", q.Get("start"), q.Get("ordering"))
		}
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "batches") || !strings.Contains(res.Notes[0], "3,000 ids") {
		t.Errorf("notes = %v, want the batch sort note", res.Notes)
	}
}

// Only the plain id filter is batched: a negated one (whose values AND) and
// other fields are sent as before.
func TestQueryBatchesOnlyThePlainIDFilter(t *testing.T) {
	for _, f := range []provider.Filter{
		{Field: "id", Operator: "n", Value: idFilter(1, 1500).Value},
		{Field: "name", Value: idFilter(1, 1500).Value},
	} {
		s := &idServer{n: 10}
		_, _ = s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{f}, Limit: 100})
		if n := len(s.rawQueries()); n != 1 {
			t.Errorf("%s%s: requests = %d, want 1", f.Field, f.Operator, n)
		}
	}
}

// When the other parameters leave under a kilobyte for ids, the query is sent
// as before rather than as one request per id.
func TestQueryDoesNotBatchWithoutRoom(t *testing.T) {
	s := &idServer{n: 3000}
	long := provider.Filter{Field: "name", Value: strings.Repeat("a-long-device-name,", 280)}
	_, _ = s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 2000), long}, Limit: MaxLimit})
	if n := firstPages(s.requests()); n != 1 {
		t.Errorf("reads = %d, want 1", n)
	}
}

// firstPages counts the reads among requests: a read's later pages carry an
// offset.
func firstPages(qs []url.Values) int {
	n := 0
	for _, q := range qs {
		if !q.Has("offset") {
			n++
		}
	}
	return n
}

// An endpoint that ignores the id filter and holds fewer objects than a batch
// has ids still answers rows outside the batch: refused too.
func TestQueryBatchedRefusesAnIgnoredIDFilterOnASmallTable(t *testing.T) {
	s := &idServer{n: 5, ignoreID: true}
	_, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1001, 3000)}, Limit: MaxLimit})
	if err == nil || !strings.Contains(err.Error(), "id filter") {
		t.Errorf("err = %v, want a refusal naming the id filter", err)
	}
}

// A sorted batched read cut at its Limit keeps the first batches' rows, not the
// top ones by the sort: the note says so and points at the Limit.
func TestQueryBatchedSortedAndCutSaysSo(t *testing.T) {
	s := &idServer{n: 3000}
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 3000)}, Limit: 100, Ordering: "name"}
	res, err := s.start(t).Query(context.Background(), spec)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "not the top") && strings.Contains(n, "Limit") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want one saying the rows aren't the top ones by the sort", res.Notes)
	}
}

// Repeated ids don't make a list that fits look long: it is sent as one
// ordinary read, so fast paging stays on for it.
func TestQueryRepeatedIDsThatFitAreNotBatched(t *testing.T) {
	s := &idServer{n: 500}
	f := idFilter(1, 500)
	f.Value += "," + f.Value + "," + f.Value
	spec := provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{f}, Limit: MaxLimit, AllowUncounted: true}
	if _, err := s.start(t, WithCursorPaging(true)).Query(context.Background(), spec); err != nil {
		t.Fatalf("Query: %v", err)
	}
	qs := s.requests()
	if firstPages(qs) != 1 || !qs[0].Has("start") || len(qs[0]["id"]) != 500 {
		t.Errorf("first read start=%q with %d ids; want one fast-paged read of 500 distinct ids", qs[0].Get("start"), len(qs[0]["id"]))
	}
}

// An id written with leading zeros is the same object to NetBox, not a sign the
// filter was ignored.
func TestQueryBatchedComparesIDsAsNumbers(t *testing.T) {
	s := &idServer{n: 3000}
	var vals []string
	for i := 1; i <= 3000; i++ {
		vals = append(vals, fmt.Sprintf("%04d", i))
	}
	res, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{{Field: "id", Value: strings.Join(vals, ",")}}, Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 3000 {
		t.Errorf("total = %d, want 3000", res.Total)
	}
}

// A batched read checks each row's id, so it asks for id whatever the Return
// fields: a selection without it isn't refused as an ignored filter.
func TestQueryBatchedProjectionKeepsTheID(t *testing.T) {
	s := &idServer{n: 3000, honorFields: true}
	res, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{idFilter(1, 3000)}, Fields: []string{"name"}, Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 3000 || res.Rows[0]["name"] != "d1" || !slices.Equal(res.Columns, []string{"name"}) {
		t.Errorf("rows %d, first %v, columns %v; want 3000 rows of name only", len(res.Rows), res.Rows[0], res.Columns)
	}
}

// Ids are de-duplicated as NetBox reads them, as numbers: 7 and 007 are one
// object, read once.
func TestDedupeIDsAsNumbers(t *testing.T) {
	if got := dedupeIDs([]string{"7", "007", "8", "x", "x"}); !slices.Equal(got, []string{"7", "8", "x"}) {
		t.Errorf("dedupeIDs = %v", got)
	}
}

// An unsorted batched read cut at its Limit says which rows it kept.
func TestQueryBatchedCutWithoutSortSaysSo(t *testing.T) {
	s := &idServer{n: 3000}
	res, err := s.start(t).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{idFilter(1, 3000)}, Limit: 100})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "batches") && strings.Contains(n, "Limit") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want one saying the rows are the first batches'", res.Notes)
	}
}
