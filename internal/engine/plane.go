package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plane"
)

// The tracker, wired.
//
// # Plane has no outbound half, and that is the whole shape of this file
//
// A chat backend holds sockets and sends messages, so it is a transport with
// a lifecycle. Plane is inbound-only: webhooks arrive at the API's edge, the
// parser turns one into the notifications it implies, and an agent writes
// back through its own MCP tools rather than through the engine. So there is
// nothing to start and nothing to stop — only a parser, a prompt and a
// knowledge searcher to build and hand over.
//
// What that removes is a whole class of failure the chat side has to handle:
// there is no connection to lose, so an unreachable Plane instance degrades
// exactly one thing (the reads that enrich routing) and never takes a
// surface down.

// planeSeatEnv is the mcp_env server whose credentials belong to Plane, and
// planeSeatKey is the variable inside it holding a seat's own API key.
//
// A seat WITH one searches as itself and Plane enforces its own membership
// and page ACLs; a seat without one falls back to the engine's shared read
// credential, which may not search unscoped — see [knowledge.Permitted].
const (
	planeSeatEnv = "plane"
	planeSeatKey = "PLANE_API_KEY"
)

// planeParts is what the tracker contributes to a running company.
type planeParts struct {
	parser   *plane.Parser
	searcher *plane.Searcher

	// pages reads the skills container. Nil with no engine credential,
	// which is the same degradation the searcher takes: a company whose
	// read token lapsed keeps routing and stops learning.
	pages *plane.Client
}

// startPlane builds the tracker's parser, prompt and knowledge searcher.
//
// It returns them rather than storing them, because they belong to different
// owners: the parser and prompt join the inbound service, and the searcher is
// the company's knowledge backend. Returning nil parts is an ordinary answer
// — the integration is off, or it is on and this node could not build a
// client — and the caller runs the company without them.
func (e *Engine) startPlane(c *Company, cfg *config.Plane) (planeParts, error) {
	if cfg == nil || !cfg.Enabled {
		return planeParts{}, nil
	}
	// RESOLVED HERE, at construction, exactly as every other credential is:
	// a ${VAR} stays verbatim in the stored config, which is what makes
	// re-activating an unchanged revision pick up a rotated token.
	env := e.resolver()
	url, workspace := env.Value(cfg.URL), env.Value(cfg.Workspace)
	if url == "" || workspace == "" {
		// A reference that did not resolve, not a config gap: the
		// validator already refused an enabled Plane with neither
		// literal missing. Starting anyway would build clients pointed
		// at "" and fail at every call with a less useful message.
		return planeParts{}, fmt.Errorf(
			"engine: plane: url or workspace resolved empty (url=%q workspace=%q)",
			cfg.URL, cfg.Workspace)
	}
	skills := cfg.SkillsProjectKey()

	// THE ENGINE CLIENT IS OPTIONAL, and the degradation is documented
	// rather than fatal: with no token the parser still routes from what
	// the payload names (assignees, mentions) and only the enrichments go
	// — subscriber fan-out, project identifiers, knowledge search. A
	// company whose tracker credential lapsed keeps working, less well.
	var engineClient *plane.Client
	if token := strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		client, err := plane.NewClient(plane.ClientOptions{
			URL: url, Workspace: workspace, APIKey: token,
		})
		if err != nil {
			return planeParts{}, fmt.Errorf("engine: plane: %w", err)
		}
		engineClient = client
	} else {
		log.Warn("plane_has_no_engine_token",
			"detail", "routing falls back to payload-named targets; "+
				"subscriber fan-out and knowledge search are off")
	}

	cache := plane.NewProjectCache(engineClient, nil)
	parser, err := plane.NewParser(plane.ParserOptions{
		URL:      url,
		Projects: cache,
		// The subscriber lookup needs the engine credential: a thread's
		// followers are not in the payload. Nil degrades to the
		// payload's assignees, which the parser handles as a first-class
		// path rather than as an error.
		Subscribers: subscriberLookup(engineClient),
		Leads:       plane.LeadsFrom(c.Org),
		Excluded:    excludedProjects(skills),
		// THE INDEXER RUNS BEFORE EVERY ROUTING FILTER, which is the
		// point: the skills project's own webhooks are excluded from
		// routing, so a hook that only fired for routable events would
		// never see a skill change at all.
		OnPage: e.reindexSkill(skills),
	})
	if err != nil {
		return planeParts{}, fmt.Errorf("engine: plane: %w", err)
	}

	parts := planeParts{parser: parser}
	if engineClient != nil {
		parts.searcher = plane.NewSearcher(plane.SearcherOptions{
			Engine: engineClient, Cache: cache, SkillsProject: skills,
			ForSeat: seatPlaneClient(e.resolver(), url, workspace),
		})
	}
	// THE SKILL SYNC, on the same credential. It walks once here and is
	// refreshed by page webhooks after that, so editing a skill is a wiki
	// edit and nothing else — no restart, no deploy, no config push.
	if engineClient != nil {
		parts.pages = engineClient
	}

	log.Info("plane_wired", "url", url, "workspace", workspace,
		"projects_with_leads", len(plane.LeadsFrom(c.Org)),
		"skills_project", skills, "engine_token", engineClient != nil)
	return parts, nil
}

// reindexSkill re-reads one changed page into the skill registry.
//
// The LIVE half of the sync: the boot walk seeds the registry and this keeps
// it in step, so editing a skill is opening the page, editing and saving.
//
// It fetches through the PROJECT-SCOPED page read, which is the admission
// check as well: the fork 404s for a page in another project, so a page
// moved OUT of the skills container comes back missing and is evicted rather
// than continuing to serve its last body. Same for an archived one — archive
// is the operator's remove gesture, since Plane requires it before a delete.
func (e *Engine) reindexSkill(project string) plane.PageIndexer {
	if project == "" {
		return nil
	}
	return func(ctx context.Context, eventType, pageID string) error {
		e.notify.mu.Lock()
		client := e.notify.plane.pages
		e.notify.mu.Unlock()
		if client == nil || pageID == "" {
			return nil
		}
		projectID, err := e.skillsProjectID(ctx, client, project)
		if err != nil || projectID == "" {
			return err
		}

		if strings.HasSuffix(eventType, ".deleted") {
			e.evictSkillPage(pageID)
			return nil
		}
		page, err := client.GetPage(ctx, projectID, pageID)
		if err != nil {
			// MISSING IS AN EVICTION, not a failure: a page in another
			// project, an archived one, a deleted one — all of them mean
			// this page must stop serving a skill, and a caller that
			// treated it as an error would leave the last body in place.
			e.evictSkillPage(pageID)
			//nolint:nilerr // Deliberate: see the paragraph above.
			return nil
		}
		text := plane.DecodeSkillPage(page.HTML)
		if text == "" {
			// Edited into a non-skill. Evicted rather than skipped, or a
			// page whose trigger somebody removed keeps gating tools.
			e.evictSkillPage(pageID)
			return nil
		}
		skill, err := skills.Parse(text, skills.Source{PageID: page.ID})
		if err != nil {
			log.Warn("skill_page_undecodable", "page", pageID,
				"title", page.Name, "error", err.Error())
			e.evictSkillPage(pageID)
			return nil
		}
		if err := e.skills.Upsert(skill); err != nil {
			log.Warn("skill_refused", "page", pageID, "error", err.Error())
			return nil
		}
		log.Info("skill_reindexed", "page", pageID, "skill", skill.Key)
		return nil
	}
}

// evictSkillPage removes whichever skill came from a page.
//
// BY PAGE, not by key: the caller has a page id and the registry is keyed by
// skill key, and a page whose key was edited would otherwise leave the old
// key behind for ever.
func (e *Engine) evictSkillPage(pageID string) {
	for _, s := range e.skills.All() {
		if s.SourcePageID == pageID {
			if e.skills.Evict(s.Key) {
				log.Info("skill_evicted", "page", pageID, "skill", s.Key)
			}
		}
	}
}

// skillsProjectID resolves the skills container's identifier to its UUID.
func (e *Engine) skillsProjectID(ctx context.Context, client *plane.Client, project string) (string, error) {
	projects, err := client.Projects(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if strings.EqualFold(p.Identifier, project) {
			return p.ID, nil
		}
	}
	return "", nil
}

// subscriberLookup is the thread fan-out, or nil with no engine credential.
//
// TYPED NIL IS THE TRAP HERE: returning (*plane.SubscriberLookup)(nil) as a
// plane.Subscribers gives an interface that is non-nil and panics on call, so
// the parser's `p.subs != nil` degradation check would pass and then crash on
// the first threaded update. The nil has to be produced at the interface.
func subscriberLookup(client *plane.Client) plane.Subscribers {
	if client == nil {
		return nil
	}
	return &plane.SubscriberLookup{Client: client}
}

// excludedProjects is what must not produce notifications at all.
//
// Only the tool-skills project today, and it is a LIST rather than a scalar
// because the parser's rule is "these projects route to nobody" — a shape
// that does not have to change when a second such project exists.
func excludedProjects(skills string) []string {
	if skills == "" {
		return nil
	}
	return []string{skills}
}

// seatPlaneClient resolves a seat's OWN Plane credential.
//
// A seat with one searches as itself, and Plane's own membership and page
// ACLs then bound what comes back — which is what makes an unscoped search
// safe for it. The clients are CACHED per key: a search happens on the Plan
// phase's hot path, and minting an HTTP client per turn would build a fresh
// connection pool each time and throw away every keep-alive.
func seatPlaneClient(env *config.Resolver, url, workspace string) func(*org.Role) (*plane.Client, bool) {
	var (
		mu       sync.Mutex
		byAPIKey = map[string]*plane.Client{}
	)
	return func(seat *org.Role) (*plane.Client, bool) {
		if seat == nil {
			return nil, false
		}
		key := strings.TrimSpace(env.Value(
			seat.MCPEnv[planeSeatEnv][planeSeatKey]))
		if key == "" {
			return nil, false
		}
		mu.Lock()
		defer mu.Unlock()
		if client, ok := byAPIKey[key]; ok {
			return client, true
		}
		client, err := plane.NewClient(plane.ClientOptions{
			URL: url, Workspace: workspace, APIKey: key,
			// A seat's own client gets its own transport rather than
			// sharing the engine's: they authenticate as different
			// principals, and a shared pool would let a connection
			// opened for one be reused for the other.
			HTTP: &http.Client{},
		})
		if err != nil {
			// The seat searches on the engine credential instead,
			// which is the same answer as having no key at all.
			log.Warn("plane_seat_client_failed", "seat", seat.Handle(),
				"error", err.Error())
			return nil, false
		}
		byAPIKey[key] = client
		return client, true
	}
}

// reconcilePlane rebuilds the tracker for a newly applied epoch.
//
// REBUILT WHOLE rather than patched, because every input can change: the
// instance url, the read credential, which project the tool skills live in,
// and — most often — the org chart the lead map is derived from. A node that
// kept its boot-time parser would route a new revision's events by the old
// company's leads, and that failure is silent: every event still routes, just
// to whoever led the project when the process started.
//
// The cost is a cold project cache and a fresh seat-client pool per apply.
// That is a minute of refetches on a path whose floor is a minute anyway,
// against a correctness bug an operator cannot see — an easy trade.
//
// It never takes the tracker DOWN. A revision that turns Plane off leaves the
// previous parser registered until the process restarts, which is the same
// posture the chat surface has: an integration disappearing from a config is
// far more often a mistake being made than a decommission being performed,
// and the difference costs one restart to settle.
func (e *Engine) reconcilePlane(c *Company) {
	cfg := c.Config.Integrations.Plane
	if cfg == nil || !cfg.Enabled {
		return
	}
	e.notify.mu.Lock()
	svc, running := e.notify.service, e.notify.plane.parser != nil
	e.notify.mu.Unlock()
	if svc == nil || !running {
		// Not started, or started without a tracker. Boot owns that
		// case; re-running it here would race the boot path.
		return
	}

	parts, err := e.startPlane(c, cfg)
	if err != nil || parts.parser == nil {
		// THE PREVIOUS TRACKER KEEPS RUNNING. A revision whose Plane
		// block is broken must not leave the company with no tracker at
		// all — the old one routes by a stale org chart, which is worse
		// than the new one and much better than nothing.
		log.Error("plane_reconcile_failed", "error", errorText(err),
			"detail", "the previous tracker wiring is still current")
		return
	}
	if err := svc.Replace(parts.parser, planePrompt()); err != nil {
		log.Error("plane_reconcile_failed", "error", err.Error(),
			"detail", "the previous tracker wiring is still current")
		return
	}
	e.notify.mu.Lock()
	e.notify.plane = parts
	e.notify.mu.Unlock()
	log.Info("plane_reconciled", "company", c.Config.Name)
}

// errorText renders an error for a log line, including the "it built but
// produced nothing" case that carries no error of its own.
func errorText(err error) string {
	if err == nil {
		return "the tracker produced no parser"
	}
	return err.Error()
}

// Knowledge is the company's knowledge-base searcher, or nil.
//
// EXACTLY ONE BACKEND per org, which the config validator already enforces
// (Confluence XOR Plane), so this is a lookup rather than a choice. Nil means
// no backend is wired, and every consumer treats that as "search nothing"
// rather than as an error — a turn must not die because a company has no
// wiki.
func (e *Engine) Knowledge() knowledge.Searcher {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	if e.notify.confluence.searcher != nil {
		return e.notify.confluence.searcher
	}
	if e.notify.plane.searcher == nil {
		// A NIL INTERFACE, not a typed nil wrapping a nil pointer: the
		// consumers check `searcher == nil`, and a typed nil passes that
		// check and then answers as though a search had run and found
		// nothing — which is indistinguishable from a real empty result
		// and hides the fact that nothing is configured.
		return nil
	}
	return e.notify.plane.searcher
}

// planePrompt is the tracker's trigger builder. A value, held by nothing.
func planePrompt() notify.Prompt { return plane.Prompt{} }
