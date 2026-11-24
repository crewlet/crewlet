package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/flock"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/words"
)

// An embedded server's start has two halves: nats-server's Start (store
// recovery, the metadata group, leaf and client listeners), then the route
// listener a goroutine of Start opens. ([clusterReadyTimeout] bounds a later
// wait: this member's JetStream catching up with the metadata group.)
//
// THE RECOVERY HAS NO BUDGET: it is linear in an unbounded store (1 to 1.5 s per
// GiB measured) and nats-server cannot stop one, so a budget only fails working
// boots, each recovering again. It reports every [startReportEvery], stays
// cancellable ([startEmbedded]), and a dead disk is the supervisor's to bound.
// THE LISTENERS KEEP [listenTimeout], to name a nats-server that never reports
// ready. It also bounds each in-process handshake ([embeddedServer.connect]),
// where nats's 2 s default failed loaded hosts against a server that was up.
const (
	// listenTimeout bounds the listeners once Start has returned: a bind
	// each and the route listener's goroutine. At worst 37 ms measured
	// (sixteen clustered members at once, race detector, four cores); thirty
	// seconds is a thousand times that and within a watcher's patience.
	listenTimeout = 30 * time.Second

	// startReportEvery is how often a start still running says so. A fresh
	// store starts in milliseconds and gigabytes in seconds, so ten seconds
	// in is a large recovery or a stuck disk, worth a line before a person
	// watching a quiet boot reaches for an interrupt.
	startReportEvery = 10 * time.Second
)

// startTiming is the bound on an embedded server's listeners and the interval
// between reports of a long start. An unset field is production's
// ([startTiming.resolved]); it is a value, as [readiness] is, so a case can
// reach a branch only a budget running out takes without waiting half a minute.
type startTiming struct {
	listen, report time.Duration
}

// resolved is t with each unset field at production's value, field by field:
// a case tightening only the listener budget would otherwise keep a zero
// report interval, which panics [time.NewTicker].
func (t startTiming) resolved() startTiming {
	if t.listen <= 0 {
		t.listen = listenTimeout
	}
	if t.report <= 0 {
		t.report = startReportEvery
	}
	return t
}

const (
	// clusterReadyTimeout bounds waiting for a clustered member's JetStream
	// to become current: ~8 s for a quiet three-member loopback cluster,
	// multiplied generously because it is a one-time boot cost and the
	// alternative is provisioning into a leaderless metadata group.
	clusterReadyTimeout = 60 * time.Second

	// clusterReadyPoll is how often that wait re-checks: short enough not to
	// hold back a fast local cluster, at most a few hundred times.
	clusterReadyPoll = 100 * time.Millisecond

	// clusterReadyAsk is how long one "do you answer?" put to the fleet's
	// JetStream during that wait waits for its reply. It is
	// [jsprovision.ReadTerm] itself: the question is a metadata read, which
	// only the leader answers and the rest drop, the request that term sizes.
	clusterReadyAsk = jsprovision.ReadTerm
)

// readiness bounds the wait for the fleet to serve this broker: its length,
// its re-check interval and one question's reply. The zero value is production
// ([embeddedServer.readiness]); it is a value so a case can reach the branch
// that names what to fix, which only the whole sixty-second wait running out
// takes.
type readiness struct {
	timeout, poll, ask time.Duration
}

// embeddedServer is a nats-server running inside this process. Solo and fleet
// differ in cluster configuration only; the client code above is identical.
//
// Every connection this process opens to it is in process, through an
// in-memory pipe ([embeddedServer.connect]). A clustered member still opens a
// client listener, because nats-server routes only once one is open; it binds
// loopback and is advertised to nobody ([embeddedOptions]).
type embeddedServer struct {
	ns *server.Server
	// started is closed when ns.Start has returned; every shutdown waits for
	// it ([shutdownAndClean]).
	started <-chan struct{}
	// death is the server's own declaration, by its logger's Fatalf, that it
	// cannot go on ([Death]).
	death *Death
	// scratch is a private directory this server owns and deletes on
	// shutdown, used when no StoreDir was configured.
	scratch string
	// dirLock holds the configured StoreDir until the server has stopped
	// writing there ([StoreDirLock]). Nil for a scratch store and a leaf.
	dirLock *flock.Lock
	// clustered decides whether anything waits for a metadata leader.
	clustered bool
	// metaGroup is whether this member runs a metadata group at all:
	// nats-server runs one with neither a route nor a gateway port
	// standalone, starting none (server.go, standAloneMode), however its
	// cluster is named. Decided from the options, never from a read, so a
	// group that is down is never taken for none ([ErrMetaGroupDown]).
	metaGroup bool
	// leaf marks a broker joined as a LEAF: JetStream is off here, so every
	// stream and bucket is across its link ([Config.LeafURLs]).
	leaf bool
	// auth decides every connection to this server ([brokerAuth]) and holds
	// the keys this process's own connections present: the engine's, and the
	// system account's on a clustered member (membership.go).
	auth *brokerAuth
	// ready bounds the waits for the fleet to serve this broker; set only by
	// this package's tests, zero is production ([readiness]).
	ready readiness
	// tunnel is a leaf's way to the members, whose upstream socket its meter
	// reads ([tunnel], [LinkMeter]); nil on a member. Started before the
	// server and closed after it.
	tunnel *tunnel
	// pings is how long the leaf's own link may go without answering its
	// pings before the leaf closes it ([leafPingTolerance]); zero on a
	// member.
	pings time.Duration
}

// LeaseAccount is the local account a leaf's LEASE LINK binds: the second leaf
// link, isolated from every other leaf's interest, that the lease connection
// rides and nothing else ([Queue.DialLeases]; package doc, "A node's leases
// ride a link of their own"). The data link binds the global account.
const LeaseAccount = "CREWLET_LEASES"

// leafPingTolerance is how long a leaf's link may go without a pong before the
// leaf closes it as stale, read from opts as nats-server built them: the
// leaf-node ping settings where set, else the server's, defaults filled in
// (client.go setPingTimer, processPingTimer, v2.15.3). A PING whose round trip
// exceeds MaxPingsOut intervals closes the link. Zero on a member.
func leafPingTolerance(opts *server.Options) time.Duration {
	if len(opts.LeafNode.Remotes) == 0 {
		return 0
	}
	every, out := opts.PingInterval, opts.MaxPingsOut
	if opts.LeafNode.PingInterval > 0 {
		every = opts.LeafNode.PingInterval
	}
	if opts.LeafNode.MaxPingsOut > 0 {
		out = opts.LeafNode.MaxPingsOut
	}
	return time.Duration(out) * every
}

// linkMeter is the meter of this broker's link to the members: a leaf's
// tunnel's, and nil on a member, which crosses no link.
func (e *embeddedServer) linkMeter() *LinkMeter {
	if e == nil || e.tunnel == nil {
		return nil
	}
	return e.tunnel.meter
}

// readiness is the bound on the waits for the fleet to serve this broker:
// production's unless this package's own tests set [embeddedServer.ready].
func (e *embeddedServer) readiness() readiness {
	if e.ready != (readiness{}) {
		return e.ready
	}
	return readiness{timeout: clusterReadyTimeout, poll: clusterReadyPoll, ask: clusterReadyAsk}
}

// holdsStreams reports whether this process's broker is where its streams
// live — an embedded member rather than a leaf of one, or no embedded broker
// at all.
func (e *embeddedServer) holdsStreams() bool { return e != nil && !e.leaf }

// Server is an embedded broker that outlives any one client of it, so a node
// stopping does not take the broker down for its peers. Open owns both,
// because the solo topology is one process owning one broker.
type Server struct {
	embedded *embeddedServer
	cfg      Config
}

// StartServer starts an embedded broker without any client attached.
//
// The context bounds the START, not the server's life: a broker that is up
// outlives it, and stopping one is [Server.Shutdown].
func StartServer(ctx context.Context, cfg Config) (*Server, error) {
	e, err := startEmbedded(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("start embedded nats: %w", err)
	}
	return &Server{embedded: e, cfg: cfg}, nil
}

// Client connects a new queue to this server. The client does not own the
// server: stopping it leaves the broker and every peer running. opts belong to
// the client ([queue.WithNode] names the node it publishes for), because
// several nodes can be clients of one broker.
func (s *Server) Client(ctx context.Context, opts ...queue.Option) (*Queue, error) {
	return newQueueOn(ctx, s.cfg, s.embedded, false, queue.Resolve(opts...))
}

// Conn returns an in-process NATS connection to this server
// ([embeddedServer.connect]) for a caller without a queue client: a test
// exercising the coordination store, an object store or the log bridge.
//
// UNWATCHED ([watched]): NATS closing it for good is the caller's to notice.
// So a node's own coordination store rides [Queue.DialWatched] instead, whose
// close for good stops the node (ADR-0001). The caller must close it.
func (s *Server) Conn() (*nats.Conn, error) {
	if s.embedded == nil {
		return nil, errors.New("jetstream: server is shut down")
	}
	return s.embedded.connect(watched{}, s.embedded.auth.engine)
}

// LeaseConn returns an in-process connection to this server as the lease
// connection: on a leaf bound to [LeaseAccount], whose link nothing else
// rides, and to the global account elsewhere. For a caller without a queue
// client that opens the lease store as the engine does; unwatched and not
// waited for, as [Server.Conn] is. The caller must close it.
func (s *Server) LeaseConn() (*nats.Conn, error) {
	if s.embedded == nil {
		return nil, errors.New("jetstream: server is shut down")
	}
	return s.embedded.connect(watched{}, s.embedded.auth.leases)
}

// Death is this broker's own declaration that it cannot go on: closed when
// its nats-server calls Fatalf, carrying nats-server's reason. Nil once the
// server is shut down.
//
// During the boot a Fatalf fails [StartServer]; after it, the process ends, as
// stock nats-server's does: the server left the failed step's rest undone and
// carries every mailbox, ack and lease. The engine owns the process, so it
// ends it (internal/engine's watch, through the seat watchdog's exit); this
// package only declares. No nats-server v2.15.1 Fatalf is reachable after the
// boot on these options, so this is the contract for a future one.
func (s *Server) Death() *Death {
	if s.embedded == nil {
		return nil
	}
	return s.embedded.death
}

// AwaitClusterReady waits until this member can serve a stream of replicas
// copies: its JetStream current, routed to replicas-1 peers, and the metadata
// leader answering it. It is the wait [Server.Client] makes before a client
// provisions anything, and a no-op on a member with no cluster.
//
// Exported for callers of [Server.Conn], which skips that wait: a metadata
// request sent before the group has a leader is dropped, costing a whole ask
// term. Call it for every member AFTER all have started: inside [StartServer],
// a fresh cluster's first member would block the quorum it waits for.
func (s *Server) AwaitClusterReady(ctx context.Context, replicas int) error {
	if s.embedded == nil {
		return errors.New("jetstream: server is shut down")
	}
	nc, err := s.embedded.connect(watched{}, s.embedded.auth.engine)
	if err != nil {
		return fmt.Errorf("jetstream: connect to wait for the cluster: %w", err)
	}
	defer nc.Close()
	js, err := s.cfg.API().Client(nc)
	if err != nil {
		return fmt.Errorf("jetstream: open jetstream to wait for the cluster: %w", err)
	}
	return s.embedded.awaitClusterReady(ctx, js, replicas)
}

// RoutePeers names the cluster members this server currently holds a route
// to, sorted, and never itself.
//
// Test-facing: it proves a partition harness cut something, since NATS keeps
// routes open and re-dials lost ones. A partitioned member's list is empty.
//
// A dialled route is listed BEFORE the peer admits it: nats-server registers
// it on the peer's INFO, sent before the CONNECT is judged (route.go's
// createRoute and processRouteInfo, v2.15.2), so a case about refused routes
// decides on the refusals (#803). Peers, not NumRoutes: a pool of connections
// per peer plus a pinned one per system account make eight on a healthy
// three-node cluster.
func (s *Server) RoutePeers() []string {
	if s.embedded == nil {
		return nil
	}
	return s.embedded.routePeers()
}

// LeadsStream reports whether this member believes, right now, that it leads
// stream — the very predicate the server asks before it answers a request only
// a stream's leader may answer, `$JS.API.STREAM.MSG.GET` among them
// (server/jetstream_api.go, jsMsgGetRequest).
//
// Test-facing, for jetstreamtest's case holding a coordination read to never
// being answered by a member that is behind: it must know whether a cut-off
// member still believes it leads. Over the network a non-leader answers with
// silence, so "no" is a timeout a slow leader also produces; in process it is
// the server's own answer.
func (s *Server) LeadsStream(stream string) bool {
	if s.embedded == nil {
		return false
	}
	return s.embedded.ns.GlobalAccount().JetStreamIsStreamLeader(stream)
}

// routePeers is shared with [embeddedServer.awaitClusterReady], so a boot gates
// on the same reading of "who can this member reach" a partition harness
// asserts.
func (e *embeddedServer) routePeers() []string {
	rz, err := e.ns.Routez(nil)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, r := range rz.Routes {
		if r.RemoteName != "" {
			seen[r.RemoteName] = struct{}{}
		}
	}
	peers := slices.Collect(maps.Keys(seen))
	slices.Sort(peers)
	return peers
}

// AnyPort is the [Config.LeafPort] that binds the leaf listener on whichever
// port the OS hands out; [Server.LeafPort] reads it back.
//
// It is for a test whose leaves learn the member's address after it starts:
// reserving a free port and naming it leaves a window in which the rest of the
// suite can take it ([ErrLeafPortTaken]). Never a deployment's setting: Tier A
// takes a port it can name (`stream.leaf.port`, 0..65535) and refuses this.
const AnyPort = -1

// LeafPort is the port this member's LEAF listener bound, and 0 when it opened
// none. Read off Varz, the one place nats-server reports it: it offers no
// leaf listener address as it does for routes (ClusterAddr).
func (s *Server) LeafPort() int {
	if s.embedded == nil {
		return 0
	}
	varz, err := s.embedded.ns.Varz(nil)
	if err != nil || varz.LeafNode.Port < 0 {
		return 0
	}
	return varz.LeafNode.Port
}

// ClientListener is this member's client listener, as host:port on loopback,
// or "" where it opens none: a solo member, a member with only a leaf listener
// and a leaf listen on no client port at all.
//
// A clustered member opens one only because nats-server routes once it is open,
// so it binds 127.0.0.1 and is advertised to nobody ([embeddedOptions]); this
// is the only way a process on the member's host learns where. The port is the
// OS's: a fixed one would collide between members on one host.
func (s *Server) ClientListener() string {
	if s.embedded == nil {
		return ""
	}
	addr := s.embedded.ns.Addr()
	if addr == nil {
		return ""
	}
	return addr.String()
}

// Shutdown stops the broker. Every client of it should be stopped first.
//
// When it returns the server has stopped, and every writer of its store but a
// consumer's state write already in flight, which nats-server waits on for at
// most 100 ms: the caller's next step can still meet one late write
// ([shutdownAndClean]).
func (s *Server) Shutdown() {
	if s.embedded != nil {
		s.embedded.shutdown()
		s.embedded = nil
	}
}

// embeddedOptions is the nats-server option set a Config produces, together
// with the scratch directory the caller owns and must remove.
//
// Separated from the start, as dialOptions is, so options with no observable
// effect until they matter can be tested: SyncAlways shows only as acked data
// lost to a power cut, which no test can stage.
//
// auth is the server's authenticator ([brokerAuth]), built by the caller
// because the keys this process's connections sign with cannot be read back
// out of the options: it decides every connection, a clustered member's
// routes included, and declares the system account (membership.go).
func embeddedOptions(cfg Config, auth *brokerAuth) (*server.Options, string, error) {
	// The name alone: the cluster block is installed on it, so a port or peer
	// list without one starts solo. Tier A refuses that shape, and
	// [Queue.Clustered] reads the same field for the provisioning budgets, so
	// the server built and its budget cannot disagree.
	clustered := cfg.ClusterName != ""
	leaf := len(cfg.LeafURLs) > 0
	if leaf {
		// A leaf holds nothing, and every member setting is a way of holding
		// something. Refused rather than ignored: a leaf quietly given a store
		// directory is a node its operator believes is durable.
		switch {
		case clustered || len(cfg.ClusterURLs) > 0 || cfg.ClusterPort != 0:
			return nil, "", errors.New("jetstream: a leaf is not a cluster " +
				"member — drop the cluster settings or the leaf URLs")
		case cfg.LeafPort != 0:
			return nil, "", errors.New("jetstream: a leaf opens no leaf " +
				"listener of its own — other leaves join a member")
		case cfg.StoreDir != "":
			return nil, "", errors.New("jetstream: a leaf runs no JetStream, " +
				"so it has no store directory to keep")
		}
	}
	if cfg.LeafPort < 0 && cfg.LeafPort != AnyPort {
		return nil, "", fmt.Errorf("jetstream: leaf port %d is not a port — "+
			"name one in 1..65535, 0 for no listener, or AnyPort", cfg.LeafPort)
	}
	if cfg.LeafPort != 0 && cfg.StoreDir == "" {
		// A member that lets leaves in keeps everything they do, and a leaf
		// creates the streams it uses first in the FILE store. A memory-store
		// member would lose all of it on restart and refuse the leaves'
		// streams as a configuration mismatch.
		return nil, "", errors.New("jetstream: a member with a leaf listener " +
			"needs a store directory — the nodes that join it keep nothing, so " +
			"this member keeps everything they do")
	}
	// Every fleet listener is authenticated, so without a keyring the fleet
	// never forms, saying only that connections were refused; refused here
	// instead. A cluster only named, routing nowhere, needs no key.
	if cfg.FleetAuth.IsZero() && (cfg.ClusterPort != 0 || len(cfg.ClusterURLs) > 0 ||
		cfg.LeafPort != 0 || len(cfg.LeafURLs) > 0) {
		return nil, "", ErrNoFleetKeys
	}
	name := cfg.ServerName
	if name == "" {
		if clustered || leaf {
			// Refused rather than generated: a name minted at boot does not
			// survive a restart and orphans this member's replicas
			// (Config.ServerName).
			return nil, "", errors.New("jetstream: ServerName is required for a clustered embedded server or a leaf")
		}
		name = "crewlet"
	}
	opts := &server.Options{
		ServerName: name,
		JetStream:  !leaf,
		Port:       -1,
		// No listener solo: a broker with no socket is reachable by nothing
		// but this process.
		DontListen: len(cfg.ClusterURLs) == 0 && cfg.ClusterPort == 0,
		// Where routing forces one open, loopback: nats-server routes only once
		// the client listener is open (server.go Start, v2.15.1), and no
		// connection of this process dials it ([embeddedServer.connect]), so
		// it is for nobody off this host. Port -1 is the OS's: a fixed one
		// would collide between members on one host.
		Host:     "127.0.0.1",
		StoreDir: cfg.StoreDir,

		// The contract's ceiling, not nats-server's 1 MiB default
		// (queue.MaxPayloadBytes). A SERVER limit, so the client reads it off
		// INFO and refuses an oversized publish locally with an error, rather
		// than the broker closing the connection; every connection is held to
		// it when opened (carriesTheContract), so dropping this fails the boot.
		MaxPayload: queue.MaxPayloadBytes,

		// What "persisted" means (EventQueue.Publish's durability promise),
		// decided here alone. Unset, the file store
		// flushes every 2 minutes (filestore.go defaultSyncInterval), so a
		// power cut loses acked events and the coordination KV on this server.
		// A solo member's disk is its only copy, and the write rate (paced by
		// LLM turns and a 15 s heartbeat) affords the fsync. A clustered one
		// is not exempt either: a quorum survives one host's power loss, not
		// a rack's or a zone's, where it is three copies of one unflushed page
		// cache. So the operator decides. nats-server's raft WAL fsyncs
		// regardless (jetstream_cluster.go), and its async-flush gate on this
		// option is an optimisation, not a durability argument.
		SyncAlways: cfg.SyncAlways,

		// When the fsync is declined the window is named, not nats-server's
		// two-minute default nobody chose.
		SyncInterval: cfg.SyncInterval,

		// The engine owns the process signals. Left false, Server.Start's own
		// SIGINT/SIGTERM handler calls os.Exit(0) mid-drain, losing a running
		// turn and the lease release, so a peer waits out the full TTL for
		// seats a graceful shutdown should hand back at once.
		NoSigs: true,

		// How much of its volume this broker may hold, declared
		// ([Config.StoreMaxBytes]). The server option, not the account limit:
		// it bounds a reservation on both topologies (checked at create solo,
		// published as capacity for peer selection clustered); the account
		// limit reaches only the first and cannot be set to this number.
		// Zero lets nats-server derive it (three quarters of StoreDir's free
		// space plus what it occupies); [embeddedServer.budget] reports
		// either as [BudgetServerStore] for [Queue.StreamBudget].
		JetStreamMaxStore: cfg.StoreMaxBytes,

		// Every connection presents a key (package doc, "Every connection
		// presents a key"); this authenticator decides clients and leaf
		// links, which a leaf listener without its own authorization hands it.
		CustomClientAuthentication: auth,
		// And is sent a nonce to sign: with a custom authenticator the server
		// otherwise offers none (server/nkey.go nonceRequired, v2.15.1), so no
		// presented nkey could be proved.
		AlwaysEnableNonce: true,
	}
	if opts.JetStream {
		// The fleet's one domain, served by every member so a leaf's clients
		// have a JetStream across their link ([jsapi]); a member answers it as
		// it does the plain subjects, so no client knows which node it is on.
		opts.JetStreamDomain = jsapi.Domain
	}
	if leaf {
		remotes, err := leafRemotes(cfg.LeafURLs, cfg.FleetAuth.leafSeed(), cfg.LeafURLsInOrder)
		if err != nil {
			return nil, "", err
		}
		opts.LeafNode.Remotes = remotes
		// The lease link's local account, which only the lease connection
		// is bound to ([brokerAuth.bind]).
		opts.Accounts = append(opts.Accounts, server.NewAccount(LeaseAccount))
		// The leaf's clients are this process alone, and its links are the
		// ones it dials, so nothing here listens at all.
		opts.DontListen = true
		return opts, "", nil
	}
	if cfg.LeafPort != 0 {
		opts.LeafNode.Host = cfg.LeafHost
		opts.LeafNode.Port = cfg.LeafPort
		opts.LeafNode.Advertise = cfg.LeafAdvertise
		// A LEAF'S CONNECT CROSSES ITS LINK behind the member's INFO: see
		// [leafHandshakeTerm].
		opts.LeafNode.AuthTimeout = leafHandshakeTerm.Seconds()
	}
	var scratch string
	if opts.StoreDir == "" {
		// A member with no StoreDir keeps its streams in memory
		// (ensureStreams): fit for a test, never a company, since a restart
		// loses every replica. A leaf returned above.
		opts.JetStreamMaxStore = -1

		// Its metadata still needs a directory, and nats-server's default is
		// one FIXED path every process on the machine shares, so isolated
		// servers would recover each other's streams. A private directory,
		// removed on shutdown, is what "no store configured" means.
		dir, err := os.MkdirTemp("", "crewlet-jetstream-")
		if err != nil {
			return nil, "", fmt.Errorf("embedded jetstream scratch dir: %w", err)
		}
		opts.StoreDir, scratch = dir, dir
	}
	if cfg.ClusterName != "" {
		opts.Cluster = server.ClusterOpts{
			Name: cfg.ClusterName, Port: cfg.ClusterPort,
			// Pass-through, empty on a single-homed node: the engine cannot
			// tell which of a multi-homed host's addresses peers can reach.
			Host:      cfg.ClusterHost,
			Advertise: cfg.ClusterAdvertise,
			// No client is told of any member: nothing dials a member's client
			// listener, so a gossiped URL in INFO would only map the fleet's
			// listeners for whoever reads it.
			NoAdvertise: true,
		}
		// The authenticator admits a route presenting this ring's credential.
		opts.CustomRouterAuthentication = auth
		// Every route this member dials presents its own, both ways needed: in
		// each explicit URL's userinfo, since a route's CONNECT carries only its
		// URL's (route.go sendRouteConnect), and as the cluster block's
		// credentials, which a route learned by gossip takes (v2.15.1).
		username, password := cfg.FleetAuth.routeUser()
		opts.Cluster.Username, opts.Cluster.Password = username, password
		routes, err := routeURLs(cfg.ClusterURLs, username, password)
		if err != nil {
			removeScratch(scratch)
			return nil, "", err
		}
		opts.Routes = routes
		// Its system account, reachable from this process alone, is the only
		// way the metadata group's membership changes (membership.go).
		if auth.system.declared() {
			declareSystemAccount(opts)
		}
	}
	return opts, scratch, nil
}

// routeURLs parses a member's peers into the routes it dials, each carrying
// this node's route credential as its userinfo, replacing any an entry carried
// (Tier A refuses one: fleet credentials are derived). A refusal names an entry
// by position, never its text, which can carry a password.
func routeURLs(peers []string, username, password string) ([]*url.URL, error) {
	routes := make([]*url.URL, 0, len(peers))
	for i, raw := range peers {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("jetstream: cluster peer %d is not host:port behind "+
				"a nats:// scheme", i)
		}
		u.User = url.UserPassword(username, password)
		routes = append(routes, u)
	}
	return routes, nil
}

func startEmbedded(ctx context.Context, cfg Config) (*embeddedServer, error) {
	// A test's broker declares its size and keeps its store in a
	// brokertest.TempDir ([ErrUndeclaredSize], [ErrStoreOutsideTempDir]).
	if err := undeclaredSize(ctx, cfg); err != nil {
		return nil, err
	}
	if err := storeOutsideTempDir(cfg); err != nil {
		return nil, err
	}
	// The authenticator, with this start's keys ([newBrokerAuth]), the system
	// account's by the rule [embeddedOptions] reads for `clustered`.
	auth, err := newBrokerAuth(cfg)
	if err != nil {
		return nil, err
	}
	opts, scratch, err := embeddedOptions(cfg, auth)
	if err != nil {
		return nil, err
	}
	clustered := opts.Cluster.Name != ""
	metaGroup := clustered && (opts.Cluster.Port != 0 || opts.Gateway.Port != 0)

	// The route and leaf ports are probed first because the server binds them
	// LAST, after the store recovery: a taken port would cost minutes of
	// recovery before its Fatalf. The probe does not close the race; a port
	// taken after it is the Fatalf's to report ([deathError]). [AnyPort] names
	// no port to probe. Each has its own sentinel: the remedy is a different
	// setting.
	if opts.LeafNode.Port > 0 {
		free, probeErr := PortAvailable(ctx, opts.LeafNode.Host, opts.LeafNode.Port)
		switch {
		case probeErr != nil:
			removeScratch(scratch)
			return nil, fmt.Errorf("stream.leaf.port %d on %s cannot "+
				"be bound by this node: %w", opts.LeafNode.Port,
				routeHostLabel(opts.LeafNode.Host), probeErr)
		case !free:
			removeScratch(scratch)
			return nil, fmt.Errorf("%w: stream.leaf.port %d is already "+
				"in use on %s, so no stateless node could join the fleet through "+
				"this member — free that port or give this node a different one",
				ErrLeafPortTaken, opts.LeafNode.Port, routeHostLabel(opts.LeafNode.Host))
		}
	}
	if clustered && opts.Cluster.Port != 0 {
		free, probeErr := PortAvailable(ctx, opts.Cluster.Host, opts.Cluster.Port)
		switch {
		case probeErr != nil:
			// The probe could not answer (an address this host lacks, a
			// privileged port, a cancel): not "in use", which sends the
			// reader hunting for a process that does not exist.
			removeScratch(scratch)
			return nil, fmt.Errorf("stream.cluster.port %d on %s cannot be "+
				"bound by this node: %w", opts.Cluster.Port,
				routeHostLabel(opts.Cluster.Host), probeErr)
		case !free:
			removeScratch(scratch)
			return nil, fmt.Errorf("%w: stream.cluster.port %d is already in use on %s, "+
				"so this member's route listener cannot bind and it could never form "+
				"a route to a peer — free that port or give this node a different one",
				ErrRoutePortTaken, opts.Cluster.Port, routeHostLabel(opts.Cluster.Host))
		}
	}

	// The store directory is taken before the server exists ([StoreDirLock]).
	// A scratch store is this process's own, and a leaf has none.
	var dirLock *flock.Lock
	if scratch == "" && opts.StoreDir != "" {
		if dirLock, err = lockStoreDir(opts.StoreDir); err != nil {
			return nil, err
		}
	}

	// The death is made before what can declare it: the logger and a leaf's
	// tunnel ([Death]).
	death := newDeath()
	// A leaf's link dials through its tunnel, listening before the server is
	// built and closed only after it stops ([tunnel]).
	var tun *tunnel
	if len(opts.LeafNode.Remotes) > 0 {
		if tun, err = startTunnel(ctx, death); err != nil {
			dirLock.Release()
			removeScratch(scratch)
			return nil, err
		}
		tun.proxy(opts.LeafNode.Remotes)
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		tun.close()
		dirLock.Release()
		removeScratch(scratch)
		return nil, fmt.Errorf("configure embedded server: %w", err)
	}
	// Before Start, or recovery and store failures log nowhere. Trace stays
	// off (a line per protocol message); Debug is [Config.Debug], not the log
	// level. The logger is also how a Fatalf ends the waits below ([Death]).
	ns.SetLoggerV2(newNATSLogger(cfg.ServerName, death), cfg.Debug, false, false)
	// Read off the options before Start, which writes the ports it bound back
	// into them from its goroutine. A death is judged against the listeners.
	listeners := listenerSpecs{
		route: listenerSpec{port: opts.Cluster.Port, host: opts.Cluster.Host},
		leaf:  listenerSpec{port: opts.LeafNode.Port, host: opts.LeafNode.Host},
	}
	store, leaf := opts.StoreDir, len(opts.LeafNode.Remotes) > 0
	routing, servesLeaves := opts.Cluster.Port != 0, opts.LeafNode.Port != 0
	// Set before Start; read only by connections Start lets in, which the go
	// statement below orders after it.
	if err = auth.bind(ns); err != nil {
		ns.Shutdown()
		tun.close()
		dirLock.Release()
		removeScratch(scratch)
		return nil, err
	}
	// Read before Start, as the listeners are: the defaults NewServer filled
	// in are the leaf's link's own ([dataPath]).
	pings := leafPingTolerance(opts)
	// Start is joined, and every shutdown waits for it ([shutdownAndClean]).
	started := make(chan struct{})
	go func() {
		defer close(started)
		ns.Start()
	}()

	// The start, then the listeners; only the second is bounded
	// ([listenTimeout], [awaitBoot]).
	err = awaitBoot(ctx, bootWait{
		started: started, death: death,
		ready: ns.ReadyForConnections, listeners: listeners, clustered: clustered,
		timing: cfg.start.resolved(),
		slow: func(elapsed time.Duration) {
			logging.Get("queue.jetstream").Info("embedded_broker_starting",
				"server", cfg.ServerName, "store_dir", store,
				"elapsed_seconds", int(elapsed.Seconds()),
				"detail", "the embedded broker is still starting — recovering "+
					"its file store, which takes as long as the store is large, "+
					"and nats-server reports no progress of its own",
				"hint", "nothing to do while this repeats; a store that recovers "+
					"every time but slowly is recovering what it holds, and an "+
					"interrupt stops the boot once the recovery lets it")
		},
		cancelWaits: func() {
			logging.Get("queue.jetstream").Warn("embedded_start_cancel_waits",
				"server", cfg.ServerName,
				"detail", "the embedded broker is still starting — usually "+
					"recovering its store — and nats-server cannot stop a start "+
					"in progress, so this waits for it before shutting it down",
				"hint", "interrupt again to exit at once; the store recovers "+
					"again at the next start")
		},
	})
	if err != nil {
		shutdownAndClean(ns, started, scratch)
		tun.close()
		dirLock.Release()
		return nil, err
	}
	// Once a start, the keys this broker presents and accepts and where it
	// listens, so a rotation's progress reads node by node.
	auth.logKeys(openListeners(ns, routing, servesLeaves))
	return &embeddedServer{
		ns: ns, started: started, death: death,
		scratch: scratch, dirLock: dirLock, clustered: clustered, metaGroup: metaGroup, leaf: leaf,
		auth: auth, tunnel: tun, pings: pings,
	}, nil
}

// StoreDirLock is the lock file an embedded member holds inside its configured
// store directory for as long as its broker runs.
//
// nats-server takes none: a second server on one store directory is accepted
// (v2.15.1) and corrupts it silently. The node's database lock cannot stand
// in: the broker starts first, and `store.path` and `stream.store_dir` are
// independent. [startEmbedded], which every embedded member starts through,
// takes it before building the server and releases it once it has stopped; a
// second broker gets [ErrStoreDirHeld] naming the holder. Inside the directory
// because a volume root's parent may be unwritable to a non-root engine;
// nats-server reads only its `jetstream` subdirectory here.
const StoreDirLock = "crewlet-broker.lock"

// ErrStoreDirHeld reports that another broker — in another process, or another
// in this one — holds this store directory ([StoreDirLock]).
var ErrStoreDirHeld = errors.New("jetstream: the stream store directory is held by another broker")

// lockStoreDir takes dir's [StoreDirLock], or says who holds it.
func lockStoreDir(dir string) (*flock.Lock, error) {
	lock, err := flock.Try(filepath.Join(dir, StoreDirLock))
	var held *flock.HeldError
	switch {
	case errors.As(err, &held):
		return nil, fmt.Errorf("%w: stream.store_dir %s is in use by %s. Two "+
			"brokers on one store directory corrupt it, so stop that one — a "+
			"running engine, or a crewlet command reaching the fleet's store "+
			"while the engine is stopped — before starting this one",
			ErrStoreDirHeld, dir, held.Holder)
	case err != nil:
		return nil, fmt.Errorf("lock stream.store_dir %s: %w", dir, err)
	}
	return lock, nil
}

// bootWait is what [awaitBoot] waits on.
type bootWait struct {
	// started is closed when the server's Start has returned.
	started <-chan struct{}
	// death is the server's declaration that it cannot go on.
	death *Death
	// ready is the server's ReadyForConnections.
	ready func(time.Duration) bool
	// listeners and clustered are what a failure is reported against.
	listeners listenerSpecs
	clustered bool
	timing    startTiming
	// slow is told how long the start has been running, once every
	// timing.report while it is.
	slow func(elapsed time.Duration)
	// cancelWaits is told that the caller cancelled while the start was
	// still running, so the shutdown after the wait will wait for it.
	cancelWaits func()
}

// awaitBoot waits for a starting server: its Start, as long as that takes,
// then its listeners, for at most the listener budget.
//
// Besides success it ends on the server's death ([deathError], in either
// half), a Start that returned but never reported ready ([notReadyError]), or
// the context. The context ends the WAIT, never the start, which nats-server
// cannot stop: a cancel in the first half says the shutdown will wait for the
// recovery, and a second interrupt is the way out (cmd/crewlet hands the
// signals back at the first). The caller shuts down ([shutdownAndClean]).
func awaitBoot(ctx context.Context, w bootWait) error {
	begun := time.Now()
	report := time.NewTicker(w.timing.report)
	defer report.Stop()
	for running := true; running; {
		select {
		case <-w.started:
			running = false
		case <-w.death.Done():
			return deathError(w.death.Err(), w.listeners)
		case <-report.C:
			w.slow(time.Since(begun))
		case <-ctx.Done():
			// Only a start still running is announced: the select picks at
			// random between a cancel and a Start that has also returned.
			select {
			case <-w.started:
			default:
				w.cancelWaits()
			}
			return fmt.Errorf("waiting for the embedded nats server to start: %w",
				ctx.Err())
		}
	}
	// A Start can return past a Fatalf, and the select above picks at random
	// between the two, so the death is asked about first.
	if cause := w.death.Err(); cause != nil {
		return deathError(cause, w.listeners)
	}

	ready := make(chan bool, 1)
	go func() { ready <- w.ready(w.timing.listen) }()
	select {
	case ok := <-ready:
		// A death that came with readiness is still a death: the route
		// listener's Fatalf can land with the answer, and the select picks
		// at random.
		if cause := w.death.Err(); cause != nil {
			return deathError(cause, w.listeners)
		}
		if !ok {
			return notReadyError(w.timing.listen, w.clustered)
		}
		return nil
	case <-w.death.Done():
		return deathError(w.death.Err(), w.listeners)
	case <-ctx.Done():
		// Not waited for: it ends within its budget into a buffered channel.
		return fmt.Errorf("waiting for the embedded nats server's listeners: %w",
			ctx.Err())
	}
}

// notReadyError is a server whose Start returned and which then neither
// reported ready inside the listener budget nor declared itself dead.
//
// It names no listener: a failed bind is a Fatalf ([deathError]), and a port
// inferred from a listener not bound YET would misreport, say, a refused store.
func notReadyError(budget time.Duration, clustered bool) error {
	return fmt.Errorf("embedded nats server finished starting but did not "+
		"report its listeners ready within %v (clustered: %v), and declared no "+
		"failure", budget, clustered)
}

// listenerSpec is one listener a member was configured to open: its port, 0
// for none, and the address it binds.
type listenerSpec struct {
	port int
	host string
}

// listenerSpecs is the two listeners a taken port is reported against, each
// with its own sentinel and its own setting.
type listenerSpecs struct{ route, leaf listenerSpec }

// deathError is what a broker that declared itself dead during its boot means
// to whoever started it.
//
// A port lost between the probe and the bind keeps its sentinel: it is the one
// fatal a different number fixes, so harnesses retry on [ErrRoutePortTaken]
// and [ErrLeafPortTaken] and no other [ErrBrokerDied]. It is recognised from
// the error value (EADDRINUSE and the address), never the sentence. Anything
// else is nats-server's own reason, wrapped once: it names what failed better
// than a summary could.
func deathError(cause error, listeners listenerSpecs) error {
	var op *net.OpError
	if errors.Is(cause, syscall.EADDRINUSE) && errors.As(cause, &op) {
		if addr, ok := op.Addr.(*net.TCPAddr); ok {
			switch listeners.bound(addr) {
			case &listeners.route:
				return fmt.Errorf("%w: stream.cluster.port %d on %s was taken "+
					"while this member was starting, so its route listener could "+
					"not bind and it could never form a route to a peer — free "+
					"that port or give this node a different one: %w",
					ErrRoutePortTaken, listeners.route.port,
					routeHostLabel(listeners.route.host), cause)
			case &listeners.leaf:
				return fmt.Errorf("%w: stream.leaf.port %d on %s was taken "+
					"while this member was starting, so no stateless node could "+
					"join the fleet through it — free that port or give this "+
					"node a different one: %w", ErrLeafPortTaken,
					listeners.leaf.port, routeHostLabel(listeners.leaf.host), cause)
			}
		}
	}
	return fmt.Errorf("embedded nats server could not start: %w", cause)
}

// bound names which configured listener a failed bind was for, or nil.
//
// By port, and by address only where both share it (Tier A allows that only on
// addresses that cannot overlap). A host given as a name cannot be compared
// with its resolved address, so it answers nil rather than guess, and the
// caller reports nats-server's sentence, which names the port.
func (l *listenerSpecs) bound(addr *net.TCPAddr) *listenerSpec {
	var hits []*listenerSpec
	for _, spec := range []*listenerSpec{&l.route, &l.leaf} {
		if spec.port > 0 && spec.port == addr.Port {
			hits = append(hits, spec)
		}
	}
	switch len(hits) {
	case 0:
		return nil
	case 1:
		return hits[0]
	}
	for _, spec := range hits {
		if spec.host == "" && (len(addr.IP) == 0 || addr.IP.IsUnspecified()) {
			return spec
		}
		if ip := net.ParseIP(spec.host); ip != nil && ip.Equal(addr.IP) {
			return spec
		}
	}
	return nil
}

// awaitClusterReady waits until this member can serve the writes the caller is
// about to make (a no-op solo and for an external URL). Each alone failed:
//   - JetStream current: until the metadata group has a leader (~8 s), a
//     replicated create blocks rather than fails.
//   - Routed to replicas-1 peers (the replica factor, not the seed list): the
//     leader may place a stream on a member not yet routed here, whose raft
//     group never commits (`NO quorum, stalled`).
//   - The metadata leader answering: "current" is local, true with no leader
//     (raft.go isCurrent), and a non-leader drops a create for a whole
//     [jsprovision.AskTerm]. Only the leader answers the account report, asked
//     every [clusterReadyAsk] through js, the path the creates will take.
func (e *embeddedServer) awaitClusterReady(ctx context.Context, js jetstream.JetStream, replicas int) error {
	if e == nil || !e.clustered {
		return nil
	}
	r := e.readiness()
	// An R=1 stream needs no peer: the floor is zero, not a negative.
	wantPeers := max(replicas-1, 0)
	local := func() bool {
		return e.ns.JetStreamIsCurrent() && len(e.routePeers()) >= wantPeers
	}
	// answered is errNotAsked until the question is put, so a wait that never
	// asked does not report the leader silent.
	answered := errNotAsked
	ready := func() bool {
		if !local() {
			return false
		}
		askCtx, cancel := context.WithTimeout(ctx, r.ask)
		defer cancel()
		_, answered = js.AccountInfo(askCtx)
		return answered == nil
	}
	deadline := time.Now().Add(r.timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for jetstream cluster %q: %w",
				e.ns.ClusterName(), ctx.Err())
		case <-time.After(r.poll):
		}
	}
	// One last look, so the final poll's timing fails no member that got there.
	if ready() {
		return nil
	}
	// Each condition named, for its own remedy: never current is a metadata
	// group that could not form; too few peers is routing (a firewall, an
	// advertise address, a peer not started); never answered is no reachable
	// leader.
	return fmt.Errorf("embedded nats server %q is not ready to serve a "+
		"%d-replica stream within %s: jetstream current=%t, routed to %s "+
		"(want %d peers), metadata leader answered: %v", e.ns.Name(), replicas,
		r.timeout, e.ns.JetStreamIsCurrent(), words.And(e.routePeers()), wantPeers, answered)
}

// errNotAsked is the metadata question a readiness wait never got as far as
// putting, because this member was never current and routed at once.
var errNotAsked = errors.New("not asked: the member was never current with " +
	"enough routed peers")

// awaitLeafReady waits until a leaf's DATA LINK is up and the fleet's
// JetStream answers across it: the link in the global account, which the
// queue's connection rides ([awaitLinked]). A no-op for anything but a leaf.
func (e *embeddedServer) awaitLeafReady(ctx context.Context, js jetstream.JetStream) error {
	if e == nil || !e.leaf {
		return nil
	}
	return e.awaitLinked(ctx, js, server.DEFAULT_GLOBAL_ACCOUNT, "")
}

// awaitLeaseLink is [embeddedServer.awaitLeafReady] for the LEASE LINK, asked
// on js, a client over the lease connection ([Queue.DialLeases]).
func (e *embeddedServer) awaitLeaseLink(ctx context.Context, js jetstream.JetStream) error {
	if e == nil || !e.leaf {
		return nil
	}
	return e.awaitLinked(ctx, js, LeaseAccount, "lease ")
}

// awaitLinked waits until the leaf's link in account is up and the fleet's
// JetStream answers js across it; which names the link in what it says.
//
// Both: a link counts as up once accepted while the member may still be
// recovering, and only a JetStream answer proves the link. BY ACCOUNT, since a
// leaf holds two links ([leafRemotes]) and either up says nothing of the
// other. Bounded as a clustered member's wait ([clusterReadyTimeout]), since
// the far side is a cluster ([Config.Clustered]).
func (e *embeddedServer) awaitLinked(ctx context.Context, js jetstream.JetStream, account, which string) error {
	r := e.readiness()
	deadline := time.Now().Add(r.timeout)
	var last error
	for time.Now().Before(deadline) {
		if e.linked(account) {
			askCtx, cancel := context.WithTimeout(ctx, r.ask)
			_, last = js.AccountInfo(askCtx)
			cancel()
			if last == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			// The caller's deadline says nothing about the link (absent or
			// not formed YET), so no setting is named.
			return fmt.Errorf("waiting for leaf %q's %slink (account %s) to reach the "+
				"fleet: %w", e.ns.Name(), which, account, ctx.Err())
		case <-time.After(r.poll):
		}
	}
	if !e.linked(account) {
		// The key is named: a refusing member looks like an absent one from
		// here, and only the members' broker_peer_refused lines say which.
		return fmt.Errorf("leaf %q made no %slink to any member (account %s) within %s — "+
			"stream.leaf.urls names no member that is up and listening on its "+
			"stream.leaf.port, a firewall is between them, or every member "+
			"refused this node's key (it presents secrets.keys id %q; see "+
			"broker_peer_refused on the members)",
			e.ns.Name(), which, account, r.timeout, e.presentedKey())
	}
	return fmt.Errorf("leaf %q's %slink (account %s) is up but the fleet's JetStream did "+
		"not answer across it within %s: %w", e.ns.Name(), which, account, r.timeout, last)
}

// linked reports whether this leaf's link in account is up: the leaf holds a
// leaf connection bound to it. Asked by account, never by count
// ([leafRemotes]); no round trip.
func (e *embeddedServer) linked(account string) bool {
	leafz, err := e.ns.Leafz(&server.LeafzOptions{Account: account})
	return err == nil && leafz.NumLeafs > 0
}

// relink drops this leaf's link in account, so its remote dials the next of
// its URLs once the leaf's reconnect interval has passed (leafnode.go
// pickNextURL, v2.15.3): how the lease link leaves a member that answers
// nothing ([Queue.DialLeases]).
func (e *embeddedServer) relink(account string) error {
	leafz, err := e.ns.Leafz(&server.LeafzOptions{Account: account})
	if err != nil {
		return fmt.Errorf("jetstream: find the leaf's link in account %s: %w", account, err)
	}
	if len(leafz.Leafs) == 0 {
		return fmt.Errorf("jetstream: the leaf has no link in account %s to move", account)
	}
	for _, l := range leafz.Leafs {
		if err := e.ns.DisconnectClientByID(l.ID); err != nil {
			return fmt.Errorf("jetstream: drop the leaf's link in account %s: %w", account, err)
		}
	}
	return nil
}

// embeddedReconnectWait is how long a connection to this process's own broker
// waits between reconnect attempts: the external dial's second, since nothing
// about the wait differs by where the server runs.
const embeddedReconnectWait = time.Second

// embeddedReconnects is how many failed attempts in a row a connection to this
// process's own broker makes before the client closes it for good, which stops
// the node ([connectionLoss]).
//
// A push-off (slow consumer, stale connection) is undone by the first attempt,
// and success restarts the count; only a stopped broker, which nothing but the
// process restarts, runs them out. So this is how long a node with a dead
// broker stays up dark before its supervisor restarts both. That holds only
// because every attempt is at THIS member ([embeddedServer.connect]). The span
// is [listenTimeout], spent one wait at a time: a reconnect asks a started
// server what a boot does, where nats.go's default meant two minutes dark.
const embeddedReconnects = int(listenTimeout / embeddedReconnectWait)

// connect opens a client connection to this server through an in-memory pipe,
// on every topology: never slower than loopback (p50 on a three-member mesh:
// request-reply 12 µs against 19 µs, an R3 sync publish 257 µs against 309 µs)
// and needing no listener, so a member's routing listener is loopback and
// advertised to nobody ([embeddedOptions]). Its handshake timeout and
// reconnect span are [listenTimeout] ([embeddedReconnects]).
//
// Held to the contract ([carriesTheContract]) with the reconnect watch beside
// it; this and [dialWith] alone open one. at says where a close NATS makes for
// good is recorded, zero when the owner notices ([watched]); as is the identity
// it presents, the engine's or the lease connection's ([brokerAuth.bind]).
func (e *embeddedServer) connect(at watched, as processUser) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Timeout(listenTimeout),
		nats.MaxReconnects(embeddedReconnects),
		nats.ReconnectWait(embeddedReconnectWait),
		// This member and no other: a pool holding a discovered peer would
		// reconnect onto its broker, and when this member stopped the node
		// would run on with it dead instead of running out its reconnects.
		// No member advertises a peer now; this keeps it so if that changes.
		nats.IgnoreDiscoveredServers(),
		nats.ReconnectHandler(reconnectWatch{
			log: logging.Get("queue.jetstream"), embedded: true}.reconnected),
		// Async errors through the engine's logger, not raw stderr
		// ([clientErrors]).
		nats.ErrorHandler(clientErrors{log: logging.Get("queue.jetstream")}.reported),
	}
	opts = append(opts, watchClose(at, true, "")...)
	// With a key minted at this start: the authenticator admits nothing
	// that presents none, in process or not.
	opts = append(opts, nats.InProcessServer(e.ns), as.option())
	nc, err := nats.Connect("", opts...)
	if err != nil {
		return nil, err
	}
	if nc, err = heldToTheContract(nc, true); err != nil {
		return nil, err
	}
	at.track(nc)
	return nc, nil
}

// shutdown stops the server, then lets its store directory go.
//
// The lock outlives the server: released earlier, the next broker could start
// on a directory still being written (bar the late write [Server.Shutdown]
// names). A leaf's tunnel closes after its server has closed the link through
// it; closed first, it would cut a link the server re-dials into nothing.
func (e *embeddedServer) shutdown() {
	shutdownAndClean(e.ns, e.started, e.scratch)
	e.tunnel.close()
	e.dirLock.Release()
}

// stoppable is the half of a nats-server that [shutdownAndClean] drives, and
// the seam its order is tested at.
type stoppable interface {
	Shutdown()
	WaitForShutdown()
}

// shutdownAndClean waits for the server's Start to return, then stops the
// server and removes its scratch store, in that order, for every teardown.
//
// Start first: it checks for no Shutdown, so one overlapping it (only
// [startEmbedded]'s failure paths can) is undone, the store reopened and a
// deleted scratch directory made again (server.go Start, v2.15.1). Both calls:
// Shutdown returns at once if one is running, WaitForShutdown waits for it.
// Neither joins a consumer's state flush (consumerFileStore.flushLoop), which
// v2.15.1 polls for only 100 ms, so under load a write can land after this
// returns. Until a pinned nats-server joins that loop, a test's store must be
// in a brokertest.TempDir ([ErrStoreOutsideTempDir]), whose removal retries
// (crewlet/crewlet#1079); both go once it does.
func shutdownAndClean(ns stoppable, started <-chan struct{}, scratch string) {
	<-started
	ns.Shutdown()
	ns.WaitForShutdown()
	removeScratch(scratch)
}

// removeScratch deletes a scratch store directory. A failure is logged rather
// than returned: the broker is already down, and a leftover temp directory is
// not a reason to fail a shutdown the caller cannot retry.
func removeScratch(dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		logging.Get("queue.jetstream").Warn("embedded_scratch_not_removed", "dir", dir, "error", err)
	}
}

// presentedKey is the configured id of the key this broker's fleet
// credentials derive from, for a sentence that has to name it — "none" for a
// broker given no keyring.
func (e *embeddedServer) presentedKey() string {
	if e.auth == nil || e.auth.fleet.IsZero() {
		return "none"
	}
	return e.auth.fleet.activeID
}

// leafRemotes parses a leaf's URLs into its two remotes, the DATA LINK in the
// global account and the isolated LEASE LINK in [LeaseAccount] (package doc),
// each presenting seed ([FleetAuth]) and dialled in order where inOrder.
//
// ONE REMOTE WITH EVERY URL FOR EACH ACCOUNT: a remote per member would bring
// interest in by every link; a remote rotates through its own shuffled copy,
// so the links land on members independently. A second remote in the global
// account would evict the first on every connect (leafnode.go
// addLeafNodeConnection, v2.15.3: same leaf, cluster and remote account). An
// nkey, never a credential in the URL (Tier A refuses one): CONNECT signs the
// member's nonce, where a userinfo would cross in clear. The member's first
// INFO is waited for [leafHandshakeTerm].
func leafRemotes(urls []string, seed string, inOrder bool) ([]*server.RemoteLeafOpts, error) {
	parsed := func() ([]*url.URL, error) {
		out := make([]*url.URL, 0, len(urls))
		for i, raw := range urls {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err != nil || u.Host == "" {
				return nil, fmt.Errorf("jetstream: leaf URL %d is not host:port "+
					"behind a nats-leaf:// or nats:// scheme", i)
			}
			u.User = nil
			out = append(out, u)
		}
		return out, nil
	}
	data, err := parsed()
	if err != nil {
		return nil, err
	}
	leases, err := parsed()
	if err != nil {
		return nil, err
	}
	return []*server.RemoteLeafOpts{
		{URLs: data, Nkey: seed, FirstInfoTimeout: leafHandshakeTerm, NoRandomize: inOrder},
		// PAST THE TUNNEL, so it dials for itself: given the tunnel's term
		// ([tunnelDial]), since nats-server's one second leaves a lease link
		// that dropped unable to dial again behind a deep queue.
		{URLs: leases, Nkey: seed, FirstInfoTimeout: leafHandshakeTerm, DialTimeout: tunnelDial,
			LocalAccount: LeaseAccount, RequestIsolation: true, NoRandomize: inOrder},
	}, nil
}

// leafHandshakeTerm bounds a leaf link's handshake at both ends: the leaf's
// wait for the member's first INFO (FirstInfoTimeout) and the member's for the
// leaf's CONNECT (its leaf listener's AuthTimeout).
//
// A request's term ([jsapi.RequestTerm]): the handshake is two round trips,
// the leaf's wait spans three crossings and the member's four (v2.15.2), and
// each crossing queues behind the link. nats-server's own one and two seconds
// kept a leaf behind a queue deeper than about a second from relinking. The
// leaf still gives the member's answer to CONNECT a fixed two seconds
// (leafnode.go connectProcessTimeout), so no leaf links across a longer round
// trip whatever this is. The cost: a silent peer is held five seconds, not two.
const leafHandshakeTerm = jsapi.RequestTerm

// dial opens a NATS connection to an external server with this package's own
// reconnect policy, held to the transport's contract. The caller owns the
// connection and must close it; at says where a close NATS makes for good is
// recorded, and is the zero value where the caller is the one to notice — see
// [watched]. meter counts every socket the connection opens, its reconnects'
// included, as the node's data link ([LinkMeter]); nil for a dial that is not
// it.
//
// Unexported: another package reaches it through [Queue.DialOwned], which
// dials the estate the queue already proved.
func dial(cfg Config, at watched, meter *LinkMeter) (*nats.Conn, error) {
	opts, recorded, err := dialOptions(cfg, meter)
	if err != nil {
		return nil, err
	}
	return dialWith(cfg, at, opts, recorded)
}

// dialLeases opens the LEASE CONNECTION to an external server: [dial] with
// [dialLeaseOptions], unmetered, since it is not the data link the meter
// reads ([LinkMeter]).
func dialLeases(cfg Config, at watched) (*nats.Conn, error) {
	opts, recorded, err := dialLeaseOptions(cfg)
	if err != nil {
		return nil, err
	}
	return dialWith(cfg, at, opts, recorded)
}

// dialWith opens a connection to an external server with opts, which dial
// through recorded, held to the transport's contract and to a server that
// authenticates; the one dial [dial] and [dialLeases] share.
func dialWith(cfg Config, at watched, opts []nats.Option, recorded *infoDialer) (*nats.Conn, error) {
	opts = append(opts, watchClose(at, false, cfg.URL)...)
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		// By host and port, never cfg.URL, which may carry a password into
		// a boot's stderr ([serverAddresses]).
		return nil, fmt.Errorf("dial %s: %w", serverAddresses(cfg.URL), unquoted(err))
	}
	// Held to the same contract as [embeddedServer.connect].
	if nc, err = heldToTheContract(nc, false); err != nil {
		return nil, err
	}
	// Then to a server that authenticates, from the handshake's INFO. After
	// the contract, so a server short of both is refused for its max_payload,
	// as the suites staging that refusal expect.
	if nc, err = heldToAuthentication(nc, recorded, cfg.URL); err != nil {
		return nil, err
	}
	at.track(nc)
	return nc, nil
}

// externalHandshake bounds each attempt to reach an EXTERNAL server, a phase
// at a time: the TCP connect to a member (its name's addresses sharing it,
// [memberDialer]), then TLS and INFO, CONNECT, PING, PONG.
//
// A request's term ([jsapi.RequestTerm]), not the embedded [listenTimeout]:
// nothing remote has answered yet, and each handshake line queues at the
// link's bottleneck like a request, so two seconds failed every attempt behind
// a deep queue. The cost: the reconnect loop gives each silent member a dial's
// term and each wedged one a handshake's, so two dead members ahead of the
// live one take 10 to 20 s, which at the floor lease (15 s, renewed every 5 s)
// can hand its seats to a peer; the deployment guide says list live members.
const externalHandshake = jsapi.RequestTerm

// externalWriteDeadline is how long one socket Write of an external dial's
// client may take before it gives the connection up (nats.FlusherTimeout): the
// largest Write, at the slowest link pace allowed (objstore.MiBPace), ~8 min.
// The client (nats.go v1.55.0) flushes its 32 KiB write buffer, or after a
// reconnect its up-to-8 MiB reconnect buffer, as ONE Write under ONE deadline,
// and a cut Write closes the connection. At the client's minute a write over
// ~2 MiB at the floor broke the connection long before jsapi's WriteBound.
// So: one message at queue.MaxPayloadBytes behind the larger buffer, at the
// floor (jsapi.Sending). The cost: a dead connection under a large Write is
// given up only at this deadline, holding the connection's lock, so every
// caller on the queue's connection waits behind each large Write ahead of it;
// a renewal rides the lease connection, whose lock no bulk takes (ADR-0174).
var externalWriteDeadline = jsapi.Sending(
	max(nats.DefaultWriteBufSize, nats.DefaultReconnectBufSize)+queue.MaxPayloadBytes, objstore.MiBPace)

// dialOptions is the option list and the [infoDialer] it dials through, which
// keeps the INFO line [requiresAuthentication] reads. Separated from the dial
// so a test asserts what a config produces without a broker; asserting on the
// connection would test NATS's own option, behind a real TLS server.
func dialOptions(cfg Config, meter *LinkMeter) ([]nats.Option, *infoDialer, error) {
	// A custom dialer loses the client's split of its Timeout among a name's
	// addresses, so the client skips its lookup and [memberDialer] resolves
	// and shares [externalHandshake]. Every socket is the meter's.
	recorded := newInfoDialer(externalHandshake, meter)
	opts := []nats.Option{
		nats.SetCustomDialer(recorded),
		nats.SkipHostLookup(),
		nats.Name("crewlet"),
		// Reconnect forever: a broker blip must not become a node restart,
		// since coordination tells "unreachable" from "not mine" so a node
		// keeps its seats through an outage.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.Timeout(externalHandshake),
		// The largest Write at the floor, not the client's minute
		// ([externalWriteDeadline]).
		nats.FlusherTimeout(externalWriteDeadline),
		// Say so when a reconnect lands on a member holding this connection
		// to less than the contract, which a boot cannot check
		// ([reconnectWatch]).
		nats.ReconnectHandler(reconnectWatch{
			log: logging.Get("queue.jetstream")}.reconnected),
		// Async errors through the engine's logger, not raw stderr
		// ([clientErrors]).
		nats.ErrorHandler(clientErrors{log: logging.Get("queue.jetstream")}.reported),
	}
	if cfg.Credentials != "" {
		opts = append(opts, nats.UserCredentials(cfg.Credentials))
	}
	if cfg.Token != "" {
		opts = append(opts, nats.Token(cfg.Token))
	}
	// Checked here: nats.Connect's failure for an unreadable file reads as an
	// unreachable broker. The caller names the estate; this names the file
	// and the field.
	if cfg.TLS.CA != "" {
		if _, err := os.Stat(cfg.TLS.CA); err != nil {
			return nil, nil, fmt.Errorf("tls.ca %s: %w", cfg.TLS.CA, err)
		}
		opts = append(opts, nats.RootCAs(cfg.TLS.CA))
	}
	if cfg.TLS.Cert != "" {
		for field, path := range map[string]string{
			"tls.cert": cfg.TLS.Cert, "tls.key": cfg.TLS.Key,
		} {
			if _, err := os.Stat(path); err != nil {
				return nil, nil, fmt.Errorf("%s %s: %w", field, path, err)
			}
		}
		opts = append(opts, nats.ClientCert(cfg.TLS.Cert, cfg.TLS.Key))
	}
	return opts, recorded, nil
}

// leaseDeadAfter is how long the LEASE CONNECTION to an external server may
// stay silent before its client gives it up: one heartbeat at the shortest
// lease config allows, the heartbeat a renewal behind it has to make. The TTL
// in force is not known at the dial, and this bound holds at every TTL.
const leaseDeadAfter = config.MinLeaseTTL / seat.HeartbeatRatio

// leasePingsOut is how many pings the lease connection's client leaves
// unanswered before it calls the connection stale: the client's own default,
// set so [leasePingEvery] is derived from the number in force. nats.go checks
// at each ping tick (nats.go processPingTimer, v1.55.0), so a connection dies
// within (leasePingsOut + 1) intervals of its last pong.
const leasePingsOut = nats.DefaultMaxPingOut

// leasePingEvery is the lease connection's ping interval: so that
// (leasePingsOut + 1) of them are [leaseDeadAfter], 1.667 s.
const leasePingEvery = leaseDeadAfter / (leasePingsOut + 1)

// leaseConnName is the name the lease connection gives the server, so an
// operator reading its connz tells it from the queue's "crewlet".
const leaseConnName = "crewlet-leases"

// dialLeaseOptions is [dialOptions] for the LEASE CONNECTION to an external
// server ([Queue.DialLeases]): the same server, credentials, TLS, handshake
// and reconnects, unmetered, with four options of its own. Pings every
// [leasePingEvery], [leasePingsOut] of them out, so a socket that dies
// silently is given up within [leaseDeadAfter]; and the flusher held to that
// too, since this connection writes no large message (a pass's renewals at 46
// seats are 26 KB, 0.75 s at the floor) and a write stuck on a dead socket
// holds the lock the ping timer needs (the [externalWriteDeadline] doc).
func dialLeaseOptions(cfg Config) ([]nats.Option, *infoDialer, error) {
	opts, recorded, err := dialOptions(cfg, nil)
	if err != nil {
		return nil, nil, err
	}
	return append(opts,
		nats.Name(leaseConnName),
		nats.PingInterval(leasePingEvery),
		nats.MaxPingsOutstanding(leasePingsOut),
		nats.FlusherTimeout(leaseDeadAfter),
	), recorded, nil
}

// ephemeralInactive is how long the broker keeps an ephemeral consumer whose
// client has stopped asking it for anything: the live feed's
// ([Queue.SubscribeStream]) and a domain log's watch ([DomainLog.Watch]).
//
// A minute for both: long enough that a client whose connection blipped (it
// redials every second, [dialOptions]) finds its consumer, short enough that
// one whose client is gone stops costing the member memory and matching, with
// nothing having to delete it.
const ephemeralInactive = time.Minute

// SubscribeStream creates an ephemeral per-caller broadcast subscription.
//
// EVERY stream subscriber receives every matching event: the dashboard's live
// feed, not a work queue. Best-effort: no ack, a slow consumer misses messages
// rather than holding them, and anything that matters polls its own source.
func (q *Queue) SubscribeStream(ctx context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error) {
	// Not through streamFor, so it carries its own copy of the check.
	if q.isClosed() {
		return nil, ErrClosed
	}
	spec, err := specForPattern(pattern, q.cfg.EventRetention)
	if err != nil {
		return nil, err
	}
	if err = q.ensureStream(ctx, spec); err != nil {
		return nil, q.brokerFailed(ctx, err)
	}
	stream := spec.name
	consCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	q.noteConsumerProposal()
	// The caller's context bounds the create; consCtx, detached so the feed
	// outlives the call, bounds only the subscription's life.
	cons, err := q.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		FilterSubject: pattern,
		// From here on: replaying history on every browser refresh would be
		// unusable, and the durable half is answered by a query.
		DeliverPolicy: jetstream.DeliverNewPolicy,
		// No acks: a dashboard must never be able to hold a message.
		AckPolicy: jetstream.AckNonePolicy,
		// Ephemeral: the server cleans it up after the client goes away,
		// even without an Unsubscribe.
		InactiveThreshold: ephemeralInactive,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create stream consumer on %s: %w", pattern, q.brokerFailed(ctx, err))
	}

	// stopped gates dispatch beside the consume context: a message already in
	// the callback path when Unsubscribe lands must not be delivered.
	var stopped atomic.Bool
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		if stopped.Load() {
			return
		}
		var ev events.Event
		if decodeErr := json.Unmarshal(msg.Data(), &ev); decodeErr != nil {
			q.log.Warn("stream_decode_failed", "pattern", pattern, "error", decodeErr.Error())
			return
		}
		q.runStreamHandler(consCtx, h, msg.Subject(), &ev)
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("consume stream %s: %w", pattern, q.brokerFailed(ctx, err))
	}
	// Taken now, while consumption runs: it closes after the last callback
	// returns, while a Closed asked for after the subscription is gone answers
	// closed at once. For [Queue.StreamSubscriptions].
	ended := consumeCtx.Closed()
	q.mu.Lock()
	q.feeds = append(consumingFeeds(q.feeds), ended)
	q.mu.Unlock()

	var once sync.Once
	return func(context.Context) error {
		once.Do(func() {
			stopped.Store(true)
			consumeCtx.Stop()
			cancel()
		})
		return nil
	}, nil
}

// runStreamHandler isolates a stream handler's failure. A dashboard callback
// that panics must not take down the feed for every other subscriber.
func (q *Queue) runStreamHandler(ctx context.Context, h queue.StreamHandler, subject string, ev *events.Event) {
	defer func() {
		if r := recover(); r != nil {
			queue.LogStreamHandlerPanic(q.log, subject, ev, r)
		}
	}()
	h(ctx, subject, ev)
}

// ErrRoutePortTaken is a clustered member whose route port
// (`stream.cluster.port`) was held by something else.
//
// Only two places find it: the probe before the start ([PortAvailable]), and
// the bind itself for a port taken after it, whose Fatalf carries the
// EADDRINUSE [deathError] reads, so it is an [ErrBrokerDied] too. A listener
// merely not bound yet is not one ([notReadyError]).
//
// A sentinel because it is the one boot failure worth retrying with a
// different number: the e2e and jetstreamtest harnesses retry on it and
// [ErrLeafPortTaken] and no other [ErrBrokerDied], so a deterministic failure
// (a bad config, an unusable store) fails the first time.
var ErrRoutePortTaken = errors.New("cluster route port taken")

// ErrLeafPortTaken is a member whose LEAF listener's port (`stream.leaf.port`)
// was held by something else, found where [ErrRoutePortTaken] is.
//
// Its own sentinel: the listeners differ and so do the remedies, and a leaf
// port reported as a route port sends its reader to a cluster block that may
// not exist. A new number answers either, so a caller that can lose both
// matches both.
var ErrLeafPortTaken = errors.New("leaf listener port taken")

// PortAvailable reports whether a port can still be bound on host right now.
//
// A probe, not a reservation: the port is free to anyone again the instant
// it returns. It names a taken port before the server starts, rather than
// after the store recovery its bind follows ([deathError]). host empty means
// every interface, as an unset stream.cluster.host binds.
//
// Exported so jetstreamtest.PortFree runs the identical check. It answers
// (ok, err) because the caller's message depends on the failure: a cancel, a
// foreign address and a privileged port are not "in use". err is nil exactly
// when the probe ran and answered.
func PortAvailable(ctx context.Context, host string, port int) (bool, error) {
	// Cancellation is checked BEFORE the probe: Listen succeeds on a dead
	// context for a local bind, so the error path alone would report a port
	// free to a caller shutting down.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		return true, l.Close()
	}
	// In use is the one answer; any other error is handed back, not renamed.
	if errors.Is(err, syscall.EADDRINUSE) {
		return false, nil
	}
	return false, err
}

// routeHostLabel renders a cluster host for a message, naming the default
// rather than leaving a blank where an address should be.
func routeHostLabel(host string) string {
	if host == "" {
		return "every interface"
	}
	return host
}
