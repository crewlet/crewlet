package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
)

// A purge statement holds the node's one writer for as long as it runs, and
// an inline event Append waits behind it no longer than the busy timeout. So
// what one statement deletes is bounded by payload bytes as well as rows: a
// log's rows are up to one whole event each, and a statement of five hundred
// of those would hold the writer for seconds.

// purgeLog opens a log for a purge case.
func purgeLog(t *testing.T) (*DB, *EventLog) {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "purge.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, db.Events()
}

// stale appends a row past retention whose payload weighs size bytes, and
// returns its id.
func stale(t *testing.T, log *EventLog, i, size int) string {
	t.Helper()
	id := fmt.Sprintf("stale-%04d", i)
	payload := `{"x":"` + strings.Repeat("x", size-8) + `"}`
	if err := log.Append(t.Context(), EventRecord{
		ID: id, Type: "task_assigned", Category: "task",
		Time:    now().Add(-EventRetention - time.Hour).Add(time.Duration(i) * time.Millisecond),
		Payload: json.RawMessage(payload),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
	return id
}

// A BATCH STOPS AT WHICHEVER BOUND IT REACHES FIRST, AND TAKES ONE ROW AT LEAST.
//
// Oldest first: the rows that fit the byte bound go together, a row larger
// than the bound on its own goes in a batch of its own rather than never, and
// the row bound holds however small the rows are.
func TestAPurgeBatchStopsAtWhicheverBoundItReachesFirst(t *testing.T) {
	t.Parallel()
	_, log := purgeLog(t)
	const kb = 1 << 10
	for i, size := range []int{10 * kb, 10 * kb, 30 * kb, 10 * kb, 100 * kb, 10 * kb, 1 * kb, 1 * kb, 1 * kb} {
		stale(t, log, i, size)
	}
	if err := log.Append(t.Context(), EventRecord{ID: "recent", Type: "task_assigned",
		Category: "task", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cutoff := EncodeTime(now().Add(-EventRetention))

	// Rows per batch, with a bound of 25 KiB of payload and three rows.
	for i, want := range []struct {
		rows int
		more bool
	}{
		{2, true}, // 10 + 10; the 30 would cross the bound
		{1, true}, // the 30 alone: larger than the bound, and one row at least
		{1, true}, // 10; the 100 would cross the bound
		{1, true}, // the 100 alone
		{3, true}, // 10 + 1 + 1: the row bound, with a row still to come
		{1, false},
	} {
		batch, more, err := log.nextPurgeBatch(t.Context(), cutoff, 3, 25*kb)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.rowids) != want.rows || more != want.more {
			t.Fatalf("batch %d is %d rows (%d bytes), more %v; want %d rows, more %v",
				i, len(batch.rowids), batch.bytes, more, want.rows, want.more)
		}
		if len(batch.rowids) > 1 && batch.bytes > 25*kb {
			t.Fatalf("batch %d is %d rows weighing %d bytes, past the bound", i, len(batch.rowids), batch.bytes)
		}
		if _, err := log.deleteBatch(t.Context(), cutoff, batch); err != nil {
			t.Fatal(err)
		}
	}
	if batch, _, err := log.nextPurgeBatch(t.Context(), cutoff, 3, 25*kb); err != nil || len(batch.rowids) != 0 {
		t.Fatalf("a batch of %d rows after the overhang was deleted (%v)", len(batch.rowids), err)
	}
	if _, err := log.ByID(t.Context(), "recent"); err != nil {
		t.Errorf("a row inside retention is gone: %v", err)
	}
}

// A PURGE OVER MANY PARTS NEVER HOLDS ONE STATEMENT PAST THE BYTE BOUND.
//
// Rows the size of the largest event the transport carries — what a phase
// record's parts are — deleted at the constants the sweep runs at: every
// statement is within [EventPurgeBytes] unless it is one row, and they all go.
func TestAPurgeOverManyPartsNeverTakesAStatementPastTheBound(t *testing.T) {
	t.Parallel()
	_, log := purgeLog(t)
	const parts = 9
	for i := range parts {
		stale(t, log, i, queue.MaxPayloadBytes)
	}
	stale(t, log, parts, 1<<10)
	cutoff := EncodeTime(now().Add(-EventRetention))

	statements, deleted := 0, int64(0)
	for {
		batch, more, err := log.nextPurgeBatch(t.Context(), cutoff, EventPurgeBatch, EventPurgeBytes)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.rowids) == 0 {
			break
		}
		statements++
		if len(batch.rowids) > 1 && batch.bytes > EventPurgeBytes {
			t.Errorf("statement %d deletes %d rows weighing %d bytes, past the %d-byte bound",
				statements, len(batch.rowids), batch.bytes, EventPurgeBytes)
		}
		n, err := log.deleteBatch(t.Context(), cutoff, batch)
		if err != nil {
			t.Fatal(err)
		}
		deleted += n
		if !more {
			break
		}
	}
	if deleted != parts+1 {
		t.Errorf("deleted %d rows of the %d past retention", deleted, parts+1)
	}
	if want := parts * queue.MaxPayloadBytes / EventPurgeBytes; statements < want {
		t.Errorf("%d statements deleted %d rows of %d bytes; the byte bound allows no fewer than %d",
			statements, parts, queue.MaxPayloadBytes, want)
	}
}

// THE PARTY INDEX IS PURGED IN BATCHES, AND ALL OF IT.
//
// Its rows are small, but a multi-day overhang holds several for every event,
// and one statement over all of them would hold the writer for the whole
// overhang, which is what batching the events' own delete exists to stop. A
// loop that stopped after its first batch would leave the rest pointing at
// events the sweep then deletes.
func TestThePartyIndexIsPurgedInBatchesAndAllOfIt(t *testing.T) {
	t.Parallel()
	db, log := purgeLog(t)
	old := now().Add(-EventRetention - time.Hour)
	overhang := 2*eventPartyPurgeBatch + 5
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range overhang {
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES (?, ?, ?)`,
				"lead", EncodeTime(old.Add(time.Duration(i)*time.Microsecond)), uuid.NewString()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO crewlet_event_parties (party, event_time, event_id) VALUES ('lead', ?, 'recent')`,
			EncodeTime(now().Add(-time.Hour)))
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := log.Purge(t.Context()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	var left int
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM crewlet_event_parties`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("%d party rows remain of the %d past retention and the one inside it; want the one",
			left, overhang)
	}
}
