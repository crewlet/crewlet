package checkrun

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE COMPANY'S REMOTE CELLS ARE CAPPED (U45 a). A remote (e2b) check's box
// runs on a provider's machines and is billed to the company's account
// there, so what bounds it is the company's, never one node's: at most
// code.checks.remote.max_running of them at once, and at most
// code.checks.remote.box_hours_per_day hours of them started in a UTC day,
// however many git nodes run checks.
//
// # A slot is a lease
//
// The boxes at once are max_running SLOTS in the coordination store — leases
// of the class check-remote, `check-remote:0` to `check-remote:<n-1>` — and a
// runner takes one before it claims a remote check, holds it renewed while the
// box lives and gives it back once the box is closed. The store says who
// holds what for the whole company now, which is the question the cap asks;
// a lease's TTL ([RemoteSlotTTL]) is what frees the slot of a runner that
// died holding it, with no sweep. REJECTED: a row on the checks log per
// running box — the cap would be decided by the repository's committing data
// node, one per repository, while the cap is the company's across all of them;
// and a counter of boxes running — a node that dies holding one never takes
// it back, where a lease lapses.
//
// # A day's box time is counted a minute at a time
//
// Every minute a remote box is held — its first minute when its slot is
// taken, then one each minute after while it runs — is charged to the fleet's
// shared counter ([coord.Counter.Allow]) in the UTC day's window, against
// box_hours_per_day × 60. A charge the day refuses is the day spent: no new
// remote box starts until the window turns over or the cap is raised, and a
// box already running runs to its own end, its later minutes refused rather
// than counted — so a day overruns its hours by at most what the boxes
// running when it filled take to finish. REJECTED: charging a box's whole
// timeout when it starts — a fifty-minute ceiling charged for a check that
// takes two would spend a day's hours on a fraction of its boxes.
//
// # The store that cannot say starts nothing
//
// A slot the store could not grant or refuse, and a charge it could not
// count, start no box: a cap the store cannot be asked about is a cap that
// cannot hold, and the check waits queued for the next walk, which asks
// again. A box already running keeps running through an outage — a renewal
// or a charge the store left unanswered is asked again at the next renewal —
// since killing it would throw away a measurement the outage has nothing to
// do with.
//
// # A lapsed slot is taken again
//
// A renewal the store answers NO is a tenure ended, never a blip: the slot
// lapsed, because its node's renewals did not reach the store for a TTL — an
// outage that long, a host asleep, a stall — and a lapsed lease is not
// renewable (re-acquiring is the only way back, under a new epoch, which is
// what fences the gap). From the moment it lapsed the company's listings stop
// counting the box, and other nodes start boxes in its room. So the box takes
// a slot again, at each renewal until it holds one: a slot a listing calls
// free under the cap it started at, or else one PAST it — an index no new box
// is offered, which every listing counts all the same — so the cap holds new
// boxes back the moment the box is counted again, and an overrun lasts only
// until the boxes started in the gap end. REJECTED: waiting for a slot under
// the cap to free — every box started in the gap, and every one started after
// them, competes with it for each slot that frees, so it could run on
// uncounted to its own timeout, an overrun as long as a box's life.
//
// # A full cap is listed once a freed slot
//
// A listing of the slots is a scan of the class's keys in the store's lease
// bucket and a read of its clock, so a walk lists them once ([SlotView]) and
// takes each box's slot from what that listing left free, and a node that
// found max_running full lists them again only once a slot it held is given
// back, or a walk backstop ([Timing.Backstop]) after it found them full: a
// capped fleet costs the store a listing a git node a backstop, never one a
// walk, which the queue's records nudge up to once a second ([WalkGap]). A
// slot another node frees is seen a backstop late at most, as a nudge the
// watch lost is. REJECTED: a
// watch on the class (coord's WatchLive) — every renewal of every slot
// reaches every git node, 0.8 a second at the default cap and 51 at its
// bound, to learn of a release that the node making it learns for free, while
// a lapse is no write and still needs the backstop.

// Leases is the coordination store's lease surface as a remote cell's slot is
// held on it — coord.Backend satisfies it.
type Leases interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
	TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error)
	Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error)
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)
}

// Minutes is the fleet's shared counter as a remote box's minutes are charged
// to it — coord.Counter satisfies it.
type Minutes interface {
	Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error)
}

// The remote caps' constants.
const (
	// RemoteSlotClass is the lease class of a remote cell's slot: a
	// slot's resource is `check-remote:<i>`.
	RemoteSlotClass coord.Class = "check-remote"

	// RemoteSlotTTL is SIXTY SECONDS: how long a slot outlives its holder's
	// last renewal. JUDGMENT — a runner that died holding a slot frees it
	// within a minute, which a check queued behind it waiting for its box
	// does not notice, while three renewals ([RemoteSlotRenew]) fit inside
	// it, so a renewal lost to one slow round trip costs nothing.
	RemoteSlotTTL = 60 * time.Second

	// RemoteSlotRenew is TWENTY SECONDS, a third of [RemoteSlotTTL]: how
	// often a held slot is renewed, and its box's minutes charged.
	RemoteSlotRenew = RemoteSlotTTL / 3

	// RemoteMinutesBucket is the counter the company's remote box-minutes
	// are charged to.
	RemoteMinutesBucket = "code-checks-remote-minutes"

	// RemoteMinutesWindow is the window box_hours_per_day is counted in: a
	// UTC DAY, midnight to midnight — the counter's longest
	// ([coord.MaxCounterWindow]).
	RemoteMinutesWindow = coord.MaxCounterWindow
)

// RemoteHold is why a remote check was left queued rather than started: one
// of the caps full, or the store unable to say. Its value is the reason the
// crewlet.code.checks.remote.capped counter names.
type RemoteHold string

// The holds.
const (
	// HoldMaxRunning is code.checks.remote.max_running boxes already
	// running, company-wide.
	HoldMaxRunning RemoteHold = "max_running"
	// HoldBoxHours is the UTC day's code.checks.remote.box_hours_per_day
	// spent.
	HoldBoxHours RemoteHold = "box_hours_per_day"
	// HoldUnknown is the coordination store unable to say either.
	HoldUnknown RemoteHold = "unknown"
)

// RemoteHolds are the three, in the order a slot's taking meets them.
func RemoteHolds() []RemoteHold { return []RemoteHold{HoldMaxRunning, HoldBoxHours, HoldUnknown} }

// Valid reports whether h is one of [RemoteHolds].
func (h RemoteHold) Valid() bool { return slices.Contains(RemoteHolds(), h) }

// RemoteSlots is one git node's side of the company's remote caps: the slots
// it takes and keeps, and the minutes it charges.
type RemoteSlots struct {
	nodeID   string
	leases   Leases
	minutes  Minutes
	renew    time.Duration
	backstop time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu sync.Mutex
	// spent is the UTC day's window this node last saw refuse a charge, and
	// the cap it was refused at: no box starts here while both hold, since a
	// window's count only rises — so a day spent asks the counter nothing
	// more until the day turns over or the cap moves.
	spent struct {
		window time.Time
		limit  int
	}
	// full is when this node last found max_running full, on the wall
	// clock's monotonic reading, and the cap it was full at: no walk lists
	// the slots again while the cap is at most that, within a backstop of
	// it, and no slot this node held has been given back since. Zero is
	// none remembered.
	full struct {
		at    time.Time
		limit int
	}
	// frees counts the slots this node has given back, so a listing read
	// before one is never remembered as full after it.
	frees uint64
}

// newRemoteSlots builds the node's remote caps over its coordination store,
// a full cap remembered for at most backstop.
func newRemoteSlots(nodeID string, leases Leases, minutes Minutes, renew, backstop time.Duration,
	now func() time.Time, log *slog.Logger) *RemoteSlots {

	return &RemoteSlots{nodeID: nodeID, leases: leases, minutes: minutes, renew: renew, backstop: backstop,
		now: now, log: log}
}

// SlotView is one walk's reading of the company's live remote slots: listed
// at the first remote check the walk takes a slot for, then kept by the walk
// — each slot it takes, and each another runner took first, marked held — so
// a walk that starts several boxes lists them once, never once a box. A slot
// freed meanwhile is the next walk's to see. The zero value is unlisted.
type SlotView struct {
	held map[string]bool
	// frees is the node's count of slots given back as the listing began.
	frees uint64
}

// RemoteSlot is a slot this node holds for one remote check's box: renewed,
// and its minutes charged, until [RemoteSlot.Release].
type RemoteSlot struct {
	s *RemoteSlots
	// maxRunning and limit are the caps the box was started under: the
	// slots a lapsed tenure is taken again among, and the minutes a day
	// holds.
	maxRunning int
	limit      int
	taken      time.Time

	// resource, owner and epoch are the slot's tenure — resource empty
	// where it was lost and not yet taken again. keep alone writes them
	// once it runs, and Release reads them only once keep has stopped and
	// closed done, which orders every write before that read.
	resource string
	owner    string
	epoch    int64

	once sync.Once
	stop chan struct{}
	done chan struct{}
}

// Take takes a slot for one remote box under the caps maxRunning and
// minutesPerDay, charging its first minute — or answers why not: a cap full,
// or [HoldUnknown] with the store's error. view is the walk's reading of the
// slots, listed here where the walk has none yet. A slot it answers is kept
// until its Release.
func (s *RemoteSlots) Take(ctx context.Context, view *SlotView, maxRunning, minutesPerDay int) (*RemoteSlot,
	RemoteHold, error) {

	now := s.now()
	if minutesPerDay <= 0 || s.daySpent(now, minutesPerDay) {
		return nil, HoldBoxHours, nil
	}
	if maxRunning <= 0 || s.capFull(maxRunning) {
		return nil, HoldMaxRunning, nil
	}
	if view.held == nil {
		view.frees = s.freesNow()
		live, err := s.leases.ListLive(ctx, RemoteSlotClass)
		if err != nil {
			return nil, HoldUnknown, fmt.Errorf("list the remote check slots held: %w", err)
		}
		view.held = heldSlots(live)
	}
	// EVERY LIVE SLOT COUNTS, whatever its index: a cap lowered while the
	// boxes of the old one run is held until enough of them end, and a box
	// whose lapsed slot was taken again past the cap is counted.
	if len(view.held) >= maxRunning {
		s.fullAt(view, maxRunning)
		return nil, HoldMaxRunning, nil
	}
	for _, resource := range freeSlots(view.held, 0, maxRunning) {
		owner, epoch, won, err := s.acquire(ctx, resource)
		if err != nil {
			return nil, HoldUnknown, fmt.Errorf("take the remote check slot %s: %w", resource, err)
		}
		// HELD NOW, by this runner or by one that took it since the listing.
		view.held[resource] = true
		if !won {
			if len(view.held) >= maxRunning {
				break
			}
			continue
		}
		slot := &RemoteSlot{s: s, maxRunning: maxRunning, limit: minutesPerDay, taken: now, resource: resource,
			owner: owner, epoch: epoch, stop: make(chan struct{}), done: make(chan struct{})}
		// THE FIRST MINUTE, before the box: a day spent starts nothing.
		ok, err := s.charge(ctx, now, minutesPerDay)
		if err != nil || !ok {
			slot.give(ctx)
			delete(view.held, resource)
			if err != nil {
				return nil, HoldUnknown, fmt.Errorf("charge a remote check box's first minute: %w", err)
			}
			return nil, HoldBoxHours, nil
		}
		go slot.keep()
		return slot, "", nil
	}
	// EVERY SLOT THE LISTING CALLED FREE WAS TAKEN BY ANOTHER RUNNER since.
	s.fullAt(view, maxRunning)
	return nil, HoldMaxRunning, nil
}

// capFull reports whether this node found max_running full at a cap of
// maxRunning or more within a backstop, with no slot of its own given back
// since — a cap the next listing would find full too, so it is not asked.
func (s *RemoteSlots) capFull(maxRunning int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.full.at.IsZero() && maxRunning <= s.full.limit && time.Since(s.full.at) < s.backstop
}

// fullAt remembers max_running found full at maxRunning, now, by view — not
// where this node gave a slot back after view was listed, which view cannot
// have seen free.
func (s *RemoteSlots) fullAt(view *SlotView, maxRunning int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frees != view.frees {
		return
	}
	s.full.at, s.full.limit = time.Now(), maxRunning
}

// freesNow is how many slots this node has given back.
func (s *RemoteSlots) freesNow() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frees
}

// freed forgets a full cap: a slot this node held was given back, which is
// room the next walk can take.
func (s *RemoteSlots) freed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.full.at = time.Time{}
	s.frees++
}

// acquire claims resource for one tenure, under an owner of its own — the
// node, and the tenure — answering its owner and epoch, or won false where
// another runner holds it.
func (s *RemoteSlots) acquire(ctx context.Context, resource string) (owner string, epoch int64, won bool,
	err error) {

	owner = s.nodeID + "/" + uuid.NewString()
	lease, _, err := s.leases.TryAcquire(ctx, resource, coord.AcquireOptions{Owner: owner, TTL: RemoteSlotTTL,
		Preferred: s.nodeID, Ungated: true})
	if err != nil || lease == nil {
		return "", 0, false, err
	}
	return owner, lease.Epoch, true, nil
}

// heldSlots is the resources of live, as a set.
func heldSlots(live []coord.Lease) map[string]bool {
	held := make(map[string]bool, len(live))
	for _, l := range live {
		held[l.Resource] = true
	}
	return held
}

// freeSlots is the slots from index lo up to hi that held does not hold, in
// a RANDOM ORDER, so two runners taking slots at once rarely ask for the same
// one, and one that loses asks for the next.
func freeSlots(held map[string]bool, lo, hi int) []string {
	var free []string
	for i := lo; i < hi; i++ {
		if r := RemoteSlotClass.Resource(strconv.Itoa(i)); !held[r] {
			free = append(free, r)
		}
	}
	rand.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] })
	return free
}

// charge charges one box-minute at now against limit, remembering a day it
// finds spent.
func (s *RemoteSlots) charge(ctx context.Context, now time.Time, limit int) (bool, error) {
	ok, err := s.minutes.Allow(ctx, RemoteMinutesBucket, limit, RemoteMinutesWindow, now)
	if err == nil && !ok {
		window := now.Truncate(RemoteMinutesWindow)
		s.mu.Lock()
		fresh := !s.spent.window.Equal(window) || s.spent.limit != limit
		s.spent.window, s.spent.limit = window, limit
		s.mu.Unlock()
		if fresh {
			// SAID ONCE A DAY AND CAP on each node that finds it spent.
			s.log.Info("code_checks_remote_day_spent", "box_hours_per_day", limit/60,
				"resets", window.Add(RemoteMinutesWindow), "detail", "the company's remote check box-hours for "+
					"this UTC day are spent: no new e2b check starts until the next UTC midnight, or until "+
					"code.checks.remote.box_hours_per_day is raised; boxes already running run to their end")
		}
	}
	return ok, err
}

// daySpent reports whether the UTC day holding now was refused a charge at
// limit.
func (s *RemoteSlots) daySpent(now time.Time, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent.window.Equal(now.Truncate(RemoteMinutesWindow)) && s.spent.limit >= limit
}

// keep holds the slot — renewed, or taken again once lost — and charges its
// box's minutes until its release, and charges the minutes begun since its
// last renewal as it stops: the box is closed by then, so they are the last
// of its time.
func (sl *RemoteSlot) keep() {
	defer close(sl.done)
	s := sl.s
	charged := 1
	unanswered := false
	tick := time.NewTicker(s.renew)
	defer tick.Stop()
	for {
		select {
		case <-sl.stop:
			// THE BOX'S LAST MINUTES, begun since the last renewal: a box
			// closed within a renewal of a minute's start was held in that
			// minute, so it is charged — bounded as a renewal's round trip,
			// under no caller's cancellation.
			ctx, cancel := context.WithTimeout(context.Background(), s.renew)
			sl.chargeDue(ctx, charged, true)
			cancel()
			return
		case <-tick.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.renew)
		unanswered = sl.hold(ctx, unanswered)
		charged = sl.chargeDue(ctx, charged, false)
		cancel()
	}
}

// hold keeps the box counted under the company's cap for one renewal: its
// slot renewed, or — its tenure lost — a slot taken again. said is whether a
// renewal the store left unanswered was said since the last it answered, and
// hold answers it for the next renewal, so an outage is said once, not every
// renewal.
func (sl *RemoteSlot) hold(ctx context.Context, said bool) bool {
	s := sl.s
	if sl.resource != "" {
		ok, err := s.leases.Renew(ctx, sl.resource, sl.owner, sl.epoch, RemoteSlotTTL)
		if err != nil {
			// UNANSWERED, NOT LOST: the tenure may well stand, so the next
			// renewal asks again.
			if !said {
				s.log.Warn("code_check_remote_slot_not_renewed", "slot", sl.resource, "error", err.Error(),
					"detail", "the coordination store did not answer the box's renewal; it is asked again at "+
						"every renewal, and a slot that lapses meanwhile is taken again")
			}
			return true
		}
		if ok {
			return false
		}
		// THE TENURE ENDED: said once, as it is found.
		s.log.Warn("code_check_remote_slot_lost", "slot", sl.resource, "detail", "the box's slot lapsed — its "+
			"node's renewals did not reach the coordination store for "+RemoteSlotTTL.String()+" — so the "+
			"company's remote cap stopped counting it, and other boxes may have started in its room; the box "+
			"runs on, and a slot is taken again for it at each renewal until one is")
		sl.resource, sl.owner, sl.epoch = "", "", 0
	}
	sl.retake(ctx)
	return false
}

// retake takes a slot again for a box whose tenure was lost: the first it
// wins of the slots a listing calls free under the cap the box started at,
// then of the next as many as the listing holds and one more past it — of
// which the listing leaves one free at least — so the box is counted again
// whatever filled the cap meanwhile. A store that cannot say, or every slot
// taken first, leaves it to the next renewal, unsaid: the loss was said.
func (sl *RemoteSlot) retake(ctx context.Context) {
	s := sl.s
	live, err := s.leases.ListLive(ctx, RemoteSlotClass)
	if err != nil {
		return
	}
	held := heldSlots(live)
	under := freeSlots(held, 0, sl.maxRunning)
	for i, resource := range slices.Concat(under, freeSlots(held, sl.maxRunning, sl.maxRunning+len(live)+1)) {
		owner, epoch, won, err := s.acquire(ctx, resource)
		if err != nil {
			return
		}
		if !won {
			continue
		}
		sl.resource, sl.owner, sl.epoch = resource, owner, epoch
		s.log.Info("code_check_remote_slot_retaken", "slot", resource, "over_cap", i >= len(under), "detail",
			"the box whose slot lapsed is counted under the company's remote cap again; over_cap is a slot past "+
				"the cap, taken because boxes started in the gap filled it, which holds new boxes back until "+
				"enough of them end")
		return
	}
}

// chargeDue charges every minute of the box begun since the charged minutes
// before it, and answers how many are charged now: a charge the store could
// not count is asked again at the next renewal rather than lost, and a day
// found spent counts every minute begun as charged — the box runs to its end,
// its later minutes refused rather than counted, and new boxes wait. closing
// is the box closed, with no renewal left to ask again at.
func (sl *RemoteSlot) chargeDue(ctx context.Context, charged int, closing bool) int {
	s := sl.s
	now := s.now()
	for due := int(now.Sub(sl.taken)/time.Minute) + 1; charged < due; charged++ {
		ok, err := s.charge(ctx, now, sl.limit)
		if err != nil {
			detail := "charged again at the next renewal"
			if closing {
				detail = "the box has closed, so its last minutes go uncharged"
			}
			s.log.Warn("code_check_remote_minute_uncharged", "slot", sl.resource, "error", err.Error(),
				"detail", detail)
			return charged
		}
		if !ok {
			return due
		}
	}
	return charged
}

// Release gives the slot back once its box is closed: its keeping stopped and
// its lease released, under a context no caller's cancellation reaches, and
// then a full cap this node remembered forgotten — after the release, so no
// listing between the two is remembered full again. Safe to call more than
// once, and on nil.
func (sl *RemoteSlot) Release() {
	if sl == nil {
		return
	}
	sl.once.Do(func() {
		close(sl.stop)
		<-sl.done
		sl.give(context.Background())
		sl.s.freed()
	})
}

// give releases the slot's lease: at its own epoch, never another tenure's,
// and nothing where its tenure was lost and not taken again.
func (sl *RemoteSlot) give(ctx context.Context) {
	if sl.resource == "" {
		return
	}
	if _, err := sl.s.leases.Release(context.WithoutCancel(ctx), sl.resource, sl.owner, sl.epoch); err != nil {
		sl.s.log.Warn("code_check_remote_slot_not_released", "slot", sl.resource, "error", err.Error(),
			"detail", "it frees itself when its lease lapses")
	}
}
