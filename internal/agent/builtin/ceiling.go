package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
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
// turn nobody woke and for a call outside any turn; the requester a known
// principal woke it as — never the seat itself; or a stranger, a sender on a
// chat surface no seat or person maps to. A KNOWN REQUESTER IS READ BEFORE
// THE SENDERS: a person's message carries both their seat and their own
// identity as its sender, and only a turn whose senders nobody the company
// holds stands behind is a stranger's. An error is a requester this node
// could not compose, which is UNKNOWN: never read as a turn nobody woke,
// which is the one kind that bounds nothing.
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
	if len(turn.Senders) > 0 {
		return authz.Ceiling{Kind: authz.CeilingStranger}, nil
	}
	return authz.Ceiling{Kind: authz.CeilingNone}, nil
}
