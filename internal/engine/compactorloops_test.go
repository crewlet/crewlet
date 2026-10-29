package engine_test

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	codecompact "github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/engine"
)

// listings is a git node's compactor's code seam, counting the due listings
// each of its loops reads through it.
type listings struct {
	codecompact.Estate
	mu    sync.Mutex
	kinds map[code.ReadKind]int
}

func (l *listings) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	l.mu.Lock()
	l.kinds[q.Kind]++
	l.mu.Unlock()
	return l.Estate.Read(ctx, q)
}

// listed reports whether a loop has read the listing of kind k.
func (l *listings) listed(k code.ReadKind) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.kinds[k] > 0
}

// compactorLoops is how many goroutines of the process are inside a
// compactor's loop — the one frame both of its loops run in, so the count is
// of the loops running, whatever else an engine's boot and stop start and end
// around them.
func compactorLoops() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte("internal/code/compact.(*Compactor).loop("))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// THE ENGINE STARTS AND STOPS BOTH OF A GIT NODE'S MAINTENANCE LOOPS: a git
// node's compactor runs its COMPACTION loop and its PUBLICATION loop, each on a
// goroutine of its own; neither lists anything while the node is built and not
// started — no listing of the git nodes carries a node whose host has not
// beaten, so it maintains nothing — and each lists at once through the
// router's code seam, `compaction_due` and `publication_due`, once the started
// node is listed; and the engine's stop ends both, so no goroutine of either
// outlives the node.
//
// NOT PARALLEL, and read off the goroutines' own stacks rather than a settled
// count: a paused parallel test has started no engine, so the loops counted
// are this node's alone, and the loop's frame is the one both share.
//
// Mutations: start the publication loop on a lifetime of its own rather than
// the compactor's, and a loop outlives the stop; start the compaction loop
// alone, and the running node lists no publication; pass at once without
// waiting to be listed, and the node lists before it is started.
func TestTheEngineStartsAndStopsBothCompactionLoops(t *testing.T) {
	b := gitNodeBootstrap(t, nil)
	engine.RequireNodeGit(t, b, parsedCompany(t, gitCompany))
	before := compactorLoops()
	seam := &listings{kinds: map[code.ReadKind]int{}}
	e, err := engine.New(t.Context(), engine.WithCompactorEstate(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     codetest.HookCommand(t),
	}, func(inner codecompact.Estate) codecompact.Estate {
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

	// BUILT, NOT STARTED: both loops run, and for two of their rechecks
	// neither lists.
	waitFor(t, "both of the compactor's loops to run", func() bool { return compactorLoops()-before == 2 })
	time.Sleep(2 * codecompact.ListedRecheck)
	for _, kind := range []code.ReadKind{code.ReadCompactionDue, code.ReadPublicationDue} {
		if seam.listed(kind) {
			t.Errorf("a git node built and not started read the %s listing, want its loops waiting to be "+
				"listed among the git nodes", kind)
		}
	}

	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "both of the compactor's loops to list once", func() bool {
		return seam.listed(code.ReadCompactionDue) && seam.listed(code.ReadPublicationDue)
	})
	if got := compactorLoops() - before; got != 2 {
		t.Errorf("a running git node runs %d of its compactor's loops, want 2: the compaction and the "+
			"publication", got)
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
