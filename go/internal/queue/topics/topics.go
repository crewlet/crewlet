// Package topics is the one place a Crewlet subject string is built.
//
// An agent's inbox subject (crewlet.agent.{handle}.inbox) is the engine's
// routing primitive: the notification service publishes to it, the scheduler
// fires into it, an A2A ask wakes a colleague through it, the sandbox
// coordinator resumes a suspended turn on it, and the engine subscribes one
// consumer per seat to it. In the Python engine nine call sites formatted
// that string by hand — nine chances for a producer and a consumer to
// disagree about a name that has to match exactly. A mismatch is not an
// error anywhere: it is a message published to a topic nobody reads.
//
// The grammar lives here so there is exactly one definition, and here rather
// than beside the parties because it is a QUEUE fact — the handle is the
// routing key, not the identity. This package imports nothing else from the
// engine so any layer may use it. A test greps the tree and fails the build
// on a hand-built subject outside this package.
package topics

import "strings"

const (
	// AgentInboxPrefix prefixes every per-agent subject.
	AgentInboxPrefix = "crewlet.agent."
	// AgentInboxSuffix suffixes a seat's inbox subject.
	AgentInboxSuffix = ".inbox"
	// AgentControlSuffix suffixes a seat's sandbox-control subject.
	AgentControlSuffix = ".control"

	// EventsPrefix prefixes the engine's fleet-wide routing subjects.
	EventsPrefix = "crewlet.events."
	// EventsWildcard matches every engine event, for broadcast streams.
	EventsWildcard = "crewlet.events.>"

	// NotificationsInbound carries raw inbound webhook envelopes.
	NotificationsInbound = "crewlet.notifications.inbound"
	// NotificationsOutbound carries outbound notification requests.
	NotificationsOutbound = "crewlet.notifications.outbound"

	// ConfigRevisionActivated nudges nodes that a new config epoch exists.
	// Best-effort by design: losing it costs one poll interval, never a
	// revision, because the authoritative path polls the epoch pointer.
	ConfigRevisionActivated = "crewlet.config.revision_activated"
	// ConfigRevisionApplied reports a node's apply outcome.
	ConfigRevisionApplied = "crewlet.config.revision_applied"
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
	return "agent-" + handle
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
	return "agent-" + handle + "-control"
}

// HandleFromInbox recovers the seat handle from an inbox subject, reporting
// whether the subject was one. Used by diagnostics and by backends that log
// per-seat activity without threading the handle through.
func HandleFromInbox(subject string) (string, bool) {
	if !strings.HasPrefix(subject, AgentInboxPrefix) || !strings.HasSuffix(subject, AgentInboxSuffix) {
		return "", false
	}
	h := strings.TrimSuffix(strings.TrimPrefix(subject, AgentInboxPrefix), AgentInboxSuffix)
	if h == "" || strings.Contains(h, ".") {
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
func DeadLetter(topic, group string) string {
	return DeadLetterPrefix + topic + "." + group
}
