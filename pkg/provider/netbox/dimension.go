package netbox

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Dimension resolution for FieldValues.
//
// FieldValues answers "what values can this column take?". The cheap, wrong way
// is to read a page of objects out of the fact table and collect the distinct
// values it happens to contain: on a large instance, scanning a page of devices
// yields 1000 distinct sites out of 4030, so three quarters of the real sites
// are unreachable and WHICH quarter you get depends on device ordering.
//
// The right source is the dimension itself. A device's `site` column is a
// foreign key to dcim/sites, which has 4030 rows and answers in a fraction of a
// second; a device's `status` column is a closed enumeration that the OpenAPI
// schema already spells out. Neither cost grows with the size of the fact table.
//
// Both relationships are DERIVED from /api/schema/ rather than hard-coded, so
// plugin-provided models and future core models work with no code change:
//
//	/api/dcim/devices/ GET 200 -> PaginatedDeviceWithConfigContextList
//	  .properties.results.items      -> DeviceWithConfigContext
//	    .properties.site             -> BriefSite
//	      strip "Brief"              -> Site
//	        which list path returns Site? -> /api/dcim/sites/
//
// The same walk finds enumerations: a choice column is an inline object whose
// `value` (and usually `label`) property carries an `enum`.
//
// The index is built from the SAME schema document FilterFields already fetches
// and caches (30 min, partitioned by branch), so resolution normally costs no
// extra request: the query editor loads filter-fields the moment an object type
// is picked (QueryEditor.tsx), which is necessarily before any value dropdown
// for that object type can be opened.

// dimensionKind classifies what a column's values can be enumerated from.
type dimensionKind int

const (
	// dimNone: a plain scalar (name, description, an id) with no backing
	// dimension. Callers fall back to sampling the fact table.
	dimNone dimensionKind = iota
	// dimRelated: a single-valued foreign key to another list endpoint.
	dimRelated
	// dimChoice: a closed enumeration declared inline in the schema.
	dimChoice
)

// dimension describes how one top-level property of an object type can be
// enumerated. List-valued relationships (tags, and other arrays) are
// deliberately dimNone: flattenObject renders a list as one "; "-joined string,
// so the values of that COLUMN are combinations, not the members of the related
// model — enumerating the model would change what the column means.
type dimension struct {
	kind dimensionKind

	// endpoint is the related object type ("dcim/sites"), for dimRelated.
	endpoint string

	// choices are the {value,label} pairs of a dimChoice, in schema order.
	// They are fed through flattenField exactly like a live API response, so a
	// choice column renders identically whether it came from here or from a
	// sampled object.
	choices []map[string]interface{}
}

// dimIndex maps objectType -> top-level property name -> dimension. EVERY
// property of the object type is present, including scalars (dimNone): the
// index doubles as the authority on which `?fields=` projections are valid,
// and NetBox answers an unknown ?fields= name with an empty object rather than
// an error.
type dimIndex map[string]map[string]dimension

// dimDoc is the slice of the OpenAPI document the index is built from. Only the
// declared fields are decoded; the rest of the (13 MB, on a large instance)
// schema is skipped by encoding/json.
type dimDoc struct {
	Paths map[string]struct {
		Get *struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"get"`
	} `json:"paths"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]dimProp `json:"properties"`
		} `json:"schemas"`
	} `json:"components"`
}

// dimProp is one property of a component schema. `enum` members are decoded as
// interface{} (not string): NetBox declares some choice values as integers, and
// a []string target would fail the whole document.
type dimProp struct {
	Ref   string `json:"$ref"`
	AllOf []struct {
		Ref string `json:"$ref"`
	} `json:"allOf"`
	Type  string `json:"type"`
	Items struct {
		Ref string `json:"$ref"`
	} `json:"items"`
	Properties struct {
		Value struct {
			Enum []interface{} `json:"enum"`
		} `json:"value"`
		Label struct {
			Enum []interface{} `json:"enum"`
		} `json:"label"`
	} `json:"properties"`
}

// ref returns the component schema this property points at, for a SINGLE-valued
// reference only. `{"$ref": X}` and the `{"allOf":[{"$ref": X}], "nullable":
// true}` spelling drf-spectacular uses for optional FKs both count; an array of
// refs deliberately does not (see dimension).
func (p dimProp) ref() string {
	if p.Ref != "" {
		return schemaName(p.Ref)
	}
	if len(p.AllOf) == 1 && p.AllOf[0].Ref != "" {
		return schemaName(p.AllOf[0].Ref)
	}
	return ""
}

func schemaName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// parseDimensions builds the dimension index from a NetBox OpenAPI schema.
func parseDimensions(schema []byte) (dimIndex, error) {
	var doc dimDoc
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil, err
	}

	// Pass 1: objectType -> the component schema of one list result.
	itemOf := map[string]string{}
	// Pass 1b: component schema -> objectType. A schema reachable from two list
	// paths (netbox_topology_views declares one such placeholder) resolves to
	// the lexicographically smallest path so the index is deterministic.
	ownerOf := map[string]string{}
	for path, item := range doc.Paths {
		if item.Get == nil || strings.Contains(path, "{") {
			continue
		}
		idx := strings.Index(path, "/api/")
		if idx < 0 {
			continue
		}
		objectType := strings.TrimSuffix(path[idx+len("/api/"):], "/")
		if objectType == "" || !strings.Contains(objectType, "/") {
			continue
		}
		listName := schemaName(item.Get.Responses["200"].Content["application/json"].Schema.Ref)
		if listName == "" {
			continue
		}
		results, ok := doc.Components.Schemas[listName]
		if !ok {
			continue
		}
		itemName := results.Properties["results"].Items.Ref
		if itemName == "" {
			continue
		}
		itemName = schemaName(itemName)
		itemOf[objectType] = itemName
		if cur, ok := ownerOf[itemName]; !ok || objectType < cur {
			ownerOf[itemName] = objectType
		}
	}
	// The devices and virtual-machines list endpoints return
	// XWithConfigContext, so a BriefDevice reference resolves only with the
	// suffix stripped.
	for itemName, objectType := range ownerOf {
		if base := strings.TrimSuffix(itemName, "WithConfigContext"); base != itemName {
			if _, taken := ownerOf[base]; !taken {
				ownerOf[base] = objectType
			}
		}
	}

	// Pass 2: classify every property of every list endpoint's item schema.
	idx := make(dimIndex, len(itemOf))
	for objectType, itemName := range itemOf {
		item, ok := doc.Components.Schemas[itemName]
		if !ok {
			continue
		}
		props := make(map[string]dimension, len(item.Properties))
		for name, prop := range item.Properties {
			props[name] = classify(prop, ownerOf)
		}
		if len(props) > 0 {
			idx[objectType] = props
		}
	}
	return idx, nil
}

// classify decides how a single property can be enumerated.
func classify(prop dimProp, ownerOf map[string]string) dimension {
	if ref := prop.ref(); ref != "" {
		// "BriefSite"/"NestedSite" are serializer variants of "Site"; the list
		// endpoint is registered under the bare name.
		for _, candidate := range []string{ref, strings.TrimPrefix(strings.TrimPrefix(ref, "Brief"), "Nested")} {
			if endpoint, ok := ownerOf[candidate]; ok {
				return dimension{kind: dimRelated, endpoint: endpoint}
			}
		}
		return dimension{kind: dimNone}
	}
	values := prop.Properties.Value.Enum
	if len(values) == 0 {
		return dimension{kind: dimNone}
	}
	// value and label are two enums NetBox emits from ONE ordered list of
	// choices, so they correspond position by position. Equal length is the only
	// evidence of that available here; without it the labels are dropped rather
	// than risk pairing "active" with the wrong display text — flattenField then
	// falls back to showing the raw value, which is honest.
	labels := prop.Properties.Label.Enum
	if len(labels) != len(values) {
		labels = nil
	}
	choices := make([]map[string]interface{}, 0, len(values))
	for i, v := range values {
		c := map[string]interface{}{"value": v}
		if labels != nil {
			c["label"] = labels[i]
		}
		choices = append(choices, c)
	}
	return dimension{kind: dimChoice, choices: choices}
}

// columnSuffixes are the suffixes flattenObject appends when it expands a
// nested object, a choice, or a list into extra columns. A column ending in one
// of them derives from the property named by the remaining prefix.
var columnSuffixes = []string{"_id", "_slug", "_value", "_count"}

// resolve maps a FLATTENED COLUMN name back to the object-type property it came
// from, and to that property's dimension.
//
// The two namespaces matter here. `site`, `site_id` and `site_slug` are three
// columns produced by flattening ONE property, `site`; `cf_role` is one of many
// columns hoisted out of `custom_fields`. base is what `?fields=` must be given
// to fetch the column; ok is false when the column cannot be traced to a
// property at all (an unknown object type, a synthetic column such as the
// utilization metrics, or a schema we could not read), in which case the caller
// must not project and must not consult the dimension.
func (idx dimIndex) resolve(objectType, field string) (dim dimension, base string, ok bool) {
	props, known := idx[objectType]
	if !known {
		return dimension{}, "", false
	}
	if d, ok := props[field]; ok {
		return d, field, true
	}
	for _, suffix := range columnSuffixes {
		if !strings.HasSuffix(field, suffix) {
			continue
		}
		if d, ok := props[strings.TrimSuffix(field, suffix)]; ok {
			return d, strings.TrimSuffix(field, suffix), true
		}
	}
	if strings.HasPrefix(field, "cf_") {
		if _, ok := props["custom_fields"]; ok {
			return dimension{kind: dimNone}, "custom_fields", true
		}
	}
	return dimension{}, "", false
}

// valueSet collects distinct rendered column values in first-seen order,
// applying the caller's substring filter and cap, then sorts.
//
// It is shared by every FieldValues path so that a value looks the same however
// it was obtained: same nil-skip, same fmt rendering, same case-insensitive
// "contains" test, same cap, same final sort.
type valueSet struct {
	q     string
	limit int
	seen  map[string]bool
	out   []string
}

func newValueSet(q string, limit int) *valueSet {
	return &valueSet{q: strings.ToLower(q), limit: limit, seen: map[string]bool{}}
}

// add records the value of field within one flattened row. It reports whether
// the set is now full, so callers can stop fetching.
func (s *valueSet) add(vals map[string]interface{}, field string) (full bool) {
	v, ok := vals[field]
	if !ok || v == nil {
		return false
	}
	str := renderValue(v)
	if str == "" || s.seen[str] {
		return false
	}
	if s.q != "" && !strings.Contains(strings.ToLower(str), s.q) {
		return false
	}
	s.seen[str] = true
	s.out = append(s.out, str)
	return len(s.out) >= s.limit
}

// renderValue is how a flattened cell becomes a dropdown string. Kept as a
// named function because every FieldValues path must render identically —
// notably JSON numbers, which arrive as float64 and must print as "17", not
// "17.000000".
func renderValue(v interface{}) string { return fmt.Sprintf("%v", v) }

// sorted returns the collected values in ascending order. A nil slice (nothing
// collected) is returned as nil, which the resource layer renders as JSON null —
// the shape callers already receive today.
func (s *valueSet) sorted() []string {
	sort.Strings(s.out)
	return s.out
}

// flattenValues renders one dimension object (or synthetic choice) through the
// SAME flattening the query path uses, so that enumerating a dimension produces
// byte-identical strings to sampling the fact table: the site column is the
// site's display value, site_id its numeric id, site_slug its slug, a choice
// column its label and <choice>_value its raw value.
func flattenValues(base string, obj interface{}) map[string]interface{} {
	vals := map[string]interface{}{}
	flattenField(base, obj, func(name string, v interface{}) { vals[name] = v })
	return vals
}
