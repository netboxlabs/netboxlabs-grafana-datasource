package replicacache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// The query path must not wait on entity discovery to learn its columns.
// Discovery is one large document measured failing while row endpoints answered
// in about 1.3s; putting it in front of the row request delayed every panel by
// a wait the rows do not depend on. Raw columns and types come from the
// main-table sample, which needs no discovery at all.
func TestProjectionDoesNotWaitOnDiscovery(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	// Discovery hangs for the whole test, as it does when the description
	// endpoint times out while the row endpoints are healthy.
	release := make(chan struct{})
	defer close(release)
	f.hangSwagger = release

	srv := f.start(t)
	p := New(srv.URL, "t", "nb", srv.Client())

	done := make(chan error, 1)
	go func() {
		// Columns are needed for both the projection and the sort check, so this
		// spec exercises rawColumns and columnTypes with a cold cache.
		_, err := p.Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices",
			Fields:     []string{"name", "status"},
			Ordering:   "name",
			Filters:    []provider.Filter{{Field: "name", Operator: "ic", Value: "CORE"}},
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the rows are healthy, so the query must answer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the query blocked on discovery; column facts must come from the row sample")
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
	for _, name := range sortedNames(cf) {
		provider.FlattenField("cf_"+name, cf[name], func(n string, v interface{}) { cacheSide[n] = v })
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
		if len(res.Notes) != 0 {
			t.Errorf("%q: name is a stored column and must sort, got notes %v", stored, res.Notes)
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

// Whether a *_id column is a relationship is decided across the PAGE, not per
// value. One row's numeric site_id confirms the column is a real foreign key,
// so a string in the next row is malformed rather than evidence that the column
// is text — read per value it was accepted, and resolveFKs would resolve the
// first row, leave the second blank, and warn about neither.
func TestAConfirmedForeignKeyColumnRejectsTextInOtherRows(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "site_id": float64(4001)},
		{"id": float64(2), "name": "CORE-2", "site_id": "4002"},
	}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	p := newTestProvider(t, f)

	_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices"})
	if err == nil {
		t.Fatal("a confirmed relationship column must not accept text in another row")
	}
	if !errors.Is(err, errMalformedFK) {
		t.Errorf("want errMalformedFK, got %v", err)
	}
}

// The other side of the same rule: a column where NO row carries an id is not a
// relationship at all, so text in it is ordinary data. service_id is a NetBox
// CharField, and a page of them must stay a perfectly good answer.
func TestATextColumnEndingInIDIsStillNotARelationship(t *testing.T) {
	f := newFakeService()
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

// Resolving a name the caller did not ask for is not free twice over: it can
// spend the whole discovery budget, and a dimension that fails adds a warning —
// which alert evaluation treats as a hard failure. A rule selecting only
// site_id would stop firing because dcim/sites was unreachable, though every
// value it asked for was present.
func TestUnrequestedRelationshipsAreNotResolved(t *testing.T) {
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	f.entities["dcim/sites"] = []map[string]interface{}{
		{"id": float64(4001), "name": "DC-Northeast", "slug": "dc-northeast"},
	}
	f.failEntities["dcim/sites"] = true // the dimension is down
	p := newTestProvider(t, f)
	ctx := context.Background()

	// Asking for the id only: the dimension being down is not this query's
	// problem, and must not be reported as a degradation.
	res, err := p.Query(ctx, provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site_id"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("site was never asked for: %v", res.Warnings)
	}
	if res.Rows[0]["site_id"] != float64(4001) {
		t.Errorf("site_id = %v, want 4001", res.Rows[0]["site_id"])
	}

	// Asking for the NAME: now the failure is this query's problem and must be
	// reported, or the blank column would read as "this device has no site".
	res, err = p.Query(ctx, provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Error("site was asked for and could not be read; that must be reported")
	}
}

// A join key names a source the caller reads without displaying, and the same
// rule applies to it: joining on "site" needs the name, joining on "site_id"
// does not.
func TestJoinKeysFollowTheSameRule(t *testing.T) {
	if want := wantedRelations(provider.QuerySpec{
		Fields: []string{"name"}, KeyFields: []string{"site_id"},
	}); want["site"] {
		t.Errorf("site_id as a join key does not need the name: %v", want)
	}
	if want := wantedRelations(provider.QuerySpec{
		Fields: []string{"name"}, KeyFields: []string{"site"},
	}); !want["site"] {
		t.Errorf("site as a join key needs the name: %v", want)
	}
	// A slug is built from the same lookup, so it counts as wanting it.
	if want := wantedRelations(provider.QuerySpec{Fields: []string{"site_slug"}}); !want["site"] {
		t.Errorf("site_slug needs the site lookup: %v", want)
	}
	// No projection at all still means everything.
	if want := wantedRelations(provider.QuerySpec{}); want != nil {
		t.Errorf("an unprojected query wants every relationship, got %v", want)
	}
}

// A relationship the caller asked to SEE that produced no column at all must
// say so. A blank column reads as a blank; a missing one a panel selected is
// invisible, and alert evaluation treats warnings as failures precisely so it
// never runs on one.
func TestARequestedRelationshipThatVanishesIsReported(t *testing.T) {
	f := newFakeService()
	// site_id is a string in every row, so nothing proves the column is a
	// relationship and it is accepted as text — a request for "site" then
	// produced neither a column nor a word about why.
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "site_id": "4001"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name", "site"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "site") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("site was requested and never appeared; that must be stated: %v", res.Warnings)
	}

	// A field with no source column at all is reported too. This assertion was
	// the other way round when the backstop first landed — "the projection's
	// business, not a degradation" — and that reasoning was wrong for the same
	// reason everything else in this file is: absence is indistinguishable from
	// emptiness, and alert evaluation reads an empty Warnings list as
	// permission to run. A saved panel selecting a NetBox-computed column such
	// as ipam/prefixes.utilization gets nothing here, and got no word about it.
	res, err = p.Query(context.Background(), provider.QuerySpec{
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
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "site_id": "4001"}, // text, so "site" never builds
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Fields:     []string{"name"},
		KeyFields:  []string{"site"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "site") {
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
	// site_id is text in every row, so "site" can never be built.
	f.entities["dcim/devices"] = []map[string]interface{}{
		{"id": float64(1), "name": "CORE-1", "site_id": "4001"},
	}
	p := newTestProvider(t, f)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		KeyFields:  []string{"site"}, // Fields deliberately empty
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "site") {
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
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// The schema sample, so the projection is built at all.
			_, _ = w.Write([]byte(`{"count": 1, "results": [{"id": 1, "name": "a", "status": "active"}]}`))
			return
		}
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
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// The schema sample, so a projection is built at all.
			_, _ = w.Write([]byte(`{"count": 1, "results": [{"id": 1, "name": "a", "custom_field_data": "{\"owner\": 22}"}]}`))
			return
		}
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
// null. The evidence is whether the BASE is a column this entity has, taken
// from the cached schema sample.
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

// The schema sample is 20 unfiltered rows, so a sparse list custom field can be
// absent from it while the rows in hand carry it. Those rows are evidence too,
// and better evidence — they are the result being answered.
func TestCountEvidenceCanComeFromTheRowsInHand(t *testing.T) {
	f := newFakeService()
	// The schema sample reads the FIRST sampleRows rows, so the sparse field has
	// to sit beyond them for this to be the case the finding describes. Without
	// that, the sample sees it and the fix is never exercised.
	var devices []map[string]interface{}
	for i := 1; i <= sampleRows+5; i++ {
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
		Filters:    []provider.Filter{{Field: "name", Operator: "isw", Value: "RARE"}},
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
// columns for it is waste — and one of the builders consults rawColumns, which
// fetches a schema sample when the cache is cold. An alert counting a
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
