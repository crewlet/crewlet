package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"strings"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
)

// Running a third-party app's provisioning from the API rather than from a shell.
//
// # This is where the loop's two deliberate absences are filled in
//
// The reconcile loop hands every third-party app a pass with no sink and no webhook
// base, and states plainly why: a base is permission to register a hook and a
// sink is permission to mint a credential, and neither is a decision a timer
// gets to make. A pass here supplies both, because a person asked for it.
//
// The third-party app function is the SAME one, in every case. Nothing about
// provisioning is reimplemented for the API: what an adapter does is resolve
// the config, build the client, and hand over the sink and the base the loop
// withholds.

// setupPasses are the third-party apps this build can provision over the API.
//
// The three whose passes neither create an account nor issue a credential AT
// the third-party app. Each reads the instance, mints a webhook credential whose value
// is the engine's own on both ends, and registers a hook, and none of them
// produces anything a person then owns and has to be told about.
//
// GitLab is here too, and it is a different kind of act: its pass creates a
// service account per agent, mints a token on each and adds them to a group.
// Everything it makes outlives the run and is visible to the whole group, so
// it runs only with a transient administrator credential asked for on every
// pass and never stored, and it never deletes: decommissioning is left to the
// command line, because a company mid-edit looks exactly like one that
// removed a seat.
//
// Mattermost is the same shape as GitLab and joins on the same terms. It is
// the one third-party app here that needs no public address at all: it holds an
// outbound websocket per seat and verifies no inbound delivery, so its pass
// creates accounts and registers nothing.
//
// Slack stays on the command line. Its apps are created through Slack's
// app-manifest API, which authenticates with a configuration token Slack
// issues only by hand and which an organisation may not permit at all, and
// the record of what was created lives in a local ledger file rather than in
// the fleet.
func (e *Engine) setupPasses() []setup.Pass {
	return []setup.Pass{
		&githubPass{engine: e},
		&jiraPass{engine: e},
		&confluencePass{engine: e},
		&gitlabPass{engine: e},
		&mattermostPass{engine: e},
		&datadogPass{engine: e},
		&atlassianPass{engine: e},
	}
}

// atlassianPass creates one Atlassian service account per agent.
//
// THE ONE PASS WHOSE CREDENTIAL IS NOT THE INTEGRATION'S. Jira and Confluence
// authenticate as an account against a site; this authenticates as the
// ORGANIZATION those sites belong to, which is the only place an identity can
// be created. So it is its own pass with its own block rather than a step
// inside either product's.
type atlassianPass struct{ engine *Engine }

func (*atlassianPass) Kind() integration.Kind { return integration.KindAtlassian }

// Needs is nil: the organization key is held in the company document like
// every other credential.
func (*atlassianPass) Needs() *setup.Requirement { return nil }

func (p *atlassianPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Atlassian
	if cfg == nil {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	key := strings.TrimSpace(env.Value(cfg.APIKey))
	// THROUGH THE RESOLVER, like the key beside it. The organization id is
	// not a credential, but it is a value in the same document, and a
	// company keeping it in the sealed store had the literal text
	// "${ATLASSIAN_ORG_ID}" sent to Atlassian as the subject of every
	// admin call, which is refused with nothing naming the cause.
	org := strings.TrimSpace(env.Value(cfg.OrgID))
	if org == "" || key == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Atlassian organization id and its API key did not both " +
				"resolve, and the admin APIs need the organization as the " +
				"subject and the key as the authority",
		}}, nil
	}
	plan, err := atlassian.PlanFor(company.Org)
	if err != nil {
		return nil, fmt.Errorf("engine: atlassian pass: %w", err)
	}
	res, err := atlassian.Reconcile(ctx, atlassian.Options{
		Client: atlassian.NewClient(atlassian.ClientOptions{}),
		OrgID:  org, Key: key, Plan: plan, Sink: in.Sink,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: atlassian pass: %w", err)
	}
	findings := res.Findings()
	if note := p.recordSite(ctx, company, res.Site, in.Sink != nil); note != "" {
		findings = append(findings, integration.Finding{
			Kind: integration.FindingGrantShort, Detail: note,
		})
	}
	return findings, nil
}

// recordSite writes the site this pass discovered into the two product
// blocks, and says why it could not when it could not.
//
// NOBODY TYPES AN ADDRESS THE ORGANIZATION ALREADY KNOWS. The org key lists
// the products and their host, so asking an operator for a site address, a
// cloud id and a link address is asking them to copy three values out of a
// console this engine is already reading.
//
// The CLOUD ID is the load-bearing one: a provisioned service account's token
// is refused by the site host and accepted only at the API gateway, so a
// company whose agents hold provisioned accounts must reach Atlassian through
// it. A company that set these itself is left alone: the document is a
// decision, and this only fills a blank.
func (p *atlassianPass) recordSite(
	ctx context.Context, company *Company, site atlassian.Site, writing bool,
) string {
	if !writing || site.CloudID == "" {
		return ""
	}
	patch := map[string]map[string]string{}
	if j := company.Config.Integrations.Jira; j != nil && j.CloudID == "" {
		patch["jira"] = map[string]string{"cloud_id": site.CloudID, "site_url": site.HostURL}
	}
	if c := company.Config.Integrations.Confluence; c != nil && c.CloudID == "" {
		patch["confluence"] = map[string]string{
			"cloud_id": site.CloudID, "site_url": site.HostURL + "/wiki",
		}
	}
	if len(patch) == 0 {
		return ""
	}
	writer := p.engine.configWriterOrNil()
	if writer == nil {
		return "this node cannot write the company configuration, so the Atlassian " +
			"site it discovered was not recorded: set integrations.jira.cloud_id " +
			"and integrations.confluence.cloud_id to " + site.CloudID
	}
	body, err := json.Marshal(map[string]any{"integrations": patch})
	if err != nil {
		return "the discovered Atlassian site could not be encoded: " + err.Error()
	}
	if err := writer.Apply(ctx, body, "record the Atlassian site", reconcileOperator); err != nil {
		return "the Atlassian site could not be recorded: " + err.Error()
	}
	return ""
}

// Teardown deletes the accounts this pass created, when asked.
func (p *atlassianPass) Teardown(ctx context.Context, in setup.TeardownInput) error {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Atlassian
	if cfg == nil || !in.RemoveSeats {
		// Atlassian holds no webhook this engine registered (Cloud events
		// arrive through the Forge relay), so with the accounts staying
		// there is nothing to do at all.
		return nil
	}
	env := p.engine.resolver()
	key := strings.TrimSpace(env.Value(cfg.APIKey))
	if key == "" {
		return nil
	}
	plan, err := atlassian.PlanFor(company.Org)
	if err != nil {
		return fmt.Errorf("engine: atlassian teardown: %w", err)
	}
	return atlassian.Teardown(ctx, atlassian.TeardownOptions{
		Client: atlassian.NewClient(atlassian.ClientOptions{}),
		OrgID:  strings.TrimSpace(env.Value(cfg.OrgID)), Key: key, Plan: plan,
	})
}

var _ setup.Teardowner = (*atlassianPass)(nil)

// SetupRunner is the pass runner this node serves, or nil when it has no
// secret store to mint into.
//
// Nil rather than a runner that refuses: a pass that cannot record what it
// mints must not run at all, and the surface above answers 503 naming the
// keyring rather than starting something it will have to unwind.
//
// THE SAME RUNNER EVERY TIME, built once — see [Engine.setupRunner]. A caller
// that wants its own clock builds its own with [setup.NewRunner]; there is no
// second runner in the engine, because a second in-process claim map would
// mean the loop and the dashboard guard nothing against each other.
func (e *Engine) SetupRunner() *setup.Runner {
	if e == nil || e.setupRunner == nil {
		return nil
	}
	return e.setupRunner()
}

// newSetupRunner builds the one runner. Called through a [sync.OnceValue]
// installed by the constructor.
func (e *Engine) newSetupRunner() *setup.Runner {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return setup.NewRunner(e.setupPasses(), e.setupDuty, nil)
}

// holdSurface takes the one guard every writer at a surface passes through.
//
// The loop's tick and a disconnect's teardown reach a third-party app without
// going through [setup.Runner.Start], so they take the guard here instead —
// the same in-process claim and the same fleet lease an operator's pass takes,
// which is what makes the three mutually exclusive rather than merely
// serialized in pairs.
//
// A node with no runner has no keyring to mint into and therefore nothing to
// serialize: it reads and reports. held is true there, and release is a no-op.
func (e *Engine) holdSurface(
	ctx context.Context, kind integration.Kind,
) (func(), bool, error) {
	runner := e.SetupRunner()
	if runner == nil {
		return func() {}, true, nil
	}
	return runner.Hold(ctx, kind)
}

// setupDutyName is the ONE name a surface's provisioning is serialized under.
//
// A function rather than a spelling at each call site, because two spellings
// is precisely the bug this had. The dashboard's pass claimed
// `setup-provision-<kind>` and the reconcile loop claimed
// `integration-reconcile`; leases are keyed by NAME, so those are two locks,
// neither excluding the other and both looking exactly like exclusion. The
// loop's singleton name is still its own — "which node runs the loop" and
// "who is writing at this surface right now" are different questions — and
// this is the second one, asked by both callers under one key.
func setupDutyName(kind integration.Kind) string {
	return "setup-provision-" + string(kind)
}

// setupDuty is the fleet lease held while ANYTHING writes at one integration —
// a pass an operator ran, a tick of the reconcile loop, or a disconnect.
//
// All three write at the third-party app, and letting any two overlap is how
// one creates the account another is deleting, or a disconnect removes the
// webhook the pass beside it is registering. The TTL is generous relative to a
// pass because it is a BACKSTOP for a node that died mid-run rather than a
// deadline for the work: the lease is given back when the work ends.
func (e *Engine) setupDuty(kind integration.Kind) setup.Duty {
	hold := e.workerHold(setupDutyName(kind), setupLeaseTTL)
	if hold == nil {
		return nil
	}
	return setup.Duty(hold)
}

// setupLeaseTTL bounds how long one pass may hold its third-party app.
//
// Five minutes against passes measured in seconds: the value is a backstop
// for a node that died mid-run, not a deadline for the work. Shorter would
// risk a live pass losing its lease; much longer would leave a third-party app locked
// out after a crash for no benefit.
const setupLeaseTTL = 5 * time.Minute

// Teardown removes the webhooks this pass registered, at both the
// organization and the repository level.
func (p *githubPass) Teardown(ctx context.Context, in setup.TeardownInput) error {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitHub
	if cfg == nil {
		return nil
	}
	env := p.engine.resolver()
	client, err := githubReconcileClient(cfg, env)
	if err != nil {
		return fmt.Errorf("engine: github teardown: %w", err)
	}
	if err := github.Teardown(ctx, github.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		WebhookBase: company.Config.Integrations.WebhookBase(env.LookupOK),
	}); err != nil {
		return err
	}
	if !in.RemoveSeats {
		return nil
	}

	// UNINSTALLING IS WHAT ACTUALLY REVOKES ACCESS, and it is the half the
	// engine can do. Deleting the app REGISTRATION is the operator's: GitHub
	// has no API for it, so an app deletes only from its own settings page
	// in a browser. Uninstalling first means the moment a person presses
	// Disconnect the agent can no longer read or write anything, whether or
	// not they get round to deleting the registration.
	//
	// Every seat is attempted and the failures are joined, rather than
	// stopping at the first: one seat whose key is lost must not leave the
	// other nine installed.
	apiBase, _ := cfg.Bases(env.LookupOK)
	var failures []error
	for _, seat := range p.seatApps(env) {
		if err := github.UninstallSeat(ctx, github.SeatAppOptions{
			APIBase: apiBase,
		}, seat); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", seat.Handle, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf(
			"engine: github teardown: some agents' apps are still installed and "+
				"can still act, so the block stays until they are not: %w",
			errors.Join(failures...))
	}
	return nil
}

// Teardown disables the bots this pass created, when asked. Mattermost has no
// inbound registration to withdraw, so there is nothing to do otherwise.
func (p *mattermostPass) Teardown(ctx context.Context, in setup.TeardownInput) error {
	if !in.RemoveSeats {
		return nil
	}
	company := p.engine.Company()
	cfg := company.Config.Integrations.Mattermost
	if cfg == nil {
		return nil
	}
	env := p.engine.resolver()
	admin := mattermostAdminToken(cfg, env, in.Operator)
	if admin == "" {
		return fmt.Errorf(
			"engine: mattermost teardown: no admin token resolved, and the " +
				"bots' own tokens cannot disable them")
	}
	plan, err := mattermost.PlanFor(company.Org, cfg)
	if err != nil {
		return fmt.Errorf("engine: mattermost teardown: %w", err)
	}
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: env.Value(cfg.URL), Token: admin,
	})
	if err != nil {
		return fmt.Errorf("engine: mattermost teardown: %w", err)
	}
	return mattermost.Teardown(ctx, mattermost.TeardownOptions{
		Client: client, Config: cfg, Plan: plan, RemoveSeats: in.RemoveSeats,
	})
}

// datadogPass adapts datadog.Reconcile to the pass contract.
//
// The one app here whose provisioning is OPTIONAL to its integration. Alerts
// arrive and route with no credentials at all, so a company that has not
// filled in the region and the keys is not half-configured: it is running the
// integration the way it has always run. The pass says so rather than
// failing.
type datadogPass struct{ engine *Engine }

func (*datadogPass) Kind() integration.Kind { return integration.KindDatadog }

// Needs is nil: the organization keys are held in the company document like
// every other credential.
func (*datadogPass) Needs() *setup.Requirement { return nil }

func (p *datadogPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Datadog
	// DISABLED IS NOT CONFIGURED, the same answer gitlab, github and
	// mattermost give. `enabled: false` is the gesture an operator makes
	// after a leaked webhook token — the inbound route, the parser and the
	// notification wiring all gate on it — so a loop that went on
	// provisioning service accounts against a block somebody had just
	// switched off, and reported the surface Connected while doing it, is
	// answering a question nobody asked. `Datadog.validate` does not even
	// check a disabled block, so there is no guarantee the fields this pass
	// would read are coherent.
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	if cfg.Provisioning == nil {
		// A FINDING, NOT A FAULT, and not an error either: this company
		// accepts alerts and asked for no identities, which is a complete
		// configuration rather than a missing one.
		return nil, nil
	}
	env := p.engine.resolver()
	// ASKED BEFORE BUILDING, so an unset or misspelled region is a
	// FINDING rather than a fault: it is a fact about what this company
	// has written down, and a pass that could not build a client has
	// observed nothing rather than failed to look.
	if !datadog.KnownSite(cfg.Provisioning.Site) {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "integrations.datadog.provisioning.site is not a region " +
				"Datadog serves, so no call can be built from it",
		}}, nil
	}
	client, err := datadog.NewClient(datadog.ClientOptions{Site: cfg.Provisioning.Site})
	if err != nil {
		return nil, fmt.Errorf("engine: datadog pass: %w", err)
	}
	creds := datadog.Credentials{
		APIKey: strings.TrimSpace(env.Value(cfg.Provisioning.APIKey)),
		AppKey: strings.TrimSpace(env.Value(cfg.Provisioning.AppKey)),
	}
	if creds.APIKey == "" || creds.AppKey == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Datadog API key and application key did not both " +
				"resolve, and Datadog refuses a write carrying only one",
		}}, nil
	}
	plan, err := datadog.PlanFor(company.Org, cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: datadog pass: %w", err)
	}
	res, err := datadog.Reconcile(ctx, datadog.Options{
		Client: client, Config: cfg, Plan: plan, Creds: creds, Sink: in.Sink,
		// THE INBOUND HALF. Supplying the base IS the permission to
		// register the webhook, on the same terms as every other surface,
		// and the token is what the definition carries so the route it
		// posts to accepts the delivery.
		WebhookBase:  in.WebhookBase,
		WebhookToken: strings.TrimSpace(env.Value(cfg.WebhookToken)),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: datadog pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown disables the accounts this pass created, when asked.
func (p *datadogPass) Teardown(ctx context.Context, in setup.TeardownInput) error {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Datadog
	if cfg == nil || cfg.Provisioning == nil {
		return nil
	}
	env := p.engine.resolver()
	client, err := datadog.NewClient(datadog.ClientOptions{Site: cfg.Provisioning.Site})
	if err != nil {
		return fmt.Errorf("engine: datadog teardown: %w", err)
	}
	// THE PLAN IS ONLY THE ACCOUNTS' HALF, so a company whose roster
	// cannot be planned still has its webhook withdrawn: the definition is
	// named by the config alone.
	plan, err := datadog.PlanFor(company.Org, cfg)
	if err != nil && in.RemoveSeats {
		return fmt.Errorf("engine: datadog teardown: %w", err)
	}
	return datadog.Teardown(ctx, datadog.TeardownOptions{
		Client: client, Config: cfg, Plan: plan,
		Creds: datadog.Credentials{
			APIKey: strings.TrimSpace(env.Value(cfg.Provisioning.APIKey)),
			AppKey: strings.TrimSpace(env.Value(cfg.Provisioning.AppKey)),
		},
		RemoveSeats: in.RemoveSeats,
		// WHAT PROVES THE DEFINITION IS THIS DEPLOYMENT'S. A Datadog
		// webhook is addressed by name, and the teardown refuses to
		// delete one pointing anywhere but here — the same expression
		// gitlabPass.Teardown passes for the same reason.
		WebhookBase: company.Config.Integrations.WebhookBase(env.LookupOK),
	})
}

// SetupSink is the recorder a pass writes minted credentials through.
//
// The SAME type `crewlet <integration> provision -secret-store` builds, so a
// credential minted from the dashboard and one minted from a shell land in
// the same place under the same envelope. Write-through, so a value is
// durable before the next one is minted.
func (e *Engine) SetupSink(operator string) (provision.TokenSink, error) {
	if e.cipher == nil {
		return nil, fmt.Errorf("engine: this node has no keyring, so a minted credential cannot be sealed")
	}
	return &refreshingSink{
		TokenSink: provision.NewSecretStoreSink(
			fleetsecrets.New(e.backends.Fleet, e.cipher), operator),
		engine: e,
	}, nil
}

// refreshingSink rebuilds the resolver's snapshot after a run seals anything.
//
// # A SECRET NOBODY CAN SEE IS A PASS THAT DID NOTHING
//
// `${VAR}` resolves from a SNAPSHOT taken at apply time, which is what keeps
// the secret store off the path of every config read. Nothing about minting a
// credential advances an epoch, though: a pass created a GitLab service
// account, minted its token and sealed it under the name the seat pointed at,
// and every surface went on reporting that seat as waiting for an account,
// because the resolver was still holding the snapshot from before the seal.
// The pass then ran again on the next tick, found the same unresolved
// variable, and minted a second token, for ever.
//
// AFTER FLUSH, not per Record: a run that seals five credentials and is then
// rolled back should leave the snapshot where it was, and Flush is the point
// at which what was written is what stands. A refresh that fails leaves the
// previous snapshot standing, which is the same posture an apply takes.
type refreshingSink struct {
	provision.TokenSink
	engine  *Engine
	sealed  bool
	flushed bool
}

func (s *refreshingSink) Record(ctx context.Context, name, value string) error {
	if err := s.TokenSink.Record(ctx, name, value); err != nil {
		return err
	}
	s.sealed = true
	return nil
}

func (s *refreshingSink) Flush(ctx context.Context) error {
	err := s.TokenSink.Flush(ctx)
	// EVEN ON A FAILED FLUSH, because a write-through sink has already
	// made its values durable: what failed is the completion, and the
	// credentials are out there either way.
	if s.sealed && !s.flushed {
		s.flushed = true
		s.engine.refreshSecrets(ctx)
	}
	return err
}

// jiraPass adapts jira.Reconcile to the pass contract.
type jiraPass struct{ engine *Engine }

func (*jiraPass) Kind() integration.Kind { return integration.KindJira }

// Needs is nil: the org account already in the config is what registers the
// hook, so there is no second administrator credential to ask for.
func (*jiraPass) Needs() *setup.Requirement { return nil }

func (p *jiraPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Jira
	if cfg == nil {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	base := jiraBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		// The same two facts the loop reports as findings, and reported
		// the same way here: a pass that cannot reach the instance has
		// observed nothing, and calling that a fault would send an
		// operator looking for an outage.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Jira site address or the org token did not resolve, so " +
				"this pass could not reach the instance",
		}}, nil
	}
	client, err := jira.NewClient(jira.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira pass: %w", err)
	}
	res, err := jira.Reconcile(ctx, jira.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		Sink: in.Sink, WebhookBase: in.WebhookBase,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown removes the webhook this pass registered.
//
// The third-party app function is the same one a decommission from the command line
// would call, exactly as [jiraPass.Run] uses the same Reconcile the loop
// does. Nothing about removal is reimplemented for the API.
func (p *jiraPass) Teardown(ctx context.Context, in setup.TeardownInput) error {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Jira
	if cfg == nil {
		// The block already left the document, so there is nothing to
		// authenticate with and nothing this engine still holds.
		return nil
	}
	env := p.engine.resolver()
	base := jiraBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		return fmt.Errorf(
			"engine: jira teardown: the site address or the org token did not " +
				"resolve, so the webhook cannot be removed: fix the credential " +
				"or force the disconnect and remove the hook by hand")
	}
	client, err := jira.NewClient(jira.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return fmt.Errorf("engine: jira teardown: %w", err)
	}
	return jira.Teardown(ctx, jira.Options{
		Client: client, Config: cfg,
		WebhookBase: company.Config.Integrations.WebhookBase(env.LookupOK),
	})
}

// confluencePass adapts confluence.Reconcile to the pass contract.
type confluencePass struct{ engine *Engine }

func (*confluencePass) Kind() integration.Kind { return integration.KindConfluence }

func (*confluencePass) Needs() *setup.Requirement { return nil }

func (p *confluencePass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Confluence
	if cfg == nil {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	base := confluenceBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Confluence site address or the org token did not resolve, " +
				"so this pass could not reach the instance",
		}}, nil
	}
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: confluence pass: %w", err)
	}
	res, err := confluence.Reconcile(ctx, confluence.Options{
		Client: client, Config: cfg, Value: env.Value,
		Sink: in.Sink, WebhookBase: in.WebhookBase, Recreate: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: confluence pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown removes the hooks this pass registered.
func (p *confluencePass) Teardown(ctx context.Context, in setup.TeardownInput) error {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Confluence
	if cfg == nil {
		return nil
	}
	env := p.engine.resolver()
	base := confluenceBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		return fmt.Errorf(
			"engine: confluence teardown: the site address or the org token did " +
				"not resolve, so the hooks cannot be removed: fix the credential " +
				"or force the disconnect and remove them by hand")
	}
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return fmt.Errorf("engine: confluence teardown: %w", err)
	}
	return confluence.Teardown(ctx, confluence.Options{Client: client, Config: cfg})
}

// gitlabPass adapts gitlab.Reconcile to the pass contract.
type gitlabPass struct{ engine *Engine }

func (*gitlabPass) Kind() integration.Kind { return integration.KindGitLab }

// Needs is the group Owner token. Asked on every run and never stored: it
// creates accounts and mints tokens on them, which is a standing power if it
// is kept and a grant with an end if it is not.
// Needs is nil: the Owner token is held in the company document like every
// other credential now, so there is nothing transient left to ask for. See
// [gitlab.AdminCredential] for why it is kept.
func (*gitlabPass) Needs() *setup.Requirement { return nil }

func (p *gitlabPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	// The HELD credential, with a per-run override still honoured: an
	// operator rotating the token can run a pass with the new one before
	// the document carries it.
	admin := gitlabAdminToken(cfg, env, in.Operator)
	if admin == "" {
		// A FINDING, NOT A FAULT: the operator has not supplied the one
		// credential this pass cannot mint for itself, which is a fact
		// about what is missing rather than a failure to look.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "no group Owner token resolved, and the seats' own tokens " +
				"are what this pass mints, so it cannot bootstrap itself from them",
		}}, nil
	}
	plan, err := gitlab.PlanFor(company.Org, cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: env.Value(cfg.URL), Token: admin,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	signingVar, _ := provision.SoleVar(cfg.SigningSecret)
	res, err := gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: in.Sink,
		WebhookBase:      in.WebhookBase,
		SigningSecret:    env.Value(cfg.SigningSecret),
		SigningSecretVar: signingVar,
		// ROTATE ONLY WHEN ASKED, and NEVER DECOMMISSION from here.
		// Rotating revokes the credential every agent is currently
		// authenticating with, so an operator adding a tenth seat would
		// take the other nine down; deleting an account because a seat
		// left the config cannot be told apart from a company mid-edit.
		// Both stay deliberate gestures on the command line.
		Rotate: in.Recreate,
	})
	// A NAME GITLAB HAS NOT RELEASED YET IS WORK IN PROGRESS, not a failure
	// to read the integration.
	//
	// GitLab removes a user asynchronously, so a disconnect followed by a
	// reconnect inside that window finds no account, creates one, and is
	// refused with "has already been taken". Reported as an error, the card
	// read "the last pass could not read this integration", which sends an
	// operator looking for an outage over a state the next tick clears.
	if errors.Is(err, gitlab.ErrNameReserved) {
		return []integration.Finding{{
			Kind: integration.FindingIdentityMissing,
			Detail: "GitLab is still releasing the name of an account it is " +
				"deleting, so this seat's account cannot be created yet: " +
				"the next pass makes it, usually within a minute",
		}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown withdraws the hooks, and the service accounts when asked.
//
// The Owner token is the same transient credential the pass asks for, and it
// is needed for the same reason: removing an account takes the authority
// creating it did. A teardown with none can still be attempted (the hooks
// may come out under a weaker credential), so this refuses only when there is
// nothing at all to authenticate with.
func (p *gitlabPass) Teardown(ctx context.Context, in setup.TeardownInput) error {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitLab
	if cfg == nil {
		return nil
	}
	env := p.engine.resolver()
	admin := gitlabAdminToken(cfg, env, in.Operator)
	if admin == "" {
		return fmt.Errorf(
			"engine: gitlab teardown: no group Owner token resolved, and the " +
				"seats' own tokens cannot remove what created them")
	}
	plan, err := gitlab.PlanFor(company.Org, cfg)
	if err != nil {
		return fmt.Errorf("engine: gitlab teardown: %w", err)
	}
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: env.Value(cfg.URL), Token: admin,
	})
	if err != nil {
		return fmt.Errorf("engine: gitlab teardown: %w", err)
	}
	return gitlab.Teardown(ctx, gitlab.TeardownOptions{
		Client: client, Config: cfg, Plan: plan,
		WebhookBase: company.Config.Integrations.WebhookBase(env.LookupOK),
		RemoveSeats: in.RemoveSeats,
	})
}

// mattermostPass adapts mattermost.Reconcile to the pass contract.
type mattermostPass struct{ engine *Engine }

func (*mattermostPass) Kind() integration.Kind { return integration.KindMattermost }

// Needs is the administrator token. Asked on every run and never stored, for
// the reason GitLab's is: it creates accounts and mints tokens on them.
// Needs is nil, for the reason GitLab's is: the administrator token is held.
func (*mattermostPass) Needs() *setup.Requirement { return nil }

func (p *mattermostPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	admin := mattermostAdminToken(cfg, env, in.Operator)
	if admin == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "no administrator token resolved, and the bots' own tokens " +
				"are what this pass mints, so it cannot bootstrap itself from them",
		}}, nil
	}
	plan, err := mattermost.PlanFor(company.Org, cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: env.Value(cfg.URL), Token: admin,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	// NO WEBHOOK BASE IS PASSED because there is nowhere to pass it: this
	// third-party app holds an outbound socket per seat and registers nothing. And
	// no rotation and no decommissioning, for the reasons GitLab's pass
	// gives: both take working agents down and both stay deliberate
	// command-line gestures.
	res, err := mattermost.Reconcile(ctx, mattermost.Options{
		Client: client, Config: cfg, Org: company.Org, Plan: plan, Sink: in.Sink,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	return res.Findings(), nil
}

// githubPass adapts github.Reconcile to the pass contract.
type githubPass struct{ engine *Engine }

func (*githubPass) Kind() integration.Kind { return integration.KindGitHub }

// Needs is nil: GitHub issues nothing on a provisioner's behalf, so this pass
// asks for no transient administrator credential. What it writes at the
// third-party app, it writes with the organization token already in the config.
func (*githubPass) Needs() *setup.Requirement { return nil }

func (p *githubPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitHub
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	client, err := githubReconcileClient(cfg, env)
	if err != nil {
		return nil, fmt.Errorf("engine: github pass: %w", err)
	}
	res, err := github.Reconcile(ctx, github.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		// THE TWO THE LOOP WITHHOLDS. A base is permission to register,
		// a sink is permission to mint, and a person asked for both.
		Sink:             in.Sink,
		WebhookBase:      in.WebhookBase,
		RecreateWebhooks: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: github pass: %w", err)
	}
	findings := res.Findings()

	// EACH AGENT'S OWN APP, which is where identity on GitHub actually
	// lives. The pass above reads the ORGANIZATION and registers hooks; it
	// knows nothing about the app a person created for one seat. This half
	// adopts the installation that person made, and it has to run on the
	// loop rather than at connect time because installing an app is a click
	// in a browser that tells the engine nothing.
	seats := p.seatApps(env)
	if len(seats) > 0 {
		apiBase, webBase := cfg.Bases(env.LookupOK)
		apps, appsErr := github.ReconcileSeatApps(ctx, github.SeatAppOptions{
			APIBase: apiBase,
			WebBase: webBase,
			Org:     githubOrg(cfg),
			Seats:   seats,
			// A DRY RUN RECORDS NOTHING, which is what a check is: the
			// sink is the loop's permission to write, and without it the
			// pass reads and reports.
			Record: p.recordInstallation(in),
			Forget: p.forgetApp(in),
		})
		if appsErr != nil {
			return nil, fmt.Errorf("engine: github pass: %w", appsErr)
		}
		findings = append(findings, apps.Findings...)
	}
	return findings, nil
}

// seatApps reads every agent's own app out of the CURRENT company document.
//
// Through the engine's own reader, which the parser wiring uses as well: the
// reconcile asks what needs doing and the participant lookup asks whose
// credential can read a thread, and two readings of one roster would be free
// to disagree about which apps exist.
func (p *githubPass) seatApps(env *config.Resolver) []github.SeatApp {
	// THE LIVE EPOCH, which is what a pass wants: it converges the company
	// as it is now, where the apply path wires the revision it is about to
	// publish. See [Engine.githubSeatApps].
	return p.engine.githubSeatApps(p.engine.Company(), env)
}

// recordInstallation writes what the pass discovered back onto the seat.
//
// NIL ON A DRY RUN, which is what makes a check read-only: the sink is the
// loop's permission to write, and a check that adopted an installation would
// change the company from a button labelled as a read.
func (p *githubPass) recordInstallation(in setup.PassInput) func(context.Context, string, int64) error {
	if in.Sink == nil {
		return nil
	}
	return func(ctx context.Context, handle string, installationID int64) error {
		return p.engine.RecordGitHubInstallation(ctx, handle, installationID)
	}
}

// forgetApp clears a seat's stale app record, on the same terms as
// [githubPass.recordInstallation]: nil on a dry run, because the sink is the
// loop's permission to write and a check reads and reports.
func (p *githubPass) forgetApp(in setup.PassInput) func(context.Context, string) error {
	if in.Sink == nil {
		return nil
	}
	return func(ctx context.Context, handle string) error {
		return p.engine.ForgetGitHubApp(ctx, handle)
	}
}

// githubOrg is the organization these apps are installed on.
func githubOrg(cfg *config.GitHub) string {
	if cfg == nil || cfg.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Provisioning.Org)
}

// EVERY PASS THIS BUILD SERVES CAN ALSO BE TORN DOWN, asserted at compile
// time rather than discovered when somebody presses Disconnect.
//
// [setup.Teardowner] is an OPTIONAL interface, which is what lets a third-party app
// that registers nothing decline it. That flexibility is also how a third-party app
// silently loses its teardown: rename the method, change its signature, and
// the type simply stops satisfying the interface, with nothing to say so
// until a disconnect reports there is nothing to remove and leaves a live
// webhook behind. These assertions are the thing that says so.
var (
	_ setup.Teardowner = (*jiraPass)(nil)
	_ setup.Teardowner = (*confluencePass)(nil)
	_ setup.Teardowner = (*githubPass)(nil)
	_ setup.Teardowner = (*gitlabPass)(nil)
	_ setup.Teardowner = (*mattermostPass)(nil)
	_ setup.Teardowner = (*datadogPass)(nil)
)

// gitlabAdminToken resolves the group Owner credential.
//
// The per-run OVERRIDE first and the document second, which is the order that
// makes a rotation possible: an operator holding a new token can run a pass
// with it before the document carries it, and every other run needs no
// credential in hand at all.
func gitlabAdminToken(cfg *config.GitLab, env *config.Resolver, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if cfg == nil || cfg.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(env.Value(cfg.Provisioning.AdminToken))
}

// mattermostAdminToken resolves the system-administrator credential, in the
// same order and for the same reason.
func mattermostAdminToken(cfg *config.Mattermost, env *config.Resolver, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if cfg == nil || cfg.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(env.Value(cfg.Provisioning.AdminToken))
}
