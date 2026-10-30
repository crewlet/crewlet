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

	// Plan, RemoveSeats and Mode are IGNORED. They are kept only so the
	// engine's current call sites compile until they are retired: removing
	// an agent's account is per-seat access, through the ledger.
	Plan        *provision.Plan
	RemoveSeats bool
	Mode        Mode
}

// Teardown removes the webhooks this engine registered in a GitLab group.
//
// THE HOOKS ONLY. They are this engine's own registrations pointing at this
// deployment, and leaving one behind delivers a company's pushes to an engine
// that no longer has a block to route them. The service accounts are per-seat
// access ([Vendor]): turning a seat off or removing its account goes through
// the ledger, which reaches every account the engine created, including one
// whose seat has left the document.
//
// EVERY HOOK IS ATTEMPTED before the first failure is returned, and the
// errors are joined: one project a token cannot administer must not strand
// the hooks on all the others.
//
// SAFE TO REPEAT: a hook already gone is not an error.
//
// It removes no account, so the [provision.Removed] it answers is always
// empty.
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
// BY NAME RATHER THAN BY THE CURRENT ADDRESS, which is what makes a
// disconnect finish the job. This compared each hook against
// `webhookTarget(WebhookBase)` and removed only an exact match, so every hook
// a PREVIOUS public base had left behind survived the disconnect that was
// supposed to remove it — and a deployment whose base had moved, or which had
// no base left to compute a target from, removed nothing at all and said it
// was done. Measured: a group hook and two project hooks still live after the
// integration was disconnected.
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
	// The config is not a record of where the hooks ARE. A run that
	// established per-project hooks wrote them on the projects named AT THE
	// TIME, so a project dropped from `provisioning.projects` since — or a
	// company that connected with the group alone and never named one —
	// left them unreachable, and a teardown that visited only the current
	// list reported success having walked past them.
	//
	// Measured on a live disconnect: two hooks still on a project in the
	// group, pointing at dead tunnels from earlier runs, which nothing
	// would ever visit again. A `trycloudflare` hostname is re-issued to
	// whoever asks next, so those deliveries go on leaving the customer's
	// GitLab for a stranger — signed with a secret that stranger does not
	// have, so nothing can be forged INTO the engine, and the payloads
	// still leave.
	//
	// The configured list is unioned in rather than replaced, because a
	// project need not be in the group at all: `provisioning.projects`
	// takes any path this credential can administer.
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
