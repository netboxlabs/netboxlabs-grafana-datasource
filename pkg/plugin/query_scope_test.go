package plugin

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

func scopeRows() *provider.Result {
	return &provider.Result{
		Columns: []string{"ip", "device_name"},
		Rows: []map[string]interface{}{
			{"ip": "10.0.0.1", "device_name": "leaf1"},
			{"ip": "10.0.0.2", "device_name": nil},
		},
		Total: 2, MaxRows: 10000,
	}
}

// ipSource decides which provider call runs. "scope" lists NetBox under the
// query's filter rows; anything else is the IP list, as before.
func TestQuery_IPEnrichment_ScopeSourceDispatchesToResolveScope(t *testing.T) {
	fp := &fakeProvider{scopeResult: scopeRows()}
	resp := newTestDatasource(fp).query(context.Background(), backend.DataQuery{RefID: "A", JSON: []byte(`{
		"queryType":"ip-enrichment","ipSource":"scope",
		"filters":[{"field":"parent","operator":"","value":"10.0.0.0/24"},{"field":"site","operator":"","value":"$__all"}],
		"contextFields":["device_name"],"limit":500}`)}, consumerDashboard)
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if fp.scopeCalls != 1 || fp.ipCalls != 0 {
		t.Fatalf("ResolveScope ran %d times, ResolveIPs %d; want 1 and 0", fp.scopeCalls, fp.ipCalls)
	}
	// The $__all row was dropped before dispatch, like every other filter row.
	if want := []provider.Filter{{Field: "parent", Value: "10.0.0.0/24"}}; !reflect.DeepEqual(fp.scopeFilters, want) {
		t.Errorf("filters = %v, want %v", fp.scopeFilters, want)
	}
	if fp.scopeLimit != 500 {
		t.Errorf("limit = %d, want 500", fp.scopeLimit)
	}
	// Same frame shape as the list path: rows keyed by ip, notices intact.
	if resp.Frames[0].Rows() != 2 || resp.Frames[0].Meta == nil {
		t.Errorf("frame = %d rows, meta %v", resp.Frames[0].Rows(), resp.Frames[0].Meta)
	}
}

func TestQuery_IPEnrichment_ScopeRunsWithoutIPs(t *testing.T) {
	// The list path returns an empty response for an empty IP list. A scope
	// query has no IPs by construction and must not be short-circuited by that.
	fp := &fakeProvider{scopeResult: scopeRows()}
	resp := newTestDatasource(fp).query(context.Background(), backend.DataQuery{RefID: "A",
		JSON: []byte(`{"queryType":"ip-enrichment","ipSource":"scope","ips":"","filters":[{"field":"parent","operator":"","value":"10.0.0.0/24"}]}`)}, consumerDashboard)
	if resp.Error != nil || len(resp.Frames) != 1 {
		t.Fatalf("scope query with no ips: frames=%d err=%v", len(resp.Frames), resp.Error)
	}
}

// A scope with no effective filter is the whole ipam/ip-addresses table, up to
// the row cap, on every evaluation — never what a rule meant. The editor holds
// such a query back; a provisioned rule or an API caller has no editor, so the
// backend refuses it too, naming the fix. A filter row the provider would drop
// (blank value, or $__all) counts as no filter.
func TestQuery_IPEnrichment_ScopeWithoutFiltersIsRefused(t *testing.T) {
	for _, filters := range []string{
		``,
		`"filters":[],`,
		`"filters":[{"field":"parent","operator":"","value":""}],`,
		`"filters":[{"field":"site","operator":"","value":"$__all"}],`,
	} {
		fp := &fakeProvider{scopeResult: scopeRows()}
		resp := newTestDatasource(fp).query(context.Background(), backend.DataQuery{RefID: "A",
			JSON: []byte(`{"queryType":"ip-enrichment","ipSource":"scope",` + filters + `"limit":100}`)}, consumerDashboard)
		if resp.Error == nil || resp.Status != backend.StatusBadRequest {
			t.Errorf("filters %s: want a bad-request refusal, got status=%v err=%v", filters, resp.Status, resp.Error)
		}
		if resp.Error != nil && !strings.Contains(resp.Error.Error(), "filter") {
			t.Errorf("filters %s: refusal does not name the fix: %v", filters, resp.Error)
		}
		if fp.scopeCalls != 0 {
			t.Errorf("filters %s: NetBox was asked for the whole address table", filters)
		}
	}
	// An empty-family operator is a real filter with no value.
	fp := &fakeProvider{scopeResult: scopeRows()}
	resp := newTestDatasource(fp).query(context.Background(), backend.DataQuery{RefID: "A",
		JSON: []byte(`{"queryType":"ip-enrichment","ipSource":"scope","filters":[{"field":"tenant","operator":"nempty","value":""}]}`)}, consumerDashboard)
	if resp.Error != nil || fp.scopeCalls != 1 {
		t.Errorf("tenant has-any-value: err=%v calls=%d", resp.Error, fp.scopeCalls)
	}
}

func TestQuery_IPEnrichment_ListSourceIsTheDefault(t *testing.T) {
	fp := &fakeProvider{ipResult: scopeRows()}
	for _, src := range []string{`"ipSource":"list",`, `"ipSource":"",`, ``} {
		resp := newTestDatasource(fp).query(context.Background(), backend.DataQuery{RefID: "A",
			JSON: []byte(`{"queryType":"ip-enrichment",` + src + `"ips":"10.0.0.1"}`)}, consumerDashboard)
		if resp.Error != nil {
			t.Fatal(resp.Error)
		}
	}
	if fp.ipCalls != 3 || fp.scopeCalls != 0 {
		t.Errorf("ResolveIPs ran %d times, ResolveScope %d; want 3 and 0", fp.ipCalls, fp.scopeCalls)
	}
}

// The scope path is the alert-rule path, so this is the case that matters most:
// a scope that overflows its limit must fail a strict consumer, and stay a
// partial table with a notice for a dashboard.
func TestQuery_IPEnrichment_TruncatedScopeRefusesStrictConsumers(t *testing.T) {
	cut := scopeRows()
	cut.Total = 5000
	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"ip-enrichment","ipSource":"scope","limit":2,"filters":[{"field":"parent","operator":"","value":"10.0.0.0/8"}]}`)}

	for _, c := range []consumer{consumerAlert, consumerExpression} {
		resp := newTestDatasource(&fakeProvider{scopeResult: cut}).query(context.Background(), q, c)
		if resp.Error == nil {
			t.Fatalf("consumer %v: a cut-off scope must be refused", c)
		}
		// The noun is the scope's, not the list path's: nothing was "requested".
		if !strings.Contains(resp.Error.Error(), "2 of 5,000 addresses in the scope") {
			t.Errorf("consumer %v: error does not name the gap in the scope's terms: %v", c, resp.Error)
		}
	}
	resp := newTestDatasource(&fakeProvider{scopeResult: cut}).query(context.Background(), q, consumerDashboard)
	if resp.Error != nil || resp.Frames[0].Rows() != 2 {
		t.Fatalf("dashboard must keep the partial table: err=%v", resp.Error)
	}
	var noticed bool
	for _, n := range resp.Frames[0].Meta.Notices {
		noticed = noticed || strings.Contains(n.Text, "5,000 addresses in the scope")
	}
	if !noticed {
		t.Error("dashboard lost the truncation notice")
	}
}

func TestQuery_IPEnrichment_ScopeJoinKeysWork(t *testing.T) {
	// A join key on a scope result derives from the same columns as on a list
	// result; the source fetch/drop bookkeeping (ipEnrichFields) is shared.
	fp := &fakeProvider{scopeResult: &provider.Result{Columns: []string{"ip", "device_name"},
		Rows: []map[string]interface{}{{"ip": "10.0.0.1", "device_name": "LEAF1.example.net"}}, Total: 1}}
	resp := newTestDatasource(fp).query(context.Background(), backend.DataQuery{RefID: "A", JSON: []byte(`{
		"queryType":"ip-enrichment","ipSource":"scope","contextFields":["ip"],
		"filters":[{"field":"parent","operator":"","value":"10.0.0.0/24"}],
		"joinKeys":[{"source":"device_name","output":"instance","transform":"lower"}]}`)}, consumerDashboard)
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	if got := frameValue(t, resp.Frames[0], "instance"); got != "leaf1.example.net" {
		t.Errorf("instance = %v", got)
	}
	if !reflect.DeepEqual(fp.scopeFields, []string{"ip", "device_name"}) {
		t.Errorf("provider was asked for %v; the join-key source must be fetched", fp.scopeFields)
	}
}
