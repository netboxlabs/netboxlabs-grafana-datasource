package replicacache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
