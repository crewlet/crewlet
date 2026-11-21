package engine

import (
	"fmt"
	"math"
	"runtime/metrics"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/nodememory"
	"github.com/crewlet/crewlet/internal/store"
)

// limitsSet records each memory limit an engine sets, in place of the runtime.
type limitsSet struct {
	mu     sync.Mutex
	limits []int64
}

func (l *limitsSet) set(n int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = append(l.limits, n)
	return math.MaxInt64
}

func (l *limitsSet) seen() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.limits)
}

func (l *limitsSet) option(o *Options) { o.setMemoryLimit = l.set }

// runtimeOverhead is what this test binary's runtime holds outside its heap's
// objects and spans (its metadata, its stacks and the rest): what a limit must
// leave past the pools for the runtime itself. Never total less live, which in
// a binary running many engines at once counts their garbage too.
func runtimeOverhead() int64 {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/free:bytes"}, {Name: "/memory/classes/heap/released:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"}}
	metrics.Read(s)
	n := int64(s[0].Value.Uint64())
	for _, heap := range s[1:] {
		n -= int64(heap.Value.Uint64())
	}
	return n
}

// A GITLET NODE'S RESERVE AND ITS GO MEMORY LIMIT KEEP THE SAME LINES, at every
// shape the concept page's table sizes that holds what it keeps: Gitlet keeps
// the runtime's share, the page cache and the read lane from its background
// work, the limit takes the page cache and the read lane out beside git's
// transfers and that budget, and so the limit holds the executor's pools with
// the heap's room and the runtime's own overhead.
//
// Mutations, each red: drop the read lane, the page cache or the tenth from
// what Gitlet keeps, or the read lane or the page cache from what the limit
// takes out; size the limit by the node's whole memory.
func TestAGitletNodesReserveAndLimitKeepTheSameLines(t *testing.T) {
	t.Parallel()
	every, withoutSeats := []string(nil), []string{"data", "gitlet", "ingress", "workers"}
	for _, shape := range []struct {
		roles  []string
		cores  int
		memory int64
	}{
		{withoutSeats, 1, 2 << 30}, {every, 2, 4 << 30}, {every, 4, 8 << 30}, {withoutSeats, 8, 16 << 30},
	} {
		t.Run(fmt.Sprintf("%d cores %d bytes roles %v", shape.cores, shape.memory, shape.roles), func(t *testing.T) {
			t.Parallel()
			var l limitsSet
			e := bootSizedCodeNode(t, shape.roles, shape.memory, l.option, func(o *Options) {
				o.Bootstrap.Gitlet.MaxProcesses = shape.cores
			})
			n := e.native.Load()
			if n == nil || n.gitCache == nil {
				t.Fatalf("the premise: the node built no git cache")
			}
			boot := testBootstrap(t)
			boot.Node.Roles = shape.roles
			lines := e.budgetNode(&boot).Of(shape.memory, shape.cores)
			kept := lines.Kept()
			transfers := TestGitTransfers * gitproc.TransferMemory
			left := max(0, shape.memory-kept)
			serving := min(int64(shape.cores)*cache.ServingMemoryPerCore,
				left/(gitproc.CoreMemory>>20)*(cache.ServingMemoryPerCore>>20))
			budget := n.gitCache.BackgroundBudget()
			if want := max(0, shape.memory-kept-transfers-serving); budget != want {
				t.Errorf("the background budget is %d, want %d: the memory less every line (%+v), the transfers "+
					"and the serving share", budget, want, lines)
			}
			limit := (shape.memory - transfers - budget - lines.PageCache - lines.ReadLane) / 10 * 9
			if got := l.seen(); !slices.Equal(got, []int64{limit}) {
				t.Errorf("the node set limits %v, want [%d]: nine tenths of the memory less the transfers, the "+
					"budget, the page cache and the read lane", got, limit)
			}
			pools := gitWork(boot.Profile("node-a"), &boot).MemoryPools(shape.memory)
			if overhead := runtimeOverhead(); limit < pools+overhead {
				t.Errorf("the limit %d is short of the pools %d with the runtime's overhead %d", limit, pools,
					overhead)
			}
		})
	}
}

// A NODE WITHOUT GITLET LIMITS ITS HEAP TO NINE TENTHS OF ITS MEMORY LESS ITS
// PAGE CACHE, read as the node's (nodememory), since no git process shares it.
//
// Mutations, each red: set no limit where no Gitlet runs; leave the page
// cache in.
func TestANodeWithoutGitletLimitsItsHeapByItsMemory(t *testing.T) {
	t.Parallel()
	m, err := nodememory.Read()
	if err != nil {
		t.Fatalf("the premise: the machine's memory reads: %v", err)
	}
	var l limitsSet
	roles := []string{"data", "ingress", "seats", "workers"}
	bootSizedCodeNode(t, roles, 4<<30, l.option)
	boot := testBootstrap(t)
	boot.Node.Roles = roles
	cached := store.PageCacheShare(0, storeOptions(&boot, nil, 0).NodeConnections(heldLogs(&boot)))
	if got, want := l.seen(), []int64{(m.Bytes - cached) / 10 * 9}; !slices.Equal(got, want) {
		t.Errorf("a node without the gitlet role set limits %v, want %v (nine tenths of %d less its %d of "+
			"page cache)", got, want, m.Bytes, cached)
	}
}

// AN EXPLICIT GOMEMLIMIT WINS, "off" included: the engine sets none.
//
// Mutation: set the limit whatever GOMEMLIMIT says.
func TestAnExplicitGOMEMLIMITWins(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "off")
	var l limitsSet
	bootSizedCodeNode(t, nil, 4<<30, l.option)
	if got := l.seen(); len(got) != 0 {
		t.Errorf("with GOMEMLIMIT=off the engine set limits %v, want none", got)
	}
}
