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
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// POST /git/{repo}/git-upload-pack: a v2 `ls-refs` or `fetch`, or a v0
// negotiation. A fetch takes whatever the cache holds unless it lacks a want;
// then ONE linearizable sync runs first, a coding box's paid from its run's
// re-syncs and answered at its floor once they are spent (boxlimit.go). A
// check's box's wants are looked for at its check's session
// (admission.objects), a want even that lacks paid as a coding box's is. An
// `ls-refs` is read at the request's refs level (admission.refs), a box's
// raising its floor before its first byte (raiseFloor), a check's box's
// answered empty (noRefs). (package doc, "Every answer that names a ref is
// read at its request's level; an object id has no level")
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
	fresh := adm.objects()
	if n.namesRefs() {
		fresh = adm.refs()
	}
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, fresh)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	// Released at return, whatever it then holds: it is replaced below, only
	// by a Synced a sync or a rebuild handed out.
	defer func() { synced.Release() }()
	if !n.namesRefs() && n.wants && (!v2 || n.command == commandFetch) {
		// cat-file in the incarnation, so damage is repaired and the check
		// asked again (repairing), each try reading the wants afresh.
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
			// A box's re-sync is its run's to pay (boxlimit.go): spent, the
			// want is answered at the run's floor, which lacks it.
			if spent := s.boxResync(adm); spent != nil {
				s.wantNotAtFloor(w, r, adm, missing, spent, n.recorded(), start)
				return
			}
			// Once: upload-pack answers `not our ref` for whatever one sync
			// at the log's head still lacks. Into its own variable: a failed
			// sync answers the zero Synced, and the pinned directory is the
			// defer's to release.
			head, herr := s.o.Cache.Sync(ctx, adm.repo.ID, statelog.Freshness{Level: statelog.ReadLinearizable})
			if herr != nil {
				s.syncFailed(w, r, adm.name, herr)
				return
			}
			synced.Release()
			synced = head
			// A box's barrier raises its run's floor, so its next request on
			// any node pays no barrier for the same objects. Best effort: a
			// fetch by id is served whatever the floor says, so a refused
			// raise is only logged.
			if rerr := s.raiseFloor(ctx, adm, head.Position); rerr != nil {
				s.log.Warn("code_box_floor_unraised", "repo", adm.name, "path", r.URL.Path, "error", rerr)
			}
		}
	}

	out := newSink(w, "application/x-git-upload-pack-result", s.now)
	// The body is given back at the answer's first byte (requestBody.feed):
	// no retry reads it past that byte.
	out.onStart = body.started
	if n.recorded() == commandFetch {
		// Held until its pack begins (band.go): pack-objects meets a damaged
		// cache only after upload-pack's first line, and a run whose answer
		// started cannot be run again.
		out.holdPack()
	}
	defer out.End()
	// Its use, counted however the answer ends: its bytes, and a clone served
	// to its end (Server.served). The fetch was counted at its advertisement.
	var use cache.Use
	defer func() { s.served(adm, out, use) }()
	// A damaged cache is repaired and the fetch retried over the same request
	// (repairing), never for `not our ref`, which a never-served want prints
	// exactly as a lost pack does. Damage is read from trace2 format strings
	// (inIncarnation), never stderr or the side band, which echo the client's
	// request and would let a crafted fetch force a rebuild.
	//
	// Through the dropping view (Synced.Dropping): an object an expunge is
	// dropping is served by id until its commit removes it, though no push may
	// build on it (cache's doc, "The dropping view").
	err = s.repairing(ctx, r, adm.name, &synced, inIncarnation, out.Started, func(in cache.Synced) error {
		// A box is shown nothing its run's floor lacks: an `ls-refs` raises
		// the floor to its copy before its first byte (raiseFloor), so no
		// other node shows the box an older branch.
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
		// Git's answer stands, its own failure included: what it held
		// reaches the client. Anything else (the node ending git, damage a
		// repair did not end, the repair failing) is answered with a
		// status, which nothing held has yet preceded.
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
// pack (sink.PackSent). It runs before the response ends, so the engine's
// Fetched only files it in the reads ledger, never waiting on the broker.
// A fetch is one record however many negotiation rounds it took; an
// `ls-refs`, an advertisement, a fetch the node ended, a `visibility:
// company` repository and a check's box, which acts as nobody
// (admission.principal; its attempt's row is its record), record nothing
// (package doc, "Who read a unit's repository is recorded where its pack was
// sent").
func (s *Server) fetched(ctx context.Context, adm admission, out *sink) {
	p := adm.principal()
	if adm.repo.Visibility != gitlet.VisibilityUnit || !out.PackSent() || p == (gitlet.Principal{}) {
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
// row as admitted, or, asked fresher, of its row read through the barrier, so
// a stale row's bound refuses nothing. A box's barrier read is a re-sync its
// run pays for (boxlimit.go): spent, it is held to the admitted row's bound.
func (s *Server) boundOf(adm admission) func(context.Context, bool) (int64, error) {
	return func(ctx context.Context, fresher bool) (int64, error) {
		if !fresher || adm.linearizable || s.boxResync(adm) != nil {
			return RequestBound(adm.repo), nil
		}
		ref, err := s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: adm.name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
		switch {
		case errors.Is(err, gitlet.ErrRepoNotFound):
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
// unserved (its wait expired, or it got no place to wait) 503 with
// BusyRetryAfter. It logs the credential and spool credential: git does not
// retry a lazy fetch, so this is a failed checkout, and the line is how an
// operator finds the token behind it (or, keyed `runs` with no operator id,
// sees coding boxes held their share).
func (s *Server) spoolRefused(w http.ResponseWriter, r *http.Request, adm admission, e *spoolUnserved) {
	s.log.Warn("code_fetch_spool_unserved", "repo", adm.name, "path", r.URL.Path, "operator_id", adm.operatorID,
		"spool", adm.spool, "reason", e.reason, "error", e)
	retry(w, BusyRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "this node's room for large fetch requests is in use: "+
		sentence(e.detail)+"; retry in "+seconds(BusyRetryAfter)+", or against another node")
}
