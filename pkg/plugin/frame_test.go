package plugin

import (
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
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

	want := []string{"time", "title", "text", "tags", "id"}
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

func annText(t *testing.T, c provider.Change) string {
	t.Helper()
	return buildAnnotationsFrame([]provider.Change{c}).Fields[2].At(0).(string)
}

func TestBuildAnnotationsFrame_DiffLines(t *testing.T) {
	c := provider.Change{
		Action: "Updated", ObjectType: "dcim.device", ObjectRepr: "leaf1", User: "admin",
		URL: "https://nb/dcim/devices/1/",
		Diff: []provider.FieldChange{
			{Field: "platform", Before: "IOS", After: "IOS-XE"},
			{Field: "role", Before: "Router", After: "Distribution Switch"},
		},
	}
	want := `platform: IOS → IOS-XE<br>role: Router → Distribution Switch<br>Updated by admin · <a href="https://nb/dcim/devices/1/" target="_blank">Open in NetBox</a>`
	if got := annText(t, c); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

func TestBuildAnnotationsFrame_CapsAtFiveFields(t *testing.T) {
	var diff []provider.FieldChange
	for _, f := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		diff = append(diff, provider.FieldChange{Field: f, Before: "1", After: "2"})
	}
	got := annText(t, provider.Change{Action: "Updated", Diff: diff})
	want := "a: 1 → 2<br>b: 1 → 2<br>c: 1 → 2<br>d: 1 → 2<br>e: 1 → 2<br>+2 more<br>Updated"
	if got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

func TestBuildAnnotationsFrame_EmptyLongAndMultilineValues(t *testing.T) {
	long := strings.Repeat("x", 61)
	got := annText(t, provider.Change{Action: "Updated", Diff: []provider.FieldChange{
		{Field: "a", Before: "", After: "v"},
		{Field: "b", Before: long, After: "w"},
		{Field: "c", Before: "line1\nline2", After: "z"},
	}})
	want := "a: — → v<br>b: " + strings.Repeat("x", 60) + "… → w<br>c: line1 line2 → z<br>Updated"
	if got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

func TestBuildAnnotationsFrame_ParentAndMessage(t *testing.T) {
	f := buildAnnotationsFrame([]provider.Change{{
		Action: "Created", ObjectRepr: "eth0", Parent: "leaf1", Message: "CHG-1", User: "bob",
	}})
	if got := f.Fields[1].At(0).(string); got != "eth0 on leaf1" {
		t.Errorf("title = %q", got)
	}
	if got, want := f.Fields[2].At(0).(string), `"CHG-1"<br>Created by bob`; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

func TestBuildAnnotationsFrame_EscapesEveryNetBoxValue(t *testing.T) {
	const raw = `<b>"&`
	f := buildAnnotationsFrame([]provider.Change{
		{Action: "Updated", ObjectRepr: raw, Parent: raw, User: raw, Message: raw, URL: raw,
			Diff: []provider.FieldChange{{Field: raw, Before: raw, After: raw}}},
		{Kind: provider.ChangeKindJournal, ObjectRepr: raw, Comments: raw, User: raw, URL: raw},
	})
	const esc = "&lt;b&gt;&#34;&amp;"
	for i := 0; i < 2; i++ {
		for _, col := range []int{1, 2} {
			s := f.Fields[col].At(i).(string)
			if strings.Contains(s, "<b>") {
				t.Errorf("row %d field %d has raw <b>: %q", i, col, s)
			}
			if !strings.Contains(s, esc) {
				t.Errorf("row %d field %d missing escaped value: %q", i, col, s)
			}
		}
		if s := f.Fields[2].At(i).(string); !strings.Contains(s, `href="`+esc+`"`) {
			t.Errorf("row %d URL not escaped: %q", i, s)
		}
	}
	// Every value in the first row's text: field, before, after, message, user.
	if s := f.Fields[2].At(0).(string); strings.Count(s, esc) != 6 {
		t.Errorf("expected 6 escaped values (field, before, after, message, user, url), got %q", s)
	}
}

func TestBuildAnnotationsFrame_Journal(t *testing.T) {
	f := buildAnnotationsFrame([]provider.Change{{
		Kind: provider.ChangeKindJournal, ObjectRepr: "leaf1", JournalKind: "warning",
		Comments: "line1\nline2", User: "admin",
	}})
	if got, want := f.Fields[2].At(0).(string), "line1<br>line2<br>Journal by admin"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if got := f.Fields[3].At(0).(string); got != "netbox,journal,warning" {
		t.Errorf("tags = %q", got)
	}
}

func TestBuildAnnotationsFrame_Cutoff(t *testing.T) {
	f := buildAnnotationsFrame([]provider.Change{{Kind: provider.ChangeKindCutoff, Scanned: 2500}})
	if got := f.Fields[1].At(0).(string); got != "NetBox change log" {
		t.Errorf("title = %q", got)
	}
	want := "Older changes not checked: read 2,500 change-log entries without reaching the start of the range."
	if got := f.Fields[2].At(0).(string); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if got := f.Fields[3].At(0).(string); got != "netbox,cutoff" {
		t.Errorf("tags = %q", got)
	}
}

func TestBuildAnnotationsFrame_IDFieldIsZero(t *testing.T) {
	f := buildAnnotationsFrame([]provider.Change{{Action: "Created"}, {Action: "Deleted"}})
	idf := f.Fields[4]
	if idf.Name != "id" || idf.Type() != data.FieldTypeInt64 {
		t.Fatalf("id field = %q %v", idf.Name, idf.Type())
	}
	for i := 0; i < idf.Len(); i++ {
		if idf.At(i).(int64) != 0 {
			t.Errorf("id[%d] = %v", i, idf.At(i))
		}
	}
}

// The error marker's message can quote a dashboard variable's value, so it is
// escaped like any NetBox value.
func TestBuildAnnotationErrorFrame_Escapes(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	f := annotationErrorFrame(`No NetBox object named "<b>&" for this annotation's scope.`, at)

	want := []string{"time", "title", "text", "tags", "id"}
	if len(f.Fields) != len(want) {
		t.Fatalf("error frame has %d fields, want %d", len(f.Fields), len(want))
	}
	for i, name := range want {
		if f.Fields[i].Name != name {
			t.Errorf("field %d = %q, want %q", i, f.Fields[i].Name, name)
		}
	}
	if n, _ := f.RowLen(); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if got := f.Fields[0].At(0).(time.Time); !got.Equal(at) {
		t.Errorf("time = %v, want %v", got, at)
	}
	if got := f.Fields[1].At(0).(string); got != "NetBox annotation error" {
		t.Errorf("title = %q", got)
	}
	if got, want := f.Fields[2].At(0).(string), "No NetBox object named &#34;&lt;b&gt;&amp;&#34; for this annotation&#39;s scope."; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if got := f.Fields[3].At(0).(string); got != "netbox,error" {
		t.Errorf("tags = %q", got)
	}
	if got := f.Fields[4].At(0).(int64); got != 0 {
		t.Errorf("id = %d, want 0", got)
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
			{"prefix": "10.0.1.0/24", "site": "dc1", "utilization": "42"},  // numeric string parses
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

// TestBuildFrame_DeclaredColumnTypeSurvivesAnEmptyColumn covers the case value
// scanning cannot decide. is_primary_ip is nil in every row whenever the current
// IP set resolves no owner that HAS a primary IP — every address external,
// FHRP-assigned or unassigned — and the column was then emitted as a STRING
// field, while the same saved query emitted a BOOLEAN one on the refresh where
// one device matched. Boolean value mappings, `filterByValue isTrue`, field
// overrides and transformations are bound to the field type, so they silently
// stopped applying on whichever refresh happened to resolve nothing.
//
// Extending is_primary_ip beyond devices made that refresh ordinary rather than
// rare: a VM-assigned address now fills the column, but an FHRP-assigned one
// deliberately never does, so an all-null column is a normal shape for a
// VRRP-heavy IP list rather than a sign that nothing resolved.
func TestBuildFrame_DeclaredColumnTypeSurvivesAnEmptyColumn(t *testing.T) {
	// Two IPs NetBox holds no device for: the honest result, and the one that
	// used to change the column's type.
	rows := []map[string]interface{}{
		{"ip": "8.8.8.8", "match_count": float64(0), "is_primary_ip": nil},
		{"ip": "1.1.1.1", "match_count": float64(0), "is_primary_ip": nil},
	}
	cols := []string{"ip", "match_count", "is_primary_ip"}

	t.Run("declared boolean stays a nullable bool field with no values at all", func(t *testing.T) {
		frame := buildFrame("ip-enrichment", &provider.Result{
			Columns: cols, Rows: rows,
			ColumnTypes: map[string]provider.FieldType{
				"match_count":   provider.FieldTypeNumber,
				"is_primary_ip": provider.FieldTypeBoolean,
			},
		}, "http://nb")

		byName := map[string]*data.Field{}
		for _, f := range frame.Fields {
			byName[f.Name] = f
		}
		if got := byName["is_primary_ip"].Type(); got != data.FieldTypeNullableBool {
			t.Errorf("is_primary_ip type = %v, want %v — the column is a boolean by schema even on a refresh where no IP resolves a device", got, data.FieldTypeNullableBool)
		}
		if got := byName["is_primary_ip"].Len(); got != len(rows) {
			t.Errorf("is_primary_ip len = %d, want %d", got, len(rows))
		}
		if v, ok := byName["is_primary_ip"].ConcreteAt(0); ok {
			t.Errorf("value at 0 = %v, want no concrete value: null means \"this IP has no device\", which is not the same claim as false", v)
		}
	})

	t.Run("an undeclared all-null column keeps the string fallback", func(t *testing.T) {
		// The objects path: columns are discovered from NetBox at runtime and
		// declare nothing, so nothing about their typing may change.
		frame := buildFrame("dcim/devices", &provider.Result{
			Columns: []string{"name", "comments"},
			Rows: []map[string]interface{}{
				{"name": "leaf1", "comments": nil},
				{"name": "leaf2", "comments": nil},
			},
		}, "http://nb")
		for _, f := range frame.Fields {
			if f.Name == "comments" && f.Type() != data.FieldTypeString {
				t.Errorf("comments type = %v, want %v", f.Type(), data.FieldTypeString)
			}
		}
	})

	t.Run("observed values win over a declaration that contradicts them", func(t *testing.T) {
		// A declaration must never be able to delete data: building a bool field
		// out of string values would drop every one of them.
		frame := buildFrame("ip-enrichment", &provider.Result{
			Columns: []string{"is_primary_ip"},
			Rows: []map[string]interface{}{
				{"is_primary_ip": "yes"},
			},
			ColumnTypes: map[string]provider.FieldType{"is_primary_ip": provider.FieldTypeBoolean},
		}, "http://nb")
		if got := frame.Fields[0].Type(); got != data.FieldTypeString {
			t.Errorf("type = %v, want %v: values decide whenever there are any", got, data.FieldTypeString)
		}
	})

	t.Run("a resolved device still yields a nullable bool", func(t *testing.T) {
		// The other end of the same saved query: whichever way the data falls,
		// the field type is the same one.
		yes := true
		frame := buildFrame("ip-enrichment", &provider.Result{
			Columns: cols,
			Rows: []map[string]interface{}{
				{"ip": "10.20.0.1", "match_count": float64(1), "is_primary_ip": yes},
				{"ip": "8.8.8.8", "match_count": float64(0), "is_primary_ip": nil},
			},
			ColumnTypes: map[string]provider.FieldType{
				"match_count":   provider.FieldTypeNumber,
				"is_primary_ip": provider.FieldTypeBoolean,
			},
		}, "http://nb")
		for _, f := range frame.Fields {
			if f.Name == "is_primary_ip" && f.Type() != data.FieldTypeNullableBool {
				t.Errorf("is_primary_ip type = %v, want %v", f.Type(), data.FieldTypeNullableBool)
			}
		}
	})
}
