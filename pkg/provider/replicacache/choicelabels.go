package replicacache

import (
	_ "embed"
	"encoding/json"
	"strconv"
	"sync"
)

// choiceLabelsJSON is object type -> column -> stored value -> label, generated
// from NetBox's own API schemas by internal/genlabels. The replica stores a
// choice as its value ("active") and publishes no labels, while NetBox mode
// shows the label ("Active"); this is what lets the same panel read the same
// text from either backend.
//
// It reflects NetBox's built-in choices. An instance that overrides a choice
// list with FIELD_CHOICES shows its own labels in NetBox mode, and its extra
// values arrive here unlabelled.
//
//go:embed choicelabels.json
var choiceLabelsJSON []byte

// loadChoiceLabels decodes the map once, on the first query that needs it. A
// map that does not decode leaves every value as stored; the embedded-map test
// is what keeps that from shipping.
var loadChoiceLabels = sync.OnceValue(func() map[string]map[string]map[string]string {
	var m map[string]map[string]map[string]string
	if json.Unmarshal(choiceLabelsJSON, &m) != nil {
		return nil
	}
	return m
})

// choiceLabel returns NetBox's label for a stored choice value.
func choiceLabel(objectType, column, value string) (string, bool) {
	label, ok := loadChoiceLabels()[objectType][column][value]
	return label, ok
}

// applyChoiceLabels shows each choice column the way NetBox mode does: the
// label in place of the stored value. It runs after addChoiceValueAliases, so
// a requested <field>_value keeps the raw value, as NetBox mode's does.
//
// A value the map does not know stays as stored. A numeric choice (an IP
// family, a rack width) is written as text either way, so the column has one
// type whichever values a refresh happens to return.
func applyChoiceLabels(objectType string, rows []map[string]interface{}) {
	cols := loadChoiceLabels()[objectType]
	if len(cols) == 0 {
		return
	}
	for _, row := range rows {
		for col, labels := range cols {
			var value string
			switch v := row[col].(type) {
			case string:
				value = v
			case float64:
				value = strconv.FormatFloat(v, 'f', -1, 64)
				row[col] = value
			default:
				continue
			}
			if label, ok := labels[value]; ok {
				row[col] = label
			}
		}
	}
}
