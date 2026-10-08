package replicacache

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
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
// With withValues — an "All columns" query, which asks for no alias by name —
// every labelled column also gets its <field>_value, because NetBox mode
// returns both halves of a choice there. The added columns are returned.
//
// A value the map does not know stays as stored. A numeric choice (an IP
// family, a rack width) is written as text either way, so the column has one
// type whichever values a refresh happens to return.
func applyChoiceLabels(objectType string, rows []map[string]interface{}, withValues bool) []string {
	cols := loadChoiceLabels()[objectType]
	if len(cols) == 0 {
		return nil
	}
	var added []string
	for _, col := range slices.Sorted(maps.Keys(cols)) {
		labels := cols[col]
		alias := col + "_value"
		addAlias := withValues && hasColumn(rows, col) && !hasColumn(rows, alias)
		for _, row := range rows {
			raw, present := row[col]
			if addAlias && present {
				row[alias] = raw
			}
			var value string
			switch v := raw.(type) {
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
		if addAlias {
			added = append(added, alias)
		}
	}
	return added
}

// validateChoiceValues refuses an equality filter on a choice column that
// names a LABEL rather than a stored value. Rows show labels, so a value
// picked from a table cell or a variable built on the column is one; NetBox
// answers it with "Select a valid choice", and sent here it would match no row
// — or, negated, every row — with nothing to say why. A value the map does not
// know (an instance's own FIELD_CHOICES) is sent as given.
func validateChoiceValues(objectType string, filters []provider.Filter) error {
	cols := loadChoiceLabels()[objectType]
	for _, f := range filters {
		labels, ok := cols[f.Field]
		if !ok || !equalityOperator(f.Operator) {
			continue
		}
		for _, v := range splitValues(f.Value) {
			if _, stored := labels[v]; stored {
				continue
			}
			var values []string
			for value, label := range labels {
				if label == v {
					values = append(values, strconv.Quote(value))
				}
			}
			if len(values) > 0 {
				slices.Sort(values)
				return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator,
					Reason: fmt.Sprintf("%q is how the rows show %s, but filters take the stored value (%s); %s_value shows the stored values",
						v, strings.Join(values, " or "), strings.Join(values, ", "), f.Field)}
			}
		}
	}
	return nil
}

// equalityOperator reports whether op compares whole stored values: the exact
// match (whose spellings are "" and "exact").
func equalityOperator(op string) bool {
	return op == opExact || op == "exact"
}
