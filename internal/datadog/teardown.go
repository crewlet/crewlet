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
// THE WEBHOOK GOES EITHER WAY. This engine registered it and nothing else uses
// it; one left behind posts alerts to a company with no block to route them,
// refused and dropped while monitors report a healthy target. See
// [setup.TeardownInput].
//
// THE AGENTS' ACCOUNTS ARE NOT HERE. A disconnect turns each off through the
// per-seat access sync, as for a seat turned off: keys deleted, the account
// disabled, never deleted, because deleting a Datadog user detaches it from
// everything it authored.
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
// IT READS BEFORE IT DELETES: the name is not evidence of ownership, and
// somebody else's "crewlet" definition must survive. The read's answers:
//
//   - ABSENT is already gone, which keeps a repeated teardown safe.
//   - UNREADABLE is a fault, not a licence to delete.
//   - PRESENT AT ANOTHER ADDRESS is somebody else's, REPORTED rather than
//     removed: a wrong deletion is unrecoverable, a refused disconnect is not.
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
