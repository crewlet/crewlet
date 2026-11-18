package host

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A box runs generated code, so what its requests cost this node and the code
// log is bounded by its run, never left to the box.

// A box reads at its run's SESSION (boxadmit.go) and pays no barrier, but
// three reads go to the log's head: a fetch whose want this node's copy lacks
// (fetch.go), a request past its admitted row's bound (boundOf), and the
// admission's second resolve of a repository the session answer would refuse;
// a run whose row names no floor reads every ref there. Each draws on the
// run's bucket on its seat in the node's ONE Options.Resyncs (gitlet.NewResyncs:
// gitlet.ResyncBurst, refilled at the census's seat term), which the seat's code
// tools on this node share, so box and tools hold the seat to one term. A
// fetch's barrier raises the run's floor to the copy it is served from, so
// its next request on any node pays none for those objects.

// Past its re-syncs a request is answered AT ITS RUN'S FLOOR: a missing want as
// upload-pack's `ERR` (wantNotAtFloor), naming the object, the repository and
// when the re-syncs come back; a bound read off the admitted row; a refusal the
// second resolve would have judged, from the session's answer, saying it was
// not judged again. A box looping on an object nobody pushed thus costs the log
// sixteen barriers, then one every 11.5 minutes.

// At most MaxRunRequests of one run's requests are in flight on this node;
// past it a request is answered 503 (Retry-After UnavailableRetryAfter, reason
// run_busy) before its run's row is read. A place is taken when the token is
// verified and given back when the request's work context ends (Server.begin):
// at the handler's return, or when the client hangs up or the host stops,
// which end its git too. So a box's own burst waits on itself: the transfer
// lane's run class (gitproc.ShareRun) and the spool's one box credential order
// and bound the boxes together, but neither stops ONE run holding a node's
// every admission, run-store read and spooled request.

// A check's box runs its repository's checks file, code nobody vouches for, and
// is bounded the same way: its re-syncs (the admission's second read of an
// attempt the stale copy lacks among them) draw on its ATTEMPT's bucket on its
// REPOSITORY's checks (gitlet.CheckKey.Resyncs), same burst and refill, and past
// them are answered at its session; at most MaxRunRequests of its attempt's
// requests are in flight on a node. Its /box/gitlet/ reads draw on the same
// node's Resyncs (internal/gitlet/query), so the attempt has ONE burst a node,
// not one a server; keyed by repository, the checks one push fans out share one
// term.

// MaxRunRequests is how many of one executor's requests may be in flight on
// one node at once: twice sandbox.MaxRunRepos (8), since fetching each
// repository while pushing another, or a lazy fetch beside a checkout's, is two
// requests a repository; more is a loop, not a checkout.
// TestARunsRequestsInFlightAreTwiceItsRepositories holds the two equal, since
// this package imports no sandbox.
const MaxRunRequests = 16

// boxResync draws one linearizable re-sync of adm's request from
// Options.Resyncs, keyed by admission.resyncKey: nil, taken; or the refusal,
// the read then answered at the box's floor. A person's request is never
// charged: its ref reads are linearizable by right, its barriers the census's
// clients' term (gitlet.GitReadsPerDay).
func (s *Server) boxResync(adm admission) *costlimit.Refusal {
	scope, principal, ok := adm.resyncKey()
	if !ok {
		return nil
	}
	return s.o.Resyncs.Box(scope, principal)
}

// notRejudged is the clause a box's answer at its floor carries: its run's
// re-syncs or its seat's — a check's, or its repository's checks' — spent
// until when.
func (s *Server) notRejudged(adm admission, spent *costlimit.Refusal) string {
	_, _, principal, scope := adm.floorWords()
	return gitlet.NotRejudged(spent, principal, scope, s.now())
}

// wantNotAtFloor answers a box's fetch whose want oid its floor lacks, once its
// re-syncs are spent, with upload-pack's own `ERR <sentence>` (git prints
// `fatal: remote error: <sentence>` over v0 and v2 alike), running no git;
// logged INFO code_box_resyncs_spent.
func (s *Server) wantNotAtFloor(w http.ResponseWriter, r *http.Request, adm admission, oid string,
	spent *costlimit.Refusal, command string, start time.Time) {

	scope, principal, _ := adm.resyncKey()
	s.log.Info("code_box_resyncs_spent", "repo", adm.name, "path", r.URL.Path, "scope", scope,
		"principal", principal, "oid", oid, "retry_after", spent.RetryAfter)
	out := newSink(w, "application/x-git-upload-pack-result", s.now)
	defer out.End()
	w.Header().Set("Cache-Control", "no-cache")
	shown, instead, _, _ := adm.floorWords()
	_, err := out.Write(pktString("ERR " + sentence(fmt.Sprintf("%s is not in %s at the position %s; %s %s", oid,
		adm.name, shown, instead, s.notRejudged(adm, spent))) + "\n"))
	s.finishFetch(w, r, adm.name, command, cache.SyncNone, start, out, err)
}

// holdRun takes one of run's MaxRunRequests places in flight on this node for
// the request whose work context ctx is, given back when ctx ends; false,
// with nothing taken, where the run holds all of them — counted
// (crewlet.code.box.inflight_refused).
func (s *Server) holdRun(ctx context.Context, run string) bool {
	s.runsMu.Lock()
	if s.runRequests[run] >= MaxRunRequests {
		s.runsMu.Unlock()
		if s.o.Metrics != nil {
			s.o.Metrics.Add(metrics.CodeBoxInflightRefused, 1, nil)
		}
		return false
	}
	if s.runRequests == nil {
		s.runRequests = map[string]int{}
	}
	s.runRequests[run]++
	s.runsMu.Unlock()
	context.AfterFunc(ctx, func() {
		s.runsMu.Lock()
		defer s.runsMu.Unlock()
		// Drop an idle run's entry, or the map grows with every run served.
		if s.runRequests[run]--; s.runRequests[run] == 0 {
			delete(s.runRequests, run)
		}
	})
	return true
}

// How an answer at a check's box's floor names whose re-syncs ran out: its
// attempt's, or its repository's checks' (gitlet.CheckKey.Resyncs).
const (
	checkResyncs  = "this check's"
	checksResyncs = "its repository's checks'"
)

// checkHold is the key a check's box's requests in flight are counted under:
// its attempt on its repository's checks, which no executor's launch id
// spells.
func checkHold(k gitlet.CheckKey) string {
	scope, principal := k.Resyncs()
	return scope + " " + principal
}

// busySentence is what a box past its requests in flight is told: who — "this
// run", "this check" — holds them.
func busySentence(who string) string {
	return fmt.Sprintf("%s has %d requests in flight on this node", who, MaxRunRequests)
}
