package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/store"
)

// Backends is the infrastructure a node runs on.
//
// TWO SLOTS, chosen independently and validated together: the STREAM carries
// events, and COORDINATION carries seat ownership. The combinations that make
// sense are enumerated in config's topology validation — a multi-node fleet
// cannot coordinate locally, and a two-member fleet has no quorum.
//
// The STORE is not a third slot in that sense — there is nothing to choose, and
// nothing to validate against the other two. It is this node's own pair of
// databases, the node estate and the replicated estate beside it (see
// internal/store), and it is here because its lifetime is the same lifetime:
// opened before anything can deliver, closed after everything has stopped
// delivering.
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
	// LOCAL databases — two files, one process — and every one of these has
	// to be agreed across the fleet to mean anything. See coord/fleet.go
	// for what each was doing while it was per-node.
	Fleet coord.Fleet

	// Store is this node's two local databases: the node estate, and the
	// replicated estate a state log's applier writes. It is opened here
	// with the other two because everything that writes to it is driven by
	// them: a node holding a queue attachment without somewhere to record
	// what a turn did is a node that works and forgets.
	Store *store.DB

	// stopServer shuts down an embedded broker this node started. Nil when
	// the stream is external — a node must never take down a broker it
	// merely dialled.
	//
	// There is no connection beside it. The queue opens the broker
	// connection the coordination store rides, on every topology, and
	// owns it and closes it in Stop — see [openNATS].
	stopServer func()
}

// Complete reports which of the four a Backends lacks, or nil when it holds
// them all.
//
// [OpenBackends] never builds a partial set, so this is for the caller that
// supplies its own to [New]. A partial set used to be RUN rather than refused:
// the engine skipped whatever needed the missing piece, so an engine lent a
// queue and no store served a company with no native tracker, no conversation
// ledger and no record of its turns, and said nothing. Every node `crewlet run`
// builds holds all four, and so must every engine.
func (b *Backends) Complete() error {
	var missing []string
	if b.Queue == nil {
		missing = append(missing, "Queue")
	}
	if b.Coord == nil {
		missing = append(missing, "Coord")
	}
	if b.Fleet == nil {
		missing = append(missing, "Fleet")
	}
	if b.Store == nil {
		missing = append(missing, "Store")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("engine: the supplied Backends has no %s; open them with "+
		"OpenBackends, which builds the stream, coordination and the store together",
		strings.Join(missing, ", "))
}

// Conn is the broker connection the queue opened — the one the coordination
// store rides — when the broker is EMBEDDED in this node's process, and nil
// when this node dialled an external one.
//
// The backup snapshots the streams over it, and on the default topology it has
// no other way in: the embedded broker binds no socket. Nil is its answer for a
// dialled broker because of what the backup reads nil as — the stream estate
// belongs to a cluster somebody else runs, and is backed up there (see
// internal/backup's Options.Conn). The connection exists on that topology all
// the same; this accessor declines to name it.
//
// ASKED OF THE QUEUE rather than remembered from the open, because Queue is an
// exported slot a caller may fill with a wrapper around the one OpenBackends
// built, and a remembered connection could disagree with the one the queue in
// that slot rides. A queue that exposes no connection answers nil.
//
// The caller takes no ownership. The queue closes this connection in Stop,
// which [Backends.Close] calls, and a caller that closed it first would take
// the queue, every consumer and the coordination store down with it.
func (b *Backends) Conn() *nats.Conn {
	if b.stopServer == nil {
		return nil
	}
	broker, ok := b.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		return nil
	}
	return broker.Conn()
}

// Close releases the stream, the broker and the store, in the reverse order of
// acquisition.
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
//     It also closes the broker connection the queue opened and owns,
//     which the coordination store rides. So there is no separate
//     coordination step to order against it, and nothing here closes that
//     connection a second time.
//
//     COVERED IN PART, and it is worth saying which part. The wait is: a
//     consume loop cannot exit while its handler runs, so the queue's
//     bounded wait for its loops gives a handler still in flight time to
//     record what it drained before step 3 closes the store, and
//     TestTheStoreOutlivesTheHandlersThatWriteToIt goes red without this
//     step. The clean release of unacked messages is not: its whole effect
//     is on a PEER's handoff latency, and in one process a shut-down server
//     kills the connection either way. What would cover it is the fleet
//     suite — two nodes, one broker, measuring how long a successor waits
//     for a departed node's messages.
//
//  2. Shut down the embedded SERVER, if this node started one. After the
//     queue, because the queue is a client of it. Covered through a failed
//     open, whose cleanup is this Close:
//     TestAStoreThatCannotOpenTakesTheBrokerDownWithIt counts the
//     goroutines a server left running behind it.
//
//  3. Close the STORE. After everything, because everything writes to it:
//     the ledgers a turn records into, the event log the queue's own writer
//     appends to. Closing it first would turn the tail of a graceful drain
//     into a run of "database is closed" — the drain would still finish,
//     and would finish having recorded none of what it drained.
//
// A node that merely DIALLED an external broker never reaches step 2: it
// must not take down a broker its peers are using.
func (b *Backends) Close(ctx context.Context) {
	if b.Queue != nil {
		if err := b.Queue.Stop(ctx); err != nil {
			log.WarnContext(ctx, "queue_stop_failed", "error", err)
		}
	}
	if b.stopServer != nil {
		b.stopServer()
		b.stopServer = nil
	}
	if b.Store != nil {
		if err := b.Store.Close(); err != nil {
			log.WarnContext(ctx, "store_close_failed", "error", err)
		}
		b.Store = nil
	}
}

// OpenBackends builds everything a node runs on.
//
// It does NOT re-validate the topology: config.Bootstrap.Validate already
// refuses the incoherent combinations, and duplicating those rules here would
// give an operator two places to read and two chances to disagree. What this
// adds is the construction, and one rule validation cannot express — the
// coordination store rides the stream's own NATS connection, so the two slots
// are not independent at runtime even though they are in config.
//
// It takes the COMPANY as well as the bootstrap, for one field: the width of
// the vectors the configured embedding model produces. That width is Tier B
// because the model is, and the store wants it at open time — it is the only
// thing that knows how wide the packed BLOBs in its vector columns are.
//
// A NIL COMPANY IS A REAL CASE, not a caller's mistake: a node with no active
// revision has no company to be asked for. It opens at width 0 holding no
// rows, and learns its width from the first epoch that arrives — see
// [store.DB.LearnEmbeddingDim].
//
// This does NOT make the width mutable, and it was refused here while it was
// immutable for a reason that still holds: a store left at a wrong non-zero
// width refuses every write from the right provider, and recall stops
// returning anything with nothing in the log to say why. So it is fixed once
// non-zero, [Engine.buildEmbedder] refuses a revision that would change it,
// and only the never-told case is learnable.
//
// A node that HAS a revision does not arrive here nil: the caller reads the
// active revision first and passes it, so the store opens at the right width.
func OpenBackends(ctx context.Context, b *config.Bootstrap, c *config.Company) (*Backends, error) {
	if b == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}
	var out *Backends
	var err error
	switch b.Stream.Type {
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
		MaxOpenConns:   b.Store.MaxOpenConns,
		ReplicatedPath: b.Store.ReplicatedPath,
		BusyTimeout:    b.Store.BusyTimeout(),
		// ONE PINNED CONNECTION PER STATE-LOG DOMAIN, and nothing
		// else. Each domain's apply loop holds one for its life: it is
		// the single writer of that domain's tables, and a loop that
		// had to reacquire one per batch would be competing with the
		// readers it is applying for. The count is DECLARED rather
		// than discovered so the pool is sized for them: an undeclared
		// pin is a reader starved out of the pool by a writer that
		// never gives its connection back.
		//
		// It carried a `+ sweepWriterPins` term for the maintenance
		// worker, whose inbox sweep and duplicate-rank repair each took
		// a pin of their own and were refused on every tick of a
		// running node, the apply loops having taken every declared pin
		// before the first sweep asked. That term was the pool sized
		// for a job list in another package, holding on an invariant
		// nothing enforces — a tick runs its jobs in series, so at most
		// one pin at a time — and a third pinning job, or a tick that
		// ran two in parallel, would have under-declared it silently.
		// Neither sweep is a long-lived writer, so neither wants a pin:
		// both take a pooled write transaction now, which reaches the
		// same lock through the same queue.
		PinnedWriters: len(registeredDomains()),
	}
	// Nil embeddings means no vector recall is configured, which the store
	// reads as width 0: no DECLARED width, so it checks nothing against it
	// and a caller that has vectors anyway is not refused. That is the
	// honest shape of "this company does not remember by similarity", and
	// distinct from a configured width the store was never told about.
	//
	// A NIL COMPANY reads as the same 0, and is a real case rather than a
	// caller's mistake: a node with no active revision has no company to be
	// asked for. Such a store holds no rows and learns its width from the
	// first epoch that arrives — see [store.DB.LearnEmbeddingDim].
	if c != nil && c.Providers.Embeddings != nil {
		opts.EmbeddingDim = c.Providers.Embeddings.Width()
	}
	db, err := store.Open(ctx, b.Store.Path, opts)
	if err != nil {
		return nil, fmt.Errorf("engine: store: %w", err)
	}
	return db, nil
}

// openNATS builds a JetStream stream, embedded or external, and the
// coordination store that rides its connection.
//
// TWO BRANCHES FOR THE STREAM, ONE TAIL FOR COORDINATION. The branches differ
// only in who runs the broker; what is built on top of it is identical, and
// writing that twice is how one copy ends up without a fleet store — which is
// not a startup error but a panic hours later in whichever subsystem reached
// for it first.
//
// # One connection, on every topology
//
// The coordination store is handed the QUEUE'S OWN CONNECTION, and there is no
// other connection in this function to hand it. A second one would work and
// would be worse: two connections to one broker fail independently, so a node
// could hold live leases over a connection that still works while the one
// carrying its inbox has dropped — alive to its peers, deaf to its work. That
// holds on the embedded broker as much as on an external one: a clustered
// member is reached over a TCP connection to its own client port and a solo
// one over an in-process pipe, and one such connection can close while another
// to the same server stays open.
func openNATS(ctx context.Context, b *config.Bootstrap) (*Backends, error) {
	// THROUGH THE RESOLVER, never the raw field. `node.id` is only one of
	// the three places a node's name comes from — the file, then
	// CREWLET_NODE_ID, then the default — and the raw field is empty for
	// the shape a container orchestrator actually uses, which is to inject
	// the variable and leave the key out.
	//
	// Empty is not a harmless default here. JetStream places replicas BY
	// SERVER NAME, so this value is the member's identity in the cluster:
	// a clustered member with no name is REFUSED at boot (see
	// jetstream.startEmbedded), which made the documented fleet shape fail
	// to start, and a solo one silently fell back to the literal
	// "crewlet", quietly detaching the broker's identity from the node's.
	nodeID, err := config.ResolveNodeID(b, nil)
	if err != nil {
		return nil, fmt.Errorf("engine: stream: %w", err)
	}
	cfg := jetstream.Config{
		URL:              b.Stream.URL,
		StoreDir:         b.Stream.StoreDir,
		ClusterName:      b.Stream.Cluster.Name,
		ClusterURLs:      b.Stream.Cluster.Peers,
		ClusterPort:      b.Stream.Cluster.Port,
		ClusterHost:      b.Stream.Cluster.Host,
		ClusterAdvertise: b.Stream.Cluster.Advertise,
		ServerName:       nodeID,
		Replicas:         b.Stream.Replicas,
		Credentials:      b.Stream.Credentials,
		Token:            b.Stream.Token,
		TLS:              streamTLS(b.Stream.TLS),

		// The BROKER's own verbosity, which is not the engine's — see
		// jetstream.Config.Debug. Read by the embedded branch only; the
		// validator refuses it against an external cluster, so there is
		// nothing to decide here.
		Debug: b.Stream.Debug,

		// Through the accessors for the same reason EventRetention goes
		// through one: the field is a STRING with two meanings, and a
		// second place deciding which is which is a second place to get
		// "always" wrong.
		SyncAlways:   b.Stream.SyncAlways(),
		SyncInterval: b.Stream.SyncInterval(),
	}
	// Through the accessor, so the seconds-to-duration conversion happens
	// once at the edge rather than being re-derived here — one slip from
	// being off by 10^9, with the compiler accepting both.
	if b.Stream.EventRetentionHours > 0 {
		cfg.EventRetention = b.Stream.EventRetention()
	}
	q, stopServer, err := openStream(ctx, b, cfg)
	if err != nil {
		return nil, err
	}
	out := &Backends{Queue: q, stopServer: stopServer}
	if err = attachCoordination(ctx, b, out, q.Conn()); err != nil {
		out.Close(ctx)
		return nil, err
	}
	return out, nil
}

// openStream dials or starts the broker and returns the queue on it, together
// with what shuts the broker down when this node started it — nil when it
// dialled one.
//
// It returns no connection of its own: the coordination store rides the
// queue's, and [openNATS] takes it from the queue.
func openStream(ctx context.Context, b *config.Bootstrap, cfg jetstream.Config) (*jetstream.Queue, func(), error) {
	// A URL IS A BROKER SOMEBODY ELSE RUNS, and this branch is what makes
	// `stream.type: nats` mean anything. Without it every path here
	// started an in-process member and connected to THAT: an operator who
	// pointed a fleet at an external cluster got one private broker per
	// node instead, so no node shared a stream or a coordination bucket
	// with any other, and nothing anywhere said so. Every symptom that
	// followed was a fleet-shared-state break wearing a different mask —
	// a seat claimed by everyone, a trigger worked twice, a token counter
	// per node.
	if b.Stream.URL != "" {
		q, err := jetstream.Open(ctx, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: stream: %w", err)
		}
		// NO stop: this node dialled a broker its peers are using and
		// must never take it down.
		return q, nil, nil
	}

	server, err := jetstream.StartServer(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: stream: %w", err)
	}
	q, err := server.Client(ctx)
	if err != nil {
		server.Shutdown()
		return nil, nil, fmt.Errorf("engine: stream client: %w", err)
	}
	return q, server.Shutdown, nil
}

// attachCoordination builds the fleet store and the lease slot on one
// connection, which is the queue's — see [openNATS].
//
// ONE FUNCTION FOR BOTH BRANCHES, and that is the point rather than tidiness:
// an external broker and an embedded one differ in who runs the broker and
// nothing else, so two copies of this would be two chances to forget the
// fleet store on one of them — and a nil Fleet is not a startup error, it is
// a panic hours later in whichever subsystem reached for it first.
//
// # THE FLEET STORE IS ALWAYS THE KV, whatever the coordination slot says
//
// Only the LEASE store follows that setting. The two answer different
// questions and only one of them is about peers. A lease answers "who runs
// this seat": one node has nobody to fence against and re-claims everything
// at boot, so an in-process table is the honest implementation there. The
// fleet store holds RECORDS — the token counter, an open agent-to-agent ask,
// a claimed scheduled fire, a detached coding run, the company's secrets —
// and every one of those has to outlive the PROCESS, on one node as much as
// on four.
//
// It did not, and the consequences were silent: a company's token spend reset
// to zero on every restart although its bucket is documented as having no
// retention at all ("a cap is a ceiling for the life of a deployment"), a
// redelivered trigger after a restart was worked twice, and a detached
// sandbox run — a BILLED box — was forgotten by the process that launched it.
// What persistence the records get is the same choice as the event log's:
// stream.store_dir.
func attachCoordination(ctx context.Context, b *config.Bootstrap, out *Backends, conn *nats.Conn) error {
	// ONE CEILING OVER THE WHOLE BRING-UP, because this is where the
	// sequence actually is: every replicated bucket of the fleet store and
	// of the lease store, across two calls, each of which would otherwise
	// discover a wedged cluster on its own budget. Without it the real bound is the PRODUCT rather than the
	// term — a number nobody declared, which is the shape of a limit that
	// is not a decision. Each create below still takes the lesser of its
	// own budget and what is left of this one, because WithTimeout only
	// ever shortens.
	ctx, cancel := context.WithTimeout(ctx,
		jsprovision.Clustered(clusteredStream(b)).SequenceBudget())
	defer cancel()

	shared, err := openFleet(ctx, conn, b.Stream.Replicas, clusteredStream(b))
	if err != nil {
		return fmt.Errorf("engine: coordination: %w", err)
	}
	out.Fleet = shared

	if b.Coordination.Type != config.CoordinationEmbeddedKV {
		out.Coord = coordmem.New()
		return nil
	}
	leases, err := kv.Open(ctx, conn, kv.Config{
		TTL:       leaseTTL(b),
		Replicas:  b.Stream.Replicas,
		Clustered: clusteredStream(b),
	})
	if err != nil {
		return fmt.Errorf("engine: coordination: %w", err)
	}
	out.Coord = leases
	return nil
}

// openFleet builds the shared-state store beside the leases.
//
// The retentions are supplied HERE for the same reason leaseTTL is: each one
// is a BUCKET's age, fixed when the bucket is created, so a silent default
// would decide it at the moment nobody was looking. Every number is the one
// the subsystem that reads it already uses, named at its own package.
func openFleet(ctx context.Context, conn *nats.Conn, replicas int, clustered bool) (coord.Fleet, error) {
	return kv.OpenFleet(ctx, conn, kv.FleetConfig{
		RateWindow:      coord.RateWindow,
		ClaimTTL:        coord.ClaimTTL,
		LedgerRetention: coord.LedgerRetention,
		FireRetention:   coord.FireRetention,
		FollowRetention: coord.FollowRetention,
		CooldownMax:     coord.CooldownMax,
		StatusFreshness: coord.StatusFreshness,
		Replicas:        replicas,
		Clustered:       clustered,
	})
}

// streamTLS carries a Tier A tls block to the queue package's own.
//
// A TRANSLATION rather than a shared type, because the two packages are on
// opposite sides of the config seam: `queue/jetstream` takes what it needs to
// dial and knows nothing about YAML, and `config` describes a document and
// knows nothing about NATS. One struct shared between them would make either
// change the other's.
func streamTLS(t config.NATSTLS) jetstream.TLS {
	return jetstream.TLS{CA: t.CA, Cert: t.Cert, Key: t.Key}
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
	// The ZERO-MEANS-DEFAULT policy stays here rather than moving onto the
	// accessor: seat.SeatLeaseTTL is the seat package's constant, and
	// config must not import it. Only the arithmetic moves.
	if b.Coordination.LeaseTTLSeconds <= 0 {
		return seat.SeatLeaseTTL
	}
	return b.Coordination.LeaseTTL()
}

// leaseTTLInForce is the coordination backend that knows the TTL leases are
// ACTUALLY held at, which is not always the one this node's Tier A asks for.
//
// Declared here, in the package that calls it, and kept to the one method:
// [coord.Backend] says nothing about a TTL because the in-process backend has
// no ceiling to report, and widening the contract for one implementation would
// make every other one answer a question it has no basis for.
type leaseTTLInForce interface {
	TTL() time.Duration
}

// effectiveLeaseTTL resolves what this node must acquire and renew with.
//
// # Why this cannot simply be the configured value
//
// The KV backend ADOPTS the lease bucket rather than rewriting it, so on a
// fleet the TTL in force is whichever member created the bucket first — see
// [kv.Open]. The bucket's own age is the arbiter, and [kv.Store.validateTTL]
// refuses a claim longer than it. A node that went on acquiring at its own
// configured value would therefore be wrong in both directions, and one of
// them is fatal: configured SHORTER than the bucket and its leases lapse
// earlier than the operator asked for; configured LONGER and every single
// acquire is refused as too long, so the node holds no seats at all and the
// company's work sits unclaimed on a node that looks healthy.
//
// Taking the live value is also what keeps the derived timings coherent: the
// heartbeat and the release budget are fractions of this number, so a node
// renewing on a 90-second cadence against a 45-second bucket would lose every
// seat it held between beats.
//
// A mismatch is not silent — [kv.Open] logs it with both values and the
// remedy. It is not fatal either: refusing to boot over it would take a
// company down for a disagreement the fleet is already resolving one way.
func effectiveLeaseTTL(b *config.Bootstrap, backend coord.Backend) time.Duration {
	configured := leaseTTL(b)
	live, ok := backend.(leaseTTLInForce)
	if !ok {
		// The in-process backend: this node is the only holder, so its
		// own configuration is the whole truth.
		return configured
	}
	if got := live.TTL(); got > 0 {
		return got
	}
	return configured
}

// clusteredStream is whether this node's broker has PEERS.
//
// The one place the engine decides it, so the queue, the lease store and the
// fleet store cannot disagree about which budget they are on — and it is the
// same rule [Queue.Clustered] applies, which is the rule the embedded server
// itself is built by: the cluster block takes effect only when it is NAMED.
// Tier A refuses a port or a peer list without one, so the name is sufficient.
//
// A topology question rather than a replica count: an external NATS is
// somebody else's cluster, and an embedded member that names one is clustered
// whatever replica count it asks for — see [jsprovision.Clustered].
func clusteredStream(b *config.Bootstrap) bool {
	return b.Stream.Type == config.StreamNATS || b.Stream.Cluster.Name != ""
}
