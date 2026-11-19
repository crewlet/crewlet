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
	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// Backends is the infrastructure a node runs on.
//
// TWO SLOTS, chosen independently and validated together: the STREAM carries
// events, and COORDINATION carries seat ownership. The valid combinations are
// enumerated in config's topology validation.
//
// The STORE is not a third slot: nothing to choose or validate. It is this
// node's own rebuildable index file, here because its lifetime is the same:
// opened before anything can deliver, closed after everything has stopped.
//
// All three open and close together: one without the others hears work it
// may not do, holds seats it cannot serve, or runs turns it cannot record.
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

	// link is the meter of the link this node's connection to the broker
	// crosses ([jetstream.Queue.LinkMeter]): a leaf's data link to the
	// members, or every connection dialled to an external cluster. Nil on a
	// member, whose connection crosses none, and on backends a caller lent
	// without one. The memory sync's hydrations read it (memsync.Config.Link),
	// and so do the object store's transfers (natsobj.Config.Link).
	link memsync.Link

	// window is the ONE window of that link ([jetstream.Queue.LinkWindow]),
	// which every bulk transfer across it takes from, both ways: the object
	// store's puts, downloads and walks (natsobj.Config.Window) and the
	// memory sync's replay pulls (memsync.Config.Window), so a request a
	// node sends behind them all is answered inside its term. Nil on a
	// member, whose connection crosses no link.
	window *jsapi.Window
}

// meterOf is q's link meter as the memory sync reads it, and NIL where q has
// none: a member's queue answers a nil meter, which held in the interface
// would be a link that is there and panics on its first reading.
func meterOf(q *jetstream.Queue) memsync.Link {
	if m := q.LinkMeter(); m != nil {
		return m
	}
	return nil
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
// which snapshots the streams themselves. The embedded broker binds no socket
// on the default topology, so this connection is the only way to reach them.
//
// Nil when this node DIALLED an external broker: the queue owns that
// connection, and the cluster has its own backup tooling. The caller takes no
// ownership: closing it is [Backends.Close]'s job, and a caller that closed it
// would take this node's leases down SILENTLY, since a close this process
// makes itself is never the loss the node stops for.
func (b *Backends) Conn() *nats.Conn { return b.conn }

// CoordinationConn is the broker connection this node's coordination store
// rides, on every topology: the second connection [Backends] owns on an
// embedded broker, the queue's own on an external one. Nil only where there is
// no broker at all (a Backends built around the memory twin).
//
// For a subsystem that talks to the broker outside the queue contract whoever
// runs it: the seat memory changelog. It must not use [Backends.Conn], whose
// nil on an external broker would drop every seat's memory silently. Not the
// queue's connection on an embedded broker, so a seat's replay volume never
// sits on the read loop every mailbox consumes through. No ownership.
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

// Close releases both slots in reverse order of acquisition; each step
// depends on the one before:
//
//  1. QUEUE: consumers release unacked messages instead of leaving a peer
//     to wait out the ack timeout (untested: only the fleet suite could).
//  2. COORDINATION connection, after the queue: a node that released its
//     broker while holding leases looks alive and its seats stay unclaimable
//     for a full TTL. The lease store's watch stops first.
//  3. The embedded SERVER, if this node started one (never a dialled one).
//  4. The STORE, last because everything writes to it: closed first, a
//     drain would end in "database is closed" having recorded nothing.
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
// It does NOT re-validate the topology (config.Bootstrap.Validate does), but
// embedded-KV coordination rides the stream's broker, so the slots are not
// independent at runtime. The COMPANY gives the embedding vector width.
//
// A NIL COMPANY IS A REAL CASE: a node whose store activated no revision, and
// `-import-company`, which reads none so an unrunnable stored revision does not
// refuse the boot that replaces it. Both open at width 0 and learn it from the
// first epoch ([store.DB.LearnEmbeddingDim]); it is immutable once non-zero
// ([Engine.buildEmbedder]), since a wrong width silently empties recall.
// rec counts what a backend does from its first request; nil counts nothing.
func OpenBackends(ctx context.Context, b *config.Bootstrap, c *config.Company, rec *metrics.Recorder) (*Backends, error) {
	if b == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}
	var out *Backends
	var err error
	switch b.Stream.Type {
	case config.StreamNATS, config.StreamEmbedded, "":
		out, err = openNATS(ctx, b, rec)
	default:
		err = fmt.Errorf("engine: unknown stream type %q", b.Stream.Type)
	}
	if err != nil {
		return nil, err
	}

	// LAST, because it is the only step with something to clean up behind
	// it: the store is exclusive to one process, so a database left open by
	// a failed broker step would block the next attempt in this process.
	db, err := openStore(ctx, b, c)
	if err != nil {
		return nil, out.abandon(ctx, err)
	}
	out.Store = db

	// The node's own observability, registered once both halves exist and
	// BEFORE anything can publish: a listener attached later races the first
	// turn a restarting node picks up off its durable inbox, which would be
	// missing from the seat's record. Here, not beside the dashboard, because
	// a worker-only node with no API still keeps a record of its turns; the
	// live projection is the API's (see observe.Projector).
	//
	// ONLY WHERE THE STORE OUTLIVES THE PROCESS: a node without `data`
	// deletes its store at every boot, so it hands these rows to a data node
	// instead ([observe.Custody], armed by [New] once the estate client exists).
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
// hours later in whichever subsystem reached for it first. rec is the node's
// recorder, handed on to the embedded broker's refusals ([openStream]) and to
// the object store ([openObjects]).
func openNATS(ctx context.Context, b *config.Bootstrap, rec *metrics.Recorder) (*Backends, error) {
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
	out, conn, err := openStream(ctx, b, cfg, rec, queue.WithNode(nodeID))
	if err != nil {
		return nil, err
	}
	// BEFORE ANYTHING IS RESERVED ON THE BROKER. The queue's own streams,
	// made as it opened, reserve nothing; the object bucket below is the
	// first reservation sized from the broker's limit, and the state logs
	// follow it, so a test's node whose external broker's account states no
	// limit is refused before it sizes either from the runner's disk
	// ([ErrUndeclaredBrokerLimit]).
	if err = undeclaredBrokerLimit(ctx, b, out.Queue); err != nil {
		return nil, out.abandon(ctx, err)
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
	if err = openObjects(ctx, b, out, js, rec); err != nil {
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
	// THE BROKER'S CREDENTIALS ARE THE KEYRING'S, derived per key by the rule
	// every consumer of secrets.keys follows (ADR-0094): every route, leaf link
	// and operator connection of an embedded fleet presents one. Derived on
	// every node, a solo member's too — it opens no fleet listener, so its are
	// never presented — because which listeners a broker opens is the jetstream
	// package's to decide from the rest of this config, and one derivation for
	// every node is one fewer rule about which nodes need one.
	fleet, err := jetstream.FleetAuthFrom(b.Secrets.TokenMaterial())
	if err != nil {
		return jetstream.Config{}, fmt.Errorf("engine: stream: the broker's credentials "+
			"from secrets.keys: %w", err)
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
		FleetAuth:     fleet,
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

// ObjectStoreMismatchError is a node refused because its store.objects names
// another object store than the one the fleet keeps its files in.
//
// A TYPE, carrying both identities, because the remedy is an edit of this
// node's Tier A and which store is the FLEET'S is the one fact the operator
// cannot read off the node they are editing. And because the setting fails in
// other ways that name it too — the record unreadable, the bucket unreachable
// at open — so a caller asking whether a boot was refused for THIS reason (a
// restore that put the old store's record back, docs/guides/backup.md) asks
// the type rather than a substring the other failures share.
type ObjectStoreMismatchError struct {
	// Configured is the identity this node's store.objects names.
	Configured string
	// Fleet is the identity the fleet recorded when its first node opened a
	// store ([coord.Fleet.AgreeObjectBackend]).
	Fleet string
}

func (e *ObjectStoreMismatchError) Error() string {
	return fmt.Sprintf("engine: store.objects names %q and the fleet keeps its files in %q: "+
		"every node must name the same object store, or the company's files split "+
		"between the two — set store.objects on this node to match the others",
		e.Configured, e.Fleet)
}

// openObjects opens the backend the company's files are kept in, once the
// fleet has agreed it is the fleet's.
//
// AGREED BEFORE OPENED. The first node to open a store records which backend
// it is ([coord.ObjectStores]) and every later node compares its Tier A to
// that record: a node on another bucket would split the company's files
// between two stores with nothing failing, so it is refused by name and does
// not start.
//
// An `s3` client counts into rec from its first request (the bucket check
// below), so rec goes to its constructor, not onto the backend afterwards.
// Nil counts nothing.
func openObjects(ctx context.Context, b *config.Bootstrap, out *Backends, js natsjs.JetStream, rec *metrics.Recorder) error {
	objects := b.Store.Objects
	identity := objects.Identity()
	agreed, err := out.Fleet.AgreeObjectBackend(ctx, identity)
	if err != nil {
		return fmt.Errorf("engine: check store.objects against the fleet's object store: %w", err)
	}
	if agreed != identity {
		return &ObjectStoreMismatchError{Configured: identity, Fleet: agreed}
	}
	out.objectsIdentity = identity
	switch objects.BackendOrDefault() {
	case config.ObjectBackendS3:
		s3 := objects.S3
		backend, err := s3obj.Open(ctx, s3obj.Config{
			Endpoint: s3.Endpoint, Region: s3.Region, Bucket: s3.Bucket, Prefix: s3.Prefix,
			PathStyle: s3.PathStyle, AccessKeyID: s3.AccessKeyID, SecretAccessKey: s3.SecretAccessKey,
			Metrics: rec,
		})
		if err != nil {
			return fmt.Errorf("engine: the object store (store.objects.s3): %w", err)
		}
		out.Objects = backend
	default:
		openCtx, cancel := context.WithTimeout(ctx,
			jsprovision.Clustered(clusteredStream(b)).SequenceBudget())
		defer cancel()
		// ACROSS A LINK on every node whose connection is not to its own
		// member — a leaf's, an external cluster's client — where every
		// request sent behind the node's transfers waits for them, so they
		// take the link's ONE window, the queue's, beside the memory sync's
		// replays, and are judged by the link's meter.
		reservation, explicit := objectsReservation(ctx, b, out.Queue, volumeFree)
		cfg := natsobj.Config{
			Replicas: b.Stream.Replicas, Clustered: clusteredStream(b),
			MaxBytes: reservation, Explicit: explicit,
		}
		if acrossLink(b) {
			cfg.AcrossLink, cfg.Window = true, out.window
			if out.link != nil {
				cfg.Link = out.link
			}
		}
		backend, err := natsobj.Open(openCtx, js, cfg)
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
// The broker grants a stream its whole ceiling at creation, against one limit
// (stream.store_max_bytes, or the share of its volume the embedded broker
// takes). The state logs reserve [StreamBudgetShare] of it; mailboxes and
// coordination buckets reserve nothing. The bucket's own reservation
// (internal/objstore/natsobj says why) is sized here.
//
// IT IS SPENT BEFORE THE LOGS ARE SIZED: the object store opens first and the
// logs size from what is left ([logsGrantable]), so every restart divides the
// same remainder. With no stated limit both use the stand-in ([unstatedLimit]
// of free space), so they never reserve more than a stated limit would allow.

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
// takes: a quarter, the review's figure. The bucket is reserved first and the
// state logs take [StreamBudgetShare] of the three quarters it leaves, so the
// coordination remainder is (1 − share) of those three quarters — at five
// eighths, nine thirty-seconds of the limit — and stays as large as the bucket
// however full the bucket gets for any share up to two thirds
// (TestTheLogsShareLeavesTheEstateTheBucketsRoom).
const objectsBucketShare = 4

// budgetReader is the broker as the bucket's reservation is sized against it:
// the JetStream queue's own budget read, declared here by its one caller.
type budgetReader interface {
	StreamBudget(ctx context.Context) (jetstream.StorageBudget, error)
}

// objectsReservation is the byte reservation the nats bucket is created with,
// and whether an operator wrote it (store.objects.nats_max_bytes). An unset
// one is derived from the broker's limit ([derivedObjectsReservation]); where
// the broker states none this node can read, from [unstatedLimit] of the
// stream volume's free space, which is the one figure it has, exactly as the
// logs' sizing stands it in ([logsGrantable]). freeOf measures that space
// ([volumeFree] in production), handed in as the logs' sizing is handed its
// figure, so the stand-in is a table row rather than whatever the machine
// running the case has free.
func objectsReservation(ctx context.Context, b *config.Bootstrap, q any,
	freeOf func(dir string) (int64, error)) (int64, bool) {
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
		free, err := freeOf(volume)
		if err != nil {
			log.WarnContext(ctx, "objects_reservation_free_space_unmeasured",
				"path", volume, "error", err.Error(),
				"detail", "the nats bucket is reserved at its smallest; set "+
					"store.objects.nats_max_bytes to choose it")
		}
		limit = unstatedLimit(free)
	}
	reservation := derivedObjectsReservation(limit, b.Stream.DeclaredLogCeilings())
	log.InfoContext(ctx, "objects_reservation_derived", "bytes", reservation,
		"broker_limit", limit, "detail", "a bucket created now reserves this much "+
			"of the broker; one that exists keeps the ceiling it holds")
	return reservation, false
}

// unstatedLimit is the limit this node sizes against where the broker states
// none it can read, on a stream volume with free bytes free: the share of it
// [embeddedShareNum]/[embeddedShareDen] names, which is the cap nats-server
// holds every reservation to when no max_file_store is set — and the limit
// this engine's own embedded broker derives on the same volume.
//
// THE SHARE AND NEVER THE WHOLE VOLUME. The whole free space stood in for it
// once, which overstated the cap by a third: the bucket reserved a quarter of
// the volume — a third of the cap — and the logs half of what that left, so on
// a 16 GiB volume the bucket took 4 GiB of a 12 GiB cap, the logs' floors came
// out of the remaining 8, and every log the register gained at its 1 GiB floor
// came out of the coordination remainder every mailbox and bucket grows into.
func unstatedLimit(free int64) int64 {
	return free / embeddedShareDen * embeddedShareNum
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
// coordination should ride (ADR-0001): the queue's own on an external broker,
// on the embedded one a SECOND to the in-process server, shared with the
// seat-memory replay and the backup, off the mailboxes' read loop.
//
// Two connections fail independently, so a node could hold leases while deaf
// to its inbox: the second is dialled WATCHED ([jetstream.Queue.DialWatched])
// and NATS closing either for good stops the node ([Engine.Fatal]).
//
// A NODE WITHOUT `data` IS THE EMBEDDED BRANCH TOO, as a LEAF with no
// JetStream of its own (ADR-0025), so clients are built in the API the stream
// speaks ([jetstream.Config.API]). Refusals count into rec ([countRefusals]).
func openStream(ctx context.Context, b *config.Bootstrap, cfg jetstream.Config,
	rec *metrics.Recorder, opts ...queue.Option,
) (*Backends, *nats.Conn, error) {
	// A URL IS A BROKER SOMEBODY ELSE RUNS, and this branch is what makes
	// `stream.type: nats` mean anything: without it each node would start a
	// private in-process broker and share no stream or coordination bucket
	// with any other, with nothing saying so (a seat claimed by everyone, a
	// trigger worked twice, a token counter per node).
	if b.Stream.URL != "" {
		q, err := jetstream.Open(ctx, cfg, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: stream: %w", err)
		}
		// NO stopServer: this node dialled a broker its peers are using
		// and must never take it down. And no `conn` either — the QUEUE
		// owns this connection, so Close must not close it a second time
		// underneath the client that is still shutting down.
		return &Backends{Queue: q, link: meterOf(q), window: q.LinkWindow()}, q.Conn(), nil
	}

	server, err := jetstream.StartServer(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: stream: %w", err)
	}
	server.OnPeerRefused(countRefusals(rec))
	q, err := server.Client(ctx, opts...)
	if err != nil {
		server.Shutdown()
		return nil, nil, fmt.Errorf("engine: stream client: %w", err)
	}
	out := &Backends{Queue: q, stopServer: server.Shutdown, broker: server,
		death: server.Death(), link: meterOf(q), window: q.LinkWindow()}
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

// countRefusals is the sink a node's embedded broker hands its refusals to:
// crewlet.broker.peer.refusals, by the listener a refused connection reached
// and what it presented ([jetstream.Server.OnPeerRefused]). A nil recorder
// counts nothing.
//
// A COUNTER AND A LOG LINE, NEVER THE AUTHENTICATION TRAIL. The trail's rows
// are the identity estate's doors, and a broker connection is no principal: it
// presented a keyring key or nothing, so a refusal has no author a row could
// name, and it arrives at whatever rate the peer chooses, which a row in every
// backup must not (crewlet.auth.attempts.failed's reason). The broker's own
// broker_peer_refused line, once a minute a class, says which key and from where.
func countRefusals(rec *metrics.Recorder) func(jetstream.PeerRefusal, uint64) {
	return func(r jetstream.PeerRefusal, n uint64) {
		if rec == nil {
			return
		}
		rec.Add(metrics.BrokerPeerRefusals, n, metrics.Attrs{
			"listener": string(r.Listener), "presented": string(r.Presented)})
	}
}

// attachCoordination builds the fleet store and the lease slot on one
// connection, for both branches (a store forgotten on one is a later panic).
//
// THE FLEET STORE IS ALWAYS THE KV, whatever the coordination slot says; only
// the LEASE store follows it, as one node has nobody to fence against. Its
// RECORDS (token counter, A2A asks, claimed fires, executors, secrets) must
// outlive the PROCESS on one node as on four (stream.store_dir).
//
// THE FENCING COUNTER IS A RECORD too: a seat's epoch is written into fleet
// records (an executor row's owner_epoch). Counted in process it restarted at 1
// each boot and a new holder was refused as older than itself, so a single
// node mints from the persistent counter a fleet does (coord/kv.Epochs).
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

	stores := coordinationStoresOf(b)
	shared, err := kv.OpenFleet(ctx, js, stores.fleet)
	if err != nil {
		return fmt.Errorf("engine: coordination: %w", err)
	}
	out.Fleet = shared

	var epochs *kv.Epochs
	if b.Coordination.Type != config.CoordinationEmbeddedKV {
		epochs, err = kv.OpenEpochs(ctx, js, stores.epochs)
		if err != nil {
			return fmt.Errorf("engine: coordination: %w", err)
		}
		out.Coord = &coordmem.Backend{Epochs: epochs}
	} else {
		leases, err := kv.Open(ctx, js, stores.leases)
		if err != nil {
			return fmt.Errorf("engine: coordination: %w", err)
		}
		out.Coord, epochs = leases, leases.Epochs()
	}
	return floorSeatEpochs(ctx, epochs, shared, runsBucketHome(b))
}

// floorSeatEpochs raises each seat's fencing counter above every epoch its
// executors' rows carry, BEFORE this node claims a seat.
//
// The counter is the only source of tokens, so no row should carry one above
// it. A deployment whose epochs bucket was deleted and re-created has rows
// stamped above an empty counter, and the seat's next holder would be refused
// as older than its previous self on each: a parked run's answer never
// recorded, its resume and reap refused. Raising the counter is the whole
// repair: the next claim mints above the rows and recovery fences them.
//
// EVERY BOOT, on every topology: it costs one listing of the runs, and a floor
// at or below the counter changes nothing, so concurrent nodes agree.
func floorSeatEpochs(ctx context.Context, epochs *kv.Epochs, fleet coord.Fleet, home string) error {
	floors, err := sandbox.NewCoordStore(fleet).OwnerEpochs(ctx)
	if err != nil {
		return fmt.Errorf("engine: coordination: list the sandbox runs to floor each "+
			"seat's fencing epoch above them (the sandbox runs bucket, %s, must be "+
			"readable for the node to start): %w", home, err)
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

// coordinationStores is every configuration a node opens a coordination
// store with: the fleet store's beside the leases, the epochs counter's where
// the leases are local, and the lease store's where they are the KV's.
type coordinationStores struct {
	fleet  kv.FleetConfig
	epochs kv.EpochsConfig
	leases kv.Config
}

// coordinationStoresOf is every configuration a node configured as b opens its
// coordination stores with: at boot ([attachCoordination]) and for a command
// that wants the fleet store alone ([OpenFleetStore]).
//
// ONE FUNCTION every opening site takes its configuration from, so how a store
// reaches its buckets ([coordinationReach]) is decided in one place a test
// drives (coordreach_internal_test.go); a site assuming its connection crossed
// nothing would hold a large write across a leaf's link to the request term.
// Retentions are supplied HERE as leaseTTL is: each is a BUCKET's age, fixed at
// creation, so a silent default would decide it unseen.
func coordinationStoresOf(b *config.Bootstrap) coordinationStores {
	reach, replicas, clustered := coordinationReach(b), b.Stream.Replicas, clusteredStream(b)
	return coordinationStores{
		fleet: kv.FleetConfig{
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
			Reach:            reach,
		},
		epochs: kv.EpochsConfig{Replicas: replicas, Clustered: clustered, Reach: reach},
		leases: kv.Config{TTL: leaseTTL(b), Replicas: replicas, Clustered: clustered, Reach: reach},
	}
}

// acrossLink is whether a node configured as b reaches its broker's
// JetStream across a LINK — a leaf's link to the members, or a dial to an
// external cluster — rather than its own member in its own process: the one
// reading of it the engine hands every store on that connection, the object
// store's transfers ([openObjects]), the coordination store's writes
// ([coordinationReach]) and the memory sync's hydrations (memsync.Config,
// whose replays ride the coordination connection) alike. The queue reads the
// same fact off how its own connection was made.
func acrossLink(b *config.Bootstrap) bool { return b.BrokerKind() != placement.BrokerMember }

// memoryConfig is how a node configured as b reaches the memory changelog
// over backends' coordination connection (memsync.Config): across a link or
// not ([acrossLink]), on the stream's topology ([clusteredStream]), with
// its link's one window (Backends.window), which its replay pulls take from
// beside the object store's transfers, and reading the meter of its link
// where it crosses one (Backends.link) — what each of a hydration's requests
// is given, whether its pulls are windowed (memsync's package doc, "A window
// across a link"), and what tells a hydration held behind the link's other
// traffic from one whose copy stopped answering.
func memoryConfig(b *config.Bootstrap, backends *Backends) memsync.Config {
	return memsync.Config{
		AcrossLink: acrossLink(b),
		Clustered:  clusteredStream(b),
		Link:       backends.link,
		Window:     backends.window,
	}
}

// coordinationReach is how a node configured as b reaches the coordination
// store's buckets, which decides how long a write's bytes are given to cross
// to them (internal/coord/kv's Reach): across a link wherever its connection
// crosses one ([acrossLink]), and in its own process on a member — a member's
// coordination connection is a second one to its own in-process server.
func coordinationReach(b *config.Bootstrap) kv.Reach {
	if acrossLink(b) {
		return kv.AcrossLink
	}
	return kv.InProcess
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
// Not free to choose independently of the coordination backend: the KV
// store's expiry is bucket-wide, so it can honour only one, and the store
// REQUIRES the value rather than defaulting it (a bucket created with the
// wrong expiry is wrong for its lifetime).
//
// The default is supplied HERE from the seat layer's constant: 45 s, three
// heartbeat intervals, tolerating two missed renewals with a full interval to
// recover. Shorter drops healthy nodes' seats on jitter, each handoff costing
// an MCP respawn; longer leaves a dead node's seats dark.
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
// Not simply the configured value: the KV backend ADOPTS the lease bucket
// ([kv.Open]), so the TTL in force is whichever member created it first, and
// [kv.Store.validateTTL] refuses a longer claim. The node's own value is wrong
// both ways: SHORTER and leases lapse early; LONGER and every acquire is
// refused, so a healthy-looking node holds no seats. Derived timings (the
// heartbeat, a stop's allowance) are fractions of it and must follow it.
//
// A mismatch is logged by [kv.Open], not fatal.
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

// runsBucketHome says where the sandbox runs bucket this node reads lives,
// for an error an operator acts on: the place to look differs by broker, and
// naming stream.store_dir to a node whose bucket is on an external cluster
// sends the operator to a directory that holds none of it.
func runsBucketHome(b *config.Bootstrap) string {
	switch b.BrokerKind() {
	case placement.BrokerClient:
		return "on the external NATS cluster stream.url dials"
	case placement.BrokerLeaf:
		return "on the data members this node joins through stream.leaf.urls"
	default:
		return "on this node's embedded broker, kept under stream.store_dir"
	}
}

// clusteredStream is whether this node's broker has PEERS.
//
// The one place the engine decides it, so the queue, lease store, fleet store
// and memory sync (memsync.Config) cannot disagree about which budget they are
// on. It is [Queue.Clustered]'s rule, which the embedded server is built by:
// the cluster block takes effect only when NAMED (Tier A refuses a port or peer
// list without one).
//
// A topology question, not a replica count: an external NATS is somebody
// else's cluster ([jsprovision.Clustered]). A LEAF IS CLUSTERED TOO: every
// object it provisions is a replicated create on the members, and the solo
// budget would fail it against peers still forming.
func clusteredStream(b *config.Bootstrap) bool {
	return b.BrokerKind() != placement.BrokerMember || b.Stream.Cluster.Name != ""
}

// HoldsEstate reports whether a node configured as b holds the company's
// durable state — the replicated estate, opened from boot and held whole: a
// node with the `data` role holds it, and a node without holds nothing.
//
// A fact about the NODE, not the company it applies, read here by the engine
// opening the file and a backup copying it ([backup.HoldingFor]) rather than
// derived. `crewlet migrate` reaches the same answer another way: Tier A
// requires a node without `data` to be scratch, and the command refuses a
// scratch store. Through the profile, the one parse of `node.roles`.
//
// A NIL BOOTSTRAP IS EVERY ROLE, as an unset role list is: an engine assembled
// without one declares nothing to subtract.
func HoldsEstate(b *config.Bootstrap) bool {
	if b == nil {
		return true
	}
	return b.Profile("").HoldsData()
}
