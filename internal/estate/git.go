package estate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REPOSITORY'S GIT WORK IS ASKED OF ITS HOME GIT NODE (ADR-0059).
//
// An operation on a repository's git objects — a server-side read by object
// id, and an engine write built beside a copy of them (ADR-0062) — is answered
// by a node with the `git` role from its own cache, never by a data node's
// copy. Asked of whichever node the ordinary walk picked, a seat on a node
// without the git role would have every git node warm every repository it ever
// read or wrote, a cold sync paid per node per repository; so it is asked of
// the GIT nodes in rendezvous.Order(repository, every live git node) — the
// repository's HOME first, its runner-up next — the order every node computes
// alike from the same set, so the reads and the writes of one repository meet
// on one cache. The home is a PREFERENCE, never ownership: no lease and no
// epoch, and two nodes that both believe they are home answer the same
// immutable read alike, and build the same write as the same push, which the
// repository's committer lands once.

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
// with exactly the methods the git operations call: its reads by object id,
// which internal/code/query's *Server answers, and its engine writes, which
// internal/code/host's *Server builds and lands. The engine hands the router
// the two together, over one cache, one git pool and one seat git-work share.
type GitServer interface {
	// Read answers one read by object id. ctx ENDS WHEN THE ANSWER IS DUE —
	// a reply's margin before its asker stops listening, remote or in
	// process alike ([code.AnswerBy]) — and a read that ctx ends that way
	// answers code.ErrGitUnready while the asker still listens
	// ([code.AnswerDue]), never ctx's own error, which the walk would take
	// for the repository's answer.
	Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)

	// Write builds one engine write over this node's copy and lands it as a
	// push (ADR-0062). ctx ends when the answer is due, as a read's does: a
	// write that ctx ends before its landing ran nothing and answers
	// code.ErrGitUnready while the asker still listens, so the walk asks the
	// next git node, which builds the same push; one ended inside its
	// landing answers what the landing came to, its outcome read from the
	// push's row — never ctx's own error, which the walk would take for the
	// write's answer.
	Write(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error)
}

// GitPlacement is who serves GIT as the router reads it ([RouterOptions.Git]):
// every live node with the `git` role, as [Placement] names them — what the
// git walk and [Router.Home] order — and, for the question that weighs the git
// nodes against each other, ONE LISTING of them with what each ADVERTISES it
// can give its background work ([GitPlacement.Listing]), which
// [Router.Maintainer] places a repository's maintenance by. Declared here by
// its consumer; the engine answers both from its watched presence view.
type GitPlacement interface {
	Placement

	// Listing is the git nodes and what each advertises, off ONE listing of
	// the fleet: the nodes Holders would name at that listing, and what each
	// one's row says beside them ([GitListing]). One read for the whole
	// question, so a node is weighed by the very listing that names it —
	// never ordered off one listing and weighed off the next a refresh put
	// in its place, where a node the first named and the second did not
	// would read as one that said nothing. An error is UNKNOWN: the view
	// could not be read, never "no git node".
	Listing() (GitListing, error)
}

// GitListing is one listing of the fleet's git nodes, as
// [GitPlacement.Listing] answers it.
type GitListing struct {
	// Nodes is every git node the listing carries.
	Nodes []string

	// Budgets is what each node's presence row says it can give its
	// background work — its maintenance budget, which a git node's row
	// carries on every beat beside its status and never in it
	// (coord.GitBackgroundKey) — where the row says one: zero included, a
	// node saying it has nothing to give (no code host built yet, or
	// transfers that keep all of its memory). A node of Nodes with no entry
	// is NOT SAYING: its row carries no figure (a build or a host that
	// writes none), never one with nothing to give. A git node of this build
	// always says it, a beat whose status read overran included.
	Budgets map[string]int64
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
//	+ gitproc.ReadDeadline    10 s  the read's OWN GIT: the run time every process
//	                                of its kind shares — a listing's one, a
//	                                comparison's three
//	+ code.ReplyMargin         1 s  the answer's trip back
//
// The git node is served under a context that ends a reply's margin before
// the attempt does (code.AnswerBy), and EVERY wait it makes takes that
// context, so a node that cannot fit a wait answers `behind` while its asker
// still listens: a shorter attempt only hears a slow git node give up sooner;
// a longer one would leave a node that is gone to be waited out longer.
//
// WHAT THE SUM LEAVES OUT, on purpose: the read lane's queue before each of
// the read's processes (up to gitproc.SlotWait each, FIFO behind a person's
// push scans), and the short processes that find what a read names before
// its own git runs (rev-parse --disambiguate and cat-file --batch-check, each
// bounded by gitproc.ReadDeadline and milliseconds on a healthy node). Summed
// at their whole bounds they would make the attempt minutes long, and a dead
// node would be waited out that long. Left out, they come out of the attempt:
// a read they hold up past its answer's due instant is answered unready — the
// node's capacity, `share` — and the walk asks the next git node; never late,
// and never refused as too large, which only a read whose own git ran the
// whole of its budget is (internal/code/query).
//
// BenchmarkCodeRead (internal/code/query) confirms the TERMS — every warm
// kind's p99 far inside gitproc.ReadDeadline with its sync — and never sets
// the sum: a term it misses is re-derived and the sum follows.
// TestProductionsGitReadAttemptHoldsAGitReadsWaits holds it.
const GitReadAttempt = code.GitShareWait + 2*statelog.ReadBudget + gitproc.ReadDeadline + code.ReplyMargin

// GitWriteAttempt bounds one engine write's wait on one git node:
// [Production]'s [Timing.GitWriteAttempt].
//
// EIGHTY-TWO SECONDS: the git node's waits for one write at their WHOLE
// budgets, a sum of budgets as every attempt here is, never of p99s —
//
//	code.GitShareWait             2 s  a free unit of the node's seat git-work
//	                                   share, which the build holds
//	+ 3 × statelog.ReadBudget     6 s  three different waits, at most one
//	                                   each: the copy's sync to the caller's
//	                                   floor, the read of whether a head that
//	                                   moved is the write's own push, and the
//	                                   one sync at the log's head every
//	                                   refusal is judged again at
//	+ code.WriteBuildDeadline    10 s  the build's own git: the run time
//	                                   every process of it shares
//	+ code.WriteUploadAllowance  30 s  the bundle's upload, a mebibyte at the
//	                                   pace floor (objstore.MiBPace)
//	+ minWriteAttempt            33 s  the proposal's own attempt, which the
//	                                   committer answers busy with any less
//	                                   (code.MinProposeAttempt)
//	+ code.ReplyMargin            1 s  the answer's trip back
//
// THE GIT NODE FITS EVERY STAGE INTO IT. It is served under a context that
// ends a reply's margin before the attempt does (code.AnswerBy), and uploads a
// write's bundle only while the upload's allowance and the proposal's attempt
// are both still left — answering busy, nothing uploaded, where they are not
// (internal/code/host's deadline rule) — so a write's median bundle of a few
// kilobytes is never uploaded late, and a large one on a bucket at the floor
// is refused before its upload rather than landed after its asker gave up.
// So a healthy git node always answers inside the attempt, and only a WEDGED
// one costs it whole: a node that is GONE costs none, nothing serving its
// subject, and the broker's ask ends at once. The walk then asks the runner-up
// the same write, which lands the same push, or is answered from the row the
// wedged node's landing wrote ([opCodeGitWrite]).
//
// AND IT HOLDS WHATEVER THE CALLER ALLOWS ([Router.budgetFor]), unlike a
// write on a data node: a turn's deadline minutes away, waited out whole on a
// wedged home, would leave the runner-up never asked.
//
// PROVISIONAL (M3-decisions M3-O43): BenchmarkEngineWrite (internal/code/host)
// reports each term's p99 beside its budget and re-derives any term it
// contradicts, the sum following by its formula — never set by a figure
// itself. TestProductionsGitWriteAttemptHoldsAWritesWaits holds the sum.
const GitWriteAttempt = code.GitShareWait + 3*statelog.ReadBudget + code.WriteBuildDeadline +
	code.WriteUploadAllowance + minWriteAttempt + code.ReplyMargin

// errGitUnready is a git half that could not answer a read or build a write
// inside its budget for a reason about THE NODE (code.ErrGitUnready: its copy
// still syncing, its share full, no chart or no configuration to judge a seat
// by, a failure of its own, or no copy held and no fill asked of it): it ran
// nothing about the repository's content, and another git node may answer.
// The server and the router's own in-process run answer it `behind` —
// `behind_config` for that reason, the node's posture — with the reason beside
// it ([reply.Unready]), which the walk moves on from WITHOUT demoting the node
// — a node syncing a repository is the right node to ask a moment later — and
// reads to know whether the one node it lets fill a copy is still that one.
var errGitUnready = errors.New("estate: this git node cannot answer now")

// unreadyGit is a git half's code.ErrGitUnready as the walk reads it: errGitUnready,
// its cause kept for the walk's account.
type unreadyGit struct{ cause error }

func (u unreadyGit) Error() string   { return u.cause.Error() }
func (u unreadyGit) Unwrap() []error { return []error{errGitUnready, u.cause} }

// gitUnready is a git half's error as the walk reads it: code.ErrGitUnready
// becomes [errGitUnready], and every other error is the answer — a refusal
// about the read, the write or the repository (code.ErrNotPermitted,
// code.ErrObjectNotFound, code.ErrInvalid, …), which every git node would give
// alike.
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

// routeGit runs one GIT operation ([op.keyedOnGit]) — a read by object id or
// an engine write: the git nodes in the repository's rendezvous order, the ONE
// node the walk lets FILL a copy asked with its arguments stamped home
// ([exchange.home]) and every other node with them stamped not.
//
// THE FILL STARTS AT THE WALK'S EFFECTIVE HOME — its first node once the
// demotions are applied — AND MOVES ON ONLY PAST A NODE BRINGING NO COPY UP
// for the operation: one that ran nothing for it at all, and one whose git
// half said why in a reason that passes — behind its configuration, which
// judges no seat for as long as its apply fails, or a failure of its own
// (code.UnreadyReason.PassesFill, [passesFill]). Kept on the first node, a
// home that could not serve the read for a reason that lasts left the
// repository unreadable through the walk: every other node holding no copy
// answers a read not stamped home `not_held`, so a seat's read of a
// repository its stuck home alone held was answered by nobody for as long as
// the home stayed stuck — and a seat's write likewise unbuilt. And never moved
// past a node that may be FILLING —
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
// worse a git node: its git half answers a seat's read or write
// `behind_config` itself (code.UnreadyBehindConfig), which the walk reads as
// it reads any node that ran nothing — move on, no demotion, the fill passed
// on.
//
// A WRITE IS ASKED AGAIN AS A READ IS — of the next node after one that ran
// nothing and after one that went silent — because it is an
// [opIdempotentWrite] keyed by its push id ([opCodeGitWrite]): the next node
// builds the same push, and is answered from its row where a silent node
// landed it.
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

// ErrNoGitNode is a fleet with no live git node, as [Router.Home] and
// [Router.Maintainer] answer it: no node serves a repository's git, so it has
// no home and nobody to maintain it.
var ErrNoGitNode = errors.New("estate: no live git node")

// Home is a repository's HOME: the git node rendezvous ranks first for it over
// every live git node — this one among them where its git half is built — WITH
// NO DEMOTION. It is the first node of the git walk's own order
// ([Router.keyed], one function for both) before the router's memory reorders
// it, so on a quiet fleet the node a repository's git reads and engine writes
// are asked of first and the node its own git work falls to are one node, and
// every node computes the same one from the same set.
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
	h, err := r.readHomes()
	if err != nil {
		return "", fmt.Errorf("estate: the home of repository %s: %w", repoID, err)
	}
	return h.home(r, repoID), nil
}

// HomeOf answers which of repoIDs this node is the HOME of ([Router.Home]),
// every one judged against ONE READING of who serves git: the set holds
// exactly the ids this node is home for. An error is UNKNOWN for every one of
// them — who serves git could not be read, or it names no git node
// ([ErrNoGitNode]) — never "home to none", so a node that cannot tell does
// none of a home's work rather than guess. No ids read nothing.
//
// ONE READING FOR MANY REPOSITORIES, because what reads it — the backstop pass
// of the home's change work (internal/code/home), over every repository its
// workers' listings name, every minute on every git node — is a question about
// the whole listing: asked a repository at a time, each answer read the
// presence view again (its leases cloned under its lock, every node's profile
// decoded, the git nodes copied and sorted) before the one rendezvous order
// that is the answer, so a git node home to an eighth of ten thousand
// repositories paid all of that every minute for each of the rest, to find out
// that it is not their home. Read once, a repository costs its rendezvous
// order alone — 575 ns over eight git nodes and 1.28 µs over sixteen, 5.8 ms
// and 12.8 ms for ten thousand, the one read of the placement included
// (BenchmarkHomeOf, -benchtime 20x and one count, two E-cores of an Intel Core
// Ultra 7 265U, 2026-10-28) — and a pass judges every repository against one
// fleet rather than a fleet that moved part way through it. Two nodes whose
// readings differ may each judge themselves a repository's home, as they may a
// repository at a time: a home's work is decided by its repository's writers,
// which take it once (ADR-0059).
//
// THE HOME'S, NOT THE MAINTAINER'S ([Router.MaintainerOf]): the change work
// reads two commits' facts on a git node's read lane and asks for records —
// nothing charged to the background budget a node advertises, the one figure
// the maintainer's placement weighs — so nothing places it past the home,
// whose copy a repository's reads keep warm; and a node not yet listed among
// the git nodes is in this order by its own built half, as it is in Home's,
// rather than gone as it is from the maintainer's listing.
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

// homeReading is one reading of who serves git, as [Router.Home] and
// [Router.HomeOf] order a repository's git nodes over it: every live git node
// the placement names, and whether this node's own git half is built — which
// places it in the order where the placement does not yet name it, so its
// reads are answered in process.
type homeReading struct {
	nodes []string
	own   bool
}

// readHomes reads who serves git once: the placement's live git nodes and this
// node's own half. An error is the placement's, UNKNOWN; a reading that names
// no git node at all — none listed, and no half of this node's — is
// [ErrNoGitNode].
func (r *Router) readHomes() (homeReading, error) {
	nodes, err := r.gitPlacement.Holders()
	if err != nil {
		return homeReading{}, fmt.Errorf("read who serves git: %w", err)
	}
	_, own := r.gitHalf()
	if !own && !slices.ContainsFunc(nodes, func(n string) bool { return n != "" && n != r.self }) {
		return homeReading{}, ErrNoGitNode
	}
	return homeReading{nodes: nodes, own: own}, nil
}

// home is repoID's home under h: the first node of the git walk's own order
// ([Router.keyed]) before the router's memory reorders it — never empty, since
// readHomes refuses a reading that names no git node.
func (h homeReading) home(r *Router, repoID string) string {
	return r.keyed(repoID, h.nodes, h.own)[0]
}

// Maintainer is the git node a repository's MAINTENANCE is placed on — the
// compaction of its packs, the publication of its layers — for work charged at
// charge bytes of a node's background budget: the FIRST node of the
// repository's git order that can hold it, and the HOME where none can.
//
// ONE LISTING ANSWERS THE WHOLE QUESTION ([GitPlacement.Listing]): the order
// is the home's — rendezvous over the git nodes, [Router.keyed], the function
// [Router.Home] orders by — over the nodes that one listing carries, and every
// node of it is weighed by what that same listing says of it, its leases read
// once. So a node is never ordered off one listing and weighed off the next a
// refresh put in its place, where a node the first named and the second did
// not would read as one not saying and, as the home, keep its repositories'
// work on every node reading in that window. And a node the listing does not
// carry is no candidate at all — GONE, never "not saying" — this one included,
// wherever its own git half is built (unlike [Router.Home], which places this
// node by its half so that its reads are answered in process): a node that has
// given its presence back to drain, or whose row no listing has carried yet,
// is one its peers cannot see, and work it took as maintainer would be work no
// other node names it for.
//
// A NODE CAN HOLD IT where its ADVERTISED background budget is at least
// charge — the figure its own admissions hold the work to, so the node chosen
// is one that would run it. A git node says that figure on every beat, from
// memory and beside its status rather than in it (coord.GitBackgroundKey), so a
// beat whose status read overran — which drops the status whole — still says
// it.
//
// NOTHING MOVES PAST THE HOME ON MISSING INFORMATION. A node whose row carries
// no figure is NOT SAYING (a build or a host that writes none), and is no node
// known to hold the work: where it is a RUNNER-UP it is passed over. Kept in
// its place, a small runner-up not saying would take every repository it
// ranks ahead of a large node for, and its maintenance there would begin with
// a cold sync of the whole history — a runner-up holds no copy of a repository
// in the ordinary case: up to repo_max_bytes of GETs, room made for them by
// evicting what its cache serves — only for the node's own admission to refuse
// the work. Where it is the HOME it keeps its repositories, as a home that went
// quiet keeps them (Home's NO DEMOTION): the home holds the copy in the
// ordinary case, so a quiet home that cannot hold the work costs that
// repository its pass, refused at the home's admission over a copy already
// synced, while one passed over would send every repository it is home to —
// the light work every home maintains — to a runner-up's cold sync.
//
// ONE ORDER, THE HOME'S: every git node computes the same answer from the same
// listing, so on a fleet of alike nodes the maintainer IS the home and nothing
// moves, and on a fleet with one large git node every repository only it can
// hold is maintained there while every other stays with its home. NO
// DEMOTION, as in Home: maintenance is a node's own work, not a read the walk
// routes to it, so nothing the router remembers of a node's answers moves it.
// Two nodes whose views listed a heartbeat apart may each take itself for the
// maintainer — no lease and no epoch decide it (ADR-0059) — so whatever is
// done as the maintainer must be safe done twice, as a home's work is: its
// repository's committer takes it once.
//
// A LISTING THAT CANNOT BE READ ANSWERS ITS ERROR — UNKNOWN, never "this node"
// and never "nobody". A listing that carries no git node answers
// [ErrNoGitNode].
func (r *Router) Maintainer(repoID string, charge int64) (string, error) {
	listing, err := r.gitPlacement.Listing()
	if err != nil {
		return "", fmt.Errorf("estate: the maintainer of repository %s: read who serves git and what each "+
			"git node can give its background work: %w", repoID, err)
	}
	maintainer, err := r.maintainerOver(listing, repoID, charge)
	if err != nil {
		return "", fmt.Errorf("estate: the maintainer of repository %s: %w", repoID, err)
	}
	return maintainer, nil
}

// MaintainerOf answers which of the repositories charges names — each id
// mapped to the charge its work is placed by ([Router.Maintainer]) — this node
// MAINTAINS, every one judged against ONE LISTING of who serves git and what
// each git node advertises: the set holds exactly the ids this node maintains.
// An error is UNKNOWN for every one of them — the listing could not be read,
// or it carries no git node ([ErrNoGitNode]) — never "maintains none", so a
// node that cannot tell does none of a maintainer's work rather than guess. No
// ids read nothing.
//
// ONE LISTING FOR MANY REPOSITORIES, because what asks it — a maintainer's
// passes (internal/code/compact), the compaction's and the publication's, over
// every repository their due listings name, every minute on every git node — is
// a question about the whole listing: asked a repository at a time, each answer
// read the presence view again (its leases cloned under its lock, every node's
// profile decoded, the git nodes copied and sorted) before the one rendezvous
// order that is the answer, so a git node maintaining an eighth of ten
// thousand repositories paid all of that every minute for each of the rest, to
// find out that it does not maintain them — only a repository's maintainer ever
// plans it, so only its maintainer remembers a plan of it found not due, and
// every other git node judges every listed repository at every pass. Read
// once, a repository costs its rendezvous order and its nodes' budgets alone —
// 569 ns over eight git nodes and 1.11 µs over sixteen, 5.7 ms and 11.1 ms for
// ten thousand, the one read of the listing included (BenchmarkMaintainerOf,
// -benchtime 20x and one count, two E-cores of an Intel Core Ultra 7 265U,
// 2026-10-29) — and a pass judges every repository against one fleet rather
// than a fleet that moved part way through it. Two nodes whose
// listings differ may each judge themselves a repository's maintainer, as they
// may a repository at a time: its committer takes the work once (ADR-0059).
func (r *Router) MaintainerOf(charges map[string]int64) (map[string]bool, error) {
	if len(charges) == 0 {
		return map[string]bool{}, nil
	}
	listing, err := r.gitPlacement.Listing()
	if err != nil {
		return nil, fmt.Errorf("estate: the maintainers of %d repositories: read who serves git and what each "+
			"git node can give its background work: %w", len(charges), err)
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

// maintainerOver is repoID's maintainer for work charged at charge under ONE
// listing ([Router.Maintainer]'s rule): [Router.Maintainer] and
// [Router.MaintainerOf] both judge through it, so the two can never be
// computed two ways. A listing that carries no git node is [ErrNoGitNode].
func (r *Router) maintainerOver(listing GitListing, repoID string, charge int64) (string, error) {
	order := r.keyed(repoID, listing.Nodes, slices.Contains(listing.Nodes, r.self))
	if len(order) == 0 {
		return "", ErrNoGitNode
	}
	for i, node := range order {
		budget, said := listing.Budgets[node]
		switch {
		case said && budget >= charge:
			return node, nil
		case !said && i == 0:
			// THE HOME, NOT SAYING, keeps its repositories: nothing moves
			// past it on missing information. A runner-up not saying is
			// passed over.
			return node, nil
		}
	}
	return order[0], nil
}

// Listed reports whether this node is among the git nodes its git listing
// carries ([GitPlacement.Listing]) — the only nodes [Router.Maintainer] ever
// answers, so a node it does not carry maintains no repository, whatever it
// would rank: (true, nil), (false, nil), or (false, err) where the listing
// cannot be read. A maintainer's loops wait for it before their first pass
// (internal/code/compact): a booting node's presence is listed only once its
// host beats, and a pass before that would find every repository placed
// elsewhere or nowhere.
//
// THE LISTING, NEVER THIS NODE'S GIT HALF, which places this node in its own
// order for [Router.Home] alone: a node with its half built that no listing
// carries yet is one its peers cannot see.
func (r *Router) Listed() (bool, error) {
	listing, err := r.gitPlacement.Listing()
	if err != nil {
		return false, fmt.Errorf("estate: whether this node is listed among the git nodes: %w", err)
	}
	return slices.Contains(listing.Nodes, r.self), nil
}

// runLocalGit runs one git operation in-process on this node's own git half,
// x already stamped for this node's place in the walk. final is set when that
// is the answer; otherwise the walk says why not and moves on — as a remote
// git node's `no_backend` and `behind` are read, with nothing remembered,
// since asking this node's own half again costs no round trip — and passes
// says whether no copy serving the operation is coming here, so the walk lets
// the next node fill in this one's place ([passesFill]'s rule).
//
// HELD TO THE ATTEMPT A REMOTE ASKER GIVES IT: budget, the walk's attempt on
// one node ([Router.budgetFor]), served until a reply's margin before it as
// the server's git arm serves a remote one ([code.AnswerBy]). Run under the
// caller's own context instead, the home's own seats' reads waited out a busy
// read lane or a slow sync for as long as their caller allowed, and ran their
// git for as long, while every other node's seats were answered unready at the
// attempt's end: what a read or a write is answered must not depend on which
// node asked it.
func (r *Router) runLocalGit(ctx context.Context, spec *opSpec, x exchange, g GitServer, budget time.Duration,
	w *walk) (value any, final, passes bool, err error) {

	attempt, cancel := attemptContext(ctx, budget)
	defer cancel()
	served, stop := code.AnswerBy(attempt, deadlineOf(attempt))
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
	return value, true, false, err
}

// answerGit is the server's GIT ARM: a git operation that passed the
// envelope's admission and the lane check, answered from this node's git half
// — never held to a copy's verdict, a floor or the lag gate, none of which
// applies to [GitCopy]. A node whose code host is not built answers
// `no_backend`, never silence, which its asker would read as a node gone; a
// half not ready answers `behind`, naming why and carrying its reason
// ([reply.Unready]) — `behind_config` for a seat's read or write on a node
// behind the company's configuration, the node's posture as every operation
// says it.
//
// SERVED UNTIL ITS ANSWER IS DUE, a reply's margin before the request's
// deadline ([code.AnswerBy]), and no later: every wait a read or a write makes
// — its copy's sync, its unit of the share, the read lane's queue for each
// process, each process, a write's upload and its proposal — takes that one
// context, so a busy node's `behind` reaches its asker while it still
// listens. Served to the deadline itself and merely marked with the margin
// (statelog.WithReplyMargin, which only statelog's own waits read, and
// nothing on this path does), a read waiting on its share or a slot ran to
// the deadline, and its asker — its attempt spent and nothing heard — took a
// busy node for a gone one: asked it last for every operation for suspectFor,
// told both placements, and sent every repository it is home to to a
// runner-up that downloaded a copy of each.
func (s server) answerGit(ctx context.Context, spec *opSpec, req request, out reply) []byte {
	ctx, cancel := code.AnswerBy(ctx, req.Deadline)
	defer cancel()
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
