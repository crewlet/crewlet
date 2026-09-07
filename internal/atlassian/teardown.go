package atlassian

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/provision"
)

// TeardownOptions is what removing this company's Atlassian identities needs.
type TeardownOptions struct {
	Client *Client
	OrgID  string
	Key    string

	// Plan names the seats whose accounts this engine created.
	Plan *provision.Plan
}

// Teardown deletes the service accounts this engine created.
//
// ONLY ITS OWN, matched on the description it wrote: an organization's
// service accounts include ones people made for their own scripts, and a
// disconnect that removed those would destroy somebody else's automation on
// its way out.
//
// DELETION, not deactivation, because an account that merely stops working
// still holds its seat and still appears in every user list.
//
// It reports the first refusal and stops, rather than continuing over a
// credential that is being refused: with the key rejected, every remaining
// call would fail the same way and the report would name every agent as a
// separate failure of the same one thing.
func Teardown(ctx context.Context, opts TeardownOptions) error {
	if opts.Client == nil {
		return errors.New("atlassian: no client")
	}
	if opts.OrgID == "" || opts.Key == "" {
		return errors.New("atlassian: the organization id and its API key are both needed")
	}
	accounts, err := opts.Client.ListServiceAccounts(ctx, opts.Key, opts.OrgID)
	if err != nil {
		return fmt.Errorf("atlassian: list service accounts: %w", err)
	}
	wanted := map[string]bool{}
	if opts.Plan != nil {
		for _, seat := range opts.Plan.Seats {
			wanted[seat.Handle] = true
		}
	}
	for _, account := range accounts {
		handle := HandleFrom(account.DisplayName)
		if handle == "" || !wanted[handle] {
			continue
		}
		if err := opts.Client.DeleteServiceAccount(ctx, opts.Key, account.ID); err != nil {
			return fmt.Errorf("atlassian: delete the account for %s: %w", handle, err)
		}
	}
	return nil
}
