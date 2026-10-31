package query_test

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// THE HISTORY READS, MEASURED: code_blame of a file and
// code_log of a path, git run exactly as the query server runs it (blame.go,
// log.go: the same arguments, a log page of code.MaxLogLimit commits, the
// serve configuration's version-2 filters), over a history whose commit-graph
// is shaped as a git node holds one:
//
//   - filters=on/tail=0 is every commit in a graph with every changed-path
//     filter — a chain whose publication covers the whole history;
//   - filters=on/tail=N/max-new-filters=… is the newest N objects' commits
//     (compact.PublishTailObjects, a publication's tail at its bound) outside
//     the published graph layer, under the node's tip — the cache's declared
//     tip write, which computes every filter (all), against the same write
//     bounded to 100 or 1 024 new ones, every tail commit past them diffed by
//     the read;
//   - filters=off/… is the same graphs with no filter at all, what every read
//     walked before the filters;
//   - own/max-new-filters=… is OWN MODE's whole graph (the cache's graphArgs,
//     every filter, against the same bounded to 100 or 1 024), written once
//     over a repository with no graph — a repository's first own-mode write,
//     every commit past a bound's filters diffed by the read.
//
// Each arm reports the one write its shape was made by — the whole graph,
// own mode's, or the node's tip over the published layer — beside the read:
// its seconds and its peak anonymous memory (write-s, write-peak-MiB).
//
// tree is where the file sits. tree=flat is gittest.History's own layout,
// its first file d000/f00000 under a root of thirty directories: a commit
// with no filter costs the read one compare of a root tree's entry, so it
// cannot tell the bounds apart (51 and 96 ms with no filter at all at the
// gigabyte). tree=deep is gittest.DeepHistory's — the same commits with the
// file four levels down, src/core/d000/f00000, beside a wide tree of
// historyWide files whose thousand directories src/ lists before core/ — so a
// commit with no filter costs the read a walk of src/'s listing, as a
// monorepo's does.
// The file is rewritten by one commit in each of its 1 280 versions — a
// window of it each time, so a blame walks back to the last change of each of
// its lines, and a page of the path's log walks some 36 000 commits at the
// gigabyte; history=1GB is the large model's commit count (455 680 commits,
// gittest.LargeEnv) and history=100MB a run's without it. Each read must
// answer within gitproc.ReadDeadline with the filters, the tail included;
// tree=deep's arms decided that every graph write computes every filter
// (internal/code/cache's maintain.go says why) and, with their write-peak-MiB
// against objects, cache.GraphBaseBytes; estate.GitReadAttempt is re-checked
// against the slower of the two reads.
func BenchmarkBlame(b *testing.B) {
	historyReads(b, func(path, tip string) []string {
		return []string{"blame", "--porcelain", tip, "--", path}
	})
}

// BenchmarkLogPath is BenchmarkBlame's arms for code_log of the path: one
// page of code.MaxLogLimit commits that changed it, newest first in
// topological order, as the server reads one.
func BenchmarkLogPath(b *testing.B) {
	historyReads(b, func(path, tip string) []string {
		return []string{"log", "-z", "--topo-order", "--no-mailmap", "--no-color", "--no-notes",
			"--format=%H%x00%P%x00%an%x00%ae%x00%at%x00%ct%x00%s%x00", "--max-count=" + strconv.Itoa(code.MaxLogLimit),
			tip, "--", ":(literal)" + path}
	})
}

// historySizes are the histories the reads walk.
var historySizes = []struct {
	name  string
	bytes int64
	large bool
}{{"100MB", 100_000_000, false}, {"1GB", 1 << 30, true}}

// historyTree is one tree= arm: where the read's file sits, and what one
// commit of its history adds.
type historyTree struct {
	name string
	// path is the file every arm blames and logs: the history's first,
	// rewritten once in every version of every session.
	path string
	// perCommit is the objects one commit of the history adds, which the
	// tail's commits are counted from.
	perCommit int64
	// wide is how many files the wide tree holds beside the history's
	// (gittest.DeepHistory), 0 for none.
	wide int
}

// historyWide is tree=deep's wide tree: 100 000 files, as many as
// BenchmarkCodeRead's large arm lists, in a thousand directories src/ lists.
const historyWide = 100_000

// historyTrees are the tree= arms.
var historyTrees = []historyTree{
	{name: "flat", path: "d000/f00000", perCommit: gittest.HistoryObjectsPerCommit},
	{name: "deep", path: gittest.DeepPath(0), perCommit: gittest.DeepObjectsPerCommit, wide: historyWide},
}

// newFilterBounds are the bounds on a write's new changed-path filters the
// tail's and own mode's arms read through: 100 and 1 024, and "all" for none
// — the cache's own writes, which compute every filter.
var newFilterBounds = []string{"100", "1024", "all"}

// historyReads runs one read, read(path, tip) its arguments, over every
// shape of every tree of every size.
func historyReads(b *testing.B, read func(path, tip string) []string) {
	codetest.Quiet(b)
	tail := compact.PublishTailObjects
	for _, size := range historySizes {
		b.Run("history="+size.name, func(b *testing.B) {
			if size.large {
				gittest.Large(b, "a 1 GB history fast-imported, its commit-graph written nine ways")
			}
			pool, err := gitproc.New(b.Context(), gitproc.Options{Home: filepath.Join(b.TempDir(), "home"),
				MaxProcesses: runtime.NumCPU(), Memory: 1 << 40})
			if err != nil {
				b.Fatal(err)
			}
			for _, tree := range historyTrees {
				b.Run("tree="+tree.name, func(b *testing.B) {
					rig := historyRigOf(b, size.name, size.bytes, tree)
					arms := []struct{ name, shape string }{
						{"filters=on/tail=0", "on-0"},
						{"filters=off/tail=0", "off-0"},
						{fmt.Sprintf("filters=off/tail=%d", tail), "off-tail"},
					}
					for _, filters := range newFilterBounds {
						arms = append(arms, struct{ name, shape string }{
							fmt.Sprintf("filters=on/tail=%d/max-new-filters=%s", tail, filters), "on-tail-" + filters})
					}
					for _, filters := range newFilterBounds {
						arms = append(arms, struct{ name, shape string }{"own/max-new-filters=" + filters, "own-" + filters})
					}
					for _, arm := range arms {
						b.Run(arm.name, func(b *testing.B) {
							dir := rig.shaped(b, pool, arm.shape)
							for b.Loop() {
								if res, err := pool.Run(b.Context(), gitproc.Cmd{Dir: dir,
									Args: read(tree.path, rig.tip()),
									Config: []gitproc.KV{{Key: "commitGraph.changedPathsVersion", Value: "2"},
										{Key: "mailmap.blob", Value: ""}},
									Stdout: io.Discard, Class: gitproc.ClassRead, Deadline: time.Hour}); err != nil {
									b.Fatalf("git %s: %v\n%s", arm.name, err, res.Stderr)
								}
							}
							took := b.Elapsed().Seconds() / float64(b.N)
							b.ReportMetric(took/gitproc.ReadDeadline.Seconds(), "x-deadline")
							b.ReportMetric(float64(len(rig.commits)), "commits")
							b.ReportMetric(float64(rig.objects), "objects")
							if w, ok := rig.writes[arm.shape]; ok {
								b.ReportMetric(w.took.Seconds(), "write-s")
								if w.measured {
									b.ReportMetric(float64(w.peak)/(1<<20), "write-peak-MiB")
								}
							}
						})
					}
				})
			}
		})
	}
}

// historyRig is one size's history in one tree, built ONCE A PROCESS for
// both reads' benchmarks — at the gigabyte, minutes of fast-import and of
// graph writes — in a directory TestMain removes when the binary ends, since
// the benchmark that built it ends before the other reads it.
type historyRig struct {
	tree historyTree
	// repo holds hard links of the history's packs and main at its tip.
	repo    string
	commits []string
	// objects is how many objects the history's packs hold: a write's memory
	// is read against them, as the cache charges it, an amount an object.
	objects int64
	// shapes is each graph shape's repository, written once.
	shapes map[string]string
	// writes is each shape's own one write — the whole graph, own mode's, or
	// the node's tip over the published layer — its seconds and its peak
	// anonymous memory.
	writes map[string]graphWrite
}

// graphWrite is one commit-graph write's figures.
type graphWrite struct {
	took     time.Duration
	peak     int64
	measured bool
}

var historyRigs = struct {
	sync.Mutex
	m    map[string]*historyRig
	dirs []string
}{m: map[string]*historyRig{}}

// removeHistoryRigs removes every rig's directory: TestMain's, once every
// benchmark has run.
func removeHistoryRigs() {
	historyRigs.Lock()
	defer historyRigs.Unlock()
	for _, dir := range historyRigs.dirs {
		_ = os.RemoveAll(dir)
	}
}

// historyRigOf is size's rig in tree, built by the first benchmark that asks.
func historyRigOf(b *testing.B, name string, bytes int64, tree historyTree) *historyRig {
	b.Helper()
	historyRigs.Lock()
	defer historyRigs.Unlock()
	key := name + "-" + tree.name
	if r := historyRigs.m[key]; r != nil {
		return r
	}
	root, err := os.MkdirTemp("", "crewlet-history-"+key+"-")
	if err != nil {
		b.Fatal(err)
	}
	historyRigs.dirs = append(historyRigs.dirs, root)
	var (
		src     string
		commits []string
	)
	if tree.wide > 0 {
		src, commits = gittest.DeepHistory(b, gittest.NewClient(b), bytes, tree.wide)
	} else {
		src, commits = gittest.History(b, gittest.NewClient(b), bytes)
	}
	r := &historyRig{tree: tree, repo: filepath.Join(root, "repo"), commits: commits,
		objects: inPack(b, src), shapes: map[string]string{}, writes: map[string]graphWrite{}}
	linkRepo(b, src, r.repo, func(rel string) bool { return strings.HasPrefix(rel, "pack/") })
	gittest.SetRefs(b, r.repo, map[string]string{"refs/heads/main": r.tip()})
	historyRigs.m[key] = r
	return r
}

// inPack is how many objects repo's packs hold, as count-objects says.
func inPack(b *testing.B, repo string) int64 {
	b.Helper()
	for line := range strings.SplitSeq(gittest.NewClient(b).Run(b, repo, "count-objects", "-v"), "\n") {
		if n, ok := strings.CutPrefix(line, "in-pack: "); ok {
			v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			if err != nil {
				b.Fatalf("count-objects in %s: %v", repo, err)
			}
			return v
		}
	}
	b.Fatalf("count-objects in %s names no in-pack line", repo)
	return 0
}

// tip is main's commit.
func (r *historyRig) tip() string { return r.commits[len(r.commits)-1] }

// shaped is the repository of shape, its graphs written by the first arm
// that asks, on pool: on-0 and off-0 one layer over every commit, with every
// filter and with none; on-tail-<filters> and off-tail the published layer
// over every commit but the tail's — written once for the three tips over
// it — and the node's tip over the tail, computing at most filters new
// filters ("all": every one) or none; own-<filters> own mode's whole graph,
// written once over no graph at that bound, timed and its memory sampled.
func (r *historyRig) shaped(b *testing.B, pool *gitproc.Pool, shape string) string {
	b.Helper()
	historyRigs.Lock()
	defer historyRigs.Unlock()
	return r.shapedLocked(b, pool, shape)
}

// shapedLocked is shaped under the rigs' lock.
func (r *historyRig) shapedLocked(b *testing.B, pool *gitproc.Pool, shape string) string {
	b.Helper()
	if dir := r.shapes[shape]; dir != "" {
		return dir
	}
	write := func(dir string, stdin string, args ...string) graphWrite {
		b.Helper()
		cmd := gitproc.Cmd{Dir: dir, Args: append(append([]string{"commit-graph", "write"}, args...), "--no-progress"),
			Config: []gitproc.KV{{Key: "commitGraph.changedPathsVersion", Value: "2"}},
			Class:  gitproc.ClassMaintenance, Deadline: time.Hour}
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var (
			res  gitproc.Result
			err  error
			took time.Duration
		)
		peak, measured := gittest.PeakAnon(func() {
			began := time.Now()
			res, err = pool.Run(b.Context(), cmd)
			took = time.Since(began)
		})
		if err != nil {
			b.Fatalf("git commit-graph write %v in %s: %v\n%s", args, dir, err, res.Stderr)
		}
		return graphWrite{took: took, peak: peak, measured: measured}
	}
	paths := func(on bool) []string {
		if on {
			return []string{"--changed-paths"}
		}
		return nil
	}
	bound := func(filters string) []string {
		if filters == "all" {
			return nil
		}
		return []string{"--max-new-filters=" + filters}
	}
	fresh := func(dir string) {
		linkRepo(b, r.repo, dir, func(rel string) bool { return strings.HasPrefix(rel, "pack/") })
		gittest.SetRefs(b, dir, map[string]string{"refs/heads/main": r.tip()})
	}
	dir := filepath.Join(filepath.Dir(r.repo), shape)
	switch {
	case shape == "on-0" || shape == "off-0":
		fresh(dir)
		r.writes[shape] = write(dir, "", append([]string{"--reachable", "--split=no-merge"},
			paths(shape == "on-0")...)...)
	case shape == "on-base" || shape == "off-base":
		fresh(dir)
		cut := r.commits[len(r.commits)-1-int(compact.PublishTailObjects/r.tree.perCommit)]
		r.writes[shape] = write(dir, cut+"\n", append([]string{"--stdin-commits", "--split=no-merge"},
			paths(shape == "on-base")...)...)
	case strings.HasPrefix(shape, "own-"):
		// Own mode's graphArgs at the bound: one whole graph, never split.
		fresh(dir)
		r.writes[shape] = write(dir, "", append([]string{"--reachable", "--changed-paths"},
			bound(strings.TrimPrefix(shape, "own-"))...)...)
	default:
		filters, on := strings.CutPrefix(shape, "on-tail-")
		base := r.shapedLocked(b, pool, map[bool]string{true: "on-base", false: "off-base"}[on])
		linkRepo(b, base, dir, func(string) bool { return true })
		gittest.SetRefs(b, dir, map[string]string{"refs/heads/main": r.tip()})
		tip := []string{"--reachable", "--split=no-merge"}
		if on {
			tip = append(append(tip, "--changed-paths"), bound(filters)...)
		}
		r.writes[shape] = write(dir, "", tip...)
	}
	r.shapes[shape] = dir
	return dir
}

// linkRepo makes a repository at to the engine's way, holding hard links of
// every file under from's objects/ that keep admits — a link is the same
// file, so a rig outlives the temporary directory of the benchmark that
// fast-imported its history.
func linkRepo(b *testing.B, from, to string, keep func(rel string) bool) {
	b.Helper()
	for _, sub := range []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"} {
		if err := os.MkdirAll(filepath.Join(to, filepath.FromSlash(sub)), 0o700); err != nil {
			b.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"HEAD": "ref: refs/heads/main\n", "config": gitproc.RepoConfig, "packed-refs": "# pack-refs with: sorted \n",
	} {
		if err := os.WriteFile(filepath.Join(to, name), []byte(content), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	objects := filepath.Join(from, "objects")
	err := filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(objects, path)
		if err != nil || !keep(filepath.ToSlash(rel)) {
			return err
		}
		target := filepath.Join(to, "objects", rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.Link(path, target)
	})
	if err != nil {
		b.Fatalf("linking %s's objects into %s: %v", from, to, err)
	}
}
