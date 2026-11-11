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
//     delegation_depth, parent_turn_id, delegation_chain, node). The envelope
//     owns those keys and silently drops a payload field that collides — see
//     ConfigRevisionActivated, the one event that would redeclare one, and
//     BackupRequested, whose host is the envelope's `node` for that reason.
package types

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events"
)

// failureEventTypes are the events that ARE a failure by their very type,
// independent of any payload flag.
//
// Named here, beside the events themselves, because three layers need the same
// answer — the live projection stamping its feed, the event store reading a row
// back out of history, and the API serializing a push — and a second copy is
// how the same turn ends up red on one surface and not on another.
//
// UNEXPORTED, and that is the whole of the guarantee. It was exported under a
// doc line asking callers to treat it as read-only, which is the one thing a Go
// map cannot be: an exported map is writable by every importer that can name
// it, so `delete(types.FailureEventTypes, "budget_exhausted")` compiled and
// would have silently unfailed every turn refused at the budget gate, in every
// layer at once. A comment is not an access modifier. [Failed] answers the
// one-type question and [FailureEventNames] enumerates; between them nothing
// outside this package needs the map itself.
var failureEventTypes = map[string]struct{}{
	"llm_unavailable":   {},
	"budget_exhausted":  {},
	"turn.guard_breach": {},
	"executor_failed":   {},
}

// Failed reports whether the work an event describes failed.
//
// payloadFailed is the event's own failed field, available while the event is
// live; tagFailed is the failed tag the event-store writer stamps, which is all
// that survives into history (a history listing never selects the payload
// column). Either one, or a type that is itself a failure, means failed.
//
// THE ANSWER FOR ONE EVENT, which is what almost every caller has: a row, an
// envelope, a push. A caller that has to name the whole set — because it is
// composing a query rather than judging a value — takes [FailureEventNames].
func Failed(eventType string, payloadFailed, tagFailed bool) bool {
	if payloadFailed || tagFailed {
		return true
	}
	_, ok := failureEventTypes[eventType]
	return ok
}

// FailureEventNames is every type in the failure set, sorted, as a copy the
// caller owns.
//
// [Failed] cannot serve a caller that has to ENUMERATE the set rather than test
// one value against it — a SQL `IN (…)` clause needs the names and their bound
// arguments, and it needs them in the same order twice, because the predicate
// is spelled once and used for both a column and a filter. That is what sent
// internal/store's turn aggregate reaching into the map the doc asked it not
// to.
//
// SORTED, so the statement a caller builds is byte-stable: Go randomises map
// iteration, so ranging it produced a different SQL string on every process
// start — one that works, and one no two runs of a test could compare.
//
// A COPY, so a caller that sorts, appends to or trims what it is handed cannot
// reach the catalogue through the slice header.
func FailureEventNames() []string {
	return slices.Sorted(maps.Keys(failureEventTypes))
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

	// Origin is WHAT the waking event was about and why it reached this
	// seat: the chat message and the routing reason, the work item and the
	// tracker's reason, the schedule that fired. Zero, and absent from the
	// wire form, for a trigger whose event names none. For a COALESCED
	// trigger it is the primary one, the latest constituent's, as the
	// merged event's other flat fields are.
	Origin TriggerOrigin

	// Origins is every object a COALESCED trigger took in, oldest first,
	// the primary one included: one turn answered several messages, and
	// each of them started it. Nil for a trigger of one event, whose
	// Origin says it all.
	Origins []TriggerOrigin
}

// TriggerOrigin is what started a turn, as the waking event names it.
//
// THE OBJECT, NOT THE EVENT. A trigger's ID is the waking EVENT (a delivery,
// a fire), minted per recipient and per attempt, so nobody holding the chat
// message they wrote could find the turn it started by it. This names the
// thing a person holds: a message's id, a work item's key, a schedule's name.
type TriggerOrigin struct {
	// Kind is what ID names: [OriginChatMessage], [OriginWorkItem],
	// [OriginSchedule], [OriginColleagueAsk], [OriginCodeChange],
	// [OriginAlarm] or [OriginDelegatedRequest].
	Kind string `json:"kind"`
	// ID is the object's own id in that kind: a chat message's id, a work
	// item's key, a schedule's name, a colleague channel's id, a change as
	// the Gitlet screens name it (web!42), an alarm instance as the alarms
	// page names it.
	ID string `json:"id"`
	// Reason is why the object reached this seat, in its source's own
	// vocabulary (a chat routing reason, a tracker reason), and empty where
	// the source states none.
	Reason string `json:"reason,omitempty"`
}

// The kinds of object a turn can be started by.
//
// A code notice and an alarm wake a seat through the notification pipeline
// as a chat message does, so each names its object too: the change the
// notice is about, under the notice's reason, and the alarm instance, under
// what it did (fired, cleared, found firing). Without them a turn a review
// request or a firing alarm started could be found only by its delivery.
const (
	OriginChatMessage  = "chat_message"
	OriginWorkItem     = "work_item"
	OriginSchedule     = "schedule"
	OriginColleagueAsk = "colleague_ask"
	OriginCodeChange   = "code_change"
	OriginAlarm        = "alarm"
	// OriginDelegatedRequest is a request put to a delegate (ADR-0125), by
	// its id: a held call's.
	OriginDelegatedRequest = "delegated_request"
)

// IsZero reports an origin that names nothing.
func (o TriggerOrigin) IsZero() bool { return o == TriggerOrigin{} }

// Originated is a waking event that names what started the turn it wakes.
type Originated interface {
	TriggerOrigin() TriggerOrigin
}

// Gathered is a waking event that took in several objects at once, a
// coalesced notification, and names each of them ([Trigger.Origins]).
type Gathered interface {
	TriggerOrigins() []TriggerOrigin
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
	if origin, ok := e.Data.(Originated); ok {
		t.Origin = origin.TriggerOrigin()
	}
	if gathered, ok := e.Data.(Gathered); ok {
		t.Origins = gathered.TriggerOrigins()
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
//
// FIELD BY FIELD because Origins, a slice, makes the struct incomparable, so
// `t == Trigger{}` no longer compiles. A field added to Trigger must be added
// here too; TestTriggerIsZeroSeesEveryField fails until it is.
func (t Trigger) IsZero() bool {
	return t.ID == "" && t.Type == "" && t.Summary == "" && t.Actor == "" &&
		t.Timestamp.IsZero() && t.Integration == "" && t.Sender == "" &&
		t.SourceEventType == "" && t.Origin.IsZero() && len(t.Origins) == 0
}

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
	if !t.Origin.IsZero() {
		descriptor["origin"] = t.Origin.wire()
	}
	if len(t.Origins) > 0 {
		origins := make([]any, 0, len(t.Origins))
		for _, o := range t.Origins {
			origins = append(origins, o.wire())
		}
		descriptor["origins"] = origins
	}
	return descriptor
}

// wire is an origin in the descriptor's loose form: its reason only where the
// source stated one.
func (o TriggerOrigin) wire() map[string]any {
	out := map[string]any{"kind": o.Kind, "id": o.ID}
	if o.Reason != "" {
		out["reason"] = o.Reason
	}
	return out
}

// MarshalJSON writes the descriptor through Map, so the JSON a node publishes
// and the map an API handler hands the dashboard cannot drift apart.
func (t Trigger) MarshalJSON() ([]byte, error) { return json.Marshal(t.Map()) }

// triggerWire mirrors the map form for decoding. Every key is optional: a
// descriptor written by a node that knew fewer of them is still a descriptor.
type triggerWire struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Summary         string          `json:"summary"`
	Actor           string          `json:"actor"`
	Timestamp       string          `json:"timestamp"`
	Integration     string          `json:"integration"`
	Sender          string          `json:"sender"`
	SourceEventType string          `json:"source_event_type"`
	Origin          TriggerOrigin   `json:"origin"`
	Origins         []TriggerOrigin `json:"origins"`
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
		Origin:          wire.Origin, Origins: wire.Origins,
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
	// THE FIRST RUNE, not the first byte. s[:1] on a multi-byte lead rune
	// hands ToUpper an invalid UTF-8 fragment, which it substitutes: an
	// "Éditeur" phrase renders as "\uFFFD\x89diteur" in the event feed and the
	// store's summary column. Every phrase today starts with an ASCII
	// literal, so this is latent rather than live — and one variable-led
	// phrase is all it takes. Still no locale machinery: this upper-cases
	// one rune, it does not title-case a sentence.
	first, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(first)) + s[size:]
}
