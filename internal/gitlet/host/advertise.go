package host

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// GET /git/{repo}/info/refs — THE ADVERTISEMENT every git exchange opens with:
// upload-pack's for a fetch, receive-pack's for a push. A box's (/box/git/) is
// the same answer, admitted by admitBox; a check's box's is always v2's
// capability advertisement, which names no ref (admitCheck).
//
// A ref-naming answer reads at admission.refs, a v2 capability advertisement
// stale (package doc, "Every answer that names a ref is read at its request's
// level; an object id has no level"), and what can be refused without objects
// is refused here (package doc, "A refusal is decided before the bytes, and
// said in git's own words"). A push that ends here never POSTs, so it is
// recorded here; one whose advertisement was served is not, its POST being the
// push's end (pushAudit).
func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request, admit admitter) {
	start := s.now()
	svc, dumb, ok := serviceFor(r)
	switch {
	case dumb:
		auth.GitText(w, http.StatusNotFound,
			"crewlet serves git's smart HTTP protocol only: this client asked for the dumb one")
		return
	case !ok:
		auth.GitText(w, http.StatusNotFound, "unknown git service "+
			strconv.Quote(strings.Join(r.URL.Query()["service"], ",")))
		return
	}
	ctx, end, ok := s.begin(r.Context(), w)
	if !ok {
		return
	}
	defer end()
	push := svc == receivePackService
	op := auth.GitFetch
	if push {
		op = auth.GitPush
	}
	adm, ok := admit(ctx, w, r, op)
	if !ok {
		return
	}
	// A PUSH'S ONE RECORD, settled however this request ends: a return below
	// that records nothing is a push recorded failed. A fetch keeps none.
	var audit *pushAudit
	if push {
		audit = s.newPushAudit(ctx, adm, start)
		defer audit.settle()
	}
	if s.refusedAtAdvertisement(w, svc, push, adm, adm.repo, audit) {
		return
	}

	fresh, namesRefs := adm.refs(), true
	if !push && protocolVersion(r.Header.Get("Git-Protocol")) == 2 {
		fresh, namesRefs = statelog.Freshness{Level: statelog.ReadStale}, false
	}
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, fresh)
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	defer func() { synced.Release() }()
	// AGAIN ON THE SYNCED ROW: a linearizable sync may know better than
	// admission's stale resolve.
	if s.refusedAtAdvertisement(w, svc, push, adm, synced.Repo, audit) {
		return
	}

	out := newSink(w, "application/x-"+svc+"-advertisement", s.now)
	defer out.End()
	// ITS USE, counted however the answer ends: a fetch's advertisement served
	// is the fetch (Server.served); a push is counted where it is recorded, so
	// its advertisement adds only bytes.
	var use cache.Use
	defer func() { s.served(adm, out, use) }()
	w.Header().Set("Cache-Control", "no-cache")
	if push {
		s.advertisePush(ctx, w, r, adm, &synced, out, audit)
		return
	}
	if protocolVersion(r.Header.Get("Git-Protocol")) != 2 {
		out.prefix = serviceLine(svc)
	}
	// IN THE INCARNATION, so a damaged one is rebuilt under this request
	// (repairing).
	err = s.repairing(ctx, r, adm.name, &synced, inIncarnation, out.Started, func(in cache.Synced) error {
		// A BOX IS SHOWN NOTHING ITS RUN'S FLOOR DOES NOT HOLD: a v0
		// advertisement names refs, so the floor is raised to the serving
		// copy, a rebuilt one included, before its first byte (raiseFloor).
		if namesRefs {
			if rerr := s.raiseFloor(ctx, adm, in.Position); rerr != nil {
				return rerr
			}
		}
		// THROUGH THE DROPPING VIEW (Synced.Dropping), as the fetch after it
		// runs: a serving process's, never a receiving one's.
		_, gerr := s.runGit(ctx, gitproc.Cmd{
			Dir: in.Dir, Dropping: in.Dropping, Repairer: in.Repairer,
			Args: []string{"upload-pack", "--stateless-rpc", "--advertise-refs", in.Dir},
			// THE CACHE'S ONE SERVE CONFIGURATION, so the advertisement
			// offers exactly what the fetch after it honours.
			Config: s.o.Cache.ServeConfig(), Protocol: r.Header.Get("Git-Protocol"),
			Stdout: out, Class: gitproc.ClassTransfer, Share: adm.share(),
			Deadline: gitproc.UploadPackDeadline(advertBytes(in.Repo.Refs)), Stall: s.o.Timing.Stall,
		})
		return gerr
	})
	if err == nil {
		use.Fetches = 1
	}
	s.finishFetch(w, r, adm.name, commandAdvertise, synced.Kind, start, out, err)
}

// advertisePush answers a push's advertisement: receive-pack in an
// advertisement VIEW (cache.AdvertiseView), never the cache, with its POST's
// receive configuration, so it offers what the push is honoured under. A
// push's info/refs carries no Git-Protocol: the service line is always written.
//
// The view's one alternate is the incarnation, so a shape git refuses is
// rebuilt under this request (repairing); otherwise every push would be
// refused. *synced is then the rebuilt one, still the caller's to release.
//
// SERVED, the request is none of the push's ends (audit.none): its POST
// records it, or none follows (nothing to send, no push). FAILED, the
// request's record settles it failed.
func (s *Server) advertisePush(ctx context.Context, w http.ResponseWriter, r *http.Request, adm admission,
	synced *cache.Synced, out *sink, audit *pushAudit) {

	out.prefix = serviceLine(receivePackService)
	err := s.repairing(ctx, r, adm.name, synced, asAlternate, out.Started, func(in cache.Synced) error {
		// THE ADVERTISEMENT NAMES THE BASES the thin pack deltas against, so
		// a box's floor is raised to it first: the POST, perhaps on another
		// node, then syncs at least that far (raiseFloor).
		if rerr := s.raiseFloor(ctx, adm, in.Position); rerr != nil {
			return rerr
		}
		view, err := s.o.Cache.AdvertiseView(ctx, in)
		if err != nil {
			return err
		}
		defer view.Remove()
		_, err = s.runGit(ctx, gitproc.Cmd{
			Dir: view.Dir, Alternate: view.Alternate,
			Args:   []string{"receive-pack", "--stateless-rpc", "--advertise-refs", view.Dir},
			Config: append(receiveConfig(s.limits()), s.o.Cache.AdvertiseConfig()...),
			Stdout: out, Class: gitproc.ClassTransfer, Share: adm.share(),
			Deadline: gitproc.UploadPackDeadline(advertBytes(in.Repo.Refs)), Stall: s.o.Timing.Stall,
		})
		return err
	})
	switch {
	case err == nil:
		audit.none()
	case !out.Started():
		s.syncFailed(w, r, adm.name, err)
	default:
		s.failedStarted(r, adm.name, err, "step", "advertisement")
	}
}

// failedStarted ends a response whose git process failed PAST ITS FIRST BYTE:
// failedLine logged with attrs, and the response ended as git ended it or cut
// (exitedByItself). A failure the request's own end caused (client gone, host
// stopping) is logged at DEBUG as code_git_abandoned and cut: nobody reads it.
func (s *Server) failedStarted(r *http.Request, name string, err error, attrs ...any) {
	if by := s.endedBy(r); by != "" {
		s.log.Debug("code_git_abandoned", "repo", name, "path", r.URL.Path, "by", by, "error", err)
		panic(http.ErrAbortHandler)
	}
	s.log.Warn(failedLine(r), append([]any{"repo", name, "error", err}, attrs...)...)
	if !exitedByItself(err) {
		panic(http.ErrAbortHandler)
	}
}

// exitedByItself reports a git process that failed on its own
// (*gitproc.ExitError), not one the engine ended. Past the first byte the two
// end differently: git has said why in its protocol (an `ERR` packet, side
// band 3), so the response ends as git ended it, or the client prints `RPC
// failed` instead of git's reason; a killed process stopped anywhere, so the
// connection is cut lest the client read a partial answer as whole.
func exitedByItself(err error) bool {
	var exit *gitproc.ExitError
	return errors.As(err, &exit)
}

// refusedAtAdvertisement answers the refusals an advertisement makes from a
// repository row: a gone repository for either service; for a push, a grant
// that may push nothing (admission.grant, gitlet.Grant.MayPush) or a status
// that takes no push ([Server.statusRefusal]). It records a push ending here
// (a fetch's audit is nil) and reports whether it answered.
func (s *Server) refusedAtAdvertisement(w http.ResponseWriter, svc string, push bool, adm admission,
	repo gitlet.RepoRef, audit *pushAudit) bool {

	var (
		words   string
		refusal AuditRefusal
	)
	switch {
	case gone(repo.Status):
		words, refusal = s.goneSentence(repo), AuditRefusal(gitlet.RefusalRepoDeleted)
	case push && !adm.grant.MayPush():
		words, refusal = adm.grant.PushRefusal(), grantRefusal(adm.principal())
	case push:
		reason, sentence := s.statusRefusal(repo)
		if reason == "" {
			return false
		}
		words, refusal = sentence, AuditRefusal(reason)
	default:
		return false
	}
	errAdvert(w, svc, words)
	audit.record(pushRecord{outcome: types.AuditRefused, refusal: refusal})
	return true
}

// finishFetch ends a fetch's request: a failure before the first byte answered
// in its status, one after it by cutting the connection, and the request's
// duration recorded under its command and what the sync cost.
func (s *Server) finishFetch(w http.ResponseWriter, r *http.Request, name, command string,
	kind cache.SyncKind, start time.Time, out *sink, err error) {

	if s.o.Metrics != nil {
		s.o.Metrics.Observe(metrics.CodeFetchDuration, s.now().Sub(start),
			metrics.Attrs{"command": command, "sync": string(kind)})
	}
	switch {
	case err == nil:
	case !out.Started():
		s.syncFailed(w, r, name, err)
	default:
		s.failedStarted(r, name, err, "command", command)
	}
}

// receivePackService and uploadPackService are git's names for the two
// services, as an advertisement's query asks for them.
const (
	receivePackService = "git-receive-pack"
	uploadPackService  = "git-upload-pack"
)

// serviceLine is the smart protocol's first packet and its flush: what an
// advertisement starts with for protocol v0 and for every push.
func serviceLine(svc string) []byte {
	return append(pktString("# service="+svc+"\n"), flushPkt...)
}

// protocolVersion reads `version=N` among a Git-Protocol header's
// colon-separated parameters: 0 when absent or unreadable.
func protocolVersion(header string) int {
	for param := range strings.SplitSeq(header, ":") {
		if v, ok := strings.CutPrefix(param, "version="); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				return n
			}
		}
	}
	return 0
}

// advertBytes bounds a v0 advertisement's size for refs refs (a line per ref,
// a peeled tag's second line), so its deadline paces what it may send rather
// than the repository's packs.
func advertBytes(refs int64) int64 {
	return (max(refs, 0) + 1) * (gitlet.MaxRefNameBytes + 2*41 + 8)
}
