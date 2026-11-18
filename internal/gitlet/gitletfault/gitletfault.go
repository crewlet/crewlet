// Package gitletfault is THE NAMED FAULT POINTS of Gitlet's push, sync,
// compaction, publication and engine write paths, of a check's run, and of an
// expunge, an import and a pull mirror's sync, and the one seam that reaches
// them. Production passes a nil [Injector] and every call site is a nil
// check; the suites pass a [*Script].
//
// NAMED rather than ad hoc hooks, because Gitlet's crash matrices walk
// EVERY point of a path in pipeline order and kill the node there — the push
// path's ([PushPath]) the receiving node, the compaction's ([CompactPath]),
// the publication's ([PublishPath]) and the expunge's ([ExpungePath]) the
// repository's maintainer running one, and an engine write's ([WritePath])
// the Gitlet node building it, a check's run's ([CheckPath]) the Gitlet node
// running it, and an import's ([ImportPath]) and a pull mirror's sync's
// ([MirrorPath]) the repository's home: a step with no point is a step
// nobody ever crashed at. So a new step of any of those paths that can leave
// state behind gets a point here, in order, or the matrix does not cover it.
// The order is written ONCE, in this package, and a call site adds a hit and
// never edits the list.
//
// Each point is hit at exactly ONE site (TestEveryPointHasOneHitSite, which
// walks every directory that hits one, so a package that starts hitting
// points joins its list): a point hit at two sites fires at whichever runs
// first, and one hit at none is a fault no case can make. A step two paths
// share is one point on both, never a second name: an engine write and a
// merge land through the push's landing, so [WritePath] and [MergePath] end
// in its points.
//
// A crash is a hit HELD PAST THE NODE'S DEATH ([Action.Crash]). The case
// kills the node while the hit is held, restarts it over the same files and
// judges what the restart made of them, and only then does the fleet's
// teardown end the hold ([Script.ReleaseCrashes]), the hit answering
// [ErrCrashed] so nothing after the point runs before the judgement. Never an
// error return at the point, and never a hold that ends with the hit's
// context: a process that dies does not unwind its defers, while a node
// killed inside a test process cancels every context it was serving — so a
// hold that ended with its context would run the call site's error path, its
// deferred cleanup included, before the restart, and an assertion that the
// restart cleaned up would pass on what that path tidied instead.
//
// So whoever kills a node WAITS FOR NOTHING A CRASH MAY HOLD — a request, the
// committer's batch, a sync. Not every crash is in a request: the committer's
// point is hit on the committer's own batch goroutine, under a context the
// committer derives from its own lifetime. The kill closes the node's
// listener and client connections and gives up its claims on the node's files
// at once; it may begin a stop that cancels held work, and waits for none of
// it. Everything that waits for held work waits in the teardown, after
// ReleaseCrashes: httptest's Server.Close, which waits for every handler, and
// the dead node's Committer.Stop, which cancels its batches and then waits
// for each one whatever its context. And the teardown releases the crashes
// BEFORE it stops or waits for ANY node, killed or not: a crash can fire on a
// node no case killed — a case that fails between Reached and its kill, a
// sync point on another Gitlet node of the fleet — and would hold that node's
// teardown for ever, where a Block would have ended with the node's context.
// A crash hit after the release, the teardown's own included, answers at
// once.
//
// [Action.Block] is the hold of a process that LIVES — ordering two pushes,
// holding a slot, a client hanging up on a held request — and ends when its
// channel closes or the hit's context ends. [Action.Err] answers what the
// client is told when a step fails and the process lives.
//
// The committer's two points are hit by the suites, never by internal/gitlet, so
// the domain imports nothing from this package: its after-append point by a
// publisher they wrap around the committer's, and its before-admit point by
// the usage reading they hand the code log's reserve.
//
// The maintainer's step ([MaintainBeforeStep]) and an engine write's join
// ([WriteJoined]) are the two points a case only ever HOLDS or reaches, on no
// crash path. No crash matrix walks a maintenance run, whose derived files
// recover by their own declarations, so a case blocks every run's step to
// read a copy as a round left it, before the run that round's end owes the
// maintainer can replace a file of it. And a second ask of a write that joins
// the first builds nothing of its own, so a crash there leaves nothing a
// crash at the first ask's points does not: a case reaches the join to let
// the first ask's write go only once the second waits on it.
//
// Every misuse is LOUD. A case that arms a point that does not exist, an
// offset on a point that reads none, an action no hit could ever reveal, or a
// crash beside an Err or a Block it would never reach panics in the case; a
// call site that names a point that does not exist, or hands a point an
// offset it does not read, gets an error from every hit, scripted or not.
// Each of them would otherwise be a case that passes because the fault it
// meant to inject never fired.
package gitletfault

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Point is one named step. A closed set: [Points] lists it and [Point.Valid]
// reads that list.
type Point string

// The points, grouped by path, each group in its path's order. Each comment
// says what is true when the point is hit.
const (
	// HostAfterCommands: the push's commands are read and nothing has started.
	HostAfterCommands Point = "host.after_commands"
	// HostAfterSync: the cache is synced linearizably, so it holds every
	// object the push's pack may name as a delta base.
	HostAfterSync Point = "host.after_sync"
	// HostAfterScratch: the scratch repository the push is received into
	// exists.
	HostAfterScratch Point = "host.after_scratch"
	// HostHookAdmitted: receive-pack has stored the pack in the scratch, and
	// its hook has reached the engine and spent its token.
	HostHookAdmitted Point = "host.hook_admitted"
	// HostUpload: inside the bundle's upload, before each read the object
	// store makes of it, at the offset of the bytes handed to the store so
	// far ([Action.At]). NOT a count of bytes stored: s3obj fills a whole part
	// (PartBytes, 8 MiB) before sending, with up to four in flight, and
	// natsobj a whole message (MessageBytes, 128 KiB), so a case that needs
	// bytes landed waits for them at the bucket.
	HostUpload Point = "host.upload"
	// HostAfterUpload: the bundle is stored and no row names it.
	HostAfterUpload Point = "host.after_upload"
	// CommitterAfterAppend: the record is appended to the code log and its
	// result not yet returned to the receiver. Hit on the committer's batch
	// goroutine, never in a request, so a crash here holds the batch, and the
	// dead node's Committer.Stop, until the release. The change writer hits
	// it too, after a change's conversation record, on its publish goroutine.
	CommitterAfterAppend Point = "committer.after_append"
	// HostAfterPropose: the committer has answered.
	HostAfterPropose Point = "host.after_propose"
	// HostBeforeAdopt: the push's row is applied and its pack not yet adopted
	// into the cache. Adoption runs before the report, so a client told ok
	// that fetches from this node at once finds its own push there.
	HostBeforeAdopt Point = "host.before_adopt"
	// HostHookClose: the outcome is known and the report not yet written; an
	// error here drops the hook's connection before git is told the outcome.
	HostHookClose Point = "host.hook_close"

	// SyncDownload: a pack bundle's download is about to begin.
	SyncDownload Point = "sync.download"
	// SyncRefs: every pack is installed, packed-refs staged and not yet
	// renamed in.
	SyncRefs Point = "sync.refs"
	// SyncState: the refs are installed, the state staged and not yet renamed
	// in.
	SyncState Point = "sync.state"
	// SyncSwapBeforeRename: packs left an incarnation a reader may hold, the
	// round has applied to its end there, and the next incarnation is staged
	// whole under scratch/ (live packs, HEAD, config, packed-refs, the kept
	// derived files, its state; directories synced) and not yet renamed into
	// repos/: the swap's one irreversible step is next (internal/gitlet/cache's
	// swap.go).
	SyncSwapBeforeRename Point = "sync.swap_before_rename"
	// InstallAfterFetch: a published layer's every part is fetched into tmp/,
	// durable and held to its row (gitlet.CheckLayer), and nothing of it is
	// placed in an incarnation or named by a chain file (internal/gitlet/cache's
	// layers.go). Hit by a full round's install and by the installer.
	InstallAfterFetch Point = "sync.install_after_fetch"

	// CompactAfterBuild: a compaction's one pack-objects has written its
	// outputs into the compaction's scratch, and nothing of them is laid out
	// or stored (internal/gitlet/compact).
	CompactAfterBuild Point = "compact.after_build"
	// CompactUpload: the compaction's [HostUpload], inside an output's
	// upload. Every output uploads at once, so each hit is one output's, at
	// that output's own offset.
	CompactUpload Point = "compact.upload"
	// CompactAfterUpload: every output is stored, and no row names any of
	// them: no proposal has been made.
	CompactAfterUpload Point = "compact.after_upload"
	// CompactAfterPropose: the repository's committer has answered the
	// compaction's proposal, and its outputs are not yet adopted into the
	// building node's cache.
	CompactAfterPropose Point = "compact.after_propose"

	// PublishAfterBuild: a publication's layer (multi-pack-index layer,
	// bitmap, commit-graph layer) and any repack's outputs are written in its
	// scratch, and nothing of them is laid out or stored
	// (internal/gitlet/compact).
	PublishAfterBuild Point = "publish.after_build"
	// PublishUpload: the publication's [HostUpload], inside a layer part's
	// upload (at the layer's offset) or a repack output's (at its bundle's).
	// Every part and output uploads at once, so each hit is one upload's, at
	// its own offset.
	PublishUpload Point = "publish.upload"
	// PublishAfterUpload: every part and every output is stored, and no row
	// names any of them: no proposal has been made.
	PublishAfterUpload Point = "publish.after_upload"
	// PublishAfterPropose: the repository's committer has answered the
	// publication's proposal, and neither its outputs nor its layer is
	// adopted into the publishing node's cache.
	PublishAfterPropose Point = "publish.after_propose"

	// WriteAfterBuild: an engine write's build has made its commit and its one
	// pack, indexed in its scratch, and nothing is stored or proposed
	// (internal/gitlet/host's Server.Write): the landing is next ([WritePath]).
	WriteAfterBuild Point = "write.after_build"

	// MergeAfterBuild: a merge_change's build has made its commit onto the
	// target tip it read and its one pack, indexed in its scratch, and nothing
	// is stored or proposed (internal/gitlet/host's merge.go): the landing is
	// next ([MergePath]). Hit again by every rebuild onto a moved target.
	MergeAfterBuild Point = "merge.after_build"

	// ChainAfterBuild: a chain's build has merged (or skipped) each entry onto
	// the commit the one before made and made its one pack, indexed in its
	// scratch, and nothing is stored (internal/gitlet/host's chain.go): its
	// bundle's upload is next, then its caller's record, which no chain point
	// reaches. A chain killed here leaves only its scratch, emptied at boot.
	ChainAfterBuild Point = "chain.after_build"

	// CheckAfterClaim: a Gitlet node's runner has claimed a queued check (the
	// row names it the attempt's runner) and made no box
	// (internal/gitlet/checkrun): a runner that dies here leaves a running row
	// only the repository's home ends, sweeping a runner gone from its git
	// order.
	CheckAfterClaim Point = "check.after_claim"

	// ExpungeAfterBuild: an expunge's build has written its kept and its
	// dropping packs into the expunge's scratch, and nothing of them is
	// stored or proposed (internal/gitlet/compact).
	ExpungeAfterBuild Point = "expunge.after_build"
	// ExpungeAfterBegin: the expunge's begin has applied (inputs retired, kept
	// packs live, dropping packs dropping, the drop epoch moved) and its
	// rescue has not started: only its rows say what is left to do.
	ExpungeAfterBegin Point = "expunge.after_begin"
	// ExpungeAfterRescueUpload: every rescue pack is stored, and no row names
	// any of them: the commit is not proposed.
	ExpungeAfterRescueUpload Point = "expunge.after_rescue_upload"
	// ExpungeAfterCommit: the repository's committer has answered the
	// expunge's commit, and its rescue is not yet adopted into the building
	// node's cache.
	ExpungeAfterCommit Point = "expunge.after_commit"

	// ImportAfterBuild: an import's attempt has fetched the source into its
	// scratch and cut what arrived into the packs it stores, and nothing of
	// them is stored or proposed (internal/gitlet/mirror).
	ImportAfterBuild Point = "import.after_build"
	// ImportAfterUpload: a chunk's packs are stored, and no row names them:
	// the chunk is not proposed.
	ImportAfterUpload Point = "import.after_upload"
	// ImportAfterPropose: the repository's committer has answered a chunk,
	// and its packs are not yet adopted into the importing node's cache.
	ImportAfterPropose Point = "import.after_propose"

	// MirrorAfterBuild: a pull mirror's sync has fetched its source into its
	// scratch and cut what arrived, and nothing of it is stored or proposed
	// (internal/gitlet/mirror).
	MirrorAfterBuild Point = "mirror.after_build"
	// MirrorAfterUpload: a sync's packs are stored, and no row names them.
	MirrorAfterUpload Point = "mirror.after_upload"
	// MirrorAfterPropose: the repository's committer has answered the sync,
	// and its packs are not yet adopted.
	MirrorAfterPropose Point = "mirror.after_propose"

	// CommitterBeforeAdmit: a committer's batch has decided its record and its
	// admission is reading the code log's usage; nothing is appended. An Err
	// is the broker's answer to that reading, refusing the batch
	// `usage_unknown`. Hit by the suites' wrapper, as [CommitterAfterAppend]
	// is, on a wrapped writer's admission reading, never a barrier's. ON NO
	// CRASH PATH: it leaves nothing to recover; a case faults it for the refusal.
	CommitterBeforeAdmit Point = "committer.before_admit"
	// HostBeforeOffered: an attempt's count of the commits it offered is about
	// to run, in its scratch. ON NO CRASH PATH: only a push a rule refuses
	// whole meets it, so a case faults it for the attempt recorded uncounted.
	HostBeforeOffered Point = "host.before_offered"

	// MaintainBeforeStep: a maintenance run of a copy is about to take its
	// next step (an index with or without its bitmap, the commit-graph, or the
	// node's tip over a published chain; internal/gitlet/cache's maintain.go),
	// nothing of it written. Hit before every step of every run. ON NO CRASH
	// PATH: derived files recover by their declarations. A case HOLDS it (a
	// Block) to read a copy as a round left it, since the run a round's end
	// owes the maintainer rewrites the commit-graph along with the index.
	MaintainBeforeStep Point = "maintain.before_step"

	// WriteJoined: an engine write has JOINED another ask of its push id
	// building and landing on this node, and waits for its answer having done
	// nothing itself (internal/gitlet/host's Server.Write). Hit by the joining
	// ask, each time it joins. ON NO CRASH PATH: a joiner holds nothing. A
	// case REACHES it to let the joined write go only once the second ask
	// waits, so the join, never the push's row, answers that ask.
	WriteJoined Point = "write.joined"
)

// pushPath is the receive pipeline's points in the order a push reaches them.
var pushPath = []Point{
	HostAfterCommands, HostAfterSync, HostAfterScratch, HostHookAdmitted, HostUpload,
	HostAfterUpload, CommitterAfterAppend, HostAfterPropose, HostBeforeAdopt, HostHookClose,
}

// syncPath is the cache sync's points: a round's download, refs and state,
// then the swap's rename (the round applies before the swap begins), and
// LAST a layer fetched and not yet placed, which a full round meets before
// its refs and the installer after every round, so no one order places it.
var syncPath = []Point{SyncDownload, SyncRefs, SyncState, SyncSwapBeforeRename, InstallAfterFetch}

// compactPath is a compaction's points in its maintainer's order.
var compactPath = []Point{CompactAfterBuild, CompactUpload, CompactAfterUpload, CompactAfterPropose}

// publishPath is a publication's points in its maintainer's order.
var publishPath = []Point{PublishAfterBuild, PublishUpload, PublishAfterUpload, PublishAfterPropose}

// writePath is an engine write's OWN points, before the landing's
// ([WritePath]).
var writePath = []Point{WriteAfterBuild}

// mergePath is a merge_change's OWN points, before the landing's
// ([MergePath]).
var mergePath = []Point{MergeAfterBuild}

// chainPath is a chain's points: its pack made, before its bundle is stored.
var chainPath = []Point{ChainAfterBuild}

// checkPath is a check's run's points: its claim applied, before any box.
var checkPath = []Point{CheckAfterClaim}

// expungePath is an expunge's points in its maintainer's order.
var expungePath = []Point{ExpungeAfterBuild, ExpungeAfterBegin, ExpungeAfterRescueUpload, ExpungeAfterCommit}

// importPath is an import's points in its home's order.
var importPath = []Point{ImportAfterBuild, ImportAfterUpload, ImportAfterPropose}

// mirrorPath is a pull mirror's sync's points in its home's order.
var mirrorPath = []Point{MirrorAfterBuild, MirrorAfterUpload, MirrorAfterPropose}

// refusalPoints are steps no crash matrix walks, each faulted for the refusal
// a case asserts on.
var refusalPoints = []Point{CommitterBeforeAdmit, HostBeforeOffered}

// holdPoints are steps no crash matrix walks, each held or reached so a case
// orders what it does next behind the step.
var holdPoints = []Point{MaintainBeforeStep, WriteJoined}

// landingPath is the push path's points the LANDING hits (internal/gitlet/host's
// land), in order: an engine write lands as a relayed push does, so it
// crashes at these and never at the relay's own (commands, sync, scratch,
// hook), which a write never meets.
var landingPath = []Point{HostUpload, HostAfterUpload, HostAfterPropose, HostBeforeAdopt}

// PushPath is the receive pipeline's points in the order a push reaches them:
// the nine the host hits and the committer's. A fresh copy at every call, so
// nothing a crash matrix does to its copy reorders the next walk.
func PushPath() []Point { return slices.Clone(pushPath) }

// CompactPath is a compaction's points in its maintainer's order, each hit by
// internal/gitlet/compact. A fresh copy at every call, for [PushPath]'s reason.
func CompactPath() []Point { return slices.Clone(compactPath) }

// PublishPath is a publication's points in its maintainer's order, each hit
// by internal/gitlet/compact. A fresh copy at every call, for [PushPath]'s
// reason.
func PublishPath() []Point { return slices.Clone(publishPath) }

// WritePath is an engine write's points in order: its own
// ([WriteAfterBuild]), then the push path's landing points, hit once each by
// the one landing (land.go), so a write adds only its build's point to the
// closed set. A fresh copy at every call, for [PushPath]'s reason.
func WritePath() []Point { return slices.Concat(writePath, landingPath) }

// MergePath is a merge_change's points in order: its own ([MergeAfterBuild]),
// then the push path's landing points, its proposal a merge record. A fresh
// copy at every call, for [PushPath]'s reason.
func MergePath() []Point { return slices.Concat(mergePath, landingPath) }

// ChainPath is a chain's points in order, each hit by internal/gitlet/host. A
// fresh copy at every call, for [PushPath]'s reason.
func ChainPath() []Point { return slices.Clone(chainPath) }

// CheckPath is a check's run's points in order, each hit by
// internal/gitlet/checkrun. A fresh copy at every call, for [PushPath]'s
// reason.
func CheckPath() []Point { return slices.Clone(checkPath) }

// ExpungePath is an expunge's points in its maintainer's order, each hit by
// internal/gitlet/compact. A fresh copy at every call, for [PushPath]'s
// reason.
func ExpungePath() []Point { return slices.Clone(expungePath) }

// ImportPath is an import's points in its home's order, each hit by
// internal/gitlet/mirror. A fresh copy at every call, for [PushPath]'s reason.
func ImportPath() []Point { return slices.Clone(importPath) }

// MirrorPath is a pull mirror's sync's points in its home's order, each hit
// by internal/gitlet/mirror. A fresh copy at every call, for [PushPath]'s
// reason.
func MirrorPath() []Point { return slices.Clone(mirrorPath) }

// Points is the closed set: every path's points in the milestone spec's order
// (M6 §0), the push path first, then the points no crash matrix walks. A
// fresh copy at every call, so no importer can rewrite what [Point.Valid]
// accepts.
func Points() []Point {
	return slices.Concat(pushPath, syncPath, compactPath, publishPath, writePath, mergePath, chainPath,
		checkPath, expungePath, importPath, mirrorPath, refusalPoints, holdPoints)
}

// Valid reports whether p is one of [Points].
func (p Point) Valid() bool {
	return slices.Contains(pushPath, p) || slices.Contains(syncPath, p) || slices.Contains(compactPath, p) ||
		slices.Contains(publishPath, p) || slices.Contains(writePath, p) || slices.Contains(mergePath, p) ||
		slices.Contains(chainPath, p) || slices.Contains(checkPath, p) || slices.Contains(expungePath, p) ||
		slices.Contains(importPath, p) || slices.Contains(mirrorPath, p) || slices.Contains(refusalPoints, p) ||
		slices.Contains(holdPoints, p)
}

// readsOffset reports whether p is hit at an offset ([Action.At]): the
// uploads' points, [HostUpload], [CompactUpload] and [PublishUpload]. Every
// other point is hit at zero. Arm, fire and checkHit all read this ONE
// predicate: a point added at one and not another would arm an offset no hit
// reaches, or fire at offset 0 whatever its arming said.
func readsOffset(p Point) bool { return p == HostUpload || p == CompactUpload || p == PublishUpload }

// offsetPoints names the points that read an offset, for a refusal's
// sentence.
const offsetPoints = "host.upload, compact.upload and publish.upload"

// ErrNotAPoint is a hit, an arming or a count naming a point that is not one
// of [Points].
var ErrNotAPoint = errors.New("gitletfault: not a fault point")

// ErrOffset is a hit handing an offset to a point that reads none (every
// point but the uploads'), or a negative offset to an upload's. Each wrap
// says which.
var ErrOffset = errors.New("gitletfault: an offset the point does not read")

// ErrCrashed is what a hit held at a crash ([Action.Crash]) answers once
// [Script.ReleaseCrashes] ends it: the node it was serving is dead, so the
// call site's step fails and nothing after the point runs.
var ErrCrashed = errors.New("gitletfault: the node crashed at this point")

// Action is what an armed point does when it fires. The zero Action does
// nothing a hit could reveal, so [Script.Arm] refuses it: at least one of
// Err, Block, Crash and Reached is set.
type Action struct {
	// Once fires on the first hit that would fire and then disarms the
	// point; otherwise the action fires on every such hit.
	Once bool
	// Err is returned to the call site, which fails as a failure of that
	// step would.
	Err error
	// Block, when non-nil, holds the hit until it is closed or the hit's
	// context ends, then answers Err (or the context's cause): a hold in a
	// process that LIVES. Never a crash's: a node killed in-process cancels
	// the context, running the call site's error path before the restart;
	// that is [Action.Crash].
	Block <-chan struct{}
	// Crash holds the hit past the node's death, whatever its context does,
	// until [Script.ReleaseCrashes], then answers [ErrCrashed]; whatever
	// waits for the held work waits for the release too (package doc). It
	// takes neither Err nor Block, which it would never reach. A crash the
	// restarted node must not meet again is armed Once, or disarmed first.
	Crash bool
	// Reached, when non-nil, is closed when the point first fires: how a case
	// knows it may kill the node. Closed at most once, however many hits
	// fire; it must be open when armed.
	Reached chan struct{}
	// At is the uploads' points' alone: the action fires on hits whose offset
	// (bytes the object store has read, never stored) is at or past it (with
	// Once, the first such hit). Zero is the first byte.
	At int64
}

// Injector is the seam a call site holds. Nil in production, where every
// call site's nil check is the whole cost of this package.
type Injector interface {
	// Hit reports that the step p was reached, at the offset at (zero for
	// every point but the uploads'); a non-nil error fails the step.
	Hit(ctx context.Context, p Point, at int64) error
}

// Script is the [Injector] the suites arm. The zero Script is ready, with
// nothing armed. Safe for concurrent use; a nil *Script hits nothing, so a
// typed nil that passes a call site's nil check is harmless.
type Script struct {
	mu    sync.Mutex
	armed map[Point]Action
	hits  map[Point]int
	// dead is closed by ReleaseCrashes, and every crash hit waits on it.
	dead chan struct{}
}

var _ Injector = (*Script)(nil)

// Arm sets p's action, replacing any. It PANICS on a point that is not one of
// [Points], on [Action.At] set for a point that reads no offset or negative,
// on an action with none of Err, Block, Crash and Reached, on a crash beside
// an Err or a Block it would never reach, on a Reached channel already
// closed, and on a nil *Script: each is a case that can never fail where it
// means to.
func (s *Script) Arm(p Point, a Action) {
	if s == nil {
		panic(fmt.Sprintf("gitletfault: arming %s on a nil *Script, which hits nothing", p))
	}
	mustBePoint(p)
	switch {
	case a.At != 0 && !readsOffset(p):
		panic(fmt.Sprintf("gitletfault: an offset (At %d) on %s: only %s read one", a.At, p, offsetPoints))
	case a.At < 0:
		panic(fmt.Sprintf("gitletfault: a negative offset (At %d) on %s", a.At, p))
	case a.Err == nil && a.Block == nil && !a.Crash && a.Reached == nil:
		panic(fmt.Sprintf("gitletfault: an action on %s that does nothing: set Err, Block, Crash or Reached", p))
	case a.Crash && (a.Err != nil || a.Block != nil):
		panic(fmt.Sprintf("gitletfault: a crash on %s beside an Err or a Block: a crash answers %v "+
			"once released, and never reaches either", p, ErrCrashed))
	case a.Reached != nil && closed(a.Reached):
		panic(fmt.Sprintf("gitletfault: an action on %s whose Reached channel is already closed, "+
			"which would say the point fired before it was hit", p))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed == nil {
		s.armed = map[Point]Action{}
	}
	s.armed[p] = a
}

// Disarm removes p's action, if any. It PANICS on a point that is not one of
// [Points]: the point the case meant would stay armed.
func (s *Script) Disarm(p Point) {
	mustBePoint(p)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.armed, p)
}

// Hits is how many times p was hit, armed or not. It PANICS on a point that
// is not one of [Points]: such a point is never hit, so its count would pass
// any assertion that it was not.
func (s *Script) Hits(p Point) int {
	mustBePoint(p)
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[p]
}

// ReleaseCrashes ends every crash: each hit held at one ([Action.Crash]), and
// every crash hit after, answers [ErrCrashed]. The fleet's teardown calls it
// FIRST, after the case has judged the restart and before it stops or waits
// for any node, killed or not, since whatever waits for held work waits until
// then (package doc). It releases no [Action.Block], may be called more than
// once, and does nothing on a nil *Script.
func (s *Script) ReleaseCrashes() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if dead := s.deadLocked(); !closed(dead) {
		close(dead)
	}
}

// deadLocked is the channel ReleaseCrashes closes, made on first use. s.mu is
// held.
func (s *Script) deadLocked() chan struct{} {
	if s.dead == nil {
		s.dead = make(chan struct{})
	}
	return s.dead
}

// Hit counts the hit and, if p's armed action fires (at is at or past
// [Action.At] where p reads one), closes Reached (once) and then holds a
// crash until [Script.ReleaseCrashes] and answers [ErrCrashed], or holds on
// Block (answering context.Cause(ctx) if ctx ends first) and returns Err. A
// Once action is disarmed under the lock that decided it fired, so of hits
// racing on one point exactly one fires.
//
// An unknown point answers [ErrNotAPoint] and an offset the point does not
// read [ErrOffset], armed or not and on a nil *Script too, so such a call
// site fails at once rather than silently never firing. A refused hit is not
// counted.
func (s *Script) Hit(ctx context.Context, p Point, at int64) error {
	if err := checkHit(p, at); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	a, dead, fires := s.fire(p, at)
	if !fires {
		return nil
	}
	if a.Crash {
		// NOT ctx: a node killed in-process ends every context it served,
		// and a crash runs nothing of its call site before the case judges
		// the restart.
		<-dead
		return fmt.Errorf("%w (%s), held past the node's death until ReleaseCrashes", ErrCrashed, p)
	}
	if a.Block != nil {
		select {
		case <-a.Block:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	return a.Err
}

// fire counts a hit of p and decides, under one lock, whether its action
// fires, disarming a Once action and closing Reached there so neither
// happens twice. It returns the channel a crash waits on.
func (s *Script) fire(p Point, at int64) (Action, <-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hits == nil {
		s.hits = map[Point]int{}
	}
	s.hits[p]++
	a, armed := s.armed[p]
	if !armed || (readsOffset(p) && at < a.At) {
		return Action{}, nil, false
	}
	if a.Once {
		delete(s.armed, p)
	}
	// Under the lock every firing takes, so a channel two points share, or
	// an action without Once, is closed by the first firing only.
	if a.Reached != nil && !closed(a.Reached) {
		close(a.Reached)
	}
	return a, s.deadLocked(), true
}

// checkHit is the call site's half of the closed set: a point that exists,
// and an offset only where one is read.
func checkHit(p Point, at int64) error {
	switch {
	case !p.Valid():
		return fmt.Errorf("%w: %q", ErrNotAPoint, p)
	case !readsOffset(p) && at != 0:
		return fmt.Errorf("%w: %s reads no offset and was hit at %d; only %s take one", ErrOffset, p, at,
			offsetPoints)
	case at < 0:
		return fmt.Errorf("%w: %s was hit at the negative offset %d; it counts bytes read from zero",
			ErrOffset, p, at)
	}
	return nil
}

func mustBePoint(p Point) {
	if !p.Valid() {
		panic(fmt.Errorf("%w: %q", ErrNotAPoint, p).Error())
	}
}

// closed reports whether c is closed. Only ever asked of a channel nothing
// sends on, so a receive that succeeds is a close.
func closed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
