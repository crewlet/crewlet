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
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// SteerTurnTool is the tool's wire name.
const SteerTurnTool = "steer_turn"

// SteerAskBudget is how long steer_turn waits for the node running the turn to
// answer.
//
// PER ASK, and a note may take two — the probe that names the turn's seat and
// the offer — so a person waits at most twice this.
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
	// the same resolution every other operator write takes
	// ([PrincipalActor]), so a person bound to a seat sends AS that seat,
	// kind human, with their credential beside it — and carrying the
	// operation key the request's transport named as its work key
	// ([Actor.WorkKey]), which the note's id is. Required with Asker.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)
}

// steerTurn sends a note to a turn that is running now (internal/agent/steer).
//
// # Whose turn it is to steer
//
// The turn's SEAT's lead's, or the deployment's ([authz.ActionTurnSteer]) —
// never any reader's, since everybody signed in holds a credential. The seat
// is what the decision needs and the one thing this node cannot know: the
// turn runs wherever its seat is held. So the decision is taken in two steps.
// First on the turn as named, a record nobody has resolved yet
// ([authz.Object.Unresolved]): the deployment's grant admits there and then,
// and a caller who leads nobody is refused with exactly the answer they would
// get on a seat they do not lead — neither costs a round trip. A caller who
// leads somebody is told to resolve the record, and a PROBE asks the fleet
// whose turn it is without offering anything; the decision is taken again on
// that seat, and only then is the note offered. A turn's seat never changes,
// so the probe and the offer cannot disagree about whose turn it is.
//
// # What it promises
//
// `pending`: the node running the turn took the note, and the turn reads it at
// its next round boundary. What became of it is recorded there, as
// `agent_turn_steered` — `delivered` at the round that read it, or `expired`
// if the turn ended first. This node cannot see that far: the turn runs
// elsewhere, and the only thing that crossed back is the box's answer.
//
// `unknown`: no node answered the offer in time. The note may have been taken
// — an answer lost on its way back is indistinguishable from none — so sending
// it again is safe: the retry carries the same request id, and a box that took
// it once answers `accepted` without taking it twice.
//
// What it refuses is what the running node said: `not_running` for a turn that
// has ended, `conflict` for one already holding as many unread notes as it
// takes, `steer_unsupported` for one whose executor runs as a coding CLI's own
// loop — and, before asking anybody, `peer_upgrading` while any live node runs
// a build that cannot take a note at all. That last one is the fleet, not the
// seat: the note names a turn, and which node runs it is not known until one
// answers, so every node that could be the one has to be able to.
type steerTurn struct {
	deps      SteerDeps
	fleet     Fleet
	authorize Authorizer

	// org resolves the seat a probe names by its id, per call because a
	// config apply replaces the chart.
	org func() *org.Organization
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
		"their ids on the live view. Only the seat's lead, or whoever operates " +
		"the deployment, may steer its turn."
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
	if refusal := t.maySteer(ctx, turnID); refusal != nil {
		return *refusal, nil
	}

	// THE NOTE'S IDENTITY IS THE REQUEST'S, so a person's retry of one
	// request is one note on the turn. A request's operation key reaches a
	// tool as the actor's seed ([Actor.OperationSeed]): on the act route,
	// the `Idempotency-Key` the dashboard mints once per gesture and repeats
	// on every retry, scoped to the person who sent it (internal/api/opkey)
	// — the same value that route answers as `op_id`, so the note the
	// running node records in `agent_turn_steered` names the request the
	// person's surface holds.
	//
	// NOT [Actor.Operation], which is an operation a WORK tool binds for an
	// assistant's call ([WorkDeps.bindOperation]); this tool binds none, so
	// it is empty on every surface, and a note id read from it was a fresh
	// one on every retry — a note the turn would read, and act on, twice.
	// A call whose transport names no operation (an operator's own
	// assistant over MCP) is a new note every time, which is what it is.
	noteID := actor.OperationSeed()
	if noteID == "" {
		noteID = uuid.NewString()
	}
	reply, answered, refusal := t.ask(ctx, steer.Request{
		Version: steer.WireVersion, TurnID: turnID, NoteID: noteID, Note: note,
		By: actor.Handle, ByKind: iam.ActorKind(actor.Kind), OperatorID: actor.OperatorID,
	})
	if refusal != nil {
		return *refusal, nil
	}
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

// maySteer decides whether the caller may steer turnID, probing the fleet for
// the turn's seat only when the decision turns on it. Nil admits.
func (t *steerTurn) maySteer(ctx context.Context, turnID string) *tools.Result {
	if t.authorize == nil {
		refusal := authorityRefusal(SteerTurnTool, ErrNoAuthorizer)
		return &refusal
	}
	// THE TURN AS NAMED, which nobody has resolved to a seat — so its owner
	// is the one no login and no seat handle can equal ([unresolvedName]).
	// Not the turn's id: an id is lowercase hex and hyphens, which is a seat
	// handle's shape.
	err := t.authorize(ctx, authz.ActionTurnSteer, authz.Object{
		Kind: authz.KindPerson, ID: turnID, Owner: unresolvedName, Unresolved: true,
	})
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, authz.ErrUnresolved):
		refusal := authorityRefusal(SteerTurnTool, err)
		return &refusal
	}
	reply, answered, refusal := t.ask(ctx, steer.Request{
		Version: steer.WireVersion, TurnID: turnID, Probe: true,
	})
	switch {
	case refusal != nil:
		return refusal
	case !answered:
		// NOTHING WAS OFFERED, so this is not the offer's `unknown`: no
		// note is in flight to be uncertain about.
		return refusalOf(refused(tools.RefusalUnavailable, fmt.Sprintf("No node "+
			"answered for turn %s within %s: it is not running anywhere, or the node "+
			"running it did not answer in time. Nothing was sent; try again.",
			clip(turnID), SteerAskBudget)))
	}
	owner, named := t.seatOf(reply)
	if !named {
		return refusalOf(refused(tools.RefusalUnavailable, fmt.Sprintf("The node "+
			"running turn %s could not name the seat it runs for, so whether you may "+
			"steer it cannot be decided. Nothing was sent; try again shortly.",
			clip(turnID))))
	}
	return askAuthority(ctx, t.authorize, authz.ActionTurnSteer,
		authz.Object{Kind: authz.KindPerson, ID: turnID, Owner: owner})
}

// seatOf is the handle the authority is asked about for the seat a reply
// names: by its id through the running org, and the reply's own handle where
// this org no longer holds the seat — which nobody leads, so only the
// deployment's grant would have passed.
// False when the reply names no seat at all.
func (t *steerTurn) seatOf(reply steer.Reply) (string, bool) {
	id, err := uuid.Parse(reply.Agent)
	if err != nil || id == uuid.Nil {
		return "", false
	}
	if t.org != nil {
		if company := t.org(); company != nil {
			if seat := company.AgentSeatByID(id); seat != nil {
				return seat.Handle(), true
			}
		}
	}
	if reply.AgentHandle != "" {
		return reply.AgentHandle, true
	}
	return id.String(), true
}

// ask scatters one request and returns the reply that answers for its turn.
// answered is false when no node did within [SteerAskBudget]; a refusal is
// the ask itself failing, which sent nothing anywhere.
func (t *steerTurn) ask(ctx context.Context, req steer.Request) (steer.Reply, bool, *tools.Result) {
	request, err := json.Marshal(req)
	if err != nil {
		// A FAULT: the request is this build's own struct, so an encode
		// that fails fails the same way every time.
		return steer.Reply{}, false, refusalOf(faulted(ctx, SteerTurnTool, err,
			"steer_turn could not encode its request: "+faultSaid+". Nothing was sent."))
	}
	askCtx, cancel := context.WithTimeout(ctx, SteerAskBudget)
	defer cancel()
	replies, err := t.deps.Asker.Ask(askCtx, topics.SeatSteer, request, 1)
	if err != nil {
		// THE ASK COULD NOT BE MADE, so nothing was sent anywhere — which
		// is `unavailable` rather than `unknown`: there is no note in
		// flight to be uncertain about.
		return steer.Reply{}, false, refusalOf(nodeFailure(ctx, SteerTurnTool, err,
			"steer_turn could not reach the fleet", "Nothing was sent."))
	}
	reply, answered := steerReplyFor(replies, req.TurnID)
	return reply, answered, nil
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
