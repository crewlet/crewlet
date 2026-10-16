package codetest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Node is one git node of a topology: its own cache, git pool, host and HTTP
// server.
type Node interface {
	// ID is the node's id.
	ID() string
	// URL is the base URL of the node's server, which serves the host's
	// three git routes behind the API's guard: Basic on a git route, and the
	// 401 challenge git answers with its credential.
	URL() string
	// Client is the node's server's own HTTP client, for a request a case
	// builds by hand (httpxtest's rule: a double's client is its server's).
	Client() *http.Client
	Host() *host.Server
	Cache() *cache.Cache
	Pool() *gitproc.Pool
	// Metrics is the recorder the node's pool, cache and host record to —
	// one, as the engine's are.
	Metrics() *metrics.Recorder
	// CacheDir is the node's cache directory.
	CacheDir() string
	// Read is a read of the code host's rows, as the API's code reads ask.
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
}

// Fleet is a topology under test.
type Fleet interface {
	Nodes() []Node
	// Declare ensures each repository at the fleet's next activation.
	Declare(t testing.TB, repos ...code.Declared)
	// Remove deletes the named repository at the next activation.
	Remove(t testing.TB, name string)
	// Purge runs code-retention passes with the shortest retention there is
	// — the domain refuses a zero one — until every purge they began is
	// done.
	Purge(t testing.TB)
	// Collect runs one collection over the code estate, its clock a day and
	// an hour on, so every object past its grace is judged.
	Collect(t testing.TB) collect.CollectionReport
	// Token is a token bound to the human seat person; "" is one bound to
	// nobody.
	Token(t testing.TB, person string) string
	// Faults is the fleet's fault script, shared across restarts.
	Faults() *codefault.Script
	// Evict and Readmit evict and readmit the data node.
	Evict(t testing.TB, node string)
	Readmit(t testing.TB, node string)
	// Kill is a process death, as far as git and the store see it, and
	// Restart what that process comes back as over the same files.
	Kill(t testing.TB, node string)
	Restart(t testing.TB, node string)
	// PauseApplier stops the data node's applier until resume.
	PauseApplier(t testing.TB, node string) (resume func())
	// Objects is the object store's backend.
	Objects() objstore.Backend
	// Audited is every push record a host published, in order.
	Audited() []types.OperatorActed
	// DataNodes are the ids of the nodes that hold the estate.
	DataNodes() []string
	// Repos is the name of every active or archived repository, read
	// linearizable.
	Repos(ctx context.Context) ([]string, error)
	// Rows is one repository's whole state as its rows hold it at fresh;
	// code.ErrRepoNotFound when no active or archived repository has the
	// name.
	Rows(ctx context.Context, name string, fresh statelog.Freshness) (Rows, error)
}

// Shape is what a case asks of the fleet it is run on.
type Shape struct {
	// GitNodes is how many git nodes serve the estate; 0 is one.
	GitNodes int
	// Limits are code.limits; a zero field takes the default.
	Limits code.Limits
	// Timing is every host's; the zero value is host.Production with a
	// PendingGrace of two seconds and a Keepalive of 200 ms — a case
	// asserting the production grace sets it.
	Timing host.Timing
	// MaxTransfers is each git pool's transfer lane; 0 is the pool's
	// default.
	MaxTransfers int
	// WrapEstate wraps what every git node's host and cache ask — the
	// window a case has onto the estate.
	WrapEstate func(host.Estate) host.Estate
}

// NewFleet builds a fresh fleet of a shape, for one case.
type NewFleet func(t *testing.T, s Shape) Fleet

// SoloOptions configure a SoloNode.
type SoloOptions struct {
	// Shape is the case's.
	Shape
	// Objects is the object store's backend; nil is a fresh memobj.
	Objects objstore.Backend
	// Faults is the fleet's script; nil is a fresh one.
	Faults *codefault.Script
	// MaxProcesses is each git pool's read lane; 0 is the pool's default.
	MaxProcesses int
	// People are the human seats with a token each; nil is ceo and dev.
	People []string
	// GitPath is the git every pool runs; "" is the one on PATH (a case
	// that records what git ran sets gittest.Recording's).
	GitPath string
	// CacheMaxBytes is every git node's cache budget; 0 is the git node's
	// default.
	CacheMaxBytes int64
	// Logger is every host's; nil is the package's own.
	Logger *slog.Logger
	// SyncAlways has the broker fsync every write before it acknowledges it,
	// as stream.sync's default (always) has a deployment's: what a BENCHMARK
	// sets, so a figure carries the record's fsync a production push pays. A
	// case leaves it off — it judges what is written, never when it reaches
	// the disk, and an fsync a record only slows the suite.
	SyncAlways bool
}

// Solo is a NewFleet of SoloNodes over o, each case's shape laid over it — a
// wrapper o carries wrapping the case's. objects, when not nil, is each
// fleet's own backend (a case's fleet never shares a bucket with another's).
func Solo(o SoloOptions, objects func(t *testing.T) objstore.Backend) NewFleet {
	return func(t *testing.T, s Shape) Fleet {
		t.Helper()
		opts := o
		wrap := o.WrapEstate
		opts.Shape = s
		if wrap != nil {
			opts.WrapEstate = func(e host.Estate) host.Estate {
				if s.WrapEstate != nil {
					e = s.WrapEstate(e)
				}
				return wrap(e)
			}
		}
		if objects != nil {
			opts.Objects = objects(t)
		}
		return SoloNode(t, opts)
	}
}

// soloDataNode is the data node's id, which its records name, and
// soloOperatorNode the id an operator's gestures are published under.
const (
	soloDataNode     = "solo"
	soloOperatorNode = "solo-operator"
)

// SoloNode is ONE data node and its git nodes, in one process: the engine's
// own composition for the code domain, built from the two blocks every
// topology is built from (StartDataNode, StartGitNode) over one embedded
// broker.
func SoloNode(t testing.TB, o SoloOptions) *SoloFleet {
	t.Helper()
	if o.Objects == nil {
		o.Objects = memobj.New()
	}
	if o.Faults == nil {
		o.Faults = &codefault.Script{}
	}
	if o.GitNodes == 0 {
		o.GitNodes = 1
	}
	o.Limits = withDefaults(o.Limits)
	if o.People == nil {
		o.People = []string{"ceo", "dev"}
	}
	f := &SoloFleet{o: o, dir: t.TempDir(), creds: NewCredentials(o.People),
		// AN HOUR AGO, so an activation a case mints — a second apart, in
		// order — is never in the future: a deletion is dated by its
		// activation, and the purge judges its age on the wall clock.
		base: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	// REGISTERED FIRST, SO IT RUNS LAST: every node's own teardown stops it
	// before its broker does.
	t.Cleanup(f.teardown)
	f.data = StartDataNode(t, DataOptions{ID: soloDataNode, Queue: f.openBroker(t), Dir: f.dir,
		Limits: o.Limits, Faults: o.Faults, LogBytes: soloLogBytes, Operator: soloOperatorNode})
	var estate host.Estate = &soloEstate{data: f.data}
	if o.WrapEstate != nil {
		estate = o.WrapEstate(estate)
	}
	for i := range o.GitNodes {
		id := fmt.Sprintf("solo-%c", 'a'+i)
		f.git = append(f.git, StartGitNode(t, GitOptions{
			ID: id, Dir: filepath.Join(f.dir, id, "git"), Estate: estate,
			Reads: func(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
				return f.data.Served().Read(ctx, q)
			},
			Objects: func() objstore.Backend { return o.Objects }, Limits: o.Limits, Timing: o.Timing,
			Faults: o.Faults, Credentials: f.creds, Audit: f.record,
			MaxTransfers: o.MaxTransfers, MaxProcesses: o.MaxProcesses, CacheMaxBytes: o.CacheMaxBytes,
			GitPath: o.GitPath, Logger: o.Logger,
		}))
	}
	return f
}

// GitNode is the fleet's i-th git node, for a benchmark that stops it and
// starts it again over an emptied directory — a node that has never served the
// repository — while the rest of the fleet runs on.
func (f *SoloFleet) GitNode(i int) *GitNode { return f.git[i] }

// Data is the fleet's one data node, for a benchmark that proposes the pushes
// of a fixture to its committer directly — refs created by records, without a
// person's git paying its own cost for them.
func (f *SoloFleet) Data() *DataNode { return f.data }

// soloLogBytes is the code log's ceiling on a solo node: 64 MiB holds every
// record the suite writes.
const soloLogBytes = 64 << 20

// SoloFleet is the one-data-node topology: a Fleet, and its git nodes for a
// benchmark that empties one between iterations (GitNode).
type SoloFleet struct {
	o     SoloOptions
	dir   string
	creds *Credentials
	base  time.Time

	data *DataNode
	git  []*GitNode

	mu sync.Mutex
	// brokers is every incarnation's embedded broker, the running one last.
	brokers     []*js.Queue
	activations int
	audited     []types.OperatorActed
}

// openBroker opens the node's embedded broker over its own directory — the one
// it had before, on a restart.
func (f *SoloFleet) openBroker(t testing.TB) *js.Queue {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: filepath.Join(f.dir, "stream"), SyncAlways: f.o.SyncAlways})
	if err != nil {
		t.Fatalf("codetest: open the broker: %v", err)
	}
	f.mu.Lock()
	f.brokers = append(f.brokers, q)
	f.mu.Unlock()
	return q
}

// broker is the running incarnation's broker.
func (f *SoloFleet) broker() *js.Queue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.brokers[len(f.brokers)-1]
}

// record keeps every push record a host published.
func (f *SoloFleet) record(_ context.Context, e types.OperatorActed) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audited = append(f.audited, e)
}

// Nodes is every serving node the fleet runs now, in order (Fleet).
func (f *SoloFleet) Nodes() []Node {
	out := make([]Node, 0, len(f.git))
	for _, g := range f.git {
		if n := g.Node(); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// Token is a token bound to the human seat person, "" one bound to nobody
// (Fleet).
func (f *SoloFleet) Token(_ testing.TB, person string) string { return f.creds.Token(person) }

// Faults is the fleet's fault script, shared across restarts (Fleet).
func (f *SoloFleet) Faults() *codefault.Script { return f.o.Faults }

// Objects is the object store's backend (Fleet).
func (f *SoloFleet) Objects() objstore.Backend { return f.o.Objects }

// DataNodes is the one data node's id (Fleet).
func (f *SoloFleet) DataNodes() []string { return []string{soloDataNode} }

// Repos is the name of every active or archived repository, read
// linearizable from the data node (Fleet).
func (f *SoloFleet) Repos(ctx context.Context) ([]string, error) {
	return ReposOf(ctx, f.data.Served().Read)
}

// ReposOf is the name of every active or archived repository, read
// linearizable through read — a node's code half, or its router.
func ReposOf(ctx context.Context,
	read func(context.Context, code.ReadQuery) (code.ReadAnswer, error)) ([]string, error) {

	var names []string
	q := code.ReadQuery{Kind: code.ReadRepos, Limit: 500, Fresh: Linearizable}
	for {
		a, err := read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, r := range a.Repos {
			names = append(names, r.Name)
		}
		if a.Next == "" {
			return names, nil
		}
		q.Cursor = a.Next
	}
}

// Rows is one repository's whole state as the data node's rows hold it at
// fresh (Fleet).
func (f *SoloFleet) Rows(ctx context.Context, name string, fresh statelog.Freshness) (Rows, error) {
	served := f.data.Served()
	return RowsOf(ctx, name, fresh, served.Resolve, served.Snapshot)
}

// RowsOf is the repository called name's whole state as its rows hold it at
// fresh, resolved through resolve and read through state, every page —
// through a node's code half, or its router. code.ErrRepoNotFound when no
// active or archived repository has the name.
func RowsOf(ctx context.Context, name string, fresh statelog.Freshness,
	resolve func(context.Context, code.ResolveQuery) (code.RepoRef, error),
	state func(context.Context, code.StateQuery) (code.RepoSnapshot, error)) (Rows, error) {

	repo, err := resolve(ctx, code.ResolveQuery{Name: name, Fresh: fresh})
	if err != nil {
		return Rows{}, err
	}
	return snapshotRows(ctx, repo, fresh, state)
}

// Audited is every push record a host published, in order (Fleet).
func (f *SoloFleet) Audited() []types.OperatorActed {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.audited)
}

// next is the fleet's next activation instant: monotone, so EnsureRepo never
// meets an older one.
func (f *SoloFleet) next() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activations++
	return f.base.Add(time.Duration(f.activations) * time.Second)
}

// Declare ensures each repository at the fleet's next activation (Fleet).
func (f *SoloFleet) Declare(t testing.TB, repos ...code.Declared) {
	t.Helper()
	f.data.Declare(t, f.next(), repos...)
}

// Remove deletes the named repository at the next activation (Fleet).
func (f *SoloFleet) Remove(t testing.TB, name string) {
	t.Helper()
	f.data.Remove(t, f.next(), name)
}

// Purge runs code-retention passes until every purge they began is done
// (Fleet).
func (f *SoloFleet) Purge(t testing.TB) {
	t.Helper()
	f.data.Purge(t)
}

// Collect runs one collection over the code estate (Fleet).
func (f *SoloFleet) Collect(t testing.TB) collect.CollectionReport {
	t.Helper()
	return f.data.Collect(t, f.o.Objects)
}

// Evict and Readmit go through the data node's operator writer
// (soloOperatorNode): an evicted node's own records are dropped, its
// readmission included, and a solo fleet has no peer to publish it.
func (f *SoloFleet) Evict(t testing.TB, node string) {
	t.Helper()
	f.mustBeData(t, node)
	f.data.Evict(t, node)
}

// Readmit readmits the evicted data node (Fleet), as Evict says.
func (f *SoloFleet) Readmit(t testing.TB, node string) {
	t.Helper()
	f.mustBeData(t, node)
	f.data.Readmit(t, node)
}

// mustBeData fails t unless node is the fleet's one data node.
func (f *SoloFleet) mustBeData(t testing.TB, node string) {
	t.Helper()
	if node != soloDataNode {
		t.Fatalf("codetest: the solo fleet's data node is %q, not %q", soloDataNode, node)
	}
}

// Kill ends the whole solo process — one data node is one process — as far
// as git and the store see it: every git node killed, the data node killed,
// the broker stopped. It WAITS FOR NOTHING A CRASH MAY HOLD: an HTTP server's
// Close and the committer's Stop wait for their held work in the teardown,
// after the crashes are released.
func (f *SoloFleet) Kill(t testing.TB, _ string) {
	t.Helper()
	if f.data.proc() == nil {
		t.Fatal("codetest: Kill of a fleet that is not running")
	}
	for _, g := range f.git {
		g.Kill()
	}
	f.data.Kill()
	if err := f.broker().Stop(context.Background()); err != nil {
		t.Errorf("codetest: stop the killed node's broker: %v", err)
	}
}

// Restart rebuilds the process over the same directories: a new broker over
// the old one's files, the data node and every git node — new listeners, so a
// node's URL is the new one.
func (f *SoloFleet) Restart(t testing.TB, _ string) {
	t.Helper()
	if f.data.proc() != nil {
		t.Fatal("codetest: Restart of a fleet that was not killed")
	}
	f.data.Restart(t, f.openBroker(t))
	for _, g := range f.git {
		g.Restart(t)
	}
}

// PauseApplier stops the data node's applier until resume (Fleet).
func (f *SoloFleet) PauseApplier(t testing.TB, _ string) (resume func()) {
	t.Helper()
	return f.data.PauseApplier(t)
}

// teardown stops every incarnation's broker, the crashes released FIRST —
// though every node's own teardown, which ran before this one, released them.
func (f *SoloFleet) teardown() {
	f.o.Faults.ReleaseCrashes()
	f.mu.Lock()
	brokers := slices.Clone(f.brokers)
	f.brokers = nil
	f.mu.Unlock()
	for _, q := range brokers {
		_ = q.Stop(context.Background())
	}
}

// soloEstate is the data node's code half as the host and the cache ask it,
// in-process: code.Served, the one adapter the engine hands the router, read
// again at every call so a restarted node's is the one asked — with a
// committer's refusal about itself answered in the router's shape.
type soloEstate struct{ data *DataNode }

func (e *soloEstate) Resolve(ctx context.Context, q code.ResolveQuery) (code.RepoRef, error) {
	return e.data.Served().Resolve(ctx, q)
}

func (e *soloEstate) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	return e.data.Served().Snapshot(ctx, q)
}

// Propose asks the committer. A refusal about the node itself
// (code.ErrNotServing) is answered as the estate router answers a walk in
// which every node it asked refused and nobody went silent: nothing ran the
// push.
func (e *soloEstate) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	res, err := e.data.Served().Propose(ctx, p)
	if errors.Is(err, code.ErrNotServing) {
		return res, &soloUnserved{err: err}
	}
	return res, err
}

func (e *soloEstate) PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error) {
	return e.data.Served().PushOutcome(ctx, q)
}

// soloUnserved is the router's ErrUnserved for the one data node a solo
// fleet has: a walk that ran nothing, in which nobody went silent.
type soloUnserved struct{ err error }

func (u *soloUnserved) Error() string {
	return "estate: no data node serves the estate right now: " + soloDataNode + ": " + u.err.Error()
}
func (u *soloUnserved) Unwrap() error { return u.err }
func (u *soloUnserved) Silent() bool  { return false }
