// Package keyset turns one row's place in an order into the predicates that
// read the rows after it, or before it, as seeks of the index the order is
// served by.
//
// # A keyset resume is an ordered list of ARMS
//
// "Strictly after this row in this order" — the next page of a listing, the
// neighbour after a task — is, for an order over columns c0 … cn and a row
// holding v0 … vn, the rows that equal the row on c0 … ci-1 and come after it
// on ci, for every depth i. Those sets are DISJOINT, and every row of a deeper
// one precedes every row of a shallower one in the order, so reading them
// deepest first, each in the order, and stopping once enough rows are in hand
// is reading the order from the row onwards. That is what an index's own range
// scan does, and each arm is exactly one such range: an equality on the
// columns an index leads with and one bound on the next — `rank = ? AND id > ?`
// and then `rank > ?` for a board — so a later page is two seeks of the index
// the first page walked, and page fifty costs what page one does.
//
// # Why not one predicate
//
// The obvious spelling is one lexicographic disjunction — `c0 > ? OR (c0 = ?
// AND (c1 > ? OR …))` — and this engine's planner cannot seek on it: no
// conjunct bounds the leading column, so it enters on whatever equality prefix
// the rest of the statement offers and walks the index from its start,
// filtering every row before the cursor. Measured: a board's next page walked
// the project's board from the first card, the workspace's from the newest
// task in the company, and the trash's from its top, so N pages cost O(N²).
// Adding a range conjunct (`c0 >= ? AND (…)`) makes the walk start at the
// cursor's VALUE and still re-reads every row tied with it on every page:
// columns that are NOT NULL with a default of zero — the spend, the estimate,
// the points — are one tie group of most of the table, and a listing paged
// into it is quadratic again. Nor does one statement over the arms' union:
// written as their disjunction, the planner seeks neither arm and enters on
// the statement's scope alone, as it did for the nested form. Read arm by arm,
// each is a seek in the index's order that a LIMIT stops. Where no index can
// hold the order at all — a column of another table the read joins — there is
// no seek to lose, and [Disjunction] reads the arms as one predicate instead.
//
// # NULL is a position in the order
//
// Every order this is used for puts an absent value LAST in both directions,
// so after a value come the greater (or, descending, the lesser) values and
// then the NULLs, as two arms of one depth; after a NULL comes nothing at that
// depth; before a value come only the values short of it, and before a NULL
// every value; and the equality leg of a NULL is `IS NULL`. Spelled with bare
// comparisons alone, every one of those is NULL for a row with no value, so a
// page ending on such a row would end the listing and a descending order could
// never reach its undated rows at all. A column the caller says can never be
// NULL gets no NULL arm, which is what keeps its order one an index serves.
//
// # Pure over values
//
// An arm is DATA — which columns it holds equal, which one it bounds and how —
// rendered to SQL only by [Arm.SQL], so the arithmetic is tested against rows
// held in memory in every direction and with NULLs at every depth rather than
// through a database, and no consumer composes a second copy of it.
package keyset

import (
	"fmt"
	"strings"
)

// Term is one column of an order: the SQL expression the order sorts by, its
// direction, and whether the column can hold NULL.
type Term struct {
	Column     string
	Descending bool
	// NeverNull says the column can never hold NULL in the rows the order
	// is over — the schema declares it NOT NULL, or the statement's own
	// predicate excludes NULL — so no arm reads a NULL tail for it.
	NeverNull bool
}

// op is the comparison one condition of an arm makes.
type op uint8

const (
	opEqual op = iota + 1
	opIsNull
	opGreater
	opLess
	opNotNull
)

// cond is one condition of an arm, on one term of the order.
type cond struct {
	term  int
	op    op
	value any
}

// Arm is one disjoint range of an order: a conjunction of an equality prefix
// and one bound, read with the order's own ORDER BY (or its reverse).
type Arm struct {
	conds []cond
}

// After is the rows strictly after the row holding keys, as arms NEAREST
// FIRST: the rows of the first arm, read in the order, come directly after the
// row, and the second's after them. keys holds one value per term — nil for
// NULL — in the terms' order.
func After(terms []Term, keys []any) []Arm {
	return arms(terms, keys, true)
}

// Before is the rows strictly before the row holding keys, as arms NEAREST
// FIRST: the rows of the first arm, read in the order REVERSED, come directly
// before the row, and the second's before them. Reading the arms in this order
// with the order reversed is reading the order backwards from the row, so the
// first row found is the row's predecessor, and the arms' counts sum to its
// position.
func Before(terms []Term, keys []any) []Arm {
	return arms(terms, keys, false)
}

// arms builds the arms of one side, deepest depth first.
func arms(terms []Term, keys []any, after bool) []Arm {
	if len(keys) != len(terms) {
		panic(fmt.Sprintf("keyset: %d keys for an order of %d terms", len(keys),
			len(terms)))
	}
	var out []Arm
	for depth := len(terms) - 1; depth >= 0; depth-- {
		prefix := make([]cond, 0, depth+1)
		for i := 0; i < depth; i++ {
			if keys[i] == nil {
				prefix = append(prefix, cond{term: i, op: opIsNull})
				continue
			}
			prefix = append(prefix, cond{term: i, op: opEqual, value: keys[i]})
		}
		for _, bound := range bounds(terms[depth], keys[depth], after) {
			bound.term = depth
			out = append(out, Arm{conds: append(append([]cond{}, prefix...), bound)})
		}
	}
	return out
}

// bounds is what lies strictly after (or before) one value on one column, as
// the conditions of the arms at that depth, nearest first.
//
// THE NULLS ARE LAST IN BOTH DIRECTIONS. After a value, the values beyond it
// and then the NULL tail; after a NULL, nothing — only a deeper arm, where the
// NULL is held equal, carries a row past it. Before a value, only the values
// short of it, since every NULL is after it; before a NULL, every value.
func bounds(term Term, key any, after bool) []cond {
	beyond, short := opGreater, opLess
	if term.Descending {
		beyond, short = opLess, opGreater
	}
	switch {
	case after && key == nil:
		return nil
	case after && term.NeverNull:
		return []cond{{op: beyond, value: key}}
	case after:
		return []cond{{op: beyond, value: key}, {op: opIsNull}}
	case key == nil:
		return []cond{{op: opNotNull}}
	default:
		return []cond{{op: short, value: key}}
	}
}

// Disjunction renders arms as ONE predicate — their disjunction — for a read
// that seeks none of them anyway: an order on a column no index of the read's
// table holds, where reading the arms in turn would read the scope once per
// arm. It is the same set of rows; what is lost is only the seek, which such
// an order never had.
func Disjunction(terms []Term, arms []Arm) (string, []any) {
	if len(arms) == 0 {
		// NOTHING LIES THERE, said as a predicate rather than as the
		// empty disjunction, which is not SQL.
		return "0", nil
	}
	clauses := make([]string, len(arms))
	var args []any
	for i, arm := range arms {
		clause, armArgs := arm.SQL(terms)
		clauses[i] = clause
		args = append(args, armArgs...)
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// SQL renders the arm as a conjunction over the terms' columns, with its
// arguments in the order they bind.
func (a Arm) SQL(terms []Term) (string, []any) {
	clauses := make([]string, 0, len(a.conds))
	var args []any
	for _, c := range a.conds {
		column := terms[c.term].Column
		switch c.op {
		case opEqual:
			clauses = append(clauses, column+" = ?")
			args = append(args, c.value)
		case opIsNull:
			clauses = append(clauses, column+" IS NULL")
		case opGreater:
			clauses = append(clauses, column+" > ?")
			args = append(args, c.value)
		case opLess:
			clauses = append(clauses, column+" < ?")
			args = append(args, c.value)
		case opNotNull:
			clauses = append(clauses, column+" IS NOT NULL")
		}
	}
	return "(" + strings.Join(clauses, " AND ") + ")", args
}
