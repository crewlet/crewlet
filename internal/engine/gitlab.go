package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/whsec"
)

// The code host, wired.
//
// Inbound-only like the tracker, and for a stronger reason: an agent's work
// on a code host happens INSIDE its sandbox, under its own identity, through
// its own tools. The engine never pushes a commit or opens a merge request
// on a seat's behalf — it only tells the seat something happened.
//
// # Seat identity is the whole integration, and it is DERIVED
//
// A GitLab webhook names people by username, and nothing in the org model
// says which account a seat holds. Without that mapping every event names a
// stranger, the routing gate drops every target, and the integration is
// silently inert.
//
// So the engine asks: it calls GET /user with the seat's OWN credential and
// registers whatever account answers. A declared username beside the token
// would be cheaper and is the wrong shape — a declaration that disagrees
// with the credential is a misroute nothing can detect, and it would make
// the engine name a tool-specific variable that the seat's actual tools do
// not read.

// gitlabIdentities remembers which account each seat credential
// authenticates as.
//
// KEYED ON THE TOKEN, which is what makes an apply free: identity is a
// function of the credential, credentials change rarely, and a config
// revision that touched something else must not spend one request per seat
// to re-learn what it already knows. A rotated token is a cache miss and
// costs exactly one request, which is correct — it may well be a different
// account.
type gitlabIdentities struct {
	mu      sync.Mutex
	byToken map[string]string
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
func (g *gitlabIdentities) resolve(ctx context.Context, url string, tokens []string) {
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

	var wg sync.WaitGroup
	found := make([]string, len(missing))
	for i, token := range missing {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := gitlab.NewClient(gitlab.ClientOptions{URL: url, Token: token})
			if err != nil {
				log.Warn("gitlab_seat_client_failed", "error", err.Error())
				return
			}
			username, err := client.Me(ctx)
			if err != nil {
				log.Warn("gitlab_seat_identity_unresolved", "error", err.Error(),
					"detail", "this seat receives no code-host events until "+
						"the next apply re-resolves it")
				return
			}
			found[i] = username
		}()
	}
	wg.Wait()

	g.mu.Lock()
	defer g.mu.Unlock()
	for i, username := range found {
		if username != "" {
			g.byToken[missing[i]] = username
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
func (g *gitlabIdentities) register(reg *notify.Registry, c *Company, env *config.Resolver) int {
	g.mu.Lock()
	known := maps.Clone(g.byToken)
	g.mu.Unlock()

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
			// Two seats sharing one account, or a seat that is not in
			// this org. Both are faults an operator has to fix, and
			// both are silent otherwise: that account's events go to
			// whichever seat won.
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
		// A reference that did not resolve. The validator already
		// refused an enabled GitLab with no url literal, so this is a
		// missing variable — and starting anyway would point every
		// lookup at "" and fail with a much less useful message.
		return nil, fmt.Errorf("engine: gitlab: url resolved empty (%q)", cfg.URL)
	}

	// THE SIGNING SECRET IS NOT OPTIONAL, and it is checked HERE rather
	// than left to the first delivery.
	//
	// Config already refuses an enabled GitLab whose signing_secret is
	// missing or is a literal the vendor could never have produced, so
	// reaching this line with an unusable value means a ${VAR} that did not
	// resolve, or resolved to something else. Neither is visible from
	// anywhere: the route answers 503 to every delivery, GitLab's own
	// settings page shows a healthy hook that keeps failing, and no log
	// line anywhere names the variable.
	//
	// The same call url gets above, and it is not a boot refusal: the
	// caller logs gitlab_unavailable and the company runs on without its
	// code host. That is the honest outcome, because the integration IS
	// unavailable — the alternative is one that reports itself enabled and
	// is inert.
	secret := env.Value(cfg.SigningSecret)
	if secret == "" {
		return nil, fmt.Errorf(
			"engine: gitlab: signing_secret resolved empty (%q) — nothing "+
				"would verify an inbound delivery, so every webhook this "+
				"instance sends would be refused; set that variable in the "+
				"environment or this node's secret store", cfg.SigningSecret)
	}
	if !whsec.Valid(secret) {
		// NAMES ONLY, never the value: this line goes to a log file.
		return nil, fmt.Errorf(
			"engine: gitlab: signing_secret (%q) resolved to a value that is "+
				"not %s followed by standard base64 over a %d-byte key, which "+
				"is the only shape GitLab signs with — it cannot be the HMAC "+
				"key for any delivery",
			cfg.SigningSecret, whsec.Prefix, whsec.KeyBytes)
	}

	// THE ENGINE CREDENTIAL IS OPTIONAL and its absence is a documented
	// degradation rather than a failure: without it a comment reaches
	// whoever the payload named — the assignees — instead of everyone
	// taking part. Directed events are untouched, which is why this warns
	// rather than refusing.
	var lookup gitlab.Participants
	if token := strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		client, err := gitlab.NewClient(gitlab.ClientOptions{URL: url, Token: token})
		if err != nil {
			return nil, fmt.Errorf("engine: gitlab: %w", err)
		}
		lookup = gitlab.Lookup{Client: client}
		// NOT VERIFIED at boot. A GET /user here would turn an instance
		// that is briefly down into a company that will not start, to
		// learn something the first real lookup learns anyway — and that
		// one degrades instead of refusing, reporting it as
		// gitlab_participants_unavailable on the event it affected.
		//
		// The SEAT credentials below are the opposite case, and the
		// difference is what the request buys: verifying this one buys
		// nothing, while resolving those is the entire integration.
	} else {
		log.Warn("gitlab_has_no_engine_token",
			"detail", "thread activity reaches the payload's assignees "+
				"rather than everyone taking part")
	}

	e.notify.gitlab.resolve(ctx, url, gitlabSeatTokens(c, env))
	registered := e.notify.gitlab.register(e.Registry(), c, env)
	if registered == 0 {
		// Enabled with no seat identity is a company mid-setup —
		// `crewlet gitlab provision` has not run — or an instance that
		// refused every lookup. Logged loudly either way, because the
		// integration is completely inert in this state and nothing else
		// will say so.
		log.Warn("gitlab_has_no_seat_identities", "url", url,
			"detail", "every code-host webhook will name a stranger")
	}
	log.Info("gitlab_wired", "url", url,
		"seat_identities", registered, "participants_lookup", lookup != nil)
	return gitlab.NewParser(gitlab.ParserOptions{Participants: lookup}), nil
}

// reconcileGitLab rebuilds the code host for a newly applied epoch.
//
// Two things move on an apply, and only one of them is the parser. The seat
// identities are rebuilt into the new registry by refreshParties, from the
// cache, with no I/O; this re-resolves — picking up a seat the revision
// ADDED and a credential it ROTATED — and swaps the parser, whose own
// company-derived input is the engine credential. A node that skipped it
// would keep answering with the old credential, which after a rotation means
// every participants lookup 401s and every thread quietly narrows to its
// assignees.
func (e *Engine) reconcileGitLab(ctx context.Context, c *Company) {
	cfg := c.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled {
		return
	}
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	parser, err := e.startGitLab(ctx, c, cfg)
	if err != nil || parser == nil {
		// THE PREVIOUS PARSER KEEPS RUNNING, same posture as the
		// tracker's: routing by a stale credential is worse than the new
		// one and much better than not routing at all.
		log.Error("gitlab_reconcile_failed", "error", errorText(err),
			"detail", "the previous code-host wiring is still current")
		return
	}
	if err := svc.Replace(parser, gitlabPrompt()); err != nil {
		log.Error("gitlab_reconcile_failed", "error", err.Error(),
			"detail", "the previous code-host wiring is still current")
		return
	}
	log.Info("gitlab_reconciled", "company", c.Config.Name)
}

// gitlabSeatTokens are the distinct credentials the company's agent seats
// hold, sorted.
//
// DISTINCT because several seats may legitimately share one — a company mid-
// migration, or one that has not provisioned per-seat accounts yet — and
// resolving the same token once per seat would spend N requests to learn one
// answer. Sorted so a boot's log lines are diffable against the next one's.
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
// its tool stack names it.
//
// THE KEYS COME FROM THE VENDOR PACKAGE, which is also what the provisioner
// scans with. They were once written out twice — here and there — under a
// comment saying the two must not drift, which is the shape of a bug rather
// than a guard against one: a provisioner minting into a key this lookup did
// not read would hand every seat a credential nothing authenticates with,
// and the only symptom is a code host that names strangers.
func gitlabSeatToken(seat *org.Role, env *config.Resolver) string {
	if seat == nil || seat.IsHuman() {
		// A human seat is addressable through its own contact block,
		// which ReconcileHumanContacts registers. It holds no tool
		// credential and must never be looked up as though it did.
		return ""
	}
	block := seat.MCPEnv[gitlab.SeatEnv]
	for _, key := range gitlab.CredentialKeys {
		value := strings.TrimSpace(env.Value(block[key]))
		if value == "" {
			continue
		}
		// "Authorization: Bearer <pat>" carries the credential behind a
		// scheme. Stripping it is what lets one config shape work
		// through both an HTTP MCP server and this lookup.
		return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	}
	return ""
}

// gitlabPrompt is the code host's trigger builder. A value, held by nothing.
func gitlabPrompt() notify.Prompt { return gitlab.Prompt{} }
