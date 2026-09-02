package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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

// displayFields is the preference order for the human-readable label of a
// related object. It deliberately matches nestedDisplay in the NetBox provider,
// because the two have to produce the same string for the same object: a
// device-type shows its model, an IP its address, a circuit its cid.
var displayFields = []string{"display", "name", "label", "address", "prefix", "cid", "model", "rgb"}

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

// unexposedFKs point at tables replica-cache does not replicate at all (users,
// config templates). Nothing can resolve them, so the raw id stays.
var unexposedFKs = map[string]bool{
	"owner":           true,
	"config_template": true,
	"created_by":      true,
	"last_updated_by": true,
}

// ipAddressFKs are the columns that reference an IP address under a name that
// does not contain "ip-address". Convention cannot find these, and they are
// common enough on devices and VMs to be worth naming.
var ipAddressFKs = map[string]bool{
	"primary_ip":  true,
	"primary_ip4": true,
	"primary_ip6": true,
	"oob_ip":      true,
	"nat_inside":  true,
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
	if polymorphicFKs[base] || unexposedFKs[base] {
		return "", false
	}
	if ipAddressFKs[base] {
		const ipEntity = "ipam/ip-addresses"
		if known[ipEntity] {
			return ipEntity, true
		}
		return "", false
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

func splitEntity(e string) (app, model string) {
	if i := strings.IndexByte(e, '/'); i >= 0 {
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
func (p *Provider) resolveFKs(ctx context.Context, entity string, rows []map[string]interface{}) ([]string, []string) {
	// The entity list is needed to target the FKs, but this runs AFTER the rows
	// have arrived — so an unbounded fetch here delays a panel that already has
	// its data, which is the one place a long timeout buys nothing.
	//
	// So the wait is capped, and the refresh behind it is shared between
	// concurrent panels. Healthy discovery (~1.6s) resolves inside the budget
	// and the names appear; a hung one costs a few seconds once rather than a
	// full HTTP timeout per query, and the result degrades honestly. Rows are
	// complete and correct as ids either way; only the names are missing.
	known, ok := p.entitySetSoon(discoveryWaitBudget)
	if !ok {
		return nil, []string{"Related names could not be added yet: the list of available object types is still being read. Columns such as \"site\" and \"role\" are missing from this result; the matching *_id columns still hold the values, and a refresh should resolve them."}
	}

	// Group the ids to resolve by target entity.
	targets := map[string]map[int]bool{}
	bases := map[string]string{} // column base -> target entity
	for _, row := range rows {
		for col, v := range row {
			base, ok := strings.CutSuffix(col, "_id")
			if !ok || base == "" {
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
				p.fk.store(target, fetched)
				for id, r := range fetched {
					have[id] = r
				}
			}
		}
		resolved[target] = have
	}

	// Apply, collecting new column names in a deterministic order.
	var added []string
	seen := map[string]bool{}
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
				continue
			}
			id, ok := toInt(obj["id"])
			if !ok {
				continue
			}
			out[id] = related{display: displayOf(obj), slug: stringOf(obj["slug"])}
		}
	}
	return out, nil
}

// displayOf picks the human-readable label for a dimension row, mirroring the
// NetBox provider's choice so both backends render the same object the same way.
func displayOf(obj map[string]interface{}) string {
	for _, k := range displayFields {
		if s := stringOf(obj[k]); s != "" {
			return s
		}
	}
	// Nothing nameable: fall back to the id so the column is never blank when a
	// row genuinely was found.
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
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
