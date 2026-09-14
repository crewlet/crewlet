package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Errors this backend reports. Callers branch on these; everything else is
// wrapped transport failure.
var (
	// ErrSubject means a subject belongs to no stream — a publish that
	// would land where nobody consumes.
	ErrSubject = errors.New("jetstream: unroutable subject")
	// ErrClosed means the queue has been stopped. It wraps
	// queue.ErrNotLive, which is what callers above this package test:
	// they must not branch on which backend is running.
	ErrClosed = fmt.Errorf("jetstream: queue closed: %w", queue.ErrNotLive)
)

// Config configures a JetStream-backed queue.
type Config struct {
	// URL is the NATS server to dial. Empty runs an EMBEDDED server in
	// this process, which is the default single-binary topology; a URL
	// points at an external cluster. Same client code either way.
	URL string

	// StoreDir is where an embedded server persists its streams. Empty
	// selects an in-memory embedded server, which is what tests want and
	// what a stateless ingress-only node can use.
	StoreDir string

	// ClusterName, ClusterURLs and ClusterPort configure an embedded
	// server that joins peers, which is the fleet topology: every node
	// embeds a member of one cluster and streams replicate between them.
	ClusterName string
	ClusterURLs []string
	ClusterPort int

	// ClusterHost is the interface this member's route listener binds.
	//
	// Empty binds EVERY interface, which is right for a node with one
	// address and wrong twice over otherwise. On a multi-homed host it is
	// a security setting: the route port is unauthenticated cluster
	// access, and a member that binds it on a public interface is
	// offering that to anyone who can reach the machine. On a host whose
	// peers are on a private network it is also the only way to say which
	// address that is.
	//
	// It says nothing about what peers are TOLD to dial — that is
	// ClusterAdvertise below, and the two differ precisely when they have
	// to.
	ClusterHost string

	// ClusterAdvertise is the address peers should dial to reach this
	// member, when that is not the address it binds.
	//
	// A member's route address travels between peers: when member A
	// accepts a route from member B it tells every peer it already has
	// where B can be found, and each of them dials that address itself.
	// With this unset the address is derived from the CONNECTION'S REMOTE
	// ADDRESS — which is correct on a flat network and wrong wherever the
	// address B is seen from is not an address anyone else can use: a
	// container with a mapped port, a NAT, a member behind a load
	// balancer. There the derived address is either unreachable or, worse,
	// somebody else's.
	//
	// Host and port both, or a bare host to keep this member's own route
	// port: "nats-1.internal:6222", "203.0.113.9".
	ClusterAdvertise string

	// ServerName is this member's identity inside the cluster. REQUIRED
	// when clustering and ignored otherwise.
	//
	// It must be unique across the cluster — NATS rejects a route from a
	// server whose name it already knows — and it must be STABLE across
	// restarts, because JetStream places replicas BY SERVER NAME. A node
	// that comes back under a new name is a new peer: its old replicas
	// are orphaned on a member that no longer exists, and the stream sits
	// short of quorum waiting for a server that will never return. So it
	// is the node's own durable id, not something generated at boot.
	ServerName string

	// Replicas is the stream replica count. 1 for solo; 3 for a fleet,
	// where it is what makes a publish quorum-durable before Publish
	// returns.
	Replicas int

	// SyncAlways makes the embedded server fsync every write before
	// acknowledging it, at every replica count.
	//
	// DECIDED BY THE OPERATOR rather than inferred from Replicas, and the
	// inference it replaces was wrong in the case that matters. "The
	// quorum IS the durability" holds for a majority that survives, and
	// the five failure classes are not one: a single host losing power
	// (quorum survives, nothing lost), a rack or a zone losing power
	// (a majority can go together), an orderly shutdown, a kernel panic
	// (page cache lost, disk intact), and a correlated power loss across
	// every member — which is the one an fsync-per-write is the only
	// defence against, and the one a same-rack three-node fleet is
	// exposed to by construction.
	//
	// It is also a claim about what Publish RETURNING means. A caller that
	// treats an ack as durable is right under this setting and optimistic
	// without it.
	SyncAlways bool

	// SyncInterval is how often the file store flushes when SyncAlways is
	// off. Zero takes nats-server's own default.
	//
	// SET EXPLICITLY rather than left to the server, because the default
	// is two minutes (server/filestore.go defaultSyncInterval) and two
	// minutes of acked-but-unflushed writes is a recovery-point objective
	// nobody chose. An operator who declines the fsync per write is
	// choosing a window, and this is the field that names it.
	SyncInterval time.Duration

	// EventRetention bounds the audit/event stream.
	EventRetention time.Duration

	// MaxDeliver overrides the delivery budget. Zero uses the derived
	// default; tests shrink it so a dead-letter assertion costs a handful
	// of handler runs rather than dozens.
	MaxDeliver int

	// AckWait overrides how long a fetched-unacked message stays
	// invisible. Zero uses the derived default.
	AckWait time.Duration

	// FetchWait and NakDelay override the consume loop's poll window and
	// the spacing before the FIRST redelivery of a failing message. Zero
	// uses the derived defaults; tests shrink both so a suite that
	// exercises redelivery does not spend its life in timers.
	FetchWait time.Duration
	NakDelay  time.Duration

	// NakCeiling caps that spacing, which doubles on every further
	// failure. Zero uses the derived default. A test that shrinks
	// NakDelay shrinks this too, or the doubling puts its later
	// redeliveries minutes apart.
	NakCeiling time.Duration

	// LookupBudget overrides the ceiling on one existence probe — the
	// "does this object already exist" read that decides
	// create-versus-observe, including however many times
	// [jsprovision.Ask] re-issues it. Zero takes [jsprovision.LookupBudget].
	//
	// IT EXISTS SO THE EXHAUSTED PROBE IS TESTABLE, which is the same
	// reason the four knobs above it exist and is not a lesser one. The
	// fall-through that keeps a boot alive when the broker says nothing
	// only runs once a probe has spent its WHOLE ceiling; at the shipped
	// thirty seconds a case proving it costs thirty seconds, so the case
	// written for it stalled one attempt instead — and [jsprovision.Ask]
	// re-asked, got a real answer, and left the branch unexercised. The
	// test passed with the branch deleted, which is the one thing a test
	// must never do.
	//
	// Nothing in the engine sets it. A deployment that wanted a different
	// ceiling would be arguing with the server's own timing, which is
	// where the number comes from.
	LookupBudget time.Duration

	// Debug hands nats-server its own debug flag, which is what unlocks
	// the broker's internal `Debugf` population.
	//
	// SEPARATE FROM THE ENGINE'S LOG LEVEL, and that separation is the
	// whole field. It used to be derived from the level this package's
	// logger was at, so `-debug` — which an operator asks for to watch
	// turns, prompts and tool calls — also subscribed them to a per
	// internal-client lifecycle trace of a broker they deliberately never
	// deployed. The engine's own coordination reads manufacture those: a
	// KV `ListKeys` is an ordered ephemeral consumer created and deleted,
	// and deleting a consumer closes the two internal JetStream clients it
	// was built on, so every key listing writes two "JetStream connection
	// closed: Client Closed" lines. Two of the node's 15-second duty loops
	// list keys on every tick.
	//
	// It says nothing about WARNINGS AND ERRORS, which nats-server emits
	// through a path this flag does not gate (server/log.go) and which the
	// bridge keeps at their own severity — the half that was actually
	// missing before it existed. Embedded only: an external cluster logs
	// wherever its operator configured it to.
	Debug bool

	// Credentials authenticates against an external server.
	Credentials string
	Token       string

	// TLS is the transport underneath that authentication: which CA to
	// trust for the server's certificate, and which certificate to
	// present when the server requires one. See [TLS].
	TLS TLS
}

// TLS is the transport material for an external NATS server.
//
// SEPARATE FROM Credentials AND Token, which authenticate the client at the
// NATS protocol layer. This is the TCP layer underneath, and a server
// configured with `tls { verify: true }` refuses a connection presenting no
// client certificate whatever credentials would have followed.
//
// There is deliberately no way to skip verification. That switch is set once
// during a bring-up and never unset, and the connection it leaves behind
// carries every event this company publishes to whoever answers on that
// address.
type TLS struct {
	// CA is the PATH to a PEM bundle to verify the server against — the
	// file, never its contents; it is opened by name. Empty uses the
	// host's root pool.
	CA string

	// Cert and Key are the PATHS to the client certificate and its private
	// key. Both or neither.
	Cert string
	Key  string
}

// IsZero reports whether nothing was configured.
func (t TLS) IsZero() bool { return t.CA == "" && t.Cert == "" && t.Key == "" }

// Queue is the JetStream implementation of queue.EventQueue.
type Queue struct {
	log *slog.Logger
	cfg Config

	// embedded is the in-process broker, when there is one. A client may
	// reference a server it does NOT own — see ownsServer.
	embedded *embeddedServer
	// ownsServer records whether stopping this client should also stop
	// the broker. Only the client that started it does; a peer stopping
	// must never take the broker down for the rest of the fleet.
	ownsServer bool
	nc         *nats.Conn
	js         jetstream.JetStream

	// attachments holds one entry per (topic, group) this process
	// consumes, keyed by the PAIR. Keying by topic alone breaks twice
	// over: a pause hold outlives its attachment so a re-attaching node
	// is silently deaf, and every group on a shared subject gets gated
	// together. Both have happened.
	mu          sync.Mutex
	attachments map[attachKey][]*attachment
	holds       map[attachKey]map[string]struct{}
	streams     map[string]struct{}
	listeners   []queue.PublishListener
	paused      bool
	closed      bool

	// inFlight counts handler invocations, which is the number an
	// operator watches converge to zero during a drain.
	inFlight queue.Inflight
}

type attachKey struct{ topic, group string }

// Open connects a queue, starting an embedded broker when no URL is given.
//
// This is the production single-binary path, where one process legitimately
// owns one broker: stopping the queue stops the broker with it. A deployment
// that needs several clients of one embedded broker — a fleet test, a peer —
// uses StartServer and Server.Client instead.
func Open(ctx context.Context, cfg Config) (*Queue, error) {
	if cfg.URL != "" {
		return newQueueOn(ctx, cfg, nil, false)
	}
	e, err := startEmbedded(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("start embedded nats: %w", err)
	}
	q, err := newQueueOn(ctx, cfg, e, true)
	if err != nil {
		e.shutdown()
		return nil, err
	}
	return q, nil
}

// newQueueOn builds a client against an already-running broker (or an
// external URL when embedded is nil).
func newQueueOn(ctx context.Context, cfg Config, embedded *embeddedServer, owns bool) (*Queue, error) {
	q := &Queue{
		log:         logging.Get("queue.jetstream"),
		cfg:         cfg,
		embedded:    embedded,
		ownsServer:  owns,
		attachments: map[attachKey][]*attachment{},
		holds:       map[attachKey]map[string]struct{}{},
		streams:     map[string]struct{}{},
	}

	var err error
	if embedded != nil {
		q.nc, err = embedded.connect()
	} else {
		q.nc, err = dial(cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	if q.js, err = jetstream.New(q.nc); err != nil {
		q.nc.Close()
		return nil, fmt.Errorf("open jetstream: %w", err)
	}
	// A clustered member accepts connections long before its metadata
	// group has a leader, and creating a replicated stream against a
	// leaderless group BLOCKS rather than failing — so provisioning here
	// hangs with nothing to diagnose. Waited for at the point that
	// actually depends on it: doing it in StartServer instead would make
	// the first member of a fresh cluster wait for a quorum that cannot
	// exist until the peers it is blocking have started.
	if err := embedded.awaitClusterReady(ctx, q.cfg.Replicas); err != nil {
		q.nc.Close()
		return nil, err
	}
	if err := q.ensureStreams(ctx); err != nil {
		q.nc.Close()
		return nil, err
	}
	return q, nil
}

// ensureStreams provisions the engine's own streams, under ONE ceiling for
// the whole sequence.
//
// The per-create budget bounds one create and this bounds the run of them,
// because without it the real worst case is the product rather than the term:
// each stream discovers a wedged cluster independently and spends its own
// budget doing so. [jsprovision.SequenceBudget] is that ceiling, and because
// WithTimeout only ever shortens, each create below still takes the lesser of
// its own budget and what is left of this one.
func (q *Queue) ensureStreams(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx,
		q.Clustered().SequenceBudget())
	defer cancel()

	for _, spec := range engineStreams(q.cfg.EventRetention) {
		if err := q.ensureStream(ctx, spec); err != nil {
			return err
		}
	}
	return nil
}

// createStream provisions a stream, waiting out a cluster that is still
// forming.
//
// RETRIED ON PLACEMENT ONLY. "No suitable peers" means the metadata group
// has not yet seen enough members to place a replicated stream — a
// transient, self-clearing condition during cluster formation, and the one
// remaining boot hazard after awaitClusterReady: that check answers for THIS
// member's own catch-up, while placement needs the OTHER members to have
// joined, and a follower cannot observe the peer list at all
// (JetStreamClusterPeers answers only on the leader). So the placement
// attempt is the signal.
//
// Every other error is returned at once. A bad subject, a conflicting
// retention, an auth failure — none of them clears by waiting, and retrying
// would turn a config mistake into a thirty-second hang with the same
// message at the end.
func (q *Queue) createStream(ctx context.Context, config jetstream.StreamConfig) error {
	return jsprovision.Place(ctx, q.Clustered().AskTerm(), func(ctx context.Context) error {
		// CREATE, NOT CreateOrUpdate, and the difference is the whole
		// race guard above rather than a preference.
		//
		// CreateOrUpdate never returns [jetstream.ErrStreamNameAlreadyInUse]
		// — it UPDATES instead — so the caller's "a peer won the race,
		// read what it made" branch was unreachable and the losers of a
		// simultaneous boot each rewrote a configuration they already
		// agreed with. Measured on three engines starting together: the
		// update request never returns, and the boot fails after its
		// whole provisioning deadline naming a stream rather than a
		// cluster.
		//
		// It is also the honest ownership rule, and [Queue.observeStream]
		// states it: a running stream's configuration is not something a
		// booting node writes — "one node writing a shared stream's
		// configuration at boot is how a ceiling an operator raised gets
		// silently lowered".
		_, err := q.js.CreateStream(ctx, config)
		if refusedStorage(err) {
			// NAMED, because the broker's own words name no number: see
			// [ErrInsufficientStorage]. Named INSIDE the placement
			// retry rather than around it, so the refusal that is not
			// a placement failure still leaves at once.
			return fmt.Errorf("%w: %w", ErrInsufficientStorage, err)
		}
		return err
	}, func() {
		q.log.Info("jetstream_stream_awaiting_peers", "stream", config.Name,
			"replicas", config.Replicas,
			"detail", "the cluster has not yet seen enough members to place "+
				"this stream; retrying until the provisioning deadline")
	})
}

// provisionBudget is how long one stream create on this queue gets, which
// depends on whether it has peers to agree with — see [jsprovision].
func (q *Queue) provisionBudget() time.Duration {
	return q.Clustered().Budget()
}

// lookupBudget is the ceiling on one existence probe on this queue — see
// [Config.LookupBudget] for why it is overridable at all.
func (q *Queue) lookupBudget() time.Duration {
	if q.cfg.LookupBudget > 0 {
		return q.cfg.LookupBudget
	}
	return jsprovision.LookupBudget
}

// Clustered is whether this queue's broker has peers, which is the fact every
// provisioning budget branches on.
//
// THE NAME IS WHAT DECIDES IT, because the name is what [embeddedOptions]
// gates the whole cluster block on: without it the route port and the peer
// list are dropped and the server starts solo, so a predicate that counted
// either of them would promise a clustered budget to a broker that never
// clusters. Tier A refuses that shape outright (stream.cluster.name is
// required once a port or peers is set), which is what makes one field
// sufficient here.
//
// A URL is the other half: somebody else's broker, whose metadata group is
// remote, so a create on it is never the local file-store setup the solo
// budget is sized for.
//
// Read off the TOPOLOGY rather than off Replicas, which is a proxy that
// reports a single-replica clustered member as solo — see
// [jsprovision.Clustered].
func (q *Queue) Clustered() jsprovision.Clustered {
	return jsprovision.Clustered(q.cfg.URL != "" || q.cfg.ClusterName != "")
}

// ensureStream provisions one stream, remembering that it did so. Streams
// are idempotent to create, but the round trip is not free and this runs on
// every publish to a derived namespace.
func (q *Queue) ensureStream(ctx context.Context, spec streamSpec) error {
	q.mu.Lock()
	_, known := q.streams[spec.name]
	q.mu.Unlock()
	if known {
		return nil
	}

	storage := q.storage()
	config := jetstream.StreamConfig{
		Name:              spec.name,
		Subjects:          spec.subjects,
		Retention:         spec.retention,
		Storage:           storage,
		Replicas:          max(q.cfg.Replicas, 1),
		MaxAge:            spec.maxAge,
		MaxMsgsPerSubject: int64(spec.maxPerSubject),
		MaxBytes:          spec.maxBytes,
		Discard:           spec.discard,
		Duplicates:        spec.duplicates,
		DenyDelete:        spec.denyDelete,
		AllowRollup:       spec.allowRollup,
		AllowDirect:       spec.allowDirect,
		MirrorDirect:      spec.mirrorDirect,
	}
	if spec.maxPerSubject == 0 {
		// The client spells "unlimited" as -1; a zero would be read as a
		// stream that retains nothing at all.
		config.MaxMsgsPerSubject = -1
	}
	if spec.maxBytes == 0 {
		config.MaxBytes = -1
	}
	// CREATE IF ABSENT, OBSERVE IF PRESENT.
	//
	// A running stream's configuration has ONE writer and a booting node is
	// not it. This used to apply this node's own spec on every boot, which
	// is N nodes writing one shared configuration from N possibly-different
	// Tier A files, resolved by boot order: a node that came up late with a
	// smaller ceiling silently lowered one an operator had just raised, and
	// it did so with Tier A UNCHANGED, because max(M, M) is M.
	//
	// So the writer is removed rather than guarded, and what remains is a
	// comparison. See observeStream for what each class of difference does.
	// A BREADCRUMB, for [jsprovision.WhenSlow]'s reason: this step is
	// otherwise silent until its budget expires, and the one progress line
	// below it covers only the placement-retry path — not the case that
	// actually hangs, where the API request itself never returns.
	//
	// On the BOOT's context, which is this function's, so the watch spans
	// the read-back that follows a lost create as well as the create — the
	// one stretch where a stalled member says nothing is exactly the one
	// that outlived the per-create deadline. The deferred stop is what
	// ends it either way.
	stop := jsprovision.WhenSlow(ctx, func(after time.Duration) {
		q.log.WarnContext(ctx, "jetstream_stream_slow", "stream", spec.name,
			"replicas", config.Replicas, "waited", after,
			"detail", "this stream is still being provisioned; on a fleet that "+
				"is a metadata group that has not settled, and the next line "+
				"from this node says whether it got past it")
	})
	defer stop()

	if err := q.createOrObserveStream(ctx, spec, config); err != nil {
		return err
	}

	q.mu.Lock()
	if q.streams == nil {
		q.streams = map[string]struct{}{}
	}
	q.streams[spec.name] = struct{}{}
	q.mu.Unlock()
	return nil
}

// storage is the class every stream this queue creates is stored in: memory on
// an embedded server given no store directory, which is what an in-memory
// server means, and the file store everywhere else.
func (q *Queue) storage() jetstream.StorageType {
	if q.cfg.StoreDir == "" && q.cfg.URL == "" {
		return jetstream.MemoryStorage
	}
	return jetstream.FileStorage
}

// createOrObserveStream creates the stream when it is absent and COMPARES when
// it is present, writing nothing either way to a stream that already exists.
//
// The create races: two nodes booting together both find nothing and both
// create. That is fine and deliberate — the loser gets "stream name already in
// use", which is not an error here but the other node having won, so it falls
// through to the same comparison the observe path makes.
//
// ctx IS THE BOOT'S and the per-create deadline is derived below rather than
// by the caller, because the read-back at the end runs precisely when that
// deadline has expired — see [jsprovision.Settle]. Handed the expired one it
// would ask once and give up, which is the retry being dead on the one path it
// was written for.
func (q *Queue) createOrObserveStream(
	ctx context.Context, spec streamSpec, config jetstream.StreamConfig,
) error {
	// THE LOOKUP AND THE CREATE GET SEPARATE DEADLINES, because they are
	// separate operations and only one of them is a raft round trip.
	//
	// Both had their own deadline before this — the client's default is
	// five seconds for a context that carries none, and an engine boot's
	// carries none — but they shared ONE, sized for the create. So a
	// lookup that went unanswered spent the whole clustered budget and
	// left nothing of it for the create it exists to inform. See
	// [jsprovision.LookupBudget] for why a metadata read is sized by the
	// server's own hold window instead.
	//
	// WithTimeout only ever shortens against the parent, so a caller with
	// a tighter deadline of its own still wins — which is also what makes
	// the sequence ceiling [Queue.ensureStreams] applies effective, and
	// what keeps two deadlines from costing more than the one they
	// replaced.
	lookupCtx, cancelLookup := context.WithTimeout(ctx, q.lookupBudget())
	// A BREADCRUMB ON THE LOOKUP TOO, and it is the one that was missing:
	// this is the FIRST call to reach the metadata group for this stream,
	// so a member stalled against a group that has not settled waits here
	// — while the watcher around the create could not fire, because the
	// create had not started.
	stopLookup := jsprovision.WhenSlow(lookupCtx, func(after time.Duration) {
		q.log.WarnContext(ctx, "jetstream_stream_lookup_slow", "stream", spec.name,
			"waited", after,
			"detail", "still asking whether this stream already exists; on a "+
				"fleet that is a metadata group that has not settled, and the "+
				"create has not been attempted yet")
	})
	// THROUGH [jsprovision.Ask], so a request the group never answered is
	// re-issued inside the ceiling above rather than being the whole of it.
	var info jetstream.Stream
	err := jsprovision.Ask(lookupCtx, q.Clustered().AskTerm(),
		func(ctx context.Context) error {
			var e error
			info, e = q.js.Stream(ctx, spec.name)
			return e
		}, nil)
	stopLookup()
	cancelLookup()
	switch {
	case err == nil:
		return q.observeStream(spec, config, info)
	case errors.Is(err, jetstream.ErrStreamNotFound):
		// TOLD it is absent. Create it below.
	case jsprovision.Unanswered(ctx, err):
		// TOLD NOTHING, which is not the same fact and must not be
		// reported as one — see [jsprovision.Unanswered]. The create
		// below covers both things this lookup failed to say, so the
		// boot continues rather than failing on a question nobody
		// answered.
		q.log.WarnContext(ctx, "jetstream_stream_lookup_unanswered",
			"stream", spec.name, "error", err.Error(),
			"detail", "the broker did not say whether this stream exists, so "+
				"the create below decides it: absent and it is made, present "+
				"and it comes back as a peer's win and is compared")
	default:
		return fmt.Errorf("ensure stream %s: %w", spec.name, err)
	}

	createCtx, cancel := context.WithTimeout(ctx, q.provisionBudget())
	defer cancel()
	createErr := q.createStream(createCtx, config)
	if createErr == nil {
		return nil
	}
	if jsprovision.Unplaceable(createErr) {
		// STILL FORMING and it stayed that way for the whole budget,
		// which createStream has already waited out. The metadata
		// leader REFUSED to place this stream, so nothing was placed
		// and there is nothing to become visible — reading back would
		// spend the window asking after an object nobody made, and
		// append a misleading not-found to the error that says what is
		// actually wrong. [openBucket] has always gated this; the
		// stream path did not, which is the same rule written twice
		// and drifting.
		return fmt.Errorf("ensure stream %s: %w", spec.name, createErr)
	}
	// A PEER MAY HAVE WON THE RACE, and it announces that in two shapes
	// rather than one.
	//
	// The tidy shape is [jetstream.ErrStreamNameAlreadyInUse]: this
	// node's create arrived after the winner's had committed.
	//
	// The other shape is a TIMEOUT, and it is the one a fleet booting
	// together actually produces. Two members create the same stream in
	// the same instant; the server commits one and holds the other while
	// the metadata group settles, and the held request outlives the
	// caller's deadline. The stream is there — the loser simply never
	// heard so. Reported as a failure, that is a node refusing to boot
	// because a peer beat it, on a cluster where everything worked.
	//
	// So the question is re-asked rather than assumed either way: does
	// the stream exist now? A read-back is one round trip, it answers
	// exactly that, and it holds whatever it finds to the same comparison
	// a stream this node found on the first look gets.
	//
	// RE-ASKED rather than answered once: the winner's create is visible
	// to this member only on its next metadata update, so a single lookup
	// inside that window reports not-found for a stream that exists and
	// fails the boot before [Queue.DomainLog]'s own retry could help. ON
	// ctx AND NOT createCtx, because createCtx is the deadline that just
	// expired — [jsprovision.Settle] owns this read's own short window.
	err = jsprovision.Settle(ctx, func(ctx context.Context) error {
		var e error
		info, e = q.js.Stream(ctx, spec.name)
		return e
	})
	if err != nil {
		// THE CREATE'S ERROR IS WHAT IS REPORTED, with the read-back's
		// beside it: "no suitable peers" or "deadline exceeded" on the
		// create says what went wrong, and "not found" on the read
		// only says the create really did fail.
		return fmt.Errorf("ensure stream %s: %w (and it is not there: %w)",
			spec.name, createErr, err)
	}
	q.log.Info("jetstream_stream_created_by_peer", "stream", spec.name,
		"create_error", createErr.Error(),
		"detail", "this node's own create did not complete and the stream "+
			"exists, which is a peer having won the race; its configuration "+
			"is compared here exactly as one found on the first look would be")
	return q.observeStream(spec, config, info)
}

// observeStream compares a running stream against this node's spec and decides
// per FIELD CLASS what the difference means. It writes nothing.
func (q *Queue) observeStream(
	spec streamSpec, want jetstream.StreamConfig, live jetstream.Stream,
) error {
	got := live.CachedInfo().Config
	unsafe := safetyDifferences(want, got)
	if len(unsafe) > 0 {
		// REFUSES TO RUN. Every field here changes what a message on this
		// stream means, so a node that carried on would be publishing
		// durable records into a stream that drops them, replays them at
		// wall-clock speed, or lets any client delete them.
		return fmt.Errorf(
			"jetstream: the running stream %q does not match this node's "+
				"configuration on %d safety field(s): %s. Nothing here is "+
				"applied to a stream that already exists — one node writing "+
				"a shared stream's configuration at boot is how a ceiling an "+
				"operator raised gets silently lowered — so this is an "+
				"operator gesture: align the Tier A of every node, or resize "+
				"the stream deliberately",
			spec.name, len(unsafe), strings.Join(unsafe, "; "))
	}

	// DURABILITY, which is the one answer with no field class behind it:
	// the replication factor is Tier A's rather than the spec's, so it is
	// compared here directly. Below the configured factor REFUSES ADMISSION
	// TO NORMAL SERVICE — an R3-configured node against an R1 stream is
	// proving one copy while reporting healthy — and equal or higher is
	// fine, so an R1 development node against an R3 stream starts.
	if got.Replicas < want.Replicas {
		return fmt.Errorf(
			"jetstream: the running stream %q is replicated %dx and this node "+
				"is configured for %dx: an acknowledged publish would be "+
				"proving fewer copies than stream.replicas promises",
			spec.name, got.Replicas, want.Replicas)
	}

	// CAPACITY: reported, never applied.
	for _, d := range capacityDifferences(want, got) {
		q.log.Info("jetstream_stream_capacity_differs", "stream", spec.name,
			"difference", d,
			"detail", "reported rather than applied: a stream's ceiling is "+
				"changed by an operator gesture, not by whichever node booted "+
				"last")
	}
	return nil
}

// safetyDifferences lists the safety-class fields on which a running stream
// differs from this node's spec, each rendered as "field: got X, want Y".
func safetyDifferences(want, got jetstream.StreamConfig) []string {
	var out []string
	add := func(field string, gotV, wantV any) {
		if fmt.Sprint(gotV) != fmt.Sprint(wantV) {
			out = append(out, fmt.Sprintf("%s: running %v, this node %v", field, gotV, wantV))
		}
	}
	add("subjects", got.Subjects, want.Subjects)
	add("retention", got.Retention, want.Retention)
	add("max_age", got.MaxAge, want.MaxAge)
	add("max_msgs_per_subject", got.MaxMsgsPerSubject, want.MaxMsgsPerSubject)
	add("discard", got.Discard, want.Discard)
	add("deny_delete", got.DenyDelete, want.DenyDelete)
	add("allow_rollup", got.AllowRollup, want.AllowRollup)
	add("allow_direct", got.AllowDirect, want.AllowDirect)
	add("mirror_direct", got.MirrorDirect, want.MirrorDirect)
	add("storage", got.Storage, want.Storage)
	return out
}

// capacityDifferences lists the capacity-class fields, which are reported.
func capacityDifferences(want, got jetstream.StreamConfig) []string {
	var out []string
	if got.MaxBytes != want.MaxBytes {
		out = append(out, fmt.Sprintf("max_bytes: running %d, this node %d",
			got.MaxBytes, want.MaxBytes))
	}
	if got.Duplicates != want.Duplicates {
		out = append(out, fmt.Sprintf("duplicates: running %v, this node %v",
			got.Duplicates, want.Duplicates))
	}
	return out
}

// EnsureDomainStream provisions the stream a statelog domain declares.
//
// The generic path, and it is deliberately not a new entry in engineStreams:
// a domain's stream arrives WITH the domain, so the engine's own stream table
// stays the list of streams the engine itself defines. It is also what lets a
// test stand up a throwaway log stream without touching that table.
func (q *Queue) EnsureDomainStream(ctx context.Context, spec DomainStream) error {
	return q.ensureStream(ctx, spec.spec())
}

// streamFor resolves the stream carrying a subject, provisioning it when the
// subject belongs to a namespace the engine does not itself define.
func (q *Queue) streamFor(ctx context.Context, subject string) (string, error) {
	// CHECKED HERE, at the one door every broker-touching verb goes
	// through, and BEFORE the stream cache below. Without it a closed
	// client surfaces "nats: connection closed" from wherever the
	// transport happened to give up — a message that names neither this
	// queue nor its lifecycle, and that a caller above internal/queue
	// cannot match against without knowing which backend is running.
	//
	// Before the cache, because ensureStream remembers what it has already
	// provisioned: a verb whose stream was cached never reached the
	// transport at all, so whether a stopped queue refused depended on
	// what some other caller had ensured earlier in the process. That is
	// the worst shape a lifecycle answer can have — correct most of the
	// time, and a function of unrelated history.
	if q.isClosed() {
		return "", ErrClosed
	}
	spec, err := specForSubject(subject, q.cfg.EventRetention)
	if err != nil {
		return "", err
	}
	if err := q.ensureStream(ctx, spec); err != nil {
		return "", err
	}
	return spec.name, nil
}

// ackWait resolves the ack-timeout for this queue.
func (q *Queue) ackWait() time.Duration {
	if q.cfg.AckWait > 0 {
		return q.cfg.AckWait
	}
	return ackWait
}

// Conn exposes this client's NATS connection, for subsystems that ride the
// same broker outside the queue contract (the KV coordination backend).
// The queue keeps ownership: closing it is Stop's job, not the caller's.
func (q *Queue) Conn() *nats.Conn { return q.nc }

// DialOwned opens a SECOND connection to the same broker, which the caller
// owns and closes.
//
// # Why a caller would want its own rather than [Queue.Conn]
//
// Because some subsystems close what they are given, and they are right to:
// the state log's snapshot DONOR serves for the life of a node and shuts its
// connection down when it stops, which is the honest lifetime for a
// long-running server of a request/reply subject.
//
// Handed the queue's own connection, that close takes the ENGINE's broker
// with it — every publish, every consumer and the coordination store, all
// through one `nc.Close()` in a subsystem that thought it owned what it had.
// Worse where the queue was BORROWED: a caller that lent a broker to an
// engine gets it back closed.
//
// So the ownership is in the name. Callers that ride the shared connection
// take [Queue.Conn] and must not close it; callers with their own lifetime
// take this and must.
func (q *Queue) DialOwned() (*nats.Conn, error) {
	if q.embedded != nil {
		return q.embedded.connect()
	}
	if q.cfg.URL == "" {
		return nil, fmt.Errorf("jetstream: this queue has no embedded server " +
			"and no URL, so a second connection cannot be opened")
	}
	return dial(q.cfg)
}

// Backend names this backend for operator display. Nothing may branch on it.
func (q *Queue) Backend() string {
	if q.embedded != nil {
		return "jetstream-embedded"
	}
	return "jetstream"
}

// Start satisfies the contract; streams and connection are established in
// Open, so there is nothing deferred to do here.
func (q *Queue) Start(context.Context) error { return nil }

// Publish sends an event, returning only once the broker has acknowledged
// it — with replicas configured, that acknowledgement is a quorum commit, so
// "published" means "survives losing this node".
func (q *Queue) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if topic == "" {
		// An empty subject is what an unroutable handle produces. It must
		// not become a real subject nobody reads.
		return fmt.Errorf("%w: empty topic", ErrSubject)
	}
	if _, err := q.streamFor(ctx, topic); err != nil {
		return err
	}
	q.mu.Lock()
	closed := q.closed
	listeners := append([]queue.PublishListener(nil), q.listeners...)
	q.mu.Unlock()
	if closed {
		return ErrClosed
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("serialize event %s: %w", ev.Type, err)
	}
	if _, err := q.js.Publish(ctx, topic, data); err != nil {
		// TRANSLATED, not passed through. "Too large" is the one publish
		// failure a producer must not retry, and nats.ErrMaxPayload is
		// this backend's private word for it — a caller matching on that
		// would be branching on which backend is running, which the
		// contract forbids. Wrapped rather than replaced so the original
		// still reads in a log.
		if errors.Is(err, nats.ErrMaxPayload) {
			return fmt.Errorf("publish %s: %d bytes exceeds the %d-byte limit: %w: %w",
				topic, len(data), queue.MaxPayloadBytes, queue.ErrTooLarge, err)
		}
		return fmt.Errorf("publish %s: %w", topic, err)
	}

	// Listeners run inline, and must: the event store's writer is one of
	// them, and it has to see the event as part of the
	// publish rather than racing a consumer. Their failures are logged and
	// never propagate — telemetry must not be able to fail a publish.
	for _, l := range listeners {
		q.callListener(ctx, l, topic, ev)
	}
	return nil
}

func (q *Queue) callListener(ctx context.Context, l queue.PublishListener, topic string, ev *events.Event) {
	defer func() {
		if r := recover(); r != nil {
			queue.LogListenerPanic(q.log, topic, ev, r)
		}
	}()
	l(ctx, topic, ev)
}

// AddPublishListener registers a listener called inline on every publish.
func (q *Queue) AddPublishListener(l queue.PublishListener) {
	if l == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.listeners = append(q.listeners, l)
}

// EnsureSubscription creates the durable consumer with NO consumer attached,
// positioned at the earliest message.
//
// This is the operation a seat's mailbox is built on, and on JetStream it is
// an ordinary API call taking about a millisecond — no admin endpoint, no
// risk of stealing a live peer's traffic. It must run before anything
// publishes to the subject: the agent and notification streams use interest
// retention, so a message published to a subject no consumer covers is
// dropped, which is the contract's stated behaviour rather than a surprise.
func (q *Queue) EnsureSubscription(ctx context.Context, topic, group string) (bool, error) {
	if group == "" {
		return false, fmt.Errorf("%w: empty group", ErrSubject)
	}
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		return false, err
	}
	name := consumerName(topic, group)

	// Reporting whether THIS call created it is part of the contract, so
	// look first rather than inferring from an upsert.
	//
	// ITS OWN DEADLINE, and it is a LOOKUP's rather than a create's — see
	// [jsprovision.LookupBudget]. With no deadline of its own (an engine
	// boot's context has none) this reached nats.go under the client's
	// five-second default, so on a clustered boot the lookup expired
	// before the create it precedes ever ran.
	//
	// THE BUDGET WAS NEVER THE WHOLE FIX, and saying it was is what left
	// this path broken after it: a [context.DeadlineExceeded] is not
	// [jetstream.ErrConsumerNotFound], so a longer deadline only bought a
	// longer wait before the same `inspect consumer …: context deadline
	// exceeded` failed the same boot. What removes the shape is reading an
	// unanswered lookup as the third value it is and falling through to
	// the create below.
	lookupCtx, cancelLookup := context.WithTimeout(ctx, q.lookupBudget())
	defer cancelLookup()
	// AND ITS BREADCRUMB, because a budget without one just moves where
	// the silence is. This is the FIRST call to reach the metadata group
	// on this path, so a member stalled against a group that has not
	// settled now waits here — and said nothing, while the watcher around
	// the create below could not fire because the create had not started.
	stopLookup := jsprovision.WhenSlow(lookupCtx, func(after time.Duration) {
		q.log.WarnContext(ctx, "jetstream_consumer_lookup_slow", "stream", stream,
			"consumer", name, "waited", after,
			"detail", "still asking whether this mailbox already exists; on a "+
				"fleet that is a metadata group that has not settled, and the "+
				"create has not been attempted yet")
	})
	getErr := jsprovision.Ask(lookupCtx, q.Clustered().AskTerm(),
		func(ctx context.Context) error {
			_, e := q.js.Consumer(ctx, stream, name)
			return e
		}, nil)
	stopLookup()
	// EXISTED MEANS THIS NODE WAS TOLD IT WAS THERE, never merely "the
	// lookup did not fail". An unanswered probe leaves it false and the
	// create below decides: the returned bool is `!existed && won`, and
	// won is the authority on which node's create actually made it.
	existed := getErr == nil
	switch {
	case getErr == nil, errors.Is(getErr, jetstream.ErrConsumerNotFound):
		// Told, either way.
	case jsprovision.Unanswered(ctx, getErr):
		q.log.WarnContext(ctx, "jetstream_consumer_lookup_unanswered",
			"stream", stream, "consumer", name, "error", getErr.Error(),
			"detail", "the broker did not say whether this mailbox exists, so "+
				"the create below decides it rather than the boot failing on "+
				"a question nobody answered")
	default:
		return false, fmt.Errorf("inspect consumer %s: %w", name, getErr)
	}

	cons, won, err := q.ensureDurableConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: topic,
		// The pair, verbatim. The durable name cannot carry it (it is a
		// lossy rewrite plus a digest), and ListSubscriptions has to hand
		// back exactly what the caller created. Carried on the CREATE, and
		// stamped onto a consumer that predates the field by the alignment
		// below, because ensureDurableConsumer creates rather than upserts
		// and so writes nothing to a consumer that is already there.
		Metadata:  subscriptionMetadata(topic, group),
		AckPolicy: jetstream.AckExplicitPolicy,
		// Earliest, always. A consumer created at "latest" exists and
		// still discards everything published before its first
		// consumer — which is the whole failure this call prevents.
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       q.ackWait(),
		MaxDeliver:    budgetFor(q.cfg),
	})
	if err != nil {
		return false, fmt.Errorf("ensure consumer %s: %w", name, err)
	}
	// BOTH HALVES, because either alone misreports. The lookup says this
	// node did not already see it; won says this node's own create is what
	// put it there. Without won, a consumer recovered by the read-back —
	// a peer's, or this node's from an earlier boot — was reported as
	// created by THIS call, so on a fleet booting together every member
	// claimed to have made every mailbox.
	//
	// One ambiguity is the broker's and cannot be closed here: a create
	// whose configuration exactly matches an existing consumer returns
	// that consumer with no error, and is indistinguishable from having
	// made it. It costs a log line rather than a decision — nothing in the
	// engine branches on this bool.
	if !won {
		q.stampSubscriptionPair(ctx, stream, cons, topic, group)
	}
	return !existed && won, nil
}

// stampSubscriptionPair records the pair on a consumer this call FOUND rather
// than made, when the consumer does not already carry it.
//
// # Why it takes a call of its own
//
// A mailbox created before consumers carried their pair is exactly the one
// ListSubscriptions exists for: a seat removed before this build shipped left
// it, and pairOf can then only recover the pair from the durable name, which
// is a lossy rewrite and refuses to guess when the rewrite lost something (a
// dotted group, a space). Left unstamped, that mailbox is warned about on
// every maintenance sweep and retired by none of them.
//
// The create cannot do it. [Queue.ensureDurableConsumer] issues an ActionCreate
// and the broker answers an existing consumer whose configuration differs with
// ErrConsumerExists, writing nothing, which is what the read-back beside it
// then recovers. So the stamp is a second, deliberate write.
//
// THE LIVE CONFIGURATION IS WHAT IS SENT BACK, with only the two metadata keys
// set on top of it. Every other field stays as the broker holds it, because a
// booting node is not the writer of a running consumer's ack window or
// delivery budget, and rebuilding the config from this build's defaults is how
// one node's Tier A silently becomes the fleet's.
//
// BEST EFFORT, and reported rather than returned: the mailbox exists and
// carries mail either way, so failing the ensure would refuse a seat over a
// cosmetic write, while the listing degrades exactly as it did before the
// metadata existed. The one that cannot be recovered is already logged by
// [Queue.subscriptionOf].
func (q *Queue) stampSubscriptionPair(ctx context.Context, stream string,
	cons jetstream.Consumer, topic, group string) {

	if cons == nil {
		return
	}
	info := cons.CachedInfo()
	if info == nil {
		return
	}
	if md := info.Config.Metadata; md[metaTopic] == topic && md[metaGroup] == group {
		return
	}
	config := info.Config
	config.Metadata = maps.Clone(config.Metadata)
	if config.Metadata == nil {
		config.Metadata = map[string]string{}
	}
	maps.Copy(config.Metadata, subscriptionMetadata(topic, group))

	// ITS OWN BUDGET, for the reason [Queue.alignDomainConsumer] gives: an
	// UpdateConsumer is a write against the same metadata group as the
	// create, and the caller's context either carries the deadline that
	// just expired or carries none at all, which hands nats.go its
	// five-second default.
	writeCtx, cancel := context.WithTimeout(ctx, q.provisionBudget())
	defer cancel()
	if _, err := q.js.UpdateConsumer(writeCtx, stream, config); err != nil {
		q.log.WarnContext(ctx, "jetstream_subscription_pair_unstamped",
			"stream", stream, "topic", topic, "group", group, "error", err.Error(),
			"detail", "this mailbox still carries no subscription pair, so a "+
				"listing recovers it from the durable name and leaves it out "+
				"when the name cannot prove it; the mailbox itself is unaffected")
	}
}

// ensureDurableConsumer creates a durable consumer, tolerating a PEER having
// created the same one at the same moment.
//
// # Why a durable consumer needs this and an ephemeral one does not
//
// A durable consumer is named, and every node of a fleet ensures the SAME
// names at boot: a seat's mailbox, the notification feed, a change feed. So on
// a fleet starting together N members issue one call for one name, and the
// server commits one while holding the rest — and a held request outlives the
// caller's deadline. The consumer is there; the losers never heard so, and a
// node that reported that as a failure would refuse to boot because a peer
// beat it.
//
// An ephemeral consumer is nobody else's, so none of this applies and the
// callers that make one do not come through here.
//
// A read-back is one round trip and answers exactly the question — does the
// consumer exist now — so the timeout is re-asked rather than assumed either
// way. It gets its OWN context, because the caller's may be the deadline that
// just expired.
func (q *Queue) ensureDurableConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, bool, error) {

	if cfg.Durable == "" {
		return nil, false, fmt.Errorf("jetstream: ensureDurableConsumer on %s was "+
			"given no durable name — an ephemeral consumer is this caller's "+
			"alone and races nobody, so it does not belong here", stream)
	}
	// ITS OWN DEADLINE, for the reason createOrObserveStream gives — and
	// this call needed it more, not less. A durable consumer is a
	// replicated object on the same metadata group, but nothing here set a
	// budget, so the caller's context reached nats.go with no deadline of
	// its own (an engine boot's has none) and the client's FIVE-SECOND
	// default applied. That is shorter than the clustered budget by a
	// factor of twenty-four, and shorter than [jsprovision.SlowAfter] — so
	// the breadcrumb below could never fire and the retry window it
	// describes did not exist.
	//
	// NOT SHADOWING ctx, because the read-back at the end runs when THIS
	// deadline has expired and must not inherit it.
	createCtx, cancel := context.WithTimeout(ctx, q.provisionBudget())
	defer cancel()

	// THE SAME BREADCRUMB the stream and bucket creates carry, and this
	// call needs it for the same reason: a durable consumer is a replicated
	// object too, and `open consumer …: context deadline exceeded` on a
	// silent member is one of the shapes a clustered boot failed in.
	stop := jsprovision.WhenSlow(createCtx, func(after time.Duration) {
		q.log.WarnContext(ctx, "jetstream_consumer_slow", "stream", stream,
			"consumer", cfg.Durable, "waited", after,
			"detail", "this durable consumer is still being created; on a fleet "+
				"that is a metadata group that has not settled")
	})
	var cons jetstream.Consumer
	// WAITED OUT, like the stream and bucket creates. A durable consumer
	// on a replicated stream is placed by the same metadata group at the
	// same moment of the same boot, so "no suitable peers" is exactly as
	// transient here — and without the retry the clustered budget above
	// bought this call nothing, because the one condition it exists to
	// wait out was the one condition this create did not wait on.
	createErr := jsprovision.Place(createCtx, q.Clustered().AskTerm(), func(ctx context.Context) error {
		var e error
		cons, e = q.js.CreateConsumer(ctx, stream, cfg)
		return e
	}, func() {
		q.log.Info("jetstream_consumer_awaiting_peers", "stream", stream,
			"consumer", cfg.Durable,
			"detail", "the cluster has not yet seen enough members to place "+
				"this consumer; retrying until the provisioning deadline")
	})
	stop()
	if createErr == nil {
		return cons, true, nil
	}
	if jsprovision.Unplaceable(createErr) {
		// STILL UNPLACEABLE after the whole budget, so nothing was
		// placed and there is nothing to read back — the same gate the
		// stream and bucket creates take.
		return nil, false, createErr
	}
	// RE-ASKED, like the stream and bucket read-backs: a peer's create is
	// visible to this member only on its next metadata update, so one
	// lookup answers at an arbitrary instant inside that window and fails
	// a boot over a consumer that exists. ON ctx AND NOT createCtx, for
	// the reason [jsprovision.Settle] gives: createCtx may be the deadline
	// that just expired.
	err := jsprovision.Settle(ctx, func(ctx context.Context) error {
		var e error
		cons, e = q.js.Consumer(ctx, stream, cfg.Durable)
		return e
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w (and it is not there: %w)", createErr, err)
	}
	// FOUND, NOT MADE — somebody else's create is what put it there.
	return cons, false, nil
}

// DeleteSubscription destroys the durable consumer and the mail it retains.
//
// Deliberately does not require a local attachment: decommissioning a role
// must not depend on which node happened to be running the seat.
func (q *Queue) DeleteSubscription(ctx context.Context, topic, group string) (bool, error) {
	if _, err := q.Detach(ctx, topic, group); err != nil {
		return false, err
	}
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		return false, err
	}
	err = q.js.DeleteConsumer(ctx, stream, consumerName(topic, group))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		// Deleting an absent subscription is success, not an error: the
		// caller's intent is "this must not exist", and it does not.
		return false, nil
	default:
		return false, fmt.Errorf("delete consumer for %s/%s: %w", topic, group, err)
	}
}

// Consumer metadata keys recording the pair a durable consumer was created for.
// Namespaced so they cannot collide with a key an operator or a tool adds.
const (
	metaTopic = "crewlet.topic"
	metaGroup = "crewlet.group"
)

func subscriptionMetadata(topic, group string) map[string]string {
	return map[string]string{metaTopic: topic, metaGroup: group}
}

// subscriptionStreams names the streams the engine itself defines, taken from
// the one topology that declares them so a stream added there is listed here
// without a second edit.
var subscriptionStreams = sync.OnceValue(func() map[string]struct{} {
	specs := engineStreams(0)
	names := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		names[spec.name] = struct{}{}
	}
	return names
})

// carriesSubscriptions reports whether a stream on this broker is a SUBJECT
// SPACE, which is the only kind of stream a subscription can exist on.
//
// The engine's own streams plus every namespace stream provisioned on demand,
// and deliberately not the plain CREWLET_ prefix they share. A state-log
// stream is named under that prefix too and its readers are durable consumers,
// but they are an applier's cursor rather than a mailbox: listed, each would be
// a consumer no (topic, group) addresses, so every sweep would warn about a
// live subsystem and offer an operator its deletion. Coordination buckets are
// out either way, since a KV bucket's stream is named KV_.
func carriesSubscriptions(stream string) bool {
	if strings.HasPrefix(stream, derivedPrefix) {
		return true
	}
	_, ok := subscriptionStreams()[stream]
	return ok
}

// ListSubscriptions reports every durable consumer on this backend's streams
// whose topic matches topicPattern, as the pair it was created for.
//
// EVERY SUBJECT STREAM, ENUMERATED, for the reason the backup takes the same
// shape: a namespace stream exists only once something published to it, so a
// list of known streams would miss exactly the mailboxes nobody remembers. A
// pattern is not mapped onto one stream either, because a pattern like
// "crewlet.>" spans several. What is enumerated is bounded by
// carriesSubscriptions, because the same broker also carries streams that are
// no subject space at all.
//
// An ephemeral consumer (a stream subscription, a peek, a memory replay) is not
// a subscription and is skipped. A durable consumer is listed under the pair
// its metadata records when that pair derives its name; one whose metadata
// carries no such pair (an older build made it, or something rewrote it) is
// recovered from its name when that can be proven (see pairOf and
// pairFromConsumerName), and one that cannot be is logged and left out rather
// than listed under a guessed pair.
func (q *Queue) ListSubscriptions(ctx context.Context, topicPattern string) ([]queue.Subscription, error) {
	if q.isClosed() {
		return nil, ErrClosed
	}
	if topicPattern == "" {
		return nil, fmt.Errorf("%w: a subscription listing needs a topic pattern; pass \">\" for "+
			"every subscription", ErrSubject)
	}

	// Collected before any consumer is listed, and each lister drained to
	// its end: the client's listers publish on unbuffered channels from a
	// goroutine of their own, so abandoning one part-way leaks it.
	names := q.js.StreamNames(ctx)
	var streams []string
	for name := range names.Name() {
		if carriesSubscriptions(name) {
			streams = append(streams, name)
		}
	}
	if err := names.Err(); err != nil {
		return nil, fmt.Errorf("list streams: %w", err)
	}

	var out []queue.Subscription
	for _, name := range streams {
		stream, err := q.js.Stream(ctx, name)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			// Deleted between the two reads; it holds nothing now.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open stream %s: %w", name, err)
		}
		consumers := stream.ListConsumers(ctx)
		for info := range consumers.Info() {
			sub, ok := q.subscriptionOf(ctx, name, info)
			if ok && topics.Match(topicPattern, sub.Topic) {
				out = append(out, sub)
			}
		}
		if err := consumers.Err(); err != nil {
			return nil, fmt.Errorf("list consumers of stream %s: %w", name, err)
		}
	}
	slices.SortFunc(out, func(a, b queue.Subscription) int {
		if c := strings.Compare(a.Topic, b.Topic); c != 0 {
			return c
		}
		return strings.Compare(a.Group, b.Group)
	})
	return out, nil
}

// subscriptionOf reads the pair a consumer was created for, reporting false for
// a consumer that is not a durable subscription or whose pair cannot be known,
// and logging the second.
func (q *Queue) subscriptionOf(ctx context.Context, stream string, info *jetstream.ConsumerInfo) (queue.Subscription, bool) {
	sub, verdict := pairOf(info)
	if verdict == pairUnprovable {
		q.log.WarnContext(ctx, "jetstream_subscription_unnamed", "stream", stream,
			"consumer", info.Config.Durable, "filter_subject", info.Config.FilterSubject,
			"detail", "a durable consumer names no subscription pair that addresses it, so it is "+
				"left out of the subscription listing; if nothing uses it, delete it with the "+
				"nats CLI")
	}
	return sub, verdict == pairListed
}

// pairVerdict is what a consumer's config says about the subscription it is.
type pairVerdict int

const (
	// pairListed is a durable subscription whose pair is proven.
	pairListed pairVerdict = iota
	// pairNotSubscription is a consumer that is no subscription at all: an
	// ephemeral stream subscription, a peek or a replay. Skipped silently,
	// because every dashboard socket holds one and each is working as
	// designed.
	pairNotSubscription
	// pairUnprovable is a durable consumer whose pair nothing proves.
	pairUnprovable
)

// pairOf reads the pair a consumer was created for.
//
// A PAIR IS LISTED ONLY WHEN IT ADDRESSES THIS CONSUMER: the durable name the
// pair derives (consumerName) must be this consumer's own. That holds for the
// metadata as much as for a name recovered by pairFromConsumerName, because the
// caller acts on the pair rather than on the consumer: a retirement sweep
// deletes consumerName(topic, group), so metadata naming any other pair (edited
// by a tool, or copied onto another consumer) would send it to delete a
// subscription it never looked at while this one kept its mail. Metadata that
// fails the check falls through to the name, which proves its own pair or
// nothing.
func pairOf(info *jetstream.ConsumerInfo) (queue.Subscription, pairVerdict) {
	if info == nil || info.Config.Durable == "" {
		return queue.Subscription{}, pairNotSubscription
	}
	name := info.Config.Durable
	if topic, group := info.Config.Metadata[metaTopic], info.Config.Metadata[metaGroup]; topic != "" && group != "" &&
		consumerName(topic, group) == name {
		return queue.Subscription{Topic: topic, Group: group}, pairListed
	}
	topic := info.Config.FilterSubject
	if group, ok := pairFromConsumerName(name, topic); ok {
		return queue.Subscription{Topic: topic, Group: group}, pairListed
	}
	return queue.Subscription{}, pairUnprovable
}

// InFlightCount reports handler invocations currently mid-flight.
func (q *Queue) InFlightCount() int { return q.inFlight.Count() }

// PauseDelivery stops dispatching new events while leaving Publish working,
// so in-flight handlers can still emit their terminal events. One-way: once
// paused, the engine is shutting down.
func (q *Queue) PauseDelivery(context.Context) error {
	q.mu.Lock()
	q.paused = true
	atts := q.attachmentsLocked()
	q.mu.Unlock()
	for _, a := range atts {
		a.setPaused(true)
	}
	return nil
}

// WaitForHandlers waits for in-flight handlers, returning how many were
// still running when the wait ended. Zero means a clean drain; non-zero
// means the timeout expired, which is not an error — the caller owns any
// "too long" policy.
func (q *Queue) WaitForHandlers(ctx context.Context, timeout time.Duration) (int, error) {
	return q.inFlight.Wait(ctx, timeout)
}

// stopGrace bounds how long Stop waits for one consume loop to exit.
//
// Short on purpose. It covers a loop NOTICING its cancellation, not a
// handler finishing: WaitForHandlers is the API for that, and a graceful
// drain calls it first. Waiting on handlers here would make Stop hang for as
// long as the longest turn — on a path whose whole job is to let go.
const stopGrace = 250 * time.Millisecond

// Stop closes every attachment and the connection, and shuts down the
// embedded server when this process is running one.
func (q *Queue) Stop(ctx context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	atts := q.attachmentsLocked()
	q.attachments = map[attachKey][]*attachment{}
	// Pause holds are process-local state about attachments that no longer
	// exist. Carrying them past a stop would leave a restarted queue
	// silently deaf on subjects nothing is holding any more.
	q.holds = map[attachKey]map[string]struct{}{}
	q.mu.Unlock()

	// Stop attachments concurrently so one unresponsive consumer bounds
	// this step instead of stranding every attachment behind it.
	var wg sync.WaitGroup
	for _, a := range atts {
		wg.Go(func() {
			a.close()
			a.wait(stopGrace)
		})
	}
	wg.Wait()

	if q.nc != nil {
		q.nc.Close()
	}
	q.shutdownServer()
	return nil
}

func (q *Queue) shutdownServer() {
	if q.ownsServer && q.embedded != nil {
		q.embedded.shutdown()
		q.embedded = nil
	}
}

func (q *Queue) attachmentsLocked() []*attachment {
	out := make([]*attachment, 0, len(q.attachments))
	for _, group := range q.attachments {
		out = append(out, group...)
	}
	return out
}

// deadLetter publishes a message the delivery budget has exhausted, to a
// subject outside the crewlet.* space so the dashboard's crewlet.events.>
// stream cannot resurface poison as live traffic.
func (q *Queue) deadLetter(ctx context.Context, topic, group string, data []byte) {
	subject := topics.DeadLetter(topic, group)
	if _, err := q.streamFor(ctx, subject); err != nil {
		q.log.Error("dead_letter_failed", "topic", topic, "group", group, "error", err.Error())
		return
	}
	if _, err := q.js.Publish(ctx, subject, data); err != nil {
		q.log.Error("dead_letter_failed", "topic", topic, "group", group, "error", err.Error())
	}
}

var _ queue.EventQueue = (*Queue)(nil)
