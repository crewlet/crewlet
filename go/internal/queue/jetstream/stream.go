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
	"fmt"
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

// streamSpec describes the stream a subject belongs to.
//
// Retention is the interesting field, and it differs by purpose rather than
// by taste:
//
//   - Agent inboxes and the notification work queues use INTEREST retention,
//     which is precisely the engine's mailbox semantic: a message is kept
//     while a durable consumer that has not acked it exists, and publishing
//     to a subject no subscription covers drops it. The Python contract
//     states that behaviour explicitly ("publishing to a topic with NO
//     subscription drops the event silently"), so the broker enforcing it is
//     a feature, not a hazard — and it is exactly why EnsureSubscription
//     must run before anything publishes to a seat.
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

// streamSpecs is the full topology, in match order.
func streamSpecs(eventRetention time.Duration) []streamSpec {
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
	}
}

// streamForSubject reports which stream carries a subject.
//
// An unmapped subject is an error rather than a default stream: a publish
// that lands somewhere nobody consumes is the exact failure the single topic
// grammar exists to prevent, and it never raises on its own.
func streamForSubject(subject string) (string, error) {
	switch {
	case subject == "":
		return "", fmt.Errorf("%w: empty subject", ErrSubject)
	case strings.HasPrefix(subject, topics.AgentInboxPrefix):
		return streamAgent, nil
	case strings.HasPrefix(subject, topics.EventsPrefix):
		return streamEvents, nil
	case strings.HasPrefix(subject, "crewlet.notifications."):
		return streamNotifications, nil
	case strings.HasPrefix(subject, "crewlet.config."):
		return streamConfig, nil
	case strings.HasPrefix(subject, "dlq-"):
		// Dead letters live outside the crewlet.* space on purpose, so
		// the dashboard's crewlet.events.> stream cannot resurface
		// poison as live traffic. They ride the events stream's limits
		// retention because nothing consumes them automatically.
		return streamEvents, nil
	default:
		return "", fmt.Errorf("%w: %q belongs to no stream", ErrSubject, subject)
	}
}

// streamForPattern reports which stream a wildcard subscription pattern
// reads from. Patterns are only supported within one stream's subject space,
// which every engine use is: the dashboard reads crewlet.events.>.
func streamForPattern(pattern string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(pattern, ">"), "*")
	if trimmed == "" {
		return "", fmt.Errorf("%w: pattern %q spans every stream", ErrSubject, pattern)
	}
	return streamForSubject(trimmed + "x")
}

// consumerName maps a (topic, group) pair onto a durable consumer name.
//
// JetStream durable names may not contain dots, spaces, or the wildcard
// characters, while group names are already dot-free by construction
// (agent-{handle}). The topic is folded in so two subscriptions that share a
// group name on different subjects — which the engine does not do today but
// which nothing prevents — cannot collide on one consumer.
func consumerName(topic, group string) string {
	safe := func(s string) string {
		return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
	}
	return safe(group) + "__" + safe(topic)
}
