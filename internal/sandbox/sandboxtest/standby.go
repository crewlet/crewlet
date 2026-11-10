package sandboxtest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A read standing by for another read's capture of the same job: the cases
// [Run] drives for it.
//
// EVERY ONE IS A PROPERTY OF A WRITE OF THE ROW. Two reads of one job at once —
// a stop's and a collection's — ask for one place, and the one the store does
// not give it to stands by for the other's capture rather than walking away
// from it: named on that capture's stamp (sandbox.PendingStore.StandBy),
// carried by every later stamp of the same take, and keeping its own session
// in the next place only where that capture did not land
// (sandbox.PendingStore.TakeOver), or withdrawn (sandbox.PendingStore.StandDown).
// Decided by the store on the row as it stands, or two reads would each decide
// off a copy that the other's write had already made stale.

// standbyCases are the standby cases, in the shape [Run] drives.
var standbyCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"AReadStandsByOnlyForACaptureOfItsJobThatHasNotLanded", testAReadStandsByOnlyForACaptureOfItsJobThatHasNotLanded},
	{"AStandbyStaysWithItsCaptureUntilANewerOneTakesItsPlace", testAStandbyStaysWithItsCaptureUntilANewerOneTakesItsPlace},
	{"AStandbyTakesTheNextPlaceOnlyAfterACaptureThatDidNotLand",
		testAStandbyTakesTheNextPlaceOnlyAfterACaptureThatDidNotLand},
	{"AStandbyStandsDownOnlyItself", testAStandbyStandsDownOnlyItself},
	{"AStandbysWritesAreHeldToACapturesPlaceAndBounds", testAStandbysWritesAreHeldToACapturesPlaceAndBounds},
}

// placeHolder is the first read's begun capture of r's job at (1, 1), stamped
// on r's row, which every case has another read stand by for.
func placeHolder(t *testing.T, s sandbox.PendingStore, r sandbox.PendingRun) sandbox.SessionCapture {
	t.Helper()
	launch := mustLaunched(t, s, r)
	begun := sandbox.SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: launch, State: sandbox.CaptureBegun,
		Final: true, At: base, Taker: "node-a:" + incarnationSuffix, TakerEpoch: 3, Harness: "claude-code",
		SessionID: "s-1"}
	if took, err := s.StampCapture(t.Context(), r.TurnID, begun); err != nil || !took {
		t.Fatalf("stamping the first read's capture = %v, %v", took, err)
	}
	return begun
}

// otherRead is another take of placed's job at its place, at an instant and
// under a taker lease of its own: what a read whose place was taken stands by
// with.
func otherRead(placed sandbox.SessionCapture, node string, later time.Duration) sandbox.SessionCapture {
	return sandbox.SessionCapture{LaunchSeq: placed.LaunchSeq, CaptureSeq: placed.CaptureSeq, Launch: placed.Launch,
		State: sandbox.CaptureBegun, Final: true, At: placed.At.Add(later), Taker: node + ":" + incarnationSuffix,
		TakerEpoch: 7, Harness: "claude-code", SessionID: "s-1"}
}

// standingBy reports whether the newest capture on turnID's row names take
// standing by for it.
func standingBy(t *testing.T, s sandbox.PendingStore, turnID string, take sandbox.SessionCapture) bool {
	t.Helper()
	got, found, err := s.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get %s = %v, %v", turnID, found, err)
	}
	newest := got.Capture
	return newest != nil && newest.StandbyAt.Equal(take.At) && newest.StandbyTaker == take.Taker &&
		newest.StandbyTakerEpoch == take.TakerEpoch && !take.At.IsZero()
}

// A READ STANDS BY ONLY FOR A CAPTURE OF ITS JOB THAT HAS NOT LANDED, AND ONE
// READ AT A TIME. Another take of the job at the capture's own place stands by
// for it while it is begun, and for one refused; asked again, it is answered as
// standing and nothing changes. No read stands by for a capture that landed —
// the session is kept — nor for its own take, a capture at another place,
// another job's, or one another read already stands by for.
//
// Mutation: let a read stand by for a capture that landed, for one another
// read stands by for, or for a capture at another place or of another job, and
// a case here takes the standby it must refuse.
func testAReadStandsByOnlyForACaptureOfItsJobThatHasNotLanded(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-standby-begun")
	placed := placeHolder(t, s, r)
	standby := otherRead(placed, "node-b", time.Second)
	for _, refused := range []struct {
		why  string
		take sandbox.SessionCapture
	}{
		{"the capture's own take", placed},
		{"a take at another place", func() sandbox.SessionCapture {
			other := standby
			other.CaptureSeq = 2
			return other
		}()},
		{"a take of another job", func() sandbox.SessionCapture {
			other := standby
			other.Launch = "another-job"
			return other
		}()},
	} {
		if stood, err := s.StandBy(ctx, r.TurnID, refused.take); err != nil || stood {
			t.Fatalf("%s stood by = %v, %v; want it refused", refused.why, stood, err)
		}
	}
	if stood, err := s.StandBy(ctx, r.TurnID, standby); err != nil || !stood {
		t.Fatalf("another read stood by for a begun capture of its job = %v, %v; want it standing", stood, err)
	}
	if !standingBy(t, s, r.TurnID, standby) {
		t.Fatal("the row names no read standing by for its capture")
	}
	if stood, err := s.StandBy(ctx, r.TurnID, standby); err != nil || !stood {
		t.Fatalf("the standby asked again = %v, %v; want it answered as standing", stood, err)
	}
	third := otherRead(placed, "node-c", 2*time.Second)
	if stood, err := s.StandBy(ctx, r.TurnID, third); err != nil || stood {
		t.Fatalf("a third read stood by beside the standby = %v, %v; want it refused", stood, err)
	}
	if !standingBy(t, s, r.TurnID, standby) {
		t.Fatal("a third read's ask moved the standby off the capture")
	}

	refusedRow := run("t-standby-refused")
	refusedPlace := placeHolder(t, s, refusedRow)
	refusedPlace.State, refusedPlace.Reason = sandbox.CaptureRefused, "the session could not be uploaded"
	if took, err := s.StampCapture(ctx, refusedRow.TurnID, refusedPlace); err != nil || !took {
		t.Fatalf("refusing the first read's capture = %v, %v", took, err)
	}
	if stood, err := s.StandBy(ctx, refusedRow.TurnID, otherRead(refusedPlace, "node-b", time.Second)); err != nil ||
		!stood {
		t.Fatalf("another read stood by for a refused capture of its job = %v, %v; want it standing", stood, err)
	}

	landedRow := run("t-standby-landed")
	landed := placeHolder(t, s, landedRow)
	landed.State, landed.Object, landed.ObjectHash, landed.ObjectSize = sandbox.CaptureLanded, "o-1", "h-1", 1
	if took, err := s.StampCapture(ctx, landedRow.TurnID, landed); err != nil || !took {
		t.Fatalf("landing the first read's capture = %v, %v", took, err)
	}
	if stood, err := s.StandBy(ctx, landedRow.TurnID, otherRead(landed, "node-b", time.Second)); err != nil || stood {
		t.Fatalf("another read stood by for a capture that landed = %v, %v; want it refused", stood, err)
	}
}

// A STANDBY STAYS WITH ITS CAPTURE UNTIL A NEWER ONE TAKES ITS PLACE. The read
// holding the place stamps what its take came to from what it holds, which
// never knew of the standby: that stamp moves the capture on and keeps the
// standby, so a capture refused is one the standby can still keep in the next
// place. A capture that lands is kept with no standby, and a newer capture
// holds the place with none.
//
// Mutation: write the place-holder's stamp as it came, and the standby is gone
// from a capture refused under it.
func testAStandbyStaysWithItsCaptureUntilANewerOneTakesItsPlace(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-standby-carried")
	placed := placeHolder(t, s, r)
	standby := otherRead(placed, "node-b", time.Second)
	if stood, err := s.StandBy(ctx, r.TurnID, standby); err != nil || !stood {
		t.Fatalf("StandBy = %v, %v", stood, err)
	}
	refused := placed
	refused.State, refused.Reason = sandbox.CaptureRefused, "the session's archive changed while it was read"
	if took, err := s.StampCapture(ctx, r.TurnID, refused); err != nil || !took {
		t.Fatalf("refusing the place-holder's capture = %v, %v", took, err)
	}
	if !standingBy(t, s, r.TurnID, standby) {
		t.Fatal("the place-holder's refusal took the standby off its capture")
	}
	newer := placed
	newer.CaptureSeq, newer.At = 2, placed.At.Add(time.Minute)
	if took, err := s.StampCapture(ctx, r.TurnID, newer); err != nil || !took {
		t.Fatalf("stamping a newer capture = %v, %v", took, err)
	}
	if got, _, _ := s.Get(ctx, r.TurnID); got.Capture == nil || !got.Capture.StandbyAt.IsZero() {
		t.Fatalf("the newer capture holds %+v, want no read standing by for it", got.Capture)
	}

	landedRow := run("t-standby-kept")
	landedPlace := placeHolder(t, s, landedRow)
	if stood, err := s.StandBy(ctx, landedRow.TurnID, otherRead(landedPlace, "node-b", time.Second)); err != nil ||
		!stood {
		t.Fatalf("StandBy = %v, %v", stood, err)
	}
	landed := landedPlace
	landed.State, landed.Object, landed.ObjectHash, landed.ObjectSize = sandbox.CaptureLanded, "o-1", "h-1", 1
	if took, err := s.StampCapture(ctx, landedRow.TurnID, landed); err != nil || !took {
		t.Fatalf("landing the place-holder's capture = %v, %v", took, err)
	}
	if got, _, _ := s.Get(ctx, landedRow.TurnID); got.Kept == nil || !got.Kept.StandbyAt.IsZero() {
		t.Fatalf("the row keeps %+v, want the landed capture kept with no standby", got.Kept)
	}
}

// A STANDBY TAKES THE NEXT PLACE ONLY AFTER A CAPTURE THAT DID NOT LAND. Where
// the capture it stands by for was refused, its own take is stamped in the
// place after it, and asked again is answered taken; a read that is not the
// standby, a standby withdrawn, and a capture that landed take nothing, so a
// landed session is never followed by another upload of the same one.
//
// Mutation: take the next place whatever the capture came to, or for any read,
// and a case here takes a place it must not.
func testAStandbyTakesTheNextPlaceOnlyAfterACaptureThatDidNotLand(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-standby-over")
	placed := placeHolder(t, s, r)
	standby := otherRead(placed, "node-b", time.Second)
	if stood, err := s.StandBy(ctx, r.TurnID, standby); err != nil || !stood {
		t.Fatalf("StandBy = %v, %v", stood, err)
	}
	refused := placed
	refused.State, refused.Reason = sandbox.CaptureRefused, "the session could not be uploaded"
	if took, err := s.StampCapture(ctx, r.TurnID, refused); err != nil || !took {
		t.Fatalf("refusing the place-holder's capture = %v, %v", took, err)
	}
	over := standby
	over.CaptureSeq, over.At = 2, standby.At.Add(time.Minute)
	stranger := otherRead(placed, "node-c", 2*time.Second)
	if took, err := s.TakeOver(ctx, r.TurnID, stranger, over); err != nil || took {
		t.Fatalf("a read that is not the standby took the next place = %v, %v; want it refused", took, err)
	}
	if took, err := s.TakeOver(ctx, r.TurnID, standby, over); err != nil || !took {
		t.Fatalf("the standby took the place after a refused capture = %v, %v; want it taken", took, err)
	}
	if got, _, _ := s.Get(ctx, r.TurnID); got.Capture == nil || got.Capture.CaptureSeq != 2 ||
		!got.Capture.At.Equal(over.At) || got.Capture.State != sandbox.CaptureBegun {
		t.Fatalf("the row's newest capture is %+v, want the standby's own, begun in the next place", got.Capture)
	}
	if took, err := s.TakeOver(ctx, r.TurnID, standby, over); err != nil || !took {
		t.Fatalf("the takeover asked again = %v, %v; want it answered taken", took, err)
	}

	withdrawnRow := run("t-standby-withdrawn")
	withdrawnPlace := placeHolder(t, s, withdrawnRow)
	withdrawn := otherRead(withdrawnPlace, "node-b", time.Second)
	if stood, err := s.StandBy(ctx, withdrawnRow.TurnID, withdrawn); err != nil || !stood {
		t.Fatalf("StandBy = %v, %v", stood, err)
	}
	if down, err := s.StandDown(ctx, withdrawnRow.TurnID, withdrawn); err != nil || !down {
		t.Fatalf("StandDown = %v, %v", down, err)
	}
	withdrawnOver := withdrawn
	withdrawnOver.CaptureSeq = 2
	if took, err := s.TakeOver(ctx, withdrawnRow.TurnID, withdrawn, withdrawnOver); err != nil || took {
		t.Fatalf("a standby withdrawn took the next place = %v, %v; want it refused", took, err)
	}

	landedRow := run("t-standby-landed-over")
	landedPlace := placeHolder(t, s, landedRow)
	landedStandby := otherRead(landedPlace, "node-b", time.Second)
	if stood, err := s.StandBy(ctx, landedRow.TurnID, landedStandby); err != nil || !stood {
		t.Fatalf("StandBy = %v, %v", stood, err)
	}
	landed := landedPlace
	landed.State, landed.Object, landed.ObjectHash, landed.ObjectSize = sandbox.CaptureLanded, "o-1", "h-1", 1
	if took, err := s.StampCapture(ctx, landedRow.TurnID, landed); err != nil || !took {
		t.Fatalf("landing the place-holder's capture = %v, %v", took, err)
	}
	landedOver := landedStandby
	landedOver.CaptureSeq = 2
	if took, err := s.TakeOver(ctx, landedRow.TurnID, landedStandby, landedOver); err != nil || took {
		t.Fatalf("the standby took the place after a capture that landed = %v, %v; want it refused", took, err)
	}
}

// A STANDBY STANDS DOWN ONLY ITSELF. Withdrawn by its own name it is gone from
// the capture, and asked again finds nothing to withdraw; a read that is not
// the standby withdraws nothing.
//
// Mutation: withdraw whatever read stands by, and another read's ask takes the
// standby off the capture.
func testAStandbyStandsDownOnlyItself(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-standby-down")
	placed := placeHolder(t, s, r)
	standby := otherRead(placed, "node-b", time.Second)
	if stood, err := s.StandBy(ctx, r.TurnID, standby); err != nil || !stood {
		t.Fatalf("StandBy = %v, %v", stood, err)
	}
	if down, err := s.StandDown(ctx, r.TurnID, otherRead(placed, "node-c", 2*time.Second)); err != nil || down {
		t.Fatalf("another read withdrew the standby = %v, %v; want nothing withdrawn", down, err)
	}
	if !standingBy(t, s, r.TurnID, standby) {
		t.Fatal("another read's withdrawal took the standby off the capture")
	}
	if down, err := s.StandDown(ctx, r.TurnID, standby); err != nil || !down {
		t.Fatalf("the standby withdrew itself = %v, %v; want it withdrawn", down, err)
	}
	if standingBy(t, s, r.TurnID, standby) {
		t.Fatal("the withdrawn standby still stands by for the capture")
	}
	if down, err := s.StandDown(ctx, r.TurnID, standby); err != nil || down {
		t.Fatalf("the withdrawal asked again = %v, %v; want nothing left to withdraw", down, err)
	}
}

// A STANDBY'S WRITES ARE HELD TO A CAPTURE'S PLACE AND BOUNDS, BEFORE ANYTHING IS
// WRITTEN. Standing by, taking the next place and standing down each carry a
// take's stamp to the row, and each refuses one placed nowhere, and one past
// the bounds the row holds a capture to — a taker of a kilobyte, which standing
// by writes onto the capture it stands by for — leaving the row's capture as it
// was; and a stamp that carries a standby is held to the same bound on its
// standby's taker.
//
// Mutation: drop the place or the bounds from any of the three writes, or the
// standby's taker from a capture's bounds, and a write refused here is made.
func testAStandbysWritesAreHeldToACapturesPlaceAndBounds(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-standby-bounds")
	placed := placeHolder(t, s, r)
	standby := otherRead(placed, "node-b", time.Second)
	unplaced := standby
	unplaced.CaptureSeq = 0
	kilobyte := standby
	kilobyte.Taker = strings.Repeat("n", 1<<10) + ":1"
	over := standby
	over.CaptureSeq, over.At = 2, standby.At.Add(time.Minute)
	overUnplaced, overKilobyte := over, over
	overUnplaced.LaunchSeq = 0
	overKilobyte.Taker = kilobyte.Taker
	for _, refused := range []struct {
		why   string
		write func() (bool, error)
	}{
		{"standing by placed nowhere", func() (bool, error) { return s.StandBy(ctx, r.TurnID, unplaced) }},
		{"standing by under a kilobyte taker", func() (bool, error) { return s.StandBy(ctx, r.TurnID, kilobyte) }},
		{"standing down placed nowhere", func() (bool, error) { return s.StandDown(ctx, r.TurnID, unplaced) }},
		{"taking over placed nowhere", func() (bool, error) {
			return s.TakeOver(ctx, r.TurnID, standby, overUnplaced)
		}},
		{"taking over under a kilobyte taker", func() (bool, error) {
			return s.TakeOver(ctx, r.TurnID, standby, overKilobyte)
		}},
	} {
		if took, err := refused.write(); err == nil || took {
			t.Errorf("%s = %v, %v; want it refused", refused.why, took, err)
		}
	}
	got, found, err := s.Get(ctx, r.TurnID)
	if err != nil || !found {
		t.Fatalf("Get %s = %v, %v", r.TurnID, found, err)
	}
	if c := got.Capture; c == nil || c.CaptureSeq != 1 || c.State != sandbox.CaptureBegun || !c.StandbyAt.IsZero() {
		t.Fatalf("the refused writes left the row's capture %+v, want the first read's begun with no standby", c)
	}
	carrying := placed
	carrying.State, carrying.Reason = sandbox.CaptureRefused, "the session could not be uploaded"
	carrying.StandbyTaker, carrying.StandbyTakerEpoch, carrying.StandbyAt = kilobyte.Taker, 7, standby.At
	if took, err := s.StampCapture(ctx, r.TurnID, carrying); !errors.Is(err, sandbox.ErrCaptureUnbounded) || took {
		t.Errorf("a stamp carrying a kilobyte standby taker = %v, %v; want ErrCaptureUnbounded", took, err)
	}
}
