package host

import (
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
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
// EVERY ANSWER IS IDENTIFIED BY WHAT FAILED, NEVER BY ELIMINATION: the code
// log's sentence is for its own failure alone, marked where it is met
// (cache.ErrLogRead, errLogRead); the object store's is its own
// (cache.ErrStoreRead); and what nothing names (this node's disk, a process
// that could not start, an unforeseen failure) is this node's (nodeFailed).
// As the last case, the code log's sentence would send operators to the
// fleet's quorum for a full disk, a store throttle or a bug in this build.
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
	case errors.Is(err, gitlet.ErrRepoNotFound):
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name))
	case errors.As(err, &unraised):
		// A BOX'S RUN STORE, NEVER THE CODE LOG: an answer past its run's
		// floor is refused rather than shown (raiseFloor); the box retries,
		// here or on another node.
		s.log.Warn("code_box_unavailable", append([]any{"reason", "floor_unraised", "turn_id",
			unraised.ref.TurnID, "launch_id", unraised.ref.LaunchID, "repo", name, "path", r.URL.Path,
			"error", unraised.err}, attrs...)...)
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, sentence(unraised.Error())+"; retry in "+
			seconds(UnavailableRetryAfter))
	case errors.As(err, &missing):
		// WARN, as the cache's line of the same name: objects_missing, raised
		// by the collector's audit, is the alarm.
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
		// NO RETRY ENDS IT, so a 500 promises none (cache.ErrCorruptPack): the
		// store just verified these are the bytes the push stored, so the row
		// or the upload is wrong, and each retry would download the whole
		// bundle again (up to gitlet.MaxPackBytes). Only an operator ends it;
		// until then only the node that received the push serves it.
		s.log.Error("code_pack_checksum_mismatch", "repo", name, "pack_id", corrupt.Pack.PackID,
			"object", corrupt.Pack.Object.String(), "error", err)
		auth.GitText(w, http.StatusInternalServerError, name+"'s pack "+corrupt.Pack.PackID+
			" in the object store is not the bundle its row describes (code_pack_checksum_mismatch): "+
			"retrying does not end it — an operator must act on this node's code_pack_checksum_mismatch "+
			"line, and until then only the node that received the push, which holds its own copy, can "+
			"serve it")
	case errors.Is(err, cache.ErrBadSnapshot):
		// NOR THIS ONE (cache.ErrBadSnapshot): asking again reads the same
		// rows this build cannot install. Only an operator, or a newer node
		// that installs them, ends it.
		s.log.Error("code_bad_snapshot", "repo", name, "error", err,
			"detail", "the rows and the bytes disagree: an operator's problem, not a client's")
		auth.GitText(w, http.StatusInternalServerError, name+"'s rows cannot be read into a cache on this "+
			"node (code_bad_snapshot): "+sentence(err.Error())+"; retrying does not end it — an operator "+
			"must act on this node's code_bad_snapshot line, and a node running a newer build may serve it")
	case errors.Is(err, cache.ErrRebuildRefused), errors.Is(err, errStillDamaged):
		// THE COPY'S REPAIR DID NOT END ITS DAMAGE: a rebuild refused, since
		// the same rows would not end it (the error names the rule and
		// `crewlet gitlet verify`), or one made under this request that met
		// it again (repairing). Another node's copy is its own: retry there.
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
	// Before the ends below: an outgrown capture and a git line the reader
	// refused reach the pool as gitproc.ErrOutput, yet neither is this node
	// too loaded to answer, and no retry ends either.
	case refusedByCaller(err):
		s.gitRefused(w, r, name, err, attrs...)
	case errors.Is(err, gitproc.ErrPartialLine), errors.Is(err, gitproc.ErrLineTooLong):
		// GIT ANSWERED WHAT THE ENGINE CANNOT READ (gitproc.Lines): output
		// ending mid-line after a clean exit, or a line past gitproc.MaxLine.
		// The pool reports a process that died mid-line first, so this is a
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
		// store did not hand over (a timeout, a throttle past its retries,
		// the store unreachable). Another node meets the same store but may
		// hold the pack already, so the retry may go anywhere.
		s.log.Warn("code_object_store_unavailable", append([]any{"repo", name, "path", r.URL.Path,
			"error", err}, attrs...)...)
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s packs could not be read from the object store "+
			"here ("+sentence(err.Error())+"): retry in "+seconds(UnavailableRetryAfter)+", or against another node")
	default:
		s.nodeFailed(w, r, name, err, attrs...)
	}
}

// errLogRead marks the host's own read of the code log failing (a fetch's
// bound asked at the log's head, boundOf), as cache.ErrLogRead marks a sync's.
var errLogRead = errors.New("host: the code log could not be read")

// nodeFailed answers a request that failed on THIS NODE for a reason nothing
// before it named (its disk refusing a write, a git that could not start, an
// unforeseen failure): a 503 naming the failure, a retry that may go to
// another node, logged as the request's failure (failedLine) with reason node.
func (s *Server) nodeFailed(w http.ResponseWriter, r *http.Request, name string, err error, attrs ...any) {
	s.log.Warn(failedLine(r), append([]any{"repo", name, "reason", "node", "error", err}, attrs...)...)
	retry(w, UnavailableRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "this node could not serve "+name+" ("+sentence(err.Error())+
		"): retry in "+seconds(UnavailableRetryAfter)+", or against another node")
}

// gitStopped answers a request whose git process THIS NODE ENDED before it
// wrote a byte (past its deadline, stalled, or its input or output failing
// here: a fetch's spool read back, a capture), never as git refusing or the
// code log failing: a 503 saying what stopped it, logged as the request's
// failure (failedLine) beside the pool's code_git_killed. A node that ends git
// past its deadline or stalled is too loaded to answer in time, so the wait is
// BusyRetryAfter's and the retry may go elsewhere. A process the request's OWN end killed never
// reaches here: syncFailed answers it first (ended).
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

// refusedByCaller reports a refusal that is the CALLER'S, a bug in this build
// rather than the repository's, the code log's or the client's, so no retry
// ends it:
//
//   - the pool refusing the engine's own call: malformed (gitproc.ErrInvalid,
//     ErrNoDeadline), configuration or environment outside its allowlists
//     (ErrConfig, ErrEnv), a capture a call site must stream (ErrCaptureFull),
//     a remote (ErrRemote: no host request dials one), or a pool gitproc.New
//     should have refused (ErrHome, *gitproc.ErrTooOld, ErrNoTrace);
//   - the cache refusing the host's call (cache.ErrInvalid);
//   - a sync asked at a level no reader serves (gitlet.ErrBadLevel).
func refusedByCaller(err error) bool {
	var old *gitproc.ErrTooOld
	return errors.Is(err, gitproc.ErrInvalid) || errors.Is(err, gitproc.ErrNoDeadline) ||
		errors.Is(err, gitproc.ErrConfig) || errors.Is(err, gitproc.ErrEnv) ||
		errors.Is(err, gitproc.ErrHome) || errors.As(err, &old) || errors.Is(err, gitproc.ErrNoTrace) ||
		errors.Is(err, gitproc.ErrCaptureFull) || errors.Is(err, gitproc.ErrRemote) ||
		errors.Is(err, cache.ErrInvalid) || errors.Is(err, gitlet.ErrBadLevel)
}

// gitRefused answers a call-site bug (refusedByCaller): a 500 naming the
// refusal with NO Retry-After (a client or balancer obeying one would ask
// forever), logged at ERROR as code_git_refused, the one line an operator acts
// on, and never as the code log failing.
func (s *Server) gitRefused(w http.ResponseWriter, r *http.Request, name string, err error, attrs ...any) {
	s.log.Error("code_git_refused", append([]any{"repo", name, "path", r.URL.Path, "error", err}, attrs...)...)
	auth.GitText(w, http.StatusInternalServerError, "this node refused its own call serving this request ("+
		sentence(err.Error())+"): the engine's call is wrong, and retrying does not end it — an operator "+
		"must act on this node's code_git_refused line")
}

// errStillDamaged marks a git process that met damage again on a copy
// repaired under this very request (repairing).
var errStillDamaged = errors.New("host: the cache's repair did not end its damage")

// ended answers a request whose OWN context ended (its client hung up, or the
// host is stopping) and reports whether it did. Whatever failed then failed
// because of that end, so it is never answered as the failure it looks like:
// a gone client has nobody listening, and a stopping host answers as begin
// does. Logged at DEBUG: neither is the code log failing, and a WARN per
// hung-up client hides one that matters.
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

// endedBy is what ended r's own context: endedByStop when the host is
// stopping (its lifetime joins every request's), endedByClient when the client
// hung up, or "" while the request is live.
func (s *Server) endedBy(r *http.Request) string {
	switch {
	case s.life.Err() != nil:
		return endedByStop
	case r.Context().Err() != nil:
		return endedByClient
	}
	return ""
}

// stopping answers a request a stopping host will not serve: 503 and
// Retry-After StoppingRetryAfter, which takes it out of a balancer's
// rotation, and the same in words.
func stopping(w http.ResponseWriter) {
	retry(w, StoppingRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "this node's Gitlet is stopping: retry in "+
		seconds(StoppingRetryAfter)+", against another node or once it has restarted")
}

// failedLine is the log line of a git process that failed serving r, by
// itself or ended by this node: code_push_failed, code_fetch_failed or
// code_download_failed, as auth.GitAccess classifies r. The REQUEST names the
// line, never the process: a push whose sync's cat-file failed is a push that
// failed.
func failedLine(r *http.Request) string {
	switch auth.GitAccess(r) {
	case auth.GitPush:
		return "code_push_failed"
	case auth.GitDownload:
		return "code_download_failed"
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
