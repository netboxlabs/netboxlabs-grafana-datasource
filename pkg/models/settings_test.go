package models

import (
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// The secure socks proxy's settings are read by the plugin SDK, which trusts
// their JSON types: with the proxy on, a secureSocksProxyUsername that is not a
// string panics inside the SDK on every request, and an enableSecureSocksProxy
// that is not a boolean means "off" while Grafana's switch shows it on. Both
// come from hand-written API calls or provisioning, so they are refused here,
// by name, instead.
func TestLoadPluginSettings_SecureSocksProxyFields(t *testing.T) {
	load := func(jsonData string) error {
		_, err := LoadPluginSettings(backend.DataSourceInstanceSettings{JSONData: []byte(jsonData)})
		return err
	}

	for _, tc := range []struct {
		name, jsonData, wantErr string
	}{
		{"a network ID routes", `{"enableSecureSocksProxy": true, "secureSocksProxyUsername": "0d5c3a6e-pdc"}`, ""},
		{"no username falls back to the datasource UID", `{"enableSecureSocksProxy": true}`, ""},
		{"proxy off ignores the username", `{"enableSecureSocksProxy": false, "secureSocksProxyUsername": null}`, ""},
		{"neither field", `{"url": "https://netbox.example.com"}`, ""},
		{"a null username with the proxy on", `{"enableSecureSocksProxy": true, "secureSocksProxyUsername": null}`, "secureSocksProxyUsername"},
		{"a numeric username with the proxy on", `{"enableSecureSocksProxy": true, "secureSocksProxyUsername": 42}`, "secureSocksProxyUsername"},
		{"the flag as a string", `{"enableSecureSocksProxy": "true"}`, "enableSecureSocksProxy"},
		{"the flag as a number", `{"enableSecureSocksProxy": 1}`, "enableSecureSocksProxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := load(tc.jsonData)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("LoadPluginSettings(%s) = %v, want no error", tc.jsonData, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("LoadPluginSettings(%s) = %v, want an error naming %s", tc.jsonData, err, tc.wantErr)
			}
		})
	}
}
