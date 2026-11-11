package gitletfault_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
)

// hang is the hang detector every blocking case waits under: a hit that has
// not returned in thirty seconds is a hit that never will.
const hang = 30 * time.Second

var errCrash = errors.New("crashed here")

// returns runs hit on its own goroutine and hands back the error it returned,
// failing the test if it has not returned within the hang detector.
func returns(t *testing.T, hit func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- hit() }()
	select {
	case err := <-done:
		return err
	case <-time.After(hang): //fixedwait:liveness
		t.Fatalf("the hit has not returned after %s", hang)
		return nil
	}
}

// panics reports whether f panicked, and with what.
func panics(f func()) (msg string, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			msg, panicked = errorText(r), true
		}
	}()
	f()
	return "", false
}

func errorText(r any) string {
	if err, ok := r.(error); ok {
		return err.Error()
	}
	if s, ok := r.(string); ok {
		return s
	}
	return "a panic that is not text"
}

// The crash matrix walks PushPath() and kills the receiving node at each
// point, so the order IS the claim: a point listed out of order is a crash at
// a step the matrix believes comes before or after the one it really marks.
// The ten are written out here, in the order a push reaches them, rather than
// read back from the package, which would assert nothing.
func TestEveryPointIsValidAndThePushPathIsInPipelineOrder(t *testing.T) {
	want := []gitletfault.Point{
		gitletfault.HostAfterCommands,    // the commands read, nothing started
		gitletfault.HostAfterSync,        // the cache synced linearizably
		gitletfault.HostAfterScratch,     // the scratch repository exists
		gitletfault.HostHookAdmitted,     // the hook reached the engine; the pack is in the scratch
		gitletfault.HostUpload,           // inside the bundle's upload
		gitletfault.HostAfterUpload,      // the bundle stored, no row naming it
		gitletfault.CommitterAfterAppend, // the record appended, the receiver not yet told
		gitletfault.HostAfterPropose,     // the committer answered
		gitletfault.HostBeforeAdopt,      // the row applied, the pack not yet adopted
		gitletfault.HostHookClose,        // the report not yet written
	}
	if got := gitletfault.PushPath(); !slices.Equal(got, want) {
		t.Fatalf("PushPath() = %v\nwant the order a push reaches them: %v", got, want)
	}

	syncPoints := []gitletfault.Point{
		gitletfault.SyncDownload,         // a pack's download in flight
		gitletfault.SyncRefs,             // the packs in, the refs not yet renamed in
		gitletfault.SyncState,            // the refs in, the state not yet renamed in
		gitletfault.SyncSwapBeforeRename, // the round applied, the next incarnation staged, not yet renamed in
		gitletfault.InstallAfterFetch,    // a published layer fetched and checked, nothing of it placed
	}
	// The compaction's crash matrix walks CompactPath() as F21's walks the
	// push path's, so its order is a claim too.
	compactPoints := []gitletfault.Point{
		gitletfault.CompactAfterBuild,   // the outputs written in the scratch, nothing stored
		gitletfault.CompactUpload,       // inside an output's upload
		gitletfault.CompactAfterUpload,  // every output stored, nothing proposed
		gitletfault.CompactAfterPropose, // the committer answered, nothing adopted
	}
	if got := gitletfault.CompactPath(); !slices.Equal(got, compactPoints) {
		t.Fatalf("CompactPath() = %v\nwant the order a compaction reaches them: %v", got, compactPoints)
	}
	// The publication's crash matrix walks PublishPath(), so its order is a
	// claim as well.
	publishPoints := []gitletfault.Point{
		gitletfault.PublishAfterBuild,   // the layer and any outputs written in the scratch, nothing stored
		gitletfault.PublishUpload,       // inside a part's or an output's upload
		gitletfault.PublishAfterUpload,  // every part and output stored, nothing proposed
		gitletfault.PublishAfterPropose, // the committer answered, nothing adopted
	}
	if got := gitletfault.PublishPath(); !slices.Equal(got, publishPoints) {
		t.Fatalf("PublishPath() = %v\nwant the order a publication reaches them: %v", got, publishPoints)
	}
	// An engine write's crash matrix walks WritePath(): its own point, then
	// the landing's, each in the order the one landing hits it — and none of
	// the relay's own, which a write never meets.
	writePoints := []gitletfault.Point{
		gitletfault.WriteAfterBuild,  // the commit and its pack made in the scratch, nothing stored
		gitletfault.HostUpload,       // inside the bundle's upload
		gitletfault.HostAfterUpload,  // the bundle stored, no row naming it
		gitletfault.HostAfterPropose, // the committer answered
		gitletfault.HostBeforeAdopt,  // the row applied, the pack not yet adopted
	}
	if got := gitletfault.WritePath(); !slices.Equal(got, writePoints) {
		t.Fatalf("WritePath() = %v\nwant the order an engine write reaches them: %v", got, writePoints)
	}
	// A merge's landing is the one landing, after its own build's point.
	mergePoints := []gitletfault.Point{
		gitletfault.MergeAfterBuild,  // the merge's commit and pack made in its scratch, nothing stored
		gitletfault.HostUpload,       // inside the bundle's upload
		gitletfault.HostAfterUpload,  // the bundle stored, no row naming it
		gitletfault.HostAfterPropose, // the committer answered the merge record
		gitletfault.HostBeforeAdopt,  // the record applied, the pack not yet adopted
	}
	if got := gitletfault.MergePath(); !slices.Equal(got, mergePoints) {
		t.Fatalf("MergePath() = %v\nwant the order a merge reaches them: %v", got, mergePoints)
	}
	// A chain's build reaches its own point alone: what follows it — its
	// bundle stored, its caller's record — is no landing's.
	if got := gitletfault.ChainPath(); !slices.Equal(got, []gitletfault.Point{gitletfault.ChainAfterBuild}) {
		t.Fatalf("ChainPath() = %v\nwant the chain's entries merged and its pack made, nothing stored", got)
	}
	// An expunge's crash matrix walks ExpungePath(), its order a claim as
	// the others are.
	expungePoints := []gitletfault.Point{
		gitletfault.ExpungeAfterBuild,        // kept and dropping packs written in the scratch, nothing stored
		gitletfault.ExpungeAfterBegin,        // the begin applied, the rescue not started
		gitletfault.ExpungeAfterRescueUpload, // every rescue pack stored, the commit not proposed
		gitletfault.ExpungeAfterCommit,       // the commit answered, its rescue not adopted
	}
	if got := gitletfault.ExpungePath(); !slices.Equal(got, expungePoints) {
		t.Fatalf("ExpungePath() = %v\nwant the order an expunge reaches them: %v", got, expungePoints)
	}
	// A check's run walks CheckPath(): its claim applied, before any box.
	if got := gitletfault.CheckPath(); !slices.Equal(got, []gitletfault.Point{gitletfault.CheckAfterClaim}) {
		t.Fatalf("CheckPath() = %v\nwant the claim applied, before any box", got)
	}
	// An import walks ImportPath() and a pull mirror's sync MirrorPath():
	// each its build, a step's packs stored, the committer's answer to it.
	importPoints := []gitletfault.Point{
		gitletfault.ImportAfterBuild,   // fetched and cut in its scratch, nothing stored
		gitletfault.ImportAfterUpload,  // a chunk's packs stored, no row names them
		gitletfault.ImportAfterPropose, // the chunk answered, its packs not adopted
	}
	if got := gitletfault.ImportPath(); !slices.Equal(got, importPoints) {
		t.Fatalf("ImportPath() = %v\nwant the order an import reaches them: %v", got, importPoints)
	}
	mirrorPoints := []gitletfault.Point{
		gitletfault.MirrorAfterBuild,   // fetched and cut in its scratch, nothing stored
		gitletfault.MirrorAfterUpload,  // the sync's packs stored, no row names them
		gitletfault.MirrorAfterPropose, // the sync answered, its packs not adopted
	}
	if got := gitletfault.MirrorPath(); !slices.Equal(got, mirrorPoints) {
		t.Fatalf("MirrorPath() = %v\nwant the order a sync reaches them: %v", got, mirrorPoints)
	}
	if got, wantAll := gitletfault.Points(), slices.Concat(want, syncPoints, compactPoints, publishPoints,
		[]gitletfault.Point{gitletfault.WriteAfterBuild, gitletfault.MergeAfterBuild, gitletfault.ChainAfterBuild},
		[]gitletfault.Point{gitletfault.CheckAfterClaim}, expungePoints, importPoints,
		mirrorPoints, []gitletfault.Point{gitletfault.CommitterBeforeAdmit},
		[]gitletfault.Point{gitletfault.MaintainBeforeStep, gitletfault.WriteJoined}); !slices.Equal(got, wantAll) {
		t.Fatalf("Points() = %v\nwant the push path, then the sync's, the compaction's and the publication's, "+
			"then the write's own, the merge's and the chain's, then a check's run's, then the expunge's, "+
			"an import's and a pull mirror's sync's, then the committer's admission, then a maintenance "+
			"run's step and an engine write's join: %v", got, wantAll)
	}

	seen := map[gitletfault.Point]bool{}
	for _, p := range gitletfault.Points() {
		if !p.Valid() {
			t.Errorf("%q is listed and not Valid", p)
		}
		if seen[p] {
			t.Errorf("%q is listed twice: two steps would share one name", p)
		}
		seen[p] = true
	}
	for _, p := range []gitletfault.Point{"host.nope", "", "HOST.AFTER_SYNC", "host.after_sync "} {
		if p.Valid() {
			t.Errorf("Point(%q).Valid() = true; only the listed points exist", p)
		}
	}

	// Both lists are copies: a caller rewriting what it was handed changes
	// neither the next caller's answer nor what Valid accepts.
	path := gitletfault.PushPath()
	path[0] = "host.nope"
	compactions := gitletfault.CompactPath()
	compactions[0] = "host.nope"
	publications := gitletfault.PublishPath()
	publications[0] = "host.nope"
	writes := gitletfault.WritePath()
	writes[0], writes[1] = "host.nope", "host.nope"
	expunges := gitletfault.ExpungePath()
	expunges[0] = "host.nope"
	all := gitletfault.Points()
	all[0] = "host.nope"
	if gitletfault.PushPath()[0] != gitletfault.HostAfterCommands || gitletfault.Points()[0] != gitletfault.HostAfterCommands ||
		gitletfault.CompactPath()[0] != gitletfault.CompactAfterBuild ||
		gitletfault.PublishPath()[0] != gitletfault.PublishAfterBuild || gitletfault.WritePath()[0] != gitletfault.WriteAfterBuild ||
		gitletfault.ExpungePath()[0] != gitletfault.ExpungeAfterBuild ||
		gitletfault.WritePath()[1] != gitletfault.HostUpload || gitletfault.PushPath()[4] != gitletfault.HostUpload {
		t.Fatal("rewriting a returned list changed the next call's")
	}
	if gitletfault.Point("host.nope").Valid() {
		t.Fatal("rewriting a returned list made Valid accept a point no call site can hit")
	}
}

func TestAnUnarmedHitIsCountedAndPasses(t *testing.T) {
	ctx := t.Context()
	s := new(gitletfault.Script)
	for range 3 {
		if err := s.Hit(ctx, gitletfault.SyncRefs, 0); err != nil {
			t.Fatalf("an unarmed hit = %v, want nil", err)
		}
	}
	if err := s.Hit(ctx, gitletfault.HostUpload, 4096); err != nil {
		t.Fatalf("an unarmed upload hit = %v, want nil", err)
	}
	if got := s.Hits(gitletfault.SyncRefs); got != 3 {
		t.Fatalf("Hits(SyncRefs) = %d, want 3", got)
	}
	if got := s.Hits(gitletfault.HostUpload); got != 1 {
		t.Fatalf("Hits(HostUpload) = %d, want 1", got)
	}
	if got := s.Hits(gitletfault.SyncState); got != 0 {
		t.Fatalf("Hits(SyncState) = %d, want 0 for a point nothing hit", got)
	}

	// A typed nil reaching an Injector field passes the call site's nil
	// check, so it must hit nothing rather than crash the call site.
	var none *gitletfault.Script
	var inj gitletfault.Injector = none
	if err := inj.Hit(ctx, gitletfault.SyncDownload, 0); err != nil {
		t.Fatalf("a nil *Script's hit = %v, want nil", err)
	}
	if got := none.Hits(gitletfault.SyncDownload); got != 0 {
		t.Fatalf("a nil *Script counted %d hits", got)
	}
	none.Disarm(gitletfault.SyncDownload)
}

func TestADisarmedPointPasses(t *testing.T) {
	ctx := t.Context()
	s := new(gitletfault.Script)
	s.Arm(gitletfault.HostAfterSync, gitletfault.Action{Err: errCrash})
	if err := s.Hit(ctx, gitletfault.HostAfterSync, 0); !errors.Is(err, errCrash) {
		t.Fatalf("an armed hit = %v, want %v", err, errCrash)
	}
	s.Disarm(gitletfault.HostAfterSync)
	if err := s.Hit(ctx, gitletfault.HostAfterSync, 0); err != nil {
		t.Fatalf("a disarmed hit = %v, want nil", err)
	}
	if got := s.Hits(gitletfault.HostAfterSync); got != 2 {
		t.Fatalf("Hits = %d, want 2: a hit is counted armed or not", got)
	}
	s.Disarm(gitletfault.HostAfterSync) // disarming what is not armed is a no-op
}

func TestAOnceActionFiresOnce(t *testing.T) {
	ctx := t.Context()
	s := new(gitletfault.Script)
	s.Arm(gitletfault.HostAfterScratch, gitletfault.Action{Once: true, Err: errCrash})
	if err := s.Hit(ctx, gitletfault.HostAfterScratch, 0); !errors.Is(err, errCrash) {
		t.Fatalf("the first hit = %v, want %v", err, errCrash)
	}
	for range 3 {
		if err := s.Hit(ctx, gitletfault.HostAfterScratch, 0); err != nil {
			t.Fatalf("a hit after a Once action fired = %v, want nil", err)
		}
	}

	s.Arm(gitletfault.HostAfterScratch, gitletfault.Action{Err: errCrash})
	for i := range 3 {
		if err := s.Hit(ctx, gitletfault.HostAfterScratch, 0); !errors.Is(err, errCrash) {
			t.Fatalf("hit %d without Once = %v, want %v on every hit", i, err, errCrash)
		}
	}
}

// A Once action is decided under one lock, so of many hits racing on one
// point exactly one fires: the crash matrix kills ONE push at the point, and
// two pushes both believing they were the crash would leave a second
// half-finished push nobody asserted anything about.
func TestConcurrentHitsFireAOnceActionOnce(t *testing.T) {
	ctx := t.Context()
	s := new(gitletfault.Script)
	s.Arm(gitletfault.SyncDownload, gitletfault.Action{Once: true, Err: errCrash})

	const hits = 64
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fired int
	)
	for range hits {
		wg.Go(func() {
			if err := s.Hit(ctx, gitletfault.SyncDownload, 0); errors.Is(err, errCrash) {
				mu.Lock()
				fired++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if fired != 1 {
		t.Fatalf("a Once action fired on %d of %d concurrent hits, want exactly 1", fired, hits)
	}
	if got := s.Hits(gitletfault.SyncDownload); got != hits {
		t.Fatalf("Hits = %d, want %d", got, hits)
	}
}

// A Block is the hold of a process that LIVES — two pushes ordered, a slot
// held, a client hanging up on a held request — so it ends with its channel
// or with the hit's context, whichever comes first, and the call site goes on
// as it would.
//
// In a bubble, where synctest.Wait returns once every goroutine but the
// case's is durably blocked: a hit that was going to return without its hold
// has returned by then, and Reached is closed by the firing that holds.
//
// Mutations: return from a Block's hit without waiting on it — it has
// returned while its Block is open, red; wait on the Block alone — the hit
// whose context ended never returns, red.
func TestABlockHoldsUntilReleasedOrCancelled(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := new(gitletfault.Script)
			block, reached := make(chan struct{}), make(chan struct{})
			s.Arm(gitletfault.HostHookAdmitted, gitletfault.Action{Block: block, Reached: reached, Err: errCrash})
			open := sync.OnceFunc(func() { close(block) })
			t.Cleanup(open)

			done := make(chan error, 1)
			go func() { done <- s.Hit(t.Context(), gitletfault.HostHookAdmitted, 0) }()
			stillHeld(t, done, "while its Block was still open")
			requireReached(t, reached)
			open()
			select {
			case err := <-done:
				if !errors.Is(err, errCrash) {
					t.Fatalf("the released hit = %v, want its Err %v", err, errCrash)
				}
			case <-time.After(hang):
				t.Fatalf("the hit has not returned %s after its Block closed", hang)
			}
		})
	})

	t.Run("cancelled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := new(gitletfault.Script)
			reached := make(chan struct{})
			s.Arm(gitletfault.HostHookAdmitted, gitletfault.Action{Block: make(chan struct{}), Reached: reached})

			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- s.Hit(ctx, gitletfault.HostHookAdmitted, 0) }()
			stillHeld(t, done, "before its context ended")
			requireReached(t, reached)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("a hit whose context ended = %v, want context.Canceled", err)
				}
			case <-time.After(hang):
				t.Fatalf("a held hit outlived its context by %s", hang)
			}
		})
	})

	t.Run("cause", func(t *testing.T) {
		s := new(gitletfault.Script)
		s.Arm(gitletfault.HostHookAdmitted, gitletfault.Action{Block: make(chan struct{})})
		why := errors.New("the node was killed")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(why)
		if err := returns(t, func() error { return s.Hit(ctx, gitletfault.HostHookAdmitted, 0) }); !errors.Is(err, why) {
			t.Fatalf("a hit whose context ended = %v, want the context's cause %v", err, why)
		}
	})
}

// stillHeld fails the test if the hit whose answer arrives on done has
// returned once every other goroutine of the case's bubble is durably blocked:
// a hit that was going to return has returned by then, and one held is parked
// on its hold. Called inside a synctest bubble — synctest.Wait panics outside
// one.
func stillHeld(t *testing.T, done <-chan error, why string) {
	t.Helper()
	synctest.Wait()
	select {
	case err := <-done:
		t.Fatalf("the hit returned %v %s", err, why)
	default:
	}
}

// requireReached fails the test unless reached is closed: called once the
// hit that closes it is parked, inside a synctest bubble.
func requireReached(t *testing.T, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	default:
		t.Fatal("Reached was not closed by the hit's firing")
	}
}

// answer waits, under the hang detector, for the hit whose answer arrives on
// done — inside a synctest bubble, whose clock reaches the detector only once
// every goroutine of the case is durably blocked: a hit that never returns.
//
//fixedwait:bubble
func answer(t *testing.T, done <-chan error, why string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(hang):
		t.Fatalf("the hit has not returned %s after %s", why, hang)
		return nil
	}
}

// A crash is the process-death shape, and a node killed inside a test process
// cancels every request it was serving. So a crash hit must OUTLIVE its
// context: one that ended with it would run the call site's error path — its
// deferred cleanup included — before the restart, and the crash matrix's
// restart assertions would pass on what that path tidied. It ends only when
// the case has judged the restarted node and its cleanup releases the
// crashes, and then it answers ErrCrashed so nothing after the point runs.
//
// Each hold in a bubble, where synctest.Wait returns once every goroutine but
// the case's is durably blocked: a hit that was going to return without its
// hold has returned by then.
//
// Mutations: answer a crash at its context's end — the hit has returned once
// its context ended, red; answer a crash at once, unheld — it has returned
// before ReleaseCrashes, red; end a Block at ReleaseCrashes — it has returned
// before its context ended, red.
func TestACrashOutlivesItsContextUntilReleased(t *testing.T) {
	t.Run("held past its context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := new(gitletfault.Script)
			t.Cleanup(s.ReleaseCrashes)
			reached := make(chan struct{})
			s.Arm(gitletfault.HostAfterScratch, gitletfault.Action{Crash: true, Reached: reached})

			ctx, cancel := context.WithCancelCause(t.Context())
			done := make(chan error, 1)
			go func() { done <- s.Hit(ctx, gitletfault.HostAfterScratch, 0) }()
			stillHeld(t, done, "before its context ended")
			requireReached(t, reached)
			cancel(errors.New("the node was killed"))
			stillHeld(t, done, "once its context ended, as a request of a killed node: a crash runs no error "+
				"path")

			s.ReleaseCrashes()
			err := answer(t, done, "after ReleaseCrashes")
			if !errors.Is(err, gitletfault.ErrCrashed) || !strings.Contains(err.Error(), "host.after_scratch") {
				t.Fatalf("a released crash = %v, want ErrCrashed naming host.after_scratch", err)
			}
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "the node was killed") {
				t.Fatalf("a released crash = %v, which answers its context: it must answer only the crash", err)
			}
		})
	})

	t.Run("a context already ended", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := new(gitletfault.Script)
			t.Cleanup(s.ReleaseCrashes)
			reached := make(chan struct{})
			s.Arm(gitletfault.HostUpload, gitletfault.Action{Crash: true, Reached: reached, At: 10})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			done := make(chan error, 1)
			go func() { done <- s.Hit(ctx, gitletfault.HostUpload, 10) }()
			stillHeld(t, done, "on a context ended before it fired")
			requireReached(t, reached)
			s.ReleaseCrashes()
			if err := answer(t, done, "after ReleaseCrashes"); !errors.Is(err, gitletfault.ErrCrashed) {
				t.Fatalf("a released crash = %v, want ErrCrashed", err)
			}
		})
	})

	t.Run("every hold, one release", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := new(gitletfault.Script)
			t.Cleanup(s.ReleaseCrashes)
			s.Arm(gitletfault.HostAfterUpload, gitletfault.Action{Crash: true})
			const pushes = 3
			done := make(chan error, pushes)
			for range pushes {
				go func() { done <- s.Hit(t.Context(), gitletfault.HostAfterUpload, 0) }()
			}
			stillHeld(t, done, "before ReleaseCrashes")
			if got := s.Hits(gitletfault.HostAfterUpload); got != pushes {
				t.Fatalf("Hits(%s) = %d with every hit parked, want %d", gitletfault.HostAfterUpload, got, pushes)
			}
			s.ReleaseCrashes()
			for range pushes {
				if err := answer(t, done, "after ReleaseCrashes"); !errors.Is(err, gitletfault.ErrCrashed) {
					t.Fatalf("a released crash = %v, want ErrCrashed", err)
				}
			}
		})
	})

	t.Run("after the release", func(t *testing.T) {
		s := new(gitletfault.Script)
		s.Arm(gitletfault.HostBeforeAdopt, gitletfault.Action{Crash: true})
		s.ReleaseCrashes()
		s.ReleaseCrashes() // the cleanup may release twice
		// What hits a crash once the case has judged — the teardown of the
		// node it restarted — is answered at once, or that teardown hangs.
		err := returns(t, func() error { return s.Hit(t.Context(), gitletfault.HostBeforeAdopt, 0) })
		if !errors.Is(err, gitletfault.ErrCrashed) {
			t.Fatalf("a crash hit after ReleaseCrashes = %v, want ErrCrashed at once", err)
		}
		(*gitletfault.Script)(nil).ReleaseCrashes() // a nil script holds nothing
	})

	t.Run("a block is not a crash", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := new(gitletfault.Script)
			s.Arm(gitletfault.HostAfterSync, gitletfault.Action{Block: make(chan struct{})})
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- s.Hit(ctx, gitletfault.HostAfterSync, 0) }()
			stillHeld(t, done, "before ReleaseCrashes")
			if got := s.Hits(gitletfault.HostAfterSync); got != 1 {
				t.Fatalf("Hits(%s) = %d with the hit parked, want 1", gitletfault.HostAfterSync, got)
			}
			s.ReleaseCrashes()
			stillHeld(t, done, "on ReleaseCrashes, which ends crashes and never a Block")
			cancel()
			if err := answer(t, done, "after its context ended"); !errors.Is(err, context.Canceled) {
				t.Fatalf("a Block whose context ended = %v, want context.Canceled", err)
			}
		})
	})
}

// Reached is how a case knows to Kill: it closes ONCE, at the first firing,
// however many hits an action without Once fires on — a second close would
// panic inside the call site, under whatever the case was crashing.
func TestReachedClosesOnceWhateverFires(t *testing.T) {
	ctx := t.Context()
	s := new(gitletfault.Script)
	reached := make(chan struct{})
	s.Arm(gitletfault.HostAfterPropose, gitletfault.Action{Reached: reached})
	select {
	case <-reached:
		t.Fatal("Reached was closed before the point was hit")
	default:
	}
	for range 3 {
		if err := returns(t, func() error { return s.Hit(ctx, gitletfault.HostAfterPropose, 0) }); err != nil {
			t.Fatalf("a hit of an action with no Err = %v, want nil", err)
		}
	}
	select {
	case <-reached:
	default:
		t.Fatal("Reached is still open after the point fired")
	}

	// Two points armed with one channel: whichever fires first closes it,
	// and the other firing must not close it again.
	both := make(chan struct{})
	s.Arm(gitletfault.SyncRefs, gitletfault.Action{Reached: both})
	s.Arm(gitletfault.SyncState, gitletfault.Action{Reached: both})
	for _, p := range []gitletfault.Point{gitletfault.SyncRefs, gitletfault.SyncState} {
		if err := returns(t, func() error { return s.Hit(ctx, p, 0) }); err != nil {
			t.Fatalf("Hit(%s) = %v, want nil", p, err)
		}
	}
}

func TestAnUploadFaultFiresAtItsOffset(t *testing.T) {
	ctx := t.Context()
	s := new(gitletfault.Script)
	s.Arm(gitletfault.HostUpload, gitletfault.Action{At: 100, Err: errCrash})
	for _, at := range []int64{0, 50, 99} {
		if err := s.Hit(ctx, gitletfault.HostUpload, at); err != nil {
			t.Fatalf("an upload hit at %d, before the offset 100 = %v, want nil", at, err)
		}
	}
	for _, at := range []int64{100, 200} {
		if err := s.Hit(ctx, gitletfault.HostUpload, at); !errors.Is(err, errCrash) {
			t.Fatalf("an upload hit at %d, at or past 100 = %v, want %v", at, err, errCrash)
		}
	}

	s.Arm(gitletfault.HostUpload, gitletfault.Action{At: 100, Once: true, Err: errCrash})
	if err := s.Hit(ctx, gitletfault.HostUpload, 50); err != nil {
		t.Fatalf("a Once upload hit before the offset = %v, want nil", err)
	}
	if err := s.Hit(ctx, gitletfault.HostUpload, 150); !errors.Is(err, errCrash) {
		t.Fatalf("the first upload hit past the offset = %v, want %v", err, errCrash)
	}
	if err := s.Hit(ctx, gitletfault.HostUpload, 200); err != nil {
		t.Fatalf("a later upload hit after a Once action fired = %v, want nil", err)
	}

	// THE COMPACTION'S UPLOAD READS ITS OFFSET TOO: a crash "mid-bundle"
	// armed at an offset must fire there, not at the output's first byte —
	// the rule spelled at the firing's gate as at the arming and the call
	// site, or a case crashing a home half way through an output would be
	// crashing it before any byte.
	s.Arm(gitletfault.CompactUpload, gitletfault.Action{At: 4096, Once: true, Err: errCrash})
	for _, at := range []int64{0, 1024, 4095} {
		if err := s.Hit(ctx, gitletfault.CompactUpload, at); err != nil {
			t.Fatalf("a compaction's upload hit at %d, before the offset 4096 = %v, want nil", at, err)
		}
	}
	if err := s.Hit(ctx, gitletfault.CompactUpload, 8192); !errors.Is(err, errCrash) {
		t.Fatalf("the first compaction upload hit past the offset = %v, want %v", err, errCrash)
	}
	if err := s.Hit(ctx, gitletfault.HostUpload, 8192); err != nil {
		t.Fatalf("a push's upload hit beside an armed compaction upload = %v, want nil: the two points are "+
			"two steps", err)
	}

	// AND SO DOES THE PUBLICATION'S: a part's offset is the layer's, an
	// output's its bundle's, and a crash armed at one fires there.
	s.Arm(gitletfault.PublishUpload, gitletfault.Action{At: 4096, Once: true, Err: errCrash})
	for _, at := range []int64{0, 1024, 4095} {
		if err := s.Hit(ctx, gitletfault.PublishUpload, at); err != nil {
			t.Fatalf("a publication's upload hit at %d, before the offset 4096 = %v, want nil", at, err)
		}
	}
	if err := s.Hit(ctx, gitletfault.PublishUpload, 8192); !errors.Is(err, errCrash) {
		t.Fatalf("the first publication upload hit past the offset = %v, want %v", err, errCrash)
	}
	if err := s.Hit(ctx, gitletfault.CompactUpload, 8192); err != nil {
		t.Fatalf("a compaction's upload hit beside an armed publication upload = %v, want nil: the two "+
			"points are two steps", err)
	}
}

// A case that arms something no hit can ever reveal is a case that can never
// fail where it means to, so arming one panics in the case rather than
// passing in silence.
func TestArmingAPointThatDoesNotExistPanics(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	for _, c := range []struct {
		name, says string
		arm        func(s *gitletfault.Script)
	}{
		{"an unknown point", `not a fault point: "x"`,
			func(s *gitletfault.Script) { s.Arm("x", gitletfault.Action{Err: errCrash}) }},
		{"an offset on a point that has none", "only host.upload",
			func(s *gitletfault.Script) { s.Arm(gitletfault.SyncRefs, gitletfault.Action{At: 1, Err: errCrash}) }},
		{"a negative offset", "negative",
			func(s *gitletfault.Script) { s.Arm(gitletfault.HostUpload, gitletfault.Action{At: -1, Err: errCrash}) }},
		{"a negative offset on the compaction's upload", "negative",
			func(s *gitletfault.Script) {
				s.Arm(gitletfault.CompactUpload, gitletfault.Action{At: -1, Err: errCrash})
			}},
		{"an offset on a compaction point that has none", "only host.upload, compact.upload and publish.upload",
			func(s *gitletfault.Script) {
				s.Arm(gitletfault.CompactAfterUpload, gitletfault.Action{At: 1, Err: errCrash})
			}},
		{"a negative offset on the publication's upload", "negative",
			func(s *gitletfault.Script) {
				s.Arm(gitletfault.PublishUpload, gitletfault.Action{At: -1, Err: errCrash})
			}},
		{"an offset on a publication point that has none", "only host.upload, compact.upload and publish.upload",
			func(s *gitletfault.Script) {
				s.Arm(gitletfault.PublishAfterBuild, gitletfault.Action{At: 1, Err: errCrash})
			}},
		{"an action that does nothing", "does nothing",
			func(s *gitletfault.Script) { s.Arm(gitletfault.SyncRefs, gitletfault.Action{Once: true}) }},
		{"a Reached already closed", "already closed",
			func(s *gitletfault.Script) { s.Arm(gitletfault.SyncRefs, gitletfault.Action{Reached: closed}) }},
		{"a crash with an Err it never answers", "crash",
			func(s *gitletfault.Script) {
				s.Arm(gitletfault.HostAfterSync, gitletfault.Action{Crash: true, Err: errCrash})
			}},
		{"a crash with a Block it never waits on", "crash",
			func(s *gitletfault.Script) {
				s.Arm(gitletfault.HostAfterSync, gitletfault.Action{Crash: true, Block: make(chan struct{})})
			}},
		{"a nil script", "nil",
			func(*gitletfault.Script) {
				(*gitletfault.Script)(nil).Arm(gitletfault.SyncRefs, gitletfault.Action{Err: errCrash})
			}},
		{"a disarm of an unknown point", `not a fault point: "x"`,
			func(s *gitletfault.Script) { s.Disarm("x") }},
		{"a count of an unknown point", `not a fault point: "x"`,
			func(s *gitletfault.Script) { s.Hits("x") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := panics(func() { c.arm(new(gitletfault.Script)) })
			if !ok {
				t.Fatal("did not panic")
			}
			if !strings.Contains(msg, c.says) {
				t.Fatalf("panicked with %q, want it to say %q", msg, c.says)
			}
		})
	}
}

// Hit is the CALL SITE's half, so a call site that names a point that does
// not exist, or hands an offset to a point that reads none, fails loudly at
// once — armed or not, scripted or not — rather than silently never firing.
func TestAHitOnAnUnknownPointFails(t *testing.T) {
	ctx := t.Context()
	for _, c := range []struct {
		name string
		s    *gitletfault.Script
	}{{"a script", new(gitletfault.Script)}, {"a nil script", nil}} {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.Hit(ctx, "x", 0)
			if !errors.Is(err, gitletfault.ErrNotAPoint) || !strings.Contains(err.Error(), `"x"`) {
				t.Fatalf(`Hit(ctx, "x", 0) = %v, want ErrNotAPoint naming "x"`, err)
			}
			// Each refusal names what the call site has to change: an
			// offset handed to a point that reads none, or a negative one
			// handed to the upload — which DOES read an offset, so its
			// refusal must not say it reads none.
			err = c.s.Hit(ctx, gitletfault.SyncState, 7)
			if !errors.Is(err, gitletfault.ErrOffset) || !strings.Contains(err.Error(), "sync.state reads no offset") ||
				!strings.Contains(err.Error(), "7") {
				t.Fatalf("Hit(ctx, SyncState, 7) = %v, want ErrOffset saying sync.state reads no offset", err)
			}
			for _, p := range []gitletfault.Point{gitletfault.HostUpload, gitletfault.CompactUpload, gitletfault.PublishUpload} {
				err = c.s.Hit(ctx, p, -1)
				if !errors.Is(err, gitletfault.ErrOffset) || !strings.Contains(err.Error(), "negative offset -1") {
					t.Fatalf("Hit(ctx, %s, -1) = %v, want ErrOffset naming the negative offset -1", p, err)
				}
				if msg := err.Error(); strings.Contains(msg, "only host.upload") || strings.Contains(msg, "reads no") {
					t.Fatalf("Hit(ctx, %s, -1) = %q, which says %s reads no offset", p, msg, p)
				}
			}
			err = c.s.Hit(ctx, gitletfault.CompactAfterBuild, 3)
			if !errors.Is(err, gitletfault.ErrOffset) ||
				!strings.Contains(err.Error(), "compact.after_build reads no offset") {
				t.Fatalf("Hit(ctx, CompactAfterBuild, 3) = %v, want ErrOffset saying compact.after_build reads "+
					"no offset", err)
			}
		})
	}
	s := new(gitletfault.Script)
	_ = s.Hit(ctx, "x", 0)
	_ = s.Hit(ctx, gitletfault.SyncState, 7)
	if got := s.Hits(gitletfault.SyncState); got != 0 {
		t.Fatalf("a refused hit was counted: Hits(SyncState) = %d", got)
	}
}
