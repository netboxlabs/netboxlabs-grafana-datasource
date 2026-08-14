package netbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// branchResolveServer serves a branch list at the branching endpoint and echoes
// the X-NetBox-Branch header (into *got) on every other path.
func branchResolveServer(t *testing.T, got *string, branchesBody string, branchesStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/plugins/branching/branches/") {
			if branchesStatus != 0 {
				w.WriteHeader(branchesStatus)
			}
			_, _ = w.Write([]byte(branchesBody))
			return
		}
		*got = r.Header.Get("X-NetBox-Branch")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// "main"/"Main" (any case) is the default branch and must send NO X-NetBox-Branch
// header — even when branching isn't installed (the branches endpoint 404s, but
// "main" short-circuits before any fetch).
func TestResolveBranch_MainIsDefaultNoHeader(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/plugins/branching/branches/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found."}`))
			return
		}
		_, present = r.Header["X-Netbox-Branch"] // canonicalized header key
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})

	for _, v := range []string{"main", "Main", "MAIN"} {
		present = false
		if _, err := c.getBytes(provider.WithBranch(context.Background(), v), srv.URL+"/api/x/"); err != nil {
			t.Fatalf("getBytes(%q): %v", v, err)
		}
		if present {
			t.Errorf("branch %q must send NO X-NetBox-Branch header (it is the default)", v)
		}
	}
}

func TestResolveBranch_NameToSchemaID(t *testing.T) {
	var got string
	srv := branchResolveServer(t, &got,
		`{"count":1,"next":null,"results":[{"name":"demo-branch","schema_id":"kc4v9jtd"}]}`, 0)
	c := NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})

	cases := []struct{ in, want string }{
		{"demo-branch", "kc4v9jtd"}, // a name resolves to its schema id
		{"kc4v9jtd", "kc4v9jtd"},    // a schema id passes through
		{"nope", "nope"},            // an unknown value passes through (NetBox rejects it)
	}
	for _, tc := range cases {
		got = ""
		if _, err := c.getBytes(provider.WithBranch(context.Background(), tc.in), srv.URL+"/api/dcim/devices/"); err != nil {
			t.Fatalf("getBytes(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Branch %q -> header %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveBranch_SchemaIDBeatsCollidingName(t *testing.T) {
	var got string
	// Branch "collide" is NAMED "kc4v9jtd" — the same string as branch
	// "demo-branch"'s schema id. Entering that schema id must resolve to itself,
	// not to the branch that happens to be named it.
	srv := branchResolveServer(t, &got,
		`{"count":2,"next":null,"results":[`+
			`{"name":"demo-branch","schema_id":"kc4v9jtd"},`+
			`{"name":"kc4v9jtd","schema_id":"zzzz1111"}]}`, 0)
	c := NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})

	if _, err := c.getBytes(provider.WithBranch(context.Background(), "kc4v9jtd"), srv.URL+"/api/x/"); err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if got != "kc4v9jtd" {
		t.Errorf("header = %q, want kc4v9jtd (a known schema id wins over a colliding branch name)", got)
	}
}

func TestResolveBranch_FollowsPagination(t *testing.T) {
	var got, base string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/plugins/branching/branches/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") == "1" { // page 2
			_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[{"name":"page2-branch","schema_id":"pp222222"}]}`))
			return
		}
		// page 1 points to page 2 via an absolute "next"
		_, _ = w.Write([]byte(`{"count":2,"next":"` + base + `/api/plugins/branching/branches/?offset=1","results":[{"name":"page1-branch","schema_id":"pp111111"}]}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-NetBox-Branch")
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL
	c := NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})

	// A branch name that only appears on the SECOND page still resolves.
	if _, err := c.getBytes(provider.WithBranch(context.Background(), "page2-branch"), srv.URL+"/api/x/"); err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if got != "pp222222" {
		t.Errorf("header = %q, want pp222222 (resolved from page 2)", got)
	}
}

// A transient branch-list failure (5xx/timeout) must not be cached like a real
// result: fetchBranches reports ok=false so resolveBranch retries soon instead
// of blocking resolution for the full TTL. A 404 (branching absent) is an
// authoritative empty result (ok=true).
func TestFetchBranches_TransientVsAuthoritative(t *testing.T) {
	client := func(status int, body string) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})
	}

	if _, _, ok := client(http.StatusInternalServerError, `oops`).fetchBranches(context.Background()); ok {
		t.Error("5xx should be a transient failure (ok=false)")
	}
	if names, _, ok := client(http.StatusNotFound, `{"detail":"Not found."}`).fetchBranches(context.Background()); !ok || len(names) != 0 {
		t.Errorf("404 should be authoritative empty: ok=%v names=%d, want true/0", ok, len(names))
	}
	if names, _, ok := client(0, `{"count":1,"next":null,"results":[{"name":"b","schema_id":"s1"}]}`).fetchBranches(context.Background()); !ok || names["b"] != "s1" {
		t.Errorf("success should populate: ok=%v names=%v, want true/{b:s1}", ok, names)
	}
}

// BranchingInstalled must mirror the branches-endpoint signal: a 2xx means the
// plugin is present, an authoritative 404 means absent, and anything else
// (5xx/network/timeout) is inconclusive (conclusive=false) so callers fail open.
func TestBranchingInstalled(t *testing.T) {
	probe := func(status int, body string) (bool, bool) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		c := NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})
		return c.BranchingInstalled(context.Background())
	}

	if inst, ok := probe(0, `{"count":0,"results":[]}`); !ok || !inst {
		t.Errorf("2xx => installed+conclusive, got installed=%v conclusive=%v", inst, ok)
	}
	if inst, ok := probe(http.StatusNotFound, `{"detail":"Not found."}`); !ok || inst {
		t.Errorf("404 => absent+conclusive, got installed=%v conclusive=%v", inst, ok)
	}
	if inst, ok := probe(http.StatusInternalServerError, `oops`); ok || inst {
		t.Errorf("5xx => inconclusive (both false), got installed=%v conclusive=%v", inst, ok)
	}
}

// The probe must target main and send no X-NetBox-Branch header even if a branch
// sits on the context — it sets noBranchResolveKey (like fetchBranches).
func TestBranchingInstalled_SendsNoBranchHeader(t *testing.T) {
	var hadHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadHeader = r.Header["X-Netbox-Branch"] // canonicalized key
		_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})

	if _, ok := c.BranchingInstalled(provider.WithBranch(context.Background(), "td5smq0f")); !ok {
		t.Fatal("expected a conclusive probe")
	}
	if hadHeader {
		t.Error("BranchingInstalled must send NO X-NetBox-Branch header (targets main)")
	}
}

func TestResolveBranch_NoBranchingPlugin(t *testing.T) {
	var got string
	srv := branchResolveServer(t, &got, `{"detail":"Not found."}`, http.StatusNotFound)
	c := NewClient(srv.URL, "t", &http.Client{Timeout: 5 * time.Second})

	// Branching absent: the list fetch 404s, so a name passes through unresolved.
	if _, err := c.getBytes(provider.WithBranch(context.Background(), "demo-branch"), srv.URL+"/api/x/"); err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if got != "demo-branch" {
		t.Errorf("header = %q, want demo-branch (passthrough when branching absent)", got)
	}
}

func TestGetBytes_BranchHeader(t *testing.T) {
	var gotBranch string
	var hadHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBranch = r.Header.Get("X-NetBox-Branch")
		_, hadHeader = r.Header["X-Netbox-Branch"] // canonicalized key
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	// With a branch on the context, the header is sent.
	if _, err := c.getBytes(provider.WithBranch(context.Background(), "td5smq0f"), srv.URL); err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if gotBranch != "td5smq0f" {
		t.Errorf("X-NetBox-Branch = %q, want td5smq0f", gotBranch)
	}

	// Without a branch, the header is absent (targets main).
	if _, err := c.getBytes(context.Background(), srv.URL); err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if hadHeader {
		t.Error("X-NetBox-Branch must be absent when no branch is set")
	}
}

func TestGetListPage_ShapeTolerant(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	t.Run("envelope", func(t *testing.T) {
		body = `{"count":3,"next":null,"results":[{"name":"a"}]}`
		page, err := c.getListPage(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("getListPage: %v", err)
		}
		total, known := page.total()
		if total != 3 || !known || len(page.Results) != 1 || page.Next != nil {
			t.Errorf("envelope not parsed: count=%d known=%v results=%d next=%v", total, known, len(page.Results), page.Next)
		}
	})

	t.Run("bare array normalized to one page", func(t *testing.T) {
		body = `[{"name":"x"},{"name":"y"}]`
		page, err := c.getListPage(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("getListPage: %v", err)
		}
		total, known := page.total()
		if total != 2 || !known || len(page.Results) != 2 || page.Next != nil {
			t.Errorf("bare array not normalized: count=%d known=%v results=%d next=%v", total, known, len(page.Results), page.Next)
		}
	})

	// A cursor-mode envelope: NetBox 4.6 omits the count when paging by primary
	// key. "Not counted" must stay distinguishable from "counted zero" — the
	// whole reason listPage.Count is a pointer.
	t.Run("null count is not zero", func(t *testing.T) {
		body = `{"count":null,"next":null,"results":[{"name":"a"},{"name":"b"}]}`
		page, err := c.getListPage(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("getListPage: %v", err)
		}
		if total, known := page.total(); known || total != 0 {
			t.Errorf("null count should be unknown: count=%d known=%v", total, known)
		}
	})

	t.Run("zero count is counted zero", func(t *testing.T) {
		body = `{"count":0,"next":null,"results":[]}`
		page, err := c.getListPage(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("getListPage: %v", err)
		}
		if total, known := page.total(); !known || total != 0 {
			t.Errorf("zero count should be known: count=%d known=%v", total, known)
		}
	})

	t.Run("neither shape errors", func(t *testing.T) {
		body = `"not a list"`
		if _, err := c.getListPage(context.Background(), srv.URL); err == nil {
			t.Error("expected an error for a non-list JSON body, got nil")
		}
	})
}

// TestAPIErrorTruncatesURL guards the user-facing side of a batched failure.
// Grafana surfaces this message in an error toast, and ip-enrichment builds
// ~6 KB request lines (hundreds of repeated ?address= / ?id= parameters), so an
// untruncated URL buries the status and body under kilobytes of query string.
func TestAPIErrorTruncatesURL(t *testing.T) {
	long := "http://netbox.example/api/ipam/ip-addresses/?" +
		strings.Repeat("address=10.0.0.1&", 400)
	msg := (&APIError{Status: 500, URL: long, Body: "boom"}).Error()

	if len(msg) > 600 {
		t.Errorf("error message is %d bytes; a toast must not carry the whole batched URL", len(msg))
	}
	if !strings.Contains(msg, "500") || !strings.Contains(msg, "boom") {
		t.Errorf("truncation must not cost the status or the body: %q", msg)
	}
	if !strings.Contains(msg, "ipam/ip-addresses") {
		t.Errorf("the endpoint must survive truncation, it is what identifies the request: %q", msg)
	}
	if !strings.Contains(msg, "…") {
		t.Errorf("a truncated URL must say so: %q", msg)
	}

	short := "http://netbox.example/api/dcim/devices/?limit=100"
	if got := (&APIError{Status: 404, URL: short, Body: "nope"}).Error(); !strings.Contains(got, short) {
		t.Errorf("a short URL must be left intact, got %q", got)
	}
}
