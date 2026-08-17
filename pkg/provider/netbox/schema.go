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
// and operatorOrder: NetBox advertises only `__empty`, never a `__nempty` param,
// so there is no suffix to map. "nempty" is derived instead, in the parse loop's
// `tok == "empty"` branch below — and only for the subset of `__empty` params
// that are genuinely boolean; see the comment there. Do not "fix" this by adding
// `"nempty": "nempty"` here — that would make the parser accept a suffix NetBox
// doesn't send.
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
			Parameters []openAPIParam `json:"parameters"`
		} `json:"get"`
	} `json:"paths"`
}

// openAPIParam is one entry of a GET operation's `parameters` list. It decodes
// leniently on purpose: NetBox's /api/schema/ is ~10 MB with >12k GET query
// params, and with a strict decoder a SINGLE parameter of an unexpected shape
// fails the entire json.Unmarshal. parseFilterFields would then return zero
// object types, and the blast radius is total — every object type loses
// schema-derived operator narrowing and falls back to the full FILTER_OPERATORS
// list (re-offering exactly the broken empty/nempty the __empty gate below
// suppresses), Provider.schema() returns an error before it caches, so the 10 MB
// document is re-fetched on every editor call, and FieldValues gets no schema
// entry at all — no dimension resolves and it reverts to sampling. One
// unreadable parameter must cost only itself.
//
// That last consequence is NOT the dimension index's own exposure, and this
// leniency does nothing for it: the index is built by a SECOND, independent
// decode of the same bytes, over component schemas rather than query parameters.
// The union type named below reaches those as well — a component property spells
// its type the same way — so dimProp (dimension.go) carries the same leniency
// for the same reason.
//
// Shapes to survive: an OpenAPI 3.1 union type (`"type": ["string","null"]`),
// and any raw JSON Schema a plugin injects (including a non-object `schema`).
// The demo (NetBox 4.4.10) is openapi 3.0.3 — 12780 GET query params, zero
// non-string types — so nothing here fires against it; the 4.6.4 document is
// unconfirmed, hence defence rather than a bet.
type openAPIParam struct {
	Name string
	In   string
	// Type is the parameter's own declared schema type, read for exactly one
	// purpose — deciding whether `__empty` is a real BooleanFilter; see the
	// `tok == "empty"` branch in parseFilterFields. It is "" whenever the type
	// is absent, the schema is a $ref, or the type is not a plain JSON string.
	// That last case is deliberate and load-bearing: a union like
	// ["boolean","null"] is not "boolean", so the gate declines the operator
	// rather than offering one NetBox may reject.
	Type string
}

// UnmarshalJSON decodes a parameter without ever failing the document; see
// openAPIParam. Anything it cannot read degrades to a zero param, which the
// parse loop skips because In is then not "query".
func (p *openAPIParam) UnmarshalJSON(b []byte) error {
	// Schema stays raw so a non-object schema costs only the type, not the
	// whole parameter.
	var raw struct {
		Name   string          `json:"name"`
		In     string          `json:"in"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		*p = openAPIParam{}
		return nil
	}
	*p = openAPIParam{Name: raw.Name, In: raw.In}
	var sch struct {
		Type json.RawMessage `json:"type"`
	}
	if json.Unmarshal(raw.Schema, &sch) != nil || len(sch.Type) == 0 {
		return nil
	}
	var typ string
	if json.Unmarshal(sch.Type, &typ) == nil {
		p.Type = typ
	}
	return nil
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
		// Collect the set of query param names first (to detect lookup variants),
		// plus each param's declared schema type (needed by the __empty gate below).
		present := map[string]bool{}
		paramType := map[string]string{}
		var order []string
		for _, pr := range item.Get.Parameters {
			if pr.In == "query" {
				if !present[pr.Name] {
					order = append(order, pr.Name)
					paramType[pr.Name] = pr.Type
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
						// NetBox advertises `__empty` on far more fields than accept
						// it, because it builds lookups from two different maps
						// (netbox/utilities/filters.py):
						//
						//   FILTER_CHAR_BASED_LOOKUP_MAP    = dict(..., empty='empty',  ...)
						//   FILTER_NUMERIC_BASED_LOOKUP_MAP = dict(..., empty='isnull', ...)
						//
						// The char-based map produces a real BooleanFilter. The
						// numeric-based one (MultiValueNumber/Decimal/Date/DateTime/
						// TimeFilter) produces a param that still validates as its
						// PARENT field's type, so `?x__empty=true` is a 400.
						//
						// /api/schema/ exposes the difference on the __empty param
						// itself, so gate on that and only that. Measured against the
						// demo (NetBox 4.4.10), same endpoint, both integer fields:
						//   mtu__empty   {"type":"boolean"}                -> 200, count=34
						//   speed__empty {"type":"array",…"format":"int32"} -> 400
						//     {"speed__empty":["Enter a whole number."]}
						// and on devices:
						//   last_updated__empty {"type":"array",…"date-time"} -> 400
						//     {"last_updated__empty":["Enter a valid date/time."]}
						//
						// DO NOT re-simplify this into a field-type or timestamp
						// check. That rule was measured wrong in BOTH directions: 7
						// date-time fields DO accept __empty, and speed__empty is an
						// int32 (not a date) that fails on the very endpoint where the
						// equally-integer mtu__empty succeeds. Only the __empty
						// parameter's own declared type predicts the outcome.
						//
						// No declared type means we cannot tell, so we do not offer
						// the pair: an operator the user can pick and NetBox rejects
						// is worse than one absent from the dropdown. On 4.4.10 all
						// 930 __empty params declare a type (685 boolean, 245 not) and
						// on 4.6.4 all 1044 do, so this fallback never fires today.
						// A type openAPIParam could not read as a plain string (an
						// OpenAPI 3.1 union like ["boolean","null"]) arrives here as ""
						// and is declined for the same reason.
						if tok == "empty" && paramType[name] != "boolean" {
							continue
						}
						addField(base)
						ops[base][tok] = true
						// A genuine BooleanFilter answers both directions from the one
						// advertised param: "is empty" (true) and "has any value"
						// (false). NetBox never advertises a `__nempty` param.
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
