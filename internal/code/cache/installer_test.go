package cache_test

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
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
