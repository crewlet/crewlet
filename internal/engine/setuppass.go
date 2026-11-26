package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"

	"github.com/crewlet/crewlet/internal/atlassian"
	// Atlassian's per-seat access registers from its own package, because
	// jira imports atlassian.
	_ "github.com/crewlet/crewlet/internal/atlassian/agentaccess"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
)

// Provisioning a third-party app from the API. The reconcile loop runs each
// pass with no sink and no webhook base, since minting a credential and
// registering a hook are not a timer's decisions; a pass here supplies both
// because a person asked. The pass function is the loop's own: an adapter
// only resolves the config, builds the client and hands over the two.

// setupPasses are the third-party apps this build can provision over the API.
//
// GitLab and Mattermost create an account per agent, which outlives the run
// and is visible company-wide, so they run only on the admin token the
// company document holds ([gitlab.AdminCredential]) and the pass never
// deletes: decommissioning is the command line's, since a company mid-edit
// looks exactly like one that removed a seat. Mattermost registers no hook
// (an outbound websocket per seat). The per-agent identities of Slack (app
// manifest, D11; the install stays a person's click), Cloudflare, Scaleway,
// Google Cloud and SSH are the access sync's alone.
func (e *Engine) setupPasses() []setup.Pass {
	return []setup.Pass{
		&slackPass{engine: e},
		&githubPass{engine: e},
		&jiraPass{engine: e},
		&confluencePass{engine: e},
		&gitlabPass{engine: e},
		&mattermostPass{engine: e},
		&datadogPass{engine: e},
		&cloudflarePass{engine: e},
		&scalewayPass{engine: e},
		&expoPass{engine: e},
		&digitaloceanPass{engine: e},
		&awsPass{engine: e},
		&googleCloudPass{engine: e},
		&sshPass{engine: e},
		&metaAdsPass{engine: e},
		&appStoreConnectPass{engine: e},
		&appleAdsPass{engine: e},
		&googleGrowthPass{engine: e, kind: integration.KindGoogleAnalytics, name: "Google Analytics"},
		&googleGrowthPass{engine: e, kind: integration.KindGoogleAds, name: "Google Ads"},
		&adMobPass{engine: e},
		&adSensePass{engine: e},
		&googleGrowthPass{engine: e, kind: integration.KindSearchConsole, name: "Search Console"},
		&googleGrowthPass{engine: e, kind: integration.KindGooglePlay, name: "Google Play"},
		&atlassianPass{engine: e},
	}
}

// slackPass runs the access sync for every agent's own Slack app. The company
// has nothing at Slack to converge (each app's Request URL is typed by a
// person, [integration.IngressOperator]), so its company half answers only
// whether the company runs Slack.
type slackPass struct{ engine *Engine }

func (*slackPass) Kind() integration.Kind { return integration.KindSlack }

// Needs is nil: the app-configuration token is held in the company document.
func (*slackPass) Needs() *setup.Requirement { return nil }

// Run is the agents' own access ([Engine.withSeatAccess]).
func (p *slackPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *slackPass) run(context.Context, setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	if company == nil || company.Config.Integrations.Slack == nil {
		return nil, integration.ErrNotConfigured
	}
	return nil, nil
}

// atlassianPass creates one Atlassian service account per agent. It is its
// own pass because it authenticates as the ORGANIZATION, the only place an
// identity can be created, where Jira and Confluence authenticate as an
// account on a site.
type atlassianPass struct{ engine *Engine }

func (*atlassianPass) Kind() integration.Kind { return integration.KindAtlassian }

// Needs is nil: the organization key is held in the company document like
// every other credential.
func (*atlassianPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *atlassianPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *atlassianPass) run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Atlassian
	if cfg == nil {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	key := strings.TrimSpace(env.Value(cfg.APIKey))
	// Through the resolver like the key: a sealed org id would otherwise
	// reach Atlassian as the literal "${ATLASSIAN_ORG_ID}", refused with no
	// cause named.
	org := strings.TrimSpace(env.Value(cfg.OrgID))
	if org == "" || key == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Atlassian organization id and its API key did not both " +
				"resolve, and the admin APIs need the organization as the " +
				"subject and the key as the authority",
		}}, nil
	}
	// NO SEATS: every agent's account is the access sync's, which runs after
	// this ([Engine.withSeatAccess]); this converges the company's half.
	res, err := atlassian.Reconcile(ctx, atlassian.Options{
		Client: atlassian.NewClient(atlassian.ClientOptions{}),
		OrgID:  org, Key: key,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: atlassian pass: %w", err)
	}
	return p.recordSite(ctx, company, res.Site, in.Sink != nil), nil
}

// recordSite writes the site this pass discovered (the org key lists it) into
// the Jira and Confluence blocks naming no address, or says why it could not.
// Only a writing pass, one handed a sink, records: the dashboard's read-only
// check must not advance the config epoch. The cloud ID is load-bearing: a
// provisioned service account's token is accepted only at the API gateway, and
// a Forge token is bound to it. A value the company set itself is left alone,
// so a block naming its site by url is told what the Forge relay needs.
func (p *atlassianPass) recordSite(
	ctx context.Context, company *Company, site atlassian.Site, writing bool,
) []integration.Finding {
	in := company.Config.Integrations
	patch := sitePatch(in, site)
	var findings []integration.Finding
	recorded := map[string]bool{}
	if writing && len(patch) > 0 {
		if note := p.applySite(ctx, patch, site); note != "" {
			findings = append(findings, integration.Finding{
				Kind: integration.FindingGrantShort, Detail: note,
			})
		} else {
			for product := range patch {
				recorded[product] = true
			}
		}
	}
	return append(findings, forgeHolds(in, p.engine.Resolve, recorded)...)
}

// forgeHolds is a finding for each product whose Forge relay events are
// refused for want of a cloud id, except one this pass has just recorded.
func forgeHolds(in config.Integrations, resolve func(string) string, recorded map[string]bool) []integration.Finding {
	_, held := in.ForgeBinding(resolve)
	var findings []integration.Finding
	for _, product := range held {
		if !recorded[product] {
			findings = append(findings, integration.Finding{
				Kind: integration.FindingIngressBlocked, Detail: config.ForgeHoldReason(product),
			})
		}
	}
	return findings
}

// sitePatch is the cloud id and site address for each product block naming
// no address of its own; empty when the site is unknown or every block names
// one. A block naming a url is never given a cloud id beside it, which config
// refuses as two names for one instance.
func sitePatch(in config.Integrations, site atlassian.Site) map[string]map[string]string {
	patch := map[string]map[string]string{}
	if site.CloudID == "" {
		return patch
	}
	if j := in.Jira; j != nil && j.CloudID == "" && j.URL == "" {
		patch["jira"] = map[string]string{"cloud_id": site.CloudID, "site_url": site.HostURL}
	}
	if c := in.Confluence; c != nil && c.CloudID == "" && c.URL == "" {
		patch["confluence"] = map[string]string{
			"cloud_id": site.CloudID, "site_url": site.HostURL + "/wiki",
		}
	}
	return patch
}

// applySite writes patch into the company document, or says why it could not.
func (p *atlassianPass) applySite(
	ctx context.Context, patch map[string]map[string]string, site atlassian.Site,
) string {
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
	if err := writer.Apply(ctx, body, "record the Atlassian site", loopActor()); err != nil {
		return "the Atlassian site could not be recorded: " + err.Error()
	}
	return ""
}

// NO TEARDOWN for Atlassian: it registers no webhook (Cloud events arrive
// through the Forge relay), and every agent's account is the disconnect's
// access sync's ([Engine.tearDownSeatAccess]).

// SetupRunner is the pass runner this node serves, built once
// ([Engine.setupRunner]), or nil on an Engine with no fleet store (a test's
// hand-built one; [New] refuses that). A second runner in the engine would be
// a second in-process claim map, leaving the loop and the dashboard unguarded
// against each other; a caller wanting its own clock uses [setup.NewRunner].
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

// holdSurface takes the guard an operator's pass takes in
// [setup.Runner.Execute] (the in-process claim and the fleet lease) for the
// writers that bypass it: the reconcile worker around a whole visit, and a
// disconnect's teardown. So all three exclude each other.
//
// Work must run on the returned context: it carries [setup.PassDeadline],
// strictly inside [setup.LeaseTTL], and the lease is never renewed mid-pass.
// With no runner ([Engine.SetupRunner]) held is true, the deadline still
// applies, and release is the cancel.
func (e *Engine) holdSurface(
	ctx context.Context, kind integration.Kind,
) (context.Context, func(), bool, error) {
	// A nil runner takes [setup.Runner.Hold]'s nil-receiver branch: the rule
	// for an unguarded hold lives with the guard.
	return e.SetupRunner().Hold(ctx, kind)
}

// setupDutyName is the ONE lease name a surface's provisioning is serialized
// under, by the dashboard's pass and the reconcile loop alike: leases are
// keyed by name, so two spellings would be two locks excluding nothing. The
// loop's singleton name stays separate; it answers which node runs the loop,
// not who is writing at this surface.
func setupDutyName(kind integration.Kind) string {
	return "setup-provision-" + string(kind)
}

// setupDuty is the fleet lease held while anything writes at one integration:
// an operator's pass, a reconcile tick or a disconnect. Two overlapping could
// create an account the other is deleting, or remove a webhook being
// registered. The TTL is a backstop for a node that died mid-run, not the
// work's deadline; the lease is given back when the work ends.
func (e *Engine) setupDuty(kind integration.Kind) setup.Duty {
	hold := e.workerHold(setupDutyName(kind), setup.LeaseTTL)
	if hold == nil {
		return nil
	}
	return setup.Duty(hold)
}

// Teardown removes the webhooks this pass registered, at organization and
// repository level, and with RemoveSeats uninstalls each seat's App.
//
// It reports no removed account: GitHub has no API to delete an App, so the
// registration and its `private_key` stay valid, and naming that key in a
// [provision.Removed] would delete a company's only copy of a working key
// (as [Engine.ForgetGitHubApp] says). The disconnect route reports the sealed
// values off the seat roster instead. It does clear `installation_id`: a
// record naming a removed installation reads as a finished agent and keeps
// the integrations row alive on an app that reaches nothing.
func (p *githubPass) Teardown(ctx context.Context, in setup.TeardownInput) (provision.Removed, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitHub
	if cfg == nil {
		return provision.Removed{}, nil
	}
	env := p.engine.resolver()
	client, err := githubReconcileClient(cfg, env)
	if err != nil {
		return provision.Removed{}, fmt.Errorf("engine: github teardown: %w", err)
	}
	if err := github.Teardown(ctx, github.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		WebhookBase: p.engine.webhookBase,
	}); err != nil {
		return provision.Removed{}, err
	}
	if !in.RemoveSeats {
		return provision.Removed{}, nil
	}

	// Uninstalling revokes access, the half the engine can do; deleting the
	// registration is the operator's, in GitHub's settings page. Every seat is
	// attempted and the failures joined, so one seat with a lost key does not
	// leave the rest installed.
	apiBase, _ := cfg.Bases(env.LookupOK)
	var failures []error
	for _, seat := range p.seatApps(env) {
		if err := github.UninstallSeat(ctx, github.SeatAppOptions{
			APIBase: apiBase,
		}, seat); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", seat.Handle, err))
			continue
		}
		// The record follows: zero is what [Engine.RecordGitHubInstallation]
		// holds for a removed installation. A failure fails the teardown, so
		// the surface holds in PhaseDisconnecting and the retry rewrites it;
		// swallowed, the block goes with seats claiming a gone installation
		// that nothing ever re-checks.
		if err := p.engine.RecordGitHubInstallation(ctx, seat.Handle, 0); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", seat.Handle, err))
		}
	}
	if len(failures) > 0 {
		return provision.Removed{}, fmt.Errorf(
			"engine: github teardown: some agents' apps are still installed, or "+
				"still recorded as installed, so the block stays until they are "+
				"not: %w",
			errors.Join(failures...))
	}
	return provision.Removed{}, nil
}

// NO TEARDOWN for Mattermost: it has no inbound registration to withdraw, and
// every agent's bot is turned off, or removed when asked, by the disconnect's
// access sync ([Engine.tearDownSeatAccess]) from the account ledger.

// datadogPass adapts datadog.Reconcile to the pass contract.
type datadogPass struct{ engine *Engine }

func (*datadogPass) Kind() integration.Kind { return integration.KindDatadog }

// Needs is nil: the organization keys are held in the company document like
// every other credential.
func (*datadogPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *datadogPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *datadogPass) run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Datadog
	// Disabled is not configured, as for gitlab, github and mattermost:
	// `enabled: false` is the gesture after a leaked webhook token, and
	// `Datadog.validate` does not check a disabled block, so its fields may
	// be incoherent.
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	if cfg.Provisioning == nil {
		// Config refuses an enabled block without provisioning, whose keys
		// register the webhook alerts arrive by; a document that got here
		// anyway is reported as that finding, never as ready.
		return []integration.Finding{{
			Kind:    integration.FindingCredentialMissing,
			Subject: "integrations.datadog.provisioning",
			Detail: "integrations.datadog.provisioning is unset, so nothing can " +
				"register the webhook alerts arrive by or make the agents' accounts",
		}}, nil
	}
	env := p.engine.resolver()
	// Asked before building: an unknown region is a finding about the
	// document, not a fault.
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
	// NO SEATS: every agent's account is the access sync's, which runs after
	// this ([Engine.withSeatAccess]); this converges the company's half.
	managed, err := p.engine.datadogManaged(ctx, company, cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: datadog pass: %w", err)
	}
	res, err := datadog.Reconcile(ctx, datadog.Options{
		Client: client, Config: cfg, Creds: creds, ReadOnly: in.Sink == nil,
		// THE AGENTS' ACCOUNTS, which the orphan advisory spares: the
		// ledger's, and the address every document seat's would have.
		Managed: managed,
		// Supplying the base is the permission to register the webhook; the
		// token is what the route it posts to accepts.
		WebhookBase:    in.WebhookBase,
		WebhookBaseKey: in.WebhookBaseKey,
		WebhookToken:   strings.TrimSpace(env.Value(cfg.WebhookToken)),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: datadog pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown withdraws the webhook this pass registered.
func (p *datadogPass) Teardown(ctx context.Context, in setup.TeardownInput) (provision.Removed, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Datadog
	if cfg == nil || cfg.Provisioning == nil {
		return provision.Removed{}, nil
	}
	env := p.engine.resolver()
	client, err := datadog.NewClient(datadog.ClientOptions{Site: cfg.Provisioning.Site})
	if err != nil {
		return provision.Removed{}, fmt.Errorf("engine: datadog teardown: %w", err)
	}
	// THE WEBHOOK ONLY: every agent's account is turned off, or removed
	// when asked, by the disconnect's access sync before this runs
	// ([Engine.tearDownSeatAccess]).
	err = datadog.Teardown(ctx, datadog.TeardownOptions{
		Client: client, Config: cfg,
		Creds: datadog.Credentials{
			APIKey: strings.TrimSpace(env.Value(cfg.Provisioning.APIKey)),
			AppKey: strings.TrimSpace(env.Value(cfg.Provisioning.AppKey)),
		},
		// Proves the definition is this deployment's: a webhook is addressed
		// by name, and the teardown refuses one pointing anywhere else.
		WebhookBase: p.engine.webhookBase,
	})
	if err != nil {
		return provision.Removed{}, fmt.Errorf("engine: datadog teardown: %w", err)
	}
	return provision.Removed{}, nil
}

// datadogManaged is every Datadog account the access sync manages, which the
// company-level pass's orphan advisory spares: each account the ledger
// records, and the address every document seat's account is created under.
func (e *Engine) datadogManaged(
	ctx context.Context, company *Company, cfg *config.Datadog,
) ([]string, error) {
	held, err := e.seatAccessAccounts(ctx, integration.KindDatadog)
	if err != nil {
		return nil, err
	}
	out := slices.Collect(maps.Values(held))
	for _, seat := range seataccess.SeatsOf(company.Config) {
		out = append(out, datadog.AccountEmail(cfg.Provisioning, seat.Handle))
	}
	slices.Sort(out)
	return out, nil
}

// cloudflarePass adapts cloudflare.Reconcile to the pass contract: the
// provisioner token and the account it mints in, then each agent's own token.
//
// NO TEARDOWN: the engine puts nothing company-wide at Cloudflare. Every
// agent's token is deleted by the disconnect's access sync
// ([Engine.tearDownSeatAccess]), from the ledger.
type cloudflarePass struct{ engine *Engine }

func (*cloudflarePass) Kind() integration.Kind { return integration.KindCloudflare }

// Needs is nil: the provisioner token is held in the company document.
func (*cloudflarePass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access
// ([Engine.withSeatAccessReads]): the inspection the company half made is
// handed to the agents' session, so the account's zones and permission
// groups are read once a pass.
func (p *cloudflarePass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccessReads(ctx, p.Kind(), in, p.run)
}

func (p *cloudflarePass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, seataccess.PassReads, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Cloudflare
	if cfg == nil || !cfg.Enabled {
		return nil, nil, integration.ErrNotConfigured
	}
	if cfg.Provisioning == nil {
		// Config validation refuses this shape; a document that reached
		// here anyway is reported as the finding it is.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: "integrations.cloudflare.provisioning",
			Detail: "integrations.cloudflare.provisioning is unset, so no agent's token can be minted",
		}}, nil, nil
	}
	env := p.engine.resolver()
	token := strings.TrimSpace(env.Value(cfg.Provisioning.APIToken))
	if token == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: "integrations.cloudflare.provisioning.api_token",
			Detail: "the Cloudflare provisioner token did not resolve, so no agent's token can be minted",
		}}, nil, nil
	}
	client, err := cloudflare.NewClient(cloudflare.ClientOptions{
		AccountID: env.Value(cfg.Provisioning.AccountID), Token: token,
	})
	if err != nil {
		// THE ACCOUNT ID, the one input left that NewClient refuses: a
		// fact about the document, not a pass that failed to look.
		//nolint:nilerr // Deliberate: reported as a finding on the document.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: "integrations.cloudflare.provisioning.account_id",
			Detail: "integrations.cloudflare.provisioning.account_id does not resolve to a Cloudflare account ID",
		}}, nil, nil
	}
	// THE AGENTS' TOKENS, which the orphan advisory spares: the ledger's.
	held, err := p.engine.seatAccessAccounts(ctx, integration.KindCloudflare)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: cloudflare pass: %w", err)
	}
	res, err := cloudflare.Reconcile(ctx, cloudflare.Options{
		Client: client, Config: cfg, Company: company.Config.Name,
		Managed: slices.Collect(maps.Values(held)),
		InUse:   seataccess.LevelsInUse(company.Config, seataccess.LaneCloudflare),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("engine: cloudflare pass: %w", err)
	}
	return res.Findings(), res.Inspection, nil
}

// SetupSink is the recorder a pass writes minted credentials through: the
// same write-through sink `crewlet <integration> provision -secret-store`
// builds, so a value is durable before the next is minted.
//
// by is who the run is for (the person who pressed Connect, or [loopActor]
// for a timer's pass), recorded on every sealed row and on the revision the
// rebuild writes. It errs only for an engine with no company secret store,
// which [New] never builds: a caller reports that as a wiring fault, never
// carries on with nothing sealed.
func (e *Engine) SetupSink(by iam.Actor) (provision.TokenSink, error) {
	if e.cipher == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil, errors.New("engine: this engine holds no company secret " +
			"store to seal a minted credential into; it was not built by engine.New")
	}
	return &refreshingSink{
		TokenSink: provision.NewSecretStoreSink(
			fleetsecrets.New(e.backends.Fleet, e.cipher), secrets.Author{
				Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID,
			}),
		engine: e,
		by:     by,
	}, nil
}

// refreshingSink republishes what a run sealed, so the company runs on it.
//
// `${VAR}` resolves from a snapshot taken at apply time and minting advances
// no epoch, so without this a sealed value stays unresolved and the next
// tick mints again, for ever. After each Flush that wrote, it refreshes the
// snapshot (the value becomes resolvable), passes this node's held seats over
// their tool servers ([Engine.passHeldSeats]), the one rebuild a node can do
// alone, and requests a re-activation so everything that already resolved
// the old value is rebuilt ([Engine.rebuildForSealedSecrets]). After Flush,
// not per Record: a run rolled back leaves the snapshot where it was.
type refreshingSink struct {
	provision.TokenSink
	engine *Engine

	// by is who this run is for, so the revision the rebuild writes is
	// credited to them rather than the reconcile loop; the sink stamps the
	// same party on every secret record.
	by iam.Actor

	// changed is whether a value was sealed or cleared since the last Flush,
	// not since the run began: a pass flushes twice (its company work, then
	// its seats' access sync), and the second's seals must refresh too.
	changed bool
}

func (s *refreshingSink) Record(ctx context.Context, name, value string) error {
	if err := s.TokenSink.Record(ctx, name, value); err != nil {
		return err
	}
	s.changed = true
	return nil
}

// Forget implements [provision.TokenSink], and marks the snapshot stale: a
// value cleared from the store still resolves from the snapshot until it is
// refreshed, and a seat turned off would go on starting its tool with the
// credential its off revoked.
func (s *refreshingSink) Forget(ctx context.Context, names ...string) error {
	err := s.TokenSink.Forget(ctx, names...)
	if len(names) > 0 {
		s.changed = true
	}
	return err
}

func (s *refreshingSink) Flush(ctx context.Context) error {
	err := s.TokenSink.Flush(ctx)
	// Even on a failed Flush: a write-through sink already made its values
	// durable.
	if s.changed {
		s.changed = false
		// This node's held seats first, without waiting on a re-activation
		// that can fail; only when a snapshot was actually installed.
		if s.engine.refreshSecrets(ctx) {
			s.engine.passHeldSeats(ctx)
		}
		// No context on purpose: ctx is the pass's, bounded by
		// [setup.PassDeadline] inside the surface lease, while the
		// re-activation outlives the pass and needs no lease.
		// [republisher.startNow] runs it on a background context that
		// [Engine.rebuildForSealedSecrets] bounds itself.
		//nolint:contextcheck // detached by design; see [republisher.request]
		s.engine.republish.request(s.by, s.engine.rebuildForSealedSecrets)
	}
	return err
}

// rebuildForSealedSecrets re-activates the current revision so everything
// built at apply time (seat identities, parsers, transports, provider
// clients, MCP children; the vendor reconcilers run only in the apply) is
// rebuilt against credentials a pass just sealed. A snapshot refresh reaches
// only what resolves from then on.
//
// Re-activating, not re-running the reconcilers here: every node built its
// own wiring, and moving the append-only activation pointer to an unchanged
// revision (the control plane's credential-rotation gesture) is what every
// node watches. Requests are coalesced by [republisher]: an apply brings a
// pass forward, and a pass that never converges would otherwise seal, and so
// rebuild, as fast as an apply completes.
func (e *Engine) rebuildForSealedSecrets(ctx context.Context, by iam.Actor) {
	writer := e.configWriterOrNil()
	if writer == nil {
		// No config surface (a worker-only node, or before the API is
		// wired): the values are durable and resolvable, and the next apply
		// from anywhere rebuilds.
		log.WarnContext(ctx, "sealed_credentials_not_republished",
			"detail", "this node cannot re-activate the revision, so anything "+
				"built at the last apply but this node's seat tool servers keeps "+
				"the credential it resolved then; POST /config/reload once a node "+
				"with the config surface is up")
		return
	}
	// Detached and bounded: the pass's context may be all but spent and is
	// cancelled when the pass ends, which would leave the stale wiring.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), setup.RecordDeadline)
	defer cancel()
	if by.Name == "" {
		by = loopActor()
	}
	if err := writer.Reload(ctx,
		"reload after provisioning sealed a credential", by); err != nil {
		log.ErrorContext(ctx, "sealed_credentials_not_republished", "error", err,
			"detail", "the credentials are sealed and resolvable, and this node's "+
				"seat tool servers are brought onto them, but the seat "+
				"identities, parsers and transports built at the last apply — and "+
				"every other node's tool servers — still hold what they resolved "+
				"then; POST /config/reload")
		return
	}
	log.InfoContext(ctx, "sealed_credentials_republished",
		"detail", "the revision was re-activated so every node rebuilds against "+
			"the credentials this pass sealed")
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
		// A finding, as the loop reports it: a pass that cannot reach the
		// instance observed nothing, and a fault would send an operator
		// hunting an outage.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Jira site address or the org token did not resolve, so " +
				"this pass could not reach the instance",
		}}, nil
	}
	client, err := jira.NewClient(jira.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
		Declared: company.Config.Integrations.Atlassian.Declared(),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira pass: %w", err)
	}
	res, err := jira.Reconcile(ctx, jira.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		Sink: in.Sink, WebhookBase: in.WebhookBase, WebhookBaseKey: in.WebhookBaseKey,
		RecreateWebhook: in.Recreate,
		// The credential's age alone separates "Atlassian is still
		// granting this new account" from "this credential is wrong":
		// both are the same 401.
		CredentialSealed: p.engine.atlassianCredentialSealed(company),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown removes the webhook this pass registered, through the function a
// command-line decommission calls. Its Removed is always empty: an agent
// works in Jira as the Atlassian account [atlassianPass] made, so a Jira
// teardown strands no credential.
func (p *jiraPass) Teardown(ctx context.Context, in setup.TeardownInput) (provision.Removed, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Jira
	if cfg == nil {
		// The block already left the document, so there is nothing to
		// authenticate with and nothing this engine still holds.
		return provision.Removed{}, nil
	}
	env := p.engine.resolver()
	base := jiraBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		return provision.Removed{}, fmt.Errorf(
			"engine: jira teardown: the site address or the org token did not " +
				"resolve, so the webhook cannot be removed: fix the credential " +
				"or force the disconnect and remove the hook by hand")
	}
	client, err := jira.NewClient(jira.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
		Declared: company.Config.Integrations.Atlassian.Declared(),
	})
	if err != nil {
		return provision.Removed{}, fmt.Errorf("engine: jira teardown: %w", err)
	}
	return provision.Removed{}, jira.Teardown(ctx, jira.Options{
		Client: client, Config: cfg,
		WebhookBase: p.engine.webhookBase,
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
		Declared: company.Config.Integrations.Atlassian.Declared(),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: confluence pass: %w", err)
	}
	res, err := confluence.Reconcile(ctx, confluence.Options{
		Client: client, Config: cfg, Value: env.Value,
		Sink: in.Sink, WebhookBase: in.WebhookBase, WebhookBaseKey: in.WebhookBaseKey,
		Recreate: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: confluence pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown removes the hooks this pass registered, and strands no credential:
// both Atlassian products authenticate as the service account [atlassianPass]
// made, which the disconnect's access sync removes.
func (p *confluencePass) Teardown(ctx context.Context, in setup.TeardownInput) (provision.Removed, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Confluence
	if cfg == nil {
		return provision.Removed{}, nil
	}
	env := p.engine.resolver()
	base := confluenceBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		return provision.Removed{}, fmt.Errorf(
			"engine: confluence teardown: the site address or the org token did " +
				"not resolve, so the hooks cannot be removed: fix the credential " +
				"or force the disconnect and remove them by hand")
	}
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
		Declared: company.Config.Integrations.Atlassian.Declared(),
	})
	if err != nil {
		return provision.Removed{}, fmt.Errorf("engine: confluence teardown: %w", err)
	}
	return provision.Removed{}, confluence.Teardown(ctx, confluence.Options{Client: client, Config: cfg})
}

// gitlabPass adapts gitlab.Reconcile to the pass contract.
type gitlabPass struct{ engine *Engine }

func (*gitlabPass) Kind() integration.Kind { return integration.KindGitLab }

// Needs is nil: the Owner token is held in the company document like every
// other credential, so there is nothing transient to ask for. See
// [gitlab.AdminCredential] for why it is kept.
func (*gitlabPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *gitlabPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *gitlabPass) run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	admin := gitlabAdminToken(cfg, env, in.Operator)
	if admin == "" {
		// A finding, not a fault: the one credential this pass cannot mint
		// for itself is missing.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "no group Owner token resolved, and the seats' own tokens " +
				"are what this pass mints, so it cannot bootstrap itself from them",
		}}, nil
	}
	// NO SEATS: every agent's account is the access sync's, which runs after
	// this ([Engine.withSeatAccess]); this converges the company's half.
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: env.Value(cfg.URL), Token: admin,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	signingVar, _ := provision.SoleVar(cfg.SigningSecret)
	res, err := gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: cfg, Sink: in.Sink,
		WebhookBase:      in.WebhookBase,
		WebhookBaseKey:   in.WebhookBaseKey,
		SigningSecret:    env.Value(cfg.SigningSecret),
		SigningSecretVar: signingVar,
		// ROTATE ONLY WHEN ASKED: a new signing secret re-points the hook
		// at a key the running engine does not hold yet.
		Rotate: in.Recreate.Asked(),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	return res.Findings(), nil
}

// Teardown withdraws the hooks, as the group Owner token the pass uses
// ([gitlabAdminToken]); the seats' own tokens cannot remove what created them.
func (p *gitlabPass) Teardown(ctx context.Context, in setup.TeardownInput) (provision.Removed, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitLab
	if cfg == nil {
		return provision.Removed{}, nil
	}
	env := p.engine.resolver()
	admin := gitlabAdminToken(cfg, env, in.Operator)
	if admin == "" {
		return provision.Removed{}, fmt.Errorf(
			"engine: gitlab teardown: no group Owner token resolved, and the " +
				"seats' own tokens cannot remove what created them")
	}
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: env.Value(cfg.URL), Token: admin,
	})
	if err != nil {
		return provision.Removed{}, fmt.Errorf("engine: gitlab teardown: %w", err)
	}
	// THE HOOKS ONLY: every agent's account is turned off, or removed when
	// asked, by the disconnect's access sync before this runs
	// ([Engine.tearDownSeatAccess]).
	return gitlab.Teardown(ctx, gitlab.TeardownOptions{
		Client: client, Config: cfg,
	})
}

// mattermostPass adapts mattermost.Reconcile to the pass contract.
type mattermostPass struct{ engine *Engine }

func (*mattermostPass) Kind() integration.Kind { return integration.KindMattermost }

// Needs is nil, for the reason GitLab's is: the administrator token is held
// in the company document.
func (*mattermostPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *mattermostPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *mattermostPass) run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	// The people, as the running transport last found them
	// ([mattermost.Transport.PeopleFindings]), on every reporting return:
	// they are no part of provisioning, so a company with hand-made bots and
	// no admin token still gets them.
	people := p.engine.Mattermost().PeopleFindings()
	env := p.engine.resolver()
	admin := mattermost.AdminToken(cfg, env, in.Operator)
	if admin == "" {
		return append([]integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "no administrator token resolved, and the bots' own tokens " +
				"are what the access sync mints, so it cannot bootstrap itself from them",
		}}, people...), nil
	}
	// Only a company that provisions its bots: a per-run admin token cannot
	// make this pass manage bots nobody asked it to (the setup route refuses
	// such a run too).
	if cfg.Provisioning == nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", integration.ErrNotConfigured)
	}
	// NO SEATS: every agent's account is the access sync's, which runs after
	// this ([Engine.withSeatAccess]); this converges the company's half.
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: env.Value(cfg.URL), Token: admin,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	// The company's half: whether the instance lets this engine provision,
	// and each bot's display name. No webhook base: it registers nothing.
	managed, err := p.engine.seatAccessAccounts(ctx, integration.KindMattermost)
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	res, err := mattermost.Reconcile(ctx, mattermost.Options{
		Client: client, Config: cfg, Org: company.Org, ReadOnly: in.Sink == nil,
		// THE AGENTS' BOTS, the only ones the pass renames: those the
		// ledger records, so a bot somebody else made under a seat's
		// username keeps its name.
		Managed: managed,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	return append(res.Findings(), people...), nil
}

// githubPass adapts github.Reconcile to the pass contract.
type githubPass struct{ engine *Engine }

func (*githubPass) Kind() integration.Kind { return integration.KindGitHub }

// Needs is nil: the pass writes at GitHub with the organization token the
// config already holds.
func (*githubPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *githubPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *githubPass) run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
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
	// Each agent's own app, read first: every app carries its own webhook,
	// so the organization pass judges coverage by whether any exist
	// ([github.Options.SeatApps]).
	seats := p.seatApps(env)
	res, err := github.Reconcile(ctx, github.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		SeatApps: len(seats),
		// THE TWO THE LOOP WITHHOLDS. A base is permission to register,
		// a sink is permission to mint, and a person asked for both.
		Sink:             in.Sink,
		WebhookBase:      in.WebhookBase,
		WebhookBaseKey:   in.WebhookBaseKey,
		RecreateWebhooks: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: github pass: %w", err)
	}
	// Keep each agent's app in step with GitHub on the loop, since installing
	// an app is a browser click that tells the engine nothing: record a new
	// installation, clear a removed one, forget a deleted app (the token
	// issuer reads these). Off seats too, so a disconnect finds their
	// suspended installations. What an agent may do, and every finding about
	// it, is the access sync's ([Engine.withSeatAccess]); no app is off (D9).
	if len(seats) > 0 {
		apiBase, webBase := cfg.Bases(env.LookupOK)
		if err := github.AdoptSeatApps(ctx, github.SeatAppOptions{
			APIBase: apiBase,
			WebBase: webBase,
			Org:     githubOrg(cfg),
			Seats:   seats,
			// A dry run (no sink) records nothing.
			Record: p.recordInstallation(in),
			Forget: p.forgetApp(in),
		}); err != nil {
			return nil, fmt.Errorf("engine: github pass: %w", err)
		}
	}
	return res.Findings(), nil
}

// seatApps reads every agent's own app from the current company document,
// through the reader the parser wiring uses too, so the two cannot disagree
// about which apps exist.
func (p *githubPass) seatApps(env *config.Resolver) []github.SeatApp {
	// The live epoch: a pass converges the company as it is now, where the
	// apply wires the revision it is about to publish ([Engine.githubSeatApps]).
	return p.engine.githubSeatApps(p.engine.Company(), env)
}

// recordInstallation writes what the pass discovered back onto the seat, or
// is nil on a dry run: the sink is the permission to write, and a check must
// not change the company.
func (p *githubPass) recordInstallation(in setup.PassInput) func(context.Context, string, int64) error {
	if in.Sink == nil {
		return nil
	}
	return func(ctx context.Context, handle string, installationID int64) error {
		return p.engine.RecordGitHubInstallation(ctx, handle, installationID)
	}
}

// forgetApp clears a seat's stale app record, nil on a dry run as
// [githubPass.recordInstallation] is.
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

// The passes that register something stay [setup.Teardowner]s, asserted at
// compile time: the interface is optional, so a renamed or re-signed method
// would silently drop the teardown and a disconnect would leave a live
// webhook behind.
var (
	_ setup.Teardowner = (*jiraPass)(nil)
	_ setup.Teardowner = (*confluencePass)(nil)
	_ setup.Teardowner = (*githubPass)(nil)
	_ setup.Teardowner = (*gitlabPass)(nil)
	_ setup.Teardowner = (*datadogPass)(nil)
	_ setup.Teardowner = (*scalewayPass)(nil)
)

// gitlabAdminToken resolves the group Owner credential: the per-run override
// first, so an operator can rotate to a new token before the document
// carries it, then the document's.
func gitlabAdminToken(cfg *config.GitLab, env *config.Resolver, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if cfg == nil || cfg.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(env.Value(cfg.Provisioning.AdminToken))
}

// atlassianCredentialSealed answers when a seat's Atlassian credential was
// sealed into this company's secret store: a refusal means opposite things
// by the credential's age ([atlassian.GrantPropagation]), and the tracker is
// handed this because internal/jira must not import the coordination layer.
//
// Nil where nothing can say (no derivable plan, no provisioned accounts): a
// refusal is then reported as a failure, since guessing "still propagating"
// would hide every broken seat. The variables come from the plan the
// provisioning mints by ([seatMinter]), so only a seat this engine
// provisions gets a clock; a hand-made account's refusal is real.
func (e *Engine) atlassianCredentialSealed(
	company *Company,
) func(context.Context, string) (time.Time, bool) {
	if e.cipher == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	plan, err := (&atlassianPass{engine: e}).seatPlan(company)
	if err != nil || len(plan.Seats) == 0 {
		// NO SEAT THIS ENGINE PROVISIONS, so no clock: an account made by
		// hand has no grant of this engine's to be waiting on.
		return nil
	}
	vars := make(map[string]string, len(plan.Seats))
	for _, seat := range plan.Seats {
		if seat.TokenVar != "" {
			vars[seat.Handle] = seat.TokenVar
		}
	}
	store := fleetsecrets.New(e.backends.Fleet, e.cipher)
	return func(ctx context.Context, handle string) (time.Time, bool) {
		name, planned := vars[handle]
		if !planned {
			// Not a seat this engine provisions: no grant to wait on.
			return time.Time{}, false
		}
		rec, found, err := store.Describe(ctx, name)
		if err != nil || !found {
			return time.Time{}, false
		}
		return rec.UpdatedAt, true
	}
}
