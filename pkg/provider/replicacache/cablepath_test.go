package replicacache

import (
	"reflect"
	"testing"
)

// The shape replica-cache v1.40.1 serves dcim_cablepath.path in, measured on
// the local replica: PostgreSQL's jsonb text of the nested arrays, as a JSON
// string. Every step is a list of "<content type id>:<object id>" nodes.
func TestParseCablePath_JSONString(t *testing.T) {
	got, err := parseCablePath("[[\"9:1\"], [\"53:1\"], [\"9:2\"]]")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]pathNode{{{9, 1}}, {{53, 1}}, {{9, 2}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// A breakout's far end is one step of several nodes, in NetBox's order.
	got, err = parseCablePath(`[["9:36"], ["53:12"], ["9:41", "9:42"]]`)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]pathNode{{{9, 36}}, {{53, 12}}, {{9, 41}, {9, 42}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Defensively, the same path already decoded: a service that sent the jsonb
// value as JSON rather than as its text would be giving the same thing.
func TestParseCablePath_NestedArrays(t *testing.T) {
	got, err := parseCablePath([]any{[]any{"9:1"}, []any{"53:1"}, []any{"9:2"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]pathNode{{{9, 1}}, {{53, 1}}, {{9, 2}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Rows written before replica-cache v1.35.5 may keep goavro's union spelling
// ({"string": …}) on array items; read through, not refused.
func TestParseCablePath_AvroUnionWrapped(t *testing.T) {
	want := [][]pathNode{{{9, 1}}, {{53, 1}}, {{9, 2}}}
	for _, in := range []any{
		`[[{"string":"9:1"}], [{"string":"53:1"}], [{"string":"9:2"}]]`,
		[]any{[]any{map[string]any{"string": "9:1"}}, []any{map[string]any{"string": "53:1"}}, []any{map[string]any{"string": "9:2"}}},
		map[string]any{"string": `[["9:1"], ["53:1"], ["9:2"]]`},
	} {
		got, err := parseCablePath(in)
		if err != nil {
			t.Errorf("%v: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%v: got %v, want %v", in, got, want)
		}
	}
}

// Anything else is not a path: refused rather than read as a shorter one,
// which would put a link on the wrong device or drop it unseen.
func TestParseCablePath_Malformed(t *testing.T) {
	for _, in := range []any{
		nil,
		"",
		"not json",
		`{"a":1}`,
		`[]`,
		`[[]]`,
		`["9:1"]`,
		`[["9"]]`,
		`[["9:x"]]`,
		`[["0:1"]]`,
		`[["9:-1"]]`,
		`[["9:1:2"]]`,
		`[[9]]`,
		`[[null]]`,
		`[["9:1"], null]`,
		`[["9:99999999999999999999"]]`,
		42.0,
		map[string]any{"int": "9:1"},
	} {
		if got, err := parseCablePath(in); err == nil {
			t.Errorf("%#v: got %v, want an error", in, got)
		}
	}
}
