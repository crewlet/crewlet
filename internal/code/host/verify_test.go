package host_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
)

// A VERIFY NAMES A REPOSITORY, AND ANSWERS FOR IT OR SAYS WHY NOT: a declared
// repository's cache is checked against its rows and found whole, a name no
// repository is declared under and one the grammar refuses are both not found,
// and a stopped host verifies nothing.
func TestAVerifyAnswersTheRepositoryItNames(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	h := c.Fleet.Nodes()[0].Host()

	report, err := h.VerifyRepo(c.Ctx, codetest.RepoName, false)
	if err != nil {
		t.Fatalf("a verify of %s failed: %v", codetest.RepoName, err)
	}
	if report.Name != codetest.RepoName || report.Packs != 1 || len(report.Problems) != 0 {
		t.Fatalf("a verify of a repository one push old answered %+v, want it named, one pack, no problem",
			report)
	}
	for _, name := range []string{"nobody-declared-this", "not a name"} {
		if _, err := h.VerifyRepo(c.Ctx, name, false); !errors.Is(err, code.ErrRepoNotFound) {
			t.Errorf("a verify of %q answered %v, want code.ErrRepoNotFound", name, err)
		}
	}
	h.Stop(c.Ctx)
	if _, err := h.VerifyRepo(c.Ctx, codetest.RepoName, false); !errors.Is(err, host.ErrStopping) {
		t.Errorf("a verify of a stopped host answered %v, want host.ErrStopping", err)
	}
}

// A VERIFY THE HOST'S STOP CUTS SHORT IS ANSWERED STOPPING. Stop ends every
// verify in flight through the host's lifetime, and what that leaves is the
// context's own `canceled` — which reads as the caller giving up, while the
// caller is still waiting to be told the node stopped serving it (503, the
// stop's Retry-After). A verify its OWN caller ended is no stop's, and keeps
// its error.
//
// Mutation: return the work's error unwrapped, and the first half goes red.
func TestAVerifyTheStopCutsShortIsAnsweredStopping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		byStop   bool
		stopping bool
	}{
		{"the host stops under it", true, true},
		{"its own caller gives up", false, false},
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
				_, err := h.VerifyRepo(ctx, codetest.RepoName, false)
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
}
