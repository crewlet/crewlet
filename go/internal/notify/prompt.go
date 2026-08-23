package notify

import (
	"maps"
	"slices"
	"strings"
)

// Prompt is everything the spine needs to know about ONE vendor.
//
// Four questions, and each is asked by a different part of the spine — which
// is why they are one interface rather than four registries that could drift
// out of step about what a source is:
//
//   - Build renders the trigger a seat is woken with.
//   - RequiresRecon says whether that trigger is the context or a POINTER at
//     it, which the Plan-phase prefetches read.
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
	// any context. When this is true the Plan-phase relevance filters skip
	// their auxiliary model call, because filtering against a bare pointer
	// is near-guaranteed to be worth nothing and the planner is already
	// told to re-query after it has looked.
	RequiresRecon(n Inbound) bool

	// ConversationKey is the SOURCE-LOCAL identity of the conversation.
	//
	// Local — "ENG-42", "C42:1718.003" — because the caller namespaces it.
	// Empty means this source cannot derive one for this event, and the
	// event is then never merged with anything.
	ConversationKey(metadata map[string]string, subject string) string

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
}

// Party is one addressable member of the company, human or agent.
type Party struct {
	Handle string
	Name   string

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
