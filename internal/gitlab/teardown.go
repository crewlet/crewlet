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
	// WHERE THIS COMPANY'S ACCOUNTS LIVE, settled ONCE and before anything
	// is touched. Both halves of removing an account — revoking its tokens
	// and deleting it — are addressed through the same owner, and a route
	// that does not resolve is a refusal naming the field to fix rather
	// than a fallback to the other owner's: asking /personal_access_tokens
	// about a group account answers an empty list, which reads as "nothing
	// to revoke" and is the silent half of the bug this ordering exists to
	// fix.
	scope, routeErr := accountRoute(opts, groupID)
	var failures []error
	// STRANDED, and named once. The route is a property of the COMPANY
	// rather than of a seat, so one sentence repeated per seat would be
	// the same fact N times in a report an operator reads.
	var stranded []string
	for _, seat := range opts.Plan.Seats {
		username := Username(opts.Config.Provisioning, seat.Handle)
		user, found, err := opts.Client.UserByUsername(ctx, username)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: look up %s to remove it: %w", username, err))
			continue
		}
		if !found {
			// ALREADY GONE, and its credential with it. Reported whatever
			// the route says, because "this account does not exist" is an
			// answer the instance gave and does not depend on how a
			// surviving one would have been removed.
			removed.Add(provision.Removal{
				Handle: seat.Handle, Role: seat.Role, Account: username,
				Secrets: secretsOf(seat),
			})
			continue
		}
		if routeErr != nil {
			// AN ACCOUNT THAT IS THERE AND CANNOT BE ADDRESSED. Nothing is
			// touched and nothing is reported removed, so its sealed value
			// stays where it is.
			stranded = append(stranded, username)
			continue
		}
		// THE TOKENS FIRST, THEN THE ACCOUNT, which is the order datadog's
		// teardown and mattermost's both argue for: a revoked token on a
		// live account is an agent that can do nothing, and a live token
		// on a removed account is a credential that works again the moment
		// anybody restores it. The first is the safer thing to be
		// interrupted at.
		//
		// IT IS NOT HYPOTHETICAL HERE. GitLab's service-account delete
		// BLOCKS rather than erases — measured on a live disconnect, which
		// left `crewlet-sre-lead` state: blocked, out of the group, and
		// holding one active token — while the engine deleted its own copy
		// of the value. So the company lost the credential and GitLab kept
		// a working one, on an account one click restores.
		if err := opts.Client.RevokeTokens(ctx, scope, user.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: revoke %s's tokens before removing it: %w", username, err))
			// AND THE ACCOUNT STAYS AS IT IS, so the state is one an
			// operator can see: an account still listed holding a
			// credential nobody could withdraw, rather than a blocked one
			// quietly holding a working token. The seat is NOT reported
			// removed either, so its sealed value stays where it is —
			// deleting the company's only copy of a live credential is
			// the one move nothing can undo.
			continue
		}
		// DOWN THE ROUTE THAT OWNS IT. This always sent the group delete,
		// which answers 404 as success for an instance-owned account — so
		// the teardown reported every one of them removed while they
		// stayed live.
		if err := removeOne(ctx, opts, scope, user.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"gitlab: remove service account %s: %w", username, err))
			continue
		}
		removed.Add(provision.Removal{
			Handle: seat.Handle, Role: seat.Role, Account: username,
			Secrets: secretsOf(seat),
		})
	}
	if len(stranded) > 0 {
		failures = append(failures, fmt.Errorf(
			"%w — these accounts are still live: %s",
			routeErr, strings.Join(stranded, ", ")))
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
// accountRoute is the owner this company's service accounts are addressed
// through: the group's id in group mode, and 0 for the INSTANCE's own.
//
// Zero is a real value here rather than "unset", which is why the group that
// does not resolve is an ERROR instead: an instance-mode 0 means
// /personal_access_tokens and /service_accounts, and falling back to it for
// an account the group owns asks about the wrong thing entirely — a token
// listing answers empty and a delete answers 404, both of which read as
// success.
func accountRoute(opts TeardownOptions, groupID int) (int, error) {
	if opts.Mode.Or() == ModeInstance {
		return 0, nil
	}
	if groupID == 0 {
		return 0, fmt.Errorf(
			"gitlab: %s does not resolve, so these accounts cannot be removed "+
				"through it — restore the group, correct "+
				"integrations.gitlab.provisioning.group, or supply a token "+
				"that can see it",
			strings.TrimSpace(opts.Config.Provisioning.Group))
	}
	return groupID, nil
}

func removeOne(ctx context.Context, opts TeardownOptions, scope, userID int) error {
	if opts.Mode.Or() == ModeInstance {
		return opts.Client.DeleteInstanceServiceAccount(ctx, userID)
	}
	return opts.Client.DeleteServiceAccount(ctx, scope, userID)
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
