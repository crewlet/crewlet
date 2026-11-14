package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// A REPORT ASKED OF A SERVER THAT HAS EXITED IS REFUSED BEFORE ANYTHING REAPS
// IT, by both fakes that stand in for a harness's own server, as the real
// server's closed port refuses its client from the moment the server exits.
// The serve lifecycle's cases hold the usage to being asked while the server
// is up on exactly that refusal, so a fake that answered until the reap passed
// a run that asked once its server was stopped and not yet reaped (#1080's
// defect, read off kill(pid, 0)).
//
// The server here is a process this case starts, kills and holds unreaped
// until it ends, so every report is asked of an exited, unreaped server, never
// of one a race left running or reaped. Linux alone, for waitid's WNOWAIT: the
// one wait that returns once the kernel holds the child as a zombie and reaps
// nothing.
func TestAReportAskedOfAnExitedServerIsRefusedBeforeItsReap(t *testing.T) {
	pid := exitedUnreapedServer(t)
	for _, tc := range []struct {
		name, script, passwordEnv string
		args                      []string
	}{
		{"claude code", `{"serve":{"report_usage":{"input":1,"output":1}}}`, "FAKE_SERVER_PASSWORD",
			[]string{"report"}},
		{"opencode", `{"harness":"opencode"}`, "OPENCODE_PASSWORD", []string{"api", "session.get"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			server := fmt.Sprintf("http://fake-%d", pid)
			// The server's address and password, and the total OpenCode's server
			// would report: a fake that answered has everything to answer with,
			// so its refusal is the one thing that can pass.
			if err := os.WriteFile(filepath.Join(dir, fakeServerFile), []byte(server+" pw"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, fakeTotalFile), []byte(`{"input":1,"output":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], append(tc.args, "--server", server)...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), helperEnv+"=1", fakeMCPEnv+"="+tc.script, tc.passwordEnv+"=pw")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 4 || !strings.Contains(string(out), "connection refused") {
				t.Fatalf("the report asked of an exited server answered %q (%v), want it refused with exit 4", out, err)
			}
		})
	}
}

// exitedUnreapedServer starts a process, kills it and returns its pid once the
// kernel holds it as a zombie, which the case's end reaps.
func exitedUnreapedServer(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the stand-in server: %v", err)
	}
	pid := cmd.Process.Pid
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
	for errors.Is(err, unix.EINTR) {
		// A signal the runtime took, not the child's exit: the same wait again.
		err = unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
	}
	if err != nil {
		t.Fatalf("waiting for the stand-in server to exit, unreaped: %v", err)
	}
	proc, found, err := procgroup.Inspect(pid)
	if err != nil || !found || !proc.Zombie {
		t.Fatalf("the stand-in server reads as %+v (found %t, %v), want an exited process nothing has reaped",
			proc, found, err)
	}
	return pid
}
