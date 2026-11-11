package gitletcluster_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
)

// members is how many data nodes the topology has: the fewest that keep a
// quorum with one of them down.
const members = 3

// clusterTiming is how long every member's router waits on a data node.
//
// THE READ ATTEMPT is two seconds against production's ten: a serving node
// fits its waits into whatever attempt it is handed and answers `behind` a
// reply's margin short of it (estate.Timing), so a shorter attempt costs only
// that a member whose applier a case holds says so sooner — which is every read
// F13 sends its paused member.
//
// THE WRITE ATTEMPT is what a push asked of a member that never answers — F1's
// committer, killed after appending — costs before the runner-up is asked, and
// it also bounds how long a push may queue at a committer: the committer
// answers a queued push busy once less than gitlet.CommitBudget and
// gitlet.ReplyMargin are left of its asker's attempt. So it is the shortest the
// router accepts — a floor wait, a commit budget and the reply margin, 33 s —
// and twelve seconds more, the queue F19 builds: its committer's first record
// is held until every one of forty-eight pushes has uploaded, about three
// seconds measured for fifty pushes on four cores under the race detector.
// Production's sixty would cost each of the two backends' F1 and committer
// crash a quarter of a minute more of waiting on a dead member.
//
// THE ADMISSION BUDGET is the read attempt's: no case asks it.
//
// THE GIT READ ATTEMPT is production's (estate.GitReadAttempt): no case wedges
// a Gitlet node for a read, and a member that is killed or stopped costs a git
// read no wait at all — nothing serves its subjects, and the broker's ask ends
// at once.
//
// THE GIT WRITE ATTEMPT is production's (estate.GitWriteAttempt) too, though
// it is what F41's home — killed after its write's proposal, its answer never
// sent — costs before the runner-up is asked, on each backend: a Gitlet node
// uploads a write's bundle only while the upload's allowance and the
// proposal's own attempt are still left (gitlet.WriteUploadAllowance and
// gitlet.MinProposeAttempt, 63 s together, internal/gitlet/host's deadline rule),
// so an attempt short of production's by its sync and build budgets would
// answer a write that synced or built slowly on a loaded runner busy — a case
// failing for the suite's timing rather than Gitlet's.
var clusterTiming = estate.Timing{
	ReadAttempt:     2 * time.Second,
	WriteAttempt:    statelog.ReadBudget + gitlet.CommitBudget + gitlet.ReplyMargin + 12*time.Second,
	AdmissionAsk:    2 * time.Second,
	GitReadAttempt:  estate.GitReadAttempt,
	GitWriteAttempt: estate.GitWriteAttempt,
}

// clusterEnvelopes are every member's estate envelopes ([estate.Envelopes]):
// one fleet, so one keyring, under which each member signs what it asks and
// what it answers.
var clusterEnvelopes = func() *estate.Envelopes {
	env, err := estate.NewEnvelopes(recordseal.OneKey("k1", "the code cluster's fleet key material"))
	if err != nil {
		panic(err)
	}
	return env
}()

// codeLogBytes is the code log's ceiling: room for every case's records on one
// cluster, F19's and F21's included, where each record is small.
const codeLogBytes = 256 << 20

// bucketBytes is the broker bucket's byte reservation, which every bucket
// must hold (natsobj.Config.MaxBytes): a gibibyte, the least
// store.objects.nats_max_bytes takes and far past every pack bundle one
// cluster's cases push, each a few kilobytes, so a push is never refused as
// the bucket filling.
const bucketBytes = 1 << 30

// transfers is each Gitlet node's transfer lane: F19's sixteen pushes through
// one member each hold a slot until they are answered, so a narrower lane
// would hold some of them waiting for a slot rather than queued at their
// committer — and eight more, for the reads a case makes beside them.
const transfers = 16 + 8

// lapse is how far Settle moves every router's clock: past the half minute a
// router asks last a member that refused or went silent, and every window it
// keeps. R7 fails if it no longer is — the readmitted committer is then still
// asked last.
const lapse = time.Hour

// cluster is the topology: three members of one broker cluster, each a data
// node and a Gitlet node, the production router between them.
type cluster struct {
	// life is the test that started the cluster: every member, however
	// often a case restarts it, stops with it.
	life    testing.TB
	brokers *jetstreamtest.Cluster
	members []*member
	creds   *gitlettest.Credentials
	// runs is the cluster's one run store, every member's Gitlet node's.
	runs      *gitlettest.FixtureRuns
	faults    *gitletfault.Script
	placement *placement
	// shared is the S3 bucket every member reads and writes, or nil on the
	// broker's own object store, which each member reaches through its own
	// broker client.
	shared objstore.Backend
	base   time.Time

	// skew is every router's clock's distance from the wall clock (Settle).
	skew atomic.Int64

	// revoked is the revision of the chart a case runs the fleet under
	// (Revoke), nil for the fixture chart with every member current.
	revoked atomic.Pointer[revocation]

	mu          sync.Mutex
	activations int
	audited     []types.OperatorActed
	// split is every repository whose maintainer a case split the members'
	// view of (SplitMaintainers): the members that each take themselves for
	// its maintainer.
	split map[string][]string
	// confined is every repository that existed when the running case
	// began (ConfineMaintenance), which no member maintains until it ends;
	// nil while no case confines its maintenance.
	confined map[string]bool
	// proposed is the last compaction any member's compactor proposed of
	// each repository, by its id: what ProposeCompactionAgain asks again.
	proposed map[string]gitlet.CompactionProposal
}

// member is one member: one process, as far as the cluster is concerned.
type member struct {
	c   *cluster
	i   int
	id  string
	dir string

	data *gitlettest.DataNode
	git  *gitlettest.GitletNode

	// router is this member's estate router; the incarnation's, replaced at
	// every restart, and read by every request its Gitlet node serves.
	router atomic.Pointer[estate.Router]

	// gitAnswered is how many reads by object id this member's git half has
	// answered — its router's in process and its git lane's alike
	// (memberGit): which member a read was served by.
	gitAnswered atomic.Int64
	// waitedOut is how many proposals the members' routers waited out on
	// this member (waitedOutAsker).
	waitedOut atomic.Int64

	mu       sync.Mutex
	q        *js.Queue
	objects  objstore.Backend
	withdraw queue.Unsubscribe
	running  bool
	paused   bool
}

var _ gitlettest.Cluster = (*cluster)(nil)

// startCluster starts the topology over backend: "nats", the broker's own
// object store, one bucket every member reaches through its own client as a
// deployment's do; or "s3", one S3 bucket in process. Its brokers decline the
// fsync per write, which the suite's claims do not turn on and which would
// only slow them.
func startCluster(t testing.TB, backend string) *cluster {
	t.Helper()
	return startClusterOf(t, backend, js.Config{})
}

// startClusterOf is startCluster over brokers of broker's configuration — a
// benchmark's, which measures the fsync per write production makes.
func startClusterOf(t testing.TB, backend string, broker js.Config) *cluster {
	t.Helper()
	c := &cluster{life: t, faults: &gitletfault.Script{}, placement: &placement{},
		creds: gitlettest.NewCredentials(gitlettest.DefaultPeople()),
		runs:  gitlettest.NewFixtureRuns(gitlettest.NewBoxTokens(t)),
		// AN HOUR AGO, so an activation a case mints — a second apart, in
		// order — is never in the future.
		base: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	// QUORUM-DURABLE: a record one member acknowledged is one every member
	// will see.
	broker.Replicas = members
	c.brokers = jetstreamtest.StartCluster(t, members, broker)
	switch backend {
	case "nats":
	case "s3":
		c.shared = s3Bucket(t)
	default:
		t.Fatalf("codecluster: no object store called %q", backend)
	}
	root := t.TempDir()
	for i := range members {
		m := &member{c: c, i: i, id: fmt.Sprintf("m%d", i), dir: filepath.Join(root, fmt.Sprintf("m%d", i))}
		if err := os.MkdirAll(filepath.Join(m.dir, "data"), 0o755); err != nil {
			t.Fatalf("codecluster: %s's directory: %v", m.id, err)
		}
		c.members = append(c.members, m)
	}
	c.placement.set(c.Members())
	for _, m := range c.members {
		m.q = c.brokers.Client(t, m.i)
		m.objects = c.objectsOver(t, m.q)
		m.data = gitlettest.StartDataNode(t, gitlettest.DataOptions{ID: m.id, Queue: m.q,
			Dir: filepath.Join(m.dir, "data"), Faults: c.faults, LogBytes: codeLogBytes,
			SeatRules: m.seatRules, Current: m.current})
		m.route(t)
		m.running = true
	}
	for _, m := range c.members {
		g := gitlettest.StartGitletNode(t, gitlettest.GitOptions{
			ID: m.id, Dir: filepath.Join(m.dir, "git"), Estate: memberEstate{m}, Reads: m.read,
			Compactions: m.compact, Expunges: m.expunge, Maintainers: memberMaintainers{m},
			Objects: m.backend, Faults: c.faults, Credentials: c.creds, Runs: c.runs, Audit: c.record,
			MaxTransfers: transfers, Roster: m.roster, Current: m.current,
		})
		// UNDER THE MEMBER'S LOCK, since its serving registration — made
		// before its Gitlet node — reads it from the broker's goroutines
		// (memberGit).
		m.mu.Lock()
		m.git = g
		m.mu.Unlock()
	}
	if err := c.drained(t.Context()); err != nil {
		t.Fatalf("codecluster: %v", err)
	}
	return c
}

// s3Bucket is a fresh in-process S3 bucket, through the production backend.
func s3Bucket(t testing.TB) objstore.Backend {
	t.Helper()
	srv := s3fake.Start(t, "code")
	b, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "code", Prefix: "acme/",
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err != nil {
		t.Fatalf("codecluster: open the S3 fake: %v", err)
	}
	return b
}

// objectsOver is a member's object store over its broker client q: the shared
// S3 bucket, or the broker's bucket opened through q as a data node of a
// clustered fleet opens it.
func (c *cluster) objectsOver(t testing.TB, q *js.Queue) objstore.Backend {
	t.Helper()
	if c.shared != nil {
		return c.shared
	}
	b, err := natsobj.Open(t.Context(), q.JetStream(), natsobj.Config{MaxBytes: bucketBytes, Replicas: members, Clustered: true})
	if err != nil {
		t.Fatalf("codecluster: open the broker's bucket: %v", err)
	}
	return b
}

// now is every router's clock: the wall clock, moved on by Settle.
func (c *cluster) now() time.Time { return time.Now().Add(time.Duration(c.skew.Load())) }

// record keeps every push record a host published.
func (c *cluster) record(_ context.Context, e types.OperatorActed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audited = append(c.audited, e)
}

// route builds the member's router and makes it answer the fleet on its
// subjects — the engine's estate wiring, over this incarnation's client: every
// member is a Gitlet node too, so its router asks a repository's git reads of
// the members (the same placement) with its own git half in process, and its
// registration serves the git lane over that half.
func (m *member) route(t testing.TB) {
	t.Helper()
	r, err := estate.NewRouter(estate.RouterOptions{Self: m.id, Queue: waitedOutAsker{Asker: m.q, c: m.c},
		Placement: m.c.placement, Git: gitlettest.GitPlacement{Placement: m.c.placement, Node: m.c.gitletNodeOf},
		LocalGit: memberGit{m}, Local: memberCopy{m}, Session: estate.NewSession(), Timing: clusterTiming,
		Envelopes: clusterEnvelopes, Now: m.c.now})
	if err != nil {
		t.Fatalf("codecluster: %s's router: %v", m.id, err)
	}
	m.router.Store(r)
	// ON THE CLUSTER'S CONTEXT: the registration outlives the case that
	// restarted the member.
	withdraw, err := estate.Serve(m.c.life.Context(), m.q, estate.ServerOptions{
		Self: m.id, Local: memberCopy{m}, Git: memberGit{m}, Envelopes: clusterEnvelopes})
	if err != nil {
		t.Fatalf("codecluster: %s serves the estate: %v", m.id, err)
	}
	m.withdraw = withdraw
}

// gitletNodeOf is the Gitlet node of the member id names — nil for an id the
// cluster holds no member for, and for a member whose Gitlet node is not started
// yet — whose presence row the routers' git placement reads what it says its
// cache gives its background work off (gitlettest.GitPlacement), as the
// engine's presence row carries it beside its status: a member not started
// yet is not saying, a stopped one is not listed (placement), and a killed
// one says what its last beat did, as its presence lease runs on.
func (c *cluster) gitletNodeOf(id string) *gitlettest.GitletNode {
	for _, m := range c.members {
		if m.id == id {
			return m.gitletNode()
		}
	}
	return nil
}

// gitletNode is the member's Gitlet node, nil before it is started.
func (m *member) gitletNode() *gitlettest.GitletNode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.git
}

// memberGit is the member's git half as its router and its serving
// registration ask for it (estate.LocalGit): its Gitlet node's query server and
// host, read again at every request — the Gitlet node is started after the
// registration, and a restart builds new ones — its reads counted, so a case
// can tell which member answered a read (member.gitAnswered).
type memberGit struct{ m *member }

func (g memberGit) Git() (estate.GitServer, bool) {
	n := g.m.gitletNode()
	if n == nil {
		return nil, false
	}
	s, ok := n.Git()
	if !ok {
		return nil, false
	}
	return countedGit{m: g.m, inner: s}, true
}

// countedGit is a member's git half, counting every read it answers.
type countedGit struct {
	m     *member
	inner estate.GitServer
}

func (c countedGit) Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	a, err := c.inner.Read(ctx, q)
	if err == nil {
		c.m.gitAnswered.Add(1)
	}
	return a, err
}

// Write is the member's engine write, as its Gitlet node's host builds it: which
// member built a write is its push row's (gitlet.PushRow.Node), so nothing is
// counted here.
func (c countedGit) Write(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	return c.inner.Write(ctx, w)
}

// MergePreview is the member's merge preview, as its Gitlet node's previews
// answer it — a read, and counted as one.
func (c countedGit) MergePreview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	a, err := c.inner.MergePreview(ctx, q)
	if err == nil {
		c.m.gitAnswered.Add(1)
	}
	return a, err
}

// read is a read of Gitlet through the member's router, as its API's
// code reads ask.
func (m *member) read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return m.router.Load().Code().Read(ctx, q)
}

// compact is a compaction proposed through the member's router, as the
// engine's compactor proposes through its node's code seam: to the
// repository's committer, its runner-up when the committer is silent.
func (m *member) compact(ctx context.Context, p gitlet.CompactionProposal) (gitlet.CompactResult, error) {
	m.c.mu.Lock()
	if m.c.proposed == nil {
		m.c.proposed = map[string]gitlet.CompactionProposal{}
	}
	m.c.proposed[p.Compaction.RepoID] = p
	m.c.mu.Unlock()
	return m.router.Load().Code().Compact(ctx, p)
}

// expunge is what the member's compactor proposes an expunge's begin and
// commit to — and a case a person's purge request: the member's own router's
// code seam, as the engine hands its compactor.
func (m *member) expunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	return m.router.Load().Code().Expunge(ctx, p)
}

// memberMaintainers is which repositories the member maintains, as its
// compactor asks once a pass: its own router — the incarnation's, read again
// at every question — but for a repository whose maintainer a case split the
// fleet's view of (SplitMaintainers), of which every member it named takes
// itself for the maintainer; and none, whatever a split says, of the
// repositories that existed when the running case began (ConfineMaintenance).
type memberMaintainers struct{ m *member }

func (h memberMaintainers) MaintainerOf(charges map[string]int64) (map[string]bool, error) {
	out := map[string]bool{}
	rest := map[string]int64{}
	for id, charge := range charges {
		split, ok := h.m.c.splitOf(id)
		switch {
		case h.m.c.confinedOut(id):
			out[id] = false
		case ok:
			out[id] = slices.Contains(split, h.m.id)
		default:
			rest[id] = charge
		}
	}
	maintained, err := h.m.router.Load().MaintainerOf(rest)
	if err != nil {
		return nil, err
	}
	for id, yes := range maintained {
		out[id] = yes
	}
	return out, nil
}

// Listed is whether the member is listed among the Gitlet nodes, by its own
// router.
func (h memberMaintainers) Listed() (bool, error) { return h.m.router.Load().Listed() }

// backend is the member's object store, its incarnation's.
func (m *member) backend() objstore.Backend {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects
}

// memberEstate is what a member's Gitlet node asks: its member's router's code
// seam — the incarnation's, read again at every call.
type memberEstate struct{ m *member }

func (e memberEstate) Resolve(ctx context.Context, q gitlet.ResolveQuery) (gitlet.RepoRef, error) {
	return e.m.router.Load().Code().Resolve(ctx, q)
}

func (e memberEstate) RepoState(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
	return e.m.router.Load().Code().RepoState(ctx, q)
}

func (e memberEstate) Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error) {
	return e.m.router.Load().Code().Propose(ctx, p)
}

func (e memberEstate) PushOutcome(ctx context.Context, q gitlet.OutcomeQuery) (gitlet.PushOutcome, error) {
	return e.m.router.Load().Code().PushOutcome(ctx, q)
}

func (e memberEstate) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return e.m.read(ctx, q)
}

func (e memberEstate) Merge(ctx context.Context, p gitlet.MergeProposal) (gitlet.MergeResult, error) {
	return e.m.router.Load().Code().Merge(ctx, p)
}

func (e memberEstate) Queue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error) {
	return e.m.router.Load().Code().Queue(ctx, p)
}

// memberCopy is the member's own copy of the estate as its router and its
// serving half ask it: the code half its data node serves (gitlet.Served, the
// adapter the engine hands the router), its applier's wait, whether the copy
// answers — the state log's own judgement of its health — and whether the
// member serves the company's configuration (member.current). A held applier
// makes a LAGGING copy, asked last, never a wrong one; a member a case left
// behind a revision (Revoke) is a node behind the configuration, asked last
// and judging no grant.
type memberCopy struct{ m *member }

func (c memberCopy) For(context.Context, string) (estate.Backend, bool) {
	d := c.m.data
	return estate.Backend{
		Code:    d.Served(),
		Reached: d.WaitApplied,
		Answers: func(ctx context.Context) bool {
			h, err := d.Health(ctx)
			if err != nil {
				return false
			}
			ok, _ := h.Answers()
			return ok
		},
		Current: c.m.current,
	}, true
}

// revocation is a revision of the chart a case runs the fleet under
// (cluster.Revoke): seat taken out of the chart of every member that applied
// it, which is every member but behind.
type revocation struct{ seat, behind string }

// roster is the chart the member judges a seat's grant by — its Gitlet node's
// reads and engine writes, its committer's decide and its change writer's: the
// fixture chart, less the seat a revision revoked where the member applied
// that revision.
func (m *member) roster() gitlet.Roster {
	if r := m.c.revoked.Load(); r != nil && r.behind != m.id {
		return gitlettest.FixtureRosterWithout(r.seat)
	}
	return gitlettest.FixtureRoster()
}

// seatRules is the member's chart on the company's clock, as its committer
// judges a seat's push by them and its change writer a seat's write of a
// change's conversation.
func (m *member) seatRules() (gitlet.SeatRules, bool) {
	return gitlet.SeatRules{Roster: m.roster(), Zone: time.UTC}, true
}

// current is whether the member serves the company's configuration, as every
// reader of a node's posture on it reads it — false for the member a case left
// behind a revision (Revoke).
func (m *member) current() bool {
	r := m.c.revoked.Load()
	return r == nil || r.behind != m.id
}

// Revoke takes seat out of every member's chart but behind's, and leaves behind
// confirmed behind the company's configuration (gitlettest.Cluster). restore is
// the fixture chart, every member current, and may be run more than once.
func (c *cluster) Revoke(t testing.TB, seat, behind string) (restore func()) {
	t.Helper()
	c.member(t, behind)
	c.revoked.Store(&revocation{seat: seat, behind: behind})
	return func() { c.revoked.Store(nil) }
}

// placement is what every router routes by: the members whose PRESENCE is
// live, as the engine's watched presence view lists them, and how often a
// router has reported each one unanswered.
//
// A KILLED MEMBER STAYS LISTED, because a process that dies gives nothing back:
// its presence lease runs on until its TTL ends, and until then every peer's
// router asks it, hears nothing — no member serves its subjects — and reports
// it, which is the walk a dead committer costs (F20). A member restarted
// inside that window is the same listing, so a kill leaves the placement
// alone until the restart. A STOPPED member gives its presence back FIRST, as
// the engine's drain does before its teardown begins, and is listed again
// when it is restarted. How the engine's view learns of either is the
// engine's to test; this suite's subject is the router's walk over what the
// view lists.
type placement struct {
	mu         sync.Mutex
	listed     []string
	unanswered map[string]int
}

func (p *placement) Holders() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.listed), nil
}

func (p *placement) Unanswered(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unanswered == nil {
		p.unanswered = map[string]int{}
	}
	p.unanswered[node]++
}

// reported is how often a router has reported node unanswered.
func (p *placement) reported(node string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unanswered[node]
}

func (p *placement) set(nodes []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listed = slices.Clone(nodes)
}

func (p *placement) drop(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listed = slices.DeleteFunc(p.listed, func(n string) bool { return n == node })
}

func (p *placement) add(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Contains(p.listed, node) {
		p.listed = append(p.listed, node)
	}
}

// member is the member called id, failing t when there is none.
func (c *cluster) member(t testing.TB, id string) *member {
	t.Helper()
	for _, m := range c.members {
		if m.id == id {
			return m
		}
	}
	t.Fatalf("codecluster: no member called %q (%v)", id, c.Members())
	return nil
}

// isRunning and isPaused read a member's state.
func (m *member) isRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *member) isPaused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused
}

// writers is every running member whose applier runs: those a write through
// them is answered by, and those a gesture is waited for on.
func (c *cluster) writers() []*member {
	var out []*member
	for _, m := range c.members {
		if m.isRunning() && !m.isPaused() {
			out = append(out, m)
		}
	}
	return out
}

// first is the first running member whose applier runs, but none of not.
func (c *cluster) first(not ...string) (*member, error) {
	for _, m := range c.writers() {
		if !slices.Contains(not, m.id) {
			return m, nil
		}
	}
	return nil, fmt.Errorf("codecluster: no running member but %v", not)
}

// via is first, failing t.
func (c *cluster) via(t testing.TB, not ...string) *member {
	t.Helper()
	m, err := c.first(not...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// level waits until every running member whose applier runs has applied
// everything the code log held when asked.
func (c *cluster) level(ctx context.Context) error {
	var errs []error
	for _, m := range c.writers() {
		errs = append(errs, m.data.Level(ctx))
	}
	return errors.Join(errs...)
}

// mustLevel is level, failing t.
func (c *cluster) mustLevel(t testing.TB) {
	t.Helper()
	if err := c.level(t.Context()); err != nil {
		t.Fatalf("codecluster: the members are not level with the code log: %v", err)
	}
}

// next is the fleet's next activation instant: monotone, so EnsureRepo never
// meets an older one.
func (c *cluster) next() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activations++
	return c.base.Add(time.Duration(c.activations) * time.Second)
}

func (c *cluster) Members() []string {
	ids := make([]string, len(c.members))
	for i, m := range c.members {
		ids[i] = m.id
	}
	return ids
}

func (c *cluster) DataNodes() []string {
	var ids []string
	for _, m := range c.members {
		if m.isRunning() {
			ids = append(ids, m.id)
		}
	}
	return ids
}

func (c *cluster) Nodes() []gitlettest.Node {
	var out []gitlettest.Node
	for _, m := range c.members {
		if !m.isRunning() {
			continue
		}
		if n := m.git.Node(); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// Committer is the router's own rule over the running members: the first of
// the repository's rendezvous order.
func (c *cluster) Committer(repoID string) string {
	order := rendezvous.Order(repoID, c.DataNodes())
	if len(order) == 0 {
		return ""
	}
	return order[0]
}

// Appends is node's committer's count, and zero for a node that is no member.
func (c *cluster) Appends(node string) int {
	for _, m := range c.members {
		if m.id == node {
			return m.data.Appends()
		}
	}
	return 0
}

// Closes is how many graceful stops have closed node's writers, and zero for a
// node that is no member (gitlettest.Cluster).
func (c *cluster) Closes(node string) int {
	for _, m := range c.members {
		if m.id == node {
			return m.data.Closes()
		}
	}
	return 0
}

// RecordWriter is the writer of the code log's record at at, read through the
// first running member (gitlettest.Cluster).
func (c *cluster) RecordWriter(ctx context.Context, at statelog.Position) (string, error) {
	m, err := c.first()
	if err != nil {
		return "", err
	}
	return m.data.Writer(ctx, at)
}

// GitRead is a read of a repository by object id through the first running
// member's router (gitlettest.Fleet).
func (c *cluster) GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	m, err := c.first()
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	return m.router.Load().Code().GitRead(ctx, q)
}

// Write is an engine write through the first running member's router
// (gitlettest.Fleet).
func (c *cluster) Write(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	m, err := c.first()
	if err != nil {
		return gitlet.EngineWriteResult{}, err
	}
	return m.router.Load().Code().GitWrite(ctx, w)
}

// Lifecycle is a lifecycle request through the first running member's router
// (gitlettest.Fleet).
func (c *cluster) Lifecycle(ctx context.Context, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
	m, err := c.first()
	if err != nil {
		return gitlet.LifecycleResult{}, err
	}
	return m.router.Load().Code().Lifecycle(ctx, r)
}

// GitReadThrough is a read of a repository by object id through member node's
// own router (gitlettest.Cluster).
func (c *cluster) GitReadThrough(ctx context.Context, node string, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	for _, m := range c.members {
		if m.id == node && m.isRunning() {
			return m.router.Load().Code().GitRead(ctx, q)
		}
	}
	return gitlet.GitAnswer{}, fmt.Errorf("codecluster: no running member called %q", node)
}

// GitWriteThrough is an engine write through member node's own router
// (gitlettest.Cluster).
func (c *cluster) GitWriteThrough(ctx context.Context, node string, w gitlet.EngineWrite) (gitlet.EngineWriteResult,
	error) {

	for _, m := range c.members {
		if m.id == node && m.isRunning() {
			return m.router.Load().Code().GitWrite(ctx, w)
		}
	}
	return gitlet.EngineWriteResult{}, fmt.Errorf("codecluster: no running member called %q", node)
}

// ChangeThrough is a write of a change's lifecycle asked through member node's
// own router (gitlettest.Cluster).
func (c *cluster) ChangeThrough(ctx context.Context, node string, p gitlet.ChangeProposal) (gitlet.ChangeResult,
	error) {

	for _, m := range c.members {
		if m.id == node && m.isRunning() {
			return m.router.Load().Code().Change(ctx, p)
		}
	}
	return gitlet.ChangeResult{}, fmt.Errorf("codecluster: no running member called %q", node)
}

// ConverseThrough is a write of a change's conversation asked through member
// node's own router (gitlettest.Cluster).
func (c *cluster) ConverseThrough(ctx context.Context, node string, w gitlet.ConversationWrite) (
	gitlet.ConversationResult, error) {

	for _, m := range c.members {
		if m.id == node && m.isRunning() {
			return m.router.Load().Code().Converse(ctx, w)
		}
	}
	return gitlet.ConversationResult{}, fmt.Errorf("codecluster: no running member called %q", node)
}

// gitAnswered is how many reads by object id member node's git half has
// answered.
func (c *cluster) gitAnswered(t testing.TB, node string) int64 {
	t.Helper()
	return c.member(t, node).gitAnswered.Load()
}

func (c *cluster) Unanswered(node string) int { return c.placement.reported(node) }

// WaitedOut is how many proposals the members' routers waited out on node, and
// zero for a node that is no member (gitlettest.Cluster).
func (c *cluster) WaitedOut(node string) int {
	for _, m := range c.members {
		if m.id == node {
			return int(m.waitedOut.Load())
		}
	}
	return 0
}

// waitedOutAsker is a member's router's queue, counting every proposal the
// router waits out on a member (gitlettest.Cluster.WaitedOut): an ask of the
// member's proposal lane whose attempt ends at its DEADLINE rather than by the
// router's cancel once it moved on. Counted as the attempt ends, so a wait is
// counted whoever made it — the broker's for an answer that never came, or the
// router's own after one; a caller that hung up cancels the attempt, and is
// no member's silence.
type waitedOutAsker struct {
	estate.Asker
	c *cluster
}

func (a waitedOutAsker) Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error) {
	for _, m := range a.c.members {
		if subject == estate.ProposalSubject(m.id) {
			context.AfterFunc(ctx, func() {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					m.waitedOut.Add(1)
				}
			})
			break
		}
	}
	return a.Asker.Ask(ctx, subject, request, want)
}

func (c *cluster) Settle(ctx context.Context) error {
	var errs []error
	for _, m := range c.members {
		switch {
		case !m.isRunning():
			errs = append(errs, fmt.Errorf("member %s is down", m.id))
		case m.isPaused():
			errs = append(errs, fmt.Errorf("member %s's applier is held", m.id))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := c.level(ctx); err != nil {
		return err
	}
	if err := c.drained(ctx); err != nil {
		return err
	}
	c.skew.Add(int64(lapse))
	return nil
}

// drained waits until every running member's applier has DRAINED the code log
// once since it started — what makes its copy one that answers requests
// (statelog.Health.Serving) rather than one asked only once no other copy will
// take a request. An applier latches it once the broker says nothing follows
// what it has committed, which a member just restarted or just started has not
// heard yet, and a push to a repository whose committer has not would be
// decided by whichever member happened to be level at that instant — the
// router's rule for a lagging copy, and not the order a case is checking.
func (c *cluster) drained(ctx context.Context) error {
	for _, m := range c.writers() {
		for !m.data.Drained() {
			select {
			case <-ctx.Done():
				return fmt.Errorf("member %s's applier has not drained the code log: %w", m.id, ctx.Err())
			case <-time.After(20 * time.Millisecond): //fixedwait:liveness
			}
		}
	}
	return nil
}

func (c *cluster) Declare(t testing.TB, repos ...gitlet.Declared) {
	t.Helper()
	c.via(t).data.Declare(t, c.next(), repos...)
	c.mustLevel(t)
}

func (c *cluster) Remove(t testing.TB, name string) {
	t.Helper()
	c.via(t).data.Remove(t, c.next(), name)
	c.mustLevel(t)
}

func (c *cluster) Purge(t testing.TB) {
	t.Helper()
	c.via(t).data.Purge(t)
	c.mustLevel(t)
}

func (c *cluster) Collect(t testing.TB) collect.CollectionReport {
	t.Helper()
	m := c.via(t)
	return m.data.Collect(t, m.backend())
}

func (c *cluster) CollectAt(t testing.TB, at time.Time) collect.CollectionReport {
	t.Helper()
	m := c.via(t)
	return m.data.CollectAt(t, m.backend(), at)
}

func (c *cluster) Audit(t testing.TB) collect.AuditReport {
	t.Helper()
	m := c.via(t)
	return m.data.Audit(t, m.backend())
}

func (c *cluster) Retain(t testing.TB, now time.Time) gitlet.RetentionReport {
	t.Helper()
	report := c.via(t).data.Retain(t, now)
	c.mustLevel(t)
	return report
}

// Advertise has member node say its git cache gives its background work
// budget bytes until the case ends (gitlettest.Cluster).
func (c *cluster) Advertise(t testing.TB, node string, budget int64) {
	t.Helper()
	m := c.member(t, node)
	m.mu.Lock()
	g := m.git
	m.mu.Unlock()
	g.Advertise(budget)
	t.Cleanup(func() { g.Advertise(0) })
}

// SplitMaintainers has every member of nodes take itself for repoID's
// maintainer until the case ends (gitlettest.Cluster): a presence view split
// between them, which no lease or epoch arbitrates (ADR-0089).
func (c *cluster) SplitMaintainers(t testing.TB, repoID string, nodes ...string) {
	t.Helper()
	c.mu.Lock()
	if c.split == nil {
		c.split = map[string][]string{}
	}
	c.split[repoID] = slices.Clone(nodes)
	c.mu.Unlock()
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.split, repoID)
	})
}

// splitOf is the members a case split repoID's maintainer between, if it did.
func (c *cluster) splitOf(repoID string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nodes, ok := c.split[repoID]
	return nodes, ok
}

// ConfineMaintenance has no member maintain, until the case ends, any
// repository that exists now — read through the first running member, every
// status (gitlettest.Cluster). A member's compactor asks who maintains a
// repository at every pass, of its compactions, publications and expunges
// alike (memberMaintainers), so the case's passes meet the repositories it
// makes and none an earlier case left on the cluster.
func (c *cluster) ConfineMaintenance(t testing.TB) {
	t.Helper()
	m, err := c.first()
	if err != nil {
		t.Fatalf("codecluster: confine the case's maintenance: %v", err)
	}
	earlier, err := gitlettest.RepoIDsOf(t.Context(), m.read)
	if err != nil {
		t.Fatalf("codecluster: the repositories the case's maintenance leaves out: %v", err)
	}
	c.mu.Lock()
	c.confined = earlier
	c.mu.Unlock()
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.confined = nil
	})
}

// confinedOut reports whether repoID existed when the running case began, and
// so is maintained by no member until it ends (ConfineMaintenance).
func (c *cluster) confinedOut(repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.confined[repoID]
}

func (c *cluster) Token(_ testing.TB, person string) string { return c.creds.Token(person) }

// Runs is the cluster's executors' rows, which every member's Gitlet node
// admits a coding box by.
func (c *cluster) Runs() *gitlettest.FixtureRuns { return c.runs }

// ClientGit is the PATH's git, held to the client floor: a fleet's persons run
// what the one-node suite's do outside its floors' arm.
func (c *cluster) ClientGit() string { return gittest.Require(c.life) }

func (c *cluster) Faults() *gitletfault.Script { return c.faults }

func (c *cluster) Objects() objstore.Backend {
	m, err := c.first()
	if err != nil {
		return nil
	}
	return m.backend()
}

func (c *cluster) Audited() []types.OperatorActed {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.audited)
}

func (c *cluster) Repos(ctx context.Context) ([]string, error) {
	m, err := c.first()
	if err != nil {
		return nil, err
	}
	return gitlettest.ReposOf(ctx, m.read)
}

func (c *cluster) Rows(ctx context.Context, name string, fresh statelog.Freshness) (gitlettest.Rows, error) {
	m, err := c.first()
	if err != nil {
		return gitlettest.Rows{}, err
	}
	seam := m.router.Load().Code()
	return gitlettest.RowsOf(ctx, name, fresh, seam.Resolve, seam.RepoState)
}

func (c *cluster) State(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
	m, err := c.first()
	if err != nil {
		return gitlet.RepoSnapshot{}, err
	}
	return m.router.Load().Code().RepoState(ctx, q)
}

func (c *cluster) Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error) {
	m, err := c.first()
	if err != nil {
		return gitlet.ProposeResult{}, err
	}
	return m.router.Load().Code().Propose(ctx, p)
}

// PushOutcome is what became of one push, read through the first running
// member's router, as Propose asks it (gitlettest.Fleet).
func (c *cluster) PushOutcome(ctx context.Context, q gitlet.OutcomeQuery) (gitlet.PushOutcome, error) {
	m, err := c.first()
	if err != nil {
		return gitlet.PushOutcome{}, err
	}
	return m.router.Load().Code().PushOutcome(ctx, q)
}

// Queued is how many proposals wait in repoID's queue across the members'
// committers — the repository's committer holds them, and a member that
// commits none of it holds none — and none on a killed member, which runs
// no committer (gitlettest.Fleet).
func (c *cluster) Queued(repoID string) int {
	n := 0
	for _, m := range c.members {
		if committer := m.data.Served().Committer; committer != nil {
			n += committer.Queued(repoID)
		}
	}
	return n
}

// Expunge is an expunge proposal asked through the first running member's
// router — of the repository's committer first, its runner-up after — as a
// member's compactor asks its begins and commits (gitlettest.Fleet).
func (c *cluster) Expunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	m, err := c.first()
	if err != nil {
		return gitlet.ExpungeResult{}, err
	}
	return m.router.Load().Code().Expunge(ctx, p)
}

// ProposeCompactionAgain asks member node's router to decide, again, the last
// compaction a member's compactor proposed of repoID — under its own
// compaction id, as a compactor whose answer came back late asks again.
func (c *cluster) ProposeCompactionAgain(ctx context.Context, node, repoID string) (gitlet.CompactResult, error) {
	c.mu.Lock()
	p, ok := c.proposed[repoID]
	c.mu.Unlock()
	if !ok {
		return gitlet.CompactResult{}, fmt.Errorf("codecluster: no compactor has proposed a compaction of %s", repoID)
	}
	return c.member(c.life, node).router.Load().Code().Compact(ctx, p)
}

// StopGit stops member node's Gitlet node alone, gracefully, its data node and
// its router running on.
func (c *cluster) StopGit(t testing.TB, node string) {
	t.Helper()
	c.member(t, node).git.Stop(t.Context())
}

// StartGit starts member node's Gitlet node again over its directory.
func (c *cluster) StartGit(t testing.TB, node string) {
	t.Helper()
	c.member(t, node).git.Restart(t)
}

// Evict and Readmit go through a member OTHER than node — every record an
// evicted member publishes is dropped, its own readmission included — and are
// waited for on every member whose applier runs.
func (c *cluster) Evict(t testing.TB, node string) {
	t.Helper()
	c.waitApplied(t, c.via(t, node).data.Evict(t, node))
}

func (c *cluster) Readmit(t testing.TB, node string) {
	t.Helper()
	c.waitApplied(t, c.via(t, node).data.Readmit(t, node))
}

// waitApplied waits until every member whose applier runs has applied at.
func (c *cluster) waitApplied(t testing.TB, at statelog.Position) {
	t.Helper()
	for _, m := range c.writers() {
		if err := m.data.WaitApplied(t.Context(), at); err != nil {
			t.Fatalf("codecluster: %s applying %s: %v", m.id, at, err)
		}
	}
}

func (c *cluster) PauseApplier(t testing.TB, node string) (resume func()) {
	t.Helper()
	m := c.member(t, node)
	// LEVEL FIRST, so what the member's copy holds while it is held is the
	// whole log as it stood: a case pausing a member it then writes past
	// reads a member exactly that far behind.
	if err := m.data.Level(t.Context()); err != nil {
		t.Fatalf("codecluster: level %s before holding its applier: %v", m.id, err)
	}
	held := m.data.PauseApplier(t)
	m.mu.Lock()
	m.paused = true
	m.mu.Unlock()
	return func() {
		held()
		m.mu.Lock()
		m.paused = false
		m.mu.Unlock()
		if err := m.data.Level(t.Context()); err != nil {
			t.Errorf("codecluster: %s catching up once resumed: %v", m.id, err)
		}
	}
}

// Level waits until node's data node has reached the code log's end (Fleet).
func (c *cluster) Level(ctx context.Context, node string) error {
	for _, m := range c.members {
		if m.id == node {
			return m.data.Level(ctx)
		}
	}
	return fmt.Errorf("codecluster: no member called %q (%v)", node, c.Members())
}

// Kill is the member's process dying: its Gitlet node killed, its broker client
// closed FIRST — a process that dies sends nothing more, so no answer it was
// in the middle of reaches its asker, which hears silence — its estate
// subjects withdrawn, its data node killed, its broker member stopped. It
// STAYS IN THE PLACEMENT: a process that dies gives no presence back
// (placement).
func (c *cluster) Kill(t testing.TB, node string) {
	t.Helper()
	m := c.member(t, node)
	if !m.isRunning() {
		t.Fatalf("codecluster: Kill of %s, which is not running", node)
	}
	c.down(m)
}

// down takes a running member down as Kill describes, leaving the placement
// to its caller.
func (c *cluster) down(m *member) {
	m.mu.Lock()
	m.running = false
	withdraw := m.withdraw
	m.withdraw = nil
	m.mu.Unlock()
	m.git.Kill()
	_ = m.q.Stop(context.Background())
	if withdraw != nil {
		// WAITING FOR NOTHING: an answer still running finishes against a
		// connection that is gone.
		ended, cancel := context.WithCancel(context.Background())
		cancel()
		_ = withdraw(ended)
	}
	m.data.Kill()
	c.brokers.StopMember(m.i)
}

// Stop is a graceful stop in the engine's order (engine.Engine.Stop: the drain,
// then the teardown):
//
//  1. Its presence given back, which the engine's drain begins with: its
//     peers' routers stop listing it.
//  2. Its Gitlet node drained and stopped — the API's listener, which the engine
//     closes before its teardown, finishing what it let through, then the
//     teardown's first step, stopGitletHost — so the member takes no push and
//     appends no sync's barrier once its committer refuses: every push in
//     flight there proposes, and every sync appends, through the estate.
//  3. Its committer refusing — every proposal queued at it answered "not
//     serving", which its asker takes to the repository's runner-up, and the
//     batch in flight finished.
//  4. Its estate subjects no longer answered, the answers in flight waited
//     for. A member that stopped answering before its committer refused
//     would leave every push it held answered with the withdrawal's
//     cancellation, a final failure for a push its batch might yet land.
//  5. The rest as Kill takes it: its broker client, its data node and its
//     broker member.
func (c *cluster) Stop(t testing.TB, node string) {
	t.Helper()
	m := c.member(t, node)
	if !m.isRunning() {
		t.Errorf("codecluster: Stop of %s, which is not running", node)
		return
	}
	c.placement.drop(m.id)
	// THE DRAIN HAS NO BOUND OF ITS OWN, as the engine's has none
	// (api.App.DrainGit): what bounds it is the test binary's -timeout,
	// this process's supervisor.
	m.git.Stop(c.life.Context())
	m.data.StopCommitter()
	m.mu.Lock()
	withdraw := m.withdraw
	m.withdraw = nil
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), statelog.DefaultResolveBudget)
	defer cancel()
	if err := withdraw(ctx); err != nil {
		t.Errorf("codecluster: %s stops answering on its estate subjects: %v", node, err)
	}
	c.down(m)
}

// Restart is what the member's process comes back as: its broker member over
// its own store and route port, its data node over its own files and the new
// client, a new router and serving registration, and its Gitlet node over its own
// cache directory, on a new listener — and its presence, live again (a killed
// member's never lapsed).
func (c *cluster) Restart(t testing.TB, node string) {
	t.Helper()
	m := c.member(t, node)
	if m.isRunning() {
		t.Fatalf("codecluster: Restart of %s, which was not stopped", node)
	}
	q := c.brokers.RestartMember(t, m.i)
	objects := c.objectsOver(t, q)
	m.mu.Lock()
	m.q, m.objects = q, objects
	m.mu.Unlock()
	m.data.Restart(t, q)
	m.route(t)
	m.git.Restart(t)
	m.mu.Lock()
	m.running = true
	m.mu.Unlock()
	c.placement.add(m.id)
}
