package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// FK resolution turns replica-cache's flat foreign keys into the shape the
// NetBox provider emits.
//
// replica-cache stores rows as they sit in the database: a device carries
// site_id 4001 and nothing else. The NetBox REST API instead nests the related
// object, and our NetBox provider flattens that into three columns — site (the
// display name), site_id and site_slug. Dashboards, join keys, alert rules and
// the documented recipes are all written against those names.
//
// So this is not a nicety. Without it, switching Mode changes every column a
// panel selects, and a query that groups by "site" silently returns nothing.
// The whole point of the second provider is that the panels do not have to know.

// fkCacheTTL bounds how long a resolved dimension row is reused. Dimension
// tables are the slowest-changing data in NetBox — sites, roles, tenants — and
// re-fetching them per query would dominate the cost of every panel refresh.
const fkCacheTTL = 5 * time.Minute

// fkBatchSize bounds how many ids go into one `in` filter. The value keeps the
// query string comfortably inside normal proxy limits while still resolving a
// full page of rows in a handful of requests.
const fkBatchSize = 200

// polymorphicFKs are id columns whose target is decided by a companion
// content-type column rather than by the column name.
//
// They are left unresolved on purpose. The service exposes no content-type
// table — /v1/extras/content-types and /v1/extras/object-types both answer
// "endpoint not found" — so the type id cannot be turned into a model name
// through the API at all. Hardcoding the mapping is not an option either: a
// content-type id is assigned per NetBox instance, so a constant that happens
// to be right for one deployment is wrong for the next, and would resolve an
// IP's parent to a confidently incorrect object.
var polymorphicFKs = map[string]bool{
	"assigned_object": true,
	"scope":           true,
	"parent_object":   true,
}

// notFKs are columns whose names end in _id but which are not foreign keys at
// all: NetBox CharFields holding somebody else's identifier, like a rack's
// facility id or a provider network's service id.
//
// Every entry was found rather than guessed — the NetBox 4.4.10 schema was
// searched for properties of type string whose name ends in _id, and this is
// all seven, paired with the model each belongs to. Value-type evidence catches
// most of them (a string is not a key), but not when the column is null on
// every row of a page: there is nothing to classify, so the column read as an
// all-null RELATIONSHIP and gained a fabricated derived column that Fields then
// advertised to the editor.
//
// Keyed by ENTITY, not by bare name. Several of these words would resolve by
// convention if the value ever were numeric — "service" finds ipam/services —
// so the exclusion has to be by name rather than by inference; but the fact
// being recorded is about a specific NetBox model, and applying it globally
// would suppress a legitimate plugins/acme/widgets.service_id pointing at
// plugins/acme/services on the strength of what a core circuits model does.
var notFKs = map[string]bool{
	"dcim/racks.facility":                    true,
	"core/jobs.job":                          true,
	"dcim/inventory-items.part":              true,
	"dcim/inventory-item-templates.part":     true,
	"core/object-changes.request":            true,
	"extras/object-changes.request":          true,
	"plugins/branching/branches.schema":      true,
	"circuits/provider-networks.service":     true,
	"circuits/circuit-terminations.xconnect": true,
}

// notAKey reports whether this entity's column is one of the text identifiers
// above.
func notAKey(entity, base string) bool { return notFKs[entity+"."+base] }

// unexposedFKs point at tables replica-cache does not replicate at all (users,
// config templates). Nothing can resolve them, so the raw id stays.
var unexposedFKs = map[string]bool{
	"owner":           true,
	"config_template": true,
	"created_by":      true,
	"last_updated_by": true,
}

// namedFKs are relationships convention cannot derive, because the column is
// named for its ROLE rather than for its target: an interface's untagged_vlan
// is a vlan, a device's primary_ip4 an ip-address, a device-type's
// default_platform a platform.
//
// A key of "app/model.column" applies to that entity only; a bare "column"
// applies wherever the column appears. Every entry was measured: fkTarget was
// run over every foreign-key column on a live instance and its answer compared
// against the target NetBox's own schema gives, and these are the pairs where
// the derivation could not reach a table this deployment actually serves.
//
// The qualified entry is the one that matters most. A virtual machine's role is
// a dcim device-role, which no candidate finds, and the global-basename
// fallback then settled on the only "roles" model in the list — ipam/roles —
// rendering VM roles with unrelated IPAM names wherever the ids overlapped.
// That is the confidently-wrong-name failure the fallback is otherwise careful
// to avoid, and it is why an override is checked before any derivation.
var namedFKs = map[string]string{
	"virtualization/virtual-machines.role": "dcim/device-roles",

	// A wireless link's two ends are interfaces, but nothing in "interface_a"
	// derives dcim/interfaces: the candidates look for wireless/interface-as.
	// Qualified rather than bare, because the _a/_b suffix is a convention for
	// "the two ends of a thing", and what those ends ARE is the model's answer,
	// not the column name's.
	"wireless/wireless-links.interface_a": "dcim/interfaces",
	"wireless/wireless-links.interface_b": "dcim/interfaces",

	"primary_ip":  "ipam/ip-addresses",
	"primary_ip4": "ipam/ip-addresses",
	"primary_ip6": "ipam/ip-addresses",
	"oob_ip":      "ipam/ip-addresses",
	"nat_inside":  "ipam/ip-addresses",

	"untagged_vlan":       "ipam/vlans",
	"qinq_svlan":          "ipam/vlans",
	"primary_mac_address": "dcim/mac-addresses",
	"default_platform":    "dcim/platforms",
}

// selfFKs are the columns whose target is the table they appear in. NetBox's
// self-referential relationships are named for the role the OTHER row plays —
// an interface's lag is the port-channel it belongs to, its bridge the bridge
// it is a member of — so the derivation looks for "dcim/lags" or
// "dcim/bridges", which no deployment has, and the column stayed a bare id
// while NetBox mode resolved it to a name.
//
// NetBox's polymorphic parents are different columns entirely (parent_object,
// excluded above), so none of these can point anywhere but home.
var selfFKs = map[string]bool{
	"parent": true,
	"lag":    true,
	"bridge": true,
}

// fkTarget decides which entity a foreign-key column points at.
//
// The mapping is derived rather than tabulated, and every candidate is checked
// against the entities this deployment actually reports. That matters because
// the same column name means different things in different apps: role_id on a
// device is a dcim device-role, on a rack a rack-role, and on a prefix the
// shared ipam role. A hand-written table would have to enumerate all of those
// and would silently rot as models are added; asking "does dcim/device-roles
// exist here?" answers it from data, and a wrong guess simply fails to match.
//
// Candidates are tried most specific first:
//
//	role_id on dcim/devices  -> dcim/device-roles  (qualified by the owner)
//	role_id on ipam/prefixes -> ipam/roles         (plain, same app)
//	tenant_id on dcim/devices -> tenancy/tenants   (unique across apps)
func fkTarget(entity, base string, known map[string]bool) (string, bool) {
	// Polymorphic stays global. assigned_object, scope and parent_object name a
	// NetBox-wide convention — the target is decided by a companion
	// content-type column — and a plugin using one of those exact names is
	// following that convention, so resolving it against a same-app model that
	// happened to match would be the confidently-wrong-name failure this file
	// works to avoid.
	if polymorphicFKs[base] {
		return "", false
	}
	if notAKey(entity, base) {
		return "", false
	}
	// Unexposed is different, and applies to CORE models only. owner,
	// created_by and last_updated_by are mixin fields pointing at users, and
	// config_template at a table this service does not replicate — facts about
	// NetBox, not about an arbitrary plugin, which may perfectly well have its
	// own owners collection. Qualifying these by entity is not an option the way
	// it was for the text identifiers: they appear on dozens of core models and
	// the list would rot, so the split is core versus plugin.
	//
	// Safe because a plugin gets no global-basename fallback: only a same-app or
	// owner-qualified candidate can match, which is evidence rather than
	// coincidence. A plugin whose owner_id really does point at users finds no
	// plugins/acme/owners and stays an id, as it should.
	if unexposedFKs[base] && !strings.HasPrefix(entity, "plugins/") {
		return "", false
	}
	// Overrides win over every derivation below, including the global-basename
	// fallback that would otherwise answer some of them incorrectly. Still gated
	// on the entity list: a deployment not serving the target degrades to the
	// raw id rather than to the name of something it cannot read.
	if e, ok := namedFKs[entity+"."+base]; ok {
		return e, known[e]
	}
	if e, ok := namedFKs[base]; ok {
		return e, known[e]
	}
	// Not for a plugin's models, for the same reason the global-basename
	// fallback is not: parent, lag and bridge are NetBox-core conventions, and
	// they are generic enough words that an arbitrary third-party schema can use
	// any of them for something else entirely. The named overrides above stay
	// available to plugins because those names — primary_ip4, untagged_vlan —
	// are specific enough that using one is following the convention on purpose.
	if selfFKs[base] && !strings.HasPrefix(entity, "plugins/") {
		return entity, known[entity]
	}

	app, model := splitEntity(entity)
	slug := strings.ReplaceAll(base, "_", "-")

	// Most specific: the owning model qualifies the target, e.g. a site's
	// group_id is a site-group, a device's role_id a device-role.
	for _, cand := range pluralize(singularize(model) + "-" + slug) {
		if e := app + "/" + cand; known[e] {
			return e, true
		}
	}
	// Same app, plain name: a prefix's role_id is an ipam role.
	for _, cand := range pluralize(slug) {
		if e := app + "/" + cand; known[e] {
			return e, true
		}
	}
	// Any app, but only when exactly one matches. Ambiguity here would be a
	// coin flip between two real models, which is worse than leaving the id.
	//
	// Not for a plugin's models. The measurement that justifies this fallback —
	// 33 right against 1 wrong — was taken over NetBox's own apps, where the
	// naming is consistent because one project chose it. A plugin's schema is
	// arbitrary and third-party, so a unique basename elsewhere in the
	// deployment is not evidence about it: plugins/acme/widgets.role_id would
	// resolve to ipam/roles purely because that is the only "roles" model
	// anyone happens to serve. Leaving the id is the honest answer.
	if strings.HasPrefix(app, "plugins/") {
		return "", false
	}
	var found string
	var n int
	for _, cand := range pluralize(slug) {
		for e := range known {
			if _, m := splitEntity(e); m == cand {
				if e != found {
					n++
				}
				found = e
			}
		}
	}
	if n == 1 {
		return found, true
	}
	return "", false
}

// pluralize returns the plural spellings worth trying, most likely first. It is
// deliberately crude: every candidate is checked against the real entity list,
// so an implausible guess costs a map lookup and nothing else.
func pluralize(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{s + "s", s}
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") || strings.HasSuffix(s, "ch") {
		out = append([]string{s + "es"}, out...)
	}
	if strings.HasSuffix(s, "y") {
		out = append([]string{strings.TrimSuffix(s, "y") + "ies"}, out...)
	}
	return out
}

// singularize undoes the entity list's pluralization well enough to build a
// qualified candidate ("devices" -> "device", "ip-addresses" -> "ip-address").
func singularize(s string) string {
	switch {
	// Already singular despite the trailing s: "virtual-chassis" is one chassis,
	// and trimming it yields "virtual-chassi", which matches nothing.
	case strings.HasSuffix(s, "is"):
		return s
	case strings.HasSuffix(s, "ies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "ches"):
		return strings.TrimSuffix(s, "es")
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// splitEntity divides an object type into its app and its model.
//
// It splits at the LAST slash, not the first, because a plugin's models sit one
// level deeper: plugins/acme/widgets is the widgets model of the plugins/acme
// app, which is also what parseEntityPath produces for it. Splitting at the
// first slash made the app "plugins" and the model "acme/widgets", so a
// role_id could not reach plugins/acme/roles and — worse — fell through to the
// global-basename fallback, which would have resolved it to ipam/roles.
func splitEntity(e string) (app, model string) {
	if i := strings.LastIndexByte(e, '/'); i >= 0 {
		return e[:i], e[i+1:]
	}
	return "", e
}

// related is the resolved label of one row in a dimension table.
type related struct {
	display string
	slug    string
}

// fkCache holds resolved dimension rows per entity.
type fkCache struct {
	mu      sync.Mutex
	entries map[string]*fkCacheEntry
}

type fkCacheEntry struct {
	rows    map[int]related
	expires time.Time
}

func newFKCache() *fkCache { return &fkCache{entries: map[string]*fkCacheEntry{}} }

// lookup returns the cached rows for an entity and the ids still missing.
func (c *fkCache) lookup(entity string, ids []int) (map[int]related, []int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	have := map[int]related{}
	var missing []int
	e := c.entries[entity]
	if e == nil || time.Now().After(e.expires) {
		return have, ids
	}
	for _, id := range ids {
		if r, ok := e.rows[id]; ok {
			have[id] = r
		} else {
			missing = append(missing, id)
		}
	}
	return have, missing
}

func (c *fkCache) store(entity string, rows map[int]related) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[entity]
	if e == nil || time.Now().After(e.expires) {
		e = &fkCacheEntry{rows: map[int]related{}, expires: time.Now().Add(fkCacheTTL)}
		c.entries[entity] = e
	}
	for id, r := range rows {
		e.rows[id] = r
	}
}

// resolveFKs adds the display and slug columns for every foreign key present in
// the rows, in place.
//
// It returns the columns it added, in a stable order, and a list of
// user-facing warnings for dimensions it could not FETCH. The distinction
// matters: a dimension that this deployment does not expose is not a warning,
// because nothing failed and the id is still there to see. A dimension that
// exists but did not answer leaves a blank name column, which looks exactly
// like "this device has no site" — so that one is stated.
// want names the derived columns the caller actually asked for, or is nil to
// mean all of them.
//
// It exists because resolving a name the caller did not request is not free
// twice over: it can spend the whole discovery budget, and a dimension that
// fails adds a warning — which alert evaluation treats as a hard failure (see
// pkg/plugin/query.go's degradationError). A rule selecting only site_id would
// stop firing because dcim/sites was unreachable, though every value it asked
// for was present.
func (p *Provider) resolveFKs(ctx context.Context, entity string, rows []map[string]interface{}, want map[string]bool) ([]string, []string) {
	// The entity list is needed to target the FKs, but this runs AFTER the rows
	// have arrived — so an unbounded fetch here delays a panel that already has
	// its data, which is the one place a long timeout buys nothing.
	//
	// So the wait is capped, and the refresh behind it is shared between
	// concurrent panels. Healthy discovery (~1.6s) resolves inside the budget
	// and the names appear; a hung one costs a few seconds once rather than a
	// full HTTP timeout per query, and the result degrades honestly. Rows are
	// complete and correct as ids either way; only the names are missing.
	// Nothing to resolve means nothing to wait for. An empty result, or a
	// projection of scalar columns only, has no *_id to target — and paying the
	// discovery budget to discover that would add seconds to a query that was
	// never going to use the answer.
	if !hasResolvableFK(entity, rows, want) {
		return nullFKColumns(entity, rows, p.cachedEntitySet), nil
	}

	known, ok := p.entitySetSoon(ctx, discoveryWaitBudget)
	if !ok {
		// The all-null columns still stand: they are derived from the rows
		// alone and need nothing from discovery, so a projection of site and
		// tenant should lose only the one that had ids to resolve.
		return nullFKColumns(entity, rows, p.cachedEntitySet), []string{"Related names could not be added yet: the list of available object types is still being read. Columns such as \"site\" and \"role\" are missing from this result; the matching *_id columns still hold the values, and a refresh should resolve them."}
	}

	// Group the ids to resolve by target entity.
	targets := map[string]map[int]bool{}
	bases := map[string]string{} // column base -> target entity
	for _, row := range rows {
		for col, v := range row {
			base, ok := strings.CutSuffix(col, "_id")
			if !ok || base == "" || notAKey(entity, base) || !wanted(want, base) {
				continue
			}
			id, ok := toInt(v)
			if !ok {
				continue
			}
			target, ok := bases[base]
			if !ok {
				target, _ = fkTarget(entity, base, known)
				bases[base] = target
			}
			if target == "" {
				continue
			}
			if targets[target] == nil {
				targets[target] = map[int]bool{}
			}
			targets[target][id] = true
		}
	}

	resolved := map[string]map[int]related{}
	var warnings []string
	for target, idSet := range targets {
		ids := make([]int, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		sort.Ints(ids)

		have, missing := p.fk.lookup(target, ids)
		if len(missing) > 0 {
			fetched, err := p.fetchRelated(ctx, target, missing)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf(
					"Related names from %s could not be read, so those columns are blank on some rows; the matching *_id columns still hold the values.", target))
			} else {
				// Only the rows that were ASKED for. An id__in lookup answered
				// with an id outside the request is not an answer to anything,
				// and storing it would let a later query take an unrequested —
				// possibly wrong — label as authoritative, with no lookup and no
				// warning, for the whole cache lifetime.
				wanted := make(map[int]bool, len(missing))
				for _, id := range missing {
					wanted[id] = true
				}
				keep := make(map[int]related, len(fetched))
				for id, r := range fetched {
					if wanted[id] {
						keep[id] = r
					}
				}
				fetched = keep

				p.fk.store(target, fetched)
				for id, r := range fetched {
					have[id] = r
				}
				// A 200 that answers for only some of the ids asked about is not
				// a protocol failure: these are separate tables replicated
				// independently, so a row can reference a site whose own row has
				// not arrived yet, or was deleted between the two requests. It
				// still leaves the column blank on those rows, which reads as
				// "this device has no site" — the same confusion the warning
				// beside it exists to prevent, and one an alert rule would
				// otherwise consume as fact.
				// By NAME, not by count. A lookup for {1,2} answered with
				// {1,999} has the same cardinality while leaving 2 unresolved,
				// so subtracting map sizes reported nothing missing.
				var absent int
				for _, id := range missing {
					if _, ok := fetched[id]; !ok {
						absent++
					}
				}
				if n := absent; n > 0 {
					warnings = append(warnings, fmt.Sprintf(
						"Related names from %s are missing for %d of the %d objects referenced here, so those columns are blank on those rows; the matching *_id columns still hold the values. The cache mirrors each table separately, so a reference can outrun the row it points at.",
						target, n, len(ids)))
				}
			}
		}
		resolved[target] = have
	}

	// Apply, collecting new column names in a deterministic order.
	added := nullFKColumns(entity, rows, p.cachedEntitySet)
	seen := map[string]bool{}
	for _, c := range added {
		seen[c] = true
	}
	baseNames := make([]string, 0, len(bases))
	for b := range bases {
		baseNames = append(baseNames, b)
	}
	sort.Strings(baseNames)

	for _, base := range baseNames {
		target := bases[base]
		if target == "" {
			continue
		}
		byID := resolved[target]
		for _, row := range rows {
			id, ok := toInt(row[base+"_id"])
			if !ok {
				continue
			}
			r, ok := byID[id]
			if !ok {
				continue
			}
			if _, exists := row[base]; !exists {
				row[base] = r.display
			}
			if r.slug != "" {
				row[base+"_slug"] = r.slug
			}
		}
		// A column is only announced if at least one row actually carries it,
		// so an entity whose ids all failed to resolve does not contribute an
		// all-empty column to every panel.
		for _, row := range rows {
			if _, ok := row[base]; ok && !seen[base] {
				seen[base] = true
				added = append(added, base)
			}
			if _, ok := row[base+"_slug"]; ok && !seen[base+"_slug"] {
				seen[base+"_slug"] = true
				added = append(added, base+"_slug")
			}
		}
	}
	return added, warnings
}

// fetchRelated reads the named ids out of a dimension table.
func (p *Provider) fetchRelated(ctx context.Context, entity string, ids []int) (map[int]related, error) {
	out := make(map[int]related, len(ids))
	for start := 0; start < len(ids); start += fkBatchSize {
		end := start + fkBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]

		parts := make([]string, len(batch))
		for i, id := range batch {
			parts[i] = strconv.Itoa(id)
		}
		q := url.Values{}
		q.Set(param("id", "in"), strings.Join(parts, ","))

		raws, _, err := p.client.list(ctx, entity, q, len(batch))
		if err != nil {
			return nil, err
		}
		for _, raw := range raws {
			var obj map[string]interface{}
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, &TransportError{Op: "reading a " + entity + " row", Err: err, Message: rowShapeGuidance}
			}
			if obj == nil {
				// Same trap as the top-level rows: a JSON null decodes without
				// error and leaves the map nil. Skipping it made fetchRelated
				// report SUCCESS with a name missing, so resolveFKs raised no
				// degradation warning and the panel showed a blank "site" with
				// nothing to say why.
				return nil, &TransportError{Op: "reading a " + entity + " row", Err: errMalformedRow, Message: rowShapeGuidance}
			}
			if id, ok := toInt(obj["id"]); ok {
				// Same rule as the main rows. Letting the last one win stored an
				// arbitrary label in the SHARED cache, where a later query takes
				// it without another lookup or a warning — the duplicate is gone
				// by then, so nothing can even tell that a choice was made.
				if _, dup := out[id]; dup {
					return nil, &TransportError{Op: "reading a " + entity + " row", Err: errDuplicateRow, Message: rowShapeGuidance}
				}
			}
			id, ok := toInt(obj["id"])
			if !ok {
				// Being object-shaped is not enough: without an id there is
				// nothing to match against the rows that referenced it. Skipping
				// it let fetchRelated report SUCCESS with names missing, so no
				// degradation warning was raised and the panel showed a blank
				// "site" with nothing to explain it — the same silence as the
				// null rows above, one shape further in.
				return nil, &TransportError{Op: "reading a " + entity + " row", Err: errRowWithoutID, Message: rowShapeGuidance}
			}
			out[id] = related{display: displayOf(obj), slug: stringOf(obj["slug"])}
		}
	}
	return out, nil
}

// displayOf picks the human-readable label for a dimension row.
//
// The preference order IS the NetBox provider's, not a copy of it: both call
// the shared contract, so the two cannot drift into rendering the same object
// differently. The id fallback is this backend's own, and deliberate — a
// dimension row that was genuinely found must never render blank, because a
// blank site reads as "this device has no site" rather than as "the name could
// not be determined".
func displayOf(obj map[string]interface{}) string {
	if s := provider.NestedDisplay(obj); s != "" {
		return s
	}
	if id, ok := toInt(obj["id"]); ok {
		return strconv.Itoa(id)
	}
	return ""
}

func stringOf(v interface{}) string {
	s, _ := v.(string)
	return s
}

// toInt accepts the shapes a JSON number can arrive in.
// toInt reads a primary key or foreign key. Every caller wants an identifier,
// so the bar is a positive whole number that fits: JSON has one number type, and
// an id of 1.9 silently truncated to 1 would be a DIFFERENT object in every
// lookup and deep link built from it, while 0, a negative and a non-finite
// value are not identifiers at all.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return fromFloat(n)
	case int:
		return n, n > 0
	case json.Number:
		i, err := n.Int64()
		if err != nil || i <= 0 {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// maxExactID bounds ids at the range where float64 is INJECTIVE, which is a
// stronger property than being exactly representable and is the one that
// matters here.
//
// Ids arrive through encoding/json as float64, and the rounding happens during
// decode, before anything here can inspect the value: the wire integer
// 9007199254740993 is already 9007199254740992 by the time it is checked, so a
// bound that merely excluded inexact values could not detect the collision.
// Below 2^53 no two integers share a float64, so an accepted value names
// exactly one object. 2^53 itself is excluded because 2^53+1 rounds onto it.
//
// The alternative — decoding rows with json.Number to keep the token — would
// change the type of every value in every row, and with it type inference and
// frame building, to defend a boundary no NetBox instance reaches: its keys are
// 64-bit, but 2^53 is nine quadrillion rows.
const maxExactID = 1 << 53

func fromFloat(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f <= 0 || f >= maxExactID {
		return 0, false
	}
	return int(f), true
}

// hasResolvableFK reports whether any row carries a column that could name a
// related object. It is deliberately cheap and permissive: a false positive
// costs one discovery lookup, while a false negative would silently drop the
// resolved names.
// wanted reports whether a relationship's derived columns were asked for. A nil
// set means every column was, which is what an unprojected query and the schema
// sample both need.
func wanted(want map[string]bool, base string) bool {
	return want == nil || want[base]
}

// nullFKColumns announces the derived column for a relationship that is null on
// every row, and fills it with nils.
//
// NetBox mode has the column: its API answers "tenant": null, which flattens to
// a tenant column of nulls. Here the same fact arrives as tenant_id: null, from
// which nothing is resolved, so the column did not exist at all — and a panel
// selecting tenant lost it on switching modes, or came back with NO columns
// when tenant was the only field selected.
//
// This is not the same as the ids that FAILED to resolve, which are still left
// out on purpose a few lines below. An all-empty column there would read as
// "this device has no tenant" when the truth is "the tenant could not be read",
// and that case has its own warning. All-null is different: there really is no
// tenant, and saying so is the accurate answer as well as the compatible one.
func nullFKColumns(entity string, rows []map[string]interface{}, cachedSet func() (map[string]bool, bool)) []string {
	// A plugin's models need a discovered target, not just the suffix. On
	// NetBox's own, a *_id column that is null everywhere is a relationship
	// unless it is one of the seven text identifiers named above — a list taken
	// from the schema. A plugin can define anything and there is no such list,
	// so the suffix alone fabricated an "external" column for an ordinary
	// nullable external_id, which Fields advertised until a row carried a
	// string and it vanished.
	//
	// The entity list settles it, and reading it here costs nothing: this is
	// the ALREADY-CACHED set, never a fetch, the same accessor
	// validateObjectType uses. plugins/acme/widgets.owner_id resolves to
	// plugins/acme/owners and keeps its column; external_id resolves to nothing
	// and gets none. Uncached, no plugin column is synthesized — conservative
	// until the editor warms it, which it does when it populates its object-type
	// dropdown.
	plugin := strings.HasPrefix(entity, "plugins/")
	var known map[string]bool
	if plugin {
		set, ok := cachedSet()
		if !ok {
			return nil
		}
		known = set
	}
	bases := map[string]bool{}
	for _, row := range rows {
		for col, v := range row {
			base, ok := strings.CutSuffix(col, "_id")
			if !ok || base == "" || polymorphicFKs[base] || notAKey(entity, base) {
				continue
			}
			if unexposedFKs[base] && !strings.HasPrefix(entity, "plugins/") {
				continue
			}
			switch _, kind := classifyFKValue(v); kind {
			case fkID:
				// Some row does carry an id, so this column is resolution's
				// business, not ours.
				bases[base] = false
			case fkBadID:
				// Unreachable: flattenRows refuses the row before this runs. Kept
				// explicit so the switch stays exhaustive and a future caller
				// that skips that check does not silently land in fkNull.
				bases[base] = false
			case fkNotAKey:
				// Not a foreign key at all — NetBox has CharFields whose names
				// end in _id, such as circuits.ProviderNetwork.service_id, which
				// holds the provider's own service identifier as text. Deriving
				// a "service" column of nils from one was inventing a
				// relationship that does not exist.
				bases[base] = false
			case fkNull:
				if _, seen := bases[base]; !seen {
					bases[base] = true
				}
			}
		}
	}

	out := make([]string, 0, len(bases))
	for base, allNull := range bases {
		if !allNull {
			continue
		}
		if plugin {
			if _, ok := fkTarget(entity, base, known); !ok {
				continue
			}
		}
		out = append(out, base)
	}
	sort.Strings(out)
	for _, base := range out {
		for _, row := range rows {
			if _, exists := row[base]; !exists {
				row[base] = nil
			}
		}
	}
	return out
}

// fkValueKind says what a *_id column's value is, which the three cases below
// need to tell apart and toInt alone cannot: it answers false for a genuine
// null, for a malformed number and for a column that is not a key at all.
type fkValueKind int

const (
	fkNull    fkValueKind = iota // a genuine null relationship
	fkID                         // a usable identifier
	fkBadID                      // a number, but not an identifier
	fkNotAKey                    // not a number, so not a foreign key
)

func classifyFKValue(v interface{}) (int, fkValueKind) {
	if v == nil {
		return 0, fkNull
	}
	switch n := v.(type) {
	case float64, int, json.Number:
		if id, ok := toInt(n); ok {
			return id, fkID
		}
		return 0, fkBadID
	}
	return 0, fkNotAKey
}

func hasResolvableFK(entity string, rows []map[string]interface{}, want map[string]bool) bool {
	for _, row := range rows {
		for col, v := range row {
			base, ok := strings.CutSuffix(col, "_id")
			if !ok || base == "" || notAKey(entity, base) || !wanted(want, base) {
				continue
			}
			// The suffix alone is not enough. owner_id, created_by_id and
			// assigned_object_id are rejected by fkTarget no matter what
			// discovery returns, so counting them here bought a wait of up to
			// the whole budget to learn something already known — on a
			// projection of exactly those columns, every query paid it.
			if polymorphicFKs[base] {
				continue
			}
			// Core-only, matching fkTarget. Applied globally here, the early
			// exit ran BEFORE discovery, so a projected plugin query asking for
			// plugins/acme/widgets.owner never reached the scoped logic at all:
			// no discovery, no resolution, and a degradation warning that fails
			// alert evaluation even though plugins/acme/owners is right there.
			if unexposedFKs[base] && !strings.HasPrefix(entity, "plugins/") {
				continue
			}
			if _, kind := classifyFKValue(v); kind == fkID {
				return true
			}
		}
	}
	return false
}
