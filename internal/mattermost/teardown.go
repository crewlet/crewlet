package mattermost

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
)

// TeardownOptions is what removing a Mattermost integration needs.
type TeardownOptions struct {
	Client *Client
	Config *config.Mattermost
	// Plan names the seats, the same way [Options] does, so a teardown
	// reaches exactly the bots a pass created.
	Plan *provision.Plan
	// RemoveSeats disables the bots this engine created.
	RemoveSeats bool
}

// Teardown removes what this engine created in a Mattermost instance.
//
// NO WEBHOOKS TO WITHDRAW, and that is the whole difference from the other
// third-party apps. Mattermost holds an outbound websocket per seat and verifies no
// inbound delivery, so this engine registers nothing at the instance that
// would outlive a disconnect. Dropping the block is enough to stop every
// socket.
//
// What is left is the bots, and they go only when asked — this is what the
// console's checkbox means here.
//
// DISABLED RATHER THAN DELETED, which is not a weaker teardown but the
// correct one: deleting a Mattermost user takes its POSTS with it, so a
// disconnect would silently rewrite the history of every channel the seat
// ever spoke in. A disabled bot keeps what it said and can say nothing more,
// which is what decommissioning a colleague actually means. The same
// reasoning [Client.DisableBot] already carries.
//
// SAFE TO REPEAT: a bot already gone, or already disabled, is not an error.
func Teardown(ctx context.Context, opts TeardownOptions) error {
	if opts.Client == nil {
		return errors.New("mattermost: no client")
	}
	if !opts.RemoveSeats || opts.Config == nil || opts.Config.Provisioning == nil {
		return nil
	}
	if opts.Plan == nil {
		return nil
	}

	var failures []error
	for _, seat := range opts.Plan.Seats {
		username := BotUsername(opts.Config.Provisioning, seat.Handle)
		bot, found, err := opts.Client.BotByUsername(ctx, username)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"mattermost: look up %s to disable it: %w", username, err))
			continue
		}
		if !found {
			continue
		}
		if err := opts.Client.DisableBot(ctx, bot.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"mattermost: disable %s: %w", username, err))
		}
	}
	return errors.Join(failures...)
}
