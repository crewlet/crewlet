package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skillsync"
	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The knowledge base, wired: a PARSER that routes page events (broken, it costs
// notifications) and a SEARCHER behind every turn's knowledge block (broken, it
// costs every seat its company's knowledge, silently: an empty block looks like
// a company that wrote nothing). So the org credential is optional for routing
// (a page's mentions are in the payload), REQUIRED for search, and the two are
// reported separately.

// confluenceParts is what the knowledge base contributes to a company.
type confluenceParts struct {
	parser   *confluence.Parser
	searcher *confluence.Searcher

	// pages reads the skills space; nil with no org credential, as the
	// searcher is.
	pages *confluence.Client

	// base and skillsSpace identify the skills source ([Engine.skillSource]).
	// Held here, not re-derived from the epoch: a broken revision keeps the
	// PREVIOUS wiring running, and the skills follow the running wiring.
	base, skillsSpace string
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
	declared := c.Config.Integrations.Atlassian.Declared()
	if base == "" {
		// A reference that did not resolve (the validator refused a block
		// with neither literal); starting anyway points every read at "".
		return confluenceParts{}, fmt.Errorf(
			"engine: confluence: neither url (%q) nor cloud_id (%q) resolved "+
				"to anything", cfg.URL, cfg.CloudID)
	}
	site := resolved.ShareableBaseURL()
	skillsSpace := c.Config.SkillsContainerKey()

	// Without the org credential only a seat with its own searches, and
	// routing is unaffected, so this warns rather than refuses.
	var orgClient *confluence.Client
	if token := strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: base, Email: env.Value(cfg.Email), Token: token, Declared: declared,
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
			// RUNS BEFORE EVERY ROUTING FILTER: routing excludes the skills
			// space's own events, so after it no skill change would be seen.
			OnPage: e.noteConfluencePage,
			// watchersFor, never the *pageWatchers itself: a nil pointer in
			// the interface field slips past the parser's `watchers == nil`
			// check and is called through a nil receiver.
			Watchers: watchersFor(e.confluenceWatchers()),
		}),
		pages: orgClient, base: base, skillsSpace: skillsSpace,
	}
	if orgClient != nil {
		parts.searcher = confluence.NewSearcher(confluence.SearcherOptions{
			Org: orgClient, ForSeat: seatConfluenceClient(env, base, declared),
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
// Both parts derive from the company, and two inputs move on an apply: the
// SPACE-LEAD MAP (a kept parser routes page activity to the old chart's lead,
// silently) and the CREDENTIAL (after a rotation the old client 401s and the
// knowledge block goes empty with nothing saying why).
func (e *Engine) reconcileConfluence(c *Company) {
	cfg := c.Config.Integrations.Confluence
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		// No edge running: a node's first company starts one instead
		// ([Engine.startInbound]).
		return
	}
	// CONVERGES BOTH WAYS: it builds a parser even when none runs (a
	// revision that adds Confluence after boot, or reconnects it, must not
	// leave deliveries stored and routed to nobody), and retires it when the
	// revision drops the block, so nothing keeps routing under a credential
	// being revoked. Unlike the other reconcilers the SEARCHER goes too: the
	// turn-start prefetch would otherwise keep searching a removed wiki.
	if cfg == nil {
		e.notify.mu.Lock()
		e.notify.confluence = confluenceParts{}
		e.notify.mu.Unlock()
		if svc.Unregister(confluence.Backend) {
			log.Info("confluence_retired",
				"detail", "the revision no longer declares confluence; page "+
					"activity routes to no seat and knowledge search is off")
		}
		return
	}

	parts, err := e.startConfluence(c, cfg)
	if err != nil || parts.parser == nil {
		// THE PREVIOUS WIRING KEEPS RUNNING: a stale credential beats no
		// knowledge base at all.
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
func seatConfluenceClient(env *config.Resolver, base string, declared atlassianhost.Deployment) confluence.SeatClient {
	return func(seat *org.Role) (*confluence.Client, bool) {
		if seat == nil || seat.IsHuman() {
			return nil, false
		}
		// THROUGH THE ONE READER BOTH PRODUCTS SHARE
		// ([atlassian.CredentialAt]), so Confluence and Jira never disagree
		// on whether a seat holds a credential.
		cred := atlassian.CredentialOf(atlassian.ProductConfluence, seat, env.Value)
		if !cred.Held() {
			return nil, false
		}
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: base, Email: cred.Email, Token: cred.Token, Declared: declared,
		})
		if err != nil {
			log.Warn("confluence_seat_client_failed", "seat", seat.Handle(),
				"error", err.Error())
			return nil, false
		}
		return client, true
	}
}

// noteConfluencePage hands a page change the parser heard to the skill sync.
//
// Called for every page in EVERY space: a page that left the skills space is
// announced from its new one, and only the registry knows it held a skill. Any
// read runs on the sync's own loop, so a slow wiki never holds the consumer.
func (e *Engine) noteConfluencePage(ctx context.Context, change confluence.PageChange) error {
	return e.skillSync.PageChanged(ctx, skillsync.Change{
		Backend: confluence.Backend, Container: change.Space, PageID: change.PageID,
	})
}

// confluencePrompt is the knowledge base's trigger builder.
func confluencePrompt() notify.Prompt { return confluence.Prompt{} }

// Knowledge is the company's knowledge-base searcher, or nil.
//
// EXACTLY ONE per company (`knowledge.backend` picks it, and config refuses
// two: an answer must not depend on
// which searcher was asked), so trying Doclet first is a nil check, not a
// preference. Nil means none is wired, which consumers treat as "search
// nothing", never an error.
func (e *Engine) Knowledge() knowledge.Searcher {
	// A NIL INTERFACE, never a typed nil: consumers check `searcher == nil`,
	// and a typed nil would answer like a search that found nothing.
	// Doclet is the router's on every node running it: a node with the
	// index answers from it, one without searches through one that has it.
	// A data node's runtime builds its searcher, so run.wiki implies one.
	if run := e.nativeHalves(); run.wiki && e.router != nil {
		return e.router.Knowledge()
	}
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	if e.notify.confluence.searcher == nil {
		return nil
	}
	return e.notify.confluence.searcher
}
