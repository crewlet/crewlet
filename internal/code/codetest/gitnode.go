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
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Credentials are a topology's people: a token for every human seat and one
// bound to nobody, the Tier A guard every git node's server admits them by,
// and the seat each credential is bound to.
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

// Person is the human seat a credential is bound to (host.Options.Persons):
// every person's own, and none for ci's.
func (c *Credentials) Person(operatorID string) string {
	if _, ok := c.tokens[operatorID]; ok && operatorID != "" && operatorID != "ci" {
		return operatorID
	}
	return ""
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
	// read the code host by.
	Reads func(context.Context, code.ReadQuery) (code.ReadAnswer, error)
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
	// Audit is handed every push record the host publishes; nil keeps none.
	Audit func(context.Context, types.OperatorActed)
	// MaxTransfers and MaxProcesses are the pool's transfer and read lanes;
	// 0 is the pool's default.
	MaxTransfers, MaxProcesses int
	// CacheMaxBytes is the cache's budget; 0 is defaultCacheMaxBytes.
	CacheMaxBytes int64
	// GitPath is the git the pool runs; "" is the one on PATH.
	GitPath string
	// Logger is the host's; nil is the package's own.
	Logger *slog.Logger
}

// GitNode is one git node: its pool, its cache over its own directory, its
// host and its HTTP server mounting the host's three routes behind the API's
// own guard, so a person's git meets the 401 challenge and Basic credential it
// meets in production.
//
// Like a DataNode, ITS LIFETIME IS THE TEST THAT STARTED IT, and a Restart's t
// only reports.
type GitNode struct {
	o    GitOptions
	life testing.TB

	mu   sync.Mutex
	cur  *gitIncarnation
	dead []*gitIncarnation
}

// gitIncarnation is one incarnation of a git node.
type gitIncarnation struct {
	id      string
	dir     string
	cache   *cache.Cache
	pool    *gitproc.Pool
	metrics *metrics.Recorder
	host    *host.Server
	srv     *httptest.Server
	reads   func(context.Context, code.ReadQuery) (code.ReadAnswer, error)
}

func (n *gitIncarnation) ID() string                 { return n.id }
func (n *gitIncarnation) URL() string                { return n.srv.URL }
func (n *gitIncarnation) Client() *http.Client       { return n.srv.Client() }
func (n *gitIncarnation) Host() *host.Server         { return n.host }
func (n *gitIncarnation) Cache() *cache.Cache        { return n.cache }
func (n *gitIncarnation) Pool() *gitproc.Pool        { return n.pool }
func (n *gitIncarnation) Metrics() *metrics.Recorder { return n.metrics }
func (n *gitIncarnation) CacheDir() string           { return n.dir }
func (n *gitIncarnation) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return n.reads(ctx, q)
}

// StartGitNode starts a git node. It stops with t.
func StartGitNode(t testing.TB, o GitOptions) *GitNode {
	t.Helper()
	switch {
	case o.ID == "" || o.Dir == "" || o.Estate == nil || o.Reads == nil || o.Objects == nil ||
		o.Credentials == nil:
		t.Fatalf("codetest: a git node needs an id, a directory, an estate, its reads, its objects "+
			"and its credentials (%+v)", o)
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
	n := &gitIncarnation{id: g.o.ID, dir: g.o.Dir, reads: g.o.Reads}
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
	if n.cache, err = cache.Open(ctx, cache.Options{
		Layout: layout, MaxBytes: g.o.CacheMaxBytes, Git: n.pool, Objects: objects, Fetch: g.o.Estate,
		Limits: limits, HookCommand: HookCommand(t), Faults: g.o.Faults, Metrics: n.metrics, Now: time.Now,
	}); err != nil {
		t.Fatalf("codetest: open %s's cache: %v", n.id, err)
	}
	audit := g.o.Audit
	if audit == nil {
		audit = func(context.Context, types.OperatorActed) {}
	}
	if n.host, err = host.New(host.Options{
		Cache: n.cache, Git: n.pool, Estate: g.o.Estate, Objects: objects,
		Persons: g.o.Credentials.Person, Audit: audit, Limits: limits,
		DeletedRetention: func() time.Duration { return deletedRetention },
		NodeID:           n.id, Timing: g.o.Timing, Faults: g.o.Faults, Metrics: n.metrics, Logger: g.o.Logger,
	}); err != nil {
		t.Fatalf("codetest: build %s's host: %v", n.id, err)
	}
	if err := n.host.Start(ctx); err != nil {
		t.Fatalf("codetest: start %s's host: %v", n.id, err)
	}
	mux := http.NewServeMux()
	mux.Handle(host.PatternInfoRefs, n.host.InfoRefs())
	mux.Handle(host.PatternUploadPack, n.host.UploadPack())
	mux.Handle(host.PatternReceivePack, n.host.ReceivePack())
	n.srv = httptest.NewUnstartedServer(auth.New(g.o.Credentials.Guard()).Middleware(mux))
	n.srv.Config.ErrorLog = log.New(serverLog{t: g.life}, "", 0)
	n.srv.Start()
	return n
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
	}
	g.mu.Unlock()
	if n == nil {
		return
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	n.srv.Listener.Close()
	n.srv.CloseClientConnections()
	n.host.Stop(ended)
}

// Stop ends the node as the engine's graceful stop does, as far as git sees
// it, in the engine's order. FIRST THE LISTENER, drained within drain: no new
// connection is taken and every request already running finishes — the API's
// listener, which the engine shuts down before its teardown and whose drain
// finishes the git requests it let through (api.App.DrainGit), with no bound
// of its own, since what bounds a shutdown is whatever supervises the process.
// THEN THE HOST, the teardown's first step (the engine's stopCodeHost) and
// bounded as it is, by host.StopBudget: a request still running past the
// drain is cancelled and waited for, its relay and its cache stopped. Unlike
// Kill, it waits for what it ends, so it is never the stop of a node a crash
// holds.
func (g *GitNode) Stop(drain context.Context) {
	g.mu.Lock()
	n := g.cur
	g.cur = nil
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
	g.cur = n
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
	g.cur, g.dead = nil, nil
	g.mu.Unlock()
	for _, n := range nodes {
		n.host.Stop(context.Background())
		n.srv.Close()
	}
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
