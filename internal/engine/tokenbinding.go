package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHICH SEAT A TIER A TOKEN ACTS AS: the identity directory's half.
//
// # The binding moved out of the org chart
//
// It used to be a field on a human seat's contact block naming one of Tier A's
// token ids. That field is gone, and with it the whole idea that the COMPANY
// DOCUMENT says who a credential is — a seat's contact block is how to reach a
// person, and a credential is something they hold. The identity estate says it
// instead: a MACHINE enrolled under the token's own login (`token:<id>`) and
// bound to a seat, arbitrated on `iam.seat.<handle>` so two holders cannot
// claim one seat.
//
// # This is half, and the other half is the SAME as a session's
//
// What this answers is the directory row. Whether the seat it names is one the
// credential may act as NOW — present, human, not removed, followed through a
// rename — is internal/api/auth's question, asked through [SeatView] and
// [session.ResolveSeat], which is exactly what a signed-in person's binding
// goes through. Two resolutions of "which seat is this" would be two answers
// that drift, and the one that drifted here had no chart in it at all: a token
// went on acting as a removed seat, a renamed one, and — through a handle the
// chart had since given somebody else — a seat that was never its own.

// errNoIdentityDomain is what a node that applies no identity records answers.
//
// AN ERROR AND NOT AN EMPTY BINDING. A node that does not run the domain has
// a legitimately empty copy of its tables, and reading that emptiness as "this
// token is bound to nothing" would have a bound credential act as itself on
// this node and as its seat on the next — one actor under two names in one
// audit trail. The full API is only ever served beside the domain, so reaching
// this is a node that cannot tell, which is 503.
var errNoIdentityDomain = errors.New("engine: this node runs no identity " +
	"domain, so it cannot say which seat a credential is bound to")

// BoundSeat is the identity directory's binding for a Tier A token's login, as
// the seat table reads a holder's row.
//
// # Three answers
//
//   - A row the credential ACTS THROUGH: `Found`, with the seat handle and the
//     chart position the binding was decided at.
//   - NO SUCH BINDING, which is the zero row and no error — the ordinary state
//     of every Tier A token on a fresh estate: an operator rather than a
//     colleague.
//   - This node CANNOT SAY, which is an error and never the zero row.
//
// # Only an ACTIVE MACHINE binds a credential
//
// `token:<id>` is a machine's name, and internal/iam holds each login grammar
// to its kind, so no person can be enrolled under one or renamed into one. A
// person row found here anyway — written before that rule, or by a build
// without it — is not the credential's and is IGNORED rather than honoured:
// honouring it is the deployment's own token acting as whoever chose that
// name. A suspended or retired machine binds nothing either, because only an
// active principal may act and a binding is a way of acting.
//
// # A reservation binds nothing
//
// A row whose enrolment stopped after its login claim — a machine enrolled
// under `token:<id>` whose content record never landed — holds the login and
// is nobody: no kind, no stage, no seat anybody decided. The reader reports it
// as a RESERVATION rather than failing to decode it — a sighting with no kind —
// and the machine check below answers UNBOUND for it, which is the narrower
// surface. It used to answer UNKNOWN, and the
// token it names then got 503 on every guarded route until a sweep collected
// the row — a break-glass credential locked out by a binding that never
// existed.
//
// # When a row cannot be vouched for
//
// A binding this node holds is refused as UNKNOWN when its applier is past
// [statelog.StallGrace] or it holds a newer peer's record covering the row's
// bucket — exactly the two facts the session table refuses a cookie on. It is
// refused only in the direction that WIDENS: a stale bound row may be a
// binding the fleet has since withdrawn, or a machine it has since suspended,
// and honouring it is the token acting as a seat it no longer holds. An
// UNBOUND answer from the same node is served, because acting as the bare
// credential is the narrower surface — and it is the posture break-glass
// depends on, since an operator's token is normally bound to nothing and must
// still reach a node whose identity applier is the thing they came to fix.
func (e *Engine) BoundSeat(ctx context.Context, login string) (session.PersonRow, error) {
	if e == nil {
		return session.PersonRow{}, errNoIdentityDomain
	}
	reader := e.IAM()
	if reader == nil {
		return session.PersonRow{}, errNoIdentityDomain
	}
	ctx, cancel := context.WithTimeout(ctx, requestReadBudget)
	defer cancel()
	return boundRowOf(ctx, reader, login)
}

// bindingDirectory is the part of the identity reader a token's binding reads.
//
// DECLARED HERE, by its one caller, so the rule above is exercised against a
// fake rather than only through a running domain.
type bindingDirectory interface {
	PersonByLogin(ctx context.Context, login string) (iamdomain.Sighting, error)
	Staleness(person string) (lag time.Duration, deferred bool)
}

// boundRowOf is [Engine.BoundSeat] over its seam.
func boundRowOf(ctx context.Context, dir bindingDirectory, login string) (
	session.PersonRow, error) {

	if login == "" {
		return session.PersonRow{}, nil
	}
	seen, err := dir.PersonByLogin(ctx, login)
	if err != nil {
		return session.PersonRow{}, fmt.Errorf("engine: read the directory's "+
			"row for %s: %w", login, err)
	}
	if seen.ID == "" || seen.Kind != iam.KindMachine || !seen.Stage.MayAct() ||
		seen.Seat == "" {
		return session.PersonRow{}, nil
	}
	lag, deferred := dir.Staleness(seen.ID)
	switch {
	case lag > statelog.StallGrace:
		return session.PersonRow{}, fmt.Errorf("engine: %s is bound to %s on "+
			"this node, and its identity applier is %s behind — past the %s "+
			"stall grace, so it cannot say the binding still stands", login,
			seen.Seat, lag, statelog.StallGrace)
	case deferred:
		return session.PersonRow{}, fmt.Errorf("engine: %s is bound to %s on "+
			"this node, which holds a record it cannot decode about that "+
			"machine's bucket, so it cannot say the binding still stands",
			login, seen.Seat)
	}
	// THE BINDING AND NOTHING ELSE. The row's grants and colleague level
	// are deliberately left behind: a Tier A token's authority is what
	// Tier A declares for it, clamped by the ceiling, and a directory row
	// that could widen it would make the identity estate a way to raise a
	// credential the configuration pinned.
	return session.PersonRow{
		Found: true, Stage: seen.Stage, Login: seen.Login,
		Seat: seen.Seat, SeatAt: seen.SeatAt,
	}, nil
}

// requestReadBudget bounds what one identity read in front of a request may
// cost: a Tier A token's binding ([Engine.BoundSeat]) and a machine token's
// row ([Engine.MachineToken]).
//
// TWO SECONDS: each is a keyed local read of a replicated table on this node's
// own store, so anything approaching a second means the store is in trouble
// rather than that the answer is slow — and it sits in front of every request
// such a credential makes, so a longer one would hold the whole surface behind
// one sick file handle. Running out is the unknown answer, a 503, never an
// unbound credential or a refused one. ONE CONSTANT for both because they are
// the same read against the same store on the same request path; two would be
// two opinions about how sick a file handle may be.
const requestReadBudget = 2 * time.Second
