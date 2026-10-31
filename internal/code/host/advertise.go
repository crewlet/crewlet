package host

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// GET /git/{repo}/info/refs — THE ADVERTISEMENT every git exchange opens with:
// a fetch's refs from upload-pack, or a push's from receive-pack. A box's
// (/box/git/) is the same answer, admitted by admitBox — a check's box's
// always protocol v2's capability advertisement, which names no ref, its
// admission refusing it any other (admitCheck).
//
// EVERY ANSWER THAT NAMES A REF IS READ AT THE REQUEST'S REFS LEVEL
// (admission.refs): a person's v0 advertisement and push advertisement are
// LINEARIZABLE, read through the code log's barrier, so a node in a minority
// answers 503 rather than an old head; a box's are its run's SESSION, and
// raise the run's floor to what they show before their first byte
// (raiseFloor). A v2 capability advertisement names no ref and takes whatever
// the cache holds. And everything that can be refused without objects is
// refused HERE — a token bound to nobody, a box whose run may not push, an
// archived or a deleted repository — in the one form git prints at this step,
// `fatal: remote error: <sentence>`; a push that ends here never POSTs, so it
// is recorded here — refused, or failed however its advertisement failed —
// and one whose advertisement was served is not, since its POST is the push's
// end (pushAudit).
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
	// A PUSH'S ONE RECORD from here, settled however this request ends: every
	// return below that records nothing — the sync, the advertisement's git —
	// is a push recorded failed. A fetch keeps none.
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
	// AGAIN ON THE SYNCED ROW: a linearizable sync may know better than the
	// stale resolve admission answered from.
	if s.refusedAtAdvertisement(w, svc, push, adm, synced.Repo, audit) {
		return
	}

	out := newSink(w, "application/x-"+svc+"-advertisement", s.now)
	defer out.End()
	// ITS USE OF THE REPOSITORY, counted however the answer ends: a fetch's
	// advertisement served is the fetch (Server.served), and a push is
	// counted where it is recorded, so a push's advertisement adds only its
	// bytes.
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
		// advertisement names refs, so the floor is raised to the copy it
		// is served from before its first byte (raiseFloor) — a rebuilt one
		// included.
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
// advertisement VIEW (cache.AdvertiseView) — never in the cache, where
// receive-pack never runs — with the receive configuration its POST will run
// with, so it offers the capabilities the push is honoured under. A push asks
// no protocol v2 (measured: its info/refs carries no Git-Protocol), so no
// GIT_PROTOCOL is passed and the service line is always written.
//
// The view's one alternate is the incarnation, so a pool that refuses the
// incarnation's shape refuses the view, and the incarnation is rebuilt under
// this request (repairing) — every push begins with this advertisement, so a
// cache git refuses for its shape would otherwise refuse every push. *synced
// is the rebuilt one after a rebuild, still the caller's to release.
//
// SERVED, the request is none of the push's ends (audit.none): the push's
// POST follows, and records it — or none follows, a push that found nothing
// to send, which is no push. FAILED, it is left to the request's record,
// which settles it failed.
func (s *Server) advertisePush(ctx context.Context, w http.ResponseWriter, r *http.Request, adm admission,
	synced *cache.Synced, out *sink, audit *pushAudit) {

	out.prefix = serviceLine(receivePackService)
	err := s.repairing(ctx, r, adm.name, synced, asAlternate, out.Started, func(in cache.Synced) error {
		// THE PUSH'S OWN ADVERTISEMENT NAMES THE BASES its thin pack deltas
		// against, so a box's run's floor is raised to it before its first
		// byte: the push's POST, which a balancer may send to another node,
		// then syncs at least that far (raiseFloor).
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

// failedStarted ends a response whose git process failed PAST ITS FIRST BYTE,
// when no status is left to answer with: the request's line (failedLine)
// logged with attrs, and the response ended as git ended it or cut
// (exitedByItself). A failure the request's own end caused — its client gone,
// the host stopping — is logged at DEBUG as code_git_abandoned instead, and
// cut: nobody is reading, and a stopping host serves nothing to its end.
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

// exitedByItself reports a git process that ended on its own with a failure
// (*gitproc.ExitError) — as against one the engine ended: past its deadline,
// stalled, its input failed or its context gone. PAST THE FIRST BYTE THE TWO
// END DIFFERENTLY. Git that fails by itself has said why in its own protocol —
// an `ERR` packet, side band 3 — and the client prints it, so the response
// ends as git ended it, as git's own http-backend ends it; cut, the client
// would print `RPC failed` in place of git's reason (`not our ref` among
// them). A process the engine killed stopped wherever it was, and the
// connection is cut so the client cannot read a cut answer as a whole one.
func exitedByItself(err error) bool {
	var exit *gitproc.ExitError
	return errors.As(err, &exit)
}

// refusedAtAdvertisement answers the refusals an advertisement makes from a
// repository row — a deleted or purging repository for either service, and
// for a push a principal whose grant as its admission judged it
// (admission.grant) lets it push nothing — a token bound to nobody, or a box
// whose run was given the repository to read or whose seat pushes nothing
// there: code.Grant.MayPush — or a repository that takes no push, archived,
// being imported or a pull mirror ([Server.statusRefusal]) — and records a
// push that ends here through its request's record (a fetch's is nil, and
// records nothing). It reports whether it answered.
func (s *Server) refusedAtAdvertisement(w http.ResponseWriter, svc string, push bool, adm admission,
	repo code.RepoRef, audit *pushAudit) bool {

	var (
		words   string
		refusal AuditRefusal
	)
	switch {
	case gone(repo.Status):
		words, refusal = s.goneSentence(repo), AuditRefusal(code.RefusalRepoDeleted)
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

// advertBytes is an upper bound on a v0 advertisement's size for a repository
// of refs refs — each a line naming the ref and an id, and a peeled tag's
// second line — so its deadline is a pace over what it may send rather than
// over the repository's packs.
func advertBytes(refs int64) int64 {
	return (max(refs, 0) + 1) * (code.MaxRefNameBytes + 2*41 + 8)
}
