package plugin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// TestRequestConsumer pins how a QueryData call is classified. The expression
// header is the opposite case from FromAlert (see TestIsAlertRequest): Grafana
// forwards it with the "http_" prefix, so the SDK accessor is the right read and
// a direct map lookup of the bare name would find nothing.
func TestRequestConsumer(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    consumer
	}{
		{"no headers", nil, consumerDashboard},
		{"unrelated header", map[string]string{"http_X-Rule-Uid": "abc"}, consumerDashboard},
		{"expression, as Grafana forwards it", map[string]string{"http_X-Grafana-From-Expr": "true"}, consumerExpression},
		{"expression, other casing", map[string]string{"http_x-grafana-from-expr": "True"}, consumerExpression},
		{"expression explicitly false", map[string]string{"http_X-Grafana-From-Expr": "false"}, consumerDashboard},
		{"bare name is not what Grafana sends", map[string]string{"X-Grafana-From-Expr": "true"}, consumerDashboard},
		{"alert", map[string]string{"FromAlert": "true"}, consumerAlert},
		// A rule with a SQL expression carries both. Alert is the stricter reading
		// (it also drops the sort), so it wins.
		{"alert rule with an expression", map[string]string{"FromAlert": "true", "http_X-Grafana-From-Expr": "true"}, consumerAlert},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestConsumer(&backend.QueryDataRequest{Headers: tc.headers}); got != tc.want {
				t.Errorf("requestConsumer(%v) = %v, want %v", tc.headers, got, tc.want)
			}
		})
	}
	if requestConsumer(nil) != consumerDashboard {
		t.Error("a nil request must be treated as a plain dashboard query")
	}
}

// A query whose frame feeds an expression must refuse a partial result in every
// row-returning branch. Expressions drop the input frame's meta.notices exactly
// as alert evaluation does, so the truncation notice a dashboard relies on never
// reaches the panel: measured live, a SQL join over dcim/devices at limit 5 of
// 15 reported "AMS1: 5" with no notice anywhere.
func TestQuery_ExpressionConsumerRefusesPartialResults(t *testing.T) {
	rows := make([]map[string]interface{}, 5)
	for i := range rows {
		rows[i] = map[string]interface{}{"name": fmt.Sprintf("dev-%d", i), "address": fmt.Sprintf("10.0.0.%d", i)}
	}
	truncated := &provider.Result{Columns: []string{"name"}, Rows: rows, Total: 15, MaxRows: 10000}
	degraded := &provider.Result{Columns: []string{"name"}, Rows: rows, Total: 5, Warnings: []string{"2 lookups failed."}}
	cappedRes := &provider.Result{Columns: []string{"name", "utilization"}, Rows: rows, Total: 5,
		Capped: &provider.Cap{Columns: []string{"utilization"}, Measured: 3, Rows: 5}}
	truncatedIPs := &provider.Result{Columns: []string{"address"}, Rows: rows[:2], Total: 5}

	truncatedGraph := fabric()
	truncatedGraph.Total, truncatedGraph.Fetched, truncatedGraph.MaxRows = 40, 4, 10000
	degradedGraph := fabric()
	degradedGraph.Warnings = []string{"Links for 1 device could not be read."}

	cases := []struct {
		name     string
		provider *fakeProvider
		json     string
		want     string // substring naming the gap, or the whole sentence where the wording is new
	}{
		{"objects, truncated", &fakeProvider{result: truncated},
			`{"queryType":"objects","objectType":"dcim/devices","limit":5}`, "5 of 15"},
		{"objects, degraded", &fakeProvider{result: degraded},
			`{"queryType":"objects","objectType":"dcim/devices","limit":100}`,
			"Query feeding an expression returned a degraded result, so the expression would compute on data that is missing for a reason the numbers cannot show. 2 lookups failed."},
		{"objects, capped", &fakeProvider{result: cappedRes},
			`{"queryType":"objects","objectType":"dcim/devices","limit":100}`,
			"Query feeding an expression measured utilization for 3 of its 5 rows, so the other 2 would reach the expression blank instead of with the values they hold. Lower the row limit to 3 or fewer"},
		{"ip-enrichment, truncated", &fakeProvider{ipResult: truncatedIPs},
			`{"queryType":"ip-enrichment","ips":"10.0.0.1 10.0.0.2 10.0.0.3 10.0.0.4 10.0.0.5","limit":2}`, "2 of 5"},
		{"topology-edges, truncated", &fakeProvider{graph: truncatedGraph},
			`{"queryType":"topology-edges","limit":4}`, "4 of 40"},
		{"topology-edges, degraded", &fakeProvider{graph: degradedGraph},
			`{"queryType":"topology-edges","limit":100}`,
			"Query feeding an expression returned an incomplete set of links, so the expression would see a device as less connected than it is. Links for 1 device could not be read."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDatasource(tc.provider)
			q := backend.DataQuery{RefID: "A", JSON: []byte(tc.json)}

			resp := d.query(context.Background(), q, consumerExpression)
			if resp.Error == nil {
				t.Fatal("a query feeding an expression must fail on a partial result, not hand the expression a subset")
			}
			if resp.Status != backend.StatusBadRequest {
				t.Errorf("status = %v, want %v: the author must fix the query, not retry it", resp.Status, backend.StatusBadRequest)
			}
			msg := resp.Error.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error does not name the gap %q: %s", tc.want, msg)
			}
			// The reader is looking at a dashboard panel, not a rule. Telling them
			// their "alert query" failed sends them looking for a rule that does
			// not exist.
			if strings.Contains(strings.ToLower(msg), "alert") {
				t.Errorf("error talks about alerting to someone who has no alert rule: %s", msg)
			}
			if !strings.Contains(msg, "expression") {
				t.Errorf("error does not say why a dashboard query was refused: %s", msg)
			}

			// The plain dashboard path is unchanged: partial beats none.
			if resp := d.query(context.Background(), q, consumerDashboard); resp.Error != nil {
				t.Errorf("a plain dashboard query must still render the partial answer: %v", resp.Error)
			}
		})
	}
}

// The truncation guard compares rows against Total, so an expression-consumed
// query has to get a real count: with fast paging on, AllowUncounted would hand
// back Total 0 and the guard would wave every truncated result through. The sort,
// on the other hand, stays: unlike an alert instance set, the same frame may also
// be drawn in the panel next to the expression's output, where row order shows.
func TestQuery_ExpressionConsumerKeepsTheTotalAndTheSort(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}, Total: 1}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","ordering":"-name","limit":100}`),
	}, consumerExpression)
	if resp.Error != nil {
		t.Fatalf("complete result must pass: %v", resp.Error)
	}
	if fp.querySpec.AllowUncounted {
		t.Error("AllowUncounted was set for an expression-consumed query: the truncation guard would compare against a Total of 0")
	}
	if fp.querySpec.Ordering != "-name" {
		t.Errorf("Ordering = %q, want %q: the frame can still be shown in the panel", fp.querySpec.Ordering, "-name")
	}
}

// The alertTable shape is strict for everyone, and already was. What changes is
// only who the refusal is addressed to.
func TestQuery_AlertTable_ExpressionConsumerIsToldAboutTheExpression(t *testing.T) {
	rows := make([]map[string]interface{}, 5)
	for i := range rows {
		rows[i] = map[string]interface{}{"name": fmt.Sprintf("dev-%d", i)}
	}
	d := newTestDatasource(&fakeProvider{result: &provider.Result{Columns: []string{"name"}, Rows: rows, Total: 15}})
	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"limit":5}`)}

	resp := d.query(context.Background(), q, consumerExpression)
	if resp.Error == nil || !strings.Contains(resp.Error.Error(), "expression") {
		t.Errorf("want a refusal that names the expression, got %v", resp.Error)
	}
	// Anyone else asking for the alert shape is writing, or previewing, a rule.
	resp = d.query(context.Background(), q, consumerDashboard)
	if resp.Error == nil || !strings.Contains(resp.Error.Error(), "Alert query") {
		t.Errorf("want the alert wording outside an expression, got %v", resp.Error)
	}
}
