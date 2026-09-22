package replicacache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

func TestCatalog_ParsesTheSchemaRoute(t *testing.T) {
	f := newFakeService()
	p := newTestProvider(t, f)
	c, err := p.catalogue(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !c.SnapshotComplete || c.NetBoxURL != "" {
		t.Errorf("envelope = complete:%v url:%q", c.SnapshotComplete, c.NetBoxURL)
	}
	dev, ok := c.Entities["dcim/devices"]
	if !ok {
		t.Fatalf("entities keyed by object type, got %v", keys(c.Entities))
	}
	if dev.PrimaryKey != "id" || !dev.Ingested || dev.DataAsOf == nil {
		t.Errorf("devices = %+v", dev)
	}
	if got := dev.DataAsOf.UTC().Format(time.RFC3339); got != "2026-09-22T14:03:11Z" {
		t.Errorf("data_as_of = %s", got)
	}
	// Columns keep the route's (table) order; the references parse whole.
	if dev.Columns[0].Name != "id" || dev.Columns[1].Name != "name" {
		t.Errorf("column order lost: %v", dev.Columns[:2])
	}
	site, ok := dev.column("site_id")
	if !ok || site.Ref == nil || site.Ref.ExpandKey != "site" || !site.Ref.Available || strings.Join(site.Ref.Columns, ",") != "name,slug" {
		t.Errorf("site_id reference = %+v", site.Ref)
	}
	if plat, _ := dev.column("platform_id"); plat.Ref == nil || plat.Ref.Available {
		t.Errorf("platform_id must be an unavailable reference, got %+v", plat.Ref)
	}
	if got := dev.expandedColumns(); strings.Join(got, ",") != "role,role_slug,tenant,tenant_slug,site,site_slug,rack" {
		t.Errorf("expandedColumns = %v (unavailable platform must be absent)", got)
	}
	if ot := c.Entities["core/object-types"]; ot.PrimaryKey != "contenttype_ptr_id" {
		t.Errorf("primary key must be read, not assumed: %q", ot.PrimaryKey)
	}
	if pl := c.Entities["dcim/platforms"]; pl.Ingested || len(pl.Columns) != 0 || pl.DataAsOf != nil {
		t.Errorf("unfed entity = %+v", pl)
	}
}

func TestCatalog_IsCachedForTheTTLAndForcedByHealthCheck(t *testing.T) {
	f := newFakeService()
	p := newTestProvider(t, f)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := p.catalogue(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.countRequestsFor("_meta/schema"); n != 1 {
		t.Errorf("schema route fetched %d times within the TTL, want 1", n)
	}
	if _, err := p.catalogue(ctx, true); err != nil {
		t.Fatal(err)
	}
	if n := f.countRequestsFor("_meta/schema"); n != 2 {
		t.Errorf("a forced refresh must refetch, got %d requests", n)
	}
}

func TestCatalog_MissingRouteIsAnOlderBuild(t *testing.T) {
	f := newFakeService()
	f.schema = nil
	p := newTestProvider(t, f)
	_, err := p.catalogue(context.Background(), false)
	if err == nil {
		t.Fatal("a build without the schema route must be an error, not an empty catalogue")
	}
	u := provider.Classify(err)
	if u == nil || u.Kind != provider.ErrorKindUpstream || !strings.Contains(u.Detail, "predates") {
		t.Errorf("classification = %+v, want upstream with the 'predates' message", u)
	}
	// And it is not cached: the next call asks again.
	_, _ = p.catalogue(context.Background(), false)
	if n := f.countRequestsFor("_meta/schema"); n != 2 {
		t.Errorf("a failed fetch must not be cached, got %d requests", n)
	}
}

// A 404 that is not the service's own — an HTML page from a proxy or a wrong
// path — is a wrong URL, not an old build, and the message must send the
// reader to the setting rather than to a vendor upgrade.
func TestCatalog_NonServiceRouteIsNotAnOlderBuild(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		_, _ = w.Write([]byte("<html><body>not found</body></html>"))
	}))
	defer srv.Close()
	p := New(srv.URL, "t", "nb", srv.Client())
	_, err := p.HealthCheck(context.Background())
	u := provider.Classify(err)
	if u == nil || strings.Contains(u.Detail, "predates") || !strings.Contains(u.Detail, "URL") {
		t.Errorf("classification = %+v, want the check-the-URL message", u)
	}
}

// The wire format is RFC3339 today; a tenant that ever reports another common
// shape must not collapse into "no commit time", and one that reports
// something unreadable must say so rather than pass as unknown.
func TestCatalog_DataAsOfLayouts(t *testing.T) {
	for raw, want := range map[string]string{
		"2026-09-22T14:03:11Z":            "2026-09-22T14:03:11Z",
		"2026-09-22T14:03:11.123456Z":     "2026-09-22T14:03:11Z",
		"2026-09-22T14:03:11+00:00":       "2026-09-22T14:03:11Z",
		"2026-09-22T14:03:11":             "2026-09-22T14:03:11Z",
		"2026-09-22 14:03:11":             "2026-09-22T14:03:11Z",
		"2026-09-22T16:03:11+0200":        "2026-09-22T14:03:11Z",
		"2026-09-22T14:03:11.123456+0000": "2026-09-22T14:03:11Z",
	} {
		e := schemaEntity{DataAsOf: strp(raw)}.toEntity()
		if e.DataAsOf == nil || e.DataAsOf.UTC().Format(time.RFC3339) != want {
			t.Errorf("%q: DataAsOf=%v, want %s", raw, e.DataAsOf, want)
		}
		if e.DataAsOfRaw != "" {
			t.Errorf("%q: parsed, so the raw value must not be kept: %q", raw, e.DataAsOfRaw)
		}
	}
	e := schemaEntity{DataAsOf: strp("soon")}.toEntity()
	if e.DataAsOf != nil || e.DataAsOfRaw != "soon" {
		t.Errorf("unreadable: DataAsOf=%v raw=%q", e.DataAsOf, e.DataAsOfRaw)
	}
}

// netbox_url is upstream-controlled and becomes a link target, so only an
// http(s) URL with a host is accepted, with the /api suffix a NetBox base
// often carries stripped as WithNetBoxURL strips it.
func TestCatalog_NetBoxURLIsValidated(t *testing.T) {
	for raw, want := range map[string]string{
		"https://nb.example.com":      "https://nb.example.com",
		"https://nb.example.com/api/": "https://nb.example.com",
		"http://nb:8000/":             "http://nb:8000",
		"javascript:alert(1)":         "",
		"//evil.example.com":          "",
		"nb.example.com":              "",
		"":                            "",
	} {
		c, err := schemaDoc{NetBoxURL: raw, Entities: map[string]schemaEntity{"/v1/x/y": {}}}.toCatalog()
		if err != nil {
			t.Fatal(err)
		}
		if c.NetBoxURL != want {
			t.Errorf("%q: NetBoxURL=%q, want %q", raw, c.NetBoxURL, want)
		}
	}
}

// Twenty panels refreshing together must not fetch the catalogue twenty
// times, on a cold start or at the TTL boundary.
func TestCatalog_ConcurrentCallersShareOneFetch(t *testing.T) {
	f := newFakeService()
	p := newTestProvider(t, f)
	burst := func() {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := p.ObjectTypes(context.Background()); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	}
	burst()
	if n := f.countRequestsFor("_meta/schema"); n != 1 {
		t.Errorf("cold burst fetched the catalogue %d times, want 1", n)
	}
	p.catMu.Lock()
	p.catExpires = time.Now().Add(-time.Second)
	p.catMu.Unlock()
	burst()
	if n := f.countRequestsFor("_meta/schema"); n != 2 {
		t.Errorf("expiry burst fetched the catalogue %d more times, want 1 (total %d)", n-1, n)
	}
}

func TestFieldTypeOf(t *testing.T) {
	for duck, want := range map[string]provider.FieldType{
		"VARCHAR": provider.FieldTypeString, "BIGINT": provider.FieldTypeNumber, "INTEGER": provider.FieldTypeNumber,
		"DOUBLE": provider.FieldTypeNumber, "FLOAT": provider.FieldTypeNumber, "DECIMAL(10,2)": provider.FieldTypeNumber,
		"BOOLEAN": provider.FieldTypeBoolean, "TIMESTAMP": provider.FieldTypeTime, "TIMESTAMP WITH TIME ZONE": provider.FieldTypeTime,
		"DATE": provider.FieldTypeTime, "BLOB": provider.FieldTypeString, "": provider.FieldTypeString,
	} {
		if got := fieldTypeOf(duck); got != want {
			t.Errorf("fieldTypeOf(%q) = %v, want %v", duck, got, want)
		}
	}
}

func keys(m map[string]entity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// catalogFromFake parses a fake schema document through the same code the
// provider uses on the wire, so a unit test on the catalogue's helpers works
// on exactly what a fetch would have produced.
func catalogFromFake(t *testing.T, s *fakeSchema) *catalog {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc schemaDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	c, err := doc.toCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// expandedColumn maps a derived name back to the reference that produces it
// and the target column it stands for, available or not: validation has to
// name an unfed target, not merely fail to find the column.
func TestEntity_ExpandedColumnNamesItsReferenceAndTarget(t *testing.T) {
	e := catalogFromFake(t, devicesSchema()).Entities["dcim/devices"]
	for name, want := range map[string][2]string{
		"site": {"site_id", "name"}, "site_slug": {"site_id", "slug"}, "rack": {"rack_id", "name"}, "platform": {"platform_id", "name"},
	} {
		col, target, ok := e.expandedColumn(name)
		if !ok || col.Name != want[0] || target != want[1] {
			t.Errorf("%s: (%s, %s, %v), want (%s, %s)", name, col.Name, target, ok, want[0], want[1])
		}
	}
	for _, name := range []string{"rack_slug", "site_id", "name", "cf_x"} {
		if _, _, ok := e.expandedColumn(name); ok {
			t.Errorf("%s is not an expansion", name)
		}
	}
	if !e.has("name") || e.has("site") {
		t.Error("has answers for physical columns only")
	}
}
