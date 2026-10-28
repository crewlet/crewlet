// Package codefault is THE NAMED FAULT POINTS of the code host's push, sync
// and compaction paths, and the one seam that reaches them. Production passes
// a nil [Injector] and every call site is a nil check; the suites pass a
// [*Script].
//
// NAMED rather than ad hoc hooks, because the code host's crash matrices walk
// EVERY point of a path in pipeline order and kill the node there — the push
// path's ([PushPath]) the receiving node, the compaction's ([CompactPath])
// the repository's home git node building one, and an engine write's
// ([WritePath]) the git node building it: a step with no point is a step
// nobody ever crashed at. So a new step of the receive pipeline, of a
// compaction or of an engine write, that can leave state behind gets a point
// here, in order, or the matrix does not cover it. The order is written ONCE,
// in this package, and a call site adds a hit and never edits the list.
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
// sync point on another git node of the fleet — and would hold that node's
// teardown for ever, where a Block would have ended with the node's context.
// A crash hit after the release, the teardown's own included, answers at
// once.
//
// [Action.Block] is the hold of a process that LIVES — ordering two pushes,
// holding a slot, a client hanging up on a held request — and ends when its
// channel closes or the hit's context ends. [Action.Err] answers what the
// client is told when a step fails and the process lives.
//
// The committer's after-append point is hit by a publisher the suites wrap
// around the committer's, never by internal/code, so the domain imports
// nothing from this package.
//
// Every misuse is LOUD. A case that arms a point that does not exist, an
// offset on a point that reads none, an action no hit could ever reveal, or a
// crash beside an Err or a Block it would never reach panics in the case; a
// call site that names a point that does not exist, or hands a point an
// offset it does not read, gets an error from every hit, scripted or not.
// Each of them would otherwise be a case that passes because the fault it
// meant to inject never fired.
package codefault

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

// The push path's points, in the order a push reaches them, then the sync's,
// then the compaction's, then the engine write's own. Each comment says what
// is true when the point is hit.
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
	// store makes of the bundle, at the offset of the bundle bytes handed to
	// the store so far — one of the two points that read an offset
	// ([Action.At]), with [CompactUpload].
	// It is NOT a count of bytes stored: s3obj fills a whole part
	// (PartBytes, 8 MiB — all of a smaller bundle) before it sends any and
	// reads the next part while up to four are in flight unanswered, and
	// natsobj holds a whole message (MessageBytes, 128 KiB) before the
	// library sends it. A case that needs bytes landed at the bucket waits
	// for them there rather than reading an offset as a stored count.
	HostUpload Point = "host.upload"
	// HostAfterUpload: the bundle is stored and no row names it.
	HostAfterUpload Point = "host.after_upload"
	// CommitterAfterAppend: the record is appended to the code log and its
	// result not yet returned to the receiver — the durable record the
	// receiver never heard about. Hit on the committer's batch goroutine,
	// never in a request, so a crash here holds the batch, and the dead
	// node's Committer.Stop that waits for it, until the release.
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
	// round has applied to its end there (its state renamed in), and the next
	// incarnation is staged whole under scratch/ — every live pack, HEAD,
	// config, the packed-refs and the derived files a retirement keeps linked
	// in, its directories synced and its own state written — and not yet
	// renamed into repos/: the swap's one irreversible step is next
	// (internal/code/cache's swap.go).
	SyncSwapBeforeRename Point = "sync.swap_before_rename"
	// InstallAfterFetch: a published layer's every part is fetched into
	// tmp/, its files made durable and held to its row (code.CheckLayer),
	// and nothing of it is placed: no incarnation holds a file of it, and
	// no chain file names it (internal/code/cache's layers.go) — hit by a
	// full round's install and by the installer alike.
	InstallAfterFetch Point = "sync.install_after_fetch"

	// CompactAfterBuild: a compaction's one pack-objects has written its
	// outputs into the compaction's scratch, and nothing of them is laid out
	// or stored (internal/code/compact).
	CompactAfterBuild Point = "compact.after_build"
	// CompactUpload: inside an output's upload, before each read the object
	// store makes of the output's bundle, at the offset of the bundle bytes
	// handed to the store so far — the compaction's [HostUpload], an offset
	// rather than a count of bytes stored, for its reasons. Every output is
	// uploaded at once, so each hit is one output's, at that output's own
	// offset.
	CompactUpload Point = "compact.upload"
	// CompactAfterUpload: every output is stored, and no row names any of
	// them: no proposal has been made.
	CompactAfterUpload Point = "compact.after_upload"
	// CompactAfterPropose: the repository's committer has answered the
	// compaction's proposal, and its outputs are not yet adopted into the
	// building node's cache.
	CompactAfterPropose Point = "compact.after_propose"

	// WriteAfterBuild: an engine write's build has made its commit and its one
	// pack, indexed in the write's scratch, and nothing of it is stored or
	// proposed (internal/code/host's Server.Write): the landing every push
	// takes is next, its own points the write's from here on ([WritePath]).
	WriteAfterBuild Point = "write.after_build"
)

// pushPath is the receive pipeline's points in the order a push reaches them.
var pushPath = []Point{
	HostAfterCommands, HostAfterSync, HostAfterScratch, HostHookAdmitted, HostUpload,
	HostAfterUpload, CommitterAfterAppend, HostAfterPropose, HostBeforeAdopt, HostHookClose,
}

// syncPath is the cache sync's points in the order a sync reaches them: a
// round's download, its refs and its state, then — since the round has
// applied to its end on the incarnation it leaves before the swap begins —
// the swap's rename, and LAST a published layer fetched and not yet placed,
// which a full round meets before its refs and the installer after every
// round, so no order of the round's own puts it elsewhere.
var syncPath = []Point{SyncDownload, SyncRefs, SyncState, SyncSwapBeforeRename, InstallAfterFetch}

// compactPath is a compaction's points in the order its home reaches them:
// its build, each output's upload, the uploads ended, the committer's answer.
var compactPath = []Point{CompactAfterBuild, CompactUpload, CompactAfterUpload, CompactAfterPropose}

// writePath is an engine write's OWN points: its build ended, before the
// landing. What it meets after them is the landing's ([WritePath]).
var writePath = []Point{WriteAfterBuild}

// landingPath is the push path's points the LANDING hits (internal/code/host's
// land), in its order: an engine write lands as a relayed push does, so a write
// crashes at every point of its landing the push path names, and at no point
// of the relay's own — its commands, its sync, its scratch, its hook — which
// a write never meets.
var landingPath = []Point{HostUpload, HostAfterUpload, HostAfterPropose, HostBeforeAdopt}

// PushPath is the receive pipeline's points in the order a push reaches them:
// the nine the host hits and the committer's. A fresh copy at every call,
// because the crash matrix walks it and nothing it does to its copy may
// reorder the next walk.
func PushPath() []Point { return slices.Clone(pushPath) }

// CompactPath is a compaction's points in the order its home reaches them,
// every one hit by internal/code/compact. A fresh copy at every call, for
// [PushPath]'s reason: the compaction's crash matrix walks it.
func CompactPath() []Point { return slices.Clone(compactPath) }

// WritePath is an engine write's points in the order it reaches them: its own
// ([WriteAfterBuild]), then the landing's of the push path, in their order —
// every point an engine write's crash matrix kills the git node at. A fresh
// copy at every call, for [PushPath]'s reason. The landing's points are the
// push path's own, hit once each by the one landing (land.go), so the write
// adds a point to the closed set for its build alone.
func WritePath() []Point { return slices.Concat(writePath, landingPath) }

// Points is the closed set: every point, the push path first in pipeline
// order, then the sync's, then the compaction's, then the engine write's own.
// A fresh copy at every call, so no importer can rewrite what [Point.Valid]
// accepts.
func Points() []Point { return slices.Concat(pushPath, syncPath, compactPath, writePath) }

// Valid reports whether p is one of [Points].
func (p Point) Valid() bool {
	return slices.Contains(pushPath, p) || slices.Contains(syncPath, p) || slices.Contains(compactPath, p) ||
		slices.Contains(writePath, p)
}

// readsOffset reports whether p is hit at an offset ([Action.At]): an upload's
// point, [HostUpload] or [CompactUpload], hit before each read the object
// store makes of the bytes it uploads. Every other point is hit at zero.
//
// ONE PREDICATE, read at all three places the rule is applied — an arming's
// refusal (Arm), a firing's offset gate (fire) and a call site's refusal
// (checkHit): spelled once a place, a point added at one and not another
// either armed an offset no hit could reach or fired at its first hit, offset
// 0, whatever its arming said.
func readsOffset(p Point) bool { return p == HostUpload || p == CompactUpload }

// offsetPoints names the points that read an offset, for a refusal's
// sentence.
const offsetPoints = "host.upload and compact.upload"

// ErrNotAPoint is a hit, an arming or a count naming a point that is not one
// of [Points].
var ErrNotAPoint = errors.New("codefault: not a fault point")

// ErrOffset is a hit handing an offset to a point that reads none — every
// point but the uploads', [HostUpload] and [CompactUpload], is hit at zero —
// or a negative offset to an upload's, which counts bytes from zero. Each
// wrap says which of the two it was.
var ErrOffset = errors.New("codefault: an offset the point does not read")

// ErrCrashed is what a hit held at a crash ([Action.Crash]) answers once
// [Script.ReleaseCrashes] ends it: the node it was serving is dead, so the
// call site's step fails and nothing after the point runs.
var ErrCrashed = errors.New("codefault: the node crashed at this point")

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
	// context ends, and the hit then answers Err (or the context's cause):
	// a hold in a process that LIVES. It is never a crash's — a node killed
	// in-process cancels the hit's context, and the call site's error path
	// would run before the restart; that is [Action.Crash].
	Block <-chan struct{}
	// Crash holds the hit past the node's death: whatever its context does,
	// until [Script.ReleaseCrashes], and the hit then answers [ErrCrashed].
	// Whatever waits for the work it holds — a request's handler, the
	// committer's batch, a sync — waits for the release too, which is what
	// the package doc asks of a kill and a teardown. It takes neither Err
	// nor Block, which it would never reach. A crash the restarted node must
	// not meet again is armed Once, or disarmed before the restart.
	Crash bool
	// Reached, when non-nil, is closed when the point first fires — how a
	// case knows the step was reached and it may kill the node. It is
	// closed at most once, however many hits fire, and must be open when
	// armed.
	Reached chan struct{}
	// At is the uploads' alone, [HostUpload]'s and [CompactUpload]'s: the
	// action fires on hits whose offset — bundle bytes the object store has
	// read, never bytes it has stored — is at or past it (with Once, the
	// first such hit). Zero is the first byte.
	At int64
}

// Injector is the seam a call site holds. Nil in production, where every
// call site's nil check is the whole cost of this package.
type Injector interface {
	// Hit reports that the step p was reached, at the offset at (zero for
	// every point but the uploads', [HostUpload] and [CompactUpload]); a
	// non-nil error fails the step.
	Hit(ctx context.Context, p Point, at int64) error
}

// Script is the [Injector] the suites arm. The zero Script is ready, with
// nothing armed. Safe for concurrent use; a nil *Script hits nothing, so a
// typed nil reaching an Injector field — which passes the call site's nil
// check — is harmless.
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
		panic(fmt.Sprintf("codefault: arming %s on a nil *Script, which hits nothing", p))
	}
	mustBePoint(p)
	switch {
	case a.At != 0 && !readsOffset(p):
		panic(fmt.Sprintf("codefault: an offset (At %d) on %s: only %s read one", a.At, p, offsetPoints))
	case a.At < 0:
		panic(fmt.Sprintf("codefault: a negative offset (At %d) on %s", a.At, p))
	case a.Err == nil && a.Block == nil && !a.Crash && a.Reached == nil:
		panic(fmt.Sprintf("codefault: an action on %s that does nothing: set Err, Block, Crash or Reached", p))
	case a.Crash && (a.Err != nil || a.Block != nil):
		panic(fmt.Sprintf("codefault: a crash on %s beside an Err or a Block: a crash answers %v "+
			"once released, and never reaches either", p, ErrCrashed))
	case a.Reached != nil && closed(a.Reached):
		panic(fmt.Sprintf("codefault: an action on %s whose Reached channel is already closed, "+
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
// every crash hit after it, answers [ErrCrashed]. The fleet's teardown calls
// it FIRST — after the case has judged the restarted node, and before it
// stops or waits for any node, killed or not — because whatever waits for
// work a crash holds waits until then: httptest's Server.Close for a dead
// node's held handlers, that node's Committer.Stop for its held batch
// whatever its context, and the ordinary teardown of a node no case killed
// that a crash fired on all the same. It releases no [Action.Block], may be
// called more than once, and on a nil *Script, which holds nothing, does
// nothing.
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

// Hit counts the hit and, if p is armed and the action fires — p reads no
// offset or at is at or past [Action.At], and a Once action has not
// already fired — closes Reached (once) and then: holds a crash, whatever ctx
// does, until [Script.ReleaseCrashes] and answers [ErrCrashed]; or holds on
// Block (answering context.Cause(ctx) if ctx ends first) and returns Err. A
// Once action that fired is disarmed under the same lock that decided it
// fired, so of hits racing on one point exactly one fires.
//
// A point that is not one of [Points] answers [ErrNotAPoint], and an offset a
// point does not read [ErrOffset] — whether or not anything is armed, and on
// a nil *Script too — so a call site naming either fails at once rather than
// silently never firing. A refused hit is not counted.
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
		// NOT ctx: a node killed in-process ends every context it was
		// serving, and a crash runs nothing of its call site before the
		// case has judged the restart.
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
// fires: disarming a Once action and closing Reached there, so neither can
// happen twice. It hands back the channel a crash waits on.
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
	// Closed under the lock that every firing takes, so a channel two
	// points share — or an action without Once firing on every hit — is
	// closed by the first firing and only checked by the rest.
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
