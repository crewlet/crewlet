package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
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

// skillVariables resolves the operator's substitution map for an epoch, plus
// the one variable the engine reserves.
//
// RESOLVED, because a value may be a ${VAR} reference: the whole point of
// the map is to carry deployment facts like a tenant URL into skill prose,
// and those are exactly the values an operator keeps out of a config file.
//
// # Why crewlet_base_url is injected rather than declared
//
// It is the fact a skill most reliably needs and an operator most reliably
// cannot supply: where this deployment answers is Tier A's `api.public_url`,
// and the company document may not read Tier A. Declaring it by hand would
// mean writing the deployment's address into a revision that staging and
// production both run, which is the one thing putting it in Tier A prevents.
//
// The name is RESERVED, and the loader refuses a company that declares it.
// Two sources for one address is how a skill comes to link at the deployment
// this company used to run on: an operator who moves behind a new domain
// updates Tier A, the stale declaration keeps winning, and every link a
// person clicks lands nowhere with nothing anywhere saying why.
//
// UNSET WHEN THERE IS NO PUBLIC URL, rather than empty. A skill referencing
// it then renders the literal ${crewlet_base_url} and warns, which is the
// registry's own signal for a variable nobody defined — and is what an
// operator needs to hear. Defining it as "" would compose every link as a
// rooted path a person cannot click and say nothing at all.
func skillVariables(env *config.Resolver, c *Company, publicBase string) map[string]string {
	declared := c.Config.SkillVariables
	if len(declared) == 0 && publicBase == "" {
		return nil
	}
	out := make(map[string]string, len(declared)+1)
	for name, value := range declared {
		out[name] = env.Value(value)
	}
	if publicBase != "" {
		out[config.ReservedBaseURLVariable] = publicBase
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
	e.skills.SetVariables(skillVariables(e.resolver(), c, e.publicBase))
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

// SkillsContainer is the knowledge container the sync worker walks, or "".
//
// Empty means no sync: a company with `knowledge.backend: none`, or one that
// turned tool skills off with `knowledge.skills_container: ""`. Both are
// ordinary, and both mean the catalogue stays empty rather than the engine
// inventing one.
//
// CONTAINER rather than the backend's own word, because the walk it feeds is
// backend-neutral: [Engine.SyncSkills] takes rendered pages, so the backend
// that read them is the caller's business and not this signature's. It was
// `SkillsProject` while Plane was served — a name that outlived its vendor
// and then described a Confluence SPACE, which is the drift this rename ends.
func (e *Engine) SkillsContainer(c *Company) string {
	return c.Config.SkillsContainerKey()
}

// skillsContainer is [Engine.SkillsContainer] read off the CURRENT epoch.
//
// The form the long-lived, per-NODE consumers take — the native searcher and
// the page change feed. Neither is rebuilt by an apply (a projector and a
// durable consumer both follow a coordination family, which a company
// revision does not change), so each has to read the key at the moment it
// uses it or hold a stale one for the life of the process.
//
// Empty before the first epoch is published, which is the honest answer:
// nothing is reserved until there is a company saying so.
func (e *Engine) skillsContainer() string {
	c := e.Company()
	if c == nil || c.Config == nil {
		return ""
	}
	return e.SkillsContainer(c)
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
	container := e.SkillsContainer(c)
	if container == "" || walk == nil {
		return
	}
	pages, err := walk(ctx, container)
	if err != nil {
		log.ErrorContext(ctx, "tool_skill_sync_failed", "container", container, "error", err.Error(),
			"detail", "the registry keeps whatever it already held; agents run "+
				"without this company's tool guidance until the next sync")
		return
	}
	e.SyncSkills(pages)
	e.auditSkills(c)
}
