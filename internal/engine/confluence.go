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
// SEARCHER answers a turn's "what do we already know about this", and a
// broken one costs every seat its company's written knowledge on every turn.
//
// So the org credential is optional for routing (a page's mentions are in
// the payload) and REQUIRED for search, and the two are reported separately.

// confluenceParts is what the knowledge base contributes to a company.
type confluenceParts struct {
	parser   *confluence.Parser
	searcher *confluence.Searcher

	// pages reads the skills space, and is what a promotion draft is
	// written through. Nil wherever the searcher is — with no org
	// credential, and for a company whose knowledge base is not Confluence —
	// so a company whose read token lapsed keeps routing and stops learning.
	pages *confluence.Client

	// base and skillsSpace are the instance and the skills space this
	// wiring was built for: the identity of the skills source, read by
	// [Engine.skillSource]. Held with the parts rather than re-derived from
	// the epoch because a revision whose Confluence block is broken keeps
	// the PREVIOUS wiring running, and the skills have to follow the wiring
	// that is actually running rather than the one that failed to build.
	base, skillsSpace string
}

// startConfluence builds the parser for a company that declares the block,
// and the searcher and the org client for one whose knowledge base it is.
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

	// THE READING HALF IS BUILT ONLY FOR A COMPANY WHOSE KNOWLEDGE BASE THIS
	// IS. A company on `backend: none` may declare the block for its page
	// routing alone, and a searcher or a promotion writer built for it would
	// read or write a wiki the company says it does not run. [Engine.Knowledge]
	// answers by the same field, so it would never ask such a searcher — this
	// is what keeps one from existing, and the promotion pass, which asks
	// only whether an org client is wired, from drafting into it.
	reads := c.Config.KnowledgeBackendFor() == config.KnowledgeConfluence

	// THE ORG CREDENTIAL IS WHAT SEARCH RESTS ON, as the file head says:
	// without it no searcher is built, so no seat searches — not even one
	// holding a credential of its own — and the tool-skill walk cannot run.
	// Routing is unaffected either way, which is why this warns rather than
	// refusing.
	var orgClient *confluence.Client
	token := strings.TrimSpace(env.Value(cfg.Token))
	switch {
	case !reads:
	case token == "":
		log.Warn("confluence_has_no_org_token",
			"detail", "no knowledge search runs, for any seat, and the "+
				"tool-skill walk cannot run; page activity still routes. "+
				"Set integrations.confluence.token")
	default:
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: base, Email: env.Value(cfg.Email), Token: token,
		})
		if err != nil {
			return confluenceParts{}, fmt.Errorf("engine: confluence: %w", err)
		}
		orgClient = client
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
		"knowledge_base", reads, "searches", parts.searcher != nil)
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
// every read, so every knowledge search the node runs fails.
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
	// IT CONVERGES IN BOTH DIRECTIONS. The parser set is assembled once,
	// in New, so a revision that ADDS Confluence after boot finds no
	// parser running, and one that removes it finds one: disconnecting and
	// reconnecting is that sequence, and a reconciler that handled only
	// one direction would leave the route verifying and storing every
	// delivery while nothing turned one into work for a seat, or leave the
	// boot-time parser routing page activity under the credential being
	// revoked.
	//
	// RETIRED when the revision no longer declares it, and the SEARCHER
	// goes as well: it is what the knowledge search reads through, so
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

// Knowledge is the searcher of the knowledge base the CURRENT epoch runs, or
// nil.
//
// EXACTLY ONE per company, which is the seam's own rule and not a limitation
// of this function: "what do we already know about this" must not depend on
// which searcher was asked, so `knowledge.backend` picks one.
//
// # It answers by that field, never by which searchers exist
//
// This node can hold both at once. The native searcher belongs to the NODE:
// it starts with the node's native backends and lives as long as they do,
// whatever a later revision says. The Confluence one is built for a revision
// on `backend: confluence` ([Engine.startConfluence]), and a later revision
// whose Confluence block fails to build leaves it running
// ([Engine.reconcileConfluence]). Answered by which searcher exists instead, a
// company that moved off the native knowledge base by a live apply would go
// on searching its pages until the node restarted, and one that moved off
// Confluence onto a block that did not build would go on searching the wiki
// it left.
//
// A company moved ONTO the native knowledge base is answered natively only by
// a node whose boot revision ran it, which is the node that still holds the
// searcher; a node that booted on another backend gets nil here until it
// restarts, because the native backends start with the node. Nil is honest
// there — that node holds no copy of the pages to search.
//
// Nil means no backend is wired, and every consumer treats that as "search
// nothing" rather than as an error — a turn must not die because a company
// has no wiki. [Engine.KnowledgeServed] is the same answer with the reason
// beside it.
func (e *Engine) Knowledge() knowledge.Searcher {
	searcher, _ := e.KnowledgeServed()
	return searcher
}

// KnowledgeServed is [Engine.Knowledge] and, when nothing answers, which of
// the two reasons it is: the company runs no knowledge base, or it runs one
// this node is not serving.
//
// ONE ANSWER RATHER THAN A SEARCHER AND A SEPARATE STATUS, because the two
// are read together — a reader told "no knowledge backend is configured" about
// a company that configured one goes and checks a setting that is already
// right — and two reads could straddle an apply and disagree.
func (e *Engine) KnowledgeServed() (knowledge.Searcher, knowledge.Refusal) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil, knowledge.Refusal{State: knowledge.NoBackend,
			Detail: "no company configuration is active on this node"}
	}
	// A NIL INTERFACE, never a typed nil wrapping a nil pointer: the
	// consumers check `searcher == nil`, and a typed nil passes that check
	// and then answers as though a search had run and found nothing —
	// indistinguishable from a real empty result, and it hides the fact
	// that nothing is configured.
	switch c.Config.KnowledgeBackendFor() {
	case config.KnowledgeNative:
		if native := e.NativeSearcher(); native != nil {
			return native, knowledge.Refusal{}
		}
		return nil, knowledge.Refusal{State: knowledge.NotServed,
			Detail: "the company's knowledge base is the engine's own " +
				"(`knowledge.backend: native`), and this node started without " +
				"it, so it holds no copy of the pages to search; a restart " +
				"starts it"}
	case config.KnowledgeConfluence:
		e.notify.mu.Lock()
		searcher := e.notify.confluence.searcher
		e.notify.mu.Unlock()
		if searcher != nil {
			return searcher, knowledge.Refusal{}
		}
		return nil, knowledge.Refusal{State: knowledge.NotServed,
			Detail: e.confluenceUnserved(c.Config.Integrations.Confluence)}
	}
	return nil, knowledge.Refusal{State: knowledge.NoBackend,
		Detail: "the company runs no knowledge base (`knowledge.backend: none`)"}
}

// confluenceUnserved says why a company on Confluence has no searcher here.
//
// THE CREDENTIAL IS NAMED WHEN IT IS THE CAUSE, because it is the one cause
// this can read for itself: [Engine.startConfluence] builds no searcher
// without the org token, and logs `confluence_has_no_org_token` when it does
// not. Anything else is a connection that failed to build, whose own log line
// carries the reason.
func (e *Engine) confluenceUnserved(cfg *config.Confluence) string {
	// NIL ONLY FOR A REVISION THE VALIDATOR REFUSES (`knowledge.backend:
	// confluence` with no block), which cannot be applied — answered
	// rather than dereferenced, because this runs on a search path.
	if cfg == nil {
		return "the company's knowledge base is Confluence, and it declares no " +
			"`integrations.confluence` block"
	}
	if strings.TrimSpace(e.resolver().Value(cfg.Token)) == "" {
		return "the company's knowledge base is Confluence, and " +
			"`integrations.confluence.token` resolves to no credential, so no " +
			"search runs for anybody; set it"
	}
	return "the company's knowledge base is Confluence, and its connection did " +
		"not build on this node — the `confluence_unavailable` or " +
		"`confluence_reconcile_failed` line in this node's log says why"
}
