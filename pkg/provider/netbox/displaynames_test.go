package netbox

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// roleSchemaFixture is dimSchemaFixture plus a device role (role ->
// BriefDeviceRole -> dcim/device-roles), an integer that is not a relation
// (mtu), and an IP address with NetBox's generic assigned_object relation.
var roleSchemaFixture = strings.NewReplacer(
	`"/api/dcim/sites/":   {"get": {`,
	`"/api/dcim/device-roles/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedDeviceRoleList"}}}}}}},
  "/api/ipam/ip-addresses/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedIPAddressList"}}}}}}},
  "/api/dcim/sites/":   {"get": {`,
	`"PaginatedSiteList":`,
	`"PaginatedDeviceRoleList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/DeviceRole"}}}},
  "PaginatedIPAddressList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/IPAddress"}}}},
  "PaginatedSiteList":`,
	`"name":   {"type": "string"},`,
	`"name":   {"type": "string"},
    "role":   {"$ref": "#/components/schemas/BriefDeviceRole"},
    "mtu":    {"type": "integer"},`,
	`"Site":   {"properties"`,
	`"DeviceRole": {"properties": {"name": {"type": "string"}}},
  "IPAddress": {"properties": {
    "assigned_object_type": {"type": "string", "nullable": true},
    "assigned_object_id":   {"type": "integer", "nullable": true},
    "assigned_object":      {"type": "object", "additionalProperties": {}, "nullable": true, "readOnly": true}
  }},
  "Site":   {"properties"`,
).Replace(dimSchemaFixture)

// nameServer is a NetBox 4.4-shaped instance for diff name resolution.
func nameServer() *typeServer {
	return &typeServer{
		objectTypes: []string{
			objectTypeRow(12, "dcim.device", "/api/dcim/devices/"),
			objectTypeRow(95, "ipam.ipaddress", "/api/ipam/ip-addresses/"),
		},
		schema: roleSchemaFixture,
		objects: map[string]map[int64]string{
			"dcim/device-roles": {3: "Router", 7: "Distribution Switch"},
			"dcim/sites":        {1: "AMS1"},
		},
	}
}

var changeTime = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// updateRow is one update change-log row, n minutes older than changeTime.
func updateRow(t *testing.T, ct string, n int, pre, post string) decodedChange {
	t.Helper()
	return mustDecode(t, fmt.Sprintf(
		`{"id":%d,"time":%q,"user_name":"admin","action":{"value":"update","label":"Updated"},"changed_object_type":%q,"changed_object_id":1,"object_repr":"sw1","prechange_data":%s,"postchange_data":%s}`,
		1000-n, changeTime.Add(-time.Duration(n)*time.Minute).Format(time.RFC3339), ct, pre, post))
}

func diffField(t *testing.T, c provider.Change, field string) provider.FieldChange {
	t.Helper()
	for _, f := range c.Diff {
		if f.Field == field {
			return f
		}
	}
	t.Fatalf("no %q in diff %+v", field, c.Diff)
	return provider.FieldChange{}
}

func diffFields(c provider.Change) []string {
	out := make([]string, 0, len(c.Diff))
	for _, f := range c.Diff {
		out = append(out, f.Field)
	}
	return out
}

func TestResolveDiffs_ReplacesRelatedIDs(t *testing.T) {
	s := nameServer()
	p := s.start(t)

	changes := p.resolveDiffs(context.Background(), []decodedChange{
		updateRow(t, "dcim.device", 0, `{"role":3,"site":1,"description":"a"}`, `{"role":7,"site":null,"description":"b"}`),
		// 99 is not on the server (a deleted role): it keeps its id.
		updateRow(t, "dcim.device", 1, `{"role":7}`, `{"role":99}`),
	}, nil)

	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(changes))
	}
	if got := diffField(t, changes[0], "role"); got.Before != "Router" || got.After != "Distribution Switch" {
		t.Errorf("role = %q → %q, want Router → Distribution Switch", got.Before, got.After)
	}
	if got := diffField(t, changes[0], "site"); got.Before != "AMS1" || got.After != "" {
		t.Errorf("site = %q → %q, want AMS1 → (empty)", got.Before, got.After)
	}
	if got := diffField(t, changes[0], "description"); got.Before != "a" || got.After != "b" {
		t.Errorf("description = %q → %q, want a → b", got.Before, got.After)
	}
	if got := diffField(t, changes[1], "role"); got.Before != "Distribution Switch" || got.After != "99" {
		t.Errorf("role = %q → %q, want Distribution Switch → 99", got.Before, got.After)
	}

	// One batched brief lookup per related endpoint, never one per row.
	roles := s.queries("/api/dcim/device-roles/")
	if len(roles) != 1 {
		t.Fatalf("device-roles read %d times, want 1", len(roles))
	}
	if roles[0].Get("brief") != "true" {
		t.Errorf("device-roles lookup brief = %q, want true", roles[0].Get("brief"))
	}
	if got := sortedInts(t, roles[0]["id"]); !reflect.DeepEqual(got, []int64{3, 7, 99}) {
		t.Errorf("device-roles ids = %v, want [3 7 99]", got)
	}
	if roles[0].Get("limit") != "3" {
		t.Errorf("device-roles limit = %q, want 3", roles[0].Get("limit"))
	}
	if n := s.hits("/api/dcim/sites/"); n != 1 {
		t.Errorf("sites read %d times, want 1", n)
	}
}

func TestResolveDiffs_LookupFailureKeepsIDs(t *testing.T) {
	s := nameServer()
	s.failing = map[string]int{"dcim/device-roles": http.StatusInternalServerError}
	p := s.start(t)

	changes := p.resolveDiffs(context.Background(), []decodedChange{
		updateRow(t, "dcim.device", 0, `{"role":3,"site":1}`, `{"role":7,"site":null}`),
	}, nil)

	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1: a failed lookup must not drop the change", len(changes))
	}
	if got := diffField(t, changes[0], "role"); got.Before != "3" || got.After != "7" {
		t.Errorf("role = %q → %q, want the ids 3 → 7", got.Before, got.After)
	}
	// The other endpoint's lookup is unaffected.
	if got := diffField(t, changes[0], "site"); got.Before != "AMS1" {
		t.Errorf("site before = %q, want AMS1", got.Before)
	}
}

// manyRelationsSchema is a devices list whose item has n foreign keys f01..fNN,
// each to its own list endpoint test/relNN.
func manyRelationsSchema(n int) string {
	paths := []string{`"/api/dcim/devices/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedDeviceList"}}}}}}}`}
	schemas := []string{`"PaginatedDeviceList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/Device"}}}}`}
	var props []string
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("Rel%02d", i)
		paths = append(paths, fmt.Sprintf(`"/api/test/rel%02d/": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Paginated%sList"}}}}}}}`, i, name))
		schemas = append(schemas,
			fmt.Sprintf(`"Paginated%sList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/%s"}}}}`, name, name),
			fmt.Sprintf(`%q: {"properties": {"name": {"type": "string"}}}`, name))
		props = append(props, fmt.Sprintf(`"f%02d": {"$ref": "#/components/schemas/Brief%s"}`, i, name))
	}
	schemas = append(schemas, `"Device": {"properties": {`+strings.Join(props, ",")+`}}`)
	return `{"paths": {` + strings.Join(paths, ",") + `}, "components": {"schemas": {` + strings.Join(schemas, ",") + `}}}`
}

func TestResolveDiffs_AtMostTenEndpoints(t *testing.T) {
	s := nameServer()
	s.schema = manyRelationsSchema(12)
	s.objects = map[string]map[int64]string{}
	for i := 1; i <= 12; i++ {
		s.objects[fmt.Sprintf("test/rel%02d", i)] = map[int64]string{1: fmt.Sprintf("R%02d-old", i), 2: fmt.Sprintf("R%02d-new", i)}
	}
	p := s.start(t)

	// Five rendered fields per row, so twelve endpoints take three rows. They
	// appear in field order, newest row first.
	fields := func(from, to int) (string, string) {
		var pre, post []string
		for i := from; i <= to; i++ {
			pre = append(pre, fmt.Sprintf(`"f%02d":1`, i))
			post = append(post, fmt.Sprintf(`"f%02d":2`, i))
		}
		return "{" + strings.Join(pre, ",") + "}", "{" + strings.Join(post, ",") + "}"
	}
	pre1, post1 := fields(1, 5)
	pre2, post2 := fields(6, 10)
	pre3, post3 := fields(11, 12)
	changes := p.resolveDiffs(context.Background(), []decodedChange{
		updateRow(t, "dcim.device", 0, pre1, post1),
		updateRow(t, "dcim.device", 1, pre2, post2),
		updateRow(t, "dcim.device", 2, pre3, post3),
	}, nil)

	for i := 1; i <= 10; i++ {
		path := fmt.Sprintf("/api/test/rel%02d/", i)
		if n := s.hits(path); n != 1 {
			t.Errorf("%s read %d times, want 1", path, n)
		}
	}
	for i := 11; i <= 12; i++ {
		path := fmt.Sprintf("/api/test/rel%02d/", i)
		if n := s.hits(path); n != 0 {
			t.Errorf("%s read %d times, want 0: past the ten-endpoint budget", path, n)
		}
	}
	if got := diffField(t, changes[0], "f01"); got.Before != "R01-old" || got.After != "R01-new" {
		t.Errorf("f01 = %q → %q, want R01-old → R01-new", got.Before, got.After)
	}
	if got := diffField(t, changes[2], "f12"); got.Before != "1" || got.After != "2" {
		t.Errorf("f12 = %q → %q, want the ids 1 → 2", got.Before, got.After)
	}
}

func TestResolveDiffs_AtMostHundredIDsPerEndpoint(t *testing.T) {
	s := nameServer()
	roles := map[int64]string{}
	for id := int64(1); id <= 120; id++ {
		roles[id] = fmt.Sprintf("role%d", id)
	}
	s.objects["dcim/device-roles"] = roles
	p := s.start(t)

	// 60 rows, newest first, each changing role 2i+1 → 2i+2: 120 distinct ids.
	var rows []decodedChange
	for i := 0; i < 60; i++ {
		rows = append(rows, updateRow(t, "dcim.device", i, fmt.Sprintf(`{"role":%d}`, 2*i+1), fmt.Sprintf(`{"role":%d}`, 2*i+2)))
	}
	changes := p.resolveDiffs(context.Background(), rows, nil)

	q := s.queries("/api/dcim/device-roles/")
	if len(q) != 1 {
		t.Fatalf("device-roles read %d times, want 1", len(q))
	}
	got := sortedInts(t, q[0]["id"])
	if len(got) != 100 || got[0] != 1 || got[99] != 100 {
		t.Errorf("device-roles ids = %d ids from %v, want the 100 ids of the newest 50 rows (1..100)", len(got), got)
	}
	if q[0].Get("limit") != "100" {
		t.Errorf("limit = %q, want 100", q[0].Get("limit"))
	}
	if f := diffField(t, changes[49], "role"); f.Before != "role99" || f.After != "role100" {
		t.Errorf("row 49 role = %q → %q, want role99 → role100", f.Before, f.After)
	}
	if f := diffField(t, changes[50], "role"); f.Before != "101" || f.After != "102" {
		t.Errorf("row 50 role = %q → %q, want the ids 101 → 102", f.Before, f.After)
	}
}

// The unscoped default: rows with no diff, or whose rendered fields hold no id,
// cost no request at all — not even the object-type or schema read.
func TestResolveDiffs_NoRelationsNoRequests(t *testing.T) {
	s := nameServer()
	p := s.start(t)

	changes := p.resolveDiffs(context.Background(), []decodedChange{
		mustDecode(t, changeRecord("dcim.device", "leaf1", 0)),
		mustDecode(t, `{"action":{"value":"create","label":"Created"},"changed_object_type":"dcim.device","postchange_data":{"name":"leaf2","role":3}}`),
		mustDecode(t, `{"action":{"value":"delete","label":"Deleted"},"changed_object_type":"dcim.device","prechange_data":{"name":"leaf3","role":3}}`),
		updateRow(t, "dcim.device", 3,
			`{"description":"a","status":"active","tags":["core"],"custom_fields":{"rack_units":1}}`,
			`{"description":"b","status":"offline","tags":["core","edge"],"custom_fields":{"rack_units":2}}`),
	}, nil)

	if len(changes) != 4 {
		t.Fatalf("changes = %d, want 4", len(changes))
	}
	if got := s.allRequests(); len(got) != 0 {
		t.Errorf("requests = %v, want none: nothing rendered can be a relation", got)
	}
	if got := diffField(t, changes[3], "cf_rack_units"); got.Before != "1" || got.After != "2" {
		t.Errorf("cf_rack_units = %q → %q, want 1 → 2", got.Before, got.After)
	}
}

// An integer that is not a relation, and NetBox's generic relations, keep their
// values and are never looked up.
func TestResolveDiffs_NonRelationsStayAsTheyAre(t *testing.T) {
	s := nameServer()
	p := s.start(t)

	changes := p.resolveDiffs(context.Background(), []decodedChange{
		updateRow(t, "dcim.device", 0, `{"mtu":1500}`, `{"mtu":9000}`),
		updateRow(t, "ipam.ipaddress", 1, `{"assigned_object_type":30,"assigned_object_id":5}`, `{"assigned_object_type":30,"assigned_object_id":6}`),
	}, nil)

	for _, req := range s.allRequests() {
		path, _, _ := strings.Cut(req, "?")
		if path != "/api/core/object-types/" && path != "/api/schema/" {
			t.Errorf("unexpected request %s: no rendered field is a relation", req)
		}
	}
	if got := diffField(t, changes[0], "mtu"); got.Before != "1500" || got.After != "9000" {
		t.Errorf("mtu = %q → %q, want 1500 → 9000", got.Before, got.After)
	}
	if got := diffField(t, changes[1], "assigned_object_id"); got.Before != "5" || got.After != "6" {
		t.Errorf("assigned_object_id = %q → %q, want 5 → 6", got.Before, got.After)
	}
}

func TestResolveDiffs_PreferredFieldsFirst(t *testing.T) {
	s := nameServer()
	p := s.start(t)
	row := updateRow(t, "dcim.device", 0,
		`{"asset_tag":"a","comments":"a","description":"a","name":"a","serial":"a","status":"active","custom_fields":{"owner":"a","site_code":"a"}}`,
		`{"asset_tag":"b","comments":"b","description":"b","name":"b","serial":"b","status":"offline","custom_fields":{"owner":"b","site_code":"b"}}`)

	cases := []struct {
		name      string
		preferred []string
		want      []string
	}{
		{"none: alphabetical", nil,
			[]string{"asset_tag", "cf_owner", "cf_site_code", "comments", "description", "name", "serial", "status"}},
		{"a field and a custom field by its name", []string{"status", "owner"},
			[]string{"cf_owner", "status", "asset_tag", "cf_site_code", "comments", "description", "name", "serial"}},
		{"a custom field by cf_ name", []string{"cf_site_code"},
			[]string{"cf_site_code", "asset_tag", "cf_owner", "comments", "description", "name", "serial", "status"}},
		{"custom_fields matches every custom field", []string{"custom_fields", "serial"},
			[]string{"cf_owner", "cf_site_code", "serial", "asset_tag", "comments", "description", "name", "status"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changes := p.resolveDiffs(context.Background(), []decodedChange{row}, c.preferred)
			if got := diffFields(changes[0]); !reflect.DeepEqual(got, c.want) {
				t.Errorf("diff order = %v, want %v", got, c.want)
			}
		})
	}
	if got := row.diff[0].Field; got != "asset_tag" {
		t.Errorf("the caller's row was reordered (first field %q)", got)
	}
}

// Only the fields a marker renders get names, so a relation pushed past the
// fifth line by the preferred fields costs no lookup.
func TestResolveDiffs_OnlyRenderedFieldsAreLookedUp(t *testing.T) {
	s := nameServer()
	p := s.start(t)
	row := updateRow(t, "dcim.device", 0,
		`{"asset_tag":"a","comments":"a","description":"a","name":"a","role":3,"serial":"a","status":"active"}`,
		`{"asset_tag":"b","comments":"b","description":"b","name":"b","role":7,"serial":"b","status":"offline"}`)

	changes := p.resolveDiffs(context.Background(), []decodedChange{row}, []string{"status", "serial"})
	if got := diffField(t, changes[0], "role"); got.Before != "3" || got.After != "7" {
		t.Errorf("role (7th) = %q → %q, want the ids 3 → 7", got.Before, got.After)
	}
	if n := s.hits("/api/dcim/device-roles/"); n != 0 {
		t.Errorf("device-roles read %d times for a field that does not render, want 0", n)
	}

	changes = p.resolveDiffs(context.Background(), []decodedChange{row}, nil)
	if got := diffField(t, changes[0], "role"); got.Before != "Router" || got.After != "Distribution Switch" {
		t.Errorf("role (5th) = %q → %q, want Router → Distribution Switch", got.Before, got.After)
	}
}

func TestChanges_DiffShowsRelatedNames(t *testing.T) {
	s := nameServer()
	s.changes = []string{
		`{"id":1,"time":"2026-08-13T12:00:00Z","user_name":"admin","action":{"value":"update","label":"Updated"},"changed_object_type":"dcim.device","changed_object_id":1,"object_repr":"sw1","prechange_data":{"role":3},"postchange_data":{"role":7}}`,
	}
	p := s.start(t)

	changes, err := p.Changes(context.Background(), provider.ChangeSpec{Limit: 10})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if got := diffField(t, changes[0], "role"); got.Before != "Router" || got.After != "Distribution Switch" {
		t.Errorf("role = %q → %q, want Router → Distribution Switch", got.Before, got.After)
	}
}

// sortedInts parses and sorts repeated integer query values.
func sortedInts(t *testing.T, vals []string) []int64 {
	t.Helper()
	out := make([]int64, 0, len(vals))
	for _, v := range vals {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("not an id: %q", v)
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
