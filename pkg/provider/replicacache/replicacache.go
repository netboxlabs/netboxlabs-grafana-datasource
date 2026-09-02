// Package replicacache implements provider.Provider against the NetBox
// replica-cache read API: a columnar, read-only mirror of a NetBox instance
// built for scale that the REST API cannot serve interactively.
//
// It exists for one measured problem. On a multi-million-object instance a
// dashboard panel backed by the REST API times out, because the cost is in
// NetBox's own serialization and counting rather than in anything the plugin
// does. replica-cache answers the same questions from a copy of the underlying
// tables, and pushes filtering, sorting, projection and counting down into it.
//
// The trade is fidelity. It serves raw table rows, so every relationship is a
// bare integer id, and a handful of NetBox concepts have no representation at
// all — the change log, contacts, and the content-type table that would say
// what an IP is attached to. What can be reconstructed is reconstructed (see
// fk.go); what cannot is refused explicitly rather than returned as an empty
// result that looks like an answer.
package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// schemaTTL bounds how long the discovered entity list is reused.
const schemaTTL = 10 * time.Minute

// Provider is the replica-cache implementation of provider.Provider.
type Provider struct {
	client *Client
	fk     *fkCache
	// netboxURL is the NetBox instance this cache mirrors, used only to build
	// deep links back into the NetBox UI. Empty means no links are produced.
	netboxURL string

	mu       sync.Mutex
	entities []provider.ObjectType
	expires  time.Time

	fieldsMu sync.Mutex
	fields   map[string]fieldsCacheEntry
}

type fieldsCacheEntry struct {
	fields []provider.Field
	raw    map[string]bool // column names as they exist upstream
	types  map[string]provider.FieldType
	// complete is false when FK resolution degraded while this entry was built,
	// so `fields` is missing the resolved columns. The entry is still cached,
	// because `raw` and `types` come from the main-table sample and are correct
	// either way — and they are what decides which columns may be FILTERED. An
	// entry withheld entirely made rawColumns return nil, which FilterFields
	// read as "no restriction" and used to advertise site and cf_* as
	// filterable; selecting one sends a synthesized name upstream as a physical
	// column and answers 400.
	complete bool
	expires  time.Time
}

// sampleRows is how many rows Fields reads to learn the columns and their
// types. More than one, because a column that is null in the sampled row cannot
// be typed, and a mistyped column is not cosmetic here: the operators a column
// is offered depend on whether it holds text (see filterFieldsFor). Twenty is
// one small page and resolves the common case of a sparsely populated column.
const sampleRows = 20

// Option configures a Provider.
type Option func(*Provider)

// WithNetBoxURL supplies the NetBox instance this cache mirrors, so rows can
// carry a link back to the object in the NetBox UI. replica-cache serves
// database rows and cannot produce that link itself.
func WithNetBoxURL(base string) Option {
	return func(p *Provider) { p.netboxURL = base }
}

// New builds a replica-cache provider. netboxID is the tenant identifier sent
// as NBC-Netbox-ID; the service rejects requests without it.
func New(base, token, netboxID string, httpClient *http.Client, opts ...Option) *Provider {
	p := &Provider{
		client: NewClient(base, token, netboxID, httpClient),
		fk:     newFKCache(),
		fields: map[string]fieldsCacheEntry{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Provider) Name() string { return "replica-cache" }

// BaseURL returns the base the result's deep links were built from, which is
// what the seam documents it as and what the plugin layer uses it for:
// rewriting links from an internal host to a browser-facing one (publicUrl).
//
// For this provider that is the NETBOX base, not the cache's own root. The
// links point at objects in the NetBox UI — the cache has no UI — so returning
// the cache root would leave the rewrite prefix matching nothing and hand the
// user an internal, unreachable NetBox URL.
//
// It falls back to the cache root when no NetBox URL is configured. Nothing is
// linkable in that case, so there is nothing to rewrite, and the value is only
// ever used as a prefix to match.
func (p *Provider) BaseURL() string {
	if p.netboxURL != "" {
		return strings.TrimRight(strings.TrimSpace(p.netboxURL), "/")
	}
	return p.client.BaseURL()
}

// UnsupportedError is a capability this backend does not have. It is not a
// failure and not the user's mistake: the answer does not exist here.
type UnsupportedError struct {
	// Feature names the capability in the words the reader would use.
	Feature string
	// Detail is a complete, self-authored sentence explaining the gap and what
	// to do instead. It never contains upstream response text.
	Detail string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("replica-cache does not support %s: %s", e.Feature, e.Detail)
}

// Classification reports the gap as unsupported so the plugin layer states it
// as a capability limit rather than as an outage or a rejected request.
func (e *UnsupportedError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{Kind: provider.ErrorKindUnsupported, Detail: e.Detail}
}

// UnknownObjectTypeError is an object type this deployment does not serve.
type UnknownObjectTypeError struct {
	Type  string
	Known int
}

func (e *UnknownObjectTypeError) Error() string {
	return fmt.Sprintf("unknown replica-cache object type %q: not one of the %d types this deployment reports", e.Type, e.Known)
}

// Classification reports the type as the user's input, answered as a bad
// request rather than an upstream failure.
func (e *UnknownObjectTypeError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{
		Kind:       provider.ErrorKindUnknownObjectType,
		ObjectType: e.Type,
		KnownTypes: e.Known,
	}
}

// HealthCheck verifies connectivity, credentials and the tenant header.
func (p *Provider) HealthCheck(ctx context.Context) (string, error) {
	types, err := p.ObjectTypes(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Connected to replica-cache (%d object types)", len(types)), nil
}

// swaggerDoc is the subset of the service's API description we read. The
// document is Swagger 2.0 and describes no per-entity schemas — every list
// endpoint returns a generic "row" object whose columns depend on the
// deployment — so it is useful for discovering WHICH entities exist and for
// nothing else. Columns come from sampling a row (see Fields).
type swaggerDoc struct {
	Paths map[string]json.RawMessage `json:"paths"`
}

// ObjectTypes lists the entities this deployment serves.
func (p *Provider) ObjectTypes(ctx context.Context) ([]provider.ObjectType, error) {
	p.mu.Lock()
	if p.entities != nil && time.Now().Before(p.expires) {
		out := p.entities
		p.mu.Unlock()
		return out, nil
	}
	p.mu.Unlock()

	var doc swaggerDoc
	if err := p.client.get(ctx, "/docs/openapi.json", nil, &doc); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var out []provider.ObjectType
	for path := range doc.Paths {
		app, model, ok := parseEntityPath(path)
		if !ok {
			continue
		}
		value := app + "/" + model
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, provider.ObjectType{
			Value: value,
			Label: humanize(model),
			App:   app,
			Model: model,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })

	p.mu.Lock()
	p.entities = out
	p.expires = time.Now().Add(schemaTTL)
	p.mu.Unlock()
	return out, nil
}

// parseEntityPath accepts "/v1/dcim/devices" and rejects "/v1/dcim/devices/{id}"
// and anything else, so only listable collections become object types.
func parseEntityPath(path string) (app, model string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", "", false
	}
	if strings.ContainsAny(parts[1]+parts[2], "{}") {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// acronyms are rendered upper-case in labels, so the editor reads "IP
// Addresses" rather than "Ip Addresses".
var acronyms = map[string]string{
	"ip": "IP", "vlan": "VLAN", "vrf": "VRF", "asn": "ASN", "rir": "RIR",
	"vpn": "VPN", "l2vpn": "L2VPN", "fhrp": "FHRP", "ike": "IKE",
	"ipsec": "IPsec", "mac": "MAC", "oob": "OOB", "vm": "VM", "wan": "WAN",
	"ip4": "IPv4", "ip6": "IPv6",
}

func humanize(model string) string {
	words := strings.Split(model, "-")
	for i, w := range words {
		if a, ok := acronyms[w]; ok {
			words[i] = a
			continue
		}
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// entitySet returns the discovered entities as a lookup set.
func (p *Provider) entitySet(ctx context.Context) (map[string]bool, error) {
	types, err := p.ObjectTypes(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(types))
	for _, t := range types {
		set[t.Value] = true
	}
	return set, nil
}

// validateObjectType rejects a type this deployment does not serve, before any
// request is built. Doing it here rather than letting the service answer 404
// is what makes the message actionable: the reader learns it is not one of the
// N types available, not that a URL was not found.
func (p *Provider) validateObjectType(ctx context.Context, objectType string) error {
	set, err := p.entitySet(ctx)
	if err != nil {
		// Discovery being unavailable must not take the datasource down with it.
		// The entity list is served by one large document, and it was measured
		// failing (TLS timeouts, truncated bodies) against an instance whose row
		// endpoints were still answering — so blocking every query on it would
		// turn a slow endpoint into a total outage.
		//
		// Proceeding is safe because the query itself is authoritative: an object
		// type this deployment does not serve answers 404, which classifies as
		// not-found and reads correctly. What is lost is only the better message
		// (naming how many types DO exist), which is not worth the availability.
		return nil
	}
	if !set[objectType] {
		return &UnknownObjectTypeError{Type: objectType, Known: len(set)}
	}
	return nil
}

// Fields returns the columns available for an object type.
//
// The API description carries no schemas, so the columns are read off a sample
// row — the same approach the NetBox provider uses for plugin models, and for
// the same reason: it works for whatever the deployment actually holds instead
// of what a spec claims it holds. The sample is passed through the same
// flattening and FK resolution as a real query, so the editor offers exactly
// the columns a query will return, including the resolved names (site) and
// custom fields (cf_*) that are not columns upstream.
func (p *Provider) Fields(ctx context.Context, objectType string) ([]provider.Field, error) {
	if err := p.validateObjectType(ctx, objectType); err != nil {
		return nil, err
	}
	p.fieldsMu.Lock()
	if e, ok := p.fields[objectType]; ok && e.complete && time.Now().Before(e.expires) {
		p.fieldsMu.Unlock()
		return e.fields, nil
	}
	p.fieldsMu.Unlock()

	raws, _, err := p.client.list(ctx, objectType, nil, sampleRows)
	if err != nil {
		return nil, err
	}
	if len(raws) == 0 {
		// An empty table is not an error: the type exists and a query against it
		// legitimately returns nothing. There is simply no sample to learn
		// columns from, so the editor falls back to free text.
		return nil, nil
	}

	rawCols := map[string]bool{}
	var obj map[string]interface{}
	if err := json.Unmarshal(raws[0], &obj); err != nil {
		return nil, fmt.Errorf("reading sample row for %s: %w", objectType, err)
	}
	for k := range obj {
		rawCols[k] = true
	}

	cols, rows := flattenRows(raws)
	if addDeepLinks(p.netboxURL, objectType, rows) {
		cols = append(cols, deepLinkColumn)
	}
	added, degraded := p.resolveFKs(ctx, objectType, rows)
	cols = append(cols, added...)

	// Type each column from the first NON-NULL value seen across the sample. A
	// column that is null everywhere in the sample stays unknown, which
	// fieldType reports as string; filterFieldsFor treats only a confirmed
	// string as text-searchable, so an unknown column loses the text operators
	// rather than being offered one that may match every row.
	types := make(map[string]provider.FieldType, len(cols))
	known := make(map[string]bool, len(cols))
	for _, c := range cols {
		for _, row := range rows {
			if v, ok := row[c]; ok && v != nil {
				types[c] = fieldType(v)
				known[c] = true
				break
			}
		}
	}

	fields := make([]provider.Field, 0, len(cols))
	for _, c := range cols {
		t := provider.FieldTypeString
		if known[c] {
			t = types[c]
		} else {
			// Never typed: not text as far as operator selection is concerned.
			types[c] = provider.FieldType("")
		}
		fields = append(fields, provider.Field{Name: c, Type: t})
	}
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })

	// The entry is always stored, but only a COMPLETE one is served back from
	// the cache by Fields.
	//
	// The resolved columns exist only if every dimension answered, so a single
	// timed-out dimension yields a SHORTER list — measured live as 47 and 53
	// columns where a healthy run returns 56, losing site, role, device_type,
	// location and their slugs. Serving that from cache would pin the loss for
	// the whole TTL: the editor would stop offering columns that queries keep
	// returning, with no way to tell a column that never exists from one that
	// briefly failed to resolve. Marking it incomplete makes the next call
	// re-read a sample, which costs one small request and self-heals.
	//
	// Storing it anyway matters just as much. raw and types come from the
	// main-table sample, which succeeded, and they are what decide which columns
	// may be filtered and with which operators. Withholding the entry made
	// rawColumns return nil, and FilterFields reads an empty map as "no
	// restriction" — advertising site and cf_* as filterable, which answers 400
	// when selected.
	p.fieldsMu.Lock()
	p.fields[objectType] = fieldsCacheEntry{
		fields:   fields,
		raw:      rawCols,
		types:    types,
		complete: len(degraded) == 0,
		expires:  time.Now().Add(schemaTTL),
	}
	p.fieldsMu.Unlock()
	return fields, nil
}

// rawColumns reports the column names that exist upstream for an object type,
// which is what a projection may name. Resolved columns (site) and custom
// fields (cf_*) are ours, not the service's, and asking for them by name would
// be rejected as an unknown column.
func (p *Provider) rawColumns(ctx context.Context, objectType string) (map[string]bool, error) {
	p.fieldsMu.Lock()
	e, ok := p.fields[objectType]
	p.fieldsMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.raw, nil
	}
	if _, err := p.Fields(ctx, objectType); err != nil {
		return nil, err
	}
	p.fieldsMu.Lock()
	defer p.fieldsMu.Unlock()
	return p.fields[objectType].raw, nil
}

// columnTypes reports the inferred type per upstream column. An entry missing
// or empty means the type could not be determined from the sample.
func (p *Provider) columnTypes(ctx context.Context, objectType string) map[string]provider.FieldType {
	p.fieldsMu.Lock()
	e, ok := p.fields[objectType]
	p.fieldsMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.types
	}
	if _, err := p.Fields(ctx, objectType); err != nil {
		return nil
	}
	p.fieldsMu.Lock()
	defer p.fieldsMu.Unlock()
	return p.fields[objectType].types
}

func fieldType(v interface{}) provider.FieldType {
	switch v.(type) {
	case bool:
		return provider.FieldTypeBoolean
	case float64, int, json.Number:
		return provider.FieldTypeNumber
	default:
		return provider.FieldTypeString
	}
}

// FilterFields advertises every column with the operator set the backend
// honours.
func (p *Provider) FilterFields(ctx context.Context, objectType string) ([]provider.FilterField, error) {
	fields, err := p.Fields(ctx, objectType)
	if err != nil {
		return nil, err
	}
	raw, err := p.rawColumns(ctx, objectType)
	if err != nil {
		return nil, err
	}
	return filterFieldsFor(fields, raw, p.columnTypes(ctx, objectType)), nil
}

// FieldValues returns distinct values for a field, for editor autocomplete.
//
// There is no distinct-values endpoint, so this reads a bounded page and
// de-duplicates in process. That is honest about what it is: a sample of the
// values present, not the full domain of the column. The alternative — paging
// millions of rows to be exhaustive — would make the editor unusable for the
// instances this backend exists to serve.
func (p *Provider) FieldValues(ctx context.Context, objectType, field, q string, limit int) ([]string, error) {
	if err := p.validateObjectType(ctx, objectType); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	raw, err := p.rawColumns(ctx, objectType)
	if err != nil {
		return nil, err
	}
	// Autocomplete on a column we synthesize has nothing upstream to read.
	if !raw[field] {
		return nil, nil
	}

	// A substring search is only pushed down when the column holds text.
	// ILIKE against a non-text column is the hazard this provider already
	// guards in FilterFields: it answers either 500 or, worse, HTTP 200 with
	// the whole unfiltered population. Autocomplete would then quietly offer
	// values that do not match what the user typed, while looking healthy.
	//
	// For every other column the page is fetched unfiltered and matched here.
	// That is a sample rather than the column's full domain, which is already
	// true of this endpoint, and an honest subset beats a confident wrong list.
	pushDown := q != "" && p.columnTypes(ctx, objectType)[field] == provider.FieldTypeString
	var params url.Values
	if pushDown {
		var err error
		params, err = buildFilterValues([]provider.Filter{{Field: field, Operator: opIContns, Value: q}})
		if err != nil {
			return nil, err
		}
	}
	params = withFields(params, []string{field})

	raws, _, err := p.client.list(ctx, objectType, params, limit)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(q)
	seen := map[string]bool{}
	var out []string
	for _, r := range raws {
		var obj map[string]interface{}
		if json.Unmarshal(r, &obj) != nil {
			continue
		}
		s := valueString(obj[field])
		if s == "" || seen[s] {
			continue
		}
		// When the search could not be pushed down, apply it here so the caller
		// still gets matches rather than an arbitrary page of every value.
		if !pushDown && needle != "" && !strings.Contains(strings.ToLower(s), needle) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func valueString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// Changes is not supported.
//
// The decisive reason is that the service does not replicate
// extras.objectchange: there is no change log to read, and nothing else can
// stand in for one.
//
// Reconstructing an approximation from row timestamps does not work either, and
// not for the reason it first appeared. created and last_updated are not
// universally absent — tenancy/tenants carries both on every row — but they are
// null on every row of the large tables measured (devices, sites, ip-addresses,
// circuits). So the columns work while the data is missing from exactly the
// tables an annotation would be about, which is worse than a clean absence: a
// time-windowed query against them succeeds and returns nothing, which reads as
// "nothing changed". That gap is a replication issue on the service side and
// may close; the missing change log is the structural reason and will not.
func (p *Provider) Changes(_ context.Context, _ provider.ChangeSpec) ([]provider.Change, error) {
	return nil, &UnsupportedError{
		Feature: "the change log",
		Detail:  "This datasource is configured to read from replica-cache, which does not replicate NetBox's change log, so annotations cannot be built from it. Point the annotation at a datasource in NetBox mode.",
	}
}

// ResolveIPs is not supported.
//
// The service holds ipam.ipaddress rows, but each one names its parent through
// assigned_object_type_id — a content-type id — and exposes no table to
// translate that id into a model. The id is also assigned per NetBox instance,
// so it cannot be hardcoded. Returning an unattached IP would be worse than
// refusing: the enrichment's whole purpose is naming the device behind the
// address.
func (p *Provider) ResolveIPs(_ context.Context, _ []string, _ []string, _ int) (*provider.Result, error) {
	return nil, &UnsupportedError{
		Feature: "IP enrichment",
		Detail:  "This datasource is configured to read from replica-cache, which cannot say what an IP address is assigned to because it does not expose NetBox's content-type table. Use a datasource in NetBox mode for IP enrichment.",
	}
}

// Topology is not supported.
//
// It needs the cable path walk — cables, terminations and the content types
// that say what each termination points at — and rests on the same missing
// content-type table as ResolveIPs.
func (p *Provider) Topology(_ context.Context, _ provider.TopologySpec) (*provider.Graph, error) {
	return nil, &UnsupportedError{
		Feature: "topology",
		Detail:  "This datasource is configured to read from replica-cache, which cannot resolve cable terminations to their endpoints. Use a datasource in NetBox mode for the topology view.",
	}
}
