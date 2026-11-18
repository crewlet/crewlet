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

// A run's OWN SKILLS ([sandbox.RunRequest.Skills]): how each harness is handed
// them, and how it reports a load to the run's tool bridge.
//   - CLAUDE CODE: a plugin named [sandbox.SkillNamespace], loaded with
//     `--plugin-dir` ([Paths.ClaudeSkillsPlugin]), listing each skill as
//     `crewlet:<name>`, whose PostToolUse mcp_tool hook on the Skill tool calls
//     the bridge as the run's notes do (steer.go): no new credential, URL or
//     program reaches the box. Skills and hook switch together ([claudeSkillsGate]).
//   - OPENCODE: a directory per skill named `crewlet:<name>` ([Paths.OpenCodeSkills]),
//     and a plugin (opencode_skills.js) reporting each load its `skill` tool makes.
//
// The engine decides what a load unlocks (internal/api/mcpbridge, skills.go). The
// hook's tool is never listed: only the hook can attest the model read the skill.

// skillHolder is a harness that can be handed skills of its own
// ([sandbox.RunRequest.Skills]) and report their loads to the run's bridge.
type skillHolder interface {
	HandsSkills() bool
}

// HandsSkills reports that a Claude Code run takes skills of its own.
func (ClaudeCode) HandsSkills() bool { return true }

// HandsSkills reports that an OpenCode run takes skills of its own.
func (OpenCode) HandsSkills() bool { return true }

// refuseSkills refuses a run handed skills its harness cannot take, with
// nowhere to report a load, or under a name no harness reads a skill by: each
// would start without skills the engine holds its tools to, or name a skill
// directory outside the box.
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

// skillFile is one skill's SKILL.md: its name and description as frontmatter,
// then its body.
//
// Each value is a JSON string, which is a YAML double-quoted scalar: a
// description's colons, quotes and line breaks would read as structure in a
// plain scalar. `&`, `<` and `>` are written as themselves.
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
// and sets cs to settings ([Paths.ClaudeSkillsSettings]), which switch hooks on:
// a repository's `disableAllHooks: true` would silently drop the load report,
// and a required skill would never unlock. On an older release a run with a
// REQUIRED skill is refused before the harness starts (it could only fail its
// task); any other starts without its skills, on the usual settings, and says
// so on stderr.
func claudeSkillsGate(plugin, settings string, required bool) string {
	older := `echo "crewlet: this box runs Claude Code $n, older than ` + claudeSkillsFloor + `, the oldest ` +
		`that reports loading a skill; the run starts without its company's skills" >&2`
	if required {
		older = `echo "crewlet: this box runs Claude Code $n, older than ` + claudeSkillsFloor + `, the oldest ` +
			`that reports loading a skill, and this run's company requires a skill before some of its ` +
			`tools; upgrade Claude Code in the box image" >&2; exit 1`
	}
	return "if at " + floorArgs(claudeSkillsFloor) + `; then set -- "$@" --plugin-dir ` + sandbox.ShellQuote(plugin) +
		"; cs=" + sandbox.ShellQuote(settings) + "; else " + older + "; fi\n"
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

// writeOpenCodeSkills writes each skill's SKILL.md into [Paths.OpenCodeSkills],
// in a directory named as the skill is listed ([sandbox.Skill.Listed]): OpenCode
// loads a skill by its directory's name.
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

// openCodeSkillsPlugin is opencode_skills.js, which reports each skill an
// OpenCode run's model loads to the run's bridge. Embedded so it matches this
// build's bridge; written as its directory's `index` module
// ([Paths.OpenCodeSkillsPlugin], [openCodeSteerEntry]).
//
//go:embed opencode_skills.js
var openCodeSkillsPlugin []byte
