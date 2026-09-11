package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provision"
)

// The hosted code host, wired.
//
// Inbound-only, for the same reason the self-hosted one is: an agent's work
// on a code host happens INSIDE its sandbox, under its own identity, through
// its own tools. The engine never pushes a commit or opens a pull request on
// a seat's behalf — it only tells the seat something happened.
//
// # Seat identity is the whole integration, and it is DERIVED
//
// A GitHub delivery names people by login, and nothing in the org model says
// which account a seat holds. Without that mapping every event names a
// stranger, the routing gate drops every target, and the integration is
// silently inert.
//
// So the engine asks: it calls GET /user with the seat's OWN credential and
// registers whatever account answers. A declared login beside the token
// would be cheaper and is the wrong shape — a declaration that disagrees
// with the credential is a misroute nothing can detect, and it would make
// the engine name a tool-specific variable that the seat's actual tools do
// not read.

// githubIdentities remembers which account each seat credential
// authenticates as.
//
// KEYED ON THE TOKEN, which is what makes an apply free: identity is a
// function of the credential, credentials change rarely, and a config
// revision that touched something else must not spend one request per seat
// to re-learn what it already knows. A rotated token is a cache miss and
// costs exactly one request, which is correct — it may well be a different
// account.
type githubIdentities struct {
	mu      sync.Mutex
	byToken map[string]string
}

// resolve fills in the accounts behind any credentials not already known.
//
// CONCURRENTLY and bounded — see [identityLookups]. Sequentially this is one
// round trip per seat on the boot path, which on a company of thirty seats is
// thirty timeouts end to end against a degraded API; unbounded it is thirty
// simultaneous connections to one third-party app, which is the shape an abuse
// detector is built to notice.
//
// A seat whose lookup FAILS is left unresolved rather than failing the boot:
// GitHub may be briefly down or rate-limiting, and the next apply retries.
// What that costs is that seat's inbound routing until then, which is the
// honest consequence and is reported per seat.
func (g *githubIdentities) resolve(ctx context.Context, api, web string, tokens []string) {
	g.mu.Lock()
	if g.byToken == nil {
		g.byToken = map[string]string{}
	}
	var missing []string
	for _, token := range tokens {
		if _, known := g.byToken[token]; !known {
			missing = append(missing, token)
		}
	}
	g.mu.Unlock()
	if len(missing) == 0 {
		return
	}

	found := make([]string, len(missing))
	provision.ResolveConcurrently(len(missing), func(i int) {
		token := missing[i]
		client, err := github.NewClient(github.ClientOptions{
			APIBase: api, WebBase: web, Token: token,
		})
		if err != nil {
			log.WarnContext(ctx, "github_seat_client_failed", "error", err.Error())
			return
		}
		login, err := client.Me(ctx)
		if err != nil {
			log.WarnContext(ctx, "github_seat_identity_unresolved", "error", err.Error(),
				"detail", "this seat receives no code-host events until "+
					"the next apply re-resolves it")
			return
		}
		found[i] = login
	})

	g.mu.Lock()
	defer g.mu.Unlock()
	for i, login := range found {
		if login != "" {
			g.byToken[missing[i]] = login
		}
	}
}

// register binds each resolved seat to its account in the given registry.
//
// NO I/O. It takes the registry rather than reading the live one because an
// apply builds a NEW registry from the new company, and a config-derived
// binding has to be rebuilt into it at that moment. (A chat backend's
// identities are the opposite — facts about a live server, carried across an
// apply by the transport that resolved them.)
func (g *githubIdentities) register(reg *notify.Registry, c *Company, env *config.Resolver) int {
	g.mu.Lock()
	known := maps.Clone(g.byToken)
	g.mu.Unlock()

	var (
		registered int
		byApp      = map[string]bool{}
	)

	// AN AGENT'S OWN APP IS ITS IDENTITY, and it costs no request at all:
	// GitHub derives an app's account from its slug, which the engine wrote
	// down when it created the app. Registered FIRST, because a seat that
	// has one acts as it, and the token below would otherwise overwrite the
	// mapping with whatever account a leftover credential authenticates as.
	//
	// TWO SPELLINGS, one identity. A person writing a mention types the
	// slug; every payload reporting what the app did carries the slug with
	// `[bot]`. Nothing relates them, and the registry is a bijection per
	// namespace, so the bot login goes in the companion namespace that
	// exists for exactly this — the same shape Slack and Mattermost use.
	for role := range c.Config.EachRole() {
		seat := role.Seat()
		app := role.Integrations.GitHub
		if !seat.IsAgent() || app == nil {
			continue
		}
		slug := github.NormalizeLogin(app.AppSlug)
		if slug == "" {
			// The app has not been created yet. The seat is on the
			// roster with a click outstanding, and it acts as nobody
			// until somebody makes it one.
			continue
		}
		handle := seat.Handle()
		if err := reg.Register(github.Backend, slug, handle); err != nil {
			log.Warn("github_seat_identity_refused", "seat", handle,
				"login", slug, "error", err.Error())
			continue
		}
		if bot := github.BotLogin(slug); bot != "" {
			if err := reg.Register(notify.BotNamespace(github.Backend), bot, handle); err != nil {
				// THE MENTION STILL WORKS. What is lost is recognising
				// this agent's own activity in a payload, so it is a
				// warning and not a reason to drop the seat.
				log.Warn("github_seat_bot_identity_refused", "seat", handle,
					"login", bot, "error", err.Error())
			}
		}
		byApp[handle] = true
		registered++
	}

	for seat := range c.Org.AllRoles() {
		if byApp[seat.Handle()] {
			continue
		}
		token := github.CredentialOf(seat, env.Value)
		if token == "" {
			continue
		}
		login := github.NormalizeLogin(known[token])
		if login == "" {
			continue
		}
		if err := reg.Register(github.Backend, login, seat.Handle()); err != nil {
			// Two seats sharing one account, or a seat that is not in
			// this org. Both are faults an operator has to fix, and
			// both are silent otherwise: that account's events go to
			// whichever seat won.
			log.Warn("github_seat_identity_refused", "seat", seat.Handle(),
				"login", login, "error", err.Error())
			continue
		}
		registered++
	}
	return registered
}

// startGitHub builds the hosted code host's parser and resolves its seat
// identities.
func (e *Engine) startGitHub(ctx context.Context, c *Company, cfg *config.GitHub) (*github.Parser, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}
	env := e.resolver()

	// THE URL IS OPTIONAL AND IS RESOLVED THROUGH THE SAME CHAIN, because
	// an Enterprise Server address is as much a ${VAR} as a credential is
	// — it differs between a staging deployment and a production one. An
	// empty result is github.com, which is the documented default and not
	// an error: there is no way to tell "unset" from "resolved to nothing"
	// here, and refusing would break every company that simply uses
	// github.com.
	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	api, web := resolved.APIBase(), resolved.WebURL()

	// THE SIGNING SECRET IS NOT OPTIONAL, and it is checked HERE rather
	// than left to the first delivery.
	//
	// Config already refuses an enabled GitHub with no webhook_secret, so
	// reaching this line with an empty value means a ${VAR} that did not
	// resolve. That is invisible from anywhere else: the route answers 503
	// to every delivery, GitHub's own settings page shows a hook whose
	// deliveries keep failing, and no log line anywhere names the
	// variable.
	//
	// It is not a boot refusal: the caller logs github_unavailable and the
	// company runs on without its code host. That is the honest outcome,
	// because the integration IS unavailable — the alternative is one that
	// reports itself enabled and is inert.
	if strings.TrimSpace(env.Value(cfg.WebhookSecret)) == "" {
		// NAMES ONLY, never the value: this line goes to a log file.
		return nil, fmt.Errorf(
			"engine: github: webhook_secret resolved empty (%q) — nothing "+
				"would verify an inbound delivery, so every webhook GitHub "+
				"sends would be refused; set that variable in the environment "+
				"or this node's secret store", cfg.WebhookSecret)
	}

	// WHO ELSE IS TAKING PART, read through the agents' OWN apps.
	//
	// This took a shared organization token an operator pasted in, and the
	// token had nothing else left to do: an agent that acts as itself
	// already holds a credential that answers the question, installed on
	// the repositories it works in, and every tier grants the two reads a
	// participant lookup needs. So the reader is there for free, scoped to
	// what that agent may see rather than to whatever the person who made
	// the token could reach, and there is one fewer credential to paste,
	// rotate and be warned about.
	//
	// NOT VERIFIED at boot. A probe here would turn a rate-limited API
	// into a company that will not start, to learn what the first real
	// lookup learns anyway, and that one degrades instead of refusing.
	lookup := &github.SeatLookup{Opts: github.SeatAppOptions{
		APIBase: api, WebBase: web, Org: githubOrg(cfg),
		Seats: e.githubSeatApps(c, env),
	}}

	e.notify.github.resolve(ctx, api, web, github.SeatCredentials(c.Org, env.Value))
	registered := e.notify.github.register(e.Registry(), c, env)
	if registered == 0 {
		// Enabled with no seat identity is a company mid-setup —
		// `crewlet github provision` has not run — or a deployment that
		// refused every lookup. Logged loudly either way, because the
		// integration is completely inert in this state and nothing else
		// will say so.
		log.WarnContext(ctx, "github_has_no_seat_identities", "api", api,
			"detail", "every code-host webhook will name a stranger")
	}
	log.InfoContext(ctx, "github_wired", "api", api,
		"seat_identities", registered, "participants_lookup", len(lookup.Opts.Seats) > 0)
	return github.NewParser(github.ParserOptions{Participants: lookup}), nil
}

// reconcileGitHub rebuilds the hosted code host for a newly applied epoch.
//
// Two things move on an apply, and only one of them is the parser. The seat
// identities are rebuilt into the new registry by refreshParties, from the
// cache, with no I/O; this re-resolves — picking up a seat the revision
// ADDED and a credential it ROTATED — and swaps the parser, whose own
// company-derived input is the engine credential. A node that skipped it
// would keep answering with the old credential, which after a rotation means
// every participants lookup 401s and every thread quietly narrows to its
// author and assignees.
func (e *Engine) reconcileGitHub(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.GitHub
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	// RETIRED when the revision no longer declares it. Every reconciler
	// here converged only toward "configured", so setting `integrations.github.enabled: false` — the
	// gesture an operator makes after a credential leak — applied
	// cleanly, changed nothing, and left the boot-time parser routing
	// deliveries under the credential being revoked, while RoutedSources
	// went on listing it as reachable.
	if cfg == nil || !cfg.Enabled {
		if svc.Unregister(github.Backend) {
			log.InfoContext(ctx, "github_retired",
				"detail", "the revision no longer enables github; its deliveries "+
					"are refused at the webhook route and route to no seat")
		}
		return
	}
	parser, err := e.startGitHub(ctx, c, cfg)
	if err != nil || parser == nil {
		// THE PREVIOUS PARSER KEEPS RUNNING, same posture as the
		// self-hosted host's: routing by a stale credential is worse than
		// the new one and much better than not routing at all.
		log.ErrorContext(ctx, "github_reconcile_failed", "error", errorText(err),
			"detail", "the previous hosted code-host wiring is still current")
		return
	}
	if err := svc.Replace(parser, githubPrompt()); err != nil {
		log.ErrorContext(ctx, "github_reconcile_failed", "error", err.Error(),
			"detail", "the previous hosted code-host wiring is still current")
		return
	}
	log.InfoContext(ctx, "github_reconciled", "company", c.Config.Name)
}

// githubSeatApps reads every agent's own app out of the company document,
// with its key resolved. A seat with no block at all is skipped: it is a
// company that has not started, not a seat with a fault.
//
// ONE READING, used by both halves that need it: the reconcile, which asks
// what is still outstanding, and the participant lookup, which asks whose
// credential can read a thread. Two readers of one roster would be free to
// disagree about which apps exist.
//
// THE COMPANY IS PASSED IN, never read off the live epoch, because one caller
// is the APPLY. A revision is wired before it is published, so
// [Engine.Company] there is still the OUTGOING one: reading it would build
// the new revision's participant lookup from the old revision's seat apps,
// and a seat whose app was created, rotated or removed in the very apply
// being wired would be looked up as whatever it was before. Every other input
// [Engine.startGitHub] uses already comes from its own argument.
func (e *Engine) githubSeatApps(company *Company, env *config.Resolver) []github.SeatApp {
	out := []github.SeatApp{}
	if company == nil {
		return out
	}
	for role := range company.Config.EachRole() {
		seat := role.Seat()
		if !seat.IsAgent() {
			continue
		}
		app := role.Integrations.GitHub
		if app == nil {
			continue
		}
		tier, _ := github.ParseTier(app.TierOrDefault())
		out = append(out, github.SeatApp{
			Handle: seat.Handle(), Name: role.Name, Tier: tier, Repos: app.Repos,
			AppID: app.AppID, Slug: app.AppSlug,
			InstallationID: app.InstallationID,
			Key:            strings.TrimSpace(env.Value(app.PrivateKey)),
		})
	}
	return github.SeatsFrom(out)
}

// githubPrompt is the hosted code host's trigger builder. A value, held by
// nothing.
func githubPrompt() notify.Prompt { return github.Prompt{} }

// unresolved names the seats holding a code-host credential that resolves to
// no account.
//
// A SEAT WITH ITS OWN APP IS NEVER HERE, and that is the one way this differs
// from the other two: GitHub derives an app's account from its slug, which
// this engine wrote down when it created the app, so such a seat is routable
// with no lookup at all. Reporting it as unresolved because its leftover token
// happened not to answer would put a working seat on the card as broken.
//
// See [jiraIdentities.unresolved] for the rest of the contract.
func (g *githubIdentities) unresolved(c *Company, env *config.Resolver) []string {
	g.mu.Lock()
	known := maps.Clone(g.byToken)
	g.mu.Unlock()

	byApp := map[string]bool{}
	for role := range c.Config.EachRole() {
		seat := role.Seat()
		if app := role.Integrations.GitHub; seat.IsAgent() && app != nil &&
			github.NormalizeLogin(app.AppSlug) != "" {
			byApp[seat.Handle()] = true
		}
	}

	var out []string
	for seat := range c.Org.AllRoles() {
		handle := seat.Handle()
		if byApp[handle] {
			continue
		}
		token := github.CredentialOf(seat, env.Value)
		if token != "" && known[token] == "" {
			out = append(out, handle)
		}
	}
	slices.Sort(out)
	return out
}
