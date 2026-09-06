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
