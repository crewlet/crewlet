package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// What a Slack disconnect takes away beyond the access sync's work.
//
// The access sync has already turned every seat off and revoked its bot token,
// and, where the accounts are to be removed, deleted each agent's app with the
// app-configuration token and cleared its sealed values once Slack confirmed
// the deletion ([Engine.tearDownSeatAccess]); it refuses the disconnect while
// a removal still waits. What is left is the document: each agent's
// `integrations.slack` block, naming an app that no longer exists.
//
// A seat is addressed through the entity route because a merge patch on
// `roles` replaces the whole list, and seats nest inside `units:` to any
// depth, so N seats are N revisions. A disconnect is a thing an operator does
// rarely and deliberately.
func (e *Engine) tearDownSlack(ctx context.Context, removeSeats bool, by iam.Actor) error {
	// KEPT, NOT CLEARED, when the operator did not ask for the accounts:
	// the apps and their OAuth clients stay, so a reconnect turns the
	// agents back on with an install.
	if !removeSeats {
		return nil
	}
	company := e.Company()
	if company == nil || company.Config == nil {
		// UNREACHABLE THROUGH THE LOOP, which guards this in
		// [Engine.dropBlock] before any vendor step runs. Answered
		// anyway rather than dereferenced, because this is a method on
		// the engine and the guard is in another file.
		return fmt.Errorf("%w: this node has no active company revision",
			integration.ErrDisconnectUnavailable)
	}
	for seat := range company.Org.AllRoles() {
		if seat.Slack.IsZero() {
			continue
		}
		// EVERY STEP IS "REMOVE THIS IF IT IS THERE", so a failure is
		// returned for the loop to retry, and the seats cleared before it
		// stay cleared.
		if err := e.clearSlackSeat(ctx, seat.Handle(), by); err != nil {
			return err
		}
	}
	return nil
}

// clearSlackSeat removes one seat's whole `integrations.slack` block.
//
// THE WHOLE BLOCK, not the fields inside it: a seat left holding
// `slack: {enabled: true}` after its app was deleted would ask the next
// connect to create an app nobody chose again.
//
// RECORDED AS BY'S, whoever asked for the disconnect — see
// [integration.State.RequestedBy].
func (e *Engine) clearSlackSeat(ctx context.Context, handle string, by iam.Actor) error {
	writer := e.configWriterOrNil()
	if writer == nil {
		// THE SENTINEL, WRAPPED WITH WHAT IT IS REFUSING, for the reason
		// [Engine.editGitHubSeat] gives: the loop keys on errors.Is and
		// leaves the row alone, so a node that has a config surface
		// finishes the disconnect.
		return fmt.Errorf(
			"%w: no config surface is wired on this node, so %s's Slack "+
				"block cannot be removed", integration.ErrDisconnectUnavailable, handle)
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
		"disconnect slack: remove "+handle+"'s Slack block", by)
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

// Teardown reports nothing removed: every credential the agents' apps held
// is the access sync's to clear, and it already has.
func (s slackTeardown) Teardown(
	ctx context.Context, in setup.TeardownInput,
) (provision.Removed, error) {
	return provision.Removed{}, s.engine.tearDownSlack(ctx, in.RemoveSeats, in.By)
}
