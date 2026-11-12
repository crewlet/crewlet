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

// compactorLoops is how many goroutines of the process are inside a
// compactor's loop — the one frame all three of its loops run in, so the count
// is of the loops running, whatever else an engine's boot and stop start and
// end around them.
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

// THE ENGINE STARTS AND STOPS EVERY ONE OF A GITLET NODE'S MAINTENANCE LOOPS: a
// Gitlet node's compactor runs its COMPACTION loop, its PUBLICATION loop and its
// EXPUNGE loop, each on a goroutine of its own; none lists anything while the
// node is built and not started — no listing of the Gitlet nodes carries a node
// whose host has not beaten, so it maintains nothing — and each lists at once
// through the router's code seam, `compaction_due`, `publication_due` and
// `expunge_due`, once the started node is listed; and the engine's stop ends
// all three, so no goroutine of any outlives the node.
//
// NOT PARALLEL, and read off the goroutines' own stacks rather than a settled
// count: a paused parallel test has started no engine, so the loops counted
// are this node's alone, and the loop's frame is the one all three share.
//
// Mutations: start the publication loop on a lifetime of its own rather than
// the compactor's, and a loop outlives the stop; start the compaction loop
// alone, and the running node lists no publication and no expunge; pass at
// once without waiting to be listed, and the node lists before it is started.
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

	// BUILT, NOT STARTED: all three loops run, and for two of their
	// rechecks none lists.
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
