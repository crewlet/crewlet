package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// roomInUse is how many bytes of c's spool room are held, as a taker finds
// it: the most it could take now, from the room's whole.
func roomInUse(c *Coordinator) int64 {
	free, lo, hi := int64(0), int64(0), spoolRoomBytes
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if c.spoolRoom.sem.TryAcquire(mid) {
			c.spoolRoom.sem.Release(mid)
			free, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	return spoolRoomBytes - free
}

// finalTake is a collection's take of run's job, ended, through runner.
func finalTake(t *testing.T, rig *coordRig, run PendingRun, runner Runner) (*spooledCapture, *SessionKept) {
	t.Helper()
	return rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID), runner,
		readCapture(BoxReadCollect, Result{}, true))
}

// FOUR SAVES AT THE SEALED BOUND LEAVE A FINAL TAKE ITS ROOM. Saves take a
// take's room ([takeRoomBytes]) and never wait for it, so with every one of a
// node's saves under way a final take of a session at its bound still spools
// at once, and a fifth save finds none.
//
// Mutation: size the room at four sessions at the bound, or at four and a
// mebibyte, and the final waits out its minute and keeps nothing.
func TestASaveLeavesRoomForOneFinalSessionAtTheBound(t *testing.T) {
	t.Parallel()
	if counted := countedFor(executorkeep.SessionMaxBytes); counted != executorkeep.SealedMaxBytes {
		t.Fatalf("a session at its bound is counted at %d, want the sealed bound %d", counted,
			executorkeep.SealedMaxBytes)
	}
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		rig.runner.mu.Lock()
		rig.runner.Session.Archive.Size = executorkeep.SessionMaxBytes
		rig.runner.mu.Unlock()
		for save := range maxConcurrentSaves {
			hold := rig.coordinator.trySpoolRoom(takeRoomBytes)
			if hold == nil {
				t.Fatalf("save %d of %d found no room at the sealed bound", save+1, maxConcurrentSaves)
			}
			defer hold.release()
		}
		began := time.Now()
		spooled, kept := finalTake(t, rig, run, rig.runner)
		if spooled == nil || kept == nil || kept.NotKept != "" {
			t.Fatalf("the final take beside %d saves came to %+v, want its session spooled",
				maxConcurrentSaves, kept)
		}
		if waited := time.Since(began); waited != 0 {
			t.Errorf("the final take waited %v for room beside every save, want none", waited)
		}
		if fifth := rig.coordinator.trySpoolRoom(takeRoomBytes); fifth != nil {
			fifth.release()
			t.Errorf("a save past the node's %d found room at the bound beside a final take",
				maxConcurrentSaves)
		}
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
	})
}

// A TAKE'S SCRUB SPOOLS IN WHAT ITS SEALED SPOOL LEAVES OF ITS ROOM: a final
// take holds a take's room and names its scrub's spool, in its process's own
// spool directory, the part of it past a sealed session at the bound, which
// the seal of a session at the bound keeps whole; and once the take is done
// its scrub's part is back, the room holding the sealed spool alone.
//
// Mutation: split the scrub's part off first, give it the take's whole room,
// leave the spool's members out of the room, or keep it past the take, and the
// part, the seal or the room goes red.
func TestATakesScrubSpoolsInWhatItsSealedSpoolLeaves(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		keep.gate = make(chan struct{})
		rig.runner.mu.Lock()
		rig.runner.Session.Archive.Size = executorkeep.SessionMaxBytes
		rig.runner.mu.Unlock()
		spooled, kept := finalTake(t, rig, run, rig.runner)
		if spooled == nil || kept == nil || kept.NotKept != "" {
			t.Fatalf("a take of a session at the bound came to %+v, want it spooled", kept)
		}
		takes := rig.runner.Takes()
		spool := takes[len(takes)-1].Spool
		dir, err := rig.coordinator.spoolHere()
		if err != nil {
			t.Fatal(err)
		}
		if want := takeRoomBytes - executorkeep.SealedMaxBytes; spool.Bytes != want || spool.Dir != dir {
			t.Errorf("the take named its scrub %d bytes in %s, want %d in the process's own %s", spool.Bytes,
				spool.Dir, want, dir)
		}
		if least := executorkeep.GzipMostBytes(executorkeep.SealedMaxBytes) +
			executorkeep.SpoolMembersMostBytes; spool.Bytes < least {
			t.Errorf("the take named its scrub %d bytes, short of the %d a kept session at the bound comes to "+
				"as gzip writes it, with every member", spool.Bytes, least)
		}
		if used := roomInUse(rig.coordinator); used != spooled.spool.Size {
			t.Errorf("once the take is done the room holds %d bytes, want its sealed spool's %d", used,
				spooled.spool.Size)
		}
		close(keep.gate)
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
	})
}

// A SAVE NEVER WAITS FOR SPOOL ROOM: where the room is full it ends at once,
// not yet, naming the room, before its pack, so it never queues ahead of a
// final take; once the room is back the next save lands.
//
// Mutation: wait for the room as a take does, and the save blocks the bubble.
func TestASaveNeverWaitsForSpoolRoom(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.launchKept(t, "t1")
		full := rig.coordinator.trySpoolRoom(spoolRoomBytes)
		began := time.Now()
		out := saveOf(rig, "t1", nil)
		if out.class != saveNotYet || out.unplaced != "spool" || !strings.Contains(out.why, "room") {
			t.Fatalf("a save with the room full came to %+v, want it not yet, the room named", out)
		}
		if waited := time.Since(began); waited != 0 {
			t.Errorf("the save waited %v for room, want none", waited)
		}
		rig.runner.mu.Lock()
		packs := rig.runner.packs
		rig.runner.mu.Unlock()
		if packs != 0 {
			t.Errorf("the save packed %d archives with no room to spool them, want none", packs)
		}
		if row := rig.get("t1"); row.Capture != nil {
			t.Errorf("the save stamped %+v with no room to spool, want nothing", row.Capture)
		}
		full.release()
		if out := saveOf(rig, "t1", nil); out.class != saveLanded {
			t.Fatalf("the save once the room was back came to %+v, want it landed", out)
		}
		if used := roomInUse(rig.coordinator); used != 0 {
			t.Errorf("the landed save left %d bytes of the room held", used)
		}
	})
}

// A FINAL TAKE WITH NO ROOM KEEPS NOTHING AND SAYS WHY: it waits a minute for
// room, then spools nothing and uploads nothing, stamps its capture refused for
// the room ([executorkeep.LossNoSpoolRoom]), and its result names the room, so
// the next launch continues the turn's newest kept capture and says why.
//
// Mutation: spool anyway once the wait ends, or stamp nothing, and a spool, an
// upload or the missing stamp shows below.
func TestAFinalTakeWithNoRoomIsNotKeptAndSaysWhy(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := keepingRig(t)
		run := rig.launchKept(t, "t1")
		full := rig.coordinator.trySpoolRoom(spoolRoomBytes)
		defer full.release()
		began := time.Now()
		spooled, kept := finalTake(t, rig, run, rig.runner)
		if spooled != nil {
			rig.coordinator.keepLater(t.Context(), spooled)
			rig.awaitKeeps()
			t.Fatalf("the take spooled its session with no room for it")
		}
		if kept == nil || !strings.Contains(kept.NotKept, "room") {
			t.Fatalf("the take's result is %+v, want it not kept, the room named", kept)
		}
		if waited := time.Since(began); waited != spoolRoomWait {
			t.Errorf("the take gave up after %v, want its wait for room, %v", waited, spoolRoomWait)
		}
		if row := rig.get("t1"); row.Capture == nil || row.Capture.State != CaptureRefused ||
			row.Capture.Loss != executorkeep.LossNoSpoolRoom {
			t.Errorf("the take stamped %+v on the run's row with no room to spool, want it refused for the room",
				row.Capture)
		}
		if uploads := keep.uploads(); len(uploads) != 0 {
			t.Errorf("uploaded %d sessions, want none", len(uploads))
		}
		if left := rig.spools(t); len(left) != 0 {
			t.Errorf("the take left spools %v", left)
		}
	})
}

// EVERY SPOOL GIVES ITS ROOM BACK WITH ITS FILE, and holds no more than its
// sealed size while it waits for its keep: a final take kept, one a stopped
// node drops, a save landed, a save whose seal passes what its seat's ledger
// counted, a standby that takes the next place and one a stopped node drops.
// No seal writes past the room its spool holds.
//
// Mutation: drop the release from a spool's removal, the shrink to its sealed
// size, or the seal's cap at its room, and a case below goes red.
func TestEverySpoolGivesItsRoomBackWithItsFile(t *testing.T) {
	t.Parallel()
	given := func(t *testing.T, rig *coordRig) {
		t.Helper()
		if used := roomInUse(rig.coordinator); used != 0 {
			t.Errorf("%d bytes of the room are still held", used)
		}
		if left := rig.spools(t); len(left) != 0 {
			t.Errorf("spools %v are left", left)
		}
	}
	t.Run("a final take kept", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep := keepingRig(t)
			run := rig.launchKept(t, "t1")
			keep.gate = make(chan struct{})
			spooled, _ := finalTake(t, rig, run, rig.runner)
			if spooled == nil {
				t.Fatal("the take spooled nothing")
			}
			rig.coordinator.keepLater(t.Context(), spooled)
			synctest.Wait()
			if used := roomInUse(rig.coordinator); used != spooled.spool.Size {
				t.Errorf("a spool waiting for its upload holds %d bytes of the room, want its sealed %d",
					used, spooled.spool.Size)
			}
			close(keep.gate)
			rig.awaitKeeps()
			given(t, rig)
		})
	})
	t.Run("a final take a stopped node drops", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			spooled, _ := finalTake(t, rig, run, rig.runner)
			if spooled == nil {
				t.Fatal("the take spooled nothing")
			}
			rig.coordinator.Stop()
			rig.coordinator.keepLater(t.Context(), spooled)
			given(t, rig)
		})
	})
	t.Run("a save landed", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			if out := saveOf(rig, "t1", nil); out.class != saveLanded {
				t.Fatalf("the save came to %+v, want it landed", out)
			}
			given(t, rig)
		})
	})
	t.Run("a save whose seal passes what was counted", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			archive := rig.runner.Session.Archive
			archive.Size = 1
			counted := countedFor(archive.Size)
			rig.runner.mu.Lock()
			rig.runner.Session = &FakeSession{Archive: archive, Body: bytes.Repeat([]byte("s"), int(2*counted))}
			rig.runner.mu.Unlock()
			out := saveOf(rig, "t1", nil)
			if out.class != saveFailed || !strings.Contains(out.why, "save ledger counted") {
				t.Fatalf("the save came to %+v, want it refused past what its seat's ledger counted", out)
			}
			for _, wrote := range keep.seals() {
				if wrote > counted {
					t.Errorf("a seal wrote %d bytes into a spool holding room for %d", wrote, counted)
				}
			}
			given(t, rig)
		})
	})
	t.Run("a standby that takes the next place", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			stampedBegun(t, rig, run)
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby {
				t.Fatalf("the read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			rig.now = rig.now.Add(SessionCaptureBudget)
			if kept := rig.coordinator.keepCapture(t.Context(), standby); kept == nil || kept.NotKept != "" {
				t.Fatalf("the standby's keep came to %+v, want its session kept", kept)
			}
			given(t, rig)
		})
	})
	t.Run("a standby a stopped node drops", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			stampedBegun(t, rig, run)
			standby := standingBy(t, rig, run, bytes.NewReader(standbyArchive))
			if standby == nil || !standby.standby {
				t.Fatalf("the read beside node-a's capture spooled %+v, want it standing by", standby)
			}
			rig.coordinator.Stop()
			rig.coordinator.keepLater(t.Context(), standby)
			given(t, rig)
		})
	})
}

// stampedBegun stamps run's capture begun by node-a's taker, as a read on
// another node holding its place would.
func stampedBegun(t *testing.T, rig *coordRig, run PendingRun) {
	t.Helper()
	begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
		State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess, TakerEpoch: 1,
		Harness: "claude-code", SessionID: "s-1"}
	if took, err := rig.pending.StampCapture(t.Context(), run.TurnID, begun); err != nil || !took {
		t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
	}
}

// A FINAL TAKE WAITS FOR ROOM BEFORE IT HOLDS ANYTHING A SAVE WAITS FOR: a
// save holding room waits for the node's pack slot and then for the take's own
// slots (the executor's take and large-read slots, held from its TakeSession
// to its scrub's end), so a final take waiting for room holds neither the pack
// slot nor a take of its box. Its sealed spool's room, taken at the sealed
// bound, is shrunk to its archive's counted size before it streams, beside
// what its scrub's spool holds (the fake's, nothing).
//
// Mutation: wait for the room after the pack slot is taken or after the box's
// session is taken, or keep the bound's room while streaming, and a check
// below goes red.
func TestAFinalTakeWaitsForRoomBeforeItHoldsWhatASaveWaitsFor(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		run := rig.launchKept(t, "t1")
		full := rig.coordinator.trySpoolRoom(spoolRoomBytes)
		var streamingHeld int64
		rig.runner.mu.Lock()
		counted := countedFor(rig.runner.Session.Archive.Size)
		rig.runner.Session.Streaming = func(context.Context) { streamingHeld = roomInUse(rig.coordinator) }
		rig.runner.mu.Unlock()
		var spooled *spooledCapture
		taken := make(chan *SessionKept, 1)
		go func() {
			var kept *SessionKept
			spooled, kept = finalTake(t, rig, run, rig.runner)
			taken <- kept
		}()
		synctest.Wait()
		select {
		case rig.coordinator.sharedPack <- struct{}{}:
			<-rig.coordinator.sharedPack
		default:
			t.Error("a take waiting for room held the node's pack slot")
		}
		rig.runner.mu.Lock()
		takes := len(rig.runner.takes)
		rig.runner.mu.Unlock()
		if takes != 0 {
			t.Errorf("a take waiting for room had taken its box's session %d times, want none", takes)
		}
		full.release()
		if kept := <-taken; kept == nil || kept.NotKept != "" {
			t.Errorf("the take came to %+v once the room was back, want its session taken", kept)
		}
		if streamingHeld != counted {
			t.Errorf("the take streamed holding %d bytes of the room, want its archive's counted %d",
				streamingHeld, counted)
		}
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
	})
}

// spoolMeterSpy is a coordinator's [SpoolMeter] that keeps what it is told.
type spoolMeterSpy struct {
	mu       sync.Mutex
	waits    map[SpoolOutcome][]time.Duration
	held     int64
	heldTold bool
}

func (m *spoolMeterSpy) SpoolWaited(outcome SpoolOutcome, waited time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waits == nil {
		m.waits = map[SpoolOutcome][]time.Duration{}
	}
	m.waits[outcome] = append(m.waits[outcome], waited)
}

func (m *spoolMeterSpy) SpoolHeld(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held, m.heldTold = bytes, true
}

// waited is the waits the spy was told ended with outcome.
func (m *spoolMeterSpy) waited(outcome SpoolOutcome) []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.waits[outcome])
}

// heldNow is the room held as the spy was last told it.
func (m *spoolMeterSpy) heldNow(t *testing.T) int64 {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.heldTold {
		t.Fatal("the meter was never told the room held")
	}
	return m.held
}

// A FINAL TAKE REFUSED FOR ROOM IS COUNTED AND SAYS HOW FULL THE ROOM WAS:
// the meter is told its minute's wait as refused, and its line at warning names the
// wait, the room it asked for, the room's size and what other spools held,
// so a rising count says whether the room or the minute wants retuning.
//
// Mutation: tell the meter nothing of a refusal, or drop a field from its line.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAFinalTakeRefusedForRoomIsCountedAndLogged(t *testing.T) {
	logs := captureLogs(t)
	meter := &spoolMeterSpy{}
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Spools = meter })
		run := rig.launchKept(t, "t1")
		full := rig.coordinator.trySpoolRoom(spoolRoomBytes)
		defer full.release()
		if spooled, kept := finalTake(t, rig, run, rig.runner); spooled != nil || kept == nil || kept.NotKept == "" {
			t.Fatalf("the take with the room full came to %+v, want it not kept", kept)
		}
		if waits := meter.waited(SpoolRefused); !slices.Equal(waits, []time.Duration{spoolRoomWait}) {
			t.Errorf("the meter was told of refused waits %v, want one of the minute's %v", waits, spoolRoomWait)
		}
		if waits := meter.waited(SpoolTaken); len(waits) != 0 {
			t.Errorf("the meter was told of taken waits %v for a take that found no room", waits)
		}
		var line string
		for l := range strings.Lines(logs.String()) {
			if strings.Contains(l, "executor_session_not_kept") && strings.Contains(l, "room_bytes=") {
				line = l
			}
		}
		for _, want := range []string{"level=WARN", "waited_seconds=60 ",
			fmt.Sprintf(" bytes=%d ", takeRoomBytes),
			fmt.Sprintf(" room_bytes=%d ", spoolRoomBytes),
			fmt.Sprintf(" others_held_bytes=%d", spoolRoomBytes)} {
			if !strings.Contains(line, want) {
				t.Errorf("the refused take's line %q lacks %q", line, want)
			}
		}
	})
}

// EVERY FINAL TAKE'S WAIT FOR ROOM IS TIMED BY HOW IT ENDED: one that gets its
// room is told the meter as taken with its wait, none at once and ten seconds
// behind a full room for ten; one whose own time runs out first as ended.
//
// Mutation: tell the meter nothing when the room is taken, or time the wait from
// anywhere but its start, and a case below goes red.
func TestEveryWaitForSpoolRoomIsTimedByHowItEnded(t *testing.T) {
	t.Parallel()
	const behind = 10 * time.Second
	t.Run("taken at once", func(t *testing.T) {
		t.Parallel()
		meter := &spoolMeterSpy{}
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Spools = meter })
			run := rig.launchKept(t, "t1")
			spooled, kept := finalTake(t, rig, run, rig.runner)
			if spooled == nil || kept == nil || kept.NotKept != "" {
				t.Fatalf("the take came to %+v, want its session spooled", kept)
			}
			rig.coordinator.keepLater(t.Context(), spooled)
			rig.awaitKeeps()
			if waits := meter.waited(SpoolTaken); !slices.Equal(waits, []time.Duration{0}) {
				t.Errorf("the meter was told of taken waits %v, want one of none", waits)
			}
		})
	})
	t.Run("taken behind a full room", func(t *testing.T) {
		t.Parallel()
		meter := &spoolMeterSpy{}
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Spools = meter })
			run := rig.launchKept(t, "t1")
			full := rig.coordinator.trySpoolRoom(spoolRoomBytes)
			var spooled *spooledCapture
			taken := make(chan *SessionKept, 1)
			go func() {
				var kept *SessionKept
				spooled, kept = finalTake(t, rig, run, rig.runner)
				taken <- kept
			}()
			synctest.Wait()
			time.Sleep(behind)
			full.release()
			if kept := <-taken; kept == nil || kept.NotKept != "" {
				t.Fatalf("the take came to %+v once the room was back, want its session spooled", kept)
			}
			rig.coordinator.keepLater(t.Context(), spooled)
			rig.awaitKeeps()
			if waits := meter.waited(SpoolTaken); !slices.Equal(waits, []time.Duration{behind}) {
				t.Errorf("the meter was told of taken waits %v, want one of %v", waits, behind)
			}
		})
	})
	t.Run("ended by the take's own time", func(t *testing.T) {
		t.Parallel()
		meter := &spoolMeterSpy{}
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Spools = meter })
			full := rig.coordinator.trySpoolRoom(spoolRoomBytes)
			defer full.release()
			ctx, cancel := context.WithTimeout(t.Context(), behind)
			defer cancel()
			hold, refused := rig.coordinator.awaitSpoolRoom(ctx, executorkeep.SealedMaxBytes)
			if hold != nil || refused == nil || !errors.Is(refused.ended, context.DeadlineExceeded) {
				hold.release()
				t.Fatalf("a take whose time ended while it waited came to %+v, %+v, want it refused, its "+
					"time named", hold, refused)
			}
			if waits := meter.waited(SpoolEnded); !slices.Equal(waits, []time.Duration{behind}) {
				t.Errorf("the meter was told of ended waits %v, want one of %v", waits, behind)
			}
			if waits := meter.waited(SpoolRefused); len(waits) != 0 {
				t.Errorf("a take whose own time ended was also told refused: %v", waits)
			}
		})
	})
}

// THE METER IS TOLD EXACTLY WHAT THE ROOM HOLDS, through a take without
// waiting, a take that waits, a shrink to a spool's sealed size and every
// release, and 0 once all are given back: read against the room itself.
//
// Mutation: skip the count in a hold's shrink, a take or a release, and a step
// below goes red.
func TestTheMeterIsToldWhatTheSpoolRoomHolds(t *testing.T) {
	t.Parallel()
	meter := &spoolMeterSpy{}
	inBubble(t, func(t *testing.T) {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.Spools = meter })
		c := rig.coordinator
		step := func(when string, want int64) {
			t.Helper()
			if held, used := meter.heldNow(t), roomInUse(c); held != want || used != want {
				t.Errorf("%s: the meter was told %d bytes held and the room holds %d, want both %d",
					when, held, used, want)
			}
		}
		step("before any spool", 0)
		save := c.trySpoolRoom(executorkeep.SealedMaxBytes)
		step("a save's room at the bound", executorkeep.SealedMaxBytes)
		take, refused := c.awaitSpoolRoom(t.Context(), executorkeep.SealedMaxBytes)
		if refused != nil {
			t.Fatalf("a take beside one save found no room: %s", refused.reason())
		}
		step("and a take's", 2*executorkeep.SealedMaxBytes)
		take.shrinkTo(1 << 20)
		step("the take shrunk to its sealed size", executorkeep.SealedMaxBytes+1<<20)
		save.release()
		step("the save given back", 1<<20)
		take.release()
		take.release()
		step("every spool given back", 0)
	})
}
