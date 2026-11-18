package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// turnRequesters composes the principal a seat's turn acts for: the requester
// ceiling every call in a turn somebody woke is decided against
// (builtin.Requesters). ONE value answers the app verbs and the relations the
// artifacts, datastore and code tools ask, so a requester is one principal
// whichever tool decides. Admin is never in a ceiling: an administrator asking
// an agent for somebody's private artifact asks as a colleague.
//
// An agent seat is the seat principal (every app). A person's seat is the
// person the routed identity directory binds to it, at their stage, with the
// apps their grants name (ADR-0109); a seat no row binds holds no app, since a
// permission is a person's (ADR-0033). A credential acting as no seat is
// [turnRequesters.Credential].
type turnRequesters struct{ engine *Engine }

// Credential is a credential acting as no seat, by its login, as THIS node
// resolves it now (builtin.Requesters.Credential): a machine principal with
// the apps its declaration's grants name, never admin, the same on every node
// whatever its admin posture.
//
// By the login's namespace alone: a `token:` login is a Tier A token of this
// node's own configuration (a directory row under it is read only for whether
// it binds the token to a seat, making it a person's), any other a directory
// service account. Not placed (false, no error; a stranger): a login neither
// names, a bound token, or an account removed, suspended, bound or not a
// machine. An error, a directory unread or unvouched, is UNKNOWN: neither a
// stranger (perhaps active) nor a credential (perhaps removed).
func (r turnRequesters) Credential(ctx context.Context, login string) (iam.Principal, bool, error) {
	dir, ok := r.engine.identityDirectory()
	if !ok {
		return iam.Principal{}, false, errNoDirectory
	}
	if _, routed := dir.(routedIdentity); !routed {
		// ONE KEYED READ OF THIS NODE'S OWN STORE, bounded as a request's
		// is ([requestReadBudget]); a routed one is bounded by the router.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestReadBudget)
		defer cancel()
	}
	return credentialOf(ctx, dir, r.engine.tierATokens, login)
}

// credentialOf is [turnRequesters.Credential] over its two seams: the identity
// directory this node reads, and this node's Tier A tokens with the grants each
// declares.
func credentialOf(ctx context.Context, dir bindingDirectory, tokens map[string][]iam.Grant,
	login string) (iam.Principal, bool, error) {

	if id, isToken := strings.CutPrefix(login, iam.TokenLoginPrefix); isToken {
		grants, declared := tokens[id]
		if !declared {
			return iam.Principal{}, false, nil
		}
		bound, err := boundRowOf(ctx, dir, login)
		switch {
		case err != nil:
			return iam.Principal{}, false, err
		case bound.Found:
			return iam.Principal{}, false, nil
		}
		return iam.Principal{Kind: iam.KindMachine, Login: login, Stage: iam.StageActive,
			Grants: iam.AppsOf(grants)}, true, nil
	}
	seen, vouch, err := dir.PersonByLoginVouched(ctx, login)
	switch {
	case err != nil:
		return iam.Principal{}, false, fmt.Errorf("engine: read the directory's row "+
			"for %s: %w", login, err)
	case seen.ID == "" || seen.Kind != iam.KindMachine || seen.Seat != "" || !seen.Stage.MayAct():
		return iam.Principal{}, false, nil
	case vouch.Lag > statelog.StallGrace:
		return iam.Principal{}, false, fmt.Errorf("engine: can't confirm what %s may "+
			"use: this node is %s behind on identity changes (limit %s)",
			login, vouch.Lag, statelog.StallGrace)
	case vouch.Deferred:
		return iam.Principal{}, false, fmt.Errorf("engine: can't confirm what %s may "+
			"use: this node holds an identity change it can't read", login)
	}
	p := iam.Principal{Kind: iam.KindMachine, Login: seen.Login, Stage: seen.Stage,
		Grants: iam.AppsOf(seen.Grants)}
	if id, err := uuid.Parse(seen.ID); err == nil {
		p.ID = id
	}
	return p, true, nil
}

// errNoSuchSeat is a requester's handle the organisation holds no seat for:
// a definitive answer, which a caller asking on somebody's behalf long after
// they asked (the merge queue's close, codequeue.go) tells apart from a read
// that failed.
var errNoSuchSeat = errors.New("engine: the organisation holds no seat")

func (r turnRequesters) Requester(ctx context.Context, handle string) (iam.Principal, error) {
	seat, found, err := SeatViewOf(r.engine).Seat(ctx, handle)
	switch {
	case err != nil:
		return iam.Principal{}, err
	case !found:
		return iam.Principal{}, fmt.Errorf("%w %q", errNoSuchSeat, handle)
	}
	p := iam.Principal{Seat: seat.Handle, Position: seat.Unit, Stage: iam.StageActive}
	if org.RoleKind(seat.Kind) != org.KindHuman {
		p.Kind, p.Grants = iam.KindSeat, slices.Clone(iam.Apps)
		return p, nil
	}
	p.Kind = iam.KindPerson
	if r.engine.router == nil {
		return iam.Principal{}, errors.New("engine: this node reaches no identity directory")
	}
	// ONE SEAT'S REQUESTER, never the whole directory: this runs on a call
	// of any tool that decides for somebody, so it is the routed point read,
	// whose cost does not grow with everyone the company ever hired or removed.
	h, held, err := r.engine.router.Identity().Requester(ctx, seat.Handle)
	switch {
	case err != nil:
		return iam.Principal{}, err
	case held && h.Removed:
		// THE PERSON WHO HELD IT WAS REMOVED, and nobody has held it
		// since: whoever speaks as the seat now acts as nobody.
		p.Stage = iam.StageRetired
		return p, nil
	case held:
		// THE PERSON BY THEIR ID AND SEAT: every artifact relation of a
		// person is decided on the seat they hold (a person's artifacts
		// are owned under it), so no login is needed for the ceiling.
		if id, err := uuid.Parse(h.Person); err == nil {
			p.ID = id
		}
		p.Stage = h.Stage
		if p.Stage.MayAct() {
			p.Grants = iam.AppsOf(h.Grants)
		}
		return p, nil
	}
	// A SEAT NOBODY HOLDS IN THE DIRECTORY is a person the chart knows by
	// their contacts alone: their seat's shares are theirs, and no app is.
	return p, nil
}
