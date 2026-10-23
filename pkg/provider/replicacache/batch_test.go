package replicacache

import (
	"context"
	"encoding/json"
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

const terminations = "dcim/cable-terminations"

// terminationFake serves dcim/cable-terminations with one row per device id
// given, on that device.
func terminationFake(deviceIDs ...int) *fakeService {
	f := newFakeService()
	f.addTopologyTables()
	rows := make([]map[string]interface{}, 0, len(deviceIDs))
	for i, d := range deviceIDs {
		rows = append(rows, map[string]interface{}{"id": float64(i + 1), "cable_id": float64(i + 1), "cable_end": "A",
			"termination_type_id": float64(9), "termination_id": float64(i + 1), "_device_id": float64(d)})
	}
	f.entities[terminations] = rows
	return f
}

func idStrings(ids []int) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.Itoa(id)
	}
	return out
}

// rowIDs is the primary key of each row, in order.
func rowIDs(t *testing.T, rows []json.RawMessage) []int {
	t.Helper()
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		var obj struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(r, &obj); err != nil {
			t.Fatal(err)
		}
		out = append(out, obj.ID)
	}
	return out
}

// Values that cannot share one request within the budget are spread over as
// many as it takes, each within it, and the rows come back as one answer.
// 3,000 thirteen-digit ids are about 48 KB comma-joined, three requests'
// worth at the least.
func TestListBatched_SplitsUnder16KB(t *testing.T) {
	devices := make([]int, 3000)
	for i := range devices {
		devices[i] = 1_000_000_000_000 + i
	}
	f := terminationFake(devices...)
	p := newTestProvider(t, f)

	var st readStats
	rows, capped, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), idStrings(devices), 0, MaxLimit,
		url.Values{"fields": {"id,_device_id"}}, &st)
	if err != nil {
		t.Fatal(err)
	}
	if capped {
		t.Error("capped, with one row per value")
	}
	if len(rows) != len(devices) {
		t.Errorf("%d rows, want %d", len(rows), len(devices))
	}
	sent := f.sentWith(terminations, "filter[_device_id]__in")
	if len(sent) < 4 {
		t.Errorf("%d requests, want the values spread over at least four", len(sent))
	}
	seen := map[string]bool{}
	for _, r := range sent {
		if !fitsBudget(r.query) {
			t.Errorf("a request of %d bytes was sent, past the budget", len(withoutCursor(r.query).Encode()))
		}
		if r.query.Get("fields") != "id,_device_id" {
			t.Errorf("fields = %q, want every batch to carry the caller's parameters", r.query.Get("fields"))
		}
		if len(r.query["filter[_device_id]__in"]) != 1 {
			t.Errorf("the filter key went %d times; the replica refuses a repeated key", len(r.query["filter[_device_id]__in"]))
		}
		if r.query.Get("cursor") == "" {
			for _, v := range strings.Split(r.query.Get("filter[_device_id]__in"), ",") {
				if seen[v] {
					t.Errorf("value %s went in two batches", v)
				}
				seen[v] = true
			}
		}
	}
	if len(seen) != len(devices) {
		t.Errorf("%d values sent, want %d", len(seen), len(devices))
	}
}

// maxValues bounds a batch by count as well, for a value list the budget
// alone would let grow past what one request should carry.
func TestListBatched_MaxValuesBoundsABatch(t *testing.T) {
	f := terminationFake(1, 2, 3, 4, 5)
	p := newTestProvider(t, f)
	rows, _, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"1", "2", "3", "4", "5"}, 2, MaxLimit, nil, &readStats{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowIDs(t, rows); !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
		t.Errorf("rows %v", got)
	}
	var batches []string
	for _, r := range f.sentWith(terminations, "filter[_device_id]__in") {
		batches = append(batches, r.query.Get("filter[_device_id]__in"))
	}
	if !slices.Equal(batches, []string{"1,2", "3,4", "5"}) {
		t.Errorf("batches %q, want at most two values each, in order", batches)
	}
}

// A batch reads past one page by its cursor, up to its ceiling.
func TestListBatched_WalksCursorsPastOnePage(t *testing.T) {
	f := terminationFake(7, 7, 7, 7, 7)
	f.pageCap = 2
	p := newTestProvider(t, f)
	rows, capped, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"7"}, 0, MaxLimit, nil, &readStats{})
	if err != nil {
		t.Fatal(err)
	}
	if capped || len(rows) != 5 {
		t.Errorf("rows=%d capped=%v, want all five across three pages", len(rows), capped)
	}
	if n := len(f.sentWith(terminations, "cursor")); n != 2 {
		t.Errorf("%d pages followed by cursor, want 2", n)
	}
}

// capped says a value had more rows than one batch may read, so the answer
// lacks some. A batch whose values only TOGETHER pass the ceiling is split and
// read again, not capped: the ceiling is per value, as NetBox mode's is per
// device, and a batch is this datasource's choice, not the reader's.
func TestListBatched_ReportsCapped(t *testing.T) {
	t.Run("one value past the ceiling", func(t *testing.T) {
		f := terminationFake(7, 7, 7, 7, 7)
		p := newTestProvider(t, f)
		rows, capped, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"7"}, 0, 3, nil, &readStats{})
		if err != nil {
			t.Fatal(err)
		}
		if !capped || len(rows) != 3 {
			t.Errorf("rows=%d capped=%v, want 3 rows and capped", len(rows), capped)
		}
	})
	t.Run("exactly the ceiling", func(t *testing.T) {
		f := terminationFake(7, 7, 7)
		p := newTestProvider(t, f)
		rows, capped, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"7"}, 0, 3, nil, &readStats{})
		if err != nil {
			t.Fatal(err)
		}
		if capped || len(rows) != 3 {
			t.Errorf("rows=%d capped=%v, want 3 rows, not capped", len(rows), capped)
		}
	})
	t.Run("values that pass it only together", func(t *testing.T) {
		f := terminationFake(7, 7, 8, 8)
		p := newTestProvider(t, f)
		rows, capped, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"7", "8"}, 0, 3, nil, &readStats{})
		if err != nil {
			t.Fatal(err)
		}
		if capped {
			t.Error("capped, though no value has more rows than the ceiling")
		}
		if got := rowIDs(t, rows); !slices.Equal(got, []int{1, 2, 3, 4}) {
			t.Errorf("rows %v, want all four in order", got)
		}
	})
}

// A row two batches both return — the replica moved it between them — is one
// object, kept once, as it first arrived.
func TestListBatched_DedupesAcrossBatches(t *testing.T) {
	var mu sync.Mutex
	srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var results []map[string]interface{}
		switch r.URL.Query().Get("filter[id]__in") {
		case "1":
			results = []map[string]interface{}{{"id": 1, "name": "first"}, {"id": 2, "name": "moved"}}
		case "2":
			results = []map[string]interface{}{{"id": 2, "name": "moved again"}, {"id": 3, "name": "third"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": len(results), "results": results})
	}))
	t.Cleanup(srv.Close)
	p := New(srv.URL, "t", "nb", srv.Client())

	rows, _, err := p.listBatched(context.Background(), "dcim/devices", param("id", "in"), []string{"1", "2"}, 1, MaxLimit, nil, &readStats{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowIDs(t, rows); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("rows %v, want 1, 2, 3", got)
	}
	if !strings.Contains(string(rows[1]), `"moved"`) {
		t.Errorf("kept %s, want the row as it first arrived", rows[1])
	}
}

// A table with no data on the tenant is no rows, not a failure: an empty
// NetBox table is never fed, so a tenant without wireless links or patch
// panels has those tables unfed, and a topology over it is complete. Both
// ways of learning it: the catalogue (nothing is sent), and the route's own
// 404 when the catalogue is stale.
func TestListBatched_UnfedIsEmpty(t *testing.T) {
	t.Run("the catalogue says so", func(t *testing.T) {
		f := terminationFake(1)
		f.setIngested(terminations, false)
		p := newTestProvider(t, f)
		st := readStats{}
		rows, capped, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"1"}, 0, MaxLimit, nil, &st)
		if err != nil || capped || len(rows) != 0 {
			t.Errorf("rows=%d capped=%v err=%v, want none and no error", len(rows), capped, err)
		}
		if n := f.countRequestsFor(terminations); n != 0 {
			t.Errorf("%d requests to a table the catalogue has as unfed", n)
		}
		if st.unknown || st.asOf != nil {
			t.Errorf("a table not read says nothing about the result's age: %+v", st)
		}
	})
	t.Run("the route says so", func(t *testing.T) {
		f := terminationFake(1)
		p := newTestProvider(t, f)
		if _, err := p.catalogue(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		f.setIngested(terminations, false) // after the catalogue was cached
		rows, capped, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"1"}, 0, MaxLimit, nil, &readStats{})
		if err != nil || capped || len(rows) != 0 {
			t.Errorf("rows=%d capped=%v err=%v, want none and no error", len(rows), capped, err)
		}
		if n := f.countRequestsFor(terminations); n != 1 {
			t.Errorf("%d requests, want the one the route answered 404", n)
		}
	})
}

// Any other failure fails the read, on any batch: a read missing a batch is
// a read missing rows, with nothing to show for it.
func TestListBatched_FailsOnAnyBatch(t *testing.T) {
	f := terminationFake(1, 2)
	p := newTestProvider(t, f)
	if _, err := p.catalogue(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status, f.errBody = 503, "unavailable"
	f.mu.Unlock()
	_, _, err := p.listBatched(context.Background(), terminations, param("_device_id", "in"), []string{"1", "2"}, 1, MaxLimit, nil, &readStats{})
	if u := provider.Classify(err); u == nil || u.Status != 503 {
		t.Errorf("err = %v, want the replica's 503", err)
	}
}

// listByIDs reads every row a set of ids names, by any column.
func TestListByIDs(t *testing.T) {
	f := terminationFake(4, 5, 6)
	p := newTestProvider(t, f)
	rows, capped, err := p.listByIDs(context.Background(), terminations, "cable_id", []string{"3", "1"}, url.Values{"fields": {"id,cable_id"}}, &readStats{})
	if err != nil || capped {
		t.Fatalf("capped=%v err=%v", capped, err)
	}
	if got := rowIDs(t, rows); !slices.Equal(got, []int{1, 3}) {
		t.Errorf("rows %v", got)
	}
	if r, ok := f.requestWith(terminations, "filter[cable_id]__in"); !ok || r.query.Get("filter[cable_id]__in") != "3,1" {
		t.Errorf("request %v", r.query)
	}
}

// A result read from several tables is as old as its oldest part, and of
// unknown age when any part's age is unknown.
func TestReadStats_KeepsTheOldestInstant(t *testing.T) {
	t1 := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	t0 := t1.Add(-time.Hour)
	var st readStats
	st.note(&t1)
	st.note(&t0)
	st.note(&t1)
	if st.asOf == nil || !st.asOf.Equal(t0) || st.unknown {
		t.Errorf("%+v, want %v", st, t0)
	}
	if got := st.dataAsOf(); got == nil || !got.Equal(t0) {
		t.Errorf("dataAsOf = %v", got)
	}
	st.note(nil)
	if !st.unknown || st.dataAsOf() != nil {
		t.Errorf("%+v: one read of unknown age makes the whole of unknown age", st)
	}
}

// rowID accepts exactly what toInt accepts of a decoded id: a positive whole
// number below 2^53. Anything else — a string, null, a boolean, a fraction, a
// missing key — is a row without a usable id, never a guessed one.
func TestRowID_AcceptsOnlyAPositiveWholeNumber(t *testing.T) {
	for _, tc := range []struct {
		row  string
		want int
		err  error
	}{
		{`{"id": 7}`, 7, nil},
		{`{"id": 7.0}`, 7, nil},
		{`{"id": 9007199254740991}`, 9007199254740991, nil},
		{`{"id": 9007199254740992}`, 0, errRowWithoutID},
		{`{"id": 0}`, 0, errRowWithoutID},
		{`{"id": -3}`, 0, errRowWithoutID},
		{`{"id": 7.5}`, 0, errRowWithoutID},
		{`{"id": "7"}`, 0, errRowWithoutID},
		{`{"id": null}`, 0, errRowWithoutID},
		{`{"id": true}`, 0, errRowWithoutID},
		{`{"name": "x"}`, 0, errRowWithoutID},
		{`null`, 0, errMalformedRow},
	} {
		got, err := rowID(json.RawMessage(tc.row), "id")
		if got != tc.want || err != tc.err {
			t.Errorf("rowID(%s) = %d, %v; want %d, %v", tc.row, got, err, tc.want, tc.err)
		}
	}
}
