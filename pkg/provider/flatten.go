package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DisplayFields is the order in which a nested object's human-readable label is
// looked for. Shared so that every backend renders the same object the same
// way: a device-type shows its model, an IP its address, a circuit its cid.
var DisplayFields = []string{"display", "name", "label", "address", "prefix", "cid", "model", "rgb"}

// FlattenField expands one field into the columns a panel sees, calling add for
// each. It is the flattening CONTRACT, shared by every backend, because a panel
// saved against one and pointed at another has to keep its columns:
//
//   - scalars (string/number/bool/null) pass through under their own key;
//   - nested object references (with an "id") become <key> (best display value),
//     plus <key>_id and, when present, <key>_slug;
//   - choice objects ({value,label}) become <key> (label) plus <key>_value;
//   - "custom_fields" are hoisted to cf_<name> columns, each flattened by these
//     same rules;
//   - lists become a "; "-joined <key> plus a numeric <key>_count.
//
// The rules are intentionally generic so plugin-provided models work without
// any model-specific code.
func FlattenField(key string, v interface{}, add func(name string, v interface{})) {
	switch val := v.(type) {
	case nil:
		add(key, nil)
	case bool, float64, string:
		add(key, val)
	case map[string]interface{}:
		if key == "custom_fields" {
			for _, ck := range sortedKeys(val) {
				FlattenField("cf_"+ck, val[ck], add)
			}
			return
		}
		add(key, NestedDisplay(val))
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
				parts = append(parts, NestedDisplay(m))
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

// NestedDisplay picks the most human-friendly string from a nested object or
// choice field.
func NestedDisplay(m map[string]interface{}) string {
	for _, k := range DisplayFields {
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

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable, deterministic column order for custom fields
	return keys
}
