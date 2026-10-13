package cache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A COLD SYNC DOWNLOADS SIXTEEN AT ONCE: on S3 a GET is bound by its first
// byte, so width is what a cold sync is bought with — every open is held
// until sixteen are open together, which a sync downloading fewer at once
// never reaches.
func TestAColdSyncDownloadsSixteenAtOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onS3)
	repo := w.repo(t, 1)
	for range 40 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t)
	w.objects.holdUntil(cache.FetchConcurrency, hangDetector)
	began := time.Now()
	w.sync(t, c, repo)
	if took := time.Since(began); took >= hangDetector {
		t.Fatalf("the sync took %v: the gate of %d opens at once was never reached", took, cache.FetchConcurrency)
	}
	if _, _, most := w.objects.counts(); most != cache.FetchConcurrency {
		t.Errorf("the most downloads at once was %d, want %d", most, cache.FetchConcurrency)
	}
}

// EVERY CACHE ON A NODE SHARES ITS FETCHES: two repositories synced at once
// download together at most what the node allows, not what each sync does.
func TestEveryCacheOnANodeSharesItsFetches(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onS3)
	w.s3.Latency = 20 * time.Millisecond // every GET waits, so downloads overlap
	a, b := w.repo(t, 1), w.repo(t, 2)
	for range 12 {
		w.push(t, a, "refs/heads/main", nil)
		w.push(t, b, "refs/heads/main", nil)
	}
	c := w.open(t)
	cache.SetNodeFetches(c, 4)
	var wg sync.WaitGroup
	for _, repo := range []string{a, b} {
		wg.Go(func() {
			if _, err := c.Sync(t.Context(), repo, linearizable); err != nil {
				t.Errorf("sync of %s: %v", repo, err)
			}
		})
	}
	wg.Wait()
	if _, _, most := w.objects.counts(); most != 4 {
		t.Errorf("the most downloads at once across both syncs was %d, want the node's 4", most)
	}
}

// THE NODE'S DOWNLOAD BUFFERS ARE ITS SLOTS: a copy buffer is handed out with
// a node slot and taken back with it, so however many repositories' rounds
// wait for one — a node that just joined a fleet, asked for every repository
// at once — the node holds NodeFetches buffers, never one for every round's
// every worker. A round waiting for a slot holds no buffer at all.
func TestTheNodesDownloadBuffersAreItsSlots(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	var repos []string
	for i := range 6 {
		repo := w.repo(t, i+1)
		for range 3 {
			w.push(t, repo, "refs/heads/main", nil)
		}
		repos = append(repos, repo)
	}
	c := w.open(t)
	// One download a round, so the node's two slots are two rounds' and the
	// other four each wait for one.
	cache.SetNodeFetches(c, 2)
	cache.SetFetchConcurrency(c, 1)
	release := make(chan struct{})
	w.objects.hold = func(objstore.Key) <-chan struct{} { return release }
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Go(func() {
			s, err := c.Sync(t.Context(), repo, linearizable)
			if err != nil {
				t.Errorf("sync of %s: %v", repo, err)
				return
			}
			s.Release()
		})
	}
	// Two rounds' downloads hold the node's two slots, and every other round
	// waits for one.
	eventually(t, "two downloads held and four rounds waiting for a slot", func() bool {
		return w.objects.open() == 2 && cache.SlotWaiters(c) == 4
	})
	if now, most := cache.BuffersOut(c); now != 2 || most != 2 {
		t.Errorf("with six rounds at the node's two slots, %d copy buffers are out (at most %d), want 2", now, most)
	}
	close(release)
	wg.Wait()
	if now, most := cache.BuffersOut(c); now != 0 || most != 2 {
		t.Errorf("after every round, %d copy buffers are out and at most %d were, want none and 2", now, most)
	}
}

// A NODE SLOT GOES BACK WHEN THE GET ENDS: what follows a download — the
// seal's check, the files made durable, the renames — is this node's disk, and
// a slot held through it is a GET the node does not make. With every install
// held at its first stage, the node's one slot is free for the next download.
func TestANodeSlotGoesBackWhenTheGetEnds(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t)
	cache.SetNodeFetches(c, 1)
	cache.SetFetchConcurrency(c, 3)
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	t.Cleanup(release) // before the cache's Stop, which waits for the round
	var installing atomic.Int32
	cache.SetInstallHook(c, func(stage, _ string) {
		if stage == "fetched" {
			installing.Add(1)
			<-held
		}
	})
	done := make(chan error, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			s.Release()
		}
		done <- err
	}()
	// Three downloads through one slot, each install held after its GET:
	// only a slot given back before the install lets the next GET start.
	eventually(t, "every download fetched while every install waits", func() bool {
		return installing.Load() == 3
	})
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// THE LARGEST PACK IS FETCHED FIRST: a round ends when its last download does,
// so the longest start first and the small ones fill the gaps.
func TestTheLargestPackIsFetchedFirst(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	sizes := map[objstore.Key]int64{}
	for i, size := range []int{1 << 10, 64 << 10, 512 << 10} {
		_, row := w.push(t, repo, "refs/heads/main", map[string]string{fmt.Sprintf("blob%d", i): noise(i, size)})
		sizes[row.Object] = row.Size
	}
	c := w.open(t)
	cache.SetFetchConcurrency(c, 1)
	w.sync(t, c, repo)
	_, order, _ := w.objects.counts()
	var got []int64
	for _, k := range order {
		got = append(got, sizes[k])
	}
	if !slices.IsSortedFunc(got, func(a, b int64) int { return int(b - a) }) || len(got) != 3 {
		t.Errorf("downloaded bundles of %v bytes in that order, want the largest first", got)
	}
}

// noise is size bytes no compressor shrinks, from seed.
func noise(seed, size int) string {
	r := rand.New(rand.NewPCG(uint64(seed), 7))
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return string(b)
}

// A COLD SYNC RUNS NO GIT: a bundle carries the index and the reverse index
// its receiving git wrote, so installing it is renames — no node but the
// receiving one ever runs index-pack. The only processes a round may run are
// the peel of the tags it sets and, past MidxPacks packs, the index it writes
// before it answers.
func TestAColdSyncRunsNoGit(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		packs int
		tag   bool
		want  [][]string
	}{
		{name: "seven packs", packs: cache.MidxPacks - 1},
		{name: "an annotated tag", packs: 3, tag: true, want: [][]string{{"cat-file", "--batch-check=%(objectname)"}}},
		{name: "twenty packs", packs: 20, want: [][]string{{"multi-pack-index", "write", "--stdin-packs", "--no-progress"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			path, calls := gittest.Recording(t)
			w.gitPath = path
			repo := w.repo(t, 1)
			for range c.packs {
				w.push(t, repo, "refs/heads/main", nil)
			}
			if c.tag {
				w.tag(t, repo, "v1", true)
			}
			cc := w.open(t)
			// The recording lists its calls in no order: what the sync ran
			// is what is there now and was not before.
			seen := map[string]int{}
			for _, call := range calls() {
				seen[fmt.Sprint(call.Args)]++
			}
			s := w.sync(t, cc, repo)
			var got [][]string
			for _, call := range calls() {
				if key := fmt.Sprint(call.Args); seen[key] > 0 {
					seen[key]--
					continue
				}
				got = append(got, call.Args)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("a cold sync ran %q, want %q", got, c.want)
			}
			if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
				t.Errorf("fsck of a cache installed with no git: %v", err)
			}
		})
	}
}

// A BUNDLE IS SPLIT AS ITS ROW SAYS: the pack, the index and the reverse index
// at the row's sizes, each byte for byte what the receiving git wrote — and
// all three are in tmp/, checked, before any of them is in objects/pack.
func TestABundleIsSplitAsItsRowSays(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, small := w.push(t, repo, "refs/heads/main", nil)
	many := map[string]string{}
	for i := range 2998 {
		many[fmt.Sprintf("d/%04d", i)] = fmt.Sprintf("file %d\n", i)
	}
	_, large := w.push(t, repo, "refs/heads/main", many)
	if large.Objects < 3000 {
		t.Fatalf("the large pack holds %d objects, want 3 000", large.Objects)
	}
	c := w.open(t)
	cache.SetFetchConcurrency(c, 1)
	installed := 0
	cache.SetInstallHook(c, func(stage, packDir string) {
		if stage != "fetched" {
			return
		}
		tmp := names(t, w.path("tmp"))
		if len(tmp) != 3 {
			t.Errorf("at %q tmp/ holds %v, want one bundle's three files", stage, tmp)
		}
		if got := len(packFiles(t, filepath.Dir(filepath.Dir(packDir)))); got != 3*installed {
			t.Errorf("at %q objects/pack holds %d pack files, want %d: a checked bundle's files are in "+
				"objects/pack before every one is", stage, got, 3*installed)
		}
		installed++
	})
	s := w.sync(t, c, repo)
	for _, row := range []code.PackRow{small, large} {
		b := w.bundle(row.GitChecksum)
		want := map[string][]byte{
			".pack": b.Bytes[:b.PackBytes],
			".idx":  b.Bytes[b.PackBytes : b.PackBytes+b.IndexBytes],
			".rev":  b.Bytes[b.PackBytes+b.IndexBytes:],
		}
		for ext, section := range want {
			got, err := os.ReadFile(filepath.Join(s.Dir, "objects", "pack", "pack-"+row.GitChecksum+ext))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, section) {
				t.Errorf("the %s of %s is %d bytes not the receiver's %d", ext, row.PackID, len(got), len(section))
			}
		}
	}
}

// A SYNC INSTALLS PACKS BEFORE REFS: a reader never sees a ref before its
// objects. Held at the moment the refs are staged, the new commit is already
// readable in the incarnation and packed-refs still names the old tip; and
// at every step of an install, a pack's .idx — what makes git count it — is
// there only once its .pack is.
func TestASyncInstallsPacksBeforeRefs(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	old, _ := w.push(t, repo, "refs/heads/main", nil)
	script := &codefault.Script{}
	c := w.open(t, func(o *cache.Options) { o.Faults = script })
	w.sync(t, c, repo).Release()
	cache.SetInstallHook(c, func(_, packDir string) {
		for _, name := range names(t, packDir) {
			if strings.HasSuffix(name, ".idx") && !statOK(t, filepath.Join(packDir, strings.TrimSuffix(name, ".idx")+".pack")) {
				t.Errorf("%s is in objects/pack before its .pack", name)
			}
		}
	})
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	reached, held := make(chan struct{}), make(chan struct{})
	script.Arm(codefault.SyncRefs, codefault.Action{Block: held, Reached: reached})
	done := make(chan error, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			s.Release()
		}
		done <- err
	}()
	<-reached
	dir := w.dir(t, c, repo)
	if _, err := runGit(t, w.pool, dir, "", "cat-file", "-e", tip); err != nil {
		t.Errorf("held before the refs move, the new commit is not readable: %v", err)
	}
	if refs := read(t, filepath.Join(dir, "packed-refs")); !strings.Contains(refs, old+" refs/heads/main") {
		t.Errorf("held before the refs move, packed-refs already moved:\n%s", refs)
	}
	close(held)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if refs := read(t, filepath.Join(dir, "packed-refs")); !strings.Contains(refs, tip+" refs/heads/main") {
		t.Errorf("after the round packed-refs does not name the new tip:\n%s", refs)
	}
}

// A MISSING PACK IS ASKED AGAIN LINEARIZABLY: a copy behind the log may name
// a pack a purge has since removed with its row, so a round that was not
// linearizable reads again at linearizable before it calls the pack missing —
// where the repository may be gone, or the pack definitely is. Only that
// final answer is logged: a not-found the log's head explains is no alarm.
func TestAMissingPackIsAskedAgainLinearizably(t *testing.T) {
	t.Parallel()
	for _, purged := range []bool{true, false} {
		t.Run(fmt.Sprintf("purged=%t", purged), func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			_, row := w.push(t, repo, "refs/heads/main", nil)
			if err := w.store.Delete(t.Context(), row.Object); err != nil {
				t.Fatal(err)
			}
			if purged {
				w.log.set(func(f *fakeLog) {
					f.hook = func(call fakeCall) error {
						if call.Level == "linearizable" {
							w.log.purge(repo)
						}
						return nil
					}
				})
			}
			c := w.open(t)
			_, err := c.Sync(t.Context(), repo, stale)
			var levels []string
			for _, call := range w.log.callsOf(repo) {
				levels = append(levels, string(call.Level))
			}
			if fmt.Sprint(levels) != "[stale linearizable]" {
				t.Errorf("the round read at %v, want stale then linearizable", levels)
			}
			if purged {
				if !errors.Is(err, code.ErrRepoNotFound) {
					t.Errorf("Sync: %v, want code.ErrRepoNotFound", err)
				}
				// The pack was never missing: the copy that named it was
				// behind a purge, and nothing is logged against the store.
				if got := w.logs.lines(t, "code_pack_missing"); len(got) != 0 {
					t.Errorf("logged %d code_pack_missing lines for a pack a purge removed with its row", len(got))
				}
				return
			}
			var missing *cache.ErrPackMissing
			if !errors.As(err, &missing) || missing.Pack.PackID != row.PackID || missing.Pack.Object != row.Object ||
				missing.Damaged || !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("Sync: %v, want *ErrPackMissing naming %s", err, row.PackID)
			}
			if got := w.logs.lines(t, "code_pack_missing"); len(got) != 1 {
				t.Errorf("logged %d code_pack_missing lines, want the one the log's head made final", len(got))
			}
		})
	}
}

// A DAMAGED PACK IS NAMED: bytes the store holds under the row's object that
// are not the row's are a definite answer, and nothing of them is installed.
func TestADamagedPackIsNamed(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, row := w.push(t, repo, "refs/heads/main", nil)
	w.mem.Corrupt(row.Object.Name(), bytes.Repeat([]byte{0xff}, int(row.Size)))
	c := w.open(t)
	_, err := c.Sync(t.Context(), repo, linearizable)
	var missing *cache.ErrPackMissing
	if !errors.As(err, &missing) || !missing.Damaged || !errors.Is(err, objstore.ErrCorrupt) {
		t.Fatalf("Sync: %v, want a damaged *ErrPackMissing", err)
	}
	if got := packFiles(t, w.dir(t, c, repo)); len(got) != 0 {
		t.Errorf("a damaged pack left %v in objects/pack", got)
	}
	w.tmpEmpty(t)
}

// A CHECKSUM MISMATCH IS NOT RETRIED: the store has just said these are the
// bytes the receiver stored, so a bundle that is not its row's is the row's
// or the uploader's fault, and asking again — at any level — fixes neither.
// The round reads at stale, where a pack the store does not have WOULD be
// asked about again at linearizable: a mismatch is not.
func TestAChecksumMismatchIsNotRetried(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, first := w.push(t, repo, "refs/heads/main", nil)
	_, second := w.push(t, repo, "refs/heads/main", nil)
	bad := w.storeBundle(t, w.bundle(first.GitChecksum), "")
	bad.GitChecksum = second.GitChecksum
	w.log.write(repo, []code.PackRow{bad}, nil)
	c := w.open(t)
	_, err := c.Sync(t.Context(), repo, stale)
	var corrupt *cache.ErrCorruptPack
	if !errors.As(err, &corrupt) || corrupt.Pack.PackID != bad.PackID || !errors.Is(err, code.ErrBadBundle) ||
		!strings.Contains(err.Error(), second.GitChecksum) {
		t.Fatalf("Sync: %v, want *ErrCorruptPack naming the checksum", err)
	}
	if opens, _, _ := w.objects.counts(); opens[bad.Object] != 1 {
		t.Errorf("the bundle was downloaded %d times, want once", opens[bad.Object])
	}
	if n := len(w.log.firstPages(repo)); n != 1 {
		t.Errorf("the round read its snapshot %d times, want once: a mismatch is not retried", n)
	}
	for _, call := range w.log.callsOf(repo) {
		if call.Level == statelog.ReadLinearizable {
			t.Errorf("the round read at linearizable (%+v): a mismatch is asked of no one again", call)
		}
	}
	if len(w.logs.lines(t, "code_pack_rejected")) == 0 {
		t.Error("no code_pack_rejected line")
	}
	w.tmpEmpty(t)
}

// BYTES THAT ARE NOT THE ROW'S BUNDLE ARE A CORRUPT PACK, whatever their
// digest: each case is uploaded under its TRUE digest, so the store's check
// passes and only the bundle's own seal can see it.
func TestBytesThatAreNotTheRowsBundleAreACorruptPack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		bytes func(w *syncWorld, own, other gittest.Bundled) []byte
	}{
		{"random bytes", func(_ *syncWorld, own, _ gittest.Bundled) []byte {
			return []byte(noise(1, len(own.Bytes)))
		}},
		{"the index and the reverse index swapped", func(_ *syncWorld, own, _ gittest.Bundled) []byte {
			pack, idx, rev := sections(own)
			return slices.Concat(pack, rev, idx)
		}},
		{"another pack's index of the same count", func(_ *syncWorld, own, other gittest.Bundled) []byte {
			pack, _, rev := sections(own)
			_, idx, _ := sections(other)
			return slices.Concat(pack, idx, rev)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			_, row := w.push(t, repo, "refs/heads/main", nil)
			_, other := w.push(t, repo, "refs/heads/dev", nil)
			own, them := w.bundle(row.GitChecksum), w.bundle(other.GitChecksum)
			if own.Objects != them.Objects {
				t.Fatalf("the fixtures' packs hold %d and %d objects", own.Objects, them.Objects)
			}
			body := c.bytes(w, own, them)
			obj, err := w.store.Put(t.Context(), bytes.NewReader(body), int64(len(body)), objstore.PutMeta{})
			if err != nil {
				t.Fatal(err)
			}
			bad := row
			bad.PackID, bad.Object, bad.Hash, bad.Size = "01900000-0000-7000-8000-000000000001", obj.Key, obj.Hash, obj.Size
			bad.IndexBytes, bad.ReverseBytes = own.IndexBytes, own.ReverseBytes
			w.log.set(func(f *fakeLog) {
				f.repos[repo].packs = []code.PackRow{bad, other}
			})
			cc := w.open(t)
			_, err = cc.Sync(t.Context(), repo, linearizable)
			var corrupt *cache.ErrCorruptPack
			if !errors.As(err, &corrupt) || !errors.Is(err, code.ErrBadBundle) {
				t.Fatalf("Sync: %v, want *ErrCorruptPack wrapping code.ErrBadBundle", err)
			}
			if len(w.logs.lines(t, "code_pack_rejected")) == 0 {
				t.Error("no code_pack_rejected line")
			}
			if statOK(t, filepath.Join(w.dir(t, cc, repo), "objects", "pack", "pack-"+row.GitChecksum+".idx")) {
				t.Error("a corrupt bundle was installed")
			}
			w.tmpEmpty(t)
		})
	}
}

// sections is a bundle's pack, index and reverse index.
func sections(b gittest.Bundled) ([]byte, []byte, []byte) {
	return b.Bytes[:b.PackBytes], b.Bytes[b.PackBytes : b.PackBytes+b.IndexBytes], b.Bytes[b.PackBytes+b.IndexBytes:]
}

// failOnHit fails the n'th hit of one point, and only that one.
type failOnHit struct {
	point codefault.Point
	n     int

	mu   sync.Mutex
	hits int
}

func (f *failOnHit) Hit(_ context.Context, p codefault.Point, _ int64) error {
	if p != f.point {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.hits == f.n {
		return errors.New("a download that failed")
	}
	return nil
}

// A FAILED ROUND LEAVES NO TEMPORARY FILES — and keeps what it installed:
// the packs before the failure are named by rows, so the next round skips
// them and downloads only the rest.
func TestAFailedRoundLeavesNoTemporaryFiles(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range 6 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, func(o *cache.Options) { o.Faults = &failOnHit{point: codefault.SyncDownload, n: 3} })
	cache.SetFetchConcurrency(c, 1)
	if _, err := c.Sync(t.Context(), repo, linearizable); err == nil {
		t.Fatal("a round whose third download failed succeeded")
	}
	w.tmpEmpty(t)
	installed := 0
	for _, name := range packFiles(t, w.dir(t, c, repo)) {
		if strings.HasSuffix(name, ".idx") {
			installed++
		}
	}
	if installed != 2 {
		t.Errorf("%d packs installed before the failure stayed, want 2", installed)
	}
	before := w.objects.total()
	w.sync(t, c, repo)
	if got := w.objects.total() - before; got != 6-installed {
		t.Errorf("the next round downloaded %d bundles, want the %d not installed", got, 6-installed)
	}
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("image:\n%s\nwant:\n%s", got, w.rebuilt(t, repo))
	}
}
