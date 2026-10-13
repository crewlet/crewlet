package cache_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/logging"
)

// The benchmarks below are the figures the package doc's "Measured" section
// records, and the ones a change to the boot or to the write path is compared
// by (make bench, six counts on each tree, benchstat).

// BenchmarkBoot is a warm git node's boot of its cache: Prepare — its reading
// of repos/ and Layout.Held — then Open and Stop, over repos repositories of
// packs packs each (a pack is three files; one push stores one, and M2
// compacts nothing, so a busy repository's packs are its pushes). With a
// state in every incarnation, what each holds is the state's to say, so a
// boot reads one small file a repository and stats none of its packs: the
// figure follows repos and not packs. states=false is the same tree with no
// state anywhere — what a boot pays for an incarnation it must walk, and what
// every boot paid when Held was a walk of repos/.
//
// The pack files are empty: nothing a boot does reads their bytes, and a
// fixture of millions of real packs would measure the disk filling up.
func BenchmarkBoot(b *testing.B) {
	quiet(b)
	gittest.Require(b)
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
					Limits: code.DefaultLimits, HookCommand: []string{"/bin/true"}, Now: time.Now,
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
		want := advertise(b, w.pool, s.Dir, nil)
		for _, arm := range []struct {
			in     string
			dir    string
			config []gitproc.KV
		}{
			{"cache", s.Dir, nil},
			{"view", view.Dir, nil},
			{"view-config", view.Dir, c.AdvertiseConfig()},
		} {
			b.Run(fmt.Sprintf("refs=%d/in=%s", refs, arm.in), func(b *testing.B) {
				if got := advertise(b, w.pool, arm.dir, arm.config); got != want {
					b.Fatalf("the advertisement in %s is not the cache's own (%d bytes against %d)",
						arm.in, len(got), len(want))
				}
				for b.Loop() {
					advertise(b, w.pool, arm.dir, arm.config)
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
