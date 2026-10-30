package code

import (
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SEAT'S RE-SYNCS AT THE LOG'S HEAD ARE BOUNDED: every linearizable re-sync
// a seat's work takes on a git node — a coding box's (internal/code/host's box
// routes) and a turn's code tools' (the git reads of internal/code/query and
// an engine write's re-judgement in internal/code/host) — appends a barrier
// to the code log, and the code log's census gives a seat
// statelog.LinearizableReadsPerSeatDay of them. Without a bound those
// re-syncs were the one barrier a seat could append at will: a box asking for
// one object its run's floor does not hold, in a loop, or a tool naming one,
// took one barrier a request, outside the census the log's ceiling, its trim
// cadence and a rejoining node's replay were sized by — until census_drift
// fired, and past it the log filled and refused every push of the company.
//
// THE BUCKETS ARE internal/costlimit's, the engine's ONE limiter (ADR-0008):
// one Limiter a surface a node — a node's boxes, and its seats' tools — each
// charged 1 a re-sync, the principal the run (a box's launch) or the turn (a
// tool's), the scope its seat, every bucket [ResyncBurst] refilled at the
// census's seat term. ONE live run or turn a seat is bounded exactly to that
// term; a seat with several live on one node shares its seat's bucket there,
// which is the term the census gave the seat. Per node, in memory, as every
// costlimit bucket is: a box whose requests spread over N git nodes gets N
// bursts, which bounds the log's barriers by the fleet's git nodes — the
// census's own reading of a seat, multiplied by a count the operator chose.
//
// PAST THE BUCKET THE REQUEST IS ANSWERED AT ITS FLOOR — what its run or its
// turn has been shown, which holds everything it pushed and every ref it read
// — and a refusal its re-sync would have judged again at the log's head is
// answered from there, SAYING SO ([NotRejudged]) and when the bucket holds a
// re-sync again. That is the one departure from the host's rule that no
// refusal is decided on a stale read, and it is taken only where the asker
// has spent its share of the log: an object or a grant the head would have
// given is one the next re-sync, past the refill, finds.

// ResyncBurst is how many linearizable re-syncs a seat's run or turn may take
// at once on one node, and its seat on that node, before the census's refill
// paces them.
//
// SIXTEEN, twice MaxRunRepos' eight repositories: a coding run given its
// eight repositories may meet each one's floor short twice — a fetch whose
// wants a lagging cache lacks, and a request past the bound its admitted row
// set — before any of it waits on the refill; and a turn's tools meet at most
// one re-sync a call (the git node's one second look, the write's one
// re-judgement), so sixteen is sixteen calls in a burst each naming what its
// copy lacks, which no turn that reads what it was shown comes near. The
// refill is the census's, statelog.LinearizableReadsPerSeatDay a day (125: one
// re-sync every 11.5 minutes), BORROWED rather than copied, so a census
// re-derived moves the bucket with it.
const ResyncBurst = 16

// resyncsPerSecond is the census's seat term as a bucket's rate.
var resyncsPerSecond = float64(statelog.LinearizableReadsPerSeatDay) / (24 * time.Hour).Seconds()

// NewResyncs is one node's re-sync buckets for one surface — its boxes', or
// its seats' tools' — on a clock (nil is the wall clock): charged
// Take(seat, run or turn, 1) a linearizable re-sync, every bucket
// [ResyncBurst] refilled at the census's seat term.
func NewResyncs(now func() time.Time) *costlimit.Limiter {
	return costlimit.New(costlimit.Rates{PrincipalRate: resyncsPerSecond, PrincipalBurst: ResyncBurst,
		ScopeRate: resyncsPerSecond, ScopeBurst: ResyncBurst}, now)
}

// Resyncs is whose re-syncs a CHECK'S BOX draws on — its git's at the code
// host and its reads at /box/code/ at the git node that answers them — as a
// [NewResyncs] bucket keys them: the attempt k, on its repository's checks
// (`check:<repository id>`, the key its transfers share). A check's box runs
// its repository's checks file, code nobody vouches for as much as a seat's
// box, and every read it makes is at its check's session, which holds its one
// commit — so a check that meets its floor short is a file asking for what
// nobody pushed, and one keyed by its repository holds the dozen checks a
// push fans out to the one term a coding run gets: nothing a seat's census
// counts, and nothing a person's clients' term was sized for.
func (k CheckKey) Resyncs() (scope, principal string) {
	return "check:" + k.RepoID, k.OID + " " + k.Target + " " + k.Name + " " + strconv.Itoa(k.Attempt)
}

// NotRejudged is the clause a refusal answered at its floor carries once its
// re-sync's bucket is spent: whose bucket refused — principal names the
// principal's ("this run's", "this turn's"), scope the scope's ("this
// seat's") — and the instant it holds a re-sync again, which is the refusal's
// Retry-After from now, to the second above it.
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
