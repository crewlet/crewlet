package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
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
	// Asker reaches every node, one of which runs a turn that has not
	// launched its executor yet. Nil omits the tool.
	Asker FleetAsker

	// Wire signs every request the tool scatters and verifies every reply,
	// under the fleet keyring ([steer.Channel], internal/signedask) — the
	// node's own, which its desk answers with. Required with Asker: the
	// running node offers a note as the person it names on this node's
	// decision that they may, so a note it could not verify would be one
	// anybody on the broker could have written, and a reply this node did
	// not verify could name whichever seat the caller leads.
	Wire *signedask.Channel[steer.Request, steer.Reply]

	// Runs is where a turn's notes wait once it has launched its executor:
	// the run's record, which every node reads ([RunSteers]). Required with
	// Asker — a surface that asked only the scatter would send every note to
	// a turn whose run is out to nobody.
	Runs RunSteers

	// Actor is who is sending the note, off the caller's own credential —
	// the same resolution every other operator write takes
	// ([PrincipalActor]), so a person bound to a seat sends AS that seat,
	// kind human, with their credential beside it — and carrying the
	// operation key the request's transport named as its work key
	// ([Actor.WorkKey]), which the note's id is. Required with Asker.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)
}

// RunSteers is where a turn's notes wait from the launch of its executor to
// the turn's end: the run's own record in the coordination store, sealed
// (internal/engine, runsteer.go, and ADR-0067). Declared here, by the consumer.
type RunSteers interface {
	// Seat is the seat the turn's run serves, as the record's sealed anchor
	// names it. Found false where the turn has no run record — it has not
	// launched one, or it has ended. An error is a record that could not be
	// read, which decides nothing.
	Seat(ctx context.Context, turnID string) (RunSeat, bool, error)

	// File files the note on the turn's run record and answers what became
	// of it. An error is a filing that may not have landed; a retry is the
	// same note, so it is never taken twice.
	File(ctx context.Context, turnID string, note steer.Note) (RunFiled, error)
}

// RunSeat is the seat a turn's run record names: its derived agent id, which a
// person's authority is decided on, and its handle, a label.
type RunSeat struct {
	Agent  string
	Handle string

	// Unverified is set, and the seat empty, where the anchor names no seat
	// anybody can vouch for: [steer.ErrSeatUnnamed], [steer.ErrSeatUnverified],
	// or [steer.ErrSeatKeyUnknown] for one sealed under a key this node does
	// not hold. A VERDICT, NEVER A SENTENCE: the tool words it for each of its
	// readers ([unopenedRefusal]), and the record's reader writes none. No
	// authority is decided on a seat nobody can vouch for.
	Unverified error
}

// RunFiled is what became of a note filed on a turn's run record.
type RunFiled struct {
	Status      steer.Status
	Reads       steer.Reads
	AgentHandle string
}

// steerTurn sends a note to a turn that is running now (internal/agent/steer).
//
// # Where the note goes
//
// A turn's notes wait in one of two places, one after the other. Once the turn
// has launched its executor, on the RUN'S RECORD, which every node reads: the
// tool reads it first, and files the note there itself. Before that, in the box
// the turn's first segment holds on the node running it, which nothing here can
// read — so the note is SCATTERED to the fleet ([steer.Request]), and the node
// running the turn answers. A note that read "no record" a moment before the
// launch wrote one still reaches the turn: the node that held the box filed
// what it held on the record as the run launched, and files a late offer there
// too.
//
// # Whose turn it is to steer
//
// The turn's SEAT's lead's, or an admin's ([authz.ActionTurnSteer]) — never
// any reader's, since everybody signed in reads the overview. The decision is
// taken first on the turn as named, a record nobody has resolved yet
// ([authz.Object.Unresolved]): admin admits there and then, and a caller who
// leads nobody is refused with exactly the answer they would get on a seat they
// do not lead. A caller who leads somebody is told to resolve the record, and
// the seat is read: off the run record's SEALED ANCHOR where the turn has one —
// written by the launch, so nobody who can rewrite the record in the clear can
// move a person's authority onto another seat — and from a PROBE of the fleet
// where it has none, which offers nothing. The decision is taken again on that
// seat, and only then is the note sent. A turn's seat never changes, so the
// read and the filing cannot disagree about whose turn it is.
//
// # What it promises
//
// `pending`: the turn took the note, and `reads` says when it will be read —
// `next_step`, at the turn's next step, or `after_answer`, once the person its
// executor asked has answered and whatever resumes the turn reads it. What
// became of it is recorded where the turn reads it, as `agent_turn_steered` —
// `delivered` where it was read, or `expired` if the turn ended first. This node
// sees no further.
//
// `unknown`: no node answered a scattered offer in time. The note may have been
// taken — an answer lost on its way back is indistinguishable from none — so
// sending it again is safe: the retry carries the same request id, and a turn
// that took it once answers `accepted` without taking it twice.
//
// What it refuses is what the turn said: `not_running` for a turn that has
// ended, `notes_full` for one already holding as many unread notes as may wait,
// and `notes_spent` for one that has taken as many notes as a turn takes. Where
// the run's record, or the node running the turn, could not be read or written,
// it says what stopped it ([nodeFailure]): a condition of the node or the fleet
// is `unavailable`, with the wait that clears it, and a fault of this node's own
// is `internal_error`, which no wait clears.
type steerTurn struct {
	deps      SteerDeps
	authorize Authorizer

	// org resolves the seat a record or a probe names by its id, per call
	// because a config apply replaces the chart.
	org func() *org.Organization
}

var _ tools.SeatCallable = (*steerTurn)(nil)

func (t *steerTurn) Name() string { return SteerTurnTool }

func (t *steerTurn) Description() string {
	return "Send a short note to an agent's turn while it is running. The turn " +
		"reads it at its next step, as a correction or addition to the work it " +
		"has in hand, and keeps to it for the rest of the turn: a note sent while " +
		"the turn's executor works in its box reaches it between its steps, and is read by " +
		"the review that judges its work and by every launch the turn makes after it; one " +
		"sent while the executor waits on a person's answer is read once the turn resumes. " +
		"Use it to " +
		"redirect work you can see going the wrong way; for anything longer than " +
		"a paragraph, or for work the turn has not started, write on the work " +
		"item instead. Find running turns and their ids on the live view. Only " +
		"the seat's lead, or an admin, may steer its turn."
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

	if t.deps.Asker == nil || t.deps.Actor == nil || t.deps.Wire == nil || t.deps.Runs == nil {
		return refusedBy(tools.RefusalUnavailable, waitless(), SteerTurnTool+
			" is unavailable: this surface was built without a way to reach the fleet."), nil
	}
	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return refused(tools.RefusalForbidden, SteerTurnTool+
			" sends a note as the person whose credential calls it, and this call carries none."), nil
	}
	turnID := strings.TrimSpace(argString(args, "turn_id"))
	if turnID == "" {
		return failed(SteerTurnTool + " needs a `turn_id` — the running turn's id."), nil
	}
	note := strings.TrimSpace(argString(args, "note"))
	if invalid := steer.Validate(note); invalid != nil {
		var long *steer.TooLongError
		if errors.As(invalid, &long) {
			return failed(fmt.Sprintf("That `note` is %d characters and a note may be "+
				"at most %d: a turn reads it between two steps, beside its task. "+
				"Put the detail on the work item and send a note saying where it is.",
				long.Runes, steer.MaxNoteRunes)), nil
		}
		return failed(SteerTurnTool + " needs a `note` — what the running turn should take into account."), nil
	}
	// THE RUN'S RECORD FIRST: where it exists it is where the note goes, and
	// the seat it names is the one authority is decided on.
	seat, recorded, refusal := t.runSeat(ctx, turnID)
	if refusal != nil {
		return *refusal, nil
	}
	if denied := t.maySteer(ctx, turnID, seat, recorded); denied != nil {
		return *denied, nil
	}

	// THE NOTE'S IDENTITY IS THE REQUEST'S, so a person's retry of one
	// request is one note on the turn. A request's operation key reaches a
	// tool as the actor's seed ([Actor.OperationSeed]): on the act route,
	// the `Idempotency-Key` the dashboard mints once per gesture and repeats
	// on every retry, scoped to the person who sent it (internal/api/opkey)
	// — the same value that route answers as `op_id`, so the note the turn
	// records in `agent_turn_steered` names the request the person's surface
	// holds.
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
	if recorded {
		return t.file(ctx, turnID, steer.Note{
			ID: noteID, Text: note, By: actor.Handle, ByKind: iam.ActorKind(actor.Kind),
			OperatorID: actor.OperatorID,
		})
	}
	reply, answered, refusal := t.ask(ctx, steer.Request{
		Version: steer.WireVersion, TurnID: turnID, NoteID: noteID, Note: note,
		By: actor.Handle, ByKind: iam.ActorKind(actor.Kind), OperatorID: actor.OperatorID,
	})
	if refusal != nil {
		return *refusal, nil
	}
	if answered && reply.Unverified != nil {
		return unverifiedRefusal(ctx, turnID, reply), nil
	}
	if !answered {
		log.WarnContext(ctx, "steer_turn_unanswered",
			"turn_id", turnID, "note_id", noteID, "budget", SteerAskBudget.String())
		return jsonResult(map[string]any{
			"turn_id": turnID, "note_id": noteID, "outcome": string(statelog.OutcomeUnknown),
		})
	}
	reads := reply.Reads
	if reads == "" {
		reads = steer.ReadsNextStep
	}
	return t.answered(turnID, noteID, reply.Status, reads, reply.AgentHandle)
}

// file files a note on the turn's run record — see [RunSteers.File].
func (t *steerTurn) file(ctx context.Context, turnID string, note steer.Note) (tools.Result, error) {
	fileCtx, cancel := context.WithTimeout(ctx, SteerAskBudget)
	defer cancel()
	filed, err := t.deps.Runs.File(fileCtx, turnID, note)
	if err != nil {
		// A CONDITION IS WRITTEN HERE, with the turn and the note, since
		// [nodeFailure] logs only a fault: a store that did not answer may
		// have taken the note all the same ([RunSteers.File]), and the note's
		// id is what tells a later delivery of it from a second note.
		if _, condition := Condition(err); condition {
			log.WarnContext(ctx, "steer_turn_unfiled", "turn_id", turnID, "note_id", note.ID,
				"error", err.Error())
		}
		return nodeFailure(ctx, SteerTurnTool, err, fmt.Sprintf("Could not file the note on "+
			"the record of turn %s's executor", clip(turnID)), ""), nil
	}
	return t.answered(turnID, note.ID, filed.Status, filed.Reads, filed.AgentHandle)
}

// answered is what the turn said of the note, each in the class the person's
// surface acts on — a note accepted, refused or answered with a status this
// build does not know.
func (t *steerTurn) answered(turnID, noteID string, status steer.Status, reads steer.Reads,
	handle string,
) (tools.Result, error) {
	switch status {
	case steer.StatusAccepted:
		return jsonResult(map[string]any{
			"turn_id": turnID, "note_id": noteID, "agent_handle": handle,
			"outcome": string(statelog.OutcomePending), "reads": string(reads),
		})
	case steer.StatusClosed:
		return refused(tools.RefusalNotRunning, fmt.Sprintf("Turn %s is not running: "+
			"it has ended, so nothing will read a note. Nothing was sent.",
			clip(turnID))), nil
	case steer.StatusFull:
		// THE TURN'S STATE, which clears on its own: it reads its notes
		// at its next step.
		return refused(tools.RefusalNotesFull, fmt.Sprintf("This turn already has %d "+
			"notes it hasn't read yet, so nothing was sent. Send it again once it "+
			"reads them, at its next step.", steer.MaxPendingNotes)), nil
	case steer.StatusSpent:
		return refused(tools.RefusalNotesSpent, fmt.Sprintf("Turn %s has been sent the %d "+
			"notes a turn takes. Nothing was sent: put the rest on the work item, where "+
			"this turn and every later one can read it.", clip(turnID), steer.MaxTurnNotes)), nil
	}
	// A STATUS THIS BUILD DOES NOT KNOW, from a newer peer. Whether the note
	// was taken is exactly what this node cannot tell.
	return jsonResult(map[string]any{
		"turn_id": turnID, "note_id": noteID, "agent_handle": handle,
		"outcome": string(statelog.OutcomeUnknown),
	})
}

// runSeat reads the turn's run record for the seat it names. recorded is false
// where the turn has none; a refusal is a record that could not be read, which
// decides nothing and sends nothing.
func (t *steerTurn) runSeat(ctx context.Context, turnID string) (RunSeat, bool, *tools.Result) {
	readCtx, cancel := context.WithTimeout(ctx, SteerAskBudget)
	defer cancel()
	seat, recorded, err := t.deps.Runs.Seat(readCtx, turnID)
	if err != nil {
		// A CONDITION IS WRITTEN HERE, with the turn, since [nodeFailure]
		// logs only a fault.
		if _, condition := Condition(err); condition {
			log.WarnContext(ctx, "steer_turn_record_unread", "turn_id", turnID,
				"error", err.Error())
		}
		return RunSeat{}, false, refusalOf(nodeFailure(ctx, SteerTurnTool, err,
			fmt.Sprintf("Could not read the record of turn %s's executor", clip(turnID)),
			"Nothing was sent."))
	}
	return seat, recorded, nil
}

// maySteer decides whether the caller may steer turnID: on the seat the run's
// record names where there is one, and on the seat a probe of the fleet names
// where there is none — read only when the decision turns on it. Nil admits.
func (t *steerTurn) maySteer(ctx context.Context, turnID string, seat RunSeat, recorded bool) *tools.Result {
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
	if recorded {
		// THE RECORD'S ANCHOR, and nothing else: no scatter is asked about a
		// turn whose run record already names its seat.
		if seat.Unverified != nil {
			return refusalOf(unopenedRefusal(turnID, seat.Unverified))
		}
		owner, named := t.seatOf(seat.Agent, seat.Handle)
		if !named {
			return refusalOf(refusedBy(tools.RefusalUnavailable, waitless(), fmt.Sprintf("The "+
				"record of turn %s's executor names no seat, so whether you may steer it "+
				"cannot be decided. Nothing was sent.", clip(turnID))))
		}
		return askAuthority(ctx, t.authorize, authz.ActionTurnSteer,
			authz.Object{Kind: authz.KindPerson, ID: turnID, Owner: owner})
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
		return refusalOf(refusedBy(tools.RefusalUnavailable, waits(), fmt.Sprintf("No node "+
			"answered for turn %s within %s: it is not running anywhere, or the node "+
			"running it did not answer in time. Nothing was sent; try again.",
			clip(turnID), SteerAskBudget)))
	case reply.Unverified != nil:
		return refusalOf(unverifiedRefusal(ctx, turnID, reply))
	}
	owner, named := t.seatOf(reply.Agent, reply.AgentHandle)
	if !named {
		return refusalOf(refusedBy(tools.RefusalUnavailable, waits(), fmt.Sprintf("The node "+
			"running turn %s could not name the seat it runs for, so whether you may "+
			"steer it cannot be decided. Nothing was sent; try again shortly.",
			clip(turnID))))
	}
	return askAuthority(ctx, t.authorize, authz.ActionTurnSteer,
		authz.Object{Kind: authz.KindPerson, ID: turnID, Owner: owner})
}

// seatOf is the handle the authority is asked about for the seat a run record
// or a probe names: by its id through the running org, and the named handle
// where this org no longer holds the seat — which nobody leads, so only admin
// would have passed. False when no seat is named at all.
func (t *steerTurn) seatOf(agent, handle string) (string, bool) {
	id, err := uuid.Parse(agent)
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
	if handle != "" {
		return handle, true
	}
	return id.String(), true
}

// ask scatters one request and returns the reply that answers for its turn.
// answered is false when no node did within [SteerAskBudget]; a refusal is
// the ask itself failing, which sent nothing anywhere.
//
// SIGNED AND BOUND, every time: a fresh id per ask — a probe and the offer
// after it are two requests — and the ask's own deadline, which is how long
// a node remembers the id; and a reply is believed only where it verifies
// and answers that id.
func (t *steerTurn) ask(ctx context.Context, req steer.Request) (steer.Reply, bool, *tools.Result) {
	askCtx, cancel := context.WithTimeout(ctx, SteerAskBudget)
	defer cancel()
	req.ID = signedask.NewID()
	req.Deadline, _ = askCtx.Deadline()
	request, err := t.deps.Wire.SealRequest(req)
	if err != nil {
		// A FAULT: the request is this build's own struct, bound here, so
		// a seal that fails fails the same way every time.
		return steer.Reply{}, false, refusalOf(faulted(ctx, SteerTurnTool, err,
			SteerTurnTool+" could not sign its request: "+faultSaid+". Nothing was sent."))
	}
	replies, err := t.deps.Asker.Ask(askCtx, topics.SeatSteer, request, 1)
	if err != nil {
		// THE ASK COULD NOT BE MADE, so nothing was sent anywhere — which
		// is `unavailable` rather than `unknown`: there is no note in
		// flight to be uncertain about.
		return steer.Reply{}, false, refusalOf(nodeFailure(ctx, SteerTurnTool, err,
			SteerTurnTool+" could not reach the fleet", "Nothing was sent."))
	}
	reply, answered := t.replyFor(ctx, replies, req)
	return reply, answered, nil
}

// replyFor is the reply that answers req, if any did. A reply the fleet did
// not sign, one signed for another request, one this build cannot read and
// one about another turn are each a node that did not answer — never an
// answer: a reply nobody can vouch for that named the caller's own report as
// the turn's seat would be the authority deciding on a forgery.
func (t *steerTurn) replyFor(ctx context.Context, replies [][]byte, req steer.Request) (steer.Reply, bool) {
	for _, raw := range replies {
		reply, why, err := t.deps.Wire.OpenReply(raw, req.ID)
		switch {
		case err != nil:
			log.WarnContext(ctx, "steer_turn_reply_unread", "turn_id", req.TurnID,
				"error", err.Error())
			continue
		case why != "":
			log.WarnContext(ctx, "steer_turn_reply_refused", "turn_id", req.TurnID,
				"detail", why)
			continue
		case reply.TurnID != req.TurnID:
			continue
		}
		return reply, true
	}
	return steer.Reply{}, false
}

// unverifiedRefusal is the running node saying it took nothing because it
// could not verify the request: unavailable, on [steer.ErrUnverified], a
// condition no wait clears, since what changes the answer is an operator
// bringing the two nodes' keyrings into agreement.
//
// A CONDITION, SO EACH READER HAS WORDS OF THEIR OWN: the person's sentence
// says the note was not taken and who fixes it ([ConditionSentence]), and the
// assistant that called the tool is told the same and that retrying won't
// help. As a bare refusal carrying the person's sentence, it handed that
// assistant words written for the person steering. The act route answers a
// person only in words the refusal catalog wrote, and the catalog words no
// condition (ADR-0060), so there the person steering is told the
// `unavailable` code's own sentence for a refusal no wait clears, under no
// Retry-After, and the tool's sentence goes to the assistant's model alone.
// The node running the turn, the keys it holds and the turn go to this node's
// log, which is where the person who administers the keyring reads them. Said
// to the person steering, they were a turn id and a list of key ids nobody at
// a dashboard chose. They are fields the running node answered rather than a
// sentence it wrote, because its sentence said "this node", which read here as
// this one.
func unverifiedRefusal(ctx context.Context, turnID string, reply steer.Reply) tools.Result {
	log.WarnContext(ctx, "steer_turn_unverified", "turn_id", turnID,
		"running_node", reply.Unverified.Node, "held_keys", reply.Unverified.HeldKeys,
		"detail", "the node running the turn holds none of the keys the note is "+
			"signed under; add the key the fleet signs with to its secrets.keys")
	return refusedBy(tools.RefusalUnavailable, steer.ErrUnverified, "The node running "+
		"this turn couldn't verify the note, so it took nothing, and sending it again "+
		"won't change that. Tell the person steering that an administrator needs to "+
		"give the fleet's nodes the same keys.")
}

// unopenedRefusal is a run's record whose sealed anchor names no seat anybody
// can vouch for ([RunSeat.Unverified]): unavailable, and no wait clears any of
// them, since the launch sealed the anchor once.
//
// ONE UNDER A KEY THIS NODE DOES NOT HOLD IS THE FLEET'S NODES NOT HOLDING THE
// SAME KEYS, the fact [unverifiedRefusal] answers on the scatter, so it is told
// the same way: a condition no wait clears ([steer.ErrSeatKeyUnknown]), which
// gives each reader words of their own from the one condition table, and the
// assistant that called the tool is told that sending it again won't help and
// who the person steering should be told fixes it. As a bare no-wait refusal
// it was handed a sentence the engine wrote for the person, and no reader that
// tells a condition knew it as one. The keys this node holds, and the turn, are
// in its log (steer_seat_key_unknown, internal/engine). A record naming no
// seat, or an anchor that does not verify, is a record no keyring opens, and
// says so.
func unopenedRefusal(turnID string, unverified error) tools.Result {
	switch {
	case errors.Is(unverified, steer.ErrSeatKeyUnknown):
		return refusedBy(tools.RefusalUnavailable, unverified, "Whose turn this is cannot "+
			"be decided: its executor's record names its seat under a key this node "+
			"doesn't hold, so nothing was sent, and sending it again won't change that. "+
			"Tell the person steering that an administrator needs to give the fleet's "+
			"nodes the same keys.")
	case errors.Is(unverified, steer.ErrSeatUnnamed):
		return refusedBy(tools.RefusalUnavailable, waitless(), fmt.Sprintf("Whose turn %s "+
			"is cannot be decided: its executor's record names no seat. Nothing was sent.",
			clip(turnID)))
	}
	return refusedBy(tools.RefusalUnavailable, waitless(), fmt.Sprintf("Whose turn %s is "+
		"cannot be decided: the seat its executor's record names does not verify. Nothing was "+
		"sent.", clip(turnID)))
}
