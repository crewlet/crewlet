package tracker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RETRY OF A WRITE THAT LANDED IS ANSWERED WITH THE WRITE, NOT DECIDED AGAIN
// — through the real writer, the real publisher and the real ledger.
//
// The retry takes a snapshot that already holds the first application. Decided
// again there, it is refused for reasons the write itself caused: a purge finds
// its task gone and answers "not on this node", an update conditioned on a
// version finds that version moved by its own first copy and answers "stale".
// The caller of an `unknown` was told to retry under the same operation id,
// and did — and is then told the operation failed when it succeeded.
func TestARetryOfAWriteThatLandedIsAnsweredWithIt(t *testing.T) {
	t.Parallel()

	t.Run("a purge whose acknowledgement was lost", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		filedTask(t, r, "t-1")
		lossy, lost := r.lossyWriter(t)

		// THE ACKNOWLEDGEMENT IS LOST, and so is the broker's answer to
		// the probe that would have found the record: the operator is
		// told `unknown` and handed the operation id to retry under.
		op := statelog.NewOpID(time.Now(), "purge-t-1")
		lost.drop(2)
		first, err := lossy.PurgeTask(t.Context(), op, "t-1", "ENG", "spam")
		if err != nil || first.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("the purge with its answer lost = (%+v, %v), want unknown",
				first.Result, err)
		}
		r.drain()
		end := r.logEnd(t)

		retry, err := r.writer.PurgeTask(t.Context(), op, "t-1", "ENG", "spam")
		if err != nil {
			t.Fatalf("the retry of a purge that landed: %v — the destruction the "+
				"operator asked for happened, and a retry is told otherwise", err)
		}
		if retry.Outcome != statelog.OutcomeApplied || !retry.Collapsed ||
			retry.Position.Seq != end {
			t.Fatalf("the retry = %+v, want applied at the purge's own record %d",
				retry.Result, end)
		}
		if got := r.logEnd(t); got != end {
			t.Fatalf("the retry put %d record(s) on the log — a second purge", got-end)
		}
	})

	t.Run("an update conditioned on the version its first copy moved", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		created := r.createTask("who owns the rollback")
		op := statelog.NewOpID(time.Now(), "update-"+created.ID)
		update := func() (tracker.WriteResult, error) {
			urgent := tracker.PriorityUrgent
			return r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
				created.Version, tracker.TaskPatch{Priority: &urgent},
				tracker.ChangeFields, nil)
		}
		first, err := update()
		if err != nil {
			t.Fatalf("the first application: %v", err)
		}
		r.drain()
		retry, err := update()
		if errors.Is(err, tracker.ErrStaleVersion) {
			t.Fatalf("the retry was refused as stale — against the version its "+
				"own first copy moved: %v", err)
		}
		if err != nil || retry.Position != first.Position || !retry.Collapsed {
			t.Fatalf("the retry = (%+v, %v), want the first copy's %s",
				retry.Result, err, first.Position)
		}
	})

	// A CREATE WHOSE KEY WAS ALREADY MINTED UNDER THIS OPERATION does not
	// build a task on a number the counter never recorded: that number is
	// the next create's too, and two tasks with one key is the one thing
	// the create sequence exists to prevent.
	t.Run("a create whose key was minted under this operation", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		op := statelog.NewOpID(time.Now(), "create")
		if _, err := r.writer.CreateTask(t.Context(), op, newTask("t-1"), nil); err != nil {
			t.Fatalf("the first create: %v", err)
		}
		r.drain()
		end := r.logEnd(t)

		_, err := r.writer.CreateTask(t.Context(), op, newTask("t-2"), nil)
		if !errors.Is(err, statelog.ErrUnavailable) || !strings.Contains(err.Error(), "already landed") {
			t.Fatalf("the create = %v, want the key mint's own refusal — a create "+
				"that builds a task on the counter step an earlier copy of its "+
				"operation took files it under a number the counter never "+
				"recorded, which the next create takes as well", err)
		}
		if got := r.logEnd(t); got != end {
			t.Fatalf("the refused create put %d record(s) on the log", got-end)
		}
	})
}

// lossyWriter is a second writer over this harness's own log and estate whose
// broker answers can be LOST: the append lands and nothing that reports it
// arrives.
func (r *roundTrip) lossyWriter(t *testing.T) (*tracker.Writer, *lossyLog) {
	t.Helper()
	rows, err := tracker.NewRows(r.db)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	fence := tracker.NewFence(r.db, "node-a")
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := r.log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	fence.Committed = r.waiter.Committed
	lost := &lossyLog{Appender: r.log}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: tracker.Domain{}, Log: lost, Rows: rows, Fence: fence,
		Gates: tracker.NewGates(r.db), Waiter: r.waiter, Identity: r.waiter,
		NodeID: "node-a", Generation: func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	writer, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: publisher, DB: r.db, NodeID: "node-a", Claims: memory.New(),
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	return writer, lost
}

// logEnd is the log's last sequence.
func (r *roundTrip) logEnd(t *testing.T) uint64 {
	t.Helper()
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	return end
}

// lossyLog is the broker with its next few ANSWERS lost: an append still lands.
type lossyLog struct {
	statelog.Appender
	mu      sync.Mutex
	pending int
}

// drop loses the next n answers.
func (l *lossyLog) drop(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = n
}

func (l *lossyLog) lose() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == 0 {
		return false
	}
	l.pending--
	return true
}

var errAnswerLost = errors.New("nats: timeout")

func (l *lossyLog) Append(ctx context.Context, subject, msgID string, expect *uint64,
	body []byte) (uint64, bool, error) {

	seq, dup, err := l.Appender.Append(ctx, subject, msgID, expect, body)
	if err == nil && l.lose() {
		return 0, false, errAnswerLost
	}
	return seq, dup, err
}

func (l *lossyLog) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	if l.lose() {
		return 0, false, errAnswerLost
	}
	return l.Appender.LastSeq(ctx, subject)
}
