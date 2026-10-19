package kv

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
)

// How a lost compare-and-swap is retried in the fleet store.
//
// # A lost race is not an outage, so it is bounded by time
//
// A compare-and-swap that loses proves two things: the store answered, and
// another writer changed the record between this one's read and its write. So
// a count of lost races measures how many peers wrote rather than whether the
// store works, and a bound on attempts reports a healthy, busy record as an
// outage once enough writers overlap on it. So a race lost in the fleet store
// is retried after a jittered wait ([raceRetryDelay]) for as long as the
// store's PATIENCE allows ([FleetStore.patience]), never for a number of
// attempts: through [FleetStore.retryRaces], and through the token counter's
// own loop, which holds each write in a batch to its own patience
// (counter.go) — unless what the loop races ends it of itself (below).
//
// What this replaced was sixteen immediate attempts for every record, and
// three records have writers nothing bounds. The token counter is the hottest
// key the fleet has, and counter.go measures what sixteen cost there. A
// detached run's record ([FleetStore.MutateSandboxRun], sandboxruns.go) takes
// every tool call its box makes over the bridge — from as many as twenty
// subagents at once — beside its seat's own writes, a person's answer, a note
// and a stop. And a person's chat read record is written by every tab and
// device they read in and by the reconcile, each flushing again as soon as it
// has landed ([FleetStore.mutateChatRead]), so sixteen held sixteen writers and
// a seventeenth tab failed the flush. The rest race writers something does
// bound — the two ends of a channel, the messages that assert one follow, a
// create that finds the key removed between its refusal and its read — and
// lose for a whole patience only on a store this engine does not produce; on
// those the rule costs nothing.
//
// The patience is the JetStream client's own request timeout. Every request
// this package makes on a context with no deadline already waits that long for
// a store that does not answer at all, so a record that answers every attempt
// and still has not taken this write inside it is, to the caller, exactly as
// unavailable, and is answered the same way: [coord.ErrUnavailable], naming the
// contention ([outwaited]), never an answer the store did not give. Only a
// record written faster than it can serve one key gets that answer from
// contention, and no retry policy turns that store into one that keeps up.
//
// IT DECIDES ONLY WHETHER TO START AN ATTEMPT. An attempt already sent is cut
// short by nothing but its caller's own context, so its outcome is the answer:
// a deadline over the whole race would cut the request in flight when it fell,
// and a write the broker applied but had not yet acknowledged would be answered
// as one that never landed.
//
// # The loops that do not take it
//
// Five retry loops in this package are bounded otherwise, each because what it
// races ends the loop of itself. Four keep a count that is exact. The rate
// valve's every lost race is another caller's increment, so it reads a full
// window within its own limit of them ([FleetStore.Allow]). A lease's races the
// nodes contending one seat, a head count the placement model bounds
// ([casAttempts]). A delivery claim loses a round only to a caller that took
// the lapsed claim first or released it, and its next read is the answer
// either way ([claimAttempts]). A create loses past its third round only to
// another writer removing the key again inside its own read-to-write window,
// which nothing in this estate does repeatedly ([createRounds]). And the
// activation pointer's unconditional write runs until its caller's context
// ends ([FleetStore.assert]): its one caller is `crewlet run -import-company`,
// an operator replacing the company at one node's start, and each round it
// loses is another activation landing — an API write or a peer's boot
// publish, each of which lands once. A compare-and-swap whose loss IS its
// answer — a conditional activation, a seat's pause — is not retried at all.

// retryRaces makes attempt until it does not LOSE A RACE — another writer
// changed the record between its read and its write — waiting [raceRetryDelay]
// after each loss. It starts no attempt once the store's patience has run out
// since the first, and answers [outwaited] for op on resource instead;
// otherwise it answers attempt's own error. Only ctx, the caller's, cuts an
// attempt in flight short, and a ctx that ends between attempts is answered as
// the store unavailable for it: every attempt before lost, so nothing of the
// write has landed.
func (f *FleetStore) retryRaces(ctx context.Context, op, resource string,
	attempt func() (lost bool, err error)) error {

	began := time.Now()
	for n := 0; ; n++ {
		if n > 0 {
			if !awaitRace(ctx, n) {
				return unavailable(op, ctx.Err())
			}
			if time.Since(began) >= f.patience {
				return outwaited(op, resource, f.patience)
			}
		}
		lost, err := attempt()
		if !lost {
			return err
		}
	}
}

// awaitRace waits [raceRetryDelay] before the attempt'th retry of a lost race,
// reporting false when ctx ends first.
func awaitRace(ctx context.Context, attempt int) bool {
	wait := time.NewTimer(raceRetryDelay(attempt))
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wait.C:
		return true
	}
}

// The retry backoff between two attempts that lost a race: a doubling wait
// from raceRetryBase to raceRetryCeiling, jittered across most of itself.
//
// A MILLISECOND TO START, because that is a compare-and-swap's own round trip
// on a clustered broker — a shorter wait is still inside the write that beat
// this one — and doubling to a CEILING OF 32 ms, which every wait after the
// sixth stays at until the race ends: small beside the model call a round is
// about to make, and what it buys is that the writers that lost one race do
// not arrive together at the next. The JITTER is the widest [backoff.Jitter]
// gives, because a record's writers lost the same race at the same instant and
// only the spread separates them.
const (
	raceRetryBase    = time.Millisecond
	raceRetryCeiling = 32 * time.Millisecond
	raceRetryJitter  = 0.9
)

// raceRetryDelay is the wait before the attempt'th retry of a lost race.
func raceRetryDelay(attempt int) time.Duration {
	return backoff.Jitter(backoff.Doubling(attempt, raceRetryBase, raceRetryCeiling), raceRetryJitter)
}

// outwaited reports a write that raced for the whole of its patience without
// landing.
//
// UNKNOWN, not a refusal, for [contended]'s reason; and named as contention
// rather than as a store that did not answer, because the store did answer,
// every time. And a DEFINITE "did not land": a write is answered this only
// between attempts, where every attempt it made had completed and lost.
func outwaited(op, resource string, patience time.Duration) error {
	return fmt.Errorf("%w: %s(%s) did not land within %v: another writer changed the record "+
		"first at every attempt", coord.ErrUnavailable, op, resource, patience)
}
