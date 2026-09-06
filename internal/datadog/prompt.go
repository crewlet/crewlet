package datadog

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a firing monitor asks of the seat it reached.
//
// # It tailors to the ROUTING REASON, not to the transition
//
// A seat named by a monitor's own tag owns that monitor: it is being told
// about its own service. A seat reached through the company's fallback is
// there only because nothing named anybody, so the first thing it has to
// establish is whether this is even its problem. Asking both of them to
// "investigate and mitigate" tells the second one to start work on something
// it may have no context for at all.
//
// NOTHING HERE NAMES A TOOL. The prompt describes the capability, and the
// model finds the matching tool in its own catalogue, because the deployed
// MCP server's tool names are not knowable by the engine. See
// docs/concepts/tool-capabilities.md.
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

// ConversationKey implements [notify.Prompt]: the MONITOR is the conversation.
//
// Keyed on the monitor's title rather than on the alert id, and the two are
// opposite choices. The alert id is unique per notification, so keying on it
// would make every firing its own conversation and a seat would never see
// that this is the fourth time tonight. The monitor is the thing that keeps
// firing, so a trigger, its recovery and its re-trigger coalesce into one
// thread, which is what a person paging through an incident actually reads.
//
// The TITLE rather than a monitor id because the payload template carries no
// id: Datadog's `$ID` is the notification's, and there is no `$MONITOR_ID`
// variable. A retitled monitor therefore starts a new conversation, which is
// the correct behaviour for the one case it costs anything: renaming a
// monitor is usually redefining what it watches.
func (Prompt) ConversationKey(metadata map[string]string, subject string) string {
	if monitor := metadata[MonitorField]; monitor != "" {
		return monitor
	}
	return subject
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
func (Prompt) WakesActor(string) bool { return false }

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
			"belongs to you before you work it: if it belongs to a colleague, " +
			"hand it to them and say why.\n")
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
func writeAlertAsk(b *strings.Builder) {
	b.WriteString("## What to do\n\n")
	b.WriteString("1. Look at the alert before concluding anything. Read the " +
		"monitor's query and its recent history, and check whether anything " +
		"else is firing at the same time.\n")
	b.WriteString("2. Decide whether this is real. A monitor that fires on a " +
		"threshold nobody has revisited is a monitor to fix, not an incident " +
		"to work, and saying so is a complete answer.\n")
	b.WriteString("3. If it is real, say what is affected and what you are doing " +
		"about it, in the channel your team reads. Do not sit on it while you " +
		"investigate.\n")
	b.WriteString("4. Escalate to your manager if it is beyond what you can " +
		"safely act on, and say what you have already ruled out.\n")
}

// writeRecoveryAsk is the ask for a monitor that has cleared.
//
// A recovery is not a no-op and it is not an incident either. What it needs
// is a decision about whether the thing that broke is actually fixed, and a
// closing note to whoever was told it was broken.
func writeRecoveryAsk(b *strings.Builder) {
	b.WriteString("## What to do\n\n")
	b.WriteString("1. Confirm this is a real recovery rather than the signal " +
		"going away. A monitor with no data recovers exactly like one whose " +
		"problem was fixed.\n")
	b.WriteString("2. If you or a colleague reported this incident, close it out " +
		"where you reported it, and say what the cause turned out to be if you " +
		"know it.\n")
	b.WriteString("3. If it recovered on its own and you do not know why, say " +
		"that plainly rather than closing it silently. A monitor that " +
		"self-resolves is usually about to fire again.\n")
}
