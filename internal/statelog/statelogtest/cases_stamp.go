package statelogtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// SuiteWriter is the node id the suite's publisher stamps, and SuiteGeneration
// the generation its estate's checkpoint is placed at before a candidate
// writes. Neither is a zero value, so a domain that leaves its envelope's
// writer or generation unset cannot pass by matching them.
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
// [statelog.Stamp] — or that installs a gate at any version but the one its
// own changed apply needs ([gateStamp]).
//
// The write path, not [Candidate.Encode], a fixture the test writes: the stamp
// is kept or lost in the domain's own decision, and a record with no writer
// is one the eviction gate never drops. The bytes are read back independently
// of the publisher's own refusal, so a publisher that stopped checking cannot
// take the certification down with it. Exported and returning the verdict for
// [Declaration]'s reason.
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
	node, db := openNodeEstate(t, c)
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
	// Signed: the framework refuses a publisher with no signer.
	signer, err := recordseal.NewSigner(name, suiteRing())
	if err != nil {
		t.Fatalf("build a signer for %s: %v", name, err)
	}
	verifier, err := recordseal.NewVerifier(name, suiteRing())
	if err != nil {
		t.Fatalf("build a verifier for %s: %v", name, err)
	}
	// An applier runs over the publisher's log: a multi-step write path
	// decides each step from the rows the one before made (a rename from the
	// create's row), so without it only the first record could certify.
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: c.Domain, Spec: c.spec(), Applier: c.Applier, Verifier: verifier,
		Fetch: log, Log: log, Node: node, DB: db, Checkpoint: at,
	})
	if err != nil {
		t.Fatalf("build %s's applier: %v", name, err)
	}
	startLoop(t, runner)
	deps := statelog.Deps{
		Domain: c.Domain, Spec: c.spec(),
		Log: log, Signer: signer, Rows: rows,
		Fence: openFence{}, Gates: openGates{},
		Waiter: runner, Voids: runner, Identity: runner,
		NodeID:     SuiteWriter,
		Generation: func() uint32 { return SuiteGeneration },
		// Read back through the verifier: the log holds signed frames.
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		// Under test is what the decision writes, not the log's room.
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
	for _, framed := range records {
		payload, verdict := verifier.Open(framed)
		if verdict != recordseal.Verified {
			return fmt.Errorf("%s's record does not open under the key it was "+
				"signed with (%s)", name, verdict)
		}
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
		if err := gateStamp(c, name, env, payload); err != nil {
			return err
		}
	}
	return nil
}

// gateStamp holds a record that INSTALLS A GATE to the one version a gate may
// carry: exactly what its own changed apply needs, and nothing above it.
//
// An older build HALTS at a gate it cannot read rather than retaining it, since
// a deferred gate would license every record above it. So a gate rises above
// the base only for a field marking a changed apply
// ([statelog.VersionedField.Gate]); an ordinary versioned field on it would
// halt older nodes for nothing. The encoder refuses that
// ([statelog.RecordFields.Ungated]); this checks the published bytes again,
// since the encoder is the code under test.
func gateStamp(c Candidate, name string, env statelog.Envelope, payload []byte) error {
	if !c.Domain.InstallsGate(env) {
		return nil
	}
	want, err := c.Fields.Minimum(env.Op, payload)
	if err != nil {
		return fmt.Errorf("%s: read a %s gate record against its own field "+
			"table: %w", name, env.Kind, err)
	}
	if ungated := c.Fields.Ungated(env.Op, payload); len(ungated) > 0 {
		return fmt.Errorf("%s published a %s record that installs a gate and "+
			"carries %s above its own changed apply — every older build halts "+
			"at a gate it cannot read, so this one would take them out of "+
			"service for a field rather than for a rule they cannot honour",
			name, env.Kind, strings.Join(ungated, ", "))
	}
	if env.V != want {
		return fmt.Errorf("%s published a %s record that installs a gate at "+
			"record version %d, want %d — every build that cannot read a gate "+
			"halts at it, so a gate is raised above the base only by a field "+
			"its table marks VersionedField.Gate, and to exactly that field's "+
			"version", name, env.Kind, env.V, want)
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
// domain keeps one ([statelog.ReservationOf]), and none otherwise.
func admissionFor(t *testing.T, c Candidate) statelog.Admission {
	t.Helper()
	if !statelog.ReservationOf(c.Domain).Kept {
		return nil
	}
	reserve, err := statelog.NewReserve(c.spec(),
		func(context.Context) (statelog.Usage, error) { return statelog.Usage{}, nil })
	if err != nil {
		t.Fatalf("build a reserve over %s: %v", c.Domain.Name(), err)
	}
	return reserve
}

// openFence refuses nothing: what is under test is what the decision writes.
type openFence struct{}

func (openFence) Evicted(context.Context) (bool, error)                 { return false, nil }
func (openFence) ClearForZero(context.Context, statelog.Position) error { return nil }

// openGates reports no gate.
type openGates struct{}

func (openGates) GatedAt(context.Context, statelog.Subject, string, string,
	statelog.Position, []byte) (statelog.Reason, bool, error) {
	return "", false, nil
}

// errNoApplier is what the suite's waiter answers when asked to wait for an
// apply: nothing applies what the suite's publisher appends, so every write
// resolves `pending` at once rather than spending the resolve budget.
var errNoApplier = errors.New("statelogtest: nothing applies what this suite appends")

// suiteWaiter is a node whose rows stand at one position, on the live stream.
type suiteWaiter struct{ at statelog.Position }

func (w suiteWaiter) Reached() statelog.Position                         { return w.at }
func (suiteWaiter) WaitReached(context.Context, statelog.Position) error { return nil }
func (suiteWaiter) StreamIdentity() error                                { return nil }
func (suiteWaiter) Truncated() error                                     { return nil }

// Voided voids nothing: no reanchor places a rule on the suite's log.
func (suiteWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

func (suiteWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return errNoApplier
}

// suiteRing is the keyring every record the suite publishes is signed under.
func suiteRing() recordseal.Keyring { return recordseal.OneKey("suite", "statelogtest-material") }
