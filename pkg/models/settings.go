package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// ProviderMode selects which enrichment backend the datasource talks to.
type ProviderMode string

const (
	// ModeNetBox queries the NetBox REST API directly. This is the default.
	ModeNetBox ProviderMode = "netbox"
	// ModeReplicaCache queries the NetBox replica-cache read API: a columnar
	// mirror built to answer at a scale the REST API cannot serve interactively.
	//
	// It is not a drop-in replacement. It cannot serve annotations, IP
	// enrichment or topology, and it says so explicitly rather than returning
	// empty results; see pkg/provider/replicacache.
	ModeReplicaCache ProviderMode = "replica-cache"
)

// PluginSettings holds the non-secret configuration for a datasource instance.
type PluginSettings struct {
	// URL is the base URL of the service this datasource reads from: the
	// NetBox instance (https://netbox.example.com, without a trailing /api) in
	// NetBox mode, the replica-cache deployment in replica-cache mode. One
	// field for both, because a datasource talks to exactly one of them.
	URL string `json:"url"`
	// PublicURL, when set, is where users' browsers reach NetBox if that
	// differs from the base the links are built from — URL in NetBox mode
	// (compose/k8s service DNS), the NetBox URL the replica reports in
	// replica-cache mode. Deep-link URLs in results are rewritten from that
	// base to PublicURL's. Empty = no rewrite.
	PublicURL string `json:"publicUrl"`
	// Mode selects the enrichment backend. Defaults to "netbox".
	Mode ProviderMode `json:"mode"`
	// NetBoxID identifies the NetBox instance the cache is holding, sent as the
	// NBC-Netbox-ID header. The service rejects requests without it.
	NetBoxID string `json:"netboxId"`
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
	// MaxDataAge, when set, is how old a replica-cache result may be before an
	// alert rule or an expression-fed query refuses it rather than evaluating
	// a stale inventory as current — a Go duration such as "15m". Empty means
	// never refuse ON AGE: a replica can legitimately report no age for an
	// entity once its snapshot is complete, and dashboards only show the age.
	// (A replica still loading its initial snapshot is refused regardless, as
	// a degraded result.) Read in replica-cache mode alone, where the editor
	// shows it; NetBox mode ignores it, including a value that does not parse.
	// See MaxDataAgeDuration.
	MaxDataAge string `json:"maxDataAge"`

	Secrets *SecretPluginSettings `json:"-"`
}

// MaxDataAgeDuration parses MaxDataAge. Empty is off (0). An unparseable or
// negative value is an ERROR, not off: the setting exists to make a rule
// refuse stale data, and a typo that silently disabled it would be the one
// wrong direction. Save & Test and the strict query paths both report it.
func (s *PluginSettings) MaxDataAgeDuration() (time.Duration, error) {
	raw := strings.TrimSpace(s.MaxDataAge)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("max data age %q is not a duration such as 15m or 2h", raw)
	}
	if d < 0 {
		return 0, fmt.Errorf("max data age %q is negative", raw)
	}
	return d, nil
}

// SecretPluginSettings holds values that are encrypted at rest by Grafana and
// only ever decrypted server-side.
type SecretPluginSettings struct {
	// APIToken is the credential for the service URL names: a NetBox API token
	// (`Authorization: Token <token>`) in NetBox mode, the replica-cache bearer
	// token in replica-cache mode. One field, as URL is one field.
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
