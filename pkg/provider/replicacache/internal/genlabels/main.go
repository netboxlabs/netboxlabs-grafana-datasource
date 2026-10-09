// Command genlabels builds choicelabels.json, the value -> label map that lets
// replica-cache mode show a choice the way NetBox mode does ("Active" for
// "active").
//
// It reads NetBox OpenAPI schemas (GET /api/schema/?format=json), oldest
// version first, and writes object type -> column -> value -> label. A later
// schema's label wins, and values only an older version has are kept, so the
// map covers every supported NetBox version. Regenerate it when the supported
// range moves:
//
//	go run ./pkg/provider/replicacache/internal/genlabels \
//	  schema-4.2.json schema-4.4.json schema-4.6.json \
//	  > pkg/provider/replicacache/choicelabels.json
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type schemaDoc struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

// node is the part of an OpenAPI schema object this command reads.
type node struct {
	Ref        string                     `json:"$ref"`
	Properties map[string]json.RawMessage `json:"properties"`
	Items      json.RawMessage            `json:"items"`
	AllOf      []json.RawMessage          `json:"allOf"`
	OneOf      []json.RawMessage          `json:"oneOf"`
	AnyOf      []json.RawMessage          `json:"anyOf"`
	Enum       []any                      `json:"enum"`
}

type labels map[string]map[string]map[string]string

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: genlabels schema.json [schema.json ...]  (oldest NetBox version first)")
		os.Exit(2)
	}
	out := labels{}
	for _, path := range os.Args[1:] {
		raw, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		var doc schemaDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			fail(fmt.Errorf("%s: %w", path, err))
		}
		merge(out, collect(doc))
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(out); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "genlabels:", err)
	os.Exit(1)
}

func merge(into, from labels) {
	for objectType, cols := range from {
		if into[objectType] == nil {
			into[objectType] = map[string]map[string]string{}
		}
		for col, values := range cols {
			if into[objectType][col] == nil {
				into[objectType][col] = map[string]string{}
			}
			for v, l := range values {
				into[objectType][col][v] = l
			}
		}
	}
}

// collect maps every list endpoint (/api/<app>/<model>/) to the choice fields
// of the object its GET returns.
func collect(doc schemaDoc) labels {
	out := labels{}
	for path, ops := range doc.Paths {
		if !strings.HasPrefix(path, "/api/") || !strings.HasSuffix(path, "/") || strings.Contains(path, "{") {
			continue
		}
		objectType := strings.Trim(strings.TrimPrefix(path, "/api/"), "/")
		// Plugin endpoints depend on what the instance that produced the schema
		// had installed, so the map keeps to NetBox's own models.
		if strings.Count(objectType, "/") < 1 || strings.HasPrefix(objectType, "plugins/") {
			continue
		}
		get, ok := ops["get"]
		if !ok {
			continue
		}
		item := listItemSchema(doc, get)
		if item == nil {
			continue
		}
		cols := map[string]map[string]string{}
		for name, prop := range item.Properties {
			if values := choiceLabels(doc, prop); len(values) > 0 {
				cols[name] = values
			}
		}
		if len(cols) > 0 {
			out[objectType] = cols
		}
	}
	return out
}

// listItemSchema follows GET -> 200 -> application/json -> Paginated<X>List ->
// results -> items to the object schema.
func listItemSchema(doc schemaDoc, getOp json.RawMessage) *node {
	var op struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	}
	if json.Unmarshal(getOp, &op) != nil {
		return nil
	}
	body, ok := op.Responses["200"].Content["application/json"]
	if !ok {
		return nil
	}
	page := resolve(doc, body.Schema)
	if page == nil || page.Properties["results"] == nil {
		return nil
	}
	results := resolve(doc, page.Properties["results"])
	if results == nil || results.Items == nil {
		return nil
	}
	return resolve(doc, results.Items)
}

// choiceLabels returns value -> label when prop is a NetBox choice object:
// {value: {enum: [...]}, label: {enum: [...]}}, the two enums in matching order.
// A nullable choice arrives wrapped in allOf/oneOf/anyOf, so those are searched.
func choiceLabels(doc schemaDoc, prop json.RawMessage) map[string]string {
	n := resolve(doc, prop)
	if n == nil {
		return nil
	}
	if v, l := n.Properties["value"], n.Properties["label"]; v != nil && l != nil {
		values, labels := resolve(doc, v), resolve(doc, l)
		if values == nil || labels == nil {
			return nil
		}
		// Values and labels pair by position. "No choice" appears among the
		// values as null (with a "---------" label) or as a trailing "" with no
		// label at all, so it is skipped, and a "" without a label is dropped
		// before pairing.
		vs, ls := values.Enum, labels.Enum
		if len(vs) != len(ls) {
			vs = withoutBlank(vs)
		}
		if len(vs) == 0 || len(vs) != len(ls) {
			return nil
		}
		out := map[string]string{}
		for i, v := range vs {
			value, ok := valueText(v)
			label, okL := ls[i].(string)
			if ok && okL {
				out[value] = label
			}
		}
		return out
	}
	for _, group := range [][]json.RawMessage{n.AllOf, n.OneOf, n.AnyOf} {
		for _, sub := range group {
			if out := choiceLabels(doc, sub); len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func withoutBlank(enum []any) []any {
	var out []any
	for _, e := range enum {
		if e != nil && e != "" {
			out = append(out, e)
		}
	}
	return out
}

// valueText is a choice value as the replica's text column or number column
// holds it: a string as is, a number in its shortest decimal form (4, 9600).
// Null and "" are "no choice" and have no label.
func valueText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	}
	return "", false
}

// resolve decodes a schema object, following one or more $refs into
// components/schemas.
func resolve(doc schemaDoc, raw json.RawMessage) *node {
	for range 10 {
		var n node
		if json.Unmarshal(raw, &n) != nil {
			return nil
		}
		if n.Ref == "" {
			return &n
		}
		name := strings.TrimPrefix(n.Ref, "#/components/schemas/")
		next, ok := doc.Components.Schemas[name]
		if !ok {
			return nil
		}
		raw = next
	}
	return nil
}
