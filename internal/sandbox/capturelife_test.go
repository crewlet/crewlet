package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A CAPTURE'S BUDGETS COUNT EVERY STEP OF ITS LIFE AT THE SLOWEST CROSSING, and
// read it on another machine's clock within twice the skew the engine allows
// one. Each figure is its terms: a change of a run's row another node reads at
// the slowest crossing a link is allowed; a keep's two reads beside its upload
// and its record; a take's life from its instant (the rest of its take with its
// reservation's room, its digest, its keep and its stamp) and a takeover's
// (the takeover, the standby's withdrawal and its read of the row back where
// the store did not answer it, its keep and its stamp); a standby's wait (the
// longer life and one read), its window from its own instant (the rest of its
// take and its digest, the wait, and three changes past it: a settle the wait's
// last pass may still be writing, its last read and its takeover); the spool's
// longest wait for its keep; and each reading across machines, the one-clock
// figure and two hours. And the figures the docs state at today's bounds are
// these, to the nanosecond.
//
// Mutation: drop any one term of any figure, or the skew from either reading,
// and its row goes red.
func TestACapturesBudgetsCountEveryStepAtTheSlowestCrossing(t *testing.T) {
	t.Parallel()
	change := sessionRecordBudget + coord.MaxValueCrossing
	keep := 2*sessionRecordBudget + objstore.PaceFor(executorkeep.SealedMaxBytes) + sessionRecordBudget
	take := SessionPackWait + SessionScrubWait + change + discardGrace + keep + change
	over := change + change + change + keep + change
	life := max(take, over)
	wait := life + captureWaitTick
	window := SessionPackWait + SessionScrubWait + change + discardGrace + wait + change + change + change
	opened := discardGrace + wait + 5*change + 2*sessionRecordBudget
	skew := 2 * objstore.AssumedClockSkew
	for _, c := range []struct {
		name      string
		got, want time.Duration
		floor     string
	}{
		{"one change of a run's row another node reads", captureStampBudget, change, "13m30.025663374s"},
		{"a keep", SessionKeepBudget, keep, "18m0s"},
		{"a keep's read", keepReadRoom, sessionRecordBudget, "30s"},
		{"a take's life", takeLife, take, "49m30.051326748s"},
		{"a takeover's life", takeOverLife, over, "1h12m0.102653496s"},
		{"a capture's life", captureLife, life, "1h12m0.102653496s"},
		{"a capture read on another machine's clock", SessionCaptureBudget, life + skew, "3h12m0.102653496s"},
		{"a standby's wait", standbyWait, wait, "1h12m2.102653496s"},
		{"a standby's window", standbyWindow, window, "2h10m32.205306992s"},
		{"a standby read on another machine's clock", standbyBudget, window + skew, "4h10m32.205306992s"},
		{"a spool's longest wait for its keep", spoolOpenedWithin, opened, "2h21m2.230970366s"},
	} {
		if c.got != c.want {
			t.Errorf("%s is %v, want its terms, %v", c.name, c.got, c.want)
		}
		if c.got.String() != c.floor {
			t.Errorf("%s is %v at today's bounds, and the docs state %s: restate them", c.name, c.got, c.floor)
		}
	}
}

// A CAPTURE'S AGE IS READ AS ANY TWO NODES' CLOCKS ALLOW. A stamp's instant is
// its taker's clock's, and a node reading its age reads it on its own, which the
// engine holds within two hours of any other's. So a begun capture is read in
// flight, by a reader whose clock stands two hours ahead of its taker's, until
// its whole life has passed in real time and no longer; a read standing by,
// until its window has; and with the clocks in step, until the life and the two
// hours, and the window and the two hours.
//
// Mutation: read a begun capture against its life alone, or a standby against
// its window alone, and the reader ahead releases one still being kept.
func TestACapturesAgeIsReadAsAnyTwoNodesClocksAllow(t *testing.T) {
	t.Parallel()
	stamped := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	ahead := 2 * objstore.AssumedClockSkew
	begun := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: "launch-1", State: CaptureBegun, At: stamped,
		Taker: "node-a:1", TakerEpoch: 1}
	standing := begun
	standing.State, standing.Reason = CaptureRefused, "the session could not be uploaded"
	standing.StandbyAt, standing.StandbyTaker, standing.StandbyTakerEpoch = stamped, "node-b:1", 1
	for _, c := range []struct {
		name     string
		capture  SessionCapture
		age      time.Duration
		ahead    time.Duration
		inFlight bool
	}{
		{"a capture within its life, read two hours ahead", begun, captureLife - time.Nanosecond, ahead, true},
		{"a capture past its life, read two hours ahead", begun, captureLife + time.Nanosecond, ahead, false},
		{"a standby within its window, read two hours ahead", standing, standbyWindow - time.Nanosecond, ahead, true},
		{"a standby past its window, read two hours ahead", standing, standbyWindow + time.Nanosecond, ahead, false},
		{"a capture in step, within its reading", begun, SessionCaptureBudget - time.Nanosecond, 0, true},
		{"a capture in step, at its reading", begun, SessionCaptureBudget, 0, false},
		{"a standby in step, within its reading", standing, standbyBudget - time.Nanosecond, 0, true},
		{"a standby in step, at its reading", standing, standbyBudget, 0, false},
	} {
		capture := c.capture
		if got := captureInFlight(PendingRun{Capture: &capture}, stamped.Add(c.age+c.ahead)); got != c.inFlight {
			t.Errorf("%s: %v old in real time, read %v ahead, is in flight %t, want %t", c.name, c.age, c.ahead,
				got, c.inFlight)
		}
	}
}

// AN UNANSWERED RECORD IS ASKED AGAIN WHATEVER ITS KEY'S AGE READS HERE. A
// capture's key is minted on the clock of the node that spooled it, and the
// executor log refuses a record naming it past objstore.RecordWithin by its own
// deciding clock, which the engine holds only within two hours of the asking
// node's. So the turn's end asks the record again whatever its key's age reads
// on the end's clock, and the log decides: the end's clock two hours ahead of
// the log's, a key eleven hours old to the log is asked and lands, and one past
// the window to the log is asked and refused, the end naming it unkept for the
// log's reason. And a reader holds a capture whose record awaits its answer
// until the window and two hours have passed on its own clock, and no longer.
//
// Mutation: refuse the record unasked once its key reads past the window on the
// asker's clock, and the end names unkept a capture the log would have taken;
// read the reader's window without the skew, and a capture the log may still
// take is read as answered.
func TestAnUnansweredRecordIsAskedWhateverItsKeysAgeReadsHere(t *testing.T) {
	t.Parallel()
	ahead := 2 * objstore.AssumedClockSkew
	for _, c := range []struct {
		name string
		// logAge is how long before the log's clock the key was minted.
		logAge time.Duration
		landed bool
	}{
		{"a key the log reads within its window", 11 * time.Hour, true},
		{"a key the log reads past its window", objstore.RecordWithin + time.Minute, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.launchKept(t, "t1")
				minted := rig.now
				session := rig.runner.Session
				writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
				rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
				collectHeld(t, rig, "t1")
				if run := rig.get("t1"); run.Capture == nil || run.Capture.State != CaptureUnresolved {
					t.Fatalf("the row holds %+v, want the capture's record unanswered", run.Capture)
				}

				writes.answer(nil)
				logNow := minted.Add(c.logAge)
				rig.coordinator.sessions = windowedLog{sessionWrites: writes, now: func() time.Time { return logNow }}
				rig.now = logNow.Add(ahead)
				asked := len(writes.captured())
				ctx := t.Context()
				if _, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{
					Outcome: executorkeep.OutcomeCompleted}); err != nil {
					t.Fatalf("OweEnd: %v", err)
				}
				if _, err := rig.coordinator.publishEnd(ctx, rig.get("t1")); err != nil {
					t.Fatalf("publishEnd: %v", err)
				}
				if again := len(writes.captured()) - asked; again != 1 {
					t.Fatalf("the end asked the record %d times, want it asked once more, whatever its key's "+
						"age reads %s ahead of the log", again, ahead)
				}
				ends := writes.endedRecords()
				if len(ends) != 1 {
					t.Fatalf("recorded %d ends, want the turn's one", len(ends))
				}
				unkept := ends[0].Ending.Unkept
				switch run := rig.get("t1"); {
				case c.landed && (unkept != nil || run.Kept == nil || run.Kept.State != CaptureLanded):
					t.Fatalf("the end names %+v unkept and the row keeps %+v, want the capture the log took "+
						"kept and nothing unkept", unkept, run.Kept)
				case !c.landed && (unkept == nil || !strings.Contains(unkept.Reason, staleUpload)):
					t.Fatalf("the end names %+v unkept, want the capture the log refused, for its reason", unkept)
				}
			})
		})
	}

	readAt := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	window := objstore.RecordWithin + ahead
	for _, c := range []struct {
		name   string
		age    time.Duration
		awaits bool
	}{
		{"a record within the window and two hours", window - time.Minute, true},
		{"a record past the window and two hours", window + time.Minute, false},
	} {
		run := PendingRun{LaunchSeq: 1, Capture: &SessionCapture{LaunchSeq: 1, CaptureSeq: 1,
			State: CaptureUnresolved, Object: objstore.KeyAt(readAt.Add(-c.age)).String()}}
		if got := captureAwaitsAnswer(run, readAt); got != c.awaits {
			t.Errorf("%s: a capture whose key reads %v old awaits its record's answer %t, want %t", c.name, c.age,
				got, c.awaits)
		}
	}
}

// staleUpload is what [windowedLog] says of a record whose object it reads past
// objstore.RecordWithin.
const staleUpload = "the session's object is too old to name"

// windowedLog is the executor log's capture as the log decides it on its own
// clock, now: a record whose object's key was minted more than
// objstore.RecordWithin before it is refused (internal/executordomain's
// ErrUploadStale); any other is [sessionWrites]'s to answer.
type windowedLog struct {
	*sessionWrites
	now func() time.Time
}

func (w windowedLog) Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error) {
	answer, err := w.sessionWrites.Capture(ctx, c)
	//fixedwait:stated the case states the log's clock, and the rig's stepped clock minted the key
	if answer == LogLanded && w.now().Sub(c.Object.Key.Minted()) > objstore.RecordWithin {
		return LogRefused, fmt.Errorf("%s: object %s was minted more than %s ago", staleUpload, c.Object.Key,
			objstore.RecordWithin)
	}
	return answer, err
}

// pacedKeep is a keep whose upload notes the instant it began and its
// context's deadline, on the case's clock, and is otherwise [sessionKeep].
type pacedKeep struct {
	*sessionKeep

	mu     sync.Mutex
	upload handed
}

func (k *pacedKeep) Upload(ctx context.Context, spool executorkeep.SealedSpool,
	sealed io.Reader) (executorkeep.SealedObject, error) {

	deadline, set := ctx.Deadline()
	k.mu.Lock()
	k.upload = handed{at: time.Now(), deadline: deadline, set: set}
	k.mu.Unlock()
	return k.sessionKeep.Upload(ctx, spool, sealed)
}

func (k *pacedKeep) uploaded() handed {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.upload
}

// unansweredListing is a run store that never answers a listing of a seat's
// runs: the listing waits until its context ends, and the store notes when it
// began and when it was let go. Every other call is the store underneath's.
type unansweredListing struct {
	PendingStore

	mu         sync.Mutex
	began, cut time.Time
}

func (s *unansweredListing) ListActiveForSeat(ctx context.Context, _ string) ([]PendingRun, error) {
	s.mu.Lock()
	s.began = time.Now()
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	s.cut = time.Now()
	s.mu.Unlock()
	return nil, ctx.Err()
}

func (s *unansweredListing) listed() (began, cut time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.began, s.cut
}

// A KEEP'S READS LEAVE ITS UPLOAD ITS WHOLE PACE. A keep reads the coordination
// store twice before it uploads (the turn's operations minted, the seat's runs
// listed for its budget), and each is held to a room of its own beside the
// upload's pace in the keep's budget, so a store that answers them slowly, or
// never, costs the upload nothing. A listing that never answers is cut at its
// room and decides nothing: the session is uploaded, handed at least its pace
// at the slowest a store may take it and its record's budget, and lands. A
// mint that never answers is cut at its room and refuses the capture there, as
// one that fails does, rather than at the keep's end. Exact, in a bubble.
//
// Mutation: hold the reads to the keep's budget alone, and the listing runs to
// the keep's end with nothing left to upload in, and the mint's refusal comes
// at the keep's end; or refuse a capture whose listing ran past its room, and
// one the log would take is not uploaded.
func TestAKeepsReadsLeaveItsUploadItsWholePace(t *testing.T) {
	t.Parallel()
	take := func(t *testing.T, rig *coordRig) *spooledCapture {
		t.Helper()
		run := rig.launchKept(t, "t1")
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID), rig.runner,
			readCapture(BoxReadStop, Result{}, true))
		if spooled == nil || spooled.standby {
			t.Fatalf("the stop's read spooled %+v, want the capture's own place", spooled)
		}
		return spooled
	}
	t.Run("the listing never answers", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, base := keepingRig(t)
			spooled := take(t, rig)
			store := &unansweredListing{PendingStore: rig.coordinator.pending}
			keep := &pacedKeep{sessionKeep: base}
			rig.coordinator.pending, rig.coordinator.keep = store, keep
			kept := rig.coordinator.keepCapture(t.Context(), spooled)
			if kept == nil || kept.NotKept != "" {
				t.Fatalf("the keep came to %+v beside a listing that never answered, want the session kept", kept)
			}
			if began, cut := store.listed(); began.IsZero() || cut.Sub(began) != keepReadRoom {
				t.Errorf("the listing was let go %v after it began, want it cut at its room, %v",
					cut.Sub(began), keepReadRoom)
			}
			pace := objstore.PaceFor(executorkeep.SealedMaxBytes) + sessionRecordBudget
			if up := keep.uploaded(); !up.set || up.deadline.Sub(up.at) < pace {
				t.Errorf("the upload was handed %v (set %t), want at least the pace of a session at its bound "+
					"and its record's budget, %v", up.deadline.Sub(up.at), up.set, pace)
			}
			if captured := writes.captured(); len(captured) != 1 || captured[0].err != nil {
				t.Errorf("recorded %d captures, want the one", len(captured))
			}
		})
	})
	t.Run("the mint never answers", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep := keepingRig(t)
			spooled := take(t, rig)
			keep.mintGate = make(chan struct{})
			began := time.Now()
			kept := rig.coordinator.keepCapture(t.Context(), spooled)
			if kept == nil || !strings.Contains(kept.NotKept, "could not be minted") {
				t.Fatalf("the keep came to %+v beside a mint that never answered, want the capture refused", kept)
			}
			if took := time.Since(began); took != keepReadRoom {
				t.Errorf("the keep refused the capture %v after it began, want at the mint's room, %v", took,
					keepReadRoom)
			}
			if uploads := keep.uploads(); len(uploads) != 0 {
				t.Errorf("uploaded %d sessions, want none without the turn's operations", len(uploads))
			}
		})
	})
}

// A SPOOL OUTLIVES THE LONGEST ITS KEEP WAITS TO OPEN IT. A read standing by
// spools the job's session and its keep opens the spool only once the capture
// it stood by for has come to something and it has taken the place after it:
// at the longest, past its whole wait and every change of the row around its
// takeover. A take on the same node sweeps the spools a crash left behind, by
// their age on the machine's wall clock, which NTP may step forward by up to
// the skew the engine assumes. So a standby's spool aged by that longest wait,
// counted here step by step (its digest, its wait for the capture it stood by
// for, five changes of the row at the slowest crossing and its keep's two
// reads), and a step just short of the skew is still there when its keep opens
// it, and the keep keeps it; a spool past both by a minute is swept.
//
// Mutation: sweep at the longest wait alone, at twice a capture's life, or at a
// longest wait one change of the row short, and the standby's spool is swept
// before its keep opens it, which then keeps nothing.
func TestASpoolOutlivesTheLongestItsKeepWaitsToOpenIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess, TakerEpoch: 1,
			Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
		}
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standby {
			t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
		}
		age := func(file string, by time.Duration) {
			at := time.Now().Add(-by)
			if err := os.Chtimes(file, at, at); err != nil {
				t.Fatal(err)
			}
		}
		change := sessionRecordBudget + coord.MaxValueCrossing
		longest := discardGrace + captureLife + captureWaitTick + 5*change + 2*sessionRecordBudget
		age(standby.file, longest+objstore.AssumedClockSkew-time.Minute)
		crashed := filepath.Join(rig.coordinator.spoolDirectory(), spoolPrefix+"crashed")
		if err := os.WriteFile(crashed, []byte("sealed"), 0o600); err != nil {
			t.Fatal(err)
		}
		age(crashed, longest+objstore.AssumedClockSkew+time.Minute)
		rig.coordinator.sweepSpools()
		if _, err := os.Stat(standby.file); err != nil {
			t.Errorf("the standby's spool, aged by its keep's longest wait, was swept: %v", err)
		}
		if _, err := os.Stat(crashed); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a spool past the longest wait and the skew was left (%v)", err)
		}
		// THE CAPTURE IT STOOD BY FOR read as past what any clock allows, so
		// the keep takes the place after it at once and opens its spool.
		rig.now = rig.now.Add(SessionCaptureBudget)
		if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
			t.Fatalf("the standby's keep came to %+v, want its session kept", kept)
		}
		keptByTheStandby(t, rig, writes, keep)
	})
}

// A TURN'S END WAITS FOR EACH CAPTURE IT SEES ITS WHOLE LIFE FROM FIRST SIGHT,
// on its own clock, and for each read it sees standing by its window.
//
// A standby that stands after the end began: a read admitted on another
// node's host keeps reading its box after a stop's ending is decided, and its
// take, finding the stop's capture begun, stands by for it. That capture,
// begun under a lease that holds, never stamps what came of it. The end waits
// on: the standby's own wait outlasts the capture's whole life, it takes the
// place after it and keeps its session there, and only then does the end's
// wait return, the standby's capture landed and its record taken ahead of the
// end's tombstone.
//
// A standby's takeover that lands in its window's last instants, after the
// end's last round read the row: the end reads the row once more once the
// window has passed, sees the takeover, and waits for what came of it.
//
// And a capture whose stamp's clock stands hours ahead, so its age never ends
// it here, begun under a lease that holds and never stamped: the end gives up
// on it its whole life after it first saw it, and within a tick of that.
//
// Exact, in a bubble.
//
// Mutation: cap the end at one capture's life from its own start, and it
// returns before the late standby's takeover lands; decide off the last
// round's read once every bound has passed, and it returns on the row before
// the takeover; or hold each sighting to no bound of its own, and the end
// waits on the stuck capture past its life.
func TestAnEndWaitsForEachCaptureItSeesItsWholeLifeFromFirstSight(t *testing.T) {
	t.Parallel()
	begin := func(t *testing.T, rig *coordRig, run PendingRun, ahead time.Duration) SessionCapture {
		t.Helper()
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now().Add(ahead), Taker: takingProcess,
			TakerEpoch: 1, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
		}
		return begun
	}
	type waited struct {
		run   PendingRun
		found bool
		err   error
	}
	t.Run("a standby that stands after the end began", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			begin(t, rig, run, -time.Second)
			ended := make(chan waited, 1)
			row := rig.get("t1")
			go func() {
				got, found, err := rig.coordinator.awaitEndCaptures(t.Context(), row)
				ended <- waited{got, found, err}
			}()
			synctest.Wait()
			time.Sleep(time.Minute)
			// ON THE RIG'S CLOCK AS IT STANDS, which the end reads as it
			// waits: node-a's capture was stamped a second before it.
			standby, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
				takenAs{FakeRunner: rig.runner, body: bytes.NewReader(standbyArchive)},
				readCapture(BoxReadCollect, Result{}, true))
			if standby == nil || !standby.standby {
				t.Fatalf("the late read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			kept := make(chan *SessionKept, 1)
			go func() { kept <- rig.coordinator.keepCapture(t.Context(), standby) }()
			end := <-ended
			if end.err != nil || !end.found {
				t.Fatalf("the end's wait = %v, %v", end.found, end.err)
			}
			if c := end.run.Capture; c == nil || c.CaptureSeq != 2 || c.State != CaptureLanded {
				t.Errorf("the end's wait returned on %+v, want the late standby's capture landed in the next "+
					"place", c)
			}
			if captured := writes.captured(); len(captured) != 1 || captured[0].Stamp.CaptureSeq != 2 {
				t.Errorf("the log had taken %d captures as the end's wait returned, want the standby's", len(captured))
			}
			if got := <-kept; got == nil || got.NotKept != "" {
				t.Errorf("the standby's keep came to %+v, want its session kept", got)
			}
			if uploads := keep.uploads(); len(uploads) != 1 || !bytes.Equal(uploads[0], standbyArchive) {
				t.Errorf("uploaded %q, want the standby's session once", uploads)
			}
		})
	})
	t.Run("a takeover that lands after the end's last round, inside its window", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			begun := begin(t, rig, run, 0)
			// THE STANDBY STANDS AS THE END FIRST SEES IT, so its window and
			// the end's hold on it run from the same instant.
			standby := begun
			standby.At = begun.At.Add(time.Second)
			if stood, err := rig.pending.StandBy(t.Context(), "t1", standby); err != nil || !stood {
				t.Fatalf("standing by = %v, %v", stood, err)
			}
			ended := make(chan waited, 1)
			row := rig.get("t1")
			go func() {
				got, found, err := rig.coordinator.awaitEndCaptures(t.Context(), row)
				ended <- waited{got, found, err}
			}()
			// THE END'S LAST ROUND reads the row at the last tick inside the
			// standby's window; the takeover lands halfway from there to the
			// window's end.
			lastRound := standbyWindow - standbyWindow%captureWaitTick
			if lastRound == standbyWindow {
				t.Fatalf("the premise: the standby's window, %v, is a whole number of ticks, so the end's "+
					"last round reads the row at its very end", standbyWindow)
			}
			time.Sleep(lastRound + (standbyWindow-lastRound)/2)
			refused := begun
			refused.State, refused.Reason = CaptureRefused, "the session could not be uploaded"
			if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
				t.Fatalf("refusing the read holding the place = %v, %v", took, err)
			}
			over := standby
			over.CaptureSeq, over.At = 2, standby.At.Add(time.Second)
			if took, err := rig.pending.TakeOver(t.Context(), "t1", standby, over); err != nil || !took {
				t.Fatalf("the standby's takeover = %v, %v", took, err)
			}
			// ITS KEEP lands what came of it a minute later.
			time.Sleep(time.Minute)
			landed := over
			landed.State, landed.Object, landed.ObjectHash, landed.ObjectSize =
				CaptureLanded, objstore.KeyAt(rig.coordinator.now()).String(), "h-1", 1
			if took, err := rig.pending.StampCapture(t.Context(), "t1", landed); err != nil || !took {
				t.Fatalf("stamping the takeover landed = %v, %v", took, err)
			}
			end := <-ended
			if c := end.run.Capture; end.err != nil || !end.found || c == nil || c.CaptureSeq != 2 ||
				c.State != CaptureLanded {
				t.Errorf("the end's wait returned on %+v (%v), want the standby's takeover, which landed "+
					"inside its window after the end's last round, waited for and landed", c, end.err)
			}
		})
	})
	t.Run("a capture whose age never ends it here", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			begun := begin(t, rig, run, 2*objstore.AssumedClockSkew)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ended := make(chan waited, 1)
			row := rig.get("t1")
			began := time.Now()
			go func() {
				got, found, err := rig.coordinator.awaitEndCaptures(ctx, row)
				ended <- waited{got, found, err}
			}()
			select {
			case end := <-ended:
				took := time.Since(began)
				if took < captureLife || took > captureLife+captureWaitTick {
					t.Errorf("the end's wait returned %v after it first saw the capture, want its life, %v, "+
						"and within a tick of it", took, captureLife)
				}
				if c := end.run.Capture; end.err != nil || c == nil || !c.sameTake(begun) || c.State != CaptureBegun {
					t.Errorf("the end's wait returned on %+v (%v), want node-a's capture as it stood, begun", c,
						end.err)
				}
			case <-time.After(captureLife + 10*time.Minute):
				t.Errorf("the end still waits on a capture %v after it first saw it, past its life, %v",
					captureLife+10*time.Minute, captureLife)
				cancel()
				<-ended
			}
		})
	})
}

// A READ STANDING BY TAKES THE NEXT PLACE ONCE ITS OWN WAIT OUTLASTS THE
// CAPTURE. It waits for the capture it stood by for on its own clock, that
// capture's whole life and one read, never by the capture's stamped instant,
// which another machine's clock wrote. Then, the wait run out:
//
//   - the capture still begun under a lease that holds, its stamp's clock two
//     hours ahead so its age never ends it here, and never stamped: its taker
//     sends nothing more that lands, so the standby takes the place after it
//     and keeps its session there;
//   - the capture's record sent and never answered, the log still silent: it
//     may yet land, so the standby stands down and nothing is uploaded twice;
//   - the coordinator stopping before the wait runs out: the standby stands
//     down, withdrawn from the row, and takes nothing.
//
// Exact, in a bubble.
//
// Mutation: stand down at the wait's end whatever the row says, as a standby
// did, and the stuck capture's session is never kept; take the next place
// beside a record still unanswered, and the session is uploaded twice; or take
// it however the wait ended, and a stopping coordinator takes a place it
// cannot keep.
func TestAReadStandingByTakesTheNextPlaceOnceItsOwnWaitOutlastsTheCapture(t *testing.T) {
	t.Parallel()
	stuck := func(t *testing.T, rig *coordRig, run PendingRun) {
		t.Helper()
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now().Add(2 * objstore.AssumedClockSkew),
			Taker: takingProcess, TakerEpoch: 1, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
		}
	}
	t.Run("the capture stays begun", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			stuck(t, rig, run)
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby {
				t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			began := time.Now()
			if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
				t.Fatalf("the standby's keep came to %+v, want the session kept in the next place", kept)
			}
			if took := time.Since(began); took < standbyWait {
				t.Errorf("the standby took the next place %v into its keep, before its wait of %v ran out", took,
					standbyWait)
			}
			keptByTheStandby(t, rig, writes, keep)
		})
	})
	t.Run("the capture's record goes unanswered", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			held := newHeldArchive(placeHolderArchive)
			placed := holdingPlace(t, rig, run, held)
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby {
				t.Fatalf("the collection's read beside the stop's spooled %+v, want it standing by", standby)
			}
			held.end <- nil
			spooled := <-placed
			if spooled == nil {
				t.Fatal("the read holding the place spooled nothing")
			}
			writes.answer(errors.New("the log did not answer"))
			rig.coordinator.keepCapture(t.Context(), spooled)
			writes.answer(nil)
			if row := rig.get("t1"); row.Capture == nil || row.Capture.State != CaptureUnresolved {
				t.Fatalf("the place-holder's capture stands %+v, want its record unanswered", row.Capture)
			}
			writes.holdOn()
			began := time.Now()
			if kept := rig.coordinator.keepCapture(t.Context(), standby); kept != nil {
				t.Errorf("beside a record still unanswered, the standby's keep came to %+v, want nothing", kept)
			}
			if took := time.Since(began); took < standbyWait {
				t.Errorf("the standby stood down %v into its keep, before its wait of %v ran out", took,
					standbyWait)
			}
			if uploads := keep.uploads(); len(uploads) != 1 || !bytes.Equal(uploads[0], placeHolderArchive) {
				t.Errorf("uploaded %q, want the place-holder's session once", uploads)
			}
			if row := rig.get("t1"); row.Capture == nil || row.Capture.CaptureSeq != 1 ||
				row.Capture.State != CaptureUnresolved || !row.Capture.StandbyAt.IsZero() {
				t.Errorf("the row's newest capture is %+v, want the place-holder's unanswered, the standby "+
					"withdrawn", row.Capture)
			}
		})
	})
	t.Run("the coordinator stops first", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			stuck(t, rig, run)
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby {
				t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			ctx, cancel := context.WithCancel(t.Context())
			kept := make(chan *SessionKept, 1)
			go func() { kept <- rig.coordinator.keepCapture(ctx, standby) }()
			time.Sleep(time.Hour)
			cancel()
			if got := <-kept; got != nil {
				t.Errorf("the standby's keep came to %+v once its coordinator stopped, want nothing", got)
			}
			if row := rig.get("t1"); row.Capture == nil || row.Capture.CaptureSeq != 1 ||
				!row.Capture.StandbyAt.IsZero() {
				t.Errorf("the row's newest capture is %+v, want node-a's with the standby withdrawn", row.Capture)
			}
			if uploads := keep.uploads(); len(uploads) != 0 {
				t.Errorf("uploaded %d sessions, want none", len(uploads))
			}
		})
	})
}

// atTheFloor is a run store across a link at the slowest pace a link is
// allowed ([PendingStore.RowCrossing]), whose every change of a capture's
// place on a run's row, and every read of the row held to one change's room,
// spends what its room leaves but a second before the store underneath answers
// it ([atTheFloor.last], which the case sets in its bubble): each step of a
// capture's life at its bound. It notes the instant each was asked, and each
// stamp of an outcome and each takeover the store made. Where loseTakeOver is
// set, a takeover is made and its answer lost, as a store whose answer never
// reached its asker loses it.
type atTheFloor struct {
	PendingStore
	last         func(ctx context.Context, read bool)
	loseTakeOver bool

	mu sync.Mutex
	at map[string]time.Time
}

func (s *atTheFloor) RowCrossing() time.Duration { return coord.MaxValueCrossing }

// mark notes the first instant of what, on the case's clock.
func (s *atTheFloor) mark(what string, args ...any) {
	key := fmt.Sprintf(what, args...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.at[key]; !seen {
		s.at[key] = time.Now()
	}
}

// when is the first instant of what, and whether it came.
func (s *atTheFloor) when(what string, args ...any) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, seen := s.at[fmt.Sprintf(what, args...)]
	return at, seen
}

func (s *atTheFloor) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	if stamp.State == CaptureBegun {
		s.mark("asked begun %d", stamp.CaptureSeq)
	}
	s.last(ctx, false)
	took, err := s.PendingStore.StampCapture(ctx, turnID, stamp)
	if took && stamp.State != CaptureBegun {
		s.mark("made %s %d", stamp.State, stamp.CaptureSeq)
	}
	return took, err
}

func (s *atTheFloor) StandBy(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	s.mark("asked standby %d", capture.CaptureSeq)
	s.last(ctx, false)
	return s.PendingStore.StandBy(ctx, turnID, capture)
}

func (s *atTheFloor) TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error) {
	s.mark("asked takeover %d", capture.CaptureSeq)
	s.last(ctx, false)
	took, err := s.PendingStore.TakeOver(ctx, turnID, standby, capture)
	if took {
		s.mark("made takeover %d", capture.CaptureSeq)
	}
	if s.loseTakeOver && err == nil {
		return false, errors.New("the store's answer never reached its asker")
	}
	return took, err
}

func (s *atTheFloor) SettleBegun(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	s.last(ctx, false)
	took, err := s.PendingStore.SettleBegun(ctx, turnID, stamp)
	if took {
		s.mark("settled %s %d", stamp.State, stamp.CaptureSeq)
	}
	return took, err
}

func (s *atTheFloor) StandDown(ctx context.Context, turnID string, standby SessionCapture) (bool, error) {
	s.last(ctx, false)
	return s.PendingStore.StandDown(ctx, turnID, standby)
}

func (s *atTheFloor) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	s.last(ctx, true)
	return s.PendingStore.Get(ctx, turnID)
}

func (s *atTheFloor) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	s.last(ctx, false)
	return s.PendingStore.ListActiveForSeat(ctx, handle)
}

// floorKeep is a keep whose reads before the upload spend their room but a
// second and whose upload takes the pace of a session at its bound but a
// second ([floorKeep.last], [floorKeep.upload], set in the case's bubble).
type floorKeep struct {
	*sessionKeep
	last   func(ctx context.Context, read bool)
	upload func()
}

func (k *floorKeep) MintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error) {
	k.last(ctx, false)
	return k.sessionKeep.MintedAt(ctx, turnID, workKey, workSince)
}

func (k *floorKeep) Upload(ctx context.Context, spool executorkeep.SealedSpool,
	sealed io.Reader) (executorkeep.SealedObject, error) {

	k.upload()
	return k.sessionKeep.Upload(ctx, spool, sealed)
}

// floorWrites is the case's executor log whose every capture record spends its
// room but a second before it answers.
type floorWrites struct {
	*sessionWrites
	last func(ctx context.Context, read bool)
}

func (w floorWrites) Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error) {
	w.last(ctx, false)
	return w.sessionWrites.Capture(ctx, c)
}

// floorArchive is an archive whose first read waits ([floorArchive.wait], set
// in the case's bubble), a scrub at its bound, and which then streams body.
type floorArchive struct {
	body io.Reader
	wait func()
	once sync.Once
}

func (a *floorArchive) Read(p []byte) (int, error) {
	a.once.Do(a.wait)
	return a.body.Read(p)
}

// EVERY CAPTURE'S OUTCOME LANDS WITHIN ITS LIFE. Each step between a capture's
// instant and the stamp of what came of it is answered a second inside its
// room, a change of the run's row at the slowest crossing a link is allowed:
// the reservation of its place, its spool to the end of the take, the digest
// it leaves in the box, the keep's two reads, its upload at the pace of a
// session at its bound and its record, and the stamp. So:
//
//   - a stop's take stamps what came of it within a take's life ([takeLife])
//     of the moment it reserved its place, its stamped instant;
//   - a collection's take does the same while its box's pause blocks, its
//     keep started once its take is spooled rather than behind its read;
//   - a takeover the store made and never answered (the standby withdrawn
//     after it, answered not taken, and its row read back) stamps what came
//     of it within a takeover's life ([takeOverLife]) of its instant;
//   - a read standing by for a capture that never stamps, whose taker goes in
//     its wait's last tick, settles that capture at the wait's end in the
//     room a settle is given past it, reads the row once more and takes the
//     place after it within its window ([standbyWindow]) of its own instant;
//
// each within a capture's life ([captureLife]) where it is a capture's, and each
// within a minute of its figure, so each figure is the life it bounds. Exact,
// in a bubble.
//
// Mutation: drop a step from a take's life (its digest), or from a takeover's
// (its read of the row back), and its row lands past it; start a collection's
// keep only once its read returns, and nothing is stamped while its box's
// pause blocks; drop one change of the row from a standby's window (the
// settle still writing past its wait, its last read or its takeover), or wait
// for the capture it stood by for as long as any node reads it as being kept,
// and the takeover lands past the standby's window.
func TestEveryCapturesOutcomeLandsWithinItsLife(t *testing.T) {
	t.Parallel()
	// floor sets every fake of rig at the slowest crossing, and hands back
	// the store, which notes each step, and the archive a take streams.
	type floored struct {
		store   *atTheFloor
		archive *floorArchive
	}
	floor := func(rig *coordRig, writes *sessionWrites, keep *sessionKeep, last func(context.Context, bool),
		upload func(), spool func(*time.Time) func()) floored {

		store := &atTheFloor{PendingStore: rig.coordinator.pending, last: last, at: map[string]time.Time{}}
		var takeBy time.Time
		archive := &floorArchive{body: bytes.NewReader(standbyArchive), wait: spool(&takeBy)}
		rig.runner.Session.Taking = func(ctx context.Context) { takeBy, _ = ctx.Deadline() }
		rig.runner.Session.Keeping = func(ctx context.Context) { last(ctx, false) }
		rig.coordinator.pending = store
		rig.coordinator.keep = &floorKeep{sessionKeep: keep, last: last, upload: upload}
		rig.coordinator.sessions = floorWrites{sessionWrites: writes, last: last}
		rig.coordinator.recordBudget = sessionRecordBudget
		return floored{store: store, archive: archive}
	}
	// within holds a step's life to its figure: never past it, and within a
	// minute and slack of it.
	within := func(t *testing.T, what string, life, figure, slack time.Duration) {
		t.Helper()
		if life > figure {
			t.Errorf("%s came %v after its instant, past its figure, %v", what, life, figure)
		}
		if life < figure-slack-time.Minute {
			t.Errorf("the premise: %s came %v after its instant, short of its figure, %v, by more than a "+
				"minute and %v: a step was not at its bound", what, life, figure, slack)
		}
	}
	t.Run("a stop's take", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			f := floor(rig, writes, keep, func(ctx context.Context, read bool) {
				if deadline, ok := ctx.Deadline(); ok && (!read || time.Until(deadline) <= captureStampBudget) {
					time.Sleep(time.Until(deadline) - time.Second)
				}
			}, func() { time.Sleep(objstore.PaceFor(executorkeep.SealedMaxBytes) - time.Second) },
				func(by *time.Time) func() { return func() { time.Sleep(time.Until(*by) - time.Second) } })
			spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
				takenAs{FakeRunner: rig.runner, body: f.archive}, readCapture(BoxReadStop, Result{}, true))
			if spooled == nil || spooled.standby {
				t.Fatalf("the stop's read spooled %+v, want the capture's own place", spooled)
			}
			if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" {
				t.Fatalf("the keep came to %+v, want the session kept", kept)
			}
			from, asked := f.store.when("asked begun %d", 1)
			to, made := f.store.when("made %s %d", CaptureLanded, 1)
			if !asked || !made {
				t.Fatalf("the premise: the take's reservation asked %t, its landing stamped %t", asked, made)
			}
			within(t, "the take's stamp of what came of it", to.Sub(from), takeLife, 0)
			within(t, "the take's stamp of what came of it", to.Sub(from), captureLife, captureLife-takeLife)
		})
	})
	t.Run("a takeover the store never answered", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
				State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess, TakerEpoch: 1,
				Harness: "claude-code", SessionID: "s-1"}
			if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
				t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
			}
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby {
				t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			refused := begun
			refused.State, refused.Reason = CaptureRefused, "the session could not be uploaded"
			if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
				t.Fatalf("refusing node-a's capture = %v, %v", took, err)
			}
			f := floor(rig, writes, keep, func(ctx context.Context, read bool) {
				if deadline, ok := ctx.Deadline(); ok && (!read || time.Until(deadline) <= captureStampBudget) {
					time.Sleep(time.Until(deadline) - time.Second)
				}
			}, func() { time.Sleep(objstore.PaceFor(executorkeep.SealedMaxBytes) - time.Second) },
				func(*time.Time) func() { return func() {} })
			f.store.loseTakeOver = true
			if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
				t.Fatalf("the standby's keep came to %+v, want its session kept in the place it took", kept)
			}
			from, asked := f.store.when("asked takeover %d", 2)
			to, made := f.store.when("made %s %d", CaptureLanded, 2)
			if !asked || !made {
				t.Fatalf("the premise: the takeover asked %t, its landing stamped %t", asked, made)
			}
			within(t, "the takeover's stamp of what came of it", to.Sub(from), takeOverLife, 0)
			within(t, "the takeover's stamp of what came of it", to.Sub(from), captureLife,
				captureLife-takeOverLife)
		})
	})
	t.Run("a read standing by for a capture that never stamps", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			store := newTakerStore()
			// THE LEASES' CLOCK HELD STILL, so node-a's lease holds for as
			// long as the case says rather than its TTL past its claim.
			frozen := time.Now()
			store.Clock = func() time.Time { return frozen }
			rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
			run := rig.launchKept(t, "t1")
			lease := claimTaker(t, store, takingProcess)
			begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
				State: CaptureBegun, Final: true, At: rig.coordinator.now().Add(-time.Second), Taker: takingProcess,
				TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
			if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
				t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
			}
			// THE LOG NAMES NO RECORD of node-a's capture, so the wait settles
			// it refused once its taker is gone.
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}, noRowFound: true}
			f := floor(rig, writes, keep, func(ctx context.Context, read bool) {
				if deadline, ok := ctx.Deadline(); ok && (!read || time.Until(deadline) <= captureStampBudget) {
					time.Sleep(time.Until(deadline) - time.Second)
				}
			}, func() { time.Sleep(objstore.PaceFor(executorkeep.SealedMaxBytes) - time.Second) },
				func(by *time.Time) func() { return func() { time.Sleep(time.Until(*by) - time.Second) } })
			standby, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
				takenAs{FakeRunner: rig.runner, body: f.archive}, readCapture(BoxReadCollect, Result{}, true))
			if standby == nil || !standby.standby {
				t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			// NODE-A'S LEASE GOES while the standby's wait's last read of the
			// row crosses, which the store answers a second before the wait,
			// begun as the keep begins, ends: that pass finds node-a gone and
			// settles its capture, a write given its own room past the wait's
			// end, and the wait's own read of the row and the takeover come
			// after it.
			waitEnds := time.Now().Add(standbyWait)
			go func() {
				time.Sleep(time.Until(waitEnds) - 10*time.Second)
				if gave, err := store.Backend.Release(context.WithoutCancel(t.Context()),
					coord.TakerResource(takingProcess), takingProcess, lease.Epoch); err != nil || !gave {
					t.Errorf("giving node-a's taker lease back = %v, %v", gave, err)
				}
			}()
			if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
				t.Fatalf("the standby's keep came to %+v, want its session kept in the next place", kept)
			}
			from, asked := f.store.when("asked standby %d", 1)
			settled, refused := f.store.when("settled %s %d", CaptureRefused, 1)
			to, made := f.store.when("made takeover %d", 2)
			if !asked || !refused || !made {
				t.Fatalf("the premise: the standby asked %t, node-a's capture settled refused %t, the "+
					"takeover made %t", asked, refused, made)
			}
			if !settled.After(waitEnds) {
				t.Fatalf("the premise: node-a's capture was settled %v before the standby's wait ended, want "+
					"its settle still writing past it", waitEnds.Sub(settled))
			}
			within(t, "the standby's takeover", to.Sub(from), standbyWindow, 0)
		})
	})
	t.Run("a collection's take whose box's pause blocks", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			box := rig.provider.Box(rig.get("t1").SandboxID)
			pausing, release := make(chan struct{}), make(chan struct{})
			box.PauseFunc = func(ctx context.Context) error {
				close(pausing)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
			rig.resumer.failWith(errors.New("held for the case to read the row"))
			payload, ev := rig.completion("t1")
			f := floor(rig, writes, keep, func(ctx context.Context, read bool) {
				if deadline, ok := ctx.Deadline(); ok && (!read || time.Until(deadline) <= captureStampBudget) {
					time.Sleep(time.Until(deadline) - time.Second)
				}
			}, func() { time.Sleep(objstore.PaceFor(executorkeep.SealedMaxBytes) - time.Second) },
				func(by *time.Time) func() { return func() { time.Sleep(time.Until(*by) - time.Second) } })
			// THE TAKE STREAMS TO ITS DEADLINE, as a pack and a scrub at their
			// bound leave it, once its place is reserved.
			rig.runner.Session.Streaming = func(ctx context.Context) {
				if by, ok := ctx.Deadline(); ok {
					time.Sleep(time.Until(by) - time.Second)
				}
			}
			collected := make(chan error, 1)
			go func() {
				collected <- rig.coordinator.OnCompleted(t.Context(), payload, ev)
			}()
			<-pausing
			// THE PAUSE BLOCKS for a keep's whole budget and its stamp's room
			// past the take, which it began after.
			time.Sleep(SessionKeepBudget + captureStampBudget)
			from, asked := f.store.when("asked begun %d", 1)
			to, made := f.store.when("made %s %d", CaptureLanded, 1)
			close(release)
			if err := <-collected; err == nil {
				t.Error("the refused resume was acknowledged")
			}
			rig.awaitKeeps()
			if !asked {
				t.Fatal("the premise: the collection never reserved its capture's place")
			}
			if !made {
				t.Fatal("the collection's keep had stamped nothing a keep's budget and a stamp's room after " +
					"its box's pause began blocking, want its capture stamped landed while the pause blocks")
			}
			within(t, "the collection's stamp of what came of it", to.Sub(from), takeLife, 0)
			within(t, "the collection's stamp of what came of it", to.Sub(from), captureLife,
				captureLife-takeLife)
		})
	})
}
