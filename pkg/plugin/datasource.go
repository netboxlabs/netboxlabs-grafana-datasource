package plugin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/models"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

// Ensure Datasource implements the required SDK interfaces.
var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

// Datasource is a NetBox enrichment datasource instance. It is provider-backed:
// today the provider is the NetBox REST API (see pkg/provider).
type Datasource struct {
	cfg             *models.PluginSettings
	provider        provider.Provider
	resourceHandler backend.CallResourceHandler
}

// NewDatasource creates a new datasource instance for a given configuration.
func NewDatasource(ctx context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	cfg, err := models.LoadPluginSettings(settings)
	if err != nil {
		return nil, err
	}
	httpClient, err := newHTTPClient(ctx, cfg, settings)
	if err != nil {
		return nil, err
	}
	p, err := newProvider(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	ds := &Datasource{cfg: cfg, provider: p}
	ds.resourceHandler = httpadapter.New(ds.newRouter())
	return ds, nil
}

// newHTTPClient builds the upstream HTTP client from the Grafana SDK so that
// Grafana's proxy/TLS/timeout config and Private Data Source Connect (PDC) — the
// path to a customer's private NetBox from Grafana Cloud — are applied
// automatically. PDC requires a backend datasource using this client.
func newHTTPClient(ctx context.Context, cfg *models.PluginSettings, settings backend.DataSourceInstanceSettings) (*http.Client, error) {
	opts, err := settings.HTTPClientOptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("http client options: %w", err)
	}
	// Apply our explicit timeout / TLS-skip config on top of Grafana's defaults.
	timeouts := httpclient.DefaultTimeoutOptions
	if cfg.TimeoutSeconds > 0 {
		timeouts.Timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	opts.Timeouts = &timeouts
	if cfg.TLSSkipVerify {
		if opts.TLS == nil {
			opts.TLS = &httpclient.TLSOptions{}
		}
		opts.TLS.InsecureSkipVerify = true
	}
	return httpclient.New(opts)
}

// newProvider selects the enrichment backend based on the configured mode.
func newProvider(cfg *models.PluginSettings, httpClient *http.Client) (provider.Provider, error) {
	switch cfg.Mode {
	case models.ModeNetBox, "":
		return netbox.New(cfg.URL, cfg.Secrets.APIToken, httpClient,
			netbox.WithCursorPaging(cfg.FastPagingNoTotals),
			// The same number newHTTPClient puts on the client. The provider cannot
			// read it back off an http.Client it did not build, and it needs it to
			// size the utilization measurement budget: raising the timeout is how a
			// user says "I will wait", and it is the only such dial they have.
			netbox.WithRequestTimeout(time.Duration(cfg.TimeoutSeconds)*time.Second)), nil
	default:
		return nil, fmt.Errorf("unknown provider mode %q", cfg.Mode)
	}
}

// Dispose is called when the instance is replaced after a config change.
func (d *Datasource) Dispose() {}

// QueryData handles multiple queries and returns multiple responses.
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()
	fromAlert := isAlertRequest(req)
	for _, q := range req.Queries {
		response.Responses[q.RefID] = d.query(ctx, q, fromAlert)
	}
	return response, nil
}

// fromAlertHeader is the key Grafana puts in QueryDataRequest.Headers when the
// request is an alert-rule evaluation (including a rule preview via
// POST /api/v1/eval). It is the only signal that distinguishes the two, and it
// matters because alerting and dashboards want opposite things from a partial
// result: a dashboard shows what it has plus a notice, an alert must refuse.
//
// The objects query type has an explicit alertTable flag as well, but that flag
// says what SHAPE the rule wants its frame in, not who is asking: a rule can
// perfectly well evaluate a plain objects query, and the editor defaults the
// flag to false. So the header is the discriminator there too — it is what keeps
// fast paging (uncounted, ID-ordered pages) off every alert evaluation rather
// than only the ones that ticked the box. ip-enrichment has no such flag at all.

// isAlertRequest reports whether this QueryData call is an alert evaluation.
//
// It reads req.Headers directly and NOT via the SDK's GetHTTPHeader: that
// accessor (backend/http_headers.go) only surfaces the OAuth/cookie keys and
// keys carrying the "http_" forwarding prefix, and FromAlert is none of those,
// so GetHTTPHeader("FromAlert") returns "" no matter what Grafana sent.
// Verified against grafana-plugin-sdk-go v0.294.0 and live against Grafana 13.
//
// The comparison is case-insensitive on both key and value: Headers is a plain
// map with no canonicalization, so nothing guarantees the exact casing Grafana
// happens to use today.
func isAlertRequest(req *backend.QueryDataRequest) bool {
	if req == nil {
		return false
	}
	for k, v := range req.Headers {
		if strings.EqualFold(k, backend.FromAlertHeaderName) {
			return strings.EqualFold(strings.TrimSpace(v), "true")
		}
	}
	return false
}

// CheckHealth verifies the datasource can reach and authenticate to NetBox.
func (d *Datasource) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if d.cfg.URL == "" {
		return &backend.CheckHealthResult{Status: backend.HealthStatusError, Message: "NetBox URL is missing"}, nil
	}
	if d.cfg.Secrets == nil || d.cfg.Secrets.APIToken == "" {
		return &backend.CheckHealthResult{Status: backend.HealthStatusError, Message: "API token is missing"}, nil
	}

	msg, err := d.provider.HealthCheck(ctx)
	if err != nil {
		log.DefaultLogger.Warn("health check failed", "error", err)
		return &backend.CheckHealthResult{Status: backend.HealthStatusError, Message: healthErrorMessage(err)}, nil
	}
	return &backend.CheckHealthResult{Status: backend.HealthStatusOk, Message: msg}, nil
}

// CallResource powers the query editor (object types, fields, autocomplete,
// preview) and variable queries.
func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	return d.resourceHandler.CallResource(ctx, req, sender)
}
