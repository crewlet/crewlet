// Package statelogtest is the contract suite every statelog domain must pass.
//
// One suite, every domain — the engine's own work tracker, the compacted
// vector domain, anything a later build registers. A domain the suite has not
// certified does not exist as far as the framework is concerned, and a
// divergence between two domains becomes a failing test here rather than a
// production-only surprise on the one node that happens to run the other.
//
// The cases are named for the invariant they defend, and the names are the
// documentation: the table that must carry a class, the stream whose settings
// must agree with its replay protocol, the apply that must produce the same
// rows twice, the envelope that must not fail on a version this build cannot
// read.
//
// # Bringing up a new domain: if a case fails, suspect the case
//
// Not politeness — the base rate, and the sibling suites in this tree record
// what it cost them. A suite's fixtures, constants, helpers and doctrine are
// written by the same mind as its cases, so they fail together, and the first
// thing a new domain finds is usually an assumption the suite made about the
// only domain that existed when it was written.
//
// Two shapes to be especially suspicious of here:
//
//   - A case that assumes ARBITRATION. A compacted domain publishes no
//     per-subject expectation at all — its idempotency is its own row guard —
//     so a case requiring an anchor, an operation ledger or a version guard is
//     encoding the strict domain's shape as a universal rule.
//   - A case that assumes CONTIGUITY. A compacted stream removes interior
//     sequences by design, so "the applier saw every sequence" is true of one
//     protocol and false of the other, and the framework's own loop already
//     branches on exactly that.
//
// Before concluding a domain is at fault, establish that the framework's own
// contracts require what the case demands — [statelog]'s package doc is where
// they are stated, and a property documented at its definition as a permitted
// difference is a permitted difference rather than a failure.
//
// # A control case, expected to come back clean
//
// [Run] ends with a control: a domain that satisfies every contract, run
// through the same cases. A suite that reports a problem in everything it
// touches is a suite nobody reads, and the control is what says the cases can
// come back clean at all.
package statelogtest

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Candidate is a domain under test, with the two things a suite needs that a
// domain cannot supply on its own: its schema, and a valid record.
type Candidate struct {
	// Domain is the declaration under test.
	Domain statelog.Domain

	// Applier is its state machine.
	Applier statelog.Applier

	// Migrate creates this domain's own tables in a fresh replicated
	// estate. The framework's three are already there.
	Migrate func(ctx context.Context, db *store.DB) error

	// Encode builds a valid record for an object of this domain. The
	// suite varies the version to reach the deferral contract, so a
	// version above the domain's own must still ENCODE — it is what a
	// later build publishes and this one has to retain.
	Encode func(kind, id, opID string, version int) ([]byte, error)

	// Kinds are the subject kinds the suite may publish. The first is
	// used wherever one is needed.
	Kinds []string
}

// Factory builds a fresh candidate for one case.
type Factory func(t *testing.T) Candidate

// Run certifies a domain against every contract the framework relies on.
func Run(t *testing.T, new Factory) {
	t.Helper()
	t.Run("declaration", func(t *testing.T) { runDeclaration(t, new) })
	t.Run("tables", func(t *testing.T) { runTables(t, new) })
	t.Run("envelope", func(t *testing.T) { runEnvelope(t, new) })
	t.Run("apply", func(t *testing.T) { runApply(t, new) })
}

// openEstate brings up a replicated estate with the framework's tables and the
// candidate's own.
func openEstate(t *testing.T, c Candidate) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	if c.Migrate != nil {
		if err := c.Migrate(t.Context(), db); err != nil {
			t.Fatalf("create %s's tables: %v", c.Domain.Name(), err)
		}
	}
	return db
}

// countRows is what determinism and idempotency are both asserted over: the
// exact contents of every table the domain declares, in a stable order.
func countRows(t *testing.T, db *store.DB, tables map[string]statelog.TableClass) map[string]int {
	t.Helper()
	out := map[string]int{}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		for name := range tables {
			var n int
			if err := tx.QueryRowContext(t.Context(),
				`SELECT COUNT(*) FROM `+name).Scan(&n); err != nil {
				return err
			}
			out[name] = n
		}
		return nil
	}); err != nil {
		t.Fatalf("count the domain's rows: %v", err)
	}
	return out
}

// runTables certifies that the domain's declared tables accept the framework's
// own statements.
//
// A domain declares three table NAMES and the framework writes their COLUMNS,
// and nothing in Go connects the two. A migration that spelled a column
// differently compiles, migrates, opens and serves every read — and fails the
// first time a record this build cannot decode arrives, which is the rarest
// path in the system and the one whose failure is a stalled log.
func runTables(t *testing.T, new Factory) {
	t.Helper()
	c := new(t)
	db := openEstate(t, c)
	if err := statelog.CheckTables(t.Context(), db, c.Domain); err != nil {
		t.Fatal(err)
	}
}
