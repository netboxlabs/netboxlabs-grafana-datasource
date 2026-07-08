package netbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// flattenObject converts a single NetBox object (raw JSON) into a flat set of
// columns suitable for a joinable Grafana table. It returns the column names in
// the object's natural key order plus a value map.
//
// Flattening rules:
//   - scalars (string/number/bool/null) pass through under their own key;
//   - nested object references (with an "id") become <key> (best display value),
//     plus <key>_id and, when present, <key>_slug;
//   - choice objects ({value,label}) become <key> (label) plus <key>_value;
//   - "custom_fields" are hoisted to cf_<name> columns;
//   - lists become a "; "-joined <key> plus a numeric <key>_count.
//
// The rules are intentionally generic so plugin-provided models work without any
// model-specific code.
func flattenObject(raw json.RawMessage) ([]string, map[string]interface{}, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("unmarshal object: %w", err)
	}
	keys, err := orderedKeys(raw)
	if err != nil {
		// Fall back to unordered keys; correctness over presentation.
		keys = keys[:0]
		for k := range obj {
			keys = append(keys, k)
		}
	}

	cols := make([]string, 0, len(keys)+4)
	vals := make(map[string]interface{}, len(keys)+4)
	add := func(name string, v interface{}) {
		if _, exists := vals[name]; !exists {
			cols = append(cols, name)
		}
		vals[name] = v
	}

	for _, k := range keys {
		flattenField(k, obj[k], add)
	}
	return cols, vals, nil
}

func flattenField(key string, v interface{}, add func(name string, v interface{})) {
	switch val := v.(type) {
	case nil:
		add(key, nil)
	case bool, float64, string:
		add(key, val)
	case map[string]interface{}:
		if key == "custom_fields" {
			for _, ck := range sortedKeys(val) {
				flattenField("cf_"+ck, val[ck], add)
			}
			return
		}
		add(key, nestedDisplay(val))
		if id, ok := val["id"]; ok {
			add(key+"_id", id)
		} else if _, hasVal := val["value"]; hasVal {
			// Choice field: expose the raw value alongside the label.
			add(key+"_value", val["value"])
		}
		if slug, ok := val["slug"].(string); ok && slug != "" {
			add(key+"_slug", slug)
		}
	case []interface{}:
		if len(val) == 0 {
			add(key, "")
			add(key+"_count", float64(0))
			return
		}
		parts := make([]string, 0, len(val))
		for _, el := range val {
			if m, ok := el.(map[string]interface{}); ok {
				parts = append(parts, nestedDisplay(m))
			} else {
				parts = append(parts, fmt.Sprintf("%v", el))
			}
		}
		add(key, strings.Join(parts, "; "))
		add(key+"_count", float64(len(val)))
	default:
		b, _ := json.Marshal(v)
		add(key, string(b))
	}
}

// nestedDisplay picks the most human-friendly string from a nested object or
// choice field.
func nestedDisplay(m map[string]interface{}) string {
	for _, k := range []string{"display", "name", "label", "address", "prefix", "cid", "model", "rgb"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	if v, ok := m["value"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// orderedKeys returns the top-level object keys of a JSON document in document
// order.
func orderedKeys(raw []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected JSON object")
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		keys = append(keys, key)
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func skipValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return nil // scalar already consumed
	}
	switch d {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil { // key
				return err
			}
			if err := skipValue(dec); err != nil { // value
				return err
			}
		}
		_, err = dec.Token() // closing }
		return err
	case '[':
		for dec.More() {
			if err := skipValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token() // closing ]
		return err
	}
	return nil
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// stable, deterministic column order for custom fields
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
