package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// Two reads of one job at once — a stop's and a collection's — of which one
// holds the capture's place and the other stands by for it.

// placeHolderArchive and standbyArchive are what the two reads take of the
// job's session, told apart by their bytes so a case can say whose was kept.
var (
	placeHolderArchive = []byte("the session as the read holding the place took it")
	standbyArchive     = []byte("the session as the read standing by took it")
)

// takenAs is a runner whose take hands back body as the box's session, and
// otherwise answers as the rig's fake does. Where taking is set it is closed
// as the take begins, once the read has read the run's row; where proceed is
// set, the take waits for it before it hands anything back.
type takenAs struct {
	*FakeRunner
	body            io.Reader
	taking, proceed chan struct{}
}

func (r takenAs) TakeSession(ctx context.Context, box Sandbox, take SessionTake) (TakenSession, error) {
	if r.taking != nil {
		close(r.taking)
	}
	if r.proceed != nil {
		select {
		case <-r.proceed:
		case <-ctx.Done():
			return TakenSession{}, ctx.Err()
		}
	}
	taken, err := r.FakeRunner.TakeSession(ctx, box, take)
	if err != nil || taken.Body == nil {
		return taken, err
	}
	taken.Body = io.NopCloser(r.body)
	return taken, nil
}

// heldArchive is an archive a take streams that holds at its first read,
// closing reading, until a word comes on end: nil, and it streams what it
// holds; an error, and its read fails with it, as a scrub that refused the
// archive fails the take's reader.
type heldArchive struct {
	rest    io.Reader
	reading chan struct{}
	end     chan error
	once    sync.Once
	failed  error
}

func newHeldArchive(body []byte) *heldArchive {
	return &heldArchive{rest: bytes.NewReader(body), reading: make(chan struct{}), end: make(chan error, 1)}
}

func (a *heldArchive) Read(p []byte) (int, error) {
	a.once.Do(func() {
		close(a.reading)
		a.failed = <-a.end
	})
	if a.failed != nil {
		return 0, a.failed
	}
	return a.rest.Read(p)
}

// failingArchive is an archive whose every read fails with err.
type failingArchive struct{ err error }

func (a failingArchive) Read([]byte) (int, error) { return 0, a.err }

// errScrubRefused is a take's reader failing as the scrub does where the
// archive changed between its two passes.
var errScrubRefused = errors.New("executor: the session's archive changed its member s-1.jsonl while it was read")

// holdingPlace starts the read that holds the capture's place — a stop's, the
// job over — whose take streams held, and answers once its place is reserved
// and its spool holds at held's first read; what its take came to lands on the
// channel it answers.
func holdingPlace(t *testing.T, rig *coordRig, run PendingRun, held *heldArchive) <-chan *spooledCapture {
	t.Helper()
	done := make(chan *spooledCapture, 1)
	go func() {
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
			takenAs{FakeRunner: rig.runner, body: held}, readCapture(BoxReadStop, Result{}, true))
		done <- spooled
	}()
	select {
	case <-held.reading:
	case spooled := <-done:
		t.Fatalf("the read holding the place ended before its spool read its archive: %+v", spooled)
	}
	return done
}

// standingBy takes run's session as a collection does, while the read holding
// the capture's place is still at it — its stamp begun on the row, its clock a
// second on — and answers what it spooled, which the case holds to be a
// standby's.
func standingBy(t *testing.T, rig *coordRig, run PendingRun, body io.Reader) *spooledCapture {
	t.Helper()
	rig.now = rig.now.Add(time.Second)
	spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
		takenAs{FakeRunner: rig.runner, body: body}, readCapture(BoxReadCollect, Result{}, true))
	return spooled
}

// keptByTheStandby holds rig's row, its uploads and its log to the session the
// read standing by took, kept in the place after the one it stood by for, and
// nothing else: one upload, one record, and no capture left in flight.
func keptByTheStandby(t *testing.T, rig *coordRig, writes *sessionWrites, keep *sessionKeep) {
	t.Helper()
	row := rig.get("t1")
	switch {
	case row.Capture == nil || row.Capture.State != CaptureLanded || row.Capture.CaptureSeq != 2:
		t.Errorf("the row's newest capture is %+v, want the standby's own landed in the next place", row.Capture)
	case row.Kept == nil || row.Kept.CaptureSeq != 2:
		t.Errorf("the row keeps %+v, want the standby's capture", row.Kept)
	}
	if uploads := keep.uploads(); len(uploads) != 1 || !bytes.Equal(uploads[0], standbyArchive) {
		t.Errorf("uploaded %q, want the standby's session once", uploads)
	}
	if captured := writes.captured(); len(captured) != 1 || captured[0].Stamp.CaptureSeq != 2 {
		t.Errorf("recorded %d captures, want the standby's alone", len(captured))
	}
	if captureInFlight(row, rig.now) {
		t.Errorf("a capture is still in flight once the standby's landed: %+v", row.Capture)
	}
	if left := rig.spools(t); len(left) > 0 {
		t.Errorf("spools were left behind: %v", left)
	}
}

// A READ STANDING BY KEEPS THE SESSION WHERE THE READ HOLDING THE PLACE FAILS.
// A stop's read and a collection's of one job take its session at once: the
// stop's holds the capture's place, and the collection's, finding that
// capture begun, takes the session too and stands by for it. Then the read
// holding the place fails — its scrub refuses its archive, its seal fails, its
// upload is refused, or its node stops before it uploads — and while it stands
// refused the turn's end waits, for the standby. The standby's keep then takes
// the place after it and keeps its own session there: one upload, one record,
// the standby's, and nothing left in flight or on the node's disk.
//
// Mutation: stand down where another read's capture is found begun, as a read
// once did, and nothing is kept; have the turn's end's wait pass over a read
// standing by, and the end would not wait for it; carry no standby on the
// place-holder's refusal, and the standby finds itself withdrawn and keeps
// nothing.
func TestAReadStandingByKeepsTheSessionWhereTheReadHoldingThePlaceFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// fail makes the read holding the place fail, once the other read
		// stands by for it.
		fail func(t *testing.T, rig *coordRig, keep *sessionKeep, held *heldArchive, placed <-chan *spooledCapture)
	}{
		{name: "its scrub refused its archive",
			fail: func(t *testing.T, _ *coordRig, _ *sessionKeep, held *heldArchive, placed <-chan *spooledCapture) {
				held.end <- errScrubRefused
				if spooled := <-placed; spooled != nil {
					t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
				}
			}},
		{name: "its seal failed",
			fail: func(t *testing.T, _ *coordRig, keep *sessionKeep, held *heldArchive, placed <-chan *spooledCapture) {
				keep.mu.Lock()
				keep.refuseSealOf = placeHolderArchive
				keep.mu.Unlock()
				held.end <- nil
				if spooled := <-placed; spooled != nil {
					t.Fatalf("a take whose seal failed spooled %+v", spooled)
				}
			}},
		{name: "its upload was refused",
			fail: func(t *testing.T, rig *coordRig, keep *sessionKeep, held *heldArchive, placed <-chan *spooledCapture) {
				keep.mu.Lock()
				keep.refusePutOf = placeHolderArchive
				keep.mu.Unlock()
				held.end <- nil
				spooled := <-placed
				if spooled == nil {
					t.Fatal("the read holding the place spooled nothing")
				}
				if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept == "" {
					t.Fatalf("the place-holder's refused upload kept %+v, want it refused", kept)
				}
			}},
		{name: "its node stopped before it uploaded",
			fail: func(t *testing.T, rig *coordRig, _ *sessionKeep, held *heldArchive, placed <-chan *spooledCapture) {
				held.end <- nil
				spooled := <-placed
				if spooled == nil {
					t.Fatal("the read holding the place spooled nothing")
				}
				rig.coordinator.dropSpool(spooled, "the node stopped before the session could be uploaded")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, writes, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			held := newHeldArchive(placeHolderArchive)
			placed := holdingPlace(t, rig, run, held)
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby || standby.stamp.CaptureSeq != 1 {
				t.Fatalf("the collection's read beside the stop's spooled %+v, want it standing by at the "+
					"stop's place", standby)
			}
			tc.fail(t, rig, keep, held, placed)
			if row := rig.get("t1"); row.Capture == nil || row.Capture.State == CaptureLanded ||
				!captureInFlight(row, rig.now) {
				t.Fatalf("with the place-holder failed, the row holds %+v, which a turn's end would not wait on",
					row.Capture)
			}
			if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
				t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
			}
			keptByTheStandby(t, rig, writes, keep)
		})
	}
}

// settledBegun is a run store that counts the settles of a begun capture whose
// taker is gone ([PendingStore.SettleBegun]).
type settledBegun struct {
	PendingStore
	settled atomic.Int32
}

func (s *settledBegun) SettleBegun(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	s.settled.Add(1)
	return s.PendingStore.SettleBegun(ctx, turnID, capture)
}

// A READ STANDING BY KEEPS THE SESSION WHERE THE READ HOLDING THE PLACE STOPPED
// MID-KEEP. The read holding the capture's place was node-a's, which stopped
// with its capture still begun: its taker lease lapsed, and the turn's row on
// the executor log names no capture of its. The read standing by, waiting for
// that capture, settles it from the log as every wait settles one whose taker
// is gone — refused, its taker stopped — rather than wait out what a capture
// can take, and keeps its own session in the next place.
//
// Mutation: have the standby's wait pass over a capture whose taker is gone,
// and it waits for as long as that capture could take, and the case runs out
// its time.
func TestAReadStandingByKeepsTheSessionWhereTheReadHoldingThePlaceStopped(t *testing.T) {
	t.Parallel()
	store := newTakerStore()
	rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
	run := rig.launchKept(t, "t1")
	lease := claimTaker(t, store, takingProcess)
	begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID, State: CaptureBegun,
		Final: true, At: rig.coordinator.now(), Taker: takingProcess, TakerEpoch: lease.Epoch,
		Harness: "claude-code", SessionID: "s-1"}
	if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
		t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
	}
	// THE TURN'S ROW ON THE LOG, its begin's: it names no capture.
	rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": {Handle: "swe", Turn: "t1"}}}
	standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
	if standby == nil || !standby.standby {
		t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
	}
	lapse(store)
	settles := &settledBegun{PendingStore: rig.coordinator.pending}
	rig.coordinator.pending = settles
	if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
		t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
	}
	if n := settles.settled.Load(); n != 1 {
		t.Errorf("node-a's capture was settled from the log %d times, want once", n)
	}
	keptByTheStandby(t, rig, writes, keep)
}

// A READ WHOSE RESERVATION ANOTHER READ BEAT STANDS BY FOR THAT READ'S CAPTURE.
// A collection's read reads the run's row before either read has a place, and
// its reservation then finds the place the stop's read reserved meanwhile:
// rather than walk away, it stands by for that capture, and where the stop's
// read's scrub refuses its archive it keeps its own session in the next place.
//
// Mutation: stand down where the store refuses the reservation, as a read
// once did, and nothing is kept.
func TestAReadWhoseReservationAnotherReadBeatStandsByForItsCapture(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	run := rig.launchKept(t, "t1")
	taking, proceed := make(chan struct{}), make(chan struct{})
	second := make(chan *spooledCapture, 1)
	go func() {
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
			takenAs{FakeRunner: rig.runner, body: bytes.NewReader(standbyArchive), taking: taking, proceed: proceed},
			readCapture(BoxReadCollect, Result{}, true))
		second <- spooled
	}()
	select {
	case <-taking:
	case got := <-second:
		t.Fatalf("the collection's read ended before its take: %+v", got)
	}
	// THE COLLECTION'S READ HAS READ THE ROW, which held no capture: now the
	// stop's read reserves the place, and holds in its spool.
	held := newHeldArchive(placeHolderArchive)
	placed := holdingPlace(t, rig, run, held)
	rig.now = rig.now.Add(time.Second)
	close(proceed)
	standby := <-second
	if standby == nil || !standby.standby || standby.stamp.CaptureSeq != 1 {
		t.Fatalf("the read whose reservation the stop's beat spooled %+v, want it standing by at the stop's "+
			"place", standby)
	}
	held.end <- errScrubRefused
	if spooled := <-placed; spooled != nil {
		t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
	}
	if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
		t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
	}
	keptByTheStandby(t, rig, writes, keep)
}

// askedTakeOver is a run store that counts the asks to take the place after a
// capture stood by for ([PendingStore.TakeOver]).
type askedTakeOver struct {
	PendingStore
	asked atomic.Int32
}

func (s *askedTakeOver) TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error) {
	s.asked.Add(1)
	return s.PendingStore.TakeOver(ctx, turnID, standby, capture)
}

// A READ STANDING BY KEEPS NOTHING ONCE THE CAPTURE IT STOOD BY FOR LANDS. The
// read holding the place keeps the job's session, and the standby's keep, which
// finds that capture landed, asks the store for no place and uploads and
// records nothing: one upload of the session, the place-holder's, no capture in
// the place after it, and the standby's spool gone from the node's disk.
//
// Mutation: have the standby ask for the next place whatever the capture it
// stood by for came to, and it asks beside a capture that landed, which only
// the store's own refusal keeps from a second upload of the session (the
// shared suite's case of a standby taking the next place holds the store to
// that).
func TestAReadStandingByKeepsNothingOnceTheCaptureItStoodByForLands(t *testing.T) {
	t.Parallel()
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
	if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" {
		t.Fatalf("the place-holder's keep came to %+v, want the session kept", kept)
	}
	asks := &askedTakeOver{PendingStore: rig.coordinator.pending}
	rig.coordinator.pending = asks
	if kept := rig.coordinator.keepCapture(t.Context(), standby); kept != nil {
		t.Errorf("the standby's keep came to %+v beside a capture that landed, want nothing", kept)
	}
	if n := asks.asked.Load(); n != 0 {
		t.Errorf("the standby asked the store %d times for the place after a capture that landed, want never", n)
	}
	row := rig.get("t1")
	if row.Capture == nil || row.Capture.State != CaptureLanded || row.Capture.CaptureSeq != 1 {
		t.Errorf("the row's newest capture is %+v, want the place-holder's, landed", row.Capture)
	}
	if uploads := keep.uploads(); len(uploads) != 1 || !bytes.Equal(uploads[0], placeHolderArchive) {
		t.Errorf("uploaded %q, want the place-holder's session once", uploads)
	}
	if captured := writes.captured(); len(captured) != 1 {
		t.Errorf("recorded %d captures, want the place-holder's alone", len(captured))
	}
	if left := rig.spools(t); len(left) > 0 {
		t.Errorf("spools were left behind: %v", left)
	}
}

// A READ STANDING BY THAT CANNOT KEEP WHAT IT TOOK IS WITHDRAWN. Its spool
// failed, or its node stopped before its keep: either way it holds nothing it
// could keep, so it withdraws from the capture it stood by for, and once the
// read holding the place fails, no turn's end waits for a read that keeps
// nothing — and the place-holder's refusal stands as the capture's outcome.
//
// Mutation: leave a standby whose spool failed standing, or refuse a standby
// whose node stops as a capture of its own, and the turn's end waits for a read
// that will keep nothing.
func TestAReadStandingByThatCannotKeepWhatItTookIsWithdrawn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// stand has the read stand by and then cease to be able to keep what
		// it took.
		stand func(t *testing.T, rig *coordRig, run PendingRun)
	}{
		{name: "its spool failed", stand: func(t *testing.T, rig *coordRig, run PendingRun) {
			if spooled := standingBy(t, rig, run, failingArchive{err: errScrubRefused}); spooled != nil {
				t.Fatalf("a read whose own spool failed spooled %+v", spooled)
			}
		}},
		{name: "its node stopped before its keep", stand: func(t *testing.T, rig *coordRig, run PendingRun) {
			spooled := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if spooled == nil || !spooled.standby {
				t.Fatalf("the collection's read beside the stop's spooled %+v, want it standing by", spooled)
			}
			rig.coordinator.dropSpool(spooled, "the node stopped before the session could be uploaded")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			held := newHeldArchive(placeHolderArchive)
			placed := holdingPlace(t, rig, run, held)
			tc.stand(t, rig, run)
			if row := rig.get("t1"); row.Capture == nil || !row.Capture.StandbyAt.IsZero() {
				t.Fatalf("the row holds %+v, want no read standing by for its capture", row.Capture)
			}
			held.end <- errScrubRefused
			if spooled := <-placed; spooled != nil {
				t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
			}
			if row := rig.get("t1"); row.Capture == nil || row.Capture.State != CaptureRefused ||
				captureInFlight(row, rig.now) {
				t.Errorf("the row holds %+v, want the place-holder's refusal, which no end waits on", row.Capture)
			}
		})
	}
}

// A READ STANDING BY WHOSE TAKER IS GONE IS WITHDRAWN, NOT WAITED FOR. The read
// holding the place was refused, and the read that stood by for it was taken
// under a taker lease that has since lapsed — its process stopped before it
// kept anything — so the turn's end's wait, asking after that lease as it asks
// after a begun capture's, withdraws it and goes on, rather than sit it out for
// as long as a standby may stand.
//
// IN A SYNCTEST BUBBLE, so the wait's own bound is the bubble's to run out,
// which only the wait's ticks advance.
//
// Mutation: leave a standby whose taker is gone to its age, and the wait ends
// on its bound with the standby still standing.
func TestAReadStandingByWhoseTakerIsGoneIsWithdrawnNotWaitedFor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := newTakerStore()
		rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
		run := rig.launchKept(t, "t1")
		refused := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureRefused, Reason: "the session could not be uploaded", Final: true,
			At: rig.coordinator.now(), Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
			t.Fatalf("stamping the refused capture = %v, %v", took, err)
		}
		lease := claimTaker(t, store, takingProcess)
		standby := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: refused.At.Add(time.Second), Taker: takingProcess,
			TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
		if stood, err := rig.pending.StandBy(t.Context(), "t1", standby); err != nil || !stood {
			t.Fatalf("standing by = %v, %v", stood, err)
		}
		lapse(store)
		got, found, err := rig.coordinator.awaitCapture(t.Context(), rig.get("t1"), SessionCaptureBudget)
		if err != nil || !found {
			t.Fatalf("awaitCapture = %v, %v", found, err)
		}
		if got.Capture == nil || !got.Capture.StandbyAt.IsZero() || captureInFlight(got, rig.coordinator.now()) {
			t.Errorf("the wait ended on %+v, want the gone standby withdrawn and nothing left in flight",
				got.Capture)
		}
	})
}
