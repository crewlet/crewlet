package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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
// Here, because here is the one frame that holds BOTH halves: the turn's
// result and the event that triggered it. Anywhere earlier there is no
// answer; anywhere later the trigger is gone.

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

// answerColleague replies on the channel this turn was asked on, and closes
// it.
//
// Best effort in both directions, and deliberately so: the turn has already
// run and its side effects have already shipped, so a failure here must not
// turn a delivered turn into a failed one. What it costs is the asker's wake,
// which is what the idle sweep is the backstop for.
func (e *Engine) answerColleague(ctx context.Context, c *Company, req Request, res turn.Result) {
	ask := askOf(req.Events)
	if ask == nil {
		return
	}
	if res.Suspended {
		// The turn is parked on a detached coding run and has not
		// finished. Answering now would send the asker an artifact from
		// a turn that is still going; the resumed turn comes back through
		// this same frame with the same trigger and answers then.
		return
	}
	channelID, _ := ask.Payload["channel_id"].(string)
	if channelID == "" {
		log.WarnContext(ctx, "a2a_answer_unaddressable", "seat", req.Handle,
			"detail", "the ask carries no channel id, so there is nowhere to reply")
		return
	}
	svc := e.a2aService(c)
	if svc == nil {
		return
	}

	answer := a2a.Answer{
		ChannelID: channelID, Sender: req.Handle, Content: answerContent(res),
		Question: askedQuestion(ask),
		CausedBy: ask,
		// UNCHANGED, not incremented. The ask is the delegation and this
		// is that hop completing; charging the return leg spends the cap
		// in the one direction nobody meant to — see a2a.Service.Reply.
		DelegationDepth: req.Depth,
		DelegationChain: ask.DelegationChain,
		// The ANSWERING turn is what produced this, so it is the parent
		// of whatever the reply wakes.
		ParentTurnID: req.WorkKey,
	}
	if c.Org != nil {
		if seat := c.Org.AgentSeatByHandle(req.Handle); seat != nil {
			// The ROLE name, so the asker's prompt can say who answered
			// rather than only which handle did.
			answer.SenderRole = seat.Name
		}
	}

	if err := svc.Reply(ctx, answer); err != nil {
		// A closed channel is an ordinary outcome rather than a fault:
		// the asker's turn may have been superseded, or the sweep may
		// have reaped a long exchange. Logged at a level that says so.
		if errors.Is(err, a2a.ErrClosed) {
			log.InfoContext(ctx, "a2a_answer_channel_closed", "seat", req.Handle,
				"channel_id", channelID)
			return
		}
		log.WarnContext(ctx, "a2a_answer_failed", "seat", req.Handle,
			"channel_id", channelID, "error", err.Error())
		return
	}

	// ONE ASK, ONE ANSWER, THEN CLOSED — and closed HERE rather than left
	// to the idle sweep, because the answer wake is documented to land on
	// a channel that is already closed. Closing after the reply, never
	// before: Reply refuses a closed channel.
	if err := svc.Close(ctx, channelID); err != nil {
		log.WarnContext(ctx, "a2a_channel_close_failed", "seat", req.Handle,
			"channel_id", channelID, "error", err.Error(),
			"detail", "the exchange was answered; the idle sweep closes it later")
	}
}

// answerContent is what the asker is told, which is NOT the artifact on every
// path.
//
// Only a turn that reached `done` produced something for a colleague. The
// other decisions each carry an artifact that means something else entirely,
// and forwarding it verbatim sends a colleague the wrong thing while looking
// like an answer:
//
//   - `skipped` puts the PLANNER'S REASONING in the artifact — a private "no
//     one was asking this seat to do anything", which is both internal and
//     wrong, since somebody plainly was.
//   - a guard breach and a failure end the turn with whatever text was in
//     hand when it stopped, which is a fragment of working-out rather than an
//     answer to the question.
//
// Silence is worse than any of them: the asker waits on a channel the sweep
// closes an hour later with no explanation, and a short honest "I could not"
// is something it can act on in the turn it is woken for.
func answerContent(res turn.Result) string {
	if res.Decision == phase.Done && res.Artifact != "" {
		return res.Artifact
	}
	if res.Breach != nil {
		return "I could not answer this: my turn stopped on the engine's " +
			string(res.Breach.Kind) + " guard. Please ask again more narrowly, " +
			"or take it up on a shared surface."
	}
	return "I could not produce an answer for this (" + res.Decision.String() +
		"). Please ask again with more detail, or take it up on a shared surface."
}

// askedQuestion is the brief the asker sent, echoed back so the woken turn
// has the context its own turn ended with. See a2a.Answer.Question.
func askedQuestion(ask *events.Event) string {
	content, _ := ask.Payload["content"].(string)
	return content
}
