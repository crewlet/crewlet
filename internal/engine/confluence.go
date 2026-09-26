package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skillsync"
	"github.com/crewlet/crewlet/internal/atlassian"
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
// SEARCHER answers a turn's "what do we already know about this",
// and a broken one costs every seat its company's written knowledge on every
// turn — silently, because an empty knowledge block is indistinguishable
// from a company that has written nothing down.
//
// So the org credential is optional for routing (a page's mentions are in
// the payload) and REQUIRED for search, and the two are reported separately.

// confluenceParts is what the knowledge base contributes to a company.
type confluenceParts struct {
	parser   *confluence.Parser
	searcher *confluence.Searcher

	// pages reads the skills space. Nil with no org credential, which is
	// the same degradation the searcher takes: a company whose read token
	// lapsed keeps routing and stops learning.
	pages *confluence.Client

	// base and skillsSpace are the instance and the skills space this
	// wiring was built for: the identity of the skills source, read by
	// [Engine.skillSource]. Held with the parts rather than re-derived from
	// the epoch because a revision whose Confluence block is broken keeps
	// the PREVIOUS wiring running, and the skills have to follow the wiring
	// that is actually running rather than the one that failed to build.
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
	if base == "" {
		// A reference that did not resolve: the validator already refused
		// a block with neither literal, and starting anyway would point
		// every read at "" and fail with a much less useful message.
		return confluenceParts{}, fmt.Errorf(
			"engine: confluence: neither url (%q) nor cloud_id (%q) resolved "+
				"to anything", cfg.URL, cfg.CloudID)
	}
	site := resolved.ShareableBaseURL()
	skillsSpace := c.Config.SkillsContainerKey()

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
			OnPage: e.noteConfluencePage,
			// A TYPED NIL WOULD NOT BE NIL. confluenceWatchers returns a
			// *pageWatchers, and assigning one straight into an interface
			// field makes a non-nil interface holding a nil pointer — so
			// the parser's `watchers == nil` check would pass and every
			// call would go through a nil receiver.
			Watchers: watchersFor(e.confluenceWatchers()),
		}),
		pages: orgClient, base: base, skillsSpace: skillsSpace,
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
// every read, and a turn's knowledge block goes empty with nothing saying
// why.
//
// The tracker beside this one is reconciled for the first reason, and this
// package needed the same edge from the moment it had a lead map.
func (e *Engine) reconcileConfluence(c *Company) {
	cfg := c.Config.Integrations.Confluence
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		// Nothing to register a parser with. An apply reconciles only a
		// running edge and starts one for a node's first company instead
		// (see [Engine.startInbound]).
		return
	}
	// IT REVIVES AS WELL AS RETIRES, and it did not.
	//
	// This returned early unless a parser was ALREADY running, on the
	// reasoning that boot owns the first build. Boot owns the first one and
	// nothing owned the second: the parser set is assembled once, in New,
	// so a revision that ADDS Confluence after boot found no parser
	// running, took this branch, and registered nothing. Disconnecting and
	// reconnecting is exactly that sequence, and it left the route
	// verifying and storing every delivery while nothing turned one into
	// work for a seat, until the process was restarted.
	//
	// A reconciler that converges in one direction is not a reconciler.
	// The other three here have always had this shape; this one is the odd
	// case because it also owns a searcher, which is what the guard was
	// really protecting and which [startConfluence] rebuilds anyway.
	// RETIRED when the revision no longer declares it, like the other
	// three reconcilers — each converged only toward "configured", so
	// removing the block applied cleanly and left the boot-time parser
	// routing page activity under the credential being revoked.
	//
	// Confluence needs one step the others do not: the SEARCHER goes as
	// well. It is what the turn-start knowledge prefetch reads through, so
	// leaving it would have every seat go on searching a wiki the company
	// has removed, using the same credential.
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
		// THROUGH THE ONE READER BOTH PRODUCTS SHARE. This walked a
		// confluence-only list of servers and key spellings, which is how a
		// seat holding `mcp_env.atlassian.JIRA_API_TOKEN` — the spelling
		// atlassian.PlanFor's own note tells operators to write — searched
		// Confluence as the ORG account while Jira reported it ready. See
		// [atlassian.CredentialAt].
		cred := atlassian.CredentialOf(atlassian.ProductConfluence, seat, env.Value)
		if !cred.Held() {
			return nil, false
		}
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: base, Email: cred.Email, Token: cred.Token,
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
// CALLED FOR EVERY PAGE CHANGE IN EVERY SPACE, and the sync decides whether it
// concerns the registry: a page that left the skills space is announced from
// the space it moved to, and only the registry knows it held a skill. The
// read, when there is one, happens on the sync's own loop rather than on the
// delivery's goroutine, so a slow wiki never holds the inbound consumer.
func (e *Engine) noteConfluencePage(ctx context.Context, change confluence.PageChange) error {
	return e.skillSync.PageChanged(ctx, skillsync.Change{
		Backend: confluence.Backend, Container: change.Space, PageID: change.PageID,
	})
}

// confluencePrompt is the knowledge base's trigger builder.
func confluencePrompt() notify.Prompt { return confluence.Prompt{} }

// Knowledge is the company's knowledge-base searcher, or nil.
//
// EXACTLY ONE per company, which is the seam's own rule and not a limitation
// of this function: "what do we already know about this" must not depend on
// which searcher was asked, so `knowledge.backend` picks one and config
// refuses a company that configures two. The NATIVE one is answered first
// because it is the default; a company on `backend: confluence` has no
// native projector at all, so the branch is a nil check rather than a
// preference.
//
// Nil means no backend is wired, and every consumer treats that as "search
// nothing" rather than as an error — a turn must not die because a company
// has no wiki.
func (e *Engine) Knowledge() knowledge.Searcher {
	// A NIL INTERFACE, never a typed nil wrapping a nil pointer: the
	// consumers check `searcher == nil`, and a typed nil passes that check
	// and then answers as though a search had run and found nothing —
	// indistinguishable from a real empty result, and it hides the fact
	// that nothing is configured.
	if native := e.NativeSearcher(); native != nil {
		return native
	}
	if r := e.remote.Load(); r != nil && r.wiki {
		// A NODE THAT HOLDS NO INDEX searches through one that does.
		return r.client.Knowledge()
	}
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	if e.notify.confluence.searcher == nil {
		return nil
	}
	return e.notify.confluence.searcher
}
