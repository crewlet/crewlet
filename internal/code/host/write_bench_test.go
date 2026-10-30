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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
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

// AN ENGINE WRITE, MEASURED (ADR-0062): from the call of the code write tool a
// seat's turn makes to the pack adopted into the copy (BenchmarkEngineWrite),
// and its build alone (BenchmarkEngineCommit). What each arm reports confirms
// or replaces the budgets an engine write is held to — every term of
// estate.GitWriteAttempt, code.WriteUploadAllowance, code.WriteBuildDeadline,
// code.MaxFileOps, code.MaxWritePackBytes, the git writes' lane's width and the
// seat git-work share's half — and doc.go's measured section and
// code-host.md's "What an engine write costs" carry the figures, with the
// machine, the git and the object store each was taken on.

// writeStore is an object store an engine write's arms run over: the
// in-memory twin, or the S3 fake behind the production backend answering each
// request firstByte after it arrives.
type writeStore struct {
	name      string
	firstByte time.Duration
}

// writeStores are BenchmarkEngineWrite's: the twin, and the S3 fake at the two
// ends of the 100–200 ms AWS documents for S3's first byte (M2-decisions
// O15.7), at benchConnBytes a connection — every figure an in-process
// store's, none a real bucket's.
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

// writeFigures are what an arm's git node did for its writes, as it did them —
// read from the git node's own seams, never from a fake: the host's per-stage
// durations as it observes them (crewlet.code.host.write.duration, every
// observation handed to a bound sink), each PUT at the object store the node
// writes through (its timedObjects), and each engine write's proposal at the
// estate the node proposes through (its timedEstate). A NEW SET EVERY ARM,
// reset once its fixture is built, so a seeded branch is no sample.
type writeFigures struct {
	mu sync.Mutex
	// stages is every observation of crewlet.code.host.write.duration, by
	// its stage: sync, build, land and total.
	stages map[string][]time.Duration
	// puts is how long each object's last PUT took and putCalls how many
	// PUTs it was given, by the object's name; engine is the names an
	// engine write's proposal named — a person's push in a beside=push arm
	// stores bundles too, which are not the writes'.
	puts     map[string]time.Duration
	putCalls map[string]int
	engine   map[string]bool
	// proposals is how long each engine write's proposal was in flight: the
	// committer's batch, its record and the wait for its row, which the
	// committer answers once its row applied (or pending, past its own
	// wait, which pending counts); answered is when, by push id.
	proposals []time.Duration
	answered  map[string]time.Time
	pending   int
	// packs is each engine write's pack bytes, as its proposal names them.
	packs []int64
	// getN counts the git node's GETs.
	getN atomic.Int64
}

// newWriteFigures is an empty set.
func newWriteFigures() *writeFigures {
	fig := &writeFigures{}
	fig.reset()
	return fig
}

// reset forgets everything so far: an arm's fixture is built before it.
func (fig *writeFigures) reset() {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	fig.stages, fig.engine = map[string][]time.Duration{}, map[string]bool{}
	fig.puts, fig.putCalls = map[string]time.Duration{}, map[string]int{}
	fig.proposals, fig.answered, fig.pending, fig.packs = nil, map[string]time.Time{}, 0, nil
	fig.getN.Store(0)
}

// observe is the sink bound to the host's write durations: called under the
// recorder's own lock, so it keeps nothing of attrs but the stage.
func (fig *writeFigures) observe(ms float64, attrs map[string]string) {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	stage := attrs["stage"]
	fig.stages[stage] = append(fig.stages[stage], time.Duration(ms*float64(time.Millisecond)))
}

// stage is every observation of stage, sorted.
func (fig *writeFigures) stage(stage string) []time.Duration {
	fig.mu.Lock()
	defer fig.mu.Unlock()
	return inOrder(fig.stages[stage])
}

// uploads is every engine write's PUT duration, sorted, and how many PUTs the
// engine writes' objects were given in all.
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

// timedObjects is the git node's object store with every PUT timed and
// counted by the object's name and every GET counted.
type timedObjects struct {
	objstore.Backend
	fig *writeFigures
}

func (o timedObjects) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	start := time.Now()
	err := o.Backend.Put(ctx, name, r, m)
	took := time.Since(start)
	o.fig.mu.Lock()
	defer o.fig.mu.Unlock()
	o.fig.putCalls[name]++
	if err == nil {
		o.fig.puts[name] = took
	}
	return err
}

func (o timedObjects) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	o.fig.getN.Add(1)
	return o.Backend.Get(ctx, name, off, n)
}

// timedEstate is the git node's estate with every engine write's proposal
// timed, and the pack it names kept.
type timedEstate struct {
	host.Estate
	fig *writeFigures
}

func (e timedEstate) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	start := time.Now()
	res, err := e.Estate.Propose(ctx, p)
	if p.Via != code.ViaEngine {
		return res, err
	}
	end := time.Now()
	e.fig.mu.Lock()
	defer e.fig.mu.Unlock()
	e.fig.proposals = append(e.fig.proposals, end.Sub(start))
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

// writeCall is what one tool call's write came to, as the tool's own call of
// the code host saw it (timedWriter): when it asked and was answered, and the
// answer — a write's push id, commit and floor are none of a seat's tool
// answer, which names its branch and commit.
type writeCall struct {
	asked             bool
	askedAt, answerAt time.Time
	res               code.EngineWriteResult
	err               error
}

// writeCallKey carries a tool call's writeCall in its context, to the
// timedWriter the tool's write reaches.
type writeCallKey struct{}

// timedWriter is the code host's writes as the code tools reach them — the
// fleet's routed seam (estate.CodeSeam), its lifecycle requests as they are —
// with each engine write recorded into the writeCall its tool call's context
// carries.
type timedWriter struct{ estate.CodeSeam }

func (w timedWriter) GitWrite(ctx context.Context, ew code.EngineWrite) (code.EngineWriteResult, error) {
	call, _ := ctx.Value(writeCallKey{}).(*writeCall)
	start := time.Now()
	res, err := w.CodeSeam.GitWrite(ctx, ew)
	if call != nil {
		call.asked, call.askedAt, call.answerAt, call.res, call.err = true, start, time.Now(), res, err
	}
	return res, err
}

// noRequesters composes nobody: every turn an arm makes was woken by no one,
// so no code call of it is decided for anybody but the seat — the wiring
// builtin.CodeDeps requires beside its code host, never asked here.
type noRequesters struct{}

func (noRequesters) Requester(_ context.Context, handle string) (iam.Principal, error) {
	return iam.Principal{}, fmt.Errorf("no requester is composed here, and %q was asked for", handle)
}

// writeTools are the code write tools an arm's writes are asked through, by
// name: the real ones (builtin.CodeTools) over the fleet's routed code host,
// every write timed (timedWriter), as a seat's turn on a node holding nothing
// of its own is handed them.
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

// writeFleet is a solo fleet of ONE git node over backend — its broker fsyncing
// every write as a deployment's does (SyncAlways), its git node's estate and
// object store timed into fig and its write durations handed to fig as the
// host observes them — whose seats may land as many pushes a day as the
// quota's ceiling admits, since an arm's writes are all one seat's.
func writeFleet(b *testing.B, backend objstore.Backend, fig *writeFigures) *codetest.SoloFleet {
	b.Helper()
	f := codetest.SoloNode(b, codetest.SoloOptions{
		Shape: codetest.Shape{
			Limits:     code.Limits{PushesPerSeatDay: config.CodePushesPerSeatDayCeiling},
			WrapEstate: func(e host.Estate) host.Estate { return timedEstate{Estate: e, fig: fig} },
		},
		Objects: backend, SyncAlways: true, CacheMaxBytes: benchCacheBytes,
		WrapObjects: func(_ string, o objstore.Backend) objstore.Backend { return timedObjects{Backend: o, fig: fig} },
	})
	node := f.Nodes()[0]
	if err := node.Metrics().BindHistogram(metrics.CodeHostWriteDuration, fig.observe); err != nil {
		b.Fatal(err)
	}
	b.Logf("the pool runs git %s; its read lane is %d slots, the seats' share %d units",
		node.Pool().Version(), node.Pool().Slots(gitproc.ClassRead), node.Share().Units())
	return f
}

// The turn a seat's writes are made in: alice — the fixture chart's
// full_access seat — on a work item of the tracker's usual key, on a model id
// of a provider's usual length.
const (
	benchModel   = "fixture-model-2031"
	benchWorkKey = "ENG-1234"
)

// benchSeat is alice as a write's principal names her, in a turn of its own:
// what a seed's write, which no tool makes, is asked as.
func benchSeat() code.Principal {
	return code.Principal{Kind: code.PrincipalAgent, Seat: codetest.FixtureAlice, Run: code.RunProvenance{
		TurnID: statelog.NewOpID(time.Now(), ""), WorkBackend: string(types.WorkNative), WorkKey: benchWorkKey,
		Model: benchModel}}
}

// benchTurn is a turn of alice's own, as the engine dispatches one: its run id
// minted as the engine mints it, charged to the work item, with the memo of
// what it looked up by name and the holds it took empty — so the write it
// makes is the first call of its turn, which resolves the repository's name
// at the log's head as every turn's first code call does.
func benchTurn(seat *org.Role) *turnctx.Turn {
	return &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat, Lookups: turnctx.NewLookups(),
		Holds: turnctx.NewHolds(nil), WorkItem: &types.WorkItem{Backend: types.WorkNative,
			ID: "0199f0a2-7b1c-7cde-8f00-000000001234", Key: benchWorkKey, Project: codetest.FixtureUnit}}
}

// The kinds an arm writes.
const (
	writeFiles1  = "commit_files-1"
	writeFiles10 = "commit_files-10"
	writeRevert  = "revert"
	writeBranch  = "create_branch"
)

// writeArm is one BenchmarkEngineWrite arm: its object store, the kind of
// write, how many are asked at once and over how many repositories, and
// whether a person pushes to the first of them beside the writes.
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

// writeArms are one object store's arms: every kind alone; one file's commit
// eight and sixty-four at once, over one repository and over sixty-four; and
// one at once and sixty-four at once beside a person's pushes.
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
	// writesAtOnce is the widest arm: as many writes at once as the git
	// node's write lane answers (queue.ConcurrentAnswers), each of a turn
	// of its own — a node's whole complement of turns and a second node's,
	// each inside one write.
	writesAtOnce = 64

	// besidePace is how often the person of a beside=push arm pushes: a
	// push of one commit every 100 ms, which a busy repository's people and
	// their CI make together.
	besidePace = 100 * time.Millisecond

	// besideAlone is how many of the person's pushes are timed with nothing
	// beside them, before the arm's writes begin: the figure the pushes
	// beside the writes are held to.
	besideAlone = 20

	// seedAtOnce is how many of an arm's branches are seeded at once: few
	// enough that a one-unit share serves each inside its wait
	// (code.GitShareWait) at any build this package measures, and seedTries
	// how many times a seed is asked before a fixture gives up on a node
	// that answers it unready every time.
	seedAtOnce = 4
	seedTries  = 10
)

// BenchmarkEngineWrite is AN ENGINE WRITE, FROM ITS TOOL'S CALL TO ITS PACK
// ADOPTED (ADR-0062): each write the call of a code write tool a seat's turn
// makes — the real one (builtin.CodeTools: code_commit_files, code_revert,
// code_create_branch, called as the seat's registry calls it past its
// authority gate, an in-memory decision), on a node holding nothing of its
// own, over the production estate.Router, whose walk asks the repository's
// home git node over the broker as a seat's turn on any other node does — and
// timed to the tool's answer, which follows the git node's, given only once
// the push's row applied and its pack is linked into the copy
// (codecluster's BenchmarkEngineWrite times the same write asked in process
// at the home, and through a member that is not). Every write is one seat's —
// alice, the first call of a turn of its own, so every one resolves the
// repository's name at the log's head as a turn's first code call does — onto
// a branch of her namespace that that worker's writes alone move, floored by
// the tool as a seat's write is: at this node's own writes to the
// repository, or its turn's read of it, whichever is later.
//
//   - objects= the in-memory twin, and the S3 fake at a 100 ms and a 200 ms
//     first byte.
//   - kind= one commit of one file and of ten onto the branch's head; a
//     revert of the head; a branch created at a commit the repository holds,
//     which builds nothing and stores no pack.
//   - concurrency= how many writes are asked at once, each round's waited for
//     together, the next round asked at once again — over ONE repository,
//     every write a branch of its own there, and over sixty-four, each round
//     spreading its writes over the repositories in turn (repos=).
//   - beside= nothing, or a person's `git push` of one commit to the first
//     repository's main every besidePace, timed besideAlone times alone
//     before the writes begin and then through every round.
//
// Each arm reports, over the writes that applied: their p50 and p99 whole, as
// the turn waited; what the tool added around its write (tool: the caller,
// the arguments, the repository's resolution at the log's head and the
// answer); the host's own stages (sync, build, land, total; the build holding
// its unit of the seat git-work share, so its time includes the wait for one);
// the bundle's PUT (upload), the proposal and the wait for its row (propose),
// and what follows the answer of the proposal (adopt: the pack linked into
// the copy and the answer's way back); PUTs and GETs a write; the writes
// answered unready (unready/write) — at concurrency=64 the git node's share
// holds half its read lane, and a write that waits past code.GitShareWait for
// a unit is sent on, which with one git node is an answer the turn asks again;
// the most writes the git node held at once (inflight-peak), which at
// concurrency=64 is the write lane's whole width held with nothing queued for
// a slot; and, beside=push, the person's push p50 and p99 alone, beside the
// writes, and alone again after them (person-after: over the packs the
// writes left, which nothing in an arm compacts).
//
// HELD by its own assertions: at the S3 fake's 200 ms first byte every arm's
// whole write p99 is under a quarter of estate.GitWriteAttempt and its upload
// p99 under a tenth of code.WriteUploadAllowance; and beside sixty-four
// writes at once the person's push p99 is within twice its p99 alone — the
// share leaves the person's scans their half of the read lane. This package's
// doc.go carries the figures.
func BenchmarkEngineWrite(b *testing.B) {
	codetest.Quiet(b)
	for _, store := range writeStores {
		b.Run("objects="+store.name, func(b *testing.B) {
			for _, arm := range writeArms(store) {
				b.Run(arm.String(), func(b *testing.B) { newWriteRig(b, arm).run(b) })
			}
		})
	}
}

// writeBranchState is one branch an arm's writes move: worker's in the arm's
// repository repo — the worker its only writer — and its head as the last
// write left it.
type writeBranchState struct {
	worker, repo int
	ref, head    string
}

// writeRig is one BenchmarkEngineWrite arm's fixture and its figures.
type writeRig struct {
	arm  writeArm
	f    *codetest.SoloFleet
	node codetest.Node
	fig  *writeFigures
	// seam is the fleet's routed code host, seat the seat every write is
	// made as, and calls the write tools by name, over the seam.
	seam  estate.CodeSeam
	seat  *org.Role
	calls map[string]tools.SeatCallable
	// repos and names are the arm's repositories, the first RepoName.
	repos, names []string
	// branches is every branch the arm's writes move, by worker and
	// repository.
	branches map[[2]int]*writeBranchState
	// created counts the create_branch arm's branches, which name each
	// apart.
	created atomic.Int64

	mu sync.Mutex
	// packs is how many live packs each repository holds — two a seeded
	// branch, one an applied write that stored one — which nothing in an
	// arm compacts.
	packs []int
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

// newWriteRig builds arm's fleet, declares its repositories and seeds every
// branch its writes will move.
func newWriteRig(b *testing.B, arm writeArm) *writeRig {
	b.Helper()
	r := &writeRig{arm: arm, fig: newWriteFigures(), branches: map[[2]int]*writeBranchState{},
		ended: map[string]time.Time{}, packs: make([]int, arm.repos),
		seat: &org.Role{Name: codetest.FixtureAlice}}
	r.f = writeFleet(b, arm.store.backend(b), r.fig)
	r.node = r.f.Nodes()[0]
	r.seam = r.f.Code(b)
	r.calls = writeTools(b, r.seam)
	declared := make([]code.Declared, arm.repos)
	for i := range declared {
		name := codetest.RepoName
		if i > 0 {
			name = fmt.Sprintf("bench-%02d", i)
		}
		declared[i] = code.Declared{Name: name, Settings: codetest.Settings()}
	}
	r.f.Declare(b, declared...)
	for _, d := range declared {
		rows, err := r.f.Rows(b.Context(), d.Name, codetest.Linearizable)
		if err != nil {
			b.Fatalf("the rows of %s: %v", d.Name, err)
		}
		r.repos, r.names = append(r.repos, rows.Repo.ID), append(r.names, d.Name)
	}
	// EVERY BRANCH A ROUND WILL MOVE, seeded with two commits — a root, and
	// a change of its file, so a revert has a change to undo — before the
	// timer: worker k's write of round i is to repository (k + i×concurrency)
	// mod repos, so the branches are the pairs that formula reaches.
	var todo []*writeBranchState
	for k := range arm.concurrency {
		for i := range arm.repos {
			repo := (k + i*arm.concurrency) % arm.repos
			if _, ok := r.branches[[2]int{k, repo}]; ok {
				continue
			}
			br := &writeBranchState{worker: k, repo: repo,
				ref: fmt.Sprintf("refs/heads/seats/%s/w%02d", codetest.FixtureAlice, k)}
			r.branches[[2]int{k, repo}] = br
			todo = append(todo, br)
		}
	}
	r.seed(b, todo)
	return r
}

// seed lands two commits on every branch of todo, seedAtOnce at a time, each
// asked again while its git node answers it unready — asked of the fleet's
// routing directly, no tool making a fixture's writes.
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
				w := code.EngineWrite{Kind: code.WriteCommitFiles, RepoID: r.repos[br.repo],
					Principal: benchSeat(), OpID: opNow(),
					Fresh: statelog.FloorAt(statelog.ReadSession, r.seam.FloorOf(r.repos[br.repo])), Ref: br.ref,
					ExpectedHead: br.head, Message: fmt.Sprintf("seed %d", i),
					Files: []code.FileOp{text(r.path(br, 0), content)}}
				res, err := r.landSeed(b.Context(), w)
				if err != nil {
					errs <- fmt.Errorf("seeding %s of %s: %w", br.ref, r.names[br.repo], err)
					return
				}
				br.head = res.Commit
				r.mu.Lock()
				r.packs[br.repo]++
				r.mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		b.Fatal(err)
	}
	r.fig.reset()
}

// landSeed asks w until it applies — again, the same write, a share's wait
// later, while it is answered unready, at most seedTries times — and fails on
// any other answer.
func (r *writeRig) landSeed(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	for try := 1; ; try++ {
		res, err := r.f.Write(ctx, w)
		switch {
		case unready(err) && try < seedTries:
			time.Sleep(code.GitShareWait)
			continue
		case err != nil:
			return res, err
		case res.Outcome != statelog.OutcomeApplied || res.Refusal != "" || res.Commit == "":
			return res, fmt.Errorf("answered %+v", res)
		}
		return res, nil
	}
}

// path is the j-th file a branch's writes put: in a directory of the
// worker's own.
func (r *writeRig) path(br *writeBranchState, j int) string {
	return fmt.Sprintf("w%02d/f%d.txt", br.worker, j)
}

// callOf is worker br.worker's write of round i as its tool is called: the
// tool of the arm's kind and the arguments a seat's turn gives it — the
// branch's head as the worker's last write left it, every revision a whole
// id, so the tool resolves the repository's name and nothing else.
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
	args["branch"] = fmt.Sprintf("seats/%s/c%06d", codetest.FixtureAlice, r.created.Add(1))
	args["at"] = br.head
	return r.calls[builtin.CodeCreateBranchTool], args
}

// round is one round of the arm: every worker's write asked at once and the
// round waited for whole.
//
// NO ROUND TAKES A REPOSITORY PAST code.MaxPacksPerRepo: every write but a
// create_branch stores a pack and nothing in an arm compacts, so a round that
// could is refused the arm, naming the bound — at sixty-four writes a round
// into one repository that is thirty rounds past the seeds — since its writes
// would otherwise be refused compaction_behind and the arm would measure
// refusals.
func (r *writeRig) round(b *testing.B, i int) {
	reach := make([]int, r.arm.repos)
	for k := range r.arm.concurrency {
		reach[(k+i*r.arm.concurrency)%r.arm.repos]++
	}
	r.mu.Lock()
	for repo, n := range reach {
		if r.arm.kind != writeBranch && r.packs[repo]+n > code.MaxPacksPerRepo {
			r.mu.Unlock()
			b.Fatalf("round %d would take %s past code.MaxPacksPerRepo (%d live packs), which nothing compacts in "+
				"this arm: run it for fewer rounds (-benchtime)", i, r.names[repo], code.MaxPacksPerRepo)
		}
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for k := range r.arm.concurrency {
		br := r.branches[[2]int{k, (k + i*r.arm.concurrency) % r.arm.repos}]
		wg.Go(func() { r.one(b.Context(), br, i) })
	}
	wg.Wait()
	if r.failed != nil {
		b.Fatal(r.failed)
	}
}

// one is one write, timed from its tool's call to the tool's answer.
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
		r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s answered without asking the code host: %s", tool.Name(),
			br.ref, out.Output))
	case unready(call.err):
		r.unready++
	case call.err != nil:
		r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s: %w", tool.Name(), br.ref, call.err))
	case res.Command.Reason == code.RefusalBusy:
		r.busy++
	case res.Outcome != statelog.OutcomeApplied || res.Refusal != "" || res.Commit == "":
		r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s answered %+v", tool.Name(), br.ref, res))
	default:
		if answered := toolAnswered(out); out.Failed || answered.Outcome != statelog.OutcomeApplied ||
			answered.Commit != res.Commit {
			r.failed = cmpOr(r.failed, fmt.Errorf("%s on %s applied commit %s and its tool answered %q", tool.Name(),
				br.ref, res.Commit, out.Output))
			return
		}
		r.took = append(r.took, end.Sub(start))
		r.tool = append(r.tool, call.askedAt.Sub(start)+end.Sub(call.answerAt))
		r.ended[res.PushID] = call.answerAt
		if r.arm.kind != writeBranch {
			br.head = res.Commit
			r.packs[br.repo]++
		}
	}
}

// toolAnswer is what a write tool's answer says a write came to: the fields a
// turn reads off it.
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

// unready reports a write no git node ran because none could run it now —
// the walk's answer when its one git node answered code.ErrGitUnready (its
// share full past code.GitShareWait, its copy still syncing): a refusal that
// names the node, never the write, and that nobody went silent for. A git
// node that went silent is no such answer, and fails the arm.
func unready(err error) bool {
	var (
		node *code.ErrGitUnready
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
		person = newBesidePusher(b, r.f, r.node)
		var err error
		if alone, err = person.paced(b.Context(), besideAlone); err != nil {
			b.Fatal(err)
		}
		r.fig.reset()
	}
	watch, stopWatch := context.WithCancel(b.Context())
	peak := watchInFlight(watch, r.node.Host())
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
	elapsed := time.Since(start)
	stopPushing()
	stopWatch()
	inFlight := peak()

	r.mu.Lock()
	took, tool, applied := inOrder(r.took), inOrder(r.tool), len(r.took)
	asked := rounds * r.arm.concurrency
	var adopt []time.Duration
	r.fig.mu.Lock()
	for id, end := range r.ended {
		if at, ok := r.fig.answered[id]; ok {
			adopt = append(adopt, end.Sub(at))
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
	b.ReportMetric(float64(applied)/elapsed.Seconds(), "writes/s")
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
	if len(uploads) > 0 {
		b.ReportMetric(quantile(uploads, 0.50), "upload-p50-ms")
		b.ReportMetric(quantile(uploads, 0.99), "upload-p99-ms")
	}
	b.ReportMetric(quantile(proposals, 0.50), "propose-p50-ms")
	b.ReportMetric(quantile(proposals, 0.99), "propose-p99-ms")
	b.ReportMetric(quantile(adopt, 0.50), "adopt-p50-ms")
	b.ReportMetric(quantile(adopt, 0.99), "adopt-p99-ms")
	b.ReportMetric(float64(puts)/float64(applied), "puts/write")
	b.ReportMetric(float64(r.fig.getN.Load())/float64(applied), "gets/write")
	b.ReportMetric(float64(unready)/float64(asked), "unready/write")
	b.ReportMetric(float64(busy)/float64(asked), "busy/write")
	b.ReportMetric(float64(pending)/float64(asked), "pending/write")
	b.ReportMetric(float64(inFlight), "inflight-peak")
	if r.arm.kind != writeBranch && packs != len(proposals) {
		b.Errorf("%d of %d proposals named a pack: every %s's push carries one", packs, len(proposals), r.arm.kind)
	}

	if r.arm.store.firstByte == 200*time.Millisecond {
		if p99 := quantile(took, 0.99); p99 >= float64(estate.GitWriteAttempt/4)/float64(time.Millisecond) {
			b.Errorf("the whole write's p99 is %.0f ms at a 200 ms first byte, past a quarter of "+
				"estate.GitWriteAttempt (%s)", p99, estate.GitWriteAttempt)
		}
		if p99 := quantile(uploads, 0.99); p99 >= float64(code.WriteUploadAllowance/10)/float64(time.Millisecond) {
			b.Errorf("the upload's p99 is %.0f ms at a 200 ms first byte, past a tenth of "+
				"code.WriteUploadAllowance (%s)", p99, code.WriteUploadAllowance)
		}
	}
	if person == nil {
		return
	}
	got := <-besides
	if got.err != nil {
		b.Fatal(got.err)
	}
	// AND ALONE AGAIN, once the writes have stopped: the repository then
	// holds every pack the arm's writes added, which nothing in an arm
	// compacts, so this tells a push slowed by the writes beside it from
	// one slowed by the packs they left.
	after, err := person.paced(b.Context(), besideAlone)
	if err != nil {
		b.Fatal(err)
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
	if r.arm.concurrency == writesAtOnce && quantile(beside, 0.99) > 2*quantile(alone, 0.99) {
		b.Errorf("the person's push p99 beside %d writes at once is %.0f ms, past twice its %.0f ms alone",
			writesAtOnce, quantile(beside, 0.99), quantile(alone, 0.99))
	}
}

// watchInFlight samples how many writes s holds every millisecond until ctx
// ends, and answers the most it saw once it has stopped.
func watchInFlight(ctx context.Context, s *host.Server) func() int {
	var peak int
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Millisecond)
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
		<-done
		return peak
	}
}

// besideResult is a beside=push arm's pushes while its writes ran.
type besideResult struct {
	took []time.Duration
	err  error
}

// besidePusher is a person pushing one commit at a time to an arm's first
// repository's main, through the arm's git node.
type besidePusher struct {
	p    *codetest.Person
	work string
	url  string
	n    int
}

// newBesidePusher is ceo, with RepoName's main seeded from a work tree of
// theirs.
func newBesidePusher(b *testing.B, f *codetest.SoloFleet, node codetest.Node) *besidePusher {
	b.Helper()
	p := codetest.NewPerson(b, f, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "seed")
	mustPush(b, p, work, repoURL(node), "main")
	return &besidePusher{p: p, work: work, url: repoURL(node)}
}

// push makes one commit of one 4 KiB file, untimed, and answers how long its
// push took the person's git.
func (bp *besidePusher) push(ctx context.Context) (time.Duration, error) {
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
	return time.Since(start), nil
}

// paced is n pushes — every push until ctx ends where n is 0 — one every
// besidePace, each starting at once where the one before took longer.
func (bp *besidePusher) paced(ctx context.Context, n int) ([]time.Duration, error) {
	var took []time.Duration
	for i := 0; n == 0 || i < n; i++ {
		start := time.Now()
		d, err := bp.push(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return took, nil
		case err != nil:
			return took, err
		}
		took = append(took, d)
		select {
		case <-ctx.Done():
			return took, nil
		case <-time.After(time.Until(start.Add(besidePace))):
		}
	}
	return took, nil
}

// BenchmarkEngineCommit is AN ENGINE COMMIT'S BUILD ALONE (ADR-0062: build.go's
// plan, its one read of the copy, its new files hashed, its touched
// directories written a level at a time, its commit, and its pack and that
// pack's index-pack --strict), as the host times it (crewlet.code.host.write.
// duration's build stage, which holds the write's unit of the seat git-work
// share): every write a commit_files by alice creating a branch of her own at
// the same commit, its files' text its own, asked of the git node's host
// directly, the build alone being the subject — so every build of an arm does
// the same work, and none leans on the one before. Each fixture's copy is the
// one a node that stood still serves — indexed whole, its bitmap and graph
// written (warmIndex) — and its maintainer is then held off for the arms
// (holdMaintenance), so no index of the packs the arms' writes add is written
// beside a build: on a few cores such a run, a heavier one over the larger
// tree, would be timed into the builds it ran beside — the maintainer's cost,
// which internal/code/cache measures, never the build's. Both trees' fixtures
// are built first, each warmed by a few untimed builds, and every files= and
// depth= arm then runs in the one tree and the other back to back
// (files=/depth=/tree=), so the two builds an assertion compares are taken
// on one moment of the machine.
//
//   - tree= a tree of about 10 000 files, and of 100 000 (CREWLET_BENCH_LARGE),
//     built by gittest.FastImport along the path p1/…/p6: every directory of
//     that path, the root's too, holds the next one, commitSiblingFiles files
//     and commitSiblingDirs directories, and those directories hold the rest
//     of the files between them — so every directory a build touches holds
//     the same entries in either tree, and what differs is what a build must
//     never read (TestAnEngineCommitReadsNoWholeTree). Every file is a blob of
//     its own, as a repository's are, so the copy's objects are the tree's.
//   - files= 1, 10 and 100 files put into the path's directory at depth= 1, 3
//     and 6; and code.MaxFileOps files at depth 6, the bound itself.
//   - flat= one file put into a directory of 10 000 entries, and of 100 000
//     (CREWLET_BENCH_LARGE), each a blob of its own: the one cost that follows
//     a directory's ENTRIES — mktree reads each entry, the directory's tree is
//     written whole into the write's self-contained pack, its every id
//     incompressible, and index-pack --strict checks each entry's link into
//     the copy.
//
// Each arm reports its build's p50 and p99 and its pack's bytes (pack-bytes,
// the largest of the arm's, every write's alike). HELD by its own assertions:
// for each files= and depth=, the two trees' build p50 within a tenth of each
// other — the cost follows the change, never the tree — where both ran; a
// hundred files at depth 6 in the 100 000-file tree under a tenth of
// code.WriteBuildDeadline; code.MaxFileOps files there inside the whole of
// it; and the 100 000-entry directory's pack under code.MaxWritePackBytes.
func BenchmarkEngineCommit(b *testing.B) {
	codetest.Quiet(b)
	// BOTH TREES' FIXTURES FIRST, then each arm in the one tree and the other
	// back to back: an arm's two trees are compared, so their builds are
	// taken seconds apart rather than one tree's whole run apart. Run tree by
	// tree on a shared two-core slot, the larger tree's single-file arms
	// came right after its fixture's import and minutes after the smaller
	// tree's, and were 30–45% slower where the ten-file arms between them
	// were within 5%: the comparison read the machine's moment, never the
	// tree.
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
					case arm.files == 100 && p99 >= float64(code.WriteBuildDeadline/10)/float64(time.Millisecond):
						b.Errorf("a hundred files at depth %d in a tree of %d build in %.0f ms at the p99, past a "+
							"tenth of code.WriteBuildDeadline (%s)", commitDepth, files, p99, code.WriteBuildDeadline)
					case arm.files == code.MaxFileOps &&
						p99 >= float64(code.WriteBuildDeadline)/float64(time.Millisecond):
						b.Errorf("code.MaxFileOps files at depth %d in a tree of %d build in %.0f ms at the p99, "+
							"past code.WriteBuildDeadline (%s)", commitDepth, files, p99, code.WriteBuildDeadline)
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
			if entries == commitTreeFilesLarge && pack >= code.MaxWritePackBytes {
				b.Errorf("a file put into a directory of %d entries packs %d bytes, past code.MaxWritePackBytes "+
					"(%d)", entries, pack, code.MaxWritePackBytes)
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

// commitArms are 1, 10 and 100 files at depths 1, 3 and 6, and code.MaxFileOps
// files at depth 6.
func commitArms() []commitArm {
	var arms []commitArm
	for _, files := range []int{1, 10, 100} {
		for _, depth := range []int{1, 3, commitDepth} {
			arms = append(arms, commitArm{files, depth})
		}
	}
	return append(arms, commitArm{code.MaxFileOps, commitDepth})
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

// inlineFile writes one fast-import file line whose blob is its own: the file
// at path holding its own path, so no two files of a fixture share a blob, as
// no two of a repository's directories' entries name one id in the common
// case — and the ids a touched directory's tree holds compress as a real
// tree's do, which is not at all.
func inlineFile(w *bufio.Writer, path string) error {
	content := path + "\n"
	_, err := fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n%s\n", path, len(content), content)
	return err
}

// commitTree writes the tree= arms' tree of about files files as fast-import
// file lines, every one a blob of its own: along the path p1/…/p6, every
// directory — the root's too — holds the next, commitSiblingFiles files and
// commitSiblingDirs directories, and the rest of the files are spread over
// those directories.
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

// commitRig is a BenchmarkEngineCommit fixture: one git node holding a
// repository of one root commit, its tree the arm's, pushed by ceo.
type commitRig struct {
	f    *codetest.SoloFleet
	node codetest.Node
	fig  *writeFigures
	repo string
	base string
	pos  statelog.Position
	who  code.Principal
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
	r.f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	p := codetest.NewPerson(b, r.f, "ceo")
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
	rows, err := r.f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	r.repo, r.pos = rows.Repo.ID, rows.Position
	warmIndex(b, r.node, r.repo, 0)
	holdMaintenance(b, r.node)
	return r
}

// holdMaintenance holds every slot of node's maintenance lane until b ends —
// taken as each comes free, so a run the maintainer has in hand ends first —
// so its maintainer writes nothing more while b runs.
func holdMaintenance(b *testing.B, node codetest.Node) {
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
		case !errors.Is(err, gitproc.ErrBusy) || time.Now().After(deadline):
			b.Fatalf("hold node %s's maintenance lane, %d of its %d slots held: %v", node.ID(), len(held), slots,
				err)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// commitWarmBuilds is how many untimed builds each rig makes before its
// first arm: the first builds after a fixture run beside what its import
// and push left the machine — the page cache, the Go heap, the copy's
// first reads of its packs — which no later build of the arm meets.
const commitWarmBuilds = 5

// warm makes the rig's commitWarmBuilds untimed builds.
func (r *commitRig) warm(b *testing.B) {
	b.Helper()
	for range commitWarmBuilds {
		r.build(b, 1, commitPath(commitDepth))
	}
}

// build is the rig's next write: files files put into dir by a write
// creating a branch at the rig's commit, asked of the git node's host.
func (r *commitRig) build(b *testing.B, files int, dir string) {
	b.Helper()
	r.n++
	w := code.EngineWrite{Kind: code.WriteCommitFiles, RepoID: r.repo, Principal: r.who, OpID: opNow(),
		Fresh: statelog.FloorAt(statelog.ReadSession, r.pos), Base: r.base, Message: fmt.Sprintf("build %d", r.n),
		Ref: fmt.Sprintf("refs/heads/seats/%s/b%06d", codetest.FixtureAlice, r.n)}
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
