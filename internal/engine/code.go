package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The code host, wired: its limits, the seam the API's routes and the host
// reach it through, its committer's stop, and the census term of its git
// clients.
//
// # Every data node commits
//
// A company that sets `code.native` gets a committer and a lifecycle store on
// every data node ([Engine.startNative]), served to the estate router beside
// the code log's reader ([Engine.estateBackend], [code.Served]); a push is
// asked of its repository's committer — the first node rendezvous ranks for
// it — whichever node received it (internal/estate). There is no committer
// election and no per-repository lease: every node computes the same order,
// and two nodes with different views of the fleet still commit safely, since
// the repository's subject lets one record land per round.

// codeLimits is the code host's limits IN FORCE at the current epoch
// ([Company.codeLimits]) — the defaults where no company is current yet. Read
// per call, by the committer's decide and the reader's repository detail, so a
// revision's limits hold from its activation on, with no restart: the limits
// are an epoch's, never the runtime's.
func (e *Engine) codeLimits() code.Limits {
	c := e.Company()
	switch {
	case c == nil || c.Config == nil:
		return code.DefaultLimits()
	case c.codeLimits != nil:
		return *c.codeLimits
	}
	return code.LimitsOf(c.Config.Code)
}

// carriedCodeLimits is the code host's limits in force at next, installed over
// prev ([Engine.installEpoch]): next's own where it sets `code.native`, and
// otherwise prev's in force.
//
// CARRIED, because the revision that turns the host off cannot hold its limits
// — `code.limits` without `code.native` is refused, since after a restart it
// would read as live while limiting nothing — and a node whose runtime was
// built with the host keeps serving it until that restart
// ([backendSwitches]). Read off that revision, every push the still-running
// committer decided was held to the DEFAULTS: a company that had capped its
// repositories at 2 GiB and its pushes at 64 MiB got 20 GiB and 1 GiB on a
// host it had just asked to wind down, and one that had raised its file limit
// had files refused that were accepted the day before. A revision that hosts
// code again brings its own.
func carriedCodeLimits(next, prev *Company) *code.Limits {
	if next.Config != nil && next.Config.Code.Native {
		limits := code.LimitsOf(next.Config.Code)
		return &limits
	}
	if prev == nil {
		return nil
	}
	return prev.codeLimits
}

// OperatorCode is the code host's estate seam as the API's routes and the host
// reach it — the router's, on every node, for [OperatorWork]'s reason — or
// false where this node's RUNNING runtime was not built with `code.native`: a
// company that does not host code, one that turned it on and has not
// restarted, and a node that runs no native runtime yet. Which halves a node
// runs is [Engine.nativeHalves]' one answer, so the code host is handed out
// exactly where a data node serves it.
func OperatorCode(e *Engine) (estate.CodeSeam, bool) {
	if e.router == nil || !e.nativeHalves().code {
		return estate.CodeSeam{}, false
	}
	return e.router.Code(), true
}

// stopCommitter ends this node's committer: a proposal queued on it is
// answered "not serving", which every asker reads as a node that ran nothing
// and takes to the repository's runner-up with the same push id, and a batch
// already publishing finishes — or, past the bound, is cut and answers its own
// outcome (unknown, naming its operation).
//
// IN THE TEARDOWN BEFORE THE ESTATE SUBJECT STOPS ANSWERING
// ([Engine.stopServingEstate]), because that withdrawal CANCELS the answers in
// flight before it waits for them (queue.Queue.Serve's contract), and a
// proposal still queued here is one of them: its context ended, the committer
// answers it with that cancellation, and the estate carries the failure back as
// the push's answer — final, so its asker reports `context canceled` to the git
// client, for a push its batch may yet have landed, rather than moving on.
// Stopped first, the committer answers every queued proposal "not serving" and
// finishes the batch in flight before anything is cancelled, so every asker
// either has its outcome or takes the push to the runner-up under the same id.
// The order saves no wait: once the subject is withdrawn, an ask finds no
// responder at once and moves on as fast as a refusal. Nil-safe, which is a
// node with no runtime or a company that hosts no code.
func (e *Engine) stopCommitter(ctx context.Context) { e.native.Load().stopCommitter(ctx) }

// stopCommitter is [Engine.stopCommitter] on this runtime, and the stop its
// own failure path takes ([native.shutdown]). [code.Committer.Stop] is
// idempotent, so the teardown's stop and the shutdown's after it are one.
//
// BOUNDED by [statelog.DefaultResolveBudget]: a batch in flight resolves
// within it when the broker answers — it is the bound every write's own wait
// for its applier takes — and when the broker does not, the push ids the batch
// carried collapse on whatever landed when their clients push again.
func (n *native) stopCommitter(ctx context.Context) {
	if n == nil || n.committer == nil {
		return
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), statelog.DefaultResolveBudget)
	defer cancel()
	n.committer.Stop(bounded)
}

// clientTerm is one log's census term of readers that are neither seats nor
// the engine's own passes ([statelog.Census.Clients]), as a function of the
// byte ceiling the log's stream HOLDS and the trim's min_age.
type clientTerm func(holds int64, minAge time.Duration) int

// clientBarriers is the census term of a log's readers that are neither seats
// nor the engine's own passes: the code log's git clients —
// [code.GitReadsPerDay] over the ceiling the log's stream holds, so a resize
// of the log (`crewlet retention set-capacity`) moves the census with it. Nil
// for every other log.
//
// A TERM OF THE LOG, WHATEVER THIS NODE HOSTS. The code log runs, and keeps
// the ceiling the term is a share of, in every register, whether or not the
// company hosts code; and the rate the census is held against is the barriers
// EVERY node applied over the alarm's day ([retention.census]) — the fleet's
// reads, never this node's. Keyed on this node's runtime instead, the term
// judged a fleet's git reads against the seats' term alone in two places: on
// every node restarted after a revision turned `code.native` off, while up to
// a day of git barriers from before the switch were still in the window; and,
// during the rolling restart after a revision turned it on, on every node not
// yet restarted, while its peers served git. A healthy host fired
// census_drift for up to a day either way. On a company that has never hosted
// code nothing appends a git barrier, so the term allows for reads that never
// come.
func clientBarriers(domain string) clientTerm {
	if domain != (code.Domain{}).Name() {
		return nil
	}
	return code.GitReadsPerDay
}
