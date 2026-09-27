package upkeep

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// scrubClock is the time a scrub reads and the waits it asks for: a wait moves
// the clock rather than sleeping, and a wait of at least stopAt — the one
// between cycles — ends the scrub.
type scrubClock struct {
	mu     sync.Mutex
	now    time.Time
	waits  []time.Duration
	stopAt time.Duration
	cancel context.CancelFunc
}

func (c *scrubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *scrubClock) wait(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	if d >= c.stopAt {
		c.cancel()
		return false
	}
	if d > 0 {
		c.now = c.now.Add(d)
	}
	return ctx.Err() == nil
}

// paced is the total of the waits shorter than the one between cycles.
func (c *scrubClock) paced() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total time.Duration
	for _, d := range c.waits {
		if d < c.stopAt {
			total += d
		}
	}
	return total
}

func (c *scrubClock) last() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits[len(c.waits)-1]
}

var scrubStart = time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)

// scrubbing is a data node with chunks of size bytes each, and the clock its
// scrub runs on.
func scrubbing(t *testing.T, chunks, size int) (*Node, *disk.Store, []objstore.Hash, *scrubClock) {
	t.Helper()
	return scrubbingOn(t, openStore(t), chunks, size)
}

// scrubbingOn is scrubbing over a store the test opened.
func scrubbingOn(t *testing.T, store *disk.Store, chunks, size int) (*Node, *disk.Store, []objstore.Hash, *scrubClock) {
	t.Helper()
	var held []objstore.Hash
	for i := range chunks {
		data := make([]byte, size)
		data[0], data[1] = byte(i), byte(i>>8)
		h := objstore.HashOf(data)
		if err := store.Put(h, data); err != nil {
			t.Fatal(err)
		}
		held = append(held, h)
	}
	slices.Sort(held)
	clock := &scrubClock{now: scrubStart, stopAt: time.Hour}
	n, err := NewNode(NodeOptions{Self: "data-a", Local: store, Peers: &answering{},
		Layouts:    func() (*placement.Layout, bool) { return nil, false },
		References: References{newRefs()}, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	n.wait = clock.wait
	return n, store, held, clock
}

// rot overwrites a held chunk's bytes.
func rot(t *testing.T, store *disk.Store, h objstore.Hash) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store.Root(), disk.Layout(h)), []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runScrub runs a scrub until its clock stops it.
func runScrub(t *testing.T, n *Node, clock *scrubClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock.mu.Lock()
	clock.cancel = cancel
	clock.mu.Unlock()
	if err := n.Scrub(ctx); err != nil {
		t.Fatal(err)
	}
}

func readCursor(t *testing.T, store *disk.Store) scrubCursor {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store.Root(), scrubCursorName))
	if err != nil {
		t.Fatal(err)
	}
	var cur scrubCursor
	if err := json.Unmarshal(raw, &cur); err != nil {
		t.Fatal(err)
	}
	return cur
}

// A SCRUB READS EVERY CHUNK THE NODE HOLDS AND REMOVES THE ONES THAT ROTTED —
// the rot nobody reading would ever find — at a paced rate: a node holding
// little reads at the floor rather than spreading a few bytes over a week, and
// then waits out the rest of the week before its next cycle.
func TestAScrubChecksEveryChunkAndRemovesTheRotten(t *testing.T) {
	t.Parallel()
	const size = 64 << 10
	n, store, held, clock := scrubbing(t, 8, size)
	rotten := held[3]
	rot(t, store, rotten)
	runScrub(t, n, clock)

	if store.Has(rotten) {
		t.Fatal("the rotten chunk survived the scrub")
	}
	st := n.Status().Scrub
	if st.Verified != 7 || st.Rotten != 1 || st.Progress != 1 || !st.CycleStarted.Equal(scrubStart) ||
		st.LastRotten.IsZero() || st.Error != "" {
		t.Fatalf("status = %+v, want 7 verified, 1 rotten, the cycle done", st)
	}
	// THE FLOOR: each read waited out the one before it at a MiB/s — the
	// rotten chunk's share being the three bytes it had rotted to.
	var before int64
	for _, h := range held[:len(held)-1] {
		if h == rotten {
			before += 3
		} else {
			before += size
		}
	}
	want := time.Duration(before * int64(time.Second) / ScrubFloor)
	if got := clock.paced(); got < want-time.Microsecond || got > want+time.Microsecond {
		t.Fatalf("the reads were paced over %v, want %v at the floor rate", got, want)
	}
	// AND THE NEXT CYCLE is a week after this one began.
	if got, want := clock.last(), ScrubInterval-clock.paced(); got != want {
		t.Fatalf("the scrub then waited %v, want %v — a week from the cycle's start", got, want)
	}
	if cur := readCursor(t, store); cur.Next != placement.Slots || cur.Verified != 7 || cur.Rotten != 1 {
		t.Fatalf("the persisted cursor is %+v, want the finished cycle", cur)
	}
}

// unreadableDisk fails every read of the chunks named in it with EIO — the
// latent sector error, which a healthy disk in a temporary directory cannot
// be made to return.
type unreadableDisk struct {
	mu  sync.Mutex
	bad map[string]bool
}

func (u *unreadableDisk) spoil(hs ...objstore.Hash) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, h := range hs {
		u.bad[string(h)] = true
	}
}

func (u *unreadableDisk) read(f *os.File) ([]byte, error) {
	u.mu.Lock()
	bad := u.bad[filepath.Base(f.Name())]
	u.mu.Unlock()
	if bad {
		return nil, &os.PathError{Op: "read", Path: f.Name(), Err: syscall.EIO}
	}
	return io.ReadAll(f)
}

// A CHUNK THAT WILL NOT READ IS COUNTED, REMOVED AND STEPPED PAST — the latent
// sector error a scrub most exists to find. Answered as the walk failing, it
// had the scrub try the same slots every minute for ever: stuck on the one
// chunk, every slot after it unchecked, and the bad copy still counted held.
func TestAChunkThatWillNotReadIsRemovedAndTheScrubGoesOn(t *testing.T) {
	t.Parallel()
	u := &unreadableDisk{bad: map[string]bool{}}
	store, err := disk.Options{Space: halfFull, Read: u.read}.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	n, store, held, clock := scrubbingOn(t, store, 8, 1024)
	u.spoil(held[2], held[5])
	runScrub(t, n, clock)

	for _, h := range []objstore.Hash{held[2], held[5]} {
		if store.Has(h) {
			t.Fatalf("%s would not read and is still held", h)
		}
	}
	st := n.Status().Scrub
	if st.Verified != 6 || st.Unreadable != 2 || st.Rotten != 0 || st.Progress != 1 ||
		st.LastUnreadable.IsZero() || st.Error != "" {
		t.Fatalf("status = %+v, want 6 verified, 2 unreadable, the cycle done", st)
	}
	if cur := readCursor(t, store); cur.Next != placement.Slots || cur.Unreadable != 2 {
		t.Fatalf("the persisted cursor is %+v, want the finished cycle counting both", cur)
	}
}

// refusing is a chunk store whose Verify of one chunk fails with err, as a
// disk does that will not open a file or has been released.
type refusing struct {
	*disk.Store
	chunk objstore.Hash
	err   error
}

func (r refusing) Verify(h objstore.Hash) (bool, error) {
	if h == r.chunk {
		return false, r.err
	}
	return r.Store.Verify(h)
}

// A CHUNK THE DISK WILL NOT EVEN OPEN is stepped past too, and counted — it
// stays, since a name that will not open is as often every name. But a store
// RELEASED under the scrub stops it: counting every chunk left as unreadable
// would report a failed disk when the store only closed.
func TestAScrubStepsPastAChunkItCannotOpenAndStopsOnAReleasedStore(t *testing.T) {
	t.Parallel()
	t.Run("a chunk that will not open", func(t *testing.T) {
		t.Parallel()
		n, store, held, clock := scrubbing(t, 6, 1024)
		n.opts.Local = refusing{Store: store, chunk: held[1],
			err: fmt.Errorf("objstore/disk: open %s: %w", held[1], syscall.EIO)}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		clock.cancel = cancel
		n.wait = func(ctx context.Context, d time.Duration) bool {
			if d == scrubRetry {
				t.Error("the scrub retried the window over one chunk it could not open")
				cancel()
				return false
			}
			return clock.wait(ctx, d)
		}
		if err := n.Scrub(ctx); err != nil {
			t.Fatal(err)
		}
		st := n.Status().Scrub
		if st.Verified != 5 || st.Unreadable != 1 || st.Progress != 1 || st.Error != "" {
			t.Fatalf("status = %+v, want 5 verified, 1 unreadable, the cycle done", st)
		}
		if !store.Has(held[1]) {
			t.Fatal("a chunk that would not open was removed")
		}
	})
	t.Run("a store released", func(t *testing.T) {
		t.Parallel()
		n, store, held, clock := scrubbing(t, 6, 1024)
		n.opts.Local = refusing{Store: store, chunk: held[3], err: disk.ErrClosed}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		n.wait = func(ctx context.Context, d time.Duration) bool {
			if d == scrubRetry {
				cancel()
				return false
			}
			return clock.wait(ctx, d)
		}
		if err := n.Scrub(ctx); err != nil {
			t.Fatal(err)
		}
		st := n.Status().Scrub
		if st.Unreadable != 0 || st.Verified != 3 || !strings.Contains(st.Error, "released") {
			t.Fatalf("status = %+v, want the scrub stopped at the fourth chunk, naming why", st)
		}
	})
}

// A SCRUB RESUMES WHERE IT STOPPED: a node restarted every few days would
// otherwise check its first slots over and over and never reach its last. What
// is below the cursor is not read again this cycle, and the week is counted
// from when the cycle began, not from the restart.
func TestAScrubResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	n, store, held, clock := scrubbing(t, 12, 4096)
	resume := held[6].Slot()
	if held[5].Slot() == resume {
		t.Fatal("the fixture's fifth and sixth chunks share a slot; the resume point must split them")
	}
	before, after := held[2], held[9]
	rot(t, store, before)
	rot(t, store, after)
	started := scrubStart.Add(-time.Hour)
	if err := writeCursor(filepath.Join(store.Root(), scrubCursorName),
		scrubCursor{Started: started, Next: resume, Bytes: 12 * 4096, Verified: 5}); err != nil {
		t.Fatal(err)
	}
	runScrub(t, n, clock)

	if !store.Has(before) {
		t.Fatal("a chunk below the cursor was read again")
	}
	if store.Has(after) {
		t.Fatal("a rotten chunk above the cursor was not found")
	}
	st := n.Status().Scrub
	if st.Verified != 5+5 || st.Rotten != 1 || !st.CycleStarted.Equal(started) {
		t.Fatalf("status = %+v, want the resumed cycle's counts carried on", st)
	}
	if got, want := clock.last(), ScrubInterval-time.Hour-clock.paced(); got != want {
		t.Fatalf("the scrub then waited %v, want %v — a week from when the cycle began", got, want)
	}
}

// A NEW CYCLE STARTS A WEEK AFTER THE LAST ONE BEGAN: at once when that is
// past, and not before when it is not.
func TestANewCycleStartsAWeekAfterTheLast(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		started time.Time
		scrubs  bool
	}{
		{"due", scrubStart.Add(-8 * 24 * time.Hour), true},
		{"not yet due", scrubStart.Add(-24 * time.Hour), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n, store, _, clock := scrubbing(t, 3, 1024)
			if err := writeCursor(filepath.Join(store.Root(), scrubCursorName),
				scrubCursor{Started: c.started, Next: placement.Slots, Verified: 99}); err != nil {
				t.Fatal(err)
			}
			runScrub(t, n, clock)
			st := n.Status().Scrub
			if c.scrubs {
				if st.Verified != 3 || !st.CycleStarted.Equal(scrubStart) {
					t.Fatalf("a due cycle = %+v, want a new one run at once", st)
				}
				return
			}
			if got, want := clock.waits[0], 6*24*time.Hour; got != want {
				t.Fatalf("the scrub first waited %v, want %v", got, want)
			}
			if st.Verified != 99 || !st.CycleStarted.Equal(c.started) {
				t.Fatalf("a cycle that was not due = %+v, want the last one's", st)
			}
		})
	}
}

// A CURSOR THIS BUILD CANNOT READ STARTS A FRESH CYCLE — checking more, never
// skipping slots on the word of a file it does not understand.
func TestAnUnreadableCursorStartsAFreshCycle(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"not json":            "{half",
		"a slot out of range": `{"started":"2026-09-20T00:00:00Z","next":70000}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, store, _, clock := scrubbing(t, 4, 1024)
			if err := os.WriteFile(filepath.Join(store.Root(), scrubCursorName), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			runScrub(t, n, clock)
			if st := n.Status().Scrub; st.Verified != 4 || !st.CycleStarted.Equal(scrubStart) {
				t.Fatalf("status = %+v, want a fresh cycle over all 4", st)
			}
		})
	}
}

// A SCRUB STOPPED PART WAY LEAVES ITS PLACE BEHIND, so the restart that
// follows resumes there.
func TestAStoppedScrubLeavesItsPlace(t *testing.T) {
	t.Parallel()
	n, store, held, clock := scrubbing(t, 10, 1024)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	n.wait = func(ctx context.Context, d time.Duration) bool {
		reads++
		if reads == 5 {
			cancel()
			return false
		}
		return clock.wait(ctx, d)
	}
	if err := n.Scrub(ctx); err != nil {
		t.Fatal(err)
	}
	cur := readCursor(t, store)
	if cur.Next != held[4].Slot() || cur.Verified != 4 {
		t.Fatalf("the cursor left behind is %+v, want the fifth chunk's slot %#x after 4 verified",
			cur, held[4].Slot())
	}
}

// ONE SCRUB PER NODE: a second would share the cursor and read what the first
// already had.
func TestANodeRunsOneScrub(t *testing.T) {
	t.Parallel()
	n, _, _, _ := scrubbing(t, 2, 1024)
	started := make(chan struct{})
	n.wait = func(ctx context.Context, _ time.Duration) bool {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return false
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- n.Scrub(ctx) }()
	<-started
	if err := n.Scrub(t.Context()); err == nil {
		t.Fatal("a second scrub ran beside the first")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// THE RATE SPREADS A CYCLE OVER A WEEK, between the floor and the ceiling.
func TestTheScrubRateIsAWeekBetweenFloorAndCeiling(t *testing.T) {
	t.Parallel()
	week := int64(ScrubInterval / time.Second)
	for _, c := range []struct {
		held int64
		want float64
	}{
		{0, ScrubFloor},
		{week * ScrubFloor / 2, ScrubFloor},
		{week * 4 << 20, 4 << 20},
		{week * ScrubCeiling * 3, ScrubCeiling},
	} {
		if got := scrubRate(c.held); got != c.want {
			t.Errorf("scrubRate(%d) = %v, want %v", c.held, got, c.want)
		}
	}
}
