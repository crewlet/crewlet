package store_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A SCHEMA PENDING CANNOT READ IS AN ERROR, NOT A FRESH DATABASE. A database
// with no schema_migrations table has applied nothing, and that is the one
// state "nothing applied" describes; a table that is there and fails part way
// through its read is a fault, and answered as nothing applied it would tell a
// deploy gate that a migrated database has every migration still to run.
func TestPendingReportsASchemaItCannotReadAsAnError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "migrated.db")
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// THE CONTROL, unarmed: the same database through the same wrapped
	// driver reports every migration applied, so the failure below is the
	// fault's and not a fixture that cannot read at all.
	errRead := errors.New("the disk went away mid-read")
	fault := storetest.FailReadsAfter(1, errRead)
	schemas, err := store.Pending(t.Context(), path, store.Options{WrapDriver: fault.Wrap})
	if err != nil {
		t.Fatalf("the control Pending: %v", err)
	}
	for _, sch := range schemas {
		if len(sch.Pending) != 0 || len(sch.Applied) != len(store.SchemaVersions(sch.Estate)) {
			t.Fatalf("the control reports the %s estate with %d applied and %d pending, want all applied",
				sch.Estate, len(sch.Applied), len(sch.Pending))
		}
	}

	fault.Arm()
	schemas, err = store.Pending(t.Context(), path, store.Options{WrapDriver: fault.Wrap})
	if !errors.Is(err, errRead) {
		t.Fatalf("Pending over a read that failed = %+v, %v; want the failure, not a database "+
			"that has applied nothing", schemas, err)
	}
}
