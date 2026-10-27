package cache_test

import (
	"errors"
	"testing"
	"time"

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
