package iamdomain_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SWEEP DELETES BY POSITION RANGE, NEVER BY A CLOCK.
//
// THE case this whole mechanism exists for. These rows are identity-claimed:
// N nodes assert their replicated tables are byte-identical. A node sweeping
// on its own clock would hold different bytes from its peers, and the claim
// would quietly become a claim about how synchronised their clocks were —
// checkable by nothing, false whenever an NTP correction landed, and
// indistinguishable from an applier that had diverged.
//
// So the publisher resolves each horizon to a POSITION, once, and every node
// deletes exactly the same rows.
//
// THE CLOCK THAT COULD DIFFER IS THE BATCH INSTANT. Every other instant the
// applier can reach is the BROKER's, stamped on the record and identical on
// every node — which is a structural guarantee rather than a rule somebody
// follows: [applyContext] carries the broker's instant and does not carry
// ApplyOptions.Now at all, so there is nothing per-node for an arm here to
// read. This case varies the one that IS per-node and asserts it changes
// nothing, and then asserts the POSITION is what decided.
func TestTheSweepDeletesByPositionRangeNeverByAClock(t *testing.T) {
	t.Parallel()

	// TWO NODES, TWO BATCH CLOCKS, ONE RECORD. The skew is a year, which
	// is past every horizon this domain keeps — so a sweep that read the
	// batch instant at all would delete a different set on each.
	early := brokerAt.Add(-365 * 24 * time.Hour)
	late := brokerAt.Add(365 * 24 * time.Hour)

	first := sweepRig(t, early)
	second := sweepRig(t, late)
	if got, want := first.trail(), second.trail(); !equalRows(got, want) {
		t.Fatalf("the two nodes started from different rows: %v vs %v", got, want)
	}

	// The record's own horizon: everything below position 3, which is the
	// first two trail rows and not the third.
	record := sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
		Changes: 3,
	})
	first.apply(record, early)
	second.apply(record, late)

	got, want := first.trail(), second.trail()
	if !equalRows(got, want) {
		t.Fatalf("two nodes a year apart on their batch clocks deleted "+
			"different rows:\n node with the early clock kept %v\n node with "+
			"the late clock kept %v\n— the sweep is reading a per-node clock, "+
			"so 'N byte-identical copies' is a claim about how synchronised "+
			"they are", got, want)
	}
	if len(got) != 1 {
		t.Errorf("the sweep kept %d rows, want the one at or above the "+
			"horizon: %v", len(got), got)
	}
}

// A ZERO HORIZON DELETES NOTHING, which is the correct reading of "this
// company has no rows old enough yet" and is NOT the same as "everything".
//
// A zero is below every row's version, so the arithmetic already says so —
// this case exists because the OPPOSITE reading is the one somebody writes by
// accident, and it would empty the authentication trail of a company on its
// first tick.
func TestAZeroHorizonSweepsNothing(t *testing.T) {
	t.Parallel()
	rig := sweepRig(t, brokerAt)
	before := rig.trail()
	rig.apply(sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
	}), brokerAt)
	if after := rig.trail(); !equalRows(before, after) {
		t.Errorf("a sweep with no horizon deleted rows: %v became %v — a zero "+
			"position is below every row's version, and reading it as "+
			"'everything' empties a new company's trail on its first tick",
			before, after)
	}
}

// AND A SWEEP NEVER EXCEEDS ONE TRANSACTION'S ROW BUDGET.
//
// The budget is per TRANSACTION and the sweep is per BUCKET, and the two are
// not the same bound: a bucket unswept for a month is not a sixty-fourth of a
// tick, it is a sixty-fourth of a month. Without the cap, one record's apply
// holds this store's only writer for as long as that takes, with every other
// domain's apply waiting behind it.
//
// THE BUCKET CONVERGES ANYWAY, which is what makes the cap cheap: what is left
// is swept on the next tick.
func TestTheSweepNeverExceedsOneTransactionsBudget(t *testing.T) {
	t.Parallel()
	rig := sweepRig(t, brokerAt)

	// More trail rows than one transaction may delete, in one bucket.
	const over = iamdomain.MaxSweepRows + 25
	rig.fillTrail(over)

	rows := rig.apply(sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
		Changes: 1 << 40,
	}), brokerAt)
	if rows > iamdomain.MaxSweepRows {
		t.Errorf("one sweep record deleted %d rows and the budget is %d — this "+
			"transaction holds the store's only writer, and every other "+
			"domain's apply is waiting behind it", rows, iamdomain.MaxSweepRows)
	}
	if rows == 0 {
		t.Fatal("the sweep deleted nothing, so the cap is not what bounded it")
	}
	// AND WHAT IS LEFT IS SWEPT NEXT TICK, which is what makes the cap a
	// bound on one transaction rather than on the retention itself.
	remaining := len(rig.trail())
	if remaining == 0 {
		t.Fatal("one record swept everything, so the cap did not apply and " +
			"this case is asserting nothing")
	}
	for range 3 {
		rig.apply(sweepRecord(t, iamdomain.Sweep{
			V: iamdomain.DocumentVersion, Bucket: iamdomain.BucketOf(sweptPerson),
			Changes: 1 << 40,
		}), brokerAt)
	}
	if after := len(rig.trail()); after >= remaining {
		t.Errorf("three more sweeps left %d rows of %d — a bucket past the cap "+
			"must converge, or its retention never takes effect at all",
			after, remaining)
	}
}

// A SWEEP NAMING A BUCKET THIS ESTATE DOES NOT HAVE IS REFUSED.
//
// It would otherwise delete nothing, silently, for ever — a retention that
// reports itself running and collects nothing, which is the failure mode every
// horizon in this tree is written to avoid.
func TestASweepPastTheBucketCountIsRefused(t *testing.T) {
	t.Parallel()
	rig := sweepRig(t, brokerAt)
	record := sweepRecord(t, iamdomain.Sweep{
		V: iamdomain.DocumentVersion, Bucket: iamdomain.Buckets + 1, Changes: 1 << 40,
	})
	err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := rig.applier.Apply(t.Context(), tx, record,
			statelog.ApplyOptions{Now: brokerAt, StoredAt: brokerAt})
		return err
	})
	if err == nil {
		t.Error("a sweep naming a bucket outside the estate applied — it would " +
			"delete nothing, for ever, while reporting itself as retention " +
			"that runs")
	}
}

// --- the rig ---------------------------------------------------------------- //

const sweptPerson = "018f3a9c-0000-7000-8000-00000000beef"

// sweepRigT is one node's applier and store, with a trail already in it.
type sweepRigT struct {
	*writeRig
	written int
}

// sweepRig builds a node whose clock is `now`, with three trail rows at
// positions 1, 2 and 3.
func sweepRig(t *testing.T, now time.Time) *sweepRigT {
	t.Helper()
	rig := &sweepRigT{writeRig: newWriteRig(t)}
	for seq := range 3 {
		rig.write(uint64(seq+1), now)
	}
	return rig
}

// write puts one trail row in, through the APPLIER rather than through SQL, so
// the rows are the ones a record actually produces.
func (r *sweepRigT) write(seq uint64, now time.Time) {
	r.t.Helper()
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.RecordVersion, OpID: opIDFor(seq),
			Subject: iamdomain.PersonSubject(sweptPerson),
			Op:      iamdomain.OpStatus, Gen: 0, Writer: "node-a",
			Scope: iamdomain.PeopleScope(sweptPerson),
		},
		Person: sweptPerson, Actor: "suite", ActorKind: iam.KindMachine,
		Mutation: mustJSON(r.t, iamdomain.StatusChange{
			V: iamdomain.DocumentVersion, Stage: iam.StageActive,
		}),
	})
	if err != nil {
		r.t.Fatalf("encode: %v", err)
	}
	r.apply(statelog.Record{
		Envelope: statelog.Envelope{
			V: iamdomain.RecordVersion, Kind: string(iamdomain.KindPerson),
			Subject: statelog.Subject{
				Kind: string(iamdomain.KindPerson), ID: sweptPerson,
			},
			Op: string(iamdomain.OpStatus), OpID: opIDFor(seq),
		},
		Position: statelog.Position{
			Stream: iamdomain.Domain{}.Stream().Name, Seq: seq,
		},
		Payload: payload, StoredAt: brokerAt,
	}, now)
	r.written++
}

// fillTrail puts n more trail rows in, at ascending positions.
func (r *sweepRigT) fillTrail(n int) {
	r.t.Helper()
	for i := range n {
		r.write(uint64(r.written+i+1), brokerAt)
	}
}

// apply runs one record through this node's applier and returns the rows it
// wrote or deleted.
//
// `now` IS THE BATCH INSTANT AND NOTHING ELSE. StoredAt stays the broker's own
// stamp, identical on every node, because that is what it is: a record carries
// one instant from the broker and every node applying it sees that one. The
// split is the whole point of the skew case — varying both would prove only
// that two different records produce two different states.
func (r *sweepRigT) apply(rec statelog.Record, now time.Time) int {
	r.t.Helper()
	var rows int
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		var err error
		rows, err = r.applier.Apply(r.t.Context(), tx, rec,
			statelog.ApplyOptions{Now: now, StoredAt: rec.StoredAt})
		return err
	}); err != nil {
		r.t.Fatalf("apply: %v", err)
	}
	return rows
}

// trail is every trail row's id, in order — the thing two nodes must agree on.
func (r *sweepRigT) trail() []string {
	return r.column(`SELECT id FROM iam_history ORDER BY id`)
}

// sweepRecord wraps one sweep payload as the framework delivers it.
func sweepRecord(t *testing.T, sweep iamdomain.Sweep) statelog.Record {
	t.Helper()
	subject := iamdomain.SweepSubject(sweep.Bucket)
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.RecordVersion, OpID: "sweep-" + subject.ID,
			Subject: subject, Op: iamdomain.OpSweep, Writer: "node-a",
			Scope: iamdomain.BucketScope(sweep.Bucket % iamdomain.Buckets),
		},
		Actor: "engine", ActorKind: iam.KindEngine,
		Mutation: mustJSON(t, sweep),
	})
	if err != nil {
		t.Fatalf("encode a sweep: %v", err)
	}
	return statelog.Record{
		Envelope: statelog.Envelope{
			V: iamdomain.RecordVersion, Kind: string(iamdomain.KindSweep),
			Subject: statelog.Subject{
				Kind: string(iamdomain.KindSweep), ID: subject.ID,
			},
			Op: string(iamdomain.OpSweep), OpID: "sweep-" + subject.ID,
		},
		Position: statelog.Position{
			Stream: iamdomain.Domain{}.Stream().Name, Seq: 1 << 30,
		},
		Payload: payload, StoredAt: brokerAt,
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func opIDFor(seq uint64) string {
	return "op-" + string(rune('a'+int(seq%26))) + itoa(int(seq))
}

func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = context.Background
