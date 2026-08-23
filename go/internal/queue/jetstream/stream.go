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
// mailbox is built on. Pulsar needs an admin REST call for that because
// joining a Shared subscription to create it steals a live peer's traffic;
// here it is an ordinary API call, and that whole workaround disappears.
//
// Pull consumers remove the other Pulsar hazard: nothing is pushed into a
// client-side queue, so a wedged node holds no mail it has not fetched, and
// resuming a quiesced attachment does not have to reclaim a prefetch.
//
// # Where it differs, and what that cost
//
// Two Pulsar behaviours do not carry over, both measured — see
// rewrite/decisions/102-jetstream-redelivery.md. There is no free handoff
// (every path back to the broker increments the delivery count), and
// redeliveries return BEHIND never-delivered messages rather than replaying
// from the head. The first is absorbed by a larger delivery budget; the
// second is why conversation order comes from event timestamps rather than
// from the broker.
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

	// derivedPrefix names a stream provisioned for a subject namespace the
	// engine does not itself define.
	derivedPrefix = "CREWLET_NS_"
)

// maxDeliver is the delivery budget before a message is dead-lettered.
//
// On Pulsar this covered poison ∧ node-death. On JetStream it must also
// cover HANDOFF, because a deferred delivery returns via Nak and that
// increments the count (measured). 25 leaves ample headroom: a message is
// normally handled in seconds, seat migrations are rate-limited, and a
// message would have to be in flight across 25 of them to exhaust the
// budget — a fleet thrashing that hard has a louder problem.
//
// The honest caveat the Python engine records still stands and no cap
// solves it: a fast crash-loop is indistinguishable from poison.
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
// The Python event store had NO retention sweep at all and leaned on
// TimescaleDB chunking, which made it the one table that grew without
// policy. Here the stream's own age limit is the authority and the local
// materialized index mirrors it.
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
type streamSpec struct {
	name      string
	subjects  []string
	retention jetstream.RetentionPolicy
	maxAge    time.Duration
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
