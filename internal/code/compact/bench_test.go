package compact_test

import (
	"bufio"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// BenchmarkCompact is what a home git node's compaction of a repository's
// packs costs, and the figures the compactor's constants and the cache's
// charge of a build are decided by. Every figure lands in the doc of what it
// decides with the arm's name, the machine, the date and the object store it
// ran over (CONTRIBUTING's benchmark rule): the in-memory store here, so a
// figure an S3 bucket would bill is stated as the requests and bytes the arm
// counts, never a bucket's time.
//
//   - history=source/inputs={4,8,16,64}: ONE compaction of a repository whose
//     history — gittest.History's 100 MB composition, the cache benchmarks'
//     own — was pushed as that many pushes, each its own pack, on the home that
//     holds the repository already (the ordinary case: its reads are asked of
//     it too). The build's peak anonymous memory, sampled every millisecond
//     (gittest.PeakAnon, the method the cache's per-object figures were taken
//     by), over the objects it merged (anon-B/object); the build's wall clock a
//     gibibyte of the pack bytes it merged (build-s/GiB), read against
//     gitproc.PackWorkDeadline's; and the bytes in and out (in-MB, out-MB).
//     cache.CompactBytesPerObject is the 64-input arm's anon-B/object rounded
//     up to the next 50 bytes, with its growth across the four arms stated
//     beside it.
//   - history=source/cap=input_byte_cap/cut=4MiB: a roll at its byte cap
//     (compact.InputByteCap) over the same history pushed as 64 packs, at a
//     cut scaled to 4 MiB with the bundle bound beside it: the build's
//     outputs over ⌈the pack bytes it merged ÷ the cut⌉ (outputs/cuts), from
//     which InputByteCap's half is re-derived — at most code.MaxCompactOutputs
//     outputs a roll at the cap, whatever pack-objects writes whole.
//   - history=source/roll=16/beside={history,none}: ONE compaction of
//     code.CompactMinMerge one-commit pushes, sixteen — beside the source
//     history pushed as one pack, which the roll leaves out, and beside
//     nothing — its build's wall clock (build-ms) and peak anonymous memory
//     (peak-MiB), which follow the roll and not the history when the two
//     arms agree: the build's walk for its name hashes ends at the roll's
//     edge (cache.Cache.NewMergeScratch), where over the whole copy it ran
//     the history — a roll of eight, the floor then, took 451 ms and
//     100.8 MiB beside it, against 19.6 ms and 0.8 MiB over its store (the
//     figures are cache's scratch.go's).
//   - steady/pushes=6000/rate=300/min/min-merge={4,8,16} (LARGE): 6 000
//     one-commit pushes of 20 KB through the data node's committer, 300 to a
//     simulated minute, and a pass of the home's compactor merging at that
//     many packs at every minute, while the other git node syncs after every
//     push and every pass, as a node serving the repository's fetches does —
//     a BUSY repository, whose every pass finds the minute's three hundred
//     packs whatever its floor. min-merge=16, the production floor, is the
//     STEADY arm: push-KB is what a push's pack weighed — its file, its
//     commit and its two trees — which every figure over the bytes pushed
//     divides by; its max-live-packs must stay under code.CompactTargetPacks
//     plus a minute's pushes, which confirms Poll; retired/push is the roll's
//     ρ; rewrite-B/pushed-B what the outputs weigh over what was pushed,
//     O(log n) at factor 2; record-B/push the compactions' records on the
//     code log a push, their signed frames included; and got-B/pushed-B what
//     the other git node downloads over what was pushed, about 1 + log₂(the
//     largest merged pack ÷ a push's) — stated in scaling.md (M3-decisions
//     M3-O63). Every arm states, per 1 000 pushes, the GETs a cold sync of
//     the result makes (cold-gets/kpush, cold-MB/kpush), the compactions' PUTs
//     (puts/kpush, put-MB/kpush) and the GETs every git node but the builder
//     makes of their outputs on a fleet of eight (fleet-gets/kpush,
//     fleet-MB/kpush: the measured node's times seven, since each runs the
//     same rounds over the same rows) — requests, S3's billing unit, and
//     bytes, which a node's disk writes.
//   - steady/pushes=1200/rate=1/min/min-merge={4,8,16} (LARGE): the same, a
//     QUIET repository pushed once a minute, so every floor binds — a merge
//     waits until that many packs have gathered — and the floors' costs
//     differ, where at three hundred a minute every floor is passed at every
//     pass and the three measure alike. code.CompactMinMerge is the floor
//     with the fewest GETs and PUTs a thousand pushes here, where it saves
//     more than a tenth of them against the floor before it — 16, which
//     halved 8's (code's limits.go has each floor's figures). Its
//     retired/push and record-B/push are a merge at the floor's: the rows
//     and the records a repository pushed more slowly than its home polls
//     costs.
//   - blobs=max_blob/inputs=8 (LARGE): one compaction over eight packs, each a
//     revision of one blob of the default max_blob_bytes, so the delta search
//     holds two of them at once: the build's peak anonymous memory (peak-MiB)
//     against what it was admitted at (charge-MiB: its objects at
//     cache.CompactBytesPerObject, room beside them for a clone of the whole
//     repository — whose every object it merges — and
//     cache.CompactWindowBytes), and the window the peak leaves past the
//     per-object charge (window-MiB). A peak past the charge raises
//     CompactWindowBytes to the window measured.
//   - swap/packs={50,2048} (2048 LARGE): one sync of a copy holding that many
//     packs — at 2 048 the bound itself — after a compaction retired three of
//     them — a SWAP into the copy's next directory, of links: its wall clock,
//     its download of the one output included, and the files it links
//     (links/swap), which follow the pack count, never the bytes.
//   - poll/repos={1000,10000}/op=pass/home={all,1of8}: one pass over that
//     many listed repositories none of which is due here — the memory's
//     steady state — on the home of every one (all: each remembered as not
//     due, a lookup) and on a node home to an eighth of them, as each node of
//     a fleet of eight is (1of8: the eighth remembered, the rest judged
//     against the pass's ONE reading of who serves git — a set's lookup here,
//     the router's own judging priced by estate's BenchmarkHomeOf); ns/repo
//     is what a repository costs a pass.
//   - poll/repos={1000,10000}/op=page: the serving data node's read of ONE
//     page of the listing of that many repositories (code_repos_packs_idx,
//     at stale, as every pass reads it). Past 100 ms at 10 000 — two listings
//     a minute from each of sixteen git nodes would hold over a twentieth of
//     one reader — the listing is re-decided (M3-decisions M3-O26).
func BenchmarkCompact(b *testing.B) {
	quiet(b)
	b.Run("history=source", benchHistory)
	b.Run("steady", benchSteady)
	b.Run("blobs=max_blob/inputs=8", benchBlobs)
	for _, packs := range []int{50, 2048} {
		b.Run(fmt.Sprintf("swap/packs=%d", packs), func(b *testing.B) { benchSwap(b, packs) })
	}
	for _, repos := range []int{1000, 10_000} {
		b.Run(fmt.Sprintf("poll/repos=%d", repos), func(b *testing.B) { benchPoll(b, repos) })
	}
}

// quiet keeps every component's own log lines — the cache's, the host's, the
// committer's — off a benchmark's output, where a figure is read.
func quiet(b *testing.B) {
	b.Helper()
	was := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	b.Cleanup(func() { slog.SetDefault(was) })
}

// The benchmark's shapes, each with its reason.
const (
	// historyBytes is the source history the history= arms merge:
	// gittest.History's 100 MB composition — about 300 000 objects and
	// 42 000 commits, the cache benchmarks' own — so the per-object figure
	// is taken over a source repository's shape and not a synthetic one.
	historyBytes = 100_000_000

	// steadyPushBytes is each steady push's new bytes, the 20 KB a push of
	// one commit carries in the figure scaling.md states.
	steadyPushBytes = 20_000

	// eightNodes is the fleet the min-merge figures are stated for: one
	// builder and seven git nodes downloading each output once.
	eightNodes = 8

	// rollPushes and rollPushBytes are the roll= arms' roll: the fewest
	// one-commit pushes a compaction merges (code.CompactMinMerge), each the
	// steady arm's push — the ordinary compaction of a busy repository, over
	// a few dozen objects whatever history stands beside it.
	rollPushes    = code.CompactMinMerge
	rollPushBytes = steadyPushBytes

	// swapPushBytes is each of the swap arm's pushes: a file of a kibibyte,
	// so what its figure follows is the pack count and never the bytes.
	swapPushBytes = 1 << 10

	// benchProcesses is every benchmark git node's read lane, as the
	// package's cases run (fixtureProcesses): the maintenance lane two
	// slots, the maintainer keeping one, so a compaction never builds beside
	// a maintainer's run and its peak is its own.
	benchProcesses = fixtureProcesses

	// benchLogBytes is a benchmark fleet's code log: 256 MiB admits about
	// 210 MB of records beside its gate reserve, three steady runs' 18 000
	// pushes among them, where a solo node's own 64 MiB admits about 7 MB.
	benchLogBytes = 256 << 20
)

// traffic is the requests and bytes one kind of a node's object store calls
// carried.
type traffic struct{ requests, bytes atomic.Int64 }

// figures are t's counts now.
func (t *traffic) figures() (requests, bytes int64) { return t.requests.Load(), t.bytes.Load() }

// countingGets is a git node's backend with every GET it makes counted — each
// request, and every byte read through the streams it answers: what the node
// downloads, apart from what any other node downloads from the same store.
type countingGets struct {
	objstore.Backend
	t *traffic
}

func (c countingGets) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	rc, err := c.Backend.Get(ctx, name, off, n)
	if err != nil {
		return nil, err
	}
	c.t.requests.Add(1)
	return countedRead{ReadCloser: rc, t: c.t}, nil
}

// countedRead counts every byte read through it.
type countedRead struct {
	io.ReadCloser
	t *traffic
}

func (r countedRead) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.t.bytes.Add(int64(n))
	return n, err
}

// countingPuts is a compactor's object store with every output it stores
// counted: the compactions' PUTs, and what each stored.
type countingPuts struct {
	*objstore.Store
	t *traffic
}

func (c countingPuts) Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object,
	error) {
	obj, err := c.Store.Put(ctx, r, limit, m)
	if err == nil {
		c.t.requests.Add(1)
		c.t.bytes.Add(obj.Size)
	}
	return obj, err
}

// benchFleet is one arm's code host: a solo fleet over the in-memory store,
// its data node's committer taking every push the arm proposes, its first git
// node the HOME whose compactor the arm builds and drives itself (never
// started), and every git node's downloads counted apart.
type benchFleet struct {
	fleet *codetest.SoloFleet
	store *objstore.Store
	// gets is what each git node downloaded, by its id.
	gets map[string]*traffic
	// repos counts the repositories the arm declared, each named apart.
	repos int
}

// newBenchFleet is a fleet of gitNodes git nodes, its broker syncing every
// write as a deployment's does (SyncAlways).
func newBenchFleet(b *testing.B, gitNodes int) *benchFleet {
	b.Helper()
	gittest.RequireNode(b)
	objects := memobj.New()
	f := &benchFleet{gets: map[string]*traffic{}}
	f.fleet = codetest.SoloNode(b, codetest.SoloOptions{
		Shape: codetest.Shape{GitNodes: gitNodes}, Objects: objects, MaxProcesses: benchProcesses,
		SyncAlways: true, LogBytes: benchLogBytes,
		WrapObjects: func(id string, backend objstore.Backend) objstore.Backend {
			t := &traffic{}
			f.gets[id] = t
			return countingGets{Backend: backend, t: t}
		},
	})
	store, err := objstore.NewStore(objects)
	if err != nil {
		b.Fatal(err)
	}
	f.store = store
	return f
}

// home is the git node every arm's compactor builds on.
func (f *benchFleet) home() codetest.Node { return f.fleet.Nodes()[0] }

// declare is a fresh repository of the arm's own: its name and its id.
func (f *benchFleet) declare(b *testing.B) (name, id string) {
	b.Helper()
	f.repos++
	name = fmt.Sprintf("bench-%d", f.repos)
	f.fleet.Declare(b, code.Declared{Name: name, Settings: codetest.Settings()})
	rows, err := f.fleet.Rows(b.Context(), name, codetest.Linearizable)
	if err != nil {
		b.Fatalf("the rows of %s: %v", name, err)
	}
	return name, rows.Repo.ID
}

// compactor is a compactor over the home's cache — storing through puts,
// listing and proposing through the data node, home for every repository —
// merging at minMerge packs and more, as the engine builds one but never
// started: the arm drives every pass and every compaction.
func (f *benchFleet) compactor(b *testing.B, minMerge int, puts *traffic) (*compact.Compactor, *metrics.Recorder,
	*recordedEstate) {

	b.Helper()
	rec, err := metrics.New()
	if err != nil {
		b.Fatal(err)
	}
	est := &recordedEstate{Estate: served{f.fleet}}
	c, err := compact.New(compact.Options{Cache: f.home().Cache(), Objects: countingPuts{Store: f.store, t: puts},
		Estate: est, Homes: homes{home: true}, NodeID: f.home().ID(), Limits: code.DefaultLimits, Metrics: rec,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	compact.MergeFrom(c, minMerge)
	b.Cleanup(func() { c.Stop(context.Background()) })
	return c, rec, est
}

// recordedEstate is a compactor's code log with every compaction it APPLIED
// recorded, and the position of the last.
type recordedEstate struct {
	compact.Estate
	mu      sync.Mutex
	applied []code.Compaction
	last    statelog.Position
}

func (r *recordedEstate) Compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	res, err := r.Estate.Compact(ctx, p)
	if err == nil && res.Outcome == statelog.OutcomeApplied {
		r.mu.Lock()
		r.applied = append(r.applied, p.Compaction)
		r.last = res.Position
		r.mu.Unlock()
	}
	return res, err
}

// since is every compaction applied after the first n, and the last one's
// position.
func (r *recordedEstate) since(n int) ([]code.Compaction, statelog.Position) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.applied[n:]), r.last
}

// pushed is one push's pack, built before an arm's clock runs: its files in
// bare under checksum, and the commit main moves from and to.
type pushed struct {
	bare, checksum, old, tip string
}

// propose lands p on the repository id through the data node's committer — a
// push no person's git sent, carrying exactly the objects the arm packed — and
// answers its pack's row and the position it landed at.
func (f *benchFleet) propose(b *testing.B, id string, p pushed) (code.PackRef, statelog.Position) {
	b.Helper()
	pushID := uuid.Must(uuid.NewV7()).String()
	path := filepath.Join(p.bare, "objects", "pack", "pack-"+p.checksum+".pack")
	pack, err := code.LayBundle(pushID, path, p.checksum)
	if err != nil {
		b.Fatal(err)
	}
	if err := code.StoreBundle(b.Context(), f.store, path, pack, nil); err != nil {
		b.Fatalf("store pack-%s: %v", p.checksum, err)
	}
	res, err := f.fleet.Data().Served().Propose(b.Context(), code.Proposal{
		PushID: pushID, RepoID: id, Principal: pushRef, Via: code.ViaGit, Node: f.home().ID(),
		ReceivedAt: time.Now().UTC(), Pack: pack,
		Commands: []code.Command{{Ref: "refs/heads/main", Old: p.old, New: p.tip}},
	})
	if err != nil || res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
		res.Answer.Commands[0].Result != code.ResultOK {
		b.Fatalf("the push of pack-%s: %+v, %v", p.checksum, res, err)
	}
	return *pack, res.Position
}

// sync has node's cache sync the repository id to at least at, and lets go of
// it: what a node serving the repository's fetches does as its rows move.
func (f *benchFleet) sync(b *testing.B, node codetest.Node, id string, at statelog.Position) {
	b.Helper()
	s, err := node.Cache().Sync(b.Context(), id, statelog.FloorAt(statelog.ReadSession, at))
	if err != nil {
		b.Fatalf("%s's sync of %s: %v", node.ID(), id, err)
	}
	s.Release()
}

// candidate is the repository called name as the due listing names it, at
// stale as every pass reads it: at the floor of one live pack, whatever a plan
// makes of it — and false where it is not listed.
func (f *benchFleet) candidate(b *testing.B, name string) (code.CompactionCandidate, bool) {
	b.Helper()
	q := code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: 1, Fresh: codetest.Stale}
	for {
		a, err := f.fleet.Data().Served().Read(b.Context(), q)
		if err != nil {
			b.Fatalf("the due listing: %v", err)
		}
		for _, cand := range a.Due {
			if cand.Name == name {
				return cand, true
			}
		}
		if a.Next == "" {
			return code.CompactionCandidate{}, false
		}
		q.Cursor = a.Next
	}
}

// logBytes is what the code log's stream holds now.
func (f *benchFleet) logBytes(b *testing.B) int64 {
	b.Helper()
	stats, err := f.fleet.Data().LogStats(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	return int64(stats.Bytes)
}

// buildMeasure is every build a compactor ran, measured as it ran: the most
// anonymous memory any git process held meanwhile (gittest.PeakAnon) and the
// wall clock the builds took — what BuildWith hands a compactor in place of
// the reserved slot's own run, which it calls.
type buildMeasure struct {
	mu   sync.Mutex
	peak int64
	took time.Duration
	// measured is false where /proc says nothing (any system but Linux):
	// the arm then reports its times without its memory.
	measured bool
}

func (m *buildMeasure) run(ctx context.Context, adm *cache.Compacting, cmd gitproc.Cmd) (gitproc.Result, error) {
	var (
		res gitproc.Result
		err error
	)
	began := time.Now()
	peak, ok := gittest.PeakAnon(func() { res, err = compact.RunInSlot(ctx, adm, cmd) })
	took := time.Since(began)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peak, m.took, m.measured = max(m.peak, peak), m.took+took, ok
	return res, err
}

// figures are the peak, the builds' time and whether the memory was read.
func (m *buildMeasure) figures() (int64, time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak, m.took, m.measured
}

// benchHistory is the history= arms: one source history, pushed as each arm's
// pushes into a repository of its own on a fleet of its own.
func benchHistory(b *testing.B) {
	gittest.RequireNode(b)
	client := gittest.NewClient(b)
	src, commits := gittest.History(b, client, historyBytes)
	for _, n := range []int{4, 8, 16, 64} {
		b.Run(fmt.Sprintf("inputs=%d", n), func(b *testing.B) {
			benchInputs(b, historyPushes(b, client, src, commits, n), n)
		})
	}
	b.Run(fmt.Sprintf("cap=input_byte_cap/cut=%dMiB", scaledCut>>20), func(b *testing.B) {
		benchCap(b, historyPushes(b, client, src, commits, gittest.HistorySessions))
	})
	// THE ROLL'S OWN COST, beside the history and beside nothing: the same
	// shape of rollPushes one-commit pushes, made last since they extend src's
	// main — each stamped after the history's newest commit, so no walk of
	// git's reads the history for a commit older than its parent.
	b.Run(fmt.Sprintf("roll=%d", rollPushes), func(b *testing.B) {
		tip := commits[len(commits)-1]
		history := historyPushes(b, client, src, commits, 1)
		roll := commitPushes(b, client, src, tip, extend(b, client, src, tip, len(commits), rollPushes,
			rollPushBytes))
		alone := gittest.Empty(b)
		lone := commitPushes(b, client, alone, code.ZeroOID, extend(b, client, alone, "", len(commits), rollPushes,
			rollPushBytes))
		b.Run("beside=history", func(b *testing.B) { benchRoll(b, append(slices.Clone(history), roll...)) })
		b.Run("beside=none", func(b *testing.B) { benchRoll(b, lone) })
	})
}

// benchRoll is history=source/roll=…: a compaction of a repository whose last
// rollPushes pushes are one commit each, a loop, each over a repository of its
// own — beside=history after the source history pushed as ONE pack, which the
// roll leaves out, and beside=none alone. The build's wall clock (build-ms)
// and its peak anonymous memory (peak-MiB) are the roll's, never the
// history's, when the two arms agree: the build's walk for its name hashes
// ends at the roll's edge (cache.Cache.NewMergeScratch).
func benchRoll(b *testing.B, pushes []pushed) {
	f := newBenchFleet(b, 1)
	c, _, est := f.compactor(b, code.CompactMinMerge, &traffic{})
	m := &buildMeasure{}
	compact.BuildWith(c, m.run)
	var objects, beside int64
	for b.Loop() {
		b.StopTimer()
		name, _, packs := f.repoOf(b, pushes)
		cand, ok := f.candidate(b, name)
		if !ok {
			b.Fatalf("%s is not listed", name)
		}
		seen, _ := est.since(0)
		b.StartTimer()
		if outcome, err := compactWhileBusy(b, c, cand); outcome != compact.OutcomeApplied {
			b.Fatalf("the roll of %d pushes: %s, %v; want applied", rollPushes, outcome, err)
		}
		b.StopTimer()
		applied, _ := est.since(len(seen))
		if len(applied) != 1 || len(applied[0].Retire) != rollPushes {
			b.Fatalf("the roll applied %v, want one compaction merging the %d one-commit pushes", applied,
				rollPushes)
		}
		objects, beside = 0, 0
		for _, p := range packs {
			if slices.Contains(applied[0].Retire, p.PackID) {
				objects += p.Objects
			} else {
				beside += p.Objects
			}
		}
		b.StartTimer()
	}
	peak, took, measured := m.figures()
	b.ReportMetric(float64(objects), "objects")
	b.ReportMetric(float64(beside), "beside-objects")
	b.ReportMetric(float64(took.Microseconds())/1e3/float64(b.N), "build-ms")
	if measured {
		b.ReportMetric(float64(peak)/(1<<20), "peak-MiB")
	}
}

// historyPushes is the history of src split into n pushes of about equal
// commits, oldest first, each one pack: the objects its tip reaches and the
// tip before it does not — exactly what a client pushing the history in n
// pushes would send, completed whole as a receiver's index-pack stores it.
func historyPushes(b *testing.B, client *gittest.Client, src string, commits []string, n int) []pushed {
	b.Helper()
	bare := gittest.Empty(b)
	out := make([]pushed, n)
	old := code.ZeroOID
	for i := range n {
		tip := commits[(i+1)*len(commits)/n-1]
		revs := []string{tip}
		if old != code.ZeroOID {
			revs = append(revs, "^"+old)
		}
		out[i] = pushed{bare: bare, checksum: gittest.Pack(b, client, bare, src, revs...), old: old, tip: tip}
		old = tip
	}
	return out
}

// repoOf is a fresh repository holding pushes, each proposed in turn, synced
// on the home — which holds it, as the node its reads are asked of — and the
// packs it holds.
func (f *benchFleet) repoOf(b *testing.B, pushes []pushed) (string, string, []code.PackRef) {
	b.Helper()
	name, id := f.declare(b)
	packs := make([]code.PackRef, len(pushes))
	var at statelog.Position
	for i, p := range pushes {
		packs[i], at = f.propose(b, id, p)
	}
	f.sync(b, f.home(), id, at)
	return name, id, packs
}

// benchInputs is history=source/inputs=n: a compaction of n pushes' packs a
// loop, each over a repository of its own.
func benchInputs(b *testing.B, pushes []pushed, n int) {
	f := newBenchFleet(b, 1)
	c, _, _ := f.compactor(b, min(n, code.CompactMinMerge), &traffic{})
	m := &buildMeasure{}
	compact.BuildWith(c, m.run)
	var objects, in, merged, out int64
	for b.Loop() {
		b.StopTimer()
		name, _, packs := f.repoOf(b, pushes)
		cand, ok := f.candidate(b, name)
		if !ok {
			b.Fatalf("%s is not listed", name)
		}
		objects, in, merged = 0, 0, 0
		for _, p := range packs {
			objects += p.Objects
			in += p.Size
			merged += p.PackBytes()
		}
		b.StartTimer()
		if outcome, err := compactWhileBusy(b, c, cand); outcome != compact.OutcomeApplied {
			b.Fatalf("the compaction of %d pushes: %s, %v; want applied", n, outcome, err)
		}
		b.StopTimer()
		rows, err := f.fleet.Rows(b.Context(), name, codetest.Linearizable)
		if err != nil {
			b.Fatal(err)
		}
		out = 0
		for _, r := range rows.Packs {
			out += r.Size
		}
		b.StartTimer()
	}
	peak, took, measured := m.figures()
	b.ReportMetric(float64(objects), "objects")
	b.ReportMetric(float64(in)/1e6, "in-MB")
	b.ReportMetric(float64(out)/1e6, "out-MB")
	b.ReportMetric(took.Seconds()/float64(b.N)/(float64(merged)/(1<<30)), "build-s/GiB")
	if measured && objects > 0 {
		b.ReportMetric(float64(peak)/float64(objects), "anon-B/object")
		b.ReportMetric(float64(peak)/(1<<20), "peak-MiB")
	}
}

// benchCap is history=source/cap=…: a roll at its byte cap at the scaled cut,
// over the history pushed as gittest.HistorySessions packs — each a fraction
// of the cut, so the cap, never the roll, says what is merged. The cuts an
// output count is read against are the BUILD'S OWN: a roll as dense as a
// source history's is cut below the nominal cut, so its bundles fit the bound
// (bounds.cutFor), and its byte cap follows that cut.
func benchCap(b *testing.B, pushes []pushed) {
	f := newBenchFleet(b, 1)
	c, _, est := f.compactor(b, code.CompactMinMerge, &traffic{})
	compact.ScaleBounds(c, scaledCut, scaledBundle)
	bld := &builder{}
	compact.BuildWith(c, bld.run)
	var outputs, cuts int64
	for b.Loop() {
		b.StopTimer()
		name, _, packs := f.repoOf(b, pushes)
		cand, ok := f.candidate(b, name)
		if !ok {
			b.Fatalf("%s is not listed", name)
		}
		seen, _ := est.since(0)
		b.StartTimer()
		if outcome, err := compactWhileBusy(b, c, cand); outcome != compact.OutcomeApplied {
			b.Fatalf("a roll at its byte cap: %s, %v; want applied", outcome, err)
		}
		b.StopTimer()
		applied, _ := est.since(len(seen))
		if len(applied) != 1 {
			b.Fatalf("the roll applied %d compactions, want one", len(applied))
		}
		var merged int64
		for _, p := range packs {
			if slices.Contains(applied[0].Retire, p.PackID) {
				merged += p.PackBytes()
			}
		}
		asked, _ := bld.calls()
		cut := asked[len(asked)-1]
		if limit := compact.InputByteCap(cut); merged > limit {
			b.Fatalf("the roll merged %d pack bytes, past its cap of %d at the cut of %d", merged, limit, cut)
		}
		outputs, cuts = int64(len(applied[0].Add)), (merged+cut-1)/cut
		b.StartTimer()
	}
	b.ReportMetric(float64(outputs), "outputs")
	b.ReportMetric(float64(cuts), "cuts")
	b.ReportMetric(float64(outputs)/float64(cuts), "outputs/cuts")
}

// steadyRates are the steady arms' repositories, each with its reason:
// pushes landed, and how many to a simulated minute — a pass of the home's
// compactor at every minute.
var steadyRates = []struct{ pushes, perMinute int }{
	// BUSY: 300 a minute, the rate the master's confirmation of Poll is
	// stated at, twenty minutes of it.
	{pushes: 6_000, perMinute: 300},
	// QUIET: one a minute, so every pass finds one pack more and every floor
	// binds — twenty hours of it, about seventy-five merges at the highest
	// floor.
	{pushes: 1_200, perMinute: 1},
}

// benchSteady is steady/…/min-merge={4,8,16}: for each of steadyRates, one
// pool of one-commit pushes, built once, landed by each arm into a repository
// of its own on a fleet of its own, a pass of the home's compactor a simulated
// minute.
func benchSteady(b *testing.B) {
	gittest.Large(b, "one-commit pushes packed, then landed, synced and compacted at each floor")
	gittest.RequireNode(b)
	client := gittest.NewClient(b)
	for _, rate := range steadyRates {
		b.Run(fmt.Sprintf("pushes=%d/rate=%d/min", rate.pushes, rate.perMinute), func(b *testing.B) {
			src := gittest.Empty(b)
			commits := extend(b, client, src, "", 0, rate.pushes, steadyPushBytes)
			pushes := commitPushes(b, client, src, code.ZeroOID, commits)
			for _, minMerge := range []int{4, 8, 16} {
				b.Run(fmt.Sprintf("min-merge=%d", minMerge), func(b *testing.B) {
					var fig steadyFigures
					for b.Loop() {
						fig = steadyRun(b, pushes, rate.perMinute, minMerge)
					}
					fig.report(b)
				})
			}
		})
	}
}

// steadyFigures is what one steady run measured.
type steadyFigures struct {
	pushes                 int
	pushedBytes            int64
	maxLive                int64
	compactions, retired   int
	outputBytes            int64
	recordBytes            int64
	puts, putBytes         int64
	outputGets, outputGot  int64
	otherGot               int64
	coldGets, coldGetBytes int64
}

// steadyRun lands pushes into a fresh repository on a fleet of its own, the
// home's compactor merging at minMerge passing every perMinute pushes, the
// other git node syncing after every push and every pass; its figures.
func steadyRun(b *testing.B, pushes []pushed, perMinute, minMerge int) steadyFigures {
	b.Helper()
	b.StopTimer()
	f := newBenchFleet(b, 2)
	puts := &traffic{}
	c, rec, est := f.compactor(b, minMerge, puts)
	other := f.fleet.Nodes()[1]
	got := f.gets[other.ID()]
	name, id := f.declare(b)
	b.StartTimer()

	fig := steadyFigures{pushes: len(pushes)}
	for i, p := range pushes {
		pack, at := f.propose(b, id, p)
		fig.pushedBytes += pack.Size
		f.sync(b, other, id, at)
		if (i+1)%perMinute != 0 && i != len(pushes)-1 {
			continue
		}
		cand, ok := f.candidate(b, name)
		if !ok {
			b.Fatalf("%s is not listed", name)
		}
		fig.maxLive = max(fig.maxLive, cand.LivePacks)
		seen, _ := est.since(0)
		before := f.logBytes(b)
		pollWhileBusy(b, c, rec)
		fig.recordBytes += f.logBytes(b) - before
		applied, last := est.since(len(seen))
		if len(applied) == 0 {
			continue
		}
		// THE OTHER NODE'S ROUND AFTER A PASS downloads the outputs and
		// nothing else: every push's pack it held already.
		requests, bytes := got.figures()
		f.sync(b, other, id, last)
		afterRequests, afterBytes := got.figures()
		fig.outputGets += afterRequests - requests
		fig.outputGot += afterBytes - bytes
		for _, comp := range applied {
			fig.compactions++
			fig.retired += len(comp.Retire)
			for _, out := range comp.Add {
				fig.outputBytes += out.Size
			}
		}
	}
	b.StopTimer()
	fig.puts, fig.putBytes = puts.figures()
	_, fig.otherGot = got.figures()
	rows, err := f.fleet.Rows(b.Context(), name, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	fig.coldGets = int64(len(rows.Packs))
	for _, r := range rows.Packs {
		fig.coldGetBytes += r.Size
	}
	b.StartTimer()
	return fig
}

// report states fig on b, its requests and bytes a thousand pushes.
func (fig steadyFigures) report(b *testing.B) {
	kpush := float64(fig.pushes) / 1000
	b.ReportMetric(float64(fig.pushedBytes)/float64(fig.pushes)/1e3, "push-KB")
	b.ReportMetric(float64(fig.maxLive), "max-live-packs")
	b.ReportMetric(float64(fig.compactions), "compactions")
	b.ReportMetric(float64(fig.retired)/float64(fig.pushes), "retired/push")
	b.ReportMetric(float64(fig.outputBytes)/float64(fig.pushedBytes), "rewrite-B/pushed-B")
	b.ReportMetric(float64(fig.recordBytes)/float64(fig.pushes), "record-B/push")
	b.ReportMetric(float64(fig.otherGot)/float64(fig.pushedBytes), "got-B/pushed-B")
	b.ReportMetric(float64(fig.coldGets)/kpush, "cold-gets/kpush")
	b.ReportMetric(float64(fig.coldGetBytes)/1e6/kpush, "cold-MB/kpush")
	b.ReportMetric(float64(fig.puts)/kpush, "puts/kpush")
	b.ReportMetric(float64(fig.putBytes)/1e6/kpush, "put-MB/kpush")
	b.ReportMetric(float64(fig.outputGets*(eightNodes-1))/kpush, "fleet-gets/kpush")
	b.ReportMetric(float64(fig.outputGot*(eightNodes-1))/1e6/kpush, "fleet-MB/kpush")
}

// extendDirs is how many directories extend spreads a history's files over,
// as internal/code/cache's benchmarks spread theirs: a push of one file then
// carries it and two trees of at most a hundred entries — the directory it
// lands in and the root — as a push into a repository of directories does.
// Every file in ONE directory would have every push carry a root tree listing
// every file the history has added, some 40 bytes a file: over the steady
// arm's 6 000 pushes about 73 KB a push of trees beside its 20 KB of file, so
// what a push weighed would have been mostly a tree that real repositories
// never carry, and every figure over the bytes pushed understated by it.
const extendDirs = 100

// extend fast-imports n commits onto src's main after from ("" for its first
// commit), the first of them the history's first-th, each adding one file of
// size bytes from a stream seeded by first — incompressible, as most of a
// push's new bytes are — in one of extendDirs directories, and answers them,
// oldest first. Each is stamped a second after the one before (benchEpoch,
// first), so no commit is older than its parent: a skewed one has git's
// date-ordered walks read the whole history (gittest.After).
func extend(b *testing.B, client *gittest.Client, src, from string, first, n, size int) []string {
	b.Helper()
	gittest.FastImport(b, client, src, func(w *bufio.Writer) error {
		bytes := rand.NewChaCha8([32]byte{byte(first), byte(first >> 8), byte(first >> 16)})
		body := make([]byte, size)
		for i := range n {
			_, _ = bytes.Read(body)
			msg := fmt.Sprintf("push %d\n", first+i)
			if _, err := fmt.Fprintf(w, "commit refs/heads/main\ncommitter Bench <bench@example.com> %d +0000\n"+
				"data %d\n%s", benchEpoch+first+i, len(msg), msg); err != nil {
				return err
			}
			// THE FIRST COMMIT NAMES ITS PARENT: fast-import continues a branch
			// from its tip only within one stream.
			if i == 0 && from != "" {
				if _, err := fmt.Fprintf(w, "from %s\n", from); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "M 100644 inline d%02d/p%07d.bin\ndata %d\n", (first+i)%extendDirs,
				first+i, size); err != nil {
				return err
			}
			if _, err := w.Write(body); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	})
	revs := "main"
	if from != "" {
		revs = from + "..main"
	}
	commits := strings.Fields(client.Run(b, src, "rev-list", "--reverse", revs))
	if len(commits) != n {
		b.Fatalf("fast-import wrote %d commits, want %d", len(commits), n)
	}
	return commits
}

// benchEpoch is every benchmark commit's first committer time, each a second
// past the one before: 2026-01-01, gittest.History's own.
const benchEpoch = 1_767_225_600

// commitPushes is one push a commit of commits, oldest first, after old: each
// its own pack of what the commit adds to its parent, in a bare repository of
// the pool's own.
func commitPushes(b *testing.B, client *gittest.Client, src, old string, commits []string) []pushed {
	b.Helper()
	bare := gittest.Empty(b)
	out := make([]pushed, len(commits))
	for i, tip := range commits {
		revs := []string{tip}
		if old != code.ZeroOID {
			revs = append(revs, "^"+old)
		}
		out[i] = pushed{bare: bare, checksum: gittest.Pack(b, client, bare, src, revs...), old: old, tip: tip}
		old = tip
	}
	return out
}

// benchBlobs is blobs=max_blob/inputs=8: one compaction a loop of eight pushes,
// each a revision of one blob of the default max_blob_bytes.
func benchBlobs(b *testing.B) {
	gittest.Large(b, "eight revisions of a 50 MiB blob, packed whole and compacted")
	gittest.RequireNode(b)
	client := gittest.NewClient(b)
	src := gittest.Empty(b)
	limits := code.DefaultLimits()
	commits := revisions(b, client, src, 8, int(limits.MaxBlobBytes))
	pushes := commitPushes(b, client, src, code.ZeroOID, commits)
	f := newBenchFleet(b, 1)
	c, _, _ := f.compactor(b, code.CompactMinMerge, &traffic{})
	m := &buildMeasure{}
	compact.BuildWith(c, m.run)
	var objects int64
	for b.Loop() {
		b.StopTimer()
		name, _, packs := f.repoOf(b, pushes)
		cand, ok := f.candidate(b, name)
		if !ok {
			b.Fatalf("%s is not listed", name)
		}
		objects = 0
		for _, p := range packs {
			objects += p.Objects
		}
		b.StartTimer()
		if outcome, err := compactWhileBusy(b, c, cand); outcome != compact.OutcomeApplied {
			b.Fatalf("the compaction of eight blob revisions: %s, %v; want applied", outcome, err)
		}
	}
	peak, _, measured := m.figures()
	if !measured {
		return
	}
	// THE ADMISSION'S CHARGE: the objects merged, and a clone's room of the
	// whole repository — the same objects, since the arm merges every pack
	// the repository holds — and the window.
	perObject := objects * cache.CompactBytesPerObject
	charge := perObject + objects*gitproc.TransferBytesPerObject + cache.CompactWindowBytes(limits)
	b.ReportMetric(float64(peak)/(1<<20), "peak-MiB")
	b.ReportMetric(float64(charge)/(1<<20), "charge-MiB")
	b.ReportMetric(float64(peak-perObject)/(1<<20), "window-MiB")
}

// revisions fast-imports n commits onto src's main, each a revision of ONE
// file of size bytes — the first seeded random bytes, every later one a
// sixteenth of the one before rewritten — so every revision deltas against
// the one before it, and a delta search holds two of them at once; and
// answers the commits, oldest first.
func revisions(b *testing.B, client *gittest.Client, src string, n, size int) []string {
	b.Helper()
	gittest.FastImport(b, client, src, func(w *bufio.Writer) error {
		bytes := rand.NewChaCha8([32]byte{2})
		draw := rand.New(rand.NewPCG(2, 2))
		body := make([]byte, size)
		_, _ = bytes.Read(body)
		window := size / 16
		for i := range n {
			if i > 0 {
				off := draw.IntN(size - window)
				_, _ = bytes.Read(body[off : off+window])
			}
			msg := fmt.Sprintf("revision %d\n", i)
			if _, err := fmt.Fprintf(w, "commit refs/heads/main\ncommitter Bench <bench@example.com> %d +0000\n"+
				"data %d\n%sM 100644 inline blob.bin\ndata %d\n", benchEpoch+i, len(msg), msg, size); err != nil {
				return err
			}
			if _, err := w.Write(body); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	})
	return strings.Fields(client.Run(b, src, "rev-list", "--reverse", "main"))
}

// benchSwap is swap/packs=n: a loop of syncs that each swap a copy holding n
// packs, after a compaction retired the three newest. The repository holds
// n − 2 packs between two syncs, so the two pushes each iteration lands before
// its compaction take it to n and never past code.MaxPacksPerRepo — at
// packs=2048 the bound itself, where a third would be refused
// compaction_behind — and the compaction, of the two pushes and the pack
// below them, takes it back to n − 2.
func benchSwap(b *testing.B, n int) {
	if n > 50 {
		gittest.Large(b, fmt.Sprintf("%d pushes packed, stored and synced", n))
	}
	gittest.RequireNode(b)
	client := gittest.NewClient(b)
	src := gittest.Empty(b)
	commits := extend(b, client, src, "", 0, n-2, swapPushBytes)
	f := newBenchFleet(b, 1)
	node := f.home()
	_, id, packs := f.repoOf(b, commitPushes(b, client, src, code.ZeroOID, commits))
	// live is the repository's live packs, oldest first, each with the
	// commits it holds: those tip reaches and from does not.
	type livePack struct{ id, from, tip string }
	live := make([]livePack, len(packs))
	for i, p := range packs {
		from := code.ZeroOID
		if i > 0 {
			from = commits[i-1]
		}
		live[i] = livePack{id: p.PackID, from: from, tip: commits[i]}
	}
	tip, made := commits[len(commits)-1], len(commits)
	// retireThree lands two more pushes, has the node hold them, and lands a
	// compaction retiring them and the pack below them into one output, which
	// the node does not hold yet: what its next sync swaps out. It answers
	// that compaction's position.
	retireThree := func() statelog.Position {
		b.Helper()
		two := extend(b, client, src, tip, made, 2, swapPushBytes)
		made += len(two)
		var at statelog.Position
		from := tip
		for i, p := range commitPushes(b, client, src, tip, two) {
			var pack code.PackRef
			pack, at = f.propose(b, id, p)
			live = append(live, livePack{id: pack.PackID, from: from, tip: two[i]})
			from = two[i]
		}
		f.sync(b, node, id, at)
		three := live[len(live)-3:]
		var ids []string
		for _, p := range three {
			ids = append(ids, p.id)
		}
		// THE OUTPUT HOLDS EXACTLY ITS INPUTS' OBJECTS: the commits the newest
		// reaches and the oldest's from does not.
		revs := []string{two[1]}
		if three[0].from != code.ZeroOID {
			revs = append(revs, "^"+three[0].from)
		}
		bare := gittest.Empty(b)
		checksum := gittest.Pack(b, client, bare, src, revs...)
		tip = two[1]
		cid := statelog.NewOpID(time.Now(), "")
		path := filepath.Join(bare, "objects", "pack", "pack-"+checksum+".pack")
		out, err := code.LayBundle(code.DeriveOutputPackID(cid, 0), path, checksum)
		if err != nil {
			b.Fatal(err)
		}
		if err := code.StoreBundle(b.Context(), f.store, path, out, nil); err != nil {
			b.Fatal(err)
		}
		slices.Sort(ids)
		res, err := f.fleet.Data().Served().ProposeCompaction(b.Context(), code.CompactionProposal{
			Compaction: code.Compaction{RepoID: id, CompactionID: cid, Node: node.ID(), Seen: at, Retire: ids,
				Add: []code.PackRef{*out}}})
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			b.Fatalf("the compaction retiring the three newest packs: %+v, %v", res, err)
		}
		live = append(live[:len(live)-3], livePack{id: out.PackID, from: three[0].from, tip: two[1]})
		return res.Position
	}
	swaps := func() uint64 { return counterOn(node, metrics.CodeCacheSwaps) }
	before := swaps()
	for b.Loop() {
		b.StopTimer()
		at := retireThree()
		b.StartTimer()
		f.sync(b, node, id, at)
	}
	b.StopTimer()
	if got := swaps() - before; got != uint64(b.N) {
		b.Fatalf("%d syncs after a compaction swapped %d times, want every one", b.N, got)
	}
	// THE FILES ONE SWAP LINKS, counted on one more: the copy before it held
	// by a reader, so both directories stand side by side.
	held, err := node.Cache().Sync(b.Context(), id, codetest.Stale)
	if err != nil {
		b.Fatal(err)
	}
	at := retireThree()
	next, err := node.Cache().Sync(b.Context(), id, statelog.FloorAt(statelog.ReadSession, at))
	if err != nil {
		b.Fatal(err)
	}
	links := linkedFiles(b, held.Dir, next.Dir)
	held.Release()
	next.Release()
	b.ReportMetric(float64(links), "links/swap")
}

// counterOn is a counter's total on node over every series of it.
func counterOn(node codetest.Node, name string) uint64 {
	var total uint64
	for _, s := range node.Metrics().Read() {
		if s.Name == name {
			total += s.Total
		}
	}
	return total
}

// linkedFiles is how many files under next are the same file as one under
// held — hard links a swap made of what it kept.
func linkedFiles(b *testing.B, held, next string) int {
	b.Helper()
	if held == next {
		b.Fatalf("the sync after a compaction serves %s still, want the copy's next directory", held)
	}
	old := map[string]os.FileInfo{}
	if err := filepath.WalkDir(held, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(held, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		old[rel] = info
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	var links int
	if err := filepath.WalkDir(next, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(next, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if was, ok := old[rel]; ok && os.SameFile(was, info) {
			links++
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	return links
}

// benchPoll is poll/repos=n: a pass over n listed repositories none of which
// is due, and the data node's read of the listing's page of n.
func benchPoll(b *testing.B, n int) {
	listed := make([]code.CompactionCandidate, n)
	rng := rand.New(rand.NewPCG(uint64(n), 5))
	for i := range listed {
		// AT THE FLOOR AND UNDER THE BOUND, most first: a listing of
		// repositories a pass plans nothing for, none it raises the alarm
		// for.
		listed[i] = code.CompactionCandidate{ID: uuid.NewString(), Name: fmt.Sprintf("repo-%05d", i),
			LivePacks:   int64(code.CompactMinMerge + rng.IntN(code.MaxPacksPerRepo-code.CompactMinMerge)),
			LiveObjects: int64(1 + rng.IntN(1_000_000)),
			Version: statelog.Position{Stream: topics.CodeLogStream, Generation: 1,
				Seq: uint64(1 + rng.IntN(1<<20))}}
	}
	// THE LISTING'S OWN ORDER, most live packs first and then by id, as the
	// data node serves it.
	slices.SortFunc(listed, func(x, y code.CompactionCandidate) int {
		return cmp.Or(cmp.Compare(y.LivePacks, x.LivePacks), strings.Compare(y.ID, x.ID))
	})
	// THE PASS, on the home of every listed repository and on a git node home
	// to an eighth of them, as each node of a fleet of eight is: only a
	// repository's home ever plans it, so only its home remembers it as not
	// due, and every other listed repository is judged against the pass's one
	// reading of who serves git — here a set's lookup, the router's own
	// judging priced apart (estate's BenchmarkHomeOf).
	for _, arm := range []struct {
		name string
		home func(i int) bool
	}{
		{"home=all", func(int) bool { return true }},
		{fmt.Sprintf("home=1of%d", eightNodes), func(i int) bool { return i%eightNodes == 0 }},
	} {
		b.Run("op=pass/"+arm.name, func(b *testing.B) {
			store, err := objstore.NewStore(memobj.New())
			if err != nil {
				b.Fatal(err)
			}
			home := homeTo{}
			for i, cand := range listed {
				if arm.home(i) {
					home[cand.ID] = true
				}
			}
			reached := &unreached{}
			c, err := compact.New(compact.Options{Cache: reached, Objects: countingPuts{Store: store, t: &traffic{}},
				Estate: pagedListing{due: listed}, Homes: home, NodeID: "bench", Limits: code.DefaultLimits,
				Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				b.Fatal(err)
			}
			for _, cand := range listed {
				if home[cand.ID] {
					compact.RememberNotDue(c, cand)
				}
			}
			for b.Loop() {
				report, err := c.Poll(b.Context())
				if err != nil || report.Listed != n || report.Home != 0 {
					b.Fatalf("a pass over %d repositories none of which is due here: %+v, %v", n, report, err)
				}
			}
			if calls := reached.calls.Load(); calls > 0 {
				b.Fatalf("a pass over repositories none of which is due here reached the cache %d times", calls)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/repo")
		})
	}
	b.Run("op=page", func(b *testing.B) {
		r := seededListing(b, listed)
		q := code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: code.CompactMinMerge, Fresh: codetest.Stale}
		for b.Loop() {
			// ONE PAGE HOLDS THEM ALL, up to code.MaxSnapshotItems — and a page
			// filled to that carries a cursor whatever follows it, since
			// nothing past its last row is read.
			a, err := r.Read(b.Context(), q)
			if err != nil || len(a.Due) != n || (n < code.MaxSnapshotItems && a.Next != "") {
				b.Fatalf("one page of the listing of %d: %d rows, next %q, %v", n, len(a.Due), a.Next, err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "page-ms")
	})
}

// homeTo is a node's homes from a fixed set — the repositories it is the home
// of, as one reading of who serves git names them — answered by a lookup each,
// so a pass's own share of what its homes cost is what the poll arms price.
type homeTo map[string]bool

func (h homeTo) HomeOf(repoIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range repoIDs {
		if h[id] {
			out[id] = true
		}
	}
	return out, nil
}

// pagedListing is a code log whose due listing is due, served in the pages a
// data node serves it in (code.MaxSnapshotItems rows), and which proposes
// nothing.
type pagedListing struct{ due []code.CompactionCandidate }

func (l pagedListing) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if q.Kind != code.ReadCompactionDue {
		return code.ReadAnswer{}, fmt.Errorf("a pass over a listing read %s", q.Kind)
	}
	from := 0
	if q.Cursor != "" {
		if _, err := fmt.Sscan(q.Cursor, &from); err != nil {
			return code.ReadAnswer{}, err
		}
	}
	to := min(from+code.MaxSnapshotItems, len(l.due))
	a := code.ReadAnswer{Due: slices.Clone(l.due[from:to])}
	if to < len(l.due) {
		a.Next = fmt.Sprint(to)
	}
	return a, nil
}

func (pagedListing) Compact(context.Context, code.CompactionProposal) (code.CompactResult, error) {
	return code.CompactResult{}, errors.New("a pass over repositories none of which is due proposed a compaction")
}

// unreached is a compactor's cache that a pass over repositories none of which
// is due must never reach: every call is counted and refused.
type unreached struct{ calls atomic.Int64 }

var errUnreached = errors.New("a pass over repositories none of which is due reached the cache")

func (u *unreached) Sync(context.Context, string, statelog.Freshness) (cache.Synced, error) {
	u.calls.Add(1)
	return cache.Synced{}, errUnreached
}

func (u *unreached) Packs(cache.Synced) []code.PackRow {
	u.calls.Add(1)
	return nil
}

func (u *unreached) AdmitCompaction(cache.Synced, int64, code.Limits) (*cache.Compacting, error) {
	u.calls.Add(1)
	return nil, errUnreached
}

func (u *unreached) NewMergeScratch(context.Context, cache.Synced, string, []string) (*cache.Scratch, error) {
	u.calls.Add(1)
	return nil, errUnreached
}

func (u *unreached) Adopt(context.Context, string, *cache.Scratch, ...code.PackRef) error {
	u.calls.Add(1)
	return errUnreached
}

// seededListing is a data node's code reader over an estate holding one
// active repository row for each of listed — every figure the listing reads,
// nothing else — with no statistics gathered, the state every node's estate is
// in: what ONE page of the due listing costs the node serving it.
func seededListing(b *testing.B, listed []code.CompactionCandidate) *code.Reader {
	b.Helper()
	node, db := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "estate.db"), store.Options{}, 1)
	b.Cleanup(func() { _ = node.Close() })
	vars := db.Caps().MaxVariables
	if err := db.Tx(b.Context(), func(tx *sql.Tx) error {
		_, err := store.InsertRows(b.Context(), tx, vars,
			`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner, default_branch, created_at,
				live_packs, live_objects, version) VALUES `,
			`(?, ?, 'active', 'config', 'unit', 'ENG', 'main', 0, ?, ?, ?)`, ``, len(listed),
			func(i int) []any {
				c := listed[i]
				return []any{c.ID, c.Name, c.LivePacks, c.LiveObjects, int64(c.Version.Seq)}
			})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	authority, err := statelogtest.LocalReader(code.Domain{}, db, statelog.Position{})
	if err != nil {
		b.Fatal(err)
	}
	r, err := code.NewReader(code.ReaderOptions{Log: authority})
	if err != nil {
		b.Fatal(err)
	}
	return r
}
