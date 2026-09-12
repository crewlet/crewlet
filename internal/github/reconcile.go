package github

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"

	"github.com/crewlet/crewlet/internal/integration"
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

// IdentityOutcome is what one run learned about a seat's own code-host
// credential, and it has THREE values because two of them are an empty Login
// and they lead to opposite conclusions.
//
// A seat that NAMES a credential which does not authenticate has a fault
// somebody can fix: every call its tools make is refused, and an operator has
// a variable to set or a token to reissue. A seat that names NONE has not
// asked for one — identity on this host is each agent's OWN GitHub App, which
// [ReconcileSeatApps] is the authority on and which this run cannot see from
// the org chart it was handed.
//
// Reported as one state, they were both "Login is empty, Reason says why",
// and every agent seat on the current per-seat-app shape — where by design no
// `mcp_env.github` token exists anywhere — became one
// [integration.FindingIdentityFailed]. A healthy company sat in
// [integration.PhaseDegraded] for ever, retried on the admin backoff, over a
// credential the design had deliberately removed: the permanent note on a
// card with nothing wrong with it that [Result.Findings] refuses one
// paragraph up, arrived at from the other direction.
//
// A value rather than a convention on Reason, for the reason [HookOutcome]
// is one: an empty Login is what a caller sees, and no amount of prose in
// Reason changes what [Result.Findings] does with it.
type IdentityOutcome string

// The three outcomes.
const (
	// IdentityResolved is a seat whose credential named an account.
	IdentityResolved IdentityOutcome = "resolved"

	// IdentityRefused is a seat that names a credential this run could
	// not turn into an account. THE ONE THAT BECOMES A FINDING.
	IdentityRefused IdentityOutcome = "refused"

	// IdentityUnclaimed is a seat that names no code-host credential at
	// all. Not a fault, and not a finding: the reason travels in Reason
	// for a person reading the run.
	IdentityUnclaimed IdentityOutcome = "unclaimed"
)

// Valid reports an outcome this build knows, so one off the wire is a value
// rather than a panic.
func (o IdentityOutcome) Valid() bool {
	switch o {
	case IdentityResolved, IdentityRefused, IdentityUnclaimed:
		return true
	default:
		return false
	}
}

// SeatIdentity is one seat's code-host account, or why there is none.
type SeatIdentity struct {
	Handle string
	// Login is the account the seat's own credential authenticates as.
	// Empty means this run resolved none, which [SeatIdentity.Outcome]
	// says whether to act on.
	Login string

	// Outcome is what this run learned. THE ZERO VALUE READS AS REFUSED,
	// deliberately and by way of [SeatIdentity.Refused] rather than by
	// naming the empty string: every path that gives up on a seat sets
	// only Reason, so the honest default for "this walk returned without
	// saying otherwise" is the reporting one, and a new early return is
	// surfaced rather than silently swallowed. It is [HookState.Outcome]'s
	// rule, for [HookState.Outcome]'s reason.
	Outcome IdentityOutcome

	// Reason says why an empty Login is empty, in terms an operator can
	// act on.
	Reason string
}

// Routes reports a seat whose inbound events can reach it on a credential
// this run resolved.
func (s SeatIdentity) Routes() bool { return s.Login != "" }

// Refused reports a seat that named a credential this run could not turn
// into an account — the only case [Result.Findings] reports.
//
// Stated as what was NOT established rather than as an equality, so that a
// seat nothing positively concluded about is reported: silence is the answer
// a forgotten branch gives, and on a walk whose whole output is "which seats
// are broken" the forgiving reading of silence is the one that loses a
// finding.
func (s SeatIdentity) Refused() bool {
	return !s.Routes() && s.Outcome != IdentityUnclaimed
}

// HookOutcome is what happened at one webhook target, and it has THREE
// values because two of them were indistinguishable and led to opposite
// actions.
//
// A hook that was attempted and refused is an ingress block: events reach
// nobody and somebody has to widen a credential. A target with NOTHING TO
// HOOK is not — an archived repository emits no events, so a hook on it
// would be correct and pointless — and reporting it as a block put a company
// permanently in [integration.PhaseDegraded], retried on the admin backoff
// for ever, over a repository that is finished.
//
// Both used to be "URL is empty, Detail says why", which is why the
// distinction has to be a value rather than a convention: an empty URL is
// what a caller sees, and no amount of prose in Detail changes what
// [Result.Findings] does with it.
type HookOutcome string

// The three outcomes.
const (
	// HookRegistered is a target this run left with a working hook.
	HookRegistered HookOutcome = "registered"

	// HookSkipped is a target with nothing to hook. Not a fault, and not
	// a finding: the reason travels in Detail for a person reading the
	// run.
	HookSkipped HookOutcome = "skipped"

	// HookBlocked is a target this run tried and could not hook. This is
	// the one that becomes [integration.FindingIngressBlocked].
	HookBlocked HookOutcome = "blocked"
)

// Valid reports an outcome this build knows, so one off the wire is a value
// rather than a panic.
func (o HookOutcome) Valid() bool {
	switch o {
	case HookRegistered, HookSkipped, HookBlocked:
		return true
	default:
		return false
	}
}

// HookState is what one webhook target looks like after the run.
type HookState struct {
	Target Target

	// Outcome is what happened. The ZERO VALUE IS BLOCKED, deliberately:
	// every path that gives up on a target returns early having set only
	// Detail, so the honest default for "this function returned without
	// saying otherwise" is the refusing one. A new early return is
	// therefore reported rather than silently swallowed.
	Outcome HookOutcome

	// URL is the delivery address the hook now points at, empty for any
	// outcome but [HookRegistered].
	URL string

	// Created is true for a hook this run made, false for one it
	// converged.
	Created bool

	// Detail says why, for a target that was refused OR skipped, in terms
	// an operator can act on.
	Detail string
}

// Hooked reports a target this run left with a working hook.
func (h HookState) Hooked() bool { return h.Outcome == HookRegistered }

// Blocks reports a target whose events reach nobody AND that somebody can do
// something about — the only case [Result.Findings] reports.
func (h HookState) Blocks() bool { return h.Outcome == HookBlocked }

// Result is what one reconcile found and did.
type Result struct {
	// Login is who the org credential authenticates as, or empty when the
	// run had no org credential to probe.
	Login string
	Seats []SeatIdentity
	Hooks []HookState
	Notes []string

	// NoIngress says why this run registered no delivery path AT ALL, and
	// is empty when it had an address to register one against.
	//
	// A SEPARATE FIELD because an empty Hooks list means two opposite
	// things. A pass given no public base registers nothing by
	// construction — every hook state it would have produced is simply
	// absent — and Classify over no findings is Ready. So a company whose
	// public_base_url was never set reported GitHub as working while
	// nothing at GitHub pointed at it, which is the same not-there
	// coverage every other rule here exists to refuse.
	//
	// The zero value is "nothing to report", deliberately: a Result built
	// anywhere but Reconcile must not invent an ingress problem.
	NoIngress string

	// NoRegistrar says why this run could register no webhook AT ALL
	// although the company asked for one, and is empty when it could.
	//
	// SEPARATE FROM NoIngress, whose subject is the ADDRESS. This one's is
	// the credential: `provisioning` names an organization or a list of
	// repositories, which is a company asking for hooks on them, and
	// `integrations.github.token` is what registering one takes. With no
	// token the pass reads nothing and writes nothing — and said so only in
	// Notes, which are not findings, so a surface that had authenticated
	// with nobody reported READY. Measured on a live connect: `phase:
	// ready`, `findings: []`, `routes: true`, and exactly one webhook on
	// the organization, belonging to somebody else's deployment.
	//
	// The form asks for no token, deliberately (see [Requirements]), so
	// this finding has no field to offer and its detail names the variable
	// and the way out instead.
	NoRegistrar string

	// NoKeyring is a run that had to mint the webhook signing secret and
	// had nowhere to seal one.
	//
	// SEPARATE FROM NoIngress although both end as one ingress finding,
	// because they name DIFFERENT FIELDS to change and the subject is the
	// whole value of the report: NoIngress is answered by setting
	// integrations.public_base_url, and this is answered by setting
	// secrets.keys or by supplying the secret yourself.
	//
	// REPORTED, NOT RAISED. Record answers [provision.ErrNoSink] on a sink
	// that cannot seal, so this pass faulted on every tick for ever over a
	// deployment that had simply not set secrets.keys — the exact
	// permanent-fault posture [provision.ReadOnly] exists to remove.
	NoKeyring bool
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
	if opts.Config == nil {
		return nil, errors.New("github: no github config")
	}
	// A PASS THAT WAS NEVER RUN REPORTS NOTHING.
	//
	// The arm below is the only way out of this function that touches no
	// network, so it is the only one a dead context cannot fail on its
	// own — and it is a supported configuration rather than an edge, since
	// `integrations.github.token` is optional and the engine hands a nil
	// client for an empty one. A node draining therefore answered "no org
	// credential resolved" with no findings and no error, which the loop
	// reads as a converged integration and trusts for a full settled
	// interval.
	if err := interrupted(ctx); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		// NO ORG CREDENTIAL IS A FINDING, NOT A FAULT. The token is
		// optional on this host and its absence is a documented
		// degradation: participant fan-out is off and a thread's
		// watchers hear nothing, which is exactly what an empty Login
		// makes Findings report. Refusing here instead turned that into
		// "the last pass could not read this integration", which sends
		// an operator looking for an outage rather than for a variable.
		//
		// Nothing else can be read without one. Seat identities are API
		// lookups and a hook is an API write, so the honest result is a
		// run that says only what it knows.
		//
		// THE ADDRESS IS STILL REPORTED, because it is a fact about the
		// company document rather than about GitHub and needs no
		// credential to establish. Without it nothing at GitHub — not an
		// org hook, not a repository hook, not the webhook in each
		// agent's own app manifest — has anywhere to deliver to, and
		// leaving [Result.NoIngress] empty here reported a company that
		// receives nothing as Ready for exactly the companies most likely
		// to have no org token.
		return &Result{
			Notes: []string{
				"no org credential resolved, so this run read nothing at GitHub",
			},
			NoIngress:   noIngressReason(opts),
			NoRegistrar: noRegistrarReason(opts),
		}, nil
	}
	login, err := opts.Client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"github: the credential this run authenticates with was refused, "+
				"so nothing else it reported would be trustworthy: %w",
			integration.Reject(err, Status(err)))
	}

	res := &Result{Login: login}
	res.Seats = resolveSeats(ctx, opts)

	in, err := ensureWebhooks(ctx, opts)
	res.Hooks = in.Hooks
	res.Notes = append(res.Notes, in.Notes...)
	res.NoKeyring = in.NoKeyring
	res.NoIngress = noIngressReason(opts)
	if err == nil {
		// AND AGAIN AT THE END, because a pass cancelled halfway does not
		// stop halfway: every read between here and the probe reports its
		// own failure as a FINDING rather than raising — a seat whose
		// lookup failed becomes identity_failed, a repository whose hook
		// listing failed becomes ingress_blocked — so a node draining
		// mid-pass would record a page of sentences about the operator's
		// credentials, every one of them actually about this engine
		// shutting down.
		err = interrupted(ctx)
	}
	if err != nil {
		// THE SINK IS FLUSHED ON THE WAY OUT, WHICHEVER WAY THAT IS.
		//
		// [webhookSecret] seals a fresh secret BEFORE the hooks that have
		// to carry it are registered, and the sink the loop hands in makes
		// what it recorded visible to a running engine only inside Flush.
		// Returning from a failure below that point left the minted secret
		// sealed and INVISIBLE: the next pass resolved nothing, minted a
		// second secret, failed at the same place, and went on doing that
		// for as long as the failure lasted — a key rotated every few
		// minutes by the loop whose whole promise is that it is safe to
		// leave switched on, which is the exact runaway the "mint only
		// where there is nothing usable" rule exists to prevent.
		//
		// ON AN UNCANCELLABLE COPY, the rule every teardown in this tree
		// follows: the failure being cleaned up after is frequently the
		// cancellation itself, and a flush that inherits a dead context
		// does nothing at all — which is the bug again, reached through
		// the drain instead of through a refused hook.
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

// flushSink completes this run's sink, where there is one.
//
// The context is the CALLER'S on the success path and an uncancellable copy
// of it on the failure path — see the call site for why. The success path
// keeps the deadline, because there a flush that hangs is a pass that never
// returns.
func flushSink(ctx context.Context, opts Options) error {
	if opts.Sink == nil {
		return nil
	}
	if err := opts.Sink.Flush(ctx); err != nil {
		return fmt.Errorf("github: %w", err)
	}
	return nil
}

// interrupted reports a pass whose context is done, as the error the caller
// must raise instead of answering.
//
// THE CONTEXT, NEVER THE ERROR A CALL CAME BACK WITH. `errors.Is(err,
// context.DeadlineExceeded)` looks like the same question and is not: net/http
// gives a Client.Timeout that very sentinel — measured, not assumed: a request
// that outruns [ClientTimeout] under a perfectly live parent context satisfies
// it — so testing the error would read a slow GitHub as a torn-down pass and
// raise on exactly the case this package deliberately leaves the world alone
// for. What is being asked is whether THIS PASS is still running, and only
// ctx.Err answers that.
//
// Wrapped with %w, so a caller can still tell a cancellation from a deadline
// and the loop's own backoff sees the sentinel it expects.
func interrupted(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	return fmt.Errorf(
		"github: this pass was interrupted before it finished reading the "+
			"deployment, so it has nothing to report about it — an engine "+
			"shutting down must not leave GitHub recorded as ready: %w", err)
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
		out[i] = SeatIdentity{Handle: seat.Handle(), Outcome: IdentityRefused}
		tokens[i] = CredentialOf(seat, opts.Value)
		if tokens[i] != "" {
			lookups = append(lookups, i)
			continue
		}
		if key, declared := credentialSlot(seat); declared {
			// NAMED AND EMPTY IS A FAULT. The operator wrote this seat a
			// credential slot and nothing came out of it, so its tools
			// authenticate as nobody — and the field to edit is the one
			// thing they need told.
			//
			// THE KEY, NEVER THE VALUE, for the reason [webhookSecret]
			// does not quote its own: the slot normally holds a `${VAR}`
			// and printing it would be helpful, but one way to reach
			// this line is a LITERAL somebody pasted, and then the thing
			// it would print is the credential.
			out[i].Reason = "mcp_env." + SeatEnv + "." + key + " resolved to " +
				"nothing — set the variable it names, or drop the entry if this " +
				"seat acts through its own GitHub App instead"
			continue
		}
		// NAMED NOTHING IS NOT A FAULT — see [IdentityOutcome]. This
		// seat's identity is its own GitHub App, which this run cannot
		// see and [ReconcileSeatApps] reports on.
		out[i].Outcome = IdentityUnclaimed
		out[i].Reason = "no credential under mcp_env." + SeatEnv +
			", so this seat acts through its own GitHub App rather than a " +
			"personal access token"
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
		out[i].Login, out[i].Outcome = login, IdentityResolved
	})
	return out
}

// noIngressReason says why this run could register no delivery path, or "".
//
// ONLY THE ADDRESS. A company with no `provisioning` block has no
// organization and no repositories for an org- or repo-level hook, and that
// is a working configuration rather than a gap: each agent's own GitHub App
// carries its own webhook, registered in the app's manifest. The public base
// is different — with none, nothing this engine runs has an address for
// GitHub to deliver to at all.
func noIngressReason(opts Options) string {
	if webhookTarget(opts.WebhookBase) != "" {
		return ""
	}
	return "integrations.public_base_url is unset, so nothing at GitHub has " +
		"an address to deliver to and no event reaches this deployment"
}

// noRegistrarReason says why a run with no organization credential could
// register nothing although the company asked for hooks, or "" when it asked
// for none.
//
// ASKED FOR is the whole of the test, and it is the `provisioning` block: an
// organization or a list of repositories is a company saying it wants hooks
// on them. A company with no block wants none — each agent's own app carries
// its own webhook in its own manifest — so silence there is correct rather
// than a gap, and reporting it would put a permanent finding on every company
// running GitHub the way the form sets it up.
func noRegistrarReason(opts Options) string {
	pv := opts.Config.Provisioning
	if pv == nil {
		return ""
	}
	where := hookTargetNames(pv)
	if len(where) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"integrations.github.provisioning names %s, so this company asked for "+
			"a webhook there, and no credential resolved to register one with: "+
			"nothing at GitHub delivers to this deployment from %s. Set "+
			"integrations.github.token to a credential with webhook access, or "+
			"clear the provisioning block and let each agent's own app carry "+
			"its own webhook",
		strings.Join(where, ", "), strings.Join(where, ", "))
}

// ingress is what one webhook pass concluded. A struct rather than three
// returns because the third — a node that could not seal a signing secret —
// is a POSTURE the caller reports rather than an error, and a bare bool
// beside a slice and an error is the shape nobody reads.
type ingress struct {
	Hooks     []HookState
	Notes     []string
	NoKeyring bool
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
func ensureWebhooks(ctx context.Context, opts Options) (ingress, error) {
	target := webhookTarget(opts.WebhookBase)
	if target == "" {
		return ingress{Notes: []string{
			"no webhook was registered: pass the deployment's public base URL " +
				"to register one, or add it by hand — without it GitHub " +
				"delivers nothing and the integration looks idle rather than " +
				"unconfigured"}}, nil
	}
	pv := opts.Config.Provisioning
	if pv == nil {
		return ingress{Notes: []string{
			"no webhook was registered: integrations.github.provisioning is " +
				"unset, so this run has no organization and no repositories to " +
				"register one on"}}, nil
	}

	key, err := webhookSecret(ctx, opts, target)
	if err != nil {
		return ingress{Notes: key.Notes}, err
	}
	if key.NoKeyring {
		return ingress{Notes: key.Notes, NoKeyring: true}, nil
	}
	secret, minted, notes := key.Secret, key.Minted, key.Notes

	mode := config.ContainerWebhookAuto
	if pv.OrgWebhook != "" {
		mode = pv.OrgWebhook
	}
	var hooks []HookState

	if org := strings.TrimSpace(pv.Org); org != "" && mode != config.ContainerWebhookNever {
		state, err := ensureOrgWebhook(ctx, opts, org, target, secret, minted)
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
				return ingress{Hooks: hooks, Notes: notes}, nil
			}
		case mode == config.ContainerWebhookRequire:
			return ingress{Hooks: hooks, Notes: notes}, fmt.Errorf(
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
		return ingress{Hooks: hooks, Notes: notes}, nil
	}
	for _, t := range targets {
		hooks = append(hooks, ensureRepoWebhook(ctx, opts, t, target, secret, minted))
	}
	return ingress{Hooks: hooks, Notes: notes}, nil
}

// ensureOrgWebhook converges the organization's hook.
//
// The error is returned rather than folded into the state because the caller
// decides what an org-hook refusal MEANS — a hard failure under `true`, a
// fallback under `auto` — and it cannot decide that from a string.
func ensureOrgWebhook(
	ctx context.Context, opts Options, org, target, secret string, minted bool,
) (HookState, error) {
	state := HookState{Target: Target{Org: org}, Outcome: HookBlocked}
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
		if converged(hook, minted) {
			// ALREADY CORRECT, and left exactly as it is.
			//
			// THE STEADY STATE IS WHERE THIS LOOP LIVES: the reconcile
			// runs every few minutes for the life of the deployment, and
			// [integration.DefaultSchedule] is anchored on each pass
			// issuing no writes when nothing has changed. An
			// unconditional update was one write per target per pass, for
			// ever, on a hook that needed nothing.
			//
			// `minted` is the half a URL comparison cannot see: GitHub
			// never gives a secret back, so a hook pointing at the right
			// address is still signed with the old key when this run made
			// a new one.
			state.Outcome, state.URL = HookRegistered, target
			return state, nil
		}
		if _, err := opts.Client.UpdateOrgWebhook(ctx, org, hook.ID, target, secret); err != nil {
			return state, err
		}
		state.Outcome, state.URL = HookRegistered, target
		return state, nil
	}
	if _, err := opts.Client.CreateOrgWebhook(ctx, org, target, secret); err != nil {
		return state, err
	}
	state.Outcome, state.URL, state.Created = HookRegistered, target, true
	return state, nil
}

// ensureRepoWebhook converges one repository's hook.
//
// A FAILURE HERE IS REPORTED, NOT RAISED. A company's repository list will
// contain one that was renamed, archived or made private to a team this
// credential is not in, and failing the whole run over it would leave every
// other repository unhooked to punish one typo.
func ensureRepoWebhook(
	ctx context.Context, opts Options, t Target, target, secret string, minted bool,
) HookState {
	state := HookState{Target: t, Outcome: HookBlocked}
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
		// NOTHING TO HOOK, which is not the same as failing to hook: an
		// archived repository emits no events, so a hook on it would be
		// correct and pointless. Saying so is what stops an operator
		// debugging a repository that is finished — and the outcome is
		// what stops the loop reporting the company degraded over it,
		// for ever, on the admin backoff.
		state.Outcome = HookSkipped
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
		if converged(hook, minted) {
			// ALREADY CORRECT — see [ensureOrgWebhook] for why this
			// branch exists and what `minted` covers that a URL
			// comparison cannot.
			state.Outcome, state.URL = HookRegistered, target
			return state
		}
		if _, err := opts.Client.UpdateRepoWebhook(ctx, t.Owner, t.Repo, hook.ID, target, secret); err != nil {
			state.Detail = err.Error()
			return state
		}
		state.Outcome, state.URL = HookRegistered, target
		return state
	}
	if _, err := opts.Client.CreateRepoWebhook(ctx, t.Owner, t.Repo, target, secret); err != nil {
		state.Detail = err.Error()
		return state
	}
	state.Outcome, state.URL, state.Created = HookRegistered, target, true
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
//
// # And "nothing usable" includes what this deployment already sealed
//
// Which is [provision.MintSecret]'s whole subject, and the reason it is not
// written out here: the sink is asked before anything is minted, the read is
// three-valued, and a node with no keyring reports rather than faults. This
// pass had none of that — it minted whenever the RESOLVER answered empty, and
// the resolver answers from a snapshot taken at apply time, so every pass in
// the window after a mint sealed a second secret over the first and
// re-registered every hook with it.
type webhookKey struct {
	// Secret is the value every hook is registered with. Empty only where
	// NoKeyring is set or an error was returned: a hook must never be
	// registered unsigned.
	Secret string

	// Minted is whether a FRESH secret was minted on this run, and it is
	// what lets a converged pass leave a working hook alone: GitHub never
	// gives a secret back, so "the hook already points at the right
	// address" is only enough when this run did not change the key it must
	// be signed with.
	Minted bool

	// NoKeyring is a run that had to mint and had nowhere to seal it. See
	// [Result.NoKeyring].
	NoKeyring bool

	// Notes is what to tell the operator about a value this run created.
	Notes []string
}

func webhookSecret(
	ctx context.Context, opts Options, target string,
) (webhookKey, error) {
	var resolved string
	if opts.Value != nil {
		resolved = strings.TrimSpace(opts.Value(opts.Config.WebhookSecret))
	}
	if resolved != "" && !opts.RecreateWebhooks {
		return webhookKey{Secret: resolved}, nil
	}
	secretVar, ok := provision.SoleVar(opts.Config.WebhookSecret)
	if !ok {
		// THE VALUE IS NOT QUOTED. It is normally a ${VAR} and quoting it
		// would be helpful, but the case this refusal exists for is a
		// slot holding a LITERAL — so the one time the message is
		// reached, the thing it would print is the credential. The path
		// is what an operator needs, and the path is what it says.
		return webhookKey{}, fmt.Errorf(
			"github: integrations.github.webhook_secret holds neither a value "+
				"this run could resolve nor a whole ${VAR} reference to mint "+
				"one into — point it at a variable, set that variable, or "+
				"clear both -public-url and integrations.public_base_url and "+
				"register %s by hand", target)
	}
	// rand.Text is 26 base32 characters over a 128-bit draw. GitHub
	// accepts any string as a webhook secret and signs with it verbatim,
	// so the only property that matters is that it is unguessable — there
	// is no shape to satisfy, unlike the self-hosted host's whsec_ form.
	secret, err := provision.MintSecret(ctx, opts.Sink, secretVar,
		opts.RecreateWebhooks, func() (string, error) { return rand.Text(), nil })
	if err != nil {
		return webhookKey{}, fmt.Errorf("github: %w", err)
	}
	key := webhookKey{
		Secret:    secret.Value,
		Minted:    secret.Minted,
		NoKeyring: secret.NoKeyring,
	}
	if secret.Minted {
		note := fmt.Sprintf(
			"a fresh webhook secret was minted into %s — %s", secretVar,
			opts.Sink.NextStep())
		if opts.RecreateWebhooks {
			note += ". The previous secret is now invalid on every other " +
				"deployment of this company"
		}
		key.Notes = []string{note}
	}
	return key, nil
}

// converged reports a hook that already carries everything this run would
// write, so writing it again is a request spent to change nothing.
//
// The URL is the caller's business — it is what selected this hook — so what
// is left is the two fields a delivery depends on and the one thing that
// cannot be read back. A disabled hook delivers nothing; a hook missing an
// event type delivers everything but that one, which is a silence nobody
// notices; and a secret this run just replaced has to be sent, because GitHub
// answers with no secret at all and there is nothing to compare.
//
// EXTRA events are not a reason to write. An operator who added one to
// Crewlet's hook wanted it, and rewriting the list every pass to take it away
// again is the same argument as [integration.Reconciler]'s: converge what
// this engine needs, and do not undo what it does not.
func converged(hook Webhook, minted bool) bool {
	if minted || !hook.Active {
		return false
	}
	for _, want := range WebhookEvents {
		if !slices.Contains(hook.Events, want) {
			return false
		}
	}
	return true
}

// webhookTarget is the route a GitHub delivery arrives on.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + "/webhooks/github"
}
