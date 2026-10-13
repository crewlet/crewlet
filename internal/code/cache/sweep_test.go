package cache_test

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// A DELTA SWEEPS NOTHING: the steady-state cost of a sync is proportional to
// what moved — between two rounds of one process nothing but this package
// writes the directory, so a delta neither lists nor sweeps it, in either of
// the sweep's halves. A stray pack (the packs' half), and a stray file at the
// incarnation's root and a loose ref (the first half), planted after the first
// round therefore survive a delta, and go with the full first round of the
// next process.
func TestADeltaSweepsNothing(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	dir := w.sync(t, c, repo).Dir
	other := w.client.Init(t)
	strayTip := w.client.Commit(t, other, map[string]string{"x": "stray\n"}, "stray")
	stray := gittest.Pack(t, w.client, dir, other, strayTip)
	strays := []string{
		filepath.Join(dir, "objects", "pack", "pack-"+stray+".idx"),
		filepath.Join(dir, "description"),
		filepath.Join(dir, "refs", "heads", "loose"),
	}
	write(t, strays[1], "a stray at the root\n")
	write(t, strays[2], tip+"\n")

	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	for _, path := range strays {
		if !statOK(t, path) {
			t.Errorf("a delta round swept %s", path)
		}
	}

	c = w.reopen(t, c)
	w.sync(t, c, repo)
	for _, path := range strays {
		if statOK(t, path) {
			t.Errorf("the first round of a new process left %s, outside the image", path)
		}
	}
}
