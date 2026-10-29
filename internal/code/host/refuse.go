package host

import (
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// ERRORS ON THE WIRE, before a git process has written a byte: every one a
// status and a GitText sentence, every 503 a Retry-After that its sentence
// says again in words — git does not read the header — and a failure no retry
// ends a 500 that promises none.

// syncFailed answers a request whose cache could not be synced, or whose git
// process failed before writing anything: what each failure means to the
// person, and what to do. attrs go on the line a git process's failure is
// logged with (a push's push_id).
//
// EVERY ANSWER IS IDENTIFIED BY WHAT FAILED, NEVER BY ELIMINATION. The code
// log's sentence — "the code log could not be read here", the node cut off
// from the fleet — is for the code log's own failure alone, marked where it
// is met (cache.ErrLogRead, errLogRead); the object store's failure is its
// own (cache.ErrStoreRead); and what nothing names — this node's disk, a
// process that could not start, a failure no case foresaw — is said to be
// this node's (nodeFailed), with what failed in the sentence. As the last
// case's answer the code log's sentence sent operators to the fleet's
// quorum for a full disk, an object store throttle and a refusal in this
// build alike.
func (s *Server) syncFailed(w http.ResponseWriter, r *http.Request, name string, err error, attrs ...any) {
	if s.ended(w, r, name, err) {
		return
	}
	var (
		missing  *cache.ErrPackMissing
		corrupt  *cache.ErrCorruptPack
		exit     *gitproc.ExitError
		unraised *floorUnraised
	)
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name))
	case errors.As(err, &unraised):
		// A BOX'S RUN STORE, NEVER THE CODE LOG: the answer it was about to
		// be shown is refused rather than shown past its run's floor
		// (raiseFloor), and the box retries — here or on another node.
		s.log.Warn("code_box_unavailable", append([]any{"reason", "floor_unraised", "turn_id",
			unraised.ref.TurnID, "launch_id", unraised.ref.LaunchID, "repo", name, "path", r.URL.Path,
			"error", unraised.err}, attrs...)...)
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, sentence(unraised.Error())+"; retry in "+
			seconds(UnavailableRetryAfter))
	case errors.As(err, &missing):
		// WARN, as the cache's line of the same name for the sync that found
		// it: objects_missing is the alarm, raised by the collector's audit.
		s.log.Warn("code_pack_missing", "repo", name, "pack_id", missing.Pack.PackID,
			"object", missing.Pack.Object.String(), "damaged", missing.Damaged, "error", err)
		retry(w, UnavailableRetryAfter)
		what := "is missing from the object store"
		if missing.Damaged {
			what = "is damaged in the object store"
		}
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s pack "+missing.Pack.PackID+" "+what+
			" (the daily audit raises objects_missing for it); retry in "+seconds(UnavailableRetryAfter))
	case errors.As(err, &corrupt):
		// NO RETRY ENDS IT, so none is promised (cache.ErrCorruptPack): the
		// store has just verified these are the bytes the push stored, so
		// the row or the upload is wrong, and every retry would start a sync
		// that downloads the whole bundle again — up to code.MaxPackBytes a
		// time, from a client or a balancer obeying the promise. A 500 with
		// no Retry-After, then: only an operator ends it, and only the node
		// that received the push, which holds its own copy, serves it until
		// then.
		s.log.Error("code_pack_checksum_mismatch", "repo", name, "pack_id", corrupt.Pack.PackID,
			"object", corrupt.Pack.Object.String(), "error", err)
		auth.GitText(w, http.StatusInternalServerError, name+"'s pack "+corrupt.Pack.PackID+
			" in the object store is not the bundle its row describes (code_pack_checksum_mismatch): "+
			"retrying does not end it — an operator must act on this node's code_pack_checksum_mismatch "+
			"line, and until then only the node that received the push, which holds its own copy, can "+
			"serve it")
	case errors.Is(err, cache.ErrBadSnapshot):
		// NOR THIS ONE (cache.ErrBadSnapshot): the rows hold what this
		// build cannot install, and asking again reads the same rows. Only
		// an operator, or a node of a newer build that installs them,
		// ends it.
		s.log.Error("code_bad_snapshot", "repo", name, "error", err,
			"detail", "the rows and the bytes disagree: an operator's problem, not a client's")
		auth.GitText(w, http.StatusInternalServerError, name+"'s rows cannot be read into a cache on this "+
			"node (code_bad_snapshot): "+sentence(err.Error())+"; retrying does not end it — an operator "+
			"must act on this node's code_bad_snapshot line, and a node running a newer build may serve it")
	case errors.Is(err, cache.ErrRebuildRefused), errors.Is(err, errStillDamaged):
		// THE COPY'S REPAIR DID NOT END ITS DAMAGE: refused, since one that
		// did not end it will not end it a second time from the same rows
		// (the error says which rule and names `crewlet code verify`), or
		// made under this very request and met again (repairing). Another
		// node's copy is its own, so the retry may go there.
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s cache on this node is still damaged after its "+
			"last repair ("+sentence(err.Error())+"): retry in "+seconds(UnavailableRetryAfter)+
			", or against another node")
	case errors.Is(err, cache.ErrStopped):
		stopping(w)
	case errors.Is(err, gitproc.ErrBusy):
		retry(w, BusyRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, "every git slot on this node stayed busy for "+
			seconds(gitproc.SlotWait)+": retry in "+seconds(BusyRetryAfter))
	// Before the ends below: a capture that outgrew its bound, and a line of
	// git's that the engine's reader refused, reach the pool as its Stdout
	// failing (gitproc.ErrOutput) — but neither is this node too loaded to
	// answer, and no retry ends either.
	case refusedByCaller(err):
		s.gitRefused(w, r, name, err, attrs...)
	case errors.Is(err, gitproc.ErrPartialLine), errors.Is(err, gitproc.ErrLineTooLong):
		// GIT ANSWERED WHAT THE ENGINE CANNOT READ (gitproc.Lines): output
		// ending inside a line once git exited as if it had answered, or a
		// line past gitproc.MaxLine. A process that died mid-line is
		// reported as that by the pool first, so this is git's answer — a
		// git failure, as an exit is, which no retry ends.
		s.log.Warn(failedLine(r), append([]any{"repo", name, "error", err}, attrs...)...)
		auth.GitText(w, http.StatusInternalServerError, "git's answer on this node could not be read ("+
			sentence(err.Error())+"); retrying does not end it")
	case errors.Is(err, gitproc.ErrDeadline), errors.Is(err, gitproc.ErrStalled),
		errors.Is(err, gitproc.ErrInput), errors.Is(err, gitproc.ErrOutput):
		s.gitStopped(w, r, name, err, attrs...)
	case errors.As(err, &exit):
		s.log.Warn(failedLine(r), append([]any{"repo", name, "command", exit.Command, "error", err}, attrs...)...)
		auth.GitText(w, http.StatusInternalServerError, "git "+exit.Command+" failed on this node ("+
			kindOf(exit)+"): "+sentence(exit.Result.LastLine()))
	case errors.Is(err, cache.ErrLogRead), errors.Is(err, errLogRead):
		s.unavailable(w, r, name, err)
	case errors.Is(err, cache.ErrStoreRead):
		// THE OBJECT STORE, not the code log: a pack the rows name that the
		// store did not hand over, for no reason that is an answer about
		// it — a timeout, a throttle past its retries, the store
		// unreachable. Another node meets the same store, but may hold the
		// pack already, so the retry it promises may go anywhere.
		s.log.Warn("code_object_store_unavailable", append([]any{"repo", name, "path", r.URL.Path,
			"error", err}, attrs...)...)
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s packs could not be read from the object store "+
			"here ("+sentence(err.Error())+"): retry in "+seconds(UnavailableRetryAfter)+", or against another node")
	default:
		s.nodeFailed(w, r, name, err, attrs...)
	}
}

// errLogRead marks the host's own read of the code log failing — a fetch
// request's bound asked at the log's head (boundOf) — as cache.ErrLogRead
// marks a sync's: the code log's failure, answered as that.
var errLogRead = errors.New("host: the code log could not be read")

// nodeFailed answers a request that failed on THIS NODE for a reason nothing
// before it named — its disk refusing a write into the cache or a spool, a
// git that could not start, a failure no case foresaw — as that: 503, the
// failure in the sentence, and a retry that may go to another node, logged as
// the request's failure (failedLine) with reason node. Not the code log's
// sentence, which would send the operator to the fleet for this node's disk.
func (s *Server) nodeFailed(w http.ResponseWriter, r *http.Request, name string, err error, attrs ...any) {
	s.log.Warn(failedLine(r), append([]any{"repo", name, "reason", "node", "error", err}, attrs...)...)
	retry(w, UnavailableRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "this node could not serve "+name+" ("+sentence(err.Error())+
		"): retry in "+seconds(UnavailableRetryAfter)+", or against another node")
}

// gitStopped answers a request whose git process THIS NODE ENDED before it
// wrote a byte — past its deadline, stalled, or its own input or output
// failing here (a fetch's spooled request read back, a capture) — which is
// neither git refusing the request nor the code log failing, and is never
// answered as either: a 503 saying what stopped it, logged as the push's or
// the fetch's failure (failedLine) beside the pool's own code_git_killed. A
// node that ends git past its deadline or stalled is one too loaded to answer
// in time, so the wait is BusyRetryAfter's and the retry may go elsewhere.
// A process the REQUEST'S OWN end killed never reaches here: syncFailed
// answers that as what it is first (ended).
func (s *Server) gitStopped(w http.ResponseWriter, r *http.Request, name string, err error, attrs ...any) {
	reason, why := "deadline", "past its deadline"
	switch {
	case errors.Is(err, gitproc.ErrStalled):
		reason, why = "stall", "stalled"
	case errors.Is(err, gitproc.ErrInput):
		reason, why = "input", "when its input failed"
	case errors.Is(err, gitproc.ErrOutput):
		reason, why = "output", "when its output failed"
	}
	s.log.Warn(failedLine(r), append([]any{"repo", name, "reason", reason, "error", err}, attrs...)...)
	retry(w, BusyRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "git on this node was stopped before it answered, "+why+
		" ("+sentence(err.Error())+"): retry in "+seconds(BusyRetryAfter)+", or against another node")
}

// refusedByCaller reports a refusal that is the CALLER'S, never the
// repository's, the code log's or the client's — a bug in this build, which
// the request changes nothing about, so no retry ends it:
//
//   - the pool refusing the engine's own call to git: malformed
//     (gitproc.ErrInvalid, ErrNoDeadline), naming configuration or environment
//     outside its allowlists (ErrConfig, ErrEnv), capturing output a call site
//     must stream (ErrCaptureFull, whose own message says so), naming a
//     remote no git process may dial (ErrRemote: no request of the host's
//     dials another host, so one reaching here is the engine's own call), or
//     reaching a pool that should never have been built — git's HOME not
//     empty (ErrHome), a git below the node floor (*gitproc.ErrTooOld), a git
//     that writes no trace2 stream (ErrNoTrace), each refused by gitproc.New
//     and never by a run;
//   - the cache refusing the host's call (cache.ErrInvalid): a Synced or a
//     scratch it did not hand out, an id that is no canonical UUID, an
//     adoption whose row and scratch disagree;
//   - a sync asked at a level no reader serves (code.ErrBadLevel).
func refusedByCaller(err error) bool {
	var old *gitproc.ErrTooOld
	return errors.Is(err, gitproc.ErrInvalid) || errors.Is(err, gitproc.ErrNoDeadline) ||
		errors.Is(err, gitproc.ErrConfig) || errors.Is(err, gitproc.ErrEnv) ||
		errors.Is(err, gitproc.ErrHome) || errors.As(err, &old) || errors.Is(err, gitproc.ErrNoTrace) ||
		errors.Is(err, gitproc.ErrCaptureFull) || errors.Is(err, gitproc.ErrRemote) ||
		errors.Is(err, cache.ErrInvalid) || errors.Is(err, code.ErrBadLevel)
}

// gitRefused answers a request the engine's own call failed for the caller's
// fault (refusedByCaller): a 500 naming the refusal, with NO Retry-After — no
// retry ends a call-site bug, and a client or a balancer obeying a promise of
// one would ask again for ever — logged at ERROR as code_git_refused, the one
// line an operator acts on. Never "the code log could not be read": the log
// was read fine, and the sentence would send the operator to the fleet's
// quorum for a bug in this build.
func (s *Server) gitRefused(w http.ResponseWriter, r *http.Request, name string, err error, attrs ...any) {
	s.log.Error("code_git_refused", append([]any{"repo", name, "path", r.URL.Path, "error", err}, attrs...)...)
	auth.GitText(w, http.StatusInternalServerError, "this node refused its own call serving this request ("+
		sentence(err.Error())+"): the engine's call is wrong, and retrying does not end it — an operator "+
		"must act on this node's code_git_refused line")
}

// errStillDamaged marks a git process that failed, over a cache, with damage
// the cache repaired under this very request and met again on the repaired
// copy (repairing): the repair did not end it.
var errStillDamaged = errors.New("host: the cache's repair did not end its damage")

// ended answers a request whose OWN context ended — its client hung up, or
// the host is stopping — and reports whether it did. Whatever failed then
// failed because of that end (a sync abandoned, a git process the request's
// context killed), so it is never answered as the failure it looks like: a
// client that hung up has nobody listening, and a stopping host answers what
// begin answers a request that arrives at one. Logged at DEBUG for what it
// is: neither is the code log failing, and a WARN for every client that hangs
// up is noise that hides one that matters.
func (s *Server) ended(w http.ResponseWriter, r *http.Request, name string, err error) bool {
	by := s.endedBy(r)
	if by == "" {
		return false
	}
	s.log.Debug("code_git_abandoned", "repo", name, "path", r.URL.Path, "by", by, "error", err)
	if by == endedByStop {
		stopping(w)
	}
	return true
}

// What ended a request before its answer: the host's stop, or its client.
const (
	endedByStop   = "stop"
	endedByClient = "client"
)

// endedBy is what ended r's own context — endedByStop when the host is
// stopping (its lifetime joins every request's), endedByClient when the client
// hung up — or "" while the request is live.
func (s *Server) endedBy(r *http.Request) string {
	switch {
	case s.life.Err() != nil:
		return endedByStop
	case r.Context().Err() != nil:
		return endedByClient
	}
	return ""
}

// stopping answers a request a stopping host will not serve: 503, Retry-After
// StoppingRetryAfter — what takes a stopping node out of a balancer's
// rotation — and the same in words.
func stopping(w http.ResponseWriter) {
	retry(w, StoppingRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "this node's code host is stopping: retry in "+
		seconds(StoppingRetryAfter)+", against another node or once it has restarted")
}

// failedLine is the log line of a git process that failed serving r — by
// itself, or ended by this node: code_push_failed for a push's request (its
// advertisement, its receive-pack, or a process its sync ran), and
// code_fetch_failed for a fetch's, as auth.GitAccess classifies the request.
// The REQUEST names the line, never the process: a push whose sync's
// cat-file failed is a push that failed.
func failedLine(r *http.Request) string {
	if auth.GitAccess(r) == auth.GitPush {
		return "code_push_failed"
	}
	return "code_fetch_failed"
}

// kindOf is an exit's classified kind, in words: its Kind, the highest the
// run named — never whichever errors.Is reaches first, since an exit matches
// every kind its run named.
func kindOf(e *gitproc.ExitError) string {
	switch {
	case errors.Is(e.Kind, gitproc.ErrCorrupt):
		return "corrupt"
	case errors.Is(e.Kind, gitproc.ErrMissingObject):
		return "missing object"
	case errors.Is(e.Kind, gitproc.ErrNotOurRef):
		return "not our ref"
	}
	return "failed"
}
