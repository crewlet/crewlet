package engine

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A HELPER'S RUN HAS NO WEB TOOL, WHATEVER ITS ENTRY SAYS. Ask Crewlet is
// assigned the ordinary entry the seats run with their web tools on, and the
// run the engine builds for it carries the empty allow list and no tool of its
// own, while the seats' run of the same entry keeps WebFetch and WebSearch; an
// entry no helper names gets no helper's run. Mutation: build the helper from
// the entry's own overrides and its argv keeps the web.
func TestAHelpersRunHasNoWebToolWhateverItsEntrySays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doc := fmt.Sprintf(`name: Acme
providers:
  llm:
    sonnet:
      harness: claude-code
      model: sonnet
      state_dir: %[1]s/sonnet
      overrides: {web_tools: allowed}
    other:
      harness: claude-code
      model: haiku
      state_dir: %[1]s/other
assistant:
  model: sonnet
`, dir)
	c, err := NewCompany(parseCompany(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	seat, ok := c.Models.Provider("sonnet")
	if !ok {
		t.Fatal("the seats' run of the entry was not built")
	}
	if args := seat.(*harness.Provider).Profile().CompleteArgs; !slices.Contains(args, "WebFetch,WebSearch") {
		t.Errorf("the seats' run lost its web tools: %q", args)
	}
	helper, ok := c.Helpers["sonnet"].(*harness.Provider)
	if !ok {
		t.Fatalf("Ask Crewlet's model has no helper's run: %v", c.Helpers)
	}
	profile := helper.Profile()
	if profile.EffectiveWebTools() != harnessprofile.WebToolsDenied || profile.ToolsOffProblem() != "" {
		t.Errorf("the helper's run keeps a tool of its own: web %s, %s", profile.EffectiveWebTools(),
			profile.ToolsOffProblem())
	}
	if slices.Contains(profile.CompleteArgs, "WebFetch,WebSearch") {
		t.Errorf("the helper's argv allows the web: %q", profile.CompleteArgs)
	}
	if _, built := c.Helpers["other"]; built {
		t.Error("an entry no helper names was built as a helper's run")
	}
}
