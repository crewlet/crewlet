package iam

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Stage is how far through enrolment a principal is.
//
// IT IS CALLED Stage AND NOT Posture. `FramePosture` is a different closed set
// in the websocket layer answering a different question — what a connection is
// doing — and two closed sets under one name is how a later change maps the
// wrong one, silently, with each side staying self-consistent.
//
// A seat and the engine are always [StageActive]: neither enrols, and neither
// can be suspended by anything but the org chart and the process. The other
// four are states a PERSON or a MACHINE passes through.
type Stage string

const (
	// StageInvited is a principal the company has created and who has
	// proved nothing yet. It may not act.
	StageInvited Stage = "invited"

	// StageEnrolling is a principal mid-proof — setting a credential,
	// completing a second factor. Distinct from invited because an
	// abandoned enrolment is a half-built credential somebody has to
	// clean up, and collapsing the two hides which ones those are.
	StageEnrolling Stage = "enrolling"

	// StageActive is enrolled and may act. The only stage that may.
	StageActive Stage = "active"

	// StageSuspended is enrolled and may NOT act, with the record kept.
	// Separate from retired because suspension is reversible and the
	// difference decides whether re-admitting somebody re-enrols them.
	StageSuspended Stage = "suspended"

	// StageRetired is a principal who has left. The record is kept rather
	// than deleted so every audit row they authored still resolves to a
	// name — a history whose authors evaporate is not an audit trail.
	StageRetired Stage = "retired"
)

// Stages are the five.
var Stages = []Stage{
	StageInvited, StageEnrolling, StageActive, StageSuspended, StageRetired,
}

// Valid reports whether a stage off the wire is one this build knows.
func (s Stage) Valid() bool { return slices.Contains(Stages, s) }

// MayAct reports whether a principal at this stage is allowed to do anything
// at all, before any grant is consulted.
//
// AN ALLOWLIST OF ONE, never a denylist. A stage this build does not know —
// a newer peer's row on a rolling upgrade — answers false, which is the safe
// direction: a denylist would have admitted it.
func (s Stage) MayAct() bool { return s == StageActive }

// ErrInvalidPrincipal is what every [Principal.Validate] failure wraps, so a
// caller can tell a malformed principal from the store failure that produced
// it without matching on message text.
var ErrInvalidPrincipal = errors.New("iam: invalid principal")

// Principal is who is acting.
//
// One struct for a person, a seat, a machine and the engine, because every one
// of them authors rows in the same tables and the audit trail's whole value is
// that it does not have four shapes. Which one it is, is [Principal.Kind].
type Principal struct {
	// ID is the principal's stable identity, and what every row keys on.
	// A uuid rather than the login, because a person changes their login
	// and an audit row must not change with it.
	ID uuid.UUID

	// Login is what this principal is NAMED and authenticates as: a
	// person's dotted login (jane.doe) or a machine's coloned handle
	// (ci:release). One field for both, which is exactly why the two
	// grammars are disjoint — see the package doc. A seat's name is
	// Seat, and the engine's is the node's own id.
	Login string

	// Kind is what sort of thing this is.
	Kind Kind

	// Seat is the seat handle this principal acts as, or "" for one that
	// acts as itself. For [KindSeat] it is the seat. For [KindPerson] it
	// is the binding the IDENTITY DIRECTORY holds for them, which is what
	// lets somebody at the dashboard act AS THEMSELVES rather than as a
	// credential — and unbound is an ordinary state, not a
	// misconfiguration.
	//
	// Carried as an opaque string: internal/org owns the seat-handle
	// grammar, and a second copy of it in a leaf package is a copy that
	// drifts the first time org widens it.
	Seat string

	// SeatAt is the CHART POSITION the binding was decided at: the
	// position the org chart's own log had reached when somebody wrote
	// this person's seat down.
	//
	// A POSITION AND NOT A CLOCK, and the difference is the whole point of
	// the field. What reads it is a three-valued lookup: a seat missing
	// from this node's chart view means either that the seat is GONE or
	// that this node has NOT YET APPLIED the hire, and those answers are
	// 403 and 503. Comparing this node's own chart position against this
	// value is what tells them apart — and comparing two nodes' wall
	// clocks is what the coordination layer states it never does.
	//
	// It is comparable across a reanchor because a packed position carries
	// the generation in its high bits. Zero means no binding has ever been
	// decided, which is the only honest reading when Seat is "".
	SeatAt uint64

	// Grants are the capabilities this principal carries. A slice rather
	// than a set, because it is a row's own list and the order it was
	// written in is what an operator sees when they read it back.
	//
	// IT MAY HOLD STRINGS THIS BUILD DOES NOT KNOW. See [Principal.Can].
	Grants []Grant

	// Colleague is how far into the company's own WORK this principal
	// reaches — the other hat, orthogonal to Grants. [Colleague] argues
	// why one ladder could not have carried both.
	//
	// Its zero is [ColleagueNone], which is the closed end and a real
	// setting: most credentials are nobody's colleague. That is the one
	// place this struct's zero value is meaningful rather than refused,
	// and it is meaningful only because the zero is the closed end.
	Colleague Colleague

	// Position is where this principal sits in the chart: the unit path
	// its seat belongs to ("engineering/backend"), or "" for one that
	// holds no seat. Carried beside the handle rather than looked up,
	// because a gate that re-walked the org chart per request would read
	// a different chart from the turn it is guarding.
	Position string

	// ReauthAt is the instant after which this principal's proof of
	// identity is too old for an ordinary step-up gesture
	// ([RecencyStepUp]), in UTC.
	//
	// ZERO IS NOT "NEVER" — see [Principal.Fresh]. A zero deadline read
	// as "never expires" is a session somebody forgot to bound, which is
	// the same failure the zero [Grant] would be.
	ReauthAt time.Time

	// SensitiveReauthAt is the same instant for a SENSITIVE gesture
	// ([RecencySensitive]): the proof plus `step_up_sensitive` rather than
	// plus `step_up`, and so never later than ReauthAt on a principal the
	// guard composed.
	//
	// TWO DEADLINES AND NOT ONE, because the two windows are two settings
	// and a gesture asks for one of them: a single deadline could say
	// whether a proof is inside the hour and could not say whether it is
	// inside the quarter-hour. Carried as instants rather than as the proof
	// and the two windows, because the windows are THIS NODE's settings and
	// the node that composed the principal is the one whose windows apply
	// — exactly as its ceiling is the one cut into the grants.
	//
	// ZERO IS STALE here too, for ReauthAt's reason.
	SensitiveReauthAt time.Time

	// Stage is how far through enrolment this principal is.
	Stage Stage

	// Via is the credential this principal is acting THROUGH, named as the
	// audit trail records it, when that credential is not the principal
	// itself: `pat:<id>` for a machine token acting as its owner
	// ([MachineTokenName]), and `session:<lineage>` for a browser session
	// ([SessionName]) — whoever it stands for, a Tier A token's exchanged
	// session included.
	//
	// EMPTY FOR A PRINCIPAL THAT IS ITS OWN CREDENTIAL — a Tier A bearer's
	// login already names the token, a seat is its own turn, the engine is
	// its node — and that is the meaningful zero: [ActorFor] then records
	// the login as the operator.
	//
	// WHY IT EXISTS: a machine token is composed as its OWNER, so without
	// this every write one made was recorded exactly as the owner's own, and
	// a token minted on somebody's account could file, edit and close their
	// work with no row saying a token was used, let alone which. The author
	// stays the owner — it is their authority being exercised — and this is
	// the column beside it that says through what. A session is the same
	// question asked of a browser: the person is the author, and the lineage
	// is which sign-in — which of their browsers, which shared machine — the
	// write came through.
	Via string
}

// Can reports whether this principal carries g.
//
// AN UNKNOWN GRANT IS IGNORED FOR ITS CAPABILITY, NEVER A REASON TO REJECT THE
// PRINCIPAL. A rolling upgrade puts a newer build's grant strings in front of
// an older node — that is what additive evolution means on a shared store —
// and refusing the whole principal over one string it cannot parse would log
// every person in the company out for the length of the deploy. So the string
// simply grants nothing here: this build cannot open a door it has never heard
// of, and it does not need to in order to open the doors it has.
//
// The asked-for grant is checked too, so a caller that composes one from a
// string cannot accidentally match an unknown one a principal happens to hold.
func (p Principal) Can(g Grant) bool {
	if !g.Valid() {
		return false
	}
	return slices.Contains(p.Grants, g)
}

// KnownGrants are the carried grants this build can act on.
func (p Principal) KnownGrants() []Grant {
	known := make([]Grant, 0, len(p.Grants))
	for _, g := range p.Grants {
		if g.Valid() {
			known = append(known, g)
		}
	}
	return known
}

// UnknownGrants are the carried grants this build cannot act on.
//
// Returned rather than dropped silently so a log line can NAME them: "this
// node ignored two capabilities" is an upgrade in progress, and "this node
// ignored two capabilities" repeated for a week is a node nobody upgraded.
func (p Principal) UnknownGrants() []Grant {
	unknown := make([]Grant, 0)
	for _, g := range p.Grants {
		if !g.Valid() {
			unknown = append(unknown, g)
		}
	}
	return unknown
}

// Fresh reports whether this principal's proof of identity still holds at now.
//
// A ZERO ReauthAt IS STALE, not eternal. The two readings are one keystroke
// apart and only one of them fails safe: an unset deadline is a principal
// nobody bounded, and reading that as "never expires" turns a field somebody
// forgot to fill in into a session that outlives the company.
func (p Principal) Fresh(now time.Time) bool {
	return !p.ReauthAt.IsZero() && now.Before(p.ReauthAt)
}

// Proved reports whether this principal's proof of identity is recent enough
// for a gesture that asks for r, at now.
//
// A RECENCY THIS BUILD CANNOT NAME IS NOT PROVED, which is the direction that
// fails safe: a rule written by a newer peer and read here must not open a
// door because this build did not know how recent the proof had to be. And
// [RecencyAny] is proved by anybody, which is what signing in was for.
func (p Principal) Proved(r Recency, now time.Time) bool {
	switch r {
	case RecencyAny:
		return true
	case RecencyStepUp:
		return p.Fresh(now)
	case RecencySensitive:
		return !p.SensitiveReauthAt.IsZero() && now.Before(p.SensitiveReauthAt)
	}
	return false
}

// Validate reports what is wrong with this principal, naming the field.
//
// IT DOES NOT LOOK AT [Principal.Grants], and that omission is the rule rather
// than an oversight — see [Principal.Can]. Everything else here is a fact this
// build minted or has to be able to key on, and a principal that fails any of
// it is one no row can address.
func (p Principal) Validate() error {
	if p.ID == uuid.Nil {
		return fmt.Errorf("%w: id is the nil uuid — a principal no row can key on", ErrInvalidPrincipal)
	}
	if !p.Kind.Valid() {
		return fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidPrincipal, p.Kind, Kinds)
	}
	if !p.Stage.Valid() {
		return fmt.Errorf("%w: stage %q is not one of %v", ErrInvalidPrincipal, p.Stage, Stages)
	}
	switch p.Kind {
	case KindSeat:
		// A seat principal IS its handle. Without one there is nothing
		// to route to, nothing to attribute and nothing to scope.
		if p.Seat == "" {
			return fmt.Errorf("%w: a %s principal must name its seat in `seat`",
				ErrInvalidPrincipal, p.Kind)
		}
	case KindPerson:
		if !ValidLoginFor(p.Kind, p.Login) {
			return fmt.Errorf(
				"%w: login %q is not a person login — write it as dotted segments (jane.doe), which is what keeps it out of the seat-handle namespace",
				ErrInvalidPrincipal, p.Login)
		}
	case KindMachine:
		if !ValidLoginFor(p.Kind, p.Login) {
			return fmt.Errorf(
				"%w: login %q is not a machine handle — write it as class:name (ci:release), which is what keeps it out of the seat-handle namespace",
				ErrInvalidPrincipal, p.Login)
		}
	case KindEngine:
		// The engine names itself with the NODE's own id, which is
		// minted rather than typed, so there is no grammar to hold it
		// to — only the requirement that it say which node. The
		// `system` author kind is what tells it from a seat.
		if p.Login == "" {
			return fmt.Errorf("%w: an %s principal must carry its node id in `login`",
				ErrInvalidPrincipal, p.Kind)
		}
	}
	// A CREDENTIAL IT ACTS THROUGH IS ONE THIS BUILD NAMES. Via is written
	// into every row the principal authors, so a value in no namespace would
	// be an operator column no reader can resolve back to anything.
	if p.Via != "" && !ValidCredentialName(p.Via) {
		return fmt.Errorf("%w: via %q names no credential — a machine token "+
			"is %s followed by its id, and a browser session %s followed by "+
			"its lineage", ErrInvalidPrincipal, p.Via, MachineTokenPrefix,
			SessionPrefix)
	}
	return nil
}
