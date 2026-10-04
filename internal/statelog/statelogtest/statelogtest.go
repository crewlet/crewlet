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
// carries, because an older build decides by it — the record that must belong
// to the partition of the log it is on — and the framework's own records to
// none, since each of the domain's logs carries them alike — the record that
// must name the node that wrote it and the generation it was decided in, the
// evictions and releases a domain that claims identity must be able to list —
// because the trim counts nodes per log, and a log whose evictions nothing
// reads counts an evicted node for ever — and the node gate that is the
// domain's own eviction, release and readmission and nothing else, because a
// write flagged one is excused the fences and the reserve every other write is
// held to.
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
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// Candidate is a domain under test, with the two things a suite needs that a
// domain cannot supply on its own: its schema, and a valid record.
type Candidate struct {
	// Domain is the declaration under test.
	Domain statelog.Domain

	// Layout and Log are the layout the domain is certified under and the
	// one of its logs every case runs it on. Both zero take the domain's
	// layout-0 log ([statelog.EstateLayout]), which is where a domain of
	// this build runs; a candidate named for no layout-0 log names a
	// partitioned one, which is how the suite certifies the framework on a
	// log whose names the partition grammar gives.
	Layout statelog.Layout
	Log    statelog.LogID

	// Applier is its state machine.
	Applier statelog.Applier

	// Migrate creates this domain's own tables in a fresh replicated
	// estate. The framework's three are already there.
	Migrate func(ctx context.Context, db store.PartitionHandle) error

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

	// Rows builds the domain's own read seam over an estate, on the log spec
	// names — the one its production publisher decides through.
	Rows func(db store.PartitionReader, spec statelog.StreamSpec) (statelog.Rows, error)

	// Write performs at least one write through the domain's OWN
	// production write path — the writer every caller reaches, not a
	// fixture — over the publisher the suite hands it, which decides from
	// db. It is how [Stamped] reaches the one builder that can forget the
	// framework's stamp.
	Write func(ctx context.Context, pub *statelog.Publisher, db store.PartitionReader) error

	// Generation is the domain's generation record — the one a reanchor opens
	// a generation with — or nil for a domain that keeps none. What the
	// suite reads of it is the record's kind: a generation record is the
	// framework's, on whichever log it is appended to ([Placement]).
	Generation statelog.GenerationEncoder

	// EncodeGate builds the record that evicts nodeID from this domain's log
	// — or, with readmit, takes it back — as the domain's own writer
	// publishes it. Required of a domain that claims identity, whose log the
	// trim counts nodes on; see [Evictions].
	EncodeGate func(nodeID string, readmit bool) ([]byte, error)

	// EncodeRelease builds the record by which nodeID RELEASES this domain's
	// log as it leaves the log's partition — the node's own statement,
	// written by it — as the domain's own writer publishes it. Required of a
	// domain that claims identity, for EncodeGate's reason: a node that left
	// a partition is still counted on the partition's logs until its
	// release's rows say otherwise, and the same gate drops what it writes
	// there afterwards. See [Evictions] and [NodeGates].
	EncodeRelease func(nodeID string) ([]byte, error)
}

// layout is the layout the candidate is certified under.
func (c Candidate) layout() statelog.Layout {
	if c.Layout.Spaces == nil {
		return statelog.EstateLayout(c.Domain.Name())
	}
	return c.Layout
}

// log is the log every case runs the candidate on.
func (c Candidate) log() statelog.LogID {
	if c.Log == (statelog.LogID{}) {
		return statelog.LogID{Domain: c.Domain.Name(), Partition: statelog.EstatePartition}
	}
	return c.Log
}

// spec is that log's stream, as the layout names it.
func (c Candidate) spec() statelog.StreamSpec {
	return c.layout().StreamSpec(c.Domain, c.log())
}

// Factory builds a fresh candidate for one case.
type Factory func(t *testing.T) Candidate

// Run certifies a domain against every contract the framework relies on.
func Run(t *testing.T, new Factory) {
	t.Helper()
	t.Run("declaration", func(t *testing.T) { runDeclaration(t, new) })
	t.Run("placement", func(t *testing.T) { runPlacement(t, new) })
	t.Run("tables", func(t *testing.T) { runTables(t, new) })
	t.Run("envelope", func(t *testing.T) { runEnvelope(t, new) })
	t.Run("apply", func(t *testing.T) { runApply(t, new) })
	t.Run("versions", func(t *testing.T) { runVersions(t, new) })
	t.Run("stamp", func(t *testing.T) { runStamp(t, new) })
	t.Run("evictions", func(t *testing.T) { runEvictions(t, new) })
	t.Run("node gates", func(t *testing.T) { runNodeGates(t, new) })
}

// openEstate brings up a partition with the framework's tables and the
// candidate's own, and answers the handle the runtime would hand the
// candidate's applier and readers.
func openEstate(t *testing.T, c Candidate) store.PartitionHandle {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
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
func countRows(t *testing.T, db store.PartitionHandle, tables map[string]statelog.TableClass) map[string]int {
	t.Helper()
	out := map[string]int{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
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
	if err := statelog.CheckTables(t.Context(), db, c.Domain, c.spec()); err != nil {
		t.Fatal(err)
	}
}
