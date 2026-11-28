package provider

import "testing"

// Both modes and the plugin read a nested column's name this one way.
func TestNestedColumn(t *testing.T) {
	for name, want := range map[string][3]string{
		"device_type__manufacturer":       {"device_type", "manufacturer", "ok"},
		"device_type__manufacturer__name": {"device_type", "manufacturer__name", "ok"},
		"site":                            {"", "", ""},
		"__x":                             {"", "", ""},
		"x__":                             {"", "", ""},
		"cf__private":                     {"", "", ""},
		"cf_tier__x":                      {"", "", ""},
	} {
		ref, col, ok := NestedColumn(name)
		if ref != want[0] || col != want[1] || ok != (want[2] == "ok") {
			t.Errorf("%s: %q %q %v, want %q %q %v", name, ref, col, ok, want[0], want[1], want[2] == "ok")
		}
	}
}
