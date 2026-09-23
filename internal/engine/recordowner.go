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
// [iam.OwnerOf].
//
// # The answer is the name that person ACTS under
//
// A person's inbox, pins and priorities are kept under the name every write
// they make is attributed to — [iam.RecordOwner] of the principal a request of
// theirs resolves to. So the question "whose record is `jane.doe`'s?" has the
// SAME answer as "what would a request from jane.doe act as?", and it is asked
// through the SAME tables: the directory's row for the login, and
// [session.ResolveSeat] over this node's chart view for the seat it binds —
// followed through a rename, refused once the seat is gone. A second
// resolution written here would be the place the two drifted, and the one it
// replaced had no directory in it at all: the login was read literally, so a
// bound person's record was looked for, and written, where nothing of theirs
// is kept.
//
// # A Tier A token is a principal with no row
//
// `token:<id>` acts from the configuration alone; the directory only BINDS it,
// through [boundRowOf]'s rule — an active machine bound to a seat — which is
// the rule the request path applies to it. So a token login with no row names
// the token's own record rather than nobody: its absence from the directory is
// the ordinary state of a break-glass credential, not the absence of one.

// HolderRecord answers whose record a login names — see [iam.Holders].
//
// BOUNDED BY [requestReadBudget], for [Engine.BoundSeat]'s reason: it is one
// keyed local read in front of a request, and running out is the unknown
// answer — a 503 — never "nobody".
func (e *Engine) HolderRecord(ctx context.Context, login string) (string, error) {
	if e == nil {
		return "", errNoIdentityDomain
	}
	reader := e.IAM()
	if reader == nil {
		return "", errNoIdentityDomain
	}
	ctx, cancel := context.WithTimeout(ctx, requestReadBudget)
	defer cancel()
	return holderRecordOf(ctx, reader, SeatViewOf(e), login)
}

var _ iam.Holders = (*Engine)(nil)

// holderRecordOf is [Engine.HolderRecord] over its two seams.
func holderRecordOf(ctx context.Context, dir bindingDirectory, chart session.Chart,
	login string) (string, error) {

	if strings.HasPrefix(login, iam.TokenLoginPrefix) {
		// THE TOKEN'S OWN TABLE, which already refuses a row it cannot
		// vouch for in the direction that widens.
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
	// A ROW THIS NODE CANNOT VOUCH FOR IS UNKNOWN IN BOTH DIRECTIONS — the
	// opposite of a token's binding, where only the widening one is. A
	// stale binding names a seat the person may since have left and a
	// stale absence one they may since have joined, and either wrong
	// answer writes a record under a name nothing of theirs reads. An
	// absent row is vouched for over the ROOT, which every deferral covers:
	// nothing can say which bucket a record this node could not decode is
	// about, and it may be the enrolment that claimed this login.
	switch {
	case vouch.Lag > statelog.StallGrace:
		return "", fmt.Errorf("engine: this node's identity applier is %s "+
			"behind — past the %s stall grace — so it cannot say whose record "+
			"%s is", vouch.Lag, statelog.StallGrace, login)
	case vouch.Deferred:
		return "", fmt.Errorf("engine: this node holds an identity record it "+
			"cannot decode that may be about %s, so it cannot say whose "+
			"record that is", login)
	case seen.ID == "" || seen.Reserved:
		// NOBODY, or the reservation an unfinished enrolment leaves: the
		// login is held and nobody holds it yet, so nothing is kept under
		// it either.
		return "", fmt.Errorf("%w: %s", iam.ErrNoHolder, login)
	}
	return recordThrough(ctx, chart, login, session.PersonRow{
		Found: true, Stage: seen.Stage, Login: seen.Login,
		Seat: seen.Seat, SeatAt: seen.SeatAt,
	})
}

// recordThrough is the record a directory row keeps a login's holder's work
// under: the seat the chart resolves its binding to, or the login for a row
// bound to none.
//
// THE STAGE DOES NOT MOVE IT. A suspended person's record is still their
// seat's — an administrator unsticking the queue of somebody who is away is
// what writing another person's record is for — and a Tier A token's inactive
// binding was already dropped by [boundRowOf], because that token then acts
// as itself.
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
	return "", fmt.Errorf("%w: %s — %s; rebind them in the identity directory, "+
		"or name the seat", iam.ErrHolderUnseated, login, binding.Detail)
}
