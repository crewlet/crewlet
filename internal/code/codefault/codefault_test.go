package codefault_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/codefault"
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
	case <-time.After(hang):
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
	want := []codefault.Point{
		codefault.HostAfterCommands,    // the commands read, nothing started
		codefault.HostAfterSync,        // the cache synced linearizably
		codefault.HostAfterScratch,     // the scratch repository exists
		codefault.HostHookAdmitted,     // the hook reached the engine; the pack is in the scratch
		codefault.HostUpload,           // inside the bundle's upload
		codefault.HostAfterUpload,      // the bundle stored, no row naming it
		codefault.CommitterAfterAppend, // the record appended, the receiver not yet told
		codefault.HostAfterPropose,     // the committer answered
		codefault.HostBeforeAdopt,      // the row applied, the pack not yet adopted
		codefault.HostHookClose,        // the report not yet written
	}
	if got := codefault.PushPath(); !slices.Equal(got, want) {
		t.Fatalf("PushPath() = %v\nwant the order a push reaches them: %v", got, want)
	}

	syncPoints := []codefault.Point{
		codefault.SyncDownload,         // a pack's download in flight
		codefault.SyncRefs,             // the packs in, the refs not yet renamed in
		codefault.SyncState,            // the refs in, the state not yet renamed in
		codefault.SyncSwapBeforeRename, // the round applied, the next incarnation staged, not yet renamed in
		codefault.InstallAfterFetch,    // a published layer fetched and checked, nothing of it placed
	}
	// The compaction's crash matrix walks CompactPath() as F21's walks the
	// push path's, so its order is a claim too.
	compactPoints := []codefault.Point{
		codefault.CompactAfterBuild,   // the outputs written in the scratch, nothing stored
		codefault.CompactUpload,       // inside an output's upload
		codefault.CompactAfterUpload,  // every output stored, nothing proposed
		codefault.CompactAfterPropose, // the committer answered, nothing adopted
	}
	if got := codefault.CompactPath(); !slices.Equal(got, compactPoints) {
		t.Fatalf("CompactPath() = %v\nwant the order a compaction reaches them: %v", got, compactPoints)
	}
	// The publication's crash matrix walks PublishPath(), so its order is a
	// claim as well.
	publishPoints := []codefault.Point{
		codefault.PublishAfterBuild,   // the layer and any outputs written in the scratch, nothing stored
		codefault.PublishUpload,       // inside a part's or an output's upload
		codefault.PublishAfterUpload,  // every part and output stored, nothing proposed
		codefault.PublishAfterPropose, // the committer answered, nothing adopted
	}
	if got := codefault.PublishPath(); !slices.Equal(got, publishPoints) {
		t.Fatalf("PublishPath() = %v\nwant the order a publication reaches them: %v", got, publishPoints)
	}
	// An engine write's crash matrix walks WritePath(): its own point, then
	// the landing's, each in the order the one landing hits it — and none of
	// the relay's own, which a write never meets.
	writePoints := []codefault.Point{
		codefault.WriteAfterBuild,  // the commit and its pack made in the scratch, nothing stored
		codefault.HostUpload,       // inside the bundle's upload
		codefault.HostAfterUpload,  // the bundle stored, no row naming it
		codefault.HostAfterPropose, // the committer answered
		codefault.HostBeforeAdopt,  // the row applied, the pack not yet adopted
	}
	if got := codefault.WritePath(); !slices.Equal(got, writePoints) {
		t.Fatalf("WritePath() = %v\nwant the order an engine write reaches them: %v", got, writePoints)
	}
	// A merge's landing is the one landing, after its own build's point.
	mergePoints := []codefault.Point{
		codefault.MergeAfterBuild,  // the merge's commit and pack made in its scratch, nothing stored
		codefault.HostUpload,       // inside the bundle's upload
		codefault.HostAfterUpload,  // the bundle stored, no row naming it
		codefault.HostAfterPropose, // the committer answered the merge record
		codefault.HostBeforeAdopt,  // the record applied, the pack not yet adopted
	}
	if got := codefault.MergePath(); !slices.Equal(got, mergePoints) {
		t.Fatalf("MergePath() = %v\nwant the order a merge reaches them: %v", got, mergePoints)
	}
	// A chain's build reaches its own point alone: what follows it — its
	// bundle stored, its caller's record — is no landing's.
	if got := codefault.ChainPath(); !slices.Equal(got, []codefault.Point{codefault.ChainAfterBuild}) {
		t.Fatalf("ChainPath() = %v\nwant the chain's entries merged and its pack made, nothing stored", got)
	}
	// An expunge's crash matrix walks ExpungePath(), its order a claim as
	// the others are.
	expungePoints := []codefault.Point{
		codefault.ExpungeAfterBuild,        // kept and dropping packs written in the scratch, nothing stored
		codefault.ExpungeAfterBegin,        // the begin applied, the rescue not started
		codefault.ExpungeAfterRescueUpload, // every rescue pack stored, the commit not proposed
		codefault.ExpungeAfterCommit,       // the commit answered, its rescue not adopted
	}
	if got := codefault.ExpungePath(); !slices.Equal(got, expungePoints) {
		t.Fatalf("ExpungePath() = %v\nwant the order an expunge reaches them: %v", got, expungePoints)
	}
	// A check's run walks CheckPath(): its claim applied, before any box.
	if got := codefault.CheckPath(); !slices.Equal(got, []codefault.Point{codefault.CheckAfterClaim}) {
		t.Fatalf("CheckPath() = %v\nwant the claim applied, before any box", got)
	}
	// An import walks ImportPath() and a pull mirror's sync MirrorPath():
	// each its build, a step's packs stored, the committer's answer to it.
	importPoints := []codefault.Point{
		codefault.ImportAfterBuild,   // fetched and cut in its scratch, nothing stored
		codefault.ImportAfterUpload,  // a chunk's packs stored, no row names them
		codefault.ImportAfterPropose, // the chunk answered, its packs not adopted
	}
	if got := codefault.ImportPath(); !slices.Equal(got, importPoints) {
		t.Fatalf("ImportPath() = %v\nwant the order an import reaches them: %v", got, importPoints)
	}
	mirrorPoints := []codefault.Point{
		codefault.MirrorAfterBuild,   // fetched and cut in its scratch, nothing stored
		codefault.MirrorAfterUpload,  // the sync's packs stored, no row names them
		codefault.MirrorAfterPropose, // the sync answered, its packs not adopted
	}
	if got := codefault.MirrorPath(); !slices.Equal(got, mirrorPoints) {
		t.Fatalf("MirrorPath() = %v\nwant the order a sync reaches them: %v", got, mirrorPoints)
	}
	if got, wantAll := codefault.Points(), slices.Concat(want, syncPoints, compactPoints, publishPoints,
		[]codefault.Point{codefault.WriteAfterBuild, codefault.MergeAfterBuild, codefault.ChainAfterBuild},
		expungePoints, []codefault.Point{codefault.CheckAfterClaim}, importPoints,
		mirrorPoints); !slices.Equal(got, wantAll) {
		t.Fatalf("Points() = %v\nwant the push path, then the sync's, the compaction's and the publication's, "+
			"then the write's own, the merge's and the chain's, then the expunge's, then a check's run's, then "+
			"an import's and a pull mirror's sync's: %v", got, wantAll)
	}

	seen := map[codefault.Point]bool{}
	for _, p := range codefault.Points() {
		if !p.Valid() {
			t.Errorf("%q is listed and not Valid", p)
		}
		if seen[p] {
			t.Errorf("%q is listed twice: two steps would share one name", p)
		}
		seen[p] = true
	}
	for _, p := range []codefault.Point{"host.nope", "", "HOST.AFTER_SYNC", "host.after_sync "} {
		if p.Valid() {
			t.Errorf("Point(%q).Valid() = true; only the listed points exist", p)
		}
	}

	// Both lists are copies: a caller rewriting what it was handed changes
	// neither the next caller's answer nor what Valid accepts.
	path := codefault.PushPath()
	path[0] = "host.nope"
	compactions := codefault.CompactPath()
	compactions[0] = "host.nope"
	publications := codefault.PublishPath()
	publications[0] = "host.nope"
	writes := codefault.WritePath()
	writes[0], writes[1] = "host.nope", "host.nope"
	expunges := codefault.ExpungePath()
	expunges[0] = "host.nope"
	all := codefault.Points()
	all[0] = "host.nope"
	if codefault.PushPath()[0] != codefault.HostAfterCommands || codefault.Points()[0] != codefault.HostAfterCommands ||
		codefault.CompactPath()[0] != codefault.CompactAfterBuild ||
		codefault.PublishPath()[0] != codefault.PublishAfterBuild || codefault.WritePath()[0] != codefault.WriteAfterBuild ||
		codefault.ExpungePath()[0] != codefault.ExpungeAfterBuild ||
		codefault.WritePath()[1] != codefault.HostUpload || codefault.PushPath()[4] != codefault.HostUpload {
		t.Fatal("rewriting a returned list changed the next call's")
	}
	if codefault.Point("host.nope").Valid() {
		t.Fatal("rewriting a returned list made Valid accept a point no call site can hit")
	}
}

func TestAnUnarmedHitIsCountedAndPasses(t *testing.T) {
	ctx := t.Context()
	s := new(codefault.Script)
	for range 3 {
		if err := s.Hit(ctx, codefault.SyncRefs, 0); err != nil {
			t.Fatalf("an unarmed hit = %v, want nil", err)
		}
	}
	if err := s.Hit(ctx, codefault.HostUpload, 4096); err != nil {
		t.Fatalf("an unarmed upload hit = %v, want nil", err)
	}
	if got := s.Hits(codefault.SyncRefs); got != 3 {
		t.Fatalf("Hits(SyncRefs) = %d, want 3", got)
	}
	if got := s.Hits(codefault.HostUpload); got != 1 {
		t.Fatalf("Hits(HostUpload) = %d, want 1", got)
	}
	if got := s.Hits(codefault.SyncState); got != 0 {
		t.Fatalf("Hits(SyncState) = %d, want 0 for a point nothing hit", got)
	}

	// A typed nil reaching an Injector field passes the call site's nil
	// check, so it must hit nothing rather than crash the call site.
	var none *codefault.Script
	var inj codefault.Injector = none
	if err := inj.Hit(ctx, codefault.SyncDownload, 0); err != nil {
		t.Fatalf("a nil *Script's hit = %v, want nil", err)
	}
	if got := none.Hits(codefault.SyncDownload); got != 0 {
		t.Fatalf("a nil *Script counted %d hits", got)
	}
	none.Disarm(codefault.SyncDownload)
}

func TestADisarmedPointPasses(t *testing.T) {
	ctx := t.Context()
	s := new(codefault.Script)
	s.Arm(codefault.HostAfterSync, codefault.Action{Err: errCrash})
	if err := s.Hit(ctx, codefault.HostAfterSync, 0); !errors.Is(err, errCrash) {
		t.Fatalf("an armed hit = %v, want %v", err, errCrash)
	}
	s.Disarm(codefault.HostAfterSync)
	if err := s.Hit(ctx, codefault.HostAfterSync, 0); err != nil {
		t.Fatalf("a disarmed hit = %v, want nil", err)
	}
	if got := s.Hits(codefault.HostAfterSync); got != 2 {
		t.Fatalf("Hits = %d, want 2: a hit is counted armed or not", got)
	}
	s.Disarm(codefault.HostAfterSync) // disarming what is not armed is a no-op
}

func TestAOnceActionFiresOnce(t *testing.T) {
	ctx := t.Context()
	s := new(codefault.Script)
	s.Arm(codefault.HostAfterScratch, codefault.Action{Once: true, Err: errCrash})
	if err := s.Hit(ctx, codefault.HostAfterScratch, 0); !errors.Is(err, errCrash) {
		t.Fatalf("the first hit = %v, want %v", err, errCrash)
	}
	for range 3 {
		if err := s.Hit(ctx, codefault.HostAfterScratch, 0); err != nil {
			t.Fatalf("a hit after a Once action fired = %v, want nil", err)
		}
	}

	s.Arm(codefault.HostAfterScratch, codefault.Action{Err: errCrash})
	for i := range 3 {
		if err := s.Hit(ctx, codefault.HostAfterScratch, 0); !errors.Is(err, errCrash) {
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
	s := new(codefault.Script)
	s.Arm(codefault.SyncDownload, codefault.Action{Once: true, Err: errCrash})

	const hits = 64
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fired int
	)
	for range hits {
		wg.Go(func() {
			if err := s.Hit(ctx, codefault.SyncDownload, 0); errors.Is(err, errCrash) {
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
	if got := s.Hits(codefault.SyncDownload); got != hits {
		t.Fatalf("Hits = %d, want %d", got, hits)
	}
}

// A Block is the hold of a process that LIVES — two pushes ordered, a slot
// held, a client hanging up on a held request — so it ends with its channel
// or with the hit's context, whichever comes first, and the call site goes on
// as it would.
func TestABlockHoldsUntilReleasedOrCancelled(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		s := new(codefault.Script)
		block, reached := make(chan struct{}), make(chan struct{})
		s.Arm(codefault.HostHookAdmitted, codefault.Action{Block: block, Reached: reached, Err: errCrash})

		done := make(chan error, 1)
		go func() { done <- s.Hit(t.Context(), codefault.HostHookAdmitted, 0) }()
		select {
		case <-reached:
		case <-time.After(hang):
			t.Fatalf("Reached was not closed %s after the hit", hang)
		}
		select {
		case err := <-done:
			t.Fatalf("the hit returned %v while its Block was still open", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(block)
		select {
		case err := <-done:
			if !errors.Is(err, errCrash) {
				t.Fatalf("the released hit = %v, want its Err %v", err, errCrash)
			}
		case <-time.After(hang):
			t.Fatalf("the hit has not returned %s after its Block closed", hang)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		s := new(codefault.Script)
		reached := make(chan struct{})
		s.Arm(codefault.HostHookAdmitted, codefault.Action{Block: make(chan struct{}), Reached: reached})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Hit(ctx, codefault.HostHookAdmitted, 0) }()
		select {
		case <-reached:
		case <-time.After(hang):
			t.Fatalf("Reached was not closed %s after the hit", hang)
		}
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

	t.Run("cause", func(t *testing.T) {
		s := new(codefault.Script)
		s.Arm(codefault.HostHookAdmitted, codefault.Action{Block: make(chan struct{})})
		why := errors.New("the node was killed")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(why)
		if err := returns(t, func() error { return s.Hit(ctx, codefault.HostHookAdmitted, 0) }); !errors.Is(err, why) {
			t.Fatalf("a hit whose context ended = %v, want the context's cause %v", err, why)
		}
	})
}

// stillHeld fails the test if the hit whose answer arrives on done has
// returned within held: how long a case watches a hold it expects to last
// before it ends it.
func stillHeld(t *testing.T, done <-chan error, why string) {
	t.Helper()
	const held = 100 * time.Millisecond
	select {
	case err := <-done:
		t.Fatalf("the hit returned %v %s", err, why)
	case <-time.After(held):
	}
}

// answer waits, under the hang detector, for the hit whose answer arrives on
// done.
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

// waitHits waits, under the hang detector, until p has been hit n times.
func waitHits(t *testing.T, s *codefault.Script, p codefault.Point, n int) {
	t.Helper()
	deadline := time.Now().Add(hang)
	for s.Hits(p) < n {
		if time.Now().After(deadline) {
			t.Fatalf("Hits(%s) = %d after %s, want %d", p, s.Hits(p), hang, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A crash is the process-death shape, and a node killed inside a test process
// cancels every request it was serving. So a crash hit must OUTLIVE its
// context: one that ended with it would run the call site's error path — its
// deferred cleanup included — before the restart, and the crash matrix's
// restart assertions would pass on what that path tidied. It ends only when
// the case has judged the restarted node and its cleanup releases the
// crashes, and then it answers ErrCrashed so nothing after the point runs.
func TestACrashOutlivesItsContextUntilReleased(t *testing.T) {
	t.Run("held past its context", func(t *testing.T) {
		s := new(codefault.Script)
		reached := make(chan struct{})
		s.Arm(codefault.HostAfterScratch, codefault.Action{Crash: true, Reached: reached})

		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Hit(ctx, codefault.HostAfterScratch, 0) }()
		select {
		case <-reached:
		case <-time.After(hang):
			t.Fatalf("Reached was not closed %s after the hit", hang)
		}
		cancel(errors.New("the node was killed"))
		stillHeld(t, done, "once its context ended, as a request of a killed node: a crash runs no error path")

		s.ReleaseCrashes()
		err := answer(t, done, "after ReleaseCrashes")
		if !errors.Is(err, codefault.ErrCrashed) || !strings.Contains(err.Error(), "host.after_scratch") {
			t.Fatalf("a released crash = %v, want ErrCrashed naming host.after_scratch", err)
		}
		if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "the node was killed") {
			t.Fatalf("a released crash = %v, which answers its context: it must answer only the crash", err)
		}
	})

	t.Run("a context already ended", func(t *testing.T) {
		s := new(codefault.Script)
		reached := make(chan struct{})
		s.Arm(codefault.HostUpload, codefault.Action{Crash: true, Reached: reached, At: 10})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		done := make(chan error, 1)
		go func() { done <- s.Hit(ctx, codefault.HostUpload, 10) }()
		select {
		case <-reached:
		case <-time.After(hang):
			t.Fatalf("Reached was not closed %s after the hit", hang)
		}
		stillHeld(t, done, "on a context ended before it fired")
		s.ReleaseCrashes()
		if err := answer(t, done, "after ReleaseCrashes"); !errors.Is(err, codefault.ErrCrashed) {
			t.Fatalf("a released crash = %v, want ErrCrashed", err)
		}
	})

	t.Run("every hold, one release", func(t *testing.T) {
		s := new(codefault.Script)
		s.Arm(codefault.HostAfterUpload, codefault.Action{Crash: true})
		const pushes = 3
		done := make(chan error, pushes)
		for range pushes {
			go func() { done <- s.Hit(t.Context(), codefault.HostAfterUpload, 0) }()
		}
		waitHits(t, s, codefault.HostAfterUpload, pushes)
		stillHeld(t, done, "before ReleaseCrashes")
		s.ReleaseCrashes()
		for range pushes {
			if err := answer(t, done, "after ReleaseCrashes"); !errors.Is(err, codefault.ErrCrashed) {
				t.Fatalf("a released crash = %v, want ErrCrashed", err)
			}
		}
	})

	t.Run("after the release", func(t *testing.T) {
		s := new(codefault.Script)
		s.Arm(codefault.HostBeforeAdopt, codefault.Action{Crash: true})
		s.ReleaseCrashes()
		s.ReleaseCrashes() // the cleanup may release twice
		// What hits a crash once the case has judged — the teardown of the
		// node it restarted — is answered at once, or that teardown hangs.
		err := returns(t, func() error { return s.Hit(t.Context(), codefault.HostBeforeAdopt, 0) })
		if !errors.Is(err, codefault.ErrCrashed) {
			t.Fatalf("a crash hit after ReleaseCrashes = %v, want ErrCrashed at once", err)
		}
		(*codefault.Script)(nil).ReleaseCrashes() // a nil script holds nothing
	})

	t.Run("a block is not a crash", func(t *testing.T) {
		s := new(codefault.Script)
		s.Arm(codefault.HostAfterSync, codefault.Action{Block: make(chan struct{})})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Hit(ctx, codefault.HostAfterSync, 0) }()
		waitHits(t, s, codefault.HostAfterSync, 1)
		s.ReleaseCrashes()
		stillHeld(t, done, "on ReleaseCrashes, which ends crashes and never a Block")
		cancel()
		if err := answer(t, done, "after its context ended"); !errors.Is(err, context.Canceled) {
			t.Fatalf("a Block whose context ended = %v, want context.Canceled", err)
		}
	})
}

// Reached is how a case knows to Kill: it closes ONCE, at the first firing,
// however many hits an action without Once fires on — a second close would
// panic inside the call site, under whatever the case was crashing.
func TestReachedClosesOnceWhateverFires(t *testing.T) {
	ctx := t.Context()
	s := new(codefault.Script)
	reached := make(chan struct{})
	s.Arm(codefault.HostAfterPropose, codefault.Action{Reached: reached})
	select {
	case <-reached:
		t.Fatal("Reached was closed before the point was hit")
	default:
	}
	for range 3 {
		if err := returns(t, func() error { return s.Hit(ctx, codefault.HostAfterPropose, 0) }); err != nil {
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
	s.Arm(codefault.SyncRefs, codefault.Action{Reached: both})
	s.Arm(codefault.SyncState, codefault.Action{Reached: both})
	for _, p := range []codefault.Point{codefault.SyncRefs, codefault.SyncState} {
		if err := returns(t, func() error { return s.Hit(ctx, p, 0) }); err != nil {
			t.Fatalf("Hit(%s) = %v, want nil", p, err)
		}
	}
}

func TestAnUploadFaultFiresAtItsOffset(t *testing.T) {
	ctx := t.Context()
	s := new(codefault.Script)
	s.Arm(codefault.HostUpload, codefault.Action{At: 100, Err: errCrash})
	for _, at := range []int64{0, 50, 99} {
		if err := s.Hit(ctx, codefault.HostUpload, at); err != nil {
			t.Fatalf("an upload hit at %d, before the offset 100 = %v, want nil", at, err)
		}
	}
	for _, at := range []int64{100, 200} {
		if err := s.Hit(ctx, codefault.HostUpload, at); !errors.Is(err, errCrash) {
			t.Fatalf("an upload hit at %d, at or past 100 = %v, want %v", at, err, errCrash)
		}
	}

	s.Arm(codefault.HostUpload, codefault.Action{At: 100, Once: true, Err: errCrash})
	if err := s.Hit(ctx, codefault.HostUpload, 50); err != nil {
		t.Fatalf("a Once upload hit before the offset = %v, want nil", err)
	}
	if err := s.Hit(ctx, codefault.HostUpload, 150); !errors.Is(err, errCrash) {
		t.Fatalf("the first upload hit past the offset = %v, want %v", err, errCrash)
	}
	if err := s.Hit(ctx, codefault.HostUpload, 200); err != nil {
		t.Fatalf("a later upload hit after a Once action fired = %v, want nil", err)
	}

	// THE COMPACTION'S UPLOAD READS ITS OFFSET TOO: a crash "mid-bundle"
	// armed at an offset must fire there, not at the output's first byte —
	// the rule spelled at the firing's gate as at the arming and the call
	// site, or a case crashing a home half way through an output would be
	// crashing it before any byte.
	s.Arm(codefault.CompactUpload, codefault.Action{At: 4096, Once: true, Err: errCrash})
	for _, at := range []int64{0, 1024, 4095} {
		if err := s.Hit(ctx, codefault.CompactUpload, at); err != nil {
			t.Fatalf("a compaction's upload hit at %d, before the offset 4096 = %v, want nil", at, err)
		}
	}
	if err := s.Hit(ctx, codefault.CompactUpload, 8192); !errors.Is(err, errCrash) {
		t.Fatalf("the first compaction upload hit past the offset = %v, want %v", err, errCrash)
	}
	if err := s.Hit(ctx, codefault.HostUpload, 8192); err != nil {
		t.Fatalf("a push's upload hit beside an armed compaction upload = %v, want nil: the two points are "+
			"two steps", err)
	}

	// AND SO DOES THE PUBLICATION'S: a part's offset is the layer's, an
	// output's its bundle's, and a crash armed at one fires there.
	s.Arm(codefault.PublishUpload, codefault.Action{At: 4096, Once: true, Err: errCrash})
	for _, at := range []int64{0, 1024, 4095} {
		if err := s.Hit(ctx, codefault.PublishUpload, at); err != nil {
			t.Fatalf("a publication's upload hit at %d, before the offset 4096 = %v, want nil", at, err)
		}
	}
	if err := s.Hit(ctx, codefault.PublishUpload, 8192); !errors.Is(err, errCrash) {
		t.Fatalf("the first publication upload hit past the offset = %v, want %v", err, errCrash)
	}
	if err := s.Hit(ctx, codefault.CompactUpload, 8192); err != nil {
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
		arm        func(s *codefault.Script)
	}{
		{"an unknown point", `not a fault point: "x"`,
			func(s *codefault.Script) { s.Arm("x", codefault.Action{Err: errCrash}) }},
		{"an offset on a point that has none", "only host.upload",
			func(s *codefault.Script) { s.Arm(codefault.SyncRefs, codefault.Action{At: 1, Err: errCrash}) }},
		{"a negative offset", "negative",
			func(s *codefault.Script) { s.Arm(codefault.HostUpload, codefault.Action{At: -1, Err: errCrash}) }},
		{"a negative offset on the compaction's upload", "negative",
			func(s *codefault.Script) { s.Arm(codefault.CompactUpload, codefault.Action{At: -1, Err: errCrash}) }},
		{"an offset on a compaction point that has none", "only host.upload, compact.upload and publish.upload",
			func(s *codefault.Script) {
				s.Arm(codefault.CompactAfterUpload, codefault.Action{At: 1, Err: errCrash})
			}},
		{"a negative offset on the publication's upload", "negative",
			func(s *codefault.Script) { s.Arm(codefault.PublishUpload, codefault.Action{At: -1, Err: errCrash}) }},
		{"an offset on a publication point that has none", "only host.upload, compact.upload and publish.upload",
			func(s *codefault.Script) {
				s.Arm(codefault.PublishAfterBuild, codefault.Action{At: 1, Err: errCrash})
			}},
		{"an action that does nothing", "does nothing",
			func(s *codefault.Script) { s.Arm(codefault.SyncRefs, codefault.Action{Once: true}) }},
		{"a Reached already closed", "already closed",
			func(s *codefault.Script) { s.Arm(codefault.SyncRefs, codefault.Action{Reached: closed}) }},
		{"a crash with an Err it never answers", "crash",
			func(s *codefault.Script) {
				s.Arm(codefault.HostAfterSync, codefault.Action{Crash: true, Err: errCrash})
			}},
		{"a crash with a Block it never waits on", "crash",
			func(s *codefault.Script) {
				s.Arm(codefault.HostAfterSync, codefault.Action{Crash: true, Block: make(chan struct{})})
			}},
		{"a nil script", "nil",
			func(*codefault.Script) {
				(*codefault.Script)(nil).Arm(codefault.SyncRefs, codefault.Action{Err: errCrash})
			}},
		{"a disarm of an unknown point", `not a fault point: "x"`,
			func(s *codefault.Script) { s.Disarm("x") }},
		{"a count of an unknown point", `not a fault point: "x"`,
			func(s *codefault.Script) { s.Hits("x") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg, ok := panics(func() { c.arm(new(codefault.Script)) })
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
		s    *codefault.Script
	}{{"a script", new(codefault.Script)}, {"a nil script", nil}} {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.Hit(ctx, "x", 0)
			if !errors.Is(err, codefault.ErrNotAPoint) || !strings.Contains(err.Error(), `"x"`) {
				t.Fatalf(`Hit(ctx, "x", 0) = %v, want ErrNotAPoint naming "x"`, err)
			}
			// Each refusal names what the call site has to change: an
			// offset handed to a point that reads none, or a negative one
			// handed to the upload — which DOES read an offset, so its
			// refusal must not say it reads none.
			err = c.s.Hit(ctx, codefault.SyncState, 7)
			if !errors.Is(err, codefault.ErrOffset) || !strings.Contains(err.Error(), "sync.state reads no offset") ||
				!strings.Contains(err.Error(), "7") {
				t.Fatalf("Hit(ctx, SyncState, 7) = %v, want ErrOffset saying sync.state reads no offset", err)
			}
			for _, p := range []codefault.Point{codefault.HostUpload, codefault.CompactUpload, codefault.PublishUpload} {
				err = c.s.Hit(ctx, p, -1)
				if !errors.Is(err, codefault.ErrOffset) || !strings.Contains(err.Error(), "negative offset -1") {
					t.Fatalf("Hit(ctx, %s, -1) = %v, want ErrOffset naming the negative offset -1", p, err)
				}
				if msg := err.Error(); strings.Contains(msg, "only host.upload") || strings.Contains(msg, "reads no") {
					t.Fatalf("Hit(ctx, %s, -1) = %q, which says %s reads no offset", p, msg, p)
				}
			}
			err = c.s.Hit(ctx, codefault.CompactAfterBuild, 3)
			if !errors.Is(err, codefault.ErrOffset) ||
				!strings.Contains(err.Error(), "compact.after_build reads no offset") {
				t.Fatalf("Hit(ctx, CompactAfterBuild, 3) = %v, want ErrOffset saying compact.after_build reads "+
					"no offset", err)
			}
		})
	}
	s := new(codefault.Script)
	_ = s.Hit(ctx, "x", 0)
	_ = s.Hit(ctx, codefault.SyncState, 7)
	if got := s.Hits(codefault.SyncState); got != 0 {
		t.Fatalf("a refused hit was counted: Hits(SyncState) = %d", got)
	}
}
