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

// schemaTTL bounds how long a sampled column list is reused.
const schemaTTL = 10 * time.Minute

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

	fieldsMu sync.Mutex
	fields   map[string]fieldsCacheEntry

	// The catalogue (GET /v1/_meta/schema), cached for catalogTTL. See
	// catalog.go.
	catMu      sync.Mutex
	cat        *catalog
	catExpires time.Time
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
	// Normalized exactly as netbox.NewClient normalizes it, and for the same
	// reason: the setting explicitly tolerates a trailing "/api", so a
	// datasource configured in NetBox mode and then switched here carries that
	// suffix. Stored raw it produced "View in NetBox" links to
	// https://host/api/dcim/devices/<id>/ — the REST response for the object
	// rather than its page.
	return func(p *Provider) {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		p.netboxURL = strings.TrimSuffix(base, "/api")
	}
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

// HealthCheck reads the catalogue afresh — never from the cache, so Save & Test
// cannot report "Connected" from a stale answer after a token revocation — and
// says how much of the deployment has data.
func (p *Provider) HealthCheck(ctx context.Context) (string, error) {
	c, err := p.catalogue(ctx, true)
	if err != nil {
		return "", err
	}
	fed := 0
	for _, e := range c.Entities {
		if e.Ingested {
			fed++
		}
	}
	return fmt.Sprintf("Connected to replica-cache (%d object types, %d with data)", len(c.Entities), fed), nil
}

// ObjectTypes lists every entity the catalogue configures, fed or not. The
// value is the object type ("dcim/devices", "plugins/bgp/bgp-sessions") —
// deliberately what the NetBox provider produces, so one saved query names
// one thing in both modes. An unfed entity stays listed: the error a query
// against it gets explains the situation better than a picker that silently
// lacks it.
func (p *Provider) ObjectTypes(ctx context.Context) ([]provider.ObjectType, error) {
	c, err := p.catalogue(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ObjectType, 0, len(c.Entities))
	for key := range c.Entities {
		app, model := splitEntity(key)
		label := humanize(model)
		if plugin, ok := strings.CutPrefix(app, "plugins/"); ok {
			label = humanize(plugin) + ": " + label
		}
		out = append(out, provider.ObjectType{Value: key, Label: label, App: app, Model: model})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out, nil
}

// entityFor is the lookup every query-path method starts with: the entity's
// catalogue entry, the catalogue it came from, or the reason there is none.
func (p *Provider) entityFor(ctx context.Context, objectType string) (entity, *catalog, error) {
	c, err := p.catalogue(ctx, false)
	if err != nil {
		return entity{}, nil, err
	}
	e, ok := c.Entities[objectType]
	if !ok {
		return entity{}, nil, &UnknownObjectTypeError{Type: objectType, Known: len(c.Entities)}
	}
	return e, c, nil
}

func (p *Provider) validateObjectType(ctx context.Context, objectType string) error {
	_, _, err := p.entityFor(ctx, objectType)
	return err
}

// entitySetIfWarm and entitySetSoon are the entity-set views fk.go still reads
// while the client-side resolution exists. Both answer from the catalogue; the
// background warming they used to drive is gone, since the catalogue is read
// on the query path itself.
func (p *Provider) entitySetIfWarm() (map[string]bool, bool) {
	p.catMu.Lock()
	c := p.cat
	p.catMu.Unlock()
	if c == nil {
		return nil, false
	}
	return entitySet(c), true
}

func (p *Provider) entitySetSoon(ctx context.Context, _ time.Duration) (map[string]bool, bool) {
	c, err := p.catalogue(ctx, false)
	if err != nil {
		return nil, false
	}
	return entitySet(c), true
}

func entitySet(c *catalog) map[string]bool {
	set := make(map[string]bool, len(c.Entities))
	for key := range c.Entities {
		set[key] = true
	}
	return set
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
	// "" and "main" both mean the unbranched dataset — the same sentinel the
	// NetBox client honours, and the value the branch variable emits for its
	// always-present first option. This mode mirrors exactly that dataset, so
	// rejecting the word for it turned every panel driven by the documented
	// variable into an error while it was pointed at the data we serve.
	branch := strings.TrimSpace(provider.BranchFromContext(ctx))
	if branch == "" || strings.EqualFold(branch, "main") {
		return nil
	}
	return &UnsupportedError{
		Feature: "branches",
		Detail:  "This datasource is configured to read from replica-cache, which mirrors the main dataset only and cannot answer a branch-scoped query. Clear the branch, or use a datasource in NetBox mode.",
	}
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
		return nil, &TransportError{Op: "reading a sample row for " + objectType, Err: err, Message: rowShapeGuidance}
	}
	for k := range obj {
		rawCols[k] = true
	}

	cols, rows, err := flattenRows(raws, nil)
	if err != nil {
		return nil, err
	}
	if addDeepLinks(p.netboxURL, objectType, rows) {
		cols = append(cols, deepLinkColumn)
	}
	// nil: the editor's field list has to offer every relationship, not just
	// the ones some earlier query happened to select.
	added, degraded := p.resolveFKs(ctx, objectType, rows, nil)
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
		// Classified like every other row-shape failure. Unclassified this
		// rendered as "Couldn't reach NetBox" for a malformed response from the
		// cache — a connection this mode may not even have configured.
		return fieldsCacheEntry{}, &TransportError{Op: "reading a sample row for " + objectType, Err: err, Message: rowShapeGuidance}
	}
	for k := range obj {
		rawCols[k] = true
	}
	cols, rows, err := flattenRows(raws, nil)
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
		first := true
		for _, row := range rows {
			v, ok := row[c]
			if !ok || v == nil {
				continue
			}
			if first {
				types[c] = fieldType(c, v)
				first = false
				continue
			}
			// A timestamp claim has to hold across the WHOLE sample, not rest on
			// the first row that happened to be non-null. One RFC3339-looking
			// value in a text column — a description, or anything on a plugin
			// model — would otherwise type the column as time, and the damage is
			// not cosmetic: filterFieldsFor then offers is-empty, which this
			// backend answers with IS NULL, and NetBox stores a blank text field
			// as "" — so the filter returns the exact opposite population while
			// looking healthy. That is the inversion this provider already
			// refuses elsewhere; here it would arrive through a wrong type.
			//
			// Only the time claim is retested. The others are not dangerous in
			// this way, and re-deriving them per row would make a column of
			// mixed shapes flip type on the sample's order.
			if types[c] == provider.FieldTypeTime && fieldType(c, v) != provider.FieldTypeTime {
				types[c] = provider.FieldTypeString
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
	// Pushed down only when the text can be sent as a literal. buildFilterValues
	// refuses a value carrying % or _ — the service has no escape syntax, so
	// they would widen the match — and it splits on commas as a multi-value
	// dashboard filter. Neither is right for a search box: typing CORE_SW is
	// one literal name, and propagating the refusal replaced the suggestions
	// with an error. The local matching below is the existing answer for a
	// column that cannot be searched upstream, so it answers this too.
	pushDown := q != "" && types[field] == provider.FieldTypeString && literalPushable(q)
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
	seenIDs := map[int]bool{}
	var out []string
	for _, r := range raws {
		var obj map[string]interface{}
		if err := json.Unmarshal(r, &obj); err != nil {
			// Skipping made an unreadable response look like a short list of
			// values, which the editor then presents as authoritative — the
			// same silence the query path refuses, in the one place a user is
			// choosing what to filter on.
			return nil, &TransportError{Op: "reading a value row for " + objectType, Err: err, Message: rowShapeGuidance}
		}
		if obj == nil {
			// A JSON null decodes without error and leaves the map nil.
			return nil, &TransportError{Op: "reading a value row for " + objectType, Err: errMalformedRow, Message: rowShapeGuidance}
		}
		id, hasID := toInt(obj["id"])
		if hasID {
			// Same rule as every other consumer of these rows. Two rows sharing
			// an id carry two values for one object, and only one can be its
			// own — offering both puts a value in the picker that filtering by
			// it would then match nothing. Deduplicating by the displayed value
			// below does not catch this: the values differ, which is the
			// problem.
			if seenIDs[id] {
				return nil, &TransportError{Op: "reading a value row for " + objectType, Err: errDuplicateRow, Message: rowShapeGuidance}
			}
			seenIDs[id] = true
		}
		if !hasID {
			// The same invariant the query path enforces, and it holds here for
			// the same measured reason: the service returns id on every
			// projection, even one that did not ask for it — `fields=serial`
			// comes back as {id, serial}, and the projection above is exactly
			// that shape. A row without one identifies no object, so offering
			// its value would put something in the picker that nothing in the
			// deployment corresponds to.
			return nil, &TransportError{Op: "reading a value row for " + objectType, Err: errRowWithoutID, Message: rowShapeGuidance}
		}
		v, present := obj[field]
		if !present {
			// The projection asked for this column, so its absence is the
			// service failing to answer rather than the object having no value
			// — and treating the two alike turned an incomplete response into a
			// short list the editor presents as authoritative. Measured before
			// requiring it: 92 projected rows across 30 entities on a live
			// instance, none missing the column they were projected onto.
			return nil, &TransportError{Op: "reading a value row for " + objectType, Err: errRowWithoutField, Message: rowShapeGuidance}
		}
		s := valueString(v)
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

// literalPushable reports whether a search box's text can go upstream as
// written. A % or _ would be a wildcard there and a comma a value separator,
// and none of the three means that in something a person is typing.
func literalPushable(q string) bool {
	return !strings.ContainsAny(q, "%_,")
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

// ResolveScope is the scope source of the same query, and is unsupported for
// the same reason: every address in a scope still has to be resolved to what
// it is assigned to, which goes through the content-type table.
func (p *Provider) ResolveScope(_ context.Context, _ []provider.Filter, _ []string, _ int) (*provider.Result, error) {
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
