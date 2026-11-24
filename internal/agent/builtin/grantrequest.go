package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// request_access's second form: one permission beside the agent's level, from
// the integration's live catalogue. The engine stores the request and posts it
// as a decision whose grant options carry its id; a person answers by saving
// the grant, the agent learns its new access next turn, and its credential
// keeps its value.

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

	// Grant saves request id's grant for asker from the stored request (ADR-0118),
	// after admit(raise) admits it. A lapsed request is [seataccess.ErrNoRequest];
	// one it cannot save is a [*seataccess.GrantRefused].
	Grant(ctx context.Context, asker, id string, permanent bool, admit func(raise bool) error) error
}

// requestGrant is request_access's integration form.
func (t *requestAccess) requestGrant(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	desk := t.comment.deps.Grants
	lane := seataccess.Lane(strings.TrimSpace(argString(args, "integration")))
	permission := strings.TrimSpace(argString(args, "permission"))
	scope := argStrings(args, "scope")
	hours, hoursGiven := argIntValue(args["hours"])
	target := strings.TrimSpace(argString(args, "target"))
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
	case hoursGiven && (hours < seataccess.MinGrantHours || hours > seataccess.MaxGrantHours):
		return failed(fmt.Sprintf("%s: `hours` is %d; ask for %d to %d.", RequestAccessTool, hours,
			seataccess.MinGrantHours, seataccess.MaxGrantHours)), nil
	case target != "" && !config.SeatGrantTarget(target):
		return failed(RequestAccessTool + ": `target` is one resource's name as the integration " +
			"names it, with no space or quote, such as a bucket's name."), nil
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
	asked := seataccess.Grant{Permission: permission, Scope: scope, Target: target}
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
	case entry.OneChange() && target == "":
		return failed(fmt.Sprintf("%s: %s is granted for one change; name what that change changes in "+
			"`target` (a bucket's name, say) and the change itself in `why`.", RequestAccessTool,
			entry.Name)), nil
	case entry.OneChange() && hours > entry.MaxHours:
		return failed(fmt.Sprintf("%s: %s is granted for one change, for at most %d hours; ask for %d "+
			"to %d.", RequestAccessTool, entry.Name, entry.MaxHours, seataccess.MinGrantHours,
			entry.MaxHours)), nil
	}
	switch {
	case hoursGiven:
	case entry.OneChange():
		hours = seataccess.MinGrantHours
	default:
		hours = seataccess.DefaultGrantHours
	}
	check := opts
	if entry.Unavailable != "" {
		// STILL ASKED, naming what the provisioner must hold first: the person
		// who grants it can add it at the vendor.
		i := slices.IndexFunc(check.Grants, func(e seataccess.GrantEntry) bool { return e.ID == permission })
		check.Grants = slices.Clone(check.Grants)
		check.Grants[i].Unavailable = ""
	}
	asked.ExpiresAt = time.Now().UTC().Add(time.Duration(hours) * time.Hour)
	if refusal := grantRefusal(check, want.Level, asked); refusal != "" {
		return failed(RequestAccessTool + ": " + refusal + "."), nil
	}
	req := seataccess.GrantRequest{
		ID: seataccess.NewRequestID(), Kind: lane.Kind(), Lane: lane, Handle: handle,
		Permission: permission, Scope: scope, Hours: hours, Reason: why, Target: target,
		Item: argString(args, "item"), Ask: person, AskedAt: time.Now().UTC(),
	}
	if size := req.StoredBytes(); size > coord.MaxGrantRequestBytes {
		return failed(fmt.Sprintf("%s: the request is %d bytes as it is kept, past the %d one request "+
			"holds: name fewer `scope` entries, or say `why` in fewer words.", RequestAccessTool, size,
			coord.MaxGrantRequestBytes)), nil
	}
	if ferr := desk.File(ctx, req); ferr != nil {
		return failed(fmt.Sprintf("%s: the request could not be stored (%v); try again in a moment.",
			RequestAccessTool, ferr)), nil
	}
	res, _, err := tools.Invoke(decision.WithEngineOptions(ctx), tools.Invocation{
		Name: CommentOnWorkTool, Tool: t.comment,
		Args:   tools.ArgsOf(t.comment, false, grantAsk(turn.Seat, req, entry)),
		AsSeat: true, Turn: turn, Where: RequestAccessTool,
	})
	if err != nil || res.Failed {
		// Nothing asked, so nothing kept: a stored request nobody can answer
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

// grantAsk is the comment_on_work_item arguments that post req as the
// engine's own decision ([seataccess.RequestAsk]), in the wire form the tool reads.
func grantAsk(seat *org.Role, req seataccess.GrantRequest, entry seataccess.GrantEntry) map[string]any {
	body, asked := seataccess.RequestAsk(seat.Name, req, entry)
	options := make([]any, 0, len(asked.Options))
	for _, o := range asked.Options {
		options = append(options, map[string]any{"id": o.ID, "label": o.Label, "detail": o.Detail})
	}
	return map[string]any{"item": req.Item, "body": body, "ask": req.Ask, "asks": true,
		"decision": map[string]any{"question": asked.Question, "role": string(asked.Role),
			"recommended": asked.Recommended, "rationale": asked.Rationale, "options": options}}
}

// grantLaneTerms is one sentence part per lane an agent may ask a grant on:
// the lane's name and what of its [seataccess.GrantTerms] part picks, each
// lane whose part is empty left out.
func grantLaneTerms(part func(seataccess.GrantTerms) string) string {
	var out []string
	for _, lane := range seataccess.GrantLanes() {
		if text := part(seataccess.GrantTermsOf(lane)); text != "" {
			out = append(out, seataccess.LaneName(lane)+": "+text)
		}
	}
	return strings.Join(out, "; ")
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

// grantRequestChosen makes choosing a request's grant option the approval
// (ADR-0118): decided as setup.access.write (and .raise for a raise), never a
// seat's, then saved, then answered. Nil once the grant is held.
func (d WorkDeps) grantRequestChosen(ctx context.Context, choice string,
	thread worklet.ResolvedThread) *tools.Result {

	id, permanent, ok := seataccess.GrantOption(choice)
	if !ok || thread.AnswersDecision == nil {
		return nil
	}
	if d.Grants == nil {
		refused := failed(CommentOnWorkTool + ": this server cannot save the grant, so the answer " +
			"was not posted. Answer from the dashboard of a node that serves the setup surface.")
		return &refused
	}
	held, err := d.Grants.Granted(ctx, thread.AnsweredAuthor, id, permanent)
	switch {
	case err != nil:
		refused := failed(fmt.Sprintf("%s: the grant could not be confirmed (%v), so the answer was "+
			"not posted. Try again in a moment.", CommentOnWorkTool, err))
		return &refused
	case held:
		return nil
	}
	// The write first, before the save reads anything; the raise once it is judged.
	if err = d.authorizeGrant(ctx, authz.ActionSetupAccessWrite); err != nil {
		refusal := authorityRefusal(string(authz.ActionSetupAccessWrite), err)
		return &refusal
	}
	err = d.Grants.Grant(ctx, thread.AnsweredAuthor, id, permanent, func(raise bool) error {
		if !raise {
			return nil
		}
		return d.authorizeGrant(ctx, authz.ActionSetupAccessRaise)
	})
	var refusedGrant *seataccess.GrantRefused
	switch {
	case err == nil:
	case errors.Is(err, ErrRefused) || errors.Is(err, ErrUndecidable) || errors.Is(err, ErrNoAuthorizer):
		refusal := authorityRefusal(string(authz.ActionSetupAccessRaise), err)
		return &refusal
	case errors.Is(err, seataccess.ErrNoRequest):
		refused := failed(fmt.Sprintf("%s: the agent's request %s has lapsed or was already settled, "+
			"so nothing was granted and the answer was not posted. Answer \"Not now\", and the agent "+
			"asks again if it still needs it.", CommentOnWorkTool, id))
		return &refused
	case errors.As(err, &refusedGrant):
		refused := failed(fmt.Sprintf("%s: the grant was not saved, so the answer was not posted: %s.",
			CommentOnWorkTool, refusedGrant.Reason))
		return &refused
	default:
		refused := failed(fmt.Sprintf("%s: the grant could not be saved (%v), so the answer was not "+
			"posted. Try again in a moment.", CommentOnWorkTool, err))
		return &refused
	}
	// The answer waits until this node serves the revision that holds the grant.
	if held, err = d.Grants.Granted(ctx, thread.AnsweredAuthor, id, permanent); err != nil || !held {
		refused := failed(fmt.Sprintf("%s: the grant was saved, and this node does not serve its "+
			"revision yet, so the answer was not posted. Send the same answer again in a moment; "+
			"the grant is not saved twice.", CommentOnWorkTool))
		return &refused
	}
	return nil
}

// authorizeGrant decides one access verb of the caller.
func (d WorkDeps) authorizeGrant(ctx context.Context, action authz.Action) error {
	if d.Authorize == nil {
		return ErrNoAuthorizer
	}
	return d.Authorize(ctx, action, authz.Object{Kind: authz.KindCompany})
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
