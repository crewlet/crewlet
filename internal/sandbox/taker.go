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
// ITS TTL IS THE SEAT LEASE TTL THE DEPLOYMENT CONFIGURES, renewed every TTL /
// [seat.HeartbeatRatio] — presence's own cadence, for presence's reason: two
// renewals may fail in a row with a full interval left to recover in, and a
// taker that stopped is read as gone within one TTL of its last renewal, the
// bound a dead node's seats already move in. Shorter would fence a healthy
// node's keeps on a store blip; longer is how long a stopped taker's capture
// holds the turn's end and a relaunch.
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

	mu sync.Mutex
	// epoch is the tenure held, 0 for none; through is the end of the
	// exclusive window its last confirmed claim or renewal certified.
	epoch   int64
	through time.Time

	// stop ends the renewals, waits for the one under way, and releases the
	// tenure held — once, however often it is asked.
	stop func()
}

// takerHold is the tenure of the taker lease a capture is stamped under: its
// holder and its fencing token ([SessionCapture.Taker], [SessionCapture.TakerEpoch]).
type takerHold struct {
	holder string
	epoch  int64
}

// takerUnheld is why a take is refused untaken where this process holds no
// taker lease to name on its stamp ([Coordinator.takeCapture]).
const takerUnheld = "this node holds no taker lease whose last renewal still covers it — its claim or its " +
	"renewals went unanswered — so a capture it began could not be told apart from one whose taker " +
	"stopped, and none is begun"

// newTakerLease claims holder's taker lease in leases at ttl — zero takes
// [seat.SeatLeaseTTL] — and goes on renewing it on its own goroutine until
// stop.
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
	t := &takerLease{
		leases: leases, holder: holder, resource: coord.TakerResource(holder),
		ttl: ttl, every: ttl / seat.HeartbeatRatio,
	}
	life, end := context.WithCancel(context.Background())
	t.beat(life)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.hold(life)
	}()
	t.stop = sync.OnceFunc(func() {
		end()
		<-done
		t.release()
	})
	return t
}

// hold renews the lease every interval, or claims it again where it lapsed,
// until ctx ends.
func (t *takerLease) hold(ctx context.Context) {
	tick := time.NewTicker(t.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
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
		case err != nil:
			log.WarnContext(ctx, "executor_taker_lease_unanswered", "taker", t.holder, "taker_epoch", epoch,
				"error", err.Error(),
				"detail", "the coordination store did not answer this node's taker lease renewal; its keeps "+
					"go on while the last renewal covers them, and stop once it does not")
			return
		case renewed:
			t.confirm(epoch, asked)
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
	case err != nil:
		log.WarnContext(ctx, "executor_taker_lease_unanswered", "taker", t.holder, "error", err.Error(),
			"detail", "the coordination store did not answer this node's taker lease claim; no capture is "+
				"begun here until it does")
	case lease == nil:
		log.WarnContext(ctx, "executor_taker_lease_unanswered", "taker", t.holder, "refusal", string(refused),
			"detail", "the coordination store refused this node's taker lease claim; no capture is begun "+
				"here until it is held")
	default:
		t.confirm(lease.Epoch, asked)
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
	return takerHold{holder: t.holder, epoch: t.epoch}, true
}

// fences says why a keep of capture may go no further, and "" where it may:
// this process holds the lease at the epoch the capture's stamp names, inside
// the window of its last confirmed renewal. Never a store read — see the type
// doc.
func (t *takerLease) fences(capture SessionCapture) string {
	hold, ok := t.held()
	switch {
	case !ok:
		return fmt.Sprintf("its taker %s holds no lease whose last renewal still covers the keep", nodeOf(t.holder))
	case capture.Taker != hold.holder || capture.TakerEpoch != hold.epoch:
		return fmt.Sprintf("its taker %s holds its lease at epoch %d, past the %d the capture was taken under",
			nodeOf(t.holder), hold.epoch, capture.TakerEpoch)
	}
	return ""
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

// release gives the tenure held back, on a budget of its own — one renewal
// interval: a release the store does not answer by then falls back to the
// lapse at the TTL, which is what a crash leaves anyway.
func (t *takerLease) release() {
	t.mu.Lock()
	epoch := t.epoch
	t.epoch, t.through = 0, time.Time{}
	t.mu.Unlock()
	if epoch == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.every)
	defer cancel()
	if _, err := t.leases.Release(ctx, t.resource, t.holder, epoch); err != nil {
		log.WarnContext(ctx, "executor_taker_lease_unreleased", "taker", t.holder, "taker_epoch", epoch,
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
// attempt at its record ([takerLease.fences]): none for a stamp that names no
// taker, which nothing settles under it.
func (c *Coordinator) keepFence(stamp SessionCapture) func() string {
	if c.taker == nil || stamp.Taker == "" {
		return func() string { return "" }
	}
	return func() string { return c.taker.fences(stamp) }
}

// fenceKeep ends a keep its own taker lease no longer covers, at step, with the
// outcome it knows — refused where no record was sent, unresolved where one was
// and went unanswered — stamped and said; nothing more is uploaded or recorded.
func (c *Coordinator) fenceKeep(ctx context.Context, run PendingRun, stamp SessionCapture, step string,
	outcome CaptureState, why string) *SessionKept {

	stamp.State = outcome
	switch {
	case outcome == CaptureUnresolved:
		why += ", after its record was sent and not answered, so it was asked no more"
	case step == "upload":
		why += ", so it was neither uploaded nor recorded"
	default:
		why += ", so its upload was not recorded"
	}
	stamp.Reason = stampReason(why)
	log.WarnContext(ctx, "executor_session_fenced", "turn_id", run.TurnID, "agent", run.AgentHandle,
		"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "step", step,
		"taker_epoch", stamp.TakerEpoch, "state", string(outcome), "detail", stamp.Reason)
	c.stampSettled(ctx, time.Time{}, run.TurnID, stamp)
	return stamp.kept()
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
