package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executordomain/executordomaintest"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/textcut"
)

// appliedLog is the executor log as one data node applies and reads it — the
// shipped applier and the shipped reader (executordomaintest) — answering this
// package's records as the engine's adapter answers them (internal/engine's
// executorRecords): a capture the turn's row names once it is applied has
// LANDED, one the apply wrote nothing for was PASSED OVER, and an end is taken;
// and reading a turn's row as the engine's restore reads it (its
// executorRestore), the log's not-found as [ErrTurnRowNotFound]. Every end
// asked of it is kept, for a case to read the end its coordinator made.
type appliedLog struct {
	log *executordomaintest.Log

	mu   sync.Mutex
	ends []EndRecord
}

var (
	_ SessionWrites  = (*appliedLog)(nil)
	_ SessionRestore = (*appliedLog)(nil)
)

// Begin refuses every begin: no case over this log launches a turn that read
// its conversation, and the begin's own rules are internal/executordomain's
// suite's to hold.
func (a *appliedLog) Begin(context.Context, BeginRecord) (BeginAnswer, error) {
	return BeginAnswer{Log: LogRefused}, errors.New("this log records no begin: no case over it launches " +
		"a turn that read its conversation")
}

// Capture applies c as the log carries a capture: the payload the engine's
// writer builds from it, its payload version the one every capture of this
// build carries.
func (a *appliedLog) Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error) {
	s := c.Stamp
	recorded, err := a.log.Capture(ctx, executordomain.Capture{
		V: 1, Handle: c.Handle, Turn: c.Turn, ConversationKey: c.ConversationKey, WorkKey: c.WorkKey,
		Harness: s.Harness, HarnessVersion: s.HarnessVersion, SessionID: s.SessionID,
		Object: c.Object.Key.String(), ObjectHash: string(c.Object.Hash), ObjectSize: c.Object.Size,
		ContentKey: s.ContentKey, ArchiveFormat: s.ArchiveFormat, LaunchSeq: s.LaunchSeq,
		CaptureSeq: s.CaptureSeq, Home: s.Home, Workspace: s.Workspace, SeatBudget: s.SeatBudget,
		ContextTokens: s.ContextTokens, Interrupted: s.Interrupted,
		ReadThrough: c.ReadThrough, Absorbs: c.Absorbs,
	})
	switch {
	case err != nil:
		return LogUnsettled, err
	case !recorded:
		return LogPassedOver, nil
	}
	return LogLanded, nil
}

// End applies e as the log carries an end: its ask cut and its ending fitted to
// the log's bounds, as the engine's writer cuts and fits them.
func (a *appliedLog) End(ctx context.Context, e EndRecord) (LogAnswer, error) {
	a.mu.Lock()
	a.ends = append(a.ends, e)
	a.mu.Unlock()
	if err := a.log.End(ctx, executordomain.End{V: 1, Handle: e.Handle, Turn: e.Turn,
		ConversationKey: e.ConversationKey, WorkKey: e.WorkKey,
		AskExcerpt: textcut.Within(e.Ask, executordomain.AskExcerptMaxBytes), Ending: e.Ending.Fit()}); err != nil {
		return LogUnsettled, err
	}
	return LogLanded, nil
}

// Session reads a turn's row as the engine's restore reads it.
func (a *appliedLog) Session(ctx context.Context, handle, turn string) (executorkeep.TurnRow, error) {
	read, err := a.log.Session(ctx, handle, turn)
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		return executorkeep.TurnRow{}, fmt.Errorf("%w: %w", ErrTurnRowNotFound, err)
	case err != nil:
		return executorkeep.TurnRow{}, err
	}
	return read.Session.Kept(), nil
}

// errNoArchive is this log's answer to a fetch: it keeps rows, never the
// objects they name, and no case over it continues a session.
var errNoArchive = errors.New("this log keeps no session's archive: no case over it continues one")

func (a *appliedLog) Fetch(context.Context, executorkeep.TurnRow, io.Writer) error {
	return errNoArchive
}

func (a *appliedLog) OpenSpooled(executorkeep.TurnRow, io.Reader) (io.Reader, error) {
	return nil, errNoArchive
}

func (a *appliedLog) Open(context.Context, executorkeep.TurnRow) (io.ReadCloser, error) {
	return nil, errNoArchive
}

// ended is every end asked of the log, in order.
func (a *appliedLog) ended() []EndRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ends)
}

// recordedLate is the run store whose first settle of a begun capture lets land
// go next: the record that capture's taker handed on before its lease's window
// closed — to a data node, or to its broker connection's buffer — which the log
// takes only once the settle has read the turn's row there and stamped the
// capture refused, the one order the fence cannot rule out (session.go, "WHAT
// THE FENCE CANNOT REACH").
type recordedLate struct {
	PendingStore
	land func(context.Context) (LogAnswer, error)

	mu      sync.Mutex
	settled *SessionCapture
	landed  LogAnswer
	err     error
}

func (r *recordedLate) SettleBegun(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	took, err := r.PendingStore.SettleBegun(ctx, turnID, capture)
	if err != nil || !took {
		return took, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled == nil {
		stamped := capture
		r.settled = &stamped
		// ON A CONTEXT OF ITS OWN: the stamp's is cut at a record's budget,
		// and the record lands whatever the stamp's writer has left.
		r.landed, r.err = r.land(context.WithoutCancel(ctx))
	}
	return took, nil
}

// late is what the settle stamped, and what the log answered the record that
// landed after it.
func (r *recordedLate) late() (*SessionCapture, LogAnswer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settled, r.landed, r.err
}

// A CAPTURE RECORDED AFTER ITS SETTLE IS KEPT THROUGH THE TURN'S END. A record
// its taker handed on before its lease's window closed can be taken by the log
// only after another node's settle read the turn's row there, found no such
// capture, and stamped it refused — the one order the fence cannot rule out —
// and NO REFUSAL OF THE SETTLE'S GOES TO THE LOG, since one ordered there would
// have the applier refuse that late capture and lose a session. So the log
// takes the late record as any other, and the turn's row names it; the turn's
// end names the run's newest capture as unkept — the settled stamp, at the
// place the row now names, no newer than it — and the applier, deciding on the
// row's own place, releases nothing; and the next read of the turn's row, the
// one a session read and the conversation's next turn make, has it kept: its
// object named, its conversation's head, and no account of it as not kept.
// DRIVEN THROUGH THE SHIPPED APPLIER AND READER (executordomaintest), never a
// double's reading of the end's release.
//
// Mutation: decide the end's release at or past the row's own place
// (executordomain's unkeptSupersedes as at-or-after), and the end takes the
// late capture off the row; name the end's unkept capture past the run's
// newest stamp (unkeptOf), and it releases the capture the row names; answer
// executordomain's Session.Unkept off the ending alone, and the kept row tells
// its own session as not kept; or record the settle's refusal on the log, as
// ADR-0073 declines to, and the late record is refused.
func TestACaptureRecordedAfterItsSettleIsKeptThroughTheTurnsEnd(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ctx := t.Context()
		store := newTakerStore()
		rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
		applied := &appliedLog{log: executordomaintest.Open(t)}
		rig.coordinator.sessions, rig.coordinator.restore = applied, applied
		// THE SHIPPED RECORD BUDGET: every record here is applied to a real store,
		// which a loaded runner can hold past the rig's fifth of a second.
		rig.coordinator.recordBudget = sessionRecordBudget
		turn := uuid.NewString()
		run := rig.launchKept(t, turn)

		// node-a's PROCESS TOOK THE JOB'S FINAL CAPTURE under its taker lease,
		// uploaded it and sent its record, and stopped before the log took it.
		lease := claimTaker(t, store, takingProcess)
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
			TakerEpoch: lease.Epoch, Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1",
			ArchiveFormat: FakeArchiveFormat, Through: new(0), SeatBudget: 64 << 20}
		if took, err := rig.pending.StampCapture(ctx, turn, begun); err != nil || !took {
			t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
		}
		body := []byte("node-a's session of the job")
		object := objstore.Object{Key: objstore.KeyAt(begun.At), Hash: objstore.HashOf(body), Size: int64(len(body))}
		handedOn := begun
		handedOn.Object, handedOn.ObjectHash, handedOn.ObjectSize = object.Key.String(), string(object.Hash), object.Size
		handedOn.ContentKey = "k1"
		late := &recordedLate{PendingStore: rig.coordinator.pending, land: func(ctx context.Context) (LogAnswer, error) {
			return applied.Capture(ctx, CaptureRecord{Handle: run.AgentHandle, Turn: turn,
				ConversationKey: run.ConversationKey, WorkKey: run.WorkKey, Object: object, Stamp: handedOn})
		}}
		rig.coordinator.pending = late
		lapse(store)

		// THE LOST TURN'S END, by the ending's own retry: its wait settles node-a's
		// capture, the late record lands, and the end is recorded after both.
		rig.coordinator.finish(ctx, rig.get(turn), ending{fence: fenceOf(run),
			note: &failureNote{reason: string(LossBoxGone), detail: "the box is gone from its host"}})
		for range 3 {
			if rig.fireRetries() == 0 {
				break
			}
		}

		settled, landed, landErr := late.late()
		switch {
		case settled == nil:
			t.Fatal("the end's wait settled nothing, so no record landed after a settle")
		case settled.State != CaptureRefused || !settled.sameTake(begun) ||
			!strings.Contains(settled.Reason, "its taker node-a stopped before it was kept"):
			t.Fatalf("the settle stamped %+v, want node-a's begun take refused for its stopped taker", *settled)
		case landErr != nil || landed != LogLanded:
			t.Fatalf("the late record = %s, %v; want the log to take it after the settle", landed, landErr)
		}
		ends := applied.ended()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the lost turn's one", len(ends))
		}
		unkept := ends[0].Ending.Unkept
		if unkept == nil || unkept.LaunchSeq != begun.LaunchSeq || unkept.CaptureSeq != begun.CaptureSeq ||
			!strings.Contains(unkept.Reason, "its taker node-a stopped before it was kept") {
			t.Fatalf("the end names %+v unkept, want the settled stamp, at its own place", unkept)
		}

		read, err := applied.log.Session(ctx, run.AgentHandle, turn)
		if err != nil {
			t.Fatalf("the next read of the turn's row: %v", err)
		}
		row := read.Session
		switch {
		case !row.Ended() || !row.HasObject() || row.Object.Key.String() != object.Key.String():
			t.Fatalf("the turn's row is %+v, want it ended and naming the late capture's object", row)
		case row.LaunchSeq != unkept.LaunchSeq || row.CaptureSeq != unkept.CaptureSeq:
			t.Fatalf("the row keeps (launch %d, capture %d), want the place the end named unkept", row.LaunchSeq,
				row.CaptureSeq)
		case row.Unkept() != nil || row.NotKept() != "":
			t.Errorf("the row tells its own kept session %+v unkept and says %q", row.Unkept(), row.NotKept())
		case read.Head != turn:
			t.Errorf("the row's own read names %q its conversation's head, want the turn", read.Head)
		}
		head, err := applied.log.Head(ctx, run.AgentHandle, run.ConversationKey)
		if err != nil || head.Session == nil || head.Session.Turn != turn ||
			head.Session.Object.Key.String() != object.Key.String() {
			t.Errorf("the conversation's next turn is handed %+v (%v), want the late capture", head.Session, err)
		}
		if _, found, _ := rig.pending.Get(ctx, turn); found {
			t.Error("the lost run's record outlived its end")
		}
	})
}
