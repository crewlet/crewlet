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
	// sender on a chat surface no seat or person maps to. A stranger uses no
	// app and opens no artifact, so an agent acting for one takes no app's
	// verb for them at all, and reaches only the edits to its own work that
	// no app gates (ADR-0109).
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
	// the identity directory composes them — the apps their grants name,
	// never admin — or a colleague seat as its own turn would. Ignored for
	// every other kind.
	Principal iam.Principal
}

// DecideWithin answers whether agent p may do a to o INSIDE A TURN somebody
// woke: [Decide] for p, then the same for the principal the turn acts for, the
// app first ([DecideWithinApps]), then the relation.
//
// THE CONFUSED DEPUTY, CLOSED: somebody who cannot open a private artifact can
// message the agent that can, so every verb in such a turn is decided AGAIN FOR
// THAT REQUESTER on the same object and admitted only when both are. Their
// authority is a ceiling, never a source: what is admitted is the agent's own
// act. The requester is decided by [Decide] as if they asked themselves, so the
// ceiling cannot drift. A refusal is [ReasonCeiling] naming no grant of the
// caller's; an undecidable requester is UNKNOWN.
func DecideWithin(ctx context.Context, p iam.Principal, c Ceiling, a Action, o Object,
	chart Chart, now time.Time) Decision {

	// THE CEILING IS THE TURN'S and passes to every turn it hands on: a
	// colleague the agent asks, the answer, and what the agent writes on the
	// engine's own surfaces or Gitlet wake their target in a turn worked for
	// the same requester (ADR-0109, internal/agent/builtin's workingForOf). A
	// vendor's surface carries none: its webhook wake has no engine field. It
	// bounds what a turn does for its requester, not what an agent may tell them.
	d := DecideWithinApps(ctx, p, c, a, o, chart, now)
	if d.Unknown() || !d.Allowed || c.Kind == CeilingNone {
		return d
	}
	// EXCEPT EDITS TO ITS OWN WORK: a steerable row ([rule.steerable]) on an
	// object the agent OWNS skips the requester's RELATION, since anybody who
	// can message an agent steers its work and the ceiling would refuse "fix
	// the typo in your table" from a viewer. Reads and sharing are never
	// steerable, so a private artifact never reaches a requester who could not
	// open it. The app was asked first: steering in an app is using it.
	if steerable, _ := SteerableOf(a); steerable && ownedBy(p, o) {
		return d
	}
	if c.Kind == CeilingStranger {
		return Decision{Reason: ReasonCeiling, Ceiling: CeilingStranger}
	}
	req := Decide(ctx, c.Principal, a, o, chart, now)
	switch {
	case req.Unknown():
		return Decision{Reason: ReasonCeiling, Ceiling: CeilingRequester, Err: req.Err}
	case !req.Allowed:
		return Decision{Reason: ReasonCeiling, Ceiling: CeilingRequester, RequesterReason: req.Reason,
			RequesterKind: c.Principal.Kind}
	}
	return d
}

// DecideWithinApps answers whether agent p may do a to o INSIDE A TURN
// somebody woke, as far as the APPS go: [Decide] for p, then whether the
// requester uses the app a's row names (ADR-0109).
//
// An agent holds every app (ADR-0033), so somebody kept off an app could have
// an agent use it for them: a verb whose row names an app is admitted only
// when the requester holds it (any one of a [ClassAnyApp] set; on a
// [ClassAppOrAsked] row, the requester an open ask names). THE ROW'S APP AND
// NOTHING ELSE: their other relations stay the agent's own, or an agent would
// be refused its own inbox. A stranger, a requester who may not act and an
// unfilled ceiling use no app: [ReasonCeiling], with [Decision.RequesterReason]
// and the apps that would admit them in [Decision.RequesterGrants].
func DecideWithinApps(ctx context.Context, p iam.Principal, c Ceiling, a Action, o Object,
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
	r := rules[a]
	apps := r.anyOf
	if r.app != "" {
		apps = []iam.Grant{r.app}
	}
	if len(apps) == 0 {
		// A ROW NAMING NO APP is not this half's to bound: whatever bounds
		// it for the requester is the relation [DecideWithin] decides.
		return d
	}
	if c.Kind == CeilingStranger {
		return Decision{Reason: ReasonCeiling, Ceiling: CeilingStranger}
	}
	q := c.Principal
	if !q.Stage.MayAct() {
		// THEIR STAGE REFUSES THEM, and no app would change that: a
		// requester who may not act is told as one, never sent to ask for
		// an app.
		return Decision{Reason: ReasonCeiling, Ceiling: c.Kind, RequesterReason: ReasonStage,
			RequesterKind: q.Kind}
	}
	refused := Decision{Reason: ReasonCeiling, Ceiling: c.Kind, RequesterGrants: slices.Clone(apps),
		RequesterReason: ReasonNoGrant, RequesterKind: q.Kind}
	for _, app := range apps {
		if q.Can(app) {
			return d
		}
	}
	if r.class == ClassAppOrAsked {
		// THE PERSON ASKED, as the row admits them asking themselves: by the
		// seat they hold or the login they act under.
		if o.Asked != "" && (q.Seat != "" && q.Seat == o.Asked || q.Login != "" && q.Login == o.Asked) {
			return d
		}
		if o.Unresolved {
			// NOBODY HAS READ THE ITEM YET, so nobody knows whether an open
			// ask on it names the requester: read it, resolve its asks, and
			// decide again on the row.
			refused.Err = ErrUnresolved
			return refused
		}
	}
	return refused
}

// NamesApp reports whether a's row names an app — the rows the app half of
// the requester ceiling bounds ([DecideWithinApps]). It is what a caller that
// composes a turn's requester only when it is needed asks first: a verb no
// app gates asks nothing of the requester's apps, so it costs no read of them.
func NamesApp(a Action) bool {
	r := rules[a]
	return r.app != "" || len(r.anyOf) > 0
}

// ownedBy reports whether p is o's owner, by the login it acts under or the
// seat it holds, as every relation class compares an owner.
func ownedBy(p iam.Principal, o Object) bool {
	return o.Owner != "" && (p.Login != "" && p.Login == o.Owner ||
		p.Seat != "" && p.Seat == o.Owner)
}
