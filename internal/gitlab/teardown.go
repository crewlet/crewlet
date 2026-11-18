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
}

// Teardown removes the webhooks this engine registered in a GitLab group.
//
// THE HOOKS ONLY: leaving one behind delivers a company's pushes to an engine
// with no block to route them. Service accounts are per-seat access
// ([Vendor]); removing one goes through the ledger, which reaches every
// account the engine created, even one whose seat has left the document. So
// the [provision.Removed] it answers is always empty.
//
// EVERY HOOK IS ATTEMPTED before the joined errors are returned: one project a
// token cannot administer must not strand the hooks on all the others.
// SAFE TO REPEAT: a hook already gone is not an error.
func Teardown(ctx context.Context, opts TeardownOptions) (provision.Removed, error) {
	var none provision.Removed
	if opts.Client == nil {
		return none, errors.New("gitlab: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		return none, nil
	}
	group := strings.TrimSpace(opts.Config.Provisioning.Group)
	if group == "" {
		return none, nil
	}
	groupID, found, err := groupIDOf(ctx, opts.Client, group)
	switch {
	case err != nil:
		return none, fmt.Errorf("gitlab: resolve group %q to remove what it holds: %w", group, err)
	case !found:
		// The group is gone, and every hook registered on it with it.
		return none, nil
	}
	return none, errors.Join(removeHooks(ctx, opts, groupID)...)
}

// groupIDOf resolves the group, reporting absence separately from failure.
func groupIDOf(ctx context.Context, c *Client, path string) (int, bool, error) {
	group, found, err := c.GroupByPath(ctx, path)
	if err != nil || !found {
		return 0, found, err
	}
	return group.ID, true, nil
}

// removeHooks withdraws the group hook and every project hook, matched by
// [ours] — the same rule the reconcile uses, because an instance carries
// hooks other integrations registered and taking the first one found would
// delete somebody else's.
//
// BY NAME RATHER THAN BY THE CURRENT ADDRESS: matching `webhookTarget` would
// leave every hook a PREVIOUS public base registered, and remove nothing when
// no base is left to compute a target from, yet report done.
func removeHooks(ctx context.Context, opts TeardownOptions, groupID int) []error {
	name := opts.Config.WebhookNameOrDefault()
	var failures []error

	// BOTH LEVELS, whatever the configured mode says today. A group hook
	// and per-project hooks are two branches the pass chooses between on
	// the instance's tier, and that answer moves; sweeping only where the
	// current mode would have written leaves every hook the other branch
	// ever made.
	if hooks, err := opts.Client.GroupHooks(ctx, groupID); err != nil {
		if !gatedByTier(err) {
			failures = append(failures,
				fmt.Errorf("gitlab: list group hooks to remove them: %w", err))
		}
	} else {
		for _, hook := range mine(hooks, name) {
			if err := opts.Client.DeleteGroupHook(ctx, groupID, hook.ID); err != nil {
				failures = append(failures, fmt.Errorf(
					"gitlab: remove the group hook at %s: %w", hook.URL, err))
			}
		}
	}

	// EVERY PROJECT IN THE GROUP, not only the ones the config names.
	//
	// The config is not a record of where the hooks ARE: hooks were written on
	// the projects named AT THE TIME, so a project since dropped from
	// `provisioning.projects` keeps them, unvisited. A dead tunnel hostname
	// (`trycloudflare`) is re-issued to a stranger, so those payloads would
	// keep leaving the customer's GitLab for them.
	//
	// The configured list is unioned in, not replaced: a project need not be
	// in the group, `provisioning.projects` takes any path the credential
	// can administer.
	for _, project := range sweepable(ctx, opts, groupID, &failures) {
		hooks, err := opts.Client.ProjectHooks(ctx, project)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: list %s's hooks to remove them: %w", project, err))
			continue
		}
		for _, hook := range mine(hooks, name) {
			if err := opts.Client.DeleteProjectHook(ctx, project, hook.ID); err != nil {
				failures = append(failures, fmt.Errorf(
					"gitlab: remove %s's hook at %s: %w", project, hook.URL, err))
			}
		}
	}
	return failures
}

// sweepable is every project a teardown should look for this engine's hooks
// on: the group's own, subgroups included, plus whatever the config names.
//
// A FAILED ENUMERATION IS RECORDED AND THE CONFIGURED LIST STILL RUNS. The
// group listing needs a credential that can read it, and a teardown that gave
// up on the whole project sweep because the group could not be enumerated
// would leave the hooks it CAN reach behind as well — the opposite of what
// this function was added for.
func sweepable(
	ctx context.Context, opts TeardownOptions, groupID int, failures *[]error,
) []string {
	seen := map[string]bool{}
	var out []string
	add := func(paths ...string) {
		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	if groupID != 0 {
		held, err := opts.Client.GroupProjects(ctx, groupID)
		if err != nil {
			*failures = append(*failures, fmt.Errorf(
				"gitlab: list the group's projects to remove this engine's "+
					"hooks from them: %w", err))
		}
		add(held...)
	}
	add(opts.Config.Provisioning.Projects...)
	return out
}
