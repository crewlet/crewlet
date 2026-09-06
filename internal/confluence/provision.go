package confluence

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// Reconcile brings a Confluence instance's inbound hooks in line with the
// company: on Cloud one token-bearing hook per event, on Data Center one
// signed hook for all of them.
//
// # It is a reconcile, not a setup
//
// Running it twice must be safe and quiet: a hook that already points where
// it should is left alone, one pointing elsewhere is re-pointed, and one the
// operator made by hand is never touched, because only hooks named under
// [HookNamePrefix] are this engine's to converge.
//
// # The token is minted once, not per run
//
// The Cloud token lives in the URL Confluence delivers to, so re-minting it
// re-registers every hook and invalidates the token the running engine
// holds, from a command whose whole promise is that it is safe to re-run. It
// is minted only when the config's ${VAR} resolves to nothing, or when the
// operator asked to recreate the hooks having planned the restart.

// Options are what one run needs.
type Options struct {
	// Client talks to the instance as the org account.
	Client *Client

	// Config is the company's confluence block, UNRESOLVED: a minted token
	// goes INTO its ${VAR}, so the reference has to survive.
	Config *config.Confluence

	// Value resolves a config value to what it holds.
	Value func(string) string

	// Sink records a minted token. Required only when one has to be
	// minted, which is why it is not checked up front.
	Sink provision.TokenSink

	// WebhookBase is this deployment's public base URL, or empty to skip
	// registration.
	//
	// SKIPPED RATHER THAN GUESSED: a hook pointing at the wrong host is
	// worse than no hook, because the instance then reports a healthy
	// integration that delivers into the void.
	WebhookBase string

	// Recreate deletes and remakes every hook to mint a fresh token.
	// Destructive: it invalidates the token every other deployment of
	// this company holds.
	Recreate bool
}

// HookState is what one hook looks like after the run.
type HookState struct {
	Event string
	// URL is the delivery address the hook now points at, or empty when
	// no hook could be established for this event.
	URL string
	// Created is true for a hook this run made, false for one it found
	// already correct or re-pointed.
	Created bool
	// Detail carries the refusal for an event that could not be hooked.
	Detail string
}

// Hooked reports a hook that is in place.
func (h HookState) Hooked() bool { return h.URL != "" }

// Result is what one reconcile found and did.
type Result struct {
	Deployment Deployment
	// Account is who the org credential authenticates as.
	Account string
	Hooks   []HookState
	Notes   []string
	// Recorded counts the values this run wrote to the sink.
	Recorded int
}

// Reconcile runs one pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("confluence: no client")
	}
	if opts.Config == nil {
		return nil, errors.New("confluence: no confluence config")
	}
	account, err := opts.Client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"confluence: the org credential in integrations.confluence.token was "+
				"refused, so nothing else this run reports would be trustworthy: %w",
			integration.Reject(err, Status(err)))
	}
	res := &Result{Deployment: opts.Client.Deployment(), Account: account}

	base := strings.TrimRight(strings.TrimSpace(opts.WebhookBase), "/")
	if base == "" {
		res.Notes = append(res.Notes,
			"no webhook was registered: set integrations.public_base_url or pass "+
				"-public-url to register one. Without it the instance delivers "+
				"nothing and the integration looks idle rather than unconfigured")
		return res, nil
	}

	switch res.Deployment {
	case Cloud:
		err = reconcileCloud(ctx, opts, base, res)
	default:
		err = reconcileDataCenter(ctx, opts, base, res)
	}
	if err != nil {
		return res, err
	}
	if opts.Sink != nil {
		if err := opts.Sink.Flush(ctx); err != nil {
			return res, fmt.Errorf("confluence: %w", err)
		}
	}
	return res, nil
}

// reconcileCloud converges one token-bearing hook per event.
func reconcileCloud(ctx context.Context, opts Options, base string, res *Result) error {
	token, notes, err := cloudToken(ctx, opts)
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		return err
	}

	existing, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("confluence: list webhooks: %w", err)
	}
	byName := make(map[string]Webhook, len(existing))
	for _, hook := range existing {
		if strings.HasPrefix(hook.Name, HookNamePrefix) {
			byName[hook.Name] = hook
		}
	}

	for _, event := range WebhookEvents {
		name := HookName(event)
		target := CloudWebhookTarget(base, token, event)
		state := HookState{Event: event}

		current, found := byName[name]
		switch {
		case found && opts.Recreate:
			if err := opts.Client.DeleteWebhook(ctx, current.ID); err != nil {
				return fmt.Errorf("confluence: replace webhook for %s: %w", event, err)
			}
			// No `continue`: falling out of the switch reaches the
			// create below, which is the second half of a replace.
		case found && SameTarget(current.URL, target) && sameEvents(current.Events, event):
			// ALREADY CORRECT, and left exactly as it is. This is the
			// steady state and the one a re-run spends most of its time
			// in; touching it would be a write per event per run for a
			// hook that needed nothing.
			state.URL = target
			res.Hooks = append(res.Hooks, state)
			continue
		case found:
			if _, err := opts.Client.UpdateWebhook(ctx, current.ID, name, target,
				[]string{event}, ""); err != nil {
				return fmt.Errorf("confluence: update webhook for %s: %w", event, err)
			}
			state.URL = target
			res.Hooks = append(res.Hooks, state)
			continue
		}
		if _, err := opts.Client.CreateWebhook(ctx, name, target, []string{event}, ""); err != nil {
			return fmt.Errorf("confluence: create webhook for %s: %w", event, err)
		}
		state.URL, state.Created = target, true
		res.Hooks = append(res.Hooks, state)
	}
	return nil
}

// reconcileDataCenter converges the single signed hook Data Center wants.
func reconcileDataCenter(ctx context.Context, opts Options, base string, res *Result) error {
	secret, notes, err := dataCenterSecret(ctx, opts)
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		return err
	}
	target := base + "/webhooks/confluence"
	name := HookName("all")

	existing, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("confluence: list webhooks: %w", err)
	}
	for _, hook := range existing {
		if hook.URL != target {
			continue
		}
		if opts.Recreate {
			if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
				return fmt.Errorf("confluence: replace webhook: %w", err)
			}
			break
		}
		if _, err := opts.Client.UpdateWebhook(ctx, hook.ID, name, target, WebhookEvents, secret); err != nil {
			return fmt.Errorf("confluence: update webhook: %w", err)
		}
		res.Hooks = append(res.Hooks, HookState{Event: "all", URL: target})
		return nil
	}
	if _, err := opts.Client.CreateWebhook(ctx, name, target, WebhookEvents, secret); err != nil {
		return fmt.Errorf("confluence: create webhook: %w", err)
	}
	res.Hooks = append(res.Hooks, HookState{Event: "all", URL: target, Created: true})
	return nil
}

// cloudToken is the value the Cloud hooks carry, minted only where nothing
// usable resolves.
func cloudToken(ctx context.Context, opts Options) (string, []string, error) {
	return mintInto(ctx, opts, opts.Config.WebhookToken, "webhook_token",
		"the token every Cloud hook carries in its URL")
}

// dataCenterSecret is the HMAC key the Data Center hook is signed with.
func dataCenterSecret(ctx context.Context, opts Options) (string, []string, error) {
	return mintInto(ctx, opts, opts.Config.WebhookSecret, "webhook_secret",
		"the key the instance signs every delivery with")
}

// mintInto resolves a config reference, minting a fresh value into its ${VAR}
// when it holds nothing.
//
// The tempting shape is to mint every run, and it is an outage: the engine is
// running with the OLD value, and re-registering with a fresh one makes every
// delivery fail verification at the edge. So a value that already resolves is
// used as it is, and minting happens when there is none or when the operator
// asked to recreate the hooks having planned the restart.
func mintInto(ctx context.Context, opts Options, ref, field, role string) (string, []string, error) {
	var resolved string
	if opts.Value != nil {
		resolved = strings.TrimSpace(opts.Value(ref))
	}
	if resolved != "" && !opts.Recreate {
		return resolved, nil, nil
	}
	variable, ok := provision.SoleVar(ref)
	if !ok {
		return "", nil, fmt.Errorf(
			"confluence: integrations.confluence.%s is %q, which is neither a "+
				"value this run could resolve nor a whole ${VAR} reference to "+
				"mint one into; point it at a variable and set that variable, "+
				"or drop -public-url and register the hooks by hand", field, ref)
	}
	if opts.Sink == nil {
		return "", nil, provision.ErrNoSink
	}
	value := rand.Text()
	if err := opts.Sink.Record(ctx, variable, value); err != nil {
		return "", nil, fmt.Errorf("confluence: record %s: %w", variable, err)
	}
	note := fmt.Sprintf("a fresh value was minted into %s (%s), and %s",
		variable, role, opts.Sink.NextStep())
	if opts.Recreate {
		note += ". The previous value is now invalid on every other deployment of this company"
	}
	return value, []string{note}, nil
}

// sameEvents reports whether a hook subscribes to exactly one event.
func sameEvents(have []string, want string) bool {
	return len(have) == 1 && have[0] == want
}

// Findings reads this run as the vendor-neutral vocabulary.
//
// Only what a run that was ASKED to register can establish. A run with no
// base registers nothing and reports nothing about ingress, for the reason
// jira.Result.Findings states: an empty hook list is what a read-only pass
// produces by construction, and reading it as "no webhook" would park every
// company on a block nobody can clear.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding
	for _, hook := range r.Hooks {
		if hook.Hooked() {
			continue
		}
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: hook.Event,
			Detail: fmt.Sprintf("no webhook for %s, so those events reach nobody: %s",
				hook.Event, hook.Detail),
		})
	}
	return out
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error.
//
// The same accessor GitLab and Mattermost export, for the same reason: a
// caller deciding what a refusal MEANS needs the number, and the meaning is
// decided once, in [integration.Reject], rather than per vendor.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}
