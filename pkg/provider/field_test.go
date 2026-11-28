package provider

import (
	"encoding/json"
	"testing"
)

// Sortable is replica-cache's to set. NetBox mode never does, and its /fields
// answer must stay the two keys it has always had.
func TestFieldJSON_SortableOnlyWhenSet(t *testing.T) {
	plain, err := json.Marshal(Field{Name: "name", Type: FieldTypeString})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(plain), `{"name":"name","type":"string"}`; got != want {
		t.Errorf("a field with no sortable flag = %s, want %s", got, want)
	}
	sortable, err := json.Marshal(Field{Name: "name", Type: FieldTypeString, Sortable: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(sortable), `{"name":"name","type":"string","sortable":true}`; got != want {
		t.Errorf("a sortable field = %s, want %s", got, want)
	}
}
