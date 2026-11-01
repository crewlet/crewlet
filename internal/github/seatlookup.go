package github

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

	// Issuer is where each seat's token comes from: the process's own,
	// shared with every other reader of a seat's token, so the parser a
	// revision rebuilds starts with the tokens the last one held. Nil
	// gives this lookup one of its own.
	Issuer *Issuer

	own sync.Once
}

// ErrNoSeatCanRead reports a company whose agents can answer nothing yet.
var ErrNoSeatCanRead = errors.New(
	"github: no agent's app could read this repository: install an agent's app " +
		"on it, or a thread's other participants and a pull request's preview are " +
		"not read")

// issuer is the [Issuer] this lookup reads tokens through.
func (l *SeatLookup) issuer() *Issuer {
	l.own.Do(func() {
		if l.Issuer == nil {
			l.Issuer = &Issuer{}
		}
	})
	return l.Issuer
}

// token is a token for one seat good for [ToolTokenLife].
//
// CACHED by the issuer, because this is the inbound hot path: a busy
// repository produces a delivery per app per comment, and minting a token
// for each would spend one request per delivery to learn something that is
// valid for an hour. A seat whose mint GitHub refuses costs the same: the
// issuer holds the refusal for its wait ([Issuer.RetryWait]), so the next
// seat is asked without a request to learn the first one's answer again.
func (l *SeatLookup) token(ctx context.Context, seat SeatApp) (string, error) {
	minted, err := l.issuer().Token(ctx, l.Opts, seat, ToolTokenLife)
	if err != nil {
		return "", err
	}
	return minted.Token, nil
}

// Of implements [Participants].
func (l *SeatLookup) Of(
	ctx context.Context, owner, repo, kind string, number int,
) ([]string, error) {
	var people []string
	err := l.read(ctx, func(client *Client) error {
		var err error
		people, err = client.ParticipantsOf(ctx, owner, repo, kind, number)
		return err
	})
	return people, err
}

// read runs one read through the first agent's app that answers it: each
// installed seat's token in turn, the next asked when one cannot mint or its
// app does not cover the repository. THE ONE WALK, for every read the agents'
// apps make on the company's behalf (a thread's participants, a pull
// request's preview), so they cannot come to disagree about whose credential
// answers.
func (l *SeatLookup) read(ctx context.Context, fn func(*Client) error) error {
	var last error
	for _, seat := range l.Opts.Seats {
		if !Issuable(seat) {
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
		if err := fn(client); err != nil {
			// A SEAT WHOSE APP DOES NOT COVER THIS REPOSITORY answers
			// 404, which is indistinguishable from one that is not
			// there: a company can scope each agent to its own
			// repositories, so the next seat is the one to ask.
			last = err
			continue
		}
		return nil
	}
	if last != nil {
		return fmt.Errorf("%w: %w", ErrNoSeatCanRead, last)
	}
	return ErrNoSeatCanRead
}
