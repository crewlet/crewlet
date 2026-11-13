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
// woke: [Decide] for p, and then the same question for the principal the turn
// is acting for — the app first ([DecideWithinApps]), then the relation.
//
// # The confused deputy, closed
//
// An agent reads what is shared with it. Somebody who cannot open a private
// artifact can still message the agent that can, and ask it to summarise the
// artifact or to tick its approval column. So every verb an agent takes in a
// turn somebody woke is decided AGAIN FOR THAT REQUESTER on the same object,
// and admitted only when both are: in that turn the agent discloses no
// artifact to a requester who could not open it, and writes nothing on a
// requester's behalf that the requester could not write. The requester's
// authority is a ceiling and never a source: whatever is admitted is the
// agent's own act, recorded as the agent.
//
// THE CEILING IS THE TURN'S, and every turn it hands on to: it is composed
// from what woke that turn, and what the turn hands on carries it. A colleague
// the agent asks answers in a turn worked for whoever the asking turn worked
// for, and the answer wakes the agent in a turn worked for them too; and what
// the agent writes on the engine's own surfaces (a work item, a page or a
// comment on one, a Chatlet message, a change, a review, a comment or a push
// on Gitlet) wakes its colleague in a turn worked for them as well (ADR-0109,
// internal/agent/builtin's workingForOf), and so does what the engine records
// on Gitlet about the agent's earlier push. The edges are what carries no
// requester: a vendor's surface, whose wake comes back by webhook with no
// field of the engine's, and, until crewlet#798's piece 4c-ii, what Gitlet's
// merge queue records about an earlier enqueue. Nor does it reach what the
// agent already knows. It bounds what a turn does for its requester, not what
// an agent may go on to tell them.
//
// # Except the edits an agent makes to its own work
//
// A verb whose row is steerable ([rule.steerable]) on an object the agent
// OWNS skips the requester's RELATION: anybody who can message an agent
// steers its own work, as they steer every other piece of work an agent owns,
// and applying the ceiling there would refuse "please fix the typo in your
// table" from a colleague who only views it. Reads are never steerable, and
// neither is sharing, so an agent's private artifact still never reaches a
// requester who could not open it. A create names the agent as the object's
// owner: the artifact it makes is its own.
//
// THE APP IS ASKED BEFORE THE SPARE: steering an agent's own work in an app
// is using the app, so a requester who does not use it steers nothing there
// (ADR-0109). The spare decides relations among the app's users, never
// whether somebody uses the app at all.
//
// ONE DECISION OF THE SAME TABLE, ASKED TWICE: the requester is decided by
// [Decide] exactly as they would be if they asked themselves, so the ceiling
// cannot drift from the rules it bounds. A refusal of the requester is
// [ReasonCeiling], naming no grant of the caller's, because nothing the agent
// could hold would change it; a requester this node cannot decide is UNKNOWN.
func DecideWithin(ctx context.Context, p iam.Principal, c Ceiling, a Action, o Object,
	chart Chart, now time.Time) Decision {

	d := DecideWithinApps(ctx, p, c, a, o, chart, now)
	if d.Unknown() || !d.Allowed || c.Kind == CeilingNone {
		return d
	}
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
// somebody woke, as far as the APPS go: [Decide] for p, and then whether the
// principal the turn is acting for uses the app a's row names (ADR-0109).
//
// # Every app, for every family
//
// An agent holds every app (ADR-0033). Somebody the company kept off Worklet,
// Doclet, Chatlet or authoring artifacts could still message an agent and
// have it use the app for them — so inside a turn somebody woke, every verb
// whose row names an app is admitted only when the requester holds that app:
// any one of a [ClassAnyApp] row's set, and on a row that admits without the
// app ([ClassAppOrAsked]) the requester an open ask names, on the terms the
// row admits them asking for themselves. It is what the one authorizer every
// builtin passes decides for a seat's call; [DecideWithin] asks it first and
// then decides the requester's relation too, for the families whose objects
// carry one.
//
// THE ROW'S APP AND NOTHING ELSE OF IT. The requester's other relations to
// the object — their own record, their lead, a comment's authorship, the
// container — stay the agent's own: decided for the requester, they would
// refuse an agent its own inbox and its own priorities in every turn a person
// woke, since those rows are decided on the agent's record, which the
// requester is not.
//
// A stranger uses no app. A requester who may not act uses none. A turn
// nobody woke is bounded by nothing beyond p's own authority, and a ceiling
// nobody filled in is refused rather than read as one. A refusal is
// [ReasonCeiling], with why the requester is refused in
// [Decision.RequesterReason] and, where an app would admit them, the apps in
// [Decision.RequesterGrants].
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
