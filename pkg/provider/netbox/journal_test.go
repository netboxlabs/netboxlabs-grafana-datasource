package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// journalRow is one journal entry as NetBox serves it.
type journalRow struct {
	typ     string
	objID   int64
	display string // "" = assigned_object is null
	minutes int    // before scopedNow
	by      int64  // 0 = no author
	kind    string
	text    string
}

func (r journalRow) json() string {
	obj := "null"
	if r.display != "" {
		obj = fmt.Sprintf(`{"id":%d,"display":%q}`, r.objID, r.display)
	}
	by := "null"
	if r.by != 0 {
		by = strconv.FormatInt(r.by, 10)
	}
	kind := r.kind
	if kind == "" {
		kind = "info"
	}
	return fmt.Sprintf(`{"id":1,"assigned_object_type":%q,"assigned_object_id":%d,"assigned_object":%s,"created":%q,"created_by":%s,"kind":{"value":%q,"label":%q},"comments":%q,"display_url":"http://nb/extras/journal-entries/1/"}`,
		r.typ, r.objID, obj, scopedNow.Add(-time.Duration(r.minutes)*time.Minute).Format(time.RFC3339), by, kind, kind, r.text)
}

// journalServer serves the journal, the users, the object-type list and a
// change log of fixed rows. It ignores every journal filter, so the tests see
// what the local check does on its own, and records each request.
type journalServer struct {
	rows          []journalRow
	changes       []loggedChange
	users         map[int64]string
	journalStatus int // non-zero: the journal answers this status
	usersStatus   int
	typesStatus   int
	branches      string
	usersHandle   func() // non-nil: runs before users/users answers

	mu       sync.Mutex
	requests []string // path?query
	headers  []string
}

func (s *journalServer) start(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
		if path == "plugins/branching/branches" {
			if s.branches == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprint(w, s.branches)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.Path+"?"+r.URL.RawQuery)
		s.headers = append(s.headers, r.Header.Get("X-NetBox-Branch"))
		s.mu.Unlock()
		refuse := func(status int) {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `{"detail":"nope"}`)
		}
		switch path {
		case "core/object-types":
			if s.typesStatus != 0 {
				refuse(s.typesStatus)
				return
			}
			var rows []string
			for ct, id := range scopedTypeIDs {
				rows = append(rows, objectTypeRow(id, ct, ""))
			}
			writePage(w, len(rows), rows)
		case "extras/journal-entries":
			if s.journalStatus != 0 {
				refuse(s.journalStatus)
				return
			}
			rows := make([]string, len(s.rows))
			for i, row := range s.rows {
				rows[i] = row.json()
			}
			writePage(w, len(rows), rows)
		case "users/users":
			if s.usersHandle != nil {
				s.usersHandle()
			}
			if s.usersStatus != 0 {
				refuse(s.usersStatus)
				return
			}
			var rows []string
			for _, id := range r.URL.Query()["id"] {
				n, _ := strconv.ParseInt(id, 10, 64)
				if name, ok := s.users[n]; ok {
					rows = append(rows, fmt.Sprintf(`{"id":%d,"username":%q}`, n, name))
				}
			}
			writePage(w, len(rows), rows)
		case "core/object-changes":
			rows := make([]string, len(s.changes))
			for i, c := range s.changes {
				rows[i] = c.json()
			}
			writePage(w, len(rows), rows)
		default:
			refuse(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", srv.Client())
}

// journalReads returns the query of every journal request.
func (s *journalServer) journalReads() []url.Values { return s.reads("/api/extras/journal-entries/") }
func (s *journalServer) userReads() []url.Values    { return s.reads("/api/users/users/") }

func (s *journalServer) reads(path string) []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []url.Values
	for _, req := range s.requests {
		p, raw, _ := strings.Cut(req, "?")
		if p == path {
			q, _ := url.ParseQuery(raw)
			out = append(out, q)
		}
	}
	return out
}

func journalSpec() provider.ChangeSpec {
	return provider.ChangeSpec{
		From:    scopedNow.Add(-time.Hour),
		To:      scopedNow,
		Journal: true,
		Limit:   10,
	}
}

func journalScope() *resolvedScope {
	return &resolvedScope{Type: "dcim.device", TypeID: 12, Objects: []scopeObject{{ID: 17, Name: "leaf1"}, {ID: 18, Name: "leaf2"}}}
}

func journalOf(t *testing.T, p *Provider, spec provider.ChangeSpec, scope *resolvedScope, wanted ...string) []provider.Change {
	t.Helper()
	out, err := p.journalEntries(context.Background(), spec, scope, normalizeObjectTypes(wanted), spec.Limit)
	if err != nil {
		t.Fatalf("journalEntries: %v", err)
	}
	return out
}

func TestJournal_ScopedFilters(t *testing.T) {
	s := &journalServer{rows: []journalRow{
		{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 1},
		{typ: "dcim.device", objID: 99, display: "other", minutes: 2},   // ignored filter: id not in scope
		{typ: "dcim.interface", objID: 17, display: "eth0", minutes: 3}, // ignored filter: wrong type, same id
		{typ: "dcim.device", objID: 18, display: "leaf2", minutes: 4},
	}}
	got := journalOf(t, s.start(t), journalSpec(), journalScope())
	var titles []string
	for _, c := range got {
		titles = append(titles, c.ObjectRepr)
	}
	if want := []string{"leaf1", "leaf2"}; !reflect.DeepEqual(titles, want) {
		t.Errorf("titles = %v, want %v", titles, want)
	}
	reads := s.journalReads()
	if len(reads) != 1 {
		t.Fatalf("journal reads = %d, want 1", len(reads))
	}
	q := reads[0]
	for param, want := range map[string][]string{
		"assigned_object_type_id": {"12"},
		"assigned_object_id":      {"17", "18"},
		"ordering":                {"-created"},
		"created_after":           {scopedNow.Add(-time.Hour).Format(time.RFC3339)},
		"created_before":          {scopedNow.Format(time.RFC3339)},
		"limit":                   {"10"},
	} {
		if !reflect.DeepEqual(q[param], want) {
			t.Errorf("%s = %v, want %v", param, q[param], want)
		}
	}
	if q.Has("assigned_object_type") {
		t.Errorf("assigned_object_type sent beside the id form: %v", q)
	}
}

func TestJournal_ScopedByNameWhenTypeIDUnknown(t *testing.T) {
	s := &journalServer{}
	scope := journalScope()
	scope.TypeID = 0
	journalOf(t, s.start(t), journalSpec(), scope)
	q := s.journalReads()[0]
	if q.Get("assigned_object_type") != "dcim.device" || q.Has("assigned_object_type_id") {
		t.Errorf("query = %v, want the name form only", q)
	}
}

func TestJournal_UnscopedObjectTypesUpstream(t *testing.T) {
	s := &journalServer{rows: []journalRow{
		{typ: "dcim.site", objID: 1, display: "ams1", minutes: 1},
		{typ: "dcim.device", objID: 2, display: "leaf1", minutes: 2},
		{typ: "dcim.cable", objID: 3, display: "c3", minutes: 3}, // ignored filter
	}}
	got := journalOf(t, s.start(t), journalSpec(), nil, " DCIM.Site", "dcim.device")
	if len(got) != 2 || got[0].ObjectRepr != "ams1" || got[1].ObjectRepr != "leaf1" {
		t.Errorf("entries = %+v", got)
	}
	ids := s.journalReads()[0]["assigned_object_type_id"]
	slices.Sort(ids)
	if want := []string{"12", "6"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("assigned_object_type_id = %v, want %v", ids, want)
	}
}

func TestJournal_UnscopedNoTypesSendsNoTypeFilter(t *testing.T) {
	s := &journalServer{}
	journalOf(t, s.start(t), journalSpec(), nil)
	q := s.journalReads()[0]
	if q.Has("assigned_object_type_id") || q.Has("assigned_object_type") {
		t.Errorf("query = %v", q)
	}
	for _, req := range s.requests {
		if strings.Contains(req, "object-types") {
			t.Errorf("object-type map read without a type filter: %s", req)
		}
	}
}

func TestJournal_DegradedMapStillNonWidening(t *testing.T) {
	s := &journalServer{typesStatus: http.StatusForbidden, rows: []journalRow{
		{typ: "dcim.site", objID: 1, display: "ams1", minutes: 1},
		{typ: "dcim.cable", objID: 3, display: "c3", minutes: 2},
		{typ: "dcim.device", objID: 2, display: "leaf1", minutes: 3},
	}}
	p := s.start(t)
	got := journalOf(t, p, journalSpec(), nil, "dcim.site", "dcim.device")
	if len(got) != 2 || got[0].ObjectRepr != "ams1" || got[1].ObjectRepr != "leaf1" {
		t.Errorf("entries = %+v", got)
	}
	q := s.journalReads()[0]
	if q.Has("assigned_object_type_id") || q.Has("assigned_object_type") {
		t.Errorf("several types pushed without the map: %v", q)
	}

	// One type goes by name.
	journalOf(t, p, journalSpec(), nil, "dcim.site")
	if got := s.journalReads()[1].Get("assigned_object_type"); got != "dcim.site" {
		t.Errorf("assigned_object_type = %q, want dcim.site", got)
	}
}

func TestJournal_SkipsWhenObjectTypesExcludeScopeType(t *testing.T) {
	s := &journalServer{rows: []journalRow{{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 1}}}
	got := journalOf(t, s.start(t), journalSpec(), journalScope(), "dcim.site")
	if len(got) != 0 {
		t.Errorf("entries = %+v, want none", got)
	}
	if n := len(s.journalReads()); n != 0 {
		t.Errorf("journal reads = %d, want 0", n)
	}
}

func TestJournal_MarkerFields(t *testing.T) {
	s := &journalServer{
		users: map[int64]string{5: "alice"},
		rows: []journalRow{
			{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 5, by: 5, kind: "warning", text: "swapped the fan"},
			{typ: "dcim.device", objID: 18, minutes: 6}, // assigned_object is null
		},
	}
	got := journalOf(t, s.start(t), journalSpec(), nil)
	if len(got) != 2 {
		t.Fatalf("entries = %+v", got)
	}
	want := provider.Change{
		Time:        scopedNow.Add(-5 * time.Minute),
		Kind:        provider.ChangeKindJournal,
		ObjectType:  "dcim.device",
		ObjectRepr:  "leaf1",
		JournalKind: "warning",
		Comments:    "swapped the fan",
		User:        "alice",
		URL:         "http://nb/extras/journal-entries/1/",
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("marker = %+v, want %+v", got[0], want)
	}
	if got[1].ObjectRepr != "dcim.device #18" || got[1].User != "" {
		t.Errorf("fallback marker = %+v", got[1])
	}
}

func TestJournal_AuthorBatchedLookup(t *testing.T) {
	s := &journalServer{
		users: map[int64]string{1: "a", 2: "b", 3: "c"},
		rows: []journalRow{
			{typ: "dcim.site", objID: 1, display: "s1", minutes: 1, by: 1},
			{typ: "dcim.site", objID: 2, display: "s2", minutes: 2, by: 2},
			{typ: "dcim.site", objID: 3, display: "s3", minutes: 3, by: 3},
			{typ: "dcim.site", objID: 4, display: "s4", minutes: 4, by: 1},
		},
	}
	got := journalOf(t, s.start(t), journalSpec(), nil)
	reads := s.userReads()
	if len(reads) != 1 {
		t.Fatalf("user reads = %d, want 1", len(reads))
	}
	ids := reads[0]["id"]
	slices.Sort(ids)
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("id = %v, want %v", ids, want)
	}
	if reads[0].Get("brief") != "true" {
		t.Errorf("brief = %q, want true", reads[0].Get("brief"))
	}
	var users []string
	for _, c := range got {
		users = append(users, c.User)
	}
	if want := []string{"a", "b", "c", "a"}; !reflect.DeepEqual(users, want) {
		t.Errorf("users = %v, want %v", users, want)
	}
}

func TestJournal_AuthorLookupFailureOmitsUser(t *testing.T) {
	s := &journalServer{usersStatus: http.StatusForbidden, rows: []journalRow{
		{typ: "dcim.site", objID: 1, display: "s1", minutes: 1, by: 1, text: "hi"},
	}}
	got := journalOf(t, s.start(t), journalSpec(), nil)
	if len(got) != 1 || got[0].User != "" || got[0].Comments != "hi" {
		t.Errorf("entries = %+v", got)
	}
}

func TestJournal_ForbiddenIsAnnotationError(t *testing.T) {
	s := &journalServer{journalStatus: http.StatusForbidden}
	_, err := s.start(t).Changes(context.Background(), journalSpec())
	ae, ok := err.(*AnnotationError)
	if !ok || ae.Reason != provider.AnnotationReasonJournalForbidden {
		t.Fatalf("err = %#v, want a journal-forbidden AnnotationError", err)
	}
}

func TestJournal_OtherErrorFailsTheQuery(t *testing.T) {
	s := &journalServer{journalStatus: http.StatusInternalServerError}
	_, err := s.start(t).Changes(context.Background(), journalSpec())
	if err == nil {
		t.Fatal("want an error")
	}
	if _, ok := err.(*AnnotationError); ok {
		t.Errorf("a 500 became an AnnotationError: %v", err)
	}
}

func TestJournal_ActionsAndChangedFieldsDontApply(t *testing.T) {
	s := &journalServer{rows: []journalRow{{typ: "dcim.site", objID: 1, display: "s1", minutes: 1}}}
	spec := journalSpec()
	spec.Actions = []string{"delete"}
	spec.ChangedFields = []string{"status"}
	got := journalOf(t, s.start(t), spec, nil)
	if len(got) != 1 {
		t.Errorf("entries = %+v, want the entry", got)
	}
	q := s.journalReads()[0]
	if q.Has("action") || q.Has("kind") {
		t.Errorf("query = %v", q)
	}
}

func TestJournal_OffMakesNoRequest(t *testing.T) {
	s := &journalServer{rows: []journalRow{{typ: "dcim.site", objID: 1, display: "s1", minutes: 1}}}
	spec := journalSpec()
	spec.Journal = false
	changesOK(t, s.start(t), spec)
	if n := len(s.journalReads()) + len(s.userReads()); n != 0 {
		t.Errorf("journal or user requests = %d, want 0", n)
	}
}

func TestJournal_CarriesBranch(t *testing.T) {
	s := &journalServer{
		branches: `{"count":1,"next":null,"results":[{"name":"br1","schema_id":"td5smq0f"}]}`,
		users:    map[int64]string{5: "alice"},
		rows:     []journalRow{{typ: "dcim.site", objID: 1, display: "s1", minutes: 1, by: 5}},
	}
	_, err := s.start(t).Changes(provider.WithBranch(context.Background(), "br1"), journalSpec())
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seenJournal, seenUsers := false, false
	for i, req := range s.requests {
		seenJournal = seenJournal || strings.HasPrefix(req, "/api/extras/journal-entries/")
		seenUsers = seenUsers || strings.HasPrefix(req, "/api/users/users/")
		if s.headers[i] != "td5smq0f" {
			t.Errorf("%s sent X-NetBox-Branch %q, want td5smq0f", req, s.headers[i])
		}
	}
	if !seenJournal || !seenUsers {
		t.Errorf("journal read = %v, user read = %v", seenJournal, seenUsers)
	}
}

func TestChanges_JournalMergedUnderLimit(t *testing.T) {
	s := &journalServer{
		changes: []loggedChange{ch(1, 1, "dcim.site", 1), ch(2, 4, "dcim.site", 1), ch(3, 6, "dcim.site", 1)},
		rows: []journalRow{
			{typ: "dcim.site", objID: 1, display: "j2", minutes: 2},
			{typ: "dcim.site", objID: 1, display: "j5", minutes: 5},
		},
	}
	spec := journalSpec()
	spec.Limit = 4
	got := changesOK(t, s.start(t), spec)
	wantIDs(t, got, "#1", "j2", "#2", "j5")
	if got[1].Kind != provider.ChangeKindJournal {
		t.Errorf("second marker kind = %v", got[1].Kind)
	}
}

func TestWithBranchParam(t *testing.T) {
	tests := []struct{ name, in, id, want string }{
		{"empty url", "", "4qv48we9", ""},
		{"plain", "http://nb/core/changelog/455/", "4qv48we9", "http://nb/core/changelog/455/?_branch=4qv48we9"},
		{"keeps query and fragment", "http://nb/dcim/devices/1/?a=1#frag", "4qv48we9", "http://nb/dcim/devices/1/?_branch=4qv48we9&a=1#frag"},
		{"replaces an existing param", "http://nb/x/?_branch=old", "4qv48we9", "http://nb/x/?_branch=4qv48we9"},
		{"main leaves it alone", "http://nb/x/?a=1", "", "http://nb/x/?a=1"},
		{"unparsable is unchanged", "http://nb/%zz", "4qv48we9", "http://nb/%zz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := withBranchParam(tc.in, tc.id); got != tc.want {
				t.Errorf("withBranchParam(%q, %q) = %q, want %q", tc.in, tc.id, got, tc.want)
			}
		})
	}
}

func TestChanges_BranchLinksCarryBranchParam(t *testing.T) {
	s := &journalServer{
		branches: `{"count":1,"next":null,"results":[{"name":"feature","schema_id":"4qv48we9"}]}`,
		changes:  []loggedChange{ch(455, 2, "dcim.device", 17)},
		rows:     []journalRow{{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 1}},
	}
	p := s.start(t)
	run := func(branch string) []provider.Change {
		t.Helper()
		ctx := provider.WithBranch(context.Background(), branch)
		got, err := p.Changes(ctx, journalSpec())
		if err != nil {
			t.Fatalf("Changes(%q): %v", branch, err)
		}
		return got
	}

	// A branch name resolves to its schema id, which is what the link carries.
	got := run("feature")
	kinds := map[provider.ChangeKind]bool{}
	for _, c := range got {
		kinds[c.Kind] = true
		if !strings.Contains(c.URL, "_branch=4qv48we9") {
			t.Errorf("%s marker URL = %q, want _branch=4qv48we9", c.Kind, c.URL)
		}
	}
	if !kinds[provider.ChangeKindChange] || !kinds[provider.ChangeKindJournal] {
		t.Fatalf("markers = %+v, want one change and one journal marker", got)
	}

	for _, branch := range []string{"", "main"} {
		for _, c := range run(branch) {
			if strings.Contains(c.URL, "_branch") {
				t.Errorf("branch %q: %s marker URL = %q, want no _branch", branch, c.Kind, c.URL)
			}
		}
	}
}

func TestJournal_AuthorLookupIsBounded(t *testing.T) {
	release := make(chan struct{})
	s := &journalServer{
		rows:        []journalRow{{typ: "dcim.site", objID: 1, display: "s1", minutes: 1, by: 5}},
		usersHandle: func() { <-release },
	}
	p := s.start(t)
	// Registered after start's srv.Close, so it runs first: Close waits for
	// handlers, and this one only returns once released.
	t.Cleanup(func() { close(release) })
	p.namesWaitOverride = 50 * time.Millisecond

	done := make(chan []provider.Change, 1)
	go func() { done <- journalOf(t, p, journalSpec(), nil) }()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].User != "" {
			t.Errorf("markers = %+v, want one marker with no author", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("journalEntries still blocked after 2s: the author lookup is not bounded by namesWait (%v)", p.namesWaitOverride)
	}
}
