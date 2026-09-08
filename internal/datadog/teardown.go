package datadog

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// TeardownOptions is what removing a Datadog integration needs.
type TeardownOptions struct {
	Client *Client
	Config *config.Datadog
	Plan   *provision.Plan
	Creds  Credentials
	// RemoveSeats disables the service accounts this engine created.
	RemoveSeats bool
}

// Teardown removes what this engine created at Datadog.
//
// THE WEBHOOK GOES EITHER WAY. This engine registered it, nothing else at
// Datadog uses it, and one left behind posts every alert to a company that no
// longer has a block to route it: the deliveries are refused, counted and
// dropped, and the monitors that name it go on reporting a healthy target.
// That is the same rule every other surface's hooks follow — see
// [setup.TeardownInput] — and it is why the definition is not gated on
// RemoveSeats.
//
// The accounts go only when asked, and they are DISABLED rather than deleted:
// deleting a Datadog user detaches it from everything it did, so dashboards,
// monitors and notebooks it authored lose their author. A disabled service
// account keeps what it made and can do nothing more.
//
// EVERY SEAT IS ATTEMPTED before the first failure is returned, and the
// errors are joined: one account a credential cannot touch must not strand
// the rest, and an operator reading a stuck teardown should see everything
// blocking it at once.
func Teardown(ctx context.Context, opts TeardownOptions) error {
	if opts.Client == nil {
		return errors.New("datadog: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		// Nothing was ever registered or provisioned, so there is nothing
		// to remove and the disconnect finishes.
		return nil
	}
	if opts.Creds.APIKey == "" || opts.Creds.AppKey == "" {
		return errors.New(
			"datadog: both keys are needed to withdraw the webhook this engine " +
				"registered and to disable the accounts it created; force the " +
				"disconnect to drop the block and remove them at Datadog by hand")
	}

	// THE WEBHOOK FIRST, because it is what is still delivering. An
	// account that outlives a failed teardown does nothing on its own; a
	// definition that does posts every alert to a route that will refuse
	// it.
	if err := opts.Client.DeleteWebhook(
		ctx, opts.Creds, WebhookNameOf(opts.Config)); err != nil {
		return fmt.Errorf("datadog: withdraw the webhook: %w",
			integration.Reject(err, Status(err)))
	}

	if !opts.RemoveSeats || opts.Plan == nil {
		return nil
	}

	existing, err := opts.Client.ListServiceAccounts(ctx, opts.Creds, emailDomainOf(opts.Config))
	if err != nil {
		return fmt.Errorf("datadog: list the accounts to disable them: %w",
			integration.Reject(err, Status(err)))
	}
	byEmail := make(map[string]User, len(existing))
	for _, user := range existing {
		byEmail[user.Email] = user
	}

	var failures []error
	for _, seat := range opts.Plan.Seats {
		account, found := byEmail[seat.Email]
		if !found || account.Disabled {
			// Already in the state a disconnect wants, which is why this
			// is safe to repeat after a partial failure.
			continue
		}
		if err := opts.Client.DisableUser(ctx, opts.Creds, account.ID); err != nil {
			failures = append(failures, fmt.Errorf("datadog: disable %s: %w",
				seat.Handle, integration.Reject(err, Status(err))))
		}
	}
	return errors.Join(failures...)
}
