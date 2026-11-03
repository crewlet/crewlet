package cache_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
)

// A RETIRED INCARNATION OUTLIVES ITS LAST READER, and not a moment longer: a
// reader's git process has that directory as its GIT_DIR, so removing or
// renaming it under the process would have it open another directory's files
// half way through its run — and a directory nobody holds any more is disk the
// cache gives back at once. Release is idempotent, so a reader that releases
// twice never steals another reader's pin.
func TestARetiredIncarnationLeavesWithItsLastReader(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	dir := w.path("repos", repoA+".2.git")
	writeState(t, dir, aState(repoA))
	other := w.path("repos", repoB+".1.git")
	writeState(t, other, aState(repoB))
	c := w.open(t)
	cache.JanitorPass(c) // the boot's own trash

	first, _ := cache.Pin(c, repoA)
	second, ok := cache.Pin(c, repoA)
	if !ok || first.Kind != cache.SyncNone || !first.Kind.Valid() {
		t.Fatalf("pinning a held repository = %+v, %v", first, ok)
	}
	if got := cache.Pins(c, repoA); got != 2 {
		t.Fatalf("pins = %d after two readers took it, want 2", got)
	}
	first.Release()
	first.Release()
	if got := cache.Pins(c, repoA); got != 1 {
		t.Fatalf("pins = %d after one reader released twice, want 1", got)
	}

	if !cache.Retire(c, repoA) {
		t.Fatal("nothing to retire")
	}
	cache.JanitorPass(c)
	if !exists(dir) {
		t.Fatal("a retired incarnation a reader still holds left repos/")
	}

	second.Release()
	if exists(dir) {
		t.Fatal("a retired incarnation stayed in repos/ after its last reader let go")
	}
	if got := names(t, w.path("trash")); len(got) != 1 {
		t.Fatalf("trash/ holds %v, want the released incarnation", got)
	}
	cache.JanitorPass(c)
	if got := names(t, w.path("trash")); len(got) != 0 {
		t.Errorf("trash/ after a janitor pass holds %v", got)
	}

	// Nobody holds the other repository: retired, it goes at once.
	if !cache.Retire(c, repoB) {
		t.Fatal("nothing to retire")
	}
	if exists(other) {
		t.Error("an unpinned incarnation stayed in repos/ after it was retired")
	}
}

// A READER THAT OUTLIVES STOP MOVES NOTHING when it lets go: Stop releases
// the directory's claim, so the directory may already be another process's —
// the next Open's scan, which needs nothing from a stopped cache, decides
// what becomes of it.
func TestAReleaseAfterStopMovesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	dir := w.path("repos", repoA+".1.git")
	writeState(t, dir, aState(repoA))
	c := w.open(t)

	held, ok := cache.Pin(c, repoA)
	if !ok {
		t.Fatal("nothing to pin")
	}
	cache.Retire(c, repoA)
	c.Stop(context.Background())
	held.Release()
	if !exists(dir) {
		t.Fatal("a release after Stop moved a directory the next Open owns")
	}

	again := w.open(t)
	if got := cache.Repos(again); got[repoA] != 1 {
		t.Errorf("the next Open holds %v, want the directory a stopped cache left", got)
	}
}

// SYNCKIND IS A CLOSED SET: the host labels a histogram with it, and a value
// off the set is a label nobody's dashboard reads.
func TestSyncKindIsAClosedSet(t *testing.T) {
	t.Parallel()
	for _, k := range cache.SyncKinds {
		if !k.Valid() {
			t.Errorf("%q is listed and not valid", k)
		}
	}
	for _, k := range []cache.SyncKind{"", "partial", "Full"} {
		if k.Valid() {
			t.Errorf("%q is valid", k)
		}
	}
}
