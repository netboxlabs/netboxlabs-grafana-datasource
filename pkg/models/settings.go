package models

import (
	"encoding/json"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// ProviderMode selects which enrichment backend the datasource talks to.
type ProviderMode string

const (
	// ModeNetBox queries the NetBox REST API directly. This is the default and
	// the only mode implemented today.
	ModeNetBox ProviderMode = "netbox"
)

// PluginSettings holds the non-secret configuration for a datasource instance.
type PluginSettings struct {
	// URL is the base URL of the NetBox instance, e.g.
	// https://netbox.example.com — without a trailing /api.
	URL string `json:"url"`
	// PublicURL, when set, is where users' browsers reach NetBox if that
	// differs from URL (compose/k8s service DNS). Deep-link URLs in results
	// are rewritten from URL's base to PublicURL's. Empty = no rewrite.
	PublicURL string `json:"publicUrl"`
	// Mode selects the enrichment backend. Defaults to "netbox" — the only
	// implemented backend today. The field is kept as the seam for a planned
	// second, high-volume backend; there is no UI selector until that ships.
	Mode ProviderMode `json:"mode"`
	// TLSSkipVerify disables TLS certificate verification (self-signed certs).
	TLSSkipVerify bool `json:"tlsSkipVerify"`
	// TimeoutSeconds bounds individual upstream HTTP requests. Defaults to 30.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// FastPagingNoTotals opts this datasource into NetBox cursor pagination for
	// table queries: NetBox pages by primary key and skips counting the matches,
	// which is where the time goes on a model holding millions of rows.
	//
	// It is OFF by default and must stay that way. What it buys is not worth
	// measuring below a few hundred thousand objects, while what it costs is
	// visible at every size: rows come back in ID order rather than the model's
	// natural order, and the match count is gone, so a panel cannot say
	// "showing 100 of N".
	//
	// Query paths that need the count never use it (provider.QuerySpec's
	// AllowUncounted is opt-in per query), and no alert evaluation ever asks for
	// it: pkg/plugin.query gates AllowUncounted on the FromAlert header, so
	// alerting's count, truncation and ordering keep their real values
	// regardless of this setting — for every rule, not only the ones whose query
	// uses the Alert table shape.
	FastPagingNoTotals bool `json:"fastPagingNoTotals"`

	Secrets *SecretPluginSettings `json:"-"`
}

// SecretPluginSettings holds values that are encrypted at rest by Grafana and
// only ever decrypted server-side.
type SecretPluginSettings struct {
	// APIToken is the NetBox API token used for `Authorization: Token <token>`.
	APIToken string `json:"apiToken"`
}

// LoadPluginSettings parses datasource instance settings, applies defaults and
// attaches decrypted secrets.
func LoadPluginSettings(source backend.DataSourceInstanceSettings) (*PluginSettings, error) {
	settings := PluginSettings{}
	if len(source.JSONData) > 0 {
		if err := json.Unmarshal(source.JSONData, &settings); err != nil {
			return nil, fmt.Errorf("could not unmarshal PluginSettings json: %w", err)
		}
	}

	if settings.Mode == "" {
		settings.Mode = ModeNetBox
	}
	if settings.TimeoutSeconds <= 0 {
		settings.TimeoutSeconds = 30
	}

	settings.Secrets = loadSecretPluginSettings(source.DecryptedSecureJSONData)

	return &settings, nil
}

func loadSecretPluginSettings(source map[string]string) *SecretPluginSettings {
	return &SecretPluginSettings{
		APIToken: source["apiToken"],
	}
}
