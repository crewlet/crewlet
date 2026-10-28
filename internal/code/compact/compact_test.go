package compact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// TestMain is the hook when real git starts this binary as proc-receive: a
// case's pushes go through the production relay.
func TestMain(m *testing.M) {
	if status, ok := codetest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	os.Exit(m.Run())
}

// fixture is one solo fleet holding codetest.RepoName, a person pushing to it
// from a work tree, and what a case's own compactor is built over: the git
// node's cache and the fleet's object store, the data node's reads and its
// committer.
type fixture struct {
	t     *testing.T
	fleet *codetest.SoloFleet
	c     *codetest.Case
	p     *codetest.Person
	work  string
	store *objstore.Store
	rng   *rand.Rand
	// pushes is how many pushes the fixture has made.
	pushes int
}

// fixtureProcesses is every fixture's git pool's read lane: four, so the
// maintenance lane is two slots and the maintainer keeps one of them, on any
// machine — what a case judging a compaction's admission counts on.
const fixtureProcesses = 4

// newFixture is a fleet holding an empty codetest.RepoName.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	gittest.RequireNode(t)
	f := codetest.SoloNode(t, codetest.SoloOptions{MaxProcesses: fixtureProcesses})
	f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	store, err := objstore.NewStore(f.Objects())
	if err != nil {
		t.Fatal(err)
	}
	x := &fixture{t: t, fleet: f, c: &codetest.Case{T: t, Fleet: f, Ctx: t.Context()}, store: store,
		rng: rand.New(rand.NewChaCha8(sha256.Sum256([]byte(t.Name()))))}
	x.p = codetest.NewPerson(t, f, "ceo")
	x.work = x.p.Init()
	return x
}

// push makes one push of one commit to refs/heads/main, adding a file of
// seeded random bytes — incompressible, as most of a push's new bytes are —
// for each of sizes.
func (x *fixture) push(sizes ...int) string {
	x.t.Helper()
	files := map[string]string{}
	for i, n := range sizes {
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(x.rng.Uint32())
		}
		files[fmt.Sprintf("p%04d/f%02d.bin", x.pushes, i)] = string(b)
	}
	x.pushes++
	head := x.p.Commit(x.work, files, fmt.Sprintf("push %d", x.pushes))
	x.c.MustPush(x.p, x.work, "main")
	return head
}

// pushes makes n pushes, each of sizes.
func (x *fixture) pushN(n int, sizes ...int) {
	x.t.Helper()
	for range n {
		x.push(sizes...)
	}
}

// node is the fleet's one git node.
func (x *fixture) node() codetest.Node { return x.fleet.Nodes()[0] }

// rows are the repository's rows, read linearizable.
func (x *fixture) rows() codetest.Rows {
	x.t.Helper()
	return x.c.Rows()
}

// candidate is the repository as a due listing names it — at the floor of one
// live pack, whatever the plan makes of it.
func (x *fixture) candidate() code.CompactionCandidate {
	x.t.Helper()
	a, err := x.fleet.Data().Served().Read(x.c.Ctx, code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: 1,
		Fresh: codetest.Linearizable})
	if err != nil {
		x.t.Fatalf("the due listing: %v", err)
	}
	for _, cand := range a.Due {
		if cand.Name == codetest.RepoName {
			return cand
		}
	}
	x.t.Fatalf("the due listing names no %s: %+v", codetest.RepoName, a.Due)
	return code.CompactionCandidate{}
}

// served is the data node's code half as a compactor reaches it: its reads,
// and its committer's decide of a compaction.
type served struct{ f *codetest.SoloFleet }

func (s served) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return s.f.Data().Served().Read(ctx, q)
}

func (s served) Compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	return s.f.Data().Served().ProposeCompaction(ctx, p)
}

// homes answers every question of which repositories this node is the home
// of with home for every one asked, or err.
type homes struct {
	home bool
	err  error
}

func (h homes) HomeOf(repoIDs []string) (map[string]bool, error) {
	if h.err != nil {
		return nil, h.err
	}
	out := map[string]bool{}
	for _, id := range repoIDs {
		out[id] = h.home
	}
	return out, nil
}

// askedHomes is a compactor's homes that records every question it is asked —
// the ids of each, one entry a reading — and answers home for every one while
// home is set.
type askedHomes struct {
	mu    sync.Mutex
	home  bool
	asked [][]string
}

func (h *askedHomes) HomeOf(repoIDs []string) (map[string]bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, slices.Clone(repoIDs))
	out := map[string]bool{}
	for _, id := range repoIDs {
		out[id] = h.home
	}
	return out, nil
}

// readings is every question asked so far.
func (h *askedHomes) readings() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked)
}

// setHome makes every later answer home, or not.
func (h *askedHomes) setHome(home bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.home = home
}

// compactorOf builds a compactor over x's git node's cache and the fleet's
// store, listing and proposing through the data node, home for every
// repository — each of which edit may replace before New.
func (x *fixture) compactorOf(edit func(*compact.Options)) (*compact.Compactor, *metrics.Recorder, *logs) {
	x.t.Helper()
	rec, err := metrics.New()
	if err != nil {
		x.t.Fatal(err)
	}
	l := &logs{}
	o := compact.Options{Cache: x.node().Cache(), Objects: x.store, Estate: served{x.fleet}, Homes: homes{home: true},
		NodeID: x.node().ID(), Limits: code.DefaultLimits, Metrics: rec, Logger: l.logger()}
	if edit != nil {
		edit(&o)
	}
	c, err := compact.New(o)
	if err != nil {
		x.t.Fatalf("compact.New: %v", err)
	}
	x.t.Cleanup(func() { c.Stop(context.Background()) })
	return c, rec, l
}

// compactWhileBusy is c's compaction of cand, asked again while it answers
// busy: the node's maintainer keeps the one slot of the maintenance lane a
// compaction may take for a moment at a time (a commit-graph after a sync), so
// a case — or a benchmark's arm — that judges something else waits it out, for
// a minute at the most.
func compactWhileBusy(t testing.TB, c *compact.Compactor, cand code.CompactionCandidate) (compact.Outcome, error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := c.Compact(t.Context(), cand)
		if out != compact.OutcomeBusy || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pollWhileBusy is c's pass, made again while the pass found its one
// compaction busy (compactWhileBusy's reason).
func pollWhileBusy(t testing.TB, c *compact.Compactor, rec *metrics.Recorder) compact.PollReport {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		busy := counter(rec, metrics.CodeCompactRuns, "outcome", string(compact.OutcomeBusy))
		report, err := c.Poll(t.Context())
		if err != nil {
			t.Fatalf("a pass: %v", err)
		}
		if counter(rec, metrics.CodeCompactRuns, "outcome", string(compact.OutcomeBusy)) == busy ||
			time.Now().After(deadline) {
			return report
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// counter is a counter's total over the series whose attribute attr is value.
func counter(rec *metrics.Recorder, name, attr, value string) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name == name && s.Attrs[attr] == value {
			n += s.Total
		}
	}
	return n
}

// logs is what a case's compactor logged, JSON a line.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// count is how many lines carry msg and every one of attrs (JSON fragments).
func (l *logs) count(msg string, attrs ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line := range strings.Lines(l.buf.String()) {
		if !strings.Contains(line, `"msg":"`+msg+`"`) {
			continue
		}
		all := true
		for _, a := range attrs {
			all = all && strings.Contains(line, a)
		}
		if all {
			n++
		}
	}
	return n
}

// String is every line, for a failure to quote.
func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// counted is a compaction's cache as a case watches it: every sync and every
// read of the live packs counted, and its admission answered by admit where
// admit is set.
type counted struct {
	compact.Cache
	mu    sync.Mutex
	syncs int
	plans int
	admit func(s cache.Synced, objects int64, limits code.Limits) (*cache.Compacting, error)
}

func countedOver(c *cache.Cache) *counted { return &counted{Cache: c} }

func (c *counted) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error) {
	c.mu.Lock()
	c.syncs++
	c.mu.Unlock()
	return c.Cache.Sync(ctx, repoID, fresh)
}

func (c *counted) Packs(s cache.Synced) []code.PackRow {
	c.mu.Lock()
	c.plans++
	c.mu.Unlock()
	return c.Cache.Packs(s)
}

func (c *counted) AdmitCompaction(s cache.Synced, objects int64, limits code.Limits) (*cache.Compacting, error) {
	c.mu.Lock()
	admit := c.admit
	c.mu.Unlock()
	if admit != nil {
		return admit(s, objects, limits)
	}
	return c.Cache.AdmitCompaction(s, objects, limits)
}

// answerAdmissions has every later admission answered by admit; nil asks the
// cache.
func (c *counted) answerAdmissions(admit func(cache.Synced, int64, code.Limits) (*cache.Compacting, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.admit = admit
}

// counts is the syncs and plans so far.
func (c *counted) counts() (syncs, plans int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncs, c.plans
}

// noMemory is an admission the node's memory refuses, as the cache answers it.
func noMemory(cache.Synced, int64, code.Limits) (*cache.Compacting, error) {
	return nil, fmt.Errorf("cache: compact %s: %w: a case's node too small for the build", codetest.RepoName,
		cache.ErrNoMemory)
}

// listed is a compactor's reads of the code host with every due listing
// answered for codetest.RepoName at the floor of one live pack, and its live
// packs shown as livePacks where that is set: a repository a listing names
// whatever its plan makes of it — at the pack bound, or with fewer packs than
// a production listing's floor — without pushing as many.
type listed struct {
	compact.Estate
	mu        sync.Mutex
	livePacks int64
}

func (l *listed) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if q.Kind != code.ReadCompactionDue {
		return l.Estate.Read(ctx, q)
	}
	q.MinPacks = 1
	a, err := l.Estate.Read(ctx, q)
	if err != nil {
		return a, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range a.Due {
		if l.livePacks > 0 {
			a.Due[i].LivePacks = l.livePacks
		}
	}
	return a, nil
}

// showPacks has every later listing show livePacks live packs.
func (l *listed) showPacks(n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.livePacks = n
}

// recordingStore is a compactor's object store with every put recorded — its
// size and the key it minted — and held by hold, where hold is set, before it
// reaches the store; a put answered by refuse, where that is set, never
// reaches it.
type recordingStore struct {
	*objstore.Store
	mu     sync.Mutex
	puts   []objstore.Object
	sizes  []int64
	hold   func(ctx context.Context) error
	refuse func() error
	usage  func() (objstore.Usage, bool, error)
}

func (r *recordingStore) Put(ctx context.Context, rd io.Reader, limit int64, m objstore.PutMeta) (objstore.Object,
	error) {
	r.mu.Lock()
	hold, refuse := r.hold, r.refuse
	r.sizes = append(r.sizes, limit)
	r.mu.Unlock()
	if hold != nil {
		if err := hold(ctx); err != nil {
			return objstore.Object{}, err
		}
	}
	if refuse != nil {
		if err := refuse(); err != nil {
			_, _ = io.Copy(io.Discard, rd)
			return objstore.Object{}, err
		}
	}
	obj, err := r.Store.Put(ctx, rd, limit, m)
	if err == nil {
		r.mu.Lock()
		r.puts = append(r.puts, obj)
		r.mu.Unlock()
	}
	return obj, err
}

func (r *recordingStore) Usage(ctx context.Context) (objstore.Usage, bool, error) {
	r.mu.Lock()
	usage := r.usage
	r.mu.Unlock()
	if usage != nil {
		return usage()
	}
	return r.Store.Usage(ctx)
}

// stored is every object a put stored, and every size a put was handed.
func (r *recordingStore) stored() ([]objstore.Object, []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.puts), slices.Clone(r.sizes)
}

// set changes the store's hold, refusal and usage under its lock.
func (r *recordingStore) set(edit func(r *recordingStore)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	edit(r)
}

// pushRef is the principal a case's own proposals are made as.
var pushRef = code.Principal{Kind: code.PrincipalOperator, OperatorID: "compact-test", Seat: "ceo"}

// proposePack stores the pack named checksum in bare as one bundle and
// proposes it as a push creating ref at tip, through the data node's committer
// — a push no person's git made, carrying exactly the objects a case packed.
func (x *fixture) proposePack(bare, checksum, ref, tip string) code.PackRow {
	x.t.Helper()
	pushID := uuid.Must(uuid.NewV7()).String()
	path := filepath.Join(bare, "objects", "pack", "pack-"+checksum+".pack")
	pack, err := code.LayBundle(pushID, path, checksum)
	if err != nil {
		x.t.Fatal(err)
	}
	if err := code.StoreBundle(x.c.Ctx, x.store, path, pack, nil); err != nil {
		x.t.Fatalf("store pack-%s: %v", checksum, err)
	}
	rows := x.rows()
	res, err := x.fleet.Data().Served().Propose(x.c.Ctx, code.Proposal{
		PushID: pushID, RepoID: rows.Repo.ID, Principal: pushRef, Via: code.ViaGit, Node: x.node().ID(),
		ReceivedAt: time.Now().UTC(), Pack: pack,
		Commands: []code.Command{{Ref: ref, Old: code.ZeroOID, New: tip}},
	})
	if err != nil || res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
		res.Answer.Commands[0].Result != code.ResultOK {
		x.t.Fatalf("the push of pack-%s: %+v, %v", checksum, res, err)
	}
	for _, r := range x.rows().Packs {
		if r.PackID == pushID {
			return r
		}
	}
	x.t.Fatalf("the push of pack-%s applied and no row names it", checksum)
	return code.PackRow{}
}

// objectIDs is every object id the bundle of r holds, read back from the
// store: its index's names.
func (x *fixture) objectIDs(r code.PackRow) []string {
	x.t.Helper()
	ctx, cancel := context.WithTimeout(x.c.Ctx, time.Minute)
	defer cancel()
	rc, err := x.store.Open(ctx, r.Ref().Content())
	if err != nil {
		x.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	all, err := io.ReadAll(rc)
	if err != nil {
		x.t.Fatal(err)
	}
	idx := filepath.Join(x.t.TempDir(), "pack.idx")
	if err := os.WriteFile(idx, all[r.Ref().PackBytes():r.Ref().PackBytes()+r.IndexBytes], 0o600); err != nil {
		x.t.Fatal(err)
	}
	n, f, err := code.OpenIdx(idx)
	if err != nil {
		x.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ids := make([]string, n)
	for i := range ids {
		var oid [20]byte
		if _, err := io.ReadFull(f, oid[:]); err != nil {
			x.t.Fatal(err)
		}
		ids[i] = fmt.Sprintf("%x", oid)
	}
	return ids
}

// errHeld is a hold's answer when its case ended first.
var errHeld = errors.New("the case ended while the put was held")
