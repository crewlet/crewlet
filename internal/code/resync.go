package code

import (
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
// THE BUCKETS ARE internal/costlimit's, the engine's ONE limiter (ADR-0008),
// and a git node keeps ONE set of them ([Resyncs]) for every re-sync it
// bounds — its coding boxes', its checks' boxes' and its seats' tools' alike —
// each charged 1 a re-sync, the principal the run (a box's launch), the
// attempt (a check's) or the turn (a tool's), the scope its seat (a check's,
// its repository's checks), every bucket [ResyncBurst] refilled at the
// census's seat term. ONE SET, because the census gives a seat ONE term on a
// log however its work reaches the log's head: a box's barriers and its
// turns' tools' each drawing on buckets of their own put twice the seat's
// term on the log from every git node, and a check's git and its /box/code/
// reads were two bursts of one attempt. ONE live run or turn a seat is
// bounded exactly to that term; a seat with several live on one node — runs
// and turns together — shares its seat's bucket there, which is the term the
// census gave the seat. Per node, in memory, as every costlimit bucket is: a
// seat whose requests spread over N git nodes gets N bursts, which bounds the
// log's barriers by the fleet's git nodes — the census's own reading of a
// seat, multiplied by a count the operator chose.
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
// TWICE MaxRunRepos, sixteen at its eight: a coding run given its most
// repositories may meet each one's floor short twice — a fetch whose wants a
// lagging cache lacks, and a request past the bound its admitted row set —
// before any of it waits on the refill; and a turn's tools meet at most one
// re-sync a call (the git node's one second look, the write's one
// re-judgement), so the burst is as many calls each naming what its copy
// lacks, which no turn that reads what it was shown comes near. DERIVED rather
// than written as a figure, because the run bound is what sizes it: a burst
// left behind a raised bound has a run given its most repositories spend its
// seat's burst on the node part way through them, and every later re-sync is
// answered at its stale floor, so a fetch a lagging cache cannot serve fails
// at the box instead of re-syncing. MaxRunRepos is held to at least
// sandbox.MaxRunRepos, the bound a run's repositories are granted under
// (internal/engine), so raising that bound raises this burst with it, and
// internal/code/host's TestARunsResyncBurstIsTwiceItsRepositories holds the
// burst to it directly. The refill is the census's,
// statelog.LinearizableReadsPerSeatDay a day (125: one re-sync every 11.5
// minutes), BORROWED rather than copied, so a census re-derived moves the
// bucket with it.
const ResyncBurst = 2 * MaxRunRepos

// resyncsPerSecond is the census's seat term as a bucket's rate.
var resyncsPerSecond = float64(statelog.LinearizableReadsPerSeatDay) / (24 * time.Hour).Seconds()

// Resyncs is ONE git node's re-sync buckets ([NewResyncs]): every draw on it,
// whichever server takes it — the code host's box routes and engine writes,
// the query server's reads — pays the one bucket of its principal on its
// scope and the one of its scope, so a seat's term on this node is one term
// however its work reaches the log's head.
type Resyncs struct {
	limiter *costlimit.Limiter
	metrics *metrics.Recorder
}

// NewResyncs is one git node's re-sync buckets, on a clock (nil is the wall
// clock), every bucket [ResyncBurst] refilled at the census's seat term — a
// box's draws counted to m (crewlet.code.box.resyncs; nil records nothing).
// The engine builds ONE a git node and hands it to the code host and the
// query server both.
func NewResyncs(now func() time.Time, m *metrics.Recorder) *Resyncs {
	return &Resyncs{limiter: costlimit.New(costlimit.Rates{PrincipalRate: resyncsPerSecond,
		PrincipalBurst: ResyncBurst, ScopeRate: resyncsPerSecond, ScopeBurst: ResyncBurst}, now), metrics: m}
}

// THE PRINCIPALS ARE SPELLED BY KIND within their seat's scope: a coding
// run's launch id and a turn's id are each a uuid nobody chose, but one
// spelling for both would make "a run's bucket is its own" a property of how
// ids happen to be minted rather than of the key.
const (
	runPrincipal  = "run "
	turnPrincipal = "turn "
)

// RunResyncs is whose re-syncs a CODING BOX's request draws on, as [Resyncs]
// keys them: its run (launch) on its seat.
func RunResyncs(seat, launch string) (scope, principal string) {
	return seat, runPrincipal + launch
}

// Turn draws one re-sync of a SEAT'S TOOL — a read's one second look at the
// log's head, an engine write's one re-judgement there: its turn on its seat,
// the seat's bucket its boxes on this node draw on too. Nil where it is
// taken; past the buckets, the refusal its answer is given at the seat's
// floor by.
func (r *Resyncs) Turn(seat, turn string) *costlimit.Refusal {
	return r.limiter.Take(seat, turnPrincipal+turn, 1)
}

// Box draws one re-sync of a BOX's request — a coding run's ([RunResyncs]) or
// a check's ([CheckKey.Resyncs]), its transfer's at the code host or its read
// at /box/code/ at the node that answers it, the same bucket either way —
// counted crewlet.code.box.resyncs by outcome: `taken`, a barrier on the code
// log; `spent`, answered at its floor instead, the refusal returned.
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

// Resyncs is whose re-syncs a CHECK'S BOX draws on — its transfers' at the
// code host and its reads at /box/code/ at the node that answers them, ONE
// bucket of that node's [Resyncs] — as its keys: the attempt k, on its
// repository's checks
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
