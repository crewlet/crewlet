// Package codefault is THE NAMED FAULT POINTS of the code host's push and sync
// paths, and the one seam that reaches them. Production passes a nil
// [Injector] and every call site is a nil check; the suites pass a [*Script].
//
// NAMED rather than ad hoc hooks, because the code host's crash matrix walks
// EVERY point of the push path ([PushPath]) in pipeline order and kills the
// receiving node there: a step with no point is a step nobody ever crashed
// at. So a new step of the receive pipeline that can leave state behind gets
// a point here, in order, or the matrix does not cover it. The order is
// written ONCE, in this package, and a call site adds a hit and never edits
// the list.
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

// The push path's points, in the order a push reaches them, then the sync's.
// Each comment says what is true when the point is hit.
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
	// the store so far — the one point that reads an offset ([Action.At]).
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

// PushPath is the receive pipeline's points in the order a push reaches them:
// the nine the host hits and the committer's. A fresh copy at every call,
// because the crash matrix walks it and nothing it does to its copy may
// reorder the next walk.
func PushPath() []Point { return slices.Clone(pushPath) }

// Points is the closed set: every point, the push path first in pipeline
// order, then the sync's. A fresh copy at every call, so no importer can
// rewrite what [Point.Valid] accepts.
func Points() []Point { return slices.Concat(pushPath, syncPath) }

// Valid reports whether p is one of [Points].
func (p Point) Valid() bool {
	return slices.Contains(pushPath, p) || slices.Contains(syncPath, p)
}

// ErrNotAPoint is a hit, an arming or a count naming a point that is not one
// of [Points].
var ErrNotAPoint = errors.New("codefault: not a fault point")

// ErrOffset is a hit handing an offset to a point that reads none — every
// point but [HostUpload] is hit at zero — or a negative offset to the upload,
// which counts bytes from zero. Each wrap says which of the two it was.
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
	// At is [HostUpload]'s alone: the action fires on hits whose offset —
	// bundle bytes the object store has read, never bytes it has stored —
	// is at or past it (with Once, the first such hit). Zero is the first
	// byte.
	At int64
}

// Injector is the seam a call site holds. Nil in production, where every
// call site's nil check is the whole cost of this package.
type Injector interface {
	// Hit reports that the step p was reached, at the offset at (zero for
	// every point but [HostUpload]); a non-nil error fails the step.
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
// [Points], on [Action.At] set for any point but [HostUpload] or negative,
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
	case a.At != 0 && p != HostUpload:
		panic(fmt.Sprintf("codefault: an offset (At %d) on %s: only %s reads one", a.At, p, HostUpload))
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

// Hit counts the hit and, if p is armed and the action fires — p is not
// [HostUpload] or at is at or past [Action.At], and a Once action has not
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
	if !armed || (p == HostUpload && at < a.At) {
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
	case p != HostUpload && at != 0:
		return fmt.Errorf("%w: %s reads no offset and was hit at %d; only %s takes one", ErrOffset, p, at, HostUpload)
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
