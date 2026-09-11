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
// # What it reports
//
// Every planned seat whose account is now absent from the organization, with
// BOTH of the variables that seat's credentials live in: Atlassian assigns the
// account's address at creation and its products authenticate
// base64(address:token), so a pass seals two values per agent and a deletion
// strands two.
//
// AN END STATE, not a delta — see [setup.Teardowner]. A seat whose account is
// not in the listing at all is reported as removed: either an earlier attempt
// deleted it, or somebody did it by hand, and in both cases the credential
// sealed for it is dead. A retry that reported only what THIS call deleted
// would find nothing to do and let the block drop with the values still
// resolving, which is the defect reached through the recovery path.
func Teardown(ctx context.Context, opts TeardownOptions) (provision.Removed, error) {
	var removed provision.Removed
	if opts.Client == nil {
		return removed, errors.New("atlassian: no client")
	}
	if opts.OrgID == "" || opts.Key == "" {
		return removed, errors.New("atlassian: the organization id and its API key are both needed")
	}
	accounts, err := opts.Client.ListServiceAccounts(ctx, opts.Key, opts.OrgID)
	if err != nil {
		return removed, fmt.Errorf("atlassian: list service accounts: %w", err)
	}
	wanted := map[string]provision.Seat{}
	if opts.Plan != nil {
		for _, seat := range opts.Plan.Seats {
			wanted[seat.Handle] = seat
		}
	}
	for _, account := range accounts {
		handle := HandleFrom(account.DisplayName)
		seat, planned := wanted[handle]
		if handle == "" || !planned {
			continue
		}
		if err := opts.Client.DeleteServiceAccount(ctx, opts.Key, account.ID); err != nil {
			return removed, fmt.Errorf("atlassian: delete the account for %s: %w", handle, err)
		}
		removed.Add(removalFor(seat, account.Email))
		delete(wanted, handle)
	}
	// WHAT WAS ALREADY GONE. Everything still in `wanted` has no account in
	// the organization, so its credentials are dead whoever removed it.
	for _, seat := range wanted {
		removed.Add(removalFor(seat, ""))
	}
	return removed, nil
}

// removalFor is one seat's removal, naming both credential slots.
func removalFor(seat provision.Seat, account string) provision.Removal {
	out := provision.Removal{Handle: seat.Handle, Role: seat.Role, Account: account}
	for _, name := range []string{seat.TokenVar, seat.EmailVar} {
		if name != "" {
			out.Secrets = append(out.Secrets, name)
		}
	}
	return out
}
