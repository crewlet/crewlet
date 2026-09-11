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

	// WebhookBase is this deployment's public base URL, and it is what
	// PROVES the definition about to be deleted is this engine's.
	//
	// A Datadog webhook is addressed by name, and a name is not ownership:
	// deleting whatever currently holds it takes down an integration this
	// engine never made if somebody else registered one under the same
	// word. Every other teardown in this tree proves ownership before it
	// destroys — GitHub, GitLab and Jira all match the delivery URL — and
	// this one could not, because the type carried nothing to compare
	// against.
	//
	// Empty withdraws nothing: without a base this deployment registered
	// no definition, so there is none of its own to remove.
	WebhookBase string
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
// # The application key goes with the account, and that is new
//
// This disabled the account and left its APPLICATION KEY — the value sealed in
// the seat's variable — live. Mattermost's teardown states exactly why that is
// wrong, about its own bots: "a live token on a disabled bot is a credential
// that works again the moment anybody re-enables the account". Re-enabling a
// Datadog user restores a working key held by a company that has disconnected,
// and this engine now re-enables accounts it disabled, so that moment is one
// button press away rather than hypothetical.
//
// # What it reports
//
// A seat whose account is absent, or is disabled AND holds no key this engine
// minted. Both halves are required: a seat whose key could not be deleted is
// NOT reported, because deleting the variable would destroy the company's only
// record of a credential that is still live.
func Teardown(ctx context.Context, opts TeardownOptions) (provision.Removed, error) {
	var removed provision.Removed
	if opts.Client == nil {
		return removed, errors.New("datadog: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		// Nothing was ever registered or provisioned, so there is nothing
		// to remove and the disconnect finishes.
		return removed, nil
	}
	if opts.Creds.APIKey == "" || opts.Creds.AppKey == "" {
		return removed, errors.New(
			"datadog: both keys are needed to withdraw the webhook this engine " +
				"registered and to disable the accounts it created; force the " +
				"disconnect to drop the block and remove them at Datadog by hand")
	}

	// THE WEBHOOK FIRST, because it is what is still delivering. An
	// account that outlives a failed teardown does nothing on its own; a
	// definition that does posts every alert to a route that will refuse
	// it.
	if err := withdrawWebhook(ctx, opts); err != nil {
		return removed, err
	}

	if !opts.RemoveSeats || opts.Plan == nil {
		return removed, nil
	}

	existing, err := opts.Client.ListServiceAccounts(ctx, opts.Creds, emailDomainOf(opts.Config))
	if err != nil {
		return removed, fmt.Errorf("datadog: list the accounts to disable them: %w",
			integration.Reject(err, Status(err)))
	}
	byEmail := make(map[string]User, len(existing))
	for _, user := range existing {
		byEmail[user.Email] = user
	}

	var failures []error
	for _, seat := range opts.Plan.Seats {
		account, found := byEmail[seat.Email]
		if !found {
			// The account is gone entirely, so its key went with it.
			removed.Add(removalFor(seat, seat.Email))
			continue
		}
		// THE KEY FIRST, THEN THE ACCOUNT, which is the order mattermost's
		// teardown argues for: a revoked key on a live account is an agent
		// that can do nothing, and a live key on a disabled account is a
		// credential that works again the moment anybody re-enables it.
		// The first is the safer thing to be interrupted at.
		if err := revokeAppKeys(ctx, opts, account.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"datadog: revoke %s's application keys before disabling it: %w",
				seat.Handle, integration.Reject(err, Status(err))))
			// AND THE ACCOUNT STAYS AS IT IS, so the state is the one an
			// operator can see: an account still listed with a credential
			// nobody could withdraw, rather than a disabled one quietly
			// holding a working key.
			continue
		}
		if !account.Disabled {
			if err := opts.Client.DisableUser(ctx, opts.Creds, account.ID); err != nil {
				failures = append(failures, fmt.Errorf("datadog: disable %s: %w",
					seat.Handle, integration.Reject(err, Status(err))))
				continue
			}
		}
		removed.Add(removalFor(seat, account.Email))
	}
	return removed, errors.Join(failures...)
}

// revokeAppKeys deletes every application key on one account.
//
// ALL OF THEM, not only the one this run can name: a key's VALUE is returned
// once and never again, so there is nothing to match a stored value against,
// and the account exists solely because this engine created it. An
// administrator has no reason to have put a key of their own on an agent's
// service account, and leaving one would leave exactly the live credential
// this deletion exists to remove.
func revokeAppKeys(ctx context.Context, opts TeardownOptions, accountID string) error {
	keys, err := opts.Client.ListAppKeys(ctx, opts.Creds, accountID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := opts.Client.DeleteAppKey(ctx, opts.Creds, accountID, key.ID); err != nil {
			return err
		}
	}
	return nil
}

// removalFor is one seat's removal, naming the variable its key lives in.
func removalFor(seat provision.Seat, account string) provision.Removal {
	out := provision.Removal{Handle: seat.Handle, Role: seat.Role, Account: account}
	if seat.TokenVar != "" {
		out.Secrets = append(out.Secrets, seat.TokenVar)
	}
	return out
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
