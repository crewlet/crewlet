package host_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/statelog"
)

// awaitVerify is the verification id of name once it has ended, failing t if
// it never does.
func awaitVerify(t *testing.T, h *host.Server, name string, id uuid.UUID) host.Verification {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, ok := h.Verification(name, id)
		if !ok {
			t.Fatalf("the node keeps no verification %s of %s", id, name)
		}
		if v.State != host.VerifyRunning {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("the verification %s of %s never ended", id, name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdMaintainerOff admits a check of the case's repository of its own and
// holds the maintainer off the repository with it until release, or the case's
// end: the copy a case checks was just pushed and holds no commit-graph, so
// the maintainer's graph-first run on it is due at once, and a check's release
// lets the maintainer back on, whose run takes a maintenance slot at once.
// Unheld, that slot is the maintainer's as often as the case's next check's —
// and on a node of two process slots, whose maintenance lane is one, it is the
// maintainer's every time: the second of two verifies back to back is refused
// "no slot was free". The held check keeps one slot of the lane, so a case
// holding it runs with MaxProcesses four (a lane of two on every machine) and
// its own checks take the other; a maintainer run already in flight is left
// to finish first. It answers the lane's slot count and the held check's
// release, which may be called before the case's end.
func holdMaintainerOff(t *testing.T, c *codetest.Case) (slots int, release func()) {
	t.Helper()
	node := c.Fleet.Nodes()[0]
	slots = node.Pool().Slots(gitproc.ClassMaintenance)
	if slots != 2 {
		t.Fatalf("the maintenance lane holds %d slots, want 2: one for the held check, one for the case's", slots)
	}
	held, err := node.Cache().AdmitVerify(c.Rows().Repo.ID)
	if err != nil {
		t.Fatalf("the check held across the case was refused: %v", err)
	}
	t.Cleanup(held.Release) // a case that fails while it holds the check lets it go before the fleet stops
	deadline := time.Now().Add(time.Minute)
	for node.Pool().Free(gitproc.ClassMaintenance) != slots-1 {
		if time.Now().After(deadline) {
			t.Fatalf("the maintenance lane has %d of its %d slots free a minute after the held check's admission, "+
				"want all but its own", node.Pool().Free(gitproc.ClassMaintenance), slots)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return slots, held.Release
}

// A VERIFY NAMES A REPOSITORY THIS NODE HOLDS, AND ANSWERS FOR IT OR SAYS WHY
// NOT: a repository the node never served is no check — the check would build
// a copy, a whole download of it — and once a push has given the node a copy,
// that copy is checked against its rows and found whole, answered through the
// verification the start handed back. A name no repository is declared under
// and one the grammar refuses are both not found, a verification of another
// repository or an id the node never made is none, and a stopped host
// verifies nothing.
func TestAVerifyAnswersTheRepositoryItNames(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	h := c.Fleet.Nodes()[0].Host()
	if _, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{}); !errors.Is(err, cache.ErrNotHeld) {
		t.Fatalf("a verify of a repository this node never served answered %v, want cache.ErrNotHeld", err)
	}
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	started, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{})
	if err != nil {
		t.Fatalf("a verify of %s failed to start: %v", codetest.RepoName, err)
	}
	if started.State != host.VerifyRunning || started.Repo != codetest.RepoName || started.Repair {
		t.Fatalf("a verify started as %+v, want it running, named, no repair", started)
	}
	ended := awaitVerify(t, h, codetest.RepoName, started.ID)
	if ended.State != host.VerifyDone || ended.Err != nil || ended.Ended.Before(ended.Started) {
		t.Fatalf("the verify ended %+v, want done", ended)
	}
	if r := ended.Report; r.Name != codetest.RepoName || r.Packs != 1 || len(r.Problems) != 0 {
		t.Fatalf("a verify of a repository one push old answered %+v, want it named, one pack, no problem", r)
	}
	if _, ok := h.Verification("another", started.ID); ok {
		t.Error("the verification answered under another repository's name")
	}
	if _, ok := h.Verification(codetest.RepoName, uuid.Must(uuid.NewV7())); ok {
		t.Error("the node answered a verification it never made")
	}
	for _, name := range []string{"nobody-declared-this", "not a name"} {
		if _, err := h.StartVerify(c.Ctx, name, false, statelog.Position{}); !errors.Is(err, code.ErrRepoNotFound) {
			t.Errorf("a verify of %q answered %v, want code.ErrRepoNotFound", name, err)
		}
	}
	h.Stop(c.Ctx)
	if _, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{}); !errors.Is(err, host.ErrStopping) {
		t.Errorf("a verify of a stopped host answered %v, want host.ErrStopping", err)
	}
	if v, ok := h.Verification(codetest.RepoName, started.ID); !ok || v.State != host.VerifyDone {
		t.Errorf("a stopped host answered its ended verification %+v, %v; want it kept", v, ok)
	}
}

// A VERIFY OUTLIVES ITS REQUEST: the check reads every byte of a repository
// and a repair rebuilds it, which outlives a balancer's read timeout, whose
// close ends the request — so the check runs under the host's lifetime and
// never the request's, and the request that started it ending mid-check ends
// nothing: the check and its repair complete, and the verification reports
// both. Meanwhile a second asker of the same check shares it, and one asking
// for the other is refused naming it.
//
// Mutation: run the check under the request's context, and the verification
// fails cancelled with nothing repaired.
func TestAVerifyOutlivesItsRequest(t *testing.T) {
	var (
		holding atomic.Bool
		reached = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)
	e := &recording{state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
		if holding.Load() {
			once.Do(func() { close(reached) })
			select {
			case <-release:
			case <-ctx.Done():
				return code.RepoSnapshot{}, ctx.Err()
			}
		}
		return inner.RepoState(ctx, q)
	}}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	h := node.Host()
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	// A byte gone bad near the end of the one pack: the check finds it, and
	// the repair rebuilds the copy.
	packs, err := filepath.Glob(filepath.Join(node.CacheDir(), "repos", "*", "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("the node's copy holds packs %v (%v), want one", packs, err)
	}
	damagePack(t, packs[0])

	holding.Store(true)
	ctx, cancel := context.WithCancel(c.Ctx)
	started, err := h.StartVerify(ctx, codetest.RepoName, true, statelog.Position{})
	if err != nil {
		t.Fatalf("a repair failed to start: %v", err)
	}
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the check never reached its sync")
	}
	cancel()
	shared, err := h.StartVerify(c.Ctx, codetest.RepoName, true, statelog.Position{})
	if err != nil || shared.ID != started.ID {
		t.Errorf("a second asker of the same repair was handed %+v, %v; want the running %s", shared, err,
			started.ID)
	}
	var running *host.VerifyRunningError
	if _, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{}); !errors.As(err, &running) ||
		running.Running.ID != started.ID {
		t.Errorf("a check beside a running repair answered %v, want a VerifyRunningError naming %s", err,
			started.ID)
	}
	holding.Store(false)
	close(release)

	ended := awaitVerify(t, h, codetest.RepoName, started.ID)
	if ended.State != host.VerifyDone || ended.Err != nil {
		t.Fatalf("the repair its request left ended %+v, want done", ended)
	}
	if r := ended.Report; !r.Rebuilt || len(r.Found) == 0 || len(r.Problems) != 0 {
		t.Errorf("the repair reported %+v, want what it found and a rebuild that left no problem", r)
	}
}

// A VERIFY SAYS WHEN THE NODE ENDS ITS CHECK: while it brings its copy to the
// rows first it states no deadline — each read of the object store that sync
// makes is bounded on its own — and once its check pass begins the
// verification carries the instant the pass's bound ends it by
// (cache.CheckBudget over what the copy holds), which a caller waits by; an
// ended verification carries none.
//
// Mutation: drop the deadline the check pass is told, and the running
// check states none.
func TestAVerifySaysWhenItEndsItsCheck(t *testing.T) {
	var (
		holding atomic.Bool
		reached = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)
	e := &recording{state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
		// The check's read of the rows is at the floor of what it listed:
		// held there, the check is under way.
		if holding.Load() && q.Fresh.Level == statelog.ReadSession {
			once.Do(func() { close(reached) })
			select {
			case <-release:
			case <-ctx.Done():
				return code.RepoSnapshot{}, ctx.Err()
			}
		}
		return inner.RepoState(ctx, q)
	}}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
	h := c.Fleet.Nodes()[0].Host()
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	holding.Store(true)
	began := time.Now()
	started, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{})
	if err != nil {
		t.Fatalf("a verify failed to start: %v", err)
	}
	if !started.Deadline.IsZero() {
		t.Errorf("a verify not yet checking stated a deadline, %s", started.Deadline)
	}
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the check never read its rows")
	}
	checking, ok := h.Verification(codetest.RepoName, started.ID)
	if most := began.Add(cache.CheckBudget(1 << 30)); !ok || checking.State != host.VerifyRunning ||
		checking.Deadline.Before(began.Add(cache.CheckBudget(0))) || checking.Deadline.After(most) {
		t.Errorf("a verify checking its copy states %+v, %v; want it running, ending its check within the "+
			"bound of a small copy from its start", checking, ok)
	}
	holding.Store(false)
	close(release)
	if ended := awaitVerify(t, h, codetest.RepoName, started.ID); ended.State != host.VerifyDone ||
		!ended.Deadline.IsZero() {
		t.Errorf("the verify ended %+v, want done and stating no deadline", ended)
	}
}

// A VERIFY AT ITS CALLER'S FLOOR APPENDS NO BARRIER: named a position on the
// code log — a listing's, read at the head once for a whole sweep — the
// repository is resolved and its copy brought to that position, and a copy
// already there is checked as it is, so nothing of the verify reads at the
// log's head. Named none, the copy is brought to the head. A floor on another
// stream is no floor on a repository's rows, and is refused.
//
// THE MAINTAINER IS HELD OFF THE REPOSITORY ([holdMaintainerOff]): the case
// runs two verifies back to back, and the first one's release let the
// maintainer's graph-first run take the lane's one slot on a node of two
// process slots, so the second was refused "no slot was free" there every time.
// Held off, the maintainer reads nothing either, so every read recorded is a
// verify's own.
//
// Mutation: bring the copy to the head whatever the floor, and the floored
// verify reads through the barrier.
func TestAVerifyAtItsCallersFloorAppendsNoBarrier(t *testing.T) {
	e := &recording{}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}, MaxProcesses: 4},
		codetest.Settings())
	h := c.Fleet.Nodes()[0].Host()
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	holdMaintainerOff(t, c)
	ref, err := e.Estate.Resolve(c.Ctx, code.ResolveQuery{Name: codetest.RepoName,
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil {
		t.Fatal(err)
	}

	verify := func(floor statelog.Position) []call {
		t.Helper()
		e.mu.Lock()
		e.calls = nil
		e.mu.Unlock()
		started, err := h.StartVerify(c.Ctx, codetest.RepoName, false, floor)
		if err != nil {
			t.Fatalf("a verify at %v failed to start: %v", floor, err)
		}
		if ended := awaitVerify(t, h, codetest.RepoName, started.ID); ended.State != host.VerifyDone ||
			len(ended.Report.Problems) != 0 {
			t.Fatalf("a verify at %v ended %+v, want done and clean", floor, ended)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return slices.Clone(e.calls)
	}
	for _, call := range verify(ref.Position) {
		if call.level == statelog.ReadLinearizable {
			t.Errorf("a verify at its caller's floor %s read %s at the log's head", ref.Position, call.op)
		}
	}
	if head := verify(statelog.Position{}); !slices.ContainsFunc(head, func(c call) bool {
		return c.op == "state" && c.level == statelog.ReadLinearizable
	}) {
		t.Errorf("a verify naming no floor read %v, want its copy brought to the log's head", head)
	}
	if _, err := h.StartVerify(c.Ctx, codetest.RepoName, false,
		statelog.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 1}); !errors.Is(err, code.ErrInvalid) {
		t.Errorf("a verify at a floor on another stream answered %v, want code.ErrInvalid", err)
	}
}

// A VERIFY IS AWAITED WHERE IT WAS ASKED: the request that began it may wait
// for its end — VerifyHold, so a small copy's check is answered in that one
// request — and is answered it done, with everything the check held already
// let go: a caller that asks for the next check at once finds the
// maintenance slot free rather than being told to wait for it. A check that
// outlasts the wait is answered as it stands, running; an id the node does
// not keep, or another repository's, is none.
//
// THE LANE IS READ WITH THE MAINTAINER HELD OFF, by a check of the case's own
// admitted across the loop ([holdMaintainerOff], over the cache's
// holdMaintainer): the maintainer's graph-first run, due at once on a copy
// just pushed, takes a maintenance slot the moment a check lets go of the
// repository, so the lane's free count read after an answer was the
// maintainer's as often as the check's (one run in fifteen failed so). The
// held check keeps one slot of the lane's two (MaxProcesses four, so the lane
// is two on any machine), and every check of the loop must leave the other
// free.
//
// Mutation: record the end before the check lets go of its slot, and the
// next check can find the lane still held.
func TestAVerifyIsAwaitedWhereItWasAsked(t *testing.T) {
	var (
		holding atomic.Bool
		release = make(chan struct{})
	)
	e := &recording{state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
		if holding.Load() && q.Fresh.Level == statelog.ReadSession {
			select {
			case <-release:
			case <-ctx.Done():
				return code.RepoSnapshot{}, ctx.Err()
			}
		}
		return inner.RepoState(ctx, q)
	}}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}, MaxProcesses: 4},
		codetest.Settings())
	node := c.Fleet.Nodes()[0]
	h := node.Host()
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	slots, letGo := holdMaintainerOff(t, c)
	for i := range 20 {
		started, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{})
		if err != nil {
			t.Fatalf("verify %d failed to start: %v", i, err)
		}
		hold, cancel := context.WithTimeout(c.Ctx, 30*time.Second)
		ended, ok := h.AwaitVerification(hold, codetest.RepoName, started.ID)
		cancel()
		if !ok || ended.State != host.VerifyDone {
			t.Fatalf("verify %d was awaited as %+v, %v; want it done", i, ended, ok)
		}
		if free := node.Pool().Free(gitproc.ClassMaintenance); free != slots-1 {
			t.Fatalf("verify %d was answered done with %d of the maintenance lane's %d slots free, one of them "+
				"the held check's; want the answered check's let go first", i, free, slots)
		}
	}
	letGo()

	holding.Store(true)
	started, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{})
	if err != nil {
		t.Fatal(err)
	}
	hold, cancel := context.WithTimeout(c.Ctx, 50*time.Millisecond)
	running, ok := h.AwaitVerification(hold, codetest.RepoName, started.ID)
	cancel()
	if !ok || running.State != host.VerifyRunning {
		t.Errorf("a check outlasting the wait was awaited as %+v, %v; want it running", running, ok)
	}
	holding.Store(false)
	close(release)
	if _, ok := h.AwaitVerification(c.Ctx, "another", started.ID); ok {
		t.Error("a verification was awaited under another repository's name")
	}
	if _, ok := h.AwaitVerification(c.Ctx, codetest.RepoName, uuid.Must(uuid.NewV7())); ok {
		t.Error("the node awaited a verification it never made")
	}
	if ended := awaitVerify(t, h, codetest.RepoName, started.ID); ended.State != host.VerifyDone {
		t.Errorf("the held check ended %+v, want done", ended)
	}
}

// damagePack overwrites eight bytes just before the trailer of the pack at
// path — a blob's, which no connectivity check reads, so only the hash can
// find them.
func damagePack(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, info.Size()-28); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A VERIFY THE HOST'S STOP CUTS SHORT IS ANSWERED STOPPING. Stop ends every
// verify in flight through the host's lifetime, and what that leaves is the
// context's own `canceled` — which reads as the caller giving up, while the
// caller is owed what every request the node stopped serving is told (503,
// the stop's Retry-After). So a start the stop cut short in its resolve
// answers ErrStopping, and so does a check the stop cut short, recorded on
// its verification beside what it had found. A start its OWN caller ended is
// no stop's, and keeps its error.
//
// Mutation: return the work's error unwrapped, and the stop's cases go red.
func TestAVerifyTheStopCutsShortIsAnsweredStopping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		byStop   bool
		stopping bool
	}{
		{"the host stops under its start", true, true},
		{"its own caller gives up its start", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				holding atomic.Bool
				reached = make(chan struct{})
				once    sync.Once
			)
			e := &recording{resolve: func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error) {
				if holding.Load() {
					once.Do(func() { close(reached) })
					<-ctx.Done()
					return code.RepoRef{}, ctx.Err()
				}
				return inner.Resolve(ctx, q)
			}}
			c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
			h := c.Fleet.Nodes()[0].Host()
			holding.Store(true)
			ctx, cancel := context.WithCancel(c.Ctx)
			defer cancel()
			answered := make(chan error, 1)
			go func() {
				_, err := h.StartVerify(ctx, codetest.RepoName, false, statelog.Position{})
				answered <- err
			}()
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatal("the verify never reached its resolve")
			}
			if tc.byStop {
				h.Stop(c.Ctx)
			} else {
				cancel()
			}
			var err error
			select {
			case err = <-answered:
			case <-time.After(10 * time.Second):
				t.Fatal("the verify never returned once it was cut short")
			}
			if got := errors.Is(err, host.ErrStopping); got != tc.stopping {
				t.Errorf("the verify answered %v: stopping %v, want %v", err, got, tc.stopping)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the verify answered %v, which no longer says what cut it", err)
			}
		})
	}

	t.Run("the host stops under its check", func(t *testing.T) {
		var (
			holding atomic.Bool
			reached = make(chan struct{})
			once    sync.Once
		)
		e := &recording{state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
			if holding.Load() {
				once.Do(func() { close(reached) })
				<-ctx.Done()
				return code.RepoSnapshot{}, ctx.Err()
			}
			return inner.RepoState(ctx, q)
		}}
		c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
		h := c.Fleet.Nodes()[0].Host()
		p := codetest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
		c.MustPush(p, work, "main")
		holding.Store(true)
		started, err := h.StartVerify(c.Ctx, codetest.RepoName, false, statelog.Position{})
		if err != nil {
			t.Fatalf("a verify failed to start: %v", err)
		}
		select {
		case <-reached:
		case <-time.After(10 * time.Second):
			t.Fatal("the check never reached its sync")
		}
		h.Stop(c.Ctx)
		ended, ok := h.Verification(codetest.RepoName, started.ID)
		switch {
		case !ok:
			t.Fatal("the stopped host kept no record of the check it cut short")
		case ended.State != host.VerifyFailed || !errors.Is(ended.Err, host.ErrStopping) ||
			!errors.Is(ended.Err, context.Canceled):
			t.Errorf("the check the stop cut short ended %+v, want failed, stopping, saying what cut it", ended)
		case ended.Report.Repo == "" || ended.Report.Name != codetest.RepoName:
			t.Errorf("the check the stop cut short reported %+v, want the repository named", ended.Report)
		}
	})
}
