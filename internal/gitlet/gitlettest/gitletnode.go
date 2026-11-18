package gitlettest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Credentials are a topology's people: a token for every human seat and one
// bound to nobody, the Tier A guard every Gitlet node's server admits them by,
// and each token's seat, bound as the engine's guard binds a Tier A token
// (auth.SeatBindings).
type Credentials struct {
	tokens map[string]string // person → token; "" bound to nobody
}

// NewCredentials mints a token for each of people and one bound to nobody.
func NewCredentials(people []string) *Credentials {
	c := &Credentials{tokens: map[string]string{"": newToken()}}
	for _, p := range people {
		c.tokens[p] = newToken()
	}
	return c
}

// newToken is a credential's random value.
func newToken() string {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// Token is the token bound to the human seat person; "" is the one bound to
// nobody.
func (c *Credentials) Token(person string) string { return c.tokens[person] }

// Guard is the Tier A every Gitlet node's server reads: `api.auth.tokens`, one per
// person under the person's name, and `ci` bound to nobody.
func (c *Credentials) Guard() *config.Bootstrap {
	b := &config.Bootstrap{}
	ids := make([]string, 0, len(c.tokens))
	for person := range c.tokens {
		ids = append(ids, person)
	}
	slices.Sort(ids)
	for _, person := range ids {
		id := person
		if id == "" {
			id = "ci"
		}
		b.API.Auth.Tokens = append(b.API.Auth.Tokens, config.APIToken{ID: id, Token: c.tokens[person]})
	}
	return b
}

// OperatorID is the credential person's token pushes through, as every record
// names it (iam.ActorFor): the Tier A token's login, `token:<id>` — ci's for
// the token bound to nobody.
func (c *Credentials) OperatorID(person string) string {
	if person == "" {
		return iam.TokenLogin("ci")
	}
	return iam.TokenLogin(person)
}

// guard is the API's guard over [Credentials.Guard], each person's token bound
// to their own seat and ci's to none.
func (c *Credentials) guard() *auth.Guard {
	return auth.New(c.Guard()).BindSeats(auth.SeatBindings{Directory: c, Chart: c})
}

// BoundSeat is the identity directory's row under a token's login
// (auth.BindingDirectory): a person's own seat, and none for ci's.
func (c *Credentials) BoundSeat(_ context.Context, login string) (session.PersonRow, error) {
	for person := range c.tokens {
		if person != "" && login == iam.TokenLogin(person) {
			return session.PersonRow{Found: true, Stage: iam.StageActive, Login: login, Seat: person}, nil
		}
	}
	return session.PersonRow{}, nil
}

// Seat is the running org a binding is resolved in (session.Chart): every
// person's seat, as a human one.
func (c *Credentials) Seat(_ context.Context, handle string) (session.Seat, bool, error) {
	if _, ok := c.tokens[handle]; !ok || handle == "" {
		return session.Seat{}, false, nil
	}
	return session.Seat{Handle: handle, Kind: session.SeatKindHuman}, true, nil
}

// defaultCacheMaxBytes is a Gitlet node's cache budget unless a topology sets
// one: a gibibyte holds every repository the suite pushes, so no case is
// evicted by a budget it did not mean to test.
const defaultCacheMaxBytes = 1 << 30

// GitOptions configure a Gitlet node.
type GitOptions struct {
	// ID is the node's id.
	ID string
	// Dir is the cache's directory, the pool's HOME beneath it.
	Dir string
	// Estate is what the node's host and cache ask. It resolves the serving
	// data node at every call, so no restart hands it a dead one.
	Estate host.Estate
	// Reads answers Node.Read and the compactor's due listings, through the
	// seam the node's routes read Gitlet by.
	Reads func(context.Context, gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	// Compactions is where the node's compactor proposes a compaction: the
	// repository's committer, resolved at every call (the router's code
	// seam's Compact, in the engine).
	Compactions func(context.Context, gitlet.CompactionProposal) (gitlet.CompactResult, error)
	// Expunges is where it proposes an expunge's begin and commit, as
	// Compactions (the code seam's Expunge, in the engine).
	Expunges func(context.Context, gitlet.ExpungeProposal) (gitlet.ExpungeResult, error)
	// Maintainers is which repositories the node maintains, asked once a
	// pass: the node's own router, as in the engine, or a case's fixed answer
	// (SplitMaintainers); on a fleet, none an earlier case made
	// (ConfineMaintenance).
	Maintainers compact.Maintainers
	// Objects is the object store's backend, asked again at every start: a
	// backend riding a broker connection dies with a member's restart.
	Objects func() objstore.Backend
	// Limits are code.limits; a zero field takes the default.
	Limits gitlet.Limits
	// Timing is the host's; the zero value is host.Production with a
	// PendingGrace of two seconds and a Keepalive of 200 ms.
	Timing host.Timing
	// Faults is the fleet's script; nil is a fresh one.
	Faults *gitletfault.Script
	// Credentials are who the node's guard admits, and the seat each is.
	Credentials *Credentials
	// Runs is the executors' rows the host admits a coding box by: ONE store
	// for every Gitlet node of a fleet, as every engine node reads the
	// coordination store's; nil is the node's own (NewFixtureRuns).
	Runs host.Runs
	// BoxTokens verifies a box's run token over the fleet's keyring; nil is
	// the node's own (NewBoxTokens), as every node builds its own.
	BoxTokens *host.BoxTokens
	// CheckTokens verifies a check's box's token over the fleet's keyring;
	// nil is the node's own (NewCheckTokens).
	CheckTokens *host.CheckTokens
	// Audit is handed every push record the host publishes; nil keeps none.
	Audit func(context.Context, types.OperatorActed)
	// Fetched is handed every read of a `visibility: unit` repository the
	// host and the query server record; nil keeps none.
	Fetched func(context.Context, types.CodeFetched)
	// MaxTransfers and MaxProcesses are the pool's transfer and read lanes;
	// 0 is the pool's default.
	MaxTransfers, MaxProcesses int
	// CacheMaxBytes is the cache's budget; 0 is defaultCacheMaxBytes.
	CacheMaxBytes int64
	// GitPath is the git the pool runs; "" is the one on PATH, held to the
	// node floor (gittest.RequireNode).
	GitPath string
	// Logger is the host's; nil is the package's own.
	Logger *slog.Logger
	// Roster is the chart a seat's reads and engine writes are judged by; nil
	// is FixtureRoster. One answering nil stands in a node holding no chart,
	// which answers a seat unready (no_chart).
	Roster func() gitlet.Roster
	// Signer signs the node's engine-built commits; nil is FixtureSigner.
	Signer host.Signer
	// Current is whether the node serves the company's configuration; nil is
	// current.
	Current func() bool
	// QueryPool stands between the query server and the node's pool, to hold
	// or record a read's processes; nil is the pool itself.
	QueryPool func(query.Pool) query.Pool
	// Fleet is how many live git and data nodes the cache's backfill paces
	// its reads by (cache.Options.Fleet); nil is one of each.
	Fleet cache.Fleet
	// ResyncClock is the clock of the node's ONE set of re-sync buckets
	// (gitlet.NewResyncs), shared by its host and query server and read by
	// every incarnation, so a case can move a refill by hand; nil is the
	// wall clock.
	ResyncClock func() time.Time
}

// rowsOf is a Gitlet node's cache's rows (cache.Fetcher): snapshots through
// the host's estate and reads (the backfill's pages among them) through
// Node.Read's seam, as the engine hands the cache one router for both.
type rowsOf struct {
	host.Estate
	read func(context.Context, gitlet.ReadQuery) (gitlet.ReadAnswer, error)
}

func (r rowsOf) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return r.read(ctx, q)
}

// GitletNode is one Gitlet node, built as the engine builds one: its pool, its
// cache over its own directory, its host, its query server under the node's
// git-work share, and an HTTP server mounting the host's routes behind the
// API's own guard, so a person's git meets production's 401 challenge.
//
// Like a DataNode, ITS LIFETIME IS THE TEST THAT STARTED IT; a Restart's t only
// reports.
type GitletNode struct {
	o    GitOptions
	life testing.TB

	mu   sync.Mutex
	cur  *gitIncarnation
	dead []*gitIncarnation
	// killed is the incarnation a KILLED node's presence row still speaks
	// for (BackgroundBudget): set by Kill, cleared by Stop and Restart.
	killed *gitIncarnation
	// advertised, when positive, replaces the cache's own background budget
	// in the presence row (Advertise).
	advertised int64
}

// Advertise has the node's presence row say it gives its background work
// budget bytes (BackgroundBudget); 0 restores the cache's own figure. It stands
// in nodes of different sizes, which maintainer placement reads, on one
// machine; the node's own admissions still hold builds to the real budget.
func (g *GitletNode) Advertise(budget int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.advertised = budget
}

// gitIncarnation is one incarnation of a Gitlet node.
type gitIncarnation struct {
	id      string
	dir     string
	cache   *cache.Cache
	pool    *gitproc.Pool
	metrics *metrics.Recorder
	host    *host.Server
	share   *gitlet.GitShare
	resyncs *gitlet.Resyncs
	query   *query.Server
	// previews answers merge previews over the query server's cache, pool
	// and share.
	previews *home.Previews
	// compactor is built as the engine builds a Gitlet node's and NEVER
	// STARTED: every case drives Compact or Poll itself, so no case races a
	// pass of its own.
	compactor   *compact.Compactor
	maintainers compact.Maintainers
	srv         *httptest.Server
	reads       func(context.Context, gitlet.ReadQuery) (gitlet.ReadAnswer, error)
}

func (n *gitIncarnation) ID() string                 { return n.id }
func (n *gitIncarnation) URL() string                { return n.srv.URL }
func (n *gitIncarnation) Client() *http.Client       { return n.srv.Client() }
func (n *gitIncarnation) Host() *host.Server         { return n.host }
func (n *gitIncarnation) Cache() *cache.Cache        { return n.cache }
func (n *gitIncarnation) Pool() *gitproc.Pool        { return n.pool }
func (n *gitIncarnation) Metrics() *metrics.Recorder { return n.metrics }
func (n *gitIncarnation) CacheDir() string           { return n.dir }
func (n *gitIncarnation) Share() *gitlet.GitShare    { return n.share }
func (n *gitIncarnation) Compactor() *compact.Compactor {
	return n.compactor
}
func (n *gitIncarnation) Maintainers() compact.Maintainers { return n.maintainers }
func (n *gitIncarnation) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return n.reads(ctx, q)
}
func (n *gitIncarnation) GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	return n.query.Read(ctx, q)
}

// StartGitletNode starts a Gitlet node. It stops with t.
func StartGitletNode(t testing.TB, o GitOptions) *GitletNode {
	t.Helper()
	switch {
	case o.ID == "" || o.Dir == "" || o.Estate == nil || o.Reads == nil || o.Compactions == nil ||
		o.Expunges == nil || o.Maintainers == nil || o.Objects == nil || o.Credentials == nil:
		t.Fatalf("gitlettest: a Gitlet node needs an id, a directory, an estate, its reads, its compactions, "+
			"its expunges, its maintainers, its objects and its credentials (%+v)", o)
	}
	if o.Faults == nil {
		o.Faults = &gitletfault.Script{}
	}
	if o.Timing == (host.Timing{}) {
		o.Timing = host.Production
		o.Timing.PendingGrace, o.Timing.Keepalive = 2*time.Second, 200*time.Millisecond
	}
	o.Limits = withDefaults(o.Limits)
	if o.CacheMaxBytes == 0 {
		o.CacheMaxBytes = defaultCacheMaxBytes
	}
	// THE PATH'S GIT IS HELD TO THE NODE FLOOR HERE, before a pool refuses
	// it, so a machine below the floor reads what to run rather than an
	// operator's refusal; a GitPath a case names (a recorder, a wrap) stands
	// in front of that same git.
	if path := gittest.RequireNode(t); o.GitPath == "" {
		o.GitPath = path
	}
	if o.BoxTokens == nil {
		o.BoxTokens = NewBoxTokens(t)
	}
	if o.CheckTokens == nil {
		o.CheckTokens = NewCheckTokens(t)
	}
	if o.Runs == nil {
		o.Runs = NewFixtureRuns(o.BoxTokens)
	}
	g := &GitletNode{o: o, life: t}
	t.Cleanup(g.teardown)
	g.cur = g.start(t)
	return g
}

// start builds one incarnation in the engine's order: the cache prepared
// before the pool, whose HOME it names; the cache opened; the host built and
// started; its routes mounted behind the API's guard.
func (g *GitletNode) start(t testing.TB) *gitIncarnation {
	t.Helper()
	ctx := g.life.Context()
	n := &gitIncarnation{id: g.o.ID, dir: g.o.Dir, reads: g.o.Reads, maintainers: g.o.Maintainers}
	var err error
	if n.metrics, err = metrics.New(); err != nil {
		t.Fatalf("gitlettest: a recorder: %v", err)
	}
	layout, err := prepare(ctx, n.dir)
	if err != nil {
		t.Fatalf("gitlettest: prepare %s's cache: %v", n.id, err)
	}
	defer layout.Release()
	if n.pool, err = gitproc.New(ctx, gitproc.Options{Path: g.o.GitPath, Home: layout.Home,
		MaxProcesses: g.o.MaxProcesses, MaxTransfers: g.o.MaxTransfers, Metrics: n.metrics}); err != nil {
		t.Fatalf("gitlettest: %s's git pool: %v", n.id, err)
	}
	objects, err := objstore.NewStore(g.o.Objects())
	if err != nil {
		t.Fatalf("gitlettest: %s's object store: %v", n.id, err)
	}
	limits := func() gitlet.Limits { return g.o.Limits }
	fleet := g.o.Fleet
	if fleet == nil {
		fleet = func() (int, int, error) { return 1, 1, nil }
	}
	if n.cache, err = cache.Open(ctx, cache.Options{
		Layout: layout, MaxBytes: g.o.CacheMaxBytes, Git: n.pool, Objects: objects,
		Fetch: rowsOf{Estate: g.o.Estate, read: g.o.Reads}, Fleet: fleet,
		Limits: limits, HookCommand: HookCommand(t), Faults: g.o.Faults, Metrics: n.metrics, Logger: g.o.Logger,
		Now: time.Now,
	}); err != nil {
		t.Fatalf("gitlettest: open %s's cache: %v", n.id, err)
	}
	audit := g.o.Audit
	if audit == nil {
		audit = func(context.Context, types.OperatorActed) {}
	}
	fetched := g.o.Fetched
	if fetched == nil {
		fetched = func(context.Context, types.CodeFetched) {}
	}
	roster := g.o.Roster
	if roster == nil {
		roster = FixtureRoster
	}
	var signer host.Signer = FixtureSigner{}
	if g.o.Signer != nil {
		signer = g.o.Signer
	}
	// ONE share of the pool's read lane and ONE set of re-sync buckets (the
	// boxes', the checks' and the seats' tools'), which the query server and
	// the host both draw on, as in the engine.
	n.share = gitlet.NewGitShare(n.id, n.pool.Slots(gitproc.ClassRead), n.metrics)
	n.resyncs = gitlet.NewResyncs(g.o.ResyncClock, n.metrics)
	if n.host, err = host.New(host.Options{
		Cache: n.cache, Git: n.pool, Estate: g.o.Estate, Objects: objects,
		Audit: audit, Fetched: fetched, Limits: limits,
		DeletedRetention: func() time.Duration { return deletedRetention },
		Roster:           roster, Signer: signer, Share: n.share, Resyncs: n.resyncs, Current: g.o.Current,
		Boxes:  host.Boxes{Tokens: g.o.BoxTokens, Runs: g.o.Runs, Checks: g.o.CheckTokens},
		NodeID: n.id, ExternalBase: ExternalBase, Timing: g.o.Timing, Faults: g.o.Faults, Metrics: n.metrics,
		Logger: g.o.Logger,
	}); err != nil {
		t.Fatalf("gitlettest: build %s's host: %v", n.id, err)
	}
	if err = n.host.Start(ctx); err != nil {
		t.Fatalf("gitlettest: start %s's host: %v", n.id, err)
	}
	var reads query.Pool = n.pool
	if g.o.QueryPool != nil {
		reads = g.o.QueryPool(reads)
	}
	if n.query, err = query.New(query.Options{NodeID: n.id, Cache: n.cache, Git: reads, Share: n.share,
		Resyncs: n.resyncs,
		Roster:  roster, Current: g.o.Current, Fetched: fetched}); err != nil {
		t.Fatalf("gitlettest: build %s's query server: %v", n.id, err)
	}
	// The merge previews take the pool itself, never the QueryPool stand-in.
	if n.previews, err = home.NewPreviews(home.PreviewOptions{NodeID: n.id, Cache: n.cache, Git: n.pool,
		Share: n.share, Roster: roster, Current: g.o.Current, Fetched: fetched, Metrics: n.metrics,
		Logger: g.o.Logger}); err != nil {
		t.Fatalf("gitlettest: build %s's merge previews: %v", n.id, err)
	}
	// The compactor is built after the host has started, as in the engine.
	proposals := compactions{read: g.o.Reads, compact: g.o.Compactions, expunge: g.o.Expunges}
	if n.compactor, err = compact.New(compact.Options{Cache: n.cache, Objects: objects, Estate: proposals,
		Maintainers: g.o.Maintainers, NodeID: n.id, Limits: limits, Faults: g.o.Faults, Metrics: n.metrics,
		Logger: g.o.Logger}); err != nil {
		t.Fatalf("gitlettest: build %s's compactor: %v", n.id, err)
	}
	// The host's own handlers behind the API's real guard, never the API's
	// mount (internal/api's tests hold that), so a box meets the guard as in
	// production: its route unguarded, its run's token no guard credential.
	mux := http.NewServeMux()
	mux.Handle(host.PatternInfoRefs, n.host.InfoRefs())
	mux.Handle(host.PatternUploadPack, n.host.UploadPack())
	mux.Handle(host.PatternReceivePack, n.host.ReceivePack())
	mux.Handle(host.PatternRaw, n.host.Raw())
	mux.Handle(host.PatternArchive, n.host.Archive())
	mux.Handle(host.PatternBoxInfoRefs, n.host.BoxInfoRefs())
	mux.Handle(host.PatternBoxUploadPack, n.host.BoxUploadPack())
	mux.Handle(host.PatternBoxReceivePack, n.host.BoxReceivePack())
	n.srv = httptest.NewUnstartedServer(g.o.Credentials.guard().Middleware(mux))
	n.srv.Config.ErrorLog = log.New(serverLog{t: g.life}, "", 0)
	n.srv.Start()
	return n
}

// Git is the node's git half as the estate's router and server ask for it
// (estate.LocalGit): the running incarnation's query server, host and
// previews, read again at every request so a restarted node's is asked; none
// while the node is killed or stopped, which the serving side answers
// `no_backend`.
func (g *GitletNode) Git() (estate.GitServer, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur == nil {
		return nil, false
	}
	return gitHalf{reads: g.cur.query, writes: g.cur.host, previews: g.cur.previews}, true
}

// gitHalf is one incarnation's git half: its query server's reads, its host's
// engine writes and its merge previews (estate.GitServer).
type gitHalf struct {
	reads    *query.Server
	writes   *host.Server
	previews *home.Previews
}

func (h gitHalf) Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	return h.reads.Read(ctx, q)
}

func (h gitHalf) Write(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	return h.writes.Write(ctx, w)
}

func (h gitHalf) MergePreview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	return h.previews.MergePreview(ctx, q)
}

// BackgroundBudget is what the node's presence row says its cache gives its
// background work (cache.Cache.BackgroundBudget): the running incarnation's
// figure, or what Advertise set; a KILLED node's last, since a dead process
// gives no presence back and its row stands until its lease lapses; and
// nothing for a STOPPED node, which gave its presence back.
func (g *GitletNode) BackgroundBudget() (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.cur
	if n == nil {
		n = g.killed
	}
	switch {
	case n == nil:
		return 0, false
	case g.advertised > 0:
		return g.advertised, true
	}
	return n.cache.BackgroundBudget(), true
}

// GitPlacement is a topology's git placement as its routers read it
// (estate.GitPlacement): its Placement (who serves git) and, off ONE read of
// that listing, each listed node's background budget
// (GitletNode.BackgroundBudget). Node is the Gitlet node an id names, nil for
// one the topology holds none for yet; such a node is NOT SAYING, as is a
// stopped one still listed.
type GitPlacement struct {
	estate.Placement
	Node func(id string) *GitletNode
}

// Listing is the Placement's own listing — one read of it — with what each Gitlet
// node it lists says beside it.
func (p GitPlacement) Listing() (estate.GitListing, error) {
	nodes, err := p.Holders()
	if err != nil {
		return estate.GitListing{}, err
	}
	listing := estate.GitListing{Nodes: nodes, Budgets: map[string]int64{}}
	for _, id := range nodes {
		g := p.Node(id)
		if g == nil {
			continue
		}
		if budget, said := g.BackgroundBudget(); said {
			listing.Budgets[id] = budget
		}
	}
	return listing, nil
}

// Node is the Gitlet node as a case reads it — nil while it is killed or stopped.
func (g *GitletNode) Node() Node {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cur == nil {
		return nil
	}
	return g.cur
}

// Kill ends the node as a process death does, as far as git sees it: its
// listener and every client connection closed, its host stopped under an
// ended context. It WAITS FOR NOTHING A CRASH MAY HOLD: the server's Close
// waits for its handlers in the teardown, after the crashes are released.
func (g *GitletNode) Kill() {
	g.mu.Lock()
	n := g.cur
	g.cur = nil
	if n != nil {
		g.dead = append(g.dead, n)
		g.killed = n
	}
	g.mu.Unlock()
	if n == nil {
		return
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	n.compactor.Stop(ended)
	n.srv.Listener.Close()
	n.srv.CloseClientConnections()
	n.previews.Stop(ended)
	n.query.Stop(ended)
	n.host.Stop(ended)
}

// Stop ends the node as the engine's graceful stop does, in its order. FIRST
// THE LISTENER, drained within drain as the API's is (api.App.DrainGit): no new
// connection, every running request finishes, and no bound of its own, since
// the process's supervisor bounds a shutdown. THEN THE GIT READS AND THE HOST,
// as stopGitletHost stops them, bounded by host.StopBudget: in-flight reads and
// previews are cut and waited for, and a request still running past the
// drain is cancelled and waited for. Unlike Kill, it waits for what it ends,
// so it never stops a node a crash holds.
func (g *GitletNode) Stop(drain context.Context) {
	g.mu.Lock()
	n := g.cur
	g.cur, g.killed = nil, nil
	if n != nil {
		g.dead = append(g.dead, n)
	}
	g.mu.Unlock()
	if n == nil {
		return
	}
	if err := n.srv.Config.Shutdown(drain); err != nil {
		g.life.Logf("gitlettest: %s's listener drained only in part: %v", g.o.ID, err)
	}
	stop, cancel := context.WithTimeout(context.WithoutCancel(drain), host.StopBudget)
	defer cancel()
	n.compactor.Stop(stop)
	n.previews.Stop(stop)
	n.query.Stop(stop)
	n.host.Stop(stop)
	n.srv.CloseClientConnections()
}

// Restart rebuilds the node over the same directory, with a new listener and
// so a new URL. t reports; the node's lifetime stays its starter's.
func (g *GitletNode) Restart(t testing.TB) {
	t.Helper()
	g.mu.Lock()
	running := g.cur != nil
	g.mu.Unlock()
	if running {
		t.Fatalf("gitlettest: Restart of Gitlet node %s, which was neither killed nor stopped", g.o.ID)
	}
	n := g.start(t)
	g.mu.Lock()
	g.cur, g.killed = n, nil
	g.mu.Unlock()
}

// teardown ends every incarnation, the crashes released FIRST: a server's
// Close waits for every handler, and a crash holds one past its node's death.
func (g *GitletNode) teardown() {
	g.o.Faults.ReleaseCrashes()
	g.mu.Lock()
	nodes := slices.Clone(g.dead)
	if g.cur != nil {
		nodes = append(nodes, g.cur)
	}
	g.cur, g.dead, g.killed = nil, nil, nil
	g.mu.Unlock()
	for _, n := range nodes {
		n.compactor.Stop(context.Background())
		n.previews.Stop(context.Background())
		n.query.Stop(context.Background())
		n.host.Stop(context.Background())
		n.srv.Close()
	}
}

// compactions is a Gitlet node's code log as its compactor reaches it
// (compact.Estate).
type compactions struct {
	read    func(context.Context, gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	compact func(context.Context, gitlet.CompactionProposal) (gitlet.CompactResult, error)
	expunge func(context.Context, gitlet.ExpungeProposal) (gitlet.ExpungeResult, error)
}

func (c compactions) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return c.read(ctx, q)
}

func (c compactions) Compact(ctx context.Context, p gitlet.CompactionProposal) (gitlet.CompactResult, error) {
	return c.compact(ctx, p)
}

func (c compactions) Expunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	return c.expunge(ctx, p)
}

// serverLog is a Gitlet node's HTTP server's error log: A HANDLER THAT PANICS
// FAILS THE CASE. net/http recovers the panic and only logs it, and git prints
// the cut connection as `RPC failed`, which a case asserting a refusal could
// take for one. http.ErrAbortHandler (the host's deliberate cut) is never
// logged; every other line goes to the case's log.
type serverLog struct{ t testing.TB }

func (l serverLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("http: panic serving")) {
		l.t.Errorf("gitlettest: a Gitlet node's handler panicked: %s", p)
	} else {
		l.t.Logf("gitlettest: a Gitlet node's server: %s", bytes.TrimSpace(p))
	}
	return len(p), nil
}

// prepare is cache.Prepare, retried briefly while another incarnation still
// holds the directory: a killed node's cache gives its claim back when its last
// work ends: at once in a process that really died, a moment later here.
func prepare(ctx context.Context, dir string) (cache.Layout, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		layout, err := cache.Prepare(ctx, dir)
		if !errors.Is(err, cache.ErrCacheInUse) || time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: it answers Prepare's last error, which fails the restart
			return layout, err
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}
