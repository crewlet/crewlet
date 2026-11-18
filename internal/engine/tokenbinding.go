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
// The binding is a directory row, not a field of the company document: a
// MACHINE enrolled under the token's login (`token:<id>`) and bound to a seat
// by one record on the directory subject, so two holders cannot hold one seat.
// Whether that seat may be acted as NOW (present, human, not removed) is
// internal/api/auth's question, through [SeatView] and [session.ResolveSeat],
// the path a signed-in person's binding takes: two resolutions of "which seat
// is this" would drift, and one without the org let a token act as a removed
// seat.

// errNoDirectory is what an engine holding no identity directory answers: a
// nil one, a data node whose core runtime is not up yet, or one built by hand
// with neither a core nor a router.
//
// An error, never an empty answer: read as "bound to nothing", a bound
// credential would act as itself here and as its seat on every other node, one
// actor under two names in one audit trail. It cannot tell, which is 503.
var errNoDirectory = errors.New("engine: this engine holds no identity " +
	"directory, so it cannot say who holds what")

// BoundSeat is the identity directory's binding for a Tier A token's login, as
// the seat table reads a holder's row: `Found` with the seat handle; the zero
// row and no error when unbound (an operator, the usual state); an error,
// never the zero row, when this node cannot say ([boundRowOf] has which rows
// bind and when one is unknown).
//
// A data node reads its own rows within [requestReadBudget]. A node without
// `data` asks one through the estate's router (internal/estate's Binding) and
// weighs that row and its vouch by the same [boundRowOf], so a stale node
// answering is refused as it would refuse its own request; the router and the
// caller bound it, for [Engine.HolderRecord]'s reason.
func (e *Engine) BoundSeat(ctx context.Context, login string) (session.PersonRow, error) {
	if e == nil {
		return session.PersonRow{}, errNoDirectory
	}
	dir, ok := e.identityDirectory()
	if !ok {
		return session.PersonRow{}, errNoDirectory
	}
	if _, routed := dir.(routedIdentity); !routed {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestReadBudget)
		defer cancel()
	}
	return boundRowOf(ctx, dir, login)
}

// bindingDirectory is the part of the identity reader a token's binding reads,
// declared by its one caller so [boundRowOf] is tested against a fake.
type bindingDirectory interface {
	PersonByLoginVouched(ctx context.Context, login string) (iamdomain.Sighting,
		iamdomain.Vouch, error)
}

// boundRowOf is [Engine.BoundSeat] over its seam.
//
// Only an ACTIVE MACHINE binds: `token:<id>` is a machine's login grammar
// (internal/iam), and a person row under it anyway is ignored, or whoever chose
// that name acts as the deployment's token. A bound row this node cannot vouch
// for (applier past [statelog.StallGrace], or a deferred record covering its
// bucket: the session table's two facts) is unknown, since a stale binding may
// be withdrawn and honouring it widens. An unbound answer is served even then:
// it is the narrower surface, and the one a break-glass token (normally bound
// to nothing) needs on a node whose identity applier is what it came to fix.
func boundRowOf(ctx context.Context, dir bindingDirectory, login string) (
	session.PersonRow, error) {

	if login == "" {
		return session.PersonRow{}, nil
	}
	// THE ROW AND HOW FAR THIS NODE CAN VOUCH FOR IT, in one snapshot: read
	// apart, a record reprocessed between the two paired a binding from
	// before it with "nothing deferred".
	seen, vouch, err := dir.PersonByLoginVouched(ctx, login)
	if err != nil {
		return session.PersonRow{}, fmt.Errorf("engine: read the directory's "+
			"row for %s, and whether this node holds a record about it that it "+
			"has not applied: %w", login, err)
	}
	if seen.ID == "" || seen.Kind != iam.KindMachine || !seen.Stage.MayAct() ||
		seen.Seat == "" {
		return session.PersonRow{}, nil
	}
	switch {
	case vouch.Lag > statelog.StallGrace:
		return session.PersonRow{}, fmt.Errorf("engine: can't confirm %s is bound "+
			"to %s: this node is %s behind on identity changes (limit %s)",
			login, seen.Seat, vouch.Lag, statelog.StallGrace)
	case vouch.Deferred:
		return session.PersonRow{}, fmt.Errorf("engine: can't confirm %s is bound "+
			"to %s: this node holds an identity change it can't read",
			login, seen.Seat)
	}
	// THE BINDING AND NOTHING ELSE: a Tier A token's authority is what Tier A
	// declares, clamped by the ceiling; the row's grants would let the
	// identity estate raise a credential the configuration pinned.
	return session.PersonRow{
		Found: true, Stage: seen.Stage, Login: seen.Login,
		Seat: seen.Seat,
	}, nil
}

// requestReadBudget bounds one identity read in front of a request on a node
// holding the rows: a Tier A token's binding ([Engine.BoundSeat]) and a
// machine token's row ([Engine.MachineToken]). The router bounds a routed one
// ([Engine.HolderRecord]'s reason).
//
// Two seconds: each is a keyed local read of a replicated table, so nearing a
// second means the store is sick, and a longer budget would hold every such
// credential's requests behind one sick file handle. Running out is unknown, a
// 503, never an unbound or refused credential. One constant for both: the
// same read on the same store and request path.
const requestReadBudget = 2 * time.Second
