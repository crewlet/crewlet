package github

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Reconcile brings a GitHub deployment in line with the company config.
//
// # What GitHub lets a provisioner do, and what it does not
//
// The chat backend and the self-hosted code host can CREATE an account and
// mint its credential, so their reconciles are about converging a fleet of
// accounts. GitHub cannot, and the reason is not a missing scope: there is
// no API that creates a user, and the API that once created a token on
// somebody's behalf was withdrawn in 2020. A run that pretended otherwise
// would print instructions dressed as actions.
//
// So this reconcile does the two things GitHub genuinely allows, and each
// answers a question that is otherwise invisible until an event reaches
// nobody:
//
//   - WHICH ACCOUNT each seat's credential authenticates as. That mapping is
//     the whole of a seat's inbound routing, and nothing in the org model
//     declares it — the engine resolves it at boot and says nothing about
//     the seats it could not.
//   - THE INBOUND WEBHOOKS, registered with a secret the engine holds: one
//     on the organization where the credential may, otherwise one per
//     repository. Without them the deployment delivers nothing and the
//     integration looks idle rather than unconfigured.

// Options are one reconcile's inputs.
type Options struct {
	// Client talks to the deployment as the org account.
	Client *Client

	// Config is the company's github block, UNRESOLVED: a minted webhook
	// secret goes INTO its `${VAR}`, so the reference has to survive.
	Config *config.GitHub

	// Org is the company's org chart, for the seat walk.
	Org *org.Organization

	// Value resolves a config value — a literal or a `${VAR}` — to what it
	// holds. A function rather than the resolver itself, so this package
	// stays out of the config resolver's import graph.
	Value func(string) string

	// Sink records a minted webhook secret. Required only when one has to
	// be minted, which is why it is not checked up front: a run against a
	// deployment whose secret is already set has nothing to record.
	Sink provision.TokenSink

	// WebhookBase is this deployment's public base URL, or empty to skip
	// webhook registration.
	//
	// SKIPPED RATHER THAN GUESSED: a hook pointing at the wrong host is
	// worse than no hook, because GitHub then reports a healthy
	// integration that delivers into the void.
	WebhookBase string

	// RecreateWebhooks deletes and remakes every hook to mint a fresh
	// secret, for the case where the existing one's secret was lost.
	// Destructive: it invalidates the secret every other deployment of
	// this company holds.
	RecreateWebhooks bool
}

// SeatIdentity is one seat's code-host account, or why there is none.
type SeatIdentity struct {
	Handle string
	// Login is the account the seat's own credential authenticates as.
	// Empty means this seat receives NO GitHub events at all — which is
	// the one finding this command exists to surface.
	Login string
	// Reason says why an empty Login is empty, in terms an operator can
	// act on.
	Reason string
}

// Routes reports a seat whose inbound events can reach it.
func (s SeatIdentity) Routes() bool { return s.Login != "" }

// HookState is what one webhook target looks like after the run.
type HookState struct {
	Target Target
	// URL is the delivery address the hook now points at, or empty for a
	// target no hook was registered on.
	URL string
	// Created is true for a hook this run made, false for one it
	// converged.
	Created bool
	// Detail carries the refusal for a target that could not be hooked,
	// in terms an operator can act on.
	Detail string
}

// Hooked reports a target this run left with a working hook.
func (h HookState) Hooked() bool { return h.URL != "" }

// Result is what one reconcile found and did.
type Result struct {
	// Login is who the org credential authenticates as, or empty when the
	// run had no org credential to probe.
	Login string
	Seats []SeatIdentity
	Hooks []HookState
	Notes []string
}

// Routing reports the seats whose inbound events can reach them.
func (r *Result) Routing() int {
	var n int
	for _, seat := range r.Seats {
		if seat.Routes() {
			n++
		}
	}
	return n
}

// Reconcile runs one pass.
//
// The order is deliberate: PROBE the org credential, then read, then write.
// A run that registered webhooks before discovering the credential was dead
// would leave GitHub delivering to an engine that cannot enrich anything it
// receives.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("github: no client")
	}
	if opts.Config == nil {
		return nil, errors.New("github: no github config")
	}
	login, err := opts.Client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"github: the credential this run authenticates with was refused, "+
				"so nothing else it reported would be trustworthy: %w", err)
	}

	res := &Result{Login: login}
	res.Seats = resolveSeats(ctx, opts)

	hooks, notes, err := ensureWebhooks(ctx, opts)
	res.Hooks = hooks
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		return res, err
	}
	if opts.Sink != nil {
		if err := opts.Sink.Flush(ctx); err != nil {
			return res, fmt.Errorf("github: %w", err)
		}
	}
	return res, nil
}

// resolveSeats asks each seat's own credential who it is.
//
// CONCURRENTLY, because sequentially this is one round trip per seat and the
// whole point of the command is that an operator runs it and reads the
// answer. A seat whose lookup fails is reported unresolved rather than
// failing the run: the finding IS the report.
func resolveSeats(ctx context.Context, opts Options) []SeatIdentity {
	if opts.Org == nil {
		return nil
	}
	var seats []*org.Role
	for seat := range opts.Org.AllRoles() {
		if !seat.IsHuman() {
			seats = append(seats, seat)
		}
	}
	out := make([]SeatIdentity, len(seats))

	// The credentials are read first so the fan-out below runs over the
	// seats that actually have one: a seat with no token needs no lookup,
	// and letting it occupy a slot would idle part of the bound.
	tokens := make([]string, len(seats))
	var lookups []int
	for i, seat := range seats {
		out[i] = SeatIdentity{Handle: seat.Handle()}
		tokens[i] = CredentialOf(seat, opts.Value)
		if tokens[i] == "" {
			out[i].Reason = "no credential under mcp_env." + SeatEnv +
				" — this seat receives no GitHub events at all"
			continue
		}
		lookups = append(lookups, i)
	}

	// BOUNDED, at the same cap as the engine's own resolvers and for the
	// same reason — see [provision.IdentityLookups]. This path is the
	// operator-invoked `crewlet github provision`, so unbounded it opened
	// one socket per credentialled seat in a burst.
	provision.ResolveConcurrently(len(lookups), func(n int) {
		i := lookups[n]
		client, err := NewClient(ClientOptions{
			APIBase: opts.Client.APIBase(),
			WebBase: opts.Client.WebBase(),
			Token:   tokens[i],
		})
		if err != nil {
			out[i].Reason = err.Error()
			return
		}
		login, err := client.Me(ctx)
		if err != nil {
			out[i].Reason = err.Error()
			return
		}
		out[i].Login = login
	})
	return out
}

// ensureWebhooks registers the inbound hooks, or converges the ones already
// there.
//
// # The organization hook is tried first and is not required
//
// One org hook covers every repository in the organization, including ones
// created after this run — which is the difference between a new repository
// routing on day one and routing whenever somebody remembers. It needs
// `admin:org_hook`, which a fine-grained token cannot carry, so `auto` falls
// back to per-repository hooks rather than failing.
func ensureWebhooks(ctx context.Context, opts Options) ([]HookState, []string, error) {
	target := webhookTarget(opts.WebhookBase)
	if target == "" {
		return nil, []string{
			"no webhook was registered: pass the deployment's public base URL " +
				"to register one, or add it by hand — without it GitHub " +
				"delivers nothing and the integration looks idle rather than " +
				"unconfigured"}, nil
	}
	pv := opts.Config.Provisioning
	if pv == nil {
		return nil, []string{
			"no webhook was registered: integrations.github.provisioning is " +
				"unset, so this run has no organization and no repositories to " +
				"register one on"}, nil
	}

	secret, notes, err := webhookSecret(ctx, opts, target)
	if err != nil {
		return nil, notes, err
	}

	mode := config.ContainerWebhookAuto
	if pv.OrgWebhook != "" {
		mode = pv.OrgWebhook
	}
	var hooks []HookState

	if org := strings.TrimSpace(pv.Org); org != "" && mode != config.ContainerWebhookNever {
		state, err := ensureOrgWebhook(ctx, opts, org, target, secret)
		switch {
		case err == nil:
			hooks = append(hooks, state)
			if state.Hooked() {
				// ONE HOOK IS ENOUGH. Adding repository hooks beside a
				// working org hook would deliver every event twice —
				// deduped by delivery id, so the second is silent
				// waste rather than a double wake, but waste on every
				// event for ever.
				notes = append(notes, fmt.Sprintf(
					"one organization-level hook on %s covers every repository "+
						"in it, including ones created later — the repos list "+
						"was not hooked separately", org))
				return hooks, notes, nil
			}
		case mode == config.ContainerWebhookRequire:
			return hooks, notes, fmt.Errorf(
				"github: org_webhook: true demands one hook on %s and this "+
					"credential cannot register it (%w) — a classic token needs "+
					"the admin:org_hook scope, which a fine-grained token cannot "+
					"carry at all. Set org_webhook: false to hook each "+
					"repository instead", org, err)
		default:
			notes = append(notes, fmt.Sprintf(
				"no organization hook on %s (%s) — falling back to one hook per "+
					"repository, which does not cover repositories created later",
				org, err.Error()))
		}
	}

	targets := TargetsOf(pv)
	if len(targets) == 0 {
		notes = append(notes, "integrations.github.provisioning.repos is empty, "+
			"so there is nothing left to hook — name the repositories whose "+
			"events should reach the engine")
		return hooks, notes, nil
	}
	for _, t := range targets {
		hooks = append(hooks, ensureRepoWebhook(ctx, opts, t, target, secret))
	}
	return hooks, notes, nil
}

// ensureOrgWebhook converges the organization's hook.
//
// The error is returned rather than folded into the state because the caller
// decides what an org-hook refusal MEANS — a hard failure under `true`, a
// fallback under `auto` — and it cannot decide that from a string.
func ensureOrgWebhook(ctx context.Context, opts Options, org, target, secret string) (HookState, error) {
	state := HookState{Target: Target{Org: org}}
	hooks, err := opts.Client.OrgWebhooks(ctx, org)
	if err != nil {
		return state, err
	}
	for _, hook := range hooks {
		// MATCHED ON THE DELIVERY URL, never on a name: GitHub gives a
		// webhook no name at all, and an organization carries hooks other
		// integrations registered. A run that converged the first one it
		// found would take down an unrelated integration.
		if hook.URL != target {
			continue
		}
		if opts.RecreateWebhooks {
			if err := opts.Client.DeleteOrgWebhook(ctx, org, hook.ID); err != nil {
				return state, err
			}
			break
		}
		if _, err := opts.Client.UpdateOrgWebhook(ctx, org, hook.ID, target, secret); err != nil {
			return state, err
		}
		state.URL = target
		return state, nil
	}
	if _, err := opts.Client.CreateOrgWebhook(ctx, org, target, secret); err != nil {
		return state, err
	}
	state.URL, state.Created = target, true
	return state, nil
}

// ensureRepoWebhook converges one repository's hook.
//
// A FAILURE HERE IS REPORTED, NOT RAISED. A company's repository list will
// contain one that was renamed, archived or made private to a team this
// credential is not in, and failing the whole run over it would leave every
// other repository unhooked to punish one typo.
func ensureRepoWebhook(ctx context.Context, opts Options, t Target, target, secret string) HookState {
	state := HookState{Target: t}
	repo, err := opts.Client.RepoOf(ctx, t.Owner, t.Repo)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			// GITHUB CONFLATES THESE TWO and says so nowhere: a private
			// repository answers 404 to a credential that cannot see
			// it, precisely so a probe cannot enumerate what exists. An
			// operator reading "not found" about a repository they are
			// looking at needs to be told the other half.
			state.Detail = "not found, or not visible to this credential — " +
				"GitHub answers 404 for both, so check the spelling and check " +
				"that this token's account has access"
			return state
		}
		state.Detail = err.Error()
		return state
	}
	if !repo.Permissions.Admin {
		// Checked BEFORE the write, because the failure is otherwise a
		// 404 on the hooks path — the same status as a missing
		// repository, on a repository this run just read successfully.
		state.Detail = "this credential has no admin access, and only an " +
			"admin may register a webhook"
		return state
	}
	if repo.Archived {
		// Not a failure: an archived repository emits no events, so a
		// hook on it would be correct and pointless. Saying so is what
		// stops an operator debugging a repository that is finished.
		state.Detail = "archived, so it emits no events — no hook registered"
		return state
	}

	hooks, err := opts.Client.RepoWebhooks(ctx, t.Owner, t.Repo)
	if err != nil {
		state.Detail = err.Error()
		return state
	}
	for _, hook := range hooks {
		if hook.URL != target {
			continue
		}
		if opts.RecreateWebhooks {
			if err := opts.Client.DeleteRepoWebhook(ctx, t.Owner, t.Repo, hook.ID); err != nil {
				state.Detail = err.Error()
				return state
			}
			break
		}
		if _, err := opts.Client.UpdateRepoWebhook(ctx, t.Owner, t.Repo, hook.ID, target, secret); err != nil {
			state.Detail = err.Error()
			return state
		}
		state.URL = target
		return state
	}
	if _, err := opts.Client.CreateRepoWebhook(ctx, t.Owner, t.Repo, target, secret); err != nil {
		state.Detail = err.Error()
		return state
	}
	state.URL, state.Created = target, true
	return state
}

// webhookSecret is the value every hook is registered with.
//
// # Minted only where there is nothing usable
//
// The tempting shape is to mint every run, and it is an outage: the engine
// is running with the OLD secret, and re-registering with a fresh one makes
// GitHub sign every delivery with a key the running engine does not hold —
// every webhook refused at the edge, from a command whose whole promise is
// that it is safe to re-run. So a secret that already resolves is used as it
// is, and minting happens when there is none, or when the operator asked to
// recreate the hooks having planned the restart.
//
// ONE SECRET FOR EVERY TARGET, because there is one route and one configured
// value behind it: `integrations.github.webhook_secret` is what the edge
// verifies against, so a per-repository secret would be a key the engine
// never checks.
func webhookSecret(ctx context.Context, opts Options, target string) (string, []string, error) {
	var resolved string
	if opts.Value != nil {
		resolved = strings.TrimSpace(opts.Value(opts.Config.WebhookSecret))
	}
	if resolved != "" && !opts.RecreateWebhooks {
		return resolved, nil, nil
	}
	secretVar, ok := provision.SoleVar(opts.Config.WebhookSecret)
	if !ok {
		return "", nil, fmt.Errorf(
			"github: integrations.github.webhook_secret is %q, which is neither "+
				"a value this run could resolve nor a whole ${VAR} reference to "+
				"mint one into — point it at a variable, set that variable, or "+
				"drop -public-url and register %s by hand",
			opts.Config.WebhookSecret, target)
	}
	if opts.Sink == nil {
		return "", nil, provision.ErrNoSink
	}
	// rand.Text is 26 base32 characters over a 128-bit draw. GitHub
	// accepts any string as a webhook secret and signs with it verbatim,
	// so the only property that matters is that it is unguessable — there
	// is no shape to satisfy, unlike the self-hosted host's whsec_ form.
	secret := rand.Text()
	if err := opts.Sink.Record(ctx, secretVar, secret); err != nil {
		return "", nil, fmt.Errorf("github: record %s: %w", secretVar, err)
	}
	note := fmt.Sprintf(
		"a fresh webhook secret was minted into %s — %s", secretVar,
		opts.Sink.NextStep())
	if opts.RecreateWebhooks {
		note += ". The previous secret is now invalid on every other " +
			"deployment of this company"
	}
	return secret, []string{note}, nil
}

// webhookTarget is the route a GitHub delivery arrives on.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + "/webhooks/github"
}
