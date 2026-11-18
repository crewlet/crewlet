package notify

import (
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Prompt is everything the spine needs to know about ONE third-party app.
//
// Nine questions, each asked by a different part of the spine (Build,
// Provenance, RequiresRecon, SubjectIsLabel, Addressed, PartitionKey,
// ConversationIdentity, WakesActor, DigestBody), so one interface rather than
// nine registries that could drift about what a source is. Each method's doc
// names who asks and why its conservative answer is what it is.
//
// A third-party app implements it; nothing in the spine does. The generic
// fallback answers for a source with no prompt, the most conservative reading
// of each question.
type Prompt interface {
	// Source is the integration name this prompt answers for.
	Source() string

	// Build renders the notification as the ask a seat is given.
	//
	// n.Body ARRIVES FENCED: [Render], the one caller, has already put it
	// inside a fence of the wake's token labelled by [Prompt.Provenance]
	// (ADR-0040), so a prompt renders it where it likes and cannot hand a
	// seat somebody's words bare. A blank body arrives as it is.
	//
	// Parties resolves external ids to colleagues; a nil one is valid and
	// means the raw ids are rendered, which is what a node with no org
	// context can honestly do.
	Build(n Inbound, parties Parties) string

	// Provenance is what the notification's body is (the surface it was read
	// from) and who wrote it: what the spine fences it as ([Render]) and what
	// the turn's ask is fenced as for an auxiliary model.
	//
	// Asked of the source because only it knows its surfaces: a code host's
	// body is a comment on one event and a description on the next, and who
	// CAUSED the event is often not who wrote the body. Most sources answer from
	// what their parser stamped ([Stated]); a chat message answers from its
	// sender. An author nobody can name is no author, which the fence labels
	// `external`.
	Provenance(n Inbound, parties Parties) Provenance

	// RequiresRecon reports that the body only says WHERE to look.
	//
	// A webhook saying "PR #42 got a comment" gives a seat no context until it
	// fetches the thread. When true, the turn-start relevance filters skip
	// their auxiliary model call: filtering against a bare pointer is worth
	// nothing and the agent is told to re-query after it has looked.
	RequiresRecon(n Inbound) bool

	// SubjectIsLabel reports that the subject is the SURFACE's name for the
	// event (the same words on every event) rather than part of what was sent.
	//
	// The turn's ask leads with the subject, content on most sources. A chat
	// backend's is "Slack message" on every message: leading with it gave every
	// chat turn the same words and pulled every turn's vector towards every
	// other. Only the source knows which kind it writes.
	//
	// The conservative answer is FALSE: a label read as content costs a few
	// repeated words, content read as a label drops what was sent.
	SubjectIsLabel(n Inbound) bool

	// Addressed reports that somebody is waiting on THIS seat for an
	// answer: a direct message, a personal mention, an assignment.
	//
	// The turn engine's delivery check reads it: an addressed turn may not end
	// in silence, which to the asker is indistinguishable from a lost message;
	// an unaddressed one may do nothing, which makes triage cheap.
	//
	// Only the app can say which events are an ask (a tracker's routing reason,
	// a chat backend's channel type and whether the seat was named), so it is
	// not pattern-matched centrally. The conservative answer is FALSE: a seat
	// wrongly told nobody waits may stay silent, while one wrongly told somebody
	// waits must post something on every broadcast it observes.
	Addressed(n Inbound) bool

	// PartitionKey is the SOURCE-LOCAL grouping key: which other events
	// this one is handled together with.
	//
	// Local ("ENG-42", "C42:1718.003") because the caller namespaces it. Empty
	// means no key can be derived and the event is never merged.
	//
	// Two readers: the broker partitions a seat's inbox by it and the coalescer
	// merges a partition into one trigger. A parked executor matches on the
	// identity instead: a person answers on the conversation, and their reply
	// often lands in a finer partition than the question.
	PartitionKey(metadata map[string]string, subject string) string

	// ConversationIdentity is the SOURCE-LOCAL identity of the ongoing
	// conversation: what the executor log, the episodes and conversation_key
	// file this event under. Namespaced like [Prompt.PartitionKey]; empty means
	// none. REQUIRED, not an optional side-interface, whose type assertion would
	// give a source that forgot it a silent default.
	//
	// THE INVARIANT EVERY IMPLEMENTATION OWES: the identity is CONSTANT ACROSS
	// EVERY EVENT IN A PARTITION, so a partition key is the identity or a finer
	// cut of it; else a coalesced turn's key would depend on which event sorted
	// first. [ChatPrompt] and Worklet's cut finer (Worklet's by the surface a
	// wake owes its reply on, so a merge never drops one of two promises).
	ConversationIdentity(metadata map[string]string, subject string) string

	// WakesActor reports whether a notification reaches the party who
	// caused it, overriding the self-action rule.
	//
	// True for the OUTCOME of the actor's own action (a failed pipeline names
	// the pusher, who has to fix it); false for what the actor already knows,
	// nearly everything. See [WakesActor] for a source with no prompt.
	//
	// HANDED THE WHOLE NOTIFICATION, never its event type alone: the native
	// tracker answers by the ROUTING REASON the parser stamped on each
	// recipient's copy. A status change that unblocked a task reaches its actor
	// as `unblocked` and no other copy, yet the type is `status` on all, so a
	// type-only answer is no for every copy.
	WakesActor(n Inbound) bool

	// DigestBody is the per-source supersede rule for one constituent of a
	// merged trigger.
	//
	// Some third-party apps re-emit their whole current state on every
	// event (a tracker sending the full issue description each time a
	// field changes), so rendering N copies of it buries the one line
	// that actually changed. Such a source returns "" for those event
	// types and the digest line collapses to its lead; only the latest
	// state, rendered in full below the digest, matters. A source whose
	// every event IS a message keeps them all.
	DigestBody(eventType, body string) string
}

// Inbound is one notification as the spine sees it.
//
// The third-party app's parser produces this; everything downstream reads
// it. It is deliberately flat and stringly-typed in Metadata: what a
// third-party app puts there is its own business, and the spine's only
// interest is the handful of keys the prompt itself reads back.
type Inbound struct {
	Source string

	// EventType is the third-party app's own name for what happened:
	// "issue_comment", "message", "pipeline.failed".
	EventType string

	// Sender is the third-party app's external id for who sent it, resolved to a
	// colleague through Parties where one is known.
	Sender string

	// Writer is who wrote the record a FIRST-PARTY notice announces — set by
	// the engine's own producers alone, from the record ([FirstPartyWriter]),
	// and nil on every vendor's: a first-party Sender is a handle, a login or
	// a node's name, and only the record's actor kind says which. See
	// [types.NotificationWriter].
	Writer *types.NotificationWriter

	Subject string

	// Body is the message as it arrived, raw. The prompt turns it into the
	// enriched trigger and is handed it fenced ([Render]); every other
	// question the spine asks of a notification is asked of it raw.
	Body string

	Metadata map[string]string

	// Origin is what this notification is about and why it reaches its
	// recipient ([types.TriggerOrigin]), SET BY A FIRST-PARTY PARSER PER
	// RECIPIENT: the reason is the recipient's own. The delivery carries it
	// onto the notification it publishes, so the turn it wakes is filed
	// under the object a person holds. Zero on every vendor edge.
	Origin types.TriggerOrigin

	// Fence is the token a prompt fences the foreign text it weaves into the
	// trigger under ([provenance.Fence]): minted by the delivery for this
	// wake alone and recorded on the notification it publishes, so the turn
	// that reads the trigger can re-key those fences onto its own. Set by the
	// service, never by a parser.
	Fence provenance.Boundary

	// Owes names the CHAT SURFACE the woken seat promised to post on, when the
	// promise is not simply "answer where you were asked" ([Prompt.Addressed]).
	//
	// SET BY THE PARSER, PER RECIPIENT, because only it knows which copy of one
	// change carries a promise: the tracker's `answered` wake to an asker whose
	// decision said it would report the outcome in a channel, not the other
	// copies. Empty, on every vendor edge, owes nothing beyond the source.
	//
	// A FIELD, not a [Prompt] method: it is one first-party promise, and nine
	// prompts answering "" would be nine places to forget a merge must carry it.
	Owes string
}

// FirstPartyWriter is the writer a first-party notice states
// ([Inbound.Writer]) as the record names it: the seat's handle for a seat's or
// a person's write (iam.ActorFor), the credential's login with its recorded
// apps (iam.ActorApps) for a credential acting as no seat, nothing for the
// engine's. THE KIND DECIDES, NEVER THE SPELLING: a handle can be a node's name.
//
// THE ONE PLACE A WRITER IS BUILT, so no credential's write goes without the
// apps a woken turn may use (none is worked for as a stranger), nor an agent's
// without whom it was written for (ADR-0109). Apps are kept for a credential
// alone, known ones, once each, in iam.Apps order; the carrier for an agent's
// write and the engine's record about an agent's earlier act (only
// internal/gitlet's writerOf); the store refuses it elsewhere (types.WrittenFor.For).
func FirstPartyWriter(kind iam.ActorKind, actor string, apps []iam.Grant,
	workingFor *types.WorkingFor) *types.NotificationWriter {

	w := &types.NotificationWriter{Kind: kind}
	switch kind {
	case iam.ActorAgent, iam.ActorHuman:
		w.Seat = strings.TrimSpace(actor)
	case iam.ActorOperator:
		w.Login = strings.TrimSpace(actor)
		w.Apps = knownApps(apps)
	}
	if (kind == iam.ActorAgent || kind == iam.ActorSystem) && workingFor != nil {
		w.WorkingFor = workingFor.Clone()
		w.WorkingFor.Apps = knownApps(workingFor.Apps)
	}
	return w
}

// knownApps is apps as a writer carries them: only the apps this build knows,
// once each, in iam.Apps order, and nil for none.
func knownApps(apps []iam.Grant) []iam.Grant {
	var out []iam.Grant
	for _, app := range iam.Apps {
		if slices.Contains(apps, app) {
			out = append(out, app)
		}
	}
	return out
}

// Parties resolves a third-party app's external id to a colleague in the org.
//
// An interface rather than the registry itself so a prompt can be built and
// tested without an organization, and so the prompt package never becomes a
// second place that knows how a party is resolved.
type Parties interface {
	// ByExternalID resolves a transport-scoped external id. The second
	// result is false when nobody matches, which is ordinary: most senders
	// on a shared channel are not colleagues.
	//
	// It answers for EVERY source, the engine's own included: on a
	// first-party source the id is the seat's handle, and the registry
	// answers it as one ([NewRegistry]'s handle namespaces) — so a reader
	// that asks whoever a trigger names, without knowing which sources are
	// the engine's, is answered the same colleague a first-party prompt
	// names.
	ByExternalID(transport, externalID string) (Party, bool)

	// ByHandle resolves a seat's own identity.
	//
	// FOR THE FIRST-PARTY SOURCES, whose actor IS a handle: nothing on
	// Worklet or Doclet authenticates as an account
	// somewhere else, so a prompt that already holds the handle asks by it
	// rather than through a namespace.
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
	// Derived rather than looked up (a UUIDv5 over the org name and the
	// handle, ADR-0013), which is what lets one node address a seat another
	// node is running.
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
// A VALUE built and passed, not a package-level map: a mutable global would
// make registration order matter, leak one test's app into the next, and stop
// a process running two companies with different integration sets, as an epoch
// swap does.
//
// THE ZERO VALUE IS A REGISTRY WITH NO APPS and answers every source with the
// generic fallback, which is why the fallback is not a field: a field is nil
// wherever [NewPrompts] did not build the value, and a service handed a zero
// registry (every node has one before an integration is wired) would panic on
// the first delivery. The value is copied on every inbound event.
type Prompts struct {
	bySource map[string]Prompt
}

// NewPrompts builds a registry over the given third-party apps.
//
// A later entry for the same source REPLACES an earlier one, so a caller can
// layer an override over the defaults without having to filter the list first.
func NewPrompts(prompts ...Prompt) Prompts {
	registry := Prompts{bySource: make(map[string]Prompt, len(prompts))}
	for _, p := range prompts {
		if p == nil || p.Source() == "" {
			continue
		}
		registry.bySource[p.Source()] = p
	}
	return registry
}

// With returns a registry carrying one more third-party app.
//
// A COPY, so a service handing its registry to a delivery in flight cannot
// have it change underneath: the value is read once per event and a
// registration that mutated it in place would be a data race on the one path
// that runs on every inbound message.
func (r Prompts) With(p Prompt) Prompts {
	if p == nil || p.Source() == "" {
		return r
	}
	next := Prompts{bySource: maps.Clone(r.bySource)}
	if next.bySource == nil {
		next.bySource = map[string]Prompt{}
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
	return Generic{}
}

// Sources names the third-party apps with a prompt of their own, sorted.
func (r Prompts) Sources() []string { return slices.Sorted(maps.Keys(r.bySource)) }

// Partition is the FULL partition key for a notification: the source's own
// local key, namespaced.
//
// Empty when the source cannot derive one, which the caller turns into the
// per-event fallback. It is a method on the registry rather than a function
// taking a Prompt so the namespacing rule has exactly one caller per key —
// two namespacing sites is how a Jira issue "42" and a GitLab issue "42" get
// to merge into one trigger on one of them and not the other.
func (r Prompts) Partition(n Inbound) string {
	return Namespaced(n.Source, r.For(n.Source).PartitionKey(n.Metadata, n.Subject))
}

// Conversation is the FULL conversation identity for a notification, through
// the same namespacing rule for the same reason.
func (r Prompts) Conversation(n Inbound) string {
	return Namespaced(n.Source, r.For(n.Source).ConversationIdentity(n.Metadata, n.Subject))
}
