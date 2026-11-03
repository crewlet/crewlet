package iamdomain_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// BenchmarkSeatHolder is what a seat's turn pays to compose the person it acts
// for, once per call of a tool that reads or writes for somebody
// (internal/engine's turnRequesters): ONE seat's holder over a directory of
// people={100,10000} bound people and removed={100,10000} removal tombstones,
// alternating a bound seat and a removed one — read=one, Reader.SeatHolder's
// two seeks — beside read=whole, the whole directory (Reader.SeatHolders) that
// composition read before, which grows with both. ns/op is per question.
//
// The rows are written straight into a migrated estate: a directory of this
// size through the write path would be twenty thousand records applied for a
// fixture the reads never look behind.
func BenchmarkSeatHolder(b *testing.B) {
	for _, size := range []int{100, 10_000} {
		reader := benchDirectory(b, size, size)
		seats := []string{fmt.Sprintf("seat-%05d", size/2), fmt.Sprintf("gone-%05d", size/2)}
		b.Run(fmt.Sprintf("people=%d/removed=%d/read=one", size, size), func(b *testing.B) {
			i := 0
			for b.Loop() {
				h, held, err := reader.SeatHolder(b.Context(), seats[i%2])
				if err != nil || !held || h.Seat != seats[i%2] {
					b.Fatalf("seat %s's holder = (%+v, %v, %v)", seats[i%2], h, held, err)
				}
				i++
			}
		})
		b.Run(fmt.Sprintf("people=%d/removed=%d/read=whole", size, size), func(b *testing.B) {
			for b.Loop() {
				holders, err := reader.SeatHolders(b.Context())
				if err != nil || len(holders) != 2*size {
					b.Fatalf("the directory = %d holders (%v), want %d", len(holders), err,
						2*size)
				}
			}
		})
	}
}

// benchDirectory is a reader over a migrated estate holding people people,
// each bound to seat-NNNNN, and removed tombstones, each naming the seat
// gone-NNNNN it released and no bind has stamped.
func benchDirectory(b *testing.B, people, removed int) *iamdomain.Reader {
	b.Helper()
	node, db := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"),
		store.Options{}, 1)
	b.Cleanup(func() { _ = node.Close() })
	if err := db.Tx(b.Context(), func(tx *sql.Tx) error {
		for i := range people {
			if _, err := tx.ExecContext(b.Context(), `
				INSERT INTO iam_people (id, kind, stage, login, seat_id, created_at,
					updated_at, version, document)
				VALUES (?, 'person', 'active', ?, ?, 1, 1, 1, x'00')`,
				uuid.NewString(), fmt.Sprintf("login-%05d", i),
				fmt.Sprintf("seat-%05d", i)); err != nil {
				return err
			}
		}
		for i := range removed {
			seat := fmt.Sprintf("gone-%05d", i)
			if _, err := tx.ExecContext(b.Context(), `
				INSERT INTO iam_removed (person_id, at, claims_json, seat_id, version)
				VALUES (?, 1, ?, ?, 1)`,
				uuid.NewString(), fmt.Sprintf(`{"seat_id":%q}`, seat), seat); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed the directory: %v", err)
	}
	waiter := &rigWaiter{at: statelog.Position{Stream: iamdomain.Domain{}.Stream().Name}}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB: db.Reader(), Reached: waiter.Reached,
	})
	if err != nil {
		b.Fatalf("build the reader: %v", err)
	}
	return reader
}
