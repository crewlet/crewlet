package notify

import (
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Prompt is everything the spine needs to know about ONE vendor.
//
// Four questions, and each is asked by a different part of the spine — which
// is why they are one interface rather than four registries that could drift
// out of step about what a source is:
//
//   - Build renders the trigger a seat is woken with.
//   - RequiresRecon says whether that trigger is the context or a POINTER at
//     it, which the turn-start prefetches read.
//   - Addressed says whether somebody is waiting on this seat for an answer,
//     which the turn engine's delivery check reads.
//   - ConversationKey identifies the conversation, for inbox partitioning.
//   - DigestBody is the supersede rule when several of them merge.
//
// A vendor implements it; nothing in the spine does. The generic fallback
// below is the answer for a source nobody has written one for — an extension's
// own events, or a vendor added to config before its prompt exists — and it is
// deliberately the most conservative reading of each question.
type Prompt interface {
	// Source is the integration name this prompt answers for.
	Source() string

	// Build renders the notification as the ask a seat is given.
	//
	// Parties resolves external ids to colleagues; a nil one is valid and
	// means the raw ids are rendered, which is what a node with no org
	// context can honestly do.
	Build(n Inbound, parties Parties) string

	// RequiresRecon reports that the body only says WHERE to look.
	//
	// A webhook saying "PR #42 got a comment" tells a seat where the work
	// is, not what it is: the seat has to fetch the thread before it has
	// any context. When this is true the turn-start relevance filters skip
	// their auxiliary model call, because filtering against a bare pointer
	// is near-guaranteed to be worth nothing and the agent is already
	// told to re-query after it has looked.
	RequiresRecon(n Inbound) bool

	// Addressed reports that somebody is waiting on THIS seat for an
	// answer: a direct message, a personal mention, an assignment.
	//
	// The turn engine reads it as the half of "did this turn deliver?" a
	// model cannot get wrong — an addressed turn may not end in silence,
	// because to the person who asked, silence is indistinguishable from a
	// message that was lost. An unaddressed one may end having done
	// nothing at all, which is what makes triage cheap.
	//
	// Only the vendor can say which of its events are an ask. A tracker's
	// answer is its own routing reason (assigned, mentioned); a chat
	// backend's is the channel type and whether the seat was named. So it
	// is asked here rather than pattern-matched centrally, and the
	// conservative answer is FALSE: a seat wrongly told nobody is waiting
	// keeps the freedom to stay silent, while one wrongly told somebody is
	// must post something on every broadcast it observes.
	Addressed(n Inbound) bool

	// ConversationKey is the SOURCE-LOCAL identity of the conversation.
	//
	// Local — "ENG-42", "C42:1718.003" — because the caller namespaces it.
	// Empty means this source cannot derive one for this event, and the
	// event is then never merged with anything.
	ConversationKey(metadata map[string]string, subject string) string

	// WakesActor reports whether an event type reaches the party who
	// caused it, overriding the self-action rule.
	//
	// True for an event reporting the OUTCOME of the actor's own action —
	// a pipeline that failed names the person who pushed as its actor, and
	// they are the one who has to fix it. False for an event the actor
	// already knows about, which is nearly everything: their own comment,
	// their own assignment, their own edit.
	//
	// Only the vendor can say which of its event types are which, so it is
	// asked here rather than pattern-matched on the name centrally. See
	// [WakesActor] for what happens to a source with no registered prompt.
	WakesActor(eventType string) bool

	// DigestBody is the per-source supersede rule for one constituent of a
	// merged trigger.
	//
	// Some vendors re-emit their whole current state on every event — a
	// tracker sending the full issue description each time a field changes
	// — so rendering N copies of it buries the one line that actually
	// changed. Such a source returns "" for those event types and the
	// digest line collapses to its lead; only the latest state, rendered
	// in full below the digest, matters. A source whose every event IS a
	// message keeps them all.
	DigestBody(eventType, body string) string
}

// Inbound is one notification as the spine sees it.
//
// The vendor's parser produces this; everything downstream reads it. It is
// deliberately flat and stringly-typed in Metadata: what a vendor puts there is
// its own business, and the spine's only interest is the handful of keys the
// prompt itself reads back.
type Inbound struct {
	Source string

	// EventType is the vendor's own name for what happened —
	// "issue_comment", "message", "pipeline.failed".
	EventType string

	// Sender is the vendor's external id for who sent it, resolved to a
	// colleague through Parties where one is known.
	Sender string

	Subject string

	// Body is the message as it arrived. The prompt turns it into the
	// enriched trigger; this stays raw.
	Body string

	Metadata map[string]string
}

// Parties resolves a vendor's external id to a colleague in the org.
//
// An interface rather than the registry itself so a prompt can be built and
// tested without an organization, and so the prompt package never becomes a
// second place that knows how a party is resolved.
type Parties interface {
	// ByExternalID resolves a transport-scoped external id. The second
	// result is false when nobody matches, which is ordinary: most senders
	// on a shared channel are not colleagues.
	ByExternalID(transport, externalID string) (Party, bool)

	// ByHandle resolves a seat's own identity.
	//
	// FOR THE FIRST-PARTY SOURCES, whose actor IS a handle: nothing on the
	// engine's own tracker or knowledge base authenticates as an account
	// somewhere else, so there is no external id to scope and asking for
	// one would mean registering every seat's handle as its own external
	// id in a namespace that exists only to satisfy this call.
	//
	// A vendor parser must NOT reach for this to shortcut a lookup its
	// payload cannot support — a Slack id is not a handle, and treating
	// one as the other resolves to nobody in the ordinary case and to the
	// WRONG seat in the case where a handle happens to collide.
	ByHandle(handle string) (Party, bool)
}

// Party is one addressable member of the company, human or agent.
type Party struct {
	Handle string
	Name   string

	// AgentID is the seat's derived runtime id, and the ZERO VALUE is
	// meaningful twice over: a human seat never has one, and neither does
	// a party the registry could not derive one for. Both mean "not
	// addressable as an agent", which is the only thing a caller does with
	// it — so the two need not be told apart here.
	//
	// Derived rather than looked up (a UUIDv5 over org name and handle),
	// which is what lets one node address a seat another node is running.
	AgentID uuid.UUID

	// Human marks a seat that is addressable and never spawned. It changes
	// what a prompt TELLS the agent: a human replies on the external
	// surface, asynchronously, and cannot be reached by an agent-to-agent
	// ask — so a prompt that rendered a person as an agent would send the
	// seat looking for a tool that will never answer.
	Human bool
}

// Label renders a party as a prompt names them.
//
// Agents render as "Role (handle)": the role is what a model recognises from
// prior conversation and the handle is what a colleague lookup takes. Humans
// carry the fact of being human, because everything the seat can do next
// depends on it.
func (p Party) Label() string {
	name, handle := strings.TrimSpace(p.Name), strings.TrimSpace(p.Handle)
	if !p.Human {
		if name != "" && handle != "" {
			return name + " (" + handle + ")"
		}
		return cmpFirst(handle, name)
	}
	if name != "" && handle != "" {
		return name + " (" + handle + ", human colleague)"
	}
	if first := cmpFirst(name, handle); first != "" {
		return first + " (human colleague)"
	}
	return ""
}

func cmpFirst(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// Prompts is the per-source registry.
//
// A VALUE built and passed, not a package-level map. A mutable global would
// make registration order matter, leak one test's vendor into the next, and
// give a process no way to run two companies with different integration sets —
// which is exactly what an epoch swap is.
type Prompts struct {
	bySource map[string]Prompt
	fallback Prompt
}

// NewPrompts builds a registry over the given vendors.
//
// A later entry for the same source REPLACES an earlier one, so a caller can
// layer an override over the defaults without having to filter the list first.
func NewPrompts(prompts ...Prompt) Prompts {
	registry := Prompts{bySource: make(map[string]Prompt, len(prompts)), fallback: Generic{}}
	for _, p := range prompts {
		if p == nil || p.Source() == "" {
			continue
		}
		registry.bySource[p.Source()] = p
	}
	return registry
}

// With returns a registry carrying one more vendor.
//
// A COPY, so a service handing its registry to a delivery in flight cannot
// have it change underneath: the value is read once per event and a
// registration that mutated it in place would be a data race on the one path
// that runs on every inbound message.
func (r Prompts) With(p Prompt) Prompts {
	if p == nil || p.Source() == "" {
		return r
	}
	next := Prompts{bySource: maps.Clone(r.bySource), fallback: r.fallback}
	if next.bySource == nil {
		next.bySource = map[string]Prompt{}
	}
	if next.fallback == nil {
		next.fallback = Generic{}
	}
	next.bySource[p.Source()] = p
	return next
}

// For returns the prompt for a source, or the generic fallback.
//
// NEVER NIL. Every caller would otherwise need the same nil check, and the one
// that forgot it would panic on an event from a source somebody added to
// config that afternoon.
func (r Prompts) For(source string) Prompt {
	if p, ok := r.bySource[source]; ok {
		return p
	}
	return r.fallback
}

// Sources names the vendors with a prompt of their own, sorted.
func (r Prompts) Sources() []string { return slices.Sorted(maps.Keys(r.bySource)) }

// Key is the FULL conversation key for a notification: the source's own local
// key, namespaced.
//
// Empty when the source cannot derive one, which the caller turns into the
// per-event fallback. It is a method on the registry rather than a function
// taking a Prompt so the namespacing rule has exactly one caller.
func (r Prompts) Key(n Inbound) string {
	return Namespaced(n.Source, r.For(n.Source).ConversationKey(n.Metadata, n.Subject))
}
