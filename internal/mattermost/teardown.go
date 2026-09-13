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
// AND ITS TOKENS GO, which is what makes the disable stand up. This is the
// one place where disabling instead of deleting costs something: deleting an
// account takes its credentials with it, and disabling one leaves them
// sitting there. Mattermost refuses a deactivated account's token, so the
// credential looks dead, but it is only dormant: the account keeps its
// username, so re-enabling it, by this engine or by an administrator in the
// console, makes every token that was ever minted on it work again. A
// disconnect that answered "the accounts are removed" while leaving a live
// credential in the company's secret store, at an instance the operator has
// just disconnected from, is the strongest thing this teardown can get wrong.
//
// ONLY THE TOKENS THIS TOOL MINTED, matched on [TokenDescription], which is
// the same rule the reconcile retires under and the same rule GitLab's
// teardown matches hooks by: an administrator's own token on the account is
// not this engine's to take.
//
// SAFE TO REPEAT: a bot already gone, already disabled, or holding no token
// of ours, is not an error.
// # What it reports
//
// A seat whose bot is gone, or whose minted tokens were revoked AND whose bot
// was then disabled. NOT one whose revoke failed: that branch deliberately
// leaves the bot enabled, so the stored token is still live, and naming it
// would delete the company's only record of a working credential — which is
// the failure the paragraph above is written to avoid, arriving through the
// report instead of through the teardown.
func Teardown(ctx context.Context, opts TeardownOptions) (provision.Removed, error) {
	var removed provision.Removed
	if opts.Client == nil {
		return removed, errors.New("mattermost: no client")
	}
	if !opts.RemoveSeats || opts.Config == nil || opts.Config.Provisioning == nil {
		return removed, nil
	}
	if opts.Plan == nil {
		return removed, nil
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
			// ALREADY GONE, and the token sealed for it is dead.
			removed.Add(provision.Removal{
				Handle: seat.Handle, Role: seat.Role, Account: username,
				Secrets: secretsOf(seat),
			})
			continue
		}
		// THE CREDENTIAL FIRST, THEN THE ACCOUNT. Either order leaves
		// work behind if the run dies between the two, and these are the
		// two halves: a revoked token on a live bot is an agent that can
		// do nothing, and a live token on a disabled bot is a credential
		// that works again the moment anybody re-enables the account.
		// The first is the safer thing to be interrupted at.
		if _, err := opts.Client.RevokeMinted(ctx, bot.ID,
			TokenDescription(seat.Handle), ""); err != nil {
			failures = append(failures, fmt.Errorf(
				"mattermost: revoke %s's tokens before disabling it: %w",
				username, err))
			// AND THE BOT STAYS ENABLED, so the state is the one the
			// operator can see: an agent still listed at the instance
			// with a credential nobody could withdraw, rather than a
			// disabled account quietly holding a working token. The
			// disconnect fails, and repeating it resumes here.
			continue
		}
		// MARKED BEFORE IT IS DISABLED, and marked whatever state it is
		// already in. See [Client.DisconnectedDescription]: "disabled" is
		// an ambiguous bit, and a teardown that met a bot somebody had
		// already deactivated would otherwise write no provenance and
		// still report the seat removed — leaving an account this engine
		// decommissioned that it can never prove it decommissioned, and
		// so can never bring back.
		if err := opts.Client.MarkDisconnected(ctx, bot.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"mattermost: record that this engine is disabling %s: %w",
				username, err))
			continue
		}
		if err := opts.Client.DisableBot(ctx, bot.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"mattermost: disable %s: %w", username, err))
			continue
		}
		// BOTH HALVES DONE: the token is revoked and the bot is disabled,
		// so the value sealed for this seat authenticates as nothing and
		// cannot start working again.
		removed.Add(provision.Removal{
			Handle: seat.Handle, Role: seat.Role, Account: username,
			Secrets: secretsOf(seat),
		})
	}
	return removed, errors.Join(failures...)
}

// secretsOf is the variables one seat's credentials live in.
func secretsOf(seat provision.Seat) []string {
	var out []string
	for _, name := range []string{seat.TokenVar, seat.EmailVar} {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}
