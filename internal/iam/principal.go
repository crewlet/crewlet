package iam

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/words"
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
	// StageInvited is a principal who has proved nothing yet, and may not
	// act. NOTHING IN THIS BUILD CREATES ONE HERE: an administrator's
	// create lands a person ACTIVE, handed a first password link, and an
	// invitation creates nobody until its redemption, which lands them
	// active too. It remains a stage an administrator may set.
	StageInvited Stage = "invited"

	// StageEnrolling is a principal part-way through proving who they
	// are, who may not act yet. Distinct from invited because an
	// abandoned enrolment is a half-built credential somebody has to
	// clean up, and collapsing the two hides which ones those are.
	//
	// NOT THE SECOND-FACTOR ENROLMENT a required factor asks for: nothing
	// in this build moves a person here on its own — an administrator
	// may set it — and a person who must enrol a factor before anything
	// else is ACTIVE, holding a SESSION that may only enrol one (the
	// identity estate's Session.EnrolmentOnly, carried on the session's
	// bearer too). The restriction is what a sign-in proved, which is a
	// fact about the session; a stage is a fact about the person.
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
	// credential. Every person holds a human seat (ADR-0032), so a person
	// with none is one recorded before that rule, which `iam check`
	// reports; a MACHINE acting as itself is the ordinary unbound case,
	// and it keeps [KindMachine] — a bound one is composed as the person
	// it acts as.
	//
	// Carried as the string the binding holds and NOT re-validated here:
	// the grammar is [ValidSeatHandle], asked where a handle is created,
	// and a principal only ever carries one the chart already holds.
	Seat string

	// Grants are the permissions this principal carries. A slice rather
	// than a set, because it is a row's own list and the order it was
	// written in is what an operator sees when they read it back.
	//
	// IT MAY HOLD NAMES THIS BUILD DOES NOT KNOW, which grant nothing — see
	// [Principal.Can] — and [GrantAdmin] in it implies every app in [Apps]
	// through [Expand], whether or not the app is listed beside it. A
	// principal the request guard composed carries its node's EFFECTIVE set
	// ([AdminPosture.Effective]): known names only, apps spelled out.
	Grants []Grant

	// Position is where this principal sits in the chart: the unit path
	// its seat belongs to ("engineering/backend"), or "" for one that
	// holds no seat. Carried beside the handle rather than looked up,
	// because a gate that re-walked the org chart per request would read
	// a different chart from the turn it is guarding.
	Position string

	// ReauthAt is the instant after which this principal's proof of
	// identity is too old for a step-up gesture ([RecencyStepUp]), in UTC:
	// the proof plus `api.auth.session.step_up`.
	//
	// AN INSTANT RATHER THAN THE PROOF AND THE WINDOW, because the window
	// is THIS NODE's setting and the node that composed the principal is
	// the one whose window applies — exactly as the node's admin posture
	// (`api.auth.admin`) is the one cut into the grants.
	//
	// ZERO IS NOT "NEVER" — see [Principal.Fresh]. A zero deadline read
	// as "never expires" is a session somebody forgot to bound, which is
	// the same failure the zero [Grant] would be.
	ReauthAt time.Time

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
	//
	// `assist:<grant>` ([AssistName]) is the Dashboard Assistant acting for
	// a person signed in through a session: the person is the author, and
	// the grant is the credential the request came through (ADR-0050).
	Via string

	// AssistScope is how far the assistant grant named by Via reaches:
	// [AssistScopeRead] for a turn's reads, [AssistScopeExec] for the one
	// approved request an exec grant is bound to.
	//
	// SET EXACTLY WHEN Via IS AN ASSISTANT GRANT, and the zero otherwise,
	// which [Principal.Validate] holds in both directions: a scope with no
	// grant would narrow nothing anybody can name, and a grant with no
	// scope would reach as far as nobody decided.
	AssistScope AssistScope
}

// Can reports whether this principal carries g, counting every app an
// administrator carries through [Expand].
//
// AN UNKNOWN NAME IS IGNORED FOR ITS CAPABILITY, NEVER A REASON TO REJECT THE
// PRINCIPAL. A rolling upgrade puts a newer build's names in front of an older
// node — that is what additive evolution means on a shared store — and
// refusing the whole principal over one string it cannot parse would log every
// person in the company out for the length of the deploy. So the string simply
// grants nothing here: this build cannot open a door it has never heard of,
// and it does not need to in order to open the doors it has.
//
// The asked-for permission is checked too, so a caller that composes one from
// a string cannot accidentally match an unknown one a principal happens to
// hold.
func (p Principal) Can(g Grant) bool {
	return g.Valid() && slices.Contains(Expand(p.Grants), g)
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
	}
	return false
}

// MachineToken is the id of the machine token this principal acts through, and
// whether it acts through one at all.
//
// THE ONE FACT ON A PRINCIPAL THAT SAYS NOBODY IS PRESENT. A machine token is
// composed as its OWNER — a person, at their stage, carrying their seat — and
// stepped up by construction, so neither the kind nor the clock can tell a
// request it made from one its owner made at a keyboard. What it came through
// can, and it is read here, once, off [Principal.Via]: internal/authz refuses
// it every gesture whose row needs a person present, and the routes that must
// know which token it was read the id from the same place.
func (p Principal) MachineToken() (string, bool) {
	id, ok := strings.CutPrefix(p.Via, MachineTokenPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
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
		return fmt.Errorf("%w: kind %q is not one of %s", ErrInvalidPrincipal, p.Kind, words.Or(Kinds))
	}
	if !p.Stage.Valid() {
		return fmt.Errorf("%w: stage %q is not one of %s", ErrInvalidPrincipal, p.Stage, words.Or(Stages))
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
		return fmt.Errorf("%w: via %q names no credential: a machine token "+
			"is %s followed by its id, a browser session %s followed by "+
			"its lineage, and an assistant grant %s followed by its id",
			ErrInvalidPrincipal, p.Via, MachineTokenPrefix, SessionPrefix,
			AssistPrefix)
	}
	// AN ASSISTANT GRANT IS A PERSON'S, AND IT SAYS HOW FAR IT REACHES. The
	// grant is derived only from a person's own session (ADR-0050), so any
	// other kind acting through one is a principal nobody composed, and the
	// scope travels with the grant and nowhere else.
	_, assisted := p.Assisted()
	switch {
	case assisted && p.Kind != KindPerson:
		return fmt.Errorf("%w: a %s acts through the assistant grant %q, which "+
			"only a person's own session derives", ErrInvalidPrincipal, p.Kind, p.Via)
	case assisted && !p.AssistScope.Valid():
		return fmt.Errorf("%w: the assistant grant %q carries scope %q, which is "+
			"not one of %s", ErrInvalidPrincipal, p.Via, p.AssistScope, words.Or(AssistScopes))
	case !assisted && p.AssistScope != "":
		return fmt.Errorf("%w: assist scope %q set on a principal acting through "+
			"no assistant grant", ErrInvalidPrincipal, p.AssistScope)
	}
	return nil
}
