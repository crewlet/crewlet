package engine

import (
	"context"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/notify"
)

// Retrying the seat identities this node could not resolve. A webhook names
// people by account, so the engine looks each seat's account up with the
// seat's own credential; a failed lookup leaves the seat unresolved, and no
// apply comes along to retry it. The integration loop does: an unresolved
// seat is a FINDING, which keeps the surface from reporting itself ready, so
// the loop keeps visiting on the engine-acts cadence (30 s doubling to 5 min)
// rather than a timer of its own, and the card stops claiming Connected.

// resolveRouting re-resolves this surface's seat identities and reports the
// ones that still have none.
//
// It re-resolves before reporting, so the report says what is true after this
// retry; a seat that resolves here goes into the LIVE registry and receives
// work at once, with no config change.
func (e *Engine) resolveRouting(
	ctx context.Context, kind integration.Kind, reported map[string]bool,
) []integration.Finding {
	company := e.Company()
	if company == nil {
		return nil
	}
	var unresolved []string
	switch kind {
	case integration.KindJira:
		unresolved = e.rewireJira(ctx, company)
	case integration.KindGitLab:
		unresolved = e.rewireGitLab(ctx, company)
	case integration.KindGitHub:
		unresolved = e.rewireGitHub(ctx, company)
	default:
		// Every other surface resolves a seat without a lookup (Slack and
		// Mattermost at connect, Confluence and Datadog from the handle).
		return nil
	}

	out := make([]integration.Finding, 0, len(unresolved))
	for _, handle := range unresolved {
		// A seat the surface's own pass already reported gets no second
		// finding, which would contradict the first about who has to act
		// (`reported` in integrations.go).
		if reported[handle] {
			continue
		}
		out = append(out, integration.Finding{
			Kind: integration.FindingIdentityMissing, Subject: handle,
			Detail: handle + " holds a credential for this surface and no " +
				"account could be read with it, so every delivery naming " +
				"that account reaches a stranger and every one naming this " +
				"seat reaches nobody — retried on each pass",
		})
	}
	return out
}

// rewireJira re-resolves the tracker's seat identities into the live registry
// and answers which seats still have none.
func (e *Engine) rewireJira(ctx context.Context, c *Company) []string {
	cfg := c.Config.Integrations.Jira
	if cfg == nil {
		return nil
	}
	env := e.resolver()
	base := jiraBaseURL(cfg, env)
	if base == "" {
		// The pass reports the unresolved reference itself; a finding per
		// seat here would bury it.
		return nil
	}
	e.notify.jira.resolve(ctx, base, c.Config.Integrations.Atlassian.Declared(), jiraSeatCredentials(c, env))
	// Into the registry built for c or nowhere ([Engine.registryOf]): a
	// newer revision's registry holds its own credentials' accounts, and
	// writing c's over them would route a rotated credential to nobody.
	registered, current := e.intoRegistryOf(c, func(reg *notify.Registry) int {
		return e.notify.jira.register(reg, c, env)
	})
	if !current {
		return nil
	}
	unresolved := e.notify.jira.unresolved(c, env)
	e.logRewired(ctx, integration.KindJira, registered, unresolved)
	return unresolved
}

// logRewired records a retry that resolved seats, so the log does not end on
// the apply's `*_has_no_seat_identities` warning after routing recovered.
//
// Only on a change of the registered count: it runs on every pass of three
// surfaces, and a line per pass would drown the lines that matter.
func (e *Engine) logRewired(
	ctx context.Context, kind integration.Kind, registered int, unresolved []string,
) {
	if !e.rewired.changed(kind, registered) {
		return
	}
	log.InfoContext(ctx, kind.String()+"_seat_identities_resolved",
		"seat_identities", registered, "still_unresolved", len(unresolved))
}

// rewireLog remembers what the last retry resolved, so only a change is
// logged. The zero value is ready.
//
// In memory and per node, deliberately: a restarted node's first pass should
// say what it found, and a count shared through coordination would silence it
// at the one moment somebody reads the log.
type rewireLog struct {
	mu   sync.Mutex
	last map[integration.Kind]int
}

// changed reports a count this surface has not already logged.
func (r *rewireLog) changed(kind integration.Kind, registered int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if was, seen := r.last[kind]; seen && was == registered {
		return false
	}
	if r.last == nil {
		r.last = map[integration.Kind]int{}
	}
	r.last[kind] = registered
	// Zero is not news: the pass's finding and the
	// `*_has_no_seat_identities` warning already say it.
	return registered > 0
}

// rewireGitLab is the code host's half, on identical terms.
func (e *Engine) rewireGitLab(ctx context.Context, c *Company) []string {
	cfg := c.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	env := e.resolver()
	url := env.Value(cfg.URL)
	// The start path's refusal, through the same function: without it this
	// would report agents wired behind a route answering 503 to every
	// delivery. The pass reports the unusable secret; a finding per seat
	// here would bury it.
	if url == "" || gitlabWirable(cfg, env) != nil {
		return nil
	}
	e.notify.gitlab.resolve(ctx, url, gitlabSeatTokens(c, env))
	// Into the registry built for c or nowhere ([Engine.registryOf]): a
	// newer revision's registry holds its own credentials' accounts, and
	// writing c's over them would route a rotated credential to nobody.
	registered, current := e.intoRegistryOf(c, func(reg *notify.Registry) int {
		return e.notify.gitlab.register(reg, c, env)
	})
	if !current {
		return nil
	}
	unresolved := e.notify.gitlab.unresolved(c, env)
	e.logRewired(ctx, integration.KindGitLab, registered, unresolved)
	return unresolved
}

// rewireGitHub is the hosted code host's rewire. A seat with its own app needs
// no lookup (GitHub derives the account from its slug), so it is never
// reported unresolved.
func (e *Engine) rewireGitHub(ctx context.Context, c *Company) []string {
	cfg := c.Config.Integrations.GitHub
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	env := e.resolver()
	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	api, web := resolved.APIBase(), resolved.WebURL()
	if api == "" || githubWirable(cfg, env) != nil {
		return nil
	}
	e.notify.github.resolve(ctx, api, web, github.SeatCredentials(c.Org, env.Value))
	// Into the registry built for c or nowhere ([Engine.registryOf]): a
	// newer revision's registry holds its own credentials' accounts, and
	// writing c's over them would route a rotated credential to nobody.
	registered, current := e.intoRegistryOf(c, func(reg *notify.Registry) int {
		return e.notify.github.register(reg, c, env)
	})
	if !current {
		return nil
	}
	unresolved := e.notify.github.unresolved(c, env)
	e.logRewired(ctx, integration.KindGitHub, registered, unresolved)
	return unresolved
}
