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
	"github.com/crewlet/crewlet/internal/setup"

	"github.com/crewlet/crewlet/internal/integration"
)

// Reconcile brings a GitHub deployment in line with the company config.
//
// GitHub has no API that creates a user or mints its token, so this cannot
// converge accounts. It does the two things GitHub allows:
//
//   - WHICH ACCOUNT each seat's credential authenticates as: the whole of a
//     seat's inbound routing, declared nowhere in the org model.
//   - THE INBOUND WEBHOOKS, registered with a secret the engine holds: one
//     on the organization where the credential may, else one per repository.
//     Without them the integration looks idle rather than unconfigured.

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

	// SeatApps is how many of this company's agents carry their OWN GitHub
	// App — each one with its own webhook baked into its own manifest.
	//
	// IT IS WHAT MAKES "NOTHING REGISTERED ORGANIZATION-WIDE" AN ANSWER
	// RATHER THAN A GAP. This pass cannot see an agent's app, which is
	// private to the seat, so no org hook and no repository hooks is the
	// recommended arrangement when those apps exist and a company receiving
	// nothing when they do not; only this count tells the two apart.
	// Counted, not named: the question is whether coverage comes from apps
	// at all, not which seats.
	SeatApps int

	// WebhookBase is where GitHub reaches this deployment
	// ([config.API.WebhookBase]: the public listener's address where
	// `api.public` is set), or empty to skip webhook registration.
	//
	// SKIPPED RATHER THAN GUESSED: a hook pointing at the wrong host is
	// worse than no hook, because GitHub then reports a healthy
	// integration that delivers into the void.
	WebhookBase string

	// WebhookBaseKey is the Tier A key WebhookBase is read from
	// ([config.API.WebhookBaseKey]), which a run with no address names as
	// the setting to fill in. Empty is [setup.BaseKey]'s.
	WebhookBaseKey string

	// RecreateWebhooks deletes and remakes every hook to mint a fresh
	// secret, for the case where the existing one's secret was lost.
	// Destructive: it invalidates the secret every other deployment of
	// this company holds.
	//
	// Spelled as the caller spells its switch ([provision.FreshSwitch]):
	// `-recreate-webhooks` from `crewlet github provision`,
	// `recreate_webhooks` from the setup route. A refusal of it names
	// that spelling, never this package's guess at one.
	RecreateWebhooks provision.FreshSwitch
}

// IdentityOutcome is what one run learned about a seat's own code-host
// credential, with THREE values because two of them are an empty Login and
// lead to opposite conclusions.
//
// A seat that NAMES a credential which does not authenticate has a fault
// somebody can fix. A seat that names NONE has not asked for one: its
// identity is its OWN GitHub App, which the access sync owns and this run
// cannot see. Folded together, every per-seat-app seat became an
// [integration.FindingIdentityFailed], [integration.PhaseDegraded] for ever.
// A value, not a convention on Reason, as [HookOutcome] is.
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

// HookOutcome is what happened at one webhook target, with THREE values
// because two of them look alike and lead to opposite actions.
//
// A hook attempted and refused is an ingress block: events reach nobody and
// somebody has to widen a credential. A target with NOTHING TO HOOK is not
// (an archived repository emits no events); reporting it as a block would
// hold the company in [integration.PhaseDegraded] over a finished repository.
//
// A value, not a convention on Detail: a caller sees an empty URL, and prose
// in Detail changes nothing [Result.Findings] does with it.
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
	// things. A pass given no public base registers nothing by construction,
	// and Classify over no findings is Ready, so it would report GitHub as
	// working while nothing at GitHub pointed at it.
	//
	// The zero value is "nothing to report": a Result built anywhere but
	// Reconcile must not invent an ingress problem.
	NoIngress string

	// NoIngressKey is the Tier A key this deployment's webhook address is
	// read from, which NoIngress names as the setting an operator fills in
	// to give GitHub an address — as [Options] handed it over. Empty is
	// [setup.BaseKey]'s.
	NoIngressKey string

	// NoRegistrar says why this run could register no webhook AT ALL
	// although the company asked for one, and is empty when it could.
	//
	// SEPARATE FROM NoIngress, whose subject is the ADDRESS; this one's is
	// the credential: `provisioning` names hooks the company asked for, and
	// `integrations.github.token` is what registering one takes. With no
	// token the pass reads and writes nothing, and said so only in Notes,
	// which are not findings, so a surface that authenticated as nobody
	// reported READY.
	//
	// The form asks for no token (see [Requirements]), so this finding has no
	// field to offer and its detail names the variable instead.
	NoRegistrar string

	// Coverage is what this company's agents hear about, when that is
	// narrower than the organization and is the arrangement rather than a
	// fault. The zero finding when nothing needs saying.
	//
	// A NOTE ON A WORKING INTEGRATION. It is reported as
	// [integration.FindingCoveragePartial], whose verdict is ready, and the
	// alternatives were both wrong: an ingress_blocked finding reads as
	// Action required over agents receiving events perfectly well, and a
	// note is dropped before anything renders it, so a person learns what
	// their integration does not see only by noticing its absence.
	Coverage integration.Finding

	// Undeliverable says why GitHub can deliver nothing to this deployment
	// although it HAS an address: the address is one GitHub cannot reach
	// ([Deliverable]), which is what a local engine's is. The zero finding
	// otherwise.
	//
	// AN ADVISORY, NOT A BLOCK. Such a deployment registers no hook, and its
	// agents' apps are created with their webhooks off ([BuildManifest]),
	// but the agents act on GitHub perfectly well through those apps:
	// pushing, opening pull requests and reviewing are all outbound. So it
	// is reported as [integration.FindingCoveragePartial] on a ready card,
	// saying what does not arrive and what makes it arrive, rather than as
	// Action required over agents that are working.
	Undeliverable integration.Finding
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
		// NO ORG CREDENTIAL IS A FINDING, NOT A FAULT. The token is optional
		// and its absence a documented degradation (participant fan-out
		// off), which an empty Login makes Findings report; refusing would
		// send an operator looking for an outage rather than a variable.
		// Seat lookups and hooks need the credential, so the run says only
		// what it knows.
		//
		// THE ADDRESS IS STILL REPORTED: it is a fact about the company
		// document and needs no credential. Without it nothing at GitHub,
		// the agents' own app webhooks included, has anywhere to deliver,
		// and an empty [Result.NoIngress] here read as Ready.
		return &Result{
			Notes: []string{
				"no org credential resolved, so this run read nothing at GitHub",
			},
			NoIngress:   noIngressReason(opts),
			NoRegistrar: noRegistrarReason(opts),
			// AND THIS IS THE ARM THE RECOMMENDED ARRANGEMENT TAKES, which
			// is why the coverage sentence has to be reachable from here
			// and not only from [ensureWebhooks]: a company hooking each
			// agent's own app has no organization credential by design, so
			// the pass returns above ever registering anything.
			Coverage:      coverageFor(opts, nil),
			Undeliverable: undeliverable(opts),
			NoIngressKey:  opts.WebhookBaseKey,
		}, nil
	}
	login, err := opts.Client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"github: the credential this run authenticates with was refused, "+
				"so nothing else it reported would be trustworthy: %w",
			integration.Reject(err, Status(err)))
	}

	res := &Result{Login: login, NoIngressKey: opts.WebhookBaseKey}
	res.Seats = resolveSeats(ctx, opts)

	in, err := ensureWebhooks(ctx, opts)
	res.Hooks = in.Hooks
	res.Notes = append(res.Notes, in.Notes...)
	// SAID ONCE, FROM WHAT IS ACTUALLY DELIVERING. Deciding it inside
	// [ensureWebhooks] put it in one of the two arms that reach this state
	// and missed the other — see the nil-client return above.
	res.Coverage = coverageFor(opts, res.Hooks)
	res.NoIngress = noIngressReason(opts)
	res.Undeliverable = undeliverable(opts)
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
		// [webhookSecret] seals a fresh secret BEFORE the hooks that carry it
		// are registered, and the sink shows it to a running engine only
		// inside Flush. Returning without one left the secret sealed and
		// INVISIBLE, so every pass minted another: the runaway the "mint only
		// where there is nothing usable" rule exists to prevent.
		//
		// ON AN UNCANCELLABLE COPY: the failure being cleaned up after is
		// often the cancellation itself, and a flush inheriting a dead
		// context does nothing.
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
// THE CONTEXT, NEVER THE ERROR A CALL CAME BACK WITH. net/http gives a
// Client.Timeout the context.DeadlineExceeded sentinel (a request outrunning
// [ClientTimeout] under a live parent satisfies it), so testing the error
// would read a slow GitHub as a torn-down pass. Only ctx.Err says whether
// THIS PASS is still running.
//
// Wrapped with %w, so the loop's backoff still sees the sentinel it expects.
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
	seats := agentSeats(opts.Org)
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
		// see and the access sync reports on.
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
	return setup.BaseKey(opts.WebhookBaseKey) + " is unset, so nothing at " +
		"GitHub has an address to deliver to and no event reaches this deployment"
}

// undeliverable is the advisory for an address GitHub cannot deliver to: what
// does not arrive, what still works, and what makes it arrive. The zero
// finding when the deployment has no address ([noIngressReason]'s case) or
// one GitHub reaches.
//
// THE KEY AND THE ADDRESS IN BACKTICKS, so a reader sets them as the literals
// they are: the address is a URL nobody should follow, and a renderer that
// links bare URLs would make it one.
func undeliverable(opts Options) integration.Finding {
	base := strings.TrimRight(strings.TrimSpace(opts.WebhookBase), "/")
	if base == "" || Deliverable(base, enterprise(opts)) {
		return integration.Finding{}
	}
	key := setup.BaseKey(opts.WebhookBaseKey)
	return integration.Finding{
		Kind:    integration.FindingCoveragePartial,
		Subject: key,
		Title:   "GitHub can't reach this engine",
		Detail: "`" + key + "` is `" + base + "`, so no GitHub events arrive. Agents " +
			"still push, open pull requests and review through their own apps",
		Remedy: "Set `" + key + "` to a public address to receive events",
	}
}

// enterprise reports a company on Enterprise Server rather than github.com,
// which decides what [Deliverable] counts as reachable. The address is read
// through the run's resolver, because it is a `${VAR}` as often as a literal
// and one that resolves to nothing is github.com.
func enterprise(opts Options) bool {
	if opts.Config == nil {
		return false
	}
	url := opts.Config.URL
	if opts.Value != nil {
		url = opts.Value(url)
	}
	return strings.TrimSpace(url) != ""
}

// noRegistrarReason says why a run with no organization credential could
// register nothing the company ASKED FOR, or "" when it asked for nothing.
//
// The org is named for the agents' own apps (the form requires it, with no
// token), not for a hook, so the name alone is no wish for an org webhook.
// A company asks for a hook this credential must register with:
//
//   - `org_webhook: true`, which demands one and has no fallback;
//   - a non-empty `repos` list, which no agent's own app covers.
//
// `auto` with no repositories (the form's default) takes an org hook if one
// can be had and per-agent app webhooks otherwise: the design, not a gap.
func noRegistrarReason(opts Options) string {
	pv := opts.Config.Provisioning
	if pv == nil {
		return ""
	}
	// AND AN AGENT'S OWN APP IS A REGISTRAR. This asked whether the
	// company wanted something an organization credential must register,
	// and stopped there — so a company whose agents each carry their own
	// app, receiving events through them, was told it had no webhook and
	// pointed at a token. Worse, the card that said so spent its time
	// asking for the apps to be installed, and installing one can never
	// produce `admin:org_hook`: an operator who did exactly as they were
	// told watched nothing change. See [Options.SeatApps].
	if opts.SeatApps > 0 {
		return ""
	}
	var asked []string
	if org := strings.TrimSpace(pv.Org); org != "" &&
		pv.OrgWebhook == config.ContainerWebhookRequire {
		asked = append(asked, org)
	}
	for _, target := range TargetsOf(pv) {
		asked = append(asked, target.String())
	}
	if len(asked) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"no webhook on %s: registering one needs integrations.github.token, "+
			"and none resolved. Set it, or let each agent's own app carry its "+
			"own webhook instead",
		strings.Join(asked, ", "))
}

// ingress is what one webhook pass concluded. A struct rather than three
// returns because the third — a node that could not seal a signing secret —
// is a POSTURE the caller reports rather than an error, and a bare bool
// beside a slice and an error is the shape nobody reads.
type ingress struct {
	Hooks []HookState
	Notes []string
}

// coverageFor is the note for a company whose events arrive through its
// agents' OWN apps, or the zero finding when there is nothing to say.
//
// THE TEST IS WHAT IS DELIVERING, not what the config asked for: no org or
// repository hook while each agent's app carries events is one arrangement
// however the mode field got there.
//
// Both halves of the `if` matter: with no apps the roster's "no app of its
// own" finding speaks; with a hook working, nothing narrower is left.
//
// AND ONLY AT AN ADDRESS GITHUB DELIVERS TO: with none, no app's events
// arrive either, which [noIngressReason] or [undeliverable] already says.
func coverageFor(opts Options, hooks []HookState) integration.Finding {
	if opts.SeatApps == 0 || noIngressReason(opts) != "" || undeliverable(opts).Kind != "" {
		return integration.Finding{}
	}
	for _, h := range hooks {
		if h.Hooked() {
			return integration.Finding{}
		}
	}
	var org string
	if pv := opts.Config.Provisioning; pv != nil {
		org = strings.TrimSpace(pv.Org)
	}
	return coverageNote(org)
}

// coverageNote is the note a company reads when its agents' own apps are
// what carry its events.
//
// IT NAMES BOTH HALVES. Saying only what is covered reads as a complete
// answer and leaves somebody surprised by the first repository nobody hears
// about; saying only what is not reads as a fault.
//
// THE REMEDY IS WORDED AS AN OPTION, because nothing is broken: a company
// that chose this arrangement must not read a decision it already took as
// work outstanding. And it is the token, never an install, since no app
// carries `admin:org_hook` at any permission.
func coverageNote(org string) integration.Finding {
	where := "the rest of the organization"
	if org != "" {
		where = "the rest of " + org
	}
	return integration.Finding{
		Kind:    integration.FindingCoveragePartial,
		Subject: "github coverage",
		Title:   "Events from your agents' repositories only",
		Detail: "GitHub sends events for the repositories your agents' apps are " +
			"installed on, and none for " + where,
		Remedy: "To cover every repository, set `integrations.github.token` and " +
			"Crewlet adds one organization-wide hook",
	}
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
	if !Deliverable(opts.WebhookBase, enterprise(opts)) {
		// NO HOOK AT AN ADDRESS GITHUB CANNOT REACH: it would fail every
		// delivery while reading as registered. [undeliverableReason] is
		// the finding that says so.
		return ingress{Notes: []string{
			"no webhook was registered: " + target + " is not an address " +
				"GitHub can deliver to"}}, nil
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
		case mode == config.ContainerWebhookRequire && opts.SeatApps > 0:
			// ASKED FOR MORE THAN IT GOT, over agents that are hearing
			// about their own repositories perfectly well. That is not a
			// failure of the pass and it is not Action required: the
			// company has coverage, narrower than it chose, and what
			// closes the gap is a value in its own configuration.
			//
			// It used to be the error below, which stopped the pass and
			// put the surface at degraded — over an integration
			// delivering events the whole time.
			return ingress{Hooks: hooks, Notes: notes}, nil
		case mode == config.ContainerWebhookRequire:
			return ingress{Hooks: hooks, Notes: notes}, fmt.Errorf(
				"github: org_webhook: true demands one hook on %s and this "+
					"credential cannot register it (%w) — a classic token needs "+
					"the admin:org_hook scope, which a fine-grained token cannot "+
					"carry at all, and no app can carry it however it is "+
					"installed. Supply integrations.github.token, or choose the "+
					"coverage each agent's own app already gives", org, err)
		default:
			notes = append(notes, fmt.Sprintf(
				"no organization hook on %s (%s) — falling back to one hook per "+
					"repository, which does not cover repositories created later",
				org, err.Error()))
		}
	}

	targets := TargetsOf(pv)
	if len(targets) == 0 {
		// NOTHING LEFT TO HOOK IS TWO DIFFERENT WORLDS, and this said the
		// same thing about both. A company whose agents each carry their
		// own app has exactly this shape by design — no org hook, no
		// repository list — and telling it to "name the repositories whose
		// events should reach the engine" describes a gap that is not
		// there, over events already arriving.
		//
		// So the arrangement is reported as one, and the sentence about
		// naming repositories is kept for the company it is true of: no
		// app anywhere, and nothing registered.
		if opts.SeatApps > 0 {
			// THE ARRANGEMENT, not a gap — said by [coverageFor], which
			// this only has to stop contradicting.
			return ingress{Hooks: hooks, Notes: notes}, nil
		}
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
		if opts.RecreateWebhooks.Asked() {
			if err := opts.Client.DeleteOrgWebhook(ctx, org, hook.ID); err != nil {
				return state, err
			}
			break
		}
		if converged(hook, minted) {
			// ALREADY CORRECT, and left alone: the steady state is where this
			// loop lives, and [integration.DefaultSchedule] is anchored on a
			// pass issuing no writes when nothing changed.
			//
			// `minted` is the half a URL comparison cannot see: GitHub never
			// gives a secret back, so a hook at the right address is still
			// signed with the old key when this run made a new one.
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
		if opts.RecreateWebhooks.Asked() {
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
// MINTED ONLY WHERE THERE IS NOTHING USABLE: the running engine holds the
// OLD secret, so minting every run makes GitHub sign with a key the edge
// refuses. A resolving secret is used as is; one is minted when there is
// none, or when the operator asked to recreate the hooks.
//
// ONE SECRET FOR EVERY TARGET: the edge verifies against the one
// `integrations.github.webhook_secret`.
// "Nothing usable" includes what this deployment already sealed
// ([provision.MintSecret]): the resolver answers from an apply-time
// snapshot, so trusting it alone sealed a second secret after every mint.
type webhookKey struct {
	// Secret is the value every hook is registered with. Empty only where
	// an error was returned: a hook must never be registered unsigned.
	Secret string

	// Minted is whether a FRESH secret was minted on this run, and it is
	// what lets a converged pass leave a working hook alone: GitHub never
	// gives a secret back, so "the hook already points at the right
	// address" is only enough when this run did not change the key it must
	// be signed with.
	Minted bool

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
	if resolved != "" && !opts.RecreateWebhooks.Asked() {
		return webhookKey{Secret: resolved}, nil
	}
	secretVar, ok := provision.SoleVar(opts.Config.WebhookSecret)
	if !ok && opts.RecreateWebhooks.Asked() {
		// A VALUE THE DOCUMENT OWNS, which the switch asked to replace:
		// refused naming the switch as its caller spells it and both
		// ways on ([provision.RefuseFresh]), never as a slot that
		// resolved to nothing, which is what the refusal below is about.
		if err := provision.RefuseFresh("integrations.github.webhook_secret",
			opts.RecreateWebhooks, opts.Config.WebhookSecret); err != nil {
			return webhookKey{}, fmt.Errorf("github: %w", err)
		}
	}
	if !ok {
		// THE VALUE IS NOT QUOTED, the rule [provision.Shape] states for
		// every refusal of a slot like this one. What reaches here holds
		// no value to register, but it can still hold text a credential
		// sits in — a reference embedded in other text, or a literal a
		// run handed no resolver could not read — and this error becomes
		// State.LastError. The path is what an operator needs, and the
		// path is what it says.
		return webhookKey{}, fmt.Errorf(
			"github: integrations.github.webhook_secret holds neither a value "+
				"this run could resolve nor a whole ${VAR} reference to mint "+
				"one into — point it at a variable, set that variable, or "+
				"clear both -public-url and %s and register %s by hand",
			setup.BaseKey(opts.WebhookBaseKey), target)
	}
	// rand.Text is 26 base32 characters over a 128-bit draw. GitHub
	// accepts any string as a webhook secret and signs with it verbatim,
	// so the only property that matters is that it is unguessable — there
	// is no shape to satisfy, unlike the self-hosted host's whsec_ form.
	secret, err := provision.MintSecret(ctx, opts.Sink, secretVar,
		opts.RecreateWebhooks.Asked(), func() (string, error) { return rand.Text(), nil })
	if err != nil {
		return webhookKey{}, fmt.Errorf("github: %w", err)
	}
	key := webhookKey{Secret: secret.Value, Minted: secret.Minted}
	if secret.Minted {
		note := fmt.Sprintf(
			"a fresh webhook secret was minted into %s — %s", secretVar,
			opts.Sink.NextStep())
		if opts.RecreateWebhooks.Asked() {
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
// The URL selected this hook, so what is left is the two fields a delivery
// depends on and the one thing that cannot be read back. A disabled hook
// delivers nothing; a hook missing an event type silences that one; a
// secret this run just replaced has to be sent, since GitHub returns none to
// compare.
//
// EXTRA events are not a reason to write: an operator added one on purpose.
// Converge what this engine needs and do not undo what it does not, as
// [integration.Reconciler] does.
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
