package pickup

// Conditions is what the node holding a seat knows about it at one pass, read
// from the sources its delivery screening reads, so an idle seat is one a
// delivery would reach and run.
type Conditions struct {
	// Owned is whether this node holds the seat's lease freshly enough to
	// start work on it.
	Owned bool

	// Paused is a person's pause, and PauseUnknown a node that has not read
	// the fleet's pauses yet, which is not "not paused".
	Paused, PauseUnknown bool

	// TurnEngineReady is false while the company configures no model.
	TurnEngineReady bool

	// Dispatching is a delivery of the seat being dispatched on this node:
	// a turn running, or about to.
	Dispatching bool

	// ExecutorHolds is a detached executor run holding the seat, and
	// ExecutorAwaits one of its runs parked on a person's answer.
	ExecutorHolds, ExecutorAwaits bool

	// Parked is a budget or provider park holding the seat's inbox, and
	// Refused a budget window or a provider limit that would refuse the
	// turn a pickup starts.
	Parked, Refused bool

	// MailWaiting is a delivery waiting in the seat's inbox, which runs a
	// turn of its own first. A display, not a ledger: read only to decide
	// whether to derive a pickup now, never whether to deliver one.
	MailWaiting bool

	// Shedding is a node whose config posture refuses new work, and
	// Draining one that is stopping.
	Shedding, Draining bool

	// IdleBefore is whether the previous pass found the seat idle too.
	IdleBefore bool
}

// Skip is why a pass derived no pickup for a seat: the `reason` of the
// `work_pickup_skipped` line and of the pickup counter.
type Skip string

// The reasons a seat is not handed work, in the order [Idle] asks them.
const (
	SkipNotOwned    Skip = "not_owned"
	SkipDraining    Skip = "draining"
	SkipShedding    Skip = "shedding"
	SkipPaused      Skip = "paused"
	SkipNoModel     Skip = "no_model"
	SkipBusy        Skip = "busy"
	SkipExecutor    Skip = "executor"
	SkipParked      Skip = "parked"
	SkipRefused     Skip = "refused"
	SkipMailWaiting Skip = "mail_waiting"
	SkipSettling    Skip = "settling"
	SkipNothing     Skip = "nothing_ready"
	SkipOffered     Skip = "all_offered"
	SkipCooldown    Skip = "cooldown"
	SkipClaimed     Skip = "claimed"
	SkipUnknown     Skip = "unknown"
)

// Idle reports whether a seat may be handed work now, and why not: whose it
// is first, then a person's decision, what runs, what waits, and the settle
// last, the one condition that is only a matter of time.
func Idle(c Conditions) (bool, Skip) {
	switch {
	case !c.Owned:
		return false, SkipNotOwned
	case c.Draining:
		return false, SkipDraining
	case c.Shedding:
		return false, SkipShedding
	case c.Paused || c.PauseUnknown:
		return false, SkipPaused
	case !c.TurnEngineReady:
		return false, SkipNoModel
	case c.Dispatching:
		return false, SkipBusy
	case c.ExecutorHolds || c.ExecutorAwaits:
		return false, SkipExecutor
	case c.Parked:
		return false, SkipParked
	case c.Refused:
		return false, SkipRefused
	case c.MailWaiting:
		return false, SkipMailWaiting
	case !c.IdleBefore:
		return false, SkipSettling
	}
	return true, ""
}

// IdleNow is [Idle] without the settle: whether this pass finds the seat idle,
// which the next pass reads as [Conditions.IdleBefore].
func IdleNow(c Conditions) bool {
	c.IdleBefore = true
	ok, _ := Idle(c)
	return ok
}
