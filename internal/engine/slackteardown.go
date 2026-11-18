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

// What a Slack disconnect takes away beyond the access sync's work. The sync
// has turned every seat off and revoked its bot token and, when the accounts
// go, deleted each agent's app and cleared its sealed values once Slack
// confirmed ([Engine.tearDownSeatAccess]), refusing the disconnect while a
// removal waits. What is left is each agent's `integrations.slack` block,
// naming an app that no longer exists.
//
// One entity-route write per seat, so N seats are N revisions (fine for a
// rare, deliberate disconnect): a merge patch on `roles` replaces the whole
// list, and seats nest in `units:` at any depth.
func (e *Engine) tearDownSlack(ctx context.Context, removeSeats bool, by iam.Actor) error {
	// Kept when the operator did not ask for the accounts: the apps and
	// their OAuth clients stay, so a reconnect is just an install.
	if !removeSeats {
		return nil
	}
	company := e.Company()
	if company == nil || company.Config == nil {
		// [Engine.dropBlock] guards this before any vendor step; answered
		// rather than dereferenced, since that guard is in another file.
		return fmt.Errorf("%w: this node has no active company revision",
			integration.ErrDisconnectUnavailable)
	}
	for seat := range company.Org.AllRoles() {
		if seat.Slack.IsZero() {
			continue
		}
		// Every step is "remove this if it is there", so a failure goes
		// back to the loop to retry and seats already cleared stay so.
		if err := e.clearSlackSeat(ctx, seat.Handle(), by); err != nil {
			return err
		}
	}
	return nil
}

// clearSlackSeat removes one seat's whole `integrations.slack` block, not its
// fields: a seat left with `slack: {enabled: true}` would have the next
// connect create an app nobody chose. Recorded as by's, whoever asked for the
// disconnect ([integration.State.RequestedBy]).
func (e *Engine) clearSlackSeat(ctx context.Context, handle string, by iam.Actor) error {
	writer := e.configWriterOrNil()
	if writer == nil {
		// The sentinel, wrapped, for [Engine.editGitHubSeat]'s reason: the
		// loop keys on errors.Is and leaves the row for a node with a
		// config surface to finish.
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
		// Already gone: what a retry finds after a later seat failed.
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
// surface's has. A type rather than a branch in [vendorDisconnect.Disconnect],
// whose branch is for a surface with no teardown: folded in there, the next
// kind with no vendor pass would inherit a Slack teardown. The shared seam
// also routes it through [Engine.forgetRemoved].
type slackTeardown struct{ engine *Engine }

// Teardown reports nothing removed: every credential the agents' apps held
// is the access sync's to clear, and it already has.
func (s slackTeardown) Teardown(
	ctx context.Context, in setup.TeardownInput,
) (provision.Removed, error) {
	return provision.Removed{}, s.engine.tearDownSlack(ctx, in.RemoveSeats, in.By)
}
