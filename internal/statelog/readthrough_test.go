package statelog_test

import (
	"context"
	"database/sql"
	"testing"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE CARRY NAMES THE ROWS, AND NOTHING ELSE.
//
// A read transaction's checkpoint row is carried to the position the read was
// released at only where every record between the two is one this node
// reached without committing — a barrier, which writes no row — which holds
// on the row's own stream and generation alone, and only forward.
func TestTheCheckpointIsCarriedOnlyOverWhatTheReadWasReleasedPast(t *testing.T) {
	t.Parallel()
	at := func(stream string, gen uint32, seq uint64) statelog.Position {
		return statelog.Position{Stream: stream, Generation: gen, Seq: seq}
	}
	for _, c := range []struct {
		name                string
		row, released, want statelog.Position
	}{
		{"released past the row on its generation: the barriers between are carried",
			at("LOG", 2, 10), at("LOG", 2, 13), at("LOG", 2, 13)},
		{"released at the row: nothing to carry",
			at("LOG", 2, 10), at("LOG", 2, 10), at("LOG", 2, 10)},
		{"a row committed past the release: the rows hold what the row says",
			at("LOG", 2, 14), at("LOG", 2, 13), at("LOG", 2, 14)},
		{"released on a generation the row has left: a sequence there names nothing in the row's",
			at("LOG", 3, 1), at("LOG", 2, 13), at("LOG", 3, 1)},
		{"released on a later generation than the row: never carried across one",
			at("LOG", 2, 10), at("LOG", 3, 2), at("LOG", 2, 10)},
		{"released on another stream: no position of it orders against the row",
			at("LOG", 2, 10), at("OTHER", 2, 13), at("LOG", 2, 10)},
		{"no row yet, barriers reached from the log's start: carried from nothing",
			at("LOG", 0, 0), at("LOG", 0, 3), at("LOG", 0, 3)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := statelog.CurrentThrough(c.row, c.released); got != c.want {
				t.Errorf("the row at %s released at %s is current through %s, want %s",
					c.row, c.released, got, c.want)
			}
		})
	}
}

// A STAMPED READ IS NEVER BELOW ONE ANOTHER NODE ANSWERED BEFORE IT.
//
// Node A holds its commit of a barrier node B appended, so A's checkpoint row
// stays at the write while A decodes, and reaches, every barrier after it. B
// answers a stamped read first; A's, asked after B's had answered, appends a
// barrier past B's and is released through it — and is stamped at or past
// B's, because [statelog.Reader.ReadThrough] hands A's transaction its row
// carried to that release. Handed the row alone it was stamped at the write,
// below B's stamp on the very same rows: Gitlet's cache, synced
// linearizable on such a node, read as behind the rows another member had
// just answered at, and took a linearizable answer below its mark for its
// log gone back.
//
// Released, A commits every record on the log, and a read past nothing it has
// not committed is handed its row exactly, with the instant of the record the
// row names.
func TestAStampedReadIsNeverBelowOneAnotherNodeAnsweredBeforeIt(t *testing.T) {
	t.Parallel()
	q := soloBroker(t, js.Config{SyncAlways: true})
	hold := storetest.HoldCommits(t)
	a := running(newWiredNodeWith(t, q, "node-a", store.Options{WrapDriver: hold.Wrap}))
	b := running(newWiredNode(t, q, "node-b"))
	ctx, cancel := context.WithTimeout(t.Context(), heldBudget)
	defer cancel()
	subject := probeSubject("stamped")
	written, err := a.write(ctx, subject, statelog.Position{})
	if err != nil {
		t.Fatalf("a write before the hold: %v", err)
	}
	stamped := func(n *wiredNode, name string, level statelog.ReadLevel) (statelog.Answer, statelog.Checkpoint) {
		t.Helper()
		var (
			handed statelog.Checkpoint
			rows   int
		)
		answer, err := n.reader(t, n.runner).ReadThrough(ctx, statelog.Query{
			Level: level, Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
		}, func(tx *sql.Tx, cp statelog.Checkpoint) error {
			handed = cp
			var err error
			rows, err = objects(ctx, tx)
			return err
		})
		if err != nil {
			t.Fatalf("node %s's %s read: %v", name, level, err)
		}
		if rows != 1 {
			t.Errorf("node %s's %s read saw %d writes, want the one", name, level, rows)
		}
		if answer.Position.Packed() > handed.At.Packed() {
			t.Errorf("node %s's %s read was released at %s and handed its transaction %s: a stamp below "+
				"the position the read was released at", name, level, answer.Position, handed.At)
		}
		return answer, handed
	}

	hold.Arm()
	defer hold.Release()
	first, err := b.barrier(ctx)
	if err != nil {
		t.Fatalf("node B's barrier: %v", err)
	}
	if err := hold.AwaitHeld(ctx, 1); err != nil {
		t.Fatalf("node A's checkpoint of B's barrier never reached its commit: %v", err)
	}
	_, onB := stamped(b, "B", statelog.ReadLinearizable)
	if onB.At.Packed() < first.Packed() {
		t.Fatalf("node B's read, whose barrier follows B's own at %s, was stamped %s", first, onB.At)
	}
	_, onA := stamped(a, "A", statelog.ReadLinearizable)
	if got := a.runner.Committed(); got != written.Position {
		t.Fatalf("node A's checkpoint is at %s, want it held at the write at %s: the case held no "+
			"commit of a barrier", got, written.Position)
	}
	if onA.At.Packed() < onB.At.Packed() {
		t.Errorf("node A's read, asked after B's answered at %s, was stamped %s on the same rows: its "+
			"checkpoint row, held at the write, and not the barriers it was released past", onB.At, onA.At)
	}
	if !onA.StoredAt.IsZero() {
		t.Errorf("node A's stamp %s, past its checkpoint row at %s, carries the instant %s of a record no "+
			"row of A's names", onA.At, written.Position, onA.StoredAt)
	}
	if onA.KeyedTo.IsZero() || !onA.KeyedTo.Equal(onB.KeyedTo) {
		t.Errorf("node A's stamp is keyed to the stream made at %s, node B's to %s: one log", onA.KeyedTo,
			onB.KeyedTo)
	}

	// THE LOG'S LAST RECORD, which nothing appends past: A's own read's
	// barrier. That is what A has to commit before its row stops moving, and
	// never its reached position, which is how far A has decoded rather than
	// where the log ends. A's read was released on the record before its
	// barrier, so the barrier can still be on its way to A's loop when A has
	// committed everything it reached, and a fetch that fails puts the
	// reached position back on the checkpoint. Either way the two met below
	// the end, A committed the rest between the row read below and the read
	// compared with it, and that read was handed the later row.
	_, last, err := a.log.Bounds(ctx)
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	end := written.Position.At(last)
	hold.Release()
	until(t, "node A committing every record on the log, once released", func() bool {
		return a.runner.Committed() == end
	})
	row, _, err := statelog.CheckpointOf(ctx, a.estate, a.spec.Name)
	if err != nil {
		t.Fatalf("read node A's checkpoint row: %v", err)
	}
	_, settled := stamped(a, "A", statelog.ReadStale)
	if settled.At != row.At || !settled.KeyedTo.Equal(row.KeyedTo) || !settled.StoredAt.Equal(row.StoredAt) {
		t.Errorf("node A, past nothing it has not committed, handed its read %+v, want its checkpoint row "+
			"%+v exactly", settled, row)
	}
}
