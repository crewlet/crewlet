package cache_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE INSTALLER'S TRIES (layers.go): when a repository's next try is due,
// what a try that failed waits, and what decides a layer refused — each held
// on a case's own clock, the installer's passes run one at a time by the case
// (cache.InstallPass).

// A MISSING LAYER THE LOG'S HEAD CANNOT DECIDE WAITS AS A STORE FAILURE DOES: a
// part the object store holds nothing for is refused only once a read at the
// log's head says the rows still name the layer, and while that read fails
// nothing is decided — so the try is one that may pass, and the repository
// waits installRetryBase before the store is asked again, rather than being
// taken up by the very next pass: a GET, a staging and a failed round of the
// repository at every pass for as long as the head cannot be read, the whole
// window after a restore. Logged code_layer_install_failed with its outcome and
// its wait. Once the head answers, the layer is refused missing.
//
// Mutation: settle a miss the head did not decide as a decided one
// (settleInstall's undecided case dropped) — the second pass at the same
// instant asks the store again, and no wait is recorded.
func TestAMissingLayerTheHeadCannotDecideWaits(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 2)
	clk := newClock()
	c := w.open(t, clk.on)
	w.sync(t, c, repo).Release()
	chain := w.buildChain(t, p)
	lost := chain[0].Parts[0].Object
	w.objects.mu.Lock()
	w.objects.fail = func(k objstore.Key) error {
		if k == lost {
			return objstore.ErrNotFound
		}
		return nil
	}
	w.objects.mu.Unlock()
	w.log.publish(repo, chain, nil)
	w.sync(t, c, repo).Release()
	unreadable := errors.New("the estate is catching up")
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if call.Level == statelog.ReadLinearizable {
				return unreadable
			}
			return nil
		}
	})

	cache.InstallPass(c)
	if got := cache.Refused(c, repo); len(got) != 0 {
		t.Fatalf("the layer is refused %v while the log's head could not be read", got)
	}
	at, failures := cache.InstallWait(c, repo)
	if want := clk.Now().Add(time.Minute); !at.Equal(want) || failures != 1 {
		t.Fatalf("the repository's next try is due at %v after %d failures, want %v after one: a miss the head "+
			"did not decide is a failure that may pass", at, failures, want)
	}
	lines := w.logs.lines(t, "code_layer_install_failed")
	if len(lines) != 1 || lines[0]["outcome"] != "missing" || lines[0]["retry_in"] != float64(time.Minute) {
		t.Errorf("code_layer_install_failed = %v, want one line with outcome missing and retry_in a minute", lines)
	}
	gets := w.gets(partsOf(chain...))
	cache.InstallPass(c)
	if got := w.gets(partsOf(chain...)); got != gets {
		t.Errorf("a pass at the same instant asked the store for the layer again (%d asks, then %d): the "+
			"repository waits out its try", gets, got)
	}

	// The head answers again and the wait ends: the layer is refused missing.
	w.log.set(func(f *fakeLog) { f.hook = nil })
	clk.Add(time.Minute)
	cache.InstallPass(c)
	if got := cache.Refused(c, repo)[chain[0].LayerID]; got != "missing" {
		t.Errorf("once the head answers, the layer the store lost is refused %q, want missing", got)
	}
}

// A LAYER INSTALL THAT FAILED IS COUNTED: a try of a layer the store failed to
// answer — a full round's, which answers without the layer and leaves it to
// the installer, and the installer's, which waits and tries again — is
// crewlet.code.cache.layers{outcome=failed}, once a try, beside its
// code_layer_install_failed line: the count an operator reads an object store
// failing the installer by.
//
// Mutation: count no failed try (the countLayers of settleInstall's and
// roundLayerFailed's failure dropped) — the counter stays at zero.
func TestAFailedLayerInstallIsCounted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 2)
	chain := w.buildChain(t, p)
	w.refuseParts(chain...)
	w.log.publish(repo, chain, nil)
	c := w.open(t, newClock().on)

	w.sync(t, c, repo).Release()
	if got := cache.Installed(c, repo); len(got) != 0 {
		t.Fatalf("the cold round installed %v from a store refusing the layer's part", layerIDs(got))
	}
	if got := w.layerCount("failed"); got != 1 {
		t.Errorf("after the full round's failed fetch crewlet.code.cache.layers{outcome=failed} = %d, want 1", got)
	}
	cache.InstallPass(c)
	if got := w.layerCount("failed"); got != 2 {
		t.Errorf("after the installer's failed try crewlet.code.cache.layers{outcome=failed} = %d, want 2", got)
	}
	if got := len(w.logs.lines(t, "code_layer_install_failed")); got != 2 {
		t.Errorf("code_layer_install_failed logged %d times, want once a failed try", got)
	}
}

// THE LAYERS A FAILED SWAP WAS TO PLACE ARE COUNTED: a placement on a copy a
// reader may hold is a swap's layer step, and a swap that fails at a step — a
// write this node's disk failed, here at the rename's fault point — places
// none of the layers it was to: a try that may pass, the installer's to try
// again, counted crewlet.code.cache.layers{outcome=failed} once whoever's the
// placement was. "a full round's": a retirement sweep's full round, whose swap
// was to place the chain, answers from the copy as it was and leaves the chain
// to the installer, which places it. "the installer's": counted once, never
// twice.
//
// Mutations, each seen red: count only the installer's (the swap's count
// dropped, installRepo's back) — "a full round's" counts nothing; count both —
// "the installer's" counts two.
func TestTheLayersAFailedSwapWasToPlaceAreCounted(t *testing.T) {
	t.Parallel()
	// open is a copy synced cold, its chain published since and not read
	// yet, and the fault script its cache runs.
	open := func(t *testing.T) (*syncWorld, *cache.Cache, *codefault.Script, string, []code.LayerRef) {
		t.Helper()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		p := w.pushN(t, repo, 2)
		faults := &codefault.Script{}
		c := w.open(t, newClock().on, func(o *cache.Options) { o.Faults = faults })
		w.sync(t, c, repo).Release()
		chain := w.buildChain(t, p)
		w.log.publish(repo, chain, nil)
		return w, c, faults, repo, chain
	}
	failed := func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, chain []code.LayerRef) {
		t.Helper()
		if got := w.layerCount("failed"); got != uint64(len(chain)) {
			t.Errorf("crewlet.code.cache.layers{outcome=failed} = %d, want %d: each layer the swap was to place, "+
				"once", got, len(chain))
		}
		if lines := w.logs.lines(t, "code_cache_swap_failed"); len(lines) != 1 || lines[0]["step"] != "rename" {
			t.Errorf("code_cache_swap_failed = %v, want one line at the rename's step", lines)
		}
		if got := cache.Installed(c, repo); len(got) != 0 {
			t.Errorf("the copy holds %v after the swap that was to place it failed", layerIDs(got))
		}
	}

	t.Run("a full round's", func(t *testing.T) {
		t.Parallel()
		w, c, faults, repo, chain := open(t)
		faults.Arm(codefault.SyncSwapBeforeRename, codefault.Action{Once: true, Err: errKilled})
		w.log.write(repo, nil, nil)
		w.log.sweepRetired(repo, w.log.head())
		s := w.sync(t, c, repo)
		if s.Kind != cache.SyncFull {
			t.Fatalf("the round below the sweep was %s, want a full one", s.Kind)
		}
		s.Release()
		failed(t, w, c, repo, chain)
		cache.InstallPass(c)
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
			t.Errorf("the installer's pass after the round left the copy holding %v, want %v", got,
				layerIDs(chain))
		}
	})
	t.Run("the installer's", func(t *testing.T) {
		t.Parallel()
		w, c, faults, repo, chain := open(t)
		w.sync(t, c, repo).Release()
		faults.Arm(codefault.SyncSwapBeforeRename, codefault.Action{Once: true, Err: errKilled})
		cache.InstallPass(c)
		failed(t, w, c, repo, chain)
	})
}

// refuseParts makes the world's store refuse every part of chain with an
// error that may pass — a bucket slowing its callers down — and answers the
// call that makes it answer again.
func (w *syncWorld) refuseParts(chain ...code.LayerRef) func() {
	parts := partsOf(chain...)
	slowDown := errors.New("SlowDown: please reduce your request rate")
	w.objects.mu.Lock()
	w.objects.fail = func(k objstore.Key) error {
		if slices.Contains(parts, k.String()) {
			return slowDown
		}
		return nil
	}
	w.objects.mu.Unlock()
	return func() {
		w.objects.mu.Lock()
		w.objects.fail = nil
		w.objects.mu.Unlock()
	}
}

// A STALE LAYER'S WAIT IS ITS OWN EPISODE'S: a layer the rows no longer name
// keeps serving, its packs held, while its replacement cannot be placed, and is
// cut once IT has stood in for installRetryCap — never by the clock of an
// earlier episode. Here a repacking publication's layer is refused by the
// store, so the layer it replaces stands in; a full round then places the
// replacement itself, the store answering again — an end the installer never
// sees. Two hours on, the next repacking publication's layer is refused too:
// its first failed fetch keeps the layer it replaces, which has stood in for no
// time at all, and the cut lands exactly at installRetryCap of its own, the
// pack only it held leaving with it. And an install that finds nothing due —
// the installer waking once the first episode's wait ended — forgets the
// repository's wait.
//
// Mutations: key the stale clock to the repository alone (stuckFor matching
// any layer recorded stale) — "unobserved" cuts at the first failed fetch;
// keep a repository's wait through an install that found nothing due
// (installRepo's forget dropped) — "observed" still waits out the old
// episode's failure.
func TestAStaleLayersWaitIsItsOwnEpisodes(t *testing.T) {
	t.Parallel()
	// resolved is a copy whose stale layer stood in while the store refused
	// its replacement, until a full round placed the replacement itself: its
	// world, cache and clock, the repository, its pushes, and the chain and
	// its one pack the copy holds now.
	type resolved struct {
		w      *syncWorld
		c      *cache.Cache
		clk    *clock
		repo   string
		p      []pushed
		merged code.PackRow
		chain  []code.LayerRef
	}
	setup := func(t *testing.T) resolved {
		t.Helper()
		r := resolved{w: newSyncWorld(t, onMemory), clk: newClock()}
		w := r.w
		r.repo = w.repo(t, 1)
		r.p = w.pushN(t, r.repo, 3)
		old := w.buildChain(t, r.p[:2])
		w.log.publish(r.repo, old, nil)
		r.c = w.open(t, r.clk.on)
		w.sync(t, r.c, r.repo).Release()
		r.merged = w.merged(t, r.p[0].row, r.p[1].row)
		r.chain = w.buildChain(t, []pushed{{oid: r.p[1].oid, row: r.merged}})
		answer := w.refuseParts(r.chain...)
		w.log.publish(r.repo, r.chain, []code.PackRow{r.merged}, r.p[0].row.PackID, r.p[1].row.PackID)
		w.sync(t, r.c, r.repo).Release()
		cache.InstallPass(r.c)
		if got := layerIDs(cache.Installed(r.c, r.repo)); !slices.Equal(got, layerIDs(old)) {
			t.Fatalf("with the replacement refused, the copy holds %v, want the stale %v standing in", got,
				layerIDs(old))
		}
		answer()
		cache.Reread(r.c, cache.RebuildStream)
		w.sync(t, r.c, r.repo).Release()
		if got := layerIDs(cache.Installed(r.c, r.repo)); !slices.Equal(got, layerIDs(r.chain)) {
			t.Fatalf("the full round left the copy holding %v, want the replacement %v", got, layerIDs(r.chain))
		}
		return r
	}

	t.Run("unobserved", func(t *testing.T) {
		t.Parallel()
		r := setup(t)
		w, c, clk, repo := r.w, r.c, r.clk, r.repo
		clk.Add(2 * time.Hour)
		merged := w.merged(t, r.merged, r.p[2].row)
		next := w.buildChain(t, []pushed{{oid: r.p[2].oid, row: merged}})
		w.refuseParts(next...)
		w.log.publish(repo, next, []code.PackRow{merged}, r.merged.PackID, r.p[2].row.PackID)
		w.sync(t, c, repo).Release()
		began := clk.Now()
		cache.InstallPass(c)
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(r.chain)) {
			t.Fatalf("the replacement's first failed fetch left the copy holding %v, want %v kept: the layer "+
				"it replaces has stood in for no time at all", got, layerIDs(r.chain))
		}

		clk.Set(began.Add(cache.InstallRetryCap - time.Second))
		cache.InstallPass(c)
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(r.chain)) {
			t.Fatalf("a second short of installRetryCap the copy holds %v, want %v kept", got, layerIDs(r.chain))
		}
		if at, _ := cache.InstallWait(c, repo); !at.Equal(began.Add(cache.InstallRetryCap)) {
			t.Errorf("the next try is due at %v, want the moment the stale layer has stood in for "+
				"installRetryCap, %v", at, began.Add(cache.InstallRetryCap))
		}
		clk.Set(began.Add(cache.InstallRetryCap))
		cache.InstallPass(c)
		if got := cache.Installed(c, repo); len(got) != 0 {
			t.Fatalf("at installRetryCap the copy holds %v, want the stale layer cut", layerIDs(got))
		}
		s := w.sync(t, c, repo)
		if exists(filepath.Join(s.Dir, "objects", "pack", "pack-"+r.merged.GitChecksum+".pack")) {
			t.Errorf("pack-%s, retired and held only by the stale layer, is still in the copy after its cut",
				r.merged.GitChecksum)
		}
	})

	t.Run("observed", func(t *testing.T) {
		t.Parallel()
		r := setup(t)
		r.clk.Add(time.Minute)
		cache.InstallPass(r.c)
		if at, failures := cache.InstallWait(r.c, r.repo); !at.IsZero() || failures != 0 {
			t.Errorf("after a try that found nothing due, the repository waits until %v after %d failures: "+
				"an install that ends with nothing due forgets its wait", at, failures)
		}
	})
}

// AN INSTALLER PASS COSTS WHAT IT TAKES UP, NOT WHAT THE INSTALLER HOLDS: a
// node catching up after a restore, or after a burst of publications, holds
// thousands of repositories — due a try, or waiting out the backoff a store's
// outage gave them — and a pass runs at every kick: every task's end, and every
// round of a repository the installer holds, one waiting included. Draining
// 10 000 repositories due a try takes each in hand once, to start it; kicking
// a thousand of those waiting takes none of them in hand while their waits
// hold; and once the waits end each is taken in hand twice — moved among the
// due, then started — never once a pass for every repository held: about Q²/32
// at Q held and sixteen tasks a pass, some six million at 10 000, all of it
// under the lock every round's kick, every task's end and every full round's
// install waits on.
//
// Mutation: walk every repository held at every pass (installPass scanning
// the whole queue, as it did) — draining takes millions in hand.
func TestAnInstallPassCostsWhatItTakesUp(t *testing.T) {
	t.Parallel()
	clk := newClock()
	c := newWorld(t).open(t, clk.on)
	const held = 10_000
	for i := range held {
		cache.HoldInstallUntil(c, fmt.Sprintf("waiting-%05d", i), clk.Now().Add(time.Hour))
		cache.KickInstall(c, fmt.Sprintf("due-%05d", i), false)
	}
	drain := func() int {
		took := 0
		for {
			n := len(cache.InstallPass(c))
			if n == 0 {
				return took
			}
			took += n
		}
	}

	before := cache.InstallHandled(c)
	if took := drain(); took != held {
		t.Fatalf("the passes took up %d repositories, want the %d due", took, held)
	}
	if got := cache.InstallHandled(c) - before; got != held {
		t.Errorf("draining %d repositories due a try took %d in hand, want each once", held, got)
	}

	before = cache.InstallHandled(c)
	for i := range 1000 {
		cache.KickInstall(c, fmt.Sprintf("waiting-%05d", i), false)
		cache.InstallPass(c)
	}
	if got := cache.InstallHandled(c) - before; got != 0 {
		t.Errorf("a thousand kicks of repositories waiting out their backoff, a pass each, took %d in hand, "+
			"want none while their waits hold", got)
	}

	clk.Add(time.Hour)
	before = cache.InstallHandled(c)
	if took := drain(); took != held {
		t.Fatalf("once their waits ended the passes took up %d repositories, want the %d waiting", took, held)
	}
	if got := cache.InstallHandled(c) - before; got != 2*held {
		t.Errorf("draining %d repositories whose waits ended took %d in hand, want each twice: moved among "+
			"the due, then started", held, got)
	}
}

// A KICK WHILE A TASK RUNS THE REPOSITORY HOLDS IT ONCE MORE: a round that
// brings new rows while the installer is trying the repository — between the
// try's end and its settling — is not lost to a task already done with the
// rows before them. The repository is held once more as the task ends, its
// wait ended where the round served the chain, and the next pass installs
// what the round brought: after a try that placed the chain it had ("done"),
// and after one the store failed ("failed").
//
// Mutations: let a kick of a running repository say nothing (kickInstall's
// running case dropped) — "done" lets the repository go and the new chain
// waits for its next round; let a fresh kick of a running repository end no
// wait (settleInstall's againFresh dropped) — "failed" holds the new chain
// for the old one's failure.
func TestAKickWhileATaskRunsTheRepositoryHoldsItOnceMore(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "done", true: "failed"}[failed], func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			p := w.pushN(t, repo, 3)
			settling, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			c := w.open(t, newClock().on, cache.InstallSettlingWith(func(string) {
				once.Do(func() {
					close(settling)
					<-release
				})
			}))
			w.sync(t, c, repo).Release()
			b := w.newChainBuild(t, p[:2], p[2:])
			root := b.layer(t, p[:2], false)
			top := b.layer(t, p[2:], false)
			answer := func() {}
			if failed {
				answer = w.refuseParts(root)
			}
			w.log.publish(repo, []code.LayerRef{root}, nil)
			w.sync(t, c, repo).Release()

			pass := make(chan []string, 1)
			go func() { pass <- cache.InstallPass(c) }()
			select {
			case <-settling:
			case <-time.After(hangDetector):
				t.Fatalf("the installer's task did not end its try within %v", hangDetector)
			}
			// The try has ended and is not settled yet: the store answers,
			// and the maintainer's next publication arrives through a delta.
			answer()
			chain := []code.LayerRef{root, top}
			w.log.publish(repo, chain, nil)
			w.sync(t, c, repo).Release()
			close(release)
			<-pass

			cache.InstallPass(c)
			if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(chain)) {
				t.Errorf("the pass after the task's end left the copy holding %v, want the chain the round "+
					"brought while the task ran, %v", got, layerIDs(chain))
			}
		})
	}
}

// THE INSTALLER WAITS OUT A STORE THAT FAILS: each try of a repository the
// store fails to answer waits installRetryBase, doubled after each failure in a
// row and never past installRetryCap — 1, 2, 4, 8, 16 and then 30 minutes, at
// most one try of the repository a node every half hour while it keeps
// failing — logged code_layer_install_failed with that wait (retry_in); a pass
// before the wait ends asks the store nothing, and one at its end tries again.
//
// Mutations: an uncapped window (installWindow without its cap) — the sixth
// wait is 32 minutes; a window that does not double — the second is a minute.
func TestTheInstallerWaitsOutAStoreThatFails(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 2)
	clk := newClock()
	c := w.open(t, clk.on)
	w.sync(t, c, repo).Release()
	chain := w.buildChain(t, p)
	w.refuseParts(chain...)
	w.log.publish(repo, chain, nil)
	w.sync(t, c, repo).Release()

	parts := partsOf(chain...)
	for n, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, cache.InstallRetryCap, cache.InstallRetryCap} {
		gets := w.gets(parts)
		cache.InstallPass(c)
		if got := w.gets(parts) - gets; got != len(parts) {
			t.Fatalf("try %d asked the store for the layer's parts %d times, want once each", n+1, got)
		}
		at, failures := cache.InstallWait(c, repo)
		if failures != n+1 || !at.Equal(clk.Now().Add(wait)) {
			t.Fatalf("after try %d the next is due at %v after %d failures, want %v after %d", n+1, at, failures,
				clk.Now().Add(wait), n+1)
		}
		lines := w.logs.lines(t, "code_layer_install_failed")
		if len(lines) != n+1 || lines[n]["retry_in"] != float64(wait) {
			t.Fatalf("try %d logged code_layer_install_failed %d times in all, the last %v: want once a try, "+
				"retry_in %v", n+1, len(lines), lines[len(lines)-1], wait)
		}
		clk.Set(at.Add(-time.Second))
		gets = w.gets(parts)
		cache.InstallPass(c)
		if got := w.gets(parts); got != gets {
			t.Fatalf("a pass a second before try %d's wait ended asked the store again", n+2)
		}
		clk.Set(at)
	}
	if got := w.layerCount("failed"); got != 7 {
		t.Errorf("crewlet.code.cache.layers{outcome=failed} = %d, want the seven tries", got)
	}
}

// A ROUND THAT SERVES THE CHAIN ENDS THE INSTALLER'S WAIT: a new publication
// reaches the node as new rows through a delta, which is a new try of the
// repository — the installer takes it up at once, whatever its wait after
// failures of the chain before — rather than through the retry the wait holds
// back.
//
// Mutation: drop the wait's end at a round that serves the chain
// (kickInstall's fresh) — the pass at the delta's instant asks the store
// nothing, and the new chain waits out the old one's failures.
func TestARoundThatServesTheChainEndsTheInstallersWait(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 3)
	clk := newClock()
	c := w.open(t, clk.on)
	w.sync(t, c, repo).Release()
	b := w.newChainBuild(t, p[:2], p[2:])
	root := b.layer(t, p[:2], false)
	top := b.layer(t, p[2:], false)
	answer := w.refuseParts(root)
	w.log.publish(repo, []code.LayerRef{root}, nil)
	w.sync(t, c, repo).Release()
	for range 3 {
		cache.InstallPass(c)
		at, _ := cache.InstallWait(c, repo)
		clk.Set(at)
	}
	cache.InstallPass(c)
	if at, failures := cache.InstallWait(c, repo); failures != 4 || !at.After(clk.Now()) {
		t.Fatalf("after four failed tries the next is due at %v after %d failures, want a wait ahead", at,
			failures)
	}

	// The store answers, and the maintainer's next publication arrives.
	answer()
	w.log.publish(repo, []code.LayerRef{root, top}, nil)
	w.sync(t, c, repo).Release()
	cache.InstallPass(c)
	if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, []string{root.LayerID, top.LayerID}) {
		t.Fatalf("the pass at the delta's own instant left the copy holding %v, want the new chain %v placed: "+
			"a round that serves the chain is a new try", got, []string{root.LayerID, top.LayerID})
	}
	if at, failures := cache.InstallWait(c, repo); !at.IsZero() || failures != 0 {
		t.Errorf("with the chain placed the repository waits until %v after %d failures, want no wait", at,
			failures)
	}
}

// THE INSTALLER WAKES WHEN ITS EARLIEST WAIT ENDS: the loop Start begins runs
// a pass at every kick, and between kicks sleeps until the earliest wait of a
// repository it holds ends — told to its wake (here the case's, in place of a
// timer) — and retries then, with no kick: a store failing a layer is asked
// again a minute later, and the layer placed once it answers.
//
// Mutation: sleep until a kick alone (runInstaller's wake left nil) — the loop
// asks to be woken at nothing, and the layer is never retried.
func TestTheInstallerWakesWhenItsEarliestWaitEnds(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	p := w.pushN(t, repo, 2)
	clk := newClock()
	var mu sync.Mutex
	var asked []time.Time
	fire := make(chan time.Time)
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.InstallWakesWith(o, func(at time.Time) <-chan time.Time {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, at)
			return fire
		})
	})
	c.Start(t.Context())
	w.sync(t, c, repo).Release()
	chain := w.buildChain(t, p)
	answer := w.refuseParts(chain...)
	w.log.publish(repo, chain, nil)
	w.sync(t, c, repo).Release()

	due := clk.Now().Add(cache.InstallRetryBase)
	eventually(t, "the installer asking to be woken once its first try's wait ends", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(asked, due.Equal)
	})
	answer()
	clk.Set(due)
	select {
	case fire <- due:
	case <-time.After(hangDetector):
		t.Fatalf("the installer's loop took no wake within %v", hangDetector)
	}
	eventually(t, "the layer placed by the retry the wake made", func() bool {
		return slices.Equal(layerIDs(cache.Installed(c, repo)), layerIDs(chain))
	})
}

// THE INSTALLER HOLDS ONE SLOT OF THE READ LANE FOR ITS CHECKS AND ONE FOR ITS
// CANARIES: a layer's check hashes its files a piece at a time in a slot of
// the read lane, and a placement's canary is a git read there — the lane a
// core a slot for the work requests wait on. The installer runs sixteen tasks
// to keep sixteen GETs in flight, not to hash on sixteen cores: here sixteen
// repositories' layers land together, and every piece of every check takes
// the installer's one share for checks in turn, as the scrub, the cache's
// other work behind the serving, holds one slot; and every canary takes the
// one share for canaries (TestAnInstallersCanaryWaitsBehindNoCheck holds why
// it is a share of its own).
//
// Mutations, each seen red: the shares as wide as the tasks
// (installReadSlots 16) — each kind holds many at once; the canary outside a
// share, or told as a check's — no canary takes the canaries' share.
func TestTheInstallerHoldsOneReadSlotForItsChecksAndOneForItsCanaries(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	first := w.repo(t, 1)
	p := w.pushN(t, first, 2)
	chain := w.buildChain(t, p[:1], p[1:])
	type share struct{ now, most, holds int }
	var mu sync.Mutex
	shares := map[bool]*share{false: {}, true: {}}
	c := w.open(t, cache.InstallReadingWith(func(canary, held bool) {
		mu.Lock()
		s := shares[canary]
		if !held {
			s.now--
			mu.Unlock()
			return
		}
		s.now++
		s.holds++
		s.most = max(s.most, s.now)
		mu.Unlock()
		// Held a while, so any other task's work of the kind the share let
		// through meanwhile would be seen beside it.
		time.Sleep(20 * time.Millisecond)
	}))
	const repos = cache.FetchConcurrency
	ids := []string{first}
	for n := 2; n <= repos; n++ {
		id := w.repo(t, n)
		var rows []code.PackRow
		for _, r := range rowsOf(p) {
			rows = append(rows, w.storeBundle(t, w.bundle(r.GitChecksum), ""))
		}
		w.log.write(id, rows, map[string]string{"refs/heads/main": p[len(p)-1].oid})
		ids = append(ids, id)
	}
	for _, id := range ids {
		w.sync(t, c, id).Release()
		w.log.publish(id, chain, nil)
		w.sync(t, c, id).Release()
	}
	if took := cache.InstallPass(c); len(took) != repos {
		t.Fatalf("the pass took up %d repositories, want %d at once", len(took), repos)
	}
	for _, id := range ids {
		if got := layerIDs(cache.Installed(c, id)); !slices.Equal(got, layerIDs(chain)) {
			t.Fatalf("%s holds %v, want %v", id, got, layerIDs(chain))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// Two layers a repository, each its three files' check, and one
	// placement's canary.
	for _, k := range []struct {
		canary bool
		what   string
		least  int
	}{{false, "check pieces", repos * 2 * 3}, {true, "canaries", repos}} {
		s := shares[k.canary]
		if s.holds < k.least {
			t.Errorf("the installer's %s took their share of the read lane %d times, want every one: at least %d",
				k.what, s.holds, k.least)
		}
		if s.most != 1 {
			t.Errorf("the installer's %s held %d slots of the read lane at once, want one", k.what, s.most)
		}
	}
}

// AN INSTALLER'S CANARY WAITS BEHIND NO CHECK: a placement's canary runs
// inside its swap, under the repository's write lock, which every round of the
// repository waits for — so it takes the installer's share of the read lane
// for canaries, never the share its checks' pieces take in turn, where it
// waited behind up to fifteen other tasks' pieces with that lock held. Here
// one repository's check holds the checks' share, a piece of it held there,
// while another repository's placement — of a layer its maintainer adopted,
// which the installer checks no piece of — reads its chain through the bitmap
// and is made.
//
// Mutation: take a canary's share from the checks' (takeInstallRead's
// semaphore for a canary in.checks) — the adopted layer's placement waits for
// the held piece, and is not made while it is held.
func TestAnInstallersCanaryWaitsBehindNoCheck(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	checked, adopted := w.repo(t, 1), w.repo(t, 2)
	pc, pa := w.pushN(t, checked, 2), w.pushN(t, adopted, 2)
	// The first piece the installer's check takes its share for is held
	// there, the share with it, until the case lets it go.
	piece, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := w.open(t, cache.InstallReadingWith(func(canary, held bool) {
		if !canary && held {
			once.Do(func() {
				close(piece)
				<-release
			})
		}
	}))
	let := sync.OnceFunc(func() { close(release) })
	t.Cleanup(let)
	w.sync(t, c, checked).Release()
	w.sync(t, c, adopted).Release()
	chain := w.buildChain(t, pc)
	w.log.publish(checked, chain, nil)
	w.sync(t, c, checked).Release()
	first := make(chan []string, 1)
	go func() { first <- cache.InstallPass(c) }()
	select {
	case <-piece:
	case <-time.After(hangDetector):
		t.Fatalf("the installer's check of %s took no piece within %v", checked, hangDetector)
	}

	// The checks' share held: the other repository's maintainer adopts its
	// layer, which the next pass places.
	s := w.sync(t, c, adopted)
	b := w.newChainBuild(t, pa)
	layer := b.layer(t, pa, false)
	sc, err := c.NewScratch(t.Context(), s, "0198a4f4-0000-7000-8000-000000000003")
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Remove()
	b.into(t, sc.Dir)
	if err := c.AdoptLayer(t.Context(), adopted, sc, layer); err != nil {
		t.Fatalf("AdoptLayer: %v", err)
	}
	s.Release()
	w.log.publish(adopted, []code.LayerRef{layer}, nil)
	w.sync(t, c, adopted).Release()
	second := make(chan []string, 1)
	go func() { second <- cache.InstallPass(c) }()
	eventually(t, "the adopted layer placed while the check's piece holds the checks' share", func() bool {
		return slices.Equal(layerIDs(cache.Installed(c, adopted)), []string{layer.LayerID})
	})

	let()
	<-first
	<-second
	if got := layerIDs(cache.Installed(c, checked)); !slices.Equal(got, layerIDs(chain)) {
		t.Errorf("once its piece was let go, %s holds %v, want %v", checked, got, layerIDs(chain))
	}
}

// holdFirst holds the first read of key in the world's store until the answer
// is called, and refuses the first read of it that is not held — one a case
// makes while the held one waits — with an error that may pass; every other
// read of it is answered.
func (w *syncWorld) holdFirst(key objstore.Key) func() {
	held, refused := make(chan struct{}), errors.New("SlowDown: please reduce your request rate")
	var holds, fails atomic.Int32
	w.objects.mu.Lock()
	w.objects.hold = func(k objstore.Key) <-chan struct{} {
		if k == key && holds.Add(1) == 1 {
			return held
		}
		return nil
	}
	w.objects.fail = func(k objstore.Key) error {
		// Asked once a read is past its hold: the held read's ask comes after
		// that of any read made while it was held.
		if k == key && fails.Add(1) == 1 && holds.Load() > 1 {
			return refused
		}
		return nil
	}
	w.objects.mu.Unlock()
	return sync.OnceFunc(func() { close(held) })
}

// THE INSTALLER DOWNLOADS A LAYER ONCE, WHATEVER MOVES UNDER IT: a placement
// that finds the copy or its chain moved since its plan places nothing, and
// the next plan installs the same layers over the same base — so the layers
// it fetched are held for that plan, which claims each it still wants rather
// than downloading it again (a root layer of the largest repository the
// defaults admit is some 2.6 GB), and a round that serves the very chain the
// plan was made over is no move at all.
//
//   - "a swap": the tail's packs merged and leaving by a swap while the
//     installer downloads the layer — the next pass places it with no second
//     GET, and nothing of it is left in tmp/;
//   - "a publication past it": a publication replacing the layer while the
//     installer downloads it — the moved placement stages it, and the next
//     plan, which no longer wants it, takes it out of tmp/ at once rather
//     than the janitor twelve hours on;
//   - "a full round": a retirement sweep's full round while the installer
//     downloads, serving the same chain, whose own fetch of the layer the
//     store refuses — the installer's placement is made in the same pass.
//
// Mutations, each seen red: drop what a moved placement fetched
// (installRepo's holdFetched) — "a swap" downloads the layer twice; drop no
// staged fetch a plan does not want (installRepo's dropFetched) — "a
// publication past it" leaves it in tmp/; count every chain a round reads as
// a move (setChain's chainGen without its sameRows) — "a full round" places
// nothing in the pass.
func TestTheInstallerDownloadsALayerOnceWhateverMovesUnderIt(t *testing.T) {
	t.Parallel()
	// begin publishes a one-layer chain over a copy that holds its packs, has
	// the store hold the installer's read of its part, and runs a pass that
	// waits for it: the world, the cache, the repository, its pushes, the
	// chain and its part, the call that lets the read go and the pass's end.
	type begun struct {
		w     *syncWorld
		c     *cache.Cache
		repo  string
		p     []pushed
		chain []code.LayerRef
		part  objstore.Key
		land  func()
		pass  chan []string
	}
	begin := func(t *testing.T, pushes int) begun {
		t.Helper()
		b := begun{w: newSyncWorld(t, onMemory), pass: make(chan []string, 1)}
		w := b.w
		b.repo = w.repo(t, 1)
		b.p = w.pushN(t, b.repo, pushes)
		b.c = w.open(t, newClock().on)
		w.sync(t, b.c, b.repo).Release()
		b.chain = w.buildChain(t, b.p[:2])
		b.part = b.chain[0].Parts[0].Object
		b.land = w.holdFirst(b.part)
		w.log.publish(b.repo, b.chain, nil)
		w.sync(t, b.c, b.repo).Release()
		go func() { b.pass <- cache.InstallPass(b.c) }()
		eventually(t, "the installer's read of the layer's part", func() bool {
			return w.gets([]string{b.part.String()}) == 1
		})
		return b
	}

	t.Run("a swap", func(t *testing.T) {
		t.Parallel()
		b := begin(t, 4)
		w, c, repo := b.w, b.c, b.repo
		held := w.dir(t, c, repo)
		w.compacted(t, repo, b.p[2].row, b.p[3].row)
		w.sync(t, c, repo).Release()
		if w.dir(t, c, repo) == held {
			t.Fatal("the compaction's round made no swap: the case needs the copy moved under the install")
		}
		b.land()
		<-b.pass
		cache.InstallPass(c)
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(b.chain)) {
			t.Fatalf("after the pass that planned again the copy holds %v, want %v", got, layerIDs(b.chain))
		}
		if got := w.gets([]string{b.part.String()}); got != 1 {
			t.Errorf("the layer's part was read %d times, want once: the plan after the move takes what "+
				"the moved placement fetched", got)
		}
		if got := cache.StagedLayers(c); len(got) != 0 {
			t.Errorf("tmp/ still holds the staged layers %v", got)
		}
	})

	t.Run("a publication past it", func(t *testing.T) {
		t.Parallel()
		b := begin(t, 3)
		w, c, repo := b.w, b.c, b.repo
		next := w.buildChain(t, b.p)
		w.log.publish(repo, next, nil)
		w.sync(t, c, repo).Release()
		b.land()
		<-b.pass
		if got := cache.StagedLayers(c)[repo]; !slices.Equal(got, layerIDs(b.chain)) {
			t.Fatalf("the moved placement staged %v, want the layer it fetched, %v", got, layerIDs(b.chain))
		}
		cache.InstallPass(c)
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(next)) {
			t.Fatalf("after the pass that planned again the copy holds %v, want the new chain %v", got,
				layerIDs(next))
		}
		if got := cache.StagedLayers(c); len(got) != 0 {
			t.Errorf("tmp/ still holds %v, a layer the rows moved past: the plan that no longer wants it "+
				"drops it", got)
		}
	})

	t.Run("a full round", func(t *testing.T) {
		t.Parallel()
		b := begin(t, 2)
		w, c, repo := b.w, b.c, b.repo
		w.log.write(repo, nil, nil)
		w.log.sweepRetired(repo, w.log.head())
		if s := w.sync(t, c, repo); s.Kind != cache.SyncFull {
			t.Fatalf("the round below the sweep was %s, want a full one", s.Kind)
		}
		b.land()
		<-b.pass
		if got := layerIDs(cache.Installed(c, repo)); !slices.Equal(got, layerIDs(b.chain)) {
			t.Fatalf("the pass the full round ran beside left the copy holding %v, want %v placed: a round "+
				"that serves the chain the plan was made over moves nothing", got, layerIDs(b.chain))
		}
		cache.InstallPass(c)
		if got := w.gets([]string{b.part.String()}); got != 2 {
			t.Errorf("the layer's part was read %d times, want twice: the installer's, and the full "+
				"round's the store refused", got)
		}
	})
}
