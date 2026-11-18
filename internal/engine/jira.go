package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
)

// The Atlassian tracker, wired: inbound only, like the other tracker, since an
// agent's Jira work runs through its own MCP server under its own credential;
// the engine only decides which seats an event concerns and tells them.
//
// Seat identity is the whole integration, and it is DERIVED: a webhook names
// people by account id, so the engine calls /myself with each seat's OWN
// credential and registers the account that answers. Without it every event
// names a stranger and the integration is silently inert. A declared account
// id beside the token is wrong: one that disagrees with the credential
// misroutes undetectably.

// jiraIdentities remembers which account each seat credential authenticates
// as. The cache's rules are [identityCache]'s; this adds the client, the
// account id Jira routes by and what a failed lookup logs.
//
// KEYED ON THE CREDENTIAL, email included: Jira Cloud authenticates
// base64(email:token), so the same token under another address may resolve to
// another account.
type jiraIdentities struct {
	identityCache[atlassian.Credential]
}

// resolve fills in the accounts behind any credentials not already known, on
// [identityCache.resolve]'s promise that a caller that waited holds the answer
// when it returns, which register depends on.
func (j *jiraIdentities) resolve(ctx context.Context, url string, declared atlassianhost.Deployment, creds []atlassian.Credential) {
	j.identityCache.resolve(ctx, creds, func(cred atlassian.Credential) string {
		client, err := jira.NewClient(jira.ClientOptions{
			URL: url, Email: cred.Email, Token: cred.Token, Declared: declared,
		})
		if err != nil {
			log.WarnContext(ctx, "jira_seat_client_failed", "error", err.Error())
			return ""
		}
		account, err := client.Me(ctx)
		if err != nil {
			log.WarnContext(ctx, "jira_seat_identity_unresolved", "error", err.Error(),
				"detail", unresolvedSeatDetail("tracker events"))
			return ""
		}
		return account
	})
}

// register binds each resolved seat to its account in reg, with NO I/O. It
// takes the registry rather than reading the live one because an apply builds
// a NEW registry from the new company and must rebuild this binding into it.
func (j *jiraIdentities) register(reg *notify.Registry, c *Company, env *config.Resolver) int {
	known := j.snapshot()

	var registered int
	for seat := range c.Org.AllRoles() {
		cred := atlassian.CredentialOf(atlassian.ProductJira, seat, env.Value)
		if !cred.Held() {
			continue
		}
		account := known[cred]
		if account == "" {
			continue
		}
		if err := reg.Register(jira.Backend, account, seat.Handle()); err != nil {
			// Two seats sharing an account, or a seat not in this org: an
			// operator's fault, silent otherwise (the account's events go
			// to whichever seat won).
			log.Warn("jira_seat_identity_refused", "seat", seat.Handle(),
				"account", account, "error", err.Error())
			continue
		}
		registered++
	}
	return registered
}

// startJira builds the tracker's parser and resolves its seat identities.
func (e *Engine) startJira(ctx context.Context, c *Company, cfg *config.Jira) (*jira.Parser, error) {
	if cfg == nil {
		return nil, nil
	}
	env := e.resolver()
	base := jiraBaseURL(cfg, env)
	if base == "" {
		// A missing variable (the validator refuses a block with neither):
		// starting anyway would fail every lookup at "" with a worse message.
		return nil, fmt.Errorf(
			"engine: jira: neither url (%q) nor cloud_id (%q) resolved to "+
				"anything", cfg.URL, cfg.CloudID)
	}
	declared := c.Config.Integrations.Atlassian.Declared()
	deploy := jira.DeploymentOf(base, declared)

	// THE ORG CREDENTIAL IS OPTIONAL, a documented degradation: without it an
	// event still reaches whoever the payload names (assignee, mentions), and
	// only the issue's watchers go unheard.
	var watchers jira.Watchers
	if token := strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		client, err := jira.NewClient(jira.ClientOptions{
			URL: base, Email: env.Value(cfg.Email), Token: token, Declared: declared,
		})
		if err != nil {
			return nil, fmt.Errorf("engine: jira: %w", err)
		}
		watchers = jira.Lookup{Client: client}
		// NOT VERIFIED at boot: a /myself here would turn a briefly-down
		// instance into a company that will not start, to learn what the
		// first lookup learns anyway, degrading instead. The SEAT credentials
		// below are resolved because they are the whole integration.
	} else {
		log.WarnContext(ctx, "jira_has_no_org_token",
			"detail", "an issue's watchers cannot be read, so events reach "+
				"its assignee and anyone mentioned and nobody else")
	}

	e.notify.jira.resolve(ctx, base, declared, jiraSeatCredentials(c, env))
	registered := e.intoLiveRegistry(func(reg *notify.Registry) int {
		return e.notify.jira.register(reg, c, env)
	})
	if registered == 0 {
		// Setup unfinished (`crewlet jira provision` has not run) or every
		// lookup refused: loud, as the integration is inert and nothing
		// else says so.
		log.WarnContext(ctx, "jira_has_no_seat_identities", "url", base,
			"detail", "every tracker webhook will name a stranger, and every "+
				"issue will fall through to its project's lead")
	}
	leads := jira.LeadsFrom(c.Org)
	log.InfoContext(ctx, "jira_wired", "url", base, "deployment", string(deploy),
		"seat_identities", registered, "projects_with_leads", len(leads),
		"watcher_lookup", watchers != nil)

	return jira.NewParser(jira.ParserOptions{
		URL:      jiraShareableURL(cfg, env),
		Watchers: watchers,
		Leads:    leads,
	}), nil
}

// reconcileJira rebuilds the tracker for a newly applied epoch. refreshParties
// rebuilds the seat identities into the new registry from the cache; this
// re-resolves (a seat ADDED, a credential ROTATED) and swaps the parser, whose
// company-derived inputs are the org credential and the lead map, so a
// renamed project stops routing to its old lead.
func (e *Engine) reconcileJira(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.Jira
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	// RETIRED when the revision no longer declares it: removing the
	// `integrations.jira` block is how an operator answers a credential leak,
	// and a kept parser would route under the revoked credential while
	// RoutedSources listed it as reachable.
	if cfg == nil {
		if svc.Unregister(jira.Backend) {
			log.InfoContext(ctx, "jira_retired",
				"detail", "the revision no longer declares jira; its deliveries "+
					"route to no seat")
		}
		return
	}
	parser, err := e.startJira(ctx, c, cfg)
	if err != nil || parser == nil {
		// THE PREVIOUS PARSER KEEPS RUNNING, as for the other two: a stale
		// credential routes worse than the new one, far better than none.
		log.ErrorContext(ctx, "jira_reconcile_failed", "error", errorText(err),
			"detail", "the previous tracker wiring is still current")
		return
	}
	if err := svc.Replace(parser, jiraPrompt()); err != nil {
		log.ErrorContext(ctx, "jira_reconcile_failed", "error", err.Error(),
			"detail", "the previous tracker wiring is still current")
		return
	}
	log.InfoContext(ctx, "jira_reconciled", "company", c.Config.Name)
}

// jiraBaseURL is the REST base, resolved ([config.Jira.ResolvedBaseURL]).
func jiraBaseURL(cfg *config.Jira, env *config.Resolver) string {
	return cfg.ResolvedBaseURL(env.Value)
}

// jiraShareableURL is the base a person's browser can open: EMPTY for a cloud
// id with no site url, since a link to the API gateway looks right and opens
// nothing, and the prompt omits a link rather than print a dead one.
func jiraShareableURL(cfg *config.Jira, env *config.Resolver) string {
	resolved := config.Jira{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		SiteURL: strings.TrimSpace(env.Value(cfg.SiteURL)),
	}
	return resolved.ShareableBaseURL()
}

// jiraSeatCredentials are the DISTINCT credentials the company's agent seats
// hold: seats may share one (mid-migration, no per-seat accounts yet), and one
// answer should cost one request, not one per seat.
func jiraSeatCredentials(c *Company, env *config.Resolver) []atlassian.Credential {
	held := map[atlassian.Credential]bool{}
	for seat := range c.Org.AllRoles() {
		if cred := atlassian.CredentialOf(atlassian.ProductJira, seat, env.Value); cred.Held() {
			held[cred] = true
		}
	}
	// Sorted so a boot's log lines are diffable against the next one's.
	creds := slices.Collect(maps.Keys(held))
	slices.SortFunc(creds, func(a, b atlassian.Credential) int {
		if a.Token != b.Token {
			return strings.Compare(a.Token, b.Token)
		}
		return strings.Compare(a.Email, b.Email)
	})
	return creds
}

// jiraPrompt is the tracker's trigger builder. A value, held by nothing.
func jiraPrompt() notify.Prompt { return jira.Prompt{} }

// unresolved names the seats holding a tracker credential that resolves to no
// account: THE COMPLEMENT OF [jiraIdentities.register] over the same inputs,
// so the two never disagree. A seat with no credential has opted out, a
// choice rather than a fault, and listing it would put every human seat on
// the card.
func (j *jiraIdentities) unresolved(c *Company, env *config.Resolver) []string {
	known := j.snapshot()

	var out []string
	for seat := range c.Org.AllRoles() {
		cred := atlassian.CredentialOf(atlassian.ProductJira, seat, env.Value)
		if cred.Held() && known[cred] == "" {
			out = append(out, seat.Handle())
		}
	}
	slices.Sort(out)
	return out
}
