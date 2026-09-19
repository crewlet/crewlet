package followsync_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify/followsync"
	"github.com/crewlet/crewlet/internal/store"
)

func at(min int) time.Time {
	return time.Date(2026, 9, 19, 12, min, 0, 0, time.UTC)
}

func row(thread, reason string, min int) store.Follow {
	return store.Follow{
		Backend: "slack", Handle: "agent-swe", Channel: "C1",
		Thread: thread, Reason: reason, UpdatedAt: at(min),
	}
}

// THE ROWS REACH THE FLEET AND LEAVE THE TABLE, which is the whole job.
func TestTheHandoffCarriesEveryRowAndEmptiesTheTable(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []store.Follow{
		row("t-1", "mention", 1), row("t-2", "collective", 2), row("t-3", "explicit", 3),
	}}
	dst := newFakeFleet()

	created, err := followsync.Migrate(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if created != 3 {
		t.Errorf("created = %d, want 3", created)
	}
	if got := dst.count(); got != 3 {
		t.Errorf("the fleet holds %d follow(s), want 3", got)
	}
	if left := src.remaining(); left != 0 {
		t.Errorf("%d row(s) left in the table, so every boot would re-attempt them", left)
	}
	// The REASON travels, not just the key: an operator asking why a seat
	// answered a thread reads it, and `explicit` is the one with no
	// re-establishing path at all.
	if got := dst.reason("slack", "agent-swe", "C1", "t-3"); got != "explicit" {
		t.Errorf("reason = %q, want it carried across", got)
	}
}

// A ROW THE FLEET ALREADY HAS IS LEFT ALONE AND STILL LEAVES THE TABLE.
//
// The create-only write is what makes the pass safe to run while inbound chat
// is live: a stale local row must not land on top of a fresh mention. But the
// local copy is still removed, or the pass never terminates.
func TestAFollowTheFleetAlreadyHoldsIsNotOverwrittenAndStillLeaves(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []store.Follow{row("t-1", "collective", 1)}}
	dst := newFakeFleet()
	// The fleet's own, fresher record.
	if _, err := dst.FollowIfAbsent(context.Background(), "slack", "agent-swe",
		"C1", "t-1", "app_mention", at(9)); err != nil {
		t.Fatalf("seed the fleet: %v", err)
	}

	created, err := followsync.Migrate(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0 — the fleet already had it", created)
	}
	if got := dst.reason("slack", "agent-swe", "C1", "t-1"); got != "app_mention" {
		t.Errorf("reason = %q, want the fleet's own record untouched — a stale "+
			"local row overwrote a fresher one", got)
	}
	if left := src.remaining(); left != 0 {
		t.Error("a row the fleet already held was left in the table, so every " +
			"boot re-reads and re-attempts it for the life of the deployment")
	}
}

// A ROW THAT COULD NOT BE CARRIED IS NOT REMOVED.
//
// The clause that makes this not best effort. Deleting a row this node is the
// only holder of, because the write failed, is the exact harm the handoff
// exists to prevent — and the first symptom would be a seat silently not
// answering a thread.
func TestAFollowThatCouldNotBeCarriedStaysInTheTable(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []store.Follow{row("t-1", "mention", 1), row("t-keep", "mention", 2)}}
	dst := newFakeFleet()
	dst.failOn = "t-keep"

	created, err := followsync.Migrate(context.Background(), src, dst)
	if err == nil {
		t.Fatal("a pass that could not carry a row reported success")
	}
	if created != 1 {
		t.Errorf("created = %d, want the one that did land", created)
	}
	if !src.stillHas("t-keep") {
		t.Fatal("the row whose write failed was deleted anyway — this node was " +
			"its only holder and it is now gone")
	}
	if src.stillHas("t-1") {
		t.Error("the row that DID land was left behind, so the pass never terminates")
	}
}

// TWO NODES HANDING OFF AT ONCE NEED NOTHING AGREED BETWEEN THEM.
//
// Their local tables overlap. Exactly one create wins per key, the loser
// learns the fleet already has it, and both empty their own table.
func TestTwoNodesHandingOffTheSameThreadsConverge(t *testing.T) {
	t.Parallel()
	dst := newFakeFleet()
	a := &fakeSource{rows: []store.Follow{row("t-1", "mention", 1), row("t-2", "mention", 1)}}
	b := &fakeSource{rows: []store.Follow{row("t-2", "collective", 2), row("t-3", "collective", 2)}}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	made := make([]int, 2)
	for i, src := range []*fakeSource{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			made[i], errs[i] = followsync.Migrate(context.Background(), src, dst)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
	}
	if total := made[0] + made[1]; total != 3 {
		t.Errorf("between them they created %d, want 3 distinct threads", total)
	}
	if got := dst.count(); got != 3 {
		t.Errorf("the fleet holds %d, want 3", got)
	}
	if a.remaining() != 0 || b.remaining() != 0 {
		t.Errorf("rows left behind: node a %d, node b %d", a.remaining(), b.remaining())
	}
}

// AN EMPTY TABLE IS THE STEADY STATE AND COSTS ONE READ.
func TestAnEmptyTableIsOneReadAndNoWrites(t *testing.T) {
	t.Parallel()
	src := &fakeSource{}
	dst := newFakeFleet()

	created, err := followsync.Migrate(context.Background(), src, dst)
	if err != nil || created != 0 {
		t.Fatalf("Migrate over an empty table: created=%d err=%v", created, err)
	}
	if src.lists != 1 {
		t.Errorf("listed %d time(s), want exactly one", src.lists)
	}
	if dst.writes() != 0 {
		t.Errorf("wrote %d time(s) with nothing to carry", dst.writes())
	}
}

// --- fakes -----------------------------------------------------------------

type fakeSource struct {
	mu      sync.Mutex
	rows    []store.Follow
	dropped map[string]bool
	lists   int
}

func (f *fakeSource) List(context.Context) ([]store.Follow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	out := make([]store.Follow, 0, len(f.rows))
	for _, r := range f.rows {
		if !f.dropped[r.Thread] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeSource) Drop(_ context.Context, one store.Follow) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropped == nil {
		f.dropped = map[string]bool{}
	}
	if f.dropped[one.Thread] {
		return false, nil
	}
	f.dropped[one.Thread] = true
	return true, nil
}

func (f *fakeSource) remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if !f.dropped[r.Thread] {
			n++
		}
	}
	return n
}

func (f *fakeSource) stillHas(thread string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Thread == thread && !f.dropped[thread] {
			return true
		}
	}
	return false
}

// fakeFleet is the create-only destination, with the same exclusion the real
// backends have: one key, one winner.
type fakeFleet struct {
	mu     sync.Mutex
	held   map[string]string
	puts   int
	failOn string
}

func newFakeFleet() *fakeFleet { return &fakeFleet{held: map[string]string{}} }

func (f *fakeFleet) FollowIfAbsent(_ context.Context, backend, handle, channel, thread, reason string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if thread == f.failOn {
		return false, errors.New("the broker is unreachable")
	}
	key := backend + "|" + handle + "|" + channel + "|" + thread
	if _, held := f.held[key]; held {
		return false, nil
	}
	f.held[key] = reason
	return true, nil
}

func (f *fakeFleet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.held)
}

func (f *fakeFleet) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

func (f *fakeFleet) reason(backend, handle, channel, thread string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[backend+"|"+handle+"|"+channel+"|"+thread]
}
