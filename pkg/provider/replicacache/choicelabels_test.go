package replicacache

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// NetBox mode shows a choice by its label ("Active") and keeps the raw value
// in <field>_value. The replica stores only the value, so the same panel read
// "active" in cache mode and "Active" in NetBox mode. Labels come from NetBox's
// own API schema (choicelabels.json); a value the map does not know stays as
// stored rather than being guessed at.
func TestQuery_ChoiceColumnsCarryNetBoxLabels(t *testing.T) {
	f := newFakeService()
	retired := deviceFixture(2, "CORE-2", 4001)
	retired["status"] = "decommissioning"
	odd := deviceFixture(3, "CORE-3", 4001)
	odd["status"] = "not-a-netbox-status"
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001), retired, odd}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", Fields: []string{"name", "status", "status_value"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := []struct{ status, value string }{
		{"Active", "active"},
		{"Decommissioning", "decommissioning"},
		{"not-a-netbox-status", "not-a-netbox-status"},
	}
	for i, w := range want {
		if got := res.Rows[i]["status"]; got != w.status {
			t.Errorf("row %d status = %v, want the label %q", i, got, w.status)
		}
		if got := res.Rows[i]["status_value"]; got != w.value {
			t.Errorf("row %d status_value = %v, want the raw value %q", i, got, w.value)
		}
	}

	// The label is for reading. A filter still names the stored value, as
	// NetBox's own filters do.
	if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{{Field: "status", Operator: "", Value: "active"}}}); err != nil {
		t.Fatalf("Query with a filter: %v", err)
	}
	req, ok := f.requestWith("dcim/devices", "filter[status]__eq")
	if !ok || req.query.Get("filter[status]__eq") != "active" {
		t.Errorf("filter sent %v, want filter[status]__eq=active", req.query)
	}
}

// Some choices are numbers: a prefix's family is 4 or 6, shown in NetBox mode
// as "IPv4" / "IPv6" with the number in family_value. A number the map does
// not know is written as text too, so the column keeps one type whatever a
// refresh returns, while family_value stays numeric, as in NetBox mode.
func TestQuery_NumericChoiceColumnsCarryLabels(t *testing.T) {
	f := newFakeService()
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR", "family:INTEGER")
	f.entities["ipam/prefixes"] = []map[string]interface{}{
		{"id": 1, "prefix": "10.0.0.0/8", "family": 4},
		{"id": 2, "prefix": "2001:db8::/32", "family": 6},
		{"id": 3, "prefix": "odd", "family": 8},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "ipam/prefixes", Fields: []string{"prefix", "family", "family_value"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := []struct {
		family string
		value  float64
	}{{"IPv4", 4}, {"IPv6", 6}, {"8", 8}}
	for i, w := range want {
		if got := res.Rows[i]["family"]; got != w.family {
			t.Errorf("row %d family = %#v, want %q", i, got, w.family)
		}
		if got := res.Rows[i]["family_value"]; got != w.value {
			t.Errorf("row %d family_value = %#v, want the number %v", i, got, w.value)
		}
	}
}

// The embedded map is generated data; this pins that it loaded and holds what
// the rest of the provider relies on.
func TestChoiceLabels_EmbeddedMapCoversCommonChoices(t *testing.T) {
	cases := []struct{ objectType, column, value, label string }{
		{"dcim/devices", "status", "active", "Active"},
		{"dcim/devices", "status", "offline", "Offline"},
		{"dcim/interfaces", "type", "1000base-t", "1000BASE-T (1GE)"},
		{"ipam/prefixes", "status", "container", "Container"},
	}
	for _, c := range cases {
		if got, ok := choiceLabel(c.objectType, c.column, c.value); !ok || got != c.label {
			t.Errorf("choiceLabel(%s, %s, %s) = %q, %v; want %q", c.objectType, c.column, c.value, got, ok, c.label)
		}
	}
	if _, ok := choiceLabel("dcim/devices", "name", "anything"); ok {
		t.Error("a column that is not a choice has no labels")
	}
}

// "All columns" in NetBox mode returns both halves of a choice: status (the
// label) and status_value (the stored value). Labelling the column here must
// not lose the stored value from such a query.
func TestQuery_AllColumnsKeepsTheStoredChoiceValue(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["status"]; got != "Active" {
		t.Errorf("status = %v, want the label", got)
	}
	if got := res.Rows[0]["status_value"]; got != "active" {
		t.Errorf("status_value = %v, want the stored value", got)
	}
	if !slices.Contains(res.Columns, "status_value") {
		t.Errorf("Columns = %v, want status_value announced", res.Columns)
	}
	// Only choice columns get one: name is not a choice.
	if slices.Contains(res.Columns, "name_value") {
		t.Errorf("Columns = %v: name is not a choice and has no _value", res.Columns)
	}
}

// Once rows read "Active", a value picked from them is a label. NetBox refuses
// status=Active ("Select a valid choice"); sent here it would match nothing,
// and as a negation it would match everything, with no error either way. So a
// known label that is not also a stored value is refused, naming the value.
func TestQuery_ALabelAsAFilterValueIsRefused(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{{Field: "status", Value: "planned,Active"}}})
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want an UnsupportedFilterError", err)
	}
	if !strings.Contains(unsupported.Reason, `"active"`) || !strings.Contains(unsupported.Reason, `"Active"`) {
		t.Errorf("reason = %q, want it to name the stored value for the label", unsupported.Reason)
	}

	// A value outside NetBox's built-in list (FIELD_CHOICES) is not a label
	// the map knows, so it is sent as given.
	if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Filters: []provider.Filter{{Field: "status", Value: "quarantined"}}}); err != nil {
		t.Errorf("a value the map does not know was refused: %v", err)
	}
}
