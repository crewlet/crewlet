package host

import (
	"errors"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// ERRORS ON THE WIRE, before a git process has written a byte: every one a
// status and a GitText sentence, and every 503 a Retry-After that its sentence
// says again in words — git does not read the header.

// syncFailed answers a request whose cache could not be synced, or whose git
// process failed before writing anything: what each failure means to the
// person, and what to do. attrs go on the line a git process's failure is
// logged with (a push's push_id).
func (s *Server) syncFailed(w http.ResponseWriter, r *http.Request, name string, err error, attrs ...any) {
	if s.ended(w, r, name, err) {
		return
	}
	var (
		missing *cache.ErrPackMissing
		corrupt *cache.ErrCorruptPack
		exit    *gitproc.ExitError
	)
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name))
	case errors.As(err, &missing):
		s.log.Error("code_pack_missing", "repo", name, "pack", missing.Pack.PackID,
			"object", missing.Pack.Object.String(), "damaged", missing.Damaged, "error", err)
		retry(w, UnavailableRetryAfter)
		what := "is missing from the object store"
		if missing.Damaged {
			what = "is damaged in the object store"
		}
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s pack "+missing.Pack.PackID+" "+what+
			" (the daily audit raises objects_missing for it); retry in "+seconds(UnavailableRetryAfter))
	case errors.As(err, &corrupt):
		s.log.Error("code_pack_checksum_mismatch", "repo", name, "pack", corrupt.Pack.PackID,
			"object", corrupt.Pack.Object.String(), "error", err)
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s pack "+corrupt.Pack.PackID+
			" in the object store is not the bundle its row describes (code_pack_checksum_mismatch); "+
			"retry in "+seconds(UnavailableRetryAfter))
	case errors.Is(err, cache.ErrBadSnapshot):
		s.log.Error("code_bad_snapshot", "repo", name, "error", err,
			"detail", "the rows and the bytes disagree: an operator's problem, not a client's")
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s rows cannot be read into a cache on this "+
			"node: "+sentence(err.Error()))
	case errors.Is(err, cache.ErrRebuildRefused):
		retry(w, UnavailableRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, name+"'s cache on this node was rebuilt moments ago "+
			"and is still damaged: retry in "+seconds(UnavailableRetryAfter)+", or against another node")
	case errors.Is(err, cache.ErrStopped):
		stopping(w)
	case errors.Is(err, errSpoolFull):
		retry(w, BusyRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, "this node's room for large fetch requests is in "+
			"use ("+sentence(err.Error())+"): retry in "+seconds(BusyRetryAfter)+", or against another node")
	case errors.Is(err, gitproc.ErrBusy):
		retry(w, BusyRetryAfter)
		auth.GitText(w, http.StatusServiceUnavailable, "every git slot on this node stayed busy for "+
			seconds(gitproc.SlotWait)+": retry in "+seconds(BusyRetryAfter))
	case errors.Is(err, gitproc.ErrDeadline), errors.Is(err, gitproc.ErrStalled),
		errors.Is(err, gitproc.ErrInput), errors.Is(err, gitproc.ErrOutput):
		s.gitStopped(w, r, name, err, attrs...)
	case errors.As(err, &exit):
		s.log.Warn(failedLine(r), append([]any{"repo", name, "command", exit.Command, "error", err}, attrs...)...)
		auth.GitText(w, http.StatusInternalServerError, "git "+exit.Command+" failed on this node ("+
			kindOf(exit)+"): "+lastLine(exit.Result.Stderr))
	default:
		s.unavailable(w, r, name, err)
	}
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

// kindOf is an exit's classified kind, in words.
func kindOf(e *gitproc.ExitError) string {
	switch {
	case errors.Is(e, gitproc.ErrCorrupt):
		return "corrupt"
	case errors.Is(e, gitproc.ErrMissingObject):
		return "missing object"
	case errors.Is(e, gitproc.ErrNotOurRef):
		return "not our ref"
	}
	return "failed"
}

// lastLine is the last non-empty line of a process's stderr, as a sentence.
func lastLine(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	return sentence(lines[len(lines)-1])
}

// rebuildReason is the cache's name for what a git process in a cache
// directory failed with, and whether it is one a rebuild repairs: never
// ErrNotOurRef, which a want the repository never served and a lost pack print
// alike — the next linearizable sync re-downloads any pack whose file is gone.
func rebuildReason(err error) (cache.RebuildReason, bool) {
	switch {
	case errors.Is(err, gitproc.ErrCorrupt):
		return cache.RebuildCorrupt, true
	case errors.Is(err, gitproc.ErrMissingObject):
		return cache.RebuildMissing, true
	}
	return "", false
}
