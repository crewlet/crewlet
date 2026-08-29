package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/pulsar"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/store"
)

// Backends is the infrastructure a node runs on.
//
// TWO SLOTS, chosen independently and validated together: the STREAM carries
// events, and COORDINATION carries seat ownership. The combinations that make
// sense are enumerated in config's topology validation — a Pulsar stream has no
// compare-and-set, so it cannot also be the coordination store, and a
// multi-node fleet cannot coordinate locally.
//
// The STORE is not a third slot in that sense — there is nothing to choose, and
// nothing to validate against the other two. It is this node's own file, a
// rebuildable index over what the replicated layer already holds, and it is
// here because its lifetime is the same lifetime: opened before anything can
// deliver, closed after everything has stopped delivering.
//
// All three are opened here and closed together, because a node holding one
// without the others is a node that can hear work it may not do, hold seats it
// cannot serve, or run turns it cannot record.
type Backends struct {
	Queue queue.EventQueue
	Coord coord.Backend

	// Fleet is the shared state beyond ownership: the notification valve's
	// counter, inbound-delivery claims, the turn-completion ledger,
	// credential cooldowns and the config activation pointer.
	//
	// Beside Coord rather than inside Store, because Store is this node's
	// LOCAL database — one file, one process — and every one of these has
	// to be agreed across the fleet to mean anything. See coord/fleet.go
	// for what each was doing while it was per-node.
	Fleet coord.Fleet

	// Store is this node's local materialized index — the third thing a
	// node runs on, and the one that is not replicated. It is opened here
	// with the other two because everything that writes to it is driven by
	// them: a node holding a queue attachment without somewhere to record
	// what a turn did is a node that works and forgets.
	Store *store.DB

	// stopServer shuts down an embedded broker this node started. Nil when
	// the stream is external — a node must never take down a broker it
	// merely dialled.
	stopServer func()

	// conn is the NATS connection the coordination store rides when it
	// shares the stream's broker. Held so Close can release it; the store
	// does not own it, because on an embedded topology the SERVER owns the
	// lifetime and closing the connection from underneath it would take
	// the stream down with the leases.
	conn *nats.Conn
}

// Close releases both slots, in the reverse order of acquisition.
//
// THE ORDER IS THE POINT and each step depends on the one before:
//
//  1. Stop the QUEUE. That closes every attachment and waits briefly for
//     in-flight handlers, so consumers release their unacked messages
//     cleanly instead of having them time out. Skipping it and going
//     straight to the server shutdown works, in the sense that the process
//     exits — and leaves every prefetched message to wait out the broker's
//     full ack timeout before a peer can have it.
//
//     NOT COVERED BY A TEST IN THIS PACKAGE, and it is worth saying so
//     rather than implying otherwise: the whole effect is on a PEER's
//     handoff latency, and in one process a shut-down server kills the
//     connection either way. What would cover it is the fleet suite —
//     two nodes, one broker, measuring how long a successor waits for a
//     departed node's messages.
//
//  2. Close the COORDINATION connection, if the store rides one. After the
//     queue, because a node that released its broker while still holding
//     leases looks alive to its peers: renewals keep succeeding against a
//     store it can no longer reach work through, and its seats stay
//     unclaimable for a full TTL.
//
//  3. Shut down the embedded SERVER, if this node started one. Last of the
//     three broker steps, because everything above it is a client of it.
//
//     Also not covered here, and for a plainer reason: a solo embedded
//     server runs with DontListen and binds no port at all, so there is no
//     socket to probe after the fact. A clustered one does bind, but a
//     single member never reaches JetStream quorum — measured, it waits
//     the full 60 s and fails — so a one-node probe cannot be stood up.
//
//  4. Close the STORE. After everything, because everything writes to it:
//     the ledgers a turn records into, the event log the queue's own writer
//     appends to. Closing it first would turn the tail of a graceful drain
//     into a run of "database is closed" — the drain would still finish,
//     and would finish having recorded none of what it drained.
//
// A node that merely DIALLED an external broker never reaches step 3: it
// must not take down a broker its peers are using.
func (b *Backends) Close(ctx context.Context) {
	if b.Queue != nil {
		if err := b.Queue.Stop(ctx); err != nil {
			log.Warn("queue_stop_failed", "error", err)
		}
	}
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	if b.stopServer != nil {
		b.stopServer()
		b.stopServer = nil
	}
	if b.Store != nil {
		if err := b.Store.Close(); err != nil {
			log.Warn("store_close_failed", "error", err)
		}
		b.Store = nil
	}
}

// OpenBackends builds everything a node runs on.
//
// It does NOT re-validate the topology: config.Bootstrap.Validate already
// refuses the incoherent combinations, and duplicating those rules here would
// give an operator two places to read and two chances to disagree. What this
// adds is the construction, and one rule validation cannot express — an
// embedded-KV coordination store rides the stream's own NATS connection, so the
// two slots are not independent at runtime even though they are in config.
//
// It takes the COMPANY as well as the bootstrap, for one field: the width of
// the vectors the configured embedding model produces. That width is Tier B
// because the model is, and the store needs it at open time — it is the only
// thing that knows how wide the packed BLOBs in its vector columns are. Passing
// it is not optional and a nil company is refused rather than defaulted,
// because the default would be silent: a store opened at the wrong width
// refuses every write from the right one, and recall would simply stop
// returning anything with nothing in the log to say why.
func OpenBackends(ctx context.Context, b *config.Bootstrap, c *config.Company) (*Backends, error) {
	if b == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}
	if c == nil {
		return nil, fmt.Errorf("engine: no company config: the store needs the " +
			"configured embedding width to open")
	}

	var out *Backends
	var err error
	switch b.Stream.Type {
	case config.StreamPulsar:
		out, err = openPulsar(ctx, b)
	case config.StreamNATS, config.StreamEmbedded, "":
		out, err = openNATS(ctx, b)
	default:
		err = fmt.Errorf("engine: unknown stream type %q", b.Stream.Type)
	}
	if err != nil {
		return nil, err
	}

	// LAST, because it is the only step whose failure has something to
	// clean up behind it. Opening the file first and then failing to reach
	// a broker would leave the node's own database open with nobody
	// holding it — and the store is exclusive to one process, so the next
	// attempt in the same process would contend with the corpse of this
	// one.
	db, err := openStore(ctx, b, c)
	if err != nil {
		out.Close(ctx)
		return nil, err
	}
	out.Store = db

	// The node's own observability, registered the moment both halves
	// exist and BEFORE anything can publish. A listener attached later
	// races the first turn a restarting node picks up off its durable
	// inbox — that turn's phases would be projected onto a dashboard and
	// missing from the record of the same seat.
	//
	// Here rather than beside the dashboard because persisting what this
	// node did is the engine's business: a worker-only node with no API
	// still keeps a record of its turns. The other half of the pipeline —
	// feeding a live projection — is the API's, and is a broadcast
	// subscription for reasons observe.Projector states.
	out.Queue.AddPublishListener(observe.NewWriter(db.Events()).Listen())
	return out, nil
}

// openStore opens this node's local database.
func openStore(ctx context.Context, b *config.Bootstrap, c *config.Company) (*store.DB, error) {
	opts := store.Options{
		Driver:       store.Driver(b.Store.Driver),
		MaxOpenConns: b.Store.MaxOpenConns,
		BusyTimeout:  time.Duration(b.Store.BusyTimeoutSeconds * float64(time.Second)),
	}
	// Nil embeddings means no vector recall is configured, which the store
	// reads as width 0 — writes carrying a vector are refused, everything
	// else works. That is the honest shape of "this company does not
	// remember by similarity", and distinct from a configured width the
	// store was never told about.
	if c.Providers.Embeddings != nil {
		opts.EmbeddingDim = c.Providers.Embeddings.Width()
	}
	db, err := store.Open(ctx, b.Store.Path, opts)
	if err != nil {
		return nil, fmt.Errorf("engine: store: %w", err)
	}
	return db, nil
}

// openNATS builds a JetStream stream, embedded or external, and the
// coordination store that may ride its connection.
func openNATS(ctx context.Context, b *config.Bootstrap) (*Backends, error) {
	cfg := jetstream.Config{
		URL:         b.Stream.URL,
		StoreDir:    b.Stream.StoreDir,
		ClusterName: b.Stream.Cluster.Name,
		ClusterURLs: b.Stream.Cluster.Peers,
		ClusterPort: b.Stream.Cluster.Port,
		ServerName:  b.Node.ID,
		Replicas:    b.Stream.Replicas,
		Credentials: b.Stream.Credentials,
		Token:       b.Stream.Token,
	}
	if b.Stream.EventRetentionHours > 0 {
		cfg.EventRetention = time.Duration(b.Stream.EventRetentionHours * float64(time.Hour))
	}

	server, err := jetstream.StartServer(cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: stream: %w", err)
	}
	q, err := server.Client(ctx)
	if err != nil {
		server.Shutdown()
		return nil, fmt.Errorf("engine: stream client: %w", err)
	}
	out := &Backends{Queue: q, stopServer: server.Shutdown}

	// The coordination store rides the STREAM'S OWN connection. A second
	// dial would work and would be worse: two connections to one broker
	// fail independently, so a node could hold live leases over a
	// connection that still works while the one carrying its inbox has
	// dropped — alive to its peers, deaf to its work.
	conn, err := server.Conn()
	if err != nil {
		out.Close(ctx)
		return nil, fmt.Errorf("engine: coordination connection: %w", err)
	}
	// THE FLEET STORE IS ALWAYS THE KV, whatever the coordination slot
	// says, and only the LEASE store follows it.
	//
	// The two answer different questions and only one of them is about
	// peers. A lease answers "who runs this seat": one node has nobody to
	// fence against and re-claims everything at boot, so an in-process
	// table is the honest implementation there. The fleet store holds
	// RECORDS — the token counter, an open agent-to-agent ask, a claimed
	// scheduled fire, a detached coding run — and every one of those has to
	// outlive the PROCESS, on one node as much as on four.
	//
	// It did not, and the consequences were silent: a company's token spend
	// reset to zero on every restart although its bucket is documented as
	// having no retention at all ("a cap is a ceiling for the life of a
	// deployment"), a redelivered trigger after a restart was worked twice,
	// and a detached sandbox run — a BILLED box — was forgotten by the
	// process that launched it. What persistence the records get is now the
	// same choice as the event log's: stream.store_dir.
	shared, err := openFleet(ctx, conn, b.Stream.Replicas)
	if err != nil {
		conn.Close()
		out.Close(ctx)
		return nil, fmt.Errorf("engine: coordination: %w", err)
	}
	out.Fleet = shared
	out.conn = conn

	if b.Coordination.Type != config.CoordinationEmbeddedKV {
		out.Coord = coordmem.New()
		return out, nil
	}
	leases, err := kv.Open(ctx, conn, kv.Config{
		TTL:      leaseTTL(b),
		Replicas: b.Stream.Replicas,
	})
	if err != nil {
		out.Close(ctx)
		return nil, fmt.Errorf("engine: coordination: %w", err)
	}
	out.Coord = leases
	return out, nil
}

// openPulsar builds a Pulsar stream and the NATS estate its leases live on.
//
// TWO ESTATES, and that is the whole shape of this topology. Pulsar has no
// compare-and-set, which is the one primitive a lease needs, so the
// coordination slot cannot be the stream — it is a NATS KV the nodes reach
// separately, either one an operator runs or a cluster they embed among
// themselves.
//
// The coordination estate is opened FIRST, so a config with nowhere to keep
// leases fails before any Pulsar client exists. The order is NOT load-bearing
// and it is worth saying so rather than implying otherwise: pulsar.Open
// connects lazily — it validates and constructs, it does not dial — so neither
// order can leave a node attached to topics it must not take work from. Both
// orders clean up after themselves, and the difference is which error an
// operator with two broken halves reads first.
func openPulsar(ctx context.Context, b *config.Bootstrap) (*Backends, error) {
	// CHECKED BEFORE EITHER DIAL. Reaching a broker only to discover the
	// topology cannot use it wastes a connection attempt and, worse,
	// reports a network failure when the problem is a configuration one —
	// an operator whose broker happens to be down would read the wrong
	// error and go fix the wrong thing.
	if b.Coordination.Type != config.CoordinationEmbeddedKV {
		return nil, fmt.Errorf("engine: a %q stream needs %q coordination: "+
			"Pulsar has no compare-and-set, so it cannot hold leases",
			config.StreamPulsar, config.CoordinationEmbeddedKV)
	}

	// Not a duplicate of the topology validation, which refuses this too.
	// An empty block is genuinely unopenable rather than merely invalid:
	// "embed a member with defaults" and "nothing was said about leases"
	// are the same value, and taking the first reading gives every node in
	// a fleet its own private in-memory lease table — so every node claims
	// every seat, and nothing anywhere reports a problem.
	if b.Coordination.NATS.IsZero() {
		return nil, fmt.Errorf("engine: a %q stream keeps its leases on a NATS "+
			"estate and this config names none: set coordination.nats.url, "+
			"or coordination.nats.cluster to embed one", config.StreamPulsar)
	}

	out := &Backends{}
	if err := openCoordinationEstate(ctx, b, out); err != nil {
		return nil, err
	}

	q, err := pulsar.Open(ctx, pulsar.Config{
		URL:               b.Stream.URL,
		Tenant:            b.Stream.Tenant,
		Namespace:         b.Stream.Namespace,
		AdminURL:          b.Stream.AdminURL,
		Token:             b.Stream.Token,
		TLSTrustCertsPath: b.Stream.TLSTrustCerts,
	})
	if err != nil {
		out.Close(ctx)
		return nil, fmt.Errorf("engine: stream: %w", err)
	}
	out.Queue = q
	return out, nil
}

// openCoordinationEstate fills the coordination slot from its own NATS estate.
//
// Only reachable on a Pulsar topology — validation refuses the block anywhere
// else, because every other stream type already carries coordination on the
// connection it is using for work.
func openCoordinationEstate(ctx context.Context, b *config.Bootstrap, out *Backends) error {
	estate := b.Coordination.NATS

	var conn *nats.Conn
	var err error
	if estate.Embedded() {
		server, serr := jetstream.StartServer(jetstream.Config{
			StoreDir:    estate.StoreDir,
			ClusterName: estate.Cluster.Name,
			ClusterURLs: estate.Cluster.Peers,
			ClusterPort: estate.Cluster.Port,
			ServerName:  b.Node.ID,
			Credentials: estate.Credentials,
			Token:       estate.Token,
		})
		if serr != nil {
			return fmt.Errorf("engine: coordination estate: %w", serr)
		}
		// Recorded before the connection is opened, so a failure below is
		// cleaned up by Close rather than leaking the server it started.
		//
		// NOT COVERED, and not coverable from here: Conn fails only when
		// the server is already shut down, which cannot be true one line
		// after starting it. The ordering is correctness by construction
		// rather than by test — kept because the alternative is a leak
		// that would only appear if that ever stopped being true.
		out.stopServer = server.Shutdown
		conn, err = server.Conn()
	} else {
		// Credentials and Token are passed through UNCOVERED. Standing up
		// a NATS server that REQUIRES either is not something this
		// package can do — StartServer's own Token is a client dial
		// option, not an authorization rule, so an embedded server always
		// accepts anyone. It is the least costly of the gaps here: an
		// operator whose credentials never arrive reads an authorization
		// violation at boot, which is loud, immediate and names itself.
		conn, err = jetstream.Dial(jetstream.Config{
			URL:         estate.URL,
			Credentials: estate.Credentials,
			Token:       estate.Token,
		})
	}
	if err != nil {
		out.Close(ctx)
		return fmt.Errorf("engine: coordination connection: %w", err)
	}
	out.conn = conn

	// Replicas is passed through UNCOVERED: a replica count above 1 only
	// takes effect against a clustered estate, and a single member never
	// reaches quorum — measured on the stream side, it waits the full 60 s
	// and fails — so a one-process test cannot tell 3 from 1. What would
	// cover it is the fleet suite, three members and one bucket.
	leases, err := kv.Open(ctx, conn, kv.Config{
		TTL:      leaseTTL(b),
		Replicas: estate.Replicas,
	})
	if err != nil {
		out.Close(ctx)
		return fmt.Errorf("engine: coordination: %w", err)
	}
	shared, err := openFleet(ctx, conn, estate.Replicas)
	if err != nil {
		out.Close(ctx)
		return fmt.Errorf("engine: coordination: %w", err)
	}
	out.Coord = leases
	out.Fleet = shared
	return nil
}

// openFleet builds the shared-state store beside the leases.
//
// The retentions are supplied HERE for the same reason leaseTTL is: each one
// is a BUCKET's age, fixed when the bucket is created, so a silent default
// would decide it at the moment nobody was looking. Every number is the one
// the subsystem that reads it already uses, named at its own package.
func openFleet(ctx context.Context, conn *nats.Conn, replicas int) (coord.Fleet, error) {
	return kv.OpenFleet(ctx, conn, kv.FleetConfig{
		RateWindow:      coord.RateWindow,
		ClaimTTL:        coord.ClaimTTL,
		LedgerRetention: coord.LedgerRetention,
		FireRetention:   coord.FireRetention,
		CooldownMax:     coord.CooldownMax,
		StatusFreshness: coord.StatusFreshness,
		Replicas:        replicas,
	})
}

// leaseTTL is the lease TTL both slots must agree on.
//
// Not free to choose independently of the coordination backend: the KV store's
// expiry is bucket-wide, so it can only honour one — and a backend that
// silently accepted a different per-call value would be lying about when a
// lease expires. That is also why the store REQUIRES it rather than defaulting
// one: a bucket created with the wrong expiry is wrong for its lifetime, and a
// silent default would decide that at the moment nobody was looking.
//
// So the default is supplied HERE, from the seat layer's own measured constant
// — 45 s, three heartbeat intervals, which tolerates two consecutive missed
// renewals with a full interval left to recover in. Shorter drops healthy
// nodes' seats on ordinary jitter, and each spurious handoff costs a real MCP
// respawn; longer is time a dead node's seats sit dark.
func leaseTTL(b *config.Bootstrap) time.Duration {
	if b.Coordination.LeaseTTLSeconds <= 0 {
		return seat.SeatLeaseTTL
	}
	return time.Duration(b.Coordination.LeaseTTLSeconds * float64(time.Second))
}
