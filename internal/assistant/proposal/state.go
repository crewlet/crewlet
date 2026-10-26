package proposal

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// DefaultTTL is how long a proposal waits on its person: fifteen minutes, long
// enough to read a change and check it against the screen it is about, short
// enough that the object it previewed has rarely moved, and the bound
// `assistant.proposal_ttl_seconds` overrides (60 to 3600 seconds).
const DefaultTTL = 15 * time.Minute

// UnknownAfter is how long an approved or executing proposal may go without a
// settlement before it reads as unknown: two minutes, twice the exec grant's
// sixty-second lifetime, so a write still in flight on its grant is never
// called unknown while it can still land, and a node that died mid-write is
// offered a Retry within a minute of the grant that carried it ending.
const UnknownAfter = 2 * time.Minute

// Event is what moves a proposal.
type Event string

const (
	// EventApprove is the person's approval, arbitrated.
	EventApprove Event = "approve"
	// EventDecline is the person's decline.
	EventDecline Event = "decline"
	// EventStop is the person stopping the turn the proposal parked.
	EventStop Event = "stop"
	// EventExpire is the expiry passing with nobody deciding.
	EventExpire Event = "expire"
	// EventStale is an approval finding the object moved since its preview.
	EventStale Event = "stale"
	// EventExecute is the server sending an approved write.
	EventExecute Event = "execute"
	// EventClaim is the page claiming an approved page action.
	EventClaim Event = "claim"
	// EventApplied is the write landing (the act route answered applied or
	// pending), or the page reporting it applied the action.
	EventApplied Event = "applied"
	// EventRefused is the write refused, or the page reporting it could not
	// apply the action.
	EventRefused Event = "refused"
	// EventUnknown is a write whose outcome nobody can confirm.
	EventUnknown Event = "unknown"
	// EventRetry is the person sending an unknown write again, under the
	// same operation key.
	EventRetry Event = "retry"
	// EventPageStale is the page reporting its revision moved.
	EventPageStale Event = "page_stale"
	// EventWithdrawn is an approval finding that the person no longer holds
	// an app the proposing turn read through: nothing is sent, and the turn
	// does not carry on.
	EventWithdrawn Event = "withdrawn"
)

// Events are every event, in declaration order.
var Events = []Event{
	EventApprove, EventDecline, EventStop, EventExpire, EventStale, EventExecute,
	EventClaim, EventApplied, EventRefused, EventUnknown, EventRetry, EventPageStale,
	EventWithdrawn,
}

// Valid reports whether e is an event this build knows.
func (e Event) Valid() bool { return slices.Contains(Events, e) }

// move is one legal transition.
type move struct {
	from  wire.ProposalState
	event Event
}

// moves is THE STATE MACHINE (ADR-0051): every legal transition, and nothing
// else. A settled state has no row, so nothing leaves it; unknown is not
// settled, and its one way out is a Retry.
var moves = map[move]wire.ProposalState{
	{wire.ProposalProposed, EventApprove}: wire.ProposalApproved,
	{wire.ProposalProposed, EventDecline}: wire.ProposalDeclined,
	{wire.ProposalProposed, EventStop}:    wire.ProposalDeclined,
	{wire.ProposalProposed, EventExpire}:  wire.ProposalExpired,
	{wire.ProposalProposed, EventStale}:   wire.ProposalStale,
	// An approval that found the person's access to what the turn read
	// withdrawn: refused before anything is sent.
	{wire.ProposalProposed, EventWithdrawn}: wire.ProposalFailed,

	{wire.ProposalApproved, EventExecute}: wire.ProposalExecuting,
	{wire.ProposalApproved, EventClaim}:   wire.ProposalApplying,
	// A page claiming an action from a screen whose revision moved since
	// the action was prepared: it is never applied.
	{wire.ProposalApproved, EventPageStale}: wire.ProposalStale,
	// An approval whose node died before it sent the write: nothing can say
	// whether it was sent, so it is unknown and offered a Retry.
	{wire.ProposalApproved, EventUnknown}: wire.ProposalUnknown,

	{wire.ProposalExecuting, EventApplied}: wire.ProposalApplied,
	{wire.ProposalExecuting, EventRefused}: wire.ProposalFailed,
	{wire.ProposalExecuting, EventUnknown}: wire.ProposalUnknown,

	{wire.ProposalUnknown, EventRetry}: wire.ProposalExecuting,

	{wire.ProposalApplying, EventApplied}:   wire.ProposalApplied,
	{wire.ProposalApplying, EventPageStale}: wire.ProposalStale,
	{wire.ProposalApplying, EventRefused}:   wire.ProposalFailed,
}

// ErrIllegalMove reports a transition the state machine does not allow.
var ErrIllegalMove = errors.New("proposal: the state machine allows no such move")

// Next is the state event moves a proposal in from to, or [ErrIllegalMove].
func Next(from wire.ProposalState, e Event) (wire.ProposalState, error) {
	to, ok := moves[move{from, e}]
	if !ok {
		return "", fmt.Errorf("%w: %s from %s", ErrIllegalMove, e, from)
	}
	return to, nil
}

// Allowed reports whether some event moves a proposal from from to to: the
// question a recorded move answers, which [Next] answers by event.
func Allowed(from, to wire.ProposalState) bool {
	for m, target := range moves {
		if m.from == from && target == to {
			return true
		}
	}
	return false
}

// Expired reports whether a proposal expiring at expires has expired at now.
func Expired(expires, now time.Time) bool { return !now.Before(expires) }

// StateNow is the state a stored proposal is in at now, with the two states
// nobody records the instant they become true decided here: a proposed
// proposal past its expiry is expired, and an approved or executing SERVER
// write with no settlement [UnknownAfter] after its last move is unknown. A
// page action is never unknown: nothing durable was sent, so an approved one
// nobody claimed is still the page's to apply, and its turn ends with its own
// expiry.
//
// THE ONE READING every surface renders a proposal card by, so a reload, a
// stream attach and an approval all agree on what a card offers.
func StateNow(p assistantdomain.Proposal, now time.Time) wire.ProposalState {
	switch p.State {
	case wire.ProposalProposed:
		if Expired(p.ExpiresAt, now) {
			return wire.ProposalExpired
		}
	case wire.ProposalApproved, wire.ProposalExecuting:
		if p.Scope != wire.ScopeServer {
			break
		}
		since := p.DecidedAt
		if since.IsZero() {
			since = p.CreatedAt
		}
		if !now.Before(since.Add(UnknownAfter)) {
			return wire.ProposalUnknown
		}
	}
	return p.State
}

// Lapsed reports whether a parked turn waits on nothing any more: none of its
// proposals (ps, the turn's own) can still move it. Each one is settled at
// now ([StateNow]), or is a page action approved or applying past its expiry,
// which the page never reported. A server write in doubt (unknown) is NOT
// lapsed: the person's Retry still settles it, and the turn waits for that.
//
// THE ONE RULE every place that ends a parked turn as expired reads: a Start
// in its conversation, a reload, an expired decision and the reaper's
// backstop.
func Lapsed(ps []assistantdomain.Proposal, now time.Time) bool {
	for _, p := range ps {
		switch state := StateNow(p, now); {
		case state.Settled():
		case p.Scope == wire.ScopePage && Expired(p.ExpiresAt, now) &&
			(state == wire.ProposalApproved || state == wire.ProposalApplying):
		default:
			return false
		}
	}
	return true
}
