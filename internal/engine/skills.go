package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/plane"
)

// The tool-skill registry, wired.
//
// # It outlives an epoch, and the org chart is why
//
// The registry's CONTENT comes from the knowledge base rather than from
// config — a skill is a page somebody wrote, and an apply that changed a
// seat's model has nothing to say about it. So the registry is built once
// per node and survives every apply; what an apply refreshes is the
// operator's ${var} map, which IS config.
//
// Rebuilding it per epoch would empty it on every apply and leave every seat
// running without its company's guidance until the next sync walk — which on
// a webhook-driven sync could be never.

// skillVariables resolves the operator's substitution map for an epoch.
//
// RESOLVED, because a value may be a ${VAR} reference: the whole point of
// the map is to carry deployment facts like a tenant URL into skill prose,
// and those are exactly the values an operator keeps out of a config file.
func skillVariables(env *config.Resolver, c *Company) map[string]string {
	declared := c.Config.SkillVariables
	if len(declared) == 0 {
		return nil
	}
	out := make(map[string]string, len(declared))
	for name, value := range declared {
		out[name] = env.Value(value)
	}
	return out
}

// refreshSkillVariables installs an epoch's substitution map.
//
// Called on every apply, boot included. A variable REMOVED by a revision
// then surfaces immediately — the registry re-checks every registered skill
// against the new map — rather than on that skill's next edit, which might
// be never.
func (e *Engine) refreshSkillVariables(c *Company) {
	e.skills.SetVariables(skillVariables(e.resolver(), c))
}

// auditSkills reports every skill whose trigger names a tool this company
// does not have.
//
// PER EPOCH, because the tool surface is what an apply changes: a revision
// that removed an MCP server silently un-triggers every skill about it, and
// nothing else in the system would say so. See [skills.Trigger.Classify] for
// why drift and a foreign stack are reported differently.
func (e *Engine) auditSkills(c *Company) {
	if e.skills.Len() == 0 || c.Tools == nil {
		return
	}
	snapshot := c.Tools.Snapshot()
	e.skills.Audit(snapshot.Names(), snapshot.MCPServers())
}

// SkillsProject is the knowledge container the sync worker walks, or "".
//
// Empty means no sync: a company with no knowledge backend, or one whose
// backend is configured without a skills project. Both are ordinary, and
// both mean the catalogue stays empty rather than the engine inventing one.
func (e *Engine) SkillsProject(c *Company) string {
	if plane := c.Config.Integrations.Plane; plane != nil && plane.Enabled {
		return plane.SkillsProjectKey()
	}
	// THE KNOWLEDGE BACKEND IS SINGLE-HOMED (config enforces Confluence
	// XOR Plane), so this is a lookup rather than a precedence: whichever
	// backend the company runs is the one holding its skills.
	if cf := c.Config.Integrations.Confluence; cf != nil {
		return cf.SkillsSpaceKey()
	}
	return ""
}

// Skills is this node's tool-skill registry.
//
// NEVER NIL: it is built with the engine, before anything can ask, and a
// company that has published no skills has an empty one rather than none.
// The difference matters at the call site — a nil registry would need a
// check on every phase build, and the one that forgot it would panic on a
// company mid-setup.
func (e *Engine) Skills() *skills.Registry { return e.skills }

// SyncSkills replaces the registry from a knowledge container's pages.
//
// ALL OR NOTHING. A caller that could not enumerate the whole container must
// report the failure rather than hand over what it managed to read: the
// replace is wholesale, so a partial walk silently DELETES every skill it
// did not reach — and a webhook-driven sync may not walk again for days.
//
// It takes rendered page text rather than a backend client, so the walk
// belongs to whoever owns the backend and this stays the one place that
// decides what a skill is.
func (e *Engine) SyncSkills(pages []skills.Page) {
	admitted, report := skills.Admit(pages)
	e.skills.Replace(admitted)
	log.Info("tool_skills_synced", "skills", len(admitted),
		"pages", report.Pages, "not_skills", report.Ordinary,
		"undecodable", len(report.Undecodable))
}

// syncSkillsFrom walks the configured skills container and replaces the
// registry.
//
// Best effort and LOUD on failure: a company whose skills did not load runs
// with agents that do not know its conventions, which looks from the outside
// like models that stopped following instructions.
func (e *Engine) syncSkillsFrom(ctx context.Context, c *Company, walk func(context.Context, string) ([]skills.Page, error)) {
	project := e.SkillsProject(c)
	if project == "" || walk == nil {
		return
	}
	pages, err := walk(ctx, project)
	if err != nil {
		log.Error("tool_skill_sync_failed", "project", project, "error", err.Error(),
			"detail", "the registry keeps whatever it already held; agents run "+
				"without this company's tool guidance until the next sync")
		return
	}
	e.SyncSkills(pages)
	e.auditSkills(c)
}

// startSkillSync walks the configured skills container once, in the
// background.
//
// IN THE BACKGROUND, because it is a network walk on the boot path and a
// company must not wait on its wiki to start taking work: seats come up with
// an empty catalogue and gain it moments later, which is strictly better
// than a boot that blocks on an instance that is down.
//
// The walk itself is ALL OR NOTHING — see [Engine.SyncSkills] — so a failure
// leaves whatever the registry already held rather than emptying it.
func (e *Engine) startSkillSync(ctx context.Context, c *Company) {
	project := e.SkillsProject(c)
	e.notify.mu.Lock()
	client := e.notify.plane.pages
	e.notify.mu.Unlock()
	if project == "" || client == nil {
		return
	}
	go e.syncSkillsFrom(ctx, c, func(ctx context.Context, project string) ([]skills.Page, error) {
		return planePages(ctx, client, project)
	})
}

// planePages resolves the container's identifier and reads its pages.
func planePages(ctx context.Context, client *plane.Client, project string) ([]skills.Page, error) {
	// The walk is PROJECT-SCOPED and the endpoint takes a UUID, so the
	// operator's identifier has to be resolved first — and a project that
	// does not resolve is a configuration problem rather than an empty
	// container, which is why it is an error rather than no pages.
	projects, err := client.Projects(ctx)
	if err != nil {
		return nil, err
	}
	var id string
	for _, p := range projects {
		if strings.EqualFold(p.Identifier, project) {
			id = p.ID
			break
		}
	}
	if id == "" {
		return nil, fmt.Errorf("engine: no project %q in this workspace", project)
	}

	rows, err := client.ListPages(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]skills.Page, 0, len(rows))
	for _, row := range rows {
		out = append(out, skills.Page{
			ID: row.ID, Title: row.Name,
			// The DECODED text, so this package stays the one place that
			// decides what a skill is and the backend stays the one place
			// that knows how its pages are shaped.
			Text: plane.DecodeSkillPage(row.HTML),
		})
	}
	return out, nil
}
