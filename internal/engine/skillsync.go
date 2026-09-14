package engine

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/skillsync"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
)

// The tool-skill sync, wired.
//
// # Which knowledge backend the skills come from is config
//
// The registry's content is a wiki's, but WHICH wiki, which container in it,
// and whether there is one at all are all in the company document. So every
// apply re-derives the source and hands it to the node's one sync loop, which
// walks a source that changed, retires the skills of one that went away, and
// leaves an unchanged one alone. See package skillsync for the loop itself.

// newSkillSync builds the node's sync loop over its registry, stopped.
//
// Built with the engine rather than when it starts, so the native projection's
// post-commit nudge, which is wired before the loop runs, has something to
// nudge: a request made before the loop starts is kept and acted on once it
// does.
func (e *Engine) newSkillSync(nodeID string) error {
	opts := skillsync.Options{
		Registry: e.skills, Node: nodeID, OnChange: e.auditCurrentSkills,
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

// startSkillSync runs the loop and attaches the fleet nudge.
//
// BEFORE the notification service, whose Confluence parser hands this loop
// every page change it hears: a parser that ran first would drop the changes
// that arrived in between on a loop that was not there to take them.
func (e *Engine) startSkillSync(ctx context.Context) { e.skillSync.Start(ctx) }

// stopSkillSync ends the loop. Nil-safe, which is the engine that never built
// one because its boot failed first.
func (e *Engine) stopSkillSync(ctx context.Context) { e.skillSync.Stop(ctx) }

// reconcileSkills points the sync loop at the skills source an epoch names.
//
// ON EVERY APPLY, and at boot. The loop decides what that costs: a source that
// moved is walked (and the previous one's skills are retired at once), a source
// whose last walk failed is walked again, and an unchanged healthy source costs
// nothing. Before it was wired here, no apply reached the registry at all, so
// connecting Confluence live loaded no skills, moving the skills container kept
// serving the old one's, and disconnecting the wiki left every skill it had
// registered for the life of the process.
//
// AFTER the knowledge base's own reconcile, because the Confluence source is
// read off the wiring that reconcile left running.
func (e *Engine) reconcileSkills(c *Company) {
	e.skillSync.SetSource(e.skillSource(c))
}

// skillSource is where an epoch's skills are read from on this node.
func (e *Engine) skillSource(c *Company) skillsync.Source {
	if c == nil || c.Config == nil {
		return skillsync.Source{}
	}
	backend := c.Config.KnowledgeBackendFor()
	src := skillsync.Source{Backend: string(backend), Container: e.SkillsContainer(c)}
	if src.Container == "" {
		return src
	}
	switch backend {
	case config.KnowledgeConfluence:
		return e.confluenceSkillSource(src)
	case config.KnowledgeNative:
		if e.native == nil || e.native.pageReader == nil {
			src.Unreadable = "the native knowledge base is not running on this " +
				"node: it starts with the node, so a node that booted on another " +
				"knowledge backend serves native skills after it restarts"
			return src
		}
		src.Walk = e.walkNativeSkills
		return src
	default:
		src.Unreadable = "knowledge.backend " + string(backend) + " has no " +
			"tool-skill source this build can read"
		return src
	}
}

// confluenceSkillSource is the Confluence half of [Engine.skillSource].
//
// THE WIRING THAT IS RUNNING, not the block the epoch declares: a revision
// whose Confluence block is broken keeps the previous parser and client, and
// the skills follow the same wiring rather than going dark over a block that
// never built.
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
			// GONE, deleted or in the trash. An answer rather than a
			// failure: the page holds nothing, and a failure would walk
			// the whole space to learn the same thing.
			return skillsync.PageRead{}, nil
		case err != nil:
			return skillsync.PageRead{}, err
		}
		return skillsync.PageRead{Page: page, Container: space, Exists: true}, nil
	}
	return src
}
