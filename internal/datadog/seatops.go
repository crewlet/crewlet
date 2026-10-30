package datadog

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// The per-seat operations the access pass is built from (design 5.2).
//
// Each is one thing done at Datadog to one account, with the rule that keeps
// it safe written beside it, so the access pass that composes them and the
// teardown that reuses them cannot disagree about what turning an account off
// means.

// Account is what a pass reads back about one seat's account, every pass.
type Account struct {
	// ID is Datadog's user id, which the ledger records.
	ID    string
	Email string
	// Disabled is the account's own state at Datadog.
	Disabled bool
	// Marked reports that the account carries [DisconnectedTitle]: this
	// engine is the one that turned it off.
	Marked bool
	// RoleIDs are the roles it holds, sorted.
	RoleIDs []string
	// Keys are its application keys, without their values.
	Keys []AppKey
}

// accountOf is the observed account from a user row and its keys.
func accountOf(user User, keys []AppKey) Account {
	return Account{
		ID: user.ID, Email: user.Email, Disabled: user.Disabled,
		Marked:  user.Title == DisconnectedTitle,
		RoleIDs: slices.Clone(user.RoleIDs), Keys: keys,
	}
}

// EngineKeys are the keys on the account this engine minted, by the name every
// mint here writes.
func (a Account) EngineKeys() []AppKey {
	var out []AppKey
	for _, key := range a.Keys {
		if key.Name == appKeyName {
			out = append(out, key)
		}
	}
	return out
}

// SetRole brings an account to hold exactly one role, and reports the roles it
// took away.
//
// ADD FIRST, THEN REMOVE, so the account is never without a role and never
// holds anything beyond what it held before plus what the document asks for.
// The reverse order opens a window with no role at all, and an account in no
// role at Datadog is one whose next call is refused for a reason nobody can
// see from the seat. If a removal fails after the add, the account holds the
// union it already had: no wider than before, reported, and retried.
func (c *Client) SetRole(
	ctx context.Context, creds Credentials, account Account, roleID string,
) (removed []string, err error) {
	if !slices.Contains(account.RoleIDs, roleID) {
		if err := c.AddRoleMember(ctx, creds, roleID, account.ID); err != nil {
			return nil, err
		}
	}
	for _, held := range account.RoleIDs {
		if held == roleID {
			continue
		}
		if err := c.RemoveRoleMember(ctx, creds, held, account.ID); err != nil {
			return removed, err
		}
		removed = append(removed, held)
	}
	return removed, nil
}

// RevokeKeys deletes every application key on an account.
//
// EVERY KEY, not only the ones this engine can name: a key's value is served
// once, the account exists solely because this engine created it, and a key
// left on it is exactly the live credential turning it off exists to remove.
func (c *Client) RevokeKeys(ctx context.Context, creds Credentials, account Account) error {
	for _, key := range account.Keys {
		if err := c.DeleteAppKey(ctx, creds, account.ID, key.ID); err != nil {
			return fmt.Errorf("revoke the application key %q: %w", key.Name, err)
		}
	}
	return nil
}

// Disable marks an account as this engine's off and disables it.
//
// THE MARKER BEFORE THE DISABLE, so an interrupted run leaves a marked, live
// account rather than a disabled one with no provenance, which nothing can
// undo on its own (D3). MARKED WHATEVER STATE THE ACCOUNT IS IN: an account
// already disabled by hand is still recorded as this engine's off once the
// seat's document says off too, or it could never be turned back on when the
// document says on again.
func (c *Client) Disable(ctx context.Context, creds Credentials, account Account) error {
	if err := c.MarkDisconnected(ctx, creds, account.ID); err != nil {
		return fmt.Errorf("record that this engine is turning the account off: %w", err)
	}
	if account.Disabled {
		return nil
	}
	if err := c.DisableUser(ctx, creds, account.ID); err != nil {
		return fmt.Errorf("disable the account: %w", err)
	}
	return nil
}

// TurnOff revokes every key on an account, then marks and disables it.
//
// THE KEYS FIRST: a revoked key on a live account is an agent that can do
// nothing, and a live key on a disabled account is a credential that works
// again the moment anybody re-enables it. The first is the safer thing to be
// interrupted at. One rule for a seat turned off and for a disconnect.
func (c *Client) TurnOff(ctx context.Context, creds Credentials, account Account) error {
	if err := c.RevokeKeys(ctx, creds, account); err != nil {
		return err
	}
	return c.Disable(ctx, creds, account)
}

// RetireKeys deletes the keys this engine minted on an account other than
// keep, which is a rotation's second half: keep was minted and sealed by the
// step before, so the keys it replaces hold values nothing reads.
//
// IT REFUSES RATHER THAN GUESSES. A keep the account does not hold means the
// key just minted is gone, and deleting the others would leave the seat with
// nothing; keys a person put on the account are not this engine's to retire.
func (c *Client) RetireKeys(
	ctx context.Context, creds Credentials, account Account, keep string,
) (int, error) {
	engine := account.EngineKeys()
	if keep == "" || !slices.ContainsFunc(engine, func(key AppKey) bool { return key.ID == keep }) {
		return 0, errors.New("datadog: the key this rotation minted is not on the account, " +
			"so no older key was deleted; rotate again")
	}
	retired := 0
	for _, key := range engine {
		if key.ID == keep {
			continue
		}
		if err := c.DeleteAppKey(ctx, creds, account.ID, key.ID); err != nil {
			return retired, fmt.Errorf("delete the replaced application key: %w", err)
		}
		retired++
	}
	return retired, nil
}
