package netbox

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// tsDeclaration returns the body of a top-level `export const <name>` in
// src/types.ts, from the marker to the line that closes it.
//
// Narrowing to the one declaration is the point rather than an optimisation:
// src/types.ts is full of unrelated string literals — filter operators, join
// transforms, other allow-lists — and a regex run over the whole file would pull
// them into the comparison and turn a guard into a coin toss.
//
// The Go side does the reading because it is the side that can do it for free:
// reading a file from Jest needs Node's fs typings, which this project's tsconfig
// does not include.
func tsDeclaration(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "src", "types.ts"))
	if err != nil {
		t.Fatalf("read src/types.ts: %v", err)
	}
	start := strings.Index(string(src), "export const "+name)
	if start < 0 {
		t.Fatalf("%s not found in src/types.ts — update this guard, do not delete it", name)
	}
	rest := string(src)[start:]

	// A declaration here ends at the first line that closes it: `];` for the
	// array constants, `};` for the keyed record. Taking whichever comes first
	// keeps one helper honest for both shapes.
	end := -1
	for _, closer := range []string{"\n];", "\n};"} {
		if i := strings.Index(rest, closer); i >= 0 && (end < 0 || i < end) {
			end = i
		}
	}
	if end < 0 {
		t.Fatalf("could not find the end of %s in src/types.ts — update this guard, do not delete it", name)
	}
	return rest[:end]
}

// tsOrderingEntry matches one entry of ORDERING_FIELDS — a quoted "app/model"
// key mapped to a literal array of bare field names:
//
//	'dcim/sites': ['id', 'name', 'slug', 'region', 'facility'],
//
// (?s) so an array long enough for prettier to wrap it across several lines
// still matches as a single entry. The key takes ANY number of slash-separated
// segments, not just two, so that a plugin model ('plugins/bgp/bgp-sessions')
// added to the picker one day is compared rather than quietly skipped — a key
// this regex cannot see would be reported as missing from src/types.ts while
// sitting in plain view in it.
var tsOrderingEntry = regexp.MustCompile(`(?s)'([a-z0-9-]+(?:/[a-z0-9-]+)+)':\s*\[([^\]]*)\]`)

// TestOrderingFieldsMatchFrontend is the mechanical half of the "MUST be changed
// together" comments that sit on both orderingFields (ordering.go) and
// ORDERING_FIELDS (src/types.ts). It is the same guard, and the same reasoning,
// as TestIPContextFieldsMatchFrontend in ipenrich_test.go: one list, two
// languages, nothing but a comment holding them together.
//
// Drift is invisible in BOTH directions without this, because neither side's own
// suite compares itself to the other. The Go suite never reads the editor's list
// at all, so a field added here alone stays green. src/ordering.test.ts spells
// out exactly one model's array (dcim/devices, via orderingFieldsFor) and
// otherwise pins only the map's keys, the three exclusions, the presence of `id`
// and the shape of each name — so a field added to the editor's dcim/sites,
// dcim/interfaces, ipam/*, or virtualization/* entry leaves the whole TS suite
// green. Even on dcim/devices it compares TypeScript to a TypeScript literal, so
// updating that expectation alongside the list — the obvious thing to do when it
// fails — drifts from Go without a word.
//
// What the user gets from each direction:
//
//   - TS only: the "Sort by" picker offers the field, orderingValue refuses to
//     send it, and applyOrdering returns the INFO note saying this data source
//     "only sorts <model> on the fields NetBox is known to accept" — about a
//     field the editor offered them one click earlier.
//   - Go only: the backend would have sorted on it and the picker never offers
//     it, so the field is unreachable outside a hand-written dashboard JSON.
//
// Field ORDER is deliberately not compared. The picker renders the TS array in
// order and the Go slice is only ever membership-tested by orderingValue, so a
// reordering changes what the user reads first and nothing about what works. The
// SET is what the two sides have to agree on.
func TestOrderingFieldsMatchFrontend(t *testing.T) {
	decl := tsDeclaration(t, "ORDERING_FIELDS")

	ts := map[string][]string{}
	for _, m := range tsOrderingEntry.FindAllStringSubmatch(decl, -1) {
		var fields []string
		for _, q := range tsQuoted.FindAllStringSubmatch(m[2], -1) {
			fields = append(fields, q[1])
		}
		ts[m[1]] = fields
	}
	// A parse that silently returns nothing would make this guard pass forever.
	if len(ts) == 0 {
		t.Fatalf("parsed no object types out of ORDERING_FIELDS in src/types.ts — update this guard, do not delete it")
	}

	// Whole models first, both directions: the coarsest drift is a type that one
	// side will sort and the other has never heard of.
	for _, objectType := range slices.Sorted(maps.Keys(orderingFields)) {
		if _, ok := ts[objectType]; !ok {
			t.Errorf("%s: sortable in orderingFields (pkg/provider/netbox/ordering.go) but missing from ORDERING_FIELDS (src/types.ts) — the editor shows no sort control for it at all",
				objectType)
		}
	}
	for _, objectType := range slices.Sorted(maps.Keys(ts)) {
		if _, ok := orderingFields[objectType]; !ok {
			t.Errorf("%s: sortable in ORDERING_FIELDS (src/types.ts) but missing from orderingFields (pkg/provider/netbox/ordering.go) — every field the picker offers for it is dropped, and every sort answers with the INFO note",
				objectType)
		}
	}

	// Then field by field, naming the model and the field so the fix is the one
	// line the message points at.
	for _, objectType := range slices.Sorted(maps.Keys(orderingFields)) {
		goFields, tsFields := orderingFields[objectType], ts[objectType]
		if tsFields == nil {
			continue // already reported as a missing model above
		}
		for _, f := range goFields {
			if !slices.Contains(tsFields, f) {
				t.Errorf("%s: %q is in orderingFields (pkg/provider/netbox/ordering.go) but not in ORDERING_FIELDS (src/types.ts) — the backend would sort on it, the picker never offers it. Add %q to ORDERING_FIELDS['%s'] or drop it from ordering.go",
					objectType, f, f, objectType)
			}
		}
		for _, f := range tsFields {
			if !slices.Contains(goFields, f) {
				t.Errorf("%s: %q is in ORDERING_FIELDS (src/types.ts) but not in orderingFields (pkg/provider/netbox/ordering.go) — the picker offers it and the backend refuses it, so choosing it returns unsorted rows and the note about fields NetBox accepts. Add %q to orderingFields[%q] (with the measurement that earns it) or drop it from src/types.ts",
					objectType, f, f, objectType)
			}
		}
	}
}
