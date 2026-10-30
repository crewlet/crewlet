package builtin_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/tools"
)

// keyedSkills serves no skill and lists keys.
type keyedSkills []string

func (k keyedSkills) Load(string) (skills.Loaded, bool) { return skills.Loaded{}, false }

func (k keyedSkills) Keys() []string { return k }

// A KEY NOBODY PUBLISHED IS ANSWERED WITH THE KEYS THAT EXIST, the near ones
// first, so a model that guessed corrects itself in one step: GP-38 tried
// "artifacts", then "artifact", for "skill:artifacts", and was told only that
// each was wrong.
//
// Mutation: answer the miss without the keys and the second guess follows.
func TestAMissedToolSkillNamesTheKeysThatExist(t *testing.T) {
	t.Parallel()
	keys := keyedSkills{"skill:artifacts", "skill:deploys", "skill:github"}
	tool := registered(t, builtin.Deps{ToolSkills: keys}, builtin.LoadToolSkillTool)
	for _, asked := range []string{"artifacts", "artifact", "ARTIFACTS"} {
		res := callFor(t, tool, executing(t), map[string]any{"key": asked})
		if !res.Failed || tools.RefusalOf(res) != tools.RefusalNotFound {
			t.Fatalf("%q answered %q", asked, res.Output)
		}
		for _, want := range []string{`Did you mean "skill:artifacts"?`,
			`The keys are: "skill:artifacts", "skill:deploys" and "skill:github".`} {
			if !strings.Contains(res.Output, want) {
				t.Errorf("%q answered %q, which does not say %q", asked, res.Output, want)
			}
		}
	}
	if res := callFor(t, tool, executing(t), map[string]any{"key": "zzz"}); strings.Contains(res.Output, "Did you mean") {
		t.Errorf("a key near nothing suggested one: %q", res.Output)
	}
}

// THE LIST IS BOUNDED, and a company with no skills is told there is none.
func TestAMissedToolSkillsListIsBounded(t *testing.T) {
	t.Parallel()
	var many keyedSkills
	for i := range 45 {
		many = append(many, fmt.Sprintf("skill:k%02d", i))
	}
	res := callFor(t, registered(t, builtin.Deps{ToolSkills: many}, builtin.LoadToolSkillTool),
		executing(t), map[string]any{"key": "nope"})
	if !strings.Contains(res.Output, `"skill:k29" and 15 more.`) || strings.Contains(res.Output, "skill:k30") {
		t.Errorf("a long catalogue answered %q", res.Output)
	}
	none := callFor(t, registered(t, builtin.Deps{ToolSkills: keyedSkills{}}, builtin.LoadToolSkillTool),
		executing(t), map[string]any{"key": "nope"})
	if !strings.Contains(none.Output, "publishes no tool skills") {
		t.Errorf("an empty catalogue answered %q", none.Output)
	}
}
