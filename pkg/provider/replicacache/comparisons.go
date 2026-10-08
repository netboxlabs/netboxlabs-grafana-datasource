package replicacache

import (
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

const (
	opGTE = "gte"
	opLTE = "lte"
)

// The replica compares with gt and lt only. On a column whose values are all
// multiples of one step — 1 for whole numbers, 0.01 for a DECIMAL(8,2) — the
// inclusive comparisons have exact strict equivalents, because there is no
// stored value strictly between a multiple of the step and the next one:
//
//	>= x  is  > ceil(x) - step        <= x  is  < floor(x) + step
//	>  x  is  > floor(x)              <  x  is  < ceil(x)
//
// with floor and ceil taken to the step. The last two change no answer; they
// put the literal on the column's own scale, so the result never depends on
// how the service rounds a finer one. Floating-point columns have no step, and
// a neighbour computed in float64 does not survive a FLOAT column's own
// conversion, so they get no inclusive comparison; NetBox stores none.

// plainDecimal is the number syntax accepted: digits with an optional point and
// sign. big.Rat would also take "1/2", "0x10" and exponents, none of which a
// filter value means.
var plainDecimal = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$`)

var decimalType = regexp.MustCompile(`^DECIMAL\((\d+),\s*(\d+)\)$`)

// wholeNumberBits is the width of each whole-number type the catalogue names.
// HUGEINT's range is symmetric, ±(2^127 - 1).
var wholeNumberBits = map[string]uint{"TINYINT": 8, "SMALLINT": 16, "INTEGER": 32, "BIGINT": 64, "HUGEINT": 128}

// stepOf returns the step a column's values are multiples of, as a decimal
// scale (0 for whole numbers), and the range the type holds. ok is false for
// every type without one.
func stepOf(duckType string) (scale int, lo, hi *big.Rat, ok bool) {
	t := strings.ToUpper(strings.TrimSpace(duckType))
	if bits, whole := wholeNumberBits[t]; whole {
		limit := new(big.Int).Lsh(big.NewInt(1), bits-1)
		hiInt := new(big.Int).Sub(limit, big.NewInt(1))
		loInt := new(big.Int).Neg(limit)
		if t == "HUGEINT" {
			loInt.Add(loInt, big.NewInt(1))
		}
		return 0, new(big.Rat).SetInt(loInt), new(big.Rat).SetInt(hiInt), true
	}
	m := decimalType.FindStringSubmatch(t)
	if m == nil {
		return 0, nil, nil, false
	}
	precision, _ := strconv.Atoi(m[1])
	scale, _ = strconv.Atoi(m[2])
	if precision < 1 || precision > 38 || scale > precision {
		return 0, nil, nil, false
	}
	// The largest DECIMAL(p,s) is 10^(p-s) less one step.
	hi = new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision-scale)), nil))
	hi.Sub(hi, stepRat(scale))
	return scale, new(big.Rat).Neg(hi), hi, true
}

func stepRat(scale int) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
}

// floorTo is x rounded down to a multiple of 10^-scale. big.Int's Div rounds
// toward negative infinity for a positive divisor, which a Rat's denominator
// always is.
func floorTo(x *big.Rat, scale int) *big.Rat {
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Int).Mul(x.Num(), unit)
	q := new(big.Int).Div(scaled, x.Denom())
	return new(big.Rat).SetFrac(q, unit)
}

func ceilTo(x *big.Rat, scale int) *big.Rat {
	return new(big.Rat).Neg(floorTo(new(big.Rat).Neg(x), scale))
}

// rewriteComparisons turns >= and <= into the strict comparisons the replica
// has, and puts > and < on the column's scale, for every whole-number and
// DECIMAL column. Anything else passes through for validateFilters and
// buildFilterValues to judge as before. The entity must be ingested: the
// rewrite needs the column's type.
//
// At the edge of the type the strict equivalent would be a literal the type
// cannot hold, but the filter is still exact: <= the largest value is every
// row with a value (is-not-empty, or nothing on a NOT NULL column), and >=
// past the largest is no row — none.
func rewriteComparisons(filters []provider.Filter, e entity, c *catalog) (out []provider.Filter, none bool, err error) {
	out = make([]provider.Filter, 0, len(filters))
	seen := map[string]bool{}
	notEmpty := map[string]bool{}
	for _, f := range filters {
		op := f.Operator
		strict, inclusive := map[string]string{opGTE: opGT, opLTE: opLT}[op]
		if op == opGT || op == opLT {
			strict = op
		}
		values := splitValues(f.Value)
		col, known := filterColumn(e, c, f.Field)
		scale, lo, hi, stepped := stepOf(col.Type)
		if strict == "" || len(values) == 0 || !known || !stepped || !slices.Contains(col.Operators, strict) {
			out = append(out, f)
			continue
		}
		refuse := func(reason string) error {
			return &UnsupportedFilterError{Field: f.Field, Operator: f.Operator, Reason: reason}
		}
		// buildFilterValues makes both checks too, but after this rewrite it
		// would name gt where the panel said gte.
		if len(values) > 1 {
			return nil, false, refuse("a comparison accepts a single value, but several were given")
		}
		if seen[f.Field+"|"+strict] {
			return nil, false, refuse("the same comparison is applied twice to this field; this backend cannot combine them, so remove one")
		}
		seen[f.Field+"|"+strict] = true

		x, isNumber := parseNumber(values[0])
		if !isNumber {
			if inclusive {
				return nil, false, refuse(fmt.Sprintf("a %s column takes a number to compare with, and %q is not one", strings.ToLower(col.Type), values[0]))
			}
			out = append(out, f) // as before: sent as given
			continue
		}
		var bound *big.Rat
		switch op {
		case opGTE:
			bound = new(big.Rat).Sub(ceilTo(x, scale), stepRat(scale))
		case opLTE:
			bound = new(big.Rat).Add(floorTo(x, scale), stepRat(scale))
		case opGT:
			bound = floorTo(x, scale)
		case opLT:
			bound = ceilTo(x, scale)
		}
		if inclusive && (bound.Cmp(lo) < 0 || bound.Cmp(hi) > 0) {
			// Past the low end, >= holds for every value and <= for none; past
			// the high end, the reverse.
			if (bound.Cmp(lo) < 0) == (op == opLTE) {
				// No row; the rest is still validated by the caller.
				none = true
				continue
			}
			if !col.Nullable {
				continue // every row has a value
			}
			if !slices.Contains(col.Operators, "isnull") {
				return nil, false, refuse(fmt.Sprintf("every value a %s column holds satisfies this, so it means \"has any value\", which this column cannot be filtered on", strings.ToLower(col.Type)))
			}
			notEmpty[f.Field] = true
			out = append(out, provider.Filter{Field: f.Field, Operator: opNEmpty})
			continue
		}
		out = append(out, provider.Filter{Field: f.Field, Operator: strict, Value: bound.FloatString(scale)})
	}
	// "Has a value" beside the panel's own "is empty" on that field: no row.
	for _, f := range out {
		if f.Operator == opEmpty && notEmpty[f.Field] {
			none = true
		}
	}
	return out, none, nil
}

// parseNumber reads a plain decimal exactly.
func parseNumber(s string) (*big.Rat, bool) {
	if !plainDecimal.MatchString(s) {
		return nil, false
	}
	return new(big.Rat).SetString(s)
}

// filterColumn is the column a filter on field compares: the stored column, or
// for an expanded name the target's column.
func filterColumn(e entity, c *catalog, field string) (column, bool) {
	if own, ok := e.column(field); ok {
		return own, true
	}
	if via, target, ok := e.expandedColumn(field); ok && via.Ref.Available {
		return targetColumn(c, via.Ref, target), true
	}
	return column{}, false
}
