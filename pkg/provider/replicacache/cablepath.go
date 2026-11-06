package replicacache

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// pathNode is one node of a NetBox cable path: the content type of the object
// the path passes and its id, as NetBox writes them ("9:1").
type pathNode struct{ TypeID, ObjectID int }

// errMalformedPath is a dcim_cablepath.path that is not a list of steps of
// "<type>:<id>" nodes.
var errMalformedPath = errors.New("cable path is not a list of steps of type:id nodes")

// parseCablePath reads dcim_cablepath.path: the steps a path takes, each the
// list of nodes it passes at once — several where a cable has several
// terminations on an end. replica-cache v1.40.1 serves the column as a JSON
// string holding PostgreSQL's jsonb text ("[[\"9:1\"], [\"53:1\"], [\"9:2\"]]",
// measured on the local replica). Read defensively as well: the same value
// already decoded into nested arrays, and goavro's union spelling
// ({"string": …}), which rows written before replica-cache v1.35.5 may keep.
//
// Anything else is refused, not read as a shorter path: a path read wrong puts
// a link on the wrong device, or drops it, with nothing to show for it.
func parseCablePath(v any) ([][]pathNode, error) {
	v, err := unwrapPathValue(v)
	if err != nil {
		return nil, err
	}
	if s, ok := v.(string); ok {
		var decoded any
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &decoded); err != nil {
			return nil, fmt.Errorf("%w: %v", errMalformedPath, err)
		}
		if v, err = unwrapPathValue(decoded); err != nil {
			return nil, err
		}
	}
	steps, ok := v.([]any)
	if !ok || len(steps) == 0 {
		return nil, errMalformedPath
	}
	out := make([][]pathNode, 0, len(steps))
	for _, s := range steps {
		s, err := unwrapPathValue(s)
		if err != nil {
			return nil, err
		}
		nodes, ok := s.([]any)
		if !ok || len(nodes) == 0 {
			return nil, errMalformedPath
		}
		step := make([]pathNode, 0, len(nodes))
		for _, n := range nodes {
			n, err := unwrapPathValue(n)
			if err != nil {
				return nil, err
			}
			text, ok := n.(string)
			if !ok {
				return nil, errMalformedPath
			}
			node, err := parsePathNode(text)
			if err != nil {
				return nil, err
			}
			step = append(step, node)
		}
		out = append(out, step)
	}
	return out, nil
}

// unwrapPathValue takes a value out of goavro's union spelling, {"string": v}
// or {"array": v}; any other object is not part of a path.
func unwrapPathValue(v any) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return v, nil
	}
	if len(m) == 1 {
		for _, key := range []string{"string", "array"} {
			if inner, ok := m[key]; ok {
				return inner, nil
			}
		}
	}
	return nil, errMalformedPath
}

// parsePathNode reads "<type>:<id>": two ids, each positive and exact.
func parsePathNode(s string) (pathNode, error) {
	typeText, idText, ok := strings.Cut(s, ":")
	if !ok {
		return pathNode{}, errMalformedPath
	}
	typeID, err1 := strconv.Atoi(typeText)
	objectID, err2 := strconv.Atoi(idText)
	if err1 != nil || err2 != nil || typeID <= 0 || objectID <= 0 || objectID >= maxExactID {
		return pathNode{}, errMalformedPath
	}
	return pathNode{TypeID: typeID, ObjectID: objectID}, nil
}
