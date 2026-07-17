package plugin

import (
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netbox/pkg/provider"
)

func TestBuildFrame_TypesAndLinks(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"name", "interface_count", "enabled", "created", "display_url"},
		Rows: []map[string]interface{}{
			{"name": "leaf1", "interface_count": float64(48), "enabled": true, "created": "2026-06-01T00:00:00Z", "display_url": "http://nb/dcim/devices/1/"},
			{"name": "leaf2", "interface_count": float64(24), "enabled": false, "created": "2026-06-02T00:00:00Z", "display_url": "http://nb/dcim/devices/2/"},
		},
	}
	frame := buildFrame("dcim/devices", res, "http://nb")

	if frame.Meta == nil || frame.Meta.PreferredVisualization != data.VisTypeTable {
		t.Errorf("frame should prefer table visualization")
	}

	byName := map[string]*data.Field{}
	for _, f := range frame.Fields {
		byName[f.Name] = f
	}

	if byName["name"].Type() != data.FieldTypeString {
		t.Errorf("name type = %v", byName["name"].Type())
	}
	if byName["interface_count"].Type() != data.FieldTypeNullableFloat64 {
		t.Errorf("interface_count type = %v", byName["interface_count"].Type())
	}
	if byName["enabled"].Type() != data.FieldTypeNullableBool {
		t.Errorf("enabled type = %v", byName["enabled"].Type())
	}
	if byName["created"].Type() != data.FieldTypeNullableTime {
		t.Errorf("created type = %v", byName["created"].Type())
	}

	// Data link on the primary label field.
	nameField := byName["name"]
	if nameField.Config == nil || len(nameField.Config.Links) != 1 {
		t.Fatalf("expected a data link on name field")
	}
	if nameField.Config.Links[0].URL == "" {
		t.Errorf("data link URL empty")
	}
}

func TestBuildAnnotationsFrame(t *testing.T) {
	changes := []provider.Change{
		{Time: time.Now(), Action: "Updated", ObjectType: "dcim.device", ObjectRepr: "leaf1", User: "admin"},
	}
	frame := buildAnnotationsFrame(changes)

	want := []string{"time", "title", "text", "tags"}
	if len(frame.Fields) != len(want) {
		t.Fatalf("annotation frame has %d fields, want %d", len(frame.Fields), len(want))
	}
	for i, name := range want {
		if frame.Fields[i].Name != name {
			t.Errorf("field %d = %q, want %q", i, frame.Fields[i].Name, name)
		}
	}
	if frame.Fields[2].At(0).(string) != "Updated by admin" {
		t.Errorf("text = %v", frame.Fields[2].At(0))
	}
}

func TestToString(t *testing.T) {
	cases := map[interface{}]string{
		float64(48):  "48",
		float64(1.5): "1.5",
		true:         "true",
		"x":          "x",
		nil:          "",
	}
	for in, want := range cases {
		if got := toString(in); got != want {
			t.Errorf("toString(%#v) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildFrame_ReducibleForAlerting documents that object frames built from rows
// carrying a numeric column expose it as a numeric field. (Alerting on a plain table
// frame is not supported regardless — see the count option — so this is a
// characterization test of buildFrame's typing, not a guarantee of alert-reducibility.)
func TestBuildFrame_ReducibleForAlerting(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"id", "name", "status"},
		Rows: []map[string]interface{}{
			{"id": float64(1), "name": "leaf1", "status": "active"},
			{"id": float64(2), "name": "leaf2", "status": "offline"},
		},
	}
	frame := buildFrame("dcim/devices", res, "http://nb")

	hasNumeric := false
	for _, f := range frame.Fields {
		if f.Type() == data.FieldTypeNullableFloat64 {
			hasNumeric = true
			break
		}
	}
	if !hasNumeric {
		t.Fatal("object frame has no numeric field; a Grafana alert Reduce/Threshold expression cannot evaluate it")
	}
}

func TestBuildCountFrame(t *testing.T) {
	frame := buildCountFrame("dcim/devices", 3)
	if len(frame.Fields) != 1 {
		t.Fatalf("count frame has %d fields, want 1", len(frame.Fields))
	}
	if frame.Fields[0].Name != "count" {
		t.Errorf("field name = %q, want %q", frame.Fields[0].Name, "count")
	}
	if frame.Fields[0].Type() != data.FieldTypeFloat64 {
		t.Errorf("count type = %v, want %v", frame.Fields[0].Type(), data.FieldTypeFloat64)
	}
	if v, ok := frame.Fields[0].At(0).(float64); !ok || v != 3 {
		t.Errorf("count value = %v, want 3", frame.Fields[0].At(0))
	}
}

func TestBuildNodeGraphFrames_NoLinkWhenURLMissing(t *testing.T) {
	g := &provider.Graph{
		Nodes: []provider.GraphNode{
			{ID: "1", Title: "leaf1", URL: "https://nb.example/dcim/devices/1/"},
			{ID: "2", Title: "leaf2"}, // no URL
		},
	}
	nodes := buildNodeGraphFrames(g)[0]
	for _, f := range nodes.Fields {
		if f.Name != "url" {
			continue
		}
		if f.Config != nil && len(f.Config.Links) > 0 {
			t.Errorf("url field must not carry a link when any node URL is missing, got %+v", f.Config.Links)
		}
		return
	}
	t.Fatal("nodes frame missing url field")
}

func TestBuildNodeGraphFrames_EdgeKind(t *testing.T) {
	g := &provider.Graph{
		Nodes: []provider.GraphNode{{ID: "1", Title: "a"}, {ID: "2", Title: "b"}},
		Edges: []provider.GraphEdge{{ID: "e1", Source: "1", Target: "2", Kind: "path"}},
	}
	frames := buildNodeGraphFrames(g)
	edges := frames[1]
	for _, f := range edges.Fields {
		if f.Name == "detail__kind" {
			if got, ok := f.At(0).(string); !ok || got != "path" {
				t.Fatalf("detail__kind = %v, want path", f.At(0))
			}
			return
		}
	}
	t.Fatal("edges frame missing detail__kind field")
}

func TestBuildAlertFrame_NumericLongType(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"name", "site"},
		Rows:    []map[string]interface{}{{"name": "leaf1", "site": "dc1"}},
	}
	f := buildAlertFrame("dcim/devices", res, "")

	// Grafana's server-side expression engine (Reduce/Threshold) rejects an
	// untyped multi-row string+numeric frame ("input data must be a wide series
	// but got type ..."). Declaring numeric-long lets it convert the frame to
	// numeric-multi — one alert instance per row, string columns as labels.
	if f.Meta == nil {
		t.Fatal("alert frame has no Meta; SSE cannot classify it")
	}
	if f.Meta.Type != data.FrameTypeNumericLong {
		t.Errorf("Meta.Type = %q, want %q", f.Meta.Type, data.FrameTypeNumericLong)
	}
	if f.Meta.TypeVersion != (data.FrameTypeVersion{0, 1}) {
		t.Errorf("Meta.TypeVersion = %v, want {0 1}", f.Meta.TypeVersion)
	}
}

func TestBuildAlertFrame_ConstantValue(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"name", "site", "role"},
		Rows: []map[string]interface{}{
			{"name": "leaf1", "site": "dc1", "role": "switch"},
			{"name": "rtr1", "site": "dc2", "role": nil}, // nil label -> ""
		},
	}
	f := buildAlertFrame("dcim/devices", res, "")

	if f.Name != "netbox_devices" {
		t.Errorf("frame name = %q, want netbox_devices", f.Name)
	}
	if len(f.Fields) != 4 {
		t.Fatalf("fields = %d, want 4 (3 labels + value)", len(f.Fields))
	}
	// Exactly one numeric column, named "value", and it is the last field.
	last := f.Fields[len(f.Fields)-1]
	if last.Name != "value" {
		t.Errorf("last field = %q, want value", last.Name)
	}
	for _, fld := range f.Fields[:len(f.Fields)-1] {
		if _, ok := fld.At(0).(string); !ok {
			t.Errorf("label field %q is not a plain string", fld.Name)
		}
	}
	if got := last.At(0).(float64); got != 1 {
		t.Errorf("value[0] = %v, want 1", got)
	}
	if got := f.Fields[2].At(1).(string); got != "" {
		t.Errorf("nil label = %q, want empty string", got)
	}
}

func TestBuildAlertFrame_ValueField(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"prefix", "site", "utilization"},
		Rows: []map[string]interface{}{
			{"prefix": "10.0.0.0/24", "site": "dc1", "utilization": 87.5},
			{"prefix": "10.0.1.0/24", "site": "dc1", "utilization": "42"},   // numeric string parses
			{"prefix": "10.0.2.0/24", "site": "dc2", "utilization": "n/a"}, // unparseable -> 0
		},
	}
	f := buildAlertFrame("ipam/prefixes", res, "utilization")

	if len(f.Fields) != 3 {
		t.Fatalf("fields = %d, want 3 (prefix, site, value)", len(f.Fields))
	}
	if f.Fields[0].Name != "prefix" || f.Fields[1].Name != "site" || f.Fields[2].Name != "value" {
		t.Fatalf("field names = %q,%q,%q", f.Fields[0].Name, f.Fields[1].Name, f.Fields[2].Name)
	}
	want := []float64{87.5, 42, 0}
	for i, w := range want {
		if got := f.Fields[2].At(i).(float64); got != w {
			t.Errorf("value[%d] = %v, want %v", i, got, w)
		}
	}
}

func TestBuildNodeGraphFrames_NodeURLLink(t *testing.T) {
	g := &provider.Graph{
		Nodes: []provider.GraphNode{{ID: "1", Title: "leaf1", URL: "https://nb.example/dcim/devices/1/"}},
	}
	nodes := buildNodeGraphFrames(g)[0]
	var urlField *data.Field
	for _, f := range nodes.Fields {
		if f.Name == "url" {
			urlField = f
		}
	}
	if urlField == nil {
		t.Fatal("nodes frame missing url field")
	}
	if got, _ := urlField.At(0).(string); got != "https://nb.example/dcim/devices/1/" {
		t.Errorf("url[0] = %v, want the device URL", urlField.At(0))
	}
	if urlField.Config == nil || len(urlField.Config.Links) != 1 {
		t.Fatalf("url field must carry exactly one data link, got %+v", urlField.Config)
	}
	if link := urlField.Config.Links[0]; link.Title != "View in NetBox" || link.URL != `${__data.fields["url"]}` || !link.TargetBlank {
		t.Errorf("link = %+v, want {View in NetBox, ${__data.fields[\"url\"]}, TargetBlank}", link)
	}
}
