package cache_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	rtmetrics "runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE PUBLISHED LAYERS, MEASURED: what git costs a maintainer to publish a
// repository's bitmapped multi-pack-index as layers, what a node pays to install
// and serve them, and each own-mode write beside them. The constants of cache and
// compact are re-derived from these figures on every pin.
//
// Each step is the production command on a pool run as the node's (pack.threads
// at one, its serve configuration), over a scratch of hard links to a synced
// copy's packs, relinked for every run of a writing step; git only creates or
// renames files, so the linked originals stay intact. Own mode's and the tip's
// arguments are cache.DerivedFiles'; the publisher's copy compact's writeLayerIn
// and pack, so change both together. Figures: s/op and, where /proc allows, the
// git's peak anonymous memory (gittest.PeakAnon) in MiB, B/obj-repo and B/obj-new.

// gitPin and gitFloor name an arm's git: the pin, or the node floor's build
// (gittest.NodeFloor), so an operator on the floor reads what publishing costs
// there.
const (
	gitPin   = "pin"
	gitFloor = "floor"
)

// bigNodeEnv set to 1 beside gittest.LargeEnv runs BenchmarkBigNode's 12 GB
// arm: its fixture builds for hours and needs 64 GB of memory to hold its bitmap
// beside the in-memory store, which no default run or CI job has.
const bigNodeEnv = "CREWLET_BENCH_BIGNODE"

// steps is an arm's runs of one git step: how many, their wall clock, and
// the most anonymous memory any of them held.
type steps struct {
	n        int
	took     time.Duration
	peak     int64
	measured bool
}

// run runs cmd on pool, timed and its memory sampled, failing b on an error.
func (s *steps) run(b *testing.B, pool *gitproc.Pool, cmd gitproc.Cmd) gitproc.Result {
	b.Helper()
	res, err := s.try(b, pool, cmd)
	if err != nil {
		b.Fatalf("git %s in %s: %v\n%s", strings.Join(cmd.Args, " "), cmd.Dir, err, res.Stderr)
	}
	return res
}

// try is run answering git's failure: a step whose failure is its figure.
func (s *steps) try(b *testing.B, pool *gitproc.Pool, cmd gitproc.Cmd) (gitproc.Result, error) {
	b.Helper()
	var (
		res  gitproc.Result
		err  error
		took time.Duration
	)
	peak, ok := gittest.PeakAnon(func() {
		began := time.Now()
		res, err = pool.Run(b.Context(), cmd)
		//fixedwait:measure a step's wall clock, reported; root-write's mean also caps the stacked arm (cap-s), whose crossing is reported as pathological and fails nothing
		took = time.Since(began)
	})
	s.n++
	s.took += took
	s.peak, s.measured = max(s.peak, peak), ok
	return res, err
}

// mean is a run's wall clock, on average.
func (s *steps) mean() time.Duration {
	if s.n == 0 {
		return 0
	}
	return s.took / time.Duration(s.n)
}

// report states s's figures, each unit under prefix ("" for the arm's one
// step): its seconds a run, and where its memory was measured, the peak in
// MiB, over the repository's objects, and over the new objects (none where
// fresh is 0).
func (s *steps) report(b *testing.B, prefix string, repo, fresh int64) {
	b.Helper()
	if s.n == 0 {
		return
	}
	b.ReportMetric(s.mean().Seconds(), prefix+"s/op")
	if !s.measured {
		return
	}
	b.ReportMetric(float64(s.peak)/(1<<20), prefix+"peak-MiB")
	if repo > 0 {
		b.ReportMetric(float64(s.peak)/float64(repo), prefix+"B/obj-repo")
	}
	if fresh > 0 {
		b.ReportMetric(float64(s.peak)/float64(fresh), prefix+"B/obj-new")
	}
}

// declaredArgs is the argument list the cache declares for step when
// trigger runs it (cache.DerivedFiles): own mode's whole index, bitmap and
// graph, and the node's tip.
func declaredArgs(b *testing.B, step cache.Step, trigger cache.Trigger) []string {
	b.Helper()
	for _, d := range cache.DerivedFiles() {
		for _, w := range d.Writes {
			if w.Step == step && w.Trigger == trigger && w.Args != nil {
				return slices.Clone(w.Args)
			}
		}
	}
	b.Fatalf("the cache declares no %s write its %s runs", step, trigger)
	return nil
}

// tipGraphArgs is the node's declared tip write (every filter its commits lack)
// or, at filters >= 0, the same bounded to that many new filters
// (--max-new-filters), to measure the declared write against.
func tipGraphArgs(b *testing.B, filters int) []string {
	b.Helper()
	declared := declaredArgs(b, cache.StepTipGraph, cache.TriggerMaintainer)
	if filters < 0 {
		return declared
	}
	n := len(declared)
	return append(append(slices.Clone(declared[:n-1]), "--max-new-filters="+strconv.Itoa(filters)), declared[n-1])
}

// packList is packs as git's --stdin-packs reads them, ext the file each
// line names: ".idx" for a multi-pack-index or a commit-graph, ".pack" for
// pack-objects.
func packList(packs []string, ext string) string {
	var out strings.Builder
	for _, cs := range packs {
		out.WriteString("pack-" + cs + ext + "\n")
	}
	return out.String()
}

// midxLayerCmd is the publisher's multi-pack-index layer (compact's writeLayerIn)
// over exactly packs in dir: incremental, with its bitmap and lookup table (or
// none, for step=tip-plain), no chain file written, over base ("none" for a
// root; otherwise dir's chain file must name it).
func midxLayerCmd(dir string, packs []string, base string, bitmap bool) gitproc.Cmd {
	args := []string{"multi-pack-index", "write", "--incremental", "--bitmap", "--stdin-packs",
		"--no-write-chain-file", "--base=" + base, "--no-progress"}
	if !bitmap {
		args = slices.DeleteFunc(args, func(a string) bool { return a == "--bitmap" })
	}
	return gitproc.Cmd{Dir: dir, Class: gitproc.ClassMaintenance, Args: args, Stdin: strings.NewReader(packList(packs, ".idx")),
		Config: []gitproc.KV{{Key: "pack.writeBitmapLookupTable", Value: "true"}}, Deadline: time.Hour}
}

// graphLayerCmd is the publisher's commit-graph layer over packs in dir:
// split and never merged, every version-2 changed-path filter computed,
// since it is written once for the fleet (writeLayerIn).
func graphLayerCmd(dir string, packs []string) gitproc.Cmd {
	return gitproc.Cmd{Dir: dir, Class: gitproc.ClassMaintenance,
		Args:   []string{"commit-graph", "write", "--stdin-packs", "--split=no-merge", "--changed-paths", "--no-progress"},
		Stdin:  strings.NewReader(packList(packs, ".idx")),
		Config: []gitproc.KV{{Key: "commitGraph.changedPathsVersion", Value: "2"}}, Deadline: time.Hour}
}

// scratchRepo is an empty repository the engine's way — gittest.Empty's
// shape — made under root rather than in a test's own temporary directory,
// so a fixture one sub-benchmark builds outlives it for the next.
func scratchRepo(b *testing.B, root, name string) string {
	b.Helper()
	dir, err := os.MkdirTemp(root, name+"-") //nolint:usetesting // under the caller's root, outliving this sub-benchmark: b.TempDir() would go with it
	if err != nil {
		b.Fatal(err)
	}
	for _, sub := range []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(sub)), 0o700); err != nil {
			b.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"HEAD": "ref: refs/heads/main\n", "config": gitproc.RepoConfig, "packed-refs": "# pack-refs with: sorted \n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

// linkScratch is a scratch repository under root holding a hard link of
// every file under from's objects/ that keep admits (its path relative to
// objects/, slash-separated): packs, layers and chain files, as an
// incarnation holds them.
func linkScratch(b *testing.B, root, name, from string, keep func(rel string) bool) string {
	b.Helper()
	dir := scratchRepo(b, root, name)
	objects := filepath.Join(from, "objects")
	err := filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(objects, path)
		if err != nil || !keep(filepath.ToSlash(rel)) {
			return err
		}
		to := filepath.Join(dir, "objects", rel)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		return os.Link(path, to)
	})
	if err != nil {
		b.Fatalf("linking %s into a scratch: %v", from, err)
	}
	return dir
}

// packPath is the path of pack cs's file of extension ext in the repository
// at dir.
func packPath(dir, cs, ext string) string {
	return filepath.Join(dir, "objects", "pack", "pack-"+cs+ext)
}

// isPack reports whether rel, a path under objects/, is a file of one of
// packs.
func isPack(rel string, packs map[string]bool) bool {
	name, ok := strings.CutPrefix(rel, "pack/pack-")
	if !ok {
		return false
	}
	cs, _, _ := strings.Cut(name, ".")
	return packs[cs]
}

// isLayer reports whether rel is a file of the multi-pack-index or the
// commit-graph chain.
func isLayer(rel string) bool {
	return strings.HasPrefix(rel, "pack/multi-pack-index.d/") || strings.HasPrefix(rel, "info/commit-graphs/")
}

// linkPacks hard-links each of packs' three files from the repository at
// from into the one at to.
func linkPacks(b *testing.B, from, to string, packs ...string) {
	b.Helper()
	for _, cs := range packs {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			if err := os.Link(packPath(from, cs, ext), packPath(to, cs, ext)); err != nil {
				b.Fatalf("linking pack %s: %v", cs, err)
			}
		}
	}
}

// idxObjects is how many objects the version-2 pack index at path holds:
// the last entry of its fan-out table.
func idxObjects(b *testing.B, path string) int64 {
	b.Helper()
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var last [4]byte
	if _, err := f.ReadAt(last[:], 8+255*4); err != nil {
		b.Fatalf("reading the fan-out of %s: %v", path, err)
	}
	return int64(binary.BigEndian.Uint32(last[:]))
}

// writeMidxChain writes the multi-pack-index chain file of the repository at
// dir: ids, root first, as the cache writes it from the rows.
func writeMidxChain(b *testing.B, dir string, ids ...string) {
	b.Helper()
	write(b, filepath.Join(dir, "objects", "pack", "multi-pack-index.d", "multi-pack-index-chain"),
		strings.Join(ids, "\n")+"\n")
}

// graphTop is the newest commit-graph layer the repository at dir's chain
// file lists.
func graphTop(b *testing.B, dir string) string {
	b.Helper()
	lines := strings.Fields(read(b, filepath.Join(dir, "objects", "info", "commit-graphs", "commit-graph-chain")))
	if len(lines) == 0 {
		b.Fatalf("%s holds no commit-graph chain", dir)
	}
	return lines[len(lines)-1]
}

// setMain points the repository at dir's main at oid.
func setMain(b *testing.B, dir, oid string) {
	b.Helper()
	gittest.SetRefs(b, dir, map[string]string{"refs/heads/main": oid})
}

// removeAll removes a scratch.
func removeAll(b *testing.B, dir string) {
	b.Helper()
	if err := os.RemoveAll(dir); err != nil {
		b.Fatal(err)
	}
}

// layered is a repository at dir holding a published chain of one layer
// over its packs: the multi-pack-index layer's id and the commit-graph
// layer's.
type layered struct {
	dir, layer, graph string
}

// publishRoot writes the maintainer's ROOT publication over packs in dir (the
// multi-pack-index layer over no base, its chain file, its commit-graph layer)
// and answers it. main is set to tip, the packs' newest commit, first: a bitmap
// covers the commits the refs reach.
func publishRoot(b *testing.B, pool *gitproc.Pool, dir, tip string, packs []string) layered {
	b.Helper()
	setMain(b, dir, tip)
	var s steps
	id := strings.TrimSpace(string(s.run(b, pool, midxLayerCmd(dir, packs, "none", true)).Stdout))
	writeMidxChain(b, dir, id)
	s.run(b, pool, graphLayerCmd(dir, packs))
	return layered{dir: dir, layer: id, graph: graphTop(b, dir)}
}

// floorPool is a pool over the node floor's own build, made once an arm
// runs it.
func floorPool(b *testing.B) *gitproc.Pool {
	b.Helper()
	pool, err := gitproc.New(b.Context(), gitproc.Options{Path: gittest.NodeFloor(b),
		Home: filepath.Join(b.TempDir(), "home"), MaxProcesses: runtime.NumCPU(), Memory: 1 << 40})
	if err != nil {
		b.Fatalf("a pool over the node floor's git: %v", err)
	}
	return pool
}

// tailPush is one push of a fixture's tail: its commit, its pack and the
// objects the pack holds.
type tailPush struct {
	commit, pack string
	objects      int64
}

// layerArms is BenchmarkMidx's published-layer arms over one history: its 64
// packs as synced, their source, and the shared fixtures, each built by the first
// arm needing it (a -bench pattern selecting none builds none) under a directory
// the size's benchmark holds, so it outlives that arm.
type layerArms struct {
	w       *syncWorld
	c       *cache.Cache
	bytes   int64
	src     string
	commits []string
	// hist is the history's packs, one a push, oldest first; tips each push's
	// newest commit; objs every pack's objects, the tail's among them.
	hist []string
	tips []string
	objs map[string]int64
	// first is the index into commits of each push's oldest commit.
	first []int
	root  string
	// packs is a scratch of the history's packs, linked when the size begins:
	// fixtures link from here, since later arms replace the synced incarnation.
	packs string

	chained   *layered
	tail      []tailPush
	merged    *mergeChain
	floor     *gitproc.Pool
	rootWrite map[string]time.Duration
}

// newLayerArms is the published layers' arms over the history synced in s —
// its rows, its commits, src the history they were pushed from.
func newLayerArms(b *testing.B, w *syncWorld, c *cache.Cache, s cache.Synced, src string, commits []string,
	rows []gitlet.PackRow, bytes int64) *layerArms {

	b.Helper()
	p := &layerArms{w: w, c: c, bytes: bytes, src: src, commits: commits, objs: map[string]int64{}, root: b.TempDir(),
		rootWrite: map[string]time.Duration{}}
	for i, push := range splitPushes(commits, gittest.HistorySessions) {
		p.hist = append(p.hist, rows[i].GitChecksum)
		p.tips = append(p.tips, push.tip)
		p.objs[rows[i].GitChecksum] = rows[i].Objects
		p.first = append(p.first, i*len(commits)/gittest.HistorySessions)
	}
	p.packs = scratchRepo(b, p.root, "packs")
	linkPacks(b, s.Dir, p.packs, p.hist...)
	return p
}

// objectsOf is the objects packs hold.
func (p *layerArms) objectsOf(packs []string) int64 {
	var n int64
	for _, cs := range packs {
		n += p.objs[cs]
	}
	return n
}

// tailPacks is the first k pushes of the tail's packs.
func (p *layerArms) tailPacks(k int) []string {
	out := make([]string, 0, k)
	for _, t := range p.tail[:k] {
		out = append(out, t.pack)
	}
	return out
}

// head is main's commit once k pushes of the tail have landed.
func (p *layerArms) head(k int) string {
	if k == 0 {
		return p.commits[len(p.commits)-1]
	}
	return p.tail[k-1].commit
}

// floorGit is the floor's pool, made once.
func (p *layerArms) floorGit(b *testing.B) *gitproc.Pool {
	b.Helper()
	if p.floor == nil {
		p.floor = floorPool(b)
	}
	return p.floor
}

// poolOf is the pool an arm of git names runs.
func (p *layerArms) poolOf(b *testing.B, git string) *gitproc.Pool {
	b.Helper()
	if git == gitFloor {
		return p.floorGit(b)
	}
	return p.w.pool
}

// chain is THE PUBLISHED FIXTURE: the history under the maintainer's root
// publication plus a tail of pushes outside it, as a node holds a repository
// between two publications. The tail is slices.Max(staleArms) pushes of
// stalePushBytes on a branch of their own, each packed alone as a push is; the
// step= arms take the first compact.PublishPacks, where a publication falls due.
func (p *layerArms) chain(b *testing.B) *layered {
	b.Helper()
	if p.chained != nil {
		return p.chained
	}
	dir := linkScratch(b, p.root, "chained", p.packs, func(string) bool { return true })
	l := publishRoot(b, p.w.pool, dir, p.commits[len(p.commits)-1], p.hist)
	p.tail = p.tailPushes(b, dir, slices.Max(staleArms))
	p.chained = &l
	return p.chained
}

// tailPushes lands n pushes on the history in src on a branch of their own, each
// one file of stalePushBytes seeded random bytes as the stale arms' are, and
// packs each alone into dir.
func (p *layerArms) tailPushes(b *testing.B, dir string, n int) []tailPush {
	b.Helper()
	const branch = "refs/heads/layer-tail"
	base := p.commits[len(p.commits)-1]
	gittest.FastImport(b, p.w.client, p.src, func(bw *bufio.Writer) error {
		for i := range n {
			bytes, _ := chacha(drawsTail, uint64(i))
			msg := fmt.Sprintf("tail %d\n", i)
			fmt.Fprintf(bw, "commit %s\ncommitter %s %d +0000\ndata %d\n%s", branch, benchIdent, nextEpoch+i,
				len(msg), msg)
			if i == 0 {
				fmt.Fprintf(bw, "from %s\n", base)
			}
			fmt.Fprintf(bw, "M 100644 inline tail/t%06d.bin\ndata %d\n", i, stalePushBytes)
			if _, err := io.CopyN(bw, bytes, stalePushBytes); err != nil {
				return err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	})
	commits := strings.Fields(p.w.client.Run(b, p.src, "rev-list", "--reverse", base+".."+branch))
	if len(commits) != n {
		b.Fatalf("the tail is %d commits, want %d", len(commits), n)
	}
	out := make([]tailPush, 0, n)
	prev := base
	for _, commit := range commits {
		cs := gittest.Pack(b, p.w.client, dir, p.src, commit, "^"+prev)
		t := tailPush{commit: commit, pack: cs, objects: idxObjects(b, packPath(dir, cs, ".idx"))}
		p.objs[cs] = t.objects
		out = append(out, t)
		prev = commit
	}
	return out
}

// tailed is a scratch of the published fixture with the first k pushes of
// its tail, the published layers among its files where layers is set.
func (p *layerArms) tailed(b *testing.B, name string, k int, layers bool) string {
	b.Helper()
	l := p.chain(b)
	packs := setOf(append(slices.Clone(p.hist), p.tailPacks(k)...)...)
	dir := linkScratch(b, p.root, name, l.dir, func(rel string) bool {
		return isPack(rel, packs) || (layers && isLayer(rel))
	})
	setMain(b, dir, p.head(k))
	return dir
}

// run is every published-layer arm the size has.
func (p *layerArms) run(b *testing.B) {
	b.Run("step=whole", p.whole)
	b.Run("step=tip", p.tip)
	b.Run("step=tip-plain", p.tipPlain)
	b.Run("step=publish", p.publish)
	b.Run("step=tip-graph", p.tipGraph)
	if p.bytes > 1_000_000_000 {
		p.tails(b)
		return
	}
	b.Run("step=merge", p.merge)
	if p.bytes <= 100_000_000 {
		b.Run("step=closure", p.closure)
	}
	for _, k := range staleArms {
		b.Run(fmt.Sprintf("stale=%d/mode=chain", k), func(b *testing.B) { p.staleChained(b, k) })
	}
	p.shares(b)
	if p.bytes >= 1_000_000_000 {
		p.tails(b)
	}
}

// whole is OWN MODE'S WHOLE REWRITE after compact.PublishPacks tail pushes, as
// cache.DerivedFiles declares it: the multi-pack-index with and without its
// bitmap, and the commit-graph with changed-path filters, each over every pack.
// B/obj-repo decides cache.BitmapBytesPerObject, IndexBytesPerObject and
// GraphBytesPerObject; bitmap's and graph's s/op together, MidxMinInterval and
// MidxDutyCycle; B/obj-new is what step=tip is read against.
func (p *layerArms) whole(b *testing.B) {
	k := compact.PublishPacks
	for _, step := range []cache.Step{cache.StepBitmap, cache.StepIndex, cache.StepGraph} {
		b.Run("write="+string(step), func(b *testing.B) {
			args := declaredArgs(b, step, cache.TriggerMaintainer)
			var s steps
			for b.Loop() {
				b.StopTimer()
				dir := p.tailed(b, "whole", k, false)
				b.StartTimer()
				s.run(b, p.w.pool, gitproc.Cmd{Dir: dir, Args: args, Config: p.c.ServeConfig(),
					Class: gitproc.ClassMaintenance, Deadline: time.Hour})
				b.StopTimer()
				removeAll(b, dir)
				b.StartTimer()
			}
			s.report(b, "", p.objectsOf(p.hist)+p.objectsOf(p.tailPacks(k)), p.objectsOf(p.tailPacks(k)))
		})
	}
}

// tip is a node-local BITMAPPED TIP LAYER over the tail, which no node writes
// (the package doc says why): its memory grows with the history, not its new
// objects, and its time nears step=whole's. A pin under which its peak-MiB holds
// still across sizes re-decides that (CONTRIBUTING's pin checklist).
func (p *layerArms) tip(b *testing.B) {
	k := compact.PublishPacks
	var s steps
	for b.Loop() {
		b.StopTimer()
		dir := p.tailed(b, "tip", k, true)
		b.StartTimer()
		s.run(b, p.w.pool, midxLayerCmd(dir, p.tailPacks(k), p.chained.layer, true))
		b.StopTimer()
		removeAll(b, dir)
		b.StartTimer()
	}
	s.report(b, "", p.objectsOf(p.hist)+p.objectsOf(p.tailPacks(k)), p.objectsOf(p.tailPacks(k)))
}

// tipPlain is the tip WITHOUT a bitmap: op=write follows its new objects, but
// op=count (the canary's walk of every object) then runs with no bitmap, since
// git opens a layer's bitmap only when every layer above has one, which rules
// out the cheap tip too.
func (p *layerArms) tipPlain(b *testing.B) {
	k := compact.PublishPacks
	b.Run("op=write", func(b *testing.B) {
		var s steps
		for b.Loop() {
			b.StopTimer()
			dir := p.tailed(b, "tip-plain", k, true)
			b.StartTimer()
			s.run(b, p.w.pool, midxLayerCmd(dir, p.tailPacks(k), p.chained.layer, false))
			b.StopTimer()
			removeAll(b, dir)
			b.StartTimer()
		}
		s.report(b, "", p.objectsOf(p.hist)+p.objectsOf(p.tailPacks(k)), p.objectsOf(p.tailPacks(k)))
	})
	b.Run("op=count", func(b *testing.B) {
		dir := p.tailed(b, "tip-plain", k, true)
		defer removeAll(b, dir)
		var write steps
		id := strings.TrimSpace(string(write.run(b, p.w.pool,
			midxLayerCmd(dir, p.tailPacks(k), p.chained.layer, false)).Stdout))
		writeMidxChain(b, dir, p.chained.layer, id)
		var s steps
		for b.Loop() {
			s.run(b, p.w.pool, gitproc.Cmd{Dir: dir, Config: p.c.ServeConfig(), Class: gitproc.ClassRead,
				Deadline: time.Hour, Args: []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "--all"}})
		}
		s.report(b, "", 0, 0)
	})
}

// publish is ONE PUBLICATION'S BUILD, each part its own figure: part=repack
// merges the non-full packs (over compact.LayerRepackPacks) as compact does,
// charged by its inputs (cache.CompactBytesPerObject); part=midx, the layer and
// bitmap, by the repository's objects (cache.PublishBytesPerObject); part=graph,
// the graph layer with every filter, by the layer's (GraphLayerBytesPerObject).
//
//   - layer=root, the FIRST publication, merges the whole history under a root
//     layer: the graph's and repack's per-object figures. git=floor runs it too.
//   - layer=tail, every later one at its due by packs, merges compact.PublishPacks
//     tail pushes onto the chain; its bitmap still walks the whole history, and
//     its part=midx B/obj-repo decides cache.PublishTailBytesPerObject.
func (p *layerArms) publish(b *testing.B) {
	for _, git := range []string{gitPin, gitFloor} {
		b.Run("layer=root/git="+git, func(b *testing.B) {
			p.published(b, git, p.hist, func(b *testing.B) (string, string) {
				return linkScratch(b, p.root, "publish", p.packs, func(string) bool { return true }), "none"
			}, p.commits[len(p.commits)-1])
		})
	}
	k := compact.PublishPacks
	b.Run("layer=tail/git="+gitPin, func(b *testing.B) {
		p.chain(b)
		p.published(b, gitPin, p.tailPacks(k), func(b *testing.B) (string, string) {
			return p.tailed(b, "publish", k, true), p.chained.layer
		}, p.head(k))
	})
}

// published runs one publication shape on git's pool: inputs merged, then the
// layer and its graph over the output, in fixture's scratch (every pack the plan
// was made over) on the base it names, main at tip.
func (p *layerArms) published(b *testing.B, git string, inputs []string, fixture func(*testing.B) (string, string),
	tip string) {

	b.Helper()
	pool := p.poolOf(b, git)
	var repack, midx, graph steps
	var outObjects int64
	for b.Loop() {
		b.StopTimer()
		dir, base := fixture(b)
		setMain(b, dir, tip)
		store := linkScratch(b, p.root, "store", dir, func(rel string) bool { return isPack(rel, setOf(inputs...)) })
		merge := scratchRepo(b, p.root, "merge")
		write(b, filepath.Join(merge, "objects", "info", "alternates"), filepath.Join(store, "objects")+"\n")
		b.StartTimer()
		res := repack.run(b, pool, gitproc.Cmd{Dir: merge, Alternate: store, Class: gitproc.ClassMaintenance,
			Args: []string{"pack-objects", "--stdin-packs",
				"--max-pack-size=" + strconv.FormatInt(gitlet.CompactPackBytes, 10), "--delta-base-offset",
				"--quiet", "objects/pack/pack"},
			Stdin:    strings.NewReader(packList(inputs, ".pack")),
			Config:   compact.PackConfig(gitlet.DefaultLimits().MaxBlobBytes),
			Deadline: time.Hour})
		b.StopTimer()
		outs := strings.Fields(string(res.Stdout))
		linkPacks(b, merge, dir, outs...)
		outObjects = 0
		for _, cs := range outs {
			outObjects += idxObjects(b, packPath(dir, cs, ".idx"))
		}
		b.StartTimer()
		midx.run(b, pool, midxLayerCmd(dir, outs, base, true))
		graph.run(b, pool, graphLayerCmd(dir, outs))
		b.StopTimer()
		for _, d := range []string{dir, store, merge} {
			removeAll(b, d)
		}
		b.StartTimer()
	}
	// The repository the layer is over: every pack outside the inputs, and
	// the output that replaces them.
	repo := outObjects
	for _, cs := range p.hist {
		if !slices.Contains(inputs, cs) {
			repo += p.objs[cs]
		}
	}
	repack.report(b, "repack-", repo, p.objectsOf(inputs))
	midx.report(b, "midx-", repo, outObjects)
	graph.report(b, "graph-", repo, outObjects)
}

// tipGraph is THE NODE'S TIP COMMIT-GRAPH over compact.PublishPacks tail pushes
// on the published graph layer: the declared write (filters=all) beside it
// bounded to 100 and 1 024 new filters. A small tail's figure is the write's
// fixed part, cache.GraphBaseBytes, charged beside the tail= arms' slope with the
// delta base cache's cap (internal/gitlet/query's tree=deep arms measure it filled).
func (p *layerArms) tipGraph(b *testing.B) {
	k := compact.PublishPacks
	for _, filters := range []int{100, 1024, -1} {
		name := strconv.Itoa(filters)
		if filters < 0 {
			name = "all"
		}
		b.Run("filters="+name, func(b *testing.B) {
			args := tipGraphArgs(b, filters)
			var s steps
			for b.Loop() {
				b.StopTimer()
				dir := p.tailed(b, "tip-graph", k, true)
				b.StartTimer()
				s.run(b, p.w.pool, gitproc.Cmd{Dir: dir, Args: args, Config: p.c.ServeConfig(),
					Class: gitproc.ClassMaintenance, Deadline: time.Hour})
				b.StopTimer()
				removeAll(b, dir)
				b.StartTimer()
			}
			s.report(b, "", p.objectsOf(p.hist)+p.objectsOf(p.tailPacks(k)), p.objectsOf(p.tailPacks(k)))
		})
	}
}

// mergeChain is the merge arms' chain: three layers over the history's packs
// — L1 over its first 32, L2 over the next 24, L3 over the last 8 — in the
// repository at dir.
type mergeChain struct {
	dir    string
	layers [3]string
}

// mergeCuts are where the merge chain's layers end, in the history's packs.
var mergeCuts = [3]int{32, 56, 64}

// mergeFixture is the merge chain, built once.
func (p *layerArms) mergeFixture(b *testing.B) *mergeChain {
	b.Helper()
	if p.merged != nil {
		return p.merged
	}
	m := &mergeChain{dir: linkScratch(b, p.root, "merge-chain", p.packs, func(string) bool { return true })}
	base, from := "none", 0
	var s steps
	for i, cut := range mergeCuts {
		setMain(b, m.dir, p.tips[cut-1])
		m.layers[i] = strings.TrimSpace(string(s.run(b, p.w.pool,
			midxLayerCmd(m.dir, p.hist[from:cut], base, true)).Stdout))
		writeMidxChain(b, m.dir, m.layers[:i+1]...)
		base, from = m.layers[i], cut
	}
	p.merged = m
	return m
}

// merge weighs merging two published layers; compact writes a layer over exactly
// its base and never runs `multi-pack-index compact`. merge=root-compact is git's
// compact of L1 and L2 from the root, reusing their bitmaps; merge=root-write
// writes their 56 packs as a root layer, as a publication does; merge=stacked
// (100 MB, pin and floor) writes L2's and L3's packs over L1 with L2 and L3 still
// in the chain file, which the publisher's scratch (chain files naming exactly
// its base) exists to avoid, capped at ten times root-write and reported
// pathological=1 at the cap. root-compact beating root-write by more than the
// tail write it cannot skip, or stacked no longer pathological, re-decides the
// publisher's rule (CONTRIBUTING's pin checklist).
func (p *layerArms) merge(b *testing.B) {
	b.Run("merge=root-compact", func(b *testing.B) {
		m := p.mergeFixture(b)
		var s steps
		for b.Loop() {
			b.StopTimer()
			dir := linkScratch(b, p.root, "root-compact", m.dir, func(string) bool { return true })
			setMain(b, dir, p.tips[mergeCuts[1]-1])
			b.StartTimer()
			s.run(b, p.w.pool, gitproc.Cmd{Dir: dir, Class: gitproc.ClassMaintenance, Deadline: time.Hour,
				Config: []gitproc.KV{{Key: "pack.writeBitmapLookupTable", Value: "true"}},
				Args:   []string{"multi-pack-index", "compact", "--incremental", "--bitmap", m.layers[0], m.layers[1]}})
			b.StopTimer()
			removeAll(b, dir)
			b.StartTimer()
		}
		s.report(b, "", p.objectsOf(p.hist), p.objectsOf(p.hist[:mergeCuts[1]]))
	})
	for _, git := range []string{gitPin, gitFloor} {
		b.Run("merge=root-write/git="+git, func(b *testing.B) {
			var s steps
			for b.Loop() {
				p.rootWriteOnce(b, git, &s)
			}
			p.rootWrite[git] = s.mean()
			// The scratch holds L1's and L2's packs alone: they are the
			// repository the root layer is over.
			s.report(b, "", p.objectsOf(p.hist[:mergeCuts[1]]), p.objectsOf(p.hist[:mergeCuts[1]]))
		})
	}
	if p.bytes > 100_000_000 {
		return
	}
	for _, git := range []string{gitPin, gitFloor} {
		b.Run("merge=stacked/git="+git, func(b *testing.B) { p.stacked(b, git) })
	}
}

// rootWriteOnce is one run of merge=root-write on git's pool, into s: a root
// layer over L1's and L2's packs, written as a publication writes it, in a
// scratch of their own. Only the write is s's figure.
func (p *layerArms) rootWriteOnce(b *testing.B, git string, s *steps) {
	b.Helper()
	pool := p.poolOf(b, git)
	packs := setOf(p.hist[:mergeCuts[1]]...)
	dir := linkScratch(b, p.root, "root-write", p.packs, func(rel string) bool { return isPack(rel, packs) })
	setMain(b, dir, p.tips[mergeCuts[1]-1])
	s.run(b, pool, midxLayerCmd(dir, p.hist[:mergeCuts[1]], "none", true))
	removeAll(b, dir)
}

// stacked is merge=stacked on git's pool, capped at ten times git's
// root-write — written once first, untimed, where this arm runs without it.
func (p *layerArms) stacked(b *testing.B, git string) {
	m := p.mergeFixture(b)
	pool := p.poolOf(b, git)
	if p.rootWrite[git] == 0 {
		var base steps
		p.rootWriteOnce(b, git, &base)
		p.rootWrite[git] = base.mean()
	}
	limit := 10 * p.rootWrite[git]
	var s steps
	pathological := 0
	for b.Loop() {
		b.StopTimer()
		dir := linkScratch(b, p.root, "stacked", m.dir, func(string) bool { return true })
		setMain(b, dir, p.commits[len(p.commits)-1])
		cmd := midxLayerCmd(dir, p.hist[mergeCuts[0]:], m.layers[0], true)
		cmd.Deadline = limit
		b.StartTimer()
		res, err := s.try(b, pool, cmd)
		b.StopTimer()
		// AN IF, NEVER A SWITCH ON A CALL, in a b.Loop body: Go 1.27.2's b.Loop
		// pass (cmd/compile/internal/bloop) rewrites a call in a case clause
		// into an assignment, and the compiler fails with an internal error.
		if errors.Is(err, gitproc.ErrDeadline) {
			pathological = 1
		} else if err != nil {
			b.Fatalf("the stacked write: %v\n%s", err, res.Stderr)
		}
		removeAll(b, dir)
		b.StartTimer()
	}
	s.report(b, "", p.objectsOf(p.hist), p.objectsOf(p.hist[mergeCuts[0]:]))
	b.ReportMetric(float64(pathological), "pathological")
	b.ReportMetric(limit.Seconds(), "cap-s")
}

// closure is the closure git holds a bitmap to, on the pin and the floor: a
// bitmapped layer over every history pack but the first leaves its commits
// reaching objects outside it, so git refuses it ("Packfile doesn't have full
// closure"). That is why a published layer covers EVERY live pack its base does
// not (compact's planLayer, decide rule 6d); a git that writes it re-decides
// that rule and fails the arm.
func (p *layerArms) closure(b *testing.B) {
	for _, git := range []string{gitPin, gitFloor} {
		b.Run("git="+git, func(b *testing.B) {
			pool := p.poolOf(b, git)
			packs := setOf(p.hist[1:]...)
			var s steps
			for b.Loop() {
				b.StopTimer()
				dir := linkScratch(b, p.root, "closure", p.packs, func(rel string) bool { return isPack(rel, packs) })
				setMain(b, dir, p.commits[len(p.commits)-1])
				b.StartTimer()
				res, err := s.try(b, pool, midxLayerCmd(dir, p.hist[1:], "none", true))
				b.StopTimer()
				id := strings.TrimSpace(string(res.Stdout))
				bitmap := filepath.Join(dir, "objects", "pack", "multi-pack-index.d", "multi-pack-index-"+id+".bitmap")
				if _, statErr := os.Stat(bitmap); err == nil && id != "" && statErr == nil {
					b.Fatalf("git %s wrote a bitmapped layer %s over packs that are not closed: the rule that a "+
						"layer covers every live pack its base does not (internal/gitlet/compact) is to be re-decided",
						pool.Version(), id)
				}
				removeAll(b, dir)
				b.StartTimer()
			}
			b.ReportMetric(1, "refused")
		})
	}
}

// staleChained is the stale arms over the PUBLISHED chain, k pushes outside it
// read through the root layer's bitmap: op=count counts main's objects (a
// clone's walk), op=clone serves main's clone. compact.PublishPacks weighs this
// price against a build, as the own-mode arms price cache.MidxPacks.
func (p *layerArms) staleChained(b *testing.B, k int) {
	dir := p.tailed(b, "stale", k, true)
	defer removeAll(b, dir)
	b.Run("op=count", func(b *testing.B) {
		for b.Loop() {
			if _, err := p.w.pool.Run(b.Context(), gitproc.Cmd{Dir: dir, Config: p.c.ServeConfig(),
				Class: gitproc.ClassRead, Deadline: time.Hour,
				Args: []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "refs/heads/main"}}); err != nil {
				b.Fatalf("counting main's objects: %v", err)
			}
		}
	})
	b.Run("op=clone", func(b *testing.B) {
		for b.Loop() {
			packOut(b, p.w.pool, p.c, dir, "refs/heads/main")
		}
	})
}

// shareArms are the share= arms: the history's last n packs outside the
// published chain — none, a sixty-fourth, a sixteenth and a quarter of its
// objects (gittest.History's pushes are alike).
var shareArms = []struct {
	name string
	n    int
}{{"0", 0}, {"1of64", 1}, {"1of16", 4}, {"1of4", 16}}

// shares is the SHARE of a repository's objects its tail may hold before a
// publication is due (compact.PublishShare): a root layer over all but the last
// n packs, walked by op=clone (main) and op=fetch (main's newest commit). The
// largest share whose clone is at most a tenth past share=0's decides it.
func (p *layerArms) shares(b *testing.B) {
	for _, arm := range shareArms {
		b.Run("share="+arm.name, func(b *testing.B) {
			cut := len(p.hist) - arm.n
			packs := setOf(p.hist...)
			dir := linkScratch(b, p.root, "share", p.packs, func(rel string) bool { return isPack(rel, packs) })
			defer removeAll(b, dir)
			publishRoot(b, p.w.pool, dir, p.tips[cut-1], p.hist[:cut])
			setMain(b, dir, p.commits[len(p.commits)-1])
			tail := p.objectsOf(p.hist[cut:])
			b.Run("op=clone", func(b *testing.B) {
				for b.Loop() {
					packOut(b, p.w.pool, p.c, dir, "refs/heads/main")
				}
				b.ReportMetric(float64(tail), "tail-objects")
			})
			b.Run("op=fetch", func(b *testing.B) {
				for b.Loop() {
					packOut(b, p.w.pool, p.c, dir, "refs/heads/main", "^refs/heads/main~1")
				}
			})
		})
	}
}

// tailArms are the tail= arms: compact.PublishTailObjects objects outside the
// chain, a quarter of it and four times it.
var tailArms = []struct {
	name   string
	factor float64
}{{"0.25x", 0.25}, {"1x", 1}, {"4x", 4}}

// tails is THE TAIL IN OBJECTS (1 GB and 5 GB): that many history objects
// outside a root layer and graph layer over the rest, whatever the repository's
// size, and what walks them: op=clone, op=fetch (main's newest commit),
// op=canary (every object through the chain, the installer's read) and
// op=tip-graph (the node's tip: filters=all the declared write, filters=100 it
// bounded). walk-us/object is the canary's and clone's seconds per tail object:
// compact.PublishTailObjects is a second of it at gitproc.WalkPerObject, and is
// re-derived where the canary walks slower. filters=100's B/obj-new across the
// tails is cache.TipGraphBytesPerObject's slope: the bound keeps the delta base
// cache's part small, which filters=all fills toward its cap and
// cache.GraphBaseBytes charges; step=tip-graph gives the write's fixed part.
func (p *layerArms) tails(b *testing.B) {
	for _, arm := range tailArms {
		b.Run("tail="+arm.name, func(b *testing.B) {
			dir, tail := p.tailFixture(b, int64(arm.factor*float64(compact.PublishTailObjects)))
			defer removeAll(b, dir)
			report := func(b *testing.B, s *steps) {
				s.report(b, "", 0, tail)
				b.ReportMetric(s.mean().Seconds()/float64(tail)*1e6, "walk-us/object")
				b.ReportMetric(float64(tail), "tail-objects")
			}
			served := func(op string, args []string, stdin string) {
				b.Run("op="+op, func(b *testing.B) {
					var s steps
					for b.Loop() {
						cmd := gitproc.Cmd{Dir: dir, Args: args, Config: p.c.ServeConfig(), Class: gitproc.ClassRead,
							Deadline: time.Hour, Stdout: io.Discard}
						if stdin != "" {
							cmd.Stdin = strings.NewReader(stdin)
						}
						s.run(b, p.w.pool, cmd)
					}
					report(b, &s)
				})
			}
			clone := []string{"pack-objects", "--revs", "--stdout", "--delta-base-offset", "-q"}
			served("clone", clone, "refs/heads/main\n")
			served("fetch", clone, "refs/heads/main\n^refs/heads/main~1\n")
			served("canary", []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "--all"}, "")
			for _, filters := range []int{100, -1} {
				name := map[bool]string{true: "all", false: strconv.Itoa(filters)}[filters < 0]
				b.Run("op=tip-graph/filters="+name, func(b *testing.B) {
					args := tipGraphArgs(b, filters)
					var s steps
					for b.Loop() {
						b.StopTimer()
						fresh := linkScratch(b, p.root, "tail-graph", dir, func(string) bool { return true })
						copyRefs(b, dir, fresh)
						b.StartTimer()
						s.run(b, p.w.pool, gitproc.Cmd{Dir: fresh, Args: args, Config: p.c.ServeConfig(),
							Class: gitproc.ClassMaintenance, Deadline: time.Hour})
						b.StopTimer()
						removeAll(b, fresh)
						b.StartTimer()
					}
					report(b, &s)
				})
			}
		})
	}
}

// copyRefs gives the repository at to the packed-refs of the one at from.
func copyRefs(b *testing.B, from, to string) {
	b.Helper()
	write(b, filepath.Join(to, "packed-refs"), read(b, filepath.Join(from, "packed-refs")))
}

// tailFixture is a scratch whose published chain covers all but about want of
// the history's newest objects, at a commit's grain, and answers how many lie
// outside: the pushes wholly in the tail as pushed, the push the cut falls in
// split at its commit into two packs, and a root and graph layer before the cut.
func (p *layerArms) tailFixture(b *testing.B, want int64) (string, int64) {
	b.Helper()
	j, rest := len(p.hist)-1, want
	for j > 0 && p.objs[p.hist[j]] <= rest {
		rest -= p.objs[p.hist[j]]
		j--
	}
	lo, hi := p.first[j], len(p.commits)
	if j+1 < len(p.first) {
		hi = p.first[j+1]
	}
	perCommit := float64(p.objs[p.hist[j]]) / float64(hi-lo)
	c := hi - int(float64(rest)/perCommit)
	c = min(max(c, lo+1), hi)
	dir := linkScratch(b, p.root, "tail", p.packs, func(rel string) bool {
		return isPack(rel, setOf(append(slices.Clone(p.hist[:j]), p.hist[j+1:]...)...))
	})
	chain, outside := slices.Clone(p.hist[:j]), slices.Clone(p.hist[j+1:])
	before := ""
	if j > 0 {
		before = p.tips[j-1]
	}
	cut := p.commits[c-1]
	revs := []string{cut}
	if before != "" {
		revs = append(revs, "^"+before)
	}
	chain = append(chain, gittest.Pack(b, p.w.client, dir, p.src, revs...))
	if c < hi {
		outside = append(outside, gittest.Pack(b, p.w.client, dir, p.src, p.tips[j], "^"+cut))
	}
	var tail int64
	for _, cs := range outside {
		tail += idxObjects(b, packPath(dir, cs, ".idx"))
	}
	publishRoot(b, p.w.pool, dir, cut, chain)
	setMain(b, dir, p.commits[len(p.commits)-1])
	return dir, tail
}

// BenchmarkBigNode weighs one node giving the fleet a repository's bitmap against
// every node writing its own, over the large reference model's monorepo
// (gittest.History's composition, 64 pushes): 1 GB always, 5 GB under
// gittest.LargeEnv, 12 GB under bigNodeEnv too. node=own is an own-mode node's
// whole bitmapped multi-pack-index over every pack; node=publish the root
// publication, its layer and bitmap (midx-) and graph layer with every filter
// (graph-); node=install a cold sync installing that chain before it answers:
// its GETs of packs and parts, own-alloc-MiB (installed), and the seconds to its
// first bitmapped count and clone. 1 GB is the largest default size, whose
// figures the docs carry where no larger arm ran. No constant waits on 12 GB:
// scaling.md labels its figure an extrapolation until a run replaces it.
func BenchmarkBigNode(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name       string
		bytes      int64
		large, big bool
	}{{"1GB", 1_000_000_000, false, false}, {"5GB", 5_000_000_000, true, false},
		{"12GB", 12_000_000_000, true, true}} {
		b.Run("size="+arm.name, func(b *testing.B) {
			if arm.large {
				gittest.Large(b, "a "+arm.name+" history fast-imported, packed, stored and synced")
			}
			if arm.big && os.Getenv(bigNodeEnv) != "1" {
				b.Skipf("%s=1 beside %s=1 runs this arm, on a machine of at least 64 GB of memory: its fixture "+
					"builds for hours and its bitmap holds tens of gigabytes beside the in-memory store",
					bigNodeEnv, gittest.LargeEnv)
			}
			bigNodeArms(b, arm.bytes)
		})
	}
}

// bigNodeArms is BenchmarkBigNode's arms over a history of about bytes.
func bigNodeArms(b *testing.B, bytes int64) {
	b.Helper()
	w := newSyncWorld(b, onMemory)
	w.processes = runtime.NumCPU()
	repo, src, _, rows, _, objects := w.midxRepo(b, bytes)
	removeAll(b, src)
	c := w.open(b, func(o *cache.Options) { o.MaxBytes, o.Logger = 1<<40, nil })
	s := w.sync(b, c, repo)
	bn := &bigNode{w: w, c: c, s: s, repo: repo, rows: rows, objects: objects, root: b.TempDir()}
	b.Run("node=own", bn.own)
	b.Run("node=publish", bn.publish)
	b.Run("node=install", bn.install)
}

// bigNode is BenchmarkBigNode's fixture: the repository synced in s, its
// packs, and the root publication, built by node=publish or by the first arm
// that needs it.
type bigNode struct {
	w       *syncWorld
	c       *cache.Cache
	s       cache.Synced
	repo    string
	rows    []gitlet.PackRow
	objects int64
	root    string
	layer   *gitlet.LayerRef
}

// packs is the repository's packs.
func (n *bigNode) packs() []string {
	out := make([]string, 0, len(n.rows))
	for _, r := range n.rows {
		out = append(out, r.GitChecksum)
	}
	return out
}

// own is node=own: the whole index with its bitmap over every pack.
func (n *bigNode) own(b *testing.B) {
	args := declaredArgs(b, cache.StepBitmap, cache.TriggerMaintainer)
	var s steps
	for b.Loop() {
		b.StopTimer()
		dir := linkScratch(b, n.root, "own", n.s.Dir, func(rel string) bool { return isPack(rel, setOf(n.packs()...)) })
		copyRefs(b, n.s.Dir, dir)
		b.StartTimer()
		s.run(b, n.w.pool, gitproc.Cmd{Dir: dir, Args: args, Config: n.c.ServeConfig(),
			Class: gitproc.ClassMaintenance, Deadline: 24 * time.Hour})
		b.StopTimer()
		removeAll(b, dir)
		b.StartTimer()
	}
	s.report(b, "", n.objects, 0)
	b.ReportMetric(s.mean().Minutes(), "min/op")
}

// publish is node=publish: the root publication, timed by step; the last
// run's layer is kept, laid out and stored, for node=install.
func (n *bigNode) publish(b *testing.B) {
	var midx, graph steps
	for b.Loop() {
		b.StopTimer()
		dir := linkScratch(b, n.root, "publish", n.s.Dir, func(rel string) bool { return isPack(rel, setOf(n.packs()...)) })
		copyRefs(b, n.s.Dir, dir)
		b.StartTimer()
		id := strings.TrimSpace(string(midx.run(b, n.w.pool, midxLayerCmd(dir, n.packs(), "none", true)).Stdout))
		graph.run(b, n.w.pool, graphLayerCmd(dir, n.packs()))
		b.StopTimer()
		n.keep(b, dir, id)
		removeAll(b, dir)
		b.StartTimer()
	}
	midx.report(b, "midx-", n.objects, n.objects)
	graph.report(b, "graph-", n.objects, n.objects)
	b.ReportMetric((midx.mean() + graph.mean()).Minutes(), "min/op")
}

// keep lays out and stores the root layer id written in dir as the row a
// publication would name, and publishes it on the world's log.
func (n *bigNode) keep(b *testing.B, dir, id string) {
	b.Helper()
	ref := storeLayer(b, n.w, dir, id, "", nil)
	n.w.log.publish(n.repo, []gitlet.LayerRef{ref}, nil)
	n.layer = &ref
}

// storeLayer lays out the layer id written in the repository at dir over
// base ("" for a root) and the commit-graph layers below its own, stores its
// parts in w's store and answers its row.
func storeLayer(b *testing.B, w *syncWorld, dir, id, base string, baseGraphs []string) gitlet.LayerRef {
	b.Helper()
	layers := filepath.Join(dir, "objects", "pack", "multi-pack-index.d")
	paths := [3]string{
		filepath.Join(layers, "multi-pack-index-"+id+".midx"),
		filepath.Join(layers, "multi-pack-index-"+id+".bitmap"),
		filepath.Join(dir, "objects", "info", "commit-graphs", "graph-"+graphTop(b, dir)+".graph"),
	}
	ref, err := gitlet.LayLayer(b.Context(), paths[0], paths[1], paths[2], baseGraphs, gitlet.Pieces{})
	if err != nil {
		b.Fatalf("laying out layer %s: %v", id, err)
	}
	ref.BaseID, ref.Git, ref.TookMs = base, w.pool.Version().String(), 1
	if ref.Parts, err = gitlet.StoreLayer(b.Context(), w.store, paths, ref.Sections(), nil); err != nil {
		b.Fatalf("storing layer %s: %v", id, err)
	}
	if err := ref.Check(); err != nil {
		b.Fatalf("layer %s: %v", id, err)
	}
	return ref
}

// install is node=install: a cold sync that installs the published chain
// before it answers, then the first bitmapped count and the first clone —
// the publication written first, untimed, where node=publish did not run.
func (n *bigNode) install(b *testing.B) {
	if n.layer == nil {
		dir := linkScratch(b, n.root, "publish", n.s.Dir, func(rel string) bool {
			return isPack(rel, setOf(n.packs()...))
		})
		copyRefs(b, n.s.Dir, dir)
		var s steps
		id := strings.TrimSpace(string(s.run(b, n.w.pool, midxLayerCmd(dir, n.packs(), "none", true)).Stdout))
		s.run(b, n.w.pool, graphLayerCmd(dir, n.packs()))
		n.keep(b, dir, id)
		removeAll(b, dir)
	}
	installed(b, n.w, n.repo, cache.FetchConcurrency, *n.layer)
}

// installed is a cold sync of repo by a fresh cache over w, which installs the
// published chain by its full round before it answers, then the copy's first
// bitmapped count of every object and first clone of main. It reports packs/op
// and parts/op (its GETs), count-s and clone-s, and on the in-memory store
// own-alloc-MiB: heap allocations during the sync less the bytes the store
// served (it copies each object once), bounding what the sync's own code held at
// once, which heap growth (the store's copies until a collection) does not. Over
// the S3 fake the client's buffers are no copy a byte, so it is not reported.
func installed(b *testing.B, w *syncWorld, repo string, fetch int, chain ...gitlet.LayerRef) {
	b.Helper()
	parts := map[objstore.Key]bool{}
	for _, l := range chain {
		for _, p := range l.Parts {
			parts[p.Object] = true
		}
	}
	got := &getCounter{inner: w.store, parts: parts}
	root := b.TempDir()
	var count, clone steps
	var own int64
	n := 0
	for b.Loop() {
		b.StopTimer()
		var pool *gitproc.Pool
		c, drop := w.coldCache(b, root, n, fetch, func(o *cache.Options) { o.Objects, pool = got, o.Git })
		n++
		allocs, served := heapAllocs(), got.served()
		b.StartTimer()
		s, err := c.Sync(b.Context(), repo, linearizable)
		b.StopTimer()
		own = max(own, heapAllocs()-allocs-(got.served()-served))
		if err != nil {
			b.Fatalf("a cold sync of a published repository: %v", err)
		}
		if ids := layerIDs(cache.Installed(c, repo)); !slices.Equal(ids, layerIDs(chain)) {
			b.Fatalf("the cold sync's answer holds the chain %v, want %v installed before it answered", ids,
				layerIDs(chain))
		}
		count.run(b, pool, gitproc.Cmd{Dir: s.Dir, Config: c.ServeConfig(), Class: gitproc.ClassRead,
			Deadline: time.Hour, Args: []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "--all"}})
		began := time.Now()
		packOut(b, pool, c, s.Dir, "refs/heads/main")
		//fixedwait:measure the clone's wall clock, the benchmark's reported figure, held to no bound
		clone.n, clone.took = clone.n+1, clone.took+time.Since(began)
		s.Release()
		drop()
		b.StartTimer()
	}
	gets, partGets := got.totals()
	b.ReportMetric(float64(gets-partGets)/float64(b.N), "packs/op")
	b.ReportMetric(float64(partGets)/float64(b.N), "parts/op")
	if w.mem != nil {
		b.ReportMetric(float64(own)/(1<<20), "own-alloc-MiB")
	}
	b.ReportMetric(count.mean().Seconds(), "count-s")
	b.ReportMetric(clone.mean().Seconds(), "clone-s")
}

// getCounter is a store whose every GET is counted, and those of the parts
// of a published chain apart, and every byte it serves.
type getCounter struct {
	inner cache.Objects
	parts map[objstore.Key]bool

	mu              sync.Mutex
	gets, partsGets int
	bytes           int64
}

func (g *getCounter) Open(ctx context.Context, obj objstore.Object) (io.ReadCloser, error) {
	g.mu.Lock()
	g.gets++
	if g.parts[obj.Key] {
		g.partsGets++
	}
	g.mu.Unlock()
	r, err := g.inner.Open(ctx, obj)
	if err != nil {
		return nil, err
	}
	return &servedReader{ReadCloser: r, g: g}, nil
}

// totals is every GET so far, and those of the chain's parts.
func (g *getCounter) totals() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gets, g.partsGets
}

// served is every byte read from the store through g so far.
func (g *getCounter) served() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bytes
}

// servedReader counts what is read of one object into its getCounter.
type servedReader struct {
	io.ReadCloser
	g *getCounter
}

func (r *servedReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.g.mu.Lock()
	r.g.bytes += int64(n)
	r.g.mu.Unlock()
	return n, err
}

// allocSample is the Go runtime's count of every byte its heap has
// allocated since the process began.
var allocSample = []rtmetrics.Sample{{Name: "/gc/heap/allocs:bytes"}}

// heapAllocs is every byte the process's heap has allocated so far.
func heapAllocs() int64 {
	s := slices.Clone(allocSample)
	rtmetrics.Read(s)
	return int64(s[0].Value.Uint64())
}

// chainOver writes a chain over the packs of repo's copy in a fresh cache over
// w (each layer, with its graph layer, over the packs from the last cut to its
// own), stores every part, publishes the chain on w's log and answers it. tips is
// each cut's newest commit.
func chainOver(b *testing.B, w *syncWorld, repo string, rows []gitlet.PackRow, cuts []int, tips []string) []gitlet.LayerRef {
	b.Helper()
	c, drop := w.coldCache(b, b.TempDir(), 0, cache.FetchConcurrency)
	defer drop()
	s, err := c.Sync(b.Context(), repo, linearizable)
	if err != nil {
		b.Fatalf("syncing the repository a chain is written over: %v", err)
	}
	defer s.Release()
	packs := make([]string, 0, len(rows))
	for _, r := range rows {
		packs = append(packs, r.GitChecksum)
	}
	dir := linkScratch(b, b.TempDir(), "chain", s.Dir, func(rel string) bool { return isPack(rel, setOf(packs...)) })
	var (
		refs   []gitlet.LayerRef
		ids    []string
		graphs []string
		st     steps
	)
	base, from := "none", 0
	for i, cut := range cuts {
		setMain(b, dir, tips[i])
		id := strings.TrimSpace(string(st.run(b, w.pool, midxLayerCmd(dir, packs[from:cut], base, true)).Stdout))
		ids = append(ids, id)
		writeMidxChain(b, dir, ids...)
		st.run(b, w.pool, graphLayerCmd(dir, packs[from:cut]))
		prior := ""
		if i > 0 {
			prior = ids[i-1]
		}
		refs = append(refs, storeLayer(b, w, dir, id, prior, graphs))
		graphs = append(graphs, graphTop(b, dir))
		base, from = id, cut
	}
	w.log.publish(repo, refs, nil)
	return refs
}

// coldLayered is BenchmarkColdSync's arms of a PUBLISHED repository: its packs
// under one root layer (layers=chain) or twenty-one (layers=chain21, the chain
// the default repo_max_bytes reaches: the first over all but the last twenty,
// each next over one), read by a fresh cache fetching fetch bundles at once that
// installs the chain before it answers. It weighs what the parts add to a cold
// sync's GETs and time beside the same arm with no layers, and that its first
// count and clone read the bitmap. The S3 fake answers the fixture's writes at
// once and the arm's reads at firstByte. rows are the packs, one a push, oldest
// first; tips each push's commit.
func coldLayered(b *testing.B, w *syncWorld, repo string, rows []gitlet.PackRow, tips []string,
	firstByte time.Duration, layers, fetch int) {

	b.Helper()
	w.s3.Latency = 0
	cuts, cutTips := make([]int, 0, layers), make([]string, 0, layers)
	for i := range layers {
		cut := len(rows) - (layers - 1 - i)
		cuts, cutTips = append(cuts, cut), append(cutTips, tips[cut-1])
	}
	chain := chainOver(b, w, repo, rows, cuts, cutTips)
	w.s3.Latency = firstByte
	installed(b, w, repo, fetch, chain...)
}

// catchUp is install=catchup (a restore's catch-up): repos one-small-pack
// repositories synced on one node WITHOUT a chain, then a one-part layer
// published on each, which each copy's delta hands to the installer, whose
// passes are timed until every layer is placed: within a tenth of repos × (first
// byte + placement) ÷ cache.FetchConcurrency. getbound-s is the GETs' part,
// repos × firstByte ÷ FetchConcurrency; placement-ms the rest per repository.
func catchUp(b *testing.B, firstByte time.Duration, repos int) {
	b.Helper()
	w := coldWorld(b, "fake", 0)
	src := gittest.Empty(b)
	gittest.FastImport(b, w.client, src, pushStream(1, func(int) int64 { return medianPush }))
	commits := commitsOf(b, w.client, src, "main")
	rows, _ := w.storePushes(b, src, eachPush(commits))
	one := w.logRepo(0)
	w.log.write(one, rows, map[string]string{"refs/heads/main": commits[0]})
	chain := chainOver(b, w, one, rows, []int{1}, commits)
	ids := make([]string, 0, repos)
	for i := range repos {
		id := w.logRepo(i + 1)
		row := rows[0]
		row.PackID = uuid.Must(uuid.NewV7()).String()
		w.log.write(id, []gitlet.PackRow{row}, map[string]string{"refs/heads/main": commits[0]})
		ids = append(ids, id)
	}
	n := 0
	for b.Loop() {
		b.StopTimer()
		w.s3.Latency = 0
		c, drop := w.coldCache(b, b.TempDir(), n, cache.FetchConcurrency)
		n++
		for _, id := range ids {
			w.log.publish(id, nil, nil)
			s, err := c.Sync(b.Context(), id, linearizable)
			if err != nil {
				b.Fatalf("syncing a repository before its publication: %v", err)
			}
			s.Release()
			w.log.publish(id, chain, nil)
			if s, err = c.Sync(b.Context(), id, linearizable); err != nil {
				b.Fatalf("the delta that hands a layer to the installer: %v", err)
			}
			s.Release()
		}
		if q := len(cache.InstallQueued(c)); q != repos {
			b.Fatalf("the installer holds %d repositories queued, want %d", q, repos)
		}
		w.s3.Latency = firstByte
		b.StartTimer()
		for len(cache.InstallQueued(c)) > 0 {
			cache.InstallPass(c)
		}
		b.StopTimer()
		for _, id := range ids {
			if got := layerIDs(cache.Installed(c, id)); !slices.Equal(got, layerIDs(chain)) {
				b.Fatalf("%s holds %v after the catch-up, want %v", id, got, layerIDs(chain))
			}
		}
		drop()
		b.StartTimer()
	}
	took := b.Elapsed().Seconds() / float64(b.N)
	bound := float64(repos) * firstByte.Seconds() / float64(cache.FetchConcurrency)
	b.ReportMetric(bound, "getbound-s")
	b.ReportMetric(took/bound, "x-getbound")
	b.ReportMetric((took-bound)*float64(cache.FetchConcurrency)/float64(repos)*1e3, "placement-ms")
}

// layerLookups are BenchmarkPackLookup's layers= arms, a chain of that many
// layers over the same history each: among them 21, the default repo_max_bytes'
// chain (gitlet.MaxPublishedLayers' reason), the bound, and twice it.
var layerLookups = []int{1, 8, 21, 32, 64}

// geometricLayers is each layer's count of commits, ROOT FIRST, when a chain of
// layers layers cuts commits commits as compact's planner keeps one: each layer
// compact.LayerSplitFactor times the one above, the top a single commit, the
// root the rest (at least half). Layers past the doublings that fit in half the
// history are one-commit layers on top, as a publisher of only small
// publications leaves them before its top layers merge.
func geometricLayers(commits, layers int) []int {
	doubling, sum := 0, 0
	for size := 1; doubling < layers-1 && sum+size <= commits/2; size *= compact.LayerSplitFactor {
		sum += size
		doubling++
	}
	sizes := []int{commits - sum - (layers - 1 - doubling)}
	for i := doubling - 1; i >= 0; i-- {
		size := 1
		for range i {
			size *= compact.LayerSplitFactor
		}
		sizes = append(sizes, size)
	}
	for range layers - 1 - doubling {
		sizes = append(sizes, 1)
	}
	return sizes
}

// layerLookupArms is BenchmarkPackLookup's layers= arms over the history of
// commits in src: a chain of that many bitmapped layers cut by geometricLayers,
// each one pack of its commits' objects. op=lookup is cat-file --batch-check of
// lookupIDs' ids (ns/lookup), op=count a count of main through the bitmaps. git
// looks an object up from the TOP layer down (bsearch_midx), so a root-held id
// passes every layer, as at least half do here (root-share) and most do in a
// geometric chain; equal layers would understate what depth costs.
// gitlet.MaxPublishedLayers stays unless 32 layers cost over twice one per
// lookup; then it is the longest chain that does not. The first arm that runs
// draws the ids, so a pattern selecting no arm draws none.
func (w *syncWorld) layerLookupArms(b *testing.B, src string, commits []string) {
	b.Helper()
	root := b.TempDir()
	var ids string
	for _, layers := range layerLookups {
		b.Run(fmt.Sprintf("layers=%d", layers), func(b *testing.B) {
			if ids == "" {
				ids = lookupIDs(b, w, src)
			}
			dir := scratchRepo(b, root, "layers")
			defer removeAll(b, dir)
			base, cut := "none", 0
			var (
				chain      []string
				s          steps
				rootN, all int64
			)
			for i, n := range geometricLayers(len(commits), layers) {
				revs := []string{commits[cut+n-1]}
				if cut > 0 {
					revs = append(revs, "^"+commits[cut-1])
				}
				cut += n
				pack := gittest.Pack(b, w.client, dir, src, revs...)
				objects := idxObjects(b, packPath(dir, pack, ".idx"))
				if i == 0 {
					rootN = objects
				}
				all += objects
				setMain(b, dir, commits[cut-1])
				id := strings.TrimSpace(string(s.run(b, w.pool, midxLayerCmd(dir, []string{pack}, base, true)).Stdout))
				chain = append(chain, id)
				writeMidxChain(b, dir, chain...)
				base = id
			}
			setMain(b, dir, commits[len(commits)-1])
			b.Run("op=lookup", func(b *testing.B) {
				for b.Loop() {
					if _, err := w.pool.Run(b.Context(), gitproc.Cmd{Dir: dir, Args: []string{"cat-file", "--batch-check"},
						Stdin: strings.NewReader(ids), Stdout: io.Discard, Class: gitproc.ClassRead,
						Config: []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"}}, Deadline: time.Hour}); err != nil {
						b.Fatalf("looking up %d ids: %v", lookupCount, err)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/lookupCount, "ns/lookup")
				b.ReportMetric(float64(rootN)/float64(all), "root-share")
			})
			b.Run("op=count", func(b *testing.B) {
				for b.Loop() {
					if _, err := w.pool.Run(b.Context(), gitproc.Cmd{Dir: dir, Class: gitproc.ClassRead,
						Config:   []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"}, {Key: "pack.useBitmaps", Value: "true"}},
						Args:     []string{"rev-list", "--objects", "--use-bitmap-index", "--count", "refs/heads/main"},
						Deadline: time.Hour}); err != nil {
						b.Fatalf("counting main's objects: %v", err)
					}
				}
			})
		})
	}
}

// lookupCount is how many ids BenchmarkPackLookup's op=lookup asks of a
// chain.
const lookupCount = 10_000

// lookupIDs is lookupCount ids drawn — the same on every run — from every
// object the history in src holds, one a line.
func lookupIDs(b *testing.B, w *syncWorld, src string) string {
	b.Helper()
	all := strings.Split(strings.TrimSpace(w.client.Run(b, src, "rev-list", "--objects", "--all")), "\n")
	_, draw := chacha(drawsTail, 1<<32)
	var out strings.Builder
	for range lookupCount {
		oid, _, _ := strings.Cut(all[draw.IntN(len(all))], " ")
		out.WriteString(oid + "\n")
	}
	return out.String()
}
