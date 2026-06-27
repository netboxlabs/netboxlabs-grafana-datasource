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
