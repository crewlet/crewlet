package builtin

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// Telemetry is where a builtin's own lifecycle events go: a one-method
// consumer interface, satisfied by the engine's queue.
//
// A skill sends one event per LOAD, not the per-offer stamp
// internal/learning keeps silent: that fires for every skill a prompt merely
// listed and would put the catalogue's size into the event stream. The
// knowledge tools (get_page, search_knowledge, load_tool_skill) publish a
// `knowledge_read` through it too, naming the pages a call reached.
type Telemetry interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// note publishes one payload for a turn, best effort: every caller has
// already done what the event describes, so failing the tool call on a
// publish would take a working result from the model over a telemetry
// problem it cannot act on.
func note(ctx context.Context, out Telemetry, turn *turnctx.Turn, payload events.Payload) {
	if out == nil || payload == nil {
		return
	}
	ev := events.NewFrom(payload, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	// The SEAT is the source: the activity feed groups on it, and an event
	// attributed to the engine would sit outside its turn.
	ev.Source = turn.Handle()
	if err := out.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "builtin_telemetry_not_published",
			"type", ev.Type, "seat", turn.Handle(), "error", err)
	}
}

// skillUsed builds the load event, or nil when the turn cannot identify its
// seat (a tool surface built outside a turn, not a failure). page is the
// knowledge-base page a registry skill was read from; zero for a synthesized
// skill.
func skillUsed(turn *turnctx.Turn, name, skillID string,
	kind types.SkillSourceKind, page types.KnowledgeReadPage,
) events.Payload {
	agentID, why := seatAgentID(turn)
	if why != "" {
		return nil
	}
	return types.SkillUsed{
		Agent: agentID, AgentHandle: turn.Handle(), RoleName: turn.Role(),
		TurnID: turn.RunID, WorkKey: turn.WorkKey,
		SkillName: name, SkillID: skillID,
		SourceKind:   kind,
		SourcePageID: page.ID, SourceContainer: page.Container,
	}
}

// knowledgeRead builds the read event, or nil when it is not a seat's read: a
// turn with no seat is a surface built outside a turn (the operator's, whose
// reads its own record audits), and a read that reached no page is no read
// ([types.KnowledgeRead]). A page with no id is dropped, since nothing could
// count it against its page.
func knowledgeRead(turn *turnctx.Turn, via types.KnowledgeReadVia, backend, query string,
	pages []types.KnowledgeReadPage,
) events.Payload {
	agentID, why := seatAgentID(turn)
	if why != "" {
		return nil
	}
	kept := make([]types.KnowledgeReadPage, 0, len(pages))
	for _, page := range pages {
		if page.ID != "" {
			kept = append(kept, page)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return types.KnowledgeRead{
		Agent: agentID, AgentHandle: turn.Handle(), RoleName: turn.Role(),
		TurnID: turn.RunID, WorkKey: turn.WorkKey, Phase: turn.Phase,
		Via: via, Backend: backend, Query: types.KnowledgeReadQuery(query),
		Pages: kept,
	}
}

// Recaller is the turn-start prefetch's own semantic search, reachable as a tool.
//
// Satisfied by *prefetch.Fetcher, declared as just these two methods so a
// builtin reaches the search but cannot render a block. It is deliberately the
// push side's implementation: a second one would drift from the block the
// model was shown at turn start.
type Recaller interface {
	// RecallEpisodes returns past turns similar to text, most similar
	// first, and how many of the seat's turns it could not search. An error
	// means the search could not run (not "none"): wrapping
	// [learning.ErrNoEmbeddings] is a company with no embeddings, anything
	// else a failed search. An embedder's failure keeps its class through
	// the wrap ([embeddings.ErrConfiguration], [embeddings.ErrRefused],
	// [embeddings.ErrTransient]), which says whether asking again can help.
	RecallEpisodes(ctx context.Context, seat *org.Role, text string, limit int) (learning.EpisodeSearch, error)

	// RecallMemories re-runs the personal-memory relevance filter against a
	// hint, returning what it picked. senders (who triggered the turn) feed
	// the filter's per-subject rule; aux is the attribution its model call is
	// charged under ([turnctx.Turn.Aux]).
	RecallMemories(ctx context.Context, seat *org.Role, agentID, hint string,
		senders []learning.Subject, aux auxspend.Use) ([]learning.DiaryEntry, error)
}
