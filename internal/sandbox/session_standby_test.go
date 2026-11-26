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
	"github.com/crewlet/crewlet/internal/objstore"
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
// set, the take waits for it before it hands anything back, holding its pack,
// so a case that reads beside it stages the job where packs need no node's
// slot ([ownMemory]).
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

// ownMemory moves run's job to a cell that owns its memory, where a take's pack
// takes no node's pack slot ([Coordinator.packSlot]), and answers its row:
// there alone can a second read take the session while a first read's pack is
// held.
func ownMemory(t *testing.T, rig *coordRig, run PendingRun) PendingRun {
	t.Helper()
	everyCell(t, rig)
	reshape(t, rig, run.TurnID, func(r *PendingRun) { r.Placement = string(Container) })
	return rig.get(run.TurnID)
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
	return holdingPlaceAt(t, rig, run, held, readCapture(BoxReadStop, Result{}, true))
}

// holdingPlaceAt is [holdingPlace] whose read takes the session at the point at
// says: a stop's whose end could not end its job, its session taken as the job
// stood, among them.
func holdingPlaceAt(t *testing.T, rig *coordRig, run PendingRun, held *heldArchive,
	at capturePoint) <-chan *spooledCapture {

	t.Helper()
	done := make(chan *spooledCapture, 1)
	go func() {
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
			takenAs{FakeRunner: rig.runner, body: held}, at)
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
// read standing by took, kept in the place after the one it stood by for, the
// second, and nothing else: one upload, one record, and no capture left in
// flight.
func keptByTheStandby(t *testing.T, rig *coordRig, writes *sessionWrites, keep *sessionKeep) {
	t.Helper()
	keptByTheStandbyAt(t, rig, writes, keep, 2)
}

// keptByTheStandbyAt is [keptByTheStandby] for a standby that keeps its own
// session in place, the place after the one it stood by for.
func keptByTheStandbyAt(t *testing.T, rig *coordRig, writes *sessionWrites, keep *sessionKeep, place int64) {
	t.Helper()
	row := rig.get("t1")
	switch {
	case row.Capture == nil || row.Capture.State != CaptureLanded || row.Capture.CaptureSeq != place:
		t.Errorf("the row's newest capture is %+v, want the standby's own landed in the next place, %d",
			row.Capture, place)
	case row.Kept == nil || row.Kept.CaptureSeq != place:
		t.Errorf("the row keeps %+v, want the standby's capture, in place %d", row.Kept, place)
	}
	if uploads := keep.uploads(); len(uploads) != 1 || !bytes.Equal(uploads[0], standbyArchive) {
		t.Errorf("uploaded %q, want the standby's session once", uploads)
	}
	if captured := writes.captured(); len(captured) != 1 || captured[0].Stamp.CaptureSeq != place {
		t.Errorf("recorded %d captures, want the standby's alone, in place %d", len(captured), place)
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
// capture begun, stands by for it. Then the read holding the place fails (its
// scrub refuses its archive, its seal fails, its upload is refused, or its
// node stops before it uploads) and the turn's end waits for the standby,
// whose keep takes the place after it: one upload, one record, the standby's,
// and nothing left in flight or on the node's disk.
//
// Mutation: stand down where another read's capture is found begun; have the
// turn's end's wait pass over a read standing by; carry no standby on the
// place-holder's refusal, so the standby is withdrawn and keeps nothing.
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
				rig.coordinator.dropSpool(t.Context(), spooled, executorkeep.LossKeepFailed,
					"the node stopped before the session could be uploaded")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := keepingRig(t)
				run := rig.launchKept(t, "t1")
				held := newHeldArchive(placeHolderArchive)
				placed := holdingPlace(t, rig, run, held)
				standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
				if standby == nil || !standby.standingBy() || standby.stamp.CaptureSeq != 1 {
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
// the executor log names no capture of its. The standby settles that capture
// from the log (refused, its taker stopped) rather than wait out what a
// capture can take, and keeps its own session in the next place.
//
// IN A SYNCTEST BUBBLE, a standby that waited such a capture out would run out
// its own budget on the bubble's clock, and the case fail on its assertions.
//
// Mutation: have the standby's wait pass over a capture whose taker is gone,
// and it waits as long as that capture could take, and keeps nothing.
func TestAReadStandingByKeepsTheSessionWhereTheReadHoldingThePlaceStopped(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
		run := rig.launchKept(t, "t1")
		lease := claimTaker(t, store, takingProcess)
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
			TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
		}
		// THE TURN'S ROW ON THE LOG, its begin's: it names no capture.
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
			"t1": {Handle: "swe", Turn: "t1"}}}
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standingBy() {
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
	})
}

// A READ WHOSE RESERVATION ANOTHER READ BEAT STANDS BY FOR THAT READ'S CAPTURE.
// A collection's read reads the run's row before either read has a place, and
// its reservation then finds the place the stop's read reserved meanwhile:
// rather than walk away, it stands by for that capture, and where the stop's
// read's scrub refuses its archive it keeps its own session in the next place.
//
// Mutation: stand down where the store refuses the reservation, as a read
// once did, and nothing is kept; wait at the standby's own bound, and a case
// whose bounds differ goes red.
func TestAReadWhoseReservationAnotherReadBeatStandsByForItsCapture(t *testing.T) {
	t.Parallel()
	for name, tc := range standbyBounds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := keepingRig(t)
				takingAt(keep, tc.own)
				run := ownMemory(t, rig, rig.launchKept(t, "t1"))
				taking, proceed := make(chan struct{}), make(chan struct{})
				second := make(chan *spooledCapture, 1)
				go func() {
					spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
						takenAs{FakeRunner: rig.runner, body: bytes.NewReader(standbyArchive), taking: taking,
							proceed: proceed},
						readCapture(BoxReadCollect, Result{}, true))
					second <- spooled
				}()
				select {
				case <-taking:
				case got := <-second:
					t.Fatalf("the collection's read ended before its take: %+v", got)
				}
				// THE COLLECTION'S READ HAS READ THE ROW, which held no capture: now
				// the stop's read reserves the place at its own bound, and holds in
				// its spool.
				takingAt(keep, tc.stood)
				held := newHeldArchive(placeHolderArchive)
				placed := holdingPlace(t, rig, run, held)
				takingAt(keep, tc.own)
				rig.now = rig.now.Add(time.Second)
				close(proceed)
				standby := <-second
				if standby == nil || !standby.standingBy() || standby.stamp.CaptureSeq != 1 {
					t.Fatalf("the read whose reservation the stop's beat spooled %+v, want it standing by at "+
						"the stop's place", standby)
				}
				// AT THE STOP'S CAPTURE'S OWN BOUND, off the row read again, never
				// the standby's own: a version-1 reader's turn end holds a text
				// bound's wait.
				if standby.stoodBy != tc.stood {
					t.Errorf("the standby waits for the stop's capture at %v, want the bound it was taken at, %v",
						standby.stoodBy, tc.stood)
				}
				held.end <- errScrubRefused
				if spooled := <-placed; spooled != nil {
					t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
				}
				if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
					t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
				}
				keptByTheStandby(t, rig, writes, keep)
			})
		})
	}
}

// standbyBounds are the bounds a read standing by and the capture it stands by
// for are each taken at: the same, and each way apart, so a wait sized off the
// standby's own bound rather than that capture's shows ([Coordinator.besideAnother]).
var standbyBounds = map[string]struct{ own, stood captureBound }{
	"both at the text bound":                        {atTextBound, atTextBound},
	"a kept-bound read beside a text-bound capture": {atKeptBound, atTextBound},
	"a text-bound read beside a kept-bound capture": {atTextBound, atKeptBound},
}

// takingAt has keep's policy take at bound from here on.
func takingAt(keep *sessionKeep, bound captureBound) {
	keep.mu.Lock()
	defer keep.mu.Unlock()
	keep.policy.KeptBound = bound == atKeptBound
	keep.policy.MediaApart = keep.policy.MediaApart || bound == atKeptBound
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
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlace(t, rig, run, held)
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standingBy() {
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
	})
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
			if spooled == nil || !spooled.standingBy() {
				t.Fatalf("the collection's read beside the stop's spooled %+v, want it standing by", spooled)
			}
			rig.coordinator.dropSpool(t.Context(), spooled, executorkeep.LossKeepFailed,
				"the node stopped before the session could be uploaded")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
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
	inBubble(t, func(t *testing.T) {
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
		got, found, err := rig.coordinator.awaitCapture(t.Context(), rig.get("t1"), SessionCaptureBudget,
			captureInFlight)
		if err != nil || !found {
			t.Fatalf("awaitCapture = %v, %v", found, err)
		}
		if got.Capture == nil || !got.Capture.StandbyAt.IsZero() || captureInFlight(got, rig.coordinator.now()) {
			t.Errorf("the wait ended on %+v, want the gone standby withdrawn and nothing left in flight",
				got.Capture)
		}
	})
}

// A TURN'S END WAITS FOR A STANDBY'S TAKEOVER AND ITS KEEP. The read holding the
// place keeps as long as a capture can live and is refused at the end of it;
// only then does the standby take the place after it, and its keep takes as
// long as a keep can. The turn's end, a loss's whose box is gone, waits for
// all of it, then records the end with the standby's capture kept, nothing
// named unkept and the turn not gone on past it.
//
// IN A SYNCTEST BUBBLE, so the waits run on the bubble's clock.
//
// Mutation: hold the end's wait to one capture's life from its own start, and
// it records the end ahead of the standby's keep, naming its capture begun and
// never finished.
func TestATurnsEndWaitsForAStandbysTakeoverAndKeep(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping the read holding the place = %v, %v", took, err)
		}
		standby := begun
		standby.At = begun.At.Add(time.Second)
		if stood, err := rig.pending.StandBy(t.Context(), "t1", standby); err != nil || !stood {
			t.Fatalf("standing by = %v, %v", stood, err)
		}
		kept := make(chan struct{})
		go func() {
			defer close(kept)
			// THE READ HOLDING THE PLACE, refused at the end of a capture's
			// life; the standby's takeover, and its keep at a keep's.
			time.Sleep(captureLife - time.Minute)
			refused := begun
			refused.State, refused.Reason = CaptureRefused, "the session could not be uploaded"
			if took, err := rig.pending.StampCapture(context.WithoutCancel(t.Context()), "t1", refused); err != nil ||
				!took {
				t.Errorf("refusing the read holding the place = %v, %v", took, err)
				return
			}
			over := standby
			over.CaptureSeq, over.At = 2, standby.At.Add(time.Second)
			if took, err := rig.pending.TakeOver(context.WithoutCancel(t.Context()), "t1", standby, over); err != nil ||
				!took {
				t.Errorf("the standby's takeover = %v, %v", took, err)
				return
			}
			time.Sleep(SessionKeepBudget - time.Minute)
			landed := over
			landed.State, landed.Object, landed.ObjectHash, landed.ObjectSize =
				CaptureLanded, objstore.KeyAt(rig.coordinator.now()).String(), "h-1", 1
			if took, err := rig.pending.StampCapture(context.WithoutCancel(t.Context()), "t1", landed); err != nil ||
				!took {
				t.Errorf("landing the standby's capture = %v, %v", took, err)
			}
		}()
		rig.coordinator.finish(t.Context(), rig.get("t1"), ending{fence: fenceOf(run),
			note: &failureNote{reason: string(LossBoxGone), detail: "the box is gone from its host"}})
		for range 3 {
			if rig.fireRetries() == 0 {
				break
			}
		}
		<-kept
		ends := writes.endedRecords()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the lost turn's one", len(ends))
		}
		if unkept := ends[0].Ending.Unkept; unkept != nil || ends[0].Ending.WentOn {
			t.Errorf("the end names %+v unkept (gone on: %v), want the standby's capture kept and nothing unkept",
				unkept, ends[0].Ending.WentOn)
		}
	})
}

// A LOSS'S READ STANDS BY FOR A CAPTURE OF THE JOB STILL BEING KEPT. A lost
// run's ending reads its box for the session before it reclaims it, and passed
// it over wherever the row held a capture of the job taken once it had ended —
// one still being kept among them, whose keep could yet fail with the box
// already reclaimed unread. Here node-a's collection holds the job's final
// capture, begun under a lease that still holds: the loss's read is made all
// the same, stands by for it, and where node-a's capture is refused keeps its
// own session in the next place. Beside a final capture that has settled, no
// loss's read is made at all.
//
// Mutation: pass a loss's read over beside a final capture still begun, as it
// was, and the box is reclaimed unread beside a keep that then fails.
func TestALossReadStandsByForACaptureOfTheJobStillBeingKept(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
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
		lostRow := func() PendingRun {
			lost := rig.get("t1")
			lost.Ending = &RecordedEnding{ID: "e-1", Reason: string(LossEngineRestarted), Reclaim: true}
			lost.Lost = &JobLoss{Cause: LossEngineRestarted}
			return lost
		}
		if !rig.coordinator.capturesLoss(lostRow()) {
			t.Fatal("a lost run's ending passes its box over beside a capture of the job still being kept")
		}
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		rig.now = rig.now.Add(time.Second)
		spooled, _, _ := rig.coordinator.readLost(t.Context(), lostRow())
		if spooled == nil || !spooled.standingBy() || spooled.stamp.CaptureSeq != 1 {
			t.Fatalf("the loss's read beside node-a's capture spooled %+v, want it standing by for it", spooled)
		}
		refused := begun
		refused.State, refused.Reason = CaptureRefused, "the session could not be uploaded"
		if took, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil || !took {
			t.Fatalf("refusing node-a's capture = %v, %v", took, err)
		}
		if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" {
			t.Fatalf("the loss's read came to %+v, want the session kept", kept)
		}
		row := rig.get("t1")
		if row.Kept == nil || row.Kept.CaptureSeq != 2 || row.Kept.State != CaptureLanded {
			t.Errorf("the row keeps %+v, want the loss's read's capture landed in the next place", row.Kept)
		}
		if uploads := keep.uploads(); len(uploads) != 1 || len(writes.captured()) != 1 {
			t.Errorf("uploaded %d sessions and recorded %d, want the loss's read's once", len(uploads),
				len(writes.captured()))
		}
		if rig.coordinator.capturesLoss(lostRow()) {
			t.Error("a lost run's ending reads its box again beside a final capture of the job that has landed")
		}
	})
}

// refusesPlace is an executor log that refuses every record of the capture at
// place, as a log refuses one it will never take, and otherwise answers as the
// case's own does.
type refusesPlace struct {
	*sessionWrites
	place int64
}

func (w refusesPlace) Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error) {
	if c.Stamp.CaptureSeq == w.place {
		return LogRefused, errors.New("the log will never take this capture")
	}
	return w.sessionWrites.Capture(ctx, c)
}

// A READ STANDING BY TAKES OVER A CAPTURE WHOSE RECORD WENT UNANSWERED ONLY ONCE
// THE LOG REFUSES IT. The read holding the place uploaded the job's session and
// its record was never answered, which may yet be the log's: the standby asks
// it again under its own operation, as the turn's end and a relaunch ask it,
// and goes on by what the log says. Where the log holds it, the standby keeps
// nothing — the session uploaded once, the place-holder's capture landed; where
// the log refuses it, the standby keeps its own in the next place.
//
// Mutation: take over a capture whose record went unanswered without asking it
// again, and beside a record the log holds the session is uploaded twice; or
// never take one over, and where the log refused it nothing is kept.
func TestAReadStandingByTakesOverAnUnansweredCaptureOnlyOnceTheLogRefusesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refused bool
	}{
		{name: "the log holds it"},
		{name: "the log refuses it", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := keepingRig(t)
				run := rig.launchKept(t, "t1")
				held := newHeldArchive(placeHolderArchive)
				placed := holdingPlace(t, rig, run, held)
				standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
				if standby == nil || !standby.standingBy() {
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
				if tc.refused {
					rig.coordinator.sessions = refusesPlace{sessionWrites: writes, place: 1}
				}
				kept := rig.coordinator.keepCapture(t.Context(), standby)
				row := rig.get("t1")
				uploads := keep.uploads()
				switch {
				case !tc.refused && kept != nil:
					t.Errorf("beside a record the log holds, the standby's keep came to %+v, want nothing", kept)
				case !tc.refused && (row.Capture == nil || row.Capture.CaptureSeq != 1 || row.Capture.State != CaptureLanded):
					t.Errorf("the row's newest capture is %+v, want the place-holder's landed once asked again",
						row.Capture)
				case !tc.refused && len(uploads) != 1:
					t.Errorf("uploaded %d sessions, want the place-holder's once", len(uploads))
				case tc.refused && (kept == nil || kept.NotKept != ""):
					t.Errorf("beside a record the log refused, the standby's keep came to %+v, want the session kept",
						kept)
				case tc.refused && (row.Kept == nil || row.Kept.CaptureSeq != 2):
					t.Errorf("the row keeps %+v, want the standby's capture in the next place", row.Kept)
				case tc.refused && (len(uploads) != 2 || !bytes.Equal(uploads[1], standbyArchive)):
					t.Errorf("uploaded %q, want the place-holder's and then the standby's", uploads)
				}
			})
		})
	}
}

// A FINAL READ BESIDE A CAPTURE TAKEN AS THE JOB STOOD TAKES THE NEXT PLACE. A
// stop's read whose end could neither end its job nor poll it takes the
// session as it stands, not final, and reserves the capture's place first; the
// job's collection, which found it over, holds the session whole and finds its
// own reservation of that place refused. It takes the place after it and keeps
// the job's final session there, whatever the stop's capture comes to: here it
// lands, and the row keeps the collection's, final.
//
// Mutation: stand by for every capture whose place another read holds, as the
// first standby did, and the stop's capture lands, the collection's final
// session dropped beside it.
func TestAFinalReadBesideACaptureTakenAsTheJobStoodTakesTheNextPlace(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := keepingRig(t)
		run := ownMemory(t, rig, rig.launchKept(t, "t1"))
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
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlaceAt(t, rig, run, held, readCapture(BoxReadStop, Result{}, false))
		rig.now = rig.now.Add(time.Second)
		close(proceed)
		final := <-second
		if final == nil || final.standingBy() || final.stamp.CaptureSeq != 2 || !final.stamp.Final {
			t.Fatalf("the collection's read beside the stop's capture taken as the job stood spooled %+v, want its "+
				"own final capture in the next place", final)
		}
		held.end <- nil
		stopped := <-placed
		if stopped == nil || stopped.stamp.Final {
			t.Fatalf("the stop's read spooled %+v, want its capture of the job as it stood", stopped)
		}
		for _, spooled := range []*spooledCapture{stopped, final} {
			if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" {
				t.Fatalf("the keep of the capture at %d came to %+v, want it kept", spooled.stamp.CaptureSeq, kept)
			}
		}
		if row := rig.get("t1"); row.Kept == nil || row.Kept.CaptureSeq != 2 || !row.Kept.Final {
			t.Errorf("the row keeps %+v, want the collection's final capture", row.Kept)
		}
		if uploads := keep.uploads(); len(uploads) != 2 || !bytes.Equal(uploads[1], standbyArchive) {
			t.Errorf("uploaded %q, want the stop's and then the collection's", uploads)
		}
	})
}

// failsReservations is a run store that, once armed, answers no reservation of
// a capture's place — a stamp begun — as a store having a bad moment answers
// nothing: the asker hears an error, and the row is left as it was.
type failsReservations struct {
	PendingStore
	armed atomic.Bool
}

func (s *failsReservations) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	if s.armed.Load() && stamp.State == CaptureBegun {
		return false, errNoLeader
	}
	return s.PendingStore.StampCapture(ctx, turnID, stamp)
}

// A READ WHOSE RESERVATION WENT UNANSWERED BESIDE ANOTHER READ'S STANDS BY FOR IT.
// The collection's read reads the run's row before either read has a place,
// and the stop's read reserves the place meanwhile; then the collection's read
// asks for that place, and the store never answers. Settling the place, it
// finds the stop's read holding it: it holds the session whole all the same,
// so it stands by for the stop's capture as a read refused outright does, and
// where the stop's scrub refuses its archive it keeps its own session in the
// next place.
//
// Mutation: walk away from the place another read holds once the reservation
// went unanswered, as such a read once did, and nothing keeps the session;
// wait at the standby's own bound, and a case whose bounds differ goes red.
func TestAReadWhoseReservationWentUnansweredBesideAnotherStandsByForIt(t *testing.T) {
	t.Parallel()
	for name, tc := range standbyBounds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := keepingRig(t)
				takingAt(keep, tc.own)
				run := ownMemory(t, rig, rig.launchKept(t, "t1"))
				store := &failsReservations{PendingStore: rig.coordinator.pending}
				rig.coordinator.pending = store
				taking, proceed := make(chan struct{}), make(chan struct{})
				second := make(chan *spooledCapture, 1)
				go func() {
					spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
						takenAs{FakeRunner: rig.runner, body: bytes.NewReader(standbyArchive), taking: taking,
							proceed: proceed},
						readCapture(BoxReadCollect, Result{}, true))
					second <- spooled
				}()
				select {
				case <-taking:
				case got := <-second:
					t.Fatalf("the collection's read ended before its take: %+v", got)
				}
				// THE COLLECTION'S READ HAS READ THE ROW, which held no capture: now
				// the stop's read reserves the place at its own bound and holds in
				// its spool, and the store answers no reservation after it.
				takingAt(keep, tc.stood)
				held := newHeldArchive(placeHolderArchive)
				placed := holdingPlace(t, rig, run, held)
				takingAt(keep, tc.own)
				store.armed.Store(true)
				rig.now = rig.now.Add(time.Second)
				close(proceed)
				standby := <-second
				if standby == nil || !standby.standingBy() || standby.stamp.CaptureSeq != 1 {
					t.Fatalf("the read whose reservation went unanswered spooled %+v, want it standing by for "+
						"the stop's", standby)
				}
				if standby.stoodBy != tc.stood {
					t.Errorf("the standby waits for the stop's capture at %v, want the bound it was taken at, %v",
						standby.stoodBy, tc.stood)
				}
				held.end <- errScrubRefused
				if spooled := <-placed; spooled != nil {
					t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
				}
				if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
					t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
				}
				keptByTheStandby(t, rig, writes, keep)
			})
		})
	}
}

// lostStandBy is a run store that makes every standby it is asked for and
// answers none, as a store whose replies never reach their asker does.
type lostStandBy struct{ PendingStore }

func (s lostStandBy) StandBy(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	if _, err := s.PendingStore.StandBy(ctx, turnID, capture); err != nil {
		return false, err
	}
	return false, errors.New("the store's answer never reached its asker")
}

// A STANDBY THE STORE NEVER ANSWERED KEEPS THE SESSION WHERE IT STANDS. The store
// made the collection's read standing by for the stop's capture and never said
// so. Walked away from, that standby stood on the row with nothing behind it,
// every wait for the turn holding for it, and the session the read held was
// dropped. The read goes on as one that may stand: it spools the session, its
// keep finds the row naming it, and once the stop's capture is refused it
// keeps its own in the next place.
//
// Mutation: walk away from a standby the store never answered, as a read once
// did, and nothing keeps the session.
func TestAStandbyTheStoreNeverAnsweredKeepsTheSessionWhereItStands(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlace(t, rig, run, held)
		rig.coordinator.pending = lostStandBy{PendingStore: rig.coordinator.pending}
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standingBy() {
			t.Fatalf("the read whose standby the store never answered spooled %+v, want it going on as one", standby)
		}
		held.end <- errScrubRefused
		if spooled := <-placed; spooled != nil {
			t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
		}
		if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
			t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
		}
		keptByTheStandby(t, rig, writes, keep)
	})
}

// lostTakeOver is a run store that makes every takeover it is asked for and
// answers none, as a store whose replies never reach their asker does.
type lostTakeOver struct{ PendingStore }

func (s lostTakeOver) TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error) {
	if _, err := s.PendingStore.TakeOver(ctx, turnID, standby, capture); err != nil {
		return false, err
	}
	return false, errors.New("the store's answer never reached its asker")
}

// A TAKEOVER THE STORE NEVER ANSWERED IS KEPT UNDER WHERE IT WAS MADE. The read
// holding the place was refused, and the store made the standby's takeover of
// the place after it and never said so. Walked away from, that place stood
// begun under a lease that still holds with no keep behind it — every wait for
// the turn holding for it for a capture's whole budget — and the session was
// dropped. The standby settles it: nothing left on the row to withdraw, it
// reads the row, finds the place its own, and keeps its session there.
//
// Mutation: stand down from a takeover the store never answered, as the
// standby once did, and the place stands begun with nothing kept.
func TestATakeoverTheStoreNeverAnsweredIsKeptUnderWhereItWasMade(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlace(t, rig, run, held)
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standingBy() {
			t.Fatalf("the collection's read beside the stop's spooled %+v, want it standing by", standby)
		}
		held.end <- errScrubRefused
		if spooled := <-placed; spooled != nil {
			t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
		}
		rig.coordinator.pending = lostTakeOver{PendingStore: rig.coordinator.pending}
		if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
			t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
		}
		keptByTheStandby(t, rig, writes, keep)
	})
}

// unansweredReadBack is a run store whose connection takes crossing to carry a
// change of a run's row at its bound and readCrossing a read's reply, which
// makes every takeover it is asked for and answers none, as [lostTakeOver]
// does, and which from that ask on never answers a read of the row, as a store
// whose reply is still crossing a slow link does not: the read waits until its
// context ends, and the store notes when the first such read began and when
// the last was let go. reading is closed once the first has begun.
type unansweredReadBack struct {
	PendingStore
	crossing, readCrossing time.Duration
	reading                chan struct{}

	mu        sync.Mutex
	takenOver bool
	began     time.Time
	cut       time.Time
}

func (s *unansweredReadBack) RowCrossing() time.Duration { return s.crossing }

func (s *unansweredReadBack) RowReadCrossing() time.Duration { return s.readCrossing }

func (s *unansweredReadBack) TakeOver(ctx context.Context, turnID string, standby,
	capture SessionCapture) (bool, error) {

	if _, err := s.PendingStore.TakeOver(ctx, turnID, standby, capture); err != nil {
		return false, err
	}
	s.mu.Lock()
	s.takenOver = true
	s.mu.Unlock()
	return false, errors.New("the store's answer never reached its asker")
}

func (s *unansweredReadBack) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	s.mu.Lock()
	takenOver := s.takenOver
	s.mu.Unlock()
	if !takenOver {
		return s.PendingStore.Get(ctx, turnID)
	}
	s.mu.Lock()
	if s.began.IsZero() {
		s.began = time.Now()
		close(s.reading)
	}
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	s.cut = time.Now()
	s.mu.Unlock()
	return PendingRun{}, false, ctx.Err()
}

// readBack is when the store's first unanswered read of the row began and when
// its last was let go, both zero where none was made.
func (s *unansweredReadBack) readBack() (began, cut time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.began, s.cut
}

// A TAKEOVER LEFT UNANSWERED READS ITS ROW WITHIN A READ'S ROOM, NEVER A
// CHANGE'S: a record's budget and this node's crossing of a read's reply at
// the row's bound ([Coordinator.rowReadContext]), as [takeOverLife] counts it.
// The store made the standby's takeover without saying so, then never answered
// the read of the row that says whether the place is its own. Every reader of
// a begun capture holds that read to a bound, and the keep's context has no
// deadline before its budget starts, so held to it alone the read waited as
// long as the store stayed silent. Here it is cut exactly that room after it
// began, on a store whose change crosses in more (exact, in a bubble).
//
// Mutation: read the row under the keep's context alone, and the keep waits a
// minute past the read's room; give it a change's room, and it is cut late.
func TestATakeoverLeftUnansweredReadsItsRowWithinARead(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		const crossing, readCrossing = 7 * time.Minute, 3 * time.Minute
		rig, _, keep := keepingRig(t)
		rig.coordinator.recordBudget = sessionRecordBudget
		run := rig.launchKept(t, "t1")
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlace(t, rig, run, held)
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standingBy() {
			t.Fatalf("the collection's read beside the stop's spooled %+v, want it standing by", standby)
		}
		held.end <- errScrubRefused
		if spooled := <-placed; spooled != nil {
			t.Fatalf("a take whose scrub refused its archive spooled %+v", spooled)
		}
		store := &unansweredReadBack{PendingStore: rig.coordinator.pending, crossing: crossing,
			readCrossing: readCrossing, reading: make(chan struct{})}
		rig.coordinator.pending = store
		room := sessionRecordBudget + readCrossing
		keeping, cancel := context.WithCancel(t.Context())
		defer cancel()
		kept := make(chan *SessionKept, 1)
		go func() { kept <- rig.coordinator.keepCapture(keeping, standby) }()
		select {
		case <-store.reading:
		case got := <-kept:
			t.Fatalf("the premise: the standby's keep came to %+v without reading the row back after its "+
				"takeover went unanswered", got)
		}
		select {
		case got := <-kept:
			if got != nil {
				t.Errorf("the standby's keep came to %+v with the row unread, want it keeping nothing", got)
			}
			if began, cut := store.readBack(); cut.Sub(began) != room {
				t.Errorf("the read of the row was let go %v after it began, want it cut at one read of the "+
					"row, a record's budget and the reply's crossing, %v", cut.Sub(began), room)
			}
		case <-time.After(room + time.Minute):
			t.Errorf("the standby's keep is still waiting %v after its read of the row began, on a read the "+
				"store never answers, past one read of the row, %v", room+time.Minute, room)
			cancel()
			<-kept
		}
		if row := rig.get("t1"); row.Capture == nil || row.Capture.CaptureSeq != 2 ||
			row.Capture.State != CaptureBegun {
			t.Errorf("the row's newest capture is %+v, want the premise: the standby's takeover made, begun",
				row.Capture)
		}
		if uploads := keep.uploads(); len(uploads) != 0 {
			t.Errorf("uploaded %d sessions with the row unread, want none", len(uploads))
		}
	})
}

// refusedTake is a runner whose every take of a box's session is refused with
// err, as a box whose session cannot be read refuses it.
type refusedTake struct {
	*FakeRunner
	err error
}

func (r refusedTake) TakeSession(context.Context, Sandbox, SessionTake) (TakenSession, error) {
	return TakenSession{}, r.err
}

// A READ STANDING BY THAT TOOK NOTHING LEAVES THE CAPTURE IT STOOD BY FOR AS IT
// STANDS. A collection's read finds the stop's capture of the job begun, and
// would stand by for it, but its own take is refused — the box's session could
// not be read — so it holds nothing to stand by with: it says so, and the
// stop's capture stays the row's newest, still being kept, with nothing in the
// place after it for a turn's end to read as the turn's newest and stop
// waiting for that keep.
//
// Mutation: stamp the refusal in the next place, as a read that holds no place
// stamps one, and the capture still being kept is buried under it.
func TestAReadStandingByThatTookNothingLeavesTheCaptureItStoodByFor(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlace(t, rig, run, held)
		rig.now = rig.now.Add(time.Second)
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID),
			refusedTake{FakeRunner: rig.runner, err: errors.New("the box's session could not be read")},
			readCapture(BoxReadCollect, Result{}, true))
		if spooled != nil {
			t.Fatalf("a read whose take was refused spooled %+v", spooled)
		}
		if row := rig.get("t1"); row.Capture == nil || row.Capture.CaptureSeq != 1 ||
			row.Capture.State != CaptureBegun || !captureInFlight(row, rig.now) {
			t.Errorf("beside the refused read, the row's newest capture is %+v, want the stop's, still begun",
				row.Capture)
		}
		held.end <- nil
		if stopped := <-placed; stopped != nil {
			rig.coordinator.keepCapture(t.Context(), stopped)
		}
	})
}

// A WAIT LEAVES A READ STANDING BY WHOSE TAKER STILL HOLDS ITS LEASE. The read
// holding the place was refused, and the read standing by for it is node-a's,
// whose taker lease still holds: every pass of the turn's end's wait asks after
// that lease, finds it held, and leaves the standby standing — waited for, to
// the wait's own bound — never withdrawn as one whose process stopped.
//
// IN A SYNCTEST BUBBLE, so the wait's bound is the bubble's to run out.
//
// Mutation: withdraw a standby whatever its taker's lease says, and the wait's
// first pass withdraws one whose process is still keeping the session.
func TestAWaitLeavesAReadStandingByWhoseTakerHolds(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
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
		got, found, err := rig.coordinator.awaitCapture(t.Context(), rig.get("t1"), 5*captureWaitTick,
			captureInFlight)
		if err != nil || !found {
			t.Fatalf("awaitCapture = %v, %v", found, err)
		}
		if got.Capture == nil || !got.Capture.standsBy(standby) {
			t.Errorf("the wait ended on %+v, want the standby whose taker holds still standing", got.Capture)
		}
		if row := rig.get("t1"); row.Capture == nil || !row.Capture.standsBy(standby) {
			t.Errorf("the row holds %+v after the wait, want the standby whose taker holds still standing",
				row.Capture)
		}
	})
}

// A READ STANDING BY TAKES OVER A CAPTURE BEGUN PAST WHAT ANY NODE'S CLOCK READS
// A CAPTURE AS ABLE TO TAKE. The read holding the place is node-a's, whose
// taker lease still holds, and its capture has stood begun longer than any
// node's clock reads a capture as able to take ([SessionCaptureBudget]), so
// the standby takes the place after it and keeps its own session there,
// rather than wait out its own time and keep nothing.
//
// IN A SYNCTEST BUBBLE, so a standby that waited would run out its own budget
// on the bubble's clock.
//
// Mutation: wait for a begun capture however long it has stood, and the
// standby's wait runs out and it keeps nothing.
func TestAReadStandingByTakesOverACaptureBegunPastItsBudget(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
		run := rig.launchKept(t, "t1")
		lease := claimTaker(t, store, takingProcess)
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
			TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
		}
		standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
		if standby == nil || !standby.standingBy() {
			t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
		}
		rig.now = rig.now.Add(SessionCaptureBudget)
		if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
			t.Fatalf("the standby's keep came to %+v, want the session kept", kept)
		}
		keptByTheStandby(t, rig, writes, keep)
	})
}

// A READ STANDING BY KEEPS NOTHING WHERE THE LOG TOOK THE CAPTURE IT STOOD BY
// FOR. Node-a's lease holds while none of its stamps reach the row. Once that
// capture is begun past its budget, the standby reads the turn's row on the log
// once: named, it keeps nothing and withdraws on the log's word alone, whether
// or not the store takes the landed settle; no record, the row naming only the
// save, or a log that does not answer, it keeps its own; a lease the store
// cannot say of is read as held; a stamp naming no taker goes by age, unread.
// In a synctest bubble.
//
// Mutation: decide by age with the log unread; read it every pass or early;
// decide on the store's take; stand down on any named stamp or an unanswered
// log; read only a lease answering held; or admit a stamp naming no taker.
func TestAReadStandingByKeepsNothingWhereTheLogTookTheCaptureItStoodByFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// took puts node-a's record on the turn's row on the log.
		took bool
		// unread is every read's answer from the log: a log that does not
		// answer.
		unread error
		// unsettled makes the run's store take no settle of a begun stamp
		// ([PendingStore.SettleBegun]) at the standby's last decision: its
		// node's runs stream lost for the settle's whole room.
		unsettled bool
		// leaseUnread makes every read of a taker lease fail while this
		// process's own renewals land: a store that cannot say of node-a's
		// lease.
		leaseUnread bool
		// saveTook makes node-a's capture a job's final take that overtook a
		// save of the job still under way beside it ([overtakenSave]), and
		// puts the SAVE's record on the turn's row on the log: the log took
		// the save and not the final take the standby stands by for.
		saveTook bool
		// noTaker stamps node-a's capture with no taker, as a coordinator
		// with no taker lease writes it, its record on the log all the same.
		noTaker bool
	}{
		{name: "the log took the capture's record, its stamp never on the run's row", took: true},
		{name: "the log holds no record of the capture"},
		{name: "the log does not answer", unread: errors.New("the executor log did not answer")},
		{name: "the log took the capture's record, and the run's record does not take its settle", took: true,
			unsettled: true},
		{name: "the log took the capture's record, and the store cannot say of its taker's lease", took: true,
			leaseUnread: true},
		{name: "the log took the record of the save the capture overtook", saveTook: true},
		{name: "the log took the capture's record, its stamp naming no taker", took: true, noTaker: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
				run := rig.launchKept(t, "t1")
				lease := claimTaker(t, store, takingProcess)
				restore := &keptRestore{rows: map[string]executorkeep.TurnRow{}, noRowFound: true,
					unreadRows: c.unread}
				var begun SessionCapture
				if c.saveTook {
					var save SessionCapture
					save, begun = overtakenSave(t, rig, "t1", takingProcess, lease.Epoch, CaptureBegun)
					onTheLog(restore, landedRow(t, save))
					// ONE SECOND MORE: overtakenSave stamps the final take a
					// second past the clock and standingBy moves the clock a
					// second on, so without it the standby's stamp would be
					// the final take's own instant rather than another take's.
					rig.now = rig.now.Add(time.Second)
				} else {
					begun = SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
						State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
						TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
					if c.noTaker {
						begun.Taker, begun.TakerEpoch = "", 0
					}
					if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
						t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
					}
				}
				row := landedRow(t, begun)
				if c.took {
					onTheLog(restore, row)
				}
				rig.coordinator.restore = restore
				standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
				if standby == nil || !standby.standingBy() {
					t.Fatalf("this node's read beside node-a's capture spooled %+v, want it standing by", standby)
				}
				if standby.stamp.CaptureSeq != begun.CaptureSeq {
					t.Fatalf("the premise: the standby stands by at %d, want node-a's capture's place %d",
						standby.stamp.CaptureSeq, begun.CaptureSeq)
				}
				if c.unsettled {
					rig.coordinator.pending = stallingSettles{PendingStore: rig.coordinator.pending}
				}
				if c.leaseUnread {
					store.failReads(errors.New("coord: the lease store could not say"))
				}
				clock := &stepClock{at: rig.now}
				rig.coordinator.now = clock.now
				kept := make(chan *SessionKept, 1)
				go func() { kept <- rig.coordinator.keepCapture(t.Context(), standby) }()
				time.Sleep(3 * captureWaitTick)
				synctest.Wait()
				if read := restore.askedFor(); len(read) != 0 {
					t.Fatalf("the turn's row on the log was read %v while node-a's capture was within its "+
						"budget, want it read only at the standby's last decision", read)
				}
				clock.advance(SessionCaptureBudget)
				got := <-kept
				if held := heldBy(t, store, takingProcess); held == nil || held.Epoch != lease.Epoch {
					t.Fatalf("the premise: node-a's taker lease is %+v, want it still held at %d", held, lease.Epoch)
				}
				switch read := restore.askedFor(); {
				case c.noTaker && len(read) != 0:
					t.Errorf("the turn's row on the log was read %v for a stamp that names no taker, want it "+
						"left to the capture's age", read)
				case !c.noTaker && len(read) != 1:
					t.Errorf("the turn's row on the log was read %v, want once, at the standby's last decision",
						read)
				}
				if !c.took || c.noTaker {
					if got == nil || got.NotKept != "" {
						t.Fatalf("the standby's keep came to %+v, want its own session kept in the next place", got)
					}
					keptByTheStandbyAt(t, rig, writes, keep, begun.CaptureSeq+1)
					return
				}
				if got != nil {
					t.Errorf("the standby's keep came to %+v, want nothing kept beside the record the log took", got)
				}
				if uploads := keep.uploads(); len(uploads) != 0 {
					t.Errorf("uploaded %q, want nothing: the log took node-a's session already", uploads)
				}
				if captured := writes.captured(); len(captured) != 0 {
					t.Errorf("recorded %d captures, want none beside the record the log took", len(captured))
				}
				now := rig.get("t1")
				switch {
				case c.unsettled:
					if now.Capture == nil || !now.Capture.sameTake(begun) || now.Capture.State != CaptureBegun ||
						!now.Capture.StandbyAt.IsZero() {
						t.Errorf("the row holds %+v, want node-a's capture left begun, its settle not taken, "+
							"and the standby withdrawn", now.Capture)
					}
				case now.Kept == nil || !now.Kept.samePlace(begun) || now.Kept.State != CaptureLanded ||
					now.Kept.Object != row.Object.Key.String() || now.Kept.Taker != takingProcess:
					t.Errorf("the row keeps %+v, want node-a's capture settled landed as the log's row names it",
						now.Kept)
				}
				if captureInFlight(now, clock.now()) {
					t.Errorf("a capture is still in flight once the standby stood down: %+v", now.Capture)
				}
				if left := rig.spools(t); len(left) > 0 {
					t.Errorf("spools were left behind: %v", left)
				}
			})
		})
	}
}

// failsGets is a run store whose reads of a row fail once armed, as a store
// that stopped answering reads does.
type failsGets struct {
	PendingStore
	armed atomic.Bool
}

func (s *failsGets) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.armed.Load() {
		return PendingRun{}, false, errNoLeader
	}
	return s.PendingStore.Get(ctx, turnID)
}

// A STANDBY WHOSE ROW WENT UNREAD WAITS AT ITS OWN BOUND: a read whose
// reservation another read beat, and which then cannot read the row again,
// stands by at its own place, its wait held to its own take's bound, the text
// bound in a gated fleet, never the kept bound's longer wait, which would
// outlive a version-1 reader's turn end.
//
// Mutation: default the stood-by capture's bound to the kept bound, and it
// goes red.
func TestAStandbyWhoseRowWentUnreadWaitsAtItsOwnBound(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := ownMemory(t, rig, rig.launchKept(t, "t1"))
		store := &failsGets{PendingStore: rig.coordinator.pending}
		rig.coordinator.pending = store
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
		held := newHeldArchive(placeHolderArchive)
		placed := holdingPlace(t, rig, run, held)
		store.armed.Store(true)
		rig.now = rig.now.Add(time.Second)
		close(proceed)
		standby := <-second
		store.armed.Store(false)
		if standby == nil || !standby.standingBy() {
			t.Fatalf("the read whose row went unread spooled %+v, want it standing by", standby)
		}
		if standby.stoodBy != atTextBound {
			t.Errorf("the standby waits at %+v, want its own take's text bound", standby.stoodBy)
		}
		held.end <- errScrubRefused
		<-placed
		standby.remove()
	})
}
