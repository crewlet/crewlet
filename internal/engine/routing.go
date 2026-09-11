package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
)

// Retrying the seat identities this node could not resolve.
//
// # A lookup that failed had nothing that would ever ask again
//
// A tracker or code-host webhook names people by ACCOUNT, and nothing in the
// org model says which account a seat holds — so the engine asks, with the
// seat's own credential, and registers whatever answers. Each of the three
// surfaces that does this leaves a seat whose lookup FAILED unresolved rather
// than failing the boot, on the reasoning that the instance may be briefly
// down and the next apply retries.
//
// The next apply is the problem. Nothing schedules one: an apply happens when
// a person changes the configuration, so a lookup that failed for a reason
// that cleared itself a second later stayed failed until somebody edited
// something unrelated.
//
// Measured: a seat's Atlassian token was created and sealed, the tracker
// checked it one second later, the account was not grantable yet and answered
// 403 — and the surface held seat_identities=0 with every card reading
// Connected. Two minutes later it had still not asked again. The same
// happened on the code host, under the same "every webhook will name a
// stranger" warning nobody was watching.
//
// # The reconcile loop is what asks again, and it already has the cadence
//
// This is not a new timer. The integration loop visits each surface on a
// cadence that follows WHO HAS TO ACT, and an unresolved seat identity is the
// ENGINE's own work — the 30-seconds-doubling-to-5-minutes row, which is
// exactly the shape of a lookup that will probably succeed shortly. So the
// retry is a visit, and what makes the loop keep visiting is that the surface
// stops reporting itself ready: an unresolved seat is a FINDING now, which is
// the same change that stops the card claiming Connected over an agent that
// receives nothing.

// resolveRouting re-resolves this surface's seat identities and reports the
// ones that still have none.
//
// RE-RESOLVING BEFORE REPORTING, in that order, because the report is about
// what is true after this node has tried again — not about what was true at
// the last apply. A seat that resolves here is registered into the LIVE
// registry and starts receiving work immediately, with no config change and
// nothing for an operator to press.
func (e *Engine) resolveRouting(ctx context.Context, kind integration.Kind) []integration.Finding {
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
		// Every other surface resolves a seat from the document alone —
		// Slack and Mattermost from an identity their transport learned at
		// connect, Confluence and Datadog from the handle itself — so
		// there is no lookup here to have failed.
		return nil
	}

	out := make([]integration.Finding, 0, len(unresolved))
	for _, handle := range unresolved {
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
		// Nothing to ask. The pass reports the unresolved reference as a
		// credential finding of its own, and a second one here naming
		// every seat would bury it.
		return nil
	}
	e.notify.jira.resolve(ctx, base, jira.DeploymentOf(base), jiraSeatCredentials(c, env))
	e.notify.jira.register(e.Registry(), c, env)
	return e.notify.jira.unresolved(c, env)
}

// rewireGitLab is the code host's half, on identical terms.
func (e *Engine) rewireGitLab(ctx context.Context, c *Company) []string {
	cfg := c.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	env := e.resolver()
	url := env.Value(cfg.URL)
	// THE SAME REFUSAL THE START PATH MAKES, through the same function.
	// Written without it, this resolved seat identities on a config that
	// can verify no delivery at all — reporting agents wired behind a route
	// answering 503 to everything, which is the state the refusal exists to
	// prevent. The surface's own pass reports the unusable secret; a second
	// finding per seat here would bury it.
	if url == "" || gitlabWirable(cfg, env) != nil {
		return nil
	}
	e.notify.gitlab.resolve(ctx, url, gitlabSeatTokens(c, env))
	e.notify.gitlab.register(e.Registry(), c, env)
	return e.notify.gitlab.unresolved(c, env)
}

// rewireGitHub is the hosted code host's, and differs in one way that matters:
// a seat with its own APP needs no lookup at all, because GitHub derives the
// account from a slug this engine wrote down. Those seats are never
// unresolved and must never be reported.
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
	e.notify.github.register(e.Registry(), c, env)
	return e.notify.github.unresolved(c, env)
}
