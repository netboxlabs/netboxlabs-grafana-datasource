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

// discoveryWarmBudget bounds the background discovery refresh itself. No caller
// waits this long; it exists so a hung endpoint cannot hold a goroutine and a
// connection forever.
const discoveryWarmBudget = 60 * time.Second

// discoveryWaitBudget is the most a query will wait for discovery it needs but
// does not have. Measured healthy at ~1.6s for the full document, so this
// covers the good case with headroom while capping the bad one far below the
// request timeout it would otherwise inherit.
const discoveryWaitBudget = 4 * time.Second

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
	// warmDone is non-nil while a background discovery refresh is in flight, and
	// is closed when it ends. One refresh serves every concurrent panel, and a
	// caller may wait on it for a bounded time instead of issuing its own.
	warmDone chan struct{}

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
//
// Detail is set because the shared wording for this kind is NetBox's: it tells
// the reader to use a singular app_label.model such as dcim.device, which this
// backend does not accept. Object types here are the plural slash paths the
// service publishes, so following that advice would produce a second failure.
func (e *UnknownObjectTypeError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{
		Kind:       provider.ErrorKindUnknownObjectType,
		ObjectType: e.Type,
		KnownTypes: e.Known,
		Detail: fmt.Sprintf("Replica cache has no object type %q — it isn't one of the %d types this deployment reports. Object types here are plural paths, e.g. dcim/devices or ipam/ip-addresses.",
			e.Type, e.Known),
	}
}

// rejectBranch refuses a branch-scoped request.
//
// replica-cache mirrors the main dataset and has no notion of a NetBox branch.
// A query carrying one would be answered from main and look entirely healthy,
// so a panel or alert rule would report the main branch's data while its editor
// says it is scoped to a branch. That is a wrong answer rather than a missing
// feature, which is why it fails instead of ignoring the field.
//
// It is reachable: a saved or provisioned query keeps its branch when the
// datasource behind it is switched to this mode.
func rejectBranch(ctx context.Context) error {
	if provider.BranchFromContext(ctx) == "" {
		return nil
	}
	return &UnsupportedError{
		Feature: "branches",
		Detail:  "This datasource is configured to read from replica-cache, which mirrors the main dataset only and cannot answer a branch-scoped query. Clear the branch, or use a datasource in NetBox mode.",
	}
}

// HealthCheck verifies connectivity, credentials and the tenant header.
//
// It bypasses the discovery cache deliberately. Answering from a result up to
// ten minutes old would let Save & Test report "Connected" after the token has
// been revoked or the service has gone away, while every query fails — a wrong
// answer from the one button whose whole job is to make a live request. The
// cost is one request per press, which is what the button is for; the refresh
// also leaves the cache warm.
func (p *Provider) HealthCheck(ctx context.Context) (string, error) {
	types, err := p.objectTypes(ctx, forceRefresh)
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
	return p.objectTypes(ctx, useCache)
}

// cachePolicy says whether a discovery result may be served from cache.
type cachePolicy bool

const (
	useCache     cachePolicy = false
	forceRefresh cachePolicy = true
)

func (p *Provider) objectTypes(ctx context.Context, refresh cachePolicy) ([]provider.ObjectType, error) {
	p.mu.Lock()
	if !bool(refresh) && p.entities != nil && time.Now().Before(p.expires) {
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
		label := humanize(model)
		if plugin, ok := strings.CutPrefix(app, "plugins/"); ok {
			// "Bgp: Bgp Sessions", as the NetBox provider labels the same model.
			label = humanize(plugin) + ": " + label
		}
		out = append(out, provider.ObjectType{
			Value: value,
			Label: label,
			App:   app,
			Model: model,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })

	// An empty list is treated as a FAILURE rather than an answer. A 200
	// carrying no usable paths — a transient discovery hiccup, a truncated
	// document — would otherwise be cached for the full TTL, and every object
	// query then fails locally against an entity set that says nothing exists,
	// while the row endpoints are perfectly healthy. That is the opposite of
	// the degradation this path is built for: not knowing must let the row
	// request decide, and caching "nothing" is a confident wrong answer.
	if len(out) == 0 {
		// Classified, not a bare error: the service that failed is replica-cache,
		// and NetBox is optional in this mode. Unclassified it renders through
		// the plugin's fallback as "Cannot reach NetBox", pointing Save & Test at
		// the wrong service — or at one that is not configured at all.
		return nil, &TransportError{
			Op:      "listing object types",
			Err:     errEmptyDiscovery,
			Message: "Replica cache returned an empty API description, so no object types could be listed. The service is reachable but answered with nothing usable; retry, and check the replica-cache URL and NetBox instance ID.",
		}
	}

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
	if len(parts) < 3 || parts[0] != "v1" {
		return "", "", false
	}
	for _, p := range parts {
		// Detail routes carry a path parameter; only collections are listable.
		if strings.ContainsAny(p, "{}") {
			return "", "", false
		}
	}
	switch {
	case len(parts) == 3:
		return parts[1], parts[2], true
	case len(parts) == 4 && parts[1] == "plugins":
		// A plugin's models sit one level deeper. The app carries the plugin
		// name so that the object type reads plugins/bgp/bgp-sessions — the same
		// value the NetBox provider produces for the same model, which is the
		// point: a saved query has to name one thing in both modes.
		return parts[1] + "/" + parts[2], parts[3], true
	}
	return "", "", false
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

// cachedEntitySet returns the discovered entities ONLY if they are already
// cached, never fetching. It exists so the query path can consult discovery
// without waiting on it.
func (p *Provider) cachedEntitySet() (map[string]bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entities == nil || time.Now().After(p.expires) {
		return nil, false
	}
	set := make(map[string]bool, len(p.entities))
	for _, t := range p.entities {
		set[t.Value] = true
	}
	return set, true
}

// warmEntities starts a background discovery refresh if one is not already
// running, and returns a channel closed when it finishes. It never blocks.
//
// Discovery is the slowest and least reliable request in this service —
// measured timing out while row endpoints answered in ~1.3s — so it must never
// sit inline on the query path. But it cannot simply be skipped either: nothing
// else on a rendering dashboard populates the cache (the editor warms it when it
// lists object types; a dashboard that only renders panels does not), so FK
// names would be permanently absent there.
//
// One refresh therefore serves every caller, and callers choose how long they
// are willing to wait for it.
func (p *Provider) warmEntities() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warmDone != nil {
		return p.warmDone
	}
	done := make(chan struct{})
	p.warmDone = done

	go func() {
		// Bounded so a hung discovery endpoint cannot hold a goroutine and a
		// connection indefinitely. Nothing is required to wait for this, so the
		// budget can exceed what any caller will spend on it.
		ctx, cancel := context.WithTimeout(context.Background(), discoveryWarmBudget)
		defer cancel()
		_, _ = p.ObjectTypes(ctx)

		p.mu.Lock()
		p.warmDone = nil
		p.mu.Unlock()
		close(done)
	}()
	return done
}

// entitySetSoon returns the entity list, waiting at most budget for a refresh
// that is already running or that it starts.
//
// The bound is the whole point. Waiting indefinitely puts a timeout-prone
// request in front of work that does not depend on it; not waiting at all
// throws away the healthy case, where discovery answers in under two seconds
// and the caller can simply have the right answer. A short cap keeps the good
// case correct and makes the bad case cost a fixed, small amount instead of a
// full HTTP timeout — per refresh rather than per panel, since the refresh is
// shared.
func (p *Provider) entitySetSoon(ctx context.Context, budget time.Duration) (map[string]bool, bool) {
	if set, ok := p.cachedEntitySet(); ok {
		return set, true
	}
	select {
	case <-p.warmEntities():
	case <-time.After(budget):
	case <-ctx.Done():
		// The caller has gone — a cancelled dashboard, or one that hit its
		// deadline. Holding the backend for the rest of the budget serves
		// nobody: the rows are already fetched and nothing will read them. The
		// refresh itself continues in the background, so the next query still
		// benefits.
	}
	return p.cachedEntitySet()
}

// validateObjectType rejects a type this deployment does not serve, before any
// request is built. Doing it here rather than letting the service answer 404
// is what makes the message actionable: the reader learns it is not one of the
// N types available, not that a URL was not found.
func (p *Provider) validateObjectType(_ context.Context, objectType string) error {
	// Deliberately consults only an ALREADY-CACHED entity list, and never
	// fetches one.
	//
	// The list is served by a single large document that was measured failing
	// (TLS timeouts, truncated bodies) against an instance whose row endpoints
	// were still answering in ~1.3s. Fetching here put that request in front of
	// every query, so a slow discovery endpoint delayed each panel by a full
	// timeout before the row request it does not depend on had even started —
	// and an object query could then pay it a second time in resolveFKs.
	//
	// Tolerating the failure was not enough; the wait was the problem. Skipping
	// validation is safe because the row request is authoritative: an object
	// type this deployment does not serve answers 404, which classifies as
	// not-found and reads correctly. What is lost is only the better message
	// naming how many types DO exist, and only until something warms the cache —
	// which the query editor does when it populates its object-type dropdown.
	set, ok := p.cachedEntitySet()
	if !ok {
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
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
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

	cols, rows, err := flattenRows(raws)
	if err != nil {
		return nil, err
	}
	if addDeepLinks(p.netboxURL, objectType, rows) {
		cols = append(cols, deepLinkColumn)
	}
	added, degraded := p.resolveFKs(ctx, objectType, rows)
	cols = append(cols, added...)

	// Shared with the query path's lighter probe, so the two cannot disagree
	// about a column's type.
	types := typeColumns(cols, rows)

	fields := make([]provider.Field, 0, len(cols))
	for _, c := range cols {
		// A column that was null everywhere in the sample is reported to the
		// editor as text — it has to be shown as something — while its recorded
		// type stays empty, which is what filterFieldsFor and validateFilterTypes
		// read as "not confirmed text" so it loses the text operators rather
		// than being offered one that may match every row.
		t := types[c]
		if t == provider.FieldType("") {
			t = provider.FieldTypeString
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
	e, err := p.columnSample(ctx, objectType)
	if err != nil {
		return nil, err
	}
	return e.raw, nil
}

// columnSample returns the cached column facts, reading a sample if there are
// none.
//
// It deliberately does NOT go through Fields. Fields is the editor-facing
// method and resolves foreign keys, which waits on entity discovery — measured
// at up to the four-second budget against an instance whose row endpoints were
// answering in about 1.3s. On the query path that wait buys nothing: raw
// columns and types both come from the main-table sample, which is one small
// request, and putting discovery in front of it delayed every panel by a wait
// the row request does not depend on. Resolution still happens after the rows
// arrive, where it belongs.
func (p *Provider) columnSample(ctx context.Context, objectType string) (fieldsCacheEntry, error) {
	p.fieldsMu.Lock()
	e, ok := p.fields[objectType]
	p.fieldsMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e, nil
	}

	raws, _, err := p.client.list(ctx, objectType, nil, sampleRows)
	if err != nil {
		return fieldsCacheEntry{}, err
	}
	if len(raws) == 0 {
		// An empty table teaches nothing about columns. Not an error, and not
		// cached either: the next call should look again rather than pin an
		// empty schema for the whole TTL.
		return fieldsCacheEntry{}, nil
	}

	rawCols := map[string]bool{}
	var obj map[string]interface{}
	if err := json.Unmarshal(raws[0], &obj); err != nil {
		return fieldsCacheEntry{}, fmt.Errorf("reading sample row for %s: %w", objectType, err)
	}
	for k := range obj {
		rawCols[k] = true
	}
	cols, rows, err := flattenRows(raws)
	if err != nil {
		return fieldsCacheEntry{}, err
	}

	entry := fieldsCacheEntry{
		raw: rawCols,
		// Incomplete on purpose: this entry has no resolved columns and no
		// field list, so Fields must not serve it back as one.
		types:   typeColumns(cols, rows),
		expires: time.Now().Add(schemaTTL),
	}

	// Never overwrite a live entry: Fields may have stored a richer one while
	// this sample was in flight, and that one knows about resolved columns.
	p.fieldsMu.Lock()
	if cur, ok := p.fields[objectType]; !ok || !time.Now().Before(cur.expires) {
		p.fields[objectType] = entry
	} else {
		entry = cur
	}
	p.fieldsMu.Unlock()
	return entry, nil
}

// typeColumns types each column from the first NON-NULL value seen across the
// sample. A column that is null everywhere stays unknown — recorded as the
// empty type, which filterFieldsFor and validateFilterTypes both read as "not
// confirmed text" rather than as text.
func typeColumns(cols []string, rows []map[string]interface{}) map[string]provider.FieldType {
	types := make(map[string]provider.FieldType, len(cols))
	for _, c := range cols {
		types[c] = provider.FieldType("")
		for _, row := range rows {
			if v, ok := row[c]; ok && v != nil {
				types[c] = fieldType(c, v)
				break
			}
		}
	}
	return types
}

// columnTypes reports the inferred type per upstream column. An entry missing
// or empty means the type could not be determined from the sample.
// The error is RETURNED rather than folded into a nil map. Both mean "no types
// here", but they need different answers: an empty table genuinely has no types
// to offer and the filter check should fail closed on that, while a 401 or a
// 5xx is an outage. Swallowing the second turned it into an
// UnsupportedFilterError and an HTTP 400 telling the reader their column's type
// could not be determined — sending them to edit a filter that was fine, over a
// credential or a service that was not.
func (p *Provider) columnTypes(ctx context.Context, objectType string) (map[string]provider.FieldType, error) {
	e, err := p.columnSample(ctx, objectType)
	if err != nil {
		return nil, err
	}
	return e.types, nil
}

// fieldType classifies a sampled value.
//
// The timestamp case is not cosmetic. An RFC3339 value arrives from JSON as a
// string like any other, and calling it text is what decides that the column is
// offered a "contains" filter — which this backend then applies to a TIMESTAMP
// column, answering either a 500 or, worse, HTTP 200 over the unfiltered
// population. Mirrors netbox.inferType, which draws the same distinction for
// the same reason.
func fieldType(name string, v interface{}) provider.FieldType {
	switch v.(type) {
	case bool:
		return provider.FieldTypeBoolean
	case float64, int, json.Number:
		return provider.FieldTypeNumber
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return provider.FieldTypeString
	}
	// A value carrying a date, a time and a zone is a timestamp whatever the
	// column is called. NetBox stores no free text in that shape, and column
	// names cannot be enumerated: the 4.4 schema has DateTimeFields named
	// completed, started, scheduled, read, last_login, last_sync, last_synced,
	// merged_time, data_synced and date_joined, none of which any name rule
	// would have guessed. The value decides.
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return provider.FieldTypeTime
	}
	// A bare YYYY-MM-DD is genuinely ambiguous — a serial or an asset tag can
	// look like one — so here the column name still has to agree.
	if isTimeColumn(name) {
		if _, err := time.Parse(time.DateOnly, s); err == nil {
			return provider.FieldTypeTime
		}
	}
	return provider.FieldTypeString
}

// isTimeColumn reports whether a column's name agrees that a bare YYYY-MM-DD
// value is a date. Only the ambiguous date-without-a-zone case consults it;
// a full RFC3339 value is a timestamp on its own evidence.
//
// The affixes catch the DateField columns a fixed list cannot enumerate —
// termination_date, install_date, date_added — without claiming every string
// that happens to parse as a date.
func isTimeColumn(name string) bool {
	switch name {
	case "created", "last_updated", "last_used", "time", "expires":
		return true
	}
	return strings.HasSuffix(name, "_date") ||
		strings.HasSuffix(name, "_at") ||
		strings.HasPrefix(name, "date_")
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
	types, err := p.columnTypes(ctx, objectType)
	if err != nil {
		return nil, err
	}
	return filterFieldsFor(fields, raw, types), nil
}

// FieldValues returns distinct values for a field, for editor autocomplete.
//
// There is no distinct-values endpoint, so this reads a bounded page and
// de-duplicates in process. That is honest about what it is: a sample of the
// values present, not the full domain of the column. The alternative — paging
// millions of rows to be exhaustive — would make the editor unusable for the
// instances this backend exists to serve.
func (p *Provider) FieldValues(ctx context.Context, objectType, field, q string, limit int) ([]string, error) {
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
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
	// A sampling failure here is not fatal: not knowing the type means not
	// pushing down, and the unfiltered fetch below still answers. The request
	// that follows is authoritative — if the service is genuinely down, it fails
	// there, with its own classified error, rather than here.
	types, _ := p.columnTypes(ctx, objectType)
	pushDown := q != "" && types[field] == provider.FieldTypeString
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
