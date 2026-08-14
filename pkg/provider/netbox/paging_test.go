package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// recordingNetBox is a NetBox stand-in that records every list request it served
// and answers with a configurable envelope. The recorded query strings are the
// point of most tests here: the parameters this provider sends are exactly the
// thing NetBox will silently ignore if they are wrong.
type recordingNetBox struct {
	srv      *httptest.Server
	requests []url.Values

	// count is the envelope count. nil serializes as `"count": null`, which is
	// what NetBox 4.6 returns in cursor mode.
	count *int
	// rows is how many objects each page carries.
	rows int
	// object is the JSON object each row holds.
	object string
	// honorStart, when false, makes the stub behave like a NetBox that does not
	// implement cursor pagination: it ignores ?start= and still reports a count.
	honorStart bool
}

func newRecordingNetBox(t *testing.T, rows int, count *int) *recordingNetBox {
	t.Helper()
	nb := &recordingNetBox{count: count, rows: rows, object: `{"id":1,"name":"a","config_context":{}}`, honorStart: true}
	nb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nb.requests = append(nb.requests, r.URL.Query())
		results := make([]string, 0, nb.rows)
		for i := 0; i < nb.rows; i++ {
			results = append(results, nb.object)
		}
		count := "null"
		if !nb.honorStart || nb.count != nil {
			c := 0
			if nb.count != nil {
				c = *nb.count
			}
			count = fmt.Sprintf("%d", c)
		}
		if r.URL.Query().Has(startParam) && nb.honorStart {
			count = "null"
		}
		_, _ = fmt.Fprintf(w, `{"count":%s,"next":null,"results":[%s]}`, count, strings.Join(results, ","))
	}))
	t.Cleanup(nb.srv.Close)
	return nb
}

func (nb *recordingNetBox) provider(opts ...Option) *Provider {
	return New(nb.srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second}, opts...)
}

func (nb *recordingNetBox) last() url.Values {
	if len(nb.requests) == 0 {
		return url.Values{}
	}
	return nb.requests[len(nb.requests)-1]
}

func intp(n int) *int { return &n }

// --- (a) config-context exclusion -------------------------------------------

// The exclusion is only free while the projection has already dropped the
// column. Unprojected, `config_context` is a column this provider surfaces, and
// deleting it would be a visible change on an instance of any size.
func TestQuery_ExcludeConfigContext_OnlyRidesWithAProjection(t *testing.T) {
	t.Run("projected query sends it", func(t *testing.T) {
		nb := newRecordingNetBox(t, 1, intp(1))
		if _, err := nb.provider().Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices", Fields: []string{"name"}, Limit: 10,
		}); err != nil {
			t.Fatalf("Query: %v", err)
		}
		if got := nb.last().Get(excludeParam); got != configContextField {
			t.Errorf("exclude = %q, want %q", got, configContextField)
		}
		if got := nb.last().Get(fieldsParam); got != "name" {
			t.Errorf("fields = %q, want %q", got, "name")
		}
	})

	t.Run("unprojected query does not", func(t *testing.T) {
		nb := newRecordingNetBox(t, 1, intp(1))
		if _, err := nb.provider().Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices", Limit: 10,
		}); err != nil {
			t.Fatalf("Query: %v", err)
		}
		if nb.last().Has(excludeParam) {
			t.Errorf("unprojected query must keep the config_context column, got exclude=%q",
				nb.last().Get(excludeParam))
		}
	})

	t.Run("a query asking for the column does not", func(t *testing.T) {
		nb := newRecordingNetBox(t, 1, intp(1))
		if _, err := nb.provider().Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices", Fields: []string{"name", "config_context"}, Limit: 10,
		}); err != nil {
			t.Fatalf("Query: %v", err)
		}
		if nb.last().Has(excludeParam) {
			t.Error("a query that selects config_context must not exclude it")
		}
	})
}

// The projection fallback exists to recover every column when NetBox did not
// honor ?fields=. Leaving the exclusion behind would make that refetch return
// every column but one.
func TestQuery_ProjectionFallback_DropsTheExclusion(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(1))
	// An object that carries none of the requested columns forces the refetch.
	nb.object = `{"unrelated":1}`
	if _, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", Fields: []string{"name"}, Limit: 10,
	}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(nb.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (projected then refetched)", len(nb.requests))
	}
	if nb.last().Has(fieldsParam) || nb.last().Has(excludeParam) {
		t.Errorf("refetch must be unprojected and unexcluded, got %v", nb.last())
	}
}

// --- CountOnly ---------------------------------------------------------------

func TestQuery_CountOnly_FetchesTheCheapestRowAndKeepsTheTotal(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(6824570))
	res, err := nb.provider(WithCursorPaging(true)).Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", CountOnly: true, Limit: 1,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 6824570 {
		t.Errorf("Total = %d, want the envelope count", res.Total)
	}
	q := nb.last()
	if q.Get(fieldsParam) != "id" || q.Get(excludeParam) != configContextField {
		t.Errorf("count query = %v, want fields=id and the config-context exclusion", q)
	}
	// Even with the datasource opted into cursor paging: a count query is the
	// total, so it can never be served by the mode that does not produce one.
	if q.Has(startParam) {
		t.Error("a count query must never use cursor paging")
	}
	if q.Get("limit") != "1" {
		t.Errorf("limit = %q, want 1", q.Get("limit"))
	}
}

// The two flags state opposite things about the same value. A caller that sets
// both has a bug that would otherwise surface as an alert rule evaluating a
// silent zero, so it fails here instead.
func TestQuery_CountOnlyWithAllowUncounted_IsAnError(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(1))
	_, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", CountOnly: true, AllowUncounted: true,
	})
	if err == nil {
		t.Fatal("want an error for CountOnly + AllowUncounted, got nil")
	}
	if len(nb.requests) != 0 {
		t.Errorf("the contradiction must be caught before any request, got %d", len(nb.requests))
	}
}

// --- (b) cursor paging is opt-in --------------------------------------------

func TestQuery_CursorPaging_RequiresBothTheSettingAndThePermission(t *testing.T) {
	cases := []struct {
		name      string
		opts      []Option
		spec      provider.QuerySpec
		wantStart bool
	}{
		{
			name:      "off by default, even where it would be legal",
			spec:      provider.QuerySpec{ObjectType: "dcim/devices", AllowUncounted: true, Limit: 10},
			wantStart: false,
		},
		{
			name:      "setting on but the caller needs a total",
			opts:      []Option{WithCursorPaging(true)},
			spec:      provider.QuerySpec{ObjectType: "dcim/devices", Limit: 10},
			wantStart: false,
		},
		{
			name:      "setting on and the caller can do without one",
			opts:      []Option{WithCursorPaging(true)},
			spec:      provider.QuerySpec{ObjectType: "dcim/devices", AllowUncounted: true, Limit: 10},
			wantStart: true,
		},
		{
			// NetBox answers start+ordering with HTTP 400. A filter row can put
			// an ordering parameter on the wire, and a broken panel is worse
			// than a slow one.
			name: "a query that orders cannot use it",
			opts: []Option{WithCursorPaging(true)},
			spec: provider.QuerySpec{
				ObjectType:     "dcim/devices",
				AllowUncounted: true,
				Filters:        []provider.Filter{{Field: "ordering", Value: "name"}},
				Limit:          10,
			},
			wantStart: false,
		},
		{
			// NetBox answers start+offset with HTTP 400 as well.
			name: "a query that offsets cannot use it",
			opts: []Option{WithCursorPaging(true)},
			spec: provider.QuerySpec{
				ObjectType:     "dcim/devices",
				AllowUncounted: true,
				Filters:        []provider.Filter{{Field: "offset", Value: "10"}},
				Limit:          10,
			},
			wantStart: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nb := newRecordingNetBox(t, 1, intp(3))
			if _, err := nb.provider(tc.opts...).Query(context.Background(), tc.spec); err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got := nb.last().Has(startParam); got != tc.wantStart {
				t.Errorf("start sent = %v, want %v (query %v)", got, tc.wantStart, nb.last())
			}
		})
	}
}

// An uncounted full page is otherwise indistinguishable from a complete answer.
func TestQuery_CursorPaging_UncountedResultSaysSo(t *testing.T) {
	t.Run("full page", func(t *testing.T) {
		nb := newRecordingNetBox(t, 10, nil)
		res, err := nb.provider(WithCursorPaging(true)).Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices", AllowUncounted: true, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if res.Total != 0 {
			t.Errorf("Total = %d, want 0 (no count was reported)", res.Total)
		}
		if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "total is unavailable") {
			t.Fatalf("want a note stating the count is unavailable, got %v", res.Notes)
		}
	})

	t.Run("short page needs no note", func(t *testing.T) {
		nb := newRecordingNetBox(t, 3, nil)
		res, err := nb.provider(WithCursorPaging(true)).Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/devices", AllowUncounted: true, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(res.Notes) != 0 {
			t.Errorf("a complete result needs no note, got %v", res.Notes)
		}
	})
}

// NetBox ignores parameters it does not implement. On a release without cursor
// pagination the request comes back as an ordinary counted page — which is the
// old behaviour exactly, and the count is real, so it must be reported.
func TestQuery_CursorPaging_DegradesToCountedOnAnOlderNetBox(t *testing.T) {
	nb := newRecordingNetBox(t, 10, intp(42))
	nb.honorStart = false
	res, err := nb.provider(WithCursorPaging(true)).Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/devices", AllowUncounted: true, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 42 {
		t.Errorf("Total = %d, want 42: a counted envelope is a counted answer whatever we asked for", res.Total)
	}
	if len(res.Notes) != 0 {
		t.Errorf("nothing was lost, so nothing to note, got %v", res.Notes)
	}
}

// --- (c) fail properly on an unfiltered enormous query ----------------------

func TestQuery_HugeUnfilteredResult_TellsTheUserToFilter(t *testing.T) {
	t.Run("huge and unfiltered", func(t *testing.T) {
		nb := newRecordingNetBox(t, 10, intp(12936080))
		res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/interfaces", Limit: 10,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(res.Notes) != 1 {
			t.Fatalf("want one note, got %v", res.Notes)
		}
		if !strings.Contains(res.Notes[0], "12,936,080") || !strings.Contains(res.Notes[0], "Add a filter") {
			t.Errorf("note %q must name the number and the remedy", res.Notes[0])
		}
	})

	t.Run("huge but filtered", func(t *testing.T) {
		nb := newRecordingNetBox(t, 10, intp(12936080))
		res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/interfaces",
			Filters:    []provider.Filter{{Field: "device", Value: "leaf-01"}},
			Limit:      10,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(res.Notes) != 0 {
			t.Errorf("the user already filtered; nothing to advise, got %v", res.Notes)
		}
	})

	t.Run("a filter row with no value is not a filter", func(t *testing.T) {
		nb := newRecordingNetBox(t, 10, intp(12936080))
		res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
			ObjectType: "dcim/interfaces",
			Filters:    []provider.Filter{{Field: "device", Value: "  "}},
			Limit:      10,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(res.Notes) != 1 {
			t.Fatalf("an empty filter narrows nothing, so the advice stands: %v", res.Notes)
		}
	})

	// The demo instance holds 15 devices. The threshold has to be far enough
	// above every real small-instance size that no browse of one can trip it.
	t.Run("a small instance never sees it", func(t *testing.T) {
		for _, total := range []int{0, 15, 23, 1000, 82372, hugeUnfilteredTotal - 1} {
			nb := newRecordingNetBox(t, 10, intp(total))
			res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
				ObjectType: "dcim/devices", Limit: 10,
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(res.Notes) != 0 {
				t.Errorf("total=%d produced %v, want no notes", total, res.Notes)
			}
		}
	})
}

// A count query renders a number, not a table, so there is no place to put a
// note — and its whole job is to answer the very question the note would raise.
func TestQuery_CountOnly_CarriesNoScaleNote(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(12936080))
	res, err := nb.provider().Query(context.Background(), provider.QuerySpec{
		ObjectType: "dcim/interfaces", CountOnly: true, Limit: 1,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Notes) != 0 {
		t.Errorf("count query notes = %v, want none", res.Notes)
	}
}

// Everything that is not the object table still walks NetBox in offset mode,
// because each of them reads the count for something: FieldValues compares the
// sample against it to decide whether the sample IS the population, utilization
// sums envelope counts, and Changes sorts with ?ordering= — which cursor mode
// rejects with a 400.
func TestNonQueryPaths_NeverUseCursorPaging(t *testing.T) {
	nb := newRecordingNetBox(t, 1, intp(5))
	nb.object = `{"id":1,"name":"a","site":{"id":1,"name":"AMS1","slug":"ams1"}}`
	p := nb.provider(WithCursorPaging(true))
	ctx := context.Background()

	if _, err := p.Fields(ctx, "dcim/devices"); err != nil {
		t.Fatalf("Fields: %v", err)
	}
	if _, err := p.FieldValues(ctx, "dcim/devices", "site", "", 10); err != nil {
		t.Fatalf("FieldValues: %v", err)
	}
	if _, err := p.Changes(ctx, provider.ChangeSpec{Limit: 10}); err != nil {
		t.Fatalf("Changes: %v", err)
	}
	for i, q := range nb.requests {
		if q.Has(startParam) {
			t.Errorf("request %d used cursor paging: %v", i, q)
		}
	}
}

// listPage.total() is the only way a count reaches a caller, and the whole point
// of it is that "not counted" cannot be read as "counted zero".
func TestListPageTotal_DistinguishesNullFromZero(t *testing.T) {
	var nullCount, zeroCount listPage
	if err := json.Unmarshal([]byte(`{"count":null,"results":[]}`), &nullCount); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"count":0,"results":[]}`), &zeroCount); err != nil {
		t.Fatal(err)
	}
	if _, known := nullCount.total(); known {
		t.Error("a null count must not be reported as known")
	}
	if n, known := zeroCount.total(); !known || n != 0 {
		t.Errorf("a zero count must be reported as a known zero, got %d known=%v", n, known)
	}
}
