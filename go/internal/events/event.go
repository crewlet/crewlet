// Package events defines the engine's event envelope and the registry that
// reconstructs typed events off the wire.
//
// Every inter-component message in Crewlet is an Event. The envelope carries
// identity (id, type, timestamp, source), trace context captured where the
// event is created, and the turn engine's delegation bookkeeping. Typed
// per-event fields live in a Payload value registered under the event's type
// string.
//
// Two properties are load-bearing and must survive any change here:
//
//   - Additive-only evolution. New fields get defaults; nothing is removed or
//     repurposed. A fleet mid-upgrade has both versions on the wire.
//   - Unknown types never fail. An event whose type this build does not know
//     decodes into the envelope with its unknown fields preserved in Extra,
//     and re-encodes losslessly. A rolling upgrade publishes types the older
//     half has never heard of; dropping or erroring on those would make every
//     upgrade an outage.
package events

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Payload is the typed body of an event. Implementations are plain structs
// with JSON tags; their fields are marshalled flat alongside the envelope,
// so an event is one JSON object rather than a nested envelope/body pair.
type Payload interface {
	// EventType returns the wire type string this payload is registered
	// under, e.g. "task_assigned".
	EventType() string
}

// Event is the envelope every message on the queue carries.
type Event struct {
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`

	// Payload is the free-form bag the Python engine also carries. Typed
	// fields belong in Data; this stays for genuinely unstructured extras.
	Payload map[string]any `json:"payload,omitempty"`

	// Trace context, captured at construction from the active span.
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id"`

	// Turn-engine bookkeeping. Populated on outbound events by the turn
	// engine and forwarded by the notification and A2A services, so an
	// agent woken by a colleague handoff inherits the correct depth and
	// chain.
	DelegationDepth int      `json:"delegation_depth"`
	ParentTurnID    string   `json:"parent_turn_id"`
	DelegationChain []string `json:"delegation_chain,omitempty"`

	// Data is the typed body, non-nil when Type is registered in this
	// build. Marshalled flat into the same JSON object as the envelope.
	Data Payload `json:"-"`

	// Extra holds fields that belong to neither the envelope nor a known
	// Data type — which is what an event from a newer build looks like.
	// Preserved verbatim so a round trip through an older node is lossless.
	Extra map[string]any `json:"-"`
}

// envelopeKeys are the JSON keys owned by the envelope itself. Anything else
// in an incoming object belongs to Data (if the type is known) or Extra.
var envelopeKeys = map[string]struct{}{
	"id": {}, "type": {}, "timestamp": {}, "source": {}, "payload": {},
	"trace_id": {}, "span_id": {}, "parent_span_id": {},
	"delegation_depth": {}, "parent_turn_id": {}, "delegation_chain": {},
}

// New builds an event of the given type carrying data, stamping a fresh id,
// the current time, and the caller-supplied trace context.
//
// Trace context is passed rather than read from an ambient span: the Go
// rewrite threads context explicitly (see rewrite/decisions/401), and the
// callers that create events already hold the context that knows the span.
func New(data Payload, tc TraceContext) *Event {
	return &Event{
		ID:           uuid.New(),
		Type:         data.EventType(),
		Timestamp:    time.Now().UTC(),
		TraceID:      tc.TraceID,
		SpanID:       tc.SpanID,
		ParentSpanID: tc.ParentSpanID,
		Data:         data,
	}
}

// TraceContext is the W3C trace linkage stamped onto an event at creation.
type TraceContext struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
}

// The optional interfaces below let a payload contribute to the envelope's
// derived fields. They are deliberately narrow: a payload states the ONE fact
// it knows and the envelope owns the resolution order, so "who was the actor"
// has a single definition instead of one per event type.
//
// Handing a payload the whole *Event was the alternative. It would let sixty
// payloads each re-derive the chain, and the moment two of them disagree the
// same turn reads differently on two surfaces.
type (
	// Summarizer provides a summary needing nothing but the payload.
	Summarizer interface{ Summary() string }

	// ActorSummarizer provides a summary that leads with the actor. The
	// RESOLVED actor is passed in, so a payload never has to know how the
	// chain reaches it.
	ActorSummarizer interface{ SummaryFor(actor string) string }

	// Actorer names the actor outright, overriding the chain — for events
	// whose actor is neither the role nor the publisher, such as an
	// inbound notification's human sender.
	Actorer interface{ Actor() string }

	// Roler contributes the payload's role, the chain's first link.
	Roler interface{ Role() string }

	// AgentIdentified contributes an agent id, the chain's last resort
	// before falling back to the engine itself.
	AgentIdentified interface{ AgentID() string }
)

// Summary is the human-readable "who did what" line the dashboard shows in
// its trace view.
func (e *Event) Summary() string {
	if s, ok := e.Data.(ActorSummarizer); ok {
		if out := s.SummaryFor(e.Actor()); out != "" {
			return out
		}
	}
	if s, ok := e.Data.(Summarizer); ok {
		if out := s.Summary(); out != "" {
			return out
		}
	}
	return titleWords(strings.ReplaceAll(e.Type, "_", " "))
}

// titleWords upper-cases the first letter of each space-separated word.
// Event type strings are ASCII snake_case by construction, so this needs
// none of the locale machinery a general title-caser would carry.
func titleWords(s string) string {
	parts := strings.Split(s, " ")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// Actor names who performed the event.
//
// The chain, in order: an explicit override, the payload's role, the
// envelope's source, an agent id, then the engine itself. Each link is what
// some family of events actually knows about itself, and resolving them in
// one place is what keeps a seat's activity attributable across every surface
// that renders it.
func (e *Event) Actor() string {
	if a, ok := e.Data.(Actorer); ok {
		if out := a.Actor(); out != "" {
			return out
		}
	}
	if r, ok := e.Data.(Roler); ok {
		if out := r.Role(); out != "" {
			return out
		}
	}
	if e.Source != "" {
		return e.Source
	}
	if a, ok := e.Data.(AgentIdentified); ok {
		if out := a.AgentID(); out != "" {
			return out
		}
	}
	return "system"
}

// Clone returns a deep-enough copy for requeue: identity, timestamp and trace
// are preserved verbatim, which the completion ledger's idempotency and the
// oldest-first batch ordering both depend on.
func (e *Event) Clone() *Event {
	out := *e
	if e.Payload != nil {
		out.Payload = maps.Clone(e.Payload)
	}
	if e.Extra != nil {
		out.Extra = maps.Clone(e.Extra)
	}
	out.DelegationChain = slices.Clone(e.DelegationChain)
	return &out
}

// envelope mirrors Event's JSON-visible envelope fields without the custom
// marshalling, so MarshalJSON can encode it without recursing.
type envelope struct {
	ID              uuid.UUID      `json:"id"`
	Type            string         `json:"type"`
	Timestamp       time.Time      `json:"timestamp"`
	Source          string         `json:"source"`
	Payload         map[string]any `json:"payload,omitempty"`
	TraceID         string         `json:"trace_id"`
	SpanID          string         `json:"span_id"`
	ParentSpanID    string         `json:"parent_span_id"`
	DelegationDepth int            `json:"delegation_depth"`
	ParentTurnID    string         `json:"parent_turn_id"`
	DelegationChain []string       `json:"delegation_chain,omitempty"`
}

// MarshalJSON emits the envelope and the typed body as ONE flat object.
//
// Flat rather than nested because the whole system — dashboards, the event
// store's promoted filter columns, an operator reading a log — treats an
// event's own fields as first-class, and a nested body would push every one
// of them behind an extra hop.
func (e *Event) MarshalJSON() ([]byte, error) {
	env := envelope{
		ID: e.ID, Type: e.Type, Timestamp: e.Timestamp, Source: e.Source,
		Payload: e.Payload, TraceID: e.TraceID, SpanID: e.SpanID,
		ParentSpanID: e.ParentSpanID, DelegationDepth: e.DelegationDepth,
		ParentTurnID: e.ParentTurnID, DelegationChain: e.DelegationChain,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal event envelope: %w", err)
	}
	merged := map[string]any{}
	if err := json.Unmarshal(raw, &merged); err != nil {
		return nil, fmt.Errorf("remap event envelope: %w", err)
	}
	// Unknown-type fields first, so a known Data field always wins over a
	// stale copy of itself that rode in as Extra.
	for k, v := range e.Extra {
		if _, reserved := envelopeKeys[k]; !reserved {
			merged[k] = v
		}
	}
	if e.Data != nil {
		body, err := json.Marshal(e.Data)
		if err != nil {
			return nil, fmt.Errorf("marshal event data (%s): %w", e.Type, err)
		}
		fields := map[string]any{}
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, fmt.Errorf("remap event data (%s): %w", e.Type, err)
		}
		for k, v := range fields {
			if _, reserved := envelopeKeys[k]; !reserved {
				merged[k] = v
			}
		}
	}
	return json.Marshal(merged)
}

// UnmarshalJSON reconstructs an event, decoding the typed body when this
// build knows the type and preserving unknown fields in Extra when it does
// not. It never fails on an unknown type — see the package doc.
func (e *Event) UnmarshalJSON(data []byte) error {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("unmarshal event envelope: %w", err)
	}
	e.ID, e.Type, e.Timestamp, e.Source = env.ID, env.Type, env.Timestamp, env.Source
	e.Payload, e.TraceID, e.SpanID = env.Payload, env.TraceID, env.SpanID
	e.ParentSpanID, e.DelegationDepth = env.ParentSpanID, env.DelegationDepth
	e.ParentTurnID, e.DelegationChain = env.ParentTurnID, env.DelegationChain

	all := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("scan event fields: %w", err)
	}

	if proto, ok := lookup(env.Type); ok {
		body := proto()
		if err := json.Unmarshal(data, body); err != nil {
			// A malformed body must not lose the event: fall through to
			// the envelope-plus-Extra representation, which is exactly
			// how an unknown type is carried.
			e.Data = nil
		} else {
			e.Data = body
		}
	}

	known := map[string]struct{}{}
	if e.Data != nil {
		if raw, err := json.Marshal(e.Data); err == nil {
			fields := map[string]json.RawMessage{}
			if json.Unmarshal(raw, &fields) == nil {
				for k := range fields {
					known[k] = struct{}{}
				}
			}
		}
	}
	for k, v := range all {
		if _, reserved := envelopeKeys[k]; reserved {
			continue
		}
		if _, isKnown := known[k]; isKnown {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			continue
		}
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		e.Extra[k] = val
	}
	return nil
}

// --- registry -------------------------------------------------------------

var (
	registryMu sync.RWMutex
	registry   = map[string]func() Payload{}
)

// Register associates a wire type string with a constructor for its payload.
// Registration is explicit — there is no reflection scan over a types package
// — so the set of types this build understands is greppable and a typo shows
// up as an unknown type rather than a silently missing subclass.
//
// Panics on a duplicate type string: two payloads under one name is a
// programming error that would otherwise decode events into the wrong struct.
func Register[T Payload](proto T) {
	t := proto.EventType()
	if t == "" {
		panic("events.Register: empty event type")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[t]; dup {
		panic("events.Register: duplicate event type " + t)
	}
	registry[t] = func() Payload {
		var zero T
		v := any(&zero)
		//nolint:errcheck // T is constrained to Payload; *T implements it too.
		return v.(Payload)
	}
}

func lookup(t string) (func() Payload, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[t]
	return f, ok
}

// RegisteredTypes returns every event type this build knows, sorted. Used by
// diagnostics and by the test that asserts the registry matches the declared
// event set.
func RegisteredTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return slices.Sorted(maps.Keys(registry))
}

// DataAs decodes an event's typed body into *T, reporting whether the event
// actually carries that type. It is the one way callers should reach typed
// fields, so an unknown or mismatched type is a boolean rather than a panic.
func DataAs[T Payload](e *Event) (T, bool) {
	var zero T
	if e == nil || e.Data == nil {
		return zero, false
	}
	v, ok := e.Data.(T)
	if !ok {
		return zero, false
	}
	return v, true
}
