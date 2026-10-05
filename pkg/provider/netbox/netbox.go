// Package netbox implements provider.Provider against the NetBox REST API.
package netbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// cacheTTL bounds how long discovery and field metadata are cached.
const cacheTTL = 5 * time.Minute

// schemaTTL bounds how long the parsed OpenAPI filter schema is cached. The
// schema changes only on a NetBox upgrade, so this is much longer than cacheTTL.
const schemaTTL = 30 * time.Minute

// objectTypeFailureTTL is how long a FAILED read of the object-type map is
// remembered — 30x shorter than the cacheTTL a successful read earns, and the
// asymmetry is the whole design.
//
// Something has to be remembered, or the failure is paid for once per caller
// that arrives after it: a dashboard opens every annotation query at the same
// instant, they all miss together, and a failure nobody records is a failure
// every one of them re-attempts in turn. This bounds that to one doomed read per
// ten seconds, however many queries ask and however fast they refresh.
//
// It stays SHORT because what is remembered is an outage, not a fact. A 503 from
// a restarting instance, or a proxy blip, must not disable the id pushdown until
// the plugin restarts — that would trade a retry storm for a day of quietly
// degraded annotations, which is the worse bug and the harder one to notice. Ten
// seconds is longer than a dashboard's opening burst and short enough that a
// recovered instance is picked up within a refresh or two, and the degraded path
// it holds open still returns annotations (see pushChangedObjectType).
//
// A var, not a const, so tests can shorten it (as they do retryBackoff).
var objectTypeFailureTTL = 10 * time.Second

// defaultLimit / MaxLimit bound result sizes when the caller does not specify.
// MaxLimit is exported because the plugin layer must be able to tell the user
// when their requested row limit was reduced to it.
const (
	defaultLimit = 1000
	MaxLimit     = 10000
	pageSize     = 500
)

// nonCollectionEndpoints are NetBox API endpoints that look like collections in
// the root index but are not list endpoints, so they are excluded from
// discovery.
var nonCollectionEndpoints = map[string]bool{
	"dcim/connected-device": true,
}

// Provider queries NetBox directly over its REST API.
type Provider struct {
	client *Client

	// cursorPaging is the datasource's opt-in to NetBox cursor pagination for
	// object queries (see startParam). OFF unless the user turned it on: it
	// trades the match count and the model's natural row order for speed, which
	// is a bargain worth nothing below a few million rows and a visible change
	// at every size.
	cursorPaging bool

	// requestTimeout is the datasource's configured Timeout, mirrored here
	// because it is not readable back off the http.Client the provider is handed
	// (Grafana's SDK builds that client, and its Timeout may be its own default
	// rather than the user's setting). It sizes the utilization measurement
	// budget and nothing else — the timeout itself is enforced by the client.
	// Zero means "not told", and utilizationBudget falls back to the same 30s
	// default the settings loader applies.
	requestTimeout time.Duration

	mu          sync.Mutex
	types       []provider.ObjectType
	typesExpiry time.Time
	fields      map[string]fieldsCacheEntry

	// schemaByBranch caches the parsed OpenAPI filter schema keyed by branch
	// ("" = main). The schema is fetched branch-scoped (X-NetBox-Branch), and a
	// branch may define custom-field filters (cf_*) main lacks, so main and each
	// branch must cache separately — mirroring the fields cache.
	schemaByBranch map[string]schemaCacheEntry
	// schemaFlightByBranch is the in-progress schema fetch per branch, so
	// panels opening together on a cold cache wait for one download.
	schemaFlightByBranch map[string]*schemaFlight

	// customFieldsByBranch caches the custom-field type index (see
	// customFieldTypes) keyed the same way, for the same reason: a branch can
	// define custom fields main does not.
	customFieldsByBranch map[string]customFieldTypesEntry
	// customFieldsFlightByBranch is the in-progress definition fetch per branch,
	// the sole writer of customFieldsByBranch (see customFieldsFlight).
	customFieldsFlightByBranch map[string]*customFieldsFlight
	// customFieldsWaitOverride shortens customFieldsWaitBudget; tests only.
	customFieldsWaitOverride time.Duration

	// branchingInstalled caches whether netbox-branching is installed — an
	// instance-wide, branch-invariant property, so a single value + expiry
	// suffices (no per-branch map). Only conclusive probes are cached; the
	// zero-value branchingExpiry forces the first probe.
	branchingInstalled bool
	branchingExpiry    time.Time

	// objectTypeIDs caches app_label.model -> object-type id (see
	// pushChangedObjectType). Object-type ids are instance-wide and identical on
	// every branch — a branch changes DATA, not the model registry — so, like
	// branchingInstalled, one value + expiry suffices rather than a per-branch
	// map.
	objectTypeIDs       map[string]int
	objectTypeIDsExpiry time.Time

	// objectTypeErr is the NEGATIVE half of that cache: the failure the last read
	// ended with, held for the much shorter objectTypeFailureTTL so that a
	// failing endpoint costs one request per window rather than one per caller.
	objectTypeErr       error
	objectTypeErrExpiry time.Time

	// objectTypeFlight is the read currently running, if any — the single-flight
	// handle every simultaneous miss waits on, so a dashboard opening all its
	// annotation queries at once makes one fetch instead of one per query.
	//
	// A CHANNEL and not a mutex: a waiter has its own deadline (Grafana's query
	// timeout) and must be able to stop waiting when that expires, which means a
	// select on ctx.Done(), which a mutex cannot offer. Queueing on a lock made a
	// caller sit out the leader's whole fetch whatever its own budget was —
	// TestChanges_WaiterDoesNotOutliveItsOwnDeadline pins that, and measured 2.0s
	// against a 20ms deadline before this changed.
	objectTypeFlight *objectTypeFlight
}

// objectTypeFlight is one in-flight read of the object-type map, shared by every
// caller that misses the cache while it runs — including its failure, which is
// the point: the alternative is each waiter re-attempting a read that has just
// been shown not to work, one after another.
//
// ids and err are written once BEFORE done is closed and read only AFTER, so the
// close is the happens-before edge that publishes them and no lock is needed on
// either field.
type objectTypeFlight struct {
	done chan struct{}
	ids  map[string]int
	err  error
}

// schemaCacheEntry holds everything derived from one fetch of /api/schema/.
// Both derivations (filter params for the editor, dimension relationships for
// FieldValues) come from the same document, so they share a fetch and a TTL.
type schemaCacheEntry struct {
	filters map[string][]provider.FilterField
	dims    dimIndex
	expiry  time.Time
}

type fieldsCacheEntry struct {
	fields []provider.Field
	expiry time.Time
}

// schemaFlight is one OpenAPI schema fetch that every caller arriving while it
// runs waits for; entry and err are set before done closes.
type schemaFlight struct {
	done  chan struct{}
	entry schemaCacheEntry
	err   error
}

// dropExpired deletes the entries of m that expired before now. The caches it
// runs on are only ever overwritten by their own key, so without it a key that
// is never asked again — a branch since merged and deleted — stayed for the
// life of the process. Callers hold p.mu, and call it when they write.
func dropExpired[V any](m map[string]V, now time.Time, expiry func(V) time.Time) {
	for k, v := range m {
		if !now.Before(expiry(v)) {
			delete(m, k)
		}
	}
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithCursorPaging enables NetBox cursor pagination for object queries that can
// be answered without a match count (see startParam and QuerySpec.AllowUncounted).
// It is the datasource's "very large instance" setting and defaults to OFF —
// passing false is exactly the historical behaviour.
func WithCursorPaging(enabled bool) Option {
	return func(p *Provider) { p.cursorPaging = enabled }
}

// WithRequestTimeout tells the provider the datasource's configured Timeout, so
// that work which fans out per row can be bounded by the user's own statement of
// how long this datasource may take rather than by a constant measured on
// somebody else's instance. See utilizationBudget.
func WithRequestTimeout(d time.Duration) Option {
	return func(p *Provider) { p.requestTimeout = d }
}

// New constructs a NetBox provider over the given HTTP client.
func New(base, token string, httpClient *http.Client, opts ...Option) *Provider {
	p := &Provider{
		client: NewClient(base, token, httpClient),
		fields: map[string]fieldsCacheEntry{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Provider) Name() string    { return "netbox" }
func (p *Provider) BaseURL() string { return p.client.BaseURL() }

// HealthCheck verifies connectivity and authentication.
func (p *Provider) HealthCheck(ctx context.Context) (string, error) {
	var status map[string]interface{}
	if err := p.client.getJSON(ctx, p.client.apiURL("status", nil), &status); err != nil {
		return "", err
	}
	ver, _ := status["netbox-version"].(string)
	if ver == "" {
		return "Connected to NetBox", nil
	}
	return fmt.Sprintf("Connected to NetBox %s", ver), nil
}

// BranchingInstalled reports whether netbox-branching is installed, cached for
// cacheTTL. It satisfies the optional provider.BranchingCapable capability and
// is deliberately NOT part of the backend-agnostic provider.Provider interface.
// Inconclusive probes (transient upstream failures) are not cached and return
// conclusive=false so callers fail open.
func (p *Provider) BranchingInstalled(ctx context.Context) (installed bool, conclusive bool) {
	p.mu.Lock()
	if time.Now().Before(p.branchingExpiry) {
		v := p.branchingInstalled
		p.mu.Unlock()
		return v, true
	}
	p.mu.Unlock()

	v, ok := p.client.BranchingInstalled(ctx)
	if !ok {
		return false, false // inconclusive: don't cache, let the caller fail open
	}

	p.mu.Lock()
	p.branchingInstalled = v
	p.branchingExpiry = time.Now().Add(cacheTTL)
	p.mu.Unlock()
	return v, true
}

// ObjectTypes dynamically discovers the queryable object types, including
// plugin-provided models.
func (p *Provider) ObjectTypes(ctx context.Context) ([]provider.ObjectType, error) {
	p.mu.Lock()
	if p.types != nil && time.Now().Before(p.typesExpiry) {
		cached := p.types
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	types, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Value < types[j].Value })

	p.mu.Lock()
	p.types = types
	p.typesExpiry = time.Now().Add(cacheTTL)
	p.mu.Unlock()
	return types, nil
}

// discover walks NetBox's tree of URL indexes to enumerate every queryable
// collection, so nothing is hard-coded: GET /api/ yields {app: url} (dcim,
// ipam, …) and each app index yields {model: url}. Plugin endpoints are
// handled by discoverPlugins.
//
// Only the keys are used. Each one is the path segment its URL names, and the
// next index is built from it on the configured URL: the values are absolute
// URLs NetBox wrote from the request as it arrived, which behind a proxy that
// does not pass X-Forwarded-Proto/-Host name plain http or another host (see
// nextPageURL). Queries already address object types by these keys.
func (p *Provider) discover(ctx context.Context) ([]provider.ObjectType, error) {
	root, err := p.urlMap(ctx, p.client.apiURL("", nil))
	if err != nil {
		return nil, fmt.Errorf("discover api root: %w", err)
	}

	var types []provider.ObjectType
	for app := range root {
		if app == "status" {
			continue
		}
		models, err := p.urlMap(ctx, p.client.apiURL(url.PathEscape(app), nil))
		if err != nil {
			continue // tolerate individual app discovery failures
		}
		if app == "plugins" {
			types = append(types, p.discoverPlugins(ctx, models)...)
			continue
		}
		for model := range models {
			value := app + "/" + model
			if nonCollectionEndpoints[value] {
				continue
			}
			types = append(types, provider.ObjectType{
				Value: value,
				Label: humanize(model),
				App:   app,
				Model: model,
			})
		}
	}
	return types, nil
}

// discoverPlugins enumerates plugin endpoints. Each entry under /api/plugins/ is
// either a plugin sub-app (a URL map of its models) or a direct collection.
func (p *Provider) discoverPlugins(ctx context.Context, plugins map[string]string) []provider.ObjectType {
	var types []provider.ObjectType
	for plugin := range plugins {
		sub, err := p.urlMap(ctx, p.client.apiURL("plugins/"+url.PathEscape(plugin), nil))
		if err != nil {
			// Direct collection (e.g. installed-plugins).
			types = append(types, provider.ObjectType{
				Value: "plugins/" + plugin,
				Label: humanize(plugin),
				App:   "plugins",
				Model: plugin,
			})
			continue
		}
		for model := range sub {
			types = append(types, provider.ObjectType{
				Value: "plugins/" + plugin + "/" + model,
				Label: humanize(plugin) + ": " + humanize(model),
				App:   "plugins/" + plugin,
				Model: model,
			})
		}
	}
	return types
}

// urlMap fetches a JSON object and returns it as a map ONLY if every value is a
// string URL (i.e. an API index). Otherwise it returns an error so callers can
// treat the endpoint as a collection.
func (p *Provider) urlMap(ctx context.Context, rawURL string) (map[string]string, error) {
	body, err := p.client.getBytes(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || !strings.HasPrefix(s, "http") {
			return nil, fmt.Errorf("not a URL index")
		}
		out[k] = s
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty index")
	}
	return out, nil
}

// Query executes an object query and returns flattened, joinable rows.
func (p *Provider) Query(ctx context.Context, spec provider.QuerySpec) (*provider.Result, error) {
	if spec.ObjectType == "" {
		return nil, fmt.Errorf("objectType is required")
	}
	// A caller cannot both read only the count and be able to do without one.
	// Failing here is the point: it is the contradiction that would otherwise
	// surface as an alert rule quietly evaluating "how many devices are offline"
	// as zero.
	if spec.CountOnly && spec.AllowUncounted {
		return nil, fmt.Errorf("invalid query: CountOnly needs a total, AllowUncounted says one is not needed")
	}
	limit := spec.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	q := buildFilterValues(spec.Filters)

	if spec.CountOnly {
		// The caller reads Result.Total and nothing else, so fetch the smallest
		// row NetBox can build. spec is a value copy: narrowing it here keeps
		// every step below (projection, utilization, column projection) consistent
		// with what was actually requested.
		spec.Fields = countOnlyFields
		spec.KeyFields = nil
		// A count reads the envelope and no rows at all, so a sort is pure upstream
		// cost — measured at 27.6s against 0.9s natural for `role` on 6.8M devices —
		// buying an order nothing will ever look at. Dropped here rather than at the
		// call site so that every caller of a count gets it, including one written
		// later, and dropped SILENTLY: there is no table to annotate, which is the
		// same reason scaleNote returns early for a count.
		spec.Ordering = ""
		limit = 1
	}

	// The properties of the selection NetBox can serialize — everything the
	// caller asked for except the columns computed below. Both the projection
	// and its safety net reason about THIS list, never spec.Fields: a computed
	// column is added after the fetch and can never come back in the response.
	fetchable := fetchableFields(spec.Fields)

	// Whether the `exclude` parameter is the CALLER'S rather than ours. Read
	// before the projection sets it, because that is the only moment the two are
	// still distinguishable. setExcludeConfigContext yields the parameter to a
	// caller's own filter of that name, exactly as the projection below yields
	// `fields`, and the fallback may only delete what it added: dropping a user's
	// filter answers a WIDER question than the one they asked.
	callerOwnsExclude := q.Has(excludeParam)

	// Ask NetBox to serialize only the properties this query reads. Skipped when
	// the caller's own filters already occupy the parameter: a NetBox model may
	// legitimately have a filter named "fields", and their filter is the answer
	// they asked for, while the projection is only an optimisation.
	projection := ""
	if q.Has(fieldsParam) {
		log.DefaultLogger.Debug("netbox: a filter occupies the fields parameter; fetching whole objects",
			"objectType", logSafe(spec.ObjectType))
	} else if len(spec.Fields) > 0 {
		// An empty selection is every column, which no projection can express;
		// a selection of only computed columns still projects, down to the
		// properties those columns are computed from.
		if projection = projectionValue(fetchable, fetchOnlyFields(spec)); projection != "" {
			q.Set(fieldsParam, projection)
			setExcludeConfigContext(q, projection)
		}
	}

	// Cursor paging is opt-in, and the caller must have said it can present a
	// result with no match count. AllowUncounted defaults to false so that a
	// path added later inherits the counted behaviour by omission — forgetting
	// this field costs speed, never correctness.
	cursor := p.cursorPaging && spec.AllowUncounted && cursorLegal(q)

	// Sorting is decided AFTER cursor mode, and the order of these two lines is the
	// whole interaction between them. NetBox refuses ?start= together with
	// ?ordering=, and cursorLegal reads the query values — so setting the sort first
	// would silently turn the data source's fast paging off for any panel that sorts.
	// Deciding cursor mode first makes the operator's instance-wide setting win and
	// leaves applyOrdering to say, in a note, that the sort was not applied.
	orderNote := applyOrdering(q, spec, cursor)

	page, err := p.fetchList(ctx, spec.ObjectType, q, limit, cursor)
	// One repeat, at most, and which of the two it is depends on whether there is
	// a projection to drop. projection != "" is a precondition on the first, not a
	// nicety: it protects the query above that YIELDED to a caller's own `fields`
	// filter, where deleting the parameter would silently answer a WIDER question
	// than the user asked.
	switch {
	case err != nil && projection != "" && projectionRejected(err, q):
		// The projection is an optimisation, so it must never be the reason a
		// query that used to work now fails. NetBox does NOT always ignore a name
		// it cannot serialize: `?fields=` feeds prefetch_related(), and a name that
		// looks like a relation but is not one raises AttributeError — HTTP 500.
		// Reproduced on the bundled demo (NetBox 4.4.10): selecting `prefix_count`
		// on dcim/sites sends `prefix` alongside it (appendUpstreamNames strips the
		// derived suffix, and it must — `tags_count` genuinely does come from
		// `tags`), and NetBox answers "Cannot find 'prefix' on Site object". The
		// panel, the join key and the alert rule that read that column all went
		// from rows to an error toast.
		//
		// Whether a name resolves is the remote serializer's decision, so the
		// answer is to take the decision back: drop the projection and ask for
		// whole objects, exactly as this provider did before there was one. One
		// wasted request, on the rare query NetBox rejects, to keep every such
		// query answering.
		//
		// The one wasted request is the whole budget, so a refusal that names a
		// parameter of its own never reaches here (projectionRejected /
		// blamesAnotherParam): the refetch would carry that parameter unchanged
		// and fail identically, and this WARN would name the projection as the
		// cause of a failure it had no part in.
		// The sort is logged alongside the projection because the two INTERACT in
		// exactly the failure this branch recovers from: dcim/sites
		// ?ordering=device_count answers 200 alone and 500 with ?fields=, so a 500
		// naming a keyword it cannot resolve may be about either parameter. The
		// refetch drops only the projection — which is what fixes that case — and
		// keeps the sort, because with a row limit the sort decides which rows come
		// back and dropping it would answer a different question than the one asked.
		log.DefaultLogger.Warn("netbox: field projection rejected; refetching whole objects",
			"objectType", logSafe(spec.ObjectType), "fields", logSafe(projection),
			"ordering", logSafe(q.Get(orderingParam)), "error", logSafe(err.Error()))
		q.Del(fieldsParam)
		// The config-context exclusion rides with the projection and only with it
		// (see the refetch below) — but only when it is OURS. A caller's own
		// `exclude` filter is their answer, and a fallback that exists to preserve
		// answers must not widen one.
		if !callerOwnsExclude {
			q.Del(excludeParam)
		}
		// No projection is in play any more, which also retires the "returned none
		// of the requested columns" check below: there is nothing left to blame.
		projection = ""
		page, err = p.fetchList(ctx, spec.ObjectType, q, limit, cursor)

	case err != nil && projection == "" && upstreamServerError(err):
		// The same single repeat for the query that has NO projection to drop.
		// It re-issues the identical request, which is worth doing for exactly one
		// status: getListPageRetry owns 502/503/504 and the transport failures, and
		// deliberately leaves 500 alone, so this call site is the only place a 500
		// is ever tried twice. An all-columns query used to get this repeat as a
		// side effect — "refetch without the projection" IS the identical request
		// when there is no projection — and dropping it silently would have made
		// two panels that differ only in which columns they select behave
		// differently against the same upstream blip.
		//
		// Deliberately not extended to a 400: that is NetBox refusing the request
		// as sent, and re-sending it unchanged asks the same question again.
		log.DefaultLogger.Warn("netbox: upstream 500; retrying the query once",
			"objectType", logSafe(spec.ObjectType), "error", logSafe(err.Error()))
		page, err = p.fetchList(ctx, spec.ObjectType, q, limit, cursor)
	}
	if err != nil {
		return nil, err
	}
	rows, total := page.rows, page.total
	columns, seen, flatRows, flatRaws := flattenRows(rows)

	// A projection that came back without the properties the requested columns
	// flatten from would empty the table with a 200 and no error anywhere. The
	// mapping above sends both the column's own name and the property it may
	// derive from, so this does not fire for an ordinary model — but whether a
	// name resolves is the remote serializer's decision, not ours.
	//
	// THIS BRANCH IS NOT DEAD CODE. Measured on the bundled demo stack (NetBox
	// 4.4, plugin log): a COUNT query against core/background-queues or
	// core/background-workers takes it every time. Those two are hand-rolled
	// serializers over RQ's queue and worker objects — they ignore ?fields=
	// entirely AND have no `id`, so countOnlyFields asks for the one property
	// neither model has and none of the objects that come back carries it. The
	// refetch is what makes that count a number instead of a blank stat.
	//
	// The comparison is against the FETCHABLE selection, not spec.Fields: the
	// computed utilization columns are added a few lines below, so a selection
	// naming only those has nothing here for the response to match and an empty
	// projectColumns would mean nothing was wrong. When there is nothing
	// fetchable to compare, this check has nothing to tell us — and a second,
	// unprojected fetch of up to MaxLimit whole objects is exactly the cost the
	// projection exists to avoid, so skip it.
	if projection != "" && len(flatRows) > 0 && len(fetchable) > 0 && len(projectColumns(columns, fetchable)) == 0 {
		log.DefaultLogger.Warn("netbox: field projection returned none of the requested columns; refetching whole objects",
			"objectType", logSafe(spec.ObjectType), "fields", logSafe(projection))
		q.Del(fieldsParam)
		// The config-context exclusion rides with the projection and only with
		// it: unprojected rows carry a config_context column, and leaving the
		// parameter set here would delete it from the very refetch whose whole
		// purpose is to return every column. Only when the exclusion is OURS,
		// though — same reasoning as the refetch above, and the same consequence
		// for getting it wrong: deleting a caller's own `exclude` filter answers a
		// wider question than they asked, with a full table to make it look right.
		if !callerOwnsExclude {
			q.Del(excludeParam)
		}
		if page, err = p.fetchList(ctx, spec.ObjectType, q, limit, cursor); err != nil {
			return nil, err
		}
		rows, total = page.rows, page.total
		columns, seen, flatRows, flatRaws = flattenRows(rows)
	}

	var notes, warnings []string
	if orderNote != "" {
		notes = append(notes, orderNote)
	}
	var capped *provider.Cap
	if isUtilizationType(spec.ObjectType) && wantsUtilization(spec.Fields) {
		// APPEND, never assign. Both features can be live on one query —
		// ipam/prefixes is in the ordering allow-list and is also one of the two
		// types isUtilizationType covers — so assigning here dropped an ordering
		// note that had already been recorded, and the panel then showed NetBox's
		// natural order with nothing saying why. Warnings take the same treatment
		// for the same reason, though nothing writes one before this point today.
		utilNotes, utilWarnings, utilCapped := p.enrichUtilization(ctx, spec.ObjectType, spec.Fields, flatRaws, flatRows)
		notes = append(notes, utilNotes...)
		warnings = append(warnings, utilWarnings...)
		capped = utilCapped
		for _, name := range utilizationFieldNames() {
			if !seen[name] {
				seen[name] = true
				columns = append(columns, name)
			}
		}
	}

	if len(spec.Fields) > 0 {
		columns = projectColumns(columns, spec.Fields)
	}

	if n := scaleNote(spec, page, len(flatRows), limit); n != "" {
		notes = append(notes, n)
	}

	// A custom field set on no row in this result has nothing for the frame
	// builder to infer a type from, so it would fall back to a string column of
	// '' — and flip to a nullable number the moment one row is set. The field's
	// DEFINITION knows the type on both days; declare it so the column is the
	// same type whatever the rows hold (see provider.Result.ColumnTypes). Only
	// the all-null custom-field columns are asked about: values decide the
	// rest, and the restriction is what keeps the index's unknown-column rule
	// exact. A result is not cached, so whether the declaration is settled
	// does not matter here: an unsettled one is an untyped refresh, no more.
	columnTypes, _, _ := p.declaredCustomFieldTypes(ctx, allNullCustomFieldColumns(columns, flatRows))

	return &provider.Result{Columns: columns, Rows: flatRows, Total: total, Notes: notes, Warnings: warnings, Capped: capped, MaxRows: MaxLimit, ColumnTypes: columnTypes}, nil
}

// projectionRejected reports whether a failed list request is worth repeating
// WITHOUT the ?fields= projection.
//
// Only an APIError qualifies: NetBox answered, and what it answered was a
// refusal of the request as sent — which is the only kind of failure dropping a
// request parameter can fix. A 500 is the observed shape (prefetch_related on a
// name that is not a relation), a 400 is the shape a stricter release would use,
// and neither is worth enumerating because the point is that this is the remote
// serializer's decision and we do not get to predict it.
//
// Two exclusions, both so the retry cannot make a bad situation worse:
//
//   - A TRANSPORT failure (no APIError) means the request never got an answer at
//     all. The projection had nothing to do with it, and repeating costs another
//     full timeout on a query that is already slow.
//   - The load-shedding statuses getListPageRetry already retries. Those are
//     "not now", not "not that": by the time one reaches here it has been tried
//     three times, and a fourth attempt carrying different parameters would be
//     pressure on an upstream that just said it had none to spare.
//
// sent is the query the failed request carried, because a 400 alone does not say
// WHAT was refused — see blamesAnotherParam.
func projectionRejected(err error, sent url.Values) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	// An ALLOWLIST, not "everything except the transient ones". The fallback
	// costs a second, unprojected fetch of up to MaxLimit whole objects, so it
	// must only fire where dropping ?fields= could plausibly be the cure.
	//
	// 400 is NetBox rejecting a PARAMETER outright — which is only this
	// parameter's problem when the parameter it names is the projection. 500 is
	// the case this fallback was built for: a projected name that is a real
	// relation on ANOTHER model reaches prefetch_related() and raises
	// AttributeError (prefix_count on dcim/sites strips to prefix and 500s).
	//
	// Everything else is emphatically not a projection problem, and two of them
	// are actively harmful to repeat. 429 carries a Retry-After that this
	// immediate re-request ignores — getListPageRetry deliberately does NOT
	// retry a 429 for exactly that reason, and firing here would defeat that
	// through a different door, doubling load on an instance already telling us
	// to slow down. 401/403 will answer the same twice, and 404 means the object
	// type is not there at all.
	switch apiErr.Status {
	case http.StatusInternalServerError:
		return true
	case http.StatusBadRequest:
		return !blamesAnotherParam(apiErr.Body, sent)
	}
	return false
}

// upstreamServerError reports whether NetBox answered with a 500 — the one
// status Query repeats an UNCHANGED request for.
//
// It is not in retryable()'s list on purpose: a 500 is usually the same answer
// twice (a serializer that cannot render an object renders it no better a second
// later), so it is not worth three attempts and two seconds of backoff on every
// page of a paged walk. One immediate repeat per QUERY is a different trade, and
// it is the one this call site has always made — an unprojected query got it as
// a side effect of the projection fallback long before there was a name for it.
//
// Be honest about what that repeat costs, because it is not one request. Query
// repeats the fetchList CALL, so the walk starts again at page 1: every page
// that already succeeded is fetched a second time, and a query that reaches
// MaxLimit walks up to 20 pages (MaxLimit / pageSize). A 500 on the last page of
// such a walk therefore costs 20 wasted requests, not one, and the worst case
// for the whole query is just under two full walks.
//
// It remains the right trade HERE, and the boundary is what makes it one: this
// fires once per query rather than once per page, only on a status that ends the
// query outright, and only where the alternative is an error toast on a panel a
// second attempt would have drawn. It is not a licence to move the repeat down
// into the page loop, where the same idea would multiply by the page count
// instead of being bounded by it.
func upstreamServerError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusInternalServerError
}

// blamesAnotherParam reports whether a 400 body names a request parameter that
// is NOT the projection — i.e. whether NetBox has already told us the projection
// is not what it refused.
//
// NetBox validates list parameters through django-filter and DRF renders the
// failure as a JSON object keyed by the offending parameter. Measured on the
// bundled demo (NetBox 4.4.10):
//
//	?speed__empty=true               400 {"speed__empty":["Enter a whole number."]}
//	?status=nosuch                   400 {"status":["Select a valid choice. …"]}
//	?created__gte=notadate           400 {"created__gte":["Enter a valid date/time."]}
//	all three at once                400 {"id":[…],"status":[…],"created__gte":[…]}
//
// That keying is django-filter's, not NetBox's: DjangoFilterBackend raises
// translate_validation(filterset.errors), which is an OrderedDict keyed by
// parameter name. Checked in the containers for BOTH supported extremes —
// 4.4.10 and 4.6.0 both run django-filter 25.2 with the identical code path — so
// this is not a 4.4-shaped reading of a 4.4-only body.
//
// Without this check every one of those was read as "the projection was
// rejected": the fallback stripped ?fields= and re-asked THE SAME BAD FILTER,
// unprojected and at up to MaxLimit whole objects, for a second identical 400.
// Two round trips, the expensive one last, and a WARN naming the projection as
// the cause of a failure it had no part in.
//
// It stays deliberately hard to satisfy, because the fallback is what makes
// over-projection survivable and the case it was built for MUST keep firing.
// Only a key that was actually SENT counts as blame, so an unparseable body, a
// body from something other than NetBox (a proxy's HTML 400), a shape we have
// not seen, or one naming `fields`/`exclude` themselves all keep the fallback.
//
// Do not "simplify" this back to `case http.StatusBadRequest: return true`.
func blamesAnotherParam(body string, sent url.Values) bool {
	for _, param := range blamedParams(body) {
		// fields and exclude ARE the projection: a release strict enough to
		// refuse them by name is exactly what this fallback exists for.
		if param == fieldsParam || param == excludeParam {
			continue
		}
		if _, ok := sent[param]; ok {
			return true
		}
	}
	return false
}

// blamedParams returns the TOP-LEVEL keys of a JSON object body — the parameter
// names in a django-filter/DRF validation error — reading as far as the body
// goes and stopping there.
//
// Reading it as a stream rather than json.Unmarshal-ing the whole thing is the
// point, because the body handed to us is not the whole thing: APIError.Body has
// already been through snippet(), which cuts it to 300 characters so the log
// line and the Grafana toast stay readable. A whole-body unmarshal fails on
// anything past that cut, and the failure degrades to "the projection was
// rejected" — so the misattributed refetch survived for exactly the bodies this
// check was written to catch.
//
// That cut is NOT the "out of reach" that an earlier revision of this comment
// claimed at ~40 bytes per parameter. A single invalid-choice error is 81 bytes
// ({"status":["Select a valid choice. nosuch is not one of the available
// choices."]}), and five stale values in a panel's filter row measured 319 bytes
// against the demo — see netboxFilterError400 in the tests, which is that body.
//
// What remains out of the parser's sight is only a key BEYOND the cut, and that
// keeps the fallback armed, which is the safe direction. A key whose value is
// truncated still counts: the key is NetBox's word regardless of how much of the
// message came with it.
func blamedParams(body string) []string {
	dec := json.NewDecoder(strings.NewReader(body))
	// Anything that is not a JSON object — an empty body, a proxy's HTML, a bare
	// array — names no parameter at all.
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var params []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return params
		}
		// The closing brace, or a shape we do not recognise.
		key, ok := tok.(string)
		if !ok {
			return params
		}
		// Decoding the value (rather than reading its tokens) is what keeps
		// NESTED keys out of this list: only a key NetBox used as a parameter
		// name may disarm the fallback.
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return append(params, key)
		}
		params = append(params, key)
	}
}

// hugeUnfilteredTotal is the match count above which an UNFILTERED object query
// is told, in so many words, to add a filter.
//
// The threshold is a SIZE argument, not a timing one. Counting is cheap on the
// object types most instances are largest in and only becomes the dominant term
// on a model holding millions of rows; how expensive it gets there depends on
// how the NetBox behind it is hosted, so no fixed number would travel.
//
// The size argument is what fixes the
// threshold at a round million: MaxLimit is 10,000 rows, so at a million objects
// the very largest page this datasource will ever return is under 1% of the
// table. No panel above this line is showing the user their data; it is showing
// them an arbitrary corner of it, and the only useful next action is a filter.
//
// The bundled demo instance holds 15 devices and 23 IP addresses — five orders
// of magnitude below — so this cannot fire there.
const hugeUnfilteredTotal = 1_000_000

// scaleNote returns the INFO note, if any, that a result owes the user about its
// own scale. Empty string for the overwhelmingly common case: a result whose
// size speaks for itself.
//
// Two situations, and they are mutually exclusive because one needs a count and
// the other exists because there is none:
//
//   - COUNTED and enormous and unfiltered: the count was paid for anyway, so it
//     is free to say what it implies. Purely factual notices already state
//     "showing X of Y" (pkg/plugin/notices.go); this one adds the imperative,
//     which that notice deliberately withholds because it fires on every routine
//     browse. Here it does not: nothing under a million objects reaches it.
//
//   - UNCOUNTED and full: cursor paging bought its speed by not counting, so the
//     truncation notice that would normally say "showing 100 of N"
//     cannot fire. A full page with no count is otherwise indistinguishable from
//     a complete answer, which is precisely the silent-wrong-answer case the
//     opt-in must not create.
func scaleNote(spec provider.QuerySpec, page listResult, shown, limit int) string {
	if spec.CountOnly {
		return "" // the caller renders the number itself; there is no table to annotate
	}
	if !page.totalKnown {
		if len(page.rows) < limit {
			return "" // short page: every matching object is here, whatever the count would have been
		}
		return fmt.Sprintf(
			"Showing the first %s objects by ID. Fast paging is on for this data source, so NetBox is not "+
				"counting the matches: the total is unavailable and there may be many more. Add a filter to see a "+
				"complete result, or turn off fast paging in the data source settings to get totals back.",
			thousands(shown))
	}
	if page.total >= hugeUnfilteredTotal && unfilteredQuery(spec) && shown < page.total {
		return fmt.Sprintf(
			"%s holds %s objects and this query has no filters, so it can only ever show the first few. "+
				"Add a filter to narrow it to the objects you mean.",
			spec.ObjectType, thousands(page.total))
	}
	return ""
}

// unfilteredQuery reports whether a spec narrows the object set at all, judged
// on the parameters that actually reach NetBox: a filter row with no value is
// dropped by buildFilterValues and narrows nothing, so counting rows would call
// a half-typed filter a filter.
func unfilteredQuery(spec provider.QuerySpec) bool {
	return len(buildFilterValues(spec.Filters)) == 0
}

// thousands formats n with comma separators (1234567 -> "1,234,567").
// Duplicated deliberately from pkg/plugin: this package is the backend-agnostic
// seam and does not import the plugin layer (nor it this one).
func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// fetchOnlyFields lists what a query READS from a row without returning it as a
// column, and therefore must fetch even though no column names it.
//
// Two sources, both of which fail silently under a projection rather than
// loudly: a join key derives its output from row[Source] whatever the selection
// says (see pkg/plugin.joinKeySources), and utilization is computed from
// properties of the object that the utilization columns do not name.
func fetchOnlyFields(spec provider.QuerySpec) []string {
	extra := spec.KeyFields
	if isUtilizationType(spec.ObjectType) && wantsUtilization(spec.Fields) {
		extra = append(append([]string(nil), extra...), utilizationSourceFields()...)
	}
	return extra
}

// flattenRows flattens raw objects into value maps, returning the union of their
// columns in first-seen order (and that union as a set), plus the raw objects
// index-aligned with the flattened rows.
func flattenRows(rows []json.RawMessage) (columns []string, seen map[string]bool, flatRows []map[string]interface{}, flatRaws []json.RawMessage) {
	seen = map[string]bool{}
	flatRows = make([]map[string]interface{}, 0, len(rows))
	flatRaws = make([]json.RawMessage, 0, len(rows)) // index-aligned with flatRows
	for _, raw := range rows {
		cols, vals, err := flattenObject(raw)
		if err != nil {
			continue
		}
		for _, c := range cols {
			if !seen[c] {
				seen[c] = true
				columns = append(columns, c)
			}
		}
		flatRows = append(flatRows, vals)
		flatRaws = append(flatRaws, raw)
	}
	return columns, seen, flatRows, flatRaws
}

// listResult is a completed paged walk: the raw objects, and the match count the
// source reported for the query.
//
// totalKnown exists because cursor mode does not answer "how many matched": it
// is the difference between "nothing matched" and "we did not ask", which a bare
// total of 0 cannot express. Every consumer of a count goes through this struct
// so that distinction cannot be lost between the envelope and the caller.
type listResult struct {
	rows       []json.RawMessage
	total      int
	totalKnown bool
}

// fetchRows pages through a NetBox list endpoint in the default OFFSET mode,
// returning raw object JSON and the total match count reported by the list
// envelope (from the first page).
//
// This is the signature every caller but Query uses, and it stays offset-only on
// purpose: FieldValues decides whether a sample IS the population by comparing
// against this total, utilization sums envelope counts, and Changes sorts with
// ?ordering= (which cursor mode rejects outright). A count of 0 from here means
// the envelope said 0.
func (p *Provider) fetchRows(ctx context.Context, objectType string, q url.Values, limit int) ([]json.RawMessage, int, error) {
	res, err := p.fetchList(ctx, objectType, q, limit, false)
	if err != nil {
		return nil, 0, err
	}
	return res.rows, res.total, nil
}

// startParam is NetBox 4.6's CURSOR pagination parameter: ?start=<pk> pages by
// primary key instead of by offset. NetBoxPagination's own docstring states that
// "in cursor mode, count is omitted (null) for performance" — it never calls
// .count(), which is where the time goes on a multi-million-row table. The
// saving is entirely the dropped count, not the change of ordering: the same
// probe with &ordering=id in offset mode is barely faster than plain offset.
//
// Its cost is that rows come back in ascending pk order rather than the model's
// natural ordering, and that there is no total. Both are visible to the user, so
// this parameter is only ever sent when the datasource is explicitly configured
// for it AND the caller has said it can answer without a total.
const startParam = "start"

// offsetParam is the default pagination parameter. NetBox rejects it alongside
// start with HTTP 400 ("'start' and 'offset' are mutually exclusive"), so a
// query that already carries one cannot use cursor mode.
const offsetParam = "offset"

// orderingParam is NetBox's sort parameter. It is likewise rejected alongside
// start with HTTP 400 ("Ordering cannot be specified in conjunction with
// cursor-based pagination"), so a query that sorts cannot use cursor mode.
const orderingParam = "ordering"

// cursorLegal reports whether a query may be walked in cursor mode at all —
// independently of whether the datasource opted in. NetBox answers both of these
// combinations with a 400, so sending them would replace a slow panel with a
// broken one.
func cursorLegal(q url.Values) bool {
	return !q.Has(startParam) && !q.Has(offsetParam) && !q.Has(orderingParam)
}

// fetchList pages through a NetBox list endpoint and returns the objects plus
// what is known about the match count.
//
// cursor selects NetBox's cursor pagination (see startParam). Callers must not
// pass true unless they can present a result with no total AND the query is
// cursorLegal.
//
// Cursor mode DEGRADES SAFELY on an instance that does not implement it: NetBox
// ignores unknown query parameters silently, so a 4.4 instance answers ?start=0
// with an ordinary offset-mode page — count present, natural ordering, `next`
// carrying an offset. That is exactly the shape the offset walk expects, and the
// envelope count is the honest one, so the result is reported as counted. This
// is also the control that proves the parameter was honored where it was: a
// cursor page is identifiable by its null count, not by our having asked.
func (p *Provider) fetchList(ctx context.Context, objectType string, q url.Values, limit int, cursor bool) (listResult, error) {
	pq := url.Values{}
	for k, vs := range q {
		pq[k] = vs
	}
	pq.Set("limit", fmt.Sprintf("%d", min(limit, pageSize)))
	if cursor {
		// start=0 is "from the lowest pk"; NetBox's own `next` link then carries
		// start=<last pk + 1> for every subsequent page, so the walk below is
		// unchanged.
		pq.Set(startParam, "0")
	}
	next := p.client.apiURL(objectType, pq)

	out := listResult{totalKnown: true}
	firstPage := true
	for next != "" && len(out.rows) < limit {
		page, err := p.getListPageRetry(ctx, next)
		if err != nil {
			return listResult{}, err
		}
		if firstPage {
			out.total, out.totalKnown = page.total()
			firstPage = false
		}
		out.rows = append(out.rows, page.Results...)
		if page.Next == nil {
			break
		}
		if next, err = p.client.nextPageURL(next, *page.Next); err != nil {
			return listResult{}, err
		}
	}
	if len(out.rows) > limit {
		out.rows = out.rows[:limit]
	}
	return out, nil
}

// retryBackoff is the wait before each successive retry of a list page. Two
// retries, ~2s of added latency in the worst case, which is well inside a
// dashboard's patience and far cheaper than losing a 20-page walk to one blip.
var retryBackoff = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond}

// retryable reports whether a failed request is worth repeating: something that
// says "not now" rather than "not ever".
//
// Two kinds qualify, and both were observed aborting real paged walks against
// a large remote NetBox. An upstream/gateway STATUS (502/503/504) is the load-shedding
// answer a busy instance gives. A TRANSPORT failure is the same event seen one
// layer down — the run that motivated this died with "read: connection reset by
// peer" on page 18 of 20, which is not an APIError at all, so a status-only rule
// would have let the exact failure through.
//
// Everything else is left alone. 429 carries a Retry-After that a fixed backoff
// would ignore. A 4xx is the user's answer, and repeating it only delays the
// real error — multiplied by the batch count on the fan-out hops. A decode
// failure means the body was not what we asked for, which a second identical
// request does not change.
//
// A cancelled or expired context is explicitly NOT retryable even though it
// surfaces as a transport error: the caller has gone away, and the retry loop's
// own ctx.Done guard would already refuse to sleep — this just keeps the
// classification honest rather than relying on that.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	return transientTransport(err)
}

// transientTransport reports whether an error is the connection failing, as
// opposed to the server answering. A repeat of an idempotent GET is the right
// response to all of these; none of them says anything about the request.
//
// The point of matching several shapes is that ONE event — the connection
// dropping mid-request — reaches this function differently depending on WHEN it
// dropped, and the earlier, narrower rule only recognised the early half:
//
//   - Before the response headers, http.Client.Do fails and client.getBytes
//     wraps a *url.Error. That was the whole of the old rule.
//   - AFTER the headers, Do has already returned 200 and the connection dies
//     inside io.ReadAll on the body, which getBytes wraps on its own as
//     "read body <url>: …". There is no *url.Error on that path and no APIError
//     either, so the old rule called the late failure permanent — and a reset
//     "on page 18 of 20", the failure getListPageRetry cites as its reason for
//     existing, is by definition a late one.
//
// What is included, and why each is a distinct shape rather than a synonym:
//
//   - net.Error covers the connection-level errors as the net package reports
//     them (*net.OpError from a read/write on a dead socket, *net.DNSError, and
//     *url.Error itself, which implements the interface). This is the bulk.
//   - io.ErrUnexpectedEOF is NOT a net error: it is net/http's own verdict when
//     a body stops short of its Content-Length or a chunked body ends without
//     its terminator. The peer went away mid-body; the socket never complained.
//   - syscall.ECONNRESET / EPIPE normally arrive already inside a *net.OpError
//     and are caught above. They are named explicitly for the paths that unwrap
//     to a bare errno — h2 and some proxy/PDC transports do — so the same event
//     cannot be classified two different ways depending on the dialer Grafana
//     handed us.
//
// Deliberately EXCLUDED, and this is the interesting one: a bare io.EOF. It is
// not evidence of a transport failure at all. io.ReadAll treats EOF as the
// normal end of a read and never returns it, so a body-read failure surfaces as
// ErrUnexpectedEOF above; what does surface as io.EOF is a decode of an empty or
// truncated document, and a second identical request gets the same empty
// document. (The one genuinely transient io.EOF — a server closing an idle
// keep-alive connection just as we reuse it — arrives from Do wrapped in a
// *url.Error, so it is retried as a net.Error without io.EOF needing to be
// retryable in its own right.) Treating bare EOF as transient would buy nothing
// here and would turn "this endpoint returns nothing" into three requests and
// 2s of backoff before the same error.
//
// Excluded EXPLICITLY, because net.Error would otherwise swallow it: a TLS
// certificate failure. An x509 error arrives from Do inside a *url.Error, and
// *url.Error satisfies net.Error, so widening to that interface silently made
// an expired or untrusted certificate "transient". It is not — a certificate
// does not become valid 2s later — and the cost is that every page of every
// query against a misconfigured endpoint takes three attempts and two seconds
// of backoff to report the same error the first attempt already knew. The doc
// comment here used to claim this case was excluded while the code retried it;
// the check below is what makes the claim true.
//
// Note that widening to net.Error does NOT quietly make the datasource's own
// Timeout retryable. An http.Client.Timeout that expires during the body read
// reports "(Client.Timeout or context cancellation while reading body)" and
// unwraps to context.DeadlineExceeded, so retryable's first guard rejects it
// before reaching here — the user's stated budget for this datasource is still
// spent once, not three times.
func transientTransport(err error) bool {
	// Certificate failures first: they reach us inside a *url.Error, which is a
	// net.Error, so the check below would otherwise call them transient.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return false
	}

	// net.Error covers the connection-level shapes: *net.OpError from a read or
	// write on a dead socket, *net.DNSError, *url.Error from the Do path, and —
	// because syscall.Errno implements Timeout() and Temporary() on every
	// platform we build for — a bare ECONNRESET or EPIPE that unwrapped to an
	// errno. An earlier revision listed those two errnos separately; that arm
	// was unreachable, and a mutation test confirmed removing it changed
	// nothing.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// Not a net error at all: net/http's own verdict when a body stops short of
	// its Content-Length or a chunked body ends without its terminator. The peer
	// went away mid-body and the socket never complained.
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// getListPageRetry fetches one list page, retrying a transient failure (see
// retryable) with bounded backoff.
//
// Paging is where a transient upstream failure is most expensive: a walk of N
// pages fails if ANY one of them does, so the chance of losing the whole query
// grows with the result size — exactly the case this provider is being made to
// support. Measured against a large remote NetBox, a 20-page topology walk
// aborted on most real runs (502/503, and a connection reset on page 18 of 20)
// with no retry anywhere in the path. The user saw an error toast and an empty
// panel for something that succeeded on the next attempt.
//
// A GET on a list endpoint is safe to repeat, and the retry is bounded and
// context-aware, so a cancelled query (a closed dashboard, an expired deadline)
// stops immediately rather than sleeping out its backoff. On success first try —
// every request on a healthy instance — this costs one comparison.
func (p *Provider) getListPageRetry(ctx context.Context, rawURL string) (listPage, error) {
	page, _, err := p.getListPageRetryN(ctx, rawURL)
	return page, err
}

// getListPageRetryN is getListPageRetry, additionally reporting how many HTTP
// requests it actually made. The count exists for the utilization cost note: a
// page that succeeded on its third attempt cost the upstream three requests,
// and reporting one understates the load precisely when the instance is
// struggling, which is when that number is worth reading.
func (p *Provider) getListPageRetryN(ctx context.Context, rawURL string) (listPage, int, error) {
	requests := 1
	page, err := p.client.getListPage(ctx, rawURL)
	for attempt := 0; err != nil && retryable(ctx, err) && attempt < len(retryBackoff); attempt++ {
		select {
		case <-ctx.Done():
			return listPage{}, requests, err
		case <-time.After(retryBackoff[attempt]):
		}
		log.DefaultLogger.Warn("netbox: retrying list page after a transient upstream failure",
			"attempt", attempt+1, "of", len(retryBackoff),
			"error", logSafe(err.Error()))
		page, err = p.client.getListPage(ctx, rawURL)
		requests++
	}
	return page, requests, err
}

// Fields returns the columns of an object type, inferred from a sample object.
func (p *Provider) Fields(ctx context.Context, objectType string) ([]provider.Field, error) {
	// Partition the cache by branch: a branch may define custom fields that main
	// (or another branch) does not, so main and each branch must cache their
	// field sets separately. The key is the branch as resolved (branchKey), not
	// the value the query carried. The null byte cannot appear in an object-type
	// path or a branch schema id, so it is a collision-free key separator.
	cacheKey := objectType
	if branch := p.client.branchKey(ctx); branch != "" {
		cacheKey = objectType + "\x00" + branch
	}

	p.mu.Lock()
	if e, ok := p.fields[cacheKey]; ok && time.Now().Before(e.expiry) {
		p.mu.Unlock()
		return e.fields, nil
	}
	p.mu.Unlock()

	q := url.Values{}
	q.Set("limit", "1")
	rows, _, err := p.fetchRows(ctx, objectType, q, 1)
	if err != nil {
		return nil, err
	}
	var fields []provider.Field
	// A list whose custom-field types were read off the definition index is
	// bound by that index. cacheable is false when the index was a stand-in
	// (still in flight when this sample was typed, so an unset custom field
	// fell back to string): such a list is returned but not stored, and the
	// next call reads the settled index. expiry is pulled in to the index's
	// own when the index was consulted: a failure NetBox gave is settled for a
	// minute, not for cacheTTL, and a list cached past that point would keep
	// offering string operators after the retry that repairs the index.
	cacheable := true
	expiry := time.Now().Add(cacheTTL)
	if len(rows) > 0 {
		cols, vals, err := flattenObject(rows[0])
		if err == nil {
			// The sample is one object, so a custom field it leaves unset would be
			// typed as string here and offered string operators in the editor; the
			// field's definition knows better. Values still win when present.
			declared, settled, validUntil := p.declaredCustomFieldTypes(ctx, allNullCustomFieldColumns(cols, []map[string]interface{}{vals}))
			for _, c := range cols {
				t := inferType(c, vals[c])
				if vals[c] == nil && strings.HasPrefix(c, cfPrefix) {
					if d, ok := declared[c]; ok {
						t = d
					}
					if !settled {
						cacheable = false
					} else if validUntil.Before(expiry) {
						expiry = validUntil
					}
				}
				fields = append(fields, provider.Field{Name: c, Type: t})
			}
		}
	}

	if isUtilizationType(objectType) {
		for _, name := range utilizationFieldNames() {
			fields = append(fields, provider.Field{Name: name, Type: provider.FieldTypeNumber})
		}
	}

	if cacheable {
		p.mu.Lock()
		dropExpired(p.fields, time.Now(), func(e fieldsCacheEntry) time.Time { return e.expiry })
		p.fields[cacheKey] = fieldsCacheEntry{fields: fields, expiry: expiry}
		p.mu.Unlock()
	}
	return fields, nil
}

// FilterFields returns the valid filter parameters and operators for an object
// type, parsed from NetBox's OpenAPI schema (cached schemaTTL). On any fetch or
// parse failure it returns an error and an empty slice so callers can fall back.
func (p *Provider) FilterFields(ctx context.Context, objectType string) ([]provider.FilterField, error) {
	entry, err := p.schema(ctx)
	if err != nil {
		return nil, err
	}
	return entry.filters[objectType], nil
}

// schema returns the parsed OpenAPI schema for the context's branch, from the
// cache or from one fetch shared by every caller that arrives while it runs.
// One fetch feeds both derivations: the filter params FilterFields serves, and
// the dimension index FieldValues resolves against.
//
// A failure to parse the dimension index is NOT fatal — FilterFields is the
// long-standing consumer and must keep working — so the entry is cached with a
// nil index and FieldValues falls back to sampling.
func (p *Provider) schema(ctx context.Context) (schemaCacheEntry, error) {
	branch := p.client.branchKey(ctx)

	p.mu.Lock()
	if e, ok := p.schemaByBranch[branch]; ok && time.Now().Before(e.expiry) {
		p.mu.Unlock()
		return e, nil
	}
	flight := p.schemaFlightByBranch[branch]
	if flight == nil {
		flight = &schemaFlight{done: make(chan struct{})}
		if p.schemaFlightByBranch == nil {
			p.schemaFlightByBranch = map[string]*schemaFlight{}
		}
		p.schemaFlightByBranch[branch] = flight
		// Detached from this caller's cancellation, as the custom-field fetch
		// is: the callers waiting on it are not all this one, and a panel
		// closed mid-download must not fail the others. The HTTP client's
		// timeout still bounds it.
		go p.runSchemaFetch(context.WithoutCancel(ctx), branch, flight)
	}
	p.mu.Unlock()

	select {
	case <-flight.done:
		return flight.entry, flight.err
	case <-ctx.Done():
		return schemaCacheEntry{}, ctx.Err()
	}
}

// runSchemaFetch downloads and parses the schema for branch, publishes it to
// the cache on success, and retires the flight under the same lock.
func (p *Provider) runSchemaFetch(ctx context.Context, branch string, flight *schemaFlight) {
	flight.entry, flight.err = p.fetchSchema(ctx)
	p.mu.Lock()
	if flight.err == nil {
		if p.schemaByBranch == nil {
			p.schemaByBranch = map[string]schemaCacheEntry{}
		}
		dropExpired(p.schemaByBranch, time.Now(), func(e schemaCacheEntry) time.Time { return e.expiry })
		p.schemaByBranch[branch] = flight.entry
	}
	delete(p.schemaFlightByBranch, branch)
	p.mu.Unlock()
	close(flight.done)
}

// fetchSchema downloads and parses the OpenAPI schema. The fetch carries the
// branch via ctx (X-NetBox-Branch), so the result belongs to that branch alone.
func (p *Provider) fetchSchema(ctx context.Context) (schemaCacheEntry, error) {
	raw, err := p.client.getBytes(ctx, p.client.apiURL("schema", url.Values{"format": {"json"}}))
	if err != nil {
		return schemaCacheEntry{}, fmt.Errorf("fetch OpenAPI schema: %w", err)
	}
	parsed, err := parseFilterFields(raw)
	if err != nil {
		return schemaCacheEntry{}, fmt.Errorf("parse OpenAPI schema: %w", err)
	}
	dims, err := parseDimensions(raw)
	if err != nil {
		log.DefaultLogger.Warn("netbox: OpenAPI dimension index unavailable; field values fall back to sampling",
			"error", logSafe(err.Error()))
		dims = nil
	}
	return schemaCacheEntry{filters: parsed, dims: dims, expiry: time.Now().Add(schemaTTL)}, nil
}

// FieldValues returns distinct values of a column for autocomplete.
//
// The values come from whichever source can answer COMPLETELY:
//
//   - If the object type holds no more rows than one sample page, the sample IS
//     the population. Every value the column actually takes is in it, so the
//     sample is returned — this is the historical behaviour and it is exactly
//     right for a small instance.
//
//   - Otherwise the sample is provably a fraction of the table, and collecting
//     distinct values from it is arbitrary rather than merely incomplete: on a
//     multi-million-device instance the site column offered 500 of 4030 sites, and WHICH
//     500 was decided by device ordering, so seven eighths of the sites could
//     not be picked at all. There the column's DIMENSION answers instead
//     — the related list endpoint for a foreign key, the schema's enumeration
//     for a choice field — at a cost set by the size of the dimension, not of
//     the fact table.
//
//   - A column with no backing dimension (a name, a description, a custom
//     field) has nowhere else to come from, so it keeps sampling.
//
// The sample is projected with ?fields= to the one property the column derives
// from, which cuts a 500-device page from 1.4 MB to 140 KB without changing a
// single returned value. Projection names are taken from the OpenAPI schema
// because NetBox answers an unknown ?fields= name with empty objects rather
// than an error; the result is re-checked against the response as well, so a
// rejected projection degrades to a full fetch instead of an empty dropdown.
func (p *Provider) FieldValues(ctx context.Context, objectType, field, q string, limit int) ([]string, error) {
	if field == "" {
		return nil, fmt.Errorf("field is required")
	}
	if limit <= 0 || limit > pageSize {
		limit = pageSize
	}

	dim, base, resolved := p.resolveDimension(ctx, objectType, field)

	sample := url.Values{}
	if resolved {
		sample.Set("fields", base)
	}
	rows, total, err := p.fetchRows(ctx, objectType, sample, pageSize)
	if err != nil {
		return nil, err
	}
	if resolved && !projectionHonored(rows, base) {
		// NetBox rejected the projection (an older release, or a property the
		// list serializer does not expose). Re-fetch unprojected rather than
		// report an empty column.
		if rows, total, err = p.fetchRows(ctx, objectType, url.Values{}, pageSize); err != nil {
			return nil, err
		}
	}

	// The sample covered the whole table: it is the complete answer.
	if total <= len(rows) {
		return sampledValues(rows, field, q, limit), nil
	}

	switch dim.kind {
	case dimChoice:
		return choiceValues(dim, base, field, q, limit), nil
	case dimRelated:
		values, err := p.relatedValues(ctx, dim.endpoint, base, field, q, limit)
		if err == nil {
			return values, nil
		}
		// A dimension that will not answer must not lose the user their
		// dropdown; the (incomplete) sample is still better than an error.
		log.DefaultLogger.Warn("netbox: dimension lookup failed; falling back to sampling",
			"objectType", logSafe(objectType), "field", logSafe(field), "dimension", logSafe(dim.endpoint),
			"error", logSafe(err.Error()))
	}

	log.DefaultLogger.Debug("netbox: field values sampled from an incomplete page",
		"objectType", logSafe(objectType), "field", logSafe(field), "sampled", len(rows), "total", total)
	return sampledValues(rows, field, q, limit), nil
}

// resolveDimension looks the column up in the schema-derived index. Any failure
// to read the schema is reported as "unresolved", never as an error: FieldValues
// must keep working on an instance whose schema endpoint is slow or restricted.
func (p *Provider) resolveDimension(ctx context.Context, objectType, field string) (dimension, string, bool) {
	entry, err := p.schema(ctx)
	if err != nil || entry.dims == nil {
		return dimension{}, "", false
	}
	return entry.dims.resolve(objectType, field)
}

// projectionHonored reports whether a ?fields=<base> response actually carries
// that property. NetBox silently returns `{}` per row for an unknown projection
// name, which would otherwise read as "this column has no values".
func projectionHonored(rows []json.RawMessage, base string) bool {
	for _, raw := range rows {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		if _, ok := obj[base]; ok {
			return true
		}
	}
	return len(rows) == 0
}

// sampledValues collects the distinct values of a column from sampled objects.
func sampledValues(rows []json.RawMessage, field, q string, limit int) []string {
	set := newValueSet(q, limit)
	for _, raw := range rows {
		_, vals, err := flattenObject(raw)
		if err != nil {
			continue
		}
		if set.add(vals, field) {
			break
		}
	}
	return set.sorted()
}

// choiceValues enumerates a closed choice column from the OpenAPI schema. It
// costs no request, and unlike sampling it cannot miss a value that is rare in
// the table.
func choiceValues(dim dimension, base, field, q string, limit int) []string {
	set := newValueSet(q, limit)
	for _, choice := range dim.choices {
		if set.add(flattenValues(base, choice), field) {
			break
		}
	}
	return set.sorted()
}

// relatedValues enumerates a foreign-key column from the related list endpoint.
//
// brief=true keeps the payload minimal, and the caller's substring goes upstream
// WHEN A PARAMETER CAN EXPRESS IT (see substringParam) so that typing reaches
// values far past the first page — the tail of a 4030-site instance is
// unreachable any other way, because the response is a flat list with no
// pagination channel back to the UI. The same substring is then applied locally,
// so the result can never be WIDER than the documented "contains" contract even
// where NetBox's search covers extra attributes.
//
// Where no parameter can express it, the local pass is the ONLY filter, and it
// can only see the rows that came back: on a dimension bigger than one page, a
// substring matching solely a row beyond that page yields nothing. That is a
// real limitation, and it is the one to accept here — the alternative, pushing
// the substring at a parameter that searches different text, does not merely
// miss the tail but drops values from the very first page (a ?q=123 that
// excludes site 123). Fewer answers beat wrong ones, and the widened fetch below
// keeps "fewer" as small as one request allows.
func (p *Provider) relatedValues(ctx context.Context, endpoint, base, field, q string, limit int) ([]string, error) {
	query := url.Values{"brief": {"true"}}
	fetch := limit
	exactID := ""
	if q != "" {
		if param := p.substringParam(ctx, endpoint, base, field); param != "" {
			query.Set(param, q)
		} else {
			// Filtering locally: fetch a whole page rather than just the caller's
			// cap, so the pass has the most rows to work with. limit is already
			// capped at pageSize, so this is still ONE request — a bigger one, and
			// brief=true keeps it small.
			fetch = pageSize

			// One case the local pass alone cannot serve: an _id column where the
			// user typed a COMPLETE id belonging to a row beyond the first page.
			// Before this path existed, ?q= happened to find it when the id also
			// appeared in the name ("Site 1234"); dropping the pushdown lost that.
			// NetBox has no substring lookup on id — exact, gt/gte, lt/lte only —
			// so the recovery is an exact id=, fetched alongside rather than
			// instead of the page: a partial "12" must still match 1234 from the
			// page, which an exact filter would exclude. Digits only, so a name
			// like "1234" never reaches it on a non-id column.
			if isIDColumn(base, field) && allDigits(q) {
				exactID = q
			}
		}
	}
	rows, _, err := p.fetchRows(ctx, endpoint, query, fetch)
	if err != nil {
		return nil, err
	}

	// The exact-id recovery above, if it applies. A failure here is NOT fatal:
	// the page already fetched is a complete answer for everything on it, and
	// losing the whole dropdown because one extra lookup failed would be a worse
	// outcome than losing one row beyond the page.
	if exactID != "" {
		idQuery := url.Values{"brief": {"true"}, "id": {exactID}}
		if extra, _, idErr := p.fetchRows(ctx, endpoint, idQuery, 1); idErr == nil {
			rows = append(rows, extra...)
		}
	}

	set := newValueSet(q, limit)
	for _, raw := range rows {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		if set.add(flattenValues(base, obj), field) {
			break
		}
	}
	return set.sorted(), nil
}

// isIDColumn reports whether field is the numeric-id column flattening derives
// from base (site -> site_id).
func isIDColumn(base, field string) bool { return field == base+"_id" }

// allDigits reports whether s is a non-empty run of ASCII digits, i.e. something
// that could be a NetBox primary key. Deliberately not strconv.Atoi: a value
// like "+1" or " 1" parses but is not what the user typed at an id column, and
// an id NetBox would accept never needs normalising.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// substringParam names the query parameter that narrows the DIMENSION endpoint
// to rows whose value for THIS column contains q — or "" when no parameter can
// express it, in which case the substring is applied locally instead.
//
// The distinction exists because flattening turns one property into several
// columns: `site` renders the related object's display text, `site_id` its
// numeric id, `site_slug` its slug. NetBox's generic ?q= is a TEXT search over
// the model's descriptive fields — SiteFilterSet.search covers name, facility,
// description, both addresses and comments, and reads neither the id nor the
// slug — so it answers the FIRST column and no other. Sending "123" as ?q=123
// excludes site 123 upstream, before the local comparison can ever see it
// (verified against NetBox 4.4: /api/dcim/sites/?q=3 returns 0 on an instance
// that has site id 3).
//
//   - <base>: ?q= searches the same text the column renders. Pushed, as always.
//   - <base>_slug: the slug is a real filter, and `slug__ic` is precisely the
//     case-insensitive "contains" the local pass applies — the same test, run
//     over the whole dimension instead of one page. Only used where the SCHEMA
//     advertises the lookup: it is present on models that have a slug (sites,
//     tenants, roles) and absent on those that do not (devices, VRFs).
//   - anything else, `<base>_id` above all: NetBox's id filter offers exact,
//     gt/gte and lt/lte and no substring lookup at all, so there is nothing to
//     push and the substring stays local.
func (p *Provider) substringParam(ctx context.Context, endpoint, base, field string) string {
	switch field {
	case base:
		return "q"
	case base + "_slug":
		if p.schemaHasLookup(ctx, endpoint, "slug", "ic") {
			return "slug__ic"
		}
	}
	return ""
}

// schemaHasLookup reports whether the OpenAPI schema advertises <field>__<op>
// (or bare <field>, for op "") as a query parameter of objectType's list
// endpoint. It reads the schema entry FieldValues has already fetched and
// cached, so it costs no request; a schema that cannot be read answers false,
// which sends the caller down the local-filter path rather than at a parameter
// that may not exist.
func (p *Provider) schemaHasLookup(ctx context.Context, objectType, field, op string) bool {
	entry, err := p.schema(ctx)
	if err != nil {
		return false
	}
	for _, f := range entry.filters[objectType] {
		if f.Name == field {
			return slices.Contains(f.Operators, op)
		}
	}
	return false
}

// Changes returns change-log events within a time window for annotations.
func (p *Provider) Changes(ctx context.Context, spec provider.ChangeSpec) ([]provider.Change, error) {
	limit := spec.Limit
	if limit <= 0 || limit > MaxLimit {
		limit = defaultLimit
	}
	q := url.Values{}
	q.Set("ordering", "-time")
	if !spec.From.IsZero() {
		q.Set("time_after", spec.From.UTC().Format(time.RFC3339))
	}
	if !spec.To.IsZero() {
		q.Set("time_before", spec.To.UTC().Format(time.RFC3339))
	}
	// ONE normalised list feeds both the pushdown and the local filter below, so
	// the two cannot disagree about case, spacing, or what counts as no filter at
	// all. They did: a whitespace-only entry was dropped by the pushdown (nothing
	// went upstream) but kept by the local filter (every row was discarded), which
	// is a silently empty annotation with no error anywhere.
	wanted := normalizeObjectTypes(spec.ObjectTypes)

	// An object type the instance does not know is an ERROR here, before any
	// change-log request is made, and that is the point of the id form: it is a
	// wrong question, and answering it with an empty annotation track that looks
	// exactly like a quiet window is the silent-wrong-answer this whole function
	// is being fixed for.
	if err := p.pushChangedObjectType(ctx, q, wanted); err != nil {
		return nil, err
	}
	rows, _, err := p.fetchRows(ctx, "core/object-changes", q, limit)
	if err != nil {
		return nil, err
	}

	// The local filter stays, and is not dead code once the type is pushed
	// upstream. It is not a correctness net — an unknown type never reaches the
	// query at all now — it is what makes the pushdown strictly NON-WIDENING:
	// whatever NetBox does with the parameter (ignores it, ORs it, honours only
	// the last value, or does not implement it on this version), the frame can
	// never contain a type the user did not ask for. It is also the only filter
	// at all on the degraded path, where the id map could not be read and a
	// multi-type request is pushed nowhere.
	//
	// Built from the SAME list the pushdown was given, not from spec.ObjectTypes
	// again: " DCIM.Device " resolves to an id whose rows NetBox returns, and must
	// not then be discarded here for its spacing — and a blank entry, which is no
	// filter upstream, must not become "match nothing" here.
	typeFilter := make(map[string]bool, len(wanted))
	for _, t := range wanted {
		typeFilter[t] = true
	}

	var changes []provider.Change
	for _, raw := range rows {
		var oc objectChange
		if err := json.Unmarshal(raw, &oc); err != nil {
			continue
		}
		ct := oc.changedType()
		if len(typeFilter) > 0 && !typeFilter[normalizeObjectType(ct)] {
			continue
		}
		t, _ := time.Parse(time.RFC3339, oc.Time)
		changes = append(changes, provider.Change{
			Time:       t,
			Action:     oc.actionLabel(),
			ObjectType: ct,
			ObjectRepr: oc.ObjectRepr,
			User:       oc.userName(),
			URL:        oc.deepLink(p.client.BaseURL()),
		})
	}
	return changes, nil
}

// changedObjectTypeIDParam selects change-log rows by the numeric object-type id
// of the object that changed. This is the parameter the pushdown uses.
//
// changedObjectTypeParam is the same filter keyed by app_label.model. It is only
// ever used as the single-type fallback when the id map cannot be read; see
// pushChangedObjectType for why it cannot be the primary form.
const (
	changedObjectTypeIDParam = "changed_object_type_id"
	changedObjectTypeParam   = "changed_object_type"
)

// objectTypesEndpoint answers app_label/model/id for every model the instance
// knows, including plugin models — 154 rows on the demo (4.4.10) and 179 on
// 4.6.4, since the set grows with the version and with installed plugins. That
// is exactly why the map is read rather than hardcoded, and why nothing here
// depends on its size.
//
// core/ is the canonical spelling and the only one that works everywhere.
// extras/object-types is a legacy alias: 4.4.10 answers it with the same rows
// AND the same canonical self-URL (…/api/core/object-types/33/), but 4.6 no
// longer registers object-types under extras (verified in both containers:
// extras/api/urls.py registers it on 4.4, only core/api/urls.py does on 4.6.0)
// and the legacy path 404s there. Reading the alias would leave the id map
// unreadable on every newer instance — the whole pushdown degrading to the
// silently empty annotation it exists to fix, and, since only successful
// fetches are cached, a fresh doomed request on every refresh. The demo cannot
// catch that, because there the alias still works.
const objectTypesEndpoint = "core/object-types"

// maxObjectTypes caps the object-type walk. 154 rows today; the cap is set far
// above any plausible plugin count so it never truncates in practice, and exists
// only so a malformed `next` cannot page forever.
const maxObjectTypes = 5000

// normalizeObjectType canonicalises a user-supplied app_label.model. The name
// parameter this pushdown replaced matched case-insensitively (measured:
// ?changed_object_type=IPAM.FHRPGroup returns the same count=14 as the lowercase
// form) and tolerated surrounding whitespace (?changed_object_type=dcim.device%20
// returns count=35), and the annotation editor's MultiSelect allows custom
// values. So the id lookup normalises the same way: the id form must not be
// STRICTER than the name form it replaces, or a query that worked yesterday
// becomes today's hard error.
func normalizeObjectType(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// normalizeObjectTypes canonicalises a requested type list and drops everything
// that is not a filter: blanks and duplicates.
//
// A whitespace-only entry is dropped rather than reported as an unknown type,
// because that is what NetBox itself does with it: measured on the demo,
// ?changed_object_type=%20 returns count=274 — the unfiltered total, identical
// to the empty value. Doing it HERE, once, is the point: the pushdown and the
// local filter read the same list, so a blank entry cannot mean "no filter"
// upstream and "match nothing" locally.
func normalizeObjectTypes(types []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range types {
		t = normalizeObjectType(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// pushChangedObjectType moves the annotation's object-type filter into the
// NetBox query, one repeated changed_object_type_id= per requested type.
//
// Without it, Changes reads the N newest rows of EVERY type and filters in Go.
// That is not a slow answer, it is a WRONG one: on a 38.8M-row change log a
// device annotation came back empty while device changes existed, because the
// newest N rows were all some other type. The local pass can only ever discard
// rows it was given, and by then the interesting ones are gone.
//
// It is the ID form and not the name form, which costs a cached
// /api/core/object-types/ read to resolve, and buys two things the name form
// cannot give at any price:
//
//   - MULTI-TYPE IS PORTABLE. Repeating the NAME parameter is an OR on NetBox
//     4.6.4 but LAST-VALUE-WINS on 4.4.10: ?…=dcim.site&…=ipam.fhrpgroup returned
//     count=14 (the fhrpgroup total) and the same pair reversed returned count=3
//     (the site total), measured on the demo. Pushing two names would silently
//     drop one — a fresh instance of exactly the bug this function removes.
//     Repeating the ID parameter ORs on both: ids 6 and 86 returned 17, exactly
//     3 + 14. (Comma is not list syntax in either form, and the two forms refuse
//     it differently. Measured on the demo, 4.4.10: ?changed_object_type_id=6,86
//     is HTTP 400, {"changed_object_type_id":["“6,86” is not a valid value."]},
//     while ?changed_object_type=dcim.site,ipam.fhrpgroup is HTTP 200 with
//     count=0 — the name form reads the whole string as one unknown type. The id
//     form's refusal is the loud one, which is the direction to prefer, but
//     neither is a filter.)
//
//   - IT VALIDATES. ?changed_object_type_id=99999 is HTTP 400 "Select a valid
//     choice."; the name form answers dcim.nosuchmodel — and dcim.devices, the
//     plural — with HTTP 200 and count=0, indistinguishable from a quiet window.
//     We never actually send an unknown id, because resolving the name against
//     the map fails first; the 400 is the guarantee behind that check, not the
//     mechanism. An unknown type is returned as an error, NOT logged and
//     swallowed: Changes returns ([]Change, error) and the plugin layer turns
//     that into a visible query error. Do not soften this back into a warning —
//     a wrong type name that renders as "no changes" is the whole defect.
//
// If the map cannot be read at all (network blip, or a token whose permissions
// exclude the endpoint) this DEGRADES to the pre-id behaviour rather than
// failing the annotation: one type still goes upstream by name, several types
// are left entirely to Changes's local filter, and an unresolvable name cannot
// be detected so it is not reported. That is strictly the behaviour of the
// commit before this one — worse than the id path, better than no annotations.
// The one failure NOT degraded is the caller's own expired context, which is not
// a statement about the instance at all.
//
// wanted must already be normalised by normalizeObjectTypes — the caller does
// that so the local filter can be built from the identical list. An empty list
// therefore means "no filter", and an empty parameter is IGNORED upstream
// (measured: it returns the unfiltered count), so an unfiltered annotation query
// sends nothing at all rather than a parameter that means nothing — and must not
// buy the object-type map to do it.
func (p *Provider) pushChangedObjectType(ctx context.Context, q url.Values, wanted []string) error {
	if len(wanted) == 0 {
		return nil
	}

	ids, err := p.objectTypeIDMap(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// Not an upstream problem: this caller's own deadline ran out (see
			// objectTypeIDMap — a waiter never spends more than its own budget).
			// Degrading here would send a change-log request on a dead context and
			// report a WARN about an instance that may be perfectly healthy.
			return err
		}
		log.DefaultLogger.Warn("netbox: could not resolve NetBox object-type ids; the annotation's type filter is only partly pushed upstream",
			"error", logSafe(err.Error()))
		if len(wanted) == 1 {
			q.Add(changedObjectTypeParam, wanted[0])
		}
		return nil
	}

	for _, t := range wanted {
		id, ok := ids[t]
		if !ok {
			// logSafe because the type is free text from the annotation editor and
			// this string reaches both a log record and the user's screen.
			return &UnknownObjectTypeError{Type: logSafe(t), Known: len(ids)}
		}
		// Add, not Set: the repeated parameter is the OR, measured on both 4.4.10
		// and 4.6.4. Collapsing these into one comma-joined value is HTTP 400
		// ("“6,86” is not a valid value.", measured on 4.4.10), so it would not
		// merely lose the filter — it would fail the annotation outright.
		q.Add(changedObjectTypeIDParam, strconv.Itoa(id))
	}
	return nil
}

// UnknownObjectTypeError reports an annotation object-type filter that this
// NetBox instance does not know — a typo in the annotation editor, or a model
// that lives in a plugin the instance does not have installed.
//
// It is a TYPE and not a fmt.Errorf because the plugin layer classifies upstream
// failures by matching error types (queryErrorMessage matches *APIError the same
// way). An untyped error there falls through every case into the transport
// message, so a mistyped object type was reported as "Couldn't reach NetBox" —
// a user-input mistake dressed up as an outage, pointing the reader at the
// network instead of at the field they have to fix.
//
// Type is logSafe'd where the error is constructed: it is free text from the
// editor and this string reaches both a log record and the user's screen.
type UnknownObjectTypeError struct {
	Type  string // the normalised, log-safe type the user asked for
	Known int    // how many types the instance reported
}

// Classification reports this as the user's input rather than an upstream
// failure, so the plugin layer answers it as a bad request. It carries the type
// and the known-type count because the message is only actionable with them.
func (e *UnknownObjectTypeError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{
		Kind:       provider.ErrorKindUnknownObjectType,
		ObjectType: e.Type,
		KnownTypes: e.Known,
	}
}

func (e *UnknownObjectTypeError) Error() string {
	return fmt.Sprintf("unknown NetBox object type %q: not one of the %d types this instance reports at /api/%s/ (expected app_label.model, e.g. dcim.device)",
		e.Type, e.Known, objectTypesEndpoint)
}

// objectTypeIDMap returns app_label.model -> object-type id, cached for cacheTTL.
//
// The map changes only when the set of installed models changes (a NetBox
// upgrade or a plugin install), which would argue for the much longer schemaTTL.
// It follows the shorter discovery TTL instead because a stale entry here is not
// merely stale: a model added since the fetch resolves to nothing, and
// pushChangedObjectType turns that into a hard error rather than a slightly old
// answer. Five minutes bounds that window; the fetch is one request for 154 rows.
//
// One request per TTL for SIMULTANEOUS callers too. Checking the cache and then
// fetching is not enough on its own: a dashboard refresh opens every annotation
// query at the same instant, they all miss together, and each walks the same
// 93 KB list. So a miss joins the read already running instead of starting its
// own, and every caller — leader and waiters alike — takes whatever that one
// read returns.
//
// Two things follow from that, and both are about a read that goes WRONG:
//
//   - A waiter waits on ITS OWN context. The wait is a select over ctx.Done()
//     and the flight's done channel, never a mutex, because a mutex cannot be
//     told about a deadline. Queueing on one made a caller with a 20ms deadline
//     return only when the leader's fetch finished, 2.0s later; scale that to
//     Grafana's real 30s timeout and a hanging endpoint and the last of eight
//     annotation queries waits eight timeouts for a request it never made. A
//     caller may only ever spend its own budget.
//
//   - A FAILED read is SHARED, not re-attempted. Waiters take the leader's error,
//     and it is remembered for objectTypeFailureTTL so the callers just behind
//     them take it too without a request. Letting each waiter retry in turn —
//     which is what not caching the failure meant — turned one broken endpoint
//     into a serial retry storm: 8 simultaneous Changes calls behind a 403 held
//     200ms took 1.62s and issued 8 fetches, where all 8 should have failed
//     together in ~200ms. The window is short precisely because the failure is
//     an outage and not a fact (see objectTypeFailureTTL): a transient 503 costs
//     seconds of the degraded path, never the process's lifetime.
//
// Both numbers are the two tests, not anecdotes: TestChanges_-
// WaiterDoesNotOutliveItsOwnDeadline and _SimultaneousMissesShareAFailedFetch
// fail with exactly those measurements if either half is undone.
//
// The read itself is detached from the caller that started it (WithoutCancel,
// bounded by the datasource's own timeout) so that the leader going away does
// not cancel a read its waiters are still using, and so that the result still
// reaches the cache when every caller has given up — the next refresh finds it
// there. It cannot outlive that bound, so a hanging endpoint cannot pin the
// flight, and only one such read exists at a time.
//
// No ?fields= projection: measured on the demo, /api/core/object-types/ ignores
// it — 93,251 bytes of response with and without — so sending it would be a
// parameter that does nothing.
func (p *Provider) objectTypeIDMap(ctx context.Context) (map[string]int, error) {
	ids, flight, err := p.objectTypeIDsOrFlight(ctx)
	if flight == nil {
		return ids, err
	}
	select {
	case <-ctx.Done():
		// THIS caller's deadline, not the leader's. The read carries on for
		// whoever else is waiting, and its result still lands in the cache.
		return nil, ctx.Err()
	case <-flight.done:
		return flight.ids, flight.err
	}
}

// objectTypeIDsOrFlight answers a caller from the cache when it can — the map,
// or a recent failure — and otherwise hands back the read to wait on, joining
// the one already running or starting it.
//
// Exactly one of the three results is meaningful, and a non-nil flight is what
// says "nothing is decided yet". mu is held for the whole decision so that two
// simultaneous misses cannot both start a read, and released before the read
// begins: mu guards short map reads all over the provider and must never be held
// across an HTTP request.
func (p *Provider) objectTypeIDsOrFlight(ctx context.Context) (map[string]int, *objectTypeFlight, error) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.objectTypeIDs != nil && now.Before(p.objectTypeIDsExpiry) {
		return p.objectTypeIDs, nil, nil
	}
	if p.objectTypeErr != nil && now.Before(p.objectTypeErrExpiry) {
		return nil, nil, p.objectTypeErr
	}
	if p.objectTypeFlight != nil {
		return nil, p.objectTypeFlight, nil
	}
	flight := &objectTypeFlight{done: make(chan struct{})}
	p.objectTypeFlight = flight
	go p.runObjectTypeFetch(ctx, flight)
	return nil, flight, nil
}

// runObjectTypeFetch performs one read of the object-type map, publishes it to
// the cache (positively or negatively) and releases everyone waiting on it.
//
// ctx is the STARTER'S context and is used only for its values — the branch
// header rides on it — never for its cancellation: see objectTypeIDMap for why
// the read is detached, and objectTypeFetchBudget for what bounds it instead.
func (p *Provider) runObjectTypeFetch(ctx context.Context, flight *objectTypeFlight) {
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.objectTypeFetchBudget())
	defer cancel()

	ids, err := p.readObjectTypeIDs(fetchCtx)

	p.mu.Lock()
	if err == nil {
		p.objectTypeIDs, p.objectTypeIDsExpiry = ids, time.Now().Add(cacheTTL)
		p.objectTypeErr, p.objectTypeErrExpiry = nil, time.Time{}
	} else {
		p.objectTypeErr, p.objectTypeErrExpiry = err, time.Now().Add(objectTypeFailureTTL)
	}
	// Retire the flight under the SAME lock that published the outcome, so a
	// caller arriving now sees one or the other and never a gap between them.
	p.objectTypeFlight = nil
	p.mu.Unlock()

	flight.ids, flight.err = ids, err
	close(flight.done)
}

// objectTypeFetchBudget bounds the detached read, so that a hanging endpoint
// cannot pin the flight — and with it the failure window — indefinitely.
//
// The datasource's configured Timeout is the right bound because it is the
// user's own statement of how long this datasource may take, and because it is
// the same order as what the walk had before it was detached: it inherited the
// annotation query's context, which carries Grafana's query timeout. What
// changes is WHO waits, not how long the instance is given to answer.
//
// It bounds the whole walk rather than one request (the client enforces the
// per-request timeout of its own). That is only a real cap on an instance
// holding thousands of models — maxObjectTypes / pageSize is 10 pages, and the
// demo answers all 154 rows in one.
func (p *Provider) objectTypeFetchBudget() time.Duration {
	if p.requestTimeout > 0 {
		return p.requestTimeout
	}
	return defaultRequestTimeout
}

// readObjectTypeIDs walks /api/core/object-types/ and builds app_label.model ->
// id. Every failure it returns is one the caller may degrade on.
func (p *Provider) readObjectTypeIDs(ctx context.Context) (map[string]int, error) {
	rows, _, err := p.fetchRows(ctx, objectTypesEndpoint, url.Values{}, maxObjectTypes)
	if err != nil {
		return nil, fmt.Errorf("fetch object types: %w", err)
	}
	ids := make(map[string]int, len(rows))
	for _, raw := range rows {
		var ot struct {
			ID       int    `json:"id"`
			AppLabel string `json:"app_label"`
			Model    string `json:"model"`
		}
		if err := json.Unmarshal(raw, &ot); err != nil || ot.AppLabel == "" || ot.Model == "" {
			continue
		}
		ids[normalizeObjectType(ot.AppLabel+"."+ot.Model)] = ot.ID
	}
	// An empty map would make every type unknown, so treat it as a failed read and
	// let the caller degrade instead of reporting the user's correct type as wrong.
	if len(ids) == 0 {
		return nil, fmt.Errorf("fetch object types: %s returned no usable rows", objectTypesEndpoint)
	}
	return ids, nil
}

// allNullCustomFieldColumns returns the cf_* columns among columns that hold
// nil in every row — the only columns a type declaration can matter for.
func allNullCustomFieldColumns(columns []string, rows []map[string]interface{}) []string {
	var out []string
	for _, c := range columns {
		if !strings.HasPrefix(c, cfPrefix) {
			continue
		}
		allNull := true
		for _, r := range rows {
			if v, ok := r[c]; ok && v != nil {
				allNull = false
				break
			}
		}
		if allNull {
			out = append(out, c)
		}
	}
	return out
}

func projectColumns(have, want []string) []string {
	set := map[string]bool{}
	for _, c := range have {
		set[c] = true
	}
	var out []string
	for _, w := range want {
		if set[w] {
			out = append(out, w)
		}
	}
	return out
}

// inferType maps a column name + sample value to a logical field type.
func inferType(name string, v interface{}) provider.FieldType {
	switch v.(type) {
	case bool:
		return provider.FieldTypeBoolean
	case float64:
		return provider.FieldTypeNumber
	}
	if isTimeColumn(name) {
		if s, ok := v.(string); ok {
			if _, err := time.Parse(time.RFC3339, s); err == nil {
				return provider.FieldTypeTime
			}
		}
	}
	return provider.FieldTypeString
}

func isTimeColumn(name string) bool {
	switch name {
	case "created", "last_updated", "last_used", "time", "expires":
		return true
	}
	return false
}

// humanize turns a slug like "device-roles" into "Device Roles".
func humanize(s string) string {
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	words := strings.Fields(s)
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
