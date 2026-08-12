package netbox

import (
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Pins the existing contract: a filter key is the field, or field__operator, and
// CSV values become repeated params (which NetBox ORs).
func TestBuildFilterValues_KeysAndCSV(t *testing.T) {
	got := buildFilterValues([]provider.Filter{
		{Field: "site", Operator: "", Value: "dc1,dc2"},
		{Field: "name", Operator: "ic", Value: "spine"},
	})
	if vs := got["site"]; len(vs) != 2 || vs[0] != "dc1" || vs[1] != "dc2" {
		t.Errorf("site = %v, want [dc1 dc2] (repeated params = OR)", vs)
	}
	if v := got.Get("name__ic"); v != "spine" {
		t.Errorf("name__ic = %q, want spine", v)
	}
}

func TestBuildFilterValues_EmptyFamily(t *testing.T) {
	got := buildFilterValues([]provider.Filter{
		// A stray value is deliberate on both rows: the editor hides the value
		// control for empty-family operators but does not clear an already-saved
		// value, so the backend must ignore it. The value also makes the
		// "no leaked key" guard below load-bearing — without it, the pre-fix code
		// skips the row entirely (Split("", ",") yields only "") and the guard
		// would pass vacuously.
		{Field: "name", Operator: "empty", Value: "ignored"},
		{Field: "serial", Operator: "nempty", Value: "ignored"},
	})
	if v := got.Get("name__empty"); v != "true" {
		t.Errorf("name__empty = %q, want true", v)
	}
	if v := got.Get("serial__empty"); v != "false" {
		t.Errorf("serial__empty = %q, want false", v)
	}
	// The operator must not leak into the key as a suffix of its own.
	if _, bad := got["serial__nempty"]; bad {
		t.Error("serial__nempty must not be emitted; nempty maps to __empty=false")
	}
}
