package builtin

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/tools"
)

// LoadToolSkillTool is the tool's wire name.
const LoadToolSkillTool = "load_tool_skill"

// ToolSkills is what load_tool_skill needs from the skill registry.
//
// Consumer-defined and one method wide. The registry is a live,
// webhook-updated store sourced from the team knowledge base; none of that
// reaches this package, which knows only that a key resolves to prose.
type ToolSkills interface {
	// Body returns the rendered skill, reporting whether the key exists.
	Body(key string) (string, bool)
}

// loadToolSkill fetches the body behind a catalogue entry.
//
// THE CATALOGUE IS A MENU and this is how a dish is ordered. Every phase
// whose surface matches sees a one-line summary per skill; the body arrives
// only when the model decides it needs it, because a company with twenty MCP
// servers would otherwise spend its prompt on documentation for tools this
// turn will not touch.
//
// It is also the UNLOCK for a required skill: the guard refuses calls to the
// tools a required skill covers until this has run for it in the current
// session. Which is why this tool is never itself gated — see
// skills.ExemptTools.
type loadToolSkill struct{ skills ToolSkills }

var _ tools.Callable = (*loadToolSkill)(nil)

func (t *loadToolSkill) Name() string { return LoadToolSkillTool }

func (t *loadToolSkill) Description() string {
	return "Read the full guidance behind one entry in your tool-skills " +
		"catalogue: workflow examples, conventions, and the details of how " +
		"this company uses that tool. Load a skill BEFORE using the tools it " +
		"covers — entries marked required are enforced, and calls to their " +
		"tools are refused until you have."
}

func (t *loadToolSkill) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The skill key, exactly as the catalogue lists it",
			},
		},
		"required": []string{"key"},
	}
}

func (t *loadToolSkill) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	key := argString(args, "key")
	if key == "" {
		return failed("load_tool_skill needs a `key` — one of the keys your " +
			"tool-skills catalogue lists."), nil
	}
	body, ok := t.skills.Body(key)
	if !ok {
		// NAMES THE MISTAKE rather than reporting an empty skill: a model
		// that mistyped a key and got back "" would read it as a skill
		// with nothing in it and proceed, which is exactly the state the
		// required-skill guard exists to prevent.
		return failed(fmt.Sprintf("No tool skill %q. Use a key exactly as "+
			"your catalogue lists it.", key)), nil
	}
	return tools.Result{Output: body}, nil
}
