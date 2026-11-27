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
// own namesWait. The mutex guards reading and charging left; it is never held
// across a wait. spentLogged makes the "budget used up" line once per call.
type schemaBudget struct {
	mu          sync.Mutex
	left        time.Duration
	spentLogged bool
}

// withSchemaBudget gives a Changes call its schema budget.
func (p *Provider) withSchemaBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, schemaBudgetKey{}, &schemaBudget{left: p.namesWait()})
}

// budgetedSchema is p.schema waited on within what the call's schema budget has
// left (also bounded by ctx), charging the wait to the budget. A cache hit
// answers at once either way. Changes installs the budget; a caller without
// one waits at most one namesWait.
func (p *Provider) budgetedSchema(ctx context.Context) (schemaCacheEntry, error) {
	b, _ := ctx.Value(schemaBudgetKey{}).(*schemaBudget)
	if b == nil {
		waitCtx, cancel := context.WithTimeout(ctx, p.namesWait())
		defer cancel()
		return p.schema(waitCtx)
	}
	b.mu.Lock()
	left := b.left
	b.mu.Unlock()
	if left <= 0 {
		logSchemaBudgetSpent(b)
	}
	start := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, left)
	defer cancel()
	entry, err := p.schema(waitCtx)
	b.mu.Lock()
	b.left -= time.Since(start)
	b.mu.Unlock()
	if err != nil && waitCtx.Err() != nil && ctx.Err() == nil {
		logSchemaBudgetSpent(b)
	}
	return entry, err
}

// logSchemaBudgetSpent says, once per call, why the related objects named
// through the schema stay ids.
func logSchemaBudgetSpent(b *schemaBudget) {
	b.mu.Lock()
	first := !b.spentLogged
	b.spentLogged = true
	b.mu.Unlock()
	if first {
		log.DefaultLogger.Debug("netbox: the call's schema wait budget is used up; related objects named through the schema stay ids (generic relations are still named)")
	}
}

// resolveDiffs builds the provider rows, with each diff in display order and the
// related-object ids in its rendered fields replaced by the objects' names (a
// generic relation's type by its app_label.model).
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
		out = append(out, d.toChange(base, func(field string, v json.RawMessage, after bool) string {
			if name, ok := names.name(fields, d.snapshot(after), field, v); ok {
				return name
			}
			return formatValue(v)
		}))
	}
	return out
}

// diffFieldMatches reports whether a diff field is one of the Changed-fields
// names: the name itself (cf_<name> for a custom field), or any custom field
// for custom_fields. A bare name is never a custom field: checkChangedFields
// refuses one no object type has when it can read the schema, so matching it
// to cf_<name> too would make the same query a refusal or a track depending on
// whether the schema answered in time.
func diffFieldMatches(field string, names []string) bool {
	for _, n := range names {
		if field == n || (n == customFieldsKey && strings.HasPrefix(field, cfPrefix)) {
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

// genericPairType reads field as the id half of a generic relation in one
// side's snapshot data: field is <x>_id, and data holds <x>_type as a positive
// plain integer (snapshotID), the object-type id of what <x>_id points at. A
// custom field is never one.
func genericPairType(data map[string]json.RawMessage, field string) (int64, bool) {
	x, ok := strings.CutSuffix(field, "_id")
	if !ok || x == "" || strings.HasPrefix(field, cfPrefix) {
		return 0, false
	}
	return snapshotID(data, x+"_type")
}

// genericTypeField reports whether field is the type half of a generic relation
// in one side's snapshot data: <x>_type with <x>_id beside it. A _type field
// with no _id beside it is an ordinary field.
func genericTypeField(data map[string]json.RawMessage, field string) bool {
	x, ok := strings.CutSuffix(field, "_type")
	if !ok || x == "" || strings.HasPrefix(field, cfPrefix) {
		return false
	}
	_, ok = data[x+"_id"]
	return ok
}

// snapshot is the row's snapshot on one side of the change: the prechange data
// for a before value, the postchange data for an after value.
func (d decodedChange) snapshot(after bool) map[string]json.RawMessage {
	if after {
		return d.oc.PostchangeData
	}
	return d.oc.PrechangeData
}

// value is the field's before or after value.
func (f rawFieldChange) value(after bool) json.RawMessage {
	if after {
		return f.After
	}
	return f.Before
}

// nameEntry is what a brief lookup names an object by: its display, and its
// parent's, the device or virtual machine its brief row nests ("" when it nests
// neither). A plain foreign key shows the display alone. A generic relation
// shows "display (parent)" (label): its field says neither what kind of object
// it points at nor whose, and an interface's display is only its own name. A
// scope object is named the same way, for the same reason.
type nameEntry struct{ display, parent string }

// label is the display, with the parent's in brackets when there is one:
// "eth0 (leaf1)". No display is no label.
func (e nameEntry) label() string {
	if e.display == "" || e.parent == "" {
		return e.display
	}
	return e.display + " (" + e.parent + ")"
}

// diffNames is what displayNames found: which fields of which changed types are
// plain foreign keys, the object types and endpoints generic relations point
// at, and the names of the related objects it looked up.
type diffNames struct {
	related    map[string]map[string]string   // changed type -> field -> related list endpoint
	types      map[int64]string               // object-type id -> app_label.model
	generic    map[string]string              // app_label.model -> list endpoint, for generic relations
	byEndpoint map[string]map[int64]nameEntry // related list endpoint -> id -> name
}

// lookup is where one side's value of a diff field is looked up by id: the list
// endpoint, and whether the field is a generic relation's id. data is that
// side's snapshot, and fields the changed type's plain foreign keys. ok is false
// when the value is not looked up. A generic relation's type never is: the
// object-type map already names it.
func (n diffNames) lookup(fields map[string]string, data map[string]json.RawMessage, field string) (endpoint string, generic, ok bool) {
	if typeID, pair := genericPairType(data, field); pair {
		endpoint, ok = n.generic[n.types[typeID]]
		return endpoint, true, ok
	}
	if genericTypeField(data, field) {
		return "", false, false
	}
	endpoint, ok = fields[field]
	return endpoint, false, ok
}

// name renders one side's value of a diff field by name: a related object's id
// as its display, with its parent's for a generic relation, and a generic
// relation's type as its app_label.model. ok is false when there is no name,
// and the value shows as it is.
func (n diffNames) name(fields map[string]string, data map[string]json.RawMessage, field string, v json.RawMessage) (string, bool) {
	id, ok := relatedID(v)
	if !ok {
		return "", false
	}
	if genericTypeField(data, field) {
		ct, ok := n.types[id]
		return ct, ok
	}
	endpoint, generic, ok := n.lookup(fields, data, field)
	if !ok {
		return "", false
	}
	e, ok := n.byEndpoint[endpoint][id]
	switch {
	case !ok:
		return "", false
	case generic:
		return e.label(), true
	}
	return e.display, true
}

// displayNames looks up the objects behind the ids in rows' rendered diff fields.
//
// A plain foreign key is known from the cached OpenAPI schema's dimension index
// (dimension.go), which still answers for deleted objects. A generic relation
// (assigned_object_id beside assigned_object_type) is known from the row's own
// snapshot on that side (genericPairType), and the object type it points at
// from the cached object-type map, by id. That needs no schema, so a generic
// relation is named even when the schema can't be read or the call's schema
// budget is spent; only the plain foreign keys then stay ids. Both share the
// lookups, one per related endpoint within the same bounds, and the one names
// wait.
//
// A field is only a candidate when it is not a custom field and one of its
// values is an id, so rows with no diff, or with only text, choices and lists in
// their rendered lines, cost no request at all, not even the object-type or
// schema read. Every failure leaves the ids in place.
func (p *Provider) displayNames(ctx context.Context, rows []decodedChange) diffNames {
	names := diffNames{
		related:    map[string]map[string]string{},
		generic:    map[string]string{},
		byEndpoint: map[string]map[int64]nameEntry{},
	}

	var types []string                  // changed types with a plain candidate, in order of first appearance
	candidates := map[string][]string{} // changed type -> plain candidate fields
	var genericTypes []int64            // object types generic relations point at, in order of first appearance
	generic := false                    // a rendered field is half of a generic relation
	for _, d := range rows {
		ct := normalizeObjectType(d.oc.changedType())
		for _, f := range renderedDiff(d.diff) {
			if strings.HasPrefix(f.Field, cfPrefix) {
				continue
			}
			plain := false
			for _, after := range []bool{false, true} {
				data := d.snapshot(after)
				if _, ok := relatedID(f.value(after)); !ok {
					continue
				}
				if typeID, ok := genericPairType(data, f.Field); ok {
					generic = true
					if !slices.Contains(genericTypes, typeID) {
						genericTypes = append(genericTypes, typeID)
					}
				} else if genericTypeField(data, f.Field) {
					generic = true
				} else {
					plain = true
				}
			}
			if !plain || slices.Contains(candidates[ct], f.Field) {
				continue
			}
			if _, ok := candidates[ct]; !ok {
				types = append(types, ct)
			}
			candidates[ct] = append(candidates[ct], f.Field)
		}
	}
	if len(types) == 0 && !generic {
		return names
	}

	discoveryFailed := false
	endpointOf := func(ct string) (string, bool) {
		if discoveryFailed {
			return "", false
		}
		endpoint, found, err := p.contentTypeEndpoint(ctx, ct)
		if err != nil {
			// Stop at the first failure: ObjectTypes does not cache one, so
			// every further type would repeat the failed discovery.
			discoveryFailed = true
			log.DefaultLogger.Debug("netbox: could not find an object type's API endpoint; diffs show ids for it and the types after it",
				"objectType", logSafe(ct), "error", logSafe(err.Error()))
			return "", false
		}
		return endpoint, found
	}

	endpoints := map[string]string{} // changed type -> its list endpoint
	for _, ct := range types {
		if endpoint, found := endpointOf(ct); found {
			endpoints[ct] = endpoint
		}
	}
	if generic {
		// The cached map contentTypeEndpoint reads, so the two share one read.
		typeIDs, err := p.objectTypeIDMap(ctx)
		if err != nil {
			log.DefaultLogger.Debug("netbox: could not read NetBox object-type ids; generic relations in diffs show ids",
				"error", logSafe(err.Error()))
		}
		names.types = make(map[int64]string, len(typeIDs))
		for ct, id := range typeIDs {
			names.types[int64(id)] = ct
		}
		for _, typeID := range genericTypes {
			if ct, ok := names.types[typeID]; ok {
				if endpoint, found := endpointOf(ct); found {
					names.generic[ct] = endpoint
				}
			}
		}
	}
	if len(endpoints) > 0 {
		names.related = p.plainRelations(ctx, endpoints, candidates)
	}

	var order []string          // related endpoints, in order of first appearance
	ids := map[string][]int64{} // related endpoint -> ids to look up
	queued := map[string]map[int64]bool{}
	for _, d := range rows {
		fields := names.related[normalizeObjectType(d.oc.changedType())]
		for _, f := range renderedDiff(d.diff) {
			for _, after := range []bool{false, true} {
				id, ok := relatedID(f.value(after))
				if !ok {
					continue
				}
				endpoint, _, ok := names.lookup(fields, d.snapshot(after), f.Field)
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
	looked := make([]map[int64]nameEntry, len(order))
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

// plainRelations finds which candidate fields of each changed type are plain
// foreign keys, from the schema's dimension index, waited on within the call's
// schema budget (budgetedSchema): changed type -> field -> related list
// endpoint. A schema that can't be read in time finds none.
func (p *Provider) plainRelations(ctx context.Context, endpoints map[string]string, candidates map[string][]string) map[string]map[string]string {
	related := map[string]map[string]string{}
	entry, err := p.budgetedSchema(ctx)
	if err != nil {
		log.DefaultLogger.Debug("netbox: OpenAPI schema unavailable; diffs show plain foreign keys as ids",
			"error", logSafe(err.Error()))
		return related
	}
	for ct, endpoint := range endpoints {
		for _, field := range candidates[ct] {
			// A diff field is a model field, never a column flattening derived,
			// so only an exact property counts (base == field).
			if dim, base, ok := entry.dims.resolve(endpoint, field); ok && base == field && dim.kind == dimRelated {
				if related[ct] == nil {
					related[ct] = map[string]string{}
				}
				related[ct][field] = dim.endpoint
			}
		}
	}
	return related
}

// lookupNames reads the brief form of the objects with these ids from one list
// endpoint, in one request, and returns id -> name: the object's display, and
// the display of the device or virtual machine the brief row nests, as an
// interface's does. A failure is logged and returns nil, so the caller shows
// the objects by id: a diff's related objects, or an annotation scope's.
func (p *Provider) lookupNames(ctx context.Context, endpoint string, ids []int64) map[int64]nameEntry {
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
	out := make(map[int64]nameEntry, len(rows))
	for _, raw := range rows {
		var obj struct {
			ID      int64  `json:"id"`
			Display string `json:"display"`
			// Raw, and read on their own below, so an odd value cannot cost the
			// row its display.
			Device         json.RawMessage `json:"device"`
			VirtualMachine json.RawMessage `json:"virtual_machine"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Display != "" {
			out[obj.ID] = nameEntry{display: obj.Display, parent: parentDisplay(obj.Device, obj.VirtualMachine)}
		}
	}
	return out
}

// parentDisplay is the display of the first of these nested objects that has
// one, or "".
func parentDisplay(nested ...json.RawMessage) string {
	for _, raw := range nested {
		var obj struct {
			Display string `json:"display"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Display != "" {
			return obj.Display
		}
	}
	return ""
}
