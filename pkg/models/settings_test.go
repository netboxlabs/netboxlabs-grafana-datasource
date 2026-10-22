package models

import (
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// The secure socks proxy's settings are read by the plugin SDK, which trusts
// their JSON types: with the proxy on, a secureSocksProxyUsername that is not a
// string panics inside the SDK on every request, and a flag that is not a JSON
// boolean is read as off. Only the shapes that break something are refused —
// the panic, and a flag written to mean on that the SDK would silently treat
// as off. A flag that reads as off is accepted as off, as the SDK and Grafana
// read it: refusing it would break a datasource that works today.
func TestLoadPluginSettings_SecureSocksProxyFields(t *testing.T) {
	for _, tc := range []struct {
		name, jsonData, wantErr string
	}{
		{"a network ID routes", `{"enableSecureSocksProxy": true, "secureSocksProxyUsername": "0d5c3a6e-pdc"}`, ""},
		{"no username falls back to the datasource UID", `{"enableSecureSocksProxy": true}`, ""},
		{"proxy off ignores the username", `{"enableSecureSocksProxy": false, "secureSocksProxyUsername": null}`, ""},
		{"neither field", `{"url": "https://netbox.example.com"}`, ""},
		{"a null flag is off", `{"enableSecureSocksProxy": null}`, ""},
		{"a flag written as \"false\" is off", `{"enableSecureSocksProxy": "false"}`, ""},
		{"a flag written as 0 is off", `{"enableSecureSocksProxy": 0}`, ""},
		{"keys are matched exactly, as the SDK reads them", `{"enableSecureSocksProxy": true, "SecureSocksProxyUsername": 42}`, ""},
		{"a null username with the proxy on", `{"enableSecureSocksProxy": true, "secureSocksProxyUsername": null}`, "secureSocksProxyUsername"},
		{"a numeric username with the proxy on", `{"enableSecureSocksProxy": true, "secureSocksProxyUsername": 42}`, "secureSocksProxyUsername"},
		{"an object username with the proxy on", `{"enableSecureSocksProxy": true, "secureSocksProxyUsername": {"id": "x"}}`, "secureSocksProxyUsername"},
		{"a flag written as \"true\" would silently be off", `{"enableSecureSocksProxy": "true"}`, "enableSecureSocksProxy"},
		{"a flag written as 1 would silently be off", `{"enableSecureSocksProxy": 1}`, "enableSecureSocksProxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPluginSettings(backend.DataSourceInstanceSettings{JSONData: []byte(tc.jsonData)})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("LoadPluginSettings(%s) = %v, want no error", tc.jsonData, err)
			case tc.wantErr == "":
				return
			case err == nil || !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("LoadPluginSettings(%s) = %v, want an error naming %s", tc.jsonData, err, tc.wantErr)
			}
			// The datasource's own settings are wrong, not the plugin: classified
			// as downstream, Grafana shows the message on panels too, rather than
			// "An error occurred within the plugin".
			if !backend.IsDownstreamError(err) {
				t.Errorf("error %v is not classified as downstream", err)
			}
			// Panels show it to every viewer: it describes the value, it does not
			// echo whatever was written there.
			if strings.Contains(err.Error(), `"id"`) {
				t.Errorf("error %q echoes the raw value", err)
			}
		})
	}
}
