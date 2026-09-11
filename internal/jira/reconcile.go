package jira

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"

	"github.com/crewlet/crewlet/internal/integration"
)

// Reconcile brings a Jira instance in line with the company config.
//
// # What Jira lets a provisioner do, and what it does not
//
// The chat backend and the tracker this build already serves can CREATE an
// account and mint its credential, so their reconciles are about converging
// a fleet of accounts. Jira cannot: a Cloud API token is issued by the
// person it belongs to at Atlassian's own account site, and a Data Center
// personal access token can only be minted for the calling user. A run that
// pretended otherwise would print instructions dressed as actions.
//
// So this reconcile does the three things Jira genuinely allows, and each of
// them answers a question that is otherwise invisible until an issue reaches
// nobody:
//
//   - WHICH ACCOUNT each seat's credential authenticates as. That mapping is
//     the whole of a seat's inbound routing, and nothing in the org model
//     declares it — the engine resolves it at boot and says nothing about
//     the seats it could not.
//   - WHETHER EVERY PROJECT the org names exists, and whether Jira's own
//     idea of who leads it agrees with the org chart's. A disagreement
//     splits notification: Jira's own mail goes one way, the engine's
//     lead-fallback goes another, and both halves look healthy.
//   - THE INBOUND WEBHOOK, on Data Center, registered with a secret the
//     engine holds. Without one the instance delivers nothing and the
//     integration looks idle rather than unconfigured.

// DefaultWebhookName is the name the engine's own hook is registered under
// when the company names none.
//
// Restated from `integrations.jira.webhook_name`, whose doc carries the
// reasoning, and asserted equal to [config.Jira.WebhookNameOrDefault] by a
// test.
const DefaultWebhookName = "crewlet"

// ours reports a hook this deployment registered.
//
// TWO CONDITIONS, and both are needed.
//
// THE NAME IS THE IDENTITY, because it is the only field that survives a
// change of public base. Matching on the URL instead meant a deployment that
// moved created a second hook and left the first: one live orphan per change,
// each delivering to an address that no longer answers, and a run that
// "converged" left three registrations behind. So the name is what selects,
// and this file's own comment used to say the opposite — matched on the URL
// "because an instance may carry hooks somebody else registered".
//
// THE DELIVERY PATH IS THE GUARD that concern deserves. Every hook this
// engine registers ends in /webhooks/jira whatever base it was registered
// against, so a hook that merely shares the name and points somewhere else
// is not this engine's and is left alone — which is the whole of what
// matching on the URL was protecting, kept without the orphans.
//
// Two DEPLOYMENTS of one company watching one instance is the case the name
// cannot settle, because they share this document: they set
// `integrations.jira.webhook_name` to two values, exactly as they would
// Datadog's.
func ours(hook Webhook, name string) bool {
	return hook.Name == name && strings.HasSuffix(hook.URL, webhookPath)
}

// webhookPath is where every Jira delivery arrives, whatever base carries it.
const webhookPath = "/webhooks/jira"

// Options are one reconcile's inputs.
type Options struct {
	// Client talks to the instance as the org account.
	Client *Client

	// Config is the company's jira block, UNRESOLVED: a minted webhook
	// secret goes INTO its `${VAR}`, so the reference has to survive.
	Config *config.Jira

	// Org is the company's org chart, for the seat and project walks.
	Org *org.Organization

	// Value resolves a config value — a literal or a `${VAR}` — to what it
	// holds. A function rather than the resolver itself, so this package
	// stays out of the config resolver's import graph.
	Value func(string) string

	// Sink records a minted webhook secret. Required only when one has to
	// be minted, which is why it is not checked up front: a run against an
	// instance whose secret is already set has nothing to record.
	Sink provision.TokenSink

	// WebhookBase is this deployment's public base URL, or empty to skip
	// webhook registration.
	//
	// SKIPPED RATHER THAN GUESSED: a hook pointing at the wrong host is
	// worse than no hook, because the instance then reports a healthy
	// integration that delivers into the void.
	WebhookBase string

	// RecreateWebhook deletes and remakes the hook to mint a fresh
	// secret, for the case where the existing one's secret was lost.
	// Destructive: it invalidates the secret every other deployment of
	// this company holds.
	RecreateWebhook bool
}

// SeatIdentity is one seat's tracker account, or why there is none.
type SeatIdentity struct {
	Handle string
	// Project is where the seat files, if it declares one.
	Project string
	// Account is the id the seat's own credential authenticates as. Empty
	// means this seat receives NO Jira events at all — which is the one
	// finding this command exists to surface.
	Account string
	// Reason says why an empty Account is empty, in terms an operator can
	// act on.
	Reason string
}

// Routes reports a seat whose inbound events can reach it.
func (s SeatIdentity) Routes() bool { return s.Account != "" }

// ProjectCheck is one declared project as the instance has it.
type ProjectCheck struct {
	Key  string
	Name string
	// Exists is false for a project the instance does not have, which is
	// almost always a typo in the org chart — and a silent one: the
	// webhook arrives, the key matches no lead, and the issue reaches
	// nobody.
	Exists bool
	// OrgLead is the handle the org chart says owns the project.
	OrgLead string
	// JiraLead is the account Jira itself calls the project's lead, and
	// JiraLeadHandle is the seat that account belongs to where one does.
	JiraLead       string
	JiraLeadName   string
	JiraLeadHandle string
}

// Agrees reports the two ideas of ownership pointing at one seat.
//
// UNKNOWN IS NOT DISAGREEMENT: a Jira lead who is simply not a seat here is
// an ordinary arrangement — a human manager owns the project and the org
// chart names the agent who triages it — so it is reported as a fact and
// never as a fault.
func (p ProjectCheck) Agrees() bool {
	return p.OrgLead != "" && p.JiraLeadHandle == p.OrgLead
}

// Result is what one reconcile found and did.
type Result struct {
	Deployment Deployment
	// Account is who the org credential authenticates as.
	Account string
	Seats   []SeatIdentity
	// Projects is every project the org declares, in key order.
	Projects []ProjectCheck
	// Hooked is the webhook target this run registered, or empty.
	Hooked string
	Notes  []string

	// NoIngress says why this run left the instance with no delivery path
	// at all, and is empty when it had an address to register one against
	// or when a hook is not how events arrive here.
	//
	// A SEPARATE FIELD because an empty Hooked means three unrelated
	// things: a read-only pass, a working Cloud company whose events come
	// through the Forge relay, and a deployment nothing can reach. Only
	// the third is a problem, and Classify over no findings is Ready — so
	// a Data Center company that never set its public base reported Jira
	// working while the instance had nowhere to deliver to.
	//
	// The zero value is "nothing to report", deliberately: a Result built
	// anywhere but Reconcile must not invent an ingress problem.
	NoIngress string

	// NoKeyring is a run that had to mint a webhook signing secret and had
	// nowhere to seal one, so it registered no hook.
	//
	// A STATE, not an error, and the distinction is the whole reason the
	// field exists — the same one [gitlab.Result] and [mattermost.Result]
	// draw. A fault reports the engine working on it and is retried for
	// ever; this never resolves until somebody sets secrets.keys or sets
	// the variable integrations.jira.webhook_secret points at. Reported as
	// a fault it was exactly the permanent-fault posture
	// [provision.ReadOnly] was introduced to remove: a node with no
	// keyring answered [provision.ErrNoSink] from Record on every tick,
	// for ever, over a deployment doing what it was configured to do.
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
// The one write this command makes is the webhook, and a run that registered
// it before discovering the credential was dead would leave an instance
// delivering to an engine that cannot enrich anything it receives.
//
// # THE SINK IS COMPLETED WHATEVER THE PASS DID
//
// [provision.TokenSink.Flush] is the point at which what a run sealed
// STANDS, and under the reconcile loop it is load-bearing rather than
// ceremonial: the loop's own sink rebuilds the engine's `${VAR}` snapshot
// there, and only there. This returned the pass's error several statements
// BEFORE reaching it, and the mint is earlier still — so a pass that minted a
// signing secret and then failed to reach the instance sealed a value and
// never announced it.
//
// On THIS pass that state is permanent rather than self-healing, and the
// reason is the fix that came before it. Everywhere the sealed-but-unflushed
// value is simply re-minted next tick, the damage is a rotation on a timer;
// here the next pass reads the sealed value back through
// [provision.TokenSink.Value] (see [webhookSecret]) and therefore never
// Records again — so the sink never seals again, so the snapshot is never
// rebuilt, for the life of the deployment. The instance ends up signing with
// a key the engine's own webhook route cannot resolve, every delivery is
// refused at the edge, and the pass reports Ready on every tick.
//
// So the flush happens on EVERY exit path, and it is not conditioned on
// whether this pass minted: a sink that recorded nothing has nothing to make
// durable and says so cheaply, where a "did we mint" flag is one more thing
// that has to stay in step with a mint several call frames away.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	res, err := reconcile(ctx, opts)
	if opts.Sink == nil {
		return res, err
	}
	// WITHOUT THE CALLER'S CANCELLATION when the pass is already failing,
	// which is the rule this tree applies to every rollback and teardown:
	// the failure being completed is frequently the cancellation itself —
	// a node shutting down mid-pass, having just sealed a secret — and a
	// completion that inherits a dead context does nothing at all, which is
	// the exact state above. A pass that SUCCEEDED keeps its caller's
	// deadline, because there is nothing to rescue and a completion that
	// outlives the request it belongs to is its own problem.
	flushCtx := ctx
	if err != nil {
		flushCtx = context.WithoutCancel(ctx)
	}
	if flushErr := opts.Sink.Flush(flushCtx); flushErr != nil {
		// JOINED, NEVER SUBSTITUTED. The pass's own error is the root
		// cause and callers route on it — [integration.Reject] classifies
		// it, and errors.Is against [integration.ErrCredentialRejected]
		// decides whether an operator is sent to rotate a token or told
		// to wait — so replacing it with a flush failure sends them to the
		// wrong place, and dropping the flush failure hides a sink this
		// deployment can no longer seal into. errors.Join keeps both
		// reachable to errors.Is and errors.As.
		return res, errors.Join(err, fmt.Errorf("jira: %w", flushErr))
	}
	return res, err
}

// reconcile is the pass itself, with no opinion about the sink's completion:
// see [Reconcile], which owns that on every path out of here.
func reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("jira: no client")
	}
	if opts.Config == nil {
		return nil, errors.New("jira: no jira config")
	}
	account, err := opts.Client.Me(ctx)
	if err != nil {
		// The probe exists to fail here rather than midway, so what it
		// reports has to say WHICH kind of failure it was: a refused
		// credential is the operator's to fix and never clears on its
		// own, where an unreachable third-party app clears without anybody.
		rejected := integration.Reject(err, Status(err))
		return nil, fmt.Errorf(
			"jira: the org credential in integrations.jira.token %s, "+
				"so nothing else this run reports would be trustworthy: %w",
			integration.Refusal(rejected), rejected)
	}

	res := &Result{Deployment: opts.Client.Deployment(), Account: account}
	seats, err := resolveSeats(ctx, opts)
	res.Seats = seats
	if err != nil {
		return res, err
	}
	projects, err := checkProjects(ctx, opts, res.Seats)
	res.Projects = projects
	if err != nil {
		return res, err
	}

	in, err := ensureWebhook(ctx, opts)
	res.Notes = append(res.Notes, in.Notes...)
	if err != nil {
		return res, err
	}
	res.Hooked = in.Hooked
	res.NoKeyring = in.NoKeyring
	res.NoIngress = noIngressReason(opts, res.Deployment)
	return res, nil
}

// resolveSeats asks each seat's own credential who it is.
//
// CONCURRENTLY, because sequentially this is one round trip per seat against
// an instance that may be slow, and the whole point of the command is that
// an operator runs it and reads the answer.
//
// # A credential the instance REFUSED is a finding; one it did not answer is
// a fault
//
// The same rule checkProjects states below, applied to the half of the walk
// that did not have it. Every lookup failure became the same empty Account,
// which [Result.Findings] renders as `identity_failed` — "this seat has no
// Jira account", owed by an ADMIN and never clearing on its own. That is a
// true statement about a token the instance refused with a 401, and a
// fabrication about an instance that did not answer at all: one blip, or one
// node shutting down mid-pass, reported every credentialled seat as an
// account somebody has to go and create, and reported it with the transport
// error as the reason.
//
// Three-valued, therefore, as everything else in this engine is: resolved,
// definitively not resolved, and NOT OBSERVED. The third raises, so the loop
// records it as this surface's fault and retries it, and the seats it never
// reached are not described at all.
func resolveSeats(ctx context.Context, opts Options) ([]SeatIdentity, error) {
	if opts.Org == nil {
		return nil, nil
	}
	var seats []*org.Role
	for seat := range opts.Org.AllRoles() {
		if !seat.IsHuman() {
			seats = append(seats, seat)
		}
	}
	out := make([]SeatIdentity, len(seats))

	// The credentials are read first so the fan-out below runs over the
	// seats that actually have one: a seat with no credential needs no
	// lookup, and letting it occupy a slot would idle part of the bound.
	creds := make([]Credential, len(seats))
	var lookups []int
	for i, seat := range seats {
		out[i] = SeatIdentity{
			Handle:  seat.Handle(),
			Project: org.NormalizeScope(seat.JiraProject),
		}
		creds[i] = CredentialOf(seat, opts.Value)
		if !creds[i].Held() {
			out[i].Reason = "no credential under mcp_env." +
				strings.Join(SeatEnvs, " or mcp_env.") +
				" — this seat receives no Jira events at all"
			continue
		}
		lookups = append(lookups, i)
	}

	// Written by INDEX from inside the fan-out, exactly as out is, so no
	// two goroutines touch one element and the scan below reads them in
	// seat order rather than in whatever order they finished. A pass that
	// raised on whichever lookup lost the race would report a different
	// seat on every run over one unchanged world.
	refusals := make([]error, len(seats))

	// BOUNDED, at the same cap as the engine's own resolvers and for the
	// same reason — see [provision.IdentityLookups]. This path is the
	// operator-invoked `crewlet jira provision`, so unbounded it opened
	// one socket per credentialled seat in a burst.
	provision.ResolveConcurrently(len(lookups), func(n int) {
		i := lookups[n]
		client, err := NewClient(ClientOptions{
			URL:        opts.Client.URL(),
			Email:      creds[i].Email,
			Token:      creds[i].Token,
			Deployment: opts.Client.Deployment(),
		})
		if err != nil {
			// THE CREDENTIAL ITSELF, not the instance: nothing was
			// asked, so there is nothing an instance could have failed
			// to answer. It is the document's to fix, which is what a
			// finding says.
			out[i].Reason = err.Error()
			return
		}
		id, err := client.Me(ctx)
		if err != nil {
			out[i].Reason = err.Error()
			refusals[i] = err
			return
		}
		out[i].Account = id
	})

	for _, i := range lookups {
		// A STATUS MEANS THE INSTANCE ANSWERED, whatever the number was,
		// and its answer about this credential is the report this command
		// exists to make. No status means the request never got one — a
		// dial that failed, a read that timed out, a cancelled context —
		// and that is the engine failing to look at the world rather than
		// anything about the seat.
		if refusals[i] != nil && Status(refusals[i]) == 0 {
			return out, fmt.Errorf(
				"jira: ask the instance which account %s authenticates as: %w",
				out[i].Handle, refusals[i])
		}
	}
	return out, nil
}

// checkProjects reads every project the org declares.
//
// A READ THAT FAILED IS A FAULT, NOT A FINDING, and that is
// [integration.Reconciler]'s own contract: "an error is the engine or the
// third-party app failing to look at that world at all, which the loop
// records as a fault and retries". It used to become a `grant_pending`
// finding — a kind defined as "nobody has to act; it resolves on its own",
// whose actor is the PROVIDER — so a permanent 403 from a token without
// Browse Projects reported forever that somebody else was working on it,
// while State.LastError stayed empty and the refusal appeared on no surface
// at all.
func checkProjects(
	ctx context.Context, opts Options, seats []SeatIdentity,
) ([]ProjectCheck, error) {
	keys := ProjectsOf(opts.Org)
	if len(keys) == 0 {
		return nil, nil
	}
	leads := LeadsFrom(opts.Org)
	byAccount := make(map[string]string, len(seats))
	for _, seat := range seats {
		if seat.Account != "" {
			byAccount[seat.Account] = seat.Handle
		}
	}

	out := make([]ProjectCheck, len(keys))
	for i, key := range keys {
		out[i] = ProjectCheck{Key: key, OrgLead: leads[key]}
		project, err := opts.Client.ProjectOf(ctx, key)
		switch {
		case err == nil:
		case notFound(err):
			// THE INSTANCE ANSWERED, and the answer was that there is no
			// such project. Exists stays false and Detail stays EMPTY,
			// which is what separates this from a read that failed.
			//
			// Collapsing the two was the bug: [ProjectCheck.Exists]
			// promises "a project the instance does not have, which is
			// almost always a typo in the org chart", and every failure
			// produced that same shape, so a typo and a timed-out
			// instance were indistinguishable. Downstream they want
			// opposite treatment: one is a document an operator must fix
			// and the other is worth another look in thirty seconds.
			continue
		default:
			// RAISED, so [integration.Observe] records it as this
			// surface's LastError and [integration.Reject] can route a
			// 401 or 403 to the operator rather than to the wait every
			// transport fault gets.
			return out, fmt.Errorf("jira: read project %s: %w",
				key, integration.Reject(err, Status(err)))
		}
		out[i].Exists = true
		out[i].Name = project.Name
		out[i].JiraLead = project.Lead
		out[i].JiraLeadName = project.LeadName
		out[i].JiraLeadHandle = byAccount[project.Lead]
	}
	return out, nil
}

// noIngressReason says why this run left the instance unable to deliver, or
// "".
//
// DATA CENTER ONLY. A Cloud webhook belongs to an app rather than to an API
// token, and those events arrive through the Forge relay at an address this
// engine did not register — so an empty Hooked there is a working company,
// and reporting it would park one on a block nobody can clear. On Data
// Center the hook this run registers IS how events arrive, and without a
// public base there is no address to put in it.
func noIngressReason(opts Options, deployment Deployment) string {
	if deployment == Cloud || webhookTarget(opts.WebhookBase) != "" {
		return ""
	}
	return "integrations.public_base_url is unset, so this Jira instance has " +
		"no address to deliver to and no issue or comment reaches this " +
		"deployment"
}

// ingress is what one pass did about inbound delivery, and why it did not do
// more.
//
// A STRUCT rather than a string, because "no hook" is three different
// answers to an operator — this run was not given an address, this node
// cannot hold a signing secret, or there was nothing to change — and only
// the first two are things anybody has to act on.
type ingress struct {
	// Hooked is the target this run registered or converged, or empty.
	Hooked string
	// NoKeyring is a run that had to mint a signing secret and had
	// nowhere to seal one. See [Result.NoKeyring].
	NoKeyring bool
	Notes     []string
}

// ensureWebhook registers the inbound hook, or converges the one that is
// already there.
func ensureWebhook(ctx context.Context, opts Options) (ingress, error) {
	target := webhookTarget(opts.WebhookBase)
	if target == "" {
		return ingress{Notes: []string{
			"no webhook was registered: pass the deployment's public base URL " +
				"to register one, or add it by hand — without it the instance " +
				"delivers nothing and the integration looks idle rather than " +
				"unconfigured"}}, nil
	}
	key, err := webhookSecret(ctx, opts, target)
	if err != nil {
		return ingress{Notes: key.Notes}, err
	}
	if key.NoKeyring {
		// NOTHING IS REGISTERED WITHOUT A KEY TO SIGN WITH. Jira signs a
		// delivery with whatever string the hook was registered under, and
		// the engine's own webhook route verifies with the value
		// integrations.jira.webhook_secret resolves to — which is nothing
		// here, or this run would not be minting. A hook registered
		// anyway would make the instance deliver, the edge refuse every
		// delivery, and both halves look busy.
		return ingress{NoKeyring: true, Notes: key.Notes}, nil
	}
	secret, minted, notes := key.Secret, key.Minted, key.Notes

	hooks, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return ingress{Notes: notes}, fmt.Errorf("jira: list webhooks: %w", err)
	}
	// BY NAME, NOT BY ADDRESS.
	//
	// It matched on the URL, so a hook this engine had registered at a
	// DIFFERENT address was invisible to it: changing the public base URL
	// created a second hook and left the first, and every change after that
	// added another. A deployment behind a tunnel accumulated one per
	// restart, all live, all delivering to addresses that no longer answer.
	//
	// The name is what says the hook is this engine's, so the name is what
	// converges: one hook, pointed wherever the company says it is reachable
	// now.
	// ONE HOOK, and the extras go.
	//
	// Converging on the first match still leaves every hook a previous
	// address created: this engine's own name on three live registrations,
	// two of them delivering to somewhere that no longer answers. What
	// "converged" has to mean is one.
	name := opts.Config.WebhookNameOrDefault()
	mine := make([]Webhook, 0, len(hooks))
	for _, hook := range hooks {
		if ours(hook, name) {
			mine = append(mine, hook)
		}
	}
	for _, extra := range mine[min(1, len(mine)):] {
		if err := opts.Client.DeleteWebhook(ctx, extra.ID); err != nil {
			return ingress{Notes: notes}, fmt.Errorf(
				"jira: remove a duplicate webhook: %w", err)
		}
	}
	if len(mine) > 0 {
		hook := mine[0]
		if opts.RecreateWebhook {
			if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
				return ingress{Notes: notes}, fmt.Errorf("jira: replace webhook: %w", err)
			}
		} else {
			if converged(hook, target, minted) {
				// ALREADY CORRECT, and left exactly as it is. This is
				// the steady state and where the loop spends its life:
				// [integration.DefaultSchedule] is anchored on a pass
				// issuing no writes when nothing has changed, and an
				// unconditional update was one write per pass, for ever,
				// on a hook that needed nothing.
				return ingress{Hooked: target, Notes: notes}, nil
			}
			if _, err := opts.Client.UpdateWebhook(
				ctx, hook.ID, name, target, secret); err != nil {
				return ingress{Notes: notes}, fmt.Errorf("jira: update webhook: %w", err)
			}
			return ingress{Hooked: target, Notes: notes}, nil
		}
	}
	if _, err := opts.Client.CreateWebhook(ctx, name, target, secret); err != nil {
		return ingress{Notes: notes}, fmt.Errorf("jira: create webhook: %w", err)
	}
	return ingress{Hooked: target, Notes: notes}, nil
}

// webhookKey is what this run will have the instance sign deliveries with.
type webhookKey struct {
	// Secret is the value, and it is empty only where NoKeyring is set or
	// an error was returned: a hook must never be registered unsigned.
	Secret string
	// Minted is whether a FRESH secret was minted on this run, and it is
	// what lets a converged pass leave a working hook alone: Jira never
	// gives a secret back, so "the hook already points at the right
	// address" is only enough when this run did not change the key it
	// must be signed with.
	Minted bool
	// NoKeyring is a run that had to mint and had nowhere to seal it. See
	// [Result.NoKeyring].
	NoKeyring bool
	Notes     []string
}

// webhookSecret is the value the hook is registered with.
//
// # Minted only where there is nothing usable
//
// The tempting shape is to mint every run, and it is an outage: the engine
// is running with the OLD secret, and re-registering with a fresh one makes
// the instance sign every delivery with a key the running engine does not
// hold — every webhook refused at the edge, from a command whose whole
// promise is that it is safe to re-run. So a secret that already resolves is
// used as it is, and minting happens when there is none, or when the
// operator asked to recreate the hook having planned the restart.
//
// # And "nothing usable" includes what this deployment already sealed
//
// [integration.Reconciler]'s safety contract states it outright — "check what
// the sink recorded (provision.TokenSink.Value) and keep a working
// credential" — and this pass was the one that did not. The resolver answers
// from a SNAPSHOT taken at apply time, so the variable a previous pass minted
// into resolves to nothing until something rebuilds it; every pass in that
// window minted a second secret, sealed it over the first and re-registered
// the hook with it. The engine's own webhook route verifies with the
// snapshot, so each rotation moved the instance further from the value the
// running process holds, on the reconcile loop's timer.
//
// So the sink is asked first, and only a name nothing holds is minted into.
// The read is THREE-VALUED like every other in this engine: held,
// definitively not held, and "the store could not say" — and the third
// raises, because minting over a value that may exist is the outage above.
func webhookSecret(
	ctx context.Context, opts Options, target string,
) (webhookKey, error) {
	var resolved string
	if opts.Value != nil {
		resolved = strings.TrimSpace(opts.Value(opts.Config.WebhookSecret))
	}
	if resolved != "" && !opts.RecreateWebhook {
		return webhookKey{Secret: resolved}, nil
	}
	secretVar, ok := provision.SoleVar(opts.Config.WebhookSecret)
	if !ok {
		// THE SHAPE, NEVER THE VALUE, the rule every other provisioner
		// here states at its own call site. This error reaches further
		// than theirs: it becomes State.LastError, which is written to
		// the fleet's coordination store and served on the integrations
		// query, so a %q of a literal signing secret publishes it.
		return webhookKey{}, fmt.Errorf(
			"jira: integrations.jira.webhook_secret is %s rather than a value "+
				"this run could resolve or a whole ${VAR} reference to mint "+
				"one into — point it at a variable, set that variable, or "+
				"clear both -public-url and integrations.public_base_url and "+
				"register %s by hand",
			provision.Shape(opts.Config.WebhookSecret), target)
	}
	if opts.Sink == nil {
		// THE COMMAND LINE'S CASE, and it stays a refusal: a run told to
		// mint with nowhere to put the result would leave a live signing
		// secret at the instance and print none of it.
		return webhookKey{}, provision.ErrNoSink
	}
	if !provision.CanMint(opts.Sink) {
		// A NODE WITH NO KEYRING REPORTS, IT DOES NOT FAULT. Record
		// answers [provision.ErrNoSink] on such a sink, so this pass
		// raised on every tick for ever over a deployment that had simply
		// not set secrets.keys — the exact permanent-fault posture
		// [provision.ReadOnly] exists to remove. See [Result.NoKeyring].
		return webhookKey{NoKeyring: true}, nil
	}
	if !opts.RecreateWebhook {
		// ASKED BEFORE ANYTHING IS MINTED, and skipped only where the
		// operator asked for a fresh key having planned the restart that
		// rotating one costs.
		held, ok, err := opts.Sink.Value(ctx, secretVar)
		if err != nil {
			return webhookKey{}, fmt.Errorf("jira: read %s: %w", secretVar, err)
		}
		if ok && strings.TrimSpace(held) != "" {
			return webhookKey{Secret: strings.TrimSpace(held)}, nil
		}
	}
	fresh := rand.Text()
	if recordErr := opts.Sink.Record(ctx, secretVar, fresh); recordErr != nil {
		return webhookKey{}, fmt.Errorf("jira: record %s: %w", secretVar, recordErr)
	}
	note := fmt.Sprintf(
		"a fresh webhook secret was minted into %s — %s", secretVar,
		opts.Sink.NextStep())
	if opts.RecreateWebhook {
		note += ". The previous secret is now invalid on every other " +
			"deployment of this company"
	}
	return webhookKey{Secret: fresh, Minted: true, Notes: []string{note}}, nil
}

// converged reports a hook that already carries everything this run would
// write, so writing it again is a request spent to change nothing.
//
// The name and the address are the caller's business — they are what selected
// this hook and what it is being pointed at. What is left is whether it
// delivers at all, whether it carries every event the parser reads, and the
// one thing that cannot be read back: a secret this run just replaced has to
// be sent, because Jira answers with no secret and there is nothing to
// compare.
//
// EXTRA events are not a reason to write, for the reason the GitHub side
// gives: converge what this engine needs and do not undo what it does not.
func converged(hook Webhook, target string, minted bool) bool {
	if minted || !hook.Enabled || hook.URL != target {
		return false
	}
	for _, want := range WebhookEvents {
		if !slices.Contains(hook.Events, want) {
			return false
		}
	}
	return true
}

// webhookTarget is the route a Jira delivery arrives on.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + webhookPath
}

// notFound reports a refusal that means the instance has no such thing.
//
// Through [APIError.Status] rather than by matching the message, which is
// exactly what that type exists for: the wording of a Jira refusal differs by
// version and by locale, and a substring match on it is a check that stops
// working when somebody's instance is in German.
func notFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
