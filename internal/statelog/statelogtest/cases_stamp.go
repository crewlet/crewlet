package statelogtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// SuiteWriter is the node id the suite's publisher stamps, and SuiteGeneration
// the generation its estate's checkpoint is placed at before a candidate
// writes.
//
// NEITHER IS A ZERO VALUE, deliberately: a domain that wrote an empty writer
// or generation zero into its envelope would pass a suite whose own node were
// unnamed and whose estate were at the first generation — which is exactly
// what every domain in this tree did, for as long as nothing checked.
const (
	SuiteWriter     = "suite-writer"
	SuiteGeneration = uint32(5)
)

// runStamp reports what [Stamped] found.
func runStamp(t *testing.T, new Factory) {
	t.Helper()
	t.Run("every record its own write path publishes names its writer and generation",
		func(t *testing.T) {
			if err := Stamped(t, new); err != nil {
				t.Fatal(err)
			}
		})
}

// Stamped runs the candidate's own write path once, over a publisher this
// suite builds, and reports a record that does not carry the framework's
// [statelog.Stamp].
//
// # Why the write path and not the encoder
//
// [Candidate.Encode] is a fixture the TEST writes, so a suite that checked it
// would certify the test's idea of a record. The stamp is lost or kept in the
// domain's own decision — the one builder every production write goes through
// — and that is only reached by writing through it. Every domain in this tree
// had a writer field and a generation field on its envelope, and not one of
// their writers set either, so the applier's eviction gate compared an empty
// writer against every eviction on the log and dropped nothing.
//
// The publisher refuses an unstamped record on its own; this reads the bytes
// back independently of that refusal, so a publisher that stopped checking
// would not take the certification down with it.
//
// EXPORTED AND RETURNING THE VERDICT, for [Declaration]'s reason: the suite's
// own tests hand it a domain that forgets, and read the refusal back.
func Stamped(t *testing.T, new Factory) error {
	t.Helper()
	c := new(t)
	name := c.Domain.Name()
	switch {
	case c.Write == nil:
		return fmt.Errorf("%s supplies no write path, so nothing can show that "+
			"the records it publishes name the node that wrote them — the "+
			"eviction gate reads exactly that, on every applier", name)
	case c.Rows == nil:
		return fmt.Errorf("%s supplies no read seam for its write path to "+
			"decide through", name)
	}
	db := openEstate(t, c)
	stream := c.spec().Name
	at := statelog.Position{Stream: stream, Generation: SuiteGeneration}
	if err := seedCheckpoint(t.Context(), db, at); err != nil {
		t.Fatalf("place %s's checkpoint at generation %d: %v", name, SuiteGeneration, err)
	}
	rows, err := c.Rows(db.Reader(), c.spec())
	if err != nil {
		t.Fatalf("build %s's read seam: %v", name, err)
	}
	log := newSuiteLog()
	deps := statelog.Deps{
		Domain: c.Domain, Spec: c.spec(),
		Log: log, Records: log, Rows: rows,
		Fence: openFence{}, Gates: openGates{},
		Waiter: suiteWaiter{at: at}, Voids: suiteWaiter{at: at}, Identity: suiteWaiter{at: at},
		NodeID:     SuiteWriter,
		Generation: func() uint32 { return SuiteGeneration },
		// A RESERVE OVER A LOG WITH NO CEILING where the domain keeps
		// one: what is under test is what the decision writes, not the
		// log's room.
		Admission: admissionFor(t, c),
	}
	pub, err := statelog.NewPublisher(deps)
	if err != nil {
		t.Fatalf("build a publisher over %s: %v", name, err)
	}
	if err := c.Write(t.Context(), pub, db.Reader()); err != nil {
		return fmt.Errorf("%s's own write path failed through the framework's "+
			"publisher — a refusal naming the writer or the generation means "+
			"its decision does not put the stamp it is handed on its "+
			"envelope: %w", name, err)
	}
	records := log.appended()
	if len(records) == 0 {
		return fmt.Errorf("%s's write path appended nothing, so it certifies "+
			"nothing — the write it supplies has to publish a record", name)
	}
	for _, payload := range records {
		env, err := c.Domain.Envelope(payload)
		if err != nil {
			return fmt.Errorf("%s published a record its own envelope reader "+
				"refuses: %w", name, err)
		}
		if env.Writer != SuiteWriter || env.Gen != SuiteGeneration {
			return fmt.Errorf("%s published a %s record naming writer %q at "+
				"generation %d, from a publisher that is %q over an estate at "+
				"generation %d — every applier's eviction gate reads that "+
				"writer, and a record naming nobody is one no eviction can drop",
				name, env.Kind, env.Writer, env.Gen, SuiteWriter, SuiteGeneration)
		}
	}
	return nil
}

// seedCheckpoint places a fresh estate's checkpoint for one stream, which is
// the row a snapshot reads the generation it stamps from.
func seedCheckpoint(ctx context.Context, db store.ReplicatedHandle, at statelog.Position) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		now := store.EncodeTime(time.Unix(1_700_000_000, 0).UTC())
		_, err := tx.ExecContext(ctx, `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?, ?, ?, ?, ?, 0)`,
			at.Stream, int64(at.Generation), int64(at.Seq), now, now)
		return err
	})
}

// admissionFor is a reserve over a log with no ceiling where the candidate's
// domain keeps one, and none where it keeps none — [statelog.KeepsGateReserve]'s
// rule, which every publisher, read index and sweep is held to.
func admissionFor(t *testing.T, c Candidate) statelog.Admission {
	t.Helper()
	if !statelog.KeepsGateReserve(c.Domain) {
		return nil
	}
	reserve, err := statelog.NewReserve(c.spec().Name,
		func(context.Context) (statelog.Usage, error) { return statelog.Usage{}, nil })
	if err != nil {
		t.Fatalf("build a reserve over %s: %v", c.Domain.Name(), err)
	}
	return reserve
}

// openFence refuses nothing: the suite's node is neither evicted nor below
// any floor, because what is under test is what the decision writes.
type openFence struct{}

func (openFence) Evicted(context.Context) (bool, error)                 { return false, nil }
func (openFence) ClearForZero(context.Context, statelog.Position) error { return nil }

// openGates reports no gate.
type openGates struct{}

func (openGates) GatedAt(context.Context, statelog.Subject, string, string,
	statelog.Position) (statelog.Reason, bool, error) {
	return "", false, nil
}

// errNoApplier is what the suite's waiter answers when asked to wait for an
// apply: nothing applies what the suite's publisher appends, so every write
// resolves `pending` at once rather than spending the resolve budget.
var errNoApplier = errors.New("statelogtest: nothing applies what this suite appends")

// suiteWaiter is a node whose rows stand at one position, on the live stream.
type suiteWaiter struct{ at statelog.Position }

func (w suiteWaiter) Committed() statelog.Position                         { return w.at }
func (suiteWaiter) WaitCommitted(context.Context, statelog.Position) error { return nil }
func (suiteWaiter) StreamIdentity() error                                  { return nil }
func (suiteWaiter) Truncated() error                                       { return nil }

// Voided voids nothing: no reanchor places a rule on the suite's log.
func (suiteWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

func (suiteWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return errNoApplier
}
