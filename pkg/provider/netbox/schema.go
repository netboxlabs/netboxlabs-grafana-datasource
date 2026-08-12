package netbox

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// suffixToken maps a NetBox lookup suffix to our operator token (token == suffix).
// This models NetBox's full documented string/negation/numeric lookup set so the
// dropdown-only editor doesn't drop supported filters (e.g. regex, nic). A suffix
// not listed here is ignored. Keep in sync with FILTER_OPERATORS in src/types.ts —
// the editor only surfaces operators present in both.
//
// "nempty" is deliberately NOT a key here even though it IS in FILTER_OPERATORS
// and operatorOrder: NetBox advertises only `__empty` (a BooleanFilter), never a
// `__nempty` param, so there is no suffix to map. "nempty" is derived instead,
// in the parse loop's `if tok == "empty"` branch below. Do not "fix" this by
// adding `"nempty": "nempty"` here — that would make the parser accept a suffix
// NetBox doesn't send.
var suffixToken = map[string]string{
	"n": "n", "ie": "ie", "nie": "nie",
	"ic": "ic", "nic": "nic",
	"isw": "isw", "nisw": "nisw",
	"iew": "iew", "niew": "niew",
	"regex": "regex", "iregex": "iregex",
	"gte": "gte", "lte": "lte", "gt": "gt", "lt": "lt",
	"empty": "empty",
}

// operatorOrder is the canonical display order (matches FILTER_OPERATORS in
// src/types.ts). Exact ("") first.
var operatorOrder = []string{
	"", "n", "ie", "nie", "ic", "nic", "isw", "nisw", "iew", "niew",
	"regex", "iregex", "gte", "lte", "gt", "lt", "empty", "nempty",
}

// nonFilterParams are NetBox/DRF query params that shape the response or
// paginate rather than filter objects, so they must not appear as filter
// fields. Note: `depth` is NOT here — it is a real prefix filter (with
// __gte/__lte/etc. lookups), not a response param. `omit` (NetBox 4.5.2+) is a
// response-shaping param like `fields`.
var nonFilterParams = map[string]bool{
	"limit": true, "offset": true, "ordering": true, "format": true,
	"brief": true, "exclude": true, "fields": true, "omit": true,
}

type openAPIDoc struct {
	Paths map[string]struct {
		Get *struct {
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"get"`
	} `json:"paths"`
}

// parseFilterFields parses a NetBox OpenAPI schema into objectType -> filter fields.
func parseFilterFields(schema []byte) (map[string][]provider.FilterField, error) {
	var doc openAPIDoc
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil, err
	}
	out := map[string][]provider.FilterField{}
	for path, item := range doc.Paths {
		if item.Get == nil {
			continue
		}
		// Only list endpoints: skip detail/sub-resource paths with path params
		// (e.g. /api/ipam/prefixes/{id}/) which would produce junk map keys.
		if strings.Contains(path, "{") {
			continue
		}
		// Take the segment after "/api/" wherever it occurs, so paths resolve
		// even when NetBox is mounted under a base path (e.g.
		// "/netbox/api/ipam/prefixes/" -> "ipam/prefixes") — a deployment
		// Client.apiURL already supports. Otherwise the cache key would keep the
		// prefix and FilterFields("ipam/prefixes") would miss, dropping the
		// editor back to all-operators.
		idx := strings.Index(path, "/api/")
		if idx < 0 {
			continue
		}
		objectType := strings.TrimSuffix(path[idx+len("/api/"):], "/")
		if objectType == "" || !strings.Contains(objectType, "/") {
			continue
		}
		// Collect the set of query param names first (to detect lookup variants).
		present := map[string]bool{}
		var order []string
		for _, pr := range item.Get.Parameters {
			if pr.In == "query" {
				if !present[pr.Name] {
					order = append(order, pr.Name)
				}
				present[pr.Name] = true
			}
		}
		// ops[base] = set of operator tokens
		ops := map[string]map[string]bool{}
		ensure := func(base string) {
			if ops[base] == nil {
				ops[base] = map[string]bool{}
			}
		}
		var fieldOrder []string
		addField := func(base string) {
			if _, seen := ops[base]; !seen {
				fieldOrder = append(fieldOrder, base)
			}
			ensure(base)
		}
		for _, name := range order {
			if nonFilterParams[name] {
				continue
			}
			if i := strings.LastIndex(name, "__"); i >= 0 {
				base, suf := name[:i], name[i+2:]
				if present[base] {
					// lookup variant of an existing base
					if tok, ok := suffixToken[suf]; ok {
						addField(base)
						ops[base][tok] = true
						// __empty is a BooleanFilter, so a field that supports it
						// supports both directions: "is empty" (true) and "has any
						// value" (false). NetBox advertises only the one param.
						if tok == "empty" {
							ops[base]["nempty"] = true
						}
					}
					continue // do not treat X__Y as its own field
				}
			}
			// base field (exact)
			addField(name)
			ops[name][""] = true
		}
		var fields []provider.FilterField
		for _, base := range fieldOrder {
			var operators []string
			for _, tok := range operatorOrder {
				if ops[base][tok] {
					operators = append(operators, tok)
				}
			}
			fields = append(fields, provider.FilterField{Name: base, Operators: operators})
		}
		sort.SliceStable(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
		out[objectType] = fields
	}
	return out, nil
}
