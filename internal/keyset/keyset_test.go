package keyset

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// THE ARMS PARTITION THE ORDER, NEAREST FIRST, ON BOTH SIDES OF EVERY ROW.
//
// For an order and a row in it, the rows After's arms hold — each arm's rows in
// the order, the arms one after another — are exactly the rows after it, in
// the order; and the rows Before's arms hold, each arm's rows in the order
// reversed, are exactly the rows before it, nearest first. That is the whole of
// what a later page and a neighbour read rest on: a row in two arms is read
// twice, a row in none is skipped, and arms out of order put a page's rows out
// of order — each silently, and only for a caller who paged.
//
// Checked over every shape that matters rather than a few hand-picked rows:
// orders of one to three columns before the id that ends every order, each
// column ascending or descending, NULL-able or not, over values drawn from a
// range small enough that ties are the common case and NULLs sit at every
// depth — with every row of the set as the pivot.
func TestTheArmsPartitionTheOrderNearestFirst(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewPCG(7, 11))
	for trial := range 400 {
		terms := randomTerms(random)
		rows := randomRows(random, terms, 2+random.IntN(40))
		slices.SortFunc(rows, func(a, b []any) int { return compare(terms, a, b) })
		for at, pivot := range rows {
			after := read(rows, After(terms, pivot), terms, false)
			if want := rows[at+1:]; !sameRows(after, want) {
				t.Fatalf("trial %d, %s, pivot %v: after reads\n%v\nwant\n%v",
					trial, describe(terms), pivot, after, want)
			}
			before := read(rows, Before(terms, pivot), terms, true)
			want := slices.Clone(rows[:at])
			slices.Reverse(want)
			if !sameRows(before, want) {
				t.Fatalf("trial %d, %s, pivot %v: before reads\n%v\nwant\n%v",
					trial, describe(terms), pivot, before, want)
			}
		}
	}
}

// AN ARM IS AN EQUALITY PREFIX AND ONE BOUND, which is what makes it one seek
// of an index on the order: the board's resume after a card is the card's rank
// held equal with the id past it, and then every rank past it — and the NULL
// tail of a NULL-able column is an arm of its own, because no index range
// reaches past the values into the NULLs.
func TestAnArmIsAPrefixAndOneBound(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		terms  []Term
		keys   []any
		after  bool
		clause []string
		args   [][]any
	}{
		"a board's next page": {
			terms: []Term{{Column: "t.rank", NeverNull: true},
				{Column: "t.id", NeverNull: true}},
			keys: []any{"a5", "t-9"}, after: true,
			clause: []string{"(t.rank = ? AND t.id > ?)", "(t.rank > ?)"},
			args:   [][]any{{"a5", "t-9"}, {"a5"}},
		},
		"the workspace's next page": {
			terms: []Term{{Column: "t.updated_at", Descending: true, NeverNull: true},
				{Column: "t.id", NeverNull: true}},
			keys: []any{int64(7), "t-9"}, after: true,
			clause: []string{"(t.updated_at = ? AND t.id > ?)", "(t.updated_at < ?)"},
			args:   [][]any{{int64(7), "t-9"}, {int64(7)}},
		},
		"past a due date, the undated last": {
			terms: []Term{{Column: "t.due_at"}, {Column: "t.id", NeverNull: true}},
			keys:  []any{int64(5), "t-9"}, after: true,
			clause: []string{"(t.due_at = ? AND t.id > ?)", "(t.due_at > ?)",
				"(t.due_at IS NULL)"},
			args: [][]any{{int64(5), "t-9"}, {int64(5)}, nil},
		},
		"past an undated task, only the undated": {
			terms: []Term{{Column: "t.due_at"}, {Column: "t.id", NeverNull: true}},
			keys:  []any{nil, "t-9"}, after: true,
			clause: []string{"(t.due_at IS NULL AND t.id > ?)"},
			args:   [][]any{{"t-9"}},
		},
		"before an undated task, every dated one": {
			terms: []Term{{Column: "t.due_at"}, {Column: "t.id", NeverNull: true}},
			keys:  []any{nil, "t-9"},
			clause: []string{"(t.due_at IS NULL AND t.id < ?)",
				"(t.due_at IS NOT NULL)"},
			args: [][]any{{"t-9"}, nil},
		},
		"before a removal, the later removals": {
			terms: []Term{{Column: "t.removed_at", Descending: true, NeverNull: true},
				{Column: "t.id", NeverNull: true}},
			keys:   []any{int64(3), "t-9"},
			clause: []string{"(t.removed_at = ? AND t.id < ?)", "(t.removed_at > ?)"},
			args:   [][]any{{int64(3), "t-9"}, {int64(3)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			arms := Before(c.terms, c.keys)
			if c.after {
				arms = After(c.terms, c.keys)
			}
			if len(arms) != len(c.clause) {
				t.Fatalf("%d arms, want %d", len(arms), len(c.clause))
			}
			for i, arm := range arms {
				clause, args := arm.SQL(c.terms)
				if clause != c.clause[i] || !slices.Equal(args, c.args[i]) {
					t.Errorf("arm %d is %s %v, want %s %v", i, clause, args,
						c.clause[i], c.args[i])
				}
			}
		})
	}
}

// A DISJUNCTION IS EVERY ARM IN ONE READ, in the arms' own order and with
// their arguments bound in that order — and no arm at all is no row rather
// than a statement that does not parse.
func TestADisjunctionIsEveryArmInOneRead(t *testing.T) {
	t.Parallel()
	terms := []Term{{Column: "fs0.num", Descending: true},
		{Column: "t.id", NeverNull: true}}
	clause, args := Disjunction(terms, After(terms, []any{int64(5), "t-9"}))
	if want := "((fs0.num = ? AND t.id > ?) OR (fs0.num < ?) OR (fs0.num IS NULL))"; clause != want {
		t.Errorf("the disjunction is %s, want %s", clause, want)
	}
	if want := []any{int64(5), "t-9", int64(5)}; !slices.Equal(args, want) {
		t.Errorf("it binds %v, want %v", args, want)
	}
	if clause, args := Disjunction(terms, nil); clause != "0" || args != nil {
		t.Errorf("no arm is %q %v, want a predicate no row meets", clause, args)
	}
}

// randomTerms is an order of one to three columns and the id after them.
func randomTerms(random *rand.Rand) []Term {
	n := 1 + random.IntN(3)
	terms := make([]Term, 0, n+1)
	for i := range n {
		terms = append(terms, Term{
			Column:     fmt.Sprintf("c%d", i),
			Descending: random.IntN(2) == 0,
			NeverNull:  random.IntN(2) == 0,
		})
	}
	return append(terms, Term{Column: "id", NeverNull: true})
}

// randomRows is n rows of small values, NULL in a NULL-able column about one
// time in four, with a unique id in the last column.
func randomRows(random *rand.Rand, terms []Term, n int) [][]any {
	rows := make([][]any, n)
	for r := range rows {
		row := make([]any, len(terms))
		for i, term := range terms[:len(terms)-1] {
			if !term.NeverNull && random.IntN(4) == 0 {
				continue
			}
			row[i] = int64(random.IntN(4))
		}
		row[len(terms)-1] = int64(r)
		rows[r] = row
	}
	return rows
}

// compare orders two rows as an order's terms do, NULLs last in both
// directions.
func compare(terms []Term, a, b []any) int {
	for i, term := range terms {
		x, y := a[i], b[i]
		switch {
		case x == nil && y == nil:
			continue
		case x == nil:
			return 1
		case y == nil:
			return -1
		}
		c := 0
		switch xv, yv := x.(int64), y.(int64); {
		case xv < yv:
			c = -1
		case xv > yv:
			c = 1
		}
		if term.Descending {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// read is the rows each arm holds, in the order (or reversed), arm after arm.
func read(rows [][]any, arms []Arm, terms []Term, reverse bool) [][]any {
	var out [][]any
	for _, arm := range arms {
		var held [][]any
		for _, row := range rows {
			if arm.holds(row) {
				held = append(held, row)
			}
		}
		slices.SortFunc(held, func(a, b []any) int {
			if reverse {
				return compare(terms, b, a)
			}
			return compare(terms, a, b)
		})
		out = append(out, held...)
	}
	return out
}

// holds reports whether a row meets every condition of the arm, by the SQL
// semantics each condition renders to: a comparison with NULL is never true.
func (a Arm) holds(row []any) bool {
	for _, c := range a.conds {
		v := row[c.term]
		switch c.op {
		case opIsNull:
			if v != nil {
				return false
			}
		case opNotNull:
			if v == nil {
				return false
			}
		default:
			if v == nil {
				return false
			}
			x, y := v.(int64), c.value.(int64)
			if (c.op == opEqual && x != y) || (c.op == opGreater && x <= y) ||
				(c.op == opLess && x >= y) {
				return false
			}
		}
	}
	return true
}

func sameRows(a, b [][]any) bool {
	return slices.EqualFunc(a, b, func(x, y []any) bool { return slices.Equal(x, y) })
}

func describe(terms []Term) string {
	parts := make([]string, len(terms))
	for i, term := range terms {
		parts[i] = term.Column
		if term.Descending {
			parts[i] += " DESC"
		}
		if !term.NeverNull {
			parts[i] += " NULLS LAST"
		}
	}
	return strings.Join(parts, ", ")
}
