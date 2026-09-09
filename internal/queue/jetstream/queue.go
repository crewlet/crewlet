package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/events"
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
	// CA is a PEM bundle to verify the server against. Empty uses the
	// host's root pool.
	CA string

	// Cert and Key are the client certificate. Both or neither.
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

func (q *Queue) ensureStreams(ctx context.Context) error {
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
	for attempt := 0; ; attempt++ {
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
		if err == nil || !unplaceable(err) {
			return err
		}
		if attempt == 0 {
			q.log.Info("jetstream_stream_awaiting_peers", "stream", config.Name,
				"replicas", config.Replicas,
				"detail", "the cluster has not yet seen enough members to place "+
					"this stream; retrying until the provisioning deadline")
		}
		select {
		case <-ctx.Done():
			// The ORIGINAL error, not the context's: "no suitable peers"
			// says what is wrong and "deadline exceeded" does not.
			return err
		case <-time.After(streamPlacementRetry):
		}
	}
}

// unplaceable reports the transient "the cluster is still forming" error.
func unplaceable(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jsErrCodeNoPeers
}

// jsErrCodeNoPeers is JetStream's "no suitable peers for placement".
//
// Spelled here because nats.go names only a handful of its codes and this is
// not one of them — and matching on the description text instead would break
// the moment the server reworded it.
const jsErrCodeNoPeers jetstream.ErrorCode = 10005

// streamPlacementRetry is how often a forming cluster is re-asked.
//
// The same reasoning as clusterReadyPoll: short enough that a cluster which
// forms quickly is not held back by the poll, and it runs at most a few
// hundred times inside the provisioning deadline.
const streamPlacementRetry = 250 * time.Millisecond

// streamProvisionTimeout bounds one stream create.
//
// Sized to what the call actually does rather than to a generic request:
// with the metadata group already current — awaitClusterReady has returned —
// a replicated create is a RAFT round trip plus file-store setup, fast on a
// quiet cluster and seconds under load. Thirty is well past any healthy
// case and well inside the sixty the readiness wait already tolerates, so a
// genuinely wedged cluster still fails rather than hanging a boot.
const streamProvisionTimeout = 30 * time.Second

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

	storage := jetstream.FileStorage
	if q.cfg.StoreDir == "" && q.cfg.URL == "" {
		storage = jetstream.MemoryStorage
	}
	// ITS OWN DEADLINE, because the client's default is not sized for this
	// call. nats.go applies a five-second API timeout to a context with no
	// deadline, which is right for an ordinary request and wrong for the
	// one that provisions a REPLICATED stream: the engine has just waited
	// up to a minute for the metadata group precisely because that group
	// is slow to form, and then gave the call that depends on it five
	// seconds. Measured: a three-member cluster under load fails here
	// about one boot in six, reported as a bare "context deadline
	// exceeded" with nothing to say which deadline.
	//
	// WithTimeout only ever shortens against the parent, so a caller with
	// a tighter deadline of its own still wins.
	ctx, cancel := context.WithTimeout(ctx, streamProvisionTimeout)
	defer cancel()

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

// createOrObserveStream creates the stream when it is absent and COMPARES when
// it is present, writing nothing either way to a stream that already exists.
//
// The create races: two nodes booting together both find nothing and both
// create. That is fine and deliberate — the loser gets "stream name already in
// use", which is not an error here but the other node having won, so it falls
// through to the same comparison the observe path makes.
func (q *Queue) createOrObserveStream(
	ctx context.Context, spec streamSpec, config jetstream.StreamConfig,
) error {
	info, err := q.js.Stream(ctx, spec.name)
	switch {
	case err == nil:
		return q.observeStream(spec, config, info)
	case !errors.Is(err, jetstream.ErrStreamNotFound):
		return fmt.Errorf("ensure stream %s: %w", spec.name, err)
	}

	createErr := q.createStream(ctx, config)
	if createErr == nil {
		return nil
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
	// a stream this node found on the first look gets. The read gets its
	// OWN context, because the one above is the deadline that just
	// expired.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), streamReadBack)
	defer cancel()
	info, err = q.js.Stream(readCtx, spec.name)
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

// streamReadBack bounds the one read that asks whether a peer won the race.
//
// SHORT, and deliberately not the provisioning deadline: this is an ordinary
// metadata read against a group that has just proven it is working — it either
// answers in a round trip or the cluster has gone away, and inheriting thirty
// seconds would double a failing boot's time to say so.
const streamReadBack = 5 * time.Second

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

	// DURABILITY: below the configured factor refuses; equal or higher is
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
	_, getErr := q.js.Consumer(ctx, stream, name)
	existed := getErr == nil
	if getErr != nil && !errors.Is(getErr, jetstream.ErrConsumerNotFound) {
		return false, fmt.Errorf("inspect consumer %s: %w", name, getErr)
	}

	if _, err := q.ensureDurableConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: topic,
		AckPolicy:     jetstream.AckExplicitPolicy,
		// Earliest, always. A consumer created at "latest" exists and
		// still discards everything published before its first
		// consumer — which is the whole failure this call prevents.
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       q.ackWait(),
		MaxDeliver:    budgetFor(q.cfg),
	}); err != nil {
		return false, fmt.Errorf("ensure consumer %s: %w", name, err)
	}
	return !existed, nil
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
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	if cfg.Durable == "" {
		return nil, fmt.Errorf("jetstream: ensureDurableConsumer on %s was "+
			"given no durable name — an ephemeral consumer is this caller's "+
			"alone and races nobody, so it does not belong here", stream)
	}
	cons, createErr := q.js.CreateConsumer(ctx, stream, cfg)
	if createErr == nil {
		return cons, nil
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), streamReadBack)
	defer cancel()
	cons, err := q.js.Consumer(readCtx, stream, cfg.Durable)
	if err != nil {
		return nil, fmt.Errorf("%w (and it is not there: %w)", createErr, err)
	}
	return cons, nil
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
