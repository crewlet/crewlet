package executor

import (
	"bytes"
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
//     the bridge as the run's notes do (claudesteer.go): no new credential, URL or
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
