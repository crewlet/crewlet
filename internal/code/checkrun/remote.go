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
// again. A box already running keeps running through an outage — its renewals
// and its charges are asked again at the next renewal — since killing it would
// throw away a measurement the outage has nothing to do with.

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
	nodeID  string
	leases  Leases
	minutes Minutes
	renew   time.Duration
	now     func() time.Time
	log     *slog.Logger

	mu sync.Mutex
	// spent is the UTC day's window this node last saw refuse a charge, and
	// the cap it was refused at: no box starts here while both hold, since a
	// window's count only rises — so a day spent asks the counter nothing
	// more until the day turns over or the cap moves.
	spent struct {
		window time.Time
		limit  int
	}
}

// newRemoteSlots builds the node's remote caps over its coordination store.
func newRemoteSlots(nodeID string, leases Leases, minutes Minutes, renew time.Duration, now func() time.Time,
	log *slog.Logger) *RemoteSlots {

	return &RemoteSlots{nodeID: nodeID, leases: leases, minutes: minutes, renew: renew, now: now, log: log}
}

// RemoteSlot is a slot this node holds for one remote check's box: renewed,
// and its minutes charged, until [RemoteSlot.Release].
type RemoteSlot struct {
	s        *RemoteSlots
	resource string
	owner    string
	epoch    int64
	limit    int
	taken    time.Time

	once sync.Once
	stop chan struct{}
	done chan struct{}
}

// Take takes a slot for one remote box under the caps maxRunning and
// minutesPerDay, charging its first minute — or answers why not: a cap full,
// or [HoldUnknown] with the store's error. A slot it answers is kept until
// its Release.
func (s *RemoteSlots) Take(ctx context.Context, maxRunning, minutesPerDay int) (*RemoteSlot, RemoteHold, error) {
	now := s.now()
	if minutesPerDay <= 0 || s.daySpent(now, minutesPerDay) {
		return nil, HoldBoxHours, nil
	}
	if maxRunning <= 0 {
		return nil, HoldMaxRunning, nil
	}
	live, err := s.leases.ListLive(ctx, RemoteSlotClass)
	if err != nil {
		return nil, HoldUnknown, fmt.Errorf("list the remote check slots held: %w", err)
	}
	// EVERY LIVE SLOT COUNTS, whatever its index: a cap lowered while the
	// boxes of the old one run is held until enough of them end.
	if len(live) >= maxRunning {
		return nil, HoldMaxRunning, nil
	}
	held := make(map[string]bool, len(live))
	for _, l := range live {
		held[l.Resource] = true
	}
	var free []string
	for i := range maxRunning {
		if r := RemoteSlotClass.Resource(strconv.Itoa(i)); !held[r] {
			free = append(free, r)
		}
	}
	// A RANDOM ORDER, so two nodes taking slots at once rarely ask for the
	// same one, and one that loses asks for the next.
	rand.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] })
	owner := s.nodeID + "/" + uuid.NewString()
	for _, resource := range free {
		lease, _, err := s.leases.TryAcquire(ctx, resource, coord.AcquireOptions{Owner: owner, TTL: RemoteSlotTTL,
			Preferred: s.nodeID, Ungated: true})
		if err != nil {
			return nil, HoldUnknown, fmt.Errorf("take the remote check slot %s: %w", resource, err)
		}
		if lease == nil {
			continue
		}
		slot := &RemoteSlot{s: s, resource: resource, owner: owner, epoch: lease.Epoch, limit: minutesPerDay,
			taken: now, stop: make(chan struct{}), done: make(chan struct{})}
		// THE FIRST MINUTE, before the box: a day spent starts nothing.
		ok, err := s.charge(ctx, now, minutesPerDay)
		if err != nil || !ok {
			slot.give(ctx)
			if err != nil {
				return nil, HoldUnknown, fmt.Errorf("charge a remote check box's first minute: %w", err)
			}
			return nil, HoldBoxHours, nil
		}
		go slot.keep()
		return slot, "", nil
	}
	// EVERY SLOT A LISTING CALLED FREE WAS TAKEN BY ANOTHER RUNNER since.
	return nil, HoldMaxRunning, nil
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

// keep renews the slot and charges its box's minutes until its release, and
// charges the minutes begun since its last renewal as it stops: the box is
// closed by then, so they are the last of its time.
func (sl *RemoteSlot) keep() {
	defer close(sl.done)
	s := sl.s
	charged := 1
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
		if ok, err := s.leases.Renew(ctx, sl.resource, sl.owner, sl.epoch, RemoteSlotTTL); err != nil || !ok {
			// A SLOT LOST — lapsed through an outage, perhaps taken by
			// another runner since — leaves the box running: it is held
			// to its own timeout, and the cap is over by one until it
			// ends. Said; the next renewal asks again.
			s.log.Warn("code_check_remote_slot_not_renewed", "slot", sl.resource, "renewed", ok,
				"error", fmt.Sprint(err), "detail", "the box runs on to its own end; the company's "+
					"remote cap may be over by this box until it does")
		}
		charged = sl.chargeDue(ctx, charged, false)
		cancel()
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
// its lease released, under a context no caller's cancellation reaches. Safe
// to call more than once, and on nil.
func (sl *RemoteSlot) Release() {
	if sl == nil {
		return
	}
	sl.once.Do(func() {
		close(sl.stop)
		<-sl.done
		sl.give(context.Background())
	})
}

// give releases the slot's lease: at its own epoch, never another tenure's.
func (sl *RemoteSlot) give(ctx context.Context) {
	if _, err := sl.s.leases.Release(context.WithoutCancel(ctx), sl.resource, sl.owner, sl.epoch); err != nil {
		sl.s.log.Warn("code_check_remote_slot_not_released", "slot", sl.resource, "error", err.Error(),
			"detail", "it frees itself when its lease lapses")
	}
}
