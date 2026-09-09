package jira

import (
	"context"
	"fmt"
)

// Teardown removes what this engine registered at a Jira instance.
//
// THE WEBHOOK AND NOTHING ELSE. A Jira pass creates no account and issues no
// credential at the instance — the org token in the company document is a
// credential somebody already had — so a hook is the whole of what a
// disconnect has to withdraw. There is no seat removal here because there are
// no seats this engine made.
//
// Matched by TARGET rather than by name. A hook's name is whatever it was
// created under and an administrator may have renamed it; its target is this
// deployment's own webhook address, which nothing else would be pointing at.
// Renaming a hook must not strand it, and adopting somebody else's hook
// because it shares a name would delete an integration this engine never
// made.
//
// SAFE TO REPEAT. A failed teardown is retried, so every step is "remove this
// if it is there": a hook already gone is not an error, and the pass that
// finds none has finished rather than failed.
func Teardown(ctx context.Context, opts Options) error {
	if opts.Client == nil {
		return fmt.Errorf("jira: no client")
	}
	// NO PUBLIC BASE IS NOT A REASON TO STOP ANY MORE. It was, while a
	// hook was identified by its address: with no base there was no
	// address to compare against. The hook is identified by its NAME now
	// (see [ours]), which is exactly the identity that survives a
	// deployment losing or changing its base — and a disconnect that
	// walked away from a live hook because this node could not name its
	// own address would leave the instance delivering to it for ever.
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
