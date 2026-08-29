package jira

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
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

// WebhookName is the name the engine's own hook is registered under.
//
// Matched on the URL rather than this name, because an instance may carry
// hooks somebody else registered and a run that reconfigured the first one
// it found by name would take down an unrelated integration. The name is for
// the human reading Jira's admin page.
const WebhookName = "crewlet"

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
	// Detail carries the refusal for a project that could not be read.
	Detail string
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
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("jira: no client")
	}
	if opts.Config == nil {
		return nil, errors.New("jira: no jira config")
	}
	account, err := opts.Client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"jira: the org credential in integrations.jira.token was refused, "+
				"so nothing else this run reports would be trustworthy: %w", err)
	}

	res := &Result{Deployment: opts.Client.Deployment(), Account: account}
	res.Seats = resolveSeats(ctx, opts)
	res.Projects = checkProjects(ctx, opts, res.Seats)

	hooked, notes, err := ensureWebhook(ctx, opts)
	res.Notes = append(res.Notes, notes...)
	if err != nil {
		return res, err
	}
	res.Hooked = hooked
	if opts.Sink != nil {
		if err := opts.Sink.Flush(ctx); err != nil {
			return res, fmt.Errorf("jira: %w", err)
		}
	}
	return res, nil
}

// resolveSeats asks each seat's own credential who it is.
//
// CONCURRENTLY, because sequentially this is one round trip per seat against
// an instance that may be slow, and the whole point of the command is that
// an operator runs it and reads the answer. A seat whose lookup fails is
// reported unresolved rather than failing the run: the finding IS the
// report.
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

	var wg sync.WaitGroup
	for i, seat := range seats {
		out[i] = SeatIdentity{
			Handle:  seat.Handle(),
			Project: org.NormalizeScope(seat.JiraProject),
		}
		cred := CredentialOf(seat, opts.Value)
		if !cred.Held() {
			out[i].Reason = "no credential under mcp_env." +
				strings.Join(SeatEnvs, " or mcp_env.") +
				" — this seat receives no Jira events at all"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := NewClient(ClientOptions{
				URL:        opts.Client.URL(),
				Email:      cred.Email,
				Token:      cred.Token,
				Deployment: opts.Client.Deployment(),
			})
			if err != nil {
				out[i].Reason = err.Error()
				return
			}
			id, err := client.Me(ctx)
			if err != nil {
				out[i].Reason = err.Error()
				return
			}
			out[i].Account = id
		}()
	}
	wg.Wait()
	return out
}

// checkProjects reads every project the org declares.
func checkProjects(ctx context.Context, opts Options, seats []SeatIdentity) []ProjectCheck {
	keys := ProjectsOf(opts.Org)
	if len(keys) == 0 {
		return nil
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
		if err != nil {
			out[i].Detail = err.Error()
			continue
		}
		out[i].Exists = true
		out[i].Name = project.Name
		out[i].JiraLead = project.Lead
		out[i].JiraLeadName = project.LeadName
		out[i].JiraLeadHandle = byAccount[project.Lead]
	}
	return out
}

// ensureWebhook registers the inbound hook, or converges the one that is
// already there.
func ensureWebhook(ctx context.Context, opts Options) (string, []string, error) {
	target := webhookTarget(opts.WebhookBase)
	if target == "" {
		return "", []string{
			"no webhook was registered: pass the deployment's public base URL " +
				"to register one, or add it by hand — without it the instance " +
				"delivers nothing and the integration looks idle rather than " +
				"unconfigured"}, nil
	}
	if opts.Client.Deployment() == Cloud {
		// NOT A FAILURE, and not something a better credential fixes. On
		// Cloud a dynamic webhook belongs to an app, so this endpoint
		// refuses an API token however privileged it is. The Forge route
		// is how Cloud events reach this engine.
		return "", []string{
			"webhook registration was skipped: this is a Cloud instance, where " +
				"a webhook belongs to an app rather than to an API token. Cloud " +
				"events reach the engine through the Forge app on " +
				"/webhooks/forge, which is verified by its invocation token and " +
				"needs integrations.forge_app_id rather than a webhook secret"}, nil
	}

	secret, notes, err := webhookSecret(ctx, opts, target)
	if err != nil {
		return "", notes, err
	}

	hooks, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return "", notes, fmt.Errorf("jira: list webhooks: %w", err)
	}
	for _, hook := range hooks {
		if hook.URL != target {
			continue
		}
		if opts.RecreateWebhook {
			if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
				return "", notes, fmt.Errorf("jira: replace webhook: %w", err)
			}
			break
		}
		if _, err := opts.Client.UpdateWebhook(ctx, hook.ID, WebhookName, target, secret); err != nil {
			return "", notes, fmt.Errorf("jira: update webhook: %w", err)
		}
		return target, notes, nil
	}
	if _, err := opts.Client.CreateWebhook(ctx, WebhookName, target, secret); err != nil {
		return "", notes, fmt.Errorf("jira: create webhook: %w", err)
	}
	return target, notes, nil
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
func webhookSecret(ctx context.Context, opts Options, target string) (string, []string, error) {
	var resolved string
	if opts.Value != nil {
		resolved = strings.TrimSpace(opts.Value(opts.Config.WebhookSecret))
	}
	if resolved != "" && !opts.RecreateWebhook {
		return resolved, nil, nil
	}
	secretVar, ok := provision.SoleVar(opts.Config.WebhookSecret)
	if !ok {
		return "", nil, fmt.Errorf(
			"jira: integrations.jira.webhook_secret is %q, which is neither a "+
				"value this run could resolve nor a whole ${VAR} reference to "+
				"mint one into — point it at a variable, set that variable, or "+
				"drop -public-url and register %s by hand",
			opts.Config.WebhookSecret, target)
	}
	if opts.Sink == nil {
		return "", nil, provision.ErrNoSink
	}
	secret := rand.Text()
	if err := opts.Sink.Record(ctx, secretVar, secret); err != nil {
		return "", nil, fmt.Errorf("jira: record %s: %w", secretVar, err)
	}
	note := fmt.Sprintf(
		"a fresh webhook secret was minted into %s — %s", secretVar,
		opts.Sink.NextStep())
	if opts.RecreateWebhook {
		note += ". The previous secret is now invalid on every other " +
			"deployment of this company"
	}
	return secret, []string{note}, nil
}

// webhookTarget is the route a Jira delivery arrives on.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + "/webhooks/jira"
}
