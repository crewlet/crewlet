package builtin

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// Telemetry is where a builtin's own lifecycle events go.
//
// A ONE-METHOD consumer interface, satisfied by the engine's queue. It exists
// because the skill lifecycle's events were registered, given topics, given
// summaries and categorised — and NOTHING anywhere constructed them. `use_skill`
// bumped a counter, `refine_skill` wrote a row, and neither said so, so
// "is the skill a seat synthesized ever loaded again" — the one question skill
// induction has to answer to be worth its cost — was answerable only by diffing
// a database column.
//
// It is NOT the per-offer stamp internal/learning deliberately keeps silent.
// That one fires once per turn per seat for every skill the prompt merely
// listed, and publishing it would put the catalogue's size into the event
// stream. This is one event per LOAD, which is the measurement.
//
// The knowledge tools report through it too — get_page, search_knowledge and
// load_tool_skill each publish a `knowledge_read` naming the pages the call
// reached — for the same reason: which pages a company's seats actually open
// was otherwise answerable only by reading transcripts.
type Telemetry interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// note publishes one payload for a turn, and swallows what goes wrong.
//
// BEST EFFORT, always. Every caller has already done the thing the event
// describes — loaded a skill, stored a refinement, read a page — so failing
// the tool call on a publish would take a working result away from the model
// to report a telemetry problem it cannot act on.
func note(ctx context.Context, out Telemetry, turn *turnctx.Turn, payload events.Payload) {
	if out == nil || payload == nil {
		return
	}
	ev := events.NewFrom(payload, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	// The SEAT is the source, matching every other seat-scoped event: the
	// activity feed groups on it, and a load or a read attributed to the engine
	// would sit outside the turn it belongs to.
	ev.Source = turn.Handle()
	if err := out.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "builtin_telemetry_not_published",
			"type", ev.Type, "seat", turn.Handle(), "error", err)
	}
}

// skillUsed builds the load event, or nil when the turn cannot identify its
// seat — which is a tool surface built outside a turn, not a failure.
//
// page names the knowledge-base page a registry skill was read from, and is
// the zero value for a synthesized skill, which has none.
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

// knowledgeRead builds the read event, or nil when there is nothing to record.
//
// NIL FOR TWO REASONS, both "not a read by a seat". A turn that cannot identify
// its seat is a surface built outside a turn — the operator's, above all, where
// a person reading a page is audited by the operator surface's own record and
// is not a seat's read. And a read that reached no page is the absence of a
// read (see [types.KnowledgeRead]). A page with no id is dropped rather than
// recorded, because nothing could ever count it against the page it was.
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
// Satisfied by *prefetch.Fetcher, and declared here as the two methods this
// package needs rather than imported as that type: a builtin that could reach
// the whole prefetch could render a block, and the seam is the search.
//
// THE SAME implementation the push side uses, deliberately. A second answer to
// "which of this seat's memories bear on this text" would drift from the block
// the model was shown at turn start, in the direction nobody looks.
type Recaller interface {
	// RecallEpisodes returns past turns similar to text. An error means the
	// search could not run — a deployment with no embeddings, or a store
	// that could not be read — which is a different answer from none.
	RecallEpisodes(ctx context.Context, seat *org.Role, text string, limit int) ([]learning.Hit, error)

	// RecallMemories re-runs the personal-memory relevance filter against a
	// hint, returning what it picked.
	RecallMemories(ctx context.Context, seat *org.Role, agentID, hint string) ([]learning.DiaryEntry, error)
}
