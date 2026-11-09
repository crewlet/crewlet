package worklet

import (
	"fmt"
	"testing"
)

// fanGraph is two held roots that share one downstream of n items, each of
// which waits on both roots, with the reads of each item's waiters counted.
func fanGraph(n int) (func(string) ([]waiter, error), *int) {
	reads := 0
	return func(id string) ([]waiter, error) {
		reads++
		if id != "root-a" && id != "root-b" {
			return nil, nil
		}
		out := make([]waiter, n)
		for i := range out {
			out[i] = waiter{id: fmt.Sprintf("w-%03d", i), assignee: "pm"}
		}
		return out, nil
	}, &reads
}

// THE WALKS OF ONE READ SHARE WHAT THEY LEARN. Two roots over one shared
// downstream ask for each item's waiters once, the union, never the sum.
func TestHeldRootsSharingADownstreamReadItOnce(t *testing.T) {
	t.Parallel()
	const n = 300
	waiters, reads := fanGraph(n)
	w := newHoldingWalk("owner", MaxHoldingSteps, waiters)
	for _, root := range []string{"root-a", "root-b"} {
		row, err := w.walk(TaskRow{ID: root})
		if err != nil {
			t.Fatal(err)
		}
		if row.Blocks != n || row.BlocksCapped {
			t.Fatalf("%s blocks %d (capped %v), want %d", root, row.Blocks, row.BlocksCapped, n)
		}
	}
	if want := n + 2; *reads != want {
		t.Errorf("waiter reads = %d, want %d: each item once across both walks", *reads, want)
	}
}

// AND THE READ STOPS AT ITS BUDGET, saying which counts are floors.
func TestAWalkPastItsBudgetSaysItsCountIsAFloor(t *testing.T) {
	t.Parallel()
	waiters, reads := fanGraph(50)
	w := newHoldingWalk("owner", 10, waiters)
	row, err := w.walk(TaskRow{ID: "root-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !row.BlocksCapped || *reads != 10 {
		t.Errorf("capped %v after %d reads, want a capped count after the budget of 10", row.BlocksCapped, *reads)
	}
}
