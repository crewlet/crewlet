package gitlet

import (
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A SEAT'S RE-SYNCS AT THE LOG'S HEAD ARE BOUNDED: each one a seat's work takes
// on a Gitlet node — a coding box's (internal/gitlet/host's box routes), a turn's
// code tools' (internal/gitlet/query's git reads, an engine write's re-judgement
// in internal/gitlet/host) — appends a barrier to the code log, whose census
// gives a seat statelog.LinearizableReadsPerSeatDay of them. Unbounded, a box or
// tool asking in a loop for an object its floor lacks appends barriers outside
// the census the log's ceiling, trim and replay are sized by, until, past
// census_drift, the log fills and refuses every push. Past the bucket a request is
// answered at its floor (which holds everything its run or turn pushed or read),
// SAYING SO ([NotRejudged]): the one refusal decided on a stale read, taken only
// once the asker has spent its share, and what the head would have given the
// next re-sync past the refill finds.

// ResyncBurst is how many linearizable re-syncs a seat's run or turn, and its
// seat, may take at once on one node before the census's refill paces them.
//
// TWICE MaxRunRepos: an executor given its most repositories may meet each
// one's floor short twice (a fetch a lagging cache lacks, a request past its
// admitted row set) before waiting on the refill; a turn's tools take at most
// one re-sync a call. DERIVED, because a burst left behind a raised run bound
// spends the seat's burst part way through a run's repositories and later
// fetches fail at the box. MaxRunRepos is held to at least sandbox.MaxRunRepos,
// and internal/gitlet/host's TestARunsResyncBurstIsTwiceItsRepositories holds
// the burst to it.
const ResyncBurst = 2 * MaxRunRepos

// resyncsPerSecond is the census's seat term as a bucket's rate (125 a day: one
// every 11.5 minutes), BORROWED rather than copied so a re-derived census moves it.
var resyncsPerSecond = float64(statelog.LinearizableReadsPerSeatDay) / (24 * time.Hour).Seconds()

// Resyncs is ONE Gitlet node's re-sync buckets ([NewResyncs]), on
// internal/costlimit, the engine's one limiter (ADR-0008). Every draw —
// Gitlet's box routes and engine writes, the query server's reads, a check's
// box — pays 1 to its principal (run, turn or check attempt) and its scope (the
// seat, or a check's repository). ONE set, because the census gives a seat ONE
// term on a log however its work reaches the head: separate box and tool
// buckets would put twice it on the log. Per node and in memory, so a seat
// spread over N Gitlet nodes gets N bursts, a count the operator chose.
type Resyncs struct {
	limiter *costlimit.Limiter
	metrics *metrics.Recorder
}

// NewResyncs is one Gitlet node's re-sync buckets, on a clock (nil is the wall
// clock), every bucket [ResyncBurst] refilled at the census's seat term — a
// box's draws counted to m (crewlet.code.box.resyncs; nil records nothing).
// The engine builds ONE a node and hands it to Gitlet's host and the query
// server both.
func NewResyncs(now func() time.Time, m *metrics.Recorder) *Resyncs {
	return &Resyncs{limiter: costlimit.New(costlimit.Rates{PrincipalRate: resyncsPerSecond,
		PrincipalBurst: ResyncBurst, ScopeRate: resyncsPerSecond, ScopeBurst: ResyncBurst}, now), metrics: m}
}

// THE PRINCIPALS ARE SPELLED BY KIND within their seat's scope: launch and turn
// ids are both uuids, and one spelling would make "a run's bucket is its own" a
// property of how ids are minted rather than of the key.
const (
	runPrincipal  = "run "
	turnPrincipal = "turn "
)

// RunResyncs is whose re-syncs a CODING BOX's request draws on, as [Resyncs]
// keys them: its run (launch) on its seat.
func RunResyncs(seat, launch string) (scope, principal string) {
	return seat, runPrincipal + launch
}

// Turn draws one re-sync of a SEAT'S TOOL (a read's second look at the log's
// head, an engine write's re-judgement there) on its turn and its seat, the
// seat's bucket its boxes on this node share. Nil where taken; past the
// buckets, the refusal its answer at the seat's floor is given by.
func (r *Resyncs) Turn(seat, turn string) *costlimit.Refusal {
	return r.limiter.Take(seat, turnPrincipal+turn, 1)
}

// Box draws one re-sync of a BOX's request — an executor's ([RunResyncs]) or a
// check's ([CheckKey.Resyncs]), at Gitlet or at /box/gitlet/, the same bucket
// either way — counted crewlet.code.box.resyncs by outcome: `taken`, a barrier
// on the code log; `spent`, answered at its floor, the refusal returned.
func (r *Resyncs) Box(scope, principal string) *costlimit.Refusal {
	refused := r.limiter.Take(scope, principal, 1)
	if r.metrics != nil {
		outcome := "taken"
		if refused != nil {
			outcome = "spent"
		}
		r.metrics.Add(metrics.CodeBoxResyncs, 1, metrics.Attrs{"outcome": outcome})
	}
	return refused
}

// Resyncs is whose re-syncs a CHECK'S BOX draws on (its transfers at Gitlet,
// its reads at /box/gitlet/): the attempt k, on `check:<repository id>`, the
// scope its transfers share. Keyed by repository, not seat (no seat's census
// counts a check): a check runs its repository's checks file, code nobody
// vouches for, at a session holding its one commit, so a short floor is a file
// asking for what nobody pushed, and the dozen checks a push fans out to share
// the one term an executor gets.
func (k CheckKey) Resyncs() (scope, principal string) {
	return "check:" + k.RepoID, k.OID + " " + k.Target + " " + k.Name + " " + strconv.Itoa(k.Attempt)
}

// NotRejudged is the clause a refusal answered at its floor carries once its
// re-sync's bucket is spent: whose bucket refused (principal's, e.g. "this
// run's", or scope's, "this seat's") and when it holds a re-sync again
// ([ResyncsBackAt]).
func NotRejudged(r *costlimit.Refusal, principal, scope string, now time.Time) string {
	whose := principal
	if r.Scope == costlimit.ScopeShared {
		whose = scope
	}
	return fmt.Sprintf("(not re-judged at linearizable: %s re-syncs are spent until %s)", whose,
		ResyncsBackAt(r, now).Format(time.RFC3339))
}

// ResyncsBackAt is when a refused re-sync's bucket holds one again: now plus
// its Retry-After, rounded up to the second the sentence names, so the
// instant a sentence says is never one the bucket still refuses.
func ResyncsBackAt(r *costlimit.Refusal, now time.Time) time.Time {
	at := now.Add(r.RetryAfter).UTC()
	if t := at.Truncate(time.Second); t.Before(at) {
		return t.Add(time.Second)
	}
	return at
}
