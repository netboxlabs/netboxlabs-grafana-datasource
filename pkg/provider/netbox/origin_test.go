package netbox

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// An empty link is the end of the walk, as it always was, and so is one with
	// no query (NetBox's always carries the paging state): neither may become
	// this page's path with no query, which is the whole unfiltered table.
	for _, next := range []string{"", "  ", "https://netbox.example.com/netbox/api/dcim/devices/", "https://netbox.example.com/netbox/api/dcim/devices/?", "#x"} {
		if got, err := c.nextPageURL(current, next); err != nil || got != "" {
			t.Errorf("nextPageURL(%q) = %q, %v; want \"\" (end of the walk)", next, got, err)
		}
	}
}

// A redirect is a URL the server chose, like `next`. Go follows it and keeps
// Authorization when only the scheme changes, so a 301 to http:// on the same
// host would send the token in clear text. The client refuses any hop that
// leaves the configured origin.
func TestGetBytes_RedirectOffTheConfiguredOriginIsNotFollowed(t *testing.T) {
	t.Run("another host", func(t *testing.T) {
		other := newElsewhere(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
		}))
		defer srv.Close()
		c := NewClient(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})
		if _, err := c.getBytes(context.Background(), srv.URL+"/api/dcim/devices/"); err == nil {
			t.Error("a redirect to another host was followed")
		}
		if n := other.hits.Load(); n != 0 {
			t.Errorf("%d request(s) followed the redirect to another host", n)
		}
	})
	// Same host, plain http. A TLS test server rejects a plaintext request
	// before any handler sees it, which would hide the downgrade (the token is
	// already on the wire by then), so the policy itself is asked.
	t.Run("plain http on the same host", func(t *testing.T) {
		c := NewClient("https://netbox.example.com", "secret-token", nil)
		policy := c.http.CheckRedirect
		if policy == nil {
			t.Fatal("no redirect policy")
		}
		hop := func(raw string) *http.Request {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Request{URL: u}
		}
		via := []*http.Request{hop("https://netbox.example.com/api/dcim/devices/")}
		if err := policy(hop("http://netbox.example.com/api/dcim/devices/"), via); err == nil {
			t.Error("a redirect from https to http on the same host was allowed")
		}
		if err := policy(hop("https://netbox.example.com/api/dcim/devices/?page=2"), via); err != nil {
			t.Errorf("a same-origin redirect was refused: %v", err)
		}
	})
	t.Run("same origin still followed", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/old/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/api/new/", http.StatusMovedPermanently)
		})
		mux.HandleFunc("/api/new/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := NewClient(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})
		if _, err := c.getBytes(context.Background(), srv.URL+"/api/old/"); err != nil {
			t.Errorf("a same-origin redirect failed: %v", err)
		}
	})
}

// The caller's client is not changed: the redirect policy is this client's.
func TestNewClient_DoesNotChangeTheCallersHTTPClient(t *testing.T) {
	hc := &http.Client{Timeout: 5 * time.Second}
	NewClient("https://netbox.example.com", "t", hc)
	if hc.CheckRedirect != nil {
		t.Error("NewClient set CheckRedirect on the caller's http.Client")
	}
}

// A refused redirect is permanent: the same request is refused the same way.
// It must not be retried as if the connection had dropped.
func TestGetListPage_ARefusedRedirectIsNotRetried(t *testing.T) {
	other := newElsewhere(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	defer srv.Close()
	p := New(srv.URL, "secret-token", &http.Client{Timeout: 5 * time.Second})
	if _, _, err := p.getListPageRetryN(context.Background(), srv.URL+"/api/dcim/devices/"); err == nil {
		t.Fatal("a redirect off the configured origin was followed")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("NetBox was asked %d times, want 1 (a refused redirect is not transient)", n)
	}
}

// Upgrading to https on the same host is the redirect a NetBox configured with
// an http:// URL commonly answers with. It moves the token to a safer channel
// on the same host, and it worked before the redirect policy existed. The
// transport answers in process, so the upgrade can land on port 443 itself.
func TestGetBytes_HTTPSUpgradeOnTheSameHostIsFollowed(t *testing.T) {
	var auth atomic.Value
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "http" {
			to := "https://" + r.URL.Hostname() + r.URL.RequestURI()
			return &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{"Location": {to}},
				Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		auth.Store(r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})}

	c := NewClient("http://netbox.example.com", "secret-token", hc)
	if _, err := c.getBytes(context.Background(), "http://netbox.example.com/api/status/"); err != nil {
		t.Fatalf("an http to https upgrade on the same host was refused: %v", err)
	}
	if got, _ := auth.Load().(string); got != "Token secret-token" {
		t.Errorf("Authorization after the upgrade = %q, want the token", got)
	}
}

// The upgrade is followed only to https's own port. Go keeps the Authorization
// header across a same-host redirect whatever the port, so an upgrade to any
// other port would hand the token to whatever listens there; a NetBox served
// on another https port is configured with that https:// URL instead.
func TestRedirectPolicy_HTTPSUpgradeIsToPort443Only(t *testing.T) {
	hop := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	for _, tc := range []struct {
		base, to string
		allowed  bool
	}{
		{"http://netbox.example.com", "https://netbox.example.com/api/status/", true},
		{"http://netbox.example.com", "https://netbox.example.com:443/api/status/", true},
		{"http://netbox.example.com:80/netbox", "https://netbox.example.com/netbox/api/status/", true},
		{"http://netbox.example.com", "https://netbox.example.com:8443/api/status/", false},
		{"http://example.com/netbox", "https://example.com:8443/netbox/api/status/", false},
		// Only the standard pair is an upgrade: from another http port there
		// is no telling which https service is NetBox's.
		{"http://netbox.example.com:8080", "https://netbox.example.com/api/status/", false},
		{"http://netbox.example.com:8080", "https://netbox.example.com:8443/api/status/", false},
	} {
		c := NewClient(tc.base, "secret-token", nil)
		err := c.http.CheckRedirect(hop(tc.to), []*http.Request{hop(tc.base)})
		if tc.allowed && err != nil {
			t.Errorf("%s -> %s refused: %v", tc.base, tc.to, err)
		}
		if !tc.allowed && !errors.Is(err, errOffOrigin) {
			t.Errorf("%s -> %s: err = %v, want errOffOrigin", tc.base, tc.to, err)
		}
	}

	// End to end: the redirect to another port is not followed at all.
	var offPort atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "https" {
			offPort.Add(1)
		}
		return &http.Response{StatusCode: http.StatusMovedPermanently,
			Header: http.Header{"Location": {"https://netbox.example.com:8443" + r.URL.RequestURI()}},
			Body:   io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	c := NewClient("http://netbox.example.com", "secret-token", hc)
	if _, err := c.getBytes(context.Background(), "http://netbox.example.com/api/status/"); !errors.Is(err, errOffOrigin) {
		t.Errorf("err = %v, want errOffOrigin", err)
	}
	if n := offPort.Load(); n != 0 {
		t.Errorf("%d request(s) followed the upgrade to port 8443", n)
	}
}

// A port spelled out is the same origin as the scheme's default left implicit.
func TestRedirectPolicy_DefaultPortsAreTheSameOrigin(t *testing.T) {
	for _, tc := range []struct{ base, hop string }{
		{"https://netbox.example.com:443", "https://netbox.example.com/api/dcim/devices/"},
		{"https://netbox.example.com", "https://NetBox.Example.com:443/api/dcim/devices/"},
		{"http://netbox.example.com:80", "http://netbox.example.com/api/dcim/devices/"},
	} {
		c := NewClient(tc.base, "t", nil)
		u, _ := url.Parse(tc.hop)
		if err := c.http.CheckRedirect(&http.Request{URL: u}, []*http.Request{{URL: u}}); err != nil {
			t.Errorf("base %s, redirect to %s refused: %v", tc.base, tc.hop, err)
		}
	}
}

// A configured URL with no scheme cannot be checked against anything; it is
// reported as what it is rather than as a refusal.
func TestGetBytes_AConfiguredURLWithoutASchemeSaysSo(t *testing.T) {
	c := NewClient("netbox.example.com", "t", nil)
	_, err := c.getBytes(context.Background(), c.apiURL("status", nil))
	if !errors.Is(err, errNotAbsoluteURL) {
		t.Errorf("err = %v, want errNotAbsoluteURL", err)
	}
}

// On a shared host the path prefix is what marks out NetBox: with the URL
// configured as https://example.com/netbox, another application at
// https://example.com/other is a different place, and neither a request nor a
// redirect may take the token there. The path is cleaned first, so a dot
// segment cannot climb out of the prefix.
func TestRequestsAndRedirectsStayUnderTheConfiguredPath(t *testing.T) {
	c := NewClient("https://example.com/netbox", "secret-token", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return nil, fmt.Errorf("unexpected request")
	})})
	hop := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	via := []*http.Request{hop("https://example.com/netbox/api/status/")}
	for raw, allowed := range map[string]bool{
		"https://example.com/netbox/api/dcim/devices/":  true,
		"https://example.com/netbox":                    true,
		"https://example.com/netbox/":                   true,
		"https://example.com/other-service/":            false,
		"https://example.com/netbox-admin/":             false,
		"https://example.com/":                          false,
		"https://example.com/netbox/../other-service/":  false,
		"https://example.com/netbox/api/../../secrets/": false,
	} {
		err := c.http.CheckRedirect(hop(raw), via)
		if allowed && err != nil {
			t.Errorf("redirect to %s refused: %v", raw, err)
		}
		if !allowed && err == nil {
			t.Errorf("redirect to %s allowed; it leaves /netbox", raw)
		}
	}
	if _, err := c.getBytes(context.Background(), "https://example.com/other-service/api/"); !errors.Is(err, errOffOrigin) {
		t.Errorf("getBytes outside the configured path: err = %v, want errOffOrigin", err)
	}

	// No path configured: the whole origin is NetBox's.
	root := NewClient("https://netbox.example.com", "t", nil)
	if err := root.http.CheckRedirect(hop("https://netbox.example.com/anything/"), via); err != nil {
		t.Errorf("a root-configured client refused a same-origin path: %v", err)
	}

	// The https upgrade keeps to the prefix too.
	plain := NewClient("http://example.com/netbox", "t", nil)
	if err := plain.http.CheckRedirect(hop("https://example.com/netbox/api/status/"), via); err != nil {
		t.Errorf("an https upgrade under the prefix was refused: %v", err)
	}
	if err := plain.http.CheckRedirect(hop("https://example.com/other-service/"), via); err == nil {
		t.Error("an https upgrade outside the prefix was allowed")
	}
}
