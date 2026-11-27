package estate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A repository's git work is asked of its home Gitlet node (ADR-0089;
// package doc, "A repository's git work is asked of its home Gitlet node").

// GitCopy is the copy a GIT operation names ([op.keyedOnGit]): no domain's
// rows, but the serving node's git cache. No copy verdict, dispatch floor or
// lag gate applies (its floor travels as gitlet.GitQuery.Fresh, the cache's
// sync level), so a Gitlet node whose copy of the code log is out of service
// still serves git reads. Like [SeatCopy], no data operation may name it
// (TestEveryOperationNamesTheCopyItReads).
const GitCopy = "git"

// LocalGit is this node's git half, resolved PER REQUEST: Gitlet is built at
// boot or at the first apply that sets code.backend: gitlet, after the router
// and the server exist, so one captured at construction would answer "Gitlet
// not built" for the life of a node that applied its company later.
type LocalGit interface {
	// Git is this node's git server, or false while Gitlet is not built here.
	Git() (GitServer, bool)
}

// GitServer is the git half a Gitlet node serves, declared by its consumer:
// reads by object id (internal/gitlet/query), engine writes
// (internal/gitlet/host) and merge previews (internal/gitlet/home), over one
// cache, one git pool and one seat git-work share.
type GitServer interface {
	// Read answers one read by object id. ctx ENDS WHEN THE ANSWER IS DUE,
	// remote or in process ([gitlet.AnswerBy]); a read it ends answers
	// gitlet.ErrGitUnready while the asker still listens ([gitlet.AnswerDue]),
	// never ctx's own error, which the walk would take for the repository's
	// answer.
	Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)

	// Write builds one engine write over this node's copy and lands it as a
	// push (ADR-0092). ctx ends as a read's does: ended before the landing,
	// nothing ran and it answers gitlet.ErrGitUnready, so the walk asks the
	// next Gitlet node, which builds the same push; ended inside the landing,
	// it answers what the landing came to, read from the push's row. Never
	// ctx's own error.
	Write(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error)

	// MergePreview answers what merging one commit into another would do,
	// merged in a scratch beside this node's copy, never in it. ctx ends as a
	// read's does, answering gitlet.ErrGitUnready, never ctx's own error.
	MergePreview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error)
}

// GitPlacement is who serves GIT ([RouterOptions.Git]): every live node with
// the `gitlet` role, which the git walk and [Router.Home] order, and ONE
// listing of them with what each advertises for its background work, which
// [Router.Maintainer] places by. The engine answers both from its presence
// view.
type GitPlacement interface {
	Placement

	// Listing is the Gitlet nodes and what each advertises, off ONE listing of
	// the fleet, so a node is weighed by the listing that names it: weighed
	// off a later one that dropped it, it would read as one that said nothing.
	// An error is UNKNOWN, never "no Gitlet node".
	Listing() (GitListing, error)
}

// GitListing is one listing of the fleet's Gitlet nodes, as
// [GitPlacement.Listing] answers it.
type GitListing struct {
	// Nodes is every Gitlet node the listing carries.
	Nodes []string

	// Budgets is each node's advertised maintenance budget, which its presence
	// row carries on every beat beside its status, never in it
	// (coord.GitBackgroundKey). Zero is a node with nothing to give (Gitlet
	// not built yet, or transfers holding all its memory); a node of Nodes
	// with no entry is NOT SAYING (a build or host that writes none), never
	// one with nothing to give. A Gitlet node of this build always says it.
	Budgets map[string]int64
}

// GitReadAttempt bounds one git read's wait on one Gitlet node: [Production]'s
// [Timing.GitReadAttempt]. SEVENTEEN SECONDS, the node's waits at their WHOLE
// budgets, never p99s: its share's wait, two syncs (the asker's floor, one
// re-sync of an id the copy lacks), its own git's run time and the reply's
// margin. The lane's queue and the id-finding processes are left out, or a
// dead node would be waited out for minutes; a read they hold past its due
// instant is answered unready, never late (package doc, "What a git attempt
// sums"). TestProductionsGitReadAttemptHoldsAGitReadsWaits holds it.
const GitReadAttempt = gitlet.GitShareWait + 2*statelog.ReadBudget + gitproc.ReadDeadline + gitlet.ReplyMargin

// GitWriteAttempt bounds one engine write's wait on one Gitlet node:
// [Production]'s [Timing.GitWriteAttempt]. EIGHTY-TWO SECONDS, the node's
// waits at their WHOLE budgets: its share's wait, three syncs, the build's
// git, the bundle's upload at objstore.MiBPace, the proposal's own attempt
// (gitlet.MinProposeAttempt) and the reply's margin. The node fits every stage
// into it, answering busy before an upload that would not fit, so only a
// WEDGED node costs it whole. It holds whatever the caller allows
// ([Router.budgetFor]): a turn's minutes waited out on a wedged home would
// leave the runner-up never asked (package doc, "What a git attempt sums").
// TestProductionsGitWriteAttemptHoldsAWritesWaits holds the sum.
const GitWriteAttempt = gitlet.GitShareWait + 3*statelog.ReadBudget + gitlet.WriteBuildDeadline +
	gitlet.WriteUploadAllowance + minWriteAttempt + gitlet.ReplyMargin

// errGitUnready is a git half that could not answer inside its budget for a
// reason about THE NODE (gitlet.ErrGitUnready: syncing, its share full, no
// chart or configuration to judge a seat by, its own failure, or no copy and
// no fill asked of it): it ran nothing of the repository's content, so
// another Gitlet node may answer. It is answered `behind` (`behind_config` for
// the node's posture) with its reason ([reply.Unready]); the walk moves on
// WITHOUT demoting the node, which is the right one to ask a moment later,
// and reads the reason to know whether the fill passes on ([passesFill]).
var errGitUnready = errors.New("estate: this Gitlet node cannot answer now")

// unreadyGit is a git half's gitlet.ErrGitUnready as the walk reads it: errGitUnready,
// its cause kept for the walk's account.
type unreadyGit struct{ cause error }

func (u unreadyGit) Error() string   { return u.cause.Error() }
func (u unreadyGit) Unwrap() []error { return []error{errGitUnready, u.cause} }

// gitUnready is a git half's error as the walk reads it: gitlet.ErrGitUnready
// becomes [errGitUnready]; any other error is the answer, a refusal every
// Gitlet node would give alike (gitlet.ErrNotPermitted,
// gitlet.ErrObjectNotFound, gitlet.ErrInvalid, …).
func gitUnready(err error) error {
	var unready *gitlet.ErrGitUnready
	if errors.As(err, &unready) {
		return unreadyGit{cause: err}
	}
	return err
}

// unreadyDetail is what a node's answer says of a git half that was not ready:
// the half's own sentence, which names the node and what it is doing.
func unreadyDetail(err error) string {
	var unready *gitlet.ErrGitUnready
	if errors.As(err, &unready) {
		return unready.Detail
	}
	return err.Error()
}

// unreadyReason is the reason a git half's gitlet.ErrGitUnready gave, and empty
// for an error that carries none — which passes no fill
// (gitlet.UnreadyReason.PassesFill).
func unreadyReason(err error) gitlet.UnreadyReason {
	var unready *gitlet.ErrGitUnready
	if errors.As(err, &unready) {
		return unready.Reason
	}
	return ""
}

// passesFill reports whether a Gitlet node's reply (it ran nothing) says no
// copy serving the read is coming on that node, so the walk lets the next
// node fill in its place: a node that ran nothing for the operation at all
// (Gitlet not built, a key it does not hold, a request its build would not
// take: [ErrUnknownKeys]), or one whose reason passes (behind its
// configuration, its own failure). One that may be bringing a copy up keeps
// the fill (syncing, its chart moments away, an unknown or missing reason),
// as does one at its share, which holds its copy and is only busy. A request
// refused unread ([server.answerGit]) passes too: kept on it, every unstamped
// runner-up answered `not_held` and a request a node of the asker's build
// could serve failed with the refusal.
func passesFill(rep reply) bool {
	switch rep.Unserved {
	case unservedNoBackend, unservedUnauthenticated, unservedStale:
		return true
	case unservedBehind, unservedBehindConfig:
		return rep.Unready.PassesFill()
	}
	return errors.As(decodeError(rep.Err), new(*ErrUnknownKeys))
}

// routeGit runs one GIT operation ([op.keyedOnGit]) over the Gitlet nodes in
// the repository's rendezvous order, the ONE node the walk lets FILL a copy
// asked with its arguments stamped home ([exchange.home]), every other not.
// The fill starts at the effective home (the first node after demotions) and
// moves on only past a node bringing no copy up ([passesFill]), without
// demoting it, or a home stuck for a lasting reason would leave its repository
// unreadable; never past one that may be filling (one download per walk) or
// one at its share. The
// keyed order's demotions apply ([Router.classes]); no copy verdict, floor or
// lag gate does ([GitCopy]). A write is asked again as a read is, being an
// [opIdempotentWrite] keyed by its push id ([opCodeGitWrite]). Nothing
// answering is [ErrUnserved].
func (r *Router) routeGit(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	w := &walk{}
	own, ownHalf := r.ownGit(w)
	budget := r.budgetFor(ctx, spec, x)
	nodes, err := r.gitPlacement.Holders()
	if err != nil {
		// A placement that cannot say who serves git is UNKNOWN, never "no
		// Gitlet node"; this node's own half needs no view, so it is the one
		// node left to ask, as its home.
		if ownHalf {
			if value, final, _, ranErr := r.runLocalGit(ctx, spec, *x.home, own, budget, w); final {
				return value, ranErr
			}
		}
		if w.fallback != nil {
			return w.fallback.value, w.fallback.err
		}
		if len(w.reasons) > 0 {
			return nil, fmt.Errorf("estate: %s: read who serves git: %w (%s)", spec.name, err,
				strings.Join(w.reasons, "; "))
		}
		return nil, fmt.Errorf("estate: %s: read who serves git: %w", spec.name, err)
	}
	order := r.keyedOrder(spec, x.key, nodes, ownHalf, x.carrying(actor))
	if len(order) == 0 {
		w.note("no node with the gitlet role is live")
	}
	// fill is the place in the order of the one node allowed to fill a copy:
	// the effective home first, the next only once the one before it said no
	// copy is coming there.
	fill := 0
	for i, node := range order {
		attempt := x
		if i == fill {
			attempt = *x.home
		}
		if node == r.self {
			value, final, passes, ranErr := r.runLocalGit(ctx, spec, attempt, own, budget, w)
			if final {
				return value, ranErr
			}
			if i == fill && passes {
				fill++
			}
			continue
		}
		rep, got, err := r.askFor(ctx, spec, actor, attempt, node, budget, false, w.spent)
		if err != nil {
			return nil, err
		}
		if !got.answered() {
			// SILENT, or not believed: it may be filling, so it keeps the
			// fill; the next walk asks it last and stamps its runner-up.
			if err := r.unheard(ctx, spec, node, got, w); err != nil {
				return nil, err
			}
			continue
		}
		if value, final, ranErr := r.settle(ctx, spec, attempt, node, rep, w); final {
			return value, ranErr
		}
		if i == fill && passesFill(rep) {
			fill++
		}
	}
	if w.fallback != nil {
		return w.fallback.value, w.fallback.err
	}
	detail := strings.Join(w.reasons, "; ")
	log.WarnContext(ctx, "estate_unserved", "op", spec.name, "detail", detail)
	return nil, w.refusedOr(&ErrUnserved{Detail: detail, NodeSilent: w.silent, Git: true})
}

// ownGit is this node's git half for one walk ([Router.gitHalf]), noted on the
// walk where this Gitlet node has none. A node with no half is left out of its
// own order rather than stamped home: it would fill nothing, and the runner-up
// behind it would be told it may not either.
func (r *Router) ownGit(w *walk) (GitServer, bool) {
	if r.localGit == nil {
		return nil, false
	}
	g, ok := r.gitHalf()
	if !ok {
		w.note("%s: does not run Gitlet", r.self)
	}
	return g, ok
}

// gitHalf is this node's git half, read ONCE A REQUEST ([LocalGit]), and false
// where Gitlet is not built here.
func (r *Router) gitHalf() (GitServer, bool) {
	if r.localGit == nil {
		return nil, false
	}
	g, ok := r.localGit.Git()
	if !ok || g == nil {
		return nil, false
	}
	return g, true
}

// ErrNoGitletNode is a fleet with no live Gitlet node, as [Router.Home] and
// [Router.Maintainer] answer it: no repository has a home or a maintainer.
var ErrNoGitletNode = errors.New("estate: no live Gitlet node")

// Home is a repository's HOME: the Gitlet node rendezvous ranks first over
// every live Gitlet node (this one where its half is built) WITH NO DEMOTION,
// the git walk's own order ([Router.keyed]) before the router's memory
// reorders it, so every node computes the same one. A home that went quiet is
// still the home: its work waits for it rather than moving to a node that
// would then do it too. Two nodes may each believe they are home (no lease,
// no epoch: ADR-0089), so a home's work must be safe done twice; the
// repository's committer takes it once. A failed read of who serves git
// answers its error, UNKNOWN, never "this node" or "nobody" (unlike the git
// walk, which asks its own half: a read is the same from any node, a home's
// work done by a node that could not tell is work two may do); a fleet listing
// no Gitlet node answers [ErrNoGitletNode].
func (r *Router) Home(repoID string) (string, error) {
	h, err := r.readHomes()
	if err != nil {
		return "", fmt.Errorf("estate: the home of repository %s: %w", repoID, err)
	}
	return h.home(r, repoID), nil
}

// HomeOf answers which of repoIDs this node is the HOME of ([Router.Home]),
// all judged against ONE READING of who serves git. An error is UNKNOWN for
// every one (unreadable, or [ErrNoGitletNode]), never "home to none", so a
// node that cannot tell does none of a home's work. No ids read nothing.
//
// One reading, because its caller, internal/gitlet/home's backstop pass every
// minute over every listed repository, would otherwise re-read the presence
// view per repository (BenchmarkHomeOf), and a pass judges all against one
// fleet. It is the home's question, not [Router.MaintainerOf]'s: the change
// work charges no background budget, and a node not yet listed is in this
// order by its own built half, as in Home.
func (r *Router) HomeOf(repoIDs []string) (map[string]bool, error) {
	if len(repoIDs) == 0 {
		return map[string]bool{}, nil
	}
	h, err := r.readHomes()
	if err != nil {
		return nil, fmt.Errorf("estate: the homes of %d repositories: %w", len(repoIDs), err)
	}
	out := map[string]bool{}
	for _, id := range repoIDs {
		if h.home(r, id) == r.self {
			out[id] = true
		}
	}
	return out, nil
}

// GitOrder is a repository's WHOLE git order: every live Gitlet node (this one
// where its half is built) in rendezvous order WITH NO DEMOTION, for
// [Router.Home]'s reason, the home first. Every node ranks the repository's
// own git work by it alike: which node claims a queued check and how long the
// others wait (internal/gitlet/checkrun), and which hold its index
// (internal/codeindex). A node absent from it is no live Gitlet node, which
// judges a running check whose runner left it. A failed read of who serves
// git is UNKNOWN; none listed is [ErrNoGitletNode]. A fresh slice every call.
func (r *Router) GitOrder(repoID string) ([]string, error) {
	h, err := r.readHomes()
	if err != nil {
		return nil, fmt.Errorf("estate: the git order of repository %s: %w", repoID, err)
	}
	return r.keyed(repoID, h.nodes, h.own), nil
}

// GitOrders is [Router.GitOrder] of every one of repoIDs off ONE READING of
// who serves git, for [Router.HomeOf]'s reason: its caller, a code search's
// scatter (internal/codesearch), asks about up to config.CodeMaxRepos
// repositories at once. An error is UNKNOWN for every one; no ids read
// nothing. Each order a fresh slice.
func (r *Router) GitOrders(repoIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(repoIDs))
	if len(repoIDs) == 0 {
		return out, nil
	}
	h, err := r.readHomes()
	if err != nil {
		return nil, fmt.Errorf("estate: the git orders of %d repositories: %w", len(repoIDs), err)
	}
	for _, id := range repoIDs {
		out[id] = r.keyed(id, h.nodes, h.own)
	}
	return out, nil
}

// homeReading is one reading of who serves git, which [Router.Home] and
// [Router.HomeOf] order over: the placement's live Gitlet nodes, and whether
// this node's own half is built, which places it in the order where the
// placement does not yet name it, so its reads are answered in process.
type homeReading struct {
	nodes []string
	own   bool
}

// readHomes reads who serves git once: the placement's live Gitlet nodes and this
// node's own half. An error is the placement's, UNKNOWN; a reading that names
// no Gitlet node at all — none listed, and no half of this node's — is
// [ErrNoGitletNode].
func (r *Router) readHomes() (homeReading, error) {
	nodes, err := r.gitPlacement.Holders()
	if err != nil {
		return homeReading{}, fmt.Errorf("read who serves git: %w", err)
	}
	_, own := r.gitHalf()
	if !own && !slices.ContainsFunc(nodes, func(n string) bool { return n != "" && n != r.self }) {
		return homeReading{}, ErrNoGitletNode
	}
	return homeReading{nodes: nodes, own: own}, nil
}

// home is repoID's home under h: the first node of the git walk's own order
// ([Router.keyed]) before the router's memory reorders it — never empty, since
// readHomes refuses a reading that names no Gitlet node.
func (h homeReading) home(r *Router, repoID string) string {
	return r.keyed(repoID, h.nodes, h.own)[0]
}

// Maintainer is the Gitlet node a repository's MAINTENANCE (pack compaction,
// layer publication) is placed on, for work charged at charge bytes of a
// node's background budget: the FIRST node of the home's order ([Router.keyed])
// whose ADVERTISED budget holds charge, and the HOME where none does. One
// listing answers it all ([GitPlacement.Listing]), and a node it does not
// carry, this one included, is GONE, unlike in [Router.Home]: its peers cannot
// see it. A runner-up NOT SAYING is passed over (its cold sync would end in its
// own admission's refusal); a home not saying keeps its repositories, whose
// copies it holds. No demotion, as in Home; two nodes may each take themselves
// for maintainer, so its work must be safe twice: the committer takes it once.
// An unreadable listing is UNKNOWN; none listed is [ErrNoGitletNode] (package
// doc, "A repository's git work is asked of its home Gitlet node").
func (r *Router) Maintainer(repoID string, charge int64) (string, error) {
	listing, err := r.gitPlacement.Listing()
	if err != nil {
		return "", fmt.Errorf("estate: the maintainer of repository %s: read who serves git and what each "+
			"Gitlet node can give its background work: %w", repoID, err)
	}
	maintainer, err := r.maintainerOver(listing, repoID, charge)
	if err != nil {
		return "", fmt.Errorf("estate: the maintainer of repository %s: %w", repoID, err)
	}
	return maintainer, nil
}

// MaintainerOf answers which of the repositories charges names (each id mapped
// to its work's charge, [Router.Maintainer]) this node MAINTAINS, all judged
// against ONE LISTING. An error is UNKNOWN for every one (unreadable, or
// [ErrNoGitletNode]), never "maintains none", so a node that cannot tell does
// none of a maintainer's work. No ids read nothing. One listing for
// [Router.HomeOf]'s reason: internal/gitlet/compact's passes ask it of every
// due repository every minute on every Gitlet node (BenchmarkMaintainerOf).
func (r *Router) MaintainerOf(charges map[string]int64) (map[string]bool, error) {
	if len(charges) == 0 {
		return map[string]bool{}, nil
	}
	listing, err := r.gitPlacement.Listing()
	if err != nil {
		return nil, fmt.Errorf("estate: the maintainers of %d repositories: read who serves git and what each "+
			"Gitlet node can give its background work: %w", len(charges), err)
	}
	out := map[string]bool{}
	for id, charge := range charges {
		maintainer, err := r.maintainerOver(listing, id, charge)
		if err != nil {
			return nil, fmt.Errorf("estate: the maintainers of %d repositories: %w", len(charges), err)
		}
		if maintainer == r.self {
			out[id] = true
		}
	}
	return out, nil
}

// maintainerOver is repoID's maintainer under ONE listing ([Router.Maintainer]'s
// rule), through which [Router.Maintainer] and [Router.MaintainerOf] both
// judge, so the two can never differ. No Gitlet node is [ErrNoGitletNode].
func (r *Router) maintainerOver(listing GitListing, repoID string, charge int64) (string, error) {
	order := r.keyed(repoID, listing.Nodes, slices.Contains(listing.Nodes, r.self))
	if len(order) == 0 {
		return "", ErrNoGitletNode
	}
	for i, node := range order {
		budget, said := listing.Budgets[node]
		switch {
		case said && budget >= charge:
			return node, nil
		case !said && i == 0:
			// THE HOME, NOT SAYING, keeps its repositories; a runner-up not
			// saying is passed over.
			return node, nil
		}
	}
	return order[0], nil
}

// Listed reports whether this node is among the Gitlet nodes its git listing
// carries ([GitPlacement.Listing]), the only nodes [Router.Maintainer] answers:
// (true, nil), (false, nil), or (false, err) where the listing cannot be read.
// A maintainer's loops wait for it before their first pass
// (internal/gitlet/compact), since a booting node is listed only once its host
// beats. The listing, never this node's built half, which places it for
// [Router.Home] alone: its peers cannot see a node no listing carries.
func (r *Router) Listed() (bool, error) {
	listing, err := r.gitPlacement.Listing()
	if err != nil {
		return false, fmt.Errorf("estate: whether this node is listed among the Gitlet nodes: %w", err)
	}
	return slices.Contains(listing.Nodes, r.self), nil
}

// runLocalGit runs one git operation in process on this node's own git half,
// x stamped for this node's place in the walk. final is set when that is the
// answer; otherwise the walk notes why and moves on, remembering nothing (no
// round trip to save), and passes says the fill passes on ([passesFill]). It
// is held to the attempt a remote asker gives ([Router.budgetFor]), served
// until a reply's margin before it ([gitlet.AnswerBy]): what a read or a write
// is answered must not depend on which node asked it.
func (r *Router) runLocalGit(ctx context.Context, spec *opSpec, x exchange, g GitServer, budget time.Duration,
	w *walk) (value any, final, passes bool, err error) {

	attempt, cancel := attemptContext(ctx, budget)
	defer cancel()
	served, stop := gitlet.AnswerBy(attempt, deadlineOf(attempt))
	defer stop()
	value, err = x.local(served, Backend{Git: g, ServerSeams: r.seams})
	switch {
	case errors.Is(err, errNoHalf):
		w.note("%s", ranNothingDetail(r.self, spec.name, err))
		return nil, false, true, nil
	case errors.Is(err, errGitUnready):
		w.note("%s: %s", r.self, unreadyDetail(err))
		return nil, false, unreadyReason(err).PassesFill(), nil
	}
	if unread := unreadHere(spec, x, r.self, err); unread != nil {
		// A build that does not know a value the request names ran nothing of
		// it, in process as on the wire ([unreadHere]): the fill passes on.
		w.unreadBy(r.self, unread)
		return nil, false, true, nil
	}
	return value, true, false, err
}

// answerGit is the server's GIT ARM: a git operation past the envelope's
// admission and the lane check, answered from this node's git half with no
// copy verdict, floor or lag gate ([GitCopy]). A node without Gitlet answers
// `no_backend`, never silence (read as a node gone); a half not ready answers
// `behind` with its reason ([reply.Unready]), `behind_config` for the node's
// posture. Served until its answer is due ([gitlet.AnswerBy]): due, its asker's
// wait counted from arrival on this node's clock ([server.answer]), every wait
// taking that context (statelog.WithReplyMargin binds only statelog's), so a
// busy node's `behind` arrives while its asker listens, never read as gone. A
// write past [gitWritesHeld] is answered `behind` at its share at once, before
// decoding and without demoting the node, never left to the broker client's
// buffer, which drops a request past its bytes.
func (s server) answerGit(ctx context.Context, spec *opSpec, req request, due time.Time, out reply) []byte {
	ctx, cancel := gitlet.AnswerBy(ctx, due)
	defer cancel()
	var g GitServer
	ok := false
	if s.git != nil {
		g, ok = s.git.Git()
	}
	if !ok || g == nil {
		out.Unserved, out.Detail = unservedNoBackend, fmt.Sprintf("%s does not run Gitlet", s.self)
		return s.encodeReply(out)
	}
	if spec.lane == laneGitWrites {
		select {
		case s.writes <- struct{}{}:
			defer func() { <-s.writes }()
		default:
			out.Unserved, out.Unready = unservedBehind, gitlet.UnreadyShare
			out.Detail = fmt.Sprintf("%s holds %d engine writes for its peers, every one its writes' lane holds "+
				"at once (gitWritesHeld): it builds another when one of them ends", s.self, gitWritesHeld)
			return s.encodeReply(out)
		}
	}
	// Read whole or not run ([inputs]), after the held-writes check so a
	// write answered at once is never decoded.
	args, actor, refused, err := inputs(spec, req)
	switch {
	case err != nil:
		out.Err = encodeError(err)
		return s.encodeReply(out)
	case refused != nil:
		return s.refuseUnknown(ctx, out, req, refused)
	}
	result, err := spec.run(ctx, Backend{Git: g, ServerSeams: s.seams}, actor, args)
	switch {
	case errors.Is(err, errNoHalf):
		out.Unserved, out.Detail = unservedNoBackend, ranNothingDetail(s.self, req.Op, err)
		return s.encodeReply(out)
	case errors.Is(err, errGitUnready):
		why := unreadyReason(err)
		out.Unserved, out.Detail, out.Unready = unservedBehind, unreadyDetail(err), why
		if why == gitlet.UnreadyBehindConfig {
			// The node's posture, said as every operation says it, so an asker
			// remembers it for this node's data operations too
			// ([Router.markBehindConfig]).
			out.Unserved = unservedBehindConfig
		}
		return s.encodeReply(out)
	}
	return s.answered(out, spec, result, err)
}
