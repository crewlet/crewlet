// Package types is the engine's event catalogue: one struct per event Crewlet
// publishes, each registered with the events package under its wire type
// string.
//
// Registration happens in this package's init functions, so importing it is
// what teaches a build to decode typed events. A binary that never imports it
// still carries every event intact — the envelope preserves an unknown type's
// fields — it just sees no typed fields, which is exactly the position an older
// node is in mid-upgrade.
//
// The catalogue is grouped by domain across several files. THE JSON TAGS ARE
// THE WIRE FORMAT, and a rolling upgrade puts two builds on one stream: a
// renamed tag is a dropped field on whichever half has not been upgraded yet.
// Rename nothing here; add instead.
//
// Payloads never resolve who the actor was. Each states the one fact it knows
// — its role through Role, its agent id through AgentID, an outright override
// through Actor — and the envelope owns the order those resolve in. A summary
// that leads with the actor implements SummaryFor and is handed the resolved
// value. Handing every payload the whole event was the alternative, and it
// would let sixty of them re-derive the chain: the moment two disagree, one
// turn reads differently on two surfaces.
//
// Every payload in the catalogue implements the same small method set, so the
// comment on each one carries only what is specific to that event:
//
//   - EventType is the wire type string, and the registry key. It is the one
//     place that literal appears, which is why each method's comment quotes it
//     — godoc shows the comment, never the one-line body.
//   - Role and AgentID contribute the seat and the instance to the envelope's
//     actor chain (events.Roler, events.AgentIdentified). What they MEAN is per
//     event: the seat that spent the tokens, the seat whose budget ran out, the
//     seat being torn down.
//   - Summary or SummaryFor renders the "who did what" line every trace view
//     shows. SummaryFor is handed the already-resolved actor; Summary is for
//     the events that name their own subject and never lead with an actor.
//
// Four conventions the whole catalogue follows:
//
//   - Slices and maps carry omitempty, scalars do not. A scalar's zero value
//     IS its wire default, so writing it out always is what keeps the two
//     agreeing. A nil slice or map would marshal as null, which is a
//     different value from an absent key and from an empty list — omitting
//     the key lets the reader's own default answer.
//   - A closed set of string values is a named string type with constants, not
//     a bare string. The wire form is unchanged — these stay strings so an
//     unrecognised value from a newer node round-trips rather than failing.
//   - The fields carrying the role and agent_id wire values are named RoleName
//     and Agent. Go forbids a field and a method sharing a name, and the actor
//     chain's interfaces claim Role() and AgentID(); the tags are unchanged, and
//     those methods are how anything reads the values.
//   - No payload field may be named like an envelope field (id, type,
//     timestamp, source, payload, trace_id, span_id, parent_span_id,
//     delegation_depth, parent_turn_id, delegation_chain). The envelope owns
//     those keys and silently drops a payload field that collides — see
//     ConfigRevisionActivated, the one event that would redeclare one.
package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// FailureEventTypes are the events that ARE a failure by their very type,
// independent of any payload flag.
//
// Named here, beside the events themselves, because three layers need the same
// answer — the live projection stamping its feed, the event store reading a row
// back out of history, and the API serializing a push — and a second copy is
// how the same turn ends up red on one surface and not on another.
//
// Read-only. Ask through Failed rather than reaching into the map.
var FailureEventTypes = map[string]struct{}{
	"task_failed":       {},
	"llm_unavailable":   {},
	"budget_exhausted":  {},
	"turn.guard_breach": {},
}

// Failed reports whether the work an event describes failed.
//
// payloadFailed is the event's own failed field, available while the event is
// live; tagFailed is the failed tag the event-store writer stamps, which is all
// that survives into history (a history listing never selects the payload
// column). Either one, or a type that is itself a failure, means failed.
func Failed(eventType string, payloadFailed, tagFailed bool) bool {
	if payloadFailed || tagFailed {
		return true
	}
	_, ok := FailureEventTypes[eventType]
	return ok
}

// IntegrationTrigger is implemented by payloads that came in from an external
// integration. DescribeTrigger reads it so a turn's source can be labelled with
// the integration that caused it — a branded Slack/Jira badge naming the human
// — rather than a generic "external notification".
//
// Three separate answers, each "" when absent, because they are three separate
// facts: an event may name its integration and carry neither a sender nor a
// source event type.
type IntegrationTrigger interface {
	// Integration is the originating system: slack / jira / github / …
	Integration() string
	// IntegrationSender is the human who sent it, when one was identified.
	IntegrationSender() string
	// IntegrationEventType is the integration's own name for what happened.
	IntegrationEventType() string
}

// Trigger describes the event that woke an agent.
//
// It rides on the per-phase telemetry events (AgentPhaseStarted,
// AgentPhaseCompleted, AgentTurnProgress, AgentTurnCompleted) so a dashboard can
// show what caused each LLM invocation — the task assignment, notification, A2A
// request or schedule tick — as the source of the turn. ID is what lets it link
// through to the full event when the trigger was persisted.
//
// The zero Trigger means there was no trigger at all (an engine-internal turn).
// It marshals to {}, which the dashboard renders as "no source"; the
// empty-object form is the wire contract, not null.
type Trigger struct {
	ID        string
	Type      string
	Summary   string
	Actor     string
	Timestamp time.Time

	// Integration, Sender and SourceEventType are set ONLY when the trigger
	// arrived from an external integration, and are ABSENT from the wire form
	// when empty rather than present-and-blank: their absence is what selects
	// the dashboard's plain type label over a branded badge.
	Integration     string
	Sender          string
	SourceEventType string
}

// DescribeTrigger builds the descriptor for the event that woke an agent.
// A nil event yields the zero Trigger.
func DescribeTrigger(e *events.Event) Trigger {
	if e == nil {
		return Trigger{}
	}
	t := Trigger{
		ID:        e.ID.String(),
		Type:      e.Type,
		Summary:   e.Summary(),
		Actor:     e.Actor(),
		Timestamp: e.Timestamp,
	}
	integrationTrigger, ok := e.Data.(IntegrationTrigger)
	if !ok {
		return t
	}
	// Sender and source event type are read only inside the integration
	// branch: without an integration to brand them with, they name nothing a
	// consumer could act on.
	if t.Integration = integrationTrigger.Integration(); t.Integration != "" {
		t.Sender = integrationTrigger.IntegrationSender()
		t.SourceEventType = integrationTrigger.IntegrationEventType()
	}
	return t
}

// IsZero reports whether this descriptor names no trigger at all.
func (t Trigger) IsZero() bool { return t == Trigger{} }

// Map renders the descriptor in the loose form the wire and the dashboard use.
//
// One definition of the wire shape, which MarshalJSON also goes through, so the
// key set cannot drift between the JSON a Go node publishes and the map an API
// handler assembles by hand.
func (t Trigger) Map() map[string]any {
	if t.IsZero() {
		return map[string]any{}
	}
	timestamp := ""
	if !t.Timestamp.IsZero() {
		// RFC3339Nano, which is what the envelope's own timestamp
		// serializes to — so a reader that parses one parses the other.
		timestamp = t.Timestamp.Format(time.RFC3339Nano)
	}
	descriptor := map[string]any{
		"id":        t.ID,
		"type":      t.Type,
		"summary":   t.Summary,
		"actor":     t.Actor,
		"timestamp": timestamp,
	}
	if t.Integration != "" {
		descriptor["integration"] = t.Integration
		if t.Sender != "" {
			descriptor["sender"] = t.Sender
		}
		if t.SourceEventType != "" {
			descriptor["source_event_type"] = t.SourceEventType
		}
	}
	return descriptor
}

// MarshalJSON writes the descriptor through Map, so the JSON a node publishes
// and the map an API handler hands the dashboard cannot drift apart.
func (t Trigger) MarshalJSON() ([]byte, error) { return json.Marshal(t.Map()) }

// triggerWire mirrors the map form for decoding. Every key is optional: a
// descriptor written by a node that knew fewer of them is still a descriptor.
type triggerWire struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Summary         string `json:"summary"`
	Actor           string `json:"actor"`
	Timestamp       string `json:"timestamp"`
	Integration     string `json:"integration"`
	Sender          string `json:"sender"`
	SourceEventType string `json:"source_event_type"`
}

// UnmarshalJSON reads a descriptor back, tolerating a timestamp it cannot
// parse.
//
// Tolerating it is the point: the envelope drops the WHOLE typed body when a
// payload fails to decode, so an unreadable timestamp on a descriptor would
// cost a dashboard every other field of the event carrying it.
func (t *Trigger) UnmarshalJSON(data []byte) error {
	var wire triggerWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("unmarshal trigger: %w", err)
	}
	*t = Trigger{
		ID: wire.ID, Type: wire.Type, Summary: wire.Summary, Actor: wire.Actor,
		Integration: wire.Integration, Sender: wire.Sender,
		SourceEventType: wire.SourceEventType,
	}
	if wire.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339, wire.Timestamp); err == nil {
			t.Timestamp = parsed
		}
	}
	return nil
}

// lead renders an actor-led summary line: "Engineer completed task T-42".
//
// Its first argument is normally the RESOLVED actor an ActorSummarizer is
// handed, so the payload never has to know how the chain reached it. A few
// lines lead with a party the payload names itself instead — an A2A channel's
// requester, a message's own sender — and those pass that party.
//
// An empty actor opens the sentence with its verb rather than with a blank.
// The envelope always resolves one (worst case, the engine itself), so that
// branch is for a direct caller and for the payload-named parties, which can
// genuinely be absent.
//
// The phrases are ASCII by construction, so this needs none of the locale
// machinery a general title-caser would carry.
func lead(actor, phrase string) string {
	if actor == "" {
		return upperFirst(phrase)
	}
	return actor + " " + phrase
}

// subject renders the "who, doing what" that the turn-engine summaries open
// with: the actor and the phase it is in, either half of which may be missing.
// Joined here rather than interpolated at each call site, because an absent
// half is exactly what produces a line opening on a blank.
func subject(actor string, phase Phase) string {
	return strings.TrimSpace(actor + " " + string(phase))
}

func upperFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// a2aTag renders the [A2A:channel] marker a turn summary carries when the turn
// served an agent-to-agent ask. An empty context is not an A2A turn at all,
// which is why the whole marker disappears rather than rendering blank.
func a2aTag(context map[string]any) string {
	if len(context) == 0 {
		return ""
	}
	if channel, _ := context["channel_id"].(string); channel != "" {
		return " [A2A:" + channel + "]"
	}
	return " [A2A]"
}
