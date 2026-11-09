package alarmwake

import (
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fleetHealthTool is the tool the prompt sends a seat to: the health tools'
// read of every node's firing set, named by its authority row as the tool is.
const fleetHealthTool = string(authz.ActionFleetHealth)

// firingArg is the argument the prompt has the seat read fleet_health with:
// only the nodes with an alarm firing, which is the question a wake asks and
// an answer that stays small however many nodes the fleet has. A string here
// rather than an import, since internal/agent/builtin sits above this package;
// its TestAnAlarmWakeAsksFleetHealthOnlyForWhatIsFiring holds the prompt and
// the tool's schema to one spelling.
const firingArg = "firing"

// Prompt is what an alarm transition asks of a seat whose role carries the
// health grant.
//
// # It asks the seat to look, never to act on the delivery
//
// A transition is one change on one node at one instant. What is firing now
// is every node's own heartbeat's to say, which `fleet_health` reads, so the
// prompt sends the seat there first: the alarm may have cleared since, and a
// clear its node did not live to publish is visible only there. What the
// alarm MEANS and what an operator does about it are the alarm table's own
// words (statelog.Describe), so the seat reads the remedy the published
// reference prints rather than one this package wrote again.
//
// # Nobody is waiting on an answer
//
// The grant asks to be told, not to be asked: a seat may well look and find
// nothing it should do, and a turn obliged to post would post on every flap.
// So no copy is addressed, and the turn may end having only looked.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: always. A transition is a pointer
// at the fleet's current state, which the seat has to read before the
// trigger means anything.
func (Prompt) RequiresRecon(notify.Inbound) bool { return true }

// SubjectIsLabel implements [notify.Prompt]: never — the subject names the
// alarm, the node and what it did, which is the content.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: never. See the type's doc.
func (Prompt) Addressed(notify.Inbound) bool { return false }

// PartitionKey implements [notify.Prompt]: ONE ALARM is the conversation, on
// every node and every log or entry it stands on, so the copies of it the inbox holds
// pending together merge into one turn: the copies one condition raises across
// the fleet (every data node evaluates its own table, so a fleet-wide fact such
// as a stale backup fires on each of them within one alarm interval), and a
// fire beside its clear. The seat's one look at `fleet_health` answers all of
// them, and a turn per copy would spend a round per copy on the same read. Two
// alarms stay two things to judge. Empty, and so never merged, for a copy
// naming no alarm.
//
// THE KEY MERGES ONLY WHAT IS PENDING AT ONCE, and nothing upstream holds a
// transition back to make more of it pending: the table has no hysteresis and
// the outbox publishes every transition. At the inbox's default linger of zero
// the first copy starts a turn at once and the rest arrive during it, so one
// fleet-wide condition is two turns for each granted seat, and an alarm that
// flaps more slowly than a turn runs is a turn per transition
// (docs/concepts/event-system.md, "Alarm wakes").
func (Prompt) PartitionKey(metadata map[string]string, _ string) string {
	kind := metadata[AlarmField]
	if kind == "" {
		return ""
	}
	return "alarm:" + kind
}

// ConversationIdentity implements [notify.Prompt]: the same alarm, which is
// both the merge unit and the thread a seat follows over days — where it
// stands is in every copy's own lines.
func (p Prompt) ConversationIdentity(metadata map[string]string, subject string) string {
	return p.PartitionKey(metadata, subject)
}

// DigestBody implements [notify.Prompt]: every transition keeps its line. An
// alarm that fires, clears and fires again inside one digest window is telling
// a story whose middle is the part that matters.
func (Prompt) DigestBody(_, body string) string { return body }

// WakesActor implements [notify.Prompt]: never, and the question does not
// arise — an alarm has no author, so the parser names no actor.
func (Prompt) WakesActor(notify.Inbound) bool { return false }

// Provenance implements [notify.Prompt]: an alert, by nobody the engine can
// name, as a monitor's is. The alarm's words are the engine's, but the values
// it reports — a node id, a stream, an error a dependency returned — are what
// the measured system said, and nothing vouches for those.
func (Prompt) Provenance(notify.Inbound, notify.Parties) notify.Provenance {
	return notify.Provenance{Source: provenance.SourceAlert}
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, _ notify.Parties) string {
	meta := n.Metadata
	kind, node, domain := meta[AlarmField], meta[NodeField], meta[DomainField]
	change := statelog.TransitionChange(meta[ChangeField])

	var b strings.Builder
	b.WriteString("**Alarm:** `" + kind + "` on node `" + node + "`")
	if domain != "" {
		b.WriteString(", on the `" + domain + "` log")
	}
	if entry := meta[EntryField]; entry != "" {
		b.WriteString(", for the " + entryNoun(statelog.Kind(kind)) + " `" + entry + "`")
	}
	b.WriteString("\n**What happened:** ")
	at, since := meta[AtField], meta[SinceField]
	switch change {
	case statelog.TransitionFired:
		b.WriteString("it fired")
		if at != "" {
			b.WriteString(" at " + at)
		}
		b.WriteString(".")
	case statelog.TransitionCleared:
		b.WriteString("it cleared")
		if at != "" {
			b.WriteString(" at " + at)
		}
		if stood := stoodFor(since, at); stood != "" {
			b.WriteString(", having stood since " + since + " (" + stood + ")")
		}
		b.WriteString(".")
	default:
		b.WriteString("it changed (" + string(change) + ")")
		if at != "" {
			b.WriteString(" at " + at)
		}
		b.WriteString(".")
	}
	if n.Body != "" {
		b.WriteString("\n**Measured:** " + n.Body)
	}
	b.WriteString("\n")

	if meaning, remedy, ok := statelog.Describe(statelog.Kind(kind)); ok {
		b.WriteString("\n**What it means:** " + meaning)
		if change != statelog.TransitionCleared {
			b.WriteString("\n**What to do:** " + remedy)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nThis is one change on one node, not the fleet's current state. Read `" +
		fleetHealthTool + "` with `" + firingArg + ": true` before you act: it is every node's " +
		"own account of what is firing now, whatever the fleet's size, and another change may " +
		"already have followed this one. Your role carries the health grant, which is why you " +
		"were told; whether anything needs doing, and by whom, is yours to judge.\n")
	return b.String()
}

// entryNoun is what a per-entry kind's entry is called, the table's own split
// of [statelog.PerEntryKinds] into model entries and MCP servers.
func entryNoun(kind statelog.Kind) string {
	if slices.Contains(statelog.PerServerKinds(), kind) {
		return "MCP server"
	}
	return "model entry"
}

// stoodFor is how long an instance stood, from its two RFC 3339 instants, or
// "" where either is missing or unreadable.
func stoodFor(since, at string) string {
	s, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return ""
	}
	a, err := time.Parse(time.RFC3339, at)
	if err != nil || a.Before(s) {
		return ""
	}
	return a.Sub(s).Round(time.Second).String()
}
