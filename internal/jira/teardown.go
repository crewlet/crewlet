package jira

import (
	"context"
	"fmt"
	"strings"
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
	base := strings.TrimRight(strings.TrimSpace(opts.WebhookBase), "/")
	if base == "" {
		// Nothing was ever registered without one, so there is nothing to
		// withdraw and the disconnect finishes.
		return nil
	}
	target := webhookTarget(base)

	hooks, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("jira: list webhooks to remove them: %w", err)
	}
	for _, hook := range hooks {
		if hook.URL != target {
			continue
		}
		if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
			return fmt.Errorf("jira: remove webhook %s: %w", hook.ID, err)
		}
	}
	return nil
}
