package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
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
	// counter, inbound-delivery claims, the turn-completion ledger and the
	// config activation pointer.
	//
	// Beside Coord rather than inside Store, because Store is this node's
	// LOCAL database — one file, one process — and every one of these has
	// to be agreed across the fleet to mean anything. See coord/fleet.go
	// for what each was doing while it was per-node.
	Fleet coord.Fleet

	// Objects is where the company's files are kept — the backend Tier A's
	// store.objects names, opened here and checked against the fleet's
	// recorded one before anything can write to it (see [openObjects]).
	Objects objstore.Backend

	// objectsIdentity names that backend as the fleet records it, and
	// objectsStream the broker stream it lives in — empty for a backend
	// outside the broker — for the backup, which copies no object a stream
	// snapshot already carries.
	objectsIdentity string
	objectsStream   string

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

	// conn is the SECOND connection, the one the coordination store rides
	// on an embedded broker — dialled through [jetstream.Queue.DialWatched],
	// so NATS closing it for good stops the node as the queue's own would.
	// Held here so Close releases it in its place in the order, after the
	// queue and before the server; the store does not own it. Nil on an
	// external broker, where the store rides the queue's own.
	conn *nats.Conn

	// rides is the connection the coordination store rides on EVERY
	// topology: conn above on an embedded broker, the queue's own on an
	// external one. Not owned here — see [Backends.CoordinationConn].
	rides *nats.Conn

	// coordination is the JetStream client over rides, in the API this
	// node's broker is addressed in — what the coordination stores were
	// opened with, and what [Backends.CoordinationJetStream] hands the
	// subsystems that ride the same connection.
	coordination natsjs.JetStream

	// api is the JetStream API this node's broker is addressed in.
	api jsapi.API

	// broker is this node's own embedded broker, for the one question only
	// a member can answer in process: the metadata group's membership. Nil
	// on an external stream, and on backends a caller lent without one.
	broker brokerMembership

	// death is that same embedded broker declaring it cannot go on — see
	// [Engine.watchBroker]. Nil on an external stream, whose broker is its
	// own operator's to keep alive and whose loss the client reconnects
	// through.
	death brokerDeath
}

// Complete reports which of the four a Backends lacks, or nil when it holds
// them all.
//
// [OpenBackends] never builds a partial set, so this is for the caller that
// supplies its own to [New]. A partial set used to be RUN rather than refused:
// the engine skipped whatever needed the missing piece, so an engine lent a
// queue and no store served a company with no Worklet, no conversation
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
	if b.Objects == nil {
		missing = append(missing, "Objects")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("engine: the supplied Backends has no %s; open them with "+
		"OpenBackends, which builds the stream, coordination and the store together",
		strings.Join(missing, ", "))
}

// API is the JetStream API every client of this node's broker speaks — the
// one a subsystem reaching past the client (the backup's stream snapshot)
// has to address too. See [jsapi].
func (b *Backends) API() jsapi.API { return b.api }

// Conn exposes the broker connection this node's coordination store rides
// on an EMBEDDED broker, and nil on an external one.
//
// For the ONE subsystem whose answer differs between the two: the backup,
// which snapshots the streams themselves. The estate it copies is not addressable any other way — on the
// default topology the broker is embedded here and binds no socket — so a
// backup that could not reach this connection could not exist.
//
// Nil when this node DIALLED an external broker: the queue owns that
// connection, and the streams belong to a cluster with its own backup tooling.
// The caller takes no ownership: closing it is [Backends.Close]'s job, and a
// caller that closed it would take this node's leases down with it SILENTLY —
// a close this process makes itself is never the loss the node stops for.
func (b *Backends) Conn() *nats.Conn { return b.conn }

// CoordinationConn is the broker connection this node's coordination store
// rides, on every topology: the second connection [Backends] owns on an
// embedded broker, the queue's own on an external one. Nil only where there is
// no broker at all — a Backends a caller assembled around the memory twin.
//
// For a subsystem that talks to the broker outside the queue contract and
// whose answer does NOT depend on who runs the broker: the seat memory
// changelog, which a node publishes and a peer replays whichever broker the
// fleet shares. It read [Backends.Conn] once, whose nil is the backup's
// decision about an external cluster's streams, so every fleet on
// `stream.type: nats` carried no seat's memory at all, and a seat placement
// moved forgot everything it had learned, with nothing saying so.
//
// THE COORDINATION STORE'S CONNECTION rather than the queue's on an embedded
// broker, so what a subsystem here does in volume (a seat's replay is an
// ordered consumer over its whole history) never sits on the read loop every
// mailbox consumes through. The caller takes no ownership.
func (b *Backends) CoordinationConn() *nats.Conn { return b.rides }

// CoordinationJetStream is the JetStream client over [Backends.CoordinationConn],
// in the API this node's broker is addressed in ([jsapi]) — the client the
// coordination stores themselves were opened with — or nil where there is no
// broker at all.
//
// THE CLIENT AND NOT THE CONNECTION, because the client is where the API is
// decided: one built over the connection with the defaults reaches a member's
// JetStream and times out on a leaf's, whose JetStream is the members' under
// their domain. A subsystem that rides this connection — the seat memory
// changelog — rides it in the API the stores do, on every topology. The
// caller takes no ownership.
func (b *Backends) CoordinationJetStream() natsjs.JetStream { return b.coordination }

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
//     unclaimable for a full TTL. The lease store's own background watch —
//     the view its gates are judged by — is stopped first, while its
//     connection is still open, so it ends rather than failing.
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
			log.WarnContext(ctx, "queue_stop_failed", "error", err)
		}
	}
	if closer, ok := b.Coord.(backgroundCloser); ok {
		closer.Close()
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
			log.WarnContext(ctx, "store_close_failed", "error", err)
		}
		b.Store = nil
	}
}

// abandon closes a set a failed boot half-built, and answers err as that boot
// says it: carrying the cause of a broker connection NATS closed for good, where
// one was ([lostDuring]), beside the step's own error.
//
// THE ONE WAY OUT of [OpenBackends] once a queue exists, so no failing step
// can reach the operator as a bare "connection closed" while the sentence
// naming why it closed, and what to change, sits unread on the queue. The cause
// is read BEFORE the close: the client's closed handler runs on the client's
// own goroutine once the call that failed has been released, so the read waits
// for it where a connection is already closed — and after the close every
// connection reads as closed, its owner's close included, with no handler
// coming ([jetstream.Queue.LostDuring]).
func (b *Backends) abandon(ctx context.Context, err error) error {
	err = lostDuring(b.Queue, err)
	b.Close(ctx)
	return err
}

// backgroundCloser is a coordination store that runs something in the
// background of its own — the KV store's gate view — and stops it.
type backgroundCloser interface {
	Close()
}

// OpenBackends builds everything a node runs on.
//
// It does NOT re-validate the topology: config.Bootstrap.Validate already
// refuses the incoherent combinations, and duplicating those rules here would
// give an operator two places to read and two chances to disagree. What this
// adds is the construction, and one rule validation cannot express — an
// embedded-KV coordination store lives on the stream's own NATS broker, so the
// two slots are not independent at runtime even though they are in config.
//
// It takes the COMPANY as well as the bootstrap, for one field: the width of
// the vectors the configured embedding model produces. That width is Tier B
// because the model is, and the store wants it at open time — it is the only
// thing that knows how wide the packed BLOBs in its vector columns are.
//
// A NIL COMPANY IS A REAL CASE, not a caller's mistake, and it is two. A node
// whose store activated no revision has no company to be asked for, and holds
// no rows. A node started with `-import-company` reads none, so that a stored
// revision this build cannot run does not refuse the boot that replaces it —
// and it may hold that revision's rows. Both open at width 0 and learn their
// width from the first epoch that arrives ([store.DB.LearnEmbeddingDim]); for
// the import that epoch is the imported file's, so the store ends at the width
// a boot on the file opened it at, over rows of the width it replaced — where
// a restart onto a new width, which is how the width is changed, has always
// left a store. Opened at the stored revision's width instead, a file that
// changes it would be refused by its first apply ([Engine.buildEmbedder]) with
// the fleet already pointed at it, and need a second restart to take.
//
// This does NOT make the width mutable, and it was refused here while it was
// immutable for a reason that still holds: a store left at a wrong non-zero
// width refuses every write from the right provider, and recall stops
// returning anything with nothing in the log to say why. So it is fixed once
// non-zero, [Engine.buildEmbedder] refuses a revision that would change it,
// and only the never-told case is learnable.
//
// A node that boots ON a revision does not arrive here nil: the caller reads
// the active revision first and passes it, so the store opens at its width.
// The one node holding a revision that arrives nil is the import above.
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
		return nil, out.abandon(ctx, err)
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
	//
	// ONLY WHERE THE STORE OUTLIVES THE PROCESS. A node without `data`
	// deletes its store at every boot, so rows written into it would be
	// gone at its next restart with nothing able to read them in between
	// — it hands them to a data node instead ([observe.Custody], armed by
	// [New] once the estate client exists).
	if HoldsEstate(b) {
		out.Queue.AddPublishListener(observe.NewWriter(db.Events()).Listen())
	}
	return out, nil
}

// openStore opens this node's own database. The replicated estate it holds is
// opened on the handle this returns when the engine is built ([holdEstate]),
// whichever caller opened the store.
func openStore(ctx context.Context, b *config.Bootstrap, c *config.Company) (*store.DB, error) {
	db, err := store.OpenNode(ctx, b.Store.Path, storeOptions(b, c))
	if err != nil {
		return nil, fmt.Errorf("engine: store: %w", err)
	}
	return db, nil
}

// storeOptions is what this node's own database is opened with, decided from
// Tier A and the company.
//
// APART FROM THE OPEN so that what the file is opened as can be asked of a
// node's configuration without one.
//
// NO PINS: the writers the state log's apply loops pin are on the REPLICATED
// estate's pool, which [holdEstate] sizes for the register ([estateLogs]) on a
// node that holds it — never on this file, which no apply loop writes.
func storeOptions(b *config.Bootstrap, c *config.Company) store.Options {
	opts := store.Options{
		MaxOpenConns:   b.Store.MaxOpenConns,
		ReplicatedPath: b.Store.ReplicatedPath,
		BusyTimeout:    b.Store.BusyTimeout(),
	}
	// A NODE WITHOUT `data` HOLDS NO REPLICATED ESTATE and keeps nothing
	// that has to outlive it. Its own file is discarded and recreated at
	// every boot, and it holds nothing ([HoldsEstate]), so no replicated
	// estate is ever opened on it — a path that reaches for one is told
	// [store.ErrNoEstate] rather than handed an empty database that reads as
	// a company with nothing in it.
	//
	// DECIDED HERE ALONE: whoever else needs to know whether this node keeps
	// what it writes asks the handle this opens ([store.DB.Scratch]).
	if !HoldsEstate(b) {
		opts.Scratch = b.Store.Scratch
	}
	// Nil embeddings means no vector recall is configured, which the store
	// reads as width 0: no DECLARED width, so it checks nothing against it
	// and a caller that has vectors anyway is not refused. That is the
	// honest shape of "this company does not remember by similarity", and
	// distinct from a configured width the store was never told about.
	//
	// A NIL COMPANY reads as the same 0, and is a real case rather than a
	// caller's mistake: a node that boots on no revision has no company to be
	// asked for. It learns its width from the first epoch that arrives — see
	// [store.DB.LearnEmbeddingDim], and [OpenBackends] for the node started
	// with -import-company, which may hold rows already.
	if c != nil && c.Providers.Embeddings != nil {
		opts.EmbeddingDim = c.Providers.Embeddings.Width()
	}
	return opts
}

// openNATS builds a JetStream stream, embedded or external, and the
// coordination store on the same broker.
//
// TWO BRANCHES FOR THE STREAM, ONE TAIL FOR COORDINATION. The branches differ
// only in who owns the broker and therefore who owns the connection; what is
// built on top of it is identical, and writing that twice is how one copy
// ends up without a fleet store — which is not a startup error but a panic
// hours later in whichever subsystem reached for it first.
func openNATS(ctx context.Context, b *config.Bootstrap) (*Backends, error) {
	cfg, err := streamConfig(b)
	if err != nil {
		return nil, err
	}
	nodeID := cfg.ServerName
	// THE SAME RESOLVED ID NAMES WHAT THIS NODE PUBLISHES. Every event
	// leaves carrying it as its origin (queue.WithNode), because the event
	// store is written inline on the publishing node and a reader holding
	// any one row or frame has only that to find the node whose store holds
	// the rest — see events.Event.Node.
	out, conn, err := openStream(ctx, b, cfg, queue.WithNode(nodeID))
	if err != nil {
		return nil, err
	}
	out.rides = conn
	out.api = cfg.API()
	// IN THE API THE STREAM'S OWN CLIENT SPEAKS, over the connection
	// coordination rides — see [jsapi] for why a client built any other
	// way reaches a member's JetStream and times out on a leaf's.
	js, err := out.api.Client(conn)
	if err != nil {
		return nil, out.abandon(ctx, fmt.Errorf("engine: coordination: %w", err))
	}
	out.coordination = js
	if err = attachCoordination(ctx, b, out, js); err != nil {
		return nil, out.abandon(ctx, err)
	}
	if err = openObjects(ctx, b, out, js); err != nil {
		return nil, out.abandon(ctx, err)
	}
	return out, nil
}

// streamConfig is the broker a node configured as b runs or dials: the ONE
// reading of Tier A's stream block, which the engine's own boot and a
// `crewlet` command reaching the fleet's store with the engine stopped
// ([OpenFleetStore]) both start from — two readings would be two brokers that
// disagree about a setting, the server's name above all.
func streamConfig(b *config.Bootstrap) (jetstream.Config, error) {
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
		return jetstream.Config{}, fmt.Errorf("engine: stream: %w", err)
	}
	cfg := jetstream.Config{
		URL:              b.Stream.URL,
		StoreDir:         b.Stream.StoreDir,
		StoreMaxBytes:    b.Stream.StoreMaxBytes,
		ClusterName:      b.Stream.Cluster.Name,
		ClusterURLs:      b.Stream.Cluster.Peers,
		ClusterPort:      b.Stream.Cluster.Port,
		ClusterHost:      b.Stream.Cluster.Host,
		ClusterAdvertise: b.Stream.Cluster.Advertise,
		// THE LEAF LINK, from whichever side of it this node is: a node
		// without `data` dials the members' listeners and runs no
		// JetStream of its own, and a member opens one for them. Tier A
		// has already refused a node that is both, or neither where it
		// has to be one — see [config.Bootstrap.ValidateRoles].
		LeafURLs:      b.Stream.Leaf.URLs,
		LeafPort:      b.Stream.Leaf.Port,
		LeafHost:      b.Stream.Leaf.Host,
		LeafAdvertise: b.Stream.Leaf.Advertise,
		ServerName:    nodeID,
		Replicas:      b.Stream.Replicas,
		Credentials:   b.Stream.Credentials,
		Token:         b.Stream.Token,
		TLS:           streamTLS(b.Stream.TLS),

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
	return cfg, nil
}

// openObjects opens the backend the company's files are kept in, once the
// fleet has agreed it is the fleet's.
//
// AGREED BEFORE OPENED. The first node to open a store records which backend
// it is ([coord.ObjectStores]), and every node after compares its own Tier A
// against that record: a node pointed at another bucket — or at the broker
// where the fleet uses a bucket — would split the company's files between two
// stores, each side's uploads readable on that side alone, with nothing
// failing. Refused here, by name, it is a node that does not start.
func openObjects(ctx context.Context, b *config.Bootstrap, out *Backends, js natsjs.JetStream) error {
	objects := b.Store.Objects
	identity := objects.Identity()
	agreed, err := out.Fleet.AgreeObjectBackend(ctx, identity)
	if err != nil {
		return fmt.Errorf("engine: check store.objects against the fleet's object store: %w", err)
	}
	if agreed != identity {
		return fmt.Errorf("engine: store.objects names %q and the fleet keeps its files in %q: "+
			"every node must name the same object store, or the company's files split "+
			"between the two — set store.objects on this node to match the others",
			identity, agreed)
	}
	out.objectsIdentity = identity
	switch objects.BackendOrDefault() {
	case config.ObjectBackendS3:
		s3 := objects.S3
		backend, err := s3obj.Open(ctx, s3obj.Config{
			Endpoint: s3.Endpoint, Region: s3.Region, Bucket: s3.Bucket, Prefix: s3.Prefix,
			PathStyle: s3.PathStyle, AccessKeyID: s3.AccessKeyID, SecretAccessKey: s3.SecretAccessKey,
		})
		if err != nil {
			return fmt.Errorf("engine: the object store (store.objects.s3): %w", err)
		}
		out.Objects = backend
	default:
		openCtx, cancel := context.WithTimeout(ctx,
			jsprovision.Clustered(clusteredStream(b)).SequenceBudget())
		defer cancel()
		reservation, explicit := objectsReservation(ctx, b, out.Queue)
		backend, err := natsobj.Open(openCtx, js, natsobj.Config{
			Replicas: b.Stream.Replicas, Clustered: clusteredStream(b),
			MaxBytes: reservation, Explicit: explicit,
		})
		if err != nil {
			return fmt.Errorf("engine: the object store (store.objects.backend: nats): %w", err)
		}
		out.Objects = backend
		out.objectsStream = natsobj.Stream
	}
	return nil
}

// THE NATS BUCKET'S RESERVATION, and what it is carved out of.
//
// The broker grants a stream its whole ceiling when it creates it, against one
// limit (stream.store_max_bytes, or the share of its volume the embedded
// broker takes). The state logs reserve [StreamBudgetShare] of what it can
// grant; every seat's mailbox and every coordination bucket reserve nothing
// and share what is left. The bucket is given a reservation of its own
// (internal/objstore/natsobj says why it may not grow into that remainder),
// so it is sized here, beside the logs' share and a floor the remainder keeps.
//
// ITS RESERVATION IS SPENT BEFORE THE LOGS ARE SIZED. The object store opens
// with the backends, ahead of the state logs, so the logs size themselves
// from what the broker has left once the bucket holds its reservation
// ([logsGrantable]) and every restart divides the same remainder, which keeps
// the logs' ceilings reproducible from boot to boot. That holds on a broker
// that states no limit too: the free space standing in for the limit is
// spent by the bucket's reservation, which the broker still reports, so the
// bucket and the logs never reserve more between them than they would under a
// stated limit of that size.

// objectsCoordinationFloor is what the derived reservation always leaves the
// broker beside the state logs' share: the mailboxes and coordination buckets
// reserve nothing and grow inside it.
//
// A GIBIBYTE, the figure the object storage review states, and the size of the
// smallest corpus-sized log ([MinDomainCeiling]): the leases, ledgers and
// counters are kilobytes each, and the mailboxes are bounded by the event
// retention, so a gibibyte is months of a reference company's coordination
// traffic even with nothing trimmed.
const objectsCoordinationFloor int64 = 1 << 30

// objectsBucketShare is the most of the broker's limit the derived reservation
// takes: a quarter, the review's figure. Half of the limit is the state logs',
// and a quarter left over keeps the coordination remainder as large as the
// bucket however full the bucket gets.
const objectsBucketShare = 4

// budgetReader is the broker as the bucket's reservation is sized against it:
// the JetStream queue's own budget read, declared here by its one caller.
type budgetReader interface {
	StreamBudget(ctx context.Context) (jetstream.StorageBudget, error)
}

// objectsReservation is the byte reservation the nats bucket is created with,
// and whether an operator wrote it (store.objects.nats_max_bytes). An unset
// one is derived from the broker's limit ([derivedObjectsReservation]); where
// the broker states none this node can read, from the stream volume's free
// space, which is the one figure it has, as the logs' sizing does.
func objectsReservation(ctx context.Context, b *config.Bootstrap, q any) (int64, bool) {
	if set := b.Store.Objects.NatsMaxBytes; set > 0 {
		return set, true
	}
	limit := int64(-1)
	if broker, ok := q.(budgetReader); ok {
		readCtx, cancel := context.WithTimeout(ctx, jsprovision.LookupBudget)
		budget, err := broker.StreamBudget(readCtx)
		cancel()
		switch {
		case err != nil:
			log.WarnContext(ctx, "objects_reservation_budget_unread", "error", err.Error(),
				"detail", "the nats bucket's reservation is derived from this node's "+
					"free space instead; set store.objects.nats_max_bytes to choose it")
		case budget.Limit >= 0:
			limit = budget.Limit
		}
	}
	if limit < 0 {
		volume := streamVolume(b)
		free, err := volumeFree(volume)
		if err != nil {
			log.WarnContext(ctx, "objects_reservation_free_space_unmeasured",
				"path", volume, "error", err.Error(),
				"detail", "the nats bucket is reserved at its smallest; set "+
					"store.objects.nats_max_bytes to choose it")
		}
		limit = free
	}
	reservation := derivedObjectsReservation(limit, b.Stream.DeclaredLogCeilings())
	log.InfoContext(ctx, "objects_reservation_derived", "bytes", reservation,
		"broker_limit", limit, "detail", "a bucket created now reserves this much "+
			"of the broker; one that exists keeps the ceiling it holds")
	return reservation, false
}

// derivedObjectsReservation is the reservation an unset
// store.objects.nats_max_bytes takes against a broker limit of limit bytes,
// beside state-log ceilings an operator declared summing to declared: the
// smaller of [objectsBucketShare] of the limit and what the limit leaves once
// the logs' share (or the declared ceilings, where they exceed it) and
// [objectsCoordinationFloor] are taken out, held inside the bounds Tier A
// accepts for the field.
//
// Pure arithmetic, so every case is a table row rather than a broker.
func derivedObjectsReservation(limit, declared int64) int64 {
	logs := max(int64(float64(limit)*StreamBudgetShare), declared)
	share := min(limit/objectsBucketShare, limit-logs-objectsCoordinationFloor)
	return min(max(share, config.NatsObjectsMaxBytesFloor), config.NatsObjectsMaxBytesCeiling)
}

// openStream dials or starts the broker, and answers the connection
// coordination should ride.
//
// THE COORDINATION STORE LIVES ON THE STREAM'S BROKER either way, and that is
// ADR-0001: one estate, so reaching the leases is reaching the mail. Which
// CONNECTION it rides differs by topology. On an external broker it is the
// queue's own. On the embedded one it is a SECOND connection to the same
// in-process server, which the seat-memory replay and the backup's stream
// copies ride too, so that neither — an ordered consumer over a seat's whole
// history, a copy of whole streams — sits on the read loop every mailbox
// consumes through.
//
// Two connections fail independently, and the hazard in that is a node holding
// live leases over the one that still works while the one carrying its inbox
// is gone — alive to its peers, deaf to its work. So the second one is dialled
// WATCHED ([jetstream.Queue.DialWatched]): NATS closing either connection for
// good stops the node ([Engine.Fatal]), and the split lasts no longer than a
// reconnect.
//
// A NODE WITHOUT `data` IS THE EMBEDDED BRANCH TOO, as a LEAF: its in-process
// server runs no JetStream of its own and links to the members' listeners
// (ADR-0025), so both its connections reach JetStream through that link, under
// the members' domain — which is why every client built over them is built in
// the API the stream speaks ([jetstream.Config.API]) rather than with the
// client's defaults.
//
// EVERY QUEUE THIS OPENS CARRIES THE CALLER'S OPTIONS — the node every event
// it publishes is stamped with ([queue.WithNode]) — on both branches, so an
// event's origin names this node whichever broker topology it rode.
func openStream(ctx context.Context, b *config.Bootstrap, cfg jetstream.Config,
	opts ...queue.Option,
) (*Backends, *nats.Conn, error) {
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
		q, err := jetstream.Open(ctx, cfg, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: stream: %w", err)
		}
		// NO stopServer: this node dialled a broker its peers are using
		// and must never take it down. And no `conn` either — the QUEUE
		// owns this connection, so Close must not close it a second time
		// underneath the client that is still shutting down.
		return &Backends{Queue: q}, q.Conn(), nil
	}

	server, err := jetstream.StartServer(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: stream: %w", err)
	}
	q, err := server.Client(ctx, opts...)
	if err != nil {
		server.Shutdown()
		return nil, nil, fmt.Errorf("engine: stream client: %w", err)
	}
	out := &Backends{Queue: q, stopServer: server.Shutdown, broker: server,
		death: server.Death()}
	// WATCHED, through the queue: this connection holds every lease the
	// node renews, so NATS closing it for good is the node's loss exactly
	// as the queue's own connection closing is, and the queue reports both
	// as one ([jetstream.Queue.Lost]). A donor's second connection is the
	// one that is not — see [jetstream.Queue.DialOwned].
	conn, err := q.DialWatched()
	if err != nil {
		return nil, nil, out.abandon(ctx,
			fmt.Errorf("engine: coordination connection: %w", err))
	}
	// OWNED HERE, because this is a SECOND connection to the node's own
	// in-process server — the queue holds its own. The external branch
	// shares the queue's instead, which is why it leaves this nil.
	out.conn = conn
	return out, conn, nil
}

// attachCoordination builds the fleet store and the lease slot on one
// connection.
//
// ONE FUNCTION FOR BOTH BRANCHES, and that is the point rather than tidiness:
// an external broker and an embedded one differ in who owns the connection
// and nothing else, so two copies of this would be two chances to forget the
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
// It did not, and the consequences were silent: a company's token spend for
// the current window reset to zero on every restart, a redelivered trigger after a restart was worked twice, and a detached
// sandbox run — a BILLED box — was forgotten by the process that launched it.
// What persistence the records get is the same choice as the event log's:
// stream.store_dir.
//
// # AND THE FENCING COUNTER IS A RECORD, whatever the coordination slot says
//
// The in-process lease table is right about OWNERSHIP and was wrong about the
// TOKEN. A seat's epoch is written into fleet records (a sandbox run's
// owner_epoch) and compared with later, so it has to outlive the process as
// those records do. Counted beside the in-process leases, it restarted at 1 on
// every boot, and after a graceful restart the seat's new holder was refused
// as older than its own previous self on every run that self had parked: an
// answer to such a run was never recorded, and its resume and reap were
// refused too. So a single node's leases mint from the same persistent counter
// a fleet's lease store does (coord/kv.Epochs), in the same bucket, which also
// carries the tokens across a move from local coordination to a fleet.
func attachCoordination(ctx context.Context, b *config.Bootstrap, out *Backends, js natsjs.JetStream) error {
	// ONE CEILING OVER THE WHOLE BRING-UP, because this is where the
	// sequence actually is: every replicated bucket of both coordination
	// stores, across two calls, each of which would otherwise discover a
	// wedged cluster on its own budget. Without it the real bound is the PRODUCT rather than the
	// term — a number nobody declared, which is the shape of a limit that
	// is not a decision. Each create below still takes the lesser of its
	// own budget and what is left of this one, because WithTimeout only
	// ever shortens.
	ctx, cancel := context.WithTimeout(ctx,
		jsprovision.Clustered(clusteredStream(b)).SequenceBudget())
	defer cancel()

	shared, err := openFleet(ctx, js, b.Stream.Replicas, clusteredStream(b))
	if err != nil {
		return fmt.Errorf("engine: coordination: %w", err)
	}
	out.Fleet = shared

	var epochs *kv.Epochs
	if b.Coordination.Type != config.CoordinationEmbeddedKV {
		epochs, err = kv.OpenEpochs(ctx, js, kv.EpochsConfig{
			Replicas:  b.Stream.Replicas,
			Clustered: clusteredStream(b),
		})
		if err != nil {
			return fmt.Errorf("engine: coordination: %w", err)
		}
		out.Coord = &coordmem.Backend{Epochs: epochs}
	} else {
		leases, err := kv.Open(ctx, js, kv.Config{
			TTL:       leaseTTL(b),
			Replicas:  b.Stream.Replicas,
			Clustered: clusteredStream(b),
		})
		if err != nil {
			return fmt.Errorf("engine: coordination: %w", err)
		}
		out.Coord, epochs = leases, leases.Epochs()
	}
	return floorSeatEpochs(ctx, epochs, shared)
}

// floorSeatEpochs raises each seat's fencing counter above every epoch its
// sandbox run records carry, BEFORE this node claims a seat.
//
// The counter is the only source of tokens, so a row should never carry one
// above it, and on a deployment that has only ever minted from this bucket none
// does. One that did not (local coordination before its counter was
// persistent), or one whose epochs bucket was deleted and re-created, has rows
// stamped above an empty counter, and the seat's next holder would be refused
// as older than its own previous self on every one of them: an answer to a
// parked run never recorded, its resume and reap refused. Raising the counter
// is the whole repair, because the seat's next claim then mints above the rows
// and its recovery fences them to the new lease.
//
// EVERY BOOT, on every topology, rather than once: it costs one listing of the
// runs, and a floor at or below the counter changes nothing, so a fleet's
// nodes doing it concurrently agree.
func floorSeatEpochs(ctx context.Context, epochs *kv.Epochs, fleet coord.Fleet) error {
	floors, err := sandbox.NewCoordStore(fleet).OwnerEpochs(ctx)
	if err != nil {
		return fmt.Errorf("engine: coordination: read the epochs the sandbox runs carry: %w", err)
	}
	for seat, floor := range floors {
		resource := coord.SeatResource(seat)
		raised, err := epochs.Floor(ctx, resource, floor)
		if err != nil {
			return fmt.Errorf("engine: coordination: raise the fencing epoch of %s to %d: %w",
				resource, floor, err)
		}
		if raised {
			log.InfoContext(ctx, "seat_epoch_floored", "seat", seat, "epoch", floor,
				"detail", "a sandbox run carries a fencing epoch the counter had not "+
					"reached, so the seat's next claim mints above it")
		}
	}
	return nil
}

// openFleet builds the shared-state store beside the leases.
//
// The retentions are supplied HERE for the same reason leaseTTL is: each one
// is a BUCKET's age, fixed when the bucket is created, so a silent default
// would decide it at the moment nobody was looking. Every number is the one
// the subsystem that reads it already uses, named at its own package.
func openFleet(ctx context.Context, js natsjs.JetStream, replicas int, clustered bool) (coord.Fleet, error) {
	return kv.OpenFleet(ctx, js, kv.FleetConfig{
		MaxRateWindow:    coord.MaxRateWindow,
		MaxClaimTTL:      coord.MaxClaimTTL,
		LedgerRetention:  coord.LedgerRetention,
		FireRetention:    coord.FireRetention,
		FollowRetention:  coord.FollowRetention,
		RebaseRetention:  coord.RebaseRetention,
		BudgetRetention:  coord.BudgetRetention,
		StatusFreshness:  coord.StatusFreshness,
		CustodyRetention: coord.CustodyRetention,
		Replicas:         replicas,
		Clustered:        clustered,
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
// heartbeat and a stop's allowance are fractions of this number, so a node
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
//
// A LEAF IS CLUSTERED TOO: it holds nothing of its own, so every object it
// provisions is a replicated create on the members it reaches, and the solo
// budget would fail it against peers that are themselves still forming.
func clusteredStream(b *config.Bootstrap) bool {
	return b.BrokerKind() != placement.BrokerMember || b.Stream.Cluster.Name != ""
}

// HoldsEstate reports whether a node configured as b holds the company's
// durable state — the replicated estate, opened from boot and held whole: a
// node with the `data` role holds it, and a node without holds nothing.
//
// It is a fact about the NODE, not about the company it applies, and it is the
// one answer two callers read here rather than deriving it: the engine opening
// the file, and a backup copying it ([backup.HoldingFor]). The operator's
// `crewlet migrate` reaches the same answer by another road — Tier A requires
// a node without `data` to be scratch and refuses one with it, and the command
// refuses a scratch store before it reaches the replicated estate at all.
//
// Through the profile, which is the one parse of `node.roles`: an unreadable
// role list has already been refused by Tier A, and every other reader of the
// roles asks the same accessor.
//
// A NIL BOOTSTRAP IS EVERY ROLE, on the rule an unset role list follows: an
// engine assembled without one declares nothing to subtract.
func HoldsEstate(b *config.Bootstrap) bool {
	if b == nil {
		return true
	}
	return b.Profile("").HoldsData()
}
