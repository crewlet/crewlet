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
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/estate"
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
	// GitRead is a read of a repository by object id, answered by THIS
	// node's own query server over its cache, as the query names it — its
	// Home stamp included: a node holding no copy fills one only for a read
	// stamped its walk's home (code.GitQuery.Home), so a case asking one
	// node directly as the walk asks its first stamps it. [Fleet.GitRead] is
	// the read through the fleet's routing.
	GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
	// Share is the node's seat git-work share, which its query server's
	// reads take their units of.
	Share() *code.GitShare
	// Compactor is the node's compactor, built as the engine builds a git
	// node's and never started: a case drives its Compact or its Poll, its
	// Publish or its PublishPass.
	Compactor() *compact.Compactor
	// Maintainers is what the node's compactor asks of whether it is a
	// repository's maintainer (GitOptions.Maintainers): which node a case's
	// publication is driven on.
	Maintainers() compact.Maintainers
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
	// CollectAt runs one collection over the code estate with the
	// collector's clock at at — every table the code domain declares
	// shielding what its rows name.
	CollectAt(t testing.TB, at time.Time) collect.CollectionReport
	// Audit runs the collector's audit over the code estate — what raises
	// objects_missing — handed every table the code domain declares.
	Audit(t testing.TB) collect.AuditReport
	// Retain runs ONE code-retention pass with its clock at now — what a
	// retired pack's grace and a deletion's age are judged by — so a case
	// moves a grace without moving the broker.
	Retain(t testing.TB, now time.Time) code.RetentionReport
	// Token is a token bound to the human seat person; "" is one bound to
	// nobody.
	Token(t testing.TB, person string) string
	// ClientGit is the git every person a case makes runs ([NewPerson]): the
	// PATH's, held to the client floor, unless the fleet names another (the
	// floors' arm names the client floor's own build).
	ClientGit() string
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
	// State is one page of a repository's snapshot, read through the data
	// nodes as a git node's cache reads one — a delta's retired phase among
	// what it serves, which Rows does not carry.
	State(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error)
	// Propose is a push proposed to the repository's committer through the
	// data nodes, as a git node's host proposes one: for a case that names
	// the packs of a fixture it built and stored itself, a person's git
	// paying for none of them.
	Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error)
	// StopGit stops one git node as the engine's graceful stop does, its
	// cache's directory kept, while the rest of the fleet runs on; StartGit
	// starts it again over that directory.
	StopGit(t testing.TB, node string)
	StartGit(t testing.TB, node string)
	// GitRead is a read of a repository by object id through the fleet's
	// routing, as a seat's tool asks one: the production estate router,
	// asking the git nodes — each serving its git lane on the broker, as the
	// engine serves one — in the repository's rendezvous order, its home
	// first (ADR-0059), and stamping the walk's first attempt home itself.
	GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
	// Write is an engine write (ADR-0062) through the fleet's routing, as a
	// seat's or a person's tool asks one: the production estate router
	// asking the git nodes — each serving its git writes' lane on the
	// broker, as the engine serves one — in the repository's rendezvous
	// order, its home first (ADR-0059), the walk stamping its first attempt
	// home itself (code.EngineWrite.Home) and asking the next git node the
	// same write where one ran nothing or went silent (code.git_write).
	Write(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error)
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
	// WrapCompactions wraps what every git node's compactor proposes its
	// compactions and publications through — the window a case has onto what
	// a maintainer asks a committer to decide: a lying publisher's.
	WrapCompactions func(Compactions) Compactions
}

// Compactions is what a git node's compactor proposes a compaction or a
// publication through: the router's code seam's Compact, in the engine.
type Compactions func(context.Context, code.CompactionProposal) (code.CompactResult, error)

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
	// People are the human seats with a token each; nil is DefaultPeople.
	People []string
	// GitPath is the git every pool runs; "" is the one on PATH, held to the
	// node floor (gittest.RequireNode). A case that records what git ran
	// sets gittest.Recording's, and the floors' arm gittest.NodeFloor's.
	GitPath string
	// ClientGit is the git every person a case makes runs ([NewPerson]);
	// "" is the one on PATH, held to the client floor (gittest.Require). The
	// floors' arm sets gittest.ClientFloor's.
	ClientGit string
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
	// Roster, Current and QueryPool are every git node's query server's
	// (GitOptions).
	Roster    func() code.Roster
	Current   func() bool
	QueryPool func(query.Pool) query.Pool
	// WrapReads wraps every git node's reads of the code host (GitOptions'
	// Reads) — its cache's backfill's pages of pushes among them: the window
	// a case has onto them, to fail or count them; nil reads the data node.
	WrapReads func(Reads) Reads
	// LogBytes is the code log's ceiling; 0 is soloLogBytes. A benchmark
	// whose history outgrows what that ceiling admits beside its gate
	// reserve — about 7 MB of records, the reserve the larger of a sixteenth
	// and the fleet term (statelog.Reservation) — raises it.
	LogBytes int64
	// WrapApplier wraps the data node's applier (DataOptions.WrapApplier).
	WrapApplier func(statelog.Applier) statelog.Applier
	// WrapObjects wraps the object store's backend as the git node id reads
	// and writes it, nil for none: a benchmark's window onto what one git
	// node downloads, apart from what every other does through the same
	// store.
	WrapObjects func(id string, b objstore.Backend) objstore.Backend
}

// Reads is a git node's read of the code host's rows (GitOptions.Reads).
type Reads = func(context.Context, code.ReadQuery) (code.ReadAnswer, error)

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
		o.People = DefaultPeople()
	}
	if o.LogBytes == 0 {
		o.LogBytes = soloLogBytes
	}
	if o.ClientGit == "" {
		o.ClientGit = gittest.Require(t)
	}
	f := &SoloFleet{o: o, dir: t.TempDir(), creds: NewCredentials(o.People), life: t,
		// AN HOUR AGO, so an activation a case mints — a second apart, in
		// order — is never in the future: a deletion is dated by its
		// activation, and the purge judges its age on the wall clock.
		base: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	// REGISTERED FIRST, SO IT RUNS LAST: every node's own teardown stops it
	// before its broker does.
	t.Cleanup(f.teardown)
	f.data = StartDataNode(t, DataOptions{ID: soloDataNode, Queue: f.openBroker(t), Dir: f.dir,
		Limits: o.Limits, Faults: o.Faults, LogBytes: o.LogBytes, Operator: soloOperatorNode,
		WrapApplier: o.WrapApplier})
	var seam host.Estate = &soloEstate{data: f.data}
	if o.WrapEstate != nil {
		seam = o.WrapEstate(seam)
	}
	var reads Reads = func(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
		return f.data.Served().Read(ctx, q)
	}
	if o.WrapReads != nil {
		reads = o.WrapReads(reads)
	}
	var compactions Compactions = f.compact
	if o.WrapCompactions != nil {
		compactions = o.WrapCompactions(compactions)
	}
	f.routers = make([]atomic.Pointer[estate.Router], o.GitNodes)
	for i := range o.GitNodes {
		id := fmt.Sprintf("solo-%c", 'a'+i)
		objects := o.Objects
		if o.WrapObjects != nil {
			objects = o.WrapObjects(id, objects)
		}
		f.git = append(f.git, StartGitNode(t, GitOptions{
			ID: id, Dir: filepath.Join(f.dir, id, "git"), Estate: seam, Reads: reads,
			Compactions: compactions, Maintainers: soloMaintainers{f: f, i: i},
			Objects: func() objstore.Backend { return objects }, Limits: o.Limits, Timing: o.Timing,
			Faults: o.Faults, Credentials: f.creds, Audit: f.record, Fetched: f.recordFetched,
			MaxTransfers: o.MaxTransfers, MaxProcesses: o.MaxProcesses, CacheMaxBytes: o.CacheMaxBytes,
			GitPath: o.GitPath, Logger: o.Logger, Roster: o.Roster, Current: o.Current, QueryPool: o.QueryPool,
		}))
	}
	f.route(t)
	return f
}

// soloEnvelopes are the estate envelopes every git node of a solo fleet signs
// and verifies under, and its router: one fleet, one keyring, as every node's
// estate envelopes are (estate.Envelopes).
var soloEnvelopes = func() *estate.Envelopes {
	env, err := estate.NewEnvelopes(fleetRing)
	if err != nil {
		panic(err)
	}
	return env
}()

// soloRouter is the id the fleet's router asks as: a node holding nothing,
// as a seats-only node's router is.
const soloRouter = "solo-router"

// route makes every git node answer the fleet on the running broker as the
// engine's serveEstate makes a git node answer — its estate service, its two
// git lanes over its own git half (GitNode.Git) among its lanes — and builds
// the router [SoloFleet.GitRead] and [SoloFleet.Write] ask through, holding
// nothing of its own. Every git node holds the estate in this release, and a
// solo fleet's git nodes reach its one data node's copy in process, so each
// serves that copy (soloCopy). A Kill takes every registration with the
// broker; a Restart routes again over the new one.
func (f *SoloFleet) route(t testing.TB) {
	t.Helper()
	q := f.broker()
	ids := make([]string, len(f.git))
	for i, g := range f.git {
		ids[i] = g.o.ID
		// ON THE FLEET'S BROKER, whose stop ends the registration: a
		// process's death takes its subjects with it.
		if _, err := estate.Serve(f.life.Context(), q, estate.ServerOptions{Self: g.o.ID,
			Local: soloCopy{data: f.data}, Git: g, Envelopes: soloEnvelopes}); err != nil {
			t.Fatalf("codetest: %s serves the estate: %v", g.o.ID, err)
		}
	}
	nodes := soloPlacement(ids)
	git := GitPlacement{Placement: nodes, Node: func(id string) *GitNode {
		for _, g := range f.git {
			if g.o.ID == id {
				return g
			}
		}
		return nil
	}}
	r, err := estate.NewRouter(estate.RouterOptions{Self: soloRouter, Queue: q, Placement: nodes, Git: git,
		Session: estate.NewSession(), Timing: estate.Production(), Envelopes: soloEnvelopes})
	if err != nil {
		t.Fatalf("codetest: the fleet's router: %v", err)
	}
	f.router.Store(r)
	// EVERY GIT NODE'S OWN ROUTER, which its compactor asks whether it is a
	// repository's maintainer, as the engine hands each node's compactor the
	// node's router: over the same git placement, which lists every git node
	// with what each advertises, so a killed node — which the solo fleet's
	// placement still lists — keeps its place as its last beat left it.
	for i, g := range f.git {
		own, err := estate.NewRouter(estate.RouterOptions{Self: g.o.ID, Queue: q, Placement: nodes, Git: git,
			LocalGit: g, Session: estate.NewSession(), Timing: estate.Production(), Envelopes: soloEnvelopes})
		if err != nil {
			t.Fatalf("codetest: %s's router: %v", g.o.ID, err)
		}
		f.routers[i].Store(own)
	}
}

// soloMaintainers is which repositories a solo fleet's i-th git node
// maintains, as its compactor asks it once a pass: that node's own router,
// read again at every question, so a restarted fleet's is the one asked — and
// none while the fleet is killed, which is an error rather than an answer.
type soloMaintainers struct {
	f *SoloFleet
	i int
}

func (m soloMaintainers) MaintainerOf(charges map[string]int64) (map[string]bool, error) {
	r := m.f.routers[m.i].Load()
	if r == nil {
		return nil, errors.New("codetest: the solo fleet routes nothing while it is killed")
	}
	return r.MaintainerOf(charges)
}

// Listed is whether the solo fleet's i-th git node is listed among the git
// nodes, by that node's own router — an error while the fleet is killed.
func (m soloMaintainers) Listed() (bool, error) {
	r := m.f.routers[m.i].Load()
	if r == nil {
		return false, errors.New("codetest: the solo fleet routes nothing while it is killed")
	}
	return r.Listed()
}

// compact is what every git node's compactor proposes a compaction to: the
// data node's committer, in process, read again at every call — a refusal
// about the node itself (code.ErrNotServing) answered as the router answers a
// walk in which every node it asked refused, as Propose is.
func (f *SoloFleet) compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	res, err := f.data.Served().ProposeCompaction(ctx, p)
	if errors.Is(err, code.ErrNotServing) {
		return res, &soloUnserved{err: err}
	}
	return res, err
}

// DefaultPeople are the human seats every topology mints a token for unless a
// case names its own: ceo and dev, who push in the cases, and the fixture
// chart's pat, whose engine writes are a person's.
func DefaultPeople() []string { return []string{"ceo", "dev", FixturePat} }

// Write is an engine write through the fleet's routing (Fleet): the router
// [SoloFleet.GitRead] asks through, holding nothing of its own.
func (f *SoloFleet) Write(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	r := f.router.Load()
	if r == nil {
		return code.EngineWriteResult{}, errors.New("codetest: the solo fleet routes nothing while it is killed")
	}
	return r.Code().GitWrite(ctx, w)
}

// GitRead is a read of a repository by object id through the fleet's routing
// (Fleet).
func (f *SoloFleet) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	r := f.router.Load()
	if r == nil {
		return code.GitAnswer{}, errors.New("codetest: the solo fleet routes nothing while it is killed")
	}
	return r.Code().GitRead(ctx, q)
}

// soloPlacement is a solo fleet's git nodes as its router's placements read
// them: every one live for as long as the fleet runs, since a solo fleet's
// nodes live and die together.
type soloPlacement []string

func (p soloPlacement) Holders() ([]string, error) { return slices.Clone(p), nil }
func (p soloPlacement) Unanswered(string)          {}

// soloCopy is the data node's copy as a git node's estate service answers from
// it: its code half (code.Served), its applier's wait, and whether the copy
// answers — the state log's own judgement — read again at every request, so a
// restarted data node's is the one served.
type soloCopy struct{ data *DataNode }

func (c soloCopy) For(context.Context, string) (estate.Backend, bool) {
	d := c.data
	return estate.Backend{
		Code:      d.Served(),
		Committed: d.WaitApplied,
		Answers: func(ctx context.Context) bool {
			h, err := d.Health(ctx)
			if err != nil {
				return false
			}
			ok, _ := h.Answers()
			return ok
		},
	}, true
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
	// life is the test that started the fleet: what its git nodes' estate
	// services are registered under, however often a case restarts it.
	life testing.TB

	data *DataNode
	git  []*GitNode

	// router is the running broker's router, holding nothing of its own,
	// which [SoloFleet.GitRead] and [SoloFleet.Write] ask through (route);
	// nil while killed.
	router atomic.Pointer[estate.Router]
	// routers is each git node's own router, which its compactor asks
	// whether it is a repository's maintainer (soloMaintainers); nil while
	// killed.
	routers []atomic.Pointer[estate.Router]

	mu sync.Mutex
	// brokers is every incarnation's embedded broker, the running one last.
	brokers     []*js.Queue
	activations int
	audited     []types.OperatorActed
	fetched     []types.CodeFetched
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

// recordFetched keeps every read of a unit repository a host or a query server
// recorded.
func (f *SoloFleet) recordFetched(_ context.Context, e types.CodeFetched) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, e)
}

// Fetched is every read of a `visibility: unit` repository the fleet's hosts
// and query servers recorded, in order.
func (f *SoloFleet) Fetched() []types.CodeFetched {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fetched)
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

// ClientGit is the git every person a case makes on the fleet runs (Fleet).
func (f *SoloFleet) ClientGit() string { return f.o.ClientGit }

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
	q := code.ReadQuery{Asker: Reader, Kind: code.ReadRepos, Limit: 500, Fresh: Linearizable}
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

// State is one page of a repository's snapshot, from the data node (Fleet).
func (f *SoloFleet) State(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	return f.data.Served().Snapshot(ctx, q)
}

// Propose is a push proposed to the data node's committer, a refusal about the
// node itself answered as the router answers it (Fleet).
func (f *SoloFleet) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	return (&soloEstate{data: f.data}).Propose(ctx, p)
}

// StopGit stops the git node called node gracefully (Fleet).
func (f *SoloFleet) StopGit(t testing.TB, node string) {
	t.Helper()
	f.gitNamed(t, node).Stop(t.Context())
}

// StartGit starts the git node called node again over its directory (Fleet).
func (f *SoloFleet) StartGit(t testing.TB, node string) {
	t.Helper()
	f.gitNamed(t, node).Restart(t)
}

// gitNamed is the fleet's git node called node.
func (f *SoloFleet) gitNamed(t testing.TB, node string) *GitNode {
	t.Helper()
	for _, g := range f.git {
		if g.o.ID == node {
			return g
		}
	}
	t.Fatalf("codetest: the solo fleet has no git node %q", node)
	return nil
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

// CollectAt runs one collection over the code estate at at (Fleet).
func (f *SoloFleet) CollectAt(t testing.TB, at time.Time) collect.CollectionReport {
	t.Helper()
	return f.data.CollectAt(t, f.o.Objects, at)
}

// Audit runs the collector's audit over the code estate (Fleet).
func (f *SoloFleet) Audit(t testing.TB) collect.AuditReport {
	t.Helper()
	return f.data.Audit(t, f.o.Objects)
}

// Retain runs one code-retention pass at now (Fleet).
func (f *SoloFleet) Retain(t testing.TB, now time.Time) code.RetentionReport {
	t.Helper()
	return f.data.Retain(t, now)
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
	f.router.Store(nil)
	for i := range f.routers {
		f.routers[i].Store(nil)
	}
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
	f.route(t)
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
