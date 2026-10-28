package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// execute sends an approved (or retried) server write and settles it.
//
// SENT DETACHED FROM THE APPROVING REQUEST, so a tab closed mid-write never
// interrupts it, and bounded by the exec grant's own expiry, which is the
// longest the write could be admitted for anyway. The settlement and the
// turn's resumption are written on the same detached context, because a write
// that landed and was never recorded is exactly the proposal that reads as
// unknown, and bounded by [SettleTimeout], so a store that hangs releases the
// approving request rather than pinning it for ever.
func (a *Approver) execute(ctx context.Context, d decider, p assistantdomain.Proposal,
	from wire.ProposalState, read exec.Grant) (wire.ProposalAnswer, error) {

	step := "execute"
	if from == wire.ProposalUnknown {
		step = "retry"
	}
	if _, err := a.move(ctx, d.call(step), p.ID, assistantdomain.ProposalMove{
		From: from, To: wire.ProposalExecuting}); err != nil {
		a.d.Grants.Revoke(read.ID())
		return wire.ProposalAnswer{}, err
	}
	request := proposal.RequestOf(p)
	ctx = context.WithoutCancel(ctx)
	grant, err := a.d.Grants.Exec(ctx, Exec{Conversation: p.Conversation, Turn: p.Turn,
		Proposal: p.ID, Act: p.Act, Request: request})
	var out outcome
	if err != nil {
		// NOTHING WAS SENT: the person's session no longer derives a grant,
		// so the write failed for certain and is not offered a Retry.
		out = refused(wire.Refusal{Code: CodeNotAvailable,
			Message: "Your session can no longer carry this change, so nothing was sent."})
	} else {
		sendCtx, cancel := context.WithDeadline(ctx, grant.Expires())
		resp, sendErr := a.d.Dispatcher.Do(sendCtx, grant, request)
		cancel()
		a.d.Grants.Revoke(grant.ID())
		out = outcomeOf(p.Tool, request, resp, sendErr)
		out.invalidate = a.invalidation(out)
	}
	settleCtx, cancel := context.WithTimeout(ctx, SettleTimeout)
	defer cancel()
	return a.settle(settleCtx, d, p, out, read)
}

// SettleTimeout bounds a sent write's settlement and the turn's resumption
// together: three of the state log's ten-second resolve budgets, for the
// settle record, the resume record and the card the resumed turn shows. Past
// it the approval answers what it could record, and a settlement that did not
// land reads as unknown, which offers the same write again under its own key.
const SettleTimeout = 30 * time.Second

// CodeNotAvailable is the refusal a write meets when the person's session can
// no longer derive the grant it would be sent under: the API's
// `assistant_not_available`.
const CodeNotAvailable = "assistant_not_available"

// settle records what a sent write's request answered and hands it to the
// turn.
func (a *Approver) settle(ctx context.Context, d decider, p assistantdomain.Proposal,
	out outcome, read exec.Grant) (wire.ProposalAnswer, error) {

	move := assistantdomain.ProposalMove{From: wire.ProposalExecuting, To: out.state}
	step := "unknown"
	if out.state != wire.ProposalUnknown {
		step = "settle"
		move.Outcome, move.Position = out.outcome, out.position
		move.Receipt, move.Failure = out.receipt, out.failure
		if out.failure != nil {
			move.RefusalCode = out.failure.Refusal.Code
		}
	}
	if _, err := a.move(ctx, d.call(step), p.ID, move); err != nil {
		// The write's own outcome stands; its settlement record is what is
		// missing, and the proposal reads as unknown until a Retry, whose
		// operation the ledger answers with this write.
		a.d.Grants.Revoke(read.ID())
		log.WarnContext(ctx, "assistant_proposal_settlement_not_recorded",
			"proposal", p.ID, "state", string(out.state), "error", err)
		return wire.ProposalAnswer{}, err
	}
	card := proposal.CardAt(p, out.state, out.receipt, out.failure)
	if out.state == wire.ProposalUnknown {
		// THE TURN WAITS FOR THE PERSON'S RETRY: a model told "it may or may
		// not have happened" can only guess, and the card already offers the
		// one safe next step.
		a.d.Grants.Revoke(read.ID())
		at := a.emit(ctx, d.person.ID, p, Emission{Card: &card})
		return answer(p, wire.ProposalUnknown, at), nil
	}
	at := a.resume(ctx, Resumption{Owner: d.person.ID, Page: d.page, Conversation: p.Conversation,
		Turn: p.Turn, Proposal: p.ID, Read: read, State: out.state,
		Receipt: out.receipt, Failure: out.failure, Answer: out.answer, Card: card,
		Invalidate: out.invalidate})
	return answer(p, out.state, at), nil
}

// outcome is what a sent write's request answered, in the proposal's terms.
type outcome struct {
	state    wire.ProposalState
	outcome  string
	position string
	receipt  *wire.Receipt
	failure  *wire.Failure
	answer   json.RawMessage

	invalidate *wire.Invalidate
}

// refused is a write the engine refused.
func refused(r wire.Refusal) outcome {
	return outcome{state: wire.ProposalFailed, failure: &wire.Failure{Refusal: r}}
}

// actAnswer is the slice of the act route's answer a settlement reads: the
// three-valued outcome and where the write landed.
type actAnswer struct {
	Outcome  string  `json:"outcome"`
	Position *string `json:"position"`
	Receipt  struct {
		// Key is the task a write is about, as every task receipt names it
		// (internal/agent/builtin's receiptItem).
		Key string `json:"key"`
	} `json:"receipt"`
}

// outcomeOf reads a sent write's answer.
//
// THREE ANSWERS AND NO GUESS: a 2xx is applied (an act answer saying `unknown`
// is unknown, which is the act route's own third outcome); a 4xx is the
// engine's refusal, in its own code and words; and a request that could not
// be sent, a 5xx, or an answer cut short is UNKNOWN, offered a Retry under the
// same operation key, because whether the write landed is exactly what nobody
// can say.
func outcomeOf(tool string, request exec.Request, resp exec.Response, err error) outcome {
	switch {
	case err != nil, resp.Truncated, resp.Status >= 500, resp.Status == 0:
		return outcome{state: wire.ProposalUnknown}
	case resp.Status >= 400:
		out := refused(refusalOf(request, resp))
		// THE MODEL READS THE ROUTE'S WHOLE REFUSAL, fenced as the API's
		// answer beside the resumed result, so it can say what to change;
		// the card carries the person's sentence ([cards.PersonRefusal]).
		if json.Valid(resp.Body) && len(bytes.TrimSpace(resp.Body)) > 0 {
			out.answer = json.RawMessage(resp.Body)
		}
		return out
	}
	out := outcome{state: wire.ProposalApplied, outcome: "applied"}
	summary := "Applied"
	if json.Valid(resp.Body) && len(bytes.TrimSpace(resp.Body)) > 0 {
		out.answer = json.RawMessage(resp.Body)
		var stated actAnswer
		if json.Unmarshal(resp.Body, &stated) == nil {
			switch stated.Outcome {
			case "applied", "pending":
				out.outcome = stated.Outcome
			case "":
			default:
				return outcome{state: wire.ProposalUnknown}
			}
			if stated.Position != nil && *stated.Position != "" {
				out.position = *stated.Position
			}
			summary = receiptSummary(tool, stated.Receipt.Key)
		}
	}
	out.receipt = &wire.Receipt{Summary: summary, Position: out.position}
	return out
}

// invalidation is what open screens are told of a write that landed, or nil
// for one that did not.
//
// EVERY APPLIED WRITE, not only one whose answer named a position: a route
// outside the act surface (a brand adopted, a seat changed through the
// configuration) answers none, and the page behind the drawer read "No brand
// yet" after the brand was adopted until it was reloaded. With no position
// the screens read again without a floor to wait for. With one, its domain is
// named BY ITS NAME (`tracker`), the key a screen's floors are kept under,
// never by its stream (`CREWLET_TRACKER_LOG`), which no screen reads by.
func (a *Approver) invalidation(out outcome) *wire.Invalidate {
	if out.state != wire.ProposalApplied {
		return nil
	}
	at := &wire.Invalidate{Position: out.position}
	if stream, _, found := strings.Cut(out.position, "@"); found && stream != "" {
		at.Domain = a.d.DomainOf(stream)
	}
	return at
}

// refusalOf is a refused write as the person and the model read it: the route's
// own refusal, except where the route judged the precondition the proposal
// itself carried ([proposal.Revision.Precondition]) and found it no longer
// holds. That `412` is about the moments between the approval's re-read and
// the write, so it says what happened in the proposal's terms (the record
// changed, or what the change would have created now exists) rather than in
// headers the person never saw, under [CodeStale], the code a proposal found
// stale before it was sent carries. A `428` is a route that takes the change
// only with the version it was based on, which the preview's read did not
// answer.
//
// EVERY OTHER REFUSAL IS THE ROUTE'S, and a person reads it as the engine's
// sentence for its kind ([cards.PersonRefusal]) with the next step the
// resumed turn takes: the route's own account names fields, tools and routes,
// and it is the model's, which is handed the whole answer ([outcomeOf]).
func refusalOf(request exec.Request, resp exec.Response) wire.Refusal {
	switch {
	case resp.Status == http.StatusPreconditionFailed && request.IfNoneMatch != "":
		return wire.Refusal{Code: CodeStale, Message: "What this change would have created " +
			"was created by somebody else after it was proposed, so it was not applied.",
			Hint: "Ask for it again to see what is there now."}
	case resp.Status == http.StatusPreconditionFailed && request.IfMatch != "":
		return wire.Refusal{Code: CodeStale, Message: "The record changed after this " +
			"change was proposed, so it was not applied.",
			Hint: "Ask for it again to see it as it is now."}
	case resp.Status == http.StatusPreconditionRequired:
		return wire.Refusal{Code: exec.RefusalOf(resp).Code, Message: "This change is taken only with the " +
			"version it was based on, and that version could not be read, so it was not " +
			"applied.", Hint: "Ask for it again."}
	}
	route := exec.RefusalOf(resp)
	shown := cards.PersonRefusal(route, resp.Status)
	shown.Hint = cards.RefusalHint(route.Code)
	return shown
}

// receiptSummary is an applied write's receipt in a line, naming the task it
// is about where the act tool's own receipt names one: "Created ENG-156" for
// a create, "Applied to ENG-156" for any other write to a task, and "Applied"
// for a write that names none.
func receiptSummary(tool, key string) string {
	switch {
	case key == "":
		return "Applied"
	case strings.HasPrefix(tool, "create_"):
		return "Created " + key
	}
	return "Applied to " + key
}
