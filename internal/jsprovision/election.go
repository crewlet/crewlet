package jsprovision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
)

// Election is the longest a replicated group goes without a leader it can be
// heard from once its old one is gone, at the pinned nats-server's raft
// defaults (v2.15.3, server/raft.go): a follower campaigns when its election
// timer fires, at most maxElectionTimeoutDefault after the last heartbeat it
// heard (9 s, raft.go:329), and the leader it elects is heard within one
// hbIntervalDefault (1 s, raft.go:331). Package doc, "A group with no leader is
// waited out, not answered".
const Election = 9*time.Second + time.Second

// Leaderless reports a broker ANSWERING that the group a request needs has no
// leader: 503, err_code 10008, as the client decodes it (*jetstream.APIError)
// or a raw read does (*server.ApiError, internal/jsapi's leader reads), or a
// publish nobody heard to a stream its caller found there ([Unheard]). Not
// silence ([Unanswered]), which says nothing about who leads, and never a
// stream that is not there ([StreamAbsent]).
func Leaderless(err error) bool {
	if errors.As(err, new(*unheard)) {
		return true
	}
	var client *jetstream.APIError
	if errors.As(err, &client) {
		return client.ErrorCode == errCodeNoLeader
	}
	var raw *server.ApiError
	return errors.As(err, &raw) && raw.ErrCode == uint16(server.JSClusterNotAvailErr)
}

// Unheard is err, a publish's [jetstream.ErrNoStreamResponse], once its caller
// has found the stream there: [Leaderless] then, since only a stream's leader
// subscribes its subjects (server/stream.go, setLeader). Without that finding
// the same answer is a stream or a subject nobody binds, which no wait clears.
func Unheard(err error) error { return &unheard{err} }

type unheard struct{ err error }

func (u *unheard) Error() string {
	return fmt.Sprintf("%v: the stream is there, so it has no leader", u.err)
}

func (u *unheard) Unwrap() error { return u.err }

// StreamAbsent reports a broker answering that the stream asked about is not
// there (err_code 10059), as the client decodes it or a raw read does.
func StreamAbsent(err error) bool {
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return true
	}
	var client *jetstream.APIError
	if errors.As(err, &client) {
		return client.ErrorCode == jetstream.JSErrCodeStreamNotFound
	}
	var raw *server.ApiError
	return errors.As(err, &raw) && raw.ErrCode == uint16(server.JSStreamNotFoundErr)
}

// errCodeNoLeader is the client's spelling of JSClusterNotAvailErr, which the
// client exports no constant for.
const errCodeNoLeader = jetstream.ErrorCode(server.JSClusterNotAvailErr)

// ThroughElection asks one until its answer is not [Leaderless], one [ReAsk]
// apart, while ctx allows, or for one [Election] from the first such answer
// where ctx has no deadline; then it answers [*NoLeader] naming group. Every
// ask gets ctx itself, so what bounds one request is unchanged. Package doc,
// "A group with no leader is waited out, not answered".
func ThroughElection[T any](ctx context.Context, group string, one func(context.Context) (T, error)) (T, error) {
	wait := ctx
	for {
		v, err := one(ctx)
		if !Leaderless(err) {
			return v, err
		}
		if wait == ctx {
			// A CALLER WITH NO DEADLINE WAITS ONE [Election] AT MOST,
			// counted from this first leaderless answer.
			if _, set := ctx.Deadline(); !set {
				var cancel context.CancelFunc
				wait, cancel = context.WithTimeout(ctx, Election)
				defer cancel()
			}
		}
		// [pause], the package's one wait between asks: ended by wait
		// and by its deadline passing, whether or not wait says so yet.
		if !pause(wait, ReAsk) {
			var zero T
			return zero, &NoLeader{Group: group, Answer: err, Err: wait.Err()}
		}
	}
}

// NoLeader is a read or an append whose time ran out while Group, a stream,
// answered that it had no leader ([ThroughElection]).
type NoLeader struct {
	// Group is the stream whose group, or whose metadata group, had no
	// leader.
	Group string
	// Answer is the last answer saying so, in the broker's words.
	Answer error
	// Err is why the wait ended: its context's error.
	Err error
}

func (e *NoLeader) Error() string {
	return fmt.Sprintf("the %s stream had no leader for as long as this could wait, "+
		"a broker election outlasting it (%v)", e.Group, e.Answer)
}

// Unwrap is the context's error alone, so a caller tells a deadline from a
// cancellation as it would for any wait; never the broker's answer, so
// [Leaderless] of a wait that already ended starts no second one.
func (e *NoLeader) Unwrap() error { return e.Err }
