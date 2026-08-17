package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// demoObjectTypeIDs are the real /api/core/object-types/ ids of the demo
// instance (NetBox 4.4.10, 154 rows). They are used verbatim so the fixture's
// arithmetic is the arithmetic that was measured live:
//
//	?changed_object_type_id=6   (dcim.site)      -> count=3
//	?changed_object_type_id=86  (ipam.fhrpgroup) -> count=14
//	both repeated                                -> count=17   (OR, 3 + 14)
//	?changed_object_type_id=99999                -> HTTP 400 "Select a valid choice."
var demoObjectTypeIDs = map[string]int{
	"dcim.site":      6,
	"dcim.device":    12,
	"ipam.fhrpgroup": 86,
	"ipam.ipaddress": 95,
}

// changeLogServer reproduces the upstream behaviours this file is about, all
// measured against the demo instance (NetBox 4.4.10) with curl:
//
//   - ?changed_object_type_id= is MULTI-valued and ORs its values, and rejects an
//     id the instance does not know with HTTP 400 rather than an empty page.
//   - ?changed_object_type= (the name form) is SINGLE-valued there, so a repeated
//     parameter is LAST WINS, not OR: ?…=dcim.site&…=ipam.fhrpgroup returned
//     count=14 (the fhrpgroup total) and the same pair reversed returned count=3
//     (the site total). That is why the name form is only ever used as the
//     single-type fallback, and the fixture keeps its broken semantics so a
//     regression back to it cannot pass the multi-type test.
//   - /api/core/object-types/ answers the name -> id map (154 rows).
//
// The default shape is the NEWER one, and deliberately so: /api/core/… answers
// and the legacy /api/extras/object-types/ alias 404s, which is what NetBox 4.6
// does (its extras/api/urls.py no longer registers object-types; 4.4.10 still
// does, and there both paths return the same 154 rows with the same canonical
// self-URL, …/api/core/object-types/33/). A fixture that served only the legacy
// path would certify code that cannot work on 4.6 at all — see
// TestChanges_ResolvesIDsWithoutTheLegacyExtrasAlias.
//
// Records are stored newest-first; ?ordering=-time is the only ordering the
// caller sends, so the handler serves them in that order.
type changeLogServer struct {
	records []string // JSON objects, newest first

	// ignoreTypeFilter makes the handler behave like an instance that does not
	// implement the parameter at all, which is how the local type filter is
	// exercised on its own.
	ignoreTypeFilter bool

	// objectTypesStatus, when non-zero, is the HTTP status /api/core/object-types/
	// answers with instead of the map — the "cannot resolve" degradation (network
	// blip, or a token whose permissions exclude the endpoint).
	objectTypesStatus int

	// legacyAliasServes makes /api/extras/object-types/ answer the map too, which
	// is the 4.4.x shape. It must change nothing: the canonical path is the one
	// the provider asks for on every version.
	legacyAliasServes bool

	// objectTypesDelay holds the object-type handler open, so that concurrent
	// annotation refreshes really do overlap inside it.
	objectTypesDelay time.Duration

	// entered, when non-nil, is closed as the FIRST object-type request arrives —
	// before objectTypesDelay is slept. It lets a test place a second caller
	// provably behind an in-flight fetch rather than racing one.
	entered     chan struct{}
	enteredOnce sync.Once

	mu                 sync.Mutex
	requests           []url.Values
	objectTypeGE       int // GETs served by /api/core/object-types/
	legacyObjectTypeGE int // GETs served by /api/extras/object-types/ (the 4.4-only alias)
}

// serveObjectTypes changes what /api/core/object-types/ answers with, mid-test:
// 0 for the map, an HTTP status for a refusal. It exists so a test can let a
// transient failure PASS, which is the difference between a bounded failure
// window and a cache that disables annotations until restart.
func (s *changeLogServer) serveObjectTypes(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objectTypesStatus = status
}

func changeRecord(objectType, repr string, minutesAgo int) string {
	return fmt.Sprintf(
		`{"time":"2026-08-13T%02d:00:00Z","user_name":"admin","action":{"value":"update","label":"Updated"},"changed_object_type":%q,"object_repr":%q,"display_url":"http://nb/extras/changelog/1/"}`,
		23-minutesAgo, objectType, repr)
}

func (s *changeLogServer) start(t *testing.T) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	writeObjectTypes := func(w http.ResponseWriter) {
		if s.entered != nil {
			s.enteredOnce.Do(func() { close(s.entered) })
		}
		if s.objectTypesDelay > 0 {
			time.Sleep(s.objectTypesDelay)
		}
		s.mu.Lock()
		status := s.objectTypesStatus
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `{"detail":"nope"}`)
			return
		}
		names := make([]string, 0, len(demoObjectTypeIDs))
		for name := range demoObjectTypeIDs {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([]string, 0, len(names))
		for _, name := range names {
			app, model, _ := strings.Cut(name, ".")
			// The canonical self-URL is core/ on 4.4.10 even when the row is read
			// through the extras alias, which is why extras is the legacy spelling.
			rows = append(rows, fmt.Sprintf(`{"id":%d,"url":"http://nb/api/core/object-types/%d/","app_label":%q,"model":%q}`,
				demoObjectTypeIDs[name], demoObjectTypeIDs[name], app, model))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, len(rows), strings.Join(rows, ","))
	}
	mux.HandleFunc("/api/core/object-types/", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.objectTypeGE++
		s.mu.Unlock()
		writeObjectTypes(w)
	})
	// The 4.4-only alias. It 404s unless a test asks for the older shape, so any
	// code that depends on it fails here the way it fails on NetBox 4.6.
	mux.HandleFunc("/api/extras/object-types/", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.legacyObjectTypeGE++
		serves := s.legacyAliasServes
		s.mu.Unlock()
		if !serves {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"detail":"Not found."}`)
			return
		}
		writeObjectTypes(w)
	})
	mux.HandleFunc("/api/core/object-changes/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		s.requests = append(s.requests, q)
		s.mu.Unlock()

		typeOf := func(rec string) string {
			var oc objectChange
			if err := json.Unmarshal([]byte(rec), &oc); err != nil {
				t.Errorf("fixture record is not valid JSON: %v", err)
				return ""
			}
			return strings.ToLower(oc.ChangedType)
		}

		rows := s.records
		if vals := q["changed_object_type_id"]; len(vals) > 0 && !s.ignoreTypeFilter {
			want := map[string]bool{} // OR over every value, as measured
			for _, v := range vals {
				id, err := strconv.Atoi(v)
				if err != nil {
					http.Error(w, `{"changed_object_type_id":["Enter a number."]}`, http.StatusBadRequest)
					return
				}
				name := ""
				for n, known := range demoObjectTypeIDs {
					if known == id {
						name = n
					}
				}
				if name == "" {
					http.Error(w, fmt.Sprintf(`{"changed_object_type_id":["Select a valid choice. %d is not one of the available choices."]}`, id), http.StatusBadRequest)
					return
				}
				want[name] = true
			}
			var kept []string
			for _, rec := range rows {
				if want[typeOf(rec)] {
					kept = append(kept, rec)
				}
			}
			rows = kept
		}
		if vals := q["changed_object_type"]; len(vals) > 0 && !s.ignoreTypeFilter {
			want := strings.ToLower(vals[len(vals)-1]) // last wins, as measured
			var kept []string
			for _, rec := range rows {
				if typeOf(rec) == want {
					kept = append(kept, rec)
				}
			}
			rows = kept
		}
		total := len(rows)
		if off, err := strconv.Atoi(q.Get("offset")); err == nil && off > 0 {
			rows = rows[min(off, len(rows)):]
		}
		if lim, err := strconv.Atoi(q.Get("limit")); err == nil && lim >= 0 && lim < len(rows) {
			rows = rows[:lim]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"results":[%s]}`, total, strings.Join(rows, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", srv.Client())
}

// sentParam returns the values of one query parameter as the first change-log
// request carried them, and fails if no request was made at all.
func (s *changeLogServer) sentParam(t *testing.T, name string) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("no request reached the change-log endpoint")
	}
	return s.requests[0][name]
}

func (s *changeLogServer) changeLogRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *changeLogServer) objectTypeRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objectTypeGE
}

// legacyObjectTypeRequests counts hits on the 4.4-only /api/extras/ alias.
func (s *changeLogServer) legacyObjectTypeRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legacyObjectTypeGE
}

// The headline case, reproduced from the 38.8M-row instance in the design note:
// every row inside the requested limit is some OTHER object type, so filtering
// after the fetch returns an empty annotation track while changes of the asked-for
// type exist a few rows further down. The answer is wrong, not slow.
func TestChanges_PushesTypeIntoTheQuery(t *testing.T) {
	s := &changeLogServer{records: []string{
		changeRecord("ipam.ipaddress", "10.0.0.1/24", 0),
		changeRecord("ipam.ipaddress", "10.0.0.2/24", 1),
		changeRecord("ipam.ipaddress", "10.0.0.3/24", 2),
		changeRecord("dcim.device", "leaf1", 3),
		changeRecord("dcim.device", "leaf2", 4),
	}}
	p := s.start(t)

	changes, err := p.Changes(context.Background(), provider.ChangeSpec{
		ObjectTypes: []string{"dcim.device"}, Limit: 3,
	})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if got := s.sentParam(t, "changed_object_type_id"); len(got) != 1 || got[0] != "12" {
		t.Errorf("changed_object_type_id sent = %v, want [12] (dcim.device)", got)
	}
	if got := s.sentParam(t, "changed_object_type"); len(got) != 0 {
		t.Errorf("name-form parameter sent = %v, want none — the id form is the portable one", got)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2 (both device changes lie outside the newest 3 rows)", len(changes))
	}
	for _, c := range changes {
		if c.ObjectType != "dcim.device" {
			t.Errorf("unexpected type in frame: %q", c.ObjectType)
		}
	}
}

// The multi-type case the name form could not express. Fixture sizes are the
// demo's own: 3 dcim.site changes and 14 ipam.fhrpgroup changes, which returned
// 17 when both ids were sent and 3 or 14 (whichever came last) with the name
// form. Anything but 17 here is a silently dropped type.
func TestChanges_PushesEveryRequestedTypeAsIDs(t *testing.T) {
	var records []string
	for i := 0; i < 3; i++ {
		records = append(records, changeRecord("dcim.site", fmt.Sprintf("site%d", i), i))
	}
	for i := 0; i < 14; i++ {
		records = append(records, changeRecord("ipam.fhrpgroup", fmt.Sprintf("group%d", i), i))
	}
	for i := 0; i < 5; i++ {
		records = append(records, changeRecord("dcim.device", fmt.Sprintf("leaf%d", i), i))
	}
	s := &changeLogServer{records: records}
	p := s.start(t)

	changes, err := p.Changes(context.Background(), provider.ChangeSpec{
		ObjectTypes: []string{"dcim.site", "ipam.fhrpgroup"}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	got := append([]string(nil), s.sentParam(t, "changed_object_type_id")...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "6" || got[1] != "86" {
		t.Errorf("changed_object_type_id sent = %v, want [6 86] (dcim.site, ipam.fhrpgroup)", got)
	}
	if len(changes) != 17 {
		t.Fatalf("changes = %d, want 17 (3 sites OR 14 fhrpgroups — the measured total)", len(changes))
	}
	counts := map[string]int{}
	for _, c := range changes {
		counts[c.ObjectType]++
	}
	if counts["dcim.site"] != 3 || counts["ipam.fhrpgroup"] != 14 {
		t.Errorf("per-type counts = %v, want 3 sites and 14 fhrpgroups", counts)
	}
}

// An empty parameter is ignored upstream (measured: count=274, the unfiltered
// total), so sending one would be a meaningless parameter on every unfiltered
// annotation query — and resolving the id map would be a request bought for
// nothing.
func TestChanges_SendsNoTypeParamWhenNoneRequested(t *testing.T) {
	s := &changeLogServer{records: []string{changeRecord("dcim.device", "leaf1", 0)}}
	p := s.start(t)

	if _, err := p.Changes(context.Background(), provider.ChangeSpec{Limit: 10}); err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if got := s.sentParam(t, "changed_object_type_id"); len(got) != 0 {
		t.Errorf("changed_object_type_id sent = %q, want none", got)
	}
	if got := s.sentParam(t, "changed_object_type"); len(got) != 0 {
		t.Errorf("changed_object_type sent = %q, want none", got)
	}
	if n := s.objectTypeRequests(); n != 0 {
		t.Errorf("object-types fetched %d times for an unfiltered query, want 0", n)
	}
}

// The reason to prefer the id form: a type NetBox does not know is an ERROR the
// user sees, not an empty annotation track that looks like a quiet window. The
// name form answered dcim.nosuchmodel (and dcim.devices, the plural) with HTTP
// 200 and count=0.
func TestChanges_UnknownTypeIsAnError(t *testing.T) {
	s := &changeLogServer{records: []string{changeRecord("dcim.device", "leaf1", 0)}}
	p := s.start(t)

	_, err := p.Changes(context.Background(), provider.ChangeSpec{
		ObjectTypes: []string{"dcim.device", "dcim.devices"}, Limit: 10,
	})
	if err == nil {
		t.Fatal("Changes returned no error for an unknown object type — that is the silent zero this change removes")
	}
	if !strings.Contains(err.Error(), "dcim.devices") {
		t.Errorf("error = %q, want it to name the unknown type", err)
	}
	// TYPED, not a bare fmt.Errorf: the plugin layer classifies upstream failures
	// by error type (see queryErrorMessage), and an untyped error falls through to
	// the transport case, reporting a typo in the annotation editor as "Couldn't
	// reach NetBox".
	var unknown *UnknownObjectTypeError
	if !errors.As(err, &unknown) {
		t.Fatalf("error is %T, want *UnknownObjectTypeError so the plugin layer can tell a typo from an outage", err)
	}
	if unknown.Type != "dcim.devices" {
		t.Errorf("UnknownObjectTypeError.Type = %q, want the offending type", unknown.Type)
	}
	if unknown.Known != len(demoObjectTypeIDs) {
		t.Errorf("UnknownObjectTypeError.Known = %d, want %d (the types the instance reported)", unknown.Known, len(demoObjectTypeIDs))
	}
	if n := s.changeLogRequests(); n != 0 {
		t.Errorf("change log was queried %d times, want 0 — an unresolvable type is caught before the request", n)
	}
}

// The type is free text from the annotation editor (the MultiSelect allows
// custom values), so it reaches a log record and must not be able to forge one.
func TestChanges_UnknownTypeErrorIsSanitised(t *testing.T) {
	s := &changeLogServer{records: []string{changeRecord("dcim.device", "leaf1", 0)}}
	p := s.start(t)

	_, err := p.Changes(context.Background(), provider.ChangeSpec{
		ObjectTypes: []string{"dcim.device\nlevel=error msg=\"forged\""}, Limit: 10,
	})
	if err == nil {
		t.Fatal("Changes returned no error for an unknown object type")
	}
	if strings.ContainsAny(err.Error(), "\n\r") {
		t.Errorf("error carries a newline from user input: %q", err)
	}
}

// Case and stray whitespace must not turn a type NetBox accepts into a hard
// error: the name parameter this replaces matched case-insensitively
// (?changed_object_type=IPAM.FHRPGroup returned the same count=14) and tolerated
// surrounding whitespace, so the id lookup normalises the same way.
func TestChanges_MatchesTypeNamesCaseInsensitively(t *testing.T) {
	s := &changeLogServer{records: []string{
		changeRecord("dcim.device", "leaf1", 0),
		changeRecord("ipam.ipaddress", "10.0.0.1/24", 1),
	}}
	p := s.start(t)

	changes, err := p.Changes(context.Background(), provider.ChangeSpec{
		ObjectTypes: []string{" DCIM.Device "}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if got := s.sentParam(t, "changed_object_type_id"); len(got) != 1 || got[0] != "12" {
		t.Errorf("changed_object_type_id sent = %v, want [12] (dcim.device)", got)
	}
	if len(changes) != 1 || changes[0].ObjectType != "dcim.device" {
		t.Errorf("changes = %+v, want the one dcim.device row", changes)
	}
}

// If the id map cannot be read at all, the annotation still answers: the query
// degrades to the single-type name parameter (and, for several types, to the
// local filter alone) rather than failing outright.
func TestChanges_DegradesWhenObjectTypesCannotBeRead(t *testing.T) {
	records := []string{
		changeRecord("dcim.device", "leaf1", 0),
		changeRecord("ipam.ipaddress", "10.0.0.1/24", 1),
		changeRecord("dcim.site", "AMS1", 2),
	}

	t.Run("one type falls back to the name parameter", func(t *testing.T) {
		s := &changeLogServer{records: records, objectTypesStatus: http.StatusForbidden}
		p := s.start(t)

		changes, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if got := s.sentParam(t, "changed_object_type"); len(got) != 1 || got[0] != "dcim.device" {
			t.Errorf("changed_object_type sent = %v, want [dcim.device]", got)
		}
		if got := s.sentParam(t, "changed_object_type_id"); len(got) != 0 {
			t.Errorf("changed_object_type_id sent = %v, want none — no map, no ids", got)
		}
		if len(changes) != 1 || changes[0].ObjectType != "dcim.device" {
			t.Errorf("changes = %+v, want the one dcim.device row", changes)
		}
	})

	t.Run("several types fall back to the local filter", func(t *testing.T) {
		s := &changeLogServer{records: records, objectTypesStatus: http.StatusForbidden}
		p := s.start(t)

		changes, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device", "ipam.ipaddress"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if got := s.sentParam(t, "changed_object_type"); len(got) != 0 {
			t.Errorf("changed_object_type sent = %v, want none: repeated it is LAST WINS on 4.4.10, which would drop a type", got)
		}
		if got := s.sentParam(t, "changed_object_type_id"); len(got) != 0 {
			t.Errorf("changed_object_type_id sent = %v, want none", got)
		}
		seen := map[string]bool{}
		for _, c := range changes {
			seen[c.ObjectType] = true
		}
		if !seen["dcim.device"] || !seen["ipam.ipaddress"] || len(changes) != 2 {
			t.Errorf("changes = %+v, want one dcim.device and one ipam.ipaddress", changes)
		}
	})

	t.Run("an unknown type is not an error while the map is unreadable", func(t *testing.T) {
		s := &changeLogServer{records: records, objectTypesStatus: http.StatusForbidden}
		p := s.start(t)

		if _, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.nosuchmodel"}, Limit: 10,
		}); err != nil {
			t.Fatalf("Changes: %v — an unreadable map must not turn every type into an error", err)
		}
	})
}

// The map is 154 rows that change only when models are added, so it is fetched
// once per TTL and not once per annotation refresh.
func TestChanges_CachesTheObjectTypeMap(t *testing.T) {
	s := &changeLogServer{records: []string{changeRecord("dcim.device", "leaf1", 0)}}
	p := s.start(t)

	for i := 0; i < 3; i++ {
		if _, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device"}, Limit: 10,
		}); err != nil {
			t.Fatalf("Changes: %v", err)
		}
	}
	if n := s.objectTypeRequests(); n != 1 {
		t.Errorf("object-types fetched %d times for 3 annotation queries, want 1", n)
	}
}

// The whole reason for the id form is that it works on every supported version,
// so it must be read from the path every supported version serves.
//
// /api/core/object-types/ is that path. /api/extras/object-types/ is a 4.4-era
// alias: measured, 4.4.10 answers both with the same 154 rows and the same
// canonical self-URL (…/api/core/object-types/33/), while NetBox 4.6.0 registers
// object-types only under core (extras/api/urls.py no longer registers it) and
// 404s the extras path. Reading the legacy path would therefore make the map
// unreadable on 4.6+, degrading every annotation to exactly the silently-empty
// track this pushdown exists to remove — and, because only successful fetches
// are cached, paying a fresh doomed request on every refresh.
//
// The demo cannot catch this, since there the alias still works; only a fixture
// shaped like the newer server can.
func TestChanges_ResolvesIDsWithoutTheLegacyExtrasAlias(t *testing.T) {
	records := []string{
		changeRecord("dcim.device", "leaf1", 0),
		changeRecord("ipam.ipaddress", "10.0.0.1/24", 1),
	}

	t.Run("4.6 shape: only core answers", func(t *testing.T) {
		s := &changeLogServer{records: records} // extras 404s
		p := s.start(t)

		changes, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if got := s.sentParam(t, "changed_object_type_id"); len(got) != 1 || got[0] != "12" {
			t.Errorf("changed_object_type_id sent = %v, want [12] — the map must resolve without the legacy alias", got)
		}
		if got := s.sentParam(t, "changed_object_type"); len(got) != 0 {
			t.Errorf("name-form parameter sent = %v: the map was not read, so this is the degraded path", got)
		}
		if n := s.objectTypeRequests(); n != 1 {
			t.Errorf("core/object-types fetched %d times, want 1", n)
		}
		if n := s.legacyObjectTypeRequests(); n != 0 {
			t.Errorf("the legacy extras/object-types alias was requested %d times; it does not exist on 4.6", n)
		}
		if len(changes) != 1 || changes[0].ObjectType != "dcim.device" {
			t.Errorf("changes = %+v, want the one dcim.device row", changes)
		}
	})

	t.Run("4.4 shape: the alias also answers, and is still not used", func(t *testing.T) {
		s := &changeLogServer{records: records, legacyAliasServes: true}
		p := s.start(t)

		if _, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device"}, Limit: 10,
		}); err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if got := s.sentParam(t, "changed_object_type_id"); len(got) != 1 || got[0] != "12" {
			t.Errorf("changed_object_type_id sent = %v, want [12]", got)
		}
		if n := s.legacyObjectTypeRequests(); n != 0 {
			t.Errorf("the legacy alias was requested %d times; the canonical path answers on 4.4 too", n)
		}
	})
}

// A whitespace-only type is not a filter. It used to be BOTH: skipped by the
// pushdown (so nothing went upstream) and kept by the local filter (so every row
// was discarded) — a silently empty annotation with no error anywhere, the exact
// failure mode this file exists to remove.
//
// Absent is the honest reading, and it is upstream's own: measured on the demo,
// ?changed_object_type=%20 returns count=274, the unfiltered total, exactly like
// the empty value. The two paths now share one normalisation so they cannot
// disagree again.
func TestChanges_WhitespaceOnlyTypeIsNotAFilter(t *testing.T) {
	records := []string{
		changeRecord("dcim.device", "leaf1", 0),
		changeRecord("ipam.ipaddress", "10.0.0.1/24", 1),
	}

	t.Run("alone it filters nothing", func(t *testing.T) {
		s := &changeLogServer{records: records}
		p := s.start(t)

		changes, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"   "}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if got := s.sentParam(t, "changed_object_type_id"); len(got) != 0 {
			t.Errorf("changed_object_type_id sent = %v, want none", got)
		}
		if got := s.sentParam(t, "changed_object_type"); len(got) != 0 {
			t.Errorf("changed_object_type sent = %v, want none", got)
		}
		if len(changes) != 2 {
			t.Fatalf("changes = %d, want 2 (both rows): a blank type filters nothing upstream, so it must not discard everything locally", len(changes))
		}
		if n := s.objectTypeRequests(); n != 0 {
			t.Errorf("object-types fetched %d times for a blank type, want 0 — nothing to resolve", n)
		}
	})

	t.Run("beside a real type it is ignored, not added", func(t *testing.T) {
		s := &changeLogServer{records: records}
		p := s.start(t)

		changes, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device", " "}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if got := s.sentParam(t, "changed_object_type_id"); len(got) != 1 || got[0] != "12" {
			t.Errorf("changed_object_type_id sent = %v, want [12] only", got)
		}
		if len(changes) != 1 || changes[0].ObjectType != "dcim.device" {
			t.Errorf("changes = %+v, want the one dcim.device row", changes)
		}
	})
}

// The map is fetched once per TTL, and that has to hold for SIMULTANEOUS misses
// too: a dashboard opens every annotation query at once, and a cache that is
// only checked before the fetch lets all of them through. Here the handler is
// held open long enough that unsynchronised callers would certainly overlap.
func TestChanges_ConcurrentRefreshesShareOneObjectTypeFetch(t *testing.T) {
	s := &changeLogServer{
		records:          []string{changeRecord("dcim.device", "leaf1", 0)},
		objectTypesDelay: 50 * time.Millisecond,
	}
	p := s.start(t)

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := p.Changes(context.Background(), provider.ChangeSpec{
				ObjectTypes: []string{"dcim.device"}, Limit: 10,
			}); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Changes: %v", err)
	}

	if n := s.objectTypeRequests(); n != 1 {
		t.Errorf("object-types fetched %d times for %d simultaneous annotation queries, want 1", n, callers)
	}
}

// Sharing the fetch must not mean inheriting the leader's WAIT. A caller has its
// own deadline — Grafana's query timeout, which is what an annotation panel is
// really waiting on — and queueing behind someone else's slow request must not
// spend it.
//
// Measured on the code this test was written against: a caller whose own ctx had
// a 10ms deadline, arriving behind a leader mid-fetch, returned after 451ms — 44x
// its own budget, because it was waiting on a mutex, and a mutex cannot be told
// about a context.
func TestChanges_WaiterDoesNotOutliveItsOwnDeadline(t *testing.T) {
	s := &changeLogServer{
		records:          []string{changeRecord("dcim.device", "leaf1", 0)},
		objectTypesDelay: 2 * time.Second,
		entered:          make(chan struct{}),
	}
	p := s.start(t)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device"}, Limit: 10,
		})
	}()
	<-s.entered // the leader is provably inside the object-type fetch

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Changes(ctx, provider.ChangeSpec{ObjectTypes: []string{"dcim.device"}, Limit: 10})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded: the waiter's own deadline is what ends its wait", err)
	}
	// Generous against a loaded CI box, and still two orders of magnitude below
	// the leader's fetch: the point is that the waiter did not sit out someone
	// else's request.
	if elapsed > 500*time.Millisecond {
		t.Errorf("waiter returned after %v with a 20ms deadline; it waited out the leader's fetch instead of its own budget", elapsed)
	}
	<-leaderDone
}

// A FAILING endpoint must cost one fetch per failure window, not one per caller.
//
// Measured on the code this test was written against, with the endpoint
// returning 403 behind a 200ms delay: 8 simultaneous Changes calls took 1.611s
// and issued 8 fetches, because a failure was never cached and each waiter
// re-attempted it in turn as the lock came free. Serialised retries turn a slow
// or failing endpoint into a queue — with Grafana's 30s timeout and a hanging
// endpoint, the 8th annotation query on a dashboard waits up to 8x30s.
func TestChanges_SimultaneousMissesShareAFailedFetch(t *testing.T) {
	const delay = 200 * time.Millisecond
	s := &changeLogServer{
		records:           []string{changeRecord("dcim.device", "leaf1", 0)},
		objectTypesStatus: http.StatusForbidden,
		objectTypesDelay:  delay,
	}
	p := s.start(t)

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	begin := time.Now()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// An unreadable map still degrades rather than failing the annotation,
			// so every one of these must succeed.
			if _, err := p.Changes(context.Background(), provider.ChangeSpec{
				ObjectTypes: []string{"dcim.device"}, Limit: 10,
			}); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	elapsed := time.Since(begin)
	close(errs)
	for err := range errs {
		t.Fatalf("Changes: %v", err)
	}

	if n := s.objectTypeRequests(); n != 1 {
		t.Errorf("object-types fetched %d times behind ONE failing endpoint, want 1: a failed fetch must be shared, not re-attempted per caller", n)
	}
	// One delay, not eight. Half the callers' worth of delay is already proof the
	// retries were not serial.
	if elapsed > callers/2*delay {
		t.Errorf("%d callers took %v behind a %v failure; they queued instead of sharing it", callers, elapsed, delay)
	}
}

// The other half of that policy: a shared failure must EXPIRE. A transient 503
// that disabled the id pushdown until the plugin restarted would trade one bug
// for a worse one — annotations quietly degraded for the rest of the day.
func TestChanges_AFailedFetchIsNotCachedIndefinitely(t *testing.T) {
	saved := objectTypeFailureTTL
	objectTypeFailureTTL = 50 * time.Millisecond
	t.Cleanup(func() { objectTypeFailureTTL = saved })
	savedBackoff := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryBackoff = savedBackoff })

	// A 503 is retryable, so ONE logical read is len(retryBackoff)+1 requests
	// before it gives up. Counting rounds rather than requests is what keeps this
	// test about the failure window and not about the retry layer.
	perRound := len(retryBackoff) + 1

	s := &changeLogServer{
		records:           []string{changeRecord("dcim.device", "leaf1", 0)},
		objectTypesStatus: http.StatusServiceUnavailable,
	}
	p := s.start(t)

	call := func() {
		t.Helper()
		if _, err := p.Changes(context.Background(), provider.ChangeSpec{
			ObjectTypes: []string{"dcim.device"}, Limit: 10,
		}); err != nil {
			t.Fatalf("Changes: %v", err)
		}
	}

	call()
	if n := s.objectTypeRequests(); n != perRound {
		t.Fatalf("object-types fetched %d times, want %d (one failed read)", n, perRound)
	}
	call()
	if n := s.objectTypeRequests(); n != perRound {
		t.Errorf("object-types fetched %d times, want %d: a fresh failure is shared for its window, not re-read per query", n, perRound)
	}

	// The blip passes, and so does the window.
	s.serveObjectTypes(0)
	time.Sleep(2 * objectTypeFailureTTL)

	call()
	if n := s.objectTypeRequests(); n != perRound+1 {
		t.Errorf("object-types fetched %d times after the window, want %d: the failure must not be cached indefinitely", n, perRound+1)
	}
	if got := s.sentParam(t, "changed_object_type_id"); len(got) != 0 {
		t.Errorf("the FIRST request sent changed_object_type_id = %v; it ran while the map was unreadable", got)
	}
	last := func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.requests[len(s.requests)-1]["changed_object_type_id"]
	}()
	if len(last) != 1 || last[0] != "12" {
		t.Errorf("changed_object_type_id on the recovered query = %v, want [12]: the map is readable again", last)
	}
}

// The local filter is kept so the pushdown is strictly non-widening: whatever
// upstream does with the parameter, the frame cannot contain a type the user did
// not ask for. Here upstream ignores it entirely.
func TestChanges_LocalFilterKeepsThePushdownNonWidening(t *testing.T) {
	s := &changeLogServer{
		ignoreTypeFilter: true,
		records: []string{
			changeRecord("dcim.device", "leaf1", 0),
			changeRecord("ipam.ipaddress", "10.0.0.1/24", 1),
		},
	}
	p := s.start(t)

	changes, err := p.Changes(context.Background(), provider.ChangeSpec{
		ObjectTypes: []string{"dcim.device"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].ObjectType != "dcim.device" {
		t.Errorf("changes = %+v, want only the dcim.device row", changes)
	}
}
