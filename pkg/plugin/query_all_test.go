package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// A filter row whose value is the All token means "do not filter on this field".
// It is how a dashboard author opts a variable's All out of being expanded into
// every option (Custom all value: $__all), and how a caller with no variables to
// expand says All in the first place.
func TestDropAllFilters(t *testing.T) {
	role := provider.Filter{Field: "role", Value: "leaf"}
	cases := []struct {
		name string
		in   []provider.Filter
		want []provider.Filter
	}{
		{"nil stays nil", nil, nil},
		{"nothing to drop", []provider.Filter{role}, []provider.Filter{role}},
		{"the sentinel drops its row", []provider.Filter{{Field: "site", Value: "$__all"}, role}, []provider.Filter{role}},
		{"padded", []provider.Filter{{Field: "site", Value: " $__all "}, role}, []provider.Filter{role}},
		// All is never one choice among several: Grafana deselects everything else
		// when it is picked. A list that contains the token is malformed input, and
		// widening a query on malformed input is the wrong way to fail — NetBox
		// gets it as written and matches nothing for that element.
		{"not as one element of a list", []provider.Filter{{Field: "site", Value: "ams1,$__all"}}, []provider.Filter{{Field: "site", Value: "ams1,$__all"}}},
		{"whatever the operator", []provider.Filter{{Field: "site", Operator: "n", Value: "$__all"}, role}, []provider.Filter{role}},
		// Only the exact token. A substring search for a name that happens to
		// contain it is someone's real filter.
		{"not a substring", []provider.Filter{{Field: "name", Operator: "ic", Value: "x$__all"}}, []provider.Filter{{Field: "name", Operator: "ic", Value: "x$__all"}}},
		{"every row dropped", []provider.Filter{{Field: "site", Value: "$__all"}}, []provider.Filter{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dropAllFilters(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dropAllFilters(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Every entry point that hands filters to the provider has to drop the row: one
// that forgets sends NetBox site=$__all, which matches nothing, and the panel
// reads as an empty inventory.
func TestAllFilterNeverReachesTheProvider(t *testing.T) {
	const filters = `"filters":[{"field":"site","operator":"","value":"$__all"},{"field":"role","operator":"","value":"leaf"}]`
	want := []provider.Filter{{Field: "role", Value: "leaf"}}
	res := &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "leaf1"}}, Total: 1}

	for _, tc := range []struct{ name, json string }{
		{"objects", `{"queryType":"objects","objectType":"dcim/devices",` + filters + `}`},
		{"count", `{"queryType":"objects","objectType":"dcim/devices","count":true,` + filters + `}`},
		{"alertTable", `{"queryType":"objects","objectType":"dcim/devices","alertTable":true,` + filters + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{result: res}
			resp := newTestDatasource(fp).query(context.Background(), backend.DataQuery{RefID: "A", JSON: []byte(tc.json)}, consumerDashboard)
			if resp.Error != nil {
				t.Fatal(resp.Error)
			}
			if !reflect.DeepEqual(fp.querySpec.Filters, want) {
				t.Errorf("provider got filters %v, want %v", fp.querySpec.Filters, want)
			}
		})
	}

	for _, qt := range []string{queryTypeTopology, queryTypeTopologyEdges} {
		t.Run(qt, func(t *testing.T) {
			fp := &fakeProvider{graph: fabric()}
			resp := newTestDatasource(fp).query(context.Background(),
				backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"` + qt + `",` + filters + `}`)}, consumerDashboard)
			if resp.Error != nil {
				t.Fatal(resp.Error)
			}
			if !reflect.DeepEqual(fp.topoSpec.Filters, want) {
				t.Errorf("provider got filters %v, want %v", fp.topoSpec.Filters, want)
			}
		})
	}

	// Variable queries and the editor preview go through the resource route, and
	// a chained variable ("devices in $site") is where All is most common.
	t.Run("query resource", func(t *testing.T) {
		fp := &fakeProvider{result: res}
		rec := httptest.NewRecorder()
		body := `{"objectType":"dcim/devices",` + filters + `}`
		newTestDatasource(fp).newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(fp.querySpec.Filters, want) {
			t.Errorf("provider got filters %v, want %v", fp.querySpec.Filters, want)
		}
	})
}

// The token has one meaning on the IP-enrichment ips list too. There is no
// filter to drop there — the list IS the input — so All resolves nothing, the
// same as an empty list, rather than asking NetBox for an address called $__all.
func TestAllTokenOnIPEnrichmentResolvesNothing(t *testing.T) {
	fp := &fakeProvider{ipResult: &provider.Result{Columns: []string{"address"}, Rows: []map[string]interface{}{{"address": "10.0.0.1"}}, Total: 1}}
	for _, ips := range []string{"$__all", " $__all ", "$__all,$__all"} {
		resp := newTestDatasource(fp).query(context.Background(),
			backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"ip-enrichment","ips":"` + ips + `"}`)}, consumerDashboard)
		if resp.Error != nil {
			t.Fatalf("ips %q: %v", ips, resp.Error)
		}
		if len(resp.Frames) != 0 {
			t.Errorf("ips %q: got %d frames, want none (the provider must not be asked for an address called $__all)", ips, len(resp.Frames))
		}
	}
	// A real address next to the token is still resolved, and only that one is asked for.
	resp := newTestDatasource(fp).query(context.Background(),
		backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"ip-enrichment","ips":"$__all 10.0.0.1"}`)}, consumerDashboard)
	if resp.Error != nil || len(resp.Frames) != 1 {
		t.Fatalf("mixed list: frames=%d err=%v", len(resp.Frames), resp.Error)
	}
	if !reflect.DeepEqual(fp.ipsSeen, []string{"10.0.0.1"}) {
		t.Errorf("provider was asked to resolve %v, want [10.0.0.1]", fp.ipsSeen)
	}
}
