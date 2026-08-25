package seat

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// TestMain silences the host, which logs a line per claim, release and
// admission edge. That is exactly right in production and unreadable in a
// test run.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
	// Re-bind the package loggers: Get snapshots the root handler, so the
	// ones taken at package init still point at the boot default.
	log = logging.Get("seat.host")
	watchLog = logging.Get("seat.watchdog")
	os.Exit(m.Run())
}

// --- a clock the test moves ------------------------------------------------

// fakeClock is the STORE's clock and the host's, together.
//
// Every lapse in this suite is simulated by moving it, never by sleeping:
// the seat design turns on TTLs measured in tens of seconds, and a suite
// that waited them out would be both slow and flaky. Sharing one clock
// between the two is what makes "the lease has expired" and "the last renew
// is stale" move together, the way they do on a real node.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// --- a fleet of hosts over one store ---------------------------------------

type fleet struct {
	t     *testing.T
	clock *fakeClock
	store *memory.Backend
	ctx   context.Context
}

func newFleet(t *testing.T) *fleet {
	t.Helper()
	clk := newClock()
	store := memory.New()
	store.Clock = clk.Now
	return &fleet{t: t, clock: clk, store: store, ctx: context.Background()}
}

// newHost builds a host on this fleet's store and clock, defaulting the
// identity fields from the node id.
func (f *fleet) newHost(node string, cfg Config) *Host {
	f.t.Helper()
	cfg.NodeID = node
	if cfg.Backend == nil {
		cfg.Backend = f.store
	}
	if cfg.Owner == "" {
		cfg.Owner = node + ":incarnation"
	}
	if cfg.Seats == nil {
		cfg.Seats = seatsNamed("ceo", "eng", "ops")
	}
	if cfg.Clock == nil {
		cfg.Clock = f.clock.Now
	}
	h, err := New(cfg)
	if err != nil {
		f.t.Fatalf("New(%s): %v", node, err)
	}
	return h
}

// present registers a bare peer's presence lease — a node in the fleet that
// this test does not otherwise drive.
func (f *fleet) present(node string, ttl time.Duration, profile placement.NodeProfile) {
	f.t.Helper()
	profile.ID = node
	lease, err := f.store.TryAcquire(f.ctx, coord.NodeResource(node), coord.AcquireOptions{
		Owner: node + ":1", TTL: ttl, Preferred: node, Protocol: coord.ProtocolVersion,
		Ungated: true, Meta: profile.Meta(),
	})
	if err != nil || lease == nil {
		f.t.Fatalf("present(%s): lease=%v err=%v", node, lease, err)
	}
}

// peerTakes hands a seat to somebody else, the way a real takeover does:
// the row moves, so the previous owner's next renew reports a definite
// false rather than an error.
func (f *fleet) peerTakes(handle, owner string, epochHeldBy string, epoch int64) {
	f.t.Helper()
	if _, err := f.store.Release(f.ctx, coord.SeatResource(handle), epochHeldBy, epoch); err != nil {
		f.t.Fatalf("peerTakes release: %v", err)
	}
	lease, err := f.store.TryAcquire(f.ctx, coord.SeatResource(handle), coord.AcquireOptions{
		Owner: owner, TTL: time.Minute, Protocol: coord.ProtocolVersion,
	})
	if err != nil || lease == nil {
		f.t.Fatalf("peerTakes acquire: lease=%v err=%v", lease, err)
	}
}

func (f *fleet) leaseOf(resource string) *coord.Lease {
	f.t.Helper()
	lease, err := f.store.Get(f.ctx, resource)
	if err != nil {
		f.t.Fatalf("get(%s): %v", resource, err)
	}
	return lease
}

// --- seat providers --------------------------------------------------------

func seatsNamed(handles ...string) func() []placement.Seat {
	return func() []placement.Seat {
		out := make([]placement.Seat, 0, len(handles))
		for _, h := range handles {
			out = append(out, placement.Seat{Handle: h})
		}
		return out
	}
}

func numberedSeats(n int) func() []placement.Seat {
	handles := make([]string, 0, n)
	for i := range n {
		handles = append(handles, "s"+strconv.Itoa(i))
	}
	return seatsNamed(handles...)
}

// seedHint records a placement hint for a seat and lets the lease that
// carried it lapse. The hint OUTLIVES its holder — that is its whole purpose
// — so this is how a test stages "these seats were last served here".
func (f *fleet) seedHint(handle, node string) {
	f.t.Helper()
	lease, err := f.store.TryAcquire(f.ctx, coord.SeatResource(handle), coord.AcquireOptions{
		Owner: node + ":previous", TTL: time.Millisecond, Preferred: node,
		Protocol: coord.ProtocolVersion,
	})
	if err != nil || lease == nil {
		f.t.Fatalf("seedHint(%s): lease=%v err=%v", handle, lease, err)
	}
	f.clock.Advance(time.Second)
}

// --- hook recorder ---------------------------------------------------------

// hookLog is a [Hooks] implementation that records what it was asked to do
// and can be told to fail.
type hookLog struct {
	mu         sync.Mutex
	acquires   []string
	releases   []string
	admissions []string

	// acquireErr and releaseErr decide, per call, whether the hook fails.
	acquireErr func(handle string, calls int) error
	releaseErr func(handle string, reason ReleaseReason, calls int) error
	admitErr   error

	// block, when non-nil, is waited on inside OnAcquire.
	block func(handle string)
}

func (l *hookLog) OnAcquire(_ context.Context, handle string, _ coord.Lease) error {
	l.mu.Lock()
	l.acquires = append(l.acquires, handle)
	n := len(l.acquires)
	fail := l.acquireErr
	block := l.block
	l.mu.Unlock()
	if block != nil {
		block(handle)
	}
	if fail != nil {
		return fail(handle, n)
	}
	return nil
}

func (l *hookLog) OnRelease(_ context.Context, handle string, _ coord.Lease, reason ReleaseReason) error {
	l.mu.Lock()
	l.releases = append(l.releases, handle+":"+reason.String())
	n := len(l.releases)
	fail := l.releaseErr
	l.mu.Unlock()
	if fail != nil {
		return fail(handle, reason, n)
	}
	return nil
}

func (l *hookLog) OnAdmission(_ context.Context, handle string, admitted bool) error {
	l.mu.Lock()
	state := "false"
	if admitted {
		state = "true"
	}
	l.admissions = append(l.admissions, handle+":"+state)
	err := l.admitErr
	l.mu.Unlock()
	return err
}

func (l *hookLog) acquired() []string    { return l.copyOf(&l.acquires) }
func (l *hookLog) released() []string    { return l.copyOf(&l.releases) }
func (l *hookLog) admissioned() []string { return l.copyOf(&l.admissions) }

func (l *hookLog) copyOf(field *[]string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(*field)
}

var _ Hooks = (*hookLog)(nil)

// --- assertions ------------------------------------------------------------

func wantStrings(t *testing.T, got, want []string, what string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func wantHeld(t *testing.T, h *Host, want ...string) {
	t.Helper()
	slices.Sort(want)
	wantStrings(t, h.Held(), want, "held")
}

func wantInt(t *testing.T, got, want int, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", what, got, want)
	}
}

func wantAdmits(t *testing.T, h *Host, handle string, want bool) {
	t.Helper()
	if _, ok := h.MayStart(handle); ok != want {
		t.Errorf("MayStart(%q) = %v, want %v", handle, ok, want)
	}
}
