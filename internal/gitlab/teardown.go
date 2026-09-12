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

	// RemoveSeats deletes the service accounts this engine created.
	RemoveSeats bool

	// Mode is where those accounts are OWNED, and it decides which delete
	// route removes one.
	//
	// IT WAS NOT HERE AT ALL, so removeAccounts always sent the GROUP
	// delete — and [Client.DeleteServiceAccount] reads a 404 as success
	// ("unknown or already removed; both are the state the caller asked
	// for"). An instance-owned account therefore reported itself deleted
	// down a route that had never heard of it, and stayed live with every
	// credential it held. The reconcile path has had [deleteAccount] for
	// exactly this, whose own doc names the failure; the teardown never got
	// it.
	Mode Mode
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
func Teardown(ctx context.Context, opts TeardownOptions) (provision.Removed, error) {
	var removed provision.Removed
	if opts.Client == nil {
		return removed, errors.New("gitlab: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		return removed, nil
	}
	p := opts.Config.Provisioning
	group := strings.TrimSpace(p.Group)
	if group == "" {
		return removed, nil
	}

	var failures []error
	groupID, found, err := groupIDOf(ctx, opts.Client, group)
	switch {
	case err != nil:
		failures = append(failures, fmt.Errorf("gitlab: resolve group %q to remove what it holds: %w",
			group, err))
	case !found:
		// THE GROUP DOES NOT RESOLVE, WHICH IS NOT THE SAME AS DELETED —
		// and this arm used to read it as "the group is gone, so everything
		// in it went with it", fabricating a full [provision.Removed] for
		// every planned seat without making one request about any of them.
		// [Engine.forgetRemoved] then deleted those seats' sealed tokens.
		//
		// Three things are wrong with that reading. [Client.GroupByPath]
		// maps ANY 404 to not-found, and GitLab answers 404 for a group
		// that was renamed or moved, for a typo in `provisioning.group`,
		// and for a group the presenting credential cannot SEE — it does
		// not answer 403 for an unauthorized private resource. And an
		// account created in [ModeInstance] is a member of nothing and
		// survives its group being deleted outright, so a missing group
		// says nothing at all about it.
		//
		// SO THE ACCOUNTS ARE STILL ASKED ABOUT, one by one. That costs
		// nothing extra and answers the question honestly, because
		// [Client.UserByUsername] is an INSTANCE-level lookup that needs no
		// group: a seat whose account is genuinely absent is reported
		// removed on the instance's own authority, and one that is still
		// there is reported as a FAILURE naming it rather than as a
		// credential safe to delete.
		//
		// The hooks are skipped, and only here: they live at the group and
		// at its projects, so an address that does not resolve is one this
		// pass cannot reach either way.
		if opts.RemoveSeats {
			gone, errs := removeAccounts(ctx, opts, 0)
			removed, failures = gone, append(failures, errs...)
		}
		return removed, errors.Join(failures...)
	default:
		failures = append(failures, removeHooks(ctx, opts, groupID)...)
		if opts.RemoveSeats {
			gone, errs := removeAccounts(ctx, opts, groupID)
			removed, failures = gone, append(failures, errs...)
		}
	}
	return removed, errors.Join(failures...)
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

	for _, project := range opts.Config.Provisioning.Projects {
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

// removeAccounts deletes the service account this engine made for each seat.
//
// Resolved through [Username], which is the same derivation the pass creates
// them under, so this removes exactly what it made and cannot reach an
// account somebody else named. A seat with no account is already in the state
// a disconnect wants.
// # What it reports
//
// Every planned seat whose account is now absent from the instance, with the
// variable that seat's token lives in. AN END STATE rather than a delta — see
// [setup.Teardowner] — so a seat whose account was never there, or was removed
// by an earlier attempt, counts: its sealed token is dead either way, and a
// retry that reported only this call's own deletions would let the block drop
// with the values still resolving.
func removeAccounts(
	ctx context.Context, opts TeardownOptions, groupID int,
) (provision.Removed, []error) {
	var removed provision.Removed
	if opts.Plan == nil {
		return removed, nil
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
			// ALREADY GONE, and its credential with it.
			removed.Add(provision.Removal{
				Handle: seat.Handle, Role: seat.Role, Account: username,
				Secrets: secretsOf(seat),
			})
			continue
		}
		// DOWN THE ROUTE THAT OWNS IT. This always sent the group delete,
		// which answers 404 as success for an instance-owned account — so
		// the teardown reported every one of them removed while they
		// stayed live.
		if err := removeOne(ctx, opts, groupID, user.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: remove service account %s: %w", username, err))
			continue
		}
		removed.Add(provision.Removal{
			Handle: seat.Handle, Role: seat.Role, Account: username,
			Secrets: secretsOf(seat),
		})
	}
	return removed, failures
}

// removeOne deletes one account down the route its owner requires, which is
// the split [deleteAccount] makes on the reconcile path and for the same
// reason.
//
// A GROUP-OWNED ACCOUNT WITH NO GROUP TO DELETE IT THROUGH IS REFUSED, not
// sent down the route anyway. The group delete reads a 404 as success
// ("unknown or already removed; both are the state the caller asked for"), so
// addressing `/groups/0/service_accounts/N` would answer 404, report the
// account deleted, and leave it live with every credential it holds — the
// same trap [TeardownOptions.Mode] exists to close, reached by a different
// road. It happens when the configured group does not resolve; see the
// not-found arm of [Teardown], which is the only caller that passes zero.
func removeOne(ctx context.Context, opts TeardownOptions, groupID, userID int) error {
	if opts.Mode.Or() == ModeInstance {
		return opts.Client.DeleteInstanceServiceAccount(ctx, userID)
	}
	if groupID == 0 {
		return fmt.Errorf(
			"gitlab: %s does not resolve, so this account cannot be removed "+
				"through it — restore the group, correct "+
				"integrations.gitlab.provisioning.group, or supply a token "+
				"that can see it",
			strings.TrimSpace(opts.Config.Provisioning.Group))
	}
	return opts.Client.DeleteServiceAccount(ctx, groupID, userID)
}

// secretsOf is the variables one seat's credentials live in.
func secretsOf(seat provision.Seat) []string {
	var out []string
	for _, name := range []string{seat.TokenVar, seat.EmailVar} {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}
