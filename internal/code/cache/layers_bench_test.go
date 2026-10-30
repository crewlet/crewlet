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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE PUBLISHED LAYERS, MEASURED: what git costs a
// repository's maintainer to publish its bitmapped multi-pack-index as
// layers, what a git node pays to install and serve them, and what each of
// the cache's own-mode writes costs beside them — the figures the constants
// of internal/code/cache and internal/code/compact are re-derived from on
// every pin, old value and new in the commit that moves one.
//
// EVERY STEP IS THE PRODUCTION COMMAND, run by a pool as the node runs it —
// its pack.threads at one, its serve configuration — over files laid out as
// the node lays them out: a SCRATCH of hard links of a synced copy's packs,
// where git writes layers and chain files exactly as it writes them in an
// incarnation or a publication's scratch. Own mode's writes and the node's
// tip are the declarations' own argument lists (cache.DerivedFiles), so a
// write's flags cannot drift from what a node runs; the publisher's are
// copied from internal/code/compact's writeLayerIn and pack, argument for
// argument, so a change to either is made to both. A scratch is
// linked again for every run of a step that writes, so a run never meets
// what the one before it wrote — a write is a new file or a rename over one,
// never an edit in place, so a scratch's writes leave the files it links as
// they were.
//
// Each step reports its seconds a run (s/op) and, where Linux's /proc says,
// the most ANONYMOUS memory its one git process held (gittest.PeakAnon, the
// method of every per-object figure): in MiB, over the repository's objects
// (B/obj-repo) — what a charge by the repository's objects must hold — and
// over the objects the step adds (B/obj-new) — what it would charge if it
// followed them.

// gitNodeFloor names the arms that run the node floor's own build
// (gittest.NodeFloor) beside the pin: the one way a figure runs a git other
// than the pin, so an operator on the floor reads what publishing costs
// there.
const (
	gitPin   = "pin"
	gitFloor = "floor"
)

// bigNodeEnv is the variable that, set to 1 beside gittest.LargeEnv, runs
// BenchmarkBigNode's 12 GB arm: a fixture that takes hours to build and a
// machine of at least 64 GB of memory to hold its bitmap beside the
// in-memory store, which no default run and no CI job has.
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

// tipGraphArgs is the node's tip as the cache declares it, computing at most
// filters new changed-path filters (-1: every one): its --max-new-filters
// replaced, or dropped for every filter.
func tipGraphArgs(b *testing.B, filters int) []string {
	b.Helper()
	var out []string
	for _, a := range declaredArgs(b, cache.StepTipGraph, cache.TriggerMaintainer) {
		if !strings.HasPrefix(a, "--max-new-filters=") {
			out = append(out, a)
			continue
		}
		if filters >= 0 {
			out = append(out, "--max-new-filters="+strconv.Itoa(filters))
		}
	}
	return out
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

// midxLayerCmd is the publisher's multi-pack-index layer over packs in dir —
// internal/code/compact's writeLayerIn: incremental, with its bitmap and the
// bitmap's lookup table (or plain, the node-local tip step=tip-plain weighs), over
// exactly the packs on standard input, no chain file written, over base
// ("none" for a root, which the chain file in dir must name otherwise).
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
	dir, err := os.MkdirTemp(root, name+"-")
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

// publishRoot writes the maintainer's ROOT publication over packs in the
// repository at dir — its multi-pack-index layer over no base, the chain
// file naming it, and its commit-graph layer — main at tip, the newest commit
// the packs hold, while git writes it (a bitmap's commits are the refs' it
// reaches), and answers the layer.
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

// layerArms is BenchmarkMidx's arms of the published layers over one
// history: its 64 packs as the synced copy holds them, the source they were
// pushed from, and the fixtures the arms share, each built by the first arm
// that needs it — so a -bench pattern that selects none of them builds none —
// under a directory the size's benchmark holds, so it outlives the arm that
// built it.
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
	// packs is a scratch holding the history's packs, linked when the size
	// begins: every fixture links them from here, since the synced copy's
	// incarnation is replaced by the arms after these.
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
	rows []code.PackRow, bytes int64) *layerArms {

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

// chain is THE PUBLISHED FIXTURE: the history's packs under the maintainer's
// root publication, and a tail of pushes outside it — what a git node holds
// between two publications of a repository pushed to since the last. The
// tail is slices.Max(staleArms) pushes of stalePushBytes each on top of the
// history, made on a branch of the source of their own and packed alone, as
// a push's pack is; the step= arms take the first compact.PublishPacks of
// them, the tail at which a publication falls due by its packs.
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

// tailPushes lands n pushes on the history in src, on a branch of their own
// — each a commit adding one file of stalePushBytes seeded random bytes, as
// the stale arms' pushes do — and packs each alone into the repository at
// dir.
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

// whole is OWN MODE'S WHOLE REWRITE after the tail's compact.PublishPacks
// pushes — a rewrite paid for the new pushes alone: the maintainer's
// multi-pack-index with its bitmap, without one, and its commit-graph with
// changed-path filters, each over every pack, as cache.DerivedFiles declares
// them. B/obj-repo decides cache.BitmapBytesPerObject, IndexBytesPerObject
// and GraphBytesPerObject; s/op, bitmap and graph together a run's,
// MidxMinInterval and MidxDutyCycle; and B/obj-new is what step=tip is read
// against.
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

// tip is a node-local BITMAPPED TIP LAYER over the tail on the published
// chain — what no node writes (the cache's package doc says why): its
// memory growing with the history rather than its few new objects, and its
// time near step=whole's, is the pathology that rules it out. A pin under
// which its peak-MiB holds still across the sizes re-decides that rule
// (CONTRIBUTING's pin checklist).
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

// tipPlain is the same tip WITHOUT a bitmap: op=write follows its new
// objects, and op=count — a walk of every object through the chain, the
// canary's read — runs with no bitmap at all once the tip tops the chain,
// since git opens a layer's bitmap only with every layer above it bitmapped
// too: what rules out the cheap tip as well.
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

// publish is ONE PUBLICATION'S BUILD, in the two shapes a maintainer meets —
// each step its own figure: part=repack, the non-full packs merged first (more
// than compact.LayerRepackPacks of them) by internal/code/compact's
// pack-objects over a store of exactly them, which cache.CompactBytesPerObject
// charges by its inputs; part=midx, the layer and its bitmap over the output,
// which cache.PublishBytesPerObject charges by the repository's objects;
// part=graph, its commit-graph layer with every changed-path filter, which
// cache.GraphLayerBytesPerObject charges by the layer's.
//
//   - layer=root is a repository's FIRST publication: every one of the
//     history's pushes merged, and the layer a root over the output — the
//     publication whose graph layer and repack cover the whole history,
//     which their per-object figures are read from. git=floor runs it on the
//     node floor's own git too, so the floor's figures stand beside the pin's.
//   - layer=tail is every later one, at its due by packs: the tail of
//     compact.PublishPacks pushes merged, and the layer over the output on
//     the published chain — whose bitmap still walks the whole history.
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

// published runs one publication shape on git's pool: inputs merged, then
// the layer and its graph over the output, in the scratch fixture makes — a
// repository holding every pack the plan was made over — over the base it
// names, main at tip.
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
				"--max-pack-size=" + strconv.FormatInt(code.CompactPackBytes, 10), "--delta-base-offset",
				"--quiet", "objects/pack/pack"},
			Stdin:    strings.NewReader(packList(inputs, ".pack")),
			Config:   compact.CompactConfig(code.DefaultLimits().MaxBlobBytes),
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

// tipGraph is THE NODE'S TIP COMMIT-GRAPH over the tail of
// compact.PublishPacks pushes, on the published graph layer — the
// declaration's own write, its --max-new-filters at each bound
// cache.MaxNewFilters is chosen from, 100 and 1 024, and dropped (every
// filter): a small tail's figure is the write's fixed part, which
// cache.TipGraphBaseBytes charges beside the tail= arms' slope.
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

// merge weighs how two published layers are merged — internal/code/compact
// writes a layer over exactly its base and never runs `multi-pack-index
// compact`. merge=root-compact is git's own `multi-pack-index compact` of L1
// and L2 from the root, which reuses their bitmaps; merge=root-write is a
// direct write of their 56 packs as a root layer, what a publication writes;
// merge=stacked (100 MB, the pin and the floor) is a write over L1 of L2's
// and L3's packs with L2 and L3 still in the chain file — what the publisher's
// scratch, whose chain files name exactly its base, exists to never run —
// capped at ten times root-write's own time and reported pathological=1 when
// it reaches the cap. root-compact cheaper than root-write by more than the
// tail write it cannot skip, or stacked no longer pathological, re-decides
// the publisher's rule (CONTRIBUTING's pin checklist).
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
		// AN IF, NEVER A SWITCH ON A CALL, in a b.Loop body: Go 1.27.2's
		// b.Loop pass (cmd/compile/internal/bloop) treats a case clause's
		// expressions as statements and rewrites a call there into an
		// assignment, and the compiler then fails with an internal error.
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

// closure is the closure git holds a bitmap to, on the pin and on the
// floor: a bitmapped layer over every
// pack of the history but its first, which leaves the layer's commits
// reaching objects outside it — refused by git ("Packfile doesn't have full
// closure"), nothing written, which is why a published layer covers EVERY
// live pack its base does not (compact's planLayer, decide rule 6d). A git
// that writes it re-decides that rule, and the arm fails saying so.
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
						"layer covers every live pack its base does not (internal/code/compact) is to be re-decided",
						pool.Version(), id)
				}
				removeAll(b, dir)
				b.StartTimer()
			}
			b.ReportMetric(1, "refused")
		})
	}
}

// staleChained is the stale arms over the PUBLISHED chain — k pushes outside
// it, read through the root layer's bitmap: op=count counts main's objects,
// the walk the chain leaves a clone, and op=clone is the pack a clone of main
// is served — the price compact.PublishPacks weighs against a publication's
// build, as the own-mode arms price cache.MidxPacks against a run.
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
// publication is due (compact.PublishShare): a root layer over the history's
// packs but its last n, those n outside it, and the requests that walk them —
// op=clone, a clone of main, and op=fetch, a fetch of main's newest commit.
// The largest share whose clone is at most a tenth past share=0's decides
// PublishShare.
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

// tails is THE TAIL IN OBJECTS (1 GB and 5 GB): that many objects of the
// history outside the published chain, whatever the repository's size — a
// root layer and its graph layer over the rest — and every request and write
// that walks them: op=clone, op=fetch (main's newest commit), op=canary
// (every object through the chain, the installer's read) and op=tip-graph
// (the node's tip over them, the declaration's own write). walk-us/object is
// the canary's and the clone's seconds over the tail's objects:
// compact.PublishTailObjects is a second of it at gitproc.WalkPerObject, and
// is re-derived where the canary walks slower; tip-graph's B/obj-new across
// the three tails is cache.TipGraphBytesPerObject's slope, and its figure over
// a small tail (step=tip-graph) the fixed part beside it.
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
			b.Run("op=tip-graph", func(b *testing.B) {
				args := declaredArgs(b, cache.StepTipGraph, cache.TriggerMaintainer)
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
		})
	}
}

// copyRefs gives the repository at to the packed-refs of the one at from.
func copyRefs(b *testing.B, from, to string) {
	b.Helper()
	write(b, filepath.Join(to, "packed-refs"), read(b, filepath.Join(from, "packed-refs")))
}

// tailFixture is a scratch whose published chain covers the history but its
// newest objects — about want of them, at a commit's grain — and answers it
// with how many objects lie outside: the pushes wholly in the tail as the
// history pushed them, the push the cut falls in split at its commit by two
// packs of their own, and a root layer and its graph layer over everything
// before the cut.
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

// BenchmarkBigNode is the BIG-NODE comparison over the large reference
// model's monorepo — a 12 GB source repository's history, gittest.History's
// composition in its 64 pushes — over 5 GB wherever gittest.LargeEnv is set,
// and over a gigabyte in every run: what one git node pays to give the whole
// fleet the repository's bitmap, against every node writing its own.
//
//   - node=own is an OWN-MODE node's whole multi-pack-index and its bitmap
//     over every pack — what every git node serving the repository would
//     write without a publication: its minutes and peak memory.
//   - node=publish is the maintainer's ROOT publication over the same packs:
//     its multi-pack-index layer with the bitmap (midx-) and its commit-graph
//     layer with every changed-path filter (graph-), once for the fleet.
//   - node=install is a git node installing that chain: a cold sync of the
//     repository with the layer published, which installs the chain before it
//     answers — the GETs it makes of packs and parts, what the sync's own
//     code allocated beside the store's copies (installed's own-alloc-MiB:
//     the downloads' and the installer's buffers, no git process), and the
//     seconds from its answer to its first count of every object through the
//     bitmap and its first clone.
//
// size=1GB is the largest a default run builds, BenchmarkMidx's largest
// default history too, and the size whose figures the docs carry wherever no
// larger arm has run. size=12GB builds for hours and holds its bitmap beside
// the in-memory store, so it runs only where bigNodeEnv is 1 too, on a
// machine of at least 64 GB; no constant waits on it: scaling.md states the
// 12 GB figure as the extrapolation of the sizes measured, labelled so, until
// a run of this arm replaces it.
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
	rows    []code.PackRow
	objects int64
	root    string
	layer   *code.LayerRef
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
	n.w.log.publish(n.repo, []code.LayerRef{ref}, nil)
	n.layer = &ref
}

// storeLayer lays out the layer id written in the repository at dir over
// base ("" for a root) and the commit-graph layers below its own, stores its
// parts in w's store and answers its row.
func storeLayer(b *testing.B, w *syncWorld, dir, id, base string, baseGraphs []string) code.LayerRef {
	b.Helper()
	layers := filepath.Join(dir, "objects", "pack", "multi-pack-index.d")
	paths := [3]string{
		filepath.Join(layers, "multi-pack-index-"+id+".midx"),
		filepath.Join(layers, "multi-pack-index-"+id+".bitmap"),
		filepath.Join(dir, "objects", "info", "commit-graphs", "graph-"+graphTop(b, dir)+".graph"),
	}
	ref, err := code.LayLayer(b.Context(), paths[0], paths[1], paths[2], baseGraphs, code.Pieces{})
	if err != nil {
		b.Fatalf("laying out layer %s: %v", id, err)
	}
	ref.BaseID, ref.Git, ref.TookMs = base, w.pool.Version().String(), 1
	if ref.Parts, err = code.StoreLayer(b.Context(), w.store, paths, ref.Sections(), nil); err != nil {
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

// installed is a cold sync of repo by a fresh cache over w — the published
// chain installed by its full round before it answers — then the copy's
// first count of every object through the bitmap and its first clone of
// main: gets/op and parts/op the GETs it made of packs and of the chain's
// parts, count-s and clone-s the two requests after its answer, and —
// where w's store is the in-memory one — own-alloc-MiB, every byte the
// process's heap allocated while the sync ran less the bytes the store
// served: the in-memory store copies each object it serves once, so what is
// left is what the sync's own code allocated, the installer's buffers among
// it — a bound on what it held at once, which a heap's growth is not, since
// that is the store's copies until the collector runs. Over the S3 fake the
// client's own buffers are no copy a byte, and the figure is not reported.
func installed(b *testing.B, w *syncWorld, repo string, fetch int, chain ...code.LayerRef) {
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

// chainOver writes a published chain of layers over the packs of repo's copy
// in a fresh cache over w — the first layer over the first cut packs, each
// next over the packs to its own cut, its graph layer beside it — stores
// every part and publishes the chain on w's log, and answers it. tips is the
// newest commit each cut's packs hold.
func chainOver(b *testing.B, w *syncWorld, repo string, rows []code.PackRow, cuts []int, tips []string) []code.LayerRef {
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
		refs   []code.LayerRef
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

// coldLayered is BenchmarkColdSync's arms of a PUBLISHED repository: its
// packs under a published chain — layers=chain one root layer
// over them all, layers=chain21 twenty-one, the chain the default
// repo_max_bytes reaches, over the same packs, the first over all but the
// last twenty and each next over one — read by a fresh cache fetching fetch
// bundles at once, which installs the chain before it answers: what a cold
// sync's parts add to its GETs and its time beside the same shape's arm with
// no layers, and that its first count and its first clone read the bitmap.
// The S3 fake answers the fixture's writes at once, and every read of the
// arm at firstByte.
//
// rows are the repository's packs, one a push, oldest first, and tips each
// push's commit.
func coldLayered(b *testing.B, w *syncWorld, repo string, rows []code.PackRow, tips []string,
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

// catchUp is install=catchup (a restore's catch-up): repos
// repositories of one small pack each, every copy synced WITHOUT a chain on
// one node, then one one-part layer published on each, which each copy's
// delta hands to the node's installer — and the installer's passes, timed,
// until every layer is placed: within repos × (the first byte + a placement)
// ÷ cache.FetchConcurrency, give or take a tenth, its GETs at the share it
// keeps in flight across repositories. getbound-s is the GETs' own part,
// repos × firstByte ÷ FetchConcurrency, and placement-ms what the rest comes
// to a repository at that width.
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
		w.log.write(id, []code.PackRow{row}, map[string]string{"refs/heads/main": commits[0]})
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

// layerLookups are BenchmarkPackLookup's layers= arms: one chain each, of
// that many layers over the same 64 packs.
var layerLookups = []int{1, 8, 32, 64}

// layerLookupArms is BenchmarkPackLookup's layers= arms over the history of
// commits in src, its 64 pushes packed into a scratch of their own: a chain
// of that many layers over them, each over an equal share of the packs, and
// a lookup through it — op=lookup is cat-file --batch-check of
// lookupIDs ids drawn from every object the history holds (ns/lookup each),
// op=count a count of main's objects through the chain's bitmaps.
// code.MaxPublishedLayers stays unless 32 layers cost more than twice one per
// lookup; then it is the longest chain that does not.
//
// The packs and the ids are made by the first arm that runs, so a pattern
// that selects none of them makes neither.
func (w *syncWorld) layerLookupArms(b *testing.B, src string, commits []string) {
	b.Helper()
	root := b.TempDir()
	pushes := splitPushes(commits, gittest.HistorySessions)
	var (
		packs, ids string
		hist       []string
	)
	for _, layers := range layerLookups {
		b.Run(fmt.Sprintf("layers=%d", layers), func(b *testing.B) {
			if packs == "" {
				packs = scratchRepo(b, root, "packs")
				for _, p := range pushes {
					revs := []string{p.tip}
					if p.base != "" {
						revs = append(revs, "^"+p.base)
					}
					hist = append(hist, gittest.Pack(b, w.client, packs, src, revs...))
				}
				ids = lookupIDs(b, w, src)
			}
			dir := linkScratch(b, root, "layers", packs, func(string) bool { return true })
			defer removeAll(b, dir)
			per, base := len(hist)/layers, "none"
			var chain []string
			var s steps
			for i := range layers {
				setMain(b, dir, pushes[(i+1)*per-1].tip)
				id := strings.TrimSpace(string(s.run(b, w.pool, midxLayerCmd(dir, hist[i*per:(i+1)*per], base, true)).Stdout))
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
