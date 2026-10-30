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
//     tip write, computing at most that many new filters (100, 1 024 or every
//     one: the bounds cache.MaxNewFilters is chosen from), so every tail
//     commit past them is diffed by the read;
//   - filters=off/… is the same graphs with no filter at all, what every read
//     walked before the filters.
//
// The file blamed and the path logged is gittest.History's first, rewritten
// by one commit in each of its 1 280 versions — a window of it each time, so
// a blame walks back to the last change of each of its lines, and a page of
// the path's log walks some 36 000 commits at the gigabyte; history=1GB is
// the large model's commit count (455 680 commits, gittest.LargeEnv) and
// history=100MB a run's without it. Each read must answer within
// gitproc.ReadDeadline with the filters, the tail included;
// cache.MaxNewFilters is the smallest of the three that keeps both under half
// of it (x-deadline under 0.5), and estate.GitReadAttempt is re-checked
// against the slower of the two.
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

// newFilterBounds are the bounds on a tip's new changed-path filters the
// tail's arms read through: the candidates internal/code/cache's
// MaxNewFilters is chosen from, whichever it is now, and "all" for none.
var newFilterBounds = []string{"100", "1024", "all"}

// historyObjectsPerCommit is what one commit of gittest.History adds: the
// commit, the root tree, its directory's tree and four blobs.
const historyObjectsPerCommit = 7

// historyPath is the file every arm blames and logs: gittest.History's
// first, rewritten once in every version of every session.
const historyPath = "d000/f00000"

// historyReads runs one read, read(path, tip) its arguments, over every
// shape of every size.
func historyReads(b *testing.B, read func(path, tip string) []string) {
	codetest.Quiet(b)
	tail := compact.PublishTailObjects
	for _, size := range historySizes {
		b.Run("history="+size.name, func(b *testing.B) {
			if size.large {
				gittest.Large(b, "a 1 GB history fast-imported, its commit-graph written six ways")
			}
			rig := historyRigOf(b, size.name, size.bytes)
			pool, err := gitproc.New(b.Context(), gitproc.Options{Home: filepath.Join(b.TempDir(), "home"),
				MaxProcesses: runtime.NumCPU(), Memory: 1 << 40})
			if err != nil {
				b.Fatal(err)
			}
			arms := []struct{ name, shape string }{
				{"filters=on/tail=0", "on-0"},
				{"filters=off/tail=0", "off-0"},
				{fmt.Sprintf("filters=off/tail=%d", tail), "off-tail"},
			}
			for _, filters := range newFilterBounds {
				arms = append(arms, struct{ name, shape string }{
					fmt.Sprintf("filters=on/tail=%d/max-new-filters=%s", tail, filters), "on-tail-" + filters})
			}
			for _, arm := range arms {
				b.Run(arm.name, func(b *testing.B) {
					dir := rig.shaped(b, pool, arm.shape)
					for b.Loop() {
						if res, err := pool.Run(b.Context(), gitproc.Cmd{Dir: dir, Args: read(historyPath, rig.tip()),
							Config: []gitproc.KV{{Key: "commitGraph.changedPathsVersion", Value: "2"},
								{Key: "mailmap.blob", Value: ""}},
							Stdout: io.Discard, Class: gitproc.ClassRead, Deadline: time.Hour}); err != nil {
							b.Fatalf("git %s: %v\n%s", arm.name, err, res.Stderr)
						}
					}
					took := b.Elapsed().Seconds() / float64(b.N)
					b.ReportMetric(took/gitproc.ReadDeadline.Seconds(), "x-deadline")
					b.ReportMetric(float64(len(rig.commits)), "commits")
				})
			}
		})
	}
}

// historyRig is one size's history, built ONCE A PROCESS for both reads'
// benchmarks — at the gigabyte, minutes of fast-import and of graph writes —
// in a directory TestMain removes when the binary ends, since the
// benchmark that built it ends before the other reads it.
type historyRig struct {
	// repo holds hard links of the history's packs and main at its tip.
	repo    string
	commits []string
	// shapes is each graph shape's repository, written once.
	shapes map[string]string
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

// historyRigOf is size's rig, built by the first benchmark that asks.
func historyRigOf(b *testing.B, name string, bytes int64) *historyRig {
	b.Helper()
	historyRigs.Lock()
	defer historyRigs.Unlock()
	if r := historyRigs.m[name]; r != nil {
		return r
	}
	root, err := os.MkdirTemp("", "crewlet-history-"+name+"-")
	if err != nil {
		b.Fatal(err)
	}
	historyRigs.dirs = append(historyRigs.dirs, root)
	src, commits := gittest.History(b, gittest.NewClient(b), bytes)
	r := &historyRig{repo: filepath.Join(root, "repo"), commits: commits, shapes: map[string]string{}}
	linkRepo(b, src, r.repo, func(rel string) bool { return strings.HasPrefix(rel, "pack/") })
	gittest.SetRefs(b, r.repo, map[string]string{"refs/heads/main": r.tip()})
	historyRigs.m[name] = r
	return r
}

// tip is main's commit.
func (r *historyRig) tip() string { return r.commits[len(r.commits)-1] }

// shaped is the repository of shape, its graphs written by the first arm
// that asks, on pool: on-0 and off-0 one layer over every commit, with every
// filter and with none; on-tail-<filters> and off-tail the published layer
// over every commit but the tail's — written once for the three tips over
// it — and the node's tip over the tail, computing at most filters new
// filters ("all": every one) or none.
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
	write := func(dir string, stdin string, args ...string) {
		b.Helper()
		cmd := gitproc.Cmd{Dir: dir, Args: append(append([]string{"commit-graph", "write"}, args...), "--no-progress"),
			Config: []gitproc.KV{{Key: "commitGraph.changedPathsVersion", Value: "2"}},
			Class:  gitproc.ClassMaintenance, Deadline: time.Hour}
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		if res, err := pool.Run(b.Context(), cmd); err != nil {
			b.Fatalf("git commit-graph write %v in %s: %v\n%s", args, dir, err, res.Stderr)
		}
	}
	paths := func(on bool) []string {
		if on {
			return []string{"--changed-paths"}
		}
		return nil
	}
	dir := filepath.Join(filepath.Dir(r.repo), shape)
	switch shape {
	case "on-0", "off-0":
		linkRepo(b, r.repo, dir, func(rel string) bool { return strings.HasPrefix(rel, "pack/") })
		gittest.SetRefs(b, dir, map[string]string{"refs/heads/main": r.tip()})
		write(dir, "", append([]string{"--reachable", "--split=no-merge"}, paths(shape == "on-0")...)...)
	case "on-base", "off-base":
		linkRepo(b, r.repo, dir, func(rel string) bool { return strings.HasPrefix(rel, "pack/") })
		gittest.SetRefs(b, dir, map[string]string{"refs/heads/main": r.tip()})
		cut := r.commits[len(r.commits)-1-int(compact.PublishTailObjects/historyObjectsPerCommit)]
		write(dir, cut+"\n", append([]string{"--stdin-commits", "--split=no-merge"}, paths(shape == "on-base")...)...)
	default:
		filters, on := strings.CutPrefix(shape, "on-tail-")
		base := r.shapedLocked(b, pool, map[bool]string{true: "on-base", false: "off-base"}[on])
		linkRepo(b, base, dir, func(string) bool { return true })
		gittest.SetRefs(b, dir, map[string]string{"refs/heads/main": r.tip()})
		tip := []string{"--reachable", "--split=no-merge"}
		if on {
			tip = append(tip, "--changed-paths")
			if filters != "all" {
				tip = append(tip, "--max-new-filters="+filters)
			}
		}
		write(dir, "", tip...)
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
