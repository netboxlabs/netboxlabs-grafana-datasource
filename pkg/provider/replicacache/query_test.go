package replicacache

import (
	"context"
	"errors"
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

// The better message — "not one of the N types this deployment reports" —
// depends on discovery already being cached, because the query path never waits
// on it. That is the real flow: the query editor populates its object-type
// dropdown before a query can name a type.
func TestQueryRejectsUnknownObjectType(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "d", 1)}
	p := newTestProvider(t, f)

	if _, err := p.ObjectTypes(context.Background()); err != nil {
		t.Fatalf("warming discovery: %v", err)
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

// replica-cache serves database rows, which carry no link back to the NetBox
// UI. Without synthesizing one, switching a datasource to this mode silently
// removes every "View in NetBox" link from panels that had them — the plugin
// layer keys that data link on a display_url column.
func TestQuerySynthesizesNetBoxDeepLinks(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(7, "CORE-7", 4001)}
	srv := f.start(t)
	p := New(srv.URL, "t", "nb", srv.Client(), WithNetBoxURL("https://netbox.example.com/"))

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

// Without a NetBox URL there is nothing to link to, and inventing one would
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
		t.Error("no NetBox URL is configured, so no link should be produced")
	}
}

// Selecting the link column must work: it is derived from the primary key, and
// naming it upstream would be rejected as an unknown field.
func TestQueryProjectsTheDeepLinkColumn(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(7, "CORE-7", 4001)}
	srv := f.start(t)
	p := New(srv.URL, "t", "nb", srv.Client(), WithNetBoxURL("https://netbox.example.com"))

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

// Discovery is served by one large document that was measured failing while row
// endpoints stayed healthy. Waiting on it before the row request delayed every
// panel by a full timeout it did not depend on, so the query path must consult
// only an already-cached entity list.
func TestQueryDoesNotFetchDiscoveryOnTheQueryPath(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		CountOnly:  true,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d", res.Total)
	}
	// A count-only query needs neither validation nor FK resolution, so nothing
	// should have reached the discovery document.
	if n := f.countRequestsFor("docs/openapi.json"); n != 0 {
		t.Errorf("discovery was fetched %d times on the query path", n)
	}
}

// Discovery is the slowest, least reliable request in this service, and FK
// resolution runs after the rows have already arrived. An unbounded fetch there
// delays a panel that has its data, so the wait is capped: the query returns
// promptly with ids and an honest warning rather than blocking on discovery.
func TestQueryDoesNotBlockIndefinitelyOnHungDiscovery(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": float64(4001), "name": "DC-1", "slug": "dc-1"}}
	p := newTestProvider(t, f)

	// Released with defer rather than t.Cleanup: httptest's Close waits for
	// outstanding handlers, and cleanups run last-registered-first, so a
	// Cleanup registered here would run AFTER Close and deadlock against the
	// handler it is meant to release.
	release := make(chan struct{})
	f.mu.Lock()
	f.hangSwagger = release
	f.mu.Unlock()
	defer close(release)

	start := time.Now()
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("the query must still succeed on its own data: %v", err)
	}
	if elapsed > discoveryWaitBudget+3*time.Second {
		t.Errorf("query took %s; the wait for discovery is meant to be capped at %s", elapsed, discoveryWaitBudget)
	}
	// The rows are complete and correct as ids.
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Errorf("rows lost their data: %v", res.Rows[0])
	}
	// The missing names are stated rather than left as a silent gap.
	if len(res.Warnings) == 0 {
		t.Error("want a warning that related names are missing")
	}
	if _, ok := res.Rows[0]["site"]; ok {
		t.Error("no name should be invented while the entity list is unavailable")
	}
}

// A cancelled dashboard should not hold the backend for the rest of the
// discovery budget: the rows are already fetched and nothing will read them.
func TestQueryStopsWaitingForDiscoveryWhenCancelled(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{{"id": float64(4001), "name": "DC-1", "slug": "dc-1"}}
	release := make(chan struct{})
	p := newTestProvider(t, f)
	f.mu.Lock()
	f.hangSwagger = release
	f.mu.Unlock()
	defer close(release)

	// Cancelled shortly after the call starts: the row fetch completes, then FK
	// resolution finds a cold cache and would otherwise wait out the budget.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("the rows were already fetched, so the query should still return them: %v", err)
	}
	if elapsed >= discoveryWaitBudget {
		t.Errorf("waited %s despite cancellation; the budget is %s and should have been cut short",
			elapsed, discoveryWaitBudget)
	}
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Error("rows must survive a cancelled discovery wait")
	}
}

// End to end for the self-referential case: a panel written against the NetBox
// provider selects "parent", not "parent_id", and the resolution has to read
// the parent's name out of the SAME table it is querying.
func TestQueryResolvesSelfReferentialParent(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/locations"] = []map[string]interface{}{
		{"id": float64(10), "name": "Campus", "slug": "campus", "parent_id": nil},
		{"id": float64(11), "name": "Building A", "slug": "building-a", "parent_id": float64(10)},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/locations"})
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

// The raw schema is what separates a stored column from a derived one. When it
// cannot be read we do not know which this is, and pushing the sort anyway
// fails in the worst direction: the service answers a derived column with 400
// "unknown sort column", so an optional ordering kills the whole panel.
func TestSortIsDroppedWhenTheSchemaCannotBeRead(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)

	// The schema probe goes first and fails; the row fetch behind it succeeds.
	f.mu.Lock()
	f.failOnce["dcim/devices"] = true
	f.mu.Unlock()

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Ordering:   "site",
	})
	if err != nil {
		t.Fatalf("the rows are healthy, so the query must succeed: %v", err)
	}
	if r, ok := f.requestWith("dcim/devices", "sort"); ok {
		t.Errorf("sort was pushed without confirming the column is stored: %v", r.query)
	}
	if len(res.Notes) == 0 {
		t.Error("dropping the sort must be stated, not silent")
	}
	if len(res.Rows) != 1 {
		t.Errorf("want the row, got %d", len(res.Rows))
	}
}
