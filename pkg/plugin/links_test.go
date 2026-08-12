package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

func TestRewriteLinkURL(t *testing.T) {
	cases := []struct {
		in, internal, public, want string
	}{
		// Prefix swap: value starts with the configured internal base.
		{"http://netbox:8080/dcim/sites/1/", "http://netbox:8080", "http://localhost:8000", "http://localhost:8000/dcim/sites/1/"},
		{"http://netbox:8080/api/dcim/devices/1/", "http://netbox:8080", "https://netbox.example.com", "https://netbox.example.com/api/dcim/devices/1/"},
		// Trailing slashes on either base are tolerated (callers trim, but be safe).
		{"http://netbox:8080/dcim/sites/1/", "http://netbox:8080/", "http://localhost:8000/", "http://localhost:8000/dcim/sites/1/"},
		// Public base with a path (NetBox behind a reverse-proxy subpath).
		{"http://netbox:8080/dcim/devices/1/", "http://netbox:8080", "https://tools.example.com/netbox", "https://tools.example.com/netbox/dcim/devices/1/"},
		// NOT under the internal base -> untouched. This is load-bearing:
		// cf_*_url custom-field columns routinely hold third-party URLs
		// (runbooks, vendor portals) that must never be pointed at NetBox.
		{"https://wiki.example.com/runbooks/42", "http://netbox:8080", "http://localhost:8000", "https://wiki.example.com/runbooks/42"},
		// Prefix must match on a path boundary: netbox:8080 vs netbox:80801.
		{"http://netbox:80801/dcim/devices/1/", "http://netbox:8080", "http://localhost:8000", "http://netbox:80801/dcim/devices/1/"},
		// Relative and non-URL values pass through unchanged.
		{"/dcim/devices/1/", "http://netbox:8080", "http://localhost:8000", "/dcim/devices/1/"},
		{"not a url", "http://netbox:8080", "http://localhost:8000", "not a url"},
	}
	for _, c := range cases {
		if got := rewriteLinkURL(c.in, trimBase(c.internal), trimBase(c.public)); got != c.want {
			t.Errorf("rewriteLinkURL(%q, %q, %q) = %q, want %q", c.in, c.internal, c.public, got, c.want)
		}
	}
}

func TestRewriteLinks(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"name", "display_url", "url", "site"},
		Rows: []map[string]interface{}{
			{"name": "leaf1", "display_url": "http://netbox:8080/dcim/devices/1/", "url": "http://netbox:8080/api/dcim/devices/1/", "site": "dc1"},
			{"name": "leaf2", "display_url": nil, "site": "dc1"},
		},
	}
	rewriteLinks(res, "http://netbox:8080", "http://localhost:8000")
	if got := res.Rows[0]["display_url"]; got != "http://localhost:8000/dcim/devices/1/" {
		t.Errorf("display_url = %v", got)
	}
	if got := res.Rows[0]["url"]; got != "http://localhost:8000/api/dcim/devices/1/" {
		t.Errorf("url = %v", got)
	}
	if got := res.Rows[0]["site"]; got != "dc1" {
		t.Errorf("non-URL column touched: site = %v", got)
	}
	if got := res.Rows[1]["display_url"]; got != nil {
		t.Errorf("nil value touched: %v", got)
	}
}

// TestQueryData_PublicURLRewritesLinks guards the wiring: the rewrite must run
// on the data-query path, not just exist as a helper (fakeProvider.BaseURL()
// is "http://nb").
func TestQueryData_PublicURLRewritesLinks(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		result: &provider.Result{
			Columns: []string{"name", "display_url"},
			Rows: []map[string]interface{}{
				{"name": "leaf1", "display_url": "http://nb/dcim/devices/1/"},
			},
		},
	})
	d.cfg.PublicURL = "http://localhost:8000"
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"objectType":"dcim/devices"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	frame := resp.Responses["A"].Frames[0]
	for _, f := range frame.Fields {
		if f.Name != "display_url" {
			continue
		}
		if got, ok := f.At(0).(string); !ok || got != "http://localhost:8000/dcim/devices/1/" {
			t.Fatalf("display_url = %v, want rewritten localhost URL", f.At(0))
		}
		return
	}
	t.Fatal("display_url field missing")
}

// TestQueryData_IPEnrichment_PublicURLRewritesLinks guards the third wiring
// site: prefix objects carry url/display_url, reachable via custom context
// fields.
func TestQueryData_IPEnrichment_PublicURLRewritesLinks(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		ipResult: &provider.Result{
			Columns: []string{"ip", "display_url"},
			Rows: []map[string]interface{}{
				{"ip": "10.0.0.5", "display_url": "http://nb/ipam/prefixes/1/"},
			},
		},
	})
	d.cfg.PublicURL = "http://localhost:8000"
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"queryType":"ip-enrichment","ips":"10.0.0.5"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	frame := resp.Responses["A"].Frames[0]
	for _, f := range frame.Fields {
		if f.Name != "display_url" {
			continue
		}
		if got, ok := f.At(0).(string); !ok || got != "http://localhost:8000/ipam/prefixes/1/" {
			t.Fatalf("display_url = %v, want rewritten localhost URL", f.At(0))
		}
		return
	}
	t.Fatal("display_url field missing")
}

// TestHandleQuery_PublicURLRewritesLinks guards the /query resource endpoint
// (variable queries) the same way.
func TestHandleQuery_PublicURLRewritesLinks(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		result: &provider.Result{
			Columns: []string{"name", "display_url"},
			Rows: []map[string]interface{}{
				{"name": "leaf1", "display_url": "http://nb/dcim/devices/1/"},
			},
		},
	})
	d.cfg.PublicURL = "http://localhost:8000"
	r := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{"objectType":"dcim/devices"}`))
	w := httptest.NewRecorder()
	d.handleQuery(w, r)
	var res provider.Result
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v (status %d)", err, w.Code)
	}
	if got := res.Rows[0]["display_url"]; got != "http://localhost:8000/dcim/devices/1/" {
		t.Fatalf("display_url = %v, want rewritten localhost URL", got)
	}
}

func TestRewriteLinks_NoPublicURLIsNoOp(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"display_url"},
		Rows:    []map[string]interface{}{{"display_url": "http://netbox:8080/dcim/devices/1/"}},
	}
	rewriteLinks(res, "http://netbox:8080", "")
	if got := res.Rows[0]["display_url"]; got != "http://netbox:8080/dcim/devices/1/" {
		t.Errorf("no-op violated: %v", got)
	}
	rewriteLinks(nil, "http://netbox:8080", "http://localhost:8000") // must not panic
}

func TestRewriteGraphLinks(t *testing.T) {
	g := &provider.Graph{Nodes: []provider.GraphNode{
		{ID: "1", URL: "http://netbox:8080/dcim/devices/1/"},
		{ID: "2", URL: ""},
	}}
	rewriteGraphLinks(g, "http://netbox:8080", "http://localhost:8000")
	if g.Nodes[0].URL != "http://localhost:8000/dcim/devices/1/" {
		t.Errorf("node0 URL = %q, want the public base", g.Nodes[0].URL)
	}
	if g.Nodes[1].URL != "" {
		t.Errorf("empty node URL must stay empty, got %q", g.Nodes[1].URL)
	}

	// No publicBase -> no-op.
	g2 := &provider.Graph{Nodes: []provider.GraphNode{{URL: "http://netbox:8080/x/"}}}
	rewriteGraphLinks(g2, "http://netbox:8080", "")
	if g2.Nodes[0].URL != "http://netbox:8080/x/" {
		t.Errorf("empty publicBase must be a no-op, got %q", g2.Nodes[0].URL)
	}
}

func TestQueryData_Topology_PublicURLRewritesNodeLinks(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		graph: &provider.Graph{
			Nodes: []provider.GraphNode{{ID: "1", Title: "leaf1", URL: "http://nb/dcim/devices/1/"}},
		},
	})
	d.cfg.PublicURL = "http://localhost:8000"
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"queryType":"topology"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	nodes := resp.Responses["A"].Frames[0]
	for _, f := range nodes.Fields {
		if f.Name != "url" {
			continue
		}
		if got, _ := f.At(0).(string); got != "http://localhost:8000/dcim/devices/1/" {
			t.Fatalf("node url = %v, want rewritten localhost URL", f.At(0))
		}
		return
	}
	t.Fatal("nodes frame missing url field")
}
