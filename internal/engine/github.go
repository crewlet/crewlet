package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The hosted code host, wired. Inbound-only: an agent acts on GitHub inside
// its sandbox, through its own tools; the engine only tells the seat.
//
// A delivery names people by login, so without a seat-to-account mapping
// every target is a stranger and the integration is silently inert. The
// mapping is DERIVED from the seat's own credential (GET /user), never
// declared: a declared login that disagrees with the credential is a
// misroute nothing can detect.

// githubIdentities remembers which account each seat token authenticates as,
// keyed on the token; the cache's rules are [identityCache]'s.
type githubIdentities struct {
	identityCache[string]
}

// resolve fills in the accounts behind any tokens not already known, on
// [identityCache.resolve]'s terms: register depends on a waiting caller
// holding the answer when this returns.
func (g *githubIdentities) resolve(ctx context.Context, api, web string, tokens []string) {
	g.identityCache.resolve(ctx, tokens, func(token string) string {
		client, err := github.NewClient(github.ClientOptions{
			APIBase: api, WebBase: web, Token: token,
		})
		if err != nil {
			log.WarnContext(ctx, "github_seat_client_failed", "error", err.Error())
			return ""
		}
		login, err := client.Me(ctx)
		if err != nil {
			log.WarnContext(ctx, "github_seat_identity_unresolved", "error", err.Error(),
				"detail", unresolvedSeatDetail("code-host events"))
			return ""
		}
		return login
	})
}

// register binds each resolved seat to its account in the given registry,
// with no I/O. It takes the registry rather than the live one because an
// apply builds a NEW registry, and a config-derived binding is rebuilt into
// it then.
func (g *githubIdentities) register(reg *notify.Registry, c *Company, env *config.Resolver) int {
	known := g.snapshot()

	var (
		registered int
		byApp      = map[string]bool{}
	)

	// AN AGENT'S OWN APP IS ITS IDENTITY, at no request: its account is its
	// slug. Registered FIRST, so a leftover token below cannot overwrite it.
	//
	// TWO SPELLINGS: a mention types the slug, a payload carries `slug[bot]`.
	// The registry is a bijection per namespace, so the bot login goes in the
	// companion namespace, as Slack's and Mattermost's do.
	for seat := range c.Org.AllRoles() {
		app := seat.GitHub
		if !seat.IsAgent() || app == nil {
			continue
		}
		slug := github.NormalizeLogin(app.AppSlug)
		if slug == "" {
			// The app is not created yet: the seat acts as nobody.
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
				// THE MENTION STILL WORKS; only recognising the agent's
				// own activity is lost, so the seat is kept.
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
			// Two seats sharing one account, or a seat not in this org:
			// an operator's fault, silent otherwise (the events go to
			// whichever seat won).
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

	// THE URL MAY BE A ${VAR} (an Enterprise Server differs per deployment).
	// Empty is github.com, the documented default: "unset" and "resolved to
	// nothing" cannot be told apart here.
	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	api, web := resolved.APIBase(), resolved.WebURL()

	// THE SIGNING SECRET IS CHECKED HERE, not at the first delivery: config
	// refuses an empty one, so empty here is a ${VAR} that did not resolve,
	// otherwise visible only as 503s on every delivery with no log naming
	// the variable. Not a boot refusal: the caller logs github_unavailable
	// and the company runs on without its code host, rather than reporting
	// an integration enabled that is inert.
	if err := githubWirable(cfg, env); err != nil {
		return nil, err
	}

	// WHO ELSE IS TAKING PART, read through the agents' OWN apps: every tier
	// grants the two reads a participant lookup needs, scoped to what that
	// agent may see, with no shared organization token to paste and rotate.
	//
	// NOT VERIFIED at boot: a probe would turn a rate-limited API into a
	// company that will not start; the first real lookup degrades instead.
	lookup := &github.SeatLookup{Opts: github.SeatAppOptions{
		APIBase: api, WebBase: web, Org: githubOrg(cfg),
		Seats: e.githubSeatApps(c, env),
	}, Issuer: &e.githubIssuer}

	e.notify.github.resolve(ctx, api, web, github.SeatCredentials(c.Org, env.Value))
	registered := e.intoLiveRegistry(func(reg *notify.Registry) int {
		return e.notify.github.register(reg, c, env)
	})
	switch pending := githubAppsPending(c); {
	case registered == 0 && len(pending) > 0:
		// AGENTS WAITING ON THEIR OWN APPS: a person's step part way
		// through, not a fault, so said once per wiring at info.
		log.InfoContext(ctx, "github_seat_identities_waiting", "api", api, "seats", pending,
			"detail", "each of these agents acts on GitHub as its own app, which is not "+
				"created yet; its identity is registered once the app exists")
	case registered == 0:
		// Mid-setup (`crewlet github provision` has not run) or every
		// lookup refused: the integration is inert and nothing else will
		// say so, hence a warning.
		log.WarnContext(ctx, "github_has_no_seat_identities", "api", api,
			"detail", "every code-host webhook will name a stranger")
	}
	log.InfoContext(ctx, "github_wired", "api", api,
		"seat_identities", registered, "participants_lookup", len(lookup.Opts.Seats) > 0)
	e.githubPulls.wire(lookup, c.Config.GitHubOwners(func(v string) string {
		return strings.TrimSpace(env.Value(v))
	}))
	return github.NewParser(github.ParserOptions{Participants: lookup}), nil
}

// reconcileGitHub rebuilds the hosted code host for a newly applied epoch.
// refreshParties rebuilds the seat identities from the cache; this
// re-resolves (a seat ADDED, a credential ROTATED) and swaps the parser.
// Skipped, a rotation leaves every participants lookup 401ing and every
// thread narrowed to its author and assignees.
func (e *Engine) reconcileGitHub(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.GitHub
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	// RETIRED when the revision no longer enables it: `enabled: false` is
	// how an operator answers a credential leak, and must stop routing
	// and drop it from RoutedSources.
	if cfg == nil || !cfg.Enabled {
		e.githubPulls.wire(nil, nil)
		if svc.Unregister(github.Backend) {
			log.InfoContext(ctx, "github_retired",
				"detail", "the revision no longer enables github; its deliveries "+
					"are refused at the webhook route and route to no seat")
		}
		return
	}
	parser, err := e.startGitHub(ctx, c, cfg)
	if err != nil || parser == nil {
		// THE PREVIOUS PARSER KEEPS RUNNING: a stale credential routes
		// better than none.
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
// with its key resolved, skipping a seat with no block (not started, not a
// fault). ONE READING for the reconcile and the participant lookup, so the
// two cannot disagree about which apps exist.
//
// THE COMPANY IS PASSED IN, never read off the live epoch: in an APPLY the
// revision is wired before it is published, so [Engine.Company] is still
// the outgoing one, and an app created, rotated or removed in that apply
// would be looked up as it was before.
func (e *Engine) githubSeatApps(company *Company, env *config.Resolver) []github.SeatApp {
	out := []github.SeatApp{}
	if company == nil {
		return out
	}
	for seat := range company.Org.AllRoles() {
		if !seat.IsAgent() || seat.GitHub == nil {
			continue
		}
		out = append(out, githubSeatApp(seat, env))
	}
	return github.SeatsFrom(out)
}

// githubSeatApp is one agent's own app as the company document records it,
// with its key resolved; the caller has checked the seat carries a block.
// The one reading for the roster and [Engine.seatResolver]'s token.
func githubSeatApp(seat *org.Role, env *config.Resolver) github.SeatApp {
	app := seat.GitHub
	return github.SeatApp{
		Handle: seat.Handle(), Name: seat.Name, Off: !app.Enabled,
		Permissions: github.PermissionsOf(app.Permissions), Repos: app.Repos,
		AppID: app.AppID, Slug: app.AppSlug,
		InstallationID: app.InstallationID,
		Key:            strings.TrimSpace(env.Value(app.PrivateKey)),
	}
}

// githubAppsPending is every agent seat that is set up to act as its own
// GitHub App and whose app does not exist yet, sorted: the seats whose
// identity waits on a person creating the app ([githubIdentities.register]
// registers each one's slug the moment there is one).
func githubAppsPending(c *Company) []string {
	var out []string
	if c == nil || c.Org == nil {
		return out
	}
	for seat := range c.Org.AllRoles() {
		if seat.IsAgent() && seat.GitHub != nil && seat.GitHub.Enabled &&
			github.NormalizeLogin(seat.GitHub.AppSlug) == "" {
			out = append(out, seat.Handle())
		}
	}
	slices.Sort(out)
	return out
}

// githubPrompt is the hosted code host's trigger builder.
func githubPrompt() notify.Prompt { return github.Prompt{} }

// githubWirable reports whether this revision's code host can be wired at
// all, shared by the wiring and the identity retry on [gitlabWirable]'s
// terms: a retry that skipped it would report agents wired behind a route
// answering 503.
func githubWirable(cfg *config.GitHub, env *config.Resolver) error {
	if strings.TrimSpace(env.Value(cfg.WebhookSecret)) == "" {
		// NAMES ONLY, never the value: this line goes to a log file.
		return fmt.Errorf(
			"engine: github: webhook_secret resolved empty (%q) — nothing "+
				"would verify an inbound delivery, so every webhook GitHub "+
				"sends would be refused; set that variable in the environment "+
				"or this node's secret store", cfg.WebhookSecret)
	}
	return nil
}

// unresolved names the seats holding a code-host credential that resolves to
// no account, on [jiraIdentities.unresolved]'s contract. A SEAT WITH ITS OWN
// APP IS NEVER HERE: its slug routes it, and a leftover token that did not
// answer must not put a working seat on the card as broken.
func (g *githubIdentities) unresolved(c *Company, env *config.Resolver) []string {
	known := g.snapshot()

	byApp := map[string]bool{}
	for seat := range c.Org.AllRoles() {
		if app := seat.GitHub; seat.IsAgent() && app != nil &&
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
