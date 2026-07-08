package netbox

// objectChange mirrors the relevant fields of a NetBox change-log record
// (/api/core/object-changes/).
type objectChange struct {
	Time          string                 `json:"time"`
	UserName      string                 `json:"user_name"`
	Action        choice                 `json:"action"`
	ChangedType   string                 `json:"changed_object_type"`
	ObjectRepr    string                 `json:"object_repr"`
	DisplayURL    string                 `json:"display_url"`
	ChangedObject map[string]interface{} `json:"changed_object"`
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

// deepLink prefers the changed object's UI page, then the change-log entry page,
// then the instance root.
func (o objectChange) deepLink(base string) string {
	if o.ChangedObject != nil {
		if u, ok := o.ChangedObject["display_url"].(string); ok && u != "" {
			return u
		}
	}
	if o.DisplayURL != "" {
		return o.DisplayURL
	}
	return base
}
