package session

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Directory is what this node's own replicated copy of the identity estate
// says about one bearer.
//
// ONE METHOD AND ONE READ, because the six facts below have to be read in ONE
// snapshot: a revocation landing between a session read and a person read
// would produce a verdict that never existed at any instant. It is also the
// only shape in which "the replicated estate is not open" is one answer rather
// than three.
//
// IT IS DEFINED HERE, by the caller, and kept to what validation needs — no
// listing, no write, no enumeration. The identity estate exports a concrete
// reader and this is the two facts of it this package uses.
type Directory interface {
	// Resolve answers everything one bearer is checked against.
	//
	// AN ERROR IS THE UNKNOWN ARM, always: a replicated estate that is not
	// open, a deferred bucket this node cannot see past, a store that
	// cannot be read. Every one of them is 503, and none of them may be
	// reported as "this session does not exist".
	Resolve(ctx context.Context, lineage, person string) (Identity, error)
}

// Identity is one read's answer.
type Identity struct {
	// Applied is how far this node's iam applier has committed, packed.
	// It is what a bearer's start position is compared against.
	Applied uint64

	// Lag is how far behind the log this node is, as a duration. The
	// threshold it is compared against is [statelog.StallGrace] and is
	// NOT this package's to choose — an alarm fires at the threshold
	// another decision already made, and a second opinion about one event
	// is two numbers that drift.
	Lag time.Duration

	// Deferred reports that this node holds a record it could not decode
	// whose declared scope covers the person's bucket.
	//
	// SEPARATE FROM AN ERROR, because it is not one: the read SUCCEEDED
	// and the rows it returned are simply not known to be complete, which
	// is the framework's own coverage answer rather than a failure. It is
	// also separate from [Identity.Lag], because a node can be perfectly
	// caught up and still be holding one record from a newer peer — the
	// lag is zero and the answer is still unknown.
	Deferred bool

	// Session and Person are the rows, each three-valued in its own right
	// through its Found field.
	Session SessionRow
	Person  PersonRow

	// Generation is the fleet-wide session generation this node holds.
	Generation uint64
}

// SessionRow is the session's own row.
type SessionRow struct {
	Found bool

	// Ended reports a session that stopped — signed out, revoked, expired
	// or ended by reuse detection. The row is KEPT until the sweep
	// collects it, because "this session was ended by reuse detection" is
	// the sentence an investigation is looking for.
	Ended bool

	// Epoch is the revocation epoch the session was opened at.
	Epoch uint64

	// ProvedAt is when the holder proved who they are to open this
	// session, or zero for one opened on no proof of a person. It is what
	// a step-up surface asks to be recent, and it is the SESSION's fact
	// rather than the person's: proof on one device says nothing about
	// another, and a step-up re-proves by opening a new session.
	ProvedAt time.Time
}

// PersonRow is the holder's row.
type PersonRow struct {
	Found bool

	// Epoch is the person's CURRENT revocation epoch. A bearer carrying
	// anything below it is over.
	Epoch uint64

	Stage iam.Stage

	// Login, Colleague and Grants are what a principal is composed from.
	Login     string
	Colleague iam.Colleague
	Grants    []iam.Grant

	// Seat is the seat handle this person is bound to, and SeatAt the
	// chart position that binding was decided at. seat.go is what they
	// are for.
	Seat   string
	SeatAt uint64
}

// Row is which row of the session table a bearer landed on.
//
// THE TABLE IS A TABLE, in one place, rather than a chain of conditions spread
// through a handler. Six rows, three columns, and the whole of what a request
// is allowed to do is the cell — so a suite can assert every cell and a reader
// can see the shape rather than reconstruct it.
type Row string

const (
	// RowValid is everything checking out: the signature verifies, the
	// rotation index is current or inside the overlap, the row is present
	// and live, its epoch equals the bearer's and the person's, the
	// generation matches, and neither deadline has passed.
	RowValid Row = "valid"

	// RowEnded is a session that is over: the row says ended, or the
	// person's epoch has moved past the bearer's, or the fleet-wide
	// generation has.
	RowEnded Row = "ended"

	// RowReuse is an index the engine could not have issued, or a
	// rotation id that does not verify for the index beside it. The
	// person's revocation epoch is bumped, which ends every session they
	// hold. rotate.go argues what does and does not reach here.
	RowReuse Row = "reuse"

	// RowGone is no row, on a node whose applied position COVERS the
	// bearer's start position. That node has seen everything up to and
	// including the session's own start record, so an absent row is a
	// session that ended and was swept, not one it is waiting for.
	RowGone Row = "gone"

	// RowBehind is no row, on a node whose applied position is BELOW the
	// bearer's start position, within the stall grace. It serves reads —
	// the signature and the epoch are proof the sign-in happened — and
	// refuses writes with 503.
	//
	// THIS ROW IS WHY THE START POSITION IS IN THE BEARER. Without it
	// [RowGone] and this row are one answer, and whichever one it was
	// would be wrong half the time: 401 signs out every person whose
	// request reached a node that is a second behind, and serving would
	// honour a session that ended.
	RowBehind Row = "behind"

	// RowStalled is a node that cannot answer: the applier is more than
	// [statelog.StallGrace] behind, the person's bucket is deferred, or
	// the replicated estate could not be read at all.
	//
	// THE DEFERRED ARM IS WHY THIS IS NOT DERIVABLE FROM THE LAG ALONE. A
	// node holding one record from a newer peer whose scope covers this
	// person is perfectly caught up and still cannot say what their rows
	// are — the lag is zero and the answer is unknown.
	RowStalled Row = "stalled"

	// RowMalformed is a value that is not a bearer of this format: a
	// forged or truncated cookie, one signed under a key this node does
	// not hold, or another application's cookie on the same host.
	//
	// SEPARATE FROM [RowEnded] although both answer 401, because they are
	// different events and an operator reading a log has to be able to
	// tell an attack from an expired session.
	RowMalformed Row = "malformed"
)

// Deadline is which of a bearer's own deadlines ended a session.
//
// A VALUE BESIDE [RowEnded] RATHER THAN TWO MORE ROWS, because the table's
// answer is the same for both — refuse, and clear the cookie — and a row is a
// decision about what a request may do. What differs is what the audit trail
// says happened, and a deadline is the one way a session ends that no record
// ever states: the idle deadline lives in the bearer and nowhere else, so the
// frame that validates a bearer is the only one that can ever see it pass.
type Deadline string

const (
	// DeadlineIdle is a session unused for [Idle].
	DeadlineIdle Deadline = "idle"

	// DeadlineAbsolute is a session past the lifetime it was minted with,
	// which no re-issue moves.
	DeadlineAbsolute Deadline = "absolute"
)

// Valid reports whether d is none or one of the two deadlines.
func (d Deadline) Valid() bool {
	return d == "" || d == DeadlineIdle || d == DeadlineAbsolute
}

// Rows are the seven, in the order the design's table states them.
var Rows = []Row{
	RowValid, RowEnded, RowReuse, RowGone, RowBehind, RowStalled, RowMalformed,
}

// Need is what a request is asking to do, which picks the column.
type Need string

const (
	// NeedRead is an ordinary read.
	NeedRead Need = "read"

	// NeedWrite is anything that changes state.
	NeedWrite Need = "write"

	// NeedStepUp is a step-up surface: /config, /chart's writes, /setup,
	// /secrets, /iam and the fleet write routes.
	NeedStepUp Need = "step_up"
)

// Needs are the three.
var Needs = []Need{NeedRead, NeedWrite, NeedStepUp}

// Answer is one cell of the table.
type Answer string

const (
	// AnswerServe is: this request proceeds.
	AnswerServe Answer = "serve"

	// AnswerRefuse is 401. Reached ONLY when this node KNOWS the session
	// is over — never when it merely cannot tell.
	AnswerRefuse Answer = "refuse"

	// AnswerUnavailable is 503. A browser retries it and keeps its
	// cookie, which is the whole reason the distinction exists.
	AnswerUnavailable Answer = "unavailable"

	// AnswerStepUp is: serve if this session's proof of identity is still
	// fresh, and otherwise ask for it again. It appears in the step-up
	// column and nowhere else.
	AnswerStepUp Answer = "step_up"
)

// sessionTable is the design's table, verbatim.
//
// READ IT AS THE SPECIFICATION. Every arm in [Signer.Validate] does nothing
// but decide which ROW a bearer is on; what that row is allowed to do lives
// here and nowhere else, so a change to the policy is a change to this map
// rather than a condition somebody has to find.
var sessionTable = map[Row]struct{ Reads, Writes, StepUp Answer }{
	RowValid:     {AnswerServe, AnswerServe, AnswerStepUp},
	RowEnded:     {AnswerRefuse, AnswerRefuse, AnswerRefuse},
	RowReuse:     {AnswerRefuse, AnswerRefuse, AnswerRefuse},
	RowGone:      {AnswerRefuse, AnswerRefuse, AnswerRefuse},
	RowBehind:    {AnswerServe, AnswerUnavailable, AnswerUnavailable},
	RowStalled:   {AnswerUnavailable, AnswerUnavailable, AnswerUnavailable},
	RowMalformed: {AnswerRefuse, AnswerRefuse, AnswerRefuse},
}

// Code is the machine-readable reason a refusal or a 503 carries.
const (
	// CodeRevoked is what a 401 from this table says. ONE CODE for every
	// arm that refuses, deliberately: telling a caller whether their
	// session was revoked, expired, swept or never existed tells an
	// attacker the same.
	CodeRevoked = "session_revoked"

	// CodeUnavailable is what a 503 from this table says.
	CodeUnavailable = "identity_unavailable"
)

// Validation is what one bearer earned on this node.
type Validation struct {
	// Row is the table row, and it is set on every path including the
	// unparseable one.
	Row Row

	// Bearer is the parsed cookie, zero on [RowMalformed].
	Bearer Bearer

	// Reissue is a fresh cookie value to set, or empty. It is produced on
	// a served row only, and only when the idle deadline or the rotation
	// index has actually moved — a Set-Cookie on every response is a
	// header nobody needs.
	Reissue string

	// Reuse reports that the caller must bump this person's revocation
	// epoch, which ends every session they hold, and log
	// `iam_session_reuse_detected` at WARN.
	//
	// A FLAG RATHER THAN A WRITE FROM HERE. This package holds no
	// publisher and must not: minting and validating happen on every
	// ingress node on every request, and a validator that could append to
	// the log is one an unauthenticated caller can make write.
	Reuse bool

	// Person is the holder's row as this node has it, empty on the rows
	// that read nothing.
	Person PersonRow

	// Session is the session's own row as this node has it — when it was
	// proved — and the zero row on [RowBehind], whose whole meaning is that
	// this node has not applied it: a node that is behind serves reads on
	// the signature and the epoch, and claims no proof it cannot see.
	Session SessionRow

	// Detail says which fact decided, for a log line. It is NEVER sent to
	// the caller: the code is, and the code is the same for every refusal.
	Detail string

	// Err is the read failure behind [RowStalled], when there was one.
	Err error

	// Deadline is which of the bearer's own deadlines ended it, set on a
	// [RowEnded] a deadline decided and empty everywhere else — including
	// the ends a RECORD decided, which already said so when it landed.
	Deadline Deadline
}

// Answer is what this validation permits for one kind of request.
func (v Validation) Answer(need Need) Answer {
	cells, known := sessionTable[v.Row]
	if !known {
		// A ROW THIS BUILD CANNOT NAME REFUSES, which is the direction
		// that fails safe. The set is closed and a test walks it, so
		// reaching here means a row was added without a policy — and
		// serving by default is how a new arm ships ungated.
		return AnswerRefuse
	}
	switch need {
	case NeedRead:
		return cells.Reads
	case NeedWrite:
		return cells.Writes
	case NeedStepUp:
		return cells.StepUp
	}
	return AnswerRefuse
}

// Code is what a refusal or a 503 tells the caller.
func (v Validation) Code(need Need) string {
	switch v.Answer(need) {
	case AnswerUnavailable:
		return CodeUnavailable
	case AnswerRefuse:
		return CodeRevoked
	}
	return ""
}

// Validate resolves one cookie against this node's own rows.
//
// IT RETURNS NO ERROR, and that is the three-valued rule rather than a
// swallowed failure: a read that could not be performed is [RowStalled], which
// is an ANSWER — 503 — and carries the failure in [Validation.Err] for the
// log. Returning an error beside a verdict would give every caller two things
// to get right, and the one they would get wrong is the one that answers 401.
func (s *Signer) Validate(ctx context.Context, directory Directory,
	cookie string) Validation {

	b, err := s.parse(cookie)
	if err != nil {
		return Validation{Row: RowMalformed, Detail: err.Error()}
	}
	now := s.now()

	// THE DEADLINES FIRST, because they need nothing and because the
	// ABSOLUTE one beats everything: a re-issue moves the idle deadline
	// and never this, so a session that has reached it is over however
	// recently it was used.
	switch {
	case !now.Before(b.AbsoluteExpiresAt):
		return Validation{Row: RowEnded, Bearer: b, Deadline: DeadlineAbsolute,
			Detail: "the absolute deadline has passed"}
	case !now.Before(b.IdleExpiresAt):
		return Validation{Row: RowEnded, Bearer: b, Deadline: DeadlineIdle,
			Detail: "the idle deadline has passed"}
	}

	// THEN THE ROTATION, because reuse is the one verdict that makes this
	// node WRITE, and it must not wait on a read that may not be servable.
	// rotate.go argues what reaches each arm.
	rotation := s.rotationOf(b, now)
	if rotation == rotationReuse {
		return Validation{Row: RowReuse, Bearer: b, Reuse: true,
			Detail: fmt.Sprintf("rotation index %d is ahead of the window "+
				"this session is in", b.Rotation)}
	}

	// AND ONLY THEN THE ROWS. Nothing above this line reads anything, so
	// an unauthenticated caller cannot price a request by sending rubbish.
	identity, err := directory.Resolve(ctx, b.Lineage.String(), b.Person)
	if err != nil {
		return Validation{Row: RowStalled, Bearer: b, Err: err,
			Detail: "this node could not read the identity estate"}
	}
	if identity.Lag > statelog.StallGrace {
		return Validation{Row: RowStalled, Bearer: b,
			Detail: fmt.Sprintf("the iam applier is %s behind, past the %s "+
				"stall grace", identity.Lag, statelog.StallGrace)}
	}
	if identity.Deferred {
		return Validation{Row: RowStalled, Bearer: b,
			Detail: "this node holds a record it cannot decode whose scope " +
				"covers this person's bucket"}
	}

	// THE FLEET-WIDE GENERATION, which one write moves to end every
	// session in the company — the restore runbook's last step.
	if identity.Generation > b.Generation {
		return Validation{Row: RowEnded, Bearer: b,
			Detail: "the fleet-wide session generation has moved"}
	}

	// THE PERSON. An absent person row is NOT the absent-session arm: the
	// session's start record and the person's own are different records on
	// different subjects, so a node can hold one without the other.
	if !identity.Person.Found {
		if identity.Applied >= b.StartPosition {
			return Validation{Row: RowGone, Bearer: b,
				Detail: "this node has no row for the person the bearer names"}
		}
		// THROUGH served, LIKE THE OTHER BEHIND ARM. Both serve reads
		// on the bearer's own proof, so both must move the idle
		// deadline: an arm that served without re-issuing would let a
		// session in continuous use on a lagging node expire on the
		// deadline it was minted with.
		return s.served(RowBehind, b, identity, rotation, now,
			"this node has not applied the person's row yet")
	}
	if identity.Person.Epoch > b.Epoch {
		return Validation{Row: RowEnded, Bearer: b, Person: identity.Person,
			Detail: "the person's revocation epoch has moved past the bearer's"}
	}
	if !identity.Person.Stage.MayAct() {
		// A SUSPENDED OR RETIRED PERSON IS REFUSED HERE rather than left
		// to a grant check, because a grant check answers what somebody
		// may DO and this answers whether they are somebody at all. The
		// stage allowlist is one value wide, so a stage a newer peer
		// wrote that this build cannot name refuses.
		return Validation{Row: RowEnded, Bearer: b, Person: identity.Person,
			Detail: fmt.Sprintf("the person is %q, and only %q may act",
				identity.Person.Stage, iam.StageActive)}
	}

	// THE SESSION'S OWN ROW, and the three answers an absence has.
	switch {
	case identity.Session.Found && identity.Session.Ended:
		return Validation{Row: RowEnded, Bearer: b, Person: identity.Person,
			Detail: "the session row says it ended"}
	case identity.Session.Found && identity.Session.Epoch != b.Epoch:
		return Validation{Row: RowEnded, Bearer: b, Person: identity.Person,
			Detail: "the session row was opened at a different epoch"}
	case !identity.Session.Found && identity.Applied >= b.StartPosition:
		return Validation{Row: RowGone, Bearer: b, Person: identity.Person,
			Detail: "this node covers the session's start and has no row for it"}
	case !identity.Session.Found:
		return s.served(RowBehind, b, identity, rotation, now,
			"this node has not applied the session's start record yet")
	}
	return s.served(RowValid, b, identity, rotation, now, "")
}

// served finishes a row that is allowed to serve, re-issuing the cookie when
// something in it has actually moved.
//
// TWO REASONS TO RE-ISSUE and both are free: the idle deadline has drifted
// past [ReissueAfter], or the rotation window has turned over. Neither writes
// anything — the deadline and the index are both in the signature — so the
// only cost being managed is a Set-Cookie header, which is why the throttle
// exists at all.
func (s *Signer) served(row Row, b Bearer, identity Identity,
	rotation rotationVerdict, now time.Time, detail string) Validation {

	out := Validation{Row: row, Bearer: b, Person: identity.Person,
		Session: identity.Session, Detail: detail}
	issuedAt := b.IdleExpiresAt.Add(-Idle)
	stale := now.Sub(issuedAt) >= ReissueAfter
	if !stale && rotation != rotationBehind {
		return out
	}
	fresh, err := s.issue(b, s.rotationAt(b.Lineage, now), now)
	if err != nil {
		// A RE-ISSUE THAT CANNOT BE SIGNED DOES NOT REFUSE THE REQUEST.
		// The bearer in hand has already verified and has not expired,
		// so the session is valid; what is lost is the moved deadline,
		// which costs this session an earlier idle expiry and nothing
		// else.
		out.Detail = "served without a re-issue: " + err.Error()
		return out
	}
	out.Reissue = fresh
	return out
}
