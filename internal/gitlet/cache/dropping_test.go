package cache_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE IMAGE HOLDS EXACTLY THE LIVE AND THE DROPPING PACKS, each where its rows
// place it (package doc, "The dropping view"), as a fresh cache builds,
// through a swap and a restart. Only a process naming the view reads a
// dropping object; a merge's store is refused the dropping pack (ErrInvalid).
// The drop epoch is the begin's once its inputs are gone; the budget counts
// the view (DroppingBytes). A pack's files at the other place are strays
// Verify names and a full round removes in place; the commit removes the view.
//
// Mutations: placeOfRows placing every pack live, and the begin's sync holds
// it among the live; packStrays keeping a pack at the other place, and repair
// rebuilds and the boot keeps them; checkPacks ignoring the place, and no
// plant is named; mergedLive taking dropping rows, and the merge is not refused.
func TestTheImageHoldsExactlyLiveAndDroppingPacks(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	base, first := w.push(t, repo, "refs/heads/main", nil)
	gone, second := w.push(t, repo, "refs/heads/gone", nil)
	w.client.Run(t, w.repos[repo].work, "reset", "--hard", base)
	_, third := w.push(t, repo, "refs/heads/main", nil)
	w.log.write(repo, nil, map[string]string{"refs/heads/gone": ""})
	c := w.open(t)
	held := w.sync(t, c, repo)

	kept, dropping := w.expungeBegin(t, repo, []string{gone}, first, second, third)
	s := w.sync(t, c, repo)
	if s.Dir == held.Dir {
		t.Fatalf("the begin's sync answered the copy a reader holds, %s: its inputs leave by a swap", s.Dir)
	}
	holds := func(when string, s cache.Synced) {
		t.Helper()
		if got, want := packFiles(t, s.Dir), rowFiles(kept); !slices.Equal(got, want) {
			t.Errorf("%s, objects/pack holds %v, want the kept pack's %v", when, got, want)
		}
		if got, want := droppingFiles(t, s.Dir), rowFiles(dropping); !slices.Equal(got, want) {
			t.Errorf("%s, the dropping view holds %v, want the dropping pack's %v", when, got, want)
		}
		if got, want := imageOf(t, s.Dir), w.rebuilt(t, repo); got != want {
			t.Errorf("%s, the image:\n%s\nwant a fresh cache's:\n%s", when, got, want)
		}
		if view := filepath.Join(s.Dir, gitproc.DroppingDir); s.Dropping != view {
			t.Errorf("%s, the copy is handed out with the dropping view %q, want %s", when, s.Dropping, view)
		}
		if s.DropEpoch != 1 {
			t.Errorf("%s, the copy is handed out at drop epoch %d, want the begin's 1", when, s.DropEpoch)
		}
		if doc := readStateDoc(t, s.Dir); doc.DroppingBytes != dropping.Size {
			t.Errorf("%s, the state records %d bytes in the dropping view, want the pack's %d", when,
				doc.DroppingBytes, dropping.Size)
		}
		if got := gauge(t, w.rec, metrics.CodeCacheDroppingBytes); got != float64(dropping.Size) {
			t.Errorf("%s, %s = %v, want the dropping pack's %d bytes", when, metrics.CodeCacheDroppingBytes, got,
				dropping.Size)
		}
	}
	holds("after the begin", s)
	if held.Dropping != "" || held.DropEpoch != 0 {
		t.Errorf("the copy before the begin was handed out with the view %q at epoch %d, want none at 0",
			held.Dropping, held.DropEpoch)
	}

	// WHAT READS THE VIEW: a process naming it, and nothing else.
	catFile := func(c gitproc.Cmd) error {
		c.Args, c.Class, c.Deadline = []string{"cat-file", "-e", gone}, gitproc.ClassRead, gitproc.ReadDeadline
		_, err := w.pool.Run(t.Context(), c)
		return err
	}
	if err := catFile(gitproc.Cmd{Dir: s.Dir, Dropping: s.Dropping}); err != nil {
		t.Errorf("a serving process naming the view did not read the dropping commit %s: %v", gone, err)
	}
	if err := catFile(gitproc.Cmd{Dir: s.Dir}); err == nil {
		t.Errorf("a process in the copy naming no view read the dropping commit %s", gone)
	}
	sc, err := c.NewScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatalf("a scratch over the copy: %v", err)
	}
	if out, err := runGitOver(t, w.pool, sc, "cat-file", "-e", gone); err == nil {
		t.Errorf("a push's scratch over the copy read the dropping commit %s: %q", gone, out)
	}
	sc.Remove()
	scratched := names(t, w.path("scratch"))
	if _, err := c.NewMergeScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String(),
		[]string{dropping.GitChecksum}); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("a merge's scratch naming the dropping pack = %v, want ErrInvalid: no merge reads the view", err)
	}
	if got := names(t, w.path("scratch")); !slices.Equal(got, scratched) {
		t.Errorf("scratch/ holds %v after the refused merge, want %v: nothing made", got, scratched)
	}

	// A RESTART: the boot counts the view from the state, and the first
	// round, full, keeps it.
	c = w.reopen(t, c)
	if got := gauge(t, w.rec, metrics.CodeCacheDroppingBytes); got != float64(dropping.Size) {
		t.Errorf("the boot counts %s = %v, want the state's %d", metrics.CodeCacheDroppingBytes, got,
			dropping.Size)
	}
	s = w.sync(t, c, repo)
	holds("after a restart", s)

	// A PACK AT THE OTHER PLACE is a stray: the dropping pack's files in
	// objects/pack, and the kept pack's in the view.
	misplace := func(dir string) {
		t.Helper()
		for _, f := range rowFiles(dropping) {
			copyFile(t, filepath.Join(dir, gitproc.DroppingDir, "pack", f), filepath.Join(dir, "objects", "pack", f))
		}
		for _, f := range rowFiles(kept) {
			copyFile(t, filepath.Join(dir, "objects", "pack", f), filepath.Join(dir, gitproc.DroppingDir, "pack", f))
		}
	}
	misplace(s.Dir)
	found := []string{
		"stray pack pack-" + dropping.GitChecksum + " in objects/pack (only dropping rows name it)",
		"stray pack pack-" + kept.GitChecksum + " in the dropping view (a live row names it)",
	}
	report, err := verify(t.Context(), c, repo, false)
	if err != nil {
		t.Fatalf("Verify of the copy with both packs at the other place too: %v", err)
	}
	problemsOf(t, "Verify found", report.Problems, found...)
	// Its repair is a full round IN PLACE, swapping the strays out since the
	// case holds the copy, with no rebuild.
	repaired, err := verify(t.Context(), c, repo, true)
	if err != nil {
		t.Fatalf("Verify with repair: %v", err)
	}
	if len(repaired.Problems) != 0 || !repaired.RepairedInPlace || repaired.Rebuilt {
		t.Errorf("Verify with repair = %+v; want the copy read whole in place, leaving no problem, with no "+
			"rebuild", repaired)
	}
	problemsOf(t, "Verify's repair found first", repaired.Found, found...)
	// The planted copy, retired by the swap, goes with its last reader.
	s.Release()
	s = w.sync(t, c, repo)
	holds("after the repair", s)

	// And a full round of a copy nobody holds — a boot's first — takes them
	// out in place.
	misplace(s.Dir)
	c = w.reopen(t, c)
	s = w.sync(t, c, repo)
	holds("after a boot over a copy holding both packs at the other place too", s)

	// THE COMMIT retires the dropping pack and the view with it, by a swap
	// whose old copy goes with its last reader.
	w.log.expungeCommit(repo)
	before := s
	s = w.sync(t, c, repo)
	before.Release()
	if got, want := packFiles(t, s.Dir), rowFiles(kept); !slices.Equal(got, want) {
		t.Errorf("after the commit, objects/pack holds %v, want the kept pack's %v", got, want)
	}
	if statOK(t, filepath.Join(s.Dir, gitproc.DroppingDir)) {
		t.Errorf("after the commit the copy still holds a dropping view: %v", droppingFiles(t, s.Dir))
	}
	if got, want := imageOf(t, s.Dir), w.rebuilt(t, repo); got != want {
		t.Errorf("after the commit, the image:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
	if s.Dropping != "" || s.DropEpoch != 1 {
		t.Errorf("after the commit the copy is handed out with the view %q at epoch %d, want none at 1",
			s.Dropping, s.DropEpoch)
	}
	if got := gauge(t, w.rec, metrics.CodeCacheDroppingBytes); got != 0 {
		t.Errorf("after the commit, %s = %v, want 0", metrics.CodeCacheDroppingBytes, got)
	}
}

// ONLY AN EXPUNGE DROPS THE COMMIT-GRAPH (X04): a compaction's retirement (a
// merge drops no commit) and an expunge's begin (its packs hold every input
// object) keep it; the commit drops it, since git walks a graph naming a
// purged commit as though it were there, and the memo, which may quote what
// was purged. So does whichever round takes the dropping pack out (a delta's
// swap, a boot's full round), even one crossing the begin too: the begin's
// inputs, holding the purged commits, then leave as the commit's.
//
// Mutations: OnPackExpunged Keep for the graph, and `git log` answers the
// dropped commit; keep the memo, and its answer outlives the purge; take a
// full round's dropping stray, or a crossed begin's inputs, for a retirement,
// and the boot or the lagged copy keeps both.
func TestOnlyAnExpungeDropsTheCommitGraph(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name string
		// down: the node stops before the commit lands, so the round that
		// takes the dropping pack out is its next boot's first.
		down bool
		// lagged: the copy is not synced between the begin and the commit,
		// so one round crosses both and never places the dropping pack.
		lagged bool
	}{{"a delta's swap", false, false}, {"a boot's full round", true, false},
		{"a delta's swap crossing the begin too", false, true},
		{"a boot's full round crossing the begin too", true, true}} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			repo := w.repo(t, 1)
			base, first := w.push(t, repo, "refs/heads/main", nil)
			gone, second := w.push(t, repo, "refs/heads/gone", nil)
			w.client.Run(t, w.repos[repo].work, "reset", "--hard", base)
			_, third := w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t, clk.on)
			w.sync(t, c, repo)
			// THE GRAPH NAMES THE COMMIT THE EXPUNGE DROPS: written while a
			// ref still reached it.
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			w.log.write(repo, nil, map[string]string{"refs/heads/gone": ""})
			graphed := func(s cache.Synced) bool {
				return statOK(t, filepath.Join(s.Dir, "objects", "info", "commit-graph"))
			}

			merged := w.compacted(t, repo, first, third)
			s := w.sync(t, c, repo)
			if !graphed(s) {
				t.Fatal("a compaction's swap dropped the commit-graph: a merge drops no commit (X04)")
			}
			if !arm.lagged {
				w.expungeBegin(t, repo, []string{gone}, merged, second)
				s = w.sync(t, c, repo)
				if !graphed(s) {
					t.Fatal("an expunge's begin dropped the commit-graph: every commit of its inputs is still in the copy")
				}
			}
			answer := memoKeys("expunged", 1)
			memoize(t, c, s, answer)
			if cache.MemoEntryFile(s, answer[0]) == "" {
				t.Fatal("the memo placed no answer: the case needs one to see it go")
			}

			if arm.down {
				c.Stop(t.Context())
			}
			if arm.lagged {
				w.expungeBegin(t, repo, []string{gone}, merged, second)
			}
			w.log.expungeCommit(repo)
			if arm.down {
				c = w.open(t, clk.on)
			}
			s = w.sync(t, c, repo)
			if graphed(s) {
				t.Error("the copy holds a commit-graph after the expunge's commit took a commit out of the repository")
			}
			if path := cache.MemoEntryFile(s, answer[0]); path != "" {
				t.Errorf("the memo still answers from %s after the expunge's commit", path)
			}
			res, err := w.pool.Run(t.Context(), gitproc.Cmd{Dir: s.Dir, Dropping: s.Dropping,
				Args:  []string{"log", "--format=%H", "-1", gone},
				Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
			if err == nil {
				t.Errorf("a fresh git log over the expunged commit %s answered %q: a graph still names it", gone,
					strings.TrimSpace(string(res.Stdout)))
			}
		})
	}
}

// expungeBegin writes an expunge's begin on repo over inputs, its live packs
// (fakeLog.expungeBegin), with a maintainer's rewrite: KEPT, every object the
// refs reach, and DROPPING, what dropped reaches that no ref does plus a blob
// no commit names, as an expunge's cruft holds, so its bytes match no input.
// Both are stored, and their rows returned as the record added them.
func (w *syncWorld) expungeBegin(t testing.TB, repo string, dropped []string, inputs ...gitlet.PackRow) (kept,
	dropping gitlet.PackRow) {
	t.Helper()
	f := w.repos[repo]
	tips := w.log.tips(repo)
	bare := gittest.Empty(t)
	keptBundle := gittest.Bundle(t, bare, gittest.Pack(t, w.client, bare, f.work, tips...))

	walk := slices.Concat([]string{"rev-list", "--objects"}, dropped, []string{"--not"}, tips)
	var objects []string
	for line := range strings.Lines(w.client.Run(t, f.work, walk...)) {
		if oid, _, _ := strings.Cut(strings.TrimSpace(line), " "); oid != "" {
			objects = append(objects, oid)
		}
	}
	blob, stderr, err := w.client.Try(t, f.work, strings.NewReader("named by no commit of "+repo+"\n"),
		"hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatalf("writing the blob no commit names: %v\n%s", err, stderr)
	}
	objects = append(objects, strings.TrimSpace(blob))
	bare = gittest.Empty(t)
	droppingBundle := gittest.Bundle(t, bare, gittest.PackObjects(t, w.client, bare, f.work, objects...))
	w.mu.Lock()
	w.bundles[keptBundle.Checksum], w.bundles[droppingBundle.Checksum] = keptBundle, droppingBundle
	w.mu.Unlock()

	kept, dropping = w.storeBundle(t, keptBundle, ""), w.storeBundle(t, droppingBundle, "")
	ids := make([]string, 0, len(inputs))
	for _, r := range inputs {
		ids = append(ids, r.PackID)
	}
	at := w.log.expungeBegin(repo, []gitlet.PackRow{kept}, []gitlet.PackRow{dropping}, ids...)
	kept.Added, kept.Kind, kept.State = at, gitlet.PackExpunge, gitlet.PackLive
	dropping.Added, dropping.Kind, dropping.State = at, gitlet.PackCruft, gitlet.PackDropping
	return kept, dropping
}

// tips is every object a live ref of repo names, sorted and once each.
func (f *fakeLog) tips(repo string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.repos[repo].refs {
		if r.oid != "" && !slices.Contains(out, r.oid) {
			out = append(out, r.oid)
		}
	}
	slices.Sort(out)
	return out
}

// rowFiles is the three files a pack's row names, as an incarnation holds
// them, sorted.
func rowFiles(p gitlet.PackRow) []string {
	return []string{"pack-" + p.GitChecksum + ".idx", "pack-" + p.GitChecksum + ".pack",
		"pack-" + p.GitChecksum + ".rev"}
}

// droppingFiles is the pack files in an incarnation's dropping view, sorted:
// none where it holds no view.
func droppingFiles(t testing.TB, dir string) []string {
	t.Helper()
	view := filepath.Join(dir, gitproc.DroppingDir, "pack")
	if !statOK(t, view) {
		return nil
	}
	var out []string
	for _, name := range names(t, view) {
		if packFile.MatchString(name) {
			out = append(out, name)
		}
	}
	return out
}

// copyFile copies from to to as a new file, as a hand writing into a cache
// would.
func copyFile(t testing.TB, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, raw, 0o400); err != nil {
		t.Fatal(err)
	}
}

// AN ADOPTION OF A DROPPING PACK'S CHECKSUM MOVES ITS FILES OUT OF THE VIEW:
// a rescue byte for byte the dropping pack it was cut from, adopted by its
// maintainer, places that checksum live; the next round's swap takes the
// view's files out, leaving what a fresh cache builds: the pack in
// objects/pack, and no view.
//
// Mutation: record the adopted row live without noting the view's files as
// leaving (liveAdopted), and they stay in the view after the commit's round.
func TestAnAdoptionOfADroppingPacksChecksumMovesItsFilesOut(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	base, first := w.push(t, repo, "refs/heads/main", nil)
	gone, second := w.push(t, repo, "refs/heads/gone", nil)
	w.client.Run(t, w.repos[repo].work, "reset", "--hard", base)
	_, third := w.push(t, repo, "refs/heads/main", nil)
	w.log.write(repo, nil, map[string]string{"refs/heads/gone": ""})
	c := w.open(t)
	_, dropping := w.expungeBegin(t, repo, []string{gone}, first, second, third)
	s := w.sync(t, c, repo)
	if got := droppingFiles(t, s.Dir); !slices.Equal(got, rowFiles(dropping)) {
		t.Fatalf("the premise: after the begin the view holds %v, want the dropping pack's", got)
	}
	sc, err := c.NewScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Remove()
	for _, name := range rowFiles(dropping) {
		copyFile(t, filepath.Join(s.Dir, gitproc.DroppingDir, "pack", name), filepath.Join(sc.Dir, "objects", "pack", name))
	}
	rescue := dropping
	rescue.PackID = uuid.Must(uuid.NewV7()).String()
	w.log.expungeCommit(repo, rescue)
	if err := c.Adopt(t.Context(), repo, sc, rescue.Ref()); err != nil {
		t.Fatalf("adopting the rescue: %v", err)
	}
	after := w.sync(t, c, repo)
	if got := droppingFiles(t, after.Dir); len(got) != 0 {
		t.Errorf("after the commit's round the view holds %v, which the rescue's live row moved out of it", got)
	}
	if got, want := imageOf(t, after.Dir), w.rebuilt(t, repo); got != want {
		t.Errorf("after the commit's round, the image:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}

// AN ADOPTED DROPPING PACK IS NEVER DOWNLOADED, AND LIES IN THE VIEW: the
// maintainer adopts its build's dropping pack once the begin applied
// (AdoptDropping), and the next round finds it in the dropping view and
// opens nothing of it: the copy is what a fresh cache builds, nothing of the
// pack in objects/pack.
//
// Mutations: place the adopted pack in objects/pack (adoptedPlace answering
// placeLive), and the round moves it into the view with a GET; skip the
// adoption's links, and the round downloads it.
func TestAnAdoptedDroppingPackIsNeverDownloaded(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	base, first := w.push(t, repo, "refs/heads/main", nil)
	gone, second := w.push(t, repo, "refs/heads/gone", nil)
	w.client.Run(t, w.repos[repo].work, "reset", "--hard", base)
	_, third := w.push(t, repo, "refs/heads/main", nil)
	w.log.write(repo, nil, map[string]string{"refs/heads/gone": ""})
	c := w.open(t)
	before := w.sync(t, c, repo)
	_, dropping := w.expungeBegin(t, repo, []string{gone}, first, second, third)
	sc, err := c.NewScratch(t.Context(), before, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Remove()
	b := w.bundle(dropping.GitChecksum)
	at := int64(0)
	for _, section := range []struct {
		ext string
		n   int64
	}{{".pack", b.PackBytes}, {".idx", b.IndexBytes}, {".rev", b.ReverseBytes}} {
		write(t, filepath.Join(sc.Dir, "objects", "pack", "pack-"+dropping.GitChecksum+section.ext),
			string(b.Bytes[at:at+section.n]))
		at += section.n
	}
	if err := c.AdoptDropping(t.Context(), repo, sc, dropping.Ref()); err != nil {
		t.Fatalf("adopting the dropping pack: %v", err)
	}
	after := w.sync(t, c, repo)
	if opens, _, _ := w.objects.counts(); opens[dropping.Object] != 0 {
		t.Errorf("the round after the adoption opened the dropping pack's object %d times, want none",
			opens[dropping.Object])
	}
	if got := droppingFiles(t, after.Dir); !slices.Equal(got, rowFiles(dropping)) {
		t.Errorf("after the begin's round the view holds %v, want the adopted dropping pack's %v", got,
			rowFiles(dropping))
	}
	if got, want := imageOf(t, after.Dir), w.rebuilt(t, repo); got != want {
		t.Errorf("after the begin's round, the image:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}

// A REWRITE'S SCRATCH IS MADE OVER THE COPY SERVING ITS SYNC NOW: when a
// merge swaps the copy an expunge's scan took (emptying its live set) before
// the build's scratch or its bundle rule's rebuild, the scratch is made over
// the serving copy, its store holding every pack live there, the merge's
// output among them, never refused for inputs the old copy no longer names.
//
// Mutation: pick over the Synced's own incarnation in newRewriteScratch, and
// the scratch is refused: its input is no pack live there.
func TestARewritesScratchIsMadeOverTheCopyServingItsSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, first := w.push(t, repo, "refs/heads/main", nil)
	_, second := w.push(t, repo, "refs/heads/main", nil)
	_, third := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	scan := w.sync(t, c, repo)
	merged := w.compacted(t, repo, second, third)
	if after := w.sync(t, c, repo); after.Dir == scan.Dir {
		t.Fatalf("the premise: the merge's round left the copy at %s, want it swapped", after.Dir)
	}
	sc, err := c.StageExpunge(t.Context(), scan, uuid.Must(uuid.NewV7()).String(), []string{first.GitChecksum})
	if err != nil {
		t.Fatalf("an expunge's scratch over a copy a merge's swap replaced: %v", err)
	}
	defer sc.Remove()
	if got, want := packFiles(t, sc.Dir), rowFiles(first); !slices.Equal(got, want) {
		t.Errorf("the scratch's own packs are %v, want the input's %v", got, want)
	}
	if got, want := packFiles(t, sc.Alternate), rowFiles(merged); !slices.Equal(got, want) {
		t.Errorf("the scratch's store holds %v, want every other pack live in the copy serving now, the "+
			"merge's output %v", got, want)
	}
}

// A MERGE'S SCRATCH IS MADE OVER THE COPY SERVING ITS SYNC NOW: when another
// record swaps the copy a compaction's or a publication's plan took (emptying
// its live set) before its scratch is made, the scratch links its inputs from
// the serving copy, never refused for inputs the old copy no longer names.
//
// Mutation: resolve no serving copy for a merge in newScratch, and the
// scratch is refused: its input is no pack live in the copy the sync took.
func TestAMergesScratchIsMadeOverTheCopyServingItsSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, first := w.push(t, repo, "refs/heads/main", nil)
	_, second := w.push(t, repo, "refs/heads/main", nil)
	_, third := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	planned := w.sync(t, c, repo)
	w.compacted(t, repo, second, third)
	if after := w.sync(t, c, repo); after.Dir == planned.Dir {
		t.Fatalf("the premise: the other merge's round left the copy at %s, want it swapped", after.Dir)
	}
	sc, err := c.NewMergeScratch(t.Context(), planned, uuid.Must(uuid.NewV7()).String(), []string{first.GitChecksum})
	if err != nil {
		t.Fatalf("a merge's scratch over a copy another merge's swap replaced: %v", err)
	}
	defer sc.Remove()
	if got, want := packFiles(t, sc.Alternate), rowFiles(first); !slices.Equal(got, want) {
		t.Errorf("the merge's store holds %v, want its input's %v", got, want)
	}
}

// ONLY SERVING READS NAME THE DROPPING VIEW (M6-D3): every git process under
// internal/gitlet naming it (gitproc.Cmd.Dropping) is a gitReaders read that
// serves the copy, a droppingViewers row saying why, never beside an
// alternate; every row is found, and every process of a row's file and
// subcommand names it, so no serving read misses a dropping object. A
// process that writes objects leans on objects/ alone, so none relies on an
// object the commit takes out.
//
// Mutations: name the view in host/receive.go's receive-pack, and it is a
// writer beside an alternate; drop it from host/fetch.go's upload-pack, and
// the row finds nothing naming it.
func TestOnlyServingReadsNameTheDroppingView(t *testing.T) {
	t.Parallel()
	walk, err := codeWalk()
	if err != nil {
		t.Fatalf("walking internal/gitlet: %v", err)
	}
	rows := map[[2]string]bool{}
	for _, r := range droppingViewers {
		if _, ok := gitReaders[r.word]; !ok {
			t.Errorf("droppingViewers names %s's git %s, which is no reader of gitReaders: a dropping view is "+
				"named only where git reads what the copy serves", r.file, r.word)
		}
		rows[[2]string{r.file, r.word}] = true
	}
	named := map[[2]string]bool{}
	for _, l := range walk.literals {
		key := [2]string{l.file, l.word}
		switch {
		case l.dropping && l.alternate:
			t.Errorf("%s names a dropping view beside an alternate: a process leaning on one writes into a "+
				"repository of its own, and nothing it writes may rely on an object an expunge is dropping", l.pos)
		case l.dropping && !rows[key]:
			t.Errorf("%s names a dropping view for git %q, which no row of droppingViewers names: only a "+
				"process that serves a copy reads its dropping packs — a new one is a row, with why it serves",
				l.pos, l.word)
		case l.dropping:
			named[key] = true
		case rows[key]:
			t.Errorf("%s runs git %s, which droppingViewers names as a read serving the copy, and names no "+
				"dropping view: it misses every object an expunge is dropping that the copy still serves",
				l.pos, l.word)
		}
	}
	for _, r := range droppingViewers {
		if !named[[2]string{r.file, r.word}] {
			t.Errorf("droppingViewers names %s's git %s, and the walk finds no process of it naming the view. "+
				"Delete the row — it was: %s", r.file, r.word, r.why)
		}
	}
}

// queryServes is why every read internal/gitlet/query makes over a copy names
// its dropping view.
const queryServes = "a seat's read of the copy by object id, answered from the objects the copy serves"

// droppingViewers is the closed set of git processes under internal/gitlet
// naming a copy's dropping view, and why each serves the copy. Exact in both
// directions (TestOnlyServingReadsNameTheDroppingView).
var droppingViewers = []struct{ file, word, why string }{
	{"internal/gitlet/host/fetch.go", "upload-pack", "a clone's or a fetch's pack: what the copy serves, an " +
		"object an expunge is dropping among it, by its id"},
	{"internal/gitlet/host/advertise.go", "upload-pack", "the advertisement of the fetch after it, run as that " +
		"fetch runs"},
	{"internal/gitlet/host/download.go", "cat-file", "a download's lookup and its file's bytes, as the copy " +
		"serves them"},
	{"internal/gitlet/host/download.go", "ls-tree", "an archive's measure of the commit it serves"},
	{"internal/gitlet/host/download.go", "archive", "an archive of a commit the copy serves"},
	{"internal/gitlet/cache/missing.go", "cat-file", "which of a fetch's wants the copy lacks: a dropping object " +
		"is one the fetch's upload-pack serves, never one a sync could bring"},
	{"internal/gitlet/cache/refs.go", "cat-file", "a round's peel of the tags it sets, through the view the " +
		"directory holds (heldView): a ref may reach a dropping tag between an expunge's begin and its commit"},
	{"internal/gitlet/cache/tags.go", "cat-file", "a round's tag step: the types and dates of the tags it " +
		"levels, through the view the directory holds (heldView), as the peel reads them"},
	{"internal/gitlet/cache/tags.go", "log", "a round's tag step: the committer dates of its lightweight tags' " +
		"commits, through the view the directory holds (heldView)"},
	{"internal/gitlet/cache/layers.go", "rev-list", "a placement's canary, a bitmapped walk of what the refs " +
		"reach, through the view the directory holds (heldView)"},
	{"internal/gitlet/cache/verify.go", "fsck", "Verify's connectivity check of what the copy serves, through " +
		"the view the directory holds (heldView)"},
	{"internal/gitlet/query/anchor.go", "diff-pairs", queryServes},
	{"internal/gitlet/query/blame.go", "blame", queryServes},
	{"internal/gitlet/query/blame.go", "cat-file", queryServes},
	{"internal/gitlet/query/blob.go", "cat-file", queryServes},
	{"internal/gitlet/query/checksfile.go", "cat-file", queryServes},
	{"internal/gitlet/query/codefiles.go", "cat-file", "the atlas's read of a listed deploy or dependency " +
		"file of a commit the copy serves"},
	{"internal/gitlet/query/codefiles.go", "ls-tree", "the atlas's listing of a commit's deploy and " +
		"dependency files, of the tree the copy serves"},
	{"internal/gitlet/query/compare.go", "merge-base", queryServes},
	{"internal/gitlet/query/compare.go", "rev-list", queryServes},
	{"internal/gitlet/query/diff.go", "diff-tree", queryServes},
	{"internal/gitlet/query/diff.go", "diff-pairs", queryServes},
	{"internal/gitlet/query/excerpts.go", "diff-tree", queryServes},
	{"internal/gitlet/query/excerpts.go", "diff-pairs", queryServes},
	{"internal/gitlet/query/facts.go", "diff-tree", queryServes},
	{"internal/gitlet/query/facts.go", "patch-id", queryServes},
	{"internal/gitlet/query/facts.go", "cat-file", queryServes},
	{"internal/gitlet/query/interdiff.go", "cat-file", queryServes},
	{"internal/gitlet/query/landed.go", "merge-base", queryServes},
	{"internal/gitlet/query/log.go", "log", queryServes},
	{"internal/gitlet/query/objects.go", "rev-parse", queryServes},
	{"internal/gitlet/query/objects.go", "cat-file", queryServes},
	{"internal/gitlet/query/patches.go", "diff-pairs", queryServes},
	{"internal/gitlet/query/search.go", "rev-parse", queryServes},
	{"internal/gitlet/query/search.go", "ls-tree", queryServes},
	{"internal/gitlet/query/search.go", "grep", queryServes},
	{"internal/gitlet/query/search.go", "cat-file", queryServes},
	{"internal/gitlet/query/tips.go", "cat-file", queryServes},
	{"internal/gitlet/query/tips.go", "log", queryServes},
	{"internal/gitlet/query/tree.go", "ls-tree", queryServes},
	{"internal/gitlet/query/treecommits.go", "log", queryServes},
	{"internal/gitlet/query/treecommits.go", "ls-tree", queryServes},
}

// A CHECK IS CHARGED FOR THE DROPPING VIEW IT WALKS: git marks every object
// of every object directory Verify's check is handed, so it holds
// FsckBytesPerObject for the dropping packs' objects too; the room beside it
// is for a clone, of live objects alone. A budget holding the charge over the
// live objects only refuses it with ErrNoMemory, at admission over a copy
// holding the view and at the re-charge after a sync brought one; a budget
// holding both runs it clean.
//
// Mutations: charge Verification.charge by the live objects alone, and the
// check admitted before the expunge runs on the smaller budget; charge
// AdmitVerify so, and the copy's own check is admitted on it; let placeMoved
// count no dropping pack's objects, and both are.
func TestACheckIsChargedForTheDroppingViewItWalks(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	base, first := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	before := cache.FsckCharge(c, repo)

	gone, second := w.push(t, repo, "refs/heads/gone", nil)
	w.client.Run(t, w.repos[repo].work, "reset", "--hard", base)
	_, third := w.push(t, repo, "refs/heads/main", nil)
	w.log.write(repo, nil, map[string]string{"refs/heads/gone": ""})
	kept, dropping := w.expungeBegin(t, repo, []string{gone}, first, second, third)
	live := kept.Objects * (cache.FsckBytesPerObject + gitproc.TransferBytesPerObject)
	both := live + dropping.Objects*cache.FsckBytesPerObject
	if dropping.Objects == 0 || before > live {
		t.Fatalf("the case is no test: the dropping pack holds %d objects, and the copy before the expunge is "+
			"charged %d, over the %d its live objects are after it", dropping.Objects, before, live)
	}

	cache.SetMaintenanceBudget(c, live)
	v, err := c.AdmitVerify(repo)
	if err != nil {
		t.Fatalf("a check of the copy before the expunge, on a budget of %d past its charge of %d: %v", live,
			before, err)
	}
	if _, err := v.Run(t.Context(), cache.VerifyRun{}); !errors.Is(err, cache.ErrNoMemory) {
		t.Errorf("the check admitted before the expunge, its sync having placed a dropping view of %d objects, "+
			"on a budget of %d = %v, want ErrNoMemory: its fsck walks the view, %d bytes in all", dropping.Objects,
			live, err, both)
	}
	v.Release()

	if v, err := c.AdmitVerify(repo); !errors.Is(err, cache.ErrNoMemory) {
		if err == nil {
			v.Release()
		}
		t.Errorf("a check of the copy holding a dropping view of %d objects, on a budget of %d, admitted = %v; "+
			"want ErrNoMemory: its fsck walks the view, %d bytes in all", dropping.Objects, live, err, both)
	}

	cache.SetMaintenanceBudget(c, both)
	if r, err := verify(t.Context(), c, repo, false); err != nil || len(r.Problems) != 0 {
		t.Errorf("Verify at its charge over the live and the dropping objects, %d = %+v, %v; want it run, and "+
			"clean", both, r, err)
	}
}
