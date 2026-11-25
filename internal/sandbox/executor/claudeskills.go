package executor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// HandsSkills reports that a Claude Code run takes skills of its own.
func (ClaudeCode) HandsSkills() bool { return true }

// claudeSkillsManifest is the skills plugin's `.claude-plugin/plugin.json`;
// `name`, which its skills are listed under, is the one field Claude Code requires.
type claudeSkillsManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// claudeSkillsDescription is the plugin's description, shown to a person
// reading the run's session.
const claudeSkillsDescription = "The company's own skills, handed to this run by Crewlet."

// claudeSkillHooks is the plugin's `hooks/hooks.json`: a settings file's
// `hooks` object under a key of its own.
type claudeSkillHooks struct {
	Hooks map[string][]claudeHookMatcher `json:"hooks"`
}

// claudeSkillTool is the Claude Code tool that loads a skill: the report
// hook's matcher.
const claudeSkillTool = "Skill"

// claudeEventToolUsed is the hook event that fires once a tool has answered
// successfully: a skill that failed to load reports nothing.
const claudeEventToolUsed = "PostToolUse"

// claudeSkillLoad is the report hook's input to the bridge's tool: the skill
// the Skill tool loaded, as the model named it, filled in by the harness.
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
		// The bare name: the harness prefixes the plugin's ([sandbox.Skill.Listed]).
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

// claudeSkillsFloor is the oldest Claude Code release a run is handed skills
// on: 2.1.118 is the first that runs an mcp_tool hook (its changelog says so),
// which the load report needs. An older one would load skills and never say so.
const claudeSkillsFloor = "2.1.118"

// claudeSkillsGate is the script part that, by the harness's release
// ([claudeRelease]), at or after [claudeSkillsFloor] adds `--plugin-dir <plugin>`
// and sets hk, the plugin's hook being one of the engine's
// ([claudeHooksOffGate]). On an older release a run with a REQUIRED skill is
// refused before the harness starts (it could only fail its task); any other
// starts without its skills and says so on stderr.
func claudeSkillsGate(plugin string, required bool) string {
	older := `echo "crewlet: this box runs Claude Code $n, older than ` + claudeSkillsFloor + `, the oldest ` +
		`that reports loading a skill; the run starts without its company's skills" >&2`
	if required {
		older = `echo "crewlet: this box runs Claude Code $n, older than ` + claudeSkillsFloor + `, the oldest ` +
			`that reports loading a skill, and this run's company requires a skill before some of its ` +
			`tools; upgrade Claude Code in the box image" >&2; exit 1`
	}
	return "if at " + floorArgs(claudeSkillsFloor) + `; then set -- "$@" --plugin-dir ` + shellword.Quote(plugin) +
		"; hk=1; else " + older + "; fi\n"
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
