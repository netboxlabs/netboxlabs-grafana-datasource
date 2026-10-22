package plugin

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/proxy"
	"github.com/grafana/grafana-plugin-sdk-go/config"
)

// Save & test against a NetBox that only Grafana's secure socks proxy can
// reach: Private Data Source Connect on Grafana Cloud, an admin-run SOCKS proxy
// when self-hosted. The plugin has no proxy code of its own. The SDK applies
// the proxy when it builds the HTTP client, provided the client comes from
// settings.HTTPClientOptions, so these tests fail if newHTTPClient stops
// building it that way or anything on the way to the request swaps its
// transport.

// socksConnect is one CONNECT the proxy relayed: who asked, and where to.
type socksConnect struct {
	user, target string
}

type socksServer struct {
	addr string
	mu   sync.Mutex
	seen []socksConnect
}

func (s *socksServer) connects() []socksConnect {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// startSOCKS5 runs a SOCKS5 proxy (RFC 1928, with RFC 1929 username/password)
// that relays every CONNECT and records it. It prefers username/password when
// the client offers it, because that is where Grafana puts the PDC network.
func startSOCKS5(t *testing.T) *socksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &socksServer{addr: ln.Addr().String()}
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
	user := ""
	if slices.Contains(methods, 2) {
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
		if _, err := read(int(plen[0])); err != nil {
			return
		}
		user = string(name)
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
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = up.Close() }()
	s.mu.Lock()
	s.seen = append(s.seen, socksConnect{user: user, target: target})
	s.mu.Unlock()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(up, r) }()
	_, _ = io.Copy(c, up)
}

// fakeNetBoxStatus answers /api/status/ the way NetBox does.
func fakeNetBoxStatus(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"netbox-version": "4.4.10"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// saveAndTest builds the datasource the way Grafana does, inside a context that
// carries Grafana's proxy configuration, and runs its health check.
func saveAndTest(t *testing.T, proxyAddr string, jsonData map[string]any) *backend.CheckHealthResult {
	t.Helper()
	ctx := config.WithGrafanaConfig(context.Background(), config.NewGrafanaCfg(map[string]string{
		proxy.PluginSecureSocksProxyEnabled:       "true",
		proxy.PluginSecureSocksProxyProxyAddress:  proxyAddr,
		proxy.PluginSecureSocksProxyAllowInsecure: "true",
	}))
	raw, err := json.Marshal(jsonData)
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewDatasource(ctx, backend.DataSourceInstanceSettings{
		UID:                     "netbox-private",
		Name:                    "NetBox (private)",
		JSONData:                raw,
		DecryptedSecureJSONData: map[string]string{"apiToken": "0123456789abcdef0123456789abcdef01234567"},
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

func TestSaveAndTest_SecureSocksProxy(t *testing.T) {
	t.Run("goes through the proxy, as the PDC network, when the datasource enables it", func(t *testing.T) {
		netbox := fakeNetBoxStatus(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{
			"url":                      netbox.URL,
			"enableSecureSocksProxy":   true,
			"secureSocksProxyUsername": "0d5c3a6e-pdc-network",
		})
		if res.Status != backend.HealthStatusOk || !strings.Contains(res.Message, "4.4.10") {
			t.Fatalf("health = %v %q, want ok from NetBox 4.4.10", res.Status, res.Message)
		}
		want := socksConnect{user: "0d5c3a6e-pdc-network", target: strings.TrimPrefix(netbox.URL, "http://")}
		got := socks.connects()
		if len(got) == 0 || !slices.Contains(got, want) {
			t.Errorf("proxy relayed %v, want a CONNECT %+v", got, want)
		}
	})

	// Without a network name the SDK falls back to the datasource UID. Grafana
	// Cloud does not route that, but a self-hosted proxy may not care, and the
	// switch on the settings page sets no username.
	t.Run("sends the datasource UID when no network is named", func(t *testing.T) {
		netbox := fakeNetBoxStatus(t)
		socks := startSOCKS5(t)
		res := saveAndTest(t, socks.addr, map[string]any{"url": netbox.URL, "enableSecureSocksProxy": true})
		if res.Status != backend.HealthStatusOk {
			t.Fatalf("health = %v %q, want ok", res.Status, res.Message)
		}
		if got := socks.connects(); len(got) == 0 || got[0].user != "netbox-private" {
			t.Errorf("proxy relayed %v, want a CONNECT as the datasource UID", got)
		}
	})

	t.Run("stays direct when the datasource does not enable it", func(t *testing.T) {
		netbox := fakeNetBoxStatus(t)
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
	// quietly went direct: with the proxy down, Save & test has to fail.
	t.Run("fails rather than going direct when the proxy is down", func(t *testing.T) {
		netbox := fakeNetBoxStatus(t)
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
			t.Errorf("health = %v %q, want an error with the proxy down", res.Status, fmt.Sprint(res.Message))
		}
	})
}
