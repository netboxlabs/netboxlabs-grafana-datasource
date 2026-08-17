package plugin

import (
	"context"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The sort the user picked has to reach the provider, which is the only layer
// that can decide whether NetBox will accept it.
func TestQuery_OrderingReachesTheProvider(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   1,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","ordering":"-name","limit":100}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if fp.querySpec.Ordering != "-name" {
		t.Errorf("Ordering = %q, want %q", fp.querySpec.Ordering, "-name")
	}
}

// A saved query with no sort must produce exactly the request it produced before
// this field existed.
func TestQuery_NoOrderingAsksForNone(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}, Total: 1}}
	d := newTestDatasource(fp)
	if resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`),
	}, false); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if fp.querySpec.Ordering != "" {
		t.Errorf("Ordering = %q on a query that asked for none", fp.querySpec.Ordering)
	}
}

// An alert evaluation never pays NetBox for a sort: alerting reads rows, not
// their order, and the one way order could change what a rule sees — truncation —
// is already a hard error on every alert path here.
func TestQuery_AlertEvaluationDoesNotPayForASort(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			// The shape the QueryEditor produces by default: neither switch on.
			name: "plain objects query",
			json: `{"queryType":"objects","objectType":"dcim/devices","ordering":"status","limit":100}`,
		},
		{
			name: "alert table",
			json: `{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"ordering":"status","limit":100}`,
		},
		{
			name: "count",
			json: `{"queryType":"objects","objectType":"dcim/devices","count":true,"ordering":"status"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{result: &provider.Result{
				Columns: []string{"name"},
				Rows:    []map[string]interface{}{{"name": "a"}},
				Total:   1,
			}}
			d := newTestDatasource(fp)
			resp := d.query(context.Background(), backend.DataQuery{RefID: "A", JSON: []byte(tc.json)}, true)
			if resp.Error != nil {
				t.Fatalf("unexpected error: %v", resp.Error)
			}
			if fp.querySpec.Ordering != "" {
				t.Errorf("Ordering = %q on an alert evaluation: the rule cannot see row order, and on a large instance the sort costs it seconds per evaluation",
					fp.querySpec.Ordering)
			}
		})
	}

	// The dashboard side of the same two shapes must keep the sort, or the rule
	// above has quietly turned sorting off for everyone.
	for _, tc := range []struct{ name, json string }{
		{"plain objects query", `{"queryType":"objects","objectType":"dcim/devices","ordering":"status","limit":100}`},
		{"alert table shown on a dashboard", `{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"ordering":"status","limit":100}`},
	} {
		t.Run("dashboard "+tc.name, func(t *testing.T) {
			fp := &fakeProvider{result: &provider.Result{
				Columns: []string{"name"},
				Rows:    []map[string]interface{}{{"name": "a"}},
				Total:   1,
			}}
			d := newTestDatasource(fp)
			if resp := d.query(context.Background(), backend.DataQuery{RefID: "A", JSON: []byte(tc.json)}, false); resp.Error != nil {
				t.Fatalf("unexpected error: %v", resp.Error)
			}
			if fp.querySpec.Ordering != "status" {
				t.Errorf("Ordering = %q, want %q on a dashboard query", fp.querySpec.Ordering, "status")
			}
		})
	}
}

// A count query has no rows to order, so the sort is dropped whoever is asking —
// not only for alerting.
func TestQuery_CountNeverCarriesASort(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{Total: 12}}
	d := newTestDatasource(fp)
	if resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","count":true,"ordering":"name"}`),
	}, false); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if fp.querySpec.Ordering != "" {
		t.Errorf("Ordering = %q on a count query: NetBox would sort rows nothing reads", fp.querySpec.Ordering)
	}
}

// The provider says, in a note, when it could not sort. That note has to arrive
// on the frame as an INFO notice — the same channel every other "this is not
// quite the answer you pictured" sentence uses.
func TestQuery_OrderingNoteReachesTheFrame(t *testing.T) {
	const note = "Not sorted by \"device_count\" — NetBox does not sort dcim/sites on that field."
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   1,
		Notes:   []string{note},
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/sites","ordering":"device_count","limit":100}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(resp.Frames))
	}
	var found bool
	for _, n := range resp.Frames[0].Meta.Notices {
		if n.Text == note {
			found = true
			if n.Severity != data.NoticeSeverityInfo {
				t.Errorf("severity = %v, want info: the rows are complete and correct, only unsorted", n.Severity)
			}
		}
	}
	if !found {
		t.Errorf("notices = %v, want the provider's note about the sort", resp.Frames[0].Meta.Notices)
	}
}
