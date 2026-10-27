package plugin

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/proxy"
	"github.com/grafana/grafana-plugin-sdk-go/config"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/replicacache"
)

// Save & test against a NetBox that only Grafana's secure socks proxy can
// reach: Private Data Source Connect on Grafana Cloud, an admin-run SOCKS proxy
// when self-hosted. The plugin has no proxy code of its own. The SDK applies
// the proxy when it builds the HTTP client, provided the client comes from
// settings.HTTPClientOptions, so these tests fail if newHTTPClient stops
// building it that way or anything on the way to the request swaps its
// transport.

// pdcTestUID is the datasource the tests build. The SDK sends it as the proxy
// username when the datasource names no PDC network.
const pdcTestUID = "netbox-private"

// socksConnect is one CONNECT the proxy relayed: who asked, with which
// password, and where to.
type socksConnect struct {
	user, pass, target string
}

type socksServer struct {
	addr string
	// refuse answers every CONNECT with "connection refused" (reply 5): the
	// proxy is up but cannot reach NetBox, like a PDC agent outside NetBox's
	// network.
	refuse bool
	// silent never answers a CONNECT: an agent whose traffic to NetBox is
	// dropped by a firewall, the most common PDC failure. The request then
	// times out, and net/http's timeout error no longer carries the proxy.
	silent bool
	// rejectCredentials fails every username/password exchange (RFC 1929
	// status 1): a PDC network ID the gateway does not know.
	rejectCredentials bool
	// noAuth selects "no authentication" even when the client offers
	// username/password: a self-hosted proxy that asks for no credentials.
	noAuth bool
	mu     sync.Mutex
	seen   []socksConnect
}

func (s *socksServer) connects() []socksConnect {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// refusing makes startSOCKS5's proxy refuse every CONNECT.
func refusing(s *socksServer) { s.refuse = true }

// silent makes startSOCKS5's proxy leave every CONNECT unanswered.
func silent(s *socksServer) { s.silent = true }

// rejectingCredentials makes startSOCKS5's proxy refuse every username and
// password.
func rejectingCredentials(s *socksServer) { s.rejectCredentials = true }

// withoutAuth makes startSOCKS5's proxy ask for no credentials.
func withoutAuth(s *socksServer) { s.noAuth = true }

// startSOCKS5 runs a SOCKS5 proxy (RFC 1928, with RFC 1929 username/password)
// that relays every CONNECT and records it. It prefers username/password when
// the client offers it, because that is where Grafana puts the PDC network.
// Options apply before the proxy accepts its first connection.
func startSOCKS5(t *testing.T, opts ...func(*socksServer)) *socksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &socksServer{addr: ln.Addr().String()}
	for _, opt := range opts {
		opt(s)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *socksServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	read := func(n int) ([]byte, error) {
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		return b, err
	}
	greeting, err := read(2)
	if err != nil || greeting[0] != 5 {
		return
	}
	methods, err := read(int(greeting[1]))
	if err != nil {
		return
	}
	var user, pass string
	if slices.Contains(methods, 2) && !s.noAuth {
		if _, err := c.Write([]byte{5, 2}); err != nil {
			return
		}
		head, err := read(2) // sub-negotiation version, username length
		if err != nil {
			return
		}
		name, err := read(int(head[1]))
		if err != nil {
			return
		}
		plen, err := read(1)
		if err != nil {
			return
		}
		secret, err := read(int(plen[0]))
		if err != nil {
			return
		}
		user, pass = string(name), string(secret)
		if s.rejectCredentials {
			_, _ = c.Write([]byte{1, 1})
			return
		}
		if _, err := c.Write([]byte{1, 0}); err != nil {
			return
		}
	} else if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}
	req, err := read(4) // version, command, reserved, address type
	if err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		ip, err := read(4)
		if err != nil {
			return
		}
		host = net.IP(ip).String()
	case 3:
		n, err := read(1)
		if err != nil {
			return
		}
		name, err := read(int(n[0]))
		if err != nil {
			return
		}
		host = string(name)
	case 4:
		ip, err := read(16)
		if err != nil {
			return
		}
		host = net.IP(ip).String()
	default:
		return
	}
	port, err := read(2)
	if err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port))))
	if s.refuse {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	if s.silent {
		_, _ = io.Copy(io.Discard, r) // hold the connection until the client gives up
		return
	}
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = up.Close() }()
	s.mu.Lock()
	s.seen = append(s.seen, socksConnect{user: user, pass: pass, target: target})
	s.mu.Unlock()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(up, r) }()
	_, _ = io.Copy(c, up)
}

// fakeNetBox answers /api/status/ the way NetBox does, and counts every
// request that reaches it, so a test can tell "through the proxy" from
// "direct".
type fakeNetBox struct {
	*httptest.Server
	hits atomic.Int64
}

func newFakeNetBox(t *testing.T) *fakeNetBox {
	t.Helper()
	nb := &fakeNetBox{}
	nb.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nb.hits.Add(1)
		if r.URL.Path != "/api/status/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"netbox-version": "4.4.10"}`))
	}))
	t.Cleanup(nb.Close)
	return nb
}

// grafanaProxyCfg is Grafana's configuration for a secure socks proxy at addr,
// reached without TLS.
func grafanaProxyCfg(addr string) map[string]string {
	return map[string]string{
		proxy.PluginSecureSocksProxyEnabled:       "true",
		proxy.PluginSecureSocksProxyProxyAddress:  addr,
		proxy.PluginSecureSocksProxyAllowInsecure: "true",
	}
}

// saveAndTest runs Save & test with Grafana's proxy at proxyAddr.
func saveAndTest(t *testing.T, proxyAddr string, jsonData map[string]any) *backend.CheckHealthResult {
	t.Helper()
	return saveAndTestIn(t, grafanaProxyCfg(proxyAddr), jsonData)
}

// saveAndTestIn builds the datasource the way Grafana does, inside a context
// that carries Grafana's configuration, and runs its health check. The
// datasource carries a proxy password, as one the PDC network selector set,
// and a short timeout so a proxy that never answers fails the test quickly.
func saveAndTestIn(t *testing.T, grafanaCfg map[string]string, jsonData map[string]any) *backend.CheckHealthResult {
	t.Helper()
	ctx := config.WithGrafanaConfig(context.Background(), config.NewGrafanaCfg(grafanaCfg))
	settings := map[string]any{"timeoutSeconds": 5}
	maps.Copy(settings, jsonData)
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewDatasource(ctx, backend.DataSourceInstanceSettings{
		UID:      pdcTestUID,
		Name:     "NetBox (private)",
		Type:     "netboxlabs-netbox-datasource",
		JSONData: raw,
		DecryptedSecureJSONData: map[string]string{
			"apiToken":                 "0123456789abcdef0123456789abcdef01234567",
			"secureSocksProxyPassword": "pdc-secret",
		},
	})
	if err != nil {
		t.Fatalf("NewDatasource: %v", err)
	}
	ds, ok := inst.(*Datasource)
	if !ok {
		t.Fatalf("instance is %T, want *Datasource", inst)
	}
	res, err := ds.CheckHealth(ctx, &backend.CheckHealthRequest{})
	if err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	return res
}

// allConnects reports whether the proxy relayed at least one CONNECT and every
// one of them is want.
func allConnects(got []socksConnect, want socksConnect) bool {
	if len(got) == 0 {
		return false
	}
	for _, c := range got {
		if c != want {
			return false
		}
	}
	return true
}

func TestSaveAndTest_SecureSocksProxy(t *testing.T) {
	t.Run("goes through the proxy, as the PDC network, when the datasource enables it", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{
			"url":                      netbox.URL,
			"enableSecureSocksProxy":   true,
			"secureSocksProxyUsername": "0d5c3a6e-pdc-network",
		})
		if res.Status != backend.HealthStatusOk || !strings.Contains(res.Message, "4.4.10") {
			t.Fatalf("health = %v %q, want ok from NetBox 4.4.10", res.Status, res.Message)
		}
		want := socksConnect{user: "0d5c3a6e-pdc-network", pass: "pdc-secret", target: strings.TrimPrefix(netbox.URL, "http://")}
		if got := socks.connects(); !allConnects(got, want) {
			t.Errorf("proxy relayed %v, want only CONNECTs %+v", got, want)
		}
	})

	// Without a network name the SDK falls back to the datasource UID, which is
	// what a self-hosted proxy receives from the settings-page switch (it sets
	// no username). On Grafana Cloud the username has to be the network's ID.
	t.Run("sends the datasource UID when no network is named", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusOk {
			t.Fatalf("health = %v %q, want ok", res.Status, res.Message)
		}
		want := socksConnect{user: pdcTestUID, pass: "pdc-secret", target: strings.TrimPrefix(netbox.URL, "http://")}
		if got := socks.connects(); !allConnects(got, want) {
			t.Errorf("proxy relayed %v, want only CONNECTs %+v", got, want)
		}
	})

	t.Run("stays direct when the datasource does not enable it", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL})
		if res.Status != backend.HealthStatusOk {
			t.Fatalf("health = %v %q, want ok", res.Status, res.Message)
		}
		if got := socks.connects(); len(got) != 0 {
			t.Errorf("proxy relayed %v for a datasource that did not enable it", got)
		}
	})

	// A NetBox only PDC can reach must not look reachable because the request
	// quietly went direct: with the proxy down, Save & test has to fail, without
	// a single request reaching NetBox, and say which hop failed.
	t.Run("fails rather than going direct when the proxy is down", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		deadProxy := ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		res := saveAndTest(t, deadProxy, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error with the proxy down", res.Status, res.Message)
		}
		if n := netbox.hits.Load(); n != 0 {
			t.Errorf("%d request(s) reached NetBox directly", n)
		}
		assertNamesTheProxyOnly(t, res.Message, deadProxy, netbox.URL)
		assertPointsAtTheAgent(t, res.Message)
	})

	// The proxy is up but cannot reach NetBox. On Grafana Cloud the reader
	// cannot open the server log the message points to, so it has to say which
	// hop failed: told only "the connection was refused", they check NetBox.
	t.Run("names the proxy when it cannot reach NetBox", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t, refusing)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, socks.addr, netbox.URL)
		assertPointsAtTheAgent(t, res.Message)
	})

	// The agent's traffic to NetBox is dropped, so the CONNECT is never
	// answered and the request times out. net/http's timeout error carries no
	// trace of the proxy, which is why the wording is decided from the
	// datasource's settings rather than from the error.
	t.Run("names the proxy when it never answers", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t, silent)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true, "timeoutSeconds": 1})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "timed out") {
			t.Errorf("message %q does not say the request timed out", res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, socks.addr, netbox.URL)
		assertPointsAtTheAgent(t, res.Message)
	})

	// An answer NetBox sent back through the proxy is not the proxy's failure:
	// a login page served in place of the API, or a redirect, means the route
	// works and the URL is wrong. Naming the proxy would send the reader to the
	// one hop that did its job.
	t.Run("does not blame the proxy for an answer NetBox sent through it", func(t *testing.T) {
		loginPage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>Sign in</body></html>"))
		}))
		t.Cleanup(loginPage.Close)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": loginPage.URL, "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		if len(socks.connects()) == 0 {
			t.Fatal("the request did not go through the proxy, so this tests nothing")
		}
		assertPointsAtTheURLTheProxyReaches(t, res.Message)
	})

	// NetBox's certificate is checked end to end, through the tunnel the proxy
	// opened: a certificate Grafana cannot verify is NetBox's, and the proxy
	// relayed the handshake as it should.
	t.Run("does not blame the proxy for NetBox's certificate", func(t *testing.T) {
		netbox := newQuietTLSServer(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		if len(socks.connects()) == 0 {
			t.Fatal("the request did not go through the proxy, so this tests nothing")
		}
		if !strings.Contains(res.Message, "Skip TLS verify") {
			t.Errorf("message %q does not point at the datasource's TLS setting", res.Message)
		}
		assertPointsAtTheURLTheProxyReaches(t, res.Message)
	})

	t.Run("does not blame the proxy when NetBox does not answer with TLS", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": strings.Replace(netbox.URL, "http://", "https://", 1), "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		if len(socks.connects()) == 0 {
			t.Fatal("the request did not go through the proxy, so this tests nothing")
		}
		if !strings.Contains(res.Message, "did not answer with TLS") {
			t.Errorf("message %q does not say NetBox did not answer with TLS", res.Message)
		}
		assertPointsAtTheURLTheProxyReaches(t, res.Message)
	})

	// The PDC gateway reads the proxy username as the network to route to, so
	// a network it does not know fails the credentials exchange. Told only "the
	// request failed", the reader has nothing to check.
	t.Run("names the proxy credentials when the proxy refuses them", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t, rejectingCredentials)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true, "secureSocksProxyUsername": "deleted-network"})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, socks.addr, netbox.URL)
		assertNamesTheProxyCredentials(t, res.Message)
	})

	// An empty username — a provisioning file whose environment variable is
	// unset — is never sent: the SOCKS client refuses it before the exchange.
	t.Run("names the proxy credentials when the username is empty", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true, "secureSocksProxyUsername": ""})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, socks.addr, netbox.URL)
		assertNamesTheProxyCredentials(t, res.Message)
	})

	// Why the settings accept an empty username: a proxy that asks for no
	// credentials never reads it, and that datasource works.
	t.Run("an empty username works with a proxy that asks for no credentials", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		socks := startSOCKS5(t, withoutAuth)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true, "secureSocksProxyUsername": ""})
		if res.Status != backend.HealthStatusOk {
			t.Fatalf("health = %v %q, want ok", res.Status, res.Message)
		}
		if len(socks.connects()) == 0 {
			t.Error("the request did not go through the proxy")
		}
	})

	// Grafana's own configuration sets the TLS to the proxy, not the
	// datasource: told to try Skip TLS verify, the reader would change a
	// setting that does not apply to that hop.
	t.Run("names the proxy's TLS when the proxy's certificate fails", func(t *testing.T) {
		netbox := newFakeNetBox(t)
		proxyTLS := newQuietTLSServer(t) // a certificate the configured root CA did not sign
		addr := strings.TrimPrefix(proxyTLS.URL, "https://")
		cfg := grafanaProxyCfg(addr)
		maps.Copy(cfg, grafanaTLSToProxy(t))
		res := saveAndTestIn(t, cfg, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, addr, netbox.URL)
		if !strings.Contains(res.Message, "TLS connection to the proxy") || strings.Contains(res.Message, "Skip TLS verify") {
			t.Errorf("message %q does not put the TLS failure on the proxy's hop", res.Message)
		}
		if strings.Contains(res.Message, "can reach the NetBox URL") {
			t.Errorf("message %q asks whether the proxy reaches NetBox, which it never tried", res.Message)
		}
	})

	// The switch is on, but Grafana has no proxy configured: the SDK sends the
	// request direct, so nothing about it is the proxy's.
	t.Run("does not mention the proxy when Grafana has none", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		deadNetBox := "http://" + ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		inst, err := NewDatasource(context.Background(), backend.DataSourceInstanceSettings{
			UID:                     pdcTestUID,
			Type:                    "netboxlabs-netbox-datasource",
			JSONData:                []byte(`{"url": "` + deadNetBox + `", "enableSecureSocksProxy": true, "timeoutSeconds": 5}`),
			DecryptedSecureJSONData: map[string]string{"apiToken": "0123456789abcdef0123456789abcdef01234567"},
		})
		if err != nil {
			t.Fatalf("NewDatasource: %v", err)
		}
		res, err := inst.(*Datasource).CheckHealth(context.Background(), &backend.CheckHealthRequest{})
		if err != nil {
			t.Fatalf("CheckHealth: %v", err)
		}
		if res.Status != backend.HealthStatusError || mentionsTheProxy(res.Message) {
			t.Errorf("health = %v %q, want an error that does not mention a proxy", res.Status, res.Message)
		}
	})

	t.Run("does not mention a proxy the datasource does not use", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		deadNetBox := "http://" + ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": deadNetBox})
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		if mentionsTheProxy(res.Message) {
			t.Errorf("message %q blames a proxy this datasource does not use", res.Message)
		}
	})
}

// assertPointsAtTheURLTheProxyReaches checks the message for a failure NetBox
// itself caused on a proxied datasource: it does not blame the proxy, and it
// sends the reader to the NetBox URL as the proxy reaches it, not as Grafana
// would, since Grafana never connects to NetBox directly.
func assertPointsAtTheURLTheProxyReaches(t *testing.T, msg string) {
	t.Helper()
	if blamesTheProxy(msg) {
		t.Errorf("message %q blames the proxy for a failure that is NetBox's", msg)
	}
	if !strings.Contains(msg, "which the proxy (on Grafana Cloud, the PDC agent) connects to") || strings.Contains(msg, "reachable from Grafana") {
		t.Errorf("message %q does not point at the NetBox URL as the proxy reaches it", msg)
	}
}

// assertNamesTheProxyCredentials checks a credentials failure names the
// setting that carries the PDC network, and does not send the reader to check
// whether the proxy reaches NetBox: it never got that far.
func assertNamesTheProxyCredentials(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{"credentials", "secureSocksProxyUsername", "PDC network's ID"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "can reach the NetBox URL") {
		t.Errorf("message %q asks whether the proxy reaches NetBox, which it never tried", msg)
	}
}

// newQuietTLSServer runs an HTTPS server whose certificate no test trusts. It
// does not log the handshakes that fail against it, which is all it is for.
func newQuietTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// grafanaTLSToProxy is Grafana's configuration for TLS to the proxy: one
// self-signed certificate as both the root CA and the client certificate. A
// proxy whose certificate that CA did not sign fails the handshake.
func grafanaTLSToProxy(t *testing.T) map[string]string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "grafana"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return map[string]string{
		proxy.PluginSecureSocksProxyAllowInsecure:      "false",
		proxy.PluginSecureSocksProxyRootCAsContents:    cert,
		proxy.PluginSecureSocksProxyClientCertContents: cert,
		proxy.PluginSecureSocksProxyClientKeyContents:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}
}

// assertNamesTheProxyOnly checks a failure message names the proxy as the hop
// to look at, and prints neither the proxy's nor NetBox's address: the
// catalogue review requires that no message carry a host, port or path.
func assertNamesTheProxyOnly(t *testing.T, msg, proxyAddr, netboxURL string) {
	t.Helper()
	if !strings.Contains(msg, "secure socks proxy") {
		t.Errorf("message %q does not mention the secure socks proxy", msg)
	}
	for _, leak := range []string{proxyAddr, strings.TrimPrefix(netboxURL, "http://")} {
		if strings.Contains(msg, leak) {
			t.Errorf("message %q leaks %q", msg, leak)
		}
	}
}

// assertPointsAtTheAgent checks a failure in reaching NetBox through the proxy
// names the PDC agent, the hop that dials NetBox on Grafana Cloud.
func assertPointsAtTheAgent(t *testing.T, msg string) {
	t.Helper()
	if !strings.Contains(msg, "PDC agent") {
		t.Errorf("message %q does not mention the PDC agent", msg)
	}
}

// A query on a proxied datasource names the proxy too, and a direct one does
// not; neither prints the addresses the transport error carries.
func TestQueryErrorMessage_SecureSocksProxy(t *testing.T) {
	err := fmt.Errorf("request failed: %w", &net.OpError{Op: "dial", Net: "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 40123},
		Addr:   &net.TCPAddr{IP: net.IPv4(10, 9, 9, 9), Port: 8443},
		Err:    syscall.ECONNREFUSED})
	proxied, direct := queryErrorMessage(err, true), queryErrorMessage(err, false)
	if !strings.Contains(proxied, "secure socks proxy") || !strings.Contains(proxied, "refused") {
		t.Errorf("proxied message = %q, want the proxy named and the cause", proxied)
	}
	if strings.Contains(direct, "proxy") {
		t.Errorf("direct message = %q mentions a proxy", direct)
	}
	for _, msg := range []string{proxied, direct} {
		for _, leak := range []string{"10.1.2.3", "40123", "10.9.9.9", "8443"} {
			if strings.Contains(msg, leak) {
				t.Errorf("message %q leaks %q", msg, leak)
			}
		}
	}
}

// Panels and resource calls (the editor's pickers) word a failure by the
// datasource's route, as Save & test does: the proxy named when the datasource
// goes through it, never when it does not.
func TestErrorWording_FollowsTheDatasourcesRoute(t *testing.T) {
	for _, viaProxy := range []bool{true, false} {
		d := newTestDatasource(&fakeProvider{queryErr: fmt.Errorf("request failed: %w", errors.New("i/o timeout"))})
		d.viaProxy = viaProxy

		resp, err := d.QueryData(context.Background(), &backend.QueryDataRequest{
			Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices"}`)}},
		})
		if err != nil {
			t.Fatalf("QueryData: %v", err)
		}
		dr := resp.Responses["A"]
		if dr.Error == nil || strings.Contains(dr.Error.Error(), "secure socks proxy") != viaProxy {
			t.Errorf("viaProxy=%v: panel error = %v", viaProxy, dr.Error)
		}

		rec := httptest.NewRecorder()
		d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{"objectType":"dcim/devices"}`)))
		if body := rec.Body.String(); rec.Code < 400 || strings.Contains(body, "secure socks proxy") != viaProxy {
			t.Errorf("viaProxy=%v: resource answer = %d %s", viaProxy, rec.Code, body)
		}
	}
}

// mentionsTheProxy reports whether a message mentions Grafana's secure socks
// proxy or the PDC agent at all, which a direct datasource's must not. The bare
// word "proxy" is not enough: the not-JSON cause asks whether an HTTP proxy or
// login page answered, which is about the URL.
func mentionsTheProxy(msg string) bool {
	return strings.Contains(msg, "secure socks proxy") || strings.Contains(msg, "PDC")
}

// blamesTheProxy reports whether a message puts the failure on the proxy's
// route. A proxied datasource's message for a failure that is NetBox's still
// mentions the PDC agent, as the one that connects to the URL.
func blamesTheProxy(msg string) bool {
	return strings.Contains(msg, "through the secure socks proxy")
}

// socksHop wraps err as the SOCKS dialer wraps a failure before the tunnel to
// NetBox is up.
func socksHop(err error) error {
	return &net.OpError{Op: "socks connect", Net: "tcp", Err: err}
}

// The decision behind the wording, on the error shapes Go produces: the SOCKS
// dialer wraps a failure before the tunnel is up in a *net.OpError whose Op is
// "socks connect". Only those, or a cause the route decides (a refusal, a
// timeout, a dropped connection), name the proxy.
func TestProxiedCause(t *testing.T) {
	certErr := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	for _, tc := range []struct {
		name      string
		err       error
		wantCause string
		wantBlame bool
	}{
		{"NetBox's certificate", certErr, causeCertificate, false},
		{"the proxy's certificate", socksHop(certErr), causeProxyTLS, true},
		{"the proxy is not TLS", socksHop(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), causeProxyTLS, true},
		{"a SOCKS refusal with no category", socksHop(errors.New("unknown error general SOCKS server failure")), causeFailed, true},
		// An HTTP proxy from Grafana's environment wraps the dial in its own
		// *net.OpError, outside the SOCKS one.
		{"the SOCKS hop under an HTTP proxy", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: socksHop(errors.New("unknown error TTL expired"))}, causeFailed, true},
		{"credentials refused", socksHop(errors.New("username/password authentication failed")), causeProxyAuth, true},
		{"an answer NetBox sent that the client could not use", errors.New("malformed HTTP response"), causeFailed, false},
		{"a cancelled request", fmt.Errorf("request failed: %w", context.Canceled), causeCancelled, false},
		{"refused, on either hop", fmt.Errorf("request failed: %w", syscall.ECONNREFUSED), causeRefused, true},
		{"the proxy cannot reach the host", socksHop(errors.New("unknown error host unreachable")), causeUnreachable, true},
		// The client sends NetBox's name unresolved, so only the proxy's own
		// address can fail to resolve.
		{"the proxy's address does not resolve", socksHop(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "proxy", IsNotFound: true}}), causeUnresolved, true},
		{"timed out, on either hop", fmt.Errorf("request failed: %w", context.DeadlineExceeded), causeTimeout, true},
		{"reset, on either hop", fmt.Errorf("request failed: %w", syscall.ECONNRESET), causeReset, true},
		{"closed, on either hop", fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), causeClosed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if cause, blame := proxiedCause(tc.err); cause != tc.wantCause || blame != tc.wantBlame {
				t.Errorf("proxiedCause = %q, %v; want %q, %v", cause, blame, tc.wantCause, tc.wantBlame)
			}
		})
	}
}

// A datasource that does not use Grafana's secure socks proxy can still go
// through a SOCKS5 proxy from Grafana's environment (HTTPS_PROXY=socks5://…),
// whose client says the same words when it refuses credentials. Its message
// must not send the reader to settings the datasource does not use.
func TestErrorMessages_DirectRouteNeverMentionsTheProxySettings(t *testing.T) {
	err := fmt.Errorf("request failed: %w", socksHop(errors.New("username/password authentication failed")))
	for _, msg := range []string{healthErrorMessage(err, false), queryErrorMessage(err, false)} {
		if mentionsTheProxy(msg) || strings.Contains(msg, "secureSocksProxy") {
			t.Errorf("direct message %q mentions the secure socks proxy's settings", msg)
		}
	}
}

// Replica-cache mode builds its client the same way, so a cache only PDC can
// reach goes through the proxy too, and a failure to reach it is worded by
// that route as NetBox's is. The cache's own sentence says "reachable from
// Grafana", the one hop Grafana does not make on this route.
func TestSaveAndTest_ReplicaCacheThroughTheProxy(t *testing.T) {
	cacheMode := func(url string, extra map[string]any) map[string]any {
		jsonData := map[string]any{
			"mode":                     "replica-cache",
			"url":                      url,
			"netboxId":                 "nb-1",
			"enableSecureSocksProxy":   true,
			"secureSocksProxyUsername": "0d5c3a6e-pdc-network",
		}
		maps.Copy(jsonData, extra)
		return jsonData
	}
	namesTheCache := func(t *testing.T, msg string) {
		t.Helper()
		if !strings.Contains(msg, "replica-cache") || strings.Contains(msg, "NetBox") || strings.Contains(msg, "reachable from Grafana") {
			t.Errorf("message %q does not name replica-cache as the service the proxy dials", msg)
		}
	}

	t.Run("goes through the proxy, as the PDC network", func(t *testing.T) {
		cache := replicaStub(t, "")
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, cacheMode(cache.URL, nil))
		if res.Status != backend.HealthStatusOk {
			t.Fatalf("health = %v %q, want ok", res.Status, res.Message)
		}
		want := socksConnect{user: "0d5c3a6e-pdc-network", pass: "pdc-secret", target: strings.TrimPrefix(cache.URL, "http://")}
		if got := socks.connects(); !allConnects(got, want) {
			t.Errorf("proxy relayed %v, want only CONNECTs %+v", got, want)
		}
	})

	t.Run("names the proxy when it cannot reach replica-cache", func(t *testing.T) {
		cache := replicaStub(t, "")
		socks := startSOCKS5(t, refusing)
		res := saveAndTest(t, socks.addr, cacheMode(cache.URL, nil))
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, socks.addr, cache.URL)
		assertPointsAtTheAgent(t, res.Message)
		namesTheCache(t, res.Message)
	})

	// The timeout's advice names the service that was slow.
	t.Run("names the proxy when it never answers", func(t *testing.T) {
		cache := replicaStub(t, "")
		socks := startSOCKS5(t, silent)
		res := saveAndTest(t, socks.addr, cacheMode(cache.URL, map[string]any{"timeoutSeconds": 1}))
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "timed out") {
			t.Errorf("message %q does not say the request timed out", res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, socks.addr, cache.URL)
		namesTheCache(t, res.Message)
	})

	t.Run("names the proxy credentials when the proxy refuses them", func(t *testing.T) {
		cache := replicaStub(t, "")
		socks := startSOCKS5(t, rejectingCredentials)
		res := saveAndTest(t, socks.addr, cacheMode(cache.URL, nil))
		if res.Status != backend.HealthStatusError {
			t.Fatalf("health = %v %q, want an error", res.Status, res.Message)
		}
		assertNamesTheProxyOnly(t, res.Message, socks.addr, cache.URL)
		assertNamesTheProxyCredentials(t, res.Message)
		namesTheCache(t, res.Message)
	})
}

// A query in replica-cache mode words a failure to reach the cache by the
// datasource's route, as Save & test does. The cache's other sentences are
// about an answer it sent, which came back through a route that works, so they
// reach the reader as the provider wrote them on either route.
func TestQueryErrorMessage_ReplicaCacheThroughTheProxy(t *testing.T) {
	unreached := &replicacache.TransportError{Op: "requesting http://10.9.9.9:8443/v1/dcim/devices",
		Err: &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 9, 9, 9), Port: 8443}, Err: syscall.ECONNREFUSED}}
	direct := "Cannot reach replica-cache. Check the replica-cache URL and that the service is reachable from Grafana."

	proxied := queryErrorMessage(unreached, true)
	for _, want := range []string{"Couldn't reach replica-cache through the secure socks proxy", "refused", "PDC agent"} {
		if !strings.Contains(proxied, want) {
			t.Errorf("proxied message %q does not say %q", proxied, want)
		}
	}
	for _, unwanted := range []string{"NetBox", "10.9.9.9", "8443"} {
		if strings.Contains(proxied, unwanted) {
			t.Errorf("proxied message %q says %q", proxied, unwanted)
		}
	}
	if got := queryErrorMessage(unreached, false); got != direct {
		t.Errorf("direct message = %q, want the provider's %q", got, direct)
	}
	if got := healthErrorMessage(unreached, false); got != direct {
		t.Errorf("direct health message = %q, want the provider's %q", got, direct)
	}

	answered := &replicacache.TransportError{Op: "decoding response", Err: errors.New("invalid character"),
		Message: "Replica cache returned a response that could not be read. The service is reachable but answered with something unexpected."}
	for _, viaProxy := range []bool{true, false} {
		if got := queryErrorMessage(answered, viaProxy); got != answered.Message {
			t.Errorf("viaProxy=%v: message = %q, want the provider's %q", viaProxy, got, answered.Message)
		}
		if got := healthErrorMessage(answered, viaProxy); got != answered.Message {
			t.Errorf("viaProxy=%v: health message = %q, want the provider's %q", viaProxy, got, answered.Message)
		}
	}
}
