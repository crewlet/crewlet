package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
)

// What a turn is given before it starts.
//
// The prefetch is assembled here rather than inside the runner, and that
// placement IS the freeze: a runner handed strings has nowhere to re-fetch
// from, so a self_iterate loop cannot move the system prompt underneath the
// planner or cost a provider's prompt cache on every pass.

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
		// SummarizeEpisodes gates ONLY the episode summary. The Python
		// engine wired this switch by passing a nil provider pool, which
		// silently disabled the memory and knowledge filters too — an
		// operator turning off a summary got a company with no memory.
		SummarizeEpisodes: company.Config.Learning.Reflect.SummarizeEpisodes.Or(true),
	}
	if db := e.backends.Store; db != nil {
		src.Diary = learning.NewDiary(db)
		src.Episodes = learning.NewEpisodes(db)
		src.Counterparties = learning.NewCounterparties(db)
		src.Skills = learning.NewSkills(db)
		src.Onboarding = learning.NewOnboarding(db)
	}
	// Embed is left nil: this node has no embeddings client yet, so the
	// similarity half of the memory pool and the whole of episode recall
	// degrade — to recency-only and to an empty block respectively, both
	// of which the prefetch treats as first-class states rather than
	// failures. See providers.embeddings.
	return prefetch.New(src)
}

// prefetchFor renders one turn's context blocks.
//
// The SEAT and the AGENT ID are read off the pinned epoch, like everything
// else a turn is built from: a prefetch resolved against a revision the turn
// is not running would surface another company's memory.
func (e *Engine) prefetchFor(ctx context.Context, company *Company, req Request, task string) (prefetch.Request, prefetch.Blocks) {
	seat := company.Org.AgentSeatByHandle(req.Handle)
	if seat == nil {
		return prefetch.Request{}, prefetch.Blocks{}
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
	return r, e.prefetcher(company).Fetch(ctx, r)
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
