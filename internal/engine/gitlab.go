package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/whsec"
)

// The code host, wired. Inbound-only: an agent works on the code host inside
// its sandbox, as itself, through its own tools; the engine never pushes or
// opens a merge request for a seat, it only tells the seat what happened.
//
// Seat identity is the whole integration, and it is DERIVED: a webhook names
// people by username, so without the mapping every event names a stranger and
// the integration is silently inert. The engine calls GET /user with the
// seat's OWN credential and registers whatever answers. A declared username
// would be a misroute nothing detects when it disagrees with the credential,
// and would name a variable the seat's own tools never read.

// gitlabIdentities remembers which account each seat token authenticates as.
// Keyed on the token; the cache's rules are [identityCache]'s, and this adds
// only the client, the username and the failure logs.
type gitlabIdentities struct {
	identityCache[string]
}

// resolve fills in the accounts behind any tokens not already known, with
// [identityCache.resolve]'s promise that a caller holds the answer when this
// returns, which register depends on.
func (g *gitlabIdentities) resolve(ctx context.Context, url string, tokens []string) {
	g.identityCache.resolve(ctx, tokens, func(token string) string {
		client, err := gitlab.NewClient(gitlab.ClientOptions{URL: url, Token: token})
		if err != nil {
			log.WarnContext(ctx, "gitlab_seat_client_failed", "error", err.Error())
			return ""
		}
		username, err := client.Me(ctx)
		if err != nil {
			log.WarnContext(ctx, "gitlab_seat_identity_unresolved", "error", err.Error(),
				"detail", unresolvedSeatDetail("code-host events"))
			return ""
		}
		return username
	})
}

// register binds each resolved seat to its account in the given registry.
// NO I/O. It takes the registry rather than the live one because an apply
// builds a NEW registry, and this config-derived binding is rebuilt into it.
func (g *gitlabIdentities) register(reg *notify.Registry, c *Company, env *config.Resolver) int {
	known := g.snapshot()

	var registered int
	for seat := range c.Org.AllRoles() {
		token := gitlabSeatToken(seat, env)
		if token == "" {
			continue
		}
		username := known[token]
		if username == "" {
			continue
		}
		if err := reg.Register(gitlab.Backend, username, seat.Handle()); err != nil {
			// Two seats sharing one account, or a seat not in this org:
			// an operator's fault, silent otherwise (the account's events
			// go to whichever seat won).
			log.Warn("gitlab_seat_identity_refused", "seat", seat.Handle(),
				"username", username, "error", err.Error())
			continue
		}
		registered++
	}
	return registered
}

// startGitLab builds the code host's parser and resolves its seat
// identities.
func (e *Engine) startGitLab(ctx context.Context, c *Company, cfg *config.GitLab) (*gitlab.Parser, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}
	env := e.resolver()
	url := env.Value(cfg.URL)
	if url == "" {
		// The validator refused a missing literal, so this is an unset
		// variable; starting anyway fails every lookup less usefully.
		return nil, fmt.Errorf("engine: gitlab: url resolved empty (%q)", cfg.URL)
	}

	// The signing secret is checked here, not at the first delivery
	// ([gitlabWirable] has why).
	if err := gitlabWirable(cfg, env); err != nil {
		return nil, err
	}

	// THE ENGINE CREDENTIAL IS OPTIONAL: without it a comment reaches the
	// payload's assignees rather than everyone taking part, and directed
	// events are untouched, so this warns rather than refusing.
	var lookup gitlab.Participants
	if token := strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		client, err := gitlab.NewClient(gitlab.ClientOptions{URL: url, Token: token})
		if err != nil {
			return nil, fmt.Errorf("engine: gitlab: %w", err)
		}
		lookup = gitlab.Lookup{Client: client}
		// NOT VERIFIED at boot: a GET /user would make a briefly-down
		// instance stop the company, to learn what the first real lookup
		// learns while degrading (gitlab_participants_unavailable). The
		// seat credentials below are resolved because they ARE the
		// integration.
	} else {
		// Names the field, so the operator can act on the warning.
		log.WarnContext(ctx, "gitlab_has_no_routing_token",
			"field", "integrations.gitlab.token",
			"detail", "thread activity reaches the payload's assignees "+
				"rather than everyone taking part; set a read_api token "+
				"on the GitLab card to reach everyone taking part")
	}

	e.notify.gitlab.resolve(ctx, url, gitlabSeatTokens(c, env))
	registered := e.intoLiveRegistry(func(reg *notify.Registry) int {
		return e.notify.gitlab.register(reg, c, env)
	})
	if registered == 0 {
		// Mid-setup (`crewlet gitlab provision` has not run) or every
		// lookup refused: the integration is inert and nothing else says so.
		log.WarnContext(ctx, "gitlab_has_no_seat_identities", "url", url,
			"detail", "every code-host webhook will name a stranger")
	}
	log.InfoContext(ctx, "gitlab_wired", "url", url,
		"seat_identities", registered, "participants_lookup", lookup != nil)
	return gitlab.NewParser(gitlab.ParserOptions{Participants: lookup}), nil
}

// reconcileGitLab rebuilds the code host for a newly applied epoch.
//
// refreshParties rebuilds the seat identities into the new registry from the
// cache; this re-resolves (a seat ADDED, a credential ROTATED) and swaps the
// parser, which holds the engine credential. Skipping it after a rotation
// would 401 every participants lookup, quietly narrowing threads to assignees.
func (e *Engine) reconcileGitLab(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.GitLab
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	// RETIRED when the revision no longer enables it: `enabled: false` is
	// the gesture after a credential leak, and must stop the parser routing
	// under the revoked credential and drop it from RoutedSources.
	if cfg == nil || !cfg.Enabled {
		if svc.Unregister(gitlab.Backend) {
			log.InfoContext(ctx, "gitlab_retired",
				"detail", "the revision no longer enables gitlab; its deliveries "+
					"are refused at the webhook route and route to no seat")
		}
		return
	}
	parser, err := e.startGitLab(ctx, c, cfg)
	if err != nil || parser == nil {
		// THE PREVIOUS PARSER KEEPS RUNNING, as the tracker's does: a
		// stale credential beats not routing at all.
		log.ErrorContext(ctx, "gitlab_reconcile_failed", "error", errorText(err),
			"detail", "the previous code-host wiring is still current")
		return
	}
	if err := svc.Replace(parser, gitlabPrompt()); err != nil {
		log.ErrorContext(ctx, "gitlab_reconcile_failed", "error", err.Error(),
			"detail", "the previous code-host wiring is still current")
		return
	}
	log.InfoContext(ctx, "gitlab_reconciled", "company", c.Config.Name)
}

// gitlabWirable reports whether this revision's code host can be wired at all.
// ONE IMPLEMENTATION for [Engine.startGitLab] and [Engine.rewireGitLab], which
// must never disagree: otherwise seats look wired behind a route that 503s.
//
// THE SIGNING SECRET IS NOT OPTIONAL. Config refuses a missing or malformed
// literal, so an unusable value here is a ${VAR} unset or wrong, which nothing
// else shows: every delivery 503s while GitLab shows a healthy hook. It is no
// boot refusal: the caller logs gitlab_unavailable and the company runs on
// without its code host, rather than one that claims enabled and is inert.
func gitlabWirable(cfg *config.GitLab, env *config.Resolver) error {
	secret := env.Value(cfg.SigningSecret)
	if secret == "" {
		return fmt.Errorf(
			"engine: gitlab: signing_secret resolved empty (%q) — nothing "+
				"would verify an inbound delivery, so every webhook this "+
				"instance sends would be refused; set that variable in the "+
				"environment or this node's secret store", cfg.SigningSecret)
	}
	if !whsec.Valid(secret) {
		// NAMES ONLY, never the value: this line goes to a log file.
		return fmt.Errorf(
			"engine: gitlab: signing_secret (%q) resolved to a value that is "+
				"not %s followed by standard base64 over a %d-byte key, which "+
				"is the only shape GitLab signs with — it cannot be the HMAC "+
				"key for any delivery",
			cfg.SigningSecret, whsec.Prefix, whsec.KeyBytes)
	}
	return nil
}

// gitlabSeatTokens are the distinct credentials the company's agent seats
// hold, sorted: distinct because seats may share one (before per-seat
// provisioning) and one answer should cost one request; sorted so boots' logs
// diff.
func gitlabSeatTokens(c *Company, env *config.Resolver) []string {
	tokens := map[string]bool{}
	for seat := range c.Org.AllRoles() {
		if token := gitlabSeatToken(seat, env); token != "" {
			tokens[token] = true
		}
	}
	return slices.Sorted(maps.Keys(tokens))
}

// gitlabSeatToken reads a seat's code-host credential, under whichever key
// its tool stack names it. The keys are [gitlab.CredentialKeys], which the
// provisioner scans too: a key one side missed would mint credentials nothing
// authenticates with, and the code host would name strangers.
func gitlabSeatToken(seat *org.Role, env *config.Resolver) string {
	if seat == nil || seat.IsHuman() {
		// A human seat holds no tool credential; ReconcileHumanContacts
		// registers its contact block.
		return ""
	}
	block := seat.MCPEnv[gitlab.SeatEnv]
	for _, key := range gitlab.CredentialKeys {
		value := strings.TrimSpace(env.Value(block[key]))
		if value == "" {
			continue
		}
		// Stripping "Bearer " lets one config shape serve both an HTTP
		// MCP server's Authorization header and this lookup.
		return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	}
	return ""
}

// gitlabPrompt is the code host's trigger builder. A value, held by nothing.
func gitlabPrompt() notify.Prompt { return gitlab.Prompt{} }

// unresolved names the seats holding a code-host credential that resolves to
// no account. See [jiraIdentities.unresolved].
func (g *gitlabIdentities) unresolved(c *Company, env *config.Resolver) []string {
	known := g.snapshot()

	var out []string
	for seat := range c.Org.AllRoles() {
		token := gitlabSeatToken(seat, env)
		if token != "" && known[token] == "" {
			out = append(out, seat.Handle())
		}
	}
	slices.Sort(out)
	return out
}
