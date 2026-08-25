// Package pulsar implements the EventQueue contract on Apache Pulsar.
//
// This is the MULTI-TENANT backend. Embedded JetStream runs a company on one
// binary with no services; Pulsar runs many companies on one estate, one
// tenant per company, with the broker enforcing the boundary between them.
// That is the whole reason this backend exists, which is why Tenant and
// Namespace are required configuration here rather than defaulted (see
// Config.Validate).
//
// # How Crewlet's subject language lands on Pulsar
//
// The engine speaks one subject language everywhere: dot-separated segments
// with `*` and `>` wildcards (internal/queue/topics). Here a subject becomes
// a persistent topic under the configured tenant and namespace —
// persistent://{tenant}/{namespace}/{subject} — a consumer group becomes a
// SHARED subscription named after the group, and a broadcast pattern becomes
// a per-caller regex subscription over the namespace.
//
// # The two Pulsar-shaped decisions a reader should not have to re-derive
//
// **Subscription lifecycle goes through the admin REST API, never by
// subscribing.** A seat's subscription is Shared, so a second consumer
// joining one an owner is actively serving takes a share of that seat's live
// traffic into its own prefetch — measured at 12 of 20 messages
// (src/crewlet/queue/admin.py, tests/test_queue/test_broker_behavior.py). A
// node that "created" every seat's subscription by subscribing at boot would
// manufacture exactly the double-consumer split-brain seat ownership exists
// to prevent. See admin.go.
//
// **A graceful consumer close is the free way to hand work back.** Re-measured
// on Pulsar 4.0.6 with this client: closing a consumer that holds an unacked
// message takes 1.8 ms, and a fresh consumer receives it 8.6 ms later still
// at redeliveryCount 0 — where an ack timeout costs one. (The Python engine's
// harness measured the same free handoff on 4.2.4 with the C++ client: 9 ms,
// redeliveryCount 0. See decisions/104-pulsar-redelivery-economics.md
// for the full table, and 102 for the JetStream column it contrasts with.)
//
// So Defer here means what the contract says it means — leave it unacked,
// stop consuming — and never a NAK. This is the property JetStream does NOT
// have, and the reason Capabilities.FreeDeferral is a capability rather than
// a requirement.
//
// # What this client cannot do, and what changed because of it
//
// github.com/apache/pulsar-client-go is not the C++ client the Python engine
// drove. Two absences are load-bearing and are handled at their definitions:
//
//   - There is no consumer ack timeout (no ConsumerOptions.AckTimeout). The
//     Python engine's 30-minute unacked-message window does not exist here,
//     which deletes the batch dispatch budget outright — see
//     dispatchBudgetIsNotPorted in batch.go.
//   - There is no exported RedeliverUnacknowledgedMessages. Reclaiming a
//     quiesced consumer's prefetch is done by RECYCLING the consumer —
//     close, reopen — which on Pulsar is free. See attachment.reopen.
package pulsar

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Errors this backend reports. Callers branch on these; everything else is
// wrapped transport failure.
var (
	// ErrSubject means a subject cannot become a Pulsar topic — a publish
	// that would land where nobody can consume, or nowhere at all.
	ErrSubject = errors.New("pulsar: unroutable subject")
	// ErrClosed means the queue has been stopped.
	ErrClosed = errors.New("pulsar: queue closed")
	// ErrConfig means the configuration cannot produce a working client.
	ErrConfig = errors.New("pulsar: invalid configuration")
)

// --- tuned constants ------------------------------------------------------
//
// A tuned constant either carries verbatim from the previous engine or has to
// be re-derived for this broker, and which one it is depends on what the value
// was measured against. The broker here IS Pulsar, so the old values carry with
// their reasoning intact — but the CLIENT is different, and where that changes
// the reasoning it is said at the definition rather than assumed away.

const (
	// maxDeliveries is the delivery budget before a message is routed to
	// the dead-letter topic.
	//
	// Ten, carried from the Python engine (_MAX_REDELIVER), because in a
	// fleet the counter has two causes and only one of them is a bad
	// message: a redelivery means "the handler failed" (poison) but it
	// also means "a node died holding this" — measured, an ack-timeout
	// redelivery increments the counter. Three was sized for a
	// single-node world; in a fleet it silently became a budget of three
	// node deaths per message.
	//
	// It does NOT have to cover handoffs, which is where JetStream had to
	// re-derive it upward to 25 (d-102 decision 2). Here a handoff closes
	// its consumer and the message comes back at redeliveryCount 0.
	//
	// COUNTING CONVENTION, stated because this repo holds both: Pulsar's
	// DLQPolicy.MaxDeliveries counts TOTAL deliveries, so 10 means the
	// handler sees a persistently failing message ten times. NATS
	// MaxDeliver counts the same way; the in-memory twin counts
	// redeliveries AFTER the first. queuetest asks backends for total
	// ATTEMPTS precisely so the suite never has to guess which.
	maxDeliveries = 10

	// nakRedeliveryDelay spaces out the redelivery of a FAILING message.
	//
	// Pulsar's own default is 60 s, which is far too slow for a handler
	// that failed on a transient fault. One second is the Python value
	// and the reasoning is unchanged: fast enough that a retry is not a
	// stall, slow enough that a permanently failing handler is not a hot
	// loop against whatever is broken.
	nakRedeliveryDelay = time.Second

	// receiverQueueSize is how many messages a durable consumer
	// prefetches into its local queue.
	//
	// Set EXPLICITLY, because the client default is 1000 and that default
	// is a liability rather than a throughput win: prefetched messages are
	// delivered-but-unacked and hostage to this consumer until it closes.
	// On the C++ client that hostage window was bounded by the 30-minute
	// ack timeout. THIS client has no ack timeout at all, so the window is
	// bounded only by the connection dying — which makes a small prefetch
	// MORE important here, not less.
	//
	// Throughput does not argue back: turns are serialized per seat, one
	// at a time, minutes each, so prefetching beyond a batch buys nothing
	// but hostages. 64 covers the default max_batch of 20 three times
	// over, so the batcher's zero-linger drain still fills a full batch
	// from local state. Measured on Pulsar 4.2.4: the cap is real —
	// exactly 64 messages were unreachable by a peer.
	receiverQueueSize = 64

	// streamReceiverQueueSize is the same knob for the dashboard's
	// broadcast consumers.
	//
	// Different tradeoff, so a different number: these are non-durable,
	// start at the latest message, and their events are display-only —
	// nothing that matters is lost if one is dropped. What is bounded here
	// is memory, one queue per connected dashboard, and a browser that
	// stops reading must not make the client buffer 1000 events for it.
	streamReceiverQueueSize = 200

	// receiveWait bounds one blocking receive.
	//
	// A poll interval, not a hostage window: nothing extra is fetched by
	// waiting, and a receive that times out leaves the message where it
	// was. Short enough that a quiesce, a pause hold or a shutdown takes
	// effect promptly; long enough that an idle seat is not spinning.
	// Carried from the Python engine's _RECEIVE_TIMEOUT_MS.
	receiveWait = time.Second

	// drainWait is the receive window used when collecting the tail of a
	// batch that is already sitting in the local receiver queue. Kept
	// short: it is the only added latency a single-message batch pays.
	drainWait = 50 * time.Millisecond

	// consumeErrorBackoff is how long a consume loop waits after an
	// unexpected error before its next receive.
	//
	// The loop is guarded so one bad message cannot end a seat's inbox,
	// and a guard with no pause is its own hazard: a fault that recurs
	// every iteration becomes a hot loop with a log line per turn of it.
	consumeErrorBackoff = time.Second

	// autoDiscoveryPeriod is how often a broadcast subscription re-scans
	// the namespace for topics its pattern now matches.
	//
	// JUDGEMENT CALL, not a measurement — flagged as such because the
	// number trades broker load against how long a brand-new seat's first
	// events are invisible on every dashboard. Pulsar's client default is
	// 60 s, which the Python engine inherited and documented as a known
	// lag ("a brand-new agent's first events may lag the stream by that
	// interval"). A minute of a live feed showing nothing looks exactly
	// like a quiet company, which is the failure mode the dashboard's
	// health chrome exists to prevent — so 60 s is too long.
	//
	// Five seconds costs one namespace topic-list lookup per stream
	// subscriber per five seconds. Dashboards are counted in open browser
	// tabs, so that is single-digit lookups per second on a metadata read.
	// Re-measure this against real fan-out before raising the subscriber
	// count by an order of magnitude.
	autoDiscoveryPeriod = 5 * time.Second

	// publishAttempts and publishRetryBase bound how hard a publish tries
	// before giving up.
	//
	// A momentarily slow or unreachable broker must not drop an event: the
	// producer is created with DisableBlockIfQueueFull so a stalled broker
	// makes Send fail HERE rather than parking the caller — and the caller
	// is usually a handler holding a seat's only turn. Five attempts with
	// exponential backoff from 500 ms covers a broker restart; past that
	// the failure is real and the caller must hear about it.
	publishAttempts  = 5
	publishRetryBase = 500 * time.Millisecond

	// adminTimeout bounds one admin REST call. These run at boot, at seat
	// decommission and behind a singleton lease — never on a delivery hot
	// path — so the budget is sized for a broker under load rather than
	// for latency. Ten seconds is Pulsar's own client default.
	adminTimeout = 10 * time.Second

	// stopGrace bounds how long Stop waits for one consume loop to exit.
	//
	// Short on purpose. It covers a loop NOTICING its cancellation, not a
	// handler finishing: WaitForHandlers is the API for that and a
	// graceful drain calls it first. Waiting on handlers here would make
	// Stop hang for as long as the longest turn, on a path whose whole job
	// is to let go.
	stopGrace = 250 * time.Millisecond

	// detachGrace bounds how long DeleteSubscription waits for this
	// process's own consumer to close before asking the broker to delete.
	//
	// Longer than stopGrace because the broker REFUSES to delete a
	// subscription that still has a connected consumer, so this wait is
	// the difference between a decommission that works and one that
	// reports a 412 the caller has to interpret. Still bounded: a handler
	// that never returns must not turn a decommission into a hang.
	detachGrace = 5 * time.Second

	// dlqRetainSubscription is created on each dead-letter topic when its
	// first message is routed there.
	//
	// Without it the dead-letter topic has no subscription, and Pulsar
	// deletes a message published to a topic no subscription covers —
	// immediately. The poison message the budget just spent ten deliveries
	// establishing would be destroyed on arrival, which is the opposite of
	// what a dead-letter topic is for ("a poison message is PRESERVED, not
	// destroyed"). Nothing consumes it; it exists so the backlog is kept
	// for an operator, and the namespace's own retention/TTL policy is
	// what bounds it.
	dlqRetainSubscription = "crewlet-dlq"
)

// --- subject translation --------------------------------------------------

// pulsarNamePattern is the legal shape of a tenant or namespace name. It
// mirrors internal/config's rule for the same fields, deliberately: an
// operator must not be able to write a name the config accepts and the
// backend rejects.
var pulsarNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// nsPath is the tenant/namespace segment that scopes every topic name.
func (c Config) nsPath() string { return c.Tenant + "/" + c.Namespace }

// fullTopic maps an internal subject onto a fully-qualified Pulsar topic.
func (c Config) fullTopic(subject string) string {
	return "persistent://" + c.nsPath() + "/" + subject
}

// localSubject recovers the internal subject from a persistent://t/ns/x name.
//
// Used to tell a broadcast handler WHICH subject an event arrived on, which
// is the only routing information a pattern subscriber gets.
func (c Config) localSubject(fullTopic string) string {
	rest := fullTopic
	if _, after, found := strings.Cut(fullTopic, "://"); found {
		rest = after
	}
	// tenant/namespace/subject — a subject never contains '/', which
	// checkSubject enforces on the way in.
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return fullTopic
	}
	return parts[2]
}

// checkSubject rejects the subjects that would silently misroute.
//
// Every rejection here is a message that would otherwise be published
// somewhere no consumer looks, with no error anywhere:
//
//   - an EMPTY SEGMENT is what an unroutable handle produces
//     (topics.AgentInbox("") already returns "", but a caller that built the
//     string another way gets crewlet.agent..inbox, a real topic nobody
//     subscribes to);
//   - a '/' would be read as a tenant/namespace separator and land the event
//     in a different namespace entirely — or in another company's;
//   - whitespace and the wildcard characters are not legal in a topic name
//     and turn into a lookup failure at publish time rather than a
//     configuration error at boot.
func checkSubject(subject string) error {
	switch {
	case subject == "":
		return fmt.Errorf("%w: empty subject", ErrSubject)
	case strings.ContainsAny(subject, " \t\r\n"):
		return fmt.Errorf("%w: %q contains whitespace", ErrSubject, subject)
	case strings.Contains(subject, "/"):
		return fmt.Errorf("%w: %q contains '/', which names a namespace, not a subject", ErrSubject, subject)
	case strings.ContainsAny(subject, "*>"):
		return fmt.Errorf("%w: %q contains a wildcard; only SubscribeStream takes patterns", ErrSubject, subject)
	case strings.HasPrefix(subject, "."), strings.HasSuffix(subject, "."), strings.Contains(subject, ".."):
		return fmt.Errorf("%w: %q has an empty segment", ErrSubject, subject)
	}
	return nil
}

// patternRegex translates a subject-wildcard pattern into the topic regex a
// Pulsar pattern consumer takes.
//
// Mirrors topics.Match exactly, because a broadcast subscriber that quietly
// receives too few events looks exactly like a quiet company:
//
//   - `*` matches exactly one segment ([^.]+);
//   - `>` matches one or more trailing segments (.+) and is terminal;
//   - everything else is literal.
//
// The result is the LOCAL half only. The caller prefixes it with
// persistent://{tenant}/{namespace}/ — the client parses that prefix to learn
// which namespace to watch, then matches the remainder against each
// fully-qualified topic name it finds there. The trailing `$` is what stops
// `*` over-matching a deeper topic: without it crewlet.events.* would also
// match crewlet.events.task.created, turning a per-domain dashboard filter
// into a firehose with nothing to notice it by.
func patternRegex(pattern string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("%w: empty pattern", ErrSubject)
	}
	if strings.ContainsAny(pattern, " \t\r\n/") {
		return "", fmt.Errorf("%w: pattern %q is not a subject pattern", ErrSubject, pattern)
	}
	segments := strings.Split(pattern, ".")
	out := make([]string, 0, len(segments))
	for i, part := range segments {
		switch part {
		case ">":
			if i != len(segments)-1 {
				return "", fmt.Errorf("%w: `>` must be the last segment of %q", ErrSubject, pattern)
			}
			out = append(out, ".+")
		case "*":
			out = append(out, "[^.]+")
		case "":
			return "", fmt.Errorf("%w: pattern %q has an empty segment", ErrSubject, pattern)
		default:
			out = append(out, regexp.QuoteMeta(part))
		}
	}
	return strings.Join(out, `\.`) + "$", nil
}
