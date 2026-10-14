package codetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
	// Metrics is the recorder the node's pool and host record to.
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
	// Logger is every host's; nil is the package's own.
	Logger *slog.Logger
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
// own composition for the code domain.
func SoloNode(t *testing.T, o SoloOptions) Fleet {
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
	defaults := code.DefaultLimits()
	if o.Limits.MaxPushBytes == 0 {
		o.Limits.MaxPushBytes = defaults.MaxPushBytes
	}
	if o.Limits.MaxBlobBytes == 0 {
		o.Limits.MaxBlobBytes = defaults.MaxBlobBytes
	}
	if o.Limits.RepoMaxBytes == 0 {
		o.Limits.RepoMaxBytes = defaults.RepoMaxBytes
	}
	if o.Timing == (host.Timing{}) {
		o.Timing = host.Production
		o.Timing.PendingGrace, o.Timing.Keepalive = 2*time.Second, 200*time.Millisecond
	}
	if o.People == nil {
		o.People = []string{"ceo", "dev"}
	}
	objects, err := objstore.NewStore(o.Objects)
	if err != nil {
		t.Fatalf("codetest: the object store: %v", err)
	}
	f := &solo{t: t, o: o, dir: t.TempDir(), objects: objects, tokens: map[string]string{},
		// AN HOUR AGO, so an activation a case mints — a second apart, in
		// order — is never in the future: a deletion is dated by its
		// activation, and the purge judges its age on the wall clock.
		base: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	for _, p := range o.People {
		f.tokens[p] = newToken()
	}
	f.tokens[""] = newToken()
	t.Cleanup(f.teardown)
	f.start(t)
	return f
}

// solo is the one-data-node topology.
type solo struct {
	t       *testing.T
	o       SoloOptions
	dir     string
	objects *objstore.Store
	tokens  map[string]string // person → token; "" bound to nobody
	base    time.Time

	mu          sync.Mutex
	current     *process
	dead        []*process
	activations int
	audited     []types.OperatorActed
}

// process is one incarnation of the solo node: everything a crash ends.
type process struct {
	q        *js.Queue
	log      *js.DomainLog
	consumer *js.DomainConsumer
	db       *store.DB
	estate   store.ReplicatedHandle

	runner    *statelog.Runner
	applyMu   sync.Mutex
	applyStop func() error

	pub       *statelog.Publisher
	reader    *code.Reader
	committer *code.Committer
	lifecycle *code.Store
	// operator publishes an operator's gestures (soloOperatorNode).
	operator  *code.Store
	estateFor host.Estate

	nodes []*gitNode
}

// gitNode is one git node of an incarnation.
type gitNode struct {
	id    string
	dir   string
	cache *cache.Cache
	pool  *gitproc.Pool
	// metrics is the node's recorder: its pool's and its host's.
	metrics *metrics.Recorder
	host    *host.Server
	srv     *httptest.Server
	reader  *code.Reader
}

func (n *gitNode) ID() string                 { return n.id }
func (n *gitNode) URL() string                { return n.srv.URL }
func (n *gitNode) Client() *http.Client       { return n.srv.Client() }
func (n *gitNode) Host() *host.Server         { return n.host }
func (n *gitNode) Cache() *cache.Cache        { return n.cache }
func (n *gitNode) Pool() *gitproc.Pool        { return n.pool }
func (n *gitNode) Metrics() *metrics.Recorder { return n.metrics }
func (n *gitNode) CacheDir() string           { return n.dir }
func (n *gitNode) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return n.reader.Read(ctx, q)
}

// newToken is a credential's random value.
func newToken() string {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// start builds one incarnation over the fleet's directories: the broker, the
// estate, the applier, the write and read authorities, the committer, the
// lifecycle store, and every git node — the engine's order.
func (f *solo) start(t testing.TB) {
	t.Helper()
	ctx := f.t.Context()
	p := &process{}
	var err error
	if p.q, err = js.Open(ctx, js.Config{StoreDir: filepath.Join(f.dir, "stream")}); err != nil {
		t.Fatalf("codetest: open the broker: %v", err)
	}
	spec := code.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD OVERRIDDEN: an embedded broker in a
	// temporary directory refuses to reserve the shipped one, and 64 MiB
	// holds every record the suite writes.
	spec.MaxBytes = 64 << 20
	if err = p.q.EnsureDomainStream(ctx, js.DomainStream{Name: spec.Name, Subjects: spec.Subjects,
		MaxBytes: spec.MaxBytes, Duplicates: spec.Duplicates}); err != nil {
		t.Fatalf("codetest: provision the code log: %v", err)
	}
	if p.log, err = p.q.DomainLog(ctx, spec.Name); err != nil {
		t.Fatalf("codetest: open the code log: %v", err)
	}
	stats, err := p.log.Stats(ctx)
	if err != nil {
		t.Fatalf("codetest: read the code log: %v", err)
	}
	// THE ESTATE, seeded where none exists and opened as it is where one
	// does — which is what a restarted process does.
	p.db, p.estate = storetest.OpenEstate(t, filepath.Join(f.dir, "node.db"), store.Options{}, 1)
	checkpoint, _, err := statelog.CheckpointOf(ctx, p.estate.Reader(), spec.Name)
	if err != nil {
		t.Fatalf("codetest: read the checkpoint: %v", err)
	}
	if p.consumer, err = p.q.DomainConsumer(ctx, spec.Name, soloDataNode, checkpoint.At.Seq); err != nil {
		t.Fatalf("codetest: open the consumer: %v", err)
	}
	fence := code.NewFence(p.estate.Reader(), soloDataNode)
	if p.runner, err = statelog.NewRunner(statelog.RunnerDeps{
		Domain: code.Domain{}, Spec: spec, Applier: code.NewApplier(), Fetch: p.consumer,
		Log: p.log, Node: p.db, DB: p.estate,
		Checkpoint: checkpoint.At, CheckpointStoredAt: checkpoint.StoredAt,
		StreamCreatedAt: stats.CreatedAt.UTC(), NodeID: soloDataNode,
		// THE ONE WRITER A SOLO LOG HAS is this node, so whether it is
		// evicted is the fence's own reading of its row.
		Evicted: func(ctx context.Context, node string) (bool, error) {
			if node != soloDataNode {
				return false, nil
			}
			return fence.Evicted(ctx)
		},
	}); err != nil {
		t.Fatalf("codetest: build the applier: %v", err)
	}
	p.startApplier()

	reserve, err := statelog.NewReserve(spec.Name, func(ctx context.Context) (statelog.Usage, error) {
		s, serr := p.log.Stats(ctx)
		return statelog.Usage{Bytes: s.Bytes, MaxBytes: s.MaxBytes}, serr
	})
	if err != nil {
		t.Fatalf("codetest: build the gate reserve: %v", err)
	}
	rows, err := code.NewRows(p.estate.Reader(), spec)
	if err != nil {
		t.Fatalf("codetest: build the read seam: %v", err)
	}
	// A FLEET THAT HAS NEVER TRIMMED: a solo test node runs no trim, so the
	// published floor is zero and the log's own ends are the fence's.
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, berr := p.log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, berr
	}
	fence.Committed = p.runner.Committed
	gen := func() uint32 { return p.runner.Committed().Generation }
	if p.pub, err = statelog.NewPublisher(statelog.Deps{
		Domain: code.Domain{}, Spec: spec, Log: p.log, Records: p.log, Rows: rows, Fence: fence,
		Gates: code.NewGates(p.estate.Reader()), Waiter: p.runner, Voids: p.runner, Identity: p.runner,
		Admission: reserve, NodeID: soloDataNode, Generation: gen,
	}); err != nil {
		t.Fatalf("codetest: build the write authority: %v", err)
	}
	index, err := statelog.NewReadIndex(code.Domain{}, spec, p.log, reserve, code.EncodeBarrier, gen, nil)
	if err != nil {
		t.Fatalf("codetest: build the read index: %v", err)
	}
	authority, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: code.Domain{}, Spec: spec, Mode: statelog.ModeNormal, DB: p.estate.Reader(),
		Index: index, Waiter: p.runner, Drain: p.runner.Drain,
		Health: func() (statelog.Health, error) { return p.health(ctx) },
	})
	if err != nil {
		t.Fatalf("codetest: build the read authority: %v", err)
	}
	limits := func() code.Limits { return f.o.Limits }
	if p.reader, err = code.NewReader(code.ReaderOptions{Log: authority, Limits: limits}); err != nil {
		t.Fatalf("codetest: build the reader: %v", err)
	}
	if p.committer, err = code.NewCommitter(code.CommitterOptions{
		Publisher: &faultPublisher{inner: p.pub, faults: f.o.Faults}, DB: p.estate.Reader(),
		NodeID: soloDataNode, Limits: limits, Retention: f.retention,
	}); err != nil {
		t.Fatalf("codetest: build the committer: %v", err)
	}
	if p.lifecycle, err = code.NewStore(code.StoreOptions{Publisher: p.pub, DB: p.estate.Reader(),
		NodeID: soloDataNode}); err != nil {
		t.Fatalf("codetest: build the lifecycle store: %v", err)
	}
	// AN OPERATOR'S GESTURE COMES THROUGH ANOTHER NODE: every record an
	// evicted node publishes is dropped, its own readmission included, so a
	// fleet readmits a node through a peer. A solo fleet has no peer, so its
	// operator's evictions and readmissions are published under a node id of
	// their own — a second writer on the one log, as a peer would be.
	opFence := code.NewFence(p.estate.Reader(), soloOperatorNode)
	opFence.Floor, opFence.Ends, opFence.Committed = fence.Floor, fence.Ends, fence.Committed
	opPub, err := statelog.NewPublisher(statelog.Deps{
		Domain: code.Domain{}, Spec: spec, Log: p.log, Records: p.log, Rows: rows, Fence: opFence,
		Gates: code.NewGates(p.estate.Reader()), Waiter: p.runner, Voids: p.runner, Identity: p.runner,
		Admission: reserve, NodeID: soloOperatorNode, Generation: gen,
	})
	if err != nil {
		t.Fatalf("codetest: build the operator's write authority: %v", err)
	}
	if p.operator, err = code.NewStore(code.StoreOptions{Publisher: opPub, DB: p.estate.Reader(),
		NodeID: soloOperatorNode}); err != nil {
		t.Fatalf("codetest: build the operator's lifecycle store: %v", err)
	}
	var estate host.Estate = &soloEstate{served: code.Served{Committer: p.committer, Reader: p.reader}}
	if f.o.WrapEstate != nil {
		estate = f.o.WrapEstate(estate)
	}
	p.estateFor = estate

	for i := range f.o.GitNodes {
		p.nodes = append(p.nodes, f.startGitNode(t, p, fmt.Sprintf("solo-%c", 'a'+i)))
	}
	f.mu.Lock()
	f.current = p
	f.mu.Unlock()
}

// startGitNode builds one git node in the engine's order: the cache prepared
// before the pool, whose HOME it names; the cache opened; the host built and
// started; its routes mounted behind the API's guard.
func (f *solo) startGitNode(t testing.TB, p *process, id string) *gitNode {
	t.Helper()
	ctx := f.t.Context()
	n := &gitNode{id: id, dir: filepath.Join(f.dir, id, "git"), reader: p.reader}
	var err error
	if n.metrics, err = metrics.New(); err != nil {
		t.Fatalf("codetest: a recorder: %v", err)
	}
	layout, err := prepare(ctx, n.dir)
	if err != nil {
		t.Fatalf("codetest: prepare %s's cache: %v", id, err)
	}
	defer layout.Release()
	if n.pool, err = gitproc.New(ctx, gitproc.Options{Path: f.o.GitPath, Home: layout.Home,
		MaxProcesses: f.o.MaxProcesses, MaxTransfers: f.o.MaxTransfers, Metrics: n.metrics}); err != nil {
		t.Fatalf("codetest: %s's git pool: %v", id, err)
	}
	limits := func() code.Limits { return f.o.Limits }
	if n.cache, err = cache.Open(ctx, cache.Options{
		Layout: layout, MaxBytes: 1 << 30, Git: n.pool, Objects: f.objects, Fetch: p.estateFor,
		Limits: limits, HookCommand: HookCommand(t), Faults: f.o.Faults, Now: time.Now,
	}); err != nil {
		t.Fatalf("codetest: open %s's cache: %v", id, err)
	}
	if n.host, err = host.New(host.Options{
		Cache: n.cache, Git: n.pool, Estate: p.estateFor, Objects: f.objects,
		Persons: f.person, Audit: f.record, Limits: limits,
		DeletedRetention: func() time.Duration { return 7 * 24 * time.Hour },
		NodeID:           id, Timing: f.o.Timing, Faults: f.o.Faults, Metrics: n.metrics, Logger: f.o.Logger,
	}); err != nil {
		t.Fatalf("codetest: build %s's host: %v", id, err)
	}
	if err := n.host.Start(ctx); err != nil {
		t.Fatalf("codetest: start %s's host: %v", id, err)
	}
	mux := http.NewServeMux()
	mux.Handle(host.PatternInfoRefs, n.host.InfoRefs())
	mux.Handle(host.PatternUploadPack, n.host.UploadPack())
	mux.Handle(host.PatternReceivePack, n.host.ReceivePack())
	n.srv = httptest.NewUnstartedServer(auth.New(f.boot()).Middleware(mux))
	n.srv.Config.ErrorLog = log.New(serverLog{t: f.t}, "", 0)
	n.srv.Start()
	return n
}

// serverLog is a git node's HTTP server's error log: A HANDLER THAT PANICS
// FAILS THE CASE. net/http recovers a handler's panic and only logs it — the
// client sees its connection end, which git prints as `RPC failed` and a
// case asserting a refusal might read as one — so the line is the one place
// the panic shows, and the suite fails on it by name. http.ErrAbortHandler,
// the host's deliberate cut of a response it cannot finish, is never logged.
// Every other line the server logs is the case's to read.
type serverLog struct{ t testing.TB }

func (l serverLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("http: panic serving")) {
		l.t.Errorf("codetest: a git node's handler panicked: %s", p)
	} else {
		l.t.Logf("codetest: a git node's server: %s", bytes.TrimSpace(p))
	}
	return len(p), nil
}

// prepare is cache.Prepare, asked again for a short while when another
// incarnation of this process still holds the directory: a killed node's
// cache gives its claim back the moment the last of its work ends, which in a
// process that really died is at once and here is a moment later.
func prepare(ctx context.Context, dir string) (cache.Layout, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		layout, err := cache.Prepare(ctx, dir)
		if !errors.Is(err, cache.ErrCacheInUse) || time.Now().After(deadline) {
			return layout, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// boot is the Tier A every git node's guard reads: a token per person, and
// one bound to nobody.
func (f *solo) boot() *config.Bootstrap {
	b := &config.Bootstrap{}
	ids := make([]string, 0, len(f.tokens))
	for person := range f.tokens {
		ids = append(ids, person)
	}
	slices.Sort(ids)
	for _, person := range ids {
		id := person
		if id == "" {
			id = "ci"
		}
		b.API.Auth.Tokens = append(b.API.Auth.Tokens, config.APIToken{ID: id, Token: f.tokens[person]})
	}
	return b
}

// person is the human seat a credential is bound to: every person's own, and
// none for ci's.
func (f *solo) person(operatorID string) string {
	if _, ok := f.tokens[operatorID]; ok && operatorID != "" && operatorID != "ci" {
		return operatorID
	}
	return ""
}

// record keeps every push record a host published.
func (f *solo) record(_ context.Context, e types.OperatorActed) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audited = append(f.audited, e)
}

// retention is code.deleted_retention_days: seven days.
func (f *solo) retention() time.Duration { return 7 * 24 * time.Hour }

func (f *solo) proc() *process {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func (f *solo) Nodes() []Node {
	p := f.proc()
	out := make([]Node, len(p.nodes))
	for i, n := range p.nodes {
		out[i] = n
	}
	return out
}

func (f *solo) Token(_ testing.TB, person string) string { return f.tokens[person] }

func (f *solo) Faults() *codefault.Script { return f.o.Faults }

func (f *solo) Objects() objstore.Backend { return f.o.Objects }

func (f *solo) DataNodes() []string { return []string{soloDataNode} }

func (f *solo) Repos(ctx context.Context) ([]string, error) {
	var names []string
	q := code.ReadQuery{Kind: code.ReadRepos, Limit: 500, Fresh: Linearizable}
	for {
		a, err := f.proc().reader.Read(ctx, q)
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

func (f *solo) Rows(ctx context.Context, name string, fresh statelog.Freshness) (Rows, error) {
	p := f.proc()
	repo, err := p.reader.Resolve(ctx, code.ResolveQuery{Name: name, Fresh: fresh})
	if err != nil {
		return Rows{}, err
	}
	return snapshotRows(ctx, repo, fresh, p.reader.Snapshot)
}

func (f *solo) Audited() []types.OperatorActed {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.audited)
}

// activate raises the fence to the fleet's next activation instant: monotone,
// so EnsureRepo never meets an older one.
func (f *solo) activate(t testing.TB) code.Activation {
	t.Helper()
	f.mu.Lock()
	f.activations++
	at := f.base.Add(time.Duration(f.activations) * time.Second)
	f.mu.Unlock()
	a, err := f.proc().lifecycle.Activate(f.t.Context(), at)
	if err != nil {
		t.Fatalf("codetest: activate: %v", err)
	}
	return a
}

func (f *solo) Declare(t testing.TB, repos ...code.Declared) {
	t.Helper()
	a := f.activate(t)
	for _, d := range repos {
		if _, err := f.proc().lifecycle.EnsureRepo(f.t.Context(), a, d); err != nil {
			t.Fatalf("codetest: declare %s: %v", d.Name, err)
		}
	}
}

func (f *solo) Remove(t testing.TB, name string) {
	t.Helper()
	p := f.proc()
	ref, err := p.reader.Resolve(f.t.Context(), code.ResolveQuery{Name: name,
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil {
		t.Fatalf("codetest: resolve %s: %v", name, err)
	}
	if _, err := p.lifecycle.DeleteRepo(f.t.Context(), f.activate(t), ref.ID); err != nil {
		t.Fatalf("codetest: remove %s: %v", name, err)
	}
}

func (f *solo) Purge(t testing.TB) {
	t.Helper()
	p := f.proc()
	r := &code.Retention{Store: p.lifecycle, Reader: p.reader,
		Retention: func() time.Duration { return time.Nanosecond }}
	for range 64 {
		report, err := r.Pass(f.t.Context())
		if err != nil {
			t.Fatalf("codetest: a retention pass: %v", err)
		}
		if !report.Remain && len(report.Unhanded) == 0 && len(report.Failed) == 0 {
			return
		}
	}
	t.Fatal("codetest: sixty-four retention passes left a purge unfinished")
}

func (f *solo) Collect(t testing.TB) collect.CollectionReport {
	t.Helper()
	sources, err := collect.Sources([]objstore.ReferenceTable{code.PackReferences},
		code.ObjectEstate{Reader: f.proc().reader})
	if err != nil {
		t.Fatalf("codetest: the collector's sources: %v", err)
	}
	c, err := collect.New(collect.Options{Store: f.objects, References: sources,
		Now: func() time.Time { return time.Now().Add(25 * time.Hour) }})
	if err != nil {
		t.Fatalf("codetest: a collector: %v", err)
	}
	report, err := c.Collect(f.t.Context())
	if err != nil {
		t.Fatalf("codetest: a collection: %v", err)
	}
	return report
}

func (f *solo) Evict(t testing.TB, node string) {
	t.Helper()
	f.gate(t, node, (*code.Store).EvictNode, "evict")
}

func (f *solo) Readmit(t testing.TB, node string) {
	t.Helper()
	f.gate(t, node, (*code.Store).ReadmitNode, "readmit")
}

// gate publishes an operator's eviction or readmission of the data node.
func (f *solo) gate(t testing.TB, node string,
	op func(*code.Store, context.Context, code.Actor, string, string) (statelog.Result, error), name string) {

	t.Helper()
	if node != soloDataNode {
		t.Fatalf("codetest: the solo fleet's data node is %q, not %q", soloDataNode, node)
	}
	actor := code.Actor{ID: "ops", Kind: code.ActorOperator, OperatorID: "ops"}
	res, err := op(f.proc().operator, f.t.Context(), actor, statelog.NewOpID(time.Now(), name), node)
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("codetest: %s %s = (%+v, %v), want applied", name, node, res, err)
	}
}

// Kill ends the whole solo process — one data node is one process — as far
// as git and the store see it: every host stopped under an ended context and
// every server's listener and connections closed, the applier halted, the
// store closed, the broker stopped. It WAITS FOR NOTHING A CRASH MAY HOLD: an
// HTTP server's Close and the committer's Stop wait for their held work in
// the teardown, after the crashes are released.
func (f *solo) Kill(t testing.TB, _ string) {
	t.Helper()
	f.mu.Lock()
	p := f.current
	f.current = nil
	f.dead = append(f.dead, p)
	f.mu.Unlock()
	if p == nil {
		t.Fatal("codetest: Kill of a fleet that is not running")
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for _, n := range p.nodes {
		n.srv.Listener.Close()
		n.srv.CloseClientConnections()
		n.host.Stop(ended)
	}
	p.stopApplier()
	p.consumer.Close()
	if err := p.db.Close(); err != nil {
		t.Errorf("codetest: close the killed node's store: %v", err)
	}
	if err := p.q.Stop(context.Background()); err != nil {
		t.Errorf("codetest: stop the killed node's broker: %v", err)
	}
}

// Restart rebuilds the process over the same directories: new listeners, so
// a node's URL is the new one.
func (f *solo) Restart(t testing.TB, _ string) {
	t.Helper()
	if f.proc() != nil {
		t.Fatal("codetest: Restart of a fleet that was not killed")
	}
	f.start(t)
}

func (f *solo) PauseApplier(t testing.TB, _ string) (resume func()) {
	t.Helper()
	p := f.proc()
	p.stopApplier()
	return func() {
		at, _, _, err := statelog.CursorFor(f.t.Context(), p.estate.Reader(), code.Domain{}.Stream().Name)
		if err == nil {
			err = p.consumer.Reset(f.t.Context(), at.Seq)
		}
		if err != nil {
			t.Errorf("codetest: move the consumer back to the checkpoint: %v", err)
		}
		p.startApplier()
	}
}

// startApplier runs the applier until stopApplier.
func (p *process) startApplier() {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- p.runner.Run(ctx) }()
	p.applyStop = sync.OnceValue(func() error {
		cancel()
		err := <-stopped
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
}

// stopApplier halts the applier and waits for it.
func (p *process) stopApplier() {
	p.applyMu.Lock()
	stop := p.applyStop
	p.applyMu.Unlock()
	if stop != nil {
		_ = stop()
	}
}

// health is the data node's readiness as the engine assembles it: the
// applier's position, whether it has drained, the log's two ends and its lag,
// the stream's identity, and a floor a fleet that never trimmed publishes.
func (p *process) health(ctx context.Context) (statelog.Health, error) {
	at := p.runner.Committed()
	h := statelog.Health{Position: at, AppliedThrough: at.Seq, Drained: p.runner.Drained(),
		Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()}}
	if err := p.runner.Stopped(); err != nil && !errors.Is(err, context.Canceled) {
		h.Err = err.Error()
	}
	if deferral, ok := p.runner.Deferred(); ok {
		h.Deferred, h.DeferredFrom = 1, deferral.Position.Seq
		if deferral.Position.Seq > 0 {
			h.AppliedThrough = deferral.Position.Seq - 1
		}
	}
	first, last, err := p.log.Bounds(ctx)
	if err != nil {
		return h, err
	}
	end := max(last, at.Seq)
	lag, trim := uint64(0), uint64(0)
	if end > at.Seq {
		lag = end - at.Seq
	}
	h.FirstSeq, h.LastSeq, h.Lag, h.TrimFloor = &first, &end, &lag, &trim
	identity := p.runner.StreamIdentity()
	h.StreamRecreated = errors.Is(identity, statelog.ErrStreamRecreated)
	h.GenerationPassed = errors.Is(identity, statelog.ErrGenerationPassed)
	h.LogDiverged = p.runner.Diverged()
	return h, nil
}

// teardown ends every incarnation: the crashes released FIRST, before
// anything stops or waits, because whatever waits for work a crash holds —
// a server's Close, a committer's Stop — waits until then.
func (f *solo) teardown() {
	f.o.Faults.ReleaseCrashes()
	f.mu.Lock()
	procs := append(slices.Clone(f.dead), f.current)
	f.current, f.dead = nil, nil
	f.mu.Unlock()
	ctx := context.Background()
	for _, p := range procs {
		if p == nil {
			continue
		}
		for _, n := range p.nodes {
			n.host.Stop(ctx)
			n.srv.Close()
		}
		p.committer.Stop(ctx)
		p.stopApplier()
		p.consumer.Close()
		_ = p.db.Close()
		_ = p.q.Stop(ctx)
	}
}

// soloEstate is the data node's code half as the host and the cache ask it,
// in-process: code.Served, the one adapter the engine hands the router, with
// a committer's refusal about itself answered in the router's shape.
type soloEstate struct{ served code.Served }

func (e *soloEstate) Resolve(ctx context.Context, q code.ResolveQuery) (code.RepoRef, error) {
	return e.served.Resolve(ctx, q)
}

func (e *soloEstate) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	return e.served.Snapshot(ctx, q)
}

// Propose asks the committer. A refusal about the node itself
// (code.ErrNotServing) is answered as the estate router answers a walk in
// which every node it asked refused and nobody went silent: nothing ran the
// push.
func (e *soloEstate) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	res, err := e.served.Propose(ctx, p)
	if errors.Is(err, code.ErrNotServing) {
		return res, &soloUnserved{err: err}
	}
	return res, err
}

func (e *soloEstate) PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error) {
	return e.served.PushOutcome(ctx, q)
}

// soloUnserved is the router's ErrUnserved for the one data node a solo
// fleet has: a walk that ran nothing, in which nobody went silent.
type soloUnserved struct{ err error }

func (u *soloUnserved) Error() string {
	return "estate: no data node serves the estate right now: " + soloDataNode + ": " + u.err.Error()
}
func (u *soloUnserved) Unwrap() error { return u.err }
func (u *soloUnserved) Silent() bool  { return false }

// faultPublisher is the committer's write authority with the push path's
// CommitterAfterAppend point between the append and its result: the durable
// record the receiver never heard about. Hit here rather than in
// internal/code, so the domain imports nothing from codefault.
type faultPublisher struct {
	inner  code.Publisher
	faults *codefault.Script
}

func (p *faultPublisher) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := p.inner.Publish(ctx, req)
	if err == nil {
		if ferr := p.faults.Hit(ctx, codefault.CommitterAfterAppend, 0); ferr != nil {
			return statelog.Result{}, ferr
		}
	}
	return res, err
}
