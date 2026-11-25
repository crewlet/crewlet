package sandbox

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
)

// everyCell gives rig's coordinator a catalogue of every cell, each on the
// rig's one backend and runner, so a row's cell alone decides what a drain
// saves.
func everyCell(t *testing.T, rig *coordRig) {
	t.Helper()
	every, err := NewManager(ManagerOptions{
		Providers:        map[Placement]Provider{Direct: rig.provider, Container: rig.provider},
		Runners:          map[string]Runner{"claude-code": rig.runner},
		DefaultPlacement: Direct,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.coordinator.SetManager(every)
}

// reshape rewrites turnID's row as edit leaves it.
func reshape(t *testing.T, rig *coordRig, turnID string, edit func(*PendingRun)) {
	t.Helper()
	if _, _, err := rig.pending.mutateAny(t.Context(), turnID, func(row *PendingRun) bool {
		edit(row)
		return true
	}); err != nil {
		t.Fatalf("rewriting %s's row: %v", turnID, err)
	}
}

// A DRAIN SAVES EVERY MOVED DIRECT JOB ON ITS HOST, MOST STALE FIRST: of the
// running jobs, a draining node packs the Direct ones it launched whose
// sessions moved, the one never saved before the one saved an interval ago,
// and each lands. An unmoved one is not packed; a container's and another
// node's are not touched; and a save of a turn already under way is
// waited for, never doubled.
//
// Mutation: take a container's job too, sort the other way, start a second
// save beside one under way, or bound the take's parent by the drain's end.
func TestADrainSavesEveryMovedDirectJobOnItsHostMostStaleFirst(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		everyCell(t, rig)
		boxes, turnOf := map[string]string{}, map[string]string{}
		for _, turn := range []string{"never", "long-ago", "unmoved", "busy", "container", "elsewhere"} {
			box := rig.launchKept(t, turn).SandboxID
			boxes[turn], turnOf[box] = box, turn
		}
		reshape(t, rig, "container", func(row *PendingRun) { row.Placement = string(Container) })
		reshape(t, rig, "elsewhere", func(row *PendingRun) { row.LaunchNode = "node-b" })
		rig.now = rig.now.Add(rigSaveInterval)
		for _, turn := range []string{"long-ago", "unmoved"} {
			if !offerOf(t, rig, turn) || rig.get(turn).Kept == nil {
				t.Fatalf("the premise: the periodic save of %s did not land", turn)
			}
		}
		rig.runner.Stream(rig.provider.Box(boxes["long-ago"]), 64)

		var (
			mu     sync.Mutex
			packed []string
		)
		busyPacking, hold := make(chan struct{}, 1), make(chan struct{})
		rig.runner.Packing = func(_ context.Context, box Sandbox) {
			mu.Lock()
			packed = append(packed, turnOf[box.ID()])
			mu.Unlock()
			if box.ID() == boxes["busy"] {
				select {
				case busyPacking <- struct{}{}:
				default:
				}
				<-hold
			}
		}
		busy := rig.coordinator.offerSave(t.Context(), rig.get("busy"), rig.runner)
		if busy == nil {
			t.Fatal("the premise: the busy job's periodic save did not start")
		}
		<-busyPacking
		// EVERY OTHER SLOT HELD, as keeps under way hold them: the drain's
		// saves take busy's in turn once it is given back, so their order is
		// the drain's own, and a drain blind to busy's save would wait for a
		// slot to save it again.
		for range maxConcurrentSaves - 1 {
			rig.coordinator.saves.slots <- struct{}{}
		}
		rig.runner.Session.Taking = func(ctx context.Context) {
			if err := ctx.Err(); err != nil {
				t.Errorf("a save was taken on a context already done (%v), want one the drain's end bounds", err)
			}
		}
		done := make(chan []*saveRun, 1)
		go func() { done <- rig.coordinator.saveHosted(t.Context()) }()
		// THE DRAIN HAS LISTED THE RUNS once every goroutine waits: only then
		// does the save under way pass its spool.
		synctest.Wait()
		close(hold)
		picked := <-done
		<-busy.ended
		rig.awaitKeeps()

		mu.Lock()
		got := slices.Clone(packed)
		mu.Unlock()
		if want := []string{"busy", "never", "long-ago"}; !slices.Equal(got, want) {
			t.Errorf("packed the sessions of %v, want %v: the busy job's periodic save, then the moved Direct "+
				"jobs on this host, most stale first", got, want)
		}
		if !slices.Contains(picked, busy) {
			t.Error("the drain did not wait for the busy job's save under way")
		}
		for turn, seq := range map[string]int64{"never": 1, "long-ago": 2} {
			if kept := rig.get(turn).Kept; kept == nil || kept.CaptureSeq != seq || kept.State != CaptureLanded {
				t.Errorf("%s keeps %+v, want its drain save landed as capture %d", turn, kept, seq)
			}
		}
	})
}

// A DRAIN ADMITS NO PERIODIC SAVE: once a node drains, an offer of a box its
// poll found running starts nothing, though the box is due and moved.
//
// Mutation: drop the drain from the offer's rule.
func TestADrainAdmitsNoPeriodicSave(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		everyCell(t, rig)
		// CONTAINER JOBS, which outlive the drain in their containers, so
		// the drain itself saves neither.
		for _, turn := range []string{"before", "after"} {
			rig.launchKept(t, turn)
			reshape(t, rig, turn, func(row *PendingRun) { row.Placement = string(Container) })
		}
		rig.now = rig.now.Add(rigSaveInterval)
		if !offerOf(t, rig, "before") {
			t.Fatal("the premise: a container job due for a save was offered none")
		}
		if picked := rig.coordinator.saveHosted(t.Context()); len(picked) != 0 {
			t.Fatalf("the premise: the drain saved %d jobs, and a container job is none of its", len(picked))
		}
		if running := rig.coordinator.offerSave(t.Context(), rig.get("after"), rig.runner); running != nil {
			<-running.ended
			t.Error("an offer after the drain began started a save")
		}
	})
}

// A DRAIN SAVE ENDS WITHIN ITS WAIT: a pack held past the drain's wait is cut
// there and stamps nothing; a take held past its place is cut there and its
// place stamped refused; and the drain returns at its wait whatever a save
// under way still does.
//
// Mutation: leave the drain's box phase unbounded, or its take, or have the
// drain wait for a save past its wait.
func TestADrainSaveEndsWithinItsWait(t *testing.T) {
	t.Parallel()
	t.Run("a pack held past the wait", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			cut, release := make(chan time.Time, 1), make(chan struct{})
			rig.runner.Packing = func(ctx context.Context, _ Sandbox) {
				select {
				case <-ctx.Done():
					cut <- time.Now()
				case <-release:
				}
			}
			start := time.Now()
			picked := rig.coordinator.saveHosted(t.Context())
			if took := time.Since(start); took != drainSaveWait {
				t.Errorf("the drain returned %v after it began, want at its wait of %v", took, drainSaveWait)
			}
			select {
			case at := <-cut:
				if at.Sub(start) != drainSaveWait {
					t.Errorf("the pack was cut %v after the drain began, want at its wait of %v",
						at.Sub(start), drainSaveWait)
				}
			case <-time.After(time.Minute):
				t.Error("the pack ran on a minute past the drain's wait")
				close(release)
			}
			if len(picked) != 1 {
				t.Fatalf("the drain picked %d saves, want the job's", len(picked))
			}
			<-picked[0].ended
			if capture := rig.get("t1").Capture; capture != nil {
				t.Errorf("the run's row holds %+v, want no stamp of a save cut before its place", capture)
			}
		})
	})
	t.Run("a take held past its place", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			cut := make(chan time.Time, 1)
			rig.runner.Session.Streaming = func(ctx context.Context) {
				<-ctx.Done()
				cut <- time.Now()
			}
			start := time.Now()
			picked := rig.coordinator.saveHosted(t.Context())
			if len(picked) != 1 {
				t.Fatalf("the drain picked %d saves, want the job's", len(picked))
			}
			<-picked[0].ended
			if at := <-cut; at.Sub(start) != drainSaveWait {
				t.Errorf("the take's spool was cut %v after the drain began, want at its wait of %v",
					at.Sub(start), drainSaveWait)
			}
			if capture := rig.get("t1").Capture; capture == nil || capture.CaptureSeq != 1 ||
				capture.State != CaptureRefused {
				t.Errorf("the run's newest capture is %+v, want the drain save's place stamped refused", capture)
			}
		})
	})
	t.Run("a save under way held past the wait", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			rig.now = rig.now.Add(rigSaveInterval)
			packing, hold := make(chan struct{}, 1), make(chan struct{})
			rig.runner.Packing = func(context.Context, Sandbox) {
				select {
				case packing <- struct{}{}:
				default:
				}
				<-hold
			}
			busy := rig.coordinator.offerSave(t.Context(), rig.get("t1"), rig.runner)
			if busy == nil {
				t.Fatal("the premise: the job's periodic save did not start")
			}
			<-packing
			start := time.Now()
			time.AfterFunc(drainSaveWait+time.Minute, func() { close(hold) })
			picked := rig.coordinator.saveHosted(t.Context())
			if took := time.Since(start); took != drainSaveWait {
				t.Errorf("the drain returned %v after it began, want at its wait of %v whatever the save under "+
					"way does", took, drainSaveWait)
			}
			if !slices.Equal(picked, []*saveRun{busy}) {
				t.Errorf("the drain picked %v, want the save under way alone", picked)
			}
			<-busy.ended
		})
	})
}

// A DRAIN LETS A SAVE GO ONLY ONCE ITS KEEP IS COUNTED: a stop whose close
// lands the instant the drain is let go of a save still lets that save's keep
// land, since the keep had already joined the keeps a stop waits for.
//
// Mutation: mark the spool passed before the save joins the keeps.
func TestADrainLetsASaveGoOnlyOnceItsKeepIsCounted(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.launchKept(t, "t1")
		c := rig.coordinator
		now := c.now()
		job := saveJob{run: rig.get("t1"), drainEnd: time.Now().Add(drainSaveWait), due: now, mark: -1,
			interval: rigSaveInterval, last: now}
		in, out := c.saveInHand(c.life, t.Context(), job, saveOutcome{streamed: -1})
		if in == nil {
			t.Fatalf("the premise: the drain save ended before its take: %+v", out)
		}
		c.inflight.Add(1)
		inflight := sync.OnceFunc(c.inflight.Done)
		defer inflight()
		// THE STOP'S FIRST STEP, closing the coordinator the instant the
		// drain is let go of the save.
		letGo := func() {
			c.mu.Lock()
			c.closed = true
			c.mu.Unlock()
		}
		if out = c.takeSave(c.life, job, job.point(), in, out, letGo, inflight); out.class != saveLanded {
			t.Errorf("the drain save came to %+v, want its keep landed: counted before the drain let it go, "+
				"no stop refuses it", out)
		}
	})
}
