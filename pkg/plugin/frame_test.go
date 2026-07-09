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
