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

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/flock"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
)

// THE TWO HALVES OF AN EMBEDDED SERVER'S START, and why only one of them is
// bounded.
//
// nats-server's Start does everything a server does before it can be used —
// recovers JetStream's file store, stands up its metadata group, opens its
// leaf and client listeners — and returns; the route listener follows from a
// goroutine Start leaves behind. So a boot waits for two things, in order:
// THE START, which on any store worth the name is its recovery, and THE
// LISTENERS after it.
//
// Not to be confused with [clusterReadyTimeout] below, which bounds a later
// wait again: these are "this server is up", that one is "this member's
// JetStream has caught up with the metadata group".
//
// # Why the recovery has no budget
//
// Because a budget on it bounds nothing and can only fail a boot that is
// working. A recovery is linear in what the store holds, and the store holds
// the company's files with no ceiling but the object collector's. Measured on
// a cold page cache on a local virtio disk, nats-server v2.15.1: 2 GiB of 1 KiB
// events across four streams recovered in 2.3 s after a clean stop and 3.0 s
// after a crash; 4 GiB of 128 KiB objects in one stream, 3.9 s and 6.3 s — a
// second or a second and a half per GiB. At the thirty seconds a solo
// start used to be given, a store of twenty or thirty GiB on that disk
// could not boot, and on a slower disk proportionally less. And it could not
// boot EVER: nats-server gives no way to stop a recovery, so the failed boot
// waited for it anyway ([shutdownAndClean] joins Start), shut down the server
// it had just recovered, and the next boot recovered the same store again.
//
// So the start is waited for as long as it takes, and is never silent about
// it: a line every [startReportEvery] says the broker is still starting and
// for how long. It stays cancellable — the first interrupt ends the boot as
// soon as the recovery lets it, a second exits at once (see [startEmbedded])
// — and the bound on a disk that never answers belongs to whatever supervises
// the process, a startup probe or a person at a terminal, which can see what
// this process cannot.
//
// # Why the listeners keep one
//
// Because by then nothing is left that takes time. Every listener is a bind,
// and a bind that fails is a Fatalf, which ends the wait at once ([Death]);
// the route listener is a goroutine away. [listenTimeout] is there for what no
// one can predict — a nats-server that stops reporting itself ready for a
// reason this package does not know of — so that it fails the boot by name
// rather than hanging it. One budget for a solo server and a clustered member,
// where there were two (thirty seconds and two minutes): the clustered one was
// larger because a member recovering its store competes for the disk with the
// peers recovering theirs, and that is the half no longer bounded.
//
// # And the handshake of every connection after it
//
// The listener budget also bounds the CLIENT HANDSHAKE a connection to this
// server makes ([embeddedServer.connect]) — INFO, CONNECT, PING, PONG with a
// broker in this very process, over an in-memory pipe. That used nats's own
// default, two seconds, which is a figure for dialling a REMOTE server whose
// silence means it is not there. This one has already answered
// ReadyForConnections, so the only thing a slow handshake here measures is the
// host's scheduler: a loaded host failed a boot with `connect nats: read pipe:
// i/o timeout` against a server that was up. A handshake cut short fails the
// queue, the coordination store or a donor's connection, while a long one only
// reports a wedged broker later — so it is the budget this package already
// gives a started server to take a connection, rather than a third number to
// keep in step: 2 s became 30 s, solo and clustered alike. (While the start was
// bounded as a whole it was that budget, thirty seconds solo and two minutes
// clustered; the two minutes were for a store recovery, which no handshake
// waits on.)
const (
	// listenTimeout bounds the listeners once Start has returned: a bind
	// each, and the route listener's goroutine being scheduled. Measured
	// with sixteen clustered members started at once under the race
	// detector on four cores: at worst 37 ms from Start returning to ready,
	// most of it nats-server's own 25 ms readiness poll. Thirty seconds is
	// a thousand times that — a host this budget fails on is not running
	// a broker — and still well inside the patience of anything watching
	// a boot.
	listenTimeout = 30 * time.Second

	// startReportEvery is how often a start still running says so. A fresh
	// store starts in milliseconds and a few gigabytes in a few seconds, so
	// a start still running at ten seconds is recovering a large store or
	// stuck on its disk — either worth a line — and ten seconds between
	// lines is short enough that a person watching a quiet boot learns why
	// before they reach for an interrupt.
	startReportEvery = 10 * time.Second
)

// startTiming is the bound on an embedded server's listeners and the interval
// between reports of a long start.
//
// AN UNSET FIELD IS PRODUCTION'S ([startTiming.resolved]). It is a value at
// all, rather than the constants alone, for the reason [readiness] is: the
// branches worth a case — a start that outlives the listener budget, a server
// that never reports ready — are reached only by a budget running out, and at
// the production numbers a case proving one waits half a minute.
type startTiming struct {
	listen, report time.Duration
}

// resolved is t with each unset field at production's value.
//
// FIELD BY FIELD, never the whole struct at once: a case that tightens only
// the listener budget would otherwise keep a zero report interval, which
// [time.NewTicker] refuses with a panic.
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
	// to become current. Measured at ~8s for a quiet three-member cluster
	// on a loopback; the generous multiple is because this is a one-time
	// boot cost and the alternative to waiting is a node that provisions
	// into a leaderless metadata group and blocks with no diagnosis.
	clusterReadyTimeout = 60 * time.Second

	// clusterReadyPoll is how often that wait re-checks. Short enough that
	// a fast local cluster is not held back by the poll itself, and it runs
	// at most a few hundred times.
	clusterReadyPoll = 100 * time.Millisecond

	// clusterReadyAsk is how long one question put to the fleet's
	// JetStream during that wait — "do you answer?" — waits for its reply
	// before it is asked again.
	//
	// [jsprovision.ReadTerm], and not merely the same number: the question is
	// a metadata READ, which the metadata leader answers when it processes it
	// and every other member drops, so it is exactly the request that term is
	// sized for, and a second constant here would be a second opinion about
	// one fact.
	clusterReadyAsk = jsprovision.ReadTerm
)

// readiness bounds the wait for the fleet to be able to serve this broker:
// how long it may take, how often it re-checks, and how long one question put
// to the fleet's JetStream waits for its answer.
//
// THE ZERO VALUE IS PRODUCTION ([embeddedServer.readiness]). It is a value at
// all, rather than the constants alone, because the branch that names what to
// fix is reached only by the WHOLE wait running out — sixty seconds — and a
// case that cannot afford to reach it ends up asserting on whatever error a
// shorter deadline of its own produced instead, which names nothing.
type readiness struct {
	timeout, poll, ask time.Duration
}

// embeddedServer is a nats-server running inside this process.
//
// The single-binary topology: no listener, no port, no service to operate.
// In a fleet the same server joins a cluster, so the difference between
// "solo" and "fleet" is cluster configuration rather than a different
// architecture — and the client code above it is identical either way.
//
// EVERY CONNECTION THIS PROCESS OPENS TO IT IS IN PROCESS, on every topology
// ([embeddedServer.connect]): through an in-memory pipe, never a socket. A
// clustered member still opens a client listener, because nats-server starts
// routing only once one is open, and it binds loopback and is advertised to
// nobody ([embeddedOptions]) — no engine connection dials it.
type embeddedServer struct {
	ns *server.Server
	// started is closed when ns.Start has returned. Every shutdown waits for
	// it; see [shutdownAndClean].
	started <-chan struct{}
	// death is the server's own declaration that it cannot go on, made by
	// its logger's Fatalf — see [Death] and [Server.Death].
	death *Death
	// scratch is a private directory this server owns and deletes on
	// shutdown, used when no StoreDir was configured. See startEmbedded.
	scratch string
	// dirLock is this server's hold on its configured StoreDir, released
	// once it has stopped writing there — see [StoreDirLock]. Nil for a
	// scratch store and a leaf, which have no directory anyone else names.
	dirLock *flock.Lock
	// clustered records whether this member has peers, which decides
	// whether anything has to wait for a metadata leader.
	clustered bool
	// leaf records a broker that joined the fleet as a LEAF: JetStream is
	// off here, so everything a stream or a bucket needs is across its link
	// rather than in this process. See [Config.LeafURLs].
	leaf bool
	// auth decides every connection to this server ([brokerAuth]), and holds
	// the keys this process's own connections present: the engine's on every
	// server, and the system account's on a clustered member — see
	// membership.go.
	auth *brokerAuth
	// ready bounds the waits for the fleet to serve this broker; zero is
	// production. Set by this package's own tests and nothing else — see
	// [readiness].
	ready readiness
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

// Server is an embedded broker that outlives any one client of it.
//
// The split matters for the same reason the in-memory twin is a broker plus
// N clients rather than one object: a node stopping must not take the broker
// down for its peers. Open is the production convenience that owns both,
// because the solo topology genuinely is one process owning one broker.
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

// Client connects a new queue to this server. The client does NOT own the
// server: stopping it leaves the broker and every peer running.
//
// opts are the contract-level settings — [queue.WithNode] names the node the
// client publishes for — and belong to the CLIENT rather than to the server,
// because several nodes can be clients of one broker.
//
// Its connections are in process, as every connection this package opens to
// an embedded broker is ([embeddedServer.connect]).
func (s *Server) Client(ctx context.Context, opts ...queue.Option) (*Queue, error) {
	return newQueueOn(ctx, s.cfg, s.embedded, false, queue.Resolve(opts...))
}

// Conn returns a NATS connection to this server, in process and held to the
// transport's contract like every connection this package opens to an embedded
// broker ([embeddedServer.connect]), for a caller that rides the broker without
// a queue client of its own: a test standing members up to exercise the
// coordination store, an object store or the broker's log bridge directly.
//
// UNWATCHED: NATS closing it for good is the caller's to notice, and no queue
// reports it as a lost node — see [watched]. That is why a node's own
// coordination store does not take its connection from here: on an embedded
// broker it rides [Queue.DialWatched], whose close for good stops the node
// rather than leaving it renewing nothing while it consumes work (ADR-0001).
//
// The caller owns the connection and must close it.
func (s *Server) Conn() (*nats.Conn, error) {
	if s.embedded == nil {
		return nil, errors.New("jetstream: server is shut down")
	}
	return s.embedded.connect(watched{})
}

// Death is this broker's own declaration that it cannot go on: closed when
// its nats-server calls Fatalf, carrying nats-server's reason. Nil once the
// server is shut down.
//
// # What it means after the boot
//
// During the boot a Fatalf fails [StartServer] itself; this is for every
// moment after it, and the answer is the one stock nats-server gives: the
// process ends. A broker that has called itself dead is not one a node can
// keep serving on — it carries every seat's mailbox, every acknowledgement and
// every lease — and nothing the server does after a Fatalf promises to
// recover, since it returned from the step that failed and left the rest
// undone. The engine owns the process, so the engine ends it
// (internal/engine's watch, through the seat watchdog's exit); this package
// only declares.
//
// None of nats-server v2.15.1's own Fatalf call sites is reachable after the
// boot on the options this package sets — every listener is bound, and so
// checked, before [StartServer] returns — so this is the contract for the
// next one rather than a path anything walks today.
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
// EXPORTED FOR WHAT [Server.Conn] SKIPS. A subsystem that rides a member's
// connection without a client — the coordination store, in a test that
// stands members up itself — gets none of that wait, and a metadata request
// it sends before the group has a leader is not refused but DROPPED: it costs
// a whole ask term before anything asks again. A caller that starts members
// waits for every one of them here, AFTER all of them have started: waited
// inside [StartServer], the first member of a fresh cluster would wait for a
// quorum its own blocking keeps from forming.
func (s *Server) AwaitClusterReady(ctx context.Context, replicas int) error {
	if s.embedded == nil {
		return errors.New("jetstream: server is shut down")
	}
	nc, err := s.embedded.connect(watched{})
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
// Test-facing, and it exists for one assertion: that a partition harness
// actually cut something. NATS keeps an established route open indefinitely
// and re-dials a lost one on its own schedule, so a harness that only stops
// listeners partitions nothing — and every fleet-failure case above it then
// passes for the wrong reason. A partitioned member's list is empty.
//
// PEERS RATHER THAN CONNECTIONS, which is the whole reason this is not
// nats-server's own NumRoutes: since 2.10 a member opens a POOL of route
// connections to each peer (three by default) plus a pinned one per
// system account, so a healthy three-node cluster reports eight routes and
// counting them says nothing a reader can check against the topology.
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
// Test-facing, for the case in jetstreamtest that holds a coordination read to
// never being answered by a member that is behind. That case has to know, of a
// member cut off from its cluster, whether it still believes it leads — a cut
// leader goes on answering until it notices it lost its quorum, and is waited
// out — and asked over the network the question has no "no": a member that
// does not lead a stream answers a request about it with silence, so "no" was
// a timeout, and a leader slow to answer on a loaded machine read as one that
// did not lead and failed the case over a read it was entitled to answer.
// Asked here, in process, it is the server's own answer and nothing else.
func (s *Server) LeadsStream(stream string) bool {
	if s.embedded == nil {
		return false
	}
	return s.embedded.ns.GlobalAccount().JetStreamIsStreamLeader(stream)
}

// routePeers is the derivation itself, shared with [embeddedServer.awaitClusterReady]
// — which gates a boot on the same number a partition harness asserts about.
// Two readings of "who can this member reach" would be two answers.
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
// port the OS hands out — nats-server's own spelling of that request — and
// [Server.LeafPort] reads back which one it was.
//
// It is for a member whose leaves are told its address AFTER it starts, which
// is every test that joins a leaf to a member. Reserving a free port and naming
// it instead means binding one, reading its number and letting it go before
// the member binds it, and anything else on the machine — usually the rest of
// this suite, starting brokers by the dozen — can take it in between: the
// member then refuses to start ([ErrLeafPortTaken]) and the case fails for a
// reason that has nothing to do with it. Bound by the OS, there is no window.
//
// Never a deployment's setting: a fleet's leaves are configured with the
// member's address before either starts, so Tier A takes a port it can name
// (`stream.leaf.port`, 0..65535) and refuses this.
const AnyPort = -1

// LeafPort is the port this member's LEAF listener bound, and 0 when it opened
// none — what a leaf dials when the member was asked for [AnyPort].
//
// READ OFF THE SERVER'S OWN REPORT (Varz), the one place nats-server says it:
// it writes the port it bound back into its options before it reports ready,
// and offers no listener address for leaves the way it does for routes
// (nats-server's ClusterAddr).
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
// A CLUSTERED MEMBER OPENS ONE ONLY BECAUSE nats-server starts routing once its
// client listener is open (server.go Start, route.go StartRouting, v2.15.1), so
// it binds 127.0.0.1 and is advertised to nobody ([embeddedOptions]): no
// connection of this process dials it ([embeddedServer.connect]), and only a
// process on this member's own host can reach it, told where by this and by
// nothing else. The port is the OS's, chosen at each start — a fixed default
// would collide between members on one host — and read back here, from the
// listener itself.
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
// WHEN IT RETURNS THE SERVER HAS STOPPED, and every writer of its store has
// stopped but one: a consumer's state write already in flight, which
// nats-server waits for for at most 100 ms. A write that a loaded machine
// stretches past that lands afterwards. So the caller's next step — a
// test's TempDir removing the store, a scratch store being deleted, a process
// exiting with its consumers' last acknowledgements on disk — can still meet
// one late write. It is rare on an idle host and measured under contention in
// [shutdownAndClean], which also says what would remove it.
func (s *Server) Shutdown() {
	if s.embedded != nil {
		s.embedded.shutdown()
		s.embedded = nil
	}
}

// embeddedOptions is the nats-server option set a Config produces, together
// with the scratch directory the caller owns and must remove.
//
// SEPARATED FROM THE START for the same reason dialOptions is, and it earns it
// twice over here: two of these options have no observable effect until the
// day they matter. A wrong MaxPayload surfaced as one oversized publish
// failing in production — every connection is now held to the contract when
// it is opened ([carriesTheContract]), so it fails the boot instead, and only
// through that check — and a wrong SyncAlways surfaces only as acked data
// missing after a host loses power, which no test can stage.
//
// auth is the server's authenticator ([brokerAuth]), built by the caller
// because the keys this process's own connections sign with cannot be read
// back out of the options: it decides every connection on every server, a
// clustered member's routes included, and declares the system account a
// clustered member's membership code reaches (see membership.go).
func embeddedOptions(cfg Config, auth *brokerAuth) (*server.Options, string, error) {
	// THE NAME, and only the name: the cluster block below is installed on
	// it alone, so a port or a peer list without one configures nothing and
	// this member starts solo. Tier A refuses that shape (it requires
	// stream.cluster.name once either is set), and [Queue.Clustered] reads
	// the same field for the provisioning budgets — one rule, so the server
	// that gets built and the budget it is given cannot disagree.
	clustered := cfg.ClusterName != ""
	leaf := len(cfg.LeafURLs) > 0
	if leaf {
		// A LEAF HOLDS NOTHING, and every member setting is a way of
		// holding something: a cluster is raft groups, a leaf listener
		// is a hub other leaves depend on, a store directory is streams.
		// Refused rather than ignored, because a leaf quietly given a
		// store directory is a node its operator believes is durable.
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
		// A MEMBER THAT LETS LEAVES IN IS WHERE EVERYTHING THEY DO IS
		// KEPT — their seats' mailboxes, every record they write — and a
		// leaf creates the streams it is first to use in the FILE store,
		// as a client of any broker it does not run does. A member keeping
		// streams in memory would lose all of it at its next restart and
		// refuse the leaves' own streams as a configuration mismatch.
		return nil, "", errors.New("jetstream: a member with a leaf listener " +
			"needs a store directory — the nodes that join it keep nothing, so " +
			"this member keeps everything they do")
	}
	// EVERY FLEET LISTENER IS AUTHENTICATED, and a broker with no keyring has
	// nothing to authenticate one with: a route or leaf listener it opened
	// would refuse every peer, and a route or a link it dialled would be
	// refused by every one — a fleet that never forms, saying only that
	// connections were refused. Refused here, before anything is built. A
	// cluster that is only named, routing nowhere, opens and dials nothing,
	// and needs no key.
	if cfg.FleetAuth.IsZero() && (cfg.ClusterPort != 0 || len(cfg.ClusterURLs) > 0 ||
		cfg.LeafPort != 0 || len(cfg.LeafURLs) > 0) {
		return nil, "", ErrNoFleetKeys
	}
	name := cfg.ServerName
	if name == "" {
		if clustered || leaf {
			// Refused rather than generated. A generated name is unique,
			// which is half the requirement — the other half is that it
			// survives a restart, and a name minted at boot silently
			// orphans this member's replicas every time the process
			// comes back. See Config.ServerName.
			return nil, "", errors.New("jetstream: ServerName is required for a clustered embedded server or a leaf")
		}
		name = "crewlet"
	}
	opts := &server.Options{
		ServerName: name,
		JetStream:  !leaf,
		Port:       -1,
		// No listener in the solo case. This is a security property as
		// much as a convenience: an embedded broker with no socket
		// cannot be reached by anything but this process.
		DontListen: len(cfg.ClusterURLs) == 0 && cfg.ClusterPort == 0,
		// AND WHERE ROUTING FORCES ONE OPEN, LOOPBACK. nats-server starts a
		// clustered member's routing only once its client listener is open
		// (server.go Start hands StartRouting a channel only AcceptLoop
		// closes, v2.15.1), so a member that routes cannot set DontListen.
		// No connection of this process dials the listener — every one is
		// in process ([embeddedServer.connect]) — so it is for nobody off
		// this host, and binding every interface, as it did, offered it to
		// anything that could reach the machine. The port stays the OS's
		// (Port: -1): a fixed one would collide between members on one host.
		Host:     "127.0.0.1",
		StoreDir: cfg.StoreDir,

		// The transport's own ceiling, from the contract's number rather
		// than nats-server's 1 MiB default — see queue.MaxPayloadBytes for
		// what that default cost. A SERVER limit rather than a per-stream
		// one, deliberately: the client reads it off the server's INFO and
		// refuses an oversized publish locally, which is what turns "this
		// delivery is too big" into an error the publisher can report
		// instead of a connection the broker closes underneath it. And
		// every connection is held to it when it is opened
		// (carriesTheContract), so an edit that dropped this line fails
		// every boot, blaming the build, rather than the first large write.
		MaxPayload: queue.MaxPayloadBytes,

		// WHAT "PERSISTED" MEANS, and the one place it is decided.
		//
		// EventQueue.Publish promises that a returned publish is durable.
		// Left unset, nats-server's file store flushes on a 2-MINUTE
		// background interval (server/filestore.go defaultSyncInterval),
		// so an acked publish survives this process crashing and does NOT
		// survive the host losing power — up to two minutes of acked
		// events, and with them the coordination KV that rides this same
		// server: the completion ledger, claimed scheduled fires, a
		// detached (billed) sandbox run, a just-written secret. Losing
		// those replays a trigger already worked and forgets a box still
		// running.
		//
		// So a SOLO member fsyncs every write. There is no peer to recover
		// from, which makes its disk the only copy, and the cost is
		// affordable at this engine's write rate: events are paced by LLM
		// turns and lease renewals by a 15-second heartbeat, not by a
		// throughput workload.
		//
		// A CLUSTERED member USED TO BE EXEMPTED HERE, on the argument
		// that the quorum is the durability: a publish returns once a
		// majority holds it, so a host that loses power loses nothing its
		// peers cannot replay. That is true of ONE failure class and the
		// inference covered five.
		//
		// A single host losing power, a kernel panic, an orderly
		// shutdown: the quorum survives all three and the exemption is
		// right. A RACK or a ZONE losing power takes a majority
		// together, and a correlated power loss takes every member —
		// and a three-node fleet on one rack, which is what a first
		// production deployment looks like, is exposed to both by
		// construction. Against those the quorum is three copies of the
		// same unflushed page cache.
		//
		// So the operator decides and the engine stops guessing.
		// nats-server's own async-flush path is gated on this too — it
		// enables it for a replicated stream only when SyncAlways is off
		// (server/filestore.go) — but that is an OPTIMISATION GATE
		// rather than an argument about durability: the raft WAL under a
		// replicated stream sets SyncAlways for itself regardless
		// (server/jetstream_cluster.go:2982-2983), which is the server
		// saying that consensus state is worth an fsync even when the
		// stream's own data is not.
		SyncAlways: cfg.SyncAlways,

		// AND WHEN THE FSYNC IS DECLINED, the window is named rather than
		// inherited. Unset, the file store flushes on a two-minute
		// interval, which is a recovery-point objective nobody chose.
		SyncInterval: cfg.SyncInterval,

		// THE ENGINE OWNS THE PROCESS SIGNALS. Left false, Server.Start
		// installs its own SIGINT/SIGTERM handler, which shuts the
		// broker down and then calls os.Exit(0) — from a library, inside
		// a process that is in the middle of something.
		//
		// Measured, end to end: SIGTERM to a solo node ran the drain and
		// the process vanished part way through Backends.Close, exit 0,
		// with the seat release done and nothing after it. A slower
		// drain — one with a turn still running, which is the case the
		// drain exists for — loses that turn and the lease release with
		// it, so a peer waits out the full TTL for seats a graceful
		// shutdown was meant to hand back immediately.
		//
		// The engine's own handler is the one that must run, and it must
		// run to completion.
		NoSigs: true,

		// HOW MUCH OF ITS OWN VOLUME THIS BROKER MAY HOLD, declared
		// rather than inferred — see [Config.StoreMaxBytes].
		//
		// THE SERVER OPTION IS THE LEVER, not the account's limit, and
		// the two are not interchangeable. This number is what bounds a
		// stream's reservation on BOTH topologies: standalone it is
		// checked directly against the create (server/jetstream.go,
		// checkBytesLimits), and clustered it is what each member
		// publishes as its own capacity for the metadata leader's peer
		// selection to subtract from. The ACCOUNT limit reaches only
		// the first of those, and cannot be set to this same number at
		// all — the server compares a limit change against the
		// difference from "unlimited", which is one byte more than the
		// limit being asked for.
		//
		// Zero leaves nats-server to derive it: three quarters of the
		// free space on StoreDir when its JetStream comes up, plus what
		// it already occupies there (server/jetstream.go,
		// dynJetStreamConfig and finalizeDynamicMaxStore). Either way
		// the number in force is readable afterwards, and declared or
		// derived is invisible to the reader: [embeddedServer.budget]
		// reports whichever it is as [BudgetServerStore], which is what
		// [Queue.StreamBudget] holds a reservation to.
		JetStreamMaxStore: cfg.StoreMaxBytes,

		// EVERY CONNECTION PRESENTS A KEY, on every server whatever its
		// topology: the package doc's "Every connection presents a key".
		// The client authenticator decides clients and leaf links (a leaf
		// listener with no authorization block of its own hands its links
		// to it), and the one posture is easier to hold than four.
		CustomClientAuthentication: auth,
		// AND EVERY CONNECTION IS SENT A NONCE to sign: with a custom
		// authenticator the server builds no nkey table, and without one
		// it offers a client no nonce at all (server/nkey.go
		// nonceRequired, v2.15.1) — so no nkey a client presents could be
		// proved. A leaf link is always sent one.
		AlwaysEnableNonce: true,
	}
	if opts.JetStream {
		// THE FLEET'S ONE DOMAIN, served by every member so a leaf's
		// clients have a JetStream to address across their link — see
		// [jsapi]. A member's own clients address it too, and its local
		// server answers the domain's subjects exactly as it answers the
		// plain ones, so no client has to know which kind of node it is
		// on.
		opts.JetStreamDomain = jsapi.Domain
	}
	if leaf {
		remotes, err := leafRemotes(cfg.LeafURLs, cfg.FleetAuth.leafSeed())
		if err != nil {
			return nil, "", err
		}
		opts.LeafNode.Remotes = remotes
		// The leaf's clients are this process alone, and its one link is
		// the one it dials, so nothing here listens at all.
		opts.DontListen = true
		return opts, "", nil
	}
	if cfg.LeafPort != 0 {
		opts.LeafNode.Host = cfg.LeafHost
		opts.LeafNode.Port = cfg.LeafPort
		opts.LeafNode.Advertise = cfg.LeafAdvertise
	}
	var scratch string
	if opts.StoreDir == "" {
		// A member with no StoreDir keeps its streams in memory (see
		// ensureStreams), which suits a test and no member serving a
		// company: a restart loses every replica it held. A leaf never
		// reaches here — it returned above with JetStream off — so this
		// branch is only ever a member's.
		opts.JetStreamMaxStore = -1

		// It still needs somewhere to put its own metadata, and left
		// empty nats-server picks a FIXED default path. That path is
		// shared by every crewlet process and every test binary on the
		// machine, so servers meant to be isolated recover each other's
		// streams: measured as KV epochs arriving at 136 in a suite that
		// had claimed a seat five times, and as a nil-pointer panic
		// inside the server's own stream recovery when two runs
		// overlapped. A private directory, removed on shutdown, is what
		// "no store configured" was always meant to mean.
		dir, err := os.MkdirTemp("", "crewlet-jetstream-")
		if err != nil {
			return nil, "", fmt.Errorf("embedded jetstream scratch dir: %w", err)
		}
		opts.StoreDir, scratch = dir, dir
	}
	if cfg.ClusterName != "" {
		opts.Cluster = server.ClusterOpts{
			Name: cfg.ClusterName, Port: cfg.ClusterPort,
			// Both empty on an ordinary single-homed node, which is why
			// they are pass-through rather than derived: the engine has
			// no way to tell which of a multi-homed host's addresses its
			// peers can reach, and guessing wrong forms a cluster with a
			// member nobody can route to.
			Host:      cfg.ClusterHost,
			Advertise: cfg.ClusterAdvertise,
			// NO CLIENT IS TOLD OF ANY MEMBER. Left false, a member gossips
			// its client listener's URL to its peers and hands every peer's
			// to each client in its INFO — on the host's external address
			// while the listener bound every interface — and a client takes
			// them into its reconnect pool. Nothing dials a member's client
			// listener (every connection of this process is in process), so
			// there is nobody to tell, and a URL in any INFO is only a map of
			// the fleet's listeners for whoever reads it.
			NoAdvertise: true,
		}
		// EVERY ROUTE IS DECIDED BY THE AUTHENTICATOR, which admits a member
		// presenting a route credential of this node's ring.
		opts.CustomRouterAuthentication = auth
		// AND EVERY ROUTE THIS MEMBER DIALS PRESENTS ITS OWN, TWO WAYS, both
		// measured as necessary: in each explicit route URL's userinfo,
		// because a route's CONNECT carries only its URL's (server/route.go
		// sendRouteConnect), and as the cluster block's username and
		// password, which nats-server gives every route it learns of by
		// gossip (route.go, an implicit route's URL takes them, v2.15.1) —
		// with the URLs alone, the route two members learn of from a third
		// never formed.
		username, password := cfg.FleetAuth.routeUser()
		opts.Cluster.Username, opts.Cluster.Password = username, password
		routes, err := routeURLs(cfg.ClusterURLs, username, password)
		if err != nil {
			removeScratch(scratch)
			return nil, "", err
		}
		opts.Routes = routes
		// AND ITS SYSTEM ACCOUNT, reachable from this process alone — the
		// only account the metadata group's membership is changed through.
		// See membership.go.
		if auth.system.declared() {
			declareSystemAccount(opts)
		}
	}
	return opts, scratch, nil
}

// routeURLs parses a member's peers into the routes it dials, each carrying
// this node's route credential as its userinfo — replacing whatever userinfo
// an entry carried, which Tier A refuses: a fleet's credentials are derived,
// never written. A member that routes nowhere (no peers) has none to carry.
//
// NAMED BY POSITION in a refusal, never by its text: an entry is a URL, and a
// URL can carry a password.
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
	// THE AUTHENTICATOR, with this start's own keys: the engine's on every
	// server, and the system account's for a clustered member — the one rule
	// [embeddedOptions] reads for `clustered`, which refuses a leaf that
	// names a cluster before anything declares it.
	auth, err := newBrokerAuth(cfg)
	if err != nil {
		return nil, err
	}
	opts, scratch, err := embeddedOptions(cfg, auth)
	if err != nil {
		return nil, err
	}
	clustered := opts.Cluster.Name != ""

	// THE ROUTE AND LEAF PORTS ARE PROBED BEFORE THE SERVER IS ASKED FOR
	// THEM, because the server binds them LAST.
	//
	// nats-server opens its leaf listener after JetStream has recovered its
	// store, and its route listener after that, from a goroutine Start
	// leaves behind. A bind that fails there is a Fatalf, and the wait below
	// ends on it with the port named — but only once the store recovery
	// before it has run, which on a large store is minutes of work thrown
	// away over a setting that could have been read in microseconds. Before
	// that Fatalf was carried at all ([Death]), a taken route port had no
	// symptom but the readiness wait expiring two minutes later and blaming
	// the network.
	//
	// The probe cannot close the race and does not try: a port taken between
	// it and the bind is the Fatalf's to report ([deathError]). Not
	// [AnyPort], which names no port to probe: the OS hands out a free one.
	// Each refused under its own sentinel, never the other's: the remedy is a
	// different setting on a different listener.
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
			// THE PROBE ITSELF COULD NOT ANSWER — the address is not one
			// this host has, the port is privileged, or the caller is
			// shutting down. Reported as what it was: calling any of
			// those "already in use" sends the reader hunting for a
			// process that does not exist.
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

	// THE STORE DIRECTORY IS TAKEN BEFORE THE SERVER EXISTS — see
	// [StoreDirLock]. A scratch store is a directory this process made for
	// itself, and a leaf has none.
	var dirLock *flock.Lock
	if scratch == "" && opts.StoreDir != "" {
		if dirLock, err = lockStoreDir(opts.StoreDir); err != nil {
			return nil, err
		}
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		dirLock.Release()
		removeScratch(scratch)
		return nil, fmt.Errorf("configure embedded server: %w", err)
	}
	// BEFORE Start, or the boot is the one stretch that logs nowhere —
	// which is where stream recovery and a failed store directory report.
	// Trace is never enabled: it is a line per protocol message, and the
	// engine publishes every event through here. Debug is the operator's
	// own answer rather than the log level's — see [Config.Debug].
	//
	// AND IT IS HOW A FATALF REACHES THIS FUNCTION: the logger declares the
	// broker's death, and the waits below end on it — see [Death].
	death := newDeath()
	ns.SetLoggerV2(newNATSLogger(cfg.ServerName, death), cfg.Debug, false, false)
	// EVERYTHING READ OFF THE OPTIONS IS READ BEFORE Start: the server
	// writes what it resolves — the ports it bound — back into these same
	// options, from Start's goroutine. The listeners are what a death is
	// judged against.
	listeners := listenerSpecs{
		route: listenerSpec{port: opts.Cluster.Port, host: opts.Cluster.Host},
		leaf:  listenerSpec{port: opts.LeafNode.Port, host: opts.LeafNode.Host},
	}
	store, leaf := opts.StoreDir, len(opts.LeafNode.Remotes) > 0
	routing, servesLeaves := opts.Cluster.Port != 0, opts.LeafNode.Port != 0
	// THE AUTHENTICATOR'S SERVER, set before Start and read only from the
	// connections Start lets in, which the go statement below orders after it.
	auth.ns = ns
	// START IS JOINED, and every shutdown of this server waits for it: see
	// [shutdownAndClean] for what a Shutdown that overlaps it leaves behind.
	started := make(chan struct{})
	go func() {
		defer close(started)
		ns.Start()
	}()

	// THE START, THEN THE LISTENERS — see [listenTimeout] for why only the
	// second is bounded, and [awaitBoot] for what ends each.
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
		dirLock.Release()
		return nil, err
	}
	// ONCE A START, the keys this broker presents and accepts and where it
	// listens: a rotation's progress reads node by node, and an operator finds
	// a member's client listener here as well as in `crewlet fleet broker list`.
	auth.logKeys(openListeners(ns, routing, servesLeaves))
	return &embeddedServer{
		ns: ns, started: started, death: death,
		scratch: scratch, dirLock: dirLock, clustered: clustered, leaf: leaf,
		auth: auth,
	}, nil
}

// StoreDirLock is the lock file an embedded member holds inside its configured
// store directory for as long as its broker runs.
//
// # Why the directory needs a lock of its own
//
// nats-server takes none. Two servers started on one store directory both
// recover it and both write it — its streams, its buckets, its metadata — and
// the second is ACCEPTED rather than refused (measured, nats-server v2.15.1),
// so the corruption is silent until a read meets it. The node's database lock
// could not stand in for this one: the engine starts its broker before it opens
// that database, and the two are independent settings (`store.path`,
// `stream.store_dir`) that no rule makes one node's.
//
// So the directory is locked here, in the one function every embedded member
// starts through — the engine's, a `crewlet` command reaching the fleet's store
// with the engine stopped, and every test that stands a broker up — before the
// server is even built, and released only once it has stopped. A second
// broker on the directory, in this process or another, is refused with
// [ErrStoreDirHeld] naming the holder, before it has read anything.
//
// INSIDE the directory rather than beside it: a store directory can be the
// root of its own volume, whose parent a non-root engine on a read-only root
// filesystem cannot write. nats-server keeps everything under its own
// `jetstream` subdirectory and reads nothing else here.
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
// THREE THINGS END IT BESIDES SUCCESS, each with its own answer: the server's
// own death ([deathError], at once, in either half), a server that returned
// from Start and never reported ready ([notReadyError]), and the caller's
// context. The context ends the WAIT, never the start — nats-server gives no
// way to stop a Start in progress — so a cancel during the first half says
// that the shutdown after it will wait for the recovery, and a second interrupt
// is the operator's way out (cmd/crewlet hands the signals back at the first).
//
// It shuts nothing down: the caller does, and the caller's shutdown waits for
// Start ([shutdownAndClean]).
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
			// ONLY A START STILL RUNNING IS WAITED FOR. A cancel and a Start
			// that has returned can both be ready when the select looks, and
			// it picks between them at random: told regardless, half of
			// those would announce a wait for a start that had finished.
			select {
			case <-w.started:
			default:
				w.cancelWaits()
			}
			return fmt.Errorf("waiting for the embedded nats server to start: %w",
				ctx.Err())
		}
	}
	// A START THAT RETURNED PAST A FATALF — early, before the steps after the
	// one that failed, or, at the one v2.15.1 call site that does not
	// return, after all of them — is asked about the death before anything
	// else: the select above picks at random between a Start that returned
	// and a death declared before it did.
	if cause := w.death.Err(); cause != nil {
		return deathError(cause, w.listeners)
	}

	ready := make(chan bool, 1)
	go func() { ready <- w.ready(w.timing.listen) }()
	select {
	case ok := <-ready:
		// A DEATH THAT CAME WITH READINESS is still a death. A Fatalf from
		// a goroutine Start left behind — the route listener's — can land
		// as the readiness answer does, and a select with both ready picks
		// one at random; asked here, the death wins whichever it picked.
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
		// The readiness goroutine is not waited for: it ends within its
		// budget, into a channel buffered for the answer nobody reads.
		return fmt.Errorf("waiting for the embedded nats server's listeners: %w",
			ctx.Err())
	}
}

// notReadyError is a server whose Start returned and which then neither
// reported ready inside the listener budget nor declared itself dead.
//
// IT NAMES NO LISTENER, because there is no evidence that one failed: every
// bind nats-server cannot make is a Fatalf, which ends the wait at once as
// [deathError]. Inferring a taken route port from a listener that was merely
// not bound YET is what reported a refused store as a port collision.
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
// A PORT SOMEBODY ELSE HOLDS keeps its sentinel. The probes before the start
// catch the ordinary case; this is the port lost in the window between the
// probe and the bind, and it is the one fatal that a different number fixes —
// which is why the harnesses retry on [ErrRoutePortTaken] and
// [ErrLeafPortTaken] and on no other [ErrBrokerDied]. It is recognised from the
// error VALUE nats-server handed its Fatalf, never from its sentence: the bind
// failure names the address it could not bind and wraps EADDRINUSE.
//
// EVERYTHING ELSE IS NATS-SERVER'S OWN REASON, wrapped once. It names what
// failed — "Can't start JetStream: storage directory is not writable" — far
// better than any summary of it could.
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
// BY PORT, and by address only where the port alone cannot tell: Tier A lets
// the two share a number only on addresses that cannot overlap, so two
// candidates are told apart by the address the bind was for. A host given as
// a name cannot be compared with the address it resolved to, and is answered
// nil rather than guessed at — the caller then reports nats-server's own
// sentence, which names the port.
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

// awaitClusterReady waits for this member to be able to serve the writes the
// caller is about to make: its JetStream caught up with the metadata group,
// AND enough peers routed to place a replicated stream.
//
// # Why the metadata group is not enough on its own
//
// Accepting connections is NOT the same as being able to serve JetStream. A
// clustered member answers its client port as soon as it is listening, while
// the metadata group takes seconds to elect a leader — measured at around
// eight on a quiet three-member cluster — and until it has one, creating a
// replicated stream BLOCKS rather than failing. A node that provisioned at
// boot therefore hung with nothing to diagnose, looking exactly like a broker
// that is up and ignoring you. That is the first half.
//
// # And why placement succeeding is not enough either
//
// The metadata group becomes current for THIS member as soon as it has caught
// up with whatever group exists — which on a fleet booting together is two of
// three. The provision that follows then asks the leader to place a stream at
// `replicas`, and the leader answers from the peer set IT knows, which can
// already include a member whose routes have not converged here. Placement
// succeeds, the stream is created, and its own raft group never commits:
// nats-server reports `NO quorum, stalled` per stream and every publish into
// it blocks for ever.
//
// That is not a hypothetical either. It is exactly what three engines starting
// at once produce, and the symptom is the worst kind — a cluster that formed,
// reported itself ready, created every stream, and cannot write to any of
// them.
//
// So the wait is on the number the WRITE actually needs: a stream of R
// replicas needs R members, which is this one plus R-1 routed peers. Derived
// from the replica factor rather than from the peer LIST, because a peer list
// is a seed set — it may name this member, it may name a subset, and neither
// shape says how many members a stream needs.
//
// # And why "current" is not "answering"
//
// JetStreamIsCurrent is a LOCAL flag: it asks whether this member's copy of
// the metadata log has commits and has applied them, and whether a leader it
// KNOWS OF has spoken lately — and a member that knows of no leader at all,
// mid-election or restarted with its log on disk, still reports itself current
// (server/raft.go, isCurrent skips the leader-contact check when there is no
// leader to have heard from). Provisioning that starts then is put to a group
// with no leader, and a group with no leader does not refuse it: only the
// leader takes a create, and every member but the leader returns WITHOUT
// REPLYING to it (server/jetstream_api.go, jsStreamCreateRequest) until the
// group is ten seconds old — so each create waits out one whole
// [jsprovision.AskTerm], fifteen seconds, and the pause before it is asked
// again.
//
// So the wait ends on an ANSWER: the fleet's JetStream account report, which
// in a cluster only the metadata leader gives (jsAccountInfoRequest returns
// silently on every other member), asked every [clusterReadyAsk] once the two
// local halves hold. An answer is the leader, reachable from this member, which
// is precisely what the provisioning that follows needs; a dropped request is
// asked again a heartbeat later rather than waited on.
//
// js is the client the provisioning will use, so the question travels the
// path the creates will.
//
// A no-op for a solo member and for an external URL: solo has no metadata
// group to join, and an external cluster is its own operator's to have made
// ready before pointing an engine at it.
func (e *embeddedServer) awaitClusterReady(ctx context.Context, js jetstream.JetStream, replicas int) error {
	if e == nil || !e.clustered {
		return nil
	}
	r := e.readiness()
	// A member of a cluster still writes R=1 streams sometimes, and one
	// replica needs no peer at all — so the floor is zero rather than a
	// negative that would read as "wait for nobody" by accident.
	wantPeers := max(replicas-1, 0)
	local := func() bool {
		return e.ns.JetStreamIsCurrent() && len(e.routePeers()) >= wantPeers
	}
	// answered is the last answer to the metadata question, and
	// errNotAsked until one is put: a wait that never got as far as asking
	// must not report the leader as silent.
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
	// ONE LAST LOOK, so a member that got there during the final poll is not
	// failed for the timing of the loop around it.
	if ready() {
		return nil
	}
	// THE THREE HALVES ARE NAMED SEPARATELY, because they have different
	// remedies: a member that never became current is one whose metadata
	// group could not form; a member that is current with too few peers is
	// a routing problem — a firewall, a wrong advertise address, a peer
	// that never started; and one that is both but was never answered is
	// a group with no leader it can reach.
	return fmt.Errorf("embedded nats server %q is not ready to serve a "+
		"%d-replica stream within %s: jetstream current=%t, routed to %v "+
		"(want %d peers), metadata leader answered: %v", e.ns.Name(), replicas,
		r.timeout, e.ns.JetStreamIsCurrent(), e.routePeers(), wantPeers, answered)
}

// errNotAsked is the metadata question a readiness wait never got as far as
// putting, because this member was never current and routed at once.
var errNotAsked = errors.New("not asked: the member was never current with " +
	"enough routed peers")

// awaitLeafReady waits until a leaf's link is up and the fleet's JetStream
// answers across it.
//
// BOTH HALVES, because either alone is a lie about the one that matters. The
// link counts as up the moment it is accepted, while the member at the other
// end may still be recovering its streams; and a JetStream answer proves the
// link rather than merely the member. Bounded by the wait a clustered member
// gets for its metadata group ([clusterReadyTimeout]), because the far side IS
// a cluster as far as this node can tell — see [Config.Clustered].
//
// A no-op for everything that is not a leaf.
func (e *embeddedServer) awaitLeafReady(ctx context.Context, js jetstream.JetStream) error {
	if e == nil || !e.leaf {
		return nil
	}
	r := e.readiness()
	deadline := time.Now().Add(r.timeout)
	var last error
	for time.Now().Before(deadline) {
		if e.ns.NumLeafNodes() > 0 {
			askCtx, cancel := context.WithTimeout(ctx, r.ask)
			_, last = js.AccountInfo(askCtx)
			cancel()
			if last == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			// THE CALLER'S DEADLINE, which says nothing about the link:
			// cut short, a wait cannot tell a member that is not there
			// from a link that has not formed YET, so naming the setting
			// to fix here would be a diagnosis it has no evidence for.
			return fmt.Errorf("waiting for leaf %q to reach the fleet: %w",
				e.ns.Name(), ctx.Err())
		case <-time.After(r.poll):
		}
	}
	if e.ns.NumLeafNodes() == 0 {
		// THE KEY IS NAMED, because a member that refuses it looks, from
		// here, exactly like a member that is not there: the link is cut
		// either way, and only the members' broker_peer_refused lines say
		// which key they turned away.
		return fmt.Errorf("leaf %q made no link to any member within %s — "+
			"stream.leaf.urls names no member that is up and listening on its "+
			"stream.leaf.port, a firewall is between them, or every member "+
			"refused this node's key (it presents secrets.keys id %q; see "+
			"broker_peer_refused on the members)",
			e.ns.Name(), r.timeout, e.presentedKey())
	}
	return fmt.Errorf("leaf %q is linked to the fleet but its JetStream did not "+
		"answer within %s: %w", e.ns.Name(), r.timeout, last)
}

// embeddedReconnectWait is how long a connection to this process's own broker
// waits between two reconnect attempts: the second the external dial waits,
// since nothing about the wait itself differs by where the server runs.
const embeddedReconnectWait = time.Second

// embeddedReconnects is how many failed attempts in a row a connection to this
// process's own broker makes before the client closes it for good — which the
// node then stops for ([connectionLoss]).
//
// # Only a stopped broker runs them out
//
// The server is in this process, so a connection to it drops for one of two
// reasons. Either the server pushed the client off — a slow consumer, a stale
// connection — which the first attempt undoes, the count starting again from
// zero on every success; or the server has stopped, which no attempt undoes,
// because nothing starts an in-process server again but the process. So the
// budget is how long a node whose own broker has stopped stays up, dark, before
// it exits for its supervisor to restart the two together.
//
// Only because every attempt is at THIS member: the dial is in process, so
// each attempt asks this server for a pipe, and no URL in the pool reaches
// another. A member advertises no peer to a client either ([embeddedOptions]
// sets Cluster.NoAdvertise), and [embeddedServer.connect] ignores what one
// discovers besides — a pool that took a peer would land the reconnect on it
// and never run out at all.
//
// # The listener budget, spent one wait at a time
//
// It is the decision [listenTimeout] already made about this server — how long
// a server that has started gets to take a connection, thirty seconds, solo or
// clustered — because a reconnect asks the same server the same question a
// boot does once the broker's start has returned. It was nats.go's own
// default, sixty attempts two seconds apart, which was nobody's decision: two
// minutes dark on a node whose broker a boot would have given thirty seconds,
// and the one number on the path to a node stopping itself that nothing here
// stated.
const embeddedReconnects = int(listenTimeout / embeddedReconnectWait)

// connect opens a client connection to this server, through an in-memory
// pipe — on every topology.
//
// # In process, a clustered member's too
//
// A clustered member's connections used to dial its client listener over
// TCP, at the URL nats-server reports for it, while a solo member and a leaf
// used the pipe. That made the listener something the engine needed: bound on
// every interface, its URL — on the host's external address — gossiped to
// every member's clients, and a connection that dropped could reconnect onto
// a peer's broker. The pipe needs none of it, and it is never the slower dial
// (measured on a three-member mesh, p50: a request and its reply 12 µs in
// process against 19 µs over the loopback, an R3 synchronous publish 257 µs
// against 309 µs). So the listener a member keeps open for routing is bound to
// loopback and advertised to nobody ([embeddedOptions]), and nothing here
// dials it.
//
// THE DIAL TAKES THE LISTENER BUDGET as its handshake timeout, not nats's
// two-second default — see the budgets' doc for why a handshake with a server
// already accepting shares it — and as the span of its reconnects
// ([embeddedReconnects]).
//
// AND EVERY CONNECTION IS HELD TO THE CONTRACT ([carriesTheContract]), with
// the reconnect watch installed beside it — this and [dial] are the only two
// places this package opens one, so the queue's own, the coordination
// store's and a donor's second connection cannot differ about it.
//
// at says where a close NATS makes for good is recorded, and is the zero value
// for a connection whose owner is the one to notice — see [watched].
func (e *embeddedServer) connect(at watched) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Timeout(listenTimeout),
		nats.MaxReconnects(embeddedReconnects),
		nats.ReconnectWait(embeddedReconnectWait),
		// THIS MEMBER AND NO OTHER. A clustered member used to advertise
		// every peer's client URL in the INFO it sends, and the client adds
		// each to its reconnect pool unless told not to: a connection that
		// dropped — a slow consumer is enough — reconnected to a random
		// member and carried this node's traffic to a peer for the rest of
		// its life, and one whose own member had STOPPED was carried on a
		// peer's broker instead of running its reconnects out, so the node
		// ran on with its member dead, the cluster a replica short, and
		// nothing saying so. In process, every attempt is a pipe to this
		// server whatever URL the pool names, and no member advertises a
		// peer to a client any more; this stays so the pool never names one
		// should the dial or the options change again. The member is in this
		// process; a connection to any other is not what this dial is for.
		nats.IgnoreDiscoveredServers(),
		nats.ReconnectHandler(reconnectWatch{
			log: logging.Get("queue.jetstream"), embedded: true}.reconnected),
		// And the client's asynchronous errors through the engine's own
		// logger rather than its default's raw stderr: see [clientErrors].
		nats.ErrorHandler(clientErrors{log: logging.Get("queue.jetstream")}.reported),
	}
	opts = append(opts, watchClose(at, true, "")...)
	// AS THIS PROCESS, with the key it minted at this start: the server's
	// authenticator admits nothing that presents none, in process or not.
	opts = append(opts, nats.InProcessServer(e.ns), e.auth.engine.option())
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
// THE LOCK OUTLIVES THE SERVER, never the reverse: released before the server
// had stopped, the next broker could start on a directory this one was still
// writing. What it cannot cover is the one write [Server.Shutdown] says can
// land after the server reports it has stopped — a consumer's state write
// already in flight — which nats-server itself gives no way to wait for.
func (e *embeddedServer) shutdown() {
	shutdownAndClean(e.ns, e.started, e.scratch)
	e.dirLock.Release()
}

// stoppable is the half of a nats-server that [shutdownAndClean] drives, and
// the seam its order is tested at.
type stoppable interface {
	Shutdown()
	WaitForShutdown()
}

// shutdownAndClean waits for the server's Start to return, then stops the
// server and removes its scratch store, IN THAT ORDER AND WAITING IN BETWEEN.
//
// START FIRST, because nats-server's Start is not safe against a Shutdown that
// overlaps it. It checks for none: it sets the flag that lets the server start
// goroutines again — the flag Shutdown cleared — and enables JetStream, which
// creates or recovers the store, whether or not a Shutdown has already run
// (server.go Start, v2.15.1). A Shutdown that lands first therefore leaves a
// server that comes up behind it, after WaitForShutdown has returned: its
// store open and written to, and, for a scratch member, a directory that
// removeScratch had just deleted made again. Only the two failure paths in
// [startEmbedded] can overlap a Start — every other caller saw the server
// ready, and Start returns right after that — so the wait costs nothing
// there, and nothing on a fresh store, whose Start takes milliseconds. On a
// large store still recovering it waits for the recovery, which nats-server
// gives no way to interrupt; [startEmbedded] says so when it happens.
//
// BOTH CALLS, because [server.Server.Shutdown] runs a shutdown to its end only
// when it is the call that starts one: if the server is already shutting down
// (its own lame-duck exit, or a second caller) it returns at once, and
// [server.Server.WaitForShutdown] is what waits for that one.
//
// WHAT EITHER WAITS FOR IS THE SERVER'S OWN GOROUTINES, and one writer of the
// store is not among them: every file-backed consumer flushes its state from
// a goroutine nats-server starts beside the consumer's store
// (consumerFileStore.flushLoop), which neither call joins. Through nats-server
// v2.15.0 a consumer stopped while that flusher was mid-write returned without
// waiting for it, so the write — `o.dat.tmp` renamed to `o.dat` under
// `obs/<consumer>` — landed after this function returned: in 90 of 1,200
// shutdowns of a member whose consumers had just been acknowledged (24
// members at once under CPU contention, the shape of this package's suite),
// and in a test whose TempDir removal then failed with "directory not empty".
// v2.15.1 makes the stop wait for a flush in flight and refuses a write after
// it, so no release before it is one to pin — but that NARROWS THE RACE AND
// DOES NOT CLOSE IT. The wait (waitOnFlusher) polls for 100 ms and then
// returns whether or not the write has finished, and a loaded machine
// stretches a write past that: on v2.15.1, with 32 such members at once on a
// real disk beside a running suite, 3 of 1,528 shutdowns still had a write in
// flight or landing after this returned (0 of 1,200 in the lighter harness
// above). With the stop joining the flush loop instead of polling for it, the
// same runs gave 0 of 1,531. That change is nats-server's to make; nothing
// here can join a goroutine the server keeps no handle to.
//
// It exists because the two shapes had drifted: the ordinary teardown waited
// and the two failure paths inside [startEmbedded] did not, which is the
// harder half to notice — those run when a boot is already going wrong, and a
// RemoveAll that raced left an error about a scratch directory on top of the
// failure the operator actually needed to read.
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

// leafRemotes parses a leaf's URLs into the one remote it dials, presenting
// seed — the active key's leaf nkey ([FleetAuth]) — on every link it makes.
//
// ONE REMOTE WITH EVERY URL rather than one remote per URL, because the fleet
// is one account on one hub: a remote per member would be N links into the
// same account, and interest would arrive by every one of them. The server
// rotates through the URLs of a single remote, which is exactly "any member
// that answers".
//
// AN NKEY, and never a credential in the URL, which Tier A refuses: the link's
// CONNECT signs the member's nonce with the seed (server/leafnode.go, v2.15.1),
// so what crosses the network is a public key and a signature. A userinfo would
// be sent beside it, in clear.
func leafRemotes(urls []string, seed string) ([]*server.RemoteLeafOpts, error) {
	parsed := make([]*url.URL, 0, len(urls))
	for i, raw := range urls {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("jetstream: leaf URL %d is not host:port "+
				"behind a nats-leaf:// or nats:// scheme", i)
		}
		u.User = nil
		parsed = append(parsed, u)
	}
	return []*server.RemoteLeafOpts{{URLs: parsed, Nkey: seed}}, nil
}

// dial opens a NATS connection to an external server with this package's own
// reconnect policy, held to the transport's contract. The caller owns the
// connection and must close it; at says where a close NATS makes for good is
// recorded, and is the zero value where the caller is the one to notice — see
// [watched].
//
// UNEXPORTED, because a caller outside this package reaches it through a
// queue: [Queue.DialOwned] is how a subsystem gets a second connection with
// the same reconnect-forever policy, and it dials the estate the queue
// already proved. An exported Dial beside it was kept for a caller that would
// otherwise reimplement the option list, and none ever existed — it was code
// with no caller, indistinguishable from code whose caller nobody had found.
func dial(cfg Config, at watched) (*nats.Conn, error) {
	opts, recorded, err := dialOptions(cfg)
	if err != nil {
		return nil, err
	}
	opts = append(opts, watchClose(at, false, cfg.URL)...)
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		// NAMED BY HOST AND PORT, never by cfg.URL itself: stream.url may
		// carry a password or a token as its userinfo, and this sentence
		// is a boot's stderr. See [serverAddresses].
		return nil, fmt.Errorf("dial %s: %w", serverAddresses(cfg.URL), unquoted(err))
	}
	// The other place a connection is opened, and held to the same
	// contract — see [embeddedServer.connect].
	if nc, err = heldToTheContract(nc, false); err != nil {
		return nil, err
	}
	// And to a server that authenticates somebody, read from the INFO the
	// connection's own handshake carried. After the contract, so a server
	// short of both is refused for its max_payload, as it was before this
	// check existed and as the suites staging that refusal expect. Every
	// connection on an external broker comes through here, the
	// coordination store's and every [Queue.DialOwned] one's included.
	if nc, err = heldToAuthentication(nc, recorded.info()); err != nil {
		return nil, err
	}
	at.track(nc)
	return nc, nil
}

// externalHandshake bounds each attempt to reach an EXTERNAL server: the dial,
// the TLS handshake and INFO, CONNECT, PING, PONG.
//
// # Two seconds, and why the embedded member's budget does not transfer
//
// A connection to this process's own member takes the LISTENER budget
// ([listenTimeout]), because that server has already answered
// ReadyForConnections and a slow handshake there measures only the host's
// contention. Nothing has answered for a remote one, and here the figure is
// also how long the reconnect loop waits on each member of the pool: it tries
// them in turn, and a member that silently drops packets — a partition, a host
// that went away without closing anything — holds every attempt at it for
// exactly this long before the next member is asked. The coordination store
// rides this connection, and an outage past the lease TTL (45 s by default)
// hands this node's seats to a peer, so each second here is a second a node
// with a healthy member in its pool spends not renewing them. Two seconds is
// NATS's own figure for a remote server, and far above the handshake of a
// reachable one; stated rather than inherited, so it is a decision a change
// to either side has to read.
const externalHandshake = 2 * time.Second

// dialOptions is the option list, separated from the dial so a test can
// assert what a config produces without a broker to connect to — and the
// [infoDialer] the list dials through, which keeps the INFO line
// [requiresAuthentication] reads.
//
// The alternative was asserting on the connection, which needs a real TLS
// server with `verify: true` and a generated certificate chain — and would
// then be testing that NATS honours its own documented option rather than
// that this package passes it.
func dialOptions(cfg Config) ([]nats.Option, *infoDialer, error) {
	// THE CLIENT'S OWN DIALER, kept: a custom one replaces it, and the
	// client applies its Timeout only to the dialer it builds. So the TCP
	// dial is bounded by the same [externalHandshake] here.
	recorded := &infoDialer{dialer: net.Dialer{Timeout: externalHandshake}}
	opts := []nats.Option{
		nats.SetCustomDialer(recorded),
		nats.Name("crewlet"),
		// Reconnect forever. A broker blip must not become a node
		// restart: the coordination layer already distinguishes
		// "unreachable" from "not mine", and a node that keeps its
		// seats through a two-second outage is the entire point of that
		// distinction.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.Timeout(externalHandshake),
		// And say so when a reconnect lands on a member that holds this
		// connection to less than the contract — the half of the dial's
		// own check a boot cannot reach. See [reconnectWatch].
		nats.ReconnectHandler(reconnectWatch{
			log: logging.Get("queue.jetstream")}.reconnected),
		// And the client's asynchronous errors through the engine's own
		// logger rather than its default's raw stderr: see [clientErrors].
		nats.ErrorHandler(clientErrors{log: logging.Get("queue.jetstream")}.reported),
	}
	if cfg.Credentials != "" {
		opts = append(opts, nats.UserCredentials(cfg.Credentials))
	}
	if cfg.Token != "" {
		opts = append(opts, nats.Token(cfg.Token))
	}
	// THE FILES ARE CHECKED HERE rather than left for nats.Connect. Its
	// own failure for an unreadable certificate arrives as a dial error,
	// which reads as "the broker is unreachable" — so an operator goes
	// looking at the network for a path that is simply not there. The
	// caller names which estate this was (engine: stream / engine:
	// coordination), so this only has to name the file and the field.
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

// ephemeralInactive is how long the broker keeps an ephemeral consumer whose
// client has stopped asking it for anything: the live feed's
// ([Queue.SubscribeStream]) and a domain log's watch ([DomainLog.Watch]).
//
// A MINUTE, and one number for both, because both strike the same bargain.
// Long enough that a client whose connection blipped — it dials again every
// second ([dialOptions]) — finds its consumer where it left it. Short enough
// that one whose client went for good (a closed browser tab, a stopped node, a
// watch that replaced a consumer it had given up on) stops costing the member
// holding it memory, and the matching of every record its stream stores,
// within a minute — with nothing having to delete it, which a client that
// crashed never does.
const ephemeralInactive = time.Minute

// SubscribeStream creates an ephemeral per-caller broadcast subscription.
//
// Unlike a durable group subscription, EVERY stream subscriber receives
// every matching event — this is the dashboard's live feed, not a work
// queue. Best-effort by design: there is no ack, a slow consumer misses
// messages rather than holding them, and the authoritative path for anything
// that matters polls its own source.
func (q *Queue) SubscribeStream(ctx context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error) {
	// The one broker-touching verb that does not go through streamFor, so
	// it carries its own copy of the same check — see there.
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
	// THE CALLER'S CONTEXT BOUNDS THE CREATE, and consCtx only the
	// subscription's life. The create ran on consCtx, which is detached
	// from ctx precisely so the feed outlives the call that opened it — so
	// a caller's deadline and cancellation bounded nothing here, and a
	// SubscribeStream against a broker that was not answering waited out
	// the client's own default however little time it had been given.
	cons, err := q.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		FilterSubject: pattern,
		// From here on, not from the beginning: a live feed that
		// replayed a month of history on every browser refresh would be
		// unusable, and the durable half of every such question is
		// answered by a query rather than by this stream.
		DeliverPolicy: jetstream.DeliverNewPolicy,
		// No acks: a dashboard must never be able to hold a message.
		AckPolicy: jetstream.AckNonePolicy,
		// Ephemeral — cleaned up by the server shortly after the client
		// goes away, even if the caller never invokes Unsubscribe.
		InactiveThreshold: ephemeralInactive,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create stream consumer on %s: %w", pattern, q.brokerFailed(ctx, err))
	}

	// stopped gates dispatch rather than relying on the consume context
	// alone: a message can already be in the client's callback path when
	// Unsubscribe lands, and a dashboard that keeps receiving after it
	// unsubscribed is a subscription that never really ended.
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
// TWO PLACES FIND IT, and nothing else may. The probe before the start
// ([PortAvailable]) finds the ordinary case. A port taken in the window
// between that probe and nats-server's bind is found by the bind itself: the
// listener's failure is a Fatalf carrying the bind's own *net.OpError, which
// names the address and wraps EADDRINUSE, and [deathError] reads that value —
// so the error is an [ErrBrokerDied] as well as this. A listener that is merely
// not bound yet is NOT one: inferring a taken port from it is what reported a
// store nats-server had refused as a port collision ([notReadyError]).
//
// A SENTINEL because it is the one boot failure that is worth RETRYING WITH A
// DIFFERENT NUMBER rather than reporting, and telling it apart from
// everything else a boot can fail on is not something a caller should do by
// matching text. The e2e and jetstreamtest harnesses retry on exactly this
// and on [ErrLeafPortTaken], and on no other [ErrBrokerDied]: every other
// death is a fact about the configuration or the host that a new port does
// not change, and a deterministic failure — a bad company config, a store
// directory the broker cannot use — must fail the first time rather than
// three times with a misleading diagnosis.
var ErrRoutePortTaken = errors.New("cluster route port taken")

// ErrLeafPortTaken is a member whose LEAF listener's port (`stream.leaf.port`)
// was held by something else: found by the probe before the start, or by the
// bind's own EADDRINUSE carried by nats-server's Fatalf when the port was
// taken after the probe — the same two places as [ErrRoutePortTaken], and an
// [ErrBrokerDied] beside this in the second.
//
// A SENTINEL OF ITS OWN, beside [ErrRoutePortTaken] rather than folded into
// it: the two name different listeners with different remedies — a member
// that cannot route is cut off from its peers, one that cannot open its leaf
// listener has nowhere for a stateless node to join — and reported as a route
// port, a taken leaf port sent its reader to a cluster block that may not
// exist. Retrying on a different number answers either, so a caller that can
// lose both matches both.
var ErrLeafPortTaken = errors.New("leaf listener port taken")

// PortAvailable reports whether a port can still be bound on host right now.
//
// A PROBE, NOT A RESERVATION: it binds, reads the answer and lets go, so the
// port is free again — and available to somebody else — the instant it
// returns. Nothing can close that window short of never releasing the port,
// and closing it is not what this is for. What it buys is a taken port named
// before the server starts: the server binds its route and leaf listeners only
// after it has recovered its store, so the same port refused by the server's
// own bind ([deathError]) is refused only once that recovery has been paid
// for.
//
// host empty means every interface, which is what an unset stream.cluster.host
// binds.
//
// EXPORTED because the test harness needs the identical check against members
// it does not start itself (see jetstreamtest.PortFree), and two spellings of
// "is this port free" is how one stops matching the other.
//
// It answers (ok, err) rather than a bool because the CALLER'S MESSAGE depends
// on which failure it was. Collapsing every listen error into "not free" made
// a cancelled start, an address this host does not have, and a privileged port
// all report themselves as a port somebody else is holding — three different
// remedies behind one sentence, and two of them send the reader to look for a
// process that does not exist. err is nil exactly when the probe ran and
// answered.
func PortAvailable(ctx context.Context, host string, port int) (bool, error) {
	// CANCELLATION IS NOT AN ANSWER about the port, and it is checked
	// BEFORE the probe rather than after: [net.ListenConfig.Listen] does
	// not refuse on a dead context for a plain local bind — measured, it
	// succeeds and hands back a listener — so a check on the error path
	// alone would report a port free to a caller that is shutting down.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		return true, l.Close()
	}
	// IN USE is the one this exists to name. Everything else — a host
	// address that is not this machine's, a port below 1024 without the
	// capability — is a configuration error of a different kind, and it is
	// handed back rather than renamed.
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
