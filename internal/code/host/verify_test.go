package host_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
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
	if _, err := h.StartVerify(c.Ctx, codetest.RepoName, false); !errors.Is(err, cache.ErrNotHeld) {
		t.Fatalf("a verify of a repository this node never served answered %v, want cache.ErrNotHeld", err)
	}
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	started, err := h.StartVerify(c.Ctx, codetest.RepoName, false)
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
		if _, err := h.StartVerify(c.Ctx, name, false); !errors.Is(err, code.ErrRepoNotFound) {
			t.Errorf("a verify of %q answered %v, want code.ErrRepoNotFound", name, err)
		}
	}
	h.Stop(c.Ctx)
	if _, err := h.StartVerify(c.Ctx, codetest.RepoName, false); !errors.Is(err, host.ErrStopping) {
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
	started, err := h.StartVerify(ctx, codetest.RepoName, true)
	if err != nil {
		t.Fatalf("a repair failed to start: %v", err)
	}
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the check never reached its sync")
	}
	cancel()
	shared, err := h.StartVerify(c.Ctx, codetest.RepoName, true)
	if err != nil || shared.ID != started.ID {
		t.Errorf("a second asker of the same repair was handed %+v, %v; want the running %s", shared, err,
			started.ID)
	}
	var running *host.VerifyRunningError
	if _, err := h.StartVerify(c.Ctx, codetest.RepoName, false); !errors.As(err, &running) ||
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
				_, err := h.StartVerify(ctx, codetest.RepoName, false)
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
		started, err := h.StartVerify(c.Ctx, codetest.RepoName, false)
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
