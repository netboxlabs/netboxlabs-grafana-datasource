package netbox

import (
	"sort"
	"testing"
)

const schemaFixture = `{
  "paths": {
    "/api/ipam/prefixes/": {
      "get": {
        "parameters": [
          {"name":"prefix","in":"query"},
          {"name":"status","in":"query"},
          {"name":"status__ic","in":"query"},
          {"name":"status__isw","in":"query"},
          {"name":"status__n","in":"query"},
          {"name":"status__nic","in":"query"},
          {"name":"status__regex","in":"query"},
          {"name":"status__iregex","in":"query"},
          {"name":"status__foobar","in":"query"},
          {"name":"status__empty","in":"query","schema":{"type":"boolean"}},
          {"name":"mask_length","in":"query"},
          {"name":"mask_length__gte","in":"query"},
          {"name":"mask_length__lte","in":"query"},
          {"name":"depth","in":"query"},
          {"name":"depth__gte","in":"query"},
          {"name":"depth__lte","in":"query"},
          {"name":"vrf","in":"query"},
          {"name":"vrf__n","in":"query"},
          {"name":"q","in":"query"},
          {"name":"limit","in":"query"},
          {"name":"ordering","in":"query"},
          {"name":"omit","in":"query"}
        ]
      }
    },
    "/api/ipam/prefixes/{id}/": {
      "get": {"parameters": [{"name":"id","in":"path"}]}
    },
    "/netbox/api/dcim/devices/": {
      "get": {"parameters": [{"name":"name","in":"query"},{"name":"name__ic","in":"query"}]}
    }
  }
}`

func TestParseFilterFields(t *testing.T) {
	m, err := parseFilterFields([]byte(schemaFixture))
	if err != nil {
		t.Fatalf("parseFilterFields: %v", err)
	}
	got := m["ipam/prefixes"]
	// index by name
	ops := map[string][]string{}
	for _, ff := range got {
		ops[ff.Name] = ff.Operators
	}

	want := map[string][]string{
		"prefix": {""},
		// full documented lookup set is modeled; __foobar (unmodeled) is dropped.
		// order follows operatorOrder: "", n, ic, nic, isw, regex, iregex, empty, nempty.
		"status":      {"", "n", "ic", "nic", "isw", "regex", "iregex", "empty", "nempty"},
		"mask_length": {"", "gte", "lte"},
		"depth":       {"", "gte", "lte"}, // a real prefix filter, NOT a response param
		"vrf":         {"", "n"},
		"q":           {""},
	}
	for name, w := range want {
		if !equalStrs(ops[name], w) {
			t.Errorf("%s operators = %v, want %v", name, ops[name], w)
		}
	}
	// an unmodeled lookup suffix on a present base is ignored (not an operator).
	for _, op := range ops["status"] {
		if op == "foobar" {
			t.Error("unmodeled suffix __foobar should be ignored, not offered as an operator")
		}
	}
	// non-filter params (pagination/response-shaping) must not appear as fields;
	// omit is a NetBox 4.5.2+ response param, like fields.
	for _, bad := range []string{"limit", "ordering", "omit"} {
		if _, ok := ops[bad]; ok {
			t.Errorf("%s should be excluded from filter fields", bad)
		}
	}
	// exact ("") is always the first operator.
	if s := ops["status"]; len(s) == 0 || s[0] != "" {
		t.Errorf("exact operator should be first: %v", s)
	}

	// Exact field-name set: catches spurious fields (e.g. an unmodeled suffix like
	// status__nic leaking as its own field) and dropped fields in one assertion.
	var names []string
	for _, ff := range got {
		names = append(names, ff.Name)
	}
	sort.Strings(names)
	wantNames := []string{"depth", "mask_length", "prefix", "q", "status", "vrf"}
	if !equalStrs(names, wantNames) {
		t.Errorf("filter field names = %v, want %v", names, wantNames)
	}

	// Detail/sub-resource paths ({id}) are skipped — no junk map key.
	if _, ok := m["ipam/prefixes/{id}"]; ok {
		t.Error("detail path /api/ipam/prefixes/{id}/ must not produce a filter-fields entry")
	}

	// Base-path-mounted NetBox: "/netbox/api/dcim/devices/" must key as
	// "dcim/devices" (segment after /api/), not "netbox/api/dcim/devices", so
	// FilterFields(objectType) resolves.
	if _, ok := m["netbox/api/dcim/devices"]; ok {
		t.Error("base-path prefix must be stripped from the object-type key")
	}
	dev := m["dcim/devices"]
	if len(dev) == 0 {
		t.Fatal("dcim/devices from a base-path-mounted schema should be discovered")
	}
	var devOps []string
	for _, ff := range dev {
		if ff.Name == "name" {
			devOps = ff.Operators
		}
	}
	if !equalStrs(devOps, []string{"", "ic"}) {
		t.Errorf("dcim/devices name operators = %v, want [\"\" \"ic\"]", devOps)
	}
}

func TestParseFilterFields_EmptyImpliesNempty(t *testing.T) {
	m, err := parseFilterFields([]byte(`{"paths":{"/api/dcim/devices/":{"get":{"parameters":[
		{"name":"serial","in":"query"},
		{"name":"serial__empty","in":"query","schema":{"type":"boolean"}}]}}}}`))
	if err != nil {
		t.Fatalf("parseFilterFields: %v", err)
	}
	var ops []string
	for _, ff := range m["dcim/devices"] {
		if ff.Name == "serial" {
			ops = ff.Operators
		}
	}
	want := []string{"", "empty", "nempty"}
	if len(ops) != len(want) {
		t.Fatalf("serial operators = %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("serial operators = %v, want %v", ops, want)
		}
	}
}

// TestParseFilterFields_EmptyOnlyWhenParamIsBoolean pins the only signal that
// predicts whether `?x__empty=true` is accepted upstream: the declared type of
// the `__empty` PARAMETER ITSELF.
//
// The fixture is taken verbatim from /api/schema/ on the demo (NetBox 4.4.10)
// and is chosen to defeat the two heuristics that look right and are wrong:
//
//   - mtu vs speed — both integer fields on the SAME endpoint, yet
//     ?mtu__empty=true is 200 count=34 and ?speed__empty=true is
//     400 {"speed__empty":["Enter a whole number."]}. So the parent field's
//     type cannot be the rule.
//   - last_updated — a timestamp that is broken here, but 7 other date-time
//     fields accept __empty, so "suppress on timestamps" is not the rule either.
//
// label__empty carries no schema type at all: unknown means not offered.
func TestParseFilterFields_EmptyOnlyWhenParamIsBoolean(t *testing.T) {
	m, err := parseFilterFields([]byte(`{"paths":{"/api/dcim/interfaces/":{"get":{"parameters":[
		{"name":"mtu","in":"query","schema":{"type":"integer"}},
		{"name":"mtu__empty","in":"query","schema":{"type":"boolean"}},
		{"name":"speed","in":"query","schema":{"type":"integer"}},
		{"name":"speed__empty","in":"query","schema":{"type":"array","items":{"type":"integer","format":"int32"}}},
		{"name":"last_updated","in":"query","schema":{"type":"string","format":"date-time"}},
		{"name":"last_updated__empty","in":"query","schema":{"type":"array","items":{"type":"string","format":"date-time"}}},
		{"name":"description","in":"query","schema":{"type":"string"}},
		{"name":"description__empty","in":"query","schema":{"type":"boolean"}},
		{"name":"label","in":"query","schema":{"type":"string"}},
		{"name":"label__empty","in":"query"}]}}}}`))
	if err != nil {
		t.Fatalf("parseFilterFields: %v", err)
	}
	ops := map[string][]string{}
	for _, ff := range m["dcim/interfaces"] {
		ops[ff.Name] = ff.Operators
	}
	want := map[string][]string{
		// __empty declared boolean -> a real BooleanFilter, both directions offered.
		"mtu":         {"", "empty", "nempty"},
		"description": {"", "empty", "nempty"},
		// __empty keeps the parent field's type -> NetBox 400s, so offer neither.
		"speed":        {""},
		"last_updated": {""},
		// no declared type -> cannot tell, so do not offer.
		"label": {""},
	}
	for name, w := range want {
		if !equalStrs(ops[name], w) {
			t.Errorf("%s operators = %v, want %v", name, ops[name], w)
		}
	}
	// The suppressed fields must survive as fields — only the empty family goes.
	for _, name := range []string{"speed", "last_updated", "label"} {
		if _, ok := ops[name]; !ok {
			t.Errorf("%s should still be offered as a filter field", name)
		}
	}
}

// TestParseFilterFields_OddParamSurvivesAsOneParam pins the blast radius of a
// query parameter we cannot fully read. /api/schema/ is ~10 MB with >12k GET
// query params; a strict decoder fails the WHOLE json.Unmarshal on the first
// odd one, and parseFilterFields then returns zero object types — every object
// type loses operator narrowing and falls back to the full FILTER_OPERATORS
// list (re-offering the broken empty/nempty this file suppresses), FieldValues
// loses its dimension index, and Provider.schema() re-fetches 10 MB per editor
// call because it caches only on success.
//
// The odd shapes here are an OpenAPI 3.1 union type (`["string","null"]`) and a
// JSON Schema boolean in place of the schema object. Neither is present on the
// demo (4.4.10 is openapi 3.0.3: 12780 GET query params, 0 non-string types),
// so this is defensive against 3.1 documents and plugin-injected schemas.
func TestParseFilterFields_OddParamSurvivesAsOneParam(t *testing.T) {
	m, err := parseFilterFields([]byte(`{"paths":{
		"/api/dcim/devices/":{"get":{"parameters":[
			{"name":"name","in":"query","schema":{"type":["string","null"]}},
			{"name":"name__ic","in":"query","schema":{"type":"string"}},
			{"name":"serial","in":"query","schema":true},
			{"name":123,"in":"query","schema":{"type":"string"}}]}},
		"/api/ipam/prefixes/":{"get":{"parameters":[
			{"name":"status","in":"query","schema":{"type":"string"}},
			{"name":"status__n","in":"query","schema":{"type":"string"}},
			{"name":"status__empty","in":"query","schema":{"type":"boolean"}}]}}}}`))
	if err != nil {
		t.Fatalf("parseFilterFields: %v", err)
	}
	// An unrelated object type keeps every operator it earned.
	var status []string
	for _, ff := range m["ipam/prefixes"] {
		if ff.Name == "status" {
			status = ff.Operators
		}
	}
	if want := []string{"", "n", "empty", "nempty"}; !equalStrs(status, want) {
		t.Errorf("ipam/prefixes status operators = %v, want %v", status, want)
	}
	// The odd params themselves stay filterable — only their declared type is
	// unknown, and type is read for the __empty gate alone.
	ops := map[string][]string{}
	for _, ff := range m["dcim/devices"] {
		ops[ff.Name] = ff.Operators
	}
	if want := []string{"", "ic"}; !equalStrs(ops["name"], want) {
		t.Errorf("dcim/devices name operators = %v, want %v (union-typed param must stay filterable)", ops["name"], want)
	}
	if want := []string{""}; !equalStrs(ops["serial"], want) {
		t.Errorf("dcim/devices serial operators = %v, want %v (non-object schema must stay filterable)", ops["serial"], want)
	}
	// A parameter that is not readable at all (non-string name) costs itself and
	// nothing else: it is dropped, and produces no junk field.
	var names []string
	for _, ff := range m["dcim/devices"] {
		names = append(names, ff.Name)
	}
	sort.Strings(names)
	if want := []string{"name", "serial"}; !equalStrs(names, want) {
		t.Errorf("dcim/devices field names = %v, want %v", names, want)
	}
}

// TestParseFilterFields_UnionTypedEmptyNotOffered holds the item-3 rule at the
// exact line: empty/nempty is offered only when the __empty parameter's own
// type is the string "boolean". A union type is not "boolean" — NetBox may or
// may not accept the param, and an operator the user can pick and NetBox 400s
// is worse than one absent from the dropdown.
func TestParseFilterFields_UnionTypedEmptyNotOffered(t *testing.T) {
	m, err := parseFilterFields([]byte(`{"paths":{"/api/dcim/devices/":{"get":{"parameters":[
		{"name":"serial","in":"query","schema":{"type":"string"}},
		{"name":"serial__empty","in":"query","schema":{"type":["boolean","null"]}},
		{"name":"asset_tag","in":"query","schema":{"type":"string"}},
		{"name":"asset_tag__empty","in":"query","schema":{"type":{"$ref":"#/components/schemas/Whatever"}}},
		{"name":"description","in":"query","schema":{"type":"string"}},
		{"name":"description__empty","in":"query","schema":{"type":"boolean"}}]}}}}`))
	if err != nil {
		t.Fatalf("parseFilterFields: %v", err)
	}
	ops := map[string][]string{}
	for _, ff := range m["dcim/devices"] {
		ops[ff.Name] = ff.Operators
	}
	want := map[string][]string{
		"serial":    {""},
		"asset_tag": {""},
		// control: a plain "boolean" still earns both directions.
		"description": {"", "empty", "nempty"},
	}
	for name, w := range want {
		if !equalStrs(ops[name], w) {
			t.Errorf("%s operators = %v, want %v", name, ops[name], w)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
