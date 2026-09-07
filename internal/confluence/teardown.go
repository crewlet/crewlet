package confluence

import (
	"context"
	"fmt"
	"strings"
)

// Teardown removes the hooks this engine registered at a Confluence site.
//
// THE WEBHOOKS AND NOTHING ELSE, for the same reason as Jira's: a Confluence
// pass creates no account and issues no credential at the site, so there are
// no seats this engine made and none for a disconnect to remove.
//
// Matched by the NAME PREFIX rather than by target, which is the opposite of
// Jira's rule and follows how each third-party app lets a hook be identified.
// Confluence registers one hook per event, each carrying the event's own
// token in its URL, so a target comparison would need the token this engine
// no longer wants to resolve; the [HookNamePrefix] every one of them is
// created under names the whole set in one test. It is a namespace this
// engine owns, which is why [HookNamePrefix] exists.
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
