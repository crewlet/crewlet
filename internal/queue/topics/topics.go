// Package topics is the one place a Crewlet subject string is built.
//
// An agent's inbox subject (crewlet.agent.{handle}.inbox) is the engine's
// routing primitive: the notification service publishes to it, the scheduler
// fires into it, an A2A ask wakes a colleague through it, the sandbox
// coordinator resumes a suspended turn on it, and the engine subscribes one
// consumer per seat to it. Formatted by hand, that string has nine call sites
// — nine chances for a producer and a consumer to disagree about a name that
// has to match exactly. A mismatch is not an
// error anywhere: it is a message published to a topic nobody reads.
//
// The grammar lives here so there is exactly one definition, and here rather
// than beside the parties because it is a QUEUE fact — the handle is the
// routing key, not the identity. This package imports nothing else from the
// engine so any layer may use it. A test greps the tree and fails the build
// on a hand-built subject outside this package.
package topics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// AgentInboxPrefix prefixes every per-agent subject.
	AgentInboxPrefix = "crewlet.agent."
	// AgentInboxSuffix suffixes a seat's inbox subject.
	AgentInboxSuffix = ".inbox"
	// AgentControlSuffix suffixes a seat's sandbox-control subject.
	AgentControlSuffix = ".control"

	// AgentGroupPrefix prefixes every per-seat durable consumer group.
	//
	// Named rather than written inline because the group grammar is a
	// wire name like any other: the two functions below were the only
	// place in this package that built part of a name from a bare
	// literal, which is precisely the duplication the package exists to
	// remove.
	AgentGroupPrefix = "agent-"
	// AgentControlGroupSuffix distinguishes a seat's sandbox-control
	// consumer group from the inbox group of that same seat.
	//
	// It and AgentGroupPrefix do NOT make a group name unique on their
	// own — a seat handled `a-control` mints the same group as seat `a`'s
	// control group, because a hyphen is legal inside a handle. That is
	// safe only because a subscription is keyed on the (topic, group)
	// PAIR by every backend, and the two pairs differ in their topic. See
	// TestGroupNamesAreNotUniqueOnTheirOwn.
	AgentControlGroupSuffix = "-control"

	// EventsPrefix prefixes the engine's fleet-wide routing subjects.
	EventsPrefix = "crewlet.events."
	// EventsWildcard matches every engine event, for broadcast streams.
	EventsWildcard = EventsPrefix + ">"

	// NotificationsPrefix prefixes the notification work queues. A stream
	// topology needs the DOMAIN, not the leaves — without a name for it a
	// backend hand-writes "crewlet.notifications.>" and the grammar has
	// two definitions again.
	NotificationsPrefix = "crewlet.notifications."
	// NotificationsInbound carries raw inbound webhook envelopes.
	NotificationsInbound = NotificationsPrefix + "inbound"
	// NotificationsOutbound carries outbound notification requests.
	NotificationsOutbound = NotificationsPrefix + "outbound"

	// ConfigPrefix prefixes the control-plane subjects, for the same
	// reason NotificationsPrefix exists.
	ConfigPrefix = "crewlet.config."
	// ConfigRevisionActivated nudges nodes that a new config epoch exists.
	// Best-effort by design: losing it costs one poll interval, never a
	// revision, because the authoritative path polls the epoch pointer.
	ConfigRevisionActivated = ConfigPrefix + "revision_activated"
	// ConfigRevisionApplied reports a node's apply outcome.
	ConfigRevisionApplied = ConfigPrefix + "revision_applied"
)

// AgentInbox returns the inbox subject for the seat with this handle.
//
// The handle is the SEAT's handle, which every process derives from the org
// — never a process-local instance id. An empty handle returns an empty
// subject rather than a topic named after nothing: callers must treat that
// as "not routable" instead of publishing to crewlet.agent..inbox, a real
// topic that no consumer subscribes to and that would swallow the event.
func AgentInbox(handle string) string {
	if handle == "" {
		return ""
	}
	return AgentInboxPrefix + handle + AgentInboxSuffix
}

// AgentInboxGroup returns the durable consumer group for a seat's inbox.
//
// One group per seat, so membership IS ownership: the node that attaches a
// consumer to agent-{handle} is the node that receives that seat's work.
// Nothing computes "which node" — routing falls out of who subscribed.
func AgentInboxGroup(handle string) string {
	if handle == "" {
		return ""
	}
	return AgentGroupPrefix + handle
}

// AgentControl returns the seat's sandbox-control subject.
//
// Separate from the inbox because a detached sandbox run PAUSES the inbox: a
// completion riding the inbox would queue behind the very pause it exists to
// lift. Attached and detached alongside the inbox, so a completion reaches
// the seat's owner and only its owner.
func AgentControl(handle string) string {
	if handle == "" {
		return ""
	}
	return AgentInboxPrefix + handle + AgentControlSuffix
}

// AgentControlGroup returns the durable consumer group for a seat's control
// subject.
func AgentControlGroup(handle string) string {
	if handle == "" {
		return ""
	}
	return AgentGroupPrefix + handle + AgentControlGroupSuffix
}

// HandleFromInbox recovers the seat handle from an inbox subject, reporting
// whether the subject was one. Used by diagnostics and by backends that log
// per-seat activity without threading the handle through.
//
// It is the exact inverse of [AgentInbox]: it reports true only for a
// subject AgentInbox could have produced. The suffix is matched against what
// is LEFT after the prefix, not against the whole subject, because the two
// overlap on one dot — "crewlet.agent.inbox" carries both, and matching them
// independently recovered a handle of "inbox" from a subject that is nobody's
// inbox. A false identification is worse than none here: the caller is a log
// line or a diagnostic that would then name a seat which is not involved.
func HandleFromInbox(subject string) (string, bool) {
	rest, ok := strings.CutPrefix(subject, AgentInboxPrefix)
	if !ok {
		return "", false
	}
	h, ok := strings.CutSuffix(rest, AgentInboxSuffix)
	if !ok || h == "" || strings.Contains(h, ".") {
		return "", false
	}
	return h, true
}

// Event returns the fleet-wide routing subject for an event type. Each of
// these has ONE fleet-wide consumer group, so the node that wins a delivery
// is rarely the node running the recipient — which is why recipients are
// always resolved from the org, never from a local agent pool.
func Event(eventType string) string {
	if eventType == "" {
		return ""
	}
	return EventsPrefix + eventType
}

// DeadLetterPrefix is the namespace dead letters live in.
const DeadLetterPrefix = "dlq."

// deadLetterDigestBytes is how much of the pair's digest ends the subject.
// Forty-eight bits leaves a collision probability below one in ten million
// for a company with ten thousand subscriptions, against a real ceiling in
// the hundreds — the same sizing, for the same reason, as the durable
// consumer name in internal/queue/jetstream.
const deadLetterDigestBytes = 6

// DeadLetter returns the dead-letter subject for a (topic, group) pair.
//
// Deliberately OUTSIDE the crewlet.* space: the dashboard streams
// crewlet.events.> and a dead-lettered subject inside it would resurface
// poison as live traffic.
//
// Dot-segmented rather than hyphenated so it is an ordinary subject in the
// same grammar as everything else — a backend that groups subjects by their
// leading segment can then carry dead letters without a special case, and a
// special case in the poison path is exactly the code nobody exercises.
//
// It must be INJECTIVE, and that is why it is not a string join. A join on a
// dot aliases whenever either half can contain one: DeadLetter("a.b", "c")
// and DeadLetter("a", "b.c") were the same subject, so two subscriptions'
// poison landed in one place with nothing to tell it apart — the dead letter
// carries the original event, not the subscription that failed it. Reachable
// rather than theoretical: the conformance suite subscribes with the dotted
// group "h.i" precisely because a backend was aliasing distinct pairs there.
//
// So the readable head is for OPERATORS — it keeps a DLQ greppable by the
// topic that produced it — and the DIGEST is the identity. The digest is
// taken over the raw pair joined by a NUL, which cannot occur in either half,
// so no two distinct pairs share one. Sanitising could not carry the identity
// for the same reason the join could not: it is lossy, and lossy is exactly
// what aliases.
//
// The head introduces no new length bound — the topic already had to be a
// publishable subject and the group a legal subscription name — and the
// digest is appended LAST, so a backend that ever has to truncate cannot
// reintroduce an alias by doing so.
func DeadLetter(topic, group string) string {
	sum := sha256.Sum256([]byte(group + "\x00" + topic))
	id := hex.EncodeToString(sum[:deadLetterDigestBytes])
	return DeadLetterPrefix + subjectSafe(topic) + "." + subjectSafe(group) + "." + id
}

// subjectSafe rewrites the parts of a name that would stop the result from
// being a publishable subject.
//
// The set is exactly what makes a subject unpublishable: whitespace, the two
// wildcards (which would make the result a PATTERN rather than a subject),
// and an empty segment. That is nats-server's own rule — see isValidSubject,
// which rejects an empty token and a subject that is anything but a literal
// past a '>'. A dead-letter subject tripping any of those is a poison message
// that cannot be published at all — and that is the one message where losing
// it costs the only copy of the evidence.
//
// Nothing else is rewritten, deliberately. A character that the broker
// accepts is a character this must not mangle: the head is here to be read by
// a person grepping a DLQ, and every substitution makes two different pairs
// look more alike.
//
// Dots are left alone: they are legal, and keeping them keeps a DLQ greppable
// by topic. Being lossy is safe here only because the digest beside it, not
// this, is what identifies the pair.
func subjectSafe(s string) string {
	segments := strings.Split(s, ".")
	for i, segment := range segments {
		segment = strings.Map(func(r rune) rune {
			switch r {
			case ' ', '\t', '\r', '\n', '*', '>':
				return '_'
			}
			return r
		}, segment)
		if segment == "" {
			segment = "_"
		}
		segments[i] = segment
	}
	return strings.Join(segments, ".")
}
