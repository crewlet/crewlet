package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/seataccess"
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
		"that needs it: " + accessList() + " with `access`; or, with `integration` and " +
		"`permission`, one permission at an integration beside your level there, as its " +
		"catalogue names it, on the entries in `scope` or its whole reach, granted for `hours` " +
		"or for good. When an integration refuses you for a permission your credential lacks (" +
		grantLaneTerms(func(w seataccess.GrantTerms) string { return w.Refusal }) + "), ask for " +
		"that permission here; never ask a person to change your credential at the vendor by " +
		"hand. Your access is your Crewlet grant, which an " +
		"administrator sets in Crewlet, never at the vendor. The person sees the exact " +
		"change to your access and decides; nothing changes until they apply it, and you " +
		"are told your new access on your next turn. Ask an administrator, who can grant " +
		"it, and say in `why` what you tried and what the access is for. " + outsideList
}

// outsideList is what a seat does for access neither form names.
const outsideList = "For access neither form names (a level or a scope), ask an " +
	"administrator in words with " + CommentOnWorkTool + ", naming where they set it: " +
	"Settings, Integrations, then the integration's Agents tab."

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
				"description": "The one access to ask for. Leave it out to ask for an integration's permission."},
			"integration": map[string]any{"type": "string",
				"description": "The integration whose permission you lack: one of " + grantLaneList() + "."},
			"permission": map[string]any{"type": "string",
				"description": "The one permission to ask for at `integration`, as its catalogue names it: " +
					grantLaneTerms(func(w seataccess.GrantTerms) string { return w.Permission }) + "."},
			"scope": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Where the permission applies: " +
					grantLaneTerms(func(w seataccess.GrantTerms) string { return w.Scope }) +
					". Leave it out for the permission's whole reach."},
			"hours": map[string]any{"type": "integer", "minimum": seataccess.MinGrantHours,
				"maximum": seataccess.MaxGrantHours,
				"description": fmt.Sprintf("How long the change needs it, %d to %d hours; %d when left out. "+
					"The person may grant it for good instead, except a permission granted for one change, "+
					"which lasts %d hour when left out and never past its own bound.", seataccess.MinGrantHours,
					seataccess.MaxGrantHours, seataccess.DefaultGrantHours, seataccess.MinGrantHours)},
			"target": map[string]any{"type": "string",
				"description": "What the one change you need it for changes, as the integration names it, " +
					"such as a bucket's name. Required for a permission granted for one change; the person " +
					"and your access are told it."},
			"why": map[string]any{"type": "string",
				"description": fmt.Sprintf("What you tried and what the access is for, in at "+
					"most %d bytes. It is the person's reason to say yes.", MaxAccessReasonBytes)},
		},
		"required": []any{"item", "ask", "why"},
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
	integration := strings.TrimSpace(argString(args, "integration"))
	switch access := strings.TrimSpace(argString(args, "access")); {
	case access != "" && integration != "":
		return failed(RequestAccessTool + ": give `access` or `integration` with `permission`, not both."), nil
	case integration != "":
		return t.requestGrant(ctx, turn, args)
	}
	key := advisor.AccessKey(strings.TrimSpace(argString(args, "access")))
	person := strings.TrimSpace(argString(args, "ask"))
	why := strings.TrimSpace(argString(args, "why"))
	switch err := advisor.CanAsk(turn.Seat, key); {
	case errors.Is(err, advisor.ErrAccessUnknown):
		return failed(RequestAccessTool + ": `access` is one of " + accessList() + ". " +
			outsideList), nil
	case errors.Is(err, advisor.ErrAccessHeld):
		return failed(RequestAccessTool + ": you can already " + key.Does() +
			"; read Your access."), nil
	case errors.Is(err, advisor.ErrAccessNotTakable):
		return failed(RequestAccessTool + ": you have no GitHub App to add this to, which " +
			"an administrator turns on for you under " + key.Place() + "; ask for it in " +
			"words with " + CommentOnWorkTool + ", naming that place."), nil
	}
	if refused := askable(turn, person, why); refused != nil {
		return *refused, nil
	}
	name := turn.Seat.Name
	question := fmt.Sprintf("Allow %s to %s?", name, key.Does())
	// THE ENGINE'S OWN OPTIONS, the one path a grant's reserved id is admitted on.
	posted := map[string]any{
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
	}
	res, _, err := tools.Invoke(decision.WithEngineOptions(ctx), tools.Invocation{
		Name: CommentOnWorkTool, Tool: t.comment, Args: tools.ArgsOf(t.comment, false, posted),
		AsSeat: true, Turn: turn, Where: RequestAccessTool,
	})
	return res, err
}

// accessList is the catalogue's keys, as the alternatives a sentence offers.
func accessList() string {
	parts := make([]words.Said, 0, len(advisor.AccessKeys()))
	for _, k := range advisor.AccessKeys() {
		parts = append(parts, words.Said(fmt.Sprintf("`%s` (%s)", k, k.Does())))
	}
	return words.Or(parts)
}

// grantChosen decides an answer choosing a grant (ADR-0118): a request's option
// is saved by choosing it; an access key's is refused until its asker holds it.
func (d WorkDeps) grantChosen(ctx context.Context, choice string,
	thread worklet.ResolvedThread) *tools.Result {

	if refused := d.grantRequestChosen(ctx, choice, thread); refused != nil {
		return refused
	}
	if _, _, request := seataccess.GrantOption(choice); request || thread.AnswersDecision == nil ||
		!strings.HasPrefix(choice, advisor.AccessPrefix) {
		return nil
	}
	key := advisor.AccessKey(choice)
	if !key.Valid() {
		refused := failed(fmt.Sprintf("%s: %q is a grant Crewlet no longer offers, so the "+
			"answer was not posted and the agent was not told yes. Answer \"Not now\" and let "+
			"the agent ask again, which offers the grants there are now; an administrator "+
			"grants anything else under Settings, Integrations, then the integration's Agents tab.",
			CommentOnWorkTool, choice))
		return &refused
	}
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
