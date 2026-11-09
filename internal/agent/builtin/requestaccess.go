package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// RequestAccessTool asks a person for one grant from a closed list (ADR-0118).
const RequestAccessTool = toolname.RequestAccess

// MaxAccessReasonBytes bounds the reason, a decision's rationale.
const MaxAccessReasonBytes = decision.MaxRationale

// declineOption is the option that grants nothing.
const declineOption = "decline"

// requestAccess asks for access as a decision on a work item: one option per
// grant, named by its catalogue key so the change is built by the engine for
// this seat alone, and one that declines. It posts through comment_on_work_item,
// so the item's gates and the ask's routing are that tool's, unchanged.
type requestAccess struct{ comment *commentOnWorkItem }

var _ tools.SeatCallable = (*requestAccess)(nil)

func (t *requestAccess) Name() string { return RequestAccessTool }

func (t *requestAccess) Description() string {
	return "Ask a person for one more kind of access your work needs, on the work item " +
		"that needs it: " + accessList() + ". The person sees the exact change to your " +
		"access and decides; nothing changes until they apply it, and you are told your " +
		"new access on your next turn. Ask an administrator, who can grant it, and say " +
		"in `why` what you tried and what the access is for."
}

func (t *requestAccess) Parameters() map[string]any {
	keys := make([]any, 0, len(advisor.AccessKeys()))
	for _, k := range advisor.AccessKeys() {
		keys = append(keys, string(k))
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{"type": "string",
				"description": "The work item that needs the access. " + itemRef},
			"ask": map[string]any{"type": "string",
				"description": "The handle of the person to ask: an administrator."},
			"access": map[string]any{"type": "string", "enum": keys,
				"description": "The one access to ask for."},
			"why": map[string]any{"type": "string",
				"description": fmt.Sprintf("What you tried and what the access is for, in at "+
					"most %d bytes. It is the person's reason to say yes.", MaxAccessReasonBytes)},
		},
		"required": []any{"item", "ask", "access", "why"},
	}
}

func (t *requestAccess) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *requestAccess) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if turn == nil || turn.Seat == nil || turn.Org == nil {
		return notInATurn(RequestAccessTool), nil
	}
	key := advisor.AccessKey(strings.TrimSpace(argString(args, "access")))
	person := strings.TrimSpace(argString(args, "ask"))
	why := strings.TrimSpace(argString(args, "why"))
	switch err := advisor.CanAsk(turn.Seat, key); {
	case errors.Is(err, advisor.ErrAccessUnknown):
		return failed(RequestAccessTool + ": `access` is one of " + accessList() + "."), nil
	case errors.Is(err, advisor.ErrAccessHeld):
		return failed(RequestAccessTool + ": you can already " + key.Does() +
			"; read Your access."), nil
	case errors.Is(err, advisor.ErrAccessNotTakable):
		return failed(RequestAccessTool + ": you have no GitHub App to add this to; ask " +
			"for it in words with " + CommentOnWorkTool + "."), nil
	}
	switch asked := turn.Org.Role(person); {
	case asked == nil || asked.Kind != org.KindHuman:
		return failed(RequestAccessTool + ": `ask` is a person's handle: only a person " +
			"decides what an agent may do."), nil
	case why == "":
		return failed(RequestAccessTool + " needs `why`: what you tried and what the access is for."), nil
	case len(why) > MaxAccessReasonBytes:
		return failed(fmt.Sprintf("%s: `why` is %d bytes; it can be at most %d.",
			RequestAccessTool, len(why), MaxAccessReasonBytes)), nil
	}
	name := turn.Seat.Name
	question := fmt.Sprintf("Allow %s to %s?", name, key.Does())
	// THE ENGINE'S OWN OPTIONS, the one path a grant's reserved id is admitted on.
	return t.comment.CallForTurn(decision.WithEngineOptions(ctx), turn, map[string]any{
		"item": argString(args, "item"), "body": why, "ask": person, "asks": true,
		"decision": map[string]any{
			"question": question, "role": string(decision.RoleApprover),
			"recommended": string(key), "rationale": why,
			"options": []any{
				map[string]any{"id": string(key), "label": "Allow it",
					"detail": fmt.Sprintf("%s can %s. You review the exact change first.", name, key.Does())},
				map[string]any{"id": declineOption, "label": "Not now",
					"detail": "Nothing changes, and the agent is told."},
			},
		},
	})
}

// accessList is the catalogue's keys, as the alternatives a sentence offers.
func accessList() string {
	parts := make([]words.Said, 0, len(advisor.AccessKeys()))
	for _, k := range advisor.AccessKeys() {
		parts = append(parts, words.Said(fmt.Sprintf("`%s` (%s)", k, k.Does())))
	}
	return words.Or(parts)
}

// grantChosen refuses an answer that chooses a grant its asker does not hold
// yet: the grant is saved first and only then answered (ADR-0118), whoever sends it.
func (d WorkDeps) grantChosen(ctx context.Context, choice string,
	thread worklet.ResolvedThread) *tools.Result {

	if thread.AnswersDecision == nil || !advisor.IsAccessOption(choice) {
		return nil
	}
	key := advisor.AccessKey(choice)
	if d.FleetSeat == nil {
		refused := failed(CommentOnWorkTool + ": this server cannot confirm the grant, so " +
			"the answer was not posted. Answer from the dashboard, which applies it first.")
		return &refused
	}
	seat, err := d.FleetSeat(ctx, thread.AnsweredAuthor)
	if err != nil {
		refused := failed(fmt.Sprintf("%s: the grant could not be confirmed (%v), so the "+
			"answer was not posted. Try again in a moment.", CommentOnWorkTool, err))
		return &refused
	}
	if !errors.Is(advisor.CanAsk(seat, key), advisor.ErrAccessHeld) {
		refused := failed(fmt.Sprintf("%s: %s cannot %s yet. Apply the grant first, then "+
			"answer: the agent is told yes only once it holds the access.",
			CommentOnWorkTool, thread.AnsweredAuthor, key.Does()))
		return &refused
	}
	return nil
}
