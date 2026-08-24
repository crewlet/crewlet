package skills

import (
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/prompts"
)

// The registry: the live set of skills this company has published.
//
// Populated at boot by the knowledge backend's sync worker and refreshed by
// its page webhooks, so editing a skill is a wiki edit and nothing else —
// no restart, no deploy, no config push.

// Registry is the in-memory store of tool skills.
//
// READS TAKE NO LOCK BEYOND A SNAPSHOT SWAP, deliberately: the prompt-build
// path consults it once per phase and can accept an eventually-consistent
// answer — a skill upserted mid-build lands in this prompt or the next, and
// both are correct. Writes are serialised because a page webhook races the
// boot walk during their brief overlap.
type Registry struct {
	mu sync.Mutex

	// skills and variables are swapped WHOLE rather than mutated, so a
	// lock-free reader sees one consistent set: a reader holding a map
	// being written to would see a half-applied edit, which for the
	// variable map means a body rendered with some substitutions applied
	// and some not.
	skills    map[string]Skill
	variables map[string]string
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{skills: map[string]Skill{}, variables: map[string]string{}}
}

// SetVariables installs the operator-defined ${var} substitution map.
//
// Called on every apply, boot included. Re-checking every registered skill
// against the new map is what makes a variable REMOVED by a config push
// surface immediately rather than on that skill's next edit — which might
// be never.
func (r *Registry) SetVariables(variables map[string]string) {
	if r == nil {
		return
	}
	next := maps.Clone(variables)
	if next == nil {
		next = map[string]string{}
	}
	r.mu.Lock()
	r.variables = next
	current := slices.Collect(maps.Values(r.skills))
	r.mu.Unlock()

	for _, skill := range current {
		warnUnresolved(skill, next)
	}
}

// warnUnresolved reports a skill referencing a variable nobody defined.
//
// Substitution deliberately leaves an unknown reference as literal ${name},
// which is visible and greppable — but the only place that literal is ever
// seen is inside an LLM prompt, and no operator reads those. This turns a
// silent prompt defect into a log line at the moment it becomes true.
func warnUnresolved(s Skill, variables map[string]string) {
	for field, text := range map[string]string{
		"title": s.Title, "summary": s.Summary, "body": s.Body,
	} {
		for _, name := range VariableRefs(text) {
			if _, ok := variables[name]; !ok {
				log.Warn("skill_variable_unresolved", "skill", s.Key,
					"field", field, "variable", name)
			}
		}
	}
}

// Upsert registers or replaces a skill.
//
// A REPLACE rather than a merge, because a page IS the skill: an edit that
// removed a trigger leaf must remove it here, and a merge would keep the
// skill matching a surface its author has just stopped claiming.
func (r *Registry) Upsert(s Skill) error {
	if r == nil {
		return nil
	}
	if err := s.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	next := maps.Clone(r.skills)
	next[s.Key] = s
	r.skills = next
	variables := r.variables
	r.mu.Unlock()

	warnUnresolved(s, variables)
	return nil
}

// Evict removes a skill, reporting whether it was there.
//
// The reported bool is what lets a caller tell "the page was deleted" from
// "a page that was never a skill was deleted" — the sync worker evicts on
// every page removal and only one of those is worth a log line.
func (r *Registry) Evict(key string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.skills[key]; !ok {
		return false
	}
	next := maps.Clone(r.skills)
	delete(next, key)
	r.skills = next
	return true
}

// Replace swaps the whole set, for a sync worker's full walk.
//
// ATOMIC, which is what makes a boot walk safe to run against a registry
// that is already serving: a walk applied skill by skill would leave a
// window where half the company's guidance exists.
//
// A caller that could not complete its walk must NOT call this — see the
// sync worker, which refuses to seed from a partial enumeration precisely
// because a wholesale replace from partial rows silently deletes skills.
func (r *Registry) Replace(skills []Skill) {
	if r == nil {
		return
	}
	next := make(map[string]Skill, len(skills))
	for _, s := range skills {
		if err := s.Validate(); err != nil {
			log.Warn("skill_refused", "skill", s.Key, "error", err.Error())
			continue
		}
		next[s.Key] = s
	}
	r.mu.Lock()
	r.skills = next
	variables := r.variables
	r.mu.Unlock()

	for _, s := range next {
		warnUnresolved(s, variables)
	}
	log.Info("skills_replaced", "count", len(next))
}

// Get returns one skill by key.
func (r *Registry) Get(key string) (Skill, bool) {
	if r == nil {
		return Skill{}, false
	}
	r.mu.Lock()
	skills := r.skills
	r.mu.Unlock()
	s, ok := skills[key]
	return s, ok
}

// Len is how many skills are registered.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.skills)
}

// Matching returns the skills whose trigger fires for a surface in a phase.
//
// KEY-SORTED. The prompt package sorts again — its byte-stability is its own
// promise to keep — but answering in map order here would make every other
// caller's output move between two identical builds for no reason anybody
// could see.
func (r *Registry) Matching(phase prompts.Phase, surface prompts.Surface) []Skill {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	skills := r.skills
	r.mu.Unlock()

	var out []Skill
	for _, s := range skills {
		if s.AppliesTo(phase) && s.Trigger.Matches(surface) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Skill) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// SkillsFor implements [prompts.SkillCatalogue].
func (r *Registry) SkillsFor(phase prompts.Phase, surface prompts.Surface) []prompts.Skill {
	matching := r.Matching(phase, surface)
	out := make([]prompts.Skill, 0, len(matching))
	for _, s := range matching {
		out = append(out, prompts.Skill{
			Key: s.Key, Summary: s.Summary, Required: s.Required,
		})
	}
	return out
}

// Render implements [prompts.SkillCatalogue].
func (r *Registry) Render(text string) string {
	if r == nil {
		return text
	}
	r.mu.Lock()
	variables := r.variables
	r.mu.Unlock()
	return Substitute(text, variables)
}

// Audit reports every skill whose trigger names a tool this deployment does
// not have.
//
// Run against the real registry once per apply, because exact-string trigger
// matching is validated nowhere else: an upstream MCP server renaming a tool
// silently disables the skill AND the guard that was enforcing it, and
// nothing raises. See [Trigger.Classify] for why a partially-live skill is a
// warning and a wholly-dead one is a note.
func (r *Registry) Audit(knownTools, knownServers []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	skills := r.skills
	r.mu.Unlock()

	for _, key := range slices.Sorted(maps.Keys(skills)) {
		verdict := skills[key].Trigger.Classify(knownTools, knownServers)
		switch {
		case len(verdict.Dangling) == 0:
		case verdict.Live:
			log.Warn("skill_trigger_partially_dangling", "skill", key,
				"unknown_tools", verdict.Dangling,
				"detail", "some of this trigger still matches, so these names "+
					"are most likely drift after a tool was renamed upstream")
		default:
			log.Info("skill_trigger_matches_nothing", "skill", key,
				"unknown_tools", verdict.Dangling,
				"detail", "no part of this trigger matches this deployment; it "+
					"may be authored for a stack this company does not run")
		}
	}
}

// Body renders a skill for loading, or reports that it is not there.
//
// The rendered body carries its TITLE and its trigger's subject, because a
// model that asked for a key gets back prose with no header otherwise — and
// a body it cannot attribute is a body it cannot decide to trust.
func (r *Registry) Body(key string) (string, bool) {
	s, ok := r.Get(key)
	if !ok {
		return "", false
	}
	var b strings.Builder
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = s.Key
	}
	b.WriteString("# " + r.Render(title) + "\n\n")
	if summary := strings.TrimSpace(s.Summary); summary != "" {
		b.WriteString(r.Render(summary) + "\n\n")
	}
	b.WriteString(r.Render(s.Body))
	return strings.TrimRight(b.String(), "\n") + "\n", true
}

// All returns every registered skill, key-sorted.
//
// For the callers that must find a skill by something other than its key —
// the webhook path evicts by SOURCE PAGE, because a page whose key was
// edited would otherwise leave the old key behind for ever.
func (r *Registry) All() []Skill {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	skills := r.skills
	r.mu.Unlock()

	out := make([]Skill, 0, len(skills))
	for _, key := range slices.Sorted(maps.Keys(skills)) {
		out = append(out, skills[key])
	}
	return out
}
