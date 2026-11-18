package worklet

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// ---- the orders no column holds ----------------------------------------- //
//
// Four sort keys order by what a value MEANS: a key by its number (ENG-51
// before ENG-185), a status by a board's order, a type by the catalogue, an
// assignee by the seat's name. Each compiles to terms over the task row, read
// through the same [keyset] arms as every other order. The chart's order is a
// keyed table ([Writer.ApplyRoster]) joined on the handle ([assigneeOrder]), so
// a page costs what `sort=title` does: a CASE naming every seat walked the chart
// per row (3 to 18 s a page over 20 000 tasks and 5 000 seats, past the read
// budget), and a bound list is scanned per row under some filters. The
// catalogue's is spelled, bounded by [MaxTypes]. An edit of either changes the
// order, so a cursor minted before it is refused ([ErrBadCursor], [cursorArms]).

// orderBasis is what the derived orders are built from, resolved once per read
// so the page, its cursor and its neighbours are compiled from one answer.
type orderBasis struct {
	// types is the company's type slugs in the catalogue's own order.
	types []string
	// roster is the version of the seats' order `sort=assignee` joins, zero
	// before the first chart apply wrote one.
	roster uint64
}

// resolveOrder reads what this query's order is built from, inside the read's
// own transaction so the catalogue and the seats' order are the ones the rows
// are read against.
func resolveOrder(ctx context.Context, tx *sql.Tx, q Query) (orderBasis, error) {
	var out orderBasis
	if q.sortsBy("type") {
		catalogue, _, err := readTypeCatalogue(ctx, tx)
		if err != nil {
			return orderBasis{}, err
		}
		// EVERY TYPE, ARCHIVED ONES INCLUDED: work filed under a type the
		// company has since retired is still listed, and still has a place.
		for _, t := range EffectiveTypes(catalogue.Types) {
			out.types = append(out.types, t.Slug)
		}
	}
	if q.sortsBy("assignee") {
		var version int64
		err := tx.QueryRowContext(ctx, rosterVersionStatement, RosterSubject().ID).Scan(&version)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return orderBasis{}, fmt.Errorf("worklet: read the seats' order: %w", err)
		default:
			out.roster = uint64(version)
		}
	}
	return out, nil
}

// rosterVersionStatement is [resolveOrder]'s read of the seats' order's
// version, named for the plan test.
const rosterVersionStatement = `SELECT version FROM tracker_rosters WHERE name = ?`

// sortsBy reports whether the query's order names a key, either way round.
func (q Query) sortsBy(key string) bool {
	return slices.ContainsFunc(q.Sort, func(s Sort) bool { return s.Key == key })
}

// seatsByName is every handle in the order of its seat's name, in the Unicode
// Collation Algorithm's root collation (CLDR root, tertiary strength), then the
// name's bytes, then the handle.
//
// A COLLATION, NOT LOWERCASED BYTES, which put "Émile" after "Zoe"; it orders
// as the dashboard's seat lists (`localeCompare`) do. The bytes and the handle
// make the order total, so two seats of one name order alike on every node.
func seatsByName(names map[string]string) []string {
	collator := collate.New(language.Und)
	var buf collate.Buffer
	keys := make(map[string][]byte, len(names))
	out := make([]string, 0, len(names))
	for handle, name := range names {
		keys[handle] = bytes.Clone(collator.KeyFromString(&buf, name))
		buf.Reset()
		out = append(out, handle)
	}
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(
			bytes.Compare(keys[a], keys[b]),
			strings.Compare(names[a], names[b]),
			strings.Compare(a, b),
		)
	})
	return out
}

// derivedOrders are the sort keys an expression orders by, each compiled to
// its terms in the direction asked for, a join it needs named by alias.
// [sortTerms] appends the id after them.
var derivedOrders = map[string]func(basis orderBasis, descending bool, alias string) []sortTerm{
	"key":      keyOrder,
	"status":   statusOrder,
	"type":     typeOrder,
	"assignee": assigneeOrder,
}

// keyOrder is a key's own project, then its number as a number.
//
// THE KEY'S PREFIX, not the task's project column: a reader expects the order
// the key reads in ([taskKeyPattern]: no dash inside the project).
func keyOrder(_ orderBasis, descending bool, _ string) []sortTerm {
	return []sortTerm{
		{Column: "substr(t.key, 1, instr(t.key, '-') - 1)",
			Descending: descending, NeverNull: true},
		{Column: "CAST(substr(t.key, instr(t.key, '-') + 1) AS INTEGER)",
			Descending: descending, NeverNull: true},
	}
}

// statusOrder is the six in the order a board draws them ([Statuses]).
func statusOrder(_ orderBasis, descending bool, _ string) []sortTerm {
	return []sortTerm{{Column: placeIn("t.status", declaredOrder(Statuses), descending),
		NeverNull: true}}
}

// typeOrder is the company's catalogue order, then the types it does not
// declare, by slug.
func typeOrder(basis orderBasis, descending bool, _ string) []sortTerm {
	return []sortTerm{
		{Column: placeIn("t.type", basis.types, descending), NeverNull: true},
		{Column: "t.type", Descending: descending, NeverNull: true},
	}
}

// assigneeOrder is the seats by name, then the handles the chart does not
// hold, by handle, then the unassigned.
//
// A seat's place is its `tracker_seat_places` row, absent for a handle the
// chart does not hold; the unassigned are the second term's NULL. [renderOrder]
// puts an absent value last either way, the unassigned after the seatless
// handles. The roster's version is the order's [sortTerm.Basis], so a cursor
// minted under another roster is refused.
func assigneeOrder(basis orderBasis, descending bool, alias string) []sortTerm {
	return []sortTerm{
		{Column: alias + ".place", Descending: descending,
			Join: " LEFT JOIN tracker_seat_places " + alias + " ON " +
				alias + ".handle = t.assignee",
			Basis: "roster " + strconv.FormatUint(basis.roster, 10)},
		{Column: "NULLIF(t.assignee, '')", Descending: descending},
	}
}

// placeIn is a value's place in a declared order: its position in ranked, then
// one place for every value ranked does not hold.
//
// THE DIRECTION IS IN THE POSITIONS and the term reads ascending, so what
// cannot be placed sorts LAST either way, as an absent date does
// ([renderOrder]): an undeclared type is neither "first" nor "last".
func placeIn(expr string, ranked []string, descending bool) string {
	n := len(ranked)
	if n == 0 {
		// A CASE WITH NO WHEN IS NOT SQL: nothing declared, nothing placed.
		return "0"
	}
	var out strings.Builder
	out.WriteString("CASE " + expr)
	for i, value := range ranked {
		at := i
		if descending {
			at = n - 1 - i
		}
		fmt.Fprintf(&out, " WHEN %s THEN %d", sqlText(value), at)
	}
	fmt.Fprintf(&out, " ELSE %d END", n)
	return out.String()
}

// sqlText is a string literal: single quotes around the text, each one inside
// it doubled. An order is compared and resumed as written ([keyset]), so its
// values are literals rather than bound arguments.
func sqlText(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
