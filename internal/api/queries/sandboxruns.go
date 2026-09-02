package queries

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The sandbox-runs answer: the detached coding runs the engine still holds.
//
// THE LIVE PROJECTION IS THE WRONG SOURCE for this question, in two ways. It
// is in memory, so it starts empty after a restart; and it sweeps an entry
// after hours, while a run parked on a question can legitimately wait days for
// a person to answer. The states that most need somebody were therefore the
// ones least likely to be on screen — and a `reseed` run (its pause expired,
// its box reclaimed, its work safe on a pushed branch) had no surface at all.
// It looked exactly like work that had finished.
//
// The fleet's run record is the durable one, and this reads it — every node's
// runs, not this node's, because a run is recovered by whichever node owns its
// seat and a per-node read drew a board that disagreed with itself depending on
// which node answered.
//
// What it deliberately does NOT return is the execute state, the serialised
// Execute conversation. It is by far the largest column in the row, and every
// prompt in it is already reachable through the event store — shipping it to a
// board that renders one line per run would be a page-weight problem in
// exchange for nothing.

// PendingRuns is the durable record a sandbox-runs answer reads.
//
// Declared here rather than imported as a concrete store so this package
// depends on the shape, and so the memory twin answers it too.
type PendingRuns interface {
	ListActive(ctx context.Context) ([]sandbox.PendingRun, error)
}

func (s Sources) sandboxRuns(ctx context.Context, _ Params) (any, error) {
	runs, err := s.Sandbox.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, serialiseRun(run))
	}
	return map[string]any{"runs": out}, nil
}

func serialiseRun(run sandbox.PendingRun) map[string]any {
	return map[string]any{
		"turn_id":      run.TurnID,
		"agent_handle": run.AgentHandle,
		"role":         run.Role,
		"status":       run.Status,
		"coding_agent": run.CodingAgent,
		// WHERE the run is, which became an operator question the moment
		// providers.sandbox became a catalogue: one company now runs some
		// seats on the engine host and others in a remote box, and "is
		// this job on my machine" has no other surface. Empty on a row
		// written before the field existed.
		"placement":        run.Placement,
		"task_description": run.TaskDescription,
		"question":         run.Question,
		"audience":         run.Audience,
		"branch":           run.Branch,
		"trace_id":         run.TraceID,
		"owner":            run.Owner,
		// The two facts the board draws, rather than the ids themselves: a
		// non-empty sandbox id means a box exists, and a set paused_at
		// means it is currently held as a snapshot and being paid for.
		"box_exists":         run.SandboxID != "",
		"paused_at":          isoOrEmpty(run.PausedAt),
		"pause_ttl_seconds":  run.PauseTTLSeconds,
		"started_at":         isoOrEmpty(run.CreatedAt),
		"updated_at":         isoOrEmpty(run.UpdatedAt),
		"answerable_in_chat": answerableInChat(run.ConversationKey),
	}
}

// eventKeyPrefix marks a conversation key minted from an event id.
const eventKeyPrefix = "event:"

// answerableInChat reports whether a reply on a chat surface could ever reach
// this run.
//
// The resume path matches an inbound notification's conversation key against
// the one stored at kick-off, by exact string equality. A run started by
// anything OTHER than an external notification — a schedule tick, a task
// assignment, an A2A wake — stored a key derived from an event id, which no
// inbound message can reproduce. Such a run is not answerable through any chat
// surface, and telling somebody to "reply in the thread" would send them to a
// thread that does not exist.
func answerableInChat(conversationKey string) bool {
	return conversationKey != "" && !strings.HasPrefix(conversationKey, eventKeyPrefix)
}
