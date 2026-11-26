package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A CAPTURE'S BUDGETS COUNT EVERY STEP OF ITS LIFE AT THE SLOWEST CROSSING, a
// change's for a change and a read's for a read, and a reading on another
// machine's clock adds twice the skew allowed one. A keep is three reads, upload
// and record; a take's life, the rest of its take with its reservation's room,
// its digest, keep and stamp; an unanswered takeover's, the takeover,
// withdrawal and read back, keep and stamp; a standby's wait, the longer life
// and one tick; its window, the rest of its take with its reservation's room,
// its digest, three changes past the wait (a settle still writing, the read of
// the log and the settle it decides), its last read and its takeover. Each is
// the docs' figure at today's bounds, to the nanosecond.
//
// Mutation: drop a term, a reading's skew, or count a read as a change.
func TestACapturesBudgetsCountEveryStepAtTheSlowestCrossing(t *testing.T) {
	t.Parallel()
	change := sessionRecordBudget + coord.MaxValueCrossing
	read := sessionRecordBudget + coord.MaxValueReadCrossing
	keep := 3*sessionRecordBudget + objstore.PaceFor(executorkeep.SealedMaxBytes) + sessionRecordBudget
	take := SessionPackWait + SessionScrubWait + change + discardGrace + keep + change
	over := change + change + read + keep + change
	life := max(take, over)
	wait := life + captureWaitTick
	window := SessionPackWait + SessionScrubWait + change + discardGrace + wait + change + change + change +
		read + change
	skew := 2 * objstore.AssumedClockSkew
	for _, c := range []struct {
		name      string
		got, want time.Duration
		floor     string
	}{
		{"one change of a run's row another node reads", captureStampBudget, change, "13m30.025663374s"},
		{"one read of a run's row another node waits on", captureReadBudget, read, "4m30s"},
		{"a keep", SessionKeepBudget, keep, "26m30s"},
		{"a keep's read", keepReadRoom, sessionRecordBudget, "30s"},
		{"a take's life", takeLife, take, "1h2m0.051326748s"},
		{"a takeover's life", takeOverLife, over, "1h11m30.076990122s"},
		{"a capture's life", captureLife, life, "1h11m30.076990122s"},
		{"a capture read on another machine's clock", SessionCaptureBudget, life + skew, "3h11m30.076990122s"},
		{"a standby's wait", standbyWait, wait, "1h11m32.076990122s"},
		{"a standby's window", standbyWindow, window, "2h32m2.205306992s"},
		{"a standby read on another machine's clock", standbyBudget, window + skew, "4h32m2.205306992s"},
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
// its taker's clock's and a reader reads its age on its own, which the engine
// holds within two hours of any other's. So a reader two hours ahead holds a
// begun capture in flight until its life has passed in real time, and a standby
// until its window has; a reader in step, for each plus two hours.
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

// AN UNANSWERED RECORD IS ASKED AGAIN WHATEVER ITS KEY'S AGE READS HERE. The
// executor log refuses a record whose key is past objstore.RecordWithin on its
// own clock, which may stand two hours from the asker's, so the turn's end asks
// again whatever the key's age reads here and the log decides: with the end two
// hours ahead, a key eleven hours old to the log lands, and one past the window
// is refused and named unkept for the log's reason. A reader holds a capture
// whose record awaits its answer until the window and two hours have passed on
// its own clock, and no longer.
//
// Mutation: refuse the record unasked once its key reads past the window here,
// and the end names unkept a capture the log would take; read the reader's
// window without the skew, and one the log may still take reads as answered.
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
		if got := captureAwaitsAnswer(run, readAt) != nil; got != c.awaits {
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

// A KEEP'S READS LEAVE ITS UPLOAD ITS WHOLE PACE. Its two reads of the
// coordination store before the upload (the turn's operations minted, the
// seat's runs listed) each have a room of their own beside the upload's pace in
// the keep's budget, so a store slow or silent on them costs the upload
// nothing. A listing that never answers is cut at its room and decides nothing:
// the upload is handed at least its pace at the slowest and its record's
// budget, and lands. A mint that never answers is cut at its room and refuses
// the capture there, as a failed one does. Exact, in a bubble.
//
// Mutation: hold the reads to the keep's budget alone, and the listing leaves
// nothing to upload in and the mint refuses only at the keep's end; or refuse a
// capture whose listing overran its room, and one the log would take is lost.
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

// A TURN'S END WAITS FOR EACH CAPTURE IT SEES ITS WHOLE LIFE FROM FIRST SIGHT,
// on its own clock, and for each standby its window. Exact, in a bubble:
//
//   - another node's late read stands by after the end began, beside a capture
//     that never stamps: the end waits until its capture lands and is recorded;
//   - a takeover lands after the end's last round, inside its window: the end
//     reads the row once more past the window and waits for it;
//   - a capture stamped hours ahead, so its age never ends it here: the end
//     gives up on it a life after first sight, within a tick.
//
// Mutation: cap the end at one life from its own start, decide off the last
// round's read, or hold no sighting to a bound of its own, and a case fails.
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
// CAPTURE, its life and one read on its own clock, never the capture's stamped
// instant. Exact in a bubble, once the wait runs out: a capture still begun
// under a live lease, stamped hours ahead, has its place taken and the session
// kept; the same capture whose record the log took (named by the turn's row)
// is read there once, settled landed, and nothing is kept; an unanswered
// record may yet land, so the standby stands down; and a standby whose
// coordinator stops first takes nothing.
//
// Mutation: stand down whatever the row says, decide without reading the turn's
// row on the log, take the place beside an unanswered record, or take it
// however the wait ended, and a case fails.
func TestAReadStandingByTakesTheNextPlaceOnceItsOwnWaitOutlastsTheCapture(t *testing.T) {
	t.Parallel()
	stuck := func(t *testing.T, rig *coordRig, run PendingRun) SessionCapture {
		t.Helper()
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now().Add(2 * objstore.AssumedClockSkew),
			Taker: takingProcess, TakerEpoch: 1, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
		}
		return begun
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
	t.Run("the log took the capture's record, its stamp never on the run's row", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			begun := stuck(t, rig, run)
			restore := &keptRestore{rows: map[string]executorkeep.TurnRow{}, noRowFound: true}
			row := landedRow(t, begun)
			onTheLog(restore, row)
			rig.coordinator.restore = restore
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby {
				t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			began := time.Now()
			if kept := rig.coordinator.keepCapture(t.Context(), standby); kept != nil {
				t.Errorf("beside the record the log took, the standby's keep came to %+v, want nothing", kept)
			}
			if took := time.Since(began); took < standbyWait {
				t.Errorf("the standby stood down %v into its keep, before its wait of %v ran out", took,
					standbyWait)
			}
			if read := restore.askedFor(); len(read) != 1 {
				t.Errorf("the turn's row on the log was read %v, want once, as the standby's wait ran out", read)
			}
			if uploads := keep.uploads(); len(uploads) != 0 {
				t.Errorf("uploaded %q, want nothing: the log took node-a's session already", uploads)
			}
			if captured := writes.captured(); len(captured) != 0 {
				t.Errorf("recorded %d captures, want none beside the record the log took", len(captured))
			}
			now := rig.get("t1")
			if now.Kept == nil || !now.Kept.samePlace(begun) || now.Kept.State != CaptureLanded ||
				now.Kept.Object != row.Object.Key.String() {
				t.Errorf("the row keeps %+v, want node-a's capture settled landed as the log's row names it",
					now.Kept)
			}
			if now.Capture == nil || !now.Capture.StandbyAt.IsZero() {
				t.Errorf("the row's newest capture is %+v, want the standby withdrawn", now.Capture)
			}
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

// atTheFloor is a run store across a link at the slowest pace allowed
// ([PendingStore.RowCrossing], [PendingStore.RowReadCrossing]): every change
// of a capture's place, and every read held to one change's room or less,
// answers a second before its room ends ([atTheFloor.last], set in the case's
// bubble), each step at its bound. It notes the instant each step was asked
// and each outcome and takeover the store made. loseTakeOver makes a takeover
// and loses its answer, as a store whose answer never reached its asker does.
// dropFirstSettle spends every attempt at the first settle of a begun stamp
// ([PendingStore.SettleBegun]) and takes none, as a store too slow for a
// write's room does. A read asked once its context has ended answers that end,
// as a store across a link does.
type atTheFloor struct {
	PendingStore
	last            func(ctx context.Context, read bool)
	loseTakeOver    bool
	dropFirstSettle bool

	mu       sync.Mutex
	at       map[string]time.Time
	dropping time.Time
}

func (s *atTheFloor) RowCrossing() time.Duration { return coord.MaxValueCrossing }

func (s *atTheFloor) RowReadCrossing() time.Duration { return coord.MaxValueReadCrossing }

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
	if s.dropFirstSettle {
		// THE FIRST SETTLE'S EVERY ATTEMPT, told by the room they share.
		room, _ := ctx.Deadline()
		s.mu.Lock()
		if s.dropping.IsZero() {
			s.dropping = room
		}
		dropped := s.dropping.Equal(room)
		s.mu.Unlock()
		if dropped {
			s.mark("dropped settle %d", stamp.CaptureSeq)
			s.last(ctx, false)
			return false, errors.New("the store did not take the settle in its room")
		}
	}
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
	if err := ctx.Err(); err != nil {
		return PendingRun{}, false, err
	}
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

// EVERY CAPTURE'S OUTCOME LANDS WITHIN ITS LIFE, each step answering a second
// inside its room at the slowest crossing. A stop's take, and a collection's
// while its box's pause blocks (its keep starts once spooled), stamp within
// [takeLife]; an unanswered takeover, within [takeOverLife]; a standby whose
// capture's taker goes in the wait's last tick, its settle not taken, reads the
// log, settles again, reads the row and takes over within [standbyWindow], a
// save beside it or not; a capture's in [captureLife], each a minute of its figure.
//
// Mutation: drop a step from a figure (a take's digest, a takeover's read back,
// one change past a standby's wait), start a collection's keep behind its read,
// skip the read of the log, wait while any node reads the capture as kept, or
// settle the save in the standby's passes.
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
	// THE STANDBY'S ROW, alone and beside a save of the same job begun under
	// the same lease, the gone taker's capture still within its budget at the
	// wait's last pass or, read on the log there ([Coordinator.awaitStood]'s
	// last read), past it: the standby settles the capture it stands by for
	// alone, so the save adds no change to the window.
	for _, row := range []struct {
		name               string
		saving, pastBudget bool
	}{
		{name: "a read standing by for a capture that never stamps"},
		{name: "a read standing by for a capture that never stamps, beside a save under way", saving: true},
		{name: "a read standing by for a capture past its budget at the wait's last pass, beside a save under way",
			saving: true, pastBudget: true},
	} {
		saving := row.saving
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				// THE LEASES' CLOCK HELD STILL, so node-a's lease holds for as
				// long as the case says rather than its TTL past its claim.
				frozen := time.Now()
				store.Clock = func() time.Time { return frozen }
				rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
				// PAST ITS BUDGET from the lease's going on, on this node's
				// clock, so the wait's last pass is its read of the log.
				var past atomic.Int64
				if row.pastBudget {
					clock := rig.coordinator.now
					rig.coordinator.now = func() time.Time { return clock().Add(time.Duration(past.Load())) }
				}
				run := rig.launchKept(t, "t1")
				lease := claimTaker(t, store, takingProcess)
				begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
					State: CaptureBegun, Final: true, At: rig.coordinator.now().Add(-time.Second),
					Taker: takingProcess, TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
				if saving {
					save := begun
					save.Final, save.Interrupted, save.Through = false, true, new(0)
					if took, err := rig.pending.ReserveSave(t.Context(), "t1", save); err != nil || !took {
						t.Fatalf("reserving node-a's save = %v, %v", took, err)
					}
					begun.CaptureSeq = save.CaptureSeq + 1
				}
				if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
					t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
				}
				if got := rig.get("t1"); saving && (got.savingTake() == nil || got.savingTake().State != CaptureBegun) {
					t.Fatalf("the premise: the row's save under way is %+v, want node-a's save begun", got.Saving)
				}
				// THE LOG NAMES NO RECORD of node-a's capture, so the wait
				// settles it refused once its taker is gone.
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
				// NODE-A'S LEASE GOES while the wait's last read of the row
				// crosses, answered a second before the wait ends: that pass
				// settles node-a's capture in its own room past the wait, which
				// the store does not take; the read past the wait reads the
				// turn's row on the log, which names no record, and settles it
				// refused in a room of its own; the wait's last read and the
				// takeover follow, every change at its bound.
				f.store.dropFirstSettle = true
				waitEnds := time.Now().Add(standbyWait)
				go func() {
					time.Sleep(time.Until(waitEnds) - 10*time.Second)
					if row.pastBudget {
						past.Store(int64(time.Until(begun.At.Add(SessionCaptureBudget)) + time.Second))
					}
					if gave, err := store.Backend.Release(context.WithoutCancel(t.Context()),
						coord.TakerResource(takingProcess), takingProcess, lease.Epoch); err != nil || !gave {
						t.Errorf("giving node-a's taker lease back = %v, %v", gave, err)
					}
				}()
				if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
					t.Fatalf("the standby's keep came to %+v, want its session kept in the next place", kept)
				}
				stood := begun.CaptureSeq
				from, asked := f.store.when("asked standby %d", stood)
				dropped, droppedAny := f.store.when("dropped settle %d", stood)
				settled, refused := f.store.when("settled %s %d", CaptureRefused, stood)
				to, made := f.store.when("made takeover %d", stood+1)
				if !asked || !droppedAny || !refused || !made {
					t.Fatalf("the premise: the standby asked %t, the wait's settle dropped %t, node-a's "+
						"capture settled refused %t, the takeover made %t", asked, droppedAny, refused, made)
				}
				if !dropped.Before(waitEnds) || !settled.After(waitEnds.Add(2*captureStampBudget-time.Minute)) {
					t.Fatalf("the premise: the wait's settle was asked %v before the wait ended and node-a's "+
						"capture settled %v after it, want the first inside the wait and the second past the "+
						"settle's room and the read past the wait", waitEnds.Sub(dropped), settled.Sub(waitEnds))
				}
				within(t, "the standby's takeover", to.Sub(from), standbyWindow, 0)
			})
		})
	}
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

// settleLate is how long into its room [lateSettles] answers a settle a wait
// decided: past the whole wait the cases hold a relaunch to
// ([relaunchAskWait], [settledOnRelaunch]), inside the room a write of the
// run's row is given in the store's own process ([sessionRecordBudget]).
const settleLate = 20 * time.Second

// lateSettles is a run store slow to take the writes a wait decides: each one
// [lateSettles.answer] names is answered only once it lets it, and one it cuts
// first is not applied. A read under an ended context fails, as a store's read
// past its caller's deadline does. It notes every capture-stamp write asked.
type lateSettles struct {
	PendingStore
	answer func(ctx context.Context, write string) error

	mu     sync.Mutex
	writes []string
}

func (s *lateSettles) note(write string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, write)
}

// written is every write of a capture's stamp the store was asked, by name.
func (s *lateSettles) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

func (s *lateSettles) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	s.note("StampCapture")
	if err := s.answer(ctx, "StampCapture"); err != nil {
		return false, err
	}
	return s.PendingStore.StampCapture(ctx, turnID, stamp)
}

func (s *lateSettles) SettleBegun(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	s.note("SettleBegun")
	if err := s.answer(ctx, "SettleBegun"); err != nil {
		return false, err
	}
	return s.PendingStore.SettleBegun(ctx, turnID, stamp)
}

func (s *lateSettles) StandDown(ctx context.Context, turnID string, standby SessionCapture) (bool, error) {
	s.note("StandDown")
	if err := s.answer(ctx, "StandDown"); err != nil {
		return false, err
	}
	return s.PendingStore.StandDown(ctx, turnID, standby)
}

func (s *lateSettles) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if err := ctx.Err(); err != nil {
		return PendingRun{}, false, err
	}
	return s.PendingStore.Get(ctx, turnID)
}

// answeredLate answers write, when it is one of late, [settleLate] after it
// was asked, and any other at once; a write whose context ends first is
// answered with that context's end.
//
//fixedwait:bubble
func answeredLate(late ...string) func(ctx context.Context, write string) error {
	return func(ctx context.Context, write string) error {
		if !slices.Contains(late, write) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleLate):
			return nil
		}
	}
}

// settledOnRelaunch is a kept turn ([keptTurnBy]) taken under this process's
// taker lease in a store of the case's, where node-a's process may take the
// newest capture or stand by for it, its relaunch held to an ask's wait
// ([relaunchAskWait]) for a capture of any size, with room for a write of the
// run's row: the rig, the turn's row on the log, its kept capture and the
// lease store.
//
// Built on [keepingRig], and so only in a bubble ([inBubble]).
//
//fixedwait:bubble
func settledOnRelaunch(t *testing.T) (*coordRig, *keptRestore, SessionCapture, *takerStore) {
	t.Helper()
	store := newTakerStore()
	rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
	rig.relaunchWithin(relaunchAskWait)
	rig.coordinator.recordBudget = sessionRecordBudget
	return rig, restore, kept, store
}

// A WAIT'S SETTLE IS WRITTEN IN ITS OWN ROOM PAST THE WAIT. A wait bounds its
// reads and asks by its own patience, but each settle it decides gets a
// record's budget and this node's crossing of the row, as any stamp off a read
// does. So a store that answers a settle only after the wait ended still takes
// it, and the wait answers what it settled: a relaunch's settle of a gone
// taker's capture the log names landed, its withdrawal of a gone standby, its
// stamp of what its ask landed, and an end's settle at a round's end. Each is
// answered [settleLate] into its room, past [relaunchAskWait] and the round.
//
// Mutation: hold a settle to the wait's deadline, and it is cut unapplied; or
// leave a withdrawal off the wait's copy where the row cannot be read back,
// and the relaunch still reads the standby as standing.
func TestAWaitsSettleIsWrittenInItsOwnRoomPastTheWait(t *testing.T) {
	t.Parallel()
	if settleLate <= relaunchAskWait || settleLate >= sessionRecordBudget {
		t.Fatalf("the premise: a settle answered %s into its room, want past the relaunch's %s and inside a "+
			"write's %s", settleLate, relaunchAskWait, sessionRecordBudget)
	}
	t.Run("a relaunch's settle of a gone taker's capture", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept, store := settledOnRelaunch(t)
			lease := claimTaker(t, store, takingProcess)
			begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
			onTheLog(restore, landedRow(t, begun))
			lapse(store)
			late := &lateSettles{PendingStore: rig.coordinator.pending, answer: answeredLate("SettleBegun")}
			rig.coordinator.pending = late

			started := time.Now()
			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			if waited := time.Since(started); waited <= relaunchAskWait {
				t.Fatalf("the premise: the relaunch returned %s after it began, want its settle answered past "+
					"its %s", waited, relaunchAskWait)
			}
			if c := rig.get("t1").Capture; c == nil || !c.sameTake(begun) || c.State != CaptureLanded {
				t.Errorf("the row holds %+v, want the gone taker's capture settled landed past the wait", c)
			}
			if why != "" || !continued.Capture.samePlace(begun) || continued.Capture.State != CaptureLanded {
				t.Errorf("the relaunch continues %+v (%q), want the capture its wait settled landed", continued,
					why)
			}
		})
	})
	t.Run("a relaunch's withdrawal of a gone standby", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			// A JOB THAT KEPT NOTHING BEFORE the refused capture, so a relaunch
			// reading a copy that still names the standby has nothing older of
			// the job to continue and answers that capture still being kept.
			store := newTakerStore()
			rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
			rig.relaunchWithin(relaunchAskWait)
			rig.coordinator.recordBudget = sessionRecordBudget
			run := rig.launchKept(t, "t1")
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}, noRowFound: true}
			refused := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
				State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: thisProcess, TakerEpoch: 1,
				Harness: "claude-code", SessionID: "s-1"}
			if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
				t.Fatalf("stamping the capture begun = %v, %v", took, err)
			}
			refused.State, refused.Reason = CaptureRefused, "the session could not be uploaded"
			if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
				t.Fatalf("refusing the capture = %v, %v", took, err)
			}
			if got := rig.get("t1"); got.Kept != nil {
				t.Fatalf("the premise: the row keeps %+v, want nothing of the job kept", got.Kept)
			}
			lease := claimTaker(t, store, takingProcess)
			standby := refused
			standby.State, standby.Reason, standby.At = CaptureBegun, "", refused.At.Add(time.Second)
			standby.Taker, standby.TakerEpoch = takingProcess, lease.Epoch
			if stood, err := rig.pending.StandBy(t.Context(), "t1", standby); err != nil || !stood {
				t.Fatalf("standing by = %v, %v", stood, err)
			}
			lapse(store)
			late := &lateSettles{PendingStore: rig.coordinator.pending, answer: answeredLate("StandDown")}
			rig.coordinator.pending = late

			_, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			if c := rig.get("t1").Capture; c == nil || !c.sameTake(refused) || !c.StandbyAt.IsZero() {
				t.Errorf("the row holds %+v, want the gone standby withdrawn past the wait", c)
			}
			if strings.Contains(why, "still being kept") || !strings.Contains(why, unkeptWhy(refused)) {
				t.Errorf("the relaunch answered %q, want the capture whose standby its wait withdrew read as %q, "+
					"never as still being kept", why, unkeptWhy(refused))
			}
		})
	})
	t.Run("a relaunch's stamp of what its ask landed", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, _ := keepingRig(t)
			rig.runner.Carries = true
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
			rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
				Captured: &session.Archive})
			collectHeld(t, rig, "t1")
			unresolved := rig.get("t1").Capture
			if unresolved == nil || unresolved.State != CaptureUnresolved {
				t.Fatalf("the premise: the row holds %+v, want its capture unresolved", unresolved)
			}
			writes.answer(nil)
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
				"t1": keptRow(t, *unresolved)}}
			rig.relaunchWithin(relaunchAskWait)
			rig.coordinator.recordBudget = sessionRecordBudget
			late := &lateSettles{PendingStore: rig.coordinator.pending, answer: answeredLate("StampCapture")}
			rig.coordinator.pending = late

			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			if why != "" || !continued.Capture.samePlace(*unresolved) || continued.Capture.State != CaptureLanded {
				t.Fatalf("the relaunch continues %+v (%q), want the capture its ask landed", continued, why)
			}
			run := rig.get("t1")
			if c := run.Capture; c == nil || !c.sameTake(*unresolved) || c.State != CaptureLanded {
				t.Errorf("the row holds %+v, want the capture the ask landed stamped landed past the wait", c)
			}
			if run.Kept == nil || !run.Kept.samePlace(*unresolved) {
				t.Errorf("the row keeps %+v, want the capture the ask landed", run.Kept)
			}
		})
	})
	t.Run("an end's settle at the end of a round of its wait", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			store := newTakerStore()
			// THE LEASES' CLOCK HELD STILL, so node-a's lease holds for as
			// long as the case says rather than its TTL past its claim.
			frozen := time.Now()
			store.Clock = func() time.Time { return frozen }
			rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
			rig.coordinator.recordBudget = sessionRecordBudget
			run := rig.launchKept(t, "t1")
			lease := claimTaker(t, store, takingProcess)
			begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
				State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
				TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
			if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
				t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
			}
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
				"t1": landedRow(t, begun)}}
			late := &lateSettles{PendingStore: rig.coordinator.pending, answer: answeredLate("SettleBegun")}
			rig.coordinator.pending = late

			type waited struct {
				run   PendingRun
				found bool
				err   error
			}
			ended := make(chan waited, 1)
			row := rig.get("t1")
			go func() {
				got, found, err := rig.coordinator.awaitEndCaptures(t.Context(), row)
				ended <- waited{got, found, err}
			}()
			// NODE-A'S LEASE LAPSES five seconds before the end's first round,
			// a capture's life from first sight, runs out: the wait's next
			// pass settles it, and the store answers past the round's end.
			time.Sleep(captureLife - 5*time.Second)
			lapse(store)
			end := <-ended
			if end.err != nil || !end.found {
				t.Fatalf("the end's wait = %v, %v", end.found, end.err)
			}
			if c := end.run.Capture; c == nil || !c.sameTake(begun) || c.State != CaptureLanded {
				t.Errorf("the end's wait answered %+v, want node-a's capture settled landed past its round", c)
			}
			if c := rig.get("t1").Capture; c == nil || !c.sameTake(begun) || c.State != CaptureLanded {
				t.Errorf("the row holds %+v, want node-a's capture settled landed", c)
			}
		})
	})
}

// AN ASK THAT LEAVES A RECORD UNANSWERED WRITES NOTHING. The row already holds
// the capture unresolved, the same take in the same state, so a relaunch's ask
// the log leaves unanswered asks the record again and writes the store nothing:
// another whole-row write would say what the row says, at a write's whole room.
//
// Mutation: stamp the unresolved answer, and the store is asked a stamp.
func TestAnAskThatLeavesARecordUnansweredWritesNothing(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.runner.Carries = true
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &session.Archive})
		collectHeld(t, rig, "t1")
		unresolved := rig.get("t1").Capture
		if unresolved == nil || unresolved.State != CaptureUnresolved {
			t.Fatalf("the premise: the row holds %+v, want its capture unresolved", unresolved)
		}
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
		late := &lateSettles{PendingStore: rig.coordinator.pending, answer: answeredLate()}
		rig.coordinator.pending = late
		asked := len(writes.captured())

		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
		if len(writes.captured()) <= asked {
			t.Fatalf("the log was asked nothing more by the relaunch, want the capture's record asked again")
		}
		if written := late.written(); len(written) != 0 {
			t.Errorf("the store was asked %s, want no write of a capture whose record is still unanswered",
				strings.Join(written, ", "))
		}
		if c := rig.get("t1").Capture; c == nil || !c.sameTake(*unresolved) || c.State != CaptureUnresolved ||
			c.Reason != unresolved.Reason {
			t.Errorf("the row holds %+v, want the capture as it stood, %+v", c, unresolved)
		}
		if why == "" || continued.Capture.CaptureSeq != 0 {
			t.Errorf("the relaunch continues %+v (%q), want none of a capture whose record was never answered",
				continued, why)
		}
	})
}
