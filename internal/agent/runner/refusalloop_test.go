package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
)

// refusingWrite is a write tool that refuses every call on its policy, as
// create_artifact refused the GP-38 brand kit fourteen times.
type refusingWrite struct{}

func (refusingWrite) Name() string               { return "create_thing" }
func (refusingWrite) Description() string        { return "creates a thing" }
func (refusingWrite) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (refusingWrite) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "create_thing refused the version: brand.json: index: voice.x is not a key",
		Failed: true, Cause: tools.RefusalInvalid}, nil
}

// THE GP-38 TURN, REPLAYED THROUGH THE RUNNER (ADR-0059): the model asks for
// the artifacts skill by the wrong key and is told the right one, loads it,
// and then keeps writing a version its tool refuses. From the fourth refusal
// on, the answer tells it to stop and raise a blocker, and the node counts the
// loop once (crewlet.tools.policy_refusal_loops); the turn ends on its own
// submission.
//
// Mutation: drop the Metrics wiring from the runner's surface and nothing is
// counted; drop the key list from load_tool_skill and the miss names none.
func TestTheGP38LoopIsStoppedAndCounted(t *testing.T) {
	t.Parallel()
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{
		Key: builtin.ArtifactsToolSkillKey, Title: "Publishing artifacts",
		Summary: "the design contract", Body: "Read the format first.",
		Trigger: skills.Trigger{Tool: "create_thing"}, SourcePageID: "artifacts-page",
	}); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(refusingWrite{}, tools.OriginBuiltin,
		tools.Annotations{ReadOnly: mcp.No, OpenWorld: mcp.Yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := builtin.Register(reg, builtin.Deps{ToolSkills: registry,
		Authorize: builtin.Decide(authz.NoChart{})}); err != nil {
		t.Fatal(err)
	}
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	attempts := tools.PolicyRefusalLimit + 1
	script := []llmtest.Response{
		submitCall(t, builtin.LoadToolSkillTool, `{"key":"artifacts"}`),
		submitCall(t, builtin.LoadToolSkillTool, `{"key":"`+builtin.ArtifactsToolSkillKey+`"}`),
	}
	for range attempts {
		script = append(script, submitCall(t, "create_thing", `{}`))
	}
	script = append(script, submitCall(t, runner.SubmitWorkTool,
		`{"outcome":"blocked","summary":"the brand kit is refused","evidence":"create_thing refused it"}`))
	prov := &scriptedProvider{execute: script}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatal(err)
	}
	role := &org.Role{Name: "Designer", DeclaredHandle: "designer"}
	o := &org.Organization{Name: "GoPhrase", Roles: []*org.Role{role}}
	r, err := runner.New(runner.Config{
		Seat: prompts.Seat{Org: o, Role: role}, Registry: reg, Models: models, Skills: registry,
		Caps: runner.Caps{ExecutorRounds: attempts + 4}, Task: "draft the brand kit (GP-38)",
		Metrics: rec,
		Turn: runner.Turn{RunID: "t-gp38", WorkKey: "GP-38", AgentID: "agent-designer",
			Context: &turnctx.Turn{RunID: "t-gp38", WorkKey: "GP-38", Seat: role, Org: o}},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if miss := prov.answerTo("execute", builtin.LoadToolSkillTool); !strings.Contains(miss,
		`Did you mean "`+builtin.ArtifactsToolSkillKey+`"?`) {
		t.Errorf("the wrong key was answered %q", miss)
	}
	var stops int
	for i, res := range prov.answered("execute") {
		if res.Name != "create_thing" {
			continue
		}
		if strings.Contains(res.Output, "STOP RETRYING create_thing") {
			stops++
			if !strings.Contains(res.Output, "raise a blocker on it") {
				t.Errorf("answer %d stops without saying to raise a blocker: %q", i, res.Output)
			}
		}
	}
	if want := attempts - tools.PolicyRefusalLimit + 1; stops != want {
		t.Errorf("%d answers said stop, want %d (from the %dth refusal on)", stops, want,
			tools.PolicyRefusalLimit)
	}
	var counted uint64
	for _, s := range rec.Read() {
		if s.Name == metrics.ToolPolicyRefusalLoops && s.Attrs["tool"] == "create_thing" {
			counted = s.Total
		}
	}
	if counted != 1 {
		t.Errorf("the node counted %d refusal loops, want the one", counted)
	}
}
