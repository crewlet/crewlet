//go:build unix

package sandbox_test

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// AN E2B TEMPLATE'S PROFILE CANNOT REPLACE THE RUN ENVIRONMENT.
//
// envd hands a process its environment before its shell starts, and every
// command here was `bash -l`: a template's /etc/profile.d script or
// ~/.bash_profile ran after the run environment and won, so a template that
// exported OTEL_LOG_USER_PROMPTS=true turned a coding agent's prompt capture
// back on over the engine's `false`. The request a login command sends is run
// here under a real `bash` exactly as envd runs it — its argv, its envs — in a
// home whose profile fights every variable; the run environment must still be
// what the command sees, the profile's own additions kept beside it.
//
// THE CONTROL: the same profile under the request shape this replaced loses
// the run environment, so the case is not passing on a profile that never ran.
//
// Mutation: send the run environment as the process's envs with no script
// putting it back, and the login case goes red.
func TestAnE2BLoginShellKeepsTheRunEnvironmentOverItsTemplatesProfile(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("this case runs envd's own shell, and there is no bash here: %v", err)
	}
	stub := newE2BStub()
	stub.frames = []any{startEvent(7), endEvent(0)}
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	const cmd = `printf '%s|%s|%s|%s' "$OTEL_LOG_USER_PROMPTS" "$GITHUB_TOKEN" "$PROFILE_ONLY" ` +
		`"$(env | grep -c '^CREWLET_RUN_ENV_')"`
	env := map[string]string{"OTEL_LOG_USER_PROMPTS": "false", "GITHUB_TOKEN": "declared-by-config"}
	if _, err := box.Exec(context.Background(), cmd, sandbox.ExecOptions{Env: env, Login: true}); err != nil {
		t.Fatal(err)
	}
	process, _ := stub.body("/process.Process/Start")["process"].(map[string]any)
	var args []string
	for _, a := range process["args"].([]any) {
		args = append(args, a.(string))
	}
	envs := map[string]string{}
	for k, v := range process["envs"].(map[string]any) {
		envs[k] = v.(string)
	}
	if len(args) != 3 || args[0] != "-l" || args[1] != "-c" {
		t.Fatalf("args = %q, want a login shell", args)
	}
	if strings.Contains(strings.Join(args, " "), "declared-by-config") {
		t.Fatalf("a value reached the command line: %q", args)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"),
		[]byte("export OTEL_LOG_USER_PROMPTS=true GITHUB_TOKEN=from-the-profile PROFILE_ONLY=kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	asEnvd := func(args []string, envs map[string]string) string {
		t.Helper()
		run := exec.CommandContext(t.Context(), bash, args...)
		run.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		for _, k := range slices.Sorted(maps.Keys(envs)) {
			run.Env = append(run.Env, k+"="+envs[k])
		}
		out, err := run.Output()
		if err != nil {
			t.Fatalf("bash %q: %v", args, err)
		}
		return string(out)
	}
	if got := asEnvd(args, envs); got != "false|declared-by-config|kept|0" {
		t.Errorf("the command saw %q, want the run environment over the profile's, its own "+
			"variable kept and no stash", got)
	}
	if got := asEnvd([]string{"-l", "-c", cmd}, env); got != "true|from-the-profile|kept|0" {
		t.Fatalf("the control saw %q: the profile did not run, so the case above proves nothing", got)
	}
}
