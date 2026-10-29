package host

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// POST /git/{repo}/git-upload-pack — A FETCH'S RPC: a v2 `ls-refs` or
// `fetch`, or a v0 negotiation.
//
// AN OBJECT ID HAS NO LEVEL: a fetch names ids that mean the same bytes on
// every node, so it takes whatever the cache holds — unless the cache lacks a
// want, which means the client's refs came from a node further ahead, and
// then ONE linearizable sync runs before it is served — a coding box's too,
// a rare barrier the box's floor cannot spare. An `ls-refs` names refs and is
// read at the request's refs level, as every answer naming a ref is
// (admission.refs): linearizable for a person, the run's session for a box,
// whose floor it raises before its first byte (raiseFloor) — and a check's
// box's is answered empty, the box shown no ref (noRefs); a fetch names none,
// since the host offers no want-ref (cache.ServeConfig).
func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request, admit admitter) {
	start := s.now()
	if r.Header.Get("Content-Type") != UploadPackRequestType {
		auth.GitText(w, http.StatusUnsupportedMediaType,
			"a git-upload-pack request must be "+UploadPackRequestType)
		return
	}
	ctx, end, ok := s.begin(r.Context(), w)
	if !ok {
		return
	}
	defer end()
	adm, ok := admit(ctx, w, r, auth.GitFetch)
	if !ok {
		return
	}
	if gone(adm.repo.Status) {
		auth.GitText(w, http.StatusNotFound, s.goneSentence(adm.repo))
		return
	}

	decoded, ok := decodedBody(w, r, s)
	if !ok {
		return
	}
	declared := r.ContentLength
	if r.Header.Get("Content-Encoding") != "" {
		declared = -1 // the length on the wire, not of what it decodes to
	}
	body, err := s.readRequest(ctx, decoded, declared, adm.spool, s.boundOf(adm))
	if err != nil {
		s.requestRefused(w, r, adm, err)
		return
	}
	defer body.close()

	v2 := protocolVersion(r.Header.Get("Git-Protocol")) == 2
	n, err := parseNegotiation(body.reader(), body.length(), v2)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	if n.namesRefs() && adm.noRefs {
		s.noRefs(w, r, adm, start)
		return
	}
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	if n.namesRefs() {
		fresh = adm.refs()
	}
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, fresh)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	// Whatever synced holds when the handler returns: it is replaced below,
	// and only ever by a Synced a sync or a rebuild handed out.
	defer func() { synced.Release() }()
	if !n.namesRefs() && n.wants && (!v2 || n.command == commandFetch) {
		// cat-file IN THE INCARNATION, so a damaged one is rebuilt here
		// (repairing) and asked again — the wants are read from the request
		// afresh.
		var missing string
		merr := s.repairing(ctx, r, adm.name, &synced, inIncarnation, notStarted, func(in cache.Synced) error {
			var cerr error
			missing, cerr = s.o.Cache.Missing(ctx, in, wantsOf(body.reader, body.length(), v2), body.length())
			return cerr
		})
		if merr != nil {
			s.syncFailed(w, r, adm.name, merr)
			return
		}
		if missing != "" {
			// ONCE: upload-pack answers `not our ref` for whatever is
			// still absent past one sync at the log's head. Into a
			// variable of its own: a failed sync answers the zero Synced,
			// and the directory already pinned is the defer's to release.
			head, herr := s.o.Cache.Sync(ctx, adm.repo.ID, statelog.Freshness{Level: statelog.ReadLinearizable})
			if herr != nil {
				s.syncFailed(w, r, adm.name, herr)
				return
			}
			synced.Release()
			synced = head
		}
	}

	out := newSink(w, "application/x-git-upload-pack-result", s.now)
	// THE BODY IS GIVEN BACK at the answer's first byte once upload-pack has
	// read it (requestBody.feed): no retry reads it past that byte.
	out.onStart = body.started
	if n.recorded() == commandFetch {
		// A FETCH'S ANSWER IS HELD UNTIL ITS PACK BEGINS (band.go): its
		// pack-objects meets a damaged cache only after upload-pack's first
		// line, and a run whose answer started can no longer be run again.
		out.holdPack()
	}
	defer out.End()
	// ITS USE OF THE REPOSITORY, counted however the answer ends: its bytes,
	// and a clone where it was one, served to its end (Server.served). The
	// fetch itself was counted at its advertisement.
	var use cache.Use
	defer func() { s.served(adm, out, use) }()
	// A DAMAGED CACHE IS REBUILT AND THE FETCH RETRIED, over the same request
	// (repairing) — never for `not our ref`, which a want the repository never
	// served prints exactly as a lost pack does. Upload-pack is read like
	// every other process over the incarnation (inIncarnation): its tree's
	// trace2 format strings, which carry none of the request's text — never
	// its stderr or its answer's side band, which copy the client's request
	// ("unexpected line: '<arg>'") and so would let a crafted fetch force a
	// rebuild.
	//
	// THROUGH THE DROPPING VIEW (Synced.Dropping): an object an expunge has
	// begun dropping is served by id until its commit removes it — a fetch
	// that already holds its id loses nothing before then — while no push
	// may build on it (cache's doc, "The dropping view").
	err = s.repairing(ctx, r, adm.name, &synced, inIncarnation, out.Started, func(in cache.Synced) error {
		// A BOX IS SHOWN NOTHING ITS RUN'S FLOOR DOES NOT HOLD: an `ls-refs`
		// raises the floor to the copy it is served from before its first
		// byte (raiseFloor), so the box never reads a branch older on another
		// node than it read here.
		if n.namesRefs() {
			if rerr := s.raiseFloor(ctx, adm, in.Position); rerr != nil {
				return rerr
			}
		}
		out.again()
		_, gerr := s.runGit(ctx, gitproc.Cmd{
			Dir: in.Dir, Dropping: in.Dropping, Repairer: in.Repairer,
			Args:   []string{"upload-pack", "--stateless-rpc", in.Dir},
			Config: s.o.Cache.ServeConfig(), Protocol: r.Header.Get("Git-Protocol"),
			Stdin: body.feed(), Stdout: out, Class: gitproc.ClassTransfer, Share: adm.share(),
			Deadline: gitproc.UploadPackDeadline(in.Repo.LiveBytes), Stall: s.o.Timing.Stall,
		})
		return gerr
	})
	if gitsAnswer(err) {
		// The run's answer stands — what it held reaches the client as git
		// wrote it, its own failure included. Anything else (the node
		// ending git, a repair that did not end the damage, the repair's
		// own failure) is answered with a status, which nothing held has
		// yet preceded.
		if rerr := out.release(); err == nil {
			err = rerr
		}
	}
	if err == nil && n.clone() {
		use.Clones = 1
	}
	if err == nil && n.recorded() == commandFetch {
		s.fetched(ctx, adm, out)
	}
	s.finishFetch(w, r, adm.name, n.recorded(), synced.Kind, start, out, err)
}

// fetched records a fetch of a `visibility: unit` repository that sent its
// contents (Options.Fetched): its request ended well and its answer carried a
// pack — read by the sink's own reading of the answer (sink.PackSent), the
// reading the hold releases the answer by. As the principal its admission
// resolved (admission.principal), one read, and the bytes the client was
// written — before the response ends, which is why the engine's Fetched only
// files it in the node's reads ledger and never waits on the broker.
//
// FETCHES THAT SENT CONTENTS, NOT REQUESTS: over a stateless transport a fetch
// negotiates in rounds, each a request of its own whose answer is
// acknowledgments and no object, so a clone or a fetch is one record however
// many rounds it took. An `ls-refs`, an advertisement and a fetch the node
// ended before its answer was whole are none — they did not send the
// repository, or not all of it — and neither is any read of a `visibility:
// company` repository, which every seat reads by declaration. NOR A CHECK'S
// BOX'S FETCH, which acts as nobody (admission.principal): the engine running
// its repository's own checks file, whose attempt's row is its record.
func (s *Server) fetched(ctx context.Context, adm admission, out *sink) {
	p := adm.principal()
	if adm.repo.Visibility != code.VisibilityUnit || !out.PackSent() || p == (code.Principal{}) {
		return
	}
	s.o.Fetched(ctx, types.CodeFetched{RepoID: adm.repo.ID, RepoName: adm.name,
		PrincipalKind: types.CodeReaderKind(p.Kind), Principal: p.Actor(), Seat: p.Seat,
		Via: types.FetchedViaGit, Reads: 1, Bytes: out.Sent()})
}

// noRefs answers an `ls-refs` of a request shown no ref (admission.noRefs: a
// check's box's) with the listing upload-pack writes of a repository holding
// none — its flush alone — having synced nothing and run nothing: there is no
// ref the request may be shown, and the box fetches its one commit by id.
func (s *Server) noRefs(w http.ResponseWriter, r *http.Request, adm admission, start time.Time) {
	out := newSink(w, "application/x-git-upload-pack-result", s.now)
	defer out.End()
	w.Header().Set("Cache-Control", "no-cache")
	_, err := out.Write([]byte(flushPkt))
	s.finishFetch(w, r, adm.name, commandLsRefs, cache.SyncNone, start, out, err)
}

// gitsAnswer reports whether a fetch's last run ended in an answer of git's
// own: it succeeded, or git failed by itself (exitedByItself) on a copy the
// host did not find still damaged after its repair (errStillDamaged), which is
// answered with the cache's status instead.
func gitsAnswer(err error) bool {
	return err == nil || (exitedByItself(err) && !errors.Is(err, errStillDamaged))
}

// decodedBody is a request's body, paced a mebibyte at a time and decoded
// from gzip or x-gzip where the client compressed it (git compresses a large
// negotiation). Any other encoding is refused 415, an unreadable gzip header
// 400; ok false when it answered.
func decodedBody(w http.ResponseWriter, r *http.Request, s *Server) (io.Reader, bool) {
	in := paced.NewBody(r.Body, http.NewResponseController(w), s.now)
	switch r.Header.Get("Content-Encoding") {
	case "":
		return in, true
	case "gzip", "x-gzip":
		z, err := gzip.NewReader(in)
		if err != nil {
			auth.GitText(w, http.StatusBadRequest, "the request's gzip encoding is unreadable")
			return nil, false
		}
		return z, true
	}
	auth.GitText(w, http.StatusUnsupportedMediaType, "the request's content encoding "+
		r.Header.Get("Content-Encoding")+" is not gzip, the one git sends")
	return nil, false
}

// boundOf is a fetch request's bound for adm's repository: RequestBound of its
// row as admitted, and — asked fresher — of its row read through the barrier,
// so a request past a bound read from a stale row is judged against the
// current one before it is refused.
func (s *Server) boundOf(adm admission) func(context.Context, bool) (int64, error) {
	return func(ctx context.Context, fresher bool) (int64, error) {
		if !fresher || adm.linearizable {
			return RequestBound(adm.repo), nil
		}
		ref, err := s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: adm.name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
		switch {
		case errors.Is(err, code.ErrRepoNotFound):
			return 0, err
		case err != nil:
			return 0, fmt.Errorf("%w: %w", errLogRead, err)
		}
		return RequestBound(ref), nil
	}
}

// requestRefused answers a fetch request that could not be read: past its
// repository's bound or past what this node spools for one request (413), its
// turn at the spool allowance ended unserved (503, spoolRefused), the client's
// body failing or running past the length it declared (400), or the rows or
// the cache's volume failing (503).
func (s *Server) requestRefused(w http.ResponseWriter, r *http.Request, adm admission, err error) {
	name := adm.name
	var unserved *spoolUnserved
	switch {
	case errors.As(err, &unserved):
		s.spoolRefused(w, r, adm, unserved)
	case errors.Is(err, errRequestTooLarge):
		auth.GitText(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("this fetch request is larger than "+
			"any repository %s holds could need — %s — which is fifty bytes for each object and live ref "+
			"it holds, and a mebibyte", name, strings.TrimPrefix(err.Error(), errRequestTooLarge.Error()+": ")))
	case errors.Is(err, errSpoolPastCeiling):
		auth.GitText(w, http.StatusRequestEntityTooLarge, "this fetch request is larger than this node "+
			"spools for one request ("+sentence(strings.TrimPrefix(err.Error(), errSpoolPastCeiling.Error()+": "))+
			"); fetch fewer objects at once — a sparse checkout asks only for the files it checks out")
	case errors.Is(err, paced.ErrBody), errors.Is(err, gzip.ErrChecksum), errors.Is(err, gzip.ErrHeader),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, errPastClaim):
		auth.GitText(w, http.StatusBadRequest, "the request body could not be read: "+sentence(err.Error()))
	default:
		s.syncFailed(w, r, name, err)
	}
}

// spoolRefused answers a fetch whose turn at the spool allowance ended
// unserved — its wait expired, or it was refused a place to wait — 503 with
// BusyRetryAfter, saying what held it back, and logged with the credential
// that sent it and the spool credential it waited as: git does not retry a
// lazy fetch, so this is a checkout that failed, and the line is how an
// operator finds the token behind it — or, keyed `runs` with no operator id,
// that coding boxes held their share.
func (s *Server) spoolRefused(w http.ResponseWriter, r *http.Request, adm admission, e *spoolUnserved) {
	s.log.Warn("code_fetch_spool_unserved", "repo", adm.name, "path", r.URL.Path, "operator_id", adm.operatorID,
		"spool", adm.spool, "reason", e.reason, "error", e)
	retry(w, BusyRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "this node's room for large fetch requests is in use: "+
		sentence(e.detail)+"; retry in "+seconds(BusyRetryAfter)+", or against another node")
}
