package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/maintenance"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// letter is mail addressed to a seat, so a test can ask whether a mailbox
// still holds what was sent to it.
type letter struct {
	Body string `json:"body"`
}

func (letter) EventType() string { return "test.mailbox_letter" }

func init() { events.Register[letter]() }

// grace is the shipped grace period, named short for the arithmetic below.
const grace = maintenance.MailboxRetirementGrace

// roster is a controllable active revision: the handles it names, or the
// error that makes it unknown.
type roster struct {
	mu      sync.Mutex
	handles []string
	err     error
	// during, when set, runs inside the next read, once. It is how a test
	// changes the company while a sweep is between its reads.
	during func()
}

func (r *roster) read(context.Context) ([]string, error) {
	r.mu.Lock()
	hook := r.during
	r.during = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return slices.Clone(r.handles), nil
}

func (r *roster) set(handles ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handles, r.err = handles, nil
}

func (r *roster) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// gatedQueue counts subscription deletes, and can hold them at a gate or fail
// them, so a test can act in the middle of a retirement.
type gatedQueue struct {
	*qmem.Queue

	deletes atomic.Int32

	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
	failure error
}

func (q *gatedQueue) DeleteSubscription(ctx context.Context, topic, group string) (bool, error) {
	q.mu.Lock()
	gate, entered, failure := q.gate, q.entered, q.failure
	q.gate, q.entered = nil, nil
	q.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if gate != nil {
		// The caller's deadline still applies, as it does to a broker
		// request, so a test can tell a retirement that gave up on time
		// from one that waited out whatever held it.
		select {
		case <-gate:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if failure != nil {
		return false, failure
	}
	q.deletes.Add(1)
	return q.Queue.DeleteSubscription(ctx, topic, group)
}

// holdNextDelete makes the next subscription delete wait until release is
// closed, returning a channel closed when the delete is reached.
func (q *gatedQueue) holdNextDelete(release chan struct{}) <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.gate, q.entered = release, make(chan struct{})
	return q.entered
}

func (q *gatedQueue) failDeletes(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failure = err
}

type mailboxHarness struct {
	t       *testing.T
	records *coordmem.Fleet
	queue   *gatedQueue
	leases  coord.Backend
	roster  *roster
	m       *maintenance.Mailboxes
	// builds numbers every Mailboxes this harness makes, so each claims
	// seat leases under an owner of its own, as each process does.
	builds atomic.Int32
}

// harnessLeaseTTL is the seat lease TTL the harness's retirements claim with:
// long enough that no test's work limit is the lease's rather than the budget's
// unless the test says so.
const harnessLeaseTTL = time.Hour

func newMailboxHarness(t *testing.T, tune func(*maintenance.MailboxOptions)) *mailboxHarness {
	t.Helper()
	q := qmem.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	h := &mailboxHarness{
		t: t, records: coordmem.NewFleet(), queue: &gatedQueue{Queue: q},
		leases: &coordmem.Backend{}, roster: &roster{},
	}
	h.m = h.build(tune)
	return h
}

// build makes a Mailboxes over the harness's shared stores, as a second node
// in the same fleet would have.
func (h *mailboxHarness) build(tune func(*maintenance.MailboxOptions)) *maintenance.Mailboxes {
	h.t.Helper()
	opts := maintenance.MailboxOptions{
		Records: h.records, Queue: h.queue, Leases: h.leases, Roster: h.roster.read,
		Owner:        fmt.Sprintf("retirement-%d", h.builds.Add(1)),
		LeaseTTL:     harnessLeaseTTL,
		RegisterPoll: 5 * time.Millisecond,
	}
	if tune != nil {
		tune(&opts)
	}
	m, err := maintenance.NewMailboxes(opts)
	if err != nil {
		h.t.Fatalf("NewMailboxes: %v", err)
	}
	return m
}

// seat is what a node does for a seat in the revision it applied: register
// the mailbox, create the inbox, and (as the seat's owner) subscribe the
// control topic. One letter is then sent, so the mailbox holds something.
func (h *mailboxHarness) seat(handle string) {
	h.t.Helper()
	if err := h.m.Register(h.t.Context(), handle); err != nil {
		h.t.Fatalf("Register(%s): %v", handle, err)
	}
	h.ensure(handle)
	if _, err := h.queue.EnsureSubscription(h.t.Context(),
		topics.AgentControl(handle), topics.AgentControlGroup(handle)); err != nil {
		h.t.Fatalf("control subscription for %s: %v", handle, err)
	}
	h.send(handle, "hello "+handle)
}

func (h *mailboxHarness) ensure(handle string) {
	h.t.Helper()
	if _, err := h.queue.EnsureSubscription(h.t.Context(),
		topics.AgentInbox(handle), topics.AgentInboxGroup(handle)); err != nil {
		h.t.Fatalf("inbox for %s: %v", handle, err)
	}
}

func (h *mailboxHarness) send(handle, body string) {
	h.t.Helper()
	ev := events.New(letter{Body: body}, events.TraceContext{})
	if err := h.queue.Publish(h.t.Context(), topics.AgentInbox(handle), ev); err != nil {
		h.t.Fatalf("publish to %s: %v", handle, err)
	}
}

// held is how many letters the seat's inbox retains.
func (h *mailboxHarness) held(handle string) int {
	return len(h.queue.Backlog(topics.AgentInbox(handle), topics.AgentInboxGroup(handle)))
}

// exists reports whether a subscription is there, without creating one.
func (h *mailboxHarness) exists(topic, group string) bool {
	h.t.Helper()
	made, err := h.queue.EnsureSubscription(h.t.Context(), topic, group)
	if err != nil {
		h.t.Fatalf("probe %s/%s: %v", topic, group, err)
	}
	if made {
		// The probe created what was not there; take it away again so the
		// probe does not change the state it reports on.
		if _, err := h.queue.Queue.DeleteSubscription(h.t.Context(), topic, group); err != nil {
			h.t.Fatalf("undo probe %s/%s: %v", topic, group, err)
		}
	}
	return !made
}

func (h *mailboxHarness) inboxExists(handle string) bool {
	return h.exists(topics.AgentInbox(handle), topics.AgentInboxGroup(handle))
}

func (h *mailboxHarness) controlExists(handle string) bool {
	return h.exists(topics.AgentControl(handle), topics.AgentControlGroup(handle))
}

func (h *mailboxHarness) record(handle string) (coord.MailboxRecord, bool) {
	h.t.Helper()
	rec, found, err := h.records.Mailbox(h.t.Context(), handle)
	if err != nil {
		h.t.Fatalf("Mailbox(%s): %v", handle, err)
	}
	return rec, found
}

// tick runs the sweep through a real worker at a pinned instant, so the
// cutoff is the one the worker derives from the job's own horizon.
func (h *mailboxHarness) tick(m *maintenance.Mailboxes, at time.Time) (int64, error) {
	w := maintenance.New(maintenance.Options{Now: fixed(at), Jobs: m.Jobs()})
	swept, err := w.Tick(h.t.Context())
	return swept["seat_mailboxes"], err
}

func (h *mailboxHarness) mustTick(at time.Time) int64 {
	h.t.Helper()
	n, err := h.tick(h.m, at)
	if err != nil {
		h.t.Fatalf("tick at %s: %v", at, err)
	}
	return n
}

// removed sets up the ordinary starting point: two seats with mail, and a
// revision that no longer has one of them, already observed by a sweep.
func (h *mailboxHarness) removed() {
	h.t.Helper()
	h.seat("ceo")
	h.seat("swe")
	h.roster.set("ceo")
	if n := h.mustTick(base); n != 0 {
		h.t.Fatalf("the first sweep to see the absence retired %d mailboxes", n)
	}
	rec, _ := h.record("swe")
	if !rec.AbsentSince.Equal(base) {
		h.t.Fatalf("the absence was stamped %v, want the sweep's own instant %v", rec.AbsentSince, base)
	}
}

// A seat deleted by accident and restored within the grace period must come
// back to the mail it was sent while it was gone. Retiring on the first sweep
// that noticed would make an undo lose a day of work.
func TestARemovedSeatsMailboxSurvivesTheGracePeriod(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()

	for _, at := range []time.Time{base.Add(time.Hour), base.Add(grace - time.Minute)} {
		if n := h.mustTick(at); n != 0 {
			t.Fatalf("a sweep at %s retired %d mailboxes inside the grace period", at.Sub(base), n)
		}
		if got := h.held("swe"); got != 1 {
			t.Fatalf("at %s the removed seat's mailbox holds %d letters, want 1", at.Sub(base), got)
		}
		if !h.controlExists("swe") {
			t.Fatalf("at %s the removed seat's control subscription is gone", at.Sub(base))
		}
		rec, found := h.record("swe")
		if !found || !rec.AbsentSince.Equal(base) || rec.Retiring() {
			t.Fatalf("at %s the record is %+v (found %v), want it still absent since %s",
				at.Sub(base), rec, found, base)
		}
	}
	if got := h.queue.deletes.Load(); got != 0 {
		t.Fatalf("%d subscriptions were deleted inside the grace period", got)
	}
}

// Past the grace period the removed seat's mailbox is deleted, mail and all,
// and so is its sandbox control subscription. A seat still in the company is
// untouched, and a seat added again under the retired handle starts empty
// rather than working the old backlog under a new role.
func TestARemovedSeatsMailboxIsRetiredAfterTheGracePeriod(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()

	if n := h.mustTick(base.Add(grace + time.Minute)); n != 1 {
		t.Fatalf("the sweep after the grace period retired %d mailboxes, want 1", n)
	}
	if h.inboxExists("swe") || h.controlExists("swe") {
		t.Fatal("the removed seat's subscriptions survived its retirement, so its mail is retained for ever")
	}
	if _, found := h.record("swe"); found {
		t.Fatal("the retired mailbox's record survived, so the registry grows with every removed seat")
	}
	if got := h.held("ceo"); got != 1 {
		t.Fatalf("the retirement of one seat touched another: ceo holds %d letters, want 1", got)
	}
	if rec, found := h.record("ceo"); !found || !rec.Present() {
		t.Fatalf("ceo's record = %+v (found %v), want it present", rec, found)
	}

	// The same handle, added again.
	h.roster.set("ceo", "swe")
	if err := h.m.Register(t.Context(), "swe"); err != nil {
		t.Fatalf("Register after retirement: %v", err)
	}
	h.ensure("swe")
	if got := h.held("swe"); got != 0 {
		t.Fatalf("a seat added under a retired handle inherited %d letters of its predecessor", got)
	}
}

// A seat that returns inside the grace period clears its absence, whichever
// writer sees it first, and a later removal starts the clock again rather than
// retiring on the first one's schedule.
func TestASeatReturningWithinTheGraceClearsTheRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		comeBack func(h *mailboxHarness)
	}{
		{"seen by the sweep", func(h *mailboxHarness) {
			h.roster.set("ceo", "swe")
			h.mustTick(base.Add(time.Hour))
		}},
		{"registered by a node applying the revision", func(h *mailboxHarness) {
			h.roster.set("ceo", "swe")
			if err := h.m.Register(h.t.Context(), "swe"); err != nil {
				h.t.Fatalf("Register: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMailboxHarness(t, nil)
			h.removed()

			tc.comeBack(h)
			rec, found := h.record("swe")
			if !found || !rec.Present() {
				t.Fatalf("after the seat returned the record is %+v (found %v), want it present", rec, found)
			}
			if n := h.mustTick(base.Add(grace + time.Hour)); n != 0 {
				t.Fatalf("a seat that returned inside the grace period had %d mailboxes retired", n)
			}
			if got := h.held("swe"); got != 1 {
				t.Fatalf("the returned seat holds %d letters, want the one it was sent", got)
			}

			// Removed again: a new absence, measured from when it is seen.
			h.roster.set("ceo")
			again := base.Add(grace + 2*time.Hour)
			h.mustTick(again)
			if n := h.mustTick(again.Add(grace - time.Minute)); n != 0 {
				t.Fatal("a second removal was retired on the first removal's clock")
			}
			if n := h.mustTick(again.Add(grace + time.Minute)); n != 1 {
				t.Fatalf("the second removal retired %d mailboxes after its own grace period, want 1", n)
			}
		})
	}
}

// listingBarrier makes every caller of Mailboxes wait until the other has
// listed too, so two sweeps are guaranteed to decide from the same snapshot.
type listingBarrier struct {
	maintenance.MailboxRecords
	arrived sync.WaitGroup
	wait    chan struct{}
}

func (b *listingBarrier) Mailboxes(ctx context.Context) ([]coord.MailboxRecord, error) {
	records, err := b.MailboxRecords.Mailboxes(ctx)
	b.arrived.Done()
	<-b.wait
	return records, err
}

// Two sweeps overlap during a duty handoff: the lease moves while the previous
// holder's tick is still running. Both read the same record, and exactly one
// may retire it; the other must see that it lost and do nothing.
func TestTwoSweepsRetireAMailboxOnce(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()

	barrier := &listingBarrier{MailboxRecords: h.records, wait: make(chan struct{})}
	barrier.arrived.Add(2)
	nodes := make([]*maintenance.Mailboxes, 2)
	for i := range nodes {
		nodes[i] = h.build(func(o *maintenance.MailboxOptions) { o.Records = barrier })
	}
	go func() {
		barrier.arrived.Wait()
		close(barrier.wait)
	}()

	at := base.Add(grace + time.Minute)
	var retired atomic.Int64
	var wg sync.WaitGroup
	for _, m := range nodes {
		wg.Go(func() {
			n, err := h.tick(m, at)
			if err != nil {
				t.Errorf("tick: %v", err)
			}
			retired.Add(n)
		})
	}
	wg.Wait()

	if got := retired.Load(); got != 1 {
		t.Fatalf("two overlapping sweeps retired the mailbox %d times, want once", got)
	}
	// The inbox and the control subscription, once each.
	if got := h.queue.deletes.Load(); got != 2 {
		t.Fatalf("%d subscription deletes, want 2: the losing sweep acted anyway", got)
	}
	if h.inboxExists("swe") {
		t.Fatal("neither sweep retired the mailbox")
	}
}

// UNKNOWN IS NEVER ABSENT. A roster that cannot be read, or a registry or a
// lease that cannot, must stamp nothing and retire nothing: a store blip or a
// node behind on the revision must not be able to delete a live seat's mail.
func TestNothingIsRetiredWhileTheActiveRevisionCannotBeRead(t *testing.T) {
	unreadable := errors.New("the activation pointer could not be read")

	t.Run("an unreadable roster retires nothing and says why", func(t *testing.T) {
		h := newMailboxHarness(t, nil)
		h.removed()
		h.roster.fail(unreadable)

		_, err := h.tick(h.m, base.Add(grace+time.Hour))
		if !errors.Is(err, unreadable) {
			t.Fatalf("tick = %v, want the roster's own error so the log says why nothing was retired", err)
		}
		if got := h.held("swe"); got != 1 || h.queue.deletes.Load() != 0 {
			t.Fatalf("a sweep with no roster deleted mail: %d letters left, %d deletes",
				got, h.queue.deletes.Load())
		}
		if rec, _ := h.record("swe"); rec.Retiring() || !rec.AbsentSince.Equal(base) {
			t.Fatalf("a sweep with no roster changed the record: %+v", rec)
		}
	})

	t.Run("an unreadable roster stamps no seat absent", func(t *testing.T) {
		h := newMailboxHarness(t, nil)
		h.seat("swe")
		h.roster.fail(unreadable)
		if _, err := h.tick(h.m, base); !errors.Is(err, unreadable) {
			t.Fatalf("tick = %v, want the roster's error", err)
		}
		if rec, _ := h.record("swe"); !rec.Present() {
			t.Fatalf("a seat was stamped absent against a roster nobody could read: %+v", rec)
		}
	})

	t.Run("a fleet with no revision is judged not at all, quietly", func(t *testing.T) {
		h := newMailboxHarness(t, nil)
		h.seat("swe")
		h.roster.fail(maintenance.ErrNoActiveRevision)
		if n, err := h.tick(h.m, base.Add(grace+time.Hour)); err != nil || n != 0 {
			t.Fatalf("tick = (%d, %v), want nothing done and nothing to report", n, err)
		}
		if rec, _ := h.record("swe"); !rec.Present() {
			t.Fatalf("a company with no revision had a seat stamped absent: %+v", rec)
		}
	})

	t.Run("an unreadable seat lease retires nothing", func(t *testing.T) {
		faulty := coordtest.NewFaulty(&coordmem.Backend{})
		h := newMailboxHarness(t, func(o *maintenance.MailboxOptions) { o.Leases = faulty })
		h.removed()
		faulty.Break(nil)

		if _, err := h.tick(h.m, base.Add(grace+time.Hour)); !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("tick = %v, want the lease store's error", err)
		}
		if got := h.held("swe"); got != 1 {
			t.Fatalf("a retirement went ahead without knowing who holds the seat: %d letters left", got)
		}
		if rec, _ := h.record("swe"); rec.Retiring() {
			t.Fatalf("the record was marked for a retirement that could not be judged: %+v", rec)
		}
	})
}

// A node still holding a removed seat's lease is still consuming its mailbox,
// on a revision that has the seat. Deleting the subscription would pull it out
// from under that node's turns, so the retirement waits for the lease to go.
func TestAMailboxAHolderStillConsumesIsKept(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()
	lease, err := h.leases.TryAcquire(t.Context(), coord.SeatResource("swe"),
		coord.AcquireOptions{Owner: "lagging-node", TTL: time.Hour})
	if err != nil || lease == nil {
		t.Fatalf("TryAcquire = (%v, %v)", lease, err)
	}

	if n := h.mustTick(base.Add(grace + time.Hour)); n != 0 {
		t.Fatalf("a mailbox a node still holds the seat for was retired (%d)", n)
	}
	if h.held("swe") != 1 {
		t.Fatal("the held seat's mail was deleted")
	}

	if _, err := h.leases.Release(t.Context(), lease.Resource, lease.Owner, lease.Epoch); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n := h.mustTick(base.Add(grace + 2*time.Hour)); n != 1 {
		t.Fatalf("once the lease was released the sweep retired %d mailboxes, want 1", n)
	}
}

// A node that installs a revision adding a seat back claims the seat off that
// company without reading the mailbox record, and attaching its consumer creates
// the subscriptions a retirement may be deleting. The retirement holds the
// seat's lease for exactly that span, so the claim loses until the deletes are
// done and then succeeds at once.
func TestNoNodeCanClaimASeatWhileItsMailboxIsRetired(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()

	release := make(chan struct{})
	entered := h.queue.holdNextDelete(release)
	retired := make(chan int64, 1)
	go func() {
		n, err := h.tick(h.m, base.Add(grace+time.Minute))
		if err != nil {
			t.Errorf("tick: %v", err)
		}
		retired <- n
	}()
	<-entered

	claim := func() *coord.Lease {
		t.Helper()
		lease, err := h.leases.TryAcquire(t.Context(), coord.SeatResource("swe"),
			coord.AcquireOptions{Owner: "returning-node", TTL: time.Minute})
		if err != nil {
			t.Fatalf("TryAcquire: %v", err)
		}
		return lease
	}
	if lease := claim(); lease != nil {
		t.Fatalf("a node claimed seat swe (epoch %d) while its mailbox was being deleted, so its "+
			"consumer attaches to a subscription the retirement is about to remove", lease.Epoch)
	}

	close(release)
	if n := <-retired; n != 1 {
		t.Fatalf("the retirement retired %d mailboxes, want 1", n)
	}
	if lease := claim(); lease == nil {
		t.Fatal("the retirement kept the seat's lease after it finished, so a seat added back " +
			"cannot be claimed until the lease lapses")
	}
}

// The lease is what keeps a claiming node off the seat, so a retirement that is
// still issuing deletes after it could have lapsed is not excluded from
// anything. Its work is held to half the lease TTL, however generous the budget.
func TestARetirementEndsWhileItsSeatLeaseIsStillLive(t *testing.T) {
	const leaseTTL = time.Second
	h := newMailboxHarness(t, func(o *maintenance.MailboxOptions) {
		o.LeaseTTL = leaseTTL
		o.RetireBudget = time.Hour
	})
	h.removed()

	// A delete that answers only well after the lease could have lapsed, so a
	// retirement bounded by anything longer finishes late and fails the
	// assertions below rather than hanging the suite.
	slow := make(chan struct{})
	h.queue.holdNextDelete(slow)
	defer time.AfterFunc(3*leaseTTL, func() { close(slow) }).Stop()
	started := time.Now()
	_, err := h.tick(h.m, base.Add(grace+time.Minute))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tick = %v, want the retirement to stop on its own deadline", err)
	}
	if took := time.Since(started); took >= leaseTTL {
		t.Fatalf("the retirement acted for %v, past the %v lease that excluded a claiming node", took, leaseTTL)
	}
	if rec, _ := h.record("swe"); rec.Retiring() {
		t.Fatalf("a retirement that ran out of time left its mark: %+v", rec)
	}
}

// A node applying a revision that adds a seat back while a sweep is deleting
// that seat's previous mailbox must not create its inbox until the delete is
// done, or the delete lands on the new inbox and the seat is deaf.
func TestARegistrationWaitsForARetirementInFlight(t *testing.T) {
	h := newMailboxHarness(t, func(o *maintenance.MailboxOptions) { o.RetireBudget = 10 * time.Second })
	h.removed()

	release := make(chan struct{})
	entered := h.queue.holdNextDelete(release)
	retired := make(chan int64, 1)
	go func() {
		n, err := h.tick(h.m, base.Add(grace+time.Minute))
		if err != nil {
			t.Errorf("tick: %v", err)
		}
		retired <- n
	}()
	<-entered
	if rec, _ := h.record("swe"); !rec.Retiring() {
		t.Fatalf("the retirement is deleting without having marked the record: %+v", rec)
	}

	registered := make(chan error, 1)
	go func() { registered <- h.m.Register(context.WithoutCancel(t.Context()), "swe") }()
	select {
	case err := <-registered:
		t.Fatalf("Register returned (%v) while the previous mailbox was still being deleted", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if n := <-retired; n != 1 {
		t.Fatalf("the retirement retired %d mailboxes, want 1", n)
	}
	if err := <-registered; err != nil {
		t.Fatalf("Register after the retirement finished: %v", err)
	}
	if rec, found := h.record("swe"); !found || !rec.Present() {
		t.Fatalf("the returning seat's record is %+v (found %v), want it registered afresh", rec, found)
	}
	h.ensure("swe")
	h.send("swe", "welcome back")
	if got := h.held("swe"); got != 1 {
		t.Fatalf("the returning seat's new inbox holds %d letters, want only the new one", got)
	}
}

// A sweep that marked a retirement and then died leaves the mark behind. A seat
// that returns must not wait on it for ever: past twice the budget, the
// retiring sweep can no longer act, so the node takes the record over.
func TestARegistrationTakesOverAnAbandonedRetirement(t *testing.T) {
	budget := 20 * time.Millisecond
	h := newMailboxHarness(t, func(o *maintenance.MailboxOptions) { o.RetireBudget = budget })
	h.removed()
	h.markRetiring("swe", base.Add(grace))

	started := time.Now()
	if err := h.m.Register(t.Context(), "swe"); err != nil {
		t.Fatalf("Register over an abandoned retirement: %v", err)
	}
	if waited := time.Since(started); waited < 2*budget {
		t.Fatalf("Register took an in-flight retirement over after %v, before twice its budget (%v)",
			waited, 2*budget)
	}
	if rec, _ := h.record("swe"); !rec.Present() {
		t.Fatalf("the abandoned retirement was not taken over: %+v", rec)
	}
}

func (h *mailboxHarness) markRetiring(handle string, at time.Time) {
	h.t.Helper()
	rec, found := h.record(handle)
	if !found {
		h.t.Fatalf("no record for %s to mark", handle)
	}
	rec.RetiringSince = at
	if _, ok, err := h.records.UpdateMailbox(h.t.Context(), rec); err != nil || !ok {
		h.t.Fatalf("mark %s retiring = (%v, %v)", handle, ok, err)
	}
}

// A sweep that died mid-retirement is resumed by a later one, but only once
// the mark is stale: a fresh mark is a peer's retirement still running, and
// resuming it would race that peer's deletes.
func TestASweepResumesAnAbandonedRetirementOnceItIsStale(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()
	marked := base.Add(grace)
	h.markRetiring("swe", marked)

	if n := h.mustTick(marked.Add(time.Minute)); n != 0 || h.held("swe") != 1 {
		t.Fatalf("a fresh retirement mark was resumed by another sweep (%d retired)", n)
	}
	if n := h.mustTick(marked.Add(maintenance.Interval + time.Minute)); n != 1 {
		t.Fatalf("a stale retirement mark was not resumed (%d retired)", n)
	}
	if h.inboxExists("swe") {
		t.Fatal("the resumed retirement left the inbox behind")
	}
}

// An abandoned retirement of a seat that has since come back may have deleted
// its inbox before it died. The sweep that clears the record restores it.
func TestAnAbandonedRetirementOfAReturningSeatRestoresItsInbox(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()
	marked := base.Add(grace)
	h.markRetiring("swe", marked)
	if _, err := h.queue.Queue.DeleteSubscription(t.Context(),
		topics.AgentInbox("swe"), topics.AgentInboxGroup("swe")); err != nil {
		t.Fatalf("simulate the partial retirement: %v", err)
	}

	h.roster.set("ceo", "swe")
	h.mustTick(marked.Add(maintenance.Interval + time.Minute))
	if rec, _ := h.record("swe"); !rec.Present() {
		t.Fatalf("the returning seat's record is %+v, want it present", rec)
	}
	if !h.inboxExists("swe") {
		t.Fatal("the returning seat's inbox was left deleted, so its mail is dropped")
	}
}

// A retirement whose record changed between its mark and its delete lost to a
// registration that took it over, which may have created the inbox before the
// retirement's delete landed. The inbox is restored rather than left deleted.
func TestARetirementThatLosesItsRecordRestoresTheInbox(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()

	release := make(chan struct{})
	entered := h.queue.holdNextDelete(release)
	done := make(chan int64, 1)
	go func() {
		n, err := h.tick(h.m, base.Add(grace+time.Minute))
		if err != nil {
			t.Errorf("tick: %v", err)
		}
		done <- n
	}()
	<-entered
	// The takeover a registering node performs once a retirement outlives
	// its budget, and the inbox it then creates.
	rec, _ := h.record("swe")
	rec.AbsentSince, rec.RetiringSince = time.Time{}, time.Time{}
	if _, ok, err := h.records.UpdateMailbox(t.Context(), rec); err != nil || !ok {
		t.Fatalf("take over = (%v, %v)", ok, err)
	}
	h.ensure("swe")
	close(release)

	if n := <-done; n != 0 {
		t.Fatalf("a retirement that lost its record reported %d retired", n)
	}
	if !h.inboxExists("swe") {
		t.Fatal("the returning seat's inbox was deleted by the retirement it overtook")
	}
	if rec, _ := h.record("swe"); !rec.Present() {
		t.Fatalf("the returning seat's record is %+v, want it present", rec)
	}
}

// A seat added back while the sweep is between its first roster read and the
// end of the retirement gets its inbox and its record restored, rather than
// waiting for its node's next apply.
func TestASeatAddedBackDuringItsRetirementIsRestored(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()

	release := make(chan struct{})
	entered := h.queue.holdNextDelete(release)
	done := make(chan int64, 1)
	go func() {
		n, err := h.tick(h.m, base.Add(grace+time.Minute))
		if err != nil {
			t.Errorf("tick: %v", err)
		}
		done <- n
	}()
	<-entered
	h.roster.set("ceo", "swe")
	close(release)

	if n := <-done; n != 1 {
		t.Fatalf("retired %d, want the old mailbox retired once", n)
	}
	if !h.inboxExists("swe") {
		t.Fatal("a seat added back during its retirement was left without an inbox")
	}
	if got := h.held("swe"); got != 0 {
		t.Fatalf("the restored inbox holds %d letters of the retired one", got)
	}
	if rec, found := h.record("swe"); !found || !rec.Present() {
		t.Fatalf("the restored seat's record is %+v (found %v), want it present", rec, found)
	}
}

// A retirement that cannot delete the subscriptions is undone to an ordinary
// absence, so the next tick retries it and a returning seat does not wait out a
// mark nothing is acting on.
func TestAFailedRetirementIsRetriedOnTheNextTick(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.removed()
	broker := errors.New("the broker refused the delete")
	h.queue.failDeletes(broker)

	if _, err := h.tick(h.m, base.Add(grace+time.Minute)); !errors.Is(err, broker) {
		t.Fatalf("tick = %v, want the broker's refusal", err)
	}
	if rec, _ := h.record("swe"); rec.Retiring() || !rec.AbsentSince.Equal(base) {
		t.Fatalf("a failed retirement left the record %+v, want it absent since %s and unmarked", rec, base)
	}

	h.queue.failDeletes(nil)
	if n := h.mustTick(base.Add(grace + time.Hour)); n != 1 {
		t.Fatalf("the retry retired %d mailboxes, want 1", n)
	}
}

// A mailbox created by a node whose registration failed, or by a build that
// predates the registry, is registered by the sweep while its seat is still in
// the company, so it can be retired if the seat is ever removed.
func TestASeatInTheRosterWithoutARecordIsRegistered(t *testing.T) {
	h := newMailboxHarness(t, nil)
	h.ensure("swe")
	h.roster.set("swe")
	h.mustTick(base)
	if rec, found := h.record("swe"); !found || !rec.Present() {
		t.Fatalf("the unregistered seat's record is %+v (found %v), want it registered", rec, found)
	}
}

func TestNewMailboxesNamesEveryMissingDependency(t *testing.T) {
	_, err := maintenance.NewMailboxes(maintenance.MailboxOptions{})
	if err == nil {
		t.Fatal("NewMailboxes accepted no dependencies")
	}
	for _, field := range []string{"Records", "Queue", "Leases", "Owner", "LeaseTTL", "Roster"} {
		if !strings.Contains(err.Error(), "MailboxOptions."+field) {
			t.Errorf("the error does not name %s: %v", field, err)
		}
	}
}
