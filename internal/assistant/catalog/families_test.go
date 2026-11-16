package catalog_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// everyTool is the whole catalogue a turn can be offered, generic and deep work included.
func everyTool(t *testing.T) []catalog.Tool {
	t.Helper()
	docs, read, change := generic(t, admin)
	return append(catalog.Tools(), docs, read, change, catalog.DeepWorkTool())
}

// Every write is in `writes`, so denying it leaves Ask Crewlet only reading; a
// chat, atlas or agent-control write is in its own family too.
func TestEveryWriteIsInAFamilyTheCompanyCanDeny(t *testing.T) {
	t.Parallel()
	for _, tool := range everyTool(t) {
		families := catalog.FamiliesOf(tool)
		if tool.Kind == catalog.KindWrite && !slices.Contains(families, config.FamilyWrites) {
			t.Errorf("write %s is not in writes", tool.Name)
		}
		for _, c := range []struct {
			in     bool
			family config.AssistantToolFamily
		}{
			{tool.Kind == catalog.KindWrite && slices.Contains(tool.Apps, iam.GrantChatlet), config.FamilyChatWrites},
			{tool.Kind == catalog.KindWrite && strings.HasPrefix(tool.Act, "atlas_"), config.FamilyAtlasWrites},
			{tool.Name == catalog.ProposeAPIChangeTool, config.FamilyGenericChanges},
			{tool.Name == catalog.APIRequestTool, config.FamilyGenericReads},
			{tool.Kind == catalog.KindPageAction, config.FamilyPageChanges},
			{tool.Kind == catalog.KindDeep, config.FamilyDeepWork},
		} {
			if c.in != slices.Contains(families, c.family) {
				t.Errorf("%s in %s = %v, want %v", tool.Name, c.family, !c.in, c.in)
			}
		}
	}
	for _, name := range []string{"pause_agent", "resume_agent", "steer_turn", "answer_executor"} {
		i := slices.IndexFunc(everyTool(t), func(tool catalog.Tool) bool { return tool.Name == name })
		if i < 0 || !slices.Contains(catalog.FamiliesOf(everyTool(t)[i]), config.FamilyAgentControls) {
			t.Errorf("%s is not in agent_controls", name)
		}
	}
	// Every family names at least one tool, so none is a setting that does nothing.
	for _, f := range config.AssistantToolFamilies {
		if !slices.ContainsFunc(everyTool(t), func(tool catalog.Tool) bool {
			return slices.Contains(catalog.FamiliesOf(tool), f)
		}) {
			t.Errorf("no tool is in %s", f)
		}
	}
}

// Denying a family removes exactly its tools and nothing else.
func TestDenyingAFamilyRemovesOnlyItsTools(t *testing.T) {
	t.Parallel()
	tools := everyTool(t)
	readOnly := catalog.Allowed(tools, []config.AssistantToolFamily{config.FamilyWrites})
	for _, tool := range readOnly {
		if tool.Kind == catalog.KindWrite {
			t.Errorf("writes denied, and %s is still offered", tool.Name)
		}
	}
	if !slices.ContainsFunc(readOnly, func(tool catalog.Tool) bool { return tool.Kind == catalog.KindRead }) {
		t.Error("writes denied took the reads too")
	}
	noChat := catalog.Allowed(tools, []config.AssistantToolFamily{config.FamilyChatWrites})
	if len(tools)-len(noChat) == 0 || slices.ContainsFunc(noChat, func(tool catalog.Tool) bool {
		return tool.Name == "post_chat_message"
	}) {
		t.Errorf("chat_writes denied left %d of %d tools, post_chat_message among them", len(noChat), len(tools))
	}
	if got := catalog.Allowed(tools, nil); len(got) != len(tools) {
		t.Errorf("nothing denied removed %d tools", len(tools)-len(got))
	}
}
