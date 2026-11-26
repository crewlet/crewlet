package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
)

// savingRig is a keeping rig whose node takes saves: its taker lease held in
// takers, its seats' save ledger in ledger, and a runner whose harness saves.
// Only in a bubble ([keepingRig]).
//
//fixedwait:bubble
func savingRig(t *testing.T, ledger SaveLedgers, takers *takerStore) (*coordRig, *sessionWrites, *sessionKeep) {
	t.Helper()
	rig, writes, keep := keepingRigWith(t, func(o *CoordinatorOptions) {
		taking(thisProcess, takers)(o)
		o.Saves = ledger
	})
	rig.runner.SavesSessions = true
	return rig, writes, keep
}

// saveOf runs one save task of turnID's job ([dueSave]) under the
// coordinator's own lifetime ([saveUnder]).
func saveOf(rig *coordRig, turnID string, edit func(*saveJob)) saveOutcome {
	return saveUnder(rig.coordinator.life, rig, dueSave(rig, turnID, edit))
}

// dueSave is a save of turnID's job as its spawner hands it: due now, never
// saved on this node, pacing from now, as edit leaves it.
func dueSave(rig *coordRig, turnID string, edit func(*saveJob)) saveJob {
	now := rig.coordinator.now()
	job := saveJob{run: rig.get(turnID), due: now, mark: -1, interval: 10 * time.Minute, last: now}
	if edit != nil {
		edit(&job)
	}
	return job
}

// saveUnder runs job's save task to its end as its spawner does, under offerer,
// the life of the waiter an offer came from, counted in the coordinator's
// inflight.
func saveUnder(offerer context.Context, rig *coordRig, job saveJob) saveOutcome {
	life, phase, endPhase := rig.coordinator.saveContexts(offerer, job.drainEnd)
	defer endPhase()
	rig.coordinator.inflight.Add(1)
	return rig.coordinator.saveTask(life, phase, job)
}

// ledgerOf is seat's save ledger in ledger as it stands, empty for none.
func ledgerOf(t *testing.T, ledger SaveLedgers, seat string) saveLedger {
	t.Helper()
	raw, _, err := ledger.SaveLedger(t.Context(), seat)
	if err != nil {
		t.Fatalf("reading %s's save ledger: %v", seat, err)
	}
	read, err := readSaveLedger(raw)
	if err != nil {
		t.Fatalf("decoding %s's save ledger: %v", seat, err)
	}
	return read
}

// writeLedger writes raw as seat's save ledger in ledger, over whatever is
// there.
func writeLedger(t *testing.T, ledger SaveLedgers, seat string, raw []byte) {
	t.Helper()
	_, revision, err := ledger.SaveLedger(t.Context(), seat)
	if err != nil {
		t.Fatalf("reading %s's save ledger: %v", seat, err)
	}
	if wrote, err := ledger.SwapSaveLedger(t.Context(), seat, raw, revision); err != nil || wrote == 0 {
		t.Fatalf("writing %s's save ledger = %d, %v", seat, wrote, err)
	}
}

// ledgerEntries is a ledger of n entries of size bytes each, counted until until.
func ledgerEntries(t *testing.T, n int, size int64, until time.Time) []byte {
	t.Helper()
	ledger := saveLedger{V: saveLedgerVersion}
	for range n {
		ledger.Entries = append(ledger.Entries, savedEntry{ID: uuid.New(), Until: until, Bytes: size})
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		t.Fatalf("encoding the ledger: %v", err)
	}
	return raw
}

// swapSpy is a save ledger that runs swapping, where set, before each swap,
// and loses the answer of the first swap the store took, where loseFirst.
type swapSpy struct {
	SaveLedgers
	mu        sync.Mutex
	swapping  func(ctx context.Context)
	loseFirst bool
	lost      bool
}

func (l *swapSpy) SwapSaveLedger(ctx context.Context, seat string, value []byte,
	revision uint64) (uint64, error) {

	l.mu.Lock()
	swapping := l.swapping
	l.mu.Unlock()
	if swapping != nil {
		swapping(ctx)
	}
	wrote, err := l.SaveLedgers.SwapSaveLedger(ctx, seat, value, revision)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loseFirst && !l.lost && err == nil && wrote != 0 {
		l.lost = true
		return 0, errors.New("the store's answer never reached its asker")
	}
	return wrote, err
}

// THE SAVE LEDGER'S HOLD OUTLASTS THE COLLECTOR AND FITS THE UPLOADS BUCKET. An
// entry is written before its object is stored, which is at most the take's
// life after it; the collector deletes a superseded object within its grace
// and two of its passes; and the entry's end is read on another node's clock.
// The bucket ages a ledger out past the hold, never inside it.
//
// Mutation: drop the skew or a pass from the hold, or restate the collector's
// terms apart from it, or grow the hold past the bucket's age.
func TestTheSaveLedgerHoldOutlastsTheCollectorAndFitsTheUploadsBucket(t *testing.T) {
	t.Parallel()
	if collectPendingGrace != collect.PendingGrace || collectInterval != collect.CollectInterval {
		t.Errorf("the hold restates the collector's grace %v and interval %v, and the collector's are %v and %v",
			collectPendingGrace, collectInterval, collect.PendingGrace, collect.CollectInterval)
	}
	want := takeLife + collect.PendingGrace + 2*collect.CollectInterval + 2*objstore.AssumedClockSkew
	if saveLedgerHold != want {
		t.Errorf("the save ledger's hold is %v, want the take's life, the collector's grace, two of its passes "+
			"and twice the assumed skew: %v", saveLedgerHold, want)
	}
	if saveLedgerHold >= coord.UploadRetention {
		t.Errorf("the save ledger's hold is %v, not under the uploads bucket's age of %v", saveLedgerHold,
			coord.UploadRetention)
	}
	if margin := coord.UploadRetention - saveLedgerHold; margin.Round(time.Second) != 14*time.Minute+30*time.Second {
		t.Errorf("the save ledger's hold is %v under the uploads bucket's age, and its doc states fourteen and a "+
			"half minutes: restate it", margin)
	}
}

// A SEAT'S SAVES STAY WITHIN ITS ALLOWANCE. Every save is counted against its
// seat before its stamp, on the size its packed archive seals at with a
// margin, and at its keep on what it uploads, in one ledger every node agrees
// on: two runs of one seat on two nodes share it; an entry counts until its
// end, read on this node's clock; the ledger holds at most its cap; a newer
// build's ledger is never rewritten; an append asked again counts once; and a
// spool sealing past its room is refused before its keep.
//
// Mutation: drop the room check, the end, the cap, the version check, the id
// check or the spool's room check, or charge the save after its reservation.
func TestASeatsSavesStayWithinItsAllowance(t *testing.T) {
	t.Parallel()
	counted := countedFor(14, textBound.sealed)
	// LONG PAST ITS PACE, so the charge alone decides: a room of half a save
	// paces one two holds out of its last attempt.
	longPast := func(j *saveJob) { j.last = j.last.Add(-3 * saveLedgerHold) }
	t.Run("two runs of one seat on two nodes share its room", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ledger := memory.NewFleet()
			first, _, firstKeep := savingRig(t, ledger, newTakerStore())
			second, _, secondKeep := savingRig(t, ledger, newTakerStore())
			// AN UPLOAD OF OVER HALF THE COUNT, so the first save's entry, cut
			// to what it uploads at its keep, still leaves the second no room.
			const size = 128 << 10
			charged := countedFor(size, textBound.sealed)
			for _, rig := range []*coordRig{first, second} {
				rig.runner.Session.Archive.Size, rig.runner.Session.Body = size, bytes.Repeat([]byte("x"), size)
			}
			firstKeep.policy.SeatBudget, secondKeep.policy.SeatBudget = charged*3/2, charged*3/2
			first.launchKept(t, "t1")
			second.launchKept(t, "t1")
			if out := saveOf(first, "t1", longPast); out.class != saveLanded || out.counted != charged {
				t.Fatalf("the first node's save came to %+v, want it landed and counted at %d", out, charged)
			}
			out := saveOf(second, "t1", longPast)
			if out.class != savePaced {
				t.Errorf("the second node's save came to %+v, want it paced: the seat's room is spent", out)
			}
			if row := second.get("t1"); row.Capture != nil {
				t.Errorf("the second node's run holds %+v, want nothing reserved", row.Capture)
			}
			if released := second.runner.Released(); len(released) != 1 {
				t.Errorf("the second node released %v, want its one pack released", released)
			}
			if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != 1 || got.Entries[0].Bytes != size {
				t.Errorf("the seat's ledger holds %+v, want the first save alone, at what it uploaded, %d",
					got.Entries, size)
			}
		})
	})
	t.Run("an entry counts until its end", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ledger := memory.NewFleet()
			rig, _, keep := savingRig(t, ledger, newTakerStore())
			keep.policy.SeatBudget = counted * 3 / 2
			rig.launchKept(t, "t1")
			writeLedger(t, ledger, "swe", ledgerEntries(t, 1, counted, rig.coordinator.now().Add(time.Minute)))
			if out := saveOf(rig, "t1", longPast); out.class != savePaced {
				t.Errorf("beside an entry another node's clock ends a minute ahead, the save came to %+v, want "+
					"it paced", out)
			}
			writeLedger(t, ledger, "swe", ledgerEntries(t, 1, counted, rig.coordinator.now().Add(-time.Minute)))
			if out := saveOf(rig, "t1", longPast); out.class != saveLanded {
				t.Fatalf("beside an entry ended a minute ago, the save came to %+v, want it landed", out)
			}
			if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != 1 ||
				!got.Entries[0].Until.Equal(rig.coordinator.now().Add(saveLedgerHold)) {
				t.Errorf("the seat's ledger holds %+v, want the lapsed entry gone and the save's held for %v",
					got.Entries, saveLedgerHold)
			}
		})
	})
	t.Run("the ledger holds at most its cap", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ledger := memory.NewFleet()
			rig, _, _ := savingRig(t, ledger, newTakerStore())
			rig.launchKept(t, "t1")
			writeLedger(t, ledger, "swe", ledgerEntries(t, saveLedgerCap, 1, rig.coordinator.now().Add(time.Hour)))
			if out := saveOf(rig, "t1", nil); out.class != savePaced {
				t.Errorf("beside %d entries, the save came to %+v, want it paced", saveLedgerCap, out)
			}
			writeLedger(t, ledger, "swe", ledgerEntries(t, saveLedgerCap-1, 1, rig.coordinator.now().Add(time.Hour)))
			if out := saveOf(rig, "t1", nil); out.class != saveLanded {
				t.Errorf("beside %d entries, the save came to %+v, want it landed", saveLedgerCap-1, out)
			}
			if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != saveLedgerCap {
				t.Errorf("the seat's ledger holds %d entries, want its cap, %d", len(got.Entries), saveLedgerCap)
			}
		})
	})
	t.Run("a newer build's ledger takes no save and is never rewritten", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ledger := memory.NewFleet()
			rig, _, _ := savingRig(t, ledger, newTakerStore())
			rig.launchKept(t, "t1")
			newer := []byte(`{"v":2,"entries":[],"held":"by a newer build"}`)
			writeLedger(t, ledger, "swe", newer)
			if out := saveOf(rig, "t1", nil); out.class != saveNotYet {
				t.Errorf("beside a newer build's ledger, the save came to %+v, want it not yet", out)
			}
			if raw, _, err := ledger.SaveLedger(t.Context(), "swe"); err != nil || !bytes.Equal(raw, newer) {
				t.Errorf("the seat's ledger reads %s, %v; want the newer build's, untouched", raw, err)
			}
			if takes := rig.runner.Takes(); len(takes) != 0 || rig.get("t1").Capture != nil {
				t.Errorf("took %d sessions and reserved %+v, want nothing", len(takes), rig.get("t1").Capture)
			}
		})
	})
	t.Run("the charge comes before the reservation", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			spy := &swapSpy{SaveLedgers: memory.NewFleet()}
			rig, _, _ := savingRig(t, spy, newTakerStore())
			rig.launchKept(t, "t1")
			var reserved []bool
			spy.swapping = func(ctx context.Context) {
				reserved = append(reserved, rig.getIn(ctx, "t1").Capture != nil)
			}
			// THE CHARGE, then its amend at the keep to what the save uploads.
			if out := saveOf(rig, "t1", nil); out.class != saveLanded || len(reserved) != 2 || reserved[0] ||
				!reserved[1] {
				t.Errorf("the save came to %+v, its ledger swapped with a place reserved %v, want it charged "+
					"before its reservation and amended after", out, reserved)
			}
		})
	})
	t.Run("a charge asked again counts once", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			spy := &swapSpy{SaveLedgers: memory.NewFleet(), loseFirst: true}
			rig, _, _ := savingRig(t, spy, newTakerStore())
			rig.launchKept(t, "t1")
			if out := saveOf(rig, "t1", nil); out.class != saveLanded {
				t.Errorf("the save came to %+v, want it landed", out)
			}
			if !spy.lost {
				t.Fatal("the premise: the first charge's answer was never lost")
			}
			if got := ledgerOf(t, spy, "swe"); len(got.Entries) != 1 {
				t.Errorf("the seat's ledger holds %+v, want the save counted once", got.Entries)
			}
		})
	})
	t.Run("a spool sealed past its room is refused before its keep", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.runner.Session.Archive.Size = 1
			rig.runner.Session.Body = bytes.Repeat([]byte("x"), 4*saveMarginFloor)
			rig.launchKept(t, "t1")
			if out := saveOf(rig, "t1", nil); out.class != saveFailed {
				t.Errorf("the save came to %+v, want it failed", out)
			}
			if uploads := keep.uploads(); len(uploads) != 0 {
				t.Errorf("uploaded %d sessions, want none", len(uploads))
			}
			if row := rig.get("t1"); row.Capture == nil || row.Capture.State != CaptureRefused ||
				!strings.Contains(row.Capture.Reason, "spools' room held") {
				t.Errorf("the run's newest capture is %+v, want the save refused past its room", row.Capture)
			}
			if left := rig.spools(t); len(left) != 0 {
				t.Errorf("left the spools %v, want none", left)
			}
		})
	})
}

// A SEAT'S SAVES ARE PACED BY SIZE AS ITS ALLOWANCE RUNS SHORT: a periodic save
// is due once the seat's room, spent at the session's expected size once per
// hold, lasts the interval, at last + hold * expected / room, or within an
// hour, when the room is read again. The expected size is this launch's
// newest landed save's, else its last counted save's, else the kept capture
// the launch continued, never another launch's. A drain save is never paced.
//
// Mutation: ignore the pacing, read the expected size off any launch's kept
// capture, or pace a drain save.
func TestASeatsSavesArePacedBySizeAsItsAllowanceRunsShort(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	own := PendingRun{LaunchID: "job-2", LaunchSeq: 2}
	landed := own
	landed.Kept = &SessionCapture{Launch: "job-2", LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded,
		ObjectSize: 1 << 20}
	other := own
	other.Kept = &SessionCapture{Launch: "job-1", LaunchSeq: 1, CaptureSeq: 4, State: CaptureLanded,
		Through: new(7), ObjectSize: 32 << 20}
	continued := other
	continued.Launch.Continued = &ContinuedPoint{LaunchSeq: 1, Through: 7}
	pastIt := other
	pastIt.Launch.Continued = &ContinuedPoint{LaunchSeq: 1, Through: 5}
	at := saveJob{interval: 10 * time.Minute, last: now.Add(-10 * time.Minute)}
	counted := at
	counted.counted = 1 << 20
	for _, c := range []struct {
		name  string
		row   PendingRun
		job   saveJob
		room  int64
		until time.Time // zero: not paced
	}{
		{name: "this launch's landed save", row: landed, job: at, room: 64 << 20,
			until: at.last.Add(saveLedgerHold / 64)},
		{name: "this launch's last counted save", row: own, job: counted, room: 64 << 20,
			until: at.last.Add(saveLedgerHold / 64)},
		{name: "only another launch's capture", row: other, job: at, room: 1 << 20},
		{name: "the kept capture this launch continued", row: continued, job: at, room: 64 << 20,
			until: now.Add(saveRetryCeiling)},
		{name: "a kept capture past the one this launch continued", row: pastIt, job: at, room: 1 << 20},
		{name: "this launch's counted save over the one it continued", row: continued, job: counted,
			room: 64 << 20, until: at.last.Add(saveLedgerHold / 64)},
		{name: "a stretch past the hour", row: landed, job: at, room: 8 << 20, until: now.Add(saveRetryCeiling)},
		{name: "a stretch already waited", row: landed, job: saveJob{interval: at.interval,
			last: now.Add(-time.Hour)}, room: 64 << 20},
		{name: "a stretch inside the interval", row: landed, job: at, room: 1 << 30},
		{name: "no room", row: own, job: at, room: 0, until: now.Add(saveRetryCeiling)},
	} {
		until, paced := paceAt(expectedSave(c.row, c.job), c.job, c.room, now)
		switch {
		case c.until.IsZero() && paced:
			t.Errorf("%s: paced until %v, want the save due", c.name, until)
		case !c.until.IsZero() && (!paced || !until.Equal(c.until)):
			t.Errorf("%s: paced %v until %v, want it paced until %v", c.name, paced, until, c.until)
		}
	}
	inBubble(t, func(t *testing.T) {
		rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
		keep.policy.SeatBudget = 8 << 20
		rig.launchKept(t, "t1")
		periodic := func(j *saveJob) { j.counted = 1 << 20 }
		if out := saveOf(rig, "t1", periodic); out.class != savePaced || out.pacedUntil.IsZero() {
			t.Errorf("the periodic save came to %+v, want it paced", out)
		}
		if takes := rig.runner.Takes(); len(takes) != 0 {
			t.Errorf("the paced save took %d sessions, want none", len(takes))
		}
		drain := func(j *saveJob) {
			periodic(j)
			j.drainEnd = time.Now().Add(time.Minute)
		}
		if out := saveOf(rig, "t1", drain); out.class != saveLanded {
			t.Errorf("the drain save came to %+v, want it landed: a drain save is not paced", out)
		}
	})
}

// memoIn is a copy of this node's memory of turnID's saves, nil for none.
func memoIn(rig *coordRig, turnID string) *saveMemo {
	rig.coordinator.saves.mu.Lock()
	defer rig.coordinator.saves.mu.Unlock()
	memo := rig.coordinator.saves.memos[turnID]
	if memo == nil {
		return nil
	}
	held := *memo
	return &held
}

// A LAUNCH'S FIRST SAVE IS PACED BY THE SESSION IT CONTINUED: before any save
// of its own, a launch whose row says it continued the turn's kept capture
// expects that capture's size, so its first save is held, unpacked, to its
// start plus the hold times that size over the seat's room, not packed and
// charged at its interval.
//
// Mutation: drop the continued capture from the expected size, and the save
// is packed and lands at its interval.
func TestALaunchsFirstSaveIsPacedByTheSessionItContinued(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ledger := memory.NewFleet()
		rig, _, keep := savingRig(t, ledger, newTakerStore())
		const size = 2 << 20
		continuingJob(t, rig, func(run *PendingRun) {
			run.Kept = &SessionCapture{Launch: "job-1", LaunchSeq: 1, CaptureSeq: 3, State: CaptureLanded,
				Interrupted: true, Through: new(7), ObjectSize: size}
		})
		packs := 0
		rig.runner.Packing = func(context.Context, Sandbox) { packs++ }
		launched := rig.now
		if offerOf(t, rig, "t1") {
			t.Fatal("the premise: a save was started at the launch, before its interval")
		}
		rig.now = launched.Add(rigSaveInterval)
		if !offerOf(t, rig, "t1") {
			t.Fatal("the premise: no save was started at the launch's interval")
		}
		want := launched.Add(saveLedgerHold / time.Duration(keep.policy.SeatBudget/size))
		if memo := memoIn(rig, "t1"); memo == nil || !memo.pacedUntil.Equal(want) {
			t.Errorf("the first save's memory is %+v, want it paced until %v", memo, want)
		}
		if packs != 0 {
			t.Errorf("the first save packed %d times, want none: the session it continued paces it", packs)
		}
		if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != 0 {
			t.Errorf("the seat's ledger holds %+v, want nothing charged", got.Entries)
		}
	})
}

// A FIRST SAVE THAT PACKS PAST ITS PACE IS RELEASED UNCHARGED: a launch whose
// row sizes no session it continued packs its first save at its interval, and
// a pack too large for the seat's room to last the interval at is released
// from its box, charged nothing, spooled nothing, and paced at the size it
// measured, from the save's last attempt.
//
// Mutation: drop the pace after the pack, and the save is charged and lands.
func TestAFirstSaveThatPacksPastItsPaceIsReleasedUncharged(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ledger := memory.NewFleet()
		rig, _, _ := savingRig(t, ledger, newTakerStore())
		run := rig.launchKept(t, "t1")
		const size = 24 << 20
		rig.runner.Session.Archive.Size = size
		out := saveOf(rig, "t1", nil)
		if out.class != savePaced || !out.pacedUntil.Equal(rig.now.Add(saveRetryCeiling)) ||
			out.counted != countedFor(size, textBound.sealed) {
			t.Errorf("the save came to %+v, want it paced an hour out and counted at %d", out,
				countedFor(size, textBound.sealed))
		}
		released, left := rig.runner.Released(), rig.runner.SavesLeft(rig.provider.Box(run.SandboxID))
		if len(released) != 1 || len(left) != 0 {
			t.Errorf("released %v and left %v in the box, want the one pack released", released, left)
		}
		if takes := rig.runner.Takes(); len(takes) != 0 {
			t.Errorf("the paced save took %d sessions, want none", len(takes))
		}
		if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != 0 {
			t.Errorf("the seat's ledger holds %+v, want nothing charged", got.Entries)
		}
		if used := roomInUse(rig.coordinator); used != 0 {
			t.Errorf("%d bytes of the spool room are still held", used)
		}
		if row := rig.get("t1"); row.Capture != nil {
			t.Errorf("the run's newest capture is %+v, want none placed", row.Capture)
		}
	})
}

// A PACED FIRST SAVE REMEMBERS WHAT IT MEASURED: once a first save's pack is
// paced, each hour that caps its wait brings no second pack, since the next
// save is paced before its pack at the size the first measured.
//
// Mutation: drop the measured size from the paced outcome, and the session is
// packed again every hour.
func TestAPacedFirstSaveRemembersWhatItMeasured(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.launchKept(t, "t1")
		const size = 24 << 20
		rig.runner.Session.Archive.Size = size
		packs := 0
		rig.runner.Packing = func(context.Context, Sandbox) { packs++ }
		launched := rig.now
		offerOf(t, rig, "t1")
		rig.now = launched.Add(rigSaveInterval)
		if !offerOf(t, rig, "t1") || packs != 1 {
			t.Fatalf("the premise: at its interval the first save packed %d times, want once", packs)
		}
		if memo := memoIn(rig, "t1"); memo == nil || memo.counted != countedFor(size, textBound.sealed) {
			t.Errorf("the paced save's memory is %+v, want it counted at %d", memo,
				countedFor(size, textBound.sealed))
		}
		// THE POLL'S CADENCE, which keeps the memory: an offer every interval.
		started, end := 0, rig.now.Add(3*time.Hour)
		for rig.now.Before(end) {
			rig.now = rig.now.Add(rigSaveInterval)
			if offerOf(t, rig, "t1") {
				started++
			}
		}
		if started < 3 {
			t.Fatalf("the premise: %d saves were started in three hours, want one an hour", started)
		}
		if packs != 1 {
			t.Errorf("the session was packed %d times in three hours past its paced first save, want once", packs)
		}
		if memo := memoIn(rig, "t1"); !memo.pacedUntil.After(rig.now) {
			t.Errorf("the save's memory is %+v, want it still paced past %v", memo, rig.now)
		}
	})
}

// A DRAIN SAVE IS NEVER PACED AFTER ITS PACK: a draining node saves what its
// box holds, so a first save whose pack a periodic save would pace is charged
// at its size and lands.
//
// Mutation: pace a drain save after its pack, and it is released uncharged.
func TestADrainSaveIsNeverPacedAfterItsPack(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ledger := memory.NewFleet()
		rig, _, keep := savingRig(t, ledger, newTakerStore())
		rig.launchKept(t, "t1")
		const size = 24 << 20
		rig.runner.Session.Archive.Size = size
		drain := func(j *saveJob) { j.drainEnd = time.Now().Add(time.Minute) }
		counted := countedFor(size, textBound.sealed)
		if out := saveOf(rig, "t1", drain); out.class != saveLanded || out.counted != counted {
			t.Errorf("the drain save came to %+v, want it landed and counted at %d", out, counted)
		}
		if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != 1 ||
			got.Entries[0].Bytes != int64(len(keep.uploads()[0])) {
			t.Errorf("the seat's ledger holds %+v, want the drain save charged at what it uploaded", got.Entries)
		}
	})
}

// A SAVE AFTER AN UNRESOLVED SAVE ASKS IT AGAIN FIRST, under the save's own
// operation, before any pack: a save of the turn whose record went unanswered
// may still land, so a save asked beside it is refused, and the box is spared
// the pack. Answered landed, the turn's next save is taken in the place after.
//
// Mutation: skip the ask, and the unanswered case asks nothing and the
// answered one takes no save.
func TestASaveAfterAnUnresolvedSaveAsksItAgainFirst(t *testing.T) {
	t.Parallel()
	for _, answered := range []bool{false, true} {
		t.Run(map[bool]string{false: "still unanswered", true: "answered"}[answered], func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := savingRig(t, memory.NewFleet(), newTakerStore())
				run := rig.launchKept(t, "t1")
				save := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
					State: CaptureBegun, Interrupted: true, Through: new(0), At: rig.now, Harness: "claude-code",
					HarnessVersion: "2.1.310", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat}
				if took, err := rig.pending.ReserveSave(t.Context(), "t1", save); err != nil || !took {
					t.Fatalf("reserving the earlier save = %v, %v", took, err)
				}
				stampOut(t, rig, "t1", save, CaptureUnresolved)
				if !answered {
					writes.answer(errors.New("the log said nothing"))
				}
				out := saveOf(rig, "t1", nil)
				asked := 0
				for _, c := range writes.captured() {
					if c.Stamp.CaptureSeq == 1 {
						asked++
					}
				}
				if asked == 0 {
					t.Errorf("the unresolved save was asked again %d times, want it asked", asked)
				}
				if !answered {
					if out.class != saveNotYet || len(rig.runner.Takes()) != 0 {
						t.Errorf("beside a save still unanswered, came to %+v having taken %d, want not yet "+
							"and no pack", out, len(rig.runner.Takes()))
					}
					return
				}
				if row := rig.get("t1"); out.class != saveLanded || row.Kept == nil || row.Kept.CaptureSeq != 2 {
					t.Errorf("after the earlier save landed, came to %+v keeping %+v, want the save in place 2 "+
						"landed", out, row.Kept)
				}
			})
		})
	}
}

// A SAVE AFTER ITS TAKER DIED SETTLES THAT TAKER'S SAVE: a save begun under a
// taker lease that is gone (an engine that died mid-save, its successor on the
// node polling the box), which the log names no record of, is settled refused
// off the turn's row before any pack, and the save is then taken in the place
// after it.
//
// Mutation: skip the settle, and the begun save holds the license and no save
// is taken.
func TestASaveAfterItsTakerDiedSettlesThatTakersSave(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		takers := newTakerStore()
		rig, _, _ := savingRig(t, memory.NewFleet(), takers)
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}, noRowFound: true}
		run := rig.launchKept(t, "t1")
		lease := claimTaker(t, takers, takingProcess)
		save := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID, State: CaptureBegun,
			Interrupted: true, Through: new(0), At: rig.now, Taker: takingProcess, TakerEpoch: lease.Epoch,
			Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat}
		if took, err := rig.pending.ReserveSave(t.Context(), "t1", save); err != nil || !took {
			t.Fatalf("reserving the dead taker's save = %v, %v", took, err)
		}
		if gave, err := takers.Backend.Release(t.Context(), coord.TakerResource(takingProcess), takingProcess,
			lease.Epoch); err != nil || !gave {
			t.Fatalf("giving the dead taker's lease back = %v, %v", gave, err)
		}
		out := saveOf(rig, "t1", nil)
		row := rig.get("t1")
		if out.class != saveLanded || row.Kept == nil || row.Kept.CaptureSeq != 2 {
			t.Fatalf("the save came to %+v keeping %+v, want it landed in place 2", out, row.Kept)
		}
		if row.Saving != nil && row.Saving.CaptureSeq == 1 && row.Saving.State == CaptureBegun {
			t.Errorf("the dead taker's save still stands begun: %+v", row.Saving)
		}
	})
}

// A SAVE IS SCRUBBED OF EVERY SECRET ITS BOX WAS HANDED: its take is handed
// every value its launch handed the box, as every capture's is.
//
// Mutation: hand the take no scrub values.
func TestASaveIsScrubbedOfEverySecretItsBoxWasHanded(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.launchKept(t, "t1")
		if out := saveOf(rig, "t1", nil); out.class != saveLanded {
			t.Fatalf("the save came to %+v, want it landed", out)
		}
		takes := rig.runner.Takes()
		if len(takes) != 1 || takes[0].Save == "" || takes[0].Pack {
			t.Fatalf("the runner was asked %+v, want one take of the save", takes)
		}
		if !slices.Contains(takes[0].Scrub, handedToken) {
			t.Errorf("the take scrubs %q, want %q among them", takes[0].Scrub, handedToken)
		}
	})
}

// A SAVE TAKEN AFTER A RENEWAL HOLDS NO RENEWED VALUE: the scrub's values are
// read after the pack, so a token the poll recorded while the pack ran, before
// it wrote it into the box, is handed to the take.
//
// Mutation: read the scrub's values before the pack.
func TestASaveTakenAfterARenewalHoldsNoRenewedValue(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		run := rig.launchKept(t, "t1")
		const renewed = "a-token-the-poll-renewed-during-the-pack"
		rig.runner.Packing = func(ctx context.Context, _ Sandbox) {
			if err := rig.coordinator.RecordHanded(context.WithoutCancel(ctx), run, renewed,
				rig.now.Add(25*time.Minute)); err != nil {
				t.Errorf("RecordHanded: %v", err)
			}
		}
		if out := saveOf(rig, "t1", nil); out.class != saveLanded {
			t.Fatalf("the save came to %+v, want it landed", out)
		}
		if takes := rig.runner.Takes(); len(takes) != 1 || !slices.Contains(takes[0].Scrub, renewed) {
			t.Errorf("the takes were %+v, want one scrubbing %q", takes, renewed)
		}
	})
}

// ITS WAITER'S STOP CANCELS A SAVE'S PACK AND NEVER ITS TAKE: the box phase is
// the offering waiter's, so its stop during the pack stamps nothing, charges
// nothing and leaves no save in the box; past the pack the save is the
// coordinator's, and one after it is reserved, spooled and landed.
//
// Mutation: run the box phase off the offerer, or carry the offerer past the
// pack.
func TestItsWaitersStopCancelsASavesPackAndNeverItsTake(t *testing.T) {
	t.Parallel()
	t.Run("during the pack", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ledger := memory.NewFleet()
			rig, _, _ := savingRig(t, ledger, newTakerStore())
			run := rig.launchKept(t, "t1")
			offerer, stop := context.WithCancel(t.Context())
			rig.runner.Packing = func(context.Context, Sandbox) {
				stop()
				synctest.Wait()
			}
			out := saveUnder(offerer, rig, dueSave(rig, "t1", nil))
			if out.class != saveStoodDown {
				t.Errorf("the save came to %+v, want it stood down", out)
			}
			if row := rig.get("t1"); row.Capture != nil {
				t.Errorf("the run's row holds %+v, want nothing reserved", row.Capture)
			}
			if left := rig.runner.SavesLeft(rig.provider.Box(run.SandboxID)); len(left) != 0 {
				t.Errorf("left %v in the box, want no save", left)
			}
			if got := ledgerOf(t, ledger, "swe"); len(got.Entries) != 0 {
				t.Errorf("the seat's ledger holds %+v, want nothing charged", got.Entries)
			}
		})
	})
	t.Run("after the pack", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			spy := &swapSpy{SaveLedgers: memory.NewFleet()}
			rig, _, _ := savingRig(t, spy, newTakerStore())
			rig.launchKept(t, "t1")
			offerer, stop := context.WithCancel(t.Context())
			spy.swapping = func(context.Context) {
				stop()
				synctest.Wait()
			}
			rig.runner.Session.Taking = func(ctx context.Context) {
				if err := ctx.Err(); err != nil {
					t.Errorf("the save was taken on a context already done (%v), want the coordinator's", err)
				}
			}
			out := saveUnder(offerer, rig, dueSave(rig, "t1", nil))
			if row := rig.get("t1"); out.class != saveLanded || row.Kept == nil {
				t.Errorf("the save came to %+v keeping %+v, want it landed", out, row.Kept)
			}
		})
	})
}

// A SAVE IS CUT OFF AND PLACED AT ITS LAUNCH'S NEWEST SAVE POINT: one read of
// the run's row before the pack places it, raised after its take to the point
// of this launch's newest landed save where both name one newest compaction,
// or zero, never at another launch's; it is stamped cut off, not final,
// holding no note; and a capture placed during its pack refuses it.
//
// Mutation: stamp no point or a fixed one, read the point off any launch's
// kept capture, or place the save off a row read after the pack.
func TestASaveIsCutOffAndPlacedAtItsLaunchsNewestSavePoint(t *testing.T) {
	t.Parallel()
	saved := func(rig *coordRig, place int64, through int) SessionCapture {
		run := rig.get("t1")
		return SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: place, Launch: run.LaunchID,
			State: CaptureBegun, Interrupted: true, Through: new(through), At: rig.now.Add(-time.Second),
			Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat}
	}
	cutOff := func(t *testing.T, newest *SessionCapture, place int64, through int) {
		t.Helper()
		switch {
		case newest == nil || newest.CaptureSeq != place || newest.State != CaptureLanded:
			t.Errorf("the run's newest capture is %+v, want the save landed in place %d", newest, place)
		case newest.Through == nil || *newest.Through != through:
			t.Errorf("the save is placed at %v, want %d", newest.Through, through)
		case !newest.Interrupted || newest.Final || newest.NoteCursor != 0:
			t.Errorf("the save is stamped interrupted %t, final %t, holding notes through %d; want it cut off, "+
				"not final, holding none", newest.Interrupted, newest.Final, newest.NoteCursor)
		}
	}
	t.Run("this launch's landed save", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			earlier := saved(rig, 1, 7)
			earlier.ResumedCount = &ResumedCount{Tokens: 1, Restart: new("u-1")}
			rig.runner.Session.Count = &ResumedCount{Tokens: 1, Restart: new("u-1")}
			rig.runner.Session.ResumeRead = true
			if took, err := rig.pending.ReserveSave(t.Context(), "t1", earlier); err != nil || !took {
				t.Fatalf("reserving the earlier save = %v, %v", took, err)
			}
			stampOut(t, rig, "t1", earlier, CaptureLanded)
			saveOf(rig, "t1", nil)
			cutOff(t, rig.get("t1").Capture, 2, 7)
		})
	})
	t.Run("another launch's kept capture", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			run := rig.launchKept(t, "t1")
			if _, _, err := rig.pending.mutateAny(t.Context(), "t1", func(row *PendingRun) bool {
				row.Kept = &SessionCapture{LaunchSeq: run.LaunchSeq - 1, CaptureSeq: 3, Launch: "an-earlier-job",
					State: CaptureLanded, Interrupted: true, Through: new(9), At: rig.now}
				return true
			}); err != nil {
				t.Fatalf("keeping another launch's capture: %v", err)
			}
			saveOf(rig, "t1", nil)
			cutOff(t, rig.get("t1").Capture, 1, 0)
		})
	})
	t.Run("a capture placed during the pack", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			rig.runner.Packing = func(ctx context.Context, _ Sandbox) {
				during := saved(rig, 1, 0)
				if took, err := rig.pending.ReserveSave(context.WithoutCancel(ctx), "t1", during); err != nil ||
					!took {
					t.Errorf("reserving the save placed during the pack = %v, %v", took, err)
				}
				stampOutIn(context.WithoutCancel(ctx), t, rig, "t1", during, CaptureLanded)
			}
			out := saveOf(rig, "t1", nil)
			if row := rig.get("t1"); out.class != saveNotYet || row.Capture == nil || row.Capture.CaptureSeq != 1 {
				t.Errorf("the save came to %+v with the newest capture %+v, want it not yet and the capture "+
					"placed during its pack the newest", out, row.Capture)
			}
		})
	})
}

// boundTake is a runner whose take first runs hold, as a scrub at its bound
// does, and fails with what hold answers.
type boundTake struct {
	*FakeRunner
	hold func(ctx context.Context) error
}

func (r boundTake) TakeSession(ctx context.Context, box Sandbox, take SessionTake) (TakenSession, error) {
	if err := r.hold(ctx); err != nil {
		return TakenSession{}, err
	}
	return r.FakeRunner.TakeSession(ctx, box, take)
}

// A SAVE IS TAKEN WHOLE AFTER A PACK AT ITS BOUND: its take context starts once
// the pack is done, so a pack that takes its whole wait leaves the take its
// whole budget, and a take at its bound still reserves, spools and lands.
//
// Mutation: start the take context before the pack, and the take runs out
// within its own budget.
func TestASaveIsTakenWholeAfterAPackAtItsBound(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.launchKept(t, "t1")
		// THE SCRUB AT ITS BOUND: the take's whole budget but a second.
		atItsBound := func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(SessionTakeBudget - time.Second):
				return nil
			}
		}
		manager, err := NewManager(ManagerOptions{
			Providers: map[Placement]Provider{Direct: rig.provider},
			Runners:   map[string]Runner{"claude-code": boundTake{FakeRunner: rig.runner, hold: atItsBound}},
		})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		rig.coordinator.SetManager(manager)
		rig.runner.Packing = func(context.Context, Sandbox) { time.Sleep(SessionPackWait) }
		if out := saveOf(rig, "t1", nil); out.class != saveLanded {
			t.Errorf("the save came to %+v, want it landed", out)
		}
	})
}

// floorLedger is a save ledger whose every swap first runs last, which the
// case sets to answer a second before the swap's room ends: the charge at its
// bound.
type floorLedger struct {
	SaveLedgers
	last func(ctx context.Context, read bool)
}

func (l *floorLedger) SwapSaveLedger(ctx context.Context, seat string, value []byte,
	revision uint64) (uint64, error) {

	l.last(ctx, false)
	return l.SaveLedgers.SwapSaveLedger(ctx, seat, value, revision)
}

// ReserveSave is [atTheFloor]'s reservation of a save: noted as a begun
// stamp asked, and answered a second before its room ends.
func (s *atTheFloor) ReserveSave(ctx context.Context, turnID string, save SessionCapture) (bool, error) {
	s.mark("asked begun %d", save.CaptureSeq)
	s.last(ctx, false)
	return s.PendingStore.ReserveSave(ctx, turnID, save)
}

// A SAVE'S OUTCOME LANDS WITHIN ITS LIFE: every step past the stamp's instant
// at its bound, the reservation, the take's spool, the digest, the keep and the
// outcome stamp come within [takeLife] of it, and the charge of the seat's
// ledger, at its own bound, comes before that instant, the place asked at it.
//
// Mutation: charge the ledger between the stamp's instant and its reservation.
func TestASavesOutcomeLandsWithinItsLife(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		last := func(ctx context.Context, read bool) {
			if deadline, ok := ctx.Deadline(); ok && (!read || time.Until(deadline) <= captureStampBudget) {
				time.Sleep(time.Until(deadline) - time.Second)
			}
		}
		rig, writes, keep := savingRig(t, &floorLedger{SaveLedgers: memory.NewFleet(), last: last},
			newTakerStore())
		rig.launchKept(t, "t1")
		// THE STAMP'S INSTANT ON THE CASE'S CLOCK, which the floor's notes read.
		rig.coordinator.now = time.Now
		store := &atTheFloor{PendingStore: rig.coordinator.pending, last: last, at: map[string]time.Time{}}
		rig.coordinator.pending = store
		// AT THE KEPT BOUND, whose keep is the longest a save lives by.
		keep.mu.Lock()
		keep.policy.MediaApart, keep.policy.KeptBound = true, true
		keep.mu.Unlock()
		rig.coordinator.keep = &floorKeep{sessionKeep: keep, last: last,
			upload: func() { time.Sleep(time.Duration(executorkeep.KeptPaceMiB)*objstore.MiBPace - time.Second) }}
		rig.coordinator.sessions = floorWrites{sessionWrites: writes, last: last}
		rig.coordinator.recordBudget = sessionRecordBudget
		var takeBy time.Time
		rig.runner.Session.Taking = func(ctx context.Context) { takeBy, _ = ctx.Deadline() }
		rig.runner.Session.Streaming = func(context.Context) { time.Sleep(time.Until(takeBy) - time.Second) }
		rig.runner.Session.Keeping = func(ctx context.Context) { last(ctx, false) }
		if out := saveOf(rig, "t1", nil); out.class != saveLanded {
			t.Fatalf("the save came to %+v, want it landed", out)
		}
		kept := rig.get("t1").Kept
		to, made := store.when("made %s %d", CaptureLanded, 1)
		if kept == nil || !made {
			t.Fatalf("the premise: the row keeps %+v, the landing stamped %t", kept, made)
		}
		if asked, ok := store.when("asked begun %d", 1); !ok || asked.Sub(kept.At) > 0 {
			t.Errorf("the save's place was asked %v after its instant (asked %t), want at once: nothing waits "+
				"between the two", asked.Sub(kept.At), ok)
		}
		// UNMADE, the keep's media steps (its count, its read again, and the
		// amend, read again and record of a carry gone), which a save naming
		// none never makes.
		unmade := 2*keepReadRoom + 2*MediaReadersRoom + sessionRecordBudget
		if life := to.Sub(kept.At); life > takeLife || life < takeLife-unmade-time.Minute {
			t.Errorf("the save's outcome came %v after its instant, want within a minute and its unmade "+
				"media steps, %v, of its life, %v", life, unmade, takeLife)
		}
	})
}

// A NEWER BUILD'S LEDGER IS LOGGED ONCE A LAUNCH: a seat whose save ledger a
// newer build owns takes no save, and its job's first offer says why at Warn;
// every offer after it says so at Debug alone.
//
// Mutation: log the newer build's ledger at Debug alone, or at Warn on every
// offer.
func TestANewerBuildsLedgerIsLoggedOnceALaunch(t *testing.T) {
	logs := captureLogs(t)
	inBubble(t, func(t *testing.T) {
		ledger := memory.NewFleet()
		rig, _, _ := savingRig(t, ledger, newTakerStore())
		rig.launchKept(t, "t1")
		writeLedger(t, ledger, "swe", []byte(`{"v":2,"entries":[]}`))
		for offer := range 2 {
			rig.now = rig.now.Add(rigSaveInterval)
			if !offerOf(t, rig, "t1") {
				t.Fatalf("the premise: offer %d of a due job started no save", offer+1)
			}
			// THE TASK LOGS once its offer's wait has ended.
			synctest.Wait()
		}
		warned := 0
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, "executor_session_not_kept") && strings.Contains(line, "newer build") {
				warned++
			}
		}
		if warned != 1 {
			t.Errorf("two offers beside a newer build's ledger logged it %d times at Warn, want once for the "+
				"launch", warned)
		}
	})
}
