package cache_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
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
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
// every worker. A round waiting for a slot, or for a pack in hand before it,
// holds no buffer at all.
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
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	t.Cleanup(release) // before the cache's Stop, which waits for the rounds
	w.objects.hold = func(objstore.Key) <-chan struct{} { return held }
	var wg sync.WaitGroup
	start := func(repo string) {
		wg.Go(func() {
			s, err := c.Sync(t.Context(), repo, linearizable)
			if err != nil {
				t.Errorf("sync of %s: %v", repo, err)
				return
			}
			s.Release()
		})
	}
	// Two rounds' downloads hold the node's two slots, and each of those
	// rounds holds a second pack in hand waiting on its own share — the
	// node's four — so every other round waits for a pack in hand, holding
	// no files, no slot and no buffer. The two are started one after the
	// other, each once the one before holds its two packs: rounds started
	// together share the node's four packs in hand however they reach
	// them, and a round holding one waits for its next as the others do.
	for i, repo := range repos[:2] {
		start(repo)
		eventually(t, fmt.Sprintf("round %d's download held and its second pack in hand", i+1), func() bool {
			now, _ := cache.InHand(c)
			return w.objects.open() == i+1 && now == int64(2*(i+1))
		})
	}
	for _, repo := range repos[2:] {
		start(repo)
	}
	eventually(t, "two downloads held and four rounds waiting for a pack in hand", func() bool {
		return w.objects.open() == 2 && cache.HandWaiters(c) == 4 && cache.SlotWaiters(c) == 0
	})
	if now, most := cache.BuffersOut(c); now != 2 || most != 2 {
		t.Errorf("with six rounds at the node's two slots, %d copy buffers are out (at most %d), want 2", now, most)
	}
	release()
	wg.Wait()
	if now, most := cache.BuffersOut(c); now != 0 || most != 2 {
		t.Errorf("after every round, %d copy buffers are out and at most %d were, want none and 2", now, most)
	}
}

// A NODE SLOT GOES BACK WHEN THE GET ENDS: what follows a download — the
// seal's check, the files made durable, the renames — is this node's disk, and
// a slot held through it is a GET the node does not make. With every install
// held at its first stage, the node's one slot is free for the next download,
// up to the packs the node holds in hand for it (installsPerFetch, two) and
// no further.
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
	// Downloads through one slot, each install held after its GET: only a
	// slot given back before the install lets the next GET start — and the
	// node's two packs in hand stop the third.
	eventually(t, "two downloads fetched through one slot while their installs wait", func() bool {
		return installing.Load() == 2
	})
	if now, most := cache.InHand(c); now != 2 || most != 2 {
		t.Errorf("with two installs held, the node holds %d packs in hand (at most %d), want its two", now, most)
	}
	if got := installing.Load(); got != 2 {
		t.Errorf("%d downloads fetched past the node's two packs in hand", got)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A PACK IN HAND WAITS FOR ITS GET WITH ITS FILES MADE: creating a bundle's
// three files is the dearest step of its disk work on the measured machine —
// a third of a millisecond a file, made one at a time in tmp/ — and made
// while the GET's share and slot are held it is time no GET runs in. So the
// round makes a pack's files as it hands the pack out, and its worker only
// then waits for its turn and the round's share: with a round of one GET at
// a time and that GET held open, the round's second pack in hand already has
// its three files in tmp/.
func TestAPackInHandWaitsForItsGetWithItsFilesMade(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t)
	cache.SetFetchConcurrency(c, 1)
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	t.Cleanup(release) // before the cache's Stop, which waits for the round
	w.objects.hold = func(objstore.Key) <-chan struct{} { return held }
	done := make(chan error, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			s.Release()
		}
		done <- err
	}()
	eventually(t, "the held GET's files and the next pack's in tmp/", func() bool {
		return w.objects.open() == 1 && len(names(t, w.path("tmp"))) == 6
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

// A ROUND GRANTS ITS GETS IN ITS ORDER, LARGEST FIRST, and makes its bundles'
// files in that order too. Its workers run in whatever order the scheduler
// picks — usually the one started last first — so a round whose workers each
// asked for a GET as soon as they could gave its first shares to the packs it
// handed out last, and a round whose workers made their own files held its
// first GET behind whichever pack's files were made last. Here the order the
// round made the files in is read, and the largest pack's worker is held, its
// files made, until the round's whole share is in GETs or grantQuiet passes: a
// round that grants in its order starts none meanwhile. Eight packs at a share
// of two, more than the four a round holds in hand, every GET held until the
// case lets it go, one at a time — so the order is checked at every grant,
// across every window the hand moves through, and never at the instant two
// GETs start together.
func TestARoundGrantsItsGetsInItsOrder(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	// Sizes in an order that is neither the pushes' nor their pack ids'.
	sizes := map[objstore.Key]int64{}
	var largest []objstore.Key
	for i, n := range []int{3, 7, 1, 5, 0, 6, 2, 4} {
		_, row := w.push(t, repo, "refs/heads/main", map[string]string{fmt.Sprintf("blob%d", i): noise(i, (n+1)<<13)})
		sizes[row.Object] = row.Size
		largest = append(largest, row.Object)
	}
	slices.SortFunc(largest, func(a, b objstore.Key) int { return cmp.Compare(sizes[b], sizes[a]) })

	const share = 2
	// full is closed once the round's whole share is in GETs: a round that
	// granted out of its order has given every share away by then, before
	// the largest pack has asked for one.
	full := make(chan struct{})
	var opens atomic.Int64
	gets := map[objstore.Key]chan struct{}{}
	for k := range sizes {
		gets[k] = make(chan struct{})
	}
	w.objects.hold = func(k objstore.Key) <-chan struct{} {
		if opens.Add(1) == share {
			close(full)
		}
		return gets[k]
	}
	var stagedMu sync.Mutex
	var staged []objstore.Key
	c := w.open(t, cache.StagedWith(func(k objstore.Key) {
		stagedMu.Lock()
		defer stagedMu.Unlock()
		staged = append(staged, k)
	}), cache.AskingWith(func(k objstore.Key) {
		if k == largest[0] {
			select {
			case <-full:
			case <-time.After(grantQuiet):
			}
		}
	}))
	var letMu sync.Mutex
	let := func(k objstore.Key) {
		letMu.Lock()
		defer letMu.Unlock()
		select {
		case <-gets[k]:
		default:
			close(gets[k])
		}
	}
	t.Cleanup(func() { // before the cache's Stop, which waits for the round
		for k := range gets {
			let(k)
		}
	})
	cache.SetFetchConcurrency(c, share)
	done := make(chan error, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			s.Release()
		}
		done <- err
	}()
	opened := func(n int) []objstore.Key {
		t.Helper()
		eventually(t, fmt.Sprintf("GET %d of the round", n), func() bool { return w.objects.total() >= n })
		_, order, _ := w.objects.counts()
		return order
	}
	describe := func(keys []objstore.Key) []int64 {
		var out []int64
		for _, k := range keys {
			out = append(out, sizes[k])
		}
		return out
	}
	got := opened(share)
	if len(got) != share || !slices.Contains(got, largest[0]) || !slices.Contains(got, largest[1]) {
		t.Fatalf("a round of two GETs at once started bundles of %v bytes first, want the two largest %v",
			describe(got), describe(largest[:share]))
	}
	for i := share; i < len(largest); i++ {
		// The earliest GET still held ends, and its share is the round's one
		// grant: exactly one more GET starts, and it is the next largest.
		let(got[i-share])
		got = opened(i + 1)
		if len(got) != i+1 || got[i] != largest[i] {
			t.Fatalf("a round's GETs started on bundles of %v bytes, want %v",
				describe(got), describe(largest[:i+1]))
		}
	}
	for k := range gets {
		let(k)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stagedMu.Lock()
	defer stagedMu.Unlock()
	if !slices.Equal(staged, largest) {
		t.Errorf("the round made the files of bundles of %v bytes in that order, want %v",
			describe(staged), describe(largest))
	}
}

// grantQuiet is how long TestARoundGrantsItsGetsInItsOrder holds the largest
// pack's worker while the round's share is not all in GETs: room for the
// round's other workers to make their files and ask, a few milliseconds each.
// Never a performance claim — a round that grants in its order passes however
// long it is.
const grantQuiet = 250 * time.Millisecond

// A ROUND'S GETS NEVER WAIT ON ITS DISK: the round's share goes back when a GET
// ends, so with every file's sync held, a round of one GET at a time still
// downloads its next bundle — and stops at the packs it may hold in hand
// (installsPerFetch, two), so the disk it does wait on holds its GETs back
// rather than a growing backlog of downloads.
func TestARoundsGetsNeverWaitOnItsDisk(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range 4 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	c := w.open(t, cache.FileSyncWith(func(f *os.File) error {
		<-held
		return f.Sync()
	}))
	t.Cleanup(release) // before the cache's Stop, which waits for the round
	cache.SetFetchConcurrency(c, 1)
	done := make(chan error, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			s.Release()
		}
		done <- err
	}()
	eventually(t, "a second GET while the first bundle's syncs are held", func() bool {
		return w.objects.total() >= 2
	})
	// The round holds its two packs in hand now: one whose syncs are held,
	// and one downloaded behind it. A third GET would be a backlog.
	time.Sleep(50 * time.Millisecond)
	if got := w.objects.total(); got != 2 {
		t.Errorf("with its syncs held, a round of one GET at a time made %d GETs, want its two packs in hand", got)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := w.objects.total(); got != 4 {
		t.Errorf("the round made %d GETs for its four packs", got)
	}
}

// THE NODE HOLDS TWO SHARES OF PACKS IN HAND: however many rounds run, the
// node's packs between their GET and their place — downloading, or downloaded
// and waiting on the disk — are at most installsPerFetch × NodeFetches, so a
// joining node whose disk stalls under a burst of cold syncs stops starting
// GETs rather than piling up downloads, and syncs in flight, without end.
func TestTheNodeHoldsTwoSharesOfPacksInHand(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	var repos []string
	for i := range 3 {
		repo := w.repo(t, i+1)
		for range 4 {
			w.push(t, repo, "refs/heads/main", nil)
		}
		repos = append(repos, repo)
	}
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	c := w.open(t, cache.FileSyncWith(func(f *os.File) error {
		<-held
		return f.Sync()
	}))
	t.Cleanup(release) // before the cache's Stop, which waits for the rounds
	cache.SetNodeFetches(c, 1)
	cache.SetFetchConcurrency(c, 4)
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Go(func() {
			s, err := c.Sync(context.Background(), repo, linearizable)
			if err != nil {
				t.Errorf("sync of %s: %v", repo, err)
				return
			}
			s.Release()
		})
	}
	eventually(t, "the node's two packs in hand downloaded while their syncs are held", func() bool {
		return w.objects.total() >= 2
	})
	time.Sleep(50 * time.Millisecond)
	if got := w.objects.total(); got != 2 {
		t.Errorf("with every sync held, three rounds made %d GETs through a node of one slot, "+
			"want its two packs in hand", got)
	}
	release()
	wg.Wait()
	if _, most := cache.InHand(c); most != 2 {
		t.Errorf("the node held at most %d packs in hand, want its two", most)
	}
}

// A BUNDLE IS DURABLE BEFORE ANY OF IT IS PLACED: every file of a bundle is
// synced while it is still in tmp/, the three of a bundle in flight together
// — syncs one after another each pay a journal commit of their own — and no
// file of the bundle is in objects/pack until all of them have been synced.
func TestABundleIsDurableBeforeAnyOfItIsPlaced(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, row := w.push(t, repo, "refs/heads/main", nil)
	var mu sync.Mutex
	inFlight, most := 0, 0
	together := make(chan struct{})
	var once sync.Once
	c := w.open(t, cache.FileSyncWith(func(f *os.File) error {
		if dir := filepath.Dir(f.Name()); dir != w.path("tmp") {
			t.Errorf("synced %s, not a file in tmp/", f.Name())
		}
		placed, err := filepath.Glob(w.path("repos", "*", "objects", "pack", "pack-"+row.GitChecksum+".*"))
		if err != nil || len(placed) > 0 {
			t.Errorf("while %s is synced, objects/pack already holds %v (%v)", filepath.Base(f.Name()), placed, err)
		}
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		if inFlight == 3 {
			once.Do(func() { close(together) })
		}
		mu.Unlock()
		select {
		case <-together:
		case <-time.After(hangDetector):
		}
		err = f.Sync()
		mu.Lock()
		inFlight--
		mu.Unlock()
		return err
	}))
	s := w.sync(t, c, repo)
	defer s.Release()
	if most != 3 {
		t.Errorf("at most %d of the bundle's three syncs were in flight together, want all three", most)
	}
	if got := packFiles(t, s.Dir); len(got) != 3 {
		t.Errorf("objects/pack holds %v after the round, want the bundle's three files", got)
	}
}

// A SYNC THAT FAILS PLACES NOTHING OF ITS BUNDLE: its files stay out of
// objects/pack and leave tmp/, the round answers the failure, and the next
// round downloads the pack again and places it.
func TestASyncThatFailsPlacesNothingOfItsBundle(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, row := w.push(t, repo, "refs/heads/main", nil)
	errDisk := errors.New("the disk failed this sync")
	var failed atomic.Bool
	c := w.open(t, cache.FileSyncWith(func(f *os.File) error {
		if strings.HasSuffix(f.Name(), ".idx") && failed.CompareAndSwap(false, true) {
			return errDisk
		}
		return f.Sync()
	}))
	if _, err := c.Sync(t.Context(), repo, linearizable); !errors.Is(err, errDisk) {
		t.Fatalf("a round whose sync failed answered %v, want the sync's failure", err)
	}
	placed, err := filepath.Glob(w.path("repos", "*", "objects", "pack", "pack-"+row.GitChecksum+".*"))
	if err != nil || len(placed) > 0 {
		t.Errorf("a bundle whose sync failed has %v in objects/pack (%v)", placed, err)
	}
	if left := names(t, w.path("tmp")); len(left) > 0 {
		t.Errorf("a bundle whose sync failed left %v in tmp/", left)
	}
	s := w.sync(t, c, repo)
	defer s.Release()
	if got := packFiles(t, s.Dir); len(got) != 3 {
		t.Errorf("the next round left %v in objects/pack, want the bundle's three files", got)
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
// the peel of the tags it sets and, from MidxRoundPacks packs (lowered here to
// caseRoundPacks), the index it writes before it answers.
func TestAColdSyncRunsNoGit(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		packs int
		tag   bool
		want  [][]string
	}{
		{name: "under the round's threshold", packs: caseRoundPacks - 1},
		{name: "an annotated tag", packs: 3, tag: true, want: [][]string{{"cat-file", "--batch-check=%(objectname)", "--buffer"}}},
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
			cc := w.open(t, cache.RoundPacksAt(caseRoundPacks))
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
// at the row's sizes, each byte for byte what the receiving git wrote. That
// all three are in tmp/, checked and durable, before any of them is in
// objects/pack is TestABundleIsDurableBeforeAnyOfItIsPlaced's.
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

// A MISSING PACK IS ASKED AGAIN LINEARIZABLY, ONCE, WHATEVER THE ROUND READ
// AT: a copy behind the log may name a pack a purge has since removed with its
// row, and even a read at the log's head was made before the store answered,
// so a round reads at linearizable once more before it calls the pack missing
// — where the repository may be gone, or the pack definitely is. Only that
// final answer is logged: a not-found the log's head explains is no alarm.
// And it is final: a pack the store has lost costs its round one read more,
// never a loop — a third read is failed, so a round that kept asking fails
// the case rather than hangs it.
//
// Mutation: ask again only from a round below linearizable (the linearizable
// rounds read once, and the purged one calls the pack missing).
func TestAMissingPackIsAskedAgainLinearizably(t *testing.T) {
	t.Parallel()
	thirdRead := errors.New("a round read its snapshot a third time")
	for _, fresh := range []statelog.Freshness{stale, linearizable} {
		for _, purged := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/purged=%t", fresh.Level, purged), func(t *testing.T) {
				t.Parallel()
				w := newSyncWorld(t, onMemory)
				repo := w.repo(t, 1)
				_, row := w.push(t, repo, "refs/heads/main", nil)
				if err := w.store.Delete(t.Context(), row.Object); err != nil {
					t.Fatal(err)
				}
				var reads atomic.Int32
				w.log.set(func(f *fakeLog) {
					f.hook = func(fakeCall) error {
						switch reads.Add(1) {
						case 2:
							if purged {
								// Between the round's read and the one it asks
								// the log's head after the store's answer.
								w.log.purge(repo)
							}
						case 3:
							return thirdRead
						}
						return nil
					}
				})
				c := w.open(t)
				_, err := c.Sync(t.Context(), repo, fresh)
				var levels []string
				for _, call := range w.log.callsOf(repo) {
					levels = append(levels, string(call.Level))
				}
				if want := fmt.Sprintf("[%s linearizable]", fresh.Level); fmt.Sprint(levels) != want {
					t.Errorf("the round read at %v, want %s: its own read, then the log's head once", levels, want)
				}
				if purged {
					if !errors.Is(err, code.ErrRepoNotFound) {
						t.Errorf("Sync: %v, want code.ErrRepoNotFound", err)
					}
					// The pack was never missing: the read that named it
					// predates a purge, and nothing is logged against the
					// store.
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
}

// A PACK RETIRED UNDER A ROUND IS ASKED ABOUT AGAIN: even a linearizable
// round's barrier is taken before it downloads what its snapshot names, and a
// row can leave the log while it downloads — a compaction retires the packs it
// merged into one, and once their grace has passed (ADR-0074) the collector
// deletes their objects. So the store's not-found is asked of the log's head
// once more, and a head that no longer names the pack converges the round to
// what the log holds now: the merged pack, which holds every object the
// retired ones did. Failing the round instead failed every caller of it for a
// pack no row names, and logged code_pack_missing — the line the
// objects_missing alarm stands behind — for a pack nobody lost.
//
// Both shapes of round: a cold one reading the rows whole, and a warm one's
// delta from the position it holds. The log here retires the rows directly,
// as a compaction's record applies them: the inputs leave the live rows, and
// the merged pack is added at the record's own position.
//
// Mutation: ask the head again only from a round below linearizable (the
// round fails ErrPackMissing naming a retired pack).
func TestAPackRetiredUnderARoundIsAskedAboutAgain(t *testing.T) {
	t.Parallel()
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			kept, _ := w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t)
			if warm {
				w.sync(t, c, repo).Release()
			}
			_, first := w.push(t, repo, "refs/heads/main", nil)
			tip, second := w.push(t, repo, "refs/heads/main", nil)
			// The merge of the two, stored before its record as every pack is:
			// one pack of every object either of them holds.
			bare := gittest.Empty(t)
			merged := w.storeBundle(t, gittest.Bundle(t, bare,
				gittest.Pack(t, w.client, bare, w.repos[repo].work, tip, "^"+kept)), "")
			var retired sync.Once
			w.log.set(func(f *fakeLog) {
				// Once the round has its answer, and before it downloads: the
				// compaction applies, and its inputs' objects go, as the
				// collector deletes them once their grace has passed.
				f.after = func(fakeCall, *code.RepoSnapshot) {
					retired.Do(func() {
						w.log.set(func(f *fakeLog) {
							r := f.repos[repo]
							r.packs = slices.DeleteFunc(r.packs, func(p code.PackRow) bool {
								return p.PackID == first.PackID || p.PackID == second.PackID
							})
							f.seq++
							row := merged
							row.Added = f.at()
							r.packs = append(r.packs, row)
						})
						for _, gone := range []code.PackRow{first, second} {
							if err := w.store.Delete(t.Context(), gone.Object); err != nil {
								t.Errorf("collecting the retired pack %s: %v", gone.PackID, err)
							}
						}
					})
				}
			})
			pages := len(w.log.firstPages(repo))
			s, err := c.Sync(t.Context(), repo, linearizable)
			if err != nil {
				t.Fatalf("a linearizable round whose packs were retired and collected under it = %v, "+
					"want it converged on the merged pack", err)
			}
			defer s.Release()
			var levels []string
			for _, call := range w.log.firstPages(repo)[pages:] {
				levels = append(levels, string(call.Level))
			}
			if fmt.Sprint(levels) != "[linearizable linearizable]" {
				t.Errorf("the round read its snapshot at %v, want at the log's head, and there once more after the "+
					"store's not-found", levels)
			}
			if want := map[bool]cache.SyncKind{false: cache.SyncFull, true: cache.SyncDelta}[warm]; s.Kind != want {
				t.Errorf("Kind = %s, want %s", s.Kind, want)
			}
			if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
				t.Errorf("the converged image:\n%s\nwant a fresh cache's of the rows the log holds now:\n%s", got, want)
			}
			if _, err := runGit(t, w.pool, s.Dir, "", "cat-file", "-e", tip); err != nil {
				t.Errorf("the converged copy does not hold the tip the retired packs carried: %v", err)
			}
			if got := w.logs.lines(t, "code_pack_missing"); len(got) != 0 {
				t.Errorf("logged %d code_pack_missing lines for packs the log's head no longer names", len(got))
			}
			w.tmpEmpty(t)
		})
	}
}

// A LOST PACK ENDS ITS ROUND AT THE HEAD'S ANSWER. The read made after the
// store's not-found decides the pack: one that still names it names an object
// the store lost while a live row named it, and the round fails there, having
// fetched nothing twice. Applied again instead, it staged and fetched again
// every pack the not-found had cancelled in flight — up to a share of partial
// downloads, the largest first — and asked the store once more for the pack it
// had just been told was not there, on every round of a repository with a
// lost pack, holding its write lock.
//
// Two packs: the larger's GET held in flight, the smaller's object deleted
// while its row is live. A third read is failed, so a round that kept asking
// fails the case rather than hangs it.
//
// Mutation: apply the head's answer whatever it names (each object is opened
// twice).
func TestALostPackEndsItsRoundAtTheHeadsAnswer(t *testing.T) {
	t.Parallel()
	thirdRead := errors.New("a round read its snapshot a third time")
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, large := w.push(t, repo, "refs/heads/main", map[string]string{"big.bin": noise(1, 256<<10)})
	_, lost := w.push(t, repo, "refs/heads/main", nil)
	if lost.Size >= large.Size {
		t.Fatalf("the lost pack is %d bytes and the held one %d: the held one must be fetched first", lost.Size,
			large.Size)
	}
	if err := w.store.Delete(t.Context(), lost.Object); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	w.log.set(func(f *fakeLog) {
		f.hook = func(fakeCall) error {
			if reads.Add(1) == 3 {
				return thirdRead
			}
			return nil
		}
	})
	c := w.open(t)
	// Held until the round's own failure cancels it: the larger is fetched
	// first, so it is in flight when the smaller's not-found arrives.
	held := make(chan struct{})
	t.Cleanup(func() { close(held) }) // before the cache's Stop, which waits for the round
	w.objects.hold = func(k objstore.Key) <-chan struct{} {
		if k == large.Object {
			return held
		}
		return nil
	}
	_, err := c.Sync(t.Context(), repo, linearizable)
	var missing *cache.ErrPackMissing
	if !errors.As(err, &missing) || missing.Pack.PackID != lost.PackID || missing.Damaged ||
		!errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Sync: %v, want *ErrPackMissing naming %s", err, lost.PackID)
	}
	var levels []string
	for _, call := range w.log.callsOf(repo) {
		levels = append(levels, string(call.Level))
	}
	if fmt.Sprint(levels) != "[linearizable linearizable]" {
		t.Errorf("the round read at %v, want its own read, then the log's head once", levels)
	}
	opens, _, _ := w.objects.counts()
	if opens[lost.Object] != 1 {
		t.Errorf("the store was asked for the lost pack %d times, want once: the head's answer decides it",
			opens[lost.Object])
	}
	if opens[large.Object] != 1 {
		t.Errorf("the pack in flight at the not-found was fetched %d times, want once: nothing is installed past "+
			"the answer that makes the round fail", opens[large.Object])
	}
	if got := w.logs.lines(t, "code_pack_missing"); len(got) != 1 {
		t.Errorf("logged %d code_pack_missing lines, want the one the head's answer made final", len(got))
	}
	w.tmpEmpty(t)
}

// EVERY PACK THE STORE LACKS IS ASKED OF THE HEAD AFTER ITS OWN NOT-FOUND.
// The head's answer after one pack's not-found says nothing of a pack the
// store fails afterwards: that pack's row may leave the log after the answer
// and its object go with it — a compaction retiring it, the collector deleting
// it — exactly as the first one's did. So each not-found is decided by a read
// made after it. Decided by the first one's read, the second failed the round
// naming a pack no row names, and logged code_pack_missing for it: what asking
// the head again is there to stop.
//
// The retirements come one after the other: the round's first answer names
// three pushes, two of which are merged and collected once that answer is
// made; the head's answer after their not-found names that merge and the
// third push, which are merged in turn and collected once THAT answer is made.
// The third push's GET is held while the round installs from its first answer,
// so the round meets it from its second however its downloads interleave. The
// round converges on the second merge to a fresh cache's image, cold (a full
// read) and warm (a delta).
//
// Mutation: decide every not-found after the first by the first one's read
// (the round fails ErrPackMissing naming the first merge or the third push).
func TestEveryPackTheStoreLacksIsAskedOfTheHeadAfterItsOwnNotFound(t *testing.T) {
	t.Parallel()
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			kept, _ := w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t)
			if warm {
				w.sync(t, c, repo).Release()
			}
			_, first := w.push(t, repo, "refs/heads/main", nil)
			two, second := w.push(t, repo, "refs/heads/main", nil)
			tip, third := w.push(t, repo, "refs/heads/main", nil)
			// Each merge stored before its record, as every pack is: one pack
			// of every object the packs it replaces hold.
			merged := func(to string) code.PackRow {
				bare := gittest.Empty(t)
				return w.storeBundle(t, gittest.Bundle(t, bare,
					gittest.Pack(t, w.client, bare, w.repos[repo].work, to, "^"+kept)), "")
			}
			early, late := merged(two), merged(tip)
			// retire applies a compaction of gone into into, and collects gone's
			// objects as the collector does once their grace has passed.
			retire := func(into code.PackRow, gone ...code.PackRow) {
				w.log.set(func(f *fakeLog) {
					r := f.repos[repo]
					r.packs = slices.DeleteFunc(r.packs, func(p code.PackRow) bool {
						return slices.ContainsFunc(gone, func(g code.PackRow) bool { return g.PackID == p.PackID })
					})
					f.seq++
					row := into
					row.Added = f.at()
					r.packs = append(r.packs, row)
				})
				for _, g := range gone {
					if err := w.store.Delete(t.Context(), g.Object); err != nil {
						t.Errorf("collecting the retired pack %s: %v", g.PackID, err)
					}
				}
			}
			var answers atomic.Int32
			w.log.set(func(f *fakeLog) {
				f.after = func(fakeCall, *code.RepoSnapshot) {
					switch answers.Add(1) {
					case 1:
						retire(early, first, second)
					case 2:
						retire(late, early, third)
					}
				}
			})
			held := make(chan struct{})
			t.Cleanup(func() { close(held) }) // before the cache's Stop, which waits for the round
			w.objects.hold = func(k objstore.Key) <-chan struct{} {
				if k == third.Object && answers.Load() == 1 {
					return held
				}
				return nil
			}
			pages := len(w.log.firstPages(repo))
			s, err := c.Sync(t.Context(), repo, linearizable)
			if err != nil {
				t.Fatalf("a linearizable round whose packs were retired and collected under each of its reads = "+
					"%v, want it converged on the last merge", err)
			}
			defer s.Release()
			var levels []string
			for _, call := range w.log.firstPages(repo)[pages:] {
				levels = append(levels, string(call.Level))
			}
			if fmt.Sprint(levels) != "[linearizable linearizable linearizable]" {
				t.Errorf("the round read its snapshot at %v, want at the log's head, and there once more after each "+
					"answer's not-found", levels)
			}
			if want := map[bool]cache.SyncKind{false: cache.SyncFull, true: cache.SyncDelta}[warm]; s.Kind != want {
				t.Errorf("Kind = %s, want %s", s.Kind, want)
			}
			if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
				t.Errorf("the converged image:\n%s\nwant a fresh cache's of the rows the log holds now:\n%s", got, want)
			}
			if _, err := runGit(t, w.pool, s.Dir, "", "cat-file", "-e", tip); err != nil {
				t.Errorf("the converged copy does not hold the tip the retired packs carried: %v", err)
			}
			if got := w.logs.lines(t, "code_pack_missing"); len(got) != 0 {
				t.Errorf("logged %d code_pack_missing lines for packs the log's head no longer names", len(got))
			}
			w.tmpEmpty(t)
		})
	}
}

// A FORCED ROUND THAT MEETS A LOST PACK HASHES ITS INDEX FILES ONCE. A
// request's repair in place reads the copy whole at the log's head and holds
// every index file to its checksum, vouched or not — about a quarter of the
// copy's pack bytes, holding every reader of the repository. A pack the rows
// name that the store has lost is decided by the head's answer after its
// not-found, and the round ends there; applied again, it hashed every file a
// second time for the same not-found.
//
// A third read is failed, so a round that kept asking fails the case rather
// than hangs it.
//
// Mutation: apply the head's answer whatever it names (every index file is
// hashed twice).
func TestAForcedRoundThatMeetsALostPackHashesOnce(t *testing.T) {
	t.Parallel()
	thirdRead := errors.New("a round read its snapshot a third time")
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	var counted hashes
	c := w.open(t, counted.on)
	s := w.sync(t, c, h.repo)
	_, lost := w.push(t, h.repo, "refs/heads/main", nil)
	if err := w.store.Delete(t.Context(), lost.Object); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if call.Page == 1 && reads.Add(1) == 3 {
				return thirdRead
			}
			return nil
		}
	})
	before := counted.now()
	_, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
	var missing *cache.ErrPackMissing
	if !errors.As(err, &missing) || missing.Pack.PackID != lost.PackID || missing.Damaged {
		t.Fatalf("Rebuild: %v, want the forced round's *ErrPackMissing naming %s", err, lost.PackID)
	}
	hashed := counted.now()
	if maps.Equal(hashed, before) {
		t.Fatal("the forced round hashed no file: the case tests nothing")
	}
	for name, n := range hashed {
		if got := n - before[name]; got != 1 {
			t.Errorf("the forced round hashed %s %d times, want once", name, got)
		}
	}
}

// hashes counts every file a cache's checks and scrubs hash, by its name
// (cache.WhileHashing).
type hashes struct {
	mu     sync.Mutex
	counts map[string]int
}

// on makes the cache Open builds count its hashes into h.
func (h *hashes) on(o *cache.Options) {
	cache.WhileHashing(o, func(path string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.counts == nil {
			h.counts = map[string]int{}
		}
		h.counts[filepath.Base(path)]++
	})
}

// now is every file hashed so far, with how many times.
func (h *hashes) now() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.counts)
}

// errPushRefused is the object store refusing a push's bundle (refusePush).
var errPushRefused = errors.New("the object store refuses the push's bundle")

// refusePush pushes once more to h's repository and has the object store
// refuse that push's bundle (errPushRefused) — so a round that adds it fails
// at its install — and hold every GET of h's newer pack until the round that
// asked it has failed, so no round installs that pack again first. The
// answer lets both go, as the case's cleanup does before the cache's Stop,
// which waits for the round.
func (w *syncWorld) refusePush(t *testing.T, h history) (release func()) {
	t.Helper()
	_, push := w.push(t, h.repo, "refs/heads/main", nil)
	held := make(chan struct{})
	release = sync.OnceFunc(func() {
		w.objects.mu.Lock()
		w.objects.fail, w.objects.hold = nil, nil
		w.objects.mu.Unlock()
		close(held)
	})
	t.Cleanup(release)
	w.objects.mu.Lock()
	defer w.objects.mu.Unlock()
	w.objects.fail = func(k objstore.Key) error {
		if k == push.Object {
			return errPushRefused
		}
		return nil
	}
	w.objects.hold = func(k objstore.Key) <-chan struct{} {
		if k == h.newer.Object {
			return held
		}
		return nil
	}
	return release
}

// A FORCED ROUND THAT FAILS AT ITS INSTALL HAS SPENT ITS FORCES. A request's
// repair in place holds every index file to its checksum, vouched or not, and
// a round that then fails at its install — on a pack it ADDS, which the object
// store will not hand over — has done all its forces asked: what its check
// found waits on the repair mark for the rounds after it. Settled only once an
// install had acted, the forces made every sync of the copy after it a forced
// round of its own — every index file hashed again, about a quarter of the
// copy's pack bytes, under the repository's write lock — for as long as the
// store kept failing. And it is the FINDING the round leaves, not the forces:
// an index damaged in place keeps its identity and the vouch an earlier check
// gave it, which no check but a forced one reads past, so the round that can
// install the pack at last knows of it only from the mark.
//
// The newer pack's index damaged in place, and a push the store refuses; the
// newer pack's GET held until each round has failed, so no round installs it
// again before the store answers (refusePush). The request's forced round
// hashes each index file once and fails with the store's refusal; three syncs
// while the store still refuses hash nothing; and once it answers, a sync
// installs the damaged pack again from its finding, beside the push.
//
// Mutations: settle the forces only once an install has acted (each sync
// hashes every index file again, forced); spend them and carry no finding (the
// damaged index is never installed again).
func TestAForcedRoundThatFailsAtItsInstallHasSpentItsForces(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	var counted hashes
	c := w.open(t, counted.on)
	s := w.sync(t, c, h.repo)
	gittest.DamageInPlace(t, s.Dir, gittest.IdxTrailer, h.commits[3])
	release := w.refusePush(t, h)
	forced := func() uint64 {
		return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"})
	}
	before := counted.now()

	_, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
	if !errors.Is(err, errPushRefused) {
		t.Fatalf("Rebuild: %v, want its forced round failing on the store's refusal of the push", err)
	}
	spent := counted.now()
	for name, n := range spent {
		if n-before[name] != 1 {
			t.Errorf("the forced round hashed %s %d times, want once", name, n-before[name])
		}
	}
	idx := filepath.Join("objects", "pack", "pack-"+h.newer.GitChecksum+".idx")
	if got := cache.Findings(c, h.repo); !slices.Equal(got, []string{idx}) {
		t.Errorf("the repair mark holds %v once the forced round failed, want its check's finding %s", got, idx)
	}
	forcedChecks := forced()
	if forcedChecks == 0 {
		t.Fatal("the request ran no forced round: the case tests nothing")
	}

	for i := range 3 {
		if _, err := c.Sync(t.Context(), h.repo, linearizable); !errors.Is(err, errPushRefused) {
			t.Fatalf("sync %d while the store refuses the push: %v, want its refusal", i, err)
		}
	}
	if got := forced() - forcedChecks; got != 0 {
		t.Errorf("three syncs after the forced round held %d index files to their trailers in forced rounds, "+
			"want none: the forced round spent its forces", got)
	}
	if got := counted.now(); !maps.Equal(got, spent) {
		t.Errorf("three syncs after the forced round hashed files to %v, want none past the forced round's %v",
			got, spent)
	}

	release()
	r, err := c.Sync(t.Context(), h.repo, linearizable)
	if err != nil {
		t.Fatalf("the sync once the store answers: %v", err)
	}
	defer r.Release()
	lines := w.logs.lines(t, "code_cache_pack_reinstalled")
	if len(lines) != 1 || lines[0]["pack_id"] != h.newer.PackID || lines[0]["found"] != "trailer" ||
		lines[0]["trigger"] != "forced" {
		t.Errorf("code_cache_pack_reinstalled = %v, want the damaged pack installed again as the forced check "+
			"found it", lines)
	}
	if bad, err := cache.Unsound(r.Dir); err != nil || len(bad) != 0 {
		t.Errorf("once the store answers, %v fail their trailers (%v): the forced check's finding was never "+
			"acted on", bad, err)
	}
	if got := cache.Findings(c, h.repo); len(got) != 0 {
		t.Errorf("the repair mark still holds %v once its pack is installed again", got)
	}
	if got := counted.now(); !maps.Equal(got, spent) {
		t.Errorf("the sync once the store answers hashed files to %v, want none past the forced round's %v", got,
			spent)
	}
	if _, err := runGit(t, w.pool, r.Dir, "", "rev-list", "--objects", h.commits[3]); err != nil {
		t.Errorf("the repaired copy still fails: %v", err)
	}
	w.tmpEmpty(t)
}

// A FORCED ROUND WHOSE PACKS ARE RETIRED UNDER IT HASHES ONCE. The store's
// not-found for a pack a round adds is decided by the log's head, read after
// it (THE READ AFTER A NOT-FOUND DECIDES IT), and a head that no longer names
// the pack — retired, its object collected, while the round downloaded — sends
// the round on to apply what the head holds: a second apply of ONE round. A
// forced round's check spent its forces in the first, so the second holds no
// index file to its trailer again: the round hashes each once, where it hashed
// every one twice. And the pack the copy lost — what the request's repair is
// for — is put back by that second apply as the repair it is, so the request
// is answered from the repaired copy, never escalated to the scrub.
//
// The copy lost its first push; two pushes it has not read are merged and
// retired, their objects collected, once the round has its first answer; the
// lost pack's GET is held until that answer's apply has failed, so the second
// apply is what puts it back. A third read is failed, so a round that kept
// asking fails the case rather than hangs it.
//
// Mutations: settle the forces only once an install has acted (the second
// apply holds every index file to its trailer again); a lost pack counted only
// by a forced round (the second apply's is no repair, and the request is
// escalated to the scrub and a whole rebuild).
func TestAForcedRoundWhosePacksAreRetiredUnderItHashesOnce(t *testing.T) {
	t.Parallel()
	thirdRead := errors.New("a round read its snapshot a third time")
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	lostTip, lost := w.push(t, repo, "refs/heads/main", nil)
	kept, _ := w.push(t, repo, "refs/heads/main", nil)
	var counted hashes
	c := w.open(t, counted.on)
	s := w.sync(t, c, repo)
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		if err := os.Remove(filepath.Join(s.Dir, "objects", "pack", "pack-"+lost.GitChecksum+ext)); err != nil {
			t.Fatal(err)
		}
	}
	_, first := w.push(t, repo, "refs/heads/main", nil)
	tip, second := w.push(t, repo, "refs/heads/main", nil)
	bare := gittest.Empty(t)
	merged := w.storeBundle(t, gittest.Bundle(t, bare,
		gittest.Pack(t, w.client, bare, w.repos[repo].work, tip, "^"+kept)), "")
	var reads, answers atomic.Int32
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if call.Page == 1 && reads.Add(1) == 3 {
				return thirdRead
			}
			return nil
		}
		// Once the round has its first answer, and before it downloads: the
		// compaction applies, and its inputs' objects go.
		f.after = func(call fakeCall, _ *code.RepoSnapshot) {
			if call.Page != 1 || answers.Add(1) != 1 {
				return
			}
			w.log.set(func(f *fakeLog) {
				r := f.repos[repo]
				r.packs = slices.DeleteFunc(r.packs, func(p code.PackRow) bool {
					return p.PackID == first.PackID || p.PackID == second.PackID
				})
				f.seq++
				row := merged
				row.Added = f.at()
				r.packs = append(r.packs, row)
			})
			for _, gone := range []code.PackRow{first, second} {
				if err := w.store.Delete(t.Context(), gone.Object); err != nil {
					t.Errorf("collecting the retired pack %s: %v", gone.PackID, err)
				}
			}
		}
	})
	held := make(chan struct{})
	t.Cleanup(func() { close(held) }) // before the cache's Stop, which waits for the round
	w.objects.mu.Lock()
	w.objects.hold = func(k objstore.Key) <-chan struct{} {
		if k == lost.Object && answers.Load() == 1 {
			return held
		}
		return nil
	}
	w.objects.mu.Unlock()
	before := counted.now()
	pages := len(w.log.firstPages(repo))

	r, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
	if err != nil {
		t.Fatalf("Rebuild: %v, want the forced round converged on the merged pack with the lost one put back",
			err)
	}
	defer r.Release()
	if n := cache.Incarnation(r); n != cache.Incarnation(s) {
		t.Errorf("the repair answered incarnation %d, want %d repaired in place", n, cache.Incarnation(s))
	}
	var levels []string
	for _, call := range w.log.firstPages(repo)[pages:] {
		levels = append(levels, string(call.Level))
	}
	if fmt.Sprint(levels) != "[linearizable linearizable]" {
		t.Errorf("the round read its snapshot at %v, want at the log's head, and there once more after the "+
			"store's not-found", levels)
	}
	hashedNow := counted.now()
	if maps.Equal(hashedNow, before) {
		t.Fatal("the forced round hashed no file: the case tests nothing")
	}
	for name, n := range hashedNow {
		if got := n - before[name]; got != 1 {
			t.Errorf("the round hashed %s %d times, want once", name, got)
		}
	}
	if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"}); got != 0 {
		t.Errorf("the request's repair scrubbed %d files, want none: its round put the lost pack back", got)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildStored)); got != 1 {
		t.Errorf("rebuilds{reason=stored} = %d, want the one lost pack put back", got)
	}
	lines := w.logs.lines(t, "code_cache_pack_reinstalled")
	if len(lines) != 1 || lines[0]["pack_id"] != lost.PackID || lines[0]["found"] != "absent" {
		t.Errorf("code_cache_pack_reinstalled = %v, want the lost pack put back, found=absent", lines)
	}
	for _, oid := range []string{lostTip, tip} {
		if _, err := runGit(t, w.pool, r.Dir, "", "cat-file", "-e", oid); err != nil {
			t.Errorf("the repaired copy does not hold %s: %v", oid, err)
		}
	}
	w.tmpEmpty(t)
}

// A PACK A FULL ANSWER NAMED IS DECIDED BY A FULL READ. The log may answer a
// delta in full — page 1 decides, where the delta would outweigh its full
// answer or reach below its swept tombstones or its swept retirements — and a
// full answer names packs at or below the incarnation's position: one whose
// files the copy lost is installed again from it. No delta from that position
// names such a pack, so the read that decides its not-found is full. Asked as
// a delta, a pack the store had lost as well was taken for a row gone from the
// log, and the round converged the copy at the log's position without a pack
// its rows name: every request over that pack's objects then failing on a copy
// that says it is whole.
//
// The copy holds both pushes; the older one's files are removed from it and
// its object from the store, and the log answers the round's delta in full,
// and the read after it as the delta it asks. A third read is failed, so a
// round that kept asking fails the case rather than hangs it.
//
// Mutation: ask the deciding read as the round asked its own (the round
// converges, its copy lacking the lost pack).
func TestAPackAFullAnswerNamedIsDecidedByAFullRead(t *testing.T) {
	t.Parallel()
	thirdRead := errors.New("a round read its snapshot a third time")
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, lost := w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	dir := w.sync(t, c, repo).Dir
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		if err := os.Remove(filepath.Join(dir, "objects", "pack", "pack-"+lost.GitChecksum+ext)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.store.Delete(t.Context(), lost.Object); err != nil {
		t.Fatal(err)
	}
	var reads, answers atomic.Int32
	w.log.set(func(f *fakeLog) {
		f.hook = func(fakeCall) error {
			if reads.Add(1) == 3 {
				return thirdRead
			}
			return nil
		}
		f.after = func(_ fakeCall, snap *code.RepoSnapshot) {
			if answers.Add(1) != 1 {
				return
			}
			if snap.Full {
				t.Error("the round's own read was answered in full as asked: the case tests nothing")
				return
			}
			whole, err := silent{f}.RepoState(context.Background(), code.StateQuery{RepoID: repo, Fresh: linearizable})
			if err != nil {
				t.Errorf("answering the round's delta in full: %v", err)
				return
			}
			*snap = whole
		}
	})
	pages := len(w.log.firstPages(repo))
	_, err := c.Sync(t.Context(), repo, linearizable)
	var missing *cache.ErrPackMissing
	if !errors.As(err, &missing) || missing.Pack.PackID != lost.PackID || missing.Damaged {
		t.Fatalf("Sync: %v, want *ErrPackMissing naming the pack the copy and the store both lost", err)
	}
	if calls := w.log.firstPages(repo)[pages:]; len(calls) != 2 || calls[0].Since.IsZero() || !calls[1].Since.IsZero() {
		t.Errorf("the round read %+v, want its own delta, then the log's head in full", calls)
	}
	if got := w.logs.lines(t, "code_pack_missing"); len(got) != 1 {
		t.Errorf("logged %d code_pack_missing lines, want the one the full read made final", len(got))
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
	if len(w.logs.lines(t, "code_pack_checksum_mismatch")) == 0 {
		t.Error("no code_pack_checksum_mismatch line")
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
			if len(w.logs.lines(t, "code_pack_checksum_mismatch")) == 0 {
				t.Error("no code_pack_checksum_mismatch line")
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

// failFrom fails a round's downloads from its n'th on, until disarmed —
// the n'th only once the n-1 packs before it are placed (before, closed by
// the install hook), saying first that it has been reached. So which packs a
// failed round installed is the round's ORDER, never the scheduler's: the
// fault point is the top of a download, before its turn and its GET, so the
// n'th could otherwise fire while the pack before it is still on the wire,
// and its failure end that GET too. Every hit after the n'th fails as well,
// so no later pack is installed whatever runs first.
type failFrom struct {
	point           codefault.Point
	n               int
	before, reached chan struct{}

	mu   sync.Mutex
	hits int
	off  bool
}

var errFailedDownload = errors.New("a download that failed")

func (f *failFrom) Hit(ctx context.Context, p codefault.Point, _ int64) error {
	if p != f.point {
		return nil
	}
	f.mu.Lock()
	f.hits++
	hit, off := f.hits, f.off
	f.mu.Unlock()
	switch {
	case off || hit < f.n:
		return nil
	case hit == f.n:
		close(f.reached)
		select {
		case <-f.before:
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(hangDetector):
			return fmt.Errorf("the %d packs before the failing download were not placed in %v", f.n-1,
				hangDetector)
		}
	}
	return errFailedDownload
}

// disarm lets every later hit through.
func (f *failFrom) disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.off = true
}

// A FAILED ROUND LEAVES NO TEMPORARY FILES — and keeps what it installed:
// the packs before the failure are named by rows, so the next round skips
// them and downloads only the rest. One GET at a time, the round's third
// download failing once the two before it are placed — and the second's GET
// held until the third has reached its failure, the schedule in which a
// failure at the top of the third download ended the second's GET and the
// case read one pack installed where the round's order put two.
func TestAFailedRoundLeavesNoTemporaryFiles(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range 6 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	faults := &failFrom{point: codefault.SyncDownload, n: 3, before: make(chan struct{}),
		reached: make(chan struct{})}
	var opens atomic.Int32
	w.objects.hold = func(objstore.Key) <-chan struct{} {
		if opens.Add(1) == 2 {
			return faults.reached
		}
		return nil
	}
	c := w.open(t, func(o *cache.Options) { o.Faults = faults })
	cache.SetFetchConcurrency(c, 1)
	var placed atomic.Int32
	closeBefore := sync.OnceFunc(func() { close(faults.before) })
	cache.SetInstallHook(c, func(stage, _ string) {
		if stage == "idx" && placed.Add(1) == int32(faults.n-1) {
			closeBefore()
		}
	})
	if _, err := c.Sync(t.Context(), repo, linearizable); !errors.Is(err, errFailedDownload) {
		t.Fatalf("a round whose third download failed answered %v, want that failure", err)
	}
	faults.disarm()
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
