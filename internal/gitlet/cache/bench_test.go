package cache_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/queue"
)

// These benchmarks are the package doc's "Measured" figures; compare a change
// to the boot or the write path by them (make bench, six counts a tree,
// benchstat).

// BenchmarkBoot is a warm Gitlet node's boot of its cache (Prepare, then Open
// and Stop) over repos repositories of packs packs each: 100 and 1 000 packs
// are a repository its compactions keep near gitlet.CompactTargetPacks and one
// falling behind them. A boot reads one small state a repository and stats no
// pack, so the figure follows repos, not packs; states=false has no state
// anywhere, so every incarnation is walked.
//
// The pack files are empty: a boot reads none of their bytes, and millions of
// real packs would measure the disk filling up.
func BenchmarkBoot(b *testing.B) {
	quiet(b)
	gittest.RequireNode(b)
	for _, arm := range []struct {
		repos, packs int
		states       bool
	}{
		{100, 100, true},
		{100, 1000, true},
		{1000, 100, true},
		{1000, 100, false},
	} {
		b.Run(fmt.Sprintf("repos=%d/packs=%d/states=%t", arm.repos, arm.packs, arm.states), func(b *testing.B) {
			dir := filepath.Join(b.TempDir(), "c")
			first, err := cache.Prepare(b.Context(), dir)
			if err != nil {
				b.Fatalf("Prepare: %v", err)
			}
			// Built once over the first Prepare's HOME: a later Prepare moves
			// HOME aside, which a built pool never looks at again.
			pool, err := gitproc.New(b.Context(), gitproc.Options{Home: first.Home, MaxProcesses: 2})
			if err != nil {
				b.Fatalf("gitproc.New: %v", err)
			}
			first.Release()
			bootFixture(b, dir, arm.repos, arm.packs, arm.states)

			for b.Loop() {
				layout, err := cache.Prepare(b.Context(), dir)
				if err != nil {
					b.Fatalf("Prepare: %v", err)
				}
				c, err := cache.Open(b.Context(), cache.Options{
					Layout: layout, MaxBytes: 1 << 40, Git: pool, Objects: noObjects{t: b}, Fetch: noRows{t: b},
					Fleet: soloFleet, Limits: gitlet.DefaultLimits, HookCommand: []string{"/bin/true"}, Now: time.Now,
				})
				if err != nil {
					b.Fatalf("Open: %v", err)
				}
				c.Stop(context.Background())

				// No janitor empties the boot's trash/ here: empty it untimed,
				// so every boot finds what a warm node's does.
				b.StopTimer()
				emptyTrash(b, dir)
				b.StartTimer()
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(arm.repos), "ns/repo")
		})
	}
}

// bootFixture writes repos repositories of packs packs each under dir/repos,
// each with a state of this build unless states is false.
func bootFixture(b *testing.B, dir string, repos, packs int, states bool) {
	b.Helper()
	for r := range repos {
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", r)
		inc := filepath.Join(dir, "repos", id+".1.git")
		pack := filepath.Join(inc, "objects", "pack")
		if err := os.MkdirAll(pack, 0o700); err != nil {
			b.Fatal(err)
		}
		for p := range packs {
			stem := filepath.Join(pack, fmt.Sprintf("pack-%040x", p))
			for _, ext := range []string{".pack", ".rev", ".idx"} {
				f, err := os.Create(stem + ext)
				if err != nil {
					b.Fatal(err)
				}
				if err = f.Close(); err != nil {
					b.Fatal(err)
				}
			}
		}
		write(b, filepath.Join(inc, "packed-refs"), "# pack-refs with: peeled sorted \n")
		if states {
			doc := aState(id)
			doc.Packs = packs
			writeState(b, inc, doc)
		}
	}
}

// emptyTrash removes everything under dir/trash.
func emptyTrash(b *testing.B, dir string) {
	b.Helper()
	list, err := os.ReadDir(filepath.Join(dir, "trash"))
	if err != nil {
		b.Fatal(err)
	}
	for _, d := range list {
		if err = os.RemoveAll(filepath.Join(dir, "trash", d.Name())); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPlace prices the durability rule (durable.go): a step renaming files
// staged, durable files into one directory, with a directory sync after every
// rename (sync=each) against one after the step (sync=once). Each sync is a
// journal commit, which is why the rule syncs a step's directory once.
func BenchmarkPlace(b *testing.B) {
	const files = 300
	for _, each := range []bool{true, false} {
		name := "sync=once"
		if each {
			name = "sync=each"
		}
		b.Run(name, func(b *testing.B) {
			root := b.TempDir()
			staged, dest := filepath.Join(root, "tmp"), filepath.Join(root, "dest")
			for _, d := range []string{staged, dest} {
				if err := os.Mkdir(d, 0o700); err != nil {
					b.Fatal(err)
				}
			}
			round := 0
			for b.Loop() {
				b.StopTimer()
				names := make([]string, files)
				for i := range names {
					names[i] = fmt.Sprintf("r%d-%d", round, i)
					f, err := os.Create(filepath.Join(staged, names[i]))
					if err != nil {
						b.Fatal(err)
					}
					if err = f.Sync(); err != nil {
						b.Fatal(err)
					}
					if err = f.Close(); err != nil {
						b.Fatal(err)
					}
				}
				round++
				b.StartTimer()

				for _, n := range names {
					if err := os.Rename(filepath.Join(staged, n), filepath.Join(dest, n)); err != nil {
						b.Fatal(err)
					}
					if each {
						syncDirectory(b, dest)
					}
				}
				if !each {
					syncDirectory(b, dest)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/files, "ns/file")
		})
	}
}

// BenchmarkBundleFiles prices a cold sync's disk work for a bundle, without its
// GET or hashing: its three files created in tmp/, written at a median push's
// sizes, closed, synced and renamed into one pack directory .idx last
// (placePack's order), by width workers (one, and FetchConcurrency).
// sync=together syncs the three at once, as placePack does; sync=none skips the
// syncs to show what they add. create-us is a bundle's three creates.
func BenchmarkBundleFiles(b *testing.B) {
	const bundles = 400
	sizes := []int{medianPush, 1_200, 100}
	data := make([]byte, medianPush)
	bytes, _ := chacha(drawsFiles, 0)
	_, _ = bytes.Read(data)
	for _, durable := range []bool{true, false} {
		for _, width := range []int{1, cache.FetchConcurrency} {
			name := fmt.Sprintf("sync=none/width=%d", width)
			if durable {
				name = fmt.Sprintf("sync=together/width=%d", width)
			}
			b.Run(name, func(b *testing.B) {
				root := b.TempDir()
				var created atomic.Int64
				round := 0
				for b.Loop() {
					b.StopTimer()
					tmp := filepath.Join(root, "tmp"+strconv.Itoa(round))
					dest := filepath.Join(root, "pack"+strconv.Itoa(round))
					round++
					for _, d := range []string{tmp, dest} {
						if err := os.Mkdir(d, 0o700); err != nil {
							b.Fatal(err)
						}
					}
					work := make(chan int, bundles)
					for i := range bundles {
						work <- i
					}
					close(work)
					b.StartTimer()
					var g errgroup.Group
					for range width {
						g.Go(func() error {
							for i := range work {
								if err := bundleFiles(tmp, dest, i, data, sizes, durable, &created); err != nil {
									return err
								}
							}
							return nil
						})
					}
					if err := g.Wait(); err != nil {
						b.Fatal(err)
					}
					syncDirectory(b, dest)
				}
				n := float64(b.N * bundles)
				b.ReportMetric(n/b.Elapsed().Seconds(), "bundles/s")
				b.ReportMetric(float64(created.Load())/n/1e3, "create-us")
			})
		}
	}
}

// bundleFiles is BenchmarkBundleFiles' work for bundle i, synced only when
// durable; the creates' time is added to created.
func bundleFiles(tmp, dest string, i int, data []byte, sizes []int, durable bool, created *atomic.Int64) error {
	exts := []string{".pack", ".idx", ".rev"}
	began := time.Now()
	files := make([]*os.File, len(exts))
	for k, ext := range exts {
		f, err := os.OpenFile(filepath.Join(tmp, "b"+strconv.Itoa(i)+ext), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		files[k] = f
	}
	//fixedwait:measure the files' creation time, added to a total the benchmark reports and holds to no bound
	created.Add(int64(time.Since(began)))
	for k, f := range files {
		if _, err := f.Write(data[:sizes[k]]); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if durable {
		var g errgroup.Group
		for _, ext := range exts {
			g.Go(func() error {
				f, err := os.Open(filepath.Join(tmp, "b"+strconv.Itoa(i)+ext))
				if err != nil {
					return err
				}
				err = f.Sync()
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}
	for _, ext := range []string{".pack", ".rev", ".idx"} {
		name := "b" + strconv.Itoa(i) + ext
		if err := os.Rename(filepath.Join(tmp, name), filepath.Join(dest, name)); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkAdvertise is a push's advertisement (receive-pack --stateless-rpc
// --advertise-refs) over a repository of refs refs: in=cache in the incarnation
// itself, the floor; in=view in an advertisement view with no configuration,
// where receive-pack also lists its alternate's refs for .have lines it then
// has none of to print; in=view-config with Cache.AdvertiseConfig, as the host
// runs it. All three print the same bytes (checked once an arm), so they differ
// by the alternate's listing alone. refs=100000 is gitlet.MaxRefsPerRepo.
func BenchmarkAdvertise(b *testing.B) {
	quiet(b)
	for _, refs := range []int{10_000, 100_000} {
		w := newSyncWorld(b, onMemory)
		repo := w.repo(b, 1)
		tip, _ := w.push(b, repo, "refs/heads/main", nil)
		many := make(map[string]string, refs)
		for i := range refs - 1 {
			many[fmt.Sprintf("refs/heads/topic/%06d", i)] = tip
		}
		w.log.write(repo, nil, many)
		c := w.open(b)
		s := w.sync(b, c, repo)
		view, err := c.AdvertiseView(b.Context(), s)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(view.Remove)
		want := advertise(b, w.pool, s.Dir, "", nil)
		for _, arm := range []struct {
			in, alternate string
			dir           string
			config        []gitproc.KV
		}{
			{"cache", "", s.Dir, nil},
			{"view", view.Alternate, view.Dir, nil},
			{"view-config", view.Alternate, view.Dir, c.AdvertiseConfig()},
		} {
			b.Run(fmt.Sprintf("refs=%d/in=%s", refs, arm.in), func(b *testing.B) {
				if got := advertise(b, w.pool, arm.dir, arm.alternate, arm.config); got != want {
					b.Fatalf("the advertisement in %s is not the cache's own (%d bytes against %d)",
						arm.in, len(got), len(want))
				}
				for b.Loop() {
					advertise(b, w.pool, arm.dir, arm.alternate, arm.config)
				}
			})
		}
	}
}

// syncDirectory fsyncs dir, as the cache's syncDir does.
func syncDirectory(b *testing.B, dir string) {
	f, err := os.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		b.Fatal(err)
	}
	if err = f.Close(); err != nil {
		b.Fatal(err)
	}
}

// quiet keeps a benchmark's output to its figures: benchstat cannot read an
// info line among them.
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// THE BYTE PATH: BenchmarkColdSync (a node's first sync of a repository),
// BenchmarkDeltaSync (every node's sync after every push), BenchmarkMidx
// (indexing a repository's packs) and BenchmarkPackLookup (an object lookup
// across many packs). Every fixture is real git, built untimed: a history
// fast-imported, each push's objects packed alone as receive-pack stores them,
// its row in the fake log. Arms whose fixture takes minutes and gigabytes run
// only under gittest.LargeEnv.

// benchWorkers is how many fixture pushes are packed and stored at once: a pack
// is mostly a git process starting and a put mostly waiting on a round trip, so
// sixteen keep four cores busy.
const benchWorkers = 16

// medianPush is the median push's bytes (master §2.9).
const medianPush = 150_000

// benchIdent and benchEpoch are every imported commit's committer and the first
// one's time, each later commit a second on, so a fixture is the same history
// and packs on every run.
const (
	benchIdent = "Crewlet Bench <bench@example.com>"
	benchEpoch = 1_767_225_600 // 2026-01-01T00:00:00Z
	// nextEpoch is the first nextPush's time: past every history's last commit,
	// so a push lands on top in time too.
	nextEpoch = benchEpoch + 100_000_000
)

// providerFirstByte is the S3 fake's wait before answering each request in
// every BenchmarkColdSync shape: 30 ms, the floor of S3's. Every GET pays it
// whatever its size, which bounds a cold sync by its GET count (sixteen at once
// at about 33 ms a GET allow some 480 bundles a second); a loopback fake
// answering thousands a second could never show that.
const providerFirstByte = 30 * time.Millisecond

// s3FirstBytes are the first bytes BenchmarkColdSync's pack-count shapes also
// run at: 100 and 200 ms, S3's range per AWS's performance guidelines, so a
// cold sync's rate at a real bucket is measured at each end, never computed
// from the 30 ms arm.
var s3FirstBytes = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}

// The pack counts BenchmarkColdSync reads a repository at, each a push of
// medianPush bytes a pack.
const (
	// compactedPacks is a repository its home compacts, held near
	// gitlet.CompactTargetPacks live packs.
	compactedPacks = gitlet.CompactTargetPacks

	// uncompactedPacks is 10 000 pushes NOTHING compacted, one pack a push:
	// five times gitlet.MaxPacksPerRepo, which refuses such pushes until the
	// home merges them. It is the uncompacted baseline beside compactedPacks.
	uncompactedPacks = 10_000
)

// providerConnBytes is the most the S3 fake sends one connection a second: 80
// MB/s, the low end of one S3 connection.
const providerConnBytes = 80_000_000

// commitsOf is ref's commits in src, oldest first.
func commitsOf(b *testing.B, client *gittest.Client, src, ref string) []string {
	b.Helper()
	return strings.Fields(client.Run(b, src, "rev-list", "--reverse", ref))
}

// chacha is a fixture's seeded bytes and draws for the n'th item of a domain:
// the same every run and different for every (domain, n), since a push of bytes
// an earlier one stored would download nothing.
func chacha(domain byte, n uint64) (*rand.ChaCha8, *rand.Rand) {
	var key [32]byte
	key[0] = domain
	binary.LittleEndian.PutUint64(key[1:], n)
	src := rand.NewChaCha8(key)
	return src, rand.New(src)
}

// The domains of chacha's draws, one a kind of fixture.
const (
	drawsPushes byte = iota + 1
	drawsNext
	drawsFiles
	drawsReuse
	// drawsTail is BenchmarkMidx's published fixture's tail of pushes and
	// the ids its chain lookups draw.
	drawsTail
)

// pushStream is a fast-import stream of n commits on main, the i'th adding
// d<i mod 100>/f<i>.bin of size(i) seeded random bytes — incompressible, as
// most of a push's new bytes are — so each push's pack holds one commit, two
// trees of at most a hundred entries and the file.
func pushStream(n int, size func(int) int64) func(*bufio.Writer) error {
	return func(w *bufio.Writer) error {
		bytes, _ := chacha(drawsPushes, 0)
		for i := range n {
			msg := fmt.Sprintf("push %d\n", i)
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter %s %d +0000\ndata %d\n%s", benchIdent, benchEpoch+i, len(msg), msg)
			fmt.Fprintf(w, "M 100644 inline d%02d/f%05d.bin\ndata %d\n", i%100, i, size(i))
			if _, err := io.CopyN(w, bytes, size(i)); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	}
}

// pushRange is one push of a fixture's history: the objects `git rev-list
// --objects tip ^base` names, base "" for the first, and the annotated tags
// it pushes with them.
type pushRange struct {
	base, tip string
	tags      []string
}

// eachPush is every commit of commits pushed alone, oldest first.
func eachPush(commits []string) []pushRange {
	return splitPushes(commits, len(commits))
}

// splitPushes is commits pushed as n pushes of consecutive commits, oldest
// first, as even as n divides them.
func splitPushes(commits []string, n int) []pushRange {
	out := make([]pushRange, 0, n)
	base := ""
	for j := range n {
		tip := commits[(j+1)*len(commits)/n-1]
		out = append(out, pushRange{base: base, tip: tip})
		base = tip
	}
	return out
}

// storePushes packs each push's objects alone out of src, as a receiving node's
// receive-pack stores them, puts each bundle in w's store, and answers every
// push's row in order and their bytes: benchWorkers at once, each pack's files
// removed once stored, so the disk holds one pack a worker.
func (w *syncWorld) storePushes(b *testing.B, src string, pushes []pushRange) ([]gitlet.PackRow, int64) {
	b.Helper()
	scratch := b.TempDir()
	rows := make([]gitlet.PackRow, len(pushes))
	g, ctx := errgroup.WithContext(b.Context())
	g.SetLimit(benchWorkers)
	for i, p := range pushes {
		g.Go(func() error {
			dir := filepath.Join(scratch, strconv.Itoa(i))
			if err := os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o700); err != nil {
				return err
			}
			revs := append([]string{p.tip}, p.tags...)
			if p.base != "" {
				revs = append(revs, "^"+p.base)
			}
			cs, err := gittest.TryPack(ctx, w.client, dir, src, revs...)
			if err != nil {
				return err
			}
			bundle, err := gittest.ReadBundle(dir, cs)
			if err != nil {
				return err
			}
			if err = os.RemoveAll(dir); err != nil {
				return err
			}
			rows[i], err = w.putBundle(ctx, bundle, "")
			return err
		})
	}
	if err := g.Wait(); err != nil {
		b.Fatalf("storing the fixture's pushes: %v", err)
	}
	var total int64
	for _, r := range rows {
		total += r.Size
	}
	return rows, total
}

// logRepo makes the n'th benchmark repository in w's log, with no refs and
// no packs.
func (w *syncWorld) logRepo(n int) string {
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("bench-repo-"+strconv.Itoa(n))).String()
	w.log.create(id, "bench-"+strconv.Itoa(n))
	return id
}

// writePushes writes each push to repo in w's log as the record that landed it:
// its pack's row, main moved to its tip, and the tip as a commit it introduced,
// so every delta's fold builds commit records as a real push's does.
func (w *syncWorld) writePushes(repo string, pushes []pushRange, rows []gitlet.PackRow) {
	for i, p := range pushes {
		w.log.writePushes(repo, fakePush{packs: []gitlet.PackRow{rows[i]},
			refs: map[string]string{"refs/heads/main": p.tip}, introduced: []string{p.tip}})
	}
}

// coldCache is a node that has never served the repository: bootCache over a
// directory of its own under root. drop stops it and removes the directory, so
// a loop of cold syncs holds one cache on disk at a time.
func (w *syncWorld) coldCache(b *testing.B, root string, n, fetch int, mutate ...func(*cache.Options)) (
	_ *cache.Cache, drop func()) {

	b.Helper()
	dir := filepath.Join(root, strconv.Itoa(n))
	c, stop := w.bootCache(b, dir, fetch, mutate...)
	return c, func() {
		stop()
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
	}
}

// keptCache is a node RESTARTED OVER THE CACHE IT KEPT, in a directory of its
// own under root: a first boot syncs every repository of ids (linearizable, all
// at once, full reads uncapped) and stops; a second boots over the same
// directory with mutate applied, so each repository's first round reads its
// rows whole and has nothing to download (holding cache.Options.Objects to that
// is the caller's). drop stops it and removes the directory.
func (w *syncWorld) keptCache(b *testing.B, root string, n int, ids []string, mutate ...func(*cache.Options)) (
	_ *cache.Cache, drop func()) {

	b.Helper()
	dir := filepath.Join(root, strconv.Itoa(n))
	first, stop := w.bootCache(b, dir, cache.FetchConcurrency)
	cache.SetMaxFullRounds(first, int64(len(ids)))
	var g errgroup.Group
	for _, id := range ids {
		g.Go(func() error {
			s, err := first.Sync(b.Context(), id, linearizable)
			if err != nil {
				return err
			}
			s.Release()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		b.Fatalf("the kept cache's first boot: %v", err)
	}
	stop()
	c, stopAgain := w.bootCache(b, dir, cache.FetchConcurrency, mutate...)
	return c, func() {
		stopAgain()
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
	}
}

// bootCache is a node's boot over dir as it finds it: a cache over w's rows,
// store and recorder (each mutate applied after), its pool a slot a core, a
// round fetching fetch bundles at once. stop is the node going down with its
// cache's volume kept.
func (w *syncWorld) bootCache(b *testing.B, dir string, fetch int, mutate ...func(*cache.Options)) (
	_ *cache.Cache, stop func()) {

	b.Helper()
	layout, err := cache.Prepare(b.Context(), dir)
	if err != nil {
		b.Fatalf("Prepare: %v", err)
	}
	o := cache.Options{
		Layout: layout, MaxBytes: 1 << 40, Git: newPoolOf(b, layout, "", runtime.NumCPU(), 0, 1<<40), Objects: w.store,
		Fetch: silent{w.log}, Fleet: soloFleet, Limits: gitlet.DefaultLimits, HookCommand: []string{"/bin/true"},
		Metrics: w.rec, Now: time.Now,
	}
	for _, m := range mutate {
		m(&o)
	}
	c, err := cache.Open(b.Context(), o)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	cache.SetFetchConcurrency(c, fetch)
	return c, func() {
		c.Stop(context.Background())
		layout.Release()
	}
}

// coldShape is one cold-sync fixture: a history of packs pushes of
// medianPush bytes each, the first of them tail bytes when tail is set.
type coldShape struct {
	packs int
	tail  int64
}

func (s coldShape) size(i int) int64 {
	if i == 0 && s.tail > 0 {
		return s.tail
	}
	return medianPush
}

// coldWorld is a sync world over the bucket a cold-sync arm names, the S3 fake
// answering each request firstByte after it arrives.
func coldWorld(b *testing.B, bucket string, firstByte time.Duration) *syncWorld {
	b.Helper()
	switch bucket {
	case "memobj":
		return newSyncWorld(b, onMemory)
	case "fake":
		srv := s3fake.Start(b, "files")
		srv.Latency, srv.ConnBytesPerSecond = firstByte, providerConnBytes
		w := syncWorldOver(b, openS3Fake(b, srv))
		w.s3 = srv
		return w
	case "real":
		// A PREFIX OF THE ARM'S OWN, cleared when it ends
		// (s3fake.RealBackend).
		return syncWorldOver(b, s3fake.RealBackend(b))
	}
	b.Fatalf("no bucket %q", bucket)
	return nil
}

// coldRepo is a repository of shape's pushes in w's log and store, the bytes
// its bundles hold, its pack rows, one a push, oldest first, and each push's
// commit.
func (w *syncWorld) coldRepo(b *testing.B, shape coldShape) (string, int64, []gitlet.PackRow, []string) {
	b.Helper()
	src := gittest.Empty(b)
	gittest.FastImport(b, w.client, src, pushStream(shape.packs, shape.size))
	commits := commitsOf(b, w.client, src, "main")
	pushes := eachPush(commits)
	rows, total := w.storePushes(b, src, pushes)
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	repo := w.logRepo(1)
	w.writePushes(repo, pushes, rows)
	return repo, total, rows, commits
}

// BenchmarkColdSync is a node's FIRST sync of a repository it never served,
// from linearizable rows to an answer, by a fresh cache fetching fetch bundles
// at once (1; FetchConcurrency; NodeFetches, the whole node). Shapes are
// compactedPacks, 200, gitlet.MaxPacksPerRepo and uncompactedPacks pushes of
// medianPush (the last two under CREWLET_BENCH_LARGE=1, uncompacted at
// FetchConcurrency only), and shape=tail, 200 behind a 512 MiB first push whose
// download only s3obj's ranged reads shorten. bucket=memobj is the node's side
// alone, fake the S3 fake at providerConnBytes and firstbyte, real the
// CREWLET_BENCH_S3_* bucket. dl-s (first GET's open to last GET's close) is
// what gitlet.MaxPacksPerRepo is held to: 27 s, half the spool wait, at 200 ms
// and fetch=16, and ns/op to the whole wait. cpu-ms/pack includes the
// in-process fake's CPU. layers=… is coldLayered; install=catchup is catchUp.
func BenchmarkColdSync(b *testing.B) {
	quiet(b)
	type arm struct {
		bucket    string
		firstByte time.Duration
		shapes    []coldShape
		fetches   []int
	}
	compacted, bound, uncompacted := coldShape{packs: compactedPacks}, coldShape{packs: gitlet.MaxPacksPerRepo},
		coldShape{packs: uncompactedPacks}
	counts := []coldShape{compacted, {packs: 200}, bound, uncompacted}
	every := append(slices.Clone(counts), coldShape{packs: 200, tail: 512 << 20})
	widths := []int{1, cache.FetchConcurrency, cache.NodeFetches}
	arms := []arm{
		{bucket: "memobj", shapes: counts, fetches: widths},
		{bucket: "fake", firstByte: providerFirstByte, shapes: every, fetches: widths},
	}
	for _, firstByte := range s3FirstBytes {
		arms = append(arms, arm{bucket: "fake", firstByte: firstByte, shapes: []coldShape{compacted, bound, uncompacted},
			fetches: []int{cache.FetchConcurrency, cache.NodeFetches}})
	}
	arms = append(arms, arm{bucket: "real", shapes: every, fetches: widths})
	for _, a := range arms {
		for _, shape := range a.shapes {
			name := "bucket=" + a.bucket
			if a.bucket == "fake" {
				name += fmt.Sprintf("/firstbyte=%dms", a.firstByte.Milliseconds())
			}
			name += fmt.Sprintf("/packs=%d", shape.packs)
			if shape.tail > 0 {
				name += "/shape=tail"
			}
			fetches := a.fetches
			if shape == uncompacted {
				fetches = []int{cache.FetchConcurrency}
			}
			b.Run(name, func(b *testing.B) {
				if shape.packs > 200 {
					gittest.Large(b, fmt.Sprintf("%d pushes packed and stored", shape.packs))
				}
				w := coldWorld(b, a.bucket, a.firstByte)
				repo, total, rows, tips := w.coldRepo(b, shape)
				for _, fetch := range fetches {
					b.Run(fmt.Sprintf("fetch=%d", fetch), func(b *testing.B) {
						root := b.TempDir()
						b.SetBytes(total)
						timed := &timedObjects{inner: w.store}
						var cpu, downloads time.Duration
						n := 0
						for b.Loop() {
							b.StopTimer()
							c, drop := w.coldCache(b, root, n, fetch, func(o *cache.Options) { o.Objects = timed })
							n++
							timed.reset()
							began := processCPU(b)
							b.StartTimer()
							s, err := c.Sync(b.Context(), repo, linearizable)
							b.StopTimer()
							cpu += processCPU(b) - began
							downloads += timed.span()
							if err != nil {
								b.Fatalf("a cold sync: %v", err)
							}
							if s.Kind != cache.SyncFull {
								b.Fatalf("a cold sync was a %s round", s.Kind)
							}
							s.Release()
							drop()
							b.StartTimer()
						}
						b.ReportMetric(float64(shape.packs)*float64(b.N)/b.Elapsed().Seconds(), "packs/s")
						b.ReportMetric(float64(timed.took.Load())/float64(timed.gets.Load())/1e6, "get-ms")
						b.ReportMetric(downloads.Seconds()/float64(b.N), "dl-s")
						b.ReportMetric(float64(cpu)/float64(shape.packs*b.N)/1e6, "cpu-ms/pack")
					})
				}
				// THE PUBLISHED ARMS LAST: each publishes a chain on the
				// repository, which every cold sync after it installs.
				if a.bucket != "fake" || !slices.Contains(s3FirstBytes, a.firstByte) ||
					(shape != compacted && shape != bound) {
					return
				}
				b.Run(fmt.Sprintf("layers=chain/fetch=%d", cache.FetchConcurrency), func(b *testing.B) {
					coldLayered(b, w, repo, rows, tips, a.firstByte, 1, cache.FetchConcurrency)
				})
				if shape == compacted {
					b.Run(fmt.Sprintf("layers=chain21/fetch=%d", cache.FetchConcurrency), func(b *testing.B) {
						coldLayered(b, w, repo, rows, tips, a.firstByte, 21, cache.FetchConcurrency)
					})
				}
			})
		}
	}
	for _, firstByte := range s3FirstBytes {
		for _, repos := range []int{1_000, 10_000} {
			b.Run(fmt.Sprintf("install=catchup/firstbyte=%dms/repos=%d", firstByte.Milliseconds(), repos),
				func(b *testing.B) {
					if repos > 1_000 {
						gittest.Large(b, "10 000 repositories synced, each then handed a published layer")
					}
					catchUp(b, firstByte, repos)
				})
		}
	}
}

// timedObjects times every read from open to close (get-ms) and keeps the span
// from the earliest open to the latest close since reset (dl-s).
type timedObjects struct {
	inner      cache.Objects
	took, gets atomic.Int64
	// first and last are the earliest open and the latest close since reset,
	// in Unix nanoseconds, 0 before any.
	first, last atomic.Int64
}

// reset forgets the span: a sync about to begin.
func (o *timedObjects) reset() {
	o.first.Store(0)
	o.last.Store(0)
}

// span is the earliest open since reset to the latest close: 0 where no read
// both opened and closed.
func (o *timedObjects) span() time.Duration {
	first, last := o.first.Load(), o.last.Load()
	if first == 0 || last < first {
		return 0
	}
	return time.Duration(last - first)
}

// extreme moves v to at where at is past it in the direction later says —
// later, or earlier where later is false — v's 0 holding no instant yet.
func extreme(v *atomic.Int64, at int64, later bool) {
	for {
		cur := v.Load()
		if cur != 0 && (cur >= at) == later {
			return
		}
		if v.CompareAndSwap(cur, at) {
			return
		}
	}
}

func (o *timedObjects) Open(ctx context.Context, obj objstore.Object) (io.ReadCloser, error) {
	began := time.Now()
	extreme(&o.first, began.UnixNano(), false)
	rc, err := o.inner.Open(ctx, obj)
	if err != nil {
		return nil, err
	}
	return &timedRead{ReadCloser: rc, done: func() {
		ended := time.Now()
		//fixedwait:measure a GET's duration, added to a total the benchmark reports and holds to no bound
		o.took.Add(int64(ended.Sub(began)))
		o.gets.Add(1)
		extreme(&o.last, ended.UnixNano(), true)
	}}, nil
}

// timedRead is one timed read, counted once when it closes.
type timedRead struct {
	io.ReadCloser
	done func()
}

func (r *timedRead) Close() error {
	err := r.ReadCloser.Close()
	r.done()
	return err
}

// processCPU is this process's user and system CPU time, the in-process S3
// fake's among it.
func processCPU(b *testing.B) time.Duration {
	b.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkColdJoin is nodes BRINGING UP 500 REPOSITORIES AT ONCE through one
// data node (coldLane), full reads capped at rounds (MaxFullRounds), while the
// lane's other traffic arrives every coldOtherEvery: wait-p99-ms and
// wait-max-ms are that traffic's wait, what the cap is for, and up-s the last
// node's time to come up. cache=fresh is four nodes joining with empty caches,
// each round downloading beside its read; rounds=500 is no cap. cache=kept is
// one node restarted over the cache it kept (keptCache): every first round is a
// full read with nothing to download, so the reads are all of up-s. The data
// node is a model (coldPageServe, coldReaders); the cache's own work is real.
func BenchmarkColdJoin(b *testing.B) {
	quiet(b)
	const repos = 500
	w := newSyncWorld(b, onMemory)
	src := w.repo(b, 1)
	tip, row := w.commit(b, src, nil, "")
	ids := make([]string, repos)
	for i := range ids {
		// One object under a pack id of each repository's own, as a push mints
		// one: stored once, installed for every repository.
		ids[i] = w.logRepo(i + 1)
		row.PackID = uuid.Must(uuid.NewV7()).String()
		w.log.write(ids[i], []gitlet.PackRow{row}, map[string]string{"refs/heads/main": tip})
	}
	type event struct {
		nodes  int
		kept   bool
		rounds []int64
	}
	events := []event{
		{nodes: 4, rounds: []int64{4, 8, 16, 32, repos}},
		{nodes: 1, kept: true, rounds: []int64{4, 8, 16, 32, queue.ConcurrentAnswers, repos}},
	}
	root := b.TempDir()
	n := 0
	for _, ev := range events {
		state := "fresh"
		if ev.kept {
			state = "kept"
		}
		for _, rounds := range ev.rounds {
			b.Run(fmt.Sprintf("nodes=%d/repos=%d/cache=%s/rounds=%d", ev.nodes, repos, state, rounds), func(b *testing.B) {
				var (
					waits []time.Duration
					up    time.Duration
				)
				for b.Loop() {
					b.StopTimer()
					lane := newColdLane(silent{w.log})
					caches := make([]*cache.Cache, ev.nodes)
					drops := make([]func(), ev.nodes)
					for i := range caches {
						if ev.kept {
							// A restart downloads nothing: a read of the
							// store fails the arm (noObjects).
							caches[i], drops[i] = w.keptCache(b, root, n, ids, func(o *cache.Options) {
								o.Fetch, o.Objects = lane, noObjects{t: b}
							})
						} else {
							caches[i], drops[i] = w.coldCache(b, root, n, cache.FetchConcurrency,
								func(o *cache.Options) { o.Fetch = lane })
						}
						n++
						cache.SetMaxFullRounds(caches[i], rounds)
					}
					stop := lane.traffic()
					b.StartTimer()
					began := time.Now()
					var g errgroup.Group
					for _, c := range caches {
						for _, id := range ids {
							g.Go(func() error {
								s, err := c.Sync(b.Context(), id, linearizable)
								if err != nil {
									return err
								}
								s.Release()
								if s.Kind != cache.SyncFull {
									return fmt.Errorf("%s's first sync on a node just up (cache=%s) was a %s round, "+
										"want a full one", id, state, s.Kind)
								}
								return nil
							})
						}
					}
					err := g.Wait()
					//fixedwait:measure the nodes' time to come up, the benchmark's reported figure, held to no bound
					up += time.Since(began)
					b.StopTimer()
					waits = append(waits, stop()...)
					if err != nil {
						b.Fatalf("%d nodes coming up (cache=%s): %v", ev.nodes, state, err)
					}
					for _, drop := range drops {
						drop()
					}
					b.StartTimer()
				}
				slices.Sort(waits)
				at := func(q float64) float64 {
					return float64(waits[int(q*float64(len(waits)-1))].Microseconds()) / 1e3
				}
				b.ReportMetric(at(0.99), "wait-p99-ms")
				b.ReportMetric(at(1), "wait-max-ms")
				b.ReportMetric(up.Seconds()/float64(b.N), "up-s")
			})
		}
	}
}

// The data node BenchmarkColdJoin models. coldPageServe is a page's hold on its
// answer and reader: gitlet's BenchmarkServeASnapshotPage measured 59 ms for a
// full page of refs (gitlet.MaxSnapshotItems of a gitlet.MaxRefsPerRepo
// repository), plus a millisecond for the estate's crossing
// (internal/gitlet/gitletcluster's package doc). coldReaderConns is store's
// defaultReaderConns, max(8, GOMAXPROCS), on the smallest data node.
// coldOtherEvery and coldOtherServe are the lane's other traffic: 200 reads a
// second, each a millisecond's crossing and a millisecond's read.
const (
	coldPageServe   = 60 * time.Millisecond
	coldReaderConns = 8
	coldOtherEvery  = 5 * time.Millisecond
	coldOtherServe  = 2 * time.Millisecond
)

// coldLane is ONE DATA NODE in front of the rows: its shared lane's
// queue.ConcurrentAnswers answers, a request past them waiting first come,
// first served (queue.EventQueue.Serve), then inside the answer one of its
// reader connections, since every page is one read transaction
// (statelog.Reader.Read). A page holds both for coldPageServe, so the node
// scans at most coldReaderConns pages at once.
type coldLane struct {
	inner   cache.Fetcher
	answers *semaphore.Weighted
	readers *coldReaders
}

// newColdLane is one data node answering pages from inner.
func newColdLane(inner cache.Fetcher) *coldLane {
	return &coldLane{
		inner: inner, answers: semaphore.NewWeighted(queue.ConcurrentAnswers),
		readers: &coldReaders{free: coldReaderConns},
	}
}

func (l *coldLane) RepoState(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
	if err := l.answers.Acquire(ctx, 1); err != nil {
		return gitlet.RepoSnapshot{}, err
	}
	defer l.answers.Release(1)
	l.readers.take()
	defer l.readers.give()
	//fixedwait:simulated a data node's serve time for one snapshot page (coldPageServe)
	time.Sleep(coldPageServe)
	return l.inner.RepoState(ctx, q)
}

// Read is the backfill's read of push rows (code.read), held to the same lane
// and reader as a page (statelog.Reader.Read) at no cost of its own: the model
// prices only a snapshot's page. A cold join's caches are never started, so no
// backfill reads here.
func (l *coldLane) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	if err := l.answers.Acquire(ctx, 1); err != nil {
		return gitlet.ReadAnswer{}, err
	}
	defer l.answers.Release(1)
	l.readers.take()
	defer l.readers.give()
	return l.inner.Read(ctx, q)
}

// traffic starts the lane's other traffic, a read every coldOtherEvery; stop
// ends it and answers each read's wait for its answer and reader, once all have
// had both.
func (l *coldLane) traffic() (stop func() []time.Duration) {
	var (
		mu    sync.Mutex
		waits []time.Duration
		wg    sync.WaitGroup
	)
	done := make(chan struct{})
	wg.Go(func() {
		//fixedwait:simulated the lane's other traffic, a read every coldOtherEvery
		tick := time.NewTicker(coldOtherEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			wg.Go(func() {
				began := time.Now()
				if err := l.answers.Acquire(context.Background(), 1); err != nil {
					return
				}
				l.readers.take()
				//fixedwait:measure how long the other read waited, which the benchmark reports and holds to no bound
				waited := time.Since(began)
				//fixedwait:simulated the other read's serve time on its reader (coldOtherServe)
				time.Sleep(coldOtherServe)
				l.readers.give()
				l.answers.Release(1)
				mu.Lock()
				waits = append(waits, waited)
				mu.Unlock()
			})
		}
	})
	return func() []time.Duration {
		close(done)
		wg.Wait()
		return waits
	}
}

// coldReaders is the modeled reader connections, handed out as database/sql
// hands out its pool's: a returned connection goes to a waiting read CHOSEN AT
// RANDOM (connRequestSet.TakeRandom), not the longest waiting, so once full
// pages queue, another read's wait is a draw among them with a tail many pages
// long.
type coldReaders struct {
	mu      sync.Mutex
	free    int
	waiting []chan struct{}
}

// take waits for a connection, never giving up: each arm waits for every read.
func (r *coldReaders) take() {
	r.mu.Lock()
	if r.free > 0 {
		r.free--
		r.mu.Unlock()
		return
	}
	turn := make(chan struct{})
	r.waiting = append(r.waiting, turn)
	r.mu.Unlock()
	<-turn
}

// give hands a connection to a waiting read drawn at random, or back to the
// free ones when nobody waits.
func (r *coldReaders) give() {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.waiting)
	if n == 0 {
		r.free++
		return
	}
	i := rand.IntN(n)
	turn := r.waiting[i]
	r.waiting[i] = r.waiting[n-1]
	r.waiting = r.waiting[:n-1]
	close(turn)
}

// timedLog is a Fetcher timing its log's answers, so a benchmark can name the
// fake's share of each operation.
type timedLog struct {
	inner cache.Fetcher
	took  atomic.Int64
}

func (l *timedLog) RepoState(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
	began := time.Now()
	//fixedwait:measure the fake log's own time, reported so the benchmark can name it, held to no bound
	defer func() { l.took.Add(int64(time.Since(began))) }()
	return l.inner.RepoState(ctx, q)
}

func (l *timedLog) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	began := time.Now()
	//fixedwait:measure the fake log's own time, reported so the benchmark can name it, held to no bound
	defer func() { l.took.Add(int64(time.Since(began))) }()
	return l.inner.Read(ctx, q)
}

// nextPush lands one push on repo in w: a commit on top of tip adding the
// n'th file of size seeded random bytes, fast-imported into src on branch,
// packed alone, stored, and written to w's log with main moved to it. It
// answers the commit.
func (w *syncWorld) nextPush(b *testing.B, src, branch, repo, tip string, n int, size int64) string {
	b.Helper()
	gittest.FastImport(b, w.client, src, func(bw *bufio.Writer) error {
		bytes, _ := chacha(drawsNext, uint64(n))
		msg := fmt.Sprintf("next %d\n", n)
		fmt.Fprintf(bw, "commit %s\ncommitter %s %d +0000\ndata %d\n%sfrom %s\n", branch, benchIdent,
			nextEpoch+n, len(msg), msg, tip)
		fmt.Fprintf(bw, "M 100644 inline next/n%06d.bin\ndata %d\n", n, size)
		if _, err := io.CopyN(bw, bytes, size); err != nil {
			return err
		}
		return bw.WriteByte('\n')
	})
	next := strings.TrimSpace(w.client.Run(b, src, "rev-parse", branch))
	pushes := []pushRange{{base: tip, tip: next}}
	rows, _ := w.storePushes(b, src, pushes)
	w.writePushes(repo, pushes, rows)
	return next
}

// BenchmarkDeltaSync is the sync every node runs after every push on a WARM
// cache: one new pack of medianPush and main moved, read as a delta, downloaded
// and renamed in, packed-refs and the state rewritten, over packs packs and
// refs refs. At refs=100000 (gitlet.MaxRefsPerRepo) packed-refs is about 6.6 MB
// written whole each push.
//
// log-ns/op is the fake log's share: it walks every ref for a delta where the
// real read scans one index range, so the cache's own figure is ns/op less it.
func BenchmarkDeltaSync(b *testing.B) {
	quiet(b)
	for _, packs := range []int{100, 10_000} {
		b.Run(fmt.Sprintf("packs=%d", packs), func(b *testing.B) {
			if packs > 100 {
				gittest.Large(b, "10 000 pushes packed and stored, synced twice")
			}
			w := newSyncWorld(b, onMemory)
			src := gittest.Empty(b)
			// Small pushes: the claim under test is that a delta sync does not
			// depend on the pack count.
			gittest.FastImport(b, w.client, src, pushStream(packs, func(int) int64 { return 1 << 10 }))
			commits := commitsOf(b, w.client, src, "main")
			pushes := eachPush(commits)
			rows, _ := w.storePushes(b, src, pushes)
			root := b.TempDir()
			for i, refs := range []int{1_000, 100_000} {
				// Built once; every count of the arm pushes onto them.
				repo := w.logRepo(i + 1)
				w.writePushes(repo, pushes, rows)
				tip := commits[len(commits)-1]
				many := make(map[string]string, refs-1)
				for r := range refs - 1 {
					many[fmt.Sprintf("refs/heads/topic/%06d", r)] = tip
				}
				w.log.write(repo, nil, many)
				log := &timedLog{inner: silent{w.log}}
				c, drop := w.coldCache(b, root, i, cache.FetchConcurrency, func(o *cache.Options) { o.Fetch = log })
				b.Cleanup(drop)
				s, err := c.Sync(b.Context(), repo, linearizable)
				if err != nil || s.Kind != cache.SyncFull {
					b.Fatalf("the warm cache's first round: a %s round, %v", s.Kind, err)
				}
				s.Release()
				branch := "refs/heads/next-" + strconv.Itoa(i)
				n := 0
				b.Run(fmt.Sprintf("refs=%d", refs), func(b *testing.B) {
					log.took.Store(0)
					for b.Loop() {
						b.StopTimer()
						tip = w.nextPush(b, src, branch, repo, tip, n, medianPush)
						n++
						b.StartTimer()
						s, err := c.Sync(b.Context(), repo, linearizable)
						b.StopTimer()
						if err != nil {
							b.Fatalf("a delta sync: %v", err)
						}
						if s.Kind != cache.SyncDelta {
							b.Fatalf("a sync after one push was a %s round", s.Kind)
						}
						s.Release()
						b.StartTimer()
					}
					b.ReportMetric(float64(log.took.Load())/float64(b.N), "log-ns/op")
				})
			}
		})
	}
}

// midxRepo is BenchmarkMidx's repository of about size bytes in w's log and
// store — gittest.History's source repository, one push a session, so
// gittest.HistorySessions packs — with the history it was pushed from, its
// main's commits, its pack rows, and the bytes and objects they hold.
func (w *syncWorld) midxRepo(b *testing.B, size int64) (repo, src string, commits []string, rows []gitlet.PackRow,
	total, objects int64) {

	b.Helper()
	src, commits = gittest.History(b, w.client, size)
	pushes := splitPushes(commits, gittest.HistorySessions)
	rows, total = w.storePushes(b, src, pushes)
	for _, r := range rows {
		objects += r.Objects
	}
	repo = w.logRepo(1)
	w.writePushes(repo, pushes, rows)
	return repo, src, commits, rows, total, objects
}

// staleArms are how many pushes BenchmarkMidx lands outside the maintainer's
// index before it prices a request: none, a few, and on to twice MidxPacks —
// enough to read what each one costs as a slope.
var staleArms = []int{0, 8, 32, 128}

// stalePushBytes is each stale push's one small file, so the clone they make
// dearer pays for the walk the bitmap no longer saves, not for bytes.
const stalePushBytes = 4 << 10

// packOut is pack-objects in dir as upload-pack runs it for a request
// (ServeConfig, the read lane, bytes discarded) over revs: what a clone or a
// fetch is served.
func packOut(b *testing.B, pool *gitproc.Pool, c *cache.Cache, dir string, revs ...string) {
	b.Helper()
	packOutFiltered(b, pool, c, dir, "", revs...)
}

// packOutFiltered is packOut for a request that asked for filter ("" for
// none), which upload-pack hands pack-objects as --filter.
func packOutFiltered(b *testing.B, pool *gitproc.Pool, c *cache.Cache, dir, filter string, revs ...string) {
	b.Helper()
	args := []string{"pack-objects", "--revs", "--stdout", "--delta-base-offset", "-q"}
	if filter != "" {
		args = append(args, "--filter="+filter)
	}
	_, err := pool.Run(b.Context(), gitproc.Cmd{
		Dir:      dir,
		Args:     args,
		Config:   c.ServeConfig(),
		Stdin:    strings.NewReader(strings.Join(revs, "\n") + "\n"),
		Stdout:   io.Discard,
		Class:    gitproc.ClassRead,
		Deadline: time.Hour,
	})
	if err != nil {
		b.Fatalf("packing %v out of %s: %v", revs, dir, err)
	}
}

// BenchmarkMidx is indexing a repository's 64 packs, in OWN mode (the node's
// index) and CHAIN mode (the layers a maintainer publishes,
// layers_bench_test.go). index=round is the full round's index with no bitmap
// (MidxRoundPacks), what a cold request waits for; index=maintainer is one
// maintenance run, index and bitmap then commit-graph, paced by MidxMinInterval
// and MidxDutyCycle. memory/step=… are the memory a clone and a check hold
// (memoryArms); lookup=on|off is the bitmap with and without its lookup table
// (lookupArms); step=…, stale=k/mode=chain, share=… and tail=… are the
// published layers' (layerArms.run); stale=k/mode=own is a count and a clone
// once k pushes landed outside the maintainer's index, the price MidxPacks
// weighs. size=10MB, under the per-object charges' sizes, prices the fixed part
// of step=whole's and step=publish's memory.
func BenchmarkMidx(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name  string
		bytes int64
	}{{"10MB", 10_000_000}, {"100MB", 100_000_000}, {"1GB", 1_000_000_000}, {"5GB", 5_000_000_000}} {
		b.Run("size="+arm.name, func(b *testing.B) {
			if arm.bytes > 1_000_000_000 {
				gittest.Large(b, "a 5 GB history fast-imported, packed, stored and synced")
			}
			w := newSyncWorld(b, onMemory)
			w.processes = runtime.NumCPU()
			repo, src, commits, rows, total, objects := w.midxRepo(b, arm.bytes)
			stale := arm.bytes >= 100_000_000 && arm.bytes <= 1_000_000_000
			clk := newClock()
			c := w.open(b, clk.on, func(o *cache.Options) { o.MaxBytes, o.Logger = 1<<40, nil })
			s := w.sync(b, c, repo)
			defer func() { s.Release() }()
			// The 64 packs are under MidxRoundPacks; the arm prices the round's
			// index whatever count triggers it.
			cache.SetRoundPacks(c, 1)
			clk.Add(cache.MidxDebounce)
			for _, index := range []string{"round", "maintainer"} {
				b.Run("index="+index, func(b *testing.B) {
					b.SetBytes(total)
					for b.Loop() {
						b.StopTimer()
						if err := cache.ForgetMaintenance(c, repo); err != nil {
							b.Fatal(err)
						}
						b.StartTimer()
						switch index {
						case "round":
							if !cache.IndexRound(c, repo, rows) {
								b.Fatal("the round wrote no index")
							}
						case "maintainer":
							if ran := cache.MaintainerPass(c); len(ran) != 1 {
								b.Fatalf("the maintainer ran %v, want the repository", ran)
							}
						}
					}
					b.ReportMetric(float64(objects), "objects")
					b.ReportMetric(float64(len(commits)), "commits")
				})
			}
			if arm.bytes >= 1_000_000_000 {
				w.lookupArms(b, c, s, repo)
			}
			// THE PUBLISHED LAYERS' ARMS, before the own-mode stale arms below
			// replace the synced copy's incarnation: their scratches link its
			// packs.
			newLayerArms(b, w, c, s, src, commits, rows, arm.bytes).run(b)
			if !stale {
				// No memory arms at 5 GB, whose bitmap holds some 10 GB beside
				// the in-memory store's history, nor at 10 MB, which prices
				// only the layers' fixed parts.
				return
			}
			w.memoryArms(b, c, s, repo, objects)
			if err := cache.ForgetMaintenance(c, repo); err != nil {
				b.Fatal(err)
			}
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				b.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			tip, pushed := commits[len(commits)-1], 0
			for _, k := range staleArms {
				for ; pushed < k; pushed++ {
					tip = w.nextPush(b, src, "refs/heads/main", repo, tip, pushed, stalePushBytes)
				}
				s.Release()
				s = w.sync(b, c, repo)
				b.Run(fmt.Sprintf("stale=%d/mode=own/op=count", k), func(b *testing.B) {
					for b.Loop() {
						_, err := w.pool.Run(b.Context(), gitproc.Cmd{
							Dir:    s.Dir,
							Args:   []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "refs/heads/main"},
							Config: c.ServeConfig(), Class: gitproc.ClassRead, Deadline: time.Hour,
						})
						if err != nil {
							b.Fatalf("counting main's objects: %v", err)
						}
					}
				})
				b.Run(fmt.Sprintf("stale=%d/mode=own/op=clone", k), func(b *testing.B) {
					for b.Loop() {
						packOut(b, w.pool, c, s.Dir, "refs/heads/main")
					}
				})
			}
		})
	}
}

// BenchmarkVerify is an operator's check of a held repository
// (Cache.AdmitVerify, then Verification.Run), the figures CheckBudget is set
// from. op=check checks a whole copy: every bundle hashed against its row,
// packed-refs, git's connectivity check. op=repair first damages the largest
// pack untimed, so the check finds it, the rebuild links the rest and downloads
// that pack, and the copy is checked again. op=hash is the hashing alone, what
// VerifyHashPerMiB charges beside the connectivity check's
// gitproc.PackWorkDeadline. size=small is five medianPush pushes; 100MB and 1GB
// are BenchmarkMidx's composition (1GB under CREWLET_BENCH_LARGE=1). ms/MiB is
// the operation over the copy's bytes.
func BenchmarkVerify(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name  string
		bytes int64
	}{{"small", 0}, {"100MB", 100_000_000}, {"1GB", 1_000_000_000}} {
		b.Run("size="+arm.name, func(b *testing.B) {
			if arm.bytes >= 1_000_000_000 {
				gittest.Large(b, "a 1 GB history fast-imported, packed, stored and synced")
			}
			w := newSyncWorld(b, onMemory)
			w.processes = runtime.NumCPU()
			var (
				repo           string
				total, objects int64
			)
			if arm.bytes == 0 {
				repo, total, _, _ = w.coldRepo(b, coldShape{packs: 5})
			} else {
				var src string
				repo, src, _, _, total, objects = w.midxRepo(b, arm.bytes)
				if err := os.RemoveAll(src); err != nil {
					b.Fatal(err)
				}
			}
			// THE CLOCK STANDS STILL, so no maintainer run shares the disk with
			// a check.
			clk := newClock()
			c := w.open(b, clk.on, func(o *cache.Options) { o.MaxBytes, o.Logger = 1<<40, nil })
			s, err := c.Sync(b.Context(), repo, linearizable)
			if err != nil {
				b.Fatalf("syncing the repository: %v", err)
			}
			s.Release()
			for _, op := range []string{"hash", "check", "repair"} {
				b.Run("op="+op, func(b *testing.B) {
					b.SetBytes(total)
					for b.Loop() {
						if op == "hash" {
							if _, err := cache.HashPacks(b.Context(), w.dir(b, c, repo)); err != nil {
								b.Fatalf("hashing the packs: %v", err)
							}
							continue
						}
						if op == "repair" {
							b.StopTimer()
							damageLargestPack(b, w.dir(b, c, repo))
							b.StartTimer()
						}
						v, err := c.AdmitVerify(repo)
						if err != nil {
							b.Fatalf("admitting the check: %v", err)
						}
						report, err := v.Run(b.Context(), cache.VerifyRun{Repair: op == "repair"})
						v.Release()
						switch {
						case err != nil:
							b.Fatalf("a %s: %v", op, err)
						case len(report.Problems) != 0, op == "repair" && !report.Rebuilt:
							b.Fatalf("a %s answered %+v", op, report)
						}
						if op == "repair" {
							// The damaged incarnation's directory is in trash/
							// once its last pin is gone: emptied now, or a
							// gigabyte arm fills the disk a repair at a time.
							b.StopTimer()
							cache.JanitorPass(c)
							b.StartTimer()
						}
					}
					b.ReportMetric(float64(b.Elapsed())/float64(b.N)/1e6/(float64(total)/(1<<20)), "ms/MiB")
					if objects > 0 {
						b.ReportMetric(float64(objects), "objects")
					}
				})
			}
		})
	}
}

// damageLargestPack overwrites eight bytes just before the trailer of the
// largest pack in the incarnation directory dir — its last object's, which
// only the hash of the bundle can find — as a disk going bad would.
func damageLargestPack(b *testing.B, dir string) {
	b.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) == 0 {
		b.Fatalf("the packs in %s: %v (%d)", dir, err, len(packs))
	}
	var largest string
	var size int64
	for _, p := range packs {
		info, err := os.Stat(p)
		if err != nil {
			b.Fatal(err)
		}
		if info.Size() > size {
			largest, size = p, info.Size()
		}
	}
	if err := os.Chmod(largest, 0o600); err != nil {
		b.Fatal(err)
	}
	f, err := os.OpenFile(largest, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, size-28); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
}

// lookupArms are BenchmarkMidx's lookup=off|on arms over repo, synced in s: the
// maintainer's bitmap without its lookup table and with it, and under each a
// blobless clone's lazy fetch of one blob and a fetch of main's newest commit,
// the requests that open the bitmap for a handful of objects.
//
// EACH ARM'S SETUP IS ITS OWN SUB-BENCHMARK'S, so a -bench pattern selecting
// neither skips a maintainer run (minutes at 1 and 5 GB), and the production
// setting is put back after, since every later arm writes its own bitmap.
func (w *syncWorld) lookupArms(b *testing.B, c *cache.Cache, s cache.Synced, repo string) {
	b.Helper()
	defer cache.SetBitmapLookupTable(c, true)
	for _, on := range []bool{false, true} {
		name := "lookup=off"
		if on {
			name = "lookup=on"
		}
		b.Run(name, func(b *testing.B) {
			cache.SetBitmapLookupTable(c, on)
			if err := cache.ForgetMaintenance(c, repo); err != nil {
				b.Fatal(err)
			}
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				b.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			b.Run("op=blob", func(b *testing.B) {
				blob := w.lazyBlob(b, c, s)
				for b.Loop() {
					packOutFiltered(b, w.pool, c, s.Dir, "blob:none", blob)
				}
			})
			b.Run("op=fetch", func(b *testing.B) {
				for b.Loop() {
					packOut(b, w.pool, c, s.Dir, "refs/heads/main", "^refs/heads/main~1")
				}
			})
		})
	}
}

// lazyBlob is the blob BenchmarkMidx's op=blob arms fetch as a blobless
// clone's checkout would: main's first file.
func (w *syncWorld) lazyBlob(b *testing.B, c *cache.Cache, s cache.Synced) string {
	b.Helper()
	res, err := w.pool.Run(b.Context(), gitproc.Cmd{
		Dir: s.Dir, Args: []string{"rev-parse", "--verify", "refs/heads/main:d000/f00000"},
		Config: c.ServeConfig(), Class: gitproc.ClassRead, Deadline: time.Minute,
	})
	if err != nil {
		b.Fatalf("the blob a lazy fetch asks for: %v", err)
	}
	return strings.TrimSpace(string(res.Stdout))
}

// memorySteps are what BenchmarkMidx's memory arms price, in order: each git
// process a cache runs over a whole repository, with what it finds in the
// incarnation. Its writes (the index, its bitmap, the commit-graph) are
// step=whole's (layers_bench_test.go).
var memorySteps = []struct {
	name string
	// args is the process; none is a clone (packOut), run in the read lane as
	// a request's transfer runs; the rest run in the maintenance lane.
	args []string
	// index is the multi-pack-index the incarnation holds when it starts:
	// "plain" one without a bitmap, "bitmap" the maintainer's.
	index string
	// graph: the commit-graph is there when it starts.
	graph bool
}{
	{name: "clone-walk", index: "plain"},
	{name: "clone", index: "bitmap", graph: true},
	{name: "fsck", args: []string{"fsck", "--connectivity-only", "--no-dangling", "--no-progress"}, index: "bitmap",
		graph: true},
}

// memoryArms are BenchmarkMidx's memory/step=… arms over repo, synced in s:
// what a clone and a check of the whole repository hold, the figures
// gitproc.TransferBytesPerObject and FsckBytesPerObject are set from. Each is
// the git process's peak ANONYMOUS memory, sampled every millisecond from /proc
// (gittest.PeakAnon; Linux only, elsewhere only times), over the objects:
// mapped pack pages are not counted, since the kernel reclaims them rather than
// killing anybody. Each step starts from the maintainer's declared writes
// (cache.DerivedFiles). step=clone-walk and step=clone are a clone of main
// served (packOut) with the plain index and with the bitmap; step=fsck is
// Verify's connectivity check.
func (w *syncWorld) memoryArms(b *testing.B, c *cache.Cache, s cache.Synced, repo string, objects int64) {
	b.Helper()
	run := func(b *testing.B, args []string) {
		b.Helper()
		if _, err := w.pool.Run(b.Context(), gitproc.Cmd{
			Dir: s.Dir, Args: args, Config: c.ServeConfig(), Class: gitproc.ClassMaintenance, Deadline: time.Hour,
		}); err != nil {
			b.Fatalf("git %v: %v", args, err)
		}
	}
	// holding puts the incarnation in the state a step starts from.
	holding := func(b *testing.B, index string, graph bool) {
		b.Helper()
		if err := cache.ForgetMaintenance(c, repo); err != nil {
			b.Fatal(err)
		}
		switch index {
		case "plain":
			run(b, declaredArgs(b, cache.StepIndex, cache.TriggerMaintainer))
		case "bitmap":
			run(b, declaredArgs(b, cache.StepBitmap, cache.TriggerMaintainer))
		}
		if graph {
			run(b, declaredArgs(b, cache.StepGraph, cache.TriggerMaintainer))
		}
	}
	for _, step := range memorySteps {
		b.Run("memory/step="+step.name, func(b *testing.B) {
			var peak int64
			measured := false
			for b.Loop() {
				b.StopTimer()
				holding(b, step.index, step.graph)
				b.StartTimer()
				n, ok := gittest.PeakAnon(func() {
					if step.args == nil {
						packOut(b, w.pool, c, s.Dir, "refs/heads/main")
						return
					}
					run(b, step.args)
				})
				peak, measured = max(peak, n), ok
			}
			b.ReportMetric(float64(objects), "objects")
			if measured {
				b.ReportMetric(float64(peak)/float64(objects), "anon-B/object")
				b.ReportMetric(float64(peak)/(1<<20), "anon-MiB")
			}
		})
	}
}

// lookupSizes are the histories BenchmarkPackLookup splits into packs: 100 MB
// (about 300 000 objects, 42 000 commits) at every pack count, and a gigabyte
// at the counts around MidxRoundPacks, so the threshold is measured at two
// sizes rather than assumed.
var lookupSizes = []struct {
	name  string
	bytes int64
	packs []int
}{
	{"100MB", 100_000_000, []int{8, 32, 128, 256, 512, 1_000, 10_000}},
	{"1GB", 1_000_000_000, []int{128, 256, 512}},
}

// lookupFetch is how many of the history's newest commits BenchmarkPackLookup's
// fetch asks for: a push's worth, what a CI fetch after one is served.
const lookupFetch = 4

// BenchmarkPackLookup is what a cache of packs packs pays a request with no
// multi-pack-index, the round's and the maintainer's, and the round's write:
// the figures MidxRoundPacks is set from. packs runs to gitlet.MaxPacksPerRepo
// and uncompactedPacks' 10 000 (with the gigabyte, under
// CREWLET_BENCH_LARGE=1), a size's arms splitting one history. write=round is
// IndexRound over the packs. index=none|round|maintainer serve a cold request,
// those until the maintainer's index lands, and every one after: op=missing is
// Missing of 1 000 commits drawn from every pack (ns/lookup includes the shared
// cat-file's start), op=fetch the newest lookupFetch commits, op=clone main
// whole. layers=… (100MB) is the history under a published chain cut as
// compact's planner keeps one (layerLookupArms), bounded by
// gitlet.MaxPublishedLayers.
func BenchmarkPackLookup(b *testing.B) {
	quiet(b)
	for _, size := range lookupSizes {
		b.Run("size="+size.name, func(b *testing.B) {
			if size.bytes > 100_000_000 {
				gittest.Large(b, "a 1 GB history fast-imported, split into hundreds of packs three times over")
			}
			w := newSyncWorld(b, onMemory)
			src, commits := gittest.History(b, w.client, size.bytes)
			var oids []string
			for i := range 1_000 {
				oids = append(oids, commits[i*len(commits)/1_000])
			}
			have := commits[len(commits)-1-lookupFetch]
			root := b.TempDir()
			for _, packs := range size.packs {
				b.Run(fmt.Sprintf("packs=%d", packs), func(b *testing.B) {
					if packs > 1_000 {
						gittest.Large(b, "10 000 packs packed, stored and synced")
					}
					lookupArm(b, w, root, src, commits, packs, oids, have)
				})
			}
			if size.bytes <= 100_000_000 {
				w.layerLookupArms(b, src, commits)
			}
		})
	}
}

// lookupArm is one of BenchmarkPackLookup's pack counts: the history of
// commits in src split into packs pushes, stored, synced cold, and measured.
func lookupArm(b *testing.B, w *syncWorld, root, src string, commits []string, packs int, oids []string,
	have string) {

	b.Helper()
	pushes := splitPushes(commits, packs)
	rows, _ := w.storePushes(b, src, pushes)
	repo := w.logRepo(packs)
	w.writePushes(repo, pushes, rows)
	var pool *gitproc.Pool
	clk := newClock()
	c, drop := w.coldCache(b, root, packs, cache.FetchConcurrency, func(o *cache.Options) {
		pool, o.Now = o.Git, clk.Now
	})
	b.Cleanup(drop)
	s, err := c.Sync(b.Context(), repo, linearizable)
	if err != nil {
		b.Fatalf("syncing %d packs: %v", packs, err)
	}
	b.Cleanup(s.Release)
	cache.SetRoundPacks(c, 1)
	// The maintainer indexes packs that have stood still.
	clk.Add(cache.MidxDebounce)
	b.Run("write=round", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			if err := cache.ForgetMaintenance(c, repo); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if !cache.IndexRound(c, repo, rows) {
				b.Fatal("the round wrote no index")
			}
		}
	})
	for _, index := range []string{"none", "round", "maintainer"} {
		if err := cache.ForgetMaintenance(c, repo); err != nil {
			b.Fatal(err)
		}
		switch index {
		case "round":
			if !cache.IndexRound(c, repo, rows) {
				b.Fatal("the round wrote no index")
			}
		case "maintainer":
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				b.Fatalf("the maintainer ran %v, want the repository", ran)
			}
		}
		b.Run(fmt.Sprintf("index=%s/op=missing", index), func(b *testing.B) {
			for b.Loop() {
				missing, err := c.Missing(b.Context(), s, wants(oids...), int64(len(oids))*41)
				if err != nil || missing != "" {
					b.Fatalf("Missing of %d present objects: %q missing, %v", len(oids), missing, err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(oids)), "ns/lookup")
		})
		b.Run(fmt.Sprintf("index=%s/op=fetch", index), func(b *testing.B) {
			for b.Loop() {
				packOut(b, pool, c, s.Dir, "refs/heads/main", "^"+have)
			}
		})
		b.Run(fmt.Sprintf("index=%s/op=clone", index), func(b *testing.B) {
			for b.Loop() {
				packOut(b, pool, c, s.Dir, "refs/heads/main")
			}
		})
	}
}

// tagStream is n commits on main, the i'th adding t/f<i> and tagged t<i> by
// an annotated tag: every push a release.
func tagStream(n int) func(*bufio.Writer) error {
	return func(w *bufio.Writer) error {
		for i := range n {
			msg := fmt.Sprintf("release %d\n", i)
			fmt.Fprintf(w, "commit refs/heads/main\nmark :%d\ncommitter %s %d +0000\ndata %d\n%s", i+1, benchIdent,
				benchEpoch+i, len(msg), msg)
			fmt.Fprintf(w, "M 100644 inline t/f%05d\ndata %d\n%s\n", i, len(msg), msg)
			fmt.Fprintf(w, "tag t%05d\nfrom :%d\ntagger %s %d +0000\ndata %d\n%s\n", i, i+1, benchIdent,
				benchEpoch+i, len(msg), msg)
		}
		return nil
	}
}

// BenchmarkPeel is a full round's peel of the tags it sets, one cat-file asking
// every tag's object as refs.go peels packed-refs, over packs packs of one
// commit and one annotated tag each: write=round is the round's index,
// index=none and index=round the peel without it and with it. The round writes
// its index BEFORE the peel, so what it saves here it saves the cold request.
func BenchmarkPeel(b *testing.B) {
	quiet(b)
	w := newSyncWorld(b, onMemory)
	root := b.TempDir()
	for _, packs := range []int{32, 128, 256, 512, 1_000, 5_000} {
		b.Run(fmt.Sprintf("packs=%d", packs), func(b *testing.B) {
			if packs > 1_000 {
				gittest.Large(b, "5 000 tagged pushes packed, stored and synced")
			}
			src := gittest.Empty(b)
			gittest.FastImport(b, w.client, src, tagStream(packs))
			commits := commitsOf(b, w.client, src, "main")
			pushes := eachPush(commits)
			var ask strings.Builder
			for i := range pushes {
				tag := fmt.Sprintf("refs/tags/t%05d", i)
				pushes[i].tags = []string{tag}
				ask.WriteString(strings.TrimSpace(w.client.Run(b, src, "rev-parse", tag)) + "^{}\n")
			}
			rows, _ := w.storePushes(b, src, pushes)
			repo := w.logRepo(packs)
			w.writePushes(repo, pushes, rows)
			var pool *gitproc.Pool
			c, drop := w.coldCache(b, root, packs, cache.FetchConcurrency, func(o *cache.Options) { pool = o.Git })
			b.Cleanup(drop)
			s, err := c.Sync(b.Context(), repo, linearizable)
			if err != nil {
				b.Fatalf("syncing %d packs: %v", packs, err)
			}
			b.Cleanup(s.Release)
			cache.SetRoundPacks(c, 1)
			b.Run("write=round", func(b *testing.B) {
				for b.Loop() {
					b.StopTimer()
					if err := cache.ForgetMaintenance(c, repo); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					if !cache.IndexRound(c, repo, rows) {
						b.Fatal("the round wrote no index")
					}
				}
			})
			for _, index := range []string{"none", "round"} {
				if err := cache.ForgetMaintenance(c, repo); err != nil {
					b.Fatal(err)
				}
				if index == "round" && !cache.IndexRound(c, repo, rows) {
					b.Fatal("the round wrote no index")
				}
				b.Run("index="+index, func(b *testing.B) {
					for b.Loop() {
						res, err := pool.Run(b.Context(), gitproc.Cmd{
							Dir: s.Dir, Args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)",
								"--buffer"},
							Config: c.ServeConfig(), Stdin: strings.NewReader(ask.String()),
							Class: gitproc.ClassRead, Deadline: time.Minute,
						})
						if err != nil || strings.Count(string(res.Stdout), "\n") != packs {
							b.Fatalf("peeling %d tags: %v", packs, err)
						}
					}
				})
			}
		})
	}
}

// BenchmarkMemoTrim is the janitor's pass that drops the memo's oldest cohort
// (trim) over copies warm copies of cohorts cohorts each: a walk of the copies,
// a rename of the cohort's directory in each, and the removal from trash/ of
// what was renamed. A trim reads no answer, so the figure follows the copies;
// each cohort holds one empty answer, whose bytes the state records. list is a
// boot's first janitor pass: every copy's cohorts' names, no answers.
func BenchmarkMemoTrim(b *testing.B) {
	quiet(b)
	gittest.RequireNode(b)
	block := cache.MemoEntryBytes(0)
	for _, arm := range []struct{ copies, cohorts int }{{1000, 17}, {10000, 17}} {
		for _, step := range []string{"list", "trim"} {
			b.Run(fmt.Sprintf("copies=%d/cohorts=%d/%s", arm.copies, arm.cohorts, step), func(b *testing.B) {
				dir := filepath.Join(b.TempDir(), "c")
				first, err := cache.Prepare(b.Context(), dir)
				if err != nil {
					b.Fatalf("Prepare: %v", err)
				}
				pool, err := gitproc.New(b.Context(), gitproc.Options{Home: first.Home, MaxProcesses: 2})
				if err != nil {
					b.Fatalf("gitproc.New: %v", err)
				}
				first.Release()
				incs := memoFixture(b, dir, arm.copies, arm.cohorts, block)
				memo := int64(arm.copies*arm.cohorts) * block
				// One cohort past the share: the pass drops the oldest and no
				// more.
				budget := cache.MemoShare * (memo - int64(arm.copies)*block)
				for b.Loop() {
					b.StopTimer()
					for _, inc := range incs {
						write(b, filepath.Join(cache.MemoCohortDir(inc, 1), "entry"), "")
					}
					emptyTrash(b, dir)
					layout, err := cache.Prepare(b.Context(), dir)
					if err != nil {
						b.Fatalf("Prepare: %v", err)
					}
					c, err := cache.Open(b.Context(), cache.Options{
						Layout: layout, MaxBytes: 1 << 40, Git: pool, Objects: noObjects{t: b}, Fetch: noRows{t: b},
						Fleet: soloFleet, Limits: gitlet.DefaultLimits, HookCommand: []string{"/bin/true"}, Now: time.Now,
					})
					if err != nil {
						b.Fatalf("Open: %v", err)
					}
					if step == "trim" {
						cache.JanitorPass(c) // the boot's listing
						cache.SetMaxBytes(c, budget)
					}
					b.StartTimer()
					cache.JanitorPass(c)
					b.StopTimer()
					if step == "trim" && cache.MemoBytes(c) != memo-int64(arm.copies)*block {
						b.Fatalf("the pass left the memo at %d bytes, want the oldest cohort of every copy gone: %d",
							cache.MemoBytes(c), memo-int64(arm.copies)*block)
					}
					c.Stop(context.Background())
					b.StartTimer()
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(arm.copies), "ns/copy")
			})
		}
	}
}

// memoFixture writes copies repositories under dir/repos, each with a state of
// this build recording a memo of cohorts cohorts of block bytes each, and each
// cohort's directory holding one empty entry; it answers the incarnations'
// directories.
func memoFixture(b *testing.B, dir string, copies, cohorts int, block int64) []string {
	b.Helper()
	incs := make([]string, copies)
	for r := range copies {
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000001", r)
		inc := filepath.Join(dir, "repos", id+".1.git")
		incs[r] = inc
		if err := os.MkdirAll(filepath.Join(inc, "objects", "pack"), 0o700); err != nil {
			b.Fatal(err)
		}
		write(b, filepath.Join(inc, "packed-refs"), "# pack-refs with: peeled sorted \n")
		doc := aState(id)
		doc.Packs = 0
		memo := derivedDoc{Bytes: int64(cohorts) * block, Step: string(cache.StepMemo)}
		for n := 1; n <= cohorts; n++ {
			memo.Cohorts = append(memo.Cohorts, cohortDoc{N: uint64(n), Bytes: block})
			write(b, filepath.Join(cache.MemoCohortDir(inc, uint64(n)), "entry"), "")
		}
		doc.Derived[string(cache.DerivedMemo)] = memo
		writeState(b, inc, doc)
	}
	return incs
}
