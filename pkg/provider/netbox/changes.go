package netbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// objectChange mirrors the relevant fields of a NetBox change-log record
// (/api/core/object-changes/).
type objectChange struct {
	ID              int    `json:"id"`
	Time            string `json:"time"`
	UserName        string `json:"user_name"`
	Action          choice `json:"action"`
	ChangedType     string `json:"changed_object_type"`
	ChangedObjectID int64  `json:"changed_object_id"`
	ObjectRepr      string `json:"object_repr"`
	DisplayURL      string `json:"display_url"`
	Message         string `json:"message"`
	// ChangedObject is an object, a string (the repr, on NetBox 4.2-4.4 for models
	// without a serializer) or null, so it is decoded lazily where it is read.
	ChangedObject  json.RawMessage            `json:"changed_object"`
	PrechangeData  map[string]json.RawMessage `json:"prechange_data"`
	PostchangeData map[string]json.RawMessage `json:"postchange_data"`
}

type choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (o objectChange) changedType() string { return o.ChangedType }

func (o objectChange) actionLabel() string {
	if o.Action.Label != "" {
		return o.Action.Label
	}
	return o.Action.Value
}

func (o objectChange) userName() string { return o.UserName }

// changedObjectMap returns changed_object when it is an object, else nil.
func (o objectChange) changedObjectMap() map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if len(o.ChangedObject) == 0 || o.ChangedObject[0] != '{' {
		return nil
	}
	if err := json.Unmarshal(o.ChangedObject, &m); err != nil {
		return nil
	}
	return m
}

// deepLink prefers the changed object's UI page, then the change-log entry page,
// then the instance root.
func (o objectChange) deepLink(base string) string {
	if raw, ok := o.changedObjectMap()["display_url"]; ok {
		var u string
		if json.Unmarshal(raw, &u) == nil && u != "" {
			return u
		}
	}
	if o.DisplayURL != "" {
		return o.DisplayURL
	}
	return base
}

// title names the changed object. NetBox only returns object_repr from 4.5, so
// older versions fall back through what the record does carry.
func (o objectChange) title() string {
	if o.ObjectRepr != "" {
		return o.ObjectRepr
	}
	if len(o.ChangedObject) > 0 && o.ChangedObject[0] == '"' {
		var s string
		if json.Unmarshal(o.ChangedObject, &s) == nil && s != "" {
			return s
		}
	}
	if raw, ok := o.changedObjectMap()["display"]; ok {
		if s := formatValue(raw); s != "" {
			return s
		}
	}
	field := lookupField(o.ChangedType)
	for _, data := range []map[string]json.RawMessage{o.PostchangeData, o.PrechangeData} {
		if s := formatValue(data[field]); s != "" {
			return s
		}
	}
	return fmt.Sprintf("%s #%d", o.ChangedType, o.ChangedObjectID)
}

// rawFieldChange is one changed field with its undecoded before and after values.
type rawFieldChange struct {
	Field         string
	Before, After json.RawMessage
}

// decodedChange is a change-log row plus its field-level diff.
type decodedChange struct {
	oc   objectChange
	diff []rawFieldChange
}

// decodeChange parses one change-log row. A row that is not valid JSON is skipped.
func decodeChange(raw json.RawMessage) (decodedChange, bool) {
	var oc objectChange
	if err := json.Unmarshal(raw, &oc); err != nil {
		return decodedChange{}, false
	}
	return decodedChange{oc: oc, diff: changeDiff(oc)}, true
}

// canonicalJSON re-encodes a value with sorted keys and normalised numbers, so
// 1 and 1.0 and differently ordered objects compare equal. Missing is null.
func canonicalJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "null"
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// changeDiff lists the fields an update changed. The change log's pre/post data
// is already stripped of fields the viewer may not see, server side.
func changeDiff(o objectChange) []rawFieldChange {
	if o.Action.Value != "update" {
		return nil
	}
	var out []rawFieldChange
	add := func(field string, before, after json.RawMessage) {
		if canonicalJSON(before) != canonicalJSON(after) {
			out = append(out, rawFieldChange{Field: field, Before: before, After: after})
		}
	}
	for key := range unionKeys(o.PrechangeData, o.PostchangeData) {
		if strings.HasPrefix(key, "_") || key == "created" || key == "last_updated" {
			continue
		}
		if key == "custom_fields" {
			var pre, post map[string]json.RawMessage
			_ = json.Unmarshal(o.PrechangeData[key], &pre)
			_ = json.Unmarshal(o.PostchangeData[key], &post)
			for name := range unionKeys(pre, post) {
				add("cf_"+name, pre[name], post[name])
			}
			continue
		}
		add(key, o.PrechangeData[key], o.PostchangeData[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

func unionKeys(a, b map[string]json.RawMessage) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// formatValue renders a JSON value for display.
func formatValue(v json.RawMessage) string {
	if len(bytes.TrimSpace(v)) == 0 {
		return ""
	}
	var x interface{}
	if err := json.Unmarshal(v, &x); err != nil {
		return string(v)
	}
	switch t := x.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []interface{}:
		var elems []json.RawMessage
		_ = json.Unmarshal(v, &elems)
		parts := make([]string, len(elems))
		for i, e := range elems {
			parts[i] = formatValue(e)
		}
		return strings.Join(parts, ", ")
	default:
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return string(v)
		}
		return buf.String()
	}
}

// toChange builds the provider row. format renders a diff value for display.
func (d decodedChange) toChange(base string, format func(field string, v json.RawMessage) string) provider.Change {
	t, _ := time.Parse(time.RFC3339, d.oc.Time)
	c := provider.Change{
		Time:       t,
		Action:     d.oc.actionLabel(),
		ObjectType: d.oc.changedType(),
		ObjectRepr: d.oc.title(),
		User:       d.oc.userName(),
		URL:        d.oc.deepLink(base),
		Kind:       provider.ChangeKindChange,
		Message:    d.oc.Message,
	}
	for _, f := range d.diff {
		c.Diff = append(c.Diff, provider.FieldChange{
			Field:  f.Field,
			Before: format(f.Field, f.Before),
			After:  format(f.Field, f.After),
		})
	}
	return c
}
