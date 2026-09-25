package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// SteerTurnTool is the tool's wire name.
const SteerTurnTool = "steer_turn"

// SteerAskBudget is how long steer_turn waits for the node running the turn to
// answer.
//
// TWO SECONDS: one broker round trip, which on a healthy fleet is
// milliseconds, plus room for a node mid-GC or mid-heartbeat. The same budget
// the fleet's history scatter answers inside, for the same reason — a person
// is waiting on a button — and a too-tight value is visible rather than
// silent: every note it cuts short comes back `unknown`, never as a false
// refusal. UNMEASURED on a large fleet under load; if `unknown` answers appear
// on a healthy one, this is the number to raise.
const SteerAskBudget = 2 * time.Second

// FleetAsker scatters one request on the queue's ephemeral verb — see
// [queue.EventQueue.Ask].
type FleetAsker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// SteerDeps are steer_turn's dependencies.
type SteerDeps struct {
	// Asker reaches every node, one of which runs the turn. Nil omits the
	// tool.
	Asker FleetAsker

	// Actor is who is sending the note, off the caller's own credential —
	// the same resolution every other operator write takes. Required with
	// Asker.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)
}

// steerTurn sends a note to a turn that is running now (internal/agent/steer).
//
// # What it promises
//
// `pending`: the node running the turn took the note, and the turn reads it at
// its next round boundary. What became of it is recorded there, as
// `agent_turn_steered` — `delivered` at the round that read it, or `expired`
// if the turn ended first. This node cannot see that far: the turn runs
// elsewhere, and the only thing that crossed back is the box's answer.
//
// `unknown`: no node answered in time. The note may have been taken — an
// answer lost on its way back is indistinguishable from none — so sending it
// again is safe: the retry carries the same request id, and a box that took it
// once answers `accepted` without taking it twice.
//
// What it refuses is what the running node said: `not_running` for a turn that
// has ended, `conflict` for one already holding as many unread notes as it
// takes, `steer_unsupported` for one whose executor runs as a coding CLI's own
// loop — and, before asking anybody, `peer_upgrading` while any live node runs
// a build that cannot take a note at all. That last one is the fleet, not the
// seat: the note names a turn, and which node runs it is not known until one
// answers, so every node that could be the one has to be able to.
type steerTurn struct {
	deps  SteerDeps
	fleet Fleet
}

var _ tools.SeatCallable = (*steerTurn)(nil)

func (t *steerTurn) Name() string { return SteerTurnTool }

func (t *steerTurn) Description() string {
	return "Send a short note to an agent's turn while it is running. The turn " +
		"reads it at its next round — after the tool call in flight returns — " +
		"as a correction or addition to the work it has in hand, and keeps to it " +
		"for the rest of the turn. Use it to redirect work you can see going the " +
		"wrong way; for anything longer than a paragraph, or for work the turn " +
		"has not started, write on the work item instead. Find running turns and " +
		"their ids on the live view."
}

func (t *steerTurn) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"turn_id": map[string]any{
				"type":        "string",
				"description": "The running turn's id — its `turn_id` on the live view.",
			},
			"note": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("What the turn should take into account, "+
					"as its agent should read it. At most %d characters.", steer.MaxNoteRunes),
			},
		},
		"required": []any{"turn_id", "note"},
	}
}

func (t *steerTurn) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *steerTurn) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if t.deps.Asker == nil || t.deps.Actor == nil {
		return refused(tools.RefusalUnavailable, SteerTurnTool+" is unavailable: "+
			"this surface was built without a way to reach the fleet."), nil
	}
	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return refused(tools.RefusalForbidden, SteerTurnTool+
			" sends a note as the person whose credential calls it, and this call carries none."), nil
	}
	turnID := strings.TrimSpace(argString(args, "turn_id"))
	if turnID == "" {
		return failed("steer_turn needs a `turn_id` — the running turn's id."), nil
	}
	note := strings.TrimSpace(argString(args, "note"))
	if invalid := steer.Validate(note); invalid != nil {
		var long *steer.TooLongError
		if errors.As(invalid, &long) {
			return failed(fmt.Sprintf("That `note` is %d characters and a note may be "+
				"at most %d: a turn reads it between two rounds, beside its task. "+
				"Put the detail on the work item and send a note saying where it is.",
				long.Runes, steer.MaxNoteRunes)), nil
		}
		return failed("steer_turn needs a `note` — what the running turn should take into account."), nil
	}
	if refusal := fleetCanCarry(ctx, t.fleet, SteerTurnTool, coord.FeatureSteer); refusal != nil {
		return *refusal, nil
	}

	// THE NOTE'S IDENTITY IS THE REQUEST'S, so a person's retry of one
	// request is one note on the turn — see [Actor.OperationSeed]. A call
	// that names no request (an operator's own assistant over MCP) is a new
	// note every time, which is what it is.
	noteID := actor.OperationSeed()
	if noteID == "" {
		noteID = uuid.NewString()
	}
	request, err := json.Marshal(steer.Request{
		Version: steer.WireVersion, TurnID: turnID, NoteID: noteID, Note: note,
		By: actor.Handle, BySeat: actor.Seat,
	})
	if err != nil {
		return tools.Result{}, fmt.Errorf("builtin: encode a note: %w", err)
	}
	askCtx, cancel := context.WithTimeout(ctx, SteerAskBudget)
	defer cancel()
	replies, err := t.deps.Asker.Ask(askCtx, topics.SeatSteer, request, 1)
	if err != nil {
		// THE ASK COULD NOT BE MADE, so nothing was sent anywhere — which
		// is `unavailable` rather than `unknown`: there is no note in
		// flight to be uncertain about.
		return refused(tools.RefusalUnavailable, fmt.Sprintf("steer_turn could not "+
			"reach the fleet (%v). Nothing was sent; try again.", err)), nil
	}
	reply, answered := steerReplyFor(replies, turnID)
	if !answered {
		log.WarnContext(ctx, "steer_turn_unanswered",
			"turn_id", turnID, "note_id", noteID, "budget", SteerAskBudget.String())
		return jsonResult(map[string]any{
			"turn_id": turnID, "note_id": noteID, "outcome": string(statelog.OutcomeUnknown),
		})
	}
	switch reply.Status {
	case steer.StatusAccepted:
		return jsonResult(map[string]any{
			"turn_id": turnID, "note_id": noteID, "agent_handle": reply.AgentHandle,
			"outcome": string(statelog.OutcomePending),
		})
	case steer.StatusClosed:
		return refused(tools.RefusalNotRunning, fmt.Sprintf("Turn %s is not running: "+
			"it has ended or parked, so no later round will read a note. Nothing was sent.",
			clip(turnID))), nil
	case steer.StatusFull:
		return refused(tools.RefusalConflict, fmt.Sprintf("Turn %s already holds %d "+
			"notes it has not read yet. Nothing was sent: once it has read them — at "+
			"its next round — this note may be sent again.", clip(turnID), steer.MaxPendingNotes)), nil
	case steer.StatusUnsupported:
		return refused(tools.RefusalSteerUnsupported, fmt.Sprintf("Turn %s cannot "+
			"take a note: %s's executor runs as a coding CLI's own agentic loop, "+
			"whose rounds the engine does not drive. Nothing was sent.",
			clip(turnID), reply.AgentHandle)), nil
	}
	// A STATUS THIS BUILD DOES NOT KNOW, from a newer peer. Whether the note
	// was taken is exactly what this node cannot tell.
	return jsonResult(map[string]any{
		"turn_id": turnID, "note_id": noteID, "agent_handle": reply.AgentHandle,
		"outcome": string(statelog.OutcomeUnknown),
	})
}

// steerReplyFor is the reply that answers for turnID, if any did. A reply this
// build cannot read, or one about another turn, is a node that did not answer.
func steerReplyFor(replies [][]byte, turnID string) (steer.Reply, bool) {
	for _, raw := range replies {
		var reply steer.Reply
		if err := json.Unmarshal(raw, &reply); err != nil || reply.TurnID != turnID {
			continue
		}
		return reply, true
	}
	return steer.Reply{}, false
}
