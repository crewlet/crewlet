package confluence

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// Reconcile brings a Confluence instance's inbound hooks in line with the
// company: on Cloud one token-bearing hook per event, on Data Center one
// signed hook for all of them.
//
// It is a reconcile, not a setup: running it twice is safe and quiet. A hook
// pointing elsewhere is re-pointed, and one the operator made by hand is
// never touched: only hooks named under [HookNamePrefix] are this engine's.
//
// The Cloud token lives in the delivery URL, so re-minting it re-registers
// every hook and invalidates the token the running engine holds. It is minted
// only when the config's ${VAR} resolves to nothing, or when the operator
// asked to recreate the hooks having planned the restart.

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

	// WebhookBase is where the instance reaches this deployment
	// ([config.API.WebhookBase]: the public listener's address where
	// `api.public` is set), or empty to skip registration.
	//
	// SKIPPED RATHER THAN GUESSED: a hook pointing at the wrong host is
	// worse than no hook, because the instance then reports a healthy
	// integration that delivers into the void.
	WebhookBase string

	// WebhookBaseKey is the Tier A key WebhookBase is read from
	// ([config.API.WebhookBaseKey]), which a run with no address names as
	// the setting to fill in. Empty is [setup.BaseKey]'s.
	WebhookBaseKey string

	// Recreate deletes and remakes every hook to mint a fresh token.
	// Destructive: it invalidates the token every other deployment of
	// this company holds.
	//
	// Spelled as the caller spells its switch ([provision.FreshSwitch]):
	// `-recreate-webhooks` from `crewlet confluence provision`,
	// `recreate_webhooks` from the setup route. A refusal of it names
	// that spelling, never this package's guess at one.
	Recreate provision.FreshSwitch
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
		rejected := integration.Reject(err, Status(err))
		return nil, fmt.Errorf(
			"confluence: the org credential in integrations.confluence.token %s, "+
				"so nothing else this run reports would be trustworthy: %w",
			integration.Refusal(rejected), rejected)
	}
	res := &Result{Deployment: opts.Client.Deployment(), Account: account}

	err = converge(ctx, opts, res)
	if err == nil {
		// A PASS THAT WAS CUT SHORT HAS NOT SEEN THE WORLD IT REPORTS ON.
		// On a converged instance the walk makes no request and answers
		// with no findings, the loop's word for "ready", so a context that
		// died after the listing would report a healthy Confluence it had
		// stopped reading. It is not only shutdown: the pass's context is
		// bounded by its lease, so a slow instance hits the same deadline.
		err = cutShort(ctx)
	}
	if err != nil {
		// THE SINK IS FLUSHED ON THE WAY OUT, whichever way that is.
		// mintInto seals a fresh value BEFORE the hooks carrying it are
		// registered, and the resolver's snapshot is rebuilt only inside
		// Flush. Without one a minted value stays sealed and INVISIBLE:
		// every later pass mints another, for as long as the failure
		// lasts, the runaway the refreshing sink exists to prevent.
		if flushErr := flushSink(context.WithoutCancel(ctx), opts); flushErr != nil {
			return res, errors.Join(err, flushErr)
		}
		return res, err
	}
	if err := flushSink(ctx, opts); err != nil {
		return res, err
	}
	return res, nil
}

// converge is the pass's own work: the hooks, on whichever deployment this
// is, or nothing at all when there is no address to point them at.
//
// A FUNCTION RATHER THAN A BLOCK INSIDE [Reconcile] so that every exit,
// the no-base one included, reaches the cutShort fold: an early return
// straight out of Reconcile would answer "ready" (no findings, no error) for
// a pass that ran out of context. [Client.Me] failing a dead context does not
// make that unreachable: it covers a cancellation, not a DEADLINE, and the
// pass's context is bounded by its lease, so an expiry between Me and this
// line is ordinary. A company with no public base URL has no later request
// to notice on.
func converge(ctx context.Context, opts Options, res *Result) error {
	base := strings.TrimRight(strings.TrimSpace(opts.WebhookBase), "/")
	if base == "" {
		res.Notes = append(res.Notes,
			"no webhook was registered: set "+setup.BaseKey(opts.WebhookBaseKey)+
				" or pass -public-url to register one. Without it the instance "+
				"delivers nothing and the integration looks idle rather than "+
				"unconfigured")
		return nil
	}
	switch res.Deployment {
	case Cloud:
		return reconcileCloud(ctx, opts, base, res)
	default:
		return reconcileDataCenter(ctx, opts, base, res)
	}
}

// flushSink completes the run's sink, where there is one.
//
// The context is the CALLER'S on the success path and an uncancellable copy
// of it on the failure path, which is the rule every rollback in this tree
// follows: the failure being cleaned up after is often the cancellation
// itself, and a flush that inherits a dead context does nothing at all. The
// success path keeps the deadline, because there a flush that hangs is a pass
// that never returns.
func flushSink(ctx context.Context, opts Options) error {
	if opts.Sink == nil {
		return nil
	}
	if err := opts.Sink.Flush(ctx); err != nil {
		return fmt.Errorf("confluence: %w", err)
	}
	return nil
}

// cutShort reports the pass's own failure when its context has died, and nil
// while the pass may honestly carry on.
//
// THE TWO ANSWERS ARE OPPOSITE TO THE LOOP, which is the whole of
// integrationtest's seventh clause: an error is a fault it retries, and a
// findings list — empty most of all — is a statement about the operator's
// world. A pass that kept walking on a dead context can only produce the
// second, because every event it never reached records nothing.
func cutShort(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf(
			"confluence: this pass stopped before it had finished converging the "+
				"instance's hooks, so what it observed is partial rather than "+
				"a healthy integration: %w", err)
	}
	return nil
}

// reconcileCloud converges one token-bearing hook per event.
func reconcileCloud(ctx context.Context, opts Options, base string, res *Result) error {
	token, err := cloudToken(ctx, opts, res)
	if err != nil {
		return err
	}

	existing, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("confluence: list webhooks: %w", err)
	}
	// INDEXED BY NAME, and the name is where this pass's namespace rule is
	// enforced: every lookup below is keyed on HookName(event), which always
	// carries [HookNamePrefix], so a hook an operator registered under any
	// other name is never found and never touched.
	//
	// There was a prefix filter here too, dropping foreign hooks as the index
	// was built. It was a second copy of that same rule and an unfalsifiable
	// one — measured: removing it changes no behaviour any test in this
	// package can see, because nothing reads this map except through a
	// prefixed key. One rule at the point it is enforced is what this tree
	// asks for everywhere else, and TestCloudLeavesForeignHooksAlone pins the
	// guarantee itself rather than one of two spellings of it.
	byName := make(map[string]Webhook, len(existing))
	for _, hook := range existing {
		byName[hook.Name] = hook
	}

	// ONE EVENT'S REFUSAL IS NOT THE PASS'S. Returning an error here would
	// skip the remaining events and never reach Findings(), leaving
	// [HookState.Detail] unset. The loop reads the two answers oppositely:
	// an error is a FAULT, retried on the waiting backoff for ever, while an
	// ingress block is degraded and owed by the admin who can grant the
	// permission. So a refusal is recorded per event and the walk continues,
	// as github.ensureRepoWebhook does.
	for _, event := range WebhookEvents {
		// STOP WORKING ON A DEAD CONTEXT rather than firing seven more
		// doomed registrations at the instance and recording each refusal
		// as this event's own. The walk below is the one place a single
		// pass makes eight decisions, so it is the one place a context that
		// died halfway through turns into a report about Confluence.
		if err := cutShort(ctx); err != nil {
			return err
		}
		name := HookName(event)
		target := CloudWebhookTarget(base, token, event)
		state := HookState{Event: event}

		current, found := byName[name]
		switch {
		case found && opts.Recreate.Asked():
			if err := opts.Client.DeleteWebhook(ctx, current.ID); err != nil {
				if stop := blame(ctx, &state, err); stop != nil {
					return stop
				}
				res.Hooks = append(res.Hooks, state)
				continue
			}
			// No `continue` on success: falling out of the switch
			// reaches the create below, which is the second half of a
			// replace.
		case found && converged(current, target, event):
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
				if stop := blame(ctx, &state, err); stop != nil {
					return stop
				}
				res.Hooks = append(res.Hooks, state)
				continue
			}
			state.URL = target
			res.Hooks = append(res.Hooks, state)
			continue
		}
		if _, err := opts.Client.CreateWebhook(ctx, name, target, []string{event}, ""); err != nil {
			if stop := blame(ctx, &state, err); stop != nil {
				return stop
			}
			res.Hooks = append(res.Hooks, state)
			continue
		}
		state.URL, state.Created = target, true
		res.Hooks = append(res.Hooks, state)
	}
	return nil
}

// converged reports a Cloud hook that already carries everything this pass
// would write.
//
// ENABLED IS PART OF IT, and it was the piece being thrown away. Webhook.Enabled
// is parsed off the wire on every pass and was read nowhere, so a hook at the
// right address for the right event that the instance had DISABLED was
// stamped as hooked and reported Ready — the one field that says whether it
// delivers anything, fetched every pass and discarded at the only point a
// decision was made. Both writers already send Enabled: true, so falling
// through to the update is what re-asserts it.
func converged(hook Webhook, target, event string) bool {
	return hook.Enabled && SameTarget(hook.URL, target) &&
		sameEvents(hook.Events, event)
}

// refusal is what a per-event failure records, in terms an operator can act
// on.
//
// Through [integration.Reject], so a 401 or 403 from the hooks endpoint is
// marked as the credential refusal it is rather than folded in with the
// transport faults that clear on their own — the same treatment the identity
// probe in this file already gets, and which the webhook calls did not.
func refusal(err error) string {
	return integration.Reject(err, Status(err)).Error()
}

// blame decides WHOSE failure a per-event write is, and it is not always the
// event's.
//
// Confluence refusing one registration is that event's: it is recorded on the
// state and the walk carries on. A dead context refuses all eight identically,
// and recording that eight times would claim the instance blocked eight event
// classes, sending an ADMINISTRATOR to grant a permission that was never
// missing. So a dead context stops the walk and becomes the pass's error, and
// only a refusal the instance actually made is written onto the state.
func blame(ctx context.Context, state *HookState, err error) error {
	if stop := cutShort(ctx); stop != nil {
		return stop
	}
	state.Detail = refusal(err)
	return nil
}

// dataCenterHookEvent is the pseudo-event the single Data Center hook is
// recorded under, and the name it is registered as.
//
// A constant because three things now read it and a typo in any one of them
// is silent: the registration's name, the [HookState] the pass records, and
// the sentence [Result.Findings] writes when that hook could not be
// established.
const dataCenterHookEvent = "all"

// reconcileDataCenter converges the single signed hook Data Center wants.
//
// A REFUSAL FROM THE INSTANCE IS THIS HOOK'S, NOT THE PASS'S, as in the Cloud
// walk above. A 403 (an org account without the Confluence Administrator
// global permission) must not leave [Reconcile] as an error: Classify reads
// that as a FAULT, retried on the waiting backoff for ever, where
// FindingIngressBlocked is degraded and owed by the ADMINISTRATOR who can
// grant the permission.
//
// It costs more here than on Cloud: the single hook carries every event, so a
// refusal means no Confluence event reaches the engine at all. A dead context
// is still the pass's, through the same [blame].
func reconcileDataCenter(ctx context.Context, opts Options, base string, res *Result) error {
	secret, minted, err := dataCenterSecret(ctx, opts, res)
	if err != nil {
		return err
	}
	target := base + "/webhooks/confluence"
	name := HookName(dataCenterHookEvent)
	state := HookState{Event: dataCenterHookEvent}

	existing, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return fmt.Errorf("confluence: list webhooks: %w", err)
	}
	// BY NAME, as the Cloud half and [Teardown] do; the three must agree
	// about which hook is this engine's. Matching on URL would read a
	// MOVED hook as "not mine" (a second registration, the first left
	// enabled with a valid secret) and ADOPT an operator's hand-made hook
	// at the same address, against the Reconcile doc.
	for _, hook := range existing {
		if hook.Name != name {
			continue
		}
		if opts.Recreate.Asked() {
			if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
				if stop := blame(ctx, &state, err); stop != nil {
					return stop
				}
				// NO FALL-THROUGH TO THE CREATE. The hook this run was
				// asked to replace is still registered, still enabled and
				// still carrying the previous secret, so creating a second
				// one would leave the instance delivering twice — once to
				// an address whose key the engine has replaced.
				res.Hooks = append(res.Hooks, state)
				return nil
			}
			break
		}
		// ALREADY CORRECT, as on the Cloud branch: the pass runs every few
		// minutes, so no unconditional PUT. BUT NOT WHEN THIS RUN MINTED THE
		// KEY: a Data Center URL carries no token ([SameAddress] drops the
		// query) and Confluence never reads a secret back, so the checks
		// below cannot see a new key. It would never be sent, the instance
		// would keep signing with the old one and every delivery would be
		// refused by a surface reporting ready, permanently.
		if !minted && hook.Enabled && SameAddress(hook.URL, target) &&
			sameEventSet(hook.Events, WebhookEvents) {
			state.URL = target
			res.Hooks = append(res.Hooks, state)
			return nil
		}
		if _, err := opts.Client.UpdateWebhook(ctx, hook.ID, name, target, WebhookEvents, secret); err != nil {
			if stop := blame(ctx, &state, err); stop != nil {
				return stop
			}
			res.Hooks = append(res.Hooks, state)
			return nil
		}
		state.URL = target
		res.Hooks = append(res.Hooks, state)
		return nil
	}
	if _, err := opts.Client.CreateWebhook(ctx, name, target, WebhookEvents, secret); err != nil {
		if stop := blame(ctx, &state, err); stop != nil {
			return stop
		}
		res.Hooks = append(res.Hooks, state)
		return nil
	}
	state.URL, state.Created = target, true
	res.Hooks = append(res.Hooks, state)
	return nil
}

// sameEventSet reports a registration already carrying every event this
// engine subscribes to.
//
// EXTRA events are not a reason to write: an operator who added one wanted
// it, and rewriting the list every pass to take it away again is the opposite
// of converging what this engine needs.
func sameEventSet(have, want []string) bool {
	for _, event := range want {
		if !slices.Contains(have, event) {
			return false
		}
	}
	return true
}

// cloudToken is the value the Cloud hooks carry, minted only where nothing
// usable resolves.
// The minted flag is DROPPED here, and only here: a Cloud hook carries the
// token in its own URL, so a fresh one changes the target and [SameTarget]
// — which compares the query — already reports the hook as needing a write.
// The Data Center half has no such tell, which is what its flag is for.
func cloudToken(ctx context.Context, opts Options, res *Result) (string, error) {
	value, _, err := mintInto(ctx, opts, res, opts.Config.WebhookToken, "webhook_token",
		"the token every Cloud hook carries in its URL")
	return value, err
}

// dataCenterSecret is the HMAC key the Data Center hook is signed with.
func dataCenterSecret(ctx context.Context, opts Options, res *Result) (string, bool, error) {
	return mintInto(ctx, opts, res, opts.Config.WebhookSecret, "webhook_secret",
		"the key the instance signs every delivery with")
}

// mintInto resolves a config reference, minting a fresh value into its ${VAR}
// when it holds nothing.
//
// Minting every run would be an outage: the engine runs with the OLD value, so
// a fresh registration fails every delivery at the edge. A value that already
// resolves is used as it is; minting happens when there is none or when the
// operator asked to recreate the hooks having planned the restart.
//
// It also reports WHETHER it minted: Confluence never gives a secret back, so
// whether THIS run replaced the value is the only fact that settles a
// converged check, and it is known here alone. See [reconcileDataCenter]; the
// Jira sibling threads the same flag ([jira.converged]).
func mintInto(
	ctx context.Context, opts Options, res *Result, ref, field, role string,
) (string, bool, error) {
	var resolved string
	if opts.Value != nil {
		resolved = strings.TrimSpace(opts.Value(ref))
	}
	if resolved != "" && !opts.Recreate.Asked() {
		return resolved, false, nil
	}
	variable, ok := provision.SoleVar(ref)
	if !ok && opts.Recreate.Asked() {
		// A VALUE THE DOCUMENT OWNS, which the switch asked to replace:
		// refused naming the switch as its caller spells it and both
		// ways on ([provision.RefuseFresh]), never as a slot that
		// resolved to nothing, which is what the refusal below is about.
		if err := provision.RefuseFresh("integrations.confluence."+field,
			opts.Recreate, ref); err != nil {
			return "", false, fmt.Errorf("confluence: %w", err)
		}
	}
	if !ok {
		// THE SHAPE, NEVER THE VALUE. See [provision.Shape]: this error
		// becomes State.LastError, which the fleet stores and the
		// integrations query serves, so a %q of a literal webhook token
		// publishes the one credential that route authenticates with.
		return "", false, fmt.Errorf(
			"confluence: integrations.confluence.%s is %s rather than a value "+
				"this run could resolve or a whole ${VAR} reference to mint "+
				"one into; point it at a variable and set that variable, "+
				"or drop -public-url and register the hooks by hand",
			field, provision.Shape(ref))
	}
	secret, err := provision.MintSecret(ctx, opts.Sink, variable, opts.Recreate.Asked(),
		func() (string, error) { return rand.Text(), nil })
	if err != nil {
		return "", false, fmt.Errorf("confluence: %w", err)
	}
	if !secret.Minted {
		// ALREADY SEALED, by a pass whose value the resolver has not
		// caught up with. Nothing was minted, so the note below would
		// claim a rotation that did not happen.
		return secret.Value, false, nil
	}
	value := secret.Value
	// SAID IN THE NOTES, which is the one place a reader looks. There was a
	// Result.Recorded counter beside this line as well, and nothing read it:
	// printConfluenceHooks renders Deployment, Account, Hooks and Notes, and
	// the Recorded counts that are read are GitLab's, Mattermost's and
	// Slack's, each deciding its own report's Next: line. A second, silent
	// record of the same fact is a field waiting for its first reader to
	// trust a number nothing keeps true, so the note below is the whole
	// report and the counter is gone.
	note := fmt.Sprintf("a fresh value was minted into %s (%s), and %s",
		variable, role, opts.Sink.NextStep())
	if opts.Recreate.Asked() {
		note += ". The previous value is now invalid on every other deployment of this company"
	}
	res.Notes = append(res.Notes, note)
	return value, true, nil
}

// sameEvents reports whether a hook subscribes to exactly one event.
func sameEvents(have []string, want string) bool {
	return len(have) == 1 && have[0] == want
}

// unhooked is the sentence an administrator reads about a hook that could
// not be established, and the two deployments do not lose the same thing.
//
// A Cloud hook covers ONE event, because the payload names none and the path
// is all that knows which fired, so a refusal there costs that event class
// and the other seven keep arriving. Data Center registers a single signed
// hook for every event at once, so the same refusal is total: nothing
// reaches the engine at all. Rendering both through the Cloud sentence read
// "no webhook for all, so those events reach nobody", which understates the
// worse of the two while barely parsing.
func unhooked(hook HookState) string {
	if hook.Event == dataCenterHookEvent {
		// "POINTS AT" rather than "is registered", because both ways this
		// hook can be missing end here: one the pass could not create, and
		// one it could not re-point after the engine moved. The second
		// leaves a registration in place at an address that no longer
		// answers, so a sentence saying nothing is registered would send
		// somebody to the instance to look for what is plainly there.
		return fmt.Sprintf(
			"no webhook points at this engine, so NO Confluence event reaches it: %s",
			detailOr(hook.Detail))
	}
	return fmt.Sprintf("no webhook for %s, so those events reach nobody: %s",
		hook.Event, detailOr(hook.Detail))
}

// detailOr keeps a finding from ending in a dangling colon.
//
// The fallback the sibling github.detailOr has and this one did not: every
// path that leaves a hook unregistered now records a reason, but a state
// arriving from anywhere else would have rendered "…reach nobody: ".
func detailOr(detail string) string {
	if trimmed := strings.TrimSpace(detail); trimmed != "" {
		return trimmed
	}
	return "the credential may not register one"
}

// Findings reads this run as the integration-neutral vocabulary.
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
			Detail:  unhooked(hook),
		})
	}
	return out
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error.
//
// The same accessor GitLab and Mattermost export, for the same reason: a
// caller deciding what a refusal MEANS needs the number, and the meaning is
// decided once, in [integration.Reject], rather than per integration.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}
