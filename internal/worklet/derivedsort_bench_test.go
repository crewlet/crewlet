package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// BenchmarkADerivedOrder is one read of the work list in an order built from
// the type catalogue or the org chart, beside `sort=title`, an order no index
// serves either, over [corpusRows] tasks held by every seat of a chart of 500
// or 5 000:
//
//	go test ./internal/worklet -run '^$' -bench BenchmarkADerivedOrder -benchtime 10x
//
// EACH READ A PAGE MAKES: the first page, a page from the middle of the order,
// and where one task sits with its neighbours (`around=`). The assignee order
// reads a row's place through one seek of the roster's places
// ([assigneeOrder]), so every read here costs about what the title's does, at
// either chart size and at any depth. Spelled as a CASE naming every seat, it
// cost 3 to 5 s for a first page and 10 to 18 s for a later one or a task's
// neighbours at 5 000 seats, past the estate's read budget.
func BenchmarkADerivedOrder(b *testing.B) {
	for _, seats := range []int{500, 5_000} {
		db := derivedOrderStore(b, seats)
		basis := derivedOrderBasis()
		for _, sort := range []string{"title", "type", "assignee", "-assignee"} {
			for _, read := range []string{"first page", "later page", "around"} {
				b.Run(fmt.Sprintf("seats=%d/sort=%s/%s", seats, sort, read), func(b *testing.B) {
					q := derivedOrderQuery(b, sort, read, db, basis, seats)
					for b.Loop() {
						if err := db.Read(b.Context(), func(tx *sql.Tx) error {
							return readDerivedOrder(b.Context(), tx, q, read, basis)
						}); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// derivedOrderMiddle is the task a later page resumes after and the around read
// places: one in the middle of the corpus, held by the seat whose name is in
// the middle of the chart's order, so neither end of any order is near it.
func derivedOrderMiddle(seats int) string {
	return fmt.Sprintf("t-%04d", corpusRows/2+seats/2)
}

// derivedOrderStore is the plan corpus ([taskRow]) with its work held by every
// seat of a chart of seats, four tasks to a seat at 5 000, and the roster's
// places those seats' names give them, as a chart apply leaves them.
func derivedOrderStore(b *testing.B, seats int) store.ReplicatedHandle {
	b.Helper()
	dbNode, db := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"), store.Options{}, 1)
	b.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			b.Errorf("close the store: %v", err)
		}
	})
	names := make(map[string]string, seats)
	for i := range seats {
		names[fmt.Sprintf("h-%d", i)] = fmt.Sprintf("Seat %05d", seats-i)
	}
	ordered := seatsByName(names)
	if err := db.Tx(b.Context(), func(tx *sql.Tx) error {
		if err := insertAll(b.Context(), tx, db.Caps().MaxVariables, insertTask,
			taskValues, corpusRows, func(i int) []any {
				row := taskRow(i)
				row[11] = fmt.Sprintf("h-%d", i%seats)
				return row
			}); err != nil {
			return err
		}
		return insertAll(b.Context(), tx, db.Caps().MaxVariables,
			`INSERT INTO tracker_seat_places (handle, place) VALUES`, `(?,?)`,
			len(ordered), func(i int) []any { return []any{ordered[i], i} })
	}); err != nil {
		b.Fatalf("seed the corpus: %v", err)
	}
	return db
}

// derivedOrderBasis is the builtin catalogue and the roster
// [derivedOrderStore] placed the seats by.
func derivedOrderBasis() orderBasis {
	basis := orderBasis{roster: 1}
	for _, kind := range EffectiveTypes(nil) {
		basis.types = append(basis.types, kind.Slug)
	}
	return basis
}

// derivedOrderQuery is the workspace's whole list, finished work included, in
// one order, with the cursor a later page resumes from or the task an around
// read places.
func derivedOrderQuery(b *testing.B, sort, read string, db store.ReplicatedHandle,
	basis orderBasis, seats int) Query {

	b.Helper()
	q, err := ParseQuery(MapParams(map[string]any{"sort": sort, "show_closed": "true",
		"subtasks": "separate"}), planNow, time.UTC)
	if err != nil {
		b.Fatalf("ParseQuery: %v", err)
	}
	switch read {
	case "later page":
		terms := sortTerms(q, nil, basis)
		if err := db.Read(b.Context(), func(tx *sql.Tx) error {
			keys, err := sortValuesOf(b.Context(), tx, terms, derivedOrderMiddle(seats))
			q.Cursor = mintCursor(keys, cursorOrder(terms))
			return err
		}); err != nil {
			b.Fatalf("read the middle task's place: %v", err)
		}
	case "around":
		q.Around = derivedOrderMiddle(seats)
	}
	return q
}

// readDerivedOrder makes the read [Reader.Tasks] makes for q: a page, from the
// cursor when q carries one, or the around task's place and neighbours.
func readDerivedOrder(ctx context.Context, tx *sql.Tx, q Query, read string,
	basis orderBasis) error {

	set, err := compile(q, planNow, nil)
	if err != nil {
		return err
	}
	terms := sortTerms(q, nil, basis)
	if read == "around" {
		hint, capped, err := countHint(ctx, tx, set)
		if err != nil {
			return err
		}
		answer := Answer{TotalHint: hint, TotalCapped: capped}
		around, err := readAround(ctx, tx, q, nil, set, terms, &answer, nil)
		if err == nil && around == nil {
			err = fmt.Errorf("the around read did not place %s", q.Around)
		}
		return err
	}
	resume, err := pageClause(q, nil, basis)
	if err != nil {
		return err
	}
	rows, _, err := readPage(ctx, tx, set, resume, terms, PageDefault, planNow)
	if err == nil && len(rows) != PageDefault {
		err = fmt.Errorf("the page holds %d rows, want %d", len(rows), PageDefault)
	}
	return err
}
