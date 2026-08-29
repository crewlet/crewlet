package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The knowledge base, wired.
//
// # Two halves with different failure postures, from one credential
//
// The PARSER routes page events, and a broken one costs notifications. The
// SEARCHER answers the Plan phase's "what do we already know about this",
// and a broken one costs every seat its company's written knowledge on every
// turn — silently, because an empty knowledge block is indistinguishable
// from a company that has written nothing down.
//
// So the org credential is optional for routing (a page's mentions are in
// the payload) and REQUIRED for search, and the two are reported separately.

// confluenceSeatEnvs are the mcp_env servers a seat's own Confluence
// credential can live under, in the order they are tried.
//
// The same two the tracker reads, because it is the same Atlassian identity
// and the community MCP server covers both products under one entry.
var confluenceSeatEnvs = []string{"atlassian", "confluence"}

// confluenceCredentialKeys and confluenceEmailKeys are the spellings a
// seat's credential arrives under.
//
// A seat WITH one searches as itself and Confluence enforces its own page
// ACLs; a seat without one falls back to the org account, and an unscoped
// search is then refused — see [knowledge.Permitted].
var (
	confluenceCredentialKeys = []string{
		"CONFLUENCE_API_TOKEN", "CONFLUENCE_PERSONAL_TOKEN",
		"CONFLUENCE_TOKEN", "ATLASSIAN_API_TOKEN",
	}
	confluenceEmailKeys = []string{
		"CONFLUENCE_USERNAME", "CONFLUENCE_EMAIL", "ATLASSIAN_EMAIL",
	}
)

// confluenceParts is what the knowledge base contributes to a company.
type confluenceParts struct {
	parser   *confluence.Parser
	searcher *confluence.Searcher

	// pages reads the skills space. Nil with no org credential, which is
	// the same degradation the searcher takes: a company whose read token
	// lapsed keeps routing and stops learning.
	pages *confluence.Client
}

// startConfluence builds the knowledge base's parser and searcher.
func (e *Engine) startConfluence(c *Company, cfg *config.Confluence) (confluenceParts, error) {
	if cfg == nil {
		return confluenceParts{}, nil
	}
	env := e.resolver()
	resolved := config.Confluence{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		CloudID: strings.TrimSpace(env.Value(cfg.CloudID)),
		SiteURL: strings.TrimSpace(env.Value(cfg.SiteURL)),
	}
	base := resolved.BaseURL()
	if base == "" {
		// A reference that did not resolve: the validator already refused
		// a block with neither literal, and starting anyway would point
		// every read at "" and fail with a much less useful message.
		return confluenceParts{}, fmt.Errorf(
			"engine: confluence: neither url (%q) nor cloud_id (%q) resolved "+
				"to anything", cfg.URL, cfg.CloudID)
	}
	site := resolved.ShareableBaseURL()
	skillsSpace := cfg.SkillsSpaceKey()

	// THE ORG CREDENTIAL IS WHAT SEARCH RESTS ON. Without it a seat with
	// its own credential still searches — as itself, which is the better
	// path anyway — and a seat without one gets nothing. Routing is
	// unaffected either way, which is why this warns rather than refusing.
	var orgClient *confluence.Client
	if token := strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: base, Email: env.Value(cfg.Email), Token: token,
		})
		if err != nil {
			return confluenceParts{}, fmt.Errorf("engine: confluence: %w", err)
		}
		orgClient = client
	} else {
		log.Warn("confluence_has_no_org_token",
			"detail", "a seat with no Confluence credential of its own reads "+
				"nothing, and the tool-skill walk cannot run")
	}

	leads := confluence.LeadsFrom(c.Org)
	parts := confluenceParts{
		parser: confluence.NewParser(confluence.ParserOptions{
			SiteURL: site, Leads: leads, SkillsSpace: skillsSpace,
			// THE INDEXER RUNS BEFORE EVERY ROUTING FILTER, which is the
			// point: the skills space's own events are excluded from
			// routing, so an indexer that only saw routable events would
			// never see a skill change at all.
			OnPage: e.reindexConfluenceSkill(skillsSpace),
			// A TYPED NIL WOULD NOT BE NIL. confluenceWatchers returns a
			// *pageWatchers, and assigning one straight into an interface
			// field makes a non-nil interface holding a nil pointer — so
			// the parser's `watchers == nil` check would pass and every
			// call would go through a nil receiver.
			Watchers: watchersFor(e.confluenceWatchers()),
		}),
		pages: orgClient,
	}
	if orgClient != nil {
		parts.searcher = confluence.NewSearcher(confluence.SearcherOptions{
			Org: orgClient, ForSeat: seatConfluenceClient(env, base),
			SkillsSpace: skillsSpace, SiteURL: site,
		})
	}
	log.Info("confluence_wired", "url", base, "site", site,
		"spaces_with_leads", len(leads), "skills_space", skillsSpace,
		"org_token", orgClient != nil)
	return parts, nil
}

// reconcileConfluence rebuilds the knowledge base for a newly applied epoch.
//
// EVERYTHING THE PARSER AND SEARCHER HOLD IS DERIVED FROM THE COMPANY, and
// two halves of it move on an apply. The SPACE-LEAD MAP is the org chart, so
// a node that kept its boot-time parser routes the new revision's page
// activity to the seat that led that space under the old one — silently,
// because a lead-fallback notification looks identical whoever it reached.
// The CREDENTIAL is the other half: after a rotation the old client 401s on
// every read, and the Plan phase's knowledge block goes empty with nothing
// saying why.
//
// The tracker beside this one is reconciled for the first reason, and this
// package needed the same edge from the moment it had a lead map.
func (e *Engine) reconcileConfluence(c *Company) {
	cfg := c.Config.Integrations.Confluence
	if cfg == nil {
		return
	}
	e.notify.mu.Lock()
	svc, running := e.notify.service, e.notify.confluence.parser != nil
	e.notify.mu.Unlock()
	if svc == nil || !running {
		// Not started, or started without a knowledge base. Boot owns
		// that case; re-running it here would race the boot path.
		return
	}

	parts, err := e.startConfluence(c, cfg)
	if err != nil || parts.parser == nil {
		// THE PREVIOUS WIRING KEEPS RUNNING. A revision whose Confluence
		// block is broken must not leave the company with no knowledge
		// base at all — the old one reads by a stale credential, which is
		// worse than the new one and much better than nothing.
		log.Error("confluence_reconcile_failed", "error", errorText(err),
			"detail", "the previous knowledge-base wiring is still current")
		return
	}
	if err := svc.Replace(parts.parser, confluencePrompt()); err != nil {
		log.Error("confluence_reconcile_failed", "error", err.Error(),
			"detail", "the previous knowledge-base wiring is still current")
		return
	}
	e.notify.mu.Lock()
	e.notify.confluence = parts
	e.notify.mu.Unlock()
	log.Info("confluence_reconciled", "company", c.Config.Name)
}

// seatConfluenceClient resolves a seat's own credential.
//
// The second result is what [knowledge.Permitted] turns on: TRUE means the
// search runs as this seat and Confluence's own ACLs bound it, so an
// unscoped search is safe. A seat with no credential gets (nil, false) and
// falls back to the org account, which may not search unscoped.
func seatConfluenceClient(env *config.Resolver, base string) confluence.SeatClient {
	return func(seat *org.Role) (*confluence.Client, bool) {
		if seat == nil || seat.IsHuman() {
			return nil, false
		}
		var token, email string
		for _, name := range confluenceSeatEnvs {
			block := seat.MCPEnv[name]
			for _, key := range confluenceCredentialKeys {
				if value := strings.TrimSpace(env.Value(block[key])); value != "" {
					token = value
					break
				}
			}
			if token == "" {
				continue
			}
			for _, key := range confluenceEmailKeys {
				if value := strings.TrimSpace(env.Value(block[key])); value != "" {
					email = value
					break
				}
			}
			break
		}
		if token == "" {
			return nil, false
		}
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: base, Email: email, Token: token,
		})
		if err != nil {
			log.Warn("confluence_seat_client_failed", "seat", seat.Handle(),
				"error", err.Error())
			return nil, false
		}
		return client, true
	}
}

// reindexConfluenceSkill re-reads one page into the skill registry.
//
// PER PAGE rather than a whole re-walk, because a wiki edit is one page and
// walking a space on every save would spend a request per page per edit. A
// page that is not a skill decodes to nothing and is dropped by the
// registry's own admission, which is what makes this safe to call for every
// change in the space.
func (e *Engine) reindexConfluenceSkill(space string) func(context.Context, string, string) error {
	if space == "" {
		return nil
	}
	return func(ctx context.Context, eventType, pageID string) error {
		e.notify.mu.Lock()
		client := e.notify.confluence.pages
		e.notify.mu.Unlock()
		if client == nil || pageID == "" {
			return nil
		}
		company := e.Company()
		if company == nil {
			return nil
		}
		// A DELETED PAGE CANNOT BE READ, so the whole space is re-walked
		// instead — which is the only way to notice a removal, since the
		// registry replace is wholesale and a page that is simply absent
		// from the next walk is how a skill goes away.
		if strings.HasSuffix(eventType, "_removed") || strings.HasSuffix(eventType, "_trashed") {
			e.syncSkillsFrom(ctx, company, func(ctx context.Context, space string) ([]skills.Page, error) {
				return confluence.SkillPages(ctx, client, space)
			})
			return nil
		}
		page, err := client.PageByID(ctx, pageID)
		if err != nil {
			return err
		}
		if !strings.EqualFold(page.Space, space) {
			return nil
		}
		e.syncSkillsFrom(ctx, company, func(ctx context.Context, space string) ([]skills.Page, error) {
			return confluence.SkillPages(ctx, client, space)
		})
		return nil
	}
}

// confluencePrompt is the knowledge base's trigger builder.
func confluencePrompt() notify.Prompt { return confluence.Prompt{} }

// startConfluenceSkillSync walks the skills space once, in the background.
//
// IN THE BACKGROUND, because it is a network walk on the boot path and a
// company must not wait on its wiki to start taking work: seats come up with
// an empty catalogue and gain it moments later, which is strictly better
// than a boot that blocks on an instance that is down.
func (e *Engine) startConfluenceSkillSync(ctx context.Context, c *Company) {
	space := e.SkillsProject(c)
	e.notify.mu.Lock()
	client := e.notify.confluence.pages
	e.notify.mu.Unlock()
	if space == "" || client == nil {
		return
	}
	go e.syncSkillsFrom(ctx, c, func(ctx context.Context, space string) ([]skills.Page, error) {
		return confluence.SkillPages(ctx, client, space)
	})
}

// Knowledge is the company's knowledge-base searcher, or nil.
//
// ONE BACKEND, and Confluence is it. The seam stays an interface anyway —
// [knowledge.Searcher] is declared by its consumers, so a second backend is
// a new implementation rather than a rewrite of everything that searches.
//
// Nil means no backend is wired, and every consumer treats that as "search
// nothing" rather than as an error — a turn must not die because a company
// has no wiki.
func (e *Engine) Knowledge() knowledge.Searcher {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	if e.notify.confluence.searcher == nil {
		// A NIL INTERFACE, not a typed nil wrapping a nil pointer: the
		// consumers check `searcher == nil`, and a typed nil passes that
		// check and then answers as though a search had run and found
		// nothing — which is indistinguishable from a real empty result
		// and hides the fact that nothing is configured.
		return nil
	}
	return e.notify.confluence.searcher
}
