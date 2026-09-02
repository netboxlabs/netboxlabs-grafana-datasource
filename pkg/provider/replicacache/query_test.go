package replicacache

import (
	"context"
	"errors"
	"strings"
	"testing"

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

func TestQueryResolvesForeignKeysToNames(t *testing.T) {
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
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(res.Rows))
	}
	row := res.Rows[0]

	// This is the whole point of FK resolution: a panel written against the
	// NetBox provider selects "site", not "site_id".
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
		t.Errorf("site = %v; the id column was not fetched to build it", res.Rows[0]["site"])
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

func TestQueryRejectsUnknownObjectType(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "d", 1)}
	p := newTestProvider(t, f)

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

// Sorting on a column we synthesize cannot be pushed down, and the service
// ignores it. Saying so is required: a panel must not believe it is sorted.
func TestQueryNotesWhenSortCannotBePushedDown(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": float64(4001), "name": "DC-1", "slug": "dc-1"}}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Ordering: "site"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Notes) == 0 {
		t.Fatal("want a note saying the rows are not sorted by a derived column")
	}
	if !strings.Contains(res.Notes[0], "site") {
		t.Errorf("note should name the column: %q", res.Notes[0])
	}
}

// A dimension that fails to answer leaves a blank name column, which looks
// exactly like "this device has no site". That gap has to be stated.
func TestQueryWarnsWhenADimensionCannotBeRead(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	// dcim/sites is a known entity but returns rows that omit the id, so nothing
	// resolves; the entity list still contains it.
	f.entities["dcim/sites"] = []map[string]interface{}{}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// No site name is available, and the id is still present rather than blank.
	if _, ok := res.Rows[0]["site"]; ok {
		t.Error("site should be absent when it could not be resolved, not blank")
	}
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Error("the raw id must survive an unresolved FK")
	}
}

// Dimension tables are the slowest-changing data in NetBox, and a panel
// refreshing every few seconds would otherwise re-read the site list on every
// query. The second query must resolve names without touching the wire.
func TestFKResolutionCachesDimensionReads(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"})
		if err != nil {
			t.Fatalf("Query %d: %v", i, err)
		}
		if res.Rows[0]["site"] != "DC-Northeast" {
			t.Fatalf("query %d lost the resolved name: %v", i, res.Rows[0]["site"])
		}
	}
	if n := f.countRequestsFor("dcim/sites"); n != 1 {
		t.Errorf("dimension read %d times across 3 queries, want 1 (the cache is not holding)", n)
	}
}
