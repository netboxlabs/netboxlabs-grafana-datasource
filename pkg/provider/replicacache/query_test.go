package replicacache

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// deviceFixture is a row shaped like a real one: flat foreign keys and custom
// fields encoded as a JSON string.
func deviceFixture(id int, name string, siteID int) map[string]interface{} {
	return map[string]interface{}{
		"id": float64(id), "name": name, "status": "active",
		"site_id": float64(siteID), "role_id": float64(5), "tenant_id": nil,
		"custom_field_data": `{"lifecycle_phase":"production","auto_remediation":false}`,
	}
}

func newTestProvider(t *testing.T, f *fakeService) *Provider {
	t.Helper()
	srv := f.start(t)
	return New(srv.URL, "test-token", "nb-test", srv.Client())
}

// A panel written against the NetBox provider selects "site", not "site_id".
// The server joins the name in under expand=; nothing is read client-side.
// Only what is asked for: an unprojected query returns the stored columns,
// because expanding every reference on a large table is what made the
// default query time out (dcim/interfaces: one reference alone took 62 s for
// 100 rows on a large replica, and all eight answered 503).
func TestQueryExpandsForeignKeysToNames(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	f.entities["dcim/device-roles"] = []map[string]interface{}{
		{"id": float64(5), "name": "Core Switch", "slug": "core-switch"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if req, ok := f.requestWith("dcim/devices", "expand"); ok {
		t.Errorf("an unprojected query expands nothing, got %v", req.query)
	}
	if _, ok := res.Rows[0]["site"]; ok || res.Rows[0]["site_id"] != float64(4001) {
		t.Errorf("unprojected row = %v, want the stored columns alone", res.Rows[0])
	}

	res, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "site", "site_slug", "role", "site_id"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(res.Rows))
	}
	row := res.Rows[0]

	if req, ok := f.requestWith("dcim/devices", "expand"); !ok || req.query.Get("expand") != "role,site" {
		t.Errorf("the requested names are expanded, in catalogue order, got %v", req.query)
	}
	if n := f.countRequestsFor("dcim/sites"); n != 0 {
		t.Errorf("the server resolves the names; %d dimension reads were made", n)
	}
	if got := row["site"]; got != "DC-Northeast" {
		t.Errorf("site = %v, want DC-Northeast", got)
	}
	if got := row["site_slug"]; got != "dc-northeast" {
		t.Errorf("site_slug = %v, want dc-northeast", got)
	}
	if got := row["role"]; got != "Core Switch" {
		t.Errorf("role = %v, want Core Switch (a device role, not an ipam role)", got)
	}
	// The raw id must survive: it is what join keys and deep links use.
	if got := row["site_id"]; got != float64(4001) {
		t.Errorf("site_id = %v, want 4001", got)
	}
}

func TestQueryExpandsCustomFields(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	row := res.Rows[0]
	if got := row["cf_lifecycle_phase"]; got != "production" {
		t.Errorf("cf_lifecycle_phase = %v, want production", got)
	}
	if got, ok := row["cf_auto_remediation"]; !ok || got != false {
		t.Errorf("cf_auto_remediation = %v (present=%v), want false", got, ok)
	}
	// The blob itself is not a column; it was replaced by the cf_ columns.
	if _, ok := row["custom_field_data"]; ok {
		t.Error("custom_field_data should not survive as a column")
	}
}

// Total must come from the service's count, which is for the whole filter and
// independent of how many rows were returned. Alerting reads it, and deriving
// it from len(rows) would report a truncated page as the whole population.
func TestQueryTotalIsTheFilterCountNotTheRowCount(t *testing.T) {
	f := newFakeService()
	f.pageCap = 2
	for i := 1; i <= 7; i++ {
		f.entities["dcim/devices"] = append(f.entities["dcim/devices"], deviceFixture(i, "d", 1))
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Errorf("want 3 rows (the limit), got %d", len(res.Rows))
	}
	if res.Total != 7 {
		t.Errorf("Total = %d, want 7 (every matching object, not the page)", res.Total)
	}
}

// The provider reports its own ceiling on every result, which is what the
// plugin layer's truncation advice quotes.
func TestQueryReportsItsRowCeiling(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "d", 1)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.MaxRows != MaxLimit {
		t.Errorf("MaxRows = %d, want %d", res.MaxRows, MaxLimit)
	}
}

// A limit larger than one page must be satisfied by walking the cursor. The
// service silently returns a short page for an oversized limit, so a provider
// that asked once would return 2 rows and call it 6.
func TestQueryPagesToReachTheRequestedLimit(t *testing.T) {
	f := newFakeService()
	f.pageCap = 2
	for i := 1; i <= 6; i++ {
		f.entities["dcim/devices"] = append(f.entities["dcim/devices"], deviceFixture(i, "d", 1))
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Limit: 6})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 6 {
		t.Fatalf("want 6 rows across pages, got %d", len(res.Rows))
	}
	seen := map[float64]bool{}
	for _, r := range res.Rows {
		id := r["id"].(float64)
		if seen[id] {
			t.Errorf("row id %v returned twice: the cursor walk overlaps", id)
		}
		seen[id] = true
	}
}

func TestQueryPushesFiltersAndSortUpstream(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	_, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Filters:    []provider.Filter{{Field: "status", Value: "active"}},
		Ordering:   "-name",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	req, ok := f.requestWith("dcim/devices", "filter[status]__eq")
	if !ok {
		t.Fatal("no request carried the filter: it was not pushed down")
	}
	if got := req.query.Get("filter[status]__eq"); got != "active" {
		t.Errorf("filter = %q, want active", got)
	}
	if got := req.query.Get("sort"); got != "-name" {
		t.Errorf("sort = %q, want -name", got)
	}
}

// Selecting a resolved column must fetch its SOURCE id column, or the panel
// gets an empty "site" with nothing to say why.
func TestQueryProjectsResolvedColumnsOntoTheirIDs(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Rows[0]["site"] != "DC-Northeast" {
		t.Errorf("site = %v; the name was not expanded", res.Rows[0]["site"])
	}
	if len(res.Columns) != 2 || res.Columns[0] != "name" || res.Columns[1] != "site" {
		t.Errorf("Columns = %v, want the requested fields in order", res.Columns)
	}
}

// A custom field is not a column upstream; asking for it by name is an error,
// so the blob it lives in has to be requested instead.
func TestQueryProjectsCustomFieldsOntoTheBlob(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"cf_lifecycle_phase"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["cf_lifecycle_phase"]; got != "production" {
		t.Errorf("cf_lifecycle_phase = %v, want production", got)
	}
}

// KeyFields are fetched so a join key can be built, but never announced as
// columns: asking to join on something is not asking to see it.
func TestQueryFetchesKeyFieldsWithoutShowingThem(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
		KeyFields:  []string{"status"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["status"]; got != "active" {
		t.Errorf("key field not fetched: status = %v", got)
	}
	for _, c := range res.Columns {
		if c == "status" {
			t.Error("a key field must not be announced as a column")
		}
	}
}

func TestQueryCountOnly(t *testing.T) {
	f := newFakeService()
	for i := 1; i <= 5; i++ {
		f.entities["dcim/devices"] = append(f.entities["dcim/devices"], deviceFixture(i, "d", 1))
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", CountOnly: true, Limit: 5000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 5 {
		t.Errorf("Total = %d, want 5", res.Total)
	}
	req, ok := f.requestWith("dcim/devices", "limit")
	if !ok {
		t.Fatal("no request recorded")
	}
	if got := req.query.Get("limit"); got != "1" {
		t.Errorf("a count-only query fetched limit=%q; it reads only the total", got)
	}
}

// The contract's own contradiction, rejected the same way the NetBox provider
// rejects it.
func TestQueryRejectsCountOnlyWithAllowUncounted(t *testing.T) {
	p := newTestProvider(t, newFakeService())
	_, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", CountOnly: true, AllowUncounted: true,
	})
	if err == nil {
		t.Fatal("want an error for CountOnly with AllowUncounted")
	}
}

// An object type the catalogue does not list is refused before any row
// request, with the message naming how many types this deployment reports.
func TestQueryRejectsUnknownObjectType(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "d", 1)}
	p := newTestProvider(t, f)

	if _, err := p.ObjectTypes(context.Background()); err != nil {
		t.Fatalf("warming the catalogue: %v", err)
	}

	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/widgets"})
	if err == nil {
		t.Fatal("want an error for an unknown object type")
	}
	var unknown *UnknownObjectTypeError
	if !errors.As(err, &unknown) {
		t.Fatalf("want *UnknownObjectTypeError, got %T", err)
	}
	// Classified as the user's input so the plugin answers it as a bad request
	// with an actionable message, not as an outage.
	if c := unknown.Classification(); c.Kind != provider.ErrorKindUnknownObjectType {
		t.Errorf("classification = %q, want unknown-object-type", c.Kind)
	}
}

// Sorting on a related name whose target has no data cannot be pushed down —
// the service answers 400 "cannot sort on platform" — so the ordering is
// dropped and the note names the cause.
func TestQueryNotesWhenSortTargetHasNoData(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Ordering: "platform"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "dcim/platforms") {
		t.Errorf("the note should name the unfed target: %v", res.Notes)
	}
	if r, ok := f.requestWith("dcim/devices", "sort"); ok {
		t.Errorf("sort was pushed for an unfed target: %v", r.query)
	}
}

// replica-cache serves database rows, which carry no link back to the NetBox
// UI. Without synthesizing one, switching a datasource to this mode silently
// removes every "View in NetBox" link from panels that had them — the plugin
// layer keys that data link on a display_url column.
func TestQuerySynthesizesNetBoxDeepLinks(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(7, "CORE-7", 4001)}
	f.schema.NetBoxURL = "https://netbox.example.com/"
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := "https://netbox.example.com/dcim/devices/7/"
	if got := res.Rows[0]["display_url"]; got != want {
		t.Errorf("display_url = %v, want %v", got, want)
	}
	found := false
	for _, c := range res.Columns {
		if c == "display_url" {
			found = true
		}
	}
	if !found {
		t.Error("display_url must be announced as a column, or the data link is never attached")
	}
}

// When the replica reports no NetBox there is nothing to link to, and inventing one would
// produce links that 404.
func TestQueryOmitsDeepLinksWithoutANetBoxURL(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(7, "CORE-7", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if _, ok := res.Rows[0]["display_url"]; ok {
		t.Error("the replica reports no NetBox, so no link should be produced")
	}
}

// Selecting the link column must work: it is derived from the primary key, and
// naming it upstream would be rejected as an unknown field.
func TestQueryProjectsTheDeepLinkColumn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(7, "CORE-7", 4001)}
	f.schema.NetBoxURL = "https://netbox.example.com"
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "display_url"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["display_url"]; got != "https://netbox.example.com/dcim/devices/7/" {
		t.Errorf("display_url = %v", got)
	}
}

// An empty Fields means "all columns". KeyFields name join-key sources the
// caller reads but did not ask to see, and object queries populate them
// whenever a join mapping is configured — so treating them as a projection
// silently reduced an ordinary panel left at "All columns" to nothing but its
// join keys.
func TestQueryKeepsAllColumnsWhenOnlyKeyFieldsAreGiven(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		KeyFields:  []string{"name"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Every stored column should still be present, not just the join key.
	for _, want := range []string{"status", "site_id", "role_id", "id"} {
		if _, ok := res.Rows[0][want]; !ok {
			t.Errorf("column %q was dropped; an empty Fields means all columns", want)
		}
	}
	if len(res.Columns) < 5 {
		t.Errorf("only %d columns returned (%v); the row was projected away", len(res.Columns), res.Columns)
	}
	// No projection should have been requested upstream at all.
	if req, ok := f.requestWith("dcim/devices", "fields"); ok {
		t.Errorf("a projection was pushed down for a key-fields-only query: %v", req.query)
	}
}

// An explicit projection still fetches key-field sources alongside the
// requested columns, which is what KeyFields is for.
func TestQueryProjectsKeyFieldsAlongsideExplicitFields(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
		KeyFields:  []string{"status"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Rows[0]["status"] != "active" {
		t.Error("the key field's source was not fetched")
	}
	if len(res.Columns) != 1 || res.Columns[0] != "name" {
		t.Errorf("Columns = %v, want only the requested field", res.Columns)
	}
}

// End to end for the self-referential case: a panel written against the NetBox
// provider selects "parent", not "parent_id", and the resolution has to read
// the parent's name out of the SAME table it is querying.
func TestQueryResolvesSelfReferentialParent(t *testing.T) {
	f := newFakeService()
	f.addEntity("dcim/locations", "id:BIGINT:pk", "name:VARCHAR", "slug:VARCHAR", "parent_id:BIGINT")
	f.addReference("dcim/locations", "parent_id", "dcim/locations", "parent", "name", "slug")
	f.entities["dcim/locations"] = []map[string]interface{}{
		{"id": float64(10), "name": "Campus", "slug": "campus", "parent_id": nil},
		{"id": float64(11), "name": "Building A", "slug": "building-a", "parent_id": float64(10)},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/locations", Fields: []string{"id", "name", "parent", "parent_slug", "parent_id"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var child map[string]interface{}
	for _, r := range res.Rows {
		if r["id"] == float64(11) {
			child = r
		}
	}
	if child == nil {
		t.Fatal("Building A missing from the result")
	}
	if got := child["parent"]; got != "Campus" {
		t.Errorf("parent = %v, want Campus", got)
	}
	if got := child["parent_slug"]; got != "campus" {
		t.Errorf("parent_slug = %v, want campus", got)
	}
	// The raw id must survive: join keys and deep links use it.
	if got := child["parent_id"]; got != float64(10) {
		t.Errorf("parent_id = %v, want 10", got)
	}
}

// A panel written against NetBox mode selects status_value, which there is the
// raw value beside the label. This backend has no labels — the physical column
// holds the raw value — so the alias is built from it rather than left blank.
func TestChoiceValueAliasIsBuiltFromThePhysicalColumn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "status": "active"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "status_value"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["status_value"]; got != "active" {
		t.Errorf("status_value = %v, want active", got)
	}
	var announced bool
	for _, c := range res.Columns {
		if c == "status_value" {
			announced = true
		}
	}
	if !announced {
		t.Errorf("status_value must be announced as a column, got %v", res.Columns)
	}

	// The projection has to fetch the physical column it is built from, or the
	// alias would be built from a value that was never requested.
	if r, ok := f.requestWith("dcim/devices", "fields"); ok {
		if !strings.Contains(r.query.Get("fields"), "status") {
			t.Errorf("projection dropped the source column: %q", r.query.Get("fields"))
		}
	}
}

// A composite custom field has to expand the way NetBox mode expands it, or a
// panel selecting the cf_<name>_count that mode produces gets no such column
// and the list itself renders in a different format.
func TestCompositeCustomFieldsFollowTheSharedContract(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{
		"id": float64(1), "name": "CORE-1",
		"custom_field_data": `{"services": ["dns", "ntp", "syslog"],
			"tier": "gold",
			"empty_list": [],
			"owner": {"id": 22, "name": "Acme", "slug": "acme"},
			"criticality": {"value": "high", "label": "High"}}`,
	}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	row := res.Rows[0]
	for _, tc := range []struct {
		col  string
		want interface{}
	}{
		{"cf_services", "dns; ntp; syslog"},
		{"cf_services_count", float64(3)},
		{"cf_empty_list", ""},
		{"cf_empty_list_count", float64(0)},
		{"cf_tier", "gold"},
		{"cf_owner", "Acme"},
		{"cf_owner_id", float64(22)},
		{"cf_owner_slug", "acme"},
		{"cf_criticality", "High"},
		{"cf_criticality_value", "high"},
	} {
		if got := row[tc.col]; got != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.col, got, tc.want)
		}
	}
}

// Guards the CONTRACT rather than the wiring: that hoisting a whole
// custom_fields object produces exactly the columns that calling the same
// contract per field does. The sibling test above guards the wiring — it is the
// one that fails if this backend stops routing through the contract at all.
func TestCustomFieldExpansionMatchesNetBoxMode(t *testing.T) {
	cf := map[string]interface{}{
		"services":    []interface{}{"dns", "ntp"},
		"owner":       map[string]interface{}{"id": float64(22), "name": "Acme", "slug": "acme"},
		"criticality": map[string]interface{}{"value": "high", "label": "High"},
		"tier":        "gold",
	}
	netboxSide := map[string]interface{}{}
	provider.FlattenField("custom_fields", cf, func(n string, v interface{}) { netboxSide[n] = v })

	cacheSide := map[string]interface{}{}
	if err := expandCustomFields(cf, cacheSide); err != nil {
		t.Fatal(err)
	}

	if len(netboxSide) != len(cacheSide) {
		t.Fatalf("column sets differ: netbox %v, cache %v", netboxSide, cacheSide)
	}
	for k, want := range netboxSide {
		if got, ok := cacheSide[k]; !ok || got != want {
			t.Errorf("%s: cache has %#v, netbox has %#v", k, got, want)
		}
	}
}

// A mixed selection: a custom choice already flattened to cf_x_value, and a
// physical choice needing the alias. Returning on the first stopped the second
// from being built, and the projection then dropped it entirely.
func TestChoiceValueAliasSurvivesAnEarlierRealValueColumn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{
		"id": float64(1), "name": "CORE-1", "status": "active",
		"custom_field_data": `{"criticality": {"value": "high", "label": "High"}}`,
	}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"cf_criticality_value", "status_value"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// The real column keeps its own value; it is not overwritten by the alias.
	if got := res.Rows[0]["cf_criticality_value"]; got != "high" {
		t.Errorf("cf_criticality_value = %#v, want high", got)
	}
	if got := res.Rows[0]["status_value"]; got != "active" {
		t.Errorf("status_value = %#v, want active — the earlier field must not stop it", got)
	}
	for _, want := range []string{"cf_criticality_value", "status_value"} {
		var found bool
		for _, c := range res.Columns {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from columns %v", want, res.Columns)
		}
	}
}

// A provisioned dashboard's YAML can store " -name ", which the frontend
// normalizes and displays as name descending. Untrimmed, the "-" prefix was not
// even seen, so a stored descending sort on a perfectly ordinary stored column
// was reported as derived and dropped, and the panel came back unsorted.
func TestOrderingIsNormalizedBeforeItIsValidated(t *testing.T) {
	for _, stored := range []string{" -name ", "  name", "-name", " name "} {
		f := newFakeService()
		f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
		p := newTestProvider(t, f)

		res, err := p.Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices",
			Ordering:   stored,
		})
		if err != nil {
			t.Fatalf("%q: %v", stored, err)
		}
		for _, n := range res.Notes {
			if strings.Contains(n, "not sorted") {
				t.Errorf("%q: name is a stored column and must sort, got note %q", stored, n)
			}
		}
		r, ok := f.requestWith("dcim/devices", "sort")
		if !ok {
			t.Errorf("%q: no sort was pushed down", stored)
			continue
		}
		want := "name"
		if strings.Contains(stored, "-") {
			want = "-name"
		}
		if got := r.query.Get("sort"); got != want {
			t.Errorf("%q: sort=%q, want %q", stored, got, want)
		}
	}
}

// A column is a relationship only when the catalogue declares a reference for
// it. service_id is a NetBox CharField — circuits.ProviderNetwork's own service
// identifier — and its name must not conjure a "service" column.
func TestATextColumnEndingInIDIsStillNotARelationship(t *testing.T) {
	f := newFakeService()
	f.addEntity("circuits/provider-networks", "id:BIGINT:pk", "name:VARCHAR", "service_id:VARCHAR")
	f.entities["circuits/provider-networks"] = []map[string]interface{}{
		{"id": float64(1), "name": "NET-1", "service_id": "SVC-9"},
		{"id": float64(2), "name": "NET-2", "service_id": "SVC-10"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "circuits/provider-networks"})
	if err != nil {
		t.Fatalf("a text column that ends in _id is ordinary data: %v", err)
	}
	if res.Rows[0]["service_id"] != "SVC-9" {
		t.Errorf("service_id = %v, want SVC-9", res.Rows[0]["service_id"])
	}
	for _, c := range res.Columns {
		if c == "service" {
			t.Errorf("invented a relationship from a text column: %v", res.Columns)
		}
	}
}

// Returning nil for an unreadable payload read as "no custom fields", so the
// physical column was dropped, every requested cf_* column vanished, and
// nothing said why.
func TestUnreadableCustomFieldsAreReported(t *testing.T) {
	for _, bad := range []interface{}{
		`{"tier": `, // truncated
		`["not", "an", "object"]`,
		float64(42),
		true,
	} {
		f := newFakeService()
		f.entities["dcim/devices"] = []map[string]interface{}{
			{"id": float64(1), "name": "CORE-1", "custom_field_data": bad},
		}
		p := newTestProvider(t, f)
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
		if err == nil {
			t.Errorf("custom_field_data %#v was silently dropped", bad)
			continue
		}
		if !errors.Is(err, errMalformedCustomFields) {
			t.Errorf("%#v: want errMalformedCustomFields, got %v", bad, err)
		}
	}

	// The legitimately empty shapes are still answers, not failures.
	for _, ok := range []interface{}{nil, "", "   ", "{}"} {
		f := newFakeService()
		f.entities["dcim/devices"] = []map[string]interface{}{
			{"id": float64(1), "name": "CORE-1", "custom_field_data": ok},
		}
		p := newTestProvider(t, f)
		if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"}); err != nil {
			t.Errorf("custom_field_data %#v is an empty answer, not a failure: %v", ok, err)
		}
	}
}

// Expanding a name the caller did not ask for is not free: the server joins
// for it and every row widens. A rule selecting only site_id gets exactly that.
func TestUnrequestedRelationshipsAreNotExpanded(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	res, err := p.Query(ctx, provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site_id"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if req, ok := f.requestWith("dcim/devices", "expand"); ok {
		t.Errorf("site was never asked for, yet it was expanded: %v", req.query)
	}
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Errorf("site_id = %v, want 4001", res.Rows[0]["site_id"])
	}

	res, err = p.Query(ctx, provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if req, ok := f.requestWith("dcim/devices", "expand"); !ok || req.query.Get("expand") != "site" {
		t.Errorf("site was asked for and must be expanded, got %v", req.query)
	}
	if res.Rows[0]["site"] != "DC-Northeast" {
		t.Errorf("site = %v, want DC-Northeast", res.Rows[0]["site"])
	}
}

// A join key names a source the caller reads without displaying, and the same
// rule applies to it: joining on "site" needs the name, joining on "site_id"
// does not.
func TestJoinKeysFollowTheSameRule(t *testing.T) {
	e := catalogFromFake(t, devicesSchema()).Entities["dcim/devices"]
	expand := func(spec provider.QuerySpec) []string {
		t.Helper()
		return planRequest(e, spec).expand
	}
	if got := expand(provider.QuerySpec{Fields: []string{"name"}, KeyFields: []string{"site_id"}}); len(got) != 0 {
		t.Errorf("site_id as a join key does not need the name: %v", got)
	}
	if got := expand(provider.QuerySpec{Fields: []string{"name"}, KeyFields: []string{"site"}}); !slices.Equal(got, []string{"site"}) {
		t.Errorf("site as a join key needs the name: %v", got)
	}
	// A slug comes from the same expansion, so it counts as wanting it.
	if got := expand(provider.QuerySpec{Fields: []string{"site_slug"}}); !slices.Equal(got, []string{"site"}) {
		t.Errorf("site_slug needs the site expansion: %v", got)
	}
	// No projection at all expands nothing: resolving every reference on a
	// large table is what made the default query time out (measured), so a
	// related name is resolved when something asks for it by name.
	if got := expand(provider.QuerySpec{}); len(got) != 0 {
		t.Errorf("an unprojected query must not expand every reference, got %v", got)
	}
}

// A column the caller asked to SEE that the result does not contain must say
// so. A blank column reads as a blank; a missing one a panel selected is
// invisible, and alert evaluation treats warnings as failures precisely so it
// never runs on one. A saved panel selecting a NetBox-computed column such as
// ipam/prefixes.utilization gets nothing here, and got no word about it.
func TestARequestedColumnThatCannotBeProducedIsReported(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "utilization"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var told bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "utilization") {
			told = true
		}
	}
	if !told {
		t.Errorf("a column this backend cannot produce must be stated: %v", res.Warnings)
	}

	// And a query that got everything it asked for stays silent, or the
	// warning becomes noise that hides the real ones.
	res, err = p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "id"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("nothing was missing: %v", res.Warnings)
	}
}

// A join source is deliberately absent from Columns — the caller reads it
// without displaying it — so a relationship that failed to build there produced
// no warning, and applyJoinKeys announced an output column with an empty value
// on every row.
func TestAJoinKeySourceThatVanishesIsReported(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
		KeyFields:  []string{"utilization"}, // nothing this backend produces
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "utilization") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a join key that never built must be stated: %v", res.Warnings)
	}

	// And a join key that DID build stays silent — checking Columns instead of
	// Rows would have reported every join key as missing, since they are
	// deliberately not announced.
	f2 := newFakeService()
	f2.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f2.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p2 := newTestProvider(t, f2)
	res, err = p2.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
		KeyFields:  []string{"site"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("the join key built fine: %v", res.Warnings)
	}
	if res.Rows[0]["site"] != "DC-Northeast" {
		t.Errorf("site = %v, want DC-Northeast in the rows", res.Rows[0]["site"])
	}
}

// An OBJECT custom field flattens in NetBox mode to cf_<name> (the display
// name) plus cf_<name>_id. The column this mirrors holds the bare id, so
// cf_<name> is that number and the _id column was simply absent — a saved panel
// selecting it got no column and no explanation.
func TestObjectCustomFieldExposesItsID(t *testing.T) {
	f := newFakeService()
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "custom_field_data:VARCHAR")
	f.entities["dcim/interfaces"] = []map[string]interface{}{{
		"id": float64(1), "name": "eth0",
		"custom_field_data": `{"owning_tenant": 22, "tier": "gold", "tags": ["a","b"]}`,
	}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/interfaces",
		Fields:     []string{"name", "cf_owning_tenant_id"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["cf_owning_tenant_id"]; got != float64(22) {
		t.Errorf("cf_owning_tenant_id = %#v, want 22", got)
	}
	var announced bool
	for _, c := range res.Columns {
		if c == "cf_owning_tenant_id" {
			announced = true
		}
	}
	if !announced {
		t.Errorf("cf_owning_tenant_id missing from %v", res.Columns)
	}

	// A custom field whose value is NOT an identifier has no id to offer, and
	// inventing one would be worse than the missing column.
	res, err = p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/interfaces",
		Fields:     []string{"name", "cf_tier_id", "cf_tags_id"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, c := range []string{"cf_tier_id", "cf_tags_id"} {
		if _, ok := res.Rows[0][c]; ok {
			t.Errorf("%s was invented from a value that is not an identifier", c)
		}
	}
}

// No rows is not a degradation. hasColumn is false for every field when there
// is nothing to observe a column in, so an ordinary empty result — an
// offline-device rule while nothing is offline — reported every requested
// column as missing, and degradationError turns that into a rule in Error
// rather than a healthy empty evaluation.
func TestAnEmptyResultIsNotADegradation(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site", "status"},
		Filters:    []provider.Filter{{Field: "status", Operator: "", Value: "offline"}},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("this filter should match nothing, got %d rows", len(res.Rows))
	}
	if len(res.Warnings) != 0 {
		t.Errorf("an empty result is the healthy state of an alert, not a failure: %v", res.Warnings)
	}
}

// A join source is fetched but not displayed, so an alias asked for only as a
// join key had its physical source projected in and then never built:
// applyJoinKeys produced an empty output column, and since the backstop covers
// key fields too, alert evaluation failed on it.
func TestAliasesAreBuiltForJoinKeySourcesToo(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{
		"id": float64(1), "name": "CORE-1", "status": "active",
		"custom_field_data": `{"owning_tenant": 22}`,
	}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
		KeyFields:  []string{"status_value", "cf_owning_tenant_id"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// The contract: KeyFields are guaranteed in Rows, not in Columns.
	if got := res.Rows[0]["status_value"]; got != "active" {
		t.Errorf("status_value = %#v, want active", got)
	}
	if got := res.Rows[0]["cf_owning_tenant_id"]; got != float64(22) {
		t.Errorf("cf_owning_tenant_id = %#v, want 22", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("both key sources were built, so nothing is degraded: %v", res.Warnings)
	}
	// And they stay out of Columns: the caller reads them without displaying.
	for _, c := range res.Columns {
		if c == "status_value" || c == "cf_owning_tenant_id" {
			t.Errorf("%q was announced, but it was only a join source: %v", c, res.Columns)
		}
	}
}

// "All columns" leaves Fields empty while a join mapping still names a source
// in KeyFields. Skipping validation on Fields alone let applyJoinKeys announce
// an output column that was blank on every row with nothing to say why.
func TestKeyOnlyJoinsAreValidatedUnderAllColumns(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		KeyFields:  []string{"utilization"}, // Fields deliberately empty
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "utilization") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the join source never built and must be reported: %v", res.Warnings)
	}

	// And an all-columns query with no join mapping stays silent, since every
	// column the backend can produce is already there.
	f2 := newFakeService()
	f2.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f2.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	f2.entities["dcim/device-roles"] = []map[string]interface{}{
		{"id": float64(5), "name": "Core Router", "slug": "core-router"},
	}
	res, err = newTestProvider(t, f2).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("an unprojected healthy query has nothing to report: %v", res.Warnings)
	}
}

// Per row, not existentially. A page answering fields=name with
// [{"id":1,"name":"a"},{"id":2}] has the column somewhere, so an "is it
// anywhere" test passes it and the second object becomes a blank cell — a
// variable option silently dropped, or an alert label that lost its identity.
func TestProjectedColumnsAreCheckedOnEveryRow(t *testing.T) {
	srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 2, "results": [{"id": 1, "name": "a"}, {"id": 2}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
	})
	if err == nil {
		t.Fatal("a row missing a projected column must not become a blank cell")
	}
	if !errors.Is(err, errRowWithoutField) {
		t.Errorf("want errRowWithoutField, got %v", err)
	}
}

// A projected column that is PRESENT and null is an object with no value there,
// which is an ordinary answer and must stay one.
func TestProjectedNullsAreOrdinaryAnswers(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "serial": "ABC"},
		{"id": float64(2), "name": "CORE-2", "serial": nil},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "serial"},
	})
	if err != nil {
		t.Fatalf("a null value is an ordinary answer: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Errorf("rows = %d, want 2", len(res.Rows))
	}
}

// custom_field_data does not survive flattening — it becomes cf_* columns — so
// after flattening a row that omitted it cannot be told from one that carried
// an empty blob, and the existential check passes as soon as ANY row supplies
// the cf_* column. The projection is checked against the objects as they
// arrived, which is the only place the distinction still exists.
func TestCustomFieldProjectionIsCheckedBeforeItIsConsumed(t *testing.T) {
	srv := httptest.NewServer(withSchema(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The first row supplies cf_owner; the second omits the blob entirely.
		_, _ = w.Write([]byte(`{"count": 2, "results": [` +
			`{"id": 1, "custom_field_data": "{\"owner\": 22}"},` +
			`{"id": 2}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"cf_owner"},
	})
	if err == nil {
		t.Fatal("a row omitting the projected blob must not pass because another row carried it")
	}
	if !errors.Is(err, errRowWithoutField) {
		t.Errorf("want errRowWithoutField, got %v", err)
	}
}

// A DEFINED custom field is present in the blob even when unset — measured on a
// live instance, where every dcim/devices row carries the same six keys with
// three of them null. So flattening already gives it a column, and an alert
// whose objects all leave it unset evaluates a real column of nulls rather than
// erroring on a missing one.
func TestADefinedButUnsetCustomFieldHasItsColumn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "custom_field_data": `{"tier": null}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_tier"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("the field is defined, so nothing is missing: %v", res.Warnings)
	}
	if v, ok := res.Rows[0]["cf_tier"]; !ok || v != nil {
		t.Errorf("cf_tier = %#v (present %v), want a null value", v, ok)
	}
}

// And a cf_* absent from EVERY row is one this deployment does not have —
// deleted, or mistyped in a saved query. Synthesizing it hid that from the
// missing-column backstop and let an alert evaluate a column of nothing as
// authoritative.
func TestAnUnknownCustomFieldIsReportedNotInvented(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "custom_field_data": `{"tier": "gold"}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_deleted_field"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var told bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "cf_deleted_field") {
			told = true
		}
	}
	if !told {
		t.Errorf("a custom field this deployment does not have must be stated: %v", res.Warnings)
	}
	if _, ok := res.Rows[0]["cf_deleted_field"]; ok {
		t.Error("it must not be invented as a column of nulls")
	}
}

// A _count suffix is not proof that the column is a list's derived count: a
// custom field can be NAMED service_count, and NetBox shows an unset one as
// null. The evidence is whether the BASE is a column this entity has, read off
// the rows in hand: a defined custom field is in every row's blob.
func TestUnsetCountNeedsEvidenceThatItIsDerived(t *testing.T) {
	// No object anywhere has "services", so cf_services is not a column of this
	// entity and cf_services_count is read as a field named that way.
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "custom_field_data": "{}"},
	}
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_services_count"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// With no "services" key anywhere, there is no list for this to be the count
	// OF, and no literal field by that name either — so it is a column this
	// deployment does not have, and saying so beats inventing it.
	if _, ok := res.Rows[0]["cf_services_count"]; ok {
		t.Error("with no evidence of a list, the count must not be invented")
	}
	// The rows in hand are the evidence, and the whole of it: a defined
	// custom field is present in every row's blob, so a names read could not
	// find what the rows lack.
	if n := f.countRequestsFor("dcim/devices"); n != 1 {
		t.Errorf("made %d requests deciding a count; the rows already answered", n)
	}
	var told bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "cf_services_count") {
			told = true
		}
	}
	if !told {
		t.Errorf("that must be reported: %v", res.Warnings)
	}

	// With the list present somewhere in the entity, the same column IS the
	// derived count and an unset row gets the numeric zero the contract
	// promises.
	f2 := newFakeService()
	f2.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "custom_field_data": `{"services": ["dns"]}`},
		{"id": float64(2), "name": "CORE-2", "custom_field_data": "{}"},
	}
	res, err = newTestProvider(t, f2).Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_services_count"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, r := range res.Rows {
		if r["id"] == float64(2) && r["cf_services_count"] != float64(0) {
			t.Errorf("unset row count = %#v, want float64(0)", r["cf_services_count"])
		}
	}
}

// A mixed result — some objects with the custom field set, some without —
// already carries the column, so deciding existentially skipped the synthesis
// and left the unset rows without it. That is the cross-mode difference the
// contract exists to remove, and it hits exactly the rows a threshold cares
// about.
func TestUnsetCustomFieldsAreFilledPerRow(t *testing.T) {
	f := newFakeService()
	// Both rows carry both defined keys, which is what the service returns —
	// unset shows as null rather than an absent key.
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "custom_field_data": `{"services": ["dns","ntp"], "tier": "gold"}`},
		{"id": float64(2), "name": "CORE-2", "custom_field_data": `{"services": null, "tier": null}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_services_count", "cf_tier"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	byID := map[float64]map[string]interface{}{}
	for _, r := range res.Rows {
		byID[r["id"].(float64)] = r
	}
	if got := byID[1]["cf_services_count"]; got != float64(2) {
		t.Errorf("populated row count = %#v, want 2", got)
	}
	// The row this finding is about: unset, and previously left with no column.
	if got := byID[2]["cf_services_count"]; got != float64(0) {
		t.Errorf("unset row count = %#v, want float64(0)", got)
	}
	if got := byID[1]["cf_tier"]; got != "gold" {
		t.Errorf("cf_tier = %#v, want gold", got)
	}
	if v, ok := byID[2]["cf_tier"]; !ok || v != nil {
		t.Errorf("unset cf_tier = %#v (present %v), want a null value", v, ok)
	}
}

// The names read is 20 unfiltered rows, so a sparse list custom field can be
// absent from it while the rows in hand carry it. Those rows are evidence too,
// and better evidence — they are the result being answered.
func TestCountEvidenceCanComeFromTheRowsInHand(t *testing.T) {
	f := newFakeService()
	// Custom-field names are read from the FIRST customFieldSampleRows rows, so
	// the sparse field has to sit beyond them for this to be the case the
	// finding describes. Without that, the read sees it and the fix is never
	// exercised.
	var devices []map[string]interface{}
	for i := 1; i <= customFieldSampleRows+5; i++ {
		devices = append(devices, map[string]interface{}{
			"id": float64(i), "name": fmt.Sprintf("PLAIN-%02d", i), "custom_field_data": "{}",
		})
	}
	devices = append(devices,
		map[string]interface{}{"id": float64(100), "name": "RARE-1", "custom_field_data": `{"services": ["dns"]}`},
		map[string]interface{}{"id": float64(101), "name": "RARE-2", "custom_field_data": "{}"},
	)
	f.entities["dcim/devices"] = devices
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_services_count"},
		Filters:    []provider.Filter{{Field: "name", Operator: "ic", Value: "RARE"}},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, r := range res.Rows {
		if r["name"] == "RARE-2" && r["cf_services_count"] != float64(0) {
			t.Errorf("unset row count = %#v, want float64(0)", r["cf_services_count"])
		}
		if r["name"] == "RARE-1" && r["cf_services_count"] != float64(1) {
			t.Errorf("populated row count = %#v, want 1", r["cf_services_count"])
		}
	}
}

// A model can define BOTH a "service" custom field and a genuine one called
// "service_count". The flattener emits cf_X_count only alongside cf_X for the
// same object, so a row carrying the count without the base proves the name
// came out of the blob literally — and NetBox shows an unset literal field as
// null, not zero.
func TestALiteralCountFieldBeatsTheBaseNameHeuristic(t *testing.T) {
	f := newFakeService()
	// Both defined keys on both rows, as the service returns them. The literal
	// service_count and the list-derived cf_service_count claim the same column
	// name, and the contract's key ordering settles it: "service" sorts first,
	// its derived count lands, and the literal then overwrites it.
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "A", "custom_field_data": `{"service": null, "service_count": "SLA-2"}`},
		{"id": float64(2), "name": "B", "custom_field_data": `{"service": ["dns"], "service_count": null}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_service_count"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, r := range res.Rows {
		switch r["name"] {
		case "A":
			if r["cf_service_count"] != "SLA-2" {
				t.Errorf("A: cf_service_count = %#v, want its literal value", r["cf_service_count"])
			}
		case "B":
			if v, ok := r["cf_service_count"]; !ok || v != nil {
				t.Errorf("B: cf_service_count = %#v (present %v), want null — the field is literal and unset", v, ok)
			}
		}
	}
}

// A literal custom field named service_count, DEFINED but unset, beside a
// populated list-valued service. The two claim the same column name, and the
// resolution is the shared contract's ordering: sorted keys put "service"
// first, so its derived count lands and the literal null then overwrites it —
// the same answer NetBox mode reaches through the same contract.
//
// It works because the service carries every defined custom field in the blob,
// unset ones as an explicit null (measured on a live instance: all rows of
// dcim/devices carry the same six keys, three of them null). Pinned here
// because the behaviour depends on that ordering, which is easy to disturb.
func TestALiteralCountFieldUnsetBeatsTheDerivedOne(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "A", "custom_field_data": `{"service": ["dns","ntp"], "service_count": null}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "cf_service_count"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if v, ok := res.Rows[0]["cf_service_count"]; !ok || v != nil {
		t.Errorf("cf_service_count = %#v (present %v), want the literal field's null", v, ok)
	}
}

// A count-only caller reads Result.Total and nothing else, so building derived
// columns for it is waste — and one of the builders reads the custom-field
// names, a row request when the cache is cold. An alert counting a
// multi-million-row table was paying for a second list request to decorate rows
// it then discards.
func TestACountOnlyQueryMakesOneRequest(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "custom_field_data": `{"tier": "gold"}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		CountOnly:  true,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("total = %d, want 1", res.Total)
	}
	if n := f.countRequestsFor("dcim/devices"); n != 1 {
		t.Errorf("made %d requests for a count; the first one already had the answer", n)
	}
}

// And an ordinary query still gets its derived columns.
func TestAnOrdinaryQueryStillGetsDerivedColumns(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "status": "active", "custom_field_data": `{"tier": "gold"}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "status_value", "cf_tier"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Rows[0]["status_value"]; got != "active" {
		t.Errorf("status_value = %#v, want active", got)
	}
	if got := res.Rows[0]["cf_tier"]; got != "gold" {
		t.Errorf("cf_tier = %#v, want gold", got)
	}
}

// A slug comes from the object a foreign key points at, so an unset
// relationship has none. The missing-column backstop could not tell that from a
// slug that should have been built and was not, so it reported the legitimate
// case as a degradation — and a rule whose objects all have no tenant failed
// for having no tenant.
func TestARequestedSlugSurvivesAnUnsetRelationship(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "tenant_id": nil},
		{"id": float64(2), "name": "CORE-2", "tenant_id": nil},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "tenant", "tenant_slug"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("an unset relationship is not a degradation: %v", res.Warnings)
	}
	for _, col := range []string{"tenant", "tenant_slug"} {
		if v, ok := res.Rows[0][col]; !ok || v != nil {
			t.Errorf("%s = %#v (present %v), want a null value", col, v, ok)
		}
	}
}

// planRequest decides, from the catalogue alone, what a QuerySpec becomes on
// the wire: which stored columns to project, which references to expand, and
// whether the ordering can be pushed. It is pure, so every case is a table.
func TestPlanRequest(t *testing.T) {
	e := catalogFromFake(t, devicesSchema()).Entities["dcim/devices"]
	for _, tc := range []struct {
		name           string
		spec           provider.QuerySpec
		fields, expand []string
		sort           string
		notesHas       string
		warnsHas       string
	}{
		{"physical only", provider.QuerySpec{Fields: []string{"name", "serial"}}, []string{"name", "serial", "id"}, nil, "", "", ""},
		{"the key is not duplicated", provider.QuerySpec{Fields: []string{"id", "name"}}, []string{"id", "name"}, nil, "", "", ""},
		{"expanded name pulls its key", provider.QuerySpec{Fields: []string{"name", "site", "site_slug"}}, []string{"name", "id"}, []string{"site"}, "", "", ""},
		{"key field is fetched too", provider.QuerySpec{Fields: []string{"name"}, KeyFields: []string{"rack"}}, []string{"name", "id"}, []string{"rack"}, "", "", ""},
		{"cf pulls custom_field_data", provider.QuerySpec{Fields: []string{"cf_lifecycle_phase"}}, []string{"custom_field_data", "id"}, nil, "", "", ""},
		{"display_url pulls nothing extra", provider.QuerySpec{Fields: []string{"display_url"}}, []string{"id"}, nil, "", "", ""},
		{"empty fields expands nothing", provider.QuerySpec{}, nil, nil, "", "", ""},
		{"empty fields still expands a join key", provider.QuerySpec{KeyFields: []string{"site"}}, nil, []string{"site"}, "", "", ""},
		{"sort on physical", provider.QuerySpec{Fields: []string{"name"}, Ordering: "-name"}, []string{"name", "id"}, nil, "-name", "", ""},
		{"sort on expansion expands it", provider.QuerySpec{Fields: []string{"name"}, Ordering: "site"}, []string{"name", "id"}, []string{"site"}, "site", "", ""},
		{"descending sort on expansion", provider.QuerySpec{Fields: []string{"name"}, Ordering: " -site "}, []string{"name", "id"}, []string{"site"}, "-site", "", ""},
		{"sort on unavailable expansion is dropped with the cause", provider.QuerySpec{Fields: []string{"name"}, Ordering: "platform"}, []string{"name", "id"}, nil, "", "dcim/platforms has received no data", ""},
		{"sort on unknown is dropped", provider.QuerySpec{Fields: []string{"name"}, Ordering: "colour"}, []string{"name", "id"}, nil, "", "no such column", ""},
		{"unavailable expansion requested warns and is not expanded", provider.QuerySpec{Fields: []string{"name", "platform"}}, []string{"name", "id"}, nil, "", "", "platform cannot be resolved"},
		{"filter on expanded name expands it", provider.QuerySpec{Fields: []string{"name"}, Filters: []provider.Filter{{Field: "site", Operator: "ic", Value: "ams"}}}, []string{"name", "id"}, []string{"site"}, "", "", ""},
		{"count-only expands only for its filters", provider.QuerySpec{CountOnly: true, Ordering: "site", Filters: []provider.Filter{{Field: "rack", Operator: "ic", Value: "r1"}}}, nil, []string{"rack"}, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := planRequest(e, tc.spec)
			if !slices.Equal(r.fields, tc.fields) || !slices.Equal(r.expand, tc.expand) || r.sort != tc.sort {
				t.Errorf("fields=%v expand=%v sort=%q; want %v %v %q", r.fields, r.expand, r.sort, tc.fields, tc.expand, tc.sort)
			}
			if tc.notesHas != "" && !strings.Contains(strings.Join(r.notes, " "), tc.notesHas) {
				t.Errorf("notes %v lack %q", r.notes, tc.notesHas)
			}
			if tc.warnsHas != "" && !strings.Contains(strings.Join(r.warnings, " "), tc.warnsHas) {
				t.Errorf("warnings %v lack %q", r.warnings, tc.warnsHas)
			}
			if tc.warnsHas == "" && len(r.warnings) != 0 {
				t.Errorf("unexpected warnings %v", r.warnings)
			}
		})
	}
}

func TestQuery_ExpandsServerSideAndSortsOnTheName(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": 1, "name": "AMS1", "slug": "ams1"}, {"id": 2, "name": "NYC1", "slug": "nyc1"}}
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": 10, "name": "n-1", "site_id": 2, "rack_id": nil, "platform_id": 5, "custom_field_data": `{}`},
		{"id": 11, "name": "a-1", "site_id": 1, "rack_id": nil, "platform_id": nil, "custom_field_data": `{}`},
		{"id": 12, "name": "x-1", "site_id": nil, "rack_id": nil, "platform_id": nil, "custom_field_data": `{}`},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices",
		Fields: []string{"name", "site", "site_slug", "rack", "platform"}, Ordering: "site", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := f.requestWith("dcim/devices", "expand")
	if req.query.Get("expand") != "site,rack" || req.query.Get("sort") != "site" || req.query.Get("fields") != "name,id" {
		t.Errorf("request = %v", req.query)
	}
	if f.countRequestsFor("dcim/sites") != 0 {
		t.Error("no client-side dimension fetch: the server resolved the names")
	}
	// Sorted by site name on the server, nulls last; the null-site row keeps
	// null expansions (not blanks, not missing).
	if got := rowNames(res.Rows); !slices.Equal(got, []string{"a-1", "n-1", "x-1"}) {
		t.Errorf("order = %v", got)
	}
	if v, ok := res.Rows[2]["site"]; !ok || v != nil {
		t.Errorf("a null FK must yield null expansions, got %v", res.Rows[2])
	}
	if !slices.Equal(res.Columns, []string{"name", "site", "site_slug", "rack"}) {
		t.Errorf("columns = %v (platform is unavailable and must be absent, not blank)", res.Columns)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "platform cannot be resolved") {
		t.Errorf("warnings = %v, want exactly the unavailable-target warning", res.Warnings)
	}
}

// expand= goes with every query, count-only included: a filter on an expanded
// name is only valid under it, and the alert Count path sends the rule's
// filters with CountOnly. Only the sort and the projection are dropped.
func TestQuery_CountOnlyStillExpandsForFilters(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": 1, "name": "AMS1", "slug": "ams1"}}
	f.entities["dcim/devices"] = []map[string]interface{}{{"id": 1, "name": "a", "site_id": 1, "custom_field_data": `{}`}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", CountOnly: true, Limit: 1,
		Filters: []provider.Filter{{Field: "site", Operator: "ic", Value: "ams"}}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := f.requestWith("dcim/devices", "expand")
	if req.query.Get("expand") != "site" || req.query.Get("sort") != "" || req.query.Get("fields") != "" {
		t.Errorf("count-only request = %v: expand must travel, sort and fields must not", req.query)
	}
	if res.Total != 1 {
		t.Errorf("total = %d", res.Total)
	}
}

// The backend takes a text match's value literally — its ilike is a
// case-insensitive contains, and a % or _ in the value is that character — so
// the value goes upstream as written and the old wildcard refusal is gone.
func TestQuery_TextMatchSendsTheValueLiterally(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{"id": 1, "name": "100%", "custom_field_data": `{}`}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"},
		Filters: []provider.Filter{{Field: "name", Operator: "ic", Value: "0%"}}})
	if err != nil {
		t.Fatalf("a literal wildcard character is an ordinary value: %v", err)
	}
	req, _ := f.requestWith("dcim/devices", "filter[name]__ilike")
	if got := req.query.Get("filter[name]__ilike"); got != "0%" {
		t.Errorf("value = %q, want it sent as written", got)
	}
	if len(res.Rows) != 1 {
		t.Errorf("rows = %d, want the one name containing 0%%", len(res.Rows))
	}
}

// DATA-408: on a replica that lists them, the anchored matches are pushed
// down under their wire names with the value bare (the service's operators
// are literal and case-insensitive, like ilike), and the rows come back
// through the usual path. The row sets assert the fake's emulation of the
// service, not the service; the wire assertions are the behaviour under test.
func TestQuery_AnchoredTextMatchesArePushedDownWhenCatalogued(t *testing.T) {
	f := newFakeService()
	f.schema = withAnchoredText(devicesSchema())
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": 1, "name": "CORE-N9504-01", "custom_field_data": `{}`},
		{"id": 2, "name": "SPINE-CORE-01", "custom_field_data": `{}`},
		{"id": 3, "name": "core-n9504-02", "custom_field_data": `{}`},
	}
	p := newTestProvider(t, f)
	for _, tc := range []struct {
		op, wire, value string
		want            []string
	}{
		{"isw", "istartswith", "core", []string{"CORE-N9504-01", "core-n9504-02"}},
		{"iew", "iendswith", "-01", []string{"CORE-N9504-01", "SPINE-CORE-01"}},
		{"ie", "iexact", "core-n9504-01", []string{"CORE-N9504-01"}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}, Ordering: "id",
				Filters: []provider.Filter{{Field: "name", Operator: tc.op, Value: tc.value}}})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			param := "filter[name]__" + tc.wire
			req, ok := f.requestWith("dcim/devices", param)
			if !ok {
				t.Fatalf("no request carried %s", param)
			}
			if got := req.query.Get(param); got != tc.value {
				t.Errorf("%s = %q, want the value sent bare", param, got)
			}
			var got []string
			for _, r := range res.Rows {
				got = append(got, r["name"].(string))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// On a replica that lists only ilike (before DATA-408), a saved query with an
// anchored match is refused before anything is sent, with the reason naming
// the match that works — never sent as a contains that matches more rows.
func TestQuery_AnchoredTextMatchIsRefusedWhenTheCatalogueLacksIt(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{"id": 1, "name": "CORE-N9504-01", "custom_field_data": `{}`}}
	p := newTestProvider(t, f)
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"},
		Filters: []provider.Filter{{Field: "name", Operator: "isw", Value: "core"}}})
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Reason, "contains") {
		t.Fatalf("want an UnsupportedFilterError pointing at contains, got %v", err)
	}
	if _, ok := f.requestWith("dcim/devices", "filter[name]__istartswith"); ok {
		t.Error("the operator must not be sent to a replica that does not list it")
	}
}

// An unfed entity is not judged against its (empty) catalogue entry, but the
// operator vocabulary is the build's, not the entity's: on a replica whose
// catalogue lists the anchored matches nowhere, a starts-with on an unfed
// entity is refused before sending, like everywhere else — not sent to be
// answered with a bare HTTP 400 once the entity is fed. On a DATA-408 build
// it goes through, and the row route answers for itself.
func TestQuery_AnchoredTextMatchOnAnUnfedEntityFollowsTheBuild(t *testing.T) {
	spec := provider.QuerySpec{ObjectType: "dcim/platforms", Fields: []string{"name"},
		Filters: []provider.Filter{{Field: "name", Operator: "isw", Value: "ios"}}}

	f := newFakeService()
	p := newTestProvider(t, f)
	_, err := p.Query(context.Background(), spec)
	var unsupported *UnsupportedFilterError
	if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Reason, "contains") {
		t.Fatalf("pre-408 build: want the contains-pointing refusal, got %v", err)
	}
	if _, ok := f.requestWith("dcim/platforms", "filter[name]__istartswith"); ok {
		t.Error("pre-408 build: the operator must not be sent")
	}

	f = newFakeService()
	f.schema = withAnchoredText(devicesSchema())
	p = newTestProvider(t, f)
	_, _ = p.Query(context.Background(), spec)
	if _, ok := f.requestWith("dcim/platforms", "filter[name]__istartswith"); !ok {
		t.Error("DATA-408 build: the request goes, and the service answers for the unfed entity")
	}
}

// NetBox matches an exact address filter by host when the value has no mask
// (ipam's net_in lookup), so "address = 10.0.16.1" finds 10.0.16.1/21 and
// 10.0.16.1/24 alike. The replica stores the inet text, and equality on it
// found nothing while looking healthy. Where the schema lists host on the
// column, a bare value goes out as host; a masked one stays equality, in the
// form PostgreSQL prints (a /32 has no mask in the stored text).
func TestQuery_ExactAddressFilterMatchesLikeNetBox(t *testing.T) {
	f := newFakeService()
	f.addAddressEntity(true)
	f.entities["ipam/ip-addresses"] = []map[string]interface{}{
		{"id": 1, "address": "10.0.16.1/21"},
		{"id": 2, "address": "10.0.16.2/21"},
		{"id": 3, "address": "10.0.0.1"}, // a /32: PostgreSQL prints it without the mask
		{"id": 4, "address": "10.0.16.1/24"},
	}
	p := newTestProvider(t, f)
	for _, tc := range []struct {
		value, wire string
		want        []string
	}{
		{"10.0.16.1", "filter[address]__host", []string{"1", "4"}},
		{"10.0.16.1, 10.0.16.2", "filter[address]__host", []string{"1", "2", "4"}},
		{"10.0.0.1", "filter[address]__host", []string{"3"}},
		{"10.0.0.1/32", "filter[address]__eq", []string{"3"}},
		{"10.0.16.1/21", "filter[address]__eq", []string{"1"}},
		{"10.0.16.1, not-an-address", "filter[address]__host", []string{"1", "4"}},
		{"not-an-address", "filter[address]__eq", nil},
	} {
		t.Run(tc.value, func(t *testing.T) {
			res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/ip-addresses",
				Fields: []string{"id", "address"}, Ordering: "id",
				Filters: []provider.Filter{{Field: "address", Value: tc.value}}})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if _, ok := f.requestWith("ipam/ip-addresses", tc.wire); !ok {
				t.Errorf("no request carried %s", tc.wire)
			}
			var got []string
			for _, r := range res.Rows {
				got = append(got, fmt.Sprint(r["id"]))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

// The replica stores a single-host record as PostgreSQL prints it, with no
// mask ("10.0.0.1"); NetBox shows the same record as "10.0.0.1/32". Shown bare,
// a value picked from the rows or the value list read as a bare address and
// matched by host, broader than the record picked (it also caught 10.0.0.1/24),
// and a multi-select holding it beside a masked value was refused as a mix.
// Shown with its mask, the picked value matches that record alone.
func TestQuery_SingleHostAddressesShowTheirMask(t *testing.T) {
	f := newFakeService()
	f.addAddressEntity(true)
	f.entities["ipam/ip-addresses"] = []map[string]interface{}{
		{"id": 1, "address": "10.0.0.1"},
		{"id": 2, "address": "10.0.0.1/24"},
		{"id": 3, "address": "2001:db8::1"},
		{"id": 4, "address": "10.0.16.1/21"},
	}
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/ip-addresses", Fields: []string{"id", "address"}, Ordering: "id"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var shown []string
	for _, r := range res.Rows {
		shown = append(shown, fmt.Sprint(r["address"]))
	}
	if want := []string{"10.0.0.1/32", "10.0.0.1/24", "2001:db8::1/128", "10.0.16.1/21"}; !slices.Equal(shown, want) {
		t.Errorf("addresses shown = %v, want %v", shown, want)
	}

	values, err := p.FieldValues(context.Background(), "ipam/ip-addresses", "address", "", 100)
	if err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if !slices.Contains(values, "10.0.0.1/32") || slices.Contains(values, "10.0.0.1") {
		t.Errorf("values = %v; the single-host record must be offered with its mask", values)
	}

	// Picking two offered values — the single host and a masked one — matches
	// exactly those two records, no refusal and nothing broader.
	res, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/ip-addresses", Fields: []string{"id", "address"}, Ordering: "id",
		Filters: []provider.Filter{{Field: "address", Value: "10.0.0.1/32, 10.0.16.1/21"}}})
	if err != nil {
		t.Fatalf("filtering on offered values: %v", err)
	}
	var ids []string
	for _, r := range res.Rows {
		ids = append(ids, fmt.Sprint(r["id"]))
	}
	if !slices.Equal(ids, []string{"1", "4"}) {
		t.Errorf("ids = %v, want [1 4]: the two records picked, not every record with host 10.0.0.1", ids)
	}
}

// An expanded name whose target column lists host matches the same way:
// "primary_ip4 = 10.0.0.11" finds the device whose primary address is
// 10.0.0.11/21. Measured on a live replica: host works on an expanded name.
func TestQuery_ExactAddressFilterOnAnExpandedNameMatchesByHost(t *testing.T) {
	f := newFakeService()
	f.addAddressEntity(true)
	f.entities["ipam/ip-addresses"] = []map[string]interface{}{{"id": 1865, "address": "10.0.0.11/21"}}
	e := f.schema.Entities["/v1/dcim/devices"]
	e.Columns = append(e.Columns, fakeColumn{Name: "primary_ip4_id", Type: "BIGINT", Nullable: true, Operators: []string{"eq", "gt", "lt", "in", "isnull"}})
	f.schema.Entities["/v1/dcim/devices"] = e
	f.addReference("dcim/devices", "primary_ip4_id", "ipam/ip-addresses", "primary_ip4", "address")
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": 1, "name": "a", "primary_ip4_id": 1865, "custom_field_data": `{}`},
		{"id": 2, "name": "b", "primary_ip4_id": nil, "custom_field_data": `{}`},
	}
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "primary_ip4"},
		Filters: []provider.Filter{{Field: "primary_ip4", Value: "10.0.0.11"}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	req, ok := f.requestWith("dcim/devices", "filter[primary_ip4]__host")
	if !ok {
		t.Fatal("the expanded address name should be matched by host")
	}
	if !strings.Contains(req.query.Get("expand"), "primary_ip4") {
		t.Errorf("expand = %q, want primary_ip4 expanded for the filter", req.query.Get("expand"))
	}
	if len(res.Rows) != 1 || res.Rows[0]["name"] != "a" {
		t.Errorf("rows = %v, want device a alone", res.Rows)
	}
}

// A build whose schema does not list host keeps equality on the text, as
// before: host is never sent to a replica that would refuse it.
func TestQuery_ExactAddressFilterStaysEqualityWithoutHost(t *testing.T) {
	f := newFakeService()
	f.addAddressEntity(false)
	f.entities["ipam/ip-addresses"] = []map[string]interface{}{{"id": 1, "address": "10.0.16.1/21"}}
	p := newTestProvider(t, f)
	if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/ip-addresses", Fields: []string{"id", "address"},
		Filters: []provider.Filter{{Field: "address", Value: "10.0.16.1"}}}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if _, ok := f.requestWith("ipam/ip-addresses", "filter[address]__host"); ok {
		t.Error("host must not be sent to a build whose schema does not list it")
	}
	if _, ok := f.requestWith("ipam/ip-addresses", "filter[address]__eq"); !ok {
		t.Error("a build without host keeps equality")
	}
}

// Not every entity is keyed by "id": core/object-types is keyed by
// contenttype_ptr_id (measured on a live replica). The catalogue names the
// primary key, and every place that reads a row's identity — the duplicate
// and missing-id checks, the deep link, autocomplete — reads that column.
func TestQuery_UsesTheCataloguePrimaryKey(t *testing.T) {
	f := newFakeService()
	f.entities["core/object-types"] = []map[string]interface{}{
		{"contenttype_ptr_id": 1, "public": true},
		{"contenttype_ptr_id": 2, "public": false},
	}
	f.schema.NetBoxURL = "https://netbox.example.com"
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "core/object-types", Fields: []string{"public", "display_url"}})
	if err != nil {
		t.Fatalf("a table keyed by something other than id must be queryable: %v", err)
	}
	if len(res.Rows) != 2 || res.Rows[0]["display_url"] != "https://netbox.example.com/core/object-types/1/" {
		t.Errorf("rows = %v", res.Rows)
	}
	req, _ := f.requestWith("core/object-types", "fields")
	if got := req.query.Get("fields"); got != "public,contenttype_ptr_id" {
		t.Errorf("fields = %q, want the primary key projected in", got)
	}
	vals, err := p.FieldValues(context.Background(), "core/object-types", "public", "", 10)
	if err != nil || len(vals) != 2 {
		t.Errorf("autocomplete on such a table: %v %v", vals, err)
	}
}

// An entity the catalogue lists but has fed nothing for is a distinct error
// naming the type, not an empty table and not a missing endpoint. The ROW
// ROUTE says so, not the cached catalogue: a type that was fed a minute ago
// must not be refused for the rest of the catalogue's ten-minute life.
func TestQuery_UnfedEntityIsNotReplicated(t *testing.T) {
	f := newFakeService()
	p := newTestProvider(t, f)
	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/platforms"})
	u := provider.Classify(err)
	if u == nil || u.Kind != provider.ErrorKindNotReplicated || !strings.Contains(u.Detail, "dcim/platforms") {
		t.Errorf("got %v / %+v, want not-replicated naming the type", err, u)
	}
	if n := f.countRequestsFor("dcim/platforms"); n != 1 {
		t.Errorf("the row route is authoritative; %d requests were made", n)
	}
	// With a filter too: an unfed entity has no columns in the catalogue, so
	// judging the filter against it would refuse "no such column" — the wrong
	// subject. The alert Count path always sends the rule's filters.
	_, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/platforms", CountOnly: true,
		Filters: []provider.Filter{{Field: "name", Operator: "ic", Value: "ios"}}})
	if u := provider.Classify(err); u == nil || u.Kind != provider.ErrorKindNotReplicated {
		t.Errorf("filtered query on an unfed entity: got %v / %+v, want not-replicated", err, u)
	}
	if n := f.countRequestsFor("dcim/platforms"); n != 2 {
		t.Errorf("the row route must be asked; %d requests in total", n)
	}

	// The replica starts feeding platforms while the catalogue is still cached
	// as unfed: the next query gets the rows.
	f.mu.Lock()
	e := f.schema.Entities["/v1/dcim/platforms"]
	e.Ingested = true
	f.schema.Entities["/v1/dcim/platforms"] = e
	f.mu.Unlock()
	f.entities["dcim/platforms"] = []map[string]interface{}{{"id": float64(1), "name": "IOS-XR"}}
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/platforms"})
	if err != nil || len(res.Rows) != 1 {
		t.Errorf("a newly fed entity must not be refused from a stale catalogue: rows=%v err=%v", res, err)
	}
}

// Freshness is read from the list envelope, which carries the instant for
// THIS response, not from the catalogue cached up to ten minutes earlier: a
// continuously ingesting replica would otherwise report ages up to ten
// minutes too old, and Max data age would refuse fresh data.
func TestQuery_FreshnessComesFromTheEnvelope(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)
	if _, err := p.ObjectTypes(context.Background()); err != nil { // cache the catalogue at 14:03:11
		t.Fatal(err)
	}
	f.mu.Lock()
	e := f.schema.Entities["/v1/dcim/devices"]
	e.DataAsOf = strp("2026-09-22T15:00:00Z")
	f.schema.Entities["/v1/dcim/devices"] = e
	f.mu.Unlock()

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.DataAsOf == nil || res.DataAsOf.UTC().Format(time.RFC3339) != "2026-09-22T15:00:00Z" {
		t.Errorf("DataAsOf = %v, want the envelope's instant", res.DataAsOf)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "2026-09-22 15:00:00 UTC") {
		t.Errorf("notes = %v", res.Notes)
	}

	// The envelope's instant also ends the loading state for this entity,
	// whatever the cached catalogue still says.
	f.mu.Lock()
	f.schema.SnapshotComplete = false
	f.mu.Unlock()
	p = newTestProvider(t, f)
	res, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 || res.DataAsOf == nil {
		t.Errorf("an entity with an instant is not loading: warnings=%v DataAsOf=%v", res.Warnings, res.DataAsOf)
	}
}

// A commit time the replica reports but this datasource cannot read is said,
// not silently treated as "no commit time".
func TestQuery_UnreadableCommitTimeIsSaid(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.mu.Lock()
	e := f.schema.Entities["/v1/dcim/devices"]
	e.DataAsOf = strp("yesterday-ish")
	f.schema.Entities["/v1/dcim/devices"] = e
	f.mu.Unlock()
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(res.Notes, " ")
	if res.DataAsOf != nil || !strings.Contains(notes, "could not be read") || !strings.Contains(notes, "yesterday-ish") {
		t.Errorf("DataAsOf=%v notes=%v", res.DataAsOf, res.Notes)
	}
}

// A reference the replica cannot resolve — the row points at an object the
// replica does not hold, the normal state while a snapshot loads and after
// any create/delete window — comes back as a null name beside a real id. That
// is indistinguishable from "no site" to a rule grouping on it, so it is
// reported, as the client-side cascade used to report it.
func TestQuery_DanglingReferenceIsReported(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": float64(4001), "name": "DC-1", "slug": "dc-1"}}
	f.entities["dcim/devices"] = []map[string]interface{}{
		deviceFixture(1, "a", 4001),
		deviceFixture(2, "b", 999), // no such site
		{"id": float64(3), "name": "c", "site_id": nil, "role_id": float64(5), "custom_field_data": "{}"},
	}
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "site"}})
	if err != nil {
		t.Fatal(err)
	}
	warned := strings.Join(res.Warnings, " ")
	if !strings.Contains(warned, "dcim/sites") || !strings.Contains(warned, "1 of 3") || !strings.Contains(warned, "site_id") {
		t.Errorf("warnings = %v, want the dangling site named with its count", res.Warnings)
	}
	if v, ok := res.Rows[1]["site"]; !ok || v != nil {
		t.Errorf("the null stays a null: %v", res.Rows[1])
	}

	// Null ids are not dangling, and a fully resolved page stays silent.
	f.entities["dcim/devices"] = f.entities["dcim/devices"][:1]
	f.entities["dcim/devices"] = append(f.entities["dcim/devices"], map[string]interface{}{"id": float64(3), "name": "c", "site_id": nil, "role_id": nil, "custom_field_data": "{}"})
	res, err = newTestProvider(t, f).Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name", "site", "role"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "dcim/sites") {
			t.Errorf("a null id is not a dangling reference: %v", res.Warnings)
		}
	}
}

func rowNames(rows []map[string]interface{}) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["name"].(string))
	}
	return out
}

// JSON has one number type, so an id of 1.9 truncated to 1 would be a DIFFERENT
// object in every lookup and deep link built from it, and 0, a negative or a
// non-finite value is not an identifier at all.
func TestIDsMustBeWholeAndPositive(t *testing.T) {
	for _, v := range []interface{}{
		float64(1.9), float64(0), float64(-5), math.NaN(), math.Inf(1),
		"12", nil, true, float64(1) / 3,
	} {
		if id, ok := toInt(v); ok {
			t.Errorf("toInt(%#v) = %d, accepted; not a usable identifier", v, id)
		}
	}
	for _, v := range []interface{}{
		float64(1), float64(4001), int(7),
		// NetBox primary keys are 64-bit. An int32 cap — which this first had —
		// would reject legitimate ids on a large instance, and the bound that
		// matters is the one the wire format imposes: ids arrive as float64.
		float64(2147483648), float64(1 << 52), float64(maxExactID - 1),
	} {
		id, ok := toInt(v)
		if !ok {
			t.Errorf("toInt(%#v) rejected a valid id", v)
		}
		if f, isF := v.(float64); isF && float64(id) != f {
			t.Errorf("toInt(%#v) = %d, which is a different object", v, id)
		}
	}
	// The bound is INJECTIVITY, not exact representability. 2^53 is exactly
	// representable, but so is 2^53+1's rounded form — they are the same
	// float64 — and the rounding happens during decode, before anything here
	// can see it. Accepting 2^53 would therefore accept 2^53+1 as a different
	// object's id.
	if id, ok := toInt(float64(maxExactID)); ok {
		t.Errorf("toInt accepted %d, which 2^53+1 also decodes to", id)
	}
	if id, ok := toInt(float64(maxExactID) * 4); ok {
		t.Errorf("toInt accepted an inexact id as %d", id)
	}
	// Everything below the bound is unambiguous.
	if got, ok := toInt(float64(maxExactID - 1)); !ok || got != maxExactID-1 {
		t.Errorf("toInt(2^53-1) = %d,%v; want it accepted unchanged", got, ok)
	}
}

// Every result says how current its rows are, from the catalogue's per-entity
// instant. A replica still loading its initial snapshot is a WARNING: the rows
// may be a fraction of the fleet, served as a confident 200. An unknown age
// after the snapshot is a note.
func TestQuery_ReportsFreshness(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{{"id": 1, "name": "a", "custom_field_data": `{}`}}
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.DataAsOf == nil || res.DataAsOf.UTC().Format(time.RFC3339) != "2026-09-22T14:03:11Z" || res.SnapshotComplete == nil || !*res.SnapshotComplete {
		t.Errorf("DataAsOf=%v SnapshotComplete=%v", res.DataAsOf, res.SnapshotComplete)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "Data as of 2026-09-22 14:03:11 UTC") || len(res.Warnings) != 0 {
		t.Errorf("notes=%v warnings=%v", res.Notes, res.Warnings)
	}

	// Loading: no instant and the tenant-wide snapshot not complete.
	f.mu.Lock()
	f.schema.SnapshotComplete = false
	ent := f.schema.Entities["/v1/dcim/devices"]
	ent.DataAsOf = nil
	f.schema.Entities["/v1/dcim/devices"] = ent
	f.mu.Unlock()
	p = newTestProvider(t, f)
	res, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "still loading its initial snapshot") || res.DataAsOf != nil || *res.SnapshotComplete {
		t.Errorf("warnings=%v DataAsOf=%v", res.Warnings, res.DataAsOf)
	}

	// Unknown age after the snapshot: a note, not a warning.
	f.mu.Lock()
	f.schema.SnapshotComplete = true
	f.mu.Unlock()
	p = newTestProvider(t, f)
	res, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Fields: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "age of this data is unknown") || len(res.Warnings) != 0 {
		t.Errorf("notes=%v warnings=%v", res.Notes, res.Warnings)
	}
}
