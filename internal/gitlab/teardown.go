package gitlab

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
)

// TeardownOptions is what removing a GitLab integration needs.
type TeardownOptions struct {
	Client *Client
	Config *config.GitLab
	// Plan names the seats, the same way [Options] does, so a teardown
	// removes exactly what a pass created.
	Plan *provision.Plan
	// WebhookBase is the address the hooks point at, which is how they
	// are identified. Empty means none were ever registered.
	WebhookBase string
	// RemoveSeats deletes the service accounts this engine created.
	RemoveSeats bool
}

// Teardown removes what this engine created in a GitLab group.
//
// TWO KINDS OF THING, and only one comes out unconditionally. The hooks are
// this engine's own registrations pointing at this deployment, so they go
// whatever the operator chose; leaving one behind delivers a company's pushes
// to an engine that no longer has a block to route them.
//
// The SERVICE ACCOUNTS are the other kind, and they go only when asked. Each
// is a member of the customer's group with commits, review comments and issue
// history attached to it, and deleting one because somebody pressed
// Disconnect would rewrite that history's author. This is the account
// removal the console's checkbox is asking about.
//
// EVERY STEP IS ATTEMPTED before the first failure is returned, and the
// errors are joined. One project a token cannot administer must not strand
// the hooks on all the others, and an operator reading a stuck teardown
// should see everything blocking it rather than one item per retry.
//
// SAFE TO REPEAT: a hook or an account already gone is not an error, so the
// pass retried after a partial failure finishes the rest.
func Teardown(ctx context.Context, opts TeardownOptions) error {
	if opts.Client == nil {
		return errors.New("gitlab: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		return nil
	}
	p := opts.Config.Provisioning
	group := strings.TrimSpace(p.Group)
	if group == "" {
		return nil
	}

	var failures []error
	groupID, found, err := groupIDOf(ctx, opts.Client, group)
	switch {
	case err != nil:
		failures = append(failures, fmt.Errorf("gitlab: resolve group %q to remove what it holds: %w",
			group, err))
	case !found:
		// The group is gone, so everything in it went with it. Nothing
		// left to remove and nothing to report.
		return nil
	default:
		failures = append(failures, removeHooks(ctx, opts, groupID)...)
		if opts.RemoveSeats {
			failures = append(failures, removeAccounts(ctx, opts, groupID)...)
		}
	}
	return errors.Join(failures...)
}

// groupIDOf resolves the group, reporting absence separately from failure.
func groupIDOf(ctx context.Context, c *Client, path string) (int, bool, error) {
	group, found, err := c.GroupByPath(ctx, path)
	if err != nil || !found {
		return 0, found, err
	}
	return group.ID, true, nil
}

// removeHooks withdraws the group hook and every project hook, matched on the
// delivery URL — the same rule the reconcile uses, because an instance
// carries hooks other integrations registered and taking the first one found
// would delete somebody else's.
func removeHooks(ctx context.Context, opts TeardownOptions, groupID int) []error {
	target := webhookTarget(opts.WebhookBase)
	if target == "" {
		return nil
	}
	var failures []error

	// BOTH LEVELS, whatever the configured mode says today. A group hook
	// and per-project hooks are two branches the pass chooses between on
	// the instance's tier, and that answer moves; sweeping only where the
	// current mode would have written leaves every hook the other branch
	// ever made.
	if hooks, err := opts.Client.GroupHooks(ctx, groupID); err != nil {
		failures = append(failures, fmt.Errorf("gitlab: list group hooks to remove them: %w", err))
	} else {
		for _, hook := range hooks {
			if hook.URL != target {
				continue
			}
			if err := opts.Client.DeleteGroupHook(ctx, groupID, hook.ID); err != nil {
				failures = append(failures, fmt.Errorf("gitlab: remove group hook: %w", err))
			}
		}
	}

	for _, project := range opts.Config.Provisioning.Projects {
		hooks, err := opts.Client.ProjectHooks(ctx, project)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: list %s's hooks to remove them: %w", project, err))
			continue
		}
		for _, hook := range hooks {
			if hook.URL != target {
				continue
			}
			if err := opts.Client.DeleteProjectHook(ctx, project, hook.ID); err != nil {
				failures = append(failures, fmt.Errorf(
					"gitlab: remove %s's hook: %w", project, err))
			}
		}
	}
	return failures
}

// removeAccounts deletes the service account this engine made for each seat.
//
// Resolved through [Username], which is the same derivation the pass creates
// them under, so this removes exactly what it made and cannot reach an
// account somebody else named. A seat with no account is already in the state
// a disconnect wants.
func removeAccounts(ctx context.Context, opts TeardownOptions, groupID int) []error {
	if opts.Plan == nil {
		return nil
	}
	var failures []error
	for _, seat := range opts.Plan.Seats {
		username := Username(opts.Config.Provisioning, seat.Handle)
		user, found, err := opts.Client.UserByUsername(ctx, username)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: look up %s to remove it: %w", username, err))
			continue
		}
		if !found {
			continue
		}
		if err := opts.Client.DeleteServiceAccount(ctx, groupID, user.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: remove service account %s: %w", username, err))
		}
	}
	return failures
}
