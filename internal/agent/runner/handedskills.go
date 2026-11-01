package runner

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE EXECUTOR'S TOOL SKILLS ARE ITS HARNESS'S OWN.
//
// Every other phase lists the Tool Skills its surface matches in its prompt and
// loads one with load_tool_skill. The executor is its harness's own run, and
// both executor harnesses load skills of their own: Claude Code's Skill tool,
// OpenCode's skill tool, each listing a skill's name and description to its
// model and handing it the skill's text only when the model loads it — the
// same two tiers the catalogue and load_tool_skill make. So the executor's
// skills are handed to its harness as its own (internal/sandbox,
// RunRequest.Skills), and its prompt lists none.
//
// ONE KIND OF LOAD, whichever loader made it. The harness reports each load
// to the run's bridge, which records it as the run's own load_tool_skill call
// with the skill's key (internal/api/mcpbridge, skills.go) — so the
// required-skill guard unlocks by its own reading of that call, the skill's
// page records the read, and every rebuild of the run replays the load from
// its record, as it replays one made over the bridge. load_tool_skill stays
// on the executor's surface for that call to be made on, and the bridge never
// lists it to the harness: its own tool is the loader its model knows.
//
// A SKILL IS NAMED FOR THE HARNESS ([nativeNames]): a key such as `mcp:github`
// is no name either harness reads a skill by, so it is listed as
// `crewlet:mcp-github` (internal/sandbox, SkillNamespace) — on both harnesses
// alike, so the guard's refusal names the skill the run's model can load
// without knowing which harness runs it ([skills.Guard.HandedAs]). The names a
// launch handed are on the parked state ([execstate.State.NativeSkills]), so
// every later opening of the run's bridge reads a reported name the way the
// launch's did.

// HandedSkill is one Tool Skill a run's harness is handed as its own: the
// skill's key, and the skill as the harness is given it.
type HandedSkill struct {
	Key   string
	Skill sandbox.Skill
}

// HandedSkills are the skills a launch hands its run's harness.
type HandedSkills []HandedSkill

// Box is the skills as the run's box is given them.
func (h HandedSkills) Box() []sandbox.Skill {
	if len(h) == 0 {
		return nil
	}
	out := make([]sandbox.Skill, 0, len(h))
	for _, s := range h {
		out = append(out, s.Skill)
	}
	return out
}

// Listed is the key each skill stands for, by the name its harness lists it
// under: what a load the harness reports is read by (internal/api/mcpbridge,
// Session.Skills). Nil for none.
func (h HandedSkills) Listed() map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for _, s := range h {
		out[s.Skill.Listed()] = s.Key
	}
	return out
}

// requiredNote is what a required skill's description ends with, beside its
// summary: the harness lists only a name and a description, and a model told a
// skill holds tools back loads it before it calls them rather than after a
// refusal.
const requiredNote = " Required: load this skill before you call the tools it covers."

// handSkills is the Tool Skills the executor's harness is handed as its own,
// matched against the surface the run is launched with as the executor's
// catalogue was, and recorded as what the run was offered
// (`knowledge_read` with `via: skill_injected`, [emitter.skillsInjected]): the
// harness lists every one to its model, which is the summary a catalogue line
// carried. The run's guard refuses a required one's tools by the name it is
// handed under.
func (r *Runner) handSkills(ctx context.Context, surface *tools.Surface) HandedSkills {
	offer := r.cfg.Skills.Offer()
	matched := offer.Hand(prompts.PhaseExecute, catalogueSurface(surface))
	r.emitter().skillsInjected(ctx, phase.Execute, offer.Drain())
	if len(matched) == 0 {
		r.handedAs(nil)
		return nil
	}
	keys := make([]string, 0, len(matched))
	for _, m := range matched {
		keys = append(keys, m.Skill.Key)
	}
	names := nativeNames(keys)
	out := make(HandedSkills, 0, len(matched))
	for _, m := range matched {
		description := strings.TrimSpace(offer.Render(m.Skill.Summary))
		if m.Skill.Required {
			description += requiredNote
		}
		out = append(out, HandedSkill{Key: m.Skill.Key, Skill: sandbox.Skill{
			Name: names[m.Skill.Key], Description: strings.TrimSpace(description),
			Body: m.Loaded.Body, Required: m.Skill.Required,
		}})
	}
	r.handedAs(out.Listed())
	return out
}

// handedAs tells the run's guard the name its harness lists each skill under,
// by key, from the listed names a launch handed (listed name to key).
func (r *Runner) handedAs(listed map[string]string) {
	byKey := make(map[string]string, len(listed))
	for name, key := range listed {
		byKey[key] = name
	}
	r.mu.Lock()
	guard := r.guard
	r.mu.Unlock()
	guard.HandedAs(byKey)
}

// maxNativeName is the longest name a skill is handed under
// ([sandbox.ValidSkillName]).
const maxNativeName = 64

// nativeNames is the name each of keys is handed under: the key in lowercase,
// every run of anything but an ASCII letter or digit one hyphen, trimmed of
// hyphens at either end and to [maxNativeName] — `mcp:github` is
// `mcp-github`, `skill:platform_mentions` `skill-platform-mentions`. Two keys
// that come to one name are told apart in key order: the first keeps it, the
// next is `-2`, then `-3`, so one set of keys is named alike on every node and
// at every launch of it. A key with nothing to keep is `skill`.
func nativeNames(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	taken := make(map[string]bool, len(keys))
	for _, key := range slices.Sorted(slices.Values(keys)) {
		if _, done := out[key]; done {
			continue
		}
		base := slugOf(key)
		name := base
		for n := 2; taken[name]; n++ {
			suffix := "-" + strconv.Itoa(n)
			name = strings.TrimRight(base[:min(len(base), maxNativeName-len(suffix))], "-") + suffix
		}
		taken[name] = true
		out[key] = name
	}
	return out
}

// slugOf is key as a skill's name before any other key is considered — see
// [nativeNames].
func slugOf(key string) string {
	var b strings.Builder
	hyphen := false
	for _, c := range strings.ToLower(key) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
			hyphen = false
			continue
		}
		if b.Len() > 0 && !hyphen {
			b.WriteByte('-')
			hyphen = true
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if len(name) > maxNativeName {
		name = strings.TrimRight(name[:maxNativeName], "-")
	}
	if name == "" {
		return "skill"
	}
	return name
}
