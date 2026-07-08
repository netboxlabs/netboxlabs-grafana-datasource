package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"

	"github.com/netboxlabs/netbox/pkg/models"
	"github.com/netboxlabs/netbox/pkg/provider"
)

// fakeProvider is a controllable provider.Provider for unit tests.
type fakeProvider struct {
	healthMsg string
	healthErr error
	types     []provider.ObjectType
	result    *provider.Result
	changes   []provider.Change
	ipResult  *provider.Result
	graph     *provider.Graph
}

func (f *fakeProvider) Name() string    { return "fake" }
func (f *fakeProvider) BaseURL() string { return "http://nb" }
func (f *fakeProvider) HealthCheck(context.Context) (string, error) {
	return f.healthMsg, f.healthErr
}
func (f *fakeProvider) ObjectTypes(context.Context) ([]provider.ObjectType, error) {
	return f.types, nil
}
func (f *fakeProvider) Fields(context.Context, string) ([]provider.Field, error) {
	return []provider.Field{{Name: "name", Type: provider.FieldTypeString}}, nil
}
func (f *fakeProvider) Query(context.Context, provider.QuerySpec) (*provider.Result, error) {
	return f.result, nil
}
func (f *fakeProvider) FieldValues(context.Context, string, string, string, int) ([]string, error) {
	return []string{"a", "b"}, nil
}
func (f *fakeProvider) Changes(context.Context, provider.ChangeSpec) ([]provider.Change, error) {
	return f.changes, nil
}
func (f *fakeProvider) ResolveIPs(context.Context, []string, []string, int) (*provider.Result, error) {
	return f.ipResult, nil
}
func (f *fakeProvider) Topology(context.Context, provider.TopologySpec) (*provider.Graph, error) {
	return f.graph, nil
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
	d := newTestDatasource(&fakeProvider{
		graph: &provider.Graph{
			Nodes: []provider.GraphNode{{ID: "1", Title: "leaf1", Status: "active"}, {ID: "2", Title: "leaf2", Status: "offline"}},
			Edges: []provider.GraphEdge{{ID: "10", Source: "1", Target: "2"}},
		},
	})
	req := &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"queryType":"topology"}`)}},
	}
	resp, err := d.QueryData(context.Background(), req)
	if err != nil {
		t.Fatalf("QueryData: %v", err)
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
