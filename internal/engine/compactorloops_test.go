package engine_test

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/gitlet"
	gitletcompact "github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
)

// listings is a Gitlet node's compactor's code seam, counting the due listings
// each of its loops reads through it.
type listings struct {
	gitletcompact.Estate
	mu    sync.Mutex
	kinds map[gitlet.ReadKind]int
}

func (l *listings) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	l.mu.Lock()
	l.kinds[q.Kind]++
	l.mu.Unlock()
	return l.Estate.Read(ctx, q)
}

// listed reports whether a loop has read the listing of kind k.
func (l *listings) listed(k gitlet.ReadKind) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.kinds[k] > 0
}

// compactorLoops counts the process's goroutines inside a compactor's loop,
// the one frame all three of its loops run in.
func compactorLoops() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte("internal/gitlet/compact.(*Compactor).loop("))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// THE ENGINE STARTS AND STOPS EVERY ONE OF A GITLET NODE'S MAINTENANCE LOOPS:
// the compactor's COMPACTION, PUBLICATION and EXPUNGE loops each run on their
// own goroutine, list nothing while the node is built and not yet listed among
// the Gitlet nodes, each list their `*_due` listing once it is started, and all
// end with the engine's stop.
//
// NOT PARALLEL: a paused parallel test has started no engine, so the loops
// counted off the stacks are this node's alone.
//
// Mutations: start the publication loop on its own lifetime (it outlives the
// stop); start only the compaction loop (no publication or expunge listed);
// skip waiting to be listed (it lists before it is started).
func TestTheEngineStartsAndStopsEveryMaintenanceLoop(t *testing.T) {
	b := gitletNodeBootstrap(t, nil)
	engine.RequireNodeGit(t, b, parsedCompany(t, gitCompany))
	before := compactorLoops()
	seam := &listings{kinds: map[gitlet.ReadKind]int{}}
	e, err := engine.New(t.Context(), engine.WithCompactorEstate(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     gitlettest.HookCommand(t),
	}, func(inner gitletcompact.Estate) gitletcompact.Estate {
		seam.Estate = inner
		return seam
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			e.Stop(context.WithoutCancel(t.Context()))
		}
	})

	// BUILT, NOT STARTED: all three run, and none lists for two rechecks.
	waitFor(t, "the compactor's three loops to run", func() bool { return compactorLoops()-before == 3 })
	time.Sleep(2 * gitletcompact.ListedRecheck)
	for _, kind := range []gitlet.ReadKind{gitlet.ReadCompactionDue, gitlet.ReadPublicationDue, gitlet.ReadExpungeDue} {
		if seam.listed(kind) {
			t.Errorf("a Gitlet node built and not started read the %s listing, want its loops waiting to be "+
				"listed among the Gitlet nodes", kind)
		}
	}

	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "each of the compactor's loops to list once", func() bool {
		return seam.listed(gitlet.ReadCompactionDue) && seam.listed(gitlet.ReadPublicationDue) &&
			seam.listed(gitlet.ReadExpungeDue)
	})
	if got := compactorLoops() - before; got != 3 {
		t.Errorf("a running Gitlet node runs %d of its compactor's loops, want 3: the compaction, the "+
			"publication and the expunge", got)
	}

	e.Stop(context.WithoutCancel(t.Context()))
	stopped = true
	deadline := time.Now().Add(10 * time.Second)
	for compactorLoops() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d of the compactor's loops outlived the engine's stop", compactorLoops()-before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
