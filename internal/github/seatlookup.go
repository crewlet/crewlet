package github

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SeatLookup reads a thread's participants through the agents' own apps.
//
// # It replaced a shared organization token, and the token had nothing left to do
//
// Fan-out needs one question answered that no payload carries: who has
// COMMENTED on this issue, and who has REVIEWED this pull request. That took
// a personal access token an operator pasted in, held at the company and used
// for every repository.
//
// An agent that acts as itself already holds a credential that can answer it.
// Its app is installed on the repositories it works in and every tier grants
// `issues:read` and `pull_requests:read`, so the reader is there for free and
// scoped to what that agent may see, where the shared token was scoped to
// whatever the person who made it could reach.
//
// ANY INSTALLED SEAT ANSWERS FOR THE THREAD, and the first one that mints is
// taken. The question is about the thread rather than about an agent, and
// every app on one organization sees the same commenters; trying the next
// seat when one refuses is what keeps the answer available while an operator
// is mid-rollout, with some apps installed and others not.
type SeatLookup struct {
	// Opts is the same roster the reconcile runs on, captured when the
	// parser was built. An app created or installed later writes the
	// company document, which applies a revision and rebuilds the parser,
	// so the roster follows without a restart.
	Opts SeatAppOptions

	// Now is the clock the token freshness is judged on. Nil takes the
	// wall clock.
	Now func() time.Time

	mu     sync.Mutex
	tokens map[int64]*InstallationToken
}

// ErrNoSeatCanRead reports a company whose agents can answer nothing yet.
var ErrNoSeatCanRead = errors.New(
	"github: no agent's app could read this thread, so its other participants " +
		"are not looked up")

func (l *SeatLookup) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

// token is a fresh installation token for one seat, minted at most once per
// hour per installation.
//
// CACHED, because this is the inbound hot path: a busy repository produces a
// delivery per app per comment, and minting a token for each would spend one
// request per delivery to learn something that is valid for an hour. The
// cache is keyed on the INSTALLATION rather than the seat, since that is what
// the token is issued against.
func (l *SeatLookup) token(ctx context.Context, seat SeatApp) (string, error) {
	l.mu.Lock()
	held, ok := l.tokens[seat.InstallationID]
	l.mu.Unlock()
	if ok && held.Fresh(l.now()) {
		return held.Token, nil
	}

	minted, err := TokenFor(ctx, l.Opts, seat)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	if l.tokens == nil {
		l.tokens = map[int64]*InstallationToken{}
	}
	l.tokens[seat.InstallationID] = minted
	l.mu.Unlock()
	return minted.Token, nil
}

// Of implements [Participants].
func (l *SeatLookup) Of(
	ctx context.Context, owner, repo, kind string, number int,
) ([]string, error) {
	var last error
	for _, seat := range l.Opts.Seats {
		if seat.InstallationID == 0 || seat.Key == "" {
			continue
		}
		token, err := l.token(ctx, seat)
		if err != nil {
			last = err
			continue
		}
		client, err := NewClient(ClientOptions{
			APIBase: l.Opts.APIBase, WebBase: l.Opts.WebBase, Token: token,
		})
		if err != nil {
			last = err
			continue
		}
		people, err := client.ParticipantsOf(ctx, owner, repo, kind, number)
		if err != nil {
			// A SEAT WHOSE APP DOES NOT COVER THIS REPOSITORY answers
			// 404, which is indistinguishable from one that is not
			// there: a company can scope each agent to its own
			// repositories, so the next seat is the one to ask.
			last = err
			continue
		}
		return people, nil
	}
	if last != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoSeatCanRead, last)
	}
	return nil, ErrNoSeatCanRead
}
