package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
func (f *fakeProvider) ResolveIPs(_ context.Context, _ []string, fields []string, _ int) (*provider.Result, error) {
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
