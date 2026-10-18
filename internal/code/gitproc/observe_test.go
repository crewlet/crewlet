package gitproc_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// A RESULT SAYS WHEN ITS PROCESS BEGAN: after its slot was granted — a run
// waiting behind a held lane began only once the lane let it in — and before
// everything the process did, so a run that sleeps 200 ms began at least that
// long before Run returned. A caller orders it against a time.Now() of its
// own: the cache spares a file it rewrote after a report's process began.
//
// Mutation: take Began at the reap — the run began less than 200 ms before it
// returned; take it before the lane's wait — it began before the holder let go.
func TestAResultSaysWhenItsProcessBegan(t *testing.T) {
	t.Parallel()
	p := fakePool(t, `case "$1" in hold) `+holdBody+`;; sleepy) sleep 0.2 ;; esac`, gitproc.Options{MaxProcesses: 1})
	called := time.Now()
	h := hold(t, p, gitproc.ClassRead)
	done := start(t.Context(), p, gitproc.Cmd{Args: []string{"sleepy"}, Class: gitproc.ClassRead, Deadline: time.Minute})
	time.Sleep(100 * time.Millisecond)
	released := time.Now()
	h.end(t)
	o := await(t, done, "a run behind a held lane")
	returned := time.Now()
	if o.err != nil {
		t.Fatalf("the run: %v", o.err)
	}
	switch {
	case !o.res.Began.After(called):
		t.Errorf("Began %v is not after the call at %v", o.res.Began, called)
	case !o.res.Began.After(released):
		t.Errorf("Began %v is before the held lane let the run in at %v", o.res.Began, released)
	case returned.Sub(o.res.Began) < 200*time.Millisecond:
		t.Errorf("Began %v is %v before Run returned, less than the process slept", o.res.Began,
			returned.Sub(o.res.Began))
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
// git in, or a lane with no slot. A second observer is refused while one is
// installed, and accepted once the first is removed.
//
// Mutations: observe only failed runs — the answered run is missed; observe in
// Pool.Run alone — the reservation's run is missed; report a call Run refuses
// (its prepare's or its admission's error) — the shape's or the busy lane's
// call is seen; unobserve doing nothing — the second observer is refused.
func TestEveryRunIsObserved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	p := realPool(t, gitproc.Options{MaxProcesses: 1})
	gitproc.SetTimings(p, 200*time.Millisecond, time.Second)
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
	// Ended at its deadline: a batch reader whose input never comes, and
	// named an alternate it ran beside.
	batch, _ := f.bare(t)
	blocked := &blockedReader{release: make(chan struct{})}
	c := read(batch, "cat-file", "--batch")
	c.Loads = gitproc.PartMultiPackIndex
	c.Stdin, c.Deadline = blocked, 300*time.Millisecond
	go func() { time.Sleep(2 * time.Second); close(blocked.release) }()
	if _, err := p.Run(t.Context(), c); !errors.Is(err, gitproc.ErrDeadline) {
		t.Fatalf("cat-file --batch past its deadline: %v, want ErrDeadline", err)
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
