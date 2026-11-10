package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// maxNameEndpoints and maxNameIDs bound the name lookups behind one query's
// diffs: one request per related endpoint, at most maxNameEndpoints of them, each
// for at most maxNameIDs ids. Values past either bound show as ids.
const (
	maxNameEndpoints = 10
	maxNameIDs       = 100
)

// namesWaitBudget is how long a query waits for the names behind its diffs
// before answering with ids. The names are a hint, as the custom-field index is
// (customFieldsWaitBudget, the same size): a cold, slow or failing schema must
// not hold the annotation query, which already has its rows. The schema and
// object-type reads are detached flights, so they carry on past the budget and
// fill their caches for the next refresh.
const namesWaitBudget = 2 * time.Second

// namesWait is namesWaitBudget unless a test has shortened it.
func (p *Provider) namesWait() time.Duration {
	if p.namesWaitOverride > 0 {
		return p.namesWaitOverride
	}
	return namesWaitBudget
}

// schemaBudgetKey carries a Changes call's schema budget on its context.
type schemaBudgetKey struct{}

// schemaBudget is the one namesWait a Changes call has for waiting on the
// cached OpenAPI schema, across every read that consults it: the scope lookup's
// name-filter check, the Changed-fields check and the diffs' relation lookup.
// The schema is one shared flight, so on a cold or refetching schema those
// waits would otherwise stack, one namesWait each. Each wait is charged the time
// it took; once the budget is spent the later waits give up at once and their
// reads degrade, as past their own wait. Only schema waiting draws on it: not
// the change-log reads, and not the network name reads, which each wait their
// own namesWait. The schema waits all run on the Changes goroutine, so it needs
// no lock.
type schemaBudget struct {
	left time.Duration
}

// withSchemaBudget gives a Changes call its schema budget.
func (p *Provider) withSchemaBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, schemaBudgetKey{}, &schemaBudget{left: p.namesWait()})
}

// budgetedSchema is p.schema waited on within what the call's schema budget has
// left (also bounded by ctx), charging the wait to the budget. A cache hit
// answers at once either way. Changes installs the budget; a caller without
// one waits on ctx alone.
func (p *Provider) budgetedSchema(ctx context.Context) (schemaCacheEntry, error) {
	b, _ := ctx.Value(schemaBudgetKey{}).(*schemaBudget)
	if b == nil {
		return p.schema(ctx)
	}
	start := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, b.left)
	defer cancel()
	entry, err := p.schema(waitCtx)
	b.left -= time.Since(start)
	return entry, err
}

// resolveDiffs builds the provider rows, with each diff in display order and the
// related-object ids in its rendered fields replaced by the objects' names.
//
// rows are newest first, as Changes reads them, so when a lookup budget runs out
// it is the older changes that keep their ids. preferred holds the Changed-fields
// names: the fields they match (diffFieldMatches) sort first, and the rest
// follow alphabetically. Everything the names wait on shares one namesWait,
// past which the values stay ids; the schema wait within it is further bounded
// by the call's schema budget (budgetedSchema).
func (p *Provider) resolveDiffs(ctx context.Context, rows []decodedChange, preferred []string) []provider.Change {
	ordered := make([]decodedChange, len(rows))
	for i, d := range rows {
		d.diff = orderDiff(d.diff, preferred)
		ordered[i] = d
	}
	waitCtx, cancel := context.WithTimeout(ctx, p.namesWait())
	names := p.displayNames(waitCtx, ordered)
	if waitCtx.Err() != nil && ctx.Err() == nil {
		log.DefaultLogger.Debug("netbox: related-object names not resolved within the wait budget; the diffs show ids",
			"waited", p.namesWait().String())
	}
	cancel()
	base := p.client.BaseURL()
	var out []provider.Change
	for _, d := range ordered {
		fields := names.related[normalizeObjectType(d.oc.changedType())]
		out = append(out, d.toChange(base, func(field string, v json.RawMessage) string {
			if id, ok := relatedID(v); ok {
				if name, ok := names.byEndpoint[fields[field]][id]; ok {
					return name
				}
			}
			return formatValue(v)
		}))
	}
	return out
}

// diffFieldMatches reports whether a diff field is one of the Changed-fields
// names: the name itself, cf_<name> for a custom field named without its
// prefix, or any custom field for custom_fields.
func diffFieldMatches(field string, names []string) bool {
	for _, n := range names {
		if field == n || field == cfPrefix+n || (n == customFieldsKey && strings.HasPrefix(field, cfPrefix)) {
			return true
		}
	}
	return false
}

// orderDiff returns a copy of diff in display order: the fields preferred
// matches first, then the rest, each group alphabetically.
func orderDiff(diff []rawFieldChange, preferred []string) []rawFieldChange {
	out := slices.Clone(diff)
	slices.SortFunc(out, func(a, b rawFieldChange) int {
		if ma, mb := diffFieldMatches(a.Field, preferred), diffFieldMatches(b.Field, preferred); ma != mb {
			if ma {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Field, b.Field)
	})
	return out
}

// renderedDiff is the part of an ordered diff a marker shows
// (provider.MaxDiffLines). Only those fields get names looked up.
func renderedDiff(diff []rawFieldChange) []rawFieldChange {
	return diff[:min(len(diff), provider.MaxDiffLines)]
}

// relatedID reads a diff value as a related object's id. The change log stores a
// foreign key as its integer id; a list of ids (many-to-many) is not one.
func relatedID(v json.RawMessage) (int64, bool) {
	id, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
	return id, err == nil && id > 0
}

// diffNames is what displayNames found: which fields of which changed types are
// relations, and the names of the related objects it looked up.
type diffNames struct {
	related    map[string]map[string]string // changed type -> field -> related list endpoint
	byEndpoint map[string]map[int64]string  // related list endpoint -> id -> display
}

// displayNames looks up the objects behind the ids in rows' rendered diff fields.
//
// Which fields are relations comes from the cached OpenAPI schema's dimension
// index (dimension.go), which still answers for deleted objects. A field is only
// a candidate when it is not a custom field and one of its values is an id, so
// rows with no diff, or with only text, choices and lists in their rendered
// lines, cost no request at all, not even the object-type or schema read.
//
// Every failure leaves the ids in place. Generic relations (assigned_object_id
// beside assigned_object_type) are not dimRelated, so they stay ids too.
func (p *Provider) displayNames(ctx context.Context, rows []decodedChange) diffNames {
	names := diffNames{related: map[string]map[string]string{}, byEndpoint: map[string]map[int64]string{}}

	var types []string                  // changed types with a candidate, in order of first appearance
	candidates := map[string][]string{} // changed type -> candidate fields
	for _, d := range rows {
		ct := normalizeObjectType(d.oc.changedType())
		for _, f := range renderedDiff(d.diff) {
			if strings.HasPrefix(f.Field, cfPrefix) || slices.Contains(candidates[ct], f.Field) {
				continue
			}
			if _, ok := relatedID(f.Before); !ok {
				if _, ok := relatedID(f.After); !ok {
					continue
				}
			}
			if _, ok := candidates[ct]; !ok {
				types = append(types, ct)
			}
			candidates[ct] = append(candidates[ct], f.Field)
		}
	}
	if len(types) == 0 {
		return names
	}

	endpoints := map[string]string{} // changed type -> its list endpoint
	for _, ct := range types {
		endpoint, found, err := p.contentTypeEndpoint(ctx, ct)
		if err != nil {
			// Stop at the first failure: ObjectTypes does not cache one, so
			// every further type would repeat the failed discovery.
			log.DefaultLogger.Debug("netbox: could not find a changed type's API endpoint; its diff shows ids",
				"objectType", logSafe(ct), "error", logSafe(err.Error()))
			break
		}
		if found {
			endpoints[ct] = endpoint
		}
	}
	if len(endpoints) == 0 {
		return names
	}
	entry, err := p.budgetedSchema(ctx)
	if err != nil || entry.dims == nil {
		if err != nil {
			log.DefaultLogger.Debug("netbox: OpenAPI schema unavailable; diffs show related objects as ids",
				"error", logSafe(err.Error()))
		}
		return names
	}
	for ct, endpoint := range endpoints {
		for _, field := range candidates[ct] {
			// A diff field is a model field, never a column flattening derived,
			// so only an exact property counts (base == field).
			if dim, base, ok := entry.dims.resolve(endpoint, field); ok && base == field && dim.kind == dimRelated {
				if names.related[ct] == nil {
					names.related[ct] = map[string]string{}
				}
				names.related[ct][field] = dim.endpoint
			}
		}
	}

	var order []string          // related endpoints, in order of first appearance
	ids := map[string][]int64{} // related endpoint -> ids to look up
	queued := map[string]map[int64]bool{}
	for _, d := range rows {
		fields := names.related[normalizeObjectType(d.oc.changedType())]
		for _, f := range renderedDiff(d.diff) {
			endpoint, ok := fields[f.Field]
			if !ok {
				continue
			}
			for _, v := range []json.RawMessage{f.Before, f.After} {
				id, ok := relatedID(v)
				if !ok {
					continue
				}
				if queued[endpoint] == nil {
					if len(order) == maxNameEndpoints {
						continue
					}
					order = append(order, endpoint)
					queued[endpoint] = map[int64]bool{}
				}
				if queued[endpoint][id] || len(ids[endpoint]) == maxNameIDs {
					continue
				}
				queued[endpoint][id] = true
				ids[endpoint] = append(ids[endpoint], id)
			}
		}
	}

	// Index-aligned, so each lookup owns its slot and no lock is needed.
	looked := make([]map[int64]string, len(order))
	var wg sync.WaitGroup
	for i, endpoint := range order {
		wg.Go(func() { looked[i] = p.lookupNames(ctx, endpoint, ids[endpoint]) })
	}
	wg.Wait()
	for i, endpoint := range order {
		names.byEndpoint[endpoint] = looked[i]
	}
	return names
}

// lookupNames reads the brief form of the objects with these ids from one list
// endpoint, in one request, and returns id -> display. A failure is logged and
// returns nil, so the caller shows the objects by id: a diff's related objects,
// or an annotation scope's.
func (p *Provider) lookupNames(ctx context.Context, endpoint string, ids []int64) map[int64]string {
	q := url.Values{"brief": {"true"}}
	for _, id := range ids {
		q.Add("id", strconv.FormatInt(id, 10))
	}
	rows, _, err := p.fetchRows(ctx, endpoint, q, len(ids))
	if err != nil {
		log.DefaultLogger.Debug("netbox: object name lookup by id failed; the objects show as ids",
			"endpoint", logSafe(endpoint), "ids", len(ids), "error", logSafe(err.Error()))
		return nil
	}
	out := make(map[int64]string, len(rows))
	for _, raw := range rows {
		var obj struct {
			ID      int64  `json:"id"`
			Display string `json:"display"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Display != "" {
			out[obj.ID] = obj.Display
		}
	}
	return out
}
