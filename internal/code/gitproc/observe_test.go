package gitproc_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A RESULT SAYS WHEN ITS PROCESS BEGAN: after its slot was granted — a run
// waiting behind a held lane began only once the lane let it in — and before
// everything the process did. A caller orders it against a time.Now() of its
// own: the cache spares a file it rewrote after a report's process began.
//
// The pool reads Began from a clock the case steps (gitproc.SetClock), so
// each claim is an instant the case stated rather than two readings of the
// wall clock, which a coarse clock stamps equal and a strict order then
// fails: the clock reads queued while the run waits behind the held lane
// (gitproc.Waiting), admitted from just before the case lets the lane go, and
// running once the processes gauge counts the run's process, which it does
// only after the process has started.
//
// Mutation: take Began at the reap — it reads running; take it before the
// lane's wait — it reads queued.
func TestAResultSaysWhenItsProcessBegan(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	p := fakePool(t, `case "$1" in hold) `+holdBody+`;; esac`, gitproc.Options{MaxProcesses: 1, Metrics: rec})
	queued := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	admitted, running := queued.Add(time.Minute), queued.Add(2*time.Minute)
	var clock atomic.Pointer[time.Time]
	clock.Store(&queued)
	gitproc.SetClock(p, func() time.Time { return *clock.Load() })

	held := hold(t, p, gitproc.ClassRead)
	dir := t.TempDir()
	run := holder{started: filepath.Join(dir, "started"), release: filepath.Join(dir, "release")}
	run.done = start(t.Context(), p, gitproc.Cmd{Args: []string{"hold", run.started, run.release},
		Class: gitproc.ClassRead, Deadline: time.Minute})
	awaitWaiting(t, p, gitproc.ClassRead, 1) // queued behind the held lane while the clock reads queued
	clock.Store(&admitted)
	held.end(t)
	awaitGauge(t, rec, metrics.CodeGitProcesses, "read", 1) // its process has started
	clock.Store(&running)
	o := run.end(t)
	if o.err != nil {
		t.Fatalf("the run: %v", o.err)
	}
	switch began := o.res.Began; {
	case began.Equal(queued):
		t.Errorf("Began reads %v, the clock while the run waited behind the held lane: it was taken before the lane let the run in",
			began)
	case began.Equal(running):
		t.Errorf("Began reads %v, the clock once the process was running: it was taken after the process began", began)
	case !began.Equal(admitted):
		t.Errorf("Began reads %v, want %v: the clock between the lane letting the run in and its process starting",
			began, admitted)
	}
}

// A PROCESS THAT NEVER STARTED IS NOT OBSERVED. A run admitted to its slot
// whose git cannot be started — its binary gone from under the pool — names
// nothing about any repository, and an observer handed it would read a
// Result with no Began as a report from before every write.
//
// Mutation: report whatever Began says (the observer sees the run).
func TestAProcessThatNeverStartedIsNotObserved(t *testing.T) {
	t.Parallel()
	o := gitproc.Options{MaxProcesses: 1, Home: filepath.Join(t.TempDir(), "home")}
	o.Path = gittest.Fake(t, fakeVersion, `exit 0`)
	p := newPool(t, o)
	var seen []gitproc.Ran
	var mu sync.Mutex
	unobserve, err := p.Observe(func(r gitproc.Ran) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unobserve()
	if err := os.Remove(o.Path); err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"rev-list", "--all"}, Class: gitproc.ClassRead,
		Deadline: time.Minute})
	if err == nil || !res.Began.IsZero() {
		t.Fatalf("a run whose git is gone answered %v, began %v: the case wants a process that never started",
			err, res.Began)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Errorf("the observer saw %d runs of a process that never started", len(seen))
	}
}

// EVERY RUN IS OBSERVED, AND ONLY A RUN. The pool's one observer sees what
// every run whose process started ran and how it ended — one that answered
// over damage it reported (exit 0, the damage named), one that failed, one the
// pool ended at its deadline, and one in a reservation's slot — and nothing of
// a call refused before a process started: a directory the pool will not run
// git in, or a lane with no slot. Each is observed with what its Cmd declared
// of it — the files it loads, and the caller that repairs it. A second
// observer is refused while one is installed, and accepted once the first is
// removed.
//
// Mutations: observe only failed runs — the answered run is missed; observe in
// Pool.Run alone — the reservation's run is missed; report a call Run refuses
// (its prepare's or its admission's error) — the shape's or the busy lane's
// call is seen; leave Cmd.Repairer off the Ran — the batch run is observed
// repaired by nobody; unobserve doing nothing — the second observer is
// refused; end no run at its deadline — the batch reader, whose input ends
// only in its drain, never answers.
func TestEveryRunIsObserved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{MaxProcesses: 1})
	gitproc.SetTimings(p, 200*time.Millisecond, time.Second)
	// The batch reader's input, below, ends only once the drain waits for it:
	// after its deadline has ended the process, whatever the runner's load.
	// Ended on a timer instead, a runner slow to start the process let the
	// input end first, and the batch reader answered before its deadline.
	blocked := &blockedReader{release: make(chan struct{})}
	gitproc.OnDrain(p, func(stream string) {
		if stream == "stdin" {
			close(blocked.release)
		}
	})
	var (
		mu   sync.Mutex
		seen []gitproc.Ran
	)
	unobserve, err := p.Observe(func(r gitproc.Ran) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r)
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	took := func() []gitproc.Ran {
		mu.Lock()
		defer mu.Unlock()
		s := seen
		seen = nil
		return s
	}

	// Answered over damage it named: a truncated multi-pack-index read around.
	served := gittest.Served(t, f.Fixture)
	gittest.Damage(t, served.Dir, gittest.MidxTruncated)
	if _, err := p.Run(t.Context(), read(served.Dir, "rev-list", "--objects", f.Tip())); err != nil {
		t.Fatalf("rev-list over a truncated index: %v", err)
	}
	// Failed: a revision no repository holds. Each run below has a directory
	// of its own, which is how the observer's reports are told apart.
	dir, _ := f.bare(t)
	if _, err := p.Run(t.Context(), read(dir, "rev-list", "no-such-revision")); err == nil {
		t.Fatal("rev-list of a revision that is no object answered")
	}
	// Ended at its deadline: a batch reader whose input never comes before
	// its drain, run for a caller that repairs its failure.
	batch, _ := f.bare(t)
	c := read(batch, "cat-file", "--batch")
	c.Loads, c.Repairer = gitproc.PartMultiPackIndex, 7
	c.Stdin, c.Deadline = blocked, 300*time.Millisecond
	if o := await(t, start(t.Context(), p, c), "cat-file --batch past its deadline"); !errors.Is(o.err, gitproc.ErrDeadline) {
		t.Fatalf("cat-file --batch past its deadline: %v, want ErrDeadline", o.err)
	}
	// In a reservation's slot — and, while it holds the lane's one slot, a
	// maintenance run refused busy.
	r, err := p.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	busy, _ := f.bare(t)
	if _, err := p.Run(t.Context(), maintenance(busy, "fsck", "--connectivity-only")); !errors.Is(err,
		gitproc.ErrBusy) {
		t.Fatalf("a maintenance run beside the reservation: %v, want ErrBusy", err)
	}
	reserved, _ := f.bare(t)
	if _, err := r.Run(t.Context(), maintenance(reserved, "fsck", "--connectivity-only", "--no-progress")); err != nil {
		t.Fatalf("fsck in the reservation: %v", err)
	}
	r.Release()
	// Refused for its shape: a config the engine never writes.
	shaped, _ := f.bare(t)
	if err := os.WriteFile(filepath.Join(shaped, "config"), []byte("[core]\n\tbare = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(t.Context(), read(shaped, "rev-list", "--all")); !errors.Is(err, gitproc.ErrRepoShape) {
		t.Fatalf("a run in a directory of another shape: %v, want ErrRepoShape", err)
	}

	got := took()
	var dirs []string
	for _, r := range got {
		dirs = append(dirs, r.Dir)
	}
	if want := []string{served.Dir, dir, batch, reserved}; !slices.Equal(dirs, want) {
		t.Fatalf("the observer saw runs in %q, want %q: every run whose process started, and no refused call",
			dirs, want)
	}
	var d *gitproc.Damage
	if !errors.As(got[0].Result.Damage, &d) || d.Parts != gitproc.PartMultiPackIndex || got[0].Result.Exit != 0 {
		t.Errorf("the answered run was observed with exit %d and damage %v, want exit 0 naming the index",
			got[0].Result.Exit, got[0].Result.Damage)
	}
	if got[2].Loads != gitproc.PartMultiPackIndex || got[0].Loads != 0 {
		t.Errorf("the runs were observed loading %v and %v, want what each Cmd declared", got[0].Loads, got[2].Loads)
	}
	if got[2].Repairer != 7 || got[0].Repairer != 0 {
		t.Errorf("the runs were observed repaired by %d and %d, want what each Cmd named", got[0].Repairer,
			got[2].Repairer)
	}
	for _, r := range got {
		if r.Result.Began.IsZero() {
			t.Errorf("observed run in %s has no Began", r.Dir)
		}
	}

	if _, err := p.Observe(func(gitproc.Ran) {}); !errors.Is(err, gitproc.ErrInvalid) {
		t.Errorf("a second Observe while one is installed: %v, want ErrInvalid", err)
	}
	unobserve()
	unobserve()
	again, err := p.Observe(func(gitproc.Ran) {})
	if err != nil {
		t.Fatalf("Observe once the first is removed: %v", err)
	}
	again()
	if _, err := p.Run(t.Context(), read(dir, "rev-list", "--count", f.Tip())); err != nil {
		t.Fatal(err)
	}
	if got := took(); len(got) != 0 {
		t.Errorf("a removed observer still saw %d runs", len(got))
	}
}
