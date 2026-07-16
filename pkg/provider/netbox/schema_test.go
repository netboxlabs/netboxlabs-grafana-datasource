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
          {"name":"status__empty","in":"query"},
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
		// order follows operatorOrder: "", n, ic, nic, isw, regex, iregex, empty.
		"status":      {"", "n", "ic", "nic", "isw", "regex", "iregex", "empty"},
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
