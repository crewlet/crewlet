package cache_test

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
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

// A FULL ROUND SWEEPS A DEAD WRITER'S FILES BEFORE ITS INDEX: a maintenance
// run killed with its node leaves the index's lock and a bitmap's temporary
// file in objects/pack (git removes them only when it is asked to stop), and
// no reader of any ref reads either. Left there until the refs were in, the
// lock refused the round's own index — git writes no index over another
// writer's lock — and the index's failure path then removed it from under
// the sweep that had listed it, so the sweep's move failed and the round with
// it: the first sync of the repository after the crash answered an error. A
// file of objects/pack that is no pack's goes with the round's first half;
// only the pack files wait for the refs.
func TestAFullRoundSweepsADeadWritersFilesBeforeItsIndex(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range caseRoundPacks - 1 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	round := cache.RoundPacksAt(caseRoundPacks)
	c := w.open(t, round)
	dir := w.sync(t, c, repo).Dir
	w.push(t, repo, "refs/heads/main", nil)
	packDir := filepath.Join(dir, "objects", "pack")
	if statOK(t, filepath.Join(packDir, "multi-pack-index")) {
		t.Fatal("a round under its threshold wrote an index: the case needs one with none")
	}

	c.Stop(t.Context())
	dead := []string{
		filepath.Join(packDir, "multi-pack-index.lock"),
		filepath.Join(packDir, "tmp_bitmap_Xq3vRk"),
		filepath.Join(dir, "objects", "info", "commit-graph.lock"),
	}
	for _, path := range dead {
		write(t, path, "half written\n")
	}
	c = w.open(t, round)
	s, err := c.Sync(t.Context(), repo, linearizable)
	if err != nil {
		t.Fatalf("the first sync after a crash that left a maintenance run's files: %v", err)
	}
	defer s.Release()
	for _, path := range dead {
		if statOK(t, path) {
			t.Errorf("the full round left %s, a dead writer's", path)
		}
	}
	if !statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
		t.Errorf("the full round of %d packs answered with no index: a dead writer's lock refused it", caseRoundPacks)
	}
	if got := w.logs.lines(t, "code_cache_round_index_skipped"); len(got) != 0 {
		t.Errorf("the round's index was skipped: %v", got)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image after the crash:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}
