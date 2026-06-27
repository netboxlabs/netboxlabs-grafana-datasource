package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"

	"github.com/netboxlabs/netbox/pkg/models"
	"github.com/netboxlabs/netbox/pkg/provider"
	"github.com/netboxlabs/netbox/pkg/provider/ncs"
	"github.com/netboxlabs/netbox/pkg/provider/netbox"
)

// Ensure Datasource implements the required SDK interfaces.
var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

// Datasource is a NetBox enrichment datasource instance. It is provider-backed:
// today the provider is the NetBox REST API, with NCS planned (see pkg/provider).
type Datasource struct {
	cfg             *models.PluginSettings
	provider        provider.Provider
	resourceHandler backend.CallResourceHandler
}

// NewDatasource creates a new datasource instance for a given configuration.
func NewDatasource(_ context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	cfg, err := models.LoadPluginSettings(settings)
	if err != nil {
		return nil, err
	}
	p, err := newProvider(cfg)
	if err != nil {
		return nil, err
	}
	ds := &Datasource{cfg: cfg, provider: p}
	ds.resourceHandler = httpadapter.New(ds.newRouter())
	return ds, nil
}

// newProvider selects the enrichment backend based on the configured mode.
func newProvider(cfg *models.PluginSettings) (provider.Provider, error) {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	switch cfg.Mode {
	case models.ModeNCS:
		return ncs.New(cfg.URL, cfg.Secrets.APIToken, cfg.TLSSkipVerify, timeout), nil
	case models.ModeNetBox, "":
		return netbox.New(cfg.URL, cfg.Secrets.APIToken, cfg.TLSSkipVerify, timeout), nil
	default:
		return nil, fmt.Errorf("unknown provider mode %q", cfg.Mode)
	}
}

// Dispose is called when the instance is replaced after a config change.
func (d *Datasource) Dispose() {}

// QueryData handles multiple queries and returns multiple responses.
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()
	for _, q := range req.Queries {
		response.Responses[q.RefID] = d.query(ctx, q)
	}
	return response, nil
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
