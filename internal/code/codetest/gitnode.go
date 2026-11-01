package codetest

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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Credentials are a topology's people: a token for every human seat and one
// bound to nobody, the Tier A guard every git node's server admits them by,
// and the seat each credential is bound to — answered as the engine's guard
// reads a Tier A token's binding (auth.SeatBindings): the identity directory's
// row under the token's login, resolved against a running org that holds
// every person's seat as a human one.
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

// Guard is the Tier A every git node's server reads: `api.auth.tokens`, one per
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
// to their own seat and ci's to none, as the engine binds a Tier A token
// through the identity directory.
func (c *Credentials) guard() *auth.Guard {
	return auth.New(c.Guard()).BindSeats(auth.SeatBindings{Directory: c, Chart: c})
}

// BoundSeat is the identity directory's row under a token's login
// (auth.BindingDirectory): every person's token bound to their own seat, and
// none for ci's — a credential nobody binds.
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

// defaultCacheMaxBytes is a git node's cache budget unless a topology sets
// one: a gibibyte holds every repository the suite pushes, so no case is
// evicted by a budget it did not mean to test.
const defaultCacheMaxBytes = 1 << 30

// GitOptions configure a git node.
type GitOptions struct {
	// ID is the node's id.
	ID string
	// Dir is the cache's directory, the pool's HOME beneath it.
	Dir string
	// Estate is what the node's host and cache ask — a value that answers
	// through whatever data node serves now, read again at every call, so a
	// restart of the node or of the data nodes behind it never hands it a
	// dead one.
	Estate host.Estate
	// Reads answers Node.Read, through the same seam the node's routes would
	// read the code host by — the compactor's due listing among its reads.
	Reads func(context.Context, code.ReadQuery) (code.ReadAnswer, error)
	// Compactions is what the node's compactor proposes a compaction to: the
	// repository's committer, through whatever data node serves now, read
	// again at every call — the engine hands its compactor the router's code
	// seam, whose Compact this is.
	Compactions func(context.Context, code.CompactionProposal) (code.CompactResult, error)
	// Expunges is what the node's compactor proposes an expunge's begin and
	// commit to: the repository's committer, as Compactions — the router's
	// code seam's Expunge, in the engine.
	Expunges func(context.Context, code.ExpungeProposal) (code.ExpungeResult, error)
	// Maintainers is which repositories the node maintains, as its compactor
	// asks once a pass: the node's own router in every topology, as the
	// engine hands its compactor the router — or a fixed answer a case hands
	// it (SplitMaintainers).
	Maintainers compact.Maintainers
	// Objects is the object store's backend for each incarnation: asked
	// again at every start, because a backend riding a broker connection
	// dies with a member's restart.
	Objects func() objstore.Backend
	// Limits are code.limits; a zero field takes the default.
	Limits code.Limits
	// Timing is the host's; the zero value is host.Production with a
	// PendingGrace of two seconds and a Keepalive of 200 ms.
	Timing host.Timing
	// Faults is the fleet's script; nil is a fresh one.
	Faults *codefault.Script
	// Credentials are who the node's guard admits, and the seat each is.
	Credentials *Credentials
	// Runs is the fleet's coding runs' rows, which the node's host admits a
	// coding box by — ONE store for every git node of a fleet, as every node
	// reads the one pending store on the fleet's coordination store; nil is
	// one of the node's own (NewFixtureRuns).
	Runs host.Runs
	// BoxTokens verifies a box's run token, over the fleet's keyring; nil is
	// one of the node's own over it (NewBoxTokens), as every node builds its
	// own.
	BoxTokens *host.BoxTokens
	// CheckTokens verifies a check's box's token, over the fleet's keyring;
	// nil is one of the node's own over it (NewCheckTokens).
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
	// Roster is the chart the node's query server judges a seat's read by,
	// and its code host a seat's engine write: nil is the fixture chart
	// (FixtureRoster). A case standing in a node that holds no chart hands one
	// answering nil, which answers a seat unready (no_chart).
	Roster func() code.Roster
	// Signer signs the node's engine-built commits; nil is FixtureSigner.
	Signer host.Signer
	// Current is whether the node serves the company's configuration, as
	// the query server and the code host read it; nil is current.
	Current func() bool
	// QueryPool stands between the query server and the node's pool — a
	// case that holds a read's processes, or records them; nil is the pool
	// itself.
	QueryPool func(query.Pool) query.Pool
	// Fleet is how many live git and data nodes the cache's backfill paces
	// its reads by (cache.Options.Fleet); nil is a node with no fleet, one of
	// each.
	Fleet cache.Fleet
	// ResyncClock is the clock of the node's ONE set of re-sync buckets
	// (code.NewResyncs), which its code host and its query server both draw
	// on; nil is the wall clock. A case passing a refill hands one it moves
	// by hand; every incarnation reads it.
	ResyncClock func() time.Time
}

// rowsOf is the rows' side of a git node's cache (cache.Fetcher): its
// snapshots through the estate the host asks, and its reads of the code host
// — the backfill's pages of pushes among them — through the seam Node.Read
// answers by, as the engine hands the cache one router for both.
type rowsOf struct {
	host.Estate
	read func(context.Context, code.ReadQuery) (code.ReadAnswer, error)
}

func (r rowsOf) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return r.read(ctx, q)
}

// GitNode is one git node: its pool, its cache over its own directory, its
// host and its HTTP server mounting the host's three routes behind the API's
// own guard, so a person's git meets the 401 challenge and Basic credential it
// meets in production — and its query server, over the same cache and pool,
// under the node's own seat git-work share, as the engine builds one.
//
// Like a DataNode, ITS LIFETIME IS THE TEST THAT STARTED IT, and a Restart's t
// only reports.
type GitNode struct {
	o    GitOptions
	life testing.TB

	mu   sync.Mutex
	cur  *gitIncarnation
	dead []*gitIncarnation
	// killed is the incarnation a KILLED node's presence row still speaks
	// for (BackgroundBudget): set by Kill, cleared by Stop and Restart.
	killed *gitIncarnation
	// advertised is what the node says its cache gives its background work
	// in place of the cache's own figure, where it is positive (Advertise).
	advertised int64
}

// Advertise has the node say it gives its background work budget bytes — 0
// is the cache's own figure again — as its presence row says it
// (BackgroundBudget): git nodes of different sizes, which a repository's
// maintainer is placed by, on one machine. The node's own admissions still
// hold its builds to the cache's real budget.
func (g *GitNode) Advertise(budget int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.advertised = budget
}

// gitIncarnation is one incarnation of a git node.
type gitIncarnation struct {
	id      string
	dir     string
	cache   *cache.Cache
	pool    *gitproc.Pool
	metrics *metrics.Recorder
	host    *host.Server
	share   *code.GitShare
	resyncs *code.Resyncs
	query   *query.Server
	// previews answers the node's merge previews, built beside the query
	// server over the same cache, pool and share, as the engine builds them.
	previews *home.Previews
	// compactor is built as the engine builds a git node's and NEVER
	// STARTED: every case drives Compact or Poll itself, so no case races a
	// pass of its own.
	compactor   *compact.Compactor
	maintainers compact.Maintainers
	srv         *httptest.Server
	reads       func(context.Context, code.ReadQuery) (code.ReadAnswer, error)
}

func (n *gitIncarnation) ID() string                 { return n.id }
func (n *gitIncarnation) URL() string                { return n.srv.URL }
func (n *gitIncarnation) Client() *http.Client       { return n.srv.Client() }
func (n *gitIncarnation) Host() *host.Server         { return n.host }
func (n *gitIncarnation) Cache() *cache.Cache        { return n.cache }
func (n *gitIncarnation) Pool() *gitproc.Pool        { return n.pool }
func (n *gitIncarnation) Metrics() *metrics.Recorder { return n.metrics }
func (n *gitIncarnation) CacheDir() string           { return n.dir }
func (n *gitIncarnation) Share() *code.GitShare      { return n.share }
func (n *gitIncarnation) Compactor() *compact.Compactor {
	return n.compactor
}
func (n *gitIncarnation) Maintainers() compact.Maintainers { return n.maintainers }
func (n *gitIncarnation) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return n.reads(ctx, q)
}
func (n *gitIncarnation) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	return n.query.Read(ctx, q)
}

// StartGitNode starts a git node. It stops with t.
func StartGitNode(t testing.TB, o GitOptions) *GitNode {
	t.Helper()
	switch {
	case o.ID == "" || o.Dir == "" || o.Estate == nil || o.Reads == nil || o.Compactions == nil ||
		o.Expunges == nil || o.Maintainers == nil || o.Objects == nil || o.Credentials == nil:
		t.Fatalf("codetest: a git node needs an id, a directory, an estate, its reads, its compactions, "+
			"its expunges, its maintainers, its objects and its credentials (%+v)", o)
	}
	if o.Faults == nil {
		o.Faults = &codefault.Script{}
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
	// operator's refusal — and a stand-in a case names (a recorder, a wrap)
	// stands in front of that same git.
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
	g := &GitNode{o: o, life: t}
	t.Cleanup(g.teardown)
	g.cur = g.start(t)
	return g
}

// start builds one incarnation in the engine's order: the cache prepared
// before the pool, whose HOME it names; the cache opened; the host built and
// started; its routes mounted behind the API's guard.
func (g *GitNode) start(t testing.TB) *gitIncarnation {
	t.Helper()
	ctx := g.life.Context()
	n := &gitIncarnation{id: g.o.ID, dir: g.o.Dir, reads: g.o.Reads, maintainers: g.o.Maintainers}
	var err error
	if n.metrics, err = metrics.New(); err != nil {
		t.Fatalf("codetest: a recorder: %v", err)
	}
	layout, err := prepare(ctx, n.dir)
	if err != nil {
		t.Fatalf("codetest: prepare %s's cache: %v", n.id, err)
	}
	defer layout.Release()
	if n.pool, err = gitproc.New(ctx, gitproc.Options{Path: g.o.GitPath, Home: layout.Home,
		MaxProcesses: g.o.MaxProcesses, MaxTransfers: g.o.MaxTransfers, Metrics: n.metrics}); err != nil {
		t.Fatalf("codetest: %s's git pool: %v", n.id, err)
	}
	objects, err := objstore.NewStore(g.o.Objects())
	if err != nil {
		t.Fatalf("codetest: %s's object store: %v", n.id, err)
	}
	limits := func() code.Limits { return g.o.Limits }
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
		t.Fatalf("codetest: open %s's cache: %v", n.id, err)
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
	// ONE share of the pool's read lane, as the engine builds one: the query
	// server's reads and the host's engine writes draw on it alike.
	n.share = code.NewGitShare(n.id, n.pool.Slots(gitproc.ClassRead), n.metrics)
	// And ONE set of re-sync buckets — the boxes', the checks' and the seats'
	// tools' — which both draw on, as the engine builds one.
	n.resyncs = code.NewResyncs(g.o.ResyncClock, n.metrics)
	if n.host, err = host.New(host.Options{
		Cache: n.cache, Git: n.pool, Estate: g.o.Estate, Objects: objects,
		Audit: audit, Fetched: fetched, Limits: limits,
		DeletedRetention: func() time.Duration { return deletedRetention },
		Roster:           roster, Signer: signer, Share: n.share, Resyncs: n.resyncs, Current: g.o.Current,
		Boxes:  host.Boxes{Tokens: g.o.BoxTokens, Runs: g.o.Runs, Checks: g.o.CheckTokens},
		NodeID: n.id, ExternalBase: ExternalBase, Timing: g.o.Timing, Faults: g.o.Faults, Metrics: n.metrics,
		Logger: g.o.Logger,
	}); err != nil {
		t.Fatalf("codetest: build %s's host: %v", n.id, err)
	}
	if err := n.host.Start(ctx); err != nil {
		t.Fatalf("codetest: start %s's host: %v", n.id, err)
	}
	// THE QUERY SERVER, as the engine builds it: over the node's cache and
	// pool, under the node's one share.
	var reads query.Pool = n.pool
	if g.o.QueryPool != nil {
		reads = g.o.QueryPool(reads)
	}
	if n.query, err = query.New(query.Options{NodeID: n.id, Cache: n.cache, Git: reads, Share: n.share,
		Resyncs: n.resyncs,
		Roster:  roster, Current: g.o.Current, Fetched: fetched}); err != nil {
		t.Fatalf("codetest: build %s's query server: %v", n.id, err)
	}
	// THE MERGE PREVIEWS, as the engine builds them beside the query server:
	// over the node's cache and its own pool, under the same share, judged
	// and recorded as its reads are.
	if n.previews, err = home.NewPreviews(home.PreviewOptions{NodeID: n.id, Cache: n.cache, Git: n.pool,
		Share: n.share, Roster: roster, Current: g.o.Current, Fetched: fetched, Metrics: n.metrics,
		Logger: g.o.Logger}); err != nil {
		t.Fatalf("codetest: build %s's merge previews: %v", n.id, err)
	}
	// THE COMPACTOR, as the engine builds it after the host has started: over
	// the node's cache and store, listing through the node's reads and
	// proposing through its compactions and its expunges.
	proposals := compactions{read: g.o.Reads, compact: g.o.Compactions, expunge: g.o.Expunges}
	if n.compactor, err = compact.New(compact.Options{Cache: n.cache, Objects: objects, Estate: proposals,
		Maintainers: g.o.Maintainers, NodeID: n.id, Limits: limits, Faults: g.o.Faults, Metrics: n.metrics,
		Logger: g.o.Logger}); err != nil {
		t.Fatalf("codetest: build %s's compactor: %v", n.id, err)
	}
	// A PERSON'S THREE ROUTES AND A CODING BOX'S THREE, on one mux behind the
	// API's real guard, as a git node serves them: the host's own handlers
	// — never the API's mount, which internal/api's own tests hold — so a
	// box meets the guard exactly as it does in production, its route
	// unguarded and its run's token no credential of the guard's.
	mux := http.NewServeMux()
	mux.Handle(host.PatternInfoRefs, n.host.InfoRefs())
	mux.Handle(host.PatternUploadPack, n.host.UploadPack())
	mux.Handle(host.PatternReceivePack, n.host.ReceivePack())
	mux.Handle(host.PatternBoxInfoRefs, n.host.BoxInfoRefs())
	mux.Handle(host.PatternBoxUploadPack, n.host.BoxUploadPack())
	mux.Handle(host.PatternBoxReceivePack, n.host.BoxReceivePack())
	n.srv = httptest.NewUnstartedServer(g.o.Credentials.guard().Middleware(mux))
	n.srv.Config.ErrorLog = log.New(serverLog{t: g.life}, "", 0)
	n.srv.Start()
	return n
}

// Git is the node's git half as the estate's router and server ask for it
// (estate.LocalGit): its running incarnation's query server for a read by
// object id, its host for an engine write and its previews for a merge
// preview, as the engine pairs them over one cache, one pool and one share —
// read again at every request so a restarted node's is the one asked, and none
// while the node is killed or stopped, which the serving side answers
// `no_backend`, as a git node whose code host is not built answers.
func (g *GitNode) Git() (estate.GitServer, bool) {
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

func (h gitHalf) Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	return h.reads.Read(ctx, q)
}

func (h gitHalf) Write(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	return h.writes.Write(ctx, w)
}

func (h gitHalf) MergePreview(ctx context.Context, q code.MergePreviewQuery) (code.MergePreviewAnswer, error) {
	return h.previews.MergePreview(ctx, q)
}

// BackgroundBudget is what the node's presence row says its cache gives its
// background work (cache.Cache.BackgroundBudget), as an engine's git node
// writes it on every beat beside its status: its running incarnation's
// figure — or what a case has it advertise in its place (Advertise); a KILLED
// node's last — a process that dies gives no presence back, so the row its
// last beat wrote, figure and all, stands until its lease lapses; and nothing
// for a STOPPED node, which gave its presence back before it stopped, so no
// row speaks for it.
func (g *GitNode) BackgroundBudget() (int64, bool) {
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
// (estate.GitPlacement): its Placement — who serves git, as the topology lists
// them, which the git walk and a repository's home order — and, off ONE read
// of that same listing, what each git node it lists says its cache gives its
// background work, as an engine's presence row says it
// (GitNode.BackgroundBudget). Node is the git node an id names, nil for an id
// the topology holds no git node for (yet): a node so listed is NOT SAYING,
// and so is a stopped one a topology still lists, since no row speaks for it.
type GitPlacement struct {
	estate.Placement
	Node func(id string) *GitNode
}

// Listing is the Placement's own listing — one read of it — with what each git
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

// Node is the git node as a case reads it — nil while it is killed or stopped.
func (g *GitNode) Node() Node {
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
func (g *GitNode) Kill() {
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

// Stop ends the node as the engine's graceful stop does, as far as git sees
// it, in the engine's order. FIRST THE LISTENER, drained within drain: no new
// connection is taken and every request already running finishes — the API's
// listener, which the engine shuts down before its teardown and whose drain
// finishes the git requests it let through (api.App.DrainGit), with no bound
// of its own, since what bounds a shutdown is whatever supervises the process.
// THEN THE GIT READS AND THE HOST, as the teardown's stopCodeHost stops them
// and bounded as it is, by host.StopBudget: a read or a merge preview in
// flight is cut and waited for (query.Server.Stop, home.Previews.Stop), and a
// request still running past the drain is cancelled and waited for, its relay
// and its cache stopped. Unlike Kill, it waits for what it ends, so it is
// never the stop of a node a crash holds.
func (g *GitNode) Stop(drain context.Context) {
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
		g.life.Logf("codetest: %s's listener drained only in part: %v", g.o.ID, err)
	}
	stop, cancel := context.WithTimeout(context.WithoutCancel(drain), host.StopBudget)
	defer cancel()
	n.compactor.Stop(stop)
	n.previews.Stop(stop)
	n.query.Stop(stop)
	n.host.Stop(stop)
	n.srv.CloseClientConnections()
}

// Restart rebuilds the node over the same directory: a new pool, the cache
// opened again, a new host and a new listener — so its URL is a new one. t
// reports; the node's lifetime stays its starter's.
func (g *GitNode) Restart(t testing.TB) {
	t.Helper()
	g.mu.Lock()
	running := g.cur != nil
	g.mu.Unlock()
	if running {
		t.Fatalf("codetest: Restart of git node %s, which was neither killed nor stopped", g.o.ID)
	}
	n := g.start(t)
	g.mu.Lock()
	g.cur, g.killed = n, nil
	g.mu.Unlock()
}

// teardown ends every incarnation, the crashes released FIRST: a server's
// Close waits for every handler, and a crash holds one past its node's death.
func (g *GitNode) teardown() {
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

// compactions is a git node's code log as its compactor reaches it
// (compact.Estate): the due listings through the node's reads, the proposals
// through its compactions and its expunges.
type compactions struct {
	read    func(context.Context, code.ReadQuery) (code.ReadAnswer, error)
	compact func(context.Context, code.CompactionProposal) (code.CompactResult, error)
	expunge func(context.Context, code.ExpungeProposal) (code.ExpungeResult, error)
}

func (c compactions) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return c.read(ctx, q)
}

func (c compactions) Compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	return c.compact(ctx, p)
}

func (c compactions) Expunge(ctx context.Context, p code.ExpungeProposal) (code.ExpungeResult, error) {
	return c.expunge(ctx, p)
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
