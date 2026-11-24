package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The answer leg of an agent-to-agent ask: the asker finished its turn when it
// asked (a2a_ask), so only this reply wakes it.
//
// It is sent from every frame that ends the turn. The dispatch frame holds the
// ask as its trigger; a turn that launched its executor is ended by a frame that
// never saw the trigger (its resume, a person's stop of the run), so the launch
// writes the ask onto the run's record ([sandbox.ColleagueAsk]) and that frame
// answers from there. And from the run's loss, which ends a parked turn with no
// frame at all: the coordinator hands the run to [Engine.answerLostTurn], since
// left to the idle sweep the asker hears nothing (a swept close wakes nobody).

// askOf returns the a2a request this turn is answering, or nil: the first, as an
// a2a wake carries its own conversation key, so a partition holds one ask. Were
// there two, answering the first is honest; one text for both is not.
func askOf(evs []*events.Event) *events.Event {
	for _, ev := range evs {
		if ev != nil && ev.Type == types.A2ARequestType {
			return ev
		}
	}
	return nil
}

// colleagueAskOf is the ask an a2a request carries, as the turn answering it
// holds it on its frame and its executor's run record; nil for no ask.
func colleagueAskOf(ask *events.Event) *sandbox.ColleagueAsk {
	if ask == nil {
		return nil
	}
	asked := sandbox.ColleagueAsk{
		ChannelID: askChannel(ask),
		Chain:     slices.Clone(ask.DelegationChain),
		TraceID:   ask.TraceID, SpanID: ask.SpanID, ParentSpanID: ask.ParentSpanID,
		WorkingFor: answeredFor(ask),
	}.WithQuestion(askedQuestion(ask))
	return &asked
}

// answeredFor is who the answer is worked for back on the asker
// ([sandbox.ColleagueAsk.WorkingFor]): whoever the ask carried, else the asking
// seat itself, so the turn the answer wakes is the asker's own work, never the
// answering colleague's. Read from the ask the engine built, never from
// anything the answering turn wrote.
func answeredFor(ask *events.Event) *types.WorkingFor {
	req, ok := events.DataAs[*types.A2ARequest](ask)
	switch {
	case !ok || req == nil:
		return nil
	case req.WorkingFor != nil:
		w := *req.WorkingFor
		w.Apps = slices.Clone(req.WorkingFor.Apps)
		return &w
	case strings.TrimSpace(req.Requester) != "":
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: strings.TrimSpace(req.Requester)}
	}
	return nil
}

// answeringTurn is the turn that answers an ask, as the reply names it.
type answeringTurn struct {
	handle string
	// runID is the RUN, not the unit of work — see [Engine.answerColleague].
	runID   string
	workKey string
	// depth is the turn's delegation depth, which the reply carries unchanged.
	depth int
	// notContinued is what the turn's runs' sessions lost, said in the answer
	// ([sandbox.PendingRun.ConversationNotice]); nil for a turn that ran no job
	// or lost nothing.
	notContinued *executorkeep.NotContinued
}

// answeringOf is the turn a dispatch runs, as a reply names it: one that ends
// in its dispatch ran no job, so it says no session lost.
func answeringOf(req Request) answeringTurn {
	return answeringTurn{handle: req.Handle, runID: req.RunID, workKey: req.WorkKey, depth: req.Depth}
}

// answeringRun is the turn a run's record belongs to, as a reply names it.
func answeringRun(run sandbox.PendingRun) answeringTurn {
	return answeringTurn{handle: run.AgentHandle, runID: run.TurnID, workKey: run.WorkKey,
		depth: run.DelegationDepth, notContinued: run.ConversationNotice()}
}

// colleagueAnswer is what the engine answered a colleague's ask with, for the
// record of how the answering turn ended (executorkeep.Ending's Answer and
// Delivered): the engine sent it, so the turn's own record of calls cannot say.
type colleagueAnswer struct {
	// owed says the turn answered an ask: it was woken by one and ended.
	owed bool
	// kind is what the answer was, in one word ([answerContent]), or why
	// none was sent: `unaddressable` (the ask named no channel),
	// `no_service` (this node runs no agent-to-agent service).
	kind string
	// sent is the text sent, where one was.
	sent string
	// replied says the colleague has this turn's answer: this reply reached
	// the channel, or, found closed, an earlier attempt at the same ending
	// answered it before closing it ([Engine.answeredBefore]). A channel closed
	// with no answer (the asker gave up, or the sweep reaped it) is closed, not
	// replied.
	replied, closed bool
}

// onto records the answer on an ending: what the engine sent, and whether the
// colleague has it, which for a turn that owed the answer is the whole of
// whether its reply was delivered, and so never unknown.
func (a colleagueAnswer) onto(ending *executorkeep.Ending) {
	if !a.owed {
		return
	}
	ending.Answer, ending.DeliveredTo, ending.Delivered = a.kind, replyA2A, a.replied
	ending.DeliveryUnknown = false
	if a.closed {
		ending.Answer = "channel_closed"
	}
}

// answerColleague replies on the channel ask opened with what the turn by
// concluded (res, and err where it ended on one) and closes it; a nil ask
// answers nobody. It reports what it sent and whether the reply went.
//
// Best effort: the turn's side effects have already shipped, so a failure here
// must not fail a delivered turn. It costs the asker's wake, which the idle
// sweep backstops.
func (e *Engine) answerColleague(ctx context.Context, c *Company, by answeringTurn,
	ask *sandbox.ColleagueAsk, res turn.Result, err error,
) colleagueAnswer {
	if ask == nil {
		return colleagueAnswer{}
	}
	if res.Suspended {
		// Parked on its executor's run: answering now would send an unfinished
		// turn's artifact. The frame that ends it answers, off the run's record.
		return colleagueAnswer{}
	}
	if ask.ChannelID == "" {
		log.WarnContext(ctx, "a2a_answer_unaddressable", "seat", by.handle,
			"detail", "the ask carries no channel id, so there is nowhere to reply")
		return colleagueAnswer{owed: true, kind: "unaddressable"}
	}
	svc := e.a2aService(c)
	if svc == nil {
		return colleagueAnswer{owed: true, kind: "no_service"}
	}

	content, kind := answerContent(res, err)
	sent := colleagueAnswer{owed: true, kind: kind, sent: content}
	answer := a2a.Answer{
		ChannelID: ask.ChannelID, Sender: by.handle, Content: content,
		// AND WHAT THE RUN'S SESSION LOST, beside the answer and never in the
		// colleague's words: an agent asking is told what a person in the
		// thread is (conversationnotice.go), its detail left out.
		NotContinued: withoutDetail(by.notContinued),
		// Raw, back to the model that asked it ([sandbox.ColleagueAsk.RawQuestion]).
		Question: ask.RawQuestion(),
		AskTrace: events.TraceContext{
			TraceID: ask.TraceID, SpanID: ask.SpanID, ParentSpanID: ask.ParentSpanID,
		},
		// Unchanged, not incremented: the ask is the delegation and this is
		// that hop completing; charging the return leg spends the cap where
		// nobody meant to (a2a.Service.Reply).
		DelegationDepth: by.depth,
		DelegationChain: ask.Chain,
		// The answering RUN, as the asking side stamps it
		// (internal/agent/builtin/a2a.go): it is what the reply's audit record
		// is attributed to and the parent of what it wakes, and a work key
		// cannot say which attempt answered. ADR-0017.
		TurnID:  by.runID,
		WorkKey: by.workKey,
		// Whoever the asker worked for: the turn this answer wakes works for
		// them too, never for this seat.
		WorkingFor: ask.WorkingFor,
	}
	if c.Org != nil {
		if seat := c.Org.AgentSeatByHandle(by.handle); seat != nil {
			// The role name, so the asker's prompt can say who answered, not
			// only which handle.
			answer.SenderRole = seat.Name
		}
	}

	if replyErr := svc.Reply(ctx, answer); replyErr != nil {
		// A closed channel is ordinary, logged at info: the asker's turn was
		// superseded, the sweep reaped the exchange, or a repeat of the ending
		// frame after a crash finds the channel its first attempt closed.
		if errors.Is(replyErr, a2a.ErrClosed) {
			sent.closed = true
			sent.replied = e.answeredBefore(ctx, ask.ChannelID)
			log.InfoContext(ctx, "a2a_answer_channel_closed", "seat", by.handle,
				"channel_id", ask.ChannelID, "answered_before", sent.replied)
			return sent
		}
		log.WarnContext(ctx, "a2a_answer_failed", "seat", by.handle,
			"channel_id", ask.ChannelID, "error", replyErr.Error())
		return sent
	}
	sent.replied = true

	// One ask, one answer, then closed here rather than by the idle sweep,
	// as the answer wake is documented to land on a closed channel. After the
	// reply, never before: Reply refuses a closed channel.
	if closeErr := svc.Close(ctx, a2a.Closure{
		ChannelID: ask.ChannelID,
		// This seat closed it: an unset ClosedBy reads as "system", the
		// sweep's wording.
		ClosedBy: by.handle,
		TurnID:   by.runID, WorkKey: by.workKey,
	}); closeErr != nil {
		log.WarnContext(ctx, "a2a_channel_close_failed", "seat", by.handle,
			"channel_id", ask.ChannelID, "error", closeErr.Error(),
			"detail", "the exchange was answered; the idle sweep closes it later")
	}
	return sent
}

// answeredBefore reports whether a channel found closed while answering already
// held an answer. A channel carries only the ask and its answer
// ([a2a.Channel.Messages]), and only the asked seat answers, so two messages
// mean an earlier attempt at the same ending replied and closed it before a
// crash; the ask alone means it closed unanswered (the asker gave up, or the
// sweep). An unreadable channel is logged and taken for unanswered.
func (e *Engine) answeredBefore(ctx context.Context, channelID string) bool {
	if e.backends == nil || e.backends.Fleet == nil {
		return false
	}
	ch, err := a2a.NewCoordStore(e.backends.Fleet).Get(ctx, channelID)
	if err != nil {
		log.WarnContext(ctx, "a2a_channel_unread", "channel_id", channelID, "error", err.Error(),
			"detail", "the closed channel could not be read to say whether an earlier attempt "+
				"answered it, so the turn's end says the answer did not reach the asker")
		return false
	}
	return ch.Messages >= 2
}

// answerLostTurn tells the colleague whose ask a lost run's turn took up that
// the turn was lost: the coordinator's [sandbox.CoordinatorOptions.TurnLost], a
// step of the run's ending on whichever node finishes it.
//
// From the row alone, as [Engine.closeStoppedTurn] works: the ask and the
// loss's reason ([sandbox.RecordedEnding]) are the run's, its frames gone, and
// this node's company is the one whose directory reaches the asker now. Best
// effort like every answer: a refused reply costs the asker's wake, never the
// ending; a repeat after a crash finds the channel closed. It returns how the
// turn ended (lost, whether its reply reached anybody per [sandbox.RunCalls],
// what the colleague was told) for the run's row: no frame is left to record it.
func (e *Engine) answerLostTurn(ctx context.Context, run sandbox.PendingRun,
	bridged sandbox.RunCalls) executorkeep.Ending {

	company := e.Company()
	var answered colleagueAnswer
	switch {
	case run.ColleagueAsk != nil && company == nil:
		log.WarnContext(ctx, "a2a_lost_turn_unanswered", "seat", run.AgentHandle,
			"turn_id", run.TurnID, "channel_id", run.ColleagueAsk.ChannelID,
			"detail", "the run that would have answered a colleague's ask was lost on a node "+
				"running no company, so there is no directory to reach the asker through")
		answered = colleagueAnswer{owed: true, kind: "no_service"}
	case run.ColleagueAsk != nil:
		lost := &runLostError{}
		if run.Ending != nil {
			lost.reason, lost.detail = run.Ending.Reason, run.Ending.Detail
		}
		answered = e.answerColleague(ctx, company, answeringRun(run), run.ColleagueAsk, turn.Result{}, lost)
	}
	return e.runEnding(ctx, company, run, bridged, answered)
}

// runLostError is a turn ended by the loss of its executor's run: the reason
// the ending recorded and the sentence it announced ([sandbox.RecordedEnding]).
// A type, as [stopError] is, so [answerContent] builds the asker's answer from
// its fields rather than from an error chain's accumulated wording.
type runLostError struct{ reason, detail string }

func (e *runLostError) Error() string {
	return fmt.Sprintf("the executor's launch was lost (%s): %s", e.reason, e.detail)
}

// lostRunOf recovers the loss that ended a turn from its error.
func lostRunOf(err error) (*runLostError, bool) {
	var lost *runLostError
	if errors.As(err, &lost) {
		return lost, true
	}
	return nil, false
}

// withoutDetail is verdict as a conversation is told it: the engine's sentence
// says it, and the detail stays on the turn's page; nil for none.
func withoutDetail(verdict *executorkeep.NotContinued) *executorkeep.NotContinued {
	if verdict == nil {
		return nil
	}
	told := *verdict
	told.Detail = ""
	return &told
}

// answerContent is what the asker is told, and its kind in one word for the
// turn's end: `artifact`, `evidence`, `stopped`, `lost`, `guard` or `unproduced`.
//
// Only a `done` turn produced something for a colleague. Other decisions carry
// an artifact meaning something else: `skipped` holds the executor's private
// reasoning that nobody asked, a guard breach or failure a fragment of
// working-out. An honest "I could not" beats silence, which the idle sweep ends
// without waking the asker. A stopped turn says a person stopped it, not "ask
// again with more detail"; a lost one gives the run ending's reason and
// sentence, redacted as the announcement is because it can quote the run.
func answerContent(res turn.Result, err error) (string, string) {
	if turn.Stopped(err) {
		return "I could not answer this: a person stopped my turn before it finished. " +
			"Take it up with them, or on a shared surface.", "stopped"
	}
	if lost, ok := lostRunOf(err); ok {
		return "I could not answer this: my executor's work on it in its box was lost " +
			"before the turn could finish (" + lost.reason + ": " + redact.Secrets(lost.detail) +
			"). Please ask again, or take it up on a shared surface.", "lost"
	}
	if res.Decision == phase.Done && res.Artifact != "" {
		return res.Artifact, "artifact"
	}
	// A blocked `done` turn may carry no artifact, but `submit_work` refuses a
	// blocked outcome with empty `evidence`, so its evidence answers instead.
	// The apology is no cosmetic loss: the caller closes the channel next, so
	// the asker hears "nothing" with nowhere to follow up, and re-asks, the
	// duplicate this path exists to stop.
	if res.Decision == phase.Done && res.LastWork != nil &&
		res.LastWork.Outcome == turn.OutcomeBlocked &&
		strings.TrimSpace(res.LastWork.Evidence) != "" {
		return res.LastWork.Evidence, "evidence"
	}
	if res.Breach != nil {
		return "I could not answer this: my turn stopped on the engine's " +
			string(res.Breach.Kind) + " guard. Please ask again more narrowly, " +
			"or take it up on a shared surface.", "guard"
	}
	return "I could not produce an answer for this (" + res.Decision.String() +
		"). Please ask again with more detail, or take it up on a shared surface.", "unproduced"
}

// askChannel is the channel an ask opened, and so where its answer goes.
//
// Off the typed payload: the wake is built with events.New over
// [types.A2ARequest], so the envelope's Payload bag is empty, and reading it
// leaves every real ask unaddressable (a2a_answer_unaddressable).
func askChannel(ask *events.Event) string {
	if req, ok := events.DataAs[*types.A2ARequest](ask); ok {
		return req.ChannelID
	}
	return ""
}

// askedQuestion is the brief the asker sent, echoed back so the woken turn has
// the context its own turn ended with (a2a.Answer.Question). Off the typed
// payload, for the reason [askChannel] gives.
func askedQuestion(ask *events.Event) string {
	if req, ok := events.DataAs[*types.A2ARequest](ask); ok {
		return req.Content
	}
	return ""
}
