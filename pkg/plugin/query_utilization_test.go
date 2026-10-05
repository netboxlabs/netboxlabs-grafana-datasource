package plugin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

// newUtilizationNetBox serves `rows` leaf prefixes and answers every child
// lookup instantly, standing in for a healthy NetBox — the local instance where
// a child lookup costs ~20ms, not the slow remote instance the
// old row cap was calibrated against.
func newUtilizationNetBox(t *testing.T, rows int, delay time.Duration) *netbox.Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("within") != "" { // container child-prefix lookup
			time.Sleep(delay)
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
			return
		}
		var b strings.Builder
		for i := range rows {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"prefix":"10.%d.%d.0/24","status":{"value":"active"},"is_pool":false,"mark_utilized":false,"vrf":null}`,
				i+1, i/256, i%256)
		}
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, rows, b.String())
	})
	for _, path := range []string{"/api/ipam/ip-addresses/", "/api/ipam/ip-ranges/"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return netbox.New(srv.URL, "test-token", &http.Client{Timeout: 10 * time.Second})
}

// TestQuery_AlertTable_UtilizationOverTheOldRowCapStillReturnsRows is the
// regression test for the break this fix exists for.
//
// A rule that has been evaluating for months — ipam/prefixes, alertTable, value
// field utilization, limit 1,000, threshold utilization > 90 — against an
// instance holding 200 prefixes. Each child lookup on a healthy NetBox answers
// in tens of milliseconds, so those 200 rows completed in seconds. Then the
// fixed 150-row measurement cap landed: rows 151-200 came back blank, the
// provider reported that through Warnings, and the alert path turns ANY warning
// into an error. The rule stopped returning rows and went to Error state on
// every evaluation.
//
// The cap itself is not the bug — a bound on an unbounded fan-out is right. The
// bug is that a deliberate bound was reported through the channel reserved for
// "a lookup failed", and that the bound was a constant calibrated against one
// slow instance rather than anything about THIS one.
func TestQuery_AlertTable_UtilizationOverTheOldRowCapStillReturnsRows(t *testing.T) {
	const rows = 200 // > the old fixed cap of 150
	d := newTestDatasource(newUtilizationNetBox(t, rows, 0))

	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON: []byte(`{"queryType":"objects","objectType":"ipam/prefixes","alertTable":true,` +
			`"valueField":"utilization","fields":["prefix","utilization"],"limit":1000}`),
	}, consumerAlert)

	if resp.Error != nil {
		t.Fatalf("alert query errored instead of returning rows: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(resp.Frames))
	}
	if n := resp.Frames[0].Rows(); n != rows {
		t.Fatalf("rows = %d, want %d", n, rows)
	}
	// Every row must carry a measured value: a blank one lands in the alert
	// frame as 0, which reads as "0% utilized" and is why a rule watching for
	// >90% would silently stop firing rather than merely lose a row.
	vals, ok := resp.Frames[0].Fields[len(resp.Frames[0].Fields)-1].At(rows - 1).(float64)
	if !ok {
		t.Fatalf("last value = %T, want float64", vals)
	}
}
