package host_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
)

// An engine write, measured (ADR-0092): BenchmarkEngineWrite from the code
// write tool's call to the pack adopted into the copy, BenchmarkEngineCommit
// its build alone. Their figures confirm or replace an engine write's
// budgets (estate.GitWriteAttempt's terms, gitlet.WriteUploadAllowance,
// WriteBuildDeadline, MaxFileOps, MaxWritePackBytes, the git writes' lane's
// width, the seat git-work share's half); the package doc's "Measured" and
// gitlet.md's "What an engine write costs" carry them, with the machine,
// git and object store.

// writeStore is an object store an engine write's arms run over: the
// in-memory twin, or the S3 fake behind the production backend answering each
// request firstByte after it arrives.
type writeStore struct {
	name      string
	firstByte time.Duration
}

// writeStores are BenchmarkEngineWrite's: the twin, and the S3 fake at both
// ends of AWS's documented 100–200 ms first byte (M2-decisions O15.7), at
// benchConnBytes a connection. No real bucket is measured.
var writeStores = []writeStore{{"mem", 0}, {"s3fake-100ms", 100 * time.Millisecond},
	{"s3fake-200ms", 200 * time.Millisecond}}

// backend is s's backend, for one arm.
func (s writeStore) backend(b *testing.B) objstore.Backend {
	b.Helper()
	if s.firstByte == 0 {
		return memobj.New()
	}
	srv := s3fake.Start(b, "files")
	srv.Latency, srv.ConnBytesPerSecond = s.firstByte, benchConnBytes
	return s3Over(b, srv)
}

// writeFigures are what an arm's Gitlet node did, read from its own seams,
// never a fake's: the host's stage durations
// (crewlet.code.host.write.duration), the seat git-work share's waits
// (crewlet.code.seatwork.waits), the copy's syncs
// (crewlet.code.cache.sync.duration), every object-store request
// (timedObjects) and every proposal (timedEstate). Reset each arm once its
// fixture is built and compacted, so a seed is no sample.
//
// A request's owner is read off the object it names, the one thing the store
// sees: the writes', the person's, or else the node's own (a seed, a
// compaction). Each is stamped with the arm's phase, so the person's requests
// alone are never read as theirs beside the writes.
type writeFigures struct {
	mu sync.Mutex
	// phase is the arm's phase now: what every request is stamped with.
	phase string
	// stages is every observation of crewlet.code.host.write.duration, by
	// its stage: sync, build, land and total.
	stages map[string][]time.Duration
	// shareWaits is every take of the share an engine write's build made,
	// granted or unready.
	shareWaits []time.Duration
	// syncs is every completed sync of the copy, by phase and kind.
	syncs map[string][]time.Duration
	// puts is each object's last PUT's time and putCalls its PUT count, by
	// name; engine and person are the names the writes' and the person's
	// proposals named.
	puts     map[string]time.Duration
	putCalls map[string]int
	engine   map[string]bool
	person   map[string]bool
	// calls is every request the node made of the object store, in order.
	calls []objCall
	// proposals is each engine write's proposal in flight (the committer's
	// batch, record and wait for its row), answered when each ended by push
	// id, and pending how many the committer answered pending past its wait.
	proposals []time.Duration
	answered  map[string]time.Time
	pending   int
	// personProposals is each of the person's proposals in flight, by phase.
	personProposals map[string][]time.Duration
	// packs is each engine write's pack bytes, as its proposal names them.
	packs []int64
}

// objCall is one request the Gitlet node made of the object store, and how
// long the store took to answer it.
type objCall struct {
	phase, op, name string
	took            time.Duration
}

// The phases of an arm, which every request is stamped with.
const (
	phaseAlone   = "alone"
	phaseWrites  = "writes"
	phaseCompact = "compact"
	phaseAfter   = "after"
)

// newWriteFigures is an empty set.
func newWriteFigures() *writeFigures {
	fig := &writeFigures{}
	fig.reset()
	return fig
}

// reset forgets everything so far, the arm's fixture included.
func (fig *writeFigures) reset() {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	fig.stages, fig.engine, fig.person = map[string][]time.Duration{}, map[string]bool{}, map[string]bool{}
	fig.shareWaits, fig.syncs, fig.calls = nil, map[string][]time.Duration{}, nil
	fig.puts, fig.putCalls = map[string]time.Duration{}, map[string]int{}
	fig.proposals, fig.answered, fig.pending, fig.packs = nil, map[string]time.Time{}, 0, nil
	fig.personProposals = map[string][]time.Duration{}
}

// enter stamps everything from now on with phase.
func (fig *writeFigures) enter(phase string) {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	fig.phase = phase
}

// sinceMs is a sink's observation, in milliseconds, as a duration.
func sinceMs(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }

// observe is the sink bound to the host's write durations: called under the
// recorder's own lock, so it keeps nothing of attrs but the stage.
func (fig *writeFigures) observe(ms float64, attrs map[string]string) {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	stage := attrs["stage"]
	fig.stages[stage] = append(fig.stages[stage], sinceMs(ms))
}

// observeShare is the sink bound to the share's waits, keeping an engine
// write's build's whatever their outcome; a seat's read takes the same share,
// and no arm reads.
func (fig *writeFigures) observeShare(ms float64, attrs map[string]string) {
	if attrs["work"] != string(gitlet.SeatWorkWrite) {
		return
	}
	fig.mu.Lock()
	defer fig.mu.Unlock()
	fig.shareWaits = append(fig.shareWaits, sinceMs(ms))
}

// observeSync is the sink bound to the copy's syncs, kept by phase and kind.
func (fig *writeFigures) observeSync(ms float64, attrs map[string]string) {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	key := fig.phase + "/" + attrs["kind"]
	fig.syncs[key] = append(fig.syncs[key], sinceMs(ms))
}

// shares is every engine write's wait for the share, sorted.
func (fig *writeFigures) shares() []time.Duration {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	return inOrder(fig.shareWaits)
}

// owner is whose an object is: the writes', the person's or the node's own.
func (fig *writeFigures) owner(name string) string {
	switch {
	case fig.engine[name]:
		return "writes"
	case fig.person[name]:
		return "person"
	}
	return "node"
}

// requests is every request phase made of the store for objects of owner, by
// request (PUT, GET, STAT, DELETE), each sorted by how long it took.
func (fig *writeFigures) requests(phase, owner string) map[string][]time.Duration {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	out := map[string][]time.Duration{}
	for _, c := range fig.calls {
		if c.phase == phase && fig.owner(c.name) == owner {
			out[c.op] = append(out[c.op], c.took)
		}
	}
	for op := range out {
		out[op] = inOrder(out[op])
	}
	return out
}

// logPhase logs phase's store requests by owner and request, its syncs of the
// copy and the person's proposals: what an arm's report tells a slow push by.
func (fig *writeFigures) logPhase(b *testing.B, phase string) {
	b.Helper()
	for _, owner := range []string{"writes", "person", "node"} {
		for op, took := range fig.requests(phase, owner) {
			b.Logf("%s: %d %s of the %s' objects, p50/p99 %.0f/%.0f ms", phase, len(took), op, owner,
				quantile(took, 0.50), quantile(took, 0.99))
		}
	}
	fig.mu.Lock()
	defer fig.mu.Unlock()
	for key, took := range fig.syncs {
		if kind, ok := strings.CutPrefix(key, phase+"/"); ok {
			took = inOrder(took)
			b.Logf("%s: %d %s syncs of the copy, p50/p99 %.0f/%.0f ms", phase, len(took), kind,
				quantile(took, 0.50), quantile(took, 0.99))
		}
	}
	if took := inOrder(fig.personProposals[phase]); len(took) > 0 {
		b.Logf("%s: %d of the person's proposals, p50/p99 %.0f/%.0f ms", phase, len(took), quantile(took, 0.50),
			quantile(took, 0.99))
	}
}

// stage is every observation of stage, sorted.
func (fig *writeFigures) stage(stage string) []time.Duration {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	return inOrder(fig.stages[stage])
}

// uploads is every engine write's PUT duration, sorted, and the writes'
// objects' PUT count.
func (fig *writeFigures) uploads() ([]time.Duration, int) {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	var out []time.Duration
	calls := 0
	for name, took := range fig.puts {
		if fig.engine[name] {
			out = append(out, took)
			calls += fig.putCalls[name]
		}
	}
	return inOrder(out), calls
}

// inOrder is ds in order, a copy.
func inOrder(ds []time.Duration) []time.Duration {
	out := slices.Clone(ds)
	slices.Sort(out)
	return out
}

// timedObjects is the Gitlet node's object store with every request timed
// into writeFigures.calls and every PUT counted by object name.
type timedObjects struct {
	objstore.Backend
	fig *writeFigures
}

// called keeps one request of the store.
func (o timedObjects) called(op, name string, took time.Duration) {
	o.fig.mu.Lock()
	defer o.fig.mu.Unlock()
	o.fig.calls = append(o.fig.calls, objCall{phase: o.fig.phase, op: op, name: name, took: took})
}

func (o timedObjects) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	start := time.Now()
	err := o.Backend.Put(ctx, name, r, m)
	took := time.Since(start) //fixedwait:measure a benchmark reports this against a tenth of gitlet.WriteUploadAllowance; no gate reads it
	o.called("PUT", name, took)
	o.fig.mu.Lock()
	defer o.fig.mu.Unlock()
	o.fig.putCalls[name]++
	if err == nil {
		o.fig.puts[name] = took
	}
	return err
}

// Get is timed to the store's first byte, never the last: its caller paces
// the reader.
func (o timedObjects) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	start := time.Now()
	rc, err := o.Backend.Get(ctx, name, off, n)
	o.called("GET", name, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
	return rc, err
}

func (o timedObjects) Stat(ctx context.Context, name string) (objstore.Info, error) {
	start := time.Now()
	info, err := o.Backend.Stat(ctx, name)
	o.called("STAT", name, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
	return info, err
}

func (o timedObjects) Delete(ctx context.Context, name string) error {
	start := time.Now()
	err := o.Backend.Delete(ctx, name)
	o.called("DELETE", name, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
	return err
}

// timedEstate is the Gitlet node's estate with every proposal timed and its
// pack's name kept as the writes' (with when it was answered) or the
// person's (by phase).
type timedEstate struct {
	host.Estate
	fig *writeFigures
}

func (e timedEstate) Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error) {
	start := time.Now()
	res, err := e.Estate.Propose(ctx, p)
	end := time.Now()
	e.fig.mu.Lock()
	defer e.fig.mu.Unlock()
	if p.Via != gitlet.ViaEngine {
		//fixedwait:measure a benchmark reports this; no gate reads it
		e.fig.personProposals[e.fig.phase] = append(e.fig.personProposals[e.fig.phase], end.Sub(start))
		if p.Pack != nil {
			e.fig.person[p.Pack.Object.Name()] = true
		}
		return res, err
	}
	e.fig.proposals = append(e.fig.proposals, end.Sub(start)) //fixedwait:measure a benchmark reports this; no gate reads it
	e.fig.answered[p.PushID] = end
	if err == nil && res.Outcome == statelog.OutcomePending {
		e.fig.pending++
	}
	if p.Pack != nil {
		e.fig.engine[p.Pack.Object.Name()] = true
		e.fig.packs = append(e.fig.packs, p.Pack.PackBytes())
	}
	return res, err
}

// writeCall is one tool call's write as timedWriter saw it: when asked and
// answered, and the result, whose push id and floor a seat's tool answer
// omits.
type writeCall struct {
	asked             bool
	askedAt, answerAt time.Time
	res               gitlet.EngineWriteResult
	err               error
}

// writeCallKey carries a tool call's writeCall to timedWriter.
type writeCallKey struct{}

// timedWriter is the fleet's routed seam (estate.CodeSeam) with each engine
// write recorded into the writeCall its context carries.
type timedWriter struct{ estate.CodeSeam }

func (w timedWriter) GitWrite(ctx context.Context, ew gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	call, _ := ctx.Value(writeCallKey{}).(*writeCall)
	start := time.Now()
	res, err := w.CodeSeam.GitWrite(ctx, ew)
	if call != nil {
		call.asked, call.askedAt, call.answerAt, call.res, call.err = true, start, time.Now(), res, err
	}
	return res, err
}

// noRequesters composes nobody, the wiring builtin.CodeDeps requires: no
// arm's turn was woken by anyone, so none asks it.
type noRequesters struct{}

func (noRequesters) Requester(_ context.Context, handle string) (iam.Principal, error) {
	return iam.Principal{}, fmt.Errorf("no requester is composed here, and %q was asked for", handle)
}

// Credential composes nobody either.
func (noRequesters) Credential(_ context.Context, login string) (iam.Principal, bool, error) {
	return iam.Principal{}, false, fmt.Errorf("no requester is composed here, and %q was asked for", login)
}

// writeTools are the real code write tools (builtin.CodeTools) by name, over
// the routed seam with every write timed (timedWriter), as a seat's turn on a
// node holding nothing is handed them.
func writeTools(b *testing.B, seam estate.CodeSeam) map[string]tools.SeatCallable {
	b.Helper()
	out := map[string]tools.SeatCallable{}
	for _, tool := range builtin.CodeTools(builtin.CodeDeps{Code: seam, Writer: timedWriter{CodeSeam: seam},
		Holder: seam, Requesters: noRequesters{}, Chart: authz.NoChart{}}) {
		switch tool.Name() {
		case builtin.CodeCommitFilesTool, builtin.CodeRevertTool, builtin.CodeCreateBranchTool:
			seat, ok := tool.(tools.SeatCallable)
			if !ok {
				b.Fatalf("%s is no tool a seat's turn calls", tool.Name())
			}
			out[tool.Name()] = seat
		}
	}
	if len(out) != 3 {
		b.Fatalf("the code tools serve %d of the three write tools an arm calls", len(out))
	}
	return out
}

// writeFleet is a solo fleet of ONE Gitlet node over backend, its broker
// fsyncing every write as a deployment's does (SyncAlways), its estate,
// object store, write durations, share waits and copy syncs timed into fig,
// and its seats allowed the quota ceiling's pushes a day, since all an arm's
// writes are one seat's.
func writeFleet(b *testing.B, backend objstore.Backend, fig *writeFigures) *gitlettest.SoloFleet {
	b.Helper()
	f := gitlettest.SoloNode(b, gitlettest.SoloOptions{
		Shape: gitlettest.Shape{
			Limits:     gitlet.Limits{PushesPerSeatDay: config.CodePushesPerSeatDayCeiling},
			WrapEstate: func(e host.Estate) host.Estate { return timedEstate{Estate: e, fig: fig} },
		},
		Objects: backend, SyncAlways: true, CacheMaxBytes: benchCacheBytes,
		WrapObjects: func(_ string, o objstore.Backend) objstore.Backend { return timedObjects{Backend: o, fig: fig} },
	})
	node := f.Nodes()[0]
	for name, sink := range map[string]func(float64, map[string]string){
		metrics.CodeHostWriteDuration: fig.observe, metrics.CodeSeatworkWaits: fig.observeShare,
		metrics.CodeCacheSyncDuration: fig.observeSync} {
		if err := node.Metrics().BindHistogram(name, sink); err != nil {
			b.Fatal(err)
		}
	}
	b.Logf("the pool runs git %s; its read lane is %d slots, the seats' share %d units",
		node.Pool().Version(), node.Pool().Slots(gitproc.ClassRead), node.Share().Units())
	return f
}

// The turn a seat's writes are made in: alice, the fixture chart's
// full_access seat, on a work item key and model id of the usual lengths.
const (
	benchModel   = "fixture-model-2031"
	benchWorkKey = "ENG-1234"
)

// benchSeat is alice as a write's principal, in a turn of its own: what a
// seed's write, which no tool makes, is asked as.
func benchSeat() gitlet.Principal {
	return gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: gitlettest.FixtureAlice, Run: gitlet.RunProvenance{
		TurnID: statelog.NewOpID(time.Now(), ""), WorkBackend: string(types.WorkWorklet), WorkKey: benchWorkKey,
		Model: benchModel}}
}

// benchTurn is a fresh turn of alice's as the engine dispatches one, its
// lookups and holds empty, so its write is its first code call and resolves
// the repository's name at the log's head.
func benchTurn(seat *org.Role) *turnctx.Turn {
	return &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat, Lookups: turnctx.NewLookups(),
		Holds: turnctx.NewHolds(nil), WorkItem: &types.WorkItem{Backend: types.WorkWorklet,
			ID: "0199f0a2-7b1c-7cde-8f00-000000001234", Key: benchWorkKey, Project: gitlettest.FixtureUnit}}
}

// The kinds an arm writes.
const (
	writeFiles1  = "commit_files-1"
	writeFiles10 = "commit_files-10"
	writeRevert  = "revert"
	writeBranch  = "create_branch"
)

// writeArm is one BenchmarkEngineWrite arm: its object store, the kind of
// write, how many are asked at once over how many repositories, and whether a
// person pushes to the first beside them (besidePusher).
type writeArm struct {
	store       writeStore
	kind        string
	concurrency int
	repos       int
	beside      bool
}

func (a writeArm) String() string {
	beside := "nothing"
	if a.beside {
		beside = "push"
	}
	return fmt.Sprintf("kind=%s/concurrency=%d/repos=%d/beside=%s", a.kind, a.concurrency, a.repos, beside)
}

// writeArms are one object store's arms: every kind alone; one-file commits
// eight and sixty-four at once, over one repository and over sixty-four; and
// one and sixty-four at once beside a person's pushes.
func writeArms(store writeStore) []writeArm {
	var arms []writeArm
	for _, kind := range []string{writeFiles1, writeFiles10, writeRevert, writeBranch} {
		arms = append(arms, writeArm{store: store, kind: kind, concurrency: 1, repos: 1})
	}
	for _, concurrency := range []int{8, writesAtOnce} {
		for _, repos := range []int{1, writesAtOnce} {
			arms = append(arms, writeArm{store: store, kind: writeFiles1, concurrency: concurrency, repos: repos})
		}
	}
	for _, concurrency := range []int{1, writesAtOnce} {
		arms = append(arms, writeArm{store: store, kind: writeFiles1, concurrency: concurrency, repos: 1,
			beside: true})
	}
	return arms
}

const (
	// writesAtOnce is the widest arm: as many writes at once as the Gitlet
	// node's write lane answers (queue.ConcurrentAnswers), each of its own
	// turn: two nodes' whole complement of turns, each inside one write.
	writesAtOnce = 64

	// besidePace is how often a beside=push arm's person pushes one commit:
	// a busy repository's people and their CI together.
	besidePace = 100 * time.Millisecond

	// besideAlone is how many of the person's pushes are timed alone, before
	// the writes and again after them: what the pushes beside are held to.
	besideAlone = 20

	// writesAUnitServes is how many writes at once ONE unit of the seat
	// git-work share serves inside gitlet.GitShareWait: its two seconds over a
	// build's p99 on two cores, 0.24 s at most (a hundred files at depth six,
	// BenchmarkEngineCommit), is eight. An arm asking no more than the units
	// serve must answer none unready or busy: a refusal there is the write
	// path turning writes into refusals, never the share's bound at work.
	writesAUnitServes = int(gitlet.GitShareWait / (240 * time.Millisecond))

	// compactPasses bounds the compactor's passes at one pause: a repository
	// past gitlet.CompactTargetPacks may need a few, and thirty-two is far
	// past any a round of sixty-four writes leaves.
	compactPasses = 32

	// seedAtOnce is how many branches are seeded at once, few enough that a
	// one-unit share serves each inside gitlet.GitShareWait at any build this
	// package measures; seedTries how often a seed answered unready is asked.
	seedAtOnce = 4
	seedTries  = 10
)

// BenchmarkEngineWrite times an engine write (ADR-0092) from a seat's turn
// calling a real code write tool (builtin.CodeTools) on a node holding nothing,
// through the production estate.Router over the broker, to the tool's answer,
// given once the push's row applied and its pack is in the copy
// (gitletcluster's BenchmarkEngineWrite asks it in process). Each write is the
// first call of a turn of alice's, onto a branch its worker alone moves. Arms:
// writeStores by writeArms. Between rounds, never beside one, the compactor
// runs dry (compact) with the person's pushes held off, so the rounds and the
// person's pushes alone, beside and after the writes see one pack state. Every
// answer is reported, refusals included, since a turn waits for those too.
// Budgets are logged, never failed on, and the assertions count rather than
// time (run). The package doc's "Measured" carries the figures.
func BenchmarkEngineWrite(b *testing.B) {
	gitlettest.Quiet(b)
	for _, store := range writeStores {
		b.Run("objects="+store.name, func(b *testing.B) {
			for _, arm := range writeArms(store) {
				b.Run(arm.String(), func(b *testing.B) { newWriteRig(b, arm).run(b) })
			}
		})
	}
}

// writeBranchState is worker's branch in repository repo, which only worker
// writes, and its head as the last write left it.
type writeBranchState struct {
	worker, repo int
	ref, head    string
}

// writeRig is one BenchmarkEngineWrite arm's fixture and its figures.
type writeRig struct {
	arm  writeArm
	f    *gitlettest.SoloFleet
	node gitlettest.Node
	fig  *writeFigures
	// seam is Gitlet as the fleet routes it, seat every write's seat, and
	// calls the write tools by name.
	seam  estate.CodeSeam
	seat  *org.Role
	calls map[string]tools.SeatCallable
	// repos and names are the arm's repositories, the first RepoName.
	repos, names []string
	// branches is every branch the arm's writes move, by worker and repository.
	branches map[[2]int]*writeBranchState
	// created numbers the create_branch arm's branches apart.
	created atomic.Int64

	// quiet holds the person's pushes off during a compaction: a push takes
	// it shared, a compaction whole.
	quiet sync.RWMutex
	// compacting is the compactions' time, left out of the arm's rate.
	compacting time.Duration

	mu sync.Mutex
	// answers is every write's whole time, whatever its tool answered, and
	// refusedTook the unready ones'.
	answers, refusedTook []time.Duration
	// took is every applied write's whole time, and tool what its tool call
	// added around its write; ended is when each write's answer reached its
	// tool, by push id.
	took, tool []time.Duration
	ended      map[string]time.Time
	// unready and busy count the writes answered so; failed is the first
	// answer no healthy write is given.
	unready, busy int
	failed        error
}

// newWriteRig builds arm's fleet, declares its repositories, seeds every
// branch its writes will move and compacts what the seeds left.
func newWriteRig(b *testing.B, arm writeArm) *writeRig {
	b.Helper()
	r := &writeRig{arm: arm, fig: newWriteFigures(), branches: map[[2]int]*writeBranchState{},
		ended: map[string]time.Time{}, seat: &org.Role{Name: gitlettest.FixtureAlice}}
	r.f = writeFleet(b, arm.store.backend(b), r.fig)
	r.node = r.f.Nodes()[0]
	r.seam = r.f.Code(b)
	r.calls = writeTools(b, r.seam)
	declared := make([]gitlet.Declared, arm.repos)
	for i := range declared {
		name := gitlettest.RepoName
		if i > 0 {
			name = fmt.Sprintf("bench-%02d", i)
		}
		declared[i] = gitlet.Declared{Name: name, Settings: gitlettest.Settings()}
	}
	r.f.Declare(b, declared...)
	for _, d := range declared {
		rows, err := r.f.Rows(b.Context(), d.Name, gitlettest.Linearizable)
		if err != nil {
			b.Fatalf("the rows of %s: %v", d.Name, err)
		}
		r.repos, r.names = append(r.repos, rows.Repo.ID), append(r.names, d.Name)
	}
	// Every branch a round will move gets two commits before the timer, so a
	// revert has a change to undo: worker k's write of round i is to
	// repository (k + i×concurrency) mod repos, so the branches are the pairs
	// that formula reaches.
	var todo []*writeBranchState
	for k := range arm.concurrency {
		for i := range arm.repos {
			repo := (k + i*arm.concurrency) % arm.repos
			if _, ok := r.branches[[2]int{k, repo}]; ok {
				continue
			}
			br := &writeBranchState{worker: k, repo: repo,
				ref: fmt.Sprintf("refs/heads/seats/%s/w%02d", gitlettest.FixtureAlice, k)}
			r.branches[[2]int{k, repo}] = br
			todo = append(todo, br)
		}
	}
	r.seed(b, todo)
	r.compact(b)
	r.fig.reset()
	return r
}

// compact runs the compactor until a pass compacts nothing, as a home that
// keeps up would, with the person's pushes held off and its requests stamped
// phaseCompact; its time is left out of the arm's rate. Production polls once
// a minute, so a burst leaves it more packs; the package doc's "Measured" has
// what an uncompacted repository costs a push. A failed compaction fails the
// arm: what followed would run over packs no production home leaves.
func (r *writeRig) compact(b *testing.B) {
	b.Helper()
	r.quiet.Lock()
	defer r.quiet.Unlock()
	r.fig.mu.Lock()
	was := r.fig.phase
	r.fig.mu.Unlock()
	r.fig.enter(phaseCompact)
	defer r.fig.enter(was)
	start := time.Now()
	defer func() { r.compacting += time.Since(start) }() //fixedwait:measure a benchmark leaves it out of the rate it reports; no gate reads it
	for range compactPasses {
		report, err := r.node.Compactor().Poll(b.Context())
		switch {
		case err != nil:
			b.Fatalf("the compactor's pass: %v", err)
		case report.Failed > 0:
			b.Fatalf("the compactor's pass failed a compaction: %+v", report)
		case report.Compacted == 0:
			return
		}
	}
	b.Fatalf("the compactor still compacted after %d passes", compactPasses)
}

// livePacks is how many live packs the arm's first repository holds now.
func (r *writeRig) livePacks(b *testing.B) int {
	b.Helper()
	rows, err := r.f.Rows(b.Context(), r.names[0], gitlettest.Linearizable)
	if err != nil {
		b.Fatalf("the rows of %s: %v", r.names[0], err)
	}
	return len(rows.Packs)
}

// seed lands two commits on every branch of todo, seedAtOnce at a time,
// through the fleet's routing directly: no tool makes a fixture's writes.
func (r *writeRig) seed(b *testing.B, todo []*writeBranchState) {
	b.Helper()
	errs := make(chan error, len(todo))
	slots := make(chan struct{}, seedAtOnce)
	var wg sync.WaitGroup
	for _, br := range todo {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			for i, content := range []string{"seed\n", "changed\n"} {
				w := gitlet.EngineWrite{Kind: gitlet.WriteCommitFiles, RepoID: r.repos[br.repo],
					Principal: benchSeat(), WrittenFor: types.ForNobody(), OpID: opNow(),
					Fresh: statelog.FloorAt(statelog.ReadSession, r.seam.FloorOf(r.repos[br.repo])), Ref: br.ref,
					ExpectedHead: br.head, Message: fmt.Sprintf("seed %d", i),
					Files: []gitlet.FileOp{text(r.path(br, 0), content)}}
				res, err := r.landSeed(b.Context(), w)
				if err != nil {
					errs <- fmt.Errorf("seeding %s of %s: %w", br.ref, r.names[br.repo], err)
					return
				}
				br.head = res.Commit
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		b.Fatal(err)
	}
}

// landSeed asks w again a share's wait later while it is answered unready, at
// most seedTries times, and fails on any answer but applied.
func (r *writeRig) landSeed(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	for try := 1; ; try++ {
		res, err := r.f.Write(ctx, w)
		switch {
		case unready(err) && try < seedTries:
			time.Sleep(gitlet.GitShareWait) //fixedwait:liveness
			continue
		case err != nil:
			return res, err
		case res.Outcome != statelog.OutcomeApplied || res.Refusal != "" || res.Commit == "":
			return res, fmt.Errorf("answered %+v", res)
		}
		return res, nil
	}
}

// path is the j-th file a branch's writes put, in its worker's directory.
func (r *writeRig) path(br *writeBranchState, j int) string {
	return fmt.Sprintf("w%02d/f%d.txt", br.worker, j)
}

// callOf is br.worker's write of round i as a seat's turn calls its tool:
// every revision a whole id, so the tool resolves only the repository's name.
func (r *writeRig) callOf(br *writeBranchState, i int) (tools.SeatCallable, map[string]any) {
	branch := strings.TrimPrefix(br.ref, "refs/heads/")
	args := map[string]any{"repo": r.names[br.repo], "branch": branch}
	switch r.arm.kind {
	case writeFiles1, writeFiles10:
		n := 1
		if r.arm.kind == writeFiles10 {
			n = 10
		}
		var files []any
		for j := range n {
			files = append(files, map[string]any{"op": "put", "path": r.path(br, j),
				"content": fmt.Sprintf("worker %d round %d file %d\n", br.worker, i, j)})
		}
		args["expected_head"], args["message"], args["files"] = br.head, fmt.Sprintf("round %d", i), files
		return r.calls[builtin.CodeCommitFilesTool], args
	case writeRevert:
		args["expected_head"], args["commit"], args["message"] = br.head, br.head, fmt.Sprintf("revert %d", i)
		return r.calls[builtin.CodeRevertTool], args
	}
	args["branch"] = fmt.Sprintf("seats/%s/c%06d", gitlettest.FixtureAlice, r.created.Add(1))
	args["at"] = br.head
	return r.calls[builtin.CodeCreateBranchTool], args
}

// round asks every worker's write at once, waits for all, then compacts.
func (r *writeRig) round(b *testing.B, i int) {
	var wg sync.WaitGroup
	for k := range r.arm.concurrency {
		br := r.branches[[2]int{k, (k + i*r.arm.concurrency) % r.arm.repos}]
		wg.Go(func() { r.one(b.Context(), br, i) })
	}
	wg.Wait()
	if r.failed != nil {
		b.Fatal(r.failed)
	}
	r.compact(b)
}

// one is one write, timed from its tool's call to its answer, a refusal as
// much as an applied write.
func (r *writeRig) one(ctx context.Context, br *writeBranchState, i int) {
	tool, args := r.callOf(br, i)
	call := &writeCall{}
	ctx = context.WithValue(toolloop.WithCallModel(ctx, benchModel), writeCallKey{}, call)
	start := time.Now()
	out, err := tool.CallForTurn(ctx, benchTurn(r.seat), args)
	end := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	res := call.res
	switch {
	case err != nil:
		r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s: %w", tool.Name(), br.ref, err))
	case !call.asked:
		r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s answered without asking Gitlet: %s", tool.Name(),
			br.ref, out.Output))
	case unready(call.err):
		r.unready++
		//fixedwait:measure a benchmark reports this against a quarter of estate.GitWriteAttempt; no gate reads it
		r.answers, r.refusedTook = append(r.answers, end.Sub(start)), append(r.refusedTook, end.Sub(start))
	case call.err != nil:
		r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s: %w", tool.Name(), br.ref, call.err))
	case res.Command.Reason == gitlet.RefusalBusy:
		r.busy++
		//fixedwait:measure a benchmark reports this against a quarter of estate.GitWriteAttempt; no gate reads it
		r.answers = append(r.answers, end.Sub(start))
	case res.Outcome != statelog.OutcomeApplied || res.Refusal != "" || res.Commit == "":
		r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s answered %+v", tool.Name(), br.ref, res))
	default:
		if answered := toolAnswered(out); out.Failed || answered.Outcome != statelog.OutcomeApplied ||
			answered.Commit != res.Commit {
			r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s applied commit %s and its tool answered %q", tool.Name(),
				br.ref, res.Commit, out.Output))
			return
		}
		//fixedwait:measure a benchmark reports this against a quarter of estate.GitWriteAttempt; no gate reads it
		r.took, r.answers = append(r.took, end.Sub(start)), append(r.answers, end.Sub(start))
		//fixedwait:measure a benchmark reports this; no gate reads it
		r.tool = append(r.tool, call.askedAt.Sub(start)+end.Sub(call.answerAt))
		r.ended[res.PushID] = call.answerAt
		if r.arm.kind != writeBranch {
			br.head = res.Commit
		}
	}
}

// toolAnswer is the fields a turn reads off a write tool's answer.
type toolAnswer struct {
	Outcome statelog.Outcome `json:"outcome"`
	Commit  string           `json:"commit"`
}

// toolAnswered is the tool's answer read back, zero where it is no answer.
func toolAnswered(out tools.Result) toolAnswer {
	var a toolAnswer
	_ = json.Unmarshal([]byte(out.Output), &a)
	return a
}

// unready reports a write no Gitlet node could run now: gitlet.ErrGitUnready
// (the share full past gitlet.GitShareWait, the copy still syncing), or the
// walk's estate.ErrUnserved saying so, which names the node, never the write.
// A node that went silent is no such answer and fails the arm.
func unready(err error) bool {
	var (
		node *gitlet.ErrGitUnready
		walk *estate.ErrUnserved
	)
	return errors.As(err, &node) || (errors.As(err, &walk) && walk.Git && !walk.Silent())
}

// cmpOr is the first of a and c that is set.
func cmpOr(a, c error) error {
	if a != nil {
		return a
	}
	return c
}

// run is the arm: its rounds timed, every figure reported, and its
// assertions held.
func (r *writeRig) run(b *testing.B) {
	var person *besidePusher
	var alone []time.Duration
	if r.arm.beside {
		person = newBesidePusher(b, r.f, r.node, &r.quiet)
		r.fig.enter(phaseAlone)
		b.Logf("alone: the repository holds %d live packs", r.livePacks(b))
		var err error
		if alone, err = person.paced(b.Context(), besideAlone); err != nil {
			b.Fatal(err)
		}
	}
	r.fig.enter(phaseWrites)
	peak := watchInFlight(b, r.node.Host())
	pushing, stopPushing := context.WithCancel(b.Context())
	besides := make(chan besideResult, 1)
	if person != nil {
		go func() {
			took, err := person.paced(pushing, 0)
			besides <- besideResult{took, err}
		}()
	}
	start := time.Now()
	rounds := 0
	for b.Loop() {
		r.round(b, rounds)
		rounds++
	}
	elapsed := time.Since(start) - r.compacting //fixedwait:measure a benchmark reports this; no gate reads it
	stopPushing()
	inFlight := peak()

	r.mu.Lock()
	took, tool, applied := inOrder(r.took), inOrder(r.tool), len(r.took)
	answers, refused := inOrder(r.answers), inOrder(r.refusedTook)
	asked := rounds * r.arm.concurrency
	var adopt []time.Duration
	r.fig.mu.Lock()
	for id, end := range r.ended {
		if at, ok := r.fig.answered[id]; ok {
			adopt = append(adopt, end.Sub(at)) //fixedwait:measure a benchmark reports this; no gate reads it
		}
	}
	proposals, pending, packs := inOrder(r.fig.proposals), r.fig.pending, len(r.fig.packs)
	r.fig.mu.Unlock()
	unready, busy := r.unready, r.busy
	r.mu.Unlock()
	if applied == 0 {
		b.Fatalf("none of %d writes applied: %d unready, %d busy", asked, unready, busy)
	}
	adopt = inOrder(adopt)
	uploads, puts := r.fig.uploads()
	shares := r.fig.shares()
	b.ReportMetric(float64(applied)/elapsed.Seconds(), "writes/s")
	b.ReportMetric(quantile(answers, 0.50), "answer-p50-ms")
	b.ReportMetric(quantile(answers, 0.99), "answer-p99-ms")
	if len(refused) > 0 {
		b.ReportMetric(quantile(refused, 0.99), "unready-p99-ms")
	}
	b.ReportMetric(quantile(took, 0.50), "p50-ms")
	b.ReportMetric(quantile(took, 0.99), "p99-ms")
	b.ReportMetric(quantile(tool, 0.50), "tool-p50-ms")
	b.ReportMetric(quantile(tool, 0.99), "tool-p99-ms")
	for _, stage := range []string{"sync", "build", "land", "total"} {
		if obs := r.fig.stage(stage); len(obs) > 0 {
			b.ReportMetric(quantile(obs, 0.50), stage+"-p50-ms")
			b.ReportMetric(quantile(obs, 0.99), stage+"-p99-ms")
		}
	}
	// Logged as well, since a benchmark that fails prints no metric.
	b.Logf("%d writes asked, %d applied, %d unready, %d busy: every answer p50/p99 %.0f/%.0f ms, the applied "+
		"%.0f/%.0f ms, the unready p99 %.0f ms", asked, applied, unready, busy, quantile(answers, 0.50),
		quantile(answers, 0.99), quantile(took, 0.50), quantile(took, 0.99), quantile(refused, 0.99))
	if len(shares) > 0 {
		b.ReportMetric(quantile(shares, 0.50), "share-wait-p50-ms")
		b.ReportMetric(quantile(shares, 0.99), "share-wait-p99-ms")
		// Logged beside its budget (M3-decisions M3-O43): the one term of
		// estate.GitWriteAttempt a write can reach whole.
		b.Logf("the share's wait for a build: p50/p99 %.0f/%.0f ms of gitlet.GitShareWait's %s, over %d takes",
			quantile(shares, 0.50), quantile(shares, 0.99), gitlet.GitShareWait, len(shares))
	}
	if len(uploads) > 0 {
		b.ReportMetric(quantile(uploads, 0.50), "upload-p50-ms")
		b.ReportMetric(quantile(uploads, 0.99), "upload-p99-ms")
	}
	b.ReportMetric(quantile(proposals, 0.50), "propose-p50-ms")
	b.ReportMetric(quantile(proposals, 0.99), "propose-p99-ms")
	b.ReportMetric(quantile(adopt, 0.50), "adopt-p50-ms")
	b.ReportMetric(quantile(adopt, 0.99), "adopt-p99-ms")
	b.ReportMetric(float64(puts)/float64(applied), "puts/write")
	b.ReportMetric(float64(len(r.fig.requests(phaseWrites, "writes")["GET"]))/float64(applied), "gets/write")
	b.ReportMetric(float64(len(r.fig.requests(phaseWrites, "node")["GET"]))/float64(applied), "node-gets/write")
	b.ReportMetric(float64(unready)/float64(asked), "unready/write")
	b.ReportMetric(float64(busy)/float64(asked), "busy/write")
	b.ReportMetric(float64(pending)/float64(asked), "pending/write")
	b.ReportMetric(float64(inFlight), "inflight-peak")
	r.fig.logPhase(b, phaseWrites)
	r.fig.logPhase(b, phaseCompact)
	if r.arm.kind != writeBranch && packs != len(proposals) {
		b.Errorf("%d of %d proposals named a pack: every %s's push carries one", packs, len(proposals), r.arm.kind)
	}
	if served := r.node.Share().Units() * writesAUnitServes; r.arm.concurrency <= served && unready+busy > 0 {
		b.Errorf("%d of %d writes asked %d at once were answered unready and %d busy, where the share's %d "+
			"units serve %d at once inside gitlet.GitShareWait", unready+busy, asked, r.arm.concurrency, busy,
			r.node.Share().Units(), served)
	}

	// Budgets are logged beside the p99 each bounds (M3-decisions M3-O43),
	// never failed on: a benchmark measures, a test decides.
	if r.arm.store.firstByte == 200*time.Millisecond {
		b.Logf("at a 200 ms first byte: the applied writes' p99 %.0f ms and every answer's %.0f ms, against a "+
			"quarter of estate.GitWriteAttempt's %s; the upload's p99 %.0f ms, against a tenth of "+
			"gitlet.WriteUploadAllowance's %s", quantile(took, 0.99), quantile(answers, 0.99), estate.GitWriteAttempt,
			quantile(uploads, 0.99), gitlet.WriteUploadAllowance)
	}
	if person == nil {
		return
	}
	got := <-besides
	if got.err != nil {
		b.Fatal(got.err)
	}
	// Alone again after the writes, at the same pack state: a push slower
	// here than before them was slowed by what the writes left on the node,
	// which this phase's logged requests and syncs name.
	r.fig.enter(phaseAfter)
	b.Logf("after: the repository holds %d live packs", r.livePacks(b))
	after, err := person.paced(b.Context(), besideAlone)
	if err != nil {
		b.Fatal(err)
	}
	for _, phase := range []string{phaseAlone, phaseAfter} {
		r.fig.logPhase(b, phase)
	}
	beside, after := inOrder(got.took), inOrder(after)
	alone = inOrder(alone)
	b.ReportMetric(quantile(alone, 0.50), "person-alone-p50-ms")
	b.ReportMetric(quantile(alone, 0.99), "person-alone-p99-ms")
	b.ReportMetric(quantile(beside, 0.50), "person-p50-ms")
	b.ReportMetric(quantile(beside, 0.99), "person-p99-ms")
	b.ReportMetric(quantile(after, 0.50), "person-after-p50-ms")
	b.ReportMetric(quantile(after, 0.99), "person-after-p99-ms")
	// Logged as well, since a benchmark that fails prints no metric.
	b.Logf("the person's push p50/p99: %.0f/%.0f ms alone, %.0f/%.0f ms beside the writes, %.0f/%.0f ms after them",
		quantile(alone, 0.50), quantile(alone, 0.99), quantile(beside, 0.50), quantile(beside, 0.99),
		quantile(after, 0.50), quantile(after, 0.99))
	if r.arm.concurrency == writesAtOnce {
		b.Logf("the person's push p99 beside %d writes at once: %.0f ms, against twice its %.0f ms alone",
			writesAtOnce, quantile(beside, 0.99), quantile(alone, 0.99))
	}
}

// watchInFlight samples s's writes in flight every millisecond until the
// returned function is called, which stops it and returns the peak.
func watchInFlight(b *testing.B, s *host.Server) func() int {
	ctx, stop := context.WithCancel(b.Context())
	var peak int
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Millisecond) //fixedwait:bench
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				peak = max(peak, host.WritesInFlight(s))
			}
		}
	}()
	return func() int {
		stop()
		<-done
		return peak
	}
}

// besideResult is a beside=push arm's pushes while its writes ran.
type besideResult struct {
	took []time.Duration
	err  error
}

// besidePusher is a person pushing one commit at a time to the arm's first
// repository's main, each push held off while a compaction holds quiet.
type besidePusher struct {
	p     *gitlettest.Person
	work  string
	url   string
	n     int
	quiet *sync.RWMutex
}

// newBesidePusher is ceo, with RepoName's main seeded from a work tree of
// theirs.
func newBesidePusher(b *testing.B, f *gitlettest.SoloFleet, node gitlettest.Node, quiet *sync.RWMutex) *besidePusher {
	b.Helper()
	p := gitlettest.NewPerson(b, f, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "seed")
	mustPush(b, p, work, repoURL(node), "main")
	return &besidePusher{p: p, work: work, url: repoURL(node), quiet: quiet}
}

// push commits one 4 KiB file, untimed, and returns how long the person's
// git took to push it, never beside a compaction.
func (bp *besidePusher) push(ctx context.Context) (time.Duration, error) {
	bp.quiet.RLock()
	defer bp.quiet.RUnlock()
	bp.n++
	file := filepath.Join(bp.work, "person", fmt.Sprintf("f%06d.bin", bp.n))
	if err := os.MkdirAll(filepath.Dir(file), hostbox.DirMode); err != nil {
		return 0, err
	}
	if err := os.WriteFile(file, []byte(randomBytes(4<<10)), hostbox.FileMode); err != nil {
		return 0, err
	}
	git := bp.p.Git()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", fmt.Sprintf("push %d", bp.n)}} {
		if _, stderr, err := git.TryContext(ctx, bp.work, nil, args...); err != nil {
			return 0, fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr)
		}
	}
	start := time.Now()
	res := bp.p.Push(ctx, bp.work, bp.url, "main")
	if res.Err != nil {
		return 0, fmt.Errorf("git push: %w\n%s", res.Err, res.Stderr)
	}
	return time.Since(start), nil //fixedwait:measure a benchmark reports this against twice the push alone; no gate reads it
}

// paced is n pushes (until ctx ends where n is 0), one every besidePace, each
// starting at once where the one before took longer.
func (bp *besidePusher) paced(ctx context.Context, n int) ([]time.Duration, error) {
	var took []time.Duration
	for i := 0; n == 0 || i < n; i++ {
		start := time.Now()
		d, err := bp.push(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return took, nil //nolint:nilerr // a push ctx's end cut is the pacing's end, never its failure
		case err != nil:
			return took, err
		}
		took = append(took, d)
		select {
		case <-ctx.Done():
			return took, nil
		//fixedwait:simulated a person pushing once every besidePace
		case <-time.After(time.Until(start.Add(besidePace))): //fixedwait:measure the remaining time only paces the person's pushes
		}
	}
	return took, nil
}

// BenchmarkEngineCommit times an engine commit's build alone (ADR-0092;
// build.go), the build stage of crewlet.code.host.write.duration, which holds the
// write's share unit: each a commit_files by alice creating a branch at one
// commit, asked of the host directly, so every build does the same work. Each
// copy is indexed whole (warmIndex) and its maintainer then held off
// (holdMaintenance), so no index run is timed into a build; that cost is
// internal/gitlet/cache's to measure. tree= lays files along p1/…/p6 (commitTree)
// so a touched directory holds the same entries in either tree
// (TestAnEngineCommitReadsNoWholeTree); flat= times the one cost that follows a
// directory's entries. Held: the two trees' p50 within a tenth where both ran, a
// hundred files under a tenth of gitlet.WriteBuildDeadline, MaxFileOps inside all
// of it, and the 100 000-entry directory's pack under gitlet.MaxWritePackBytes.
func BenchmarkEngineCommit(b *testing.B) {
	gitlettest.Quiet(b)
	// Both trees' fixtures first, then each arm in one tree and the other back
	// to back: run tree by tree, the comparison read the machine's moment on a
	// shared slot, tens of percent apart, never the tree.
	trees := map[int]*commitRig{}
	for _, files := range []int{commitTreeFiles, commitTreeFilesLarge} {
		if files == commitTreeFilesLarge && os.Getenv(gittest.LargeEnv) != "1" {
			continue
		}
		trees[files] = newCommitRig(b, func(w *bufio.Writer) error { return commitTree(w, files) })
	}
	for _, rig := range trees {
		rig.warm(b)
	}
	for _, arm := range commitArms() {
		b.Run(arm.String(), func(b *testing.B) {
			p50 := map[int]float64{}
			for _, files := range []int{commitTreeFiles, commitTreeFilesLarge} {
				b.Run(fmt.Sprintf("tree=%d", files), func(b *testing.B) {
					rig := trees[files]
					if rig == nil {
						gittest.Large(b, "a tree of 100 000 files pushed through the host")
					}
					build, _ := rig.builds(b, arm.files, commitPath(arm.depth))
					p50[files] = quantile(build, 0.50)
					if files != commitTreeFilesLarge || arm.depth != commitDepth {
						return
					}
					p99 := quantile(build, 0.99)
					switch {
					case arm.files == 100 && p99 >= float64(gitlet.WriteBuildDeadline/10)/float64(time.Millisecond):
						b.Errorf("a hundred files at depth %d in a tree of %d build in %.0f ms at the p99, past a "+
							"tenth of gitlet.WriteBuildDeadline (%s)", commitDepth, files, p99, gitlet.WriteBuildDeadline)
					case arm.files == gitlet.MaxFileOps &&
						p99 >= float64(gitlet.WriteBuildDeadline)/float64(time.Millisecond):
						b.Errorf("gitlet.MaxFileOps files at depth %d in a tree of %d build in %.0f ms at the p99, "+
							"past gitlet.WriteBuildDeadline (%s)", commitDepth, files, p99, gitlet.WriteBuildDeadline)
					}
				})
			}
			// THE COST FOLLOWS THE CHANGE, NEVER THE TREE: the two trees'
			// p50 within a tenth of each other, held where both ran.
			small, large := p50[commitTreeFiles], p50[commitTreeFilesLarge]
			if small == 0 || large == 0 {
				return
			}
			if lo, hi := min(small, large), max(small, large); hi > lo*1.1 {
				b.Errorf("%s builds in %.1f ms at the p50 in a tree of %d files and %.1f ms in one of %d: not "+
					"within a tenth, so the build's cost follows the tree", arm, small, commitTreeFiles, large,
					commitTreeFilesLarge)
			}
		})
	}
	for _, entries := range []int{commitTreeFiles, commitTreeFilesLarge} {
		b.Run(fmt.Sprintf("flat=%d", entries), func(b *testing.B) {
			if entries == commitTreeFilesLarge {
				gittest.Large(b, "a directory of 100 000 entries pushed through the host")
			}
			rig := newCommitRig(b, func(w *bufio.Writer) error {
				for i := range entries {
					if err := inlineFile(w, fmt.Sprintf("%s/f%06d.txt", commitFlatDir, i)); err != nil {
						return err
					}
				}
				return nil
			})
			_, pack := rig.builds(b, 1, commitFlatDir)
			if entries == commitTreeFilesLarge && pack >= gitlet.MaxWritePackBytes {
				b.Errorf("a file put into a directory of %d entries packs %d bytes, past gitlet.MaxWritePackBytes "+
					"(%d)", entries, pack, gitlet.MaxWritePackBytes)
			}
		})
	}
}

const (
	// commitTreeFiles and commitTreeFilesLarge are BenchmarkEngineCommit's
	// two trees, and its two flat directories' entries.
	commitTreeFiles      = 10_000
	commitTreeFilesLarge = 100_000

	// commitDepth is how deep the path every arm's files are put along goes.
	commitDepth = 6

	// commitSiblingDirs and commitSiblingFiles are what every directory of
	// that path holds beside the next: what a build touching it reads.
	commitSiblingDirs  = 10
	commitSiblingFiles = 10

	// commitFlatDir is the flat= arms' directory.
	commitFlatDir = "flat"
)

// commitArm is one files= and depth= arm.
type commitArm struct{ files, depth int }

func (a commitArm) String() string { return fmt.Sprintf("files=%d/depth=%d", a.files, a.depth) }

// commitArms are 1, 10 and 100 files at depths 1, 3 and 6, and gitlet.MaxFileOps
// files at depth 6.
func commitArms() []commitArm {
	var arms []commitArm
	for _, files := range []int{1, 10, 100} {
		for _, depth := range []int{1, 3, commitDepth} {
			arms = append(arms, commitArm{files, depth})
		}
	}
	return append(arms, commitArm{gitlet.MaxFileOps, commitDepth})
}

// commitPath is the directory of the path at depth: p1/…/p<depth>, "" at the
// root.
func commitPath(depth int) string {
	parts := make([]string, depth)
	for i := range parts {
		parts[i] = fmt.Sprintf("p%d", i+1)
	}
	return path.Join(parts...)
}

// inlineFile writes one fast-import file line holding its own path, so no two
// files share a blob, as in a real repository, and a touched directory's tree
// compresses as a real one does: not at all.
func inlineFile(w *bufio.Writer, path string) error {
	content := path + "\n"
	_, err := fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n%s\n", path, len(content), content)
	return err
}

// commitTree writes about files files as fast-import lines, each its own
// blob: every directory along p1/…/p6, the root's too, holds the next,
// commitSiblingFiles files and commitSiblingDirs directories, which share the
// rest of the files.
func commitTree(w *bufio.Writer, files int) error {
	levels := commitDepth + 1
	perDir := (files - levels*commitSiblingFiles) / (levels * commitSiblingDirs)
	for level := range levels {
		dir := commitPath(level)
		if dir != "" {
			dir += "/"
		}
		for s := range commitSiblingDirs {
			for i := range perDir {
				if err := inlineFile(w, fmt.Sprintf("%ss%02d/f%06d.txt", dir, s, i)); err != nil {
					return err
				}
			}
		}
		for i := range commitSiblingFiles {
			if err := inlineFile(w, fmt.Sprintf("%sf%02d.txt", dir, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// commitRig is a BenchmarkEngineCommit fixture: one Gitlet node holding a
// repository of one root commit pushed by ceo.
type commitRig struct {
	f    *gitlettest.SoloFleet
	node gitlettest.Node
	fig  *writeFigures
	repo string
	base string
	pos  statelog.Position
	who  gitlet.Principal
	// n counts the rig's writes, which name their branches and files apart.
	n int
}

// newCommitRig builds the fixture: tree writes the root commit's file lines,
// each with its blob inline. Its copy is indexed whole before the arms, and
// its node's maintainer held off for them.
func newCommitRig(b *testing.B, tree func(*bufio.Writer) error) *commitRig {
	b.Helper()
	r := &commitRig{fig: newWriteFigures(), who: benchSeat()}
	r.f = writeFleet(b, memobj.New(), r.fig)
	r.node = r.f.Nodes()[0]
	r.f.Declare(b, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
	p := gitlettest.NewPerson(b, r.f, "ceo")
	work := p.Init()
	gittest.FastImport(b, p.Git(), work, func(w *bufio.Writer) error {
		if _, err := fmt.Fprintf(w, "commit refs/heads/main\nmark :1\n"+
			"committer A <a@example.com> %d +0000\ndata 5\ntree\n", time.Now().Unix()); err != nil {
			return err
		}
		if err := tree(w); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, "\n")
		return err
	})
	r.base = strings.TrimSpace(p.Git().Run(b, work, "rev-parse", "refs/heads/main"))
	mustPush(b, p, work, repoURL(r.node), "main")
	rows, err := r.f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	r.repo, r.pos = rows.Repo.ID, rows.Position
	warmIndex(b, r.node, r.repo, 0)
	holdMaintenance(b, r.node)
	return r
}

// holdMaintenance holds every slot of node's maintenance lane until b ends,
// each taken as it comes free so a run in hand ends first.
func holdMaintenance(b *testing.B, node gitlettest.Node) {
	b.Helper()
	slots := node.Pool().Slots(gitproc.ClassMaintenance)
	deadline := time.Now().Add(5 * time.Minute)
	var held []*gitproc.Reserved
	b.Cleanup(func() {
		for _, r := range held {
			r.Release()
		}
	})
	for len(held) < slots {
		r, err := node.Pool().Reserve()
		switch {
		case err == nil:
			held = append(held, r)
		case !errors.Is(err, gitproc.ErrBusy) || time.Now().After(deadline): //fixedwait:measure a poll's own deadline, whose firing only fails the benchmark
			b.Fatalf("hold node %s's maintenance lane, %d of its %d slots held: %v", node.ID(), len(held), slots,
				err)
		default:
			time.Sleep(100 * time.Millisecond) //fixedwait:liveness
		}
	}
}

// commitWarmBuilds is how many untimed builds a rig makes first: the first
// builds after a fixture meet what its import and push left (page cache, Go
// heap, the copy's first pack reads), which no later build does.
const commitWarmBuilds = 5

// warm makes the rig's commitWarmBuilds untimed builds.
func (r *commitRig) warm(b *testing.B) {
	b.Helper()
	for range commitWarmBuilds {
		r.build(b, 1, commitPath(commitDepth))
	}
}

// build is the rig's next write: files files put into dir by a write
// creating a branch at the rig's commit, asked of the Gitlet node's host.
func (r *commitRig) build(b *testing.B, files int, dir string) {
	b.Helper()
	r.n++
	w := gitlet.EngineWrite{Kind: gitlet.WriteCommitFiles, RepoID: r.repo, Principal: r.who,
		WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: statelog.FloorAt(statelog.ReadSession, r.pos),
		Base: r.base, Message: fmt.Sprintf("build %d", r.n),
		Ref: fmt.Sprintf("refs/heads/seats/%s/b%06d", gitlettest.FixtureAlice, r.n)}
	for j := range files {
		w.Files = append(w.Files, text(path.Join(dir, fmt.Sprintf("new%06d-%04d.txt", r.n, j)),
			fmt.Sprintf("build %d file %d\n", r.n, j)))
	}
	res, err := r.node.Host().Write(b.Context(), w)
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Refusal != "" {
		b.Fatalf("the build of %d files into %q: %+v %v", files, dir, res, err)
	}
}

// builds is one arm: a build of files files into dir per iteration. It
// reports and answers every build's time, sorted, and the largest pack.
func (r *commitRig) builds(b *testing.B, files int, dir string) ([]time.Duration, int64) {
	b.Helper()
	r.fig.reset()
	for b.Loop() {
		r.build(b, files, dir)
	}
	build := r.fig.stage("build")
	r.fig.mu.Lock()
	pack := int64(0)
	for _, n := range r.fig.packs {
		pack = max(pack, n)
	}
	r.fig.mu.Unlock()
	b.ReportMetric(quantile(build, 0.50), "build-p50-ms")
	b.ReportMetric(quantile(build, 0.99), "build-p99-ms")
	b.ReportMetric(float64(pack), "pack-bytes")
	return build, pack
}
