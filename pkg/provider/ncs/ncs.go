// Package ncs is a placeholder for the Network Context Service provider.
//
// NCS is a high-volume enrichment projection of NetBox, tuned for delivering
// topology/context to observability tools at scale, intended for NetBox
// Cloud/Enterprise customers. The datasource selects a provider via its "mode"
// setting (see models.ProviderMode); this package will host the ModeNCS
// implementation as a fast-follow.
//
// It exists today so the provider seam is real and exercised: New returns a
// provider.Provider whose methods report that the mode is not yet available,
// rather than the datasource having to special-case a nil provider. When the
// NCS client lands, only the bodies below change — the datasource, query frame
// building, resources and annotations are all written against provider.Provider
// and need no modification.
package ncs

import (
	"context"
	"errors"
	"net/http"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// ErrNotImplemented is returned by every method until the NCS client ships.
var ErrNotImplemented = errors.New("NCS mode is not yet available; use NetBox mode")

// Provider is the (not yet implemented) NCS-backed enrichment provider.
type Provider struct {
	baseURL string
}

// New constructs an NCS provider bound to an endpoint.
func New(baseURL, _ string, _ *http.Client) *Provider {
	return &Provider{baseURL: baseURL}
}

func (p *Provider) Name() string    { return "ncs" }
func (p *Provider) BaseURL() string { return p.baseURL }

func (p *Provider) HealthCheck(context.Context) (string, error) { return "", ErrNotImplemented }

func (p *Provider) ObjectTypes(context.Context) ([]provider.ObjectType, error) {
	return nil, ErrNotImplemented
}

func (p *Provider) Fields(context.Context, string) ([]provider.Field, error) {
	return nil, ErrNotImplemented
}

func (p *Provider) Query(context.Context, provider.QuerySpec) (*provider.Result, error) {
	return nil, ErrNotImplemented
}

func (p *Provider) FieldValues(context.Context, string, string, string, int) ([]string, error) {
	return nil, ErrNotImplemented
}

func (p *Provider) Changes(context.Context, provider.ChangeSpec) ([]provider.Change, error) {
	return nil, ErrNotImplemented
}

func (p *Provider) ResolveIPs(context.Context, []string, []string, int) (*provider.Result, error) {
	return nil, ErrNotImplemented
}

func (p *Provider) Topology(context.Context, provider.TopologySpec) (*provider.Graph, error) {
	return nil, ErrNotImplemented
}
