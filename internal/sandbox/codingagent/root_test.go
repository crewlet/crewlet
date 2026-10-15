package codingagent_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// What these tests protect.
//
// Every headless Claude Code run passes `--permission-mode bypassPermissions`,
// and Claude Code refuses that to a process running as root unless it is told
// the process is a deliberate sandbox — measured against 2.1.300, it exits at
// once with "cannot be used with root/sudo privileges". A container under a
// rootless runtime runs its process as root, and so does any container, and
// any direct box, on a host whose engine runs as root: every such run failed at
// its first breath, read back as a run that "exited 1".

// A CONTAINER IS TOLD IT IS A SANDBOX, and nothing else is.
//
// Mutation: drop the claim and the container case goes red; make it for every
// cell and the direct and E2B cases do.
func TestOnlyAContainerIsToldItIsASandbox(t *testing.T) {
	t.Parallel()
	for placement, told := range map[sandbox.Placement]bool{
		sandbox.Container: true,
		sandbox.Direct:    false,
		sandbox.E2B:       false,
	} {
		cmd := claude().Command(sandbox.RunRequest{Brief: "fix it", Placement: placement},
			codingagent.Paths{}, "")
		if got := strings.HasPrefix(cmd, "IS_SANDBOX=1 claude -p "); got != told {
			t.Errorf("%s: told it is a sandbox = %v, want %v: %s", placement, got, told, cmd)
		}
		if !strings.Contains(cmd, "--permission-mode bypassPermissions") {
			t.Errorf("%s: the run no longer bypasses permissions: %s", placement, cmd)
		}
	}
}

// A DIRECT BOX ON AN ENGINE RUNNING AS ROOT IS REFUSED BEFORE IT EXISTS, by a
// sentence naming both ways out — not told it is a sandbox: it is the engine's
// own host, as the engine's own user, which is exactly what the CLI's refusal
// protects. Every other pairing is admitted, and so is a direct box whose
// operator has declared the host a sandbox in role.sandbox.env, as the CLI
// itself would admit it.
//
// Mutation: admit a direct box as root and the first case goes red; refuse
// it whatever the uid and the non-root case does; ignore the operator's
// declaration and its case does.
func TestADirectBoxAsRootIsRefusedByName(t *testing.T) {
	t.Parallel()
	err := codingagent.ClaudeCodeAs(0).Admit(sandbox.Spec{Placement: sandbox.Direct}, true)
	if err == nil {
		t.Fatal("a direct box on an engine running as root was admitted")
	}
	for _, names := range []string{"non-root user", "run_in: container"} {
		if !strings.Contains(err.Error(), names) {
			t.Errorf("the refusal does not name %q: %v", names, err)
		}
	}

	for _, tc := range []struct {
		name     string
		cli      codingagent.ClaudeCode
		spec     sandbox.Spec
		asEngine bool
	}{
		{"a direct box as another user", codingagent.ClaudeCodeAs(1000),
			sandbox.Spec{Placement: sandbox.Direct}, true},
		{"a container as root", codingagent.ClaudeCodeAs(0),
			sandbox.Spec{Placement: sandbox.Container}, false},
		{"an e2b box", codingagent.ClaudeCodeAs(0), sandbox.Spec{Placement: sandbox.E2B}, false},
		// The fake a demonstration runs on answers the direct cell and
		// runs nothing: the backend's answer decides, not the cell's name.
		{"a direct cell on a backend that is not the engine's host", codingagent.ClaudeCodeAs(0),
			sandbox.Spec{Placement: sandbox.Direct}, false},
		{"a direct box the operator declared a sandbox", codingagent.ClaudeCodeAs(0),
			sandbox.Spec{Placement: sandbox.Direct, Env: map[string]string{"IS_SANDBOX": "1"}}, true},
		{"a direct box under bubblewrap", codingagent.ClaudeCodeAs(0),
			sandbox.Spec{Placement: sandbox.Direct, Env: map[string]string{"CLAUDE_CODE_BUBBLEWRAP": "1"}}, true},
	} {
		if err := tc.cli.Admit(tc.spec, tc.asEngine); err != nil {
			t.Errorf("%s was refused: %v", tc.name, err)
		}
	}
	// A DECLARATION THE CLI WOULD NOT READ is no declaration: it takes
	// IS_SANDBOX as exactly "1".
	if codingagent.ClaudeCodeAs(0).Admit(sandbox.Spec{Placement: sandbox.Direct,
		Env: map[string]string{"IS_SANDBOX": "true"}}, true) == nil {
		t.Error("IS_SANDBOX=true, which Claude Code does not read as a sandbox, was admitted")
	}
}

// OPENCODE ADMITS EVERY CELL: it has no refusal of its own about where it runs.
func TestOpenCodeAdmitsEveryCell(t *testing.T) {
	t.Parallel()
	for _, p := range sandbox.Placements {
		if err := (codingagent.OpenCode{}).Admit(sandbox.Spec{Placement: p}, true); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
