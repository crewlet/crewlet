package host

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// POST /git/{repo}/git-upload-pack — A FETCH'S RPC: a v2 `ls-refs` or
// `fetch`, or a v0 negotiation.
//
// AN OBJECT ID HAS NO LEVEL: a fetch names ids that mean the same bytes on
// every node, so it takes whatever the cache holds — unless the cache lacks a
// want, which means the client's refs came from a node further ahead, and
// then ONE linearizable sync runs before it is served. An `ls-refs` and a
// `want-ref` name refs and are linearizable, as every answer naming a ref is.
func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	if r.Header.Get("Content-Type") != "application/x-git-upload-pack-request" {
		auth.GitText(w, http.StatusUnsupportedMediaType,
			"a git-upload-pack request must be application/x-git-upload-pack-request")
		return
	}
	ctx, end, ok := s.begin(r.Context(), w)
	if !ok {
		return
	}
	defer end()
	adm, ok := s.admit(ctx, w, r, auth.GitFetch)
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
	body, err := s.readRequest(ctx, decoded, declared, adm.operatorID, s.boundOf(adm))
	if err != nil {
		s.requestRefused(w, r, adm.name, err)
		return
	}
	defer body.close()

	v2 := protocolVersion(r.Header.Get("Git-Protocol")) == 2
	n, err := parseNegotiation(body.reader(), body.length(), v2)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	level := statelog.ReadStale
	if n.linearizable() {
		level = statelog.ReadLinearizable
	}
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, statelog.Freshness{Level: level})
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	// Whatever synced holds when the handler returns: it is replaced below,
	// and only ever by a Synced a sync or a rebuild handed out.
	defer func() { synced.Release() }()
	if level == statelog.ReadStale && n.wants && (!v2 || n.command == commandFetch) {
		// cat-file IN THE INCARNATION, so a damaged one is rebuilt here
		// (repairing) and asked again — the wants are read from the request
		// afresh.
		var missing string
		merr := s.repairing(ctx, r, adm.name, &synced, cache.DamageReason, notStarted, func(in cache.Synced) error {
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

	out := newSink(w, "application/x-git-upload-pack-result", nil, s.now)
	// THE BODY IS GIVEN BACK at the answer's first byte once upload-pack has
	// read it (requestBody.feed): no retry reads it past that byte.
	out.onStart = body.started
	defer out.End()
	// A DAMAGED CACHE IS REBUILT AND THE FETCH RETRIED ONCE, over the same
	// request (repairing) — never for `not our ref`, which a want the
	// repository never served prints exactly as a lost pack does.
	err = s.repairing(ctx, r, adm.name, &synced, cache.DamageReason, out.Started, func(in cache.Synced) error {
		_, gerr := s.o.Git.Run(ctx, gitproc.Cmd{
			Dir:    in.Dir,
			Args:   []string{"upload-pack", "--stateless-rpc", in.Dir},
			Config: s.o.Cache.ServeConfig(), Protocol: r.Header.Get("Git-Protocol"),
			Stdin: body.feed(), Stdout: out, Class: gitproc.ClassTransfer,
			Deadline: gitproc.UploadPackDeadline(in.Repo.LiveBytes), Stall: s.o.Timing.Stall,
		})
		return gerr
	})
	s.finishFetch(w, r, adm.name, n.recorded(), synced.Kind, start, out, err)
}

// decodedBody is a request's body, paced a mebibyte at a time and decoded
// from gzip or x-gzip where the client compressed it (git compresses a large
// negotiation). Any other encoding is refused 415, an unreadable gzip header
// 400; ok false when it answered.
func decodedBody(w http.ResponseWriter, r *http.Request, s *Server) (io.Reader, bool) {
	in := paced.Body(r.Body, http.NewResponseController(w), s.now)
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
// repository's bound or past what one credential may spool on this node (413),
// the spool allowance spent (503), the client's body failing (400), or the
// rows or the cache's volume failing (503).
func (s *Server) requestRefused(w http.ResponseWriter, r *http.Request, name string, err error) {
	switch {
	case errors.Is(err, errRequestTooLarge):
		auth.GitText(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("this fetch request is larger than "+
			"any repository %s holds could need — %s — which is fifty bytes for each object and live ref "+
			"it holds, and a mebibyte", name, strings.TrimPrefix(err.Error(), errRequestTooLarge.Error()+": ")))
	case errors.Is(err, errSpoolPastShare):
		auth.GitText(w, http.StatusRequestEntityTooLarge, "this fetch request is larger than this node "+
			"takes from one credential at once ("+sentence(strings.TrimPrefix(err.Error(),
			errSpoolPastShare.Error()+": "))+"); fetch through a node with more cores and the memory to back them, or fewer objects at once")
	case errors.Is(err, paced.ErrBody), errors.Is(err, gzip.ErrChecksum), errors.Is(err, gzip.ErrHeader),
		errors.Is(err, io.ErrUnexpectedEOF):
		auth.GitText(w, http.StatusBadRequest, "the request body could not be read: "+sentence(err.Error()))
	default:
		s.syncFailed(w, r, name, err)
	}
}
