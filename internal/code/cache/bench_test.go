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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/queue"
)

// The benchmarks below are the figures the package doc's "Measured" section
// records, and the ones a change to the boot or to the write path is compared
// by (make bench, six counts on each tree, benchstat).

// BenchmarkBoot is a warm git node's boot of its cache: Prepare — its reading
// of repos/ and Layout.Held — then Open and Stop, over repos repositories of
// packs packs each (a pack is three files; one push stores one until its
// repository's home merges them, which holds a busy repository near
// code.CompactTargetPacks and under code.MaxPacksPerRepo, so 100 and 1 000
// packs are a repository its compactions keep up with and one falling behind
// them). With a state in every incarnation, what each holds is the state's to
// say, so a boot reads one small file a repository and stats none of its
// packs: the figure follows repos and not packs. states=false is the same
// tree with no state anywhere — what a boot pays for an incarnation it must
// walk, and what every boot paid when Held was a walk of repos/.
//
// The pack files are empty: nothing a boot does reads their bytes, and a
// fixture of millions of real packs would measure the disk filling up.
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
			// The pool is built once over the HOME the first Prepare names;
			// every later Prepare moves HOME aside, which a pool built
			// already never looks at again.
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
					Fleet: soloFleet, Limits: code.DefaultLimits, HookCommand: []string{"/bin/true"}, Now: time.Now,
				})
				if err != nil {
					b.Fatalf("Open: %v", err)
				}
				c.Stop(context.Background())

				// The boot's own moves into trash/ are the janitor's to
				// remove, and this loop starts none: emptied with the timer
				// stopped, so every boot finds what a warm node's does.
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

// BenchmarkPlace prices the durability rule (durable.go): a step that renames
// files files, each staged and already durable, into one directory — a
// round's packs into objects/pack — made durable by a sync of the directory
// after EVERY rename (sync=each) against ONE sync after the step (sync=once).
// Each sync is a journal commit, which is why the rule syncs a step's
// directory once.
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

// BenchmarkBundleFiles prices a cold sync's own disk work for a bundle, apart
// from its GET and its hashing: the bundle's three files created in tmp/ and
// written at a median push's sizes, closed, made durable and renamed into one
// pack directory, .idx last — placePack's order — by width workers at once:
// one, and FetchConcurrency. sync=together syncs a bundle's three files at
// once, as placePack does; sync=none leaves them unsynced, to show what the
// syncs add. create-us is what a bundle's three creates took, so the figure
// names its own terms.
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

// bundleFiles is one bundle's file work for BenchmarkBundleFiles: bundle i's
// three files created in tmp, written from data at sizes, closed, synced at
// once when durable, and renamed into dest, the .idx last. The creates' time
// is added to created.
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

// BenchmarkAdvertise is a push's advertisement — receive-pack --stateless-rpc
// --advertise-refs, through the pool — over a repository of refs refs:
// in=cache runs it in the incarnation itself, the floor; in=view in an
// advertisement view with no configuration of its own, where receive-pack
// also lists its alternate's refs — the incarnation's, the same packed-refs —
// for the .have lines it then has none of to print; in=view-config in the
// view with Cache.AdvertiseConfig, which is how the host runs it. The three
// print the same bytes (checked once an arm), so the figures differ by the
// alternate's listing alone. refs=100000 is code.MaxRefsPerRepo.
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

// quiet keeps a benchmark's output to its figures: an info line in the middle
// of benchmark output is a line benchstat cannot read.
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// THE CACHE'S BYTE PATH, MEASURED: what a node pays the first time it serves
// a repository (BenchmarkColdSync), what every node pays after every push
// (BenchmarkDeltaSync), what indexing a repository's packs costs
// (BenchmarkMidx), and what one object lookup costs across many packs with
// and without that index (BenchmarkPackLookup). Every fixture is real git — a
// history fast-imported, each push's objects packed alone as the bundle a
// receiving node's receive-pack stores, its row in the fake log — built with
// the timer stopped. The arms whose fixture takes minutes and gigabytes — the
// cold syncs of code.MaxPacksPerRepo and of 10 000 pushes, a repository of
// 10 000 packs, a 5 GB index — run only under gittest.LargeEnv.

// benchWorkers is how many of a fixture's pushes are packed and stored at
// once: a push's pack is mostly a git process starting, and its put to a
// bucket a round trip away mostly waiting, so sixteen keep four cores busy
// and the round trips overlapped.
const benchWorkers = 16

// medianPush is what the median push adds (master §2.9), in bytes: every
// push of a cold sync's history but a tail's first, and every push a delta
// sync downloads.
const medianPush = 150_000

// benchIdent and benchEpoch are every imported commit's committer and the
// first one's time, each later commit a second on, so a fixture is the same
// history — and the same packs — on every run.
const (
	benchIdent = "Crewlet Bench <bench@example.com>"
	benchEpoch = 1_767_225_600 // 2026-01-01T00:00:00Z
	// nextEpoch is the first nextPush's time: past every history's last
	// commit, so a push lands on top of a history in time as well.
	nextEpoch = benchEpoch + 100_000_000
)

// providerFirstByte is how long the S3 fake waits before it answers each
// request in BenchmarkColdSync's every shape: 30 ms, the floor of what S3
// answers in, and the arm the cache's figures were first taken at. Every GET
// pays it whatever its size, which is what makes a cold sync bound by its GET
// count — sixteen at once at the 33 ms each takes allow about 480 bundles a
// second, and a round at FetchConcurrency reaches about 450 — and what a fake
// on loopback, answering thousands a second, could never show.
const providerFirstByte = 30 * time.Millisecond

// s3FirstBytes is the first byte BenchmarkColdSync's fake also answers its
// pack-count shapes at — the compacted repository's, the pack bound's and the
// uncompacted one's: 100 and 200 ms, the range AWS's performance guidelines
// give for S3's first byte, at which a GET holds its slot three to six times
// as long as at providerFirstByte — so a cold sync's rate at a real bucket is
// MEASURED at each end of it, never computed from the 30 ms arm.
var s3FirstBytes = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}

// The pack counts BenchmarkColdSync reads a repository at, each a push of
// medianPush bytes a pack.
const (
	// compactedPacks is a repository its home compacts: code.CompactTargetPacks,
	// the live packs a compaction holds one near — what a cold sync of every
	// compacted repository costs.
	compactedPacks = code.CompactTargetPacks

	// uncompactedPacks is a repository of 10 000 pushes that NOTHING
	// compacted, one live pack a push — the before: a repository of the code
	// host's first build, five times the pack bound (code.MaxPacksPerRepo)
	// that refuses such a history's pushes now until its home has merged them.
	uncompactedPacks = 10_000
)

// providerConnBytes is the most a second the S3 fake sends one connection of
// an object's bytes: 80 MB/s, the low end of what one S3 connection carries.
const providerConnBytes = 80_000_000

// commitsOf is ref's commits in src, oldest first.
func commitsOf(b *testing.B, client *gittest.Client, src, ref string) []string {
	b.Helper()
	return strings.Fields(client.Run(b, src, "rev-list", "--reverse", ref))
}

// chacha is a fixture's random bytes and draws for the n'th of what a domain
// generates: the same on every run, and different for every (domain, n) — so
// no two pushes of a long arm carry the same bytes, which a later push of
// bytes an earlier one stored would not download at all.
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

// storePushes packs each push's objects alone out of src — the bundle a
// receiving node's receive-pack stores — puts it in w's store, and answers
// every push's row in order and the bytes they name. benchWorkers pushes at
// once, each pack's files removed once stored, so the disk holds a worker's
// pack at a time.
func (w *syncWorld) storePushes(b *testing.B, src string, pushes []pushRange) ([]code.PackRow, int64) {
	b.Helper()
	scratch := b.TempDir()
	rows := make([]code.PackRow, len(pushes))
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

// writePushes writes each push to repo in w's log as the record that landed
// it: its pack's row, main moved to its tip, and its row naming the tip as a
// commit it introduced — so every delta's fold builds commit records, as a
// real push's does.
func (w *syncWorld) writePushes(repo string, pushes []pushRange, rows []code.PackRow) {
	for i, p := range pushes {
		w.log.writePushes(repo, fakePush{packs: []code.PackRow{rows[i]},
			refs: map[string]string{"refs/heads/main": p.tip}, introduced: []string{p.tip}})
	}
}

// coldCache prepares and opens a cache in a directory of its own under root
// — a node that has never served the repository — over w's rows, store and
// recorder (each mutate applied after), its pool a slot a core as a node's
// is, a round fetching fetch bundles at once. drop stops it and removes its
// directory, so a loop of cold syncs holds one cache on the disk at a time.
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
// own under root: a first boot syncs every repository of ids, linearizably and
// all at once with no cap on its full reads, and stops; a second boots over
// the same directory (each mutate applied to it) — every repository's copy
// and state where the last process left them, and none converged in this
// one, so each one's first round reads its rows whole and has nothing to
// download (cache.Options.Objects is the caller's to hold to that). drop
// stops it and removes its directory.
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

// bootCache prepares and opens a cache over dir as it finds it — a node's
// boot — reading w's rows, store and recorder (each mutate applied after),
// its pool a slot a core as a node's is, a round fetching fetch bundles at
// once. stop stops it and lets the directory go with everything it holds: a
// node going down, its cache's volume kept.
func (w *syncWorld) bootCache(b *testing.B, dir string, fetch int, mutate ...func(*cache.Options)) (
	_ *cache.Cache, stop func()) {

	b.Helper()
	layout, err := cache.Prepare(b.Context(), dir)
	if err != nil {
		b.Fatalf("Prepare: %v", err)
	}
	o := cache.Options{
		Layout: layout, MaxBytes: 1 << 40, Git: newPoolOf(b, layout, "", runtime.NumCPU(), 0, 1<<40), Objects: w.store,
		Fetch: silent{w.log}, Fleet: soloFleet, Limits: code.DefaultLimits, HookCommand: []string{"/bin/true"},
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

// coldWorld is a sync world over the bucket a cold-sync arm names — the S3
// fake answering every request firstByte after it arrives.
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

// coldRepo is a repository of shape's pushes in w's log and store, and the
// bytes its bundles hold.
func (w *syncWorld) coldRepo(b *testing.B, shape coldShape) (string, int64) {
	b.Helper()
	src := gittest.Empty(b)
	gittest.FastImport(b, w.client, src, pushStream(shape.packs, shape.size))
	pushes := eachPush(commitsOf(b, w.client, src, "main"))
	rows, total := w.storePushes(b, src, pushes)
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	repo := w.logRepo(1)
	w.writePushes(repo, pushes, rows)
	return repo, total
}

// BenchmarkColdSync is a git node's FIRST sync of a repository it has never
// served — a clone on a node that just joined, a rebuild, a repository the
// budget evicted — from linearizable rows to an answer: every push's bundle
// read from the object store, verified, split and renamed in, the round's
// multi-pack-index, packed-refs and the state. Over a history of packs
// pushes, each one commit adding medianPush seeded random bytes, read by a
// FRESH cache (prepared and opened with the timer stopped) fetching fetch
// bundles at once. The pack counts are a compacted repository's
// (compactedPacks, 50), 200, the pack bound's (code.MaxPacksPerRepo, 2 048:
// exactly the most a repository may hold) and an uncompacted repository's
// (uncompactedPacks, 10 000: the before); the last two only under
// CREWLET_BENCH_LARGE=1.
//
//   - bucket=memobj is the store in process: the node's own side alone —
//     SHA-256 over every byte, the disk.
//   - bucket=fake is the S3 fake over HTTP at the production part size, 80
//     MB/s a connection (providerConnBytes) and firstbyte to every
//     request's first byte: 30 ms for every shape (providerFirstByte), and
//     100 and 200 ms — S3's own range (s3FirstBytes) — for the compacted
//     repository's and the bound's at the cache's two widths, fetch=16 and
//     fetch=32, and for the uncompacted one's.
//   - bucket=real is the bucket CREWLET_BENCH_S3_* names
//     (s3fake.RealBucket), and skips when none is named.
//   - shape=tail is the same 200 pushes with the first 512 MiB — an
//     import's first step — whose download is the round's tail, shortened
//     only by reading one object's ranges at once (s3obj).
//
// fetch=1 is one GET at a time; fetch=16 is FetchConcurrency, half the
// node's; fetch=32 is NodeFetches, the whole node given to one repository,
// which the share gives up. The uncompacted shape runs at FetchConcurrency
// alone, a repository's own width, on every bucket: it is the before beside
// the compacted shape's after, and at the other widths it is ten thousand
// GETs that tell nothing the bound's 2 048 do not — one at a time, six
// minutes a count of first bytes in a row at 30 ms. packs/s at 200 and at
// 2 048 pushes is one rate when a cold sync is bound by its GETs rather than
// by the node; at a first byte f, fetch GETs in flight bound it near fetch/f a
// second. get-ms is one GET as the cache sees it, from its open to its close;
// dl-s is a sync's DOWNLOADS, from its first GET's open to its last GET's
// close — what code.MaxPacksPerRepo is held to at the bound (27 s, half the
// spool wait, at 200 ms and fetch=16), apart from the whole round (ns/op),
// which adds the read of the rows, the round's index and its peel and is held
// to the spool wait itself; cpu-ms/pack is the process's CPU a bundle, user
// and system — the S3 fake's server's among it, since it runs in the process
// and on the same cores, which a real bucket's does not.
func BenchmarkColdSync(b *testing.B) {
	quiet(b)
	type arm struct {
		bucket    string
		firstByte time.Duration
		shapes    []coldShape
		fetches   []int
	}
	compacted, bound, uncompacted := coldShape{packs: compactedPacks}, coldShape{packs: code.MaxPacksPerRepo},
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
				repo, total := w.coldRepo(b, shape)
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
			})
		}
	}
}

// timedObjects is the cache's object store with every read timed from its
// open to its close: what one GET of a cold sync takes as the cache sees it,
// and the span from the earliest open to the latest close since reset — the
// sync's downloads, whatever else its round does.
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

// processCPU is the CPU time this process has spent, user and system: every
// goroutine's, the S3 fake's server among them, since it runs in the process.
func processCPU(b *testing.B) time.Duration {
	b.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkColdJoin is git nodes BRINGING UP MANY REPOSITORIES AT ONCE, every
// repository asked of each at the same moment — a balancer moving traffic
// onto them, CI fetching through them — against ONE data node (coldLane):
// the queue.ConcurrentAnswers answers its shared lane gives code.repo_state
// beside every other read of the fleet (estate's laneShared), a request past
// them waiting its turn in arrival order, and inside each answer one of the
// coldReaderConns reader connections its store scans a page on. Each node is
// asked for 500 repositories of one pack and one ref and reads their rows
// through the data node with its full reads capped at rounds, while the
// lane's OTHER traffic (a seat's tracker read, a page write, the
// code.resolve every git request is admitted by) arrives every
// coldOtherEvery and holds its answer and its reader coldOtherServe. Two
// events, each the cap's cost shown another way:
//
//   - cache=fresh, FOUR NODES JOINING a fleet together with caches that have
//     never served a repository: every round downloads its pack beside its
//     read, so the reads' time is hidden behind the downloads of repositories
//     of more than a handful of packs (NodeFetches a node) — and what the cap
//     is for, the other traffic's wait behind four nodes' full reads, shows.
//     rounds is MaxFullRounds' candidates 4 to 32, and 500, a cap no node
//     asked for 500 repositories meets: the node without one.
//   - cache=kept, ONE NODE RESTARTED over the cache it kept — a rolling
//     restart of a git node whose git.cache_dir is on a volume the restart
//     keeps (keptCache): every repository's first round in the new process
//     is a full read, and it has nothing to download, so the reads ARE how
//     long the node takes to come up. rounds is 4 to 32, 64 — the lane's
//     every answer — and 500.
//
// THE DATA NODE IS A MODEL: every page holds its answer and its reader
// coldPageServe, a full page's cost, whatever the fake log's page holds — so
// each repository's read is one page at that cost — and a reader given back
// goes to a waiting read drawn at random, as database/sql's pool hands one
// out (coldReaders); the cache's own work (the pack's GET from a store in the
// process, its files, packed-refs and the state, and on a restart the
// hashing of every index file a previous process left) is real. wait-p99-ms
// and wait-max-ms are the other traffic's wait for its answer and its reader
// — what the cap is for; up-s is how long the last of the nodes' syncs took:
// on a join the estate's share of it, its downloads coming on top; on a
// restart the whole of it.
func BenchmarkColdJoin(b *testing.B) {
	quiet(b)
	const repos = 500
	w := newSyncWorld(b, onMemory)
	src := w.repo(b, 1)
	tip, row := w.commit(b, src, nil, "")
	ids := make([]string, repos)
	for i := range ids {
		// One object every row names, under a pack id of each repository's
		// own, as a push mints one: the store holds the bytes once, and every
		// node installs them for every repository it is asked for.
		ids[i] = w.logRepo(i + 1)
		row.PackID = uuid.Must(uuid.NewV7()).String()
		w.log.write(ids[i], []code.PackRow{row}, map[string]string{"refs/heads/main": tip})
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

// The data node BenchmarkColdJoin models. coldPageServe is how long a page
// holds its answer and its reader: 59 ms, what code's
// BenchmarkServeASnapshotPage measured a full page of refs to take to serve
// (code.MaxSnapshotItems of a repository of code.MaxRefsPerRepo, nearly all
// of it the driver's scan), and a millisecond for the estate's crossing —
// what a push proposed through another member pays over one proposed at its
// committer (internal/code/codecluster's package doc). coldReaderConns is the
// ordinary reader connections its store opens: max(8, GOMAXPROCS) (store's
// defaultReaderConns), so eight on a data node of eight cores or fewer — the
// fewest any data node has, and the most full pages such a node scans at
// once. coldOtherEvery and coldOtherServe are the lane's other traffic: two
// hundred reads a second, each a millisecond's crossing and a millisecond's
// read.
const (
	coldPageServe   = 60 * time.Millisecond
	coldReaderConns = 8
	coldOtherEvery  = 5 * time.Millisecond
	coldOtherServe  = 2 * time.Millisecond
)

// coldLane is ONE DATA NODE in front of the rows, as a read reaches it: its
// shared lane's answers, a request past them waiting its turn first come,
// first served, as a registration's requests wait in the broker's order for
// a slot (queue.EventQueue.Serve); then, inside the answer, one of the node's
// reader connections, since every page is ONE read transaction
// (statelog.Reader.Read) and every other read of the lane is too — so a page
// holds both for coldPageServe, and the node scans at most coldReaderConns
// pages at once, however many answers it gives.
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

func (l *coldLane) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	if err := l.answers.Acquire(ctx, 1); err != nil {
		return code.RepoSnapshot{}, err
	}
	defer l.answers.Release(1)
	l.readers.take()
	defer l.readers.give()
	time.Sleep(coldPageServe)
	return l.inner.RepoState(ctx, q)
}

// Read is the backfill's read of a repository's push rows (code.read), which
// a data node answers as it answers code.repo_state: on the same shared lane,
// and as ONE read transaction (statelog.Reader.Read) on one of its reader
// connections, so it holds one of each for as long as the rows take to
// answer, with no cost of its own, since the model prices only a snapshot's
// page. A cold join's caches are never started, so no backfill reads through
// the lane here and nothing the benchmark reports moves.
func (l *coldLane) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if err := l.answers.Acquire(ctx, 1); err != nil {
		return code.ReadAnswer{}, err
	}
	defer l.answers.Release(1)
	l.readers.take()
	defer l.readers.give()
	return l.inner.Read(ctx, q)
}

// traffic starts the lane's other traffic, a read every coldOtherEvery, and
// answers what stops it — which answers how long each read waited for its
// answer and its reader, once every one has had both.
func (l *coldLane) traffic() (stop func() []time.Duration) {
	var (
		mu    sync.Mutex
		waits []time.Duration
		wg    sync.WaitGroup
	)
	done := make(chan struct{})
	wg.Go(func() {
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
				waited := time.Since(began)
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

// coldReaders is the modeled data node's reader connections, handed out as
// database/sql hands out its pool's: a read past them waits, and a
// connection given back goes to a waiting read CHOSEN AT RANDOM
// (connRequestSet.TakeRandom), never the one that has waited longest — so
// once full pages queue for a node's readers, every other read's wait is a
// draw among them, and its tail is many pages long.
type coldReaders struct {
	mu      sync.Mutex
	free    int
	waiting []chan struct{}
}

// take waits for a connection. No modeled read gives up waiting: every one
// is the benchmark's own, and each arm waits for all of them.
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

// timedLog is a Fetcher that keeps how long its log took to answer, so a
// benchmark can name the share of each operation that is the fake's own work
// rather than the cache's.
type timedLog struct {
	inner cache.Fetcher
	took  atomic.Int64
}

func (l *timedLog) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	began := time.Now()
	defer func() { l.took.Add(int64(time.Since(began))) }()
	return l.inner.RepoState(ctx, q)
}

func (l *timedLog) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	began := time.Now()
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

// BenchmarkDeltaSync is the sync every git node runs after every push, on a
// WARM cache: one new pack (a commit adding medianPush bytes) and main moved,
// read as a delta, downloaded and renamed in, packed-refs rewritten with the
// move and the state after it — on a repository already holding packs packs
// and refs refs. refs=100000 is code.MaxRefsPerRepo, where packed-refs is
// about 6.6 MB written whole on every push that moves a ref.
//
// log-ns/op is the share of each operation the fake log took to answer: it
// walks every ref for a delta where the reader's statement reads one index
// range, so at a hundred thousand refs it is a cost the real read does not
// have, and the cache's own figure is ns/op less it.
func BenchmarkDeltaSync(b *testing.B) {
	quiet(b)
	for _, packs := range []int{100, 10_000} {
		b.Run(fmt.Sprintf("packs=%d", packs), func(b *testing.B) {
			if packs > 100 {
				gittest.Large(b, "10 000 pushes packed and stored, synced twice")
			}
			w := newSyncWorld(b, onMemory)
			src := gittest.Empty(b)
			// The history's pushes are small: what a delta sync is claimed
			// not to depend on is how many packs there are, not their bytes.
			gittest.FastImport(b, w.client, src, pushStream(packs, func(int) int64 { return 1 << 10 }))
			commits := commitsOf(b, w.client, src, "main")
			pushes := eachPush(commits)
			rows, _ := w.storePushes(b, src, pushes)
			root := b.TempDir()
			for i, refs := range []int{1_000, 100_000} {
				// The repository and its warm cache are built here, once,
				// and every count of the arm pushes onto them.
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
func (w *syncWorld) midxRepo(b *testing.B, size int64) (repo, src string, commits []string, rows []code.PackRow,
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

// stalePushBytes is what each of those pushes adds: one small file, so the
// clone they make dearer is dearer for the walk the bitmap no longer saves
// and not for the bytes it sends.
const stalePushBytes = 4 << 10

// packOut is pack-objects in dir as upload-pack runs it for a request — under
// ServeConfig, in the read lane, its bytes discarded — over revs: what a
// clone or a fetch is served.
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

// BenchmarkMidx is what indexing a repository's packs costs, and what the
// maintainer's index stops saving as pushes land outside it.
//
//   - index=round is the multi-pack-index a full round writes before it
//     answers once it leaves MidxRoundPacks packs or more: no bitmap, a
//     merge of the packs' own indexes, in the read lane — what a cold
//     request waits for before its first byte.
//   - index=maintainer is one maintenance run: the index with its bitmap,
//     then the commit-graph, in the maintenance lane at one thread — the
//     run MidxMinInterval and MidxDutyCycle pace, and
//     gitproc.PackWorkDeadline bounds.
//   - memory/step=… (100MB and 1GB) is the memory each git process the cache
//     runs over the whole repository holds — the figures its memory budget
//     charges by (memoryArms says which).
//   - lookup=on|off (1GB, and 5GB under CREWLET_BENCH_LARGE) is the
//     maintainer's bitmap written with its lookup table (ServeConfig's
//     pack.writeBitmapLookupTable) and without it, the maintainer run again
//     between the two with the timer stopped: op=blob is the pack a
//     blobless clone's lazy fetch of one blob is served (its --filter
//     included), op=fetch the pack a fetch of main's newest commit is
//     (main~1..main) — two requests that open the bitmap to answer a few
//     objects.
//   - stale=k (100MB and 1GB) is once k pushes of stalePushBytes have
//     landed outside the maintainer's index: op=count counts main's objects
//     from the bitmap, the walk a bitmap that many pushes behind leaves a
//     clone to do, and op=clone is the pack a clone of main is served — the
//     price MidxPacks weighs against a run.
//
// Each over a repository of 64 packs of the history above, the previous
// index and graph removed with the timer stopped. size is the packs' bytes,
// about; objects and commits are what they hold.
func BenchmarkMidx(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name  string
		bytes int64
	}{{"100MB", 100_000_000}, {"1GB", 1_000_000_000}, {"5GB", 5_000_000_000}} {
		b.Run("size="+arm.name, func(b *testing.B) {
			if arm.bytes > 1_000_000_000 {
				gittest.Large(b, "a 5 GB history fast-imported, packed, stored and synced")
			}
			w := newSyncWorld(b, onMemory)
			w.processes = runtime.NumCPU()
			repo, src, commits, rows, total, objects := w.midxRepo(b, arm.bytes)
			stale := arm.bytes <= 1_000_000_000
			if !stale {
				if err := os.RemoveAll(src); err != nil {
					b.Fatal(err)
				}
			}
			clk := newClock()
			c := w.open(b, clk.on, func(o *cache.Options) { o.MaxBytes, o.Logger = 1<<40, nil })
			s := w.sync(b, c, repo)
			defer func() { s.Release() }()
			// Its 64 packs are under MidxRoundPacks: what the arm prices is
			// the round's index over the history, whatever count writes it.
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
			if !stale {
				// No memory arms over 5 GB: its bitmap holds some 10 GB
				// beside the history the in-memory store already holds,
				// past what a machine that runs the rest has.
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
				b.Run(fmt.Sprintf("stale=%d/op=count", k), func(b *testing.B) {
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
				b.Run(fmt.Sprintf("stale=%d/op=clone", k), func(b *testing.B) {
					for b.Loop() {
						packOut(b, w.pool, c, s.Dir, "refs/heads/main")
					}
				})
			}
		})
	}
}

// BenchmarkVerify is an operator's check of a repository this node holds
// (Cache.AdmitVerify, then Verification.Run), the figures a check's bound is
// set from (CheckBudget):
//
//   - op=check is a check of a copy that is whole: its sync answered at
//     once, the listing, every bundle hashed against its row, packed-refs,
//     and git's connectivity check.
//   - op=repair is the same with one pack damaged before each run — its
//     largest, the last bytes of its last object overwritten with the timer
//     stopped — so the check finds it, the rebuild links every other pack in
//     and downloads that one from a store in the process, and the rebuilt
//     copy is checked again.
//   - op=hash is the hashing alone: every bundle's three files back to back,
//     the share of a check VerifyHashPerMiB charges by the mebibyte, beside
//     the connectivity check's own gitproc.PackWorkDeadline.
//
// size=small is five pushes of medianPush, a repository a team began this
// week; size=100MB and size=1GB are BenchmarkMidx's source composition — every
// commit four files of one directory, 64 packs — the gigabyte only under
// CREWLET_BENCH_LARGE=1. ms/MiB is the operation over the copy's bytes.
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
				repo, total = w.coldRepo(b, coldShape{packs: 5})
			} else {
				var src string
				repo, src, _, _, total, objects = w.midxRepo(b, arm.bytes)
				if err := os.RemoveAll(src); err != nil {
					b.Fatal(err)
				}
			}
			// THE CLOCK STANDS STILL, so the maintainer never finds the
			// repository due and no run of its own shares the disk with a
			// check.
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

// lookupArms are BenchmarkMidx's lookup=off|on arms over repo, synced in s:
// the maintainer's bitmap written without its lookup table and with it, each
// by a maintainer pass, and under each the two requests a node serves most
// that open the bitmap to answer a handful of objects — a blobless clone's
// lazy fetch of one blob, and a fetch of main's newest commit.
//
// EACH ARM'S SETUP IS ITS OWN SUB-BENCHMARK'S, so a -bench pattern that
// selects neither arm pays for neither maintainer run — a minute at the
// gigabyte and five at 5 GB of setup nobody reads — and the production
// setting is put back however many arms ran: every arm after these writes its
// own bitmap, under the setting every node runs with.
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

// memorySteps are what BenchmarkMidx's memory arms price, in the order they
// run: each git process a cache runs over a whole repository, with what it
// finds in the incarnation when it starts.
var memorySteps = []struct {
	name string
	// args is the process; none is a clone (packOut), run in the read lane as
	// a request's transfer runs; the rest run in the maintenance lane.
	args []string
	// index is the multi-pack-index the incarnation holds when it starts:
	// "" none, "plain" one without a bitmap, "bitmap" the maintainer's.
	index string
	// graph: the commit-graph is there when it starts.
	graph bool
}{
	{name: "index", args: []string{"multi-pack-index", "write", "--no-progress"}},
	{name: "clone-walk", index: "plain"},
	{name: "bitmap", args: []string{"multi-pack-index", "write", "--bitmap", "--no-progress"}},
	{name: "graph", args: []string{"commit-graph", "write", "--reachable", "--no-progress"}, index: "bitmap"},
	{name: "clone", index: "bitmap", graph: true},
	{name: "fsck", args: []string{"fsck", "--connectivity-only", "--no-dangling", "--no-progress"}, index: "bitmap",
		graph: true},
}

// memoryArms are BenchmarkMidx's memory/step=… arms over repo, synced in s:
// the memory each git process a cache runs over the whole repository holds —
// the figures the node's memory budget charges by (BitmapBytesPerObject,
// IndexBytesPerObject, GraphBytesPerObject, FsckBytesPerObject,
// gitproc.TransferBytesPerObject). Each is the peak ANONYMOUS memory of the one
// git process, sampled every millisecond from /proc (gittest.PeakAnon; Linux only —
// elsewhere only the times are reported), divided by the objects the packs
// hold: the packs' own pages it maps in are not counted, since the kernel
// takes those back under pressure rather than killing anybody.
//
//   - step=index is a multi-pack-index without a bitmap — a full round's own,
//     and the maintainer's where the node cannot hold the bitmap.
//   - step=bitmap is the maintainer's index with its bitmap: a walk of the
//     whole history.
//   - step=graph is the commit-graph a run writes after its index.
//   - step=clone-walk and step=clone are the pack a clone of main is served
//     (packOut) with only the index without a bitmap and with the
//     maintainer's bitmap — what one transfer of the repository holds beside
//     a run.
//   - step=fsck is Verify's connectivity check.
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
			run(b, memorySteps[0].args)
		case "bitmap":
			run(b, memorySteps[2].args)
		}
		if graph {
			run(b, memorySteps[3].args)
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

// lookupSizes are the histories BenchmarkPackLookup splits into packs, each of
// the composition above: 100 MB — about 300 000 objects and 42 000 commits, a
// repository of a size the default limits hold many of — at every pack count,
// and a gigabyte at the counts around MidxRoundPacks, so the threshold is
// measured at two sizes rather than assumed to hold at the second.
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

// BenchmarkPackLookup is what a cache holding packs packs pays per request
// with no multi-pack-index, with the round's own and with the maintainer's,
// and what writing the round's costs — the figures MidxRoundPacks is set
// from. A push stores one pack, so packs is how many pushes a repository has
// had since its home last compacted them — up to code.MaxPacksPerRepo, past
// which a push is refused until a compaction merges them, and 10 000 an
// uncompacted repository's, the code host's first build; every arm of a size
// splits the same history. The gigabyte and 10 000 packs run only under
// CREWLET_BENCH_LARGE=1.
//
//   - write=round is the round's index written over the packs (IndexRound,
//     the threshold lowered to one so every arm writes it): what a full
//     round of that many packs makes its request wait for.
//   - index=none, index=round and index=maintainer are the requests below
//     served with no index, with the round's (no bitmap) and with the
//     maintainer's (its bitmap and the commit-graph): a joining node's
//     cold request, the requests until the maintainer's index lands, and
//     every one after.
//   - op=missing is Missing of 1 000 commits drawn from the whole history —
//     from every pack, up to a thousand of them — a fetch's question
//     before it is served; ns/lookup includes starting the cat-file the
//     thousand share.
//   - op=fetch is the pack a fetch of the newest lookupFetch commits is
//     served: the request after a push.
//   - op=clone is the pack a clone of main is served — pack-objects over the
//     whole history: the request a cold cache was synced for, and the first
//     a coding run's fresh sandbox makes.
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

// BenchmarkPeel is the peel a full round runs over the tags it sets — one
// cat-file asking every tag's object, as refs.go peels packed-refs — across
// packs packs of one commit and one annotated tag each, the repository every
// one of whose pushes tagged a release: write=round is the round's index over
// them, and index=none and index=round the peel without it and with it. The
// round writes its index BEFORE the peel, so what the index saves here it
// saves the cold request it was written for.
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
							Dir: s.Dir, Args: []string{"cat-file", "--batch-check=%(objectname)", "--buffer"},
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
// (trim), over copies copies of a warm node whose memos hold cohorts cohorts
// each — a state each recording its memo, booted and listed: one walk of the
// node's copies and one rename of the cohort's directory in each, and the
// removal from trash/ of what was renamed there, which the same pass makes.
// The trim reads no answer, so the figure follows the copies and not the
// answers: each cohort holds one, an empty file, whose bytes the state says.
// list is the pass a boot's first janitor pass is: every copy's tree listed
// once, its cohorts' names read and none of its answers.
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
						Fleet: soloFleet, Limits: code.DefaultLimits, HookCommand: []string{"/bin/true"}, Now: time.Now,
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
