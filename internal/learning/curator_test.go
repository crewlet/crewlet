package learning_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

// announced collects what the background passes published.
type announced struct {
	mu   sync.Mutex
	seen []events.Payload
}

func (a *announced) record(_ context.Context, _ string, p events.Payload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, p)
}

func (a *announced) all() []events.Payload {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]events.Payload(nil), a.seen...)
}

// awaitEvents waits for n announcements. A poll rather than a channel because
// the loops publish from their own goroutines and the count is what the test
// is about — a channel would also have to say which pass sent each one.
func (a *announced) awaitEvents(t *testing.T, n int) []events.Payload {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := a.all(); len(got) >= n {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waited for %d announcements, saw %d", n, len(a.all()))
	return nil
}

// THE CURATOR AGES THE CATALOGUE ON ITS OWN, which is the whole reason it is
// a loop: nothing about a skill going unused is driven by a turn.
func TestTheCuratorStalesAnUnusedSkillOnItsOwn(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	mustInsert(t, store, newSkill("dev", "triage", base))

	var seen announced
	now := base.Add(60 * 24 * time.Hour)
	learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Skills: store,
			Policy: learning.CuratorPolicy{
				StaleAfter: 30 * 24 * time.Hour, ArchiveAfter: 90 * 24 * time.Hour,
			},
			CuratorInterval: time.Millisecond,
		},
		Publish: seen.record,
		Now:     func() time.Time { return now },
	}).Start(t.Context())

	events := seen.awaitEvents(t, 1)
	ev, ok := events[0].(types.SkillStaled)
	if !ok {
		t.Fatalf("event = %T, want SkillStaled", events[0])
	}
	if ev.SkillID != "sk-dev-triage" || ev.AgentHandle != "dev" {
		t.Errorf("event = %+v", ev)
	}
	if got := mustSkill(t, store, "sk-dev-triage"); got.State != learning.SkillStale {
		t.Errorf("state = %q, want stale", got.State)
	}
}

// A SEAT UNDER THRESHOLD IS NOT COMPACTED, and the count is what gates it:
// the check is one indexed query and "not yet" is the overwhelmingly common
// answer, so the pass must not run to discover it.
func TestASeatUnderThresholdIsNotCompacted(t *testing.T) {
	t.Parallel()
	life, eps, sum := lifecycle(t, learning.Options{Threshold: 50})
	mustAppend(t, eps, ep("a", "dev", base))

	var seen announced
	learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Lifecycle:         life,
			LifecycleInterval: time.Millisecond,
		},
		Seats:   func() []string { return []string{"dev"} },
		Publish: seen.record,
		Now:     func() time.Time { return base.Add(time.Hour) },
	}).Start(t.Context())

	time.Sleep(50 * time.Millisecond)
	if got := seen.all(); len(got) != 0 {
		t.Fatalf("a seat under threshold produced %d announcements", len(got))
	}
	if got := sum.count(); got != 0 {
		t.Fatalf("the summarizer was called %d times for a seat under threshold", got)
	}
}

// NOTHING RUNS WITHOUT THE DUTY. Two nodes compacting one seat's episodes
// summarise the same cluster twice and pay for it twice.
func TestABackgroundPassWithoutTheDutyDoesNothing(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	mustInsert(t, store, newSkill("dev", "triage", base))

	var seen announced
	learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Skills:          store,
			Policy:          learning.CuratorPolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour},
			CuratorInterval: time.Millisecond,
		},
		Publish:   seen.record,
		ClaimDuty: func(context.Context) (bool, error) { return false, nil },
		Now:       func() time.Time { return base.Add(60 * 24 * time.Hour) },
	}).Start(t.Context())

	time.Sleep(50 * time.Millisecond)
	if got := seen.all(); len(got) != 0 {
		t.Fatalf("a node without the duty ran the pass: %d announcements", len(got))
	}
	if got := mustSkill(t, store, "sk-dev-triage"); got.State != learning.SkillActive {
		t.Fatalf("state = %q, want the row untouched", got.State)
	}
}

// AN UNREACHABLE COORDINATION STORE FAILS CLOSED, which is the opposite of
// what the read side does: not knowing whether a peer holds the duty is
// exactly the case where running anyway produces two nodes doing the work.
func TestAnUnknownDutyIsTreatedAsNotHeld(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	mustInsert(t, store, newSkill("dev", "triage", base))

	var seen announced
	learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Skills:          store,
			Policy:          learning.CuratorPolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour},
			CuratorInterval: time.Millisecond,
		},
		Publish: seen.record,
		ClaimDuty: func(context.Context) (bool, error) {
			return false, errors.New("the coordination store is unreachable")
		},
		Now: func() time.Time { return base.Add(60 * 24 * time.Hour) },
	}).Start(t.Context())

	time.Sleep(50 * time.Millisecond)
	if got := seen.all(); len(got) != 0 {
		t.Fatalf("an unknown duty ran the pass: %d announcements", len(got))
	}
}

// NO IMMEDIATE FIRST TICK: every node in a fleet starts within seconds of a
// rolling restart, so firing on start means every node races for the duty at
// once and a crash-looping node spends the company's tokens on every restart.
func TestNeitherPassFiresOnStart(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	mustInsert(t, store, newSkill("dev", "triage", base))

	var claims int64
	var mu sync.Mutex
	learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Skills:          store,
			Policy:          learning.CuratorPolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour},
			CuratorInterval: time.Hour,
		},
		ClaimDuty: func(context.Context) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			claims++
			return true, nil
		},
		Now: func() time.Time { return base.Add(60 * 24 * time.Hour) },
	}).Start(t.Context())

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if claims != 0 {
		t.Fatalf("the duty was claimed %d times before the first interval elapsed", claims)
	}
	if got := mustSkill(t, store, "sk-dev-triage"); got.State != learning.SkillActive {
		t.Fatalf("state = %q, want the row untouched before the first tick", got.State)
	}
}

// A PASS THAT IS OFF CLAIMS NO DUTY, which is what every loop of a node with
// no company, or of a company with learning off, looks like.
//
// The loops are armed whatever is wired, because the next apply may turn a
// pass on and a loop armed only then would start its clock at that apply. So
// what an idle loop must not do is claim: that is a coordination round trip
// on every tick for no work. The duty counter is what makes this able to
// fail, and a millisecond interval means every loop ticks many times well
// inside the wait rather than sitting on a cadence measured in hours.
func TestAPassThatIsOffClaimsNoDuty(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		claims int
	)
	b := learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			CuratorInterval:   time.Millisecond,
			LifecycleInterval: time.Millisecond,
			ClusterInterval:   time.Millisecond,
			PromotionInterval: time.Millisecond,
		},
		ClaimDuty: func(context.Context) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			claims++
			return true, nil
		},
	})
	// It must not panic and must not spin: with every pass nil, each loop
	// ticks and finds nothing to run.
	b.Start(t.Context())
	t.Cleanup(b.Stop)

	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if claims != 0 {
		t.Fatalf("the duty was claimed %d times with no pass wired", claims)
	}
}

// dutyCounter counts the duty claims the loops make, which is the first thing
// a loop does for a pass that is on and the one thing it never does for a
// pass that is off.
type dutyCounter struct {
	mu     sync.Mutex
	claims int
}

func (d *dutyCounter) claim(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.claims++
	return true, nil
}

func (d *dutyCounter) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.claims
}

// A PASS AN APPLY TURNS ON RUNS WITHOUT A RESTART, at the cadence that apply
// configured.
//
// The loops used to be built from whichever company the process started
// with: a node that booted with no company, or with no model to compact on,
// never ran the pass however its configuration changed afterwards. Started
// here with nothing wired and the default daily cadence, the curator has to
// run within the wait only if the Reconfigure both turned it on and moved its
// clock to the new cadence.
func TestAPassAReconfigureTurnsOnRuns(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	mustInsert(t, store, newSkill("dev", "triage", base))

	var seen announced
	b := learning.NewBackground(learning.BackgroundOptions{
		Publish: seen.record,
		Now:     func() time.Time { return base.Add(60 * 24 * time.Hour) },
	})
	b.Start(t.Context())
	t.Cleanup(b.Stop)
	// Long enough for every loop to have read the daily cadence it starts
	// on, so what runs the pass is the wake rather than a loop that simply
	// started after the Reconfigure.
	time.Sleep(50 * time.Millisecond)

	b.Reconfigure(learning.BackgroundPasses{
		Skills:          store,
		Policy:          learning.CuratorPolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour},
		CuratorInterval: time.Millisecond,
	})
	if _, ok := seen.awaitEvents(t, 1)[0].(types.SkillStaled); !ok {
		t.Fatalf("events = %v, want the curator's stale transition", seen.all())
	}
}

// AND A PASS AN APPLY TURNS OFF STOPS, claiming nothing further: a company
// that turned learning off, or lost the model a pass runs on, must not go on
// running it on the revision it replaced.
func TestAPassAReconfigureTurnsOffStops(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	var duty dutyCounter
	b := learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Skills:          store,
			Policy:          learning.CuratorPolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour},
			CuratorInterval: time.Millisecond,
		},
		ClaimDuty: duty.claim,
		Now:       func() time.Time { return base },
	})
	b.Start(t.Context())
	t.Cleanup(b.Stop)

	deadline := time.Now().Add(5 * time.Second)
	for duty.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the curator loop never claimed its duty")
		}
		time.Sleep(time.Millisecond)
	}
	b.Reconfigure(learning.BackgroundPasses{CuratorInterval: time.Millisecond})
	// A tick that raced the wake may still claim with the passes it last
	// read, so the count is held to zero only once it has stopped moving.
	// A loop that never heard the wake claims every millisecond and never
	// settles.
	after, settled := duty.count(), time.Now()
	for time.Since(settled) < 30*time.Millisecond {
		if time.Now().After(deadline) {
			t.Fatal("the curator loop went on claiming a pass the reconfigure turned off")
		}
		time.Sleep(time.Millisecond)
		if n := duty.count(); n != after {
			after, settled = n, time.Now()
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := duty.count(); got != after {
		t.Fatalf("a pass turned off was claimed %d more times", got-after)
	}
}

// AN APPLY THAT LEAVES A CADENCE ALONE LEAVES ITS CLOCK ALONE.
//
// Every config apply reconfigures the loops, and a company being edited in the
// org builder applies far more often than its daily curator ticks. Resetting
// the clock on each one would push the next tick out every time, so the pass
// would never run at all. Here the loops are reconfigured with identical
// passes many times faster than the cadence, and the pass still has to run.
func TestAReconfigureThatKeepsTheCadenceKeepsTheClock(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	var duty dutyCounter
	passes := learning.BackgroundPasses{
		Skills:          store,
		Policy:          learning.CuratorPolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour},
		CuratorInterval: 40 * time.Millisecond,
	}
	b := learning.NewBackground(learning.BackgroundOptions{
		Passes: passes, ClaimDuty: duty.claim,
		Now: func() time.Time { return base },
	})
	b.Start(t.Context())
	t.Cleanup(b.Stop)

	deadline := time.Now().Add(2 * time.Second)
	for duty.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a loop reconfigured every 2ms with an unchanged 40ms cadence " +
				"never ticked, so its clock restarts on every apply")
		}
		b.Reconfigure(passes)
		time.Sleep(2 * time.Millisecond)
	}
}

// STOP IS THE ONLY EXIT the loops have. The engine starts them on a detached
// context so they outlive the signal that begins a drain, which means the
// ctx.Done arm of the loop never fires in production — and the caller's next
// move after Stop returns is to close the store every pass queries.
func TestStopEndsTheLoops(t *testing.T) {
	t.Parallel()
	skills, _ := skillStore(t)
	mustInsert(t, skills, newSkill("dev", "triage", base))

	var (
		mu     sync.Mutex
		claims int
	)
	// WithoutCancel is what the engine hands Start, so this exercises the
	// case where the context genuinely cannot end the loop.
	b := learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Skills:          skills,
			Policy:          learning.CuratorPolicy{StaleAfter: time.Hour, ArchiveAfter: 2 * time.Hour},
			CuratorInterval: time.Millisecond,
		},
		ClaimDuty: func(context.Context) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			claims++
			return true, nil
		},
		Now: func() time.Time { return base.Add(60 * 24 * time.Hour) },
	})
	b.Start(context.WithoutCancel(t.Context()))

	// Wait for the loop to prove it is running before stopping it —
	// otherwise a Stop that does nothing would pass by racing the arm.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := claims
		mu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the curator loop never claimed its duty")
		}
		time.Sleep(time.Millisecond)
	}

	b.Stop()
	mu.Lock()
	after := claims
	mu.Unlock()

	// Stop waits for the in-flight pass, so nothing may claim the duty
	// once it has returned.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if claims != after {
		t.Fatalf("the duty was claimed %d more times after Stop returned", claims-after)
	}

	// Idempotent, and safe on a Background that was never started — the
	// shape a node with no store produces, which Engine.Stop still calls.
	b.Stop()
	learning.NewBackground(learning.BackgroundOptions{}).Stop()
}

// THE ROSTER IS READ FRESH on every tick. An apply replaces it, and a pass
// holding the list it started with would keep compacting a seat the company
// removed and never touch one it added.
func TestTheRosterIsReadEveryTick(t *testing.T) {
	t.Parallel()
	life, _, _ := lifecycle(t, learning.Options{Threshold: 1})

	var (
		mu    sync.Mutex
		asked int
	)
	learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Lifecycle:         life,
			LifecycleInterval: time.Millisecond,
		},
		Seats: func() []string {
			mu.Lock()
			defer mu.Unlock()
			asked++
			return nil
		},
		Now: func() time.Time { return base },
	}).Start(t.Context())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := asked
		mu.Unlock()
		if got >= 2 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the roster was read fewer than twice across many ticks")
}

// countingSummarizer records how often compaction reached the model, which is
// the cost the threshold check exists to avoid paying.
type countingSummarizer struct {
	mu    sync.Mutex
	calls int
}

func (s *countingSummarizer) Summarize(_ context.Context, c learning.Cluster) (learning.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return learning.Summary{
		CommonTaskPattern: "a pattern", CommonOutcome: "done",
	}, nil
}

func (s *countingSummarizer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// lifecycle builds a compaction pass over its own store.
//
// Built here through the EXPORTED constructor rather than borrowing the
// internal suite's newLife: these cases drive the pass from the background
// loop, which is the seam an operator's deployment actually uses.
func lifecycle(t *testing.T, o learning.Options) (*learning.Lifecycle, *learning.Episodes, *countingSummarizer) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "life.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sum := &countingSummarizer{}
	return learning.NewLifecycle(db, sum, o), learning.NewEpisodes(db), sum
}

// A SEAT OVER THRESHOLD IS COMPACTED, and announced twice — once when it
// became due and once with what the pass did. The first is the only signal
// an operator gets when the pass then fails, which is what makes "this seat
// is over threshold and never gets compacted" visible at all.
func TestASeatOverThresholdIsCompactedAndAnnounced(t *testing.T) {
	t.Parallel()
	life, eps, _ := lifecycle(t, learning.Options{
		Threshold: 2, MinAge: time.Hour, MinClusterSize: 2,
		JaccardThreshold: 0.5, BatchSize: 100,
	})
	for _, id := range []string{"a", "b", "c"} {
		mustAppend(t, eps, ep(id, "dev", base))
	}

	var seen announced
	learning.NewBackground(learning.BackgroundOptions{
		Passes: learning.BackgroundPasses{
			Lifecycle:         life,
			LifecycleInterval: time.Millisecond,
		},
		Seats:   func() []string { return []string{"dev"} },
		Publish: seen.record,
		Now:     func() time.Time { return base.Add(90 * 24 * time.Hour) },
	}).Start(t.Context())

	got := seen.awaitEvents(t, 2)
	req, ok := got[0].(types.CompactionRequested)
	if !ok {
		t.Fatalf("first event = %T, want CompactionRequested", got[0])
	}
	if req.AgentHandle != "dev" || req.Threshold != 2 || req.RawCount < 3 {
		t.Errorf("request = %+v", req)
	}
	if _, ok := got[1].(types.CompactionCompleted); !ok {
		t.Fatalf("second event = %T, want CompactionCompleted", got[1])
	}
}
