package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/notify"
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
//
// ONE METHOD AND NO STATUS FILTER, because there is nothing left to filter:
// a run's record is deleted the moment the run settles, so [sandbox.Active] is
// every status a record can hold and "every run this store holds" and "the
// active ones" name one set. A `status=` parameter selecting `done` or
// `failed` would be a question whose answer is structurally empty — worse than
// absent, because an empty board reads as "nothing failed" rather than "this
// is not where that is recorded". How a run ENDED is on the event stream: the
// resumed turn's own events, or a `sandbox_run_failed` naming the reason.
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
	// HELD, NOT STAMPED. A box parked on a question is being paid for
	// whether or not the pause instant reached the row, and drawing the raw
	// stamp showed exactly that box as a live one — the one reading that
	// says nobody is being billed. It is the same answer the pause reaper
	// acts on ([sandbox.PendingRun.HeldSince]), so the screen and the
	// reclaim cannot disagree about which boxes are held.
	heldSince, _ := run.HeldSince()
	return map[string]any{
		"turn_id": run.TurnID,
		// The unit of work behind that run, so a board row links back to
		// the trigger rather than only to the one execution that detached.
		// THROUGH THE ACCESSOR, because nothing rewrites a parked row: a
		// run suspended before the identities were split carries the key
		// in its turn id instead. Empty when the run genuinely has none.
		"work_key":     run.UnitOfWork(),
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
		"paused_at":          isoOrEmpty(heldSince),
		"pause_ttl_seconds":  run.PauseTTLSeconds,
		"started_at":         isoOrEmpty(run.CreatedAt),
		"updated_at":         isoOrEmpty(run.UpdatedAt),
		"answerable_in_chat": answerableInChat(run.Conversation()),
		// WHO IS WAITING, which is the question a board full of parked
		// runs exists to answer and had no field for. Persisted rather
		// than re-derived precisely because the resumed turn does not see
		// its trigger — empty means nobody is waiting, which is the safe
		// half.
		"reply": run.Reply,
		// THE BRIDGE'S OWN TOOL LOG. A native loop keeps its calls in
		// memory and the turn writes them at the end; a bridged run's are
		// made by a process outside the engine, minutes or hours apart
		// and possibly across a restart, so this row is the only copy
		// there is. `bridge_calls_elided` says how many were cut from the
		// middle, because a log that silently skips is a log that lies
		// about what the run did.
		"bridge_calls":        run.BridgeCalls,
		"bridge_calls_elided": run.BridgeCallsElided,
		// THE THREE IDENTIFIERS a person needs to find this run in
		// somebody else's system: the coding CLI's own session, the
		// command the engine launched, and the chain of asks that led
		// here.
		"session_id":       run.SessionID,
		"command_id":       run.CommandID,
		"delegation_chain": run.DelegationChain,
	}
}

// answerableInChat reports whether a reply on a chat surface could ever reach
// this run.
//
// A run started by anything OTHER than an external notification — a schedule
// tick, a task assignment, an A2A wake — stored NOTHING: its trigger names
// neither key, [notify.ConversationIdentityOfAll] answers "" for a partition
// like that rather than inventing one, and [notify.Stamp] declines an empty
// value, so both of the row's conversation columns are empty. Such a run is
// not answerable through any chat surface, and telling somebody to "reply in
// the thread" would send them to a thread that does not exist.
//
// The per-event `event:` fallback is a different thing in a different place:
// [notify.KeyOf] mints it at READ time, for the broker's partition function,
// which has to give an event that names no conversation a partition of its
// own. It is not written onto a row from here. [notify.Derived] refuses it as
// well as the empty string, which is what keeps this column honest for a row
// whose key was minted that way by somebody else — a peer, or a writer this
// package does not know about.
//
// ASKED OF THE CONVERSATION the run reports back to and is answered on, never
// of the partition beside it. The question a person reading this board has is
// "is there a thread I can answer in", which is a property of the durable
// conversation; the partition is an inbox-grouping artefact with no surface
// anybody could reply to. The two are derivable or not TOGETHER on every
// source — a chat event naming a channel yields both, one naming none yields
// neither — so the verdict is the same either way; what this states is that
// the column answers the same question the resume path does.
//
// [notify.Derived] answers exactly that, and this had its own copy of the
// prefix to answer it with — so a rename of the fallback's namespace would
// have left this route confidently offering a thread that does not exist.
func answerableInChat(conversation string) bool {
	return notify.Derived(conversation)
}
