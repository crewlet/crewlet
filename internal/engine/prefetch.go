package engine

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracing"
)

// What a turn is given before it starts.
//
// The prefetch is assembled here rather than inside the runner, and that
// placement IS the freeze: a runner handed strings has nowhere to re-fetch
// from, so a self_iterate loop cannot move the system prompt underneath the
// executor or cost a provider's prompt cache on every pass. A turn that wants
// something the freeze did not surface asks for it with a tool call —
// search_knowledge — which is an ordinary entry in its own log.

// prefetcher builds the fetcher for the current node.
//
// Per CALL rather than held, because most of its sources move: the knowledge
// searcher is rebuilt whenever the tracker is reconciled, the party registry
// is rebuilt on every apply, the chat readers follow whichever transports are
// running, and a node with no store has none of the rest. Building it is
// assembling a handful of interface values — cheaper than the mutex a cached
// one would need.
func (e *Engine) prefetcher(company *Company) *prefetch.Fetcher {
	return prefetch.New(e.prefetchSources(company))
}

// prefetchSources assembles what [Engine.prefetcher] builds its fetcher from.
func (e *Engine) prefetchSources(company *Company) prefetch.Sources {
	src := prefetch.Sources{
		Knowledge: e.Knowledge(),
		// THROUGH THE AUXILIARY SEAM, like every other completion made on
		// a seat's behalf. The memory filter, the knowledge query and the
		// episode summary each send a full prompt on EVERY turn, and
		// resolved off the bare registry that spend reached no counter and
		// no record: a seat at its ceiling kept paying for its turn-start
		// context and every figure an operator reads understated it. See
		// auxiliary.go.
		Models: e.auxiliaryFor(company),
		// The same seam, as a compactor: the middle of a long chat thread
		// is condensed rather than dropped, on the seat's own aux chain.
		Compact: e.compactorFor(company),
		// The chat surfaces' READ half, which is how a seat woken in a
		// thread is handed the thread. Empty on a node running no chat
		// transport, which renders the unreadable hint rather than
		// silently nothing — see prefetch.UnreadableThreadHint.
		Threads: e.ChatThreads(),
		// SummarizeEpisodes gates ONLY the episode summary. Wiring this
		// switch by passing a nil provider pool silently disables the
		// memory and knowledge filters too — an operator turning off a
		// summary gets a company with no memory.
		SummarizeEpisodes:  company.Config.Learning.Reflect.SummarizeEpisodes.Or(true),
		SummarizeMaxTokens: company.Config.Learning.Reflect.SummarizeMaxTokens,
	}
	if db := e.backends.Store; db != nil {
		src.Diary = learning.NewDiary(db)
		src.Episodes = learning.NewEpisodes(db)
		src.Counterparties = learning.NewCounterparties(db)
		src.Skills = learning.NewSkills(db)
		src.Onboarding = learning.NewOnboarding(db)
	}
	// A nil *Registry would satisfy the interface and then be dereferenced,
	// so the seam is left unset rather than filled with one — which renders
	// every sender as the raw platform id, legible but not recognisable.
	if reg := e.Registry(); reg != nil {
		src.Parties = reg
	}
	// THE CURRENT EMBEDDER, read at each call rather than now: this
	// fetcher is also the pull tools' (see [Engine.equip]), which is built
	// before the epoch it equips stores its embedder. A company with none
	// configured answers learning.ErrNoEmbeddings, which degrades the
	// similarity half of the memory pool to recency alone and episode
	// recall to an empty block — both first-class states in the prefetch
	// rather than failures.
	src.Embed = e.embedText
	return src
}

// prefetchFor renders one turn's context blocks.
//
// The SEAT and the AGENT ID are read off the pinned epoch, like everything
// else a turn is built from: a prefetch resolved against a revision the turn
// is not running would surface another company's memory.
func (e *Engine) prefetchFor(ctx context.Context, company *Company, req Request,
	task string, aux auxspend.Use,
) prefetch.Blocks {
	seat := company.Org.AgentSeatByHandle(req.Handle)
	if seat == nil {
		return prefetch.Blocks{}
	}
	agentID, _ := company.Org.AgentIDFor(seat)
	r := prefetch.Request{
		Seat: seat, AgentID: agentID.String(), Org: company.Org,
		// THE TURN'S ATTRIBUTION, which names the RUN, not the unit of
		// work: every phase record of this turn is filed under the run,
		// so an auxiliary record carrying only the work key would sit
		// under an id no phase shares — invisible to the turn view and to
		// `GET /events?turn_id=`. See ADR-0017.
		Task: task, Aux: aux,
		// WHAT IT WAS ASKED, which every relevance judgement is made
		// against while the executor is handed the task — see
		// [prefetch.Request.Ask] and [turnAsk].
		Ask: turnAsk(req.Ask()),
		// OFF THE ASK, which for a coalesced conversation is the merged
		// digest and for everything else is the partition itself. One
		// shape rather than two: the merge is where a conversation's
		// senders and its recon flag are combined, and re-deriving them
		// from the constituents here would be a second answer to a
		// question already answered — free to disagree, and with the
		// merged event's own constituent list then read by nothing.
		Senders: sendersOf(e.Registry(), req.Ask()),
		// A POINTER TRIGGER gates the three searches that judge relevance
		// against the trigger text. Read off the events rather than
		// recomputed, because the parser that produced them is the only
		// thing that knows whether its third-party app's body is the context or a
		// reference to it.
		RequiresRecon: requiresRecon(req.Ask()),
		// THE TRIGGER'S OWN THREAD, resolved by the notification layer
		// that stamped it rather than re-derived here: the working
		// indicator, the reply target and this block must never disagree
		// about which thread a turn is in.
		Thread: threadOf(req.Ask()),
	}
	// TIMED where it runs: the context assembly is the stretch between a
	// turn announcing itself and its first phase opening, and nothing else
	// on the record can say how long it was.
	began := time.Now()
	blocks := e.prefetcher(company).Fetch(ctx, r)
	took := time.Since(began)
	e.publishPrefetchSummary(ctx, seat, agentID.String(), req.RunID, req.WorkKey, r, blocks,
		began.UTC(), took)
	e.publishPrefetchRead(ctx, seat, agentID.String(), req.RunID, req.WorkKey, blocks)
	return blocks
}

// publishPrefetchRead records the pages the turn-start knowledge block put in
// front of the seat, as a `knowledge_read` with `via: prefetch`.
//
// A SECOND EVENT rather than a list on prefetch_summary, because the two answer
// different questions for different readers: the summary is one row per turn
// about the PIPELINE (did each block fire, how large was it), and this is one
// row per read about the PAGES, which is what every other way a seat reads the
// knowledge base records too — so "which pages does this company's staff read"
// is one event type to count however the page arrived.
//
// NAMES NO PHASE: the prefetch runs before the first one opens. Nothing is
// published when the block surfaced no page, a hint included — a read of
// nothing is not a read. Best effort, like the summary beside it.
func (e *Engine) publishPrefetchRead(ctx context.Context, seat *org.Role,
	agentID, runID, workKey string, b prefetch.Blocks,
) {
	if e.backends == nil || e.backends.Queue == nil || len(b.RelevantKnowledgePages) == 0 {
		return
	}
	read := knowledge.ReadPages(b.RelevantKnowledgePages)
	if len(read) == 0 {
		return
	}
	ev := events.NewFrom(types.KnowledgeRead{
		Agent: agentID, AgentHandle: seat.Handle(), RoleName: seat.Name,
		TurnID: runID, WorkKey: workKey,
		Via:     types.ReadViaPrefetch,
		Backend: b.RelevantKnowledgePages[0].Backend,
		Query:   types.KnowledgeReadQuery(b.RelevantKnowledgeQuery),
		Pages:   read,
	}, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	e.publishEvent(ctx, ev, seat.Name)
}

// publishPrefetchSummary reports what each block actually surfaced.
//
// THE ONLY SIGNAL THIS PIPELINE HAS. Every block but the knowledge and
// chat-thread ones — which say in words when their source could not be read —
// degrades silently rather than failing (see internal/agent/prefetch), so a
// seat whose diary is unreachable and one whose auxiliary model is
// misconfigured both start their turns with no memory, exactly like a seat
// that has never stored one — and nothing else distinguishes them. Per-block
// hit and rendered size, once per turn, is what lets an operator see the
// difference.
//
// Best effort, and deliberately so: this is measurement, and a turn must not
// fail because its telemetry could not be published.
func (e *Engine) publishPrefetchSummary(ctx context.Context, seat *org.Role,
	agentID, runID, workKey string, r prefetch.Request, b prefetch.Blocks,
	startedAt time.Time, took time.Duration,
) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	ev := events.NewFrom(types.PrefetchSummary{
		Agent: agentID, AgentHandle: seat.Handle(), RoleName: seat.Name,
		TurnID:                 runID,
		WorkKey:                workKey,
		StartedAt:              startedAt,
		DurationMS:             int(took / time.Millisecond),
		CounterpartyHit:        b.CounterpartyProfile != "",
		CounterpartyBytes:      len(b.CounterpartyProfile),
		SynthesizedSkillsHit:   b.SynthesizedSkills != "",
		SynthesizedSkillsBytes: len(b.SynthesizedSkills),
		EpisodeRecallHit:       b.EpisodeRecall != "",
		EpisodeRecallBytes:     len(b.EpisodeRecall),
		OnboardingHintHit:      b.OnboardingHint != "",
		OnboardingHintBytes:    len(b.OnboardingHint),
		PersonalMemoryHit:      b.PersonalMemory != "",
		PersonalMemoryBytes:    len(b.PersonalMemory),
		RelevantKnowledgeHit:   b.RelevantKnowledge != "",
		RelevantKnowledgeBytes: len(b.RelevantKnowledge),
		// The count the block cannot carry: an empty search still
		// renders the hint, so hit=true with count=0 is "it ran and
		// found nothing" rather than "it surfaced pages".
		RelevantKnowledgeSelectionCount: len(b.RelevantKnowledgePages),
		ThreadContextHit:                b.ThreadContext != "",
		ThreadContextBytes:              len(b.ThreadContext),
		ThreadContextPosts:              b.ThreadContextPosts,
		// THE TWO FACTS THE PROSE CANNOT CARRY. Both of the block's
		// zero-message paths render a non-empty hint, so hit, bytes and
		// the count together still cannot say whether a backend answered
		// — and a thread read to a bound stops short of the message that
		// woke the turn while reporting the same count as a whole one.
		ThreadContextRead:         b.ThreadContextRead,
		ThreadContextStoppedShort: b.ThreadContextStoppedShort,
		TriggerRequiresRecon:      r.RequiresRecon,
		// AND THE FACT THE EPISODE BLOCK CANNOT CARRY: an empty block is
		// both "nothing similar" and "the embedder failed, so nothing was
		// searched".
		TurnEmbedding: b.TurnEmbedding,
	}, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	e.publishEvent(ctx, ev, seat.Name)
}

// turnAsk is what a turn was ASKED — [prefetch.Request.Ask] — read off its
// trigger events, in order, one paragraph each.
//
// DescribeTrigger's job, without the wrapping. That function renders each
// event's [events.Briefer], which for a notification is the integration's
// prompt: the message behind triage guidance, reply instructions and the ids
// to act on, identical on every turn of the surface. This renders, per event:
//
//   - a notification: its SUBJECT, where the source says the subject is
//     content, and its SALIENT body — the raw message, or a coalesced burst's
//     messages attributed to their senders with the copies a source re-sent
//     left out (notify's mergedSalient). On most sources the subject is part
//     of what was sent: an issue's key and title, a page's title, a monitor's
//     alert — and on a tracker comment the only place the topic is named at
//     all. A chat backend's is not: it names the SURFACE ("Slack message"),
//     identical on every message, so it is left out
//     ([types.ExternalNotification.SubjectIsLabel]) and a chat turn's ask is
//     what was said. Led by it, every chat turn's memory filter, knowledge
//     query and episode summary were handed the same words, and every chat
//     turn's vector was pulled towards every other's;
//   - anything else that states an ask (a schedule's task, a colleague's
//     question, their answer): its brief, which is already the ask itself —
//     and for a colleague names who asked, the one sender a turn woken by a
//     colleague has;
//   - an event from a build that predates its typed payload: the body in its
//     free-form bag, as DescribeTrigger reads it.
//
// An event with none of those contributes nothing — not its type name, which
// DescribeTrigger hands the executor so it is never given a blank ask, and
// which here would be a word every such turn is judged against.
func turnAsk(evs []*events.Event) string {
	var parts []string
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if text := strings.TrimSpace(eventAsk(ev)); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// eventAsk is one trigger event's part of [turnAsk].
func eventAsk(ev *events.Event) string {
	if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil {
		body := strings.TrimSpace(salientBody(n))
		if n.SubjectIsLabel {
			return body
		}
		return strings.TrimSpace(strings.TrimSpace(n.Subject) + "\n\n" + body)
	}
	if brief, ok := ev.Data.(events.Briefer); ok {
		if b := strings.TrimSpace(brief.Brief()); b != "" {
			return b
		}
	}
	return payloadBody(ev)
}

// requiresRecon reports that ANY constituent of the trigger is a bare
// pointer.
//
// Any, not all, and conservatively so: a coalesced trigger carrying one
// webhook that only names a thing-that-changed is still a trigger the seat
// has to go and look behind, and searching on the merged text would return
// noise for the half that has no content.
func requiresRecon(evs []*events.Event) bool {
	for _, n := range notificationsIn(evs) {
		if n.ContextRequiresRecon {
			return true
		}
	}
	return false
}

// threadOf resolves the chat thread a turn was woken in, or the zero value.
//
// FIRST WINS, over the FLAT metadata of each notification rather than over
// the constituents. A coalesced event's flat fields mirror its latest
// constituent, and a chat partition is thread-grained wherever a thread
// exists — a direct conversation partitions on the bare channel, but only for
// messages with no thread at all — so a coalesced burst can never straddle
// two threads and the flat copy is the whole answer. Reading the constituents
// would offer a choice between several identical values, which is a choice
// somebody eventually makes differently.
//
// A trigger that is not a chat message contributes nothing: [notify.ThreadOf]
// keys on the `transport` stamp only a chat parser writes.
func threadOf(evs []*events.Event) notify.Thread {
	for _, n := range notificationsIn(evs) {
		if t, ok := notify.ThreadOf(n.Metadata); ok {
			return t
		}
	}
	return notify.Thread{}
}

// notificationsIn reads the notifications out of a turn's trigger envelopes.
//
// A turn is woken by EVENTS, and only some of them are notifications: a
// scheduled fire, a sandbox completion and an agent-to-agent ask all reach a
// seat the same way and none of them has a sender or a recon flag. Those
// simply contribute nothing here rather than being an error.
func notificationsIn(evs []*events.Event) []*types.ExternalNotification {
	out := make([]*types.ExternalNotification, 0, len(evs))
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil {
			out = append(out, n)
		}
	}
	return out
}

// sendersOf resolves the parties who triggered the turn, in the order they
// spoke.
//
// EVERY DISTINCT SENDER, because a coalesced trigger is several people
// speaking and a turn woken by four of them is not a turn about the last.
// Resolution goes through the party registry, so a sender who IS a seat here
// is profiled under their handle rather than under a platform id nobody
// else uses.
func sendersOf(registry *notify.Registry, evs []*events.Event) []learning.Subject {
	var (
		out  []learning.Subject
		seen = map[learning.Subject]bool{}
	)
	add := func(s learning.Subject) {
		if !s.Valid() || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, n := range notificationsIn(evs) {
		for _, s := range subjectsOf(registry, n) {
			add(s)
		}
	}
	return out
}

// subjectsOf reads the senders off one notification.
//
// A COALESCED event carries every constituent, and the flat Sender field
// mirrors only the latest — so reading the flat field alone would profile
// the last speaker and forget the other three.
//
// EITHER the constituents OR the flat fields, never both, which is the rule
// [Engine.interactionsOf] already states: the flat fields ARE the latest
// constituent, so taking them as well put the last speaker at the head of a
// list whose whole contract is "in the order they spoke". Latent while nothing
// populated Messages, and wrong the moment something did.
func subjectsOf(registry *notify.Registry, ev *types.ExternalNotification) []learning.Subject {
	if len(ev.Messages) > 0 {
		out := make([]learning.Subject, 0, len(ev.Messages))
		for _, m := range ev.Messages {
			out = append(out, subjectOf(registry, ev.NotificationSource,
				m.Sender, m.Metadata))
		}
		return out
	}
	return []learning.Subject{
		subjectOf(registry, ev.NotificationSource, ev.Sender, ev.Metadata),
	}
}

// subjectOf resolves one sender to the identity their profile is keyed on.
func subjectOf(registry *notify.Registry, source, sender string, metadata map[string]string) learning.Subject {
	external := strings.TrimSpace(metadata[notify.ActorField])
	if external == "" {
		// Every parser stamps the actor, but an engine-authored
		// notification has none — the sender field is then all there is.
		external = strings.TrimSpace(sender)
	}
	subject := learning.Subject{
		ExternalID: external, Platform: source, Name: strings.TrimSpace(sender),
	}
	if registry != nil && external != "" {
		if party, ok := registry.ByExternalID(source, external); ok {
			// A COLLEAGUE IS KEYED ON THEIR HANDLE, so what one seat
			// learned about them on chat and what it learned on the
			// tracker are one profile rather than two half-profiles
			// under two platform ids.
			subject.Handle = party.Handle
			if party.Name != "" {
				subject.Name = party.Name
			}
		}
	}
	return subject
}

// interactionsOf renders a turn's trigger as the interactions a learning
// worker reads.
//
// ON THE EVENT rather than looked up later, because the reflect dispatcher
// is a queue consumer: the node running it may never have seen the trigger,
// and an interaction it cannot read off the payload is one it cannot reason
// about at all.
//
// EVERY constituent of a coalesced trigger, in the order they spoke — the
// same rule sendersOf states, and for the same reason. A turn woken by four
// people is not a turn about the last of them.
func (e *Engine) interactionsOf(evs []*events.Event) []types.InboundInteraction {
	registry := e.Registry()
	var out []types.InboundInteraction
	for _, n := range notificationsIn(evs) {
		if len(n.Messages) == 0 {
			out = append(out, interaction(registry, n.NotificationSource,
				n.SourceEventType, n.Sender, salientBody(n), n.Metadata,
				n.ContextRequiresRecon))
			continue
		}
		// A COALESCED EVENT'S FLAT FIELDS MIRROR THE LATEST constituent,
		// so taking them as well would double-count that one message and
		// weight it against the others in every worker that joins bodies.
		for _, m := range n.Messages {
			out = append(out, interaction(registry, n.NotificationSource,
				m.SourceEventType, m.Sender, m.Body, m.Metadata,
				m.ContextRequiresRecon))
		}
	}
	return out
}

// interaction assembles one, resolving its sender the way a profile is keyed.
func interaction(registry *notify.Registry, source, rawType, sender, body string,
	metadata map[string]string, requiresRecon bool,
) types.InboundInteraction {
	subject := subjectOf(registry, source, sender, metadata)
	return types.InboundInteraction{
		Sender: types.CanonicalIdentity{
			Handle: subject.Handle, ExternalID: subject.ExternalID,
			Platform: subject.Platform, DisplayName: subject.Name,
		},
		Body: body,
		// UNKNOWN when the source stamped nothing, which is the honest
		// answer for a tracker or a code host rather than a gap — see
		// notify.ChannelKindField.
		ChannelKind:   channelKind(metadata),
		RawEventType:  rawType,
		RequiresRecon: requiresRecon,
	}
}

// salientBody is the raw inbound message, never the enriched prompt.
//
// A NIL SalientBody falls back to Body and an EMPTY one does not: nil means
// this producer emits no distinct raw message, while empty means it set one
// and it was genuinely empty. Falling back on empty would hand every worker
// the same 1.5k of triage boilerplate and call it what the person said.
func salientBody(n *types.ExternalNotification) string {
	if n.SalientBody != nil {
		return *n.SalientBody
	}
	return n.Body
}

// channelKind reads the canonical surface shape a parser stamped.
//
// A value this build does not recognise reads as unknown rather than being
// passed through: the field is a closed set, and a consumer switching on it
// must never meet a member that arrived from a newer producer.
func channelKind(metadata map[string]string) types.ChannelKind {
	switch kind := types.ChannelKind(metadata[notify.ChannelKindField]); kind {
	case types.ChannelDM, types.ChannelGroup, types.ChannelPublic, types.ChannelInternal:
		return kind
	default:
		return types.ChannelUnknown
	}
}
