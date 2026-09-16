package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// What a Slack disconnect takes away, which for a long time was nothing.
//
// # "No pass" was read as "nothing to tear down"
//
// Slack is the one surface with no reconcile pass: its apps are created from
// the command line or by hand, one per agent, from a manifest. That was read
// as "this engine put nothing at Slack", and for the APPS it is true — only a
// person holding an app-configuration token can delete one. It was never true
// of the rest: each seat carries an `integrations.slack` block naming two
// sealed credentials, and a disconnect that dropped the company block alone
// left both exactly where they were.
//
// What an operator saw was a card stuck on PAUSED. The company block was
// gone, so nothing was enabled; the seats' signing secrets were still there,
// so a row was still emitted; and a row that exists with nothing enabled is
// what the screen draws as paused. Measured twice in a row — because
// disconnecting again is what a person does when the first one appears not to
// have worked — and both attempts logged `accounts=[] secrets=[]`.
//
// # Deleting the sealed copy loses nothing here
//
// [provision.Removed] is careful that a seat is named only where its
// credential is genuinely dead, because deleting the company's record of a
// LIVE secret is worse than keeping a dead one. Slack meets that bar by a
// different route from every other surface: the bot token and signing secret
// are on the app's own settings page permanently — Slack shows them on every
// visit, unlike Datadog's application key, which is shown once and never
// again — so a value deleted here can be read back by the person who owns the
// app. What cannot be read back is which `${VAR}` a seat's credential lived
// in, and that is in the report below.
//
// # Every seat is its own write
//
// A seat is addressed through the entity route because a merge patch on
// `roles` replaces the whole list, and seats nest inside `units:` to any
// depth — so N seats are N revisions, each advancing the epoch. That is the
// same cost [Engine.editGitHubSeat] already pays per seat on the GitHub pass,
// and a disconnect is a thing an operator does rarely and deliberately.
func (e *Engine) tearDownSlack(
	ctx context.Context, removeSeats bool,
) (provision.Removed, error) {
	var out provision.Removed
	// KEPT, NOT REMOVED, when the operator did not ask for the accounts.
	//
	// The apps go on working and the seats go on holding their credentials;
	// what the disconnect does is drop the company block, which retires the
	// transport. Reporting removals here would delete the sealed values
	// behind apps this call deliberately left running.
	if !removeSeats {
		return out, nil
	}
	company := e.Company()
	if company == nil || company.Config == nil {
		// UNREACHABLE THROUGH THE LOOP, which guards this in
		// [Engine.dropBlock] before any vendor step runs. Answered
		// anyway rather than dereferenced, because this is a method on
		// the engine and the guard is in another file.
		return out, fmt.Errorf("%w: this node has no active company revision",
			integration.ErrDisconnectUnavailable)
	}
	// WHICH APP EACH AGENT IS, learned from the running transport, because a
	// Slack app is named nowhere in the company document: the app issues the
	// token rather than the other way round. Empty where no seat came up,
	// which is a report without the app ids rather than no report.
	apps := e.SlackApps()
	for role := range company.Config.EachRole() {
		block := role.Integrations.Slack
		if block == nil {
			continue
		}
		handle := role.Seat().Handle()
		removal := provision.Removal{
			Handle: handle,
			Role:   role.Name,
			// THE APP ID, because it is the only thing that identifies one
			// agent's app: two agents may carry the same display name, and
			// nothing on the seat names the app at all. It is also what the
			// operator's remaining work is addressed by — the delete page is
			// keyed on it — so an empty one is a report that says which seats
			// held an app without saying which app.
			Account: apps[handle],
		}
		// NAMES ONLY, AND ONLY WHOLE REFERENCES. A literal in the document is
		// somebody's own value written by hand: there is no variable to
		// delete, and reading one as a name would hand the sealed store a key
		// made of a credential.
		for _, ref := range []string{block.BotToken, block.SigningSecret} {
			if name, whole := envref.Whole(ref); whole {
				removal.Secrets = append(removal.Secrets, name)
			}
		}
		out.Add(removal)
		if err := e.clearSlackSeat(ctx, handle); err != nil {
			// WHAT IS ALREADY DONE IS STILL REPORTED. The loop holds the
			// surface in PhaseDisconnecting and tries again, and every step
			// here is "remove this if it is there" — but the seats cleared
			// before the failure have credentials nothing points at any
			// more, and [Engine.forgetRemoved] is the only thing that
			// deletes them.
			return out, err
		}
	}
	return out, nil
}

// clearSlackSeat removes one seat's whole `integrations.slack` block.
//
// THE WHOLE BLOCK, not the two credentials inside it. A block is refused at
// load unless it carries both ([config.RoleSlack.validate]), so a block
// stripped of its credentials is a document that will not load at all, and an
// empty `slack: {}` left behind would make a disconnected seat read as one
// whose app is merely unconfigured.
func (e *Engine) clearSlackSeat(ctx context.Context, handle string) error {
	writer := e.configWriterOrNil()
	if writer == nil {
		// THE SENTINEL, WRAPPED WITH WHAT IT IS REFUSING, for the reason
		// [Engine.editGitHubSeat] gives: the loop keys on errors.Is and
		// leaves the row alone, so a node that has a config surface
		// finishes the disconnect.
		return fmt.Errorf(
			"%w: no config surface is wired on this node, so %s's Slack "+
				"credentials cannot be removed", integration.ErrDisconnectUnavailable, handle)
	}
	body, err := writer.Seat(ctx, handle)
	if err != nil {
		return fmt.Errorf("engine: read the seat %s: %w", handle, err)
	}
	var role map[string]any
	if decodeErr := json.Unmarshal(body, &role); decodeErr != nil {
		return fmt.Errorf("engine: decode the seat %s: %w", handle, decodeErr)
	}
	integrations, _ := role["integrations"].(map[string]any)
	if _, held := integrations["slack"]; !held {
		// ALREADY GONE, which is what a retry finds, and a retry is how
		// this runs whenever a later seat failed. Every step here is
		// "remove this if it is there".
		return nil
	}
	delete(integrations, "slack")
	role["integrations"] = integrations
	updated, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("engine: encode the seat %s: %w", handle, err)
	}
	return writer.SetSeat(ctx, handle, updated,
		"disconnect slack: remove "+handle+"'s app credentials", "reconcile loop")
}

// slackTeardown is Slack's disconnect step, in the shape every other
// surface's already has.
//
// A TYPE RATHER THAN A BRANCH in [vendorDisconnect.Disconnect]. That branch
// existed for "a surface with no teardown", and folding Slack's work into it
// would have made the one case it covers indistinguishable from the surface
// it was written for: the next kind with no vendor pass would inherit a Slack
// teardown. Here the work joins the same seam as the other seven, which is
// also what routes it through [Engine.forgetRemoved] without a second call
// site knowing to.
type slackTeardown struct{ engine *Engine }

func (s slackTeardown) Teardown(
	ctx context.Context, in setup.TeardownInput,
) (provision.Removed, error) {
	return s.engine.tearDownSlack(ctx, in.RemoveSeats)
}
