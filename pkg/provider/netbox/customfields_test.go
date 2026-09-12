package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// customFieldsServer serves a devices list whose custom fields are ALL unset —
// the state a freshly created custom field is in — plus the custom-field
// definitions that say what those columns would hold. extrasStatus lets a test
// make the definitions endpoint fail; extrasHits counts the fetches so a test
// can prove the index is cached rather than re-asked per query.
func customFieldsServer(t *testing.T, extrasStatus int, extrasHits *int32, delay ...time.Duration) *httptest.Server {
	t.Helper()
	srv, _ := customFieldsServerMutable(t, extrasStatus, extrasHits, delay...)
	return srv
}

// customFieldsServerMutable is customFieldsServer whose NetBox can change
// under the index. The returned flag selects the extra state: 1 adds a new
// Integer field, rack_units, to both the definitions and the devices — a
// field created after the index was fetched; 2 adds a cf column, ghost, to the
// devices that the definitions never mention — a name no refresh can resolve;
// 3 serialises every definition's `type` as a plain string, so no row decodes;
// 4 does that to one row only; 5 reports a count far above the rows returned
// — an instance with more definitions than the walk ceiling — and answers a
// ?name= lookup for beyond_ceiling (Integer) and for nothing else; 6 adds a
// populated Object field, owner, whose flattening yields derived columns; 7 is
// 5 with every ?name= lookup stalling for 300ms; 8 adds a Text field, rack,
// populated on the devices; 9 is 8 plus a distinct Integer field, rack_id,
// created afterwards and unset on the devices; 10 is 6 plus a distinct Integer
// field, owner_id, created afterwards and unset — the case a base that CAN
// derive _id makes ambiguous by name alone; 12 is 1 with the definitions
// endpoint stalling for 300ms, so a refresh does not settle inside a small
// wait budget; 13 is 1 with the definitions endpoint answering 500, so a
// refresh fails fast.
func customFieldsServerMutable(t *testing.T, extrasStatus int, extrasHits *int32, delay ...time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var mode int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/extras/custom-fields/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(extrasHits, 1)
		for _, d := range delay {
			time.Sleep(d)
		}
		if atomic.LoadInt32(&mode) == 12 {
			time.Sleep(300 * time.Millisecond)
		}
		if atomic.LoadInt32(&mode) == 13 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"detail":"boom"}`)
			return
		}
		if name := r.URL.Query().Get("name"); name != "" {
			if atomic.LoadInt32(&mode) == 7 {
				time.Sleep(300 * time.Millisecond)
			}
			if name == "beyond_ceiling" {
				_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"id":9,"name":"beyond_ceiling","type":{"value":"integer","label":"Integer"},"object_types":["dcim.device"]}]}`)
			} else {
				_, _ = fmt.Fprint(w, `{"count":0,"next":null,"results":[]}`)
			}
			return
		}
		if extrasStatus != http.StatusOK {
			w.WriteHeader(extrasStatus)
			_, _ = fmt.Fprint(w, `{"detail":"You do not have permission to perform this action."}`)
			return
		}
		// A branch may define a field main does not (see customFieldsByBranch).
		extra := ""
		if r.Header.Get("X-NetBox-Branch") != "" {
			extra = `,{"id":5,"name":"branch_only","type":{"value":"integer","label":"Integer"},"object_types":["dcim.device"]}`
		}
		switch atomic.LoadInt32(&mode) {
		case 1, 12:
			extra += `,{"id":6,"name":"rack_units","type":{"value":"integer","label":"Integer"},"object_types":["dcim.device"]}`
		case 3:
			_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[
				{"id":1,"name":"alert_threshold","type":"integer","object_types":["dcim.device"]},
				{"id":3,"name":"is_managed","type":"boolean","object_types":["dcim.device"]}
			]}`)
			return
		case 4:
			extra += `,{"id":7,"name":"odd_one","type":"integer","object_types":["dcim.device"]}`
		case 6:
			extra += `,{"id":8,"name":"owner","type":{"value":"object","label":"Object"},"object_types":["dcim.device"]}`
		case 8:
			extra += `,{"id":10,"name":"rack","type":{"value":"text","label":"Text"},"object_types":["dcim.device"]}`
		case 9:
			extra += `,{"id":10,"name":"rack","type":{"value":"text","label":"Text"},"object_types":["dcim.device"]},{"id":11,"name":"rack_id","type":{"value":"integer","label":"Integer"},"object_types":["dcim.device"]}`
		case 10:
			extra += `,{"id":8,"name":"owner","type":{"value":"object","label":"Object"},"object_types":["dcim.device"]},{"id":12,"name":"owner_id","type":{"value":"integer","label":"Integer"},"object_types":["dcim.device"]}`
		}
		count := "4"
		if m := atomic.LoadInt32(&mode); m == 5 || m == 7 {
			count = "50000"
		}
		_, _ = fmt.Fprint(w, `{"count":`+count+`,"next":null,"results":[
			{"id":1,"name":"alert_threshold","type":{"value":"integer","label":"Integer"},"object_types":["dcim.devicerole","dcim.device"]},
			{"id":2,"name":"weight","type":{"value":"decimal","label":"Decimal"},"object_types":["dcim.device"]},
			{"id":3,"name":"is_managed","type":{"value":"boolean","label":"Boolean"},"object_types":["dcim.device"]},
			{"id":4,"name":"notes","type":{"value":"text","label":"Text"},"object_types":["dcim.device"]}`+extra+`
		]}`)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		// branch_only exists only on the branch, so only a branch request's
		// devices carry it: on main it would be a column the definitions have
		// never heard of, which is exactly what triggers a refresh.
		extra := ""
		if r.Header.Get("X-NetBox-Branch") != "" {
			extra = `,"branch_only":null`
		}
		switch atomic.LoadInt32(&mode) {
		case 1, 12, 13:
			extra += `,"rack_units":null`
		case 2:
			extra += `,"ghost":null`
		case 5, 7:
			extra += `,"beyond_ceiling":null,"ghost":null`
		case 6:
			extra += `,"owner":{"id":7,"url":"http://x/api/tenancy/tenants/7/","display":"Team A","name":"Team A","slug":"team-a"}`
		case 8, 9:
			extra += `,"rack":"A1","rack_id":null`
		case 10:
			extra += `,"owner":{"id":7,"url":"http://x/api/tenancy/tenants/7/","display":"Team A","name":"Team A","slug":"team-a"},"owner_id":null`
		case 11:
			_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[
				{"id":1,"name":"leaf1","custom_fields":{"alert_threshold":80,"weight":1.5,"is_managed":true,"notes":"x"}},
				{"id":2,"name":"leaf2","custom_fields":{"alert_threshold":95,"weight":2.5,"is_managed":false,"notes":"y"}}
			]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[
			{"id":1,"name":"leaf1","custom_fields":{"alert_threshold":null,"weight":null,"is_managed":null,"notes":null`+extra+`}},
			{"id":2,"name":"leaf2","custom_fields":{"alert_threshold":null,"weight":null,"is_managed":null,"notes":null`+extra+`}}
		]}`)
	})
	mux.HandleFunc("/api/dcim/sites/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"id":1,"name":"dc1","slug":"dc1"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &mode
}

// ageIndex backdates the cached index for main so the unknown-column refresh
// is allowed, as the clock would after customFieldsRetryTTL.
func ageIndex(t *testing.T, p *Provider) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.customFieldsByBranch[""]
	if !ok {
		t.Fatalf("no cached index to age")
	}
	e.fetchedAt = e.fetchedAt.Add(-2 * customFieldsRetryTTL)
	p.customFieldsByBranch[""] = e
}

func queryDevices(t *testing.T, p *Provider, ctx context.Context) *provider.Result {
	t.Helper()
	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices", Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return res
}

func TestCustomFieldTypes_DeclaredFromDefinitionWhenEveryRowIsUnset(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	res := queryDevices(t, p, context.Background())

	// Every row is null, which is exactly the case value inference cannot
	// classify — the columns must still be present and now carry a type.
	for _, c := range []string{"cf_alert_threshold", "cf_weight", "cf_is_managed", "cf_notes"} {
		if !contains(res.Columns, c) {
			t.Fatalf("column %q missing from %v", c, res.Columns)
		}
	}
	want := map[string]provider.FieldType{
		"cf_alert_threshold": provider.FieldTypeNumber,
		"cf_weight":          provider.FieldTypeNumber,
		"cf_is_managed":      provider.FieldTypeBoolean,
	}
	for c, typ := range want {
		if got := res.ColumnTypes[c]; got != typ {
			t.Errorf("ColumnTypes[%q] = %q, want %q", c, got, typ)
		}
	}
	// A text field's values already type as string, so declaring it would only
	// duplicate the fallback; it must stay undeclared.
	if _, ok := res.ColumnTypes["cf_notes"]; ok {
		t.Errorf("text custom field must not be declared: %v", res.ColumnTypes)
	}
	// Non-custom columns are never declared: their types come from values only.
	if _, ok := res.ColumnTypes["name"]; ok {
		t.Errorf("non-custom column declared: %v", res.ColumnTypes)
	}
}

func TestCustomFieldTypes_IndexIsCachedAcrossQueries(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	queryDevices(t, p, context.Background())
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("custom-field definitions fetched %d times across two queries, want 1", n)
	}
}

func TestCustomFieldTypes_NotFetchedWithoutCustomFieldColumns(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/sites", Limit: 10}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("definitions fetched %d times for a result with no cf_* column, want 0", n)
	}
}

func TestCustomFieldTypes_UnavailableDefinitionsDegradeToInferenceOnce(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusForbidden, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// A token that cannot read extras must still get its objects, untyped —
	// the hint is optional, the rows are not.
	res := queryDevices(t, p, context.Background())
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(res.Rows))
	}
	if len(res.ColumnTypes) != 0 {
		t.Errorf("ColumnTypes = %v, want none when definitions are unavailable", res.ColumnTypes)
	}
	// ...and the refusal is cached like a success: a 403 does not become a
	// per-query round trip.
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("definitions fetched %d times after a 403, want 1 (negative result cached)", n)
	}
}

func TestCustomFieldTypes_TransientFailureIsRetriedOnTheNextQuery(t *testing.T) {
	var hits int32
	// A 500 rather than a 503: the list fetch retries "not now" statuses on its
	// own, which would multiply the hit count by the retry schedule and make
	// this assertion about the wrong thing. A 500 is fetched once per query.
	p := New(customFieldsServer(t, http.StatusInternalServerError, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// A failing NetBox is "not now", not "not ever". The first query degrades to
	// inference; the failure must not pin that degradation for customFieldsTTL, or a
	// single 500 turns every numeric custom field into a string column — and
	// every threshold compare against '' into 0 — for half an hour.
	queryDevices(t, p, context.Background())
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if remaining := time.Until(entry.expiry); remaining > customFieldsRetryTTL {
		t.Fatalf("transient failure cached for %v, want at most %v", remaining, customFieldsRetryTTL)
	}
	// Expire it as the clock would and confirm the next query asks again.
	p.mu.Lock()
	entry.expiry = time.Now().Add(-time.Second)
	p.customFieldsByBranch[""] = entry
	p.mu.Unlock()
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("definitions fetched %d times across a 500 and its retry window, want 2", n)
	}
}

func TestCustomFieldTypes_ConcurrentMissesShareOneFetch(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits, 150*time.Millisecond).URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// A dashboard opening: every panel misses the cold index at once. One
	// fetch, one writer, everyone typed.
	const callers = 8
	results := make(chan map[string]provider.FieldType, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- p.customFieldTypes(context.Background()).types }()
	}
	for i := 0; i < callers; i++ {
		if got := (<-results)["cf_alert_threshold"]; got != provider.FieldTypeNumber {
			t.Errorf("caller %d: cf_alert_threshold = %q, want number", i, got)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("definitions fetched %d times for %d concurrent misses, want 1", n, callers)
	}
}

func TestCustomFieldTypes_CallerCancellationDoesNotAbortTheFetch(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits, 150*time.Millisecond).URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// The starter leaves before NetBox answers. It gets nothing for its own
	// query and must not poison the cache on the way out...
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := p.customFieldTypes(ctx); len(got.types) != 0 || got.settled {
		t.Fatalf("cancelled caller got %v settled=%v, want an empty, unsettled index", got.types, got.settled)
	}
	// ...while the fetch it started completes for the cache: the next caller
	// is typed from THAT fetch, not from a second one.
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		_, inFlight := p.customFieldsFlightByBranch[""]
		p.mu.Unlock()
		if !inFlight || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.customFieldTypes(context.Background()); got.types["cf_alert_threshold"] != provider.FieldTypeNumber || !got.settled {
		t.Errorf("after the starter cancelled, cf_alert_threshold = %q settled=%v, want number, settled", got.types["cf_alert_threshold"], got.settled)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("definitions fetched %d times, want 1 (the cancelled starter's fetch served the cache)", n)
	}
}

func TestCustomFieldTypes_StalledFetchDoesNotHoldTheQuery(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits, 300*time.Millisecond).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 30 * time.Millisecond

	// extras accepts the connection and stalls. The query must go out untyped
	// well inside its own budget rather than sit here until that budget is
	// spent — the index is a hint, the query is the product.
	started := time.Now()
	got := p.customFieldTypes(context.Background())
	if waited := time.Since(started); waited > 200*time.Millisecond {
		t.Fatalf("query waited %v on an in-flight fetch, want about the wait budget", waited)
	}
	if len(got.types) != 0 || got.settled {
		t.Fatalf("stalled fetch answered %v settled=%v, want an empty, unsettled index for this query", got.types, got.settled)
	}
	// The flight is not abandoned: it fills the cache for whoever comes next.
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		_, inFlight := p.customFieldsFlightByBranch[""]
		p.mu.Unlock()
		if !inFlight || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.customFieldTypes(context.Background()); got.types["cf_alert_threshold"] != provider.FieldTypeNumber || !got.settled {
		t.Errorf("after the stall cleared, cf_alert_threshold = %q settled=%v, want number, settled", got.types["cf_alert_threshold"], got.settled)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("definitions fetched %d times, want 1 (the stalled fetch served the cache)", n)
	}
}

func TestFields_DoesNotCacheTypesTakenFromAStalledIndex(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits, 300*time.Millisecond).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 30 * time.Millisecond

	cfType := func() provider.FieldType {
		t.Helper()
		fields, err := p.Fields(context.Background(), "dcim/devices")
		if err != nil {
			t.Fatalf("Fields: %v", err)
		}
		for _, f := range fields {
			if f.Name == "cf_alert_threshold" {
				return f.Type
			}
		}
		t.Fatalf("cf_alert_threshold missing from %v", fields)
		return ""
	}
	// The picker asks while extras is stalling: it gets the string fallback
	// for this call, which is the best available answer right now...
	if got := cfType(); got != provider.FieldTypeString {
		t.Fatalf("during the stall, cf_alert_threshold = %q, want the string fallback", got)
	}
	// ...but that answer must not be what the picker keeps seeing for the
	// next five minutes once the definitions have landed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		_, inFlight := p.customFieldsFlightByBranch[""]
		p.mu.Unlock()
		if !inFlight || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := cfType(); got != provider.FieldTypeNumber {
		t.Errorf("after the definitions landed, cf_alert_threshold = %q, want number (the stalled answer was cached)", got)
	}
	// And the settled answer IS cached: a third call does not re-sample.
	if got := cfType(); got != provider.FieldTypeNumber {
		t.Errorf("third call: cf_alert_threshold = %q, want number", got)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("definitions fetched %d times, want 1", n)
	}
}

func TestFields_CacheBuiltOnATransientFailureExpiresWithIt(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusInternalServerError, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// The definitions fail with a 500, which the index remembers for a minute.
	// The field list typed against that failure is a settled answer and may be
	// cached — but not for cacheTTL, or the picker would keep the string
	// fallback for four minutes after the index has retried and recovered.
	if _, err := p.Fields(context.Background(), "dcim/devices"); err != nil {
		t.Fatalf("Fields: %v", err)
	}
	p.mu.Lock()
	entry, cached := p.fields["dcim/devices"]
	index := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if !cached {
		t.Fatalf("a settled (failed) index is an answer; the field list should be cached")
	}
	if entry.expiry.After(index.expiry) {
		t.Errorf("field list cached until %v, index only until %v: the list outlives the index it was typed from", entry.expiry, index.expiry)
	}
	if remaining := time.Until(entry.expiry); remaining > customFieldsRetryTTL {
		t.Errorf("field list cached for %v after a transient definition failure, want at most %v", remaining, customFieldsRetryTTL)
	}
}

func TestCustomFieldTypes_NewFieldRefreshesTheIndex(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	queryDevices(t, p, context.Background())
	// An administrator creates rack_units after the index was fetched. The
	// next query returns the new column, all null, and must not build it as
	// a string column for the next thirty minutes.
	atomic.StoreInt32(mode, 1)
	ageIndex(t, p)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_rack_units"]; got != provider.FieldTypeNumber {
		t.Errorf("ColumnTypes[cf_rack_units] = %q after the field was created, want number (index not refreshed)", got)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("definitions fetched %d times, want 2 (one refresh for the unknown column)", n)
	}
}

func TestCustomFieldTypes_KnownUndeclaredColumnNeverRefreshes(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// notes is a Text field: known to the index and deliberately undeclared.
	// It must not read as "never heard of it" and trigger a refresh, however
	// old the index is — that would be one fetch per query for every Text
	// field on every object type.
	queryDevices(t, p, context.Background())
	ageIndex(t, p)
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("definitions fetched %d times with only known columns, want 1", n)
	}
}

func TestCustomFieldTypes_UnknownColumnRefreshIsRateLimited(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// ghost is a column the definitions never mention. A fresh index does not
	// refresh for it (the fetch was a moment ago)...
	atomic.StoreInt32(mode, 2)
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("definitions fetched %d times on a fresh index, want 1", n)
	}
	// ...an aged one refreshes once...
	ageIndex(t, p)
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("definitions fetched %d times after ageing, want 2 (one refresh)", n)
	}
	// ...and the name being still unknown does not make the next query ask
	// again inside the retry window.
	queryDevices(t, p, context.Background())
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("definitions fetched %d times for a name no refresh resolves, want 2 (rate-limited)", n)
	}
}

func TestCustomFieldTypes_UndecodableDefinitionsAreSkippedNotFatal(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// One definition in a shape this code does not expect. Its column falls
	// back to inference; the other definitions still declare.
	atomic.StoreInt32(mode, 4)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_alert_threshold"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_alert_threshold = %q with one undecodable sibling, want number", got)
	}
	if _, ok := res.ColumnTypes["cf_odd_one"]; ok {
		t.Errorf("the undecodable definition was declared: %v", res.ColumnTypes)
	}
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if !entry.ok || time.Until(entry.expiry) < customFieldsTTL-time.Minute {
		t.Errorf("a mostly-decoded fetch should be a success on customFieldsTTL; got ok=%v expiry in %v", entry.ok, time.Until(entry.expiry))
	}
}

func TestCustomFieldTypes_NothingDecodingIsAFailureNotAnEmptyIndex(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// Every definition fails to decode. Cached as a successful empty index,
	// this would make every cf_* column "unknown" for customFieldsTTL and re-fetch
	// once a minute with nothing logged; as a failure it is retried on the
	// short TTL and says so.
	atomic.StoreInt32(mode, 3)
	res := queryDevices(t, p, context.Background())
	if len(res.ColumnTypes) != 0 {
		t.Errorf("ColumnTypes = %v from undecodable definitions, want none", res.ColumnTypes)
	}
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if entry.ok {
		t.Errorf("a fetch where nothing decoded was cached as a success")
	}
	if remaining := time.Until(entry.expiry); remaining > customFieldsRetryTTL {
		t.Errorf("cached for %v, want at most the retry TTL %v", remaining, customFieldsRetryTTL)
	}
}

func TestCustomFieldTypes_TruncatedIndexResolvesUnknownColumnsByName(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// The instance reports far more definitions than the walk returned. The
	// index must not be trusted as complete: beyond_ceiling is a real Integer
	// field the walk never reached, ghost is not a field at all.
	atomic.StoreInt32(mode, 5)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_beyond_ceiling"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_beyond_ceiling = %q on a truncated index, want number (resolved by name)", got)
	}
	if _, ok := res.ColumnTypes["cf_ghost"]; ok {
		t.Errorf("cf_ghost declared: %v", res.ColumnTypes)
	}
	if got := res.ColumnTypes["cf_alert_threshold"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_alert_threshold = %q, want number (the walked part of the index still declares)", got)
	}
	// One walk plus one lookup per unknown name...
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Fatalf("extras hit %d times, want 3 (walk + two name lookups)", n)
	}
	// ...and each name is asked once per index lifetime, whatever the answer.
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Errorf("extras hit %d times after a second query, want 3 (names remembered)", n)
	}
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if !entry.truncated || !entry.ok {
		t.Errorf("entry truncated=%v ok=%v, want truncated and ok", entry.truncated, entry.ok)
	}
}

func TestCustomFieldTypes_CompleteIndexIsNotTruncated(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	queryDevices(t, p, context.Background())
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if entry.truncated {
		t.Errorf("a walk that returned every definition was marked truncated")
	}
}

func TestCustomFieldTypes_DerivedColumnsOfAnObjectFieldAreKnown(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// A populated Object field flattens to cf_owner, cf_owner_id and
	// cf_owner_slug. Only cf_owner is a definition name; the other two must
	// still count as known, or every query carrying them would refresh the
	// index once a minute for as long as the datasource runs.
	atomic.StoreInt32(mode, 6)
	res := queryDevices(t, p, context.Background())
	for _, c := range []string{"cf_owner", "cf_owner_id", "cf_owner_slug"} {
		if !contains(res.Columns, c) {
			t.Fatalf("expected derived column %q in %v", c, res.Columns)
		}
	}
	ageIndex(t, p)
	queryDevices(t, p, context.Background())
	queryDevices(t, p, context.Background())
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("definitions fetched %d times with only derived unknowns, want 1", n)
	}
}

func TestCustomFieldTypes_TruncatedLookupsShareTheWaitBudget(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 40 * time.Millisecond

	// The walk is fast and truncated; every by-name lookup stalls. Two
	// unknown columns must not cost the query two stalls — the lookups share
	// one budget, and past it the query goes out with inference.
	atomic.StoreInt32(mode, 7)
	started := time.Now()
	res := queryDevices(t, p, context.Background())
	if waited := time.Since(started); waited > 250*time.Millisecond {
		t.Fatalf("query took %v with stalled lookups, want about one wait budget", waited)
	}
	if _, ok := res.ColumnTypes["cf_beyond_ceiling"]; ok {
		t.Errorf("a lookup that timed out declared a type: %v", res.ColumnTypes)
	}
	if got := res.ColumnTypes["cf_alert_threshold"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_alert_threshold = %q, want number (the walked index still declares)", got)
	}
}

func TestCustomFieldTypes_SuffixNamedFieldNextToAScalarBaseIsNew(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// rack is a Text field; a Text field derives no _id column, so cf_rack_id
	// is not rack's derivative — it is a field the index has not met, and it
	// must trigger the refresh like any other unknown column.
	atomic.StoreInt32(mode, 8)
	queryDevices(t, p, context.Background())
	atomic.StoreInt32(mode, 9) // rack_id created as a distinct Integer field
	ageIndex(t, p)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_rack_id"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_rack_id = %q, want number (mistaken for cf_rack's derivative, index not refreshed)", got)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("definitions fetched %d times, want 2 (one refresh for the new field)", n)
	}
}

func TestCustomFieldTypes_WalkAndLookupsShareOneBudget(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits, 30*time.Millisecond)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 60 * time.Millisecond

	// The walk takes half the budget; every lookup stalls past the rest. The
	// query must go out after about one budget, not one for the walk plus
	// one more for the lookups.
	atomic.StoreInt32(mode, 7)
	started := time.Now()
	queryDevices(t, p, context.Background())
	if waited := time.Since(started); waited > 200*time.Millisecond {
		t.Fatalf("query took %v, want about one wait budget shared by walk and lookups", waited)
	}
}

func TestCustomFieldTypes_SuffixNamedFieldNextToAnObjectBaseIsNew(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// owner is an Object field, which CAN derive cf_owner_id. Then a distinct
	// Integer field named owner_id is created. By name alone the column is
	// ambiguous; by value it is not: a derived column never exists with a
	// null value, so an all-null cf_owner_id is the new field, and it must
	// trigger the refresh.
	atomic.StoreInt32(mode, 6)
	queryDevices(t, p, context.Background())
	atomic.StoreInt32(mode, 10)
	ageIndex(t, p)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_owner_id"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_owner_id = %q, want number (taken for cf_owner's derivative, index not refreshed)", got)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("definitions fetched %d times, want 2 (one refresh for the new field)", n)
	}
}

func TestCustomFieldTypes_ColumnsWithValuesNeverConsultTheIndex(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	// Values decide a column's type, so a result whose custom-field columns
	// all carry values has nothing to declare — and must not fetch.
	atomic.StoreInt32(mode, 11)
	res := queryDevices(t, p, context.Background())
	if len(res.ColumnTypes) != 0 {
		t.Errorf("ColumnTypes = %v for fully populated custom fields, want none", res.ColumnTypes)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("definitions fetched %d times with nothing to declare, want 0", n)
	}
}

func TestCustomFieldTypes_SuccessIsTrustedForCacheTTLNotSchemaTTL(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	queryDevices(t, p, context.Background())
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	// Definitions are administrator-edited data, not NetBox's code: a field
	// deleted and recreated under the same name with another type, while
	// every value is still unset, is invisible to the unknown-column refresh,
	// so the success TTL is what bounds how long it is typed as its
	// predecessor.
	if remaining := time.Until(entry.expiry); remaining > customFieldsTTL || remaining < customFieldsTTL-time.Minute {
		t.Errorf("successful index cached for %v, want about %v", remaining, customFieldsTTL)
	}
	if customFieldsTTL >= schemaTTL {
		t.Errorf("customFieldsTTL (%v) should be shorter than schemaTTL (%v): definitions are mutable data", customFieldsTTL, schemaTTL)
	}
}

func TestCustomFieldTypes_StalledRefreshKeepsTheDeclarationsInHand(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 40 * time.Millisecond

	queryDevices(t, p, context.Background())
	// A new field appears and the refresh it triggers stalls past the budget.
	// The columns the old index knew must stay typed from it — an alert
	// comparing against cf_alert_threshold must not see it revert to a string
	// column because an unrelated field was created — and only the new one
	// falls back to inference for this refresh.
	atomic.StoreInt32(mode, 12)
	ageIndex(t, p)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_alert_threshold"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_alert_threshold = %q during a stalled refresh, want number (declarations in hand discarded)", got)
	}
	if got := res.ColumnTypes["cf_is_managed"]; got != provider.FieldTypeBoolean {
		t.Errorf("cf_is_managed = %q during a stalled refresh, want boolean", got)
	}
	if _, ok := res.ColumnTypes["cf_rack_units"]; ok {
		t.Errorf("cf_rack_units declared before the refresh settled: %v", res.ColumnTypes)
	}
	// The refresh was not abandoned: once it lands, the new field is typed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		_, inFlight := p.customFieldsFlightByBranch[""]
		p.mu.Unlock()
		if !inFlight || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	atomic.StoreInt32(mode, 1)
	if got := queryDevices(t, p, context.Background()).ColumnTypes["cf_rack_units"]; got != provider.FieldTypeNumber {
		t.Errorf("after the refresh landed, cf_rack_units = %q, want number", got)
	}
}

func TestFields_DoesNotCacheAListTypedDuringAStalledRefresh(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 40 * time.Millisecond

	cfType := func(name string) provider.FieldType {
		t.Helper()
		fields, err := p.Fields(context.Background(), "dcim/devices")
		if err != nil {
			t.Fatalf("Fields: %v", err)
		}
		for _, f := range fields {
			if f.Name == name {
				return f.Type
			}
		}
		return ""
	}
	if got := cfType("cf_alert_threshold"); got != provider.FieldTypeNumber {
		t.Fatalf("warm-up: cf_alert_threshold = %q, want number", got)
	}
	// A new field appears and its refresh stalls. The picker keeps the known
	// declarations for this call but must not CACHE the list, or the new
	// field would be offered string operators until the old index expires.
	atomic.StoreInt32(mode, 12)
	ageIndex(t, p)
	p.mu.Lock()
	delete(p.fields, "dcim/devices") // force a fresh sample past the warm-up's cache
	p.mu.Unlock()
	if got := cfType("cf_alert_threshold"); got != provider.FieldTypeNumber {
		t.Errorf("during the stalled refresh, cf_alert_threshold = %q, want number (declarations in hand)", got)
	}
	if got := cfType("cf_rack_units"); got != provider.FieldTypeString {
		t.Errorf("during the stalled refresh, cf_rack_units = %q, want the string fallback", got)
	}
	p.mu.Lock()
	_, cached := p.fields["dcim/devices"]
	p.mu.Unlock()
	if cached {
		t.Fatalf("a field list typed during a stalled refresh was cached")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		_, inFlight := p.customFieldsFlightByBranch[""]
		p.mu.Unlock()
		if !inFlight || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	atomic.StoreInt32(mode, 1)
	if got := cfType("cf_rack_units"); got != provider.FieldTypeNumber {
		t.Errorf("after the refresh landed, cf_rack_units = %q, want number", got)
	}
}

func TestCustomFieldTypes_FailedRefreshKeepsTheIndexInHandAndInTheCache(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	queryDevices(t, p, context.Background())
	// A new field appears and the refresh it triggers fails fast with a 500.
	// Known columns must keep their declarations — for this caller AND for
	// the next, so the cache must not now hold a failure entry — and the
	// unknown column must back off for the retry TTL rather than re-trigger
	// a failing refresh on every query.
	atomic.StoreInt32(mode, 13)
	ageIndex(t, p)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_alert_threshold"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_alert_threshold = %q after a failed refresh, want number (index in hand discarded)", got)
	}
	if _, ok := res.ColumnTypes["cf_rack_units"]; ok {
		t.Errorf("cf_rack_units declared by a failed refresh: %v", res.ColumnTypes)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("extras hit %d times, want 2 (walk + failed refresh)", n)
	}
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if !entry.ok || entry.types["cf_alert_threshold"] != provider.FieldTypeNumber {
		t.Errorf("cache after a failed refresh: ok=%v types=%v, want the previous successful index", entry.ok, entry.types)
	}
	// Next caller: served from the restored index, no new fetch.
	res = queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_alert_threshold"]; got != provider.FieldTypeNumber {
		t.Errorf("next caller: cf_alert_threshold = %q, want number", got)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("extras hit %d times after the failed refresh, want 2 (unknown column backed off)", n)
	}
}

// fieldsNotCached runs Fields() for dcim/devices and reports whether the
// list was stored, after clearing any earlier entry so the call samples.
func fieldsNotCached(t *testing.T, p *Provider) (fields []provider.Field, cached bool) {
	t.Helper()
	p.mu.Lock()
	delete(p.fields, "dcim/devices")
	p.mu.Unlock()
	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	p.mu.Lock()
	_, cached = p.fields["dcim/devices"]
	p.mu.Unlock()
	return fields, cached
}

func TestFields_DoesNotCacheAListTypedAfterAFailedRefresh(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	queryDevices(t, p, context.Background())
	// A new field appears; the refresh fails fast and the previous index is
	// published back. The picker keeps the known declarations but must not
	// cache the list: the new field's string fallback would otherwise sit in
	// the picker until the old index expires, past the retry that fixes it.
	atomic.StoreInt32(mode, 13)
	ageIndex(t, p)
	fields, cached := fieldsNotCached(t, p)
	got := map[string]provider.FieldType{}
	for _, f := range fields {
		got[f.Name] = f.Type
	}
	if got["cf_alert_threshold"] != provider.FieldTypeNumber || got["cf_rack_units"] != provider.FieldTypeString {
		t.Errorf("after a failed refresh: alert_threshold=%q rack_units=%q, want number and the string fallback", got["cf_alert_threshold"], got["cf_rack_units"])
	}
	if cached {
		t.Errorf("a field list typed after a failed refresh was cached")
	}
}

func TestFields_DoesNotCacheAListAfterAFailedByNameLookup(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 40 * time.Millisecond

	// Truncated index, and every by-name lookup stalls past the budget. The
	// unknown field's type is neither known nor settled, so the list must
	// not be cached against the index's five-minute life.
	atomic.StoreInt32(mode, 7)
	fields, cached := fieldsNotCached(t, p)
	got := map[string]provider.FieldType{}
	for _, f := range fields {
		got[f.Name] = f.Type
	}
	if got["cf_alert_threshold"] != provider.FieldTypeNumber || got["cf_beyond_ceiling"] != provider.FieldTypeString {
		t.Errorf("after a failed lookup: alert_threshold=%q beyond_ceiling=%q, want number and the string fallback", got["cf_alert_threshold"], got["cf_beyond_ceiling"])
	}
	if cached {
		t.Errorf("a field list typed after a failed by-name lookup was cached")
	}
}

func TestCustomFieldTypes_ExpiredIndexSurvivesAFailedRefetch(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	queryDevices(t, p, context.Background())
	// The index reaches its expiry during a bad minute at NetBox. The
	// refetch fails; the declarations must not.
	p.mu.Lock()
	e := p.customFieldsByBranch[""]
	e.expiry = time.Now().Add(-time.Second)
	p.customFieldsByBranch[""] = e
	p.mu.Unlock()
	atomic.StoreInt32(mode, 13)
	res := queryDevices(t, p, context.Background())
	if got := res.ColumnTypes["cf_alert_threshold"]; got != provider.FieldTypeNumber {
		t.Errorf("cf_alert_threshold = %q after an expiry refetch failed, want number (expired index discarded)", got)
	}
	p.mu.Lock()
	entry := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if !entry.ok || entry.types["cf_alert_threshold"] != provider.FieldTypeNumber || !entry.refreshFailed {
		t.Errorf("cache after a failed expiry refetch: ok=%v refreshFailed=%v types=%v, want the previous index published back", entry.ok, entry.refreshFailed, entry.types)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("extras hit %d times, want 2", n)
	}
}

func TestCustomFieldTypes_ConcurrentWaiterOnAStalledRefreshKeepsDeclarations(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	p.customFieldsWaitOverride = 40 * time.Millisecond

	queryDevices(t, p, context.Background())
	atomic.StoreInt32(mode, 12) // new field; the refresh stalls
	ageIndex(t, p)
	// The first caller starts the refresh; a second arrives while the cache
	// is empty and joins the flight. It has no copy of the old index, so
	// the flight must hand it back — unsettled, but with every declaration.
	started := make(chan struct{})
	go func() {
		close(started)
		p.declaredCustomFieldTypes(context.Background(), []string{"cf_alert_threshold", "cf_rack_units"})
	}()
	<-started
	time.Sleep(10 * time.Millisecond)
	declared, settled, _ := p.declaredCustomFieldTypes(context.Background(), []string{"cf_alert_threshold", "cf_rack_units"})
	if declared["cf_alert_threshold"] != provider.FieldTypeNumber {
		t.Errorf("concurrent waiter: cf_alert_threshold = %q, want number", declared["cf_alert_threshold"])
	}
	if settled {
		t.Errorf("concurrent waiter on a stalled refresh reported settled")
	}
}

func TestFields_NextCallerAfterAFailedRefreshDoesNotCacheEither(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	queryDevices(t, p, context.Background())
	atomic.StoreInt32(mode, 13)
	ageIndex(t, p)
	queryDevices(t, p, context.Background()) // triggers the refresh, which fails; previous published back
	// A later Fields() reads the republished entry from the cache. It is a
	// good answer and not a settled one: the new field is still unresolved.
	_, cached := fieldsNotCached(t, p)
	if cached {
		t.Errorf("a field list read from a republished-after-failure index was cached")
	}
}

func TestFields_ListWithAnUnknownColumnOnAYoungIndexExpiresWhenTheRefreshWindowOpens(t *testing.T) {
	var hits int32
	srv, mode := customFieldsServerMutable(t, http.StatusOK, &hits)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})

	queryDevices(t, p, context.Background())
	// A new field appears within a minute of the fetch: too soon to refresh.
	// The picker may cache the list it types now, but only until the refresh
	// window opens — not for the index's remaining five minutes.
	atomic.StoreInt32(mode, 1)
	_, cached := fieldsNotCached(t, p)
	if !cached {
		t.Fatalf("a settled answer on a young index should be cacheable")
	}
	p.mu.Lock()
	entry := p.fields["dcim/devices"]
	index := p.customFieldsByBranch[""]
	p.mu.Unlock()
	if window := index.fetchedAt.Add(customFieldsRetryTTL); entry.expiry.After(window.Add(time.Second)) {
		t.Errorf("field list cached until %v, want no later than the refresh window at %v", entry.expiry, window)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("extras hit %d times, want 1 (too young to refresh)", n)
	}
}

func TestCustomFieldsFailureTTL(t *testing.T) {
	cases := []struct {
		name string
		err  error
		ttl  time.Duration
	}{
		{"forbidden is settled", &APIError{Status: 403}, schemaTTL},
		{"not found is settled", &APIError{Status: 404}, schemaTTL},
		{"method not allowed is settled", &APIError{Status: 405}, schemaTTL},
		{"request timeout is not", &APIError{Status: 408}, customFieldsRetryTTL},
		{"unlisted 4xx is not", &APIError{Status: 418}, customFieldsRetryTTL},
		{"too many requests is not", &APIError{Status: 429}, customFieldsRetryTTL},
		{"server error is not", &APIError{Status: 500}, customFieldsRetryTTL},
		{"unavailable is not", &APIError{Status: 503}, customFieldsRetryTTL},
		{"transport failure is not", errors.New("read: connection reset by peer"), customFieldsRetryTTL},
		{"fetch budget exhausted is not", context.DeadlineExceeded, customFieldsRetryTTL},
	}
	for _, c := range cases {
		if ttl := customFieldsFailureTTL(c.err); ttl != c.ttl {
			t.Errorf("%s: %v, want %v", c.name, ttl, c.ttl)
		}
	}
}

func TestCustomFieldTypes_PartitionedByBranch(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})

	main := queryDevices(t, p, context.Background())
	branch := queryDevices(t, p, provider.WithBranch(context.Background(), "b1"))

	if _, ok := main.ColumnTypes["cf_branch_only"]; ok {
		t.Errorf("main sees the branch-only definition: %v", main.ColumnTypes)
	}
	if got := branch.ColumnTypes["cf_branch_only"]; got != provider.FieldTypeNumber {
		t.Errorf("branch ColumnTypes[cf_branch_only] = %q, want number", got)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("definitions fetched %d times for main + one branch, want 2", n)
	}
}

func TestFields_UsesDefinitionWhenTheSampleLeavesACustomFieldUnset(t *testing.T) {
	var hits int32
	p := New(customFieldsServer(t, http.StatusOK, &hits).URL, "tok", &http.Client{Timeout: 5 * time.Second})
	fields, err := p.Fields(context.Background(), "dcim/devices")
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	got := map[string]provider.FieldType{}
	for _, f := range fields {
		got[f.Name] = f.Type
	}
	if got["cf_alert_threshold"] != provider.FieldTypeNumber {
		t.Errorf("cf_alert_threshold = %q, want number (the sample row leaves it unset)", got["cf_alert_threshold"])
	}
	if got["cf_is_managed"] != provider.FieldTypeBoolean {
		t.Errorf("cf_is_managed = %q, want boolean", got["cf_is_managed"])
	}
	if got["cf_notes"] != provider.FieldTypeString {
		t.Errorf("cf_notes = %q, want string", got["cf_notes"])
	}
}

func TestCustomFieldType_Mapping(t *testing.T) {
	cases := map[string]struct {
		typ provider.FieldType
		ok  bool
	}{
		"integer": {provider.FieldTypeNumber, true},
		"decimal": {provider.FieldTypeNumber, true},
		"boolean": {provider.FieldTypeBoolean, true},
		"text":    {"", false}, "longtext": {"", false}, "url": {"", false}, "json": {"", false},
		"select": {"", false}, "multiselect": {"", false}, "date": {"", false}, "datetime": {"", false},
		"object": {"", false}, "multiobject": {"", false}, "": {"", false},
	}
	for in, want := range cases {
		typ, ok := customFieldType(in)
		if typ != want.typ || ok != want.ok {
			t.Errorf("customFieldType(%q) = (%q,%v), want (%q,%v)", in, typ, ok, want.typ, want.ok)
		}
	}
}
