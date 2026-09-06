package runner

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// A SEAT IS NEVER SHOWN A TOOL THAT WOULD ONLY REFUSE IT.
//
// run_sandbox is registered per EPOCH — the engine wires one launcher for the
// whole company — so every seat's registry carries it the moment any seat can
// run code. Two seats can never use it, and before this filter both learned so
// by calling it: a round spent, on a tool the catalogue vouched for, and
// sometimes a whole turn planned around a box that was never coming.
//
// Mutate `offersSandbox` to return true and the first two cases go red.
func TestExecutorSurfaceHidesRunSandboxFromSeatsThatCannotUseIt(t *testing.T) {
	t.Parallel()

	gated := &org.Role{Name: "Engineer", Sandbox: &org.RoleSandbox{Enabled: true}}
	ungated := &org.Role{Name: "Chief of Staff"}
	offGate := &org.Role{Name: "Analyst", Sandbox: &org.RoleSandbox{Enabled: false}}

	cases := []struct {
		name     string
		seat     *org.Role
		agentRun AgentLauncher
		want     bool
	}{
		{
			name: "no gate at all — the launcher would answer " +
				"\"this seat's sandbox is not enabled\"",
			seat: ungated,
		},
		{
			name: "a gate that is off — same refusal, and the operator " +
				"turned it off on purpose",
			seat: offGate,
		},
		{
			name: "an agent-mode executor already holds a shell, so a " +
				"second box would put the work where the turn cannot see it",
			seat:     gated,
			agentRun: stubLauncher{},
			want:     false,
		},
		{
			name: "a gated seat on the native loop is the one that can " +
				"actually launch one",
			seat: gated,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := New(Config{
				Registry: tools.NewRegistry(),
				Models:   &phase.Registry{},
				Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme"}, Role: tc.seat},
				AgentRun: tc.agentRun,
			})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := tools.NewRegistry().Snapshot().
				With(tools.Entry{Tool: stubTool(RunSandboxTool), Origin: tools.OriginBuiltin})
			if err != nil {
				t.Fatal(err)
			}
			got := slices.Contains(r.executorActive(snapshot), RunSandboxTool)
			if got != tc.want {
				t.Errorf("run_sandbox offered = %v, want %v", got, tc.want)
			}
		})
	}
}

// stubTool is a name on the surface and nothing else — what is under test is
// which names survive the filter, not what any of them do.
type stubTool string

func (s stubTool) Name() string               { return string(s) }
func (s stubTool) Description() string        { return "stub" }
func (s stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// stubLauncher stands for "this executor runs as somebody else's agentic
// loop". Only its presence is read.
type stubLauncher struct{}

func (stubLauncher) LaunchExecutor(context.Context, AgentRunRequest) error { return nil }
