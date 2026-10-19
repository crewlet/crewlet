package authz

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// CeilingKind is who an agent's turn is acting FOR, which bounds what it may
// do: nobody, a principal the company knows, or somebody it does not.
//
// A NAMED STRING WHOSE ZERO IS INVALID, and [DecideWithin] refuses the zero:
// a caller that forgot to say who woke the turn must not read as a turn
// nobody woke, which is the one kind that bounds nothing.
type CeilingKind string

const (
	// CeilingNone is a turn nobody woke: a schedule, a watch, the agent's
	// own initiative. Nothing bounds it beyond the agent's own authority.
	CeilingNone CeilingKind = "none"

	// CeilingRequester is a turn a principal the company knows woke: a
	// person's message, a colleague seat's ask. [Ceiling.Principal] is who,
	// composed as the request guard would compose them.
	CeilingRequester CeilingKind = "requester"

	// CeilingStranger is a turn somebody the company does not know woke: a
	// sender on a chat surface no seat or person maps to. A stranger opens
	// no artifact at all, so an agent acting for one reaches only the edits
	// it makes to its own work.
	CeilingStranger CeilingKind = "stranger"
)

// CeilingKinds are the three, in declaration order.
var CeilingKinds = []CeilingKind{CeilingNone, CeilingRequester, CeilingStranger}

// Valid reports whether k is a ceiling kind this build knows.
func (k CeilingKind) Valid() bool { return slices.Contains(CeilingKinds, k) }

// Ceiling is who an agent's turn is acting for: the second principal every
// verb it takes in that turn is decided for ([DecideWithin]).
type Ceiling struct {
	Kind CeilingKind

	// Principal is the requester, for [CeilingRequester] alone: a person as
	// the identity directory composes them, or a colleague seat as its own
	// turn would. Ignored for every other kind.
	Principal iam.Principal
}

// DecideWithin answers whether agent p may do a to o INSIDE A TURN somebody
// woke: [Decide] for p, and then the same question for the principal the turn
// is acting for.
//
// # The confused deputy, closed
//
// An agent reads what is shared with it. Somebody who cannot open a private
// artifact can still message the agent that can, and ask it to summarise the
// artifact or to tick its approval column. So every verb an agent takes in a
// turn somebody woke is decided AGAIN FOR THAT REQUESTER on the same object,
// and admitted only when both are: the agent never discloses an artifact to a
// requester who could not open it, and never writes on a requester's behalf
// what the requester could not write. The requester's authority is a ceiling
// and never a source: whatever is admitted is the agent's own act, recorded as
// the agent.
//
// # Except the edits an agent makes to its own work
//
// A verb whose row is steerable ([rule.steerable]) on an object the agent
// OWNS skips the requester: anybody who can message an agent steers its own
// work, as they steer every other piece of work an agent owns, and applying
// the ceiling there would refuse "please fix the typo in your table" from a
// colleague who only views it. Reads are never steerable, and neither is
// sharing, so an agent's private artifact still never reaches a requester who
// could not open it. A create names the agent as the object's owner: the
// artifact it makes is its own.
//
// ONE DECISION OF THE SAME TABLE, ASKED TWICE: the requester is decided by
// [Decide] exactly as they would be if they asked themselves, so the ceiling
// cannot drift from the rules it bounds. A refusal of the requester is
// [ReasonCeiling], naming no grant, because nothing the agent could hold
// would change it; a requester this node cannot decide is UNKNOWN.
func DecideWithin(ctx context.Context, p iam.Principal, c Ceiling, a Action, o Object,
	chart Chart, now time.Time) Decision {

	d := Decide(ctx, p, a, o, chart, now)
	if d.Unknown() || !d.Allowed {
		return d
	}
	switch c.Kind {
	case CeilingNone:
		return d
	case CeilingRequester, CeilingStranger:
	default:
		// A CEILING NOBODY FILLED IN is refused rather than read as a
		// turn nobody woke: the zero must be the closed end.
		return Decision{Reason: ReasonCeiling}
	}
	if steerable, _ := SteerableOf(a); steerable && ownedBy(p, o) {
		return d
	}
	if c.Kind == CeilingStranger {
		return Decision{Reason: ReasonCeiling}
	}
	req := Decide(ctx, c.Principal, a, o, chart, now)
	switch {
	case req.Unknown():
		return Decision{Reason: ReasonCeiling, Err: req.Err}
	case !req.Allowed:
		return Decision{Reason: ReasonCeiling}
	}
	return d
}

// ownedBy reports whether p is o's owner, by the login it acts under or the
// seat it holds, as every relation class compares an owner.
func ownedBy(p iam.Principal, o Object) bool {
	return o.Owner != "" && (p.Login != "" && p.Login == o.Owner ||
		p.Seat != "" && p.Seat == o.Owner)
}
