package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"slices"
)

// inspection is one read of the provisioner token and the account it mints
// in: the read both the Test button and the reconcile make, in this order.
type inspection struct {
	status TokenStatus
	zones  []Zone
	cat    catalogue

	// unresolved is, by level, the permission group names the account's
	// catalogue does not offer.
	unresolved map[string][]string

	// unheld is the provisioner groups the token does not hold itself; nil
	// where its own policy could not be read.
	unheld []string

	// atlasUnheld is the atlas's optional groups the token does not hold;
	// nil where its own policy could not be read. Never a gap: a token
	// without them is whole (ADR-0145).
	atlasUnheld []string
}

// stage is one read of an inspection, and the permission it needs.
type stage struct {
	name  string
	needs string
}

//nolint:gochecknoglobals // immutable descriptions, not state
var (
	stageVerify = stage{name: "the token"}
	stageGroups = stage{name: "the account's permission groups", needs: "Account API Tokens Write"}
	stageZones  = stage{name: "the account's zones", needs: "Zone Read"}
	stageOwn    = stage{name: "the token's own permissions", needs: "Account API Tokens Write"}
	stageTokens = stage{name: "the account's tokens", needs: "Account API Tokens Write"}
)

// stageError is a read of an inspection that failed, naming which.
type stageError struct {
	stage stage
	err   error
}

func (e *stageError) Error() string { return fmt.Sprintf("read %s: %v", e.stage.name, e.err) }
func (e *stageError) Unwrap() error { return e.err }

// inactiveError is a token Cloudflare knows and reports disabled or expired.
type inactiveError struct{ status string }

func (e *inactiveError) Error() string {
	return "Cloudflare reports the token " + e.status + ", so it authenticates nothing"
}

// inspect reads the provisioner token's status, the account's permission
// groups and zones, and the token's own policy. bound, where set, gives each
// call its own deadline inside ctx's.
func inspect(
	ctx context.Context, c *Client, configuredZones []string,
	bound func(context.Context) (context.Context, context.CancelFunc),
) (inspection, error) {
	call := func(s stage, fn func(context.Context) error) error {
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if bound != nil {
			callCtx, cancel = bound(ctx)
		}
		defer cancel()
		if err := fn(callCtx); err != nil {
			return &stageError{stage: s, err: err}
		}
		return nil
	}
	var in inspection
	if err := call(stageVerify, func(ctx context.Context) error {
		var err error
		if in.status, err = c.Verify(ctx); err != nil {
			return err
		}
		if in.status.Status != StatusActive {
			return &inactiveError{status: in.status.Status}
		}
		return nil
	}); err != nil {
		return in, err
	}
	var groups []PermissionGroup
	if err := call(stageGroups, func(ctx context.Context) error {
		var err error
		groups, err = c.PermissionGroups(ctx)
		return err
	}); err != nil {
		return in, err
	}
	if err := call(stageZones, func(ctx context.Context) error {
		var err error
		in.zones, err = c.Zones(ctx)
		return err
	}); err != nil {
		return in, err
	}
	in.cat = newCatalogue(c.AccountID(), groups, in.zones, configuredZones)
	in.unresolved = in.cat.unresolvedLevels()
	err := call(stageOwn, func(ctx context.Context) error {
		var err error
		in.unheld, in.atlasUnheld, err = readUnheld(ctx, c, in.cat, in.status.ID)
		return err
	})
	return in, err
}

// readUnheld is the provisioner groups the token tokenID does not hold, and
// the atlas's optional ones it does not, read from its own policy; nil where
// Cloudflare will not show the token that policy, which says nothing about
// what it holds, so nothing is guessed.
func readUnheld(ctx context.Context, c *Client, cat catalogue, tokenID string) (unheld, optional []string, err error) {
	own, found, err := c.Token(ctx, tokenID)
	if Status(err) == http.StatusForbidden {
		// NOT SHOWN, which is an answer rather than a failure: no gap is judged.
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, nil
	}
	return cat.unheldOf(own.Policies, ProvisionerGroups()), cat.unheldOf(own.Policies, AtlasGroups()), nil
}

// unheldOf is every group of names the policies do not allow. A group the
// catalogue cannot resolve is not judged here: for a provisioner group the
// level that names it is reported instead, and an optional one the account
// is not offered is nothing a token could add.
func (c catalogue) unheldOf(policies []Policy, names []string) []string {
	held := map[string]bool{}
	for _, p := range policies {
		if p.Effect != "allow" {
			continue
		}
		for _, g := range p.PermissionGroups {
			held[g.ID] = true
		}
	}
	out := []string{}
	for _, name := range names {
		ids := slices.Concat(c.idsNamed(name, scopeAccount), c.idsNamed(name, scopeZone))
		if len(ids) == 0 {
			continue
		}
		if !slices.ContainsFunc(ids, func(id string) bool { return held[id] }) {
			out = append(out, name)
		}
	}
	return out
}
