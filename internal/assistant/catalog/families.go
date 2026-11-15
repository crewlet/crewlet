package catalog

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// agentControlActs are the acts that pause, resume, steer or answer an agent's run.
var agentControlActs = []string{"pause_seat", "resume_seat", "steer_turn", "answer_executor"}

// FamiliesOf is every family `assistant.tools.deny` can turn t off by; a tool in
// none of them (a curated read, describe_api, open_in_app) is always offered.
func FamiliesOf(t Tool) []config.AssistantToolFamily {
	var out []config.AssistantToolFamily
	switch t.Kind {
	case KindWrite:
		out = append(out, config.FamilyWrites)
		switch {
		case t.Name == ProposeAPIChangeTool:
			out = append(out, config.FamilyGenericChanges)
		case slices.Contains(t.Apps, iam.GrantChatlet):
			out = append(out, config.FamilyChatWrites)
		case strings.HasPrefix(t.Act, "atlas_"):
			out = append(out, config.FamilyAtlasWrites)
		case slices.Contains(agentControlActs, t.Act):
			out = append(out, config.FamilyAgentControls)
		}
	case KindRead:
		if t.Name == APIRequestTool {
			out = append(out, config.FamilyGenericReads)
		}
	case KindPageAction:
		out = append(out, config.FamilyPageChanges)
	case KindDeep:
		out = append(out, config.FamilyDeepWork)
	}
	return out
}

// Allowed is tools without every tool in a family deny names. It only narrows:
// what a person is not offered stays unoffered whatever deny says.
func Allowed(tools []Tool, deny []config.AssistantToolFamily) []Tool {
	if len(deny) == 0 {
		return tools
	}
	return slices.DeleteFunc(slices.Clone(tools), func(t Tool) bool {
		return slices.ContainsFunc(FamiliesOf(t), func(f config.AssistantToolFamily) bool {
			return slices.Contains(deny, f)
		})
	})
}
