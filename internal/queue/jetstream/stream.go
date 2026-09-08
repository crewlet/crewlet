// Package jetstream implements the EventQueue contract on NATS JetStream,
// embedded in this process by default and reachable as an external cluster
// when configured.
//
// It is the same client code either way — embedded versus external is a
// connection choice, not a second backend — which is what lets a laptop run
// the whole company with no services and a fleet run the same binary against
// a cluster.
//
// # Why JetStream fits
//
// Crewlet's subject grammar IS NATS grammar: dot-separated segments with `*`
// and `>` wildcards. More importantly, a durable consumer can be created
// with nothing attached (measured: 1.7 ms), which is the operation a seat's
// mailbox is built on — an ordinary API call here, where a broker that only
// creates a subscription by joining it needs an out-of-band admin call to
// avoid stealing a live peer's traffic.
//
// Pull consumers are the other half: nothing is pushed into a client-side
// queue, so a wedged node holds no mail it has not fetched, and resuming a
// quiesced attachment does not have to reclaim a prefetch.
//
// # What it costs, measured
//
// Three properties an engine might want are not on offer, and each shaped the
// code above this package. There is no free handoff — every path back to the
// broker increments the delivery count — and redeliveries return BEHIND
// never-delivered messages rather than replaying from the head. The first is
// absorbed by a larger delivery budget (see maxDeliver); the second is why
// conversation order comes from event timestamps rather than from the broker
// (see queue.OrderForDispatch).
//
// The third is newer and cost a red CI to learn: A CONSUMER'S COUNTERS DO NOT
// READ YOUR OWN WRITES. The server stores a message and acks the publisher,
// and only afterwards, on an offloaded goroutine, signals the consumers that
// increment their pending counts — so a consumer that already existed when
// the publish landed can report nothing pending for a message the stream
// demonstrably holds. Measured on the embedded broker at roughly two reads in
// three from a second connection. Anything that has to be synchronous with a
// returned publish therefore reads the STREAM's own state, never a consumer's
// count; see Backlog, which is where the tempting one-round-trip form was and
// what it did.
package jetstream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Stream names. Kept short and uppercase because they appear in every
// JetStream API call and in `nats stream ls` output an operator reads.
const (
	streamAgent         = "CREWLET_AGENT"
	streamEvents        = "CREWLET_EVENTS"
	streamNotifications = "CREWLET_NOTIFICATIONS"
	streamConfig        = "CREWLET_CONFIG"
	streamDeadLetter    = "CREWLET_DLQ"
	streamMemory        = topics.MemoryStream

	// derivedPrefix names a stream provisioned for a subject namespace the
	// engine does not itself define.
	derivedPrefix = "CREWLET_NS_"
)

// maxDeliver is the delivery budget before a message is dead-lettered.
//
// It covers poison, node-death AND HANDOFF — the last because a deferred
// delivery returns via Nak and that increments the count (measured), which
// is why this is 25 rather than the ~10 a broker with a free handoff needs.
// 25 leaves ample headroom: a message is
// normally handled in seconds, seat migrations are rate-limited, and a
// message would have to be in flight across 25 of them to exhaust the
// budget — a fleet thrashing that hard has a louder problem.
//
// The honest caveat stands and no cap solves it: a fast crash-loop is
// indistinguishable from poison.
const maxDeliver = 25

// budgetFor resolves the delivery budget for a queue, letting a test shrink
// it. The dead-letter assertions need a small budget; running them against
// the production default would mean dozens of handler invocations each.
func budgetFor(cfg Config) int {
	if cfg.MaxDeliver > 0 {
		return cfg.MaxDeliver
	}
	return maxDeliver
}

// ackWait bounds how long a fetched-unacked message stays invisible.
//
// Sized to a wait behind a running turn plus one worst-case turn. It is a
// backstop, not the handoff path: a seat that loses its lease Naks
// explicitly and the successor sees the message in about a millisecond.
const ackWait = 30 * time.Minute

// defaultEventRetention bounds the audit/event stream.
//
// An event store with no retention sweep, leaning on the database engine's
// own partition management, is the one table that grows without policy. Here
// the stream's own age limit is the authority and the local materialized index
// mirrors it.
const defaultEventRetention = 30 * 24 * time.Hour

// streamSpec describes the stream carrying a subject namespace.
//
// Retention is the interesting field, and it differs by PURPOSE rather than
// by taste:
//
//   - Agent inboxes and the notification work queues use INTEREST retention,
//     which is precisely the engine's mailbox semantic: a message is kept
//     while a durable consumer that has not acked it exists, and publishing
//     to a subject no subscription covers drops it. The contract states that
//     behaviour explicitly, so the broker enforcing it is a feature — and it
//     is exactly why EnsureSubscription must run before anything publishes.
//
//   - The event stream uses LIMITS retention with an age bound, because its
//     consumers are ephemeral dashboards and per-node materializers that
//     must be able to fall behind, disconnect, and catch up.
//
// # Every field is CLASSIFIED, and the classification is exhaustive
//
// A stream this process did not create is a stream some other node created,
// possibly from a different Tier A file. What a boot does about a difference
// depends entirely on WHICH field differs, and the four answers are not
// interchangeable — see [specField] and [classifyStreamSpec]. The
// classification is derived by reflection over this struct so that adding a
// field and forgetting to classify it fails a test rather than silently
// joining the class whose default it happens to match.
type streamSpec struct {
	name      string
	subjects  []string
	retention jetstream.RetentionPolicy
	maxAge    time.Duration

	// maxPerSubject retains only the newest message on each subject,
	// turning the stream from a log into a keyed table. Zero means
	// unlimited, which is what every stream but the memory changelog
	// wants.
	maxPerSubject int

	// maxBytes caps the stream's size on disk. Zero means unlimited.
	//
	// CAPACITY rather than safety: two nodes disagreeing about how big a
	// log may grow is an operator question, and a boot that refused over
	// it would take a fleet down for a number nobody is losing data to.
	maxBytes int64

	// discard decides what a full stream does. DiscardNew REFUSES the
	// append; DiscardOld drops the oldest message to make room.
	//
	// SAFETY, and the sharpest one here. A log whose records are the
	// company's own state cannot silently drop its oldest record to
	// accept a new one: the append is refused loudly instead, and the
	// operator raises the ceiling. A stream created with the wrong
	// discard policy would lose durable state with nothing reporting it.
	discard jetstream.DiscardPolicy

	// duplicates is the window in which a repeated Nats-Msg-Id is
	// collapsed rather than appended twice.
	//
	// CAPACITY: it costs memory on the server and bounds how long a retry
	// stays idempotent. A shorter window than a publisher's retry budget
	// is a correctness question for the PUBLISHER, which is why the
	// framework derives its own budget from this value rather than
	// asserting the stream's.
	duplicates time.Duration

	// denyDelete refuses the delete-message API on this stream.
	//
	// TRUE for anything holding durable state, and it is not optional: a
	// KV bucket gets DenyDelete and DenyPurge for free and a plain stream
	// does not, so a log built as a stream is deletable by any client with
	// the API unless it says otherwise. SAFETY.
	denyDelete bool

	// allowRollup permits a publisher to replace a stream's whole history
	// with one message.
	//
	// FALSE for a log, for the same reason denyDelete is true: a rollup is
	// a delete of everything below it, spelled as a publish. SAFETY.
	allowRollup bool

	// allowDirect and mirrorDirect let a client read a message from a
	// FOLLOWER rather than the leader.
	//
	// SAFETY, and false for anything the framework reads a last-message
	// answer from: a direct get is served from replica state that may be
	// behind an acknowledged write, so a discriminator built on it would
	// answer "no message here" for a message the quorum has.
	allowDirect  bool
	mirrorDirect bool
}

// specFieldClass is what a boot does when the running stream's value for a
// field differs from this node's spec.
type specFieldClass int

const (
	// classIdentity names the stream. A difference here is not a
	// difference at all — it is a different stream.
	classIdentity specFieldClass = iota

	// classSafety is a field whose value changes what the stream MEANS.
	// A mismatch REFUSES TO RUN, naming the field, the observed value and
	// the expected one: a node that carried on would be publishing durable
	// records into a stream that drops them, replays them at wall-clock
	// speed, or lets any client delete them.
	classSafety

	// classDurability is the replication factor. A mismatch refuses
	// ADMISSION TO NORMAL SERVICE when the observed factor is BELOW the
	// configured one — an R3-configured node against an R1 stream is
	// proving one copy while reporting healthy — and is fine when it is
	// equal or higher, so an R1 development node against an R3 stream
	// starts normally.
	classDurability

	// classCapacity is a ceiling. REPORTED and not acted on: it is an
	// operator's question, the operator has a verb for it, and a boot that
	// refused over a number would take a fleet down for a difference
	// nobody is losing data to.
	classCapacity
)

// classifyStreamSpec is the one place a spec field's class is decided.
//
// KEYED ON THE STRUCT FIELD NAME and asserted EXHAUSTIVE by reflection, which
// is what a hand-written list of nine could not do: `retention` and `subjects`
// were both missing from that list, and both are fields whose difference
// changes what every message on the stream means.
func classifyStreamSpec() map[string]specFieldClass {
	return map[string]specFieldClass{
		"name": classIdentity,

		"subjects":      classSafety,
		"retention":     classSafety,
		"maxAge":        classSafety,
		"maxPerSubject": classSafety,
		"discard":       classSafety,
		"denyDelete":    classSafety,
		"allowRollup":   classSafety,
		"allowDirect":   classSafety,
		"mirrorDirect":  classSafety,

		"maxBytes":   classCapacity,
		"duplicates": classCapacity,
	}
}

// engineStreams is the topology for the subjects the engine defines. Other
// namespaces are provisioned on demand — see specForSubject.
func engineStreams(eventRetention time.Duration) []streamSpec {
	if eventRetention <= 0 {
		eventRetention = defaultEventRetention
	}
	return []streamSpec{
		{
			name:      streamAgent,
			subjects:  []string{topics.AgentInboxPrefix + ">"},
			retention: jetstream.InterestPolicy,
		},
		{
			name:      streamNotifications,
			subjects:  []string{"crewlet.notifications.>"},
			retention: jetstream.InterestPolicy,
		},
		{
			name:      streamEvents,
			subjects:  []string{topics.EventsPrefix + ">"},
			retention: jetstream.LimitsPolicy,
			maxAge:    eventRetention,
		},
		{
			// Config nudges are explicitly best-effort — losing one costs
			// a poll interval, never a revision, because the
			// authoritative path polls the epoch pointer. A short age
			// bound keeps a restarted node from replaying a week of
			// stale activation announcements.
			name:      streamConfig,
			subjects:  []string{"crewlet.config.>"},
			retention: jetstream.LimitsPolicy,
			maxAge:    time.Hour,
		},
		{
			// A seat's memory, one subject per row, ONE MESSAGE PER
			// SUBJECT. That is what makes this a keyed table rather
			// than a log: the stream holds the current value of every
			// row, so a node acquiring a seat replays it in a single
			// pass and gets the memory as it stands rather than every
			// write that ever produced it.
			//
			// No maxAge. A seat's memory has no horizon here — what
			// bounds it is the learning subsystem's own lifecycle,
			// which is where the decision about what a seat should
			// still remember belongs. An age bound on this stream
			// would silently delete memory the seat still holds
			// locally, and the two would disagree about the past.
			name:          streamMemory,
			subjects:      []string{topics.MemoryPrefix + ">"},
			retention:     jetstream.LimitsPolicy,
			maxPerSubject: 1,
		},
		{
			// Dead letters are kept by age, not by interest: nothing
			// consumes them automatically, and an operator investigating
			// poison needs them to still be there.
			name:      streamDeadLetter,
			subjects:  []string{topics.DeadLetterPrefix + ">"},
			retention: jetstream.LimitsPolicy,
			maxAge:    eventRetention,
		},
	}
}

// specForSubject reports which stream carries a subject.
//
// Subjects the engine defines get their purposeful stream. Anything else
// gets a stream named for its leading segment, provisioned on demand.
//
// Provisioning rather than refusing is deliberate. The stream topology is
// this backend's business; the SUBJECT SPACE is the engine's, and an
// extension or a test that publishes under its own namespace is using the
// contract correctly. A whitelist here would make the backend the authority
// on what the engine may name — and the special case the whitelist needed
// for dead letters was the smell that said so.
//
// The mailbox semantic (interest retention) is the default for a derived
// namespace: a subject nobody subscribes to drops its messages, which is
// what the contract already promises everywhere.
func specForSubject(subject string, eventRetention time.Duration) (streamSpec, error) {
	if subject == "" {
		return streamSpec{}, fmt.Errorf("%w: empty subject", ErrSubject)
	}
	if strings.ContainsAny(subject, " \t\r\n") {
		return streamSpec{}, fmt.Errorf("%w: %q contains whitespace", ErrSubject, subject)
	}
	if strings.Contains(subject, "..") || strings.HasPrefix(subject, ".") || strings.HasSuffix(subject, ".") {
		// An empty segment is what an unroutable handle produces
		// (crewlet.agent..inbox). It is a real subject nobody subscribes
		// to, so it would swallow events silently.
		return streamSpec{}, fmt.Errorf("%w: %q has an empty segment", ErrSubject, subject)
	}

	for _, spec := range engineStreams(eventRetention) {
		for _, pattern := range spec.subjects {
			if topics.Match(pattern, subject) {
				return spec, nil
			}
		}
	}

	ns, _, _ := strings.Cut(subject, ".")
	name, err := streamNameFor(ns)
	if err != nil {
		return streamSpec{}, err
	}
	return streamSpec{
		name:      name,
		subjects:  []string{ns, ns + ".>"},
		retention: jetstream.InterestPolicy,
	}, nil
}

// specForPattern reports which stream a wildcard subscription reads from.
//
// A pattern is resolved through the subject it would match, so a broadcast
// subscription and a publish never disagree about where the messages are.
func specForPattern(pattern string, eventRetention time.Duration) (streamSpec, error) {
	probe := strings.TrimSuffix(strings.TrimSuffix(pattern, ">"), "*")
	probe = strings.TrimSuffix(probe, ".")
	if probe == "" {
		return streamSpec{}, fmt.Errorf("%w: pattern %q spans every namespace", ErrSubject, pattern)
	}
	return specForSubject(probe+".probe", eventRetention)
}

// namespaceName is the shape a subject's first segment must already have to
// be usable as a stream name: lowercase, digits and underscore.
var namespaceName = regexp.MustCompile(`^[a-z0-9_]+$`)

// streamNameFor maps a subject's namespace onto a legal stream name.
//
// It VALIDATES rather than sanitizes. JetStream forbids dots, spaces and
// wildcards in stream names, and the previous version rewrote every such
// character to an underscore — which is lossy, and a lossy map ALIASES:
// namespaces `a-b` and `a_b` both became `A_B`, so provisioning the second
// replaced the first's subject list and the first namespace's events stopped
// being captured by anything.
//
// Refusing is the honest answer and the one the contract allows: a backend
// may decline a name it cannot represent, the way it declines a TTL it cannot
// honour. It must not accept two distinct names and quietly merge them.
// Every subject the engine publishes to has a namespace in this shape
// already, so the rule costs nothing and closes the alias.
func streamNameFor(ns string) (string, error) {
	if !namespaceName.MatchString(ns) {
		return "", fmt.Errorf(
			"%w: namespace %q must match %s to be a stream name; a rewrite would let "+
				"two namespaces share one stream", ErrSubject, ns, namespaceName)
	}
	return derivedPrefix + strings.ToUpper(ns), nil
}

// consumerNameMax bounds the readable part of a durable name. NATS allows
// more; this leaves room for the hash and keeps a name an operator can read
// in a `nats consumer ls` listing.
const consumerNameMax = 180

// consumerName maps a (topic, group) pair onto a durable consumer name.
//
// It must be INJECTIVE, and making it so is the whole reason it is not just
// a string join. JetStream durable names may not contain dots, spaces or the
// wildcard characters, so the readable part has to be a lossy rewrite of both
// halves — and lossy alone aliases: topic `a.b` and topic `a_b` in one group
// produced ONE consumer, so two subscriptions shared a mailbox and each
// received the other's events. Measured by the conformance suite's
// distinct_pairs_never_share_a_subscription, which found it because it was
// the first case to SEND a pair differing only in a rewritten character —
// no amount of mutating this backend could have revealed it, because the
// input never arrived.
//
// So the readable part is for operators and the HASH is the identity. It is
// taken over the raw pair joined by a NUL, which cannot occur in either half,
// so no two distinct pairs share a digest. Forty-eight bits leaves a
// collision probability below one in ten million for a company with ten
// thousand subscriptions, against a real ceiling in the hundreds.
func consumerName(topic, group string) string {
	safe := func(s string) string {
		return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
	}
	sum := sha256.Sum256([]byte(group + "\x00" + topic))
	id := hex.EncodeToString(sum[:6])

	readable := safe(group) + "__" + safe(topic)
	// Truncation cannot reintroduce an alias: the digest is over the full
	// pair and is appended after it.
	if max := consumerNameMax - len(id) - 2; len(readable) > max {
		readable = readable[:max]
	}
	return readable + "__" + id
}

// DomainStream is the stream a statelog domain declares, in the vocabulary a
// domain owns rather than the broker's.
//
// A NARROW SURFACE ON PURPOSE. A domain says what its log IS — its name, its
// subject space, how big it may grow and what a full log does — and every
// safety field below is the framework's, identical for every domain, because
// they are the fields that decide whether an ordered log is an ordered log.
// A domain that could set them could build one that drops its oldest record
// to accept a new one.
type DomainStream struct {
	// Name is the stream. Conventionally CREWLET_<DOMAIN>_LOG.
	Name string

	// Subjects is the subject space the domain publishes into.
	Subjects []string

	// MaxBytes is the ceiling. Zero is unlimited, which no shipped domain
	// wants: a log with no ceiling fills the volume instead of refusing.
	MaxBytes int64

	// Duplicates is the window a repeated Nats-Msg-Id is collapsed in. It
	// has to outlast a publisher's whole retry budget, or a retry that
	// takes longer than the window appends the record twice.
	Duplicates time.Duration

	// Replicas is unused here and named to say so: the replication factor
	// is Tier A's, identical for every stream on the node, and a domain
	// choosing its own would be a domain choosing its own durability.
	//
	// REPLAY IS NOT HERE EITHER, and that is a correction rather than an
	// omission: replay is a CONSUMER policy in this client
	// (jetstream.ReplayPolicy on ConsumerConfig, not on StreamConfig), so
	// a stream cannot carry it and a spec field for it could never have
	// been applied. It is set where it exists — on the replication
	// consumer the framework creates — and `ReplayInstantPolicy` is the
	// client's zero value, so the guard that matters is the one asserting
	// nothing sets it to ReplayOriginal rather than one setting it here.
}

// spec renders the domain's declaration as the broker-level spec, with every
// safety field the framework's own.
func (d DomainStream) spec() streamSpec {
	return streamSpec{
		name:      d.Name,
		subjects:  d.Subjects,
		retention: jetstream.LimitsPolicy,
		maxBytes:  d.MaxBytes,

		// REFUSE THE APPEND rather than drop the oldest record. The
		// records here are the company's own state, so a full log is an
		// operator's problem and a dropped record is nobody's until the
		// day somebody reads the gap.
		discard: jetstream.DiscardNew,

		duplicates: d.Duplicates,

		// A LOG IS NOT DELETABLE and not roll-uppable. A KV bucket gets
		// both of these for free; a plain stream does not, so without
		// them any client holding the API can delete a committed record
		// or replace the whole history with one message.
		denyDelete:  true,
		allowRollup: false,

		// NO DIRECT GETS. The framework asks the broker for a subject's
		// last message to decide whether a write raced, and a direct get
		// is served from replica state that may be behind an
		// acknowledged write — so the discriminator would answer "no
		// message here" for a message the quorum already has.
		allowDirect:  false,
		mirrorDirect: false,

		// Every record is kept until the trim moves the floor. An age
		// bound here would delete state a node has not applied.
		maxAge:        0,
		maxPerSubject: 0,
	}
}
