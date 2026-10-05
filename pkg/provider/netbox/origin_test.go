package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every request getBytes sends carries the API token, so where a request goes
// must be decided by the configured NetBox URL, never by a URL NetBox wrote into
// a response. NetBox builds `next` and its API index URLs from the request as it
// arrived (USE_X_FORWARDED_HOST, SECURE_PROXY_SSL_HEADER): behind a proxy that
// does not pass X-Forwarded-Proto/-Host, they name plain http or the proxy's
// upstream host, and following them verbatim sent the token there.

// elsewhere stands for the host or scheme a misconfigured proxy makes NetBox
// print. Nothing may ever reach it.
type elsewhere struct {
	*httptest.Server
	hits atomic.Int64
}

func newElsewhere(t *testing.T) *elsewhere {
	t.Helper()
	e := &elsewhere{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		t.Errorf("request reached another origin: %s %s (Authorization present: %t)", r.Method, r.URL, r.Header.Get("Authorization") != "")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
	}))
	t.Cleanup(e.Close)
	return e
}

// twoPages serves a list endpoint whose first page links to the second through
// nextOf(page-2 query). It records the Authorization of the second request.
func twoPages(t *testing.T, mux *http.ServeMux, path, first, second string, nextOf func(path, query string) string, secondAuth *atomic.Value) {
	t.Helper()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("offset") == "" {
			q.Set("offset", "1")
			_, _ = fmt.Fprintf(w, `{"count":2,"next":%q,"results":[%s]}`, nextOf(path, q.Encode()), first)
			return
		}
		secondAuth.Store(r.Header.Get("Authorization"))
		_, _ = fmt.Fprintf(w, `{"count":2,"next":null,"results":[%s]}`, second)
	})
}

func TestFetchList_NextOnAnotherOriginIsAskedOfTheConfiguredNetBox(t *testing.T) {
	other := newElsewhere(t)
	var auth atomic.Value
	mux := http.NewServeMux()
	twoPages(t, mux, "/api/dcim/devices/", `{"id":1}`, `{"id":2}`,
		func(path, query string) string { return other.URL + path + "?" + query }, &auth)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})
	res, err := p.fetchList(context.Background(), "dcim/devices", url.Values{}, 2, false)
	if err != nil {
		t.Fatalf("fetchList: %v", err)
	}
	if len(res.rows) != 2 {
		t.Fatalf("rows = %d, want 2 (page 2 must still be read, from the configured NetBox)", len(res.rows))
	}
	if got, _ := auth.Load().(string); got != "Token secret-token" {
		t.Errorf("page 2 Authorization = %q, want the token on the configured NetBox", got)
	}
	if n := other.hits.Load(); n != 0 {
		t.Errorf("%d request(s) went to the origin NetBox's next named", n)
	}
}

// The commonest form of the problem: TLS ends at the proxy, NetBox sees plain
// http, and `next` comes back as http:// on the same host. The page must still
// be read over https.
func TestFetchList_PlainHTTPNextFromAnHTTPSNetBoxStaysOnHTTPS(t *testing.T) {
	var auth atomic.Value
	var plain atomic.Int64
	mux := http.NewServeMux()
	var host string
	twoPages(t, mux, "/api/dcim/devices/", `{"id":1}`, `{"id":2}`,
		func(path, query string) string { return "http://" + host + path + "?" + query }, &auth)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			plain.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host = strings.TrimPrefix(srv.URL, "https://")

	p := New(srv.URL, "secret-token", srv.Client())
	res, err := p.fetchList(context.Background(), "dcim/devices", url.Values{}, 2, false)
	if err != nil {
		t.Fatalf("fetchList: %v", err)
	}
	if len(res.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(res.rows))
	}
	if got, _ := auth.Load().(string); got != "Token secret-token" {
		t.Errorf("page 2 Authorization = %q, want the token, over TLS", got)
	}
	if n := plain.Load(); n != 0 {
		t.Errorf("%d request(s) arrived without TLS", n)
	}
}

// The branch list is paged the same way and read with the same token.
func TestResolveBranch_NextOnAnotherOriginIsAskedOfTheConfiguredNetBox(t *testing.T) {
	other := newElsewhere(t)
	var auth atomic.Value
	mux := http.NewServeMux()
	twoPages(t, mux, "/api/plugins/branching/branches/",
		`{"name":"first","schema_id":"aaaa1111"}`, `{"name":"second","schema_id":"bbbb2222"}`,
		func(path, query string) string { return other.URL + path + "?" + query }, &auth)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})
	if got := c.resolveBranch(context.Background(), "second"); got != "bbbb2222" {
		t.Errorf("resolveBranch(second) = %q, want bbbb2222 (a name on page 2)", got)
	}
	if got, _ := auth.Load().(string); got != "Token secret-token" {
		t.Errorf("page 2 Authorization = %q, want the token on the configured NetBox", got)
	}
	if n := other.hits.Load(); n != 0 {
		t.Errorf("%d request(s) went to the origin NetBox's next named", n)
	}
}

// Utilization walks three child lists of its own, each with its own loop.
func TestUtilizationWalks_NextOnAnotherOriginIsAskedOfTheConfiguredNetBox(t *testing.T) {
	cases := []struct {
		name, path, first, second string
		walk                      func(p *Provider) (int, error)
	}{
		{"child IPs", "/api/ipam/ip-addresses/", `{"address":"10.0.0.1/24"}`, `{"address":"10.0.0.2/24"}`,
			func(p *Provider) (int, error) {
				out, err := p.childIPHosts(context.Background(), &utilCost{}, "10.0.0.0/24", nil)
				return len(out), err
			}},
		{"child ranges", "/api/ipam/ip-ranges/", `{"start_address":"10.0.0.10/24","end_address":"10.0.0.20/24"}`, `{"start_address":"10.0.0.30/24","end_address":"10.0.0.40/24"}`,
			func(p *Provider) (int, error) {
				out, err := p.utilizedChildRanges(context.Background(), &utilCost{}, "10.0.0.0/24", nil)
				return len(out), err
			}},
		{"child prefixes", "/api/ipam/prefixes/", `{"prefix":"10.0.0.0/26"}`, `{"prefix":"10.0.0.64/26"}`,
			func(p *Provider) (int, error) {
				out, err := p.childPrefixes(context.Background(), &utilCost{}, "10.0.0.0/24", nil)
				return len(out), err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := newElsewhere(t)
			var auth atomic.Value
			mux := http.NewServeMux()
			twoPages(t, mux, tc.path, tc.first, tc.second,
				func(path, query string) string { return other.URL + path + "?" + query }, &auth)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			p := New(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})
			n, err := tc.walk(p)
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			if n != 2 {
				t.Errorf("got %d items, want 2 (page 2 read from the configured NetBox)", n)
			}
			if got, _ := auth.Load().(string); got != "Token secret-token" {
				t.Errorf("page 2 Authorization = %q, want the token on the configured NetBox", got)
			}
			if n := other.hits.Load(); n != 0 {
				t.Errorf("%d request(s) went to the origin NetBox's next named", n)
			}
		})
	}
}

// Discovery reads NetBox's tree of API indexes, whose values are absolute URLs
// built the same way as `next`. The keys name the path; the values are not
// followed.
func TestDiscover_IndexURLsOnAnotherOriginAreNotFollowed(t *testing.T) {
	other := newElsewhere(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"dcim":%q,"plugins":%q,"status":%q}`,
			other.URL+"/api/dcim/", other.URL+"/api/plugins/", other.URL+"/api/status/")
	})
	mux.HandleFunc("/api/dcim/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"devices":%q}`, other.URL+"/api/dcim/devices/")
	})
	mux.HandleFunc("/api/plugins/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"bgp":%q}`, other.URL+"/api/plugins/bgp/")
	})
	mux.HandleFunc("/api/plugins/bgp/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"session":%q}`, other.URL+"/api/plugins/bgp/session/")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := New(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})
	types, err := p.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	got := map[string]bool{}
	for _, ot := range types {
		got[ot.Value] = true
	}
	for _, want := range []string{"dcim/devices", "plugins/bgp/session"} {
		if !got[want] {
			t.Errorf("discovery lost %s; got %v", want, got)
		}
	}
	if n := other.hits.Load(); n != 0 {
		t.Errorf("%d request(s) went to the origin NetBox's index named", n)
	}
}

// getBytes holds the line itself: whatever a caller hands it, a request that
// would leave the configured origin is refused before anything is sent, so a
// loop added later that forgets to rebuild its URL fails loudly instead of
// sending the token away.
func TestGetBytes_RefusesAURLOffTheConfiguredOrigin(t *testing.T) {
	other := newElsewhere(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})

	if _, err := c.getBytes(context.Background(), other.URL+"/api/dcim/devices/"); err == nil {
		t.Error("getBytes to another host succeeded; want a refusal")
	}
	if n := other.hits.Load(); n != 0 {
		t.Errorf("%d request(s) sent to another host", n)
	}

	// Same host, other scheme: an https NetBox is never asked over http.
	var sent atomic.Int64
	tls := NewClient("https://netbox.example.com", "secret-token", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return nil, fmt.Errorf("unexpected request to %s", r.URL)
	})})
	if _, err := tls.getBytes(context.Background(), "http://netbox.example.com/api/dcim/devices/"); err == nil {
		t.Error("getBytes over http to an https NetBox succeeded; want a refusal")
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("%d request(s) sent over http", n)
	}

	// The configured origin itself, any path under it, is fine.
	if _, err := c.getBytes(context.Background(), srv.URL+"/api/status/"); err != nil {
		t.Errorf("getBytes on the configured origin: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// nextPageURL keeps everything about where the request goes — scheme, host,
// port and path — from the request just made, and takes only the paging
// state NetBox put in the query.
func TestNextPageURL(t *testing.T) {
	c := NewClient("https://netbox.example.com/netbox", "t", nil)
	current := "https://netbox.example.com/netbox/api/dcim/devices/?limit=50&site=ams"
	cases := map[string]string{
		"http://netbox.example.com/netbox/api/dcim/devices/?limit=50&offset=50&site=ams":    "https://netbox.example.com/netbox/api/dcim/devices/?limit=50&offset=50&site=ams",
		"http://upstream:8080/api/dcim/devices/?limit=50&offset=50&site=ams":                "https://netbox.example.com/netbox/api/dcim/devices/?limit=50&offset=50&site=ams",
		"https://evil.example.org/netbox/api/dcim/devices/?limit=50&offset=50&site=ams":     "https://netbox.example.com/netbox/api/dcim/devices/?limit=50&offset=50&site=ams",
		"/netbox/api/dcim/devices/?limit=50&start=101&site=ams":                             "https://netbox.example.com/netbox/api/dcim/devices/?limit=50&start=101&site=ams",
		"https://netbox.example.com/netbox/api/dcim/devices/?limit=50&offset=50&site=ams#x": "https://netbox.example.com/netbox/api/dcim/devices/?limit=50&offset=50&site=ams",
	}
	for next, want := range cases {
		got, err := c.nextPageURL(current, next)
		if err != nil {
			t.Errorf("nextPageURL(%q): %v", next, err)
			continue
		}
		if got != want {
			t.Errorf("nextPageURL(%q) = %q, want %q", next, got, want)
		}
	}
	if _, err := c.nextPageURL(current, "http://[::1"); err == nil {
		t.Error("an unparseable next was accepted")
	}
}
