package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// request_access's second form: one permission at an integration beside the
// agent's level, from the integration's live catalogue (a Cloudflare
// permission group). The engine stores the request, posts it as a decision
// whose grant options carry the request's id, and a person answers by
// previewing and saving the grant; the agent is told its new access on its
// next turn, and its credential keeps its value.

// GrantDesk is the engine's half of an agent's request for one grant and of
// its answer.
type GrantDesk interface {
	// Offer is the seat's setting on lane in the running company document,
	// and what lane offers now, its grant catalogue read live.
	Offer(ctx context.Context, handle string, lane seataccess.Lane) (seataccess.Desired, seataccess.Options, error)

	// File stores a request about to be asked, and records that it was.
	File(ctx context.Context, req seataccess.GrantRequest) error

	// Withdraw drops a request whose question could not be posted.
	Withdraw(ctx context.Context, id string) error

	// Granted reports whether the fleet's active revision holds a live
	// grant answering request id for the seat handle, for good where
	// permanent and lapsing otherwise.
	Granted(ctx context.Context, handle, id string, permanent bool) (bool, error)

	// Declined drops request id and records that by declined it; a request
	// already gone is the outcome asked for.
	Declined(ctx context.Context, id, by string) error
}

// requestGrant is request_access's integration form.
func (t *requestAccess) requestGrant(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	desk := t.comment.deps.Grants
	lane := seataccess.Lane(strings.TrimSpace(argString(args, "integration")))
	permission := strings.TrimSpace(argString(args, "permission"))
	scope := argStrings(args, "scope")
	hours := argInt(args, "hours", seataccess.DefaultGrantHours)
	person := strings.TrimSpace(argString(args, "ask"))
	why := strings.TrimSpace(argString(args, "why"))
	handle := turn.Seat.Handle()
	switch {
	case desk == nil:
		return failed(RequestAccessTool + ": this server cannot ask for an integration's permission. " +
			outsideList), nil
	case !slices.Contains(seataccess.GrantLanes(), lane):
		return failed(fmt.Sprintf("%s: `integration` is one of %s, the integrations that grant one "+
			"permission beside an agent's level. %s", RequestAccessTool, grantLaneList(), outsideList)), nil
	case permission == "":
		return failed(RequestAccessTool + " needs `permission`: the permission you lacked, as the " +
			"integration's catalogue names it."), nil
	case hours < seataccess.MinGrantHours || hours > seataccess.MaxGrantHours:
		return failed(fmt.Sprintf("%s: `hours` is %d; ask for %d to %d.", RequestAccessTool, hours,
			seataccess.MinGrantHours, seataccess.MaxGrantHours)), nil
	}
	if refused := askable(turn, person, why); refused != nil {
		return *refused, nil
	}
	want, opts, err := desk.Offer(ctx, handle, lane)
	if err != nil {
		return failed(fmt.Sprintf("%s: %s's catalogue could not be read just now (%v); try again in "+
			"a minute.", RequestAccessTool, seataccess.LaneName(lane), err)), nil
	}
	name := seataccess.LaneName(lane)
	entry, listed := opts.Grantable(permission)
	asked := seataccess.Grant{Permission: permission, Scope: scope}
	switch {
	case opts.Managed == seataccess.ManagedExternal:
		return failed(fmt.Sprintf("%s: your company manages agents' %s access outside Crewlet, so it "+
			"grants no permission here. Ask an administrator in words with %s.", RequestAccessTool, name,
			CommentOnWorkTool)), nil
	case opts.Grants == nil:
		return failed(fmt.Sprintf("%s: %s's catalogue could not be read just now; try again in a minute.",
			RequestAccessTool, name)), nil
	case !want.Enabled:
		return failed(fmt.Sprintf("%s: you have no %s access to add a permission to; an administrator "+
			"turns it on under %s. Ask for it in words with %s, naming that place.", RequestAccessTool,
			name, seataccess.GrantPlace(lane), CommentOnWorkTool)), nil
	case !listed:
		return failed(fmt.Sprintf("%s: %s offers no permission %q an agent can be granted.%s Permissions "+
			"that manage credentials, members or billing are never granted to an agent.",
			RequestAccessTool, name, permission, nearest(opts, permission))), nil
	case slices.ContainsFunc(seataccess.LiveGrants(want.Grants, time.Now()), func(g seataccess.Grant) bool {
		return seataccess.GrantKey(g) == seataccess.GrantKey(asked)
	}):
		return failed(fmt.Sprintf("%s: you already hold %s there; read Your access.", RequestAccessTool,
			entry.Name)), nil
	}
	check := opts
	if entry.Unavailable != "" {
		// STILL ASKED, saying what the provisioner must hold first: the
		// person who grants it is the one who can add it at the vendor.
		i := slices.IndexFunc(check.Grants, func(e seataccess.GrantEntry) bool { return e.ID == permission })
		check.Grants = slices.Clone(check.Grants)
		check.Grants[i].Unavailable = ""
	}
	if refusal := grantRefusal(check, want.Level, asked); refusal != "" {
		return failed(RequestAccessTool + ": " + refusal + "."), nil
	}
	req := seataccess.GrantRequest{
		ID: seataccess.NewRequestID(), Kind: lane.Kind(), Lane: lane, Handle: handle,
		Permission: permission, Scope: scope, Hours: hours, Reason: why,
		Item: argString(args, "item"), Ask: person, AskedAt: time.Now().UTC(),
	}
	if ferr := desk.File(ctx, req); ferr != nil {
		return failed(fmt.Sprintf("%s: the request could not be stored (%v); try again in a moment.",
			RequestAccessTool, ferr)), nil
	}
	res, _, err := tools.Invoke(decision.WithEngineOptions(ctx), tools.Invocation{
		Name: CommentOnWorkTool, Tool: t.comment,
		Args:   tools.ArgsOf(t.comment, false, grantDecision(turn.Seat, req, entry, name)),
		AsSeat: true, Turn: turn, Where: RequestAccessTool,
	})
	if err != nil || res.Failed {
		// NOTHING ASKED, so nothing kept: a stored request nobody can answer
		// would only wait out its month.
		if werr := desk.Withdraw(context.WithoutCancel(ctx), req.ID); werr != nil {
			err = errors.Join(err, werr)
		}
	}
	return res, err
}

// grantRefusal is why the catalogue would not give the grant asked, "" where
// it would ([seataccess.CheckGrant]).
func grantRefusal(opts seataccess.Options, level string, g seataccess.Grant) string {
	if err := seataccess.CheckGrant(opts, level, g); err != nil {
		return err.Error()
	}
	return ""
}

// askable refuses an ask of anyone but a person, or one with no reason or
// too long a one, as both forms of the tool do.
func askable(turn *turnctx.Turn, person, why string) *tools.Result {
	var refused tools.Result
	switch asked := turn.Org.Role(person); {
	case asked == nil || asked.Kind != org.KindHuman:
		refused = failed(RequestAccessTool + ": `ask` is a person's handle: only a person " +
			"decides what an agent may do.")
	case why == "":
		refused = failed(RequestAccessTool + " needs `why`: what you tried and what the access is for.")
	case len(why) > MaxAccessReasonBytes:
		refused = failed(fmt.Sprintf("%s: `why` is %d bytes; it can be at most %d.",
			RequestAccessTool, len(why), MaxAccessReasonBytes))
	default:
		return nil
	}
	return &refused
}

// grantDecision is the request as the engine's own decision: grant for this
// change (recommended), for good, or not now. The question and the options
// are the engine's words; the agent's reason is the rationale.
func grantDecision(seat *org.Role, req seataccess.GrantRequest, entry seataccess.GrantEntry,
	vendor string) map[string]any {

	reach := grantReach(entry, req.Scope)
	question := fmt.Sprintf("Grant %s %s on %s at %s?", seat.Name, entry.Name, reach, vendor)
	note := ""
	if entry.Write {
		note += " It can change things there, so granting it asks you to sign in again."
	}
	if entry.Unavailable != "" {
		note += fmt.Sprintf(" %s must first give the company's own credential %s.", vendor,
			words.And(entry.Missing))
	}
	return map[string]any{
		"item": req.Item, "body": req.Reason, "ask": req.Ask, "asks": true,
		"decision": map[string]any{
			"question": question, "role": string(decision.RoleApprover),
			"recommended": seataccess.GrantOptionTemporary(req.ID), "rationale": req.Reason,
			"options": []any{
				map[string]any{"id": seataccess.GrantOptionTemporary(req.ID),
					"label": fmt.Sprintf("Grant for this change (expires in %d hours)", req.Hours),
					"detail": fmt.Sprintf("%s can use %s on %s for %d hours from when you grant it. You "+
						"review the exact change first.%s", seat.Name, entry.Name, reach, req.Hours, note)},
				map[string]any{"id": seataccess.GrantOptionPermanent(req.ID),
					"label": "Add to this agent permanently",
					"detail": fmt.Sprintf("%s keeps %s on %s until somebody removes it on the Agents tab. "+
						"You review the exact change first.%s", seat.Name, entry.Name, reach, note)},
				map[string]any{"id": declineOption, "label": "Not now",
					"detail": "Nothing changes, and the agent is told."},
			},
		},
	}
}

// grantReach is where a grant would apply, as the question says it.
func grantReach(entry seataccess.GrantEntry, scope []string) string {
	switch {
	case len(scope) > 0:
		return strings.Join(scope, ", ")
	case entry.ScopeKind == seataccess.GrantScopeAccount:
		return "the account"
	default:
		return "every " + string(entry.ScopeKind) + " the company's agents reach"
	}
}

// grantLaneList is the lanes an agent may ask a grant on, for a sentence.
func grantLaneList() string {
	parts := make([]words.Said, 0, len(seataccess.GrantLanes()))
	for _, lane := range seataccess.GrantLanes() {
		parts = append(parts, words.Said("`"+string(lane)+"`"))
	}
	return words.Or(parts)
}

// nearest is up to five catalogue permissions sharing a word with the one
// asked for, as a sentence, or "" where none does.
func nearest(opts seataccess.Options, asked string) string {
	want := strings.Fields(strings.ToLower(asked))
	var found []words.Said
	for _, e := range opts.Grants {
		have := strings.ToLower(e.ID)
		if slices.ContainsFunc(want, func(w string) bool { return len(w) > 3 && strings.Contains(have, w) }) {
			found = append(found, words.Said("`"+e.ID+"`"))
		}
		if len(found) == 5 {
			break
		}
	}
	if len(found) == 0 {
		return ""
	}
	return " It offers " + words.And(found) + "."
}

// grantRequestChosen refuses an answer choosing a request's grant before the
// fleet's active revision holds it for the asking agent: the grant is saved
// first and only then answered (ADR-0118), whoever sends it. Nil where the
// choice is no request's grant.
func (d WorkDeps) grantRequestChosen(ctx context.Context, choice string,
	thread worklet.ResolvedThread) *tools.Result {

	id, permanent, ok := seataccess.GrantOption(choice)
	if !ok || thread.AnswersDecision == nil {
		return nil
	}
	if d.Grants == nil {
		refused := failed(CommentOnWorkTool + ": this server cannot confirm the grant, so the answer " +
			"was not posted. Answer from the dashboard, which saves it first.")
		return &refused
	}
	held, err := d.Grants.Granted(ctx, thread.AnsweredAuthor, id, permanent)
	switch {
	case err != nil:
		refused := failed(fmt.Sprintf("%s: the grant could not be confirmed (%v), so the answer was "+
			"not posted. Try again in a moment.", CommentOnWorkTool, err))
		return &refused
	case !held:
		refused := failed(fmt.Sprintf("%s: %s does not hold the grant yet. Save it first, then answer: "+
			"the agent is told yes only once it holds it.", CommentOnWorkTool, thread.AnsweredAuthor))
		return &refused
	}
	return nil
}

// requestDeclined settles the request a posted "Not now" answered, recording
// who declined it. A failure is logged: the answer is posted, and the sweep
// drops the request once it lapses.
func (d WorkDeps) requestDeclined(ctx context.Context, choice, by string, thread worklet.ResolvedThread) {
	if choice != declineOption || thread.AnswersDecision == nil || d.Grants == nil {
		return
	}
	for _, option := range thread.AnswersDecision.Options {
		if id, _, ok := seataccess.GrantOption(option.ID); ok {
			if err := d.Grants.Declined(context.WithoutCancel(ctx), id, by); err != nil {
				log.WarnContext(ctx, "grant_request_unsettled", "request", id, "error", err.Error())
			}
			return
		}
	}
}
