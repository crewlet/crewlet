package datadog

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// TeardownOptions is what removing a Datadog integration's company half needs.
type TeardownOptions struct {
	Client *Client
	Config *config.Datadog
	Creds  Credentials

	// WebhookBase is this deployment's public base URL, and it is what
	// PROVES the definition about to be deleted is this engine's.
	//
	// A Datadog webhook is addressed by name, and a name is not ownership:
	// deleting whatever currently holds it takes down an integration this
	// engine never made if somebody else registered one under the same
	// word. Every other teardown in this tree proves ownership before it
	// destroys (GitHub, GitLab and Jira all match the delivery URL).
	//
	// Empty withdraws nothing: without a base this deployment registered
	// no definition, so there is none of its own to remove.
	WebhookBase string
}

// Teardown withdraws the webhook this engine registered at Datadog.
//
// THE WEBHOOK GOES EITHER WAY. This engine registered it, nothing else at
// Datadog uses it, and one left behind posts every alert to a company that no
// longer has a block to route it: the deliveries are refused, counted and
// dropped, and the monitors that name it go on reporting a healthy target.
// That is the same rule every other surface's hooks follow; see
// [setup.TeardownInput].
//
// THE AGENTS' ACCOUNTS ARE NOT HERE. A disconnect turns each one off through
// the per-seat access sync, by the same rule as a seat turned off: every key
// deleted, the account marked as this engine's off and disabled, never
// deleted, because deleting a Datadog user detaches it from everything it
// authored.
func Teardown(ctx context.Context, opts TeardownOptions) error {
	if opts.Client == nil {
		return errors.New("datadog: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		// Nothing was ever registered, so there is nothing to remove and
		// the disconnect finishes.
		return nil
	}
	if opts.Creds.APIKey == "" || opts.Creds.AppKey == "" {
		return errors.New(
			"datadog: both keys are needed to withdraw the webhook this engine " +
				"registered; force the disconnect to drop the block and remove " +
				"it at Datadog by hand")
	}
	return withdrawWebhook(ctx, opts)
}

// withdrawWebhook removes the definition this deployment registered, and only
// that one.
//
// IT READS BEFORE IT DELETES. The name is Datadog's primary key but it is not
// evidence of ownership: an organization may already hold a definition called
// "crewlet" that somebody else made, and a disconnect that deleted whatever
// answered to the name would take down an integration this engine never
// registered. Every other vendor teardown here proves ownership first, and
// this one issued the DELETE blind.
//
// The three answers a read gives are each acted on differently:
//
//   - ABSENT is already gone, which is the state a disconnect wants and what
//     keeps a repeated teardown safe.
//   - UNREADABLE is a fault, not a licence: a store that could not answer has
//     not said the definition is this engine's.
//   - PRESENT AT ANOTHER ADDRESS is somebody else's, and it is REPORTED
//     rather than removed, because a wrong deletion is unrecoverable where a
//     refused disconnect is not.
func withdrawWebhook(ctx context.Context, opts TeardownOptions) error {
	target := WebhookTarget(opts.WebhookBase)
	if target == "" {
		// Nothing was ever registered without one — the pass refuses to,
		// naming the missing public base — so there is nothing to
		// withdraw and the disconnect finishes.
		return nil
	}
	name := WebhookNameOf(opts.Config)
	current, found, err := opts.Client.Webhook(ctx, opts.Creds, name)
	switch {
	case err != nil:
		return fmt.Errorf("datadog: read the webhook named %q before removing it: %w",
			name, integration.Reject(err, Status(err)))
	case !found:
		return nil
	case current.URL != target:
		return fmt.Errorf(
			"datadog: the webhook named %q posts to %s and this deployment "+
				"registered %s, so it is not this engine's to remove — rename "+
				"integrations.datadog.webhook_name, or delete that definition "+
				"at Datadog if it is a leftover",
			name, current.URL, target)
	}
	if err := opts.Client.DeleteWebhook(ctx, opts.Creds, name); err != nil {
		return fmt.Errorf("datadog: withdraw the webhook: %w",
			integration.Reject(err, Status(err)))
	}
	return nil
}
