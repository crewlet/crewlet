package nodebudget_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/nodebudget"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/store"
)

// THE LIMIT IS NINE TENTHS OF WHAT THE NODE HOLDS OUTSIDE THE RUNTIME LEAVES,
// never of its whole memory, and none where nothing is left.
//
// Mutations, each red: limit the node's whole memory; leave no tenth; a limit
// at or below nothing.
func TestTheLimitIsNineTenthsOfWhatIsHeldOutsideLeaves(t *testing.T) {
	t.Parallel()
	const gib, mib = int64(1) << 30, int64(1) << 20
	for _, tc := range []struct {
		memory, outside, want int64
	}{
		{2 * gib, 0, 2 * gib / 10 * 9},
		{4 * gib, 4*224*mib + 512*mib, (4*gib - 4*224*mib - 512*mib) / 10 * 9},
		{2 * gib, 2 * gib, 0},
		{2 * gib, 3 * gib, 0},
	} {
		if got := nodebudget.Limit(tc.memory, tc.outside); got != tc.want {
			t.Errorf("a %d-byte node holding %d outside the runtime: limit %d, want %d", tc.memory, tc.outside,
				got, tc.want)
		}
	}
}

// A NODE'S LINES ARE ITS POOLS AND ITS ANSWER BUDGET OVER NINE TENTHS, ITS PAGE
// CACHE AND ITS READ LANE: the runtime's lines keep the limit at the pools and
// the answers in flight where the node holds what it keeps, the page cache is
// sized by the memory only where the node sizes it so, and the read lane is
// its slots at gitproc.ReadMemory. What it keeps is every line; what it holds
// outside the runtime is the page cache and the read lane.
//
// Mutations, each red: count the pools without the tenth; count the answer
// budget without the tenth; size the page cache by the memory where the node
// keeps the floor; count one read slot; leave a line out of what the node
// keeps or holds outside.
func TestANodesLinesAreItsPoolsItsPageCacheAndItsReadLane(t *testing.T) {
	t.Parallel()
	const memory, conns, reads = int64(8) << 30, 32, 4
	work := executor.NodeWork{Boxes: true, Turns: 32, PeerReads: 64, PersonReads: true}
	for _, sized := range []bool{true, false} {
		l := nodebudget.Node{Work: work, Conns: conns, PageCacheSized: sized}.Of(memory, reads)
		sizedBy := int64(0)
		if sized {
			sizedBy = memory
		}
		if want := work.MemoryPools(memory) * 10 / 9; l.Runtime != want {
			t.Errorf("the runtime's line is %d, want the pools over nine tenths, %d", l.Runtime, want)
		}
		if want := int64(queue.AnswerBudgetBytes) * 10 / 9; l.Answers != want {
			t.Errorf("the answers' line is %d, want the answer budget over nine tenths, %d", l.Answers, want)
		}
		if want := store.PageCacheShare(sizedBy, conns); l.PageCache != want {
			t.Errorf("sized by the memory %v: the page cache is %d, want %d", sized, l.PageCache, want)
		}
		if want := reads * gitproc.ReadMemory; l.ReadLane != want {
			t.Errorf("the read lane is %d, want %d slots at %d", l.ReadLane, reads, gitproc.ReadMemory)
		}
		if l.Kept() != l.Runtime+l.Answers+l.PageCache+l.ReadLane || l.Outside() != l.PageCache+l.ReadLane {
			t.Errorf("lines %+v: kept %d, outside %d, want every line and the page cache with the read lane",
				l, l.Kept(), l.Outside())
		}
	}
}
