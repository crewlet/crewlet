package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// Teardown removes the webhooks this engine registered at GitHub.
//
// THE HOOKS AND NOTHING ELSE: the pass creates no account.
//
// BOTH LEVELS ARE SWEPT, whatever the mode says: the pass falls back from an
// org hook to per-repository hooks as scopes and `org_webhook` move, so
// looking only where TODAY's config would put one leaves the other branch's.
// Matched on the delivery URL, as the reconcile does: a webhook has no name
// and an organization carries other integrations' hooks.
//
// EVERY TARGET IS ATTEMPTED before the first failure is returned, so one
// repository a token cannot administer strands no others; errors are joined.
func Teardown(ctx context.Context, opts Options) error {
	if opts.Config == nil {
		return errors.New("github: no github config")
	}
	target := webhookTarget(opts.WebhookBase)
	if target == "" {
		// Nothing was ever registered without a base, so there is nothing
		// to withdraw and the disconnect finishes.
		return nil
	}
	pv := opts.Config.Provisioning
	if pv == nil {
		return nil
	}
	// NO CREDENTIAL IS NOT A FAULT, and this check sits BELOW the two above
	// so that the ordinary no-op cases never reach it.
	//
	// `integrations.github.token` is optional (each agent acts through its
	// own app), so a nil client is a working company's posture. Erroring
	// made the disconnect UNCOMPLETABLE: the block is dropped only after
	// this step succeeds, so the surface retried "Disconnecting" for ever.
	// What the engine cannot list it names for the operator to remove.
	if opts.Client == nil {
		// SAID ONLY WHERE A HOOK COULD BE, and said as a place to LOOK
		// rather than as a hook that is there.
		//
		// It named every container in `provisioning` unconditionally, so a
		// company with `org_webhook: never` was told to go and clean up
		// hooks at an organization this engine never registered one on —
		// measured on a live disconnect, where the one named container
		// held nothing but an unrelated third-party hook. The engine has
		// no credential here and therefore cannot know what exists; what
		// it does know is where it would have put one, and the sentence
		// now claims exactly that much.
		if where := hookTargetNames(pv); len(where) > 0 {
			log.Warn("github_hooks_not_removed",
				"may_hold_a_hook", where,
				"delivery_url", target,
				"detail", "no integrations.github.token resolved, so this engine "+
					"cannot list or delete the webhooks it registered: check "+
					"these for a hook pointing at this address and remove it. "+
					"There may be none: this engine registers one where it "+
					"can and cannot look now")
		}
		return nil
	}

	var failures []error
	if org := strings.TrimSpace(pv.Org); org != "" {
		hooks, err := opts.Client.OrgWebhooks(ctx, org)
		switch {
		case err != nil:
			// A credential without admin:org_hook cannot LIST them
			// either, and one was never registered with it. Recorded
			// rather than fatal, so the repository sweep still runs.
			failures = append(failures,
				fmt.Errorf("github: list %s's hooks to remove them: %w", org, err))
		default:
			for _, hook := range hooks {
				if hook.URL != target {
					continue
				}
				if err := opts.Client.DeleteOrgWebhook(ctx, org, hook.ID); err != nil {
					failures = append(failures,
						fmt.Errorf("github: remove %s's hook: %w", org, err))
				}
			}
		}
	}

	for _, t := range TargetsOf(pv) {
		hooks, err := opts.Client.RepoWebhooks(ctx, t.Owner, t.Repo)
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"github: list %s/%s's hooks to remove them: %w", t.Owner, t.Repo, err))
			continue
		}
		for _, hook := range hooks {
			if hook.URL != target {
				continue
			}
			if err := opts.Client.DeleteRepoWebhook(ctx, t.Owner, t.Repo, hook.ID); err != nil {
				failures = append(failures, fmt.Errorf(
					"github: remove %s/%s's hook: %w", t.Owner, t.Repo, err))
			}
		}
	}
	return errors.Join(failures...)
}

// hookTargetNames is where a hook this engine registered may still be, for an
// operator who has to go and remove them by hand.
// EXCLUDING WHAT THE MODE FORBIDS. `org_webhook: never` means the pass never
// registered an organization hook, so naming the organization sends an
// operator to a settings page with nothing of this engine's on it.
func hookTargetNames(pv *config.GitHubProvisioning) []string {
	var out []string
	if org := strings.TrimSpace(pv.Org); org != "" &&
		pv.OrgWebhook != config.ContainerWebhookNever {
		out = append(out, org)
	}
	for _, t := range TargetsOf(pv) {
		out = append(out, t.Owner+"/"+t.Repo)
	}
	return out
}
