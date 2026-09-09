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
// THE HOOKS AND NOTHING ELSE. A GitHub pass registers deliveries and reads
// what a seat already has; it creates no account, so a disconnect has no seat
// of this engine's making to remove.
//
// BOTH LEVELS ARE SWEPT, not whichever one the current config would choose.
// The pass registers an org hook when it can and falls back to one hook per
// repository when it cannot, and that choice moves with the credential's
// scopes and with `org_webhook`. A teardown that only looked where TODAY's
// config would put a hook would leave every hook the other branch had ever
// created, so both are swept whatever the mode says.
//
// Matched on the delivery URL, the same rule the reconcile uses and for the
// same reason: GitHub gives a webhook no name, and an organization carries
// hooks other integrations registered. A sweep that took the first hook it
// found would delete somebody else's integration.
//
// EVERY TARGET IS ATTEMPTED before the first failure is returned. One
// repository a token cannot administer must not strand the hooks on all the
// others; the errors are joined so the operator sees every one at once rather
// than one per retry.
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
	// `integrations.github.token` is optional on this host, and nothing at
	// runtime needs it any more — each agent acts through its own app. So a
	// nil client is the documented posture of a working company, not a
	// misconfiguration, and returning an error for one made the disconnect
	// UNCOMPLETABLE: the block is dropped only after the vendor step
	// succeeds, so the surface sat "Disconnecting" and retried for the life
	// of the deployment.
	//
	// What the engine cannot do it says plainly instead. The hooks it can no
	// longer list are named for the operator to remove, which is the same
	// bargain the app registration itself takes: GitHub offers no API to
	// delete an app, so the engine uninstalls what it can and hands over a
	// link for the rest.
	if opts.Client == nil {
		log.Warn("github_hooks_not_removed",
			"targets", hookTargetNames(pv),
			"delivery_url", target,
			"detail", "no integrations.github.token resolved, so this engine "+
				"cannot list or delete the webhooks it registered: remove any "+
				"hook pointing at this address by hand")
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
func hookTargetNames(pv *config.GitHubProvisioning) []string {
	var out []string
	if org := strings.TrimSpace(pv.Org); org != "" {
		out = append(out, org)
	}
	for _, t := range TargetsOf(pv) {
		out = append(out, t.Owner+"/"+t.Repo)
	}
	return out
}
