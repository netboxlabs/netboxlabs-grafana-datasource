package replicacache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The replica has no negation: no neq, no nin, no "not like". NetBox has one
// for every match (n, nic, nie, nisw, niew), with Django's exclude() meaning:
// a row is dropped when its value matches, so NULL is kept, and several values
// drop each of them (not a AND not b).
//
// Each is answered exactly, by the cheapest of three routes:
//
//  1. An equality on the same field absorbs it: status in (a, b) and not a is
//     status = b (subtractNegations).
//  2. "Not these values" of a choice column is the rest of NetBox's list, which
//     the replica can filter, sort and page — once the row counts prove no row
//     holds NULL or a value outside the list (complementOf).
//  3. Otherwise the rows matching the other filters are read and tested here
//     (walkNegated), with the total from row counts: by inclusion–exclusion,
//     count(base) - count(base AND P1) - count(base AND P2) + count(base AND
//     P1 AND P2), where the replica can take each count; else by reading
//     every row, up to MaxLimit.
//
// Where none is possible the query is refused, saying how to narrow it.

// negatedOperator is the match each negation excludes.
var negatedOperator = map[string]string{
	"n":    opExact,
	"nic":  opIContns,
	"nie":  opIExact,
	"nisw": opIStarts,
	"niew": opIEnds,
}

// maxNegations bounds inclusion–exclusion at 2^3 = 8 row counts.
const maxNegations = 3

// negation is one predicate a kept row must not match: for "n", the value is
// not in values; for a text negation, one value per negation, since several
// values each drop what they match.
type negation struct {
	field  string
	op     string // the positive match
	values []string
	source provider.Filter
}

func (n negation) positive() provider.Filter {
	return provider.Filter{Field: n.field, Operator: n.op, Value: strings.Join(n.values, ",")}
}

// splitNegations separates the negations from the filters the replica takes.
// Every "n" on one field becomes one negation over all its values; a text
// negation becomes one per value. A negation with no value is dropped, as
// NetBox mode sends no parameter for it.
func splitNegations(filters []provider.Filter) ([]provider.Filter, []negation) {
	var positives []provider.Filter
	var negs []negation
	exact := map[string]int{}
	for _, f := range filters {
		pos, negated := negatedOperator[f.Operator]
		// A row with no field sends nothing, negated or not; buildFilterValues
		// skips it among the positives.
		if !negated || f.Field == "" {
			positives = append(positives, f)
			continue
		}
		values := splitValues(f.Value)
		if len(values) == 0 {
			continue
		}
		if pos != opExact {
			for _, v := range values {
				// The same case-insensitive match twice is one predicate, and
				// counting it twice would spend the inclusion–exclusion budget.
				dup := slices.ContainsFunc(negs, func(n negation) bool {
					return n.field == f.Field && n.op == pos &&
						(n.values[0] == v || isASCII(v) && isASCII(n.values[0]) && strings.EqualFold(n.values[0], v))
				})
				if !dup {
					negs = append(negs, negation{field: f.Field, op: pos, values: []string{v}, source: f})
				}
			}
			continue
		}
		i, ok := exact[f.Field]
		if !ok {
			i = len(negs)
			exact[f.Field] = i
			negs = append(negs, negation{field: f.Field, op: pos, source: f})
		}
		for _, v := range values {
			if !slices.Contains(negs[i].values, v) {
				negs[i].values = append(negs[i].values, v)
			}
		}
	}
	return positives, negs
}

// validateNegations judges each negation as the match it excludes, naming the
// operator the panel used. "Not equal" on an IP address is refused: NetBox
// compares the host whatever the mask (setAddressMatch), which the replica can
// do inside a filter but which the stored text cannot be tested against here.
func validateNegations(negs []negation, e entity, c *catalog) error {
	isAddress := addressField(e, c)
	for _, n := range negs {
		if err := validateFilters([]provider.Filter{n.positive()}, e, c); err != nil {
			var unsupported *UnsupportedFilterError
			if errors.As(err, &unsupported) {
				unsupported.Operator = n.source.Operator
			}
			return err
		}
		if n.op == opExact && isAddress(n.field) {
			return &UnsupportedFilterError{Field: n.field, Operator: n.source.Operator,
				Reason: "NetBox matches an IP address by its host whatever the mask, which this backend can test only as a filter it sends, never as one it excludes; filter on what to keep instead"}
		}
		if col, _ := filterColumn(e, c, n.field); !rowComparable(col.Type) {
			return &UnsupportedFilterError{Field: n.field, Operator: n.source.Operator,
				Reason: fmt.Sprintf("the replica converts a %s value before comparing it, which the rows read here cannot be tested against; filter on what to keep instead", strings.ToLower(col.Type))}
		}
	}
	return nil
}

// rowComparable reports whether a column's stored values, as rows return
// them, compare the way the replica compares them in a filter: text, numbers
// and booleans. A negation is offered on those only (seamOperators), because
// the rows are tested here while the totals come from the replica.
func rowComparable(colType string) bool {
	switch fieldTypeOf(colType) {
	case provider.FieldTypeNumber, provider.FieldTypeBoolean:
		return true
	}
	t := strings.ToUpper(strings.TrimSpace(colType))
	for _, text := range []string{"VARCHAR", "TEXT", "STRING", "CHAR", "BPCHAR"} {
		if t == text || strings.HasPrefix(t, text+"(") {
			return true
		}
	}
	return false
}

// sameValue compares two filter values as the replica compares them on a
// column of type colType: numbers by value ("4" is "4.0"), booleans by truth,
// everything else as text.
func sameValue(colType, a, b string) bool {
	switch fieldTypeOf(colType) {
	case provider.FieldTypeNumber:
		if bits := floatBits(colType); bits > 0 {
			x, okA := parseFloat(a, bits)
			y, okB := parseFloat(b, bits)
			if okA && okB {
				return x == y
			}
		}
		x, okA := parseNumber(a)
		y, okB := parseNumber(b)
		if okA && okB {
			return x.Cmp(y) == 0
		}
	case provider.FieldTypeBoolean:
		x, errA := strconv.ParseBool(a)
		y, errB := strconv.ParseBool(b)
		if errA == nil && errB == nil {
			return x == y
		}
	}
	return a == b
}

// floatBits is a floating-point column's width, 0 for any other type. The
// replica casts a filter value to the column's type before comparing, so a
// value is compared as that width holds it.
func floatBits(colType string) int {
	switch strings.ToUpper(strings.TrimSpace(colType)) {
	case "DOUBLE":
		return 64
	case "FLOAT", "REAL":
		return 32
	}
	return 0
}

// floatSyntax is a decimal with an optional exponent, which the replica's
// float cast takes; not Go's hexadecimal floats, Inf, NaN or underscores.
var floatSyntax = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$`)

// parseFloat reads s rounded to a float of bits, as the replica casts a value
// to a FLOAT or DOUBLE column. Filter values and stored values go through it
// alike, so both sides of a comparison read a number the same way.
func parseFloat(s string, bits int) (float64, bool) {
	if !floatSyntax.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, bits)
	return f, err == nil
}

func containsValue(colType string, set []string, v string) bool {
	return slices.ContainsFunc(set, func(s string) bool { return sameValue(colType, s, v) })
}

// subtractNegations applies route 1. Equality filters on one field are ORed,
// so their values are a set; a negation on the same field removes values from
// it and is then answered. none reports that nothing is left, so no row can
// match.
func subtractNegations(filters []provider.Filter, negs []negation, e entity, c *catalog) ([]provider.Filter, []negation, bool) {
	var rest []negation
	for _, n := range negs {
		if n.op != opExact {
			rest = append(rest, n)
			continue
		}
		var kept []provider.Filter
		var set []string
		for _, f := range filters {
			if f.Field == n.field && equalityOperator(f.Operator) {
				set = append(set, splitValues(f.Value)...)
				continue
			}
			kept = append(kept, f)
		}
		if len(set) == 0 {
			rest = append(rest, n)
			continue
		}
		col, _ := filterColumn(e, c, n.field)
		var left []string
		for _, v := range set {
			if !containsValue(col.Type, n.values, v) && !slices.Contains(left, v) {
				left = append(left, v)
			}
		}
		if len(left) == 0 {
			return nil, nil, true
		}
		filters = append(kept, provider.Filter{Field: n.field, Operator: opExact, Value: strings.Join(left, ",")})
	}
	return filters, rest, false
}

// intersect is the filters of base AND every negation in negs taken positively
// — one term of the inclusion–exclusion sum. Two text matches of one kind on
// one field merge where one value
// implies the other (a prefix of a longer prefix) and are empty where they
// contradict (two different whole values); ok is false where the replica
// cannot express the conjunction at all — it takes one value per match and
// field.
func intersect(base []provider.Filter, negs []negation) (out []provider.Filter, empty, ok bool) {
	type key struct{ field, op string }
	text := map[key][]string{}
	exact := map[string][]string{}
	for _, f := range base {
		op := f.Operator
		if equalityOperator(op) {
			exact[f.Field] = append(exact[f.Field], splitValues(f.Value)...)
			continue
		}
		if _, isText := wireTextOperator[op]; isText {
			// As buildFilterValues sends it: trimmed, and nothing at all when
			// blank. Several values were refused before this.
			if values := splitValues(f.Value); len(values) > 0 {
				text[key{f.Field, op}] = append(text[key{f.Field, op}], values[0])
			}
			continue
		}
		out = append(out, f)
	}
	for _, n := range negs {
		if n.op != opExact {
			text[key{n.field, n.op}] = append(text[key{n.field, n.op}], n.values[0])
			continue
		}
		// One "n" per field (splitNegations), and none on a field with an
		// equality (subtractNegations absorbed it), so nothing to intersect.
		exact[n.field] = n.values
	}
	for _, field := range slices.Sorted(maps.Keys(exact)) {
		out = append(out, provider.Filter{Field: field, Operator: opExact, Value: strings.Join(exact[field], ",")})
	}
	keys := slices.Collect(maps.Keys(text))
	slices.SortFunc(keys, func(a, b key) int { return strings.Compare(a.field+a.op, b.field+b.op) })
	for _, k := range keys {
		value, isEmpty, can := mergeTextMatches(k.op, text[k])
		if !can {
			return nil, false, false
		}
		if isEmpty {
			return nil, true, true
		}
		out = append(out, provider.Filter{Field: k.field, Operator: k.op, Value: value})
	}
	return out, false, true
}

// mergeTextMatches ANDs several values of one case-insensitive text match on
// one field into the single value the replica takes. The rules compare
// lowercased text, so they are applied to ASCII only: elsewhere Go's case
// mapping and the database's need not agree, and a wrong "these contradict"
// would be a wrong count.
func mergeTextMatches(op string, values []string) (value string, empty, ok bool) {
	values = slices.Compact(slices.Sorted(slices.Values(values)))
	if len(values) == 1 {
		return values[0], false, true
	}
	for _, v := range values {
		if !isASCII(v) {
			return "", false, false
		}
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	longest := strings.ToLower(values[0])
	implied := func(v string) bool {
		v = strings.ToLower(v)
		switch op {
		case opIExact:
			return v == longest
		case opIStarts:
			return strings.HasPrefix(longest, v)
		case opIEnds:
			return strings.HasSuffix(longest, v)
		default: // contains
			return strings.Contains(longest, v)
		}
	}
	for _, v := range values[1:] {
		if !implied(v) {
			// Two whole values, two prefixes or two suffixes that do not nest
			// cannot both hold; two "contains" can, and the replica cannot say so.
			return "", op != opIContns, op != opIContns
		}
	}
	return values[0], false, true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// negatedRows is what a negated query hands back to Query.
type negatedRows struct {
	raws     []json.RawMessage
	total    int
	asOf     *time.Time
	warnings []string
	// fields is the projection the rows were read with, the negated fields
	// included, when it differs from the query's; nil otherwise.
	fields []string
}

// negatedQuery holds what the routes share.
type negatedQuery struct {
	spec    provider.QuerySpec
	e       entity
	c       *catalog
	filters []provider.Filter // the positive filters, already rewritten
	negs    []negation
	expand  []string
	// countExpand is what a count needs expanded: the references the filters
	// name, and no more.
	countExpand []string
	sort        string
	fields      []string // the projection; nil for every column
	limit       int
	isAddress   func(string) bool
}

// countParams is a count's query: the filters, the joins they need, and the
// key alone for the one row the envelope comes with.
func (nq negatedQuery) countParams(filters []provider.Filter) (url.Values, error) {
	return nq.params(filters, nq.countExpand, []string{nq.e.pk()}, false)
}

func (nq negatedQuery) params(filters []provider.Filter, expand, fields []string, sorted bool) (url.Values, error) {
	q, err := buildFilterValues(filters, nq.isAddress)
	if err != nil {
		return nil, err
	}
	if len(expand) > 0 {
		q.Set("expand", strings.Join(expand, ","))
	}
	if sorted && nq.sort != "" {
		q.Set("sort", nq.sort)
	}
	return withFields(q, fields), nil
}

// counted is an inclusion–exclusion total, and consistent says every count
// came from one moment of the replica.
type counted struct {
	total      int
	asOf       *time.Time
	consistent bool
}

// countTerms builds the inclusion–exclusion terms without sending anything:
// one query per subset of the negations, nil for a subset known to match no
// row. ok is false when the replica's counts cannot answer: too many
// negations, a term it cannot express, or a text negation outside ASCII,
// whose case folding the replica's counts and this code's row test need not
// share (see mergeTextMatches).
func countTerms(nq negatedQuery) ([]url.Values, bool) {
	k := len(nq.negs)
	if k > maxNegations {
		return nil, false
	}
	for _, n := range nq.negs {
		if n.op != opExact && !isASCII(n.values[0]) {
			return nil, false
		}
	}
	terms := make([]url.Values, 1<<k)
	for mask := range terms {
		var in []negation
		for i := range k {
			if mask&(1<<i) != 0 {
				in = append(in, nq.negs[i])
			}
		}
		filters, empty, ok := intersect(nq.filters, in)
		if !ok {
			return nil, false
		}
		if empty {
			continue // a contradiction: no row, no request
		}
		q, err := nq.countParams(filters)
		if err != nil {
			return nil, false
		}
		terms[mask] = q
	}
	return terms, true
}

// count takes the terms' counts concurrently, so they are as close to one
// moment as requests can be, and sums them with alternating signs. Counts
// from different moments are taken once more before being reported as such.
func (p *Provider) count(ctx context.Context, nq negatedQuery, terms []url.Values) (counted, error) {
	var res counted
	for attempt := 0; attempt < 2; attempt++ {
		counts, asOfs, err := p.counts(ctx, nq.spec.ObjectType, terms)
		if err != nil {
			return counted{}, err
		}
		res = counted{consistent: true}
		for mask, n := range counts {
			if bitsSet(mask)%2 == 1 {
				n = -n
			}
			res.total += n
		}
		for _, t := range asOfs {
			if t == nil {
				continue
			}
			if res.asOf != nil && !t.Equal(*res.asOf) {
				res.consistent = false
			}
			res.asOf = older(res.asOf, t)
		}
		if res.consistent {
			break
		}
	}
	res.total = max(res.total, 0)
	return res, nil
}

func bitsSet(n int) int {
	c := 0
	for ; n > 0; n &= n - 1 {
		c++
	}
	return c
}

// counts asks for every non-nil query's count at once. A nil query is a term
// known to be empty.
func (p *Provider) counts(ctx context.Context, entity string, queries []url.Values) ([]int, []*time.Time, error) {
	counts := make([]int, len(queries))
	asOfs := make([]*time.Time, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		if q == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, counts[i], asOfs[i], errs[i] = p.client.list(ctx, entity, q, 1)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	return counts, asOfs, nil
}

const movingCountsWarning = "The total was counted while the replica was applying changes, so it may be off by the rows that changed during the count. Refresh to count again."

// withCount gives rows the counted total, at least every match the read saw
// (matched). It warns when the counts are from different moments, or total
// fewer matches than the read saw. Rows read at one moment and counts taken at
// the next are otherwise what any multi-page read gives on a busy replica, and
// a warning there would fail every alert rule on one; the answer reports the
// older instant instead.
func withCount(cnt counted, out negatedRows, matched int) negatedRows {
	if !cnt.consistent || cnt.total < matched {
		out.warnings = append(out.warnings, movingCountsWarning)
	}
	out.total = max(cnt.total, matched, len(out.raws))
	out.asOf = older(out.asOf, cnt.asOf)
	return out
}

// queryNegated answers a query that still has negations after
// subtractNegations, by route 2 or 3. Counts are taken only where they are
// the answer or prove one: a read that reaches the last row has the exact
// total already, which on a small table is the whole cost.
func (p *Provider) queryNegated(ctx context.Context, nq negatedQuery) (negatedRows, error) {
	terms, countable := countTerms(nq)
	var (
		cnt   counted
		taken bool
	)
	countOnce := func() error {
		if taken {
			return nil
		}
		var err error
		cnt, err = p.count(ctx, nq, terms)
		taken = err == nil
		return err
	}
	if nq.spec.CountOnly && countable {
		if err := countOnce(); err != nil {
			return negatedRows{}, err
		}
		return withCount(cnt, negatedRows{}, 0), nil
	}

	// Without counts, the total has to come from reading every row: possible
	// only up to MaxLimit, and required only for a caller that cannot do
	// without a total.
	needAll := !countable && !nq.spec.AllowUncounted
	if needAll {
		q, err := nq.countParams(nq.filters)
		if err != nil {
			return negatedRows{}, err
		}
		_, base, _, err := p.client.list(ctx, nq.spec.ObjectType, q, 1)
		if err != nil {
			return negatedRows{}, err
		}
		if base > MaxLimit {
			return negatedRows{}, &UnsupportedFilterError{Field: nq.negs[0].field, Operator: nq.negs[0].source.Operator,
				Reason: fmt.Sprintf("the replica cannot count rows for these negated filters together, so the rows are read and tested here, which works up to %s rows; the other filters match %s. Narrow them, or negate fewer values",
					thousands(MaxLimit), thousands(base))}
		}
	}

	fields := nq.fields
	if nq.spec.CountOnly {
		fields = []string{nq.e.pk()}
	}
	if fields != nil {
		for _, n := range nq.negs {
			if nq.e.has(n.field) && !slices.Contains(fields, n.field) {
				fields = append(fields, n.field)
			}
		}
	}
	q, err := nq.params(nq.filters, nq.expand, fields, !nq.spec.CountOnly)
	if err != nil {
		return negatedRows{}, err
	}
	want := nq.limit
	if nq.spec.CountOnly {
		want = 0
	}
	keep, progress := rowTester(nq), newWalker()
	read := func(scanCap int) (walked, error) {
		return p.client.walkOn(ctx, nq.spec.ObjectType, q, scan{keep: keep, want: want, countAll: needAll, scanCap: scanCap}, progress)
	}

	// Route 2 is tried only once the first page shows reading would not do:
	// it did not reach the end, nor find the rows wanted.
	complement, hasComplement := complementOf(nq)
	hasComplement = hasComplement && countable && !nq.spec.CountOnly
	scanCap := MaxLimit
	if hasComplement {
		scanCap = pageSize
	}
	w, err := read(scanCap)
	if err != nil {
		return negatedRows{}, err
	}
	if hasComplement && !w.exhausted && len(w.rows) < want {
		if err := countOnce(); err != nil {
			return negatedRows{}, err
		}
		if cnt.consistent {
			cq, err := nq.params(complement, nq.expand, nq.fields, true)
			if err != nil {
				return negatedRows{}, err
			}
			raws, total, asOf, err := p.client.list(ctx, nq.spec.ObjectType, cq, nq.limit)
			if err != nil {
				return negatedRows{}, err
			}
			if total == cnt.total && sameInstant(asOf, cnt.asOf) {
				return negatedRows{raws: raws, total: total, asOf: older(asOf, cnt.asOf)}, nil
			}
		}
		// On from the page already read.
		if w, err = read(MaxLimit); err != nil {
			return negatedRows{}, err
		}
	}

	out := negatedRows{raws: w.rows, asOf: w.asOf}
	if !nq.spec.CountOnly {
		out.fields = fields
	}
	switch {
	case w.exhausted:
		out.total = w.matched
	case countable:
		if err := countOnce(); err != nil {
			return negatedRows{}, err
		}
		out = withCount(cnt, out, w.matched)
	case needAll:
		// The base outgrew MaxLimit between the count and the walk.
		return negatedRows{}, &UnsupportedFilterError{Field: nq.negs[0].field, Operator: nq.negs[0].source.Operator,
			Reason: fmt.Sprintf("the rows matching the other filters grew past %s while they were being read, so the matches could not all be counted; narrow the other filters", thousands(MaxLimit))}
	}
	// Stopped short of the end with fewer rows than wanted, and nothing to
	// say they were all of them.
	if !w.exhausted && len(w.rows) < want && w.scanned >= MaxLimit && (!countable || out.total > len(w.rows)) {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"Only the first %s rows matching the other filters were checked against the negated ones, and %s of them were kept; rows past those were not read, so more may match. Narrow the other filters.",
			thousands(w.scanned), thousands(len(w.rows))))
	}
	return out, nil
}

// complementOf is route 2's query: a sole "not these values" on a choice
// column becomes "one of the others" from NetBox's list. The caller verifies
// it against the counts, since a NULL or a value outside the list would be
// kept by the negation and dropped by the complement.
func complementOf(nq negatedQuery) ([]provider.Filter, bool) {
	if len(nq.negs) != 1 || nq.negs[0].op != opExact {
		return nil, false
	}
	n := nq.negs[0]
	labels, ok := loadChoiceLabels()[nq.spec.ObjectType][n.field]
	if !ok {
		return nil, false
	}
	col, _ := filterColumn(nq.e, nq.c, n.field)
	var rest []string
	for _, v := range slices.Sorted(maps.Keys(labels)) {
		if !containsValue(col.Type, n.values, v) {
			rest = append(rest, v)
		}
	}
	if len(rest) == 0 {
		return nil, false
	}
	return append(slices.Clone(nq.filters), provider.Filter{Field: n.field, Operator: opExact, Value: strings.Join(rest, ",")}), true
}

// rowTester reports whether a raw row matches none of the negated predicates.
// It reads the row as stored — before labels or address masks — and compares
// as the replica does: equality by sameValue, the text matches lowercased.
// NULL matches nothing, so a NULL row is kept, as exclude() keeps it.
func rowTester(nq negatedQuery) func(json.RawMessage) (bool, error) {
	types := map[string]string{}
	for _, n := range nq.negs {
		col, _ := filterColumn(nq.e, nq.c, n.field)
		types[n.field] = col.Type
	}
	return func(raw json.RawMessage) (bool, error) {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			return false, &TransportError{Op: "reading a replica-cache row", Err: err, Message: rowShapeGuidance}
		}
		for _, n := range nq.negs {
			if matchesStored(storedValue(row[n.field]), types[n.field], n.op, n.values) {
				return false, nil
			}
		}
		return true, nil
	}
}

// storedValue reads one JSON value as the row holds it. A number keeps its own
// digits, as json.Number: float64 would round a 64-bit value the replica
// compares exactly. An object or array matches nothing.
func storedValue(raw json.RawMessage) interface{} {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return nil
	case raw[0] == '"':
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil
		}
		return text
	case string(raw) == "true":
		return true
	case string(raw) == "false":
		return false
	case raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9':
		return json.Number(raw)
	}
	return raw
}

func matchesStored(v interface{}, colType, op string, values []string) bool {
	var text string
	switch t := v.(type) {
	case nil:
		return false
	case string:
		text = t
	case json.Number:
		if op != opExact {
			return false
		}
		if bits := floatBits(colType); bits > 0 {
			if stored, ok := parseFloat(t.String(), bits); ok {
				return slices.ContainsFunc(values, func(v string) bool {
					x, ok := parseFloat(v, bits)
					return ok && x == stored
				})
			}
		}
		// The service's own number text, which may carry an exponent that a
		// filter value may not (parseNumber).
		stored, ok := new(big.Rat).SetString(t.String())
		return ok && slices.ContainsFunc(values, func(v string) bool {
			x, ok := parseNumber(v)
			return ok && x.Cmp(stored) == 0
		})
	case bool:
		text = strconv.FormatBool(t)
	default:
		return false
	}
	if op == opExact {
		return containsValue(colType, values, text)
	}
	if _, isString := v.(string); !isString {
		return false
	}
	// Lowercased with Go's simple Unicode mapping, which is what the replica's
	// DuckDB ILIKE does: measured, "s" ILIKE "ſ" is false, the Kelvin sign
	// ILIKE "k" and "İ" ILIKE "i" are true, "straße" ILIKE "STRASSE" is false,
	// and strings.ToLower agrees on each. Case folding (strings.EqualFold)
	// would not.
	text, want := strings.ToLower(text), strings.ToLower(values[0])
	switch op {
	case opIContns:
		return strings.Contains(text, want)
	case opIStarts:
		return strings.HasPrefix(text, want)
	case opIEnds:
		return strings.HasSuffix(text, want)
	case opIExact:
		return text == want
	}
	return false
}

func sameInstant(a, b *time.Time) bool {
	return a == nil || b == nil || a.Equal(*b)
}

// older is the earlier instant, so a max-data-age check judges the answer by
// its oldest part.
func older(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.Before(*a)) {
		return b
	}
	return a
}

// thousands formats n with comma separators (10000 -> "10,000"). Duplicated
// from pkg/plugin, which this package does not import.
func thousands(n int) string {
	s := strconv.Itoa(n)
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}
