package engine

import (
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
)

// The tool-skill registry, wired.
//
// It is built once per node and outlives every apply: its content is
// knowledge-base pages, not config, so rebuilding it per epoch would leave
// every seat without its company's guidance until the next walk. An apply
// refreshes only what IS config: the ${var} map and the skills source
// (skillsync.go).

// skillVariables resolves an epoch's substitution map (a value may be a
// ${VAR} reference: deployment facts stay out of config files), plus two
// RESERVED variables the loader refuses a company declaring, since a stale
// declaration would keep winning after the deployment moved:
//   - crewlet_base_url is [Engine.publicBase], never `api.public.external_url`
//     (where vendors reach it, which answers the dashboard 404). Unset rather
//     than "" without an external URL: a skill using it then renders the
//     literal and the registry warns, instead of composing unclickable paths.
//   - crewlet_kit_version is [kitVersion], the newest kit this binary embeds:
//     a page on an older kit never reads newer brand slots. Unset only when
//     the embedded kit tree cannot be read.
func skillVariables(env *config.Resolver, c *Company, publicBase, kit string) map[string]string {
	declared := c.Config.SkillVariables
	if len(declared) == 0 && publicBase == "" && kit == "" {
		return nil
	}
	out := make(map[string]string, len(declared)+2)
	for name, value := range declared {
		out[name] = env.Value(value)
	}
	if publicBase != "" {
		out[config.ReservedBaseURLVariable] = publicBase
	}
	if kit != "" {
		out[config.ReservedKitVersionVariable] = kit
	}
	return out
}

// publicBase is where a PERSON reaches this deployment, `api.external_url`:
// the base of every link composed for somebody to click (a skill's
// ${crewlet_base_url}, a page change's `url`). Never [config.API.WebhookBase],
// whose public listener answers the dashboard 404. Empty without an external
// address, which every consumer reads as "compose no link".
//
// The unused *Company keeps each call site naming the epoch it composes a link
// for, though the answer is a property of the process (Tier A).
func (e *Engine) publicBase(*Company) string { return e.externalBase }

// refreshSkillVariables installs an epoch's substitution map, on every apply
// and at boot, so a variable a revision removed surfaces at once (the registry
// re-checks every skill against the new map), not on the skill's next edit.
func (e *Engine) refreshSkillVariables(c *Company) {
	e.skills.SetVariables(skillVariables(e.resolver(), c, e.publicBase(c), kitVersion()))
}

// auditSkills reports every skill whose trigger names a tool the current
// epoch does not have: a revision that removed an MCP server silently
// un-triggers every skill about it. See [skills.Trigger.Classify] for why
// drift and a foreign stack are reported differently.
//
// Against the CURRENT epoch, never one being built, so it takes none. It runs
// wherever either input changes: an epoch installed ([Engine.installEpoch]) or
// its shared MCP tools refiled ([Engine.sharedCatalogueChanged]), and the skill
// sync's OnChange, so the last change to either is audited against the last
// value of the other. Run from [Engine.equip] instead, a registry change
// landing between build and publish would be audited against the outgoing
// epoch only.
func (e *Engine) auditSkills() {
	c := e.Company()
	if c == nil || c.Tools == nil || e.skills.Len() == 0 {
		return
	}
	snapshot := c.Tools.Snapshot()
	e.skills.Audit(snapshot.Names(), snapshot.MCPServers())
}

// SkillsContainer is the knowledge container the skill sync walks, or "".
//
// Empty means no sync (`knowledge.backend: none`, or
// `knowledge.skills_container: ""`), and the catalogue stays empty. A
// container rather than a backend's own word (a Confluence space), because the
// walk is backend-neutral: a [skillsync.Source] carries its backend's walk.
func (e *Engine) SkillsContainer(c *Company) string {
	return c.Config.SkillsContainerKey()
}

// skillsContainer is [Engine.SkillsContainer] read off the CURRENT epoch, for
// the per-node consumers no apply rebuilds (Doclet's searcher, the page change
// feed): each reads the key when it uses it rather than hold a stale one.
// Empty before the first epoch is published.
func (e *Engine) skillsContainer() string {
	c := e.Company()
	if c == nil || c.Config == nil {
		return ""
	}
	return e.SkillsContainer(c)
}

// Skills is this node's tool-skill registry. Never nil: it is built with the
// engine, and a company with no skills has an empty one, so no phase build
// needs a nil check that one would forget.
func (e *Engine) Skills() *skills.Registry { return e.skills }
