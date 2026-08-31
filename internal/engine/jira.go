package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
)

// The Atlassian tracker, wired.
//
// Inbound-only, like the other tracker and for the same reason: an agent's
// Jira work happens through its own MCP server under its own credential. The
// engine never transitions an issue or posts a comment on a seat's behalf —
// it only decides which seats an event concerns and tells them.
//
// # Seat identity is the whole integration, and it is DERIVED
//
// A Jira webhook names people by account id, and nothing in the org model
// says which account a seat holds. Without that mapping every event names a
// stranger, the routing gate drops every target, and the integration is
// silently inert.
//
// So the engine asks: it calls /myself with the seat's OWN credential and
// registers whatever account answers. A declared account id beside the token
// would be cheaper and is the wrong shape — a declaration that disagrees
// with the credential is a misroute nothing can detect.

// jiraIdentities remembers which account each seat credential authenticates
// as.
//
// KEYED ON THE CREDENTIAL, which is what makes an apply free: identity is a
// function of the credential, credentials change rarely, and a config
// revision that touched something else must not spend one request per seat
// to re-learn what it already knows. A rotated token is a cache miss and
// costs exactly one request, which is correct — it may well be a different
// account.
//
// The EMAIL is part of the key, not just the token: Jira Cloud authenticates
// base64(email:token), so the same token under a different address is a
// different credential and may well resolve to a different account.
type jiraIdentities struct {
	mu     sync.Mutex
	byCred map[jira.Credential]string
}

// resolve fills in the accounts behind any credentials not already known.
//
// CONCURRENTLY, bounded by the number of distinct credentials. Sequentially
// this is one round trip per seat on the boot path, which on a company of
// thirty seats against a slow instance is thirty timeouts end to end.
//
// A seat whose lookup FAILS is left unresolved rather than failing the boot:
// the instance may be briefly down, and the next apply retries. What that
// costs is that seat's inbound routing until then, which is the honest
// consequence and is reported per seat.
func (j *jiraIdentities) resolve(ctx context.Context, url string, deploy jira.Deployment, creds []jira.Credential) {
	j.mu.Lock()
	if j.byCred == nil {
		j.byCred = map[jira.Credential]string{}
	}
	var missing []jira.Credential
	for _, cred := range creds {
		if _, known := j.byCred[cred]; !known {
			missing = append(missing, cred)
		}
	}
	j.mu.Unlock()
	if len(missing) == 0 {
		return
	}

	found := make([]string, len(missing))
	resolveConcurrently(len(missing), func(i int) {
		cred := missing[i]
		client, err := jira.NewClient(jira.ClientOptions{
			URL: url, Email: cred.Email, Token: cred.Token, Deployment: deploy,
		})
		if err != nil {
			log.WarnContext(ctx, "jira_seat_client_failed", "error", err.Error())
			return
		}
		account, err := client.Me(ctx)
		if err != nil {
			log.WarnContext(ctx, "jira_seat_identity_unresolved", "error", err.Error(),
				"detail", "this seat receives no tracker events until "+
					"the next apply re-resolves it")
			return
		}
		found[i] = account
	})

	j.mu.Lock()
	defer j.mu.Unlock()
	for i, account := range found {
		if account != "" {
			j.byCred[missing[i]] = account
		}
	}
}

// register binds each resolved seat to its account in the given registry.
//
// NO I/O. It takes the registry rather than reading the live one because an
// apply builds a NEW registry from the new company, and a config-derived
// binding has to be rebuilt into it at that moment.
func (j *jiraIdentities) register(reg *notify.Registry, c *Company, env *config.Resolver) int {
	j.mu.Lock()
	known := maps.Clone(j.byCred)
	j.mu.Unlock()

	var registered int
	for seat := range c.Org.AllRoles() {
		cred := jira.CredentialOf(seat, env.Value)
		if !cred.Held() {
			continue
		}
		account := known[cred]
		if account == "" {
			continue
		}
		if err := reg.Register(jira.Backend, account, seat.Handle()); err != nil {
			// Two seats sharing one account, or a seat that is not in
			// this org. Both are faults an operator has to fix, and both
			// are silent otherwise: that account's events go to whichever
			// seat won.
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
		// A reference that did not resolve. The validator already refuses
		// a jira block with neither url nor cloud_id, so this is a
		// missing variable — and starting anyway would point every lookup
		// at "" and fail with a much less useful message.
		return nil, fmt.Errorf(
			"engine: jira: neither url (%q) nor cloud_id (%q) resolved to "+
				"anything", cfg.URL, cfg.CloudID)
	}
	deploy := jira.DeploymentOf(base)

	// THE ORG CREDENTIAL IS OPTIONAL and its absence is a documented
	// degradation rather than a failure: without it an event reaches
	// whoever the payload names — the assignee, anyone mentioned — and
	// only the people merely watching the issue go unheard.
	var watchers jira.Watchers
	if token := strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		client, err := jira.NewClient(jira.ClientOptions{
			URL: base, Email: env.Value(cfg.Email), Token: token,
		})
		if err != nil {
			return nil, fmt.Errorf("engine: jira: %w", err)
		}
		watchers = jira.Lookup{Client: client}
		// NOT VERIFIED at boot. A /myself here would turn an instance
		// that is briefly down into a company that will not start, to
		// learn something the first real lookup learns anyway — and that
		// one degrades instead of refusing.
		//
		// The SEAT credentials below are the opposite case, and the
		// difference is what the request buys: verifying this one buys
		// nothing, while resolving those is the entire integration.
	} else {
		log.WarnContext(ctx, "jira_has_no_org_token",
			"detail", "an issue's watchers cannot be read, so events reach "+
				"its assignee and anyone mentioned and nobody else")
	}

	e.notify.jira.resolve(ctx, base, deploy, jiraSeatCredentials(c, env))
	registered := e.notify.jira.register(e.Registry(), c, env)
	if registered == 0 {
		// Configured with no seat identity is a company mid-setup —
		// `crewlet jira provision` has not run — or an instance that
		// refused every lookup. Logged loudly either way, because the
		// integration is completely inert in this state and nothing else
		// will say so.
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

// reconcileJira rebuilds the tracker for a newly applied epoch.
//
// Two things move on an apply, and only one of them is the parser. The seat
// identities are rebuilt into the new registry by refreshParties, from the
// cache, with no I/O; this re-resolves — picking up a seat the revision
// ADDED and a credential it ROTATED — and swaps the parser, whose own
// company-derived inputs are the org credential and the lead map. A node
// that skipped it would keep routing a renamed project to its old lead.
func (e *Engine) reconcileJira(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.Jira
	if cfg == nil {
		return
	}
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	parser, err := e.startJira(ctx, c, cfg)
	if err != nil || parser == nil {
		// THE PREVIOUS PARSER KEEPS RUNNING, same posture as the other
		// two: routing by a stale credential is worse than the new one
		// and much better than not routing at all.
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

// jiraBaseURL is the REST base, resolved.
//
// The config's own BaseURL() prefers the cloud gateway, and this resolves
// each half's `${VAR}` first — a cloud id is as much a deployment-specific
// value as a hostname, and companies do reference it.
func jiraBaseURL(cfg *config.Jira, env *config.Resolver) string {
	resolved := config.Jira{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		CloudID: strings.TrimSpace(env.Value(cfg.CloudID)),
	}
	return resolved.BaseURL()
}

// jiraShareableURL is the base a person's browser can open.
//
// EMPTY for a cloud id with no site url, and deliberately: the API gateway
// is not a place a browser goes, so a link built from it looks right and
// opens nothing. The prompt omits the link rather than printing a dead one.
func jiraShareableURL(cfg *config.Jira, env *config.Resolver) string {
	resolved := config.Jira{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		SiteURL: strings.TrimSpace(env.Value(cfg.SiteURL)),
	}
	return resolved.ShareableBaseURL()
}

// jiraSeatCredentials are the distinct credentials the company's agent seats
// hold.
//
// DISTINCT because several seats may legitimately share one — a company
// mid-migration, or one that has not provisioned per-seat accounts yet — and
// resolving the same credential once per seat would spend N requests to
// learn one answer.
func jiraSeatCredentials(c *Company, env *config.Resolver) []jira.Credential {
	held := map[jira.Credential]bool{}
	for seat := range c.Org.AllRoles() {
		if cred := jira.CredentialOf(seat, env.Value); cred.Held() {
			held[cred] = true
		}
	}
	// Sorted so a boot's log lines are diffable against the next one's.
	creds := slices.Collect(maps.Keys(held))
	slices.SortFunc(creds, func(a, b jira.Credential) int {
		if a.Token != b.Token {
			return strings.Compare(a.Token, b.Token)
		}
		return strings.Compare(a.Email, b.Email)
	})
	return creds
}

// jiraPrompt is the tracker's trigger builder. A value, held by nothing.
func jiraPrompt() notify.Prompt { return jira.Prompt{} }
