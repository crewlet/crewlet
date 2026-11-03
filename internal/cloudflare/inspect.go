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
	for _, level := range Levels() {
		names, _ := Groups(level)
		if missing := in.cat.missingGroups(names); len(missing) > 0 {
			if in.unresolved == nil {
				in.unresolved = map[string][]string{}
			}
			in.unresolved[level] = missing
		}
	}
	err := call(stageOwn, func(ctx context.Context) error {
		own, found, err := c.Token(ctx, in.status.ID)
		switch {
		case err != nil && Status(err) != http.StatusForbidden:
			return err
		case err != nil || !found:
			// Its own policy is unreadable, which says nothing about what
			// it holds: the check is skipped rather than guessed at.
			return nil
		}
		in.unheld = in.cat.unheld(own.Policies)
		return nil
	})
	return in, err
}

// unheld is every provisioner group the policies do not allow, by name. A
// group the catalogue cannot resolve is not judged here: the level that names
// it is reported instead.
func (c catalogue) unheld(policies []Policy) []string {
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
	for _, name := range ProvisionerGroups() {
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
