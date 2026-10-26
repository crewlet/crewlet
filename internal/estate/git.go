package estate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REPOSITORY'S GIT WORK IS ASKED OF ITS HOME GIT NODE (ADR-0059).
//
// An operation on a repository's git objects — a server-side read by object id
// today — is answered by a node with the `git` role from its own cache, never
// by a data node's copy. Asked of whichever node the ordinary walk picked, a
// seat on a node without the git role would have every git node warm every
// repository it ever read, a cold sync paid per node per repository; so it is
// asked of the GIT nodes in rendezvous.Order(repository, every live git node)
// — the repository's HOME first, its runner-up next — the order every node
// computes alike from the same set, so the reads of one repository meet on one
// cache. The home is a PREFERENCE, never ownership: no lease and no epoch, and
// two nodes that both believe they are home answer the same immutable read.

// GitCopy is the copy a GIT operation names ([op.keyedOnGit]): no domain's
// rows on the serving node, but its git half — its cache, synced through the
// router like any reader's. So no copy verdict holds it back, no floor is
// waited for at dispatch (its floor travels in its arguments, as
// code.GitQuery.Fresh, and is the cache's sync level), and no lag gate
// applies: a git node whose own copy of the code log is out of service still
// serves git reads, from a cache it syncs from a peer. Like [SeatCopy], no
// data operation may name it (TestEveryOperationNamesTheCopyItReads).
const GitCopy = "git"

// LocalGit is this node's git half, as the router and the server ask for it —
// resolved PER REQUEST, because the code host (and with it the git half) is
// built at boot or at the first apply that sets code.native, after the router
// and the server exist; one captured at either's construction would answer
// "no code host" for the life of a node that applied its company later.
type LocalGit interface {
	// Git is this node's git server, or false while this node serves none:
	// its code host is not built — a company that hosts no code, or not
	// yet.
	Git() (GitServer, bool)
}

// GitServer is the git half a git node serves, declared here by its consumer
// with exactly the methods the git operations call. internal/code/query's
// *Server satisfies it.
type GitServer interface {
	Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
}

// GitReadAttempt bounds one git read's wait on one git node: [Production]'s
// [Timing.GitReadAttempt].
//
// SEVENTEEN SECONDS: the git node's waits at their WHOLE budgets and the
// reply's margin, a sum of budgets as every attempt here is, never of p99s —
//
//	code.GitShareWait          2 s  a free unit of the node's seat git-work share
//	+ statelog.ReadBudget      2 s  the node's cache synced to the asker's floor
//	+ statelog.ReadBudget      2 s  one linearizable re-sync of an id the copy lacks
//	+ gitproc.ReadDeadline    10 s  the read's own git process
//	+ code.ReplyMargin         1 s  the answer's trip back
//
// A node that cannot fit a wait answers `behind` inside it (its reads bound
// every wait by the request's deadline less the margin), so a shorter attempt
// only hears a slow git node give up sooner; a longer one would leave a node
// that is gone to be waited out longer. BenchmarkCodeRead (internal/code/query)
// confirms the TERMS — every warm kind's p99 far inside gitproc.ReadDeadline
// with its sync — and never sets the sum: a term it misses is re-derived and
// the sum follows. TestProductionsGitReadAttemptHoldsAGitReadsWaits holds it.
const GitReadAttempt = code.GitShareWait + 2*statelog.ReadBudget + gitproc.ReadDeadline + code.ReplyMargin

// errGitUnready is a git half that could not answer a read inside its budget
// for a reason about THE NODE (code.ErrGitUnready: its copy still syncing, its
// share full, no chart or no configuration to judge a seat by, a failure of
// its own, or no copy held and no fill asked of it): it ran nothing about the
// repository's content, and another git node may answer. The server and the
// router's own in-process run answer it `behind` — `behind_config` for that
// reason, the node's posture — with the reason beside it ([reply.Unready]),
// which the walk moves on from WITHOUT demoting the node — a node syncing a
// repository is the right node to ask a moment later — and reads to know
// whether the one node it lets fill a copy is still that one.
var errGitUnready = errors.New("estate: this git node cannot answer the read now")

// unreadyGit is a git half's code.ErrGitUnready as the walk reads it: errGitUnready,
// its cause kept for the walk's account.
type unreadyGit struct{ cause error }

func (u unreadyGit) Error() string   { return u.cause.Error() }
func (u unreadyGit) Unwrap() []error { return []error{errGitUnready, u.cause} }

// gitUnready is a git half's error as the walk reads it: code.ErrGitUnready
// becomes [errGitUnready], and every other error is the answer — a refusal
// about the read or the repository (code.ErrNotPermitted, code.ErrObjectNotFound,
// code.ErrRepoNotFound, …), which every git node would give alike.
func gitUnready(err error) error {
	var unready *code.ErrGitUnready
	if errors.As(err, &unready) {
		return unreadyGit{cause: err}
	}
	return err
}

// unreadyDetail is what a node's answer says of a git half that was not ready:
// the half's own sentence, which names the node and what it is doing.
func unreadyDetail(err error) string {
	var unready *code.ErrGitUnready
	if errors.As(err, &unready) {
		return unready.Detail
	}
	return err.Error()
}

// unreadyReason is the reason a git half's code.ErrGitUnready gave, and empty
// for an error that carries none — which passes no fill
// (code.UnreadyReason.PassesFill).
func unreadyReason(err error) code.UnreadyReason {
	var unready *code.ErrGitUnready
	if errors.As(err, &unready) {
		return unready.Reason
	}
	return ""
}

// passesFill reports whether a git node's reply — it ran nothing — says that
// no copy serving the read is coming on that node, so a walk that let it fill
// lets the next node fill in its place: a node that ran nothing for the
// operation at all (no code host built, a key it does not hold, a request it
// would not take), and one whose git half said why in a reason that passes —
// behind its configuration, a failure of its own. A node that may be bringing
// a copy up keeps the fill — syncing, its chart moments away, a reason this
// build does not know, a reply that says nothing of why — and so does one at
// its share, which holds its copy and is only busy.
func passesFill(rep reply) bool {
	switch rep.Unserved {
	case unservedNoBackend, unservedUnauthenticated, unservedStale:
		return true
	case unservedBehind, unservedBehindConfig:
		return rep.Unready.PassesFill()
	}
	return false
}

// routeGit runs one GIT operation ([op.keyedOnGit]): the git nodes in the
// repository's rendezvous order, the ONE node the walk lets FILL a copy asked
// with its arguments stamped home ([exchange.home]) and every other node with
// them stamped not.
//
// THE FILL STARTS AT THE WALK'S EFFECTIVE HOME — its first node once the
// demotions are applied — AND MOVES ON ONLY PAST A NODE BRINGING NO COPY UP
// for the read: one that ran nothing for the operation at all, and one whose
// git half said why in a reason that passes — behind its configuration, which
// judges no seat for as long as its apply fails, or a failure of its own
// (code.UnreadyReason.PassesFill, [passesFill]). Kept on the first node, a
// home that could not serve the read for a reason that lasts left the
// repository unreadable through the walk: every other node holding no copy
// answers a read not stamped home `not_held`, so a seat's read of a
// repository its stuck home alone held was answered by nobody for as long as
// the home stayed stuck. And never moved past a node that may be FILLING —
// syncing, its chart moments away, a reason this build does not know, a node
// gone silent — so one walk still starts one download at a time, and a home's
// death costs the fleet one fill rather than one a git node; nor past one at
// its SHARE, which holds its copy and is only busy, since the share is the
// node's and not the repository's (code.UnreadyReason.PassesFill). The home
// itself is not demoted for it: a person's read, which judges no chart, and
// the next walk still ask it first.
//
// THE WALK OF EVERY KEYED ORDER, AND NOTHING OF THE DATA WALK'S PREFERENCES:
// a node that ran nothing for the operation (`no_backend`: no code host) is
// asked last for [suspectFor] and a silent one last of all, through the same
// memory every keyed order reads ([Router.classes]); but no copy verdict, no
// floor and no lag gate applies to a git operation ([GitCopy]), so the walk
// never consults [Router.remembersBehind] and never hands a node to
// [Router.lastResort]. A git node behind the company's configuration is no
// worse a git node: its git half answers a seat's read `behind_config` itself
// (code.UnreadyBehindConfig), which the walk reads as it reads any node that
// ran nothing — move on, no demotion, the fill passed on.
//
// Nothing answering is [ErrUnserved], as every walk.
func (r *Router) routeGit(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	w := &walk{}
	own, ownHalf := r.ownGit(w)
	budget := r.budgetFor(ctx, spec)
	nodes, err := r.gitPlacement.Holders()
	if err != nil {
		// A PLACEMENT THAT CANNOT SAY WHO SERVES GIT names no peer to ask
		// — UNKNOWN, never "no git node" — and takes nothing from this
		// node's own git half, which needs no view to answer: with no node
		// named to order, it is the one node left to ask, as its home.
		if ownHalf {
			if value, final, _, ranErr := r.runLocalGit(ctx, spec, *x.home, own, w); final {
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
	order := r.keyedOrder(spec, x.key, nodes, ownHalf)
	if len(order) == 0 {
		w.note("no node with the git role is live")
	}
	// fill is the place in the order of the one node the walk lets fill a
	// copy: the EFFECTIVE HOME first — the order after its demotions, so a
	// home that went silent or ran nothing earlier is passed over and its
	// runner-up is the one asked — and the next node only once the one
	// before it said no copy is coming there.
	fill := 0
	for i, node := range order {
		attempt := x
		if i == fill {
			attempt = *x.home
		}
		if node == r.self {
			value, final, passes, ranErr := r.runLocalGit(ctx, spec, attempt, own, w)
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
			// SILENT, or not believed: it may be filling the copy it was
			// asked to, so it keeps the fill. The router asks it last from
			// the next walk on, whose first node — its runner-up — is
			// stamped instead.
			if err := r.unheard(ctx, spec, node, got, w); err != nil {
				return nil, err
			}
			continue
		}
		if value, final, ranErr := r.settle(spec, attempt, node, rep, w); final {
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
	return nil, &ErrUnserved{Detail: detail, NodeSilent: w.silent, Git: true}
}

// ownGit is this node's git half for one walk ([Router.gitHalf]), noted on the
// walk where this node is a git node with none, so a walk nobody served says
// why its own node ran nothing. A node with no git half is left out of its own
// order rather than stamped home: it would fill nothing, and the runner-up
// behind it would be told it may not either.
func (r *Router) ownGit(w *walk) (GitServer, bool) {
	if r.localGit == nil {
		return nil, false
	}
	g, ok := r.gitHalf()
	if !ok {
		w.note("%s: runs no code host", r.self)
	}
	return g, ok
}

// gitHalf is this node's git half, read ONCE A REQUEST ([LocalGit]): the
// server and true where this node is a git node whose code host is built, and
// false otherwise.
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

// ErrNoGitNode is a fleet with no live git node, as [Router.Home] answers it:
// no node serves a repository's git, so it has no home.
var ErrNoGitNode = errors.New("estate: no live git node")

// Home is a repository's HOME: the git node rendezvous ranks first for it over
// every live git node — this one among them where its git half is built — WITH
// NO DEMOTION. It is the first node of the git walk's own order
// ([Router.keyed], one function for both) before the router's memory reorders
// it, so on a quiet fleet the node a repository's git reads are asked of first
// and the node its own git work falls to are one node, and every node computes
// the same one from the same set.
//
// NO DEMOTION, because work that is the home's own is not a read the walk
// routes around a slow node: a home that went quiet, or declined a read, is
// still the home, and its work waits for it rather than moving to a node that
// would then do it too. A presence view that disagrees between two nodes can
// make each believe it is home — no lease and no epoch decide it (ADR-0059) —
// so whatever is done as home must be safe done twice: a home's work is
// decided by the repository's committer, which takes it once.
//
// A READ OF WHO SERVES GIT THAT FAILS ANSWERS ITS ERROR — UNKNOWN, never "this
// node" and never "nobody" — where the git walk asks this node's own half as
// the one node left: a read of an object id is the same read from any node,
// and a home's work done by a node that could not tell is work two nodes may
// do. A fleet that lists no git node answers [ErrNoGitNode].
func (r *Router) Home(repoID string) (string, error) {
	nodes, err := r.gitPlacement.Holders()
	if err != nil {
		return "", fmt.Errorf("estate: the home of repository %s: read who serves git: %w", repoID, err)
	}
	_, own := r.gitHalf()
	order := r.keyed(repoID, nodes, own)
	if len(order) == 0 {
		return "", fmt.Errorf("estate: the home of repository %s: %w", repoID, ErrNoGitNode)
	}
	return order[0], nil
}

// IsHome reports whether this node is repoID's home ([Router.Home]): (true,
// nil), (false, nil), or (false, err) where who serves git cannot be read —
// three facts, so a node that cannot tell does none of a home's work rather
// than guess.
func (r *Router) IsHome(repoID string) (bool, error) {
	home, err := r.Home(repoID)
	if err != nil {
		return false, err
	}
	return home == r.self, nil
}

// runLocalGit runs one git operation in-process on this node's own git half,
// x already stamped for this node's place in the walk. final is set when that
// is the answer; otherwise the walk says why not and moves on — as a remote
// git node's `no_backend` and `behind` are read, with nothing remembered,
// since asking this node's own half again costs no round trip — and passes
// says whether no copy serving the read is coming here, so the walk lets the
// next node fill in this one's place ([passesFill]'s rule).
func (r *Router) runLocalGit(ctx context.Context, spec *opSpec, x exchange, g GitServer,
	w *walk) (value any, final, passes bool, err error) {

	value, err = x.local(ctx, Backend{Git: g, ServerSeams: r.seams})
	switch {
	case errors.Is(err, errNoHalf):
		w.note("%s", ranNothingDetail(r.self, spec.name, err))
		return nil, false, true, nil
	case errors.Is(err, errGitUnready):
		w.note("%s: %s", r.self, unreadyDetail(err))
		return nil, false, unreadyReason(err).PassesFill(), nil
	}
	return value, true, false, err
}

// answerGit is the server's GIT ARM: a git operation that passed the
// envelope's admission and the lane check, answered from this node's git half
// — never held to a copy's verdict, a floor or the lag gate, none of which
// applies to [GitCopy]. A node whose code host is not built answers
// `no_backend`, never silence, which its asker would read as a node gone; a
// half not ready answers `behind`, naming why and carrying its reason
// ([reply.Unready]) — `behind_config` for a seat's read on a node behind the
// company's configuration, the node's posture as every operation says it.
func (s server) answerGit(ctx context.Context, spec *opSpec, req request, out reply) []byte {
	ctx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()
	ctx = statelog.WithReplyMargin(ctx, code.ReplyMargin)
	var g GitServer
	ok := false
	if s.git != nil {
		g, ok = s.git.Git()
	}
	if !ok || g == nil {
		out.Unserved, out.Detail = unservedNoBackend, fmt.Sprintf("%s runs no code host", s.self)
		return s.encodeReply(out)
	}
	result, err := spec.serve(ctx, Backend{Git: g, ServerSeams: s.seams}, req.Actor, req.Args)
	switch {
	case errors.Is(err, errNoHalf):
		out.Unserved, out.Detail = unservedNoBackend, ranNothingDetail(s.self, req.Op, err)
		return s.encodeReply(out)
	case errors.Is(err, errGitUnready):
		why := unreadyReason(err)
		out.Unserved, out.Detail, out.Unready = unservedBehind, unreadyDetail(err), why
		if why == code.UnreadyBehindConfig {
			// THE NODE'S POSTURE, said as every operation says it, so an
			// asker remembers it for this node's data operations too
			// ([Router.markBehindConfig]).
			out.Unserved = unservedBehindConfig
		}
		return s.encodeReply(out)
	}
	return s.answered(out, req.Op, result, err)
}
