package datadog

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Prompt is what a firing monitor asks of the seat it reached.
//
// # It tailors to the ROUTING REASON, not to the transition
//
// A seat named by a monitor's own tag owns that monitor. A seat reached through
// the company's fallback is there only because nothing named anybody, so it
// must first establish whether this is its problem; "investigate and mitigate"
// would start work on something it may have no context for.
//
// NOTHING HERE NAMES A TOOL: the prompt describes the capability and the model
// finds the tool in its own catalogue, as the deployed MCP server's tool names
// are not knowable by the engine. See docs/concepts/tool-capabilities.md.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Backend }

// RequiresRecon implements [notify.Prompt]: always.
//
// A monitor notification is a POINTER. It says a threshold was crossed and
// carries a rendered title; it does not carry the query, the graph, the
// recent deploys, or the other monitors that fired at the same moment, which
// are what make the alert mean anything. The seat has to look before it can
// act, so the turn-start relevance filters skip their auxiliary model call
// rather than filter against a bare pointer.
func (Prompt) RequiresRecon(notify.Inbound) bool { return true }

// SubjectIsLabel implements [notify.Prompt]: never — the subject is the
// monitor's rendered alert title, which names what crossed its threshold.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: a monitor tagged as this seat's.
//
// The SAME split the prompt frames as "your service and your call" against
// "establish whether this is even yours". A tag naming the seat is the
// company saying who owns the monitor, and an owner's turn may not end in
// silence: the ask below is to say what is affected, or that the threshold
// is the thing to fix, and either is an answer somebody reads. The fallback
// is the alert landing somewhere rather than on somebody, and a seat obliged
// to answer every untagged monitor in the company would post on each one
// whether or not it had anything to say. False is the conservative half, see
// [notify.Prompt].
func (Prompt) Addressed(n notify.Inbound) bool {
	return n.Metadata[RoutedViaField] == RoutedViaTag
}

// PartitionKey implements [notify.Prompt]: the MONITOR is the conversation.
//
// Keyed on the monitor, not the alert: the alert id is unique per
// NOTIFICATION, so every firing would be its own conversation. A trigger, its
// recovery and its re-trigger coalesce into one thread, and the digest
// coalescer partitions on this key too. THE ID, NOT THE TITLE: a title carries
// the STATE ("[Triggered] ..." then "[Recovered] ..."), splitting them.
//
// The title remains the fallback for a webhook definition lacking the id
// (hand-made, or this engine's own after an edit, until the next pass rewrites
// it). A retitled monitor then starts a new conversation, right for the one
// case it costs anything: a rename usually redefines what it watches.
func (Prompt) PartitionKey(metadata map[string]string, subject string) string {
	if id := metadata[MonitorIDField]; id != "" {
		return "monitor:" + id
	}
	if monitor := metadata[MonitorField]; monitor != "" {
		return monitor
	}
	return subject
}

// ConversationIdentity implements [notify.Prompt]: the same monitor.
//
// The two coincide because a monitor is one object that is both the merge
// unit and the durable thread — it has no sub-thread grain they could diverge
// on, and an alert is addressed to nobody, so there is no reply target a
// finer partition could steer.
func (p Prompt) ConversationIdentity(metadata map[string]string, subject string) string {
	return p.PartitionKey(metadata, subject)
}

// DigestBody implements [notify.Prompt]: every alert keeps its own message.
//
// The opposite answer to a tracker's, and for the opposite reason. A tracker
// re-sends the whole issue description on every field change, so five of them
// in one digest is a paragraph five times over. A monitor's message is not a
// snapshot of anything: it is what the operator wrote for THAT threshold, and
// a monitor that triggers, recovers and re-triggers within one digest window
// is telling a story whose middle is the part that matters. Collapsing to the
// latest would render "it recovered" and drop the fact that it had already
// gone off twice.
func (Prompt) DigestBody(_, body string) string { return body }

// WakesActor implements [notify.Prompt]: never, and the question does not
// arise.
//
// A monitor has no author. The parser stamps no actor for that reason, so the
// spine's self-action rule never has anything to suppress and this answer is
// about a case that cannot occur rather than a policy.
func (Prompt) WakesActor(notify.Inbound) bool { return false }

// Provenance implements [notify.Prompt]: an alert, by nobody the engine can
// name. A monitor's message is whoever configured it plus whatever the
// monitored system filled in when it fired — a host name, an error line — and
// nothing in a payload vouches for either.
func (Prompt) Provenance(notify.Inbound, notify.Parties) notify.Provenance {
	return notify.Provenance{Source: provenance.SourceAlert}
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, _ notify.Parties) string {
	var b strings.Builder

	transition := n.Metadata[TransitionField]
	recovered := isRecovery(transition)

	b.WriteString("A Datadog monitor ")
	switch {
	case recovered:
		b.WriteString("has recovered")
	case transition != "":
		b.WriteString("changed state (")
		b.WriteString(transition)
		b.WriteString(")")
	default:
		b.WriteString("fired")
	}
	if priority := n.Metadata[PriorityField]; priority != "" {
		b.WriteString(" at priority P")
		b.WriteString(priority)
	}
	b.WriteString(".\n\n")

	b.WriteString("Monitor: ")
	b.WriteString(n.Subject)
	b.WriteString("\n")
	if scope := n.Metadata[ScopeField]; scope != "" {
		// The scope is what separates "the API is down" from "the API is
		// down on one host", and it is the first thing that narrows where
		// to look.
		b.WriteString("Scope: ")
		b.WriteString(scope)
		b.WriteString("\n")
	}
	if link := n.Metadata[LinkField]; link != "" {
		b.WriteString("Alert: ")
		b.WriteString(link)
		b.WriteString("\n")
	}
	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString("\nThe monitor's own message:\n")
		b.WriteString(body)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	writeWhyYou(&b, n.Metadata[RoutedViaField], n.Metadata[TagsField])

	b.WriteString("\n")
	if recovered {
		writeRecoveryAsk(&b)
	} else {
		writeAlertAsk(&b)
	}
	return b.String()
}

// isRecovery reads a transition the same way [Alert.Triggered] does.
//
// The prompt cannot call that method: it is handed a [notify.Inbound] whose
// metadata the parser wrote, not the Alert, because a notification is
// re-rendered from the event store long after the delivery is gone. So the
// rule is stated once here and once there, and the two are held equal by a
// test rather than by a reader's memory.
func isRecovery(transition string) bool {
	switch strings.ToLower(strings.TrimSpace(transition)) {
	case "recovered", "ok":
		return true
	default:
		return false
	}
}

// writeWhyYou says why this seat is reading this, which is the difference
// between the two prompts this package can produce.
func writeWhyYou(b *strings.Builder, via, tags string) {
	switch via {
	case RoutedViaFallback:
		b.WriteString("## Why you\n\n")
		b.WriteString("No monitor tag named an owner for this alert, so it came to " +
			"you as this company's fallback for Datadog. Establish whether it " +
			"belongs to you before you work it, and hand it to the colleague it " +
			"belongs to if it does not.\n")
	default:
		b.WriteString("## Why you\n\n")
		b.WriteString("This monitor is tagged as yours")
		if tags != "" {
			b.WriteString(" (")
			b.WriteString(tags)
			b.WriteString(")")
		}
		b.WriteString(". It is your service and your call.\n")
	}
}

// writeAlertAsk is the ask for a monitor that is firing.
//
// IT NAMES THE ANSWERS, not the investigation. How to look at an alert is the
// model's own; what the company counts as answering one is not — the impact
// said where the team will see it, or the threshold named as the thing to fix,
// either of which ends an owner's turn ([Prompt.Addressed]).
func writeAlertAsk(b *strings.Builder) {
	b.WriteString("## What to do\n\n")
	b.WriteString("Decide whether this is real. If it is, say what is affected " +
		"and what you are doing about it in the channel your team reads, and " +
		"escalate to your manager if it is beyond what you can safely act on. " +
		"A monitor firing on a threshold nobody has revisited is a monitor to " +
		"fix, not an incident, and saying so is a complete answer.\n")
}

// writeRecoveryAsk is the ask for a monitor that has cleared.
//
// A recovery is not a no-op and it is not an incident either. What it needs
// is a decision about whether the thing that broke is actually fixed, and a
// closing note to whoever was told it was broken. The one Datadog fact a
// model would not assume is said: a monitor that stops receiving data
// recovers exactly like one whose problem was fixed.
func writeRecoveryAsk(b *strings.Builder) {
	b.WriteString("## What to do\n\n")
	b.WriteString("Confirm the recovery is real — a monitor with no data " +
		"recovers exactly like one whose problem was fixed — then close out " +
		"anything you or a colleague reported about it, where it was reported, " +
		"with the cause if you know it.\n")
}
