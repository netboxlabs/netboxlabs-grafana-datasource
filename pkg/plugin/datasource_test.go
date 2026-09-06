package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/models"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

// fakeProvider is a controllable provider.Provider for unit tests.
type fakeProvider struct {
	healthMsg         string
	healthErr         error
	types             []provider.ObjectType
	result            *provider.Result
	changes           []provider.Change
	ipResult          *provider.Result
	ipRow             map[string]interface{} // when set, ResolveIPs projects it onto the requested fields
	ipFields          []string               // captured by ResolveIPs for passthrough asserts
	ipsSeen           []string               // captured by ResolveIPs: the IPs it was asked to resolve
	ipCalls           int                    // how many times ResolveIPs ran
	scopeResult       *provider.Result       // returned by ResolveScope
	scopeFilters      []provider.Filter      // captured by ResolveScope
	scopeFields       []string               // captured by ResolveScope
	scopeLimit        int                    // captured by ResolveScope
	scopeCalls        int                    // how many times ResolveScope ran
	graph             *provider.Graph
	topoSpec          provider.TopologySpec // captured by Topology for passthrough asserts
	querySpec         provider.QuerySpec    // captured by Query for passthrough asserts
	branchSeen        string                // captured from the context by Query
	fieldsBranch      string                // captured from the context by Fields
	fieldValuesBranch string                // captured from the context by FieldValues
	queryErr          error

	filterFields       []provider.FilterField
	filterFieldsBranch string // captured from the context by FilterFields
	filterFieldsErr    error

	branchingInstalled bool // returned by BranchingInstalled
	branchingConcl     bool // conclusive flag returned by BranchingInstalled
}

func (f *fakeProvider) Name() string    { return "fake" }
func (f *fakeProvider) BaseURL() string { return "http://nb" }
func (f *fakeProvider) HealthCheck(context.Context) (string, error) {
	return f.healthMsg, f.healthErr
}
func (f *fakeProvider) ObjectTypes(context.Context) ([]provider.ObjectType, error) {
	return f.types, nil
}
func (f *fakeProvider) Fields(ctx context.Context, _ string) ([]provider.Field, error) {
	f.fieldsBranch = provider.BranchFromContext(ctx)
	return []provider.Field{{Name: "name", Type: provider.FieldTypeString}}, nil
}
func (f *fakeProvider) Query(ctx context.Context, spec provider.QuerySpec) (*provider.Result, error) {
	f.querySpec = spec
	f.branchSeen = provider.BranchFromContext(ctx)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.result, nil
}
func (f *fakeProvider) FieldValues(ctx context.Context, _, _, _ string, _ int) ([]string, error) {
	f.fieldValuesBranch = provider.BranchFromContext(ctx)
	return []string{"a", "b"}, nil
}
func (f *fakeProvider) Changes(context.Context, provider.ChangeSpec) ([]provider.Change, error) {
	return f.changes, nil
}

// ResolveIPs records the field list it was asked for and, when ipRow is set,
// PROJECTS that row onto it — the same narrowing the real implementation does
// (netbox.project). Returning a fixed result regardless of the fields would make
// any test about which fields were requested pass for the wrong reason: the
// column would be there because the fake always supplies it.
func (f *fakeProvider) ResolveIPs(_ context.Context, ips []string, fields []string, _ int) (*provider.Result, error) {
	f.ipCalls++
	f.ipsSeen = slices.Clone(ips)
	f.ipFields = slices.Clone(fields)
	if f.ipRow == nil {
		return f.ipResult, nil
	}
	row := make(map[string]interface{}, len(fields))
	for _, c := range fields {
		row[c] = f.ipRow[c]
	}
	return &provider.Result{Columns: slices.Clone(fields), Rows: []map[string]interface{}{row}, Total: 1}, nil
}
func (f *fakeProvider) ResolveScope(_ context.Context, filters []provider.Filter, fields []string, limit int) (*provider.Result, error) {
	f.scopeCalls++
	f.scopeFilters, f.scopeFields, f.scopeLimit = slices.Clone(filters), slices.Clone(fields), limit
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.scopeResult, nil
}
func (f *fakeProvider) Topology(_ context.Context, spec provider.TopologySpec) (*provider.Graph, error) {
	f.topoSpec = spec
	return f.graph, nil
}
func (f *fakeProvider) FilterFields(ctx context.Context, _ string) ([]provider.FilterField, error) {
	f.filterFieldsBranch = provider.BranchFromContext(ctx)
	return f.filterFields, f.filterFieldsErr
}

// BranchingInstalled makes fakeProvider satisfy the optional
// provider.BranchingCapable capability.
func (f *fakeProvider) BranchingInstalled(context.Context) (bool, bool) {
	return f.branchingInstalled, f.branchingConcl
}

func newTestDatasource(p provider.Provider) *Datasource {
	d := &Datasource{
		cfg:      &models.PluginSettings{URL: "http://nb", Secrets: &models.SecretPluginSettings{APIToken: "t"}},
		provider: p,
	}
	d.resourceHandler = httpadapter.New(d.newRouter())
	return d
}

func TestCheckHealth(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		d := &Datasource{cfg: &models.PluginSettings{URL: "http://nb", Secrets: &models.SecretPluginSettings{}}, provider: &fakeProvider{}}
		res, _ := d.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
		if res.Status != backend.HealthStatusError {
			t.Errorf("status = %v, want error", res.Status)
		}
	})
	t.Run("ok", func(t *testing.T) {
		d := newTestDatasource(&fakeProvider{healthMsg: "Connected to NetBox 4.5.8"})
		res, _ := d.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
		if res.Status != backend.HealthStatusOk {
			t.Errorf("status = %v, want ok", res.Status)
		}
	})
	// Save & test is read by whoever configures the datasource, and its
	// message is stored with the datasource; it says what kind of failure
	// happened and where to look, never the address, port or path the raw
	// transport error carries. The raw error goes to the server log.
	t.Run("unreachable NetBox is reported without its address", func(t *testing.T) {
		raw := fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: "http://10.0.0.5:8000/api/status/",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}})
		d := newTestDatasource(&fakeProvider{healthErr: raw})
		res, _ := d.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("status = %v, want error", res.Status)
		}
		if !strings.Contains(res.Message, "refused") || !strings.Contains(res.Message, "log") {
			t.Errorf("message = %q, want the cause and where the detail is", res.Message)
		}
		for _, leak := range []string{"10.0.0.5", "8000", "/api/status", "dial tcp"} {
			if strings.Contains(res.Message, leak) {
				t.Errorf("message %q leaks %q", res.Message, leak)
			}
		}
	})
}

func TestQueryData_Objects(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		result: &provider.Result{
			Columns: []string{"name", "site"},
			Rows: []map[string]interface{}{
				{"name": "leaf1", "site": "dc1"},
			},
		},
	})
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"objectType":"dcim/devices"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	dr := resp.Responses["A"]
	if dr.Error != nil {
		t.Fatalf("response error: %v", dr.Error)
	}
	if len(dr.Frames) != 1 || dr.Frames[0].Fields[0].Name != "name" {
		t.Fatalf("unexpected frames: %+v", dr.Frames)
	}
	if dr.Frames[0].Fields[0].Len() != 1 {
		t.Errorf("rows = %d, want 1", dr.Frames[0].Fields[0].Len())
	}
}

func TestQueryData_Annotations(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		changes: []provider.Change{{Action: "Updated", ObjectRepr: "leaf1", ObjectType: "dcim.device"}},
	})
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "Anno", JSON: []byte(`{"queryType":"annotations"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	frames := resp.Responses["Anno"].Frames
	if len(frames) != 1 || frames[0].Fields[0].Name != "time" {
		t.Fatalf("unexpected annotation frames: %+v", frames)
	}
}

func TestQueryData_Topology(t *testing.T) {
	fp := &fakeProvider{
		graph: &provider.Graph{
			Nodes: []provider.GraphNode{{ID: "1", Title: "leaf1", Status: "active"}, {ID: "2", Title: "leaf2", Status: "offline"}},
			Edges: []provider.GraphEdge{{ID: "10", Source: "1", Target: "2"}},
		},
	}
	d := newTestDatasource(fp)
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"queryType":"topology","connections":"physical"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	if fp.topoSpec.Connections != "physical" {
		t.Errorf("connections passthrough = %q, want physical", fp.topoSpec.Connections)
	}
	frames := resp.Responses["A"].Frames
	if len(frames) != 2 {
		t.Fatalf("expected nodes+edges frames, got %d", len(frames))
	}
	if frames[0].Name != "nodes" || frames[1].Name != "edges" {
		t.Errorf("frame names = %q,%q", frames[0].Name, frames[1].Name)
	}
	if frames[0].Meta == nil || frames[0].Meta.PreferredVisualization != "nodeGraph" {
		t.Errorf("nodes frame should prefer nodeGraph")
	}
}

func TestQueryData_IPEnrichment(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		ipResult: &provider.Result{
			Columns: []string{"ip", "prefix", "site"},
			Rows:    []map[string]interface{}{{"ip": "10.0.0.5", "prefix": "10.0.0.0/24", "site": "dc1"}},
		},
	})
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"queryType":"ip-enrichment","ips":"10.0.0.5"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	dr := resp.Responses["A"]
	if dr.Error != nil || len(dr.Frames) != 1 || dr.Frames[0].Fields[0].Name != "ip" {
		t.Fatalf("unexpected ip-enrichment response: %+v err=%v", dr.Frames, dr.Error)
	}
}

func TestResource_ObjectTypes(t *testing.T) {
	d := newTestDatasource(&fakeProvider{types: []provider.ObjectType{{Value: "dcim/devices", Label: "Devices"}}})

	rec := httptest.NewRecorder()
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/object-types", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []provider.ObjectType
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Value != "dcim/devices" {
		t.Errorf("got %+v", got)
	}
}

func TestResource_Query(t *testing.T) {
	d := newTestDatasource(&fakeProvider{result: &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "leaf1"}}}})
	rec := httptest.NewRecorder()
	body := `{"objectType":"dcim/devices","fields":["name"]}`
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got provider.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Errorf("rows = %d", len(got.Rows))
	}
}

func TestQueryData_AlertTable(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		result: &provider.Result{
			Columns: []string{"name", "site", "status"},
			Rows: []map[string]interface{}{
				{"name": "leaf1", "site": "dc1", "status": "offline"},
				{"name": "leaf2", "site": "dc2", "status": "offline"},
			},
		},
	})
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"objectType":"dcim/devices","alertTable":true}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	dr := resp.Responses["A"]
	if dr.Error != nil {
		t.Fatalf("response error: %v", dr.Error)
	}
	if len(dr.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(dr.Frames))
	}
	f := dr.Frames[0]
	last := f.Fields[len(f.Fields)-1]
	if last.Name != "value" {
		t.Fatalf("last field = %q, want value", last.Name)
	}
	if last.Len() != 2 || last.At(0).(float64) != 1 {
		t.Errorf("value column = len %d first %v, want len 2 first 1", last.Len(), last.At(0))
	}
	// count precedence: count:true beats alertTable:true
	req2 := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "B", JSON: []byte(`{"objectType":"dcim/devices","alertTable":true,"count":true}`)}},
	}
	resp2, _ := d.QueryData(context.Background(), req2)
	f2 := resp2.Responses["B"].Frames[0]
	if f2.Fields[0].Name != "count" {
		t.Errorf("count should win over alertTable, got field %q", f2.Fields[0].Name)
	}
}

func TestQueryData_AlertTable_MissingValueField(t *testing.T) {
	d := newTestDatasource(&fakeProvider{
		result: &provider.Result{
			Columns: []string{"prefix", "site"},
			Rows:    []map[string]interface{}{{"prefix": "10.0.0.0/24", "site": "dc1"}},
		},
	})
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"objectType":"ipam/prefixes","alertTable":true,"valueField":"utilization"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	if resp.Responses["A"].Error == nil {
		t.Fatal("expected an error when valueField is absent from results, got none")
	}
}

// TestQueryData_AlertTable_ValueFieldProjection locks both halves of the
// query.go:135 guard that appends an implied Value field to Fields:
//   - a non-empty requested subset gets the Value field appended when missing,
//     without losing the originally requested fields;
//   - an empty requested subset (meaning "all fields") is left empty — the
//     len(fields)>0 guard must not narrow it down to just the Value field.
func TestQueryData_AlertTable_ValueFieldProjection(t *testing.T) {
	t.Run("value field appended to a non-empty subset", func(t *testing.T) {
		fp := &fakeProvider{
			result: &provider.Result{
				Columns: []string{"name", "site", "utilization"},
				Rows: []map[string]interface{}{
					{"name": "leaf1", "site": "dc1", "utilization": 42.0},
				},
			},
		}
		d := newTestDatasource(fp)
		req := &backend.QueryDataRequest{
			Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(
				`{"objectType":"ipam/prefixes","alertTable":true,"fields":["name","site"],"valueField":"utilization"}`,
			)}},
		}
		resp, err := d.QueryData(context.Background(), req)
		if err != nil {
			t.Fatalf("QueryData: %v", err)
		}
		if dr := resp.Responses["A"]; dr.Error != nil {
			t.Fatalf("response error: %v", dr.Error)
		}
		if !slices.Contains(fp.querySpec.Fields, "utilization") {
			t.Errorf("provider Fields = %v, want it to contain the appended valueField %q", fp.querySpec.Fields, "utilization")
		}
		if !slices.Contains(fp.querySpec.Fields, "name") || !slices.Contains(fp.querySpec.Fields, "site") {
			t.Errorf("provider Fields = %v, want it to still contain the originally requested fields", fp.querySpec.Fields)
		}
	})

	t.Run("empty requested fields stay empty despite a value field", func(t *testing.T) {
		fp := &fakeProvider{
			result: &provider.Result{
				Columns: []string{"name", "site", "utilization"},
				Rows: []map[string]interface{}{
					{"name": "leaf1", "site": "dc1", "utilization": 42.0},
				},
			},
		}
		d := newTestDatasource(fp)
		req := &backend.QueryDataRequest{
			Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(
				`{"objectType":"ipam/prefixes","alertTable":true,"valueField":"utilization"}`,
			)}},
		}
		resp, err := d.QueryData(context.Background(), req)
		if err != nil {
			t.Fatalf("QueryData: %v", err)
		}
		if dr := resp.Responses["A"]; dr.Error != nil {
			t.Fatalf("response error: %v", dr.Error)
		}
		if len(fp.querySpec.Fields) != 0 {
			t.Errorf("provider Fields = %v, want empty (len(fields)>0 guard must not narrow an unset field selection)", fp.querySpec.Fields)
		}
	})
}

func TestQueryData_BranchContext(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "leaf1"}}}}
	d := newTestDatasource(fp)
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"objectType":"dcim/devices","branch":"td5smq0f"}`)}},
	}
	if _, err := d.QueryData(context.Background(), req); err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	if fp.branchSeen != "td5smq0f" {
		t.Errorf("branch reaching provider = %q, want td5smq0f", fp.branchSeen)
	}
}

func TestQueryData_MapsAPIError(t *testing.T) {
	d := newTestDatasource(&fakeProvider{queryErr: &netbox.APIError{Status: 405, URL: "http://nb/api/extras/scripts/upload/", Body: `{"detail":"Method \"GET\" not allowed."}`}})
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"objectType":"extras/scripts/upload"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
	}
	dr := resp.Responses["A"]
	if dr.Error == nil {
		t.Fatal("expected an error response")
	}
	msg := dr.Error.Error()
	if !strings.Contains(msg, "HTTP 405") || strings.Contains(msg, "not allowed") {
		t.Errorf("response error = %q; want mapped 405 message without raw body", msg)
	}
}

// TestQueryData_FastPaging_AlertRulesAreUnaffected drives the whole real path —
// the FromAlert header Grafana sends, QueryData, the NetBox provider with the
// datasource's fast-paging opt-in ON — and looks at what actually goes on the
// wire.
//
// A stub provider can only show which flag the plugin set; the parameter NetBox
// receives is the thing the claim on the config switch is about. `start` is
// NetBox 4.6 cursor pagination: it is what returns `"count": null`, which
// decodes to Total 0 and reads exactly like "nothing matched".
func TestQueryData_FastPaging_AlertRulesAreUnaffected(t *testing.T) {
	const objectsQuery = `{"queryType":"objects","objectType":"dcim/devices","limit":100}`

	run := func(t *testing.T, headers map[string]string) (url.Values, *backend.QueryDataResponse) {
		t.Helper()
		var got url.Values
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Query()
			// Self-consistent: one row and a count of one. An earlier version
			// claimed count 7 while returning a single row, which no real NetBox
			// does at limit=100 — and once the alert path started rejecting a
			// truncated result, that inconsistency failed the test for a reason
			// unrelated to what it checks.
			count := "1"
			if got.Has("start") {
				// What a cursor-paged NetBox answers with.
				count = "null"
			}
			_, _ = fmt.Fprintf(w, `{"count":%s,"next":null,"results":[{"id":1,"name":"leaf1"}]}`, count)
		}))
		defer srv.Close()

		d := newTestDatasource(netbox.New(srv.URL, "t", srv.Client(), netbox.WithCursorPaging(true)))
		resp, err := d.QueryData(context.Background(), &backend.QueryDataRequest{
			Headers: headers,
			Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(objectsQuery)}},
		})
		if err != nil {
			t.Fatalf("QueryData: %v", err)
		}
		if dr := resp.Responses["A"]; dr.Error != nil {
			t.Fatalf("query error: %v", dr.Error)
		}
		return got, resp
	}

	t.Run("an alert evaluation keeps the counted path", func(t *testing.T) {
		got, resp := run(t, map[string]string{backend.FromAlertHeaderName: "true"})
		if got.Has("start") {
			t.Errorf("alert evaluation sent cursor paging (%v): the rule would evaluate the lowest-ID page against a null count", got)
		}
		// The count survived, so the frame carries no "the total is unavailable"
		// note — the state in which an alert rule can be trusted at all.
		notices := resp.Responses["A"].Frames[0].Meta.Notices
		for _, n := range notices {
			if strings.Contains(n.Text, "total") {
				t.Errorf("alert frame reports a missing total: %q", n.Text)
			}
		}
	})

	t.Run("a dashboard table still uses it", func(t *testing.T) {
		got, _ := run(t, nil)
		if !got.Has("start") {
			t.Errorf("dashboard query did not use cursor paging (%v): the setting now does nothing", got)
		}
	})
}

// The two backends authenticate with different credentials against different
// services. Checking NetBox's prerequisites against a replica-cache datasource
// failed Save & Test on a correctly provisioned instance, before the provider
// was ever reached.
func TestMissingSettingIsModeAware(t *testing.T) {
	full := func() *models.PluginSettings {
		return &models.PluginSettings{
			Mode:            models.ModeReplicaCache,
			ReplicaCacheURL: "https://cache.example.com",
			NetBoxID:        "nb-1",
			Secrets:         &models.SecretPluginSettings{ReplicaCacheToken: "ff_x"},
		}
	}

	t.Run("replica-cache needs no NetBox token", func(t *testing.T) {
		cfg := full()
		if msg := missingSetting(cfg); msg != "" {
			t.Errorf("a fully configured replica-cache datasource was rejected: %q", msg)
		}
	})

	t.Run("replica-cache reports its own missing settings", func(t *testing.T) {
		cases := map[string]func(*models.PluginSettings){
			"replica-cache URL is missing":  func(c *models.PluginSettings) { c.ReplicaCacheURL = "" },
			"NetBox instance ID is missing": func(c *models.PluginSettings) { c.NetBoxID = "" },
			"replica-cache token is missing": func(c *models.PluginSettings) {
				c.Secrets = &models.SecretPluginSettings{}
			},
		}
		for want, break_ := range cases {
			cfg := full()
			break_(cfg)
			if got := missingSetting(cfg); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		}
	})

	t.Run("netbox mode is unchanged", func(t *testing.T) {
		cfg := &models.PluginSettings{Mode: models.ModeNetBox}
		if got := missingSetting(cfg); got != "NetBox URL is missing" {
			t.Errorf("got %q", got)
		}
		cfg.URL = "https://netbox.example.com"
		if got := missingSetting(cfg); got != "API token is missing" {
			t.Errorf("got %q", got)
		}
		cfg.Secrets = &models.SecretPluginSettings{APIToken: "t"}
		if got := missingSetting(cfg); got != "" {
			t.Errorf("a configured NetBox datasource was rejected: %q", got)
		}
	})

	// An empty Mode defaults to NetBox in LoadPluginSettings, and must behave
	// the same here rather than falling into a mode-specific branch.
	t.Run("an unset mode behaves as netbox", func(t *testing.T) {
		cfg := &models.PluginSettings{URL: "https://netbox.example.com",
			Secrets: &models.SecretPluginSettings{APIToken: "t"}}
		if got := missingSetting(cfg); got != "" {
			t.Errorf("got %q, want no error", got)
		}
	})
}

// Failing construction meant no Datasource existed for CheckHealth to run on,
// so Save & Test never reached missingSetting — the one place that can name
// WHICH field is absent. A provisioned datasource missing its URL reported a
// construction failure instead.
func TestReplicaCacheHealthNamesTheMissingSetting(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{"no url", `{"mode":"replica-cache","netboxId":"nb-1"}`, "replica-cache URL is missing"},
		{"no instance id", `{"mode":"replica-cache","replicaCacheUrl":"http://c"}`, "NetBox instance ID is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := NewDatasource(context.Background(), backend.DataSourceInstanceSettings{
				JSONData:                []byte(tc.json),
				DecryptedSecureJSONData: map[string]string{"replicaCacheToken": "t"},
			})
			if err != nil {
				t.Fatalf("construction must succeed so Save & Test can explain: %v", err)
			}
			ds, ok := inst.(*Datasource)
			if !ok {
				t.Fatalf("want a *Datasource, got %T", inst)
			}
			res, err := ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
			if err != nil {
				t.Fatalf("CheckHealth: %v", err)
			}
			if res.Status != backend.HealthStatusError {
				t.Errorf("status = %v, want an error", res.Status)
			}
			if !strings.Contains(res.Message, tc.want) {
				t.Errorf("message = %q, want it to name %q", res.Message, tc.want)
			}
		})
	}
}
