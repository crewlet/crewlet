package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat"
)

// TakerLeases is the coordination store as a coordinator holds its own TAKER
// LEASE in it and reads another process's ([coord.ClassTaker]; see session.go,
// "A capture whose taker stopped is settled from the log, not waited out"):
// four methods of [coord.Backend], named by the consumer as this package uses
// them. Three-valued by construction, as every answer of the store is: a lease
// held, none held, and an error that says nothing is known — which a fence
// reads as no lease and a settle as no answer, each the direction that keeps
// a capture from being refused under a taker still keeping it.
type TakerLeases interface {
	TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error)
	Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error)
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)
	Get(ctx context.Context, resource string) (*coord.Lease, error)
}

// takerLease is this process's taker lease: claimed as its coordinator is
// built, renewed every renewal interval, claimed again at a new epoch once it
// lapses, and released by [Coordinator.Stop] once the last keep has ended.
//
// ITS TTL IS THE SEAT LEASE TTL THE DEPLOYMENT CONFIGURES, so a taker that
// stopped is read as gone within one TTL of its last renewal — the bound a dead
// node's seats already move in. Shorter would fence a healthy node's keeps on
// a store blip; longer is how long a stopped taker's capture holds the turn's
// end and a relaunch. HELD TO [coord.MaxDutyTTL], the longest lease the store
// keeps outside seats and presence ([coord.CheckLeaseTTL]), which only a seat
// lease TTL past three hours reaches: asked for more, the store refused every
// claim, and no capture was begun anywhere in the deployment.
//
// RENEWED [coord.KeptRenewals] TIMES A TTL, not on presence's three: a keep goes
// on only inside the window its process's last confirmed renewal covers
// (below), and renewed a third of a TTL apart that window held two intervals,
// so the renewal after one the store did not answer was asked as the window
// closed — one blip, a leader election, refused every take and fenced every
// keep under a lease no peer could yet read as lapsed. A quarter apart, the
// renewal after an unanswered one is asked inside the window with a whole
// interval to answer in.
//
// WHAT IT HOLDS IS ITS OWN WORD on the tenure: the epoch its last confirmed
// claim or renewal answered, and the exclusive window that renewal certifies
// (coord.ExclusiveThrough, measured from the asking on this process's clock,
// one renewal interval its margin). No store is read to answer it, so a fence
// costs nothing and a store that stops answering closes the window rather than
// keeping it open.
type takerLease struct {
	leases   TakerLeases
	holder   string
	resource string
	ttl      time.Duration
	// every is the renewal interval, and the margin the exclusive window is
	// held short of the TTL by.
	every time.Duration

	// life is the renewals' own lifetime, from the claim to
	// [takerLease.stop], which ends it (end), waits for the renewal under way
	// (done) and releases the tenure held — once (stopped), however often it
	// is asked. Not the coordinator's lifetime, which its stop ends first:
	// the lease is held until the last keep has ended, after it.
	life    context.Context
	end     context.CancelFunc
	done    chan struct{}
	stopped sync.Once

	mu sync.Mutex
	// epoch is the tenure held, 0 for none; through is the end of the
	// exclusive window its last confirmed claim or renewal certified.
	epoch   int64
	through time.Time
}

// takerHold is the tenure of the taker lease a capture is stamped under: its
// holder and its fencing token ([SessionCapture.Taker],
// [SessionCapture.TakerEpoch]), and the end of the exclusive window its
// process holds it in now — zero where no lease is wired, which bounds
// nothing.
type takerHold struct {
	holder  string
	epoch   int64
	through time.Time
}

// takerUnheld is why a take is refused untaken where this process holds no
// taker lease to name on its stamp ([Coordinator.takeCapture]).
const takerUnheld = "this node holds no taker lease whose last renewal still covers it — its claim or its " +
	"renewals went unanswered — so a capture it began could not be told apart from one whose taker " +
	"stopped, and none is begun"

// newTakerLease claims holder's taker lease in leases at ttl — zero takes
// [seat.SeatLeaseTTL], and nothing is asked past [coord.MaxDutyTTL] — and goes
// on renewing it on its own goroutine until [takerLease.stop].
//
// THE FIRST CLAIM BEFORE IT RETURNS, bounded by one renewal interval as every
// beat is: a coordinator begins no capture before its lease is held, so one
// built holding it begins its first at once, where a claim left to the
// goroutine refused every take the coordinator was asked for meanwhile. A
// store that does not answer leaves the claim to the next beat.
func newTakerLease(leases TakerLeases, holder string, ttl time.Duration) *takerLease {
	if ttl <= 0 {
		ttl = seat.SeatLeaseTTL
	}
	ttl = min(ttl, coord.MaxDutyTTL)
	t := &takerLease{
		leases: leases, holder: holder, resource: coord.TakerResource(holder),
		ttl: ttl, every: ttl / coord.KeptRenewals, done: make(chan struct{}),
	}
	t.life, t.end = context.WithCancel(context.Background())
	t.beat(t.life)
	go t.hold(t.life)
	return t
}

// hold renews the lease every interval, or claims it again where it lapsed,
// until ctx ends, and says so (done).
func (t *takerLease) hold(ctx context.Context) {
	defer close(t.done)
	tick := time.NewTicker(t.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// A STOP THAT CAME WITH THE TICK ends the renewals here: the
			// select takes either at random, and a beat begun under a stop
			// is cut mid-call.
			if ctx.Err() != nil {
				return
			}
			t.beat(ctx)
		}
	}
}

// beat renews the tenure held, or claims one where none is. A renewal the store
// answers NO to is a lease that lapsed, never renewed again, only claimed at a
// new epoch — at once, in the same beat, since every keep under the old one is
// fenced from then on. A renewal or a claim the store does not answer changes
// nothing held: the window of the last confirmed renewal closes on its own.
// Each call is bounded by one interval, so a store that hangs never delays the
// next beat.
//
// A CALL THE STOP CUT says nothing of the store, and is not said to: the stop
// ends ctx while a beat may be under way, and that beat's call fails on ctx
// rather than on the store. A claim the store answered held all the same is
// confirmed, so the stop's release gives it back.
func (t *takerLease) beat(ctx context.Context) {
	call, cancel := context.WithTimeout(ctx, t.every)
	defer cancel()
	t.mu.Lock()
	epoch := t.epoch
	t.mu.Unlock()
	if epoch != 0 {
		asked := time.Now()
		renewed, err := t.leases.Renew(call, t.resource, t.holder, epoch, t.ttl)
		switch {
		case err == nil && renewed:
			t.confirm(epoch, asked)
			return
		case ctx.Err() != nil:
			return
		case err != nil:
			log.WarnContext(ctx, "executor_taker_lease_unanswered", "taker", t.holder, "taker_epoch", epoch,
				"error", err.Error(),
				"detail", "the coordination store did not answer this node's taker lease renewal; its keeps "+
					"go on while the last renewal covers them, and stop once it does not")
			return
		}
		t.mu.Lock()
		if t.epoch == epoch {
			t.epoch, t.through = 0, time.Time{}
		}
		t.mu.Unlock()
		log.WarnContext(ctx, "executor_taker_lease_lost", "taker", t.holder, "taker_epoch", epoch,
			"detail", "this node's taker lease lapsed before it was renewed, so every keep begun under it "+
				"uploads and records nothing more, and its captures are settled by whichever node waits "+
				"on them; the lease is claimed again at a new epoch")
	}
	asked := time.Now()
	lease, refused, err := t.leases.TryAcquire(call, t.resource, coord.AcquireOptions{
		// UNGATED: a taker lease says nothing about what holding a seat
		// means, and a capture is kept through a rolling upgrade as it is
		// at any other time (coord.ClassTaker).
		Owner: t.holder, TTL: t.ttl, Ungated: true,
	})
	switch {
	case err == nil && lease != nil:
		t.confirm(lease.Epoch, asked)
	case ctx.Err() != nil:
		// CUT BY THE STOP: what the store made of the claim is unknown, and a
		// tenure no answer named is no keep's to stamp — it lapses on its TTL.
	case err != nil:
		log.WarnContext(ctx, "executor_taker_lease_unanswered", "taker", t.holder, "error", err.Error(),
			"detail", "the coordination store did not answer this node's taker lease claim; no capture is "+
				"begun here until it does")
	default:
		log.WarnContext(ctx, "executor_taker_lease_refused", "taker", t.holder, "refusal", string(refused),
			"detail", "the coordination store refused this node's taker lease claim; no capture is begun "+
				"here until it is held")
	}
}

// confirm records a claim or a renewal the store answered as held at epoch,
// asked at asked: the tenure, and the window it certifies.
func (t *takerLease) confirm(epoch int64, asked time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.epoch, t.through = epoch, coord.ExclusiveThrough(asked, t.ttl, t.every)
}

// held is the tenure a capture may be stamped under now, and false where none
// is: no tenure, or the window of its last confirmed renewal closed.
func (t *takerLease) held() (takerHold, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.epoch == 0 || !time.Now().Before(t.through) {
		return takerHold{}, false
	}
	return takerHold{holder: t.holder, epoch: t.epoch, through: t.through}, true
}

// fence is the end of the window a keep of capture may go on in now, and why it
// may go no further — "" where it may: this process holds the lease at the
// epoch the capture's stamp names, inside the window of its last confirmed
// renewal, which ends at the instant answered. Never a store read — see the
// type doc.
func (t *takerLease) fence(capture SessionCapture) (time.Time, string) {
	hold, ok := t.held()
	switch {
	case !ok:
		return time.Time{}, fmt.Sprintf("its taker %s holds no lease whose last renewal still covers the keep",
			nodeOf(t.holder))
	case capture.Taker != hold.holder || capture.TakerEpoch != hold.epoch:
		return time.Time{}, fmt.Sprintf("its taker %s holds its lease at epoch %d, past the %d the capture was "+
			"taken under", nodeOf(t.holder), hold.epoch, capture.TakerEpoch)
	}
	return hold.through, ""
}

// gone reports whether the taker capture names has stopped keeping it: its
// lease lapsed or released — read as none held — or held at an epoch other
// than the capture's. One coordination read; an error is UNKNOWN, never gone.
func (t *takerLease) gone(ctx context.Context, capture SessionCapture) (bool, error) {
	lease, err := t.leases.Get(ctx, coord.TakerResource(capture.Taker))
	if err != nil {
		return false, err
	}
	return lease == nil || lease.Owner != capture.Taker || lease.Epoch != capture.TakerEpoch, nil
}

// stop ends the renewals, waits for the one under way, and releases the tenure
// held ([takerLease.release]) — once, however often it is asked.
func (t *takerLease) stop() {
	t.stopped.Do(func() {
		t.end()
		<-t.done
		t.release(t.life)
	})
}

// release gives the tenure held back, on ctx — the renewals' own lifetime,
// ended by then — with its cancellation removed and one renewal interval as
// its bound.
//
// A STEP OF THE STOP IN AN ENGINE, where the lease store the lease was claimed
// in draws every give-back made once the teardown has begun from the stop's
// one allowance (internal/engine's holdLeases): no stop's context reaches this
// one, which was made when the coordinator was built, so the frame that holds
// both the store and the stop bounds it there, and a node whose store has gone
// spends that one allowance rather than this release's interval beside it.
// The interval bounds a coordinator stopped outside an engine's stop. A
// release the store does not answer within it falls back to the lapse at the
// TTL, which is what a crash leaves anyway.
func (t *takerLease) release(ctx context.Context) {
	t.mu.Lock()
	epoch := t.epoch
	t.epoch, t.through = 0, time.Time{}
	t.mu.Unlock()
	if epoch == 0 {
		return
	}
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.every)
	defer cancel()
	if _, err := t.leases.Release(call, t.resource, t.holder, epoch); err != nil {
		log.WarnContext(call, "executor_taker_lease_unreleased", "taker", t.holder, "taker_epoch", epoch,
			"error", err.Error(),
			"detail", "the coordination store did not answer this node's taker lease release; the lease "+
				"lapses on its TTL, and whatever waits on a capture this node took settles it then")
	}
}

// heldTaker is the tenure a capture this coordinator takes is stamped under:
// an empty one, held, where no taker lease is wired — a test's coordinator,
// whose stamps name none, as an older build's do.
func (c *Coordinator) heldTaker() (takerHold, bool) {
	if c.taker == nil {
		return takerHold{}, true
	}
	return c.taker.held()
}

// keepFence is the fence a keep of stamp asks before its upload and before each
// attempt at its record ([takerLease.fence]): none, with no window to end in,
// for a stamp that names no taker, which nothing settles under it.
func (c *Coordinator) keepFence(stamp SessionCapture) func() (time.Time, string) {
	if c.taker == nil || stamp.Taker == "" {
		return func() (time.Time, string) { return time.Time{}, "" }
	}
	return func() (time.Time, string) { return c.taker.fence(stamp) }
}

// fenceKeep ends a keep its own taker lease no longer covers, at step, with the
// outcome it knows — refused where no record was sent, unresolved where one was
// and went unanswered — stamped and said, as every keep's outcome is
// ([Coordinator.finishCapture], `executor_session_not_kept`), and the fence
// besides (`executor_session_fenced`); nothing more is uploaded or recorded.
func (c *Coordinator) fenceKeep(ctx context.Context, run PendingRun, stamp SessionCapture, step string,
	outcome CaptureState, why string) *SessionKept {

	switch {
	case outcome == CaptureUnresolved:
		why += ", after its record was sent and not answered, so it was asked no more"
	case step == "upload":
		why += ", so it was neither uploaded nor recorded"
	default:
		why += ", so its upload was not recorded"
	}
	log.WarnContext(ctx, "executor_session_fenced", "turn_id", run.TurnID, "agent", run.AgentHandle,
		"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "step", step,
		"taker_epoch", stamp.TakerEpoch, "state", string(outcome), "detail", stampReason(why))
	return c.finishCapture(ctx, time.Time{}, run, stamp, outcome, why)
}

// takerGone reports whether capture's taker is gone ([takerLease.gone]): a
// begun capture whose stamp names a taker, read through the store this
// coordinator holds its own lease in. False for anything else — a stamp an
// older build wrote, and a coordinator with no lease store, wait by age — and
// an error, never gone, where the store cannot say.
func (c *Coordinator) takerGone(ctx context.Context, capture SessionCapture) (bool, error) {
	if c.taker == nil || capture.State != CaptureBegun || capture.Taker == "" {
		return false, nil
	}
	return c.taker.gone(ctx, capture)
}
