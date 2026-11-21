package jira

import (
	"context"
	"fmt"
)

// Teardown removes what this engine registered at a Jira instance.
//
// THE WEBHOOK AND NOTHING ELSE: a Jira pass creates no account and issues no
// credential at the instance, so a hook is all a disconnect withdraws.
//
// Matched by [ours] (the configured name, delivery path /webhooks/jira), so a
// hook sharing the name but pointing elsewhere is left alone.
//
// SAFE TO REPEAT. A failed teardown is retried, so every step is "remove this
// if it is there": a hook already gone is not an error, and a pass that finds
// none has finished.
func Teardown(ctx context.Context, opts Options) error {
	if opts.Client == nil {
		return fmt.Errorf("jira: no client")
	}
	// NO PUBLIC BASE IS NOT A REASON TO STOP: the hook is identified by
	// its NAME (see [ours]), which survives a deployment losing or
	// changing its base, and a disconnect that walked away from a live hook
	// would leave the instance delivering to it for ever.
	hooks, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("jira: list webhooks to remove them: %w", err)
	}
	// BY THE SAME RULE THE RECONCILE CONVERGES ON — see [ours]. Matching on
	// the address alone left every hook a previous public base created:
	// this engine's own registrations, still enabled, delivering where
	// nothing answers, surviving the disconnect that was meant to remove
	// them. The two halves of one integration must not disagree about
	// which hook is this deployment's.
	name := opts.Config.WebhookNameOrDefault()
	for _, hook := range hooks {
		if !ours(hook, name) {
			continue
		}
		if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
			return fmt.Errorf("jira: remove webhook %s: %w", hook.ID, err)
		}
	}
	return nil
}
