// Package jsinflight is how a CONDITIONAL publish to a replicated JetStream
// stream is answered while the leader still has another write to the same
// subject in flight — and why that answer is waited out rather than believed.
//
// # A conditional publish has three answers, not two
//
// A publish that names the revision it expects the subject to be at — a
// compare-and-set — lands; or the leader compares that revision with the one
// the subject is at and refuses it, naming the latter (10071, "wrong last
// sequence: N"); or the leader refuses it WITHOUT comparing, because another
// write to that subject is still in flight (10164, "wrong last sequence" with
// no number). The last one decides nothing.
//
// The leader marks a subject in flight when it proposes a conditional write to
// it, and clears the mark only after it has applied that write AND sent its
// acknowledgement (nats-server jetstream_batching.go, the
// expectedPerSubjectInProcess and inflight checks; jetstream_cluster.go
// applyStreamMsgOp, which clears both after processJetStreamMsg has acked). So
// 10164 is answered in two cases with opposite outcomes: a concurrent write
// that will land and win, and a write that has ALREADY landed — very often the
// caller's own, a moment ago — whose mark has not yet been cleared. A solo
// stream has no proposal and never gives the answer.
//
// The client maps 10164 and 10071 to one revision mismatch, its own doc calls
// them equivalent, and both of this engine's conditional writers believed it.
// Measured on three members under load: a removal at the very revision its
// caller had just created and read back was refused, so a create race over
// that record had no winner; a counter charge spent all sixteen of its
// compare-and-set rounds on refusals no other writer had caused and reported
// the store unavailable.
//
// # Why one package
//
// Two writers publish conditionally to replicated streams — [internal/coord/kv]
// to the coordination buckets and [internal/queue/jetstream] to the state
// logs — and the rule is a fact about the server, not about either. Written
// twice it would drift the way [internal/jsprovision]'s rule did before it was
// a package. It imports the JetStream client and the engine's backoff leaf and
// nothing else, so either side can take it without taking the other.
package jsinflight

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backoff"
)

// How long a conditional publish waits for the leader to finish the write
// ahead of it.
//
// FirstWait is a millisecond, under the one raft commit an idle loopback
// cluster takes (about 1.5 ms), because the commonest case is no commit at
// all: the write ahead was acknowledged already and only the leader's own
// bookkeeping is behind, which clears in microseconds. Each wait doubles to
// MaxWait, a few commits on a cluster under load — long enough that a leader
// working through a backlog is not asked sixteen times a commit, short enough
// that a write that has landed is retried within a fraction of the time it
// took. Jitter spreads the racers one write refused, so they do not all return
// on the same instant.
//
// The whole wait is held to the publish's own budget ([Sends]) — the caller's
// deadline, or where it set none what the publish alone would have waited for
// its acknowledgement: the request term and, across a link, its bytes'
// crossing for each of the sends a write may make (internal/jsapi's Budget
// and WriteSends), since an attempt after an in-flight answer sends the whole
// message again — because waiting for the write ahead to settle is waiting for
// THAT write's acknowledgement, and no publish here is promised more patience
// than its own. That budget's end is fixed when the publish starts: an
// attempt past its sends, and a wait between two attempts, moves it nowhere,
// so a publish met by an in-flight answer on every attempt ends [Undecided]
// at it however fast each answer comes back.
const (
	FirstWait = time.Millisecond
	MaxWait   = 64 * time.Millisecond
	Jitter    = 0.2
)

// InFlight reports the leader's in-flight answer: a conditional publish it
// declined to compare, because another write to the subject was still in
// flight. See the package doc for why that decides nothing.
//
// NAMED FOR WHAT IT IS AND NOT FOR THE WORDS THE SERVER USES. It was called
// Refused, which is the one thing this answer is not — the decided refusal
// is the other code — and a caller reaching for "was my write refused?" would
// have found this predicate first and matched the answer that decides
// nothing.
func InFlight(err error) bool {
	var api *jetstream.APIError
	return errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequenceConstant
}

// Undecided is a conditional publish the leader was still refusing as in
// flight when the publish's budget ran out: neither applied nor refused.
//
// IT DOES NOT WRAP THE REFUSAL, and that is the point. The client wraps 10164
// in its revision-mismatch sentinel, so an error carrying it would be read by
// every compare-and-set caller as a race somebody else won — and nothing
// about an undecided publish says anybody did. It wraps the context error that
// ended the wait instead, which is the honest third value.
type Undecided struct {
	// Last is the leader's last refusal, kept for the message only.
	Last error
	// Cause is what ended the wait: the publish's deadline, or its
	// caller going away.
	Cause error
}

func (u *Undecided) Error() string {
	return fmt.Sprintf("the leader still had another write to this subject in flight "+
		"when the publish's budget ran out, so it was neither applied nor refused "+
		"(%v): %v", u.Last, u.Cause)
}

// Unwrap is the context error, never the refusal.
func (u *Undecided) Unwrap() error { return u.Cause }

// Sends is the budget a decided publish waits within across every attempt of
// it, as the wait between two attempts needs it: the one internal/jsapi's
// Budget starts, whose every send — each attempt's, made by the publish
// itself through it — is given the time its bytes take to cross a link, an
// attempt after an in-flight answer carrying the whole message again.
type Sends interface {
	// Pause waits d inside ctx and the budget as it stands, answering why
	// it could not: the budget's end, or its caller going away.
	Pause(ctx context.Context, d time.Duration) error
}

// Decide runs one conditional publish until the leader DECIDES it: it lands,
// or it is refused on a comparison. An in-flight refusal is retried — the
// identical publish, which the leader stored nothing of — after a doubling,
// jittered wait, within the publish's budget, sends. A publish still refused
// as in flight when that runs out is [*Undecided].
//
// publish is handed ctx, its caller's, and makes each attempt's send within
// sends (internal/jsapi's Sends.Next or Sends.Whole), so no send outlasts the
// budget and each of the sends a write may make is given its own crossing; the
// wait between two attempts is held to the budget as it stands, whose end the
// attempts never move past the bound it started with.
func Decide(ctx context.Context, sends Sends, publish func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		err := publish(ctx)
		if !InFlight(err) {
			return err
		}
		if cause := sends.Pause(ctx, backoff.Jitter(backoff.Doubling(attempt, FirstWait, MaxWait), Jitter)); cause != nil {
			return &Undecided{Last: err, Cause: cause}
		}
	}
}
