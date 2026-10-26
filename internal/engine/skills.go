package engine

import (
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
)

// The tool-skill registry, wired.
//
// # It outlives an epoch, and the org chart is why
//
// The registry's CONTENT comes from the knowledge base rather than from
// config: a skill is a page somebody wrote, and an apply that changed a
// seat's model has nothing to say about it. So the registry is built once per
// node and survives every apply; what an apply refreshes is the operator's
// ${var} map, which IS config, and the SOURCE the skills are read from, which
// is config too (see skillsync.go).
//
// Rebuilding it per epoch would empty it on every apply and leave every seat
// running without its company's guidance until the next walk.

// skillVariables resolves the operator's substitution map for an epoch, plus
// the two variables the engine reserves.
//
// RESOLVED, because a value may be a ${VAR} reference: the whole point of
// the map is to carry deployment facts like a tenant URL into skill prose,
// and those are exactly the values an operator keeps out of a config file.
//
// # Why crewlet_base_url is injected rather than declared
//
// It is the fact a skill most reliably needs and an operator most reliably
// cannot supply. Where a person reaches this deployment is already written
// once, as `api.external_url` — the same address the session cookie, the
// CSRF origin check and every link a person follows are built on — so
// injecting it is the one way a company's own prose and its wiring can never
// name different deployments ([Engine.publicBase]).
//
// NEVER `api.public.external_url`. That is where VENDORS reach the
// deployment, and behind a public listener it is a socket that answers the
// dashboard 404: every link a skill composed on it would land a person on
// nothing.
//
// The name is RESERVED, and the loader refuses a company that declares it.
// Two sources for one address is how a skill comes to link at the deployment
// this company used to run on: an operator who moves behind a new domain
// updates their Tier A file, the stale declaration keeps winning, and every
// link a person clicks lands nowhere with nothing anywhere saying why.
//
// UNSET WHEN THERE IS NO EXTERNAL URL, rather than empty. A skill referencing
// it then renders the literal ${crewlet_base_url} and warns, which is the
// registry's own signal for a variable nobody defined — and is what an
// operator needs to hear. Defining it as "" would compose every link as a
// rooted path a person cannot click and say nothing at all.
//
// # Why crewlet_kit_version is injected too
//
// The artifacts skill tells a seat which kit release to link, and the only
// honest answer is the newest one this binary embeds ([kitVersion]): a page
// pinned to an older kit keeps working, but it never reads the brand slots a
// newer kit added, so the company's adopted brand silently skips it. A
// version written into the skill's prose went stale the first time the engine
// moved to a new kit, and nothing ties the skill to the build except this.
// It is reserved for the same reason as the base URL, and left undefined only
// for a build whose embedded kit tree cannot be read.
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

// publicBase is where a PERSON reaches this deployment: the base every link
// composed for somebody to click is built on — a skill's ${crewlet_base_url}
// and a page change's `url` — which is `api.external_url`.
//
// NEVER WHERE A VENDOR REACHES IT ([config.API.WebhookBase]). Behind a public
// listener (`api.public`) that is a socket serving only the webhook, OTLP and
// tool-bridge routes, which answers the dashboard 404: every link composed on
// it would land a person on nothing.
//
// IT TAKES THE COMPANY AND DOES NOT READ IT, and the unused parameter is the
// point rather than an oversight: this used to be a Tier B pointer resolved
// per epoch, so a caller holding one epoch could get a different answer from a
// caller holding another. Keeping the parameter keeps every call site honest
// about the epoch it is composing a link FOR while the answer itself is now a
// property of the process — which is what `api.external_url` being Tier A
// means.
//
// EMPTY when the deployment has no external address, which every consumer here
// reads as "compose no link" rather than as a relative one.
func (e *Engine) publicBase(*Company) string { return e.externalBase }

// refreshSkillVariables installs an epoch's substitution map.
//
// Called on every apply, boot included. A variable REMOVED by a revision
// then surfaces immediately — the registry re-checks every registered skill
// against the new map — rather than on that skill's next edit, which might
// be never.
func (e *Engine) refreshSkillVariables(c *Company) {
	e.skills.SetVariables(skillVariables(e.resolver(), c, e.publicBase(c), kitVersion()))
}

// auditSkills reports every skill whose trigger names a tool the current
// epoch does not have.
//
// PER EPOCH, because the tool surface is what an apply changes: a revision
// that removed an MCP server silently un-triggers every skill about it, and
// nothing else in the system would say so. See [skills.Trigger.Classify] for
// why drift and a foreign stack are reported differently.
//
// AGAINST THE EPOCH THAT IS CURRENT, never one still being built, which is why
// it takes no epoch. It has two inputs and runs wherever either one changes:
// after an epoch is installed ([Engine.installEpoch]) or its shared MCP tools
// are refiled ([Engine.sharedCatalogueChanged]), and after the skill sync
// changed the registry (its OnChange hook), each reading the other as it
// stands at that moment, so the last change to either is always audited
// against the last value of the other. It ran inside [Engine.equip] before,
// against the epoch an apply was building, while the sync audited against
// whatever was current: a registry change that landed between the build and
// the publish (most often the very walk the apply's own source change asked
// for) was audited against the outgoing epoch and never against the new one.
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
// Empty means no sync: a company with `knowledge.backend: none`, or one that
// turned tool skills off with `knowledge.skills_container: ""`. Both are
// ordinary, and both mean the catalogue stays empty rather than the engine
// inventing one.
//
// CONTAINER rather than the backend's own word, because the walk it feeds is
// backend-neutral: a [skillsync.Source] carries its backend's own walk, so the
// backend that reads the pages is that walk's business and not this
// signature's. It was `SkillsProject` while Plane was served, a name that
// outlived its vendor and then described a Confluence SPACE, which is the
// drift this rename ends.
func (e *Engine) SkillsContainer(c *Company) string {
	return c.Config.SkillsContainerKey()
}

// skillsContainer is [Engine.SkillsContainer] read off the CURRENT epoch.
//
// The form the long-lived, per-NODE consumers take: Doclet's searcher and
// the page change feed. Neither is rebuilt by an apply (both follow the pages
// log, which a company revision does not change), so each has to read the key
// at the moment it uses it or hold a stale one for the life of the process.
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
