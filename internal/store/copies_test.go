package store_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A COPY HOLDING THE LOG IS LISTED, AND WHAT IT HELD BACK IS MEASURED — by the
// last copy that finished, and never by one that failed.
//
// A copy stops every checkpoint at its moment for its whole run, so the -wal
// beside the live file grows by what is committed meanwhile, on the volume the
// node commits to (copies.go). What reads that off a handle is the alarms and
// the room checks above this package: which copies hold the log now
// (DB.Copies), and what a copy taken now would hold back (DB.CopyHoldsBack).
// Here a copy is held at its VACUUM INTO while the applier's pinned writer
// commits beside it: the copy is listed while it runs and not after, the log
// grows under it — the cost the measure exists for — and the measure covers at
// least what the case saw it grow by. A copy that then fails with nothing
// committed beside it changes nothing, though measured it would read as a copy
// that held nothing back; and a copy that finishes with nothing committed
// beside it does replace the measure, with nothing, since the last copy is the
// best guess at the write rate the next one will see.
func TestACopyIsListedWhileItHoldsTheLogAndMeasuresWhatItHeldBack(t *testing.T) {
	t.Parallel()
	hold := storetest.NewCopyHold()
	_, db := openReplicated(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{WrapDriver: hold.Wrap}, 1)
	ctx := t.Context()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`CREATE TABLE crewlet_copy_held (id INTEGER PRIMARY KEY, pad BLOB NOT NULL)`)
		return err
	}); err != nil {
		t.Fatalf("create the table the writer fills: %v", err)
	}
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("pin the applier's writer: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	next := 0
	write := func() {
		for range heldWrites {
			if err := w.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO crewlet_copy_held (id, pad)
					VALUES (?, zeroblob(?))`, next, heldRowBytes)
				return err
			}); err != nil {
				t.Fatalf("commit row %d: %v", next, err)
			}
			next++
		}
	}
	if held := db.CopyHoldsBack(1 << 30); held != 0 {
		t.Fatalf("before any copy the handle expects a copy to hold back %d, want "+
			"nothing: no copy has measured it", held)
	}

	// THE FILE'S SIZE WHEN THE HOLD WAS TAKEN, which is what the copy
	// measures its share against: so the measure, applied to it, is the
	// growth itself, and every later comparison is made at it — a fixed
	// size, so nothing the file does afterwards moves the figure compared.
	var listed []store.Copy
	var size, grew int64
	storetest.HoldCopy(t, db, hold, func() {
		listed = db.Copies()
		size = fileBytes(t, db.Path())
		before := fileBytes(t, db.Path()+"-wal")
		write()
		grew = fileBytes(t, db.Path()+"-wal") - before
	})
	if len(listed) != 1 || !strings.HasSuffix(listed[0].Dest, ".part") || listed[0].Since.IsZero() {
		t.Fatalf("while a copy held the log the handle listed %+v, want the one copy, "+
			"writing its part file, and when it began", listed)
	}
	if after := db.Copies(); len(after) != 0 {
		t.Errorf("a copy that returned is still listed: %+v", after)
	}
	if grew <= 0 {
		t.Fatalf("the log grew %d bytes under %d commits while a copy held it — a "+
			"checkpoint passed the copy's moment, which is the hold this measure is about",
			grew, heldWrites)
	}
	measured := db.CopyHoldsBack(size)
	if measured < grew {
		t.Fatalf("the handle expects a copy of the %d-byte file to hold back %d, short of "+
			"the %d the log grew by under the last one", size, measured, grew)
	}
	t.Logf("the log grew %d bytes under %d commits of %d bytes while the copy of the "+
		"%d-byte file held it; a copy of it is expected to hold back %d",
		grew, heldWrites, heldRowBytes, size, measured)

	// A COPY THAT FAILED CHANGES NOTHING: with nothing committed beside it,
	// a measure taken of it would read as a copy that held nothing back.
	hold.Arm()
	defer hold.Release(errors.New("the case ended before it let the copy through"))
	failed := make(chan error, 1)
	go func() {
		_, err := db.Backup(ctx, filepath.Join(t.TempDir(), "failed.db"))
		failed <- err
	}()
	if got := hold.Wait(1, storetest.CopyHoldWait); got < 1 {
		t.Fatalf("the copy that is to fail did not reach its VACUUM INTO within %s",
			storetest.CopyHoldWait)
	}
	hold.Release(errCopyFailed)
	if err := <-failed; !errors.Is(err, errCopyFailed) {
		t.Fatalf("the copy failed at its VACUUM INTO answered %v, want %v", err, errCopyFailed)
	}
	if held := db.CopyHoldsBack(size); held != measured {
		t.Fatalf("after a copy failed the handle expects a copy to hold back %d, want "+
			"the %d the last copy that finished measured", held, measured)
	}

	// AND THE LAST COPY THAT FINISHED IS THE MEASURE, however little it held.
	storetest.HoldCopy(t, db, hold, func() {})
	if held := db.CopyHoldsBack(size); held != 0 {
		t.Fatalf("after a copy that held nothing back the handle expects %d, want "+
			"nothing — the copy before it is no longer the nearest guess", held)
	}
}

const (
	// heldWrites and heldRowBytes are what the writer commits while a copy
	// holds the log: sixty-four commits of a row four kibibytes wide, each at
	// least a page of the log, so the growth is hundreds of kibibytes — well
	// past what one stray commit could account for either way.
	heldWrites   = 64
	heldRowBytes = 4096
)

// errCopyFailed is what a held copy is made to fail with.
var errCopyFailed = errors.New("the copy was failed at its VACUUM INTO")

// fileBytes is the size of the file at path, zero when there is none — which
// is what a database with nothing uncheckpointed has beside it.
func fileBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("measure %s: %v", path, err)
	}
	return info.Size()
}
