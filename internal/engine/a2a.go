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

// The answer leg of an agent-to-agent ask.
//
// a2a.Service.Reply was written, documented and tested, and had no caller
// anywhere outside the suite: an ask woke its target, the target ran a whole
// turn, and the answer went nowhere. The asker — told by a2a_ask to finish
// its turn rather than wait — was never woken, and the terminal state of
// every exchange was the maintenance sweep closing an idle channel an hour
// later.
//
// FROM EVERY FRAME THAT ENDS THE TURN, because each holds BOTH halves: the
// turn's result, and the ask it answers. The dispatch frame holds the ask as
// its trigger, and ends a turn that never parked. A turn that launches its
// executor parks on the run instead, and is ended by a frame that never saw
// the trigger — its resume off the seat's control topic, or a person's stop of
// the run — so the ask travels on the run's record ([sandbox.ColleagueAsk]),
// written by the launch, and that frame answers from there. Answered from the
// dispatch frame alone, every ask an agent seat took up went unanswered: every
// such turn launches its executor.
//
// AND FROM THE RUN'S LOSS, which ends a parked turn with no frame of it at all:
// the box gone or unreadable, the suspension never recorded, the claim
// stranded, the tail abandoned by a node that died, the seat retired. The
// coordinator decides that ending on the run's record and hands the run to
// [Engine.answerLostTurn] as a step of it, so the asker is told the turn was
// lost, and why, by whichever node finishes the ending. Left to the idle sweep
// instead, the asker heard nothing, ever: a swept close wakes nobody.

// askOf returns the a2a request this turn is answering, or nil.
//
// The FIRST one, and normally the only one: an a2a wake carries its own
// conversation key, so a partition holds one ask. A batch that somehow held
// two would be two questions with one answer, and replying to the first is
// the honest half of that rather than replying to both with the same text.
func askOf(evs []*events.Event) *events.Event {
	for _, ev := range evs {
		if ev != nil && ev.Type == types.A2ARequestType {
			return ev
		}
	}
	return nil
}

// colleagueAskOf is the ask an a2a request asks, as the turn answering it
// carries it — on its frame, and on its executor's run record — and nil for no
// ask.
func colleagueAskOf(ask *events.Event) *sandbox.ColleagueAsk {
	if ask == nil {
		return nil
	}
	return &sandbox.ColleagueAsk{
		ChannelID: askChannel(ask), Question: askedQuestion(ask),
		Chain:   slices.Clone(ask.DelegationChain),
		TraceID: ask.TraceID, SpanID: ask.SpanID, ParentSpanID: ask.ParentSpanID,
		WorkingFor: answeredFor(ask),
	}
}

// answeredFor is who the answer to ask is worked for back on the asker
// ([sandbox.ColleagueAsk.WorkingFor]): whoever the ask carried, which the
// answering turn worked for too, or the asking seat itself where the ask
// carried nobody, so the turn the answer wakes is the asker's own work as its
// ask was, and never the answering colleague's. The engine's, from the ask
// the engine built, never from anything the answering turn wrote.
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
	// depth is the turn's delegation depth, which the reply carries
	// unchanged.
	depth int
}

// answeringOf is the turn a dispatch runs, as a reply names it.
func answeringOf(req Request) answeringTurn {
	return answeringTurn{handle: req.Handle, runID: req.RunID, workKey: req.WorkKey, depth: req.Depth}
}

// answeringRun is the turn a run's record belongs to, as a reply names it.
func answeringRun(run sandbox.PendingRun) answeringTurn {
	return answeringTurn{handle: run.AgentHandle, runID: run.TurnID, workKey: run.WorkKey,
		depth: run.DelegationDepth}
}

// colleagueAnswer is what the engine answered a colleague's ask with, for the
// record of how the turn that answered it ended (executorkeep.Ending's
// Answer and Delivered): the turn's own record of its calls cannot say, since
// the engine sent it, not the turn.
type colleagueAnswer struct {
	// owed says the turn answered an ask: it was woken by one and ended.
	owed bool
	// kind is what the answer was, in one word ([answerContent]), or why
	// none was sent: `unaddressable` (the ask named no channel),
	// `no_service` (this node runs no agent-to-agent service).
	kind string
	// sent is the text sent, where one was.
	sent string
	// replied says the colleague has an answer of this turn's: this reply
	// reached the channel, or — closed, a channel already closed — an
	// earlier attempt at the same ending answered it before closing it
	// ([Engine.answeredBefore]). A channel closed with no answer on it —
	// the asker gave up, or the idle sweep reaped it — is closed and not
	// replied.
	replied, closed bool
}

// onto records the answer on an ending: what the engine sent, and whether the
// colleague waiting has it — which, for a turn that owed the engine's answer,
// is the whole of whether its reply was delivered, and known either way.
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

// answerColleague replies on the channel ask opened, with what the turn by
// concluded — res, and err where the turn ended on one — and closes it; nil
// ask answers nobody. It reports what it sent and whether the reply went, for
// the turn's end.
//
// Best effort in both directions, and deliberately so: the turn has already
// run and its side effects have already shipped, so a failure here must not
// turn a delivered turn into a failed one. What it costs is the asker's wake,
// which is what the idle sweep is the backstop for.
func (e *Engine) answerColleague(ctx context.Context, c *Company, by answeringTurn,
	ask *sandbox.ColleagueAsk, res turn.Result, err error,
) colleagueAnswer {
	if ask == nil {
		return colleagueAnswer{}
	}
	if res.Suspended {
		// The turn is parked on its executor's run and has not finished.
		// Answering now would send the asker an artifact from a turn that
		// is still going. The frame that ends it answers instead, from the
		// ask the launch wrote onto the run's record.
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
		Question: ask.Question,
		AskTrace: events.TraceContext{
			TraceID: ask.TraceID, SpanID: ask.SpanID, ParentSpanID: ask.ParentSpanID,
		},
		// UNCHANGED, not incremented. The ask is the delegation and this
		// is that hop completing; charging the return leg spends the cap
		// in the one direction nobody meant to — see a2a.Service.Reply.
		DelegationDepth: by.depth,
		DelegationChain: ask.Chain,
		// The ANSWERING turn is what produced this, so it is both what
		// the reply's audit record is attributed to and the parent of
		// whatever that reply wakes — and it is the RUN, matching what
		// the asking side stamps (internal/agent/builtin/a2a.go): a
		// parent pointer that named the unit of work could not say which
		// attempt at it actually answered. See ADR-0017.
		TurnID:  by.runID,
		WorkKey: by.workKey,
		// AND WHOEVER THE ASKER WORKED FOR, carried back: the turn this
		// answer wakes works for them too, never for this seat.
		WorkingFor: ask.WorkingFor,
	}
	if c.Org != nil {
		if seat := c.Org.AgentSeatByHandle(by.handle); seat != nil {
			// The ROLE name, so the asker's prompt can say who answered
			// rather than only which handle did.
			answer.SenderRole = seat.Name
		}
	}

	if replyErr := svc.Reply(ctx, answer); replyErr != nil {
		// A closed channel is an ordinary outcome rather than a fault:
		// the asker's turn may have been superseded, or the sweep may
		// have reaped a long exchange — or a repeat of the frame that
		// ends this turn, after a crash between the reply and its record
		// of the end, finds the channel its first attempt closed. Logged
		// at a level that says so.
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

	// ONE ASK, ONE ANSWER, THEN CLOSED — and closed HERE rather than left
	// to the idle sweep, because the answer wake is documented to land on
	// a channel that is already closed. Closing after the reply, never
	// before: Reply refuses a closed channel.
	if closeErr := svc.Close(ctx, a2a.Closure{
		ChannelID: ask.ChannelID,
		// THIS SEAT closed it, which is the one thing the close record
		// could never say: ClosedBy went unset on every path, so every
		// close this engine has ever published read as "system" — the
		// wording reserved for the sweep.
		ClosedBy: by.handle,
		TurnID:   by.runID, WorkKey: by.workKey,
	}); closeErr != nil {
		log.WarnContext(ctx, "a2a_channel_close_failed", "seat", by.handle,
			"channel_id", ask.ChannelID, "error", closeErr.Error(),
			"detail", "the exchange was answered; the idle sweep closes it later")
	}
	return sent
}

// answeredBefore reports whether a channel the engine found closed as it
// answered had an answer on it already: the ask and an answer are the whole of
// what crosses a channel ([a2a.Channel.Messages]), and the only party that
// answers is the asked seat, so two messages are this turn's answer, sent by an
// earlier attempt at the same ending that closed the channel after it — a
// repeat after a crash between the reply and the record of the turn's end. One
// closed with only the ask on it closed unanswered: the asker gave up on it, or
// the idle sweep found nothing would answer it. A channel that cannot be read
// is said, and taken for unanswered, which is what a reader is then told.
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
// the turn was lost — the coordinator's [sandbox.CoordinatorOptions.TurnLost],
// a step of the run's ending on whichever node finishes it.
//
// FROM THE ROW ALONE, as [Engine.closeStoppedTurn] closes a stopped turn: the
// ask is the run's ([sandbox.PendingRun.ColleagueAsk]) and so is the reason it
// was lost ([sandbox.RecordedEnding]), and the turn's own frames are long gone.
// Against the company this node runs, which is the one whose directory reaches
// the asker now.
//
// BEST EFFORT, like every answer: a reply the broker refuses costs the asker's
// wake, never the run's ending. A repeat of the step after a crash finds the
// channel its first attempt closed ([a2a.ErrClosed]).
//
// AND IT ANSWERS HOW THE TURN ENDED — lost, whether its reply reached anybody
// as the turn's own calls say ([sandbox.RunCalls]), and what the engine told
// the colleague — which the ending owes the executor log on the run's row and
// records there, where the turn's row on the log keeps it beside the session
// it kept. No frame of the turn is left to record it.
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
	return e.runEnding(company, run, bridged, answered)
}

// runLostError is a turn ended by the loss of its executor's run: the reason
// the ending recorded and the sentence it announced ([sandbox.RecordedEnding]).
//
// A TYPE rather than a formatted error, for the reason [stopError] is one: the
// answer the asker is sent is built from its fields ([answerContent]), not from
// whatever wording an error chain accumulates on its way there.
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

// answerContent is what the asker is told, which is NOT the artifact on every
// path.
//
// Only a turn that reached `done` produced something for a colleague. The
// other decisions each carry an artifact that means something else entirely,
// and forwarding it verbatim sends a colleague the wrong thing while looking
// like an answer:
//
//   - `skipped` puts the EXECUTOR'S OWN REASONING in the artifact: a private
//     "no one was asking this seat to do anything", which is both internal and
//     wrong, since somebody plainly was.
//   - a guard breach and a failure end the turn with whatever text was in
//     hand when it stopped, which is a fragment of working-out rather than an
//     answer to the question.
//
// Silence is worse than any of them: the asker waits on a channel the idle
// sweep closes with no explanation and without waking it, and a short honest
// "I could not" is something it can act on in the turn it is woken for.
//
// A TURN A PERSON STOPPED says so, from err: whatever decision its result
// carries is what the turn held when the stop reached it, and asking the
// colleague to ask again with more detail would put a person's decision down
// to the question.
//
// A TURN LOST WITH ITS RUN says that too, with the reason and the sentence the
// run's ending announced: the asker can do something with "its box could not
// be read back; the work it pushed, if any, is on its branch", and nothing with
// a request for more detail about a question nobody got to. The sentence is
// redacted as the announcement is, because it can quote what the run said.
//
// AND WHAT IT IS, in one word, for the turn's end: `artifact`, `evidence`,
// `stopped`, `lost`, `guard` or `unproduced`.
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
	// A DONE TURN THAT IS BLOCKED STILL HAS AN ACCOUNT, and it is the one
	// way `done` arrives carrying no prose: the seat answered as far as it
	// could, put the rest to somebody who has to answer it, and had nothing
	// left to write as an artifact.
	//
	// The evidence is there to be used. `submit_work` REFUSES a blocked
	// outcome with an empty `evidence` — what was tried, and what stopped
	// it — so the one case whose artifact may be empty is the one case
	// guaranteed to have something to say instead.
	//
	// Falling through to the apology below is not a cosmetic loss. The
	// caller CLOSES the channel immediately after this, so an asker whose
	// colleague did exactly the right thing is told the turn produced
	// nothing and then has nowhere to follow up — and it re-asks, which is
	// the duplicate this whole path exists to stop.
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

// askChannel is the channel an ask opened, and therefore where its answer goes.
//
// OFF THE TYPED PAYLOAD. [types.A2ARequest] carries the channel id as a field;
// the envelope's free-form Payload bag carries nothing at all, because the
// wake is built with events.New over the typed struct. Reading the bag — which
// is what this replaced — returned "" for every real ask, so answerColleague
// logged a2a_answer_unaddressable and returned: every colleague question ran a
// turn on the target seat and the answer never reached the asker, which the
// asker experiences as a colleague that never replied.
func askChannel(ask *events.Event) string {
	if req, ok := events.DataAs[*types.A2ARequest](ask); ok {
		return req.ChannelID
	}
	return ""
}

// askedQuestion is the brief the asker sent, echoed back so the woken turn
// has the context its own turn ended with. See a2a.Answer.Question.
//
// Off the typed payload for the reason [askChannel] gives: the bag read this
// replaced echoed an empty question on every answer.
func askedQuestion(ask *events.Event) string {
	if req, ok := events.DataAs[*types.A2ARequest](ask); ok {
		return req.Content
	}
	return ""
}
