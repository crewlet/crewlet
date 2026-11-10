package worklet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// ---- the orders no column holds ----------------------------------------- //
//
// Four sort keys order by what a value MEANS rather than how it is spelled:
// a key by its number (ENG-51 before ENG-185, which text puts the other way),
// a status by the order a board draws the six, a type by the company's own
// catalogue and an assignee by the name a person reads. Each compiles to an
// expression over the task row, so its cursor and its neighbours read through
// the same [keyset] arms every other order does.
//
// AN ORDER BUILT FROM THE CATALOGUE OR THE CHART IS ONE ORDER PER EDIT of it:
// renaming a seat or reordering the types changes the compiled order, so a
// cursor minted before the edit is refused rather than resumed in the middle
// of a different order ([cursorArms]).

// Roster is what a CHART can answer about the seats work is held by.
//
// Defined here and satisfied by the caller, for the reason [Units] is: the
// tracker holds no org, so the name a seat is read by is resolved at read
// time by whoever has the chart.
type Roster interface {
	// SeatNames is every seat the chart holds, by handle, with the name a
	// person reads it by.
	SeatNames() map[string]string
}

// orderBasis is what the derived orders are built from, resolved once per read
// so the page, its cursor and its neighbours are compiled from one answer.
type orderBasis struct {
	// types is the company's type slugs in the catalogue's own order.
	types []string
	// seats is the chart's handles in the order of their names.
	seats []string
}

// resolveOrder reads what this query's order is built from, inside the read's
// own transaction so the type catalogue is the one the rows are read against.
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
	if q.sortsBy("assignee") && q.Roster != nil {
		out.seats = seatsByName(q.Roster.SeatNames())
	}
	return out, nil
}

// sortsBy reports whether the query's order names a key, either way round.
func (q Query) sortsBy(key string) bool {
	return slices.ContainsFunc(q.Sort, func(s Sort) bool { return s.Key == key })
}

// seatsByName is every handle in the order of its seat's name, ignoring case,
// with the handle breaking a tie between two seats of one name.
func seatsByName(names map[string]string) []string {
	out := make([]string, 0, len(names))
	for handle := range names {
		out = append(out, handle)
	}
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(
			strings.Compare(strings.ToLower(names[a]), strings.ToLower(names[b])),
			strings.Compare(names[a], names[b]),
			strings.Compare(a, b),
		)
	})
	return out
}

// derivedOrders are the sort keys an expression orders by, each compiled to
// its terms in the direction asked for. [sortTerms] appends the id after them.
var derivedOrders = map[string]func(basis orderBasis, descending bool) []sortTerm{
	"key":      keyOrder,
	"status":   statusOrder,
	"type":     typeOrder,
	"assignee": assigneeOrder,
}

// keyOrder is a key's own project, then its number as a number.
//
// THE KEY'S PREFIX, not the task's project column: a key is `<PROJECT>-<n>`
// with no dash inside the project ([taskKeyPattern]), and the order a reader
// expects is the one the key they are looking at reads in.
func keyOrder(_ orderBasis, descending bool) []sortTerm {
	return []sortTerm{
		{Column: "substr(t.key, 1, instr(t.key, '-') - 1)",
			Descending: descending, NeverNull: true},
		{Column: "CAST(substr(t.key, instr(t.key, '-') + 1) AS INTEGER)",
			Descending: descending, NeverNull: true},
	}
}

// statusOrder is the six in the order a board draws them ([Statuses]).
func statusOrder(_ orderBasis, descending bool) []sortTerm {
	return []sortTerm{{Column: placeIn("t.status", declaredOrder(Statuses), descending),
		NeverNull: true}}
}

// typeOrder is the company's catalogue order, then the types it does not
// declare, by slug.
func typeOrder(basis orderBasis, descending bool) []sortTerm {
	return []sortTerm{
		{Column: placeIn("t.type", basis.types, descending), NeverNull: true},
		{Column: "t.type", Descending: descending, NeverNull: true},
	}
}

// assigneeOrder is the seats by name, then the handles the chart does not
// hold, by handle, then the unassigned.
func assigneeOrder(basis orderBasis, descending bool) []sortTerm {
	return []sortTerm{
		{Column: placeIn("t.assignee", basis.seats, descending, ""), NeverNull: true},
		{Column: "t.assignee", Descending: descending, NeverNull: true},
	}
}

// placeIn is a value's place in a declared order: its position in ranked, then
// one place for every value ranked does not hold, then the absent values.
//
// THE DIRECTION IS IN THE POSITIONS and the term is read ascending, so what
// cannot be placed sorts LAST in both directions, the rule an absent date
// keeps ([renderOrder]): a type the catalogue never declared and a task nobody
// holds are not the answer to "first" or to "last".
func placeIn(expr string, ranked []string, descending bool, absent ...string) string {
	n := len(ranked)
	var out strings.Builder
	out.WriteString("CASE " + expr)
	for _, value := range absent {
		fmt.Fprintf(&out, " WHEN %s THEN %d", sqlText(value), n+1)
	}
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
