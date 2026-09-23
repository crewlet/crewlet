package tracker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

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

	// A CREATE THAT FINISHED is answered with the task it filed, under the
	// key it took — and with no second counter record, which is a number
	// spent on every retry of work that was already done.
	t.Run("a create that finished", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		op := statelog.NewOpID(time.Now(), "create")
		first, err := r.writer.CreateTask(t.Context(), op, newTask("t-1"), nil)
		if err != nil {
			t.Fatalf("the first create: %v", err)
		}
		r.drain()
		end := r.logEnd(t)

		retry, err := r.writer.CreateTask(t.Context(), op, newTask("t-1"), nil)
		if err != nil {
			t.Fatalf("the retry of a create that landed: %v — the task exists, "+
				"and its filer is told it could not be filed", err)
		}
		if retry.Outcome != statelog.OutcomeApplied || retry.Key != first.Key ||
			retry.Position != first.Position {
			t.Fatalf("the retry = %+v key %q, want applied as %s at %s", retry.Result,
				retry.Key, first.Key, first.Position)
		}
		if got := r.logEnd(t); got != end {
			t.Fatalf("the retry put %d record(s) on the log — a create already "+
				"filed spends no second number", got-end)
		}
	})

	// A CREATE WHOSE TASK LANDED BUT HAS NOT APPLIED HERE looks, to the
	// retry, exactly like one whose task never landed — the ledger holds
	// the counter step and not the task step — so it is resumed on a fresh
	// number. What it must not do is report that number: the task step
	// then finds the earlier copy, and the task is that copy's.
	t.Run("a create whose task landed and has not applied here", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		op := statelog.NewOpID(time.Now(), "create")
		first, err := r.writer.CreateTask(t.Context(), op, newTask("t-1"), nil)
		if err != nil || first.Outcome != statelog.OutcomePending {
			t.Fatalf("the first create = (%+v, %v), want pending", first.Result, err)
		}
		// THE COUNTER STEP APPLIED, THE TASK STEP NOT: its record is the
		// one after the counter's.
		r.apply(r.consumed+1, first.Position.Seq-1)
		r.applyWhileWriting()

		retry, err := r.writer.CreateTask(t.Context(), op, newTask("t-1"), nil)
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if retry.Key != first.Key || retry.Position != first.Position ||
			!retry.Collapsed {
			t.Fatalf("the retry = %+v key %q, want the first copy's %s at %s — "+
				"the fresh number it minted is the gap, not the task's key",
				retry.Result, retry.Key, first.Key, first.Position)
		}
		r.drain()
		if rows := r.ask(map[string]any{"container": "project:ENG"}).Rows; len(rows) != 1 {
			t.Fatalf("the project holds %d tasks after one create retried once", len(rows))
		}
	})

	// A CREATE WHOSE COUNTER LANDED AND WHOSE TASK DID NOT — the crash
	// residue — is finished on a FRESH number. The one its counter step
	// took is the earlier copy's and is not in this call: built on the
	// number this call would have taken instead, the task shares its key
	// with the next create, because the counter never recorded it.
	t.Run("a create whose counter landed and whose task did not", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		lossy, lost := r.lossyWriter(t)
		op := statelog.NewOpID(time.Now(), "create")
		lost.refuse(".task.t-1")
		if _, err := lossy.CreateTask(t.Context(), op, newTask("t-1"), nil); err == nil {
			t.Fatal("the create whose task step the broker refused reported success")
		}
		lost.refuse("")
		r.drain()

		retry, err := r.writer.CreateTask(t.Context(), op, newTask("t-1"), nil)
		if err != nil {
			t.Fatalf("the retry of a create whose task never landed: %v", err)
		}
		r.drain()
		if retry.Outcome != statelog.OutcomeApplied || retry.Collapsed {
			t.Fatalf("the retry = %+v, want its own task applied", retry.Result)
		}
		if retry.Key != "ENG-2" {
			t.Fatalf("the retry filed its task as %q, want ENG-2 — ENG-1 is the "+
				"earlier copy's number, the gap a crash between the two appends "+
				"leaves", retry.Key)
		}
		next, err := r.writer.CreateTask(t.Context(),
			statelog.NewOpID(time.Now(), "create"), newTask("t-2"), nil)
		if err != nil {
			t.Fatalf("the next create: %v", err)
		}
		if next.Key == retry.Key {
			t.Fatalf("two tasks share key %s", next.Key)
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
	fence := tracker.NewFence(r.db, r.nodeID)
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
		NodeID: r.nodeID, Admission: r.reserve,
		Generation:    func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	writer, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: publisher, DB: r.db, NodeID: r.nodeID, Claims: memory.New(),
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	return writer, lost
}

// AN OPERATION ID CARRIED TO ANOTHER TASK IS REFUSED, NOT ANSWERED WITH THE
// FIRST TASK'S RECORD — through the real writer, publisher and ledger.
//
// The snapshot answers a retry from the ledger row under the operation's id
// before anything is decided. The purge's id sent with a purge of a second
// task found the first purge's row there and was answered `applied`, at the
// first task's position, with the second task never touched: the operator was
// told a destruction happened that did not.
func TestAnOperationIDCarriedToAnotherTaskIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	op := statelog.NewOpID(time.Now(), "purge-t-1")
	first, err := r.writer.PurgeTask(t.Context(), op, "t-1", "ENG", "spam")
	if err != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the purge of t-1 = (%+v, %v)", first.Result, err)
	}
	r.drain()
	end := r.logEnd(t)

	_, err = r.writer.PurgeTask(t.Context(), op, "t-2", "ENG", "spam")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonOpReused {
		t.Fatalf("t-1's purge id sent with a purge of t-2 answered %v, want an "+
			"op_reused refusal", err)
	}
	if refusal.Position != first.Position {
		t.Errorf("the refusal names %s, want t-1's purge at %s", refusal.Position,
			first.Position)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the refused purge put %d record(s) on the log", got-end)
	}
	if answer := r.ask(map[string]any{"container": "project:ENG"}); len(answer.Rows) != 1 {
		t.Fatalf("the project holds %d task(s), want t-2 untouched", len(answer.Rows))
	}
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

// lossyLog is the broker with its next few ANSWERS lost — an append still
// lands — or with the appends to one subject REFUSED outright.
type lossyLog struct {
	statelog.Appender
	mu      sync.Mutex
	pending int
	refused string

	// landed runs after an append to a subject ending in its suffix
	// LANDS, on the appending goroutine — which is where a case puts
	// somebody else's write that has to fall between two steps of one
	// sequence.
	landedOn string
	landed   func()

	// dropOn and dropStage lose ONE append's acknowledgement on one
	// subject and then the probe that resolves it — so a walk's single
	// step, and nothing either side of it, has an unknown outcome. A read
	// of the subject BEFORE the append (a create's own expectation) is
	// answered as usual.
	dropOn    string
	dropStage int
	// dropAckOnly ends a targeted drop at the acknowledgement: the probe
	// that follows is answered, so the publisher resolves the lost answer
	// from what the log holds rather than calling it unknown. dropAcks is
	// how many appends in a row lose theirs.
	dropAckOnly bool
	dropAcks    int
}

// The stages of a targeted drop.
const (
	dropIdle = iota
	dropNextAppend
	dropNextProbe
)

// dropFor loses the acknowledgement of the next append that LANDS on a
// subject ending in suffix, and the probe the publisher then resolves the
// lost acknowledgement with.
func (l *lossyLog) dropFor(suffix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropOn, l.dropStage, l.dropAckOnly = suffix, dropNextAppend, false
}

// dropAck loses the answer to the next append on a subject ending in suffix —
// whatever that answer was, a refusal included — and nothing after it, so the
// publisher probes the log and resolves the write from what it finds.
func (l *lossyLog) dropAck(suffix string) { l.dropAcksFor(suffix, 1) }

// dropAcksFor is [lossyLog.dropAck] for the next n appends on the subject.
func (l *lossyLog) dropAcksFor(suffix string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropOn, l.dropStage, l.dropAckOnly, l.dropAcks = suffix, dropNextAppend, true, n
}

// loseFor reports whether this answer about subject is the one a targeted drop
// loses: an append's acknowledgement at the first stage, a probe at the second.
func (l *lossyLog) loseFor(subject string, probe bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !strings.HasSuffix(subject, l.dropOn) || l.dropOn == "" {
		return false
	}
	switch {
	case !probe && l.dropStage == dropNextAppend && l.dropAckOnly:
		if l.dropAcks--; l.dropAcks <= 0 {
			l.dropStage, l.dropOn = dropIdle, ""
		}
		return true
	case !probe && l.dropStage == dropNextAppend:
		l.dropStage = dropNextProbe
		return true
	case probe && l.dropStage == dropNextProbe:
		l.dropStage, l.dropOn = dropIdle, ""
		return true
	}
	return false
}

// afterAppendTo runs fn once, after the next append to a subject ending in
// suffix lands.
func (l *lossyLog) afterAppendTo(suffix string, fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.landedOn, l.landed = suffix, fn
}

// takeLanded is the hook for subject, cleared as it is taken.
func (l *lossyLog) takeLanded(subject string) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.landed == nil || !strings.HasSuffix(subject, l.landedOn) {
		return nil
	}
	fn := l.landed
	l.landed = nil
	return fn
}

// refuse makes the broker refuse to store every append to a subject ending in
// suffix, as a full stream does; the empty suffix refuses nothing.
func (l *lossyLog) refuse(suffix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refused = suffix
}

func (l *lossyLog) refuses(subject string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refused != "" && strings.HasSuffix(subject, l.refused)
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

	if l.refuses(subject) {
		return 0, false, &jetstream.APIError{
			Code: 503, ErrorCode: 10077, Description: "maximum bytes exceeded",
		}
	}
	seq, dup, err := l.Appender.Append(ctx, subject, msgID, expect, body)
	if err == nil {
		if fn := l.takeLanded(subject); fn != nil {
			fn()
		}
	}
	// A TARGETED DROP LOSES THE ANSWER WHATEVER IT WAS, a refusal's
	// included: the broker decided, and the client never heard which way.
	if l.loseFor(subject, false) {
		return 0, false, errAnswerLost
	}
	if err == nil && l.lose() {
		return 0, false, errAnswerLost
	}
	return seq, dup, err
}

func (l *lossyLog) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	if l.lose() || l.loseFor(subject, true) {
		return 0, false, errAnswerLost
	}
	return l.Appender.LastSeq(ctx, subject)
}
