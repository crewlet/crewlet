package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EACH INJECTOR REACHES ITS OWN BRANCH, and the proof is that a control
// transaction and an armed one produce DIFFERENT observable states.
//
// Without the control, "the row is absent" also passes for a fixture that
// never wrote anything, and "the row is present" also passes for an injector
// that never fired. Both halves are asserted here for both injectors.
func TestCommitFaultsReachDifferentBranches(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		fault *storetest.CommitFault
		// durable is what the ARMED transaction leaves behind: a failed
		// commit leaves nothing, a lost acknowledgement leaves everything.
		durable bool
	}{
		"a failed commit is not durable":    {storetest.FailCommitAfter(0, errors.New("disk gave up")), false},
		"a lost acknowledgement IS durable": {storetest.LoseCommitAck(0), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, err := store.Open(t.Context(),
				filepath.Join(t.TempDir(), "faults.db"),
				store.Options{WrapDriver: tc.fault.Wrap})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			ctx := t.Context()

			if err := db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`CREATE TABLE crewlet_commit_probe (mark TEXT PRIMARY KEY)`)
				return err
			}); err != nil {
				t.Fatalf("schema: %v", err)
			}

			// THE CONTROL, unarmed: the same transaction shape against a
			// healthy store, so an assertion below that finds a row knows
			// the fixture can write one.
			if err := db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO crewlet_commit_probe (mark) VALUES ('control')`)
				return err
			}); err != nil {
				t.Fatalf("the control transaction failed: %v", err)
			}
			if !present(t, db, "control") {
				t.Fatal("the control row is absent: this fixture cannot write at all")
			}

			tc.fault.Arm()
			err = db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO crewlet_commit_probe (mark) VALUES ('armed')`)
				return err
			})
			tc.fault.Disarm()

			if err == nil {
				t.Fatal("the armed transaction reported success: both injectors " +
					"report failure to the caller, and that is the whole point of " +
					"the lost-acknowledgement one")
			}
			if !tc.fault.Fired() {
				t.Fatal("the injector never fired: the store retries a conflicted " +
					"transaction, so an assertion on the outcome alone can pass " +
					"with the fault never reached")
			}
			if got := present(t, db, "armed"); got != tc.durable {
				t.Errorf("after the armed transaction the row is present=%v, want %v: "+
					"a failed commit must leave nothing and a lost acknowledgement "+
					"must leave everything — the two are the same error to the "+
					"caller and opposite facts on disk", got, tc.durable)
			}
		})
	}
}

// A LOST ACKNOWLEDGEMENT SURVIVES A REOPEN, which is what makes it durable
// rather than merely visible on the handle that wrote it.
//
// This is the state a caller cannot distinguish from a real failure, and the
// reason a write outcome is three-valued rather than a bool.
func TestALostAcknowledgementIsDurableAcrossAReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lost.db")
	fault := storetest.LoseCommitAck(0)

	db, err := store.Open(t.Context(), path, store.Options{WrapDriver: fault.Wrap})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := t.Context()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`CREATE TABLE crewlet_commit_probe (mark TEXT PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("schema: %v", err)
	}

	fault.Arm()
	writeErr := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO crewlet_commit_probe (mark) VALUES ('armed')`)
		return err
	})
	fault.Disarm()
	if !errors.Is(writeErr, storetest.ErrLostAck) {
		t.Fatalf("write error = %v, want the injected lost acknowledgement", writeErr)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if !present(t, reopened, "armed") {
		t.Error("the row the caller was told did not commit is absent after a " +
			"reopen: LoseCommitAck has to commit BEFORE it reports failure, " +
			"or it is just FailCommitAfter with a different name")
	}
}

func present(t *testing.T, db *store.DB, mark string) bool {
	t.Helper()
	var n int
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT count(*) FROM crewlet_commit_probe WHERE mark = ?`, mark).Scan(&n); err != nil {
		t.Fatalf("probe %q: %v", mark, err)
	}
	return n > 0
}
