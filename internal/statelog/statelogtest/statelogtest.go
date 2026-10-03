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
// read, the version a record is stamped at — the lowest that reads what it
// carries, because an older build decides by it — the record that must name
// the node that wrote it and the generation it was decided in,
// the evictions a domain that claims identity must be able to list — because
// the trim counts nodes per log, and a log whose evictions nothing reads
// counts an evicted node for ever — and the node gate that is the domain's
// own eviction and nothing else, because a write flagged one is excused the
// fences and the reserve every other write is held to.
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
//
// # The gates, for the domains that have them
//
// [RunGates] is a second family, called beside [Run] by every domain whose
// applier installs a deletion marker and an eviction window: it holds the
// publisher-side reader, [statelog.Gates], to the one rule that interface
// states, and the domain's node-gate answer to its own purge and eviction. It
// has no control domain of its own, so it bends the candidate's own reader and
// domain each way either could break its rule and requires every bend to be
// reported.
package statelogtest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	//
	// A VERSION OF ZERO is the writer's own path: the record goes out with
	// whatever version the domain's encoder stamps, which is what the
	// versions case ([Stamping]) reads back. It carries no versioned field.
	Encode func(kind, id, opID string, version int) ([]byte, error)

	// Fields is the domain's versioned-field table — every field its
	// records gained since the base format — or nil when there is none.
	Fields statelog.RecordFields

	// Carrying builds a valid record carrying exactly the named field
	// and no other versioned field, with its version left for the
	// domain's encoder to stamp. Required whenever Fields is not empty:
	// it is what proves each field's path is where the encoder actually
	// writes it, since a path that misses stamps nothing and fails
	// nowhere else.
	Carrying func(field statelog.VersionedField) ([]byte, error)

	// Kinds are the subject kinds the suite may publish. The first is
	// used wherever one is needed.
	Kinds []string

	// Rows builds the domain's own read seam over an estate — the one its
	// production publisher decides through.
	Rows func(db *store.DB) (statelog.Rows, error)

	// Write performs at least one write through the domain's OWN
	// production write path — the writer every caller reaches, not a
	// fixture — over the publisher the suite hands it, which decides from
	// db. It is how [Stamped] reaches the one builder that can forget the
	// framework's stamp.
	Write func(ctx context.Context, pub *statelog.Publisher, db *store.DB) error

	// EncodeGate builds the record that evicts nodeID from this domain's log
	// — or, with readmit, takes it back — as the domain's own writer
	// publishes it. Required of a domain that claims identity, whose log the
	// trim counts nodes on; see [Evictions].
	EncodeGate func(nodeID string, readmit bool) ([]byte, error)
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
	t.Run("versions", func(t *testing.T) { runVersions(t, new) })
	t.Run("stamp", func(t *testing.T) { runStamp(t, new) })
	t.Run("evictions", func(t *testing.T) { runEvictions(t, new) })
	t.Run("node gates", func(t *testing.T) { runNodeGates(t, new) })
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

// tableContents is what determinism and idempotency are both asserted over:
// the exact CONTENTS of every table the domain declares, in a stable order.
//
// IT USED TO BE `SELECT COUNT(*)`, under a comment claiming exact contents.
// Two nodes that applied one record and wrote different bytes into the same
// number of rows passed — which is the whole failure the determinism contract
// exists to catch, since an applier that reads a clock, a map iteration order
// or this node's own id produces exactly that: the right shape, the wrong
// values, on one node out of three. The counts were also what an encrypted
// domain would have been certified by, and ciphertext that differs per node is
// invisible to a count.
//
// Rows are rendered as text and ordered by every column, so the comparison
// needs no per-domain key and no knowledge of what any column means.
func tableContents(t *testing.T, db *store.DB, tables map[string]statelog.TableClass) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		for name := range tables {
			rendered, err := renderTable(t.Context(), tx, name)
			if err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
			out[name] = rendered
		}
		return nil
	}); err != nil {
		t.Fatalf("read the domain's rows: %v", err)
	}
	return out
}

// renderTable is one table as a stable string: every column of every row, in
// an order the rows themselves decide.
func renderTable(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	// ORDERED BY EVERY COLUMN rather than by a primary key this suite does
	// not know: without an ORDER BY the engine may hand back rows in any
	// order it likes, and a comparison over that would fail on two
	// identical tables.
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+name)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var rendered []string
	for rows.Next() {
		cells := make([]any, len(cols))
		into := make([]any, len(cols))
		for i := range cells {
			into[i] = &cells[i]
		}
		if err := rows.Scan(into...); err != nil {
			return "", err
		}
		var line strings.Builder
		for i, cell := range cells {
			if i > 0 {
				line.WriteByte(0x1f)
			}
			// BYTES AS BYTES. A []byte column rendered with %v is a
			// list of numbers and a string is not, so two tables
			// holding the same value under different column types
			// would compare unequal; %q over the byte form makes
			// both render the same way and keeps a NULL distinct
			// from an empty string.
			switch v := cell.(type) {
			case nil:
				line.WriteString("NULL")
			case []byte:
				line.WriteString(strconv.Quote(string(v)))
			case string:
				line.WriteString(strconv.Quote(v))
			default:
				fmt.Fprintf(&line, "%v", v)
			}
		}
		rendered = append(rendered, line.String())
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	slices.Sort(rendered)
	return strings.Join(rendered, "\n"), nil
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
