package prompts_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
)

// EVERY TOOL THE ONBOARDING HEADER NAMES IS ONE THAT EXISTS, under the name it
// is registered with.
//
// The header is prose, so a tool renamed in its own package leaves it sending
// the pass to activate a tool that is not there. Held here, in an external
// test, because the runner imports this package and an internal test could not
// import it back.
func TestTheOnboardingHeaderNamesToolsThatExist(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		builtin.SearchKnowledgeTool, builtin.GetPageTool,
		runner.ActivateTool, runner.ListMCPToolsTool,
		runner.ReflectAndPersistTool, runner.MarkOnboardedTool,
	} {
		if !strings.Contains(prompts.OnboardingHeader, "`"+name) {
			t.Errorf("the onboarding header does not name %q, which is what the "+
				"tool it means is registered as:\n%s", name, prompts.OnboardingHeader)
		}
	}
}
