package netbox

import (
	"encoding/json"
	"testing"
)

func TestOrderedKeys(t *testing.T) {
	raw := []byte(`{"id":1,"name":"leaf1","site":{"id":2,"name":"dc1"},"tags":["a","b"]}`)
	keys, err := orderedKeys(raw)
	if err != nil {
		t.Fatalf("orderedKeys: %v", err)
	}
	want := []string{"id", "name", "site", "tags"}
	if len(keys) != len(want) {
		t.Fatalf("got %v want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("key %d = %q want %q (order not preserved)", i, keys[i], want[i])
		}
	}
}

func TestFlattenObject_Device(t *testing.T) {
	raw := json.RawMessage(`{
		"id": 27,
		"name": "leaf1",
		"display_url": "http://nb/dcim/devices/27/",
		"site": {"id": 2, "name": "DM-Akron", "slug": "dm-akron"},
		"status": {"value": "active", "label": "Active"},
		"primary_ip": null,
		"interface_count": 48,
		"tags": [{"name": "core", "slug": "core"}, {"name": "edge", "slug": "edge"}],
		"custom_fields": {"owner_team": "neteng"}
	}`)

	cols, vals, err := flattenObject(raw)
	if err != nil {
		t.Fatalf("flattenObject: %v", err)
	}

	// Order preserved, id first.
	if cols[0] != "id" || cols[1] != "name" {
		t.Fatalf("unexpected column order: %v", cols)
	}

	checks := map[string]interface{}{
		"name":            "leaf1",
		"site":            "DM-Akron",
		"site_id":         float64(2),
		"site_slug":       "dm-akron",
		"status":          "Active",
		"status_value":    "active",
		"interface_count": float64(48),
		"tags":            "core; edge",
		"tags_count":      float64(2),
		"cf_owner_team":   "neteng",
	}
	for k, want := range checks {
		if got := vals[k]; got != want {
			t.Errorf("vals[%q] = %#v, want %#v", k, got, want)
		}
	}
	if vals["primary_ip"] != nil {
		t.Errorf("primary_ip should be nil, got %#v", vals["primary_ip"])
	}
}

func TestFlattenObject_IPAddress(t *testing.T) {
	raw := json.RawMessage(`{"id":5,"address":"10.0.0.1/24","assigned_object":{"id":9,"display":"leaf1 eth0"}}`)
	_, vals, err := flattenObject(raw)
	if err != nil {
		t.Fatalf("flattenObject: %v", err)
	}
	if vals["address"] != "10.0.0.1/24" {
		t.Errorf("address = %#v", vals["address"])
	}
	if vals["assigned_object"] != "leaf1 eth0" {
		t.Errorf("assigned_object = %#v", vals["assigned_object"])
	}
}

func TestNestedDisplay(t *testing.T) {
	cases := []struct {
		m    map[string]interface{}
		want string
	}{
		{map[string]interface{}{"display": "X", "name": "Y"}, "X"},
		{map[string]interface{}{"name": "Y"}, "Y"},
		{map[string]interface{}{"label": "Active", "value": "active"}, "Active"},
		{map[string]interface{}{"address": "1.2.3.4/32"}, "1.2.3.4/32"},
	}
	for _, c := range cases {
		if got := nestedDisplay(c.m); got != c.want {
			t.Errorf("nestedDisplay(%v) = %q want %q", c.m, got, c.want)
		}
	}
}

func TestAuthHeader(t *testing.T) {
	cases := map[string]string{
		"nbt_abc.def": "Bearer nbt_abc.def",
		"0123456789abcdef0123456789abcdef01234567": "Token 0123456789abcdef0123456789abcdef01234567",
		"Bearer already": "Bearer already",
		"Token already":  "Token already",
	}
	for in, want := range cases {
		if got := authHeader(in); got != want {
			t.Errorf("authHeader(%q) = %q want %q", in, got, want)
		}
	}
}
