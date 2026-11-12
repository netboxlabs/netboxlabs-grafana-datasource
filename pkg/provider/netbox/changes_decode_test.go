package netbox

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// mustDecode decodes one change-log row and works out its diff, as
// walkChangeRead does for a row it keeps.
func mustDecode(t *testing.T, rec string) decodedChange {
	t.Helper()
	d, ok := decodeChangeRow(json.RawMessage(rec))
	if !ok {
		t.Fatalf("decodeChangeRow(%s) failed", rec)
	}
	d.diff = changeDiff(d.oc)
	return d
}

func TestChangeTitle_FallbackChain(t *testing.T) {
	tests := []struct {
		name string
		rec  string
		want string
	}{
		{"object_repr wins", `{"object_repr":"leaf1","changed_object":{"display":"other"},"postchange_data":{"name":"x"}}`, "leaf1"},
		{"string changed_object", `{"changed_object":"plugin-thing","postchange_data":{"name":"x"}}`, "plugin-thing"},
		{"changed_object display", `{"changed_object":{"display":"leaf2"},"postchange_data":{"name":"x"}}`, "leaf2"},
		{"postchange name", `{"changed_object_type":"dcim.device","changed_object":null,"postchange_data":{"name":"leaf3"},"prechange_data":{"name":"old"}}`, "leaf3"},
		{"prechange name on delete", `{"changed_object_type":"dcim.device","postchange_data":null,"prechange_data":{"name":"leaf4"}}`, "leaf4"},
		{"circuit cid", `{"changed_object_type":"circuits.circuit","postchange_data":{"cid":"CID-1","name":"nope"}}`, "CID-1"},
		{"type and id", `{"changed_object_type":"dcim.device","changed_object_id":42,"postchange_data":{"serial":"s"}}`, "dcim.device #42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustDecode(t, tc.rec).oc.title(); got != tc.want {
				t.Errorf("title() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeChange_StringChangedObject(t *testing.T) {
	d := mustDecode(t, `{"changed_object_type":"plugin.thing","changed_object":"plugin-thing","action":{"value":"update"}}`)
	if got := d.oc.title(); got != "plugin-thing" {
		t.Errorf("title() = %q, want plugin-thing", got)
	}
	if got := d.oc.deepLink("http://base"); got != "http://base" {
		t.Errorf("deepLink = %q, want the base", got)
	}
}

func TestChangeDiff_UpdateSkipsBookkeeping(t *testing.T) {
	d := mustDecode(t, `{"action":{"value":"update"},
		"prechange_data":{"name":"a","status":"active","_name":"x","created":"1","last_updated":"1","same":5},
		"postchange_data":{"name":"b","status":"active","_name":"y","created":"2","last_updated":"2","same":5,"site":3}}`)
	if len(d.diff) != 2 || d.diff[0].Field != "name" || d.diff[1].Field != "site" {
		t.Fatalf("diff = %+v, want name then site", d.diff)
	}
	if string(d.diff[0].Before) != `"a"` || string(d.diff[0].After) != `"b"` {
		t.Errorf("name diff = %s -> %s", d.diff[0].Before, d.diff[0].After)
	}
}

func TestChangeDiff_CustomFieldsPerKey(t *testing.T) {
	d := mustDecode(t, `{"action":{"value":"update"},
		"prechange_data":{"custom_fields":{"owner":"a","tier":1,"same":true}},
		"postchange_data":{"custom_fields":{"owner":"b","tier":1,"same":true,"new":"n"}}}`)
	var fields []string
	for _, f := range d.diff {
		fields = append(fields, f.Field)
	}
	if want := []string{"cf_new", "cf_owner"}; !reflect.DeepEqual(fields, want) {
		t.Errorf("fields = %v, want %v", fields, want)
	}
}

// Custom fields are written cf_<name> in Changed fields: a bare name is a
// standard field, and doesn't match the custom field it shares a name with.
// custom_fields matches any of them.
func TestChangeFilter_CustomFieldNames(t *testing.T) {
	d := mustDecode(t, `{"action":{"value":"update"},
		"prechange_data":{"custom_fields":{"owner":"a"}},
		"postchange_data":{"custom_fields":{"owner":"b"}}}`)
	for _, tc := range []struct {
		fields []string
		want   bool
	}{
		{[]string{"owner"}, false},
		{[]string{"cf_owner"}, true},
		{[]string{"custom_fields"}, true},
	} {
		if got := (changeFilter{fields: tc.fields}).keep(d); got != tc.want {
			t.Errorf("Changed fields %v keep a cf_owner change = %v, want %v", tc.fields, got, tc.want)
		}
	}
}

func TestChangeDiff_CreateAndDeleteHaveNone(t *testing.T) {
	for _, rec := range []string{
		`{"action":{"value":"create"},"prechange_data":null,"postchange_data":{"name":"a"}}`,
		`{"action":{"value":"delete"},"prechange_data":{"name":"a"},"postchange_data":null}`,
	} {
		if d := mustDecode(t, rec); len(d.diff) != 0 {
			t.Errorf("diff = %+v, want none for %s", d.diff, rec)
		}
	}
}

func TestChangeDiff_NumericCanonicalEquality(t *testing.T) {
	d := mustDecode(t, `{"action":{"value":"update"},
		"prechange_data":{"a":1,"b":1.5,"c":{"x":1,"y":2}},
		"postchange_data":{"a":1.0,"b":1.50,"c":{"y":2,"x":1.0}}}`)
	if len(d.diff) != 0 {
		t.Errorf("diff = %+v, want none: values are equal", d.diff)
	}
}

// Integers past 2^53 lose digits as float64, so 9007199254740992 and
// 9007199254740993 would compare equal and the change would vanish from the
// diff and from a Changed-fields filter. They compare, and show, exactly. A
// float literal still equals the integer it holds, as 1.0 equals 1.
func TestChangeDiff_LargeIntegersCompareExactly(t *testing.T) {
	d := mustDecode(t, `{"action":{"value":"update"},
		"prechange_data":{"x":9007199254740992,"y":[9007199254740992],"z":9007199254740992,"w":1e3},
		"postchange_data":{"x":9007199254740993,"y":[9007199254740993],"z":9007199254740992.0,"w":1000}}`)
	var fields []string
	for _, f := range d.diff {
		fields = append(fields, f.Field)
	}
	if want := []string{"x", "y"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %v, want %v", fields, want)
	}
	if before, after := formatValue(d.diff[0].Before), formatValue(d.diff[0].After); before != "9007199254740992" || after != "9007199254740993" {
		t.Errorf("x shows %s → %s, want 9007199254740992 → 9007199254740993", before, after)
	}
	if !(changeFilter{fields: []string{"x"}}).keep(d) {
		t.Error("Changed fields [x] dropped the update")
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{`null`, ""},
		{``, ""},
		{`"abc"`, "abc"},
		{`true`, "true"},
		{`false`, "false"},
		{`42`, "42"},
		{`42.0`, "42"},
		{`1.5`, "1.5"},
		{`1e3`, "1000"},
		{`9007199254740993`, "9007199254740993"},
		{`-9007199254740993`, "-9007199254740993"},
		{`[9007199254740993,1.0]`, "9007199254740993, 1"},
		{`[1,"a",true]`, "1, a, true"},
		{`[]`, ""},
		{`{"b":1,"a": 2}`, `{"b":1,"a":2}`},
	}
	for _, tc := range tests {
		if got := formatValue(json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("formatValue(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestChanges_UnscopedUpdateGetsDiff(t *testing.T) {
	rec := `{"id":7,"time":"2026-08-13T23:00:00Z","user_name":"admin","action":{"value":"update","label":"Updated"},
		"changed_object_type":"dcim.device","changed_object_id":12,
		"changed_object":{"display":"leaf1","display_url":"http://nb/dcim/devices/12/"},
		"message":"rack move",
		"prechange_data":{"name":"leaf1","status":"active","last_updated":"1"},
		"postchange_data":{"name":"leaf1","status":"offline","last_updated":"2"}}`
	s := &changeLogServer{records: []string{rec}}
	p := s.start(t)
	changes, err := p.Changes(context.Background(), provider.ChangeSpec{Limit: 10})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.ObjectRepr != "leaf1" || c.Message != "rack move" || c.Kind != provider.ChangeKindChange {
		t.Errorf("change = %+v", c)
	}
	if c.URL != "http://nb/dcim/devices/12/" {
		t.Errorf("URL = %q", c.URL)
	}
	want := []provider.FieldChange{{Field: "status", Before: "active", After: "offline"}}
	if !reflect.DeepEqual(c.Diff, want) {
		t.Errorf("Diff = %+v, want %+v", c.Diff, want)
	}
}

// NetBox writes some updates with no prechange snapshot: an ORM save without
// snapshot(), and the cable update seen live in the 8a run. Every field would
// look changed, so such a row has no diff at all.
func TestChangeDiff_UpdateWithoutPrechangeHasNone(t *testing.T) {
	post := `{"label":"MON514-cable","a_terminations":[36],"b_terminations":[37],"color":"","status":"connected"}`
	for _, pre := range []string{`"prechange_data":{},`, `"prechange_data":null,`, ``} {
		d := mustDecode(t, `{"action":{"value":"update"},`+pre+`"postchange_data":`+post+`}`)
		if len(d.diff) != 0 {
			t.Errorf("%s: diff = %+v, want none without a prechange snapshot", pre, d.diff)
		}
	}

	d := mustDecode(t, `{"action":{"value":"update"},"prechange_data":{"status":"planned"},"postchange_data":{"status":"active"}}`)
	if len(d.diff) != 1 || d.diff[0].Field != "status" {
		t.Errorf("diff = %+v, want an ordinary update's status change", d.diff)
	}
}

// The same holds the other way round: against no postchange snapshot every
// prechange field would look cleared.
func TestChangeDiff_UpdateWithoutPostchangeHasNone(t *testing.T) {
	pre := `{"label":"MON514-cable","a_terminations":[36],"color":"","status":"connected"}`
	for _, post := range []string{`,"postchange_data":{}`, `,"postchange_data":null`, ``} {
		d := mustDecode(t, `{"action":{"value":"update"},"prechange_data":`+pre+post+`}`)
		if len(d.diff) != 0 {
			t.Errorf("%q: diff = %+v, want none without a postchange snapshot", post, d.diff)
		}
	}
}

func TestChangeDiff_SkipsFieldsEmptyOnBothSides(t *testing.T) {
	d := mustDecode(t, `{"action":{"value":"update"},
		"prechange_data":{"label":"old"},
		"postchange_data":{"label":"MON514-cable","color":"","comments":"","description":null,"tags":[],"custom_fields":{"note":null,"ticket":"T-1"}}}`)
	var fields []string
	for _, f := range d.diff {
		fields = append(fields, f.Field)
	}
	if want := []string{"cf_ticket", "label"}; !reflect.DeepEqual(fields, want) {
		t.Errorf("fields = %v, want %v", fields, want)
	}

	d = mustDecode(t, `{"action":{"value":"update"},"prechange_data":{"color":null},"postchange_data":{"color":"x"}}`)
	if len(d.diff) != 1 || d.diff[0].Field != "color" {
		t.Errorf("diff = %+v, want null -> x kept", d.diff)
	}
}
