package netbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// --- the allow-list as data --------------------------------------------------

func TestOrderingValue(t *testing.T) {
	cases := []struct {
		name       string
		objectType string
		requested  string
		want       string
	}{
		{"no sort requested", "dcim/devices", "", ""},
		{"allowed field carries the id tiebreaker", "dcim/devices", "name", "name,id"},
		{"descending keeps its sign", "dcim/devices", "-last_updated", "-last_updated,id"},
		{"id itself still carries the tiebreaker", "dcim/devices", "id", "id,id"},
		{"surrounding whitespace is not a different field", "dcim/sites", " name ", "name,id"},
		// Each of these was measured, not guessed — but they do not all fail the
		// same way, and only device_count fails on 4.4.10 at all. See orderingFields.
		{"device_count 500s under a projection that omits it, and we cannot know per query", "dcim/sites", "device_count", ""},
		{"scope 500s on 4.6.4 and sorts nothing on 4.4.10", "ipam/prefixes", "scope", ""},
		{"assigned_object 500s on 4.6.4 and sorts nothing on 4.4.10", "ipam/ip-addresses", "assigned_object", ""},
		// A field that is real on ANOTHER model is still not allowed here.
		{"a field allowed elsewhere is not allowed everywhere", "ipam/prefixes", "name", ""},
		{"an object type with no entry sorts on nothing", "plugins/bgp/bgp-sessions", "id", ""},
		{"a bare minus sign names no field", "dcim/devices", "-", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orderingValue(tc.objectType, tc.requested); got != tc.want {
				t.Errorf("orderingValue(%q, %q) = %q, want %q", tc.objectType, tc.requested, got, tc.want)
			}
		})
	}
}

// The three exclusions are the whole reason this list is curated rather than
// intuited, so they are pinned here by name with the measurement that earns each
// one. They are NOT three copies of the same failure: device_count is the only
// one that errors on 4.4.10, and re-probing the other two on 4.4.10 alone answers
// 200 twice, which reads as permission to add them back. A later reader who
// "fixes" the list that way breaks this test before they break a user's panel.
func TestOrderingFields_KeepsTheMeasuredExclusionsOut(t *testing.T) {
	excluded := []struct {
		objectType string
		field      string
		why        string
	}{
		{"dcim/sites", "device_count", "200 and sorts unprojected, 500 (`Cannot resolve keyword 'device_count' into field`) under a ?fields= projection that does not name it — and which shape a query takes is the panel's column selection, not ours to know when the sort list is offered"},
		{"ipam/prefixes", "scope", "500 on 4.6.4 (no automatic reverse relation); on 4.4.10 a 200 that sorts nothing — `scope` and `-scope` both return [1 2 3 4], where -id and -prefix reverse to [4 3 2 1]"},
		{"ipam/ip-addresses", "assigned_object", "500 on 4.6.4; on 4.4.10 a 200 that sorts nothing — `assigned_object` and `-assigned_object` both return the unordered walk's own ids, where -id reverses it"},
	}
	for _, ex := range excluded {
		for _, f := range OrderingFields(ex.objectType) {
			if f == ex.field {
				t.Errorf("%s lists %q as sortable, but it was measured doing nothing or breaking: %s",
					ex.objectType, ex.field, ex.why)
			}
		}
	}
}

// The list is DATA, and data has to stay well-formed: a direction sign or a
// duplicate in it would either be sent to NetBox as part of a field name or make
// the editor offer the same choice twice.
func TestOrderingFields_IsWellFormedData(t *testing.T) {
	for objectType, fields := range orderingFields {
		if len(fields) == 0 {
			t.Errorf("%s has an empty allow-list; drop the entry instead", objectType)
		}
		seen := map[string]bool{}
		for _, f := range fields {
			if strings.HasPrefix(f, "-") {
				t.Errorf("%s lists %q: direction is the caller's, the list holds bare field names", objectType, f)
			}
			if strings.TrimSpace(f) != f || f == "" {
				t.Errorf("%s lists %q: a name NetBox will not match", objectType, f)
			}
			if seen[f] {
				t.Errorf("%s lists %q twice", objectType, f)
			}
			seen[f] = true
		}
	}
}

// OrderingFields is what the query editor offers, so it must not hand out the
// slice the query path validates against.
func TestOrderingFields_CannotBeMutatedByItsCaller(t *testing.T) {
	got := OrderingFields("dcim/devices")
	if len(got) == 0 {
		t.Fatal("dcim/devices offers no sort fields at all")
	}
	got[0] = "device_count"
	if orderingValue("dcim/devices", "device_count") != "" {
		t.Error("a caller's edit to the returned slice reached the allow-list the query path trusts")
	}
	if OrderingFields("plugins/bgp/bgp-sessions") != nil {
		t.Error("an object type with no entry must offer no sort at all")
	}
}

// --- what reaches NetBox -----------------------------------------------------

// The tiebreaker is not decoration. Measured on the bundled demo (4.4.10):
// dcim/interfaces ?ordering=name returns ids [6 7 4 14 8 10] and ?ordering=name,id
// returns [1 2 4 6 7 8] — the six share the name "Ethernet1", so the sort key does
// not decide their order. Walking all 34 in pages of 6 with ?ordering=name yields
// 34 rows but only 29 distinct objects (five duplicated, five never returned);
// with ?ordering=name,id every object arrives exactly once.
func TestQuery_OrderingSendsTheFieldAndTheIDTiebreaker(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(1))
	res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", Ordering: "name", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := nb.last().Get(orderingParam); got != "name,id" {
		t.Errorf("ordering = %q, want %q — without the id tiebreaker a paged walk duplicates rows", got, "name,id")
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, want none: the sort was applied exactly as asked", res.Notes)
	}
}

func TestQuery_OrderingSupportsDescending(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(1))
	if _, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", Ordering: "-name", Limit: 10,
	}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := nb.last().Get(orderingParam); got != "-name,id" {
		t.Errorf("ordering = %q, want %q", got, "-name,id")
	}
}

// Today's behaviour, unchanged: no sort asked for, no parameter sent, nothing to
// say about it.
func TestQuery_NoOrderingIsTodaysBehaviour(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(1))
	res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if nb.last().Has(orderingParam) {
		t.Errorf("ordering = %q on a query that asked for none", nb.last().Get(orderingParam))
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, want none", res.Notes)
	}
}

// An unknown ordering field must never reach NetBox: unknown fields do not
// uniformly no-op, some 500 the query outright. And the query must not pretend it
// sorted — the rows are complete, but they are not the rows the caller asked to
// see first.
func TestQuery_UnknownOrderingFieldIsNeverSentAndSaysSo(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(1))
	res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/sites", Ordering: "device_count", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Query: %v — an unsortable field must cost the sort, not the answer", err)
	}
	if nb.last().Has(orderingParam) {
		t.Fatalf("ordering = %q reached NetBox; the allow-list is the guard that stops a 500",
			nb.last().Get(orderingParam))
	}
	if len(res.Rows) != 1 {
		t.Errorf("rows = %d, want the answer to survive an unsortable field", len(res.Rows))
	}
	assertOneNote(t, res, `device_count`, "only sorts")
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v: nothing failed and no column is blank, and a warning fails an alert rule outright", res.Warnings)
	}
}

// Fast paging and sorting are mutually exclusive upstream: NetBox 400s on ?start=
// with ?ordering= (the message is quoted once, on orderingParam in netbox.go).
// That is 4.6 behaviour — on the 4.4.10 demo ?start= is an unrecognised parameter
// that is ignored, so the combination cannot be reproduced there. The data
// source's paging setting is the operator's decision about the whole instance, so
// it wins — and says so.
func TestQuery_OrderingIsNotSentInCursorMode(t *testing.T) {
	t.Run("fast paging keeps the fast path and drops the sort", func(t *testing.T) {
		nb := newRecordingNetBox(t, 1, nil)
		res, err := nb.provider(WithCursorPaging(true)).Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices", Ordering: "name", Limit: 10, AllowUncounted: true,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if nb.last().Has(orderingParam) {
			t.Errorf("ordering = %q sent with ?start=: NetBox answers that combination with a 400",
				nb.last().Get(orderingParam))
		}
		if !nb.last().Has(startParam) {
			t.Error("cursor paging was silently switched off by a sort request; the operator's setting is the one that governs")
		}
		assertOneNote(t, res, `name`, "fast paging")
	})

	t.Run("without the caller's permission to skip the count, the sort is sent", func(t *testing.T) {
		nb := newRecordingNetBox(t, 1, intp(1))
		res, err := nb.provider(WithCursorPaging(true)).Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices", Ordering: "name", Limit: 10,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if got := nb.last().Get(orderingParam); got != "name,id" {
			t.Errorf("ordering = %q, want %q — cursor mode is off here, so nothing stops the sort", got, "name,id")
		}
		if nb.last().Has(startParam) {
			t.Error("cursor mode engaged without AllowUncounted")
		}
		if len(res.Notes) != 0 {
			t.Errorf("notes = %v, want none", res.Notes)
		}
	})
}

// A NetBox model may have a filter literally named `ordering`, and the caller's
// filter is the answer they asked for — exactly as it is for `fields`.
func TestQuery_OrderingYieldsToACallersOwnOrderingFilter(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(1))
	res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices",
		Ordering:   "name",
		Filters:    []provider.Filter{{Field: orderingParam, Value: "serial"}},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := nb.last().Get(orderingParam); got != "serial" {
		t.Errorf("ordering = %q, want the caller's own filter %q untouched", got, "serial")
	}
	assertOneNote(t, res, `name`, "filter")
}

// A count query reads the envelope and no rows at all, so a sort is pure upstream
// cost: on the large Cloud instance (6.8M devices) ordering by role measured 27.6s
// against 0.9s natural.
func TestQuery_CountOnlyNeverSorts(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(4200))
	res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", Ordering: "name", CountOnly: true,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if nb.last().Has(orderingParam) {
		t.Errorf("ordering = %q on a count query: NetBox sorts rows nothing will read",
			nb.last().Get(orderingParam))
	}
	if res.Total != 4200 {
		t.Errorf("total = %d, want 4200", res.Total)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v: there is no table to annotate on a count", res.Notes)
	}
}

// orderingClashNetBox is a NetBox that 500s only when a sort and a projection
// arrive together. That is modelled on the measured shape of dcim/sites
// ?ordering=device_count, which answers 200 unprojected and 500 under a
// `?fields=` projection that does not name device_count itself — the fake takes
// the simpler rule (any projection) because the point under test is the recovery,
// not the trigger, and the provider sends the narrow projection either way.
type orderingClashNetBox struct {
	spy     *listSpy
	objects []map[string]interface{}
}

func (f *orderingClashNetBox) start(t *testing.T) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/sites/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rec := url.Values{}
		for k, v := range q {
			rec[k] = v
		}
		rec.Set("__path", "dcim/sites")
		f.spy.record(rec)
		if q.Has(orderingParam) && q.Has(fieldsParam) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": "Cannot resolve keyword"}`))
			return
		}
		results := make([]map[string]interface{}, 0, len(f.objects))
		for _, o := range f.objects {
			results = append(results, applyNetBoxFields(o, q.Get(fieldsParam)))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"count": len(f.objects), "next": nil, "results": results,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", &http.Client{Timeout: 5 * time.Second})
}

// The allow-list is measured, not proven, so a release we have not seen can still
// refuse a sort we send. When it does, the recovery is the one this provider
// already has — drop the projection and re-ask — and the SORT must survive it:
// dropping the sort instead would answer a different question, because the sort
// decides which rows a limited query returns.
func TestQuery_OrderingSurvivesTheProjectionFallback(t *testing.T) {
	spy := &listSpy{}
	p := (&orderingClashNetBox{spy: spy, objects: siteFixtures()}).start(t)

	res, err := p.Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/sites", Ordering: "region", Fields: []string{"name"},
	})
	if err != nil {
		t.Fatalf("Query: %v — the existing projection fallback must cover this 500", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(res.Rows))
	}
	if n := spy.topLevelLists("dcim/sites"); n != 2 {
		t.Fatalf("requests = %d, want 2 (projected, then unprojected) — no second recovery path", n)
	}
	spy.mu.Lock()
	last := spy.queries[len(spy.queries)-1]
	spy.mu.Unlock()
	if last.Has(fieldsParam) {
		t.Errorf("refetch still carried ?fields=%q", last.Get(fieldsParam))
	}
	if got := last.Get(orderingParam); got != "region,id" {
		t.Errorf("refetch ordering = %q, want %q: the fallback trades columns for the answer, never the row set", got, "region,id")
	}
}

// assertOneNote checks the result carries exactly one note and that it names both
// what was asked for and why it did not happen.
func assertOneNote(t *testing.T, res *provider.Result, want ...string) {
	t.Helper()
	if len(res.Notes) != 1 {
		t.Fatalf("notes = %v, want exactly one saying the rows are not sorted", res.Notes)
	}
	for _, w := range want {
		if !strings.Contains(res.Notes[0], w) {
			t.Errorf("note %q does not mention %q", res.Notes[0], w)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v: an unsorted result is complete and correct, and a warning fails an alert rule", res.Warnings)
	}
}

// The ordering note has to survive utilization enrichment. Both features can be
// live on the same query — ipam/prefixes is in the ordering allow-list AND is
// one of the two types isUtilizationType covers — and the note is the only thing
// telling the panel why its rows came back in NetBox's natural order instead of
// the requested one. Losing it leaves the exact silent-wrong-answer shape the
// note exists to prevent: a sorted-looking panel that is not sorted, and nothing
// on the frame to say so.
//
// Reported on #111 by review; the assignment from enrichUtilization replaced the
// notes slice rather than appending to it, so the ordering note was dropped
// whenever a utilization column happened to be selected too.
func TestQuery_OrderingNoteSurvivesUtilizationEnrichment(t *testing.T) {
	nb := newRecordingNetBox(t, 1, nil)
	res, err := nb.provider(WithCursorPaging(true)).Query(context.Background(), provider.QuerySpec{
		ObjectType:     "ipam/prefixes",
		Fields:         []string{"prefix", "utilization"},
		Ordering:       "prefix",
		Limit:          10,
		AllowUncounted: true,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var found bool
	for _, n := range res.Notes {
		if strings.Contains(n, "fast paging") {
			found = true
		}
	}
	if !found {
		t.Errorf("the ordering note is gone from %v — a utilization column must not silence the explanation of why the sort was dropped", res.Notes)
	}
}
