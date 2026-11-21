package confluence

import (
	"context"
	"fmt"
	"strings"
)

// Teardown removes the hooks this engine registered at a Confluence site.
//
// THE WEBHOOKS AND NOTHING ELSE, as in Jira: a Confluence pass creates no
// account or credential at the site, so there are no seats to remove.
//
// Matched by the NAME PREFIX rather than by target, the opposite of Jira's
// rule: each per-event hook carries its own token in its URL, so a target
// comparison would need a token this engine no longer resolves, while
// [HookNamePrefix] is a namespace this engine owns and names the whole set.
//
// SAFE TO REPEAT: a hook already gone is not an error, and a pass finding
// none has finished rather than failed.
func Teardown(ctx context.Context, opts Options) error {
	if opts.Client == nil {
		return fmt.Errorf("confluence: no client")
	}
	hooks, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("confluence: list webhooks to remove them: %w", err)
	}
	for _, hook := range hooks {
		if !strings.HasPrefix(hook.Name, HookNamePrefix) {
			continue
		}
		if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
			return fmt.Errorf("confluence: remove webhook %q: %w", hook.Name, err)
		}
	}
	return nil
}
