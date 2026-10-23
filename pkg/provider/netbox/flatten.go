package netbox

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
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

// flattenField delegates to the shared contract in pkg/provider. It lives
// there because a panel saved against one backend and pointed at another has to
// keep its columns, which only holds if both expand a field the same way.
func flattenField(key string, v interface{}, add func(name string, v interface{})) {
	provider.FlattenField(key, v, add)
}

func nestedDisplay(m map[string]interface{}) string { return provider.NestedDisplay(m) }

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
