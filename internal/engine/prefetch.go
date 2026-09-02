package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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
// Per CALL rather than held, because two of its sources move: the knowledge
// searcher is rebuilt whenever the tracker is reconciled, and a node with no
// store has none of the rest. Building it is assembling six interface values
// — cheaper than the mutex a cached one would need.
func (e *Engine) prefetcher(company *Company) *prefetch.Fetcher {
	src := prefetch.Sources{
		Knowledge: e.Knowledge(),
		Models:    company.Models,
		// SummarizeEpisodes gates ONLY the episode summary. Wiring this
		// switch by passing a nil provider pool silently disables the
		// memory and knowledge filters too — an operator turning off a
		// summary gets a company with no memory.
		SummarizeEpisodes: company.Config.Learning.Reflect.SummarizeEpisodes.Or(true),
	}
	if db := e.backends.Store; db != nil {
		src.Diary = learning.NewDiary(db)
		src.Episodes = learning.NewEpisodes(db)
		src.Counterparties = learning.NewCounterparties(db)
		src.Skills = learning.NewSkills(db)
		src.Onboarding = learning.NewOnboarding(db)
	}
	// Nil where a company configured no embeddings, which degrades the
	// similarity half of the memory pool to recency alone and episode
	// recall to an empty block — both first-class states in the prefetch
	// rather than failures.
	src.Embed = e.embedder()
	return prefetch.New(src)
}

// prefetchFor renders one turn's context blocks.
//
// The SEAT and the AGENT ID are read off the pinned epoch, like everything
// else a turn is built from: a prefetch resolved against a revision the turn
// is not running would surface another company's memory.
func (e *Engine) prefetchFor(ctx context.Context, company *Company, req Request, task string) prefetch.Blocks {
	seat := company.Org.AgentSeatByHandle(req.Handle)
	if seat == nil {
		return prefetch.Blocks{}
	}
	agentID, _ := company.Org.AgentIDFor(seat)
	r := prefetch.Request{
		Seat: seat, AgentID: agentID.String(), Org: company.Org,
		Task: task, TurnID: req.WorkKey,
		Senders: sendersOf(e.Registry(), req.Events),
		// A POINTER TRIGGER gates the three searches that judge relevance
		// against the trigger text. Read off the events rather than
		// recomputed, because the parser that produced them is the only
		// thing that knows whether its vendor's body is the context or a
		// reference to it.
		RequiresRecon: requiresRecon(req.Events),
	}
	blocks := e.prefetcher(company).Fetch(ctx, r)
	e.publishPrefetchSummary(ctx, seat, agentID.String(), req.WorkKey, r, blocks)
	return blocks
}

// publishPrefetchSummary reports what each block actually surfaced.
//
// THE ONLY SIGNAL THIS PIPELINE HAS. Every block degrades to empty rather
// than failing (see internal/agent/prefetch), so a seat whose diary is
// unreachable, whose auxiliary model is misconfigured and whose knowledge
// base has nothing to say all produce the same prompt — and nothing else
// distinguishes them. Per-block hit and rendered size, once per turn, is what
// lets an operator see the difference.
//
// Best effort, and deliberately so: this is measurement, and a turn must not
// fail because its telemetry could not be published.
func (e *Engine) publishPrefetchSummary(ctx context.Context, seat *org.Role,
	agentID, turnID string, r prefetch.Request, b prefetch.Blocks,
) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	ev := events.NewFrom(types.PrefetchSummary{
		Agent: agentID, AgentHandle: seat.Handle(), RoleName: seat.Name,
		TurnID:                 turnID,
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
		RelevantKnowledgeSelectionCount: b.RelevantKnowledgeHits,
		TriggerRequiresRecon:            r.RequiresRecon,
	}, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	e.publishEvent(ctx, ev, seat.Name)
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
func subjectsOf(registry *notify.Registry, ev *types.ExternalNotification) []learning.Subject {
	out := make([]learning.Subject, 0, len(ev.Messages)+1)
	out = append(out, subjectOf(registry, ev.NotificationSource,
		ev.Sender, ev.Metadata))
	for _, m := range ev.Messages {
		out = append(out, subjectOf(registry, ev.NotificationSource,
			m.Sender, m.Metadata))
	}
	return out
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
