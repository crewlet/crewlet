package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/tools"
)

// landingInstructions is an instructions store whose every save lands one
// revision past the one named, for the walks that call a write more than once.
type landingInstructions struct{}

func (landingInstructions) SaveSeatInstructions(_ context.Context, _ string, _ json.RawMessage,
	_ time.Time, revision uint64) (uint64, bool, error) {

	return revision + 1, true, nil
}

// instructionsTool is the operator catalogue's save_assistant_instructions over fleet.
func instructionsTool(t *testing.T, fleet *coordmem.Fleet) tools.Callable {
	t.Helper()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Instructions: builtin.InstructionsDeps{Store: fleet, Actor: builtin.PrincipalActor,
			Now: func() time.Time { return boardNow }},
		Authorize: builtin.Decide(seatLeads),
	}) {
		if tool.Name() == builtin.SaveInstructionsTool {
			return tool
		}
	}
	t.Fatal("the operator catalogue serves no save_assistant_instructions with a store wired")
	return nil
}

// A PERSON'S INSTRUCTIONS LAND UNDER THEIR SEAT, HELD TO THE COMPANY LAYER'S
// RULES: a credential, an unknown preset or a member the layer lacks is refused
// and nothing is stored, and the refusal never repeats the credential, so it
// reaches no log a refused call's answer is written to.
// Mutation: save without Validate, and the credential is stored.
func TestInstructionsLandUnderTheSeatAndACredentialNever(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	tool := instructionsTool(t, fleet)
	jane := boundTo("jane")
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	for name, in := range map[string]map[string]any{
		"a credential":      {"text": "Sign with " + secret},
		"an unknown length": {"length": "terse"},
		"another member":    {"tone": "warm"},
	} {
		res, _ := saveBoard(t, tool, jane, map[string]any{"instructions": in, "if_match": json.Number("0")})
		if !res.Failed {
			t.Errorf("%s was saved: %q", name, res.Output)
		}
		if strings.Contains(res.Output, secret) {
			t.Errorf("the refusal of %s repeats the credential", name)
		}
	}
	if _, _, found, _ := fleet.SeatInstructions(t.Context(), "jane"); found {
		t.Fatal("a refused save stored instructions")
	}
	res, out := saveBoard(t, tool, jane, map[string]any{
		"instructions": map[string]any{"length": "brief", "text": "Use British spelling."},
		"if_match":     json.Number("0"),
	})
	if res.Failed || out["seat"] != "jane" {
		t.Fatalf("a valid save answered %q", res.Output)
	}
	held, _, found, err := fleet.SeatInstructions(t.Context(), "jane")
	if err != nil || !found {
		t.Fatalf("nothing stored under jane (%v)", err)
	}
	if got := config.ReadAssistantInstructions(held.Instructions); got != (config.AssistantInstructions{
		Length: config.AnswerBrief, Text: "Use British spelling."}) {
		t.Errorf("stored %+v", got)
	}
}
