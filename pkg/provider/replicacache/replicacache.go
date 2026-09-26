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
// The trade is fidelity. It serves raw table rows, so a relationship is a bare
// integer id unless the service joins the name in (expand=, from the
// references its catalogue declares), and a handful of NetBox concepts have no
// representation at all — the change log, contacts, and the content-type table
// that would say what an IP is attached to. What the catalogue states is used;
// what it cannot state is refused explicitly rather than returned as an empty
// result that looks like an answer.
package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Provider is the replica-cache implementation of provider.Provider.
type Provider struct {
	client *Client
	// Custom-field names per object type, from one bounded row read each,
	// kept for catalogTTL. See customFieldNames.
	cfMu      sync.Mutex
	cfNames   map[string]cfEntry
	cfFlights map[string]*flight[cfEntry]

	// The catalogue (GET /v1/_meta/schema), cached for catalogTTL. See
	// catalog.go.
	catMu      sync.Mutex
	cat        *catalog
	catExpires time.Time
	catFlight  *flight[*catalog]
}

// customFieldDataColumn is the JSON blob a NetBox model's custom fields live
// in — the one column the catalogue cannot describe past its type.
const customFieldDataColumn = "custom_field_data"

// customFieldSampleRows bounds the read that discovers custom-field NAMES. A
// defined custom field is present in the blob even when unset (measured: every
// dcim/devices row carries the same keys, some null), so one row would name
// them all; twenty is a small page that costs the same request and covers a
// row whose blob is empty or unreadable.
const customFieldSampleRows = 20

// New builds a replica-cache provider. netboxID is the tenant identifier sent
// as NBC-Netbox-ID; the service rejects requests without it. The NetBox the
// replica mirrors — the base of every "View in NetBox" link — is not
// configured here: the catalogue reports it (see linkBase).
func New(base, token, netboxID string, httpClient *http.Client) *Provider {
	return &Provider{
		client:    NewClient(base, token, netboxID, httpClient),
		cfNames:   map[string]cfEntry{},
		cfFlights: map[string]*flight[cfEntry]{},
	}
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
// It is the same base the links were built from (linkBase): the NetBox the
// catalogue says the replica mirrors. Read from the cached catalogue only —
// this must stay cheap — and the cache root when none is known: nothing is
// linkable then, so there is nothing to rewrite, and the value is only ever
// used as a prefix to match.
func (p *Provider) BaseURL() string {
	p.catMu.Lock()
	c := p.cat
	p.catMu.Unlock()
	if base := p.linkBase(c); base != "" {
		return base
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

// notReplicatedDetail is the client's 404 classification for an entity the
// replica is configured for but has received nothing for. The row route is
// the one authority on it: the catalogue is cached for ten minutes, and an
// entity fed a minute ago must not be refused for the rest of them.
func notReplicatedDetail(objectType string) string {
	return fmt.Sprintf("%s is configured on this replica but has received no data for it (not replicated, or empty in NetBox — the cache cannot tell).", objectType)
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

// cfEntry is the custom-field discovery for one object type: the cf_* names
// one bounded read found and a type per name from its values, kept for the
// catalogue's TTL. Names are the one thing the catalogue cannot give —
// custom_field_data is an opaque JSON column — so this is discovery of NAMES,
// not inference of column types: the values go through the same FlattenField
// every row goes through, and a bool is a bool.
type cfEntry struct {
	names   []string // cf_* column names, sorted
	types   map[string]provider.FieldType
	expires time.Time
}

// Fields lists what a query against the object type can return, in this order:
// the stored columns as the catalogue orders them, typed by the catalogue; the
// columns each AVAILABLE reference adds under expand= (site, site_slug); the
// custom fields; the link. An expansion whose target has no data is not
// offered — a column that can never fill is not a column.
func (p *Provider) Fields(ctx context.Context, objectType string) ([]provider.Field, error) {
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
	e, c, err := p.entityFor(ctx, objectType)
	if err != nil {
		return nil, err
	}
	// An unfed entity carries no columns in the catalogue (the service lists
	// them once it has ingested), and its row route answers 404: nothing to
	// offer and nothing to read.
	if !e.Ingested {
		return nil, nil
	}
	var out []provider.Field
	for _, col := range e.Columns {
		if col.Name == customFieldDataColumn {
			continue
		}
		out = append(out, provider.Field{Name: col.Name, Type: fieldTypeOf(col.Type)})
	}
	for _, name := range e.expandedColumns() {
		out = append(out, provider.Field{Name: name, Type: provider.FieldTypeString})
	}
	if e.has(customFieldDataColumn) {
		cf, err := p.customFieldNames(ctx, objectType)
		if err != nil {
			return nil, err
		}
		for _, name := range cf.names {
			out = append(out, provider.Field{Name: name, Type: cf.types[name]})
		}
	}
	if p.linkBase(c) != "" {
		out = append(out, provider.Field{Name: deepLinkColumn, Type: provider.FieldTypeString})
	}
	return out, nil
}

// linkBase is the NetBox instance deep links point at: the one the catalogue
// reports, and nothing else. A replica that reports none yields rows without a
// link column rather than links built from a guess.
func (p *Provider) linkBase(c *catalog) string {
	if c == nil {
		return ""
	}
	return c.NetBoxURL
}

// customFieldNames reads one bounded page projected to the blob and flattens
// it through the shared contract, so the names — and the _count columns a
// list field produces — are exactly what a query returns.
func (p *Provider) customFieldNames(ctx context.Context, objectType string) (cfEntry, error) {
	p.cfMu.Lock()
	cur, ok := p.cfNames[objectType]
	if ok && time.Now().Before(cur.expires) {
		p.cfMu.Unlock()
		return cur, nil
	}
	// One read serves every concurrent caller (see flight).
	if fl := p.cfFlights[objectType]; fl != nil {
		p.cfMu.Unlock()
		return fl.wait(ctx)
	}
	fl := &flight[cfEntry]{done: make(chan struct{})}
	p.cfFlights[objectType] = fl
	p.cfMu.Unlock()
	fl.run(ctx, func(ctx context.Context) (cfEntry, error) { return p.readCustomFieldNames(ctx, objectType) },
		func(cfEntry, error) {
			p.cfMu.Lock()
			defer p.cfMu.Unlock()
			if p.cfFlights[objectType] == fl {
				delete(p.cfFlights, objectType)
			}
		})
	return fl.wait(ctx)
}

func (p *Provider) readCustomFieldNames(ctx context.Context, objectType string) (cfEntry, error) {
	raws, _, _, err := p.client.list(ctx, objectType, withFields(nil, []string{customFieldDataColumn}), customFieldSampleRows)
	if err != nil {
		return cfEntry{}, err
	}
	entry := cfEntry{types: map[string]provider.FieldType{}, expires: time.Now().Add(catalogTTL)}
	decided := map[string]bool{}
	for _, raw := range raws {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return cfEntry{}, &TransportError{Op: "reading custom-field names for " + objectType, Err: err, Message: rowShapeGuidance}
		}
		if obj == nil {
			return cfEntry{}, &TransportError{Op: "reading custom-field names for " + objectType, Err: errMalformedRow, Message: rowShapeGuidance}
		}
		flat := map[string]interface{}{}
		if err := expandCustomFields(obj[customFieldDataColumn], flat); err != nil {
			return cfEntry{}, err
		}
		for name, v := range flat {
			if _, seen := entry.types[name]; !seen {
				entry.names = append(entry.names, name)
				entry.types[name] = provider.FieldTypeString
			}
			// The first non-null value decides; a name null everywhere is text.
			if v != nil && !decided[name] {
				entry.types[name], decided[name] = valueType(v), true
			}
		}
	}
	sort.Strings(entry.names)
	// An empty table is not cached: the next call should look again rather
	// than pin "no custom fields" for the whole TTL.
	if len(raws) > 0 {
		p.cfMu.Lock()
		p.cfNames[objectType] = entry
		p.cfMu.Unlock()
	}
	return entry, nil
}

// valueType types a custom-field value by its JSON shape. Custom fields are
// the one place a type still comes from a value rather than the catalogue,
// and only the shape is read: a string that looks like a date is a string.
func valueType(v interface{}) provider.FieldType {
	switch v.(type) {
	case bool:
		return provider.FieldTypeBoolean
	case float64, int, json.Number:
		return provider.FieldTypeNumber
	}
	return provider.FieldTypeString
}

// FilterFields advertises every stored column and every available expanded
// name with the operators the catalogue says the backend takes on it (see
// seamOperators). An expanded name filters on the TARGET column, so its
// operators are the target's.
func (p *Provider) FilterFields(ctx context.Context, objectType string) ([]provider.FilterField, error) {
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
	e, c, err := p.entityFor(ctx, objectType)
	if err != nil {
		return nil, err
	}
	var out []provider.FilterField
	for _, col := range e.Columns {
		if col.Name == customFieldDataColumn {
			continue
		}
		out = append(out, provider.FilterField{Name: col.Name, Operators: seamOperators(col)})
	}
	for _, col := range e.Columns {
		if col.Ref == nil || !col.Ref.Available {
			continue
		}
		out = append(out, provider.FilterField{Name: col.Ref.ExpandKey, Operators: seamOperators(targetColumn(c, col.Ref, col.Ref.Columns[0]))})
		for _, tc := range col.Ref.Columns[1:] {
			out = append(out, provider.FilterField{Name: col.Ref.ExpandKey + "_" + tc, Operators: seamOperators(targetColumn(c, col.Ref, tc))})
		}
	}
	return out, nil
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
	e, c, err := p.entityFor(ctx, objectType)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	pk := e.pk()
	// A stored column is read as itself. A related name FilterFields offers
	// (site, site_slug) is read under expand=, projected to the primary key
	// alone: the service returns the expanded columns beside it, and the
	// search below is the TARGET column's, since that is what ilike runs on.
	// Anything else — a name this replica cannot produce — has nothing to read.
	col, stored := e.column(field)
	var params url.Values
	if !stored {
		via, target, ok := e.expandedColumn(field)
		if !ok || !via.Ref.Available {
			return nil, nil
		}
		params = url.Values{"expand": {via.Ref.ExpandKey}}
		col = targetColumn(c, via.Ref, target)
	}

	// A substring search is only pushed down when the catalogue says the
	// column takes ilike — text. ILIKE against anything else answers either 500
	// or, worse, HTTP 200 with the whole unfiltered population, and autocomplete
	// would then quietly offer values that do not match what the user typed.
	// For every other column the page is fetched unfiltered and matched here:
	// a sample rather than the column's full domain, which is already true of
	// this endpoint, and an honest subset beats a confident wrong list.
	//
	// The backend's ilike is a contains on the literal value, which is exactly
	// what a search box means, so the text goes as written — except that
	// buildFilterValues splits on commas as a multi-value dashboard filter,
	// which is not what someone typing "a,b" means. That text is matched
	// locally, the same answer as for a column that cannot be searched upstream.
	pushDown := q != "" && slices.Contains(col.Operators, "ilike") && literalPushable(q)
	if pushDown {
		fv, err := buildFilterValues([]provider.Filter{{Field: field, Operator: opIContns, Value: q}}, nil)
		if err != nil {
			return nil, err
		}
		if params == nil {
			params = url.Values{}
		}
		for k, vs := range fv {
			params[k] = vs
		}
	}
	if stored {
		params = withFields(params, []string{field})
	} else {
		params = withFields(params, []string{pk})
	}

	raws, _, _, err := p.client.list(ctx, objectType, params, limit)
	if err != nil {
		return nil, err
	}
	// An IP address is offered as the rows show it, with a single-host mask,
	// so that the value picked matches that record and no other.
	isAddress := addressField(e, c)
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
		id, hasID := toInt(obj[pk])
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
			// the same measured reason: the service returns the primary key on
			// every projection, even one that did not ask for it —
			// `fields=serial` comes back as {id, serial}, and the projection
			// above is exactly that shape. A row without one identifies no
			// object, so offering its value would put something in the picker
			// that nothing in the deployment corresponds to.
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
		if isAddress(field) {
			s = withSingleHostMask(s)
		}
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
// written. Only a comma cannot: buildFilterValues reads it as a value
// separator, which is not what it means in something a person is typing. The
// backend takes % and _ literally, so they can.
func literalPushable(q string) bool {
	return !strings.Contains(q, ",")
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
