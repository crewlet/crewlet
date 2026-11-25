package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
)

// TryResumeFromAnswer records a delivery as the answer to the question a parked run
// of this seat waits on, if it is one, and resumes the run. It reports what the
// CALLER does with the delivery ([AnswerDisposition], the whole contract). A reply
// answers only if written after the question ([Reply.qualifies]); the first one is
// RECORDED (compare-and-set) and spent ([AnswerConsumed]) whatever the resume
// concludes, retried with the inbox held (answerowed.go). No run to answer, or none
// the delivery qualifies for, is [AnswerNotMine]; unreadable runs answer by this
// node's count ([Coordinator.answerLookupFailed]). An unrecordable answer is
// [AnswerDeferred] (the write MAY have landed), a failure of this node or store,
// handed back to keep its place at the inbox head, not NAKed behind newer mail
// ([MaxAnswerAttempts]). A reply past [MaxAnswerBytes] is [AnswerRefused], its
// REFUSAL the outcome (ADR-0114); a run ended under it returns it THROUGH THE ROW.
func (c *Coordinator) TryResumeFromAnswer(ctx context.Context, handle string, reply Reply) (AnswerDisposition, error) {
	if reply.Conv.Identity == "" && reply.Conv.Partition == "" {
		return AnswerNotMine, nil
	}
	trigger := reply.trigger()
	runs, err := c.pending.ListActiveForSeat(ctx, handle)
	if err != nil {
		// BOTH KEYS: the identity admits a run and the partition picks between
		// them ([ConversationRef.Best]), and on a direct message they differ.
		log.WarnContext(ctx, "sandbox_answer_lookup_failed",
			"agent", handle, "conversation", reply.Conv.Identity,
			"partition", reply.Conv.Partition, "error", err.Error())
		return c.answerLookupFailed(ctx, handle, trigger, err)
	}
	// THE LOOKUP ANSWERED, so what this seat counted against an unreadable store
	// is spent.
	c.clearLookupAttempts(handle)
	// Bounded by the seat's runs: each lost race is a run another reply or
	// transition took. mayAnswer is whether the writer's holder may act now,
	// asked once and only after a run would take the reply
	// ([AudienceResolver.MayAnswer]), so a chat message no question was put to
	// its writer reads nothing of the identity directory.
	mayAnswer := false
	for range len(runs) + 1 {
		if recorded, ok := reply.recordedOn(runs); ok {
			// A COPY OF AN ANSWER ALREADY RECORDED: spent as the answer it is, and
			// if this node is not driving its resume it starts (the recording node
			// may have stopped). A copy of a REFUSAL'S delivery is kept again until
			// the refusal leaves the run ([Coordinator.refusedReply]), spent at once
			// where the attempt this copy starts kept it.
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			kept := false
			if recorded.Status == StatusAnswered && !c.owes(recorded.TurnID) {
				kept = c.resumeOwed(ctx, recorded)
			}
			if _, refused := recorded.Refused(); refused {
				// AND ITS RECORD PUBLISHED AGAIN under the identity the run, question
				// and delivery derive: the recording node may have stopped before
				// publishing, and a landed record is the same row.
				c.announceRefusal(ctx, recorded, trigger)
				return c.refusedReply(ctx, recorded, reply, kept), nil
			}
			return AnswerConsumed, nil
		}
		run, found := reply.Best(runs)
		if !found {
			// NOTHING MATCHED: an ordinary message.
			return AnswerNotMine, nil
		}
		if !mayAnswer {
			// THE WRITER'S HOLDER, NOW: a person suspended or removed since, or an
			// unheld human seat, answers nothing. A directory that cannot say
			// keeps the reply's place, bounded per run like every deferral, never
			// spent on the run nor on an ordinary turn.
			may, err := c.audience.MayAnswer(ctx, reply.Writer)
			if err != nil {
				return c.deferAnswer(ctx, run, trigger,
					fmt.Errorf("sandbox: whether %s may answer %s now: %w", reply.Writer, run.TurnID, err))
			}
			if !may {
				log.InfoContext(ctx, "sandbox_answer_writer_may_not_act", "turn_id", run.TurnID,
					"agent", handle, "writer", reply.Writer,
					"detail", "the question was put to this seat, and whoever holds it may not act in "+
						"the company now, so the reply runs as the ordinary message it is and the "+
						"run waits on its question")
				return AnswerNotMine, nil
			}
			mayAnswer = true
		}
		recorded, won, err := c.recordAnswer(ctx, run, reply)
		refused := false
		if tooLarge, ok := errors.AsType[*AnswerTooLargeError](err); ok {
			// NO ANSWER THE ROW CAN CARRY ([MaxAnswerBytes]), refused by the store
			// against the run still waiting: ITS REFUSAL IS THE QUESTION'S OUTCOME
			// (ADR-0114), recorded in the answer's place (figures, no words) and
			// resumed like an answer. Recorded on the same terms, so a run answered
			// another way or moved on is the reply's ordinary loss, read again
			// below. Left waiting, the question took the next short message ("any
			// luck?") as its answer.
			recorded, won, err = c.recordRefusal(ctx, run, reply, tooLarge)
			refused = true
		}
		if errors.Is(err, ErrSeatNotHeld) {
			// THE SEAT'S HOLDER'S TO RECORD: nothing written; the delivery goes
			// back to reach the holder, uncounted (the budget is this node's, for a
			// seat it does not hold).
			return AnswerDeferred, err
		}
		if err != nil {
			// THE RECORD MAY HAVE LANDED: a store that could not say did not say
			// no, so the delivery comes back, and a copy finding the record is
			// spent above.
			return c.deferAnswer(ctx, run, trigger,
				fmt.Errorf("sandbox: recording the answer %s is waiting on: %w", run.TurnID, err))
		}
		if won && refused {
			// THE DELIVERY IN HAND IS DECIDED BY WHAT THE INLINE ATTEMPT DID with
			// the refusal: kept by its turn, spent here as an answer's is at its
			// record; otherwise off the row ([Coordinator.refusedReply]).
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			c.announceRefusal(ctx, recorded, trigger)
			kept := c.resumeOwed(ctx, recorded)
			return c.refusedReply(ctx, recorded, reply, kept), nil
		}
		if won {
			c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
			c.resumeOwed(ctx, recorded)
			return AnswerConsumed, nil
		}
		// LOST: another reply was recorded first or the run moved on; read again
		// so the decision is made against what the store holds now.
		if runs, err = c.pending.ListActiveForSeat(ctx, handle); err != nil {
			return c.answerLookupFailed(ctx, handle, trigger, err)
		}
	}
	return c.answerLookupFailed(ctx, handle, trigger,
		errors.New("sandbox: the seat's runs kept changing under the answer's record"))
}

// owes reports whether this node is already driving a run's owed answer.
func (c *Coordinator) owes(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.retries[turnID]; ok {
		return true
	}
	for _, owed := range c.owed {
		if _, ok := owed[turnID]; ok {
			return true
		}
	}
	return false
}

// AnswerByTurn records the answer a person gave a parked run BY NAMING IT and
// resumes the run. It travels the seat's CONTROL TOPIC ([AnswerDesk.Deliver],
// [Coordinator.OnAnswerGiven]), never its mail, where a backlog starved it until
// the pause TTL. RECORDED first like a chat reply ([PendingStore.RecordAnswer]),
// then the RESUME waits for a free seat ([Coordinator.queueOwed]): resumed under the
// claim alone, a node stopping before its turn lost the answer. The delivery carries
// its question ([types.ExecutorAnswerGiven.LaunchID], new per question) and records
// only while the run waits on THAT one, else `not_awaiting`; none is refused
// ([errAnswerNamesNoQuestion]). NEVER A TURN: [AnswerConsumed] and [AnswerNotMine]
// are spent, only [AnswerDeferred] is NAKed, with no attempt ceiling on the record
// (dropping would lose the answer silently); the recorded resume is bounded like a
// chat reply's ([MaxAnswerAttempts], [answerWindow]). Publishes [types.ExecutorAnswered].
func (c *Coordinator) AnswerByTurn(ctx context.Context, given types.ExecutorAnswerGiven, trigger *events.Event) (AnswerDisposition, error) {
	answered := func(run PendingRun, outcome types.AnswerOutcome) {
		c.announceAnswered(ctx, run, types.AnswerViaOperator, outcome, answerer{
			by: given.AnsweredBy, kind: given.AnsweredByKind, operatorID: given.OperatorID,
			choice: given.Choice, said: redact.Secrets(strings.TrimSpace(given.Answer)),
			delegatedRule: given.DelegatedRule,
		})
	}
	if given.LaunchID == "" {
		// NO QUESTION NAMED, so none to pair with: spent and said why.
		log.ErrorContext(ctx, "sandbox_answer_names_no_question",
			"turn_id", given.TurnID, "agent", given.AgentHandle,
			"answered_by", given.AnsweredBy, "answered_by_kind", given.AnsweredByKind,
			"operator_id", given.OperatorID,
			"detail", "an answer by turn arrived naming no question (launch_id), so which "+
				"question it answers cannot be known; it is spent without resuming the run, "+
				"and the person has to answer the run again")
		return AnswerNotMine, fmt.Errorf("%w: run %s", errAnswerNamesNoQuestion, given.TurnID)
	}
	delivery := deliveryOf(trigger)
	if delivery == "" {
		// NOTHING TO RECORD IT UNDER: a record names its deliveries, by which a
		// copy is recognised.
		return AnswerNotMine, fmt.Errorf("sandbox: the answer to run %s arrived with no event to "+
			"record it under", given.TurnID)
	}
	for range answerRecordRounds {
		run, found, err := c.pending.Get(ctx, given.TurnID)
		if err != nil {
			// A STORE THAT COULD NOT BE READ IS NOT A RUN THAT IS GONE: handed
			// back, nothing announced.
			return AnswerDeferred, fmt.Errorf("sandbox: reading run %s for the answer it was given: %w",
				given.TurnID, err)
		}
		if !found {
			// SETTLED, or never there; all known is what the answer named.
			answered(PendingRun{
				TurnID: given.TurnID, AgentID: given.Agent, AgentHandle: given.AgentHandle,
			}, types.AnswerGone)
			return AnswerNotMine, nil
		}
		if run.AgentID != given.Agent {
			// THE ANSWER REACHED ANOTHER SEAT'S INBOX ([AnswerDesk.Deliver]
			// addresses it off the accepted row): resuming it here would re-enter
			// another seat's conversation as this one. Compared by agent id, which
			// names the inbox.
			log.ErrorContext(ctx, "sandbox_answer_misrouted",
				"turn_id", given.TurnID, "addressed_to", given.Agent,
				"run_seat", run.AgentID,
				"detail", "an answer by turn arrived on a seat that does not hold the run it "+
					"names; it is dropped rather than resumed under the wrong seat")
			return AnswerNotMine, nil
		}
		if run.Answer != nil && slices.Contains(run.Answer.EventIDs, delivery) {
			// THIS VERY ANSWER, ALREADY RECORDED (a redelivery whose ack was lost):
			// spent as the answer it is, and if this node is not driving its resume
			// it starts, unless the ending is decided, which takes no resume and
			// lets the answer go back to the seat.
			if run.Status == StatusAnswered && run.Ending == nil && !c.owes(run.TurnID) {
				c.queueOwed(ctx, run)
			}
			return AnswerConsumed, nil
		}
		if given.AnsweredByKind == string(iam.ActorSystem) && run.EngineAnswers() {
			// THE ENGINE'S ANSWER BY TURN, AS AN OLDER BUILD'S WATCH SENDS IT, even
			// past losing the seat: this build carries such a run on from its row
			// (waitover.go), and the keys saying how its wait went are gone off
			// the wire, so recorded it tells the run a person answered. Spent
			// unrecorded and untold: nobody answered.
			log.WarnContext(ctx, "sandbox_engine_answer_dropped",
				"turn_id", given.TurnID, "launch_id", given.LaunchID, "agent", given.AgentHandle,
				"detail", "an answer by turn from the engine reached a run the engine carries on from its "+
					"row; it is spent without being recorded, and the seat's watch carries the run on")
			return AnswerNotMine, nil
		}
		if slices.Contains(run.DeclinedAnswers, delivery) {
			// THE QUESTION LET THIS ANSWER GO (every resume attempt failed): this
			// is the decline's copy or the original round again. It has no
			// ordinary form, so it is spent, the person told by the COPY, which
			// the decline publishes once through the row's outbox.
			log.WarnContext(ctx, "sandbox_answer_declined_spent",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "delivery", delivery,
				"detail", "an answer by turn its run could not be resumed with was let go of; "+
					"the run waits on its question again and has to be answered again")
			if declinedCopy(run.DeclinedAnswers, delivery) {
				answered(run, types.AnswerDeclined)
			}
			return AnswerNotMine, nil
		}
		if run.Ending != nil {
			// THE RUN IS OVER though its record is not gone: a decided ending takes
			// no answer ([ErrRunEnding]) and the record waits for the ending's last
			// steps (the end's write and the delete). The copy of an answer the
			// ending let go of arrives in that window; read as a live run not
			// waiting it was announced `not_awaiting` and the person never learned
			// the run ended without their answer.
			answered(run, types.AnswerGone)
			return AnswerNotMine, nil
		}
		if !slices.Contains(Awaiting, run.Status) || given.LaunchID != run.LaunchID {
			// NOT WAITING ON THE QUESTION THIS ANSWERS: answered another way or
			// moved on since ("The question it answers" above).
			answered(run, types.AnswerNotAwaiting)
			return AnswerNotMine, nil
		}
		if heldMismatch(run, given) {
			// A HELD CALL IS ANSWERED BY ITS APPROVAL ALONE, and only a held call
			// is: the route deciding who may approve stamps its id, every other
			// route stamps none. Spent, said, never recorded: recording would make
			// or refuse a call nobody entitled decided.
			log.ErrorContext(ctx, "sandbox_answer_approval_mismatched",
				"turn_id", given.TurnID, "launch_id", given.LaunchID, "approval", given.Approval,
				"held", run.HeldCall != nil, "answered_by", given.AnsweredBy,
				"detail", "an answer by turn names a held call the run's question is not, or "+
					"answers a held call without naming it; it is spent without resuming the run")
			answered(run, types.AnswerUnoffered)
			return AnswerNotMine, nil
		}
		if run.HeldCall != nil && run.HeldCall.Lapsed(c.now()) && given.Choice != HeldDeny {
			// A LAPSED CALL TAKES DENY ALONE, the engine's own or a person's:
			// an allow arriving between the lapse and the sweep would make a
			// call on figures a day old (ADR-0171).
			log.WarnContext(ctx, "sandbox_answer_lapsed",
				"turn_id", given.TurnID, "approval", given.Approval, "choice", given.Choice,
				"answered_by", given.AnsweredBy, "lapsed_at", run.HeldCall.LapsesAt)
			answered(run, types.AnswerNotAwaiting)
			return AnswerNotMine, nil
		}
		// WHAT IT SAYS, MEASURED AS `answer_executor` MEASURED IT: as authored,
		// trimmed, before redaction, the one string both ends hold to
		// [MaxAnswerTextBytes], so a tool-accepted answer is never refused here
		// for a redaction it did not write. Before the choice is read, in the
		// tool's order.
		if said := len(strings.TrimSpace(given.Answer)); said > MaxAnswerTextBytes {
			return c.answerByTurnTooLarge(ctx, run, given, "words", said, MaxAnswerTextBytes)
		}
		words, answer, err := answerGivenOf(run, given, trigger, c.now())
		if err != nil {
			// A CHOICE THE QUESTION DOES NOT OFFER (`answer_executor` checked it
			// before delivering): spent under an outcome of its own, never handed
			// to the run as an unoffered option nor told as not waiting.
			log.ErrorContext(ctx, "sandbox_answer_choice_unoffered",
				"turn_id", given.TurnID, "launch_id", given.LaunchID, "choice", given.Choice,
				"answered_by", given.AnsweredBy, "error", err.Error())
			answered(run, types.AnswerUnoffered)
			return AnswerNotMine, nil
		}
		recorded, won, err := c.record(ctx, run, words, answer)
		if tooLarge, ok := errors.AsType[*AnswerTooLargeError](err); ok {
			// NO ANSWER THE ROW CAN CARRY ([MaxAnswerBytes]), refused before any
			// write, which no attempt changes: the row's backstop, unreachable by
			// an answer within [MaxAnswerTextBytes] even at its worst
			// (TestAnAnswerByTurnAtItsSourcesBoundFitsARecordedAnswersShare).
			return c.answerByTurnTooLarge(ctx, run, given, "record", tooLarge.Weighs, tooLarge.May)
		}
		if err != nil {
			// THE RECORD MAY HAVE LANDED, resolved towards the run as on the chat
			// route: a landed record is found on redelivery and spent.
			return AnswerDeferred, fmt.Errorf("sandbox: recording the answer run %s was given: %w",
				given.TurnID, err)
		}
		if !won {
			// LOST between read and record: read again against what the store
			// holds now.
			continue
		}
		log.InfoContext(ctx, "sandbox_clarification_answered",
			"turn_id", recorded.TurnID, "via", string(types.AnswerViaOperator),
			"answered_by", given.AnsweredBy, "answered_by_kind", given.AnsweredByKind,
			"operator_id", given.OperatorID)
		c.clearAnswerAttempts(recorded.AgentHandle, recorded.TurnID)
		c.queueOwed(ctx, recorded)
		return AnswerConsumed, nil
	}
	return AnswerDeferred, fmt.Errorf("sandbox: run %s kept changing under the answer it was given",
		given.TurnID)
}

// answerRecordRounds bounds [Coordinator.AnswerByTurn]'s record loop.
//
// A round repeats only when the record was REFUSED (answered another way or
// moved on between read and record; a lost race on the row is the store's to
// retry). The read after a refusal lands on the branch that explains it, so one
// round past the first covers it and a third covers a question answered and let
// go again in between. Past that the answer is handed back (AnswerDeferred) and
// the queue's backoff paces it, not back-to-back reads.
const answerRecordRounds = 3

// answerByTurnTooLarge spends an answer by turn past what its run may take,
// refused before any write (past [MaxAnswerTextBytes], measured "words", or past
// [MaxAnswerBytes] on the row, "record"), recording `executor_answered`
// too_large with the figures and none of the words, the question still waiting.
// It has no ordinary form and no conversation to be told in, so the record says
// it was not taken; never `declined`, which names an answer the run held and
// let go.
func (c *Coordinator) answerByTurnTooLarge(ctx context.Context, run PendingRun,
	given types.ExecutorAnswerGiven, measured string, weighs, may int,
) (AnswerDisposition, error) {
	log.ErrorContext(ctx, "sandbox_answer_too_large",
		"turn_id", given.TurnID, "launch_id", given.LaunchID, "answered_by", given.AnsweredBy,
		"measured", measured, "bytes", weighs, "max_bytes", may,
		"detail", "an answer by turn past what its run may take as an answer is spent "+
			"unrecorded, and the run waits on its question")
	c.announceAnswered(ctx, run, types.AnswerViaOperator, types.AnswerTooLarge, answerer{
		by: given.AnsweredBy, kind: given.AnsweredByKind, operatorID: given.OperatorID,
		choice: given.Choice, answerBytes: weighs, answerMaxBytes: may,
	})
	return AnswerNotMine, nil
}

// OnAnswerGiven settles one answer BY TURN off the seat's control topic: an error
// hands the delivery back, nil spends it. SPENT BEFORE IS SPENT NOW: a delivery
// recorded as worked ([CoordinatorOptions.WasSpent]) is acknowledged without
// reaching its run (it would announce `gone` or `not_awaiting` about an answer that
// resumed it), read BEFORE the coordinator is asked since the run then says nothing.
// EVERY DISPOSITION BUT [AnswerDeferred] SPENDS IT and records it as worked
// ([CoordinatorOptions.Spent]); the deferred goes back to the broker, whose backoff
// spaces attempts and whose dead-letter subject is the loud end.
func (c *Coordinator) OnAnswerGiven(ctx context.Context, given types.ExecutorAnswerGiven, ev *events.Event) error {
	handle := given.AgentHandle
	if c.wasSpent != nil && ev != nil && c.wasSpent(ctx, handle, ev) {
		log.InfoContext(ctx, "sandbox_answer_by_turn_already_spent",
			"agent_handle", handle, "turn_id", given.TurnID, "event_id", ev.ID.String(),
			"detail", "this answer by turn was spent before (taken by a resumed turn, let go "+
				"of, or settled), so its redelivery is acknowledged without reaching its run "+
				"or being announced again")
		return nil
	}
	disposition, err := c.AnswerByTurn(ctx, given, ev)
	switch {
	case disposition == AnswerDeferred:
		if err == nil {
			err = fmt.Errorf("sandbox: the answer to run %s could not be recorded", given.TurnID)
		}
		return fmt.Errorf("sandbox: %s is still owed this answer: %w", handle, err)
	case !disposition.Valid():
		log.ErrorContext(ctx, "sandbox_answer_disposition_unknown",
			"agent_handle", handle, "turn_id", given.TurnID, "disposition", disposition.String(),
			"detail", "an answer by turn was given no disposition this build knows; it is "+
				"spent, because an answer addressed to a run is never a turn")
	case err != nil:
		log.WarnContext(ctx, "sandbox_answer_by_turn_failed",
			"agent_handle", handle, "turn_id", given.TurnID,
			"disposition", disposition.String(), "error", err)
	}
	if c.spent != nil && ev != nil {
		c.spent(ctx, handle, []*events.Event{ev})
	}
	return nil
}

// errRunMovedOn is a resume's report that the run moved on to somebody else
// first (a newer lease owns it, or another party ended it), so the beside
// disposition ([AnswerNotMine]) is not a run that is gone: whoever holds it now
// drives and announces it. Neither route hands back the delivery as its own
// failure: a completion's is settled ([Coordinator.OnCompleted]), marked
// [ErrUnannounced] only while no waiter can announce its job (renew.go).
var errRunMovedOn = errors.New("sandbox: the run moved on to another holder before this " +
	"attempt could end it")

// errAnswerNamesNoQuestion is an answer by turn that carries no
// [types.ExecutorAnswerGiven.LaunchID]: it is spent rather than resumed with,
// because the question it answers cannot be known.
var errAnswerNamesNoQuestion = errors.New("sandbox: the answer names no question it answers " +
	"(no launch_id), so it is spent without resuming the run")

// declinedCopy reports whether delivery is the COPY a decline handed back for
// one of the deliveries declined beside it ([declinedCopyID]), rather than one
// of those deliveries itself.
func declinedCopy(declined []string, delivery string) bool {
	for _, id := range declined {
		original, err := uuid.Parse(id)
		if err == nil && declinedCopyID(original).String() == delivery {
			return true
		}
	}
	return false
}

// answerGivenOf is what recording an answer by turn writes: the sealed words
// (the answer as the run reads it, [AnswerText], and who gave it per
// iam.ActorFor) and the record they are sealed onto ([Coordinator.record]), with
// the one delivery it arrived in, the [types.ExecutorAnswerGiven] event, carried
// so a copy can be handed back if the answer is let go. An error is a choice the
// run's question does not offer.
func answerGivenOf(run PendingRun, given types.ExecutorAnswerGiven, trigger *events.Event,
	now time.Time,
) (AnswerWords, RecordedAnswer, error) {
	if _, err := AnswerText(run.Decision, given.Choice, given.Answer); err != nil {
		return AnswerWords{}, RecordedAnswer{}, err
	}
	words := AnswerWords{
		// Redacted HERE, as a chat reply is: it is sealed on the row and handed
		// as is to the harness, whose box and session keep it.
		Text: redact.Secrets(strings.TrimSpace(given.Answer)), Choice: given.Choice,
		Via: types.AnswerViaOperator,
		By:  given.AnsweredBy, ByKind: given.AnsweredByKind, OperatorID: given.OperatorID,
	}
	answer := RecordedAnswer{
		EventIDs: []string{trigger.ID.String()}, PostedAt: trigger.Timestamp, RecordedAt: now,
	}
	raw, err := json.Marshal(trigger)
	if err != nil {
		// Kept without it, as a chat reply's is: it costs only a let-go's copy.
		log.Warn("sandbox_answer_event_unencodable", "event_id", trigger.ID.String(), "error", err.Error())
		return words, answer, nil
	}
	answer.Events = []json.RawMessage{raw}
	return words, answer, nil
}

// answeredAs is what a settled answer became, from what the resume left the
// delivery: a turn that ran consumed it, and anything else is a run that is
// gone. Never called for a deferral, which has not become anything.
func answeredAs(disposition AnswerDisposition) types.AnswerOutcome {
	if disposition == AnswerConsumed {
		return types.AnswerResumed
	}
	return types.AnswerGone
}

// answerer is who gave an answer, in iam.ActorFor's three halves
// ([types.ExecutorAnswered.AnsweredBy]), and what they answered: the option
// chosen by id and their words, announced beside the question.
type answerer struct {
	by, kind, operatorID string
	choice, said         string
	// delegatedRule is the rule a delegate answered under, 0 for a person.
	delegatedRule int
	// answerBytes and answerMaxBytes are an answer refused as too large and what
	// one may be, on the refusal's measure ([types.AnswerTooLarge]); else zero.
	answerBytes, answerMaxBytes int
}

// chatAnswerer is who a chat reply came from: the sender as the transport's
// envelope names them, a person, no credential (the chat surface vouched).
func chatAnswerer(trigger *events.Event) answerer {
	if trigger == nil {
		return answerer{}
	}
	return answerer{by: trigger.Actor(), kind: string(iam.ActorHuman)}
}

// announceAnswered publishes what an answer to a parked run became.
//
// BEST EFFORT, like every after-the-fact announcement: a feed row that could not
// be written is a log line, not a reason to hand the delivery back and resume
// the run twice.
func (c *Coordinator) announceAnswered(ctx context.Context, run PendingRun,
	via types.AnswerVia, outcome types.AnswerOutcome, by answerer,
) {
	c.announceAnsweredAs(ctx, run, via, outcome, by, recordIdentity{})
}

// announceAnsweredAs is [Coordinator.announceAnswered] under identity as, fresh
// where zero: a record the same fact republishes carries its id and instant, which
// the event store keeps as one row ([recordIdentity]).
func (c *Coordinator) announceAnsweredAs(ctx context.Context, run PendingRun,
	via types.AnswerVia, outcome types.AnswerOutcome, by answerer, as recordIdentity,
) {
	payload := types.ExecutorAnswered{
		Agent: run.AgentID, AgentHandle: run.AgentHandle, RoleName: run.Role,
		TurnID: run.TurnID, WorkKey: run.WorkKey, WorkItem: run.WorkItem,
		Via: via, Outcome: outcome,
		AnsweredBy: by.by, AnsweredByKind: by.kind, OperatorID: by.operatorID,
		DelegatedRule: by.delegatedRule,
		// THE QUESTION AND ITS ANSWER, which the row forgets once the run moves
		// on: the one account outliving the run, read from the event log for a
		// person's answered decisions (ADR-0068).
		LaunchID: run.LaunchID, Question: run.Question, QuestionContext: run.QuestionContext,
		Decision: run.Decision, AudienceHandles: slices.Clone(run.AudienceHandles),
		Choice: by.choice, Answer: by.said,
	}
	if by.answerBytes > 0 {
		payload.AnswerBytes, payload.AnswerMaxBytes = by.answerBytes, by.answerMaxBytes
	}
	if !run.AskedAt.IsZero() {
		payload.AskedAt = run.AskedAt.UTC()
	}
	ev := events.New(payload, events.TraceContext{TraceID: run.TraceID, ParentSpanID: run.SpanID})
	if as.id != uuid.Nil {
		ev.ID = as.id
	}
	if !as.at.IsZero() {
		ev.Timestamp = as.at.UTC()
	}
	ev.Source = run.Role
	if err := c.queue.Publish(ctx, topics.Event(payload.EventType()), ev); err != nil {
		log.WarnContext(ctx, "sandbox_answer_announce_failed",
			"turn_id", run.TurnID, "via", string(via), "outcome", string(outcome),
			"error", err.Error())
	}
}
