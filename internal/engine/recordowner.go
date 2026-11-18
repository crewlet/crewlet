package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOSE RECORD SOMEBODY ELSE'S LOGIN NAMES: the identity directory's half of
// [iam.OwnerOf]. It is the name that person ACTS under ([iam.RecordOwner]),
// asked through the SAME tables a request of theirs resolves through — the
// directory's row for the login, then [session.ResolveSeat] for the seat it
// binds, refused once that seat is gone — so a second resolution here cannot
// drift from the request path.
//
// A Tier A `token:<id>` is held by its configuration entry, not a row: the
// directory only binds it ([boundRowOf]'s rule, the request path's), so a
// token with no row is its own record. But only a token THIS node's
// `api.auth.tokens` declares: any other is nobody's whatever the directory
// says, as no credential acts as it — the typo guard `set_priorities` keeps.

// HolderRecord answers whose record a login names — see [iam.Holders].
//
// Its local read is bounded by [requestReadBudget], for [Engine.BoundSeat]'s
// reason: running out is the unknown answer (a 503), never "nobody".
func (e *Engine) HolderRecord(ctx context.Context, login string) (string, error) {
	if e == nil {
		return "", errNoDirectory
	}
	// This node's rows on a data node, a data node's through the router
	// otherwise; this node's chart and Tier A tokens apply to either.
	dir, ok := e.identityDirectory()
	if !ok {
		return "", errNoDirectory
	}
	// Only the LOCAL read is bounded here: a routed one is bounded per data
	// node by [estate.Router], and a two-second ceiling over a failover would
	// answer unknown at the first silent node where the next would answer.
	if _, routed := dir.(routedIdentity); !routed {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestReadBudget)
		defer cancel()
	}
	return holderRecordOf(ctx, dir, SeatViewOf(e), e.declaresToken, login)
}

// declaresToken reports whether this node's Tier A declares a token by id.
//
// THIS NODE'S, because this node's guard authenticates the token: a fleet
// mid-rollout may legally disagree about the list, and a token this node
// cannot authenticate is one nobody acts as here.
func (e *Engine) declaresToken(id string) bool {
	_, held := e.tierATokens[id]
	return held
}

var _ iam.Holders = (*Engine)(nil)

// holderRecordOf is [Engine.HolderRecord] over its two seams.
func holderRecordOf(ctx context.Context, dir bindingDirectory, chart session.Chart,
	declared func(id string) bool, login string) (string, error) {

	if id, isToken := strings.CutPrefix(login, iam.TokenLoginPrefix); isToken {
		// The configuration first, with no read: a login no entry
		// declares is nobody's, bound row or not.
		if declared == nil || !declared(id) {
			return "", fmt.Errorf("%w: %s (no `api.auth.tokens` entry on this "+
				"node has the id %q)", iam.ErrNoHolder, login, id)
		}
		// The token's own table, which refuses a row it cannot vouch
		// for in the direction that widens.
		row, err := boundRowOf(ctx, dir, login)
		if err != nil {
			return "", err
		}
		return recordThrough(ctx, chart, login, row)
	}
	seen, vouch, err := dir.PersonByLoginVouched(ctx, login)
	if err != nil {
		return "", fmt.Errorf("engine: read the directory's row for %s, and "+
			"whether this node holds a record about it that it has not "+
			"applied: %w", login, err)
	}
	// A ROW THIS NODE CANNOT VOUCH FOR IS UNKNOWN IN BOTH DIRECTIONS (a
	// token's binding only in the widening one): a stale binding or a stale
	// absence each writes a record where nothing of theirs reads. An absent
	// row is vouched for over the ROOT, which every deferral covers: a record
	// this node could not decode may be the enrolment that took this login.
	switch {
	case vouch.Lag > statelog.StallGrace:
		return "", fmt.Errorf("engine: this node is %s behind on identity "+
			"changes (over the %s limit), so it can't look up %s",
			vouch.Lag, statelog.StallGrace, login)
	case vouch.Deferred:
		return "", fmt.Errorf("engine: this node has an identity change it "+
			"can't read that may be about %s, so it can't look them up",
			login)
	case seen.ID == "":
		// NOBODY holds the login, so nothing is kept under it either.
		return "", fmt.Errorf("%w: %s", iam.ErrNoHolder, login)
	}
	return recordThrough(ctx, chart, login, session.PersonRow{
		Found: true, Stage: seen.Stage, Login: seen.Login,
		Seat: seen.Seat,
	})
}

// recordThrough is the record a directory row keeps a login's holder's work
// under: the seat the chart resolves its binding to, or the login for a row
// bound to none.
//
// THE STAGE DOES NOT MOVE IT: a suspended person's record is still their
// seat's, since unsticking an absent person's queue is what writing another's
// record is for. A Tier A token's inactive binding was already dropped by
// [boundRowOf], as that token then acts as itself.
func recordThrough(ctx context.Context, chart session.Chart, login string,
	row session.PersonRow) (string, error) {

	if !row.Found || row.Seat == "" {
		return login, nil
	}
	binding := session.ResolveSeat(ctx, chart, row)
	switch binding.Answer() {
	case session.AnswerServe:
		if handle := binding.Handle(); handle != "" {
			return handle, nil
		}
		return login, nil
	case session.AnswerUnavailable:
		if binding.Err != nil {
			return "", fmt.Errorf("engine: %s is bound to %s and this node "+
				"cannot say where that seat is now (%s): %w", login, row.Seat,
				binding.Detail, binding.Err)
		}
		return "", fmt.Errorf("engine: %s is bound to %s and this node cannot "+
			"say where that seat is now: %s", login, row.Seat, binding.Detail)
	}
	return "", fmt.Errorf("%w: %s (%s). Move them to another seat, or name "+
		"the seat instead", iam.ErrHolderUnseated, login, binding.Detail)
}
