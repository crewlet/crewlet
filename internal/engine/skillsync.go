package engine

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/skillsync"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/doclet"
)

// The tool-skill sync, wired. Which wiki, which container in it, and whether
// there is one are company config, so every apply re-derives the source and
// hands it to the node's one sync loop (package skillsync).

// newSkillSync builds the node's sync loop over its registry, stopped: built
// with the engine rather than at start, so Doclet's applier nudge, wired
// earlier, has a loop to nudge. A request made before Start is kept.
func (e *Engine) newSkillSync(nodeID string) error {
	opts := skillsync.Options{
		Registry: e.skills, Node: nodeID, OnChange: e.auditSkills,
	}
	// A NIL INTERFACE, never a typed nil wrapping one: the loop reads
	// `Stream == nil` as "no broker, nobody to tell".
	if e.backends != nil && e.backends.Queue != nil {
		opts.Stream = e.backends.Queue
	}
	syncer, err := skillsync.New(opts)
	if err != nil {
		return err
	}
	e.skillSync = syncer
	return nil
}

// startSkillSync runs the loop and attaches the fleet nudge. Before the
// notification service, whose Confluence parser hands this loop every page
// change: started after it, the changes in between would be dropped.
func (e *Engine) startSkillSync(ctx context.Context) { e.skillSync.Start(ctx) }

// stopSkillSync ends the loop. Nil-safe, for an engine whose boot failed
// before building one.
func (e *Engine) stopSkillSync(ctx context.Context) { e.skillSync.Stop(ctx) }

// reconcileSkills points the sync loop at the skills source an epoch names,
// on every apply and at boot; the loop decides the cost (a moved source is
// walked and the old one's skills retired, a failed one walked again, an
// unchanged healthy one costs nothing). After the knowledge base's own
// reconcile, since the Confluence source reads the wiring it left running.
func (e *Engine) reconcileSkills(c *Company) {
	e.skillSync.SetSource(e.skillSource(c))
}

// skillSource is where an epoch's skills are read from on this node.
func (e *Engine) skillSource(c *Company) skillsync.Source {
	if c == nil || c.Config == nil {
		return skillsync.Source{}
	}
	backend := c.Config.KnowledgeBackendFor()
	src := skillsync.Source{Backend: skillBackend(backend), Container: e.SkillsContainer(c)}
	if src.Container == "" {
		return src
	}
	switch backend {
	case config.KnowledgeConfluence:
		return e.confluenceSkillSource(src)
	case config.KnowledgeDoclet:
		run := e.nativeHalves()
		switch {
		case !run.wiki:
			src.Unreadable = "Doclet is not running on this node: it starts " +
				"with the node, so a node that booted on another knowledge " +
				"backend serves Doclet's skills after it restarts"
		case run.own == nil:
			// THROUGH A DATA NODE, which answers only once its own copy
			// is established: the wait a local walk does for itself.
			src.Walk = e.walkRemoteSkills
		default:
			src.Walk = e.walkDocletSkills
		}
		return src
	default:
		src.Unreadable = "knowledge.backend " + string(backend) + " has no " +
			"tool-skill source this build can read"
		return src
	}
}

// skillBackend is the backend a skill's page is read under: the name every
// record of a seat reading that page states ([doclet.Backend] for Doclet's
// pages), so a skill's reads and the page's other reads are one page to
// every reader. A backend whose pages state its config value states that.
func skillBackend(backend config.KnowledgeBackend) string {
	if backend == config.KnowledgeDoclet {
		return doclet.Backend
	}
	return string(backend)
}

// confluenceSkillSource is the Confluence half of [Engine.skillSource]. It
// reads the RUNNING wiring, not the epoch's block: a revision whose block is
// broken keeps the previous parser and client, and skills follow them rather
// than go dark.
func (e *Engine) confluenceSkillSource(src skillsync.Source) skillsync.Source {
	e.notify.mu.Lock()
	parts := e.notify.confluence
	e.notify.mu.Unlock()
	if parts.parser == nil {
		src.Unreadable = "the Confluence integration is not wired on this node " +
			"(see confluence_unavailable or confluence_reconcile_failed in the log " +
			"for why it did not start)"
		return src
	}
	src.Container, src.Location = parts.skillsSpace, parts.base
	if src.Container == "" {
		return src
	}
	client := parts.pages
	if client == nil {
		src.Unreadable = "integrations.confluence has no token, and the " +
			"skills space is read with the organization's credential: set " +
			"integrations.confluence.token"
		return src
	}
	src.Walk = func(ctx context.Context, space string) ([]skills.Page, error) {
		return confluence.SkillPages(ctx, client, space)
	}
	src.Page = func(ctx context.Context, id string) (skillsync.PageRead, error) {
		page, space, err := confluence.SkillPage(ctx, client, id)
		switch {
		case confluence.Status(err) == http.StatusNotFound:
			// GONE (deleted or trashed) is an answer, not a failure,
			// which would walk the whole space to learn the same.
			return skillsync.PageRead{}, nil
		case err != nil:
			return skillsync.PageRead{}, err
		}
		return skillsync.PageRead{Page: page, Container: space, Exists: true}, nil
	}
	return src
}
