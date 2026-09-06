package replicacache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// An envelope with no "count" is valid JSON in the wrong shape — a proxy's
// error page, or a different service behind the URL. Decoded into a plain int
// it reads as zero, which reports zero for count queries and, because the
// truncation notice is suppressed when Total is zero, presents a truncated
// table as though it were the whole population.
func TestEnvelopeWithoutCountIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results": [{"id": 1, "name": "CORE-1"}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("an envelope with no count must be refused, not read as zero rows")
	}
	if !errors.Is(err, errMalformedEnvelope) {
		t.Errorf("want errMalformedEnvelope, got %v", err)
	}
	u := provider.Classify(err)
	if u == nil || !strings.Contains(u.Detail, "unexpected shape") {
		t.Errorf("guidance should point at the URL, got %+v", u)
	}
}

// A count of zero is a real answer and must still be accepted: the guard is on
// the field's presence, not its value.
func TestZeroCountIsAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 0, "results": []}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("zero rows is a valid result: %v", err)
	}
	if res.Total != 0 || len(res.Rows) != 0 {
		t.Errorf("want an empty result, got total=%d rows=%d", res.Total, len(res.Rows))
	}
}

// An unbounded read lets one upstream response exhaust the backend. The cap
// rejects rather than truncates, so an oversized body cannot arrive as a
// confusing parse error either.
func TestOversizedBodyIsRejected(t *testing.T) {
	prev := maxBodyBytes
	maxBodyBytes = 1024
	defer func() { maxBodyBytes = prev }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 1, "results": [{"pad": "`))
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
		_, _ = w.Write([]byte(`"}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("want an oversized body to be refused")
	}
	if !errors.Is(err, errOversizedBody) {
		t.Errorf("want errOversizedBody, got %v", err)
	}
	if u := provider.Classify(err); u == nil || !strings.Contains(u.Detail, "64 MiB") {
		t.Errorf("guidance should name the limit and a remedy, got %+v", u)
	}
}

// The other half of the envelope. An intermediary answering {"count":0} with
// no results decodes to a nil slice, which is indistinguishable from a real
// empty answer — including to an alert rule, which would evaluate "no matches"
// as a fact rather than as a failure to read.
func TestEnvelopeWithoutResultsIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 0}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("an envelope with no results must be refused, not read as zero matches")
	}
	if !errors.Is(err, errMalformedEnvelope) {
		t.Errorf("want errMalformedEnvelope, got %v", err)
	}
}

// And an explicitly empty results array is a real answer, so the guard stays on
// presence rather than becoming a rejection of empty tables.
func TestEmptyResultsArrayIsAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 0, "results": []}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("an empty table is a valid answer: %v", err)
	}
	if len(res.Rows) != 0 || res.Total != 0 {
		t.Errorf("want an empty result, got total=%d rows=%d", res.Total, len(res.Rows))
	}
}

// A JSON null in results decodes into the map without error and leaves it nil.
// Appending it produces an EMPTY row that the count still includes, and an
// alert-table query turns an empty row into a value of 1 — a spurious alert
// built out of a malformed response.
func TestNullResultRowIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 1, "results": [null]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("a null row must be refused, not turned into an empty row")
	}
	if !errors.Is(err, errMalformedRow) {
		t.Errorf("want errMalformedRow, got %v", err)
	}
	if u := provider.Classify(err); u == nil || !strings.Contains(u.Detail, "Replica cache") {
		t.Errorf("guidance should name the cache, got %+v", u)
	}
}

// A row that is a scalar rather than an object is the same protocol failure.
func TestScalarResultRowIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 1, "results": ["CORE-1"]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"}); err == nil {
		t.Fatal("a scalar row must be refused")
	}
}

// Every status has to name the right service. An unenumerated one kept an empty
// Detail, and both renderers then fell through to "NetBox returned HTTP 429" —
// a connection this mode may not even have configured.
func TestEveryStatusNamesTheCache(t *testing.T) {
	for _, status := range []int{405, 413, 429, 418} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error": "nope"}`))
		}))

		p := New(srv.URL, "t", "nb", srv.Client())
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d must be an error", status)
		}
		u := provider.Classify(err)
		if u == nil || !strings.Contains(u.Detail, "Replica cache") {
			t.Errorf("HTTP %d guidance should name the cache, got %+v", status, u)
		}
	}
}

// Presence is not enough: the two envelope members have to agree. A page
// holding more rows than the total it reports would be read by a count query as
// no matches, while the truncation guard treats Total==0 as "unavailable"
// rather than as a reason to refuse — so an alert could evaluate a response
// that contradicts itself.
func TestPageContradictingItsOwnCountIsRejected(t *testing.T) {
	for _, body := range []string{
		`{"count": 0, "results": [{"id": 1, "name": "CORE-1"}]}`,
		`{"count": -5, "results": []}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))

		p := New(srv.URL, "t", "nb", srv.Client())
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
		srv.Close()
		if err == nil {
			t.Errorf("%s was accepted", body)
			continue
		}
		if !errors.Is(err, errInconsistentCount) {
			t.Errorf("%s: want errInconsistentCount, got %v", body, err)
		}
	}

	// A page smaller than the total is the ordinary case — that is what paging
	// IS — and must not be caught by this guard.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 500, "results": [{"id": 1, "name": "CORE-1"}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("a partial page is normal: %v", err)
	}
	if res.Total != 500 || len(res.Rows) != 1 {
		t.Errorf("total=%d rows=%d, want 500 and 1", res.Total, len(res.Rows))
	}
}

// Pages that individually agree with their own count can still contradict each
// other. Two one-row pages each reporting count 1 hand back two rows for a
// total of one, and the truncation guard only looks for the opposite
// inequality, so an alert would evaluate the extra row as authoritative.
func TestPagesThatContradictEachOtherAreRejected(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		body := `{"count": 1, "results": [{"id": 1, "name": "A"}], "next_cursor": "c2"}`
		if n > 1 {
			body = `{"count": 1, "results": [{"id": 2, "name": "B"}]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100})
	if err == nil {
		t.Fatal("two rows for a total of one must be refused")
	}
	if !errors.Is(err, errInconsistentCount) {
		t.Errorf("want errInconsistentCount, got %v", err)
	}
}

// The first page's total goes stale BY DESIGN: this mirrors a database being
// written to, cursor paging does not freeze a snapshot, and rows inserted
// mid-walk legitimately push the running count past a total that was correct
// when it was read. The later pages' own counts have grown to cover them, so
// this must NOT be refused — validating against the first total rather than the
// largest would turn ordinary concurrent writes into a query failure.
func TestGrowingTotalMidWalkIsNotAContradiction(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		body := `{"count": 1, "results": [{"id": 1, "name": "A"}], "next_cursor": "c2"}`
		if n > 1 {
			// Two rows were inserted while we walked, so the count has grown.
			body = `{"count": 3, "results": [{"id": 2, "name": "B"}, {"id": 3, "name": "C"}]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100})
	if err != nil {
		t.Fatalf("a growing count is a concurrent write, not a contradiction: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Errorf("rows = %d, want 3", len(res.Rows))
	}
	// Total is the LARGEST count any page reported, not the first. It is still
	// the service's own answer rather than len(Rows) — those two numbers answer
	// different questions — but a stale first page understates it, and Total is
	// what the truncation guard compares against: with a first page of 9,999,
	// later pages reporting 10,001 and a limit of 10,000, the stale value made
	// len(Rows) >= Total and an incomplete subset read as the whole population.
	if res.Total != 3 {
		t.Errorf("total = %d, want the largest count seen (3)", res.Total)
	}
}

// A cursor that does not advance would re-fetch the same page until the limit
// was reached, handing back one row duplicated and another never seen — and
// passing every count check on the way, since the duplicates are real rows and
// the totals agree.
func TestRepeatedCursorIsRejected(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		// Always the same row, always the same cursor.
		_, _ = w.Write([]byte(`{"count": 2, "results": [{"id": 1, "name": "A"}], "next_cursor": "stuck"}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100})
	if err == nil {
		t.Fatal("a repeated cursor must be refused, not walked in circles")
	}
	if !errors.Is(err, errCursorNotAdvancing) {
		t.Errorf("want errCursorNotAdvancing, got %v", err)
	}
	// And it stops promptly rather than spinning to the limit.
	if hits > 3 {
		t.Errorf("made %d requests; the repeat should be caught on the second", hits)
	}
}

// Distinct cursors are the ordinary walk and must not be caught by the guard.
func TestDistinctCursorsWalkNormally(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"count": 3, "results": [{"id": 1}], "next_cursor": "c2"}`))
		case 2:
			_, _ = w.Write([]byte(`{"count": 3, "results": [{"id": 2}], "next_cursor": "c3"}`))
		default:
			_, _ = w.Write([]byte(`{"count": 3, "results": [{"id": 3}]}`))
		}
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 100})
	if err != nil {
		t.Fatalf("an ordinary multi-page walk must succeed: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Errorf("rows = %d, want 3", len(res.Rows))
	}
}

// An outage while sampling the schema is an outage, not a bad filter. Folded
// into a nil map it became an UnsupportedFilterError and an HTTP 400 telling
// the reader their column's type could not be determined — sending them to edit
// a filter that was fine, over a credential that was not.
func TestSamplingFailureIsReportedAsItselfNotAsABadFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "invalid token"}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Filters:    []provider.Filter{{Field: "name", Operator: "ic", Value: "CORE"}},
	})
	if err == nil {
		t.Fatal("want an error")
	}
	u := provider.Classify(err)
	if u == nil || u.Kind != provider.ErrorKindAuth {
		t.Fatalf("a 401 must classify as auth, got %+v", u)
	}
	if strings.Contains(u.Detail, "type could not be determined") {
		t.Errorf("the filter is not the problem: %q", u.Detail)
	}
	if !strings.Contains(u.Detail, "credentials") {
		t.Errorf("guidance should name the credentials, got %q", u.Detail)
	}
}

// {"count":1,"results":[{}]} is object-shaped and non-nil, so the null/scalar
// guards let it through — one empty row with a matching total, which an
// alert-table query turns into a value of 1. Measured on a live instance: the
// service returns id on every projection, even one that did not ask for it, so
// requiring it costs nothing.
func TestRowWithoutAPrimaryKeyIsRejected(t *testing.T) {
	for _, body := range []string{
		`{"count": 1, "results": [{}]}`,
		`{"count": 1, "results": [{"name": "CORE-1"}]}`,
		`{"count": 1, "results": [{"id": "not-a-number", "name": "CORE-1"}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))

		p := New(srv.URL, "t", "nb", srv.Client())
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
		srv.Close()
		if err == nil {
			t.Errorf("%s was accepted", body)
			continue
		}
		if !errors.Is(err, errRowWithoutID) {
			t.Errorf("%s: want errRowWithoutID, got %v", body, err)
		}
	}
}

// The case the stale total hid: a count that grows PAST the limit. The walk
// stops at the limit with a complete-looking answer, and an alert evaluates a
// subset as the whole population.
func TestTotalCoversAPageCountThatGrewPastTheLimit(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		row := `{"id": ` + strconv.Itoa(n) + `}`
		if n == 1 {
			_, _ = w.Write([]byte(`{"count": 2, "results": [` + row + `], "next_cursor": "c2"}`))
			return
		}
		// More rows arrived while we walked; the service now knows about three.
		_, _ = w.Write([]byte(`{"count": 3, "results": [` + row + `], "next_cursor": "c3"}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want the limit of 2", len(res.Rows))
	}
	if res.Total != 3 {
		t.Errorf("total = %d, want 3 — the stale 2 would make this look complete", res.Total)
	}
	if len(res.Rows) >= res.Total {
		t.Error("this result IS truncated and the numbers must say so")
	}
}
