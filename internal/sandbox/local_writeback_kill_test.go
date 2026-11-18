//go:build unix

package sandbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/hostbox/hostboxtest"
)

// A BOX'S TEARDOWN KILLED AT ANY STEP LEAVES ONE WHOLE LOGIN. A process runs
// local boxes in turn, each seeded with the shared login, its run rewriting
// both files to the other login and its teardown writing them back, and is
// killed at every step of the write-backs after its first box ([hostboxtest]'s
// kill matrix). The shared directory, judged exactly and read as the next seed
// reads it, always holds the whole login the interrupted teardown committed,
// never one sign-in's token beside the other's config.toml.
//
// Mutation: write the box's refreshed files back one at a time, and the run
// makes four writes for two logins; drop the observer from the box's layout,
// and it reaches no step.
func TestABoxTeardownKilledPartWayNeverLeavesHalfALogin(t *testing.T) {
	t.Parallel()
	hostboxtest.KillAtEveryStep(t, writeBackScript, func(t *testing.T) hostboxtest.Run {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range writeBackLogins[1] {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), writeBackWriterEnv+"="+dir, writeBackStateEnv+"="+t.TempDir())
		return hostboxtest.Run{Cmd: cmd, Dir: dir, Read: func(t *testing.T) map[string]string {
			t.Helper()
			// As the next box's seed reads it.
			release, err := hostbox.LockCredentials(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			login, err := hostbox.ReadCredentials(dir, writeBackScript.Names)
			release()
			if err != nil {
				t.Fatalf("reading what the killed teardown left: %v", err)
			}
			got := map[string]string{}
			for name, content := range login {
				got[name] = string(content)
			}
			return got
		}}
	})
}
