package datadog

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/provision"
)

// The per-seat operations the access pass is built from (design 5.2).
//
// Each is one thing done at Datadog to one account, with the rule that keeps
// it safe written beside it, so the pass that composes them and the teardown
// that reuses them cannot disagree about what turning an account off means.

// Account is what a pass reads back about one seat's account, every pass.
type Account struct {
	// ID is Datadog's user id, which the ledger records.
	ID    string
	Email string
	// Disabled is the account's own state at Datadog.
	Disabled bool
	// DisabledHere reports that THIS engine disabled it: the account
	// carries [DisconnectedTitle]. Only such an account is ever turned
	// back on automatically (D3).
	DisabledHere bool
	// RoleIDs are the roles it holds, sorted.
	RoleIDs []string
	// Keys are its application keys, without their values.
	Keys []AppKey
}

// ObserveAccount reads one account and its keys.
//
// THE ACCOUNT ABSENT IS AN ANSWER (found false, no error): the ledger row is
// closed and a seat that is on is given a fresh account (design 5.4).
func (c *Client) ObserveAccount(
	ctx context.Context, creds Credentials, accountID string,
) (Account, bool, error) {
	user, found, err := c.User(ctx, creds, accountID)
	if err != nil || !found {
		return Account{}, found, err
	}
	keys, err := c.ListAppKeys(ctx, creds, accountID)
	if err != nil {
		return Account{}, true, fmt.Errorf("datadog: read the keys on %s: %w", user.Email, err)
	}
	return accountOf(user, keys), true, nil
}

// accountOf is the observed account from a user row and its keys.
func accountOf(user User, keys []AppKey) Account {
	return Account{
		ID: user.ID, Email: user.Email, Disabled: user.Disabled,
		DisabledHere: user.Disabled && user.Title == DisconnectedTitle,
		RoleIDs:      slices.Clone(user.RoleIDs), Keys: keys,
	}
}

// HoldsOnly reports whether an account holds exactly one role, the one given.
func (a Account) HoldsOnly(roleID string) bool {
	return len(a.RoleIDs) == 1 && a.RoleIDs[0] == roleID
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

// TurnOff disables an account this engine manages and revokes every key on it.
//
// THE KEYS FIRST, THEN THE MARKER, THEN THE DISABLE, which is the order the
// teardown has always argued for: a revoked key on a live account is an agent
// that can do nothing, and a live key on a disabled account is a credential
// that works again the moment anybody re-enables it. The marker is written
// before the disable, so an interrupted run leaves a marked, live account
// rather than a disabled one with no provenance, which nothing can undo on its
// own (D3).
//
// MARKED WHATEVER STATE THE ACCOUNT IS IN: an account already disabled by
// hand is still recorded as this engine's off, because the seat's document
// now says off too, and an unmarked disabled account could never be turned
// back on when the document says on again.
func (c *Client) TurnOff(ctx context.Context, creds Credentials, account Account) error {
	for _, key := range account.Keys {
		if err := c.DeleteAppKey(ctx, creds, account.ID, key.ID); err != nil {
			return fmt.Errorf("revoke the application key %q: %w", key.Name, err)
		}
	}
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

// MintInto creates one application key on an account and records it in the
// seat's sink.
//
// A FAILED RECORD REVOKES THE KEY (design 5.2): Datadog serves a key's value
// once, so a key that was minted and not recorded is a live credential held by
// nobody. The returned error is the record's failure with the revocation's
// outcome appended, never replaced by it.
func (c *Client) MintInto(
	ctx context.Context, creds Credentials, sink provision.TokenSink,
	accountID, handle, sinkVar string,
) (AppKey, error) {
	if sink == nil {
		return AppKey{}, provision.ErrNoSink
	}
	minted, err := c.CreateAppKey(ctx, creds, accountID, appKeyName)
	if err != nil {
		return AppKey{}, err
	}
	if err := sink.Record(ctx, sinkVar, minted.Key); err != nil {
		return AppKey{}, c.revokeUnrecorded(ctx, creds, handle, accountID, minted, err)
	}
	return AppKey{ID: minted.ID, Name: minted.Name}, nil
}

// Rotate mints a new key, records it, and then deletes the keys this engine
// minted before it.
//
// IN THAT ORDER: the old key keeps working until the new value is recorded, so
// a rotation that fails part way leaves the seat with a working credential.
// Only keys under this engine's own name are deleted; a key a person put on
// the account is theirs to remove.
func (c *Client) Rotate(
	ctx context.Context, creds Credentials, sink provision.TokenSink,
	account Account, handle, sinkVar string,
) (AppKey, error) {
	minted, err := c.MintInto(ctx, creds, sink, account.ID, handle, sinkVar)
	if err != nil {
		return AppKey{}, err
	}
	var failures []error
	for _, key := range account.Keys {
		if key.Name != appKeyName || key.ID == minted.ID {
			continue
		}
		if err := c.DeleteAppKey(ctx, creds, account.ID, key.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"delete the replaced application key %s: %w", key.ID, err))
		}
	}
	return minted, errors.Join(failures...)
}

// revokeUnrecorded takes back a key that was minted and could not be recorded.
//
// DETACHED, because the failure is frequently the cancellation itself, and a
// revocation inheriting a dead context does nothing at all, which is exactly
// when the credential most needs taking back.
func (c *Client) revokeUnrecorded(
	ctx context.Context, creds Credentials, handle, accountID string, key AppKey, cause error,
) error {
	if err := c.DeleteAppKey(context.WithoutCancel(ctx), creds, accountID, key.ID); err != nil {
		return fmt.Errorf(
			"minted a key for %s and could not record it (%w), and could not "+
				"revoke it either (%v): it is live at Datadog and held by nobody, so "+
				"delete the key named %q on %s's service account",
			handle, cause, err, key.Name, handle)
	}
	return fmt.Errorf(
		"minted a key for %s and could not record it, so it was revoked again; "+
			"nothing is left at Datadog and the next pass mints another: %w",
		handle, cause)
}
