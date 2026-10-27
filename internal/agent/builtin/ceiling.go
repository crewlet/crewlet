package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// WHO A SEAT'S TURN IS ACTING FOR — the requester ceiling ([authz.Ceiling]),
// which every family of tools that reads or writes for somebody decides again:
// the artifacts tools and their datastore's on the stored artifact
// ([authz.DecideWithin]), and the code tools on a repository's grant as well
// (codetools.go).
//
// # One composition, every family's
//
// An agent reads what it may read. Somebody who may not can still message an
// agent that may, and ask it to read the thing out — so inside a turn
// somebody woke, every family asks again for that somebody. WHO that is is one
// question with one answer, composed here once: were each family to compose
// its own, a turn woken by a sender nobody maps to would be a stranger's to
// one family and nobody's to the next, and the second would read for the
// stranger whatever the agent itself may.
//
// # The turn's, and no further
//
// The answer is read off what woke THIS turn (turnctx.Turn.Requester and
// Senders), so it holds this turn alone: a colleague the seat asks with
// a2a_ask answers in a turn the asking seat woke, decided for that seat and
// not for whoever woke it, and the answer wakes the seat in a turn of its own
// work. Carrying a requester into the turns a turn sets off would mean every
// ask, answer and self-filed item carrying it, which none does; the docs say
// where the bound ends (docs/concepts/code-host.md, "Reads made for somebody
// else").

// Requesters composes the principal a seat's turn is acting for.
type Requesters interface {
	// Requester is the seat handle's principal as the authority table
	// decides it: the person bound to a human seat, or the agent seat
	// itself. An error is UNKNOWN, never "nobody".
	Requester(ctx context.Context, handle string) (iam.Principal, error)
}

// errNoRequesters is a ceiling asked of a surface that composes nobody a turn
// is acting for: a wiring that decided nothing, answered as a ceiling this
// node could not compose rather than a turn nobody woke.
var errNoRequesters = errors.New("builtin: this surface composes nobody a turn is acting for")

// ceilingOf is who a seat's turn is acting for: nobody beyond the seat for a
// turn nobody woke, for one the outcome of its own act woke and for a call
// outside any turn; the requester a known principal woke it as — never the
// seat itself; or a stranger, a sender the turn names with no requester the
// company knows behind it. A KNOWN REQUESTER IS READ BEFORE THE SENDERS: a
// person's message carries both their seat and their own identity as its
// sender. An error is a requester this node could not compose, which is
// UNKNOWN: never read as a turn nobody woke, which is the one kind that bounds
// nothing.
//
// THE SEAT'S OWN VOICE IS NO STRANGER'S. The outcome of a seat's own act — a
// workflow or a pipeline its own push failed, which a code host tells the
// actor of — wakes it with itself as the sender, and that is its own work.
// Every OTHER sender of a turn no known requester woke is a stranger, beside
// the seat's own voice or a colleague's: a thread somebody the company does
// not know started is theirs, whoever else speaks in it.
func ceilingOf(ctx context.Context, turn *turnctx.Turn, seat string, r Requesters) (authz.Ceiling, error) {
	if turn == nil {
		return authz.Ceiling{Kind: authz.CeilingNone}, nil
	}
	if handle := strings.TrimSpace(turn.Requester); handle != "" && handle != seat {
		if r == nil {
			return authz.Ceiling{}, errNoRequesters
		}
		requester, err := r.Requester(ctx, handle)
		if err != nil {
			return authz.Ceiling{}, fmt.Errorf("builtin: compose requester %s: %w", handle, err)
		}
		return authz.Ceiling{Kind: authz.CeilingRequester, Principal: requester}, nil
	}
	if slices.ContainsFunc(turn.Senders, func(s types.CanonicalIdentity) bool {
		// AN EMPTY HANDLE IS NOBODY'S, never the seat's: a seat the
		// caller named as "" must not make every unknown sender its own.
		handle := strings.TrimSpace(s.Handle)
		return handle == "" || handle != seat
	}) {
		return authz.Ceiling{Kind: authz.CeilingStranger}, nil
	}
	return authz.Ceiling{Kind: authz.CeilingNone}, nil
}
