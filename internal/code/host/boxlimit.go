package host

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// WHAT ONE CODING RUN MAY TAKE OF THIS NODE: a box runs generated code, so
// what its requests cost the node and the code log is bounded by its run —
// never left to the box to keep small.
//
// # Its re-syncs at the log's head
//
// A box reads at its run's SESSION (boxadmit.go, step 8) and pays no barrier,
// but three of its reads go to the log's head: a fetch whose want this node's
// copy lacks (fetch.go), a request past the bound its admitted row set
// (boundOf), and the admission's second resolve of a repository its run was
// given that the session answer would refuse — and a run whose row names no
// floor reads every ref there. Each appends a barrier to the code log. Every
// one draws from the run's bucket on its seat (code.NewResyncs: code.ResyncBurst
// a run and a seat on this node, refilled at the census's seat term) — and a
// fetch's barrier raises the run's floor to the copy it is served from, so its
// next request on any node pays none for the same objects — and past it the
// request is answered AT ITS RUN'S FLOOR: a want the floor's copy lacks
// in upload-pack's own form (`ERR`, which git prints as `fatal: remote error:
// …`), naming the object, the repository and when the run's re-syncs come
// back; a bound read off the admitted row; a refusal the second resolve would
// have judged again, from the session's answer — a name not found there, or
// naming another repository, saying it was not judged again. A box asking in a
// loop for an object nobody pushed thus costs the log sixteen barriers and
// then one every 11.5 minutes, where it cost one a request.
//
// # Its requests in flight
//
// At most [MaxRunRequests] of one run's requests are in flight on this node
// at once; past it a request is answered 503 with Retry-After
// UnavailableRetryAfter, reason run_busy, before its run's row is read. Taken
// when its token is verified and given back when the request's work context
// ends (Server.begin's): at its handler's return, or as soon as its client
// hangs up or the host stops, which end its git too. A box's own burst — a
// partial clone's lazy fetches, a tool's parallel `git fetch`es — thus waits
// on itself: the transfer lane's run class (gitproc.ShareRun) orders who of
// the boxes is granted a slot next, and the spool's one box credential bounds
// what they hold of the spool together, but neither stops ONE run holding a
// node's every admission, run-store read and spooled request at once.

// MaxRunRequests is how many of one coding run's requests may be in flight on
// one node at once.
//
// SIXTEEN — TWICE sandbox.MaxRunRepos (8), the most repositories a run is
// given (held equal by TestARunsRequestsInFlightAreTwiceItsRepositories, since
// this package imports no sandbox): a box fetching each of its repositories
// while it pushes another, or a partial clone's lazy fetch beside its
// checkout's, is two requests a repository, and a run with more than that in
// flight on one node is a loop, not a checkout.
const MaxRunRequests = 16

// boxResync draws one linearizable re-sync of adm's request from its run's
// bucket on its seat, counted by outcome: nil, taken; or the refusal, its
// read to be answered at the run's floor. A person's request is never
// charged: its reads of refs are linearizable by right, its barriers the
// census's clients' term (code.GitReadsPerDay).
func (s *Server) boxResync(adm admission) *costlimit.Refusal {
	scope, principal, ok := adm.resyncKey()
	if !ok {
		return nil
	}
	refused := s.boxResyncs.Take(scope, principal, 1)
	if s.o.Metrics != nil {
		outcome := "taken"
		if refused != nil {
			outcome = "spent"
		}
		s.o.Metrics.Add(metrics.CodeBoxResyncs, 1, metrics.Attrs{"outcome": outcome})
	}
	return refused
}

// notRejudged is the clause a box's answer at its run's floor carries: the
// run's re-syncs, or its seat's, spent until when.
func (s *Server) notRejudged(spent *costlimit.Refusal) string {
	return code.NotRejudged(spent, "this run's", "this seat's", s.now())
}

// wantNotAtFloor answers a box's fetch whose want oid its run's floor does not
// hold, once its run's re-syncs are spent: in upload-pack's own form for a
// want it cannot serve — `ERR <sentence>`, which git prints as `fatal: remote
// error: <sentence>` over protocol v0 and v2 alike — having run no git, and
// logged INFO code_box_resyncs_spent.
func (s *Server) wantNotAtFloor(w http.ResponseWriter, r *http.Request, adm admission, oid string,
	spent *costlimit.Refusal, command string, start time.Time) {

	scope, principal, _ := adm.resyncKey()
	s.log.Info("code_box_resyncs_spent", "repo", adm.name, "path", r.URL.Path, "scope", scope,
		"principal", principal, "oid", oid, "retry_after", spent.RetryAfter)
	out := newSink(w, "application/x-git-upload-pack-result", s.now)
	defer out.End()
	w.Header().Set("Cache-Control", "no-cache")
	_, err := out.Write(pktString("ERR " + sentence(fmt.Sprintf("%s is not in %s at the position this run has "+
		"been shown; it has not been pushed yet, or ask code_resolve %s", oid, adm.name, s.notRejudged(spent))) + "\n"))
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
		// A RUN WITH NOTHING IN FLIGHT LEAVES NO ENTRY: the map holds the
		// runs with requests here now, never every run this node served.
		if s.runRequests[run]--; s.runRequests[run] == 0 {
			delete(s.runRequests, run)
		}
	})
	return true
}

// runBusySentence is what a box past its requests in flight is told.
var runBusySentence = fmt.Sprintf("this run has %d requests in flight on this node", MaxRunRequests)
