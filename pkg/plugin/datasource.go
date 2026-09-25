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
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/replicacache"
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
// missingSetting names the first required setting this datasource has not been
// given, or "" when it can be reached. It mirrors newProvider's switch: a mode
// added there needs its prerequisites added here, or Save & Test will check the
// wrong ones. Both modes read URL and APIToken; the message names the service
// the mode talks to.
// Trimmed, because the clients trim: a whitespace-only URL becomes an empty
// request URL and a whitespace-only instance id an empty tenant header, so an
// untrimmed check passed the value through to be reported as an unreachable
// service or an upstream rejection — an outage message for the configuration
// problem this function exists to name.
func missingSetting(cfg *models.PluginSettings) string {
	switch cfg.Mode {
	case models.ModeReplicaCache:
		// A Max data age that cannot be read must be fixed before it silently
		// means "off". Only in this mode: the editor shows the field here
		// alone, and a value left behind by a mode switch must not fail a
		// NetBox-mode datasource on a field it cannot see.
		if _, err := cfg.MaxDataAgeDuration(); err != nil {
			return "Max data age " + strings.TrimPrefix(err.Error(), "max data age ")
		}
		switch {
		case strings.TrimSpace(cfg.URL) == "":
			return "replica-cache URL is missing"
		case strings.TrimSpace(cfg.NetBoxID) == "":
			return "NetBox instance ID is missing"
		case cfg.Secrets == nil || strings.TrimSpace(cfg.Secrets.APIToken) == "":
			return "API token is missing"
		}
		return ""
	default:
		switch {
		case strings.TrimSpace(cfg.URL) == "":
			return "NetBox URL is missing"
		case cfg.Secrets == nil || strings.TrimSpace(cfg.Secrets.APIToken) == "":
			return "API token is missing"
		}
		return ""
	}
}

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
	case models.ModeReplicaCache:
		// Constructed even when the URL or the instance id is missing, exactly as
		// NetBox mode is with an empty URL. Failing here failed NewDatasource, so
		// no Datasource existed for CheckHealth to run on and Save & Test never
		// reached missingSetting above — the one place that can name WHICH field
		// is absent. A provisioned datasource missing its URL reported a
		// construction failure instead of "replica-cache URL is missing".
		// "View in NetBox" links come from the NetBox URL the replica's own
		// catalogue reports; nothing is configured for them here.
		return replicacache.New(cfg.URL, cfg.Secrets.APIToken, cfg.NetBoxID, httpClient), nil
	default:
		return nil, fmt.Errorf("unknown provider mode %q", cfg.Mode)
	}
}

// Dispose is called when the instance is replaced after a config change.
func (d *Datasource) Dispose() {}

// QueryData handles multiple queries and returns multiple responses.
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()
	c := requestConsumer(req)
	for _, q := range req.Queries {
		response.Responses[q.RefID] = d.query(ctx, q, c)
	}
	return response, nil
}

// consumer says what will read a query's frames. It decides one thing: whether a
// partial result may be returned with its gap stated in a frame notice, or has to
// be refused because nothing downstream would ever show that notice.
type consumer int

const (
	// consumerDashboard is a panel, Explore or a variable: the frame reaches a
	// reader with its notices intact, so partial beats none.
	consumerDashboard consumer = iota
	// consumerExpression is a query whose frame feeds a server-side expression.
	consumerExpression
	// consumerAlert is an alert-rule evaluation (see isAlertRequest).
	consumerAlert
)

// strict reports whether a partial result must fail instead of carrying a notice.
func (c consumer) strict() bool { return c != consumerDashboard }

// refusalVoice is how a refused partial result is worded for whoever asked: the
// subject, and the consequence clause of each refusal, written out in full for
// each reader rather than assembled from swapped verbs, because "alert on a
// device" and "compute on a device" are not the same sentence with one word
// changed.
type refusalVoice struct {
	subject    string // what failed, as the reader knows it
	truncated  string // …returned N of M, so <truncated>
	degraded   string // …returned a degraded result, so <degraded>
	links      string // …returned an incomplete set of links, so <links>
	unmeasured string // …measured X for N of M rows, so the other K <unmeasured>
	stale      string // …read data that is X old / of unknown age, so <stale>
}

// voice addresses the refusal to its reader. Someone whose dashboard panel broke
// has no alert rule to go and look for, so the expression path says so; every
// other strict path is a rule being evaluated, previewed or written — the
// alertTable shape refuses for a dashboard consumer too — and keeps the alert
// wording.
func (c consumer) voice() refusalVoice {
	if c == consumerExpression {
		return refusalVoice{
			subject:    "Query feeding an expression",
			truncated:  "the expression would compute on an incomplete result",
			degraded:   "the expression would compute on data that is missing for a reason the numbers cannot show",
			links:      "the expression would see a device as less connected than it is",
			unmeasured: "would reach the expression blank instead of with the values they hold",
			stale:      "the expression would compute on a stale replica",
		}
	}
	return refusalVoice{
		subject:    "Alert query",
		truncated:  "it would alert on an incomplete result",
		degraded:   "it would alert on data that is missing for a reason the numbers cannot show",
		links:      "it would alert on a device that may have a working path it cannot see",
		unmeasured: "would evaluate as zero rather than as the values they hold",
		stale:      "it would alert on a stale replica",
	}
}

// fromExprHeader is the header Grafana sets on a query whose result feeds a
// server-side expression (SQL, math, reduce). The frontend adds it to any panel
// request that contains an expression, and a backend calling /api/ds/query can
// send it for the same purpose.
//
// It matters for the reason FromAlert does: expressions drop the input frame's
// meta.notices, so a truncated result reaches the panel as a plausible wrong
// number with nothing to say it is partial.
const fromExprHeader = "X-Grafana-From-Expr"

// requestConsumer classifies a QueryData call. Alert wins when both are set — a
// rule with a SQL expression carries both, and alert is the stricter reading.
//
// Unlike FromAlert (see isAlertRequest), the expression header IS forwarded with
// the "http_" prefix, so the SDK accessor is the right way to read it and a
// direct lookup of the bare name would find nothing. Verified against
// grafana-plugin-sdk-go v0.296.4 and live against Grafana 13.
func requestConsumer(req *backend.QueryDataRequest) consumer {
	if req == nil {
		return consumerDashboard
	}
	if isAlertRequest(req) {
		return consumerAlert
	}
	if strings.EqualFold(strings.TrimSpace(req.GetHTTPHeader(fromExprHeader)), "true") {
		return consumerExpression
	}
	return consumerDashboard
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
	// The prerequisites differ per backend, and checking NetBox's against a
	// replica-cache datasource fails Save & Test on a correctly configured
	// instance — the two authenticate with different credentials against
	// different services, and replica-cache never uses the NetBox token.
	if msg := missingSetting(d.cfg); msg != "" {
		return &backend.CheckHealthResult{Status: backend.HealthStatusError, Message: msg}, nil
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
