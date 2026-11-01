package executor

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's OWN SKILLS, in the shapes the harnesses take them.
//
// A run can be handed skills of its own ([sandbox.RunRequest.Skills]): skills
// its harness lists to its model beside the ones the box and the checkout hold,
// and its model loads with the harness's own skill tool, which puts the
// skill's text in front of it. What is here is only how each harness is handed
// them, and how it reports a load back to the run's tool bridge:
//
//   - CLAUDE CODE takes them as a PLUGIN of the run's own, named
//     [sandbox.SkillNamespace] and loaded with `--plugin-dir`
//     ([Paths.ClaudeSkillsPlugin]): a directory per skill under its `skills/`,
//     which the harness lists as `crewlet:<name>`, and in its `hooks/` one
//     PostToolUse hook on the Skill tool — an mcp_tool hook that calls the
//     run's bridge with the name of the skill the tool loaded, the mechanism
//     the run's notes ride (steer.go), so nothing new reaches the box: no
//     credential, no URL to allow, no program. The plugin carries its hook, so
//     a run's skills and the report of their loads go on or off together
//     ([claudeSkillsGate]).
//   - OPENCODE takes them as a directory its configuration's `skills` names
//     ([Paths.OpenCodeSkills]): a directory per skill named `crewlet:<name>`,
//     the id OpenCode lists and loads a skill by. A plugin of the run's own
//     (opencode_skills.js) reports each load its `skill` tool makes to the
//     bridge's skill route.
//
// THE ENGINE DECIDES WHAT A LOAD UNLOCKS; the harness only says it happened
// (internal/api/mcpbridge, skills.go). Its hook is the one party that can say a
// skill's text was put in front of its model, and a tool the model could call
// to say so instead would unlock a skill it never read — so the tool the hook
// calls is never listed.

// skillHolder is a harness whose runner hands it skills of its own
// ([sandbox.RunRequest.Skills]): written where it reads them, and their loads
// reported to the run's bridge.
type skillHolder interface {
	HandsSkills() bool
}

// HandsSkills reports that a Claude Code run is handed skills of its own — see
// the file's doc.
func (ClaudeCode) HandsSkills() bool { return true }

// HandsSkills reports that an OpenCode run is handed skills of its own — see
// the file's doc.
func (OpenCode) HandsSkills() bool { return true }

// refuseSkills refuses a run handed skills that its harness cannot take, with
// nowhere to report a load, or under names no harness reads a skill by — each
// a run that would start without skills the engine is holding its tools to, or
// on a skill directory named outside the box.
func (r *Runner) refuseSkills(req sandbox.RunRequest) error {
	if len(req.Skills) == 0 {
		return nil
	}
	if h, ok := r.harness.(skillHolder); !ok || !h.HandsSkills() {
		return fmt.Errorf("executor: %s takes no skills of its own, and this run is handed %d",
			r.harness.Name(), len(req.Skills))
	}
	if !req.SkillLoads.Set() {
		return fmt.Errorf("executor: this run is handed %d skills and nowhere to report loading one; "+
			"a required skill it loaded would still hold its tools back", len(req.Skills))
	}
	seen := make(map[string]bool, len(req.Skills))
	for _, s := range req.Skills {
		if !sandbox.ValidSkillName(s.Name) {
			return fmt.Errorf("executor: %q is not a name a harness reads a skill by: lowercase "+
				"letters and digits in runs joined by single hyphens, at most 64", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("executor: this run is handed two skills named %q", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// skillFile is one skill's SKILL.md: its frontmatter, naming it and saying
// what it is for, then its body.
//
// EACH VALUE A JSON STRING, which is a YAML double-quoted scalar: a description
// is somebody's prose, holding colons, quotes and line breaks a plain YAML
// scalar would read as structure, and both harnesses parse the frontmatter as
// YAML. `&`, `<` and `>` are written as themselves.
func skillFile(name, description, body string) ([]byte, error) {
	quoted := func(v string) (string, error) {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return "", err
		}
		return strings.TrimSuffix(b.String(), "\n"), nil
	}
	n, err := quoted(name)
	if err != nil {
		return nil, err
	}
	d, err := quoted(description)
	if err != nil {
		return nil, err
	}
	return []byte("---\nname: " + n + "\ndescription: " + d + "\n---\n\n" + body), nil
}

// claudeSkillsManifest is the run's skills plugin's manifest
// (`.claude-plugin/plugin.json`): its name, which every skill in it is listed
// under — `name` is the one field Claude Code requires.
type claudeSkillsManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// claudeSkillsDescription is the plugin's description, which a person reading
// the run's session sees for it.
const claudeSkillsDescription = "The company's own skills, handed to this run by Crewlet."

// claudeSkillHooks is the plugin's `hooks/hooks.json`: the shape of a settings
// file's `hooks` object, under a key of its own.
type claudeSkillHooks struct {
	Hooks map[string][]claudeHookMatcher `json:"hooks"`
}

// claudeSkillTool is the Claude Code tool a model loads a skill with, which is
// the matcher the report hook fires on.
const claudeSkillTool = "Skill"

// claudeEventToolUsed is the hook event that fires once a tool has answered
// successfully: a skill that failed to load reports nothing.
const claudeEventToolUsed = "PostToolUse"

// claudeSkillLoad is what the report hook hands the bridge's tool: the skill the
// Skill tool loaded, as the model named it, filled in by the harness.
type claudeSkillLoad struct {
	Skill string `json:"skill"`
}

// writeClaudeSkills writes the plugin a Claude Code run is handed its skills in
// ([Paths.ClaudeSkillsPlugin]): its manifest, a SKILL.md per skill, and the hook
// that reports each load through w.
func writeClaudeSkills(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill,
	w sandbox.SkillWiring, paths Paths,
) error {
	dir := paths.ClaudeSkillsPlugin()
	manifest, err := json.MarshalIndent(claudeSkillsManifest{
		Name: sandbox.SkillNamespace, Description: claudeSkillsDescription}, "", "  ")
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, dir+"/.claude-plugin/plugin.json", manifest); err != nil {
		return fmt.Errorf("executor: writing the run's skills plugin: %w", err)
	}
	for _, s := range skills {
		// The skill's own name: the plugin's prefixes it, which is the name
		// the harness lists it under ([sandbox.Skill.Listed]).
		var file []byte
		if file, err = skillFile(s.Name, s.Description, s.Body); err != nil {
			return err
		}
		if err = box.WriteFile(ctx, dir+"/skills/"+s.Name+"/SKILL.md", file); err != nil {
			return fmt.Errorf("executor: writing the run's skill %q: %w", s.Listed(), err)
		}
	}
	hooks, err := json.MarshalIndent(claudeSkillHooks{Hooks: map[string][]claudeHookMatcher{
		claudeEventToolUsed: {{Matcher: claudeSkillTool, Hooks: []claudeHook{{
			Type: "mcp_tool", Server: w.Server, Tool: w.Tool,
			Input:   claudeSkillLoad{Skill: "${tool_input.skill}"},
			Timeout: int(ClaudeHookTimeout.Seconds()),
		}}}},
	}}, "", "  ")
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, dir+"/hooks/hooks.json", hooks); err != nil {
		return fmt.Errorf("executor: writing the run's skill-load hook: %w", err)
	}
	return nil
}

// claudeSkillsFloor is the oldest Claude Code release a run is handed skills of
// its own on: 2.1.118, the first that runs an mcp_tool hook (its changelog:
// "Hooks can now invoke MCP tools directly via `type: "mcp_tool"`"), whose
// `${tool_input.skill}` names the skill the Skill tool loaded. An older release
// could list the skills and load them, and never say it had.
const claudeSkillsFloor = "2.1.118"

// claudeSkillsGate is the part of a run's script, handed skills of its own, that
// adds the plugin holding them to the script's arguments by the harness's
// release ([claudeRelease]): at or after [claudeSkillsFloor] it is
// `--plugin-dir <plugin>`. On an older release a run with a REQUIRED skill is
// refused before the harness starts — every call to a tool the skill covers
// would be refused, the load that unlocks them never reported, so the run
// could only fail its task — and any other run starts without its skills,
// which its error stream says: an advisory skill held nothing back.
func claudeSkillsGate(plugin string, required bool) string {
	older := `echo "crewlet: this box runs Claude Code $n, older than ` + claudeSkillsFloor + `, the oldest ` +
		`that reports loading a skill; the run starts without its company's skills" >&2`
	if required {
		older = `echo "crewlet: this box runs Claude Code $n, older than ` + claudeSkillsFloor + `, the oldest ` +
			`that reports loading a skill, and this run's company requires a skill before some of its ` +
			`tools; upgrade Claude Code in the box image" >&2; exit 1`
	}
	return "if at " + floorArgs(claudeSkillsFloor) + `; then set -- "$@" --plugin-dir ` + shellQuote(plugin) +
		"; else " + older + "; fi\n"
}

// anyRequired reports whether any of skills is required.
func anyRequired(skills []sandbox.Skill) bool {
	for _, s := range skills {
		if s.Required {
			return true
		}
	}
	return false
}

// writeOpenCodeSkills writes an OpenCode run's skills into the directory its
// configuration names ([Paths.OpenCodeSkills]): a directory per skill, named as
// the skill is listed ([sandbox.Skill.Listed]) — the id OpenCode lists and loads
// a skill by is its directory's name — holding its SKILL.md.
func writeOpenCodeSkills(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill, paths Paths) error {
	for _, s := range skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return err
		}
		if err = box.WriteFile(ctx, paths.OpenCodeSkills()+"/"+s.Listed()+"/SKILL.md", file); err != nil {
			return fmt.Errorf("executor: writing the run's skill %q: %w", s.Listed(), err)
		}
	}
	return nil
}

// openCodeSkillsPlugin is the plugin an OpenCode run's server loads to report
// each skill its model loads to the run's bridge: opencode_skills.js, whose
// header says how. Embedded, as the notes plugin is, so the plugin a run is
// handed is the one this build's bridge answers. Written as the `index`
// module of its own directory ([Paths.OpenCodeSkillsPlugin],
// [openCodeSteerEntry]).
//
//go:embed opencode_skills.js
var openCodeSkillsPlugin []byte
